package storage

import (
	"fmt"
	"sort"
	"strings"
)

// Command represents a single shell command to execute.
type Command struct {
	Name string
	Args []string
}

func (c Command) String() string {
	return c.Name + " " + strings.Join(c.Args, " ")
}

// BlockDevice describes a physical block device from the system inventory.
type BlockDevice struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	// LogicalSectorBytes is the disk's logical sector size (lsblk LOG-SEC):
	// 512 on most disks, 4096 on 4Kn disks. Plan needs it for every disk
	// carrying a vfat partition, because the smallest FAT32 filesystem
	// depends on it (see fat32MinBytes). Zero means the inventory did not
	// report one.
	LogicalSectorBytes int64  `json:"logical_sector_bytes,omitempty"`
	Transport          string `json:"transport,omitempty"`
	Model              string `json:"model,omitempty"`
}

// Plan translates a storage layout into an ordered list of commands.
// The function is pure: same inputs always produce the same outputs.
// It validates the layout and resolves device references but has no side effects.
func Plan(layout Layout, inventory []BlockDevice) ([]Command, error) {
	if err := validate(layout); err != nil {
		return nil, err
	}

	inventoryByName := make(map[string]BlockDevice, len(inventory))
	for _, bd := range inventory {
		inventoryByName[bd.Name] = bd
	}

	// Process devices in deterministic order: disks first (sorted), then
	// raids (sorted), then preserves (sorted). Retained disks are verified
	// against inventory but intentionally produce no commands.
	diskNames, retainNames, raidNames, preserveNames := classifyDevices(layout)

	// Verify all referenced disks exist in inventory.
	for _, name := range diskNames {
		if _, ok := inventoryByName[name]; !ok {
			return nil, fmt.Errorf("disk %q in layout not found in inventory", name)
		}
	}
	for _, name := range retainNames {
		if _, ok := inventoryByName[name]; !ok {
			return nil, fmt.Errorf("retained disk %q in layout not found in inventory", name)
		}
	}

	var cmds []Command

	// Phase 1: Partition disks.
	for _, name := range diskNames {
		dev := layout.Devices[name]
		if err := checkVFATSizes(name, dev, inventoryByName[name]); err != nil {
			return nil, err
		}
		diskCmds, err := planDisk(name, dev)
		if err != nil {
			return nil, fmt.Errorf("planning disk %s: %w", name, err)
		}
		cmds = append(cmds, diskCmds...)
	}

	// Phase 2: Create RAID arrays.
	mdDevMap := buildMDDevMap(layout)
	for _, name := range raidNames {
		dev := layout.Devices[name]
		cmds = append(cmds, planRAID(mdDevMap[name], dev)...)
	}

	// Phase 2b: Assemble preserve arrays (not created — they already
	// exist on disk). Ordered after raid-create so a fresh mdN numbering
	// scheme is stable regardless of which class claims a given index.
	for _, name := range preserveNames {
		dev := layout.Devices[name]
		cmds = append(cmds, planPreserve(mdDevMap[name], dev)...)
	}

	// Phase 3: Format devices that have fs set (disks already formatted their partitions).
	// RAID arrays with fs are formatted here. A RAID array without fs is
	// cleared instead: mdadm --create over the same members puts the data
	// area where the previous array's was, so that array's filesystem or
	// LVM label would show through on the new one. Only Linux reads an md
	// array's data area, so wipefs alone is enough here; partitions also
	// zero their boot sector because firmware reads those. Preserve
	// devices are never formatted or cleared — that's the entire point.
	for _, name := range raidNames {
		dev := layout.Devices[name]
		mdDev := mdDevMap[name]
		if dev.FS == "" {
			cmds = append(cmds, Command{"wipefs", []string{"-a", mdDev}})
			continue
		}
		fmtCmd, err := mkfsCommand(dev.FS, mdDev)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, fmtCmd)
	}

	// Phase 4: Mount in correct order (/ first, then deeper paths).
	cmds = append(cmds, planMounts(layout.Mounts, mdDevMap)...)

	return cmds, nil
}

// buildMDDevMap assigns /dev/mdN names to RAID and preserve devices,
// combined into one sorted sequence, so a layout with one of each doesn't
// collide on /dev/md0.
func buildMDDevMap(layout Layout) map[string]string {
	_, _, raidNames, preserveNames := classifyDevices(layout)
	names := make([]string, 0, len(raidNames)+len(preserveNames))
	names = append(names, raidNames...)
	names = append(names, preserveNames...)
	sort.Strings(names)

	m := make(map[string]string, len(names))
	for i, name := range names {
		m[name] = fmt.Sprintf("/dev/md%d", i)
	}
	return m
}

// MDDevMap returns the device-name -> /dev/mdN mapping the plan assigns to
// every raid and preserve device. Agent-runtime code (preserve preflight
// and post-assembly verification) needs to address an assembled array by
// the same node the plan mounts it on, so this exposes the same numbering
// buildMDDevMap uses internally.
func MDDevMap(layout Layout) map[string]string {
	return buildMDDevMap(layout)
}

