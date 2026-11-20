//go:build integration

package qemutest

// propGlobal is the lifecycle property tests' stand-in for
// `slagworksd global`.
//
// FakeHub (hub.go) records what a worker sends but does not behave like
// global: it never echoes an op_delta for a phase update, never publishes
// op_removed, never rebuilds the identity snapshot after a durable state
// write. History tests need the control loop closed, so propGlobal runs:
//
//   - the REAL ws.Hub (wire protocol, hello/resync barrier, claim RPC,
//     op-stream fan-out),
//   - the REAL hubhandlers.OpUpdate (the production op_update handler),
//   - an in-memory reference model of the Postgres store, whose transition
//     DECISIONS are internal/opstate's (the same functions the real store
//     calls inside its transactions) and whose SQL-shaped rules (CAS on
//     terminal statuses, no phase write on a terminal op, one active op per
//     machine, version bump per mutation, the four sweeps and the predicate
//     each re-checks under the row lock) are re-expressed here from
//     internal/store/operations.go and docs/dev/state-machine.md,
//   - a reference model of the observation handler (internal/commission)
//     minus NetBox/AWS: commission_report commits the inventory and
//     completes the op, deploy_complete validates the binding, phone_home
//     completes the deploy it names,
//   - the API server's publish rules for create/abort/sweep/lock
//     (internal/api/server.go).
//
// Every store mutation is appended to an audit trail the oracle reads.
// The model is a test oracle, not a product: it is deliberately a map and
// a mutex.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/hubhandlers"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Production sweep tiers (internal/api/server.go defaults).
const (
	propPendingOpTimeout   = 24 * time.Hour
	propOpTimeout          = 30 * time.Minute
	propPhoneHomeTimeout   = 30 * time.Minute
	propOrphanedClaimGrace = 2 * time.Minute
)

// propMachine is the model's machines row plus the identity facts the
// snapshot builder would read from NetBox.
type propMachine struct {
	ID             uuid.UUID
	Name           string
	MAC            string
	State          model.MachineState
	Locked         bool
	PXEEligible    bool
	CommissionData json.RawMessage
	SystemUUID     string
	BMCIP          string
	StorageScript  *model.SnapshotStorageScript
	// InSnapshot is false for a machine global knows nothing about at the
	// site (the "unknown MAC" bystander).
	InSnapshot bool
}

// propHubEvent is one entry of the store audit trail.
type propHubEvent struct {
	At      time.Time
	Kind    string // op_created | claimed | reclaimed | phase | completed | failed | obs | push
	OpID    uuid.UUID
	Machine uuid.UUID
	OpType  model.OperationType
	Phase   string
	Cause   string // worker | abort | sweep_* | commission_report | api
	State   model.MachineState
	Detail  string
}

func (e propHubEvent) String() string {
	return fmt.Sprintf("%s %-10s op=%s type=%s phase=%q cause=%s state=%s %s",
		e.At.Format("15:04:05.000"), e.Kind, propShortID(e.OpID), e.OpType, e.Phase, e.Cause, e.State, e.Detail)
}

func propShortID(id uuid.UUID) string {
	if id == uuid.Nil {
		return "-"
	}
	return id.String()[:8]
}

type propGlobal struct {
	t    testing.TB
	slug string
	hub  *ws.Hub
	srv  *httptest.Server
	URL  string
	auth *FakeHub // reused only for its worker-token registry (fakeAuth)

	opUpdate *hubhandlers.OpUpdate
	pub      *propPublisher

	mu       sync.Mutex
	dhcp     model.DHCPConfig
	machines map[uuid.UUID]*propMachine
	mOrder   []uuid.UUID
	initial  map[uuid.UUID]model.MachineState
	ops      map[uuid.UUID]*model.Operation
	opOrder  []uuid.UUID
	seq      int64
	lastHash string
	skew     time.Duration
	events   []propHubEvent
	resyncs  []time.Time
	// hellos counts hellos whose op_resync is still to be built.
	hellos int
	// disconnects are the times the worker's connection was torn down on
	// purpose by a history step.
	disconnects []time.Time
	// lastProgress is the time of the last store mutation.
	lastProgress time.Time

	// dbLatency models the Postgres round trips behind every op mutation
	// global makes on the worker's word (claim: two row locks, the CAS, the
	// machine-state write, the claim log envelope; phase / complete / fail:
	// one transaction each). Zero in the default simulated profile.
	dbLatency time.Duration

	// down refuses WebSocket upgrades (global unreachable).
	down atomic.Bool
	// blackhole models a global that crashes after reading frames off the
	// wire but before committing: op_updates and observations are refused
	// uncommitted, and the worker keeps them in its durable outbox and
	// sends them again.
	blackhole atomic.Bool
	// holdPush queues identity pushes instead of sending them.
	holdPush   atomic.Bool
	heldPushMu sync.Mutex
	heldPush   []model.SiteSnapshot
}

