package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

func TestExtractBMCRuntime_FullPayload(t *testing.T) {
	params := json.RawMessage(`{
		"destructive": true,
		"bmc_runtime": {
			"expected_chassis_serial": "WM235S600435",
			"extra_config": {"ipmi_cipher": 3}
		}
	}`)
	rt, err := extractBMCRuntime(params)
	if err != nil {
		t.Fatal(err)
	}
	if rt.ExpectedChassisSerial != "WM235S600435" {
		t.Errorf("serial: got %q", rt.ExpectedChassisSerial)
	}
	if rt.IpmiCipher() != 3 {
		t.Errorf("cipher: got %d, want 3", rt.IpmiCipher())
	}
}

// TestExtractBMCRuntime_EmptyParams covers ops created before chunk 3
// landed: their params are nil, and the worker still needs to handle
// them (for now — chunk 4 makes serial-check required).
func TestExtractBMCRuntime_EmptyParams(t *testing.T) {
	rt, err := extractBMCRuntime(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.ExpectedChassisSerial != "" {
		t.Errorf("expected empty serial, got %q", rt.ExpectedChassisSerial)
	}
	if rt.IpmiCipher() != 0 {
		t.Errorf("expected cipher=0 (default), got %d", rt.IpmiCipher())
	}
}

// TestExtractBMCRuntime_NoBMCBlock — op.Params is a JSON object but
// has no bmc_runtime field. Older op shapes look like this; treat
// like the empty case.
func TestExtractBMCRuntime_NoBMCBlock(t *testing.T) {
	rt, err := extractBMCRuntime(json.RawMessage(`{"destructive": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if rt.ExpectedChassisSerial != "" || rt.IpmiCipher() != 0 {
		t.Errorf("expected zero value, got %+v", rt)
	}
}

// TestExtractBMCRuntime_NonObjectPayload — params can legitimately be
// non-JSON-object (e.g. a literal `null`); we must not error on
// those, but we also must not return garbage.
func TestExtractBMCRuntime_NonObjectPayload(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `"string"`, `42`} {
		rt, err := extractBMCRuntime(json.RawMessage(payload))
		if err != nil {
			t.Errorf("payload %q: unexpected error %v", payload, err)
		}
		if rt.ExpectedChassisSerial != "" || rt.ExtraConfig != nil {
			t.Errorf("payload %q: expected zero value, got %+v", payload, rt)
		}
	}
}

// stubController implements power.Controller minimally for serial-check tests.
// Only ChassisSerial behavior matters; the other methods exist for the
// interface contract and panic-poison if exercised.
type stubController struct {
	serial string
	err    error
}

func (s *stubController) PowerOn(_ context.Context) error    { return nil }
func (s *stubController) PowerOff(_ context.Context) error   { return nil }
func (s *stubController) PowerCycle(_ context.Context) error { return nil }
func (s *stubController) PowerStatus(_ context.Context) (power.State, error) {
	return power.StateOn, nil
}
func (s *stubController) SetBootDevice(_ context.Context, _ power.BootDevice) error { return nil }
func (s *stubController) Close(_ context.Context) error                             { return nil }
func (s *stubController) ChassisSerial(_ context.Context) (string, error)           { return s.serial, s.err }
func (s *stubController) Transport() string                                         { return "stub" }

// nullLog drops log output so test stderr stays clean; verifyChassisSerial
// only logs warnings, never errors-as-logs.
func nullLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestVerifyChassisSerial_Match(t *testing.T) {
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{serial: "WM235S600435"}
	if err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestVerifyChassisSerial_CaseInsensitiveMatch(t *testing.T) {
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{serial: "wm235s600435"} // some firmware lowercases
	if err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog()); err != nil {
		t.Errorf("case-insensitive comparison should match, got %v", err)
	}
}

func TestVerifyChassisSerial_TrimsWhitespace(t *testing.T) {
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{serial: "  WM235S600435\n"} // some firmware pads
	if err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog()); err != nil {
		t.Errorf("whitespace should be trimmed, got %v", err)
	}
}

func TestVerifyChassisSerial_Mismatch_Refuses(t *testing.T) {
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{serial: "WM999S999999"}
	err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog())
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	// Both serials should be in the error so an operator can diagnose
	// from a single log line.
	msg := err.Error()
	if !strings.Contains(msg, "WM235S600435") || !strings.Contains(msg, "WM999S999999") {
		t.Errorf("error should name both serials; got: %v", err)
	}
}

func TestVerifyChassisSerial_NoExpected_Skips(t *testing.T) {
	// Empty ExpectedChassisSerial means "NetBox didn't know" — for v1
	// we warn and proceed. (Spec calls for hard refusal once
	// monitor-bootstrap lands; not a v1 property yet.)
	rt := model.BMCRuntime{ExpectedChassisSerial: ""}
	ctrl := &stubController{serial: "anything"}
	if err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog()); err != nil {
		t.Errorf("expected nil when no expected serial, got %v", err)
	}
}

func TestVerifyChassisSerial_ControllerEmpty_SkipsWithWarning(t *testing.T) {
	// Controllers like PDU return ("", nil) — they have no chassis
	// concept. Treat as "no check possible".
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{serial: ""}
	if err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog()); err != nil {
		t.Errorf("empty controller serial should not block, got %v", err)
	}
}

func TestVerifyChassisSerial_InventoryError_Refuses(t *testing.T) {
	rt := model.BMCRuntime{ExpectedChassisSerial: "WM235S600435"}
	ctrl := &stubController{err: errors.New("redfish 500")}
	err := verifyChassisSerial(context.Background(), ctrl, rt, nullLog())
	if err == nil {
		t.Fatal("expected error when inventory fails, got nil")
	}
	if !strings.Contains(err.Error(), "redfish 500") {
		t.Errorf("error should wrap underlying inventory failure; got: %v", err)
	}
}
