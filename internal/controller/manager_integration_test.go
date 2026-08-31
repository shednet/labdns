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
	"io"
	"os"
	"path/filepath"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
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
	o.latest[identity.Kind+"/"+identity.Namespace+"/"+identity.Name] = publications
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
			if event.sequence > sequence && event.identity.Kind == kind && event.identity.Name == name && predicate(event.publications) {
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

func TestManagerWatchGraphAndDNSProviderAdmission(t *testing.T) { //nolint:gocyclo
	moduleCache := os.Getenv("GOMODCACHE")
	if moduleCache == "" {
		t.Skip("GOMODCACHE is required for pinned Gateway API CRD fixtures")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "test", "fixtures", "external-dns-v0.21.0"),
		filepath.Join(moduleCache, "sigs.k8s.io", "gateway-api@v1.5.1", "config", "crd", "standard"),
	}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	}()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, labdnsv1alpha1.AddToScheme, externaldnsv1alpha1.AddToScheme, gatewayv1.Install, gatewayv1beta1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	output := newWatchOutput()
	if err := Setup(context.Background(), mgr, output, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managerErrors := make(chan error, 1)
	go func() { managerErrors <- mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not synchronize")
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	validateExamplesThroughAPI(t, ctx, direct)

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
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ip"}}}}
	if err := direct.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if provider.Spec.RecordDefaults.TTL != 300 || provider.Spec.RecordDefaults.DeletionDelay == nil || provider.Spec.RecordDefaults.DeletionDelay.Duration != time.Minute {
		t.Fatalf("defaults not applied: %#v", provider.Spec.RecordDefaults)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}
	if err := direct.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	if err := direct.Create(ctx, service); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"network.example/ip": "192.0.2.1", "network.example/ip-next": "192.0.2.9"}}}
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	nodeName := "worker"
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeName, Addresses: []string{"10.0.0.1"}}}}
	if err := direct.Create(ctx, slice); err != nil {
		t.Fatal(err)
	}
	serviceTwo := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api-two", Namespace: "app"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	if err := direct.Create(ctx, serviceTwo); err != nil {
		t.Fatal(err)
	}
	nodeTwo := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-two", Labels: map[string]string{"network.example/ip": "198.51.100.1", "network.example/ip-next": "198.51.100.9"}}}
	if err := direct.Create(ctx, nodeTwo); err != nil {
		t.Fatal(err)
	}
	nodeNameTwo := "worker-two"
	sliceTwo := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api-two", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api-two"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeNameTwo, Addresses: []string{"10.0.0.2"}}}}
	if err := direct.Create(ctx, sliceTwo); err != nil {
		t.Fatal(err)
	}
	ingressClass := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "public", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}}, Spec: networkingv1.IngressClassSpec{Controller: "example.test/controller"}}
	if err := direct.Create(ctx, ingressClass); err != nil {
		t.Fatal(err)
	}
	ingressClassName := "public"
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules: []networkingv1.IngressRule{
				ingressRule("app.example.com", "api"),
				ingressRule("two.example.com", "api-two"),
			},
		},
	}
	if err := direct.Create(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", hostTargetsAre(map[string][]string{"app.example.com": {"192.0.2.1"}, "two.example.com": {"198.51.100.1"}}))

	if err := direct.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/ipv6"] = "v6-2001-db8--1"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatalf("Kubernetes API rejected encoded IPv6 Node label: %v", err)
	}
	ipv6Provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "encoded-v6"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv6: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/ipv6"}}}}
	if err := direct.Create(ctx, ipv6Provider); err != nil {
		t.Fatal(err)
	}
	ipv6Ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "ipv6", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "encoded-v6"}},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{ingressRule("ipv6.example.com", "api")}},
	}
	if err := direct.Create(ctx, ipv6Ingress); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "ipv6", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 1 && publications[0].Records[0].RecordType == "AAAA" && reflect.DeepEqual(publications[0].Records[0].Targets, []string{"2001:db8::1"})
	})

	ipv6StableOutput := &recordingOutput{}
	ipv6Reconciler := &IngressReconciler{Client: direct, Output: ipv6StableOutput, Resolver: source.Resolver{Reader: direct}}
	ipv6Request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "ipv6"}}
	if _, err := ipv6Reconciler.Reconcile(ctx, ipv6Request); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
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
	directReconciler := &IngressReconciler{Client: direct, Output: stableOutput, Resolver: source.Resolver{Reader: direct}}
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

	oldLabelIngress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "old-label", Namespace: "app"}, Spec: networkingv1.IngressSpec{
		IngressClassName: &ingressClassName, Rules: []networkingv1.IngressRule{ingressRule("old.example.com", "api")},
	}}
	if err := direct.Create(ctx, oldLabelIngress); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
	serviceSequence := output.mark()
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "api"}, service); err != nil {
		t.Fatal(err)
	}
	service.Annotations = map[string]string{"example.test/mutated": "true"}
	if err := direct.Update(ctx, service); err != nil {
		t.Fatal(err)
	}
	output.waitAfter(t, serviceSequence, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	output.drain()
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
	generated := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "generated-watch", Namespace: "app", Annotations: map[string]string{
		source.AnnotationPrefix + "source-kind": "Ingress", source.AnnotationPrefix + "source-namespace": "app", source.AnnotationPrefix + "source-name": "web",
	}}}
	if err := direct.Create(ctx, generated); err != nil {
		t.Fatal(err)
	}
	output.waitAfter(t, endpointSequence, "Ingress", "web", hostTargetsAre(map[string][]string{"app.example.com": {"192.0.2.1"}, "two.example.com": {"198.51.100.1"}}))

	output.drain()
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "api"}, slice); err != nil {
		t.Fatal(err)
	}
	slice.Endpoints[0].NodeName = &nodeNameTwo
	if err := direct.Update(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"198.51.100.1"}}))
	slice.Endpoints[0].NodeName = &nodeName
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
	slice = &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeName, Addresses: []string{"10.0.0.1"}}}}
	if err := direct.Create(ctx, slice); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "old-label", hostTargetsAre(map[string][]string{"old.example.com": {"192.0.2.1"}}))

	if err := direct.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/ip"] = "192.0.2.2"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", targetIs("192.0.2.2"))
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
	if err := direct.Get(ctx, client.ObjectKey{Name: "www"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.IPSources.IPv4.NodeLabel = "network.example/ip-next"
	if err := direct.Update(ctx, provider); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "web", targetIs("192.0.2.9"))

	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "class", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	if err := direct.Create(ctx, class); err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "class", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}}}
	if err := direct.Create(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	port80 := gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}}},
			Hostnames:       []gatewayv1.Hostname{"route.example.com"},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "api", Port: &port80}}}}}},
		},
	}
	if err := direct.Create(ctx, route); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "route", targetIs("192.0.2.9"))
	output.drain()
	if err := direct.Get(ctx, client.ObjectKey{Namespace: "app", Name: "gateway"}, gateway); err != nil {
		t.Fatal(err)
	}
	gateway.Annotations = map[string]string{source.ExternalDNSPrefix + "gateway-marker": "gateway"}
	if err := direct.Update(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "route", metadataAnnotationIs(source.ExternalDNSPrefix+"gateway-marker", "gateway"))
	backendNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "backend"}}
	if err := direct.Create(ctx, backendNamespace); err != nil {
		t.Fatal(err)
	}
	backendService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "backend"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	if err := direct.Create(ctx, backendService); err != nil {
		t.Fatal(err)
	}
	backendSlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "backend", Labels: map[string]string{discoveryv1.LabelServiceName: "cross"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeName, Addresses: []string{"10.0.0.2"}}}}
	if err := direct.Create(ctx, backendSlice); err != nil {
		t.Fatal(err)
	}
	crossRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}}},
			Hostnames:       []gatewayv1.Hostname{"cross.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: "cross", Namespace: ptr.To(gatewayv1.Namespace("backend")), Port: &port80,
			}}}}}},
		},
	}
	if err := direct.Create(ctx, crossRoute); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "cross", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
	grant := &gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Name: "allow-cross", Namespace: "backend"}, Spec: gatewayv1beta1.ReferenceGrantSpec{From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "app"}}, To: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1beta1.ObjectName("cross"))}}}}
	if err := direct.Create(ctx, grant); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "cross", targetIs("192.0.2.9"))
	output.drain()
	if err := direct.Delete(ctx, grant); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "cross", func(publications []source.Publication) bool {
		return len(publications) == 1 && len(publications[0].Records) == 0
	})
	tlsIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-only", Namespace: "app"},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules:            []networkingv1.IngressRule{ingressRule("tls-rule.example.com", "api")},
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{"tls.example.com"}}},
		},
	}
	if err := direct.Create(ctx, tlsIngress); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "Ingress", "tls-only", func(publications []source.Publication) bool {
		return hostTargetsAre(map[string][]string{"tls-rule.example.com": {"192.0.2.9"}})(publications)
	})
	waitForEventReason(t, ctx, direct, "app", "tls-only", "TLSHostWithoutBackend")
	secondClass := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "second", Annotations: map[string]string{source.ProvidersAnnotation: "vpn"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	if err := direct.Create(ctx, secondClass); err != nil {
		t.Fatal(err)
	}
	secondGateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "second", Listeners: []gatewayv1.Listener{{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}}}}
	if err := direct.Create(ctx, secondGateway); err != nil {
		t.Fatal(err)
	}
	ambiguous := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "ambiguous", Namespace: "app"}, Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}, {Name: "second"}}}, Hostnames: []gatewayv1.Hostname{"ambiguous.example.com"}}}
	if err := direct.Create(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	waitForEventReason(t, ctx, direct, "app", "ambiguous", "AmbiguousParents")
	class.Annotations[source.ProvidersAnnotation] = ""
	if err := direct.Update(ctx, class); err != nil {
		t.Fatal(err)
	}
	output.wait(t, "HTTPRoute", "route", func(publications []source.Publication) bool { return len(publications) == 0 })

	cancel()
	select {
	case err := <-managerErrors:
		if err != nil {
			t.Fatalf("manager: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop")
	}
}

func validateExamplesThroughAPI(t *testing.T, ctx context.Context, kubeClient client.Client) {
	t.Helper()
	providersFile, err := os.Open(filepath.Join("..", "..", "examples", "dnsproviders.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(providersFile, 4096)
	providers := []*labdnsv1alpha1.DNSProvider{}
	for {
		provider := &labdnsv1alpha1.DNSProvider{}
		if err := decoder.Decode(provider); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode DNSProvider example: %v", err)
		}
		if provider.Name == "" {
			continue
		}
		if err := kubeClient.Create(ctx, provider); err != nil {
			t.Fatalf("DNSProvider example %q rejected by API admission: %v", provider.Name, err)
		}
		providers = append(providers, provider)
	}
	if len(providers) != 2 {
		t.Fatalf("decoded %d DNSProvider examples, want 2", len(providers))
	}
	if err := providersFile.Close(); err != nil {
		t.Fatal(err)
	}

	ingressFile, err := os.Open(filepath.Join("..", "..", "examples", "ingress-split-horizon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ingress := &networkingv1.Ingress{}
	if err := utilyaml.NewYAMLOrJSONDecoder(ingressFile, 4096).Decode(ingress); err != nil {
		t.Fatalf("decode Ingress example through typed client: %v", err)
	}
	if err := ingressFile.Close(); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ingress.Namespace}}
	if err := kubeClient.Create(ctx, namespace); err != nil {
		t.Fatalf("create example namespace: %v", err)
	}
	if err := kubeClient.Create(ctx, ingress); err != nil {
		t.Fatalf("Ingress example rejected by Kubernetes API: %v", err)
	}
	if err := kubeClient.Delete(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	for _, provider := range providers {
		if err := kubeClient.Delete(ctx, provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := kubeClient.Delete(ctx, namespace); err != nil {
		t.Fatal(err)
	}
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
