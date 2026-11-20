//go:build !integration

package storageapply

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// scriptRunner is a fake agent.Runner for the preserve paths: it records
// every Run/Output call and answers from scripted tables keyed on a joined
// argv. Run defaults to success; Output defaults to "" + error unless a
// prefix match is registered.
type scriptRunner struct {
	calls     []string          // "name arg arg ..." for every Run/Output
	runErr    map[string]error  // exact-argv -> error for Run
	output    map[string]string // argv-prefix -> stdout for Output
	outputErr map[string]error  // argv-prefix -> error for Output
}

func newScriptRunner() *scriptRunner {
	return &scriptRunner{
		runErr:    map[string]error{},
		output:    map[string]string{},
		outputErr: map[string]error{},
	}
}

func (r *scriptRunner) Available(string) bool { return true }

func (r *scriptRunner) Run(name string, args ...string) error {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	return r.runErr[key]
}

func (r *scriptRunner) Output(name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, key)
	for prefix, out := range r.output {
		if strings.HasPrefix(key, prefix) {
			return out, r.outputErr[prefix]
		}
	}
	for prefix, err := range r.outputErr {
		if strings.HasPrefix(key, prefix) {
			return "", err
		}
	}
	return "", fmt.Errorf("scriptRunner: no output registered for %q", key)
}

func (r *scriptRunner) ran(substr string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func scriptCtx(r *scriptRunner) *agent.Context {
	return &agent.Context{Log: slog.Default(), Exec: r}
}

const testArrayUUID = "e8c9aa81-1111-2222-3333-444455556666"

// preserveLayout builds a boot-disk + preserved-RAID0 layout like the asus
// case, scaled to a single boot disk.
func preserveLayout() storage.Layout {
	return storage.Layout{
		Devices: map[string]storage.Device{
			"nvme0n1": {Type: "disk", Boot: true, Partitions: []storage.Partition{
				{Name: "nvme0n1p1", FS: "vfat", Size: "512M", Bootable: true},
				{Name: "nvme0n1p2", FS: "ext4", Size: "rest"},
			}},
			"mddata": {Type: "preserve", Level: 0,
				Members:   []string{"nvme1n1", "nvme2n1"},
				FS:        "xfs",
				ArrayUUID: testArrayUUID},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "nvme0n1p2"},
			"/boot/efi": {Device: "nvme0n1p1"},
			"/data":     {Device: "mddata"},
		},
	}
}

// emptyMdstat is /proc/mdstat with the md driver loaded and no array
// assembled.
const emptyMdstat = "Personalities : [raid0] [raid1]\nunused devices: <none>\n"

// autoAssembledMdstat is the deploy environment's view when the initrd has
// auto-assembled preserveLayout's array under a scan-chosen name (md127)
// and an old array on a disk outside the layout (sdz) as md126.
const autoAssembledMdstat = "Personalities : [raid0] [raid1]\n" +
	"md126 : active raid1 sdz[0]\n" +
	"      937032704 blocks super 1.2 [2/1] [U_]\n\n" +
	"md127 : active raid0 nvme2n1[1] nvme1n1[0]\n" +
	"      1874065408 blocks super 1.2 512k chunks\n\n" +
	"unused devices: <none>\n"

// preserveRunner is a scriptRunner that answers the kernel's view of
// preserveLayout's members, both whole disks, on a host where no array is
// assembled yet.
func preserveRunner() *scriptRunner {
	r := newScriptRunner()
	r.output["lsblk -snlo NAME,TYPE /dev/nvme1n1"] = "nvme1n1 disk"
	r.output["lsblk -snlo NAME,TYPE /dev/nvme2n1"] = "nvme2n1 disk"
	r.output["lsblk -ndo KNAME /dev/nvme1n1"] = "nvme1n1"
	r.output["lsblk -ndo KNAME /dev/nvme2n1"] = "nvme2n1"
	r.output["cat /proc/mdstat"] = emptyMdstat
	return r
}

func TestPreflightPreserve_NoPreserveDevices_NoOp(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
	}}
	r := newScriptRunner()
	if err := PreflightPreserve(scriptCtx(r), layout, false); err != nil {
		t.Fatalf("no-preserve layout should be a no-op, got: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no exec calls, got %v", r.calls)
	}
}

func TestPreflightPreserve_Success(t *testing.T) {
	for _, destructive := range []bool{false, true} {
		t.Run(fmt.Sprintf("destructive=%v", destructive), func(t *testing.T) {
			testPreflightPreserveSuccess(t, destructive)
		})
	}
}

