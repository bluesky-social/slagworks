# Build and release

What this repository builds, how CI checks a change, and how a release is
cut. Everything here is derived from the files it names:
`scripts/build-deb.sh`, `nfpm.yaml`, `nfpm-agent.yaml`, `Dockerfile`,
`scripts/image-build/`, `.goreleaser.yaml`, `release-please-config.json`
and the workflows under `.github/workflows/`.

## Artifacts

One tree produces four kinds of artifact:

| Artifact | Built by | Carries |
|---|---|---|
| Debian packages `slagworks` and `slagworks-agent` | `scripts/build-deb.sh` | `slagworksd`, `slagctl`, `slagworks-agent`, the upgrade helper, systemd units, iPXE binaries |
| Container image | `Dockerfile` (from the two packages) | `slagworksd global` |
| Boot image `noble-pxe.tar.gz` | `scripts/image-build/build.sh` | `vmlinuz`, `initrd`, `initrd-local`, `root.squashfs` |
| slagctl release archives and Homebrew cask | goreleaser, on a release | `slagctl` for Linux and macOS |

This repository's CI builds the packages and the boot image as checks,
builds no container, and publishes only the slagctl release. Publishing
packages, the container and boot images to the places a deployment reads
them from is the deployment's job (see [Deploying](#deploying)).

## Binaries and Debian packages

