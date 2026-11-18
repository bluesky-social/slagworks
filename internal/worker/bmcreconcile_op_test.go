package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

const (
	testAdminPW         = "adm1n!pw"
	testMonitorPW       = "M0n!torFresh"
	testStoredMonitorPW = "St0red!monPW"
)

// fakeReconciler records the LAN target and monitor password it was handed
// and returns a canned outcome.
type fakeReconciler struct {
	calls            []bmcreconcile.LANTarget
	monitorPasswords []string
	result           *bmcreconcile.Result
	err              error
}

func (f *fakeReconciler) run(_ context.Context, t bmcreconcile.LANTarget, monitorPassword string) (*bmcreconcile.Result, error) {
	f.calls = append(f.calls, t)
	f.monitorPasswords = append(f.monitorPasswords, monitorPassword)
	return f.result, f.err
}

// fakeTimeReconciler records the time targets it was handed and returns a
// canned outcome.
type fakeTimeReconciler struct {
	calls  []bmcreconcile.TimeTarget
	result bmcreconcile.TimeResult
}

func (f *fakeTimeReconciler) run(_ context.Context, t bmcreconcile.TimeTarget) bmcreconcile.TimeResult {
	f.calls = append(f.calls, t)
	return f.result
}

// testOOB is a site OOB config whose prefix contains the fixtures' BMC IP
// (10.0.0.41) and carries ntp_servers.
func testOOB() model.OOBConfig {
	return model.OOBConfig{Subnet: "10.0.0.0/22", Gateway: "10.0.0.1", NTP: []string{"10.0.0.1"}}
}

// setOOBConfig installs oob as the cache's snapshot OOB config.
func setOOBConfig(c *Cache, oob model.OOBConfig) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	v := *c.view.Load()
	v.oobCfg = oob
	c.view.Store(&v)
}

func monitorResult() *bmcreconcile.Result {
	return &bmcreconcile.Result{
		CreatedOrUpdated: []bmcreconcile.ProvisionedUser{
			{Slot: 4, Name: "monitor", Password: testMonitorPW, Privilege: bmcreconcile.PrivUser},
		},
		LeftoversDisabled: []string{"slagworks"},
	}
}

// bmcReconcileFixture seeds a machine + pending bmc_reconcile op and an
// executor whose controller reports chassisSerial, whose reconciler is rec
// and whose time pass is a compliant no-op fake (bmcTimeFixture exposes
// it). The site's OOB config is testOOB(). params is the op's params blob.
func bmcReconcileFixture(t *testing.T, bmcIP, chassisSerial string, params model.OpParams, rec *fakeReconciler) (*Executor, *Cache, model.Operation, *int) {
	t.Helper()
	exec, cache, op, calls, _ := bmcTimeFixture(t, bmcIP, chassisSerial, params, rec)
	return exec, cache, op, calls
}

// bmcTimeFixture is bmcReconcileFixture that also returns the fake time
// pass, so a test can set its outcome and inspect its targets.
func bmcTimeFixture(t *testing.T, bmcIP, chassisSerial string, params model.OpParams, rec *fakeReconciler) (*Executor, *Cache, model.Operation, *int, *fakeTimeReconciler) {
	t.Helper()
	cache := NewCache(nil)
	setOOBConfig(cache, testOOB())
	machineID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: bmcIP})
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	op := model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpBMCReconcile, Status: model.OpPending, Params: raw}
	cache.opState.mu.Lock()
	cache.opState.ops[op.ID.String()] = &op
	cache.opState.mu.Unlock()
	factoryCalls := new(int)
	timeRec := &fakeTimeReconciler{result: bmcreconcile.TimeResult{NTPServers: []string{"10.0.0.1"}}}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			*factoryCalls++
			return &mockPowerController{chassisSerial: chassisSerial}, nil
		},
		bmcReconcile: rec.run,
		bmcTime:      timeRec.run,
		log:          testLogger(),
	}
	return exec, cache, op, factoryCalls, timeRec
}

func adminParams() model.OpParams {
	return model.OpParams{BMCRuntime: &model.BMCRuntime{
		ExpectedChassisSerial: "SN1", Username: "ADMIN", Password: testAdminPW,
		ExtraConfig: map[string]any{"ipmi_cipher": float64(3)},
	}}
}

