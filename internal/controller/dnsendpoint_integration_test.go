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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/dnsendpoint"
	"github.com/shednet/labdns/internal/source"
)

type apiMutationClient struct {
	client.Client
	updateConflict bool
	deleteConflict bool
	updates        int
	deletes        int
}

func (c *apiMutationClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if c.updateConflict && c.updates == 0 {
		c.updates++
		backing := c.Client
		var concurrent externaldnsv1alpha1.DNSEndpoint
		if err := backing.Get(ctx, client.ObjectKeyFromObject(object), &concurrent); err != nil {
			return err
		}
		if concurrent.Annotations == nil {
			concurrent.Annotations = map[string]string{}
		}
		concurrent.Annotations["example.test/concurrent"] = "preserved"
		if err := backing.Update(ctx, &concurrent); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Group: externaldnsv1alpha1.GroupVersion.Group, Resource: "dnsendpoints"}, object.GetName(), errors.New("injected API-server update conflict"))
	}
	return c.Client.Update(ctx, object, options...)
}

func (c *apiMutationClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if c.deleteConflict && c.deletes == 0 {
		c.deletes++
		backing := c.Client
		var concurrent externaldnsv1alpha1.DNSEndpoint
		if err := backing.Get(ctx, client.ObjectKeyFromObject(object), &concurrent); err != nil {
			return err
		}
		concurrent.Annotations[dnsendpoint.LifecycleAnnotation] = `{"version":2,"pending":[]}`
		if err := backing.Update(ctx, &concurrent); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Group: externaldnsv1alpha1.GroupVersion.Group, Resource: "dnsendpoints"}, object.GetName(), errors.New("injected API-server delete conflict"))
	}
	return c.Client.Delete(ctx, object, options...)
}

