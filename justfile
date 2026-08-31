# Maintainer commands for development and releases.

set shell := ["bash", "-euo", "pipefail", "-c"]

just := just_executable()
image_repository := "ghcr.io/shednet/labdns"
chart_repository := "oci://ghcr.io/shednet/charts/labdns"

# List available recipes.
default: list

# List available recipes.
list:
    @"{{ just }}" --list

# Print the chart version.
version:
    #!/usr/bin/env bash
    set -euo pipefail
    value=$(awk '$1 == "version:" { print $2; exit }' charts/labdns/Chart.yaml)
    [[ "$value" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Chart version is not X.Y.Z: $value" >&2; exit 1; }
    printf '%s\n' "$value"

# Print the latest release tag.
latest-tag:
    @git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || printf '%s\n' 'no tags'

# List release tags, newest first.
tags:
    @git tag --list 'v[0-9]*' --sort=-v:refname

# Run the complete non-live-E2E repository gate.
check: lint test verify
    @echo 'All non-live-E2E checks passed.'

# Run static analysis.
lint:
    make lint

# Run unit and envtest tests.
test:
    make test

# Verify source, generated output, packaging, workflows, build, and E2E-tagged compilation.
verify:
    make verify-fmt vet verify-generated verify-artifacts verify-packaging verify-e2e-build verify-workflows build

# Require a completely clean worktree, including untracked files.
clean-tree:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -n "$(git status --porcelain=v1)" ]]; then
        echo 'Working tree is not clean. Commit or stash changes first.' >&2
        git status --short >&2
        exit 1
    fi

# Show commits since the latest release tag.
changelog:
    #!/usr/bin/env bash
    set -euo pipefail
    previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
    if [[ -z "$previous" ]]; then
        git log --pretty=format:'* %s (%h)' --reverse
    else
        git log --pretty=format:'* %s (%h)' "$previous"..HEAD
    fi

