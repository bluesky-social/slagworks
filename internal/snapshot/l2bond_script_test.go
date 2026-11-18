package snapshot

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/configscript"
)

// These tests exercise the legacy pop2 L2 bond network-config script
// (assets/network/100-hot-spare-pop2) directly: they feed it golden facts via
// SLAGWORKS_NETWORK_FACTS, the same contract the deploy agent uses, and assert
// the emitted netplan + the loud-fail behavior. They are the script's unit
// tests; the full QEMU LACP integration test
// (internal/qemutest/lacp_bond_integration_test.go) proves the rendered
// netplan actually forms an 802.3ad bond against an LACP partner.
//
// We invoke the real interpreter (python3) rather than re-implementing the
// logic so the test pins the actual shipped artifact. Skips if python3 is
// absent (it is present on CI and dev laptops).

// netlibDirSnapshot returns the directory holding slagworks_netlib.py so this
// test can put it on PYTHONPATH — the shipped script `import`s it, and the
// deploy agent co-locates it next to the script (internal/agent/netlib).
func netlibDirSnapshot(t *testing.T) string {
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

// l2BondScriptPath returns the absolute path to the L2 bond script, relative
// to the repo root (two dirs up from internal/snapshot).
func l2BondScriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/snapshot -> repo root
	root := filepath.Dir(filepath.Dir(wd))
	p := filepath.Join(root, "assets", "network", "100-hot-spare-pop2")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("L2 bond script not found at %s: %v", p, err)
	}
	return p
}

// runL2Script runs the script with the given facts JSON and returns stdout,
// stderr, and the exit code.
func runL2Script(t *testing.T, factsJSON string) (stdout, stderr string, exit int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	factsFile := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(factsFile, []byte(factsJSON), 0o644); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	cmd := exec.Command("python3", l2BondScriptPath(t))
	cmd.Env = append(os.Environ(),
		"SLAGWORKS_NETWORK_FACTS="+factsFile,
		"PYTHONPATH="+netlibDirSnapshot(t))
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

type netBundleFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
	Owner   string `json:"owner"`
	Group   string `json:"group"`
}

type netBundle struct {
	Files       []netBundleFile `json:"files"`
	EnableUnits []string        `json:"enable_units"`
}

// hotSpare1Facts is the golden facts blob matching hot-spare-1.pop2 as the
// builder produces it (bond0 type=lag carrying the host IP/gateway/MTU, two
// members lag=bond0, an ipmi iface). Mirrors the NetBox recon in the plan doc.
const hotSpare1Facts = `[
  {"netbox_id": 10, "name": "bond0", "mac": "", "type": "lag", "ip": "10.253.0.22/22", "vlan": 1, "gateway": "10.253.3.254", "mtu": 1500},
  {"netbox_id": 11, "name": "enp129s0f0np0", "mac": "7c:c2:55:42:fc:30", "type": "physical", "lag": "bond0", "mtu": 1500},
  {"netbox_id": 12, "name": "enp129s0f1np1", "mac": "7c:c2:55:42:fc:31", "type": "physical", "lag": "bond0", "mtu": 1500},
  {"netbox_id": 13, "name": "ipmi0", "mac": "3c:ec:ef:de:d0:89", "type": "ipmi", "ip": "10.253.241.108/23"}
]`

