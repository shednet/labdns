#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

fake_kind="$temporary/kind"
fake_test="$temporary/fail-test"
fake_docker="$temporary/docker"
fake_kubectl="$temporary/kubectl"
state="$temporary/cluster"
log="$temporary/kind.log"
kubectl_log="$temporary/kubectl.log"
kind_kubeconfig_log="$temporary/kind-kubeconfig.log"
test_kubeconfig_log="$temporary/test-kubeconfig.log"
suite="$temporary/suite_test.go"
ambient_kubeconfig="$temporary/ambient-kubeconfig"
touch "$suite"
printf '%s\n' ambient-sentinel >"$ambient_kubeconfig"

test_invocations=(invocation-a partial-invocation current-invocation delete-failure get-failure failure-invocation ci-invocation diagnostic-invocation)
cleanup_test_paths() {
  for invocation in "${test_invocations[@]}"; do
    rm -f "/tmp/labdns-kind-owned-${invocation}" "/tmp/labdns-kind-kubeconfig-${invocation}"
  done
}
cleanup_test_paths
trap 'cleanup_test_paths; rm -rf "$temporary"' EXIT

cat >"$fake_kind" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

operation="$1"
subcommand="${2:-}"
case "$operation/$subcommand" in
	  get/clusters)
	    if [[ "${FAKE_KIND_GET_STATUS:-0}" != 0 ]]; then
	      exit "$FAKE_KIND_GET_STATUS"
	    fi
	    if [[ -f "$FAKE_KIND_STATE" ]]; then cat "$FAKE_KIND_STATE"; fi
    ;;
	  create/cluster)
	    shift 2
	    name=""
	    kubeconfig=""
	    while (($#)); do
	      case "$1" in
	        --name) name="$2"; shift 2 ;;
	        --kubeconfig) kubeconfig="$2"; shift 2 ;;
	        *) shift ;;
	      esac
	    done
	    [[ -n "$kubeconfig" ]]
	    printf 'create %s\n' "$name" >>"$FAKE_KIND_LOG"
	    printf 'create %s\n' "$kubeconfig" >>"$FAKE_KIND_KUBECONFIG_LOG"
	    printf '%s\n' "$name" >"$FAKE_KIND_STATE"
	    printf 'fake isolated kubeconfig\n' >"$kubeconfig"
	    exit "${FAKE_KIND_CREATE_STATUS:-0}"
    ;;
	  delete/cluster)
	    shift 2
	    name=""
	    kubeconfig=""
	    while (($#)); do
	      case "$1" in
	        --name) name="$2"; shift 2 ;;
	        --kubeconfig) kubeconfig="$2"; shift 2 ;;
	        *) shift ;;
	      esac
	    done
	    [[ -n "$name" && -n "$kubeconfig" ]]
	    printf 'delete %s\n' "$name" >>"$FAKE_KIND_LOG"
	    printf 'delete %s\n' "$kubeconfig" >>"$FAKE_KIND_KUBECONFIG_LOG"
	    if [[ "${FAKE_KIND_DELETE_STATUS:-0}" != 0 ]]; then
	      exit "$FAKE_KIND_DELETE_STATUS"
	    fi
	    if [[ -f "$FAKE_KIND_STATE" ]] && [[ "$(cat "$FAKE_KIND_STATE")" == "$name" ]]; then
      rm -f "$FAKE_KIND_STATE"
    fi
    ;;
  export/logs)
    shift 2
    [[ "$1" == "--name" ]]
    printf 'export %s\n' "$2" >>"$FAKE_KIND_LOG"
    ;;
  *) exit 64 ;;
esac
EOF
cat >"$fake_test" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${KUBECONFIG:-}" >"$FAKE_TEST_KUBECONFIG_LOG"
exit 23
EOF
cat >"$fake_docker" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == info && "${2:-}" == --format ]]
if [[ "${FAKE_DOCKER_SCHEMA:-engine}" == engine ]]; then
  printf '/var/lib/docker|linux|x86_64|29.7.2\n'
else
  printf '<no value>|linux|amd64|<no value>\n'
