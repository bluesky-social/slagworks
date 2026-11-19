//go:build integration

package qemutest_test

// Storage property suite, part 3: handwritten cases. They exist to prove the
// generic driver end to end and to pin the histories that found bugs. The
// generated cases live in storage_gen_prop_integration_test.go.

import (
	"testing"
)

// propD builds a disk. idx makes the size unique within the case.
func propD(id, bus string, gib int, idx int, lbs int, seed string) propDisk {
	return propDisk{ID: id, Bus: bus, Bytes: int64(gib)<<30 + int64(idx)*16*propMiB, LBS: lbs, Seed: seed}
}

// propBootDisk is the hybrid boot disk every shipped script uses: BIOS boot
// partition, ESP, then root.
func propBootDisk(disk string, esp bool, root propPart) propDiskLayout {
	second := propPart{Size: "512M"}
	if esp {
		second = propPart{Size: "512M", FS: "vfat", ESP: true, Mount: "/boot/efi"}
	}
	return propDiskLayout{Disk: disk, Parts: []propPart{{Size: "1M", BIOSGrub: true}, second, root}}
}

func propSingleLayout(disk, fs string) propLayout {
	return propLayout{Disks: []propDiskLayout{
		propBootDisk(disk, true, propPart{Size: "rest", FS: fs, Mount: "/"}),
	}}
}

func propRAID1Layout(d0, d1, fs string) propLayout {
	return propLayout{
		Disks: []propDiskLayout{
			propBootDisk(d0, true, propPart{Size: "rest"}),
			propBootDisk(d1, false, propPart{Size: "rest"}),
		},
		Arrays: []propArray{{
			Name: "mdroot", Type: "raid", Level: 1, FS: fs, Mount: "/",
			Members: []propMember{{Disk: d0, Part: 3}, {Disk: d1, Part: 3}},
		}},
	}
}

const propDataUUID = "5d1f0c1e-7a3b-4c2d-9e8f-0a1b2c3d4e5f"

// propDataLayout is a single-disk root plus a data array over whole disks.
// listed says whether the members are also declared as empty disk targets
// (required for type raid, forbidden for type preserve).
func propDataLayout(boot string, typ string, level int, members ...string) propLayout {
	l := propSingleLayout(boot, "ext4")
	a := propArray{Name: "mddata", Type: typ, Level: level, FS: "xfs", Mount: "/data", UUID: propDataUUID}
	for _, m := range members {
		a.Members = append(a.Members, propMember{Disk: m})
		if typ == "raid" {
			l.Disks = append(l.Disks, propDiskLayout{Disk: m})
		}
	}
	l.Arrays = []propArray{a}
	return l
}

