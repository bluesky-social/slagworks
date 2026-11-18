//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// firmwareBootFailMarker is what SeaBIOS emits on the serial when it has
// booted but finds nothing bootable — the deterministic post-boot marker
// for a disk-only, strict-boot BIOS VM with a blank disk. It appears once
// per boot, so it is also the marker that must RE-appear after a reset.
const firmwareBootFailMarker = "No bootable device"

// testPowerFactory is the qemutest analogue of the production
// powerControllerFactory: it uses the credential envelope,
// power.BuildBMCDialOptions, and power.NewBMCController. Manufacturer is
// left empty so the bmclib path is selected, never SMCIPMITool.
func testPowerFactory(ctx context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
	if amtRT != nil {
		return nil, &testError{"test factory does not support amt_runtime"}
	}
	if pduRT != nil {
		return nil, &testError{"test factory does not support pdu_runtime"}
	}
	if m.BMCIP == "" {
		return nil, &testError{"machine has no BMC IP"}
	}
	user, pass := rt.Username, rt.Password
	if user == "" {
		user = "admin"
	}
	host, opts := power.BuildBMCDialOptions(m.BMCIP, rt.IpmiCipher())
	ctrl, err := power.NewBMCController(ctx, host, user, pass, 30*time.Second, nil, opts...)
	if err != nil {
		return nil, err
	}
	ctrl.SetIPMICipher(rt.IpmiCipher())
	return ctrl, nil
}

