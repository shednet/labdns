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

const (
	stateCurrent     = "current"
	stateMissing     = "missing"
	stateObserved    = "observed"
	stateStale       = "stale"
	stateInvalid     = "invalid"
	stateUnsupported = "unsupported"
	stateNXDomain    = "nxdomain"
	stateMatch       = "match"
	stateMismatch    = "mismatch"
)

type sourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

type objectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type retiringTarget struct {
	Target   string    `json:"target"`
	Deadline time.Time `json:"deadline"`
}

type record struct {
	DNSName          string              `json:"dnsName"`
	RecordType       string              `json:"recordType"`
	Targets          []string            `json:"targets"`
	ActiveTargets    []string            `json:"activeTargets"`
	TTL              int64               `json:"ttl"`
	Provider         string              `json:"provider"`
	Source           sourceRef           `json:"source"`
	DNSEndpoint      objectRef           `json:"dnsEndpoint"`
	Generation       int64               `json:"generation"`
	Observed         int64               `json:"observedGeneration"`
	ExternalDNSState string              `json:"externalDNSState"`
	Retiring         []retiringTarget    `json:"retiringTargets,omitempty"`
	Properties       map[string][]string `json:"properties,omitempty"`
	LifecycleError   string              `json:"lifecycleError,omitempty"`
}

type recordList struct {
	Items []record `json:"items"`
}

type providerDetail struct {
	Found      bool     `json:"found"`
	Zones      []string `json:"zones,omitempty"`
	IPv4Label  string   `json:"ipv4NodeLabel,omitempty"`
	IPv6Label  string   `json:"ipv6NodeLabel,omitempty"`
	DefaultTTL int64    `json:"defaultTTL,omitempty"`
}

type sourceDetail struct {
	Found      bool `json:"found"`
	UIDMatches bool `json:"uidMatches"`
}

type dnsLookup struct {
	Server  string   `json:"server"`
	Answers []string `json:"answers,omitempty"`
	State   string   `json:"state"`
	Error   string   `json:"error,omitempty"`
}

type recordDetail struct {
	Record   record         `json:"record"`
	Provider providerDetail `json:"providerDetail"`
	Source   sourceDetail   `json:"sourceDetail"`
	DNS      *dnsLookup     `json:"dnsLookup,omitempty"`
}

type recordDetails struct {
	Query string         `json:"query"`
	Items []recordDetail `json:"items"`
}

type prerequisite struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type controllerStatus struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Desired    int32  `json:"desiredReplicas"`
	Available  int32  `json:"availableReplicas"`
	ReadyPods  int    `json:"readyPods"`
	GatewayAPI bool   `json:"gatewayAPIEnabled"`
}

type stateSummary struct {
	Providers         int `json:"providers"`
	PublishingSources int `json:"publishingSources"`
	DNSEndpoints      int `json:"dnsEndpoints"`
	Records           int `json:"records"`
	PendingTargets    int `json:"pendingTargets"`
	Observed          int `json:"observed"`
	Stale             int `json:"stale"`
	Invalid           int `json:"invalid"`
}

type status struct {
	Overall       string             `json:"overall"`
	Prerequisites []prerequisite     `json:"prerequisites"`
	Controllers   []controllerStatus `json:"controllers"`
	Summary       stateSummary       `json:"summary"`
	Warnings      []string           `json:"warnings,omitempty"`
}
