package store

// Sequential model-based property tests for the operation/machine state
// machine: generated histories replayed against a real Postgres-backed Store
// and the reference model in lockstep.
//
//	go test ./internal/store/ -run TestOpsmProp -count=1 -v
//	go test ./internal/store/ -run TestOpsmProp -count=1 -rapid.checks=5000

import (
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

var opsmStrictKinds = []opsmKind{
	kMirrorCreate, kMirrorDelete, kRebind, kSetLocked, kSetPXE, kPatch,
	kCreateLifecycle, kCreatePower,
	kClaim, kSetPhase, kHubComplete, kFail, kCommit, kValidate, kAbort, kDriftFail,
	kBackdate, kExpirePending, kExpireStuck, kExpireOrphans, kExpirePhoneHome, kScrub,
	kListActive,
}

var opsmRawOnlyKinds = []opsmKind{
	kRawCreate, kDeleteIfIdle, kStoreCommData,
	kCreateMachineOp, kCreateRaw, kComplete, kCompletePlain, kCompleteActive,
}

// Base weights. Transitions outweigh bookkeeping so histories make progress.
var opsmBaseWeight = map[opsmKind]int{
	kMirrorCreate: 3, kMirrorDelete: 1, kRebind: 1, kSetLocked: 2, kSetPXE: 2, kPatch: 1,
	kCreateLifecycle: 5, kCreatePower: 2,
	kClaim: 6, kSetPhase: 4, kHubComplete: 3, kFail: 2, kCommit: 4, kValidate: 1, kAbort: 1, kDriftFail: 1,
	kBackdate: 3, kExpirePending: 1, kExpireStuck: 1, kExpireOrphans: 1, kExpirePhoneHome: 1, kScrub: 1,
	kListActive: 1,
	kRawCreate:  2, kDeleteIfIdle: 1, kStoreCommData: 1,
	kCreateMachineOp: 2, kCreateRaw: 3, kComplete: 2, kCompletePlain: 1, kCompleteActive: 1,
}

var opsmBogusOp = uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")

// opsmGen draws actions for one generated case.
type opsmGen struct {
	h      *opsmHarness
	kinds  []opsmKind // weighted (a kind appears weight times)
	lastOp [opsmSlots]uuid.UUID
	queue  []opsmAction // remaining steps of the targeted pattern in progress
	last   *opsmAction  // previous step, for duplicate delivery
}

// newOpsmGen builds the per-case operation mix (swarm testing): each case
// keeps a random subset of the op kinds at a random weight, so some cases
// are sweep-heavy, some churn machines, some never fail anything.
func newOpsmGen(rt *rapid.T, h *opsmHarness, raw bool) *opsmGen {
	g := &opsmGen{h: h}
	pool := append([]opsmKind(nil), opsmStrictKinds...)
	if raw {
		pool = append(pool, opsmRawOnlyKinds...)
	}
	swarm := rapid.Bool().Draw(rt, "swarm")
	for _, k := range pool {
		w := opsmBaseWeight[k]
		if swarm {
			// 0 drops the kind from this case; 3 triples it.
			w *= rapid.SampledFrom([]int{0, 1, 1, 3}).Draw(rt, "w_"+string(k))
		}
		for i := 0; i < w; i++ {
			g.kinds = append(g.kinds, k)
		}
	}
	if len(g.kinds) == 0 {
		g.kinds = []opsmKind{kMirrorCreate, kCreateLifecycle, kClaim}
	}
	return g
}

func (g *opsmGen) slot(rt *rapid.T, label string) int {
	return rapid.IntRange(0, opsmSlots-1).Draw(rt, label)
}

// op picks the operation a step targets: usually one that is live in the
// reference state (so the step does something), sometimes any op ever
// created (terminal, or deleted with its machine), rarely an ID that never
// existed.
func (g *opsmGen) op(rt *rapid.T) uuid.UUID {
	h := g.h
	if len(h.opIDs) == 0 {
		return opsmBogusOp
	}
	mode := rapid.IntRange(0, 11).Draw(rt, "opMode")
	if mode == 0 {
		return opsmBogusOp
	}
	if mode >= 4 {
		var active []uuid.UUID
		for _, id := range h.opIDs {
			if o, ok := h.ref.ops[id]; ok && refActive(o.status) {
				active = append(active, id)
			}
		}
		if len(active) > 0 {
			return active[rapid.IntRange(0, len(active)-1).Draw(rt, "opActive")]
		}
	}
	return h.opIDs[rapid.IntRange(0, len(h.opIDs)-1).Draw(rt, "opAny")]
}

// machineSlotOf returns the slot holding the op's machine, or a drawn slot.
func (g *opsmGen) slotOfOp(rt *rapid.T, op uuid.UUID) int {
	if o, ok := g.h.ref.ops[op]; ok {
		for s, id := range g.h.slots {
			if id == o.machine {
				return s
			}
		}
	}
	return g.slot(rt, "slotFallback")
}

func (g *opsmGen) draw(rt *rapid.T) opsmAction {
	h := g.h
	kind := g.kinds[rapid.IntRange(0, len(g.kinds)-1).Draw(rt, "kind")]
	a := opsmAction{Kind: kind}
	switch kind {
	case kMirrorCreate:
		// The mirror only enrolls a serial it has no row for: prefer an
		// empty slot, and fall back to a no-op-free alternative otherwise.
		a.Slot = g.slot(rt, "slot")
		if h.slots[a.Slot] != uuid.Nil {
			for s := 0; s < opsmSlots; s++ {
				if h.slots[s] == uuid.Nil {
					a.Slot = s
					break
				}
			}
		}
		if h.slots[a.Slot] != uuid.Nil {
			// All slots full: nothing to enroll. Spend the step on a
			// pxe-enable instead, which keeps lifecycle ops creatable.
			a.Kind = kSetPXE
			a.Flag = true
			return a
		}
		a.NewID = uuid.MustParse(rapid.StringMatching(`[0-9a-f]{8}-0000-4000-8000-0000000000aa`).Draw(rt, "machineID"))
		a.Device = rapid.SampledFrom(opsmDeviceIDs).Draw(rt, "dev")
		a.Flag = rapid.Bool().Draw(rt, "pxe")
	case kRawCreate:
		a.Slot = g.slot(rt, "slot")
		a.NewID = uuid.MustParse(rapid.StringMatching(`[0-9a-f]{8}-0000-4000-8000-0000000000bb`).Draw(rt, "machineID"))
		a.Device = rapid.SampledFrom(opsmDeviceIDs).Draw(rt, "dev")
		a.State = rapid.SampledFrom(append([]string{""}, opsmStates...)).Draw(rt, "state")
		a.Flag = rapid.Bool().Draw(rt, "pxe")
	case kMirrorDelete, kDeleteIfIdle, kAbort:
		a.Slot = g.slot(rt, "slot")
	case kRebind:
		a.Slot = g.slot(rt, "slot")
		a.Device = rapid.SampledFrom(opsmDeviceIDs[1:]).Draw(rt, "dev")
	case kSetLocked, kSetPXE:
		a.Slot = g.slot(rt, "slot")
		a.Flag = rapid.Bool().Draw(rt, "flag")
	case kPatch:
		a.Slot = g.slot(rt, "slot")
		a.Text = rapid.SampledFrom([]string{"", "noble", "jammy"}).Draw(rt, "os")
	case kStoreCommData:
		a.Slot = g.slot(rt, "slot")
		a.Data = rapid.IntRange(0, len(opsmCommData)-1).Draw(rt, "data")
	case kCreateLifecycle:
		a.Slot = g.slot(rt, "slot")
		a.Type = rapid.SampledFrom([]string{"commission", "deploy"}).Draw(rt, "type")
		a.Allowed = opsmAPIAllowedSources[a.Type]
		a.Params = rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "params")
	case kCreateMachineOp:
		a.Slot = g.slot(rt, "slot")
		a.Type = rapid.SampledFrom(refAllOpTypes).Draw(rt, "type")
		a.Allowed = rapid.SliceOfDistinct(rapid.SampledFrom(opsmStates), rapid.ID[string]).Draw(rt, "allowed")
		a.Params = rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "params")
	case kCreatePower:
		a.Slot = g.slot(rt, "slot")
		a.Type = rapid.SampledFrom([]string{"power_on", "power_off", "power_cycle", "set_boot_device", "bmc_reconcile"}).Draw(rt, "type")
		a.Params = rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "params")
	case kCreateRaw:
		a.Slot = g.slot(rt, "slot")
		a.Type = rapid.SampledFrom(refAllOpTypes).Draw(rt, "type")
		a.Params = rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "params")
		a.NewID = uuid.MustParse(rapid.StringMatching(`[0-9a-f]{8}-0000-4000-8000-0000000000cc`).Draw(rt, "opID"))
	case kClaim:
		a.Op = g.op(rt)
		a.Worker = rapid.SampledFrom(opsmWorkers).Draw(rt, "worker")
	case kSetPhase:
		a.Op = g.op(rt)
		phases := opsmPhases
		if !h.strict {
			phases = append([]string{""}, opsmPhases...)
		}
		a.Phase = rapid.SampledFrom(phases).Draw(rt, "phase")
	case kHubComplete, kCompletePlain, kFail:
		a.Op = g.op(rt)
		a.Result = rapid.IntRange(0, len(opsmResults)-1).Draw(rt, "result")
	case kComplete:
		a.Op = g.op(rt)
		a.State = rapid.SampledFrom(append([]string{""}, opsmStates...)).Draw(rt, "target")
		a.Result = rapid.IntRange(0, len(opsmResults)-1).Draw(rt, "result")
	case kCompleteActive:
		a.Slot = g.slot(rt, "slot")
		a.Type = rapid.SampledFrom(refAllOpTypes).Draw(rt, "type")
		a.State = rapid.SampledFrom(opsmStates).Draw(rt, "target")
		a.Result = rapid.IntRange(0, len(opsmResults)-1).Draw(rt, "result")
	case kCommit, kValidate:
		a.Op = g.op(rt)
		// Mostly the op's own machine; sometimes another (cross-host report).
		if rapid.IntRange(0, 4).Draw(rt, "wrongMachine") == 0 {
			a.Slot2 = g.slot(rt, "slot2")
		} else {
			a.Slot2 = g.slotOfOp(rt, a.Op)
		}
		a.Data = rapid.IntRange(0, len(opsmCommData)-1).Draw(rt, "data")
		if kind == kValidate {
			a.Type = rapid.SampledFrom([]string{"commission", "deploy", "bmc_reconcile"}).Draw(rt, "type")
		}
	case kDriftFail:
		a.Slot = g.slot(rt, "slot")
		a.Text = rapid.SampledFrom([]string{"netbox device changed mid-operation (fields: site)", `quote " and \ backslash`}).Draw(rt, "reason")
	case kBackdate:
		a.Op = g.op(rt)
		a.Field = rapid.SampledFrom([]string{"created", "claimed", "phase"}).Draw(rt, "field")
	case kExpirePending, kExpireStuck, kExpireOrphans, kExpirePhoneHome:
		a.Window = opsmSweepWindow(rapid.SampledFrom([]int{int(opsmSweepOld), int(opsmSweepOld), int(opsmSweepAll), int(opsmSweepNone)}).Draw(rt, "window"))
	default: // kScrub, kListActive: no arguments
	}
	return a
}

