# Storage Templates

Status: live — implemented (internal/storage, internal/configscript); see also docs/user/hardware/adding-new-hardware.md

## Problem

The deploy agent hardcodes a single partition layout: 512MB ESP + remaining ext4 root on the first disk. Production hardware needs different layouts per server model (NVMe root + SATA data, RAID arrays, specific partition sizing, etc.).

## Architecture

Two layers with a clean seam between them:

1. **Storage scripts** — shell scripts that inspect hardware inventory and produce a JSON layout. Matched to machines by metadata. Run on the target host during deploy.
2. **Layout executor** — Go code in the agent that takes a JSON layout and translates it to an ordered list of commands (sgdisk, mkfs, mdadm, mount). The executor is the core — it's deterministic, testable, and where correctness matters.

The script is the "what do I want?" layer. The executor is the "how do I get it?" layer.

## Layout JSON format

The JSON that scripts produce and the executor consumes. This is the contract between the two layers.

```json
{
  "layout": {
    "nvme0n1": {
      "type": "disk", "ptable": "gpt", "boot": true,
      "partitions": [
        { "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
        { "name": "nvme0n1p2", "size": "rest" }
      ]
    },
    "nvme1n1": {
      "type": "disk", "ptable": "gpt", "boot": true,
      "partitions": [
        { "name": "nvme1n1p1", "fs": "vfat", "size": "512M", "bootable": true },
        { "name": "nvme1n1p2", "size": "rest" }
      ]
    },
    "mdroot": {
      "type": "raid", "level": 1,
      "members": ["nvme0n1p2", "nvme1n1p2"],
      "fs": "xfs"
    }
  },
  "mounts": {
    "/":         { "device": "mdroot" },
    "/boot/efi": { "device": "nvme0n1p1" }
  }
}
```

This is the same format the old MAAS commissioning scripts produced. It has proven expressive enough for real production layouts including RAID1 boot mirrors, RAID0/10 data arrays, and mixed NVMe/SATA configurations.

### Layout objects

**Disks** — physical block devices. Always GPT. `boot: true` marks disks that get an ESP.

```json
{
  "type": "disk", "ptable": "gpt", "boot": true,
  "partitions": [
    { "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
    { "name": "nvme0n1p2", "size": "rest" }
  ]
}
```

Partition `name` is the device node the kernel gives the partition at that position on that disk: the disk name plus the partition number, with a `p` between them when the disk name ends in a digit (`sda2`, `nvme0n1p2`, `mmcblk0p1`). Mounts and RAID members find a partition by this name, so the agent refuses, before touching any disk, a layout where a name is anything else (a label such as `root`, `nvme0n11`, `sdap1`, or the names of two partitions swapped). Partition `size` is `"512M"`, `"100G"`, or `"rest"` (fill remaining). `fs` is optional — omit it for raw partitions (e.g., RAID members). `bootable` marks the ESP partition.

A `vfat` partition is formatted FAT32, which needs at least 65525 clusters, so its minimum size depends on the disk's logical sector size: `33M` on a disk with 512-byte sectors, `257M` on a 4Kn disk (4096-byte sectors). Below that, `mkfs.fat` would format the partition anyway and UEFI firmware could not read it, so the agent refuses the layout before touching any disk, naming the minimum ("make the partition at least 257M"). The inventory a script reads does not carry the sector size, so size an ESP that may land on a 4Kn disk for 4Kn; the `512M` ESPs on this page fit both.

**RAID arrays** — software RAID via mdadm. Members reference partition names or whole-disk names from the layout.

```json
{
  "type": "raid", "level": 1,
  "members": ["nvme0n1p2", "nvme1n1p2"],
  "fs": "xfs"
}
```

Supported levels: 0, 1, 5, 6, 10. Each level needs its md personality module
(`raid0`, `raid1`, `raid456`, `raid10`) in the boot image's deploy initrd, which
`assets/initramfs/hooks/slagworks` bakes in. A boot image built without a
level's module fails the deploy at `mdadm --create`, after the member disks
were repartitioned, with an error that names the module; rebuild the image and
redeploy with `--destructive`.

**Mounts** — maps mount points to device names from the layout.

```json
{
  "/":         { "device": "mdroot" },
  "/boot/efi": { "device": "nvme0n1p1" },
  "/data":     { "device": "mddata" }
}
```

## Storage scripts

