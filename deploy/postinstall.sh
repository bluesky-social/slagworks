#!/bin/sh
set -e

# Create slagworks system user if it doesn't exist
if ! getent passwd slagworks >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin slagworks
fi

# Ensure the data/images directories exist and are owned by slagworks.
# The package ships /var/lib/slagworks but NOT the images/ and data/
# subdirs; create them here so a fresh install's postinst (the EFI symlink
# below) and first service start (the SQLite local DB in data/) don't fail
# on a missing/parent-owned directory.
#
# Symlink-safe (CWE-59): /var/lib/slagworks is owned by the unprivileged
# slagworks service account, so a compromised service could pre-replace a
# child with a symlink; a root-run `chown` that followed it would re-own the
# target (e.g. /etc). Refuse any pre-existing symlink, and chown with -h so
# we never dereference. `install -d` is idempotent on a real directory.
chown -h slagworks:slagworks /var/lib/slagworks
for dir in /var/lib/slagworks/images /var/lib/slagworks/data; do
    if [ -L "$dir" ]; then
        echo "slagworks postinst: refusing to operate on symlink $dir" >&2
        exit 1
    fi
    install -d -o slagworks -g slagworks -m 0750 "$dir"
done

# Restrict env file permissions (examples and any copied active files)
chown root:slagworks /etc/slagworks/*.env* 2>/dev/null || true
chmod 0640 /etc/slagworks/*.env* 2>/dev/null || true

# Symlink systemd-boot EFI binary into images dir for PXE deploy
EFI_SRC="/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
EFI_DST="/var/lib/slagworks/images/systemd-bootx64.efi"
if [ -f "$EFI_SRC" ] && [ ! -e "$EFI_DST" ]; then
    ln -s "$EFI_SRC" "$EFI_DST"
fi

# NOTE: the .deb-upgrade-time initrd-repack pass that used to live
# here is gone (image-pipeline phase 6, 2026-05). Initrds shipped in
# the canonical image tarballs already have the slagworks overlay
# /init baked in at image build time, so a deb upgrade has nothing
# to repack.

systemctl daemon-reload

case "$1" in
    configure)
        # Deliberately do NOT enable either service. A host runs the
        # site worker, the global server, or (rarely, pop3) both — the
        # package cannot know which. Blanket-enabling both put a
        # crash-looping global unit on every site-only oob-gw.
        # Operator enables explicitly per host after placing the env
        # file, e.g.:
        #   install -m 0640 -g slagworks site.env /etc/slagworks/site.env
        #   systemctl enable --now slagworksd-site
        if [ -n "$2" ]; then
            # Upgrade: restart any service that was already running.
            if systemctl is-active --quiet slagworksd-site 2>/dev/null; then
                systemctl restart slagworksd-site
            fi
            if systemctl is-active --quiet slagworksd-global 2>/dev/null; then
                systemctl restart slagworksd-global
            fi
        fi
        ;;
esac
