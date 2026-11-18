#!/usr/bin/env bash
# Split the QEMU integration suite into shards for the test-qemu matrix.
#
#   scripts/ci/qemu-shards.sh <shard-count> [run-regexp]
#
# Prints a JSON array of {"shard": N, "run": "^(TestA|TestB)$"} objects, one
# per non-empty shard. Tests are dealt round-robin in sorted order, so a
# given test stays in the same shard until the test list changes. With
# run-regexp, only matching tests are dealt. An empty result is an error:
# an empty matrix would let CI pass without running anything.
set -euo pipefail

count="${1:?usage: qemu-shards.sh <shard-count> [run-regexp]}"
filter="${2:-}"
case "$count" in
    '' | *[!0-9]* | 0) echo "shard count must be a positive integer, got: $count" >&2; exit 2 ;;
esac

cd "$(git rev-parse --show-toplevel)"

tests=$(go test -tags integration -list '^Test' ./internal/qemutest/ | grep '^Test' | sort)
if [ -n "$filter" ]; then
    tests=$(printf '%s\n' "$tests" | grep -E -- "$filter" || true)
fi
if [ -z "$tests" ]; then
    echo "no QEMU tests found${filter:+ matching $filter}; refusing to emit an empty matrix" >&2
    exit 1
fi

printf '%s\n' "$tests" | awk -v n="$count" '
    { shard[(NR - 1) % n] = shard[(NR - 1) % n] (shard[(NR - 1) % n] == "" ? "" : "|") $0 }
    END {
        printf "["
        sep = ""
        for (i = 0; i < n; i++) {
            if (shard[i] == "") continue
            printf "%s{\"shard\":%d,\"run\":\"^(%s)$\"}", sep, i + 1, shard[i]
            sep = ","
        }
        print "]"
    }'
