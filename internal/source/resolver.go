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
	"net/netip"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

type Resolver struct {
	Reader client.Reader
}

type PublicationOptions struct {
	Families      []AddressFamily
	Annotations   map[string]string
	TTL           *int64
	DeletionDelay *time.Duration
}

func (r Resolver) Publications(
	ctx context.Context,
	projection []HostProjection,
	providers []*labdnsv1alpha1.DNSProvider,
	options PublicationOptions,
	diagnostic DiagnosticFunc,
) ([]Publication, error) {
	emit := diagnosticOnce(diagnostic)
	result := make([]Publication, 0, len(providers))
	for _, provider := range providers {
		publication, err := r.publication(ctx, projection, provider, options, emit)
		if err != nil {
			return nil, err
		}
		result = append(result, publication)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ProviderName < result[j].ProviderName })
	return result, nil
}

func (r Resolver) publication(
	ctx context.Context,
	projection []HostProjection,
	provider *labdnsv1alpha1.DNSProvider,
	options PublicationOptions,
	diagnostic DiagnosticFunc,
) (Publication, error) {
	families := options.Families
	if len(families) == 0 {
		if provider.Spec.IPSources.IPv4 != nil {
			families = append(families, IPv4)
		}
		if provider.Spec.IPSources.IPv6 != nil {
			families = append(families, IPv6)
		}
	}
	for _, family := range families {
		if labelFor(provider, family) == "" {
			diagnostic(DiagnosticWarning, "AddressFamilyUnavailable", fmt.Sprintf("DNSProvider %q has no Node label source configured for requested %s address family", provider.Name, addressFamilyName(family)))
		}
	}
	properties, metadata := providerProperties(provider, options.Annotations)
	ttl := provider.Spec.RecordDefaults.TTL
	if ttl == 0 {
		ttl = 300
	}
	if options.TTL != nil {
		ttl = *options.TTL
	}
	delay := 60 * time.Second
	if provider.Spec.RecordDefaults.DeletionDelay != nil {
		delay = provider.Spec.RecordDefaults.DeletionDelay.Duration
	}
	if options.DeletionDelay != nil {
		delay = *options.DeletionDelay
	}
	publication := Publication{ProviderName: provider.Name, MetadataAnnotations: metadata, DeletionDelay: delay}
	zones := make([]string, 0, len(provider.Spec.Zones))
	for _, zone := range provider.Spec.Zones {
		zones = append(zones, zone.Name)
	}
	for _, host := range projection {
		if matchingZone(host.Hostname, zones) == "" {
			diagnostic(DiagnosticWarning, "HostnameOutsideZones", fmt.Sprintf("hostname %q is outside DNSProvider %q zones", host.Hostname, provider.Name))
			continue
		}
		targets := map[AddressFamily]map[string]struct{}{IPv4: {}, IPv6: {}}
		for _, backend := range host.Backends {
			resolved, err := r.backendTargets(ctx, backend, provider, families, diagnostic)
			if err != nil {
				return Publication{}, err
			}
			for family, addresses := range resolved {
				for _, address := range addresses {
					targets[family][address] = struct{}{}
				}
			}
		}
		for _, family := range families {
			values := stringSet(targets[family])
			if len(values) == 0 {
				continue
			}
			recordType := RecordTypeA
			if family == IPv6 {
				recordType = RecordTypeAAAA
			}
			publication.Records = append(publication.Records, Record{DNSName: host.Hostname, RecordType: recordType, Targets: values, TTL: ttl, ProviderSpecific: properties})
		}
	}
	sort.Slice(publication.Records, func(i, j int) bool {
		if publication.Records[i].DNSName == publication.Records[j].DNSName {
			return publication.Records[i].RecordType < publication.Records[j].RecordType
		}
		return publication.Records[i].DNSName < publication.Records[j].DNSName
	})
	return publication, nil
}

