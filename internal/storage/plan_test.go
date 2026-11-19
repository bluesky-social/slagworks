package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParseLayout(t *testing.T, s string) Layout {
	t.Helper()
	var l Layout
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	return l
}

func TestPlan_SingleDisk_ESPAndRoot(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "vda2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "vda2" },
			"/boot/efi": { "device": "vda1" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "vda", SizeBytes: 10737418240, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	expected := []Command{
		{"sgdisk", []string{"--zap-all", "/dev/vda"}},
		{"sgdisk", []string{"-n", "1:0:+512M", "-t", "1:EF00", "-c", "1:vda1", "/dev/vda"}},
		{"sgdisk", []string{"-n", "2:0:0", "-t", "2:8300", "-c", "2:vda2", "/dev/vda"}},
		{"blockdev", []string{"--rereadpt", "/dev/vda"}},
		{"mkfs.fat", []string{"-F", "32", "/dev/vda1"}},
		{"mkfs.ext4", []string{"-F", "/dev/vda2"}},
		{"mkdir", []string{"-p", "/target"}},
		{"mount", []string{"/dev/vda2", "/target"}},
		{"mkdir", []string{"-p", "/target/boot/efi"}},
		{"mount", []string{"/dev/vda1", "/target/boot/efi"}},
	}

	assertCommands(t, expected, cmds)
}

func TestPlan_NVMe_PartitionDevPaths(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "nvme0n1p2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p2" },
			"/boot/efi": { "device": "nvme0n1p1" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// NVMe devices use 'p' separator: nvme0n1p1, nvme0n1p2
	assertCommandContains(t, cmds, Command{"mkfs.fat", []string{"-F", "32", "/dev/nvme0n1p1"}})
	assertCommandContains(t, cmds, Command{"mkfs.ext4", []string{"-F", "/dev/nvme0n1p2"}})
}

func TestPlan_RAID1_Root(t *testing.T) {
	layout := mustParseLayout(t, `{
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
			"/": { "device": "mdroot" },
			"/boot/efi": { "device": "nvme0n1p1" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
		{Name: "nvme1n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// RAID partitions should not be formatted (no fs set).
	for _, c := range cmds {
		if c.Name == "mkfs.ext4" && (c.Args[len(c.Args)-1] == "/dev/nvme0n1p2" || c.Args[len(c.Args)-1] == "/dev/nvme1n1p2") {
			t.Errorf("RAID member partition should not be formatted directly: %s", c)
		}
	}

	// mdadm create with RAID1.
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md0",
		"--level=1", "--raid-devices=2", "--metadata=1.2", "--run",
		"/dev/nvme0n1p2", "/dev/nvme1n1p2",
	}})

	// RAID device formatted with xfs.
	assertCommandContains(t, cmds, Command{"mkfs.xfs", []string{"-f", "/dev/md0"}})

	// Root mounted from RAID.
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/md0", "/target"}})
}

func TestPlan_RAID1_Boot_RAID0_Data(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "nvme0n1p2", "size": "1830000M" }
				]
			},
			"nvme1n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "nvme1n1p1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "nvme1n1p2", "size": "1830000M" }
				]
			},
			"nvme2n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"nvme3n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"mdroot": {
				"type": "raid", "level": 1,
				"members": ["nvme0n1p2", "nvme1n1p2"],
				"fs": "xfs"
			},
			"mddata": {
				"type": "raid", "level": 0,
				"members": ["nvme2n1", "nvme3n1"],
				"fs": "xfs"
			}
		},
		"mounts": {
			"/": { "device": "mdroot" },
			"/boot/efi": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
		{Name: "nvme1n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
		{Name: "nvme2n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
		{Name: "nvme3n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Two RAID arrays: md0 = mddata (RAID0), md1 = mdroot (RAID1).
	// md indices are assigned in sorted order of layout names.
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md0",
		"--level=0", "--raid-devices=2", "--metadata=1.2", "--run",
		"/dev/nvme2n1", "/dev/nvme3n1",
	}})
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md1",
		"--level=1", "--raid-devices=2", "--metadata=1.2", "--run",
		"/dev/nvme0n1p2", "/dev/nvme1n1p2",
	}})

	// Data mounted.
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/md0", "/target/data"}})
}

func TestPlan_RAID10(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"sda": { "type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "sda1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "sda2", "fs": "ext4", "size": "rest" }
				]
			},
			"nvme0n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"nvme1n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"nvme2n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"nvme3n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"mddata": {
				"type": "raid", "level": 10,
				"members": ["nvme0n1", "nvme1n1", "nvme2n1", "nvme3n1"],
				"fs": "xfs"
			}
		},
		"mounts": {
			"/": { "device": "sda2" },
			"/boot/efi": { "device": "sda1" },
			"/data": { "device": "mddata" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "sda", SizeBytes: 500107862016, LogicalSectorBytes: 512},
		{Name: "nvme0n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
		{Name: "nvme1n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
		{Name: "nvme2n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
		{Name: "nvme3n1", SizeBytes: 15360747397120, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md0",
		"--level=10", "--raid-devices=4", "--metadata=1.2", "--run",
		"/dev/nvme0n1", "/dev/nvme1n1", "/dev/nvme2n1", "/dev/nvme3n1",
	}})
}

func TestPlan_SwapPartition(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "vda2", "fs": "swap", "size": "64G" },
					{ "name": "vda3", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "vda3" },
			"/boot/efi": { "device": "vda1" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "vda", SizeBytes: 1099511627776, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Swap partition type code is 8200.
	assertCommandContains(t, cmds, Command{"sgdisk", []string{
		"-n", "2:0:+64G", "-t", "2:8200", "-c", "2:vda2", "/dev/vda",
	}})

	// Swap formatted with mkswap.
	assertCommandContains(t, cmds, Command{"mkswap", []string{"/dev/vda2"}})
}

func TestPlan_MountOrdering(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "vda2", "fs": "ext4", "size": "100G" },
					{ "name": "vda3", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/boot/efi": { "device": "vda1" },
			"/data/logs": { "device": "vda3" },
			"/": { "device": "vda2" }
		}
	}`)

	inventory := []BlockDevice{
		{Name: "vda", SizeBytes: 1099511627776, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Extract mount commands.
	var mounts []string
	for _, c := range cmds {
		if c.Name == "mount" {
			mounts = append(mounts, c.Args[1])
		}
	}

	// / must come before /boot/efi and /data/logs.
	if len(mounts) != 3 || mounts[0] != "/target" {
		t.Fatalf("expected / first, got mounts: %v", mounts)
	}
}

func TestDefaultLayout_VirtioDisk(t *testing.T) {
	layout := DefaultLayout("vda")
	inventory := []BlockDevice{{Name: "vda", SizeBytes: 10737418240, LogicalSectorBytes: 512}}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan with default layout: %v", err)
	}

	assertCommandContains(t, cmds, Command{"sgdisk", []string{"--zap-all", "/dev/vda"}})
	assertCommandContains(t, cmds, Command{"mkfs.fat", []string{"-F", "32", "/dev/vda1"}})
	assertCommandContains(t, cmds, Command{"mkfs.ext4", []string{"-F", "/dev/vda2"}})
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/vda2", "/target"}})
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/vda1", "/target/boot/efi"}})
}

func TestDefaultLayout_NVMeDisk(t *testing.T) {
	layout := DefaultLayout("nvme0n1")
	inventory := []BlockDevice{{Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512}}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan with default layout: %v", err)
	}

	// NVMe uses p separator.
	assertCommandContains(t, cmds, Command{"mkfs.fat", []string{"-F", "32", "/dev/nvme0n1p1"}})
	assertCommandContains(t, cmds, Command{"mkfs.ext4", []string{"-F", "/dev/nvme0n1p2"}})
}

// --- Error cases ---

func TestPlan_RejectsEmptyLayout(t *testing.T) {
	layout := mustParseLayout(t, `{"layout": {}, "mounts": {"/": {"device": "x"}}}`)
	_, err := Plan(layout, nil)
	if err == nil {
		t.Fatal("expected error for empty layout")
	}
}

func TestPlan_RejectsMultipleRest(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "vda1", "size": "rest", "fs": "ext4" },
					{ "name": "vda2", "size": "rest", "fs": "ext4" }
				]
			}
		},
		"mounts": { "/": { "device": "vda1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for multiple rest partitions")
	}
}

