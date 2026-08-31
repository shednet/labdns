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

package main

import (
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type resourceDiscovery interface {
	ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error)
}

const (
	verbGet   = "get"
	verbList  = "list"
	verbWatch = "watch"
)

func checkPrerequisites(discovery resourceDiscovery, gatewayEnabled bool) error {
	if err := requireResources(
		discovery, "externaldns.k8s.io/v1alpha1", []requiredResource{{
			Name: "dnsendpoints", Kind: "DNSEndpoint", Namespaced: true,
			Verbs: []string{verbGet, verbList, verbWatch, "create", "update", "patch", "delete"},
		}},
	); err != nil {
		return fmt.Errorf(
			"DNSEndpoint prerequisite unavailable or incompatible; install the official ExternalDNS v0.21.0 CRD: %w",
			err,
		)
	}
	if gatewayEnabled {
		if err := requireResources(discovery, "gateway.networking.k8s.io/v1", []requiredResource{
			{Name: "httproutes", Kind: "HTTPRoute", Namespaced: true, Verbs: []string{verbGet, verbList, verbWatch}},
			{Name: "gateways", Kind: "Gateway", Namespaced: true, Verbs: []string{verbGet, verbList, verbWatch}},
			{Name: "gatewayclasses", Kind: "GatewayClass", Namespaced: false, Verbs: []string{verbGet, verbList, verbWatch}},
		}); err != nil {
			return fmt.Errorf("gateway API support requested but v1 resources are unavailable: %w", err)
		}
		if err := requireResources(
			discovery, "gateway.networking.k8s.io/v1beta1", []requiredResource{{
				Name: "referencegrants", Kind: "ReferenceGrant", Namespaced: true,
				Verbs: []string{verbGet, verbList, verbWatch},
			}},
		); err != nil {
			return fmt.Errorf("gateway API support requested but ReferenceGrant is unavailable: %w", err)
		}
	}
	return nil
}

type requiredResource struct {
	Name       string
	Kind       string
	Namespaced bool
	Verbs      []string
}

func requireResources(discovery resourceDiscovery, groupVersion string, required []requiredResource) error {
	resources, err := discovery.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return fmt.Errorf("discover %s: %w", groupVersion, err)
	}
	found := map[string]metav1.APIResource{}
	for _, resource := range resources.APIResources {
		found[resource.Name] = resource
	}
	for _, requirement := range required {
		resource, ok := found[requirement.Name]
		if !ok {
			return fmt.Errorf("resource %s/%s is missing", groupVersion, requirement.Name)
		}
		if resource.Kind != requirement.Kind {
			return fmt.Errorf(
				"resource %s/%s has kind %q, want %q",
				groupVersion, requirement.Name, resource.Kind, requirement.Kind,
			)
		}
		if resource.Namespaced != requirement.Namespaced {
			return fmt.Errorf(
				"resource %s/%s has namespaced=%t, want %t",
				groupVersion, requirement.Name, resource.Namespaced, requirement.Namespaced,
			)
		}
		for _, verb := range requirement.Verbs {
			if !slices.Contains(resource.Verbs, verb) {
				return fmt.Errorf("resource %s/%s is missing required verb %q", groupVersion, requirement.Name, verb)
			}
		}
	}
	return nil
}
