package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Property: when an op-create call is refused, its error is one an operator
// can act on. The API answers a refused create with HTTP 409 and the store's
// error text as the body (api.createOperation, setBootMachine,
// bmcReconcileMachine, the PDU outlet route), and slagctl prints that body,
// so the text of the store's error is the operator's message.
//
// The history is random op creation of every type the API creates
// (CreateMachineOp for deploy and commission; CreatePowerOp for power on,
// off and cycle, set_boot_device and bmc_reconcile) interleaved with claim,
// complete, fail, lock and unlock on two machines, so creates meet an
// active op of every other type.
//
// The reference model is a map from machine to its one active op. It does
// not model machine state or the lock: it decides only what it can decide
// from that map.
//
//   - A create on a machine the model holds an active op for must fail, and
//     the error must name that op's type and ID and say what to do next.
//   - A create that succeeds must be on a machine with no active op.
//   - No refusal, whatever its cause, carries database internals.

// conflictOpTypes are the op types the API creates and the store call each
// goes through. lifecycle types use CreateMachineOp, the rest CreatePowerOp.
var (
	conflictLifecycle = []model.OperationType{model.OpDeploy, model.OpCommission}
	conflictPowerPath = []model.OperationType{
		model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle,
		model.OpSetBootDevice, model.OpBMCReconcile,
	}
)

type conflictActive struct {
	id  uuid.UUID
	typ model.OperationType
}

// databaseInternals are fragments of a Postgres error. None belongs in a
// message for an operator.
var databaseInternals = []string{"SQLSTATE", "duplicate key", "idx_", "violates", "constraint"}

func checkNoDatabaseInternals(msg string) error {
	for _, frag := range databaseInternals {
		if strings.Contains(msg, frag) {
			return fmt.Errorf("refusal carries database internals (%q): %s", frag, msg)
		}
	}
	return nil
}

// checkConflictText is the contract for a create refused because active is
// in flight on the machine.
func checkConflictText(msg string, active conflictActive) error {
	if err := checkNoDatabaseInternals(msg); err != nil {
		return err
	}
	if !strings.Contains(msg, string(active.typ)) || !strings.Contains(msg, active.id.String()) {
		return fmt.Errorf("refusal does not name the active %s operation %s: %s", active.typ, active.id, msg)
	}
	if !strings.Contains(msg, "slagctl machine abort") {
		return fmt.Errorf("refusal does not say how to clear the active operation: %s", msg)
	}
	return nil
}

func TestOpCreateProp_RefusalIsActionable(t *testing.T) {
	st := testStore(t)
	cov := map[string]int{}
	t.Cleanup(func() { t.Logf("coverage: %v", cov) })

	rapid.Check(t, func(rt *rapid.T) {
		// Fresh machines per case; rows from earlier cases are inert (their
		// ops belong to other machine IDs).
		machines := make([]uuid.UUID, 2)
		for i := range machines {
			m := &model.Machine{
				ID: uuid.New(), NetBoxSerial: "SN-" + uuid.NewString(),
				State: model.MachineStateCommissioned, PXEEligible: true,
			}
			if err := st.CreateMachine(m); err != nil {
				rt.Fatalf("create machine: %v", err)
			}
			machines[i] = m.ID
		}
		active := map[uuid.UUID]*conflictActive{}

		steps := rapid.IntRange(1, 25).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			mid := machines[rapid.IntRange(0, len(machines)-1).Draw(rt, "machine")]
			cur := active[mid]
			switch rapid.SampledFrom([]string{
				"lifecycle", "lifecycle", "power", "power", "power",
				"claim", "complete", "fail", "unlock", "lock",
			}).Draw(rt, "action") {
			case "lifecycle":
				typ := rapid.SampledFrom(conflictLifecycle).Draw(rt, "type")
				op, err := st.CreateMachineOp(mid, typ, nil,
					model.MachineStateNew, model.MachineStateCommissioned,
					model.MachineStateFailed, model.MachineStateRunning)
				checkCreate(rt, cov, "lifecycle", typ, cur, op, err, active, mid)
			case "power":
				typ := rapid.SampledFrom(conflictPowerPath).Draw(rt, "type")
				op, err := st.CreatePowerOp(mid, typ, nil)
				checkCreate(rt, cov, "power", typ, cur, op, err, active, mid)
			case "claim":
				if cur == nil {
					continue
				}
				// A refused claim (locked machine, state not allowed) leaves
				// the op pending and active, which is all the model tracks.
				_, _ = st.ClaimOperation(cur.id, "w1")
			case "complete":
				if cur == nil {
					continue
				}
				target := model.MachineState("")
				switch cur.typ {
				case model.OpDeploy:
					target = model.MachineStateRunning
				case model.OpCommission:
					target = model.MachineStateCommissioned
				case model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle,
					model.OpSetBootDevice, model.OpBMCReconcile:
					// These change no machine state.
				}
				if _, err := st.CompleteOperationWithState(cur.id, target, nil); err != nil {
					rt.Fatalf("complete %s %s: %v", cur.typ, cur.id, err)
				}
				delete(active, mid)
			case "fail":
				if cur == nil {
					continue
				}
				if err := st.FailOperation(cur.id, []byte(`{"error":"generated"}`)); err != nil {
					rt.Fatalf("fail %s %s: %v", cur.typ, cur.id, err)
				}
				delete(active, mid)
			case "unlock":
				if err := st.SetMachineLocked(mid, false); err != nil {
					rt.Fatalf("unlock: %v", err)
				}
			case "lock":
				if err := st.SetMachineLocked(mid, true); err != nil {
					rt.Fatalf("lock: %v", err)
				}
			}
		}
	})
}

