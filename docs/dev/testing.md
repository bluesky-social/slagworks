# Testing

Status: live (generated from code, 2026-07-03)

Slagworks has three test tiers and one hard invariant that ties them
together.

- **Unit + component** — pure Go, `go test ./...`, drives the store,
  worker cache, and everything else that runs in-process.
- **Integration** — `//go:build integration`, one test binary per
  package, uses dockertest for supporting services (Postgres,
  NetBox). Runs in the `test-integration.yaml` workflow.
- **QEMU end-to-end** — same build tag, but requires `/dev/kvm`. Boots
  real VMs through a real PXE flow against an in-process FakeHub and
  SiteWorker. Runs in the `test-qemu.yaml` workflow, split into shards
  on GitHub-hosted runners.

The invariant, from `internal/qemutest/images.go` and the test-qemu
workflow: **the boot images the QEMU suite consumes are built from the
worktree by `scripts/image-build/build.sh` at the top of the job, using
the same chroot + `update-initramfs` path production uses**. There is
no swappable overlay initrd, no repack step, no substitution of a
canonical image with something more convenient. If a test asks
"does this boot," it asks that of the actual production boot artifact.

## Unit and component tests

```
just test [go test args]
```

Runs `go test ./...`. Database-backed tests start
`postgres:18-alpine` through Docker, pulled from Docker Hub unless
`SLAGWORKS_DOCKER_HUB_MIRROR` names a pull-through cache.

The toolchain is pure Go, CGO disabled. SQLite goes through
`glebarez/sqlite` (`modernc.org/sqlite` under the hood) so nothing in
the test binary needs a C compiler and the cross-compile from macOS to
Linux is native. Tests that need a real Postgres go through
`internal/testpg` (see `docs/dev/database.md`); tests that need SQLite
open a temp `LocalDB` directly.

## Integration tests

Marked with `//go:build integration` and live under `internal/netboxtest/`
(NetBox + Postgres, no VMs) and `internal/qemutest/` (full VM PXE).
The `test-integration.yaml` workflow runs the non-QEMU set; the QEMU
set is hard-coded skipped by `scripts/ci/list-integration-tests.sh`
and runs in `test-qemu.yaml` on a KVM host instead.

### Discovery matrix

`test-integration.yaml`'s `discover` job runs
`scripts/ci/list-integration-tests.sh` (in the repo). The script:

- finds every directory under the repo that contains at least one
  `//go:build integration` file (globbing only files with the tag
  keeps unit tests out of the matrix — integration tests compile
  WITH the tag, not ONLY with it),
- resolves each directory to its Go package path,
- filters out the hard-coded skip list (`internal/qemutest` today),
- lists every `TestXxx` in each surviving package via `go test -list`,
- emits a JSON array of `{pkg, test}` entries.

The `run-test` job fans that array out as a strategy matrix; one job
per test. Before the discover step, a `go test -tags integration -run '^$'`
compile-check runs across `./internal/...` so a syntax error in one
integration test doesn't silently drop its package from the matrix.

The `discover` and `run-test` jobs run on GitHub-hosted runners with no
credentials, so they are safe for pull requests from forks.

### netboxtest

`internal/netboxtest/netboxtest.go` brings up a per-test integration
environment: NetBox container (backed by its own Postgres and Redis)
plus a separate slagworks Postgres, all as dockertest containers.

