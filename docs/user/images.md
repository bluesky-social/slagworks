# Images

Status: live (generated from code, 2026-07-03)

Slagworks provisions machines by PXE-booting them into a slagworks-aware
initrd and rsyncing a squashfs root filesystem onto their disks. This
document is the operator-facing view of that image: what it contains, how
it is built, how it gets to the site workers, and how a change lands on a
target host.

## The two-artifact model

A slagworks deploy actually depends on TWO independently released
artifacts. Getting an on-host change is often two rollouts, not one.

| Artifact | Contents | Ships via | How it reaches a target |
| -------- | -------- | --------- | ----------------------- |
| Boot image | Kernel, initrd, `initrd-local`, `root.squashfs` | Object storage (per-SHA URLs) → the `Image` DB row → site workers auto-sync | PXE-booted; the initrd downloads the squashfs at run time |
| `slagworks-agent` binary | The agent that partitions disks, rsyncs the OS, installs bootloaders, reconciles BMCs, etc. | The `slagworks-agent` .deb package, installed as a dependency of `slagworks` on the site worker | Fetched over HTTP by the initrd at `/files/bin/slagworks-agent` — served from the site worker's local `/usr/bin/slagworks-agent` |

The decision rule: does the change touch `cmd/slagworks-agent/**`? Then
it ships with the AGENT and needs the site worker's `slagworks` package
upgraded. Does it touch `scripts/image-build/**`, `assets/**`, or a
deployed-OS package list? Then it ships with the IMAGE and needs a new
image pin. Global-only changes (API, snapshot, matching) ship with the
global daemon and need neither.

The rest of this doc covers the image; the agent path is covered at the
end.

## What an image is

A slagworks image is a gzipped tarball with exactly four files in a single
top-level directory:

- `vmlinuz` — the kernel booted by iPXE.
- `initrd` — a stock Ubuntu initramfs-tools initrd with the slagworks BOOT
  hook (`assets/initramfs/scripts/slagworks`), the initramfs-tools hook
  (`assets/initramfs/hooks/slagworks`), the overlay/squashfs/loop and
  storage/RAID/DM modules, and the operator recovery pubkeys baked in
  at build time via `update-initramfs`. The `boot=slagworks` kernel
  cmdline tells stock `/init` to source the hook and run its
  `mountroot()`.
- `initrd-local` — the *stock* Ubuntu initrd (no slagworks hook). On
  UEFI deploys the agent copies it to the ESP as
  `slagworks/initrd.img` for post-deploy local boots (the systemd-boot
  entry points at it; the stock `/init` handles `root=UUID=` and mdadm
  assembly). Legacy-BIOS deploys don't use it — chrooted
  `grub-install`/`update-grub` uses the deployed rootfs's own
  `/boot` initrd. `scripts/image-build/manifest_test.go` statically
  couples the agent-fetched URLs to the build's artifact list so this
  file can't silently vanish from the tarball.
- `root.squashfs` — the deploy squashfs, mounted read-only by the initrd
  and overlaid with a tmpfs upper. The agent rsyncs this onto the
  target's disk during `rsyncing_os`. It includes everything the agent
  execs in-target: `dosfstools`, `mdadm`, `rsync`, `smartmontools`, FRR,
  Tailscale, and so on, per `scripts/image-build/packages-deploy.txt`.

There is exactly ONE image. Deploy and commission both boot this same
artifact — the only thing that distinguishes them is the
`slagworks.mode=commission` vs `slagworks.mode=deploy` kernel cmdline
the HTTP boot handler sets per-operation
(`internal/httpboot/handler.go`). Earlier versions had per-kind rows
(`noble-deploy` / `noble-commission`) which let the SHAs drift; those
are gone by design.

The image row's canonical name is `noble-pxe`; the on-disk directory,
the tarball, and the S3 prefix all use that name. See
`internal/model/image.go` for the model:

