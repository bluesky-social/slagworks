package store

// Replay harness for the op/machine property tests: an operation-sequence
// AST (opsmAction), an adapter that applies an action to the real
// Postgres-backed Store, the lockstep application to the reference model
// (opsm_model_prop_test.go), and the after-every-step checks.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

// opsmT is the subset of testing.T / rapid.T the harness needs.
type opsmT interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// --- pools (deliberately tiny so operations interact) ------------------------

const opsmSlots = 3

var (
	opsmSerials   = [opsmSlots]string{"SN-A", "SN-B", "SN-C"}
	opsmDeviceIDs = []int{0, 11, 12, 13}
	opsmWorkers   = []string{"w1", "w2"}
	opsmPhases    = []string{"power_cycle", "pxe_booting", "partitioning", "awaiting_phone_home", "failed"}
	opsmStates    = []string{"new", "commissioning", "commissioned", "deploying", "running", "failed"}

	// Commission reports. 0 and 1 are the same document spelled
	// differently; 3 carries a number Postgres jsonb re-spells.
	opsmCommData = []string{
		`{"serial":"S1","cpus":2}`,
		`{ "cpus": 2, "serial": "S1" }`,
		`{"serial":"S2","cpus":4}`,
		`{"serial":"S1","mem_kb":1e3}`,
	}
	// Fail / complete results. "" is a nil result.
	opsmResults = []string{
		``,
		`{"error":"boom"}`,
		`{"error":"disk on fire","phase":"partitioning"}`,
		`"just a string"`,
		`{"error":42}`,
		`{"ok":true}`,
	}
	// Op params. Credential-bearing documents follow the documented
	// credential list; the odd shapes must survive a scrub untouched.
	opsmParams = []string{
		``,
		`{}`,
		`{"destructive":true,"bmc_runtime":{"expected_chassis_serial":"X","username":"admin","password":"hunter2","extra_config":{}}}`,
		`{"pdu_runtime":{"outlets":[{"host":"pdu1","outlet":3,"username":"apc","password":"pw1"},{"host":"pdu2","outlet":4,"username":"apc","password":"pw2"}]},"tailscale_runtime":{"auth_key":"tskey-abc","tags":["tag:x"]}}`,
		`{"amt_runtime":{"host":"h","password":"amtpw"},"monitor_runtime":{"password":"monpw"},"rotate_monitor":true}`,
		`{"bmc_runtime":"not-an-object","pdu_runtime":{"outlets":{"password":"x"}}}`,
		`[1,2,3]`,
	}
)

// --- the operation AST --------------------------------------------------------

type opsmKind string

const (
	// machines
	kMirrorCreate  opsmKind = "MirrorCreate"  // enroll a machine in an empty slot
	kMirrorDelete  opsmKind = "MirrorDelete"  // NetBox delete: fail in-flight ops, else hard delete
	kRawCreate     opsmKind = "RawCreate"     // CreateMachine with arbitrary state (raw mode)
	kDeleteIfIdle  opsmKind = "DeleteIfIdle"  // DeleteMachineIfIdle on its own (raw mode)
	kRebind        opsmKind = "Rebind"        // SetMachineNetBoxDeviceID
	kSetLocked     opsmKind = "SetLocked"     // lock / unlock
	kSetPXE        opsmKind = "SetPXE"        // pxe-enable / pxe-disable
	kPatch         opsmKind = "Patch"         // PATCH desired_os (SetMachineDesiredOS)
	kStoreCommData opsmKind = "StoreCommData" // StoreCommissionData (raw mode)
	// op creation
	kCreateLifecycle opsmKind = "CreateLifecycle" // CreateMachineOp with the API's allowed sources
	kCreateMachineOp opsmKind = "CreateMachineOp" // CreateMachineOp with arbitrary type/sources (raw mode)
	kCreatePower     opsmKind = "CreatePower"     // CreatePowerOp
	kCreateRaw       opsmKind = "CreateRaw"       // CreateOperation (raw mode)
	// op transitions
	kClaim          opsmKind = "Claim"
	kSetPhase       opsmKind = "SetPhase"
	kHubComplete    opsmKind = "HubComplete"    // CompleteOperationWithState with the hub's target for the type
	kComplete       opsmKind = "Complete"       // CompleteOperationWithState with an arbitrary target (raw mode)
	kCompletePlain  opsmKind = "CompletePlain"  // CompleteOperation (raw mode)
	kCompleteActive opsmKind = "CompleteActive" // CompleteActiveOperation (raw mode)
	kFail           opsmKind = "Fail"           // FailOperationOutcome
	kCommit         opsmKind = "Commit"         // CommitCommissionResult
	kValidate       opsmKind = "Validate"       // ValidateOperationBinding
	kAbort          opsmKind = "Abort"          // AbortMachine
	kDriftFail      opsmKind = "DriftFail"      // FailInFlightOpsForMachine
	// time and sweeps
	kBackdate        opsmKind = "Backdate"
	kExpirePending   opsmKind = "ExpirePending"
	kExpireStuck     opsmKind = "ExpireStuck"
	kExpireOrphans   opsmKind = "ExpireOrphans"
	kExpirePhoneHome opsmKind = "ExpirePhoneHome"
	kScrub           opsmKind = "Scrub"
	// reads
	kListActive opsmKind = "ListActive"
)