fi
EOF
cat >"$fake_kubectl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_KUBECTL_LOG"
EOF
chmod 0755 "$fake_kind" "$fake_test" "$fake_docker" "$fake_kubectl"

run_make() {
  PATH="$temporary:$PATH" KUBECONFIG="$ambient_kubeconfig" \
    FAKE_KIND_STATE="$state" FAKE_KIND_LOG="$log" FAKE_KUBECTL_LOG="$kubectl_log" \
	    FAKE_KIND_KUBECONFIG_LOG="$kind_kubeconfig_log" \
    FAKE_TEST_KUBECONFIG_LOG="$test_kubeconfig_log" FAKE_DOCKER_SCHEMA="${FAKE_DOCKER_SCHEMA:-engine}" \
    make -s -C "$repo_root" "$@"
}

# A non-Docker Kind provider is rejected before touching cluster state.
if run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=wrong-provider E2E_INVOCATION_ID=wrong-provider \
	  KIND_EXPERIMENTAL_PROVIDER=unsupported >"$temporary/provider.out" 2>&1; then
  echo "setup accepted a non-Docker Kind provider" >&2
  exit 1
fi
grep -q 'requires KIND_EXPERIMENTAL_PROVIDER=docker' "$temporary/provider.out"
[[ ! -e "$state" ]]

# A failed cluster inventory is never interpreted as authoritative absence.
if FAKE_KIND_GET_STATUS=42 run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=get-failure E2E_INVOCATION_ID=get-failure >"$temporary/get-setup.out" 2>&1; then
  echo "setup continued after Kind cluster inventory failed" >&2
  exit 1
fi
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-get-failure && ! -e /tmp/labdns-kind-kubeconfig-get-failure ]]

# A command-compatible engine replacement without Docker Engine identity
# fields is rejected even when its info command exits successfully.
if FAKE_DOCKER_SCHEMA=compatible run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=wrong-engine E2E_INVOCATION_ID=wrong-engine \
	  >"$temporary/engine.out" 2>&1; then
  echo "setup accepted a non-Docker engine identity" >&2
  exit 1
fi
grep -q 'requires Docker Engine identity fields' "$temporary/engine.out" || {
  cat "$temporary/engine.out" >&2
  exit 1
}
[[ ! -e "$state" ]]

# One invocation carries the exact same unique name through setup and cleanup.
run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=unique-a E2E_INVOCATION_ID=invocation-a
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=unique-a E2E_INVOCATION_ID=invocation-a
[[ "$(cat "$log")" == $'create unique-a\ndelete unique-a' ]]
[[ "$(cat "$kind_kubeconfig_log")" == $'create /tmp/labdns-kind-kubeconfig-invocation-a\ndelete /tmp/labdns-kind-kubeconfig-invocation-a' ]]
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-invocation-a ]]
[[ ! -e /tmp/labdns-kind-kubeconfig-invocation-a ]]
[[ "$(cat "$ambient_kubeconfig")" == ambient-sentinel ]]

# An exact pre-existing name is refused before its marker or cluster is touched.
: >"$log"
printf '%s\n' preexisting >"$state"
if run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=preexisting E2E_INVOCATION_ID=new-invocation >"$temporary/preexisting.out" 2>&1; then
  echo "setup reused a pre-existing cluster" >&2
  exit 1
fi
[[ "$(cat "$state")" == preexisting && ! -s "$log" ]]

# A partial create failure is cleaned with the same pair and retains its status.
rm -f "$state"
: >"$log"
if FAKE_KIND_CREATE_STATUS=42 run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=partial-a E2E_INVOCATION_ID=partial-invocation >"$temporary/partial.out" 2>&1; then
  echo "partial Kind creation unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 42' "$temporary/partial.out"
[[ "$(cat "$log")" == $'create partial-a\ndelete partial-a' ]]
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-partial-invocation ]]
[[ ! -e /tmp/labdns-kind-kubeconfig-partial-invocation ]]

# A marker from another invocation cannot authorize a same-name deletion.
: >"$log"
printf '%s\n' foreign-a >"$state"
printf '%s\n%s\n' stale-invocation foreign-a >/tmp/labdns-kind-owned-current-invocation
if run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=foreign-a E2E_INVOCATION_ID=current-invocation >"$temporary/stale.out" 2>&1; then
  echo "stale marker authorized foreign cleanup" >&2
  exit 1
