package storage

import (
	"fmt"
	"strings"
)

// validate checks a layout for internal consistency before planning.
func validate(layout Layout) error {
	if len(layout.Devices) == 0 {
		return fmt.Errorf("layout has no devices")
	}

	hasDisks := false
	for name, dev := range layout.Devices {
		switch dev.Type {
		case "disk":
			hasDisks = true
			if err := validateDisk(name, dev); err != nil {
				return err
			}
		case "retain":
			if err := validateRetain(name, dev); err != nil {
				return err
			}
		case "raid":
			if err := validateRAID(name, dev, layout); err != nil {
				return err
			}
		case "preserve":
			if err := validatePreserve(name, dev, layout); err != nil {
				return err
			}
		default:
			return fmt.Errorf("device %q: unknown type %q", name, dev.Type)
		}
	}

	if !hasDisks {
		return fmt.Errorf("layout has no disk devices")
	}

	// A preserve device's members must never also be disk targets (they'd
	// be sgdisk --zap-all'd) or members of a raid device (they'd be
	// mdadm --create'd, which destroys them). This is the single
	// highest-value static guard in the whole preserve feature: it's the
	// difference between "assemble existing data" and "recreate and lose
	// it" and it must be caught before Plan ever emits a destructive
	// command.
	if err := validatePreserveMemberCollisions(layout); err != nil {
		return err
	}
	if err := validateRetainMemberCollisions(layout); err != nil {
		return err
	}

	// Verify all mount devices exist in the layout and are mountable.
	// A disk partition referenced by a mount must declare a filesystem:
	// the planner only mkfs'es partitions with FS set but emits a mount
	// unconditionally, so mounting an FS-less partition (a bios_grub EF02
	// embedding partition, or any partition simply omitted from fs
	// config) would mount raw, unformatted bytes. RAID-member partitions
	// legitimately have no FS but are never mounted directly (the md
	// device is), so they don't hit this path.
	for path, m := range layout.Mounts {
		if !deviceExists(m.Device, layout) {
			return fmt.Errorf("mount %q references unknown device %q", path, m.Device)
		}
		if dev, ok := layout.Devices[m.Device]; ok {
			switch dev.Type {
			case "disk":
				// A whole disk is never a mountable filesystem: the
				// planner partitions disks and never lays a disk-level
				// filesystem.
				return fmt.Errorf("mount %q references whole disk %q (not a mountable filesystem)", path, m.Device)
			case "raid":
				// RAID arrays ARE mountable, but only if fs is set —
				// Phase 3 mkfs'es a RAID device only when dev.FS != "",
				// while planMounts mounts it unconditionally.
				if dev.FS == "" {
					return fmt.Errorf("mount %q references RAID device %q which has no filesystem", path, m.Device)
				}
			case "preserve":
				// Preserve arrays are mountable exactly like raid, but
				// since they're never mkfs'ed, fs is the only source of
				// truth for what's already on disk — require it declared
				// so fstab/mount have a type to use.
				if dev.FS == "" {
					return fmt.Errorf("mount %q references preserve device %q which has no filesystem declared", path, m.Device)
				}
			case "retain":
				return fmt.Errorf("mount %q references retained disk %q (retained disks cannot be mounted by the installer)", path, m.Device)
			}
		}
		if p, ok := diskPartition(m.Device, layout); ok {
			if p.BIOSGrub {
				return fmt.Errorf("mount %q references bios_grub partition %q (not a mountable filesystem)", path, m.Device)
			}
			if p.FS == "" {
				return fmt.Errorf("mount %q references partition %q which has no filesystem", path, m.Device)
			}
		}
	}

	// Verify "/" mount exists.
	if _, ok := layout.Mounts["/"]; !ok {
		return fmt.Errorf("layout has no root (/) mount")
	}

	return nil
}

func validateRetain(name string, dev Device) error {
	if dev.PTable != "" || dev.Boot || len(dev.Partitions) != 0 || dev.Level != 0 ||
		len(dev.Members) != 0 || dev.FS != "" || dev.ArrayUUID != "" || dev.FSUUID != "" {
		return fmt.Errorf("retained disk %q: type retain accepts no other device fields", name)
	}
	return nil
}

// A retained whole disk is an explicit no-write boundary. Referencing it from
// a RAID or preserve device would let array handling bypass that boundary.
func validateRetainMemberCollisions(layout Layout) error {
	for retainedName, retained := range layout.Devices {
		if retained.Type != "retain" {
			continue
		}
		for arrayName, array := range layout.Devices {
			if array.Type != "raid" && array.Type != "preserve" {
				continue
			}
			for _, member := range array.Members {
				if memberReferencesDisk(member, retainedName) {
					return fmt.Errorf(
						"retained disk %q is also a member of %s device %q",
						retainedName, array.Type, arrayName,
					)
				}
			}
		}
	}
	return nil
}

