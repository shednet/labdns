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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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

func TestManagerRestartReconstructsMetricsFromInitialWatches(t *testing.T) {
	config, scheme, direct := sharedIntegration(t)
	t.Cleanup(func() { cleanupSharedClusterObjects(t, []string{"restart-www"}, nil, nil) })
	t.Cleanup(func() { cleanupSharedNamespaces(t, "metrics-restart") })
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "metrics-restart"}}
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "restart-www"}, Spec: labdnsv1alpha1.DNSProviderSpec{
		Zones:     []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
		IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/public-ip"}},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace.Name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name, Annotations: map[string]string{
		source.EnabledAnnotation: "true", source.ProvidersAnnotation: provider.Name,
	}}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{ingressRule("app.example.com", service.Name)}}}
	for _, object := range []client.Object{namespace, provider, service, ingress} {
		if err := direct.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), ingress); err != nil {
		t.Fatal(err)
	}
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: ingress.Namespace, Name: ingress.Name, UID: ingress.UID}
	dnsObject := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{
		Name: dnsendpoint.ObjectName(identity, provider.Name), Namespace: namespace.Name,
		Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: provider.Name, dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity)},
		Annotations: map[string]string{
			dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: namespace.Name,
			dnsendpoint.SourceNameAnnotation: ingress.Name, dnsendpoint.SourceUIDAnnotation: string(ingress.UID),
			dnsendpoint.DeletionDelayAnnotation: "1m0s",
			dnsendpoint.LifecycleAnnotation:     `{"version":1,"pending":[{"dnsName":"app.example.com","recordType":"A","target":"192.0.2.10","deadline":"2099-01-01T00:00:00Z"}]}`,
		},
	}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.10"}, RecordTTL: 300}}}}
	if err := direct.Create(ctx, dnsObject); err != nil {
		t.Fatal(err)
	}

	start := func() (*Metrics, func()) {
		registry := prometheus.NewRegistry()
		metrics := NewMetrics(registry)
		mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", Controller: controllerconfig.Controller{SkipNameValidation: new(true)}})
		if err != nil {
			t.Fatal(err)
		}
		output := metricsLifecycleOutput{}
		if err := Setup(context.Background(), mgr, output, false, metrics); err != nil {
			t.Fatal(err)
		}
		if err := SetupLifecycle(mgr, output, false, metrics); err != nil {
			t.Fatal(err)
		}
		stop := startManagedTestManager(t, mgr, ctx)
		pollContext, pollCancel := context.WithTimeout(ctx, 10*time.Second)
		defer pollCancel()
		if err := wait.PollUntilContextTimeout(pollContext, 20*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
			return testutil.ToFloat64(metrics.source.WithLabelValues("Ingress")) == 1 &&
				testutil.ToFloat64(metrics.generated) == 1 && testutil.ToFloat64(metrics.pending) == 1, nil
		}); err != nil {
			t.Fatalf("initial watches did not reconstruct exact metrics: %v", err)
		}
		return metrics, stop
	}

	first, stop := start()
	assertGauge(t, first.source.WithLabelValues("Ingress"), 1)
	assertGauge(t, first.generated, 1)
	assertGauge(t, first.pending, 1)
	var beforeIngress networkingv1.Ingress
	var beforeEndpoint externaldnsv1alpha1.DNSEndpoint
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), &beforeIngress); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(dnsObject), &beforeEndpoint); err != nil {
		t.Fatal(err)
	}
	stop()

	second, _ := start()
	assertGauge(t, second.source.WithLabelValues("Ingress"), 1)
	assertGauge(t, second.generated, 1)
	assertGauge(t, second.pending, 1)
	var afterIngress networkingv1.Ingress
	var afterEndpoint externaldnsv1alpha1.DNSEndpoint
	if err := direct.Get(ctx, client.ObjectKeyFromObject(ingress), &afterIngress); err != nil {
		t.Fatal(err)
	}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(dnsObject), &afterEndpoint); err != nil {
		t.Fatal(err)
	}
	if beforeIngress.ResourceVersion != afterIngress.ResourceVersion || beforeEndpoint.ResourceVersion != afterEndpoint.ResourceVersion {
		t.Fatal("metrics reconstruction mutated Kubernetes objects")
	}
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