func TestPlan_RejectsMissingDisk(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": { "/": { "device": "nvme0n1p1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "sda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for missing disk in inventory")
	}
}

func TestPlan_RejectsMissingRootMount(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [{ "name": "vda1", "fs": "ext4", "size": "rest" }]
			}
		},
		"mounts": { "/boot/efi": { "device": "vda1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for missing root mount")
	}
}

func TestPlan_RejectsBadRAIDMember(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [{ "name": "vda1", "fs": "ext4", "size": "rest" }]
			},
			"md0": {
				"type": "raid", "level": 1,
				"members": ["vda1", "nonexistent"]
			}
		},
		"mounts": { "/": { "device": "vda1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for nonexistent RAID member")
	}
}

func TestPlan_RejectsUnsupportedFS(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [{ "name": "vda1", "fs": "btrfs", "size": "rest" }]
			}
		},
		"mounts": { "/": { "device": "vda1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for unsupported filesystem")
	}
}

// TestResolveMountDevices_SATA verifies that ResolveMountDevices returns device
// paths consistent with the mount commands Plan produces for a SATA disk.
func TestResolveMountDevices_SATA(t *testing.T) {
	layout := DefaultLayout("sda")
	devs := ResolveMountDevices(layout)

	if devs["/"] != "/dev/sda2" {
		t.Errorf("expected /dev/sda2 for /, got %q", devs["/"])
	}
	if devs["/boot/efi"] != "/dev/sda1" {
		t.Errorf("expected /dev/sda1 for /boot/efi, got %q", devs["/boot/efi"])
	}

	// Verify these match what Plan actually mounts.
	cmds, err := Plan(layout, []BlockDevice{{Name: "sda", SizeBytes: 21474836480, LogicalSectorBytes: 512}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/sda2", "/target"}})
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/sda1", "/target/boot/efi"}})
}

// TestResolveMountDevices_NVMe verifies device paths for an NVMe disk (p-separator).
func TestResolveMountDevices_NVMe(t *testing.T) {
	layout := DefaultLayout("nvme0n1")
	devs := ResolveMountDevices(layout)

	if devs["/"] != "/dev/nvme0n1p2" {
		t.Errorf("expected /dev/nvme0n1p2 for /, got %q", devs["/"])
	}
	if devs["/boot/efi"] != "/dev/nvme0n1p1" {
		t.Errorf("expected /dev/nvme0n1p1 for /boot/efi, got %q", devs["/boot/efi"])
	}

	cmds, err := Plan(layout, []BlockDevice{{Name: "nvme0n1", SizeBytes: 21474836480, LogicalSectorBytes: 512}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/nvme0n1p2", "/target"}})
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/nvme0n1p1", "/target/boot/efi"}})
}

// TestResolveMountDevices_RAID verifies that RAID device names are resolved to
// /dev/mdN paths consistent with what Plan's mdadm commands create.
func TestResolveMountDevices_RAID(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "nvme0n1p1", "size": "rest" }
				]
			},
			"nvme1n1": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "nvme1n1p1", "size": "rest" }
				]
			},
			"mdroot": {
				"type": "raid", "level": 1,
				"members": ["nvme0n1p1", "nvme1n1p1"],
				"fs": "xfs"
			}
		},
		"mounts": { "/": { "device": "mdroot" } }
	}`)
	inventory := []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 21474836480},
		{Name: "nvme1n1", SizeBytes: 21474836480},
	}

	devs := ResolveMountDevices(layout)
	if devs["/"] != "/dev/md0" {
		t.Errorf("expected /dev/md0 for RAID root, got %q", devs["/"])
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/md0", "/target"}})
}

// --- Helpers ---

func assertCommands(t *testing.T, expected, got []Command) {
	t.Helper()
	if len(expected) != len(got) {
		t.Errorf("expected %d commands, got %d", len(expected), len(got))
		for i, c := range got {
			t.Logf("  [%d] %s", i, c)
		}
		t.FailNow()
	}
	for i := range expected {
		if expected[i].Name != got[i].Name || !sliceEqual(expected[i].Args, got[i].Args) {
			t.Errorf("command %d:\n  expected: %s\n  got:      %s", i, expected[i], got[i])
		}
	}
}

func assertCommandContains(t *testing.T, cmds []Command, want Command) {
	t.Helper()
	for _, c := range cmds {
		if c.Name == want.Name && sliceEqual(c.Args, want.Args) {
			return
		}
	}
	t.Errorf("command not found: %s", want)
	t.Log("all commands:")
	for i, c := range cmds {
		t.Logf("  [%d] %s", i, c)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolveMountDevices_NoESP(t *testing.T) {
	// Layout without /boot/efi — e.g., BIOS boot or custom layout
	layout := mustParseLayout(t, `{
		"layout": {
			"sda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "sda1", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "sda1" }
		}
	}`)
	inventory := []BlockDevice{{Name: "sda", SizeBytes: 21474836480}}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	devs := ResolveMountDevices(layout)

	// / should resolve
	if devs["/"] != "/dev/sda1" {
		t.Errorf("expected /dev/sda1 for /, got %q", devs["/"])
	}

	// /boot/efi should not be present
	if _, ok := devs["/boot/efi"]; ok {
		t.Errorf("expected no /boot/efi in mount devices, got %q", devs["/boot/efi"])
	}

	// Should not have any EFI-related commands
	for _, c := range cmds {
		if c.Name == "mkfs.fat" {
			t.Errorf("unexpected FAT format command in layout without ESP: %s", c)
		}
	}
}

func TestPlan_BIOSGrub_HybridLayout(t *testing.T) {
	// Hybrid layout (the matrix default): 1M bios_grub (EF02) + 512M
	// ESP (EF00) + ext4 root. Asserts the EF02 type code and that the
	// bios_grub partition gets NO mkfs (it's unformatted).
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "size": "1M", "bios_grub": true },
					{ "name": "vda2", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "vda3", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "vda3" },
			"/boot/efi": { "device": "vda2" }
		}
	}`)

	cmds, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240, LogicalSectorBytes: 512}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	expected := []Command{
		{"sgdisk", []string{"--zap-all", "/dev/vda"}},
		{"sgdisk", []string{"-n", "1:0:+1M", "-t", "1:EF02", "-c", "1:vda1", "/dev/vda"}},
		{"sgdisk", []string{"-n", "2:0:+512M", "-t", "2:EF00", "-c", "2:vda2", "/dev/vda"}},
		{"sgdisk", []string{"-n", "3:0:0", "-t", "3:8300", "-c", "3:vda3", "/dev/vda"}},
		{"blockdev", []string{"--rereadpt", "/dev/vda"}},
		{"mkfs.fat", []string{"-F", "32", "/dev/vda2"}},
		{"mkfs.ext4", []string{"-F", "/dev/vda3"}},
		{"mkdir", []string{"-p", "/target"}},
		{"mount", []string{"/dev/vda3", "/target"}},
		{"mkdir", []string{"-p", "/target/boot/efi"}},
		{"mount", []string{"/dev/vda2", "/target/boot/efi"}},
	}
	assertCommands(t, expected, cmds)
}

