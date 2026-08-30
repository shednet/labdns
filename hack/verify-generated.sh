#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
snapshot="$(mktemp -d)"
trap 'rm -rf "${snapshot}"' EXIT

cd "${repo_root}"
cp -a api config "${snapshot}/"

make --no-print-directory manifests generate

diff -ru "${snapshot}/api" api
diff -ru "${snapshot}/config" config
