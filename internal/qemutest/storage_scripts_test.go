//go:build integration

package qemutest_test

// Storage scripts shared across matrix cells.
//
// These are minimal Python scripts that read the inventory JSON
// emitted by the agent's commission collector and produce a
// storage.Layout document on stdout. The agent pipes the inventory
// into them at deploy time. See cmd/slagworks-agent/storage.go for
// the consumer side.

// defaultStorageScript is a minimal single-disk layout that boots under
// BOTH firmware modes: a 1M GPT bios_grub (EF02) partition for legacy
// GRUB, a 512M ESP for UEFI systemd-boot, and ext4 root on the rest. The
// agent installs only the bootloader matching the host's actual firmware
// (isUEFI), so the unused partition is harmless. This lets the same
// script back both the uefi-* and bios-* matrix cells. Reads inventory
// from $SLAGWORKS_INVENTORY.
const defaultStorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disk = inv["disks"][0]["name"]

layout = {
    "layout": {
        disk: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{disk}1", "size": "1M", "bios_grub": True},
                {"name": f"{disk}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{disk}3", "fs": "ext4", "size": "rest"},
            ],
        },
    },
    "mounts": {
        "/":          {"device": f"{disk}3"},
        "/boot/efi":  {"device": f"{disk}2"},
    },
}
json.dump(layout, sys.stdout)
`

// raid1StorageScript lays out two disks with a mdraid level=1 across
// both for root, and boots under BOTH firmware modes: each disk gets a
// 1M bios_grub (EF02) partition (so GRUB is installed on both members
// and the array stays bootable if either disk is lost) and disk0 carries
// the ESP for UEFI. The agent installs only the bootloader matching the
// host firmware.
const raid1StorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(d["name"] for d in inv["disks"])
if len(disks) != 2:
    print(f"expected 2 disks, got {len(disks)}: {disks}", file=sys.stderr)
    sys.exit(1)

d0, d1 = disks[0], disks[1]

layout = {
    "layout": {
        d0: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{d0}1", "size": "1M", "bios_grub": True},
                {"name": f"{d0}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{d0}3", "size": "rest"},
            ],
        },
        d1: {
            "type": "disk", "ptable": "gpt",
            "partitions": [
                {"name": f"{d1}1", "size": "1M", "bios_grub": True},
                {"name": f"{d1}2", "size": "512M"},
                {"name": f"{d1}3", "size": "rest"},
            ],
        },
        "mdroot": {
            "type": "raid", "level": 1,
            "members": [f"{d0}3", f"{d1}3"],
            "fs": "ext4",
        },
    },
    "mounts": {
        "/":          {"device": "mdroot"},
        "/boot/efi":  {"device": f"{d0}2"},
    },
}
json.dump(layout, sys.stdout)
`

// raid1XFSThreeDiskStorageScript is a BIOS-only layout: an xfs root on a
// three-way mdraid mirror, each member disk carrying its own 1M bios_grub
// (EF02) partition, so the agent runs grub-install three times over the
// same xfs /boot/grub.
const raid1XFSThreeDiskStorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(d["name"] for d in inv["disks"])
if len(disks) != 3:
    print(f"expected 3 disks, got {len(disks)}: {disks}", file=sys.stderr)
    sys.exit(1)

layout = {"layout": {}, "mounts": {"/": {"device": "mdroot"}}}
for d in disks:
    layout["layout"][d] = {
        "type": "disk", "ptable": "gpt",
        "partitions": [
            {"name": f"{d}1", "size": "1M", "bios_grub": True},
            {"name": f"{d}2", "size": "rest"},
        ],
    }
layout["layout"]["mdroot"] = {
    "type": "raid", "level": 1,
    "members": [f"{d}2" for d in disks],
    "fs": "xfs",
}
json.dump(layout, sys.stdout)
`