The three binaries are pure Go. `scripts/build-deb.sh` builds them with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` and `-trimpath`, so any host with
the Go toolchain from `go.mod` can produce release artifacts, then packages
them with [nfpm](https://nfpm.goreleaser.com/) and deletes the intermediate
binaries. It needs git history with a reachable `v*` tag; a shallow clone
fails.

```sh
scripts/build-deb.sh   # writes slagworks_<version>_amd64.deb and slagworks-agent_<version>_amd64.deb
```

- `slagworks-agent` (`nfpm-agent.yaml`) holds `/usr/bin/slagworks-agent`
  only.
- `slagworks` (`nfpm.yaml`) holds `slagworksd`, `slagctl`, the
  `slagworks-upgrade` helper and its sudoers entry, the global and site
  systemd units, the two env example files under `/etc/slagworks/`, and the
  vendored iPXE binaries (`snponly.efi`, `undionly.kpxe`) under `/srv/tftp`.
  It depends on `systemd-boot-efi` and on `slagworks-agent` at exactly the
  same version, so both move together on a site worker.

The package version is the release-please version: `X.Y.Z` on a release
commit (the script parses release-please's `chore(main): release X.Y.Z`
subject, because the tag may not exist yet when that commit is built) and
`X.Y.Z+git.N.g<sha>` from `git describe` otherwise. The snapshot form sorts
above `X.Y.Z` and below the next release.

Two linker flags matter at runtime. `-X main.version=<git sha>` stamps the
commit into every binary; `slagworksd version` and `slagctl version` print
it, and site workers compare it with global's. `-X
main.packageVersion=<deb version>` stamps the package version into
`slagworksd`; global sends it with its SHA so an upgrading site worker knows
which package to install. A binary built any other way (`go build`,
`just build`) carries an empty package version, and site workers will not
auto-upgrade to it. The upgrade path is described in
[installation.md](../user/installation.md#site-worker-auto-upgrade).

For a compile check without packaging, `just build`; `just install` puts a
`slagctl` built from the checkout on your `$PATH`.

## Container image

`Dockerfile` is a single-stage Debian bookworm-slim image (pinned by digest)
that unpacks the two packages, exposes port 8081 with a `/readyz`
healthcheck, and runs `slagworksd global` under `dumb-init`. It copies the
debs from the build context, so build them first:

```sh
scripts/build-deb.sh
docker build -t slagworks .
```

## Boot image

`scripts/image-build/build.sh` builds the one PXE boot image that deploy and
commission both boot; [images.md](../user/images.md) describes what is in it
and how it reaches site workers. In short:

- It takes no arguments and writes `./noble-pxe.tar.gz`.
  `SLAGWORKS_RECOVERY_AUTHORIZED_KEYS` must name a file of `authorized_keys`
  lines for the recovery sshd baked into the initrd; the build fails
  without it.
- It runs `mmdebstrap` natively on a Linux x86_64 host and inside a Docker
  container anywhere else (`just image-build`).
- Packages come from the EC2 regional Ubuntu mirror unless
  `SLAGWORKS_APT_MIRROR` names another. The mirror is not date-pinned, so
  two builds of one commit can differ; a published per-commit tarball is
  the reproducible artifact.
- With `SLAGWORKS_CACHE_DIR` set, the chroot outputs are reused when no
  build input changed (CI's cache).
- A verify step at the end checks that the shipped initrd carries the boot
  hook from `assets/initramfs/scripts/slagworks`, `overlay.ko` and the
  recovery keys the build was given.

`scripts/image-build/smoke.sh` then boots the tarball under TCG QEMU against
a stub HTTP server and passes when the boot hook reports
`slagworks: agent ready`. It checks the userspace boot contract (kernel,
initrd, DHCP, HTTP fetches), not real drivers, disks or BMCs; the QEMU suite
covers those ([testing.md](testing.md)).

## CI checks

Every check below runs on a GitHub-hosted runner with no secrets. Pull
requests from forks wait for a maintainer to approve their workflow runs.

| Check | Workflow | What it does |
|---|---|---|
| `test` | `build.yaml` | `go test -race ./...`, then fails if `docs/user/slagctl-reference.md` is stale |
| `lint` | `build.yaml` | golangci-lint |
| `build` | `build.yaml` | `scripts/build-deb.sh`; the debs are a workflow artifact for seven days |
| `public-clean` | `build.yaml` | `scripts/ci/public-clean.sh`: no organization-specific identifiers in the tree |
| `security / govulncheck`, `security / gitleaks`, `security / actionlint` | `build.yaml`, calling the organization's reusable security workflow | reachable Go vulnerabilities, secrets in new commits, workflow lint with shellcheck |
| `discover`, then one `run-test` job per test | `test-integration.yaml` | integration tests outside `internal/qemutest` |
| `qemu` | `test-qemu.yaml` | the full-stack QEMU suite in shards; `qemu` is the single result, and on a pull request whose diff `scripts/ci/qemu-relevant.sh` finds nothing the suite can observe in, it passes without running the suite |
| `pr-title` | `pr-title.yml` | the title has a `type:` prefix, since it becomes the squash commit and the changelog line |

`test`, `lint`, `build`, `discover` and `qemu` run on every pull request
with no path filter, so they can be required checks without stranding a
docs-only change.

Two workflows run only when their inputs change:

- `build-images.yaml` (`image`, `smoke`) on changes to
  `scripts/image-build/`, `assets/initramfs/`, the boot-image action or
  itself: builds and smoke-tests the boot image with a throwaway recovery
  key when the repository variable is absent. A run on `main` warms the
  build cache that `test-qemu.yaml` restores through the same action.
- `build-ipxe.yaml` on changes to the iPXE inputs: rebuilds
  `assets/ipxe/*` reproducibly and opens a pull request when the binaries
  change.

`claude-review.yml` and `claude-mention.yml` run the organization's Claude
review on same-repository pull requests from people with write access; they
do not run for forks.

## Releases

Releases are driven by release-please (`release-please.yml`, using the
organization's reusable workflow, and `release-please-config.json`). Each
push to `main` updates a standing Release PR whose changelog is built from
the squash-merged pull request titles; changes only under `docs/` do not
count. Before 1.0 a breaking change bumps the minor version. Merging the
Release PR creates the `vX.Y.Z` tag and GitHub release. A Release PR made
only of dependency updates merges itself during the weekday release window;
any other waits for a maintainer.

A release is one version for the whole tree: slagctl, and the packages and
container built from the same commit, which `build-deb.sh` versions
`X.Y.Z`. When a release is created, `release-please.yml` runs:

- `goreleaser` (`.goreleaser.yaml`): builds `slagctl` for Linux and macOS on
  amd64 and arm64, attaches `slagctl_<version>_<os>_<arch>.tar.gz` and
  `checksums.txt` to the release, and publishes the `slagctl` cask to the
  public tap `bluesky-social/homebrew-tap`.
- `dispatch`: hands the release commit and tag to the maintainers'
  deployment pipeline (below).

Do not push tags by hand: that bypasses release-please's manifest. To
recover a failed release job, re-run it from the original workflow run.

### Installing slagctl

From the Homebrew tap (macOS and Linux):

```sh
brew tap bluesky-social/tap
brew trust --tap bluesky-social/tap   # Homebrew requires trusting non-official taps
brew install --cask slagctl
```

`brew upgrade slagctl` moves to a newer release, and slagctl says so when
one exists. Alternatively, download the archive for your platform from the
release page and check it against `checksums.txt`, or build it with Go:

```sh
go install github.com/bluesky-social/slagworks/cmd/slagctl@latest
```

A `go install` build reports version `dev`: slagctl never shows it an
upgrade notice and never refuses to run it for being a major version
behind.

## Deploying

The maintainers' deployment pipeline lives outside this repository. It
consumes commits from `main` through a `repository_dispatch` (event
`slagworks-build`, payload `{"sha": ..., "tag": ...}` with `tag` set only
for a release), sent by the `dispatch` jobs in `build.yaml`, after `test`,
`lint` and `build` pass on a push, and in `release-please.yml`. Both jobs
are guarded by the repository name, so they do nothing in a fork, and
nothing in this repository depends on what the pipeline does.

To run slagworks yourself, the contract is:

- Packages: build both debs with `scripts/build-deb.sh` from a commit with
  its tags and publish them to an apt repository your site workers can
  reach. Auto-upgrading workers install the exact package version global
  reports, so publish packages before rolling global to the same commit.
- Container: build it as above and run it wherever `slagworksd global`
  runs, or install the `slagworks` package on a host instead.
- Boot image: build it with your own recovery keys and upload it with the
  layout `slagctl image follow-latest` reads
  ([images.md](../user/images.md#publishing-an-image)).

## Which artifact carries a change

| Change touches | Artifact | Reaches the fleet when |
|---|---|---|
| `cmd/slagworks-agent/**` | `slagworks-agent` package | site workers upgrade their packages (automatic behind global on workers with `SLAGWORKS_AUTO_UPGRADE=1`); the initrd downloads the agent from the serving site worker at boot |
| site-worker code (`internal/worker/**`, `internal/httpboot/**`, `internal/dhcp/**`, `internal/tftp/**`) | `slagworks` package | site workers upgrade their packages, as above |
| global code (`internal/api/**`, `internal/snapshot/**`, `internal/store/**`) | container, or the `slagworks` package on a global host | global is redeployed |
| `scripts/image-build/**`, `assets/initramfs/**`, the recovery keys | boot image | the new tarball is published and `slagctl image follow-latest` (or a manual pin) moves the image row; site workers then sync it |

`slagctl image follow-latest` moves the boot image only. An agent fix that
does not take is usually a site worker still on the old package: the initrd
fetches the agent from the serving worker's `/usr/bin/slagworks-agent`, so
in a cage with two site workers both must be upgraded.