func testPreflightPreserveSuccess(t *testing.T, destructive bool) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := preserveRunner()
	r.output["cat /proc/mdstat"] = autoAssembledMdstat
	// mdadm --detail returns the matching UUID (mdadm's colon-grouped form).
	r.output["mdadm --detail "+md] = "        Version : 1.2\n           UUID : e8c9aa81:11112222:33334444:55556666\n"
	// blkid on the array returns the fs UUID (not declared here -> not checked).

	if err := PreflightPreserve(scriptCtx(r), layout, destructive); err != nil {
		t.Fatalf("preflight should succeed, got: %v", err)
	}
	// Must have assembled by uuid, verified via --detail, and stopped it.
	if !r.ran("mdadm --assemble " + md + " --uuid " + testArrayUUID + " /dev/nvme1n1 /dev/nvme2n1") {
		t.Errorf("expected assemble-by-uuid with resolved members, calls: %v", r.calls)
	}
	if !r.ran("mdadm --detail " + md) {
		t.Errorf("expected mdadm --detail verify, calls: %v", r.calls)
	}
	// The final stop (post-verify) must have happened so planPreserve can
	// re-assemble in the main loop.
	stops := 0
	for _, c := range r.calls {
		if c == "mdadm --stop "+md {
			stops++
		}
	}
	if stops < 1 {
		t.Errorf("expected at least one `mdadm --stop %s` (verified array stopped for re-assemble), calls: %v", md, r.calls)
	}
	// The initrd's auto-assembly of the members (md127) must be released
	// before the targeted assemble, or it wedges the assemble with EBUSY.
	// --destructive authorizes a global `mdadm --stop --scan`; without it
	// only md127 is stopped, and md126, an array on a disk outside the
	// layout, is left assembled.
	release := "mdadm --stop /dev/md127"
	if destructive {
		release = "mdadm --stop --scan"
	}
	relIdx, asmIdx := -1, -1
	for i, c := range r.calls {
		if c == release && relIdx < 0 {
			relIdx = i
		}
		if strings.HasPrefix(c, "mdadm --assemble ") && asmIdx < 0 {
			asmIdx = i
		}
	}
	if relIdx < 0 {
		t.Errorf("expected %q to release the auto-assembly, calls: %v", release, r.calls)
	}
	if relIdx >= asmIdx {
		t.Errorf("%q must precede the targeted assemble, calls: %v", release, r.calls)
	}
	if !destructive {
		for _, c := range r.calls {
			if c == "mdadm --stop --scan" || c == "mdadm --stop /dev/md126" {
				t.Errorf("preflight without --destructive ran %q, which stops the array on sdz outside the layout; calls: %v", c, r.calls)
			}
		}
	}
}

// TestPreflightPreserve_PostVerifyStopFails_FATAL is the roast HIGH guard: a
// verified array whose post-verify stop fails must FATAL while the host is
// still pre-destructive, not be swallowed as success (which would let the
// zap/recreate run and then have planPreserve's re-assemble fail "already
// active" after the boot disks are gone).
func TestPreflightPreserve_PostVerifyStopFails_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := preserveRunner()
	r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"
	// The targeted post-verify stop fails. (Nothing is assembled before the
	// preflight's own assemble; only the exact-argv targeted stop errors.)
	r.runErr["mdadm --stop "+md] = fmt.Errorf("mdadm: /dev/md0: still in use")

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "could not be stopped") {
		t.Fatalf("expected FATAL when the verified array can't be stopped, got: %v", err)
	}
}

