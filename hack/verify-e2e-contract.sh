#!/usr/bin/env bash

# Copyright 2026 Konstantinos Kalyvas.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

set -euo pipefail

stack=test/e2e/stack.yaml
contract=test/e2e/contract_test.go

[[ "$(grep -c '^kind: Deployment$' "${stack}")" -eq 6 ]]
[[ "$(grep -c 'image: registry.k8s.io/external-dns/external-dns:v0.21.0' "${stack}")" -eq 2 ]]
[[ "$(grep -c 'image: registry.k8s.io/etcd:3.5.21-0' "${stack}")" -eq 2 ]]
[[ "$(grep -c 'image: registry.k8s.io/coredns/coredns:v1.12.1' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--source=crd' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--policy=sync' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--registry=txt' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--events' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--interval=1m' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--min-event-sync-interval=1s' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--coredns-strictly-owned' "${stack}")" -eq 2 ]]
[[ "$(grep -c -- '--coredns-prefix=/skydns/' "${stack}")" -eq 2 ]]
grep -q -- '--label-filter=labdns.shednet.dev/provider=www' "${stack}"
grep -q -- '--label-filter=labdns.shednet.dev/provider=vpn' "${stack}"
grep -q -- '--txt-owner-id=labdns-e2e-www' "${stack}"
grep -q -- '--txt-owner-id=labdns-e2e-vpn' "${stack}"
[[ "$(grep -c 'fieldPath: status.hostIP' "${stack}")" -eq 6 ]]
[[ "$(grep -c 'value: http://$(ETCD_HOST):2379' "${stack}")" -eq 2 ]]
[[ "$(grep -c 'endpoint http://{$ETCD_HOST}:2379' "${stack}")" -eq 2 ]]

if grep -Eqi 'image:[[:space:]]+[^[:space:]]+:latest([[:space:]]|$)' "${stack}"; then
  echo "E2E stack contains an unpinned latest image" >&2
  exit 1
fi
grep -q 'normalTimeout[[:space:]]*=[[:space:]]*10 \* time.Second' "${contract}"
grep -q 'deletionDelay+normalTimeout' "${contract}"
grep -q 'netip.MustParseAddr' "${contract}"
grep -q 'g.Expect(actual).To(Equal(expected\[recordType\]))' "${contract}"
grep -q 'g.Expect(service.Owner).To(Equal(owner))' "${contract}"
grep -q 'runIn(ctx, repoRoot, "docker", "build"' "${contract}"
grep -Fq 'test "$${KIND_EXPERIMENTAL_PROVIDER}" = "docker"' Makefile
grep -Fq "{{.DockerRootDir}}|{{.OSType}}|{{.Architecture}}|{{.ServerVersion}}" Makefile
grep -q 'KIND_EXPERIMENTAL_PROVIDER: docker' .github/workflows/e2e.yml
grep -q 'KUBECONFIG: /tmp/labdns-kind-kubeconfig-' .github/workflows/e2e.yml
grep -Fq -- '--kubeconfig "/tmp/labdns-kind-kubeconfig-$${E2E_INVOCATION_ID}"' Makefile
grep -Fq 'KUBECONFIG="$$kubeconfig"' Makefile
grep -Fq 'marker="/tmp/labdns-kind-owned-${E2E_INVOCATION_ID}"' hack/collect-e2e-diagnostics.sh
if grep -R -q 'KIND_OWNERSHIP_FILE' Makefile .github/workflows/e2e.yml hack/collect-e2e-diagnostics.sh hack/verify-e2e-safety.sh test/e2e; then
  echo "E2E marker path must not be caller-controlled" >&2
  exit 1
fi
grep -q 'ClientConfigLoadingRules{ExplicitPath: kubeconfig}' "${contract}"
grep -q 'ConfigOverrides{CurrentContext: kubeContext}' "${contract}"
grep -q '"--kubeconfig", kubeconfig, "--context", kubeContext' "${contract}"
grep -q '"--kubeconfig", kubeconfig, "--kube-context", kubeContext' "${contract}"
if grep -n 'kubectl ' hack/collect-e2e-diagnostics.sh | grep -v -- '--kubeconfig "${kubeconfig}" --context "${context}"' | grep -q .; then
  echo "diagnostic kubectl access is not bound to the invocation kubeconfig and context" >&2
  exit 1
fi
grep -q 'Raw: foreignRaw' "${contract}"

if find config charts -type f -exec grep -l 'kind: DNSEndpoint' {} + | grep -q .; then
  echo "production manifests ship the prerequisite DNSEndpoint API" >&2
  exit 1
fi
if grep -R -E 'test/e2e/stack.yaml|external-dns/external-dns:v0.21.0|registry.k8s.io/etcd:3.5.21-0' config charts >/dev/null; then
  echo "production packaging references the isolated E2E stack" >&2
  exit 1
fi

echo "E2E publication contract verification passed"
