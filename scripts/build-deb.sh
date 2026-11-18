#!/usr/bin/env bash
set -euo pipefail

# Build the slagworks .deb packages. Used by both CI and local builds.
# Pure Go, no CGO — works on any platform without a C toolchain.
#
# Output:
#   slagworks-agent_${VERSION}_amd64.deb  — standalone agent (for ansible)
#   slagworks_${VERSION}_amd64.deb        — full daemon, depends on agent deb
# where VERSION is the release-please semver (X.Y.Z on a release commit,
# X.Y.Z+git.N.g<sha> snapshot otherwise — see below).

SHA=$(git rev-parse HEAD)

# Deb version = the repo's release-please semver (one repo, one version:
# the same vX.Y.Z the global deploy pins and slagctl releases under).
#
# - Release commit (release-please's merge, subject
#   "chore(main): release X.Y.Z (#NN)"): version X.Y.Z exactly. Parsed
#   from the subject rather than git-describe because build.yaml builds
#   this commit WHILE release-please is still creating the tag — the
#   tag can't be trusted to exist yet.
# - Any other commit: X.Y.Z+git.N.g<sha> (Debian snapshot convention,
#   from git describe) — sorts above X.Y.Z and below X.Y.(Z+1), so
#   break-glass SHA deploys still move the fleet monotonically.
# - No tags reachable (fresh clone, CI without fetch-depth:0): fail
#   loudly rather than mint a version that breaks pool ordering.
#
# History note: versions before 0.2.0 were 0.1.${BUILD_NUMBER} (grew to
# 0.1.552) — 0.2.0 exists to jump past that range in Debian ordering.
SUBJECT=$(git log -1 --format=%s)
if [[ "$SUBJECT" =~ ^chore\(main\):\ release\ ([0-9]+\.[0-9]+\.[0-9]+) ]]; then
    VERSION="${BASH_REMATCH[1]}"
else
    DESCRIBE=$(git describe --tags --match 'v*' --long 2>/dev/null) || {
        echo "ERROR: git describe found no v* tag (shallow clone? set fetch-depth: 0)" >&2
        exit 1
    }
    # v0.2.0-3-g1a2b3c4 -> 0.2.0+git.3.g1a2b3c4
    if [[ "$DESCRIBE" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-([0-9]+)-g([0-9a-f]+)$ ]]; then
        VERSION="${BASH_REMATCH[1]}+git.${BASH_REMATCH[2]}.g${BASH_REMATCH[3]}"
    else
        echo "ERROR: unparseable git describe output: ${DESCRIBE}" >&2
        exit 1
    fi
fi
# main.packageVersion carries the deb version alongside the SHA: global
# sends both to site workers, whose auto-upgrade helper apt-installs
# exactly that version. Only slagworksd declares it; the linker ignores
# -X for a symbol a binary does not define.
LDFLAGS="-X main.version=${SHA} -X main.packageVersion=${VERSION}"

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64


echo "Building binaries (${SHA::8})..."
go build -trimpath -ldflags="$LDFLAGS" -o slagworksd ./cmd/slagworksd/
go build -trimpath -ldflags="$LDFLAGS" -o slagctl ./cmd/slagctl/
go build -trimpath -ldflags="$LDFLAGS" -o slagworks-agent ./cmd/slagworks-agent/

echo "Packaging slagworks-agent.deb..."
export VERSION
nfpm package --config nfpm-agent.yaml --packager deb --target "slagworks-agent_${VERSION}_amd64.deb"

echo "Packaging slagworks.deb..."
nfpm package --packager deb --target "slagworks_${VERSION}_amd64.deb"

rm -f slagworksd slagctl slagworks-agent
echo "Built slagworks-agent and slagworks debs (${VERSION})"