```go
type Image struct {
    ID        uuid.UUID
    Name      string    // e.g. "noble-pxe"
    Version   string    // e.g. "24.04"
    Arch      string    // e.g. "amd64"
    SHA256    string    // the tarball's content hash
    URL       string    // the immutable per-SHA URL
    CreatedAt time.Time
    SyncSeq   int64
}
```

## Building an image

`scripts/image-build/build.sh` is the single build script. It has no
arguments; it produces `./noble-pxe.tar.gz` plus a SHA256 line on stdout.

The build uses `mmdebstrap` natively on Linux x86_64 hosts and falls
back to docker (`ubuntu:noble` container) elsewhere. Two chroots are
built:

1. **PXE chroot** (`scripts/image-build/packages-pxe.txt`, small): kernel,
   `initramfs-tools`, minimal tooling. The customize-hook installs the
   slagworks BOOT hook + initramfs-tools hook + recovery
   `authorized_keys`, saves the stock initrd as `initrd-local` BEFORE
   installing the hooks, then runs `update-initramfs -u` inside the
   chroot. The resulting initrd IS the shipped initrd — there is no cpio
   repack after this stage. `vmlinuz` and both initrds are extracted out
   of the chroot; the chroot itself is discarded.
2. **Deploy chroot** (`packages-deploy.txt`, full): every package the
   deployed OS needs, plus every binary the agent execs in-target. Built
   directly to a `root.squashfs` via `mmdebstrap --format=squashfs` so
   root:root ownership survives the unshare-namespace build. Third-party
   apt repos (FRR, Tailscale) are baked in via `apt-repos.sh`.

The build then runs a verify step: unpack the shipped initrd (in a
container in docker mode, natively on Linux hosts) and assert:

- `/scripts/slagworks` is present and byte-identical to
  `assets/initramfs/scripts/slagworks`.
- `overlay.ko` is present in the initrd's kernel modules.
- `/etc/slagworks/recovery_authorized_keys` is present and matches the
  keys file the build was given.

It also reads `/etc/apt/sources.list` out of `root.squashfs` and requires
it to name `SLAGWORKS_APT_MIRROR` and nothing else, with no apt package
indexes shipped beside it.

Any mismatch fails the build. This gate catches packaging changes
(initramfs-tools dropping a script, `MODULES=most` filtering an
essential module) at build time rather than after four minutes of QEMU
smoke or a real-hardware deploy attempt.

`packages-deploy.txt` and `packages-pxe.txt` are the human-editable
package lists. Comments and inline `# why` annotations are stripped
before being handed to `apt-get`.

Two settings name Ubuntu archive mirrors:

- `SLAGWORKS_APT_MIRROR` is the mirror written into the image's
  `sources.list`, so it is the one deployed machines fetch packages from.
  Set it to a mirror close to those machines. The default is
  `http://archive.ubuntu.com/ubuntu`.
- `SLAGWORKS_APT_BUILD_MIRROR` is the mirror the build itself downloads
  from. It is optional and defaults to `SLAGWORKS_APT_MIRROR`. Set it when
  the build host is on a different network from the deployed machines: the
  build downloads several hundred packages, and a regional or
  cloud-provider mirror that is fast inside its own network can throttle
  outside clients enough to make the build take hours. A local
  apt-cacher-ng goes here too
  (`http://127.0.0.1:3142/archive.ubuntu.com/ubuntu`).

Nothing of the build mirror stays in the image: its `sources.list` names
only `SLAGWORKS_APT_MIRROR`, and no downloaded package index is shipped, so
a deployed machine's first `apt update` fetches the indexes from that
mirror. The build's verify step checks both. The two mirrors must serve the
same Ubuntu archive.

`apt-snapshot.txt` is informational only. Reproducibility is preserved
at the output layer: every build produces a content-addressed tarball
pinned in object storage, so "rebuild the same image" means downloading
that tarball, not re-running the build.

### Smoke test

