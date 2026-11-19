//go:build !integration

package storageapply

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// Property coverage for how the agent decides which whole disk a device
// name belongs to, which is what keeps --wipe-all-disks off the disks the
// layout uses or protects. The oracle is the Linux kernel's own partition
// naming rule (block/partitions/core.c: a disk whose name ends in a digit
// gets "p<N>", any other disk gets "<N>"), written here from scratch rather
// than through any production helper.

// kernelPartName is the kernel's partition-node naming rule.
func kernelPartName(disk string, n int) string {
	last := disk[len(disk)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

func genLetters(t *rapid.T, label string) string {
	n := rapid.IntRange(1, 2).Draw(t, label+"Len")
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(byte('a' + rapid.IntRange(0, 25).Draw(t, label+"Ch")))
	}
	return b.String()
}

// genParsedDiskName draws a whole-disk kernel name from the families
// parentDisk parses: the letter-terminated sd/vd/xvd/hd names and the
// digit-terminated nvme, mmcblk, loop, dm- and md names.
func genParsedDiskName() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		switch rapid.IntRange(0, 6).Draw(t, "family") {
		case 0, 1:
			return "sd" + genLetters(t, "sd")
		case 2:
			return rapid.SampledFrom([]string{"vd", "xvd", "hd"}).Draw(t, "pfx") + genLetters(t, "vd")
		case 3, 4:
			return fmt.Sprintf("nvme%dn%d", rapid.IntRange(0, 31).Draw(t, "ctrl"), rapid.IntRange(1, 16).Draw(t, "ns"))
		case 5:
			return fmt.Sprintf("mmcblk%d", rapid.IntRange(0, 9).Draw(t, "mmc"))
		default:
			return fmt.Sprintf("%s%d", rapid.SampledFrom([]string{"loop", "dm-", "md"}).Draw(t, "special"), rapid.IntRange(0, 127).Draw(t, "specialN"))
		}
	})
}

// genHostDiskName draws a name lsblk can report with TYPE "disk". The
// common families dominate, but every other digit-terminated block driver a
// server can expose is in the pool too: pmem (NVDIMM fsdax), nbd, rbd, zd
// (zvol), nullb, drbd, bcache. parentDisk does not parse those.
func genHostDiskName() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		switch rapid.IntRange(0, 7).Draw(t, "family") {
		case 0, 1:
			return "sd" + genLetters(t, "sd")
		case 2:
			return rapid.SampledFrom([]string{"vd", "xvd", "hd"}).Draw(t, "pfx") + genLetters(t, "vd")
		case 3, 4:
			return fmt.Sprintf("nvme%dn%d", rapid.IntRange(0, 31).Draw(t, "ctrl"), rapid.IntRange(1, 16).Draw(t, "ns"))
		case 5:
			return fmt.Sprintf("mmcblk%d", rapid.IntRange(0, 9).Draw(t, "mmc"))
		default:
			return fmt.Sprintf("%s%d", rapid.SampledFrom([]string{"pmem", "nbd", "rbd", "zd", "nullb", "drbd", "bcache"}).Draw(t, "other"), rapid.IntRange(0, 15).Draw(t, "otherN"))
		}
	})
}

// TestParentDisk_Property_InvertsKernelNaming: for every whole-disk kernel
// name d of a family parentDisk parses and every partition number n,
// parentDisk(d) == d and parentDisk(<partition n of d>) == d, with or
// without a /dev/ prefix.
func TestParentDisk_Property_InvertsKernelNaming(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		d := genParsedDiskName().Draw(rt, "disk")
		n := rapid.IntRange(1, 128).Draw(rt, "part")
		p := kernelPartName(d, n)
		for _, in := range []string{d, "/dev/" + d} {
			if got := parentDisk(in); got != d {
				rt.Fatalf("parentDisk(%q) = %q, want %q (whole disk)", in, got, d)
			}
		}
		for _, in := range []string{p, "/dev/" + p} {
			if got := parentDisk(in); got != d {
				rt.Fatalf("parentDisk(%q) = %q, want %q (partition %d of %s)", in, got, d, n, d)
			}
		}
	})
}

// TestBystanderDisks_Property_ExactlyTheDisksOutsideTheLayout: on a host
// whose disks carry any kernel name, the disks --wipe-all-disks selects are
// exactly those the layout neither partitions, retains, uses as a RAID
// member nor holds a preserve member on, however the preserve member is
// spelled (kernel name, /dev/ path, /dev/disk/by-id link; whole disk or
// partition).
func TestBystanderDisks_Property_ExactlyTheDisksOutsideTheLayout(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SliceOfNDistinct(genHostDiskName(), 1, 7, func(s string) string { return s }).Draw(rt, "disks")
		h := newFakeHost()
		devices := map[string]storage.Device{}
		var raidMembers, preserveMembers, want []string
		for i, n := range names {
			h.addDisk(n, 480103981056, "")
			role := rapid.SampledFrom([]string{"target", "bare", "retain", "preserve", "bystander", "bystander"}).Draw(rt, "role")
			switch role {
			case "target":
				p1 := kernelPartName(n, 1)
				devices[n] = storage.Device{Type: "disk", Partitions: []storage.Partition{{Name: p1, Size: "rest"}}}
				raidMembers = append(raidMembers, p1)
			case "bare":
				devices[n] = storage.Device{Type: "disk"}
				raidMembers = append(raidMembers, n)
			case "retain":
				devices[n] = storage.Device{Type: "retain"}
			case "preserve":
				member, link := n, fmt.Sprintf("/dev/disk/by-id/wwn-0x%016x", i)
				h.links[link] = n
				if rapid.Bool().Draw(rt, "partitionMember") {
					pn := rapid.IntRange(1, 9).Draw(rt, "pn")
					h.addPartition(n, pn, "md:"+preserveUUID)
					member, link = kernelPartName(n, pn), fmt.Sprintf("%s-part%d", link, pn)
					h.links[link] = member
				}
				switch rapid.IntRange(0, 2).Draw(rt, "spelling") {
				case 1:
					member = "/dev/" + member
				case 2:
					member = link
				}
				preserveMembers = append(preserveMembers, member)
			default:
				want = append(want, n)
			}
		}
		if len(raidMembers) > 0 {
			devices["mdnew"] = storage.Device{Type: "raid", Level: 1, Members: raidMembers}
		}
		if len(preserveMembers) > 0 {
			devices["mdkeep"] = storage.Device{Type: "preserve", Members: preserveMembers, ArrayUUID: preserveUUID, FS: "xfs"}
		}
		layout := storage.Layout{Devices: devices}

		got, err := bystanderDisks(quietCtx(h), layout)
		if err != nil {
			rt.Fatalf("bystanderDisks: %v", err)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			rt.Fatalf("bystander disks %v, want %v\nlayout: %s", got, want, mustJSONHost(layout))
		}
	})
}
