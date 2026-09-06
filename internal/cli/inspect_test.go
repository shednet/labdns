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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/dnsendpoint"
)

func TestRecordsFlattenFilterAndLifecycle(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := externaldnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "app", Name: "web-www", Generation: 3,
			Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www"},
			Annotations: map[string]string{
				dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: "app",
				dnsendpoint.SourceNameAnnotation: "web", dnsendpoint.SourceUIDAnnotation: "source-uid",
				dnsendpoint.LifecycleAnnotation: `{"version":1,"pending":[{"dnsName":"app.example.com","recordType":"A","target":"192.0.2.2","deadline":"2026-09-02T00:00:00Z"}]}`,
			},
		},
		Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{
			{DNSName: "app.example.com", RecordType: "AAAA", Targets: []string{"2001:db8::1"}, RecordTTL: 300},
			{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.2", "192.0.2.1"}, RecordTTL: 60},
		}},
		Status: externaldnsv1alpha1.DNSEndpointStatus{ObservedGeneration: 2},
	}
	inspector := inspector{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).Build()}
	list, err := inspector.records(context.Background(), filters{Provider: "www", RecordType: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("records = %#v", list.Items)
	}
	record := list.Items[0]
	if !strings.EqualFold(record.ExternalDNSState, "stale") || len(record.Retiring) != 1 || record.Targets[0] != "192.0.2.1" {
		t.Fatalf("record = %#v", record)
	}
}

func TestDetailsCorrelatesProviderAndSource(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{externaldnsv1alpha1.AddToScheme, labdnsv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	provider := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: "www"}, Spec: labdnsv1alpha1.DNSProviderSpec{
		Zones:     []labdnsv1alpha1.DNSZone{{Name: "example.com"}},
		IPSources: labdnsv1alpha1.IPSources{IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: "example.com/ip"}},
	}}
	object := &externaldnsv1alpha1.DNSEndpoint{ObjectMeta: metav1.ObjectMeta{
		Namespace: "app", Name: "web-www", Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www"},
		Annotations: map[string]string{dnsendpoint.SourceKindAnnotation: "Unknown", dnsendpoint.SourceNamespaceAnnotation: "app", dnsendpoint.SourceNameAnnotation: "web", dnsendpoint.LifecycleAnnotation: `{"version":1,"pending":[]}`},
	}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}}}}
	inspector := inspector{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, object).Build()}
	details, err := inspector.details(context.Background(), "app.example.com", filters{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Items) != 1 || !details.Items[0].Provider.Found || details.Items[0].Provider.IPv4Label != "example.com/ip" {
		t.Fatalf("details = %#v", details)
	}
}

func TestOutputsAreMachineAndHumanReadable(t *testing.T) {
	t.Parallel()
	list := recordList{Items: []record{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}, Provider: "www", Source: sourceRef{Kind: "Ingress", Namespace: "app", Name: "web"}, DNSEndpoint: objectRef{Namespace: "app", Name: "generated"}, ExternalDNSState: "observed"}}}
	var output bytes.Buffer
	if err := writeRecordsDefault(&output, list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "DNS NAME") || !strings.Contains(output.String(), "app.example.com") {
		t.Fatalf("table output = %q", output.String())
	}
	output.Reset()
	if err := writeJSON(&output, list); err != nil {
		t.Fatal(err)
	}
	var decoded recordList
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("JSON output = %q", output.String())
	}
}

func TestObservationState(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		generation, observed int64
		want                 string
	}{{2, 2, "observed"}, {2, 0, "unobserved"}, {2, 1, "stale"}, {2, 3, "invalid"}} {
		if got := observationState(test.generation, test.observed); got != test.want {
			t.Errorf("observationState(%d, %d) = %q, want %q", test.generation, test.observed, got, test.want)
		}
	}
}

