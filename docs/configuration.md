# Configuration

labdns watches Kubernetes sources, resolves the Nodes serving their backend
Services, and creates one `DNSEndpoint` for each source and selected
`DNSProvider`. A source is managed only when its resolved annotations include
both `labdns.shednet.dev/enabled: "true"` and at least one provider name.

The [quick start](quick-start.md) shows the minimum ExternalDNS, `DNSProvider`,
Node-label, and Ingress configuration needed to publish a first record.

## Sources and annotation inheritance

Ingress watching is always enabled, although each source must still opt in with
annotations. labdns uses hosts and Service backends from `spec.rules`, and
associates TLS-only hosts with `spec.defaultBackend` when one exists.
`labdns.shednet.dev/hostnames` replaces the declared hosts and applies all
discovered Service backends to every override hostname. Resource backends other
than Services are ignored.

Ingress annotations inherit from the selected `IngressClass`, with annotations
on the Ingress taking precedence. Selection uses `spec.ingressClassName`, or
`kubernetes.io/ingress.class` when the field is unset. If neither is present,
only Ingress annotations apply.

HTTPRoute watching is opt-in with `--enable-gateway-api`, the
`config/overlays/gateway-api` Kustomize overlay, or the Helm value
`gatewayAPI.enabled=true`. Hostnames come from `spec.hostnames`, and all
supported Service backendRefs are applied to each hostname. A cross-namespace
Service reference is used only when a `ReferenceGrant` in the Service namespace
permits the route namespace to reference that Service. Unsupported or
ungranted backends are skipped with a Warning event.

HTTPRoute annotations inherit in this order: `GatewayClass`, `Gateway`, then
`HTTPRoute`. Only Gateway parentRefs are considered. When a route has multiple
supported parents, every parent chain must resolve to identical labdns and
ExternalDNS annotations; otherwise reconciliation fails with an
`AmbiguousParents` Warning event. A route with no supported parent uses only
its own annotations.

Only annotations under `labdns.shednet.dev/` and
`external-dns.alpha.kubernetes.io/` participate in inheritance. At each level,
the presence of a key overrides the inherited value, including when the value
is empty.

## Source annotations

| Annotation                            | Value and behavior                                                                                                                                                      |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `labdns.shednet.dev/enabled`          | Boolean parsed with Go boolean syntax. It must resolve to `true` for publication.                                                                                       |
| `labdns.shednet.dev/providers`        | Comma-separated `DNSProvider` names. Empty entries and duplicates are removed. Names must be valid DNS subdomains of at most 63 characters. At least one is required.   |
| `labdns.shednet.dev/hostnames`        | Optional comma-separated hostname replacement. Values are normalized to lowercase without a trailing dot; valid `*.` wildcards are accepted only in the leftmost label. |
| `labdns.shednet.dev/ttl`              | Optional record TTL override, an integer from 1 through 2147483647 seconds.                                                                                             |
| `labdns.shednet.dev/address-families` | Optional comma-separated `ipv4` and/or `ipv6` selection. By default, every family configured by the provider is used.                                                   |
| `labdns.shednet.dev/deletion-delay`   | Optional non-negative Go duration, such as `30s` or `5m`, overriding the provider default.                                                                              |

Invalid annotation values fail the complete source reconciliation and preserve
the previously published output. A selected hostname outside a provider's
zones, or a hostname with no resolved targets for a requested family, produces
no record for that provider and family.

## DNSProvider profiles

`DNSProvider` is cluster-scoped. Its name is the logical provider selected by
source annotations and is written to the generated object's
`labdns.shednet.dev/provider` label. Each profile contains:

- `zones`: one or more normalized, lowercase DNS suffixes without trailing
  dots. A provider publishes only matching hostnames.
- `ipSources`: at least one of `ipv4.nodeLabel` or `ipv6.nodeLabel`, identifying
  the Node label from which that record family's targets are read.
- `recordDefaults.ttl`: the default record TTL; defaults to 300 seconds.
- `recordDefaults.deletionDelay`: how long removed targets remain published;
  defaults to `60s` and may be `0s` for immediate retirement.
- `providerSpecific.defaults`: fixed ExternalDNS provider-specific name/value
  properties added to every record for this profile.
- `providerSpecific.annotationKeys`: source annotation keys explicitly allowed
  to override or add provider-specific properties. Other source annotations do
  not become provider-specific record properties.

All resolved `external-dns.alpha.kubernetes.io/*` annotations are also copied
to the generated `DNSEndpoint` metadata. The allowlist controls which of them
are additionally encoded as record-level provider-specific properties.

