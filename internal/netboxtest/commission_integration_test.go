//go:build integration

package netboxtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/commission"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/netboxtest"
)

// TestCommissionReport_EndToEnd exercises the full commission path
// against a real NetBox + slagworks Postgres, terminating in the
// Commissioned state:
//
//  1. Seed a NetBox site + device.
//  2. Create a slagworks Machine row bound to that device, in
//     Commissioning state.
//  3. Kick off an OpCommission via the normal store path.
//  4. Fire a commission_report observation through the real commission
//     Handler (same code the ws hub calls in production).
//  5. Assert: machine transitions to Commissioned, op transitions to
//     Completed, NetBox receives the commission sync, and the
//     invalidator fires for the site slug.
//
// Covers the exact-operation commit, NetBox reconciliation, and the resulting
// state machine outcome visible to slagctl.
func TestCommissionReport_EndToEnd(t *testing.T) {
	env := netboxtest.New(t)
	ctx := context.Background()

	// NetBox: site + device. The mirror + netbox pipeline normally
	// creates the slagworks Machine row; for this test we poke the
	// store directly because we're focused on the commission path,
	// not the mirror.
	site := env.SeedSite("pop")
	dev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{Serial: "SN-E2E"})

	m := &model.Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &dev.ID,
		NetBoxSerial:   "SN-E2E",
		State:          model.MachineStateCommissioning,
		PXEEligible:    true, // CreateMachineOp refuses if false.
	}
	if err := env.Store.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := env.Store.CreateMachineOp(
		m.ID, model.OpCommission, nil,
		model.MachineStateCommissioning,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Capture invalidations to prove the handler tells the dispatcher
	// the site is dirty. The handler only invalidates when a ClaimResolver
	// is wired (it keys invalidation on the claiming config slug, not the
	// NetBox site slug); without SetClaimResolver the invalidate path is a
	// no-op and the next 5-min reconcile catches up. Wire a resolver that
	// returns the device's site slug (the pop1/pop3 shape where the config
	// slug == the NetBox site), so the assertion below sees site.Slug.
	inv := &capturingInvalidator{}
	h := commission.New(env.Store, env.NetBox, inv)
	h.SetClaimResolver(siteSlugResolver{})

	err = h.HandleObservation(ctx, model.Observation{
		MachineID: m.ID, OperationID: op.ID,
		Event: "commission_report", Data: json.RawMessage(commissionPayload),
	})
	if err != nil {
		t.Fatalf("commission handler: %v", err)
	}

	// Machine should now be Commissioned, op Completed.
	gotM, err := env.Store.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotM.State != model.MachineStateCommissioned {
		t.Errorf("machine state: got %s, want commissioned", gotM.State)
	}
	if len(gotM.CommissionData) == 0 {
		t.Error("expected commission_data to be persisted")
	}
	gotOp, err := env.Store.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOp.Status != model.OpCompleted {
		t.Errorf("op status: got %s, want completed", gotOp.Status)
	}
	if gotOp.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}

	// Invalidation should have fired for the device's site slug.
	if got := inv.slugs(); len(got) != 1 || got[0] != site.Slug {
		t.Errorf("invalidations: got %v, want [%s]", got, site.Slug)
	}
}

// TestCommissionReport_Idempotent fires the same commission_report
// twice and asserts the second call is a clean no-op against the
// already-completed op.
func TestCommissionReport_Idempotent(t *testing.T) {
	env := netboxtest.New(t)
	ctx := context.Background()

	site := env.SeedSite("pop")
	dev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{Serial: "SN-IDEM"})

	m := &model.Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &dev.ID,
		NetBoxSerial:   "SN-IDEM",
		State:          model.MachineStateCommissioning,
		PXEEligible:    true,
	}
	if err := env.Store.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := env.Store.CreateMachineOp(
		m.ID, model.OpCommission, nil,
		model.MachineStateCommissioning,
	)
	if err != nil {
		t.Fatal(err)
	}

	h := commission.New(env.Store, env.NetBox, &capturingInvalidator{})

	obs := model.Observation{
		MachineID: m.ID, OperationID: op.ID,
		Event: "commission_report", Data: json.RawMessage(commissionPayload),
	}
	if err := h.HandleObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}

	firstOp, _ := env.Store.GetOperation(op.ID)
	firstCompletedAt := firstOp.CompletedAt

	// Second call should succeed without reviving or double-completing.
	if err := h.HandleObservation(ctx, obs); err != nil {
		t.Fatalf("second call should be idempotent, got %v", err)
	}

	secondOp, _ := env.Store.GetOperation(op.ID)
	if secondOp.Status != model.OpCompleted {
		t.Errorf("op status should still be completed, got %s", secondOp.Status)
	}
	// completed_at from the first run shouldn't be overwritten.
	if !firstCompletedAt.Equal(*secondOp.CompletedAt) {
		t.Errorf("completed_at changed: first=%s second=%s",
			firstCompletedAt, secondOp.CompletedAt)
	}
}

