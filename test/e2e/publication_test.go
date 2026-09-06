//go:build e2e

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

package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

const (
	systemNamespace = "labdns-e2e-system"
	workNamespace   = "labdns-e2e-work"
	hostname        = "shared.e2e.example.test"
	deletionHost    = "delete.e2e.example.test"
	deletionDelay   = 15 * time.Second
	normalTimeout   = 10 * time.Second
	ownerWWW        = "labdns-e2e-www"
	ownerVPN        = "labdns-e2e-vpn"
	foreignRaw      = `{"host":"203.0.113.250","ttl":123,"owner":"foreign-owner"}`
)

var (
	kubeClient  client.Client
	nodes       []corev1.Node
	wwwDNS      *dnsForward
	vpnDNS      *dnsForward
	kubeconfig  string
	kubeContext string
)

type addressPair struct {
	v4 string
	v6 string
}

var (
	publicInitial = []addressPair{{"192.0.2.11", "2001:db8:1::11"}, {"192.0.2.12", "2001:db8:1::12"}, {"192.0.2.13", "2001:db8:1::13"}}
	vpnInitial    = []addressPair{{"198.51.100.11", "2001:db8:2::11"}, {"198.51.100.12", "2001:db8:2::12"}, {"198.51.100.13", "2001:db8:2::13"}}
)

