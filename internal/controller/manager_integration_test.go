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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/source"
)

type outputEvent struct {
	identity     source.Identity
	publications []source.Publication
	sequence     int
}
type watchOutput struct {
	mu       sync.Mutex
	events   chan outputEvent
	latest   map[string][]source.Publication
	sequence int
}

func newWatchOutput() *watchOutput {
	return &watchOutput{events: make(chan outputEvent, 100), latest: map[string][]source.Publication{}}
}
func (o *watchOutput) Apply(_ context.Context, identity source.Identity, publications []source.Publication) error {
	o.mu.Lock()
	o.sequence++
	sequence := o.sequence
	o.latest[string(identity.Kind)+"/"+identity.Namespace+"/"+identity.Name] = publications
	o.mu.Unlock()
	select {
	case o.events <- outputEvent{identity: identity, publications: publications, sequence: sequence}:
	default:
	}
	return nil
}
func (o *watchOutput) wait(t *testing.T, kind, name string, predicate func([]source.Publication) bool) {
	o.waitAfter(t, 0, kind, name, predicate)
}

func (o *watchOutput) waitAfter(t *testing.T, sequence int, kind, name string, predicate func([]source.Publication) bool) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-o.events:
			if event.sequence > sequence && event.identity.Kind == source.SourceKind(kind) && event.identity.Name == name && predicate(event.publications) {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s/%s output", kind, name)
		}
	}
}

func (o *watchOutput) mark() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sequence
}

func (o *watchOutput) drain() {
	for {
		select {
		case <-o.events:
		default:
			return
		}
	}
}

func TestDNSProviderAdmission(t *testing.T) {
	_, _, direct := sharedIntegration(t)
	exerciseDNSProviderAdmission(t, context.Background(), direct)
}

func exerciseDNSProviderAdmission(t *testing.T, ctx context.Context, direct client.Client) {
	t.Helper()
	tooLongName := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 32) + "." + strings.Repeat("b", 32)}, Spec: validProviderSpec()}
	if err := direct.Create(ctx, tooLongName); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("overlong metadata.name admission error = %v", err)
	}
	maxPrefix := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	maxQualifiedName := maxPrefix + "/" + strings.Repeat("n", 63)
	boundary := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "qualified-boundary"}, Spec: validProviderSpec()}
	boundary.Spec.IPSources.IPv4.NodeLabel = maxQualifiedName
	boundary.Spec.ProviderSpecific.AnnotationKeys = []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(maxQualifiedName)}
	if err := direct.Create(ctx, boundary); err != nil {
		t.Fatalf("valid 317-byte qualified name rejected: %v", err)
	}
	if err := direct.Delete(ctx, boundary); err != nil {
		t.Fatal(err)
	}
	validIPv6 := validProviderSpec()
	validIPv6.IPSources = labdnsv1alpha1.IPSources{IPv6: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ipv6"}}
	validDual := validProviderSpec()
	validDual.IPSources.IPv6 = &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ipv6"}
	validMinimums := validProviderSpec()
	validMinimums.RecordDefaults = labdnsv1alpha1.RecordDefaults{TTL: 1, DeletionDelay: &metav1.Duration{}}
	validMaximumTTL := validProviderSpec()
	validMaximumTTL.RecordDefaults.TTL = 2147483647
	for name, spec := range map[string]labdnsv1alpha1.DNSProviderSpec{
		"ipv6-only": validIPv6, "dual-stack": validDual, "minimums": validMinimums, "maximum-ttl": validMaximumTTL,
	} {
		candidate := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "valid-" + name}, Spec: spec}
		if err := direct.Create(ctx, candidate); err != nil {
			t.Fatalf("valid %s provider rejected: %v", name, err)
		}
		if err := direct.Delete(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	for name, spec := range invalidProviderSpecs() {
		t.Run("admission_"+name, func(t *testing.T) {
			candidate := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "invalid-" + name}, Spec: spec}
			if err := direct.Create(ctx, candidate); err == nil || !apierrors.IsInvalid(err) {
				t.Fatalf("invalid provider admission error = %v", err)
			}
		})
	}
}