func TestL2BondScript_GoldenNetplan(t *testing.T) {
	stdout, stderr, exit := runL2Script(t, hotSpare1Facts)
	if exit != 0 {
		t.Fatalf("script exited %d, stderr:\n%s", exit, stderr)
	}
	var b netBundle
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatalf("bundle JSON: %v\nstdout:\n%s", err, stdout)
	}
	if len(b.Files) != 1 {
		t.Fatalf("want 1 file (netplan only, no FRR), got %d: %+v", len(b.Files), b.Files)
	}
	if len(b.EnableUnits) != 0 {
		t.Errorf("want no enable_units (no FRR on L2), got %v", b.EnableUnits)
	}
	f := b.Files[0]
	if f.Path != "/etc/netplan/99-slagworks.yaml" {
		t.Errorf("netplan path = %q", f.Path)
	}
	if f.Mode != "0600" {
		t.Errorf("netplan mode = %q, want 0600", f.Mode)
	}
	np := f.Content

	// LACP policy must be exactly the leaf bond34 shape.
	for _, want := range []string{
		"mode: 802.3ad",
		"lacp-rate: fast",
		"transmit-hash-policy: layer3+4",
		"mii-monitor-interval: 100",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q\n%s", want, np)
		}
	}
	// Both members present as match-by-MAC + set-name, enslaved to the bond.
	for _, want := range []string{
		`macaddress: "7c:c2:55:42:fc:30"`,
		`macaddress: "7c:c2:55:42:fc:31"`,
		"set-name: enp129s0f0np0",
		"set-name: enp129s0f1np1",
		"- enp129s0f0np0",
		"- enp129s0f1np1",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q\n%s", want, np)
		}
	}
	// Bond carries the host IP/CIDR directly (untagged VLAN -> no subinterface).
	if !strings.Contains(np, `- "10.253.0.22/22"`) {
		t.Errorf("netplan missing bond address 10.253.0.22/22\n%s", np)
	}
	// Default route via the gateway.
	if !strings.Contains(np, "to: default") || !strings.Contains(np, "via: 10.253.3.254") {
		t.Errorf("netplan missing default route via 10.253.3.254\n%s", np)
	}
	// MTU 1500 on members and bond (NOT 9000).
	if !strings.Contains(np, "mtu: 1500") {
		t.Errorf("netplan missing mtu 1500\n%s", np)
	}
	if strings.Contains(np, "mtu: 9000") {
		t.Errorf("netplan must not use the L3-fabric 9000 MTU\n%s", np)
	}
	// Untagged VLAN sits the address on the bond; no vlans: stanza.
	if strings.Contains(np, "vlans:") {
		t.Errorf("untagged VLAN must NOT create a vlans: subinterface stanza\n%s", np)
	}
	// Nameserver derived from the gateway (oob-gw VIP convention .240.1).
	if !strings.Contains(np, "10.253.240.1") {
		t.Errorf("netplan missing deploy-window resolver 10.253.240.1\n%s", np)
	}
	// No FRR artifacts.
	if strings.Contains(np, "frr") || strings.Contains(strings.ToLower(np), "bgp") {
		t.Errorf("L2 netplan must not reference FRR/BGP\n%s", np)
	}
}

// TestL2BondScript_MTUDefault: when the bond facts carry no MTU, the script
// defaults to 1500 (NOT the L3 9000) - a wrong MTU on a 1500 segment
// black-holes traffic.
func TestL2BondScript_MTUDefault(t *testing.T) {
	facts := `[
  {"name": "bond0", "type": "lag", "ip": "10.253.0.22/22", "gateway": "10.253.3.254"},
  {"name": "m0", "mac": "aa:bb:cc:00:00:01", "type": "physical", "lag": "bond0"},
  {"name": "m1", "mac": "aa:bb:cc:00:00:02", "type": "physical", "lag": "bond0"}
]`
	stdout, stderr, exit := runL2Script(t, facts)
	if exit != 0 {
		t.Fatalf("script exited %d, stderr:\n%s", exit, stderr)
	}
	if !strings.Contains(stdout, "mtu: 1500") {
		t.Errorf("default MTU should be 1500\n%s", stdout)
	}
	if strings.Contains(stdout, "mtu: 9000") {
		t.Errorf("must not default to 9000\n%s", stdout)
	}
}

// TestL2BondScript_SingleMemberRendersWithWarning: one member is degraded-
// valid (802.3ad runs single-link); render it and WARN, do not hard-fail.
func TestL2BondScript_SingleMemberRendersWithWarning(t *testing.T) {
	facts := `[
  {"name": "bond0", "type": "lag", "ip": "10.253.0.22/22", "gateway": "10.253.3.254", "mtu": 1500},
  {"name": "m0", "mac": "aa:bb:cc:00:00:01", "type": "physical", "lag": "bond0", "mtu": 1500}
]`
	stdout, stderr, exit := runL2Script(t, facts)
	if exit != 0 {
		t.Fatalf("single member should render (degraded-valid), got exit %d stderr:\n%s", exit, stderr)
	}
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "ONE member") {
		t.Errorf("expected a single-member WARNING on stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stdout, "mode: 802.3ad") {
		t.Errorf("single-member bond should still render 802.3ad\n%s", stdout)
	}
	if !strings.Contains(stdout, "set-name: m0") {
		t.Errorf("single-member bond should render its one member\n%s", stdout)
	}
}