// TestPreflightPreserve_MemberOnDiskTarget_FATAL: a preserve member on an
// UNDECLARED partition of a disk the layout partitions (finding #3). Static
// validate() misses it (it compares full partition names); the guard must
// catch it before any zap, whatever the member is called. The kernel, not
// the member's spelling, says which disk holds it: a /dev/disk/by-id path or
// a partition of a pmem disk does not parse back to its disk's name.
func TestPreflightPreserve_MemberOnDiskTarget_FATAL(t *testing.T) {
	cases := []struct {
		name   string
		disk   string // the disk-type layout target that holds the member
		member string
		lsblk  string // lsblk -snlo NAME,TYPE <member>
	}{
		{"kernel partition name", "nvme0n1", "nvme0n1p9", "nvme0n1p9 part\nnvme0n1   disk"},
		{"/dev/disk/by-id path to the partition", "nvme0n1",
			"/dev/disk/by-id/nvme-EXAMPLE_SERIAL_S1-part9", "nvme0n1p9 part\nnvme0n1   disk"},
		{"partition of a pmem disk", "pmem0", "pmem0p2", "pmem0p2 part\npmem0   disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := preserveLayout()
			layout.Devices[tc.disk] = storage.Device{Type: "disk", Partitions: []storage.Partition{
				{Name: tc.disk + "p1", FS: "ext4", Size: "rest"},
			}}
			dev := layout.Devices["mddata"]
			dev.Members = []string{tc.member, "nvme2n1"}
			layout.Devices["mddata"] = dev
			md := storage.MDDevMap(layout)["mddata"]

			r := preserveRunner()
			r.output["lsblk -snlo NAME,TYPE "+resolveMemberPath(tc.member)] = tc.lsblk
			// The named members would assemble and verify, so only the guard
			// stands between this layout and sgdisk --zap-all on the member.
			r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"

			err := PreflightPreserve(scriptCtx(r), layout, false)
			if err == nil {
				t.Fatalf("expected FATAL: preserve member %q is on disk target %s", tc.member, tc.disk)
			}
			if !strings.Contains(err.Error(), tc.disk) {
				t.Errorf("error should name the offending disk %s, got: %v", tc.disk, err)
			}
			// The host must be untouched: no mdadm call at all, not even a stop.
			if r.ran("mdadm") {
				t.Errorf("must FATAL before any mdadm call when a member is on a disk target, calls: %v", r.calls)
			}
		})
	}
}

// TestPreflightPreserve_MemberNotOnHost_FATAL: a member the kernel does not
// know cannot be checked against the disk targets, so the guard fails with
// the host untouched instead of proceeding.
func TestPreflightPreserve_MemberNotOnHost_FATAL(t *testing.T) {
	layout := preserveLayout()
	r := newScriptRunner()
	r.output["lsblk -snlo NAME,TYPE /dev/nvme1n1"] = "nvme1n1 disk"
	r.outputErr["lsblk -snlo NAME,TYPE /dev/nvme2n1"] = fmt.Errorf("exit status 32")

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "nvme2n1") {
		t.Fatalf("expected FATAL naming the missing member nvme2n1, got: %v", err)
	}
	if r.ran("mdadm") {
		t.Errorf("must FATAL before any mdadm call, calls: %v", r.calls)
	}
}

func TestPreflightPreserve_AssembleFails_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := preserveRunner()
	r.runErr["mdadm --assemble "+md+" --uuid "+testArrayUUID+" /dev/nvme1n1 /dev/nvme2n1"] =
		fmt.Errorf("mdadm: no recogniseable superblock")

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "assemble") {
		t.Fatalf("expected assemble-failure FATAL, got: %v", err)
	}
}

func TestPreflightPreserve_UUIDMismatch_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := preserveRunner()
	// Assembled array reports a DIFFERENT UUID than expected.
	r.output["mdadm --detail "+md] = "           UUID : deadbeef:00000000:11111111:22222222\n"

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("expected UUID-mismatch FATAL, got: %v", err)
	}
	// On mismatch the array we assembled must be stopped (leave host as found).
	stops := 0
	for _, c := range r.calls {
		if c == "mdadm --stop "+md {
			stops++
		}
	}
	if stops == 0 {
		t.Errorf("expected the mis-assembled array to be stopped on mismatch, calls: %v", r.calls)
	}
}

func TestPreflightPreserve_FSUUIDMismatch_FATAL(t *testing.T) {
	layout := preserveLayout()
	dev := layout.Devices["mddata"]
	dev.FSUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	layout.Devices["mddata"] = dev
	md := storage.MDDevMap(layout)["mddata"]

	r := preserveRunner()
	r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"
	r.output["blkid -s UUID -o value "+md] = "ffffffff-0000-1111-2222-333333333333" // != declared FSUUID

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("expected fs-UUID-mismatch FATAL, got: %v", err)
	}
}

func TestVerifyPreserveArrays_Success(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := newScriptRunner()
	r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"

	if err := VerifyPreserveArrays(scriptCtx(r), layout); err != nil {
		t.Fatalf("verify should pass with matching UUID, got: %v", err)
	}
	// Verify must NOT stop the array -- it's mounted/about-to-mount by now.
	if r.ran("--stop") {
		t.Errorf("VerifyPreserveArrays must not stop the array, calls: %v", r.calls)
	}
}

func TestVerifyPreserveArrays_Mismatch_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := newScriptRunner()
	r.output["mdadm --detail "+md] = "           UUID : deadbeef:00000000:11111111:22222222\n"

	if err := VerifyPreserveArrays(scriptCtx(r), layout); err == nil {
		t.Fatal("expected FATAL on re-assembled UUID mismatch")
	}
}