func newPropGlobal(t testing.TB, slug string, dhcp model.DHCPConfig) *propGlobal {
	g := &propGlobal{
		t:        t,
		slug:     slug,
		dhcp:     dhcp,
		machines: make(map[uuid.UUID]*propMachine),
		initial:  make(map[uuid.UUID]model.MachineState),
		ops:      make(map[uuid.UUID]*model.Operation),
		seq:      1,
		auth: &FakeHub{
			workersByHash: make(map[string]*model.SiteWorker),
		},
	}
	g.hub = ws.NewHub(ws.HubConfig{
		Auth:               propAuth{fakeAuth: fakeAuth{fh: g.auth}, g: g},
		Rebuilder:          propRebuilder{g},
		ObsHandler:         propObsHandler{g},
		OpUpdateHandler:    propOpUpdateHandler{g},
		ActiveOps:          propActiveOps{g},
		OpClaimer:          propClaimer{g},
		RebuildTimeout:     5 * time.Second,
		ObservationTimeout: 10 * time.Second,
		OpUpdateTimeout:    5 * time.Second,
		LogBatchTimeout:    5 * time.Second,
		Version:            testVersion(),
	})
	g.pub = &propPublisher{hub: g.hub}
	g.opUpdate = hubhandlers.NewOpUpdate(propStore{g})
	g.opUpdate.SetPublisher(g.pub)
	g.opUpdate.SetInvalidator(g.invalidate)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ws/site", func(w http.ResponseWriter, r *http.Request) {
		if g.down.Load() {
			http.Error(w, "global unavailable", http.StatusServiceUnavailable)
			return
		}
		g.hub.ServeHTTP(w, r)
	})
	g.srv = httptest.NewServer(mux)
	g.URL = g.srv.URL
	t.Cleanup(func() {
		g.hub.Close()
		g.srv.Close()
	})
	return g
}

func (g *propGlobal) now() time.Time { return time.Now().Add(g.skew) }

// AdvanceClock moves the model's wall clock forward; only sweep cutoffs
// read it. This is how a history "waits 31 minutes".
func (g *propGlobal) AdvanceClock(d time.Duration) {
	g.mu.Lock()
	g.skew += d
	g.mu.Unlock()
}

// Skew is how far the model's clock has been advanced.
func (g *propGlobal) Skew() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.skew
}

func (g *propGlobal) record(e propHubEvent) {
	e.At = time.Now()
	g.events = append(g.events, e)
	if e.Kind != "obs" && e.Kind != "uncommitted_update" {
		g.lastProgress = e.At
	}
}

// LastProgress is the time of the last store mutation (not an observation
// or a dropped update).
func (g *propGlobal) LastProgress() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastProgress
}

// Events returns a copy of the audit trail.
func (g *propGlobal) Events() []propHubEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]propHubEvent(nil), g.events...)
}

func (g *propGlobal) AddMachine(m propMachine) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := m
	g.machines[m.ID] = &cp
	g.mOrder = append(g.mOrder, m.ID)
	g.initial[m.ID] = m.State
}

// MachineStateAt is the machine state global held at t: the state the last
// audit-trail entry for the machine at or before t recorded, or the state
// the machine was set up in.
func (g *propGlobal) MachineStateAt(id uuid.UUID, t time.Time) model.MachineState {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.initial[id]
	for _, e := range g.events {
		if e.At.After(t) {
			break
		}
		if e.Machine == id && e.State != "" {
			st = e.State
		}
	}
	return st
}

func (g *propGlobal) Machine(id uuid.UUID) propMachine {
	g.mu.Lock()
	defer g.mu.Unlock()
	return *g.machines[id]
}

func (g *propGlobal) Op(id uuid.UUID) *model.Operation {
	g.mu.Lock()
	defer g.mu.Unlock()
	if op, ok := g.ops[id]; ok {
		cp := *op
		return &cp
	}
	return nil
}

func (g *propGlobal) Ops() []model.Operation {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]model.Operation, 0, len(g.opOrder))
	for _, id := range g.opOrder {
		out = append(out, *g.ops[id])
	}
	return out
}

func propActive(s model.OperationStatus) bool {
	return s == model.OpPending || s == model.OpClaimed || s == model.OpRunning
}

func (g *propGlobal) ActiveOps(machine uuid.UUID) []model.Operation {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []model.Operation
	for _, id := range g.opOrder {
		op := g.ops[id]
		if propActive(op.Status) && (machine == uuid.Nil || op.MachineID == machine) {
			out = append(out, *op)
		}
	}
	return out
}

