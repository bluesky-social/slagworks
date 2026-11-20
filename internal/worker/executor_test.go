package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type mockPowerController struct {
	lastAction     string
	lastBootDevice power.BootDevice
	failAction     bool
	failBootDevice bool
	chassisSerial  string // returned by ChassisSerial; default ""
}

func (m *mockPowerController) PowerOn(_ context.Context) error {
	m.lastAction = "on"
	if m.failAction {
		return fmt.Errorf("mock power on failure")
	}
	return nil
}

func (m *mockPowerController) PowerOff(_ context.Context) error {
	m.lastAction = "off"
	if m.failAction {
		return fmt.Errorf("mock power off failure")
	}
	return nil
}

func (m *mockPowerController) PowerCycle(_ context.Context) error {
	m.lastAction = "cycle"
	if m.failAction {
		return fmt.Errorf("mock power cycle failure")
	}
	return nil
}

func (m *mockPowerController) PowerStatus(_ context.Context) (power.State, error) {
	return power.StateUnknown, nil
}

func (m *mockPowerController) SetBootDevice(_ context.Context, dev power.BootDevice) error {
	m.lastBootDevice = dev
	if m.failBootDevice {
		return fmt.Errorf("mock set boot device failure")
	}
	return nil
}

func (m *mockPowerController) ChassisSerial(_ context.Context) (string, error) {
	return m.chassisSerial, nil
}

func (m *mockPowerController) Transport() string { return "mock" }

func (m *mockPowerController) Close(_ context.Context) error {
	return nil
}

// mockBootController is a mockPowerController that ALSO implements
// power.BootManager — the boot-capable stub for runSetBootDevice
// tests. The plain mockPowerController (no BootManager) exercises the
// not-supported path.
type mockBootController struct {
	mockPowerController
	overrideDevice     power.BootDevice
	overridePersistent bool
	setOverrideErr     error
}

func (m *mockBootController) BootStatus(_ context.Context) (power.BootOverride, error) {
	return power.BootOverride{Device: m.overrideDevice, Persistent: m.overridePersistent, Valid: true}, nil
}

func (m *mockBootController) SetBootOverride(_ context.Context, dev power.BootDevice, persistent bool) error {
	m.overrideDevice = dev
	m.overridePersistent = persistent
	return m.setOverrideErr
}

func TestRunSetBootDevice(t *testing.T) {
	cases := []struct {
		name        string
		params      string
		wantDevice  power.BootDevice
		wantPersist bool
	}{
		{"pxe one-time", `{"device":"pxe","persistent":false}`, power.BootPXE, false},
		{"pxe persistent", `{"device":"pxe","persistent":true}`, power.BootPXE, true},
		{"disk", `{"device":"disk"}`, power.BootDisk, false},
		{"default clear", `{"device":"default"}`, power.BootDefault, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &mockBootController{}
			if err := runSetBootDevice(context.Background(), ctrl, json.RawMessage(tc.params)); err != nil {
				t.Fatalf("runSetBootDevice: %v", err)
			}
			if ctrl.overrideDevice != tc.wantDevice || ctrl.overridePersistent != tc.wantPersist {
				t.Errorf("got device=%q persistent=%v, want %q %v",
					ctrl.overrideDevice, ctrl.overridePersistent, tc.wantDevice, tc.wantPersist)
			}
		})
	}
}

func TestRunSetBootDeviceNotSupported(t *testing.T) {
	// A controller that isn't a BootManager (bmclib/PDU) must fail with an explicit "not supported", not silently no-op.
	ctrl := &mockPowerController{}
	err := runSetBootDevice(context.Background(), ctrl, json.RawMessage(`{"device":"pxe"}`))
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("want not-supported error, got %v", err)
	}
}

func TestRunSetBootDeviceBadParams(t *testing.T) {
	ctrl := &mockBootController{}
	if err := runSetBootDevice(context.Background(), ctrl, json.RawMessage(`{"device":"floppy"}`)); err == nil {
		t.Fatal("want error for invalid device")
	}
	if err := runSetBootDevice(context.Background(), ctrl, json.RawMessage(`not json`)); err == nil {
		t.Fatal("want error for malformed params")
	}
}