func TestParseMDADMDetailUUID(t *testing.T) {
	detail := `/dev/md0:
           Version : 1.2
     Creation Time : Wed Jul 16 2026
        Raid Level : raid0
              UUID : e8c9aa81:11112222:33334444:55556666
            Events : 0
`
	got, ok := parseMDADMDetailUUID(detail)
	if !ok {
		t.Fatal("expected to find UUID line")
	}
	if got != "e8c9aa81:11112222:33334444:55556666" {
		t.Errorf("UUID = %q", got)
	}

	if _, ok := parseMDADMDetailUUID("no uuid here\n"); ok {
		t.Error("expected no UUID found")
	}
}

func TestUUIDsEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// mdadm colon-grouped vs RFC-4122 dashed, same bits.
		{"e8c9aa81:11112222:33334444:55556666", "e8c9aa81-1111-2222-3333-444455556666", true},
		// case-insensitive.
		{"E8C9AA81:11112222:33334444:55556666", "e8c9aa81-1111-2222-3333-444455556666", true},
		// different bits.
		{"e8c9aa81:11112222:33334444:55556666", "deadbeef-1111-2222-3333-444455556666", false},
		// empty never equal.
		{"", "e8c9aa81-1111-2222-3333-444455556666", false},
		{"e8c9aa81-1111-2222-3333-444455556666", "", false},
		// short/garbage never equal.
		{"abcd", "abcd", false},
	}
	for _, c := range cases {
		if got := uuidsEqual(c.a, c.b); got != c.want {
			t.Errorf("uuidsEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestHasPreserveDevices(t *testing.T) {
	if !HasPreserveDevices(preserveLayout()) {
		t.Error("preserveLayout has a preserve device, want true")
	}
	// A layout with only disk/raid devices must report false.
	plain := storage.Layout{
		Devices: map[string]storage.Device{
			"nvme0n1": {Type: "disk", Boot: true, Partitions: []storage.Partition{
				{Name: "nvme0n1p1", FS: "ext4", Size: "rest"},
			}},
		},
		Mounts: map[string]storage.Mount{"/": {Device: "nvme0n1p1"}},
	}
	if HasPreserveDevices(plain) {
		t.Error("plain disk layout has no preserve device, want false")
	}
}

// TestStopAutoAssembly: the non-destructive path's release stops every array
// the deploy environment assembled from preserve members, so planPreserve's
// targeted --assemble can claim them, and no other array. An array that
// holds a preserve member together with another device is refused before
// anything is stopped.
func TestStopAutoAssembly(t *testing.T) {
	byIDLayout := preserveLayout()
	dev := byIDLayout.Devices["mddata"]
	dev.Members = []string{"nvme1n1", "/dev/disk/by-id/nvme-EXAMPLE_SERIAL_S2"}
	byIDLayout.Devices["mddata"] = dev

	partLayout := preserveLayout()
	dev = partLayout.Devices["mddata"]
	dev.Members = []string{"nvme1n1p1", "nvme2n1p1"}
	partLayout.Devices["mddata"] = dev

	tests := []struct {
		name     string
		layout   storage.Layout
		mdstat   string // "" means the read fails
		absent   bool   // with a failed read, /proc/mdstat does not exist
		knames   map[string]string
		stopErr  string // an md device whose stop fails
		want     []string
		wantErr  []string
		wantNone bool // no mdadm or lsblk command runs at all
	}{
		{name: "nothing assembled", layout: preserveLayout(), mdstat: emptyMdstat, wantNone: true},
		{name: "md driver not loaded", layout: preserveLayout(), absent: true, wantNone: true},
		{name: "mdstat exists but cannot be read", layout: preserveLayout(),
			wantErr: []string{"/proc/mdstat exists but reading it failed", "--recovery"}},
		{name: "udev re-assembly beside an array outside the layout", layout: preserveLayout(),
			mdstat: autoAssembledMdstat, want: []string{"mdadm --stop /dev/md127"}},
		{name: "member named by /dev/disk/by-id", layout: byIDLayout,
			mdstat: autoAssembledMdstat,
			knames: map[string]string{"/dev/nvme1n1": "nvme1n1", "/dev/disk/by-id/nvme-EXAMPLE_SERIAL_S2": "nvme2n1"},
			want:   []string{"mdadm --stop /dev/md127"}},
		{name: "inactive partial assembly of partition members", layout: partLayout,
			mdstat: "Personalities : [raid0]\nmd127 : inactive nvme1n1p1[0](S)\n      937032704 blocks super 1.2\n\nunused devices: <none>\n",
			knames: map[string]string{"/dev/nvme1n1p1": "nvme1n1p1", "/dev/nvme2n1p1": "nvme2n1p1"},
			want:   []string{"mdadm --stop /dev/md127"}},
		{name: "external-metadata subarray before its container", layout: preserveLayout(),
			mdstat: "Personalities : [raid0]\n" +
				"md127 : inactive nvme2n1[1](S) nvme1n1[0](S)\n      10402 blocks super external:imsm\n\n" +
				"md126 : active raid0 nvme1n1[1] nvme2n1[0]\n      1874065408 blocks super external:/md127/0\n\n" +
				"unused devices: <none>\n",
			want: []string{"mdadm --stop /dev/md126", "mdadm --stop /dev/md127"}},
		{name: "array mixing preserve members with a disk outside the layout", layout: preserveLayout(),
			mdstat: "Personalities : [raid1]\n" +
				"md125 : active raid1 nvme2n1[0]\n      937032704 blocks super 1.2 [2/1] [U_]\n\n" +
				"md127 : active raid1 sdz[2](S) nvme1n1[0]\n      937032704 blocks super 1.2 [2/1] [U_]\n\n" +
				"unused devices: <none>\n",
			wantErr: []string{"/dev/md127", `preserve "mddata" member nvme1n1 together with sdz`, "--destructive"}},
		{name: "stop fails", layout: preserveLayout(), mdstat: autoAssembledMdstat, stopErr: "/dev/md127",
			want: []string{"mdadm --stop /dev/md127"}, wantErr: []string{"mdadm --stop /dev/md127", "--recovery"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newScriptRunner()
			if tt.knames == nil {
				// scriptRunner matches by prefix, so these would also answer
				// for nvme1n1p1: a case with other members lists them all.
				tt.knames = map[string]string{"/dev/nvme1n1": "nvme1n1", "/dev/nvme2n1": "nvme2n1"}
			}
			for path, kname := range tt.knames {
				r.output["lsblk -ndo KNAME "+path] = kname
			}
			if tt.mdstat != "" {
				r.output["cat /proc/mdstat"] = tt.mdstat
			} else {
				r.outputErr["cat /proc/mdstat"] = exitErr{1}
				if tt.absent {
					r.output["test ! -e /proc/mdstat"] = ""
				} else {
					r.outputErr["test ! -e /proc/mdstat"] = exitErr{1}
				}
			}
			if tt.stopErr != "" {
				r.runErr["mdadm --stop "+tt.stopErr] = fmt.Errorf("mdadm: Cannot get exclusive access to %s", tt.stopErr)
			}

			err := StopAutoAssembly(scriptCtx(r), tt.layout)

			var mdadm []string
			for _, c := range r.calls {
				if strings.HasPrefix(c, "mdadm") {
					mdadm = append(mdadm, c)
				}
				if tt.wantNone && (strings.HasPrefix(c, "mdadm") || strings.HasPrefix(c, "lsblk")) {
					t.Errorf("ran %q with no array to release", c)
				}
			}
			if !slices.Equal(mdadm, tt.want) {
				t.Errorf("mdadm commands = %q, want %q", mdadm, tt.want)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("StopAutoAssembly: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("StopAutoAssembly succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// A preflight without --destructive refuses, before any assemble, when an
// array the deploy environment assembled holds a preserve member together
// with a device outside the layout: it may not stop that array, and the
// preserve array cannot be assembled while it holds the member.
func TestPreflightPreserve_ArrayHoldsMemberAndOutsideDisk_Refused(t *testing.T) {
	layout := preserveLayout()
	r := preserveRunner()
	r.output["cat /proc/mdstat"] = "Personalities : [raid1]\nmd127 : active raid1 sdz[2](S) nvme2n1[1] nvme1n1[0]\n" +
		"      937032704 blocks super 1.2 [2/2] [UU]\n\nunused devices: <none>\n"

	err := PreflightPreserve(scriptCtx(r), layout, false)
	if err == nil || !strings.Contains(err.Error(), "/dev/md127") || !strings.Contains(err.Error(), "sdz") {
		t.Fatalf("expected a refusal naming md127 and sdz, got: %v", err)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "mdadm --stop") || strings.HasPrefix(c, "mdadm --assemble") {
			t.Errorf("preflight ran %q; the refusal must come before any array is stopped or assembled", c)
		}
	}
}
