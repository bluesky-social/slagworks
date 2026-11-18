//go:build !integration

package storageapply

import (
	"fmt"
	"log/slog"
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

func TestPreflightPreserve_NoPreserveDevices_NoOp(t *testing.T) {
	layout := storage.Layout{Devices: map[string]storage.Device{
		"nvme0n1": {Type: "disk"},
	}}
	r := newScriptRunner()
	if err := PreflightPreserve(scriptCtx(r), layout); err != nil {
		t.Fatalf("no-preserve layout should be a no-op, got: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no exec calls, got %v", r.calls)
	}
}

func TestPreflightPreserve_Success(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := newScriptRunner()
	// mdadm --detail returns the matching UUID (mdadm's colon-grouped form).
	r.output["mdadm --detail "+md] = "        Version : 1.2\n           UUID : e8c9aa81:11112222:33334444:55556666\n"
	// blkid on the array returns the fs UUID (not declared here -> not checked).

	if err := PreflightPreserve(scriptCtx(r), layout); err != nil {
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
	// A global `mdadm --stop --scan` must precede the targeted assemble so an
	// initrd auto-assembly under a scan-chosen md name (e.g. /dev/md127) is
	// released and doesn't wedge the assemble with EBUSY (roast MEDIUM fix).
	scanIdx, asmIdx := -1, -1
	for i, c := range r.calls {
		if c == "mdadm --stop --scan" && scanIdx < 0 {
			scanIdx = i
		}
		if strings.HasPrefix(c, "mdadm --assemble ") && asmIdx < 0 {
			asmIdx = i
		}
	}
	if scanIdx < 0 {
		t.Errorf("expected a global `mdadm --stop --scan` to release auto-assembly, calls: %v", r.calls)
	}
	if scanIdx >= asmIdx {
		t.Errorf("`mdadm --stop --scan` must precede the targeted assemble, calls: %v", r.calls)
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
	r := newScriptRunner()
	r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"
	// The targeted post-verify stop fails. (--stop --scan and the pre-assemble
	// path succeed by default; only the exact-argv targeted stop errors.)
	r.runErr["mdadm --stop "+md] = fmt.Errorf("mdadm: /dev/md0: still in use")

	err := PreflightPreserve(scriptCtx(r), layout)
	if err == nil || !strings.Contains(err.Error(), "could not be stopped") {
		t.Fatalf("expected FATAL when the verified array can't be stopped, got: %v", err)
	}
}

func TestPreflightPreserve_MemberOnDiskTarget_FATAL(t *testing.T) {
	// finding #3: a preserve member on an UNDECLARED partition of a managed
	// disk. Static validate() misses it (it compares full partition names);
	// the parent-disk match here catches it before any zap.
	layout := preserveLayout()
	dev := layout.Devices["mddata"]
	dev.Members = []string{"nvme0n1p9", "nvme2n1"} // nvme0n1 IS a disk target
	layout.Devices["mddata"] = dev

	r := newScriptRunner()
	err := PreflightPreserve(scriptCtx(r), layout)
	if err == nil {
		t.Fatal("expected FATAL: preserve member on a disk-type layout target")
	}
	if !strings.Contains(err.Error(), "nvme0n1") {
		t.Errorf("error should name the offending parent disk, got: %v", err)
	}
	// Crucially, NO assemble may have run -- we must fail before touching mdadm.
	if r.ran("--assemble") {
		t.Errorf("must FATAL before assembling when a member is on a disk target, calls: %v", r.calls)
	}
}

func TestPreflightPreserve_AssembleFails_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := newScriptRunner()
	r.runErr["mdadm --assemble "+md+" --uuid "+testArrayUUID+" /dev/nvme1n1 /dev/nvme2n1"] =
		fmt.Errorf("mdadm: no recogniseable superblock")

	err := PreflightPreserve(scriptCtx(r), layout)
	if err == nil || !strings.Contains(err.Error(), "assemble") {
		t.Fatalf("expected assemble-failure FATAL, got: %v", err)
	}
}

func TestPreflightPreserve_UUIDMismatch_FATAL(t *testing.T) {
	layout := preserveLayout()
	md := storage.MDDevMap(layout)["mddata"]
	r := newScriptRunner()
	// Assembled array reports a DIFFERENT UUID than expected.
	r.output["mdadm --detail "+md] = "           UUID : deadbeef:00000000:11111111:22222222\n"

	err := PreflightPreserve(scriptCtx(r), layout)
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

	r := newScriptRunner()
	r.output["mdadm --detail "+md] = "           UUID : e8c9aa81:11112222:33334444:55556666\n"
	r.output["blkid -s UUID -o value "+md] = "ffffffff-0000-1111-2222-333333333333" // != declared FSUUID

	err := PreflightPreserve(scriptCtx(r), layout)
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

// TestStopAutoAssembly asserts the non-destructive-path pre-assemble stop
// issues the global `mdadm --stop --scan` that releases udev re-assembly so
// planPreserve's targeted --assemble can claim its members (re-review
// finding #1).
func TestStopAutoAssembly(t *testing.T) {
	r := newScriptRunner()
	// StopResidualMDDevices reads /proc/mdstat via Output; an empty read is
	// the "no residual md" case.
	r.output["cat /proc/mdstat"] = ""
	StopAutoAssembly(scriptCtx(r))
	if !r.ran("mdadm --stop --scan") {
		t.Errorf("StopAutoAssembly must issue `mdadm --stop --scan`; calls: %v", r.calls)
	}
}
