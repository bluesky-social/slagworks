package storage

import (
	"strings"
	"testing"
)

// espLayout is a single disk with a vfat ESP of the given size and an ext4
// root on the rest.
func espLayout(espSize string) Layout {
	return Layout{
		Devices: map[string]Device{
			"vda": {Type: "disk", PTable: "gpt", Boot: true, Partitions: []Partition{
				{Name: "vda1", FS: "vfat", Size: espSize, Bootable: true},
				{Name: "vda2", FS: "ext4", Size: "rest"},
			}},
		},
		Mounts: map[string]Mount{
			"/":         {Device: "vda2"},
			"/boot/efi": {Device: "vda1"},
		},
	}
}

// TestPlan_VFATPartitionBelowFAT32MinimumRejected is the regression for a
// 256M ESP on a 4Kn disk: mkfs.fat -F 32 formatted it with fewer clusters
// than FAT32 allows, said so only in a warning, and exited 0, so the deploy
// completed and the UEFI firmware could not read the ESP. The same layout on
// a 512-byte-sector disk boots. Plan must refuse a vfat partition below the
// FAT32 minimum for its disk's logical sector size, before any command runs,
// and accept one at the minimum. The minimums (33M and 257M) are the
// smallest whole-MiB sizes that dosfstools 4.2 formats without the warning:
// 32M and 256M warn.
func TestPlan_VFATPartitionBelowFAT32MinimumRejected(t *testing.T) {
	cases := []struct {
		sector  int64
		size    string
		wantMin string // "" when the size is accepted
	}{
		{512, "32M", "33M"},
		{512, "33788K", "33M"}, // 33 MiB less 4K
		{512, "33M", ""},
		{512, "34M", ""},
		{512, "256M", ""},
		{4096, "256M", "257M"},
		{4096, "263164K", "257M"}, // 257 MiB less 4K
		{4096, "257M", ""},
		{4096, "258M", ""},
		{4096, "512M", ""},
	}
	for _, c := range cases {
		inv := []BlockDevice{{Name: "vda", SizeBytes: 8 << 30, LogicalSectorBytes: c.sector}}
		cmds, err := Plan(espLayout(c.size), inv)
		if c.wantMin == "" {
			if err != nil {
				t.Errorf("%d-byte sectors, ESP %s: Plan: %v", c.sector, c.size, err)
				continue
			}
			assertCommandContains(t, cmds, Command{"mkfs.fat", []string{"-F", "32", "/dev/vda1"}})
			continue
		}
		if err == nil {
			t.Errorf("%d-byte sectors, ESP %s: Plan accepted a partition below the FAT32 minimum", c.sector, c.size)
			continue
		}
		if cmds != nil {
			t.Errorf("%d-byte sectors, ESP %s: Plan returned commands with its error", c.sector, c.size)
		}
		for _, want := range []string{"vda1", "at least " + c.wantMin, "FAT32"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%d-byte sectors, ESP %s: error %q does not mention %q", c.sector, c.size, err, want)
			}
		}
	}
}

// TestPlan_VFATRestPartitionBelowFAT32MinimumRejected covers a vfat
// partition sized "rest": its size is what the disk leaves after the other
// partitions, the 1 MiB sgdisk keeps before the first partition, and the
// backup GPT at the end.
func TestPlan_VFATRestPartitionBelowFAT32MinimumRejected(t *testing.T) {
	layout := func(rootSize string) Layout {
		return Layout{
			Devices: map[string]Device{
				"vda": {Type: "disk", PTable: "gpt", Boot: true, Partitions: []Partition{
					{Name: "vda1", Size: "1M", BIOSGrub: true},
					{Name: "vda2", FS: "ext4", Size: rootSize},
					{Name: "vda3", FS: "vfat", Size: "rest", Bootable: true},
				}},
			},
			Mounts: map[string]Mount{
				"/":         {Device: "vda2"},
				"/boot/efi": {Device: "vda3"},
			},
		}
	}
	inv := []BlockDevice{{Name: "vda", SizeBytes: 4 << 30, LogicalSectorBytes: 4096}}

	// 4096M - 1M lead-in - 1M bios_grub - 3836M root leaves 258M, less the
	// backup GPT: at least 257M.
	if _, err := Plan(layout("3836M"), inv); err != nil {
		t.Errorf("rest partition of about 258M: Plan: %v", err)
	}
	// 3838M of root leaves 256M, less the backup GPT.
	_, err := Plan(layout("3838M"), inv)
	if err == nil {
		t.Fatal("Plan accepted a rest-sized vfat partition below the FAT32 minimum")
	}
	for _, want := range []string{"vda3", "rest", "at least 257M"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPlan_VFATNeedsLogicalSectorSize: the FAT32 minimum depends on the
// sector size, so Plan refuses to guess one for a disk with a vfat partition.
func TestPlan_VFATNeedsLogicalSectorSize(t *testing.T) {
	_, err := Plan(espLayout("512M"), []BlockDevice{{Name: "vda", SizeBytes: 8 << 30}})
	if err == nil {
		t.Fatal("Plan accepted a vfat partition on a disk with no logical sector size in the inventory")
	}
	if !strings.Contains(err.Error(), "logical sector size") {
		t.Errorf("error %q does not name the missing logical sector size", err)
	}
}

// TestSizeBytes: a fixed size is the same number of bytes on every disk,
// because sizeBytes accepts only what sgdisk realizes exactly at both
// 512-byte and 4096-byte sectors.
func TestSizeBytes(t *testing.T) {
	cases := []struct {
		size string
		want int64
	}{
		{"512M", 512 << 20},
		{"512m", 512 << 20},
		{"1G", 1 << 30},
		{"2T", 2 << 40},
		{"64K", 64 << 10},
		{"4K", 4 << 10},
	}
	for _, c := range cases {
		got, err := sizeBytes(c.size)
		if err != nil || got != c.want {
			t.Errorf("sizeBytes(%q) = %d, %v; want %d", c.size, got, err, c.want)
		}
	}
	// A bare number is a sector count to sgdisk, so its size would depend on
	// the disk; the layout grammar requires a unit. Under 4K is no sectors
	// at all on a 4Kn disk, and 6K is rounded down to 4K there.
	for _, bad := range []string{"", "M", "2048", "0M", "3K", "6K", "1.5G", "-1G", "+1G", "512MB", "10X", "1P", "9999999999T"} {
		if _, err := sizeBytes(bad); err == nil {
			t.Errorf("sizeBytes(%q) accepted a size outside the layout grammar", bad)
		}
	}
}