// --- targeted preambles ---------------------------------------------------------

// opsmGoals are the per-slot starting situations a case can fast-forward
// into before the random suffix. Each is reached through ordinary checked
// steps, so the preamble is itself a tested history.
var opsmGoals = []string{
	"empty", "new", "pending_commission", "commissioning", "commissioned",
	"pending_deploy", "deploying", "awaiting_phone_home", "running",
	"running_unlocked", "failed", "recommissioning", "power_pending",
	"power_claimed", "running_bmc_reconcile",
}

func (g *opsmGen) lifecycle(slot int, typ string) {
	h := g.h
	before := len(h.opIDs)
	h.step(opsmAction{Kind: kCreateLifecycle, Slot: slot, Type: typ, Allowed: opsmAPIAllowedSources[typ], Params: 2})
	if len(h.opIDs) > before {
		g.lastOp[slot] = h.opIDs[len(h.opIDs)-1]
	}
}

// power creates a power-plane op on the slot and claims it.
func (g *opsmGen) power(slot int, typ string) {
	h := g.h
	before := len(h.opIDs)
	h.step(opsmAction{Kind: kCreatePower, Slot: slot, Type: typ, Params: 4})
	if len(h.opIDs) > before {
		g.lastOp[slot] = h.opIDs[len(h.opIDs)-1]
		h.step(opsmAction{Kind: kClaim, Op: g.lastOp[slot], Worker: "w1"})
	}
}

