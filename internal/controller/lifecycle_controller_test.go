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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/shednet/labdns/internal/dnsendpoint"
	"github.com/shednet/labdns/internal/source"
)

type lifecycleClock struct{ now time.Time }

type noGatewayReadClient struct {
	client.Client
	reads int
}

func (c *noGatewayReadClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, gatewayRead := object.(*gatewayv1.HTTPRoute); gatewayRead {
		c.reads++
		return errors.New("Gateway API read is forbidden")
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (clock lifecycleClock) Now() time.Time { return clock.now }

func lifecycleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{networkingv1.AddToScheme, externaldnsv1alpha1.AddToScheme, gatewayv1.Install} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func TestLifecycleReconcilerMissingSourceSchedulesAndRequeues(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("old")}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).Build()
	clock := lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: clock}
	publication := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 300}}}
	if err := output.Apply(ctx, identity, []source.Publication{publication}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "app", Name: dnsendpoint.ObjectName(identity, "www")}
	result, err := (&LifecycleReconciler{Client: kubeClient, Output: output}).Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("requeue=%s", result.RequeueAfter)
	}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"192.0.2.1"`) {
		t.Fatalf("lifecycle=%s", object.Annotations[dnsendpoint.LifecycleAnnotation])
	}
}

func TestLifecycleReconcilerGatewayDisabledRetiresWithoutGatewayRead(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: "app", Name: "route", UID: types.UID("route-uid")}
	base := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).Build()
	kubeClient := &noGatewayReadClient{Client: base}
	clock := lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: clock}
	publication := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 300}}}
	if err := output.Apply(ctx, identity, []source.Publication{publication}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "app", Name: dnsendpoint.ObjectName(identity, "www")}
	result, err := (&LifecycleReconciler{Client: kubeClient, Output: output, GatewayEnabled: false}).Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if kubeClient.reads != 0 {
		t.Fatalf("Gateway API reads=%d", kubeClient.reads)
	}
	if result.RequeueAfter != time.Minute {
		t.Fatalf("requeue=%s", result.RequeueAfter)
	}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"192.0.2.1"`) {
		t.Fatalf("HTTPRoute output was not retired: %s", object.Annotations[dnsendpoint.LifecycleAnnotation])
	}
}

func TestLifecycleReconcilerGatewayEnabledVerifiesHTTPRouteUID(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: "app", Name: "route", UID: types.UID("route-uid")}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: identity.Name, Namespace: identity.Namespace, UID: identity.UID}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(route).Build()
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}}
	publication := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 300}}}
	if err := output.Apply(ctx, identity, []source.Publication{publication}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "app", Name: dnsendpoint.ObjectName(identity, "www")}
	result, err := (&LifecycleReconciler{Client: kubeClient, Output: output, GatewayEnabled: true}).Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue=%s", result.RequeueAfter)
	}
	var object externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &object); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target"`) {
		t.Fatal("live HTTPRoute was retired with Gateway enabled")
	}
}

func TestLifecycleReconcilerUIDMismatchWaitsForSourceController(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("old")}
	sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app", UID: types.UID("new")}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject).Build()
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Now()}}
	publication := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 300}}}
	if err := output.Apply(ctx, identity, []source.Publication{publication}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "app", Name: dnsendpoint.ObjectName(identity, "www")}
	var before externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	if _, err := (&LifecycleReconciler{Client: kubeClient, Output: output}).Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var after externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion || after.Annotations[dnsendpoint.SourceUIDAnnotation] != "old" {
		t.Fatal("UID mismatch altered output before source reconciliation")
	}
}

func TestLifecycleReconcilerMalformedStateEmitsWarningAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("uid")}
	sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app", UID: identity.UID}}
	object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{Name: dnsendpoint.ObjectName(identity, "www"), Namespace: "app", Labels: map[string]string{dnsendpoint.ManagedByLabel: "labdns", dnsendpoint.ProviderLabel: "www", dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity)}, Annotations: map[string]string{dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: "app", dnsendpoint.SourceNameAnnotation: "web", dnsendpoint.SourceUIDAnnotation: "uid", dnsendpoint.DeletionDelayAnnotation: "1m0s", dnsendpoint.LifecycleAnnotation: "{"}}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject, object).Build()
	recorder := events.NewFakeRecorder(1)
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Now()}}
	_, err := (&LifecycleReconciler{Client: kubeClient, Output: output, Recorder: recorder}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	if err == nil {
		t.Fatal("malformed lifecycle accepted")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "LifecycleFailed") {
			t.Fatalf("event=%q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("warning event not emitted")
	}
	var unchanged externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.ResourceVersion != object.ResourceVersion || len(unchanged.Spec.Endpoints[0].Targets) != 1 {
		t.Fatal("malformed lifecycle was not fail-closed")
	}
}

func TestLifecycleReconcilerMissingSourceUIDWarnsAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web"}
	sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app", UID: types.UID("live")}}
	object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{Name: dnsendpoint.ObjectName(identity, "www"), Namespace: "app", Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www", dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity)}, Annotations: map[string]string{dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: "app", dnsendpoint.SourceNameAnnotation: "web", dnsendpoint.DeletionDelayAnnotation: "1m0s", dnsendpoint.LifecycleAnnotation: `{"version":1,"pending":[]}`}}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject, object).Build()
	recorder := events.NewFakeRecorder(1)
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Now()}}
	_, err := (&LifecycleReconciler{Client: kubeClient, Output: output, Recorder: recorder}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	if err == nil {
		t.Fatal("missing source UID accepted")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "LifecycleFailed") {
			t.Fatalf("event=%q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("warning event not emitted")
	}
	var unchanged externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.ResourceVersion != object.ResourceVersion || len(unchanged.Spec.Endpoints) != 1 || unchanged.Annotations[dnsendpoint.SourceUIDAnnotation] != "" {
		t.Fatal("missing UID did not fail closed")
	}
}