func TestPlan_RejectsBIOSGrubWithFS(t *testing.T) {
	// bios_grub is mutually exclusive with fs/bootable.
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "vda1", "size": "1M", "bios_grub": true, "fs": "ext4" },
					{ "name": "vda2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": { "/": { "device": "vda2" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for bios_grub partition with fs set")
	}
}

func TestBIOSGrubDisks(t *testing.T) {
	// Single hybrid disk -> one bios_grub disk.
	single := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "size": "1M", "bios_grub": true },
					{ "name": "vda2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": { "/": { "device": "vda2" } }
	}`)
	if got := BIOSGrubDisks(single); len(got) != 1 || got[0] != "/dev/vda" {
		t.Errorf("single: expected [/dev/vda], got %v", got)
	}

	// Two-disk RAID1 with bios_grub on both -> sorted [/dev/sda /dev/sdb].
	raid := mustParseLayout(t, `{
		"layout": {
			"sdb": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "sdb1", "size": "1M", "bios_grub": true },
					{ "name": "sdb2", "size": "rest" }
				]
			},
			"sda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "sda1", "size": "1M", "bios_grub": true },
					{ "name": "sda2", "size": "rest" }
				]
			},
			"mdroot": { "type": "raid", "level": 1, "members": ["sda2", "sdb2"], "fs": "ext4" }
		},
		"mounts": { "/": { "device": "mdroot" } }
	}`)
	got := BIOSGrubDisks(raid)
	if len(got) != 2 || got[0] != "/dev/sda" || got[1] != "/dev/sdb" {
		t.Errorf("raid: expected [/dev/sda /dev/sdb], got %v", got)
	}

	// UEFI-only layout (no bios_grub) -> empty.
	uefi := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "vda1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "vda2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": { "/": { "device": "vda2" }, "/boot/efi": { "device": "vda1" } }
	}`)
	if got := BIOSGrubDisks(uefi); len(got) != 0 {
		t.Errorf("uefi: expected no bios_grub disks, got %v", got)
	}
}