// opsmAction is one step of a generated history. Unused fields are zero.
type opsmAction struct {
	Kind    opsmKind
	Slot    int       // machine slot
	Slot2   int       // second machine slot (the machine a report claims)
	Op      uuid.UUID // operation the step targets (may be absent or bogus)
	NewID   uuid.UUID // pre-drawn ID for a row the harness inserts itself
	Type    string    // operation type
	Worker  string
	Phase   string
	State   string // machine state (raw create, direct state, complete target)
	Device  int
	Flag    bool
	Text    string // desired_os / drift reason
	Data    int    // index into opsmCommData
	Result  int    // index into opsmResults
	Params  int    // index into opsmParams
	Allowed []string
	Window  opsmSweepWindow
	Field   string // backdate field
	// LateOp: the generator fills Op in when the step is about to run, with
	// the active (else latest) op on Slot. It lets a scripted pattern
	// target an op an earlier step of the same pattern creates.
	LateOp bool
}

func (a opsmAction) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", a.Kind)
	switch a.Kind {
	case kMirrorCreate:
		fmt.Fprintf(&b, "(slot=%d dev=%d pxe=%v)", a.Slot, a.Device, a.Flag)
	case kRawCreate:
		fmt.Fprintf(&b, "(slot=%d dev=%d state=%s pxe=%v)", a.Slot, a.Device, a.State, a.Flag)
	case kMirrorDelete, kDeleteIfIdle, kAbort:
		fmt.Fprintf(&b, "(slot=%d)", a.Slot)
	case kRebind:
		fmt.Fprintf(&b, "(slot=%d dev=%d)", a.Slot, a.Device)
	case kSetLocked, kSetPXE:
		fmt.Fprintf(&b, "(slot=%d %v)", a.Slot, a.Flag)
	case kPatch:
		fmt.Fprintf(&b, "(slot=%d os=%q)", a.Slot, a.Text)
	case kStoreCommData:
		fmt.Fprintf(&b, "(slot=%d data#%d)", a.Slot, a.Data)
	case kCreateLifecycle, kCreatePower, kCreateRaw:
		fmt.Fprintf(&b, "(slot=%d type=%s params#%d)", a.Slot, a.Type, a.Params)
	case kCreateMachineOp:
		fmt.Fprintf(&b, "(slot=%d type=%s params#%d allowed=%v)", a.Slot, a.Type, a.Params, a.Allowed)
	case kClaim:
		fmt.Fprintf(&b, "(op=%s worker=%s)", opsmShort(a.Op), a.Worker)
	case kSetPhase:
		fmt.Fprintf(&b, "(op=%s phase=%q)", opsmShort(a.Op), a.Phase)
	case kHubComplete, kCompletePlain:
		fmt.Fprintf(&b, "(op=%s result#%d)", opsmShort(a.Op), a.Result)
	case kComplete:
		fmt.Fprintf(&b, "(op=%s target=%q result#%d)", opsmShort(a.Op), a.State, a.Result)
	case kCompleteActive:
		fmt.Fprintf(&b, "(slot=%d type=%s target=%s)", a.Slot, a.Type, a.State)
	case kFail:
		fmt.Fprintf(&b, "(op=%s result#%d)", opsmShort(a.Op), a.Result)
	case kCommit:
		fmt.Fprintf(&b, "(op=%s slot=%d data#%d)", opsmShort(a.Op), a.Slot2, a.Data)
	case kValidate:
		fmt.Fprintf(&b, "(op=%s slot=%d type=%s data#%d)", opsmShort(a.Op), a.Slot2, a.Type, a.Data)
	case kDriftFail:
		fmt.Fprintf(&b, "(slot=%d reason=%q)", a.Slot, a.Text)
	case kBackdate:
		fmt.Fprintf(&b, "(op=%s %s)", opsmShort(a.Op), a.Field)
	case kExpirePending, kExpireStuck, kExpireOrphans, kExpirePhoneHome:
		fmt.Fprintf(&b, "(window=%d)", a.Window)
	default: // kScrub, kListActive: no arguments
	}
	return b.String()
}

func opsmShort(id uuid.UUID) string { return id.String()[:8] }

// --- harness ------------------------------------------------------------------

type opsmHarness struct {
	t   opsmT
	st  *Store
	ref *refModel
	// strict: the history contains only production-reachable calls, so the
	// "machine state is explained by its operations" invariants apply.
	strict bool

	slots [opsmSlots]uuid.UUID   // current machine per slot (Nil = empty)
	opIDs []uuid.UUID            // every op ID ever created, in creation order
	prev  map[uuid.UUID]opsmDBOp // op rows as of the previous check
	prevG map[uuid.UUID][3]int   // model generations as of the previous check
	trace []string
	steps int
	// noReadback skips the full-database comparison after each step; the
	// observation comparison still runs. The interleaving explorer sets it
	// for its setup steps, which the sequential properties already cover.
	noReadback bool
}

func newOpsmHarness(t opsmT, st *Store, strict bool) *opsmHarness {
	h := &opsmHarness{
		t: t, st: st, ref: newRefModel(), strict: strict,
		prev: map[uuid.UUID]opsmDBOp{}, prevG: map[uuid.UUID][3]int{},
	}
	h.resetDB()
	return h
}

// resetDB clears the three tables the state machine touches. Cheaper than
// testpg's TRUNCATE-everything and sufficient: the harness writes nowhere
// else.
func (h *opsmHarness) resetDB() {
	for _, q := range []string{
		"DELETE FROM log_envelopes", "DELETE FROM operations", "DELETE FROM machines",
	} {
		if err := h.st.db.Exec(q).Error; err != nil {
			h.t.Fatalf("reset %q: %v", q, err)
		}
	}
}

