package model

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestOpParamsWireKeys pins the exact set of JSON keys a fully-populated
// OpParams renders. The params blob crosses four boundaries with no
// shared Go type before OpParams existed — the DB jsonb row, the WS
// SiteSnapshot, the httpboot kernel-cmdline derivation, and slagctl
// display. A silent key rename or drop here would break a consumer with
// no compile-time signal. Every field is set (including omitempty ones)
// so they all render; the nested runtime blocks are pinned by their own
// key sets below.
func TestOpParamsWireKeys(t *testing.T) {
	p := OpParams{
		Destructive:      true,
		WipeAllDisks:     true,
		Recovery:         true,
		FatalAt:          "installing_bootloader",
		Efivarfs:         "skip",
		DataMarker:       "marker-token",
		BootDevice:       "pxe",
		Device:           "disk",
		Persistent:       true,
		RotateMonitor:    true,
		BMCRuntime:       &BMCRuntime{ExpectedChassisSerial: "SN1", Username: "u", Password: "pw", ExtraConfig: map[string]any{"ipmi_cipher": 3}},
		PDURuntime:       &PDURuntime{Outlets: []PDUOutlet{{PDUHost: "10.0.0.1", Outlet: 5, Serial: "PS1", Username: "apc", Password: "pw"}}, DownstreamSerial: "SN-DOWNSTREAM", Outlet: true, AllowSelf: true},
		AMTRuntime:       &AMTRuntime{Host: "10.252.240.23", Username: "admin", Password: "pw"},
		TailscaleRuntime: &TailscaleRuntime{AuthKey: "tskey-abc", Hostname: "h-pop1", Tags: []string{"tag:pop1"}},
		MonitorRuntime:   &MonitorRuntime{Password: "pw"},
	}

	assertKeys(t, mustMarshalObj(t, p), []string{
		"destructive", "wipe_all_disks", "recovery", "fatal_at", "efivarfs", "data_marker",
		"boot_device", "device", "persistent", "rotate_monitor",
		"bmc_runtime", "pdu_runtime", "amt_runtime", "tailscale_runtime", "monitor_runtime",
	})

	// Nested BMCRuntime: expected_chassis_serial and extra_config always
	// render (no omitempty — consumers rely on the keys existing).
	var top map[string]json.RawMessage
	if err := json.Unmarshal(mustMarshalObj(t, p), &top); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, top["bmc_runtime"], []string{
		"expected_chassis_serial", "username", "password", "extra_config",
	})
	assertKeys(t, top["pdu_runtime"], []string{"outlets", "downstream_serial", "outlet", "allow_self"})
	var pdu struct {
		Outlets []json.RawMessage `json:"outlets"`
	}
	if err := json.Unmarshal(top["pdu_runtime"], &pdu); err != nil {
		t.Fatal(err)
	}
	if len(pdu.Outlets) != 1 {
		t.Fatalf("want 1 outlet, got %d", len(pdu.Outlets))
	}
	assertKeys(t, pdu.Outlets[0], []string{
		"pdu_host", "outlet", "serial", "username", "password",
	})
	assertKeys(t, top["amt_runtime"], []string{"host", "username", "password"})
	assertKeys(t, top["tailscale_runtime"], []string{"auth_key", "hostname", "tags"})
	assertKeys(t, top["monitor_runtime"], []string{"password"})
}

