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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

const (
	systemNamespace = "labdns-e2e-system"
	workNamespace   = "labdns-e2e-work"
	gatewayClass    = "labdns-e2e"
	deletionDelay   = 15 * time.Second
	normalTimeout   = 10 * time.Second
	ownerWWW        = "labdns-e2e-www"
	ownerVPN        = "labdns-e2e-vpn"
	foreignRaw      = `{"host":"203.0.113.250","ttl":123,"owner":"foreign-owner"}`
	// Leave room for the deployment's readiness probes before observing
	// retained targets after restart; normal publication keeps its 10s budget.
	restartDeletionDelay = 30 * time.Second
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
	sharedSources = []publicationSource{
		{kind: "Ingress", name: "application", hostname: "ingress.e2e.example.test", deletionDelay: deletionDelay},
		{kind: "HTTPRoute", name: "application", hostname: "http.e2e.example.test", deletionDelay: deletionDelay},
		{kind: "TCPRoute", name: "application", hostname: "tcp.e2e.example.test", deletionDelay: deletionDelay},
		{kind: "UDPRoute", name: "application", hostname: "udp.e2e.example.test", deletionDelay: deletionDelay},
	}
)

type publicationSource struct {
	kind          string
	name          string
	hostname      string
	deletionDelay time.Duration
}

type endpointExpectation struct {
	source         publicationSource
	provider       string
	a              []string
	aaaa           []string
	pending        bool
	pendingTargets []pendingTargetExpectation
}

type dnsExpectation struct {
	forward    *dnsForward
	hostname   string
	recordType uint16
	addresses  []string
}

type pendingTargetExpectation struct {
	dnsName    string
	recordType string
	target     string
	notBefore  time.Time
}

type lifecycleState struct {
	Version int                    `json:"version"`
	Pending []lifecyclePendingItem `json:"pending"`
}

