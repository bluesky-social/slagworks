package commission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

type fakeStore struct {
	mu          sync.Mutex
	machines    map[uuid.UUID]*model.Machine
	stored      map[uuid.UUID][]byte
	storeErr    error
	completeErr error
	bindingErr  error
	completed   []completedOp

	// Per-pop locks emulating WithPopAdvisoryLock's serialization.
	popLocks   map[string]*sync.Mutex
	popLockErr error // injectable error for WithPopAdvisoryLock

	// bindingTypes records the op type every ValidateOperationBinding
	// call asked for; finalized records CompleteOperationWithState /
	// FailOperationOutcome calls (the bmc_reconcile_report and
	// phone_home paths). bindingAlreadyCompleted makes
	// ValidateOperationBinding report an accepted replay of a completed op.
	bindingTypes            []model.OperationType
	bindingAlreadyCompleted bool
	finalized               []finalizedOp
	finalizeErr             error

	dhcpLeaseEvents []recordedDHCPLeaseEvent
	dhcpLeaseErr    error // injectable error for RecordDHCPLeaseEvent
}

// recordedDHCPLeaseEvent captures one RecordDHCPLeaseEvent call's
// arguments for test assertions.
type recordedDHCPLeaseEvent struct {
	SiteSlug, WorkerID, Event, MAC, IP, Kind, Pool string
	MachineID                                      *uuid.UUID
	ExpiresAt                                      time.Time
}

type completedOp struct {
	MachineID   uuid.UUID
	Type        model.OperationType
	TargetState model.MachineState
}

// finalizedOp is one op the handler completed or failed directly.
type finalizedOp struct {
	OpID   uuid.UUID
	Status model.OperationStatus
	Target model.MachineState
	Result []byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		machines: make(map[uuid.UUID]*model.Machine),
		stored:   make(map[uuid.UUID][]byte),
		popLocks: make(map[string]*sync.Mutex),
	}
}

func (s *fakeStore) CompleteOperationWithState(opID uuid.UUID, target model.MachineState, result []byte) (*store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalizeErr != nil {
		return nil, s.finalizeErr
	}
	s.finalized = append(s.finalized, finalizedOp{OpID: opID, Status: model.OpCompleted, Target: target, Result: append([]byte(nil), result...)})
	return &store.OpOutcome{Op: model.Operation{ID: opID, Status: model.OpCompleted, Result: result}, MachineState: target}, nil
}

func (s *fakeStore) FailOperationOutcome(opID uuid.UUID, result []byte) (*store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalizeErr != nil {
		return nil, s.finalizeErr
	}
	s.finalized = append(s.finalized, finalizedOp{OpID: opID, Status: model.OpFailed, Result: append([]byte(nil), result...)})
	return &store.OpOutcome{Op: model.Operation{ID: opID, Status: model.OpFailed, Result: result}}, nil
}

func (s *fakeStore) WithPopAdvisoryLock(ctx context.Context, popSlug string, fn func(context.Context) error) error {
	if s.popLockErr != nil {
		return s.popLockErr
	}
	s.mu.Lock()
	mu, ok := s.popLocks[popSlug]
	if !ok {
		mu = &sync.Mutex{}
		s.popLocks[popSlug] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return fn(ctx)
}

func (s *fakeStore) RecordDHCPLeaseEvent(_ context.Context, siteSlug, workerID, event, mac, ip, kind, pool string, machineID *uuid.UUID, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dhcpLeaseErr != nil {
		return s.dhcpLeaseErr
	}
	s.dhcpLeaseEvents = append(s.dhcpLeaseEvents, recordedDHCPLeaseEvent{
		SiteSlug: siteSlug, WorkerID: workerID, Event: event, MAC: mac, IP: ip,
		Kind: kind, Pool: pool, MachineID: machineID, ExpiresAt: expiresAt,
	})
	return nil
}

func (s *fakeStore) GetMachine(id uuid.UUID) (*model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (s *fakeStore) StoreCommissionData(id uuid.UUID, data json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return s.storeErr
	}
	s.stored[id] = append([]byte(nil), data...)
	return nil
}

func (s *fakeStore) CompleteActiveOperation(
	machineID uuid.UUID,
	opType model.OperationType,
	targetState model.MachineState,
	_ []byte,
) (*store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	s.completed = append(s.completed, completedOp{machineID, opType, targetState})
	// Mirror the state transition in the fake machines map so tests
	// asserting on GetMachine afterward see the expected state.
	if m := s.machines[machineID]; m != nil {
		m.State = targetState
	}
	return &store.OpOutcome{
		Op:           model.Operation{MachineID: machineID, Type: opType, Status: model.OpCompleted},
		MachineState: targetState,
	}, nil
}

func (s *fakeStore) ValidateOperationBinding(
	_ uuid.UUID, _ uuid.UUID, opType model.OperationType, _ json.RawMessage,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindingTypes = append(s.bindingTypes, opType)
	if s.bindingErr != nil {
		return false, s.bindingErr
	}
	return s.bindingAlreadyCompleted, nil
}

func (s *fakeStore) CommitCommissionResult(
	opID, machineID uuid.UUID, data json.RawMessage,
) (*store.OpOutcome, bool, error) {
	if err := s.StoreCommissionData(machineID, data); err != nil {
		return nil, false, err
	}
	outcome, err := s.CompleteActiveOperation(
		machineID, model.OpCommission, model.MachineStateCommissioned, data,
	)
	if outcome != nil {
		outcome.Op.ID = opID
	}
	return outcome, false, err
}

type fakeNetBox struct {
	mu        sync.Mutex
	syncCalls []netbox.CommissionSync
	syncErr   error
	devices   map[int]*netbox.Device

	deviceTypes        map[int]*netbox.DeviceType
	kernelParamsWrites map[int]string
}

func (n *fakeNetBox) SyncCommissionData(_ context.Context, sync netbox.CommissionSync) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.syncCalls = append(n.syncCalls, sync)
	return n.syncErr
}