// opsmPhantomMachine is the ID used for an empty slot, so "machine absent"
// paths are exercised with a stable, never-existing ID.
func opsmPhantomMachine(slot int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", slot+1))
}

func (h *opsmHarness) machineOrPhantom(slot int) uuid.UUID {
	if id := h.slots[slot]; id != uuid.Nil {
		return id
	}
	return opsmPhantomMachine(slot)
}

func opsmErrClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrOperationBinding):
		return "binding"
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, ErrMachineNotFound):
		return "notfound"
	default:
		return "error"
	}
}

func opsmViewOf(op *model.Operation) opsmOpView {
	return opsmOpView{ID: op.ID, Status: string(op.Status), Phase: op.Phase, ClaimedBy: op.ClaimedBy, Version: op.Version}
}

func opsmObsFromOutcomes(outs []OpOutcome) opsmObs {
	sort.Slice(outs, func(i, j int) bool { return bytes.Compare(outs[i].Op.ID[:], outs[j].Op.ID[:]) < 0 })
	var o opsmObs
	for i := range outs {
		o.Ops = append(o.Ops, opsmViewOf(&outs[i].Op))
		o.MStates = append(o.MStates, string(outs[i].MachineState))
	}
	return o
}

func opsmObsFromOutcome(out *OpOutcome, err error) opsmObs {
	if err != nil {
		return opsmObs{Err: opsmErrClass(err)}
	}
	if out == nil {
		return opsmObs{Nil: true}
	}
	return opsmObs{Ops: []opsmOpView{opsmViewOf(&out.Op)}, MStates: []string{string(out.MachineState)}}
}

