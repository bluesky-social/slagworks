package storage

import (
	"strings"
	"testing"
)

// sizeLayout is one disk, sda, carrying the given partitions, with sda1
// mounted at /. No partition is vfat, so the FAT32 minimum (which reads
// sizes too) is not what decides any case below.
func sizeLayout(sizes ...string) Layout {
	parts := make([]Partition, len(sizes))
	for i, size := range sizes {
		parts[i] = Partition{Name: partName("sda", i+1), Size: size, FS: "ext4"}
	}
	return Layout{
		Devices: map[string]Device{"sda": {Type: "disk", PTable: "gpt", Partitions: parts}},
		Mounts:  map[string]Mount{"/": {Device: "sda1"}},
	}
}

func commandLines(cmds []Command) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString("\n  " + c.String())
	}
	return b.String()
}

// TestPlan_RejectsMalformedPartitionSizes: planDisk hands a partition size
// to sgdisk as "-n N:0:+<size>", and sgdisk runs only after
// "sgdisk --zap-all" has erased the disk's partition table. Every size below
// is outside the documented grammar ("512M", "100G", or "rest"), and with
// sgdisk 1.0.10 each one either fails partway through partitioning (the old
// partition table already gone) or silently creates a partition of a
// different size than declared. Plan must refuse each before any command
// runs.
func TestPlan_RejectsMalformedPartitionSizes(t *testing.T) {
	inv := []BlockDevice{{Name: "sda", SizeBytes: 480103981056, LogicalSectorBytes: 4096}}
	cases := []struct {
		name   string
		layout Layout
		want   string // the error names the offending size or rule
	}{
		// sgdisk reads a bare number as sectors: 512 is a 256 KiB partition
		// on a 512-byte-sector disk and 2 MiB on a 4Kn one.
		{"no unit", sizeLayout("512", "rest"), `"512"`},
		// sgdisk exits 0 having created a 1 GiB partition.
		{"fractional", sizeLayout("1.5G", "rest"), `"1.5G"`},
		// sgdisk exits 4: "Could not create partition 1".
		{"zero", sizeLayout("0M", "rest"), `"0M"`},
		// sgdisk rounds a size down to whole sectors, so on this 4Kn disk
		// 2K is zero too.
		{"under one sector", sizeLayout("2K", "rest"), `"2K"`},
		// What (disk_bytes - 600 MiB) // MiB, the root size most shipped
		// scripts compute, gives on a disk under 600 MiB. sgdisk exits 4.
		{"negative", sizeLayout("512M", "-536M"), `"-536M"`},
		// "rest" takes the rest of the disk, so sgdisk exits 4 on the
		// partition after it.
		{"rest before a fixed partition", sizeLayout("rest", "100G"), "last"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds, err := Plan(c.layout, inv)
			if err == nil {
				t.Fatalf("Plan accepted a partition size outside the documented grammar:%s", commandLines(cmds))
			}
			if cmds != nil {
				t.Errorf("Plan returned commands with its error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %s", err, c.want)
			}
		})
	}
}

// TestPlan_RejectsPartitionsThatDoNotFit: sgdisk starts the first partition
// 1 MiB into the disk, starts each later one on the next 1 MiB boundary, and
// keeps the end of the disk for the backup GPT. A layout whose fixed sizes
// do not fit fails at the first partition past the end, after
// "sgdisk --zap-all". Plan has the disk's size from the inventory and must
// refuse such a layout before any command runs, while accepting one that
// fills the disk.
func TestPlan_RejectsPartitionsThatDoNotFit(t *testing.T) {
	const diskMiB = 10240 // a 10 GiB disk
	inv := []BlockDevice{{Name: "sda", SizeBytes: diskMiB * mib, LogicalSectorBytes: 512}}

	// 1 MiB before sda1, 1 MiB for the backup GPT, and at least 1 MiB for
	// a "rest" partition.
	for _, sizes := range [][]string{
		{"512M", "9726M"},
		{"1M", "10237M"},
		{"512M", "9725M", "rest"},
	} {
		if _, err := Plan(sizeLayout(sizes...), inv); err != nil {
			t.Errorf("layout %v that fills a %d MiB disk: Plan: %v", sizes, diskMiB, err)
		}
	}

	cases := []struct {
		name  string
		inv   []BlockDevice
		sizes []string
	}{
		// sda2 would end at the disk's last byte, over the backup GPT.
		{"one MiB over", inv, []string{"512M", "9727M"}},
		// Each partition starts on a 1 MiB boundary, so each 4K takes 1 MiB
		// and sda3 would end at the disk's last byte.
		{"alignment", inv, []string{"4K", "4K", "10237M"}},
		// sgdisk would give sda3 less than 1 MiB; a "rest" partition must
		// get at least 1 MiB.
		{"under 1 MiB left for rest", inv, []string{"512M", "9726M", "rest"}},
		{"600G on a 480 GB disk", []BlockDevice{{Name: "sda", SizeBytes: 480103981056}}, []string{"512M", "600G"}},
		{"disk of no size", []BlockDevice{{Name: "sda"}}, []string{"rest"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds, err := Plan(sizeLayout(c.sizes...), c.inv)
			if err == nil {
				t.Fatalf("Plan accepted partitions %v that do not fit the disk:%s", c.sizes, commandLines(cmds))
			}
			if cmds != nil {
				t.Errorf("Plan returned commands with its error")
			}
			if !strings.Contains(err.Error(), "does not fit") {
				t.Errorf("error %q does not say the partitions do not fit", err)
			}
		})
	}
}
