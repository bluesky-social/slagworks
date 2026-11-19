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

// The pop1 L3 network-config script (assets/network/100-pop1-l3-kworker) is
// Python executed in the deploy initrd. These tests run it the same way the
// agent does — write facts to a file, point SLAGWORKS_NETWORK_FACTS at it,
// capture stdout — and assert the emitted bundle. They are the regression
// guard for topology-decode bugs (e.g. the leg-sort / leaf-ASN-swap class)
// that are silent and host-specific and would otherwise only surface as a
// BGP-won't-establish symptom on a live re-image.

// netlibDir returns the directory holding slagworks_netlib.py, so tests that
// exec a raw network script can put it on PYTHONPATH — mirroring how the deploy
// agent co-locates the lib next to the script (execkit.RunConfigScript /
// internal/agent/netlib). The scripts `import slagworks_netlib`, so without this
// they'd fail with ModuleNotFoundError.
func netlibDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := filepath.Join(wd, "..", "..", "internal", "agent", "netlib")
	if _, err := os.Stat(filepath.Join(dir, "slagworks_netlib.py")); err != nil {
		t.Fatalf("slagworks_netlib.py not found in %s: %v", dir, err)
	}
	return dir
}

func pop1ScriptPath(t *testing.T) string {
	t.Helper()
	// repo root is two levels up from cmd/slagworks-agent.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "assets", "network", "100-pop1-l3-kworker")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pop1 script not found at %s: %v", path, err)
	}
	return path
}

// runPop1Script executes the script with the given facts JSON and returns the
// parsed bundle plus raw stderr. A non-nil err means the script exited
// non-zero (the hard-fail path).
func runPop1Script(t *testing.T, facts string) (map[string]any, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	factsFile := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(factsFile, []byte(facts), 0644); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	cmd := exec.Command("python3", pop1ScriptPath(t))
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

// bundleFile returns the file entry with the given path, or fails.
func bundleFile(t *testing.T, bundle map[string]any, path string) map[string]any {
	t.Helper()
	files, _ := bundle["files"].([]any)
	for _, f := range files {
		fm, _ := f.(map[string]any)
		if fm["path"] == path {
			return fm
		}
	}
	t.Fatalf("bundle has no file %q; files: %v", path, bundle["files"])
	return nil
}

// l34Facts: kworker-l34-1 — legs .1/.3 on 10.254.28, loopback .97. This pair
// happens to sort correctly either lexically or numerically.
const l34Facts = `[
  {"name":"eth1","mac":"7C:C2:55:BE:F7:99","type":"physical","ip":"10.254.28.3/31"},
  {"name":"eth0","mac":"7C:C2:55:BE:F7:98","type":"physical","ip":"10.254.28.1/31"},
  {"name":"lo","type":"physical","ip":"10.254.31.97/32"},
  {"name":"ipmi0","mac":"90:5a:08:15:cc:75","type":"ipmi","ip":"10.254.241.9/22"}
]`

// l56Facts: kworker-l56-3 — legs .9/.11 on 10.254.29, loopback .67. THIS is
// the pair that exposes the lexical-vs-numeric sort bug (".11" < ".9" as
// strings) and is the plan's designated validation host.
const l56Facts = `[
  {"name":"eth0","mac":"aa:bb:cc:00:00:01","type":"physical","ip":"10.254.29.9/31"},
  {"name":"eth1","mac":"aa:bb:cc:00:00:02","type":"physical","ip":"10.254.29.11/31"},
  {"name":"lo","type":"physical","ip":"10.254.31.67/32"}
]`

// TestPop1Script_L56_LeafASNMapping is the regression guard for the leg-sort /
// leaf-ASN-swap bug: legs must sort NUMERICALLY so the lower /31 is leaf-A.
// For l56-3 (.9/.11) a lexical sort put .11 first and swapped the ASNs, so the
// configured remote-as mismatched the peer and BGP would never establish.
func TestPop1Script_L56_LeafASNMapping(t *testing.T) {
	bundle, stderr, err := runPop1Script(t, l56Facts)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}
	frr := bundleFile(t, bundle, "/etc/frr/frr.conf")["content"].(string)

	// leaf-A is the lower leg (.8, host .9) = leaf-5 = ASN 65025.
	// leaf-B is the higher leg (.10, host .11) = leaf-6 = ASN 65026.
	for _, want := range []string{
		"neighbor 10.254.29.8 remote-as 65025",
		"neighbor 10.254.29.10 remote-as 65026",
		"set ip next-hop 10.254.29.9",  // TO_LEAF_A rewrites to leg-A host
		"set ip next-hop 10.254.29.11", // TO_LEAF_B rewrites to leg-B host
		"bgp router-id 10.254.31.67",
		"router bgp 64513",
	} {
		if !strings.Contains(frr, want) {
			t.Errorf("frr.conf missing %q:\n%s", want, frr)
		}
	}
	// The swap symptom: .8 must NOT be given leaf-6's ASN.
	if strings.Contains(frr, "neighbor 10.254.29.8 remote-as 65026") {
		t.Error("leaf ASN swapped: .8 (leaf-5) got leaf-6's ASN 65026")
	}
}

