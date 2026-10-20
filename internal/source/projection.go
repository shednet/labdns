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

package source

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

const serviceKind = "Service"

type SourceKind string

const (
	SourceKindIngress   SourceKind = "Ingress"
	SourceKindHTTPRoute SourceKind = "HTTPRoute"
	SourceKindTCPRoute  SourceKind = "TCPRoute"
	SourceKindUDPRoute  SourceKind = "UDPRoute"
)

type RecordType string

const (
	RecordTypeA    RecordType = "A"
	RecordTypeAAAA RecordType = "AAAA"
)

type Identity struct {
	APIVersion string
	Kind       SourceKind
	Namespace  string
	Name       string
	UID        types.UID
}

type Backend struct {
	Namespace string
	Name      string
}

func (b Backend) Key() string { return b.Namespace + "/" + b.Name }

type HostProjection struct {
	Hostname string
	Backends []Backend
}

type Property struct {
	Name  string
	Value string
}

type Record struct {
	DNSName          string
	RecordType       RecordType
	Targets          []string
	TTL              int64
	ProviderSpecific []Property
}

type Publication struct {
	ProviderName        string
	Records             []Record
	MetadataAnnotations map[string]string
	DeletionDelay       time.Duration
}

// Output is the provider-neutral boundary implemented by the DNSEndpoint writer.
type Output interface {
	Apply(context.Context, Identity, []Publication) error
}

const (
	DiagnosticNormal  = "Normal"
	DiagnosticWarning = "Warning"
)

type DiagnosticFunc func(severity, reason, message string)

func IngressProjection(ingress *networkingv1.Ingress, overrides []string, diagnostic DiagnosticFunc) ([]HostProjection, error) {
	emit := diagnosticOnce(diagnostic)
	all := map[Backend]struct{}{}
	hasBackendReference := false
	if ingress.Spec.DefaultBackend != nil {
		hasBackendReference = ingress.Spec.DefaultBackend.Service != nil || ingress.Spec.DefaultBackend.Resource != nil
		if backend, ok := ingressBackend(ingress.Namespace, ingress.Spec.DefaultBackend); ok {
			all[backend] = struct{}{}
		} else {
			diagnoseUnsupportedIngressBackend(ingress, "default backend", ingress.Spec.DefaultBackend, emit)
		}
	}
	rules := map[string]map[Backend]struct{}{}
	hasHostname := len(overrides) != 0
	for ruleIndex, rule := range ingress.Spec.Rules {
		hasServiceBackend := false
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				hasBackendReference = hasBackendReference || path.Backend.Service != nil || path.Backend.Resource != nil
				hasServiceBackend = hasServiceBackend || path.Backend.Service != nil
				diagnoseUnsupportedIngressBackend(ingress, fmt.Sprintf("rule %d (%q)", ruleIndex, rule.Host), &path.Backend, emit)
			}
		}
		if rule.Host == "" || rule.HTTP == nil {
			if rule.Host == "" && hasServiceBackend && len(overrides) != 0 {
				emit(DiagnosticNormal, "IngressRuleWithoutHostname", fmt.Sprintf("Ingress %s/%s rule %d has no hostname; its backends are ignored even with hostname overrides", ingress.Namespace, ingress.Name, ruleIndex))
			}
			continue
		}
		host, err := NormalizeHostname(rule.Host)
		if err != nil {
			return nil, err
		}
		if rules[host] == nil {
			rules[host] = map[Backend]struct{}{}
		}
		hasHostname = true
		for _, path := range rule.HTTP.Paths {
			if backend, ok := ingressBackend(ingress.Namespace, &path.Backend); ok {
				rules[host][backend] = struct{}{}
				all[backend] = struct{}{}
			}
		}
	}
	if len(overrides) != 0 {
		if !hasBackendReference {
			emit(DiagnosticNormal, "NoBackendReferences", fmt.Sprintf("Ingress %s/%s has no backend references", ingress.Namespace, ingress.Name))
		}
		backends := backendSet(all)
		result := make([]HostProjection, 0, len(overrides))
		for _, hostname := range overrides {
			result = append(result, HostProjection{Hostname: hostname, Backends: backends})
		}
		return result, nil
	}
	for _, tls := range ingress.Spec.TLS {
		for _, raw := range tls.Hosts {
			host, err := NormalizeHostname(raw)
			if err != nil {
				return nil, err
			}
			if _, found := rules[host]; found {
				continue
			}
			hasHostname = true
			if ingress.Spec.DefaultBackend != nil {
				if backend, ok := ingressBackend(ingress.Namespace, ingress.Spec.DefaultBackend); ok {
					rules[host] = map[Backend]struct{}{backend: {}}
				} else {
					emit(DiagnosticWarning, "TLSHostWithoutBackend", fmt.Sprintf("Ingress %s/%s TLS-only hostname %q has no matching rule and its default backend is unsupported", ingress.Namespace, ingress.Name, host))
				}
			} else {
				emit(DiagnosticWarning, "TLSHostWithoutBackend", fmt.Sprintf("Ingress %s/%s TLS-only hostname %q has no matching rule or default backend", ingress.Namespace, ingress.Name, host))
			}
		}
	}
	if !hasHostname {
		emit(DiagnosticNormal, "NoPublishableHostname", fmt.Sprintf("Ingress %s/%s has no publishable hostname", ingress.Namespace, ingress.Name))
	}
	if !hasBackendReference {
		emit(DiagnosticNormal, "NoBackendReferences", fmt.Sprintf("Ingress %s/%s has no backend references", ingress.Namespace, ingress.Name))
	}
	return projectionSet(rules), nil
}