func (g *opsmGen) advance(slot int, goal string) {
	h := g.h
	if goal == "empty" || h.slots[slot] != uuid.Nil {
		return
	}
	dev := opsmDeviceIDs[slot+1]
	h.step(opsmAction{Kind: kMirrorCreate, Slot: slot, NewID: uuid.New(), Device: dev, Flag: true})
	reach := func(want string) bool { return goal == want }
	if reach("new") {
		return
	}
	if reach("power_pending") {
		before := len(h.opIDs)
		h.step(opsmAction{Kind: kCreatePower, Slot: slot, Type: "power_cycle", Params: 2})
		if len(h.opIDs) > before {
			g.lastOp[slot] = h.opIDs[len(h.opIDs)-1]
		}
		return
	}
	g.lifecycle(slot, "commission")
	if reach("pending_commission") {
		return
	}
	h.step(opsmAction{Kind: kClaim, Op: g.lastOp[slot], Worker: "w1"})
	if reach("commissioning") {
		return
	}
	h.step(opsmAction{Kind: kCommit, Op: g.lastOp[slot], Slot2: slot, Data: 0})
	if reach("commissioned") {
		return
	}
	if reach("power_claimed") {
		g.power(slot, "power_off")
		return
	}
	if reach("recommissioning") {
		g.lifecycle(slot, "commission")
		h.step(opsmAction{Kind: kClaim, Op: g.lastOp[slot], Worker: "w1"})
		return
	}
	g.lifecycle(slot, "deploy")
	if reach("pending_deploy") {
		return
	}
	h.step(opsmAction{Kind: kClaim, Op: g.lastOp[slot], Worker: "w1"})
	if reach("deploying") {
		return
	}
	if reach("failed") {
		h.step(opsmAction{Kind: kFail, Op: g.lastOp[slot], Result: 1})
		return
	}
	h.step(opsmAction{Kind: kSetPhase, Op: g.lastOp[slot], Phase: "partitioning"})
	h.step(opsmAction{Kind: kSetPhase, Op: g.lastOp[slot], Phase: "awaiting_phone_home"})
	if reach("awaiting_phone_home") {
		return
	}
	h.step(opsmAction{Kind: kHubComplete, Op: g.lastOp[slot], Result: 0})
	if reach("running") {
		return
	}
	if reach("running_bmc_reconcile") {
		// An in-service, locked host with a BMC op in flight.
		g.power(slot, "bmc_reconcile")
		return
	}
	h.step(opsmAction{Kind: kSetLocked, Slot: slot, Flag: false})
}

