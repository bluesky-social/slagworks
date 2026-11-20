package commission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// reconcileMonitorPW is the monitor password every report fixture carries.
// Tests grep the persisted op result and the handler's log output for it.
const reconcileMonitorPW = "Xq9!rotatedMON"

type fakeOpPublisher struct {
	mu      sync.Mutex
	removed []model.Operation
}

func (p *fakeOpPublisher) PublishOpRemoved(op model.Operation, _ model.MachineState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, op)
}

// bmcReconcileFixture is deployCompleteFixture plus a captured logger and
// an op publisher, for the bmc_reconcile_report path.
func bmcReconcileFixture(t *testing.T) (*Handler, *fakeStore, uuid.UUID, *awssecrets.FakeMonitorWriter, *fakeOpPublisher, *bytes.Buffer) {
	t.Helper()
	h, st, id, w := deployCompleteFixture(t)
	pub := &fakeOpPublisher{}
	h.SetPublisher(pub)
	logs := &bytes.Buffer{}
	h.SetLogger(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return h, st, id, w, pub, logs
}

func reportObs(t *testing.T, machineID, opID uuid.UUID, rep model.BMCReconcileReport) model.Observation {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return model.Observation{MachineID: machineID, OperationID: opID, Event: "bmc_reconcile_report", Data: data}
}

func decodeFinalResult(t *testing.T, f finalizedOp) model.OpResult {
	t.Helper()
	r, err := model.DecodeResult(f.Result)
	if err != nil {
		t.Fatalf("decode finalized result %s: %v", f.Result, err)
	}
	return r
}

// assertNoSecret fails if the monitor password reached the persisted op
// result or any structured log line the handler emitted.
func assertNoSecret(t *testing.T, st *fakeStore, logs *bytes.Buffer) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, f := range st.finalized {
		if strings.Contains(string(f.Result), reconcileMonitorPW) {
			t.Errorf("monitor password persisted in the op result: %s", f.Result)
		}
	}
	if strings.Contains(logs.String(), reconcileMonitorPW) {
		t.Errorf("monitor password reached the structured logs:\n%s", logs.String())
	}
}

// TestHandleBMCReconcileReport_Success: the report's monitor cred is
// upserted into pop/<pop>/bmc-creds-monitor keyed by serial (with last-known
// hostname/IP), then the op completes with a result carrying only the slot,
// monitor_rotated and the disabled leftovers — never the password — and is
// published removed.
func TestHandleBMCReconcileReport_Success(t *testing.T) {
	h, st, id, w, pub, logs := bmcReconcileFixture(t)
	opID := uuid.New()
	obs := reportObs(t, id, opID, model.BMCReconcileReport{
		Monitor:                &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
		MonitorRotated:         true,
		MonitorPasswordWritten: true,
		LeftoversDisabled:      []string{"slagworks"},
	})
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}

	if len(st.bindingTypes) != 1 || st.bindingTypes[0] != model.OpBMCReconcile {
		t.Errorf("binding must be validated against bmc_reconcile, got %v", st.bindingTypes)
	}
	got, ok := w.Secrets["pop1"]["SN-X"]
	if !ok || got.Password != reconcileMonitorPW || got.Username != "monitor" {
		t.Fatalf("monitor entry keyed by serial SN-X: %+v", w.Secrets)
	}
	if got.LastKnownHostname != "host1.pop1" || got.LastKnownIP != "10.252.240.41" {
		t.Errorf("last_known fields: %+v", got)
	}

	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpCompleted || st.finalized[0].OpID != opID {
		t.Fatalf("expected op %s completed, got %+v", opID, st.finalized)
	}
	if st.finalized[0].Target != "" {
		t.Errorf("bmc_reconcile completion must not move machine state, got target %q", st.finalized[0].Target)
	}
	res := decodeFinalResult(t, st.finalized[0])
	if res.Error != "" || res.BMCReconcile == nil || res.BMCReconcile.MonitorSlot != 4 || !res.BMCReconcile.MonitorRotated ||
		!res.BMCReconcile.MonitorPasswordWritten ||
		len(res.BMCReconcile.LeftoversDisabled) != 1 || res.BMCReconcile.LeftoversDisabled[0] != "slagworks" {
		t.Errorf("result: %s", st.finalized[0].Result)
	}
	if len(pub.removed) != 1 || pub.removed[0].ID != opID {
		t.Errorf("op_removed not published for %s: %+v", opID, pub.removed)
	}
	assertNoSecret(t, st, logs)
}