For example:

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
  providerSpecific:
    defaults:
      - name: external-dns.alpha.kubernetes.io/cloudflare-proxied
        value: "false"
    annotationKeys:
      - external-dns.alpha.kubernetes.io/cloudflare-proxied
```

See [`examples/dnsproviders.yaml`](../examples/dnsproviders.yaml) for a dual
stack, split-horizon example.

## Target resolution

For each Service backend, labdns lists its EndpointSlices and considers
endpoints that name a Node and are not explicitly unready. It reads the
provider's configured address label from those Nodes, canonicalizes and
deduplicates the results, and emits A and/or AAAA records. Missing and empty
labels contribute no target. A missing Service or Node, a failed read, or a
nonempty invalid address label fails the complete source reconciliation and
preserves its previous output.

### Node address labels

IPv4 Node-label values are ordinary canonical literals, such as `192.0.2.10`.
Kubernetes label values cannot contain colons, so IPv6 uses a reversible,
label-safe form: prefix the canonical IPv6 text with `v6-` and replace every
colon with a dash. For example, `2001:db8::10` becomes
`v6-2001-db8--10`, and `::1` becomes `v6---1`.

The `v6-` form is accepted only for an `ipSources.ipv6` label. An IPv6 source
rejects a missing prefix, malformed or non-canonical encoding, IPv4, and
IPv4-mapped IPv6. An IPv4 source rejects prefixed values and non-IPv4
addresses.

```sh
kubectl label node worker-1 \
  networking.example.com/public-ipv4=192.0.2.10 \
  networking.example.com/public-ipv6=v6-2001-db8--10
```

## Manager and metrics

labdns accepts only these manager flags:

- `--metrics-addr`
- `--health-addr`
- `--leader-elect`
- `--metrics-secure`
- `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`
- `--enable-http2`
- `--log-level`
- `--enable-gateway-api`

The manager binary defaults are `--metrics-addr=0`, `--health-addr=:8081`,
secure metrics, HTTP/2 disabled, leader election disabled, Gateway API
disabled, and `--log-level=info`. The standard Helm and Kustomize deployments
enable leader election. Canonical log levels are `debug`, `info`, `warn`, and
`error`; `warning` is also accepted as an alias for `warn`.

Metrics are disabled by default. The Helm chart supports disabled, ordinary
HTTP, and authenticated HTTPS modes through `metrics.enabled` and
`metrics.secure`. Supplying `metrics.certificate.existingSecret` mounts a
separately managed serving certificate. The metrics server reads the mounted
certificate files, but labdns does not retrieve Secret objects through the
Kubernetes API and must never receive provider credentials.

The Kustomize equivalents live in `config/overlays/metrics` and
`config/overlays/secure-metrics`.

Secure mode creates an unbound `metrics-reader` ClusterRole granting only
`GET /metrics`. Bind that role to the separately managed Prometheus or scraper
identity. It is intentionally not bound to the labdns ServiceAccount. The
labdns ServiceAccount receives only the token-review and subject-access-review
permissions needed to authenticate and authorize incoming scrape requests.

Operational metrics are:

- `labdns_reconcile_duration_seconds`, by bounded controller name.
- `labdns_reconcile_errors_total`, by bounded controller name.
- `labdns_managed_sources`, by source kind.
- `labdns_generated_dnsendpoints`.
- `labdns_pending_target_deletions`.

Metrics never contain source names or hostnames. Counts are reconstructed from
initial cache watch events after manager startup and are not authoritative DNS
state.

## Publication isolation

The `www` and `vpn` profiles in `examples/dnsproviders.yaml` can publish the
same FQDN with different Node-label targets. Each generated DNSEndpoint has a
provider label consumed by exactly one ExternalDNS deployment. Although the
source example declares Cloudflare proxying, only `www` allowlists that
property; `vpn` ignores it.

Every ExternalDNS deployment must retain its own stable owner ID, matching
provider label filter, and matching domain filter. The CoreDNS example uses
strict ownership and the `/skydns/` prefix. Its authentication and TLS
environment variables belong to the independently administered ExternalDNS
deployment.

## Target retirement

When a target disappears, labdns keeps it in the `DNSEndpoint` until the
effective deletion delay expires. Reappearing targets are restored immediately.
Disabling or deleting a source and removing or deleting a selected provider use
the same retirement path; the `DNSEndpoint` is deleted after its final target
expires. The delay stored on the existing object governs already-pending
targets, so changing a source or provider delay does not rewrite their
deadlines.

Generated lifecycle and source-identity annotations are internal controller
state and must not be edited. Invalid or inconsistent lifecycle state causes
reconciliation to stop without replacing the existing output.
