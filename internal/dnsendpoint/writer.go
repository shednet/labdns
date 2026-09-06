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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/shednet/labdns/internal/source"
)

const (
	ManagedByLabel            = "app.kubernetes.io/managed-by"
	ProviderLabel             = source.AnnotationPrefix + "provider"
	SourceKeyLabel            = source.AnnotationPrefix + "source-key"
	SourceKindAnnotation      = source.AnnotationPrefix + "source-kind"
	SourceNamespaceAnnotation = source.AnnotationPrefix + "source-namespace"
	SourceNameAnnotation      = source.AnnotationPrefix + "source-name"
	SourceUIDAnnotation       = source.AnnotationPrefix + "source-uid"
	DeletionDelayAnnotation   = source.AnnotationPrefix + "deletion-delay"
	LifecycleAnnotation       = source.AnnotationPrefix + "lifecycle"
	ManagedByValue            = "labdns"
	dnsEndpointResource       = "dnsendpoints"
	externalDNSPrefix         = "external-dns.alpha.kubernetes.io/"
)

type Clock interface{ Now() time.Time }
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Writer struct {
	Client client.Client
	Clock  Clock
}

type invalidStateError struct{ err error }

func (e invalidStateError) Error() string { return e.err.Error() }
func (e invalidStateError) Unwrap() error { return e.err }

func invalidState(err error) error {
	if err == nil || IsInvalidState(err) {
		return err
	}
	return invalidStateError{err: err}
}

// IsInvalidState reports whether err describes deterministic corruption in a
// managed DNSEndpoint rather than a transient Kubernetes API failure.
func IsInvalidState(err error) bool {
	var target invalidStateError
	return errors.As(err, &target)
}

func NewWriter(kubeClient client.Client) *Writer {
	return &Writer{Client: kubeClient, Clock: realClock{}}
}

func identityValue(identity source.Identity) string {
	return strings.Join([]string{identity.APIVersion, string(identity.Kind), identity.Namespace, identity.Name}, "\x00")
}

