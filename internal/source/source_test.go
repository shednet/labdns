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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

func TestAnnotationPrecedenceAndParsing(t *testing.T) {
	merged := MergeAnnotations(
		map[string]string{EnabledAnnotation: "true", ProvidersAnnotation: "vpn", ExternalDNSPrefix + "cloudflare-proxied": "true", "ignored": "x"},
		map[string]string{ProvidersAnnotation: " www, vpn,www ", ExternalDNSPrefix + "cloudflare-proxied": ""},
	)
	parsed, err := ParseAnnotations(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Enabled || len(parsed.Providers) != 2 || parsed.Providers[0] != "vpn" || parsed.Providers[1] != "www" {
		t.Fatalf("unexpected parse: %#v", parsed)
	}
	if value, ok := parsed.Resolved[ExternalDNSPrefix+"cloudflare-proxied"]; !ok || value != "" {
		t.Fatalf("explicit empty override lost: %#v", parsed.Resolved)
	}
	if _, ok := parsed.Resolved["ignored"]; ok {
		t.Fatal("unrelated annotation was retained")
	}
}

func TestInvalidAnnotations(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"enabled":  {EnabledAnnotation: "sometimes"},
		"ttl":      {TTLAnnotation: "0"},
		"delay":    {DeletionDelayAnnotation: "-1s"},
		"family":   {FamiliesAnnotation: "ipx"},
		"wildcard": {HostnamesAnnotation: "foo.*.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAnnotations(values); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestInvalidErrorsRemainDetectableThroughContext(t *testing.T) {
	_, err := NormalizeHostname("bad.*.example.com")
	if !IsInvalid(err) {
		t.Fatalf("error = %v, want invalid source error", err)
	}
	if wrapped := fmt.Errorf("normalize hostname: %w", err); !IsInvalid(wrapped) {
		t.Fatalf("wrapped error = %v, want invalid source error", wrapped)
	}
}

func TestMatchingZoneUsesLabelBoundaryAndLongestSuffix(t *testing.T) {
	if got := matchingZone("App.Dev.Example.com.", []string{"example.com", "dev.example.com"}); got != "dev.example.com" {
		t.Fatalf("got %q", got)
	}
	if got := matchingZone("notexample.com", []string{"example.com"}); got != "" {
		t.Fatalf("boundary match returned %q", got)
	}
	if got := matchingZone("*.example.com", []string{"example.com"}); got != "example.com" {
		t.Fatalf("wildcard got %q", got)
	}
}

func TestIngressProjectionKeepsRuleBackendAssociation(t *testing.T) {
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app"}, Spec: networkingv1.IngressSpec{
		DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "default"}},
		Rules: []networkingv1.IngressRule{
			{Host: "one.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "one"}}}}}}},
			{Host: "two.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "two"}}}}}}},
		}, TLS: []networkingv1.IngressTLS{{Hosts: []string{"one.example.com", "tls.example.com"}}},
	}}
	got, err := IngressProjection(ingress, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"one.example.com": "app/one", "two.example.com": "app/two", "tls.example.com": "app/default"}
	for _, host := range got {
		if len(host.Backends) != 1 || host.Backends[0].Key() != want[host.Hostname] {
			t.Fatalf("unexpected projection %#v", got)
		}
		delete(want, host.Hostname)
	}
	if len(want) != 0 {
		t.Fatalf("missing hosts: %#v", want)
	}
	overrides, err := IngressProjection(ingress, []string{"override.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 || len(overrides[0].Backends) != 3 {
		t.Fatalf("override did not use union: %#v", overrides)
	}
}

func TestTLSOnlyWithoutDefaultWarnsAndSkips(t *testing.T) {
	warnings := 0
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app"}, Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{Hosts: []string{"tls.example.com"}}}}}
	got, err := IngressProjection(ingress, nil, func(_, _ string) { warnings++ })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || warnings != 1 {
		t.Fatalf("got=%#v warnings=%d", got, warnings)
	}
}

func TestHTTPRouteReferenceGrant(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	_ = gatewayv1beta1.Install(scheme)
	port80 := gatewayv1.PortNumber(80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "api", Namespace: ptr.To(gatewayv1.Namespace("backend")), Port: &port80,
					}},
				}},
			}},
		},
	}
	grant := &gatewayv1beta1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "backend"}, Spec: gatewayv1beta1.ReferenceGrantSpec{From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "app"}}, To: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr.To(gatewayv1beta1.ObjectName("api"))}}}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).Build()
	got, err := HTTPRouteProjection(context.Background(), reader, route, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Backends) != 1 || got[0].Backends[0].Key() != "backend/api" {
		t.Fatalf("unexpected projection %#v", got)
	}
	grant.Spec.To[0].Name = ptr.To(gatewayv1beta1.ObjectName("other"))
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).Build()
	got, err = HTTPRouteProjection(context.Background(), reader, route, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unauthorized backend accepted: %#v", got)
	}
}

func TestResolverUsesReadyEndpointNodesAndNodeLabels(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	one, two, ignored := "one", "two", "ignored"
	ready, notReady := true, false
	objects := []runtime.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"example.test/v4": "192.0.2.2", "example.test/v6": "v6-2001-db8--2"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "two", Labels: map[string]string{"example.test/v4": "192.0.2.1", "example.test/v6": "v6-2001-db8--1"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ignored", Labels: map[string]string{"example.test/v4": "192.0.2.9"}}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &one}, {NodeName: &two, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}, {NodeName: &ignored, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/v4"}, IPv6: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/v6"}}, RecordDefaults: labdnsv1alpha1.RecordDefaults{TTL: 300, DeletionDelay: &metav1.Duration{Duration: time.Minute}}}}
	pubs, err := (Resolver{Reader: kubeClient}).Publications(context.Background(), []HostProjection{{Hostname: "app.example.com", Backends: []Backend{{Namespace: "app", Name: "api"}}}}, []*labdnsv1alpha1.DNSProvider{provider}, PublicationOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pubs) != 1 || len(pubs[0].Records) != 2 {
		t.Fatalf("unexpected %#v", pubs)
	}
	if got := pubs[0].Records[0].Targets; len(got) != 2 || got[0] != "192.0.2.1" || got[1] != "192.0.2.2" {
		t.Fatalf("A targets %#v", got)
	}
	if got := pubs[0].Records[1].Targets; len(got) != 2 || got[0] != "2001:db8::1" || got[1] != "2001:db8::2" {
		t.Fatalf("AAAA targets %#v", got)
	}
}