func propHandCases() []propCase {
	all := func(ids ...string) []string { return ids }
	cases := []propCase{
		{
			Name: "uefi-single-raid1", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, "ambiv"), propD("d3", "virtio", 4, 3, 512, "luks"),
				propD("d4", "virtio", 4, 4, 512, "dos"), propD("d5", "virtio", 4, 5, 512, "ext4"),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propSingleLayout("d0", "ext4"),
					Expect: "ok", Why: "blank target disk"},
				{Attach: all("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok", Why: "destructive redeploy"},
				{Attach: all("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propSingleLayout("d1", "xfs"),
					Expect: "fatal:safety check", Why: "d1 carries a partition table and no --destructive"},
			},
		},
		{
			Name: "bios-raid1-single", Firmware: "bios",
			Disks: []propDisk{
				propD("d0", "sata", 8, 0, 512, ""), propD("d1", "sata", 8, 1, 512, ""), propD("d2", "sata", 4, 2, 512, ""),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok", Why: "blank target disks"},
				{Attach: all("d0", "d1", "d2"), Layout: propSingleLayout("d0", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok", Why: "destructive redeploy; d1 keeps a stale RAID half"},
				{Attach: all("d0", "d1", "d2"), Layout: propSingleLayout("d0", "xfs"),
					Destructive: true, WipeAll: true, Expect: "ok", Why: "destructive redeploy with --wipe-all-disks"},
			},
		},
		{
			Name: "uefi-nvme-preserve", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "nvme", 8, 0, 512, ""), propD("d1", "nvme", 4, 1, 512, ""), propD("d2", "nvme", 4, 2, 512, ""),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "blank disks"},
				{Attach: all("d0", "d1", "d2"), Layout: propDataLayout("d0", "preserve", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "preserve the array step 0 created"},
				{Attach: all("d0", "d1", "d2"), Layout: propDataLayout("d0", "preserve", 0, "d1", "d2"),
					Destructive: true, WipeAll: true, Reboot: true, Expect: "ok", Why: "preserve members are exempt from --wipe-all-disks"},
				{Attach: all("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "re-create the array over its old self"},
			},
		},
		{
			Name: "uefi-4kn-fatalat", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 4096, ""), propD("d1", "virtio", 8, 1, 4096, "")},
			Steps: []propStep{
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "xfs"),
					Expect: "ok", Why: "blank 4Kn disks"},
				{Attach: all("d0", "d1"), Layout: propSingleLayout("d1", "ext4"), Destructive: true,
					FatalAt: "installing_bootloader", Expect: "fatal_at", Why: "interrupted after the root was copied"},
				{Attach: all("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Destructive: true, Reboot: true,
					Expect: "ok", Why: "destructive redeploy over an interrupted one"},
			},
		},
		{
			Name: "bios-scsi-kill", Firmware: "bios",
			Disks: []propDisk{propD("d0", "scsi", 8, 0, 512, ""), propD("d1", "scsi", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"), KillAtCmd: 9,
					Expect: "killed", Why: "power lost while the second disk is being partitioned"},
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok|fatal:safety check", Why: "the interrupted deploy may or may not have left a signature"},
				{Attach: all("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Destructive: true,
					Expect: "ok", Why: "destructive redeploy"},
			},
		},
	}
	cases = append(cases, propPatternCases()...)
	cases = append(cases,
		propBIOSMirrorCase("bios-xfs-mirror3-a", 3, "xfs"),
		propBIOSMirrorCase("bios-xfs-mirror3-b", 3, "xfs"),
		propBIOSMirrorCase("bios-xfs-mirror3-c", 3, "xfs"),
		propBIOSMirrorCase("bios-xfs-mirror2-a", 2, "xfs"),
		propBIOSMirrorCase("bios-xfs-mirror2-b", 2, "xfs"),
		propBIOSMirrorCase("bios-ext4-mirror3", 3, "ext4"),
		propBIOSMirrorGrubCase("bios-xfs-mirror3-grub1", 3, 1),
		propBIOSMirrorGrubCase("bios-xfs-mirror2-grub3", 2, 3),
		propSmallESPCase("uefi-4kn-esp256", 4096),
		propSmallESPCase("uefi-512-esp256", 512),
	)
	// The safety check against each kind of prior content, without
	// --destructive. (The fourth kind, "ambiv", gets through the check and
	// lives in storage_findings_prop_integration_test.go.)
	for _, seed := range []string{"ext4", "luks", "dos"} {
		cases = append(cases, propSafetyCase(seed))
	}
	return cases
}

// propPatternCases are targeted histories: each aims one suspected mechanism
// at the system instead of waiting for the generator to stumble on it. The
// ones that hit are in storage_findings_prop_integration_test.go.
func propPatternCases() []propCase {
	all := func(ids ...string) []string { return ids }
	return []propCase{
		{
			// A mirror member becomes an unformatted slot at the same
			// offset. Only the stale-state wipe stands between the old md
			// superblock and the new partition: nothing formats the slot.
			Name: "bios-raw-slot-over-member", Firmware: "bios",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: all("d0", "d1"), Expect: "ok", Why: "blank disks",
					Layout: propLayout{
						Disks: []propDiskLayout{
							{Disk: "d0", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
							{Disk: "d1", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
						},
						Arrays: []propArray{{Name: "mdroot", Type: "raid", Level: 1, FS: "ext4", Mount: "/",
							Members: []propMember{{Disk: "d0", Part: 2}, {Disk: "d1", Part: 2}}}},
					}},
				{Attach: all("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Destructive: true, Expect: "ok", Why: "destructive redeploy; d1's partition 2 is now a raw slot where a member began"},
			},
		},
		{
			// Both members of a whole-disk mirror become bystanders of a
			// --wipe-all-disks deploy.
			Name: "uefi-wipeall-whole-disk-members", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""), propD("d2", "virtio", 4, 2, 512, ""),
			},
			Steps: []propStep{
				{Attach: all("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 1, "d1", "d2"),
					Expect: "ok", Why: "blank disks"},
				{Attach: all("d0", "d1", "d2"), Layout: propSingleLayout("d0", "ext4"),
					Destructive: true, WipeAll: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy with --wipe-all-disks; d1 and d2 must end up without signatures"},
			},
		},
		{
			// A destructive deploy over a disk with signatures outside
			// where the new partition table and filesystems will land.
			Name: "destructive-over-zfs", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, "ambiv")},
			Steps: []propStep{
				{Attach: all("d0"), Layout: propSingleLayout("d0", "ext4"),
					Destructive: true, Expect: "ok", Why: "destructive deploy"},
			},
		},
	}
}

