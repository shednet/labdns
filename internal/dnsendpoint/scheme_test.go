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
	"net/url"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
)

func TestSchemeEncodesExternalDNSRequestOptions(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// Repeated registration must also work when callers compose schemes.
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	codec := runtime.NewParameterCodec(scheme)
	propagation := metav1.DeletePropagationForeground
	for _, test := range []struct {
		name    string
		options runtime.Object
		want    url.Values
	}{
		{"get", &metav1.GetOptions{ResourceVersion: "42"}, url.Values{"resourceVersion": {"42"}}},
		{"list", &metav1.ListOptions{LabelSelector: "provider=www", Limit: 10, Continue: "next"}, url.Values{"labelSelector": {"provider=www"}, "limit": {"10"}, "continue": {"next"}}},
		{"watch", &metav1.ListOptions{Watch: true, ResourceVersion: "42"}, url.Values{"watch": {"true"}, "resourceVersion": {"42"}}},
		{"create", &metav1.CreateOptions{FieldManager: "labdns", DryRun: []string{metav1.DryRunAll}}, url.Values{"fieldManager": {"labdns"}, "dryRun": {"All"}}},
		{"update", &metav1.UpdateOptions{FieldManager: "labdns"}, url.Values{"fieldManager": {"labdns"}}},
		{"patch", &metav1.PatchOptions{FieldManager: "labdns", Force: new(true)}, url.Values{"fieldManager": {"labdns"}, "force": {"true"}}},
		{"delete", &metav1.DeleteOptions{PropagationPolicy: &propagation}, url.Values{"propagationPolicy": {"Foreground"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := codec.EncodeParameters(test.options, externaldnsv1alpha1.GroupVersion)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("request parameters = %v, want %v", got, test.want)
			}
		})
	}
}