// assertNoMonitorPWInOpUpdates: op_updates become the operations-table
// result and op-log lines; the monitor password must never ride one.
func assertNoMonitorPWInOpUpdates(t *testing.T, updates []model.OperationUpdate) {
	t.Helper()
	for _, u := range updates {
		raw, _ := json.Marshal(u)
		if strings.Contains(string(raw), testMonitorPW) {
			t.Errorf("op_update carries the monitor password: %s", raw)
		}
	}
}

// TestExecutor_BMCReconcile_Success: after the chassis-serial gate passes,
// the executor runs reconcile over the LAN with the op's admin cred
// against the snapshot BMC IP (port stripped). With no stored monitor
// password on the op (a new BMC) reconcile generates one, and the outcome —
// including the fresh monitor password, flagged rotated — goes ONLY through
// a bmc_reconcile_report observation. Global finalizes the op from it, so
// the worker sends no terminal op_update.
func TestExecutor_BMCReconcile_Success(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41:443", "SN1", adminParams(), rec)

	exec.runBMCReconcile(context.Background(), op)

	if len(rec.calls) != 1 {
		t.Fatalf("expected one reconcile, got %d", len(rec.calls))
	}
	want := bmcreconcile.LANTarget{Host: "10.0.0.41", Username: "ADMIN", Password: testAdminPW, Cipher: 3}
	if rec.calls[0] != want {
		t.Errorf("LAN target = %+v, want %+v", rec.calls[0], want)
	}
	if rec.monitorPasswords[0] != "" {
		t.Errorf("no stored password on the op, but reconcile was handed %q; want \"\" (generate)", rec.monitorPasswords[0])
	}

	obs, updates := cache.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "bmc_reconcile_report" || obs[0].Obs.OperationID != op.ID || obs[0].Obs.MachineID != op.MachineID {
		t.Fatalf("expected one bmc_reconcile_report observation for the op, got %+v", obs)
	}
	var rep model.BMCReconcileReport
	if err := json.Unmarshal(obs[0].Obs.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Monitor == nil || rep.Monitor.Password != testMonitorPW || rep.Monitor.IPMIUserSlot != 4 ||
		!rep.MonitorRotated || !rep.MonitorPasswordWritten || rep.Error != "" {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.LeftoversDisabled) != 1 || rep.LeftoversDisabled[0] != "slagworks" {
		t.Errorf("leftovers = %v", rep.LeftoversDisabled)
	}
	for _, u := range updates {
		if u.Status != "" {
			t.Errorf("worker must not finalize the op itself (global does, from the report): %+v", u)
		}
	}
	assertNoMonitorPWInOpUpdates(t, updates)
}

// storedMonitorParams is adminParams plus the stored monitor password global
// resolved from pop/<pop>/bmc-creds-monitor.
func storedMonitorParams() model.OpParams {
	p := adminParams()
	p.MonitorRuntime = &model.MonitorRuntime{Password: testStoredMonitorPW}
	return p
}

// reassertedResult is what reconcile returns after writing the caller's
// stored password: the monitor account, with no password (nothing was
// generated).
func reassertedResult() *bmcreconcile.Result {
	return &bmcreconcile.Result{
		CreatedOrUpdated: []bmcreconcile.ProvisionedUser{
			{Slot: 4, Name: "monitor", Privilege: bmcreconcile.PrivUser},
		},
	}
}

// verifiedResult is what reconcile returns when the monitor already held
// the caller's stored password: the account verified, nothing written.
func verifiedResult() *bmcreconcile.Result {
	return &bmcreconcile.Result{
		PasswordVerified: []bmcreconcile.ProvisionedUser{
			{Slot: 4, Name: "monitor", Privilege: bmcreconcile.PrivUser},
		},
	}
}

// TestExecutor_BMCReconcile_StoredPassword_Reasserted: with the stored
// monitor password on the op, reconcile is handed exactly that password to
// re-assert. Whether the BMC already held it (verified, no write) or
// reconcile wrote it, the report names the monitor account, says it was
// NOT rotated, says whether the password was written, and carries no
// password: global already holds it and must not write a new secret
// version. The stored password appears in no observation, no op_update and
// no log line.
func TestExecutor_BMCReconcile_StoredPassword_Reasserted(t *testing.T) {
	cases := []struct {
		name        string
		result      *bmcreconcile.Result
		wantWritten bool
	}{
		{"already held: verified, not written", verifiedResult(), false},
		{"drifted: written", reassertedResult(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testStoredPasswordReasserted(t, tc.result, tc.wantWritten)
		})
	}
}

func testStoredPasswordReasserted(t *testing.T, result *bmcreconcile.Result, wantWritten bool) {
	t.Helper()
	rec := &fakeReconciler{result: result}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "SN1", storedMonitorParams(), rec)
	logs := &bytes.Buffer{}
	exec.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	exec.runBMCReconcile(context.Background(), op)

	if len(rec.monitorPasswords) != 1 || rec.monitorPasswords[0] != testStoredMonitorPW {
		t.Fatalf("reconcile must be handed the stored monitor password, got %q", rec.monitorPasswords)
	}
	obs, updates := cache.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("expected the report, got %+v", obs)
	}
	var rep model.BMCReconcileReport
	if err := json.Unmarshal(obs[0].Obs.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Monitor == nil || rep.Monitor.IPMIUserSlot != 4 || rep.Monitor.Password != "" || rep.MonitorRotated ||
		rep.MonitorPasswordWritten != wantWritten || rep.Error != "" {
		t.Errorf("report = %+v, want monitor at slot 4, not rotated, written=%v, no password", rep, wantWritten)
	}
	wantKeys := []string{`"monitor_rotated":false`, fmt.Sprintf(`"monitor_password_written":%v`, wantWritten)}
	for _, key := range wantKeys {
		if !strings.Contains(string(obs[0].Obs.Data), key) {
			t.Errorf("report must say %s explicitly: %s", key, obs[0].Obs.Data)
		}
	}
	for _, o := range obs {
		raw, _ := json.Marshal(o)
		if strings.Contains(string(raw), testStoredMonitorPW) {
			t.Errorf("observation carries the stored monitor password: %s", raw)
		}
	}
	for _, u := range updates {
		raw, _ := json.Marshal(u)
		if strings.Contains(string(raw), testStoredMonitorPW) {
			t.Errorf("op_update carries the stored monitor password: %s", raw)
		}
	}
	if strings.Contains(logs.String(), testStoredMonitorPW) {
		t.Errorf("the stored monitor password reached the logs:\n%s", logs.String())
	}
	for _, key := range wantKeys {
		if !strings.Contains(logs.String(), key) {
			t.Errorf("the done log line should say %s:\n%s", key, logs.String())
		}
	}
}

