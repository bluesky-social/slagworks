//go:build !integration

package storageapply

import (
	"reflect"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// TestParseBystanderDisks pins the disk-selection logic behind
// --wipe-all-disks: every whole disk lsblk reports that the layout
// does not claim is a bystander; layout targets, partitions, and
// non-disk devices are not.
func TestParseBystanderDisks(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"nvme1n1": {Type: "disk"},
		"md0":     {Type: "raid1"},
	}}

	// kworker shape: two boot NVMe (layout targets) + two U.2 data
	// disks (bystanders), plus rows lsblk emits that must be ignored.
	lsblkOut := `nvme0n1 disk
nvme1n1 disk
nvme2n1 disk
nvme3n1 disk
md0 raid1
loop0 loop
sr0 rom
`
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme2n1", "nvme3n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

func TestParseBystanderDisks_AllClaimed(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"sda": {Type: "disk"},
	}}
	if got := parseBystanderDisks("sda disk\n", layout); got != nil {
		t.Errorf("expected no bystanders, got %v", got)
	}
}

func TestParseBystanderDisks_RetainedDiskIsProtected(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"nvme1n1": {Type: "retain"},
	}}
	lsblkOut := "nvme0n1 disk\nnvme1n1 disk\nnvme2n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme2n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

func TestParseBystanderDisks_MalformedLinesIgnored(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{}}
	out := "\n  \nnvme0n1\nnvme1n1 disk extra-field\nsda disk\n"
	got := parseBystanderDisks(out, layout)
	want := []string{"sda"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

// TestParseBystanderDisks_PreserveWholeDiskMember is the regression case for
// the preserve-member wipe bug: a preserve device's member is a whole disk,
// not a disk-type layout entry, so the old dev.Type == "disk" check missed
// it entirely and would have signature-wiped the protected data disk.
func TestParseBystanderDisks_PreserveWholeDiskMember(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"data":    {Type: "preserve", Members: []string{"nvme2n1"}},
	}}
	lsblkOut := "nvme0n1 disk\nnvme2n1 disk\nnvme3n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme3n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (nvme2n1 is a preserve member and must be excluded)", got, want)
	}
}

// TestParseBystanderDisks_PreservePartitionMember covers a preserve member
// that names a PARTITION of a disk that is not itself a layout device at
// all (not disk, raid, or preserve) -- the parent disk must still be
// excluded via parentDisk, not just the partition name.
func TestParseBystanderDisks_PreservePartitionMember(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"data": {Type: "preserve", Members: []string{"nvme0n1p2"}},
	}}
	lsblkOut := "nvme0n1 disk\nnvme1n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme1n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (nvme0n1 is the parent of preserve member nvme0n1p2)", got, want)
	}
}

// TestParseBystanderDisks_RaidMembersExcluded is a regression guard: today's
// code excludes raid members only incidentally (they're usually also
// referenced elsewhere). layoutClaimedDisks must exclude them explicitly.
func TestParseBystanderDisks_RaidMembersExcluded(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"md0": {Type: "raid", Members: []string{"nvme4n1", "nvme5n1"}},
	}}
	lsblkOut := "nvme4n1 disk\nnvme5n1 disk\nnvme6n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme6n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (raid members nvme4n1/nvme5n1 must be excluded)", got, want)
	}
}

// TestParseBystanderDisks_GenuineBystanderStillSelected confirms the fix
// doesn't over-exclude: a disk in neither the layout's disk entries nor any
// raid/preserve member set is still a genuine bystander.
func TestParseBystanderDisks_GenuineBystanderStillSelected(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"data":    {Type: "preserve", Members: []string{"nvme2n1"}},
	}}
	lsblkOut := "nvme0n1 disk\nnvme2n1 disk\nnvme9n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme9n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

// TestParseBystanderDisks_DevPrefixedMemberNormalizes confirms a "/dev/"-
// prefixed member excludes the same disk as its bare form.
func TestParseBystanderDisks_DevPrefixedMemberNormalizes(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"data": {Type: "preserve", Members: []string{"/dev/nvme2n1"}},
		"raid": {Type: "raid", Members: []string{"/dev/nvme0n1p2"}},
	}}
	lsblkOut := "nvme0n1 disk\nnvme2n1 disk\nnvme3n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layout)
	want := []string{"nvme3n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (/dev/-prefixed members must normalize like bare names)", got, want)
	}
}

