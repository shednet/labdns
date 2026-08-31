# Configuration

labdns accepts only these manager flags:

- `--metrics-addr`
- `--health-addr`
- `--leader-elect`
- `--metrics-secure`
- `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`
- `--enable-http2`
- `--log-level`
- `--enable-gateway-api`

Metrics are disabled by default. The Helm chart supports disabled, ordinary
HTTP, and authenticated HTTPS modes through `metrics.enabled` and
`metrics.secure`. Supplying `metrics.certificate.existingSecret` mounts a
separately managed serving certificate; labdns itself does not read Kubernetes
Secrets. The Kustomize equivalents live in `config/overlays/metrics` and
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

## Node address labels

IPv4 Node-label values are ordinary canonical literals, such as `192.0.2.10`.
Kubernetes label values cannot contain colons, so IPv6 uses a reversible,
label-safe form: prefix the canonical IPv6 text with `v6-` and replace every
colon with a dash. For example, `2001:db8::10` becomes
`v6-2001-db8--10`, and `::1` becomes `v6---1`.

The `v6-` form is accepted only for an `ipSources.ipv6` label. An IPv6 source
rejects a missing prefix, malformed or non-canonical encoding, IPv4, and
IPv4-mapped IPv6. An IPv4 source rejects prefixed values and non-IPv4
addresses. A nonempty invalid value aborts the complete source reconciliation;
previously published output is preserved.

```sh
kubectl label node worker-1 \
  networking.example.com/public-ipv4=192.0.2.10 \
  networking.example.com/public-ipv6=v6-2001-db8--10
```

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
