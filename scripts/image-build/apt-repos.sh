# shellcheck shell=bash
# apt-repos.sh — third-party APT repos baked into the slagworks DEPLOY chroot.
#
# Sourced (not executed) by build.sh's native path AND by the docker build
# driver (which has the repo bind-mounted at /repo). Single source of truth so
# adding a repo is a one-row table edit, not a copy-paste across both build
# paths.
#
# Each repo is baked into the deployed host's apt config the secure way
# (signed-by= scoped to that repo's keyring, NOT trusted.gpg.d — a key in
# trusted.gpg.d would sign the Ubuntu archive too, CWE-345). The mechanics, the
# hard way (proven during the FRR bring-up): build-apt reads signed-by= paths
# where mmdebstrap RUNS (the host for the native path, inside the container for
# docker), NOT in the chroot. So for each repo:
#   1. fetch the key to a build-visible path (<keydir>/<keyring>.gpg),
#   2. the deb-line's signed-by= points at that build path so build-apt verifies,
#   3. a per-repo --customize-hook installs the key into the chroot at
#      /usr/share/keyrings/<keyring>.gpg (0644), writes
#      /etc/apt/sources.list.d/<list>.list signed-by that in-chroot path, and
#      deletes the build-path deb-line mmdebstrap baked into
#      /etc/apt/sources.list (that path won't exist on the deployed host, and
#      keeping the line would duplicate the .list entry).
#
# Why sources.list.d/<list>.list and not the monolithic sources.list: the
# deployed host's repos must sit at the paths the upstream install instructions
# and site Ansible's ansible roles use, so a converge sees the repo as already
# present and the host keeps upgrading these packages with plain apt. The
# sources.list layout broke that for Tailscale: the line was signed-by
# /usr/share/keyrings/tailscale.gpg, ansible's tailscale role (guarded by
# `creates: /usr/share/keyrings/tailscale-archive-keyring.gpg`) didn't see it
# and wrote its own tailscale.list signed-by the archive keyring, and apt
# rejects one source with two different Signed-By values.
#
# These repos apply to the DEPLOY chroot only; the PXE chroot gets none.

# THIRD_PARTY_APT_REPOS: one row per repo, "list|keyring|key_url|repo_url|component".
#   list      deployed-host source file: /etc/apt/sources.list.d/<list>.list
#   keyring   deployed-host keyring /usr/share/keyrings/<keyring>.gpg AND the
#             build key file basename
#   key_url   GPG key URL; "@SUITE@" is replaced with the Ubuntu suite (e.g. noble)
#   repo_url  deb repository base URL
#   component the deb-line component (the suite is supplied at use time: the
#             build's SUITE, which is the deployed OS codename)
#
# FRR (frrouting): the booted L3-fabric kworker runs frr; the network-config
#   script only drops config + enables the unit (can't apt-install). noble ships
#   8.4; we need the frr-10.5 line matching the switches' pin. Component-pinned,
#   not version-pinned (the repo drops old point releases). frr.list +
#   frrouting.gpg match site Ansible's frr role (apt_repository filename: frr).
# Tailscale: deploy-time tailnet enrollment bakes the binary in (the agent's
#   enroll step can't apt-install). Not in noble's archive; needs Tailscale's
#   repo. The baked binary goes stale between image builds; the persisted repo
#   lets the deployed host upgrade it with plain apt. tailscale.list +
#   tailscale-archive-keyring.gpg match Tailscale's install instructions and
#   site Ansible's tailscale role.
THIRD_PARTY_APT_REPOS=(
    "frr|frrouting|https://deb.frrouting.org/frr/keys.gpg|https://deb.frrouting.org/frr|frr-10.5"
    "tailscale|tailscale-archive-keyring|https://pkgs.tailscale.com/stable/ubuntu/@SUITE@.noarmor.gpg|https://pkgs.tailscale.com/stable/ubuntu|main"
)

# fetch_repo_keys <keydir> <suite>: download each repo's GPG key to
# <keydir>/<keyring>.gpg. <keydir> must be readable by the process that runs
# mmdebstrap (the host for the native path; the container for docker — where it
# is the bind-mounted /work).
fetch_repo_keys() {
    local keydir="$1" suite="$2" row keyring key_url
    for row in "${THIRD_PARTY_APT_REPOS[@]}"; do
        IFS='|' read -r _ keyring key_url _ _ <<<"$row"
        key_url="${key_url//@SUITE@/$suite}"
        curl -fsSL "$key_url" -o "$keydir/$keyring.gpg"
    done
}