func TestPlan_RejectsMountingBIOSGrub(t *testing.T) {
	// A bios_grub partition is unformatted GRUB space; mounting it
	// (here as /) must be rejected, not silently planned as a mount.
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "vda1", "size": "1M", "bios_grub": true },
					{ "name": "vda2", "fs": "ext4", "size": "rest" }
				]
			}
		},
		"mounts": { "/": { "device": "vda1" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for mounting a bios_grub partition")
	}
}

func TestPlan_RejectsMountingPartitionWithoutFS(t *testing.T) {
	// Generalization of the bios_grub case: any disk partition that is
	// mounted must declare a filesystem (the planner only mkfs'es
	// FS-set partitions but mounts unconditionally). RAID members are
	// exempt because the md device is mounted, not the member.
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": {
				"type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "vda1", "fs": "ext4", "size": "10G" },
					{ "name": "vda2", "size": "rest" }
				]
			}
		},
		"mounts": {
			"/": { "device": "vda1" },
			"/data": { "device": "vda2" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 107374182400}})
	if err == nil {
		t.Fatal("expected error for mounting a partition with no filesystem")
	}
}

func TestPlan_RejectsMountingRAIDWithoutFS(t *testing.T) {
	// A RAID device is only mkfs'ed when fs is set, but planMounts
	// mounts it unconditionally — so mounting an fs-less array would
	// mount raw bytes. Reject at validation.
	layout := mustParseLayout(t, `{
		"layout": {
			"sda": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "sda1", "size": "rest" } ] },
			"sdb": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "sdb1", "size": "rest" } ] },
			"mdroot": { "type": "raid", "level": 1, "members": ["sda1", "sdb1"] }
		},
		"mounts": { "/": { "device": "mdroot" } }
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "sda", SizeBytes: 10737418240},
		{Name: "sdb", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for mounting a RAID device with no filesystem")
	}
}

func TestPlan_RAIDCreate_WithArrayUUID_And_Preserve(t *testing.T) {
	// Root is a raid device with array_uuid pinned (created array gets a
	// script-known UUID instead of a random one). Data is a preserve
	// device (assembled, not created, by the same convention). Asserts
	// md-dev numbering doesn't collide between the two classes, the
	// preserve device is assembled (not created) and never mkfs'd, and
	// /data mounts the assembled md.
	layout := mustParseLayout(t, `{
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
				"fs": "xfs",
				"array_uuid": "11111111-1111-1111-1111-111111111111"
			},
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme2n1", "nvme3n1"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "mdroot" },
			"/boot/efi": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)

	// Preserve members (nvme2n1, nvme3n1) are deliberately NOT declared as
	// disk devices in the layout or listed in inventory — that's what
	// keeps them out of the wipe/zap set. Only the boot disks need
	// inventory entries (Plan checks disk-type devices against inventory).
	inventory := []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
		{Name: "nvme1n1", SizeBytes: 1920383410176, LogicalSectorBytes: 512},
	}

	cmds, err := Plan(layout, inventory)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// md indices assigned across raid+preserve combined, sorted by name:
	// mddata (preserve) = md0, mdroot (raid) = md1.
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md1",
		"--level=1", "--raid-devices=2", "--metadata=1.2",
		"--uuid", "11111111-1111-1111-1111-111111111111",
		"--run", "/dev/nvme0n1p2", "/dev/nvme1n1p2",
	}})
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--assemble", "/dev/md0",
		"--uuid", "22222222-2222-2222-2222-222222222222",
		"/dev/nvme2n1", "/dev/nvme3n1",
	}})

	// Preserve device is never mkfs'd.
	for _, c := range cmds {
		if strings.HasPrefix(c.Name, "mkfs") {
			for _, a := range c.Args {
				if a == "/dev/md0" {
					t.Errorf("preserve device /dev/md0 must never be mkfs'd: %s", c)
				}
			}
		}
	}

	// /data mounts the assembled preserve md device.
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/md0", "/target/data"}})
}

