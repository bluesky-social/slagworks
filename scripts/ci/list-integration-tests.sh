#!/usr/bin/env bash
# Enumerate integration tests as a JSON array of {pkg, test} objects.
# One entry per TestXxx found under the integration build tag.
#
# Only packages that have at least one `//go:build integration` file
# are considered — otherwise we'd match every unit test too (because
# tests compile WITH the integration tag, not ONLY with it).
#
# qemutest is hard-coded as skipped here because it requires /dev/kvm,
# which the containerized `arc` runners can't provide. The qemu suite
# runs in test-qemu.yaml on a kvm-tagged self-hosted runner.
#
# If the hard-coded skip list removes packages and no runnable tests remain,
# an empty matrix would let CI green-pass without running the intended suite,
# so we fail loudly instead.
#
# Called from .github/workflows/test-integration.yaml (discover job).
# Runs locally: `scripts/ci/list-integration-tests.sh | jq .`
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)

skip_pkgs=(
    github.com/bluesky-social/slagworks/internal/qemutest
)

is_skipped() {
    local needle="$1"
    for s in "${skip_pkgs[@]:-}"; do
        [ "$s" = "$needle" ] && return 0
    done
    return 1
}

# Find files that start with the integration build tag. grep exits 1 when
# nothing matches; that is a legitimate empty repository, while statuses
# greater than 1 are real discovery errors and must still fail CI.
grep_status=0
integration_files=$(grep -rl --include='*.go' -E '^//go:build integration($|\s|,)' "$repo_root") || grep_status=$?
if [ "$grep_status" -gt 1 ]; then
    exit "$grep_status"
fi

# Convert matching files to unique package directories. Avoid invoking
# xargs at all for the empty case, which is portable to macOS/Bash 3.2.
integration_dirs=""
if [ -n "$integration_files" ]; then
    integration_dirs=$(printf '%s\n' "$integration_files" \
        | xargs -n1 dirname \
        | sort -u)
fi

pkgs=()
skipped=0
for dir in $integration_dirs; do
    # Convert filesystem path to Go package path via `go list`. Scoped
    # to the directory so we don't follow imports.
    pkg=$(go list -tags integration "$dir" 2>/dev/null || true)
    if [ -z "$pkg" ]; then
        continue
    fi
    if is_skipped "$pkg"; then
        echo "list-integration-tests: skipping $pkg (hard-coded skip)" >&2
        skipped=$((skipped + 1))
        continue
    fi
    pkgs+=("$pkg")
done

entries=()
# Guard the iteration: expanding an empty array is an unbound-variable
# error under macOS's bash 3.2 with `set -u`, which would fail the
# genuinely-empty case before `[]` is emitted.
if [ ${#pkgs[@]} -gt 0 ]; then
    for pkg in "${pkgs[@]}"; do
        # One package at a time: output is test names only, no "ok pkg"
        # footer to strip. A failing go test -list (e.g. compile error)
        # exits non-zero and set -e fails the whole script so we don't
        # silently drop tests.
        names=$(go test -tags integration -list '.*' "$pkg")
        while read -r name; do
            [ -z "$name" ] && continue
            case "$name" in Test*) ;; *) continue ;; esac
            entries+=("{\"pkg\":\"$pkg\",\"test\":\"$name\"}")
        done <<< "$names"
    done
fi

if [ ${#entries[@]} -eq 0 ] && [ "$skipped" -gt 0 ]; then
    echo "list-integration-tests: the hard-coded skip list removed $skipped integration-test package(s) and no runnable integration tests remain; refusing to emit an empty matrix" >&2
    exit 1
fi

if [ ${#entries[@]} -eq 0 ]; then
    echo '[]'
else
    printf '%s\n' "${entries[@]}" | jq -s -c '.'
fi
