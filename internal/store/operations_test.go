package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestCompleteActiveOperation_TransitionsMachineState(t *testing.T) {
	st := testStore(t)

	m := &model.Machine{
		ID:           uuid.New(),
		NetBoxSerial: "SN-1",
		State:        model.MachineStateCommissioning,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		MachineID: m.ID,
		Type:      model.OpCommission,
		Status:    model.OpRunning,
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CompleteActiveOperation(m.ID, model.OpCommission,
		model.MachineStateCommissioned, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.OpCompleted {
		t.Errorf("op status: got %s, want completed", got.Status)
	}
	gotM, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotM.State != model.MachineStateCommissioned {
		t.Errorf("machine state: got %s, want ready", gotM.State)
	}
}

func TestCompleteActiveOperation_IdempotentWhenNoneInFlight(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-2", State: model.MachineStateCommissioned,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// No op for this machine. Should no-op cleanly.
	if _, err := st.CompleteActiveOperation(m.ID, model.OpCommission,
		model.MachineStateCommissioned, nil); err != nil {
		t.Fatalf("expected idempotent no-op, got %v", err)
	}
}

func TestCompleteActiveOperation_IgnoresCompletedOps(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-4", State: model.MachineStateCommissioned,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// An already-completed commission op from a prior retry.
	done := &model.Operation{MachineID: m.ID, Type: model.OpCommission, Status: model.OpCompleted}
	if err := st.CreateOperation(done); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CompleteActiveOperation(m.ID, model.OpCommission,
		model.MachineStateCommissioned, []byte(`{"retry":true}`)); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
	// Completed op result should not be overwritten.
	got, _ := st.GetOperation(done.ID)
	if len(got.Result) != 0 {
		t.Errorf("completed op result should be untouched, got %s", string(got.Result))
	}
}

func TestCompleteOperationWithState_WontReviveFailedOp(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-revive", State: model.MachineStateFailed,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		MachineID: m.ID,
		Type:      model.OpDeploy,
		Status:    model.OpFailed,
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	// Late-arriving op_update with completed status. MUST NOT flip
	// the failed op to completed, and MUST NOT transition the machine
	// away from failed.
	if _, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpFailed {
		t.Errorf("op status: got %s, want failed (should not revive)", gotOp.Status)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateFailed {
		t.Errorf("machine state: got %s, want failed", gotM.State)
	}
	// Now that completion drives the auto-lock, a wrongly-revived op
	// would also wrongly lock the machine. Assert the lock is untouched —
	// this is the stake the guarded UPDATE protects.
	if gotM.Locked {
		t.Error("a non-revived (already-failed) op must not lock the machine")
	}
}

func TestCompleteOperationWithState_EmptyTargetLeavesMachineState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-power", State: model.MachineStateCommissioned,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		MachineID: m.ID,
		Type:      model.OpPowerCycle,
		Status:    model.OpRunning,
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CompleteOperationWithState(op.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpCompleted {
		t.Errorf("op status: got %s, want completed", gotOp.Status)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateCommissioned {
		t.Errorf("machine state should be untouched: got %s, want ready", gotM.State)
	}
}

// TestCreatePowerOp_RejectedWhenLifecycleOpActive pins the current
// behaviour of the unique index `idx_one_active_op_per_machine`: only
// one active op per machine, period — there is no power/lifecycle
// exemption. The redundant auto-cycle that the deploy/commission/
// recovery API handlers used to fire after creating a lifecycle op
// always violated this constraint and was logged as a benign WARN;
// the deploy proceeded fine because the executor power-cycles the
// host inside runLifecycleOp anyway.
//
// This test ensures that if anyone re-introduces the auto-cycle, the
// build catches it. Future refactors that legitimately want power +
// lifecycle to coexist need to revisit the index too.
func TestCreatePowerOp_RejectedWhenLifecycleOpActive(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-cycle-clash", State: model.MachineStateCommissioned,
		PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned); err != nil {
		t.Fatalf("create deploy op: %v", err)
	}

	// A power-cycle op alongside an active deploy must be refused.
	// CreatePowerOp's own pre-check looks for active power ops only,
	// so the rejection comes from the unique index — and that is the
	// guard we are pinning.
	if _, err := st.CreatePowerOp(m.ID, model.OpPowerCycle, nil); err == nil {
		t.Fatal("expected error creating power-cycle op while a deploy op is active, got nil")
	}
}

// TestCreateMachineOp_DoesNotAdvanceState pins the new state-on-claim
// semantic: CreateMachineOp inserts a pending op but leaves the
// machine in its resting state. The state moves only when a worker
// claims the op via ClaimOperation.
func TestCreateMachineOp_DoesNotAdvanceState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-create-no-flip",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned); err != nil {
		t.Fatalf("create deploy op: %v", err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateCommissioned {
		t.Errorf("machine state should still be commissioned at create-time, got %s", got.State)
	}
}

// TestCreateMachineOp_MintsTraceID pins the trace_id contract: every
// op born through CreateMachineOp must carry a non-nil TraceID,
// distinct from its operation ID. The trace_id is the propagation
// key used by the structured-logs pipeline.
func TestCreateMachineOp_MintsTraceID(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-trace-id",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatalf("create deploy op: %v", err)
	}
	if op.TraceID == uuid.Nil {
		t.Errorf("CreateMachineOp left TraceID nil")
	}
	if op.TraceID == op.ID {
		t.Errorf("TraceID should be distinct from operation ID; both are %s", op.ID)
	}
	// Round-trip via GetOperation to confirm the column is persisted.
	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TraceID != op.TraceID {
		t.Errorf("TraceID not persisted: created %s, fetched %s", op.TraceID, got.TraceID)
	}
}

// TestCreatePowerOp_MintsTraceID is the power-op counterpart to
// TestCreateMachineOp_MintsTraceID.
func TestCreatePowerOp_MintsTraceID(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-power-trace",
		State: model.MachineStateCommissioned,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreatePowerOp(m.ID, model.OpPowerCycle, nil)
	if err != nil {
		t.Fatalf("create power op: %v", err)
	}
	if op.TraceID == uuid.Nil {
		t.Errorf("CreatePowerOp left TraceID nil")
	}
}

// TestCreateMachineOp_AllowedSourceCheckedBeforeUniqueIndex pins the
// validation order: an operator who tries to deploy a `new` (un-
// commissioned) machine should get a clear "must commission first"
// error, not the generic "machine has an active operation" error.
func TestCreateMachineOp_AllowedSourceCheckedBeforeUniqueIndex(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-source-order",
		State: model.MachineStateNew, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Pre-existing pending commission op (legitimate). A subsequent
	// deploy attempt should fail on allowed-source ("state new") not
	// on "active operation".
	if _, err := st.CreateMachineOp(m.ID, model.OpCommission, nil,
		model.MachineStateNew); err != nil {
		t.Fatalf("seed commission op: %v", err)
	}
	_, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned, model.MachineStateFailed)
	if err == nil {
		t.Fatal("expected error deploying a `new` machine")
	}
	if !strings.Contains(err.Error(), `state "new"`) {
		t.Errorf("expected allowed-source error mentioning state \"new\", got: %v", err)
	}
}

// TestClaimOperation_AdvancesMachineState pins the new claim-time
// state-flip: claiming a deploy op moves the machine from
// commissioned to deploying. The machine state was untouched at
// CreateMachineOp time.
func TestClaimOperation_AdvancesMachineState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-claim-deploy",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimOperation(op.ID, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed op, got nil")
	}
	if claimed.Status != model.OpClaimed {
		t.Errorf("op status: got %s, want claimed", claimed.Status)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateDeploying {
		t.Errorf("machine state after claim: got %s, want deploying", got.State)
	}
}

