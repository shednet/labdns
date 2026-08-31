/*
Copyright 2026 Konstantinos Kalyvas.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/dnsendpoint"
	"github.com/shednet/labdns/internal/source"
)

func TestMetricsTrackEventStateWithoutUnboundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.SetSource("Ingress", "one/a", true)
	metrics.SetSource("Ingress", "one/a", true)
	metrics.SetSource("Ingress", "one/b", true)
	metrics.SetSource("Ingress", "one/a", false)
	metrics.SetEndpoint("one/a", true, 2)
	metrics.SetEndpoint("one/b", true, 1)
	metrics.SetEndpoint("one/b", true, 1)
	metrics.SetEndpoint("one/a", false, 0)
	metrics.Observe("ingress", time.Now(), errors.New("failed"))

	if got := testutil.ToFloat64(metrics.source.WithLabelValues("Ingress")); got != 1 {
		t.Fatalf("managed sources = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.generated); got != 1 {
		t.Fatalf("generated endpoints = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.pending); got != 1 {
		t.Fatalf("pending targets = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.errors.WithLabelValues("ingress")); got != 1 {
		t.Fatalf("reconcile errors = %v, want 1", got)
	}
	if count, err := testutil.GatherAndCount(registry,
		"labdns_reconcile_duration_seconds",
		"labdns_reconcile_errors_total",
		"labdns_managed_sources",
		"labdns_generated_dnsendpoints",
		"labdns_pending_target_deletions",
	); err != nil || count != 5 {
		t.Fatalf("registered metric families = %d, %v", count, err)
	}
}

func TestSourceReconcilerUpdatesMetricsAcrossWatchStates(t *testing.T) {
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	scheme := testScheme(t)
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{
		Zones:     []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
		IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.test/ip"}},
	}}
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app", Annotations: map[string]string{
		source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www",
	}}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, ingress).Build()
	reconciler := &IngressReconciler{Client: kubeClient, Output: &recordingOutput{}, Resolver: source.Resolver{Reader: kubeClient}, Metrics: metrics}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ingress)}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.source.WithLabelValues("Ingress"), 1)

	if err := kubeClient.Get(ctx, request.NamespacedName, ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Annotations = map[string]string{source.EnabledAnnotation: "false"}
	if err := kubeClient.Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.source.WithLabelValues("Ingress"), 0)

	if err := kubeClient.Get(ctx, request.NamespacedName, ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Annotations = map[string]string{source.EnabledAnnotation: "invalid"}
	if err := kubeClient.Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil {
		t.Fatal("invalid annotation did not fail reconciliation")
	}
	assertGauge(t, metrics.source.WithLabelValues("Ingress"), 0)
	assertGauge(t, metrics.errors.WithLabelValues("ingress"), 1)

	if err := kubeClient.Get(ctx, request.NamespacedName, ingress); err != nil {
		t.Fatal(err)
	}
	ingress.Annotations = map[string]string{source.EnabledAnnotation: "true", source.ProvidersAnnotation: "www"}
	if err := kubeClient.Update(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.source.WithLabelValues("Ingress"), 1)
	if err := kubeClient.Delete(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.source.WithLabelValues("Ingress"), 0)
	assertBoundedMetricLabels(t, registry)
}

type metricsLifecycleOutput struct{}

func (metricsLifecycleOutput) Apply(context.Context, source.Identity, []source.Publication) error {
	return nil
}

func (metricsLifecycleOutput) Advance(context.Context, types.NamespacedName) (time.Duration, error) {
	return 0, nil
}

func TestLifecycleReconcilerUpdatesEndpointMetrics(t *testing.T) {
	ctx := context.Background()
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: "uid"}
	sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: identity.Name, Namespace: identity.Namespace, UID: identity.UID}}
	object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{
		Name: dnsendpoint.ObjectName(identity, "www"), Namespace: identity.Namespace,
		Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www", dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity)},
		Annotations: map[string]string{
			dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: identity.Namespace,
			dnsendpoint.SourceNameAnnotation: identity.Name, dnsendpoint.SourceUIDAnnotation: string(identity.UID),
			dnsendpoint.DeletionDelayAnnotation: "1m0s",
			dnsendpoint.LifecycleAnnotation:     `{"version":1,"pending":[{"dnsName":"app.example.com","recordType":"A","target":"192.0.2.1","deadline":"2026-08-30T20:01:00Z"}]}`,
		},
	}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject, object).Build()
	reconciler := &LifecycleReconciler{Client: kubeClient, Output: metricsLifecycleOutput{}, Metrics: metrics}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.generated, 1)
	assertGauge(t, metrics.pending, 1)
	if err := kubeClient.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertGauge(t, metrics.generated, 0)
	assertGauge(t, metrics.pending, 0)
	assertBoundedMetricLabels(t, registry)
}

func assertGauge(t *testing.T, collector prometheus.Collector, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(collector); got != want {
		t.Fatalf("gauge = %v, want %v", got, want)
	}
}

func assertBoundedMetricLabels(t *testing.T, registry *prometheus.Registry) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "controller":
					if label.GetValue() != "ingress" && label.GetValue() != "httproute" && label.GetValue() != "lifecycle" {
						t.Fatalf("metric %s exposes unbounded controller value %q", family.GetName(), label.GetValue())
					}
				case "source_kind":
					if label.GetValue() != "Ingress" && label.GetValue() != "HTTPRoute" {
						t.Fatalf("metric %s exposes unbounded source kind %q", family.GetName(), label.GetValue())
					}
				default:
					t.Fatalf("metric %s exposes unexpected label %q", family.GetName(), label.GetName())
				}
			}
		}
	}
}