func TestManagerDNSEndpointIsolationAndRestartRecovery(t *testing.T) { //nolint:gocyclo
	const trueValue = "true"
	moduleCache := os.Getenv("GOMODCACHE")
	if moduleCache == "" {
		t.Skip("GOMODCACHE is required for pinned fixtures")
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
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	startManager := func() (context.CancelFunc, <-chan error) {
		mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", Controller: controllerconfig.Controller{SkipNameValidation: new(true)}})
		if err != nil {
			t.Fatal(err)
		}
		output := dnsendpoint.NewWriter(mgr.GetClient())
		if err := Setup(context.Background(), mgr, output, false); err != nil {
			t.Fatal(err)
		}
		if err := SetupLifecycle(mgr, output, false); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			t.Fatal("cache did not synchronize")
		}
		return cancel, done
	}
	cancel, done := startManager()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("manager stopped: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("manager did not stop")
		}
	}
	defer func() {
		if cancel != nil {
			stop()
		}
	}()
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dnsendpoint"}}
	if err := direct.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	cloudflare := source.ExternalDNSPrefix + "cloudflare-proxied"
	providers := []*labdnsv1alpha1.DNSProvider{
		{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/public"}}, RecordDefaults: labdnsv1alpha1.RecordDefaults{TTL: 300, DeletionDelay: &metav1.Duration{Duration: 3 * time.Second}}, ProviderSpecific: labdnsv1alpha1.ProviderSpecific{Defaults: []labdnsv1alpha1.ProviderProperty{{Name: cloudflare, Value: "false"}}, AnnotationKeys: []labdnsv1alpha1.AnnotationKey{labdnsv1alpha1.AnnotationKey(cloudflare)}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "vpn"}, Spec: labdnsv1alpha1.DNSProviderSpec{Zones: []labdnsv1alpha1.DNSZone{{Name: "example.com"}}, IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "network.example/vpn"}}, RecordDefaults: labdnsv1alpha1.RecordDefaults{TTL: 300, DeletionDelay: &metav1.Duration{Duration: 3 * time.Second}}}},
	}
	for _, provider := range providers {
		if err := direct.Create(ctx, provider); err != nil {
			t.Fatal(err)
		}
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace.Name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	if err := direct.Create(ctx, service); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"network.example/public": "192.0.2.1", "network.example/vpn": "10.0.0.1"}}}
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	nodeName := "worker"
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace.Name, Labels: map[string]string{discoveryv1.LabelServiceName: "api"}}, AddressType: discoveryv1.AddressTypeIPv4, Endpoints: []discoveryv1.Endpoint{{NodeName: &nodeName, Addresses: []string{"172.18.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: new(true)}}}}
	if err := direct.Create(ctx, slice); err != nil {
		t.Fatal(err)
	}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: namespace.Name, Annotations: map[string]string{source.EnabledAnnotation: trueValue, source.ProvidersAnnotation: "www,vpn", cloudflare: trueValue}}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{ingressRule("same.example.com", "api")}}}
	if err := direct.Create(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: namespace.Name, Name: ingress.Name}
	waitObject := func(provider string, predicate func(*externaldnsv1alpha1.DNSEndpoint) bool) *externaldnsv1alpha1.DNSEndpoint {
		t.Helper()
		key := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(identity, provider)}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var object externaldnsv1alpha1.DNSEndpoint
			if err := direct.Get(ctx, key, &object); err == nil && predicate(&object) {
				return &object
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for provider %s", provider)
		return nil
	}
	www := waitObject("www", func(object *externaldnsv1alpha1.DNSEndpoint) bool { return endpointTargetsEqual(object, "192.0.2.1") })
	vpn := waitObject("vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool { return endpointTargetsEqual(object, "10.0.0.1") })
	if www.Annotations[cloudflare] != trueValue || len(www.Spec.Endpoints[0].ProviderSpecific) != 1 || www.Spec.Endpoints[0].ProviderSpecific[0].Value != trueValue {
		t.Fatal("www provider pass-through missing")
	}
	if vpn.Annotations[cloudflare] != trueValue || len(vpn.Spec.Endpoints[0].ProviderSpecific) != 0 {
		t.Fatal("resolved metadata must pass through while provider behavior remains isolated")
	}
	// Deleting a selected profile is authoritative removal, not a transient empty read.
	if err := direct.Delete(ctx, providers[1]); err != nil {
		t.Fatal(err)
	}
	waitObject("vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"10.0.0.1"`)
	})
	recreatedVPN := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "vpn"}, Spec: providers[1].Spec}
	if err := direct.Create(ctx, recreatedVPN); err != nil {
		t.Fatal(err)
	}
	waitObject("vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return endpointTargetsEqual(object, "10.0.0.1") && !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
	})
	// Explicit source disablement schedules every selected provider and re-enable cancels it.
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Annotations[source.EnabledAnnotation] = "false"
	if err := direct.Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitObject(provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Annotations[source.EnabledAnnotation] = trueValue
	if err := direct.Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitObject(provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
	// A Node-only label update must trigger rotation; the old target is retained pending.
	if err := direct.Get(ctx, client.ObjectKey{Name: node.Name}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/public"] = "192.0.2.2"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	waitObject("www", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return endpointTargetsEqual(object, "192.0.2.1", "192.0.2.2") && strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], "192.0.2.1")
	})
	// Reappearance cancels the durable pending target before its deadline.
	if err := direct.Get(ctx, client.ObjectKey{Name: node.Name}, node); err != nil {
		t.Fatal(err)
	}
	node.Labels["network.example/public"] = "192.0.2.1"
	if err := direct.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	waitObject("www", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		lifecycle := object.Annotations[dnsendpoint.LifecycleAnnotation]
		return endpointTargetsEqual(object, "192.0.2.1", "192.0.2.2") && !strings.Contains(lifecycle, `"target":"192.0.2.1"`) && strings.Contains(lifecycle, `"target":"192.0.2.2"`)
	})
	// Delete the source: Kubernetes deletion completes immediately while objects enter grace.
	if err := direct.Delete(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	var deleted networkingv1.Ingress
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), &deleted); !apierrors.IsNotFound(err) {
		t.Fatalf("source deletion blocked: %v", err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitObject(provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
	// Stop before grace expiry. Initial DNSEndpoint watch events in a fresh manager recover timers.
	stop()
	cancel = nil
	cancel, done = startManager()
	for _, provider := range []string{"www", "vpn"} {
		key := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(identity, provider)}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var object externaldnsv1alpha1.DNSEndpoint
			if err := direct.Get(ctx, key, &object); apierrors.IsNotFound(err) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		var object externaldnsv1alpha1.DNSEndpoint
		if err := direct.Get(ctx, key, &object); !apierrors.IsNotFound(err) {
			t.Fatalf("provider %s remained after recovered deadline: %v", provider, err)
		}
	}
}

func endpointTargetsEqual(object *externaldnsv1alpha1.DNSEndpoint, expected ...string) bool {
	if len(object.Spec.Endpoints) != 1 {
		return false
	}
	actual := append([]string(nil), object.Spec.Endpoints[0].Targets...)
	slices.Sort(actual)
	slices.Sort(expected)
	return slices.Equal(actual, expected)
}

func TestWriterAPIServerContract(t *testing.T) { //nolint:gocyclo
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "test", "fixtures", "external-dns-v0.21.0")}, ErrorIfCRDPathMissing: true}
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
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, externaldnsv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "writer-contract"}}
	if err := direct.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	clock := lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
	writer := &dnsendpoint.Writer{Client: direct, Clock: clock}
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: namespace.Name, Name: "shared", UID: types.UID("uid-one")}
	properties := []source.Property{{Name: "proxy", Value: "false"}}
	shared := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{
		{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.2"}, TTL: 300, ProviderSpecific: properties},
		{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, TTL: 300, ProviderSpecific: properties},
	}}
	if err := writer.Apply(ctx, identity, []source.Publication{shared}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(identity, "www")}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := direct.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if !endpointTargetsEqual(&object, "192.0.2.1", "192.0.2.2") {
		t.Fatalf("shared hostname RRset=%#v", object.Spec.Endpoints)
	}
	stableVersion := object.ResourceVersion
	if err := writer.Apply(ctx, identity, []source.Publication{shared}); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if object.ResourceVersion != stableVersion {
		t.Fatalf("identical reconciliation wrote DNSEndpoint: %s -> %s", stableVersion, object.ResourceVersion)
	}
	recreated := identity
	recreated.UID = types.UID("uid-two")
	changed := shared
	changed.Records = []source.Record{{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, TTL: 600, ProviderSpecific: []source.Property{{Name: "proxy", Value: "true"}}}}
	if err := writer.Apply(ctx, recreated, []source.Publication{changed}); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if object.Annotations[dnsendpoint.SourceUIDAnnotation] != "uid-two" || object.Spec.Endpoints[0].RecordTTL != 600 || len(object.Spec.Endpoints[0].ProviderSpecific) != 1 || object.Spec.Endpoints[0].ProviderSpecific[0].Value != "true" {
		t.Fatalf("UID/TTL/property update was not immediate: %#v", object)
	}
	for provider, invalid := range map[string]string{"malformed": "{", "unknown": `{"version":2,"pending":[]}`} {
		publication := source.Publication{ProviderName: provider, DeletionDelay: time.Minute, Records: []source.Record{{DNSName: provider + ".example.com", RecordType: "A", Targets: []string{"192.0.2.9"}, TTL: 300}}}
		if err := writer.Apply(ctx, recreated, append([]source.Publication{changed}, publication)); err != nil {
			t.Fatal(err)
		}
		invalidKey := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(recreated, provider)}
		var invalidObject externaldnsv1alpha1.DNSEndpoint
		if err := direct.Get(ctx, invalidKey, &invalidObject); err != nil {
			t.Fatal(err)
		}
		invalidObject.Annotations[dnsendpoint.LifecycleAnnotation] = invalid
		if err := direct.Update(ctx, &invalidObject); err != nil {
			t.Fatal(err)
		}
		version := invalidObject.ResourceVersion
		if err := writer.Apply(ctx, recreated, append([]source.Publication{changed}, publication)); err == nil {
			t.Fatalf("%s lifecycle accepted", provider)
		}
		if err := direct.Get(ctx, invalidKey, &invalidObject); err != nil {
			t.Fatal(err)
		}
		if invalidObject.ResourceVersion != version || invalidObject.Annotations[dnsendpoint.LifecycleAnnotation] != invalid {
			t.Fatalf("%s lifecycle did not fail closed", provider)
		}
		if err := direct.Delete(ctx, &invalidObject); err != nil {
			t.Fatal(err)
		}
	}
	updateBacking := &apiMutationClient{Client: direct, updateConflict: true}
	updateWriter := &dnsendpoint.Writer{Client: updateBacking, Clock: clock}
	conflictPublication := source.Publication{ProviderName: "conflict", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "conflict.example.com", RecordType: "A", Targets: []string{"192.0.2.10"}, TTL: 300}}}
	if err := writer.Apply(ctx, recreated, []source.Publication{changed, conflictPublication}); err != nil {
		t.Fatal(err)
	}
	conflictPublication.Records[0].TTL = 900
	if err := updateWriter.Apply(ctx, recreated, []source.Publication{changed, conflictPublication}); err != nil {
		t.Fatal(err)
	}
	conflictKey := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(recreated, "conflict")}
	var conflictObject externaldnsv1alpha1.DNSEndpoint
	if err := direct.Get(ctx, conflictKey, &conflictObject); err != nil {
		t.Fatal(err)
	}
	if updateBacking.updates != 1 || conflictObject.Spec.Endpoints[0].RecordTTL != 900 || conflictObject.Annotations["example.test/concurrent"] != "preserved" {
		t.Fatalf("update conflict did not re-fetch/preserve: %#v", conflictObject)
	}
	deletePublication := source.Publication{ProviderName: "delete-conflict", DeletionDelay: 0, Records: []source.Record{{DNSName: "delete.example.com", RecordType: "A", Targets: []string{"192.0.2.11"}, TTL: 300}}}
	if err := writer.Apply(ctx, recreated, []source.Publication{changed, conflictPublication, deletePublication}); err != nil {
		t.Fatal(err)
	}
	deleteBacking := &apiMutationClient{Client: direct, deleteConflict: true}
	deleteWriter := &dnsendpoint.Writer{Client: deleteBacking, Clock: clock}
	if err := deleteWriter.Apply(ctx, recreated, []source.Publication{changed, conflictPublication}); err == nil {
		t.Fatal("delete conflict concurrent lifecycle mutation was not rejected")
	}
	deleteKey := client.ObjectKey{Namespace: namespace.Name, Name: dnsendpoint.ObjectName(recreated, "delete-conflict")}
	var deleteObject externaldnsv1alpha1.DNSEndpoint
	if err := direct.Get(ctx, deleteKey, &deleteObject); err != nil {
		t.Fatal(err)
	}
	if deleteBacking.deletes != 1 || deleteObject.Annotations[dnsendpoint.LifecycleAnnotation] != `{"version":2,"pending":[]}` {
		t.Fatal("delete conflict did not preserve/reject concurrent lifecycle mutation")
	}
}