// runOpsmCase is one generated case: per-slot preambles, then a random
// history, every step checked.
func runOpsmCase(rt *rapid.T, st *Store, raw bool) {
	h := newOpsmHarness(rt, st, !raw)
	defer h.recordCoverage()
	g := newOpsmGen(rt, h, raw)
	for s := 0; s < opsmSlots; s++ {
		g.advance(s, rapid.SampledFrom(opsmGoals).Draw(rt, "goal"))
	}
	n := rapid.IntRange(1, 30).Draw(rt, "steps")
	for i := 0; i < n; i++ {
		h.step(g.next(rt))
	}
}

// TestOpsmProp_StrictHistories replays histories made only of calls the API,
// the hub handlers, the mirror and the sweep loop can make, and additionally
// requires machine state to be explained by the machine's operations after
// every step.
func TestOpsmProp_StrictHistories(t *testing.T) {
	st := testStore(t)
	t.Cleanup(func() { t.Logf("coverage (cumulative for this test binary):\n%s", opsmCoverageReport()) })
	rapid.Check(t, func(rt *rapid.T) { runOpsmCase(rt, st, false) })
}

// TestOpsmProp_RawHistories adds the store calls production does not make
// in these shapes (arbitrary completion targets, machines created in any
// state, bare inserts, delete attempts mid-operation). The reference model
// must still predict every observation and every row.
func TestOpsmProp_RawHistories(t *testing.T) {
	st := testStore(t)
	t.Cleanup(func() { t.Logf("coverage (cumulative for this test binary):\n%s", opsmCoverageReport()) })
	rapid.Check(t, func(rt *rapid.T) { runOpsmCase(rt, st, true) })
}