// mockPowerControllerOff wraps mockPowerController but returns StateOff for PowerStatus.
type mockPowerControllerOff struct {
	*mockPowerController
}

func (m *mockPowerControllerOff) PowerStatus(_ context.Context) (power.State, error) {
	return power.StateOff, nil
}

// addMachineToCache inserts a machine into the cache's snapshot view,
// preserving any machines already present. Rebuilds the view's machine
// map + MAC indices so accessors (MachineByID, BootActionForMAC)
// resolve it. White-box test helper for the post-decomposition view.
func addMachineToCache(c *Cache, m *model.MachineSnapshot) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	old := c.view.Load()
	v := &snapshotView{
		seqNum:          old.seqNum,
		siteInfo:        old.siteInfo,
		dhcpCfg:         old.dhcpCfg,
		oobCfg:          old.oobCfg,
		machines:        make(map[uuid.UUID]*model.MachineSnapshot, len(old.machines)+1),
		macIndex:        make(map[string]uuid.UUID),
		ambiguousMACs:   make(map[string]struct{}),
		ipByMAC:         make(map[string]string),
		oobReservations: maps.Clone(old.oobReservations),
		images:          maps.Clone(old.images),
		sshKeys:         old.sshKeys,
	}
	for id, existing := range old.machines {
		v.machines[id] = existing
		v.indexMachineInterfaces(id, existing.Interfaces)
	}
	v.machines[m.ID] = m
	v.indexMachineInterfaces(m.ID, m.Interfaces)
	c.view.Store(v)
}

// fakeClaimer is an injected Claimer for tests. It records every
// claim attempt and returns a configured outcome (success or error).
type fakeClaimer struct {
	calls    []uuid.UUID
	resultOp *model.Operation // returned on success; nil signals "already claimed"
	err      error            // returned as a hard failure
}

func (f *fakeClaimer) ClaimOp(_ context.Context, opID uuid.UUID) (*model.Operation, error) {
	f.calls = append(f.calls, opID)
	if f.err != nil {
		return nil, f.err
	}
	return f.resultOp, nil
}

func TestExecutor_ProcessOps(t *testing.T) {
	cache := NewCache(nil)

	machineID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID,
		BMCIP: "10.0.0.100"})

	mock := &mockPowerController{}

	for _, tc := range []struct {
		opType model.OperationType
		expect string
	}{
		{model.OpPowerOn, "on"},
		{model.OpPowerOff, "off"},
		{model.OpPowerCycle, "cycle"},
	} {
		opID := uuid.New()

		// Add pending op to cache
		cache.opState.mu.Lock()
		cache.opState.ops[opID.String()] = &model.Operation{
			ID:        opID,
			MachineID: machineID,
			Type:      tc.opType,
			Status:    model.OpPending,
		}
		cache.opState.mu.Unlock()

		exec := &Executor{
			cache: cache,
			factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
				return mock, nil
			},
			workerID: "test-worker",
			log:      testLogger(),
		}

		// Execute directly (skip claim since no API server)
		exec.runPowerOp(context.Background(), model.Operation{
			ID:        opID,
			MachineID: machineID,
			Type:      tc.opType,
			Status:    model.OpPending,
		})

		if mock.lastAction != tc.expect {
			t.Fatalf("expected action %q for %s, got %q", tc.expect, tc.opType, mock.lastAction)
		}

		// Check op was marked completed in cache
		cache.opState.mu.RLock()
		op := cache.opState.ops[opID.String()]
		cache.opState.mu.RUnlock()
		if op.Status != model.OpCompleted {
			t.Fatalf("expected completed for %s, got %s", tc.opType, op.Status)
		}
	}
}

