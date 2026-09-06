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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	externaldnsv1alpha1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

// integrationEnvironment is shared by all ordinary controller integration tests.
// The only test that needs a different API surface starts its own environment:
// TestGatewayDisabledRestartRetiresHTTPRouteWithoutGatewayCRDs.
type integrationEnvironment struct {
	environment *envtest.Environment
	config      *rest.Config
	scheme      *runtime.Scheme
	client      client.Client
}

var (
	sharedIntegrationEnvironment *integrationEnvironment
	sharedIntegrationOnce        sync.Once
	sharedIntegrationErr         error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedIntegrationEnvironment != nil {
		if err := sharedIntegrationEnvironment.environment.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop shared envtest: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

func startSharedIntegrationEnvironment() (*integrationEnvironment, error) {
	gatewayCRDs, err := locateGatewayAPICRD()
	if err != nil {
		return nil, err
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "test", "fixtures", "external-dns-v0.21.0"),
		gatewayCRDs,
	}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		return nil, err
	}
	scheme, err := integrationScheme()
	if err != nil {
		_ = environment.Stop()
		return nil, err
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		_ = environment.Stop()
		return nil, err
	}
	return &integrationEnvironment{environment: environment, config: config, scheme: scheme, client: direct}, nil
}

func integrationScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		labdnsv1alpha1.AddToScheme,
		externaldnsv1alpha1.AddToScheme,
		gatewayv1.Install,
		gatewayv1beta1.Install,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

func sharedIntegration(t *testing.T) (*rest.Config, *runtime.Scheme, client.Client) {
	t.Helper()
	sharedIntegrationOnce.Do(func() {
		sharedIntegrationEnvironment, sharedIntegrationErr = startSharedIntegrationEnvironment()
	})
	if sharedIntegrationErr != nil {
		t.Fatalf("start shared envtest: %v", sharedIntegrationErr)
	}
	return sharedIntegrationEnvironment.config, sharedIntegrationEnvironment.scheme, sharedIntegrationEnvironment.client
}

func startNoGatewayEnvironment(t *testing.T) (*rest.Config, *runtime.Scheme, client.Client) {
	t.Helper()
	environment := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "test", "fixtures", "external-dns-v0.21.0"),
	}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop no-Gateway envtest: %v", err)
		}
	})
	scheme, err := integrationScheme()
	if err != nil {
		t.Fatal(err)
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return config, scheme, direct
}

// cleanupSharedNamespaces removes only names reserved by the integration tests.
// It is intentionally explicit so a developer can run this package against an
// environment containing unrelated objects without deleting those objects.
func cleanupSharedNamespaces(t *testing.T, names ...string) {
	t.Helper()
	_, _, direct := sharedIntegration(t)
	ctx := context.Background()
	for _, name := range names {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := direct.Delete(ctx, namespace); err != nil && !apiNotFound(err) {
			t.Errorf("delete test namespace %q: %v", name, err)
		}
	}
}

func cleanupSharedClusterObjects(t *testing.T, providerNames, ingressClassNames, gatewayClassNames []string) {
	t.Helper()
	_, _, direct := sharedIntegration(t)
	ctx := context.Background()
	for _, name := range providerNames {
		object := &labdnsv1alpha1.DNSProvider{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := direct.Delete(ctx, object); err != nil && !apiNotFound(err) {
			t.Errorf("delete test DNSProvider %q: %v", name, err)
		}
	}
	for _, name := range ingressClassNames {
		object := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := direct.Delete(ctx, object); err != nil && !apiNotFound(err) {
			t.Errorf("delete test IngressClass %q: %v", name, err)
		}
	}
	for _, name := range gatewayClassNames {
		object := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := direct.Delete(ctx, object); err != nil && !apiNotFound(err) {
			t.Errorf("delete test GatewayClass %q: %v", name, err)
		}
	}
}

func cleanupSharedNodes(t *testing.T, names ...string) {
	t.Helper()
	_, _, direct := sharedIntegration(t)
	ctx := context.Background()
	for _, name := range names {
		object := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := direct.Delete(ctx, object); err != nil && !apiNotFound(err) {
			t.Errorf("delete test Node %q: %v", name, err)
		}
	}
}

func apiNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

func startManagedTestManager(t *testing.T, mgr manager.Manager, parent context.Context) func() {
	t.Helper()
	managerContext, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(managerContext) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("manager stopped: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("manager did not stop")
			}
		})
	}
	t.Cleanup(stop)
	if !mgr.GetCache().WaitForCacheSync(managerContext) {
		t.Fatal("cache did not synchronize")
	}
	return stop
}
