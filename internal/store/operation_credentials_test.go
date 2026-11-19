package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// credentialParams is an op params blob carrying every kind of credential,
// each a distinct fixture value, beside the non-secret fields an operator
// still needs to see.
func credentialParams() model.OpParams {
	return model.OpParams{
		Destructive: true,
		BMCRuntime: &model.BMCRuntime{ExpectedChassisSerial: "SN1", Username: "ADMIN", Password: "fixture-bmc-pw",
			ExtraConfig: map[string]any{"ipmi_cipher": float64(17)}},
		PDURuntime: &model.PDURuntime{Outlets: []model.PDUOutlet{
			{PDUHost: "10.0.0.1", Outlet: 5, Serial: "PS1", Username: "apc", Password: "fixture-pdu-pw-1"},
			{PDUHost: "10.0.0.2", Outlet: 7, Serial: "PS2", Username: "apc", Password: "fixture-pdu-pw-2"},
		}, DownstreamSerial: "SN-DOWN", Outlet: true},
		AMTRuntime:       &model.AMTRuntime{Host: "10.252.240.23", Username: "admin", Password: "fixture-amt-pw"},
		TailscaleRuntime: &model.TailscaleRuntime{AuthKey: "fixture-tskey", Hostname: "h-pop1", Tags: []string{"tag:pop1"}},
		MonitorRuntime:   &model.MonitorRuntime{Password: "fixture-monitor-pw"},
		RotateMonitor:    true,
	}
}

var credentialFixtures = []string{
	"fixture-bmc-pw", "fixture-pdu-pw-1", "fixture-pdu-pw-2", "fixture-amt-pw", "fixture-tskey", "fixture-monitor-pw",
}

// heldCredentials counts the fixture credentials in raw, so failure
// messages report a number instead of a credential.
func heldCredentials(raw []byte) int {
	n := 0
	for _, c := range credentialFixtures {
		if bytes.Contains(raw, []byte(c)) {
			n++
		}
	}
	return n
}

