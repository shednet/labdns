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
	"errors"
	"fmt"
	"maps"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
	"github.com/shednet/labdns/internal/source"
)

const legacyIngressClassAnnotation = "kubernetes.io/ingress.class"

type IngressReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Output   source.Output
	Resolver source.Resolver
}

func (r *IngressReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var ingress networkingv1.Ingress
	if err := r.Get(ctx, request.NamespacedName, &ingress); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.Output.Apply(ctx, source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: request.Namespace, Name: request.Name}, nil)
		}
		return ctrl.Result{}, err
	}
	annotations, err := r.ingressAnnotations(ctx, &ingress)
	if err != nil {
		return ctrl.Result{}, err
	}
	parsed, err := source.ParseAnnotations(annotations)
	if err != nil {
		r.warning(&ingress, "InvalidAnnotations", err.Error())
		return ctrl.Result{}, err
	}
	identity := source.Identity{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress", Namespace: ingress.Namespace, Name: ingress.Name, UID: ingress.UID}
	if !parsed.Enabled || len(parsed.Providers) == 0 {
		if err := r.Output.Apply(ctx, identity, nil); err != nil {
			r.warning(&ingress, "DNSEndpointWriteFailed", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	projection, err := source.IngressProjection(&ingress, parsed.Hostnames, func(reason, message string) { r.warning(&ingress, reason, message) })
	if err != nil {
		r.warning(&ingress, "InvalidSource", err.Error())
		return ctrl.Result{}, err
	}
	providers, err := loadProviders(ctx, r.Client, parsed.Providers)
	if err != nil {
		return ctrl.Result{}, err
	}
	publications, err := r.Resolver.Publications(ctx, projection, providers, parsed, func(reason, message string) {
		r.warning(&ingress, reason, message)
	})
	if err != nil {
		r.warning(&ingress, "ResolutionFailed", err.Error())
		return ctrl.Result{}, err
	}
	if err := r.Output.Apply(ctx, identity, publications); err != nil {
		r.warning(&ingress, "DNSEndpointWriteFailed", err.Error())
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *IngressReconciler) ingressAnnotations(ctx context.Context, ingress *networkingv1.Ingress) (map[string]string, error) {
	className := ""
	if ingress.Spec.IngressClassName != nil {
		className = *ingress.Spec.IngressClassName
	} else {
		className = ingress.Annotations[legacyIngressClassAnnotation]
	}
	if className == "" {
		return source.MergeAnnotations(ingress.Annotations), nil
	}
	var class networkingv1.IngressClass
	if err := r.Get(ctx, client.ObjectKey{Name: className}, &class); err != nil {
		return nil, fmt.Errorf("get IngressClass %s: %w", className, err)
	}
	return source.MergeAnnotations(class.Annotations, ingress.Annotations), nil
}

func (r *IngressReconciler) warning(object client.Object, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, nil, "Warning", reason, "Reconcile", "%s", message)
	}
}

type HTTPRouteReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Output   source.Output
	Resolver source.Resolver
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, request.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.Output.Apply(ctx, source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: request.Namespace, Name: request.Name}, nil)
		}
		return ctrl.Result{}, err
	}
	annotations, err := r.routeAnnotations(ctx, &route)
	if err != nil {
		r.warning(&route, "AmbiguousParents", err.Error())
		return ctrl.Result{}, err
	}
	parsed, err := source.ParseAnnotations(annotations)
	if err != nil {
		r.warning(&route, "InvalidAnnotations", err.Error())
		return ctrl.Result{}, err
	}
	identity := source.Identity{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute", Namespace: route.Namespace, Name: route.Name, UID: route.UID}
	if !parsed.Enabled || len(parsed.Providers) == 0 {
		if err := r.Output.Apply(ctx, identity, nil); err != nil {
			r.warning(&route, "DNSEndpointWriteFailed", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	projection, err := source.HTTPRouteProjection(ctx, r.Client, &route, parsed.Hostnames, func(reason, message string) { r.warning(&route, reason, message) })
	if err != nil {
		r.warning(&route, "ResolutionFailed", err.Error())
		return ctrl.Result{}, err
	}
	providers, err := loadProviders(ctx, r.Client, parsed.Providers)
	if err != nil {
		return ctrl.Result{}, err
	}
	publications, err := r.Resolver.Publications(ctx, projection, providers, parsed, func(reason, message string) {
		r.warning(&route, reason, message)
	})
	if err != nil {
		r.warning(&route, "ResolutionFailed", err.Error())
		return ctrl.Result{}, err
	}
	if err := r.Output.Apply(ctx, identity, publications); err != nil {
		r.warning(&route, "DNSEndpointWriteFailed", err.Error())
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *HTTPRouteReconciler) routeAnnotations(ctx context.Context, route *gatewayv1.HTTPRoute) (map[string]string, error) {
	chains := []map[string]string{}
	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && string(*ref.Group) != gatewayv1.GroupName || ref.Kind != nil && string(*ref.Kind) != "Gateway" {
			continue
		}
		namespace := route.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}
		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: string(ref.Name)}, &gateway); err != nil {
			return nil, fmt.Errorf("get Gateway %s/%s: %w", namespace, ref.Name, err)
		}
		var class gatewayv1.GatewayClass
		if err := r.Get(ctx, client.ObjectKey{Name: string(gateway.Spec.GatewayClassName)}, &class); err != nil {
			return nil, fmt.Errorf("get GatewayClass %s: %w", gateway.Spec.GatewayClassName, err)
		}
		chains = append(chains, source.MergeAnnotations(class.Annotations, gateway.Annotations, route.Annotations))
	}
	if len(chains) == 0 {
		return source.MergeAnnotations(route.Annotations), nil
	}
	for _, chain := range chains[1:] {
		if !maps.Equal(chains[0], chain) {
			return nil, errors.New("supported Gateway parent chains resolve to different relevant annotations")
		}
	}
	return chains[0], nil
}

func (r *HTTPRouteReconciler) warning(object client.Object, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, nil, "Warning", reason, "Reconcile", "%s", message)
	}
}

func loadProviders(ctx context.Context, reader client.Reader, names []string) ([]*labdnsv1alpha1.DNSProvider, error) {
	result := make([]*labdnsv1alpha1.DNSProvider, 0, len(names))
	for _, name := range names {
		var provider labdnsv1alpha1.DNSProvider
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, &provider); err != nil {
			if apierrors.IsNotFound(err) {
				// A deleted profile is an authoritative deselection. The output layer
				// retires only that profile's targets using its stored delay.
				continue
			}
			return nil, fmt.Errorf("get DNSProvider %s: %w", name, err)
		}
		result = append(result, &provider)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
