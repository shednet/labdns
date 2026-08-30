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
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

type Identity struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        types.UID
}

type Backend struct {
	Namespace string
	Name      string
}

func (b Backend) Key() string { return b.Namespace + "/" + b.Name }

type HostProjection struct {
	Hostname string
	Backends []Backend
}

type Property struct {
	Name  string
	Value string
}

type Record struct {
	DNSName          string
	RecordType       string
	Targets          []string
	TTL              int64
	ProviderSpecific []Property
}

type Publication struct {
	ProviderName        string
	Records             []Record
	MetadataAnnotations map[string]string
	DeletionDelay       time.Duration
}

// Output is the provider-neutral boundary implemented by the DNSEndpoint writer.
type Output interface {
	Apply(context.Context, Identity, []Publication) error
}

type WarningFunc func(reason, message string)

func IngressProjection(ingress *networkingv1.Ingress, overrides []string, warn WarningFunc) ([]HostProjection, error) {
	all := map[Backend]struct{}{}
	if ingress.Spec.DefaultBackend != nil {
		if backend, ok := ingressBackend(ingress.Namespace, ingress.Spec.DefaultBackend); ok {
			all[backend] = struct{}{}
		}
	}
	rules := map[string]map[Backend]struct{}{}
	for _, rule := range ingress.Spec.Rules {
		if rule.Host == "" || rule.HTTP == nil {
			continue
		}
		host, err := NormalizeHostname(rule.Host)
		if err != nil {
			return nil, err
		}
		if rules[host] == nil {
			rules[host] = map[Backend]struct{}{}
		}
		for _, path := range rule.HTTP.Paths {
			if backend, ok := ingressBackend(ingress.Namespace, &path.Backend); ok {
				rules[host][backend] = struct{}{}
				all[backend] = struct{}{}
			}
		}
	}
	if len(overrides) != 0 {
		backends := backendSet(all)
		result := make([]HostProjection, 0, len(overrides))
		for _, hostname := range overrides {
			result = append(result, HostProjection{Hostname: hostname, Backends: backends})
		}
		return result, nil
	}
	for _, tls := range ingress.Spec.TLS {
		for _, raw := range tls.Hosts {
			host, err := NormalizeHostname(raw)
			if err != nil {
				return nil, err
			}
			if _, found := rules[host]; found {
				continue
			}
			if ingress.Spec.DefaultBackend != nil {
				if backend, ok := ingressBackend(ingress.Namespace, ingress.Spec.DefaultBackend); ok {
					rules[host] = map[Backend]struct{}{backend: {}}
				}
			} else if warn != nil {
				warn("TLSHostWithoutBackend", fmt.Sprintf("TLS-only hostname %q has no matching rule or default backend", host))
			}
		}
	}
	return projectionSet(rules), nil
}

func ingressBackend(namespace string, backend *networkingv1.IngressBackend) (Backend, bool) {
	if backend == nil || backend.Service == nil || backend.Service.Name == "" {
		return Backend{}, false
	}
	return Backend{Namespace: namespace, Name: backend.Service.Name}, true
}

func HTTPRouteProjection(ctx context.Context, reader client.Reader, route *gatewayv1.HTTPRoute, overrides []string, warn WarningFunc) ([]HostProjection, error) {
	all := map[Backend]struct{}{}
	for _, rule := range route.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			backend, ok, err := routeBackend(ctx, reader, route, ref.BackendObjectReference)
			if err != nil {
				return nil, err
			}
			if !ok {
				if warn != nil {
					warn("UnsupportedBackend", fmt.Sprintf("backendRef %q is unsupported or lacks a ReferenceGrant", ref.Name))
				}
				continue
			}
			all[backend] = struct{}{}
		}
	}
	hostnames := overrides
	if len(hostnames) == 0 {
		for _, hostname := range route.Spec.Hostnames {
			normalized, err := NormalizeHostname(string(hostname))
			if err != nil {
				return nil, err
			}
			hostnames = append(hostnames, normalized)
		}
		hostnames = sortedUnique(hostnames)
	}
	if len(all) == 0 {
		return nil, nil
	}
	backends := backendSet(all)
	result := make([]HostProjection, 0, len(hostnames))
	for _, hostname := range hostnames {
		result = append(result, HostProjection{Hostname: hostname, Backends: backends})
	}
	return result, nil
}

func routeBackend(ctx context.Context, reader client.Reader, route *gatewayv1.HTTPRoute, ref gatewayv1.BackendObjectReference) (Backend, bool, error) {
	if ref.Group != nil && string(*ref.Group) != "" || ref.Kind != nil && string(*ref.Kind) != "Service" {
		return Backend{}, false, nil
	}
	namespace := route.Namespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}
	backend := Backend{Namespace: namespace, Name: string(ref.Name)}
	if namespace == route.Namespace {
		return backend, true, nil
	}
	var grants gatewayv1beta1.ReferenceGrantList
	if err := reader.List(ctx, &grants, client.InNamespace(namespace)); err != nil {
		return Backend{}, false, fmt.Errorf("list ReferenceGrants in %s: %w", namespace, err)
	}
	for i := range grants.Items {
		grant := &grants.Items[i]
		fromOK := false
		for _, from := range grant.Spec.From {
			if string(from.Group) == gatewayv1.GroupName && string(from.Kind) == "HTTPRoute" && string(from.Namespace) == route.Namespace {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, to := range grant.Spec.To {
			if string(to.Group) == "" && string(to.Kind) == "Service" && (to.Name == nil || string(*to.Name) == backend.Name) {
				return backend, true, nil
			}
		}
	}
	return Backend{}, false, nil
}

func projectionSet(values map[string]map[Backend]struct{}) []HostProjection {
	result := make([]HostProjection, 0, len(values))
	for hostname, backends := range values {
		if len(backends) != 0 {
			result = append(result, HostProjection{Hostname: hostname, Backends: backendSet(backends)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hostname < result[j].Hostname })
	return result
}

func backendSet(values map[Backend]struct{}) []Backend {
	result := make([]Backend, 0, len(values))
	for backend := range values {
		result = append(result, backend)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result
}

func ProviderProperties(provider *labdnsv1alpha1.DNSProvider, annotations map[string]string) ([]Property, map[string]string) {
	properties := map[string]string{}
	for _, property := range provider.Spec.ProviderSpecific.Defaults {
		properties[property.Name] = property.Value
	}
	for _, key := range provider.Spec.ProviderSpecific.AnnotationKeys {
		if value, ok := annotations[string(key)]; ok {
			properties[string(key)] = value
		}
	}
	result := make([]Property, 0, len(properties))
	for name, value := range properties {
		result = append(result, Property{Name: name, Value: value})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Value < result[j].Value
		}
		return result[i].Name < result[j].Name
	})
	metadata := map[string]string{}
	for key, value := range annotations {
		if strings.HasPrefix(key, ExternalDNSPrefix) {
			metadata[key] = value
		}
	}
	return result, metadata
}
