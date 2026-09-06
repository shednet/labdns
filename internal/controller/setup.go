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
	stdslices "slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
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
	backendServiceIndex      = "labdns.backendService"
	ingressClassIndex        = "labdns.ingressClass"
	gatewayParentIndex       = "labdns.gatewayParent"
	gatewayClassIndex        = "labdns.gatewayClass"
	providerTokenIndex       = "labdns.providerToken"
	backendNamespaceIndex    = "labdns.backendNamespace"
	endpointNodeIndex        = "labdns.endpointNode"
	endpointServiceIndex     = "labdns.endpointService"
	dnsEndpointSourceIndex   = "labdns.dnsEndpointSource"
	dnsEndpointProviderIndex = "labdns.dnsEndpointProvider"
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
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func Setup(ctx context.Context, mgr manager.Manager, output source.Output, gatewayEnabled bool, metrics *Metrics) error {
	if err := addIndexes(ctx, mgr.GetFieldIndexer(), gatewayEnabled); err != nil {
		return err
	}
	mapper := newMapper(mgr.GetClient())
	// EnqueueRequestsFromMapFunc maps both ObjectOld and ObjectNew on updates.
	// This is required for label, backend, Node-placement, and class reassignment changes.
	ingress := &ingressReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("labdns-ingress"), Output: output, Resolver: source.Resolver{Reader: mgr.GetClient()}, Metrics: metrics}
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
	route := &httpRouteReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("labdns-httproute"), Output: output, Resolver: source.Resolver{Reader: mgr.GetClient()}, Metrics: metrics}
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
func SetupLifecycle(mgr manager.Manager, output lifecycleOutput, gatewayEnabled bool, metrics *Metrics) error {
	reconciler := &lifecycleReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("labdns-lifecycle"), Output: output, GatewayEnabled: gatewayEnabled, Metrics: metrics}
	return builder.ControllerManagedBy(mgr).Named("dnsendpoint-lifecycle").For(&externaldnsv1alpha1.DNSEndpoint{}).Complete(reconciler)
}

func addIndexes(ctx context.Context, indexer client.FieldIndexer, gatewayEnabled bool) error {
	indexes := []struct {
		object client.Object
		field  string
		fn     client.IndexerFunc
	}{
		{&networkingv1.Ingress{}, backendServiceIndex, ingressBackendKeys},
		{&networkingv1.Ingress{}, ingressClassIndex, ingressClassKeys},
		{&networkingv1.Ingress{}, providerTokenIndex, providerTokens},
		{&networkingv1.IngressClass{}, providerTokenIndex, providerTokens},
		{&discoveryv1.EndpointSlice{}, endpointNodeIndex, endpointNodes},
		{&discoveryv1.EndpointSlice{}, endpointServiceIndex, endpointService},
		{&externaldnsv1alpha1.DNSEndpoint{}, dnsEndpointSourceIndex, dnsEndpointSource},
		{&externaldnsv1alpha1.DNSEndpoint{}, dnsEndpointProviderIndex, dnsEndpointProvider},
	}
	if gatewayEnabled {
		indexes = append(indexes,
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, backendServiceIndex, routeBackendKeys},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, gatewayParentIndex, routeParents},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, providerTokenIndex, providerTokens},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.HTTPRoute{}, backendNamespaceIndex, routeBackendNamespaces},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.Gateway{}, gatewayClassIndex, gatewayClass},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.Gateway{}, providerTokenIndex, providerTokens},
			struct {
				object client.Object
				field  string
				fn     client.IndexerFunc
			}{&gatewayv1.GatewayClass{}, providerTokenIndex, providerTokens},
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
	client client.Client
}