// countingMonitorWriter is a MonitorWriter that counts upserts, so a test
// can assert global wrote no secret version at all.
type countingMonitorWriter struct{ upserts int }

func (w *countingMonitorWriter) GetMonitorEntry(context.Context, string, string) (*awssecrets.BMCMonitorEntry, error) {
	return nil, nil
}

func (w *countingMonitorWriter) UpsertMonitorEntry(context.Context, string, string, awssecrets.BMCMonitorEntry) error {
	w.upserts++
	return nil
}

func (w *countingMonitorWriter) EnsureMonitorSecret(context.Context, string) error { return nil }

// TestHandleBMCReconcileReport_NotRotated_NoSecretWrite: the worker
// re-asserted the stored monitor password (monitor_rotated false, no
// password in the report), either verifying the BMC already held it or
// writing it. Global already holds that password, so it must not write the
// secret at all (a same-value write still bumps the version), and the op
// completes with monitor_rotated:false and monitor_password_written as
// reported in its result and finalize log.
func TestHandleBMCReconcileReport_NotRotated_NoSecretWrite(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(fmt.Sprintf("written=%v", written), func(t *testing.T) {
			h, st, id, _, _, logs := bmcReconcileFixture(t)
			w := &countingMonitorWriter{}
			h.SetMonitorWriter(w)
			opID := uuid.New()
			obs := reportObs(t, id, opID, model.BMCReconcileReport{
				Monitor:                &model.BMCUserCred{Username: "monitor", IPMIUserSlot: 4},
				MonitorPasswordWritten: written,
				LeftoversDisabled:      []string{"maas"},
			})
			if err := h.HandleObservation(context.Background(), obs); err != nil {
				t.Fatalf("HandleObservation: %v", err)
			}
			if w.upserts != 0 {
				t.Errorf("a re-asserted password must not write the monitor secret; got %d upserts", w.upserts)
			}
			if len(st.finalized) != 1 || st.finalized[0].Status != model.OpCompleted {
				t.Fatalf("expected the op completed, got %+v", st.finalized)
			}
			res := decodeFinalResult(t, st.finalized[0])
			if res.Error != "" || res.BMCReconcile == nil || res.BMCReconcile.MonitorSlot != 4 ||
				res.BMCReconcile.MonitorRotated || res.BMCReconcile.MonitorPasswordWritten != written {
				t.Errorf("result: %s", st.finalized[0].Result)
			}
			for _, key := range []string{`"monitor_rotated":false`, fmt.Sprintf(`"monitor_password_written":%v`, written)} {
				if !strings.Contains(string(st.finalized[0].Result), key) {
					t.Errorf("result must say %s explicitly: %s", key, st.finalized[0].Result)
				}
				if !strings.Contains(logs.String(), key) {
					t.Errorf("finalize log should say %s:\n%s", key, logs.String())
				}
			}
			assertNoSecret(t, st, logs)
		})
	}
}

// TestHandleBMCReconcileReport_PreviousReleaseWorker_Stored: a worker still
// on the previous release always rotates and reports the password without
// monitor_rotated. Global must store it, or the BMC holds a password the
// store doesn't until the next reconcile.
func TestHandleBMCReconcileReport_PreviousReleaseWorker_Stored(t *testing.T) {
	h, st, id, _, _, _ := bmcReconcileFixture(t)
	w := &countingMonitorWriter{}
	h.SetMonitorWriter(w)
	obs := reportObs(t, id, uuid.New(), model.BMCReconcileReport{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "Abc123!xyzQw", IPMIUserSlot: 4},
	})
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if w.upserts != 1 {
		t.Errorf("a reported password must be stored; got %d upserts", w.upserts)
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpCompleted {
		t.Fatalf("expected the op completed, got %+v", st.finalized)
	}
}