`scripts/image-build/smoke.sh` boots the freshly-built image under
TCG-mode QEMU, serves a minimal stub squashfs and a stub agent, and
waits (up to 240s) for the marker `slagworks: agent ready` on the serial
console. That marker is emitted by the BOOT hook after the squashfs has
been overlay-mounted and the agent binary has been fetched — the
one-check gate that validates the whole boot contract before the tarball
is published.

## Publishing an image

This repository's CI builds and smoke-tests the image but publishes
nothing; publishing is part of a deployment
([build and release](../dev/build-and-release.md#deploying)). A
deployment uploads the tarball to an object-storage bucket under two
paths, both keyed on the build's git SHA:

- `<bucket>/noble-pxe/<sha>.tar.gz` — the immutable tarball
- `<bucket>/noble-pxe/<sha>.tar.gz.sha256` — the content SHA256, as bare
  hex with no filename

It then updates a mutable pointer, after both uploads succeed so the
pointer never names a missing tarball:

- `<bucket>/noble-pxe/latest-sha.txt` — the current head SHA

The pointer file is publishing bookkeeping. Site workers NEVER read
`latest-sha.txt`. The `Image` DB row is the sole authority for what
gets deployed to the fleet.

## The rollout: from tarball to fleet

Publishing a new tarball has ZERO effect on the fleet until the
`Image` row is updated to point at it. The operator drives that with
`slagctl image follow-latest`.

`slagctl image follow-latest` (`cmd/slagctl/cmd/image.go`):

1. GETs `<bucket>/noble-pxe/latest-sha.txt`, validates it as a 40-char
   hex SHA1.
2. GETs the `<sha>.tar.gz.sha256` sidecar, validates it as a 64-char hex
   sha256.
3. Builds the immutable per-SHA URL and PUTs an updated `Image` row via
   the global API (`PUT /api/v1/images/noble-pxe`).

The row now pins:

- `URL` = the immutable per-SHA tarball URL
- `SHA256` = the tarball's content hash

Without `--bucket-url`, the command follows the bucket of the image row
already registered under that name; pass `--bucket-url <base>` to register
the first image or to move to another bucket;
`--name` overrides the row name (default `noble-pxe`).

For manual rollback or a curated pin (staging a specific SHA that isn't
`latest-sha.txt`), use `slagctl image create` with `--name`, `--version`,
`--sha256`, and `--url` pointing at the known-good per-SHA tarball URL.
"Follow latest" and "manually pin" are the only two authoritative flows
for changing what the fleet runs; they both terminate in the same DB
row change.

## Site workers: how the row reaches a running deploy

Every site worker runs `imagepull.SyncImages`
(`internal/imagepull/imagepull.go`) each time it applies a site
snapshot, and again on a backoff after a failed download (see the
notes below). Each version of the image lives in its own directory,
`<images_dir>/noble-pxe@<first 16 hex digits of its SHA256>`, and
`<images_dir>/noble-pxe` is a symlink to the current one. The
algorithm:

1. Take an exclusive `flock` on `<images_dir>`, waiting for any other
   sync of that dir to finish, then remove the temp entries
   (`.image-download-*`, `.image-extract-*`, `.image-link-*`,
   `.image-old-*`) that a sync killed partway through left behind.
2. If `<images_dir>/noble-pxe` links to the row's version directory and
   that directory exists, do nothing — the image is up to date.
3. If the version directory is already on disk (a rollback to the
   version the link was last switched away from), skip to step 6.
4. Otherwise, HTTP-GET the row's `URL` into a temp file, stream through
   `sha256.New()` while writing, verify the hash matches the row (fail
   the whole sync if not).
5. Extract the tarball into a temp dir and check that its top-level
   directory holds all four image files (`vmlinuz`, `initrd`,
   `initrd-local`, `root.squashfs`) as regular files. A tarball missing
   one fails the sync for that image and leaves the link on the previous
   version, so pin a complete image. Then rename the extracted directory
   to `<images_dir>/noble-pxe@<sha16>`.
6. Switch the link: create a new symlink and rename it over
   `<images_dir>/noble-pxe`. The rename replaces the link in one step,
   so a request never finds the image missing. A failure before this
   step leaves the link, and the version it names, as they were.
7. Delete version directories other than the new one and the one the
   link named before the switch.

Why the previous version stays: each boot script names the version
directory current when it was served (`slagworks.image=noble-pxe@<sha16>`
and the kernel and initrd URLs), and the BOOT hook and the agent build
their URLs from it. A boot that was in flight when the image was
re-pinned keeps fetching from the version it booted: `root.squashfs`
from the hook, and on a UEFI deploy the agent's `vmlinuz` and
`initrd-local` for the ESP minutes later. Without that, the ESP could
get the new image's kernel over the old squashfs's `/lib/modules`. A
boot that spans two re-pins finds its version deleted, and its next
fetch fails with a 404 rather than mixing versions; deploy it again.

Notes:

- The row's `SHA256` is compared in lowercase with surrounding
  whitespace trimmed, so a digest pinned in upper case names the same
  tarball and the same version directory.
- The download runs in the background. While it runs the worker stays
  connected to global, answers power, boot and lease reads, applies
  operations and aborts, and keeps serving boots from the previous image.
  It holds commission and deploy operations until the download ends, so
  a deploy created after the row changed installs the new image. A
  download that fails releases them; they then boot the image already on
  disk.
- A sync that fails to fetch the tarball (a connection error, a
  response other than 200, a body cut short, a stalled download) is
  retried by the site worker itself: 5 seconds after the failure, then
  doubling up to every 5 minutes, until it succeeds or a newer
  snapshot's sync takes over. Nothing else would run it again, because
  global pushes no new snapshot while the site's content is unchanged.
  A digest mismatch, a tarball that does not extract, or a failed write
  to the images directory is logged and not retried, since another
  attempt would fail the same way (and a digest mismatch costs a full
  download each time): fix the row, which pushes a new snapshot, or fix
  the host and restart the site worker.
- A download that receives no data for a minute is abandoned and logged
  as `download stalled`; a slow download that keeps receiving data is
  never cut off.
- The row name is the link name. `SyncImages` skips, with an
  error, any name that fails `model.ValidateImageName` (lowercase
  letters, digits, `.`, `_`, `-`, starting with a letter or digit)
  before downloading or removing anything, and still syncs the other
  rows. The API refuses such names when the row is written. It skips a
  row whose `sha256` is not 64 hex digits the same way.
- A plain `noble-pxe/` directory (put there by hand, or extracted in
  place by a release before version directories) is replaced by the
  link on the next sync, which downloads the image once more. Until
  then the boot handler serves the plain directory by name. A
  `noble-pxe.sha256` file left by such a release is no longer read and
  can be deleted.
- No cpio repack, no InjectOverlayInit. The initrd downloaded IS the
  initrd served. That's the entire point of baking the BOOT hook at
  image-build time.
- The site worker's HTTP boot handler serves the images dir under
  `/files/images/`. Boot scripts point at
  `/files/images/noble-pxe@<sha16>/`, which is where iPXE fetches
  `vmlinuz` + `initrd`, the initrd fetches `root.squashfs`, and the
  agent fetches `vmlinuz` + `initrd-local` for a UEFI ESP.
- `slagctl image sync --dir <path>` runs the same code path
  interactively — useful for verifying an image works on a specific
  host or for pre-populating a new site worker. Pointed at a running
  worker's images dir, it logs that another sync is running and waits
  for the worker's sync to finish (and the worker likewise waits for
  it), so the two never interleave.

Diagnosing "the new image didn't roll": compare
`readlink <images_dir>/noble-pxe` (`noble-pxe@<sha16>`) against the
start of the SHA in `slagctl image list`. If they match (ignoring hex
case), the worker HAS synced — a bug is in the built image, not in the rollout.
If they differ, look at the site worker's image sync logs: `image sync
failed; retrying` with a `retry_in` is a failed download the worker is
retrying, and `image sync failed` with a `hint` is one it will not
retry. A mid-flight failure leaves the link on the previous version.
Do NOT look for a mtime or a `latest-sha.txt` cache: site workers never
poll the bucket, by design, and their only image timer is the retry
after a failed download.

## The other artifact: the slagworks-agent binary

The agent is emphatically NOT part of the image. It ships in its own
apt package (`slagworks-agent`, see `nfpm-agent.yaml`), which the
`slagworks` package depends on (`nfpm.yaml`).

The distribution path:

1. `scripts/build-deb.sh` builds `slagworks-agent` as
   `/usr/bin/slagworks-agent` inside the `slagworks-agent` .deb.
2. A deployment publishes the .deb to an apt repository the site
   workers can reach.
3. Site workers install/upgrade the package on the site worker host
   itself, by hand or through auto-upgrade behind global
   ([installation.md](installation.md#site-worker-auto-upgrade)).
4. When a target host PXE-boots and lands in the slagworks initrd, the
   BOOT hook fetches the agent from
   `http://<site>/files/bin/slagworks-agent` — which is served by the
   site worker's HTTP boot handler from
   `s.cfg.AgentBinPath()` (typically `/usr/bin/slagworks-agent`, wired
   up in `cmd/slagworksd/site.go`).

The upshot: an agent fix is live on the fleet the moment every serving
site worker has the new .deb installed. In an HA cage where BOTH
workers can serve a deploy, both must be upgraded — otherwise a deploy
that happens to be claimed by the stale worker will run the old agent.

Diagnosing "my agent fix didn't take": check the SERVING worker's
`dpkg -l slagworks-agent` package version. Confirm it includes the
commit you expect. `slagctl image follow-latest` does NOT roll the
agent — it only moves the image row.

## Full rollout summary, in order

For a change that spans everything:

1. **Land + push to `main`.** CI runs `build`, `build-images`, and the
   integration tests concurrently; your deployment builds and publishes
   the packages, container and image from that commit.
2. **Global-only changes**: roll the global daemon. Confirms deployed
   at `/readyz`.
3. **Image changes**: once the tarball for the commit is published and
   `latest-sha.txt` names it, run `slagctl image follow-latest`. Confirm each site
   worker synced: `readlink <images_dir>/noble-pxe` on the worker names
   `noble-pxe@` and the first 16 hex digits of `slagctl image list`'s
   pinned SHA.
4. **Agent changes**: upgrade the `slagworks` (or `slagworks-agent`)
   apt package on every site worker that will serve the deploy.
   Restarting `slagworksd-site.service` picks up the new binary.

## What to consult in code

- Image row: `internal/model/image.go`
- Sync algorithm: `internal/imagepull/imagepull.go`
- Image CLI: `cmd/slagctl/cmd/image.go`
- Build script: `scripts/image-build/build.sh`
- Package lists: `scripts/image-build/packages-pxe.txt`,
  `packages-deploy.txt`
- Third-party apt repos baked in: `scripts/image-build/apt-repos.sh`
- Smoke test: `scripts/image-build/smoke.sh`
- Initrd BOOT hook (baked into the initrd): `assets/initramfs/scripts/slagworks`
- Initrd tools hook (modules, keys): `assets/initramfs/hooks/slagworks`
- Recovery pubkeys source: the file named by
  `SLAGWORKS_RECOVERY_AUTHORIZED_KEYS` at build time (required; plain keys
  or a `cert-authority` line)
- Site worker's HTTP boot handler (serves `/files/images/`,
  `/files/bin/slagworks-agent`): `internal/httpboot/handler.go`
- Agent nfpm manifest: `nfpm-agent.yaml`
- Slagworks nfpm manifest (declares the agent dependency):
  `nfpm.yaml`