// --- identity snapshot ---

func (g *propGlobal) buildSnapshotLocked() model.SiteSnapshot {
	snap := model.SiteSnapshot{
		SeqNum: g.seq,
		Site:   model.SiteInfo{Slug: g.slug, Name: g.slug},
		DHCP:   g.dhcp,
	}
	for _, id := range g.mOrder {
		m := g.machines[id]
		if !m.InSnapshot {
			continue
		}
		snap.Machines = append(snap.Machines, model.MachineSnapshot{
			ID:            m.ID,
			Hostname:      m.Name,
			State:         m.State,
			Locked:        m.Locked,
			PXEEligible:   m.PXEEligible,
			Interfaces:    []model.SnapshotInterface{{Name: "eth0", MAC: m.MAC, Type: "physical"}},
			StorageScript: m.StorageScript,
			SystemUUID:    m.SystemUUID,
			BMCIP:         m.BMCIP,
			KernelParams:  "console=ttyS1,115200n8",
		})
	}
	return snap
}

// invalidate mirrors the dispatcher: rebuild, and push with a bumped
// SeqNum only when the content hash changed.
func (g *propGlobal) invalidate() {
	g.mu.Lock()
	snap := g.buildSnapshotLocked()
	h, err := snap.ContentHash()
	if err != nil {
		g.mu.Unlock()
		g.t.Errorf("propGlobal: content hash: %v", err)
		return
	}
	if h == g.lastHash {
		g.mu.Unlock()
		return
	}
	g.lastHash = h
	g.seq++
	snap.SeqNum = g.seq
	g.record(propHubEvent{Kind: "push", Detail: fmt.Sprintf("seq=%d", snap.SeqNum)})
	g.mu.Unlock()

	if g.holdPush.Load() {
		g.heldPushMu.Lock()
		g.heldPush = append(g.heldPush, snap)
		g.heldPushMu.Unlock()
		return
	}
	g.hub.Push(g.slug, snap)
}

// PrimeSnapshot seeds the hub's cached snapshot before any worker connects.
func (g *propGlobal) PrimeSnapshot() {
	g.mu.Lock()
	snap := g.buildSnapshotLocked()
	g.lastHash, _ = snap.ContentHash()
	g.mu.Unlock()
	g.hub.Push(g.slug, snap)
}

// ReleasePushes sends identity pushes queued under holdPush.
func (g *propGlobal) ReleasePushes() {
	g.holdPush.Store(false)
	g.heldPushMu.Lock()
	held := g.heldPush
	g.heldPush = nil
	g.heldPushMu.Unlock()
	for _, s := range held {
		g.hub.Push(g.slug, s)
	}
}

type propRebuilder struct{ g *propGlobal }

func (r propRebuilder) RebuildAndWait(_ context.Context, _ string) (model.SiteSnapshot, error) {
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	return r.g.buildSnapshotLocked(), nil
}

// --- store model: hubhandlers.Store ---

type propStore struct{ g *propGlobal }

var errPropNotFound = errors.New("record not found")

func (s propStore) GetOperationIfExists(id uuid.UUID) (*model.Operation, error) {
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	op, ok := s.g.ops[id]
	if !ok {
		return nil, nil
	}
	cp := *op
	return &cp, nil
}

// SetOperationPhase mirrors store.SetOperationPhase: a terminal op is left
// as it is, and phase_entered_at and version move only on a real change.
func (s propStore) SetOperationPhase(opID uuid.UUID, phase string) (store.PhaseTransition, error) {
	g := s.g
	time.Sleep(g.dbLatency / 4)
	g.mu.Lock()
	defer g.mu.Unlock()
	op, ok := g.ops[opID]
	if !ok {
		return store.PhaseTransition{}, errPropNotFound
	}
	if op.Phase == phase || !propActive(op.Status) {
		return store.PhaseTransition{Changed: false, NewPhase: phase, MachineID: op.MachineID, TraceID: op.TraceID}, nil
	}
	now := g.now()
	prev, prevAt := op.Phase, op.PhaseEnteredAt
	op.Phase = phase
	op.PhaseEnteredAt = now
	op.Version++
	cp := *op
	g.record(propHubEvent{Kind: "phase", OpID: op.ID, Machine: op.MachineID, OpType: op.Type, Phase: phase,
		Cause: "worker", State: g.machines[op.MachineID].State, Detail: "status=" + string(op.Status)})
	return store.PhaseTransition{
		Changed: true, PrevPhase: prev, NewPhase: phase, Duration: now.Sub(prevAt),
		MachineID: op.MachineID, TraceID: op.TraceID, Op: &cp,
	}, nil
}