func diagnoseUnsupportedIngressBackend(ingress *networkingv1.Ingress, location string, backend *networkingv1.IngressBackend, diagnostic DiagnosticFunc) {
	if backend == nil || backend.Resource == nil {
		return
	}
	group := ""
	if backend.Resource.APIGroup != nil {
		group = *backend.Resource.APIGroup
	}
	diagnostic(DiagnosticWarning, "UnsupportedBackend", fmt.Sprintf("Ingress %s/%s %s uses unsupported resource backend %s/%s %q", ingress.Namespace, ingress.Name, location, group, backend.Resource.Kind, backend.Resource.Name))
}

func diagnosticOnce(diagnostic DiagnosticFunc) DiagnosticFunc {
	if diagnostic == nil {
		return func(string, string, string) {}
	}
	seen := map[string]struct{}{}
	return func(severity, reason, message string) {
		key := severity + "\x00" + reason + "\x00" + message
		if _, found := seen[key]; found {
			return
		}
		seen[key] = struct{}{}
		diagnostic(severity, reason, message)
	}
}

func ingressBackend(namespace string, backend *networkingv1.IngressBackend) (Backend, bool) {
	if backend == nil || backend.Service == nil || backend.Service.Name == "" {
		return Backend{}, false
	}
	return Backend{Namespace: namespace, Name: backend.Service.Name}, true
}

// RouteObject returns a new API object for a supported Gateway route kind.
func RouteObject(kind SourceKind) client.Object {
	switch kind {
	case SourceKindHTTPRoute:
		return &gatewayv1.HTTPRoute{}
	case SourceKindTCPRoute:
		return &gatewayv1.TCPRoute{}
	case SourceKindUDPRoute:
		return &gatewayv1.UDPRoute{}
	default:
		return nil
	}
}

// RouteKind reports the labdns source kind for a supported Gateway route.
func RouteKind(object client.Object) SourceKind {
	switch object.(type) {
	case *gatewayv1.HTTPRoute:
		return SourceKindHTTPRoute
	case *gatewayv1.TCPRoute:
		return SourceKindTCPRoute
	case *gatewayv1.UDPRoute:
		return SourceKindUDPRoute
	default:
		return ""
	}
}

// RouteParents returns the common parent references from a supported Gateway
// route. Unsupported objects have no parents.
func RouteParents(object client.Object) []gatewayv1.ParentReference {
	switch route := object.(type) {
	case *gatewayv1.HTTPRoute:
		return route.Spec.ParentRefs
	case *gatewayv1.TCPRoute:
		return route.Spec.ParentRefs
	case *gatewayv1.UDPRoute:
		return route.Spec.ParentRefs
	default:
		return nil
	}
}

// RouteBackendRefs returns each Service reference declared by the rules of a
// supported Gateway route, in rule order.
func RouteBackendRefs(object client.Object) []gatewayv1.BackendObjectReference {
	var refs []gatewayv1.BackendObjectReference
	switch route := object.(type) {
	case *gatewayv1.HTTPRoute:
		for _, rule := range route.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				refs = append(refs, ref.BackendObjectReference)
			}
		}
	case *gatewayv1.TCPRoute:
		for _, rule := range route.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				refs = append(refs, ref.BackendObjectReference)
			}
		}
	case *gatewayv1.UDPRoute:
		for _, rule := range route.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				refs = append(refs, ref.BackendObjectReference)
			}
		}
	}
	return refs
}

// HTTPRouteProjection preserves the existing HTTP route projection API.
func HTTPRouteProjection(ctx context.Context, reader client.Reader, route *gatewayv1.HTTPRoute, overrides []string, diagnostic DiagnosticFunc) ([]HostProjection, error) {
	return RouteProjection(ctx, reader, route, overrides, diagnostic)
}