type managerWatchFixture struct {
	ctx              context.Context
	direct           client.Client
	output           *watchOutput
	ingressClassName string
	nodeName         string
	nodeNameTwo      string
}

func TestManagerWatchGraph(t *testing.T) {
	fixture := newManagerWatchFixture(t)
	t.Run("Ingress validation", func(t *testing.T) { exerciseManagerWatchIngressValidation(t, fixture) })
	t.Run("Ingress dependencies", func(t *testing.T) { exerciseManagerWatchIngressDependencies(t, fixture) })
	t.Run("Gateway", func(t *testing.T) { exerciseManagerWatchGateway(t, fixture) })
}

func newManagerWatchFixture(t *testing.T) *managerWatchFixture {
	t.Helper()
	config, scheme, direct := sharedIntegration(t)
	// Register resource cleanup before startManagedTestManager registers manager
	// shutdown, so the manager is stopped before cleanup talks to the API.
	t.Cleanup(func() {
		cleanupSharedNamespaces(t, "app", "backend")
		cleanupSharedClusterObjects(t, []string{"www", "encoded-v6"}, []string{"public"}, []string{"class", "second"})
		cleanupSharedNodes(t, "worker", "worker-two")
	})
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: controllerconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	output := newWatchOutput()
	if err := Setup(ctx, mgr, output, true, nil); err != nil {
		t.Fatal(err)
	}
	startManagedTestManager(t, mgr, ctx)

	provider := &labdnsv1alpha1.DNSProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "www"},
		Spec: labdnsv1alpha1.DNSProviderSpec{
			Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
			IPSources: labdnsv1alpha1.IPSources{
				IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ip"},
			},
		},
	}
	createManagerWatchObject(t, ctx, direct, provider)
	if provider.Spec.RecordDefaults.TTL != 300 || provider.Spec.RecordDefaults.DeletionDelay == nil || provider.Spec.RecordDefaults.DeletionDelay.Duration != time.Minute {
		t.Fatalf("defaults not applied: %#v", provider.Spec.RecordDefaults)
	}
	createManagerWatchObject(t, ctx, direct, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}})
	createManagerWatchObject(t, ctx, direct, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	})
	createManagerWatchObject(t, ctx, direct, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"network.example/ip": "192.0.2.1", "network.example/ip-next": "192.0.2.9"}},
	})
	createManagerWatchObject(t, ctx, direct, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{NodeName: new("worker"), Addresses: []string{"10.0.0.1"}}},
	})
	createManagerWatchObject(t, ctx, direct, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api-two", Namespace: "app"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	})
	createManagerWatchObject(t, ctx, direct, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-two", Labels: map[string]string{"network.example/ip": "198.51.100.1", "network.example/ip-next": "198.51.100.9"}},
	})
	createManagerWatchObject(t, ctx, direct, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "api-two", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api-two"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{NodeName: new("worker-two"), Addresses: []string{"10.0.0.2"}}},
	})
	createManagerWatchObject(t, ctx, direct, &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}},
		Spec:       networkingv1.IngressClassSpec{Controller: "example.test/controller"},
	})
	className := "public"
	createManagerWatchObject(t, ctx, direct, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &className,
			Rules: []networkingv1.IngressRule{
				ingressRule("app.example.com", "api"),
				ingressRule("two.example.com", "api-two"),
			},
		},
	})
	output.wait(t, "Ingress", "web", hostTargetsAre(map[string][]string{"app.example.com": {"192.0.2.1"}, "two.example.com": {"198.51.100.1"}}))
	return &managerWatchFixture{ctx: ctx, direct: direct, output: output, ingressClassName: className, nodeName: "worker", nodeNameTwo: "worker-two"}
}

func createManagerWatchObject(t *testing.T, ctx context.Context, direct client.Client, object client.Object) {
	t.Helper()
	if err := direct.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
}

