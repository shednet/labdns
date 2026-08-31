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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DNSProviderSpec defines the desired state of DNSProvider
type DNSProviderSpec struct {
	// Zones contains the DNS suffixes this provider may publish.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Zones []DNSZone `json:"zones"`

	// IPSources selects Node labels containing publishable addresses. IPv4
	// values are literals. IPv6 values use v6- followed by canonical IPv6 text
	// with each colon replaced by a dash (for example, v6-2001-db8--10).
	// +kubebuilder:validation:XValidation:rule="has(self.ipv4) || has(self.ipv6)",message="at least one of ipv4 or ipv6 is required"
	IPSources IPSources `json:"ipSources"`

	// RecordDefaults supplies source-overridable record settings.
	// +optional
	RecordDefaults RecordDefaults `json:"recordDefaults,omitempty"`

	// ProviderSpecific controls the explicitly allowlisted ExternalDNS properties.
	// +optional
	ProviderSpecific ProviderSpecific `json:"providerSpecific,omitempty"`
}

type DNSZone struct {
	// Name is a normalized, lower-case DNS suffix without a trailing dot.
	// +kubebuilder:validation:Pattern=`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

type IPSources struct {
	// +optional
	IPv4 *NodeLabelSource `json:"ipv4,omitempty"`
	// +optional
	IPv6 *NodeLabelSource `json:"ipv6,omitempty"`
}

type NodeLabelSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:Pattern=`^(([a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*/)?[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('/') || self.split('/')[0].size() <= 253",message="qualified-name prefix must be at most 253 characters"
	NodeLabel string `json:"nodeLabel"`
}

type RecordDefaults struct {
	// +kubebuilder:default:=300
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	TTL int64 `json:"ttl,omitempty"`

	// +kubebuilder:default:="60s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="deletionDelay must not be negative"
	// +optional
	DeletionDelay *metav1.Duration `json:"deletionDelay,omitempty"`
}

type ProviderSpecific struct {
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	// +optional
	Defaults []ProviderProperty `json:"defaults,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	AnnotationKeys []AnnotationKey `json:"annotationKeys,omitempty"`
}

// AnnotationKey is a Kubernetes qualified name used as an annotation key.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=317
// +kubebuilder:validation:Pattern=`^(([a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*/)?[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
// +kubebuilder:validation:XValidation:rule="!self.contains('/') || self.split('/')[0].size() <= 253",message="qualified-name prefix must be at most 253 characters"
type AnnotationKey string

type ProviderProperty struct {
	// +kubebuilder:validation:MinLength=1
	Name  string `json:"name"`
	Value string `json:"value"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters"

// DNSProvider is the Schema for the dnsproviders API
type DNSProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DNSProvider
	// +required
	Spec DNSProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// DNSProviderList contains a list of DNSProvider
type DNSProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DNSProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSProvider{}, &DNSProviderList{})
}
