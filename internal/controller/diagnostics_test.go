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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

func TestDiagnosticSessionSuppressesStableConditionsAndReemitsChanges(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	emitter := newDiagnosticEmitter()
	emitter.now = func() time.Time { return now }
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "apps", UID: "uid-1"}}
	recorder := events.NewFakeRecorder(10)
	ctx := context.Background()

	report := func(message string, success bool) {
		session := emitter.begin(ingress, recorder)
		session.report(ctx, "Warning", "NodeAddressLabelMissing", message)
		session.report(ctx, "Warning", "NodeAddressLabelMissing", message)
		session.finish(success)
	}

	message := "provider vpn Service apps/api Node worker lacks label shednet.dev/vpn-ipv4"
	report(message, true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", message)

	now = now.Add(4 * time.Minute)
	report(message, true)
	assertNoDiagnosticEvent(t, recorder)

	now = now.Add(time.Minute)
	report(message, true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", message)

	changed := message + " (two endpoints affected)"
	report(changed, true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", changed)

	// A successful reconcile without the condition clears its suppression state.
	emitter.begin(ingress, recorder).finish(true)
	report(changed, true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", changed)
}

func TestDiagnosticFailedReconcileDoesNotClearConditions(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	emitter := newDiagnosticEmitter()
	emitter.now = func() time.Time { return now }
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "apps", UID: "uid-1"}}
	recorder := events.NewFakeRecorder(10)
	ctx := context.Background()
	message := "required label is missing"

	first := emitter.begin(ingress, recorder)
	first.report(ctx, "Warning", "NodeAddressLabelMissing", message)
	first.finish(true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", message)

	// The failed pass observes no diagnostic, so the previous condition stays active.
	emitter.begin(ingress, recorder).finish(false)
	now = now.Add(time.Minute)
	retry := emitter.begin(ingress, recorder)
	retry.report(ctx, "Warning", "NodeAddressLabelMissing", message)
	retry.finish(true)
	assertNoDiagnosticEvent(t, recorder)

	// A successful pass clears it, so its next return is emitted immediately.
	emitter.begin(ingress, recorder).finish(true)
	returned := emitter.begin(ingress, recorder)
	returned.report(ctx, "Warning", "NodeAddressLabelMissing", message)
	returned.finish(true)
	assertDiagnosticEvent(t, recorder, "Warning", "NodeAddressLabelMissing", message)
}

func TestDiagnosticDeletionClearsSourceState(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	emitter := newDiagnosticEmitter()
	emitter.now = func() time.Time { return now }
	recorder := events.NewFakeRecorder(10)
	ctx := context.Background()
	diagnostic := diagnosticKey{severity: "Warning", reason: "InvalidSource", message: "bad source"}
	first := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "apps", UID: "uid-old"}}
	emitter.report(ctx, first, recorder, diagnostic)
	assertDiagnosticEvent(t, recorder, "Warning", "InvalidSource", "bad source")

	// A deletion key does not contain a UID, so it must clear every generation.
	emitter.deleteSource(string(sourceKindIngress), "apps", "app")
	if len(emitter.entries) != 0 {
		t.Fatal("deleted source retained diagnostic state")
	}
	second := first.DeepCopy()
	second.UID = "uid-new"
	emitter.report(ctx, second, recorder, diagnostic)
	assertDiagnosticEvent(t, recorder, "Warning", "InvalidSource", "bad source")
}

func TestDiagnosticEmitterEvictsLeastRecentlyUsedSource(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	emitter := newDiagnosticEmitter()
	emitter.capacity = 2
	emitter.now = func() time.Time { return now }
	recorder := events.NewFakeRecorder(10)
	ctx := context.Background()
	diagnostic := diagnosticKey{severity: "Warning", reason: "InvalidSource", message: "bad source"}
	first := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "apps", UID: "first"}}
	second := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "apps", UID: "second"}}
	third := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "third", Namespace: "apps", UID: "third"}}

	emitter.report(ctx, first, recorder, diagnostic)
	emitter.report(ctx, second, recorder, diagnostic)
	emitter.report(ctx, first, recorder, diagnostic) // Refresh first; second becomes least recent.
	emitter.report(ctx, third, recorder, diagnostic)
	for range 3 {
		assertDiagnosticEvent(t, recorder, "Warning", "InvalidSource", "bad source")
	}
	emitter.report(ctx, second, recorder, diagnostic)
	assertDiagnosticEvent(t, recorder, "Warning", "InvalidSource", "bad source")
}

