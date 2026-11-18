#!/usr/bin/env bash
# Publish gate: scan the public tree for identifiers that must not ship.
#
# The public tree is every tracked file except the paths in .oss-exclude and
# vendor/. Each rule below is a name and an extended regex; any match fails
# the gate. Site and host short names (pop1, kworker-l34-1) and RFC 1918
# addresses in tests are deliberately not rules: they identify nothing
# reachable, and Bluesky's network scripts ship as worked examples. Run with no arguments for a per-rule summary, or name rules to
# list their matches:
#
#   scripts/ci/public-clean.sh                    # summary, exit 1 on any hit
#   scripts/ci/public-clean.sh tailnet aws        # list matches for those rules
#   scripts/ci/public-clean.sh --check tracker    # summary and exit status for those rules only
#
# Plan: docs/plans/2026-11-14-open-source-release.md
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# This script and the exclude list name the patterns they look for.
pathspec=(. ':!vendor' ':!scripts/ci/public-clean.sh' ':!.oss-exclude')
while IFS= read -r line; do
    case "$line" in
        '' | '#'*) continue ;;
    esac
    pathspec+=(":!$line")
done <.oss-exclude

rules=(
    # Internal tracker IDs: meaningless outside the org.
    'tracker|PLA-[0-9]+'
    # Tailnet and internal DNS names.
    'tailnet|blue-prometheus|[a-z0-9-]+\.ts\.net|noclues\.net'
    'domains|bsky\.(dev|network)'
    # AWS account, registries, buckets, database hosts.
    'aws|007404326489|bsky-(slagworks|apt)[a-z-]*|crlwundchz7x'
    # Private repositories and internal tooling.
    'private-repos|bsky-infra|infra-stacks|bluesky-social/(deploy|platform|\.github-private|homebrew-internal)|diderot'
    # People.
    'people|(^|[^a-z])austin([^a-z]|$)|jcalabro|mckinley'
    # Credentials that were once real, and redaction placeholders.
    'creds|slagworks1!|REDACTED-|8jRLEaxNjLOjKMt5dvMrrmMxigaOhTWbunWF1y9H'
)

scan() { # regex, extra git-grep flags...
    local re="$1"
    shift
    local status=0
    # -a: binary fixtures (console captures, database dumps) are scanned too.
    git grep -a -E -i "$@" -e "$re" -- "${pathspec[@]}" || status=$?
    # 1 is "no match"; anything else is a failed scan, which must not pass.
    if [ "$status" -gt 1 ]; then
        echo "public-clean: git grep failed (status $status)" >&2
        exit "$status"
    fi
}

# --check <rule>...: gate on the named rules only. CI uses this for the
# rules that are already clean, so they stay clean while the rest are worked
# down.
only=()
if [ "${1:-}" = --check ]; then
    shift
    only=("$@")
    if [ "${#only[@]}" -eq 0 ]; then
        echo "--check needs at least one rule name" >&2
        exit 2
    fi
    for want in "${only[@]}"; do
        known=0
        for rule in "${rules[@]}"; do
            [ "${rule%%|*}" = "$want" ] && known=1
        done
        if [ "$known" -eq 0 ]; then
            echo "unknown rule: $want" >&2
            exit 2
        fi
    done
    set --
fi

if [ "$#" -gt 0 ]; then
    for want in "$@"; do
        found=0
        for rule in "${rules[@]}"; do
            if [ "${rule%%|*}" = "$want" ]; then
                found=1
                scan "${rule#*|}" -n
            fi
        done
        if [ "$found" -eq 0 ]; then
            echo "unknown rule: $want" >&2
            exit 2
        fi
    done
    exit 0
fi

total=0
printf '%-14s %6s %6s\n' rule hits files
for rule in "${rules[@]}"; do
    name="${rule%%|*}"
    re="${rule#*|}"
    if [ "${#only[@]}" -gt 0 ]; then
        selected=0
        for want in "${only[@]}"; do
            [ "$want" = "$name" ] && selected=1
        done
        [ "$selected" -eq 1 ] || continue
    fi
    hits=$(scan "$re" | wc -l | tr -d ' ')
    files=$(scan "$re" -l | wc -l | tr -d ' ')
    printf '%-14s %6s %6s\n' "$name" "$hits" "$files"
    total=$((total + hits))
done

if [ "$total" -gt 0 ]; then
    echo "public-clean: $total hits; list them with: $0 <rule>" >&2
    exit 1
fi
echo "public-clean: clean"