func (s propStore) CompleteOperationWithState(opID uuid.UUID, target model.MachineState, result []byte) (*store.OpOutcome, error) {
	return s.g.completeOp(opID, target, result, "worker")
}

func (s propStore) FailOperationOutcome(opID uuid.UUID, result []byte) (*store.OpOutcome, error) {
	return s.g.failOp(opID, result, "worker", nil)
}

func (g *propGlobal) setMachineStateLocked(id uuid.UUID, target model.MachineState) {
	m := g.machines[id]
	cols := opstate.StateColumns(target)
	m.State = target
	if v, ok := cols["locked"]; ok {
		m.Locked = v.(bool)
	}
}

func (g *propGlobal) completeOp(opID uuid.UUID, target model.MachineState, result []byte, cause string) (*store.OpOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	op, ok := g.ops[opID]
	if !ok || !propActive(op.Status) {
		return nil, nil
	}
	now := g.now()
	op.Status = model.OpCompleted
	op.CompletedAt = &now
	op.Result = result
	op.Version++
	if target != "" {
		g.setMachineStateLocked(op.MachineID, target)
	}
	g.record(propHubEvent{Kind: "completed", OpID: op.ID, Machine: op.MachineID, OpType: op.Type, Phase: op.Phase,
		Cause: cause, State: g.machines[op.MachineID].State})
	return &store.OpOutcome{Op: *op, MachineState: target}, nil
}

// failOp mirrors store.failOperationGuarded. guard, when non-nil, is
// re-evaluated under the lock (the orphaned-claim sweep's predicate).
func (g *propGlobal) failOp(opID uuid.UUID, result []byte, cause string, guard func(*model.Operation) bool) (*store.OpOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	op, ok := g.ops[opID]
	if !ok || !propActive(op.Status) {
		return nil, nil
	}
	if guard != nil && !guard(op) {
		return nil, nil
	}
	wasPending := op.Status == model.OpPending
	now := g.now()
	op.Status = model.OpFailed
	op.CompletedAt = &now
	op.Result = result
	op.Version++
	m := g.machines[op.MachineID]
	out := opstate.FailStateOutcome(wasPending, op.Type, len(m.CommissionData) > 0)
	var st model.MachineState
	if out.ChangeState {
		g.setMachineStateLocked(op.MachineID, out.Target)
		st = out.Target
	}
	g.record(propHubEvent{Kind: "failed", OpID: op.ID, Machine: op.MachineID, OpType: op.Type, Phase: op.Phase,
		Cause: cause, State: m.State, Detail: string(result)})
	return &store.OpOutcome{Op: *op, MachineState: st}, nil
}

// --- ws.OpClaimer ---

type propClaimer struct{ g *propGlobal }

func (c propClaimer) ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error) {
	g := c.g
	time.Sleep(g.dbLatency)
	g.mu.Lock()
	defer g.mu.Unlock()
	op, ok := g.ops[opID]
	if !ok {
		return nil, nil
	}
	now := g.now()
	if opstate.CanReclaim(op, workerID) {
		op.ClaimedAt = &now
		op.Version++
		cp := *op
		g.record(propHubEvent{Kind: "reclaimed", OpID: op.ID, Machine: op.MachineID, OpType: op.Type, Cause: "worker",
			State: g.machines[op.MachineID].State})
		return &cp, nil
	}
	if op.Status != model.OpPending {
		return nil, nil
	}
	target, resting := opstate.ClaimTransition(op.Type)
	if len(resting) > 0 {
		m := g.machines[op.MachineID]
		if err := opstate.CheckClaimPreconditions(op.MachineID, op.Type, m.State, m.Locked, resting); err != nil {
			return nil, err
		}
	}
	op.Status = model.OpClaimed
	op.ClaimedBy = workerID
	op.ClaimedAt = &now
	op.Version++
	if target != "" {
		g.setMachineStateLocked(op.MachineID, target)
	}
	cp := *op
	g.record(propHubEvent{Kind: "claimed", OpID: op.ID, Machine: op.MachineID, OpType: op.Type, Cause: "worker",
		State: g.machines[op.MachineID].State})
	return &cp, nil
}

func (c propClaimer) GetOperationIfExists(opID uuid.UUID) (*model.Operation, error) {
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if op, ok := c.g.ops[opID]; ok {
		cp := *op
		return &cp, nil
	}
	return nil, nil
}

type propActiveOps struct{ g *propGlobal }

