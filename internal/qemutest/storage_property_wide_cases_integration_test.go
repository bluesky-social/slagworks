//go:build integration

package qemutest_test

// Storage property suite, part 5: pinned histories for the combinations the
// first pinned set avoided. TestStoragePropDeep plays them with the rest.
//
//   - root on parity RAID (5 and 6) under both firmwares;
//   - a BIOS root mirror whose second disk has 4096-byte sectors (GRUB is
//     installed on both disks; the firmware boots the first, a 512-byte disk,
//     since SeaBIOS cannot boot a 4Kn disk);
//   - arrays the deploy environment has assembled before the agent looks
//     (propStep.PreScan): a bystander array at the /dev/mdN a new array
//     needs, with and without --destructive, and a preserve array beside a
//     bystander array.

func propParityRootCase(name, firmware string, level, n int) propCase {
	c := propCase{Name: name, Firmware: firmware}
	l := propLayout{}
	a := propArray{Name: "mdroot", Type: "raid", Level: level, FS: "ext4", Mount: "/"}
	var attach []string
	for i := 0; i < n; i++ {
		id := "d" + string(rune('0'+i))
		c.Disks = append(c.Disks, propD(id, "virtio", 8, i, 512, ""))
		attach = append(attach, id)
		parts := []propPart{{Size: "1M", BIOSGrub: true}}
		if firmware == "uefi" {
			if i == 0 {
				parts = append(parts, propPart{Size: "512M", FS: "vfat", ESP: true, Mount: "/boot/efi"})
			} else {
				parts = append(parts, propPart{Size: "512M"})
			}
		}
		parts = append(parts, propPart{Size: "rest"})
		l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: parts})
		a.Members = append(a.Members, propMember{Disk: id, Part: len(parts)})
	}
	l.Arrays = []propArray{a}
	c.Steps = []propStep{{Attach: attach, Layout: l, Reboot: true, Expect: "ok",
		Why: "blank disks; RAID5 and RAID6 are documented levels and the root may live on any array"}}
	return c
}

// propRootMovesCase: the first deploy installs on d0; the second puts the root
// on the blank d1 and leaves d0, old install and all, outside its layout.
// The firmware enumerates d0 first. Every boot of the new install must come
// up on the new root, the second boot (PXE, then the boot-local script) as
// much as the first.
func propRootMovesCase(name, firmware string, destructive, wipeAll bool) propCase {
	return propCase{
		Name: name, Firmware: firmware,
		Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
		Steps: []propStep{
			{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Expect: "ok", Why: "blank disks"},
			{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d1", "ext4"),
				Destructive: destructive, WipeAll: wipeAll, Reboot: true, Expect: "ok",
				Why: "d1 is blank; d0 keeps the first install and is outside the layout"},
		},
	}
}

