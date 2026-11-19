//go:build integration

package qemutest_test

// Storage property suite, part 3: handwritten histories and the tests that
// play them. They prove the generic driver end to end and pin the histories
// that have caught defects, each aimed at one mechanism. The generated cases
// live in storage_property_gen_integration_test.go.
//
// Every step of a history is a VM boot, so the tests that run by default are
// few and each holds a handful of steps; propDeepEnv adds the rest.

import (
	"os"
	"sort"
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
// For type raid the members are also declared as empty disk targets, which
// the planner requires; for type preserve they must not be.
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

// propDeepEnv, when set to 1, makes TestStoragePropDeep play every pinned
// history the default tests leave out: about thirty more VM boots.
const propDeepEnv = "SLAGWORKS_STORAGE_PROP_DEEP"

func propAll(ids ...string) []string { return ids }

// propPinnedCases returns every handwritten history, keyed by name.
func propPinnedCases() map[string]propCase {
	cases := []propCase{
		{
			// Bystander disks with every kind of prior content sit next to a
			// redeploy that changes the root topology; none may be written.
			Name: "uefi-single-raid1", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, "ambiv"), propD("d3", "virtio", 4, 3, 512, "luks"),
				propD("d4", "virtio", 4, 4, 512, "dos"), propD("d5", "virtio", 4, 5, 512, "ext4"),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propSingleLayout("d0", "ext4"),
					Expect: "ok", Why: "blank target disk"},
				{Attach: propAll("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok", Why: "destructive redeploy"},
				{Attach: propAll("d0", "d1", "d2", "d3", "d4", "d5"), Layout: propSingleLayout("d1", "xfs"),
					Expect: "fatal:safety check", Why: "d1 carries a partition table and no --destructive"},
			},
		},
		{
			Name: "bios-raid1-single", Firmware: "bios",
			Disks: []propDisk{
				propD("d0", "sata", 8, 0, 512, ""), propD("d1", "sata", 8, 1, 512, ""), propD("d2", "sata", 4, 2, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok", Why: "blank target disks"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propSingleLayout("d0", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok", Why: "destructive redeploy; d1 keeps a stale RAID half"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propSingleLayout("d0", "xfs"),
					Destructive: true, WipeAll: true, Expect: "ok", Why: "destructive redeploy with --wipe-all-disks"},
			},
		},
		{
			Name: "uefi-nvme-preserve", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "nvme", 8, 0, 512, ""), propD("d1", "nvme", 4, 1, 512, ""), propD("d2", "nvme", 4, 2, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "blank disks"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propDataLayout("d0", "preserve", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "preserve the array step 0 created"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propDataLayout("d0", "preserve", 0, "d1", "d2"),
					Destructive: true, WipeAll: true, Reboot: true, Expect: "ok", Why: "preserve members are exempt from --wipe-all-disks"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 0, "d1", "d2"),
					Destructive: true, Expect: "ok", Why: "re-create the array over its old self"},
			},
		},
		{
			Name: "uefi-4kn-fatalat", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 4096, ""), propD("d1", "virtio", 8, 1, 4096, "")},
			Steps: []propStep{
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "xfs"),
					Expect: "ok", Why: "blank 4Kn disks"},
				{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d1", "ext4"), Destructive: true,
					FatalAt: "installing_bootloader", Expect: "fatal_at", Why: "interrupted after the root was copied"},
				{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Destructive: true, Reboot: true,
					Expect: "ok", Why: "destructive redeploy over an interrupted one"},
			},
		},
		{
			// Power is lost partway through the storage commands; whatever
			// that leaves behind, a destructive redeploy must install.
			Name: "bios-scsi-kill", Firmware: "bios",
			Disks: []propDisk{propD("d0", "scsi", 8, 0, 512, ""), propD("d1", "scsi", 8, 1, 512, "")},
			Steps: []propStep{
				// The plan spends ten commands on the first disk (zap, three
				// partitions, re-read, five to format or clear them); the
				// thirteenth creates the second disk's second partition.
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"), KillAtCmd: 13,
					Expect: "killed", Why: "power lost while the second disk is being partitioned"},
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok|fatal:safety check", Why: "the interrupted deploy may or may not have left a signature"},
				{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Destructive: true,
					Expect: "ok", Why: "destructive redeploy"},
			},
		},
		{
			// A mirror member becomes an unformatted slot at the same
			// offset. Nothing formats the slot, so only the plan's clearing
			// of unformatted partitions stands between the old md
			// superblock and the new partition.
			Name: "bios-raw-slot-over-member", Firmware: "bios",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: propAll("d0", "d1"), Expect: "ok", Why: "blank disks",
					Layout: propLayout{
						Disks: []propDiskLayout{
							{Disk: "d0", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
							{Disk: "d1", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
						},
						Arrays: []propArray{{Name: "mdroot", Type: "raid", Level: 1, FS: "ext4", Mount: "/",
							Members: []propMember{{Disk: "d0", Part: 2}, {Disk: "d1", Part: 2}}}},
					}},
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
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
				{Attach: propAll("d0", "d1", "d2"), Layout: propDataLayout("d0", "raid", 1, "d1", "d2"),
					Expect: "ok", Why: "blank disks"},
				{Attach: propAll("d0", "d1", "d2"), Layout: propSingleLayout("d0", "ext4"),
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
				{Attach: propAll("d0"), Layout: propSingleLayout("d0", "ext4"),
					Destructive: true, Expect: "ok", Why: "destructive deploy"},
			},
		},
		{
			// RAID5 needs a kernel module the deploy environment must carry.
			Name: "uefi-raid5-data", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "nvme", 8, 0, 512, ""), propD("d1", "nvme", 4, 1, 512, ""),
				propD("d2", "nvme", 4, 2, 512, ""), propD("d3", "nvme", 4, 3, 512, ""),
			},
			Steps: []propStep{{
				Attach: propAll("d0", "d1", "d2", "d3"),
				Layout: propDataLayout("d0", "raid", 5, "d1", "d2", "d3"),
				Reboot: true, Expect: "ok", Why: "RAID5 is a documented level",
			}},
		},
		{
			// The boot disk is the third disk the firmware enumerates. The
			// first boot uses the one-shot BootNext; the reboot goes through
			// the boot-local script, which has to find the disk itself.
			Name: "uefi-esp-third-disk", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""), propD("d2", "virtio", 4, 2, 512, ""),
			},
			Steps: []propStep{{
				Attach: propAll("d1", "d2", "d0"), Layout: propSingleLayout("d0", "ext4"),
				Reboot: true, Expect: "ok", Why: "blank disks; the installed system must survive a reboot",
			}},
		},
		{
			// The root mirror swaps which disk carries the ESP. The disk
			// that had it keeps an unformatted slot at the same offset; if
			// the old ESP survived there, bootloader and all, the firmware's
			// first disk would boot it.
			Name: "uefi-esp-swaps-disk", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Expect: "ok", Why: "blank disks"},
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d1", "d0", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy; the installed system must survive a reboot"},
			},
		},
		{
			// A data array keeps its pinned UUID while its membership
			// changes. The disk that left still carries a superblock with
			// that UUID and the deploy does not write it (it is outside the
			// layout), so at boot it could claim the array before the real
			// members do. The agent refuses instead.
			Name: "uefi-stale-uuid-member", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2", "d3"), Layout: propDataLayout("d0", "raid", 1, "d1", "d2"),
					Expect: "ok", Why: "blank disks"},
				{Attach: propAll("d0", "d1", "d2", "d3"), Layout: propDataLayout("d0", "raid", 1, "d2", "d3"),
					Destructive: true, Expect: "fatal:stale array member check",
					Why: "d1 is outside the layout and holds a stale member of the pinned array UUID"},
			},
		},
		{
			// A data array lives on disks the redeploy leaves out of its
			// layout (no --wipe-all-disks). Its superblock asks for the name
			// the new root mirror has too; the installed system must
			// assemble the root array and leave the old one alone.
			Name: "uefi-bystander-array", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2", "d3"), Expect: "ok", Why: "blank disks",
					Layout: func() propLayout {
						l := propDataLayout("d0", "raid", 1, "d2", "d3")
						l.Arrays[0].UUID = "" // an ordinary array, no pinned UUID
						return l
					}()},
				{Attach: propAll("d0", "d1", "d2", "d3"), Layout: propRAID1Layout("d0", "d1", "ext4"),
					Destructive: true, Reboot: true, Expect: "ok",
					Why: "destructive redeploy of the root as a mirror; the data disks are left alone"},
			},
		},
		{
			// A root partition with a fixed size and free space after it:
			// nothing at first boot may grow it.
			Name: "uefi-root-fixed-size", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, "")},
			Steps: []propStep{{
				Attach: propAll("d0"), Expect: "ok", Why: "blank disk",
				Layout: propLayout{Disks: []propDiskLayout{
					propBootDisk("d0", true, propPart{Size: "5G", FS: "xfs", Mount: "/"}),
				}},
			}},
		},
		// With top-level mounts besides "/", the agent has to unmount the
		// root last; a root left mounted reboots with an unreplayed journal,
		// which GRUB on BIOS may not be able to read.
		propTopMountsCase("bios-topmounts-xfs", "bios", "xfs"),
		propTopMountsCase("bios-topmounts-ext4", "bios", "ext4"),
		propTopMountsCase("uefi-topmounts-ext4", "uefi", "ext4"),
		propBIOSMirrorCase("bios-xfs-mirror3", 3, "xfs"),
		propBIOSMirrorCase("bios-xfs-mirror2", 2, "xfs"),
		propBIOSMirrorCase("bios-ext4-mirror3", 3, "ext4"),
		propBIOSMirrorGrubCase("bios-xfs-mirror3-grub1", 3, 1),
		propBIOSMirrorGrubCase("bios-xfs-mirror2-grub3", 2, 3),
		propSmallESPCase("uefi-4kn-esp256", 4096, "fatal:planning storage layout",
			"a 256M vfat partition is below the FAT32 minimum at 4096-byte sectors"),
		propSmallESPCase("uefi-512-esp256", 512, "ok", "blank disk"),
	}
	// The safety check against each kind of prior content, without
	// --destructive. "ambiv" is ext4 plus ZFS labels: two signatures a
	// probe refuses to choose between, which must not read as a clean disk.
	for _, seed := range []string{"ext4", "luks", "dos", "ambiv"} {
		cases = append(cases, propSafetyCase(seed))
	}
	byName := make(map[string]propCase, len(cases))
	for _, c := range cases {
		if _, dup := byName[c.Name]; dup {
			panic("prop: duplicate pinned case " + c.Name)
		}
		byName[c.Name] = c
	}
	return byName
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

