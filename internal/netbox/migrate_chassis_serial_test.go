package netbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// chassisSerialFixtureServer mints a stub NetBox API exposing a fixed
// list of devices on /api/dcim/devices/ and recording PATCH bodies on
// /api/dcim/devices/{id}/. The captured patches let tests assert what
// MigrateChassisSerial wrote.
func chassisSerialFixtureServer(t *testing.T, devices []map[string]any, captured *map[int]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count":   len(devices),
				"results": devices,
			})
		case http.MethodPatch:
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/dcim/devices/"), "/")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			idInt, _ := strconv.Atoi(id)
			(*captured)[idInt] = parsed
			_, _ = w.Write([]byte(`{}`))
		}
	})
	return httptest.NewServer(mux)
}

func TestMigrateChassisSerial_BranchCoverage(t *testing.T) {
	devices := []map[string]any{
		// Branch: cf empty / missing → skip
		{"id": 1, "name": "skip-empty-cf", "serial": "BUILT-1", "custom_fields": map[string]any{}},
		// Branch: cf set, built-in serial empty → write to serial, clear cf
		{"id": 2, "name": "adopt-cf", "serial": "", "custom_fields": map[string]any{"chassis_serial": "FROM-CF"}},
		// Branch: cf set, built-in == cf → clear cf only
		{"id": 3, "name": "redundant-cf", "serial": "MATCH-3", "custom_fields": map[string]any{"chassis_serial": "MATCH-3"}},
		// Branch: cf set, built-in != cf, both non-empty → conflict, leave alone
		{"id": 4, "name": "conflict-4", "serial": "BUILT-4", "custom_fields": map[string]any{"chassis_serial": "DIFFERENT"}},
	}

	captured := map[int]map[string]any{}
	srv := chassisSerialFixtureServer(t, devices, &captured)
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	// Dry-run first: examined counts populated, no PATCH bodies captured.
	dryResult, err := c.MigrateChassisSerial(context.Background(), true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dryResult.Examined != 4 {
		t.Errorf("dry: Examined=%d, want 4", dryResult.Examined)
	}
	if dryResult.Skipped != 1 {
		t.Errorf("dry: Skipped=%d, want 1", dryResult.Skipped)
	}
	if len(dryResult.Migrated) != 2 {
		t.Errorf("dry: Migrated=%d, want 2", len(dryResult.Migrated))
	}
	if len(dryResult.Conflicts) != 1 {
		t.Errorf("dry: Conflicts=%d, want 1", len(dryResult.Conflicts))
	}
	if len(captured) != 0 {
		t.Errorf("dry: expected no PATCHes, got %d", len(captured))
	}

	// Apply: same counts, now with two PATCHes captured.
	captured = map[int]map[string]any{}
	srv2 := chassisSerialFixtureServer(t, devices, &captured)
	defer srv2.Close()
	c2 := NewClient(ClientConfig{BaseURL: srv2.URL, Token: "t"})

	result, err := c2.MigrateChassisSerial(context.Background(), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Migrated) != 2 || len(result.Conflicts) != 1 {
		t.Errorf("apply: migrated=%d conflicts=%d", len(result.Migrated), len(result.Conflicts))
	}

	// Adopt-cf device: PATCH should set serial AND clear cf.
	if got := captured[2]; got == nil {
		t.Errorf("apply: missing PATCH for device 2")
	} else {
		if got["serial"] != "FROM-CF" {
			t.Errorf("apply device 2: serial=%v, want FROM-CF", got["serial"])
		}
		cf, _ := got["custom_fields"].(map[string]any)
		if cf == nil || cf["chassis_serial"] != nil {
			t.Errorf("apply device 2: chassis_serial cf should be nil, got %v", cf)
		}
	}

	// Redundant-cf device: PATCH should clear cf only, NOT touch serial.
	if got := captured[3]; got == nil {
		t.Errorf("apply: missing PATCH for device 3")
	} else {
		if _, hasSerial := got["serial"]; hasSerial {
			t.Errorf("apply device 3: should not patch serial, got %v", got)
		}
		cf, _ := got["custom_fields"].(map[string]any)
		if cf == nil || cf["chassis_serial"] != nil {
			t.Errorf("apply device 3: chassis_serial cf should be nil, got %v", cf)
		}
	}

	// Conflict device 4: must NOT have been patched.
	if _, ok := captured[4]; ok {
		t.Errorf("apply device 4: conflict should leave device untouched, got %v", captured[4])
	}

	// Idempotency: simulate what NetBox would look like after a real
	// apply by building a post-apply fixture (cf cleared on devices the
	// migration patched), then rerun the migration. The expected
	// outcome is "all skipped, nothing migrated, conflict still
	// conflicts" — which is the actual idempotency claim.
	postApply := []map[string]any{
		// Device 1: unchanged (was already skipped — empty cf).
		{"id": 1, "name": "skip-empty-cf", "serial": "BUILT-1", "custom_fields": map[string]any{}},
		// Device 2: cf cleared, built-in serial now FROM-CF.
		{"id": 2, "name": "adopt-cf", "serial": "FROM-CF", "custom_fields": map[string]any{"chassis_serial": nil}},
		// Device 3: cf cleared, built-in unchanged.
		{"id": 3, "name": "redundant-cf", "serial": "MATCH-3", "custom_fields": map[string]any{"chassis_serial": nil}},
		// Device 4: conflict was left untouched, still has both values.
		{"id": 4, "name": "conflict-4", "serial": "BUILT-4", "custom_fields": map[string]any{"chassis_serial": "DIFFERENT"}},
	}
	rerunCaptured := map[int]map[string]any{}
	srv3 := chassisSerialFixtureServer(t, postApply, &rerunCaptured)
	defer srv3.Close()
	c3 := NewClient(ClientConfig{BaseURL: srv3.URL, Token: "t"})

	rerun, err := c3.MigrateChassisSerial(context.Background(), false)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rerun.Skipped != 3 {
		t.Errorf("rerun: Skipped=%d, want 3 (devices 1,2,3 — all cleared cfs)", rerun.Skipped)
	}
	if len(rerun.Migrated) != 0 {
		t.Errorf("rerun: Migrated=%d, want 0 (nothing to do)", len(rerun.Migrated))
	}
	if len(rerun.Conflicts) != 1 {
		t.Errorf("rerun: Conflicts=%d, want 1 (device 4 still conflicts)", len(rerun.Conflicts))
	}
	if len(rerunCaptured) != 0 {
		t.Errorf("rerun: expected no PATCHes (idempotent), got %d", len(rerunCaptured))
	}
}

// TestMigrateChassisSerial_JunkCFCleared verifies DMI placeholders
// in chassis_serial cf are not adopted into the built-in serial:
// when built-in is empty/junk we just clear the cf instead.
func TestMigrateChassisSerial_JunkCFCleared(t *testing.T) {
	devices := []map[string]any{
		// Junk cf, empty built-in: clear cf only, do not adopt.
		{"id": 1, "name": "junk-cf-empty-built-in", "serial": "", "custom_fields": map[string]any{"chassis_serial": "Default string"}},
		// Junk cf with a real built-in serial: real serial wins, but
		// builtIn != cfChassis so this falls into the conflict branch
		// (correct — operator should look at it).
		{"id": 2, "name": "junk-cf-real-built-in", "serial": "REAL-SN-2", "custom_fields": map[string]any{"chassis_serial": "To Be Filled By O.E.M."}},
	}

	captured := map[int]map[string]any{}
	srv := chassisSerialFixtureServer(t, devices, &captured)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	result, err := c.MigrateChassisSerial(context.Background(), false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Device 1: cf cleared; serial NOT set to the placeholder.
	if got := captured[1]; got == nil {
		t.Fatalf("missing PATCH for device 1")
	} else {
		if _, hasSerial := got["serial"]; hasSerial {
			t.Errorf("device 1: serial should not be patched (junk cf), got %v", got["serial"])
		}
		cf, _ := got["custom_fields"].(map[string]any)
		if cf == nil || cf["chassis_serial"] != nil {
			t.Errorf("device 1: chassis_serial cf should be cleared, got %v", cf)
		}
	}

	// Device 2: surfaces as conflict.
	if len(result.Conflicts) != 1 || result.Conflicts[0].ID != 2 {
		t.Errorf("device 2: should be 1 conflict on id=2, got %v", result.Conflicts)
	}
	if _, ok := captured[2]; ok {
		t.Errorf("device 2: conflict should leave device untouched, got %v", captured[2])
	}
}

func TestReadStringCF(t *testing.T) {
	cases := []struct {
		raw  string
		key  string
		want string
	}{
		{`{}`, "x", ""},
		{`{"x": "hello"}`, "x", "hello"},
		{`{"x": null}`, "x", ""},
		{`{"x": 42}`, "x", ""},
		{``, "x", ""},
	}
	for _, c := range cases {
		got := readStringCF(json.RawMessage(c.raw), c.key)
		if got != c.want {
			t.Errorf("readStringCF(%q, %q) = %q, want %q", c.raw, c.key, got, c.want)
		}
	}
}
