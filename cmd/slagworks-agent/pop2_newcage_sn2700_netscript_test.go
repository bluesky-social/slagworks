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

func pop2NewCageSN2700ScriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "assets", "network", "300-pop2-newcage-sn2700-switch")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("POP2 new-cage SN2700 script not found at %s: %v", path, err)
	}
	return path
}

func runPop2NewCageSN2700Script(t *testing.T, facts string) (map[string]any, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	factsFile := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(factsFile, []byte(facts), 0o600); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	cmd := exec.Command("python3", pop2NewCageSN2700ScriptPath(t))
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
	if err := json.Unmarshal(out, &bundle); err != nil {
		t.Fatalf("bundle is not valid JSON: %v\nstdout: %s", err, out)
	}
	return bundle, stderr.String(), nil
}

const pop2Leaf3SN2700Facts = `[
  {"name":"bond_sp1","mac":"20:4d:52:23:00:11","type":"lag","ip":"10.253.24.50/30"},
  {"name":"eth0","mac":"0c:42:a1:8b:8f:68","type":"physical","ip":"10.253.242.23/23"},
  {"name":"ipmi0","mac":"f4:02:70:b9:85:78","type":"physical","ip":"10.253.242.104/23"},
  {"name":"swp9","mac":"90:0a:84:44:e9:7d","type":"physical","ip":"10.253.28.0/31"}
]`

func TestPop2NewCageSN2700_StaticOOBBootstrap(t *testing.T) {
	bundle, stderr, err := runPop2NewCageSN2700Script(t, pop2Leaf3SN2700Facts)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}
	file := bundleFile(t, bundle, "/etc/netplan/99-slagworks.yaml")
	content, _ := file["content"].(string)

	for _, want := range []string{
		`macaddress: "0c:42:a1:8b:8f:68"`,
		`- "10.253.242.23/23"`,
		"to: default",
		"via: 10.253.243.252",
		`- "10.253.243.252"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("netplan missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "set-name") {
		t.Errorf("management NIC must remain match-by-MAC without rename:\n%s", content)
	}
	for _, nonMgmtMAC := range []string{
		"20:4d:52:23:00:11",
		"90:0a:84:44:e9:7d",
		"f4:02:70:b9:85:78",
	} {
		if strings.Contains(content, nonMgmtMAC) {
			t.Errorf("non-management MAC %s leaked into bootstrap netplan:\n%s", nonMgmtMAC, content)
		}
	}
	if got, _ := file["mode"].(string); got != "0600" {
		t.Errorf("netplan mode = %q, want 0600", got)
	}
}

func TestPop2NewCageSN2700_RequiresExactOOBPrefix(t *testing.T) {
	facts := `[{"name":"eth0","mac":"0c:42:a1:8b:8f:68","type":"physical","ip":"10.253.240.23/22"}]`
	_, stderr, err := runPop2NewCageSN2700Script(t, facts)
	if err == nil {
		t.Fatal("expected script to reject a management IP outside the new-cage /23")
	}
	if !strings.Contains(stderr, "exact OOB prefix 10.253.242.0/23") {
		t.Errorf("unexpected error: %s", stderr)
	}
}

func TestPop2NewCageSN2700_RequiresOneModeledMgmtNIC(t *testing.T) {
	facts := `[{"name":"eth0","mac":"0c:42:a1:8b:8f:68","type":"physical","ip":""}]`
	_, stderr, err := runPop2NewCageSN2700Script(t, facts)
	if err == nil {
		t.Fatal("expected script to reject an unmodeled management NIC")
	}
	if !strings.Contains(stderr, "exactly one statically modeled management NIC") {
		t.Errorf("unexpected error: %s", stderr)
	}
}