// memberReferencesDisk reports whether an array member names a whole disk or
// one of its partitions. It understands the Linux pN separator used by disk
// families whose whole-disk name ends in a digit (nvme0n1p2, mmcblk0p1,
// dm-0p1) and the bare numeric suffix used by sd/vd/xvd disks (sda2, vdb3).
// A generic prefix check is unsafe: nvme1n10 is not on nvme1n1.
func memberReferencesDisk(member, disk string) bool {
	member = stripDevPrefix(member)
	disk = stripDevPrefix(disk)
	if disk == "" {
		return false
	}
	if member == disk {
		return true
	}
	if !strings.HasPrefix(member, disk) {
		return false
	}
	suffix := strings.TrimPrefix(member, disk)
	if suffix == "" {
		return true
	}
	last := disk[len(disk)-1]
	if last >= '0' && last <= '9' {
		if suffix[0] != 'p' {
			return false
		}
		suffix = suffix[1:]
	}
	if suffix == "" {
		return false
	}
	for _, char := range suffix {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validateDisk(name string, dev Device) error {
	// Disks with no partitions are valid — they may be whole-disk RAID members.
	restCount := 0
	for i, p := range dev.Partitions {
		if p.Name == "" {
			return fmt.Errorf("disk %q: partition missing name", name)
		}
		// planDisk creates and formats partition i+1 at the node the kernel
		// gives that position, while mounts, RAID members, fstab and the
		// bootloader reach the partition through "/dev/" + its name. Any
		// other name sends those to a different device than the one just
		// formatted. A "/dev/"-prefixed name is the same node: resolveDevPath
		// passes it through unchanged.
		if want := partName(name, i+1); stripDevPrefix(p.Name) != want {
			return fmt.Errorf("disk %q partition %d is named %q, but the kernel names it %q; "+
				"rename it %q, since mounts and RAID members find a partition by its name",
				name, i+1, p.Name, want, want)
		}
		if p.Size == "" {
			return fmt.Errorf("disk %q partition %q: missing size", name, p.Name)
		}
		if p.Size == "rest" {
			restCount++
		}
		if p.BIOSGrub && (p.FS != "" || p.Bootable) {
			return fmt.Errorf("disk %q partition %q: bios_grub is mutually exclusive with fs/bootable", name, p.Name)
		}
	}
	if restCount > 1 {
		return fmt.Errorf("disk %q: multiple partitions with size \"rest\" (max 1)", name)
	}

	return nil
}

// raidLevels is every md level a layout may request, with the minimum
// member count validateRAID enforces and the kernel module that provides
// the level's md personality. The kernel loads that module by running
// modprobe in the deploy initramfs, not in the agent's chroot, so each one
// must be baked into the deploy initrd by assets/initramfs/hooks/slagworks
// (raid_modules_test.go holds the two lists together). Without it
// `mdadm --create` fails "RUN_ARRAY failed: Invalid argument" after the
// disks have already been repartitioned.
var raidLevels = map[int]struct {
	minMembers int
	module     string
}{
	0:  {minMembers: 2, module: "raid0"},
	1:  {minMembers: 2, module: "raid1"},
	5:  {minMembers: 3, module: "raid456"},
	6:  {minMembers: 4, module: "raid456"},
	10: {minMembers: 4, module: "raid10"},
}

// RAIDModule returns the kernel module that provides the md personality
// for a RAID level the planner accepts, and false for any other level.
func RAIDModule(level int) (string, bool) {
	l, ok := raidLevels[level]
	return l.module, ok
}

func validateRAID(name string, dev Device, layout Layout) error {
	l, known := raidLevels[dev.Level]
	if !known {
		return fmt.Errorf("raid %q: unsupported RAID level %d", name, dev.Level)
	}
	if len(dev.Members) < l.minMembers {
		return fmt.Errorf("raid %q: RAID %d requires at least %d members, got %d", name, dev.Level, l.minMembers, len(dev.Members))
	}

	// Verify all members reference partitions or disks in the layout.
	for _, member := range dev.Members {
		if !memberExists(member, layout) {
			return fmt.Errorf("raid %q: member %q not found in layout", name, member)
		}
	}

	return nil
}

func validatePreserve(name string, dev Device, layout Layout) error {
	if len(dev.Members) == 0 {
		return fmt.Errorf("preserve %q: at least one member is required", name)
	}
	if dev.ArrayUUID == "" {
		return fmt.Errorf("preserve %q: array_uuid is required (identity is mandatory for preserve devices)", name)
	}
	// Reject a malformed array_uuid at plan time rather than letting it
	// surface as a runtime `mdadm --assemble --uuid` error or a silent
	// never-matches in the assembled-identity verify. Accept any 32-hex-
	// nibble form (RFC-4122 dashed or mdadm colon-grouped); the agent
	// normalizes separators when comparing.
	if !isHexUUID(dev.ArrayUUID) {
		return fmt.Errorf("preserve %q: array_uuid %q is malformed (want 32 hex digits, RFC-4122 dashed or mdadm colon-grouped)", name, dev.ArrayUUID)
	}

	// Unlike raid members, preserve members are deliberately NOT required
	// to appear as layout devices — the whole point is that they are kept
	// OUT of the layout's disk/raid vocabulary so nothing zaps or
	// recreates them. Existence on the real host is a runtime (Phase 2)
	// concern; static validation can't see hardware.

	// A preserve device that's mounted needs a declared fs — it's never
	// mkfs'ed, so FS is the only source of truth for what type to mount.
	for _, m := range layout.Mounts {
		if m.Device == name && dev.FS == "" {
			return fmt.Errorf("preserve %q: mounted device requires fs to be set", name)
		}
	}

	return nil
}

// validatePreserveMemberCollisions ensures preserve members are never also
// disk targets (which would be zapped) or members of a raid device (which
// would be recreated). Either would silently destroy the data preserve
// exists to protect.
func validatePreserveMemberCollisions(layout Layout) error {
	// diskClaimed covers both whole-disk device names AND every partition
	// declared under a disk device: planDisk's sgdisk --zap-all wipes the
	// entire GPT, so a preserve member naming a partition on a managed
	// disk (e.g. "nvme0n1p3" on disk "nvme0n1") is exactly as destroyed
	// as one naming the whole disk. Keys and lookups are both normalized
	// via stripDevPrefix: resolveDevPath treats "nvme0n1" and
	// "/dev/nvme0n1" as the identical device when emitting commands, so
	// the collision guard must too, or a "/dev/"-prefixed member name
	// bypasses it.
	diskClaimed := make(map[string]bool)
	raidMembers := make(map[string]string) // member name -> owning raid device name
	for name, dev := range layout.Devices {
		switch dev.Type {
		case "disk":
			diskClaimed[stripDevPrefix(name)] = true
			for _, p := range dev.Partitions {
				diskClaimed[stripDevPrefix(p.Name)] = true
			}
		case "raid":
			for _, member := range dev.Members {
				raidMembers[stripDevPrefix(member)] = name
			}
		}
	}

	for name, dev := range layout.Devices {
		if dev.Type != "preserve" {
			continue
		}
		for _, member := range dev.Members {
			normalized := stripDevPrefix(member)
			if diskClaimed[normalized] {
				return fmt.Errorf("preserve %q: member %q is also a disk device or partition (would be zapped, defeating preserve)", name, member)
			}
			if owner, ok := raidMembers[normalized]; ok {
				return fmt.Errorf("preserve %q: member %q is also a member of raid %q (would be recreated, defeating preserve)", name, member, owner)
			}
		}
	}
	return nil
}

// stripDevPrefix strips a leading "/dev/" so a member name is comparable
// to the bare layout device/partition names, mirroring resolveDevPath's
// treatment of "/dev/"-prefixed and bare names as the same device.
func stripDevPrefix(name string) string {
	return strings.TrimPrefix(name, "/dev/")
}

// isHexUUID reports whether s is a well-formed 128-bit UUID: exactly 32 hex
// digits (case-insensitive) once separators are removed, with NO other
// characters. Accepts both RFC-4122 dashed and mdadm colon-grouped forms
// (the agent normalizes separators when comparing). A stray non-hex,
// non-separator character is rejected outright, so "30 hex + 2 junk" can
// never masquerade as 32 nibbles.
func isHexUUID(s string) bool {
	n := 0
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			n++
		case r == '-' || r == ':' || r == ' ':
			// separator, ignore
		default:
			return false // any other char disqualifies it
		}
	}
	return n == 32
}

