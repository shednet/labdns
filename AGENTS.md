# labdns contributor guide

This repository is a greenfield replacement for labdns. It is a Kubebuilder
v4 project with module identity `github.com/shednet/labdns`.

## Architecture boundary

labdns reads Ingress and, when enabled, Gateway API sources. It resolves their
backend Services through EndpointSlices and Node labels, then writes one
ExternalDNS `DNSEndpoint` per source and logical `DNSProvider`. Separately
managed ExternalDNS deployments select those objects by label and publish to
their own DNS backends.

Never add direct Cloudflare, etcd, RFC 2136, or other DNS-provider mutation.
labdns must not read provider credentials or Secrets, implement TXT ownership,
or deploy ExternalDNS. The ExternalDNS DNSEndpoint CRD is a prerequisite and
must not be included in production manifests.

## Repository rules

- Preserve every `+kubebuilder:scaffold:*` marker.
- Do not edit `PROJECT`, `config/crd/bases/*`, `config/rbac/role.yaml`, or
  `zz_generated.*` files manually. Use Kubebuilder and Makefile generators.
- Keep tools pinned and repository-local under `bin/`; do not use helper
  binaries or envtest assets from the legacy repository.
- Preserve unrelated worktree changes.
- Keep application logs on `log/slog`, bridged into controller-runtime's logr
  logger.
- Do not add Helm packaging until the packaging stage.

## Layout and checks

- `api/v1alpha1`: DNSProvider API source and generated deepcopy code.
- `cmd/main.go`: manager entry point and scaffold markers.
- `config`: Kustomize sources and generated manifests.
- `hack/boilerplate.go.txt`: generator copyright header.

Run `make manifests generate build lint test verify-generated` before handing
off changes. Tests that need Kubernetes must use this repository's pinned
envtest assets or an explicitly isolated Kind cluster; never use a personal or
shared cluster.
