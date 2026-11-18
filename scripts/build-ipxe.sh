#!/usr/bin/env bash
# Reproducible build of the iPXE binaries shipped in assets/ipxe/.
#
# Usage: scripts/build-ipxe.sh <ipxe-checkout> <embed.ipxe> <outdir> [container-build-dir]
#
# Writes <outdir>/snponly.efi (built as snp--intel.efi, see
# .github/workflows/build-ipxe.yaml for why) and <outdir>/undionly.kpxe.
# Used by CI and runnable on a laptop (needs docker; on arm64 the
# linux/amd64 image runs under emulation and yields identical bytes).
#
# Determinism inputs, all pinned here:
#   - toolchain: base image by digest + apt packages from a fixed
#     snapshot.ubuntu.com timestamp. ubuntu-latest's host gcc/binutils
#     drift whenever the runner image is refreshed.
#   - SOURCE_DATE_EPOCH = the iPXE commit's committer time. Without it
#     iPXE links `date +%s` in as _build_timestamp (EFI .data), which
#     was the byte difference between otherwise-identical CI rebuilds.
#   - the build path: iPXE derives _build_id from a cksum of its -g
#     objects, whose DWARF comp_dir is absolute, so -ffile-prefix-map
#     strips it. The build also runs on a container-local copy, so the
#     host path never reaches the compiler. The optional 4th argument
#     moves that copy, letting CI build twice in different directories
#     and compare.
set -euo pipefail

IMAGE=ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac
APT_SNAPSHOT=20261001T000000Z

if [[ $# -lt 3 || $# -gt 4 ]]; then
	echo "usage: $0 <ipxe-checkout> <embed.ipxe> <outdir> [container-build-dir]" >&2
	exit 2
fi

src=$(cd "$1" && pwd)
embed="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
mkdir -p "$3"
out=$(cd "$3" && pwd)
build_dir=${4:-/build/ipxe}

if [[ ! -e "$src/.git" ]]; then
	echo "ERROR: $src is not a git checkout; SOURCE_DATE_EPOCH and iPXE's version string both come from its HEAD" >&2
	exit 1
fi
sde=$(git -C "$src" log -1 --format=%ct HEAD)

docker run --rm --platform linux/amd64 \
	-v "$src:/src:ro" \
	-v "$embed:/embed.ipxe:ro" \
	-v "$out:/out" \
	-e SOURCE_DATE_EPOCH="$sde" \
	-e APT_SNAPSHOT="$APT_SNAPSHOT" \
	-e BUILD_DIR="$build_dir" \
	-e HOST_UID="$(id -u)" \
	-e HOST_GID="$(id -g)" \
	"$IMAGE" bash -euo pipefail -c '
		export DEBIAN_FRONTEND=noninteractive
		# snapshot.ubuntu.com is https-only and the base image ships no
		# CA bundle. ca-certificates comes from the live archive; it
		# never reaches the build output.
		apt-get -qq -o APT::Update::Error-Mode=any update
		apt-get -qq install -y --no-install-recommends ca-certificates >/dev/null
		apt-get -qq -o APT::Update::Error-Mode=any --snapshot "$APT_SNAPSHOT" update
		apt-get -qq --snapshot "$APT_SNAPSHOT" install -y --no-install-recommends \
			gcc binutils make libc6-dev perl liblzma-dev mtools >/dev/null
		mkdir -p "$(dirname "$BUILD_DIR")"
		cp -a /src "$BUILD_DIR"
		cp /embed.ipxe "$BUILD_DIR/src/embed.ipxe"
		cd "$BUILD_DIR/src"
		# iPXE compiles with -g and derives _build_id from a cksum of the
		# objects, so DW_AT_comp_dir would leak the build path into the
		# binary.
		prefix_map="-ffile-prefix-map=$PWD=."
		make -j"$(nproc)" bin-x86_64-efi/snp--intel.efi EMBED=embed.ipxe EXTRA_CFLAGS="$prefix_map"
		make -j"$(nproc)" bin/undionly.kpxe EMBED=embed.ipxe EXTRA_CFLAGS="$prefix_map"
		install -m 0644 -o "$HOST_UID" -g "$HOST_GID" bin-x86_64-efi/snp--intel.efi /out/snponly.efi
		install -m 0644 -o "$HOST_UID" -g "$HOST_GID" bin/undionly.kpxe /out/undionly.kpxe
	'

cd "$out"
if command -v sha256sum >/dev/null; then
	sha256sum snponly.efi undionly.kpxe
else
	shasum -a 256 snponly.efi undionly.kpxe
fi