// TestHandleBMCReconcileReport_RotatedWithoutPassword_Fails: a report that
// claims a rotation but carries no password breaks the contract; storing
// it would blank the host's monitor entry. Fail the op, write nothing.
func TestHandleBMCReconcileReport_RotatedWithoutPassword_Fails(t *testing.T) {
	h, st, id, _, _, _ := bmcReconcileFixture(t)
	w := &countingMonitorWriter{}
	h.SetMonitorWriter(w)
	if err := h.HandleObservation(context.Background(), reportObs(t, id, uuid.New(), model.BMCReconcileReport{
		Monitor:        &model.BMCUserCred{Username: "monitor", IPMIUserSlot: 4},
		MonitorRotated: true,
	})); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if w.upserts != 0 {
		t.Errorf("an empty rotated password must never be stored; got %d upserts", w.upserts)
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpFailed {
		t.Fatalf("expected the op failed, got %+v", st.finalized)
	}
	if res := decodeFinalResult(t, st.finalized[0]); !strings.Contains(res.Error, "bmc-reconcile") {
		t.Errorf("failure should say what to do: %q", res.Error)
	}
}

// TestHandleBMCReconcileReport_PartialFailure: reconcile wrote the monitor
// password, then failed (e.g. disabling a leftover). The live password must
// still be stored, and the op fails with the reconcile error.
func TestHandleBMCReconcileReport_PartialFailure(t *testing.T) {
	h, st, id, w, _, logs := bmcReconcileFixture(t)
	opID := uuid.New()
	obs := reportObs(t, id, opID, model.BMCReconcileReport{
		Monitor:        &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
		MonitorRotated: true,
		Error:          "bmcreconcile: disable leftover slot 5 (maas): exit status 1",
	})
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if w.Secrets["pop1"]["SN-X"].Password != reconcileMonitorPW {
		t.Fatal("the written monitor password must be stored even when reconcile failed afterwards")
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpFailed {
		t.Fatalf("expected the op failed, got %+v", st.finalized)
	}
	res := decodeFinalResult(t, st.finalized[0])
	if !strings.Contains(res.Error, "disable leftover slot 5") || res.BMCReconcile == nil || res.BMCReconcile.MonitorSlot != 4 {
		t.Errorf("result: %s", st.finalized[0].Result)
	}
	assertNoSecret(t, st, logs)
}

// TestHandleBMCReconcileReport_TimeErrorCompletesOp: a time-pass failure
// does not fail the op — the account compliance succeeded and its monitor
// password is stored — but the persisted result and the finalize log carry
// the time outcome so the operator sees it.
func TestHandleBMCReconcileReport_TimeErrorCompletesOp(t *testing.T) {
	h, st, id, w, _, logs := bmcReconcileFixture(t)
	opID := uuid.New()
	tc := model.BMCTimeCompliance{
		NTPServers: []string{"10.254.240.1"}, TimezoneSet: true, ClockSkewSeconds: 4000,
		TimeError: "bmc time compliance incomplete: the BMC accepted the clock step but is still 1h6m40s off",
	}
	obs := reportObs(t, id, opID, model.BMCReconcileReport{
		Monitor:           &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
		MonitorRotated:    true,
		BMCTimeCompliance: tc,
	})
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if w.Secrets["pop1"]["SN-X"].Password != reconcileMonitorPW {
		t.Fatal("monitor password not stored")
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpCompleted {
		t.Fatalf("expected the op completed despite the time error, got %+v", st.finalized)
	}
	res := decodeFinalResult(t, st.finalized[0])
	if res.Error != "" || res.BMCReconcile == nil || !reflect.DeepEqual(res.BMCReconcile.BMCTimeCompliance, tc) {
		t.Errorf("result: %s", st.finalized[0].Result)
	}
	if !strings.Contains(logs.String(), `"time_error":"bmc time compliance incomplete`) {
		t.Errorf("finalize log lacks the time error:\n%s", logs.String())
	}
	assertNoSecret(t, st, logs)
}

// TestHandleBMCReconcileReport_NoMonitor_Fails: a report with neither a
// monitor cred nor an error breaks the op's contract (a fresh monitor
// password); fail the op rather than complete it.
func TestHandleBMCReconcileReport_NoMonitor_Fails(t *testing.T) {
	h, st, id, w, _, _ := bmcReconcileFixture(t)
	if err := h.HandleObservation(context.Background(),
		reportObs(t, id, uuid.New(), model.BMCReconcileReport{})); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if len(w.Secrets) != 0 {
		t.Errorf("nothing to store, but AWS was written: %+v", w.Secrets)
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpFailed {
		t.Fatalf("expected the op failed, got %+v", st.finalized)
	}
	if res := decodeFinalResult(t, st.finalized[0]); !strings.Contains(res.Error, "monitor") {
		t.Errorf("failure should explain the missing monitor credential: %q", res.Error)
	}
}

// TestHandleBMCReconcileReport_StoreFailures covers every way the rotated
// password can fail to reach the monitor secret. Operator-fixable,
// retry-can't-help cases (secret missing, no writer, no serial) fail the op
// with a message saying the BMC's monitor password changed and what to do,
// and ack the observation. A transient AWS error returns non-terminal so the
// worker retries, leaving the op open.
func TestHandleBMCReconcileReport_StoreFailures(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(h *Handler)
		wantRetry   bool
		wantInError string
	}{
		{
			name: "monitor secret missing",
			setup: func(h *Handler) {
				h.SetMonitorWriter(&errorMonitorWriter{err: fmt.Errorf("pop/pop1/bmc-creds-monitor: %w", awssecrets.ErrMonitorSecretMissing)})
			},
			wantInError: "pop/pop1/bmc-creds-monitor",
		},
		{
			name:        "no monitor writer",
			setup:       func(h *Handler) { h.SetMonitorWriter(nil) },
			wantInError: "no monitor-secret writer",
		},
		{
			name: "device has no serial",
			setup: func(h *Handler) {
				nb := h.nb.(*fakeNetBox)
				nb.mu.Lock()
				nb.devices[42].Serial = ""
				nb.mu.Unlock()
			},
			wantInError: "serial",
		},
		{
			name:      "transient AWS error",
			setup:     func(h *Handler) { h.SetMonitorWriter(&errorMonitorWriter{err: errors.New("aws is down")}) },
			wantRetry: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, id, _, _, logs := bmcReconcileFixture(t)
			tc.setup(h)
			err := h.HandleObservation(context.Background(), reportObs(t, id, uuid.New(), model.BMCReconcileReport{
				Monitor:        &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
				MonitorRotated: true,
			}))
			if tc.wantRetry {
				if err == nil || errors.Is(err, ws.ErrTerminal) {
					t.Fatalf("transient error must surface non-terminal for retry, got %v", err)
				}
				if len(st.finalized) != 0 {
					t.Errorf("op must stay open while the report retries, got %+v", st.finalized)
				}
				assertNoSecret(t, st, logs)
				return
			}
			if err != nil {
				t.Fatalf("the op is failed with the reason, so the observation acks; got %v", err)
			}
			if len(st.finalized) != 1 || st.finalized[0].Status != model.OpFailed {
				t.Fatalf("expected the op failed, got %+v", st.finalized)
			}
			res := decodeFinalResult(t, st.finalized[0])
			for _, want := range []string{tc.wantInError, "bmc-reconcile"} {
				if !strings.Contains(res.Error, want) {
					t.Errorf("failure %q should mention %q", res.Error, want)
				}
			}
			assertNoSecret(t, st, logs)
		})
	}
}

// TestHandleBMCReconcileReport_BindingRejected: a report whose op is not
// an active bmc_reconcile op for this machine (fabricated, cross-host,
// wrong type, or already terminal) is dropped terminally and must NOT write
// the monitor secret — a stale report could otherwise overwrite a newer
// password.
func TestHandleBMCReconcileReport_BindingRejected(t *testing.T) {
	h, st, id, w, _, _ := bmcReconcileFixture(t)
	st.bindingErr = fmt.Errorf("operation is terminal (failed): %w", store.ErrOperationBinding)
	err := h.HandleObservation(context.Background(), reportObs(t, id, uuid.New(), model.BMCReconcileReport{
		Monitor:        &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
		MonitorRotated: true,
	}))
	if !errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("binding rejection must be terminal, got %v", err)
	}
	if len(w.Secrets) != 0 || len(st.finalized) != 0 {
		t.Errorf("rejected report wrote state: secrets=%+v finalized=%+v", w.Secrets, st.finalized)
	}
}

// TestHandleBMCReconcileReport_MalformedData: an undecodable report never
// becomes decodable; drop it terminally instead of replaying forever.
func TestHandleBMCReconcileReport_MalformedData(t *testing.T) {
	h, _, id, _, _, _ := bmcReconcileFixture(t)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, OperationID: uuid.New(), Event: "bmc_reconcile_report",
		Data: json.RawMessage(`{not json`),
	})
	if !errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("malformed report must be terminal, got %v", err)
	}
}

