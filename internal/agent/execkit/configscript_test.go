//go:build !integration

package execkit

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRunConfigScript_RoundTrip runs a real shell script through the harness
// and confirms: the injected env var points at a file holding the data bytes,
// and the script's stdout comes back verbatim.
func TestRunConfigScript_RoundTrip(t *testing.T) {
	// Script echoes the contents of the file the harness points the env var at,
	// proving both the env-var name and the data injection work.
	script := []byte("#!/bin/sh\ncat \"$SLAGWORKS_TESTDATA\"\n")
	data := []byte(`{"hello":"world"}`)

	out, err := RunConfigScript(slog.Default(), "test", script, "SLAGWORKS_TESTDATA", data, nil)
	if err != nil {
		t.Fatalf("runConfigScript: %v", err)
	}
	if string(out) != string(data) {
		t.Errorf("stdout = %q, want the data echoed back %q", out, data)
	}
}

// TestRunConfigScript_StdoutOnly confirms only stdout is captured — a script
// that writes to stderr must not pollute the returned bytes.
func TestRunConfigScript_StdoutOnly(t *testing.T) {
	script := []byte("#!/bin/sh\necho diagnostic >&2\necho RESULT\n")
	out, err := RunConfigScript(slog.Default(), "test", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err != nil {
		t.Fatalf("runConfigScript: %v", err)
	}
	if strings.TrimSpace(string(out)) != "RESULT" {
		t.Errorf("stdout = %q, want only the stdout line (stderr excluded)", out)
	}
}

// TestRunConfigScript_NonZeroExitFails confirms a script that exits non-zero
// surfaces as an error naming the kind, not a silent empty result.
func TestRunConfigScript_NonZeroExitFails(t *testing.T) {
	script := []byte("#!/bin/sh\necho partial\nexit 3\n")
	_, err := RunConfigScript(slog.Default(), "storage", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "storage script failed") {
		t.Errorf("error should name the kind, got: %v", err)
	}
}

// TestRunConfigScript_CleansUpTempFiles confirms both temp files are removed
// after the run (no /tmp leak across a long-lived agent / many deploys).
func TestRunConfigScript_CleansUpTempFiles(t *testing.T) {
	before := countSlagworksTempFiles(t)
	script := []byte("#!/bin/sh\ncat \"$SLAGWORKS_TESTDATA\"\n")
	if _, err := RunConfigScript(slog.Default(), "test", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil); err != nil {
		t.Fatalf("runConfigScript: %v", err)
	}
	after := countSlagworksTempFiles(t)
	if after != before {
		t.Errorf("temp files leaked: before=%d after=%d", before, after)
	}
}

// TestRunConfigScript_SidecarCoLocated proves the network seam's contract: a
// script can `import` a sidecar module purely because the harness co-locates it
// in the same directory (Python's sys.path[0]), with NO PYTHONPATH plumbing —
// exactly how the deploy agent ships slagworks_netlib next to each network
// script. We scrub PYTHONPATH from the child env to prove co-location alone is
// load-bearing: if the harness stopped writing the sidecar beside the script,
// this test fails with ModuleNotFoundError.
func TestRunConfigScript_SidecarCoLocated(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	t.Setenv("PYTHONPATH", "") // co-location, not env, must resolve the import
	script := []byte("#!/usr/bin/env python3\nimport helper_mod\nprint(helper_mod.VALUE)\n")
	sidecars := map[string][]byte{"helper_mod.py": []byte("VALUE = 'from-sidecar'\n")}

	out, err := RunConfigScript(slog.Default(), "network", script, "SLAGWORKS_TESTDATA", []byte("{}"), sidecars)
	if err != nil {
		t.Fatalf("runConfigScript with sidecar: %v", err)
	}
	if strings.TrimSpace(string(out)) != "from-sidecar" {
		t.Errorf("stdout = %q, want the sidecar-imported value; the sidecar was not co-located with the script", out)
	}
}

func countSlagworksTempFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read tempdir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "slagworks-test-") {
			n++
		}
	}
	return n
}