// TestOpParamsEmptyOmitsAllKeys confirms a commission-shaped params-less
// op (zero OpParams) marshals to "{}" — every field omitempty except the
// nested-block keys, which are nil pointers here. This is the shape the
// boot handler's non-destructive path relies on (no destructive/recovery
// tokens leak onto a plain commission).
func TestOpParamsEmptyOmitsAllKeys(t *testing.T) {
	raw, err := json.Marshal(OpParams{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("empty OpParams should marshal to {}, got %s", raw)
	}
}

// TestDecodeParams_Tolerant confirms the tolerant decode: empty and
// non-object blobs decode to the zero value with no error (matching the
// pre-typing extractors' shape-probe), and a malformed object errors.
func TestDecodeParams_Tolerant(t *testing.T) {
	for _, in := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null"), json.RawMessage("[1,2]"), json.RawMessage(`"s"`), json.RawMessage("42")} {
		p, err := DecodeParams(in)
		if err != nil {
			t.Errorf("DecodeParams(%q): unexpected error %v", in, err)
		}
		if !reflect.DeepEqual(p, OpParams{}) {
			t.Errorf("DecodeParams(%q): want zero, got %+v", in, p)
		}
	}
	if _, err := DecodeParams(json.RawMessage(`{"bmc_runtime": 42}`)); err == nil {
		t.Error("malformed object should error")
	}
}