// TestExecutor_PowerOp_RoutesToPDUWhenEnvelopePresent verifies the
// explicit transport selection: an op carrying a pdu_runtime envelope
// (operator passed --via pdu) routes to a PDU controller and does NOT
// require a BMC IP, even though the machine has none. Without this, a
// BMC-less switch could never be power-controlled.
func TestExecutor_PowerOp_RoutesToPDUWhenEnvelopePresent(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	cache.opState.mu.Lock()
	// Note: NO BMCIP — this is a BMC-less switch.
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerCycle, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	var gotPDURT *model.PDURuntime
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, pduRT *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			gotPDURT = pduRT
			return mock, nil
		},
		workerID: "test-worker",
		log:      testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerCycle,
		Params: json.RawMessage(`{"pdu_runtime":{"outlets":[{"pdu_host":"10.254.240.154","outlet":4,"username":"apc","password":"pw"}]}}`),
	})

	if gotPDURT == nil {
		t.Fatal("factory received nil pduRT; envelope was not routed as PDU transport")
	}
	if len(gotPDURT.Outlets) != 1 || gotPDURT.Outlets[0].PDUHost != "10.254.240.154" || gotPDURT.Outlets[0].Outlet != 4 {
		t.Fatalf("unexpected pduRT: %+v", gotPDURT)
	}
	if mock.lastAction != "cycle" {
		t.Fatalf("expected cycle action, got %q", mock.lastAction)
	}
	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpCompleted {
		t.Fatalf("expected completed (PDU op should not fail on missing BMCIP), got %s", op.Status)
	}
}

// TestExecutor_PowerOp_NoBMCNoPDUFails verifies a machine with neither a
// BMC IP nor a pdu_runtime envelope fails cleanly (can't pick a transport).
func TestExecutor_PowerOp_NoBMCNoPDUFails(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()
	called := false
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			called = true
			return &mockPowerController{}, nil
		},
		workerID: "test-worker",
		log:      testLogger(),
	}
	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
	})
	if called {
		t.Error("factory should not be called when neither BMC nor PDU transport is available")
	}
	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed, got %s", op.Status)
	}
}

func TestExecutor_FailedOp(t *testing.T) {
	cache := NewCache(nil)

	machineID := uuid.New()

	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID,
		BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpPowerOn,
		Status:    model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{failAction: true}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		workerID: "test-worker",
		log:      testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpPowerOn,
		Status:    model.OpPending,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed, got %s", op.Status)
	}

	// The outbox carries the execution-start phase, then the failure with
	// its error.
	_, updates := cache.DrainOutbox()
	if len(updates) != 2 {
		t.Fatalf("expected phase + failed updates in outbox, got %+v", updates)
	}
	if updates[0].Phase != string(model.OpPowerOn) {
		t.Fatalf("expected phase %s first, got %+v", model.OpPowerOn, updates[0])
	}
	if updates[1].Status != "failed" {
		t.Fatalf("expected failed update, got %s", updates[1].Status)
	}
	if len(updates[1].Result) == 0 {
		t.Fatal("expected error in result")
	}
}

func TestExecutor_NoBMC(t *testing.T) {
	cache := NewCache(nil)

	machineID := uuid.New()

	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID}) // no BMCIP
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpPowerOn,
		Status:    model.OpPending,
	}
	cache.opState.mu.Unlock()

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return nil, nil
		},
		workerID: "test-worker",
		log:      testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpPowerOn,
		Status:    model.OpPending,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for no BMC, got %s", op.Status)
	}
}

