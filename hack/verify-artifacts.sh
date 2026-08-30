#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT

cd "${repo_root}"
bin/kustomize build config/default >"${rendered}"

crd_count="$(grep -c '^kind: CustomResourceDefinition$' "${rendered}" || true)"
if [[ "${crd_count}" != "1" ]]; then
  echo "production render must contain exactly one CRD; found ${crd_count}" >&2
  exit 1
fi

grep -q '^  name: dnsproviders\.labdns\.shednet\.dev$' "${rendered}" || {
  echo "production render is missing the DNSProvider CRD" >&2
  exit 1
}

for forbidden in \
  'dnsproviders/status' \
  '^[[:space:]]*subresources:[[:space:]]*$' \
  '^[[:space:]]*- secrets$' \
  'cloudflare' \
  'externaldns\.k8s\.io' \
  'external-dns' \
  'rfc2136' \
  'etcd'; do
  if grep -Eiq -- "${forbidden}" "${rendered}"; then
    echo "production render contains forbidden pattern: ${forbidden}" >&2
    exit 1
  fi
done