func exerciseManagerWatchIngressValidation(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	node := &corev1.Node{}
	if err := direct.Get(ctx, client.ObjectKey{Name: f.nodeName}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/ipv6"] = "v6-2001-db8--1"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatalf("Kubernetes API rejected encoded IPv6 Node label: %v", err)
	}
	createManagerWatchObject(t, ctx, direct, &labdnsv1alpha1.DNSProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "encoded-v6"},
		Spec: labdnsv1alpha1.DNSProviderSpec{
			Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
			IPSources: labdnsv1alpha1.IPSources{
				IPv6: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ipv6"},
			},
		},
	})
	createManagerWatchObject(t, ctx, direct, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "ipv6", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "encoded-v6"}},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{ingressRule("ipv6.example.com", "api")}},
	})
	output.wait(t, "Ingress", "ipv6", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 1 && publications[0].Records[0].RecordType == "AAAA" && reflect.DeepEqual(publications[0].Records[0].Targets, []string{"2001:db8::1"})
	})

	ipv6StableOutput := &recordingOutput{}
	ipv6Reconciler := &ingressReconciler{Client: direct, Output: ipv6StableOutput, Resolver: source.Resolver{Reader: direct}}
	ipv6Request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "ipv6"}}
	if _, err := ipv6Reconciler.Reconcile(ctx, ipv6Request); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, client.ObjectKey{Name: f.nodeName}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/ipv6"] = "v6-2001-0db8--1"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := ipv6Reconciler.Reconcile(ctx, ipv6Request); err == nil {
		t.Fatal("noncanonical IPv6 Node label unexpectedly reconciled")
	}
	if ipv6StableOutput.calls != 1 {
		t.Fatalf("invalid IPv6 label altered output: calls=%d", ipv6StableOutput.calls)
	}
	node.Labels["network.example/ipv6"] = "v6-2001-db8--1"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	stableOutput := &recordingOutput{}
	directReconciler := &ingressReconciler{Client: direct, Output: stableOutput, Resolver: source.Resolver{Reader: direct}}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "web"}}
	if _, err := directReconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	stablePublications := append([]source.Publication(nil), stableOutput.publications...)
	canceledContext, cancelRead := context.WithCancel(ctx)
	cancelRead()
	if _, err := directReconciler.Reconcile(canceledContext, request); err == nil {
		t.Fatal("canceled reconciliation unexpectedly succeeded")
	}
	if stableOutput.calls != 1 || !reflect.DeepEqual(stableOutput.publications, stablePublications) {
		t.Fatalf("canceled reconciliation altered output: calls=%d publications=%#v", stableOutput.calls, stableOutput.publications)
	}
	transientClient := failingReaderClient{Client: direct}
	directReconciler.Client = transientClient
	directReconciler.Resolver.Reader = transientClient
	if _, err := directReconciler.Reconcile(ctx, request); err == nil {
		t.Fatal("transient read failure unexpectedly succeeded")
	}
	if stableOutput.calls != 1 || !reflect.DeepEqual(stableOutput.publications, stablePublications) {
		t.Fatalf("transient read failure altered output: calls=%d publications=%#v", stableOutput.calls, stableOutput.publications)
	}
}

func exerciseManagerWatchIngressDependencies(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	exerciseManagerWatchServiceAndClass(t, f)
	exerciseManagerWatchEndpointSlice(t, f)
	exerciseManagerWatchNodeAndProvider(t, f)
}