func TestExecutor_LifecycleOp_HappyPath(t *testing.T) {
	for _, opType := range []model.OperationType{model.OpCommission, model.OpDeploy} {
		t.Run(string(opType), func(t *testing.T) {
			cache := NewCache(nil)
			machineID := uuid.New()

			opID := uuid.New()

			addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
			cache.opState.mu.Lock()
			cache.opState.ops[opID.String()] = &model.Operation{
				ID: opID, MachineID: machineID, Type: opType, Status: model.OpPending,
			}
			cache.opState.mu.Unlock()

			mock := &mockPowerController{}
			exec := &Executor{
				cache: cache,
				factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
					return mock, nil
				},
				log: testLogger(),
			}

			exec.runLifecycleOp(context.Background(), model.Operation{
				ID: opID, MachineID: machineID, Type: opType,
			})

			if mock.lastBootDevice != power.BootPXE {
				t.Fatalf("expected PXE boot device, got %v", mock.lastBootDevice)
			}
			if mock.lastAction != "cycle" {
				t.Fatalf("expected power cycle, got %q", mock.lastAction)
			}

			cache.opState.mu.RLock()
			op := cache.opState.ops[opID.String()]
			cache.opState.mu.RUnlock()
			if op.Status != model.OpRunning {
				t.Fatalf("expected running after lifecycle boot, got %s", op.Status)
			}
		})
	}
}

func TestExecutor_LifecycleOp_SetBootDeviceFailure(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()

	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{failBootDevice: true}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed on boot device error, got %s", op.Status)
	}
	// Power cycle must not have been called
	if mock.lastAction == "cycle" {
		t.Fatal("power cycle should not be called after SetBootDevice failure")
	}
}

func TestExecutor_LifecycleOp_NoBMC(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()

	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID}) // no BMCIP
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return nil, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for no BMC, got %s", op.Status)
	}
}

func TestExecutor_LifecycleOp_NotRedriven(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()

	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning,
	}
	cache.opState.mu.Unlock()

	ops := cache.PendingLifecycleOps()
	if len(ops) != 0 {
		t.Fatalf("running op should not appear in PendingLifecycleOps, got %d", len(ops))
	}
}

func TestCache_PendingPowerOps(t *testing.T) {
	cache := NewCache(nil)

	machineID := uuid.New()
	// The machine must be in the identity view: the pending-op getters
	// filter out ops for out-of-view machines (fleet-wide op fan-out).
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})
	ops := map[string]*model.Operation{
		"power-pending": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpPowerOn, Status: model.OpPending,
		},
		"power-claimed": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpPowerOff, Status: model.OpClaimed,
		},
		"deploy-pending": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpDeploy, Status: model.OpPending,
		},
	}

	cache.opState.mu.Lock()
	for _, op := range ops {
		cache.opState.ops[op.ID.String()] = op
	}
	cache.opState.mu.Unlock()

	pending := cache.PendingPowerOps()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending power op, got %d", len(pending))
	}
	if pending[0].Type != model.OpPowerOn {
		t.Fatalf("expected power_on, got %s", pending[0].Type)
	}
}

// TestCache_PendingPowerOps_IncludesSetBootDevice is a regression test:
// set_boot_device is a power-plane op (executeOp routes it through the same
// power.Controller path as the power verbs), so PendingPowerOps must return it.
// Before the fix it matched the default branch in BOTH getters and was orphaned
// pending forever — never claimed, never executed.
func TestCache_PendingPowerOps_IncludesSetBootDevice(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})

	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID,
		Type: model.OpSetBootDevice, Status: model.OpPending,
		Params: json.RawMessage(`{"device":"pxe"}`),
	}
	cache.opState.mu.Unlock()

	pending := cache.PendingPowerOps()
	if len(pending) != 1 {
		t.Fatalf("expected set_boot_device in PendingPowerOps, got %d ops", len(pending))
	}
	if pending[0].Type != model.OpSetBootDevice {
		t.Fatalf("expected set_boot_device, got %s", pending[0].Type)
	}
}

