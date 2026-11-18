#!/usr/bin/env bash
# smoke.sh — boot the built image under TCG-mode QEMU, assert the
# BOOT hook reaches the post-fetch marker.
#
# Usage:
#   bash scripts/image-build/smoke.sh
#
# There is ONE artifact (noble-pxe.tar.gz) — see build.sh's header.
# The image is mode-agnostic (deploy vs commission is a boot-time
# kernel cmdline, not baked in), so a single smoke run validates the
# whole boot contract; there is no per-flavor smoke.
#
# This is the smoke gate for CI. It validates the userspace boot
# contract: kernel loads, virtio-net comes up, DHCP gets a lease
# via SLIRP, DNS resolves, HTTP fetches against a stub server
# succeed, the BOOT hook reaches the post-fetch marker.
#
# It does NOT validate real-hardware NIC drivers, real disk
# controllers, or BMC paths — that's the hardware gate's job.
#
# Inputs (relative to repo root):
#   ./noble-pxe.tar.gz produced by build.sh
#
# What it does:
#   1. Extract vmlinuz, initrd, root.squashfs from the tarball
#   2. Stand up a stub HTTP server on the host serving:
#        /files/images/<image>/root.squashfs   <- the real built squashfs
#        /files/bin/slagworks-agent            <- a tiny shell-script stub
#   3. Boot QEMU with -kernel + -initrd + SLIRP networking,
#      passing slagworks.server=10.0.2.2:<port> on the kernel
#      cmdline (the host runner is at 10.0.2.2 from the guest's
#      SLIRP perspective).
#   4. Stream serial output, watch for the post-fetch marker.
#   5. Assert the stub server received the expected GETs.
#   6. Fail loudly on panic, marker timeout, or zero-stub-traffic.

set -euo pipefail

usage() {
    cat <<USAGE >&2
Usage: $0

  No arguments. Smoke-tests ./noble-pxe.tar.gz in the current
  working directory (produced by build.sh).
USAGE
    exit 1
}