// deviceExists checks if a name is a device or partition in the layout.
func deviceExists(name string, layout Layout) bool {
	if _, ok := layout.Devices[name]; ok {
		return true
	}
	return partitionExists(name, layout)
}

// partitionExists checks if a name matches a partition in any disk device.
func partitionExists(name string, layout Layout) bool {
	for _, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		for _, p := range dev.Partitions {
			if p.Name == name {
				return true
			}
		}
	}
	return false
}

// memberExists checks if a RAID member name is a partition or whole disk.
func memberExists(name string, layout Layout) bool {
	return deviceExists(name, layout)
}

// diskPartition returns the disk Partition matching name and true, or a
// zero Partition and false if name is not a disk partition (e.g. it's a
// RAID device or whole disk). Used by mount validation to inspect a
// mounted partition's fs/flags.
func diskPartition(name string, layout Layout) (Partition, bool) {
	// Device names take precedence over partition names (matching
	// deviceExists): a mount targeting a whole device (e.g. a RAID
	// array) is never a partition, even if some disk happens to have a
	// same-named partition.
	if _, ok := layout.Devices[name]; ok {
		return Partition{}, false
	}
	for _, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		for _, p := range dev.Partitions {
			if p.Name == name {
				return p, true
			}
		}
	}
	return Partition{}, false
}
