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

package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/source"
)

const (
	BackendServiceIndex      = "labdns.backendService"
	IngressClassIndex        = "labdns.ingressClass"
	GatewayParentIndex       = "labdns.gatewayParent"
	GatewayClassIndex        = "labdns.gatewayClass"
	ProviderTokenIndex       = "labdns.providerToken"
	BackendNamespaceIndex    = "labdns.backendNamespace"
	EndpointNodeIndex        = "labdns.endpointNode"
	EndpointServiceIndex     = "labdns.endpointService"
	DNSEndpointSourceIndex   = "labdns.dnsEndpointSource"
	DNSEndpointProviderIndex = "labdns.dnsEndpointProvider"
)

// +kubebuilder:rbac:groups=labdns.shednet.dev,resources=dnsproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=externaldns.k8s.io,resources=dnsendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;ingressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// The default Kustomize deployment removes this generated rule; the opt-in
// Gateway API overlay and Helm value grant the same permissions conditionally.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes;gateways;gatewayclasses;referencegrants,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func Setup(ctx context.Context, mgr manager.Manager, output source.Output, gatewayEnabled bool, observed ...*Metrics) error {
	var metrics *Metrics
	if len(observed) != 0 {
		metrics = observed[0]
	}
	if err := addIndexes(ctx, mgr.GetFieldIndexer(), gatewayEnabled); err != nil {
		return err
	}
	mapper := newMapper(mgr.GetClient(), gatewayEnabled)
	// EnqueueRequestsFromMapFunc maps both ObjectOld and ObjectNew on updates.
	// This is required for label, backend, Node-placement, and class reassignment changes.
	ingress := &IngressReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorder("labdns-ingress"), Output: output, Resolver: source.Resolver{Reader: mgr.GetClient()}, Metrics: metrics}
	b := builder.ControllerManagedBy(mgr).Named("ingress-source").For(&networkingv1.Ingress{}).
		Watches(&networkingv1.IngressClass{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressClass)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressService)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressSlice)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressNode)).
		Watches(&labdnsv1alpha1.DNSProvider{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressProvider)).
		Watches(&externaldnsv1alpha1.DNSEndpoint{}, handler.EnqueueRequestsFromMapFunc(mapper.ingressEndpoint))
	if err := b.Complete(ingress); err != nil {
		return err
	}
	if !gatewayEnabled {
		return nil
	}
	route := &HTTPRouteReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorder("labdns-httproute"), Output: output, Resolver: source.Resolver{Reader: mgr.GetClient()}, Metrics: metrics}
	return builder.ControllerManagedBy(mgr).Named("httproute-source").For(&gatewayv1.HTTPRoute{}).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(mapper.routeGateway)).
		Watches(&gatewayv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(mapper.routeGatewayClass)).
		Watches(&gatewayv1beta1.ReferenceGrant{}, handler.EnqueueRequestsFromMapFunc(mapper.routeGrant)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(mapper.routeService)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(mapper.routeSlice)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(mapper.routeNode)).
		Watches(&labdnsv1alpha1.DNSProvider{}, handler.EnqueueRequestsFromMapFunc(mapper.routeProvider)).
		Watches(&externaldnsv1alpha1.DNSEndpoint{}, handler.EnqueueRequestsFromMapFunc(mapper.routeEndpoint)).
		Complete(route)
}

// SetupLifecycle installs durable deadline recovery when the production output supports it.
func SetupLifecycle(mgr manager.Manager, output lifecycleOutput, gatewayEnabled bool, observed ...*Metrics) error {
	var metrics *Metrics
	if len(observed) != 0 {
		metrics = observed[0]
	}
	reconciler := &LifecycleReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("labdns-lifecycle"), Output: output, GatewayEnabled: gatewayEnabled, Metrics: metrics}
	return builder.ControllerManagedBy(mgr).Named("dnsendpoint-lifecycle").For(&externaldnsv1alpha1.DNSEndpoint{}).Complete(reconciler)
}

