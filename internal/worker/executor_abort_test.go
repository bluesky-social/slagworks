package worker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// callLog records, in order, every call the executor makes to a host's
// power controller and to the bmc_reconcile passes, and runs during(name)
// once that call is under way. A test uses it to apply a stream event while
// the executor is inside a BMC round trip, which is where the WS read loop,
// a separate goroutine in production, can land it.
type callLog struct {
	calls  []string
	during func(name string)
}

func (l *callLog) call(name string) {
	l.calls = append(l.calls, name)
	if l.during != nil {
		l.during(name)
	}
}

// loggingCtrl is a BMC power controller (with the BootManager capability,
// so set_boot_device ops run) whose every call goes to a callLog. Its
// chassis serial matches the expected serial in abortTestParams, so the
// serial gate passes.
type loggingCtrl struct{ log *callLog }

func (c loggingCtrl) PowerOn(context.Context) error    { c.log.call("PowerOn"); return nil }
func (c loggingCtrl) PowerOff(context.Context) error   { c.log.call("PowerOff"); return nil }
func (c loggingCtrl) PowerCycle(context.Context) error { c.log.call("PowerCycle"); return nil }
func (c loggingCtrl) PowerStatus(context.Context) (power.State, error) {
	c.log.call("PowerStatus")
	return power.StateOn, nil
}
func (c loggingCtrl) SetBootDevice(context.Context, power.BootDevice) error {
	c.log.call("SetBootDevice")
	return nil
}
func (c loggingCtrl) ChassisSerial(context.Context) (string, error) {
	c.log.call("ChassisSerial")
	return "SN1", nil
}
func (c loggingCtrl) Transport() string           { return "test" }
func (c loggingCtrl) Close(context.Context) error { return nil }
func (c loggingCtrl) BootStatus(context.Context) (power.BootOverride, error) {
	return power.BootOverride{}, nil
}
func (c loggingCtrl) SetBootOverride(context.Context, power.BootDevice, bool) error {
	c.log.call("SetBootOverride")
	return nil
}

// hostWrites are the calls that change a host or its BMC. ChassisSerial
// and PowerStatus are reads.
var hostWrites = []string{"SetBootDevice", "SetBootOverride", "PowerOn", "PowerOff", "PowerCycle", "Reconcile", "TimePass"}

func abortTestParams(t *testing.T, p model.OpParams) json.RawMessage {
	t.Helper()
	p.BMCRuntime = &model.BMCRuntime{ExpectedChassisSerial: "SN1", Username: "ADMIN", Password: testAdminPW}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestExecutor_OpRemovedMidRun_NoFurtherHostWrites: once the worker has
// applied op_removed for the op its executor is driving, the executor makes
// no further write to the host or its BMC and reports nothing for the op.
//
// The op_removed lands while the executor is inside a BMC round trip after
// the claim: the operator runs `slagctl machine abort` (or a NetBox drift
// fail or a timeout sweep fails the op), global publishes op_removed and
// the WS read loop, a separate goroutine, applies it with
// Cache.ApplyOpRemoved. Global already holds the terminal row, so the
// executor must not go on to arm PXE, power the host or rewrite its BMC
// users. An op_removed that lands after a write cannot undo it; the test
// pins only that nothing follows the removal.
//
// Each case also runs with op_removed for another op landing at the same
// moment, which must not stop this one: that run issues next, the write the
// removal is meant to prevent.
func TestExecutor_OpRemovedMidRun_NoFurtherHostWrites(t *testing.T) {
	cases := []struct {
		name   string
		opType model.OperationType
		params model.OpParams
		during string // the call during which the op_removed lands
		next   string // the write that follows when the op stays
	}{
		{"deploy aborted while reading power state", model.OpDeploy, model.OpParams{}, "PowerStatus", "PowerCycle"},
		{"deploy aborted during the serial gate", model.OpDeploy, model.OpParams{}, "ChassisSerial", "SetBootDevice"},
		{"commission aborted while arming PXE", model.OpCommission, model.OpParams{}, "SetBootDevice", "PowerCycle"},
		{"power cycle aborted while arming the boot device", model.OpPowerCycle, model.OpParams{BootDevice: "pxe"}, "SetBootDevice", "PowerCycle"},
		{"power off aborted during the serial gate", model.OpPowerOff, model.OpParams{}, "ChassisSerial", "PowerOff"},
		{"set_boot_device aborted during the serial gate", model.OpSetBootDevice, model.OpParams{Device: "pxe"}, "ChassisSerial", "SetBootOverride"},
		{"bmc_reconcile aborted during the serial gate", model.OpBMCReconcile, model.OpParams{}, "ChassisSerial", "Reconcile"},
		{"bmc_reconcile aborted during the account pass", model.OpBMCReconcile, model.OpParams{}, "Reconcile", "TimePass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, thisOp := range []bool{true, false} {
				cache := NewCache(nil)
				machineID := uuid.New()
				cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
					ID: machineID, BMCIP: "10.0.0.41", State: model.MachineStateRunning, PXEEligible: true,
				}}})
				setOOBConfig(cache, testOOB())
				op := model.Operation{
					ID: uuid.New(), MachineID: machineID, Type: tc.opType, Status: model.OpPending,
					Version: 1, Params: abortTestParams(t, tc.params),
				}
				seedOpsResync(cache, op)
				removedID := op.ID
				if !thisOp {
					removedID = uuid.New()
				}

				log := &callLog{}
				removed := false
				log.during = func(name string) {
					if name != tc.during || removed {
						return
					}
					removed = true
					cache.ApplyOpRemoved(removedID.String(), machineID.String(), op.Version+1, model.MachineStateFailed)
					// Drop what was queued before the removal: only reports
					// queued after it are under test.
					takeOutbox(cache)
				}
				exec := NewExecutor(ExecutorConfig{
					Cache: cache,
					Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
						return loggingCtrl{log: log}, nil
					},
					BMCReconcile: func(context.Context, bmcreconcile.LANTarget, string, []string) (*bmcreconcile.Result, error) {
						log.call("Reconcile")
						return monitorResult(), nil
					},
					BMCTime: func(context.Context, bmcreconcile.TimeTarget) bmcreconcile.TimeResult {
						log.call("TimePass")
						return bmcreconcile.TimeResult{}
					},
					Claimer:  &fakeClaimer{resultOp: &model.Operation{ID: op.ID, Status: model.OpClaimed}},
					WorkerID: "worker-1",
				})
				exec.SetLogger(testLogger())

				exec.processOps(context.Background())

				if !removed {
					t.Fatalf("the executor never reached %s (calls: %v)", tc.during, log.calls)
				}
				after := log.calls[slices.Index(log.calls, tc.during)+1:]
				if !thisOp {
					if !slices.Contains(after, tc.next) {
						t.Errorf("op_removed for another op stopped this one: no %s after %s (calls: %v)", tc.next, tc.during, log.calls)
					}
					continue
				}
				for _, c := range after {
					if slices.Contains(hostWrites, c) {
						t.Errorf("the executor issued %s after it had applied op_removed for the op during %s (calls: %v)", c, tc.during, log.calls)
					}
				}
				obs, updates := takeOutbox(cache)
				if len(obs) != 0 || len(updates) != 0 {
					t.Errorf("the executor reported on an op global had already ended: %d observations, op_updates %+v", len(obs), updates)
				}
			}
		})
	}
}