// propBIOSMirrorCase is a BIOS root mirror over n disks, each with a BIOS
// boot partition and a fixed-size member, nothing else. Generated case g0065
// (three members, xfs) stopped in GRUB with "not a correct XFS inode" after a
// clean unmount; these vary the member count and filesystem around it.
func propBIOSMirrorCase(name string, n int, fs string) propCase {
	c := propCase{Name: name, Firmware: "bios"}
	l := propLayout{}
	a := propArray{Name: "mdroot", Type: "raid", Level: 1, FS: fs, Mount: "/"}
	var attach []string
	for i := 0; i < n; i++ {
		id := "d" + string(rune('0'+i))
		c.Disks = append(c.Disks, propD(id, "virtio", 10, i, 512, ""))
		l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "5G"}}})
		a.Members = append(a.Members, propMember{Disk: id, Part: 2})
		attach = append(attach, id)
	}
	l.Arrays = []propArray{a}
	c.Steps = []propStep{{Attach: attach, Layout: l, Reboot: true, Expect: "ok", Why: "blank disks"}}
	return c
}

// propBIOSMirrorGrubCase separates the two things that grow together in
// propBIOSMirrorCase: the number of mirror members and the number of disks
// GRUB is installed on (one grub-install run per disk with a BIOS boot
// partition). Three disks always; the first `members` carry the xfs root
// mirror, the first `grubs` carry a BIOS boot partition.
func propBIOSMirrorGrubCase(name string, members, grubs int) propCase {
	c := propCase{Name: name, Firmware: "bios"}
	l := propLayout{}
	a := propArray{Name: "mdroot", Type: "raid", Level: 1, FS: "xfs", Mount: "/"}
	var attach []string
	for i := 0; i < 3; i++ {
		id := "d" + string(rune('0'+i))
		c.Disks = append(c.Disks, propD(id, "virtio", 10, i, 512, ""))
		attach = append(attach, id)
		parts := []propPart{{Size: "1M", BIOSGrub: i < grubs}, {Size: "5G"}}
		if i >= members && i >= grubs {
			continue // a bystander
		}
		l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: parts})
		if i < members {
			a.Members = append(a.Members, propMember{Disk: id, Part: 2})
		}
	}
	l.Arrays = []propArray{a}
	c.Steps = []propStep{{Attach: attach, Layout: l, Expect: "ok", Why: "blank disks"}}
	return c
}

// propSmallESPCase is a single-disk UEFI layout with a 256M ESP. Generated
// case g0064 put one on a 4Kn disk: mkfs.fat -F 32 warned that the cluster
// count is below the FAT32 minimum, the deploy completed, and the firmware
// could not boot the disk.
func propSmallESPCase(name string, lbs int) propCase {
	return propCase{
		Name: name, Firmware: "uefi",
		Disks: []propDisk{propD("d0", "virtio", 8, 0, lbs, "")},
		Steps: []propStep{{
			Attach: []string{"d0"}, Reboot: true, Expect: "ok", Why: "blank disk",
			Layout: propLayout{Disks: []propDiskLayout{{Disk: "d0", Parts: []propPart{
				{Size: "256M", FS: "vfat", ESP: true, Mount: "/boot/efi"},
				{Size: "rest", FS: "ext4", Mount: "/"},
			}}}},
		}},
	}
}

func propSafetyCase(seed string) propCase {
	return propCase{
		Name: "safety-" + seed, Firmware: "uefi",
		Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, seed)},
		Steps: []propStep{{
			Attach: []string{"d0"}, Layout: propSingleLayout("d0", "ext4"),
			Expect: "fatal:safety check", Why: "the target disk carries " + seed + " content and no --destructive",
		}},
	}
}

func TestStoragePropHand(t *testing.T) {
	for _, c := range propHandCases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}