func TestDiagnosticLogsCarrySourceFieldsAndSeverity(t *testing.T) {
	var output bytes.Buffer
	ctx := logr.NewContext(context.Background(), logr.FromSlogHandler(slog.NewJSONHandler(&output, nil)))
	emitter := newDiagnosticEmitter()
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: "uid-7"}}
	recorder := events.NewFakeRecorder(2)
	emitter.report(ctx, ingress, recorder, diagnosticKey{severity: "Warning", reason: "MissingAddress", message: "address label is missing"})
	emitter.report(ctx, ingress, recorder, diagnosticKey{severity: "Normal", reason: "NoBackendReferences", message: "route has no backend references"})
	assertDiagnosticEvent(t, recorder, "Warning", "MissingAddress", "address label is missing")
	assertDiagnosticEvent(t, recorder, "Normal", "NoBackendReferences", "route has no backend references")

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2: %s", len(lines), output.String())
	}
	var warning, normal map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &warning); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &normal); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"msg": "address label is missing", "source_kind": "Ingress", "source_namespace": "apps",
		"source_name": "web", "source_uid": "uid-7", "severity": "Warning", "reason": "MissingAddress",
	} {
		if warning[key] != want {
			t.Errorf("warning log %q = %#v, want %#v", key, warning[key], want)
		}
	}
	if !strings.EqualFold(warning["level"].(string), "WARN") {
		t.Errorf("warning log level = %#v, want WARN", warning["level"])
	}
	if normal["msg"] != "route has no backend references" || !strings.EqualFold(normal["level"].(string), "INFO") {
		t.Errorf("normal log = %#v, want INFO with diagnostic message", normal)
	}
}

func TestDiagnosticFailedConditionHistoryIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	emitter := newDiagnosticEmitter()
	emitter.now = func() time.Time { return now }
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: "uid-9"}}
	ctx := context.Background()
	first := diagnosticKey{severity: "Warning", reason: "ResolutionFailed", message: "first failure"}
	emitter.report(ctx, ingress, nil, first)
	for range diagnosticConditionLimit {
		now = now.Add(time.Second)
		session := emitter.begin(ingress, nil)
		session.report(ctx, "Warning", "ResolutionFailed", now.String())
		session.finish(false)
	}
	state := emitter.entries[diagnosticSourceKeyFor(ingress)].Value.(*diagnosticSourceState)
	if len(state.lastEmitted) != diagnosticConditionLimit {
		t.Fatalf("failed condition history has %d entries", len(state.lastEmitted))
	}
	// Eviction allows the oldest condition to be emitted again before five minutes.
	recorder := events.NewFakeRecorder(1)
	emitter.report(ctx, ingress, recorder, first)
	assertDiagnosticEvent(t, recorder, "Warning", "ResolutionFailed", "first failure")
}

func TestDiagnosticEventNoteIsBoundedAndLogKeepsFullMessage(t *testing.T) {
	var output bytes.Buffer
	ctx := logr.NewContext(context.Background(), logr.FromSlogHandler(slog.NewJSONHandler(&output, nil)))
	emitter := newDiagnosticEmitter()
	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: "uid-8"}}
	recorder := events.NewFakeRecorder(1)
	message := strings.Repeat("missing label on node 🚀; ", 80)
	emitter.report(ctx, ingress, recorder, diagnosticKey{severity: "Warning", reason: "MissingAddress", message: message})

	event := <-recorder.Events
	marker := "Warning MissingAddress "
	markerIndex := strings.Index(event, marker)
	if markerIndex < 0 {
		t.Fatalf("event %q is missing the warning reason", event)
	}
	note := event[markerIndex+len(marker):]
	if len(note) > diagnosticEventNoteLimit {
		t.Fatalf("event note is %d bytes, limit is %d", len(note), diagnosticEventNoteLimit)
	}
	if !utf8.ValidString(note) {
		t.Fatalf("event note is not valid UTF-8: %q", note)
	}
	if !strings.HasSuffix(note, diagnosticEventTruncationSuffix) {
		t.Fatalf("event note %q does not end with %q", note, diagnosticEventTruncationSuffix)
	}

	var logged map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &logged); err != nil {
		t.Fatal(err)
	}
	if logged["msg"] != message {
		t.Fatal("manager log did not retain the full diagnostic message")
	}
}

func assertDiagnosticEvent(t *testing.T, recorder *events.FakeRecorder, severity, reason, message string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, severity+" "+reason) || !strings.Contains(event, message) {
			t.Fatalf("event %q does not contain %s, %s, and %q", event, severity, reason, message)
		}
	case <-time.After(time.Second):
		t.Fatalf("no %s %s event emitted", severity, reason)
	}
}

func assertNoDiagnosticEvent(t *testing.T, recorder *events.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event: %q", event)
	default:
	}
}
