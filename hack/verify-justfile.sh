#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
just_bin="${JUST:-$repo_root/bin/just}"

"$just_bin" --justfile "$repo_root/justfile" --fmt --check
"$just_bin" --justfile "$repo_root/justfile" --list >/dev/null

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir "$tmp_dir/just-temp"
export JUST_TEMPDIR="$tmp_dir/just-temp"

external_just="$tmp_dir/caller-owned-just"
printf '%s\n' 'caller owned bytes; do not execute' >"$external_just"
touch -t 200001010000 "$external_just"
cp -p "$external_just" "$external_just.before"
external_mtime_before=$(stat -c %Y "$external_just" 2>/dev/null || stat -f %m "$external_just")
make -C "$repo_root" just JUST="$external_just" >/dev/null
external_mtime_after=$(stat -c %Y "$external_just" 2>/dev/null || stat -f %m "$external_just")
cmp "$external_just.before" "$external_just"
[[ "$external_mtime_before" == "$external_mtime_after" ]]
[[ ! -L "$external_just" ]]

new_fixture() {
  local name="$1"
  local bare="$tmp_dir/$name.git"
  local work="$tmp_dir/$name"
  git init --quiet --bare "$bare"
  git init --quiet --initial-branch=main "$work"
  git -C "$work" config user.name 'Release Test'
  git -C "$work" config user.email release-test@example.invalid
  git -C "$work" config commit.gpgsign false
  git -C "$work" config tag.gpgsign false
  mkdir -p "$work/charts/labdns" "$work/fake-bin"
  cp "$repo_root/justfile" "$work/justfile"
  printf '%s\n' \
    'apiVersion: v2' \
    'name: labdns' \
    'version: 0.1.0' \
    'appVersion: v0.1.0' >"$work/charts/labdns/Chart.yaml"
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf "%s\n" "$*" >>"$MAKE_LOG"' >"$work/fake-bin/make"
  chmod 0755 "$work/fake-bin/make"
  git -C "$work" add .
  git -C "$work" commit --quiet -m 'initial'
  git -C "$work" remote add origin "$bare"
  git -C "$work" push --quiet --set-upstream origin main
  printf '%s\n' "$work"
}

run_just() {
  local work="$1"
  shift
  PATH="$work/fake-bin:$PATH" MAKE_LOG="$work/.git/make.log" "$just_bin" \
    --justfile "$work/justfile" --working-directory "$work" "$@"
}

work="$(new_fixture release-no)"
[[ "$(run_just "$work" version)" == 0.1.0 ]]
if run_just "$work" set-version 1.2 >/dev/null 2>&1; then
  echo 'invalid version was accepted' >&2
  exit 1
fi
if run_just "$work" release sideways >/dev/null 2>&1; then
  echo 'invalid bump was accepted' >&2
  exit 1
fi
printf 'n\n' | run_just "$work" release patch >/dev/null
grep -Fxq 'version: 0.1.1' "$work/charts/labdns/Chart.yaml"
grep -Fxq 'appVersion: v0.1.1' "$work/charts/labdns/Chart.yaml"
[[ "$(git -C "$work" log -1 --format=%s)" == 'chore: release v0.1.1' ]]
[[ "$(git -C "$work" cat-file -t refs/tags/v0.1.1)" == tag ]]
[[ "$(git -C "$work" rev-parse refs/remotes/origin/main)" == "$(git --git-dir="$tmp_dir/release-no.git" rev-parse refs/heads/main)" ]]
[[ "$(git --git-dir="$tmp_dir/release-no.git" rev-parse refs/heads/main)" != "$(git -C "$work" rev-parse HEAD)" ]]
if git --git-dir="$tmp_dir/release-no.git" rev-parse --verify --quiet refs/tags/v0.1.1 >/dev/null; then
  echo 'declined release unexpectedly pushed its tag' >&2
  exit 1
fi
grep -Fxq 'lint' "$work/.git/make.log"
grep -Fxq 'test' "$work/.git/make.log"
grep -Fxq 'verify-fmt vet verify-generated verify-artifacts verify-packaging verify-e2e-build verify-workflows build' "$work/.git/make.log"
if grep -q 'test-e2e' "$work/.git/make.log"; then
  echo 'release check invoked live E2E' >&2
  exit 1
fi
run_just "$work" push >/dev/null
[[ "$(git --git-dir="$tmp_dir/release-no.git" rev-parse refs/heads/main)" == "$(git -C "$work" rev-parse HEAD)" ]]
[[ "$(git --git-dir="$tmp_dir/release-no.git" rev-parse refs/tags/v0.1.1^{})" == "$(git -C "$work" rev-parse HEAD)" ]]

work="$(new_fixture atomic-reject)"
printf 'n\n' | run_just "$work" release patch >/dev/null
remote_main_before=$(git --git-dir="$tmp_dir/atomic-reject.git" rev-parse refs/heads/main)
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'if [[ "$1" == refs/heads/main ]]; then exit 1; fi' \
  'exit 0' >"$tmp_dir/atomic-reject.git/hooks/update"
chmod 0755 "$tmp_dir/atomic-reject.git/hooks/update"
if run_just "$work" push >/dev/null 2>&1; then
  echo 'remote rejection unexpectedly accepted the atomic release push' >&2
  exit 1
fi
[[ "$(git --git-dir="$tmp_dir/atomic-reject.git" rev-parse refs/heads/main)" == "$remote_main_before" ]]
if git --git-dir="$tmp_dir/atomic-reject.git" rev-parse --verify --quiet refs/tags/v0.1.1 >/dev/null; then
  echo 'atomic push failure left the release tag on the remote' >&2
  exit 1
fi

work="$(new_fixture collision-local)"
git -C "$work" tag --annotate v0.1.1 --message collision
if run_just "$work" release patch >/dev/null 2>&1; then
  echo 'local tag collision was accepted' >&2
  exit 1
fi
[[ "$(run_just "$work" version)" == 0.1.0 ]]

work="$(new_fixture collision-remote)"
git -C "$work" tag --annotate v0.1.1 --message collision
git -C "$work" push --quiet origin refs/tags/v0.1.1
git -C "$work" tag --delete v0.1.1 >/dev/null
if run_just "$work" release patch >/dev/null 2>&1; then
  echo 'remote tag collision was accepted' >&2
  exit 1
fi
[[ "$(run_just "$work" version)" == 0.1.0 ]]

work="$(new_fixture unsynchronized)"
printf '%s\n' dirty >"$work/local-only"
git -C "$work" add local-only
git -C "$work" commit --quiet -m 'local only'
if run_just "$work" release patch >/dev/null 2>&1; then
  echo 'unsynchronized main was accepted' >&2
  exit 1
fi

echo 'justfile verification passed'
