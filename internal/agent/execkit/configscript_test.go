//go:build !integration

package execkit

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
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

	out, err := RunConfigScript(nil, slog.Default(), "test", script, "SLAGWORKS_TESTDATA", data, nil)
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
	out, err := RunConfigScript(nil, slog.Default(), "test", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
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
	_, err := RunConfigScript(nil, slog.Default(), "storage", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "storage script failed") {
		t.Errorf("error should name the kind, got: %v", err)
	}
}

// TestRunConfigScript_FailureCarriesStderr is the regression for a deploy whose
// network script hard-failed on an incomplete NetBox model: the script wrote a
// precise "fix the NetBox model" ERROR to stderr, but the returned error (and so
// the operation result) said only "exit status 1" because stderr went to the
// serial console alone. The script's diagnostics must be in the error.
func TestRunConfigScript_FailureCarriesStderr(t *testing.T) {
	script := []byte("#!/bin/sh\n" +
		"echo 'WARNING: bond member eth1 has no MAC' >&2\n" +
		"echo 'ERROR: host has no bond interface. Fix the NetBox model and redeploy.' >&2\n" +
		"exit 1\n")
	logger, records := recordingLogger()

	_, err := RunConfigScript(nil, logger, "network", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
	for _, want := range []string{
		"network script failed",
		"WARNING: bond member eth1 has no MAC",
		"ERROR: host has no bond interface. Fix the NetBox model and redeploy.",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q; got: %v", want, err)
		}
	}
	// The same diagnostics must reach the structured log, one envelope per
	// line, so `slagctl machine logs` shows them without the serial console.
	for _, want := range []string{
		"network script stderr: WARNING: bond member eth1 has no MAC",
		"network script stderr: ERROR: host has no bond interface. Fix the NetBox model and redeploy.",
	} {
		if !records.has(slog.LevelWarn, want) {
			t.Errorf("no warn log record %q; got: %v", want, records.messages())
		}
	}
}

// TestRunConfigScript_StderrLoggedOnSuccess pins that a script which succeeds
// but warns (e.g. a degraded single-member bond) has its stderr logged through
// the structured logger, not only the serial console.
func TestRunConfigScript_StderrLoggedOnSuccess(t *testing.T) {
	script := []byte("#!/bin/sh\necho 'WARNING: bond0 has only ONE member' >&2\necho '{}'\n")
	logger, records := recordingLogger()

	out, err := RunConfigScript(nil, logger, "network", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err != nil {
		t.Fatalf("runConfigScript: %v", err)
	}
	if strings.TrimSpace(string(out)) != "{}" {
		t.Errorf("stdout = %q, want only the stdout line", out)
	}
	if want := "network script stderr: WARNING: bond0 has only ONE member"; !records.has(slog.LevelWarn, want) {
		t.Errorf("no warn log record %q; got: %v", want, records.messages())
	}
}

// TestRunConfigScript_StderrCaptureIsBounded pins that a runaway script cannot
// bloat the error (and the operation result it becomes): only the tail of
// stderr is kept, so the final, usually decisive, line survives.
func TestRunConfigScript_StderrCaptureIsBounded(t *testing.T) {
	// ~12 KiB of noise (past the 8 KiB cap), then the line that matters.
	script := []byte("#!/bin/sh\n" +
		"i=0; while [ $i -lt 192 ]; do " +
		"echo 'noise noise noise noise noise noise noise noise noise noise noise' >&2; " +
		"i=$((i+1)); done\n" +
		"echo 'ERROR: the decisive last line' >&2\n" +
		"exit 2\n")
	logger, _ := recordingLogger()

	_, err := RunConfigScript(nil, logger, "storage", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil)
	if err == nil {
		t.Fatal("expected error on non-zero exit, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ERROR: the decisive last line") {
		t.Errorf("error lost the last stderr line: %q", msg)
	}
	if !strings.Contains(msg, "truncated") {
		t.Errorf("error should mark the dropped stderr head; got %q", msg)
	}
	if limit := configScriptStderrCap + 512; len(msg) > limit {
		t.Errorf("error is %d bytes, want <= %d (stderr capture must be bounded)", len(msg), limit)
	}
}

// TestRunConfigScript_CleansUpTempFiles confirms both temp files are removed
// after the run (no /tmp leak across a long-lived agent / many deploys).
func TestRunConfigScript_CleansUpTempFiles(t *testing.T) {
	before := countSlagworksTempFiles(t)
	script := []byte("#!/bin/sh\ncat \"$SLAGWORKS_TESTDATA\"\n")
	if _, err := RunConfigScript(nil, slog.Default(), "test", script, "SLAGWORKS_TESTDATA", []byte("{}"), nil); err != nil {
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

	out, err := RunConfigScript(nil, slog.Default(), "network", script, "SLAGWORKS_TESTDATA", []byte("{}"), sidecars)
	if err != nil {
		t.Fatalf("runConfigScript with sidecar: %v", err)
	}
	if strings.TrimSpace(string(out)) != "from-sidecar" {
		t.Errorf("stdout = %q, want the sidecar-imported value; the sidecar was not co-located with the script", out)
	}
}

// logRecords collects the level and message of every record a test logger
// emits.
type logRecords struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *logRecords) has(level slog.Level, msg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.recs {
		if rec.Level == level && rec.Message == msg {
			return true
		}
	}
	return false
}

func (r *logRecords) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.recs))
	for i, rec := range r.recs {
		out[i] = rec.Level.String() + " " + rec.Message
	}
	return out
}

type recordingHandler struct{ recs *logRecords }

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	h.recs.mu.Lock()
	defer h.recs.mu.Unlock()
	h.recs.recs = append(h.recs.recs, rec)
	return nil
}

func recordingLogger() (*slog.Logger, *logRecords) {
	recs := &logRecords{}
	return slog.New(recordingHandler{recs: recs}), recs
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