// TestPendingOps_EveryOpTypeIsBucketed is the structural guard that prevents
// the crack from reopening: every model.OperationType must be returned
// by at least one pending-op getter, or an op of that type is silently orphaned
// (never claimed, never executed, sits pending forever). A new op type that
// gets an executeOp case but is forgotten in both getters fails here.
func TestPendingOps_EveryOpTypeIsBucketed(t *testing.T) {
	for _, opType := range model.AllOperationTypes {
		t.Run(string(opType), func(t *testing.T) {
			cache := NewCache(nil)
			opID := uuid.New()
			machineID := uuid.New()
			addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})
			cache.opState.mu.Lock()
			cache.opState.ops[opID.String()] = &model.Operation{
				ID: opID, MachineID: machineID,
				Type: opType, Status: model.OpPending,
			}
			cache.opState.mu.Unlock()

			inPower := len(cache.PendingPowerOps()) == 1
			inLifecycle := len(cache.PendingLifecycleOps()) == 1
			if !inPower && !inLifecycle {
				t.Fatalf("op type %q is in NEITHER PendingPowerOps nor PendingLifecycleOps "+
					"— it would be orphaned pending forever. Route it in one of the getters "+
					"(internal/worker/cache.go).", opType)
			}
			if inPower && inLifecycle {
				t.Fatalf("op type %q is in BOTH getters — it would be claimed/executed twice "+
					"per tick. It must be in exactly one.", opType)
			}
		})
	}
}

func TestExecutor_PowerOp_BootDevicePXE(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerCycle, Status: model.OpPending,
		Params: json.RawMessage(`{"boot_device":"pxe"}`),
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerCycle,
		Params: json.RawMessage(`{"boot_device":"pxe"}`),
	})

	if mock.lastBootDevice != power.BootPXE {
		t.Fatalf("expected PXE boot device, got %v", mock.lastBootDevice)
	}
	if mock.lastAction != "cycle" {
		t.Fatalf("expected cycle, got %q", mock.lastAction)
	}
}

func TestExecutor_PowerOp_BootDeviceDisk(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
		Params: json.RawMessage(`{"boot_device":"disk"}`),
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
		Params: json.RawMessage(`{"boot_device":"disk"}`),
	})

	if mock.lastBootDevice != power.BootDisk {
		t.Fatalf("expected disk boot device, got %v", mock.lastBootDevice)
	}
}

func TestExecutor_PowerOp_UnknownBootDevice(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
		Params: json.RawMessage(`{"boot_device":"cdrom"}`),
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for unknown boot device, got %s", op.Status)
	}
}

func TestExecutor_PowerOp_NilController(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return nil, nil
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for nil controller, got %s", op.Status)
	}
}

func TestExecutor_PowerOp_FactoryError(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return nil, fmt.Errorf("connection refused")
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for factory error, got %s", op.Status)
	}
}

func TestExecutor_PowerOp_MachineNotInCache(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return nil, nil
		},
		log: testLogger(),
	}

	exec.runPowerOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpPowerOn,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed for missing machine, got %s", op.Status)
	}
}

func TestExecutor_LifecycleOp_PowerOff_PowersOn(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	// Override PowerStatus to return StateOff
	mockOff := &mockPowerControllerOff{mockPowerController: mock}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mockOff, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy,
	})

	if mock.lastBootDevice != power.BootPXE {
		t.Fatalf("expected PXE boot device, got %v", mock.lastBootDevice)
	}
	if mock.lastAction != "on" {
		t.Fatalf("expected power on (machine was off), got %q", mock.lastAction)
	}
}

func TestExecutor_LifecycleOp_Deploy_PreparesLogs(t *testing.T) {
	db := openTestDB(t)
	cache := NewCache(db)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy,
	})

	// Deploy logs should be prepared
	if got := db.GetCurrentDeployOp(machineID.String()); got != opID.String() {
		t.Fatalf("expected current deploy op %s, got %s", opID, got)
	}

	// Phase should be set to pxe_booting
	op := cache.GetOperation(opID.String())
	if op.Phase != "pxe_booting" {
		t.Fatalf("expected phase pxe_booting, got %s", op.Phase)
	}
}

func TestExecutor_LifecycleOp_PowerCycleFails(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{failAction: true}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission,
	})

	cache.opState.mu.RLock()
	op := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if op.Status != model.OpFailed {
		t.Fatalf("expected failed after power cycle error, got %s", op.Status)
	}
}