type lifecyclePendingItem struct {
	DNSName    string `json:"dnsName"`
	RecordType string `json:"recordType"`
	Target     string `json:"target"`
	Deadline   string `json:"deadline"`
}

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
	Expect(gatewayv1.Install(scheme)).To(Succeed())
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
		updatedWWW := addressPair{"192.0.2.111", "2001:db8:1::111"}
		updatedVPN := addressPair{"198.51.100.111", "2001:db8:2::111"}

		By("publishing exact provider-isolated multi-node A and AAAA sets", func() {
			createWorkload(ctx)

			wwwExpected := []addressPair{publicInitial[0], publicInitial[1]}
			vpnExpected := []addressPair{vpnInitial[0], vpnInitial[1]}
			assertDNSStageEventually(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected), normalTimeout)
			assertGeneratedEndpoints(ctx, dualStackEndpointExpectations(sharedSources, wwwExpected, vpnExpected, false))
			assertEtcdOwnership(ctx, "www", ownerWWW, sharedSources, wwwExpected)
			assertEtcdOwnership(ctx, "vpn", ownerVPN, sharedSources, vpnExpected)

			putForeignRecord(ctx, "www")
			putForeignRecord(ctx, "vpn")
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})

		By("converging all source kinds after ExternalDNS restarts and Node labels change", func() {
			restartDeployment(ctx, "external-dns-www")
			restartDeployment(ctx, "external-dns-vpn")

			nodeChangeAt := time.Now()
			setNodeAddresses(ctx, &nodes[0], updatedWWW, updatedVPN)
			wwwDuringGrace := []addressPair{publicInitial[0], updatedWWW, publicInitial[1]}
			vpnDuringGrace := []addressPair{vpnInitial[0], updatedVPN, vpnInitial[1]}
			assertDNSStageEventually(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwDuringGrace, vpnDNS, vpnDuringGrace), normalTimeout)
			assertGeneratedEndpoints(ctx, dualStackEndpointExpectations(sharedSources, wwwDuringGrace, vpnDuringGrace, true))

			wwwExpected := []addressPair{updatedWWW, publicInitial[1]}
			vpnExpected := []addressPair{updatedVPN, vpnInitial[1]}
			nodeRemovalRemaining := time.Until(nodeChangeAt.Add(deletionDelay + normalTimeout))
			Expect(nodeRemovalRemaining).To(BeNumerically(">", 0), "node-label removal exceeded its shared grace plus convergence deadline")
			assertDNSStageEventually(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected), nodeRemovalRemaining)
			assertGeneratedEndpointsBefore(ctx, dualStackEndpointExpectations(sharedSources, wwwExpected, vpnExpected, false), nodeChangeAt.Add(deletionDelay+normalTimeout))
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})

		By("retains only the removed target until its per-target grace expires", func() {
			placementChangeAt := time.Now()
			setEndpointPlacement(ctx, nodes[0].Name, nodes[1].Name, nodes[2].Name)
			updatedWWW := addressPair{"192.0.2.111", "2001:db8:1::111"}
			updatedVPN := addressPair{"198.51.100.111", "2001:db8:2::111"}
			wwwDuringGrace := []addressPair{updatedWWW, publicInitial[1], publicInitial[2]}
			vpnDuringGrace := []addressPair{updatedVPN, vpnInitial[1], vpnInitial[2]}
			assertDNSStageEventually(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwDuringGrace, vpnDNS, vpnDuringGrace), normalTimeout)
			assertGeneratedEndpoints(ctx, dualStackEndpointExpectations(sharedSources, wwwDuringGrace, vpnDuringGrace, true))

			wwwAfterGrace := []addressPair{updatedWWW, publicInitial[2]}
			vpnAfterGrace := []addressPair{updatedVPN, vpnInitial[2]}
			placementRemovalRemaining := time.Until(placementChangeAt.Add(deletionDelay + normalTimeout))
			Expect(placementRemovalRemaining).To(BeNumerically(">", 0), "EndpointSlice removal exceeded its shared grace plus convergence deadline")
			assertDNSStageEventually(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwAfterGrace, vpnDNS, vpnAfterGrace), placementRemovalRemaining)
			assertGeneratedEndpointsBefore(ctx, dualStackEndpointExpectations(sharedSources, wwwAfterGrace, vpnAfterGrace, false), placementChangeAt.Add(deletionDelay+normalTimeout))
			assertEtcdOwnership(ctx, "www", ownerWWW, sharedSources, wwwAfterGrace)
			assertEtcdOwnership(ctx, "vpn", ownerVPN, sharedSources, vpnAfterGrace)
		})

		By("retiring one single-address IPv4 source of each kind across one labdns restart", func() {
			wwwExpected := []addressPair{updatedWWW, publicInitial[2]}
			vpnExpected := []addressPair{updatedVPN, vpnInitial[2]}
			deletionSources := createDeletionWorkload(ctx)
			deletionAddresses := map[string][]string{}
			for _, source := range deletionSources {
				deletionAddresses[source.hostname] = []string{updatedWWW.v4}
			}
			stage := dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected)
			stage = append(stage, recordExpectations(wwwDNS, deletionAddresses, dns.TypeA)...)
			stage = append(stage, emptyDNSExpectationsByType(wwwDNS, deletionSources, dns.TypeAAAA)...)
			stage = append(stage, emptyDNSExpectations(vpnDNS, deletionSources)...)
			assertDNSStageEventually(ctx, stage, normalTimeout)
			active := append(dualStackEndpointExpectations(sharedSources, wwwExpected, vpnExpected, false), ipv4EndpointExpectations(deletionSources, "www", updatedWWW.v4, false)...)
			assertGeneratedEndpoints(ctx, active)
			assertEtcdHostTargets(ctx, "www", ownerWWW, deletionAddresses)
			assertEtcdInventoryEventually(ctx, "www", appendSources(sharedSources, deletionSources))

			deletedAt := time.Now()
			deletePublicationSources(ctx, deletionSources)
			minimumDeadline := deletedAt.Add(restartDeletionDelay)
			deletionPending := ipv4EndpointExpectations(deletionSources, "www", updatedWWW.v4, true)
			for i := range deletionPending {
				deletionPending[i].pendingTargets = []pendingTargetExpectation{{
					dnsName: deletionPending[i].source.hostname, recordType: endpoint.RecordTypeA,
					target: updatedWWW.v4, notBefore: minimumDeadline,
				}}
			}
			pending := append(dualStackEndpointExpectations(sharedSources, wwwExpected, vpnExpected, false), deletionPending...)
			assertGeneratedEndpoints(ctx, pending)
			pendingDeadlines := readPendingDeadlines(ctx, deletionSources, "www")
			graceDNS := recordExpectations(wwwDNS, deletionAddresses, dns.TypeA)
			graceDNS = append(graceDNS, emptyDNSExpectationsByType(wwwDNS, deletionSources, dns.TypeAAAA)...)
			assertRecordSetsNow(ctx, graceDNS)
			assertEtcdHostTargets(ctx, "www", ownerWWW, deletionAddresses)
			assertEtcdInventoryEventually(ctx, "www", appendSources(sharedSources, deletionSources))

			assertPendingDeadlineSet(pendingDeadlines, deletionSources, "www", minimumDeadline)
			Expect(time.Until(earliestPendingDeadline(pendingDeadlines))).To(BeNumerically(">", 0), "labdns restart must start before pending targets expire")
			restartDeployment(ctx, "labdns")
			deadline := deletedAt.Add(restartDeletionDelay + normalTimeout)
			assertRetainedThroughPendingDeadlines(ctx, deletionSources, wwwDNS, "www", ownerWWW, updatedWWW.v4, pendingDeadlines)
			remaining := time.Until(deadline)
			Expect(remaining).To(BeNumerically(">", 0), "labdns restart and persisted deadlines exceeded deletion grace plus convergence allowance")
			assertSourcesRetiredEventually(ctx, deletionSources, wwwDNS, "www", remaining)
			assertDNSStageNow(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected))
			assertForeignRecord(ctx, "www")
			assertForeignRecord(ctx, "vpn")
		})

		By("deleting all four source kinds immediately when their grace is zero", func() {
			wwwExpected := []addressPair{updatedWWW, publicInitial[2]}
			vpnExpected := []addressPair{updatedVPN, vpnInitial[2]}
			immediateSources := createImmediateWorkload(ctx)
			immediateAddresses := map[string][]string{}
			for _, source := range immediateSources {
				immediateAddresses[source.hostname] = []string{updatedWWW.v4}
			}
			stage := dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected)
			stage = append(stage, recordExpectations(wwwDNS, immediateAddresses, dns.TypeA)...)
			stage = append(stage, emptyDNSExpectationsByType(wwwDNS, immediateSources, dns.TypeAAAA)...)
			stage = append(stage, emptyDNSExpectations(vpnDNS, immediateSources)...)
			assertDNSStageEventually(ctx, stage, normalTimeout)
			active := dualStackEndpointExpectations(sharedSources, wwwExpected, vpnExpected, false)
			active = append(active, ipv4EndpointExpectations(immediateSources, "www", updatedWWW.v4, false)...)
			assertGeneratedEndpoints(ctx, active)
			assertEtcdHostTargets(ctx, "www", ownerWWW, immediateAddresses)
			assertEtcdInventoryEventually(ctx, "www", appendSources(sharedSources, immediateSources))

			deletePublicationSources(ctx, immediateSources)
			assertSourcesRetiredEventually(ctx, immediateSources, wwwDNS, "www", normalTimeout)
			assertDNSStageNow(ctx, dualStackDNSExpectations(sharedSources, wwwDNS, wwwExpected, vpnDNS, vpnExpected))
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
	createGatewayHierarchy(ctx)
	createIngressSource(ctx, sharedSources[0], "application", map[string]string{
		"labdns.shednet.dev/enabled":        "true",
		"labdns.shednet.dev/providers":      "www,vpn",
		"labdns.shednet.dev/deletion-delay": deletionDelay.String(),
	})
	for i := 1; i < len(sharedSources); i++ {
		source := sharedSources[i]
		hostname := ""
		if source.kind == "HTTPRoute" {
			hostname = source.hostname
		}
		createGatewayRoute(ctx, source, "application", hostname, nil, false)
	}
}