func newMapper(kubeClient client.Client) *mapper {
	return &mapper{client: kubeClient}
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
	if err := m.list(ctx, "Ingress dependencies", func(ctx context.Context) error {
		return m.client.List(ctx, &list, client.MatchingFields{field: value})
	}); err == nil {
		return requests(&list)
	}
	// A cache may not have the requested field index during startup or after a
	// cache rebuild. Enqueueing every source is conservative and lets each
	// reconciler re-check the actual dependency.
	list = networkingv1.IngressList{}
	if err := m.list(ctx, "all Ingress sources after indexed lookup failure", func(ctx context.Context) error {
		return m.client.List(ctx, &list)
	}); err != nil {
		m.terminalListFailure(ctx, "Ingress dependencies", err)
		return nil
	}
	return requests(&list)
}
func (m *mapper) routes(ctx context.Context, field, value string) []reconcile.Request {
	var list gatewayv1.HTTPRouteList
	if err := m.list(ctx, "HTTPRoute dependencies", func(ctx context.Context) error {
		return m.client.List(ctx, &list, client.MatchingFields{field: value})
	}); err == nil {
		return requests(&list)
	}
	list = gatewayv1.HTTPRouteList{}
	if err := m.list(ctx, "all HTTPRoute sources after indexed lookup failure", func(ctx context.Context) error {
		return m.client.List(ctx, &list)
	}); err != nil {
		m.terminalListFailure(ctx, "HTTPRoute dependencies", err)
		return nil
	}
	return requests(&list)
}
func (m *mapper) ingressClass(ctx context.Context, object client.Object) []reconcile.Request {
	return m.ingresses(ctx, ingressClassIndex, object.GetName())
}
func (m *mapper) ingressService(ctx context.Context, object client.Object) []reconcile.Request {
	return m.ingresses(ctx, backendServiceIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) routeService(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, backendServiceIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) ingressSlice(ctx context.Context, object client.Object) []reconcile.Request {
	for _, key := range endpointService(object) {
		return m.ingresses(ctx, backendServiceIndex, key)
	}
	return nil
}
func (m *mapper) routeSlice(ctx context.Context, object client.Object) []reconcile.Request {
	for _, key := range endpointService(object) {
		return m.routes(ctx, backendServiceIndex, key)
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
	if err := m.list(ctx, "EndpointSlices by Node", func(ctx context.Context) error {
		return m.client.List(ctx, &slices, client.MatchingFields{endpointNodeIndex: node})
	}); err != nil {
		slices = discoveryv1.EndpointSliceList{}
		if err := m.list(ctx, "all EndpointSlices after node index lookup failure", func(ctx context.Context) error {
			return m.client.List(ctx, &slices)
		}); err != nil {
			m.terminalListFailure(ctx, "EndpointSlices by Node", err)
			return nil
		}
	}
	result := []reconcile.Request{}
	for i := range slices.Items {
		if !containsString(endpointNodes(&slices.Items[i]), node) {
			continue
		}
		for _, key := range endpointService(&slices.Items[i]) {
			if routes {
				result = append(result, m.routes(ctx, backendServiceIndex, key)...)
			} else {
				result = append(result, m.ingresses(ctx, backendServiceIndex, key)...)
			}
		}
	}
	return dedupe(result)
}
func (m *mapper) routeGateway(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, gatewayParentIndex, object.GetNamespace()+"/"+object.GetName())
}
func (m *mapper) routeGatewayClass(ctx context.Context, object client.Object) []reconcile.Request {
	var gateways gatewayv1.GatewayList
	if err := m.list(ctx, "Gateways by GatewayClass", func(ctx context.Context) error {
		return m.client.List(ctx, &gateways, client.MatchingFields{gatewayClassIndex: object.GetName()})
	}); err != nil {
		gateways = gatewayv1.GatewayList{}
		if err := m.list(ctx, "all Gateways after class index lookup failure", func(ctx context.Context) error {
			return m.client.List(ctx, &gateways)
		}); err != nil {
			m.terminalListFailure(ctx, "Gateways by GatewayClass", err)
			return nil
		}
	}
	result := []reconcile.Request{}
	for i := range gateways.Items {
		if string(gateways.Items[i].Spec.GatewayClassName) != object.GetName() {
			continue
		}
		result = append(result, m.routeGateway(ctx, &gateways.Items[i])...)
	}
	return dedupe(result)
}
func (m *mapper) routeGrant(ctx context.Context, object client.Object) []reconcile.Request {
	return m.routes(ctx, backendNamespaceIndex, object.GetNamespace())
}
func (m *mapper) ingressProvider(ctx context.Context, object client.Object) []reconcile.Request {
	name := object.GetName()
	result := m.ingresses(ctx, providerTokenIndex, name)
	var classes networkingv1.IngressClassList
	if err := m.list(ctx, "IngressClasses by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &classes, client.MatchingFields{providerTokenIndex: name})
	}); err != nil {
		classes = networkingv1.IngressClassList{}
		if err := m.list(ctx, "all IngressClasses after provider index lookup failure", func(ctx context.Context) error {
			return m.client.List(ctx, &classes)
		}); err != nil {
			m.terminalListFailure(ctx, "IngressClasses by provider", err)
			return dedupe(result)
		}
	}
	for i := range classes.Items {
		if !containsString(providerTokens(&classes.Items[i]), name) {
			continue
		}
		result = append(result, m.ingressClass(ctx, &classes.Items[i])...)
	}
	return dedupe(result)
}
func (m *mapper) routeProvider(ctx context.Context, object client.Object) []reconcile.Request {
	name := object.GetName()
	result := m.routes(ctx, providerTokenIndex, name)
	var gateways gatewayv1.GatewayList
	if err := m.list(ctx, "Gateways by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &gateways, client.MatchingFields{providerTokenIndex: name})
	}); err != nil {
		gateways = gatewayv1.GatewayList{}
		if err := m.list(ctx, "all Gateways after provider index lookup failure", func(ctx context.Context) error {
			return m.client.List(ctx, &gateways)
		}); err != nil {
			m.terminalListFailure(ctx, "Gateways by provider", err)
			return dedupe(result)
		}
	}
	for i := range gateways.Items {
		if !containsString(providerTokens(&gateways.Items[i]), name) {
			continue
		}
		result = append(result, m.routeGateway(ctx, &gateways.Items[i])...)
	}
	var classes gatewayv1.GatewayClassList
	if err := m.list(ctx, "GatewayClasses by provider", func(ctx context.Context) error {
		return m.client.List(ctx, &classes, client.MatchingFields{providerTokenIndex: name})
	}); err != nil {
		classes = gatewayv1.GatewayClassList{}
		if err := m.list(ctx, "all GatewayClasses after provider index lookup failure", func(ctx context.Context) error {
			return m.client.List(ctx, &classes)
		}); err != nil {
			m.terminalListFailure(ctx, "GatewayClasses by provider", err)
			return dedupe(result)
		}
	}
	for i := range classes.Items {
		if !containsString(providerTokens(&classes.Items[i]), name) {
			continue
		}
		result = append(result, m.routeGatewayClass(ctx, &classes.Items[i])...)
	}
	return dedupe(result)
}

func (m *mapper) list(ctx context.Context, description string, operation func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read canceled", "read", description, "error", err)
		return err
	}
	if err := operation(ctx); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read failed", "read", description, "error", err)
		return err
	}
	return nil
}

func (m *mapper) terminalListFailure(ctx context.Context, description string, err error) {
	if ctx.Err() != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("dependency mapping read canceled", "read", description, "error", ctx.Err())
		return
	}
	ctrl.LoggerFrom(ctx).Error(err, "dependency mapping read exhausted fallback", "read", description)
}

func containsString(values []string, wanted string) bool {
	return stdslices.Contains(values, wanted)
}
func (m *mapper) ingressEndpoint(_ context.Context, object client.Object) []reconcile.Request {
	if object.GetAnnotations()[source.AnnotationPrefix+"source-kind"] != string(sourceKindIngress) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetAnnotations()[source.AnnotationPrefix+"source-namespace"], Name: object.GetAnnotations()[source.AnnotationPrefix+"source-name"]}}}
}
func (m *mapper) routeEndpoint(_ context.Context, object client.Object) []reconcile.Request {
	if object.GetAnnotations()[source.AnnotationPrefix+"source-kind"] != string(sourceKindHTTPRoute) {
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
