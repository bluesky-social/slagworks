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

// These tests exercise the two Dell PowerEdge R640 config scripts for the pop2
// new-cage oob-gws (the self-reimage targets oob-gw-3/4.pop2):
//
//   assets/storage/150-dell-r640         — RAID1 across the two SAS boot SSDs
//   assets/network/150-pop2-newcage-oob-gw — bootstrap netplan: OOB NIC only
//
// They run the scripts the agent's way (inventory/facts to a file, the env var
// pointed at it, capture stdout) and assert the emitted layout/bundle. See the
// SN2700 (sn2700_netscript_test.go) and ASUS storage tests for the pattern.

func dellScriptPath(t *testing.T, kind, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "assets", kind, name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s script not found at %s: %v", name, path, err)
	}
	return path
}

func runScript(t *testing.T, scriptPath, envVar, data string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dataFile := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(dataFile, []byte(data), 0644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	cmd := exec.Command("python3", scriptPath)
	// PYTHONPATH lets network scripts `import slagworks_netlib` (harmless for
	// storage scripts, which don't import it) — mirrors the agent co-locating
	// the lib next to the script (internal/agent/netlib).
	cmd.Env = append(os.Environ(), envVar+"="+dataFile, "PYTHONPATH="+netlibDir(t))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return string(out), stderr.String(), err
}

// --- storage: 150-dell-r640 ---

// realR640Inv is the real oob-gw-3.pop2 disk shape: two SAS boot SSDs behind
// the LSI SAS3008 HBA (a Samsung MZ7LM and a Micron MTFDDAK), no data disks.
const realR640Inv = `{
  "disks": [
    {"name": "sda", "model": "MZ7LM960HCHP00D6", "serial": "S2KKNYAG604402", "size_bytes": 960197124096, "transport": "sas", "rota": false},
    {"name": "sdb", "model": "MTFDDAK960TDN", "serial": "200526481131", "size_bytes": 960197124096, "transport": "sas", "rota": false}
  ]
}`

func TestDellR640Storage_RAID1AcrossSASBootDisks(t *testing.T) {
	out, stderr, err := runScript(t, dellScriptPath(t, "storage", "150-dell-r640"), "SLAGWORKS_INVENTORY", realR640Inv)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}
	var layout struct {
		Layout map[string]struct {
			Type       string `json:"type"`
			Level      int    `json:"level"`
			Members    []string
			Partitions []struct {
				Name     string `json:"name"`
				FS       string `json:"fs"`
				Bootable bool   `json:"bootable"`
			} `json:"partitions"`
		} `json:"layout"`
		Mounts map[string]struct {
			Device string `json:"device"`
		} `json:"mounts"`
	}
	if jerr := json.Unmarshal([]byte(out), &layout); jerr != nil {
		t.Fatalf("layout is not valid JSON: %v\nstdout: %s", jerr, out)
	}

	// Both SAS disks are partitioned (ESP + root member), bare "sdaN" suffix
	// (not "sdapN" — that's nvme/mmc only).
	for _, d := range []string{"sda", "sdb"} {
		dev, ok := layout.Layout[d]
		if !ok {
			t.Fatalf("expected disk %s in layout; got %v", d, out)
		}
		if dev.Type != "disk" {
			t.Errorf("%s should be type disk, got %q", d, dev.Type)
		}
		if len(dev.Partitions) != 2 {
			t.Fatalf("%s expected 2 partitions (ESP+root), got %d", d, len(dev.Partitions))
		}
		if dev.Partitions[0].Name != d+"1" || dev.Partitions[0].FS != "vfat" || !dev.Partitions[0].Bootable {
			t.Errorf("%s p1 should be a bootable vfat ESP named %s1, got %+v", d, d, dev.Partitions[0])
		}
		if dev.Partitions[1].Name != d+"2" {
			t.Errorf("%s p2 should be named %s2, got %q", d, d, dev.Partitions[1].Name)
		}
	}

	// mdroot is RAID1 over the two root partitions, xfs, mounted at /.
	md, ok := layout.Layout["mdroot"]
	if !ok || md.Type != "raid" || md.Level != 1 {
		t.Fatalf("expected mdroot RAID1, got %+v", md)
	}
	if len(md.Members) != 2 || md.Members[0] != "sda2" || md.Members[1] != "sdb2" {
		t.Errorf("mdroot members should be [sda2 sdb2], got %v", md.Members)
	}
	if layout.Mounts["/"].Device != "mdroot" {
		t.Errorf("/ should mount mdroot, got %q", layout.Mounts["/"].Device)
	}
	if layout.Mounts["/boot/efi"].Device != "sda1" {
		t.Errorf("/boot/efi should mount sda1, got %q", layout.Mounts["/boot/efi"].Device)
	}
	// No data array on a gateway.
	if _, ok := layout.Layout["mddata"]; ok {
		t.Error("oob-gw should have no data array, but mddata is present")
	}
}