func createGatewayHierarchy(ctx context.Context) {
	Expect(kubeClient.Create(ctx, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayClass, Annotations: map[string]string{
			"labdns.shednet.dev/enabled":        "true",
			"labdns.shednet.dev/providers":      "www,vpn",
			"labdns.shednet.dev/deletion-delay": deletionDelay.String(),
		}},
		Spec: gatewayv1.GatewayClassSpec{ControllerName: "e2e.example.test/controller"},
	})).To(Succeed())
	listeners := map[string]struct {
		protocol gatewayv1.ProtocolType
		port     gatewayv1.PortNumber
	}{
		"HTTPRoute": {protocol: gatewayv1.HTTPProtocolType, port: 80},
		"TCPRoute":  {protocol: gatewayv1.TCPProtocolType, port: 443},
		"UDPRoute":  {protocol: gatewayv1.UDPProtocolType, port: 53},
	}
	for kind, listener := range listeners {
		annotations := map[string]string{}
		if kind == "TCPRoute" || kind == "UDPRoute" {
			annotations["labdns.shednet.dev/hostnames"] = sourceForKind(sharedSources, kind).hostname
		}
		Expect(kubeClient.Create(ctx, &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayName(kind), Namespace: workNamespace, Annotations: annotations},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayv1.ObjectName(gatewayClass),
				Listeners: []gatewayv1.Listener{{
					Name:     "listener",
					Protocol: listener.protocol,
					Port:     listener.port,
				}},
			},
		})).To(Succeed())
	}
}

func createDeletionWorkload(ctx context.Context) []publicationSource {
	// ExternalDNS v0.21.0's CoreDNS reader cannot preserve every etcd key
	// identity while combining a whole multi-target dual-stack name. Keep
	// restart recovery on single-address sources; shared sources cover exact
	// dual-stack target changes throughout the rest of the contract.
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
	sources := namedSources("deletion", restartDeletionDelay)
	for _, source := range sources {
		annotations := map[string]string{
			"labdns.shednet.dev/providers":        "www",
			"labdns.shednet.dev/address-families": "ipv4",
			"labdns.shednet.dev/deletion-delay":   source.deletionDelay.String(),
		}
		if source.kind == "Ingress" {
			annotations["labdns.shednet.dev/enabled"] = "true"
			createIngressSource(ctx, source, "deletion-contract", annotations)
			continue
		}
		hostname := source.hostname
		createGatewayRoute(ctx, source, "deletion-contract", hostname, annotations, true)
	}
	return sources
}

func createImmediateWorkload(ctx context.Context) []publicationSource {
	// Reuse the single-address lifecycle backend for the same ExternalDNS
	// limitation described in createDeletionWorkload (labdns-next-vj2).
	sources := namedSources("immediate", 0)
	for _, source := range sources {
		annotations := map[string]string{
			"labdns.shednet.dev/providers":        "www",
			"labdns.shednet.dev/address-families": "ipv4",
			"labdns.shednet.dev/deletion-delay":   "0s",
		}
		if source.kind == "Ingress" {
			annotations["labdns.shednet.dev/enabled"] = "true"
			createIngressSource(ctx, source, "deletion-contract", annotations)
			continue
		}
		createGatewayRoute(ctx, source, "deletion-contract", source.hostname, annotations, true)
	}
	return sources
}