// testError is a tiny error value for the test factory so failures show
// up verbatim in the executor's op result.
type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestFakeBMCBmclibOpen exercises the TLS, Basic Auth, and service-root
// boundary without requiring QEMU. In particular, gofish requests the
// trailing-slash /redfish/v1/ path before it installs Basic Auth.
func TestFakeBMCBmclibOpen(t *testing.T) {
	bmc := qemutest.NewFakeBMC(t, filepath.Join(t.TempDir(), "unused-qmp.sock"))
	machine := &model.MachineSnapshot{BMCIP: bmc.Host()}
	runtime := model.BMCRuntime{
		Username: qemutest.FakeBMCUser,
		Password: qemutest.FakeBMCPass,
	}

	ctrl, err := testPowerFactory(context.Background(), machine, runtime, nil, nil)
	if err != nil {
		t.Fatalf("open real bmclib controller against FakeBMC: %v", err)
	}
	if err := ctrl.Close(context.Background()); err != nil {
		t.Fatalf("close real bmclib controller: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+bmc.Host()+"/redfish/v1/Systems", nil)
	if err != nil {
		t.Fatalf("create unauthenticated Systems request: %v", err)
	}
	resp, err := bmc.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("send unauthenticated Systems request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Systems status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// bmcRuntimeParams marshals a params blob carrying only bmc_runtime
// credentials (plus an optional power-op boot device).
func bmcRuntimeParams(t *testing.T, bootDevice string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(model.OpParams{
		BMCRuntime: &model.BMCRuntime{
			Username: qemutest.FakeBMCUser,
			Password: qemutest.FakeBMCPass,
		},
		BootDevice: bootDevice,
	})
	if err != nil {
		t.Fatalf("marshal bmc_runtime params: %v", err)
	}
	return raw
}

// bmcRuntimeJSON marshals a standalone BMCRuntime block for the hub
// PowerStatus RPC.
func bmcRuntimeJSON(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(model.BMCRuntime{
		Username: qemutest.FakeBMCUser,
		Password: qemutest.FakeBMCPass,
	})
	if err != nil {
		t.Fatalf("marshal bmc_runtime: %v", err)
	}
	return raw
}

// pushPendingOp registers a pending op on the FakeHub and fans it (with
// the provided snapshot) so the worker's executor drives it.
func pushPendingOp(t *testing.T, env *qemutest.Env, slug string, network *qemutest.Network, machine qemutest.Machine, op model.Operation) {
	t.Helper()
	snap := env.NewSnapshot(slug, network).
		AddMachine(machine).
		AddOperation(op).
		Build()
	env.Hub.Push(slug, snap)
}

// TestPowerCycleViaBMC proves the full production path: FakeHub -> WS
// claim -> Executor -> real BMCController -> bmclib -> TLS FakeBMC ->
// QMP. A disk-only strict-boot BIOS VM is running; a pending OpPowerCycle
// carrying only bmc_runtime credentials is pushed; the executor claims it
// over WS, forces a restart (QMP system_reset), reports a terminal update
// to FakeHub, and a fresh firmware marker appears after the serial cursor.
func TestPowerCycleViaBMC(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	machineID := uuid.New()
	mac := "52:54:00:de:ad:e1"
	slug := "powercycle-bmc"

	// Build the VM first so we can hand its dedicated FakeBMC QMP socket
	// path to the FakeBMC (the socket is exposed before Start).
	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		BootOrder:  "c",
		StrictBoot: true,
		BMCQMPPath: filepath.Join(t.TempDir(), "qmp-bmc.sock"),
		DiskSize:   "1G",
	})
	bmc := qemutest.NewFakeBMC(t, vm.BMCQMPPath())

	machine := qemutest.Machine{
		ID:    machineID,
		MAC:   mac,
		BMCIP: bmc.Host(),
		State: model.MachineStateNew,
	}

	// Phase A: push identity snapshot so the worker knows the machine.
	snap := env.NewSnapshot(slug, network).AddMachine(machine).Build()
	env.Hub.Push(slug, snap)

	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		PowerFactory: testPowerFactory,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx)

	// Initial firmware marker (disk-only strict boot, blank disk).
	vm.WaitForSerial(ctx, firmwareBootFailMarker, qemutest.Budgets.BMCPowerCycleMarker)
	cursor := vm.Serial.Cursor()

	op := qemutest.NewOperation(machineID, model.OpPowerCycle)
	op.Params = bmcRuntimeParams(t, "")

	pushPendingOp(t, env, slug, network, machine, op)

	// The executor must drive a real ForceRestart through bmclib -> QMP
	// and report completion back to the hub (not just locally).
	upd, err := env.Hub.WaitForOpUpdate(ctx, op.ID.String(), string(model.OpCompleted))
	if err != nil {
		t.Fatalf("wait for completed op update: %v", err)
	}
	if upd.OperationID != op.ID.String() {
		t.Fatalf("op update has wrong op id: %q", upd.OperationID)
	}

	// ForceRestart is what bmclib maps PowerCycle to on the redfish path.
	if !hasReset(bmc.Actions(), "ForceRestart") {
		t.Fatalf("FakeBMC did not observe ForceRestart; actions=%v", bmc.Actions())
	}

	// A fresh firmware marker must appear strictly after the cursor taken
	// before the power cycle.
	vm.WaitForSerialAfter(ctx, cursor, firmwareBootFailMarker, qemutest.Budgets.BMCPowerCycleMarker)

	sw.Shutdown()
}

// TestPXEBootAfterSetBootDevice proves the boot-override path: a pending
// OpPowerOn with BootDevice="pxe" must PATCH the Redfish Boot object to
// Pxe (HMP boot_set n) BEFORE issuing On (QMP cont), then the real iPXE
// chainload must fetch /boot/ipxe on the serial. The VM is started
// paused (-S) with disk-only strict boot so PowerOn cannot short-circuit
// on an already-On status, and a network-first firmware fallback cannot
// green-light a broken override.
func TestPXEBootAfterSetBootDevice(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	machineID := uuid.New()
	mac := "52:54:00:de:ad:e2"
	slug := "pxe-boot-bmc"

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:    qemutest.BIOS,
		MAC:         mac,
		Network:     network,
		BootOrder:   "c",
		StrictBoot:  true,
		StartPaused: true,
		BMCQMPPath:  filepath.Join(t.TempDir(), "qmp-bmc.sock"),
		DiskSize:    "1G",
	})
	bmc := qemutest.NewFakeBMC(t, vm.BMCQMPPath())

	machine := qemutest.Machine{
		ID:    machineID,
		MAC:   mac,
		BMCIP: bmc.Host(),
		State: model.MachineStateNew,
	}

	snap := env.NewSnapshot(slug, network).AddMachine(machine).Build()
	env.Hub.Push(slug, snap)

	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		PowerFactory: testPowerFactory,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx) // prelaunch (-S): no serial output until the op resumes it

	op := qemutest.NewOperation(machineID, model.OpPowerOn)
	op.Params = bmcRuntimeParams(t, "pxe")

	pushPendingOp(t, env, slug, network, machine, op)

	upd, err := env.Hub.WaitForOpUpdate(ctx, op.ID.String(), string(model.OpCompleted))
	if err != nil {
		t.Fatalf("wait for completed op update: %v", err)
	}
	if upd.OperationID != op.ID.String() {
		t.Fatalf("op update has wrong op id: %q", upd.OperationID)
	}

	// The override must be PATCHed to Pxe BEFORE the On (cont). Not merely
	// present somewhere in the log.
	if !setBootBeforeReset(bmc.Actions(), "Pxe", "On") {
		t.Fatalf("FakeBMC did not observe PATCH Pxe before On; actions=%v", bmc.Actions())
	}

	// The machine must actually land in the real iPXE chainload and fetch
	// the boot script — proving the override took effect on the wire.
	vm.WaitForSerial(ctx, "/boot/ipxe", qemutest.Budgets.PXEBootMarker)

	sw.Shutdown()
}