func propWideCases() []propCase {
	return []propCase{
		propRootMovesCase("uefi-root-moves-off-old-install", "uefi", false, false),
		propRootMovesCase("bios-root-moves-off-old-install", "bios", false, false),
		propRootMovesCase("uefi-root-moves-wipe-all", "uefi", true, true),
		{
			// bios-raid1-single's first two steps under UEFI: the second
			// layout has no array at all, and d1 keeps a stale half of the
			// old root mirror outside it. The installed system must not
			// assemble it.
			Name: "uefi-raid1-then-single", Firmware: "uefi",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, "")},
			Steps: []propStep{
				{Attach: propAll("d0", "d1"), Layout: propRAID1Layout("d0", "d1", "ext4"), Expect: "ok", Why: "blank disks"},
				{Attach: propAll("d0", "d1"), Layout: propSingleLayout("d0", "ext4"), Destructive: true, Expect: "ok",
					Why: "destructive redeploy without an array; d1 keeps a stale RAID half outside the layout"},
			},
		},
		propParityRootCase("uefi-root-raid5", "uefi", 5, 3),
		propParityRootCase("bios-root-raid5", "bios", 5, 3),
		propParityRootCase("uefi-root-raid6", "uefi", 6, 4),
		propParityRootCase("bios-root-raid6", "bios", 6, 4),
		{
			Name: "bios-4kn-second-mirror-disk", Firmware: "bios",
			Disks: []propDisk{propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 4096, "")},
			Steps: []propStep{{
				Attach: propAll("d0", "d1"), Expect: "ok", Reboot: true,
				Why: "blank disks; GRUB goes on both, the firmware boots the 512-byte one",
				Layout: propLayout{
					Disks: []propDiskLayout{
						{Disk: "d0", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
						{Disk: "d1", Parts: []propPart{{Size: "1M", BIOSGrub: true}, {Size: "rest"}}},
					},
					Arrays: []propArray{{Name: "mdroot", Type: "raid", Level: 1, FS: "ext4", Mount: "/",
						Members: []propMember{{Disk: "d0", Part: 2}, {Disk: "d1", Part: 2}}}},
				},
			}},
		},
		{
			// The deploy environment has assembled the old data array, and
			// it holds /dev/md0, the node the new layout's only array needs.
			// Without --destructive the md name check must refuse with
			// nothing written.
			Name: "uefi-prescan-bystander-md0", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
				propD("d4", "virtio", 4, 4, 512, ""), propD("d5", "virtio", 4, 5, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d2", "d3"), Expect: "ok", Why: "blank disks",
					Layout: propUnpinnedDataLayout("d0", "d2", "d3")},
				{Attach: propAll("d0", "d1", "d2", "d3", "d4", "d5"), PreScan: true, Expect: "fatal:md name check",
					Why:    "d1, d4 and d5 are blank, the array on d2 and d3 is assembled at /dev/md0 and the deploy is not --destructive",
					Layout: propUnpinnedDataLayout("d1", "d4", "d5")},
			},
		},
		{
			// As above with --destructive: the deploy stops the old array,
			// leaves its disks unwritten, and the installed system must not
			// assemble it.
			Name: "uefi-prescan-bystander-md0-destructive", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 8, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
				propD("d4", "virtio", 4, 4, 512, ""), propD("d5", "virtio", 4, 5, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d2", "d3"), Expect: "ok", Why: "blank disks",
					Layout: propUnpinnedDataLayout("d0", "d2", "d3")},
				{Attach: propAll("d0", "d1", "d2", "d3", "d4", "d5"), PreScan: true, Destructive: true, Reboot: true,
					Expect: "ok", Why: "--destructive stops the assembled bystander array and writes only the layout's disks",
					Layout: propUnpinnedDataLayout("d1", "d4", "d5")},
			},
		},
		{
			// A pinned data array is preserved while an unpinned one on other
			// disks becomes a bystander. The deploy environment has assembled
			// both. The new root goes on a blank disk, without --destructive.
			Name: "uefi-prescan-preserve-beside-bystander", Firmware: "uefi",
			Disks: []propDisk{
				propD("d0", "virtio", 8, 0, 512, ""), propD("d1", "virtio", 4, 1, 512, ""),
				propD("d2", "virtio", 4, 2, 512, ""), propD("d3", "virtio", 4, 3, 512, ""),
				propD("d4", "virtio", 4, 4, 512, ""), propD("d5", "virtio", 8, 5, 512, ""),
			},
			Steps: []propStep{
				{Attach: propAll("d0", "d1", "d2", "d3", "d4"), Expect: "ok", Why: "blank disks",
					Layout: func() propLayout {
						l := propDataLayout("d0", "raid", 1, "d1", "d2")
						l.Disks = append(l.Disks, propDiskLayout{Disk: "d3"}, propDiskLayout{Disk: "d4"})
						l.Arrays = append(l.Arrays, propArray{Name: "mddata2", Type: "raid", Level: 1, FS: "ext4", Mount: "/data2",
							Members: []propMember{{Disk: "d3"}, {Disk: "d4"}}})
						return l
					}()},
				{Attach: propAll("d5", "d0", "d1", "d2", "d3", "d4"), PreScan: true, Reboot: true, Expect: "ok",
					Why:    "d5 is blank; mddata is preserved and its assembly released; mddata2 is a bystander outside every planned node",
					Layout: propDataLayout("d5", "preserve", 1, "d1", "d2")},
			},
		},
	}
}
