//go:build !integration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The SN2700 network-config script (assets/network/200-sn2700-switch) is
// Python run in the deploy initrd. These tests run it the agent's way — facts
// to a file, SLAGWORKS_NETWORK_FACTS pointed at it, capture stdout — and
// assert the emitted netplan. Regression guard: the mgmt NIC must
// be matched by MAC (not name), because the deployed OS renames eth0->eno1 and
// an mlxsw fabric port also transiently claims eth0.

func sn2700ScriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "assets", "network", "200-sn2700-switch")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sn2700 script not found at %s: %v", path, err)
	}
	return path
}

func runSN2700Script(t *testing.T, facts string) (map[string]any, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	factsFile := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(factsFile, []byte(facts), 0644); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	cmd := exec.Command("python3", sn2700ScriptPath(t))
	cmd.Env = append(os.Environ(),
		"SLAGWORKS_NETWORK_FACTS="+factsFile,
		"PYTHONPATH="+netlibDir(t))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, stderr.String(), err
	}
	var bundle map[string]any
	if jerr := json.Unmarshal(out, &bundle); jerr != nil {
		t.Fatalf("bundle is not valid JSON: %v\nstdout: %s", jerr, out)
	}
	return bundle, stderr.String(), nil
}

// leaf6Facts is the real leaf-6.pop1 shape: two on-board 1000base-t mgmt NICs
// (eno1/ens6) plus the mlxsw Spectrum fabric (enp3s0np1..N) and LAG bonds.
// NetBox names the mgmt NICs eno1/ens6 already, but the kernel calls the
// mgmt NIC eth0 in the initrd — the point of the fix is to NOT depend on name.
const leaf6Facts = `[
  {"name":"bond_sp1","mac":"20:4d:52:26:00:11","type":"lag","ip":"10.254.24.62/30"},
  {"name":"eno1","mac":"02:00:00:00:06:01","type":"physical","ip":"10.254.240.36/22"},
  {"name":"ens6","mac":"02:00:00:00:06:02","type":"physical","ip":""},
  {"name":"enp3s0np1","mac":"90:0a:84:44:e9:7d","type":"physical","ip":""},
  {"name":"enp3s0np9","mac":"90:0a:84:44:e9:6d","type":"physical","ip":"10.254.29.6/31"},
  {"name":"enp3s0np17","mac":"90:0a:84:44:e9:41","type":"physical","ip":""}
]`

// TestSN2700_MatchesMgmtByMAC is a regression test: the mgmt NICs must be
// matched by MAC + set-name, and the fabric ports must be left out entirely.
func TestSN2700_MatchesMgmtByMAC(t *testing.T) {
	bundle, stderr, err := runSN2700Script(t, leaf6Facts)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}
	f := bundleFile(t, bundle, "/etc/netplan/99-slagworks.yaml")
	content, _ := f["content"].(string)

	// Both on-board mgmt NICs matched by MAC.
	for _, mac := range []string{"02:00:00:00:06:01", "02:00:00:00:06:02"} {
		if !strings.Contains(content, `macaddress: "`+mac+`"`) {
			t.Errorf("mgmt NIC %s not matched by MAC; netplan:\n%s", mac, content)
		}
	}
	// Matched by MAC, never by the fragile eth* name glob.
	if strings.Contains(content, `name: "eth`) || strings.Contains(content, "eth*") {
		t.Errorf("netplan must not match by eth* name:\n%s", content)
	}
	// Must NOT set-name: the NIC keeps its predictable name (eno1/ens6) so
	// the post-boot site Ansible leaf_l3 ansible (keys on ansible_facts[eno1])
	// isn't broken by a rename. The stanza key is the NetBox name.
	if strings.Contains(content, "set-name") {
		t.Errorf("netplan must NOT set-name (would break ansible's eno1 assumption):\n%s", content)
	}
	if !strings.Contains(content, "nic-eno1:") || !strings.Contains(content, "nic-ens6:") {
		t.Errorf("expected eno1/ens6 stanza keys (NetBox names, no rename):\n%s", content)
	}
	// dhcp4 on the mgmt NICs.
	if !strings.Contains(content, "dhcp4: true") {
		t.Errorf("expected dhcp4 on mgmt NICs:\n%s", content)
	}
	// The cabled NIC (eno1, has the OOB IP) must be required-for-online
	// (gates network-online.target); the uncabled second NIC (ens6, no IP)
	// must be optional so it doesn't block wait-online ~120s. Expect exactly
	// ONE optional block.
	if got := strings.Count(content, "optional: true"); got != 1 {
		t.Errorf("expected exactly 1 optional NIC (the no-IP ens6), got %d:\n%s", got, content)
	}
	// Fabric ports + bonds must NOT appear (left for ansible).
	for _, fab := range []string{"90:0a:84:44:e9:7d", "90:0a:84:44:e9:6d", "90:0a:84:44:e9:41", "20:4d:52:26:00:11"} {
		if strings.Contains(content, fab) {
			t.Errorf("fabric/bond MAC %s leaked into mgmt netplan:\n%s", fab, content)
		}
	}
}

// TestSN2700_HardFailsWhenNoMgmtNIC: a model with only fabric/lag interfaces
// (no on-board mgmt NIC) must hard-fail rather than emit an empty netplan that
// would silently strand the switch.
func TestSN2700_HardFailsWhenNoMgmtNIC(t *testing.T) {
	factsNoMgmt := `[
	  {"name":"enp3s0np1","mac":"90:0a:84:44:e9:7d","type":"physical","ip":""},
	  {"name":"bond_sp1","mac":"20:4d:52:26:00:11","type":"lag","ip":"10.254.24.62/30"}
	]`
	_, stderr, err := runSN2700Script(t, factsNoMgmt)
	if err == nil {
		t.Fatal("expected hard-fail when no on-board mgmt NIC present")
	}
	if !strings.Contains(stderr, "no on-board mgmt NIC") {
		t.Errorf("expected a clear no-mgmt-NIC error, got: %s", stderr)
	}
}
