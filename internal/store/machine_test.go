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

func TestDeleteMachine(t *testing.T) {
	st := testStore(t)
	nb := 42
	m := &model.Machine{ID: uuid.New(), NetBoxDeviceID: &nb, NetBoxSerial: "SN-D", State: model.MachineStateRunning}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteMachine(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachine(m.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected gorm.ErrRecordNotFound after delete, got %v", err)
	}
}

// TestDeleteMachine_CascadesOperationsAndLogs verifies DeleteMachine
// cleans up child rows that don't have ON DELETE CASCADE on their
// FK. Without this cleanup, the FK constraint on operations.machine_id
// would block the delete entirely.
func TestDeleteMachine_CascadesOperationsAndLogs(t *testing.T) {
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

	if err := st.DeleteMachine(m.ID); err != nil {
		t.Fatalf("delete with child op should cascade, got: %v", err)
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
