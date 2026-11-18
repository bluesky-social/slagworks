package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRunner implements agent.Runner by reading from a directory of
// fixture files.
// Mapping rules:
//   - dmidecode -s <key>  → dmi-<key>.txt
//   - lsblk -Jb ...       → lsblk.json
//   - ip -j link show     → ip-link.json
//   - lspci -nnmm         → lspci.txt
//   - awk /MemTotal/ ...  → meminfo.txt
//   - Any other command   → not available
type fixtureRunner struct {
	dir     string
	present map[string]bool // commands marked as available
}

func newFixtureRunner(dir string, presentCmds ...string) *fixtureRunner {
	p := make(map[string]bool, len(presentCmds))
	for _, c := range presentCmds {
		p[c] = true
	}
	return &fixtureRunner{dir: dir, present: p}
}

func (f *fixtureRunner) Available(name string) bool {
	return f.present[name]
}

// Output returns the trimmed stdout of the fixture-mapped command.
func (f *fixtureRunner) Output(name string, args ...string) (string, error) {
	fname := f.fixtureFile(name, args...)
	if fname == "" {
		return "", &os.PathError{Op: "exec", Path: name, Err: os.ErrNotExist}
	}
	data, err := os.ReadFile(filepath.Join(f.dir, fname))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Run discards stdout and reports only the error, matching agent.Runner.
func (f *fixtureRunner) Run(name string, args ...string) error {
	_, err := f.Output(name, args...)
	return err
}

func (f *fixtureRunner) fixtureFile(name string, args ...string) string {
	switch name {
	case "dmidecode":
		if len(args) >= 2 && args[0] == "-s" {
			return "dmi-" + args[1] + ".txt"
		}
		if len(args) >= 2 && args[0] == "-t" {
			return "dmi-type-" + args[1] + ".txt"
		}
	case "lsblk":
		return "lsblk.json"
	case "ip":
		if len(args) >= 2 && args[0] == "-j" {
			return "ip-addr.json"
		}
	case "lspci":
		return "lspci.txt"
	case "net":
		if len(args) >= 2 && args[0] == "show" {
			return "net-show-" + args[1] + ".txt"
		}
	}
	return ""
}

const fixtureDir = "testdata/ubuntu-2404"

func TestCollectInventory_Ubuntu2404(t *testing.T) {
	runner := newFixtureRunner(fixtureDir, "dmidecode", "lsblk", "ip", "lspci")

	result := CollectGraceful(runner, Options{})

	if result == nil {
		t.Fatal("CollectGraceful returned nil")
	}

	// Verify system info
	if result.System == nil {
		t.Fatal("expected system info to be populated")
	}
	if result.System.Manufacturer != "ASUSTeK Computer Inc." {
		t.Errorf("Manufacturer = %q, want ASUSTeK Computer Inc.", result.System.Manufacturer)
	}
	if result.System.ProductName != "RS501A-E12-RS4U" {
		t.Errorf("ProductName = %q, want RS501A-E12-RS4U", result.System.ProductName)
	}
	if result.System.SerialNumber != "M12345ABC678" {
		t.Errorf("SerialNumber = %q, want M12345ABC678", result.System.SerialNumber)
	}

	// Verify disks
	if len(result.Disks) != 2 {
		t.Errorf("expected 2 disks, got %d", len(result.Disks))
	} else {
		if result.Disks[0].Name != "nvme0n1" {
			t.Errorf("Disks[0].Name = %q, want nvme0n1", result.Disks[0].Name)
		}
		if result.Disks[0].SMARTHealthy != nil {
			t.Error("expected SMARTHealthy nil when smartctl not available")
		}
	}

	// Verify NICs
	if len(result.NICs) < 2 {
		t.Errorf("expected at least 2 NICs, got %d", len(result.NICs))
	} else {
		found := false
		for _, nic := range result.NICs {
			if nic.MAC == "3c:ec:ef:aa:bb:01" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected NIC with MAC 3c:ec:ef:aa:bb:01")
		}
	}

	// Verify PCI devices
	if len(result.PCIDevices) == 0 {
		t.Error("expected PCI devices to be populated")
	}

	// LLDP should be nil (not in fixture)
	if result.LLDP != nil {
		t.Error("expected LLDP nil when lldpctl not available")
	}
}

func TestCollect_NoDmidecode(t *testing.T) {
	// dmidecode not available — system info may still be populated from sysfs fallback
	runner := newFixtureRunner(fixtureDir, "lsblk", "ip", "lspci")

	result := CollectGraceful(runner, Options{})

	if result == nil {
		t.Fatal("CollectGraceful returned nil even without dmidecode")
	}
	// System info may be nil (no sysfs on this host) or populated via sysfs
	// fallback — either is acceptable. The key invariant is that we don't crash.
	if result.System != nil {
		t.Logf("sysfs fallback populated system info: manufacturer=%q", result.System.Manufacturer)
	} else {
		t.Log("no system info available (no dmidecode, no sysfs)")
	}
	// Disks, NICs should still be collected
	if len(result.Disks) == 0 {
		t.Error("expected disks even without dmidecode")
	}
	if len(result.NICs) == 0 {
		t.Error("expected NICs even without dmidecode")
	}
}

func TestCollect_NoSmartctl(t *testing.T) {
	// smartctl not in fixture dir — lsblk should still work, SMART data nil
	runner := newFixtureRunner(fixtureDir, "dmidecode", "lsblk", "ip", "lspci")

	result := CollectGraceful(runner, Options{})

	if len(result.Disks) == 0 {
		t.Fatal("expected disks")
	}
	for _, d := range result.Disks {
		if d.SMARTHealthy != nil {
			t.Errorf("disk %s: expected SMARTHealthy nil without smartctl, got %v", d.Name, d.SMARTHealthy)
		}
	}
}

func TestCollect_NoLldpcli(t *testing.T) {
	runner := newFixtureRunner(fixtureDir, "dmidecode", "lsblk", "ip", "lspci")

	result := CollectGraceful(runner, Options{})

	if result.LLDP != nil {
		t.Error("expected LLDP nil when lldpctl not available")
	}
}
