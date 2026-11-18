package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func amtStatusConn(t *testing.T, machineID uuid.UUID, factory PowerControllerFactory) *WSConn {
	t.Helper()
	cache := NewCache(nil)
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.252.240.23", AMT: true})
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)
	w.SetLogger(testLogger())
	w.powerFactory = factory
	return w
}

// TestRunPowerStatus_AMT: an amt_runtime in the request reaches the factory
// and the AMT controller's answer comes back with transport "amt".
func TestRunPowerStatus_AMT(t *testing.T) {
	machineID := uuid.New()
	var got *model.AMTRuntime
	w := amtStatusConn(t, machineID,
		func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
			got = amtRT
			return &statusController{state: power.StateOn, transport: "amt"}, nil
		})
	data, err := json.Marshal(ws.PowerStatusRequestMsg{
		MachineID:  machineID,
		AMTRuntime: json.RawMessage(`{"host":"10.252.240.23","username":"admin","password":"pw"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := w.runPowerStatus(context.Background(), data)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.PowerStatusResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "on" || out.Transport != "amt" {
		t.Errorf("got %+v, want {on amt}", out)
	}
	if got == nil || got.Host != "10.252.240.23" || got.Username != "admin" {
		t.Errorf("amt_runtime did not reach the factory: %+v", got)
	}
}

// TestRunPowerStatus_AMTMachineWithoutAMTRuntime: a bmc-shaped request for an
// AMT machine is refused without building a controller (never read AMT as
// IPMI/Redfish).
func TestRunPowerStatus_AMTMachineWithoutAMTRuntime(t *testing.T) {
	machineID := uuid.New()
	w := amtStatusConn(t, machineID,
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			t.Error("factory called for an AMT machine without amt_runtime")
			return &statusController{}, nil
		})
	resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{"username":"admin","password":"pw"}`))
	if !strings.Contains(resp.Error, "Intel AMT") {
		t.Fatalf("error = %q, want the AMT refusal", resp.Error)
	}
}

// TestRunBootStatus_AMTUnsupported: boot status on an AMT machine answers
// Supported:false without connecting.
func TestRunBootStatus_AMTUnsupported(t *testing.T) {
	machineID := uuid.New()
	w := amtStatusConn(t, machineID,
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			t.Error("factory called for a boot-status read on an AMT machine")
			return &statusController{}, nil
		})
	data, err := json.Marshal(ws.BootStatusRequestMsg{MachineID: machineID})
	if err != nil {
		t.Fatal(err)
	}
	resp := w.runBootStatus(context.Background(), data)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.BootStatusResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Supported || out.Transport != "amt" {
		t.Errorf("got %+v, want Supported=false transport=amt", out)
	}
}
