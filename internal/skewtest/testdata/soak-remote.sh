#!/usr/bin/env bash
# Runs on the soak host, from the synced tree's root.
# usage: soak-remote.sh <procs> <shards> <rapid.checks> <run-regex> [timeout]
set -u
procs="$1"
shards="$2"
checks="$3"
run="$4"
timeout="${5:-120m}"
root="$(pwd)"
export SKEW_OLD_TREE="$root/.proptest-out/old-v0.5.0"
export SKEW_SHARDS="$shards"
out="$root/.proptest-out/soak-out"
rm -rf "$out"
mkdir -p "$out"
go test -c -o "$out/skew.test" ./internal/skewtest || exit 1
cd internal/skewtest || exit 1
pids=()
for i in $(seq 1 "$procs"); do
  SKEW_PORT_BASE=$((20000 + i * 1000)) "$out/skew.test" -test.run "$run" -rapid.checks="$checks" -rapid.nofailfile -test.v -test.timeout "$timeout" > "$out/proc-$i.txt" 2>&1 &
  pids+=("$!")
done
rc=0
for p in "${pids[@]}"; do
  wait "$p" || rc=1
done
echo "soak rc=$rc"
for f in "$out"/proc-*.txt; do
  echo "== $f"
  grep -n '^--- \|^FAIL\|^ok\|^PASS\|^panic\|\[rapid\] failed\|\[rapid\] flaky\|Original traceback\|_test.go:[0-9]*: \(health\|boot\|actuate\|complete\|honest\|settle\|migrate\|credential\|harness\)' "$f" | cut -c1-1500 | head -40
done
echo "== totals"
cat "$out"/proc-*.txt | grep 'coverage: cases=' | awk '{for(i=1;i<=NF;i++){if($i ~ /^cases=/){split($i,a,"=");c+=a[2]} if($i ~ /^steps=/){split($i,b,"=");s+=b[2]}}} END {print "cases", c, "steps", s}'
exit $rc