// TestExecutor_BMCReconcile_RotateMonitor: an op with rotate_monitor (and so
// no stored password) hands reconcile no password, so it generates one, and
// the report carries the fresh password flagged rotated.
func TestExecutor_BMCReconcile_RotateMonitor(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	params := adminParams()
	params.RotateMonitor = true
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "SN1", params, rec)

	exec.runBMCReconcile(context.Background(), op)

	if len(rec.monitorPasswords) != 1 || rec.monitorPasswords[0] != "" {
		t.Fatalf("a rotation must hand reconcile no password, got %q", rec.monitorPasswords)
	}
	rep, _ := drainReport(t, cache)
	if rep.Monitor == nil || rep.Monitor.Password != testMonitorPW || !rep.MonitorRotated || !rep.MonitorPasswordWritten {
		t.Errorf("report = %+v, want the generated password, rotated and written", rep)
	}
}

// TestExecutor_BMCReconcile_SerialMismatch: the identity gate runs before
// any write, exactly as for power ops — a wrong box fails the op and
// reconcile never runs.
func TestExecutor_BMCReconcile_SerialMismatch(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "OTHER-BOX", adminParams(), rec)

	exec.runBMCReconcile(context.Background(), op)

	if len(rec.calls) != 0 {
		t.Fatal("reconcile ran despite a chassis-serial mismatch")
	}
	obs, updates := cache.DrainOutbox()
	if len(obs) != 0 {
		t.Errorf("no report expected, got %+v", obs)
	}
	if !hasFailedUpdate(updates, "serial mismatch") {
		t.Errorf("expected a failed op_update naming the serial mismatch, got %+v", updates)
	}
}