func namedSources(prefix string, delay time.Duration) []publicationSource {
	return []publicationSource{
		{kind: "Ingress", name: prefix + "-ingress", hostname: prefix + "-ingress.e2e.example.test", deletionDelay: delay},
		{kind: "HTTPRoute", name: prefix + "-http", hostname: prefix + "-http.e2e.example.test", deletionDelay: delay},
		{kind: "TCPRoute", name: prefix + "-tcp", hostname: prefix + "-tcp.e2e.example.test", deletionDelay: delay},
		{kind: "UDPRoute", name: prefix + "-udp", hostname: prefix + "-udp.e2e.example.test", deletionDelay: delay},
	}
}

func createIngressSource(ctx context.Context, source publicationSource, backend string, annotations map[string]string) {
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: source.name, Namespace: workNamespace, Annotations: annotations},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: source.hostname,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path:     "/",
				PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: backend,
					Port: networkingv1.ServiceBackendPort{Number: 8080},
				}},
			}}}},
		}}},
	}
	Expect(kubeClient.Create(ctx, ingress)).To(Succeed())
}

func createGatewayRoute(ctx context.Context, source publicationSource, backend, specHostname string, annotations map[string]string, routeHostnameOverride bool) {
	attrs := map[string]string{}
	for key, value := range annotations {
		attrs[key] = value
	}
	if routeHostnameOverride && (source.kind == "TCPRoute" || source.kind == "UDPRoute") {
		attrs["labdns.shednet.dev/hostnames"] = source.hostname
	}
	port := gatewayv1.PortNumber(8080)
	backendReference := gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(backend), Port: &port}
	common := gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(gatewayName(source.kind))}}}
	metadata := metav1.ObjectMeta{Name: source.name, Namespace: workNamespace, Annotations: attrs}
	switch source.kind {
	case "HTTPRoute":
		Expect(kubeClient.Create(ctx, &gatewayv1.HTTPRoute{
			ObjectMeta: metadata,
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: common,
				Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(specHostname)},
				Rules: []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{BackendObjectReference: backendReference},
				}}}},
			},
		})).To(Succeed())
	case "TCPRoute":
		Expect(kubeClient.Create(ctx, &gatewayv1.TCPRoute{
			ObjectMeta: metadata,
			Spec: gatewayv1.TCPRouteSpec{
				CommonRouteSpec: common,
				Rules: []gatewayv1.TCPRouteRule{{BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: backendReference,
				}}}},
			},
		})).To(Succeed())
	case "UDPRoute":
		Expect(kubeClient.Create(ctx, &gatewayv1.UDPRoute{
			ObjectMeta: metadata,
			Spec: gatewayv1.UDPRouteSpec{
				CommonRouteSpec: common,
				Rules: []gatewayv1.UDPRouteRule{{BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: backendReference,
				}}}},
			},
		})).To(Succeed())
	default:
		Fail("unsupported Gateway route kind: " + source.kind)
	}
}

func deletePublicationSources(ctx context.Context, sources []publicationSource) {
	for _, source := range sources {
		var object client.Object
		switch source.kind {
		case "Ingress":
			object = &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: source.name, Namespace: workNamespace}}
		case "HTTPRoute":
			object = &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: source.name, Namespace: workNamespace}}
		case "TCPRoute":
			object = &gatewayv1.TCPRoute{ObjectMeta: metav1.ObjectMeta{Name: source.name, Namespace: workNamespace}}
		case "UDPRoute":
			object = &gatewayv1.UDPRoute{ObjectMeta: metav1.ObjectMeta{Name: source.name, Namespace: workNamespace}}
		default:
			Fail("unsupported publication source kind: " + source.kind)
		}
		Expect(kubeClient.Delete(ctx, object)).To(Succeed())
	}
}

func sourceForKind(sources []publicationSource, kind string) publicationSource {
	for _, source := range sources {
		if source.kind == kind {
			return source
		}
	}
	Fail("missing source kind " + kind)
	return publicationSource{}
}

func gatewayName(kind string) string {
	return "gateway-" + strings.TrimSuffix(strings.ToLower(kind), "route")
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

func dualStackDNSExpectations(sources []publicationSource, wwwForward *dnsForward, www []addressPair, vpnForward *dnsForward, vpn []addressPair) []dnsExpectation {
	expectations := pairDNSExpectations(wwwForward, sources, www)
	return append(expectations, pairDNSExpectations(vpnForward, sources, vpn)...)
}

func pairDNSExpectations(forward *dnsForward, sources []publicationSource, pairs []addressPair) []dnsExpectation {
	answers, _ := expectedByType(pairs)
	expectations := make([]dnsExpectation, 0, len(sources)*2)
	for _, source := range sources {
		for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
			expectations = append(expectations, dnsExpectation{
				forward: forward, hostname: source.hostname, recordType: recordType, addresses: answers[recordType],
			})
		}
	}
	return expectations
}

