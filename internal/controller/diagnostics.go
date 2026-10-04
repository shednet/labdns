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
	"container/list"
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	diagnosticRepeatInterval = 5 * time.Minute
	diagnosticSourceCapacity = 4096
	diagnosticConditionLimit = 128
	diagnosticEventNoteLimit = 1024
)

const diagnosticEventTruncationSuffix = "… (see manager logs)"

type diagnosticKey struct {
	severity string
	reason   string
	message  string
}

type diagnosticSourceKey struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

type diagnosticSourceState struct {
	key         diagnosticSourceKey
	lastEmitted map[diagnosticKey]time.Time
}

type diagnosticEmitter struct {
	mu       sync.Mutex
	capacity int
	interval time.Duration
	now      func() time.Time
	entries  map[diagnosticSourceKey]*list.Element
	lru      *list.List
}

func newDiagnosticEmitter() *diagnosticEmitter {
	return &diagnosticEmitter{
		capacity: diagnosticSourceCapacity,
		interval: diagnosticRepeatInterval,
		now:      time.Now,
		entries:  make(map[diagnosticSourceKey]*list.Element),
		lru:      list.New(),
	}
}

type diagnosticSession struct {
	emitter  *diagnosticEmitter
	object   client.Object
	recorder events.EventRecorder
	key      diagnosticSourceKey
	mu       sync.Mutex
	seen     map[diagnosticKey]struct{}
}

type diagnosticContextKey struct{}

func (e *diagnosticEmitter) begin(object client.Object, recorder events.EventRecorder) *diagnosticSession {
	return &diagnosticSession{
		emitter:  e,
		object:   object,
		recorder: recorder,
		key:      diagnosticSourceKeyFor(object),
		seen:     make(map[diagnosticKey]struct{}),
	}
}

func (s *diagnosticSession) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, diagnosticContextKey{}, s)
}

func (s *diagnosticSession) report(ctx context.Context, severity, reason, message string) {
	key := diagnosticKey{severity: severity, reason: reason, message: message}
	s.mu.Lock()
	if _, found := s.seen[key]; found {
		s.mu.Unlock()
		return
	}
	s.seen[key] = struct{}{}
	s.mu.Unlock()

	s.emitter.report(ctx, s.object, s.recorder, key)
}

func (s *diagnosticSession) finish(success bool) {
	if !success {
		return
	}
	s.mu.Lock()
	seen := make(map[diagnosticKey]struct{}, len(s.seen))
	for key := range s.seen {
		seen[key] = struct{}{}
	}
	s.mu.Unlock()
	s.emitter.clearMissing(s.key, seen)
}

func reportSourceDiagnostic(ctx context.Context, object client.Object, recorder events.EventRecorder, emitter *diagnosticEmitter, severity, reason, message string) {
	if session, ok := ctx.Value(diagnosticContextKey{}).(*diagnosticSession); ok {
		session.report(ctx, severity, reason, message)
		return
	}
	if emitter == nil {
		emitter = newDiagnosticEmitter()
	}
	emitter.report(ctx, object, recorder, diagnosticKey{severity: severity, reason: reason, message: message})
}

func (e *diagnosticEmitter) report(ctx context.Context, object client.Object, recorder events.EventRecorder, diagnostic diagnosticKey) {
	key := diagnosticSourceKeyFor(object)
	now := e.now()
	shouldEmit := false

	e.mu.Lock()
	state := e.state(key)
	last, found := state.lastEmitted[diagnostic]
	if !found || now.Sub(last) >= e.interval {
		state.lastEmitted[diagnostic] = now
		if len(state.lastEmitted) > diagnosticConditionLimit {
			evictOldestCondition(state.lastEmitted, diagnostic)
		}
		shouldEmit = true
	}
	e.mu.Unlock()

	if !shouldEmit {
		return
	}
	if recorder != nil {
		recorder.Eventf(object, nil, diagnostic.severity, diagnostic.reason, "Reconcile", "%s", eventDiagnosticMessage(diagnostic.message))
	}
	logDiagnostic(ctx, key, diagnostic)
}

