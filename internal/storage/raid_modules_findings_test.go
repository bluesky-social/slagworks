//go:build proptest_findings

package storage

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags proptest_findings ./internal/storage/ -run TestAcceptedRAIDLevelsHaveTheirModuleInTheDeployInitrd
//
// Found by the QEMU storage property suite (internal/qemutest,
// TestStoragePropFindings/uefi-raid5-data): a layout with a RAID5 array is
// accepted by Plan and then dies in the deploy initrd, after the disks were
// repartitioned, with
//
//	mdadm: RUN_ARRAY failed: Invalid argument
//	FATAL during storage: mdadm

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// raidPersonality is the kernel module that implements each md level.
var raidPersonality = map[int]string{
	0:  "raid0",
	1:  "raid1",
	4:  "raid456",
	5:  "raid456",
	6:  "raid456",
	10: "raid10",
}

// TestAcceptedRAIDLevelsHaveTheirModuleInTheDeployInitrd ties the planner to
// the deploy image. The agent runs in a chroot of the squashfs, but the
// kernel loads an md personality by running modprobe in the INITRAMFS, and
// the initramfs carries only the md modules its hook names
// (assets/initramfs/hooks/slagworks). A level Plan accepts whose module the
// hook leaves out cannot be created: mdadm --create fails RUN_ARRAY.
func TestAcceptedRAIDLevelsHaveTheirModuleInTheDeployInitrd(t *testing.T) {
	hook, err := os.ReadFile("../../assets/initramfs/hooks/slagworks")
	if err != nil {
		t.Fatalf("read the initramfs hook: %v", err)
	}
	// The hook's module list is the word list of its `for m in ...; do
	// manual_add_modules "$m"` loop.
	loop := regexp.MustCompile(`(?s)for m in (.*?); do\s+manual_add_modules`).FindSubmatch(hook)
	if loop == nil {
		t.Fatal("could not find the manual_add_modules loop in assets/initramfs/hooks/slagworks")
	}
	baked := map[string]bool{}
	for _, m := range strings.Fields(strings.ReplaceAll(string(loop[1]), `\`, " ")) {
		baked[m] = true
	}
	if !baked["raid1"] {
		t.Fatalf("parsed module list %v does not even contain raid1; the parser is wrong", baked)
	}

	for level := 0; level <= 10; level++ {
		// Ask the planner whether it accepts this level, with enough
		// members for any of them.
		layout := Layout{
			Devices: map[string]Device{
				"vda": {Type: "disk", Partitions: []Partition{
					{Name: "vda1", FS: "vfat", Size: "512M", Bootable: true},
					{Name: "vda2", FS: "ext4", Size: "rest"},
				}},
				"mddata": {Type: "raid", Level: level, FS: "xfs",
					Members: []string{"vdb", "vdc", "vdd", "vde"}},
			},
			Mounts: map[string]Mount{"/": {Device: "vda2"}, "/boot/efi": {Device: "vda1"}, "/data": {Device: "mddata"}},
		}
		inventory := []BlockDevice{{Name: "vda"}}
		for _, d := range []string{"vdb", "vdc", "vdd", "vde"} {
			layout.Devices[d] = Device{Type: "disk"}
			inventory = append(inventory, BlockDevice{Name: d})
		}
		if _, err := Plan(layout, inventory); err != nil {
			continue // the planner rejects this level, nothing to bake
		}
		mod, known := raidPersonality[level]
		if !known {
			t.Errorf("Plan accepts RAID level %d but this test does not know its kernel module", level)
			continue
		}
		t.Run(fmt.Sprintf("raid%d", level), func(t *testing.T) {
			if !baked[mod] {
				t.Errorf("Plan accepts RAID level %d, but its md personality %q is not in the deploy initrd's "+
					"module list (assets/initramfs/hooks/slagworks bakes %v): mdadm --create fails with "+
					"RUN_ARRAY Invalid argument after the disks have been repartitioned",
					level, mod, strings.Fields(strings.ReplaceAll(string(loop[1]), `\`, " ")))
			}
		})
	}
}
