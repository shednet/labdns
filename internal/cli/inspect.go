/*
Copyright 2026 Konstantinos Kalyvas.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/dnsendpoint"
)

type filters struct {
	Namespace  string
	Provider   string
	SourceKind sourceKindFilter
	RecordType recordTypeFilter
}

type sourceKindFilter string

const (
	sourceKindIngressFilter   sourceKindFilter = "Ingress"
	sourceKindHTTPRouteFilter sourceKindFilter = "HTTPRoute"
)

type recordTypeFilter string

const (
	recordTypeAFilter    recordTypeFilter = "A"
	recordTypeAAAAFilter recordTypeFilter = "AAAA"
)

type inspector struct {
	Client           client.Client
	Discovery        discovery.DiscoveryInterface
	dnsClientFactory dnsClientFactory
}

func (i inspector) records(ctx context.Context, filters filters) (recordList, error) {
	var objects externaldnsv1alpha1.DNSEndpointList
	opts := []client.ListOption{client.MatchingLabels{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue}}
	if filters.Namespace != "" {
		opts = append(opts, client.InNamespace(filters.Namespace))
	}
	if err := i.Client.List(ctx, &objects, opts...); err != nil {
		return recordList{}, fmt.Errorf("list managed DNSEndpoints: %w", err)
	}
	result := recordList{Items: []record{}}
	for index := range objects.Items {
		object := &objects.Items[index]
		if filters.Provider != "" && object.Labels[dnsendpoint.ProviderLabel] != filters.Provider {
			continue
		}
		if filters.SourceKind != "" && !strings.EqualFold(object.Annotations[dnsendpoint.SourceKindAnnotation], string(filters.SourceKind)) {
			continue
		}
		result.Items = append(result.Items, recordsFor(object, filters.RecordType)...)
	}
	sort.Slice(result.Items, func(a, b int) bool {
		left, right := result.Items[a], result.Items[b]
		return strings.Join([]string{left.DNSName, left.Provider, left.DNSEndpoint.Namespace, left.Source.Kind, left.Source.Name, left.RecordType}, "\x00") <
			strings.Join([]string{right.DNSName, right.Provider, right.DNSEndpoint.Namespace, right.Source.Kind, right.Source.Name, right.RecordType}, "\x00")
	})
	return result, nil
}

func recordsFor(object *externaldnsv1alpha1.DNSEndpoint, recordType recordTypeFilter) []record {
	pending, lifecycleErr := dnsendpoint.InspectLifecycle(object.Annotations[dnsendpoint.LifecycleAnnotation])
	result := make([]record, 0, len(object.Spec.Endpoints))
	for _, item := range object.Spec.Endpoints {
		if item == nil || recordType != "" && !strings.EqualFold(item.RecordType, string(recordType)) {
			continue
		}
		properties := map[string][]string{}
		for _, property := range item.ProviderSpecific {
			properties[property.Name] = append(properties[property.Name], property.Value)
		}
		for name := range properties {
			sort.Strings(properties[name])
		}
		if len(properties) == 0 {
			properties = nil
		}
		record := record{
			DNSName: item.DNSName, RecordType: item.RecordType, Targets: slices.Clone(item.Targets), TTL: int64(item.RecordTTL),
			ActiveTargets: nil,
			Provider:      object.Labels[dnsendpoint.ProviderLabel],
			Source:        sourceRef{Kind: object.Annotations[dnsendpoint.SourceKindAnnotation], Namespace: object.Annotations[dnsendpoint.SourceNamespaceAnnotation], Name: object.Annotations[dnsendpoint.SourceNameAnnotation], UID: object.Annotations[dnsendpoint.SourceUIDAnnotation]},
			DNSEndpoint:   objectRef{Namespace: object.Namespace, Name: object.Name}, Generation: object.Generation,
			Observed: object.Status.ObservedGeneration, ExternalDNSState: observationState(object.Generation, object.Status.ObservedGeneration), Properties: properties,
		}
		sort.Strings(record.Targets)
		if lifecycleErr != nil {
			record.LifecycleError = lifecycleErr.Error()
			record.ExternalDNSState = stateInvalid
		} else {
			for _, target := range pending {
				if target.DNSName == item.DNSName && target.RecordType == item.RecordType {
					record.Retiring = append(record.Retiring, retiringTarget{Target: target.Target, Deadline: target.Deadline})
				}
			}
		}
		if lifecycleErr == nil {
			record.ActiveTargets = []string{}
			retiring := map[string]struct{}{}
			for _, target := range record.Retiring {
				retiring[target.Target] = struct{}{}
			}
			for _, target := range record.Targets {
				if _, found := retiring[target]; !found {
					record.ActiveTargets = append(record.ActiveTargets, target)
				}
			}
		}
		result = append(result, record)
	}
	return result
}

func observationState(generation, observed int64) string {
	switch {
	case observed == generation:
		return stateObserved
	case observed == 0:
		return "unobserved"
	case observed < generation:
		return stateStale
	default:
		return stateInvalid
	}
}

func recordState(record record) string {
	if record.LifecycleError != "" || record.ExternalDNSState == stateInvalid {
		return stateInvalid
	}
	if record.ExternalDNSState == stateObserved {
		return stateObserved
	}
	return stateStale
}

func (i inspector) details(ctx context.Context, name string, filters filters, dnsServer string) (recordDetails, error) {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	list, err := i.records(ctx, filters)
	if err != nil {
		return recordDetails{}, err
	}
	result := recordDetails{Query: normalized, Items: []recordDetail{}}
	for _, record := range list.Items {
		if record.DNSName != normalized {
			continue
		}
		detail := recordDetail{Record: record}
		detail.Provider, err = i.providerDetail(ctx, record.Provider)
		if err != nil {
			return recordDetails{}, err
		}
		detail.Source, err = i.sourceDetail(ctx, record.Source)
		if err != nil {
			return recordDetails{}, err
		}
		if dnsServer != "" {
			lookup := lookupDNS(ctx, dnsServer, record.DNSName, record.RecordType, record.Targets, i.dnsClientFactory)
			detail.DNS = &lookup
		}
		result.Items = append(result.Items, detail)
	}
	if len(result.Items) == 0 {
		return result, fmt.Errorf("no managed record found for %q", normalized)
	}
	return result, nil
}

func (i inspector) providerDetail(ctx context.Context, name string) (providerDetail, error) {
	var provider labdnsv1alpha1.DNSProvider
	if err := i.Client.Get(ctx, types.NamespacedName{Name: name}, &provider); apierrors.IsNotFound(err) {
		return providerDetail{}, nil
	} else if err != nil {
		return providerDetail{}, fmt.Errorf("get DNSProvider %s: %w", name, err)
	}
	result := providerDetail{Found: true, DefaultTTL: provider.Spec.RecordDefaults.TTL}
	for _, zone := range provider.Spec.Zones {
		result.Zones = append(result.Zones, zone.Name)
	}
	if provider.Spec.IPSources.IPv4 != nil {
		result.IPv4Label = provider.Spec.IPSources.IPv4.NodeLabel
	}
	if provider.Spec.IPSources.IPv6 != nil {
		result.IPv6Label = provider.Spec.IPSources.IPv6.NodeLabel
	}
	return result, nil
}

func (i inspector) sourceDetail(ctx context.Context, ref sourceRef) (sourceDetail, error) {
	var object client.Object
	switch ref.Kind {
	case "Ingress":
		object = &networkingv1.Ingress{}
	case "HTTPRoute":
		object = &gatewayv1.HTTPRoute{}
	default:
		return sourceDetail{}, nil
	}
	if err := i.Client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, object); apierrors.IsNotFound(err) {
		return sourceDetail{}, nil
	} else if err != nil {
		return sourceDetail{}, fmt.Errorf("get source %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return sourceDetail{Found: true, UIDMatches: ref.UID != "" && ref.UID == string(object.GetUID())}, nil
}

func (i inspector) status(ctx context.Context, controllerNamespace, controllerName string) (status, bool) {
	result := status{Overall: "healthy", Prerequisites: []prerequisite{}, Controllers: []controllerStatus{}, Warnings: []string{}}
	hardFailure := false
	for _, requirement := range []struct{ name, version, resource string }{
		{"DNSProvider API", labdnsv1alpha1.GroupVersion.String(), "dnsproviders"},
		{"DNSEndpoint API", externaldnsv1alpha1.GroupVersion.String(), "dnsendpoints"},
	} {
		available, err := resourceAvailable(i.Discovery, requirement.version, requirement.resource)
		item := prerequisite{Name: requirement.name, Available: available}
		if err != nil {
			item.Error = err.Error()
			hardFailure = true
		}
		result.Prerequisites = append(result.Prerequisites, item)
	}

	controllers, err := i.controllers(ctx, controllerNamespace, controllerName)
	if err != nil {
		result.Warnings = append(result.Warnings, err.Error())
		hardFailure = true
	} else if len(controllers) == 0 {
		result.Warnings = append(result.Warnings, "no labdns controller Deployment found")
		hardFailure = true
	} else {
		result.Controllers = controllers
		for _, controller := range controllers {
			if controller.Available < controller.Desired || controller.Desired == 0 {
				hardFailure = true
			}
			if controller.GatewayAPI {
				for _, requirement := range []struct{ version, resource string }{
					{gatewayv1.GroupVersion.String(), "httproutes"}, {gatewayv1.GroupVersion.String(), "gateways"},
					{gatewayv1.GroupVersion.String(), "gatewayclasses"}, {gatewayv1beta1.GroupVersion.String(), "referencegrants"},
				} {
					available, discoveryErr := resourceAvailable(i.Discovery, requirement.version, requirement.resource)
					if discoveryErr != nil || !available {
						result.Warnings = append(result.Warnings, "Gateway API is enabled but "+requirement.resource+" is unavailable")
						hardFailure = true
					}
				}
			}
		}
	}

	var providers labdnsv1alpha1.DNSProviderList
	if err := i.Client.List(ctx, &providers); err != nil {
		result.Warnings = append(result.Warnings, "list DNSProviders: "+err.Error())
		hardFailure = true
	} else {
		result.Summary.Providers = len(providers.Items)
	}
	records, err := i.records(ctx, filters{})
	if err != nil {
		result.Warnings = append(result.Warnings, err.Error())
		hardFailure = true
	} else {
		endpoints, sources := map[string]struct{}{}, map[string]struct{}{}
		for _, record := range records.Items {
			result.Summary.Records++
			endpoints[record.DNSEndpoint.Namespace+"/"+record.DNSEndpoint.Name] = struct{}{}
			sources[record.Source.Kind+"/"+record.Source.Namespace+"/"+record.Source.Name] = struct{}{}
			result.Summary.PendingTargets += len(record.Retiring)
			switch recordState(record) {
			case stateInvalid:
				result.Summary.Invalid++
			case stateObserved:
				result.Summary.Observed++
			default:
				result.Summary.Stale++
			}
		}
		result.Summary.DNSEndpoints, result.Summary.PublishingSources = len(endpoints), len(sources)
		if result.Summary.Stale > 0 || result.Summary.Invalid > 0 {
			result.Overall = "degraded"
			result.Warnings = append(result.Warnings, "managed records include stale or invalid state")
		}
	}
	if hardFailure {
		result.Overall = "unhealthy"
	}
	return result, hardFailure
}

func resourceAvailable(discoveryClient discovery.DiscoveryInterface, version, resource string) (bool, error) {
	resources, err := discoveryClient.ServerResourcesForGroupVersion(version)
	if err != nil {
		return false, err
	}
	for _, candidate := range resources.APIResources {
		if candidate.Name == resource {
			return true, nil
		}
	}
	return false, fmt.Errorf("resource %s not served in %s", resource, version)
}

func (i inspector) controllers(ctx context.Context, namespace, name string) ([]controllerStatus, error) {
	var deployments appsv1.DeploymentList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if name == "" {
		opts = append(opts, client.MatchingLabels{"app.kubernetes.io/name": "labdns"})
	}
	if err := i.Client.List(ctx, &deployments, opts...); err != nil {
		return nil, fmt.Errorf("discover labdns controller Deployments: %w", err)
	}
	result := []controllerStatus{}
	for index := range deployments.Items {
		deployment := &deployments.Items[index]
		if name != "" && deployment.Name != name {
			continue
		}
		item := controllerStatus{Namespace: deployment.Namespace, Name: deployment.Name, Desired: 1, Available: deployment.Status.AvailableReplicas}
		if deployment.Spec.Replicas != nil {
			item.Desired = *deployment.Spec.Replicas
		}
		if manager := managerContainer(&deployment.Spec.Template); manager != nil {
			gatewayEnabled, parseErr := gatewayAPIEnabled(manager.Args)
			if parseErr != nil {
				return nil, fmt.Errorf("inspect Deployment %s/%s manager arguments: %w", deployment.Namespace, deployment.Name, parseErr)
			}
			item.GatewayAPI = gatewayEnabled
		}
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("parse Deployment %s/%s selector: %w", deployment.Namespace, deployment.Name, err)
		}
		var pods corev1.PodList
		if err := i.Client.List(ctx, &pods, client.InNamespace(deployment.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, fmt.Errorf("list Pods for Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
		}
		for podIndex := range pods.Items {
			if podReady(&pods.Items[podIndex]) {
				item.ReadyPods++
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(a, b int) bool {
		return result[a].Namespace+"/"+result[a].Name < result[b].Namespace+"/"+result[b].Name
	})
	return result, nil
}

const (
	defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"
	managerContainerName       = "manager"
)

func managerContainer(template *corev1.PodTemplateSpec) *corev1.Container {
	if template == nil {
		return nil
	}
	for index := range template.Spec.Containers {
		if template.Spec.Containers[index].Name == managerContainerName {
			return &template.Spec.Containers[index]
		}
	}
	if name := template.Annotations[defaultContainerAnnotation]; name != "" {
		for index := range template.Spec.Containers {
			container := &template.Spec.Containers[index]
			if container.Name == name && commandIsManager(container) {
				return container
			}
		}
	}
	for index := range template.Spec.Containers {
		container := &template.Spec.Containers[index]
		if commandIsManager(container) {
			return container
		}
	}
	return nil
}

func commandIsManager(container *corev1.Container) bool {
	if container == nil || len(container.Command) == 0 {
		return false
	}
	return path.Base(container.Command[0]) == managerContainerName
}

func gatewayAPIEnabled(args []string) (bool, error) {
	enabled := false
	for _, arg := range args {
		for _, prefix := range []string{"--enable-gateway-api", "-enable-gateway-api"} {
			if arg == prefix {
				enabled = true
				break
			}
			if value, found := strings.CutPrefix(arg, prefix+"="); found {
				parsed, err := strconv.ParseBool(value)
				if err != nil {
					return false, fmt.Errorf("invalid enable-gateway-api value %q: %w", value, err)
				}
				enabled = parsed
				break
			}
		}
	}
	return enabled, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
