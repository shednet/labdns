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

func seedLifecyclePublication(t *testing.T, kubeClient client.Client, identity source.Identity) (*dnsendpoint.Writer, client.ObjectKey) {
	t.Helper()
	output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}}
	publication := source.Publication{ProviderName: "www", DeletionDelay: time.Minute, Records: []source.Record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, TTL: 300}}}
	if err := output.Apply(context.Background(), identity, []source.Publication{publication}); err != nil {
		t.Fatal(err)
	}
	return output, client.ObjectKey{Namespace: identity.Namespace, Name: dnsendpoint.ObjectName(identity, "www")}
}

func lifecycleEndpoint(identity source.Identity, deletionDelay, lifecycle string) *externaldnsv1alpha1.DNSEndpoint {
	annotations := map[string]string{
		dnsendpoint.SourceKindAnnotation:      string(identity.Kind),
		dnsendpoint.SourceNamespaceAnnotation: identity.Namespace,
		dnsendpoint.SourceNameAnnotation:      identity.Name,
		dnsendpoint.DeletionDelayAnnotation:   deletionDelay,
		dnsendpoint.LifecycleAnnotation:       lifecycle,
	}
	if identity.UID != "" {
		annotations[dnsendpoint.SourceUIDAnnotation] = string(identity.UID)
	}
	return &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsendpoint.ObjectName(identity, "www"),
			Namespace: identity.Namespace,
			Labels: map[string]string{
				dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue,
				dnsendpoint.ProviderLabel:  "www",
				dnsendpoint.SourceKeyLabel: dnsendpoint.SourceKey(identity),
			},
			Annotations: annotations,
		},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"},
		}}},
	}
}

func TestLifecycleReconcilerRetirementModes(t *testing.T) {
	tests := []struct {
		name              string
		identity          source.Identity
		sourceObject      client.Object
		gatewayEnabled    bool
		forbidGatewayRead bool
		wantGatewayReads  int
		wantRetired       bool
		wantRequeueAfter  time.Duration
	}{
		{
			name:             "missing ingress",
			identity:         source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("old")},
			wantRetired:      true,
			wantRequeueAfter: time.Minute,
		},
		{
			name:              "gateway disabled HTTPRoute",
			identity:          source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: "app", Name: "route", UID: types.UID("route-uid")},
			forbidGatewayRead: true,
			wantRetired:       true,
			wantRequeueAfter:  time.Minute,
		},
		{
			name:           "gateway enabled live HTTPRoute",
			identity:       source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: "app", Name: "route", UID: types.UID("route-uid")},
			sourceObject:   &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app", UID: types.UID("route-uid")}},
			gatewayEnabled: true,
			wantRetired:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{}
			if tc.sourceObject != nil {
				objects = append(objects, tc.sourceObject)
			}
			base := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(objects...).Build()
			var kubeClient client.Client = base
			var gatewayReader *noGatewayReadClient
			if tc.forbidGatewayRead {
				gatewayReader = &noGatewayReadClient{Client: base}
				kubeClient = gatewayReader
			}
			output, key := seedLifecyclePublication(t, kubeClient, tc.identity)
			result, err := (&lifecycleReconciler{Client: kubeClient, Output: output, GatewayEnabled: tc.gatewayEnabled}).Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err != nil {
				t.Fatal(err)
			}
			if gatewayReader != nil && gatewayReader.reads != tc.wantGatewayReads {
				t.Fatalf("Gateway API reads=%d, want %d", gatewayReader.reads, tc.wantGatewayReads)
			}
			if result.RequeueAfter != tc.wantRequeueAfter {
				t.Fatalf("requeue=%s, want %s", result.RequeueAfter, tc.wantRequeueAfter)
			}
			var object externaldnsv1alpha1.DNSEndpoint
			if err := kubeClient.Get(context.Background(), key, &object); err != nil {
				t.Fatal(err)
			}
			retired := strings.Contains(object.Annotations[dnsendpoint.LifecycleAnnotation], `"target":"192.0.2.1"`)
			if retired != tc.wantRetired {
				t.Fatalf("retired=%t, lifecycle=%s, want %t", retired, object.Annotations[dnsendpoint.LifecycleAnnotation], tc.wantRetired)
			}
		})
	}
}

func TestLifecycleReconcilerUIDMismatchWaitsForSourceController(t *testing.T) {
	ctx := context.Background()
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("old")}
	sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app", UID: types.UID("new")}}
	kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject).Build()
	output, key := seedLifecyclePublication(t, kubeClient, identity)
	var before externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	if _, err := (&lifecycleReconciler{Client: kubeClient, Output: output}).Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
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
	_, err := (&lifecycleReconciler{Client: kubeClient, Output: output, Recorder: recorder}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	if err == nil {
		t.Fatal("malformed lifecycle accepted")
	}
	if !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("error = %v, want terminal error", err)
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

func TestLifecycleReconcilerInvalidWriterStateIsTerminal(t *testing.T) {
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: "app", Name: "web", UID: types.UID("uid")}
	for name, annotations := range map[string]map[string]string{
		"invalid deletion delay": {
			dnsendpoint.DeletionDelayAnnotation: "bad",
			dnsendpoint.LifecycleAnnotation:     `{"version":1,"pending":[]}`,
		},
		"pending target absent from spec": {
			dnsendpoint.DeletionDelayAnnotation: "1m0s",
			dnsendpoint.LifecycleAnnotation:     `{"version":1,"pending":[{"dnsName":"app.example.com","recordType":"A","target":"192.0.2.2","deadline":"2099-01-01T00:00:00Z"}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			sourceObject := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: identity.Name, Namespace: identity.Namespace, UID: identity.UID}}
			object := lifecycleEndpoint(identity, annotations[dnsendpoint.DeletionDelayAnnotation], annotations[dnsendpoint.LifecycleAnnotation])
			kubeClient := fake.NewClientBuilder().WithScheme(lifecycleScheme(t)).WithObjects(sourceObject, object).Build()
			output := &dnsendpoint.Writer{Client: kubeClient, Clock: lifecycleClock{now: time.Now()}}
			_, err := (&lifecycleReconciler{Client: kubeClient, Output: output}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
			if !errors.Is(err, reconcile.TerminalError(nil)) {
				t.Fatalf("error = %v, want terminal error", err)
			}
		})
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
	_, err := (&lifecycleReconciler{Client: kubeClient, Output: output, Recorder: recorder}).Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(object)})
	if err == nil {
		t.Fatal("missing source UID accepted")
	}
	if !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("error = %v, want terminal error", err)
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