func (n *fakeNetBox) GetDeviceTypeByID(_ context.Context, id int) (*netbox.DeviceType, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	dt, ok := n.deviceTypes[id]
	if !ok {
		return nil, fmt.Errorf("device-type %d not found", id)
	}
	return dt, nil
}

func (n *fakeNetBox) SetDeviceTypeKernelParams(_ context.Context, id int, kernelParams string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.kernelParamsWrites == nil {
		n.kernelParamsWrites = map[int]string{}
	}
	n.kernelParamsWrites[id] = kernelParams
	return nil
}

func (n *fakeNetBox) GetDeviceByID(_ context.Context, id int) (*netbox.Device, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if d, ok := n.devices[id]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, nil
}

type fakeInvalidator struct {
	mu      sync.Mutex
	invalid []string
}

func (f *fakeInvalidator) Invalidate(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = append(f.invalid, slug)
}

// fakeClaimResolver maps a device to its claiming config slug(s). The
// default behavior returns the device's site slug as the config slug
// (the pop1/pop3 shape where slug == netbox_site); set byID to model a
// location-scoped config whose PK differs from its NetBox site.
type fakeClaimResolver struct {
	byID map[int][]string
}

func (r *fakeClaimResolver) ClaimingConfigs(_ context.Context, d netbox.Device) ([]string, error) {
	if r.byID != nil {
		return r.byID[d.ID], nil
	}
	if d.Site != nil && d.Site.Slug != "" {
		return []string{d.Site.Slug}, nil
	}
	return nil, nil
}