fi
grep -q 'marker does not authorize' "$temporary/stale.out"
[[ "$(cat "$state")" == foreign-a && ! -s "$log" ]]

# Cleanup failure retains both authorization and the invocation kubeconfig.
rm -f "$state" /tmp/labdns-kind-owned-current-invocation
: >"$log"
run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=delete-failure E2E_INVOCATION_ID=delete-failure
if FAKE_KIND_DELETE_STATUS=44 run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=delete-failure E2E_INVOCATION_ID=delete-failure >"$temporary/delete-failure.out" 2>&1; then
  echo "cleanup unexpectedly succeeded after Kind deletion failed" >&2
  exit 1
fi
[[ -e "$state" && -e /tmp/labdns-kind-owned-delete-failure && -e /tmp/labdns-kind-kubeconfig-delete-failure ]]
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=delete-failure E2E_INVOCATION_ID=delete-failure
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-delete-failure && ! -e /tmp/labdns-kind-kubeconfig-delete-failure ]]

run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=get-failure E2E_INVOCATION_ID=get-failure
if FAKE_KIND_GET_STATUS=42 run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=get-failure E2E_INVOCATION_ID=get-failure >"$temporary/get-cleanup.out" 2>&1; then
  echo "cleanup treated a failed Kind inventory as cluster absence" >&2
  exit 1
fi
[[ -e "$state" && -e /tmp/labdns-kind-owned-get-failure && -e /tmp/labdns-kind-kubeconfig-get-failure ]]
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=get-failure E2E_INVOCATION_ID=get-failure
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-get-failure && ! -e /tmp/labdns-kind-kubeconfig-get-failure ]]

# Neither ambient KUBECONFIG nor a path-shaped invocation can become a cleanup
# target.
if run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=invalid E2E_INVOCATION_ID=../../ambient-kubeconfig >"$temporary/path.out" 2>&1; then
  echo "cleanup accepted a path-shaped invocation ID" >&2
  exit 1
fi
[[ "$(cat "$ambient_kubeconfig")" == ambient-sentinel ]]
if run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=../ambient E2E_INVOCATION_ID=valid-cluster-check >"$temporary/cluster-path.out" 2>&1; then
  echo "setup accepted a path-shaped cluster name" >&2
  exit 1
fi
[[ "$(cat "$ambient_kubeconfig")" == ambient-sentinel ]]

# A failed test exports diagnostics, preserves its status, and cleans locally.
rm -f "$state"
: >"$log"
if run_make test-e2e KIND="$fake_kind" KIND_CLUSTER=failure-a E2E_INVOCATION_ID=failure-invocation E2E_SUITE_GLOB="$suite" E2E_TEST_COMMAND="$fake_test" E2E_DIAGNOSTICS_DIR="$temporary/diagnostics" >"$temporary/test.out" 2>&1; then
  echo "failing E2E command unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 23' "$temporary/test.out"
[[ "$(cat "$log")" == $'create failure-a\nexport failure-a\ndelete failure-a' ]]
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-failure-invocation ]]
[[ "$(cat "$test_kubeconfig_log")" == /tmp/labdns-kind-kubeconfig-failure-invocation ]]
if grep -v '^--kubeconfig /tmp/labdns-kind-kubeconfig-failure-invocation --context kind-failure-a ' "$kubectl_log" | grep -q .; then
  echo "failure diagnostics used a context other than the exact Kind cluster" >&2
  exit 1
fi

# CI preserves the same unique pair only until its explicit same-pair cleanup.
: >"$log"
: >"$kubectl_log"
if run_make test-e2e KIND="$fake_kind" KIND_CLUSTER=ci-a E2E_INVOCATION_ID=ci-invocation E2E_SUITE_GLOB="$suite" E2E_TEST_COMMAND="$fake_test" E2E_DIAGNOSTICS_DIR="$temporary/ci-diagnostics" E2E_KEEP_CLUSTER_ON_FAILURE=true >"$temporary/ci.out" 2>&1; then
  echo "failing CI E2E command unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 23' "$temporary/ci.out"