[[ $# -eq 0 ]] || usage

SUITE="noble"
NAME="${SUITE}-pxe"
TARBALL="./${NAME}.tar.gz"

[[ -f "$TARBALL" ]] || { echo "ERROR: $TARBALL not found in cwd" >&2; exit 1; }

#####################################################################
# Tunables                                                          #
#####################################################################

# How long to wait for the boot marker. TCG-mode QEMU on a shared
# CI runner is slow — the kernel boot to BOOT hook banner alone
# can take 30s, plus DHCP timeouts and HTTP fetches add another
# 30-60s. 240s (4min) is roomy for a smoke test that ought to
# actually take ~60s on a snappy runner.
BOOT_TIMEOUT="${BOOT_TIMEOUT:-240}"

# Marker we look for in the serial output. The BOOT hook prints
# "slagworks: agent ready" AFTER both squashfs and agent fetches
# complete. Reaching this proves DHCP + DNS + HTTP all worked.
MARKER="${MARKER:-slagworks: agent ready}"

# Host port for the stub HTTP server. SLIRP forwards this from the
# guest's view of the host (10.0.2.2:<port>) to the actual host
# (127.0.0.1:<port>). We use a high port to avoid privilege issues.
HTTP_PORT="${HTTP_PORT:-31415}"

#####################################################################
# Workdir                                                           #
#####################################################################

WORKDIR=$(mktemp -d -t "slagworks-smoke-${NAME}.XXXXXXXX")
QEMU_PID=""
HTTP_PID=""

cleanup() {
    if [[ -n "$QEMU_PID" ]]; then
        kill "$QEMU_PID" 2>/dev/null || true
        wait "$QEMU_PID" 2>/dev/null || true
    fi
    if [[ -n "$HTTP_PID" ]]; then
        kill "$HTTP_PID" 2>/dev/null || true
        wait "$HTTP_PID" 2>/dev/null || true
    fi
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

#####################################################################
# Extract artifacts                                                 #
#####################################################################

echo "==> Extracting $TARBALL"
tar -xzf "$TARBALL" -C "$WORKDIR"
ARTIFACTS="$WORKDIR/$NAME"

for f in vmlinuz initrd root.squashfs; do
    [[ -f "$ARTIFACTS/$f" ]] || { echo "ERROR: missing $f in tarball" >&2; exit 1; }
done

#####################################################################
# Stage stub HTTP server file tree                                  #
#####################################################################

# The BOOT hook expects two URLs:
#   http://$SERVER/files/images/$IMAGE/root.squashfs
#   http://$SERVER/files/bin/slagworks-agent
#
# $SERVER and $IMAGE come from kernel cmdline (set below).
# We use IMAGE_NAME=$NAME so the path is /files/images/$NAME/...

STUB_ROOT="$WORKDIR/stub-http"
mkdir -p "$STUB_ROOT/files/images/$NAME" "$STUB_ROOT/files/bin"

# A minimal mountable squashfs is enough for the smoke. The full
# deploy squashfs is ~1GB — serving that over SLIRP/TCG-emulated
# networking takes long enough that wget's per-attempt timeout
# trips before the transfer completes. We just need a real
# squashfs that:
#   - mounts cleanly (real magic + dir structure)
#   - contains /bin/sh so the agent can chroot in and exec
#
# It does NOT need to contain overlay.ko: the slagworks initramfs hook
# bakes the overlay module into the PXE initrd (manual_add_modules
# overlay), so the BOOT hook modprobes it from the initrd before the
# squashfs is even mounted.
#
# Build it via docker from a small ubuntu:noble container.
echo "==> Building stub squashfs"
docker run --rm --platform=linux/amd64 \
    -v "$STUB_ROOT/files/images/$NAME:/out" \
    ubuntu:noble bash -c '
        set -e
        apt-get update -qq
        apt-get install -y -qq busybox-static squashfs-tools >/dev/null

        ROOTFS=/tmp/stub-rootfs
        mkdir -p "$ROOTFS/bin"

        cp /usr/bin/busybox "$ROOTFS/bin/busybox"
        ln -sf busybox "$ROOTFS/bin/sh"
        ln -sf busybox "$ROOTFS/bin/echo"

        mksquashfs "$ROOTFS" /out/root.squashfs -comp xz -noappend -no-progress
        chmod a+r /out/root.squashfs
    '

# Stub agent: a shell script that prints a marker and exits 0.
# The BOOT hook chmods +x and execs from inside the chroot, so
# /bin/sh in the squashfs has to resolve — which it does because
# the squashfs is the real built rootfs.
cat > "$STUB_ROOT/files/bin/slagworks-agent" <<'AGENT'
#!/bin/sh
echo "slagworks: smoke-stub agent reached"
exit 0
AGENT
chmod +x "$STUB_ROOT/files/bin/slagworks-agent"

#####################################################################
# Start stub HTTP server                                            #
#####################################################################

# python3 -m http.server logs requests to stdout, which we capture.
# That's how we assert "stub received expected GETs."
HTTP_LOG="$WORKDIR/http.log"
echo "==> Starting stub HTTP server on :$HTTP_PORT"
(cd "$STUB_ROOT" && python3 -m http.server "$HTTP_PORT" --bind 127.0.0.1) \
    >"$HTTP_LOG" 2>&1 &
HTTP_PID=$!

# Give the server a moment to bind. python3 -m http.server is fast.
for _ in 1 2 3 4 5; do
    if curl -sf "http://127.0.0.1:$HTTP_PORT/files/bin/slagworks-agent" \
            -o /dev/null 2>/dev/null; then
        break
    fi
    sleep 0.5
done

# Sanity-check the stub is up.
curl -sf "http://127.0.0.1:$HTTP_PORT/files/bin/slagworks-agent" \
     -o /dev/null \
    || { echo "ERROR: stub HTTP server didn't come up" >&2; exit 1; }

#####################################################################
# Boot the image under QEMU                                         #
#####################################################################

# SLIRP user-mode networking. From the guest's view:
#   10.0.2.15  — guest IP (via QEMU's built-in DHCP)
#   10.0.2.2   — host (where the stub HTTP server lives)
#   10.0.2.3   — DNS (QEMU forwards to host's resolver)
#
# We pass slagworks.server=10.0.2.2:$HTTP_PORT explicitly on the
# kernel cmdline. That bypasses needing to set the DHCP next-server
# option — production gets it from DHCP siaddr, but the smoke is
# validating the network/HTTP path, not the DHCP-option-plumbing.
GUEST_SERVER="10.0.2.2:$HTTP_PORT"

# slagworks.image is the directory under /files/images/. The BOOT hook
# constructs the squashfs URL as:
#   http://$SERVER/files/images/$IMAGE/root.squashfs
# so we set image=$NAME so the path matches our staged file.
# slagworks.mode is fixed at "deploy" here. The image is mode-agnostic
# (the BOOT hook's deploy/commission divergence is downstream of the
# fetch marker this smoke asserts), so the mode value doesn't change what
# we validate. The previous per-flavor smoke booted the identical image
# twice with only this string differing.
#
# boot=slagworks makes the stock Ubuntu /init source our BOOT hook
# (/scripts/slagworks) and call its mountroot(); ip=dhcp makes stock
# /init run configure_networking (which leases via QEMU's SLIRP DHCP).
# Without both, stock /init would try to mount a real root and panic —
# this is the same cmdline contract httpboot/handler.go emits for real
# PXE boots (minus BOOTIF, since SLIRP has a single NIC).
KERNEL_CMDLINE="console=ttyS0 boot=slagworks ip=dhcp slagworks.server=$GUEST_SERVER slagworks.image=$NAME slagworks.mode=deploy"

# QEMU args. -nographic + -serial stdio gives us serial output on
# stdout. -no-reboot makes a guest 'reboot' exit QEMU rather than
# loop (so our smoke doesn't hang if the agent stub does something
# weird). 1G is plenty for a stub boot.
SERIAL_LOG="$WORKDIR/serial.log"
echo "==> Booting $NAME under QEMU (TCG mode, ${BOOT_TIMEOUT}s timeout)"

# -accel tcg: explicit; KVM may or may not be available on a CI
# runner, and we don't need its speed for a smoke.
qemu-system-x86_64 \
    -accel tcg \
    -m 1024 \
    -smp 2 \
    -display none \
    -monitor none \
    -no-reboot \
    -kernel "$ARTIFACTS/vmlinuz" \
    -initrd "$ARTIFACTS/initrd" \
    -append "$KERNEL_CMDLINE" \
    -netdev user,id=net0 \
    -device virtio-net-pci,netdev=net0 \
    -serial stdio \
    > "$SERIAL_LOG" 2>&1 &
QEMU_PID=$!

#####################################################################
# Wait for the marker                                               #
#####################################################################

echo "==> Waiting up to ${BOOT_TIMEOUT}s for marker: $MARKER"

# Poll the serial log for the marker. We check QEMU's process state
# too — if QEMU dies before the marker (kernel panic, exec fail),
# fail fast rather than waiting the full timeout.
deadline=$(( $(date +%s) + BOOT_TIMEOUT ))
found_marker=0
while [[ $(date +%s) -lt $deadline ]]; do
    if grep -q "$MARKER" "$SERIAL_LOG" 2>/dev/null; then
        found_marker=1
        break
    fi
    if ! kill -0 "$QEMU_PID" 2>/dev/null; then
        echo "ERROR: QEMU exited before marker; serial log:" >&2
        echo "------ serial ------" >&2
        tail -50 "$SERIAL_LOG" >&2 || true
        echo "------ stub http log ------" >&2
        cat "$HTTP_LOG" >&2 || true
        echo "------" >&2
        exit 1
    fi
    sleep 1
done

if [[ "$found_marker" -ne 1 ]]; then
    echo "ERROR: marker '$MARKER' not seen within ${BOOT_TIMEOUT}s" >&2
    echo "Last 50 lines of serial output:" >&2
    echo "------ serial ------" >&2
    tail -50 "$SERIAL_LOG" >&2 || true
    echo "------ stub http log ------" >&2
    cat "$HTTP_LOG" >&2 || true
    echo "------" >&2
    exit 1
fi

echo "==> Marker observed"

#####################################################################
# Assert stub HTTP server saw the expected requests                 #
#####################################################################

# Two requests we expect, both 2xx:
#   GET /files/images/<NAME>/root.squashfs
#   GET /files/bin/slagworks-agent
#
# The stub log format from python3 -m http.server is:
#   <ip> - - [timestamp] "GET /path HTTP/1.1" <status> -

assert_request() {
    local path="$1"
    if ! grep -E "\"GET ${path//\//\\/} HTTP/1\\.[01]\" 2[0-9][0-9]" "$HTTP_LOG" >/dev/null; then
        echo "ERROR: stub never received successful GET on $path" >&2
        echo "Stub log:" >&2
        echo "------" >&2
        cat "$HTTP_LOG" >&2 || true
        echo "------" >&2
        exit 1
    fi
}

echo "==> Asserting stub received expected GETs"
assert_request "/files/images/$NAME/root.squashfs"
assert_request "/files/bin/slagworks-agent"

echo
echo "==> Smoke PASSED for $NAME"