func TestDellR640Storage_HardFailsOnWrongDiskCount(t *testing.T) {
	// One SAS disk (a degraded/single-survivor box) must hard-fail rather than
	// emit a single-disk layout that silently drops the mirror.
	oneDisk := `{"disks": [{"name":"sda","transport":"sas","size_bytes":960197124096}]}`
	if _, stderr, err := runScript(t, dellScriptPath(t, "storage", "150-dell-r640"), "SLAGWORKS_INVENTORY", oneDisk); err == nil {
		t.Fatal("expected hard-fail with 1 SAS disk")
	} else if !strings.Contains(stderr, "expected exactly 2 SAS boot disks") {
		t.Errorf("expected a clear disk-count error, got: %s", stderr)
	}

	// Three SAS disks: ambiguous which two to mirror; hard-fail.
	threeDisks := `{"disks": [
	  {"name":"sda","transport":"sas","size_bytes":960197124096},
	  {"name":"sdb","transport":"sas","size_bytes":960197124096},
	  {"name":"sdc","transport":"sas","size_bytes":960197124096}
	]}`
	if _, stderr, err := runScript(t, dellScriptPath(t, "storage", "150-dell-r640"), "SLAGWORKS_INVENTORY", threeDisks); err == nil {
		t.Fatal("expected hard-fail with 3 SAS disks")
	} else if !strings.Contains(stderr, "expected exactly 2 SAS boot disks") {
		t.Errorf("expected a clear disk-count error, got: %s", stderr)
	}
}

// --- network: 150-pop2-newcage-oob-gw ---

// realGW3Facts is the real oob-gw-3.pop2 interface shape: the WAN NIC with a
// PUBLIC /30, the OOB mgmt NIC with a PRIVATE /23, the iDRAC port (modeled
// mgmt_only=False -> classified "physical", carries a private OOB IP), and the
// tailscale virtual NIC.
const realGW3Facts = `[
  {"netbox_id":1,"name":"eno1np0","mac":"E4:43:4B:7A:0C:EE","type":"physical","ip":"142.215.32.114/30","gateway":"142.215.32.113"},
  {"netbox_id":2,"name":"eno3","mac":"E4:43:4B:7A:0D:0E","type":"physical","ip":"10.253.243.254/23","gateway":"10.253.243.252"},
  {"netbox_id":3,"name":"ipmi0","mac":"F4:02:70:B9:85:78","type":"physical","ip":"10.253.243.124/23","gateway":"10.253.243.252"},
  {"netbox_id":4,"name":"tailscale0","mac":"","type":"physical","ip":"100.110.66.106/32"}
]`