Scripts that produce layout JSON on stdout. They must be executable and carry
`slagworks:match` headers. **Use Python** — every shipped script under
`assets/storage/` is Python 3, the deploy agent invokes them with `python3`, and
matching the convention keeps the fleet consistent. (The matcher only parses the
`slagworks:` comment headers, so the engine itself is language-agnostic, but the
runtime and all examples assume Python — don't reintroduce bash.) Each script's
match metadata determines which machines it applies to.

### Script format

```python
#!/usr/bin/env python3
# slagworks:match system_product=RS501A-E12-RS4U
# slagworks:match system_manufacturer=/ASUSTeK.*/
# slagworks:priority 100

import json
import os
import sys

# Commission inventory is provided as a JSON file (path in $SLAGWORKS_INVENTORY).
# Contains system info + disk inventory (name, model, serial, size_bytes, transport).
inv_path = os.environ.get("SLAGWORKS_INVENTORY")
if not inv_path:
    print("ERROR: SLAGWORKS_INVENTORY must be set", file=sys.stderr)
    sys.exit(1)

with open(inv_path) as f:
    inv = json.load(f)

# Find boot drives by model.
boot = sorted(d["name"] for d in inv["disks"] if "INTEL SSDPF2KX" in d.get("model", ""))
if len(boot) != 2:
    print(f"ERROR: expected 2 Intel boot drives, found {len(boot)}", file=sys.stderr)
    sys.exit(1)

# Compute partition size from the actual disk.
disk_bytes = next(d["size_bytes"] for d in inv["disks"] if d["name"] == boot[0])
root_mib = (disk_bytes - 600 * 1024 * 1024) // (1024 * 1024)

layout = {
    "layout": {
        boot[0]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[0]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[0]}p2", "size": f"{root_mib}M"},
            ],
        },
        boot[1]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[1]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[1]}p2", "size": f"{root_mib}M"},
            ],
        },
        "mdroot": {
            "type": "raid", "level": 1,
            "members": [f"{boot[0]}p2", f"{boot[1]}p2"],
            "fs": "xfs",
        },
    },
    "mounts": {
        "/": {"device": "mdroot"},
        "/boot/efi": {"device": f"{boot[0]}p1"},
    },
}

json.dump(layout, sys.stdout, indent=2)
```

### Match metadata

Scripts declare what hardware they target via comment headers:

```python
# slagworks:match system_product=RS501A-E12-RS4U
# slagworks:match system_manufacturer=/ASUSTeK.*/
# slagworks:priority 100
```

Values are exact string matches by default. Wrap in `/slashes/` for regex (anchored to the full string).

Match fields (all optional, AND'd when multiple specified):

| Field | Matches against | Example |
|-------|----------------|---------|
| `system_product` | `commission_data.system.product_name` | `RS501A-E12-RS4U` |
| `system_manufacturer` | `commission_data.system.manufacturer` | `/ASUSTeK.*/` |

A script with no `slagworks:match` lines never matches. Priority is an integer; higher wins. Priorities must be unique — the worker rejects duplicate priorities at load time. First matching script is used. If no script matches, the agent falls back to the built-in default layout (ESP + root on first disk).

### Inventory file

Scripts receive `$SLAGWORKS_INVENTORY` pointing to a JSON file with the machine's commission data. Scripts read this instead of running `lsblk` directly. This makes scripts testable: feed them a fake inventory file and assert on the JSON output.

The inventory contains the same data the commission agent already collects:

```json
{
  "system": {
    "manufacturer": "ASUSTeK Computer INC.",
    "product_name": "RS501A-E12-RS4U",
    "board_product": "Z13PP-D32"
  },
  "memory": { "total_mb": 524288 },
  "cpus": [{ "model": "AMD EPYC 9454", "cores": 48 }],
  "disks": [
    { "name": "nvme0n1", "model": "INTEL SSDPF2KX019TZ", "size_bytes": 1920383410176, "transport": "nvme" },
    { "name": "nvme1n1", "model": "INTEL SSDPF2KX019TZ", "size_bytes": 1920383410176, "transport": "nvme" },
    { "name": "nvme2n1", "model": "KIOXIA KCD6XLUL15T3", "size_bytes": 15360747397120, "transport": "nvme" },
    { "name": "nvme3n1", "model": "KIOXIA KCD6XLUL15T3", "size_bytes": 15360747397120, "transport": "nvme" }
  ]
}
```

## Data flow

```
1. Operator writes storage scripts, places them in the site worker config directory.
   Each script has match metadata headers and produces layout JSON on stdout.

2. Machine is commissioned. commission_data stored on Machine record, synced to worker cache.

3. Operator triggers deploy. Machine PXE boots, overlay init runs, agent starts.

4. Agent calls GET http://{worker}/boot/storage-script?mac={mac}
   Worker parses match metadata from all scripts in config dir.
   Worker evaluates matches against machine's commission data.
   First match wins. Worker returns the script content.
   No match: worker returns empty response (204).

5. If a script was returned:
   Agent writes commission inventory to a temp file as $SLAGWORKS_INVENTORY.
   Agent executes the script, captures stdout as layout JSON.
   If no script was returned:
   Agent uses the built-in default layout (ESP + root on first disk).

6. Agent passes layout JSON to the executor:
   a. Parse layout into Go structs
   b. Plan: generate ordered command list (sgdisk, mkfs, mdadm, mount)
   c. Execute: run the commands
   d. Mount, extract root fs, install bootloader, write fstab
   e. Report completion
```

## Layout executor

The executor is a pure function: layout JSON + device inventory in, ordered command list out. This is where correctness matters and where testing is focused.

### Plan function

```go
type Command struct {
    Name string   // "sgdisk", "mkfs.xfs", "mdadm", "mount"
    Args []string
}

// Plan translates a layout JSON into an ordered list of commands.
// It validates the layout and resolves device names but has no side effects.
func Plan(layout LayoutJSON, inventory []BlockDevice) ([]Command, error)
```

The plan is deterministic: same inputs always produce the same command list. Command ordering follows dependency order: wipe disks, create partitions, re-read partition table (`blockdev --rereadpt`), format each new partition that declares `fs` and clear every other one, create RAID arrays, format each array that declares `fs` and clear every other one with `wipefs -a`, mount.

The clearing matters on a redeploy. A new partition or array starts with whatever bytes were at its offset, and `sgdisk --zap-all` rewrites only the partition tables. Without clearing, a partition the layout leaves unformatted (a spare ESP slot, a BIOS boot partition, a RAID member) keeps the filesystem an earlier layout put there; an old ESP on the firmware's first disk then boots the old bootloader, which drops to an initramfs shell looking for a root filesystem that no longer exists. An unformatted partition is cleared in two steps: `wipefs -a` erases every signature Linux probes for, and `dd` zeroes its first 4 KiB, because UEFI firmware mounts a FAT filesystem from its boot sector even after `wipefs` has erased the magic bytes. Preserve arrays and retained disks are never cleared.

Implementation notes:

- The layout JSON uses map keys for device names. `Plan()` must sort or toposort explicitly — Go map iteration is nondeterministic.
- Only one partition per disk may use `"rest"`. `Plan()` rejects layouts with multiple `"rest"` partitions on the same disk.
- `Plan()` creates and formats partition N of a disk at the kernel's node for that position, but mounts, RAID members, fstab and the bootloader reach a partition through its declared name, so it rejects a partition whose name is not that node.
- `Plan()` rejects a `vfat` partition smaller than the FAT32 minimum for its disk's logical sector size (`BlockDevice.LogicalSectorBytes`, from `lsblk`'s `LOG-SEC` column), since `mkfs.fat -F 32` formats an undersized partition with only a warning.

### Execution

The execute step is a trivial loop over the command list. It runs each command and fails hard on any error. No partial recovery — every deploy starts fresh.

## Testing strategy

Three layers, each catching different classes of bugs:

### Unit tests (Plan function)

Feed `Plan()` a layout JSON and a fake device inventory. Assert on the exact command list. No root, no block devices, runs in milliseconds.

```go
func TestPlan_RAID1Root(t *testing.T) {
    layout := parseLayout(`{
        "layout": {
            "nvme0n1": {"type":"disk","ptable":"gpt","boot":true,"partitions":[
                {"name":"nvme0n1p1","fs":"vfat","size":"512M","bootable":true},
                {"name":"nvme0n1p2","size":"rest"}
            ]},
            "nvme1n1": {"type":"disk","ptable":"gpt","boot":true,"partitions":[...]},
            "mdroot": {"type":"raid","level":1,"members":["nvme0n1p2","nvme1n1p2"],"fs":"xfs"}
        },
        "mounts": {"/":{"device":"mdroot"},"/boot/efi":{"device":"nvme0n1p1"}}
    }`)
    inventory := []BlockDevice{
        {Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
        {Name: "nvme1n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
    }

    cmds, err := Plan(layout, inventory)
    require.NoError(t, err)
    assert.Equal(t, []Command{
        {"sgdisk", []string{"--zap-all", "/dev/nvme0n1"}},
        {"sgdisk", []string{"-n", "1:0:+512M", "-t", "1:EF00", "-c", "1:nvme0n1p1", "/dev/nvme0n1"}},
        {"sgdisk", []string{"-n", "2:0:0", "-t", "2:8300", "-c", "2:nvme0n1p2", "/dev/nvme0n1"}},
        {"blockdev", []string{"--rereadpt", "/dev/nvme0n1"}},
        {"mkfs.fat", []string{"-F", "32", "/dev/nvme0n1p1"}},
        {"wipefs", []string{"-a", "/dev/nvme0n1p2"}},
        {"dd", []string{"if=/dev/zero", "of=/dev/nvme0n1p2", "bs=4096", "count=1", "conv=fsync"}},
        // ... the same seven for nvme1n1 ...
        {"mdadm", []string{"--create", "/dev/md0", "--level=1", "--raid-devices=2",
            "--metadata=1.2", "--run", "/dev/nvme0n1p2", "/dev/nvme1n1p2"}},
        {"mkfs.xfs", []string{"-f", "/dev/md0"}},
        {"mkdir", []string{"-p", "/target"}},
        {"mount", []string{"/dev/md0", "/target"}},
        {"mkdir", []string{"-p", "/target/boot/efi"}},
        {"mount", []string{"/dev/nvme0n1p1", "/target/boot/efi"}},
    }, cmds)
}
```

### Script tests

Feed a storage script a fake inventory JSON file. Capture stdout. Assert on the layout JSON. No root, no block devices.

```go
func TestScript_ASUS_RS501A(t *testing.T) {
    inventory := writeInventoryFile(t, CommissionData{
        System: System{ProductName: "RS501A-E12-RS4U"},
        Disks: []Disk{
            {Name: "nvme0n1", Model: "INTEL SSDPF2KX019TZ", SizeBytes: 1920383410176},
            {Name: "nvme1n1", Model: "INTEL SSDPF2KX019TZ", SizeBytes: 1920383410176},
        },
    })

    out, err := runScript(t, "scripts/41-storage-asus-rs501a", inventory)
    require.NoError(t, err)

    layout := parseLayout(out)
    assert.Contains(t, layout.Layout, "mdroot")
    assert.Equal(t, 1, layout.Layout["mdroot"].Level)
}
```

### Component tests (loop devices)

Run the actual commands against loop-backed block devices. Verify the resulting layout with sgdisk/blkid/mdadm. Needs root but no QEMU, runs in seconds.

The Plan function produces commands with real device paths (e.g., `/dev/nvme0n1`). For component tests, remap these to loop devices:

```go
func TestExecute_RAID1Root(t *testing.T) {
    // Create sparse files as fake drives
    lo0 := createLoopDevice(t, "2G")  // /dev/loop0
    lo1 := createLoopDevice(t, "2G")  // /dev/loop1

    // Plan with real layout, then remap device paths to loop devices
    cmds, _ := Plan(raid1Layout, fakeInventory("nvme0n1", "nvme1n1"))
    cmds = remapDevices(cmds, map[string]string{
        "/dev/nvme0n1": lo0,
        "/dev/nvme1n1": lo1,
    })
    execute(t, cmds)

    // Verify actual block device state
    assertGPT(t, lo0)
    assertPartitionCount(t, lo0, 2)
    assertFilesystem(t, lo0+"p1", "vfat")
    assertRAID(t, "/dev/md0", 1, []string{lo0+"p2", lo1+"p2"})
    assertFilesystem(t, "/dev/md0", "xfs")
    assertMountable(t, "/dev/md0")
}
```

### Integration tests (QEMU)

Full deploy on a QEMU VM with the storage script and executor. The VM boots from the resulting layout. Phone-home proves it's bootable. The existing harness supports multi-disk VMs via `ExtraDisk` (NVMe, SATA, virtio).

## Implementation plan

### Phase 1: Layout executor

Build and test the core — JSON layout in, disk commands out.

1. `internal/storage` package: layout JSON types, `Plan()` function, command types.
2. Unit tests for `Plan()`: single disk, multi-disk, RAID1, RAID0, RAID10, error cases.
3. Component tests with loop devices: verify commands produce correct layouts.
4. Wire into the deploy agent: replace `findTargetDisk` + `partitionDisk` + `formatPartitions` with `Plan()` + `Execute()`.
5. Built-in default layout hardcoded in Go (ESP + root on first disk) — used when no script matches.

### Phase 2: Script matching + delivery

The matching layer that selects which script runs on which machine.

1. Script metadata parser: read `# slagworks:match` headers from script files.
2. `Match(commissionData, scripts) -> *Script` — evaluate match metadata against commission data.
3. Worker HTTP endpoint: `GET /boot/storage-script?mac={mac}` — returns matching script.
4. Agent: fetch script, write inventory file, execute script, feed JSON to executor.
5. Port the ASUS RS501A MAAS script to the new format.

### Phase 3: API + persistence

1. Move scripts from config files to the global store (database).
2. CRUD API endpoints for storage scripts.
3. Site worker syncs scripts from global API.
4. `slagctl storage-script` CLI commands.

### Future

- LVM volume groups and logical volumes in the layout JSON.
- `select`-style declarative templates as an alternative to scripts (for simple layouts that don't need shell).
- Wipe modes (secure erase, zero-fill).
- Dry-run preview: "what layout would this machine get?"

## Examples

### ASUS RS501A (RAID1 NVMe boot + RAID0 data)

```python
#!/usr/bin/env python3
# slagworks:match system_product=RS501A-E12-RS4U
# slagworks:priority 100

import json
import os
import sys

inv_path = os.environ.get("SLAGWORKS_INVENTORY")
if not inv_path:
    print("ERROR: SLAGWORKS_INVENTORY must be set", file=sys.stderr)
    sys.exit(1)
with open(inv_path) as f:
    inv = json.load(f)

# Boot: RAID1 on the two Intel NVMe.
boot = sorted(d["name"] for d in inv["disks"] if "INTEL SSDPF2KX" in d.get("model", ""))
if len(boot) != 2:
    print(f"expected 2 Intel boot drives, found {len(boot)}", file=sys.stderr)
    sys.exit(1)

# Data: RAID0 on the Kioxia NVMe.
data = sorted(d["name"] for d in inv["disks"] if "KCD6XLUL" in d.get("model", ""))
if len(data) != 2:
    print(f"expected 2 Kioxia data drives, found {len(data)}", file=sys.stderr)
    sys.exit(1)

disk_bytes = next(d["size_bytes"] for d in inv["disks"] if d["name"] == boot[0])
root_mib = (disk_bytes - 600 * 1024 * 1024) // (1024 * 1024)

layout = {
    "layout": {
        boot[0]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[0]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[0]}p2", "size": f"{root_mib}M"},
            ],
        },
        boot[1]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[1]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[1]}p2", "size": f"{root_mib}M"},
            ],
        },
        "mdroot": {
            "type": "raid", "level": 1,
            "members": [f"{boot[0]}p2", f"{boot[1]}p2"],
            "fs": "xfs",
        },
        "mddata": {
            "type": "raid", "level": 0,
            "members": [data[0], data[1]],
            "fs": "xfs",
        },
    },
    "mounts": {
        "/": {"device": "mdroot"},
        "/boot/efi": {"device": f"{boot[0]}p1"},
        "/data": {"device": "mddata"},
    },
}

json.dump(layout, sys.stdout, indent=2)
```

### Tyan GPU server (RAID1 boot by model, RAID10 data)

```python
#!/usr/bin/env python3
# slagworks:match system_product=/B8261T85E8HR.*/
# slagworks:priority 100

import json
import os
import sys

inv_path = os.environ.get("SLAGWORKS_INVENTORY")
if not inv_path:
    print("ERROR: SLAGWORKS_INVENTORY must be set", file=sys.stderr)
    sys.exit(1)
with open(inv_path) as f:
    inv = json.load(f)

boot = sorted(d["name"] for d in inv["disks"] if "INTEL SSDPF2KX" in d.get("model", ""))
if len(boot) != 2:
    print(f"expected 2 Intel boot drives, found {len(boot)}", file=sys.stderr)
    sys.exit(1)

data = sorted(d["name"] for d in inv["disks"] if "KCD6XLUL" in d.get("model", ""))
if len(data) != 4:
    print(f"expected 4 Kioxia data drives, found {len(data)}", file=sys.stderr)
    sys.exit(1)

disk_bytes = next(d["size_bytes"] for d in inv["disks"] if d["name"] == boot[0])
root_mib = (disk_bytes - 600 * 1024 * 1024) // (1024 * 1024)

layout = {
    "layout": {
        boot[0]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[0]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[0]}p2", "size": f"{root_mib}M"},
            ],
        },
        boot[1]: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot[1]}p1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot[1]}p2", "size": f"{root_mib}M"},
            ],
        },
        "mdroot": {
            "type": "raid", "level": 1,
            "members": [f"{boot[0]}p2", f"{boot[1]}p2"],
            "fs": "xfs",
        },
        "mddata": {
            "type": "raid", "level": 10,
            "members": [data[0], data[1], data[2], data[3]],
            "fs": "xfs",
        },
    },
    "mounts": {
        "/": {"device": "mdroot"},
        "/boot/efi": {"device": f"{boot[0]}p1"},
        "/data": {"device": "mddata"},
    },
}

json.dump(layout, sys.stdout, indent=2)
```
