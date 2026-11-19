//go:build proptest_findings

package storage

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags proptest_findings ./internal/storage/ -run TestPlan_WipesPartitionsItDoesNotFormat
//
// Found by the QEMU storage property suite (internal/qemutest,
// TestStoragePropFindings/uefi-esp-swaps-disk and generated cases g0002,
// g0013, g0014): a partition the layout leaves unformatted shows whatever
// filesystem last started at its offset, through any number of --destructive
// redeploys. When that filesystem is an old ESP on the firmware's first disk,
// the next PXE boot-local boots the old bootloader, which asks for a root
// filesystem that no longer exists:
//
//	ALERT!  UUID=5890d31b-927d-49de-8cfc-fc5d9975da54 does not exist.  Dropping to a shell!

import (
	"strings"
	"testing"
)

// TestPlan_WipesPartitionsItDoesNotFormat: the plan creates partitions at
// offsets that earlier layouts used. One it formats is overwritten by mkfs.
// One it does not format (an unformatted spare ESP slot, a BIOS boot
// partition, a RAID member) keeps the bytes that were there, and the
// agent's stale-state wipe only clears the whole-disk device. So the plan
// itself has to clear each partition it creates and does not format, after
// the kernel has re-read the table and before anything uses the partition.
//
// The layout is the mirrored-root shape of the qemutest raid1 script: the
// second disk carries an unformatted 512M slot where the first carries the
// ESP.
func TestPlan_WipesPartitionsItDoesNotFormat(t *testing.T) {
	layout := Layout{
		Devices: map[string]Device{
			"vda": {Type: "disk", Boot: true, Partitions: []Partition{
				{Name: "vda1", Size: "1M", BIOSGrub: true},
				{Name: "vda2", Size: "512M", FS: "vfat", Bootable: true},
				{Name: "vda3", Size: "rest"},
			}},
			"vdb": {Type: "disk", Partitions: []Partition{
				{Name: "vdb1", Size: "1M", BIOSGrub: true},
				{Name: "vdb2", Size: "512M"},
				{Name: "vdb3", Size: "rest"},
			}},
			"mdroot": {Type: "raid", Level: 1, Members: []string{"vda3", "vdb3"}, FS: "ext4"},
		},
		Mounts: map[string]Mount{"/": {Device: "mdroot"}, "/boot/efi": {Device: "vda2"}},
	}
	cmds, err := Plan(layout, []BlockDevice{{Name: "vda"}, {Name: "vdb"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	index := func(pred func(Command) bool) int {
		for i, c := range cmds {
			if pred(c) {
				return i
			}
		}
		return -1
	}
	names := func(dev string) func(Command) bool {
		return func(c Command) bool { return len(c.Args) > 0 && c.Args[len(c.Args)-1] == dev }
	}
	var plan []string
	for _, c := range cmds {
		plan = append(plan, c.String())
	}

	// Every partition without a filesystem of its own.
	for _, part := range []struct{ dev, disk, why string }{
		{"/dev/vdb2", "/dev/vdb", "unformatted slot; an ESP on this disk in an earlier layout stays bootable"},
		{"/dev/vda1", "/dev/vda", "BIOS boot partition"},
		{"/dev/vdb1", "/dev/vdb", "BIOS boot partition"},
		{"/dev/vda3", "/dev/vda", "RAID member; an old filesystem stays under the md superblock"},
		{"/dev/vdb3", "/dev/vdb", "RAID member; an old filesystem stays under the md superblock"},
	} {
		wipe := index(func(c Command) bool { return c.Name == "wipefs" && names(part.dev)(c) })
		if wipe < 0 {
			t.Errorf("the plan never clears %s (%s)", part.dev, part.why)
			continue
		}
		reread := index(func(c Command) bool {
			return c.Name == "blockdev" && names(part.disk)(c)
		})
		if wipe < reread {
			t.Errorf("%s is cleared before its disk's partition table is re-read", part.dev)
		}
		if use := index(func(c Command) bool { return c.Name == "mdadm" }); use >= 0 && wipe > use {
			t.Errorf("%s is cleared after mdadm has already used the members", part.dev)
		}
	}
	if t.Failed() {
		t.Logf("plan:\n  %s", strings.Join(plan, "\n  "))
	}
}
