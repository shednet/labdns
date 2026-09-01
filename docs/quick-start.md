# Quick start

This guide connects an existing ExternalDNS installation to labdns and
publishes an existing Ingress through one logical DNS provider.

You need Kubernetes 1.35 or newer, Helm, `kubectl`, and:

- an ExternalDNS deployment with working provider credentials;
- a DNS zone managed by that deployment; and
- an existing Ingress whose Service has ready EndpointSlices naming its serving
  Nodes.

The examples use a logical provider named `www`, the zone `example.com`, and an
IPv4 Node label. Replace those values with your own.

## Install labdns

Install the official ExternalDNS v0.21.0 `DNSEndpoint` CRD, then install a
published labdns release. Applying the CRD is safe when the same version is
already installed.

```sh
export LABDNS_VERSION=v0.0.2

kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/external-dns/v0.21.0/config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml

helm upgrade --install labdns oci://ghcr.io/shednet/charts/labdns \
  --namespace labdns-system --create-namespace \
  --version "${LABDNS_VERSION#v}" --wait

kubectl rollout status deployment/labdns-labdns --namespace labdns-system
```

Gateway API sources are optional and require additional CRDs and RBAC. Start
with an Ingress here; enable Gateway API later through the
[installation guide](installation.md) if needed.

## Configure ExternalDNS for labdns

Run a dedicated ExternalDNS deployment for each logical labdns `DNSProvider`.
Keep its existing provider, credentials, ownership registry, and policy, and
configure these values:

```yaml
sources:
- crd
domainFilters:
- example.com
labelFilter: labdns.shednet.dev/provider=www
triggerLoopOnEvent: true
```

The `crd` source makes ExternalDNS read `DNSEndpoint` objects. The label filter
isolates this deployment to the matching logical provider; the domain filter
limits its publication zone. `triggerLoopOnEvent` reduces publication latency
but is optional. Keep the deployment's ownership identifier stable and unique.

If an existing deployment also processes unrelated sources or zones, create a
separate deployment from the same provider configuration instead of adding the
labdns settings to it. Do not grant labdns access to ExternalDNS credentials.

See the complete
[`www` example](../examples/external-dns/cloudflare-values.yaml) for the tested
ExternalDNS Helm chart 1.21.1 shape. Apply the equivalent changes through the
same mechanism that manages your existing ExternalDNS deployment.

## Create a logical provider

Choose a Node label for the addresses this DNS view should publish. Label every
Node that may serve the Ingress backend, using each Node's real publishable
address:

```sh
kubectl label node worker-1 \
  networking.example.com/public-ipv4=192.0.2.10
```

Create a cluster-scoped `DNSProvider` using the same logical name and zone as
the ExternalDNS label and domain filters:

```yaml
apiVersion: labdns.shednet.dev/v1alpha1
kind: DNSProvider
metadata:
  name: www
spec:
  zones:
  - name: example.com
  ipSources:
    ipv4:
      nodeLabel: networking.example.com/public-ipv4
  recordDefaults:
    ttl: 300
    deletionDelay: 60s
```

Save that manifest as `dnsprovider.yaml` and apply it:

```sh
kubectl apply -f dnsprovider.yaml
```

## Enable an existing Ingress

Annotate an Ingress whose hostname is inside the configured zone:

```sh
kubectl annotate ingress application --namespace applications \
  labdns.shednet.dev/enabled=true \
  labdns.shednet.dev/providers=www
```

labdns follows the Ingress backend Service to its ready EndpointSlices, finds
the serving Nodes, and reads the configured address label from those Nodes.

## Verify publication

Confirm that labdns created the provider-labelled `DNSEndpoint`:

```sh
kubectl get dnsendpoints.externaldns.k8s.io --all-namespaces \
  --selector=labdns.shednet.dev/provider=www --output=yaml
```

The object should contain the Ingress hostname, an A record, and the labelled
Node addresses. Then check the dedicated ExternalDNS deployment's logs and
query the hostname through your DNS backend to confirm publication.

For multiple DNS views, IPv6, annotation inheritance, record overrides,
retirement behavior, and metrics, continue with the
[configuration reference](configuration.md).