// DeclaredFS returns the filesystem type the layout declares for the named
// mount device: a raid/preserve device's own FS, or a disk partition's FS.
// Returns "" when nothing is declared. Used as a cross-check against
// blkid's authoritative detected type when writing fstab -- a mismatch on
// a preserve device means the layout describes an array other than the one
// actually on disk.
func DeclaredFS(layout Layout, name string) string {
	if dev, ok := layout.Devices[name]; ok {
		return dev.FS
	}
	for _, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		for _, p := range dev.Partitions {
			if p.Name == name {
				return p.FS
			}
		}
	}
	return ""
}

// ResolveMountDevices returns a map from mount point to resolved /dev/... path.
// Useful for callers that need the device path for each mount point after
// Plan commands have been executed (e.g., to run blkid for UUIDs).
func ResolveMountDevices(layout Layout) map[string]string {
	mdDevMap := buildMDDevMap(layout)
	result := make(map[string]string, len(layout.Mounts))
	for path, m := range layout.Mounts {
		if md, ok := mdDevMap[m.Device]; ok {
			result[path] = md
		} else {
			result[path] = resolveDevPath(m.Device)
		}
	}
	return result
}

// BIOSGrubDisks returns the sorted /dev/ paths of every disk carrying a
// bios_grub (EF02) partition. The legacy bootloader path installs GRUB
// onto each one — installing on all of them keeps a RAID1 root bootable
// if either member disk is lost. Empty when the layout has no bios_grub
// partition (i.e. it's a UEFI/ESP layout).
func BIOSGrubDisks(layout Layout) []string {
	var disks []string
	for name, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		for _, p := range dev.Partitions {
			if p.BIOSGrub {
				disks = append(disks, "/dev/"+name)
				break
			}
		}
	}
	sort.Strings(disks)
	return disks
}

// classifyDevices returns sorted lists of disk, retained-disk, RAID, and
// preserve device names.
func classifyDevices(layout Layout) (disks, retains, raids, preserves []string) {
	for name, dev := range layout.Devices {
		switch dev.Type {
		case "disk":
			disks = append(disks, name)
		case "retain":
			retains = append(retains, name)
		case "raid":
			raids = append(raids, name)
		case "preserve":
			preserves = append(preserves, name)
		}
	}
	sort.Strings(disks)
	sort.Strings(retains)
	sort.Strings(raids)
	sort.Strings(preserves)
	return
}

func planDisk(name string, dev Device) ([]Command, error) {
	diskPath := "/dev/" + name
	var cmds []Command

	// Wipe existing partition table.
	cmds = append(cmds, Command{"sgdisk", []string{"--zap-all", diskPath}})

	if len(dev.Partitions) == 0 {
		return cmds, nil
	}

	// Create partitions.
	for i, part := range dev.Partitions {
		num := i + 1
		sizeArg := partitionSizeArg(part.Size)
		typeCode := partitionTypeCode(part)

		cmds = append(cmds, Command{"sgdisk", []string{
			"-n", fmt.Sprintf("%d:0:%s", num, sizeArg),
			"-t", fmt.Sprintf("%d:%s", num, typeCode),
			"-c", fmt.Sprintf("%d:%s", num, part.Name),
			diskPath,
		}})
	}

	// Re-read partition table. Use blockdev (util-linux, always available)
	// instead of partprobe (parted package, not in minimal cloud images).
	//
	// This races systemd-udevd: each sgdisk above fires a partition-table-
	// change uevent and udev opens the disk to probe the new partitions; if
	// BLKRRPART runs while udev still holds that open it fails EBUSY ("Device
	// or resource busy") and the deploy aborts in the storage phase (observed
	// on the Dell R640 oob-gws). We can't `udevadm settle` it away —
	// the agent runs inside a chroot where settle is a no-op ("Running in
	// chroot, ignoring request") — so the agent's executor RETRIES this exact
	// command with a short backoff (see runStorageCmd). The command itself is
	// unchanged; the resilience lives in the executor so the plan stays a pure
	// declarative list.
	cmds = append(cmds, Command{"blockdev", []string{"--rereadpt", diskPath}})

	// Format each partition that declares a filesystem, and clear every
	// other one. A new partition starts with whatever bytes sat at its
	// offset; sgdisk --zap-all rewrites only the partition tables, and the
	// agent's --destructive wipe clears whole-disk and md signatures only.
	// mkfs overwrites a formatted partition, but an unformatted one (a spare
	// ESP slot, a BIOS boot partition, a RAID member) would keep the
	// filesystem an earlier layout put there: an old ESP left on the
	// firmware's first disk boots the old bootloader, which asks for a root
	// filesystem that no longer exists.
	//
	// Clearing takes two commands. wipefs -a erases the filesystem, RAID and
	// LVM signatures libblkid knows, at the head or the tail (an md 1.0
	// superblock lives at the end), which is what Linux and mdadm probe. It
	// erases only the magic bytes, though, and UEFI firmware mounts FAT from
	// the boot sector's BPB without checking them: after wipefs alone, a
	// redeployed VM still booted the old ESP. Zeroing the first 4 KiB,
	// logical sector 0 at any sector size, removes the boot sector itself.
	// It also removes the MBR signature and GPT protective MBR of a nested
	// partition table, which wipefs leaves alone on a partition without
	// --force; libblkid, the kernel and firmware read no GPT without one.
	// --force is not used because it also drops wipefs's O_EXCL open, so a
	// partition something still holds would be written instead of failing
	// the deploy. Both run after the re-read, so the partition nodes exist,
	// and before Phase 2's mdadm uses any member.
	for i, part := range dev.Partitions {
		partPath := partitionDevPath(name, i+1)
		if part.FS == "" {
			cmds = append(cmds,
				Command{"wipefs", []string{"-a", partPath}},
				Command{"dd", []string{"if=/dev/zero", "of=" + partPath, "bs=4096", "count=1", "conv=fsync"}},
			)
			continue
		}
		fmtCmd, err := mkfsCommand(part.FS, partPath)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, fmtCmd)
	}

	return cmds, nil
}

