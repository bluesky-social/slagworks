package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// serialTrapController fails the test if the chassis-serial gate runs:
// AMT exposes no serial (power.ErrChassisSerialUnsupported), so the gate
// must be skipped on the AMT path exactly as on the PDU path.
type serialTrapController struct {
	mockPowerController
	t *testing.T
}

func (s *serialTrapController) ChassisSerial(context.Context) (string, error) {
	s.t.Error("chassis-serial gate ran on the AMT transport")
	return "", power.ErrChassisSerialUnsupported
}

const amtParams = `{"amt_runtime":{"host":"10.252.240.23","username":"admin","password":"pw"},` +
	`"bmc_runtime":{"expected_chassis_serial":"SN-MS01","extra_config":{}}}`

func amtExecutor(t *testing.T, m *model.MachineSnapshot, opType model.OperationType, params string, factory PowerControllerFactory) (*Executor, *Cache, model.Operation) {
	t.Helper()
	cache := NewCache(nil)
	addMachineToCache(cache, m)
	op := model.Operation{ID: uuid.New(), MachineID: m.ID, Type: opType, Status: model.OpPending, Params: json.RawMessage(params)}
	cache.opState.mu.Lock()
	cp := op
	cache.opState.ops[op.ID.String()] = &cp
	cache.opState.mu.Unlock()
	return &Executor{cache: cache, factory: factory, workerID: "w", log: testLogger()}, cache, op
}

func opResult(c *Cache, id uuid.UUID) model.Operation {
	c.opState.mu.RLock()
	defer c.opState.mu.RUnlock()
	return *c.opState.ops[id.String()]
}

// TestExecutor_AMT_PowerOpRoutesAndSkipsSerialGate: an amt_runtime op hands
// the envelope to the factory and completes without the chassis-serial gate,
// even though the op also carries an expected serial.
func TestExecutor_AMT_PowerOpRoutesAndSkipsSerialGate(t *testing.T) {
	ctrl := &serialTrapController{t: t}
	var got *model.AMTRuntime
	exec, cache, op := amtExecutor(t,
		&model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true},
		model.OpPowerCycle, amtParams,
		func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
			if pduRT != nil {
				t.Error("pduRT must be nil on the AMT path")
			}
			got = amtRT
			return ctrl, nil
		})
	exec.runPowerOp(context.Background(), op)
	if got == nil || got.Host != "10.252.240.23" || got.Username != "admin" || got.Password != "pw" {
		t.Fatalf("factory amtRT = %+v, want the op's envelope", got)
	}
	if ctrl.lastAction != "cycle" {
		t.Errorf("action = %q, want cycle", ctrl.lastAction)
	}
	if st := opResult(cache, op.ID).Status; st != model.OpCompleted {
		t.Fatalf("status = %s, want completed", st)
	}
}

// TestExecutor_AMT_LifecycleArmsPXEAndCycles: commission/deploy over AMT arms
// the one-time PXE override, then cycles, with no serial gate.
func TestExecutor_AMT_LifecycleArmsPXEAndCycles(t *testing.T) {
	ctrl := &serialTrapController{t: t}
	exec, cache, op := amtExecutor(t,
		&model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true},
		model.OpCommission, amtParams,
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return ctrl, nil
		})
	exec.runLifecycleOp(context.Background(), op)
	if ctrl.lastBootDevice != power.BootPXE {
		t.Errorf("boot device = %q, want pxe", ctrl.lastBootDevice)
	}
	if ctrl.lastAction != "cycle" {
		t.Errorf("action = %q, want cycle", ctrl.lastAction)
	}
	if st := opResult(cache, op.ID).Status; st != model.OpRunning {
		t.Fatalf("status = %s, want running (awaiting agent)", st)
	}
}

// TestExecutor_AMT_RefusesBMCPathForAMTMachine is the worker-side D1/D2
// backstop: an AMT machine whose op carries no amt_runtime (a stale or
// hand-built op) fails without the factory ever being asked to build an
// IPMI/Redfish controller against the AMT endpoint.
func TestExecutor_AMT_RefusesBMCPathForAMTMachine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opType model.OperationType
		run    func(*Executor, context.Context, model.Operation)
	}{
		{"power", model.OpPowerOn, (*Executor).runPowerOp},
		{"lifecycle", model.OpDeploy, (*Executor).runLifecycleOp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec, cache, op := amtExecutor(t,
				&model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true},
				tc.opType, `{"bmc_runtime":{"expected_chassis_serial":"SN","username":"admin","password":"pw","extra_config":{}}}`,
				func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
					t.Error("factory called for an AMT machine without amt_runtime")
					return &mockPowerController{}, nil
				})
			tc.run(exec, context.Background(), op)
			if st := opResult(cache, op.ID).Status; st != model.OpFailed {
				t.Fatalf("status = %s, want failed", st)
			}
		})
	}
}

func TestExecutor_AMT_RejectsBothEnvelopes(t *testing.T) {
	exec, cache, op := amtExecutor(t,
		&model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true},
		model.OpPowerOn,
		`{"amt_runtime":{"host":"10.0.0.1","username":"a","password":"b"},"pdu_runtime":{"outlets":[{"pdu_host":"10.0.0.2","outlet":1}]}}`,
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			t.Error("factory called for an op carrying both transport envelopes")
			return &mockPowerController{}, nil
		})
	exec.runPowerOp(context.Background(), op)
	if st := opResult(cache, op.ID).Status; st != model.OpFailed {
		t.Fatalf("status = %s, want failed", st)
	}
}