func TestGatewayDisabledRestartRetiresHTTPRouteWithoutGatewayCRDs(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "test", "fixtures", "external-dns-v0.21.0"),
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
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, externaldnsv1alpha1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gateway-disabled"}}
	if err := direct.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	identity := source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: namespace.Name, Name: "survivor", UID: types.UID("old-route-uid")}
	object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{
		Name:        dnsendpoint.ObjectName(identity, "www"),
		Namespace:   namespace.Name,
		Labels:      map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www", dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity)},
		Annotations: map[string]string{dnsendpoint.SourceKindAnnotation: "HTTPRoute", dnsendpoint.SourceNamespaceAnnotation: namespace.Name, dnsendpoint.SourceNameAnnotation: identity.Name, dnsendpoint.SourceUIDAnnotation: string(identity.UID), dnsendpoint.DeletionDelayAnnotation: "1s", dnsendpoint.LifecycleAnnotation: `{"version":1,"pending":[]}`},
	}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "route.example.com", RecordType: "A", Targets: []string{"192.0.2.50"}, RecordTTL: 300}}}}
	if err := direct.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", Controller: controllerconfig.Controller{SkipNameValidation: new(true)}})
	if err != nil {
		t.Fatal(err)
	}
	output := dnsendpoint.NewWriter(mgr.GetClient())
	if err := SetupLifecycle(mgr, output, false); err != nil {
		t.Fatal(err)
	}
	managerContext, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(managerContext) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager stopped: %v", err)
		}
	}()
	key := client.ObjectKeyFromObject(object)
	deadline := time.Now().Add(10 * time.Second)
	observedPending := false
	for time.Now().Before(deadline) {
		var current externaldnsv1alpha1.DNSEndpoint
		err := direct.Get(ctx, key, &current)
		if apierrors.IsNotFound(err) {
			if !observedPending {
				t.Fatal("HTTPRoute DNSEndpoint deleted without observable retirement")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(current.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"192.0.2.50"`) {
			observedPending = true
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("HTTPRoute DNSEndpoint did not expire with Gateway disabled")
}