func TestHandleCommissionReport_SuccessPath(t *testing.T) {
	st := newFakeStore()
	id := uuid.New()
	devID := 42
	st.machines[id] = &model.Machine{
		ID:             id,
		NetBoxDeviceID: &devID,
		State:          model.MachineStateCommissioning,
	}

	nb := &fakeNetBox{
		devices: map[int]*netbox.Device{
			devID: {ID: devID, Name: "cpu1.pop3", Site: &netbox.NestedRef{ID: 1, Slug: "pop3"}, Role: &netbox.NestedRef{ID: 3, Slug: "server"}},
		},
	}
	inv := &fakeInvalidator{}

	h := New(st, nb, inv)
	h.SetClaimResolver(&fakeClaimResolver{})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "commission_report",
		Data:      json.RawMessage(`{"system":{"uuid":"abc"}}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nb.mu.Lock()
	if len(nb.syncCalls) != 1 {
		t.Fatalf("sync calls: got %d, want 1", len(nb.syncCalls))
	}
	if nb.syncCalls[0].DeviceID != devID {
		t.Fatalf("device_id: got %d, want %d", nb.syncCalls[0].DeviceID, devID)
	}
	if nb.syncCalls[0].DeviceRole != "server" {
		t.Fatalf("device_role: got %q, want %q (drives switch interface-name preservation)", nb.syncCalls[0].DeviceRole, "server")
	}
	nb.mu.Unlock()

	st.mu.Lock()
	if len(st.stored[id]) == 0 {
		t.Fatal("expected commission data stored")
	}
	st.mu.Unlock()

	inv.mu.Lock()
	if len(inv.invalid) != 1 || inv.invalid[0] != "pop3" {
		t.Fatalf("invalidations: %+v", inv.invalid)
	}
	inv.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.completed) != 1 {
		t.Fatalf("expected 1 completed op, got %d", len(st.completed))
	}
	got := st.completed[0]
	if got.MachineID != id || got.Type != model.OpCommission || got.TargetState != model.MachineStateCommissioned {
		t.Fatalf("completed op: %+v", got)
	}
}

func TestHandleCommissionReport_BindingMismatchHasNoSideEffects(t *testing.T) {
	st := newFakeStore()
	st.bindingErr = fmt.Errorf("operation belongs to another machine: %w", store.ErrOperationBinding)
	machineID, opID := uuid.New(), uuid.New()
	devID := 42
	st.machines[machineID] = &model.Machine{ID: machineID, NetBoxDeviceID: &devID}
	nb := &fakeNetBox{devices: map[int]*netbox.Device{
		devID: {ID: devID, Name: "wrong.pop1"},
	}}

	err := New(st, nb, nil).HandleObservation(context.Background(), model.Observation{
		MachineID: machineID, OperationID: opID, Event: "commission_report",
		Data: json.RawMessage(`{"system":{"serial_number":"SN-OTHER"}}`),
	})
	if err == nil || !errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("binding mismatch error = %v, want terminal rejection", err)
	}
	nb.mu.Lock()
	syncCalls := len(nb.syncCalls)
	nb.mu.Unlock()
	st.mu.Lock()
	stored, completed := len(st.stored), len(st.completed)
	st.mu.Unlock()
	if syncCalls != 0 || stored != 0 || completed != 0 {
		t.Fatalf("binding mismatch had side effects: netbox=%d stored=%d completed=%d",
			syncCalls, stored, completed)
	}
}

func TestHandleCommissionReport_StoreFailureSkipsOpCompletion(t *testing.T) {
	st := newFakeStore()
	st.storeErr = errors.New("pg down")
	id := uuid.New()
	devID := 77
	st.machines[id] = &model.Machine{
		ID:             id,
		NetBoxDeviceID: &devID,
		State:          model.MachineStateCommissioning,
	}
	nb := &fakeNetBox{
		devices: map[int]*netbox.Device{
			devID: {ID: devID, Site: &netbox.NestedRef{ID: 1, Slug: "pop3"}},
		},
	}
	h := New(st, nb, nil)

	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "commission_report",
		Data:      json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error when StoreCommissionData fails")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.completed) != 0 {
		t.Fatalf("op should NOT be completed when store fails: %+v", st.completed)
	}
}

func TestHandleCommissionReport_MachineNotFound(t *testing.T) {
	st := newFakeStore()
	nb := &fakeNetBox{}
	h := New(st, nb, nil)

	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.New(),
		Event:     "commission_report",
		Data:      json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("got %v, want ErrMachineNotFound", err)
	}
}

func TestHandleCommissionReport_DeviceNotBound(t *testing.T) {
	st := newFakeStore()
	id := uuid.New()
	st.machines[id] = &model.Machine{ID: id} // NetBoxDeviceID nil
	h := New(st, &fakeNetBox{}, nil)

	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "commission_report",
	})
	if !errors.Is(err, ErrDeviceNotBound) {
		t.Fatalf("got %v, want ErrDeviceNotBound", err)
	}
}

func TestHandleCommissionReport_NetBoxFailure_NoPostgresWrite(t *testing.T) {
	// Ordering: NetBox first, Postgres second. If NetBox fails, we
	// must NOT write to Postgres — the ack contract relies on both
	// writes completing together.
	st := newFakeStore()
	id := uuid.New()
	devID := 42
	st.machines[id] = &model.Machine{ID: id, NetBoxDeviceID: &devID}
	nb := &fakeNetBox{
		syncErr: errors.New("netbox 500"),
		devices: map[int]*netbox.Device{devID: {ID: devID, Site: &netbox.NestedRef{Slug: "pop3"}}},
	}

	h := New(st, nb, nil)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "commission_report",
		Data:      json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error when NetBox fails")
	}
	st.mu.Lock()
	if len(st.stored) != 0 {
		t.Fatalf("postgres write happened despite netbox failure: %+v", st.stored)
	}
	st.mu.Unlock()
}

func TestHandleObservation_NonCommissionEventsAckedQuietly(t *testing.T) {
	h := New(newFakeStore(), &fakeNetBox{}, nil)
	for _, event := range []string{"phone_home", "inventory_import", "unknown_future_event"} {
		err := h.HandleObservation(context.Background(), model.Observation{
			MachineID: uuid.New(),
			Event:     event,
		})
		if err != nil {
			t.Errorf("event %q: unexpected error %v", event, err)
		}
	}
}

// phoneHomeFixture is a Handler with an op publisher and an invalidator,
// for the phone_home path.
func phoneHomeFixture() (*Handler, *fakeStore, *fakeOpPublisher, *fakeInvalidator) {
	st := newFakeStore()
	inv := &fakeInvalidator{}
	pub := &fakeOpPublisher{}
	h := New(st, &fakeNetBox{}, inv)
	h.SetPublisher(pub)
	return h, st, pub, inv
}

// TestHandlePhoneHome_CompletesDeployOp: a phone_home observation that
// names the deploy op completes it and moves the machine to running. The
// worker's op_update for the same completion is memory-only and
// unacknowledged; this observation is the copy that survives a worker
// restart or a dropped connection, so global must act on it rather than
// ack and ignore it (which left the op to fail at PhoneHomeTimeout).
func TestHandlePhoneHome_CompletesDeployOp(t *testing.T) {
	h, st, pub, inv := phoneHomeFixture()
	machineID, opID := uuid.New(), uuid.New()
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: machineID, OperationID: opID, Event: "phone_home", SiteSlug: "pop1",
	})
	if err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if len(st.bindingTypes) != 1 || st.bindingTypes[0] != model.OpDeploy {
		t.Errorf("binding must be validated against deploy, got %v", st.bindingTypes)
	}
	if len(st.finalized) != 1 || st.finalized[0].OpID != opID ||
		st.finalized[0].Status != model.OpCompleted || st.finalized[0].Target != model.MachineStateRunning {
		t.Fatalf("expected deploy op %s completed with target running, got %+v", opID, st.finalized)
	}
	if len(pub.removed) != 1 || pub.removed[0].ID != opID {
		t.Errorf("op_removed not published for %s: %+v", opID, pub.removed)
	}
	if len(inv.invalid) != 1 || inv.invalid[0] != "pop1" {
		t.Errorf("sending site's snapshot not invalidated: %+v", inv.invalid)
	}
}

// TestHandlePhoneHome_NoOpCases: the observation is acked without a
// transition when it names no op (a late phone-home: the worker held no
// active deploy op) or when the op_update path already completed the op.
func TestHandlePhoneHome_NoOpCases(t *testing.T) {
	for _, tc := range []struct {
		name             string
		opID             uuid.UUID
		alreadyCompleted bool
	}{
		{name: "no operation ID", opID: uuid.Nil},
		{name: "already completed", opID: uuid.New(), alreadyCompleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, pub, inv := phoneHomeFixture()
			st.bindingAlreadyCompleted = tc.alreadyCompleted
			err := h.HandleObservation(context.Background(), model.Observation{
				MachineID: uuid.New(), OperationID: tc.opID, Event: "phone_home", SiteSlug: "pop1",
			})
			if err != nil {
				t.Fatalf("HandleObservation: %v", err)
			}
			if len(st.finalized) != 0 || len(pub.removed) != 0 || len(inv.invalid) != 0 {
				t.Errorf("no-op phone_home changed state: finalized=%+v removed=%+v invalidated=%+v",
					st.finalized, pub.removed, inv.invalid)
			}
		})
	}
}

// TestHandlePhoneHome_BindingRejectedIsTerminal: a phone_home for an op
// that is failed (aborted, or already swept at PhoneHomeTimeout), belongs
// to another machine, or is not a deploy must not complete anything, and
// is dropped terminally so the worker stops replaying it.
func TestHandlePhoneHome_BindingRejectedIsTerminal(t *testing.T) {
	h, st, _, _ := phoneHomeFixture()
	st.bindingErr = fmt.Errorf("operation is terminal (failed): %w", store.ErrOperationBinding)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.New(), OperationID: uuid.New(), Event: "phone_home",
	})
	if !errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("binding rejection must be terminal, got %v", err)
	}
	if len(st.finalized) != 0 {
		t.Errorf("rejected phone_home finalized an op: %+v", st.finalized)
	}
}

// TestHandlePhoneHome_StoreErrorIsRetried: a transient store failure
// returns a non-terminal error, so the worker keeps the outbox row and
// replays it. This is the case the op_update path drops on the floor.
func TestHandlePhoneHome_StoreErrorIsRetried(t *testing.T) {
	h, st, pub, _ := phoneHomeFixture()
	st.finalizeErr = errors.New("connection reset by peer")
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.New(), OperationID: uuid.New(), Event: "phone_home",
	})
	if err == nil || errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("transient store failure must be retryable, got %v", err)
	}
	if len(pub.removed) != 0 {
		t.Errorf("op_removed published for a failed completion: %+v", pub.removed)
	}
}

// TestHandleDHCPLease_DecodesAndForwardsWithHubStampedIdentity pins
// that a dhcp_lease observation is decoded and forwarded to the store
// with the hub-stamped SiteSlug/WorkerID (never anything from the
// payload, which carries no such fields) and a resolved MachineID
// pointer when obs.MachineID is non-nil.
func TestHandleDHCPLease_DecodesAndForwardsWithHubStampedIdentity(t *testing.T) {
	st := newFakeStore()
	h := New(st, &fakeNetBox{}, nil)
	machineID := uuid.New()

	data, err := json.Marshal(model.DHCPLeaseEvent{
		Event: "allocate", MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.100",
		Kind: "dynamic", Pool: "10.99.1.0/24", ExpiresAt: "2026-07-13T22:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: machineID,
		Event:     "dhcp_lease",
		SiteSlug:  "pop3",
		WorkerID:  "worker-1",
		Data:      data,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(st.dhcpLeaseEvents) != 1 {
		t.Fatalf("got %d recorded events, want 1", len(st.dhcpLeaseEvents))
	}
	got := st.dhcpLeaseEvents[0]
	if got.SiteSlug != "pop3" || got.WorkerID != "worker-1" {
		t.Errorf("identity = (%q, %q), want (pop3, worker-1)", got.SiteSlug, got.WorkerID)
	}
	if got.Event != "allocate" || got.MAC != "aa:bb:cc:00:00:01" || got.IP != "10.99.1.100" || got.Kind != "dynamic" || got.Pool != "10.99.1.0/24" {
		t.Errorf("event fields = %+v, want the decoded payload", got)
	}
	if got.MachineID == nil || *got.MachineID != machineID {
		t.Errorf("MachineID = %v, want %s", got.MachineID, machineID)
	}
	wantExpiry, _ := time.Parse(time.RFC3339, "2026-07-13T22:00:00Z")
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, wantExpiry)
	}
}

// TestHandleDHCPLease_UnknownMACPassesNilMachineID pins that
// obs.MachineID == uuid.Nil (a MAC that doesn't resolve to a known
// machine — legal on the promiscuous OOB segment) is forwarded as a
// nil pointer, NOT treated as ErrMachineNotFound the way
// commission_report/deploy_complete would.
func TestHandleDHCPLease_UnknownMACPassesNilMachineID(t *testing.T) {
	st := newFakeStore()
	h := New(st, &fakeNetBox{}, nil)

	data, err := json.Marshal(model.DHCPLeaseEvent{
		Event: "allocate", MAC: "aa:bb:cc:00:00:02", IP: "10.99.1.101",
		Kind: "dynamic", Pool: "10.99.1.0/24", ExpiresAt: "2026-07-13T22:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.Nil,
		Event:     "dhcp_lease",
		SiteSlug:  "pop3",
		WorkerID:  "worker-1",
		Data:      data,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(st.dhcpLeaseEvents) != 1 {
		t.Fatalf("got %d recorded events, want 1", len(st.dhcpLeaseEvents))
	}
	if st.dhcpLeaseEvents[0].MachineID != nil {
		t.Errorf("MachineID = %v, want nil for an unresolved MAC", st.dhcpLeaseEvents[0].MachineID)
	}
}

// TestHandleDHCPLease_MalformedDataErrors pins that undecodable Data
// (or an unparseable expires_at) returns an error, so the worker
// retries rather than silently dropping the outbox row.
func TestHandleDHCPLease_MalformedDataErrors(t *testing.T) {
	h := New(newFakeStore(), &fakeNetBox{}, nil)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.New(),
		Event:     "dhcp_lease",
		SiteSlug:  "pop3",
		WorkerID:  "worker-1",
		Data:      []byte(`not json`),
	})
	if err == nil {
		t.Fatal("expected an error for malformed dhcp_lease data")
	}
}

// deployCompleteFixture builds a minimal store + netbox state with
// one provisioned machine, and a Handler wired to record monitor
// secret writes via FakeMonitorWriter.
func deployCompleteFixture(t *testing.T) (
	*Handler, *fakeStore, uuid.UUID, *awssecrets.FakeMonitorWriter,
) {
	t.Helper()
	st := newFakeStore()
	id := uuid.New()
	devID := 42
	st.machines[id] = &model.Machine{
		ID: id, NetBoxDeviceID: &devID, NetBoxSerial: "SN-X",
		State: model.MachineStateDeploying,
	}
	nb := &fakeNetBox{
		devices: map[int]*netbox.Device{
			devID: {
				ID:     devID,
				Name:   "host1.pop1",
				Serial: "SN-X",
				Site:   &netbox.NestedRef{ID: 1, Slug: "pop1"},
				OOBIP:  &netbox.NestedIPAddress{ID: 7, Address: "10.252.240.41/23"},
			},
		},
	}
	w := &awssecrets.FakeMonitorWriter{}
	h := New(st, nb, nil)
	h.SetMonitorWriter(w)
	return h, st, id, w
}

// TestHandleDeployComplete_NoData covers the no-rotation re-deploy
// path: deploy_complete with empty Data is a no-op (the agent had
// nothing fresh to phone home about).
func TestHandleDeployComplete_NoData(t *testing.T) {
	h, _, _, w := deployCompleteFixture(t)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: uuid.New(),
		Event:     "deploy_complete",
	})
	if err != nil {
		t.Errorf("empty deploy_complete should ack, got %v", err)
	}
	if len(w.Secrets) != 0 {
		t.Errorf("AWS upsert called for empty Data: %+v", w.Secrets)
	}
}

// TestHandleDeployComplete_MonitorKeyedBySerial verifies the happy path:
// the agent provisioned monitor, and the handler upserts it into
// pop/<pop>/bmc-creds-monitor keyed by the NetBox device SERIAL (like
// bmc-creds-admin), recording the host's last known hostname and BMC IP
// (the NetBox oob_ip, mask stripped).
func TestHandleDeployComplete_MonitorKeyedBySerial(t *testing.T) {
	h, _, id, w := deployCompleteFixture(t)
	creds := model.BMCCreds{
		Monitor: &model.BMCUserCred{
			Username: "monitor", Password: "DEF456UVW0", IPMIUserSlot: 4,
		},
		BootOrderPersisted: true,
	}
	data, _ := json.Marshal(&creds)
	if err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	}); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}

	if _, byHost := w.Secrets["pop1"]["host1.pop1"]; byHost {
		t.Fatal("monitor entry keyed by hostname; it must be keyed by NetBox serial")
	}
	got, ok := w.Secrets["pop1"]["SN-X"]
	if !ok {
		t.Fatalf("AWS monitor entry missing for serial SN-X: %+v", w.Secrets)
	}
	if got.Username != "monitor" || got.Password != "DEF456UVW0" || got.Kind != "bmc" {
		t.Errorf("AWS monitor entry shape: %+v", got)
	}
	if v, _ := got.ExtraConfig["ipmi_user_slot"].(int); v != 4 {
		t.Errorf("AWS monitor extra_config.ipmi_user_slot: got %v want 4", got.ExtraConfig["ipmi_user_slot"])
	}
	if got.LastKnownHostname != "host1.pop1" || got.LastKnownIP != "10.252.240.41" {
		t.Errorf("last_known fields: hostname=%q ip=%q, want host1.pop1 / 10.252.240.41",
			got.LastKnownHostname, got.LastKnownIP)
	}
}

// TestHandleDeployComplete_NoMonitor_NoWrite: the agent ran reconcile but
// wrote no new password (monitor already provisioned); only leftover
// disables are reported. Nothing to persist; ack.
func TestHandleDeployComplete_NoMonitor_NoWrite(t *testing.T) {
	h, _, id, w := deployCompleteFixture(t)
	data, _ := json.Marshal(&model.BMCCreds{LeftoversDisabled: []string{"slagworks"}})
	if err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	}); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
	if len(w.Secrets) != 0 {
		t.Errorf("AWS upsert ran for monitor=nil: %+v", w.Secrets)
	}
}

// TestHandleDeployComplete_NoSerial_FailsLoudly: the monitor secret is
// keyed by NetBox device serial, so a device without one cannot be keyed.
// Fail loudly (like the admin-secret path) and retry — the observation
// lands once the operator sets the serial in NetBox — rather than write an
// entry under some other key.
func TestHandleDeployComplete_NoSerial_FailsLoudly(t *testing.T) {
	h, _, id, w := deployCompleteFixture(t)
	nb := h.nb.(*fakeNetBox)
	nb.mu.Lock()
	nb.devices[42].Serial = ""
	nb.mu.Unlock()

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "P", IPMIUserSlot: 4},
	})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	})
	if err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("expected an error naming the missing serial, got %v", err)
	}
	if errors.Is(err, ws.ErrTerminal) {
		t.Error("a missing NetBox serial is operator-fixable; the observation must retry, not drop")
	}
	if len(w.Secrets) != 0 {
		t.Errorf("monitor entry written without a serial key: %+v", w.Secrets)
	}
}

// TestHandleDeployComplete_MonitorSecretMissing_Terminal verifies that when the
// per-pop monitor secret doesn't exist (pop not bootstrapped), the handler
// returns a TERMINAL error so the worker drops the outbox row instead of
// replaying forever. A GENERIC AWS error must remain NON-terminal
// (retryable) — only the missing-secret sentinel is terminal.
func TestHandleDeployComplete_MonitorSecretMissing_Terminal(t *testing.T) {
	h, _, id, _ := deployCompleteFixture(t)
	h.SetMonitorWriter(&errorMonitorWriter{
		err: fmt.Errorf("pop/pop2/bmc-creds-monitor: %w", awssecrets.ErrMonitorSecretMissing),
	})

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "MON99", IPMIUserSlot: 4},
	})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	})
	if err == nil {
		t.Fatal("expected an error for the missing monitor secret")
	}
	if !errors.Is(err, ws.ErrTerminal) {
		t.Errorf("missing monitor secret must be TERMINAL (worker drops the row), got non-terminal: %v", err)
	}
}

// TestHandleDeployComplete_GenericAWSError_NotTerminal guards that a transient
// AWS error (not the missing-secret sentinel) stays NON-terminal so the worker
// keeps retrying — we only want to drop the row for the unrecoverable
// pop-not-bootstrapped case.
func TestHandleDeployComplete_GenericAWSError_NotTerminal(t *testing.T) {
	h, _, id, _ := deployCompleteFixture(t)
	h.SetMonitorWriter(&errorMonitorWriter{err: errors.New("aws is down")})

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "MON", IPMIUserSlot: 4},
	})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	})
	if err == nil {
		t.Fatal("expected the AWS error to surface")
	}
	if errors.Is(err, ws.ErrTerminal) {
		t.Error("a generic/transient AWS error must NOT be terminal (worker should retry)")
	}
}

// TestHandleDeployComplete_PerPopSerialization verifies that the
// monitor upsert is wrapped in WithPopAdvisoryLock — concurrent
// deploy_complete events for hosts in the same pop must be
// serialized at the lock layer. Two concurrent calls with monitor
// creds must observe the lock holder; we check by tracking enter/exit
// of the fake's WithPopAdvisoryLock.
func TestHandleDeployComplete_PerPopSerialization(t *testing.T) {
	h, st, id, _ := deployCompleteFixture(t)

	// Wrap the monitor writer so each upsert blocks on a release
	// channel — lets us prove two concurrent invocations serialize.
	released := make(chan struct{})
	w := &blockingMonitorWriter{
		entered: make(chan struct{}, 8),
		release: released,
	}
	h.SetMonitorWriter(w)

	creds := model.BMCCreds{
		Monitor: &model.BMCUserCred{
			Username: "monitor", Password: "P000000000", IPMIUserSlot: 4,
		},
	}
	data, _ := json.Marshal(&creds)

	// First goroutine: acquires the lock, blocks in upsert.
	first := make(chan error, 1)
	go func() {
		first <- h.HandleObservation(context.Background(), model.Observation{
			MachineID: id, Event: "deploy_complete", Data: data,
		})
	}()

	// Wait for the first to enter upsert (= lock acquired).
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first upsert never started")
	}

	// Add a sibling machine in the same pop, fire its deploy_complete.
	id2 := uuid.New()
	dev2 := 99
	st.mu.Lock()
	st.machines[id2] = &model.Machine{
		ID: id2, NetBoxDeviceID: &dev2, NetBoxSerial: "SN-Y",
		State: model.MachineStateDeploying,
	}
	st.mu.Unlock()
	nb := h.nb.(*fakeNetBox)
	nb.mu.Lock()
	nb.devices[dev2] = &netbox.Device{
		ID:     dev2,
		Name:   "host2.pop1",
		Serial: "SN-Y",
		Site:   &netbox.NestedRef{ID: 1, Slug: "pop1"},
	}
	nb.mu.Unlock()

	second := make(chan error, 1)
	go func() {
		second <- h.HandleObservation(context.Background(), model.Observation{
			MachineID: id2, Event: "deploy_complete", Data: data,
		})
	}()

	// Second should NOT enter upsert until the first releases.
	select {
	case <-w.entered:
		t.Fatal("second upsert entered concurrent with first (lock not enforced)")
	case <-time.After(150 * time.Millisecond):
	}

	// Release first; second should now proceed.
	released <- struct{}{}
	if err := <-first; err != nil {
		t.Errorf("first: %v", err)
	}
	released <- struct{}{}
	if err := <-second; err != nil {
		t.Errorf("second: %v", err)
	}
}

// TestHandleDeployComplete_MalformedData verifies that a Data blob
// that doesn't decode into BMCCreds surfaces a hard error. The hub
// turns a non-nil error into a non-ack; the worker retries.
// Persistent garbage stays unacked, surfacing as "stuck observation"
// telemetry — which is the right shape because it's never a legit
// state.
func TestHandleDeployComplete_MalformedData(t *testing.T) {
	h, _, id, _ := deployCompleteFixture(t)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "deploy_complete",
		Data:      json.RawMessage(`{not valid json`),
	})
	if err == nil {
		t.Error("malformed BMCCreds data should error; got nil")
	}
}

// TestHandleDeployComplete_DeletedDevice_Acks covers the device-
// removed-mid-deploy path. We ack rather than retrying forever; the
// soft-decommission reconciler picks up the orphan on its next pass.
// The monitor upsert is skipped: without the device there is no pop to
// pick the secret.
func TestHandleDeployComplete_DeletedDevice_Acks(t *testing.T) {
	st := newFakeStore()
	id := uuid.New()
	devID := 42
	st.machines[id] = &model.Machine{ID: id, NetBoxDeviceID: &devID, NetBoxSerial: "SN-X"}
	nb := &fakeNetBox{} // empty devices map
	w := &awssecrets.FakeMonitorWriter{}
	h := New(st, nb, nil)
	h.SetMonitorWriter(w)

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "X"},
	})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	})
	if err != nil {
		t.Errorf("deleted device should ack, got %v", err)
	}
	if len(w.Secrets) != 0 {
		t.Errorf("AWS upsert ran for deleted device: %+v", w.Secrets)
	}
}

// TestHandleDeployComplete_NoNetBoxDevice covers the impossible-but-
// defended-against state where a deploying machine has no
// netbox_device_id: there is no device to key or locate the monitor
// secret, so surface ErrDeviceNotBound.
func TestHandleDeployComplete_NoNetBoxDevice(t *testing.T) {
	st := newFakeStore()
	id := uuid.New()
	st.machines[id] = &model.Machine{ID: id, NetBoxDeviceID: nil, NetBoxSerial: "SN-X"}
	h := New(st, &fakeNetBox{}, nil)
	w := &awssecrets.FakeMonitorWriter{}
	h.SetMonitorWriter(w)

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "X", IPMIUserSlot: 4},
	})
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	})
	if !errors.Is(err, ErrDeviceNotBound) {
		t.Fatalf("expected ErrDeviceNotBound, got %v", err)
	}
	if len(w.Secrets) != 0 {
		t.Errorf("AWS upsert ran for an unbound machine: %+v", w.Secrets)
	}
}

// TestHandleDeployComplete_NilMonitorWriter_Warns covers the local-
// dev / no-AWS path: handler logs a warning but doesn't fail the
// deploy when the monitor cred can't be written.
func TestHandleDeployComplete_NilMonitorWriter_Warns(t *testing.T) {
	h, _, id, _ := deployCompleteFixture(t)
	h.SetMonitorWriter(nil)

	data, _ := json.Marshal(&model.BMCCreds{
		Monitor: &model.BMCUserCred{Username: "monitor", Password: "DROPPED000"},
	})
	if err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id, Event: "deploy_complete", Data: data,
	}); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}
}

// errorMonitorWriter always errors.
type errorMonitorWriter struct{ err error }

func (e *errorMonitorWriter) GetMonitorEntry(context.Context, string, string) (*awssecrets.BMCMonitorEntry, error) {
	return nil, e.err
}

func (e *errorMonitorWriter) UpsertMonitorEntry(_ context.Context, _, _ string, _ awssecrets.BMCMonitorEntry) error {
	return e.err
}

// blockingMonitorWriter signals when the upsert is entered and waits
// for a token on `release` before returning. Used to prove the
// per-pop advisory lock serializes concurrent upserts.
type blockingMonitorWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMonitorWriter) GetMonitorEntry(context.Context, string, string) (*awssecrets.BMCMonitorEntry, error) {
	return nil, nil
}

func (b *blockingMonitorWriter) UpsertMonitorEntry(_ context.Context, _, _ string, _ awssecrets.BMCMonitorEntry) error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}

func TestHandleCommissionReport_DeletedDevice_Ackable(t *testing.T) {
	// If the NetBox device was deleted between worker send and our
	// handling, we ack (nil error) so the worker's outbox entry drops —
	// the machine will be soft-decommissioned on the next reconcile.
	st := newFakeStore()
	id := uuid.New()
	devID := 42
	st.machines[id] = &model.Machine{ID: id, NetBoxDeviceID: &devID}
	nb := &fakeNetBox{} // devices empty → GetDeviceByID returns nil

	h := New(st, nb, nil)
	err := h.HandleObservation(context.Background(), model.Observation{
		MachineID: id,
		Event:     "commission_report",
		Data:      json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("expected nil (ack) for deleted device, got %v", err)
	}
}