// TestL2BondScript_HardFailZeroMembers: a bond with no members is broken; the
// script aborts the deploy rather than coming up DHCP-only.
func TestL2BondScript_HardFailZeroMembers(t *testing.T) {
	facts := `[
  {"name": "bond0", "type": "lag", "ip": "10.253.0.22/22", "gateway": "10.253.3.254"}
]`
	stdout, stderr, exit := runL2Script(t, facts)
	if exit == 0 {
		t.Fatalf("zero members must hard-fail, got exit 0\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no member interfaces") {
		t.Errorf("expected a no-members error, got:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("must emit no bundle on hard-fail, got stdout:\n%s", stdout)
	}
}

// TestL2BondScript_HardFailNoGateway: no default route = host effectively off
// the network; loud-fail, do not render.
func TestL2BondScript_HardFailNoGateway(t *testing.T) {
	facts := `[
  {"name": "bond0", "type": "lag", "ip": "10.253.0.22/22"},
  {"name": "m0", "mac": "aa:bb:cc:00:00:01", "type": "physical", "lag": "bond0"},
  {"name": "m1", "mac": "aa:bb:cc:00:00:02", "type": "physical", "lag": "bond0"}
]`
	stdout, stderr, exit := runL2Script(t, facts)
	if exit == 0 {
		t.Fatalf("missing gateway must hard-fail, got exit 0\nstdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no gateway") {
		t.Errorf("expected a no-gateway error, got:\n%s", stderr)
	}
}

// TestL2BondScript_HardFailNoBond: no type=lag interface with an IP -> the
// host isn't bond-modeled; abort.
func TestL2BondScript_HardFailNoBond(t *testing.T) {
	facts := `[
  {"name": "eno1", "mac": "aa:bb:cc:00:00:01", "type": "physical", "ip": "10.253.0.22/22"}
]`
	_, stderr, exit := runL2Script(t, facts)
	if exit == 0 {
		t.Fatalf("no bond interface must hard-fail, got exit 0")
	}
	if !strings.Contains(stderr, "exactly one bond interface") {
		t.Errorf("expected a no-bond error, got:\n%s", stderr)
	}
}

// TestL2BondScript_Matching pins the match scope: the L2 bond script matches
// hot-spare-1/2.pop2 (site=pop2 + location=pop2-01-005330 old cage + the
// hostname regex backstop) and must NOT catch the modern pop1 L3 kworkers nor
// hosts in the pop2 NEW cage. Uses the real script headers through the same
// ParseNetworkScript + Match path snapshot.Build uses. The script
// keys on location (the primary cage discriminator) AND the hostname regex
// (defense-in-depth), so the resolver must supply location too.
func TestL2BondScript_Matching(t *testing.T) {
	body, err := os.ReadFile(l2BondScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script, err := ParseNetworkScript("100-hot-spare-pop2", body)
	if err != nil {
		t.Fatalf("parse script headers: %v", err)
	}

	const oldCage = "pop2-01-005330"
	match := func(site, location, hostname string) bool {
		resolve := func(field string) string {
			switch field {
			case "site":
				return site
			case "location":
				return location
			case "hostname":
				return hostname
			}
			return ""
		}
		got, err := configscript.Match([]configscript.Script{*script}, resolve)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		return got != nil
	}

	cases := []struct {
		name                     string
		site, location, hostname string
		want                     bool
	}{
		{"old-cage hot-spare 1", "pop2", oldCage, "hot-spare-1.pop2", true},
		{"old-cage hot-spare 2", "pop2", oldCage, "hot-spare-2.pop2", true},
		// wrong site
		{"wrong site", "pop1", oldCage, "hot-spare-1.pop2", false},
		// NEW cage location must NOT match the old-cage script, even for a
		// hostname that satisfies the regex — the location key is what splits
		// the two cages now.
		{"new-cage location", "pop2", "pop2-01-005250", "hot-spare-1.pop2", false},
		// no location resolved (e.g. unmodeled) must NOT match.
		{"no location", "pop2", "", "hot-spare-1.pop2", false},
		// L3 kworkers must NOT match (different hostname; also different site).
		{"pop1 kworker", "pop1", "pop1-02-004730", "kworker-l34-1.pop1", false},
		{"pop2 kworker-named", "pop2", oldCage, "kworker-l34-1.pop1", false},
		// out-of-range index / other old-cage hosts not in the hot-spare set.
		{"out-of-range index", "pop2", oldCage, "hot-spare-3.pop2", false},
		{"old-cage oob-gw", "pop2", oldCage, "oob-gw-1.pop2", false},
	}
	for _, c := range cases {
		if got := match(c.site, c.location, c.hostname); got != c.want {
			t.Errorf("%s: match(site=%s, loc=%s, host=%s) = %v, want %v",
				c.name, c.site, c.location, c.hostname, got, c.want)
		}
	}
}
