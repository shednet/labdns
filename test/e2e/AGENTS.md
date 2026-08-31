# labdns E2E contributor guide

The E2E suite must run only against the disposable Kind cluster created from
`test/e2e/kind.yaml`. It is a dual-stack Kubernetes 1.35 cluster with multiple
workers. Never point these tests at a personal, shared, or pre-existing
cluster. Cleanup must remove the entire named Kind cluster, even after failure,
while preserving diagnostics long enough for CI artifact upload.
These tests require a real Docker CLI and daemon. Keep
`KIND_EXPERIMENTAL_PROVIDER=docker`; other Kind providers are rejected.
Setup verifies Docker Engine identity fields rather than accepting a merely
command-compatible runtime.
Local runs export diagnostics and then clean up automatically. CI alone sets
`E2E_KEEP_CLUSTER_ON_FAILURE=true` so its next workflow step can collect more
diagnostics; the workflow's unconditional cleanup then deletes the exact named
cluster. Never use that opt-out without an equally reliable caller cleanup.

Every run has a collision-resistant cluster name and invocation ID. Local
`make test-e2e` generates both once and passes them unchanged through setup,
tests, diagnostics, and cleanup. CI defines a deterministic pair from its run
ID and attempt at job scope. A caller-supplied `KIND_CLUSTER` must likewise be
unique to that invocation; never reuse a name or invocation ID.

Setup refuses an already existing exact cluster name before writing anything.
It then writes a temporary marker containing both the invocation ID and cluster
name before Kind creation, which permits cleanup if creation fails partway.
Cleanup requires the same pair and refuses a missing or mismatched marker. The
marker proves authorization by this invocation, not the runtime identity of a
cluster; safety therefore depends on the invariant that names and invocation
IDs are unique and never reused. This prevents an old marker from authorizing
standalone cleanup of a foreign same-name cluster.

Each invocation uses `/tmp/labdns-kind-kubeconfig-<invocation-id>` and the
exact `kind-<cluster-name>` context. Kind creation, the Go suite, Helm,
kubectl, diagnostics, and controller-runtime must all bind both values rather
than reading an ambient current context. Only marker-authorized cleanup may
remove that deterministic kubeconfig, and a failed cluster deletion retains
both files for recovery.

## Boundary under test

Tests must cross the real publication path:

```text
Ingress / HTTPRoute
  -> labdns
  -> provider-labeled DNSEndpoint
  -> separate ExternalDNS v0.21.0 deployments
  -> two independent etcd backends
  -> two independent CoreDNS deployments
```

The `www` and `vpn` paths must use distinct label filters, stable owner IDs,
etcd backends, and CoreDNS instances. labdns never receives DNS-provider or
etcd credentials and must not be granted Secret access. The official pinned
DNSEndpoint CRD is test setup, not a production labdns manifest.

## Assertion rules

- Compare normalized A and AAAA answers as exact complete sets.
- Store IPv6 Node-label values in the documented `v6-` label-safe encoding;
  DNS and DNSEndpoint assertions must compare the decoded canonical address.
- Assert exact ownership fields and provider-label isolation.
- Reject every missing, extra, stale, foreign-provider, or duplicate target.
- Exercise Node-label, EndpointSlice placement/readiness, delayed per-target
  removal, immediate source deletion, and restart recovery across the real
  boundary.
- Preserve foreign-owner etcd entries exactly.
- Normal event-driven publication must converge within 10 seconds. Deletion
  may use the configured grace plus 10 seconds; do not hide a slow normal path
  behind a broad polling timeout.
- Do not replace boundary assertions with unit parsing tests or substring
  checks such as “contains an IPv6 address.”

Use pinned images and tools only. Do not contact Cloudflare or use real
Cloudflare credentials; Cloudflare behavior remains unit/envtest coverage.