func TestPlan_RetainDiskEmitsNoCommands(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{"name": "nvme0n1p1", "fs": "ext4", "size": "rest"}
				]
			},
			"nvme1n1": {"type": "retain"}
		},
		"mounts": {"/": {"device": "nvme0n1p1"}}
	}`)

	cmds, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, cmd := range cmds {
		if strings.Contains(cmd.String(), "nvme1n1") {
			t.Fatalf("retained disk must not appear in storage commands: %s", cmd)
		}
	}
}

func TestPlan_RetainDiskMustExist(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{"name": "nvme0n1p1", "fs": "ext4", "size": "rest"}
				]
			},
			"nvme1n1": {"type": "retain"}
		},
		"mounts": {"/": {"device": "nvme0n1p1"}}
	}`)

	_, err := Plan(layout, []BlockDevice{{Name: "nvme0n1", SizeBytes: 10737418240}})
	if err == nil || !strings.Contains(err.Error(), `retained disk "nvme1n1" in layout not found in inventory`) {
		t.Fatalf("expected missing retained-disk error, got %v", err)
	}
}

func TestPlan_RetainDiskCannotBeArrayMember(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{"name": "nvme0n1p1", "fs": "ext4", "size": "rest"}
				]
			},
			"nvme1n1": {"type": "retain"},
			"nvme2n1": {"type": "disk"},
			"mddata": {"type": "raid", "level": 1, "members": ["nvme1n1", "nvme2n1"]}
		},
		"mounts": {"/": {"device": "nvme0n1p1"}}
	}`)

	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
		{Name: "nvme2n1", SizeBytes: 10737418240},
	})
	if err == nil || !strings.Contains(err.Error(), `retained disk "nvme1n1" is also a member of raid device "mddata"`) {
		t.Fatalf("expected retained-disk collision error, got %v", err)
	}
}

func TestPlan_RetainDiskCannotBePrefixedPreserveMember(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": {
				"type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{"name": "nvme0n1p1", "fs": "ext4", "size": "rest"}
				]
			},
			"nvme1n1": {"type": "retain"},
			"mddata": {
				"type": "preserve", "level": 1,
				"members": ["/dev/nvme1n1p2", "/dev/nvme2n1p2"],
				"array_uuid": "01234567-89ab-cdef-0123-456789abcdef",
				"fs": "xfs"
			}
		},
		"mounts": {"/": {"device": "nvme0n1p1"}}
	}`)

	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
		{Name: "nvme2n1", SizeBytes: 10737418240},
	})
	if err == nil || !strings.Contains(err.Error(), `retained disk "nvme1n1" is also a member of preserve device "mddata"`) {
		t.Fatalf("expected prefixed retained-disk collision error, got %v", err)
	}
}

