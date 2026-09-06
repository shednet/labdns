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

package dnsendpoint

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/shednet/labdns/internal/source"
)

type fakeClock struct{ value time.Time }

const (
	targetOne = "192.0.2.1"
	targetTwo = "192.0.2.2"
)

func (clock *fakeClock) Now() time.Time { return clock.value }

type countingClient struct {
	client.Client
	creates, updates, deletes int
}

type conflictClient struct {
	client.Client
	conflicts int
}

type deleteConflictClient struct {
	client.Client
	conflicts int
}

func (c *deleteConflictClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if c.conflicts == 0 {
		c.conflicts++
		backing := c.Client
		var concurrent externaldnsv1alpha1.DNSEndpoint
		if err := backing.Get(ctx, client.ObjectKeyFromObject(object), &concurrent); err != nil {
			return err
		}
		concurrent.Annotations[LifecycleAnnotation] = `{"version":2,"pending":[]}`
		if err := backing.Update(ctx, &concurrent); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Group: externaldnsv1alpha1.GroupVersion.Group, Resource: "dnsendpoints"}, object.GetName(), errors.New("injected delete conflict"))
	}
	return c.Client.Delete(ctx, object, options...)
}

func (c *conflictClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if c.conflicts == 0 {
		c.conflicts++
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
		return apierrors.NewConflict(schema.GroupResource{Group: externaldnsv1alpha1.GroupVersion.Group, Resource: "dnsendpoints"}, object.GetName(), errors.New("injected conflict"))
	}
	return c.Client.Update(ctx, object, options...)
}

func (c *countingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.creates++
	return c.Client.Create(ctx, object, options...)
}
func (c *countingClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, object, options...)
}
func (c *countingClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	c.deletes++
	return c.Client.Delete(ctx, object, options...)
}

func newWriter(t *testing.T, objects ...client.Object) (*Writer, *countingClient, *fakeClock) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := externaldnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	counted := &countingClient{Client: base}
	clock := &fakeClock{value: time.Date(2026, 8, 30, 20, 0, 0, 123456789, time.UTC)}
	return &Writer{Client: counted, Clock: clock}, counted, clock
}

func testIdentity() source.Identity {
	return source.Identity{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "app", Name: "My_App", UID: types.UID("uid-one")}
}
func publication(provider string, delay time.Duration, records ...source.Record) source.Publication {
	return source.Publication{ProviderName: provider, DeletionDelay: delay, Records: records}
}
func record(targets ...string) source.Record {
	return source.Record{DNSName: "same.example.com", RecordType: "A", Targets: targets, TTL: 300, ProviderSpecific: []source.Property{{Name: "proxy", Value: "false"}}}
}
func getEndpoint(t *testing.T, kubeClient client.Client, identity source.Identity, provider string) *externaldnsv1alpha1.DNSEndpoint {
	t.Helper()
	var object externaldnsv1alpha1.DNSEndpoint
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: identity.Namespace, Name: ObjectName(identity, provider)}, &object); err != nil {
		t.Fatal(err)
	}
	return &object
}

func TestDeterministicIdentity(t *testing.T) {
	identity := testIdentity()
	if got := SourceKey(identity); len(got) != 16 {
		t.Fatalf("source key %q", got)
	}
	name := ObjectName(identity, "WWW.Profile")
	if !strings.HasPrefix(name, "ingress-my-app-www-profile-") || len(name) > 253 {
		t.Fatalf("object name %q", name)
	}
	recreated := identity
	recreated.UID = "different"
	if SourceKey(recreated) != SourceKey(identity) || ObjectName(recreated, "WWW.Profile") != name {
		t.Fatal("UID changed deterministic identity")
	}
}

