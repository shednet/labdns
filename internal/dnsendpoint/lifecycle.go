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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const lifecycleVersion = 1

type lifecycle struct {
	Version int             `json:"version"`
	Pending []pendingTarget `json:"pending"`
}

type pendingTarget struct {
	DNSName    string `json:"dnsName"`
	RecordType string `json:"recordType"`
	Target     string `json:"target"`
	Deadline   string `json:"deadline"`
}

type targetKey struct {
	dnsName, recordType, target string
}

func parseLifecycle(value string) (lifecycle, error) {
	if value == "" {
		return lifecycle{}, errors.New("lifecycle annotation is required")
	}
	var state lifecycle
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return lifecycle{}, fmt.Errorf("parse lifecycle annotation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return lifecycle{}, errors.New("parse lifecycle annotation: multiple JSON values")
		}
		return lifecycle{}, fmt.Errorf("parse lifecycle annotation trailing data: %w", err)
	}
	if state.Version != lifecycleVersion {
		return lifecycle{}, fmt.Errorf("unknown lifecycle version %d", state.Version)
	}
	seen := map[targetKey]struct{}{}
	for _, pending := range state.Pending {
		key := targetKey{pending.DNSName, pending.RecordType, pending.Target}
		if pending.DNSName == "" || pending.RecordType == "" || pending.Target == "" {
			return lifecycle{}, errors.New("lifecycle pending target has an empty identity field")
		}
		parsed, err := time.Parse(time.RFC3339Nano, pending.Deadline)
		if err != nil {
			return lifecycle{}, fmt.Errorf("parse lifecycle deadline %q: %w", pending.Deadline, err)
		}
		_, offset := parsed.Zone()
		if offset != 0 || parsed.UTC().Format(time.RFC3339Nano) != pending.Deadline {
			return lifecycle{}, fmt.Errorf("lifecycle deadline %q is not canonical UTC RFC3339Nano", pending.Deadline)
		}
		if _, found := seen[key]; found {
			return lifecycle{}, fmt.Errorf("duplicate lifecycle pending target %q/%s/%s", pending.DNSName, pending.RecordType, pending.Target)
		}
		seen[key] = struct{}{}
	}
	sortPending(state.Pending)
	if state.Pending == nil {
		state.Pending = []pendingTarget{}
	}
	return state, nil
}

func marshalLifecycle(state lifecycle) (string, error) {
	state.Version = lifecycleVersion
	if state.Pending == nil {
		state.Pending = []pendingTarget{}
	}
	sortPending(state.Pending)
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal lifecycle: %w", err)
	}
	return string(data), nil
}

func sortPending(values []pendingTarget) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].DNSName != values[j].DNSName {
			return values[i].DNSName < values[j].DNSName
		}
		if values[i].RecordType != values[j].RecordType {
			return values[i].RecordType < values[j].RecordType
		}
		return values[i].Target < values[j].Target
	})
}

func deadline(value pendingTarget) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value.Deadline)
	return parsed
}
