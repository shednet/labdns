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
	"context"
	"fmt"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/shednet/labdns/internal/dnsendpoint"
	"github.com/shednet/labdns/internal/source"
)

type lifecycleOutput interface {
	source.Output
	Advance(context.Context, types.NamespacedName) (time.Duration, error)
}

type lifecycleReconciler struct {
	client.Client
	Recorder       events.EventRecorder
	Output         lifecycleOutput
	GatewayEnabled bool
	Metrics        *Metrics
}

const (
	sourceKindIngress   = source.SourceKindIngress
	sourceKindHTTPRoute = source.SourceKindHTTPRoute
)

func (r *lifecycleReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, reconcileErr error) {
	started := time.Now()
	defer func() { r.Metrics.Observe("lifecycle", started, reconcileErr) }()
	metricKey := request.Namespace + "/" + request.Name
	var object externaldnsv1alpha1.DNSEndpoint
	if err := r.Get(ctx, request.NamespacedName, &object); err != nil {
		if apierrors.IsNotFound(err) {
			r.Metrics.SetEndpoint(metricKey, false, 0)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if object.Labels[dnsendpoint.ManagedByLabel] != dnsendpoint.ManagedByValue {
		r.Metrics.SetEndpoint(metricKey, false, 0)
		return ctrl.Result{}, nil
	}
	pending, err := dnsendpoint.PendingTargetCount(object.Annotations[dnsendpoint.LifecycleAnnotation])
	if err != nil {
		return ctrl.Result{}, r.terminalFailure(&object, err)
	}
	r.Metrics.SetEndpoint(metricKey, true, pending)
	identity := source.Identity{
		Kind:      source.SourceKind(object.Annotations[dnsendpoint.SourceKindAnnotation]),
		Namespace: object.Annotations[dnsendpoint.SourceNamespaceAnnotation],
		Name:      object.Annotations[dnsendpoint.SourceNameAnnotation],
		UID:       types.UID(object.Annotations[dnsendpoint.SourceUIDAnnotation]),
	}
	switch identity.Kind {
	case sourceKindIngress:
		identity.APIVersion = networkingv1.SchemeGroupVersion.String()
	case sourceKindHTTPRoute:
		identity.APIVersion = gatewayv1.GroupVersion.String()
	default:
		return ctrl.Result{}, r.terminalFailure(&object, fmt.Errorf("unsupported source kind %q", identity.Kind))
	}
	provider := object.Labels[dnsendpoint.ProviderLabel]
	if identity.Namespace == "" || identity.Name == "" || identity.UID == "" || provider == "" || object.Namespace != identity.Namespace ||
		object.Labels[dnsendpoint.SourceKeyLabel] != dnsendpoint.SourceKey(identity) || object.Name != dnsendpoint.ObjectName(identity, provider) {
		return ctrl.Result{}, r.terminalFailure(&object, fmt.Errorf("inconsistent managed source identity metadata"))
	}
	retire := identity.Kind == sourceKindHTTPRoute && !r.GatewayEnabled
	if !retire {
		exists, uid, err := r.source(ctx, identity)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !exists {
			retire = true
		} else if uid != identity.UID {
			// The source controllers are also mapped from this DNSEndpoint event. Do not
			// retire records for a newly-created source before its desired state is read.
			return ctrl.Result{}, nil
		}
	}
	if retire {
		if err := r.Output.Apply(ctx, identity, nil); err != nil {
			return ctrl.Result{}, r.outputFailure(&object, err)
		}
	}
	next, err := r.Output.Advance(ctx, request.NamespacedName)
	if err != nil {
		return ctrl.Result{}, r.outputFailure(&object, err)
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

func (r *lifecycleReconciler) source(ctx context.Context, identity source.Identity) (bool, types.UID, error) {
	key := types.NamespacedName{Namespace: identity.Namespace, Name: identity.Name}
	var object client.Object
	switch identity.Kind {
	case sourceKindIngress:
		object = &networkingv1.Ingress{}
	case sourceKindHTTPRoute:
		object = &gatewayv1.HTTPRoute{}
	}
	if err := r.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("verify source %s %s/%s: %w", identity.Kind, identity.Namespace, identity.Name, err)
	}
	return true, object.GetUID(), nil
}

func (r *lifecycleReconciler) failure(object client.Object, err error) error {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, nil, "Warning", "LifecycleFailed", "Reconcile", "%s", err)
	}
	return err
}

func (r *lifecycleReconciler) terminalFailure(object client.Object, err error) error {
	return reconcile.TerminalError(r.failure(object, err))
}

func (r *lifecycleReconciler) outputFailure(object client.Object, err error) error {
	err = r.failure(object, err)
	if dnsendpoint.IsInvalidState(err) {
		return reconcile.TerminalError(err)
	}
	return err
}
