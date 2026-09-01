# Installation

The tested publication baseline is ExternalDNS v0.21.0, installed with the
ExternalDNS Helm chart 1.21.1. ExternalDNS and its CRD have independent
lifecycles from labdns.

Install a fresh environment in this order:

1. Install the official v0.21.0 `DNSEndpoint` CRD:

   ```sh
   kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/external-dns/v0.21.0/config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml
   ```

2. Install separately managed ExternalDNS deployments. The two example values
   files deliberately use different label filters and ownership IDs:

   ```sh
   helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
   helm repo update
   helm upgrade --install external-dns-www external-dns/external-dns \
     --version 1.21.1 --namespace dns-system --create-namespace \
     -f examples/external-dns/cloudflare-values.yaml
   helm upgrade --install external-dns-vpn external-dns/external-dns \
     --version 1.21.1 --namespace dns-system \
     -f examples/external-dns/coredns-etcd-values.yaml
   ```

   Create the provider credentials and etcd TLS material independently. Never
   grant the labdns ServiceAccount access to those Secrets.

3. Install labdns. The chart installs only labdns and its `DNSProvider` CRD:

   ```sh
   helm upgrade --install labdns oci://ghcr.io/shednet/charts/labdns \
     --namespace labdns-system --create-namespace --version 0.0.1
   ```

   For source builds, use
   `make build-installer IMG=ghcr.io/shednet/labdns:v0.0.1` and apply
   `dist/install.yaml`. Gateway API watches are opt-in with
   `gatewayAPI.enabled=true`; the Gateway API v1.5.1 CRDs must already exist.

4. Create the logical provider profiles:

   ```sh
   kubectl apply -f examples/dnsproviders.yaml
   ```

5. Annotate sources, for example:

   ```sh
   kubectl apply -f examples/ingress-split-horizon.yaml
   ```

At startup labdns discovers the namespaced
`externaldns.k8s.io/v1alpha1/dnsendpoints` resource before installing any
controller. Missing or incompatible prerequisite APIs cause a clear startup
failure. Readiness reports manager process health; it does not report leadership,
cache synchronization, ExternalDNS, or backend DNS health.

Uninstalling labdns removes neither ExternalDNS nor the official DNSEndpoint
CRD. Remove those independent prerequisites only after their records and
ownership data have been handled intentionally.