// RouteProjection projects the backends of a supported Gateway route onto its
// publishable hostnames. HTTPRoute uses its spec hostnames when there are no
// overrides; TCPRoute and UDPRoute rely only on inherited hostname overrides.
func RouteProjection(ctx context.Context, reader client.Reader, route client.Object, overrides []string, diagnostic DiagnosticFunc) ([]HostProjection, error) {
	kind := RouteKind(route)
	if kind == "" {
		return nil, fmt.Errorf("unsupported Gateway route object %T", route)
	}

	emit := diagnosticOnce(diagnostic)
	all := map[Backend]struct{}{}
	refs := RouteBackendRefs(route)
	for _, ref := range refs {
		backend, ok, err := routeBackend(ctx, reader, route, kind, ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			emit(DiagnosticWarning, "UnsupportedBackend", fmt.Sprintf("%s %s/%s backendRef %q is unsupported or lacks a ReferenceGrant", kind, route.GetNamespace(), route.GetName(), ref.Name))
			continue
		}
		all[backend] = struct{}{}
	}
	hasBackendReference := len(refs) != 0
	hostnames := overrides
	if len(hostnames) == 0 && kind == SourceKindHTTPRoute {
		httpRoute := route.(*gatewayv1.HTTPRoute)
		for _, hostname := range httpRoute.Spec.Hostnames {
			normalized, err := NormalizeHostname(string(hostname))
			if err != nil {
				return nil, err
			}
			hostnames = append(hostnames, normalized)
		}
		hostnames = sortedUnique(hostnames)
	}
	if len(hostnames) == 0 {
		emit(DiagnosticNormal, "NoPublishableHostname", fmt.Sprintf("%s %s/%s has no publishable hostname", kind, route.GetNamespace(), route.GetName()))
	}
	if !hasBackendReference {
		emit(DiagnosticNormal, "NoBackendReferences", fmt.Sprintf("%s %s/%s has no backend references", kind, route.GetNamespace(), route.GetName()))
	}
	if len(all) == 0 {
		return nil, nil
	}
	backends := backendSet(all)
	result := make([]HostProjection, 0, len(hostnames))
	for _, hostname := range hostnames {
		result = append(result, HostProjection{Hostname: hostname, Backends: backends})
	}
	return result, nil
}

func routeBackend(ctx context.Context, routeReader client.Reader, route client.Object, kind SourceKind, ref gatewayv1.BackendObjectReference) (Backend, bool, error) {
	if ref.Group != nil && string(*ref.Group) != "" || ref.Kind != nil && string(*ref.Kind) != serviceKind {
		return Backend{}, false, nil
	}
	namespace := route.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}
	backend := Backend{Namespace: namespace, Name: string(ref.Name)}
	if namespace == route.GetNamespace() {
		return backend, true, nil
	}
	var grants gatewayv1beta1.ReferenceGrantList
	if err := routeReader.List(ctx, &grants, client.InNamespace(namespace)); err != nil {
		return Backend{}, false, fmt.Errorf("list ReferenceGrants in %s: %w", namespace, err)
	}
	for i := range grants.Items {
		grant := &grants.Items[i]
		fromOK := false
		for _, from := range grant.Spec.From {
			if string(from.Group) == gatewayv1.GroupName && string(from.Kind) == string(kind) && string(from.Namespace) == route.GetNamespace() {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, to := range grant.Spec.To {
			if string(to.Group) == "" && string(to.Kind) == serviceKind && (to.Name == nil || string(*to.Name) == backend.Name) {
				return backend, true, nil
			}
		}
	}
	return Backend{}, false, nil
}

func projectionSet(values map[string]map[Backend]struct{}) []HostProjection {
	result := make([]HostProjection, 0, len(values))
	for hostname, backends := range values {
		if len(backends) != 0 {
			result = append(result, HostProjection{Hostname: hostname, Backends: backendSet(backends)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hostname < result[j].Hostname })
	return result
}

func backendSet(values map[Backend]struct{}) []Backend {
	result := make([]Backend, 0, len(values))
	for backend := range values {
		result = append(result, backend)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result
}

func providerProperties(provider *labdnsv1alpha1.DNSProvider, annotations map[string]string) ([]Property, map[string]string) {
	properties := map[string]string{}
	for _, property := range provider.Spec.ProviderSpecific.Defaults {
		properties[property.Name] = property.Value
	}
	for _, key := range provider.Spec.ProviderSpecific.AnnotationKeys {
		if value, ok := annotations[string(key)]; ok {
			properties[string(key)] = value
		}
	}
	result := make([]Property, 0, len(properties))
	for name, value := range properties {
		result = append(result, Property{Name: name, Value: value})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Value < result[j].Value
		}
		return result[i].Name < result[j].Name
	})
	metadata := map[string]string{}
	for key, value := range annotations {
		if strings.HasPrefix(key, ExternalDNSPrefix) {
			metadata[key] = value
		}
	}
	return result, metadata
}