func addIndexes(ctx context.Context, indexer client.FieldIndexer, gatewayEnabled bool) error {
	indexes := []struct {
		object client.Object
		field  string
		fn     client.IndexerFunc
	}{
		{&networkingv1.Ingress{}, BackendServiceIndex, ingressBackendKeys},
		{&networkingv1.Ingress{}, IngressClassIndex, ingressClassKeys},
		{&networkingv1.Ingress{}, ProviderTokenIndex, providerTokens},
		{&networkingv1.IngressClass{}, ProviderTokenIndex, providerTokens},
		{&discoveryv1.EndpointSlice{}, EndpointNodeIndex, endpointNodes},
		{&discoveryv1.EndpointSlice{}, EndpointServiceIndex, endpointService},
		{&externaldnsv1alpha1.DNSEndpoint{}, DNSEndpointSourceIndex, dnsEndpointSource},
		{&externaldnsv1alpha1.DNSEndpoint{}, DNSEndpointProviderIndex, dnsEndpointProvider},
	}
	if gatewayEnabled {
		indexes = append(indexes,
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, BackendServiceIndex, routeBackendKeys},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, GatewayParentIndex, routeParents},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, ProviderTokenIndex, providerTokens},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, BackendNamespaceIndex, routeBackendNamespaces},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.Gateway{}, GatewayClassIndex, gatewayClass},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.Gateway{}, ProviderTokenIndex, providerTokens},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.GatewayClass{}, ProviderTokenIndex, providerTokens},
		)
	}
	for _, index := range indexes {
		if err := indexer.IndexField(ctx, index.object, index.field, index.fn); err != nil {
			return err
		}
	}
	return nil
}

func ingressBackendKeys(object client.Object) []string {
	ingress := object.(*networkingv1.Ingress)
	values := map[string]struct{}{}
	if ingress.Spec.DefaultBackend != nil && ingress.Spec.DefaultBackend.Service != nil {
		values[ingress.Namespace+"/"+ingress.Spec.DefaultBackend.Service.Name] = struct{}{}
	}
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil {
					values[ingress.Namespace+"/"+path.Backend.Service.Name] = struct{}{}
				}
			}
		}
	}
	return keys(values)
}
func ingressClassKeys(object client.Object) []string {
	ingress := object.(*networkingv1.Ingress)
	if ingress.Spec.IngressClassName != nil {
		return []string{*ingress.Spec.IngressClassName}
	}
	if name := ingress.Annotations[legacyIngressClassAnnotation]; name != "" {
		return []string{name}
	}
	return nil
}
func routeBackendKeys(object client.Object) []string {
	route := object.(*gatewayv1.HTTPRoute)
	values := map[string]struct{}{}
	for _, rule := range route.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			if ref.Group != nil && string(*ref.Group) != "" || ref.Kind != nil && string(*ref.Kind) != "Service" {
				continue
			}
			namespace := route.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}
			values[namespace+"/"+string(ref.Name)] = struct{}{}
		}
	}
	return keys(values)
}
func routeBackendNamespaces(object client.Object) []string {
	values := map[string]struct{}{}
	for _, key := range routeBackendKeys(object) {
		values[strings.SplitN(key, "/", 2)[0]] = struct{}{}
	}
	return keys(values)
}
func routeParents(object client.Object) []string {
	route := object.(*gatewayv1.HTTPRoute)
	values := map[string]struct{}{}
	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && string(*ref.Group) != gatewayv1.GroupName || ref.Kind != nil && string(*ref.Kind) != "Gateway" {
			continue
		}
		namespace := route.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}
		values[namespace+"/"+string(ref.Name)] = struct{}{}
	}
	return keys(values)
}
func gatewayClass(object client.Object) []string {
	return []string{string(object.(*gatewayv1.Gateway).Spec.GatewayClassName)}
}
func providerTokens(object client.Object) []string {
	return splitTokens(object.GetAnnotations()[source.ProvidersAnnotation])
}
func endpointNodes(object client.Object) []string {
	slice := object.(*discoveryv1.EndpointSlice)
	values := map[string]struct{}{}
	for _, endpoint := range slice.Endpoints {
		if endpoint.NodeName != nil && *endpoint.NodeName != "" {
			values[*endpoint.NodeName] = struct{}{}
		}
	}
	return keys(values)
}
func endpointService(object client.Object) []string {
	slice := object.(*discoveryv1.EndpointSlice)
	if name := slice.Labels[discoveryv1.LabelServiceName]; name != "" {
		return []string{slice.Namespace + "/" + name}
	}
	return nil
}

