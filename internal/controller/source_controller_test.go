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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/dnsendpoint"
	"github.com/shednet/labdns/internal/source"
)

type recordingOutput struct {
	calls        int
	publications []source.Publication
}

func (o *recordingOutput) Apply(_ context.Context, _ source.Identity, publications []source.Publication) error {
	o.calls++
	o.publications = publications
	return nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, discoveryv1.AddToScheme, networkingv1.AddToScheme, labdnsv1alpha1.AddToScheme, externaldnsv1alpha1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func TestSourceReconcilerRetirementWriterCorruptionIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   source.SourceKind
		exists bool
	}{
		{name: "disabled Ingress", kind: source.SourceKindIngress, exists: true},
		{name: "deleted Ingress", kind: source.SourceKindIngress},
		{name: "disabled HTTPRoute", kind: source.SourceKindHTTPRoute, exists: true},
		{name: "deleted HTTPRoute", kind: source.SourceKindHTTPRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := source.Identity{Namespace: "app", Name: "source", UID: types.UID("uid"), Kind: tc.kind}
			if tc.kind == source.SourceKindIngress {
				identity.APIVersion = networkingv1.SchemeGroupVersion.String()
			} else {
				identity.APIVersion = gatewayv1.GroupVersion.String()
			}
			endpoint := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{
				Name: dnsendpoint.ObjectName(identity, "www"), Namespace: identity.Namespace,
				Labels: map[string]string{
					dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue,
					dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity),
					dnsendpoint.ProviderLabel:  "www",
				},
				Annotations: map[string]string{
					dnsendpoint.LifecycleAnnotation:     "{",
					dnsendpoint.DeletionDelayAnnotation: "1m0s",
				},
			}}
			objects := []client.Object{endpoint}
			if tc.exists && tc.kind == source.SourceKindIngress {
				objects = append(objects, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: identity.Name, Namespace: identity.Namespace, UID: identity.UID}})
			}
			if tc.exists && tc.kind == source.SourceKindHTTPRoute {
				objects = append(objects, &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: identity.Name, Namespace: identity.Namespace, UID: identity.UID}})
			}
			kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			output := dnsendpoint.NewWriter(kubeClient)
			request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: identity.Namespace, Name: identity.Name}}
			var err error
			if tc.kind == source.SourceKindIngress {
				_, err = (&ingressReconciler{Client: kubeClient, Output: output}).Reconcile(context.Background(), request)
			} else {
				_, err = (&httpRouteReconciler{Client: kubeClient, Output: output}).Reconcile(context.Background(), request)
			}
			if !errors.Is(err, reconcile.TerminalError(nil)) {
				t.Fatalf("error = %v, want terminal writer-state error", err)
			}
		})
	}
}

func TestHTTPRouteAmbiguousParentsPreservesOutput(t *testing.T) {
	scheme := testScheme(t)
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "true"}}, Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "one"}, {Name: "two"}}}}}
	classOne := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{source.ProvidersAnnotation: "www"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	classTwo := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "two", Annotations: map[string]string{source.ProvidersAnnotation: "vpn"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	objects := []client.Object{route, classOne, classTwo, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "one", Listeners: []gatewayv1.Listener{}}}, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "two", Listeners: []gatewayv1.Listener{}}}}
	output := &recordingOutput{}
	r := &httpRouteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Output: output}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "route"}}); err == nil {
		t.Fatal("expected ambiguity")
	}
	if output.calls != 0 {
		t.Fatalf("output called %d times", output.calls)
	}
}

func TestHTTPRouteOverridesConvergeParentChainsAndPreserveProviderIsolation(t *testing.T) {
	scheme := testScheme(t)
	cloudflareKey := source.ExternalDNSPrefix + "cloudflare-proxied"
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Annotations: map[string]string{
		source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www,vpn", cloudflareKey: "",
	}}, Spec: gatewayv1.HTTPRouteSpec{
		CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "one"}, {Name: "two"}}},
		Hostnames:       []gatewayv1.Hostname{"app.example.com"},
		Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "api"},
		}}}}},
	}}
	classOne := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{source.ProvidersAnnotation: "www", cloudflareKey: "true"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	classTwo := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "two", Annotations: map[string]string{source.ProvidersAnnotation: "vpn", cloudflareKey: "false"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	providerSpec := labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/ip"}}}
	wwwProvider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: providerSpec}
	wwwProvider.Spec.ProviderSpecific.AnnotationKeys = []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(cloudflareKey)}
	vpnProvider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "vpn"}, Spec: providerSpec}
	nodeName := "worker"
	objects := []client.Object{route, classOne, classTwo,
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "one", Listeners: []gatewayv1.Listener{}}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "two", Listeners: []gatewayv1.Listener{}}},
		wwwProvider, vpnProvider,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: map[string]string{"example.test/ip": "192.0.2.1"}}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeName}}},
	}
	output := &recordingOutput{}
	r := &httpRouteReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Output:   output,
		Resolver: source.Resolver{},
	}
	r.Resolver.Reader = r.Client
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(route)}); err != nil {
		t.Fatal(err)
	}
	if output.calls != 1 || len(output.publications) != 2 {
		t.Fatalf("output calls=%d publications=%#v, want one publication for each provider", output.calls, output.publications)
	}
	for _, publication := range output.publications {
		if len(publication.Records) != 1 || publication.Records[0].DNSName != "app.example.com" || publication.Records[0].Targets[0] != "192.0.2.1" {
			t.Fatalf("unexpected %s publication: %#v", publication.ProviderName, publication)
		}
		switch publication.ProviderName {
		case "www":
			if len(publication.Records[0].ProviderSpecific) != 1 || publication.Records[0].ProviderSpecific[0].Name != cloudflareKey || publication.Records[0].ProviderSpecific[0].Value != "" {
				t.Fatalf("www provider-specific properties = %#v", publication.Records[0].ProviderSpecific)
			}
		case "vpn":
			if len(publication.Records[0].ProviderSpecific) != 0 {
				t.Fatalf("vpn provider-specific properties = %#v", publication.Records[0].ProviderSpecific)
			}
		default:
			t.Fatalf("unexpected provider publication: %#v", publication)
		}
	}
}

