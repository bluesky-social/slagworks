package netlib

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSourceEmbedded confirms the embedded lib is non-empty and carries the
// module's public entry points — a cheap guard that go:embed picked up the
// real file (an empty or wrong embed would silently break every network script
// at deploy time, which only surfaces on real hardware).
func TestSourceEmbedded(t *testing.T) {
	if len(Source) == 0 {
		t.Fatal("embedded netlib source is empty")
	}
	for _, want := range []string{"def load_facts", "def emit", "def vip_nameserver", "def bond_lines"} {
		if !bytes.Contains(Source, []byte(want)) {
			t.Errorf("embedded netlib source is missing %q", want)
		}
	}
}

// TestSourceImportsUnderPython writes the embedded lib as <ModuleName>.py and
// imports it with a stdlib-only python3 (the deploy initrd has no third-party
// packages), calling a couple of helpers to prove the module both parses and
// runs. This is the unit-level proof that what the agent ships is a valid,
// importable module — the network scripts' `import slagworks_netlib` depends on
// it. Skips if python3 is absent (present on CI + dev laptops).
func TestSourceImportsUnderPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ModuleName+".py"), Source, 0o644); err != nil {
		t.Fatalf("write lib: %v", err)
	}
	// A driver that imports the module from its own dir (sys.path[0]) and
	// exercises the VIP resolver — the assertion catches a lib that imports but
	// whose behavior drifted.
	driver := "import slagworks_netlib as nl\n" +
		"assert nl.vip_nameserver('10.253.3.254') == '10.253.240.1', nl.vip_nameserver('10.253.3.254')\n" +
		"assert nl.leaf_side_of_31('10.254.28.1/31') == '10.254.28.0'\n" +
		"print('ok')\n"
	if err := os.WriteFile(filepath.Join(dir, "driver.py"), []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command("python3", filepath.Join(dir, "driver.py"))
	cmd.Env = append(os.Environ(), "PYTHONPATH=") // co-location resolves the import
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("import/exercise embedded lib: %v\n%s", err, out)
	}
}