func recordExpectations(forward *dnsForward, hosts map[string][]string, recordType uint16) []dnsExpectation {
	hostnames := make([]string, 0, len(hosts))
	for hostname := range hosts {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)
	expectations := make([]dnsExpectation, 0, len(hostnames))
	for _, hostname := range hostnames {
		expectations = append(expectations, dnsExpectation{forward: forward, hostname: hostname, recordType: recordType, addresses: hosts[hostname]})
	}
	return expectations
}

func emptyDNSExpectations(forward *dnsForward, sources []publicationSource) []dnsExpectation {
	expectations := make([]dnsExpectation, 0, len(sources)*2)
	for _, source := range sources {
		for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
			expectations = append(expectations, dnsExpectation{forward: forward, hostname: source.hostname, recordType: recordType})
		}
	}
	return expectations
}

func emptyDNSExpectationsByType(forward *dnsForward, sources []publicationSource, recordType uint16) []dnsExpectation {
	expectations := make([]dnsExpectation, 0, len(sources))
	for _, source := range sources {
		expectations = append(expectations, dnsExpectation{forward: forward, hostname: source.hostname, recordType: recordType})
	}
	return expectations
}

func assertDNSStageEventually(ctx context.Context, expectations []dnsExpectation, timeout time.Duration) {
	Eventually(func(g Gomega) {
		assertDNSStage(g, ctx, expectations)
	}, timeout, 250*time.Millisecond).Should(Succeed())
}

func assertDNSStageNow(ctx context.Context, expectations []dnsExpectation) {
	for _, expectation := range expectations {
		actual, err := expectation.forward.Query(ctx, expectation.hostname, expectation.recordType)
		Expect(err).NotTo(HaveOccurred(), "%s %s query", expectation.hostname, dns.TypeToString[expectation.recordType])
		Expect(actual).To(Equal(normalizeAddresses(expectation.addresses)), "%s %s answers", expectation.hostname, dns.TypeToString[expectation.recordType])
	}
}

func assertDNSStage(g Gomega, ctx context.Context, expectations []dnsExpectation) {
	for _, expectation := range expectations {
		actual, err := expectation.forward.Query(ctx, expectation.hostname, expectation.recordType)
		g.Expect(err).NotTo(HaveOccurred(), "%s %s query", expectation.hostname, dns.TypeToString[expectation.recordType])
		g.Expect(actual).To(Equal(normalizeAddresses(expectation.addresses)), "%s %s answers", expectation.hostname, dns.TypeToString[expectation.recordType])
	}
}

func assertRecordSetsNow(ctx context.Context, expectations []dnsExpectation) {
	assertDNSStageNow(ctx, expectations)
}

func normalizeAddresses(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParseAddr(value).String())
	}
	sort.Strings(result)
	return result
}

func dualStackEndpointExpectations(sources []publicationSource, www, vpn []addressPair, pending bool) []endpointExpectation {
	expectations := pairEndpointExpectations(sources, "www", www, pending)
	return append(expectations, pairEndpointExpectations(sources, "vpn", vpn, pending)...)
}

func pairEndpointExpectations(sources []publicationSource, provider string, pairs []addressPair, pending bool) []endpointExpectation {
	expectations := make([]endpointExpectation, 0, len(sources))
	for _, source := range sources {
		expectations = append(expectations, endpointExpectation{source: source, provider: provider, a: addressFamilyTargets(pairs, dns.TypeA), aaaa: addressFamilyTargets(pairs, dns.TypeAAAA), pending: pending})
	}
	return expectations
}

func ipv4EndpointExpectations(sources []publicationSource, provider, address string, pending bool) []endpointExpectation {
	expectations := make([]endpointExpectation, 0, len(sources))
	for _, source := range sources {
		expectations = append(expectations, endpointExpectation{source: source, provider: provider, a: []string{address}, pending: pending})
	}
	return expectations
}

func addressFamilyTargets(pairs []addressPair, recordType uint16) []string {
	answers, _ := expectedByType(pairs)
	return answers[recordType]
}

func assertGeneratedEndpoints(ctx context.Context, expected []endpointExpectation) {
	assertGeneratedEndpointsBefore(ctx, expected, time.Now().Add(normalTimeout))
}