func dnsEndpointSource(object client.Object) []string {
	if value := object.GetLabels()[source.AnnotationPrefix+"source-key"]; value != "" {
		return []string{value}
	}
	return nil
}

func dnsEndpointProvider(object client.Object) []string {
	if value := object.GetLabels()[source.AnnotationPrefix+"provider"]; value != "" {
		return []string{value}
	}
	return nil
}
func splitTokens(value string) []string {
	values := map[string]struct{}{}
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values[item] = struct{}{}
		}
	}
	return keys(values)
}
func keys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

type mapper struct {
	client  client.Client
	gateway bool
}

func newMapper(kubeClient client.Client, gateway bool) *mapper {
	return &mapper{client: kubeClient, gateway: gateway}
}
func requests(objects client.ObjectList) []reconcile.Request {
	result := []reconcile.Request{}
	switch list := objects.(type) {
	case *networkingv1.IngressList:
		for i := range list.Items {
			result = append(result, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	case *gatewayv1.HTTPRouteList:
		for i := range list.Items {
			result = append(result, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return result
}
func (m *mapper) ingresses(ctx context.Context, field, value string) []reconcile.Request {
	var list networkingv1.IngressList
	if !m.list(ctx, "Ingress dependencies", func(ctx context.Context) error {
		return m.client.List(ctx, &list, client.MatchingFields{field: value})
	}) {
		return nil
	}
	return requests(&list)
}
func (m *mapper) routes(ctx context.Context, field, value string) []reconcile.Request {
	var list gatewayv1.HTTPRouteList
	if !m.list(ctx, "HTTPRoute dependencies", func(ctx context.Context) error {
		return m.client.List(ctx, &list, client.MatchingFields{field: value})
	}) {
		return nil
	}
	return requests(&list)
}
func (m *mapper) ingressClass(ctx context.Context, object client.Object) []reconcile.Request {
	return m.ingresses(ctx, IngressClassIndex, object.GetName())
}
func (m *mapper) ingressService(ctx context.Context, object client.Object) []reconcile.Request {
	return m.ingresses(ctx, BackendServiceIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) routeService(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, BackendServiceIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) ingressSlice(ctx context.Context, object client.Object) []reconcile.Request {
	for _, key := range endpointService(object) {
		return m.ingresses(ctx, BackendServiceIndex, key)
	}
	return nil
}
func (m *mapper) routeSlice(ctx context.Context, object client.Object) []reconcile.Request {
	for _, key := range endpointService(object) {
		return m.routes(ctx, BackendServiceIndex, key)
	}
	return nil
}
func (m *mapper) ingressNode(ctx context.Context, object client.Object) []reconcile.Request {
	return m.sourcesForNode(ctx, object.GetName(), false)
}
func (m *mapper) routeNode(ctx context.Context, object client.Object) []reconcile.Request {
	return m.sourcesForNode(ctx, object.GetName(), true)
}
func (m *mapper) sourcesForNode(ctx context.Context, node string, routes bool) []reconcile.Request {
	var slices discoveryv1.EndpointSliceList
	if !m.list(ctx, "EndpointSlices by Node", func(ctx context.Context) error {
		return m.client.List(ctx, &slices, client.MatchingFields{EndpointNodeIndex: node})
	}) {
		return nil
	}
	result := []reconcile.Request{}
	for i := range slices.Items {
		for _, key := range endpointService(&slices.Items[i]) {
			if routes {
				result = append(result, m.routes(ctx, BackendServiceIndex, key)...)
			} else {
				result = append(result, m.ingresses(ctx, BackendServiceIndex, key)...)
			}
		}
	}
	return dedupe(result)
}
func (m *mapper) routeGateway(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, GatewayParentIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) routeGatewayClass(ctx context.Context, object client.Object) []reconcile.Request {
	var gateways gatewayv1.GatewayList
	if !m.list(ctx, "Gateways by GatewayClass", func(ctx context.Context) error {
		return m.client.List(ctx, &gateways, client.MatchingFields{GatewayClassIndex: object.GetName()})
	}) {
		return nil
	}
	result := []reconcile.Request{}
	for i := range gateways.Items {
		result = append(result, m.routeGateway(ctx, &gateways.Items[i])...)
	}
	return dedupe(result)
}
func (m *mapper) routeGrant(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, BackendNamespaceIndex, object.GetNamespace())
}
func (m *mapper) ingressProvider(ctx context.Context, object client.Object) []reconcile.Request {
	name := object.GetName()
	result := m.ingresses(ctx, ProviderTokenIndex, name)
	var classes networkingv1.IngressClassList
	if m.list(ctx, "IngressClasses by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &classes, client.MatchingFields{ProviderTokenIndex: name})
	}) {
		for i := range classes.Items {
			result = append(result, m.ingressClass(ctx, &classes.Items[i])...)
		}
	}
	return dedupe(result)
}
func (m *mapper) routeProvider(ctx context.Context, object client.Object) []reconcile.Request {
	name := object.GetName()
	result := m.routes(ctx, ProviderTokenIndex, name)
	var gateways gatewayv1.GatewayList
	if m.list(ctx, "Gateways by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &gateways, client.MatchingFields{ProviderTokenIndex: name})
	}) {
		for i := range gateways.Items {
			result = append(result, m.routeGateway(ctx, &gateways.Items[i])...)
		}
	}
	var classes gatewayv1.GatewayClassList
	if m.list(ctx, "GatewayClasses by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &classes, client.MatchingFields{ProviderTokenIndex: name})
	}) {
		for i := range classes.Items {
			result = append(result, m.routeGatewayClass(ctx, &classes.Items[i])...)
		}
	}
	return dedupe(result)
}

func (m *mapper) list(ctx context.Context, description string, operation func(context.Context) error) bool {
	backoff := wait.Backoff{
		Duration: 5 * time.Millisecond,
		Factor:   2,
		Jitter:   0.1,
		Steps:    6,
		Cap:      100 * time.Millisecond,
	}
	for {
		if err := ctx.Err(); err != nil {
			ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read canceled", "read", description, "error", err)
			return false
		}

		if err := operation(ctx); err == nil {
			return true
		} else {
			ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read failed; retrying", "read", description, "error", err)
		}

		timer := time.NewTimer(backoff.Step())
		select {
		case <-ctx.Done():
			timer.Stop()
			ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read canceled", "read", description, "error", ctx.Err())
			return false
		case <-timer.C:
		}
	}
}
func (m *mapper) ingressEndpoint(_ context.Context, object client.Object) []reconcile.Request {
	if object.GetAnnotations()[source.AnnotationPrefix+"source-kind"] != sourceKindIngress {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetAnnotations()[source.AnnotationPrefix+"source-namespace"], Name: object.GetAnnotations()[source.AnnotationPrefix+"source-name"]}}}
}
func (m *mapper) routeEndpoint(_ context.Context, object client.Object) []reconcile.Request {
	if object.GetAnnotations()[source.AnnotationPrefix+"source-kind"] != sourceKindHTTPRoute {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetAnnotations()[source.AnnotationPrefix+"source-namespace"], Name: object.GetAnnotations()[source.AnnotationPrefix+"source-name"]}}}
}
func dedupe(values []reconcile.Request) []reconcile.Request {
	seen := map[types.NamespacedName]struct{}{}
	result := []reconcile.Request{}
	for _, value := range values {
		if _, ok := seen[value.NamespacedName]; !ok {
			seen[value.NamespacedName] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