func planRAID(mdDev string, dev Device) []Command {
	args := []string{
		"--create", mdDev,
		fmt.Sprintf("--level=%d", dev.Level),
		fmt.Sprintf("--raid-devices=%d", len(dev.Members)),
		"--metadata=1.2",
	}
	if dev.ArrayUUID != "" {
		args = append(args, "--uuid", dev.ArrayUUID)
	}
	args = append(args, "--run")
	for _, member := range dev.Members {
		args = append(args, resolveDevPath(member))
	}

	return []Command{{"mdadm", args}}
}

// planPreserve assembles (never creates) an existing md array by its
// expected array UUID. No mkfs — a preserve device already holds data
// that must survive the deploy. Runtime identity verification (mdadm
// --detail / blkid against dev.ArrayUUID / dev.FSUUID) is Phase 2
// (deploy.go); Plan only emits the assemble command itself.
func planPreserve(mdDev string, dev Device) []Command {
	args := []string{"--assemble", mdDev, "--uuid", dev.ArrayUUID}
	for _, member := range dev.Members {
		args = append(args, resolveDevPath(member))
	}
	return []Command{{"mdadm", args}}
}

func planMounts(mounts map[string]Mount, mdDevMap map[string]string) []Command {
	// Sort mount points by depth so / comes before /boot/efi.
	paths := make([]string, 0, len(mounts))
	for path := range mounts {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		di := strings.Count(paths[i], "/")
		dj := strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})

	var cmds []Command
	for _, path := range paths {
		m := mounts[path]
		devPath := resolveDevPath(m.Device)
		// Check if this is a RAID or preserve device name.
		if md, ok := mdDevMap[m.Device]; ok {
			devPath = md
		}
		target := "/target" + path
		if target != "/target" {
			target = strings.TrimRight(target, "/")
		}
		cmds = append(cmds, Command{"mkdir", []string{"-p", target}})
		cmds = append(cmds, Command{"mount", []string{devPath, target}})
	}
	return cmds
}

// resolveDevPath converts a partition/disk name to a /dev/ path.
// Handles both whole-disk names (nvme0n1) and partition names (nvme0n1p2, sda1).
func resolveDevPath(name string) string {
	if strings.HasPrefix(name, "/dev/") {
		return name
	}
	return "/dev/" + name
}

// partName returns the partition name for partition num on disk.
// Devices ending in a digit need a 'p' separator (nvme0n1p1, mmcblk0p1).
func partName(disk string, num int) string {
	sep := ""
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		sep = "p"
	}
	return fmt.Sprintf("%s%s%d", disk, sep, num)
}

func partitionDevPath(disk string, num int) string {
	return "/dev/" + partName(disk, num)
}

func partitionSizeArg(size string) string {
	if size == "rest" {
		return "0" // sgdisk: 0 means fill remaining
	}
	return "+" + size
}

func partitionTypeCode(part Partition) string {
	if part.BIOSGrub {
		return "EF02" // BIOS boot partition (holds GRUB core.img on GPT)
	}
	if part.Bootable {
		return "EF00" // EFI System Partition
	}
	switch part.FS {
	case "swap":
		return "8200"
	default:
		return "8300" // Linux filesystem
	}
}

func mkfsCommand(fs, dev string) (Command, error) {
	switch fs {
	case "vfat":
		return Command{"mkfs.fat", []string{"-F", "32", dev}}, nil
	case "ext4":
		return Command{"mkfs.ext4", []string{"-F", dev}}, nil
	case "xfs":
		return Command{"mkfs.xfs", []string{"-f", dev}}, nil
	case "swap":
		return Command{"mkswap", []string{dev}}, nil
	default:
		return Command{}, fmt.Errorf("unsupported filesystem: %q", fs)
	}
}
