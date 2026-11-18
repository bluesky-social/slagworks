#!/usr/bin/env bash
# Regenerate dev/netbox/netbox-baseline.dump — the post-init, zero-content
# NetBox Postgres dump the netboxtest harness restores per test.
#
# When to run: bumping the NetBox version (update BOTH the image tag in
# docker-compose.yml and netboxTag in internal/netboxtest/netboxtest.go —
# this script refuses to run if they disagree), or if the dump is ever
# suspected stale/corrupt. The dump contains NO slagworks state (custom
# fields/tags are bootstrapped per test by client.Bootstrap) and NO
# sites/devices — it exists purely so tests skip NetBox's multi-minute
# first-boot migration.
#
# What it does: compose up a fresh NetBox stack on a throwaway project +
# volume (does not touch your `just netbox-up` dev stack's data), wait
# for first-boot migrations, pg_dump, tear down. The superuser + API
# token come from the compose env; the harness regenerates its own token
# per boot anyway (NetBox 4.x token pepper).
#
# Usage: dev/netbox/regen-baseline.sh
set -euo pipefail
cd "$(dirname "$0")"

compose_tag=$(rg -o 'netboxcommunity/netbox:(\S+)' -r '$1' docker-compose.yml)
harness_tag=$(rg -o 'netboxTag\s+=\s+"([^"]+)"' -r '$1' ../../internal/netboxtest/netboxtest.go)
if [ "$compose_tag" != "$harness_tag" ]; then
    echo "tag mismatch: docker-compose.yml has $compose_tag, netboxtest.go has $harness_tag" >&2
    echo "update both before regenerating the baseline" >&2
    exit 1
fi
echo "netbox tag: $compose_tag"

PROJECT=nbx-regen-baseline
compose() { docker compose -p "$PROJECT" -f docker-compose.yml "$@"; }
cleanup() { compose down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

compose up -d

echo "waiting for NetBox first-boot migrations (takes minutes)..."
ok=""
for _ in $(seq 1 120); do
    if compose exec -T netbox curl -sf http://localhost:8080/api/status/ >/dev/null 2>&1; then
        ok=1
        break
    fi
    sleep 5
done
[ -n "$ok" ] || { echo "NetBox never came up; see: docker compose -p $PROJECT logs netbox" >&2; exit 1; }

echo "dumping..."
compose exec -T postgres pg_dump -U netbox -Fc netbox > netbox-baseline.dump.new
mv netbox-baseline.dump.new netbox-baseline.dump
ls -lh netbox-baseline.dump
echo "done — validate with the netboxtest suite (integration tag) before committing."