func TestMemberReferencesDisk_DoesNotPrefixMatchDifferentDisk(t *testing.T) {
	cases := []struct {
		member string
		disk   string
		want   bool
	}{
		{member: "/dev/nvme1n1", disk: "nvme1n1", want: true},
		{member: "/dev/nvme1n1p2", disk: "nvme1n1", want: true},
		{member: "sda2", disk: "/dev/sda", want: true},
		{member: "nvme1n10", disk: "nvme1n1", want: false},
		{member: "sdaa2", disk: "sda", want: false},
	}
	for _, tc := range cases {
		if got := memberReferencesDisk(tc.member, tc.disk); got != tc.want {
			t.Errorf("memberReferencesDisk(%q, %q) = %v, want %v", tc.member, tc.disk, got, tc.want)
		}
	}
}

func TestPlan_RAIDCreate_NoArrayUUID_Unchanged(t *testing.T) {
	// A raid device without array_uuid keeps the old behavior: no --uuid
	// in the --create invocation.
	layout := mustParseLayout(t, `{
		"layout": {
			"sda": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "sda1", "size": "rest" } ] },
			"sdb": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "sdb1", "size": "rest" } ] },
			"mdroot": { "type": "raid", "level": 1, "members": ["sda1", "sdb1"], "fs": "xfs" }
		},
		"mounts": { "/": { "device": "mdroot" } }
	}`)

	cmds, err := Plan(layout, []BlockDevice{
		{Name: "sda", SizeBytes: 10737418240},
		{Name: "sdb", SizeBytes: 10737418240},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--create", "/dev/md0",
		"--level=1", "--raid-devices=2", "--metadata=1.2", "--run",
		"/dev/sda1", "/dev/sdb1",
	}})

	for _, c := range cmds {
		if c.Name == "mdadm" {
			for _, a := range c.Args {
				if a == "--uuid" {
					t.Errorf("expected no --uuid when array_uuid is unset: %s", c)
				}
			}
		}
	}
}