type dnsEndpointIsolationFixture struct {
	namespace  *corev1.Namespace
	vpn        *labdnsv1alpha1.DNSProvider
	node       *corev1.Node
	ingress    *networkingv1.Ingress
	identity   source.Identity
	cloudflare string
}

func startDNSEndpointManager(t *testing.T, config *rest.Config, scheme *runtime.Scheme) func() {
	t.Helper()
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", Controller: controllerconfig.Controller{SkipNameValidation: new(true)}})
	if err != nil {
		t.Fatal(err)
	}
	output := dnsendpoint.NewWriter(mgr.GetClient())
	if err := Setup(context.Background(), mgr, output, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := SetupLifecycle(mgr, output, false, nil); err != nil {
		t.Fatal(err)
	}
	return startManagedTestManager(t, mgr, context.Background())
}

func createDNSEndpointIsolationFixture(t *testing.T, direct client.Client) dnsEndpointIsolationFixture {
	t.Helper()
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
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: namespace.Name, Annotations: map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www,vpn", cloudflare: "true"}}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{ingressRule("same.example.com", "api")}}}
	if err := direct.Create(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	return dnsEndpointIsolationFixture{
		namespace:  namespace,
		vpn:        providers[1],
		node:       node,
		ingress:    ingress,
		identity:   source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: namespace.Name, Name: ingress.Name},
		cloudflare: cloudflare,
	}
}

func waitForDNSEndpoint(t *testing.T, ctx context.Context, direct client.Client, namespace string, identity source.Identity, provider string, predicate func(*externaldnsv1alpha1.DNSEndpoint) bool) *externaldnsv1alpha1.DNSEndpoint {
	t.Helper()
	key := client.ObjectKey{Namespace: namespace, Name: dnsendpoint.ObjectName(identity, provider)}
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

func waitForDNSEndpointDeletion(t *testing.T, ctx context.Context, direct client.Client, namespace string, identity source.Identity, provider string) {
	t.Helper()
	key := client.ObjectKey{Namespace: namespace, Name: dnsendpoint.ObjectName(identity, provider)}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var object externaldnsv1alpha1.DNSEndpoint
		if err := direct.Get(ctx, key, &object); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var object externaldnsv1alpha1.DNSEndpoint
	err := direct.Get(ctx, key, &object)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("provider %s remained after recovered deadline: %v", provider, err)
	}
}

func assertDNSEndpointProviderIsolation(t *testing.T, www, vpn *externaldnsv1alpha1.DNSEndpoint, cloudflare string) {
	t.Helper()
	if www.Annotations[cloudflare] != "true" || len(www.Spec.Endpoints[0].ProviderSpecific) != 1 || www.Spec.Endpoints[0].ProviderSpecific[0].Value != "true" {
		t.Fatal("www provider pass-through missing")
	}
	if vpn.Annotations[cloudflare] != "true" || len(vpn.Spec.Endpoints[0].ProviderSpecific) != 0 {
		t.Fatal("resolved metadata must pass through while provider behavior remains isolated")
	}
}

func exerciseProviderDeletionAndRecreation(t *testing.T, ctx context.Context, direct client.Client, fixture dnsEndpointIsolationFixture) {
	t.Helper()
	if err := direct.Delete(ctx, fixture.vpn); err != nil {
		t.Fatal(err)
	}
	waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"10.0.0.1"`)
	})
	recreatedVPN := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "vpn"}, Spec: fixture.vpn.Spec}
	if err := direct.Create(ctx, recreatedVPN); err != nil {
		t.Fatal(err)
	}
	waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return endpointTargetsEqual(object, "10.0.0.1") && !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
	})
}

func exerciseSourceDisablementAndReenable(t *testing.T, ctx context.Context, direct client.Client, fixture dnsEndpointIsolationFixture) {
	t.Helper()
	if err := direct.Get(ctx, client.ObjectKeyFromObject(fixture.ingress), fixture.ingress); err != nil {
		t.Fatal(err)
	}
	fixture.ingress.Annotations[source.EnabledAnnotation] = "false"
	if err := direct.Update(ctx, fixture.ingress); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
	if err := direct.Get(ctx, client.ObjectKeyFromObject(fixture.ingress), fixture.ingress); err != nil {
		t.Fatal(err)
	}
	fixture.ingress.Annotations[source.EnabledAnnotation] = "true"
	if err := direct.Update(ctx, fixture.ingress); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
}

func exerciseNodeTargetRotation(t *testing.T, ctx context.Context, direct client.Client, fixture dnsEndpointIsolationFixture) {
	t.Helper()
	if err := direct.Get(ctx, client.ObjectKey{Name: fixture.node.Name}, fixture.node); err != nil {
		t.Fatal(err)
	}
	fixture.node.Labels["network.example/public"] = "192.0.2.2"
	if err := direct.Update(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "www", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		return endpointTargetsEqual(object, "192.0.2.1", "192.0.2.2") && strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], "192.0.2.1")
	})
	if err := direct.Get(ctx, client.ObjectKey{Name: fixture.node.Name}, fixture.node); err != nil {
		t.Fatal(err)
	}
	fixture.node.Labels["network.example/public"] = "192.0.2.1"
	if err := direct.Update(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "www", func(object *externaldnsv1alpha1.DNSEndpoint) bool {
		lifecycle := object.Annotations[dnsendpoint.LifecycleAnnotation]
		return endpointTargetsEqual(object, "192.0.2.1", "192.0.2.2") && !strings.Contains(lifecycle, `"target":"192.0.2.1"`) && strings.Contains(lifecycle, `"target":"192.0.2.2"`)
	})
}

func exerciseSourceDeletionAndRestartRecovery(t *testing.T, ctx context.Context, direct client.Client, fixture dnsEndpointIsolationFixture, stop func(), start func() func()) {
	t.Helper()
	if err := direct.Delete(ctx, fixture.ingress); err != nil {
		t.Fatal(err)
	}
	var deleted networkingv1.Ingress
	if err := direct.Get(ctx, client.ObjectKeyFromObject(fixture.ingress), &deleted); !apierrors.IsNotFound(err) {
		t.Fatalf("source deletion blocked: %v", err)
	}
	for _, provider := range []string{"www", "vpn"} {
		waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, provider, func(object *externaldnsv1alpha1.DNSEndpoint) bool {
			return strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`)
		})
	}
	stop()
	_ = start()
	for _, provider := range []string{"www", "vpn"} {
		waitForDNSEndpointDeletion(t, ctx, direct, fixture.namespace.Name, fixture.identity, provider)
	}
}