- One stack per test — `t.Cleanup` tears everything down.
- The NetBox Postgres is restored from `dev/netbox/netbox-baseline.dump`
  before NetBox itself starts. Regenerate the dump after a NetBox
  version bump with `dev/netbox/regen-baseline.sh` (it refuses to run
  if the compose image tag and netboxtest.go's netboxTag disagree).
- The NetBox client is bootstrapped (`client.Bootstrap`) with the
  slagworks custom fields and tags on every test; the operation is
  idempotent so it's the same code path production uses.
- Slagworks Postgres runs `store.Migrate` before use, exercising the
  same schema production runs against.

Fixture regeneration lives at `just netbox-snapshot` (writes a fresh
`pg_dump` of the production NetBox RDS) — the code path is documented
in the fixture-not-found error inside `fixtureDumpPath`.

### qemutest

`internal/qemutest/qemutest.go` is the top-level harness. One
`qemutest.Env` per test; the env starts a `FakeHub` under an
`httptest.Server` and hands the test a subnet pool. Tests then build
one or more `Network`s, spin `VM`s in them, and drive deploys or
commissions through the FakeHub.

Key rules and shapes:

- **Requires `/dev/kvm`.** `requireKVM(t)` fatals if the device is
  absent or the mode doesn't grant access. GitHub-hosted Linux runners
  provide it; locally the suite needs a Linux host with KVM, root, and
  the tools `test-qemu.yaml` installs (QEMU, OVMF, `ipxe-qemu`, `ipmi_sim` from
  OpenIPMI, `ipmitool`, LVM, `qemu-utils`, `systemd-boot-efi`), plus
  `/etc/qemu/bridge.conf` containing `allow all`.
- **Subnet isolation.** `env.NewNetwork(t)` allocates a fresh `/24`
  from the `198.18.0.0/15` IANA benchmarking range (never routed on
  the host) via a per-process `subnetPool`. Do **not** hardcode
  subnet addresses in test files. The pool has 512 slots, seeded
  from `crypto/rand` at process start, and the bridge / netns names
  hash `runID + t.Name()` so two parallel invocations don't collide
  even if they land in the same slot.
- **Netns isolation.** Each `Network` sits inside its own Linux
  network namespace (`swns<hash>`). Bridge, gateway address, DHCP /
  TFTP / HTTP listeners, and the QEMU tap device all live inside
  that namespace. Cleanup order is kill-then-del: subprocesses still
  holding `/proc/self/ns/net` keep the netns alive in an anonymous
  form after `ip netns del`, so `t.Cleanup` first `SIGKILL`s
  everything in `ip netns pids <ns>` and then deletes the namespace.
  In CI every shard runs on a fresh machine, so nothing can leak from
  one run into the next.
- **BMC simulation.** `bmcsim.go` runs OpenIPMI's `ipmi_sim` outside
  the VM and wires it to QEMU's `ipmi-bmc-extern` + `isa-ipmi-kcs`
  so the guest sees a real `/dev/ipmi0`. Out-of-process (not the
  in-tree `ipmi-bmc-sim`) because the agent's reconcile phase
  exercises Set User Name / Password, which the in-process sim
  doesn't implement. SOL routing goes through a `qemutest-serialrelay`
  subprocess inside the netns.
- **Serial monitoring.** `serial.go` merges ttyS0 (firmware side —
  iPXE / SeaBIOS) and ttyS1 (kernel + agent, driven by
  `console=ttyS1,115200n8`) into one line-ordered stream, then
  exposes `WaitFor(ctx, marker, timeout)`. Every wait emits a
  `QEMUTEST_TIMING` record to stderr with kind, marker, duration,
  and outcome (`ok` / `timeout` / `cancelled`) for post-run analysis.
- **Wait budgets.** `budgets.go` centralizes per-marker timeouts,
  derived from observed runs at ~3× the maximum
  observed value with a 30s floor. Do **not** sprinkle `time.Minute`
  literals in call sites — bump the table and note why.
- **TestMatrix.** `matrix_integration_test.go` is a table-driven
  full-stack deploy lifecycle test crossed over firmware (UEFI /
  BIOS), storage layout (single / RAID1 / xfs RAID1 with GRUB on three
  disks), and failure mode
  (none / dirty-disk hard-fail / missing-efivarfs recoverable). It
  subsumed the older per-firmware `TestDeploy_*` and
  `TestDeployFullLifecycle_*` tests. Clean cells drive through
  `deploy complete` and then wait for `phone_home`; failure cells
  assert the FATAL marker instead of completion.

### Canonical images

The QEMU workflow reads `SLAGWORKS_IMAGES_DIR` and each test's
`env.UseCanonicalImages(t)` returns an `ImageDir` pointing at it.
`test-qemu.yaml`'s `image` job produces `noble-pxe.tar.gz` through
`.github/actions/boot-image`, the same action and cache key
`build-images.yaml` uses: when no image input changed since a previous
build the tarball is restored from the cache, otherwise it is built
with `scripts/image-build/build.sh`. The suite therefore boots the
bytes `build-images.yaml` builds and smoke-tests. Each shard downloads the tarball,
extracts it into `$RUNNER_TEMP/images/noble-pxe/`, and exports the
parent as `SLAGWORKS_IMAGES_DIR`. The env var name
matches the site-worker production var so nothing has to translate
between test and production layouts.

The `UseCanonicalImages` helper `t.Fatalf`s if the directory is
missing rather than skipping — a broken image build should turn CI
red, not silently green.

### How the workflow is laid out

`changes` → `image` and `shards` → `shard` (matrix) → `qemu`.

- `changes` runs `scripts/ci/qemu-relevant.sh` on a pull request. The
  change is relevant when it touches a directory of any Go package the
  suite's binaries are built from (vendored dependencies included, via
  `go list -deps`), the Go version, an image input or boot asset, or the
  workflow itself. Documentation, `slagctl`, and a dependency bump that
  only reaches `slagctl` are not; for those the remaining jobs are
  skipped. Pushes to `main` always run everything.