func TestParseNodeAddressEncodingAndFamilyValidation(t *testing.T) {
	for name, test := range map[string]struct {
		value  string
		family AddressFamily
		want   string
		valid  bool
	}{
		"ipv4":                    {value: "192.0.2.1", family: IPv4, want: "192.0.2.1", valid: true},
		"ipv6":                    {value: "v6-2001-db8--10", family: IPv6, want: "2001:db8::10", valid: true},
		"ipv6-loopback":           {value: "v6---1", family: IPv6, want: "::1", valid: true},
		"ipv6-missing-prefix":     {value: "2001-db8--10", family: IPv6},
		"ipv6-literal":            {value: "2001:db8::10", family: IPv6},
		"ipv6-malformed":          {value: "v6-2001-db8---10", family: IPv6},
		"ipv6-noncanonical":       {value: "v6-2001-0db8--10", family: IPv6},
		"ipv6-encoded-ipv4":       {value: "v6-192.0.2.1", family: IPv6},
		"ipv4-prefixed":           {value: "v6-192.0.2.1", family: IPv4},
		"ipv4-wrong-family":       {value: "2001:db8::10", family: IPv4},
		"ipv4-mapped-ipv6-source": {value: "v6---ffff-192.0.2.1", family: IPv6},
	} {
		t.Run(name, func(t *testing.T) {
			address, err := parseNodeAddress(test.value, test.family)
			if !test.valid {
				if err == nil {
					t.Fatalf("parseNodeAddress(%q, %s) = %s, want error", test.value, test.family, address)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if address.String() != test.want {
				t.Fatalf("parseNodeAddress(%q, %s) = %s, want %s", test.value, test.family, address, test.want)
			}
		})
	}
}

func TestResolverInvalidNodeLabelFailsWholePublication(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	one := "one"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"example.test/v4": "not-an-ip"}}}, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "app", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &one}}}).Build()
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/v4"}}}}
	if _, err := (Resolver{Reader: kubeClient}).Publications(context.Background(), []HostProjection{{Hostname: "app.example.com", Backends: []Backend{{Namespace: "app", Name: "api"}}}}, []*labdnsv1alpha1.DNSProvider{provider}, PublicationOptions{}, nil); err == nil {
		t.Fatal("expected invalid IP error")
	}
}

func TestProviderSpecificAllowlist(t *testing.T) {
	cloudflareKey := ExternalDNSPrefix + "cloudflare-proxied"
	resolved := map[string]string{cloudflareKey: "true", ExternalDNSPrefix + "other": "yes"}
	www := &labdnsv1alpha1.DNSProvider{Spec: labdnsv1alpha1.DNSProviderSpec{ProviderSpecific: labdnsv1alpha1.ProviderSpecific{Defaults: []labdnsv1alpha1.ProviderProperty{{Name: cloudflareKey, Value: "false"}}, AnnotationKeys: []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(cloudflareKey)}}}}
	vpn := &labdnsv1alpha1.DNSProvider{}
	wwwProperties, wwwMetadata := providerProperties(www, resolved)
	vpnProperties, vpnMetadata := providerProperties(vpn, resolved)
	if len(wwwProperties) != 1 || wwwProperties[0] != (Property{Name: cloudflareKey, Value: "true"}) {
		t.Fatalf("www properties %#v", wwwProperties)
	}
	if len(vpnProperties) != 0 {
		t.Fatalf("vpn received non-allowlisted properties %#v", vpnProperties)
	}
	if len(wwwMetadata) != 2 || len(vpnMetadata) != 2 || wwwMetadata[cloudflareKey] != "true" || vpnMetadata[cloudflareKey] != "true" {
		t.Fatalf("metadata pass-through differs: www=%#v vpn=%#v", wwwMetadata, vpnMetadata)
	}
}

func TestResolverWarnsForHostnameOutsideProviderZones(t *testing.T) {
	provider := &labdnsv1alpha1.DNSProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "www"},
		Spec: labdnsv1alpha1.DNSProviderSpec{
			Zones:     []labdnsv1alpha1.DNSZone{{Name: "example.net"}},
			IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/ip"}},
		},
	}
	warnings := 0
	publications, err := (Resolver{}).Publications(
		context.Background(), []HostProjection{{Hostname: "app.example.com"}},
		[]*labdnsv1alpha1.DNSProvider{provider}, PublicationOptions{}, func(reason, _ string) {
			if reason == "HostnameOutsideZones" {
				warnings++
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if warnings != 1 || len(publications) != 1 || len(publications[0].Records) != 0 {
		t.Fatalf("warnings=%d publications=%#v", warnings, publications)
	}
}

type cancellationReader struct{ client.Reader }

func (r cancellationReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestResolverPropagatesContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/ip"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Resolver{Reader: cancellationReader{Reader: base}}).Publications(ctx, []HostProjection{{Hostname: "app.example.com", Backends: []Backend{{Namespace: "app", Name: "api"}}}}, []*labdnsv1alpha1.DNSProvider{provider}, PublicationOptions{}, nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}