func TestManagerDNSEndpointIsolationAndRestartRecovery(t *testing.T) {
	config, scheme, direct := sharedIntegration(t)
	t.Cleanup(func() { cleanupSharedNamespaces(t, "dnsendpoint") })
	t.Cleanup(func() { cleanupSharedClusterObjects(t, []string{"www", "vpn"}, nil, nil) })
	t.Cleanup(func() { cleanupSharedNodes(t, "worker") })
	ctx := context.Background()
	startManager := func() func() { return startDNSEndpointManager(t, config, scheme) }
	stop := startManager()
	fixture := createDNSEndpointIsolationFixture(t, direct)
	www := waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "www", func(object *externaldnsv1alpha1.DNSEndpoint) bool { return endpointTargetsEqual(object, "192.0.2.1") })
	vpn := waitForDNSEndpoint(t, ctx, direct, fixture.namespace.Name, fixture.identity, "vpn", func(object *externaldnsv1alpha1.DNSEndpoint) bool { return endpointTargetsEqual(object, "10.0.0.1") })
	assertDNSEndpointProviderIsolation(t, www, vpn, fixture.cloudflare)
	exerciseProviderDeletionAndRecreation(t, ctx, direct, fixture)
	exerciseSourceDisablementAndReenable(t, ctx, direct, fixture)
	exerciseNodeTargetRotation(t, ctx, direct, fixture)
	exerciseSourceDeletionAndRestartRecovery(t, ctx, direct, fixture, stop, startManager)
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

type writerContractFixture struct {
	ctx       context.Context
	direct    client.Client
	clock     lifecycleClock
	writer    *dnsendpoint.Writer
	namespace string
	identity  source.Identity
	shared    source.Publication
}

func createWriterContractFixture(t *testing.T, direct client.Client) writerContractFixture {
	t.Helper()
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "writer-contract"}}
	if err := direct.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	clock := lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: namespace.Name, Name: "shared", UID: types.UID("uid-one")}
	properties := []source.Property{{Name: "proxy", Value: "false"}}
	shared := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{
		{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.2"}, TTL: 300, ProviderSpecific: properties},
		{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, TTL: 300, ProviderSpecific: properties},
	}}
	return writerContractFixture{
		ctx:       ctx,
		direct:    direct,
		clock:     clock,
		writer:    &dnsendpoint.Writer{Client: direct, Clock: clock},
		namespace: namespace.Name,
		identity:  identity,
		shared:    shared,
	}
}