# mmdebstrap arg ordering matters: OPTIONS (--customize-hook=...) must come
# BEFORE the SUITE/TARGET positionals; MIRRORS (deb-lines) must come AFTER them
# (a deb-line is a positional — placed before SUITE it would be misparsed as the
# suite, pushing the real suite into a mirror slot → "invalid mirror: noble").
# So the two are emitted by SEPARATE functions the caller splices in at the
# right positions, matching how the hand-written FRR code did it. Both use the
# mirror_lines idiom (one value per line; caller reads with `while IFS= read`).
# Customize-hook / deb-line values contain no newlines, so line-per-arg is safe.

# repo_customize_hooks <keydir> <suite>: emit one --customize-hook=... per repo
# that relocates the repo from the baked sources.list into
# /etc/apt/sources.list.d/<list>.list + /usr/share/keyrings/<keyring>.gpg (see
# the header). Goes BEFORE the SUITE/TARGET positionals. mmdebstrap runs a
# non-file hook as `sh -c <hook> exec <chroot>`, so "$1" is the chroot.
repo_customize_hooks() {
    local keydir="$1" suite="$2" row list keyring repo_url component src
    for row in "${THIRD_PARTY_APT_REPOS[@]}"; do
        IFS='|' read -r list keyring _ repo_url component <<<"$row"
        src="\"\$1/etc/apt/sources.list.d/$list.list\""
        printf '%s\n' "--customize-hook=mkdir -p \"\$1/usr/share/keyrings\" \"\$1/etc/apt/sources.list.d\" && install -m 0644 \"$keydir/$keyring.gpg\" \"\$1/usr/share/keyrings/$keyring.gpg\" && echo 'deb [signed-by=/usr/share/keyrings/$keyring.gpg] $repo_url $suite $component' > $src && chmod 0644 $src && sed -i \"\\|signed-by=$keydir/$keyring.gpg|d\" \"\$1/etc/apt/sources.list\""
    done
}

# repo_deb_lines <keydir> <suite>: emit one deb-line per repo, signed-by the
# build keydir key. These are MIRRORS — go AFTER the SUITE/TARGET positionals,
# alongside the base/updates/security mirror_lines.
repo_deb_lines() {
    local keydir="$1" suite="$2" row keyring repo_url component
    for row in "${THIRD_PARTY_APT_REPOS[@]}"; do
        IFS='|' read -r _ keyring _ repo_url component <<<"$row"
        printf '%s\n' "deb [signed-by=$keydir/$keyring.gpg] $repo_url $suite $component"
    done
}

# The Ubuntu archive itself. build.sh uses two mirrors of it (see "Apt mirror"
# there): the build mirror, which mmdebstrap downloads from and writes into the
# chroot's sources.list, and the baked mirror, which is what the image ships.

# ubuntu_mirror_lines <mirror> <suite>: emit the deb-lines for one Ubuntu
# archive mirror. All three pockets (base, updates, security) are needed:
# passing mmdebstrap only the bare mirror URL silently drops -updates and
# -security, yielding an image with no security-update channel.
ubuntu_mirror_lines() {
    local mirror="$1" suite="$2" pocket
    for pocket in "$suite" "$suite-updates" "$suite-security"; do
        printf '%s\n' "deb $mirror $pocket main restricted universe multiverse"
    done
}

# baked_sources_hook <sources_file>: emit the --customize-hook=... that
# replaces the chroot's /etc/apt/sources.list with <sources_file> (the baked
# mirror's ubuntu_mirror_lines). mmdebstrap wrote that file from the mirrors
# it downloaded from, which are the build mirror's, and a deployed machine
# must not be pointed at those. Replacing the whole file rather than editing
# URLs in place means no build-mirror line can survive, whatever form
# mmdebstrap wrote it in. The third-party lines it also drops are the ones
# repo_customize_hooks deletes anyway, so the hooks run in either order.
# <sources_file> must be readable where mmdebstrap runs, like <keydir>. Goes
# BEFORE the SUITE/TARGET positionals.
baked_sources_hook() {
    local sources_file="$1"
    printf '%s\n' "--customize-hook=install -m 0644 \"$sources_file\" \"\$1/etc/apt/sources.list\""
}
