#!/usr/bin/env bash
# Decide whether a change can affect the QEMU integration suite.
#
#   scripts/ci/qemu-relevant.sh <base-commit> <head-commit>
#
# Prints "true" or "false". Run from a checkout of the head commit.
#
# The suite exercises what runs during a boot: the QEMU test package and
# the binaries it launches (the agent, the test TFTP server, the serial
# relay), the boot image, and the workflow itself. So a change is relevant
# when it touches:
#
#   - a directory of any Go package those binaries are built from, vendored
#     dependencies included (`go list -deps`). A dependency bump that only
#     reaches slagctl changes none of them;
#   - the Go version (`go` or `toolchain` in go.mod);
#   - image inputs, embedded boot assets, or the workflow and its scripts.
#
# Anything this script cannot analyze is relevant: if `go list` fails (for
# example on inconsistent vendoring) the answer is "true", and the suite's
# own jobs report the real error.
set -euo pipefail

base="${1:?usage: qemu-relevant.sh <base-commit> <head-commit>}"
head="${2:?usage: qemu-relevant.sh <base-commit> <head-commit>}"

cd "$(git rev-parse --show-toplevel)"
root="$PWD"

changed=$(git diff --name-only "$base...$head")
if [ -z "$changed" ]; then
    echo false
    exit 0
fi

# Inputs that are not Go packages.
while IFS= read -r path; do
    case "$path" in
        assets/* | scripts/image-build/* | scripts/build-deb.sh) echo true; exit 0 ;;
        .github/workflows/test-qemu.yaml | .github/workflows/build-images.yaml | .github/actions/*) echo true; exit 0 ;;
        scripts/ci/qemu-relevant.sh | scripts/ci/qemu-shards.sh) echo true; exit 0 ;;
    esac
done <<<"$changed"

# The Go version changes every binary.
if git diff -U0 "$base...$head" -- go.mod | grep -qE '^[+-](go|toolchain) '; then
    echo true
    exit 0
fi

# Directories of every package the suite's binaries are built from.
if ! dirs=$(go list -deps -test -tags integration -f '{{.Dir}}' \
        ./internal/qemutest/ ./cmd/slagworks-agent/ ./cmd/qemutest-tftpd/ ./cmd/qemutest-serialrelay/ 2>/dev/null); then
    echo true
    exit 0
fi
dirs=$(printf '%s\n' "$dirs" | grep -F "$root/" | sed "s|^$root/||" | sort -u)

# A changed file is relevant when its directory, or (for embedded files in
# subdirectories) an ancestor directory, is one of those package directories.
while IFS= read -r path; do
    dir=$(dirname "$path")
    while [ "$dir" != "." ]; do
        if grep -qxF "$dir" <<<"$dirs"; then
            echo true
            exit 0
        fi
        dir=$(dirname "$dir")
    done
done <<<"$changed"

echo false