// TestExecutor_BMCReconcile_FailsBeforeWrite: a nil Result means nothing
// was written (list failed, the slot-2 gate refused, ...): there is no
// secret to deliver, so the op fails through the ordinary op_update.
func TestExecutor_BMCReconcile_FailsBeforeWrite(t *testing.T) {
	rec := &fakeReconciler{err: &bmcreconcile.AdminNotSlot2Error{User: "root", Slot: 3, Slot2User: "ADMIN"}}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "SN1", adminParams(), rec)

	exec.runBMCReconcile(context.Background(), op)

	obs, updates := cache.DrainOutbox()
	if len(obs) != 0 {
		t.Errorf("no report expected when nothing was written, got %+v", obs)
	}
	if !hasFailedUpdate(updates, "not the reserved slot 2") {
		t.Errorf("expected a failed op_update carrying the refusal, got %+v", updates)
	}
}

// TestExecutor_BMCReconcile_PartialFailure: reconcile wrote the monitor
// password and then failed. The password must still reach global, so the
// failure travels in the report (Error set), not as an op_update.
func TestExecutor_BMCReconcile_PartialFailure(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult(), err: errors.New("bmcreconcile: disable leftover slot 5 (maas): exit status 1")}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "SN1", adminParams(), rec)

	exec.runBMCReconcile(context.Background(), op)

	obs, updates := cache.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("expected the report, got %+v", obs)
	}
	var rep model.BMCReconcileReport
	if err := json.Unmarshal(obs[0].Obs.Data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Monitor == nil || rep.Monitor.Password != testMonitorPW || !strings.Contains(rep.Error, "leftover slot 5") {
		t.Errorf("partial report = %+v", rep)
	}
	for _, u := range updates {
		if u.Status != "" {
			t.Errorf("partial failure must not also send a terminal op_update: %+v", u)
		}
	}
	assertNoMonitorPWInOpUpdates(t, updates)
}

// TestExecutor_BMCReconcile_RefusesWithoutAdminRuntime: a reconcile op must
// carry the admin cred (bmc_runtime), no PDU/AMT runtime, and a coherent
// monitor instruction: a stored password must not be empty (that would
// silently rotate), and an op cannot both rotate and carry the stored
// password. Anything else fails before the controller is even built.
func TestExecutor_BMCReconcile_RefusesWithoutAdminRuntime(t *testing.T) {
	rotateAndStored := storedMonitorParams()
	rotateAndStored.RotateMonitor = true
	cases := map[string]model.OpParams{
		"no bmc_runtime":   {},
		"empty admin cred": {BMCRuntime: &model.BMCRuntime{ExpectedChassisSerial: "SN1"}},
		"pdu runtime": {
			BMCRuntime: adminParams().BMCRuntime,
			PDURuntime: &model.PDURuntime{Outlets: []model.PDUOutlet{{PDUHost: "10.0.0.9", Outlet: 1}}},
		},
		"empty stored monitor password": {
			BMCRuntime:     adminParams().BMCRuntime,
			MonitorRuntime: &model.MonitorRuntime{},
		},
		"rotate with a stored monitor password": rotateAndStored,
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &fakeReconciler{result: monitorResult()}
			exec, cache, op, factoryCalls := bmcReconcileFixture(t, "10.0.0.41", "SN1", params, rec)
			exec.runBMCReconcile(context.Background(), op)
			if len(rec.calls) != 0 || *factoryCalls != 0 {
				t.Fatalf("refused op still reached the BMC: reconciles=%d factory=%d", len(rec.calls), *factoryCalls)
			}
			_, updates := cache.DrainOutbox()
			if !hasFailedUpdate(updates, "") {
				t.Errorf("expected a failed op_update, got %+v", updates)
			}
		})
	}
}