func assertGeneratedEndpointsBefore(ctx context.Context, expected []endpointExpectation, deadline time.Time) {
	remaining := time.Until(deadline)
	Expect(remaining).To(BeNumerically(">", 0), "DNSEndpoint convergence exceeded the stage deadline")
	wanted := make(map[string]endpointExpectation, len(expected))
	for _, item := range expected {
		key := endpointIdentity(item.provider, item.source.kind, item.source.name)
		Expect(wanted).NotTo(HaveKey(key), "duplicate expected source output %s", key)
		wanted[key] = item
	}
	Eventually(func(g Gomega) {
		var objects externaldnsv1alpha1.DNSEndpointList
		g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{"app.kubernetes.io/managed-by": "labdns"})).To(Succeed())
		g.Expect(objects.Items).To(HaveLen(len(wanted)), "managed DNSEndpoints must exactly match expected provider and source identities")
		seen := make(map[string]bool, len(wanted))
		for i := range objects.Items {
			object := &objects.Items[i]
			provider := object.Labels["labdns.shednet.dev/provider"]
			kind := object.Annotations["labdns.shednet.dev/source-kind"]
			name := object.Annotations["labdns.shednet.dev/source-name"]
			key := endpointIdentity(provider, kind, name)
			expectation, exists := wanted[key]
			g.Expect(exists).To(BeTrue(), "unexpected DNSEndpoint source identity provider=%s kind=%s name=%s", provider, kind, name)
			if !exists {
				continue
			}
			g.Expect(seen[key]).To(BeFalse(), "duplicate DNSEndpoint identity %s", key)
			seen[key] = true
			g.Expect(object.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "labdns"))
			g.Expect(object.Labels["labdns.shednet.dev/source-key"]).To(MatchRegexp(`^[0-9a-f]{16}$`))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-kind", expectation.source.kind))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-namespace", workNamespace))
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/source-name", expectation.source.name))
			g.Expect(object.Annotations["labdns.shednet.dev/source-uid"]).NotTo(BeEmpty())
			g.Expect(object.Annotations).To(HaveKeyWithValue("labdns.shednet.dev/deletion-delay", expectation.source.deletionDelay.String()))
			expectedRecords := map[string][]string{}
			if len(expectation.a) != 0 {
				expectedRecords[endpoint.RecordTypeA] = normalizeAddresses(expectation.a)
			}
			if len(expectation.aaaa) != 0 {
				expectedRecords[endpoint.RecordTypeAAAA] = normalizeAddresses(expectation.aaaa)
			}
			g.Expect(object.Spec.Endpoints).To(HaveLen(len(expectedRecords)))
			seenTypes := map[string]bool{}
			for _, record := range object.Spec.Endpoints {
				g.Expect(record.DNSName).To(Equal(expectation.source.hostname))
				g.Expect(record.RecordTTL).To(Equal(endpoint.TTL(30)))
				wantTargets, supported := expectedRecords[record.RecordType]
				g.Expect(supported).To(BeTrue(), "unexpected record type %s", record.RecordType)
				g.Expect(seenTypes[record.RecordType]).To(BeFalse(), "duplicate record type %s", record.RecordType)
				seenTypes[record.RecordType] = true
				actual := append([]string(nil), record.Targets...)
				for i := range actual {
					actual[i] = netip.MustParseAddr(actual[i]).String()
				}
				sort.Strings(actual)
				g.Expect(actual).To(Equal(wantTargets))
			}
			g.Expect(seenTypes).To(HaveLen(len(expectedRecords)))
			var state lifecycleState
			g.Expect(json.Unmarshal([]byte(object.Annotations["labdns.shednet.dev/lifecycle"]), &state)).To(Succeed())
			g.Expect(state.Version).To(Equal(1))
			if expectation.pending {
				g.Expect(state.Pending).NotTo(BeEmpty())
			} else {
				g.Expect(state.Pending).To(BeEmpty())
			}
			if len(expectation.pendingTargets) != 0 {
				g.Expect(state.Pending).To(HaveLen(len(expectation.pendingTargets)))
				for _, want := range expectation.pendingTargets {
					var matched *lifecyclePendingItem
					for j := range state.Pending {
						item := &state.Pending[j]
						if item.DNSName == want.dnsName && item.RecordType == want.recordType && item.Target == want.target {
							matched = item
							break
						}
					}
					g.Expect(matched).NotTo(BeNil(), "pending target %s/%s/%s", want.dnsName, want.recordType, want.target)
					if matched == nil {
						continue
					}
					deadline, err := time.Parse(time.RFC3339Nano, matched.Deadline)
					g.Expect(err).NotTo(HaveOccurred(), "pending deadline %q", matched.Deadline)
					if err == nil {
						g.Expect(deadline.Before(want.notBefore)).To(BeFalse(), "pending deadline for %s precedes deletion grace", want.dnsName)
					}
				}
			}
		}
		g.Expect(seen).To(HaveLen(len(wanted)))
	}, remaining, 200*time.Millisecond).Should(Succeed())
}

func endpointIdentity(provider, kind, name string) string {
	return provider + "\x00" + kind + "\x00" + name
}

func readPendingDeadlines(ctx context.Context, sources []publicationSource, provider string) map[string]time.Time {
	var objects externaldnsv1alpha1.DNSEndpointList
	Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "labdns",
		"labdns.shednet.dev/provider":  provider,
	})).To(Succeed())
	deadlines := make(map[string]time.Time, len(sources))
	for i := range objects.Items {
		object := &objects.Items[i]
		key := endpointIdentity(provider, object.Annotations["labdns.shednet.dev/source-kind"], object.Annotations["labdns.shednet.dev/source-name"])
		for _, source := range sources {
			if key != endpointIdentity(provider, source.kind, source.name) {
				continue
			}
			var state lifecycleState
			Expect(json.Unmarshal([]byte(object.Annotations["labdns.shednet.dev/lifecycle"]), &state)).To(Succeed())
			Expect(state.Pending).To(HaveLen(1), "exactly one IPv4 target should be pending for %s", source.kind)
			deadline, err := time.Parse(time.RFC3339Nano, state.Pending[0].Deadline)
			Expect(err).NotTo(HaveOccurred())
			deadlines[key] = deadline
		}
	}
	Expect(deadlines).To(HaveLen(len(sources)), "every deleted source kind must persist a deadline")
	return deadlines
}

