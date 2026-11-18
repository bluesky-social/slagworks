//go:build integration

package qemutest_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the REAL shipped MS-01 asset scripts
// (assets/storage/150-minisforum-ms-01, assets/network/150-pop3-router)
// directly: golden inventory/facts in, emitted layout/bundle out. They are
// the script-level unit tests; TestDeploy_MS01RouterStack (sibling file)
// proves the same scripts end-to-end inside a QEMU deploy.
//
// Motivated by the 2026-07-14 router1.pop3 reimage: the deploy reached
// awaiting_phone_home and the box never came back. These tests pin the
// pipeline pieces we can prove off-hardware, so the next hardware session
// starts from "the scripts are right" instead of re-litigating them.

// assetScriptPath returns the absolute path of a shipped asset script,
// relative to the repo root (two dirs up from internal/qemutest).
func assetScriptPath(t *testing.T, kind, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", kind, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("asset script not found at %s: %v", p, err)
	}
	return p
}

// netlibDir returns the directory holding slagworks_netlib.py so the
// shipped network scripts can `import slagworks_netlib` under test — the
// deploy agent co-locates the sidecar next to the script in production
// (internal/agent/netlib); PYTHONPATH is the test equivalent. Mirrors
// netlibDirSnapshot in internal/snapshot (different package, can't share).
func netlibDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(wd)), "internal", "agent", "netlib")
	if _, err := os.Stat(filepath.Join(dir, "slagworks_netlib.py")); err != nil {
		t.Fatalf("slagworks_netlib.py not found in %s: %v", dir, err)
	}
	return dir
}

// runAssetScript runs a shipped asset script with the given env var name
// pointed at a JSON fixture, returning stdout, stderr, and exit code. The
// netlib sidecar dir goes on PYTHONPATH (the agent co-locates the sidecar
// in production; PYTHONPATH is the test equivalent — same pattern as
// runL2Script).
func runAssetScript(t *testing.T, scriptPath, envVar, fixtureJSON string) (stdout, stderr string, exit int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	fixtureFile := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(fixtureFile, []byte(fixtureJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		envVar+"="+fixtureFile,
		"PYTHONPATH="+netlibDir(t))
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("running script: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), exit
}

// ms01RouterFacts is router1.pop3's NetBox interface facts shape: real
// names/MACs from the 2026-07-14 commission, OOB on the I226-LM (enp90s0,
// recabled 2026-07-15) at its 10.252.240.0/22 address (legacy /24 retired
// 2026-10-19).
const ms01RouterFacts = `[
  {"name": "lo", "type": "physical", "ip": "10.252.31.1/32", "mac": ""},
  {"name": "enp88s0", "type": "physical", "ip": "", "mac": "58:47:ca:7f:7e:e2"},
  {"name": "enp90s0", "type": "physical", "ip": "10.252.240.23/22", "mac": "58:47:ca:7f:7e:e3"},
  {"name": "enp3s0f0np0", "type": "physical", "ip": "10.252.25.9/31", "mac": "58:47:ca:7f:7e:e0"},
  {"name": "enp3s0f1np1", "type": "physical", "ip": "10.252.25.13/31", "mac": "58:47:ca:7f:7e:e1"},
  {"name": "enp1s0f0", "type": "physical", "ip": "155.204.43.106/30", "mac": "98:b7:85:1f:35:0e"},
  {"name": "enp1s0f1", "type": "physical", "ip": "155.204.43.110/30", "mac": "98:b7:85:1f:35:0f"}
]`

func TestPop3RouterScript_RendersTransitionalNetplan(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "network", "150-pop3-router")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_NETWORK_FACTS", ms01RouterFacts)
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}

	var bundle struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Mode    string `json:"mode"`
		} `json:"files"`
		EnableUnits []string `json:"enable_units"`
	}
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("decode bundle: %v\nstdout: %s", err, stdout)
	}
	if len(bundle.Files) != 1 {
		t.Fatalf("expected exactly 1 bundle file, got %d", len(bundle.Files))
	}
	f := bundle.Files[0]
	if f.Path != "/etc/netplan/99-slagworks.yaml" {
		t.Errorf("bundle path = %q, want /etc/netplan/99-slagworks.yaml", f.Path)
	}
	if f.Mode != "0600" {
		t.Errorf("bundle mode = %q, want 0600 (netplan warns on world-readable)", f.Mode)
	}
	if len(bundle.EnableUnits) != 0 {
		t.Errorf("expected no enable_units (FRR is ansible-owned on the router), got %v", bundle.EnableUnits)
	}

	np := f.Content
	// OOB leg: MAC-matched, static address, transitional default via the
	// pop3 OOB VIP, public resolvers.
	for _, want := range []string{
		`macaddress: "58:47:ca:7f:7e:e3"`,
		`set-name: enp90s0`,
		`- "10.252.240.23/22"`,
		"via: 10.252.240.1",
		// on-link keeps networkd from wedging the link in `configuring`
		// (blocking wait-online -> network-online -> phone-home) when the
		// modeled OOB prefix doesn't contain the gateway.
		"on-link: true",
		"addresses: [1.1.1.1, 1.0.0.1]",
		// Both /31 leaf downlinks at fabric MTU.
		`- "10.252.25.9/31"`,
		`- "10.252.25.13/31"`,
		"mtu: 9000",
		// Identity loopback.
		`- "10.252.31.1/32"`,
	} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q\n---\n%s", want, np)
		}
	}

	// THE WAN EXCLUSION: the EIA NICs carry public /30s and must not be
	// rendered — an addressed-but-unfirewalled transit interface before
	// ansible's nftables converge would expose the box.
	for _, banned := range []string{"155.204.", "98:b7:85", "enp1s0f0", "enp1s0f1"} {
		if strings.Contains(np, banned) {
			t.Errorf("netplan leaked WAN material %q — EIA legs must stay unrendered until ansible\n---\n%s", banned, np)
		}
	}
	// The IP-less 2.5G spare (enp88s0, the I226-V; OOB moved to the
	// AMT-capable I226-LM) must not render either.
	if strings.Contains(np, "enp88s0") {
		t.Errorf("netplan rendered the unused enp88s0 spare\n---\n%s", np)
	}
}

