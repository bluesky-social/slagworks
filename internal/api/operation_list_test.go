package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestEnrichOperationMachineNames_SerialFallback: with NetBox unwired (as in
// opFlowServer), enrichment falls back to the machine's NetBox serial so the
// MACHINE column still renders something human-readable instead of a UUID.
func TestEnrichOperationMachineNames_SerialFallback(t *testing.T) {
	s, st, _, _ := opFlowServer(t)

	// Two ops on machine A, one on machine B, plus one op whose machine
	// doesn't exist (MachineName must stay empty, not error).
	idA := seedMachine(t, st, model.MachineStateRunning)
	idB := seedMachine(t, st, model.MachineStateRunning)
	mA, err := st.GetMachine(idA)
	if err != nil {
		t.Fatal(err)
	}
	mB, err := st.GetMachine(idB)
	if err != nil {
		t.Fatal(err)
	}
	ghost := uuid.New()

	ops := []model.Operation{
		{ID: uuid.New(), MachineID: idA, Type: model.OpDeploy, Status: model.OpRunning},
		{ID: uuid.New(), MachineID: idA, Type: model.OpPowerCycle, Status: model.OpCompleted},
		{ID: uuid.New(), MachineID: idB, Type: model.OpDeploy, Status: model.OpFailed},
		{ID: uuid.New(), MachineID: ghost, Type: model.OpDeploy, Status: model.OpFailed},
	}
	if err := s.enrichOperationMachineNames(context.Background(), ops); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	if ops[0].MachineName != mA.NetBoxSerial {
		t.Errorf("op0 MachineName = %q, want serial %q", ops[0].MachineName, mA.NetBoxSerial)
	}
	if ops[1].MachineName != mA.NetBoxSerial {
		t.Errorf("op1 MachineName = %q, want serial %q", ops[1].MachineName, mA.NetBoxSerial)
	}
	if ops[2].MachineName != mB.NetBoxSerial {
		t.Errorf("op2 MachineName = %q, want serial %q", ops[2].MachineName, mB.NetBoxSerial)
	}
	if ops[3].MachineName != "" {
		t.Errorf("op3 (ghost machine) MachineName = %q, want empty", ops[3].MachineName)
	}
}

// TestEnrichOperationMachineNames_Empty: no ops is a no-op (no query, no error).
func TestEnrichOperationMachineNames_Empty(t *testing.T) {
	s, _, _, _ := opFlowServer(t)
	if err := s.enrichOperationMachineNames(context.Background(), nil); err != nil {
		t.Fatalf("enrich nil: %v", err)
	}
}