// TestClaimOperation_CommissionAdvancesToCommissioning verifies
// commission-op claim moves new → commissioning.
func TestClaimOperation_CommissionAdvancesToCommissioning(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-claim-commission",
		State: model.MachineStateNew, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpCommission, nil,
		model.MachineStateNew)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateCommissioning {
		t.Errorf("machine state: got %s, want commissioning", got.State)
	}
}

// TestClaimOperation_RecommissionFromCommissioned covers the
// commissioned → commissioning transition (re-inventory of a host that
// commissioned but hasn't deployed yet — e.g. to re-run the cable
// reconcile after a commission-side fix). commissioned is a resting, unlocked
// state, so the op both creates and claims without an unlock dance.
func TestClaimOperation_RecommissionFromCommissioned(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-recommission",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Mirror the API handler's allowed-source set, which must include
	// commissioned for this to create.
	op, err := st.CreateMachineOp(m.ID, model.OpCommission, nil,
		model.MachineStateNew, model.MachineStateCommissioned,
		model.MachineStateFailed, model.MachineStateRunning)
	if err != nil {
		t.Fatalf("create commission op from commissioned: %v", err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateCommissioning {
		t.Errorf("machine state: got %s, want commissioning", got.State)
	}
}

// TestClaimOperation_RestingStateGuard verifies the small race window
// where state moves between op-create and op-claim is closed by the
// resting-state re-check at claim time. We synthesize the race by
// flipping the machine state directly between create and claim.
func TestClaimOperation_RestingStateGuard(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-claim-guard",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate state moving to a still-disallowed state between op-create
	// and op-claim. `deploying` is a transient state, never a valid deploy
	// source, so the claim-time guard must refuse. (`running` is now an
	// allowed source — see TestClaimOperation_LockGuard for that path.)
	if err := st.db.Model(&model.Machine{}).Where("id = ?", m.ID).
		Update("state", model.MachineStateDeploying).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err == nil {
		t.Fatal("expected claim to refuse against deploying machine")
	}
	// Op should still be pending — failed claims must not leave half-state.
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpPending {
		t.Errorf("op status: got %s, want pending (claim should be all-or-nothing)", gotOp.Status)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateDeploying {
		t.Errorf("machine state should be unchanged after rejected claim, got %s", gotM.State)
	}
}

// TestClaimOperation_LockGuard verifies the claim-time lock check: a
// machine that is in an allowed source state but locked (e.g. a running
// host auto-locked on deploy completion) must refuse the claim, mirroring
// CreateMachineOp's lock gate. This is the claim-time half of the
// "don't wipe an in-service host without an explicit unlock" guarantee.
func TestClaimOperation_LockGuard(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-claim-lock-guard",
		State: model.MachineStateFailed, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Create a deploy op from a valid (unlocked) source.
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateFailed)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the machine becoming a locked, running host between
	// op-create and op-claim (running is an allowed source, so only the
	// lock should block the claim).
	if err := st.db.Model(&model.Machine{}).Where("id = ?", m.ID).
		Updates(map[string]any{"state": model.MachineStateRunning, "locked": true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err == nil {
		t.Fatal("expected claim to refuse against locked machine")
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpPending {
		t.Errorf("op status: got %s, want pending after rejected claim", gotOp.Status)
	}
}

// TestClaimOperation_PowerOpDoesNotAdvanceState verifies power ops
// claim cleanly without touching machine state.
func TestClaimOperation_PowerOpDoesNotAdvanceState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-power-claim",
		State: model.MachineStateRunning,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreatePowerOp(m.ID, model.OpPowerCycle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim power op: %v", err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateRunning {
		t.Errorf("power-op claim must not touch machine state, got %s", got.State)
	}
}

// TestClaimOperation_AlreadyClaimedReturnsNil verifies the contract
// the WS hub depends on: re-claiming a non-pending op returns
// (nil, nil), not an error.
func TestClaimOperation_AlreadyClaimedReturnsNil(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-double-claim",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	second, err := st.ClaimOperation(op.ID, "worker-2")
	if err != nil {
		t.Fatalf("re-claim should be (nil, nil), got err: %v", err)
	}
	if second != nil {
		t.Fatalf("re-claim should return nil op, got: %+v", second)
	}
}

// TestFailOperation_PendingDoesNotTouchState pins rule 1: a pending
// op (never claimed → never advanced state) being failed must NOT
// pull the machine state down.
func TestFailOperation_PendingDoesNotTouchState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-pending",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailOperation(op.ID, []byte(`{"error":"timeout"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateCommissioned {
		t.Errorf("machine state must not move on pending-op failure, got %s", gotM.State)
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpFailed {
		t.Errorf("op status: got %s, want failed", gotOp.Status)
	}
}

// TestFailOperation_PowerOpDoesNotTouchState pins rule 2: power-op
// failures must not pull machine state.
func TestFailOperation_PowerOpDoesNotTouchState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-power",
		State: model.MachineStateRunning,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreatePowerOp(m.ID, model.OpPowerCycle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.FailOperation(op.ID, []byte(`{"error":"bmc unreachable"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateRunning {
		t.Errorf("power-op failure must not move state, got %s", gotM.State)
	}
}

// TestFailOperation_BMCReconcileDoesNotTouchState: a bmc_reconcile op
// targets in-service hosts (running, auto-locked). Claiming it must not
// require an unlock, and failing it must leave the machine running AND
// locked — not flip it to failed and auto-unlock it.
func TestFailOperation_BMCReconcileDoesNotTouchState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-bmcrec",
		State: model.MachineStateRunning, Locked: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreatePowerOp(m.ID, model.OpBMCReconcile, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimOperation(op.ID, "worker-1")
	if err != nil || claimed == nil {
		t.Fatalf("claim on a locked running machine: op=%v err=%v", claimed, err)
	}
	if err := st.FailOperation(op.ID, []byte(`{"error":"bmc unreachable"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateRunning || !gotM.Locked {
		t.Errorf("bmc_reconcile failure must not move state or lock, got state=%s locked=%v", gotM.State, gotM.Locked)
	}
}

// TestFailOperation_SetBootDeviceDoesNotTouchState is the regression for a
// failed `machine power boot-set` on an in-service host: set_boot_device is
// an out-of-band BMC op, so failing it must leave the machine running AND
// locked, not flip it to failed and auto-unlock it.
func TestFailOperation_SetBootDeviceDoesNotTouchState(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-bootset",
		State: model.MachineStateRunning, Locked: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreatePowerOp(m.ID, model.OpSetBootDevice, []byte(`{"device":"pxe"}`))
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimOperation(op.ID, "worker-1"); err != nil || claimed == nil {
		t.Fatalf("claim: op=%v err=%v", claimed, err)
	}
	out, err := st.FailOperationOutcome(op.ID, []byte(`{"error":"boot-device set not supported"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || out.MachineState != "" {
		t.Errorf("fail outcome must carry no durable machine state, got %+v", out)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateRunning || !gotM.Locked {
		t.Errorf("set_boot_device failure must not move state or lock, got state=%s locked=%v", gotM.State, gotM.Locked)
	}
}

// TestFailOperation_InitialCommissionRevertsToNew pins rule 3: a
// commission op whose machine has no commission_data yet (its first
// commission) reverts to `new` on failure, not `failed`. This
// preserves the auto-commission path: the operator can retry without
// a manual SQL flip.
func TestFailOperation_InitialCommissionRevertsToNew(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-initial",
		State: model.MachineStateNew, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpCommission, nil,
		model.MachineStateNew)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// Confirm setup: machine moved to commissioning.
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateCommissioning {
		t.Fatalf("setup: machine should be commissioning, got %s", gotM.State)
	}

	if err := st.FailOperation(op.ID, []byte(`{"error":"agent died"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ = st.GetMachine(m.ID)
	if gotM.State != model.MachineStateNew {
		t.Errorf("initial-commission failure should revert to new, got %s", gotM.State)
	}
}

// TestFailOperation_RecommissionGoesToFailed verifies rule 3 only
// fires when commission_data is empty: a re-commission of an
// already-commissioned machine that fails should land in `failed`,
// not `new`.
func TestFailOperation_RecommissionGoesToFailed(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-recomm",
		State:          model.MachineStateCommissioned,
		PXEEligible:    true,
		CommissionData: []byte(`{"system":{"serial_number":"SN-fail-recomm"}}`),
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// CreateMachineOp doesn't allow commission from `commissioned`
	// in forward-path scope. Insert directly to bypass.
	op := &model.Operation{
		ID:        uuid.New(),
		MachineID: m.ID,
		Type:      model.OpCommission,
		Status:    model.OpClaimed,
	}
	now := time.Now()
	op.ClaimedAt = &now
	op.ClaimedBy = "worker-1"
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	// Move machine to commissioning to mirror state-on-claim.
	if err := st.db.Model(&model.Machine{}).Where("id = ?", m.ID).
		Update("state", model.MachineStateCommissioning).Error; err != nil {
		t.Fatal(err)
	}

	if err := st.FailOperation(op.ID, []byte(`{"error":"flake"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateFailed {
		t.Errorf("re-commission failure should go to failed, got %s", gotM.State)
	}
}

// TestFailOperation_DeployFailureGoesToFailed pins rule 4: a deploy
// op failure pulls the machine to `failed`.
func TestFailOperation_DeployFailureGoesToFailed(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-fail-deploy",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.FailOperation(op.ID, []byte(`{"error":"partition failed"}`)); err != nil {
		t.Fatal(err)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateFailed {
		t.Errorf("deploy failure should go to failed, got %s", gotM.State)
	}
}

// TestAbortMachine_RoutesViaFailOperation verifies AbortMachine's
// per-op-status branching: an aborted pending op leaves machine
// state alone; an aborted claimed op pulls state down per the
// FailOperation rules.
func TestAbortMachine_RoutesViaFailOperation(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-abort-pending",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Pending deploy op — never claimed.
	if _, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.AbortMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MachineStateCommissioned {
		t.Errorf("abort of pending op should leave state alone, got %s", got.State)
	}
}

// TestExpirePendingOperations_LeavesMachineStateAlone pins the
// behaviour described in the spec: pending ops never advanced state,
// so the timeout sweep that fails them must not pull state down.
func TestExpirePendingOperations_LeavesMachineStateAlone(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-pending-tmo",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	// Backdate created_at so the sweep picks it up.
	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("created_at", time.Now().Add(-25*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	outs, err := st.ExpirePendingOperations(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Errorf("expected 1 expired, got %d", len(outs))
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpFailed {
		t.Errorf("op status: got %s, want failed", gotOp.Status)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateCommissioned {
		t.Errorf("machine state must be untouched, got %s", gotM.State)
	}
}

// TestExpireStuckOperations_ExcludesAwaitingPhoneHome pins the
// guarantee that a deploy in phase=awaiting_phone_home is NOT
// fail-swept by the OpTimeout — phone-home has its own (independent)
// deadline anchored on phase_entered_at.
func TestExpireStuckOperations_ExcludesAwaitingPhoneHome(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-aph",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetOperationPhase(op.ID, "awaiting_phone_home"); err != nil {
		t.Fatal(err)
	}
	// Backdate claimed_at well past the OpTimeout.
	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("claimed_at", time.Now().Add(-1*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	outs, err := st.ExpireStuckOperations(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 0 {
		t.Errorf("ExpireStuckOperations should skip awaiting_phone_home, got %d", len(outs))
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status == model.OpFailed {
		t.Error("op should not be failed by OpTimeout while in awaiting_phone_home")
	}
}

// TestExpirePhoneHomeOperations_FailsStuckDeploys pins the
// PhoneHomeTimeout sweep. A deploy that's been in awaiting_phone_home
// past the cutoff fails; the machine moves to `failed` per the
// FailOperation rules.
func TestExpirePhoneHomeOperations_FailsStuckDeploys(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-aph-fail",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetOperationPhase(op.ID, "awaiting_phone_home"); err != nil {
		t.Fatal(err)
	}
	// Backdate phase_entered_at past the timeout.
	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("phase_entered_at", time.Now().Add(-31*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	outs, err := st.ExpirePhoneHomeOperations(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Errorf("expected 1 expired, got %d", len(outs))
	}
	gotOp, _ := st.GetOperation(op.ID)
	if gotOp.Status != model.OpFailed {
		t.Errorf("op status: got %s, want failed", gotOp.Status)
	}
	gotM, _ := st.GetMachine(m.ID)
	if gotM.State != model.MachineStateFailed {
		t.Errorf("machine state: got %s, want failed", gotM.State)
	}
}

// TestSetOperationPhase_BumpsPhaseEnteredAt verifies the
// phase-entry-at column is touched on phase change so PhoneHomeTimeout
// has an accurate deadline anchor.
func TestSetOperationPhase_BumpsPhaseEnteredAt(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-phase-bump",
		State: model.MachineStateDeploying, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{MachineID: m.ID, Type: model.OpDeploy, Status: model.OpClaimed}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	// Force phase_entered_at into the past so the bump is visible.
	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("phase_entered_at", time.Now().Add(-2*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetOperation(op.ID)
	if _, err := st.SetOperationPhase(op.ID, "partitioning"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetOperation(op.ID)
	if !after.PhaseEnteredAt.After(before.PhaseEnteredAt) {
		t.Errorf("phase_entered_at not bumped: before=%s after=%s",
			before.PhaseEnteredAt, after.PhaseEnteredAt)
	}

	// Same-phase write must NOT bump (deadline non-renewable on noisy
	// duplicate phase emissions).
	prev := after.PhaseEnteredAt
	if _, err := st.SetOperationPhase(op.ID, "partitioning"); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetOperation(op.ID)
	if !again.PhaseEnteredAt.Equal(prev) {
		t.Errorf("same-phase write bumped phase_entered_at: prev=%s now=%s",
			prev, again.PhaseEnteredAt)
	}
}

func TestSetOperationPhase_UpdatesFieldOnly(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-5", State: model.MachineStateDeploying,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{MachineID: m.ID, Type: model.OpDeploy, Status: model.OpRunning}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetOperationPhase(op.ID, "partitioning"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetOperation(op.ID)
	if got.Phase != "partitioning" {
		t.Errorf("phase: got %q, want partitioning", got.Phase)
	}
	if got.Status != model.OpRunning {
		t.Errorf("status should be untouched, got %s", got.Status)
	}
}

// TestSetOperationPhase_TransitionShape pins the
// PhaseTransition return shape that the hub uses to emit the
// "phase transition" log envelope. First write reports Changed=true
// with empty PrevPhase; second (different phase) reports Changed=true
// with prev_phase set and a positive duration; third (same phase)
// reports Changed=false.
func TestSetOperationPhase_TransitionShape(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-trans",
		State: model.MachineStateDeploying, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{MachineID: m.ID, Type: model.OpDeploy, Status: model.OpClaimed}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	t1, err := st.SetOperationPhase(op.ID, "pxe_booting")
	if err != nil {
		t.Fatalf("first SetOperationPhase: %v", err)
	}
	if !t1.Changed || t1.PrevPhase != "" || t1.NewPhase != "pxe_booting" {
		t.Errorf("first transition: got %+v, want Changed=true PrevPhase=\"\" NewPhase=pxe_booting", t1)
	}
	if t1.MachineID != m.ID {
		t.Errorf("MachineID = %v, want %v", t1.MachineID, m.ID)
	}

	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("phase_entered_at", time.Now().Add(-7*time.Second)).Error; err != nil {
		t.Fatal(err)
	}

	t2, err := st.SetOperationPhase(op.ID, "partitioning")
	if err != nil {
		t.Fatalf("second SetOperationPhase: %v", err)
	}
	if !t2.Changed || t2.PrevPhase != "pxe_booting" || t2.NewPhase != "partitioning" {
		t.Errorf("second transition: got %+v, want Changed=true PrevPhase=pxe_booting NewPhase=partitioning", t2)
	}
	if t2.Duration < 5*time.Second || t2.Duration > 30*time.Second {
		t.Errorf("Duration = %v, want roughly 7s", t2.Duration)
	}

	t3, err := st.SetOperationPhase(op.ID, "partitioning")
	if err != nil {
		t.Fatalf("third SetOperationPhase: %v", err)
	}
	if t3.Changed {
		t.Errorf("same-phase write reported Changed=true: %+v", t3)
	}
}

// claimedDeployInPhase creates a claimed deploy op whose phase is already
// set, for the overlapping-write tests below.
func claimedDeployInPhase(t *testing.T, st *Store, serial, phase string) uuid.UUID {
	t.Helper()
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: serial,
		State: model.MachineStateDeploying, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{MachineID: m.ID, Type: model.OpDeploy, Status: model.OpClaimed}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetOperationPhase(op.ID, phase); err != nil {
		t.Fatal(err)
	}
	return op.ID
}

// overlapSetOperationPhase runs two SetOperationPhase calls on one op that
// overlap the way two deliveries of a phase report can (the worker's outbox
// resending an op_update on a new connection while the hub is still
// handling the first copy, or one agent report reaching both site workers
// of an HA pair): the second call starts after the first has read the op
// row and before the first writes. It returns both transitions.
//
// The pause is a gorm query callback on the first call's phase read, the
// only query that names phase_entered_at. The callback starts the second
// call in a goroutine and returns once that call has either finished or is
// waiting on a row lock, so the interleave is deterministic whether or not
// SetOperationPhase locks the row it reads.
func overlapSetOperationPhase(t *testing.T, st *Store, opID uuid.UUID, firstPhase, secondPhase string) (first, second PhaseTransition) {
	t.Helper()
	type result struct {
		tr  PhaseTransition
		err error
	}
	secondDone := make(chan result, 1)
	sqlDB, err := st.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	name := "overlap-set-phase:" + uuid.NewString()
	fired := false
	if err := st.db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if fired || !strings.Contains(tx.Statement.SQL.String(), `"phase_entered_at"`) {
			return
		}
		fired = true
		go func() {
			tr, err := st.SetOperationPhase(opID, secondPhase)
			secondDone <- result{tr, err}
		}()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case r := <-secondDone:
				secondDone <- r
				return
			case <-time.After(2 * time.Millisecond):
			}
			var waiters int
			if err := sqlDB.QueryRow(`SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiters); err == nil && waiters > 0 {
				return
			}
		}
		t.Error("second SetOperationPhase neither finished nor waited on a row lock within 10s")
	}); err != nil {
		t.Fatalf("register interleave callback: %v", err)
	}
	defer func() { _ = st.db.Callback().Query().Remove(name) }()

	first, err = st.SetOperationPhase(opID, firstPhase)
	if err != nil {
		t.Fatalf("first SetOperationPhase: %v", err)
	}
	if !fired {
		t.Fatal("interleave callback never fired: SetOperationPhase's read no longer names phase_entered_at")
	}
	select {
	case r := <-secondDone:
		if r.err != nil {
			t.Fatalf("second SetOperationPhase: %v", r.err)
		}
		return first, r.tr
	case <-time.After(10 * time.Second):
		t.Fatal("second SetOperationPhase did not return within 10s")
	}
	return first, second
}

// TestSetOperationPhase_OverlappingDuplicateChangesPhaseOnce: two
// overlapping deliveries of the same phase report record one phase change.
// The other is the idempotent repeat (Changed=false). Recording both would
// publish two op_deltas and two "phase transition" envelopes, observe
// PhaseDuration twice, write phase_entered_at (the phone-home deadline
// anchor) twice, and bump the version by two.
func TestSetOperationPhase_OverlappingDuplicateChangesPhaseOnce(t *testing.T) {
	st := testStore(t)
	opID := claimedDeployInPhase(t, st, "SN-dup-phase", "finalizing")
	before, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}

	first, second := overlapSetOperationPhase(t, st, opID, "awaiting_phone_home", "awaiting_phone_home")

	if first.Changed == second.Changed {
		t.Errorf("Changed: first=%v second=%v, want exactly one delivery to record the phase change",
			first.Changed, second.Changed)
	}
	after, err := st.GetOperation(opID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version+1 {
		t.Errorf("version moved %d -> %d for one phase change, want +1", before.Version, after.Version)
	}
}

// TestSetOperationPhase_OverlappingWritersChainPrevPhase: two overlapping
// writes of different phases both change the phase, and the transitions
// chain. The later write's PrevPhase is the phase the earlier write set
// and its Duration runs from the earlier write, not from the phase both
// started in. These feed the "phase transition" envelope and the
// PhaseDuration metric.
func TestSetOperationPhase_OverlappingWritersChainPrevPhase(t *testing.T) {
	st := testStore(t)
	opID := claimedDeployInPhase(t, st, "SN-overlap-phase", "finalizing")
	// Age the starting phase so a Duration measured from it stands out.
	if err := st.db.Model(&model.Operation{}).Where("id = ?", opID).
		Update("phase_entered_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	first, second := overlapSetOperationPhase(t, st, opID, "bmc_reconcile", "awaiting_phone_home")

	if !first.Changed || !second.Changed {
		t.Fatalf("Changed: first=%v second=%v, want both (the phases differ)", first.Changed, second.Changed)
	}
	earlier, later := first, second
	if later.PrevPhase == "finalizing" {
		earlier, later = second, first
	}
	if earlier.PrevPhase != "finalizing" || later.PrevPhase != earlier.NewPhase {
		t.Errorf("transitions do not chain: %q -> %q and %q -> %q, want finalizing -> one -> the other",
			first.PrevPhase, first.NewPhase, second.PrevPhase, second.NewPhase)
	}
	if later.Duration > time.Minute {
		t.Errorf("later transition's Duration = %v, want it measured from the earlier write (milliseconds), not from finalizing (1h)",
			later.Duration)
	}
}

// --- Forward-path (running → deploying/commissioning) + auto-lock ---

// TestCompleteOperationWithState_AutoLocksOnRunning verifies a machine
// is locked when it transitions to running. A running host is in service;
// the lock makes the next deploy/commission require an explicit unlock.
func TestCompleteOperationWithState_AutoLocksOnRunning(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-autolock",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateRunning {
		t.Fatalf("state: got %s, want running", got.State)
	}
	if !got.Locked {
		t.Error("machine should be auto-locked on → running")
	}
}

// TestFailOperation_AutoUnlocksOnFailed verifies a deploy that fails a
// running machine clears the lock — the operator retries without an unlock
// dance, because the lock tracks the running state, not a sticky bit.
//
// The setup walks the realistic redeploy flow: a running+locked host is
// unlocked by the operator, a redeploy op is created and claimed (running →
// deploying), then the op fails (deploying → failed). The failure must leave
// the machine unlocked.
func TestFailOperation_AutoUnlocksOnFailed(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-autounlock",
		State: model.MachineStateRunning, PXEEligible: true, Locked: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Operator unlocks the in-service host to redeploy it.
	if err := st.SetMachineLocked(m.ID, false); err != nil {
		t.Fatal(err)
	}
	// Redeploy: create + claim from running.
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateRunning)
	if err != nil {
		t.Fatalf("create deploy op from running: %v", err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.FailOperation(op.ID, []byte(`{"error":"redeploy failed"}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMachine(m.ID)
	if got.State != model.MachineStateFailed {
		t.Errorf("state: got %s, want failed", got.State)
	}
	if got.Locked {
		t.Error("machine should be auto-unlocked on → failed")
	}
}

// TestCreateMachineOp_LockedRunningRejectsBothOpTypes verifies the
// create-time lock gate: a running, auto-locked machine refuses both a
// deploy and a commission op until unlocked. This is the operator-facing
// half of the "no accidental wipe of an in-service host" guarantee.
func TestCreateMachineOp_LockedRunningRejectsBothOpTypes(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-locked-running",
		State: model.MachineStateRunning, PXEEligible: true, Locked: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	// Locked: both op types rejected even though `running` is now an
	// allowed source state.
	if _, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning); err == nil {
		t.Error("expected deploy on locked running machine to be rejected")
	}
	if _, err := st.CreateMachineOp(m.ID, model.OpCommission, nil,
		model.MachineStateNew, model.MachineStateFailed, model.MachineStateRunning); err == nil {
		t.Error("expected commission on locked running machine to be rejected")
	}

	// After unlock, a deploy from running is accepted (redeploy path).
	if err := st.SetMachineLocked(m.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning); err != nil {
		t.Errorf("deploy from unlocked running machine should be accepted: %v", err)
	}
}

// --- B4: idempotent re-claim + orphaned-claim sweep ---

// TestClaimOperation_IdempotentReclaimSameWorker pins the B4 lost-reply
// recovery: a worker whose claim committed but whose reply was lost
// retries the claim; the same worker re-claiming an op it already
// holds (no phase yet) gets the op back instead of claim-lost.
func TestClaimOperation_IdempotentReclaimSameWorker(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-reclaim",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimOperation(op.ID, "worker-1")
	if err != nil || first == nil {
		t.Fatalf("first claim: op=%v err=%v", first, err)
	}

	// Same worker retries (reply lost on WS drop): must get the op back.
	second, err := st.ClaimOperation(op.ID, "worker-1")
	if err != nil {
		t.Fatalf("re-claim errored: %v", err)
	}
	if second == nil {
		t.Fatal("same-worker re-claim returned claim-lost; the op is stranded until a sweep (B4)")
	}
	if second.ClaimedBy != "worker-1" || second.Status != model.OpClaimed {
		t.Fatalf("re-claimed op: %+v", second)
	}
	// claimed_at must have been bumped so the orphan grace restarts.
	if !second.ClaimedAt.After(*first.ClaimedAt) && !second.ClaimedAt.Equal(*first.ClaimedAt) {
		t.Fatalf("re-claim did not refresh claimed_at: first=%v second=%v",
			first.ClaimedAt, second.ClaimedAt)
	}

	// A DIFFERENT worker still reads claim-lost.
	third, err := st.ClaimOperation(op.ID, "worker-2")
	if err != nil {
		t.Fatalf("other-worker claim errored: %v", err)
	}
	if third != nil {
		t.Fatalf("other worker stole the claim: %+v", third)
	}
}

// TestClaimOperation_ReclaimRefusedOncePhaseRecorded: once actuation
// started (any phase on the op), even the claiming worker's retry
// reads claim-lost — re-driving a partially-executed op would re-fire
// BMC power actions against unknown hardware state.
func TestClaimOperation_ReclaimRefusedOncePhaseRecorded(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-reclaim-phase",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetOperationPhase(op.ID, "power_cycle"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ClaimOperation(op.ID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("re-claim after phase recorded must be refused, got: %+v", got)
	}
}

// TestExpireOrphanedClaims pins the B4 sweep: claimed-no-phase past
// the grace fails (worker died post-claim); claimed WITH a phase, or
// within the grace, is untouched.
func TestExpireOrphanedClaims(t *testing.T) {
	st := testStore(t)
	mkClaimedOp := func(serial string) *model.Operation {
		t.Helper()
		m := &model.Machine{
			ID: uuid.New(), NetBoxSerial: serial,
			State: model.MachineStateCommissioned, PXEEligible: true,
		}
		if err := st.CreateMachine(m); err != nil {
			t.Fatal(err)
		}
		op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
			model.MachineStateCommissioned)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimOperation(op.ID, "worker-dead")
		if err != nil || claimed == nil {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		return claimed
	}
	backdate := func(op *model.Operation, age time.Duration) {
		t.Helper()
		if err := st.DB().Model(&model.Operation{}).Where("id = ?", op.ID).
			Update("claimed_at", time.Now().Add(-age)).Error; err != nil {
			t.Fatal(err)
		}
	}

	orphan := mkClaimedOp("SN-orphan")
	backdate(orphan, 10*time.Minute)

	fresh := mkClaimedOp("SN-fresh-claim") // inside grace, untouched

	progressed := mkClaimedOp("SN-progressed")
	backdate(progressed, 10*time.Minute)
	if _, err := st.SetOperationPhase(progressed.ID, "power_cycle"); err != nil {
		t.Fatal(err)
	}

	outs, err := st.ExpireOrphanedClaims(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Fatalf("expected exactly 1 orphan swept, got %d", len(outs))
	}
	assertStatus := func(id uuid.UUID, want model.OperationStatus) {
		t.Helper()
		got, err := st.GetOperation(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Fatalf("op %s: status %s, want %s", id, got.Status, want)
		}
	}
	assertStatus(orphan.ID, model.OpFailed)
	assertStatus(fresh.ID, model.OpClaimed)
	assertStatus(progressed.ID, model.OpClaimed)

	// The orphan's machine state must have been pulled down (claim
	// advanced it to deploying; the fail reverts to failed).
	machine, err := st.GetMachine(orphan.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if machine.State != model.MachineStateFailed {
		t.Fatalf("orphaned op's machine: state %s, want failed", machine.State)
	}
}

// TestOperationVersion_BumpsAcrossMutations pins the Tier 2 Phase 0
// contract: a freshly created op is version 0, and every mutating
// UPDATE (claim → phase → complete) bumps version by exactly 1 in the
// same statement. The op-delta stream applies last-writer-wins by this
// value, so a monotonic bump per mutation is the load-bearing invariant.
func TestOperationVersion_BumpsAcrossMutations(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-version-bump",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	// INSERT leaves version at the default 0.
	got, _ := st.GetOperation(op.ID)
	if got.Version != 0 {
		t.Fatalf("freshly created op version: got %d, want 0", got.Version)
	}

	// Claim bumps 0 → 1.
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetOperation(op.ID)
	if got.Version != 1 {
		t.Fatalf("version after claim: got %d, want 1", got.Version)
	}

	// Phase change bumps 1 → 2.
	if _, err := st.SetOperationPhase(op.ID, "partitioning"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetOperation(op.ID)
	if got.Version != 2 {
		t.Fatalf("version after phase change: got %d, want 2", got.Version)
	}

	// A same-phase write does NOT mutate the row, so version must hold.
	if _, err := st.SetOperationPhase(op.ID, "partitioning"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetOperation(op.ID)
	if got.Version != 2 {
		t.Fatalf("version after no-op same-phase write: got %d, want 2", got.Version)
	}

	// Completion bumps 2 → 3.
	if _, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetOperation(op.ID)
	if got.Version != 3 {
		t.Fatalf("version after complete: got %d, want 3", got.Version)
	}
}

// TestOperationVersion_BumpedOnSweepFail pins that a swept (timed-out)
// op also carries a bumped version — the sweeps route through
// FailOperation, whose CAS UPDATE bumps version alongside the status
// flip, so the terminal transition is versioned like every other.
func TestOperationVersion_BumpedOnSweepFail(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-version-sweep",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// version is now 1 (claim). Backdate claimed_at so ExpireStuckOperations
	// picks it up, then sweep it. The sweep's FailOperation CAS must bump
	// version 1 → 2.
	beforeSweep, _ := st.GetOperation(op.ID)
	if beforeSweep.Version != 1 {
		t.Fatalf("version after claim: got %d, want 1", beforeSweep.Version)
	}
	if err := st.db.Model(&model.Operation{}).Where("id = ?", op.ID).
		Update("claimed_at", time.Now().Add(-1*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	outs, err := st.ExpireStuckOperations(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Fatalf("expected 1 op swept, got %d", len(outs))
	}
	got, _ := st.GetOperation(op.ID)
	if got.Status != model.OpFailed {
		t.Fatalf("op status: got %s, want failed", got.Status)
	}
	if got.Version != 2 {
		t.Fatalf("version after sweep-fail: got %d, want 2", got.Version)
	}
}

// TestFailInFlightOpsForMachine (B7): every non-terminal op on the
// machine fails; terminal ops and other machines' ops are untouched.
func TestFailInFlightOpsForMachine(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-drift",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	other := &model.Machine{
		ID: uuid.New(), NetBoxSerial: "SN-bystander",
		State: model.MachineStateCommissioned, PXEEligible: true,
	}
	if err := st.CreateMachine(other); err != nil {
		t.Fatal(err)
	}
	otherOp, err := st.CreateMachineOp(other.ID, model.OpDeploy, nil,
		model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}

	outs, err := st.FailInFlightOpsForMachine(m.ID, "netbox device deleted mid-operation")
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Fatalf("expected 1 op failed, got %d", len(outs))
	}
	// The outcome carries the failed post-mutation row + the durable state
	// the fail wrote (claimed deploy -> failed), which the mirror publishes
	// as op_removed's machine_state.
	if outs[0].Op.ID != op.ID || outs[0].Op.Status != model.OpFailed {
		t.Fatalf("outcome op = %+v, want failed op %s", outs[0].Op, op.ID)
	}
	if outs[0].MachineState != model.MachineStateFailed {
		t.Fatalf("outcome machine state = %q, want failed", outs[0].MachineState)
	}
	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.OpFailed {
		t.Fatalf("op status: %s want failed", got.Status)
	}
	// The claim advanced the machine to deploying; the fail freezes it
	// in failed (rule 3/4 of FailOperation).
	mm, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mm.State != model.MachineStateFailed {
		t.Fatalf("machine state: %s want failed", mm.State)
	}
	// Bystander untouched.
	ob, err := st.GetOperation(otherOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ob.Status != model.OpPending {
		t.Fatalf("bystander op: %s want pending", ob.Status)
	}
}

// TestListActiveOperationsClaimedBy verifies the site-worker-delete guard
// query: it returns active ops claimed by a worker, and excludes both
// terminal ops with the same claimed_by and active ops claimed by another
// worker.
func TestListActiveOperationsClaimedBy(t *testing.T) {
	st := testStore(t)
	// One machine per op: idx_one_active_op_per_machine forbids two
	// active (pending/claimed/running) ops on the same machine.
	mkMachine := func(serial string, state model.MachineState) uuid.UUID {
		m := &model.Machine{ID: uuid.New(), NetBoxSerial: serial, State: state}
		if err := st.CreateMachine(m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}

	// Active op claimed by worker-1.
	active := &model.Operation{
		MachineID: mkMachine("SN-CLAIM-1", model.MachineStateDeploying),
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		ClaimedBy: "worker-1",
	}
	if err := st.CreateOperation(active); err != nil {
		t.Fatal(err)
	}
	// Terminal op with the same claimed_by must NOT be returned. On its
	// own machine so it doesn't collide with the active op's machine (a
	// terminal op is outside the partial index, but keeping machines
	// distinct keeps the fixture unambiguous).
	done := &model.Operation{
		MachineID: mkMachine("SN-CLAIM-2", model.MachineStateRunning),
		Type:      model.OpDeploy,
		Status:    model.OpCompleted,
		ClaimedBy: "worker-1",
	}
	if err := st.CreateOperation(done); err != nil {
		t.Fatal(err)
	}
	// Active op claimed by a different worker must NOT be returned.
	other := &model.Operation{
		MachineID: mkMachine("SN-CLAIM-3", model.MachineStateDeploying),
		Type:      model.OpDeploy,
		Status:    model.OpRunning,
		ClaimedBy: "worker-2",
	}
	if err := st.CreateOperation(other); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListActiveOperationsClaimedBy("worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(got), got)
	}
	if got[0].ID != active.ID {
		t.Fatalf("returned op %s, want the active claimed op %s", got[0].ID, active.ID)
	}

	// A worker with no claims returns an empty set.
	none, err := st.ListActiveOperationsClaimedBy("worker-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("worker-3: got %d ops, want 0", len(none))
	}
}