# Show release notes for an existing X.Y.Z version, or pending changes when omitted.
release-notes version="":
    #!/usr/bin/env bash
    set -euo pipefail
    requested='{{ version }}'
    if [[ -n "$requested" && ! "$requested" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo 'Version must be X.Y.Z.' >&2
        exit 1
    fi
    if [[ -n "$requested" ]]; then
        tag="v$requested"
        git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null || { echo "Tag $tag does not exist." >&2; exit 1; }
        previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$tag^" 2>/dev/null || true)
        range="${previous:+$previous..}$tag"
        install_version="$requested"
    else
        previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
        range="${previous:+$previous..}HEAD"
        install_version='<version>'
        echo '# Pending Release Notes'
        echo
    fi
    echo "## What's Changed"
    echo
    git log --pretty=format:'* %s (%h)' "$range"
    echo
    echo
    echo '## Installation'
    echo
    echo '```sh'
    echo "helm upgrade --install labdns {{ chart_repository }} --version $install_version"
    echo '```'

[private]
next-patch:
    #!/usr/bin/env bash
    set -euo pipefail
    current=$("{{ just }}" version)
    IFS=. read -r major minor patch <<<"$current"
    printf '%s.%s.%s\n' "$major" "$minor" "$((patch + 1))"

[private]
next-minor:
    #!/usr/bin/env bash
    set -euo pipefail
    current=$("{{ just }}" version)
    IFS=. read -r major minor _ <<<"$current"
    printf '%s.%s.0\n' "$major" "$((minor + 1))"

[private]
next-major:
    #!/usr/bin/env bash
    set -euo pipefail
    current=$("{{ just }}" version)
    IFS=. read -r major _ _ <<<"$current"
    printf '%s.0.0\n' "$((major + 1))"

# Show the current and next semantic versions.
next-versions:
    @echo "Current version: $("{{ just }}" version)"
    @echo "Next patch:      $("{{ just }}" next-patch)"
    @echo "Next minor:      $("{{ just }}" next-minor)"
    @echo "Next major:      $("{{ just }}" next-major)"

[private]
set-version version:
    #!/usr/bin/env bash
    set -euo pipefail
    new_version='{{ version }}'
    [[ "$new_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Version must be X.Y.Z.' >&2; exit 1; }
    grep -q '^version:' charts/labdns/Chart.yaml
    grep -q '^appVersion:' charts/labdns/Chart.yaml
    temporary=$(mktemp)
    trap 'rm -f "$temporary"' EXIT
    awk -v version="$new_version" '
        /^version:/ { print "version: " version; next }
        /^appVersion:/ { print "appVersion: v" version; next }
        { print }
    ' charts/labdns/Chart.yaml >"$temporary"
    mv "$temporary" charts/labdns/Chart.yaml

# Bump the patch version in Chart.yaml.
bump-patch: clean-tree
    @"{{ just }}" set-version "$("{{ just }}" next-patch)"

# Bump the minor version in Chart.yaml.
bump-minor: clean-tree
    @"{{ just }}" set-version "$("{{ just }}" next-minor)"

# Bump the major version in Chart.yaml.
bump-major: clean-tree
    @"{{ just }}" set-version "$("{{ just }}" next-major)"

# Run checks, bump patch/minor/major, and create the release commit.
prepare bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    bump='{{ bump }}'
    case "$bump" in patch|minor|major) ;; *) echo 'Bump must be patch, minor, or major.' >&2; exit 1 ;; esac
    [[ "$(git branch --show-current)" == main ]] || { echo 'Release preparation must run on main.' >&2; exit 1; }
    "{{ just }}" clean-tree
    git remote get-url origin >/dev/null
    git fetch --quiet origin '+refs/heads/main:refs/remotes/origin/main'
    [[ "$(git rev-parse HEAD)" == "$(git rev-parse refs/remotes/origin/main)" ]] || { echo 'Local main must exactly match origin/main.' >&2; exit 1; }
    next=$("{{ just }}" "next-$bump")
    tag="v$next"
    git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null && { echo "Local tag $tag already exists." >&2; exit 1; }
    remote_tag=$(git ls-remote --tags origin "refs/tags/$tag") || { echo 'Unable to check remote tags.' >&2; exit 1; }
    [[ -z "$remote_tag" ]] || { echo "Remote tag $tag already exists." >&2; exit 1; }
    "{{ just }}" check
    "{{ just }}" set-version "$next"
    git add charts/labdns/Chart.yaml
    git commit -m "chore: release $tag"
    echo "Prepared $tag on main."

# Create the annotated tag for the prepared release commit.
tag:
    #!/usr/bin/env bash
    set -euo pipefail
    [[ "$(git branch --show-current)" == main ]] || { echo 'Tagging must run on main.' >&2; exit 1; }
    "{{ just }}" clean-tree
    version=$("{{ just }}" version)
    grep -Fxq "appVersion: v$version" charts/labdns/Chart.yaml || { echo 'Chart appVersion must be vX.Y.Z.' >&2; exit 1; }
    tag="v$version"
    [[ "$(git rev-parse HEAD^)" == "$(git rev-parse refs/remotes/origin/main)" ]] || { echo 'The release commit must be directly above origin/main.' >&2; exit 1; }
    [[ "$(git log -1 --format=%s)" == "chore: release $tag" ]] || { echo 'HEAD is not the expected release commit.' >&2; exit 1; }
    git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null && { echo "Local tag $tag already exists." >&2; exit 1; }
    remote_tag=$(git ls-remote --tags origin "refs/tags/$tag") || { echo 'Unable to check remote tags.' >&2; exit 1; }
    [[ -z "$remote_tag" ]] || { echo "Remote tag $tag already exists." >&2; exit 1; }
    git tag --annotate "$tag" --message "Release $tag"
    echo "Created annotated tag $tag."

# Push the prepared main commit and its annotated tag.
push:
    #!/usr/bin/env bash
    set -euo pipefail
    [[ "$(git branch --show-current)" == main ]] || { echo 'Publishing must run on main.' >&2; exit 1; }
    "{{ just }}" clean-tree
    version=$("{{ just }}" version)
    tag="v$version"
    [[ "$(git cat-file -t "refs/tags/$tag" 2>/dev/null)" == tag ]] || { echo "$tag is not an annotated tag." >&2; exit 1; }
    [[ "$(git rev-list -n1 "$tag")" == "$(git rev-parse HEAD)" ]] || { echo "$tag does not point to HEAD." >&2; exit 1; }
    git fetch --quiet origin '+refs/heads/main:refs/remotes/origin/main'
    [[ "$(git rev-parse HEAD^)" == "$(git rev-parse refs/remotes/origin/main)" ]] || { echo 'Remote main changed after release preparation.' >&2; exit 1; }
    remote_tag=$(git ls-remote --tags origin "refs/tags/$tag") || { echo 'Unable to check remote tags.' >&2; exit 1; }
    [[ -z "$remote_tag" ]] || { echo "Remote tag $tag already exists." >&2; exit 1; }
    git push --atomic origin HEAD:refs/heads/main "refs/tags/$tag:refs/tags/$tag"
    echo "Pushed main and $tag; the release workflow will publish {{ image_repository }}:$tag and {{ chart_repository }}."

# Complete a patch/minor/major release, prompting once before the only push.
release bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    "{{ just }}" prepare '{{ bump }}'
    "{{ just }}" tag
    printf 'Push main and the release tag to origin? [y/N] '
    read -r reply
    if [[ "$reply" =~ ^[Yy]$ ]]; then
        "{{ just }}" push
    else
        echo 'Push skipped. Run `bin/just push` after review.'
    fi

# Build the canonical controller image locally.
build-local:
    #!/usr/bin/env bash
    set -euo pipefail
    version=$("{{ just }}" version)
    make docker-build IMG="{{ image_repository }}:v$version"

# Build and load into an explicitly named, invocation-owned isolated Kind cluster.
build-kind cluster:
    #!/usr/bin/env bash
    set -euo pipefail
    cluster='{{ cluster }}'
    [[ "$cluster" =~ ^labdns-e2e-[a-z0-9]([-a-z0-9]{0,50}[a-z0-9])?$ ]] || { echo 'Cluster must be an explicit labdns-e2e-* isolated cluster name.' >&2; exit 1; }
    make kind
    mapfile -t markers < <(grep -lFx "$cluster" /tmp/labdns-kind-owned-* 2>/dev/null || true)
    [[ ${#markers[@]} -eq 1 && "$(sed -n '2p' "${markers[0]}")" == "$cluster" ]] || { echo 'Cluster lacks one unambiguous labdns invocation marker.' >&2; exit 1; }
    bin/kind get clusters | grep -Fxq "$cluster" || { echo "Kind cluster $cluster does not exist." >&2; exit 1; }
    version=$("{{ just }}" version)
    image="{{ image_repository }}:v$version"
    make docker-build IMG="$image"
    bin/kind load docker-image "$image" --name "$cluster"

# Run the live E2E suite in its isolated Kind environment.
test-e2e:
    make test-e2e

# Show release state and changes since the latest tag.
status:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Current version: $("{{ just }}" version)"
    echo "Latest tag:      $("{{ just }}" latest-tag)"
    "{{ just }}" changelog

# Open the canonical GitHub releases page.
gh-releases:
    @gh browse --repo shednet/labdns releases 2>/dev/null || echo 'Visit https://github.com/shednet/labdns/releases'

# Show recent CI runs.
ci-status:
    @gh run list --repo shednet/labdns --limit 5 2>/dev/null || echo 'Install or authenticate gh to view CI status.'
