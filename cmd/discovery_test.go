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
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeDiscovery struct {
	resources map[string]*metav1.APIResourceList
	err       error
}

func (f fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resources[groupVersion], nil
}

func TestCheckPrerequisites(t *testing.T) {
	resources := validDiscoveryResources()
	if err := checkPrerequisites(fakeDiscovery{resources: resources}, true); err != nil {
		t.Fatal(err)
	}
	resources["externaldns.k8s.io/v1alpha1"].APIResources[0].Namespaced = false
	if err := checkPrerequisites(fakeDiscovery{resources: resources}, false); err == nil {
		t.Fatal("accepted cluster-scoped DNSEndpoint")
	}
}

func TestCheckPrerequisitesRejectsWrongKindAndMissingVerb(t *testing.T) {
	resources := validDiscoveryResources()
	resources["externaldns.k8s.io/v1alpha1"].APIResources[0].Kind = "WrongEndpoint"
	if err := checkPrerequisites(fakeDiscovery{resources: resources}, false); err == nil {
		t.Fatal("accepted wrong DNSEndpoint kind")
	}
	resources = validDiscoveryResources()
	resources["externaldns.k8s.io/v1alpha1"].APIResources[0].Verbs = metav1.Verbs{"get", "list", "watch"}
	if err := checkPrerequisites(fakeDiscovery{resources: resources}, false); err == nil {
		t.Fatal("accepted DNSEndpoint without write verbs")
	}
	resources = validDiscoveryResources()
	resources["gateway.networking.k8s.io/v1"].APIResources[2].Kind = "WrongClass"
	if err := checkPrerequisites(fakeDiscovery{resources: resources}, true); err == nil {
		t.Fatal("accepted wrong GatewayClass kind")
	}
}

func validDiscoveryResources() map[string]*metav1.APIResourceList {
	readVerbs := metav1.Verbs{"get", "list", "watch"}
	return map[string]*metav1.APIResourceList{
		"externaldns.k8s.io/v1alpha1": {APIResources: []metav1.APIResource{{
			Name: "dnsendpoints", Kind: "DNSEndpoint", Namespaced: true,
			Verbs: metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete"},
		}}},
		"gateway.networking.k8s.io/v1": {APIResources: []metav1.APIResource{
			{Name: "httproutes", Kind: "HTTPRoute", Namespaced: true, Verbs: readVerbs},
			{Name: "gateways", Kind: "Gateway", Namespaced: true, Verbs: readVerbs},
			{Name: "gatewayclasses", Kind: "GatewayClass", Namespaced: false, Verbs: readVerbs},
		}},
		"gateway.networking.k8s.io/v1beta1": {
			APIResources: []metav1.APIResource{{
				Name: "referencegrants", Kind: "ReferenceGrant", Namespaced: true, Verbs: readVerbs,
			}},
		},
	}
}

func TestCheckPrerequisitesExplainsDiscoveryFailure(t *testing.T) {
	if err := checkPrerequisites(fakeDiscovery{err: errors.New("forbidden")}, false); err == nil {
		t.Fatal("expected error")
	}
}
