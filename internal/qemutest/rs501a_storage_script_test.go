//go:build integration

package qemutest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// These tests exercise the REAL shipped ASUS RS501A storage scripts
// (assets/storage/100-asus-rs501a, assets/storage/150-asus-rs501a-preserve)
// directly: golden inventory fixture in, emitted layout out. Same
// runAssetScript pattern as TestMS01StorageScript_* in
// ms01_router_script_test.go (sibling file).
//
// The create script pins mddata's array_uuid to
// uuidv5(system.uuid, "mddata") so a later preserve deploy of the same host
// derives the identical UUID with no round-trip through a recommission. The
// critical property under test is that BOTH scripts derive the SAME UUID for
// the SAME system UUID.

// rs501aInventory is a fixture matching the RS501A cohort's disk shape: two
// non-Kioxia NVMe boot drives + two Kioxia (KCD6XLUL) data drives, plus a
// SMBIOS system UUID for the array_uuid derivation.
const rs501aSystemUUID = "6401d326-387e-86db-1283-107c6158ff5b"

// rs501aExpectedArrayUUID is uuidv5(rs501aSystemUUID, "mddata"), computed
// independently in Go (uuid.NewSHA1 mirrors Python's uuid.uuid5) in
// TestRS501A_ArrayUUIDMatchesGoDerivation; hardcoded here as the fixture's
// known-good value so the other tests don't need to recompute it.
const rs501aExpectedArrayUUID = "172e3333-954d-5f32-93d6-799f12b7d012"

func rs501aInventoryJSON(systemUUID string) string {
	uuidField := ""
	if systemUUID != "" {
		uuidField = `"uuid": "` + systemUUID + `",`
	}
	return `{
  "system": {` + uuidField + `"manufacturer": "ASUS"},
  "disks": [
    {"name": "nvme0n1", "transport": "nvme", "model": "Samsung SSD 990 PRO", "size_bytes": 4000000000000},
    {"name": "nvme1n1", "transport": "nvme", "model": "Samsung SSD 990 PRO", "size_bytes": 4000000000000},
    {"name": "nvme2n1", "transport": "nvme", "model": "KIOXIA KCD6XLUL15T3", "size_bytes": 15000000000000},
    {"name": "nvme3n1", "transport": "nvme", "model": "KIOXIA KCD6XLUL15T3", "size_bytes": 15000000000000}
  ]
}`
}

// rs501aLayout is the shape both scripts emit, enough to inspect mddata and
// the mount set.
type rs501aLayout struct {
	Layout map[string]struct {
		Type      string   `json:"type"`
		Level     int      `json:"level"`
		Members   []string `json:"members"`
		FS        string   `json:"fs"`
		ArrayUUID string   `json:"array_uuid"`
	} `json:"layout"`
	Mounts map[string]struct {
		Device string `json:"device"`
	} `json:"mounts"`
}

// TestRS501A_ArrayUUIDMatchesGoDerivation proves rs501aExpectedArrayUUID
// really is uuidv5(rs501aSystemUUID, "mddata") — computed independently here
// via uuid.NewSHA1 (Go's equivalent of Python's uuid.uuid5) rather than
// trusting a hand-copied constant. If the script's derivation or the
// namespace/name convention ever drifts, this fails alongside the
// script-vs-script comparison tests, pointing at which side moved.
func TestRS501A_ArrayUUIDMatchesGoDerivation(t *testing.T) {
	t.Parallel()
	systemUUID := uuid.MustParse(rs501aSystemUUID)
	got := uuid.NewSHA1(systemUUID, []byte("mddata")).String()
	if got != rs501aExpectedArrayUUID {
		t.Fatalf("uuid.NewSHA1(%s, \"mddata\") = %s, want %s (fixture constant is stale)", rs501aSystemUUID, got, rs501aExpectedArrayUUID)
	}
}

func TestRS501ACreateScript_PinsDerivedArrayUUID(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "100-asus-rs501a")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(rs501aSystemUUID))
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}

	var layout rs501aLayout
	if err := json.Unmarshal([]byte(stdout), &layout); err != nil {
		t.Fatalf("decode layout: %v\nstdout: %s", err, stdout)
	}

	mddata, ok := layout.Layout["mddata"]
	if !ok {
		t.Fatal("layout missing mddata")
	}
	if mddata.Type != "raid" {
		t.Errorf("mddata.type = %q, want raid", mddata.Type)
	}
	if mddata.ArrayUUID != rs501aExpectedArrayUUID {
		t.Errorf("mddata.array_uuid = %q, want %q (uuidv5(system_uuid, \"mddata\"))", mddata.ArrayUUID, rs501aExpectedArrayUUID)
	}
	// The data disks are also listed as disk targets on the create script
	// (they get zapped + the array is created), unlike the preserve script.
	for _, want := range []string{"nvme2n1", "nvme3n1"} {
		if dev, ok := layout.Layout[want]; !ok || dev.Type != "disk" {
			t.Errorf("create script: expected %q as a disk-type device, got %+v (ok=%v)", want, dev, ok)
		}
	}
}