func checkCreate(rt *rapid.T, cov map[string]int, path string, typ model.OperationType,
	cur *conflictActive, op *model.Operation, err error, active map[uuid.UUID]*conflictActive, mid uuid.UUID,
) {
	switch {
	case err == nil && cur != nil:
		rt.Fatalf("created %s op %s while %s op %s is active on the machine", typ, op.ID, cur.typ, cur.id)
	case err == nil:
		active[mid] = &conflictActive{id: op.ID, typ: typ}
		cov["created "+path]++
	case cur != nil && path == "lifecycle" && lockOrStateRefusal(err.Error()):
		// CreateMachineOp checks the lock and the allowed source states
		// before it looks for an active op (its documented validation
		// order), so a claimed lifecycle op, which holds the machine in a
		// transient state, or a lock set meanwhile answers first.
		cov["refused lifecycle for lock or state while "+pathOf(cur.typ)+" active"]++
		if terr := checkNoDatabaseInternals(err.Error()); terr != nil {
			rt.Fatalf("create %s while %s is active: %v", typ, cur.typ, terr)
		}
	case cur != nil:
		cov[fmt.Sprintf("refused %s while %s active", path, pathOf(cur.typ))]++
		if terr := checkConflictText(err.Error(), *cur); terr != nil {
			rt.Fatalf("create %s while %s is active: %v", typ, cur.typ, terr)
		}
	default:
		// Refused with no active op: the lock or the machine state. The
		// model does not decide which; the text still has to be clean.
		cov["refused "+path+" for lock or state"]++
		if terr := checkNoDatabaseInternals(err.Error()); terr != nil {
			rt.Fatalf("create %s: %v", typ, terr)
		}
	}
}

// lockOrStateRefusal reports whether msg is one of CreateMachineOp's two
// refusals that precede its active-op check.
func lockOrStateRefusal(msg string) bool {
	return strings.HasPrefix(msg, "machine is locked") || strings.HasPrefix(msg, "machine is in state")
}

func pathOf(typ model.OperationType) string {
	switch typ {
	case model.OpDeploy, model.OpCommission:
		return "lifecycle"
	case model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle:
		return "power"
	default:
		return string(typ)
	}
}

// TestOpCreate_ConcurrentCreateRefusalIsActionable is the same contract for
// creates that race: every create checks for an active op and then inserts,
// so two requests issued together can both pass the check, and the loser is
// refused by the unique index instead. An operator who double-submits a
// deploy must get the same refusal as one who submits it a second later.
// The schedule is the scheduler's, so the test repeats the race; the
// invariants (one winner, every loser's refusal names the winner and says
// what to do) hold on every round whichever way a loser was refused.
func TestOpCreate_ConcurrentCreateRefusalIsActionable(t *testing.T) {
	st := testStore(t)
	const rounds, racers = 40, 6
	type result struct {
		op  *model.Operation
		err error
	}
	for round := 0; round < rounds; round++ {
		m := &model.Machine{
			ID: uuid.New(), NetBoxSerial: "SN-" + uuid.NewString(),
			State: model.MachineStateCommissioned, PXEEligible: true,
		}
		if err := st.CreateMachine(m); err != nil {
			t.Fatal(err)
		}
		results := make(chan result, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			lifecycle := i%2 == 0
			go func() {
				<-start
				var r result
				if lifecycle {
					r.op, r.err = st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateCommissioned)
				} else {
					r.op, r.err = st.CreatePowerOp(m.ID, model.OpPowerCycle, nil)
				}
				results <- r
			}()
		}
		close(start)
		var winners []*model.Operation
		var refusals []string
		for i := 0; i < racers; i++ {
			r := <-results
			if r.err == nil {
				winners = append(winners, r.op)
				continue
			}
			refusals = append(refusals, r.err.Error())
		}
		if len(winners) != 1 {
			t.Fatalf("round %d: %d creates succeeded on one machine, want exactly 1", round, len(winners))
		}
		// Nothing completes the winner, so it is still the active op when
		// each loser is refused, by the check or by the index.
		won := conflictActive{id: winners[0].ID, typ: winners[0].Type}
		for _, msg := range refusals {
			if terr := checkConflictText(msg, won); terr != nil {
				t.Fatalf("round %d: %v", round, terr)
			}
		}
	}
}