// TestOpResultWireKeys pins the result blob's key set. The worker's
// failure paths (marshalError, MarkOpFailed) produce this shape; slagctl
// reads Error back off it.
func TestOpResultWireKeys(t *testing.T) {
	assertKeys(t, OpResult{Error: "boom", Phase: "finalizing"}.Marshal(),
		[]string{"error", "phase"})
	// Empty result omits every key (a power-op success leaves the row untouched).
	if string(OpResult{}.Marshal()) != "{}" {
		t.Errorf("empty OpResult should marshal to {}, got %s", OpResult{}.Marshal())
	}
	full := OpResult{BMCReconcile: &BMCReconcileResult{
		MonitorSlot:            4,
		MonitorRotated:         true,
		MonitorPasswordWritten: true,
		LeftoversDisabled:      []string{"slagworks"},
		BMCTimeCompliance: BMCTimeCompliance{
			NTPServers:       []string{"10.254.240.1"},
			NTPConfigured:    true,
			TimezoneSet:      true,
			ClockStepped:     true,
			ClockSkewSeconds: -3600,
			TimeError:        "boom",
		},
	}}
	assertKeys(t, full.Marshal(), []string{"bmc_reconcile"})
	// monitor_rotated and monitor_password_written render even when false: a
	// result must say plainly that the password was re-asserted, not
	// rotated, and whether the BMC needed it written.
	var notRotated map[string]json.RawMessage
	if err := json.Unmarshal(OpResult{BMCReconcile: &BMCReconcileResult{MonitorSlot: 4}}.Marshal(), &notRotated); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"monitor_rotated":false`, `"monitor_password_written":false`} {
		if !strings.Contains(string(notRotated["bmc_reconcile"]), key) {
			t.Errorf("%s must render: %s", key, notRotated["bmc_reconcile"])
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(full.Marshal(), &top); err != nil {
		t.Fatal(err)
	}
	// The time-compliance facts sit flat beside the account facts.
	assertKeys(t, top["bmc_reconcile"], []string{
		"monitor_slot", "monitor_rotated", "monitor_password_written", "leftover_users_disabled",
		"ntp_servers", "ntp_configured", "timezone_set", "clock_stepped", "clock_skew_seconds", "time_error",
	})
}

// TestBMCReconcileReportResult_CarriesTime: the persisted result keeps the
// report's time-compliance outcome, and a time failure alone never becomes
// the op's error (time compliance does not fail the op; see
// BMCTimeCompliance).
func TestBMCReconcileReportResult_CarriesTime(t *testing.T) {
	tc := BMCTimeCompliance{NTPServers: []string{"10.253.240.1"}, ClockStepped: true, ClockSkewSeconds: 86400, TimeError: "set timezone: HTTP 400"}
	res := BMCReconcileReport{
		Monitor:           &BMCUserCred{Username: "monitor", Password: "x", IPMIUserSlot: 3},
		BMCTimeCompliance: tc,
	}.Result()
	if res.Error != "" {
		t.Errorf("a time failure must not become the op error, got %q", res.Error)
	}
	if res.BMCReconcile == nil || !reflect.DeepEqual(res.BMCReconcile.BMCTimeCompliance, tc) {
		t.Errorf("result time compliance = %+v, want %+v", res.BMCReconcile, tc)
	}
}

// TestBMCReconcileReportResult_NoSecret pins that the op result derived
// from a bmc_reconcile report — the only part of the report that is
// persisted in the operations table and echoed in op logs — never carries
// the monitor password, on the success and the partial-failure shapes.
func TestBMCReconcileReportResult_NoSecret(t *testing.T) {
	const pw = "Zq7!monitorPW"
	for _, rep := range []BMCReconcileReport{
		{Monitor: &BMCUserCred{Username: "monitor", Password: pw, IPMIUserSlot: 4}, MonitorRotated: true, MonitorPasswordWritten: true, LeftoversDisabled: []string{"slagworks"}},
		{Monitor: &BMCUserCred{Username: "monitor", Password: pw, IPMIUserSlot: 4}, MonitorRotated: true, MonitorPasswordWritten: true, Error: "bmcreconcile: disable leftover slot 5: boom"},
		{Monitor: &BMCUserCred{Username: "monitor", IPMIUserSlot: 4}, MonitorPasswordWritten: true},
		{Monitor: &BMCUserCred{Username: "monitor", IPMIUserSlot: 4}},
	} {
		res := rep.Result()
		raw := string(res.Marshal())
		if strings.Contains(raw, pw) {
			t.Fatalf("op result leaks the monitor password: %s", raw)
		}
		if res.BMCReconcile == nil || res.BMCReconcile.MonitorSlot != 4 || res.BMCReconcile.MonitorRotated != rep.MonitorRotated ||
			res.BMCReconcile.MonitorPasswordWritten != rep.MonitorPasswordWritten {
			t.Errorf("result should carry the monitor slot, whether it was rotated and whether it was written: %s", raw)
		}
		if res.Error != rep.Error {
			t.Errorf("result error = %q, want %q", res.Error, rep.Error)
		}
	}
}

func mustMarshalObj(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func assertKeys(t *testing.T, raw json.RawMessage, want []string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("key set mismatch:\n got: %v\nwant: %v\nraw: %s", got, want, raw)
	}
}

// TestDecodeParams_PDURuntimeNullFailsClosed pins the transport-safety
// contract: pdu_runtime is the PDU-vs-BMC discriminator, so a literal
// null must error rather than decode as absent and silently route the
// op down the BMC path (the pre-typing extractor failed closed on null
// via a non-nil zero runtime with no outlets).
func TestDecodeParams_PDURuntimeNullFailsClosed(t *testing.T) {
	_, err := DecodeParams([]byte(`{"pdu_runtime": null}`))
	if err == nil {
		t.Fatal("expected error on pdu_runtime:null, got nil (silent BMC fallback)")
	}
}

// TestDecodeParams_AMTRuntimeNullFailsClosed: amt_runtime is the AMT-vs-BMC
// discriminator, so a literal null must error rather than decode as absent
// and route an AMT machine's op down the IPMI/Redfish path.
func TestDecodeParams_AMTRuntimeNullFailsClosed(t *testing.T) {
	_, err := DecodeParams([]byte(`{"amt_runtime": null}`))
	if err == nil {
		t.Fatal("expected error on amt_runtime:null, got nil (silent BMC fallback)")
	}
}

// TestDecodeParams_TransportNullInOtherCaseFailsClosed: encoding/json matches
// {"PDU_Runtime": null} to pdu_runtime and leaves PDURuntime nil, while the
// null check above looks the key up by its exact spelling, so the op would
// fall back to the BMC transport after all. DecodeParams refuses the
// spelling instead.
func TestDecodeParams_TransportNullInOtherCaseFailsClosed(t *testing.T) {
	for _, raw := range []string{`{"PDU_Runtime": null}`, `{"AMT_RUNTIME": null}`} {
		if _, err := DecodeParams([]byte(raw)); err == nil {
			t.Errorf("DecodeParams(%s): got nil error, want a refusal (silent BMC fallback)", raw)
		}
	}
}