func exerciseManagerWatchServiceAndClass(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	createManagerWatchObject(t, ctx, direct, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "old-label", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &f.ingressClassName,
			Rules:            []networkingv1.IngressRule{ingressRule("old.example.com", "api")},
		},
	})
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
	serviceSequence := output.mark()
	service := &corev1.Service{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "api"}, service); err != nil {
		t.Fatal(err)
	}
	service.Annotations = map[string]string{"example.test/mutated": "true"}
	if err := direct.Update(ctx, service); err != nil {
		t.Fatalf("Kubernetes API rejected metadata-only Service update: %v", err)
	}
	output.waitAfter(t, serviceSequence, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
	ingressClass := &networkingv1.IngressClass{}
	if err := direct.Get(ctx, client.ObjectKey{Name: "public"}, ingressClass); err != nil {
		t.Fatal(err)
	}
	ingressClass.Annotations[source.ExternalDNSPrefix+"inherited-marker"] = "class"
	if err := direct.Update(ctx, ingressClass); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", metadataAnnotationIs(source.ExternalDNSPrefix+"inherited-marker", "class"))

	output.drain()
	endpointSequence := output.mark()
	createManagerWatchObject(t, ctx, direct, &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "generated-watch", Namespace: "app",
			Annotations: map[string]string{
				source.AnnotationPrefix + "source-kind":      "Ingress",
				source.AnnotationPrefix + "source-namespace": "app",
				source.AnnotationPrefix + "source-name":      "web",
			},
		},
	})
	output.waitAfter(t, endpointSequence, "Ingress", "web", hostTargetsAre(map[string][]string{"app.example.com": {"192.0.2.1"}, "two.example.com": {"198.51.100.1"}}))
}

func exerciseManagerWatchEndpointSlice(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	slice := &discoveryv1.EndpointSlice{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "api"}, slice); err != nil {
		t.Fatal(err)
	}
	slice.Endpoints[0].NodeName = &f.nodeNameTwo
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"198.51.100.1"}}))
	slice.Endpoints[0].NodeName = &f.nodeName
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
	slice.Labels[discoveryv1.LabelServiceName] = "api-two"
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
	slice.Labels[discoveryv1.LabelServiceName] = "api"
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
	if err := direct.Delete(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
	createManagerWatchObject(t, ctx, direct, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{NodeName: &f.nodeName, Addresses: []string{"10.0.0.1"}}},
	})
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))
}

func exerciseManagerWatchNodeAndProvider(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	node := &corev1.Node{}
	if err := direct.Get(ctx, client.ObjectKey{Name: f.nodeName}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/ip"] = "192.0.2.2"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", targetIs("192.0.2.2"))
	slice := &discoveryv1.EndpointSlice{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "api"}, slice); err != nil {
		t.Fatal(err)
	}
	ready := false
	slice.Endpoints[0].Conditions.Ready = &ready
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", func(publications []source.Publication) bool {
		return hostTargetsAre(map[string][]string{"two.example.com": {"198.51.100.1"}})(publications)
	})
	slice.Endpoints[0].Conditions.Ready = nil
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", targetIs("192.0.2.2"))
	provider := &labdnsv1alpha1.DNSProvider{}
	if err := direct.Get(ctx, client.ObjectKey{Name: "www"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.IPSources.IPv4.NodeLabel = "network.example/ip-next"
	if err := direct.Update(ctx, provider); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", targetIs("192.0.2.9"))
}

func exerciseManagerWatchGateway(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	exerciseManagerWatchGatewaySources(t, f)
	exerciseManagerWatchGatewayCrossNamespace(t, f)
	exerciseManagerWatchGatewayEvents(t, f)
}

func exerciseManagerWatchGatewaySources(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	createManagerWatchObject(t, ctx, direct, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"},
	})
	createManagerWatchObject(t, ctx, direct, &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "app"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "class", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	})
	port80 := gatewayv1.PortNumber(80)
	createManagerWatchObject(t, ctx, direct, &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}}},
			Hostnames:       []gatewayv1.Hostname{"route.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "api", Port: &port80},
			}}}}},
		},
	})
	output.wait(t, "HTTPRoute", "route", targetIs("192.0.2.9"))

	output.drain()
	gateway := &gatewayv1.Gateway{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "gateway"}, gateway); err != nil {
		t.Fatal(err)
	}
	gateway.Annotations = map[string]string{source.ExternalDNSPrefix + "gateway-marker": "gateway"}
	if err := direct.Update(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "route", metadataAnnotationIs(source.ExternalDNSPrefix+"gateway-marker", "gateway"))
}

