package storage

import (
	"slices"
	"strings"
	"testing"
)

// cmdIndex returns the index of the first command matching pred, or -1.
func cmdIndex(cmds []Command, pred func(Command) bool) int {
	return slices.IndexFunc(cmds, pred)
}

// isWipeOf reports whether c is `wipefs -a <dev>`.
func isWipeOf(dev string) func(Command) bool {
	return func(c Command) bool {
		return c.Name == "wipefs" && slices.Equal(c.Args, []string{"-a", dev})
	}
}

// isZeroHeadOf reports whether c zeroes the first 4 KiB of dev.
func isZeroHeadOf(dev string) func(Command) bool {
	return func(c Command) bool {
		return c.Name == "dd" && slices.Equal(c.Args,
			[]string{"if=/dev/zero", "of=" + dev, "bs=4096", "count=1", "conv=fsync"})
	}
}

// clearTargets returns, in plan order, the device each wipefs or dd command
// writes.
func clearTargets(cmds []Command) []string {
	var devs []string
	for _, c := range cmds {
		switch c.Name {
		case "wipefs":
			devs = append(devs, c.Args[len(c.Args)-1])
		case "dd":
			for _, a := range c.Args {
				if dev, ok := strings.CutPrefix(a, "of="); ok {
					devs = append(devs, dev)
				}
			}
		}
	}
	return devs
}

func logPlan(t *testing.T, cmds []Command) {
	t.Helper()
	var lines []string
	for _, c := range cmds {
		lines = append(lines, c.String())
	}
	t.Logf("plan:\n  %s", strings.Join(lines, "\n  "))
}

