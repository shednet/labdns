#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 IMAGE" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kustomize_bin="${KUSTOMIZE:-$repo_root/bin/kustomize}"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

cp -a "$repo_root/config" "$temporary/config"
(
  cd "$temporary/config/manager"
  "$kustomize_bin" edit set image "controller=$1"
)
"$kustomize_bin" build "$temporary/config/default"