// waitRunning blocks until QEMU reports "running" on the primary test
// QMP socket, so a PowerStatus read can't race QEMU's brief prelaunch
// phase right after vm.Start.
func waitRunning(ctx context.Context, t *testing.T, vm *qemutest.VM) {
	t.Helper()
	for {
		status, err := vm.QMP.QueryStatus()
		if err == nil && status == "running" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("vm never reached running; last status %q err %v", status, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestPowerStatusReflectsQEMU proves the fake reads QEMU rather than
// request history: while running the hub PowerStatus RPC (through WSConn
// + the real factory) reports on/redfish; a direct QMP stop yields
// paused/off; a QMP cont yields running/on again.
func TestPowerStatusReflectsQEMU(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	machineID := uuid.New()
	mac := "52:54:00:de:ad:e3"
	slug := "powerstatus-bmc"

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		BMCQMPPath: filepath.Join(t.TempDir(), "qmp-bmc.sock"),
		DiskSize:   "1G",
	})
	bmc := qemutest.NewFakeBMC(t, vm.BMCQMPPath())

	machine := qemutest.Machine{
		ID:    machineID,
		MAC:   mac,
		BMCIP: bmc.Host(),
		State: model.MachineStateNew,
	}

	snap := env.NewSnapshot(slug, network).AddMachine(machine).Build()
	env.Hub.Push(slug, snap)

	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		PowerFactory: testPowerFactory,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	vm.Start(ctx)
	waitRunning(ctx, t, vm)

	read := func() wsPowerStatus {
		resp, err := env.Hub.PowerStatus(ctx, slug, machineID, bmcRuntimeJSON(t))
		if err != nil {
			t.Fatalf("hub power_status RPC: %v", err)
		}
		return wsPowerStatus{State: resp.State, Transport: resp.Transport}
	}

	// Running -> on, served over redfish.
	if got := read(); got.State != "on" || got.Transport != "redfish" {
		t.Fatalf("running status = %+v, want on/redfish", got)
	}

	// QMP-stop (the test's primary socket) pauses the VM: the fake must
	// report paused/off.
	if err := vm.QMP.Stop(); err != nil {
		t.Fatalf("qmp stop: %v", err)
	}
	if got := read(); got.State != "off" || got.Transport != "redfish" {
		t.Fatalf("paused status = %+v, want off/redfish", got)
	}

	// QMP-cont resumes: on again.
	if err := vm.QMP.Cont(); err != nil {
		t.Fatalf("qmp cont: %v", err)
	}
	if got := read(); got.State != "on" || got.Transport != "redfish" {
		t.Fatalf("resumed status = %+v, want on/redfish", got)
	}

	// The fake must have queried QEMU (not served a cached flag).
	if len(bmc.Actions()) == 0 {
		t.Fatal("FakeBMC recorded no action; status reads did not reach the fake")
	}

	sw.Shutdown()
}

// wsPowerStatus is a local copy of the struct's readable fields, keeping
// the test package free of a direct ws import.
type wsPowerStatus struct {
	State     string
	Transport string
}

func hasReset(actions []qemutest.BMCAction, resetType string) bool {
	for _, a := range actions {
		if a.Kind == qemutest.BMCActionReset && a.ResetType == resetType {
			return true
		}
	}
	return false
}

// setBootBeforeReset reports whether a PATCH of bootTarget appears at an
// earlier index than a reset of resetType.
func setBootBeforeReset(actions []qemutest.BMCAction, bootTarget, resetType string) bool {
	firstSetBoot := -1
	firstReset := -1
	for i, a := range actions {
		if firstSetBoot < 0 && a.Kind == qemutest.BMCActionSetBoot && a.BootTarget == bootTarget {
			firstSetBoot = i
		}
		if firstReset < 0 && a.Kind == qemutest.BMCActionReset && a.ResetType == resetType {
			firstReset = i
		}
	}
	return firstSetBoot >= 0 && firstReset >= 0 && firstSetBoot < firstReset
}