func hash16(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func SourceKey(identity source.Identity) string { return hash16(identityValue(identity)) }

func ObjectName(identity source.Identity, provider string) string {
	hash := hash16(identityValue(identity) + "\x00" + provider)
	prefix := sanitize(strings.Join([]string{string(identity.Kind), identity.Name, provider}, "-"))
	const maxPrefix = 253 - 1 - 16
	if len(prefix) > maxPrefix {
		prefix = strings.Trim(prefix[:maxPrefix], "-")
	}
	if prefix == "" {
		prefix = "source"
	}
	return prefix + "-" + hash
}

func sanitize(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	lastDash := false
	for _, char := range value {
		valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
		if valid {
			result.WriteRune(char)
			lastDash = false
		} else if !lastDash && result.Len() != 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func (m *Writer) now() time.Time {
	if m.Clock == nil {
		return time.Now().UTC()
	}
	return m.Clock.Now().UTC()
}

// Apply writes all selected providers and retires any previously selected provider.
func (m *Writer) Apply(ctx context.Context, identity source.Identity, publications []source.Publication) error {
	var existing externaldnsv1alpha1.DNSEndpointList
	if err := m.Client.List(ctx, &existing, client.InNamespace(identity.Namespace), client.MatchingLabels{ManagedByLabel: ManagedByValue, SourceKeyLabel: SourceKey(identity)}); err != nil {
		return fmt.Errorf("list generated DNSEndpoints: %w", err)
	}
	byProvider := make(map[string]*externaldnsv1alpha1.DNSEndpoint, len(existing.Items))
	for i := range existing.Items {
		object := &existing.Items[i]
		state, err := parseLifecycle(object.Annotations[LifecycleAnnotation])
		if err != nil {
			return invalidState(fmt.Errorf("DNSEndpoint %s/%s: %w", object.Namespace, object.Name, err))
		}
		currentTargets := endpointTargets(normalizeEndpoints(object.Spec.Endpoints))
		for _, item := range state.Pending {
			if _, found := currentTargets[targetKey{item.DNSName, item.RecordType, item.Target}]; !found {
				return invalidState(fmt.Errorf("DNSEndpoint %s/%s lifecycle pending target %q/%s/%s is absent from spec", object.Namespace, object.Name, item.DNSName, item.RecordType, item.Target))
			}
		}
		value := object.Annotations[DeletionDelayAnnotation]
		delay, err := time.ParseDuration(value)
		if value == "" || err != nil || delay < 0 {
			return invalidState(fmt.Errorf("DNSEndpoint %s/%s has missing or invalid stored deletion delay %q", object.Namespace, object.Name, value))
		}
		provider := object.Labels[ProviderLabel]
		if provider == "" || object.Name != ObjectName(identity, provider) {
			return invalidState(fmt.Errorf("DNSEndpoint %s/%s has inconsistent provider identity metadata", object.Namespace, object.Name))
		}
		if _, found := byProvider[provider]; found {
			return invalidState(fmt.Errorf("multiple generated DNSEndpoints found for provider %q", provider))
		}
		byProvider[provider] = object
	}
	desired := make(map[string]source.Publication, len(publications))
	for _, publication := range publications {
		if _, found := desired[publication.ProviderName]; found {
			return fmt.Errorf("duplicate publication for provider %q", publication.ProviderName)
		}
		if publication.ProviderName == "" || publication.DeletionDelay < 0 {
			return fmt.Errorf("invalid publication for provider %q", publication.ProviderName)
		}
		desired[publication.ProviderName] = publication
	}
	providers := make([]string, 0, len(desired)+len(byProvider))
	seen := map[string]struct{}{}
	for provider := range desired {
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	for provider := range byProvider {
		if _, ok := seen[provider]; !ok {
			providers = append(providers, provider)
		}
	}
	sort.Strings(providers)
	for _, provider := range providers {
		publication, selected := desired[provider]
		if err := m.applyOne(ctx, identity, provider, publication, selected); err != nil {
			return err
		}
	}
	return nil
}

func (m *Writer) applyOne(ctx context.Context, identity source.Identity, provider string, publication source.Publication, selected bool) error {
	key := types.NamespacedName{Namespace: identity.Namespace, Name: ObjectName(identity, provider)}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current externaldnsv1alpha1.DNSEndpoint
		err := m.Client.Get(ctx, key, &current)
		if apierrors.IsNotFound(err) {
			if !selected || len(publication.Records) == 0 {
				return nil
			}
			created, buildErr := m.build(identity, provider, publication, nil)
			if buildErr != nil {
				return buildErr
			}
			if err := m.Client.Create(ctx, created); apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(schema.GroupResource{Group: externaldnsv1alpha1.GroupVersion.Group, Resource: dnsEndpointResource}, key.Name, err)
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		if current.Labels[ManagedByLabel] != ManagedByValue || current.Labels[SourceKeyLabel] != SourceKey(identity) || current.Labels[ProviderLabel] != provider {
			return invalidState(fmt.Errorf("refusing to overwrite DNSEndpoint %s/%s with incompatible ownership metadata", key.Namespace, key.Name))
		}
		updated, buildErr := m.build(identity, provider, publication, &current)
		if buildErr != nil {
			return buildErr
		}
		if updated == nil {
			return m.Client.Delete(ctx, &current, client.Preconditions{ResourceVersion: &current.ResourceVersion})
		}
		if equivalent(&current, updated) {
			return nil
		}
		updated.ResourceVersion = current.ResourceVersion
		return m.Client.Update(ctx, updated)
	})
}

func (m *Writer) build(identity source.Identity, provider string, publication source.Publication, current *externaldnsv1alpha1.DNSEndpoint) (*externaldnsv1alpha1.DNSEndpoint, error) {
	now := m.now()
	state := lifecycle{Version: lifecycleVersion, Pending: []pendingTarget{}}
	storedDelay := publication.DeletionDelay
	if current != nil {
		var err error
		state, err = parseLifecycle(current.Annotations[LifecycleAnnotation])
		if err != nil {
			return nil, invalidState(err)
		}
		value := current.Annotations[DeletionDelayAnnotation]
		storedDelay, err = time.ParseDuration(value)
		if value == "" || err != nil || storedDelay < 0 {
			return nil, invalidState(fmt.Errorf("missing or invalid stored deletion delay %q", value))
		}
	}
	desiredRecords := normalizeRecords(publication.Records)
	desiredTargets := recordTargets(desiredRecords)
	currentRecords := []*endpoint.Endpoint{}
	if current != nil {
		currentRecords = normalizeEndpoints(current.Spec.Endpoints)
	}
	currentTargets := endpointTargets(currentRecords)
	pending := map[targetKey]pendingTarget{}
	for _, item := range state.Pending {
		key := targetKey{item.DNSName, item.RecordType, item.Target}
		if _, found := currentTargets[key]; !found {
			return nil, invalidState(fmt.Errorf("lifecycle pending target %q/%s/%s is absent from spec", item.DNSName, item.RecordType, item.Target))
		}
		pending[key] = item
	}
	for key := range currentTargets {
		if _, active := desiredTargets[key]; active {
			delete(pending, key)
			continue
		}
		if _, found := pending[key]; !found {
			pending[key] = pendingTarget{DNSName: key.dnsName, RecordType: key.recordType, Target: key.target, Deadline: now.Add(storedDelay).UTC().Format(time.RFC3339Nano)}
		}
	}
	for key := range desiredTargets {
		delete(pending, key)
	}
	for key, item := range pending {
		if !deadline(item).After(now) {
			delete(pending, key)
		}
	}
	combined := combineRecords(desiredRecords, currentRecords, pending)
	if len(combined) == 0 {
		return nil, nil
	}
	state.Pending = make([]pendingTarget, 0, len(pending))
	for _, item := range pending {
		state.Pending = append(state.Pending, item)
	}
	lifecycleValue, err := marshalLifecycle(state)
	if err != nil {
		return nil, err
	}
	result := &externaldnsv1alpha1.DNSEndpoint{TypeMeta: metav1.TypeMeta{APIVersion: externaldnsv1alpha1.GroupVersion.String(), Kind: "DNSEndpoint"}, ObjectMeta: metav1.ObjectMeta{Name: ObjectName(identity, provider), Namespace: identity.Namespace}, Spec: externaldnsv1alpha1.DNSEndpointSpec{Endpoints: combined}}
	if current != nil {
		result.ObjectMeta = *current.ObjectMeta.DeepCopy()
		result.TypeMeta = current.TypeMeta
		result.Status = current.Status
	}
	if result.Labels == nil {
		result.Labels = map[string]string{}
	}
	result.Labels[ManagedByLabel] = ManagedByValue
	result.Labels[ProviderLabel] = provider
	result.Labels[SourceKeyLabel] = SourceKey(identity)
	if result.Annotations == nil {
		result.Annotations = map[string]string{}
	}
	if len(desiredRecords) != 0 {
		for key := range result.Annotations {
			if strings.HasPrefix(key, externalDNSPrefix) {
				delete(result.Annotations, key)
			}
		}
		maps.Copy(result.Annotations, publication.MetadataAnnotations)
	}
	result.Annotations[SourceKindAnnotation] = string(identity.Kind)
	result.Annotations[SourceNamespaceAnnotation] = identity.Namespace
	result.Annotations[SourceNameAnnotation] = identity.Name
	if identity.UID != "" {
		result.Annotations[SourceUIDAnnotation] = string(identity.UID)
	}
	if len(desiredRecords) != 0 {
		result.Annotations[DeletionDelayAnnotation] = publication.DeletionDelay.String()
	}
	result.Annotations[LifecycleAnnotation] = lifecycleValue
	result.OwnerReferences = withoutSourceOwner(result.OwnerReferences, identity)
	return result, nil
}

func withoutSourceOwner(references []metav1.OwnerReference, identity source.Identity) []metav1.OwnerReference {
	result := references[:0]
	for _, reference := range references {
		if reference.APIVersion == identity.APIVersion && reference.Kind == string(identity.Kind) && reference.Name == identity.Name {
			continue
		}
		result = append(result, reference)
	}
	return result
}

func normalizeRecords(records []source.Record) []*endpoint.Endpoint {
	byRecord := make(map[string]*endpoint.Endpoint, len(records))
	for _, record := range records {
		targets := append([]string(nil), record.Targets...)
		sort.Strings(targets)
		targets = compact(targets)
		properties := make(endpoint.ProviderSpecific, 0, len(record.ProviderSpecific))
		for _, property := range record.ProviderSpecific {
			properties = append(properties, endpoint.ProviderSpecificProperty{Name: property.Name, Value: property.Value})
		}
		sort.Slice(properties, func(i, j int) bool {
			if properties[i].Name == properties[j].Name {
				return properties[i].Value < properties[j].Value
			}
			return properties[i].Name < properties[j].Name
		})
		dnsName := strings.ToLower(strings.TrimSuffix(record.DNSName, "."))
		key := dnsName + "\x00" + string(record.RecordType)
		if existing := byRecord[key]; existing != nil {
			existing.Targets = append(existing.Targets, targets...)
			continue
		}
		byRecord[key] = &endpoint.Endpoint{DNSName: dnsName, RecordType: string(record.RecordType), Targets: targets, RecordTTL: endpoint.TTL(record.TTL), ProviderSpecific: properties}
	}
	result := make([]*endpoint.Endpoint, 0, len(byRecord))
	for _, record := range byRecord {
		result = append(result, record)
	}
	return normalizeEndpoints(result)
}

func normalizeEndpoints(values []*endpoint.Endpoint) []*endpoint.Endpoint {
	result := make([]*endpoint.Endpoint, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		copy := *value
		copy.Targets = append(endpoint.Targets(nil), value.Targets...)
		sort.Strings(copy.Targets)
		copy.Targets = compact(copy.Targets)
		copy.ProviderSpecific = append(endpoint.ProviderSpecific(nil), value.ProviderSpecific...)
		sort.Slice(copy.ProviderSpecific, func(i, j int) bool {
			if copy.ProviderSpecific[i].Name == copy.ProviderSpecific[j].Name {
				return copy.ProviderSpecific[i].Value < copy.ProviderSpecific[j].Value
			}
			return copy.ProviderSpecific[i].Name < copy.ProviderSpecific[j].Name
		})
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DNSName == result[j].DNSName {
			return result[i].RecordType < result[j].RecordType
		}
		return result[i].DNSName < result[j].DNSName
	})
	return result
}

func compact[T comparable](values []T) []T {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
func recordTargets(records []*endpoint.Endpoint) map[targetKey]struct{} {
	return endpointTargets(records)
}
func endpointTargets(records []*endpoint.Endpoint) map[targetKey]struct{} {
	result := map[targetKey]struct{}{}
	for _, record := range records {
		for _, target := range record.Targets {
			result[targetKey{record.DNSName, record.RecordType, target}] = struct{}{}
		}
	}
	return result
}

func combineRecords(desired, current []*endpoint.Endpoint, pending map[targetKey]pendingTarget) []*endpoint.Endpoint {
	byRecord := map[string]*endpoint.Endpoint{}
	key := func(dnsName, recordType string) string { return dnsName + "\x00" + recordType }
	for _, record := range desired {
		copy := *record
		copy.Targets = append(endpoint.Targets(nil), record.Targets...)
		byRecord[key(record.DNSName, record.RecordType)] = &copy
	}
	currentByRecord := map[string]*endpoint.Endpoint{}
	for _, record := range current {
		currentByRecord[key(record.DNSName, record.RecordType)] = record
	}
	for target, item := range pending {
		recordKey := key(target.dnsName, target.recordType)
		record := byRecord[recordKey]
		if record == nil {
			if old := currentByRecord[recordKey]; old != nil {
				copy := *old
				copy.Targets = nil
				record = &copy
			} else {
				record = &endpoint.Endpoint{DNSName: item.DNSName, RecordType: item.RecordType}
			}
			byRecord[recordKey] = record
		}
		record.Targets = append(record.Targets, target.target)
	}
	result := make([]*endpoint.Endpoint, 0, len(byRecord))
	for _, record := range byRecord {
		sort.Strings(record.Targets)
		record.Targets = compact(record.Targets)
		if len(record.Targets) > 0 {
			result = append(result, record)
		}
	}
	return normalizeEndpoints(result)
}

func equivalent(current, desired *externaldnsv1alpha1.DNSEndpoint) bool {
	return reflect.DeepEqual(normalizeEndpoints(current.Spec.Endpoints), normalizeEndpoints(desired.Spec.Endpoints)) &&
		reflect.DeepEqual(current.Labels, desired.Labels) &&
		reflect.DeepEqual(current.Annotations, desired.Annotations) &&
		reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences)
}

// Advance expires pending targets without inventing desired state.
func (m *Writer) Advance(ctx context.Context, key types.NamespacedName) (time.Duration, error) {
	var next time.Duration
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current externaldnsv1alpha1.DNSEndpoint
		if err := m.Client.Get(ctx, key, &current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if current.Labels[ManagedByLabel] != ManagedByValue {
			return nil
		}
		value := current.Annotations[DeletionDelayAnnotation]
		delay, err := time.ParseDuration(value)
		if value == "" || err != nil || delay < 0 {
			return invalidState(fmt.Errorf("missing or invalid stored deletion delay %q", value))
		}
		state, err := parseLifecycle(current.Annotations[LifecycleAnnotation])
		if err != nil {
			return invalidState(err)
		}
		now := m.now()
		currentTargets := endpointTargets(normalizeEndpoints(current.Spec.Endpoints))
		kept := make([]pendingTarget, 0, len(state.Pending))
		expired := map[targetKey]struct{}{}
		for _, item := range state.Pending {
			if _, found := currentTargets[targetKey{item.DNSName, item.RecordType, item.Target}]; !found {
				return invalidState(fmt.Errorf("lifecycle pending target %q/%s/%s is absent from spec", item.DNSName, item.RecordType, item.Target))
			}
			remaining := deadline(item).Sub(now)
			if remaining <= 0 {
				expired[targetKey{item.DNSName, item.RecordType, item.Target}] = struct{}{}
			} else {
				kept = append(kept, item)
				if next == 0 || remaining < next {
					next = remaining
				}
			}
		}
		if len(expired) == 0 {
			return nil
		}
		endpoints := normalizeEndpoints(current.Spec.Endpoints)
		for _, record := range endpoints {
			targets := record.Targets[:0]
			for _, target := range record.Targets {
				if _, ok := expired[targetKey{record.DNSName, record.RecordType, target}]; !ok {
					targets = append(targets, target)
				}
			}
			record.Targets = targets
		}
		filtered := endpoints[:0]
		for _, record := range endpoints {
			if len(record.Targets) > 0 {
				filtered = append(filtered, record)
			}
		}
		if len(filtered) == 0 {
			return m.Client.Delete(ctx, &current, client.Preconditions{ResourceVersion: &current.ResourceVersion})
		}
		state.Pending = kept
		lifecycleValue, marshalErr := marshalLifecycle(state)
		if marshalErr != nil {
			return marshalErr
		}
		current.Spec.Endpoints = filtered
		current.Annotations[LifecycleAnnotation] = lifecycleValue
		return m.Client.Update(ctx, &current)
	})
	return next, err
}
