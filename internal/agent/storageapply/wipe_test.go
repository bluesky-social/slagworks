//go:build !integration

package storageapply

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
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
	got := parseBystanderDisks(lsblkOut, layoutClaimedDisks(layout))
	want := []string{"nvme2n1", "nvme3n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

func TestParseBystanderDisks_AllClaimed(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"sda": {Type: "disk"},
	}}
	if got := parseBystanderDisks("sda disk\n", layoutClaimedDisks(layout)); got != nil {
		t.Errorf("expected no bystanders, got %v", got)
	}
}

func TestParseBystanderDisks_RetainedDiskIsProtected(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"nvme1n1": {Type: "retain"},
	}}
	lsblkOut := "nvme0n1 disk\nnvme1n1 disk\nnvme2n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layoutClaimedDisks(layout))
	want := []string{"nvme2n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
}

func TestParseBystanderDisks_MalformedLinesIgnored(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{}}
	out := "\n  \nnvme0n1\nnvme1n1 disk extra-field\nsda disk\n"
	got := parseBystanderDisks(out, layoutClaimedDisks(layout))
	want := []string{"sda"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v", got, want)
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
	got := parseBystanderDisks(lsblkOut, layoutClaimedDisks(layout))
	want := []string{"nvme6n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (raid members nvme4n1/nvme5n1 must be excluded)", got, want)
	}
}

// TestParseBystanderDisks_DevPrefixedRaidMemberNormalizes confirms a "/dev/"-
// prefixed raid member excludes the same disk as its bare form.
func TestParseBystanderDisks_DevPrefixedRaidMemberNormalizes(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"raid": {Type: "raid", Members: []string{"/dev/nvme0n1p2", "/dev/nvme2n1"}},
	}}
	lsblkOut := "nvme0n1 disk\nnvme2n1 disk\nnvme3n1 disk\n"
	got := parseBystanderDisks(lsblkOut, layoutClaimedDisks(layout))
	want := []string{"nvme3n1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bystanders = %v, want %v (/dev/-prefixed members must normalize like bare names)", got, want)
	}
}