// ListActiveOperations is called by the hub to build a hello's op_resync,
// and again after every snapshot push to re-send the active ops as
// op_deltas. Only the first is a resync (it full-replaces the worker's op
// set; a re-send removes nothing), so a call counts as one only while a
// hello is waiting for it (propAuth). The call time is the resync
// barrier's timestamp.
func (a propActiveOps) ListActiveOperations() ([]model.Operation, error) {
	g := a.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hellos > 0 {
		g.hellos--
		g.resyncs = append(g.resyncs, time.Now())
	}
	var out []model.Operation
	for _, id := range g.opOrder {
		if op := g.ops[id]; propActive(op.Status) {
			out = append(out, *op)
		}
	}
	return out, nil
}

// propAuth is the worker-token registry, plus a count of hellos whose
// op_resync has not been built yet: the hub persists the worker's serial
// (a non-nil serial) on a hello and only there, before the resync.
type propAuth struct {
	fakeAuth
	g *propGlobal
}

func (a propAuth) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	if serial != nil {
		a.g.mu.Lock()
		a.g.hellos++
		a.g.mu.Unlock()
	}
	return a.fakeAuth.TouchSiteWorker(slug, workerID, fqdn, serial)
}

// Resyncs returns the times at which a worker was sent an op_resync.
func (g *propGlobal) Resyncs() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Time(nil), g.resyncs...)
}

// MarkDisconnect records that the worker's connection is going away (worker
// kill, WebSocket drop, hub outage). Until the next resync there is no
// connection a late op message could legitimately arrive on.
func (g *propGlobal) MarkDisconnect() {
	g.mu.Lock()
	g.disconnects = append(g.disconnects, time.Now())
	g.mu.Unlock()
}

// connLive reports whether one resynced connection carried the worker
// through the whole of [from, to]: a resync precedes from with no
// disconnect after it up to to.
func (g *propGlobal) connLive(from, to time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	var rs time.Time
	for _, t := range g.resyncs {
		if !t.After(from) {
			rs = t
		}
	}
	if rs.IsZero() {
		return false
	}
	for _, d := range g.disconnects {
		if d.After(rs) && !d.After(to) {
			return false
		}
	}
	return true
}

// connSince is the resync time of the worker's current connection, and
// whether there is one: false between a disconnect and the resync that
// follows it.
func (g *propGlobal) connSince() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.resyncs) == 0 {
		return time.Time{}, false
	}
	last := g.resyncs[len(g.resyncs)-1]
	if n := len(g.disconnects); n > 0 && !last.After(g.disconnects[n-1]) {
		return time.Time{}, false
	}
	return last, true
}

// --- inbound handlers ---

type propOpUpdateHandler struct{ g *propGlobal }

func (h propOpUpdateHandler) HandleOpUpdate(ctx context.Context, msg ws.OpUpdateMsg) error {
	if h.g.blackhole.Load() {
		id, _ := uuid.Parse(msg.OperationID)
		h.g.mu.Lock()
		h.g.record(propHubEvent{Kind: "uncommitted_update", OpID: id, Phase: msg.Phase, Cause: "blackhole",
			Detail: "status=" + msg.Status})
		h.g.mu.Unlock()
		return errors.New("propGlobal: blackhole (not committed)")
	}
	return h.g.opUpdate.HandleOpUpdate(ctx, msg)
}

type propObsHandler struct{ g *propGlobal }

// HandleObservation models internal/commission.Handler without NetBox/AWS.
func (h propObsHandler) HandleObservation(_ context.Context, obs model.Observation) error {
	g := h.g
	if g.blackhole.Load() {
		return errors.New("propGlobal: blackhole (not committed)")
	}
	g.mu.Lock()
	g.record(propHubEvent{Kind: "obs", OpID: obs.OperationID, Machine: obs.MachineID, Detail: obs.Event})
	g.mu.Unlock()
	switch obs.Event {
	case "commission_report":
		outcome, err := g.commitCommission(obs)
		if err != nil {
			return err
		}
		if outcome != nil {
			g.pub.PublishOpRemoved(outcome.Op, outcome.MachineState)
		}
		g.invalidate()
		return nil
	case "deploy_complete":
		g.mu.Lock()
		defer g.mu.Unlock()
		_, err := g.validateBindingLocked(obs.OperationID, obs.MachineID, model.OpDeploy, nil)
		return err
	case "phone_home":
		return g.phoneHomeObservation(obs)
	default:
		return nil
	}
}

// phoneHomeObservation mirrors commission.Handler.handlePhoneHome: the
// durable phone_home observation completes the deploy it names, whichever
// of it and the worker's op_update{completed} arrives first. One with no
// operation ID (a late phone-home) is acked without effect.
func (g *propGlobal) phoneHomeObservation(obs model.Observation) error {
	if obs.OperationID == uuid.Nil {
		return nil
	}
	g.mu.Lock()
	already, err := g.validateBindingLocked(obs.OperationID, obs.MachineID, model.OpDeploy, nil)
	g.mu.Unlock()
	if err != nil || already {
		return err
	}
	outcome, _ := g.completeOp(obs.OperationID, model.MachineStateRunning, nil, "phone_home")
	if outcome == nil {
		return nil
	}
	g.pub.PublishOpRemoved(outcome.Op, outcome.MachineState)
	g.invalidate()
	return nil
}