// unconfirmedReport is a failed rotation's report: a generated password
// whose write may or may not have been applied.
func unconfirmedReport(pw string) model.BMCReconcileReport {
	return model.BMCReconcileReport{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: pw, IPMIUserSlot: 4, PasswordUnconfirmed: true},
		Error:   "bmcreconcile: set password slot 4: exit status 1; the BMC may have applied the generated password all the same",
	}
}

// TestHandleBMCReconcileReport_Unconfirmed_KeptBesideStored: an unconfirmed
// password must not replace the stored one, which is still the BMC's
// password if the write was never applied, and must not be dropped, since
// it is the BMC's password if it was. It is appended to the entry's
// unconfirmed passwords (once, however often the report is replayed), and
// the op fails with the reconcile error.
func TestHandleBMCReconcileReport_Unconfirmed_KeptBesideStored(t *testing.T) {
	h, st, id, w, _, logs := bmcReconcileFixture(t)
	const storedPW, olderPW = "St0red!Monitor", "0lder!Unconf"
	w.Secrets = map[string]map[string]awssecrets.BMCMonitorEntry{"pop1": {"SN-X": {
		Kind: awssecrets.KindBMC, Username: "monitor", Password: storedPW,
		UnconfirmedPasswords: []string{olderPW},
		ExtraConfig:          map[string]any{"ipmi_user_slot": 3},
	}}}
	for range 2 { // the worker replays a report whose ack it lost
		obs := reportObs(t, id, uuid.New(), unconfirmedReport(reconcileMonitorPW))
		if err := h.HandleObservation(context.Background(), obs); err != nil {
			t.Fatalf("HandleObservation: %v", err)
		}
	}
	got := w.Secrets["pop1"]["SN-X"]
	if got.Password != storedPW || got.ExtraConfig["ipmi_user_slot"] != 3 {
		t.Errorf("the stored password and its slot must be kept; kept=%v slot=%v", got.Password == storedPW, got.ExtraConfig["ipmi_user_slot"])
	}
	if !slices.Equal(got.UnconfirmedPasswords, []string{olderPW, reconcileMonitorPW}) {
		t.Errorf("want the unconfirmed password appended once after the older one; have %d", len(got.UnconfirmedPasswords))
	}
	if got.LastKnownHostname != "host1.pop1" {
		t.Errorf("last_known fields must be refreshed: %q", got.LastKnownHostname)
	}
	if len(st.finalized) != 2 || st.finalized[0].Status != model.OpFailed {
		t.Fatalf("expected the op failed, got %+v", st.finalized)
	}
	res := decodeFinalResult(t, st.finalized[0])
	if !strings.Contains(res.Error, "may have applied") || res.BMCReconcile == nil || res.BMCReconcile.MonitorRotated || res.BMCReconcile.MonitorPasswordWritten {
		t.Errorf("result: %s", st.finalized[0].Result)
	}
	assertNoSecret(t, st, logs)
}