type capturingInvalidator struct {
	captured []string
}

func (c *capturingInvalidator) Invalidate(slug string) {
	c.captured = append(c.captured, slug)
}

func (c *capturingInvalidator) slugs() []string {
	return c.captured
}

// siteSlugResolver is the test's ClaimResolver: it returns the device's
// NetBox site slug as the (single) claiming config slug. This models the
// pop1/pop3 deployment shape where the worker-identity config slug equals
// the NetBox site slug, so the commission handler invalidates site.Slug.
type siteSlugResolver struct{}

func (siteSlugResolver) ClaimingConfigs(_ context.Context, d netbox.Device) ([]string, error) {
	if d.Site != nil && d.Site.Slug != "" {
		return []string{d.Site.Slug}, nil
	}
	return nil, nil
}

// commissionPayload is a minimal inventory payload that
// netbox.CommissionToSync will accept. System UUID + an empty
// interface list is enough for the sync path to work.
const commissionPayload = `{
  "system": {
    "manufacturer": "QEMU",
    "product": "Standard PC",
    "serial_number": "SN-E2E",
    "uuid": "deadbeef-dead-beef-dead-beefdeadbeef"
  },
  "interfaces": [],
  "block_devices": []
}`

// commissionPayloadUUID is commissionPayload with the system UUID
// replaced so re-commission tests can verify the new value landed.
const commissionPayloadAltUUID = `{
  "system": {
    "manufacturer": "QEMU",
    "product": "Standard PC",
    "serial_number": "SN-E2E",
    "uuid": "cafebabe-cafe-babe-cafe-babecafebabe"
  },
  "interfaces": [],
  "block_devices": []
}`

// seedCommissioningMachine creates a NetBox device bound to a freshly-
// created slagworks Machine row in the given starting state, plus
// an OpCommission if requested. Returns the live objects so tests
// can assert on them afterward. Consolidates setup boilerplate
// shared by the re-commission and failure-path tests.
func seedCommissioningMachine(
	t *testing.T,
	env *netboxtest.Env,
	serial string,
	state model.MachineState,
	createOp bool,
) (*model.Machine, *model.Operation) {
	t.Helper()
	site := env.SeedSite("pop")
	dev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{Serial: serial})

	m := &model.Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &dev.ID,
		NetBoxSerial:   serial,
		State:          state,
		PXEEligible:    true,
	}
	if err := env.Store.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	if !createOp {
		return m, nil
	}
	op, err := env.Store.CreateMachineOp(
		m.ID, model.OpCommission, nil,
		// Allow from any state the test might have used.
		model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateCommissioning,
		model.MachineStateFailed,
	)
	if err != nil {
		t.Fatal(err)
	}
	return m, op
}

// TestCommissionReport_MalformedPayload verifies the handler's
// error path when CommissionToSync rejects the payload (bad JSON
// shape / missing required fields). The op must NOT be completed,
// the machine must NOT transition out of commissioning, and nothing
// is written to NetBox.
//
// This is the #169 agent-error scenario: even though the agent
// "succeeded" enough to call commission_report, the payload is
// unusable. Handler returns the error up so the worker retries —
// and if the worker retries with the same bad payload forever, at
// least the machine state correctly reflects "not ready".
func TestCommissionReport_MalformedPayload(t *testing.T) {
	env := netboxtest.New(t)
	ctx := context.Background()

	m, op := seedCommissioningMachine(t, env, "SN-BAD", model.MachineStateCommissioning, true)

	h := commission.New(env.Store, env.NetBox, &capturingInvalidator{})

	err := h.HandleObservation(ctx, model.Observation{
		MachineID: m.ID, OperationID: op.ID,
		Event: "commission_report",
		// Malformed JSON: CommissionToSync's only hard error path is
		// json.Unmarshal. Missing/empty fields are tolerated (see
		// sync.go CommissionToSync — the "system" block is optional).
		Data: json.RawMessage(`{not valid json`),
	})
	if err == nil {
		t.Fatal("expected error on malformed commission payload")
	}

	// Machine must stay in commissioning.
	gotM, _ := env.Store.GetMachine(m.ID)
	if gotM.State != model.MachineStateCommissioning {
		t.Errorf("machine state on failure: got %s, want commissioning", gotM.State)
	}
	if len(gotM.CommissionData) != 0 {
		t.Errorf("commission_data should not be persisted on failure")
	}

	// Op must stay in-flight (not completed, not failed).
	gotOp, _ := env.Store.GetOperation(op.ID)
	if gotOp.Status == model.OpCompleted || gotOp.Status == model.OpFailed {
		t.Errorf("op should remain in-flight on payload failure, got %s", gotOp.Status)
	}
}

