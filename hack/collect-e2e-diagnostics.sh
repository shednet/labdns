#!/usr/bin/env bash

# Copyright 2026 Konstantinos Kalyvas.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

set -euo pipefail

: "${KIND_CLUSTER:?KIND_CLUSTER must identify the isolated E2E cluster}"
: "${E2E_INVOCATION_ID:?E2E_INVOCATION_ID must identify the isolated E2E invocation}"
: "${E2E_DIAGNOSTICS_DIR:?E2E_DIAGNOSTICS_DIR is required}"
if [[ ! "${E2E_INVOCATION_ID}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]]; then
  echo "E2E_INVOCATION_ID must be a path-safe identifier of at most 128 characters." >&2
  exit 1
fi
if [[ ! "${KIND_CLUSTER}" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
  echo "KIND_CLUSTER must be a lowercase Kind-compatible name of at most 63 characters." >&2
  exit 1
fi

marker="/tmp/labdns-kind-owned-${E2E_INVOCATION_ID}"
if [[ ! -f "${marker}" ]]; then
  echo "Refusing diagnostics for '${KIND_CLUSTER}' without its invocation marker." >&2
  exit 1
fi
marker_invocation="$(sed -n '1p' "${marker}")"
marker_cluster="$(sed -n '2p' "${marker}")"
if [[ "${marker_invocation}" != "${E2E_INVOCATION_ID}" || "${marker_cluster}" != "${KIND_CLUSTER}" ]]; then
  echo "Refusing diagnostics: marker does not authorize invocation '${E2E_INVOCATION_ID}' and cluster '${KIND_CLUSTER}'." >&2
  exit 1
fi

mkdir -p "${E2E_DIAGNOSTICS_DIR}"
context="kind-${KIND_CLUSTER}"
kubeconfig="/tmp/labdns-kind-kubeconfig-${E2E_INVOCATION_ID}"
if [[ ! -f "${kubeconfig}" ]]; then
  echo "Refusing diagnostics for '${KIND_CLUSTER}' without its invocation kubeconfig." >&2
  exit 1
fi

"${KIND:-bin/kind}" export logs --name "${KIND_CLUSTER}" "${E2E_DIAGNOSTICS_DIR}/kind" || true
kubectl --kubeconfig "${kubeconfig}" --context "${context}" get nodes -o wide >"${E2E_DIAGNOSTICS_DIR}/nodes.txt" 2>&1 || true
kubectl --kubeconfig "${kubeconfig}" --context "${context}" get all,dnsproviders,dnsendpoints,endpointslices,ingresses -A -o yaml >"${E2E_DIAGNOSTICS_DIR}/objects.yaml" 2>&1 || true
kubectl --kubeconfig "${kubeconfig}" --context "${context}" get events -A --sort-by=.lastTimestamp >"${E2E_DIAGNOSTICS_DIR}/events.txt" 2>&1 || true

for deployment in labdns external-dns-www external-dns-vpn coredns-www coredns-vpn etcd-www etcd-vpn; do
  kubectl --kubeconfig "${kubeconfig}" --context "${context}" logs -n labdns-e2e-system "deployment/${deployment}" --all-containers --prefix \
    >"${E2E_DIAGNOSTICS_DIR}/${deployment}.log" 2>&1 || true
done

for provider in www vpn; do
  pod="$(kubectl --kubeconfig "${kubeconfig}" --context "${context}" get pods -n labdns-e2e-system -l "app=etcd-${provider}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${pod}" ]]; then
    kubectl --kubeconfig "${kubeconfig}" --context "${context}" exec -n labdns-e2e-system "${pod}" -- etcdctl --endpoints=http://127.0.0.1:2379 \
      get /skydns/ --prefix --write-out=json >"${E2E_DIAGNOSTICS_DIR}/etcd-${provider}.json" 2>&1 || true
  fi
done