// propBIOSMirrorCase is a BIOS root mirror over n disks, each with a BIOS
// boot partition and a fixed-size member, nothing else. GRUB is installed
// once per disk, and each run must leave the root filesystem readable to the
// next and to the GRUB that boots.
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

// propSmallESPCase is a single-disk UEFI layout with a 256M ESP. On a 4Kn
// disk mkfs.fat cannot make that FAT32, and firmware cannot boot what it
// makes instead, so the planner refuses the layout; at 512-byte sectors it
// is an ordinary ESP.
func propSmallESPCase(name string, lbs int, expect, why string) propCase {
	st := propStep{
		Attach: []string{"d0"}, Expect: expect, Why: why,
		Layout: propLayout{Disks: []propDiskLayout{{Disk: "d0", Parts: []propPart{
			{Size: "256M", FS: "vfat", ESP: true, Mount: "/boot/efi"},
			{Size: "rest", FS: "ext4", Mount: "/"},
		}}}},
	}
	st.Reboot = expect == "ok"
	return propCase{
		Name: name, Firmware: "uefi",
		Disks: []propDisk{propD("d0", "virtio", 8, 0, lbs, "")},
		Steps: []propStep{st},
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

// propDefaultRuns maps each test that runs by default to the pinned
// histories it plays. The shard runner deals top-level tests to runners, so
// a group is sized to a few minutes of VM time and the groups spread out.
var propDefaultRuns = map[string][]string{
	"TestStoragePropSafety":      {"safety-ext4", "safety-luks", "safety-dos", "safety-ambiv"},
	"TestStoragePropRedeploy":    {"uefi-single-raid1"},
	"TestStoragePropInterrupted": {"bios-scsi-kill"},
	"TestStoragePropESPMoves":    {"uefi-esp-swaps-disk"},
	"TestStoragePropThirdDisk":   {"uefi-esp-third-disk"},
	"TestStoragePropArrays":      {"uefi-raid5-data", "uefi-stale-uuid-member"},
}

func runPropPinned(t *testing.T) {
	t.Helper()
	all := propPinnedCases()
	names, ok := propDefaultRuns[t.Name()]
	if !ok {
		t.Fatalf("%s has no entry in propDefaultRuns", t.Name())
	}
	for _, name := range names {
		c, ok := all[name]
		if !ok {
			t.Fatalf("no pinned case %q", name)
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}

// TestStoragePropSafety: a deploy without --destructive refuses a target
// disk with prior content, whatever its kind, and writes nothing.
func TestStoragePropSafety(t *testing.T) { runPropPinned(t) }

// TestStoragePropRedeploy: single disk, then a destructive redeploy as a
// mirror, then a refused non-destructive one, beside four bystander disks
// with prior content that must come through unwritten.
func TestStoragePropRedeploy(t *testing.T) { runPropPinned(t) }

// TestStoragePropInterrupted: power is lost during the storage commands of
// a BIOS mirror deploy, and the machine is deployed again over the remains.
func TestStoragePropInterrupted(t *testing.T) { runPropPinned(t) }

// TestStoragePropESPMoves: a redeploy moves the ESP to the other disk of the
// root mirror, and the machine must keep booting the new one.
func TestStoragePropESPMoves(t *testing.T) { runPropPinned(t) }

// TestStoragePropThirdDisk: the installed system reboots when its boot disk
// is the third the firmware enumerates.
func TestStoragePropThirdDisk(t *testing.T) { runPropPinned(t) }

// TestStoragePropArrays: a RAID5 data array installs and survives a reboot,
// and a redeploy that would leave a stale member of a pinned array UUID on a
// bystander disk is refused before any disk is written.
func TestStoragePropArrays(t *testing.T) { runPropPinned(t) }

// TestStoragePropDeep plays every pinned history the default tests leave
// out. Set SLAGWORKS_STORAGE_PROP_DEEP=1 and give it a long -timeout and a
// -parallel the host's memory allows (each VM takes 4 GB).
func TestStoragePropDeep(t *testing.T) {
	if os.Getenv(propDeepEnv) != "1" {
		t.Skipf("set %s=1 to play the remaining pinned histories (about thirty VM boots)", propDeepEnv)
	}
	inDefault := map[string]bool{}
	for _, names := range propDefaultRuns {
		for _, name := range names {
			inDefault[name] = true
		}
	}
	all := propPinnedCases()
	names := make([]string, 0, len(all))
	for name := range all {
		if !inDefault[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		c := all[name]
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}