// TestExecutor_BMCReconcile_RoutedFromExecuteOp: executeOp dispatches a
// claimed bmc_reconcile op to runBMCReconcile (not the power path).
func TestExecutor_BMCReconcile_RoutedFromExecuteOp(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	exec, cache, op, _ := bmcReconcileFixture(t, "10.0.0.41", "SN1", adminParams(), rec)
	exec.claimer = &fakeClaimer{resultOp: &op}

	exec.executeOp(context.Background(), op)

	if len(rec.calls) != 1 {
		t.Fatalf("executeOp did not run the reconcile (calls=%d)", len(rec.calls))
	}
	if obs, _ := cache.DrainOutbox(); len(obs) != 1 || obs[0].Obs.Event != "bmc_reconcile_report" {
		t.Errorf("expected the report observation, got %+v", obs)
	}
}

// drainReport returns the single bmc_reconcile_report the op queued and the
// op_updates beside it.
func drainReport(t *testing.T, cache *Cache) (model.BMCReconcileReport, []model.OperationUpdate) {
	t.Helper()
	obs, updates := cache.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "bmc_reconcile_report" {
		t.Fatalf("expected one bmc_reconcile_report observation, got %+v", obs)
	}
	var rep model.BMCReconcileReport
	if err := json.Unmarshal(obs[0].Obs.Data, &rep); err != nil {
		t.Fatal(err)
	}
	return rep, updates
}

// TestExecutor_BMCReconcile_TimePass: after the account reconcile the
// executor runs the time pass against the BMC's Redfish address (port kept:
// it is the qemutest fake's Redfish port), over the same admin LAN session,
// with the ntp_servers of the OOB prefix containing the BMC IP, and reports
// the outcome in the bmc_reconcile_report.
func TestExecutor_BMCReconcile_TimePass(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	exec, cache, op, _, timeRec := bmcTimeFixture(t, "10.0.0.41:443", "SN1", adminParams(), rec)
	timeRec.result = bmcreconcile.TimeResult{
		NTPServers: []string{"10.0.0.1"}, NTPConfigured: true, TimezoneSet: true, ClockStepped: true,
		ClockSkew: -90*time.Minute - 400*time.Millisecond,
	}

	exec.runBMCReconcile(context.Background(), op)

	if len(timeRec.calls) != 1 {
		t.Fatalf("time pass ran %d times, want 1", len(timeRec.calls))
	}
	want := bmcreconcile.TimeTarget{
		RedfishAddr: "10.0.0.41:443",
		LAN:         bmcreconcile.LANTarget{Host: "10.0.0.41", Username: "ADMIN", Password: testAdminPW, Cipher: 3},
		NTPServers:  []string{"10.0.0.1"},
	}
	if !reflect.DeepEqual(timeRec.calls[0], want) {
		t.Errorf("time target = %+v, want %+v", timeRec.calls[0], want)
	}
	rep, _ := drainReport(t, cache)
	wantTime := model.BMCTimeCompliance{
		NTPServers: []string{"10.0.0.1"}, NTPConfigured: true, TimezoneSet: true, ClockStepped: true,
		ClockSkewSeconds: -5400,
	}
	if !reflect.DeepEqual(rep.BMCTimeCompliance, wantTime) {
		t.Errorf("report time = %+v, want %+v", rep.BMCTimeCompliance, wantTime)
	}
	if rep.Monitor == nil || rep.Error != "" {
		t.Errorf("account part of the report = %+v", rep)
	}
}

// TestExecutor_BMCReconcile_TimeFailureLeavesAccountsAlone: a time-pass
// failure lands in TimeError only; the account outcome (monitor cred, no
// error) is reported unchanged and the op is not failed by the worker.
func TestExecutor_BMCReconcile_TimeFailureLeavesAccountsAlone(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult()}
	exec, cache, op, _, timeRec := bmcTimeFixture(t, "10.0.0.41", "SN1", adminParams(), rec)
	timeRec.result = bmcreconcile.TimeResult{NTPServers: []string{"10.0.0.1"}, Err: errors.New("bmc time compliance incomplete: configure NTP: HTTP 400")}

	exec.runBMCReconcile(context.Background(), op)

	rep, updates := drainReport(t, cache)
	if rep.Error != "" || rep.Monitor == nil || rep.Monitor.Password != testMonitorPW {
		t.Errorf("account outcome changed by a time failure: %+v", rep)
	}
	if !strings.Contains(rep.TimeError, "configure NTP: HTTP 400") {
		t.Errorf("TimeError = %q", rep.TimeError)
	}
	for _, u := range updates {
		if u.Status != "" {
			t.Errorf("time failure must not finalize the op from the worker: %+v", u)
		}
	}
}