func assertPendingDeadlineSet(deadlines map[string]time.Time, sources []publicationSource, provider string, minimum time.Time) {
	Expect(deadlines).To(HaveLen(len(sources)))
	for _, source := range sources {
		deadline, found := deadlines[endpointIdentity(provider, source.kind, source.name)]
		Expect(found).To(BeTrue(), "missing persisted deadline for %s/%s", source.kind, source.name)
		if found {
			Expect(deadline.Before(minimum)).To(BeFalse(), "persisted deadline for %s/%s is shorter than the configured grace", source.kind, source.name)
		}
	}
}

func assertRetainedThroughPendingDeadlines(ctx context.Context, sources []publicationSource, forward *dnsForward, provider, owner, target string, deadlines map[string]time.Time) {
	var latest time.Time
	for _, source := range sources {
		deadline, found := deadlines[endpointIdentity(provider, source.kind, source.name)]
		Expect(found).To(BeTrue(), "missing persisted deadline for %s/%s", source.kind, source.name)
		if found && deadline.After(latest) {
			latest = deadline
		}
	}
	remaining := time.Until(latest)
	Expect(remaining).To(BeNumerically(">", 0), "labdns restart must finish while pending targets remain")
	checked := make(map[string]bool, len(sources))
	Consistently(func(g Gomega) {
		var objects externaldnsv1alpha1.DNSEndpointList
		g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{
			"app.kubernetes.io/managed-by": "labdns",
			"labdns.shednet.dev/provider":  provider,
		})).To(Succeed())
		services := etcdServices(ctx, provider)
	sourceChecks:
		for _, source := range sources {
			identity := endpointIdentity(provider, source.kind, source.name)
			deadline, found := deadlines[identity]
			g.Expect(found).To(BeTrue())
			if !found || !time.Now().Add(time.Second).Before(deadline) {
				continue
			}
			for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
				actual, err := forward.Query(ctx, source.hostname, recordType)
				if !time.Now().Add(time.Second).Before(deadline) {
					continue sourceChecks
				}
				g.Expect(err).NotTo(HaveOccurred())
				if recordType == dns.TypeA {
					g.Expect(actual).To(Equal([]string{netip.MustParseAddr(target).String()}), "%s A record before its lifecycle deadline", source.hostname)
				} else {
					g.Expect(actual).To(BeEmpty(), "%s AAAA record before its lifecycle deadline", source.hostname)
				}
			}
			var matching []externaldnsv1alpha1.DNSEndpoint
			for i := range objects.Items {
				object := &objects.Items[i]
				if endpointIdentity(provider, object.Annotations["labdns.shednet.dev/source-kind"], object.Annotations["labdns.shednet.dev/source-name"]) == identity {
					matching = append(matching, *object)
				}
			}
			g.Expect(matching).To(HaveLen(1), "pending DNSEndpoint for %s before its lifecycle deadline", source.kind)
			if len(matching) == 1 {
				g.Expect(matching[0].Spec.Endpoints).To(HaveLen(1))
				g.Expect(matching[0].Spec.Endpoints[0].DNSName).To(Equal(source.hostname))
				g.Expect(matching[0].Spec.Endpoints[0].RecordType).To(Equal(endpoint.RecordTypeA))
				g.Expect(matching[0].Spec.Endpoints[0].Targets).To(Equal(endpoint.Targets{target}))
				var state lifecycleState
				g.Expect(json.Unmarshal([]byte(matching[0].Annotations["labdns.shednet.dev/lifecycle"]), &state)).To(Succeed())
				g.Expect(state.Pending).To(HaveLen(1))
				if len(state.Pending) == 1 {
					g.Expect(state.Pending[0].Deadline).To(Equal(deadline.UTC().Format(time.RFC3339Nano)), "restart must preserve the persisted deadline")
				}
			}
			prefix := etcdHostnamePrefix(source.hostname)
			var actual []etcdService
			for _, service := range services {
				if strings.HasPrefix(service.Key, prefix) {
					actual = append(actual, service)
				}
			}
			g.Expect(actual).To(HaveLen(1), "etcd must retain the pending address for %s", source.hostname)
			if len(actual) == 1 {
				g.Expect(actual[0].Host).To(Equal(netip.MustParseAddr(target).String()))
				g.Expect(actual[0].Owner).To(Equal(owner))
				g.Expect(actual[0].TTL).To(Equal(uint32(30)))
				g.Expect(actual[0].TargetStrip).To(Equal(1))
			}
			checked[identity] = true
		}
	}, remaining, 500*time.Millisecond).Should(Succeed())
	Expect(checked).To(HaveLen(len(sources)), "every source must be observed retaining its target after restart")
}

