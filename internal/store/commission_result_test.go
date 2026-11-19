package store

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// A byte-identical redelivery of a commission report (the worker outbox is
// at-least-once) is an accepted replay of the completed operation. The
// comparison runs against the result as the jsonb column stored it, and
// jsonb re-spells some numbers (1e3 is stored as 1000, -0 as 0). The replay
// must still be recognised as the same result, not rejected as "already
// completed with a different result".
func TestCommissionReplayWithExponentNumberIsAccepted(t *testing.T) {
	st := testStore(t)
	for _, tc := range []struct{ name, number string }{
		{"exponent", "1e3"},
		{"upper-case exponent", "1E3"},
		{"negative exponent", "1e-3"},
		{"negative zero", "-0"},
		{"Go float below 1e-6", "1e-7"},
		{"Go float at 1e21", "1e+21"},
		{"trailing fractional zero", "1.50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-replay-" + uuid.NewString(), State: model.MachineStateNew, PXEEligible: true}
			if err := st.CreateMachine(m); err != nil {
				t.Fatal(err)
			}
			op, err := st.CreateMachineOp(m.ID, model.OpCommission, nil, model.MachineStateNew)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
				t.Fatal(err)
			}
			report := json.RawMessage(`{"serial":"S1","mem_kb":` + tc.number + `}`)
			if _, _, err := st.CommitCommissionResult(op.ID, m.ID, report); err != nil {
				t.Fatalf("first delivery: %v", err)
			}

			already, err := st.ValidateOperationBinding(op.ID, m.ID, model.OpCommission, report)
			if err != nil || !already {
				t.Errorf("ValidateOperationBinding on an identical redelivery: alreadyCompleted=%v err=%v; want true, nil", already, err)
			}
			out, already, err := st.CommitCommissionResult(op.ID, m.ID, report)
			if err != nil || !already || out != nil {
				t.Errorf("CommitCommissionResult on an identical redelivery: outcome set=%v alreadyCompleted=%v err=%v; want false, true, nil", out != nil, already, err)
			}
		})
	}
}