// validateBindingLocked mirrors store.validateOperationBinding. Binding
// rejections are terminal (the worker drops the outbox row).
func (g *propGlobal) validateBindingLocked(opID, machineID uuid.UUID, want model.OperationType, result json.RawMessage) (bool, error) {
	term := func(format string, args ...any) error {
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ws.ErrTerminal)
	}
	if opID == uuid.Nil || machineID == uuid.Nil {
		return false, term("operation and machine IDs are required")
	}
	op, ok := g.ops[opID]
	if !ok {
		return false, term("operation %s not found", opID)
	}
	if op.MachineID != machineID {
		return false, term("operation %s belongs to machine %s, not %s", op.ID, op.MachineID, machineID)
	}
	if op.Type != want {
		return false, term("operation %s has type %s, want %s", op.ID, op.Type, want)
	}
	switch op.Status {
	case model.OpPending, model.OpClaimed, model.OpRunning:
		return false, nil
	case model.OpCompleted:
		if want == model.OpDeploy {
			return true, nil
		}
		if want == model.OpCommission && len(op.Result) > 0 && string(op.Result) == string(result) {
			return true, nil
		}
		return false, term("operation %s is already completed with a different result", op.ID)
	default:
		return false, term("operation %s is terminal (%s)", op.ID, op.Status)
	}
}

func (g *propGlobal) commitCommission(obs model.Observation) (*store.OpOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	already, err := g.validateBindingLocked(obs.OperationID, obs.MachineID, model.OpCommission, obs.Data)
	if err != nil {
		return nil, err
	}
	if already {
		return nil, nil
	}
	op := g.ops[obs.OperationID]
	m := g.machines[obs.MachineID]
	m.CommissionData = append(json.RawMessage(nil), obs.Data...)
	var inv struct {
		System *struct {
			UUID string `json:"uuid"`
		} `json:"system"`
	}
	if json.Unmarshal(obs.Data, &inv) == nil && inv.System != nil && inv.System.UUID != "" {
		m.SystemUUID = inv.System.UUID
	}
	now := g.now()
	op.Status = model.OpCompleted
	op.CompletedAt = &now
	op.Result = append(json.RawMessage(nil), obs.Data...)
	op.Version++
	g.setMachineStateLocked(m.ID, model.MachineStateCommissioned)
	g.record(propHubEvent{Kind: "completed", OpID: op.ID, Machine: m.ID, OpType: op.Type, Phase: op.Phase,
		Cause: "commission_report", State: m.State})
	return &store.OpOutcome{Op: *op, MachineState: model.MachineStateCommissioned}, nil
}

// --- operator API model ---

func (g *propGlobal) createOp(machineID uuid.UUID, opType model.OperationType, params model.OpParams, allowed ...model.MachineState) (*model.Operation, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	m, ok := g.machines[machineID]
	if !ok {
		g.mu.Unlock()
		return nil, fmt.Errorf("machine not found")
	}
	lifecycle := opType == model.OpCommission || opType == model.OpDeploy
	if lifecycle {
		if m.Locked {
			g.mu.Unlock()
			return nil, fmt.Errorf("machine is locked")
		}
		if !m.PXEEligible {
			g.mu.Unlock()
			return nil, fmt.Errorf("machine is not PXE-eligible")
		}
		okState := false
		for _, s := range allowed {
			if s == m.State {
				okState = true
			}
		}
		if !okState {
			g.mu.Unlock()
			return nil, fmt.Errorf("machine is in state %q; %s requires one of %v", m.State, opType, allowed)
		}
	}
	// idx_one_active_op_per_machine: one active op per machine, any type.
	for _, id := range g.opOrder {
		if o := g.ops[id]; o.MachineID == machineID && propActive(o.Status) {
			g.mu.Unlock()
			return nil, fmt.Errorf("machine already has an active %s operation", o.Type)
		}
	}
	now := g.now()
	op := &model.Operation{
		ID: uuid.New(), MachineID: machineID, TraceID: uuid.New(), Type: opType,
		Status: model.OpPending, Params: raw, CreatedAt: now, PhaseEnteredAt: now,
	}
	g.ops[op.ID] = op
	g.opOrder = append(g.opOrder, op.ID)
	cp := *op
	g.record(propHubEvent{Kind: "op_created", OpID: op.ID, Machine: machineID, OpType: opType, Cause: "api", State: m.State})
	g.mu.Unlock()
	g.pub.PublishOpDelta(cp, "")
	return &cp, nil
}