[[ "$(cat "$log")" == $'create ci-a\nexport ci-a' ]]
[[ "$(cat "$state")" == ci-a && -f /tmp/labdns-kind-owned-ci-invocation ]]
[[ -f /tmp/labdns-kind-kubeconfig-ci-invocation ]]
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=ci-a E2E_INVOCATION_ID=ci-invocation
[[ "$(tail -n 1 "$log")" == 'delete ci-a' ]]
[[ ! -e "$state" && ! -e /tmp/labdns-kind-owned-ci-invocation ]]
[[ ! -e /tmp/labdns-kind-kubeconfig-ci-invocation ]]
if grep -v '^--kubeconfig /tmp/labdns-kind-kubeconfig-ci-invocation --context kind-ci-a ' "$kubectl_log" | grep -q .; then
  echo "CI diagnostics used a context other than the exact Kind cluster" >&2
  exit 1
fi

# Standalone diagnostics refuse missing or mismatched invocation markers
# before either Kind or kubectl can access a cluster.
: >"$log"
: >"$kubectl_log"
if PATH="$temporary:$PATH" FAKE_KIND_STATE="$state" FAKE_KIND_LOG="$log" FAKE_KUBECTL_LOG="$kubectl_log" \
  KIND="$fake_kind" KIND_CLUSTER=diagnostic-a E2E_INVOCATION_ID=diagnostic-invocation \
	  E2E_DIAGNOSTICS_DIR="$temporary/missing-diagnostics" \
  "$repo_root/hack/collect-e2e-diagnostics.sh" >"$temporary/missing.out" 2>&1; then
  echo "diagnostics accepted a missing invocation marker" >&2
  exit 1
fi
[[ ! -s "$log" && ! -s "$kubectl_log" ]]

printf '%s\n%s\n' another-invocation diagnostic-a >/tmp/labdns-kind-owned-diagnostic-invocation
if PATH="$temporary:$PATH" FAKE_KIND_STATE="$state" FAKE_KIND_LOG="$log" FAKE_KUBECTL_LOG="$kubectl_log" \
  KIND="$fake_kind" KIND_CLUSTER=diagnostic-a E2E_INVOCATION_ID=diagnostic-invocation \
	  E2E_DIAGNOSTICS_DIR="$temporary/mismatched-diagnostics" \
  "$repo_root/hack/collect-e2e-diagnostics.sh" >"$temporary/mismatched.out" 2>&1; then
  echo "diagnostics accepted a mismatched invocation marker" >&2
  exit 1
fi
[[ ! -s "$log" && ! -s "$kubectl_log" ]]

printf '%s\n%s\n' diagnostic-invocation diagnostic-a >/tmp/labdns-kind-owned-diagnostic-invocation
printf '%s\n' 'fake isolated kubeconfig' >/tmp/labdns-kind-kubeconfig-diagnostic-invocation
PATH="$temporary:$PATH" FAKE_KIND_STATE="$state" FAKE_KIND_LOG="$log" FAKE_KUBECTL_LOG="$kubectl_log" \
  KIND="$fake_kind" KIND_CLUSTER=diagnostic-a E2E_INVOCATION_ID=diagnostic-invocation \
	  E2E_DIAGNOSTICS_DIR="$temporary/valid-diagnostics" \
  "$repo_root/hack/collect-e2e-diagnostics.sh"
[[ "$(cat "$log")" == 'export diagnostic-a' ]]
if grep -v '^--kubeconfig /tmp/labdns-kind-kubeconfig-diagnostic-invocation --context kind-diagnostic-a ' "$kubectl_log" | grep -q .; then
  echo "standalone diagnostics used a context other than the exact Kind cluster" >&2
  exit 1
fi
rm -f /tmp/labdns-kind-kubeconfig-diagnostic-invocation /tmp/labdns-kind-owned-diagnostic-invocation
[[ "$(cat "$ambient_kubeconfig")" == ambient-sentinel ]]

echo "E2E isolation safety verification passed"