func (r Resolver) backendTargets(ctx context.Context, backend Backend, provider *labdnsv1alpha1.DNSProvider, families []AddressFamily, diagnostic DiagnosticFunc) (map[AddressFamily][]string, error) {
	var service corev1.Service
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: backend.Namespace, Name: backend.Name}, &service); err != nil {
		wrapped := fmt.Errorf("get Service %s: %w", backend.Key(), err)
		if apierrors.IsNotFound(err) {
			return nil, Invalid(dependency("ServiceNotFound", wrapped))
		}
		return nil, dependency("ServiceReadFailed", wrapped)
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.Reader.List(ctx, &slices, client.InNamespace(backend.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: backend.Name}); err != nil {
		return nil, dependency("EndpointSliceReadFailed", fmt.Errorf("list EndpointSlices for Service %s: %w", backend.Key(), err))
	}
	if len(slices.Items) == 0 {
		diagnostic(DiagnosticNormal, "EndpointSlicesUnavailable", fmt.Sprintf("Service %s has no EndpointSlices", backend.Key()))
		return map[AddressFamily][]string{}, nil
	}
	nodeNames := map[string]struct{}{}
	readyEndpoints := 0
	readyEndpointsWithoutNodeName := 0
	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			readyEndpoints++
			if endpoint.NodeName == nil || *endpoint.NodeName == "" {
				readyEndpointsWithoutNodeName++
				continue
			}
			nodeNames[*endpoint.NodeName] = struct{}{}
		}
	}
	if readyEndpoints == 0 {
		diagnostic(DiagnosticNormal, "NoReadyEndpoints", fmt.Sprintf("Service %s has EndpointSlices but no ready endpoints", backend.Key()))
	}
	if readyEndpointsWithoutNodeName > 0 {
		diagnostic(DiagnosticWarning, "EndpointNodeNameMissing", fmt.Sprintf("Service %s has %d ready endpoint(s) without nodeName", backend.Key(), readyEndpointsWithoutNodeName))
	}
	result := map[AddressFamily][]string{}
	missingLabels := map[AddressFamily][]string{}
	for _, nodeName := range stringSet(nodeNames) {
		var node corev1.Node
		if err := r.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			wrapped := fmt.Errorf("get Node %s for Service %s: %w", nodeName, backend.Key(), err)
			if apierrors.IsNotFound(err) {
				return nil, Invalid(dependency("NodeNotFound", wrapped))
			}
			return nil, dependency("NodeReadFailed", wrapped)
		}
		for _, family := range families {
			label := labelFor(provider, family)
			if label == "" {
				continue
			}
			value := node.Labels[label]
			if value == "" {
				missingLabels[family] = append(missingLabels[family], nodeName)
				continue
			}
			address, err := parseNodeAddress(value, family)
			if err != nil {
				return nil, fmt.Errorf("node %s label %s contains invalid IP %q: %w", nodeName, label, value, err)
			}
			result[family] = append(result[family], address.String())
		}
	}
	for _, family := range families {
		nodeList := missingLabels[family]
		if len(nodeList) == 0 {
			continue
		}
		diagnostic(DiagnosticWarning, "NodeAddressLabelMissing", fmt.Sprintf("DNSProvider %q Service %s is missing Node label %q for %s on Nodes [%s]", provider.Name, backend.Key(), labelFor(provider, family), addressFamilyName(family), strings.Join(sortedUnique(nodeList), ", ")))
	}
	for family := range result {
		result[family] = sortedUnique(result[family])
	}
	return result, nil
}

func addressFamilyName(family AddressFamily) string {
	switch family {
	case IPv4:
		return "IPv4"
	case IPv6:
		return "IPv6"
	default:
		return string(family)
	}
}

func parseNodeAddress(value string, family AddressFamily) (netip.Addr, error) {
	if family == IPv4 {
		if strings.HasPrefix(value, "v6-") {
			return netip.Addr{}, Invalid(fmt.Errorf("IPv4 values must not use the v6- prefix"))
		}
		address, err := netip.ParseAddr(value)
		if err != nil {
			return netip.Addr{}, Invalid(err)
		}
		if !address.Is4() {
			return netip.Addr{}, Invalid(fmt.Errorf("address is not ipv4"))
		}
		return address, nil
	}
	if family != IPv6 {
		return netip.Addr{}, Invalid(fmt.Errorf("unsupported address family %q", family))
	}
	if !strings.HasPrefix(value, "v6-") {
		return netip.Addr{}, Invalid(fmt.Errorf("IPv6 values must use the v6- prefix"))
	}
	decoded := strings.ReplaceAll(strings.TrimPrefix(value, "v6-"), "-", ":")
	address, err := netip.ParseAddr(decoded)
	if err != nil {
		return netip.Addr{}, Invalid(err)
	}
	if !address.Is6() || address.Is4In6() {
		return netip.Addr{}, Invalid(fmt.Errorf("address is not ipv6"))
	}
	canonical := "v6-" + strings.ReplaceAll(address.String(), ":", "-")
	if value != canonical {
		return netip.Addr{}, Invalid(fmt.Errorf("IPv6 value is not canonical; use %q", canonical))
	}
	return address, nil
}

func labelFor(provider *labdnsv1alpha1.DNSProvider, family AddressFamily) string {
	if family == IPv4 && provider.Spec.IPSources.IPv4 != nil {
		return provider.Spec.IPSources.IPv4.NodeLabel
	}
	if family == IPv6 && provider.Spec.IPSources.IPv6 != nil {
		return provider.Spec.IPSources.IPv6.NodeLabel
	}
	return ""
}

func stringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