func propBMCRuntime() *model.BMCRuntime {
	return &model.BMCRuntime{Username: FakeBMCUser, Password: FakeBMCPass}
}

func (g *propGlobal) CreateDeploy(machineID uuid.UUID, destructive bool, fatalAt string) (*model.Operation, error) {
	return g.createOp(machineID, model.OpDeploy,
		model.OpParams{Destructive: destructive, FatalAt: fatalAt, BMCRuntime: propBMCRuntime()},
		model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning)
}

func (g *propGlobal) CreateCommission(machineID uuid.UUID) (*model.Operation, error) {
	return g.createOp(machineID, model.OpCommission, model.OpParams{BMCRuntime: propBMCRuntime()},
		model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning)
}

func (g *propGlobal) CreatePower(machineID uuid.UUID, opType model.OperationType) (*model.Operation, error) {
	return g.createOp(machineID, opType, model.OpParams{BMCRuntime: propBMCRuntime()})
}

// publishOutcomes applies the API server's rule: op_removed per outcome,
// one identity rebuild if any wrote durable machine state.
func (g *propGlobal) publishOutcomes(outs []store.OpOutcome) {
	durable := false
	for _, o := range outs {
		g.pub.PublishOpRemoved(o.Op, o.MachineState)
		if o.MachineState != "" {
			durable = true
		}
	}
	if durable {
		g.invalidate()
	}
}

// Abort mirrors POST /machines/:id/abort.
func (g *propGlobal) Abort(machineID uuid.UUID) int {
	var outs []store.OpOutcome
	for _, op := range g.ActiveOps(machineID) {
		out, _ := g.failOp(op.ID, []byte(`{"error":"aborted by operator"}`), "abort", nil)
		if out != nil {
			outs = append(outs, *out)
		}
	}
	g.publishOutcomes(outs)
	return len(outs)
}

func (g *propGlobal) SetLocked(machineID uuid.UUID, locked bool) {
	g.mu.Lock()
	g.machines[machineID].Locked = locked
	g.mu.Unlock()
	g.invalidate()
}

// SetPXEEligible mirrors store.SetMachinePXEEligible: enabling PXE on a
// locked machine is refused.
func (g *propGlobal) SetPXEEligible(machineID uuid.UUID, eligible bool) error {
	g.mu.Lock()
	m := g.machines[machineID]
	if eligible && m.Locked {
		g.mu.Unlock()
		return fmt.Errorf("machine %s is locked", machineID)
	}
	m.PXEEligible = eligible
	g.mu.Unlock()
	g.invalidate()
	return nil
}

// RunSweeps is one tick of the API server's expiry ticker, in its order:
// pending, stuck, orphaned claims, phone-home. Each sweep selects its ops
// with no lock and then fails each one with the selecting predicate
// re-checked under the row lock (store.failOpBatch), so an op that moved on
// in between (claimed, entered awaiting_phone_home) is spared.
//
// tag prefixes the audit cause: "sweep" when a history step runs the sweep
// (expiry racing a live operation, on purpose), "timeout" when settle runs
// it because an operation was still active with the system at rest — the
// case where the documented timeout is the only thing left to end the op.
func (g *propGlobal) RunSweeps(tag string) int {
	claimed := func(o *model.Operation) bool { return o.Status == model.OpClaimed || o.Status == model.OpRunning }
	sweeps := []struct {
		cause  string
		reason string
		match  func(o *model.Operation, now time.Time) bool
	}{
		{"_pending", `{"error":"pending op timed out"}`, func(o *model.Operation, now time.Time) bool {
			return o.Status == model.OpPending && o.CreatedAt.Before(now.Add(-propPendingOpTimeout))
		}},
		{"_stuck", `{"error":"operation timed out"}`, func(o *model.Operation, now time.Time) bool {
			return claimed(o) && o.ClaimedAt != nil && o.ClaimedAt.Before(now.Add(-propOpTimeout)) && o.Phase != "awaiting_phone_home"
		}},
		{"_orphan", `{"error":"claiming worker disconnected before execution started"}`, func(o *model.Operation, now time.Time) bool {
			return o.Status == model.OpClaimed && o.Phase == "" && o.ClaimedAt != nil && o.ClaimedAt.Before(now.Add(-propOrphanedClaimGrace))
		}},
		{"_phone_home", `{"error":"phone-home timed out"}`, func(o *model.Operation, now time.Time) bool {
			return claimed(o) && o.Phase == "awaiting_phone_home" && o.PhaseEnteredAt.Before(now.Add(-propPhoneHomeTimeout))
		}},
	}
	n, durable := 0, false
	for _, sw := range sweeps {
		g.mu.Lock()
		now := g.now()
		var ids []uuid.UUID
		for _, id := range g.opOrder {
			if sw.match(g.ops[id], now) {
				ids = append(ids, id)
			}
		}
		g.mu.Unlock()
		guard := func(o *model.Operation) bool { return sw.match(o, now) }
		var outs []store.OpOutcome
		for _, id := range ids {
			if out, _ := g.failOp(id, []byte(sw.reason), tag+sw.cause, guard); out != nil {
				outs = append(outs, *out)
			}
		}
		for _, o := range outs {
			g.pub.PublishOpRemoved(o.Op, o.MachineState)
			durable = durable || o.MachineState != ""
		}
		n += len(outs)
	}
	if durable {
		g.invalidate()
	}
	return n
}

