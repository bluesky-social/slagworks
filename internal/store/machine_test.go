package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestCreateMachineRequiresSerial(t *testing.T) {
	st := testStore(t)
	err := st.CreateMachine(&model.Machine{NetBoxSerial: ""})
	if err == nil {
		t.Fatal("expected error when serial is empty")
	}
}

func TestGetMachineByNetBoxSerial(t *testing.T) {
	st := testStore(t)
	nb := 42
	m := &model.Machine{ID: uuid.New(), NetBoxDeviceID: &nb, NetBoxSerial: "SN-ABC", State: model.MachineStateNew}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMachineByNetBoxSerial("SN-ABC")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.ID {
		t.Fatalf("got %s, want %s", got.ID, m.ID)
	}
}

func TestGetMachineByNetBoxDeviceID(t *testing.T) {
	st := testStore(t)
	nb := 42
	m := &model.Machine{ID: uuid.New(), NetBoxDeviceID: &nb, NetBoxSerial: "SN-ABC"}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMachineByNetBoxDeviceID(42)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.ID {
		t.Fatalf("got %s, want %s", got.ID, m.ID)
	}
}

func TestDeleteMachineIfIdle(t *testing.T) {
	st := testStore(t)
	nb := 42
	m := &model.Machine{ID: uuid.New(), NetBoxDeviceID: &nb, NetBoxSerial: "SN-D", State: model.MachineStateRunning}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteMachineIfIdle(m.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteMachineIfIdle = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := st.GetMachine(m.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound after delete, got %v", err)
	}
	// Already gone is what the caller asked for.
	deleted, err = st.DeleteMachineIfIdle(m.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteMachineIfIdle on a deleted machine = (%v, %v), want (true, nil)", deleted, err)
	}
}

// TestDeleteMachineIfIdle_KeepsMachineWithActiveOp: a machine with a
// pending, claimed or running op is left alone, op included.
func TestDeleteMachineIfIdle_KeepsMachineWithActiveOp(t *testing.T) {
	for _, status := range []model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning} {
		t.Run(string(status), func(t *testing.T) {
			st := testStore(t)
			m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-BUSY", State: model.MachineStateCommissioned}
			if err := st.CreateMachine(m); err != nil {
				t.Fatal(err)
			}
			op := &model.Operation{ID: uuid.New(), MachineID: m.ID, Type: model.OpDeploy, Status: status}
			if err := st.CreateOperation(op); err != nil {
				t.Fatal(err)
			}
			deleted, err := st.DeleteMachineIfIdle(m.ID)
			if err != nil || deleted {
				t.Fatalf("DeleteMachineIfIdle = (%v, %v), want (false, nil)", deleted, err)
			}
			if _, err := st.GetMachine(m.ID); err != nil {
				t.Errorf("machine deleted despite its %s op: %v", status, err)
			}
			got, err := st.GetOperation(op.ID)
			if err != nil {
				t.Fatalf("%s op deleted: %v", status, err)
			}
			if got.Status != status {
				t.Errorf("op status = %s, want %s untouched", got.Status, status)
			}
		})
	}
}

// TestDeleteMachineIfIdle_WaitsForConcurrentOpCreate pins the locking the
// idle check relies on: an op insert that is in progress when the delete
// starts holds FOR KEY SHARE on the machine row (through the operations
// foreign key), so DeleteMachineIfIdle's FOR UPDATE waits for it and, once
// it commits, sees the op and keeps the machine. Checking before taking
// the lock would miss the uncommitted op and then fail on the foreign key.
func TestDeleteMachineIfIdle_WaitsForConcurrentOpCreate(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-RACE", State: model.MachineStateCommissioned}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	create := st.db.Begin()
	t.Cleanup(func() { create.Rollback() })
	op := &model.Operation{ID: uuid.New(), MachineID: m.ID, Type: model.OpDeploy, Status: model.OpPending}
	if err := create.Create(op).Error; err != nil {
		t.Fatal(err)
	}

	var deleted bool
	var delErr error
	returned := make(chan struct{})
	go func() {
		deleted, delErr = st.DeleteMachineIfIdle(m.ID)
		close(returned)
	}()
	waitForLockWait(t, st, returned, "DeleteMachineIfIdle")

	if err := create.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteMachineIfIdle still blocked after the op insert committed")
	}
	if delErr != nil || deleted {
		t.Fatalf("DeleteMachineIfIdle = (%v, %v), want (false, nil): the op committed while it waited", deleted, delErr)
	}
	if _, err := st.GetOperation(op.ID); err != nil {
		t.Errorf("op deleted: %v", err)
	}
	if _, err := st.GetMachine(m.ID); err != nil {
		t.Errorf("machine deleted: %v", err)
	}
}

