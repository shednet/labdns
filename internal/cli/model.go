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

package cli

import "time"

type SourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type RetiringTarget struct {
	Target   string    `json:"target"`
	Deadline time.Time `json:"deadline"`
}

type Record struct {
	DNSName          string              `json:"dnsName"`
	RecordType       string              `json:"recordType"`
	Targets          []string            `json:"targets"`
	ActiveTargets    []string            `json:"activeTargets"`
	TTL              int64               `json:"ttl"`
	Provider         string              `json:"provider"`
	Source           SourceRef           `json:"source"`
	DNSEndpoint      ObjectRef           `json:"dnsEndpoint"`
	Generation       int64               `json:"generation"`
	Observed         int64               `json:"observedGeneration"`
	ExternalDNSState string              `json:"externalDNSState"`
	Retiring         []RetiringTarget    `json:"retiringTargets,omitempty"`
	Properties       map[string][]string `json:"properties,omitempty"`
	LifecycleError   string              `json:"lifecycleError,omitempty"`
}

type RecordList struct {
	Items []Record `json:"items"`
}

type ProviderDetail struct {
	Found      bool     `json:"found"`
	Zones      []string `json:"zones,omitempty"`
	IPv4Label  string   `json:"ipv4NodeLabel,omitempty"`
	IPv6Label  string   `json:"ipv6NodeLabel,omitempty"`
	DefaultTTL int64    `json:"defaultTTL,omitempty"`
}

type SourceDetail struct {
	Found      bool `json:"found"`
	UIDMatches bool `json:"uidMatches"`
}

type DNSLookup struct {
	Server  string   `json:"server"`
	Answers []string `json:"answers,omitempty"`
	State   string   `json:"state"`
	Error   string   `json:"error,omitempty"`
}

type RecordDetail struct {
	Record   Record         `json:"record"`
	Provider ProviderDetail `json:"providerDetail"`
	Source   SourceDetail   `json:"sourceDetail"`
	DNS      *DNSLookup     `json:"dnsLookup,omitempty"`
}

type RecordDetails struct {
	Query string         `json:"query"`
	Items []RecordDetail `json:"items"`
}

type Prerequisite struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type ControllerStatus struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Desired    int32  `json:"desiredReplicas"`
	Available  int32  `json:"availableReplicas"`
	ReadyPods  int    `json:"readyPods"`
	GatewayAPI bool   `json:"gatewayAPIEnabled"`
}

type StateSummary struct {
	Providers         int `json:"providers"`
	PublishingSources int `json:"publishingSources"`
	DNSEndpoints      int `json:"dnsEndpoints"`
	Records           int `json:"records"`
	PendingTargets    int `json:"pendingTargets"`
	Observed          int `json:"observed"`
	Stale             int `json:"stale"`
	Invalid           int `json:"invalid"`
}

type Status struct {
	Overall       string             `json:"overall"`
	Prerequisites []Prerequisite     `json:"prerequisites"`
	Controllers   []ControllerStatus `json:"controllers"`
	Summary       StateSummary       `json:"summary"`
	Warnings      []string           `json:"warnings,omitempty"`
}
