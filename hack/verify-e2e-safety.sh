#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

fake_kind="$temporary/kind"
fake_test="$temporary/fail-test"
state="$temporary/cluster"
log="$temporary/kind.log"
marker="$temporary/owner"
suite="$temporary/suite_test.go"
touch "$suite"

cat >"$fake_kind" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

operation="$1"
subcommand="${2:-}"
case "$operation/$subcommand" in
  get/clusters)
    if [[ -f "$FAKE_KIND_STATE" ]]; then cat "$FAKE_KIND_STATE"; fi
    ;;
  create/cluster)
    shift 2
    name=""
    while (($#)); do
      if [[ "$1" == "--name" ]]; then name="$2"; shift 2; else shift; fi
    done
    printf 'create %s\n' "$name" >>"$FAKE_KIND_LOG"
    printf '%s\n' "$name" >"$FAKE_KIND_STATE"
    exit "${FAKE_KIND_CREATE_STATUS:-0}"
    ;;
  delete/cluster)
    shift 2
    [[ "$1" == "--name" ]]
    printf 'delete %s\n' "$2" >>"$FAKE_KIND_LOG"
    if [[ -f "$FAKE_KIND_STATE" ]] && [[ "$(cat "$FAKE_KIND_STATE")" == "$2" ]]; then
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
exit 23
EOF
chmod 0755 "$fake_kind" "$fake_test"

run_make() {
  FAKE_KIND_STATE="$state" FAKE_KIND_LOG="$log" make -s -C "$repo_root" "$@"
}

# One invocation carries the exact same unique name through setup and cleanup.
run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=unique-a E2E_INVOCATION_ID=invocation-a KIND_OWNERSHIP_FILE="$marker"
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=unique-a E2E_INVOCATION_ID=invocation-a KIND_OWNERSHIP_FILE="$marker"
[[ "$(cat "$log")" == $'create unique-a\ndelete unique-a' ]]
[[ ! -e "$state" && ! -e "$marker" ]]

# An exact pre-existing name is refused before its marker or cluster is touched.
: >"$log"
printf '%s\n' preexisting >"$state"
printf '%s\n' sentinel >"$marker"
if run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=preexisting E2E_INVOCATION_ID=new-invocation KIND_OWNERSHIP_FILE="$marker" >"$temporary/preexisting.out" 2>&1; then
  echo "setup reused a pre-existing cluster" >&2
  exit 1
fi
[[ "$(cat "$marker")" == sentinel && "$(cat "$state")" == preexisting && ! -s "$log" ]]

# A partial create failure is cleaned with the same pair and retains its status.
rm -f "$state" "$marker"
: >"$log"
if FAKE_KIND_CREATE_STATUS=42 run_make setup-test-e2e KIND="$fake_kind" KIND_CLUSTER=partial-a E2E_INVOCATION_ID=partial-invocation KIND_OWNERSHIP_FILE="$marker" >"$temporary/partial.out" 2>&1; then
  echo "partial Kind creation unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 42' "$temporary/partial.out"
[[ "$(cat "$log")" == $'create partial-a\ndelete partial-a' ]]
[[ ! -e "$state" && ! -e "$marker" ]]

# A marker from another invocation cannot authorize a same-name deletion.
: >"$log"
printf '%s\n' foreign-a >"$state"
printf '%s\n%s\n' stale-invocation foreign-a >"$marker"
if run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=foreign-a E2E_INVOCATION_ID=current-invocation KIND_OWNERSHIP_FILE="$marker" >"$temporary/stale.out" 2>&1; then
  echo "stale marker authorized foreign cleanup" >&2
  exit 1
fi
grep -q 'marker does not authorize' "$temporary/stale.out"
[[ "$(cat "$state")" == foreign-a && ! -s "$log" ]]

# A failed test exports diagnostics, preserves its status, and cleans locally.
rm -f "$state" "$marker"
: >"$log"
if run_make test-e2e KIND="$fake_kind" KIND_CLUSTER=failure-a E2E_INVOCATION_ID=failure-invocation KIND_OWNERSHIP_FILE="$marker" E2E_SUITE_GLOB="$suite" E2E_TEST_COMMAND="$fake_test" E2E_DIAGNOSTICS_DIR="$temporary/diagnostics" >"$temporary/test.out" 2>&1; then
  echo "failing E2E command unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 23' "$temporary/test.out"
[[ "$(cat "$log")" == $'create failure-a\nexport failure-a\ndelete failure-a' ]]
[[ ! -e "$state" && ! -e "$marker" ]]

# CI preserves the same unique pair only until its explicit same-pair cleanup.
: >"$log"
if run_make test-e2e KIND="$fake_kind" KIND_CLUSTER=ci-a E2E_INVOCATION_ID=ci-invocation KIND_OWNERSHIP_FILE="$marker" E2E_SUITE_GLOB="$suite" E2E_TEST_COMMAND="$fake_test" E2E_DIAGNOSTICS_DIR="$temporary/ci-diagnostics" E2E_KEEP_CLUSTER_ON_FAILURE=true >"$temporary/ci.out" 2>&1; then
  echo "failing CI E2E command unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'Error 23' "$temporary/ci.out"
[[ "$(cat "$log")" == $'create ci-a\nexport ci-a' ]]
[[ "$(cat "$state")" == ci-a && -f "$marker" ]]
run_make cleanup-test-e2e KIND="$fake_kind" KIND_CLUSTER=ci-a E2E_INVOCATION_ID=ci-invocation KIND_OWNERSHIP_FILE="$marker"
[[ "$(tail -n 1 "$log")" == 'delete ci-a' ]]
[[ ! -e "$state" && ! -e "$marker" ]]

echo "E2E isolation safety verification passed"