var _ = BeforeSuite(func(ctx SpecContext) {
	cluster := requiredEnvironment("KIND_CLUSTER")
	invocation := requiredEnvironment("E2E_INVOCATION_ID")
	kubeconfig = requiredEnvironment("KUBECONFIG")
	kubeContext = "kind-" + cluster
	Expect(kubeconfig).To(Equal("/tmp/labdns-kind-kubeconfig-" + invocation))
	Expect(cluster).To(MatchRegexp(`^labdns-e2e-[a-z0-9]([-a-z0-9]{0,50}[a-z0-9])?$`), "the suite only accepts a safe invocation-unique labdns Kind cluster")
	Expect(invocation).To(MatchRegexp(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`))
	scheme := k8sruntime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(labdnsv1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(externaldnsv1alpha1.AddToScheme(scheme)).To(Succeed())
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	kubeClient, err = client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	var nodeList corev1.NodeList
	Expect(kubeClient.List(ctx, &nodeList)).To(Succeed())
	Expect(nodeList.Items).To(HaveLen(3))
	sort.Slice(nodeList.Items, func(i, j int) bool { return nodeList.Items[i].Name < nodeList.Items[j].Name })
	nodes = nodeList.Items
	for i := range nodes {
		setNodeAddresses(ctx, &nodes[i], publicInitial[i], vpnInitial[i])
	}

	wwwDNS = startDNSForward(ctx, "coredns-www")
	vpnDNS = startDNSForward(ctx, "coredns-vpn")
}, NodeTimeout(8*time.Minute))

var _ = AfterSuite(func() {
	if wwwDNS != nil {
		wwwDNS.Stop()
	}
	if vpnDNS != nil {
		vpnDNS.Stop()
	}
})

var _ = Describe("the real split-horizon publication path", func() {
	It("publishes, updates, and retires provider-isolated records", func(ctx SpecContext) {
		By("publishing exact provider-isolated multi-node A and AAAA sets", func() {
			createWorkload(ctx)

			wwwExpected := []addressPair{publicInitial[0], publicInitial[1]}
			vpnExpected := []addressPair{vpnInitial[0], vpnInitial[1]}
			assertDNSEventually(ctx, wwwDNS, wwwExpected, normalTimeout)
			assertDNSEventually(ctx, vpnDNS, vpnExpected, normalTimeout)
			assertGeneratedEndpoints(ctx, wwwExpected, vpnExpected, false)
			assertEtcdOwnership(ctx, "www", ownerWWW, wwwExpected)
			assertEtcdOwnership(ctx, "vpn", ownerVPN, vpnExpected)

			putForeignRecord(ctx, "www")
			putForeignRecord(ctx, "vpn")
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})

		By("converges from events after ExternalDNS restarts and Node labels change", func() {
			restartDeployment(ctx, "external-dns-www")
			restartDeployment(ctx, "external-dns-vpn")

			updatedWWW := addressPair{"192.0.2.111", "2001:db8:1::111"}
			updatedVPN := addressPair{"198.51.100.111", "2001:db8:2::111"}
			setNodeAddresses(ctx, &nodes[0], updatedWWW, updatedVPN)
			wwwDuringGrace := []addressPair{publicInitial[0], updatedWWW, publicInitial[1]}
			vpnDuringGrace := []addressPair{vpnInitial[0], updatedVPN, vpnInitial[1]}
			assertDNSEventually(ctx, wwwDNS, wwwDuringGrace, normalTimeout)
			assertDNSEventually(ctx, vpnDNS, vpnDuringGrace, normalTimeout)
			assertGeneratedEndpoints(ctx, wwwDuringGrace, vpnDuringGrace, true)

			wwwExpected := []addressPair{updatedWWW, publicInitial[1]}
			vpnExpected := []addressPair{updatedVPN, vpnInitial[1]}
			assertDNSEventually(ctx, wwwDNS, wwwExpected, deletionDelay+normalTimeout)
			assertDNSEventually(ctx, vpnDNS, vpnExpected, deletionDelay+normalTimeout)
			assertGeneratedEndpoints(ctx, wwwExpected, vpnExpected, false)
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})

		By("retains only the removed target until its per-target grace expires", func() {
			setEndpointPlacement(ctx, nodes[0].Name, nodes[1].Name, nodes[2].Name)
			updatedWWW := addressPair{"192.0.2.111", "2001:db8:1::111"}
			updatedVPN := addressPair{"198.51.100.111", "2001:db8:2::111"}
			wwwDuringGrace := []addressPair{updatedWWW, publicInitial[1], publicInitial[2]}
			vpnDuringGrace := []addressPair{updatedVPN, vpnInitial[1], vpnInitial[2]}
			assertDNSEventually(ctx, wwwDNS, wwwDuringGrace, normalTimeout)
			assertDNSEventually(ctx, vpnDNS, vpnDuringGrace, normalTimeout)
			assertGeneratedEndpoints(ctx, wwwDuringGrace, vpnDuringGrace, true)

			wwwAfterGrace := []addressPair{updatedWWW, publicInitial[2]}
			vpnAfterGrace := []addressPair{updatedVPN, vpnInitial[2]}
			assertDNSEventually(ctx, wwwDNS, wwwAfterGrace, deletionDelay+normalTimeout)
			assertDNSEventually(ctx, vpnDNS, vpnAfterGrace, deletionDelay+normalTimeout)
			assertGeneratedEndpoints(ctx, wwwAfterGrace, vpnAfterGrace, false)
			assertEtcdOwnership(ctx, "www", ownerWWW, wwwAfterGrace)
			assertEtcdOwnership(ctx, "vpn", ownerVPN, vpnAfterGrace)
		})

		By("keeps DNS during source deletion grace and resumes expiry after a labdns restart", func() {
			wwwExpected := []addressPair{{"192.0.2.111", "2001:db8:1::111"}, publicInitial[2]}
			vpnExpected := []addressPair{{"198.51.100.111", "2001:db8:2::111"}, vpnInitial[2]}
			createDeletionWorkload(ctx)
			assertRecordSetEventually(ctx, wwwDNS, deletionHost, dns.TypeA, []string{"192.0.2.111"}, normalTimeout)
			assertRecordSetEventually(ctx, wwwDNS, deletionHost, dns.TypeAAAA, nil, normalTimeout)
			assertRecordSetEventually(ctx, vpnDNS, deletionHost, dns.TypeA, nil, normalTimeout)
			assertSingleAddressOwnership(ctx, "www", deletionHost, ownerWWW, "192.0.2.111")

			// ExternalDNS v0.21.0's CoreDNS reader cannot preserve every etcd key
			// identity while combining a whole multi-target dual-stack name. The
			// separate single-target source keeps this restart check on a genuine
			// lifecycle event; the preceding cases cover exact shared RRset changes.
			ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "deletion-contract", Namespace: workNamespace}}
			deletedAt := time.Now()
			Expect(kubeClient.Delete(ctx, ingress)).To(Succeed())
			Eventually(func(g Gomega) {
				var current networkingv1.Ingress
				g.Expect(apierrors.IsNotFound(kubeClient.Get(ctx, types.NamespacedName{Namespace: workNamespace, Name: "deletion-contract"}, &current))).To(BeTrue())
			}, normalTimeout, 200*time.Millisecond).Should(Succeed())
			assertDeletionEndpointPending(ctx)
			assertRecordSetNow(ctx, wwwDNS, deletionHost, dns.TypeA, []string{"192.0.2.111"})

			restartDeployment(ctx, "labdns")
			Eventually(func(g Gomega) {
				var objects externaldnsv1alpha1.DNSEndpointList
				g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{
					"app.kubernetes.io/managed-by": "labdns",
					"labdns.shednet.dev/provider":  "www",
				})).To(Succeed())
				for i := range objects.Items {
					g.Expect(objects.Items[i].Annotations["labdns.shednet.dev/source-name"]).NotTo(Equal("deletion-contract"))
				}
			}, deletionDelay+normalTimeout, 200*time.Millisecond).Should(Succeed())
			remaining := time.Until(deletedAt.Add(deletionDelay + normalTimeout))
			Expect(remaining).To(BeNumerically(">", 0), "ExternalDNS exceeded deletion grace plus convergence allowance")
			assertRecordSetEventually(ctx, wwwDNS, deletionHost, dns.TypeA, nil, remaining)
			assertNoEtcdHostname(ctx, "www", deletionHost)
			assertDNSNow(ctx, wwwDNS, wwwExpected)
			assertDNSNow(ctx, vpnDNS, vpnExpected)
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})
	})
})

func requiredEnvironment(name string) string {
	value := os.Getenv(name)
	Expect(value).NotTo(BeEmpty(), name+" must identify this isolated invocation")
	return value
}

func runOutput(ctx context.Context, name string, args ...string) string {
	command := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	Expect(err).NotTo(HaveOccurred(), "%s %s failed:\n%s%s", name, strings.Join(args, " "), stdout.String(), stderr.String())
	return stdout.String()
}

func kubectlRun(ctx context.Context, args ...string) {
	runOutput(ctx, "kubectl", kubectlArgs(args...)...)
}

func kubectlOutput(ctx context.Context, args ...string) string {
	return runOutput(ctx, "kubectl", kubectlArgs(args...)...)
}

func kubectlArgs(args ...string) []string {
	return append([]string{"--kubeconfig", kubeconfig, "--context", kubeContext}, args...)
}

func setNodeAddresses(ctx context.Context, node *corev1.Node, public, vpn addressPair) {
	var current corev1.Node
	Expect(kubeClient.Get(ctx, types.NamespacedName{Name: node.Name}, &current)).To(Succeed())
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	current.Labels["networking.example.com/public-ipv4"] = public.v4
	current.Labels["networking.example.com/public-ipv6"] = encodeIPv6Label(public.v6)
	current.Labels["networking.example.com/vpn-ipv4"] = vpn.v4
	current.Labels["networking.example.com/vpn-ipv6"] = encodeIPv6Label(vpn.v6)
	Expect(kubeClient.Update(ctx, &current)).To(Succeed())
}

func encodeIPv6Label(value string) string {
	address := netip.MustParseAddr(value)
	Expect(address.Is6() && !address.Is4In6()).To(BeTrue(), "%q must be an IPv6 literal", value)
	return "v6-" + strings.ReplaceAll(address.String(), ":", "-")
}

func createWorkload(ctx context.Context) {
	Expect(kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workNamespace}})).To(Succeed())
	for name, addresses := range map[string][]string{
		"www": {"networking.example.com/public-ipv4", "networking.example.com/public-ipv6"},
		"vpn": {"networking.example.com/vpn-ipv4", "networking.example.com/vpn-ipv6"},
	} {
		provider := &labdnsv1alpha1.DNSProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: labdnsv1alpha1.DNSProviderSpec{
				Zones: []labdnsv1alpha1.DNSZone{{Name: "e2e.example.test"}},
				IPSources: labdnsv1alpha1.IPSources{
					IPv4: &labdnsv1alpha1.NodeLabelSource{NodeLabel: addresses[0]},
					IPv6: &labdnsv1alpha1.NodeLabelSource{NodeLabel: addresses[1]},
				},
				RecordDefaults: labdnsv1alpha1.RecordDefaults{TTL: 30, DeletionDelay: &metav1.Duration{Duration: deletionDelay}},
			},
		}
		Expect(kubeClient.Create(ctx, provider)).To(Succeed())
	}
	Expect(kubeClient.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "application", Namespace: workNamespace}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}}})).To(Succeed())
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "application", Namespace: workNamespace, Labels: map[string]string{discoveryv1.LabelServiceName: "application"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: pointer("http"), Port: pointer(int32(8080))}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.244.1.10"}, NodeName: &nodes[0].Name, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{Addresses: []string{"10.244.2.10"}, NodeName: &nodes[1].Name, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		},
	}
	Expect(kubeClient.Create(ctx, slice)).To(Succeed())
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "application", Namespace: workNamespace, Annotations: map[string]string{
			"labdns.shednet.dev/enabled":        "true",
			"labdns.shednet.dev/providers":      "www,vpn",
			"labdns.shednet.dev/deletion-delay": deletionDelay.String(),
		}},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: hostname, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "application", Port: networkingv1.ServiceBackendPort{Number: 8080}}}}}}}}}},
	}
	Expect(kubeClient.Create(ctx, ingress)).To(Succeed())
}

func createDeletionWorkload(ctx context.Context) {
	Expect(kubeClient.Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "deletion-contract", Namespace: workNamespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	})).To(Succeed())
	ready := true
	Expect(kubeClient.Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deletion-contract",
			Namespace: workNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "deletion-contract"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: pointer("http"), Port: pointer(int32(8080))}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.244.1.20"},
			NodeName:   &nodes[0].Name,
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	})).To(Succeed())
	pathType := networkingv1.PathTypePrefix
	Expect(kubeClient.Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deletion-contract",
			Namespace: workNamespace,
			Annotations: map[string]string{
				"labdns.shednet.dev/enabled":          "true",
				"labdns.shednet.dev/providers":        "www",
				"labdns.shednet.dev/address-families": "ipv4",
				"labdns.shednet.dev/deletion-delay":   deletionDelay.String(),
			},
		},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: deletionHost,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path:     "/",
				PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: "deletion-contract",
					Port: networkingv1.ServiceBackendPort{Number: 8080},
				}},
			}}}},
		}}},
	})).To(Succeed())
}

func pointer[T any](value T) *T { return &value }

func setEndpointPlacement(ctx context.Context, readyNode, unreadyNode, addedNode string) {
	var slice discoveryv1.EndpointSlice
	Expect(kubeClient.Get(ctx, types.NamespacedName{Namespace: workNamespace, Name: "application"}, &slice)).To(Succeed())
	ready, notReady := true, false
	slice.Endpoints = []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.1.10"}, NodeName: &readyNode, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		{Addresses: []string{"10.244.2.10"}, NodeName: &unreadyNode, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
		{Addresses: []string{"10.244.0.10"}, NodeName: &addedNode, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
	}
	Expect(kubeClient.Update(ctx, &slice)).To(Succeed())
}

func restartDeployment(ctx context.Context, name string) {
	var deployment appsv1.Deployment
	Expect(kubeClient.Get(ctx, types.NamespacedName{Namespace: systemNamespace, Name: name}, &deployment)).To(Succeed())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations["labdns.shednet.dev/e2e-restarted-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	Expect(kubeClient.Update(ctx, &deployment)).To(Succeed())
	Eventually(func(g Gomega) {
		var current appsv1.Deployment
		g.Expect(kubeClient.Get(ctx, types.NamespacedName{Namespace: systemNamespace, Name: name}, &current)).To(Succeed())
		desired := int32(1)
		if current.Spec.Replicas != nil {
			desired = *current.Spec.Replicas
		}
		g.Expect(current.Status.ObservedGeneration).To(BeNumerically(">=", current.Generation))
		g.Expect(current.Status.UpdatedReplicas).To(Equal(desired))
		g.Expect(current.Status.AvailableReplicas).To(Equal(desired))
		g.Expect(current.Status.Replicas).To(Equal(desired))
	}, 2*time.Minute, 250*time.Millisecond).Should(Succeed())
}

func expectedByType(pairs []addressPair) (map[uint16][]string, map[string]struct{}) {
	answers := map[uint16][]string{dns.TypeA: {}, dns.TypeAAAA: {}}
	targets := map[string]struct{}{}
	for _, pair := range pairs {
		for recordType, value := range map[uint16]string{dns.TypeA: pair.v4, dns.TypeAAAA: pair.v6} {
			address := netip.MustParseAddr(value).String()
			answers[recordType] = append(answers[recordType], address)
			targets[address] = struct{}{}
		}
	}
	for recordType := range answers {
		sort.Strings(answers[recordType])
	}
	return answers, targets
}

func assertDNSEventually(ctx context.Context, forward *dnsForward, pairs []addressPair, timeout time.Duration) {
	expected, _ := expectedByType(pairs)
	Eventually(func(g Gomega) {
		for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
			actual, err := forward.Query(ctx, hostname, recordType)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(actual).To(Equal(expected[recordType]))
		}
	}, timeout, 250*time.Millisecond).Should(Succeed())
}

func assertDNSNow(ctx context.Context, forward *dnsForward, pairs []addressPair) {
	expected, _ := expectedByType(pairs)
	for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		actual, err := forward.Query(ctx, hostname, recordType)
		Expect(err).NotTo(HaveOccurred())
		Expect(actual).To(Equal(expected[recordType]))
	}
}

func assertRecordSetEventually(ctx context.Context, forward *dnsForward, name string, recordType uint16, expected []string, timeout time.Duration) {
	want := normalizeAddresses(expected)
	Eventually(func(g Gomega) {
		actual, err := forward.Query(ctx, name, recordType)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(actual).To(Equal(want))
	}, timeout, 250*time.Millisecond).Should(Succeed())
}

func assertRecordSetNow(ctx context.Context, forward *dnsForward, name string, recordType uint16, expected []string) {
	actual, err := forward.Query(ctx, name, recordType)
	Expect(err).NotTo(HaveOccurred())
	Expect(actual).To(Equal(normalizeAddresses(expected)))
}

func normalizeAddresses(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParseAddr(value).String())
	}
	sort.Strings(result)
	return result
}

func assertDeletionEndpointPending(ctx context.Context) {
	Eventually(func(g Gomega) {
		var objects externaldnsv1alpha1.DNSEndpointList
		g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{
			"app.kubernetes.io/managed-by": "labdns",
			"labdns.shednet.dev/provider":  "www",
		})).To(Succeed())
		var matching []externaldnsv1alpha1.DNSEndpoint
		for i := range objects.Items {
			if objects.Items[i].Annotations["labdns.shednet.dev/source-name"] == "deletion-contract" {
				matching = append(matching, objects.Items[i])
			}
		}
		g.Expect(matching).To(HaveLen(1))
		g.Expect(matching[0].Spec.Endpoints).To(HaveLen(1))
		g.Expect(matching[0].Spec.Endpoints[0].DNSName).To(Equal(deletionHost))
		g.Expect(matching[0].Spec.Endpoints[0].RecordType).To(Equal(endpoint.RecordTypeA))
		g.Expect(matching[0].Spec.Endpoints[0].Targets).To(Equal(endpoint.Targets{"192.0.2.111"}))
		var state struct {
			Version int               `json:"version"`
			Pending []json.RawMessage `json:"pending"`
		}
		g.Expect(json.Unmarshal([]byte(matching[0].Annotations["labdns.shednet.dev/lifecycle"]), &state)).To(Succeed())
		g.Expect(state.Version).To(Equal(1))
		g.Expect(state.Pending).To(HaveLen(1))
	}, normalTimeout, 200*time.Millisecond).Should(Succeed())
}

func assertGeneratedEndpoints(ctx context.Context, www, vpn []addressPair, expectPending bool) {
	expected := map[string][]addressPair{"www": www, "vpn": vpn}
	Eventually(func(g Gomega) {
		var objects externaldnsv1alpha1.DNSEndpointList
		g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{"app.kubernetes.io/managed-by": "labdns"})).To(Succeed())
		g.Expect(objects.Items).To(HaveLen(2))
		for i := range objects.Items {
			object := &objects.Items[i]
			provider := object.Labels["labdns.shednet.dev/provider"]
			g.Expect(expected).To(HaveKey(provider))
			g.Expect(object.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "labdns"))
			g.Expect(object.Labels["labdns.shednet.dev/source-key"]).To(MatchRegexp(`^[0-9a-f]{16}$`))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-kind", "Ingress"))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-namespace", workNamespace))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-name", "application"))
			g.Expect(object.Annotations["labdns.shednet.dev/source-uid"]).NotTo(BeEmpty())
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/deletion-delay", deletionDelay.String()))
			expectedAnswers, _ := expectedByType(expected[provider])
			g.Expect(object.Spec.Endpoints).To(HaveLen(2))
			seenTypes := map[string]bool{}
			for _, record := range object.Spec.Endpoints {
				g.Expect(record.DNSName).To(Equal(hostname))
				g.Expect(record.RecordTTL).To(Equal(endpoint.TTL(30)))
				g.Expect(record.RecordType).To(Or(Equal(endpoint.RecordTypeA), Equal(endpoint.RecordTypeAAAA)))
				g.Expect(seenTypes[record.RecordType]).To(BeFalse(), "duplicate record type %s", record.RecordType)
				seenTypes[record.RecordType] = true
				actual := append([]string(nil), record.Targets...)
				for i := range actual {
					actual[i] = netip.MustParseAddr(actual[i]).String()
				}
				sort.Strings(actual)
				if record.RecordType == endpoint.RecordTypeA {
					g.Expect(actual).To(Equal(expectedAnswers[dns.TypeA]))
				} else {
					g.Expect(actual).To(Equal(expectedAnswers[dns.TypeAAAA]))
				}
			}
			g.Expect(seenTypes).To(Equal(map[string]bool{endpoint.RecordTypeA: true, endpoint.RecordTypeAAAA: true}))
			var state struct {
				Version int               `json:"version"`
				Pending []json.RawMessage `json:"pending"`
			}
			g.Expect(json.Unmarshal([]byte(object.Annotations["labdns.shednet.dev/lifecycle"]), &state)).To(Succeed())
			g.Expect(state.Version).To(Equal(1))
			if expectPending {
				g.Expect(state.Pending).NotTo(BeEmpty())
			} else {
				g.Expect(state.Pending).To(BeEmpty())
			}
		}
	}, normalTimeout, 200*time.Millisecond).Should(Succeed())
}

type etcdService struct {
	Key         string
	Raw         string
	Host        string `json:"host"`
	TTL         uint32 `json:"ttl"`
	TargetStrip int    `json:"targetstrip"`
	Owner       string `json:"owner"`
}

func etcdServices(ctx context.Context, provider string) []etcdService {
	pod := etcdPodName(ctx, provider)
	output := kubectlOutput(ctx, "exec", "--namespace", systemNamespace, pod, "--", "etcdctl", "--endpoints=http://127.0.0.1:2379", "get", "/skydns/", "--prefix", "--write-out=json")
	var response struct {
		KVs []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"kvs"`
	}
	Expect(json.Unmarshal([]byte(output), &response)).To(Succeed())
	result := make([]etcdService, 0, len(response.KVs))
	for _, pair := range response.KVs {
		key, err := base64.StdEncoding.DecodeString(pair.Key)
		Expect(err).NotTo(HaveOccurred())
		value, err := base64.StdEncoding.DecodeString(pair.Value)
		Expect(err).NotTo(HaveOccurred())
		service := etcdService{Key: string(key), Raw: string(value)}
		Expect(json.Unmarshal(value, &service)).To(Succeed())
		result = append(result, service)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func assertEtcdOwnership(ctx context.Context, provider, owner string, pairs []addressPair) {
	_, expected := expectedByType(pairs)
	Eventually(func(g Gomega) {
		services := etcdServices(ctx, provider)
		actual := map[string]struct{}{}
		for _, service := range services {
			if !strings.Contains(service.Key, "/test/example/e2e/shared/") {
				continue
			}
			g.Expect(service.Owner).To(Equal(owner))
			g.Expect(service.TTL).To(Equal(uint32(30)))
			g.Expect(service.TargetStrip).To(Equal(1))
			_, duplicate := actual[service.Host]
			g.Expect(duplicate).To(BeFalse(), "duplicate etcd host %s", service.Host)
			actual[service.Host] = struct{}{}
		}
		g.Expect(actual).To(Equal(expected))
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

func assertSingleAddressOwnership(ctx context.Context, provider, name, owner, address string) {
	prefix := etcdHostnamePrefix(name)
	Eventually(func(g Gomega) {
		var matching []etcdService
		for _, service := range etcdServices(ctx, provider) {
			if strings.HasPrefix(service.Key, prefix) {
				matching = append(matching, service)
			}
		}
		g.Expect(matching).To(HaveLen(1))
		g.Expect(matching[0].Host).To(Equal(netip.MustParseAddr(address).String()))
		g.Expect(matching[0].Owner).To(Equal(owner))
		g.Expect(matching[0].TTL).To(Equal(uint32(30)))
		g.Expect(matching[0].TargetStrip).To(Equal(1))
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

func assertNoEtcdHostname(ctx context.Context, provider, name string) {
	prefix := etcdHostnamePrefix(name)
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	labels[0] = "a-" + labels[0]
	txtPrefix := etcdHostnamePrefix(strings.Join(labels, "."))
	Eventually(func(g Gomega) {
		for _, service := range etcdServices(ctx, provider) {
			g.Expect(service.Key).NotTo(Or(HavePrefix(prefix), HavePrefix(txtPrefix)))
		}
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

func etcdHostnamePrefix(name string) string {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for left, right := 0, len(labels)-1; left < right; left, right = left+1, right-1 {
		labels[left], labels[right] = labels[right], labels[left]
	}
	return "/skydns/" + strings.Join(labels, "/") + "/"
}

func putForeignRecord(ctx context.Context, provider string) {
	pod := etcdPodName(ctx, provider)
	kubectlRun(ctx, "exec", "--namespace", systemNamespace, pod, "--", "etcdctl", "--endpoints=http://127.0.0.1:2379", "put", "/skydns/test/example/e2e/foreign/static", foreignRaw)
}

func etcdPodName(ctx context.Context, provider string) string {
	var pods corev1.PodList
	Expect(kubeClient.List(ctx, &pods, client.InNamespace(systemNamespace), client.MatchingLabels{"app": "etcd-" + provider})).To(Succeed())
	Expect(pods.Items).To(HaveLen(1))
	return pods.Items[0].Name
}

func assertForeignRecord(ctx context.Context, provider string) {
	Eventually(func(g Gomega) {
		var matching []etcdService
		for _, service := range etcdServices(ctx, provider) {
			if service.Key == "/skydns/test/example/e2e/foreign/static" {
				matching = append(matching, service)
			}
		}
		g.Expect(matching).To(Equal([]etcdService{{Key: "/skydns/test/example/e2e/foreign/static", Raw: foreignRaw, Host: "203.0.113.250", TTL: 123, Owner: "foreign-owner"}}))
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

type dnsForward struct {
	address string
	command *exec.Cmd
}

func startDNSForward(ctx context.Context, service string) *dnsForward {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())
	command := exec.Command("kubectl", kubectlArgs("port-forward", "--namespace", systemNamespace, "service/"+service, fmt.Sprintf("%d:53", port))...)
	command.Stdout = GinkgoWriter
	command.Stderr = GinkgoWriter
	Expect(command.Start()).To(Succeed())
	forward := &dnsForward{address: fmt.Sprintf("127.0.0.1:%d", port), command: command}
	Eventually(func() error {
		connection, dialErr := net.DialTimeout("tcp", forward.address, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
		}
		return dialErr
	}, normalTimeout, 100*time.Millisecond).WithContext(ctx).Should(Succeed())
	return forward
}

func (forward *dnsForward) Query(ctx context.Context, name string, recordType uint16) ([]string, error) {
	message := new(dns.Msg)
	message.SetQuestion(dns.Fqdn(name), recordType)
	response, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(ctx, message, forward.address)
	if err != nil {
		return nil, err
	}
	if response.Rcode == dns.RcodeNameError {
		return []string{}, nil
	}
	if response.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("DNS rcode %s", dns.RcodeToString[response.Rcode])
	}
	result := []string{}
	for _, answer := range response.Answer {
		switch record := answer.(type) {
		case *dns.A:
			if recordType == dns.TypeA {
				result = append(result, netip.MustParseAddr(record.A.String()).String())
			}
		case *dns.AAAA:
			if recordType == dns.TypeAAAA {
				result = append(result, netip.MustParseAddr(record.AAAA.String()).String())
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func (forward *dnsForward) Stop() {
	if forward.command.Process == nil {
		return
	}
	_ = forward.command.Process.Kill()
	_ = forward.command.Wait()
}
