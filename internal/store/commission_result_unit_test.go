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

// Numbers compare by exact value, not by spelling: jsonb re-spells numbers,
// so the stored copy of a report may spell the same value differently from
// a byte-identical redelivery.
func TestJSONSemanticallyEqualComparesNumbersByValue(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`1e3`, `1000`, true},
		{`1E+3`, `1000`, true},
		{`1e-3`, `0.001`, true},
		{`10.0e-1`, `1`, true},
		{`1.50`, `1.5`, true},
		{`1.0`, `1`, true},
		{`-0`, `0`, true},
		{`-0.0e5`, `0`, true},
		{`1e-7`, `0.0000001`, true},
		{`1e+21`, `1000000000000000000000`, true},
		{`-2.5e2`, `-250`, true},
		{`{"a":[1e3,{"b":1e-3}]}`, `{"a":[1000,{"b":0.001}]}`, true},
		// An exponent beyond 32 bits is compared by spelling; Postgres
		// numeric cannot store one, so it never comes from the store.
		{`1e9999999999`, `1e9999999999`, true},

		{`1000`, `1001`, false},
		{`1e3`, `1e4`, false},
		{`0.1`, `0.01`, false},
		{`10`, `1`, false},
		{`1`, `-1`, false},
		{`1e3`, `"1000"`, false},
		{`{"a":[1e3]}`, `{"a":[1001]}`, false},
	} {
		if got := jsonSemanticallyEqual([]byte(tc.a), []byte(tc.b)); got != tc.want {
			t.Errorf("jsonSemanticallyEqual(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := jsonSemanticallyEqual([]byte(tc.b), []byte(tc.a)); got != tc.want {
			t.Errorf("jsonSemanticallyEqual(%s, %s) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}