// TestExecutor_LifecycleOp_Commission_TracksCurrentOp covers the change:
// commission, like deploy, boots the machine and emits the boot-prefix phases,
// so it also sets the per-machine current-op pointer (SetCurrentDeployOp — the
// stale-insert guard for the log ring, generic across booting op types). It
// must point at the commission op after the executor cycles power.
func TestExecutor_LifecycleOp_Commission_TracksCurrentOp(t *testing.T) {
	db := openTestDB(t)
	cache := NewCache(db)
	machineID := uuid.New()
	opID := uuid.New()

	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpPending,
	}
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		log: testLogger(),
	}

	exec.runLifecycleOp(context.Background(), model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission,
	})

	// Commission now tracks the current op (same as deploy) so its agent log
	// envelopes are accepted into the per-machine ring and its boot-prefix
	// phases (power_cycle, pxe_booting) animate the watch TUI.
	if got := db.GetCurrentDeployOp(machineID.String()); got != opID.String() {
		t.Fatalf("expected current op %s for commission, got %q", opID.String(), got)
	}
	// The boot-prefix phase must have advanced past power_cycle to pxe_booting.
	cache.opState.mu.RLock()
	phase := cache.opState.ops[opID.String()].Phase
	cache.opState.mu.RUnlock()
	if phase != "pxe_booting" {
		t.Fatalf("expected commission op phase pxe_booting, got %q", phase)
	}
}

func TestCache_PendingLifecycleOps(t *testing.T) {
	cache := NewCache(nil)

	machineID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID})
	ops := map[string]*model.Operation{
		"commission-pending": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpCommission, Status: model.OpPending,
		},
		"deploy-pending": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpDeploy, Status: model.OpPending,
		},
		"commission-running": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpCommission, Status: model.OpRunning,
		},
		"power-pending": {
			ID: uuid.New(), MachineID: machineID,
			Type: model.OpPowerOn, Status: model.OpPending,
		},
	}

	cache.opState.mu.Lock()
	for _, op := range ops {
		cache.opState.ops[op.ID.String()] = op
	}
	cache.opState.mu.Unlock()

	pending := cache.PendingLifecycleOps()
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending lifecycle ops, got %d", len(pending))
	}
	for _, op := range pending {
		if op.Status != model.OpPending {
			t.Fatalf("expected pending status, got %s", op.Status)
		}
		if op.Type != model.OpCommission && op.Type != model.OpDeploy {
			t.Fatalf("unexpected op type in lifecycle ops: %s", op.Type)
		}
	}
}

// TestExecutor_ClaimOp_HappyPath verifies the executor calls the
// injected Claimer with the correct op id and proceeds to execute
// the op when the claim succeeds. The Claimer interface replaces
// the deleted HTTP /claim endpoint; per the design, claim flows
// over the WebSocket using the connection's authenticated
// identity rather than a client-provided worker_id field.
func TestExecutor_ClaimOp_HappyPath(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})

	opID := uuid.New()
	op := model.Operation{
		ID: opID, MachineID: machineID,
		Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &op
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	claimer := &fakeClaimer{
		resultOp: &model.Operation{
			ID: opID, MachineID: machineID,
			Type: model.OpPowerOn, Status: model.OpClaimed,
			ClaimedBy: "test-worker",
		},
	}

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		claimer:  claimer,
		workerID: "test-worker",
		log:      testLogger(),
	}

	exec.executeOp(context.Background(), op)

	if len(claimer.calls) != 1 {
		t.Fatalf("Claimer.ClaimOp called %d times, want 1", len(claimer.calls))
	}
	if claimer.calls[0] != opID {
		t.Errorf("ClaimOp opID: got %s, want %s", claimer.calls[0], opID)
	}
	if mock.lastAction != "on" {
		t.Errorf("expected power-on after successful claim, got %q", mock.lastAction)
	}
}