// TestParentDisk pins the partition-name -> parent-whole-disk inversion of
// internal/storage/plan.go's partName, across both device-naming families.
func TestParentDisk(t *testing.T) {
	tests := []struct {
		name   string
		member string
		want   string
	}{
		{"nvme whole disk (namespace suffix, not a partition)", "nvme0n1", "nvme0n1"},
		{"nvme partition", "nvme0n1p2", "nvme0n1"},
		{"sd whole disk", "sda", "sda"},
		{"sd partition", "sda1", "sda"},
		{"sd partition, two-digit partition number", "sdb12", "sdb"},
		{"/dev/-prefixed whole disk", "/dev/nvme2n1", "nvme2n1"},
		{"/dev/-prefixed partition", "/dev/nvme0n1p1", "nvme0n1"},
		// mmcblk/loop/dm-/md whole disks end in a digit that is their OWN
		// identity, not a partition marker -- same family SplitDiskPart
		// (internal/agent/bootloader/bootloader.go) guards against. Without
		// the explicit whole-disk check, bareDigitSuffix's generic
		// "letters then digits" pattern would misparse these as partitions
		// of a shorter-named disk (e.g. "loop0" -> "loo").
		{"mmcblk whole disk", "mmcblk0", "mmcblk0"},
		{"mmcblk partition", "mmcblk0p1", "mmcblk0"},
		{"loop whole disk", "loop0", "loop0"},
		{"dm whole disk", "dm-0", "dm-0"},
		{"md whole disk", "md0", "md0"},
		// Regression (roast HIGH, confirmed 2026-07-16): an sd-family disk
		// name that happens to end in the letter "p" (sda..sdz, sdaa, ...,
		// sdp is a legitimate kernel name) must not be misparsed by a
		// generic ".+p\d+$" partition strip -- "sdp1" is partition 1 of
		// disk "sdp", not partition "p1" of disk "sd".
		{"sd disk ending in letter p, partitioned", "sdp1", "sdp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parentDisk(tt.member); got != tt.want {
				t.Errorf("parentDisk(%q) = %q, want %q", tt.member, got, tt.want)
			}
		})
	}
}

// TestParseMdstat covers the DDF/IMSM container vs member-subarray
// classification that drives stopResidualMDDevices: the Dell
// R640 oob-gws ship a factory DDF container that holds the boot disk and
// must be stopped (subarray before container) before partitioning.
func TestParseMdstat(t *testing.T) {
	// Real oob-gw-3.pop2 /proc/mdstat shape: md126 = DDF subarray (external
	// references the container), md127 = the ddf0 container itself.
	const real = `Personalities : [raid0] [raid1] [raid6] [raid5] [raid4] [raid10]
md126 : inactive sda[0]
      937164800 blocks super external:/md127/0

md127 : inactive sda[0](S)
      527704 blocks super external:ddf

unused devices: <none>`
	devs := parseMdstat(real)
	if len(devs) != 2 {
		t.Fatalf("expected 2 md devices, got %d: %+v", len(devs), devs)
	}
	byName := map[string]bool{}
	for _, d := range devs {
		byName[d.name] = d.isContainer
	}
	if byName["md127"] != true {
		t.Errorf("md127 (super external:ddf) should be classified a container")
	}
	if byName["md126"] != false {
		t.Errorf("md126 (super external:/md127/0) should be a member subarray, not a container")
	}

	// Empty / no-array mdstat yields no devices (the clean-disk common case).
	if got := parseMdstat("Personalities : [raid1]\nunused devices: <none>"); len(got) != 0 {
		t.Errorf("expected no md devices on a clean host, got %+v", got)
	}

	// A normal native-metadata array (super 1.2, not external) is NOT a container.
	const native = `md0 : active raid1 sdb2[1] sda2[0]
      937032704 blocks super 1.2 [2/2] [UU]`
	got := parseMdstat(native)
	if len(got) != 1 || got[0].name != "md0" || got[0].isContainer {
		t.Errorf("native md0 should parse as one non-container device, got %+v", got)
	}
}
