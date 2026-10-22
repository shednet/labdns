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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
)

// AddToScheme registers ExternalDNS resources and their request options.
func AddToScheme(scheme *runtime.Scheme) error {
	if err := externaldnsv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	// ExternalDNS v0.22 omits the meta types required by typed client requests.
	// See https://github.com/kubernetes-sigs/external-dns/issues/6787.
	metav1.AddToGroupVersion(scheme, externaldnsv1alpha1.GroupVersion)
	return nil
}
