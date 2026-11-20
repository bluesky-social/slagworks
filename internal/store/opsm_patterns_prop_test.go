package store

// Targeted patterns for the op/machine histories. A uniformly random walk
// over ~30 op kinds almost never backdates an op and then runs the one sweep
// that would catch it, or delivers a report for an op the step before made
// terminal. These patterns script those interactions and are embedded in the
// random history (random steps may still land between two scripted ones).

import (
	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// next returns the case's next step: a queued step of a targeted pattern, a
// verbatim duplicate of the previous step (at-least-once delivery), the
// first step of a newly drawn pattern, or a random step.
func (g *opsmGen) next(rt *rapid.T) opsmAction {
	if len(g.queue) > 0 && rapid.IntRange(0, 5).Draw(rt, "runQueued") != 0 {
		return g.pop()
	}
	roll := rapid.IntRange(0, 19).Draw(rt, "roll")
	switch {
	case roll == 0 && g.last != nil:
		g.h.ref.hit("gen.duplicate_delivery")
		return *g.last
	case roll <= 5:
		g.enqueuePattern(rt)
		if len(g.queue) > 0 {
			return g.pop()
		}
	}
	return g.remember(g.draw(rt))
}

func (g *opsmGen) pop() opsmAction {
	a := g.queue[0]
	g.queue = g.queue[1:]
	if a.LateOp {
		a.LateOp = false
		a.Op = g.latestOpOn(a.Slot)
	}
	return g.remember(a)
}

// latestOpOn returns the active op on the slot's machine, else the most
// recently created op that still exists for it, else an ID that never
// existed.
func (g *opsmGen) latestOpOn(slot int) uuid.UUID {
	mid := g.h.slots[slot]
	if o := g.h.ref.activeOpOn(mid); o != nil && mid != uuid.Nil {
		return o.id
	}
	for i := len(g.h.opIDs) - 1; i >= 0; i-- {
		if o, ok := g.h.ref.ops[g.h.opIDs[i]]; ok && o.machine == mid {
			return o.id
		}
	}
	return opsmBogusOp
}

func (g *opsmGen) remember(a opsmAction) opsmAction {
	cp := a
	g.last = &cp
	return a
}

// activeOps lists the reference model's active ops in creation order.
func (g *opsmGen) activeOps() []*refOp {
	var out []*refOp
	for _, id := range g.h.opIDs {
		if o, ok := g.h.ref.ops[id]; ok && refActive(o.status) {
			out = append(out, o)
		}
	}
	return out
}

func (g *opsmGen) slotOfMachine(id uuid.UUID) (int, bool) {
	for s, mid := range g.h.slots {
		if mid == id && id != uuid.Nil {
			return s, true
		}
	}
	return 0, false
}

func (g *opsmGen) enqueuePattern(rt *rapid.T) {
	h := g.h
	pattern := rapid.SampledFrom([]string{
		"age_sweep", "age_sweep", "late_report", "late_report", "reclaim",
		"delete_recreate", "lock_dance", "phone_home", "start_lifecycle", "start_lifecycle",
		"power_op", "power_op",
	}).Draw(rt, "pattern")
	active := g.activeOps()
	var op *refOp
	if len(active) > 0 {
		op = active[rapid.IntRange(0, len(active)-1).Draw(rt, "patternOp")]
	}
	sweeps := []opsmKind{kExpirePending, kExpireStuck, kExpireOrphans, kExpirePhoneHome}

	switch pattern {
	case "age_sweep":
		// Backdate the clock a sweep reads, then sweep. The sweep is the
		// matching one most of the time and a non-matching one otherwise
		// (which must leave the op alone).
		if op == nil {
			return
		}
		field, match := "claimed", kExpireStuck
		switch {
		case op.status == "pending":
			field, match = "created", kExpirePending
		case op.phase == "awaiting_phone_home":
			field, match = "phase", kExpirePhoneHome
			if rapid.Bool().Draw(rt, "ageClaimedInstead") {
				field, match = "claimed", kExpireStuck // must be spared
			}
		case op.phase == "":
			match = rapid.SampledFrom([]opsmKind{kExpireStuck, kExpireOrphans}).Draw(rt, "claimedSweep")
		}
		sweep := match
		if rapid.IntRange(0, 3).Draw(rt, "otherSweep") == 0 {
			sweep = rapid.SampledFrom(sweeps).Draw(rt, "sweep")
		}
		g.queue = append(g.queue,
			opsmAction{Kind: kBackdate, Op: op.id, Field: field},
			opsmAction{Kind: sweep, Window: opsmSweepOld})
		h.ref.hit("gen.pattern.age_sweep")

	case "late_report":
		// Make an op terminal, then deliver reports that were already in
		// flight for it.
		if op == nil {
			return
		}
		slot, ok := g.slotOfMachine(op.machine)
		if !ok {
			return
		}
		res := rapid.IntRange(0, len(opsmResults)-1).Draw(rt, "lateResult")
		data := rapid.IntRange(0, len(opsmCommData)-1).Draw(rt, "lateData")
		terminal := []opsmAction{
			{Kind: kFail, Op: op.id, Result: res},
			{Kind: kHubComplete, Op: op.id, Result: res},
			{Kind: kAbort, Slot: slot},
			{Kind: kDriftFail, Slot: slot, Text: "netbox device changed mid-operation (fields: site)"},
			{Kind: kCommit, Op: op.id, Slot2: slot, Data: data},
		}
		g.queue = append(g.queue, terminal[rapid.IntRange(0, len(terminal)-1).Draw(rt, "terminalBy")])
		late := []opsmAction{
			{Kind: kSetPhase, Op: op.id, Phase: rapid.SampledFrom(opsmPhases).Draw(rt, "latePhase")},
			{Kind: kClaim, Op: op.id, Worker: rapid.SampledFrom(opsmWorkers).Draw(rt, "lateWorker")},
			{Kind: kHubComplete, Op: op.id, Result: res},
			{Kind: kFail, Op: op.id, Result: res},
			{Kind: kCommit, Op: op.id, Slot2: slot, Data: rapid.IntRange(0, len(opsmCommData)-1).Draw(rt, "replayData")},
			{Kind: kValidate, Op: op.id, Slot2: slot, Type: op.typ, Data: data},
			{Kind: kScrub},
		}
		n := rapid.IntRange(1, 3).Draw(rt, "lateCount")
		for i := 0; i < n; i++ {
			g.queue = append(g.queue, late[rapid.IntRange(0, len(late)-1).Draw(rt, "lateKind")])
		}
		h.ref.hit("gen.pattern.late_report")

	case "reclaim":
		// The claim RPC is at-least-once: same worker again, the other
		// worker, then a phase (which ends re-claimability), then again.
		if op == nil {
			return
		}
		w := rapid.SampledFrom(opsmWorkers).Draw(rt, "reclaimWorker")
		other := opsmWorkers[0]
		if other == w {
			other = opsmWorkers[1]
		}
		g.queue = append(g.queue,
			opsmAction{Kind: kClaim, Op: op.id, Worker: w},
			opsmAction{Kind: kClaim, Op: op.id, Worker: w},
			opsmAction{Kind: kClaim, Op: op.id, Worker: other},
			opsmAction{Kind: kSetPhase, Op: op.id, Phase: "power_cycle"},
			opsmAction{Kind: kClaim, Op: op.id, Worker: w})
		h.ref.hit("gen.pattern.reclaim")

	case "delete_recreate":
		// NetBox delete then re-create of the same serial: a new row, a new
		// UUID, and whatever op IDs the old row had are now stale.
		slot := g.slot(rt, "patternSlot")
		if h.slots[slot] == uuid.Nil {
			return
		}
		del := opsmAction{Kind: kMirrorDelete, Slot: slot}
		if !h.strict && rapid.Bool().Draw(rt, "bareDelete") {
			del.Kind = kDeleteIfIdle
		}
		id := uuid.MustParse(rapid.StringMatching(`[0-9a-f]{8}-0000-4000-8000-0000000000ee`).Draw(rt, "recreateID"))
		g.queue = append(g.queue, del, del,
			opsmAction{Kind: kMirrorCreate, Slot: slot, NewID: id, Device: opsmDeviceIDs[slot+1], Flag: true},
			opsmAction{Kind: kCreateLifecycle, Slot: slot, Type: "commission", Allowed: opsmAPIAllowedSources["commission"], Params: 3})
		if op != nil {
			g.queue = append(g.queue, opsmAction{Kind: kClaim, Op: op.id, Worker: "w1"})
		}
		h.ref.hit("gen.pattern.delete_recreate")

	case "lock_dance":
		// Operator locks a machine around an op's create/claim.
		slot := g.slot(rt, "patternSlot")
		if h.slots[slot] == uuid.Nil {
			return
		}
		typ := rapid.SampledFrom([]string{"commission", "deploy"}).Draw(rt, "lockType")
		create := opsmAction{Kind: kCreateLifecycle, Slot: slot, Type: typ, Allowed: opsmAPIAllowedSources[typ], Params: 2}
		lock := opsmAction{Kind: kSetLocked, Slot: slot, Flag: true}
		unlock := opsmAction{Kind: kSetLocked, Slot: slot, Flag: false}
		if rapid.Bool().Draw(rt, "lockAfterCreate") {
			g.queue = append(g.queue, unlock, create, lock)
		} else {
			g.queue = append(g.queue, lock, create, unlock, create)
		}
		h.ref.hit("gen.pattern.lock_dance")

	case "phone_home":
		// The awaiting_phone_home latch: enter it, regress out of it, and
		// age both clocks against both sweeps.
		if op == nil || op.status == "pending" {
			return
		}
		steps := []opsmAction{
			{Kind: kSetPhase, Op: op.id, Phase: "awaiting_phone_home"},
			{Kind: kSetPhase, Op: op.id, Phase: "awaiting_phone_home"},
			{Kind: kBackdate, Op: op.id, Field: "claimed"},
			{Kind: kExpireStuck, Window: opsmSweepOld},
			{Kind: kExpirePhoneHome, Window: opsmSweepOld},
			{Kind: kSetPhase, Op: op.id, Phase: "pxe_booting"},
			{Kind: kSetPhase, Op: op.id, Phase: "awaiting_phone_home"},
			{Kind: kBackdate, Op: op.id, Field: "phase"},
			{Kind: kExpirePhoneHome, Window: opsmSweepOld},
		}
		// A random contiguous slice keeps the pattern from always running
		// to the op's death.
		from := rapid.IntRange(0, len(steps)-2).Draw(rt, "phFrom")
		to := rapid.IntRange(from+1, len(steps)).Draw(rt, "phTo")
		g.queue = append(g.queue, steps[from:to]...)
		h.ref.hit("gen.pattern.phone_home")

	case "power_op":
		// A power-plane op (power on/off/cycle, set_boot_device,
		// bmc_reconcile) from create to its end. These target in-service
		// hosts too, and their failure must never touch machine state.
		slot := g.slot(rt, "patternSlot")
		if h.slots[slot] == uuid.Nil {
			return
		}
		typ := rapid.SampledFrom([]string{"power_on", "power_off", "power_cycle", "set_boot_device", "bmc_reconcile"}).Draw(rt, "powerType")
		g.queue = append(g.queue,
			opsmAction{Kind: kCreatePower, Slot: slot, Type: typ, Params: rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "powerParams")},
			opsmAction{Kind: kClaim, Slot: slot, LateOp: true, Worker: rapid.SampledFrom(opsmWorkers).Draw(rt, "powerWorker")})
		switch rapid.SampledFrom([]string{"fail", "complete", "abort", "drift", "stuck", "orphan", "leave"}).Draw(rt, "powerEnd") {
		case "fail":
			g.queue = append(g.queue, opsmAction{Kind: kFail, Slot: slot, LateOp: true, Result: 1})
		case "complete":
			g.queue = append(g.queue, opsmAction{Kind: kHubComplete, Slot: slot, LateOp: true, Result: 0})
		case "abort":
			g.queue = append(g.queue, opsmAction{Kind: kAbort, Slot: slot})
		case "drift":
			g.queue = append(g.queue, opsmAction{Kind: kDriftFail, Slot: slot, Text: "netbox device changed mid-operation (fields: site)"})
		case "stuck":
			g.queue = append(g.queue,
				opsmAction{Kind: kBackdate, Slot: slot, LateOp: true, Field: "claimed"},
				opsmAction{Kind: kExpireStuck, Window: opsmSweepOld})
		case "orphan":
			g.queue = append(g.queue,
				opsmAction{Kind: kBackdate, Slot: slot, LateOp: true, Field: "claimed"},
				opsmAction{Kind: kExpireOrphans, Window: opsmSweepOld})
		}
		h.ref.hit("gen.pattern.power_op")

	case "start_lifecycle":
		// Get a lifecycle op claimed on a machine that can take one.
		slot := g.slot(rt, "patternSlot")
		m, ok := h.ref.machines[h.slots[slot]]
		if !ok || h.ref.activeOpOn(m.id) != nil {
			return
		}
		typ := "commission"
		if m.comm != "" && rapid.IntRange(0, 3).Draw(rt, "deployIt") != 0 {
			typ = "deploy"
		}
		g.queue = append(g.queue,
			opsmAction{Kind: kSetLocked, Slot: slot, Flag: false},
			opsmAction{Kind: kSetPXE, Slot: slot, Flag: true},
			opsmAction{Kind: kCreateLifecycle, Slot: slot, Type: typ, Allowed: opsmAPIAllowedSources[typ],
				Params: rapid.IntRange(0, len(opsmParams)-1).Draw(rt, "lcParams")})
		h.ref.hit("gen.pattern.start_lifecycle")
	}
}
