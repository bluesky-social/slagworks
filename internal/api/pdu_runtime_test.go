package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestParseOutletIndex_StrictAndFailClosed pins the outlet-name parse.
// This is safety-critical: a sloppy parse would actuate the wrong
// outlet (cutting power to a different device on the same PDU strip).
// The convention is exactly "Outlet <n>", 1-based, anchored.
func TestParseOutletIndex_StrictAndFailClosed(t *testing.T) {
	ok := map[string]int{
		"Outlet 1":  1,
		"Outlet 5":  5,
		"Outlet 24": 24,
	}
	for name, want := range ok {
		got, err := parseOutletIndex(name)
		if err != nil {
			t.Errorf("%q: unexpected error %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %d, want %d", name, got, want)
		}
	}

	bad := []string{
		"",            // empty
		"Outlet",      // no number
		"Outlet 0",    // 0 is not a valid 1-based outlet
		"Outlet 5x",   // trailing junk
		"outlet 5",    // wrong case
		"Outlet  5",   // double space
		"AB5",         // different convention
		"5",           // bare number
		"Outlet 5 ",   // trailing space
		" Outlet 5",   // leading space
		"Port 5",      // wrong word
		"Outlet -1",   // negative
		"Outlet 5\n6", // anchored: must reject embedded newline injection
	}
	for _, name := range bad {
		if got, err := parseOutletIndex(name); err == nil {
			t.Errorf("%q: expected error, got index %d", name, got)
		}
	}
}

// TestResolvePDURuntime_NoNetBox verifies that PDU resolution fails
// loudly without NetBox — unlike BMC resolution, which soft-fails,
// PDU resolution is only invoked on explicit --via pdu, so a missing
// prerequisite is a hard error rather than a silent degrade.
func TestResolvePDURuntime_NoNetBox(t *testing.T) {
	s := &Server{}
	var params model.OpParams
	if err := s.resolvePDURuntime(context.Background(), uuid.New(), &params); err == nil {
		t.Fatal("expected error when NetBox is not configured")
	}
}

// TestPDURuntime_Shape pins the envelope wire format. The worker
// unmarshals op.Params["pdu_runtime"].outlets into model.PDUOutlet;
// renaming a JSON field here without the worker silently breaks
// actuation. resolvePDURuntime now stamps params.PDURuntime directly
// (no merge dance), so the shape is pinned by marshalling a populated
// OpParams and round-tripping the pdu_runtime block — plus confirming
// it composes with a sibling op-type field.
func TestPDURuntime_Shape(t *testing.T) {
	rt := model.PDURuntime{Outlets: []model.PDUOutlet{
		{PDUHost: "10.254.240.154", Outlet: 5, Serial: "ZA2451003321", Username: "apc", Password: "pw"},
	}}

	params := model.OpParams{BootDevice: "pxe", PDURuntime: &rt}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["boot_device"]; !ok {
		t.Errorf("marshalled params dropped boot_device key: %s", raw)
	}
	pduRaw, ok := obj["pdu_runtime"]
	if !ok {
		t.Fatalf("missing pdu_runtime key: %s", raw)
	}
	var got model.PDURuntime
	if err := json.Unmarshal(pduRaw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Outlets) != 1 || got.Outlets[0].PDUHost != "10.254.240.154" || got.Outlets[0].Outlet != 5 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