// --- op-stream publisher with fault modes ---

// propPublisher sits between global's publishers (the op_update handler,
// the observation handler, op create/abort/sweeps) and the real hub's
// op-stream fan-out. The hub keeps each connection's publish order, but
// those publishers run on different goroutines and each publishes after its
// own commit, so publish order need not be commit (version) order:
// hold/release makes such a reordering deterministic, and Replay re-sends
// already-delivered messages (an older row published after a newer one).
// The claim delta is published by the hub itself and does not pass through
// here.
type propPublisher struct {
	hub *ws.Hub

	mu      sync.Mutex
	holding bool
	held    []propOpMsg
	sent    []propOpMsg
	sentLog []propSent
}

// propSent records when an op message actually went to the hub fan-out.
type propSent struct {
	at      time.Time
	op      string
	removed bool
}

type propOpMsg struct {
	removed bool
	op      model.Operation
	st      model.MachineState
	at      time.Time // when it was first delivered
}

func (p *propPublisher) send(m propOpMsg) {
	p.mu.Lock()
	p.sentLog = append(p.sentLog, propSent{at: time.Now(), op: m.op.ID.String(), removed: m.removed})
	p.mu.Unlock()
	if m.removed {
		p.hub.PublishOpRemoved(m.op, m.st)
	} else {
		p.hub.PublishOpDelta(m.op, m.st)
	}
}

func (p *propPublisher) publish(m propOpMsg) {
	p.mu.Lock()
	if p.holding {
		p.held = append(p.held, m)
		p.mu.Unlock()
		return
	}
	m.at = time.Now()
	p.sent = append(p.sent, m)
	p.mu.Unlock()
	p.send(m)
}

func (p *propPublisher) PublishOpDelta(op model.Operation, st model.MachineState) {
	p.publish(propOpMsg{op: op, st: st})
}

func (p *propPublisher) PublishOpRemoved(op model.Operation, st model.MachineState) {
	p.publish(propOpMsg{removed: true, op: op, st: st})
}

func (p *propPublisher) Hold() {
	p.mu.Lock()
	p.holding = true
	p.mu.Unlock()
}

// Release delivers held messages, in publish order or reversed, pausing
// briefly between sends so the chosen order is the wire order.
func (p *propPublisher) Release(reverse bool) int {
	p.mu.Lock()
	p.holding = false
	held := p.held
	p.held = nil
	for i := range held {
		held[i].at = time.Now()
	}
	p.sent = append(p.sent, held...)
	p.mu.Unlock()
	if reverse {
		for i, j := 0, len(held)-1; i < j; i, j = i+1, j-1 {
			held[i], held[j] = held[j], held[i]
		}
	}
	for _, m := range held {
		p.send(m)
		time.Sleep(30 * time.Millisecond)
	}
	return len(held)
}

// Replay re-sends the last n already-delivered messages, oldest first.
// With deltasOnly, op_removed messages are left out: what arrives late is
// only the stale rows, with no second removal behind them to tidy up.
//
// Only messages first delivered on the worker's CURRENT connection are
// candidates: the hub binds every op message to the connection that existed
// when it was published, so nothing older than a connection's resync can
// turn up late on it, and nothing at all can turn up between a disconnect
// and the next resync. connSince reports that boundary and is consulted
// again before every send, because a replay takes tens of milliseconds and
// a restart can land in the middle of it.
func (p *propPublisher) Replay(n int, deltasOnly bool, connSince func() (time.Time, bool)) int {
	p.mu.Lock()
	if n > len(p.sent) {
		n = len(p.sent)
	}
	var msgs []propOpMsg
	for _, m := range p.sent[len(p.sent)-n:] {
		if deltasOnly && (m.removed || !propActive(m.op.Status)) {
			continue
		}
		msgs = append(msgs, m)
	}
	p.mu.Unlock()
	sent := 0
	for _, m := range msgs {
		if since, ok := connSince(); ok && m.at.After(since) {
			p.send(m)
			sent++
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sent
}