func TestPop3RouterScript_HardFailsWhenNotL3Modeled(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "network", "150-pop3-router")
	// One leg, no loopback: the incomplete-model shape the script must
	// refuse (coming up DHCP-only would strand the router off-fabric).
	facts := `[
	  {"name": "enp90s0", "type": "physical", "ip": "10.252.240.23/22", "mac": "58:47:ca:7f:7e:e3"},
	  {"name": "enp3s0f0np0", "type": "physical", "ip": "10.252.25.9/31", "mac": "58:47:ca:7f:7e:e0"}
	]`
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_NETWORK_FACTS", facts)
	if exit == 0 {
		t.Fatalf("script should hard-fail on an incomplete L3 model, got exit 0\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "not fully L3-modeled") {
		t.Errorf("stderr should name the failure, got: %s", stderr)
	}
}

// ms01Inventory mirrors the QEMU MS-01 cell's disk shape: the harness's
// mandatory primary virtio disk plus the two NVMe drives the real box has.
// The script must select exactly the NVMe pair and ignore the virtio disk.
const ms01Inventory = `{"disks": [
  {"name": "vda", "transport": "virtio", "model": "", "size_bytes": 4294967296},
  {"name": "nvme0n1", "transport": "nvme", "model": "Samsung SSD 990 PRO 2TB", "size_bytes": 2000398934016},
  {"name": "nvme1n1", "transport": "nvme", "model": "Samsung SSD 990 PRO 2TB", "size_bytes": 2000398934016}
]}`

func TestMS01StorageScript_RAID1AcrossNVMe(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "150-minisforum-ms-01")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", ms01Inventory)
	if exit != 0 {
		t.Fatalf("script exited %d\nstderr: %s", exit, stderr)
	}

	var layout struct {
		Layout map[string]json.RawMessage `json:"layout"`
		Mounts map[string]struct {
			Device string `json:"device"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal([]byte(stdout), &layout); err != nil {
		t.Fatalf("decode layout: %v\nstdout: %s", err, stdout)
	}

	for _, disk := range []string{"nvme0n1", "nvme1n1", "mdroot"} {
		if _, ok := layout.Layout[disk]; !ok {
			t.Errorf("layout missing %q", disk)
		}
	}
	if _, ok := layout.Layout["vda"]; ok {
		t.Error("layout touched the virtio disk — must select NVMe only")
	}
	if got := layout.Mounts["/"].Device; got != "mdroot" {
		t.Errorf("/ mounted from %q, want mdroot", got)
	}
	// NVMe partition naming uses the "p" separator; a bare "nvme0n12"
	// would brick the layout at partition time.
	if got := layout.Mounts["/boot/efi"].Device; got != "nvme0n1p1" {
		t.Errorf("/boot/efi mounted from %q, want nvme0n1p1", got)
	}
	// mdroot must mirror the p2 partitions of both drives.
	var md struct {
		Level   int      `json:"level"`
		Members []string `json:"members"`
		FS      string   `json:"fs"`
	}
	if err := json.Unmarshal(layout.Layout["mdroot"], &md); err != nil {
		t.Fatalf("decode mdroot: %v", err)
	}
	if md.Level != 1 || md.FS != "xfs" {
		t.Errorf("mdroot level=%d fs=%q, want level=1 fs=xfs", md.Level, md.FS)
	}
	wantMembers := []string{"nvme0n1p2", "nvme1n1p2"}
	if len(md.Members) != 2 || md.Members[0] != wantMembers[0] || md.Members[1] != wantMembers[1] {
		t.Errorf("mdroot members = %v, want %v", md.Members, wantMembers)
	}
}

func TestMS01StorageScript_HardFailsOnWrongDiskCount(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "storage", "150-minisforum-ms-01")
	oneDisk := `{"disks": [
	  {"name": "nvme0n1", "transport": "nvme", "model": "Samsung SSD 990 PRO 2TB", "size_bytes": 2000398934016}
	]}`
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_INVENTORY", oneDisk)
	if exit == 0 {
		t.Fatalf("script should hard-fail on 1 NVMe (failed drive / wrong box), got exit 0\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "expected exactly 2 NVMe") {
		t.Errorf("stderr should name the failure, got: %s", stderr)
	}
}