// TestHandleBMCReconcileReport_Unconfirmed_NothingStored: with no password
// stored for the device there is nothing to keep, so the unconfirmed one is
// stored as the password; the next op confirms it or writes it.
func TestHandleBMCReconcileReport_Unconfirmed_NothingStored(t *testing.T) {
	h, st, id, w, _, logs := bmcReconcileFixture(t)
	obs := reportObs(t, id, uuid.New(), unconfirmedReport(reconcileMonitorPW))
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	got := w.Secrets["pop1"]["SN-X"]
	if got.Password != reconcileMonitorPW || len(got.UnconfirmedPasswords) != 0 || got.Username != "monitor" {
		t.Errorf("want the unconfirmed password stored as the device's password")
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != model.OpFailed {
		t.Fatalf("expected the op failed, got %+v", st.finalized)
	}
	assertNoSecret(t, st, logs)
}

// TestHandleBMCReconcileReport_Success_DropsUnconfirmed: a compliance op
// that succeeded without reporting a password left the BMC holding the
// stored one, so the entry's unconfirmed passwords are dropped and the
// password kept. One that reports a password (a rotation, or an adopted
// unconfirmed password) replaces the entry, which drops them too.
func TestHandleBMCReconcileReport_Success_DropsUnconfirmed(t *testing.T) {
	const storedPW = "St0red!Monitor"
	for name, tc := range map[string]struct {
		rep    model.BMCReconcileReport
		wantPW string
	}{
		"stored password verified": {
			model.BMCReconcileReport{Monitor: &model.BMCUserCred{Username: "monitor", IPMIUserSlot: 4}}, storedPW,
		},
		"unconfirmed password adopted": {
			model.BMCReconcileReport{
				Monitor:        &model.BMCUserCred{Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 4},
				MonitorRotated: true,
			}, reconcileMonitorPW,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, st, id, w, _, logs := bmcReconcileFixture(t)
			w.Secrets = map[string]map[string]awssecrets.BMCMonitorEntry{"pop1": {"SN-X": {
				Kind: awssecrets.KindBMC, Username: "monitor", Password: storedPW,
				UnconfirmedPasswords: []string{reconcileMonitorPW},
			}}}
			if err := h.HandleObservation(context.Background(), reportObs(t, id, uuid.New(), tc.rep)); err != nil {
				t.Fatalf("HandleObservation: %v", err)
			}
			got := w.Secrets["pop1"]["SN-X"]
			if got.Password != tc.wantPW || len(got.UnconfirmedPasswords) != 0 {
				t.Errorf("want the right password stored and no unconfirmed ones left; right password=%v, unconfirmed left=%d",
					got.Password == tc.wantPW, len(got.UnconfirmedPasswords))
			}
			if len(st.finalized) != 1 || st.finalized[0].Status != model.OpCompleted {
				t.Fatalf("expected the op completed, got %+v", st.finalized)
			}
			assertNoSecret(t, st, logs)
		})
	}
}