// TestPlan_WipesPartitionsItDoesNotFormat: the plan creates partitions at
// offsets earlier layouts used. mkfs overwrites the ones it formats; one it
// does not format (an unformatted spare ESP slot, a BIOS boot partition, a
// RAID member) would keep whatever filesystem last started there, and the
// agent's --destructive stale-state wipe only clears whole-disk signatures.
// An old ESP left in disk 0's spare slot is still bootable: the firmware's
// boot-local path picks it, and its bootloader asks for a root filesystem
// that no longer exists ("ALERT! UUID=... does not exist. Dropping to a
// shell!"). So the plan clears every partition it creates and does not
// format, after the kernel has re-read the table and before mdadm uses it:
// wipefs -a for the signatures Linux probes, and a zeroed first 4 KiB for
// the FAT boot sector, which UEFI firmware mounts even with its magic bytes
// erased.
//
// The layout is the mirrored root after a redeploy moved the ESP from disk
// 0 to disk 1: vda2 is now the unformatted slot where the old ESP was.
func TestPlan_WipesPartitionsItDoesNotFormat(t *testing.T) {
	layout := Layout{
		Devices: map[string]Device{
			"vda": {Type: "disk", Partitions: []Partition{
				{Name: "vda1", Size: "1M", BIOSGrub: true},
				{Name: "vda2", Size: "512M"},
				{Name: "vda3", Size: "rest"},
			}},
			"vdb": {Type: "disk", Boot: true, Partitions: []Partition{
				{Name: "vdb1", Size: "1M", BIOSGrub: true},
				{Name: "vdb2", Size: "512M", FS: "vfat", Bootable: true},
				{Name: "vdb3", Size: "rest"},
			}},
			"mdroot": {Type: "raid", Level: 1, Members: []string{"vda3", "vdb3"}, FS: "ext4"},
		},
		Mounts: map[string]Mount{"/": {Device: "mdroot"}, "/boot/efi": {Device: "vdb2"}},
	}
	cmds, err := Plan(layout, []BlockDevice{{Name: "vda"}, {Name: "vdb"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	firstMdadm := cmdIndex(cmds, func(c Command) bool { return c.Name == "mdadm" })

	for _, part := range []struct{ dev, disk, why string }{
		{"/dev/vda1", "/dev/vda", "BIOS boot partition"},
		{"/dev/vda2", "/dev/vda", "unformatted slot where an earlier layout put a bootable ESP"},
		{"/dev/vda3", "/dev/vda", "RAID member; an old filesystem stays under the md superblock"},
		{"/dev/vdb1", "/dev/vdb", "BIOS boot partition"},
		{"/dev/vdb3", "/dev/vdb", "RAID member; an old filesystem stays under the md superblock"},
	} {
		reread := cmdIndex(cmds, func(c Command) bool {
			return c.Name == "blockdev" && slices.Equal(c.Args, []string{"--rereadpt", part.disk})
		})
		for _, step := range []struct {
			what string
			pred func(Command) bool
		}{
			{"erases the signatures of", isWipeOf(part.dev)},
			{"zeroes the boot sector of", isZeroHeadOf(part.dev)},
		} {
			at := cmdIndex(cmds, step.pred)
			if at < 0 {
				t.Errorf("the plan never %s %s (%s)", step.what, part.dev, part.why)
				continue
			}
			if reread < 0 || at < reread {
				t.Errorf("the plan %s %s at %d, before its disk's partition table is re-read at %d", step.what, part.dev, at, reread)
			}
			if firstMdadm >= 0 && at > firstMdadm {
				t.Errorf("the plan %s %s at %d, after mdadm has used the members at %d", step.what, part.dev, at, firstMdadm)
			}
		}
	}
	// mkfs is the writer for a partition that declares a filesystem; the
	// plan clears only what it leaves unformatted.
	if slices.Contains(clearTargets(cmds), "/dev/vdb2") {
		t.Errorf("the plan clears the ESP /dev/vdb2 it is about to format")
	}
	if t.Failed() {
		logPlan(t, cmds)
	}
}

// TestPlan_WipesRAIDArrayItDoesNotFormat: a raid device without fs is a
// fresh array the plan creates and leaves unformatted. mdadm --create over
// the same members lands its data area at the same offset as the array
// those members held before, so the old array's filesystem (or LVM label)
// shows up on the new md device unless the plan clears it after creating it.
func TestPlan_WipesRAIDArrayItDoesNotFormat(t *testing.T) {
	layout := Layout{
		Devices: map[string]Device{
			"vda": {Type: "disk", Partitions: []Partition{
				{Name: "vda1", Size: "512M", FS: "vfat", Bootable: true},
				{Name: "vda2", Size: "rest", FS: "ext4"},
			}},
			"vdb":       {Type: "disk"},
			"vdc":       {Type: "disk"},
			"mdscratch": {Type: "raid", Level: 1, Members: []string{"vdb", "vdc"}},
		},
		Mounts: map[string]Mount{"/": {Device: "vda2"}, "/boot/efi": {Device: "vda1"}},
	}
	cmds, err := Plan(layout, []BlockDevice{{Name: "vda"}, {Name: "vdb"}, {Name: "vdc"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	create := cmdIndex(cmds, func(c Command) bool {
		return c.Name == "mdadm" && len(c.Args) > 1 && c.Args[0] == "--create" && c.Args[1] == "/dev/md0"
	})
	wipe := cmdIndex(cmds, isWipeOf("/dev/md0"))
	if create < 0 || wipe < 0 || wipe < create {
		logPlan(t, cmds)
		t.Fatalf("want wipefs -a /dev/md0 after mdadm --create /dev/md0; create at %d, wipe at %d", create, wipe)
	}
}

// TestPlan_NeverWipesPreservedOrRetainedDevices: preserve arrays exist to
// keep their data, and a retained disk is a no-write boundary. A preserve
// array is assembled and never formatted, and its members are partitions
// nothing in the plan formats, so "clear what the plan leaves unformatted"
// must be scoped to what the plan creates.
func TestPlan_NeverWipesPreservedOrRetainedDevices(t *testing.T) {
	layout := mustParseLayout(t, `{
		"layout": {
			"nvme0n1": { "type": "disk", "ptable": "gpt", "boot": true,
				"partitions": [
					{ "name": "nvme0n1p1", "fs": "vfat", "size": "512M", "bootable": true },
					{ "name": "nvme0n1p2", "size": "rest" }
				] },
			"nvme1n1": { "type": "disk", "ptable": "gpt",
				"partitions": [
					{ "name": "nvme1n1p1", "size": "512M" },
					{ "name": "nvme1n1p2", "size": "rest" }
				] },
			"nvme4n1": { "type": "retain" },
			"mdroot": { "type": "raid", "level": 1, "members": ["nvme0n1p2", "nvme1n1p2"], "fs": "xfs" },
			"mddata": { "type": "preserve", "level": 1, "members": ["nvme2n1p1", "nvme3n1p1"],
				"fs": "xfs", "array_uuid": "22222222-2222-2222-2222-222222222222" },
			"mdspare": { "type": "preserve", "level": 1, "members": ["nvme2n1p2", "nvme3n1p2"],
				"array_uuid": "33333333-3333-3333-3333-333333333333" }
		},
		"mounts": {
			"/": { "device": "mdroot" },
			"/boot/efi": { "device": "nvme0n1p1" },
			"/data": { "device": "mddata" }
		}
	}`)
	cmds, err := Plan(layout, []BlockDevice{{Name: "nvme0n1"}, {Name: "nvme1n1"}, {Name: "nvme4n1"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// mddata = md0 and mdspare = md2 (preserve), mdroot = md1 (raid).
	want := []string{
		"/dev/nvme0n1p2", "/dev/nvme0n1p2",
		"/dev/nvme1n1p1", "/dev/nvme1n1p1",
		"/dev/nvme1n1p2", "/dev/nvme1n1p2",
	}
	if got := clearTargets(cmds); !slices.Equal(got, want) {
		logPlan(t, cmds)
		t.Fatalf("cleared devices = %v, want %v (never a preserve array, its members, or a retained disk)", got, want)
	}
}