func exerciseManagerWatchGatewayCrossNamespace(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	createManagerWatchObject(t, ctx, direct, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "backend"}})
	createManagerWatchObject(t, ctx, direct, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "backend"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	})
	createManagerWatchObject(t, ctx, direct, &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "cross", Namespace: "backend", Labels: map[string]string{discoveryv1.LabelServiceName: "cross"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{NodeName: &f.nodeName, Addresses: []string{"10.0.0.2"}}},
	})
	port80 := gatewayv1.PortNumber(80)
	createManagerWatchObject(t, ctx, direct, &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}}},
			Hostnames:       []gatewayv1.Hostname{"cross.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "cross", Namespace: ptr.To(gatewayv1.Namespace("backend")), Port: &port80},
			}}}}},
		},
	})
	output.wait(t, "HTTPRoute", "cross", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
	createManagerWatchObject(t, ctx, direct, &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-cross", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "app"}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1beta1.ObjectName("cross"))}},
		},
	})
	output.wait(t, "HTTPRoute", "cross", targetIs("192.0.2.9"))

	output.drain()
	grant := &gatewayv1beta1.ReferenceGrant{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "backend", Name: "allow-cross"}, grant); err != nil {
		t.Fatal(err)
	}
	if err := direct.Delete(ctx, grant); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "cross", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
}

func exerciseManagerWatchGatewayEvents(t *testing.T, f *managerWatchFixture) {
	t.Helper()
	ctx, direct, output := f.ctx, f.direct, f.output
	createManagerWatchObject(t, ctx, direct, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-only", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &f.ingressClassName,
			Rules:            []networkingv1.IngressRule{ingressRule("tls-rule.example.com", "api")},
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{"tls.example.com"}}},
		},
	})
	output.wait(t, "Ingress", "tls-only", func(publications []source.Publication) bool {
		return hostTargetsAre(map[string][]string{"tls-rule.example.com": {"192.0.2.9"}})(publications)
	})
	waitForEventReason(t, ctx, direct, "app", "tls-only", "TLSHostWithoutBackend")

	createManagerWatchObject(t, ctx, direct, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Annotations: map[string]string{source.ProvidersAnnotation: "vpn"}},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"},
	})
	createManagerWatchObject(t, ctx, direct, &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "app"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "second", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}},
	})
	createManagerWatchObject(t, ctx, direct, &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "ambiguous", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}, {Name: "second"}}},
			Hostnames:       []gatewayv1.Hostname{"ambiguous.example.com"},
		},
	})
	waitForEventReason(t, ctx, direct, "app", "ambiguous", "AmbiguousParents")

	class := &gatewayv1.GatewayClass{}
	if err := direct.Get(ctx, client.ObjectKey{Name: "class"}, class); err != nil {
		t.Fatal(err)
	}
	class.Annotations[source.ProvidersAnnotation] = ""
	if err := direct.Update(ctx, class); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "route", func(publications []source.Publication) bool { return len(publications) == 0 })
}