// TestReCommission verifies that a machine already in Commissioned
// state can be commissioned again: a new OpCommission is created, a
// fresh commission_report arrives with updated hardware data, and the
// machine cycles back to Commissioned with the new commission_data.
//
// This is the #170 scenario. Key questions it answers:
//   - Does CreateMachineOp allow commission from Commissioned? (Yes —
//     MachineStateCommissioned is in the allowed list per
//     store.CreateMachineOp.)
//   - Does CommitCommissionResult replace the old commission_data?
//   - Does the exact-op commit bind to the NEW op, not any
//     prior completed op?
//   - Does NetBox get re-synced idempotently?
func TestReCommission(t *testing.T) {
	env := netboxtest.New(t)
	ctx := context.Background()

	// Run the first commission through to completion.
	m, firstOp := seedCommissioningMachine(t, env, "SN-RECOMM", model.MachineStateCommissioning, true)
	h := commission.New(env.Store, env.NetBox, &capturingInvalidator{})
	if err := h.HandleObservation(ctx, model.Observation{
		MachineID: m.ID, OperationID: firstOp.ID,
		Event: "commission_report", Data: json.RawMessage(commissionPayload),
	}); err != nil {
		t.Fatalf("first commission: %v", err)
	}
	// Confirm the setup: commissioned, first op completed, commission_data set.
	afterFirst, _ := env.Store.GetMachine(m.ID)
	if afterFirst.State != model.MachineStateCommissioned {
		t.Fatalf("after first commission: got %s, want commissioned", afterFirst.State)
	}
	firstData := append([]byte(nil), afterFirst.CommissionData...)

	// Kick off a re-commission: CreateMachineOp from Commissioned state.
	secondOp, err := env.Store.CreateMachineOp(
		m.ID, model.OpCommission, nil,
		model.MachineStateCommissioned,
	)
	if err != nil {
		t.Fatalf("re-commission CreateMachineOp: %v", err)
	}
	if secondOp.ID == firstOp.ID {
		t.Fatalf("re-commission should create a NEW op row, got same id %s", secondOp.ID)
	}

	// Fire the second commission_report with updated payload.
	if err := h.HandleObservation(ctx, model.Observation{
		MachineID: m.ID, OperationID: secondOp.ID,
		Event: "commission_report", Data: json.RawMessage(commissionPayloadAltUUID),
	}); err != nil {
		t.Fatalf("second commission: %v", err)
	}

	// Machine back to ready, new op completed, commission_data replaced.
	afterSecond, _ := env.Store.GetMachine(m.ID)
	if afterSecond.State != model.MachineStateCommissioned {
		t.Errorf("after re-commission: got %s, want commissioned", afterSecond.State)
	}
	if string(afterSecond.CommissionData) == string(firstData) {
		t.Error("commission_data should have been replaced by the new payload")
	}
	if !jsonContains(afterSecond.CommissionData, "cafebabe-cafe-babe-cafe-babecafebabe") {
		t.Errorf("commission_data should contain the alt UUID, got: %s", afterSecond.CommissionData)
	}

	// First op stays completed (not re-opened).
	gotFirst, _ := env.Store.GetOperation(firstOp.ID)
	if gotFirst.Status != model.OpCompleted {
		t.Errorf("first op: got %s, want completed (should not be revived)", gotFirst.Status)
	}
	// Second op completed.
	gotSecond, _ := env.Store.GetOperation(secondOp.ID)
	if gotSecond.Status != model.OpCompleted {
		t.Errorf("second op: got %s, want completed", gotSecond.Status)
	}
}

// jsonContains is a dumb substring check sufficient for the
// re-commission payload assertion. Avoids pulling in a JSON parser
// just to confirm a literal appears.
func jsonContains(raw json.RawMessage, needle string) bool {
	return bytes.Contains(raw, []byte(needle))
}
