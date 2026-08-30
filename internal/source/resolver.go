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
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

type Resolver struct {
	Reader client.Reader
}

func (r Resolver) Publications(
	ctx context.Context,
	projection []HostProjection,
	providers []*labdnsv1alpha1.DNSProvider,
	parsed ParsedAnnotations,
	warn WarningFunc,
) ([]Publication, error) {
	result := make([]Publication, 0, len(providers))
	for _, provider := range providers {
		publication, err := r.publication(ctx, projection, provider, parsed, warn)
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
	parsed ParsedAnnotations,
	warn WarningFunc,
) (Publication, error) {
	families := parsed.Families
	if len(families) == 0 {
		if provider.Spec.IPSources.IPv4 != nil {
			families = append(families, IPv4)
		}
		if provider.Spec.IPSources.IPv6 != nil {
			families = append(families, IPv6)
		}
	}
	properties, metadata := ProviderProperties(provider, parsed.Resolved)
	ttl := provider.Spec.RecordDefaults.TTL
	if ttl == 0 {
		ttl = 300
	}
	if parsed.TTL != nil {
		ttl = *parsed.TTL
	}
	delay := 60 * time.Second
	if provider.Spec.RecordDefaults.DeletionDelay != nil {
		delay = provider.Spec.RecordDefaults.DeletionDelay.Duration
	}
	if parsed.DeletionDelay != nil {
		delay = *parsed.DeletionDelay
	}
	publication := Publication{ProviderName: provider.Name, MetadataAnnotations: metadata, DeletionDelay: delay}
	zones := make([]string, 0, len(provider.Spec.Zones))
	for _, zone := range provider.Spec.Zones {
		zones = append(zones, zone.Name)
	}
	for _, host := range projection {
		if MatchingZone(host.Hostname, zones) == "" {
			if warn != nil {
				warn("HostnameOutsideZones", fmt.Sprintf("hostname %q is outside DNSProvider %q zones", host.Hostname, provider.Name))
			}
			continue
		}
		targets := map[AddressFamily]map[string]struct{}{IPv4: {}, IPv6: {}}
		for _, backend := range host.Backends {
			resolved, err := r.backendTargets(ctx, backend, provider, families)
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
			recordType := "A"
			if family == IPv6 {
				recordType = "AAAA"
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

func (r Resolver) backendTargets(ctx context.Context, backend Backend, provider *labdnsv1alpha1.DNSProvider, families []AddressFamily) (map[AddressFamily][]string, error) {
	var service corev1.Service
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: backend.Namespace, Name: backend.Name}, &service); err != nil {
		return nil, fmt.Errorf("get Service %s: %w", backend.Key(), err)
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.Reader.List(ctx, &slices, client.InNamespace(backend.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: backend.Name}); err != nil {
		return nil, fmt.Errorf("list EndpointSlices for Service %s: %w", backend.Key(), err)
	}
	nodeNames := map[string]struct{}{}
	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			if endpoint.NodeName == nil || *endpoint.NodeName == "" {
				continue
			}
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			nodeNames[*endpoint.NodeName] = struct{}{}
		}
	}
	result := map[AddressFamily][]string{}
	for nodeName := range nodeNames {
		var node corev1.Node
		if err := r.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return nil, fmt.Errorf("get Node %s for Service %s: %w", nodeName, backend.Key(), err)
		}
		for _, family := range families {
			label := labelFor(provider, family)
			if label == "" {
				continue
			}
			value := node.Labels[label]
			if value == "" {
				continue
			}
			address, err := netip.ParseAddr(value)
			if err != nil {
				return nil, fmt.Errorf("node %s label %s contains invalid IP %q: %w", nodeName, label, value, err)
			}
			if family == IPv4 && !address.Is4() || family == IPv6 && !address.Is6() {
				return nil, fmt.Errorf("node %s label %s address %q is not %s", nodeName, label, value, family)
			}
			result[family] = append(result[family], address.String())
		}
	}
	for family := range result {
		result[family] = sortedUnique(result[family])
	}
	return result, nil
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