func earliestPendingDeadline(deadlines map[string]time.Time) time.Time {
	var earliest time.Time
	for _, deadline := range deadlines {
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest
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

func assertEtcdOwnership(ctx context.Context, provider, owner string, sources []publicationSource, pairs []addressPair) {
	_, targets := expectedByType(pairs)
	expected := map[string][]string{}
	for _, source := range sources {
		values := make([]string, 0, len(targets))
		for target := range targets {
			values = append(values, target)
		}
		sort.Strings(values)
		expected[source.hostname] = values
	}
	assertEtcdHostTargets(ctx, provider, owner, expected)
	assertEtcdInventoryEventually(ctx, provider, sources)
}

func assertEtcdHostTargets(ctx context.Context, provider, owner string, expected map[string][]string) {
	Eventually(func(g Gomega) {
		services := etcdServices(ctx, provider)
		for hostname, targets := range expected {
			prefix := etcdHostnamePrefix(hostname)
			actual := map[string]bool{}
			for _, service := range services {
				if !strings.HasPrefix(service.Key, prefix) {
					continue
				}
				g.Expect(service.Owner).To(Equal(owner))
				g.Expect(service.TTL).To(Equal(uint32(30)))
				g.Expect(service.TargetStrip).To(Equal(1))
				g.Expect(actual[service.Host]).To(BeFalse(), "duplicate etcd target %s for %s", service.Host, hostname)
				actual[service.Host] = true
			}
			want := map[string]bool{}
			for _, target := range normalizeAddresses(targets) {
				want[target] = true
			}
			g.Expect(actual).To(Equal(want), "etcd targets for %s", hostname)
		}
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

func assertEtcdInventoryEventually(ctx context.Context, provider string, activeSources []publicationSource) {
	Eventually(func(g Gomega) {
		services := etcdServices(ctx, provider)
		assertEtcdInventory(g, services, activeSources)
	}, normalTimeout, 250*time.Millisecond).Should(Succeed())
}

func assertSourcesRetiredEventually(ctx context.Context, sources []publicationSource, forward *dnsForward, provider string, timeout time.Duration) {
	identities := make(map[string]bool, len(sources))
	for _, source := range sources {
		identities[endpointIdentity(provider, source.kind, source.name)] = true
	}
	Eventually(func(g Gomega) {
		for _, source := range sources {
			for _, recordType := range []uint16{dns.TypeA, dns.TypeAAAA} {
				actual, err := forward.Query(ctx, source.hostname, recordType)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(actual).To(BeEmpty(), "%s %s remains after source retirement", source.hostname, dns.TypeToString[recordType])
			}
		}
		var objects externaldnsv1alpha1.DNSEndpointList
		g.Expect(kubeClient.List(ctx, &objects, client.InNamespace(workNamespace), client.MatchingLabels{
			"app.kubernetes.io/managed-by": "labdns",
			"labdns.shednet.dev/provider":  provider,
		})).To(Succeed())
		for i := range objects.Items {
			object := &objects.Items[i]
			key := endpointIdentity(provider, object.Annotations["labdns.shednet.dev/source-kind"], object.Annotations["labdns.shednet.dev/source-name"])
			g.Expect(identities).NotTo(HaveKey(key), "retired source DNSEndpoint still exists")
		}
		services := etcdServices(ctx, provider)
		for _, source := range sources {
			prefixes := etcdHostnamePrefixes(source.hostname)
			for _, service := range services {
				for _, prefix := range prefixes {
					g.Expect(service.Key).NotTo(HavePrefix(prefix), "etcd entry for %s remains", source.hostname)
				}
			}
		}
		assertEtcdInventory(g, services, sharedSources)
	}, timeout, 200*time.Millisecond).Should(Succeed())
}

func appendSources(groups ...[]publicationSource) []publicationSource {
	var result []publicationSource
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

func assertEtcdInventory(g Gomega, services []etcdService, activeSources []publicationSource) {
	prefixes := make([]string, 0, len(activeSources)*3)
	for _, source := range activeSources {
		prefixes = append(prefixes, etcdHostnamePrefixes(source.hostname)...)
	}
	for _, service := range services {
		if service.Key == "/skydns/test/example/e2e/foreign/static" {
			continue
		}
		allowed := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(service.Key, prefix) {
				allowed = true
				break
			}
		}
		g.Expect(allowed).To(BeTrue(), "unexpected etcd backend key %s", service.Key)
	}
}

func etcdHostnamePrefix(name string) string {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for left, right := 0, len(labels)-1; left < right; left, right = left+1, right-1 {
		labels[left], labels[right] = labels[right], labels[left]
	}
	return "/skydns/" + strings.Join(labels, "/") + "/"
}

func etcdHostnamePrefixes(hostname string) []string {
	labels := strings.Split(strings.TrimSuffix(hostname, "."), ".")
	ordinary := etcdHostnamePrefix(hostname)
	labels[0] = "a-" + labels[0]
	aPrefix := etcdHostnamePrefix(strings.Join(labels, "."))
	labels[0] = "aaaa-" + strings.TrimPrefix(labels[0], "a-")
	aaaaPrefix := etcdHostnamePrefix(strings.Join(labels, "."))
	return []string{ordinary, aPrefix, aaaaPrefix}
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