func TestPlan_RejectsPreserve_MissingArrayUUID(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme1n1", "nvme2n1"],
				"fs": "xfs"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for preserve device missing array_uuid")
	}
}

func TestPlan_RejectsPreserve_MalformedArrayUUID(t *testing.T) {
	// A syntactically wrong array_uuid must fail at plan time with a crisp
	// error, not surface later as a runtime mdadm --assemble failure.
	for _, tc := range []struct {
		name string
		uuid string
	}{
		{"too short", "11111111-1111-1111-1111-11111111111"},  // 31 nibbles
		{"too long", "11111111-1111-1111-1111-1111111111111"}, // 33 nibbles
		{"non-hex", "1111111g-1111-1111-1111-111111111111"},   // 'g'
		{"garbage", "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := mustParseLayout(t, `{
				"layout": {
					"nvme0n1": { "type": "disk", "ptable": "gpt",
						"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
					"mddata": {
						"type": "preserve", "level": 0,
						"members": ["nvme1n1", "nvme2n1"],
						"fs": "xfs",
						"array_uuid": "`+tc.uuid+`"
					}
				},
				"mounts": {
					"/": { "device": "nvme0n1p1" },
					"/data": { "device": "mddata" }
				}
			}`)
			if _, err := Plan(layout, []BlockDevice{{Name: "nvme0n1", SizeBytes: 10737418240}}); err == nil {
				t.Fatalf("expected error for malformed array_uuid %q", tc.uuid)
			}
		})
	}
}

// TestPlan_AcceptsPreserve_ColonGroupedArrayUUID pins that the mdadm
// colon-grouped UUID form (what `mdadm --detail` prints) is accepted, not
// just the RFC-4122 dashed form.
func TestPlan_AcceptsPreserve_ColonGroupedArrayUUID(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme1n1", "nvme2n1"],
				"fs": "xfs",
				"array_uuid": "11111111:22222222:33333333:44444444"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	if _, err := Plan(layout, []BlockDevice{{Name: "nvme0n1", SizeBytes: 10737418240}}); err != nil {
		t.Fatalf("colon-grouped array_uuid should be accepted: %v", err)
	}
}

func TestPlan_RejectsPreserve_MemberAlsoDiskDevice(t *testing.T) {
	// nvme1n1 appears both as a preserve member AND as a disk device
	// (which would be sgdisk --zap-all'd) — must be rejected.
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
			"nvme1n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"nvme2n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme1n1", "nvme2n1"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
		{Name: "nvme2n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for preserve member that is also a disk device")
	}
}

func TestPlan_RejectsPreserve_MemberIsPartitionOfManagedDisk(t *testing.T) {
	// nvme0n1p3 is a partition of the managed disk nvme0n1, which gets
	// sgdisk --zap-all'd — a preserve member naming that partition is
	// just as destroyed as one naming the whole disk. Not merely a
	// same-named whole-disk collision.
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "ext4", "size": "10G" },
					{ "name": "nvme0n1p3", "size": "rest" }
				] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme0n1p3"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for preserve member that is a partition of a managed disk")
	}
}

func TestPlan_RejectsPreserve_MemberDevPrefixed_CollidesWithDisk(t *testing.T) {
	// resolveDevPath treats "nvme1n1" and "/dev/nvme1n1" as the identical
	// device when emitting commands, so the collision guard must
	// normalize the same way — a "/dev/"-prefixed preserve member must
	// still be caught colliding with a bare-named managed disk.
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
			"nvme1n1": { "type": "disk", "ptable": "gpt", "partitions": [] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["/dev/nvme1n1", "nvme2n1"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for /dev/-prefixed preserve member colliding with a managed disk")
	}
}

func TestPlan_RejectsPreserve_MemberAlsoRAIDMember(t *testing.T) {
	// nvme1n1p2 appears both as a preserve member AND as a raid member
	// (which would be mdadm --create'd, destroying it) — must be rejected.
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" }, { "name": "nvme0n1p2", "size": "rest" } ] },
			"nvme1n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme1n1p2", "size": "rest" } ] },
			"mdother": {
				"type": "raid", "level": 1,
				"members": ["nvme0n1p2", "nvme1n1p2"]
			},
			"mddata": {
				"type": "preserve", "level": 1,
				"members": ["nvme1n1p2"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
		{Name: "nvme1n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for preserve member that is also a raid member")
	}
}

func TestPlan_RejectsPreserve_MountedWithoutFS(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "nvme0n1p1", "fs": "ext4", "size": "rest" } ] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme1n1", "nvme2n1"],
				"array_uuid": "22222222-2222-2222-2222-222222222222"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	_, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240},
	})
	if err == nil {
		t.Fatal("expected error for preserve device mounted without fs set")
	}
}

func TestPlan_AcceptsValidPreserve(t *testing.T) {
	// Preserve members (nvme1n1, nvme2n1) are NOT declared as disk
	// devices or listed in inventory — only the boot disk is (Plan's
	// inventory check only applies to disk-type devices).
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "nvme0n1p2", "fs": "ext4", "size": "rest" }
				] },
			"mddata": {
				"type": "preserve", "level": 0,
				"members": ["nvme1n1", "nvme2n1"],
				"fs": "xfs",
				"array_uuid": "22222222-2222-2222-2222-222222222222",
				"fs_uuid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			}
		},
		"mounts": {
			"/": { "device": "nvme0n1p2" },
			"/boot/efi": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	cmds, err := Plan(layout, []BlockDevice{
		{Name: "nvme0n1", SizeBytes: 10737418240, LogicalSectorBytes: 512},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertCommandContains(t, cmds, Command{"mdadm", []string{
		"--assemble", "/dev/md0",
		"--uuid", "22222222-2222-2222-2222-222222222222",
		"/dev/nvme1n1", "/dev/nvme2n1",
	}})
	assertCommandContains(t, cmds, Command{"mount", []string{"/dev/md0", "/target/data"}})
}

func TestPlan_RejectsMountingWholeDisk(t *testing.T) {
	// The planner never lays a disk-level filesystem, so mounting a
	// whole disk must be rejected.
	layout := mustParseLayout(t, `{
		"layout": {
			"vda": { "type": "disk", "ptable": "gpt",
				"partitions": [ { "name": "vda1", "fs": "ext4", "size": "rest" } ] }
		},
		"mounts": { "/": { "device": "vda" } }
	}`)
	_, err := Plan(layout, []BlockDevice{{Name: "vda", SizeBytes: 10737418240}})
	if err == nil {
		t.Fatal("expected error for mounting a whole disk")
	}
}