func TestLifecycleParseFailureIsInvalidAndLeavesActiveTargetsUnknown(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := externaldnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object := &externaldnsv1alpha1.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "app", Name: "web-www", Generation: 2,
			Labels: map[string]string{dnsendpoint.ManagedByLabel: dnsendpoint.ManagedByValue, dnsendpoint.ProviderLabel: "www"},
			Annotations: map[string]string{
				dnsendpoint.SourceKindAnnotation: "Ingress", dnsendpoint.SourceNamespaceAnnotation: "app",
				dnsendpoint.SourceNameAnnotation: "web", dnsendpoint.LifecycleAnnotation: "not-json",
			},
		},
		Spec:   externaldnsv1alpha1.DNSEndpointSpec{Endpoints: []*endpoint.Endpoint{{DNSName: "app.example.com", RecordType: "A", Targets: []string{"192.0.2.1"}}}},
		Status: externaldnsv1alpha1.DNSEndpointStatus{ObservedGeneration: 2},
	}
	inspector := inspector{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).Build()}
	list, err := inspector.records(context.Background(), filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("records = %#v", list.Items)
	}
	record := list.Items[0]
	if record.ExternalDNSState != stateInvalid || record.LifecycleError == "" || record.ActiveTargets != nil {
		t.Fatalf("record = %#v, want invalid lifecycle with unknown active targets", record)
	}
	var output bytes.Buffer
	if err := writeDetailsDefault(&output, recordDetails{Items: []recordDetail{{Record: record}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Active targets:       unknown") || !strings.Contains(output.String(), "ExternalDNS:          invalid") {
		t.Fatalf("details output = %q", output.String())
	}
	var encoded bytes.Buffer
	if err := writeJSON(&encoded, list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"activeTargets": null`) {
		t.Fatalf("invalid lifecycle activeTargets should be null: %s", encoded.String())
	}
}

func TestRecordStateCategoriesAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		record record
		want   string
	}{
		{name: "lifecycle error wins", record: record{LifecycleError: "bad", ExternalDNSState: stateObserved}, want: stateInvalid},
		{name: "generation invalid", record: record{ExternalDNSState: stateInvalid}, want: stateInvalid},
		{name: "observed", record: record{ExternalDNSState: stateObserved}, want: stateObserved},
		{name: "unobserved is stale", record: record{ExternalDNSState: "unobserved"}, want: "stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := recordState(test.record); got != test.want {
				t.Fatalf("recordState(%#v) = %q, want %q", test.record, got, test.want)
			}
		})
	}
}

func TestGatewayFlagReadsOnlyManagerContainer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		annotation string
		containers []corev1.Container
		want       bool
	}{
		{
			name:       "manager true and sidecar true",
			annotation: "manager",
			containers: []corev1.Container{{Name: "manager", Args: []string{"--enable-gateway-api=true"}}, {Name: "metrics", Args: []string{"--enable-gateway-api"}}},
			want:       true,
		},
		{
			name:       "manager false ignores sidecar",
			annotation: "manager",
			containers: []corev1.Container{{Name: "manager", Args: []string{"--enable-gateway-api=false"}}, {Name: "metrics", Args: []string{"--enable-gateway-api"}}},
			want:       false,
		},
		{
			name:       "command fallback",
			containers: []corev1.Container{{Name: "controller", Command: []string{"/manager"}, Args: []string{"-enable-gateway-api"}}, {Name: "metrics", Args: []string{"--enable-gateway-api"}}},
			want:       true,
		},
		{
			name:       "kubectl default sidecar does not override manager",
			annotation: "debug",
			containers: []corev1.Container{{Name: "manager", Args: []string{"--enable-gateway-api=false"}}, {Name: "debug", Args: []string{"--enable-gateway-api"}}},
			want:       false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: test.containers}}
			if test.annotation != "" {
				template.Annotations = map[string]string{defaultContainerAnnotation: test.annotation}
			}
			manager := managerContainer(template)
			if manager == nil {
				t.Fatal("manager container not found")
			}
			enabled, err := gatewayAPIEnabled(manager.Args)
			if err != nil || enabled != test.want {
				t.Fatalf("manager = %#v, enabled = %t, error = %v, want %t", manager, enabled, err, test.want)
			}
		})
	}
}

func TestGatewayFlagRejectsInvalidBoolean(t *testing.T) {
	t.Parallel()
	if _, err := gatewayAPIEnabled([]string{"--enable-gateway-api=perhaps"}); err == nil {
		t.Fatal("expected invalid boolean error")
	}
}

func TestCommandRejectsInvalidRecordFiltersBeforeKubernetesAccess(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list", "--source-kind=Service"}, {"list", "--record-type=TXT"}, {"show", "app.example.com", "--source-kind=service"}} {
		command := NewCommand("test", &bytes.Buffer{}, &bytes.Buffer{})
		command.SetArgs(args)
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("args %v error = %v, want validation error", args, err)
		}
	}
}

func TestStatusReportsHealthyControllerAndPrerequisites(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, externaldnsv1alpha1.AddToScheme, labdnsv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "labdns-system", Name: "labdns", Labels: map[string]string{"app.kubernetes.io/name": "labdns"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "labdns"}}},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "labdns-system", Name: "labdns", Labels: map[string]string{"app": "labdns"}}, Status: corev1.PodStatus{
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	discoveryClient := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
		{GroupVersion: labdnsv1alpha1.GroupVersion.String(), APIResources: []metav1.APIResource{{Name: "dnsproviders"}}},
		{GroupVersion: externaldnsv1alpha1.GroupVersion.String(), APIResources: []metav1.APIResource{{Name: "dnsendpoints"}}},
	}}}
	inspector := inspector{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, pod).Build(), Discovery: discoveryClient}
	status, failed := inspector.status(context.Background(), "", "")
	if failed || status.Overall != "healthy" || len(status.Controllers) != 1 || status.Controllers[0].ReadyPods != 1 {
		t.Fatalf("status = %#v, failed = %t", status, failed)
	}
}