// TestExecutor_ClaimOp_AlreadyClaimed verifies the executor backs
// off (does NOT execute the op) when another worker wins the claim
// race. The local op stays marked claimed — NOT rolled back to
// pending — so the executor stops re-attempting the claim every
// tick (observed live: ~78 retries over ~6 min before this fix).
// If the local mark is ever wrong, the next snapshot push
// full-replaces c.ops (Cache.ApplySnapshot) and restores truth.
// The metric outcome is "lost", not "error".
func TestExecutor_ClaimOp_AlreadyClaimed(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})

	opID := uuid.New()
	op := model.Operation{
		ID: opID, MachineID: machineID,
		Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &op
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	// resultOp=nil + err=nil signals "no rows updated; somebody else claimed it".
	claimer := &fakeClaimer{resultOp: nil, err: nil}
	metrics := NewSiteMetrics(prometheus.NewRegistry())

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		claimer:  claimer,
		workerID: "test-worker",
		log:      testLogger(),
		metrics:  metrics,
	}
	exec.executeOp(context.Background(), op)

	if mock.lastAction != "" {
		t.Errorf("expected no power action after failed claim, got %q", mock.lastAction)
	}
	cache.opState.mu.RLock()
	got := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if got.Status != model.OpClaimed {
		t.Errorf("expected status to stay claimed (stop the per-tick claim hammer), got %s", got.Status)
	}
	if n := testutil.ToFloat64(metrics.ClaimsTotal.WithLabelValues(ClaimLost)); n != 1 {
		t.Errorf("claims_total{outcome=lost}: got %v, want 1", n)
	}
	if n := testutil.ToFloat64(metrics.ClaimsTotal.WithLabelValues(ClaimErr)); n != 0 {
		t.Errorf("claims_total{outcome=error}: got %v, want 0", n)
	}

	// The op must no longer appear in the pending scan — the next
	// tick would otherwise re-fire the claim.
	if pending := cache.PendingPowerOps(); len(pending) != 0 {
		t.Errorf("expected no pending power ops after lost claim, got %d", len(pending))
	}
}

// TestExecutor_ClaimOp_TransportError verifies the executor backs
// off identically to the already-claimed path when the claim
// errors (WS disconnected, RPC timeout, etc.). The op stays in
// pending so the next tick — after WS reconnect — can retry.
func TestExecutor_ClaimOp_TransportError(t *testing.T) {
	cache := NewCache(nil)
	machineID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})

	opID := uuid.New()
	op := model.Operation{
		ID: opID, MachineID: machineID,
		Type: model.OpPowerOn, Status: model.OpPending,
	}
	cache.opState.mu.Lock()
	cache.opState.ops[opID.String()] = &op
	cache.opState.mu.Unlock()

	mock := &mockPowerController{}
	claimer := &fakeClaimer{err: fmt.Errorf("websocket not connected")}
	metrics := NewSiteMetrics(prometheus.NewRegistry())

	exec := &Executor{
		cache: cache,
		factory: func(_ context.Context, _ *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			return mock, nil
		},
		claimer:  claimer,
		workerID: "test-worker",
		log:      testLogger(),
		metrics:  metrics,
	}
	exec.executeOp(context.Background(), op)

	if mock.lastAction != "" {
		t.Errorf("expected no power action when claim transport-fails, got %q", mock.lastAction)
	}
	cache.opState.mu.RLock()
	got := cache.opState.ops[opID.String()]
	cache.opState.mu.RUnlock()
	if got.Status != model.OpPending {
		t.Errorf("expected status rolled back to pending, got %s", got.Status)
	}
	if n := testutil.ToFloat64(metrics.ClaimsTotal.WithLabelValues(ClaimErr)); n != 1 {
		t.Errorf("claims_total{outcome=error}: got %v, want 1", n)
	}
	if n := testutil.ToFloat64(metrics.ClaimsTotal.WithLabelValues(ClaimLost)); n != 0 {
		t.Errorf("claims_total{outcome=lost}: got %v, want 0", n)
	}
}
