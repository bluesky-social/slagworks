#!/usr/bin/env bash
# build.sh — produce the slagworks PXE-bootable image tarball.
#
# Usage:
#   bash scripts/image-build/build.sh
#
# There is ONE artifact. The deploy and commission "flavors" are NOT
# distinct images: they boot the identical kernel + initrd (from
# packages-pxe.txt) and rsync the identical root.squashfs (from
# packages-deploy.txt). The only thing that distinguishes a commission
# boot from a deploy boot is the `slagworks.mode=` kernel cmdline the
# site worker sets per-operation at boot time (see
# internal/httpboot/handler.go) — it is NOT baked into the image. So
# building two tarballs produced byte-identical boot artifacts and
# wasted a full second image build + smoke.
#
# This script builds that single artifact once. A deployment publishes it
# under ONE bucket prefix (noble-pxe/) for ONE Image DB row; deploy and commission
# boot the same artifact in different modes (the slagworks.mode= cmdline),
# not from different images. If deploy and commission ever need to DIVERGE
# in packages, reintroduce a flavor arg here and split packages-pxe.txt /
# packages-deploy.txt into flavor-specific lists.
#
# The two-chroot model:
#
#   1. PXE chroot (small): kernel + initramfs-tools + minimal tooling.
#      The slagworks BOOT hook (/scripts/slagworks) + initramfs-tools
#      hook are INSTALLED here, then update-initramfs bakes them into a
#      stock-init initrd (stock /init + udev + dhcpcd that sources our
#      hook for boot=slagworks). This chroot is consumed only for
#      vmlinuz + initrd; it's discarded after extraction.
#
#   2. Deploy chroot (full): the deployed-OS package set from
#      packages-deploy.txt. NO slagworks initramfs hooks — this chroot
#      becomes root.squashfs that gets rsync'd onto the host's disk.
#      Installing the PXE boot hook here would make every kernel update
#      on a deployed host regenerate its initrd with our BOOT hook —
#      bricking it on the next reboot.
#
# Both chroots share the same apt mirror so package versions match.
#
# Two settings name Ubuntu archive mirrors (see "Apt mirror" below):
#   SLAGWORKS_APT_MIRROR        written into the image's sources.list: what a
#                               deployed machine fetches packages from.
#   SLAGWORKS_APT_BUILD_MIRROR  what this build downloads from. Optional;
#                               defaults to SLAGWORKS_APT_MIRROR.
#
# Output: ./<suite>-pxe.tar.gz containing {vmlinuz, initrd,
# initrd-local, root.squashfs}, plus sha256 to stdout.
#
# Build host requirements:
#   - mmdebstrap (native Linux x86_64 host) OR docker (other hosts)
#   - mksquashfs
#   - sha256sum / shasum
#   - assets/initramfs/{scripts,hooks}/slagworks in the repo
#   - Network access to an Ubuntu archive mirror (SLAGWORKS_APT_BUILD_MIRROR,
#     or SLAGWORKS_APT_MIRROR when that is unset)

set -euo pipefail

#####################################################################
# Argument + environment                                            #
#####################################################################

usage() {
    cat <<USAGE >&2
Usage: $0

  No arguments. Builds the single PXE+squashfs artifact (see header).

Reads:
  scripts/image-build/apt-snapshot.txt    (pinned snapshot timestamp)
  scripts/image-build/packages-deploy.txt (deploy-chroot package list)
  scripts/image-build/packages-pxe.txt    (PXE-chroot package list)
  assets/initramfs/scripts/slagworks      (BOOT hook for stock /init)
  assets/initramfs/hooks/slagworks        (initramfs-tools hook)

Writes:
  ./noble-pxe.tar.gz
USAGE
    exit 1
}

