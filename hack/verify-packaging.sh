#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
helm_bin="${HELM:-$repo_root/bin/helm}"
kustomize_bin="${KUSTOMIZE:-$repo_root/bin/kustomize}"

if grep -Fq '2>/dev/null || true' Makefile; then
  echo "install/uninstall must not mask Kustomize failures" >&2
  exit 1
fi
from_count="$(grep -c '^FROM ' Dockerfile)"
if [[ "$from_count" != "2" ]] || grep '^FROM ' Dockerfile | grep -Ev '^FROM [^[:space:]@]+:[^[:space:]@]+@sha256:[0-9a-f]{64}([[:space:]]+AS[[:space:]]+[[:alnum:]_-]+)?$' | grep -q .; then
  echo "every Dockerfile base image must use readable tag@sha256 multi-arch pinning" >&2
  exit 1
fi

expected_crd=config/crd/bases/labdns.shednet.dev_dnsproviders.yaml
chart_crd=charts/labdns/crds/labdns.shednet.dev_dnsproviders.yaml
cmp "$expected_crd" "$chart_crd" || {
  echo "chart DNSProvider CRD is stale; run make helm-sync" >&2
  exit 1
}

"$helm_bin" lint charts/labdns
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

"$helm_bin" template labdns charts/labdns --namespace labdns-system >"$tmp_dir/default.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set metrics.enabled=false >"$tmp_dir/disabled.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set metrics.enabled=true --set metrics.secure=false \
  --set metrics.port=8080 >"$tmp_dir/plain.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set metrics.enabled=true --set metrics.secure=true \
  --set metrics.certificate.existingSecret=labdns-metrics-cert >"$tmp_dir/secure.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set gatewayAPI.enabled=false >"$tmp_dir/gateway-disabled.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set gatewayAPI.enabled=true >"$tmp_dir/gateway-enabled.yaml"
"$helm_bin" template labdns charts/labdns --namespace labdns-system \
  --set leaderElection=false >"$tmp_dir/leader-election-disabled.yaml"