func changedWriterPublication(fixture writerContractFixture) (source.Identity, source.Publication) {
	recreated := fixture.identity
	recreated.UID = types.UID("uid-two")
	changed := fixture.shared
	changed.Records = []source.Record{{DNSName: "same.example.com", RecordType: "A", Targets: []string{"192.0.2.1", "192.0.2.2"}, TTL: 600, ProviderSpecific: []source.Property{{Name: "proxy", Value: "true"}}}}
	return recreated, changed
}

func assertWriterSharedRRSetAndIdempotence(t *testing.T, fixture writerContractFixture) {
	t.Helper()
	if err := fixture.writer.Apply(fixture.ctx, fixture.identity, []source.Publication{fixture.shared}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: fixture.namespace, Name: dnsendpoint.ObjectName(fixture.identity, "www")}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := fixture.direct.Get(fixture.ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if !endpointTargetsEqual(&object, "192.0.2.1", "192.0.2.2") {
		t.Fatalf("shared hostname RRset=%#v", object.Spec.Endpoints)
	}
	stableVersion := object.ResourceVersion
	if err := fixture.writer.Apply(fixture.ctx, fixture.identity, []source.Publication{fixture.shared}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.direct.Get(fixture.ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if object.ResourceVersion != stableVersion {
		t.Fatalf("identical reconciliation wrote DNSEndpoint: %s -> %s", stableVersion, object.ResourceVersion)
	}
}

func assertWriterIdentityAndRecordUpdate(t *testing.T, fixture writerContractFixture, recreated source.Identity, changed source.Publication) {
	t.Helper()
	if err := fixture.writer.Apply(fixture.ctx, recreated, []source.Publication{changed}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: fixture.namespace, Name: dnsendpoint.ObjectName(recreated, "www")}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := fixture.direct.Get(fixture.ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if object.Annotations[dnsendpoint.SourceUIDAnnotation] != "uid-two" || object.Spec.Endpoints[0].RecordTTL != 600 || len(object.Spec.Endpoints[0].ProviderSpecific) != 1 || object.Spec.Endpoints[0].ProviderSpecific[0].Value != "true" {
		t.Fatalf("UID/TTL/property update was not immediate: %#v", object)
	}
}

func assertWriterRejectsInvalidLifecycle(t *testing.T, fixture writerContractFixture, recreated source.Identity, changed source.Publication) {
	t.Helper()
	for provider, invalid := range map[string]string{"malformed": "{", "unknown": `{"version":2,"pending":[]}`} {
		publication := source.Publication{ProviderName: provider, DeletionDelay: time.Minute, Records: []source.Record{{DNSName: provider + ".example.com", RecordType: "A", Targets: []string{"192.0.2.9"}, TTL: 300}}}
		publications := []source.Publication{changed, publication}
		if err := fixture.writer.Apply(fixture.ctx, recreated, publications); err != nil {
			t.Fatal(err)
		}
		invalidKey := client.ObjectKey{Namespace: fixture.namespace, Name: dnsendpoint.ObjectName(recreated, provider)}
		var invalidObject externaldnsv1alpha1.DNSEndpoint
		if err := fixture.direct.Get(fixture.ctx, invalidKey, &invalidObject); err != nil {
			t.Fatal(err)
		}
		invalidObject.Annotations[dnsendpoint.LifecycleAnnotation] = invalid
		if err := fixture.direct.Update(fixture.ctx, &invalidObject); err != nil {
			t.Fatal(err)
		}
		version := invalidObject.ResourceVersion
		if err := fixture.writer.Apply(fixture.ctx, recreated, publications); err == nil {
			t.Fatalf("%s lifecycle accepted", provider)
		}
		if err := fixture.direct.Get(fixture.ctx, invalidKey, &invalidObject); err != nil {
			t.Fatal(err)
		}
		if invalidObject.ResourceVersion != version || invalidObject.Annotations[dnsendpoint.LifecycleAnnotation] != invalid {
			t.Fatalf("%s lifecycle did not fail closed", provider)
		}
		if err := fixture.direct.Delete(fixture.ctx, &invalidObject); err != nil {
			t.Fatal(err)
		}
	}
}

func assertWriterRetriesUpdateConflict(t *testing.T, fixture writerContractFixture, recreated source.Identity, changed source.Publication) source.Publication {
	t.Helper()
	updateBacking := &apiMutationClient{Client: fixture.direct, updateConflict: true}
	updateWriter := &dnsendpoint.Writer{Client: updateBacking, Clock: fixture.clock}
	conflictPublication := source.Publication{ProviderName: "conflict", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "conflict.example.com", RecordType: "A", Targets: []string{"192.0.2.10"}, TTL: 300}}}
	if err := fixture.writer.Apply(fixture.ctx, recreated, []source.Publication{changed, conflictPublication}); err != nil {
		t.Fatal(err)
	}
	conflictPublication.Records[0].TTL = 900
	if err := updateWriter.Apply(fixture.ctx, recreated, []source.Publication{changed, conflictPublication}); err != nil {
		t.Fatal(err)
	}
	conflictKey := client.ObjectKey{Namespace: fixture.namespace, Name: dnsendpoint.ObjectName(recreated, "conflict")}
	var conflictObject externaldnsv1alpha1.DNSEndpoint
	if err := fixture.direct.Get(fixture.ctx, conflictKey, &conflictObject); err != nil {
		t.Fatal(err)
	}
	if updateBacking.updates != 1 || conflictObject.Spec.Endpoints[0].RecordTTL != 900 || conflictObject.Annotations["example.test/concurrent"] != "preserved" {
		t.Fatalf("update conflict did not re-fetch/preserve: %#v", conflictObject)
	}
	return conflictPublication
}

func assertWriterRejectsDeleteConflict(t *testing.T, fixture writerContractFixture, recreated source.Identity, changed source.Publication, conflictPublication source.Publication) {
	t.Helper()
	deletePublication := source.Publication{ProviderName: "delete-conflict", DeletionDelay: 0, Records: []source.Record{{DNSName: "delete.example.com", RecordType: "A", Targets: []string{"192.0.2.11"}, TTL: 300}}}
	if err := fixture.writer.Apply(fixture.ctx, recreated, []source.Publication{changed, conflictPublication, deletePublication}); err != nil {
		t.Fatal(err)
	}
	deleteBacking := &apiMutationClient{Client: fixture.direct, deleteConflict: true}
	deleteWriter := &dnsendpoint.Writer{Client: deleteBacking, Clock: fixture.clock}
	if err := deleteWriter.Apply(fixture.ctx, recreated, []source.Publication{changed, conflictPublication}); err == nil {
		t.Fatal("delete conflict concurrent lifecycle mutation was not rejected")
	}
	deleteKey := client.ObjectKey{Namespace: fixture.namespace, Name: dnsendpoint.ObjectName(recreated, "delete-conflict")}
	var deleteObject externaldnsv1alpha1.DNSEndpoint
	if err := fixture.direct.Get(fixture.ctx, deleteKey, &deleteObject); err != nil {
		t.Fatal(err)
	}
	if deleteBacking.deletes != 1 || deleteObject.Annotations[dnsendpoint.LifecycleAnnotation] != `{"version":2,"pending":[]}` {
		t.Fatal("delete conflict did not preserve/reject concurrent lifecycle mutation")
	}
}

func TestWriterAPIServerContract(t *testing.T) {
	_, _, direct := sharedIntegration(t)
	t.Cleanup(func() { cleanupSharedNamespaces(t, "writer-contract") })
	fixture := createWriterContractFixture(t, direct)
	assertWriterSharedRRSetAndIdempotence(t, fixture)
	recreated, changed := changedWriterPublication(fixture)
	assertWriterIdentityAndRecordUpdate(t, fixture, recreated, changed)
	assertWriterRejectsInvalidLifecycle(t, fixture, recreated, changed)
	conflictPublication := assertWriterRetriesUpdateConflict(t, fixture, recreated, changed)
	assertWriterRejectsDeleteConflict(t, fixture, recreated, changed, conflictPublication)
}

func TestGatewayDisabledRestartRetiresHTTPRouteWithoutGatewayCRDs(t *testing.T) {
	config, scheme, direct := startNoGatewayEnvironment(t)
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
	if err := SetupLifecycle(mgr, output, false, nil); err != nil {
		t.Fatal(err)
	}
	_ = startManagedTestManager(t, mgr, ctx)
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