// TestExecutor_BMCReconcile_TimeSkippedWithoutNTPServer: with no ntp_servers
// for the BMC (no OOB prefix, the BMC outside it, or the prefix lacking the
// custom field) the time pass does not run and the report says why; the
// account reconcile still ran and reported.
func TestExecutor_BMCReconcile_TimeSkippedWithoutNTPServer(t *testing.T) {
	cases := map[string]struct {
		oob  model.OOBConfig
		want string
	}{
		"no OOB prefix":      {model.OOBConfig{}, "no slagworks-oob prefix"},
		"BMC outside prefix": {model.OOBConfig{Subnet: "10.9.0.0/22", NTP: []string{"10.9.0.1"}}, "outside the site's slagworks-oob prefix 10.9.0.0/22"},
		"prefix without NTP": {model.OOBConfig{Subnet: "10.0.0.0/22"}, "no ntp_servers"},
		"unparseable subnet": {model.OOBConfig{Subnet: "10.0.0.0", NTP: []string{"10.0.0.1"}}, "not a CIDR"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &fakeReconciler{result: monitorResult()}
			exec, cache, op, _, timeRec := bmcTimeFixture(t, "10.0.0.41", "SN1", adminParams(), rec)
			setOOBConfig(cache, tc.oob)

			exec.runBMCReconcile(context.Background(), op)

			if len(timeRec.calls) != 0 {
				t.Errorf("time pass ran without an NTP server: %+v", timeRec.calls)
			}
			if len(rec.calls) != 1 {
				t.Errorf("account reconcile ran %d times, want 1", len(rec.calls))
			}
			rep, _ := drainReport(t, cache)
			if !strings.Contains(rep.TimeError, tc.want) || rep.Monitor == nil || len(rep.NTPServers) != 0 {
				t.Errorf("report = %+v, want TimeError containing %q", rep, tc.want)
			}
		})
	}
}

// TestExecutor_BMCReconcile_TimeRunsAfterPartialAccountFailure: the passes
// are independent; an account reconcile that failed after writing still
// gets the time pass, and both outcomes ride the report.
func TestExecutor_BMCReconcile_TimeRunsAfterPartialAccountFailure(t *testing.T) {
	rec := &fakeReconciler{result: monitorResult(), err: errors.New("bmcreconcile: disable leftover slot 5 (maas): exit status 1")}
	exec, cache, op, _, timeRec := bmcTimeFixture(t, "10.0.0.41", "SN1", adminParams(), rec)
	timeRec.result = bmcreconcile.TimeResult{NTPServers: []string{"10.0.0.1"}, TimezoneSet: true}

	exec.runBMCReconcile(context.Background(), op)

	if len(timeRec.calls) != 1 {
		t.Fatalf("time pass ran %d times, want 1", len(timeRec.calls))
	}
	rep, _ := drainReport(t, cache)
	if !strings.Contains(rep.Error, "leftover slot 5") || !rep.TimezoneSet {
		t.Errorf("report = %+v, want the account error and the time outcome", rep)
	}
}

// TestExecutor_BMCReconcile_NoTimePassWhenNothingWritten: a reconcile that
// refused before any write fails the op through op_update, with no report
// to carry a time outcome, so the time pass does not run.
func TestExecutor_BMCReconcile_NoTimePassWhenNothingWritten(t *testing.T) {
	rec := &fakeReconciler{err: &bmcreconcile.AdminNotSlot2Error{User: "root", Slot: 3, Slot2User: "ADMIN"}}
	exec, _, op, _, timeRec := bmcTimeFixture(t, "10.0.0.41", "SN1", adminParams(), rec)

	exec.runBMCReconcile(context.Background(), op)

	if len(timeRec.calls) != 0 {
		t.Errorf("time pass ran after a refused reconcile: %+v", timeRec.calls)
	}
}

func hasFailedUpdate(updates []model.OperationUpdate, substr string) bool {
	for _, u := range updates {
		if u.Status == string(model.OpFailed) && strings.Contains(string(u.Result), substr) {
			return true
		}
	}
	return false
}