// TestCreateMachineOp_DeleteCommittedFirstIsMachineNotFound is the other
// order: the machine delete is in progress when the op create starts. The
// create must wait for it and then refuse the way it refuses a machine
// that is already gone ("machine not found"), not read the doomed row,
// pass its checks against it and fail later on the operations foreign key
// with an error that tells the operator nothing.
func TestCreateMachineOp_DeleteCommittedFirstIsMachineNotFound(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-GONE", State: model.MachineStateCommissioned, PXEEligible: true}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	del := st.db.Begin()
	t.Cleanup(func() { del.Rollback() })
	if err := del.Delete(&model.Machine{}, "id = ?", m.ID).Error; err != nil {
		t.Fatal(err)
	}

	var op *model.Operation
	var createErr error
	returned := make(chan struct{})
	go func() {
		op, createErr = st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateCommissioned)
		close(returned)
	}()
	waitForLockWait(t, st, returned, "CreateMachineOp")

	if err := del.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("CreateMachineOp still blocked after the delete committed")
	}
	if op != nil {
		t.Fatalf("CreateMachineOp created op %s on a deleted machine", op.ID)
	}
	if !errors.Is(createErr, gorm.ErrRecordNotFound) {
		t.Fatalf("CreateMachineOp error = %v, want machine not found", createErr)
	}
}

