package store

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestValidateOperationBindingRejectsCrossMachineAndWrongType(t *testing.T) {
	machineID, otherID := uuid.New(), uuid.New()
	op := &model.Operation{
		ID: uuid.New(), MachineID: otherID, Type: model.OpCommission, Status: model.OpRunning,
	}
	if _, err := validateOperationBinding(op, machineID, model.OpCommission, nil); err == nil {
		t.Fatal("expected cross-machine operation binding to be rejected")
	}
	op.MachineID = machineID
	op.Type = model.OpDeploy
	if _, err := validateOperationBinding(op, machineID, model.OpCommission, nil); err == nil {
		t.Fatal("expected wrong operation type to be rejected")
	}
}

func TestValidateOperationBindingCompletedReplayRequiresSameResult(t *testing.T) {
	machineID := uuid.New()
	op := &model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpCommission, Status: model.OpCompleted,
		Result: json.RawMessage(`{"system":{"uuid":"abc"},"nics":[]}`),
	}
	already, err := validateOperationBinding(
		op, machineID, model.OpCommission,
		json.RawMessage("{\n  \"nics\": [], \"system\": {\"uuid\": \"abc\"}}"),
	)
	if err != nil || !already {
		t.Fatalf("same semantic result replay = already %v err %v, want true nil", already, err)
	}
	if _, err := validateOperationBinding(
		op, machineID, model.OpCommission,
		json.RawMessage(`{"system":{"uuid":"different"}}`),
	); err == nil {
		t.Fatal("expected completed replay with different result to be rejected")
	}
}

func TestValidateOperationBindingAcceptsCompletedDeployReplay(t *testing.T) {
	machineID := uuid.New()
	op := &model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpCompleted,
	}
	already, err := validateOperationBinding(op, machineID, model.OpDeploy, nil)
	if err != nil || !already {
		t.Fatalf("completed deploy replay = already %v err %v, want true nil", already, err)
	}
}

func TestJSONSemanticallyEqualPreservesLargeIntegers(t *testing.T) {
	// These adjacent integers both round to the same float64. Replay
	// comparison must not collapse them and accept different inventory.
	a := []byte(`{"size":9007199254740992}`)
	b := []byte(`{"size":9007199254740993}`)
	if jsonSemanticallyEqual(a, b) {
		t.Fatal("distinct large JSON integers compared equal")
	}
}
