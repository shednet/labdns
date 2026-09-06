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
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	AnnotationPrefix        = "labdns.shednet.dev/"
	ExternalDNSPrefix       = "external-dns.alpha.kubernetes.io/"
	EnabledAnnotation       = AnnotationPrefix + "enabled"
	ProvidersAnnotation     = AnnotationPrefix + "providers"
	HostnamesAnnotation     = AnnotationPrefix + "hostnames"
	TTLAnnotation           = AnnotationPrefix + "ttl"
	FamiliesAnnotation      = AnnotationPrefix + "address-families"
	DeletionDelayAnnotation = AnnotationPrefix + "deletion-delay"
)

// relevantAnnotations returns only annotations labdns is allowed to inherit.
func relevantAnnotations(in map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range in {
		if strings.HasPrefix(key, AnnotationPrefix) || strings.HasPrefix(key, ExternalDNSPrefix) {
			out[key] = value
		}
	}
	return out
}

// MergeAnnotations applies increasingly-specific maps. Presence, including an
// empty value, overrides inherited values.
func MergeAnnotations(levels ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, level := range levels {
		maps.Copy(out, relevantAnnotations(level))
	}
	return out
}

type ParsedAnnotations struct {
	Enabled       bool
	Providers     []string
	Hostnames     []string
	TTL           *int64
	Families      []AddressFamily
	DeletionDelay *time.Duration
	Resolved      map[string]string
}

type AddressFamily string

const (
	IPv4 AddressFamily = "ipv4"
	IPv6 AddressFamily = "ipv6"
)

func ParseAnnotations(values map[string]string) (ParsedAnnotations, error) {
	result := ParsedAnnotations{Resolved: relevantAnnotations(values)}
	if value, ok := values[EnabledAnnotation]; ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return result, Invalid(fmt.Errorf("invalid %s: %w", EnabledAnnotation, err))
		}
		result.Enabled = parsed
	}
	if value, ok := values[ProvidersAnnotation]; ok {
		result.Providers = commaList(value)
		for _, name := range result.Providers {
			if len(name) > 63 {
				return result, Invalid(fmt.Errorf("invalid provider %q: must be at most 63 characters", name))
			}
			if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
				return result, Invalid(fmt.Errorf("invalid provider %q: %s", name, strings.Join(errs, ", ")))
			}
		}
	}
	if value, ok := values[HostnamesAnnotation]; ok {
		for _, hostname := range commaList(value) {
			normalized, err := NormalizeHostname(hostname)
			if err != nil {
				return result, Invalid(fmt.Errorf("invalid hostname override %q: %w", hostname, err))
			}
			result.Hostnames = append(result.Hostnames, normalized)
		}
		result.Hostnames = sortedUnique(result.Hostnames)
	}
	if value, ok := values[TTLAnnotation]; ok {
		ttl, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || ttl < 1 || ttl > 2147483647 {
			return result, Invalid(fmt.Errorf("invalid %s %q: must be an integer in 1..2147483647", TTLAnnotation, value))
		}
		result.TTL = &ttl
	}
	if value, ok := values[DeletionDelayAnnotation]; ok {
		delay, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || delay < 0 {
			return result, Invalid(fmt.Errorf("invalid %s %q: must be a non-negative Go duration", DeletionDelayAnnotation, value))
		}
		result.DeletionDelay = &delay
	}
	if value, ok := values[FamiliesAnnotation]; ok {
		for _, family := range commaList(value) {
			switch strings.ToLower(family) {
			case string(IPv4):
				result.Families = append(result.Families, IPv4)
			case string(IPv6):
				result.Families = append(result.Families, IPv6)
			default:
				return result, Invalid(fmt.Errorf("invalid address family %q", family))
			}
		}
		result.Families = uniqueFamilies(result.Families)
	}
	return result, nil
}

func commaList(value string) []string {
	items := strings.Split(value, ",")
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return sortedUnique(result)
}

func sortedUnique(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueFamilies(values []AddressFamily) []AddressFamily {
	seen := map[AddressFamily]bool{}
	result := make([]AddressFamily, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}
