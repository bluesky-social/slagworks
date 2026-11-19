//go:build integration && proptest_findings

package qemutest_test

// Property-suite cases that reproduce a confirmed finding. They fail on the
// code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ -run TestStoragePropFindings
//
// Each case names the standalone regression tests for the same finding,
// which do not need the property harness.

import (
	"fmt"
	"testing"
)

func init() {
	// In this build a known violation is a failure again.
	propKnownAreFailures = true
}

func propFindingCases() []propCase {
	all := func(ids ...string) []string { return ids }
	return []propCase{
		// 1. The safety check passes a disk whose signatures conflict.
		// Standalone: TestDeploy_SafetyCheckRefusesConflictingSignatures and
		// internal/agent/storageapply TestCheckDisksClean_*.
		propSafetyCase("ambiv"),

		// 2. RAID5 (and RAID6) cannot be created in the deploy initrd.
		// Standalone: TestDeploy_RAID5DataArray and internal/storage
		// TestAcceptedRAIDLevelsHaveTheirModuleInTheDeployInitrd.
		{
			Name: "uefi-raid5-data", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "nvme", 8, 0, 512, ""), propD("d1", "nvme", 4, 1, 512, ""),
				propD("d2", "nvme", 4, 2, 512, ""), propD("d3", "nvme", 4, 3, 512, ""),
			},
			Steps: []propStep{{
				Attach: all("d0", "d1", "d2", "d3"),
				Layout: propDataLayout("d0", "raid", 5, "d1", "d2", "d3"),
				Reboot: true, Expect: "ok", Why: "RAID5 is a documented level",
			}},
		},

		// 3. The boot disk is the third disk the firmware enumerates; the
		// UEFI boot-local script tries drives 0x80 and 0x81 only.
		// Standalone: TestDeploy_RebootsWhenBootDiskIsThird and
		// internal/httpboot TestBootLocal_UEFI_ReachesADiskBeyondTheSecond.
		{
			Name: "uefi-esp-third-disk", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""), propD("d2", "virtio", 4, 2, 512, ""),
			},
			Steps: []propStep{{
				Attach: all("d1", "d2", "d0"), Layout: propSingleLayout("d0", "ext4"),
				Reboot: true, Expect: "ok", Why: "blank disks; the installed system must survive a reboot",
			}},
		},

		// 4. The root mirror swaps which disk carries the ESP. The disk
		// that had it keeps an unformatted slot at the same offset, the
		// stale-state wipe does not reach inside partitions, and the old
		// ESP, bootloader and all, is still there for the firmware's first
		// disk to boot. Standalone: TestDeploy_StaleESPInUnformattedSlot
		// and internal/storage TestPlan_WipesPartitionsItDoesNotFormat.
		{
			Name: "uefi-esp-swaps-disk", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok", Why: "blank disks"},
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d1", "d0", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy; the installed system must survive a reboot"},
			},
		},

		// 5. A data array keeps its pinned UUID while its membership
		// changes. The disk that left still carries a superblock with that
		// UUID, nothing wipes it (it is outside the layout), and at boot it
		// can claim the array before the real members do.
		// Standalone: TestDeploy_StaleMemberWithPinnedArrayUUID.
		{
			Name: "uefi-stale-uuid-member", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2", "d3"), Layout: propDataLayout("d0", "raid", 1, "d1", "d2"),
					Expect: "ok", Why: "blank disks"},
				{Attach: all("d0", "d1", "d2", "d3"), Layout: propDataLayout("d0", "raid", 1, "d2", "d3"),
					Destructive: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy; d1 is a bystander holding a stale member of the same array UUID"},
			},
		},

		// 7. With a top-level mount besides "/", the agent's unmount order
		// puts the root level with it; when the root goes first it fails
		// busy and the machine reboots with the root filesystem mounted.
		// With four such mounts the root is last only one time in five.
		// On BIOS, GRUB cannot read what the journal still holds.
		// Standalone: internal/agent/storageapply
		// TestCleanupMounts_LeavesNothingMounted.
		propTopMountsCase("bios-topmounts-xfs", "bios", "xfs"),
		propTopMountsCase("bios-topmounts-ext4", "bios", "ext4"),
		propTopMountsCase("uefi-topmounts-ext4", "uefi", "ext4"),

		// 8. A data array lives on disks the redeploy leaves out of its
		// layout (no --wipe-all-disks). The agent created it as /dev/md0, so
		// its superblock asks for md0; the new root mirror is also
		// /dev/md0 in the mdadm.conf the agent writes. When the initramfs
		// assembles the old array first, the root array is refused
		// ("/dev/md0 is already in use") and the machine stops in the
		// initramfs. Generated cases g0027 (BIOS) and g0013 (UEFI).
		{
			Name: "uefi-bystander-array-takes-md0", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2", "d3"), Expect: "ok", Why: "blank disks",
					Layout: func() propLayout {
						l := propDataLayout("d0", "raid", 1, "d2", "d3")
						l.Arrays[0].UUID = "" // an ordinary array, no pinned UUID
						return l
					}()},
				{Attach: all("d0", "d1", "d2", "d3"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy of the root as a mirror; the data disks are left alone"},
			},
		},

		// 6. A root partition with a fixed size and free space after it is
		// grown to the end of the disk by cloud-init on first boot.
		{
			Name: "uefi-root-fixed-size", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, "")},
			Steps: []propStep{{
				Attach: all("d0"), Expect: "ok", Why: "blank disk",
				Layout: propLayout{Disks: []propDiskLayout{
					propBootDisk("d0", true, propPart{Size: "5G", FS: "xfs", Mount: "/"}),
				}},
			}},
		},
	}
}

func propTopMountsCase(name, firmware, rootFS string) propCase {
	parts := []propPart{{Size: "1M", BIOSGrub: true}}
	if firmware == "uefi" {
		parts = append(parts, propPart{Size: "512M", FS: "vfat", ESP: true, Mount: "/boot/efi"})
	}
	parts = append(parts, propPart{Size: "5G", FS: rootFS, Mount: "/"})
	for _, mp := range []string{"/data", "/srv", "/opt", "/home"} {
		parts = append(parts, propPart{Size: "256M", FS: "ext4", Mount: mp})
	}
	return propCase{
		Name: name, Firmware: firmware,
		Disks: []propDisk{propD("d0", "virtio", 10, 0, 512, "")},
		Steps: []propStep{{
			Attach: []string{"d0"}, Expect: "ok", Why: "blank disk",
			Layout: propLayout{Disks: []propDiskLayout{{Disk: "d0", Parts: parts}}},
		}},
	}
}

// TestStoragePropStaleUUIDRepeat runs the stale-member case several times
// over. Which member udev offers to mdadm first decides whether the stale one
// wins, so one run can pass by luck; this measures how often.
func TestStoragePropStaleUUIDRepeat(t *testing.T) {
	var base propCase
	for _, c := range propFindingCases() {
		if c.Name == "uefi-stale-uuid-member" {
			base = c
		}
	}
	for i := 1; i <= 6; i++ {
		c := base
		c.Name = fmt.Sprintf("%s-%d", base.Name, i)
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}

func TestStoragePropFindings(t *testing.T) {
	for _, c := range propFindingCases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}