func invalidProviderSpecs() map[string]labdnsv1alpha1.DNSProviderSpec {
	noZones := validProviderSpec()
	noZones.Zones = nil
	duplicateZones := validProviderSpec()
	duplicateZones.Zones = append(duplicateZones.Zones, labdnsv1alpha1.DNSZone{Name: "example.com"})
	noIPs := validProviderSpec()
	noIPs.IPSources = labdnsv1alpha1.IPSources{}
	badLabel := validProviderSpec()
	badLabel.IPSources.IPv4.NodeLabel = "Bad Prefix!/label"
	uppercaseZone := validProviderSpec()
	uppercaseZone.Zones[0].Name = "Example.com"
	rootedZone := validProviderSpec()
	rootedZone.Zones[0].Name = "example.com."
	wildcardZone := validProviderSpec()
	wildcardZone.Zones[0].Name = "*.example.com"
	lowTTL := validProviderSpec()
	lowTTL.RecordDefaults.TTL = -1
	highTTL := validProviderSpec()
	highTTL.RecordDefaults.TTL = 2147483648
	negativeDelay := validProviderSpec()
	negativeDelay.RecordDefaults.DeletionDelay = &metav1.Duration{Duration: -time.Second}
	duplicateDefaults := validProviderSpec()
	duplicateDefaults.ProviderSpecific.Defaults = []labdnsv1alpha1.ProviderProperty{{Name: "key", Value: "one"}, {Name: "key", Value: "two"}}
	duplicateKeys := validProviderSpec()
	duplicateKeys.ProviderSpecific.AnnotationKeys = []labdnsv1alpha1.AnnotationKey{"example.test/key", "example.test/key"}
	badKey := validProviderSpec()
	badKey.ProviderSpecific.AnnotationKeys = []labdnsv1alpha1.AnnotationKey{"not a key"}
	emptyDefaultName := validProviderSpec()
	emptyDefaultName.ProviderSpecific.Defaults = []labdnsv1alpha1.ProviderProperty{{Name: "", Value: "value"}}
	overlongPrefix := validProviderSpec()
	prefix254 := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)
	overlongPrefix.IPSources.IPv4.NodeLabel = prefix254 + "/n"
	overlongPrefix.ProviderSpecific.AnnotationKeys = []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(prefix254 + "/n")}
	return map[string]labdnsv1alpha1.DNSProviderSpec{"no-zones": noZones, "duplicate-zones": duplicateZones, "no-ip-sources": noIPs, "bad-node-label": badLabel, "uppercase-zone": uppercaseZone, "trailing-dot-zone": rootedZone, "wildcard-zone": wildcardZone, "low-ttl": lowTTL, "high-ttl": highTTL, "negative-delay": negativeDelay, "duplicate-defaults": duplicateDefaults, "empty-default-name": emptyDefaultName, "duplicate-keys": duplicateKeys, "bad-annotation-key": badKey, "overlong-prefix": overlongPrefix}
}

func validProviderSpec() labdnsv1alpha1.DNSProviderSpec {
	return labdnsv1alpha1.DNSProviderSpec{
		Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
		IPSources: labdnsv1alpha1.IPSources{
			IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ip"},
		},
	}
}

func targetIs(want string) func([]source.Publication) bool {
	return func(publications []source.Publication) bool {
		for _, publication := range publications {
			for _, record := range publication.Records {
				if slices.Contains(record.Targets, want) {
					return true
				}
			}
		}
		return false
	}
}

func hostTargetsAre(want map[string][]string) func([]source.Publication) bool {
	return func(publications []source.Publication) bool {
		got := map[string][]string{}
		for _, publication := range publications {
			for _, record := range publication.Records {
				if record.RecordType == "A" {
					got[record.DNSName] = record.Targets
				}
			}
		}
		if len(got) != len(want) {
			return false
		}
		for hostname, targets := range want {
			if !slices.Equal(got[hostname], targets) {
				return false
			}
		}
		return true
	}
}

func metadataAnnotationIs(key, value string) func([]source.Publication) bool {
	return func(publications []source.Publication) bool {
		return len(publications) != 0 && publications[0].MetadataAnnotations[key] == value
	}
}

func ingressRule(host, service string) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host: host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", PathType: ptr.To(networkingv1.PathTypePrefix),
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: service, Port: networkingv1.ServiceBackendPort{Number: 80},
				}},
			}},
		}},
	}
}

func waitForEventReason(t *testing.T, ctx context.Context, kubeClient client.Client, namespace, name, reason string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var events eventsv1.EventList
		if err := kubeClient.List(ctx, &events, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
		for _, event := range events.Items {
			if event.Regarding.Name == name && event.Reason == reason {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event reason %s on %s/%s", reason, namespace, name)
}