func opsmRawOrNil(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func opsmBytesOrNil(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func opsmWindowTimeout(w opsmSweepWindow) time.Duration {
	switch w {
	case opsmSweepAll:
		return -time.Hour
	case opsmSweepOld:
		return time.Hour
	default:
		return 1000 * time.Hour
	}
}

// opsmHubCompletionTarget is the machine state the hub's op_update handler
// passes for a completed op of the given type (docs: "deploy passes
// MachineStateRunning; empty target means no state change (power ops)";
// the commission branch exists "for completeness against ad-hoc
// completions").
func opsmHubCompletionTarget(opType string) string {
	switch opType {
	case "deploy":
		return "running"
	case "commission":
		return "commissioned"
	default:
		return ""
	}
}

var opsmAPIAllowedSources = map[string][]string{
	"deploy":     {"commissioned", "failed", "running"},
	"commission": {"new", "commissioned", "failed", "running"},
}

// applyReal runs one action against the real store and returns what it
// observed. It must not consult the reference model.
func (h *opsmHarness) applyReal(st *Store, a opsmAction) opsmObs {
	switch a.Kind {
	case kMirrorCreate, kRawCreate:
		m := &model.Machine{
			ID: a.NewID, NetBoxSerial: opsmSerials[a.Slot],
			State: model.MachineState(a.State), PXEEligible: a.Flag,
		}
		if a.Device != 0 {
			d := a.Device
			m.NetBoxDeviceID = &d
		}
		return opsmObs{Err: opsmErrClass(st.CreateMachine(m))}
	case kMirrorDelete:
		// internal/mirror OnDeviceDeleted: fail in-flight ops and keep the
		// row when there were any; otherwise delete the idle machine. A
		// delete refused because an op appeared in between gets the same
		// treatment as an op that was in flight from the start.
		id := h.machineOrPhantom(a.Slot)
		const reason = "netbox device deleted mid-operation"
		outs, err := st.FailInFlightOpsForMachine(id, reason)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		if len(outs) > 0 {
			return opsmObsFromOutcomes(outs)
		}
		deleted, err := st.DeleteMachineIfIdle(id)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		if deleted {
			return opsmObs{Deleted: true}
		}
		outs, err = st.FailInFlightOpsForMachine(id, reason)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObsFromOutcomes(outs)
	case kDeleteIfIdle:
		deleted, err := st.DeleteMachineIfIdle(h.machineOrPhantom(a.Slot))
		return opsmObs{Err: opsmErrClass(err), Deleted: deleted}
	case kRebind:
		return opsmObs{Err: opsmErrClass(st.SetMachineNetBoxDeviceID(h.machineOrPhantom(a.Slot), a.Device))}
	case kSetLocked:
		return opsmObs{Err: opsmErrClass(st.SetMachineLocked(h.machineOrPhantom(a.Slot), a.Flag))}
	case kSetPXE:
		return opsmObs{Err: opsmErrClass(st.SetMachinePXEEligible(h.machineOrPhantom(a.Slot), a.Flag))}
	case kPatch:
		// internal/api updateMachine.
		return opsmObs{Err: opsmErrClass(st.SetMachineDesiredOS(h.machineOrPhantom(a.Slot), a.Text))}
	case kStoreCommData:
		return opsmObs{Err: opsmErrClass(st.StoreCommissionData(h.machineOrPhantom(a.Slot), opsmRawOrNil(opsmCommData[a.Data])))}

	case kCreateLifecycle, kCreateMachineOp:
		allowed := make([]model.MachineState, len(a.Allowed))
		for i, s := range a.Allowed {
			allowed[i] = model.MachineState(s)
		}
		op, err := st.CreateMachineOp(h.machineOrPhantom(a.Slot), model.OperationType(a.Type), opsmRawOrNil(opsmParams[a.Params]), allowed...)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObs{Ops: []opsmOpView{opsmViewOf(op)}}
	case kCreatePower:
		op, err := st.CreatePowerOp(h.machineOrPhantom(a.Slot), model.OperationType(a.Type), opsmRawOrNil(opsmParams[a.Params]))
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObs{Ops: []opsmOpView{opsmViewOf(op)}}
	case kCreateRaw:
		op := &model.Operation{
			ID: a.NewID, MachineID: h.machineOrPhantom(a.Slot),
			Type: model.OperationType(a.Type), Params: opsmRawOrNil(opsmParams[a.Params]),
		}
		if err := st.CreateOperation(op); err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObs{Ops: []opsmOpView{opsmViewOf(op)}}

	case kClaim:
		op, err := st.ClaimOperation(a.Op, a.Worker)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		if op == nil {
			return opsmObs{Nil: true}
		}
		return opsmObs{Ops: []opsmOpView{opsmViewOf(op)}}
	case kSetPhase:
		tr, err := st.SetOperationPhase(a.Op, a.Phase)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		o := opsmObs{Changed: tr.Changed, Prev: tr.PrevPhase}
		if tr.Op != nil {
			o.Ops = []opsmOpView{opsmViewOf(tr.Op)}
		}
		return o
	case kHubComplete:
		// internal/hubhandlers HandleOpUpdate{status=completed}: read the op
		// to pick the target, then complete.
		op, err := st.GetOperation(a.Op)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObsFromOutcome(st.CompleteOperationWithState(a.Op,
			model.MachineState(opsmHubCompletionTarget(string(op.Type))), opsmBytesOrNil(opsmResults[a.Result])))
	case kComplete:
		return opsmObsFromOutcome(st.CompleteOperationWithState(a.Op, model.MachineState(a.State), opsmBytesOrNil(opsmResults[a.Result])))
	case kCompletePlain:
		return opsmObs{Err: opsmErrClass(st.CompleteOperation(a.Op, opsmBytesOrNil(opsmResults[a.Result])))}
	case kCompleteActive:
		return opsmObsFromOutcome(st.CompleteActiveOperation(h.machineOrPhantom(a.Slot),
			model.OperationType(a.Type), model.MachineState(a.State), opsmBytesOrNil(opsmResults[a.Result])))
	case kFail:
		return opsmObsFromOutcome(st.FailOperationOutcome(a.Op, opsmBytesOrNil(opsmResults[a.Result])))
	case kCommit:
		out, already, err := st.CommitCommissionResult(a.Op, h.machineOrPhantom(a.Slot2), opsmRawOrNil(opsmCommData[a.Data]))
		o := opsmObsFromOutcome(out, err)
		o.Nil = out == nil
		o.Already = already
		return o
	case kValidate:
		already, err := st.ValidateOperationBinding(a.Op, h.machineOrPhantom(a.Slot2),
			model.OperationType(a.Type), opsmRawOrNil(opsmCommData[a.Data]))
		return opsmObs{Err: opsmErrClass(err), Already: already}
	case kAbort:
		m, outs, err := st.AbortMachine(h.machineOrPhantom(a.Slot))
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		o := opsmObsFromOutcomes(outs)
		o.MState, o.MLocked = string(m.State), m.Locked
		return o
	case kDriftFail:
		outs, err := st.FailInFlightOpsForMachine(h.machineOrPhantom(a.Slot), a.Text)
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		return opsmObsFromOutcomes(outs)

	case kBackdate:
		col := map[string]string{"created": "created_at", "claimed": "claimed_at", "phase": "phase_entered_at"}[a.Field]
		q := st.db.Model(&model.Operation{}).Where("id = ?", a.Op)
		if a.Field == "claimed" {
			q = q.Where("claimed_at IS NOT NULL")
		}
		return opsmObs{Err: opsmErrClass(q.Update(col, time.Now().Add(-2*time.Hour)).Error)}
	case kExpirePending:
		outs, err := st.ExpirePendingOperations(opsmWindowTimeout(a.Window))
		return opsmSweepObs(outs, err)
	case kExpireStuck:
		outs, err := st.ExpireStuckOperations(opsmWindowTimeout(a.Window))
		return opsmSweepObs(outs, err)
	case kExpireOrphans:
		outs, err := st.ExpireOrphanedClaims(opsmWindowTimeout(a.Window))
		return opsmSweepObs(outs, err)
	case kExpirePhoneHome:
		outs, err := st.ExpirePhoneHomeOperations(opsmWindowTimeout(a.Window))
		return opsmSweepObs(outs, err)
	case kScrub:
		n, err := st.ScrubTerminalOperationCredentials()
		return opsmObs{Err: opsmErrClass(err), N: n}
	case kListActive:
		ops, err := st.ListActiveOperations()
		if err != nil {
			return opsmObs{Err: opsmErrClass(err)}
		}
		sort.Slice(ops, func(i, j int) bool { return bytes.Compare(ops[i].ID[:], ops[j].ID[:]) < 0 })
		var o opsmObs
		for i := range ops {
			o.Ops = append(o.Ops, opsmViewOf(&ops[i]))
		}
		return o
	}
	h.t.Fatalf("applyReal: unknown action kind %q", a.Kind)
	return opsmObs{}
}

func opsmSweepObs(outs []OpOutcome, err error) opsmObs {
	if err != nil {
		return opsmObs{Err: opsmErrClass(err)}
	}
	return opsmObsFromOutcomes(outs)
}

// opsmApplyRef runs one action against the reference model. got is the real
// store's observation, consulted ONLY to learn IDs the store minted itself.
func opsmApplyRef(r *refModel, a opsmAction, mid, mid2 uuid.UUID, got opsmObs) opsmObs {
	minted := uuid.Nil
	if len(got.Ops) == 1 {
		minted = got.Ops[0].ID
	}
	switch a.Kind {
	case kMirrorCreate, kRawCreate:
		return r.createMachine(a.NewID, opsmSerialFor(a.Slot), a.Device, a.State, a.Flag, false)
	case kMirrorDelete:
		out := r.failInFlight(mid, "netbox device deleted mid-operation")
		if len(out.Ops) > 0 {
			r.hit("mirror_delete.retained_mid_op")
			return out
		}
		return r.deleteIfIdle(mid)
	case kDeleteIfIdle:
		return r.deleteIfIdle(mid)
	case kRebind:
		return r.rebind(mid, a.Device)
	case kSetLocked:
		return r.setLocked(mid, a.Flag)
	case kSetPXE:
		return r.setPXE(mid, a.Flag)
	case kPatch:
		return r.patch(mid, a.Text)
	case kStoreCommData:
		return r.storeCommission(mid, opsmCommData[a.Data])
	case kCreateLifecycle, kCreateMachineOp:
		if _, ok := r.machines[mid]; !ok {
			return opsmObs{Err: "notfound"}
		}
		return r.createMachineOp(mid, a.Type, opsmParams[a.Params], a.Allowed, minted)
	case kCreatePower:
		return r.createPowerOp(mid, a.Type, opsmParams[a.Params], minted)
	case kCreateRaw:
		return r.createOpRaw(a.NewID, mid, a.Type, opsmParams[a.Params])
	case kClaim:
		return r.claim(a.Op, a.Worker)
	case kSetPhase:
		return r.setPhase(a.Op, a.Phase)
	case kHubComplete:
		o, ok := r.ops[a.Op]
		if !ok {
			return opsmObs{Err: "notfound"}
		}
		return r.completeWithState(a.Op, opsmHubCompletionTarget(o.typ), opsmResults[a.Result])
	case kComplete:
		return r.completeWithState(a.Op, a.State, opsmResults[a.Result])
	case kCompletePlain:
		return r.completePlain(a.Op, opsmResults[a.Result])
	case kCompleteActive:
		return r.completeActive(mid, a.Type, a.State, opsmResults[a.Result])
	case kFail:
		return r.fail(a.Op, opsmResults[a.Result])
	case kCommit:
		return r.commit(a.Op, mid2, opsmCommData[a.Data])
	case kValidate:
		return r.validateBinding(a.Op, mid2, a.Type, opsmCommData[a.Data])
	case kAbort:
		if _, ok := r.machines[mid]; !ok {
			return opsmObs{Err: "notfound"}
		}
		return r.abort(mid)
	case kDriftFail:
		return r.failInFlight(mid, a.Text)
	case kBackdate:
		r.backdate(a.Op, a.Field)
		return opsmObs{}
	case kExpirePending:
		return r.expirePending(a.Window)
	case kExpireStuck:
		return r.expireStuck(a.Window)
	case kExpireOrphans:
		return r.expireOrphans(a.Window)
	case kExpirePhoneHome:
		return r.expirePhoneHome(a.Window)
	case kScrub:
		return r.scrub()
	case kListActive:
		var o opsmObs
		for _, op := range r.sortedOps() {
			if refActive(op.status) {
				o.Ops = append(o.Ops, op.view())
			}
		}
		return o
	}
	panic("opsmApplyRef: unknown action kind " + string(a.Kind))
}

func opsmSerialFor(slot int) string { return opsmSerials[slot] }

// opsmNormalizeObs erases differences that are not part of the contract: the ID
// of a row the store minted itself when the call failed in one world only is
// still caught through Err.
func opsmNormalizeObs(o opsmObs) opsmObs {
	if len(o.Ops) == 0 {
		o.Ops = nil
	}
	if len(o.MStates) == 0 {
		o.MStates = nil
	}
	return o
}

// step applies one action to the real store and to the model, compares the
// observations, updates the slot/op bookkeeping, and runs every check.
func (h *opsmHarness) step(a opsmAction) {
	h.t.Helper()
	h.steps++
	h.trace = append(h.trace, a.String())
	mid, mid2 := h.machineOrPhantom(a.Slot), h.machineOrPhantom(a.Slot2)

	got := opsmNormalizeObs(h.applyReal(h.st, a))
	want := opsmNormalizeObs(opsmApplyRef(h.ref, a, mid, mid2, got))
	if !reflect.DeepEqual(got, want) {
		h.failf("step %d %s: observation differs from the reference model\n  store: %+v\n  model: %+v", h.steps, a, got, want)
	}

	// Bookkeeping driven by the MODEL's view of what now exists.
	switch a.Kind {
	case kMirrorCreate, kRawCreate:
		if _, ok := h.ref.machines[a.NewID]; ok && h.slots[a.Slot] == uuid.Nil {
			h.slots[a.Slot] = a.NewID
		}
	case kMirrorDelete, kDeleteIfIdle:
		if id := h.slots[a.Slot]; id != uuid.Nil {
			if _, ok := h.ref.machines[id]; !ok {
				h.slots[a.Slot] = uuid.Nil
			}
		}
	case kCreateLifecycle, kCreateMachineOp, kCreatePower, kCreateRaw:
		if want.Err == "" && len(want.Ops) == 1 {
			h.opIDs = append(h.opIDs, want.Ops[0].ID)
		}
	default: // every other kind leaves the slot and op bookkeeping alone
	}
	if !h.noReadback {
		h.check(a)
	}
}

func (h *opsmHarness) failf(format string, args ...any) {
	h.t.Helper()
	msg := fmt.Sprintf(format, args...)
	h.t.Fatalf("%s\n--- history (%d steps) ---\n%s", msg, len(h.trace), strings.Join(h.trace, "\n"))
}

// --- database read-back ---------------------------------------------------------

type opsmDBMachine struct {
	ID             uuid.UUID
	NetboxSerial   string     `gorm:"column:netbox_serial"`
	NetboxDeviceID *int       `gorm:"column:netbox_device_id"`
	State          string     `gorm:"column:state"`
	Locked         bool       `gorm:"column:locked"`
	PxeEligible    bool       `gorm:"column:pxe_eligible"`
	CommissionData *string    `gorm:"column:commission_data"`
	CommissionedAt *time.Time `gorm:"column:commissioned_at"`
	DesiredOS      *string    `gorm:"column:desired_os"`
}

type opsmDBOp struct {
	ID             uuid.UUID
	MachineID      uuid.UUID  `gorm:"column:machine_id"`
	TraceID        uuid.UUID  `gorm:"column:trace_id"`
	Type           string     `gorm:"column:type"`
	Status         string     `gorm:"column:status"`
	Phase          *string    `gorm:"column:phase"`
	PhaseEnteredAt time.Time  `gorm:"column:phase_entered_at"`
	ClaimedBy      *string    `gorm:"column:claimed_by"`
	ClaimedAt      *time.Time `gorm:"column:claimed_at"`
	CreatedAt      *time.Time `gorm:"column:created_at"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
	Params         *string    `gorm:"column:params"`
	Result         *string    `gorm:"column:result"`
	Version        int        `gorm:"column:version"`
}

type opsmDBEnvelope struct {
	ID        int64
	OpID      *uuid.UUID `gorm:"column:op_id"`
	MachineID *uuid.UUID `gorm:"column:machine_id"`
	TraceID   *uuid.UUID `gorm:"column:trace_id"`
	Msg       string     `gorm:"column:msg"`
	Source    string     `gorm:"column:source"`
	Host      *string    `gorm:"column:host"`
	Payload   string     `gorm:"column:payload"`
}

func opsmDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *opsmHarness) readDB() (map[uuid.UUID]opsmDBMachine, map[uuid.UUID]opsmDBOp, map[uuid.UUID][]opsmDBEnvelope) {
	var ms []opsmDBMachine
	if err := h.st.db.Raw(`SELECT id, netbox_serial, netbox_device_id, state, locked, pxe_eligible,
		commission_data::text AS commission_data, commissioned_at, desired_os FROM machines`).Scan(&ms).Error; err != nil {
		h.t.Fatalf("read machines: %v", err)
	}
	var os []opsmDBOp
	if err := h.st.db.Raw(`SELECT id, machine_id, trace_id, type, status, phase, phase_entered_at,
		claimed_by, claimed_at, created_at, completed_at, params::text AS params, result::text AS result, version
		FROM operations`).Scan(&os).Error; err != nil {
		h.t.Fatalf("read operations: %v", err)
	}
	var es []opsmDBEnvelope
	if err := h.st.db.Raw(`SELECT id, op_id, machine_id, trace_id, msg, source, host, payload::text AS payload
		FROM log_envelopes ORDER BY id`).Scan(&es).Error; err != nil {
		h.t.Fatalf("read log_envelopes: %v", err)
	}
	mm := make(map[uuid.UUID]opsmDBMachine, len(ms))
	for _, m := range ms {
		mm[m.ID] = m
	}
	om := make(map[uuid.UUID]opsmDBOp, len(os))
	for _, o := range os {
		om[o.ID] = o
	}
	em := map[uuid.UUID][]opsmDBEnvelope{}
	for _, e := range es {
		key := uuid.Nil
		if e.OpID != nil {
			key = *e.OpID
		}
		em[key] = append(em[key], e)
	}
	return mm, om, em
}

// opsmStateDiff compares database rows with a reference model and returns a
// description of the first difference, or "" when they agree. Timestamps are
// not compared here (see check for the moved/not-moved assertions).
func opsmStateDiff(ref *refModel, machines map[uuid.UUID]opsmDBMachine, ops map[uuid.UUID]opsmDBOp, envs map[uuid.UUID][]opsmDBEnvelope) string {
	if len(machines) != len(ref.machines) {
		return fmt.Sprintf("%d machine rows, model has %d", len(machines), len(ref.machines))
	}
	for id, want := range ref.machines {
		got, ok := machines[id]
		if !ok {
			return fmt.Sprintf("machine %s missing from the database", opsmShort(id))
		}
		gotDev := 0
		if got.NetboxDeviceID != nil {
			gotDev = *got.NetboxDeviceID
		}
		if got.State != want.state || got.Locked != want.locked || got.PxeEligible != want.pxe ||
			got.NetboxSerial != want.serial || gotDev != want.deviceID ||
			opsmCanonJSON(opsmDeref(got.CommissionData)) != want.comm ||
			(got.CommissionedAt != nil) != want.commissioned ||
			opsmDeref(got.DesiredOS) != want.desiredOS {
			return fmt.Sprintf("machine %s differs from the reference model\n  store: state=%s locked=%v pxe=%v serial=%s dev=%d comm=%s commissioned_at_set=%v desired_os=%q\n  model: state=%s locked=%v pxe=%v serial=%s dev=%d comm=%s commissioned_at_set=%v desired_os=%q",
				opsmShort(id),
				got.State, got.Locked, got.PxeEligible, got.NetboxSerial, gotDev, opsmCanonJSON(opsmDeref(got.CommissionData)), got.CommissionedAt != nil, opsmDeref(got.DesiredOS),
				want.state, want.locked, want.pxe, want.serial, want.deviceID, want.comm, want.commissioned, want.desiredOS)
		}
	}
	if len(ops) != len(ref.ops) {
		return fmt.Sprintf("%d operation rows, model has %d", len(ops), len(ref.ops))
	}
	for id, want := range ref.ops {
		got, ok := ops[id]
		if !ok {
			return fmt.Sprintf("op %s missing from the database", opsmShort(id))
		}
		if got.MachineID != want.machine || got.Type != want.typ || got.Status != want.status ||
			opsmDeref(got.Phase) != want.phase || got.Version != want.version ||
			opsmDeref(got.ClaimedBy) != want.claimedBy ||
			(got.ClaimedAt != nil) != want.claimed || (got.CompletedAt != nil) != want.completed ||
			opsmCanonJSON(opsmDeref(got.Result)) != want.result || opsmCanonJSON(opsmDeref(got.Params)) != want.params {
			return fmt.Sprintf("op %s differs from the reference model\n  store: machine=%s type=%s status=%s phase=%q version=%d claimed_by=%q claimed_at_set=%v completed_at_set=%v result=%s params=%s\n  model: machine=%s type=%s status=%s phase=%q version=%d claimed_by=%q claimed_at_set=%v completed_at_set=%v result=%s params=%s",
				opsmShort(id),
				opsmShort(got.MachineID), got.Type, got.Status, opsmDeref(got.Phase), got.Version, opsmDeref(got.ClaimedBy), got.ClaimedAt != nil, got.CompletedAt != nil, opsmCanonJSON(opsmDeref(got.Result)), opsmCanonJSON(opsmDeref(got.Params)),
				opsmShort(want.machine), want.typ, want.status, want.phase, want.version, want.claimedBy, want.claimed, want.completed, want.result, want.params)
		}
		if got.CreatedAt == nil {
			return fmt.Sprintf("op %s has NULL created_at", opsmShort(id))
		}
		// The op-event log (log_envelopes) is a faithful, ordered record.
		es := envs[id]
		if len(es) != len(want.events) {
			return fmt.Sprintf("op %s has %d global envelopes, model expects %d (%+v)\n  store: %s",
				opsmShort(id), len(es), len(want.events), want.events, opsmDescribeEnvs(es))
		}
		for i, e := range es {
			if ev := opsmDecodeEnvelope(e); ev != want.events[i] {
				return fmt.Sprintf("op %s envelope %d is %+v, model expects %+v", opsmShort(id), i, ev, want.events[i])
			}
			if e.MachineID == nil || *e.MachineID != got.MachineID || e.TraceID == nil || *e.TraceID != got.TraceID ||
				e.Source != "global" {
				return fmt.Sprintf("op %s envelope %d carries the wrong identity: %+v", opsmShort(id), i, e)
			}
		}
	}
	for id, es := range envs {
		if _, ok := ref.ops[id]; !ok {
			return fmt.Sprintf("%d envelopes reference op %s, which the model says does not exist", len(es), opsmShort(id))
		}
	}
	return ""
}

// opsmStructuralDiff checks the invariants that need no model: at most one
// active op per machine and, for production-reachable histories, machine
// state explained by the machine's operations.
func opsmStructuralDiff(strict bool, machines map[uuid.UUID]opsmDBMachine, ops map[uuid.UUID]opsmDBOp) string {
	activePer := map[uuid.UUID]int{}
	for _, o := range ops {
		if refActive(o.Status) {
			activePer[o.MachineID]++
		}
	}
	for mid, n := range activePer {
		if n > 1 {
			return fmt.Sprintf("machine %s has %d active operations", opsmShort(mid), n)
		}
	}
	if strict {
		return opsmStateExplainedDiff(machines, ops)
	}
	return ""
}

// check compares the whole database state with the reference model and
// asserts the model-independent invariants.
func (h *opsmHarness) check(a opsmAction) {
	h.t.Helper()
	machines, ops, envs := h.readDB()
	if d := opsmStateDiff(h.ref, machines, ops, envs); d != "" {
		h.failf("after %s: %s", a, d)
	}
	for id, want := range h.ref.ops {
		got := ops[id]
		// Timestamps move exactly when the model says the clock was reset.
		gen := [3]int{want.createdGen, want.claimedGen, want.phaseGen}
		if prev, seen := h.prev[id]; seen {
			pg := h.prevG[id]
			h.checkMoved(a, id, "created_at", prev.CreatedAt, got.CreatedAt, pg[0] != gen[0])
			h.checkMoved(a, id, "claimed_at", prev.ClaimedAt, got.ClaimedAt, pg[1] != gen[1])
			h.checkMoved(a, id, "phase_entered_at", &prev.PhaseEnteredAt, &got.PhaseEnteredAt, pg[2] != gen[2])

			// Model-independent invariants against the previous row.
			if got.Version < prev.Version {
				h.failf("after %s: op %s version went backwards: %d -> %d", a, opsmShort(id), prev.Version, got.Version)
			}
			if got.Version > prev.Version+1 {
				h.failf("after %s: op %s version jumped by more than one in a single call: %d -> %d", a, opsmShort(id), prev.Version, got.Version)
			}
			if got.TraceID != prev.TraceID || got.Type != prev.Type || got.MachineID != prev.MachineID {
				h.failf("after %s: op %s identity columns changed", a, opsmShort(id))
			}
			// (A Backdate step is the harness itself rewriting a clock
			// column, so it is exempt from the frozen-row check.)
			if refTerminal(prev.Status) && a.Kind != kBackdate {
				// Terminal status is immutable: status, result and the
				// completion stamp never change again.
				if got.Status != prev.Status || opsmDeref(got.Result) != opsmDeref(prev.Result) ||
					!opsmTimePtrEqual(got.CompletedAt, prev.CompletedAt) ||
					opsmDeref(got.ClaimedBy) != opsmDeref(prev.ClaimedBy) || !opsmTimePtrEqual(got.ClaimedAt, prev.ClaimedAt) {
					h.failf("after %s: terminal op %s was rewritten: %+v -> %+v", a, opsmShort(id), prev, got)
				}
				if got.Version != prev.Version || opsmDeref(got.Phase) != opsmDeref(prev.Phase) ||
					!got.PhaseEnteredAt.Equal(prev.PhaseEnteredAt) {
					h.failf("after %s: terminal op %s row changed after it went terminal: version %d -> %d, phase %q -> %q",
						a, opsmShort(id), prev.Version, got.Version, opsmDeref(prev.Phase), opsmDeref(got.Phase))
				}
			}
		}
		h.prev[id] = got
		h.prevG[id] = gen
	}
	for id := range h.prev {
		if _, ok := h.ref.ops[id]; !ok {
			delete(h.prev, id)
			delete(h.prevG, id)
		}
	}
	if a.Kind == kScrub {
		for _, o := range ops {
			if refTerminal(o.Status) && opsmHasCredential(opsmDeref(o.Params)) {
				h.failf("after %s: terminal op %s still holds a credential: %s", a, opsmShort(o.ID), opsmDeref(o.Params))
			}
		}
	}
	if d := opsmStructuralDiff(h.strict, machines, ops); d != "" {
		h.failf("after %s: %s", a, d)
	}
}

// opsmStateExplainedDiff asserts that machine.state is explained by the
// machine's operations: a machine in a transient state has exactly one
// claimed/running lifecycle op of the matching type, and every
// claimed/running lifecycle op has its machine in the matching transient
// state. Only meaningful for histories built from production-reachable
// calls.
func opsmStateExplainedDiff(machines map[uuid.UUID]opsmDBMachine, ops map[uuid.UUID]opsmDBOp) string {
	driving := map[uuid.UUID][]opsmDBOp{}
	for _, o := range ops {
		if (o.Type == "deploy" || o.Type == "commission") && (o.Status == "claimed" || o.Status == "running") {
			driving[o.MachineID] = append(driving[o.MachineID], o)
		}
	}
	transient := map[string]string{"deploying": "deploy", "commissioning": "commission"}
	for id, m := range machines {
		d := driving[id]
		if len(d) > 1 {
			return fmt.Sprintf("machine %s has %d lifecycle ops driving it", opsmShort(id), len(d))
		}
		if wantType, isTransient := transient[m.State]; isTransient {
			if len(d) != 1 || d[0].Type != wantType {
				return fmt.Sprintf("machine %s is %s with no claimed/running %s op driving it (driving ops: %d): stranded transient state",
					opsmShort(id), m.State, wantType, len(d))
			}
		} else if len(d) == 1 {
			return fmt.Sprintf("%s op %s is %s but its machine %s is in state %s",
				d[0].Type, opsmShort(d[0].ID), d[0].Status, opsmShort(id), m.State)
		}
	}
	return ""
}

func (h *opsmHarness) checkMoved(a opsmAction, id uuid.UUID, col string, prev, got *time.Time, wantMoved bool) {
	h.t.Helper()
	moved := !opsmTimePtrEqual(prev, got)
	if moved != wantMoved {
		h.failf("after %s: op %s column %s moved=%v, model expects moved=%v (prev=%v now=%v)",
			a, opsmShort(id), col, moved, wantMoved, prev, got)
	}
}

func opsmTimePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func opsmDecodeEnvelope(e opsmDBEnvelope) refEvent {
	var p struct {
		Extra struct {
			Status    string `json:"status"`
			ClaimedBy string `json:"claimed_by"`
			Error     string `json:"error"`
		} `json:"extra"`
	}
	_ = json.Unmarshal([]byte(e.Payload), &p)
	return refEvent{Msg: e.Msg, Status: p.Extra.Status, ClaimedBy: p.Extra.ClaimedBy, Err: p.Extra.Error}
}

func opsmDescribeEnvs(es []opsmDBEnvelope) string {
	var parts []string
	for _, e := range es {
		parts = append(parts, fmt.Sprintf("%+v", opsmDecodeEnvelope(e)))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// --- coverage aggregation -------------------------------------------------------

var opsmCoverage = struct {
	sync.Mutex
	tags  map[string]int
	cases int
	steps int
}{tags: map[string]int{}}

func (h *opsmHarness) recordCoverage() {
	opsmCoverage.Lock()
	defer opsmCoverage.Unlock()
	opsmCoverage.cases++
	opsmCoverage.steps += h.steps
	for k, v := range h.ref.cov {
		opsmCoverage.tags[k] += v
	}
}

func opsmCoverageReport() string {
	opsmCoverage.Lock()
	defer opsmCoverage.Unlock()
	keys := make([]string, 0, len(opsmCoverage.tags))
	for k := range opsmCoverage.tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "cases=%d steps=%d\n", opsmCoverage.cases, opsmCoverage.steps)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-48s %d\n", k, opsmCoverage.tags[k])
	}
	return b.String()
}
