package worker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// opTimeline records, in order, what global is sent about one op and what
// the executor asks of the power controller. Each controller call first
// drains the cache outbox (the worker's sender does the same concurrently
// in production), so a phase queued before a call lands before it here.
type opTimeline struct {
	cache  *Cache
	opID   string
	events []string
}

func (tl *opTimeline) drain() {
	_, updates := takeOutbox(tl.cache)
	for _, u := range updates {
		if u.OperationID != tl.opID {
			continue
		}
		if u.Phase != "" {
			tl.events = append(tl.events, "phase "+u.Phase)
		}
		if u.Status != "" {
			tl.events = append(tl.events, "status "+u.Status)
		}
	}
}

func (tl *opTimeline) call(name string) {
	tl.drain()
	tl.events = append(tl.events, name)
}

func (tl *opTimeline) index(event string) int {
	return slices.Index(tl.events, event)
}

// timelineController is a power.Controller (and power.BootManager) that
// logs every call on an opTimeline and succeeds. PowerStatus reports On, so
// a lifecycle op takes the PowerCycle branch.
type timelineController struct{ tl *opTimeline }

func (c timelineController) PowerOn(context.Context) error  { c.tl.call("PowerOn"); return nil }
func (c timelineController) PowerOff(context.Context) error { c.tl.call("PowerOff"); return nil }
func (c timelineController) PowerCycle(context.Context) error {
	c.tl.call("PowerCycle")
	return nil
}
func (c timelineController) PowerStatus(context.Context) (power.State, error) {
	c.tl.call("PowerStatus")
	return power.StateOn, nil
}
func (c timelineController) SetBootDevice(context.Context, power.BootDevice) error {
	c.tl.call("SetBootDevice")
	return nil
}
func (c timelineController) ChassisSerial(context.Context) (string, error) {
	c.tl.call("ChassisSerial")
	return "", nil
}
func (c timelineController) BootStatus(context.Context) (power.BootOverride, error) {
	c.tl.call("BootStatus")
	return power.BootOverride{}, nil
}
func (c timelineController) SetBootOverride(context.Context, power.BootDevice, bool) error {
	c.tl.call("SetBootOverride")
	return nil
}
func (c timelineController) Transport() string           { return "test" }
func (c timelineController) Close(context.Context) error { return nil }

// TestExecutor_RecordsPhaseBeforeFirstControllerCall pins the contract the
// orphaned-claim sweep relies on: after winning the claim, the executor's
// first act is to record a phase, before it builds the power controller or
// talks to the BMC/PDU/AMT endpoint. Global's ExpireOrphanedClaims fails any
// op still claimed with an empty phase 2 minutes after the claim, as
// "claiming worker disconnected before execution started". A slow but live
// actuation (a sick BMC at up to 30s per call, a PDU cycle polling for up
// to 150s) must not look like that.
//
// Lifecycle ops record power_cycle first and pxe_booting only once the
// cycle has returned; power ops record their op type as the phase, the
// same way bmc_reconcile does.
func TestExecutor_RecordsPhaseBeforeFirstControllerCall(t *testing.T) {
	cases := []struct {
		opType    model.OperationType
		params    string
		phase     string // first phase global must see
		actuation string // the controller call that does the work
	}{
		{model.OpDeploy, "", "power_cycle", "PowerCycle"},
		{model.OpCommission, "", "power_cycle", "PowerCycle"},
		{model.OpPowerOn, "", "power_on", "PowerOn"},
		{model.OpPowerOff, "", "power_off", "PowerOff"},
		{model.OpPowerCycle, "", "power_cycle", "PowerCycle"},
		{model.OpSetBootDevice, `{"device":"pxe"}`, "set_boot_device", "SetBootOverride"},
	}
	for _, tc := range cases {
		t.Run(string(tc.opType), func(t *testing.T) {
			cache := NewCache(nil)
			machineID := uuid.New()
			addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
			op := model.Operation{ID: uuid.New(), MachineID: machineID, Type: tc.opType, Status: model.OpPending}
			if tc.params != "" {
				op.Params = json.RawMessage(tc.params)
			}
			seedOpsResync(cache, op)

			tl := &opTimeline{cache: cache, opID: op.ID.String()}
			claimed := op
			claimed.Status = model.OpClaimed
			exec := NewExecutor(ExecutorConfig{
				Cache: cache,
				Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
					tl.call("factory")
					return timelineController{tl: tl}, nil
				},
				Claimer:  &fakeClaimer{resultOp: &claimed},
				WorkerID: "test-worker",
			})
			exec.SetLogger(testLogger())
			exec.processOps(context.Background())
			tl.drain()

			if len(tl.events) == 0 || tl.events[0] != "phase "+tc.phase {
				t.Fatalf("timeline %q: want %q before any controller call; without it the op sits claimed with no phase "+
					"for the whole actuation, which the 2-minute orphaned-claim sweep fails as a dead worker", tl.events, "phase "+tc.phase)
			}
			act := tl.index(tc.actuation)
			if act < 0 {
				t.Fatalf("timeline %q: %s was never called", tl.events, tc.actuation)
			}
			switch tc.opType {
			case model.OpDeploy, model.OpCommission:
				if pxe := tl.index("phase pxe_booting"); pxe < act {
					t.Errorf("timeline %q: want phase pxe_booting after %s returns", tl.events, tc.actuation)
				}
			default:
				if last := tl.events[len(tl.events)-1]; last != "status completed" {
					t.Errorf("timeline %q: want the op to end completed, got %q", tl.events, last)
				}
			}
		})
	}
}
