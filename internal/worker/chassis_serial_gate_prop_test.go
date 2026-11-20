package worker

// Property test for the chassis-serial identity gate: across every op
// type that drives a BMC, every transport envelope, and every serial
// outcome, the controller's ChassisSerial is read before any actuation on
// the BMC path, and a mismatch or read failure actuates nothing (including
// the bmc_reconcile account pass). The oracle restates the documented gate
// (verifyChassisSerial's doc) independently.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

type serialGateCtrl struct {
	serial    string
	serialErr error
	calls     []string
}

func (g *serialGateCtrl) PowerOn(context.Context) error { g.calls = append(g.calls, "on"); return nil }
func (g *serialGateCtrl) PowerOff(context.Context) error {
	g.calls = append(g.calls, "off")
	return nil
}
func (g *serialGateCtrl) PowerCycle(context.Context) error {
	g.calls = append(g.calls, "cycle")
	return nil
}
func (g *serialGateCtrl) PowerStatus(context.Context) (power.State, error) {
	g.calls = append(g.calls, "status")
	return power.StateOn, nil
}
func (g *serialGateCtrl) Transport() string { return "gate" }
func (g *serialGateCtrl) SetBootDevice(_ context.Context, d power.BootDevice) error {
	g.calls = append(g.calls, "boot:"+string(d))
	return nil
}
func (g *serialGateCtrl) ChassisSerial(context.Context) (string, error) {
	g.calls = append(g.calls, "serial")
	return g.serial, g.serialErr
}
func (g *serialGateCtrl) Close(context.Context) error { return nil }
func (g *serialGateCtrl) BootStatus(context.Context) (power.BootOverride, error) {
	return power.BootOverride{}, nil
}
func (g *serialGateCtrl) SetBootOverride(_ context.Context, d power.BootDevice, _ bool) error {
	g.calls = append(g.calls, "override:"+string(d))
	return nil
}

func serialGateActuation(c string) bool {
	return c == "on" || c == "off" || c == "cycle" || c == "reconcile" ||
		strings.HasPrefix(c, "boot:") || strings.HasPrefix(c, "override:")
}

func TestProp_ChassisSerialGate(t *testing.T) {
	var refused, proceeded, pduRuns atomic.Int64
	rapid.Check(t, func(rt *rapid.T) {
		opType := rapid.SampledFrom([]model.OperationType{
			model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle, model.OpSetBootDevice,
			model.OpCommission, model.OpDeploy, model.OpBMCReconcile,
		}).Draw(rt, "op")
		viaPDU := opType != model.OpBMCReconcile && rapid.IntRange(0, 3).Draw(rt, "pdu") == 0
		expected := rapid.SampledFrom([]string{"", "SN-ALPHA1"}).Draw(rt, "expected")
		outcome := rapid.SampledFrom([]string{"match", "match-case", "match-space", "mismatch", "prefix", "empty", "error"}).Draw(rt, "outcome")
		ctrl := &serialGateCtrl{}
		switch outcome {
		case "match":
			ctrl.serial = "SN-ALPHA1"
		case "match-case":
			ctrl.serial = "sn-alpha1"
		case "match-space":
			ctrl.serial = "  SN-ALPHA1\n"
		case "mismatch":
			ctrl.serial = "SN-BRAVO2"
		case "prefix":
			ctrl.serial = "SN-ALPHA"
		case "empty":
			ctrl.serial = ""
		case "error":
			ctrl.serialErr = errors.New("inventory: HTTP 500")
		}

		params := model.OpParams{BMCRuntime: &model.BMCRuntime{
			ExpectedChassisSerial: expected, Username: "ADMIN", Password: "pw", ExtraConfig: map[string]any{},
		}}
		if viaPDU {
			params.PDURuntime = &model.PDURuntime{Outlets: []model.PDUOutlet{{PDUHost: "10.0.0.1", Outlet: 3, Username: "apc", Password: "pw"}}}
		}
		if opType == model.OpSetBootDevice {
			params.Device = "pxe"
		}
		raw, err := json.Marshal(params)
		if err != nil {
			rt.Fatal(err)
		}
		m := &model.MachineSnapshot{ID: uuid.New(), BMCIP: "192.0.2.9"}
		cache := NewCache(nil)
		addMachineToCache(cache, m)
		op := model.Operation{ID: uuid.New(), MachineID: m.ID, Type: opType, Status: model.OpPending, Params: raw}
		cache.opState.mu.Lock()
		cp := op
		cache.opState.ops[op.ID.String()] = &cp
		cache.opState.mu.Unlock()
		exec := &Executor{
			cache: cache, workerID: "w", log: testLogger(),
			factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
				return ctrl, nil
			},
			bmcReconcile: func(context.Context, bmcreconcile.LANTarget, string) (*bmcreconcile.Result, error) {
				ctrl.calls = append(ctrl.calls, "reconcile")
				return nil, errors.New("stop after the gate")
			},
			bmcTime: func(context.Context, bmcreconcile.TimeTarget) bmcreconcile.TimeResult {
				return bmcreconcile.TimeResult{}
			},
		}
		switch opType {
		case model.OpCommission, model.OpDeploy:
			exec.runLifecycleOp(context.Background(), op)
		case model.OpBMCReconcile:
			exec.runBMCReconcile(context.Background(), op)
		default:
			exec.runPowerOp(context.Background(), op)
		}

		label := fmt.Sprintf("op=%s pdu=%v expected=%q outcome=%s calls=%v", opType, viaPDU, expected, outcome, ctrl.calls)
		firstAct, serialAt := -1, -1
		for i, c := range ctrl.calls {
			if c == "serial" && serialAt < 0 {
				serialAt = i
			}
			if serialGateActuation(c) && firstAct < 0 {
				firstAct = i
			}
		}
		if viaPDU {
			pduRuns.Add(1)
			if serialAt >= 0 {
				rt.Fatalf("%s: the BMC serial gate ran on the PDU transport", label)
			}
			return
		}
		// Documented gate: skip when nothing is expected; proceed on an empty
		// read; refuse on a read error or a serial that differs after
		// trimming and case folding.
		wantRefuse := expected != "" && (outcome == "mismatch" || outcome == "prefix" || outcome == "error")
		if expected != "" && (serialAt < 0 || (firstAct >= 0 && firstAct < serialAt)) {
			rt.Fatalf("%s: actuation before (or without) the serial read", label)
		}
		if wantRefuse {
			refused.Add(1)
			if firstAct >= 0 {
				rt.Fatalf("%s: serial gate should have refused, but the op actuated", label)
			}
			if st := opResult(cache, op.ID).Status; st != model.OpFailed {
				rt.Fatalf("%s: refused op status %s, want failed", label, st)
			}
		} else {
			proceeded.Add(1)
			if firstAct < 0 {
				rt.Fatalf("%s: gate should have passed, but nothing actuated", label)
			}
		}
	})
	t.Logf("coverage: refused=%d proceeded=%d pdu=%d", refused.Load(), proceeded.Load(), pduRuns.Load())
}