// wipeCallsOn returns the recorded commands that write signatures or md
// metadata (mdadm --zero-superblock, wipefs, sgdisk) on disk or on one of its
// partitions.
func wipeCallsOn(calls []string, disk string) []string {
	var out []string
	for _, c := range calls {
		f := strings.Fields(c)
		if len(f) < 2 {
			continue
		}
		if f[0] != "wipefs" && f[0] != "sgdisk" && (f[0] != "mdadm" || f[1] != "--zero-superblock") {
			continue
		}
		for _, a := range f[1:] {
			if a == disk || strings.HasPrefix(a, disk+"p") {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// TestWipeBystanderDisks_PreserveMemberDiskIsNeverWiped: --wipe-all-disks
// must leave alone every disk that holds a preserve member, however the
// storage script spells the member. Each case gives the host's whole disks
// (`lsblk -dnlo NAME,TYPE`) and what the kernel reports for each member
// (`lsblk -snlo NAME,TYPE <member>`: the member, then the devices beneath
// it). A /dev/disk/by-id path and a pmem disk are spellings mdadm accepts
// but whose text does not name the lsblk disk, so the disk holding the data
// must come from the kernel, not from parsing the member name.
func TestWipeBystanderDisks_PreserveMemberDiskIsNeverWiped(t *testing.T) {
	cases := []struct {
		name      string
		disks     string
		members   map[string]string // member as written -> lsblk -snlo output
		protected []string          // disks holding preserve data
		bystander string            // a disk nothing claims, which must be wiped
	}{
		{
			name:      "kernel name of a whole disk",
			disks:     "nvme0n1 disk\nnvme2n1 disk\nnvme3n1 disk",
			members:   map[string]string{"nvme2n1": "nvme2n1 disk"},
			protected: []string{"/dev/nvme2n1"},
			bystander: "/dev/nvme3n1",
		},
		{
			name:      "kernel name of a partition on a disk the layout does not declare",
			disks:     "nvme0n1 disk\nnvme1n1 disk\nnvme4n1 disk",
			members:   map[string]string{"nvme1n1p2": "nvme1n1p2 part\nnvme1n1   disk"},
			protected: []string{"/dev/nvme1n1"},
			bystander: "/dev/nvme4n1",
		},
		{
			name:      "/dev/-prefixed kernel name",
			disks:     "nvme0n1 disk\nnvme2n1 disk\nnvme3n1 disk",
			members:   map[string]string{"/dev/nvme2n1": "nvme2n1 disk"},
			protected: []string{"/dev/nvme2n1"},
			bystander: "/dev/nvme3n1",
		},
		{
			name:      "/dev/disk/by-id path to a whole disk",
			disks:     "nvme0n1 disk\nnvme1n1 disk\nnvme2n1 disk",
			members:   map[string]string{"/dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1": "nvme1n1 disk"},
			protected: []string{"/dev/nvme1n1"},
			bystander: "/dev/nvme2n1",
		},
		{
			name:      "/dev/disk/by-id path to a partition",
			disks:     "nvme0n1 disk\nsdb disk\nsdc disk",
			members:   map[string]string{"/dev/disk/by-id/wwn-0x5000c500a1b2c3d4-part3": "sdb3 part\nsdb  disk"},
			protected: []string{"/dev/sdb"},
			bystander: "/dev/sdc",
		},
		{
			name:      "pmem whole disks (digit-ending name outside nvme/mmcblk/loop/dm/md)",
			disks:     "nvme0n1 disk\npmem0 disk\npmem1 disk\nnvme1n1 disk",
			members:   map[string]string{"pmem0": "pmem0 disk", "pmem1": "pmem1 disk"},
			protected: []string{"/dev/pmem0", "/dev/pmem1"},
			bystander: "/dev/nvme1n1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			members := make([]string, 0, len(tc.members))
			r := newScriptRunner()
			r.output["lsblk -dnlo NAME,TYPE"] = tc.disks
			for m, out := range tc.members {
				members = append(members, m)
				r.output["lsblk -snlo NAME,TYPE "+resolveMemberPath(m)] = out
			}
			layout := storage.Layout{Devices: map[string]storage.Device{
				"nvme0n1": {Type: "disk", Partitions: []storage.Partition{{Name: "nvme0n1p1", Size: "rest", FS: "ext4"}}},
				"mddata":  {Type: "preserve", Members: members, FS: "xfs", ArrayUUID: testArrayUUID},
			}}

			if err := WipeBystanderDisks(scriptCtx(r), layout); err != nil {
				t.Fatalf("WipeBystanderDisks: %v", err)
			}
			for _, d := range tc.protected {
				if got := wipeCallsOn(r.calls, d); len(got) > 0 {
					t.Errorf("preserve member disk %s was wiped: %v", d, got)
				}
			}
			if !r.ran("wipefs -a " + tc.bystander) {
				t.Errorf("bystander %s was not wiped; calls: %v", tc.bystander, r.calls)
			}
		})
	}
}

// TestParseExamineArrayUUIDs pins which `mdadm --examine` lines name the
// array a member belongs to: "Array UUID" for 1.x metadata, "UUID" for 0.90
// (with its "(local to host ...)" suffix dropped), never "Device UUID".
func TestParseExamineArrayUUIDs(t *testing.T) {
	tests := []struct {
		name    string
		examine string
		want    []string
	}{
		{
			name: "1.2 member",
			examine: `/dev/vdb:
          Magic : a92b4efc
        Version : 1.2
     Array UUID : 5d1f0c1e:7a3b4c2d:9e8f0a1b:2c3d4e5f
           Name : ubuntu-server:0
    Device UUID : 0b3c8f6e:1d2a4b5c:6e7f8091:a2b3c4d5
   Array State : AA ('A' == active, '.' == missing, 'R' == replacing)`,
			want: []string{"5d1f0c1e:7a3b4c2d:9e8f0a1b:2c3d4e5f"},
		},
		{
			name: "0.90 member",
			examine: `/dev/sdb1:
          Magic : a92b4efc
        Version : 0.90.00
           UUID : 5d1f0c1e:7a3b4c2d:9e8f0a1b:2c3d4e5f (local to host builder)`,
			want: []string{"5d1f0c1e:7a3b4c2d:9e8f0a1b:2c3d4e5f"},
		},
		{
			name:    "no superblock",
			examine: "",
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseExamineArrayUUIDs(tt.examine); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseExamineArrayUUIDs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWipeBystanderDisks_UnresolvablePreserveMemberWipesNothing: when the
// kernel cannot say which disk holds a preserve member, --wipe-all-disks
// must refuse to wipe anything rather than guess.
func TestWipeBystanderDisks_UnresolvablePreserveMemberWipesNothing(t *testing.T) {
	r := newScriptRunner()
	r.output["lsblk -dnlo NAME,TYPE"] = "nvme0n1 disk\nnvme1n1 disk\nnvme2n1 disk"
	r.outputErr["lsblk -snlo NAME,TYPE /dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1"] = fmt.Errorf("exit status 32")
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
		"mddata": {Type: "preserve", Members: []string{"/dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1"},
			FS: "xfs", ArrayUUID: testArrayUUID},
	}}

	err := WipeBystanderDisks(scriptCtx(r), layout)
	if err == nil || !strings.Contains(err.Error(), "/dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1") {
		t.Fatalf("expected an error naming the unresolvable member, got: %v", err)
	}
	for _, d := range []string{"/dev/nvme1n1", "/dev/nvme2n1"} {
		if got := wipeCallsOn(r.calls, d); len(got) > 0 {
			t.Errorf("%s was wiped although a preserve member could not be located: %v", d, got)
		}
	}
}

// pinnedRAIDLayout is a root disk vda plus a raid that pins its array UUID.
func pinnedRAIDLayout() storage.Layout {
	return storage.Layout{Devices: map[string]storage.Device{
		"vda":    {Type: "disk"},
		"mddata": {Type: "raid", Level: 1, Members: []string{"vda1", "vda2"}, ArrayUUID: testArrayUUID},
	}}
}

// TestCheckStaleArrayMembers_PartitionMember: a stale member on a partition
// of a disk outside the layout is found, not only one on a whole disk.
func TestCheckStaleArrayMembers_PartitionMember(t *testing.T) {
	r := newScriptRunner()
	r.output["lsblk -dnlo NAME,TYPE"] = "vda disk\nvdb disk"
	r.output["lsblk -nlo NAME /dev/vdb"] = "vdb\nvdb1"
	r.output["mdadm --examine /dev/vdb1"] = "/dev/vdb1:\n     Array UUID : e8c9aa81:11112222:33334444:55556666"

	err := CheckStaleArrayMembers(scriptCtx(r), pinnedRAIDLayout(), false)
	if err == nil || !strings.Contains(err.Error(), "/dev/vdb1") || !strings.Contains(err.Error(), `"mddata"`) {
		t.Fatalf("want a refusal naming /dev/vdb1 and mddata, got %v", err)
	}
	if r.ran("lsblk -nlo NAME /dev/vda") {
		t.Errorf("the layout's own disk vda is wiped by the deploy and must not be examined; calls: %v", r.calls)
	}
}

// TestCheckStaleArrayMembers_PartitionListingFails: a disk whose partitions
// cannot be listed has not been checked, so the deploy must not proceed as
// though it were clean.
func TestCheckStaleArrayMembers_PartitionListingFails(t *testing.T) {
	r := newScriptRunner()
	r.output["lsblk -dnlo NAME,TYPE"] = "vda disk\nvdb disk"
	r.outputErr["lsblk -nlo NAME /dev/vdb"] = errors.New("exit status 32")

	err := CheckStaleArrayMembers(scriptCtx(r), pinnedRAIDLayout(), false)
	if err == nil || !strings.Contains(err.Error(), "/dev/vdb") {
		t.Fatalf("want an error naming /dev/vdb, got %v", err)
	}
}

// TestCheckStaleArrayMembers_WipeAllDisksExaminesPreserveMemberDisk: under
// --wipe-all-disks the bystander wipe leaves a disk holding a preserve member
// alone however the member is spelled, so the stale-member check must examine
// that disk rather than assume the wipe zeroes it. The member is a by-id path,
// whose text does not name the disk; only the kernel can place it.
func TestCheckStaleArrayMembers_WipeAllDisksExaminesPreserveMemberDisk(t *testing.T) {
	const member = "/dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1"
	r := newScriptRunner()
	r.output["lsblk -dnlo NAME,TYPE"] = "vda disk\nvdb disk\nvdc disk"
	r.output["lsblk -snlo NAME,TYPE "+member] = "vdc disk"
	r.output["lsblk -nlo NAME /dev/vdc"] = "vdc"
	r.output["mdadm --examine /dev/vdc"] = "/dev/vdc:\n     Array UUID : e8c9aa81:11112222:33334444:55556666"
	layout := pinnedRAIDLayout()
	layout.Devices["mdkeep"] = storage.Device{Type: "preserve", Members: []string{member}, FS: "xfs",
		ArrayUUID: "0f0f0f0f-1111-2222-3333-444455556666"}

	err := CheckStaleArrayMembers(scriptCtx(r), layout, true)
	if err == nil || !strings.Contains(err.Error(), "/dev/vdc") || !strings.Contains(err.Error(), "leaves retained and preserve-member disks alone") {
		t.Fatalf("want a refusal naming /dev/vdc as a disk --wipe-all-disks leaves alone, got %v", err)
	}
	if r.ran("lsblk -nlo NAME /dev/vdb") {
		t.Errorf("bystander vdb is zeroed by --wipe-all-disks and must not be examined; calls: %v", r.calls)
	}
}

// TestCheckStaleArrayMembers_UnresolvablePreserveMemberFails: when the kernel
// cannot place a preserve member, the check cannot tell which disks the
// bystander wipe leaves alone, so it must refuse rather than skip them.
func TestCheckStaleArrayMembers_UnresolvablePreserveMemberFails(t *testing.T) {
	const member = "/dev/disk/by-id/nvme-EXAMPLE_SERIAL_X1"
	r := newScriptRunner()
	r.output["lsblk -dnlo NAME,TYPE"] = "vda disk\nvdb disk"
	r.outputErr["lsblk -snlo NAME,TYPE "+member] = errors.New("exit status 32")
	layout := pinnedRAIDLayout()
	layout.Devices["mdkeep"] = storage.Device{Type: "preserve", Members: []string{member}, FS: "xfs",
		ArrayUUID: "0f0f0f0f-1111-2222-3333-444455556666"}

	err := CheckStaleArrayMembers(scriptCtx(r), layout, true)
	if err == nil || !strings.Contains(err.Error(), member) {
		t.Fatalf("want an error naming %s, got %v", member, err)
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

// shellExit returns the real *exec.ExitError of a command that writes
// stderr and exits with code, captured the way Exec.Output's cmd.Output()
// captures it in production (Stderr populated).
func shellExit(t *testing.T, stderr string, code int) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", fmt.Sprintf(`printf '%%s' "$1" >&2; exit %d`, code), "sh", stderr).Output()
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("sh exiting %d: got %v, want an *exec.ExitError", code, err)
	}
	return err
}

// TestCheckDisksClean pins the boot-disk safety check that guards a deploy
// without --destructive. The fake answers both util-linux probes the way
// they answer for the disk each case describes, so the cases hold whichever
// probe the check uses. A disk passes only when it is known to carry no
// signature; a probe that cannot run is not evidence of a blank disk.
func TestCheckDisksClean(t *testing.T) {
	// `blkid -p` prints nothing on stdout for a blank disk and for one
	// whose signatures it will not choose between. It exits 2 for the
	// blank disk (and for one it cannot open or read) and 8 for the
	// ambivalent one.
	blkidNothing := shellExit(t, "", 2)
	blkidAmbivalent := shellExit(t, "blkid: /dev/vda: ambivalent result (probably more filesystems on the device, use wipefs(8) to see more details)\n", 8)
	ioErr := "wipefs: error: /dev/vda: probing initialization failed: Input/output error\n"
	notFound := func(name string) error { return &exec.Error{Name: name, Err: exec.ErrNotFound} }

	cases := []struct {
		name      string
		blkidOut  string
		blkidErr  error
		wipefsOut string
		wipefsErr error
		wantErr   []string // substrings of the refusal; nil means the disk passes
	}{
		{
			name:     "blank disk passes",
			blkidErr: blkidNothing,
		},
		{
			name:      "partition table is refused",
			blkidOut:  `/dev/vda: PTUUID="93680f81-d24c-4751-9019-86595a378c03" PTTYPE="gpt"`,
			wipefsOut: "gpt  0x200\ngpt  0x1fffffe00\nPMBR 0x1fe",
			wantErr:   []string{"/dev/vda", "(gpt at 0x200 and 1 more, PMBR at 0x1fe)", "--destructive"},
		},
		{
			name:      "filesystem is refused",
			blkidOut:  `/dev/vda: UUID="b427ce1f-d0e1-41af-a9ef-034e23e4098c" BLOCK_SIZE="4096" TYPE="ext4" USAGE="filesystem"`,
			wipefsOut: "ext4 0x438",
			wantErr:   []string{"/dev/vda", "ext4 at 0x438", "--destructive"},
		},
		{
			// An ext4 disk later handed to ZFS without a wipe: blkid -p
			// prints nothing for it, as for a blank disk.
			// wipefs lists one zfs_member line per uberblock; the
			// refusal names each type once.
			name:      "conflicting signatures are refused",
			blkidErr:  blkidAmbivalent,
			wipefsOut: "ext4       0x438\nzfs_member 0x23c00\nzfs_member 0x23800\nzfs_member 0x23400",
			wantErr:   []string{"/dev/vda", "(ext4 at 0x438, zfs_member at 0x23c00 and 2 more)", "--destructive"},
		},
		{
			name:      "unreadable disk is refused",
			blkidErr:  blkidNothing,
			wipefsErr: shellExit(t, ioErr, 1),
			wantErr:   []string{"/dev/vda", "Input/output error", "--destructive"},
		},
		{
			name:      "missing probe binary is refused",
			blkidErr:  notFound("blkid"),
			wipefsErr: notFound("wipefs"),
			wantErr:   []string{"/dev/vda", "executable file not found", "--destructive"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newScriptRunner()
			r.output["blkid "], r.outputErr["blkid "] = tc.blkidOut, tc.blkidErr
			r.output["wipefs "], r.outputErr["wipefs "] = tc.wipefsOut, tc.wipefsErr
			layout := storage.Layout{Devices: map[string]storage.Device{
				"vda": {Type: "disk", Boot: true},
				"md0": {Type: "raid", Level: 1, Members: []string{"vda2", "vdb2"}},
			}}

			err := CheckDisksClean(scriptCtx(r), layout)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("CheckDisksClean refused a blank disk: %v (probes: %q)", err, r.calls)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckDisksClean passed the disk; a deploy without --destructive would partition it (probes: %q)", r.calls)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q:\n%v", want, err)
				}
			}
			for _, c := range r.calls {
				if strings.Contains(c, "md0") {
					t.Errorf("probed the non-disk device md0: %q", c)
				}
			}
		})
	}
}