// TestPop1Script_L34_FullBundle pins the full bundle shape for a normal host:
// netplan + frr.conf (owned frr:frr) + daemons (bgpd/bfdd on) + sysctl, and
// frr.service enabled.
func TestPop1Script_L34_FullBundle(t *testing.T) {
	bundle, stderr, err := runPop1Script(t, l34Facts)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}

	netplan := bundleFile(t, bundle, "/etc/netplan/99-slagworks.yaml")
	if netplan["mode"] != "0600" {
		t.Errorf("netplan mode = %v, want 0600", netplan["mode"])
	}

	frr := bundleFile(t, bundle, "/etc/frr/frr.conf")
	if frr["owner"] != "frr" || frr["group"] != "frr" {
		t.Errorf("frr.conf must be owned frr:frr, got owner=%v group=%v", frr["owner"], frr["group"])
	}
	if !strings.Contains(frr["content"].(string), "neighbor 10.254.28.0 remote-as 65023") {
		t.Error("l34-1 leaf-A should be leaf-3 (65023) at .0")
	}

	daemons := bundleFile(t, bundle, "/etc/frr/daemons")["content"].(string)
	for _, want := range []string{"bgpd=yes", "bfdd=yes", "zebra_options=", "bgpd_options="} {
		if !strings.Contains(daemons, want) {
			t.Errorf("daemons missing %q", want)
		}
	}
	if strings.Contains(daemons, "bgpd=no") {
		t.Error("daemons still has bgpd=no")
	}

	bundleFile(t, bundle, "/etc/sysctl.d/90-l3-multipath.conf")

	units, _ := bundle["enable_units"].([]any)
	if len(units) != 1 || units[0] != "frr.service" {
		t.Errorf("enable_units = %v, want [frr.service]", bundle["enable_units"])
	}
}

// TestPop1Script_UnmodeledHostHardFails: a host matched by the pop1 L3 script
// but NOT fully L3-modeled (no /31 legs, no loopback) must HARD-FAIL, not
// silently fall back to DHCP. This is the loud-fail-over-silent-wrong-state
// fix for the l56-3 class of bug: a kworker that's supposed to be on the L3
// fabric coming up DHCP-only is a latent break, so the deploy aborts and the
// operator fixes the NetBox modeling.
func TestPop1Script_UnmodeledHostHardFails(t *testing.T) {
	facts := `[{"name":"eno1","mac":"aa:bb:cc:00:00:09","type":"physical","ip":"10.0.0.5/24"}]`
	_, stderr, err := runPop1Script(t, facts)
	if err == nil {
		t.Fatal("expected hard-fail for a matched-but-not-L3-modeled host, got success")
	}
	if !strings.Contains(stderr, "not fully L3-modeled") {
		t.Errorf("stderr should explain the modeling gap, got: %s", stderr)
	}
}

// TestPop1Script_UnknownSubnetHardFails: two /31 legs on a subnet not in the
// leaf-pair table cannot derive ASNs — must hard-fail, not ship a half-config.
func TestPop1Script_UnknownSubnetHardFails(t *testing.T) {
	facts := `[
	  {"name":"eth0","mac":"aa:bb:cc:00:00:01","type":"physical","ip":"10.254.30.1/31"},
	  {"name":"eth1","mac":"aa:bb:cc:00:00:02","type":"physical","ip":"10.254.30.3/31"},
	  {"name":"lo","type":"physical","ip":"10.254.31.200/32"}
	]`
	_, stderr, err := runPop1Script(t, facts)
	if err == nil {
		t.Fatal("expected hard-fail on unknown /31 subnet, got success")
	}
	if !strings.Contains(stderr, "not a known pop1 leaf pair") {
		t.Errorf("stderr should explain the unknown-subnet failure, got: %s", stderr)
	}
}

// TestPop1Script_LegsInDifferentSubnetsHardFail: both legs of a kworker land
// on one leaf pair, and that pair's /24 gives both leaf ASNs. Legs whose /31s
// sit in two different /24s are a NetBox modeling error. Deriving the pair from
// the lower leg alone would give the higher leg's neighbor the wrong remote-as
// (leaf-4's 65024 on a leg in leaf-5/6's 10.254.29.0/24), a session that can
// never establish, so the host would come up silently on one leg. The check
// must also not depend on which leg sorts first: an unknown /24 on the higher
// leg must fail just like one on the lower leg.
func TestPop1Script_LegsInDifferentSubnetsHardFail(t *testing.T) {
	for _, tc := range []struct{ name, legA, legB string }{
		{"two known leaf pairs", "10.254.28.1/31", "10.254.29.1/31"},
		{"unknown subnet on the higher leg", "10.254.28.1/31", "10.254.30.1/31"},
		{"unknown subnet on the lower leg", "10.254.27.1/31", "10.254.28.1/31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := `[
	  {"name":"eth0","mac":"aa:bb:cc:00:00:01","type":"physical","ip":"` + tc.legA + `"},
	  {"name":"eth1","mac":"aa:bb:cc:00:00:02","type":"physical","ip":"` + tc.legB + `"},
	  {"name":"lo","type":"physical","ip":"10.254.31.67/32"}
	]`
			bundle, stderr, err := runPop1Script(t, facts)
			if err == nil {
				frr := bundleFile(t, bundle, "/etc/frr/frr.conf")["content"].(string)
				t.Fatalf("expected hard-fail for legs %s and %s in different /24s, got success; frr.conf:\n%s",
					tc.legA, tc.legB, frr)
			}
			if !strings.Contains(stderr, "different /24s") {
				t.Errorf("stderr should explain the cross-subnet failure, got: %s", stderr)
			}
		})
	}
}
