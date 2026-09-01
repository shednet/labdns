# Installation

labdns requires Kubernetes 1.35 or newer. The commands below also require
Helm, `kubectl`, and `curl`. The tested publication baseline is ExternalDNS
v0.21.0, installed with the ExternalDNS Helm chart 1.21.1. ExternalDNS and its
CRD have independent lifecycles from labdns.

If ExternalDNS is already publishing your zone, use the
[quick start](quick-start.md) to connect it to labdns and publish an existing
Ingress. This guide covers the complete production topology and installation
order.

Choose a labdns release from the
[release page](https://github.com/shednet/labdns/releases) and export its tag.
The same tag selects the controller image; the chart version omits the leading
`v`:

```sh
export LABDNS_VERSION=vX.Y.Z
export EXAMPLES_BASE="https://raw.githubusercontent.com/shednet/labdns/${LABDNS_VERSION}/examples"
```

Install a fresh environment in this order:

1. Install the official v0.21.0 `DNSEndpoint` CRD:

   ```sh
   kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/external-dns/v0.21.0/config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml
   ```

2. Install separately managed ExternalDNS deployments. The versioned example
   values deliberately use different label filters and ownership IDs. Adapt
   their domains, ownership IDs, providers, and Secret references first.
   Create the referenced provider credentials and, for the CoreDNS example,
   the etcd Service, credentials, TLS material, and publication path before
   installing the deployments. Never grant the labdns ServiceAccount access to
   those Secrets.

   Download and edit the version-matched values files:

   ```sh
   curl -fsSLo external-dns-cloudflare-values.yaml \
     "${EXAMPLES_BASE}/external-dns/cloudflare-values.yaml"
   curl -fsSLo external-dns-coredns-etcd-values.yaml \
     "${EXAMPLES_BASE}/external-dns/coredns-etcd-values.yaml"
   # Edit both files for your providers and publication topology.
   ```

   Once their external prerequisites exist, install the matching ExternalDNS
   releases with the edited values:

   ```sh
   helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
   helm repo update
   helm upgrade --install external-dns-www external-dns/external-dns \
     --version 1.21.1 --namespace dns-system --create-namespace \
     -f external-dns-cloudflare-values.yaml
   helm upgrade --install external-dns-vpn external-dns/external-dns \
     --version 1.21.1 --namespace dns-system \
     -f external-dns-coredns-etcd-values.yaml
   ```

   These values files do not install provider credentials, etcd, CoreDNS, or
   any other DNS backend.

3. Install labdns. The chart installs only labdns and its `DNSProvider` CRD:

   ```sh
   helm upgrade --install labdns oci://ghcr.io/shednet/charts/labdns \
     --namespace labdns-system --create-namespace \
     --version "${LABDNS_VERSION#v}"
   ```

   To render an installer from the source tree, use
   `make build-installer IMG="ghcr.io/shednet/labdns:${LABDNS_VERSION}"` and
   apply `dist/install.yaml`.

   Gateway API watches are opt-in. Before enabling them, install the tested
   Gateway API v1.5.1 CRDs:

   ```sh
   kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
   helm upgrade --install labdns oci://ghcr.io/shednet/charts/labdns \
     --namespace labdns-system --create-namespace \
     --version "${LABDNS_VERSION#v}" --set gatewayAPI.enabled=true
   ```

4. Configure the Node address labels and logical provider profiles for your
   real zones. The [configuration guide](configuration.md#dnsprovider-profiles)
   defines every field and the required Node-label encodings. Download the
   version-matched split-horizon profiles as a starting point, edit them, and
   then apply them:

   ```sh
   curl -fsSLo dnsproviders.yaml "${EXAMPLES_BASE}/dnsproviders.yaml"
   # Edit dnsproviders.yaml for your zones and Node labels before applying it.
   kubectl apply -f dnsproviders.yaml
   ```

5. Opt existing Ingresses or HTTPRoutes into the matching profiles. The
   versioned Ingress example shows the complete annotation and backend shape,
   but assumes that its namespace, Service, EndpointSlices, and Node labels
   already exist:

   ```sh
   curl -fsSLo ingress-split-horizon.yaml \
     "${EXAMPLES_BASE}/ingress-split-horizon.yaml"
   # Adapt the example to an existing workload before applying it.
   kubectl apply -f ingress-split-horizon.yaml
   ```

6. Verify the controller and generated objects. Each managed source produces
   one `DNSEndpoint` per selected provider when at least one target resolves:

   ```sh
   kubectl rollout status deployment/labdns-labdns --namespace labdns-system
   kubectl get dnsendpoints.externaldns.k8s.io --all-namespaces \
     --show-labels
   ```

At startup labdns discovers the namespaced
`externaldns.k8s.io/v1alpha1/dnsendpoints` resource before installing any
controller. Missing or incompatible prerequisite APIs cause a clear startup
failure. When Gateway API support is enabled, startup also requires the
`gateway.networking.k8s.io/v1` HTTPRoute, Gateway, and GatewayClass resources
and the `gateway.networking.k8s.io/v1beta1` ReferenceGrant resource. Readiness
reports manager process health; it does not report leadership, cache
synchronization, ExternalDNS, or backend DNS health.

Uninstalling labdns removes neither ExternalDNS nor the official DNSEndpoint
CRD. Remove those independent prerequisites only after their records and
ownership data have been handled intentionally.
