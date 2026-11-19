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

func pop2L3KworkerScriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", "network", "125-pop2-l3-kworker")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pop2 L3 kworker script: %v", err)
	}
	return path
}

func runPop2L3KworkerScript(t *testing.T, facts string) (string, string, int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	factsPath := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(factsPath, []byte(facts), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", pop2L3KworkerScriptPath(t))
	cmd.Env = append(os.Environ(),
		"SLAGWORKS_NETWORK_FACTS="+factsPath,
		"PYTHONPATH="+netlibDirSnapshot(t),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	t.Fatalf("run script: %v", err)
	return "", "", -1
}

func TestPop2L3KworkerScriptGolden(t *testing.T) {
	facts := `[
  {"name":"ens2f0np0","mac":"7c:c2:55:be:f6:3a","type":"physical","ip":"10.253.28.53/31"},
  {"name":"ens2f1np1","mac":"7c:c2:55:be:f6:3b","type":"physical","ip":"10.253.28.55/31"},
  {"name":"lo","type":"virtual","ip":"10.253.31.110/32"},
  {"name":"ipmi0","mac":"90:5a:08:15:cc:98","type":"ipmi","ip":"10.253.242.203/23"}
]`
	stdout, stderr, exit := runPop2L3KworkerScript(t, facts)
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, stderr)
	}
	var bundle netBundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("bundle JSON: %v\n%s", err, stdout)
	}
	if len(bundle.Files) != 4 {
		t.Fatalf("expected netplan, FRR config/daemons, and sysctl; got %+v", bundle)
	}
	files := make(map[string]string, len(bundle.Files))
	for _, file := range bundle.Files {
		files[file.Path] = file.Content
	}
	netplan := files["/etc/netplan/99-slagworks.yaml"]
	for _, want := range []string{
		`macaddress: "7c:c2:55:be:f6:3a"`,
		`macaddress: "7c:c2:55:be:f6:3b"`,
		`- "10.253.28.53/31"`,
		`- "10.253.28.55/31"`,
		`- "10.253.31.110/32"`,
		`- "10.253.0.1"`,
		"mtu: 9000",
	} {
		if !strings.Contains(netplan, want) {
			t.Errorf("netplan missing %q:\n%s", want, netplan)
		}
	}
	if strings.Contains(netplan, "0.0.0.0/0") {
		t.Fatalf("netplan must not install a default that shadows BGP:\n%s", netplan)
	}
	frr := files["/etc/frr/frr.conf"]
	for _, want := range []string{
		"bgp router-id 10.253.31.110",
		"neighbor 10.253.28.52 remote-as 65023",
		"neighbor 10.253.28.54 remote-as 65024",
		"network 10.253.31.110/32",
		"ip route 0.0.0.0/0 10.253.28.52 250",
		"ip route 0.0.0.0/0 10.253.28.54 250",
	} {
		if !strings.Contains(frr, want) {
			t.Errorf("FRR config missing %q:\n%s", want, frr)
		}
	}
	if len(bundle.EnableUnits) != 1 || bundle.EnableUnits[0] != "frr.service" {
		t.Fatalf("FRR must be enabled at first boot: %+v", bundle.EnableUnits)
	}
}

func TestPop2L3KworkerScriptMatchScope(t *testing.T) {
	body, err := os.ReadFile(pop2L3KworkerScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script, err := ParseNetworkScript("125-pop2-l3-kworker", body)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		site, location, deviceType, hostname string
		want                                 bool
	}{
		{"pop2", "pop2-01-005250", "supermicro-as-1115hs-tnr", "kworker-l34-8.pop2", true},
		{"pop2", "pop2-01-005250", "supermicro-as-1115hs-tnr", "kworker-l56-16.pop2", true},
		{"pop2", "pop2-01-005330", "supermicro-as-1115hs-tnr", "kworker-l34-8.pop2", false},
		{"pop1", "pop2-01-005250", "supermicro-as-1115hs-tnr", "kworker-l34-8.pop2", false},
		{"pop2", "pop2-01-005250", "supermicro-as-1115hs-tnr", "algo1.pop2", false},
	}
	for _, test := range tests {
		resolve := func(field string) string {
			switch field {
			case "site":
				return test.site
			case "location":
				return test.location
			case "device_type":
				return test.deviceType
			case "hostname":
				return test.hostname
			default:
				return ""
			}
		}
		got, err := configscript.Match([]configscript.Script{*script}, resolve)
		if err != nil {
			t.Fatal(err)
		}
		if (got != nil) != test.want {
			t.Errorf("match(%s,%s,%s,%s)=%v, want %v", test.site, test.location, test.deviceType, test.hostname, got != nil, test.want)
		}
	}
}

func TestPop2L3KworkerScriptRejectsIncompleteModel(t *testing.T) {
	facts := `[{"name":"ens2f0np0","mac":"7c:c2:55:be:f6:3a","type":"physical","ip":"10.253.28.53/31"}]`
	stdout, stderr, exit := runPop2L3KworkerScript(t, facts)
	if exit == 0 || stdout != "" || !strings.Contains(stderr, "not fully L3-modeled") {
		t.Fatalf("wanted closed failure, exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

// TestPop2L3KworkerScriptRejectsLegsInDifferentSubnets: both legs of a kworker
// land on one leaf pair, and that pair's /24 gives both leaf ASNs. Legs whose
// /31s sit in two different /24s are a NetBox modeling error. Deriving the pair
// from the lower leg alone would give the higher leg's neighbor the wrong
// remote-as (leaf-4's 65024 on a leg in leaf-5/6's 10.253.29.0/24), a session
// that can never establish, so the host would come up silently on one leg. The
// check must also not depend on which leg sorts first.
func TestPop2L3KworkerScriptRejectsLegsInDifferentSubnets(t *testing.T) {
	for _, tc := range []struct{ name, legA, legB string }{
		{"two known leaf pairs", "10.253.28.1/31", "10.253.29.1/31"},
		{"unknown subnet on the higher leg", "10.253.28.1/31", "10.253.30.1/31"},
		{"unknown subnet on the lower leg", "10.253.27.1/31", "10.253.28.1/31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := `[
  {"name":"ens2f0np0","mac":"7c:c2:55:be:f6:3a","type":"physical","ip":"` + tc.legA + `"},
  {"name":"ens2f1np1","mac":"7c:c2:55:be:f6:3b","type":"physical","ip":"` + tc.legB + `"},
  {"name":"lo","type":"virtual","ip":"10.253.31.110/32"}
]`
			stdout, stderr, exit := runPop2L3KworkerScript(t, facts)
			if exit == 0 {
				var bundle netBundle
				if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
					t.Fatalf("expected hard-fail, got exit 0 and invalid bundle JSON: %v\n%s", err, stdout)
				}
				var frr string
				for _, file := range bundle.Files {
					if file.Path == "/etc/frr/frr.conf" {
						frr = file.Content
					}
				}
				t.Fatalf("expected hard-fail for legs %s and %s in different /24s, got success; frr.conf:\n%s",
					tc.legA, tc.legB, frr)
			}
			if stdout != "" || !strings.Contains(stderr, "different /24s") {
				t.Fatalf("wanted closed failure explaining the cross-subnet legs, exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
		})
	}
}