func mustMarshalParams(t *testing.T, p model.OpParams) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seedOp inserts an op with the given status and params on a fresh machine
// (idx_one_active_op_per_machine allows one active op per machine).
func seedOp(t *testing.T, st *Store, status model.OperationStatus, params json.RawMessage) *model.Operation {
	t.Helper()
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-" + uuid.NewString()[:8], State: model.MachineStateRunning}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{MachineID: m.ID, Type: model.OpPowerCycle, Status: status, Params: params, Version: 3}
	if status == model.OpCompleted || status == model.OpFailed {
		done := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
		op.CompletedAt = &done
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	return op
}

// TestScrubTerminalOperationCredentials_StripsTerminalKeepsActive: every
// credential leaves the params of completed and failed ops, every other
// field (and every other column) stays, active ops keep their credentials
// for the worker, and a second run finds nothing to do.
func TestScrubTerminalOperationCredentials_StripsTerminalKeepsActive(t *testing.T) {
	st := testStore(t)
	full := credentialParams()
	raw := mustMarshalParams(t, full)

	completed := seedOp(t, st, model.OpCompleted, raw)
	failed := seedOp(t, st, model.OpFailed, raw)
	var active []*model.Operation
	for _, s := range []model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning} {
		active = append(active, seedOp(t, st, s, raw))
	}

	n, err := st.ScrubTerminalOperationCredentials()
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if n != 2 {
		t.Errorf("scrubbed %d rows, want 2 (the completed and the failed op)", n)
	}

	want := full
	bmc, pdu, amt, ts, mon := *full.BMCRuntime, *full.PDURuntime, *full.AMTRuntime, *full.TailscaleRuntime, *full.MonitorRuntime
	pdu.Outlets = []model.PDUOutlet{full.PDURuntime.Outlets[0], full.PDURuntime.Outlets[1]}
	bmc.Password, amt.Password, ts.AuthKey, mon.Password = "", "", "", ""
	for i := range pdu.Outlets {
		pdu.Outlets[i].Password = ""
	}
	want.BMCRuntime, want.PDURuntime, want.AMTRuntime, want.TailscaleRuntime, want.MonitorRuntime = &bmc, &pdu, &amt, &ts, &mon

	for _, seeded := range []*model.Operation{completed, failed} {
		got, err := st.GetOperation(seeded.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c := heldCredentials(got.Params); c > 0 {
			t.Errorf("%s op still stores %d credential(s)", got.Status, c)
			continue
		}
		if red := model.RedactParams(got.Params); !bytes.Equal(red, got.Params) {
			t.Errorf("%s op: the redaction still finds a credential key after the scrub: %s", got.Status, got.Params)
		}
		p, err := got.DecodeParams()
		if err != nil {
			t.Fatalf("%s op: scrubbed params no longer decode: %v", got.Status, err)
		}
		if !reflect.DeepEqual(p, want) {
			t.Errorf("%s op: scrub changed non-secret params:\n got %+v\nwant %+v", got.Status, p, want)
		}
		if got.Status != seeded.Status || got.Version != seeded.Version ||
			got.CompletedAt == nil || !got.CompletedAt.Equal(*seeded.CompletedAt) {
			t.Errorf("%s op: scrub touched columns other than params: status=%s version=%d completed_at=%v",
				seeded.Status, got.Status, got.Version, got.CompletedAt)
		}
	}
	for _, seeded := range active {
		got, err := st.GetOperation(seeded.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c := heldCredentials(got.Params); c != len(credentialFixtures) {
			t.Errorf("%s op holds %d of %d credentials; an active op must keep them for the worker",
				got.Status, c, len(credentialFixtures))
		}
	}

	n, err = st.ScrubTerminalOperationCredentials()
	if err != nil {
		t.Fatalf("second scrub: %v", err)
	}
	if n != 0 {
		t.Errorf("second scrub rewrote %d rows; already-clean rows must be excluded", n)
	}
}

// TestScrubTerminalOperationCredentials_OddShapesDoNotBlock: params the
// typed union never produces (SQL NULL, JSON null, a non-object, an array
// where an object belongs) must not fail the single statement, so one odd
// row cannot keep every other terminal op's credentials in the table.
func TestScrubTerminalOperationCredentials_OddShapesDoNotBlock(t *testing.T) {
	st := testStore(t)
	odd := map[string]json.RawMessage{
		"sql null":            nil,
		"json null":           json.RawMessage(`null`),
		"array":               json.RawMessage(`[{"bmc_runtime":{"password":"x"}}]`),
		"runtime is an array": json.RawMessage(`{"bmc_runtime":[{"password":"x"}]}`),
		"runtime is a string": json.RawMessage(`{"amt_runtime":"x"}`),
		"outlets is a string": json.RawMessage(`{"pdu_runtime":{"outlets":"x"}}`),
		"no outlets":          json.RawMessage(`{"pdu_runtime":{"outlets":[]}}`),
	}
	oddOps := map[string]*model.Operation{}
	for name, params := range odd {
		oddOps[name] = seedOp(t, st, model.OpFailed, params)
	}
	mixed := seedOp(t, st, model.OpCompleted,
		json.RawMessage(`{"pdu_runtime":{"outlets":["s",{"pdu_host":"10.0.0.1","password":"fixture-pdu-pw-1"}]}}`))
	normal := seedOp(t, st, model.OpCompleted, mustMarshalParams(t, credentialParams()))

	n, err := st.ScrubTerminalOperationCredentials()
	if err != nil {
		t.Fatalf("scrub failed on odd params: %v", err)
	}
	if n != 2 {
		t.Errorf("scrubbed %d rows, want 2 (the mixed-outlets op and the normal op)", n)
	}
	for _, op := range []*model.Operation{mixed, normal} {
		got, err := st.GetOperation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c := heldCredentials(got.Params); c > 0 {
			t.Errorf("op %s still stores %d credential(s) next to odd rows", op.ID, c)
		}
	}
	gotMixed, err := st.GetOperation(mixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := json.RawMessage(`{"pdu_runtime":{"outlets":["s",{"pdu_host":"10.0.0.1"}]}}`); !jsonEqual(t, gotMixed.Params, want) {
		t.Errorf("mixed outlets = %s, want %s", gotMixed.Params, want)
	}
	for name, op := range oddOps {
		got, err := st.GetOperation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, got.Params, odd[name]) {
			t.Errorf("%s: scrub rewrote params it has no credential path into: %s", name, got.Params)
		}
	}
}

// TestScrubTerminalOperationCredentials_CaseVariantKeyYieldsNoCredential: the
// scrub removes credentials by exact jsonb path, so a terminal op whose params
// spell a credential key in another letter case keeps that value. Whatever
// the scrub leaves must not decode to a credential.
func TestScrubTerminalOperationCredentials_CaseVariantKeyYieldsNoCredential(t *testing.T) {
	st := testStore(t)
	op := seedOp(t, st, model.OpCompleted,
		json.RawMessage(`{"bmc_runtime":{"expected_chassis_serial":"SN1","username":"ADMIN","Password":"fixture-bmc-pw","extra_config":{}}}`))
	if _, err := st.ScrubTerminalOperationCredentials(); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := got.DecodeParams(); err == nil && p.BMCRuntime != nil && p.BMCRuntime.Password != "" {
		t.Error("a completed op still decodes to a BMC credential after the scrub")
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(av, bv)
}
