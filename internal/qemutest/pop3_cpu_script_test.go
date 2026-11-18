//go:build integration

package qemutest_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests exercise the REAL shipped pop3 cpu-host asset script
// (assets/network/100-pop3-cpu) directly: golden NetBox interface facts in,
// emitted netplan bundle out. Same pattern as TestPop3RouterScript_* in
// ms01_router_script_test.go (sibling file) — script-level unit tests, no
// qemu required.

// pop3CpuFacts is cpu2.pop3's real NetBox interface facts shape: eth0 is the
// OOB NIC, eth2/eth3 are the routed /31 leaf legs, lo carries the identity
// loopback, ipmi is the BMC (excluded by name), eth1 is unmodeled (no IP,
// excluded by the OOB-NIC picker's "has ip" filter).
const pop3CpuFacts = `[
  {"name": "eth0", "type": "physical", "ip": "10.252.1.124/24", "mac": "10:7c:61:58:ff:5b"},
  {"name": "eth1", "type": "physical", "ip": "", "mac": "10:7c:61:58:ff:5c"},
  {"name": "eth2", "type": "physical", "ip": "10.252.25.133/31", "mac": "b8:e9:24:ee:a7:12"},
  {"name": "eth3", "type": "physical", "ip": "10.252.25.135/31", "mac": "b8:e9:24:ee:a7:13"},
  {"name": "lo", "type": "physical", "ip": "10.252.31.102/32", "mac": ""},
  {"name": "ipmi", "type": "physical", "ip": "10.252.1.181/24", "mac": "10:7c:61:58:ff:5d"}
]`

func TestPop3CpuScript_RendersTransitionalNetplan(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "network", "100-pop3-cpu")
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_NETWORK_FACTS", pop3CpuFacts)
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
		t.Errorf("expected no enable_units (FRR is ansible-owned on the cpu host), got %v", bundle.EnableUnits)
	}

	np := f.Content
	for _, want := range []string{
		`macaddress: "10:7c:61:58:ff:5b"`,
		"via: 10.252.244.11",
		// on-link keeps networkd from wedging the link in `configuring`
		// (blocking wait-online -> network-online -> phone-home).
		"on-link: true",
		`- "10.252.25.133/31"`,
		`- "10.252.25.135/31"`,
		"mtu: 9000",
		`- "10.252.31.102/32"`,
	} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q\n---\n%s", want, np)
		}
	}

}

func TestPop3CpuScript_HardFailsWhenNotL3Modeled(t *testing.T) {
	t.Parallel()
	script := assetScriptPath(t, "network", "100-pop3-cpu")
	// Both /31 legs present, but no loopback /32 — the incomplete-model shape
	// the script must refuse.
	facts := `[
	  {"name": "eth0", "type": "physical", "ip": "10.252.1.124/24", "mac": "10:7c:61:58:ff:5b"},
	  {"name": "eth2", "type": "physical", "ip": "10.252.25.133/31", "mac": "b8:e9:24:ee:a7:12"},
	  {"name": "eth3", "type": "physical", "ip": "10.252.25.135/31", "mac": "b8:e9:24:ee:a7:13"}
	]`
	stdout, stderr, exit := runAssetScript(t, script, "SLAGWORKS_NETWORK_FACTS", facts)
	if exit == 0 {
		t.Fatalf("script should hard-fail on a missing loopback, got exit 0\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "loopback") {
		t.Errorf("stderr should mention the loopback, got: %s", stderr)
	}
}
