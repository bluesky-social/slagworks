package mirror

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestMain tears down the shared Postgres container that testpg starts for
// the database-backed tests in this package.
func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}

// opCreatedAfterIdleCheck is the real store with one addition: the first
// time the mirror asks it to fail the machine's in-flight ops, afterCheck
// runs right after that call has returned (having found nothing in flight).
// It stands for an operator request that commits between the mirror's
// "anything in flight?" check and its delete.
type opCreatedAfterIdleCheck struct {
	*store.Store
	afterCheck func()
	fired      bool
}

func (s *opCreatedAfterIdleCheck) FailInFlightOpsForMachine(id uuid.UUID, reason string) ([]store.OpOutcome, error) {
	outs, err := s.Store.FailInFlightOpsForMachine(id, reason)
	if !s.fired {
		s.fired = true
		s.afterCheck()
	}
	return outs, err
}

// OnDeviceDeleted's contract (B7): a NetBox delete that lands while the
// machine has an operation in flight fails the operation and KEEPS the row
// and its op history for the operator; only an idle machine is deleted. An
// op created after the mirror's in-flight check but before its delete must
// get the same treatment, not be hard-deleted along with the machine: the
// operator's request was acknowledged with that op, and the workers that
// received its op_delta need an op_removed to drop it.
func TestOnDeviceDeleted_KeepsAMachineWhoseOpWasCreatedMidDelete(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	dev := 9001
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-delete-race", NetBoxDeviceID: &dev,
		State: model.MachineStateCommissioned, PXEEligible: true}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	var created *model.Operation
	racing := &opCreatedAfterIdleCheck{Store: st, afterCheck: func() {
		op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateCommissioned)
		if err != nil {
			t.Errorf("mid-delete deploy request: %v", err)
			return
		}
		created = op
	}}
	pub := &fakePublisher{}
	mir := New(racing, &fakeInvalidator{})
	mir.SetPublisher(pub)

	err := mir.OnDeviceDeleted(context.Background(), dev, "pop-a")
	if created == nil {
		t.Fatal("setup: the deploy op was not created")
	}

	op, opErr := st.GetOperation(created.ID)
	if opErr != nil {
		t.Fatalf("deploy op %s was acknowledged to the operator and then hard-deleted with its machine (OnDeviceDeleted err=%v, GetOperation err=%v)",
			created.ID, err, opErr)
	}
	if err != nil {
		t.Fatalf("OnDeviceDeleted: %v", err)
	}
	if op.Status != model.OpFailed {
		t.Errorf("op survived but is %s; want failed (netbox device deleted mid-operation)", op.Status)
	}
	if _, mErr := st.GetMachine(m.ID); mErr != nil {
		t.Errorf("machine row was deleted while it had an operation: %v", mErr)
	}
	if pub.removedCount() != 1 || pub.removed[0].ID != created.ID {
		t.Errorf("op_removed publishes = %v; want exactly one, for op %s", pub.removed, created.ID)
	}
}