for rendered in "$tmp_dir"/*.yaml; do
  if grep -Eq '^  name: dnsendpoints\.externaldns\.k8s\.io$|^  kind: DNSEndpoint$' "$rendered"; then
    echo "production render ships the DNSEndpoint CRD: $rendered" >&2
    exit 1
  fi
  if grep -Eq '^  name: external-dns$|image: .*external-dns' "$rendered"; then
    echo "production render deploys ExternalDNS: $rendered" >&2
    exit 1
  fi
  if grep -Eq 'resources:.*secrets|^- secrets$|  - secrets$' "$rendered"; then
    echo "labdns RBAC grants Secret access: $rendered" >&2
    exit 1
  fi
done

cmp "$tmp_dir/default.yaml" "$tmp_dir/disabled.yaml"
if grep -q -- '--metrics-addr' "$tmp_dir/default.yaml" || grep -q '^kind: Service$' "$tmp_dir/default.yaml"; then
  echo "default/disabled chart render exposes metrics" >&2
  exit 1
fi
grep -q -- '--metrics-addr=:8080' "$tmp_dir/plain.yaml"
grep -q -- '--metrics-secure=false' "$tmp_dir/plain.yaml"
grep -q 'name: http' "$tmp_dir/plain.yaml"
grep -q -- '--metrics-secure=true' "$tmp_dir/secure.yaml"
grep -q -- '--metrics-cert-path=/var/run/labdns/metrics' "$tmp_dir/secure.yaml"
grep -q 'tokenreviews' "$tmp_dir/secure.yaml"
grep -q 'nonResourceURLs: \[/metrics\]' "$tmp_dir/secure.yaml"
if grep -q 'nonResourceURLs: \[/metrics\]' "$tmp_dir/default.yaml" || grep -q 'nonResourceURLs: \[/metrics\]' "$tmp_dir/plain.yaml"; then
  echo "metrics-reader role rendered outside secure metrics mode" >&2
  exit 1
fi
if grep -q 'leader-election' "$tmp_dir/leader-election-disabled.yaml" || grep -q -- '--leader-elect' "$tmp_dir/leader-election-disabled.yaml"; then
  echo "leader-election RBAC or flag rendered while disabled" >&2
  exit 1
fi
if grep -q 'gateway.networking.k8s.io' "$tmp_dir/gateway-disabled.yaml"; then
  echo "Gateway-disabled chart render grants Gateway API access" >&2
  exit 1
fi
grep -q 'gateway.networking.k8s.io' "$tmp_dir/gateway-enabled.yaml"
grep -q -- '--enable-gateway-api' "$tmp_dir/gateway-enabled.yaml"
if find charts/labdns -maxdepth 2 -type f \( -name Chart.lock -o -name '*.tgz' \) -print -quit | grep -q . || grep -q '^dependencies:' charts/labdns/Chart.yaml; then
  echo "labdns chart has a dependency artifact" >&2
  exit 1
fi

"$kustomize_bin" build config/default >"$tmp_dir/kustomize-default.yaml"
"$kustomize_bin" build config/overlays/metrics >"$tmp_dir/kustomize-metrics.yaml"
"$kustomize_bin" build config/overlays/secure-metrics >"$tmp_dir/kustomize-secure.yaml"
"$kustomize_bin" build config/overlays/gateway-api >"$tmp_dir/kustomize-gateway.yaml"
"$kustomize_bin" build examples >"$tmp_dir/examples.yaml"

if grep -q 'gateway.networking.k8s.io' "$tmp_dir/kustomize-default.yaml"; then
  echo "default Kustomize render grants Gateway API access" >&2
  exit 1
fi
grep -q -- '--metrics-secure=false' "$tmp_dir/kustomize-metrics.yaml"
grep -q 'tokenreviews' "$tmp_dir/kustomize-secure.yaml"
grep -q 'nonResourceURLs:' "$tmp_dir/kustomize-secure.yaml"
grep -q '/metrics' "$tmp_dir/kustomize-secure.yaml"
if grep -q 'nonResourceURLs:' "$tmp_dir/kustomize-default.yaml" || grep -q 'nonResourceURLs:' "$tmp_dir/kustomize-metrics.yaml"; then
  echo "Kustomize metrics-reader role rendered outside secure metrics mode" >&2
  exit 1
fi
grep -q -- '--enable-gateway-api' "$tmp_dir/kustomize-gateway.yaml"
grep -q 'kind: DNSProvider' "$tmp_dir/examples.yaml"
grep -q 'labdns.shednet.dev/providers: www,vpn' "$tmp_dir/examples.yaml"

for rendered in "$tmp_dir"/kustomize-*.yaml; do
  if grep -Eq '^  name: dnsendpoints\.externaldns\.k8s\.io$|^kind: DNSEndpoint$|resources:.*secrets|^- secrets$|  - secrets$|image: .*external-dns' "$rendered"; then
    echo "invalid production object or permission in $rendered" >&2
    exit 1
  fi
done

fixture=test/fixtures/external-dns-v0.21.0/dnsendpoints.externaldns.k8s.io.yaml
for production_path in config charts/labdns/crds; do
  if rg -l 'name: dnsendpoints\.externaldns\.k8s\.io' "$production_path" | grep -q .; then
    echo "DNSEndpoint prerequisite CRD leaked into $production_path" >&2
    exit 1
  fi
done
grep -q -- '--coredns-strictly-owned' examples/external-dns/coredns-etcd-values.yaml
grep -q -- '--coredns-prefix=/skydns/' examples/external-dns/coredns-etcd-values.yaml
grep -q 'labelFilter: labdns.shednet.dev/provider=www' examples/external-dns/cloudflare-values.yaml
grep -q 'labelFilter: labdns.shednet.dev/provider=vpn' examples/external-dns/coredns-etcd-values.yaml
test -f "$fixture"

before="$tmp_dir/manager-kustomization.before"
cp config/manager/kustomization.yaml "$before"
KUSTOMIZE="$kustomize_bin" hack/render-kustomize.sh example.invalid/labdns:test >"$tmp_dir/installer.yaml"
cmp "$before" config/manager/kustomization.yaml
grep -q 'image: example.invalid/labdns:test' "$tmp_dir/installer.yaml"

"$helm_bin" pull external-dns \
  --repo https://kubernetes-sigs.github.io/external-dns/ \
  --version 1.21.1 --untar --untardir "$tmp_dir"
external_chart="$tmp_dir/external-dns"
"$helm_bin" template external-dns-www "$external_chart" \
  -f examples/external-dns/cloudflare-values.yaml >"$tmp_dir/external-dns-www.yaml"
"$helm_bin" template external-dns-vpn "$external_chart" \
  -f examples/external-dns/coredns-etcd-values.yaml >"$tmp_dir/external-dns-vpn.yaml"

for rendered in "$tmp_dir/external-dns-www.yaml" "$tmp_dir/external-dns-vpn.yaml"; do
  grep -q 'image: registry.k8s.io/external-dns/external-dns:v0.21.0' "$rendered"
  grep -q -- '- --source=crd' "$rendered"
  grep -q -- '- --policy=sync' "$rendered"
  grep -q -- '- --events' "$rendered"
  grep -q -- '- --interval=1m' "$rendered"
  grep -q -- 'min-event-sync-interval=1s' "$rendered"
done
grep -q -- '- --provider=cloudflare' "$tmp_dir/external-dns-www.yaml"
grep -q -- '- --txt-owner-id=labdns-www-example-com' "$tmp_dir/external-dns-www.yaml"
grep -q -- '- --domain-filter=example.com' "$tmp_dir/external-dns-www.yaml"
grep -q -- '- --label-filter=labdns.shednet.dev/provider=www' "$tmp_dir/external-dns-www.yaml"
grep -q -- '- --provider=coredns' "$tmp_dir/external-dns-vpn.yaml"
grep -q -- '- --txt-owner-id=labdns-vpn-example-com' "$tmp_dir/external-dns-vpn.yaml"
grep -q -- '- --domain-filter=example.com' "$tmp_dir/external-dns-vpn.yaml"
grep -q -- '- --label-filter=labdns.shednet.dev/provider=vpn' "$tmp_dir/external-dns-vpn.yaml"
grep -q -- 'coredns-strictly-owned' "$tmp_dir/external-dns-vpn.yaml"
grep -q -- 'coredns-prefix=/skydns/' "$tmp_dir/external-dns-vpn.yaml"

echo "packaging and example verification passed"