[[ $# -eq 0 ]] || usage

# Suite is fixed at "noble" per Constraint 2. If we ever support a
# different Ubuntu version, lift this to an arg. NAME is the artifact
# basename + the directory inside the tarball; it is cosmetic to the
# worker, which extracts the tarball's sole top-level dir and renames
# it to the Image row name (imagepull.pullImage). "pxe" reflects that
# this one artifact backs both the deploy and commission Image rows.
SUITE="noble"
NAME="${SUITE}-pxe"

#####################################################################
# Source paths (resolve relative to the repo root)                  #
#####################################################################

# This script lives at scripts/image-build/build.sh. Find the repo
# root and then resolve everything from there, so the script can be
# invoked from any working directory.
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
IMAGE_BUILD_DIR="$REPO_ROOT/scripts/image-build"
# The slagworks BOOT hook + initramfs-tools hook. These are installed INTO the
# PXE chroot before update-initramfs, so the generated initrd is a stock
# Ubuntu initrd (stock /init, udev coldplug, dhcpcd, configure_networking) that
# carries our BOOT hook. No /init replacement, no cpio repack.
INITRAMFS_SCRIPT="$REPO_ROOT/assets/initramfs/scripts/slagworks"
INITRAMFS_HOOK="$REPO_ROOT/assets/initramfs/hooks/slagworks"

for f in "$INITRAMFS_SCRIPT" \
         "$INITRAMFS_HOOK" \
         "$IMAGE_BUILD_DIR/apt-snapshot.txt" \
         "$IMAGE_BUILD_DIR/mdadm.conf" \
         "$IMAGE_BUILD_DIR/packages-deploy.txt" \
         "$IMAGE_BUILD_DIR/packages-pxe.txt"; do
    [[ -f "$f" ]] || { echo "ERROR: required input missing: $f" >&2; exit 1; }
done

# Recovery authorized_keys
#
# Operator pubkeys for the deploy-initrd recovery sshd are a build input:
# SLAGWORKS_RECOVERY_AUTHORIZED_KEYS names a file of authorized_keys lines
# (plain keys, or a "cert-authority" line for an SSH CA). The PXE
# customize-hook stages the file into the chroot at
# /etc/slagworks/recovery_authorized_keys; the slagworks initramfs hook
# copies it into the generated initrd, and recovery_drop() reads it on
# agent failure or slagworks.recovery.
#
# No runtime fetch from the global API: recovery is for failures that may
# include network-down, where a fetch would deadlock. The deployed-OS
# authorized_keys for operator login is a separate lifecycle
# (model.SSHKey and the agent's writeSSHKeys() at deploy time).

RECOVERY_AUTHKEYS_SRC="${SLAGWORKS_RECOVERY_AUTHORIZED_KEYS:-}"
if [[ -z "$RECOVERY_AUTHKEYS_SRC" || ! -f "$RECOVERY_AUTHKEYS_SRC" ]]; then
    echo "ERROR: SLAGWORKS_RECOVERY_AUTHORIZED_KEYS must name a file of" >&2
    echo "       authorized_keys lines for the recovery sshd (got: '${RECOVERY_AUTHKEYS_SRC}')." >&2
    echo "       Example: SLAGWORKS_RECOVERY_AUTHORIZED_KEYS=~/.ssh/id_ed25519.pub" >&2
    exit 1
fi

#####################################################################
# Apt mirror                                                        #
#####################################################################

# Packages come from an Ubuntu archive mirror, not from
# snapshot.ubuntu.com: the snapshot service throttles and 503s under load.
# The tradeoff is that builds are not date-pinned. Reproducibility lives at
# the output layer instead: a published tarball is content-addressed, so
# "the same image" means downloading that tarball, not re-running the build.
# apt-snapshot.txt is informational only (roughly when the pipeline was last
# verified end to end).
#
# Two mirrors, because the right one for the build host and the right one
# for a deployed machine are often on different networks:
#
# SLAGWORKS_APT_MIRROR is the baked mirror: it is written into the image's
# sources.list, so it is what a deployed machine fetches packages from. Pick
# one close to the machines the image is deployed on.
#
# SLAGWORKS_APT_BUILD_MIRROR is the build mirror: every package this build
# downloads (both mmdebstrap runs) comes from it. It defaults to the baked
# mirror. Set it when the baked mirror is slow or unreachable from the build
# host: a regional or cloud-provider mirror is often ten times faster from
# inside its own network, and some throttle clients outside it to the point
# where the build exceeds a CI time limit. A build host with a local
# apt-cacher-ng points the build mirror at it (for example
# http://127.0.0.1:3142/archive.ubuntu.com/ubuntu). Both must serve the same
# Ubuntu archive. Nothing of the build mirror is left in the image: the
# sources.list is replaced with the baked mirror's (BAKED_SOURCES below) and
# mmdebstrap removes the downloaded package indexes, so a deployed machine's
# first `apt update` fetches them from the baked mirror.
#
# Third-party repos (apt-repos.sh) stay direct in both roles: they are small
# and https-pinned.
APT_MIRROR_BASE="${SLAGWORKS_APT_MIRROR:-http://archive.ubuntu.com/ubuntu}"
APT_BUILD_MIRROR_BASE="${SLAGWORKS_APT_BUILD_MIRROR:-$APT_MIRROR_BASE}"

# Third-party APT repos baked into the DEPLOY chroot (FRR, Tailscale, ...) live
# in apt-repos.sh as a data table + a shared key-fetch/deb-line/customize-hook
# helper, used by BOTH the native and docker build paths. Adding a repo is a
# one-row edit there. See that file for the signed-by/CWE-345 rationale.
# shellcheck source=scripts/image-build/apt-repos.sh
source "$IMAGE_BUILD_DIR/apt-repos.sh"

echo "==> Building $NAME (downloading from $APT_BUILD_MIRROR_BASE, baking apt mirror $APT_MIRROR_BASE)"

#####################################################################
# Pick the build runner (native mmdebstrap vs. docker)              #
#####################################################################

if command -v mmdebstrap &>/dev/null && [[ "$(uname -s)" == "Linux" ]] \
   && [[ "$(uname -m)" == "x86_64" ]]; then
    RUN_MODE=native
    echo "==> Using native mmdebstrap (Linux x86_64)"
elif command -v docker &>/dev/null; then
    RUN_MODE=docker
    echo "==> Using Docker (mmdebstrap not native to this host)"
    echo "    NOTE: --platform=linux/amd64 forces emulation on non-x86_64 hosts;"
    echo "          this is correct (production targets amd64) but slower."
else
    echo "ERROR: need either native mmdebstrap on Linux x86_64, or docker." >&2
    echo "       Install mmdebstrap on a Linux x86_64 build host, or install" >&2
    echo "       docker to build inside a container on any other host." >&2
    exit 1
fi

# The initrd is now generated entirely inside the PXE chroot by
# update-initramfs (stock /init + our BOOT hook baked in via the slagworks
# initramfs hook) — there is no cpio repack stage. The build-time verify step
# unpacks the final initrd to assert our BOOT hook + overlay module landed; in
# docker RUN_MODE it runs in a container, in native RUN_MODE it uses the host's
# unmkinitramfs. Only require docker up front when we'll actually use it.
if [[ "$RUN_MODE" == docker ]] && ! command -v docker &>/dev/null; then
    echo "ERROR: docker RUN_MODE selected but docker is not available." >&2
    exit 1
fi

#####################################################################
# Working dir + cleanup                                             #
#####################################################################

WORKDIR=$(mktemp -d -t "slagworks-build-${NAME}.XXXXXXXX")
# Native mmdebstrap unshare mode runs as a sub-uid via user-namespace
# mapping, and that sub-uid needs write access to children of WORKDIR.
# mktemp -d creates 0700 by default, which excludes everyone but the
# invoking user (and the sub-uid is technically a different uid even
# though it maps to ours). Loosen to 0755 so unshare can write.
chmod 0755 "$WORKDIR"

# Stage the inputs the PXE customize-hook installs into the chroot. They go
# under $WORKDIR (mounted at /work in docker mode, direct in native), in a
# single staging dir whose per-mode path is passed to the hook via
# SLAGWORKS_INITRAMFS_STAGE — same plumbing pattern as SLAGWORKS_PXE_OUT.
#   - recovery_authorized_keys : baked into the initrd by the slagworks hook
#   - scripts/slagworks        : the BOOT hook stock /init sources
#   - hooks/slagworks          : the initramfs-tools hook (modules + keys)
INITRAMFS_STAGE="$WORKDIR/initramfs-stage"
mkdir -p "$INITRAMFS_STAGE/scripts" "$INITRAMFS_STAGE/hooks"
RECOVERY_AUTHKEYS="$INITRAMFS_STAGE/recovery_authorized_keys"
install -m 0644 "$RECOVERY_AUTHKEYS_SRC" "$RECOVERY_AUTHKEYS"
install -m 0644 "$INITRAMFS_SCRIPT" "$INITRAMFS_STAGE/scripts/slagworks"
install -m 0755 "$INITRAMFS_HOOK" "$INITRAMFS_STAGE/hooks/slagworks"
authkey_count=$(grep -cvE '^[[:space:]]*(#|$)' "$RECOVERY_AUTHKEYS" || true)
if [[ "$authkey_count" -eq 0 ]]; then
    echo "ERROR: $RECOVERY_AUTHKEYS_SRC contains no usable key lines" >&2
    echo "       Refusing to bake an unrecoverable image. Put at least" >&2
    echo "       one operator key in that file before building." >&2
    exit 1
fi
echo "==> Staged $authkey_count recovery operator key(s) from $RECOVERY_AUTHKEYS_SRC"

# The image's /etc/apt/sources.list, naming the baked mirror. mmdebstrap
# writes the mirrors it downloads from (the build mirror) into the chroot's
# sources.list; a customize-hook (baked_sources_hook, apt-repos.sh) replaces
# that with this file in both chroots, and the verify step at the end compares
# the shipped image against it.
BAKED_SOURCES="$WORKDIR/baked-sources.list"
ubuntu_mirror_lines "$APT_MIRROR_BASE" "$SUITE" > "$BAKED_SOURCES"
chmod 0644 "$BAKED_SOURCES"
# Clean up anything we might have left mounted on early exit, then
# remove the workdir. In docker mode the chroot files are owned by
# container-root, so a plain `rm -rf` from the invoking user fails;
# we shell out to docker to remove them.
cleanup() {
    # PXE chroot is materialized as a directory (we need to chroot
    # in to update-initramfs); deploy is direct-to-squashfs so no
    # deploy-chroot dir to clean. The PXE chroot may have leftover
    # bind-mounts on early failure.
    if [[ -n "${PXE_CHROOT:-}" ]] && [[ -d "$PXE_CHROOT" ]]; then
        for mp in "$PXE_CHROOT/dev" "$PXE_CHROOT/sys" "$PXE_CHROOT/proc"; do
            umount "$mp" 2>/dev/null || true
        done
    fi
    if command -v docker &>/dev/null; then
        # The docker-mode verify step creates container-root-owned dirs
        # (verify-initrd.XXXX) under $WORKDIR. Without re-permissioning back
        # to the invoking user, the subsequent `rm -rf $WORKDIR` fails with
        # EACCES and leaks /tmp/slagworks-build-* dirs forever. Best-effort;
        # never mask the real exit code.
        local me
        me="$(id -u):$(id -g)"
        docker run --rm --platform=linux/amd64 -v "$WORKDIR:/work" \
            ubuntu:noble chown -R "$me" /work 2>/dev/null || true
    fi
    rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

OUT_DIR="$WORKDIR/$NAME"
mkdir -p "$OUT_DIR"
# Native mmdebstrap unshare mode runs the customize-hook as a
# sub-uid (mapped namespace-root). It needs to be able to drop
# kernel+initrd into OUT_DIR; mode 0755 is rx-only to others, so
# the sub-uid can't write there. Loosen to 0777 — this is a
# transient build dir under mktemp, no real exposure concern.
chmod 0777 "$OUT_DIR"
PXE_CHROOT="$WORKDIR/pxe-chroot"

chmod 0755 "$WORKDIR"

#####################################################################
# Helpers                                                           #
#####################################################################

# Strip comments and inline `# why` annotations from a package list,
# emit a space-separated string suitable for apt-get install.
package_list() {
    local file="$1"
    sed -e 's/#.*//' "$file" | awk '{print $1}' | grep -v '^$' | tr '\n' ' '
}

#####################################################################
# Build a chroot via mmdebstrap.                                    #
#                                                                   #
# Args:                                                             #
#   $1  rootfs target dir                                           #
#   $2  package list file                                           #
#   $3  customize-hook script (optional; pass "" to skip)           #
#####################################################################

build_chroot_native() {
    local target="$1" pkglist="$2" customize="$3" with_repos="${4:-}"

    local pkgs
    pkgs=$(package_list "$pkglist")

    local mmd_args=(
        --variant=minbase
        --components='main,restricted,universe,multiverse'
        --include="$pkgs"
        --aptopt='Acquire::Check-Valid-Until "false"'
        # Belt-and-suspenders against transient mirror blips. The
        # EC2 regional mirror is fast and reliable in practice, but
        # 5 in-process retries cost ~nothing if the underlying
        # mirror is healthy.
        --aptopt='Acquire::Retries "5"'
    )

    # mdadm's postinst generates /etc/mdadm/mdadm.conf by scanning the BUILD
    # host's block devices and /dev tree, unless the file already exists.
    # Install ours first (essential-hooks run before --include packages are
    # configured): the image must not depend on the build host, and the scan
    # hangs on a host with a very large /dev/shm. See mdadm.conf.
    if grep -qw mdadm <<<"$pkgs"; then
        # $1 is the chroot path mmdebstrap passes to the hook; the hook's
        # shell expands it, not this one.
        # shellcheck disable=SC2016
        mmd_args+=(
            --essential-hook='mkdir -p "$1/etc/mdadm"'
            --essential-hook="upload $IMAGE_BUILD_DIR/mdadm.conf /etc/mdadm/mdadm.conf"
        )
    fi

    # Third-party repos (deploy chroot only). The key fetch + signed-by deb-line
    # + chroot-relocate customize-hook all live in apt-repos.sh, shared with the
    # docker path. Native: mmdebstrap runs on the host, so keys go to $WORKDIR
    # (host-readable by build-apt). Customize-hooks are OPTIONS (added here,
    # before SUITE/TARGET); the deb-lines are MIRRORS (added after, below).
    if [[ -n "$with_repos" ]]; then
        fetch_repo_keys "$WORKDIR" "$SUITE"
        while IFS= read -r repo_arg; do
            mmd_args+=("$repo_arg")
        done < <(repo_customize_hooks "$WORKDIR" "$SUITE")
    fi

    # Leave the baked mirror, not the build mirror, in the chroot's
    # sources.list.
    mmd_args+=("$(baked_sources_hook "$BAKED_SOURCES")")

    # Pick output format from the target's extension. .squashfs goes
    # directly to mksquashfs via mmdebstrap, preserving ownership
    # without needing root (mmdebstrap defaults to --mode=unshare
    # when not running as root, which fakes root via user namespaces;
    # outputting to a directory then mksquashfs'ing that directory
    # would lose the namespaced uids and produce a squashfs with the
    # invoking user's uid/gid on root-owned files — broken /etc/shadow,
    # broken setuid bits).
    case "$target" in
        *.squashfs)
            mmd_args+=(--format=squashfs)
            ;;
    esac

    if [[ -n "$customize" ]]; then
        mmd_args+=(--customize-hook="$customize")
    fi

    mmd_args+=("$SUITE" "$target")
    # Append the build mirror's base + updates + security lines: these are
    # what mmdebstrap downloads from. One array element per line, so nothing
    # is word-split.
    local mirrors=()
    while IFS= read -r line; do mirrors+=("$line"); done < <(ubuntu_mirror_lines "$APT_BUILD_MIRROR_BASE" "$SUITE")
    mmd_args+=("${mirrors[@]}")

    # Third-party repo deb-lines are MIRRORS — appended after SUITE/TARGET,
    # alongside the base mirrors above. The customize-hooks for these repos
    # were already added (as options) before SUITE/TARGET.
    if [[ -n "$with_repos" ]]; then
        while IFS= read -r line; do mmd_args+=("$line"); done < <(repo_deb_lines "$WORKDIR" "$SUITE")
    fi

    mmdebstrap "${mmd_args[@]}"
}

# Same shape as build_chroot_native, but inside a docker container
# that has mmdebstrap pre-installed. The customize-hook script must
# be a path on the HOST that we'll bind-mount into the container.
build_chroot_docker() {
    local target="$1" pkglist="$2" customize="$3" with_repos="${4:-}"

    # Build a docker-side script that drives mmdebstrap. Easier to
    # debug than a one-liner: we get the script as a file on the host
    # via WORKDIR, and when something goes wrong we can re-run it
    # interactively.
    local driver="$WORKDIR/docker-build-driver.sh"
    cat > "$driver" <<'DRIVER'
#!/bin/bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

TARGET="$1"
SUITE="$2"
PKGS="$3"
CUSTOMIZE="$4"            # path inside container, or empty
MIRROR_LINES_FILE="$5"    # file with one build-mirror deb-line per row
WITH_REPOS="$6"           # non-empty = bake third-party repos (deploy chroot)
BAKED_SOURCES="$7"        # the image's sources.list (baked mirror), container path

apt-get update -qq
# squashfs-tools-ng provides tar2sqfs which mmdebstrap uses for the
# direct-to-squashfs output mode. The classic squashfs-tools package
# has mksquashfs but not tar2sqfs.
# curl is needed by the third-party-repo key fetch (apt-repos.sh); ubuntu:noble
# ships without it. Harmless when the deploy chroot doesn't add repos.
apt-get install -y -qq mmdebstrap squashfs-tools-ng curl >/dev/null

mmd_args=(
    --variant=minbase
    --components='main,restricted,universe,multiverse'
    --include="$PKGS"
    --aptopt='Acquire::Check-Valid-Until "false"'
    # See native block above for the retry rationale.
    --aptopt='Acquire::Retries "5"'
)
# Output format keyed off the target extension (.squashfs vs. dir).
# Same rationale as build_chroot_native — mmdebstrap can pipe to
# mksquashfs while it has the right ownership context, avoiding the
# directory-then-squashfs ownership-loss bug.
case "$TARGET" in
    *.squashfs) mmd_args+=(--format=squashfs) ;;
esac

# Install our mdadm.conf before mdadm is configured; see the native block
# above for why.
if grep -qw mdadm <<<"$PKGS"; then
    mmd_args+=(
        --essential-hook='mkdir -p "$1/etc/mdadm"'
        --essential-hook='upload /repo/scripts/image-build/mdadm.conf /etc/mdadm/mdadm.conf'
    )
fi

# Third-party repos (deploy chroot only), via the SAME apt-repos.sh the native
# path uses (bind-mounted at /repo). build-apt reads signed-by paths where
# mmdebstrap runs — here, inside this container — so keys go to /work (a
# container path). Customize-hooks are OPTIONS (added before SUITE/TARGET); the
# deb-lines are MIRRORS (added after, below) — ordering matters to mmdebstrap.
source /repo/scripts/image-build/apt-repos.sh
if [[ -n "$WITH_REPOS" ]]; then
    fetch_repo_keys /work "$SUITE"
    while IFS= read -r repo_arg; do
        mmd_args+=("$repo_arg")
    done < <(repo_customize_hooks /work "$SUITE")
fi

# Leave the baked mirror, not the build mirror, in the chroot's sources.list.
mmd_args+=("$(baked_sources_hook "$BAKED_SOURCES")")

if [[ -n "$CUSTOMIZE" ]]; then
    # Already +x on the host (we did chmod before bind-mounting).
    # The bind-mount is :ro so chmodding here would fail anyway.
    mmd_args+=(--customize-hook="$CUSTOMIZE")
fi
mmd_args+=("$SUITE" "$TARGET")
# Append the build mirror's base + updates + security lines (read from the
# file the host wrote out and bind-mounted in).
while IFS= read -r line; do
    [[ -n "$line" ]] && mmd_args+=("$line")
done < "$MIRROR_LINES_FILE"

# Third-party repo deb-lines are MIRRORS — after SUITE/TARGET (apt-repos.sh
# is sourced above).
if [[ -n "$WITH_REPOS" ]]; then
    while IFS= read -r line; do mmd_args+=("$line"); done < <(repo_deb_lines /work "$SUITE")
fi

mmdebstrap "${mmd_args[@]}"
DRIVER
    chmod +x "$driver"

    # Write the build mirror's lines to a file the container can read. This
    # is how SLAGWORKS_APT_BUILD_MIRROR reaches the mmdebstrap in the
    # container; the baked mirror reaches it as $BAKED_SOURCES under /work.
    local mirror_file="$WORKDIR/mirror-lines.txt"
    ubuntu_mirror_lines "$APT_BUILD_MIRROR_BASE" "$SUITE" > "$mirror_file"

    local pkgs
    pkgs=$(package_list "$pkglist")

    # Mount points inside the container:
    #   /work        — workdir, including chroot output
    #   /repo        — slagworks repo (kept for future hooks)
    #   /driver.sh   — the driver above
    #   /customize   — the customize hook (if any)
    #
    # The PXE customize-hook installs the slagworks BOOT hook + initramfs hook +
    # recovery keys from SLAGWORKS_INITRAMFS_STAGE (a dir under $WORKDIR =
    # /work), then update-initramfs bakes them into the final initrd. Both
    # SLAGWORKS_* paths are passed through so the hook finds its inputs and
    # output dir regardless of run mode.
    local docker_args=(
        --rm
        --platform=linux/amd64
        --privileged
        -v "$WORKDIR:/work"
        -v "$REPO_ROOT:/repo:ro"
        -v "$driver:/driver.sh:ro"
        -v "$mirror_file:/mirror-lines.txt:ro"
        -e "SLAGWORKS_PXE_OUT=$SLAGWORKS_PXE_OUT"
        -e "SLAGWORKS_INITRAMFS_STAGE=$SLAGWORKS_INITRAMFS_STAGE"
    )
    local container_customize=""
    if [[ -n "$customize" ]]; then
        docker_args+=(-v "$customize:/customize.sh:ro")
        container_customize="/customize.sh"
    fi
    # Translate host paths into the container's mount layout. The
    # target lives under $WORKDIR on the host, mounted at /work in
    # the container; substitute the prefix. The leading `#` anchors
    # to start-of-string; the second slash is the replacement
    # separator (no escaping needed).
    local container_target="${target/#$WORKDIR//work}"

    # The driver sources apt-repos.sh from the bind-mounted /repo and bakes the
    # third-party repos when WITH_REPOS is non-empty (deploy chroot only; the
    # PXE chroot passes "" and gets none). The repo table + key URLs live in
    # apt-repos.sh, so nothing repo-specific is threaded through here.
    docker_args+=(
        ubuntu:noble
        bash /driver.sh
            "$container_target"
            "$SUITE"
            "$pkgs"
            "$container_customize"
            "/mirror-lines.txt"
            "$with_repos"
            "${BAKED_SOURCES/#$WORKDIR//work}"
    )

    docker run "${docker_args[@]}"
}

build_chroot() {
    if [[ "$RUN_MODE" == native ]]; then
        build_chroot_native "$@"
    else
        build_chroot_docker "$@"
    fi
}

#####################################################################
# Customize hook for the PXE chroot.                                #
#                                                                   #
# mmdebstrap's --customize-hook runs after package install/configure,#
# so initramfs-tools' postinst has already generated a stock         #
# /boot/initrd.img-* with the stock /init. We:                       #
#   1. Save that pristine stock initrd as initrd-local (post-deploy  #
#      ESP/local boot uses the standard Ubuntu /init).               #
#   2. Install the slagworks BOOT hook (/scripts/slagworks), the     #
#      initramfs-tools hook (/usr/share/initramfs-tools/hooks/        #
#      slagworks), and the recovery authorized_keys into the chroot. #
#   3. Run update-initramfs -u, which regenerates the initrd as a    #
#      STOCK Ubuntu initrd (stock /init, udev coldplug, dhcpcd,      #
#      configure_networking) that now ALSO carries our BOOT hook +   #
#      modules + recovery keys. boot=slagworks on the kernel cmdline #
#      makes stock /init source our hook.                            #
#   4. Copy kernel + the FINAL initrd out. There is NO cpio repack   #
#      stage — the initrd update-initramfs produced IS the shipped   #
#      one.                                                          #
#####################################################################

PXE_CUSTOMIZE_HOOK="$WORKDIR/pxe-customize.sh"
cat > "$PXE_CUSTOMIZE_HOOK" <<'HOOK'
#!/bin/bash
# mmdebstrap customize-hook: $1 is the chroot path on the host (in
# unshare mode, accessible from inside the user namespace).
set -euxo pipefail

CHROOT="$1"
PXE_OUT="${SLAGWORKS_PXE_OUT:?SLAGWORKS_PXE_OUT must be set}"
STAGE="${SLAGWORKS_INITRAMFS_STAGE:?SLAGWORKS_INITRAMFS_STAGE must be set}"

mkdir -p "$PXE_OUT"

# Save the STOCK Ubuntu initrd as initrd-local BEFORE we install our hooks.
# Used by the post-deploy ESP/local boot — the standard Ubuntu /init handling
# root=UUID= + mdadm assembly. The agent's installBootloader copies it to the
# ESP. This must be the un-slagworks initrd.
install -m 0644 "$CHROOT"/boot/initrd.img-*-generic "$PXE_OUT/initrd-local"

# Install the slagworks BOOT hook. mkinitramfs auto-copies everything under
# /usr/share/initramfs-tools/scripts/ into the initrd's /scripts/, so dropping
# our script there is all it takes for stock /init to find it via
# `. /scripts/slagworks` when boot=slagworks is on the cmdline.
install -m 0755 "$STAGE/scripts/slagworks" \
    "$CHROOT/usr/share/initramfs-tools/scripts/slagworks"

# Install the slagworks initramfs-tools hook. It runs at update-initramfs time
# and bakes in the overlay/squashfs/loop + storage/raid/dm modules, a real
# wget + chroot, and the recovery authorized_keys.
install -m 0755 "$STAGE/hooks/slagworks" \
    "$CHROOT/usr/share/initramfs-tools/hooks/slagworks"

# Stage the recovery operator pubkeys where the slagworks initramfs hook reads
# them (it copies this into the initrd at the same path). Source: the
# SLAGWORKS_RECOVERY_AUTHORIZED_KEYS build input.
mkdir -p "$CHROOT/etc/slagworks"
install -m 0644 "$STAGE/recovery_authorized_keys" \
    "$CHROOT/etc/slagworks/recovery_authorized_keys"

# Regenerate the initrd inside the chroot. With our hook installed,
# update-initramfs produces the FINAL slagworks initrd in one shot: stock
# /init + udev + dhcpcd + our BOOT hook + modules + keys. No repack follows.
chroot "$CHROOT" update-initramfs -u

# Copy kernel + the FINAL initrd out. This initrd is shipped as-is.
install -m 0644 "$CHROOT"/boot/vmlinuz-*-generic    "$PXE_OUT/vmlinuz"
install -m 0644 "$CHROOT"/boot/initrd.img-*-generic "$PXE_OUT/initrd"
HOOK
chmod +x "$PXE_CUSTOMIZE_HOOK"

# The hook script needs SLAGWORKS_PXE_OUT (where to drop kernel+initrd
# output). In native mode the hook executes in mmdebstrap's unshare
# namespace but env vars are inherited. In docker mode the env var is
# passed via `docker run -e`.
if [[ "$RUN_MODE" == native ]]; then
    export SLAGWORKS_PXE_OUT="$OUT_DIR"
    export SLAGWORKS_INITRAMFS_STAGE="$INITRAMFS_STAGE"
else
    export SLAGWORKS_PXE_OUT="${OUT_DIR/#$WORKDIR//work}"
    export SLAGWORKS_INITRAMFS_STAGE="${INITRAMFS_STAGE/#$WORKDIR//work}"
fi

#####################################################################
# Build the chroots (slow, cacheable in CI).                        #
#                                                                   #
# When CI sets SLAGWORKS_CACHE_DIR, we look there for the build      #
# outputs (vmlinuz, initrd, initrd-local, root.squashfs) keyed on   #
# (packages-*.txt + apt-snapshot.txt + build.sh + assets/initramfs).#
# If present, skip the chroot build entirely. The baked mirror is   #
# an input too (it is inside root.squashfs): whoever sets            #
# SLAGWORKS_APT_MIRROR keys the cache on it, and the verify step     #
# below fails a cached root.squashfs that bakes another mirror. The  #
# build mirror is not an input: it leaves nothing in the image.      #
#                                                                   #
# Unlike the old two-phase build, the BOOT hook + initramfs hook are #
# baked in by update-initramfs INSIDE the chroot now, so the         #
# generated initrd IS the final shipped initrd — assets/initramfs/* #
# changes must be part of the CI cache key (the .github workflow     #
# hashes them) or a stale initrd would be served.                   #
#                                                                   #
# When SLAGWORKS_CACHE_DIR is unset (local dev), no caching, normal #
# flow — every invocation rebuilds from scratch.                    #
#####################################################################

PHASE_A_DONE=0
PHASE_A_OUTPUTS=(vmlinuz initrd initrd-local root.squashfs)
if [[ -n "${SLAGWORKS_CACHE_DIR:-}" ]]; then
    cache_complete=1
    for f in "${PHASE_A_OUTPUTS[@]}"; do
        [[ -f "$SLAGWORKS_CACHE_DIR/$f" ]] || { cache_complete=0; break; }
    done
    if [[ $cache_complete -eq 1 ]]; then
        echo
        echo "==> Phase A cache hit ($SLAGWORKS_CACHE_DIR), skipping chroot build"
        for f in "${PHASE_A_OUTPUTS[@]}"; do
            cp "$SLAGWORKS_CACHE_DIR/$f" "$OUT_DIR/$f"
        done
        PHASE_A_DONE=1
    else
        echo "==> Phase A cache miss at $SLAGWORKS_CACHE_DIR, will rebuild"
    fi
fi

if [[ $PHASE_A_DONE -eq 0 ]]; then
    echo
    echo "==> Building PXE chroot (kernel + initrd source)"
    build_chroot "$PXE_CHROOT" "$IMAGE_BUILD_DIR/packages-pxe.txt" "$PXE_CUSTOMIZE_HOOK"

    [[ -f "$OUT_DIR/vmlinuz" ]]      || { echo "ERROR: vmlinuz not extracted by customize-hook" >&2; exit 1; }
    [[ -f "$OUT_DIR/initrd" ]]       || { echo "ERROR: initrd not generated by customize-hook" >&2; exit 1; }
    [[ -f "$OUT_DIR/initrd-local" ]] || { echo "ERROR: initrd-local not extracted by customize-hook" >&2; exit 1; }

    echo
    echo "==> Building deploy root.squashfs"
    # Direct-to-squashfs preserves the chroot's intended root:root ownership
    # without needing the build host to run as root — mmdebstrap pipes the
    # unpacked rootfs into mksquashfs with ownership info baked into the
    # unshare-namespace context. Building to a directory first then
    # squashfs'ing it would lose that.
    # 4th arg "with-repos" bakes the third-party APT repos (FRR, Tailscale —
    # see apt-repos.sh) into the deploy chroot so those packages are on the
    # deployed host. The network-config / enrollment steps only enable units;
    # they can't apt-install. PXE chroot gets no 4th arg — it needs none.
    build_chroot "$OUT_DIR/root.squashfs" "$IMAGE_BUILD_DIR/packages-deploy.txt" "" "with-repos"

    if [[ -n "${SLAGWORKS_CACHE_DIR:-}" ]]; then
        echo
        echo "==> Saving Phase A outputs to cache: $SLAGWORKS_CACHE_DIR"
        mkdir -p "$SLAGWORKS_CACHE_DIR"
        for f in "${PHASE_A_OUTPUTS[@]}"; do
            cp "$OUT_DIR/$f" "$SLAGWORKS_CACHE_DIR/$f"
        done
    fi
fi

#####################################################################
# Verify the FINAL initrd: BOOT hook + overlay module + keys present#
#                                                                   #
# update-initramfs produced the shipped initrd inside the chroot, so#
# there is no repack to go wrong. But we still unpack and assert the #
# three things our boot path depends on actually landed, so a        #
# packaging change (initramfs-tools dropping a script, a hook not    #
# running, MODULES filtering) fails at build time rather than after  #
# 4 minutes of QEMU smoke or a real-hardware deploy attempt:         #
#   - /scripts/slagworks must be present (the BOOT hook stock /init  #
#     sources for boot=slagworks). Must match our staged copy.       #
#   - kernel/fs/overlayfs/overlay.ko must be present (the overlay    #
#     root mount needs it; catches MODULES filtering dropping it).   #
#   - /etc/slagworks/recovery_authorized_keys must be present and    #
#     match the PR-gated source.                                     #
# And one thing about root.squashfs: its sources.list names the      #
# baked apt mirror only, and it ships no apt package indexes.        #
#                                                                   #
# In docker RUN_MODE we unpack in a container (host may lack         #
# unmkinitramfs); in native RUN_MODE the host has initramfs-tools.   #
#####################################################################

echo "==> Verifying final initrd: BOOT hook + overlay module + recovery keys"
verify_dir=$(mktemp -d -p "$WORKDIR" verify-initrd.XXXX)
chmod 0777 "$verify_dir"
if [[ "$RUN_MODE" == docker ]]; then
    docker run --rm --platform=linux/amd64 \
        -v "$OUT_DIR:/out:ro" \
        -v "$verify_dir:/verify" \
        ubuntu:noble bash -c "
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq && apt-get install -y -qq initramfs-tools-core squashfs-tools-ng >/dev/null 2>&1
            cd /verify && unmkinitramfs /out/initrd .
            rdsquashfs -c /etc/apt/sources.list /out/root.squashfs > /verify/shipped-sources.list
            rdsquashfs -l /var/lib/apt/lists /out/root.squashfs > /verify/shipped-apt-lists.txt
        "
else
    ( cd "$verify_dir" && unmkinitramfs "$OUT_DIR/initrd" . )
    # rdsquashfs ships with tar2sqfs (squashfs-tools-ng), which mmdebstrap
    # needs for its squashfs output, so a native build host has it.
    rdsquashfs -c /etc/apt/sources.list "$OUT_DIR/root.squashfs" > "$verify_dir/shipped-sources.list"
    rdsquashfs -l /var/lib/apt/lists "$OUT_DIR/root.squashfs" > "$verify_dir/shipped-apt-lists.txt"
fi

# unmkinitramfs lays the main cpio out either at top level (single-segment)
# or under ./main/ (multi-segment, the noble norm). Probe both.
shipped_script=""
for candidate in "$verify_dir/scripts/slagworks" "$verify_dir/main/scripts/slagworks"; do
    [[ -f "$candidate" ]] && shipped_script="$candidate" && break
done
if [[ -z "$shipped_script" ]]; then
    echo "ERROR: built initrd is missing /scripts/slagworks (BOOT hook)" >&2
    echo "       update-initramfs did not copy our boot script. Layout:" >&2
    ls -la "$verify_dir" "$verify_dir"/main 2>/dev/null >&2
    exit 1
fi
if ! diff -q "$shipped_script" "$INITRAMFS_STAGE/scripts/slagworks" >/dev/null; then
    echo "ERROR: shipped /scripts/slagworks does not match assets/initramfs/scripts/slagworks" >&2
    diff "$shipped_script" "$INITRAMFS_STAGE/scripts/slagworks" | head -30 >&2 || true
    exit 1
fi
if ! find "$verify_dir" -name 'overlay.ko*' -print -quit | grep -q .; then
    echo "ERROR: shipped initrd is missing kernel/fs/overlayfs/overlay.ko" >&2
    echo "       The slagworks initramfs hook's manual_add_modules overlay did" >&2
    echo "       not take, or MODULES filtering dropped it. Layout:" >&2
    ls -la "$verify_dir" >&2
    exit 1
fi
shipped_authkeys=""
for candidate in "$verify_dir/etc/slagworks/recovery_authorized_keys" \
                 "$verify_dir/main/etc/slagworks/recovery_authorized_keys"; do
    [[ -f "$candidate" ]] && shipped_authkeys="$candidate" && break
done
if [[ -z "$shipped_authkeys" ]]; then
    echo "ERROR: shipped initrd missing /etc/slagworks/recovery_authorized_keys" >&2
    echo "       The slagworks initramfs hook did not bake recovery pubkeys." >&2
    exit 1
fi
if ! diff -q "$shipped_authkeys" "$RECOVERY_AUTHKEYS" >/dev/null; then
    echo "ERROR: shipped recovery_authorized_keys does not match the staged list" >&2
    diff "$shipped_authkeys" "$RECOVERY_AUTHKEYS" | head -30 >&2 || true
    exit 1
fi

# The deployed root filesystem's apt configuration names the baked mirror and
# nothing else: sources.list is exactly the baked mirror's lines (the build
# mirror and the third-party build-path lines are gone), and no downloaded
# package index, whose file name carries the host it came from, is shipped.
# This runs on a cache hit too, so a cached root.squashfs built for another
# baked mirror fails here instead of shipping.
if ! diff -q "$verify_dir/shipped-sources.list" "$BAKED_SOURCES" >/dev/null; then
    echo "ERROR: root.squashfs /etc/apt/sources.list does not name the baked apt mirror" >&2
    echo "       ($APT_MIRROR_BASE). Shipped (<) against expected (>):" >&2
    diff "$verify_dir/shipped-sources.list" "$BAKED_SOURCES" | head -30 >&2 || true
    echo "       If SLAGWORKS_CACHE_DIR holds outputs built with a different" >&2
    echo "       SLAGWORKS_APT_MIRROR, clear it and rebuild." >&2
    exit 1
fi
if grep -q '_dists_' "$verify_dir/shipped-apt-lists.txt"; then
    echo "ERROR: root.squashfs ships apt package indexes in /var/lib/apt/lists:" >&2
    grep '_dists_' "$verify_dir/shipped-apt-lists.txt" | head -10 >&2
    echo "       Their names carry the mirror the build downloaded from, and" >&2
    echo "       mmdebstrap's cleanup normally removes them. Remove whatever" >&2
    echo "       keeps them (a --skip=cleanup/apt option, an apt-get update in" >&2
    echo "       a customize-hook) and rebuild." >&2
    exit 1
fi
echo "    /scripts/slagworks matches assets/initramfs/scripts/slagworks"
echo "    overlay.ko present in initrd"
echo "    recovery_authorized_keys present and matches"
echo "    root.squashfs sources.list names $APT_MIRROR_BASE only; no apt indexes shipped"

#####################################################################
# Sanity-check artifacts                                            #
#####################################################################

for f in vmlinuz initrd initrd-local root.squashfs; do
    [[ -f "$OUT_DIR/$f" ]] || { echo "ERROR: missing $f after build" >&2; exit 1; }
done

# Docker-built artifacts are owned by root (the container's uid).
# tar from a non-root user would fail on root-owned 0600 files.
# Re-permission via docker so the subsequent tar step works.
if [[ "$RUN_MODE" == docker ]]; then
    docker run --rm --platform=linux/amd64 -v "$OUT_DIR:/output" ubuntu:noble \
        chmod -R a+rX /output >/dev/null
fi

#####################################################################
# Tar + sha256                                                      #
#####################################################################

echo
echo "==> Built artifacts:"
ls -lh "$OUT_DIR/"

echo
echo "==> Packaging tarball"
TARBALL="${NAME}.tar.gz"
tar -czf "$TARBALL" -C "$WORKDIR" "$NAME"

if command -v sha256sum &>/dev/null; then
    SHA=$(sha256sum "$TARBALL" | cut -d' ' -f1)
else
    # macOS lacks sha256sum; shasum -a 256 is equivalent.
    SHA=$(shasum -a 256 "$TARBALL" | cut -d' ' -f1)
fi

echo
echo "Built: $TARBALL"
echo "SHA256: $SHA"
echo "Apt mirror baked into the image: $APT_MIRROR_BASE"
echo "Apt mirror downloaded from: $APT_BUILD_MIRROR_BASE"