func eventDiagnosticMessage(message string) string {
	if len(message) <= diagnosticEventNoteLimit {
		return message
	}
	maxPrefixBytes := diagnosticEventNoteLimit - len(diagnosticEventTruncationSuffix)
	var prefix strings.Builder
	for _, r := range message {
		if prefix.Len()+utf8.RuneLen(r) > maxPrefixBytes {
			break
		}
		prefix.WriteRune(r)
	}
	return prefix.String() + diagnosticEventTruncationSuffix
}

func evictOldestCondition(conditions map[diagnosticKey]time.Time, protected diagnosticKey) {
	var oldest diagnosticKey
	var oldestAt time.Time
	first := true
	for key, emittedAt := range conditions {
		if key == protected {
			continue
		}
		if first || emittedAt.Before(oldestAt) {
			oldest, oldestAt, first = key, emittedAt, false
		}
	}
	if !first {
		delete(conditions, oldest)
	}
}

func (e *diagnosticEmitter) state(key diagnosticSourceKey) *diagnosticSourceState {
	if element := e.entries[key]; element != nil {
		e.lru.MoveToFront(element)
		return element.Value.(*diagnosticSourceState)
	}
	capacity := e.capacity
	if capacity <= 0 {
		capacity = diagnosticSourceCapacity
	}
	for e.lru.Len() >= capacity {
		oldest := e.lru.Back()
		if oldest == nil {
			break
		}
		state := oldest.Value.(*diagnosticSourceState)
		delete(e.entries, state.key)
		e.lru.Remove(oldest)
	}
	state := &diagnosticSourceState{key: key, lastEmitted: make(map[diagnosticKey]time.Time)}
	e.entries[key] = e.lru.PushFront(state)
	return state
}

func (e *diagnosticEmitter) clearMissing(key diagnosticSourceKey, seen map[diagnosticKey]struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	element := e.entries[key]
	if element == nil {
		return
	}
	e.lru.MoveToFront(element)
	state := element.Value.(*diagnosticSourceState)
	for diagnostic := range state.lastEmitted {
		if _, found := seen[diagnostic]; !found {
			delete(state.lastEmitted, diagnostic)
		}
	}
	if len(state.lastEmitted) == 0 {
		delete(e.entries, key)
		e.lru.Remove(element)
	}
}

func (e *diagnosticEmitter) deleteSource(kind, namespace, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, element := range e.entries {
		if key.kind == kind && key.namespace == namespace && key.name == name {
			delete(e.entries, key)
			e.lru.Remove(element)
		}
	}
}

func diagnosticSourceKeyFor(object client.Object) diagnosticSourceKey {
	kind := object.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		switch object.(type) {
		case *networkingv1.Ingress:
			kind = string(sourceKindIngress)
		case *gatewayv1.HTTPRoute:
			kind = string(sourceKindHTTPRoute)
		case *corev1.Service:
			kind = "Service"
		}
	}
	return diagnosticSourceKey{kind: kind, namespace: object.GetNamespace(), name: object.GetName(), uid: object.GetUID()}
}

func logDiagnostic(ctx context.Context, sourceKey diagnosticSourceKey, diagnostic diagnosticKey) {
	logger := slog.New(logr.ToSlogHandler(ctrl.LoggerFrom(ctx)))
	attributes := []any{
		"source_kind", sourceKey.kind,
		"source_namespace", sourceKey.namespace,
		"source_name", sourceKey.name,
		"source_uid", string(sourceKey.uid),
		"severity", diagnostic.severity,
		"reason", diagnostic.reason,
	}
	if diagnostic.severity == "Warning" {
		logger.WarnContext(ctx, diagnostic.message, attributes...)
		return
	}
	logger.InfoContext(ctx, diagnostic.message, attributes...)
}