func TestRS501ACreateScript_MissingSystemUUID_Fails(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "100-asus-rs501a")
	_, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(""))
	if exit == 0 {
		t.Fatal("script should hard-fail when system.uuid is missing, got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "uuid") {
		t.Errorf("stderr should mention uuid, got: %s", stderr)
	}
}

func TestRS501APreserveScript_DerivesSameArrayUUIDAsCreate(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "150-asus-rs501a-preserve")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(rs501aSystemUUID))
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}

	var layout rs501aLayout
	if err := json.Unmarshal([]byte(stdout), &layout); err != nil {
		t.Fatalf("decode layout: %v\nstdout: %s", err, stdout)
	}

	mddata, ok := layout.Layout["mddata"]
	if !ok {
		t.Fatal("layout missing mddata")
	}
	if mddata.Type != "preserve" {
		t.Errorf("mddata.type = %q, want preserve", mddata.Type)
	}
	// THE critical property: both scripts must derive the identical UUID
	// for the same host, with no round-trip through a recommission.
	if mddata.ArrayUUID != rs501aExpectedArrayUUID {
		t.Errorf("preserve mddata.array_uuid = %q, want %q (must match the create script's derivation exactly)", mddata.ArrayUUID, rs501aExpectedArrayUUID)
	}

	// The Kioxia data disks must NOT appear as disk-type targets — a
	// preserve member that is also a disk target is a validation FATAL
	// (internal/storage/validate.go's validatePreserveMemberCollisions).
	for _, name := range []string{"nvme2n1", "nvme3n1"} {
		if _, ok := layout.Layout[name]; ok {
			t.Errorf("preserve script: data disk %q must not appear as a layout device (would collide with the preserve member)", name)
		}
	}
	if got := layout.Mounts["/data"].Device; got != "mddata" {
		t.Errorf("/data mounted from %q, want mddata", got)
	}
}

// rs501aBlockDevices mirrors rs501aInventoryJSON's disk shape as
// storage.BlockDevice, for feeding storage.Plan's inventory check.
var rs501aBlockDevices = []storage.BlockDevice{
	{Name: "nvme0n1", SizeBytes: 4000000000000, Transport: "nvme"},
	{Name: "nvme1n1", SizeBytes: 4000000000000, Transport: "nvme"},
	{Name: "nvme2n1", SizeBytes: 15000000000000, Transport: "nvme"},
	{Name: "nvme3n1", SizeBytes: 15000000000000, Transport: "nvme"},
}

// TestRS501ACreateScript_EmittedLayoutValidates feeds the create script's
// real stdout through storage.Plan (which validates before planning) to
// prove the emitted layout is not just well-shaped JSON but a layout the
// agent's planner actually accepts.
func TestRS501ACreateScript_EmittedLayoutValidates(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "100-asus-rs501a")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(rs501aSystemUUID))
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}
	var layout storage.Layout
	if err := json.Unmarshal([]byte(stdout), &layout); err != nil {
		t.Fatalf("decode layout: %v\nstdout: %s", err, stdout)
	}
	if _, err := storage.Plan(layout, rs501aBlockDevices); err != nil {
		t.Fatalf("storage.Plan rejected the create script's emitted layout: %v\nlayout: %s", err, stdout)
	}
}

// TestRS501APreserveScript_EmittedLayoutValidates is the preserve-script
// counterpart. Only the boot disks are in inventory (the two data disks are
// preserve members, not disk-type devices, so Plan's inventory check never
// looks for them — matching TestPlan_AcceptsValidPreserve's fixture shape in
// internal/storage/plan_test.go).
func TestRS501APreserveScript_EmittedLayoutValidates(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "150-asus-rs501a-preserve")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(rs501aSystemUUID))
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}
	var layout storage.Layout
	if err := json.Unmarshal([]byte(stdout), &layout); err != nil {
		t.Fatalf("decode layout: %v\nstdout: %s", err, stdout)
	}
	boot := []storage.BlockDevice{rs501aBlockDevices[0], rs501aBlockDevices[1]}
	if _, err := storage.Plan(layout, boot); err != nil {
		t.Fatalf("storage.Plan rejected the preserve script's emitted layout: %v\nlayout: %s", err, stdout)
	}
}

func TestRS501APreserveScript_MissingSystemUUID_Fails(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "150-asus-rs501a-preserve")
	_, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", rs501aInventoryJSON(""))
	if exit == 0 {
		t.Fatal("script should hard-fail when system.uuid is missing, got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "uuid") {
		t.Errorf("stderr should mention uuid, got: %s", stderr)
	}
}
