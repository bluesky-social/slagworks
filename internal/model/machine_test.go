package model

import (
	"testing"

	"github.com/google/uuid"
)

func TestMachineHasNewFields(t *testing.T) {
	nb := 42
	m := Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &nb,
		NetBoxSerial:   "SN123",
		State:          MachineStateNew,
	}
	if m.NetBoxSerial == "" {
		t.Fatal("NetBoxSerial must be populated")
	}
	if m.NetBoxDeviceID == nil || *m.NetBoxDeviceID != 42 {
		t.Fatal("NetBoxDeviceID must be *int and equal 42")
	}
}

func TestMachineStatesAreValid(t *testing.T) {
	for _, s := range []MachineState{
		MachineStateNew,
		MachineStateCommissioning,
		MachineStateCommissioned,
		MachineStateDeploying,
		MachineStateRunning,
		MachineStateFailed,
	} {
		if !s.Valid() {
			t.Errorf("%s must be valid", s)
		}
	}
	if MachineState("ready").Valid() {
		t.Error("dropped state `ready` must NOT be valid")
	}
	if MachineState("deployed").Valid() {
		t.Error("dropped state `deployed` must NOT be valid")
	}
	if MachineState("decommissioned").Valid() {
		t.Error("dropped state `decommissioned` must NOT be valid")
	}
}
