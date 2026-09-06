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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	labdnsv1alpha1 "github.com/shednet/labdns/api/v1alpha1"
)

// locateGatewayAPICRD is kept with packaging/dependency validation because it
// resolves a module-owned test asset rather than exercising controller logic.
func locateGatewayAPICRD() (string, error) {
	output, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		return "", err
	}
	moduleDir := strings.TrimSpace(string(output))
	if moduleDir == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(moduleDir, "config", "crd", "standard"), nil
}

func TestExamplesAcceptedByAPIAdmission(t *testing.T) {
	_, _, kubeClient := sharedIntegration(t)
	ctx := context.Background()
	providers := readExampleProviders(t)
	providerNames := make([]string, 0, len(providers))
	for _, provider := range providers {
		provider.Name = "packaging-" + provider.Name
		providerNames = append(providerNames, provider.Name)
		if err := kubeClient.Create(ctx, provider); err != nil {
			t.Fatalf("DNSProvider example %q rejected by API admission: %v", provider.Name, err)
		}
	}
	t.Cleanup(func() {
		cleanupSharedClusterObjects(t, providerNames, nil, nil)
	})

	ingress := readExampleIngress(t)
	ingress.Name = "packaging-" + ingress.Name
	ingress.Namespace = "packaging-examples"
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ingress.Namespace}}
	if err := kubeClient.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupSharedNamespaces(t, ingress.Namespace) })
	if err := kubeClient.Create(ctx, ingress); err != nil {
		t.Fatalf("Ingress example rejected by Kubernetes API: %v", err)
	}
}

func readExampleProviders(t *testing.T) []*labdnsv1alpha1.DNSProvider {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "examples", "dnsproviders.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close DNSProvider example: %v", err)
		}
	}()
	decoder := utilyaml.NewYAMLOrJSONDecoder(file, 4096)
	providers := []*labdnsv1alpha1.DNSProvider{}
	for {
		provider := &labdnsv1alpha1.DNSProvider{}
		if err := decoder.Decode(provider); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode DNSProvider example: %v", err)
		}
		if provider.Name != "" {
			providers = append(providers, provider)
		}
	}
	if len(providers) != 2 {
		t.Fatalf("decoded %d DNSProvider examples, want 2", len(providers))
	}
	return providers
}

func readExampleIngress(t *testing.T) *networkingv1.Ingress {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "examples", "ingress-split-horizon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close Ingress example: %v", err)
		}
	}()
	ingress := &networkingv1.Ingress{}
	if err := utilyaml.NewYAMLOrJSONDecoder(file, 4096).Decode(ingress); err != nil {
		t.Fatalf("decode Ingress example: %v", err)
	}
	return ingress
}