type failingReaderClient struct{ client.Client }

func (f failingReaderClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*networkingv1.IngressList); ok {
		return f.Client.List(ctx, list, options...)
	}
	return errors.New("transient API failure")
}

type flakyListClient struct {
	client.Client
	failures int
	calls    int
}

func (f *flakyListClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errors.New("transient list failure")
	}
	return f.Client.List(ctx, list, options...)
}

func TestMapperFallsBackToBroadListAfterIndexedFailure(t *testing.T) {
	scheme := testScheme(t)
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"}, Spec: networkingv1.IngressSpec{
		Rules: []networkingv1.IngressRule{ingressRule("app.example.com", "api")},
	}}
	unrelated := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "app"}, Spec: networkingv1.IngressSpec{
		Rules: []networkingv1.IngressRule{ingressRule("other.example.com", "other-service")},
	}}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ingress, unrelated).
		WithIndex(&networkingv1.Ingress{}, backendServiceIndex, ingressBackendKeys).
		Build()
	const failures = 1
	flaky := &flakyListClient{Client: base, failures: failures}
	requests := newMapper(flaky).ingressService(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"},
	})
	if flaky.calls != failures+1 {
		t.Fatalf("List calls = %d, want %d", flaky.calls, failures+1)
	}
	if len(requests) != 2 || requests[0].NamespacedName != (types.NamespacedName{Namespace: "app", Name: "other"}) ||
		requests[1].NamespacedName != (types.NamespacedName{Namespace: "app", Name: "web"}) {
		t.Fatalf("unexpected mapped requests: %#v", requests)
	}
}

func TestMapperStopsImmediatelyOnContextCancellation(t *testing.T) {
	scheme := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&networkingv1.Ingress{}, backendServiceIndex, ingressBackendKeys).
		Build()
	flaky := &flakyListClient{Client: base, failures: 100}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	requests := newMapper(flaky).ingressService(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"},
	})
	if len(requests) != 0 {
		t.Fatalf("unexpected requests: %#v", requests)
	}
	if flaky.calls != 0 {
		t.Fatalf("List calls = %d, want 0 for canceled context", flaky.calls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("mapper ignored context cancellation for %s", elapsed)
	}
}

func TestIngressReadFailurePreservesOutput(t *testing.T) {
	scheme := testScheme(t)
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "ing", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "app.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api"}},
			}}}},
		}}},
	}
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/ip"}}}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress, provider, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"}}).Build()
	output := &recordingOutput{}
	wrapped := failingReaderClient{Client: base}
	r := &ingressReconciler{Client: wrapped, Output: output, Resolver: source.Resolver{Reader: wrapped}}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "ing"}}); err == nil {
		t.Fatal("expected read error")
	}
	if output.calls != 0 {
		t.Fatalf("output called %d times", output.calls)
	}
}

func TestIngressInvalidAnnotationsAreTerminal(t *testing.T) {
	scheme := testScheme(t)
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "ing", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "sometimes"},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()
	r := &ingressReconciler{Client: kubeClient, Output: &recordingOutput{}}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ingress)})
	if !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("error = %v, want terminal error", err)
	}
}

func TestIngressMissingClassIsTerminalAndWarns(t *testing.T) {
	t.Parallel()
	scheme := testScheme(t)
	className := "missing"
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "ing", Namespace: "app",
	}, Spec: networkingv1.IngressSpec{IngressClassName: &className}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()
	recorder := events.NewFakeRecorder(1)
	r := &ingressReconciler{Client: kubeClient, Output: &recordingOutput{}, Recorder: recorder}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ingress)})
	if !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("error = %v, want terminal error", err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "IngressClassNotFound") {
			t.Fatalf("event=%q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing-class warning event not emitted")
	}
}

func TestIngressMissingProviderWarnsAndDeselects(t *testing.T) {
	scheme := testScheme(t)
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "ing", Namespace: "app", Annotations: map[string]string{
			source.EnabledAnnotation: "true", source.ProvidersAnnotation: "missing",
		},
	}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "app.example.com"}}}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ingress).Build()
	recorder := events.NewFakeRecorder(1)
	output := &recordingOutput{}
	r := &ingressReconciler{Client: kubeClient, Output: output, Recorder: recorder}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ingress)}); err != nil {
		t.Fatal(err)
	}
	if output.calls != 1 || len(output.publications) != 0 {
		t.Fatalf("output calls=%d publications=%#v, want one empty publication set", output.calls, output.publications)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "DNSProviderNotFound") {
			t.Fatalf("event=%q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing-provider warning event not emitted")
	}
}
