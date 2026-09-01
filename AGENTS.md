# labdns contributor guide

labdns is a Kubebuilder v4 project with module identity
`github.com/shednet/labdns`.

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
- Keep Makefile-managed Go tools and envtest assets pinned and repository-local
  under `bin/`; do not substitute binaries or assets from another checkout.
  Helm and Kind are the documented exceptions and must be available on `PATH`.
- Preserve unrelated worktree changes.
- Keep application logs on `log/slog`, bridged into controller-runtime's logr
  logger.
- Keep the scaffolded manager health and readiness checks on the current ping
  behavior unless a change explicitly requires different semantics.
- Keep Helm and Kustomize renders equivalent at their architecture and RBAC
  boundaries. Neither path may ship ExternalDNS or its DNSEndpoint CRD.

## Development workflow

- Do normal maintainer work on a feature branch and open a pull request against
  `main`. Do not push directly to `main` except for an explicitly authorized
  exceptional workflow.
- When a ready, non-draft maintainer pull request targets `main`, successful CI
  for its current head enables GitHub squash auto-merge. A newer head must pass
  CI again before it is eligible.
- Releases are separate from the pull-request workflow. Run `just release`
  from `main` and follow its review and push prompt; do not use the release
  procedure as a shortcut for ordinary changes.

## Restricted environments

- Resolve the active Go cache paths with `go env GOCACHE GOMODCACHE`. If the
  sandbox cannot write a cache needed by the task, request narrow access to
  that path. Prefer the existing shared caches; use task-specific directories
  such as `GOCACHE=/tmp/labdns-go-build` only when narrow access is unavailable.
- Request module-cache write access only when uncached modules must be
  downloaded. Do not repurpose `HOME` or another global environment variable
  to work around cache permissions.
- Dependency, tool, and envtest downloads require network access. Request the
  environment's network permission when they are not already cached instead of
  replacing pinned versions or sourcing artifacts from another repository.
- `gh` operations and Git remote operations such as fetch, pull, push, and
  remote tag checks also require network access. Distinguish a denied network
  operation from an authentication failure before changing credentials.
- If `.git` is read-only, request the appropriate filesystem permission for
  branch, index, commit, tag, or other Git metadata changes. Do not bypass the
  sandbox or copy the repository elsewhere.
- Inspect the worktree before editing and preserve unrelated tracked and
  untracked changes throughout implementation, generation, and validation.

## Layout and checks

- `api/v1alpha1`: DNSProvider API source and generated deepcopy code.
- `cmd/main.go`: manager entry point and scaffold markers.
- `config`: Kustomize sources and generated manifests.
- `charts/labdns`: standalone labdns chart with no dependencies.
- `examples`: logical provider and separately managed ExternalDNS examples.
- `hack/boilerplate.go.txt`: generator copyright header.

Run `make manifests generate build lint test check-generated
check-packaging` before handing off changes. Tests that need Kubernetes must use
this repository's pinned envtest assets or the explicitly isolated Kind cluster
described in `test/e2e/AGENTS.md`; never use a personal or shared cluster.