// TestHandleBMCReconcileReport_Failure_KeepsUnconfirmed: a failed op that
// reports no password settles nothing, so the unconfirmed passwords stay.
func TestHandleBMCReconcileReport_Failure_KeepsUnconfirmed(t *testing.T) {
	h, _, id, w, _, _ := bmcReconcileFixture(t)
	w.Secrets = map[string]map[string]awssecrets.BMCMonitorEntry{"pop1": {"SN-X": {
		Kind: awssecrets.KindBMC, Username: "monitor", Password: "St0red!Monitor",
		UnconfirmedPasswords: []string{reconcileMonitorPW},
	}}}
	obs := reportObs(t, id, uuid.New(), model.BMCReconcileReport{Error: "bmcreconcile: could not test the password of slot 4"})
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if got := w.Secrets["pop1"]["SN-X"]; len(got.UnconfirmedPasswords) != 1 || got.Password != "St0red!Monitor" {
		t.Errorf("a failed op must leave the entry alone")
	}
}

// TestHandleDeployComplete_UnconfirmedMonitor_KeptBesideStored: the in-band
// reconcile reports through deploy_complete, and an unconfirmed password
// from it is kept beside the stored one the same way.
func TestHandleDeployComplete_UnconfirmedMonitor_KeptBesideStored(t *testing.T) {
	h, _, id, w := deployCompleteFixture(t)
	w.Secrets = map[string]map[string]awssecrets.BMCMonitorEntry{"pop1": {"SN-X": {
		Kind: awssecrets.KindBMC, Username: "monitor", Password: "St0red!Monitor",
	}}}
	data, err := json.Marshal(model.BMCCreds{Monitor: &model.BMCUserCred{
		Username: "monitor", Password: reconcileMonitorPW, IPMIUserSlot: 3, PasswordUnconfirmed: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	obs := model.Observation{MachineID: id, OperationID: uuid.New(), Event: "deploy_complete", Data: data}
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	got := w.Secrets["pop1"]["SN-X"]
	if got.Password != "St0red!Monitor" || !slices.Equal(got.UnconfirmedPasswords, []string{reconcileMonitorPW}) {
		t.Errorf("want the stored password kept and the unconfirmed one beside it")
	}
}