// waitForLockWait polls until a session in the test database is waiting on
// a lock, which here can only be the call under test blocking on the
// test's open transaction. It fails if the call returns first.
func waitForLockWait(t *testing.T, st *Store, returned <-chan struct{}, call string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int64
		if err := st.db.Raw(`SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND datname = current_database()`).
			Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-returned:
			t.Fatalf("%s returned without waiting for the open transaction", call)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never blocked on the open transaction", call)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDeleteMachineIfIdle_CascadesOperationsAndLogs verifies the delete
// cleans up child rows that don't have ON DELETE CASCADE on their
// FK. Without this cleanup, the FK constraint on operations.machine_id
// would block the delete entirely.
func TestDeleteMachineIfIdle_CascadesOperationsAndLogs(t *testing.T) {
	st := testStore(t)
	nb := 99
	m := &model.Machine{
		ID: uuid.New(), NetBoxDeviceID: &nb, NetBoxSerial: "SN-CASCADE",
		State: model.MachineStateRunning, PXEEligible: true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	// Add a (terminal) operation.
	op := &model.Operation{
		ID:        uuid.New(),
		MachineID: m.ID,
		Type:      model.OpDeploy,
		Status:    model.OpCompleted,
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}

	if deleted, err := st.DeleteMachineIfIdle(m.ID); err != nil || !deleted {
		t.Fatalf("delete with a terminal child op should cascade, got (%v, %v)", deleted, err)
	}
	if _, err := st.GetMachine(m.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("machine should be deleted, got: %v", err)
	}
	if _, err := st.GetOperation(op.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("operation should be deleted, got: %v", err)
	}
}

func TestListMachines(t *testing.T) {
	st := testStore(t)
	_ = st.CreateMachine(&model.Machine{ID: uuid.New(), NetBoxSerial: "A", State: model.MachineStateRunning})
	_ = st.CreateMachine(&model.Machine{ID: uuid.New(), NetBoxSerial: "B", State: model.MachineStateNew})
	all, err := st.ListMachines(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(all))
	}
}

func TestListMachinesByDeviceIDs(t *testing.T) {
	st := testStore(t)
	ids := []int{10, 20, 30}
	for _, id := range ids {
		if err := st.CreateMachine(&model.Machine{
			ID: uuid.New(), NetBoxDeviceID: &id, NetBoxSerial: fmt.Sprintf("SN-%d", id),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListMachinesByDeviceIDs([]int{10, 30, 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestListMachinesByIDs(t *testing.T) {
	st := testStore(t)
	var ids []uuid.UUID
	for i := range 3 {
		id := uuid.New()
		ids = append(ids, id)
		if err := st.CreateMachine(&model.Machine{
			ID: id, NetBoxSerial: fmt.Sprintf("SN-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Ask for two known IDs plus one that doesn't exist.
	got, err := st.ListMachinesByIDs([]uuid.UUID{ids[0], ids[2], uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// Empty input short-circuits without a query.
	empty, err := st.ListMachinesByIDs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("nil input: got %d, want 0", len(empty))
	}
}

func TestGetMachineNotFound(t *testing.T) {
	st := testStore(t)
	_, err := st.GetMachine(uuid.New())
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound, got %v", err)
	}
}

// SetMachineDesiredOS writes desired_os and nothing else, reports a missing
// machine as ErrMachineNotFound (instead of inserting it), and treats
// re-setting the current value as success.
func TestSetMachineDesiredOS(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-OS", State: model.MachineStateRunning, Locked: true, DesiredOS: "jammy"}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second pass re-sets the value it already holds
		if err := st.SetMachineDesiredOS(m.ID, "noble"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredOS != "noble" || got.State != model.MachineStateRunning || !got.Locked {
		t.Errorf("after SetMachineDesiredOS: desired_os=%q state=%q locked=%v, want noble/running/true", got.DesiredOS, got.State, got.Locked)
	}

	missing := uuid.New()
	if err := st.SetMachineDesiredOS(missing, "noble"); !errors.Is(err, ErrMachineNotFound) {
		t.Errorf("unknown machine: got %v, want ErrMachineNotFound", err)
	}
	if _, err := st.GetMachine(missing); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("SetMachineDesiredOS created a row for an unknown machine (GetMachine err=%v)", err)
	}
}

// SetMachinePXEEligible refuses to enable PXE on a locked machine and leaves
// the flag unchanged (docs/user/operations.md: "Locked machines cannot be
// PXE-enabled"; the API maps ErrMachineLocked to 409). Disabling stays
// allowed on a locked machine, an unlocked machine can be enabled
// repeatedly, and an unknown machine is ErrMachineNotFound (the API's 404)
// in both directions instead of a silent no-op.
func TestSetMachinePXEEligible(t *testing.T) {
	st := testStore(t)
	locked := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-locked-pxe", State: model.MachineStateRunning, Locked: true}
	if err := st.CreateMachine(locked); err != nil {
		t.Fatal(err)
	}

	if err := st.SetMachinePXEEligible(locked.ID, true); !errors.Is(err, ErrMachineLocked) {
		t.Errorf("pxe-enable on a locked machine returned %v; want ErrMachineLocked", err)
	}
	got, err := st.GetMachine(locked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PXEEligible {
		t.Errorf("locked, in-service machine is now PXE-eligible")
	}

	// Disabling is the safe direction and is not lock-gated. A deployed
	// machine is running, locked and still PXE-eligible.
	deployed := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-deployed-pxe", State: model.MachineStateRunning, Locked: true, PXEEligible: true}
	if err := st.CreateMachine(deployed); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMachinePXEEligible(deployed.ID, false); err != nil {
		t.Errorf("pxe-disable on a locked machine returned %v; want nil", err)
	}
	if got, err := st.GetMachine(deployed.ID); err != nil || got.PXEEligible {
		t.Errorf("after pxe-disable on a locked machine: pxe_eligible=%v err=%v, want false", got != nil && got.PXEEligible, err)
	}

	unlocked := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-unlocked-pxe", State: model.MachineStateCommissioned}
	if err := st.CreateMachine(unlocked); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second pass re-sets the value it already holds
		if err := st.SetMachinePXEEligible(unlocked.ID, true); err != nil {
			t.Fatalf("pxe-enable on an unlocked machine returned %v; want nil", err)
		}
	}
	if got, err := st.GetMachine(unlocked.ID); err != nil || !got.PXEEligible {
		t.Errorf("after pxe-enable on an unlocked machine: pxe_eligible=%v err=%v, want true", got != nil && got.PXEEligible, err)
	}

	missing := uuid.New()
	for _, eligible := range []bool{true, false} {
		if err := st.SetMachinePXEEligible(missing, eligible); !errors.Is(err, ErrMachineNotFound) {
			t.Errorf("SetMachinePXEEligible(unknown, %v) returned %v; want ErrMachineNotFound", eligible, err)
		}
	}
}

// SetMachineLocked reports an unknown machine as ErrMachineNotFound (the
// API's 404) instead of a silent no-op, and treats re-setting the current
// value as success.
func TestSetMachineLocked(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-lock", State: model.MachineStateRunning}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second pass re-sets the value it already holds
		if err := st.SetMachineLocked(m.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := st.GetMachine(m.ID); err != nil || !got.Locked {
		t.Errorf("after SetMachineLocked(true): locked=%v err=%v, want true", got != nil && got.Locked, err)
	}

	missing := uuid.New()
	for _, locked := range []bool{true, false} {
		if err := st.SetMachineLocked(missing, locked); !errors.Is(err, ErrMachineNotFound) {
			t.Errorf("SetMachineLocked(unknown, %v) returned %v; want ErrMachineNotFound", locked, err)
		}
	}
}

// TestMigrate_DropsMachineBMCCreds pins migration 00030: the machines
// table carries no BMC credential columns. The device's
// pop/<pop>/bmc-creds-admin entry is the only BMC credential, so a
// second copy in Postgres (which drifted from the BMC in prod) must not
// come back.
func TestMigrate_DropsMachineBMCCreds(t *testing.T) {
	st := testStore(t)
	var n int64
	if err := st.db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'machines' AND column_name IN ('bmc_user', 'bmc_pass')`).
		Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("machines still has %d BMC credential column(s); migration 00030 must drop bmc_user and bmc_pass", n)
	}
}

// TestWithPopAdvisoryLock_RunsCallback verifies the wrapper invokes
// the callback exactly once and reports its error.
func TestWithPopAdvisoryLock_RunsCallback(t *testing.T) {
	st := testStore(t)
	called := 0
	err := st.WithPopAdvisoryLock(t.Context(), "pop1", func(_ context.Context) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("callback called %d times, want 1", called)
	}

	// Errors propagate.
	sentinel := errors.New("boom")
	err = st.WithPopAdvisoryLock(t.Context(), "pop1", func(_ context.Context) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error propagation: got %v, want sentinel", err)
	}
}

// TestWithPopAdvisoryLock_SerializesSamePop verifies the lock
// actually serializes concurrent callers with the same pop slug.
// Two goroutines hold the lock; the second's callback must not
// start until the first's returns.
func TestWithPopAdvisoryLock_SerializesSamePop(t *testing.T) {
	st := testStore(t)

	firstStarted := make(chan struct{})
	firstHolding := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})

	go func() {
		_ = st.WithPopAdvisoryLock(t.Context(), "pop1", func(_ context.Context) error {
			close(firstStarted)
			close(firstHolding)
			<-firstRelease
			return nil
		})
	}()

	<-firstStarted

	go func() {
		_ = st.WithPopAdvisoryLock(t.Context(), "pop1", func(_ context.Context) error {
			close(secondStarted)
			return nil
		})
	}()

	// Give the second goroutine a window to (incorrectly) acquire
	// the lock if serialization is broken.
	select {
	case <-secondStarted:
		t.Fatal("second WithPopAdvisoryLock entered while first still held the lock")
	case <-time.After(200 * time.Millisecond):
	}

	close(firstRelease)

	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second WithPopAdvisoryLock never entered after first released")
	}
}

// TestWithPopAdvisoryLock_DifferentPopsRunInParallel confirms that
// callers with different pop slugs do not block each other —
// otherwise we'd serialize the entire fleet's monitor writes
// instead of just per-pop.
func TestWithPopAdvisoryLock_DifferentPopsRunInParallel(t *testing.T) {
	st := testStore(t)

	pop1Holding := make(chan struct{})
	pop2Started := make(chan struct{})
	pop1Release := make(chan struct{})

	go func() {
		_ = st.WithPopAdvisoryLock(t.Context(), "pop1", func(_ context.Context) error {
			close(pop1Holding)
			<-pop1Release
			return nil
		})
	}()
	<-pop1Holding

	go func() {
		_ = st.WithPopAdvisoryLock(t.Context(), "pop2", func(_ context.Context) error {
			close(pop2Started)
			return nil
		})
	}()

	select {
	case <-pop2Started:
	case <-time.After(2 * time.Second):
		t.Fatal("pop2 WithPopAdvisoryLock blocked behind pop1 (cross-pop serialization)")
	}

	close(pop1Release)
}