- `shards` runs `scripts/ci/qemu-shards.sh`, which deals the sorted
  test list round-robin into shards. Each shard runs its tests one at
  a time: a standard hosted runner has 2 vCPUs and 7 GB, and a test VM
  takes 2 to 4 GB.
- `shard` jobs run on Ubuntu 24.04, the release the boot image is
  built from (UEFI test VMs do not boot with 26.04's firmware stack),
  and build the agent with `CGO_ENABLED=0` as
  `scripts/build-deb.sh` does, because the agent runs inside the booted
  image, not on the runner.
- `qemu` is the one required check. It succeeds when every shard
  passed, or when `changes` found nothing relevant.

## Running one QEMU test ad-hoc

```
gh workflow run test-qemu.yaml -f run='TestMatrix/uefi-single-clean' -f parallel='1'
```

- `run=` is passed to `go test -run`. Empty (the default) runs
  everything.
- `parallel=` is passed to `go test -parallel` in each shard. The
  default is `1`.
- `shards=` sets the number of shards (default 12). With `run=`, only
  matching tests are dealt, so a single test lands in one shard.
- A new push to a pull request cancels that pull request's in-flight
  run. Runs for `main` are never cancelled.

## The "never pipe test output through filters" rule

From `CLAUDE.md`: save the full output to a file first, filter the
file. `grep`, `head`, `tee`-into-nothing, and friends will swallow
panics, stack traces, and useful stderr context that only matters
when a test fails. Two concrete consequences already burned in:

- The `test-qemu` workflow pipes `go test | tee` and needs the
  workflow-level `defaults.run.shell: bash` to activate `pipefail`.
  Without `pipefail`, `tee` exits `0` and a failed test inside the
  suite shows up in the results artifact but the workflow reports
  green.
- The `test-integration` workflow's `discover` step assigns the
  `list-integration-tests.sh` output to a shell variable before
  writing it to `$GITHUB_OUTPUT` for the same reason — a command
  substitution in the argument position would discard the script's
  exit status and emit an empty matrix on failure instead of
  failing the step.

## Known flakiness

- **Under-load slowdown at high parallelism.** The
  `uefi-raid1-missing-efivarfs` matrix cell deploys in ~57s solo but
  could blow a 3m30s wait budget at `-parallel 32` on the bare-metal
  runner the suite used before it moved to hosted runners.
  Root cause is not yet understood; `DeployComplete` is temporarily
  bumped to 6 minutes in `budgets.go` while it's investigated. Don't
  chase this by loosening other budgets — either the slow case is
  real and belongs in the table with a note, or it's a regression
  worth root-causing.
- **Docker Hub rate limits.** dockertest-based suites pull
  `postgres:18-alpine`, `redis:7-alpine`, and
  `netboxcommunity/netbox` on every run. CI routes those through an
  pull-through cache (`SLAGWORKS_DOCKER_HUB_MIRROR`); with it unset,
  as on a laptop, pulls go to Docker Hub.

## Cross-workflow context

- `build.yaml` runs `go test -race ./...` (unit + component only) in its
  `test` job, and its `build` job builds the `.deb`s as a packaging check.
  On a push to `main`, a commit that passes `test`, `lint` and `build` is
  dispatched to the maintainers' deployment pipeline
  (`docs/dev/build-and-release.md`).
- `test-integration.yaml` runs the discovery matrix. Its output is
  what the developer looks at when a change touches store /
  worker / netbox code.
- `test-qemu.yaml` is the slowest of the three (roughly 15 minutes
  of test time plus ~9 minutes of image build). It is required for
  any change touching the boot flow, storage layout, network-config
  bundle emission, or the agent's disk / bootloader install paths.
- `build-images.yaml` builds and smoke-tests the boot artifact when its
  inputs change and publishes nothing. The QEMU tests do not consume
  its workflow artifact; they restore or rebuild the image from the
  worktree through the same action and cache, for the reason stated at
  the top of this doc.