func TestProviderIsolationMetadataAndIdempotency(t *testing.T) {
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	www := publication("www", time.Minute, record(targetTwo, targetOne))
	www.MetadataAnnotations = map[string]string{"external-dns.alpha.kubernetes.io/cloudflare-proxied": "true"}
	vpnRecord := record("10.0.0.2")
	vpnRecord.ProviderSpecific = nil
	vpn := publication("vpn", 2*time.Minute, vpnRecord)
	if err := m.Apply(context.Background(), identity, []source.Publication{vpn, www}); err != nil {
		t.Fatal(err)
	}
	if counted.creates != 2 {
		t.Fatalf("creates=%d", counted.creates)
	}
	wwwObject := getEndpoint(t, counted, identity, "www")
	vpnObject := getEndpoint(t, counted, identity, "vpn")
	if wwwObject.Labels[ProviderLabel] != "www" || vpnObject.Labels[ProviderLabel] != "vpn" || wwwObject.Labels[SourceKeyLabel] != vpnObject.Labels[SourceKeyLabel] {
		t.Fatal("incorrect routing labels")
	}
	if wwwObject.Annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"] != "true" {
		t.Fatal("www annotation missing")
	}
	if _, found := vpnObject.Annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"]; found {
		t.Fatal("metadata crossed providers")
	}
	if len(wwwObject.OwnerReferences) != 0 || len(wwwObject.Finalizers) != 0 {
		t.Fatal("new object has lifecycle coupling")
	}
	if err := m.Apply(context.Background(), identity, []source.Publication{www, vpn}); err != nil {
		t.Fatal(err)
	}
	if counted.updates != 0 || counted.deletes != 0 {
		t.Fatalf("identical apply wrote: updates=%d deletes=%d", counted.updates, counted.deletes)
	}
	if err := m.Apply(context.Background(), identity, []source.Publication{www}); err != nil {
		t.Fatal(err)
	}
	vpnObject = getEndpoint(t, counted, identity, "vpn")
	state, err := parseLifecycle(vpnObject.Annotations[LifecycleAnnotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].Target != "10.0.0.2" {
		t.Fatalf("removed provider was not retired independently: %#v", state.Pending)
	}
	wwwObject = getEndpoint(t, counted, identity, "www")
	state, err = parseLifecycle(wwwObject.Annotations[LifecycleAnnotation])
	if err != nil || len(state.Pending) != 0 {
		t.Fatalf("retiring vpn affected www: state=%#v error=%v", state, err)
	}
}

func TestPerTargetDelayChangesAndCancellation(t *testing.T) {
	m, counted, clock := newWriter(t)
	identity := testIdentity()
	first := publication("www", time.Minute, record(targetOne, targetTwo))
	if err := m.Apply(context.Background(), identity, []source.Publication{first}); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(10 * time.Second)
	changed := record(targetTwo)
	changed.TTL = 600
	changed.ProviderSpecific = []source.Property{{Name: "proxy", Value: "true"}}
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", 5*time.Minute, changed)}); err != nil {
		t.Fatal(err)
	}
	object := getEndpoint(t, counted, identity, "www")
	if len(object.Spec.Endpoints) != 1 || len(object.Spec.Endpoints[0].Targets) != 2 || object.Spec.Endpoints[0].RecordTTL != 600 || object.Spec.Endpoints[0].ProviderSpecific[0].Value != "true" {
		t.Fatalf("pending RRset did not receive immediate properties: %#v", object.Spec.Endpoints)
	}
	state, err := parseLifecycle(object.Annotations[LifecycleAnnotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].Target != targetOne || state.Pending[0].Deadline != "2026-08-30T20:01:10.123456789Z" {
		t.Fatalf("pending=%#v", state.Pending)
	}
	// Reappearance cancels exactly that target.
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", 5*time.Minute, record(targetOne, targetTwo))}); err != nil {
		t.Fatal(err)
	}
	object = getEndpoint(t, counted, identity, "www")
	state, err = parseLifecycle(object.Annotations[LifecycleAnnotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending not canceled: %#v", state.Pending)
	}
	// Retire one again and expire only it.
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", 5*time.Minute, record(targetTwo))}); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(5 * time.Minute)
	if _, err := m.Advance(context.Background(), client.ObjectKeyFromObject(getEndpoint(t, counted, identity, "www"))); err != nil {
		t.Fatal(err)
	}
	object = getEndpoint(t, counted, identity, "www")
	if got := object.Spec.Endpoints[0].Targets; len(got) != 1 || got[0] != targetTwo {
		t.Fatalf("targets=%v", got)
	}
}