func TestDellOOBGW_BringsUpOnlyOOBNIC(t *testing.T) {
	out, stderr, err := runScript(t, dellScriptPath(t, "network", "150-pop2-newcage-oob-gw"), "SLAGWORKS_NETWORK_FACTS", realGW3Facts)
	if err != nil {
		t.Fatalf("script failed: %v\nstderr: %s", err, stderr)
	}
	var bundle map[string]any
	if jerr := json.Unmarshal([]byte(out), &bundle); jerr != nil {
		t.Fatalf("bundle is not valid JSON: %v\nstdout: %s", jerr, out)
	}
	f := bundleFile(t, bundle, "/etc/netplan/99-slagworks.yaml")
	content, _ := f["content"].(string)

	// The OOB NIC (eno3) is matched by MAC and given its static private IP +
	// the VIP gateway.
	if !strings.Contains(content, `macaddress: "e4:43:4b:7a:0d:0e"`) {
		t.Errorf("OOB NIC not matched by MAC; netplan:\n%s", content)
	}
	if !strings.Contains(content, "10.253.243.254/23") {
		t.Errorf("OOB static address missing; netplan:\n%s", content)
	}
	if !strings.Contains(content, "via: 10.253.243.252") {
		t.Errorf("default route via VIP gateway missing; netplan:\n%s", content)
	}
	if strings.Contains(content, "dhcp4: true") {
		t.Errorf("OOB NIC must be static, not DHCP:\n%s", content)
	}

	// The WAN NIC (public /30) must NOT appear — its routing is FRR-owned.
	if strings.Contains(content, "142.215.32") || strings.Contains(content, "e4:43:4b:7a:0c:ee") {
		t.Errorf("WAN NIC leaked into bootstrap netplan (must be left for FRR/ansible):\n%s", content)
	}
	// The iDRAC port (private IP but a BMC) must NOT appear.
	if strings.Contains(content, "f4:02:70:b9:85:78") || strings.Contains(content, "10.253.243.124") {
		t.Errorf("iDRAC/BMC port leaked into host netplan:\n%s", content)
	}
}

func TestDellOOBGW_HardFailsWhenNoPrivateNIC(t *testing.T) {
	// Only a public WAN NIC + BMC: no host OOB leg to bring up. Must hard-fail
	// rather than emit an empty netplan that strands the gateway mid-reimage.
	noOOB := `[
	  {"name":"eno1np0","mac":"E4:43:4B:7A:0C:EE","type":"physical","ip":"142.215.32.114/30","gateway":"142.215.32.113"},
	  {"name":"ipmi0","mac":"F4:02:70:B9:85:78","type":"physical","ip":"10.253.243.124/23","gateway":"10.253.243.252"}
	]`
	if _, stderr, err := runScript(t, dellScriptPath(t, "network", "150-pop2-newcage-oob-gw"), "SLAGWORKS_NETWORK_FACTS", noOOB); err == nil {
		t.Fatal("expected hard-fail when no private OOB NIC present")
	} else if !strings.Contains(stderr, "no OOB management NIC") {
		t.Errorf("expected a clear no-OOB-NIC error, got: %s", stderr)
	}
}

func TestDellOOBGW_NoGateway_AddressOnly(t *testing.T) {
	// OOB NIC present but no resolved gateway (the real gw-3 case: slagworks
	// resolves per-iface gateways from slagworks-PRODUCTION prefixes only, and
	// the mgmt NIC sits in the slagworks-OOB prefix). The bootstrap must still
	// succeed address-only — the site worker is on the same OOB /23 so
	// phone-home is directly connected, and ansible/FRR lay the real default
	// route post-boot. Emitting NO route here (rather than hard-failing) is the
	// fix for the live gw-3 deploy failure.
	noGW := `[
	  {"name":"eno3","mac":"E4:43:4B:7A:0D:0E","type":"physical","ip":"10.253.243.254/23"}
	]`
	out, stderr, err := runScript(t, dellScriptPath(t, "network", "150-pop2-newcage-oob-gw"), "SLAGWORKS_NETWORK_FACTS", noGW)
	if err != nil {
		t.Fatalf("expected success address-only with no gateway, got error: %v\nstderr: %s", err, stderr)
	}
	var bundle map[string]any
	if jerr := json.Unmarshal([]byte(out), &bundle); jerr != nil {
		t.Fatalf("bundle not valid JSON: %v\n%s", jerr, out)
	}
	content, _ := bundleFile(t, bundle, "/etc/netplan/99-slagworks.yaml")["content"].(string)
	if !strings.Contains(content, "10.253.243.254/23") || !strings.Contains(content, "e4:43:4b:7a:0d:0e") {
		t.Errorf("expected eno3 brought up by MAC with its address; netplan:\n%s", content)
	}
	if strings.Contains(content, "to: default") {
		t.Errorf("expected NO default route when gateway unresolved; netplan:\n%s", content)
	}
}
