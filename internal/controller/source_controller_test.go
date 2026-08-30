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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
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
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, networkingv1.AddToScheme, labdnsv1alpha1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func TestHTTPRouteAmbiguousParentsPreservesOutput(t *testing.T) {
	scheme := testScheme(t)
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", Annotations: map[string]string{source.EnabledAnnotation: "true"}}, Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "one"}, {Name: "two"}}}}}
	classOne := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{source.ProvidersAnnotation: "www"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	classTwo := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "two", Annotations: map[string]string{source.ProvidersAnnotation: "vpn"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	objects := []client.Object{route, classOne, classTwo, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "one", Listeners: []gatewayv1.Listener{}}}, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "two", Listeners: []gatewayv1.Listener{}}}}
	output := &recordingOutput{}
	r := &HTTPRouteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Output: output}
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
	}}, Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "one"}, {Name: "two"}}}}}
	classOne := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{source.ProvidersAnnotation: "www", cloudflareKey: "true"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	classTwo := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "two", Annotations: map[string]string{source.ProvidersAnnotation: "vpn", cloudflareKey: "false"}}, Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.test/controller"}}
	objects := []client.Object{route, classOne, classTwo,
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "one", Listeners: []gatewayv1.Listener{}}},
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: "app"}, Spec: gatewayv1.GatewaySpec{GatewayClassName: "two", Listeners: []gatewayv1.Listener{}}},
	}
	r := &HTTPRouteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
	annotations, err := r.routeAnnotations(context.Background(), route)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := source.ParseAnnotations(annotations)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Providers) != 2 || parsed.Providers[0] != "vpn" || parsed.Providers[1] != "www" {
		t.Fatalf("providers = %#v", parsed.Providers)
	}
	www := &labdnsv1alpha1.DNSProvider{Spec: labdnsv1alpha1.DNSProviderSpec{ProviderSpecific: labdnsv1alpha1.ProviderSpecific{AnnotationKeys: []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(cloudflareKey)}}}}
	vpn := &labdnsv1alpha1.DNSProvider{}
	wwwProperties, _ := source.ProviderProperties(www, annotations)
	vpnProperties, _ := source.ProviderProperties(vpn, annotations)
	if len(wwwProperties) != 1 || wwwProperties[0].Value != "" || len(vpnProperties) != 0 {
		t.Fatalf("provider isolation failed: www=%#v vpn=%#v", wwwProperties, vpnProperties)
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

func TestMapperRetriesTransientListFailure(t *testing.T) {
	scheme := testScheme(t)
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"}, Spec: networkingv1.IngressSpec{
		Rules: []networkingv1.IngressRule{ingressRule("app.example.com", "api")},
	}}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ingress).
		WithIndex(&networkingv1.Ingress{}, BackendServiceIndex, ingressBackendKeys).
		Build()
	const failures = 8
	flaky := &flakyListClient{Client: base, failures: failures}
	requests := newMapper(flaky, false).ingressService(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"},
	})
	if flaky.calls != failures+1 {
		t.Fatalf("List calls = %d, want %d", flaky.calls, failures+1)
	}
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Namespace: "app", Name: "web"}) {
		t.Fatalf("unexpected mapped requests: %#v", requests)
	}
}

func TestMapperRetryStopsOnContextCancellation(t *testing.T) {
	scheme := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&networkingv1.Ingress{}, BackendServiceIndex, ingressBackendKeys).
		Build()
	flaky := &flakyListClient{Client: base, failures: 100}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	requests := newMapper(flaky, false).ingressService(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"},
	})
	if len(requests) != 0 {
		t.Fatalf("unexpected requests: %#v", requests)
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
	r := &IngressReconciler{Client: wrapped, Output: output, Resolver: source.Resolver{Reader: wrapped}}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "app", Name: "ing"}}); err == nil {
		t.Fatal("expected read error")
	}
	if output.calls != 0 {
		t.Fatalf("output called %d times", output.calls)
	}
}