func TestRetirementDeletionUIDReplacementAndRestartRecovery(t *testing.T) {
	m, counted, clock := newWriter(t)
	identity := testIdentity()
	desired := publication("www", 30*time.Second, record(targetOne))
	if err := m.Apply(context.Background(), identity, []source.Publication{desired}); err != nil {
		t.Fatal(err)
	}
	// Source deletion has no UID and preserves the old UID while scheduling.
	deleted := identity
	deleted.UID = ""
	if err := m.Apply(context.Background(), deleted, nil); err != nil {
		t.Fatal(err)
	}
	object := getEndpoint(t, counted, identity, "www")
	if object.Annotations[SourceUIDAnnotation] != "uid-one" {
		t.Fatalf("lost stored UID: %q", object.Annotations[SourceUIDAnnotation])
	}
	// A same-name source can cancel pending targets and replaces the UID.
	recreated := identity
	recreated.UID = "uid-two"
	if err := m.Apply(context.Background(), recreated, []source.Publication{desired}); err != nil {
		t.Fatal(err)
	}
	object = getEndpoint(t, counted, identity, "www")
	state, err := parseLifecycle(object.Annotations[LifecycleAnnotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 0 || object.Annotations[SourceUIDAnnotation] != "uid-two" {
		t.Fatal("recreation did not replace UID and cancel pending")
	}
	// Simulate removal followed by a fresh process using only durable object state.
	if err := m.Apply(context.Background(), recreated, nil); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(31 * time.Second)
	restarted := &Writer{Client: counted, Clock: clock}
	if _, err := restarted.Advance(context.Background(), client.ObjectKey{Namespace: identity.Namespace, Name: ObjectName(identity, "www")}); err != nil {
		t.Fatal(err)
	}
	var absent externaldnsv1alpha1.DNSEndpoint
	if err := counted.Get(context.Background(), client.ObjectKey{Namespace: identity.Namespace, Name: ObjectName(identity, "www")}, &absent); err == nil {
		t.Fatal("expired object remains")
	}
}

func TestMalformedLifecycleFailsClosed(t *testing.T) {
	identity := testIdentity()
	for name, value := range map[string]string{
		"malformed":             "{",
		"unknown-version":       `{"version":2,"pending":[]}`,
		"unknown-top-level":     `{"version":1,"pending":[],"future":true}`,
		"unknown-pending-field": `{"version":1,"pending":[{"dnsName":"same.example.com","recordType":"A","target":"192.0.2.1","deadline":"2026-08-30T20:01:00Z","future":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{Name: ObjectName(identity, "www"), Namespace: identity.Namespace, Labels: map[string]string{ManagedByLabel: ManagedByValue, ProviderLabel: "www", SourceKeyLabel: SourceKey(identity)}, Annotations: map[string]string{LifecycleAnnotation: value, DeletionDelayAnnotation: "1m0s"}}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "same.example.com", RecordType: "A", Targets: []string{targetOne}}}}}
			m, counted, _ := newWriter(t, object)
			if err := m.Apply(context.Background(), identity, nil); err == nil {
				t.Fatal("accepted invalid lifecycle")
			} else if !IsInvalidState(err) {
				t.Fatalf("error = %v, want invalid state", err)
			}
			got := getEndpoint(t, counted, identity, "www")
			if len(got.Spec.Endpoints[0].Targets) != 1 || counted.updates != 0 || counted.deletes != 0 {
				t.Fatal("invalid lifecycle was not fail-closed")
			}
		})
	}
}

func TestMissingRequiredStoredMetadataFailsClosed(t *testing.T) {
	identity := testIdentity()
	validLifecycle := `{"version":1,"pending":[]}`
	for name, annotations := range map[string]map[string]string{
		"lifecycle":      {DeletionDelayAnnotation: "1m0s"},
		"deletion-delay": {LifecycleAnnotation: validLifecycle},
	} {
		t.Run(name, func(t *testing.T) {
			object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{Name: ObjectName(identity, "www"), Namespace: identity.Namespace, Labels: map[string]string{ManagedByLabel: ManagedByValue, ProviderLabel: "www", SourceKeyLabel: SourceKey(identity)}, Annotations: annotations}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "same.example.com", RecordType: "A", Targets: []string{targetOne}}}}}
			m, counted, _ := newWriter(t, object)
			if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, record(targetTwo))}); err == nil {
				t.Fatalf("accepted missing %s", name)
			} else if !IsInvalidState(err) {
				t.Fatalf("error = %v, want invalid state", err)
			}
			got := getEndpoint(t, counted, identity, "www")
			if len(got.Spec.Endpoints) != 1 || len(got.Spec.Endpoints[0].Targets) != 1 || got.Spec.Endpoints[0].Targets[0] != targetOne || counted.updates != 0 || counted.deletes != 0 {
				t.Fatalf("missing %s was not fail-closed", name)
			}
		})
	}
}

func TestPreservesUnrelatedMetadataAndStatus(t *testing.T) {
	const kept = "kept"
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, record(targetOne))}); err != nil {
		t.Fatal(err)
	}
	object := getEndpoint(t, counted, identity, "www")
	object.Labels["example.test/foreign"] = kept
	object.Annotations["example.test/foreign"] = kept
	object.Finalizers = []string{"example.test/foreign"}
	object.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: identity.APIVersion, Kind: string(identity.Kind), Name: identity.Name, UID: identity.UID},
		{APIVersion: "example.test/v1", Kind: "Foreign", Name: "kept", UID: types.UID("foreign")},
	}
	object.Status.ObservedGeneration = 7
	if err := counted.Client.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	changed := record(targetTwo)
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, changed)}); err != nil {
		t.Fatal(err)
	}
	object = getEndpoint(t, counted, identity, "www")
	if object.Labels["example.test/foreign"] != kept || object.Annotations["example.test/foreign"] != kept || len(object.Finalizers) != 1 || len(object.OwnerReferences) != 1 || object.OwnerReferences[0].Kind != "Foreign" || object.Status.ObservedGeneration != 7 {
		t.Fatal("unrelated metadata or status lost")
	}
}

func TestSourceOwnerReferenceOnlyDeltaIsWritten(t *testing.T) {
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	desired := publication("www", time.Minute, record(targetOne))
	if err := m.Apply(context.Background(), identity, []source.Publication{desired}); err != nil {
		t.Fatal(err)
	}
	object := getEndpoint(t, counted, identity, "www")
	object.OwnerReferences = []metav1.OwnerReference{{APIVersion: identity.APIVersion, Kind: string(identity.Kind), Name: identity.Name, UID: identity.UID}}
	if err := counted.Client.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(context.Background(), identity, []source.Publication{desired}); err != nil {
		t.Fatal(err)
	}
	if counted.updates != 1 {
		t.Fatalf("owner-reference-only cleanup writes=%d", counted.updates)
	}
	object = getEndpoint(t, counted, identity, "www")
	if len(object.OwnerReferences) != 0 {
		t.Fatalf("source owner references remain: %#v", object.OwnerReferences)
	}
}

func TestSharedHostnameRecordsMergeIntoOneExactRRSet(t *testing.T) {
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	first, second := record(targetTwo), record(targetOne, targetTwo)
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, first, second)}); err != nil {
		t.Fatal(err)
	}
	object := getEndpoint(t, counted, identity, "www")
	if len(object.Spec.Endpoints) != 1 {
		t.Fatalf("endpoints=%#v", object.Spec.Endpoints)
	}
	if got := object.Spec.Endpoints[0].Targets; len(got) != 2 || got[0] != targetOne || got[1] != targetTwo {
		t.Fatalf("targets=%v", got)
	}
}

func TestEarliestDeadlineAndEmptyRecordRemoval(t *testing.T) {
	m, counted, clock := newWriter(t)
	identity := testIdentity()
	a := record(targetOne, targetTwo)
	aaaa := source.Record{DNSName: "same.example.com", RecordType: "AAAA", Targets: []string{"2001:db8::1"}, TTL: 300}
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, a, aaaa)}); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(10 * time.Second)
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, record(targetTwo), aaaa)}); err != nil {
		t.Fatal(err)
	}
	clock.value = clock.value.Add(10 * time.Second)
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, aaaa)}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: identity.Namespace, Name: ObjectName(identity, "www")}
	clock.value = clock.value.Add(30 * time.Second)
	next, err := m.Advance(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if next != 20*time.Second {
		t.Fatalf("earliest requeue=%s", next)
	}
	clock.value = clock.value.Add(21 * time.Second)
	next, err = m.Advance(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if next != 9*time.Second {
		t.Fatalf("second requeue=%s", next)
	}
	object := getEndpoint(t, counted, identity, "www")
	if len(object.Spec.Endpoints) != 2 || object.Spec.Endpoints[0].RecordType != "A" || len(object.Spec.Endpoints[0].Targets) != 1 || object.Spec.Endpoints[0].Targets[0] != targetTwo {
		t.Fatalf("first expiration removed more than one target: %#v", object.Spec.Endpoints)
	}
	clock.value = clock.value.Add(10 * time.Second)
	if _, err := m.Advance(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	object = getEndpoint(t, counted, identity, "www")
	if len(object.Spec.Endpoints) != 1 || object.Spec.Endpoints[0].RecordType != "AAAA" {
		t.Fatalf("empty A entry was not removed independently: %#v", object.Spec.Endpoints)
	}
}

func TestOptimisticConflictRefetchesAndConverges(t *testing.T) {
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, record(targetOne))}); err != nil {
		t.Fatal(err)
	}
	injected := &conflictClient{Client: counted.Client}
	m.Client = injected
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", time.Minute, record(targetTwo))}); err != nil {
		t.Fatal(err)
	}
	if injected.conflicts != 1 {
		t.Fatalf("conflicts=%d", injected.conflicts)
	}
	object := getEndpoint(t, counted, identity, "www")
	if got := object.Spec.Endpoints[0].Targets; len(got) != 2 {
		t.Fatalf("rotation should retain old and publish new target: %v", got)
	}
	if object.Annotations["example.test/concurrent"] != "preserved" {
		t.Fatal("concurrent unrelated metadata was lost after update retry")
	}
}

func TestOptimisticDeleteConflictRefetchesAndConverges(t *testing.T) {
	m, counted, _ := newWriter(t)
	identity := testIdentity()
	if err := m.Apply(context.Background(), identity, []source.Publication{publication("www", 0, record(targetOne))}); err != nil {
		t.Fatal(err)
	}
	injected := &deleteConflictClient{Client: counted.Client}
	m.Client = injected
	if err := m.Apply(context.Background(), identity, nil); err == nil {
		t.Fatal("concurrent invalid lifecycle did not stop delete retry")
	}
	if injected.conflicts != 1 {
		t.Fatalf("conflicts=%d", injected.conflicts)
	}
	object := getEndpoint(t, counted, identity, "www")
	if object.Annotations[LifecycleAnnotation] != `{"version":2,"pending":[]}` || len(object.Spec.Endpoints) != 1 {
		t.Fatal("delete retry did not fail closed on concurrent lifecycle mutation")
	}
}
