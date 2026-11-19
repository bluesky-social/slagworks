//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestAbortThenRedeployReachesPXE replays a real-hardware property-test
// history end to end. A deploy was aborted in its finalizing phase, after
// the bootloader had been installed, and the operator started a fresh
// deploy at once. The site worker armed PXE on the BMC and power cycled the
// host, and the host booted the aborted install off its disk: the
// bootloader step had set BootNext, and firmware honors BootNext ahead of
// any other boot order. The new deploy sat at pxe_booting until the
// stuck-operation sweep failed it half an hour later.
//
// Here deploy A runs until its bootloader is installed, the hub fails it as
// an abort does, and deploy B is created. The executor drives B through the
// real power path (bmclib -> FakeBMC -> QMP reset). The VM's firmware boots
// the network first on every boot unless BootNext says otherwise
// (NetworkBootFirst), which is what a BMC PXE override gives a real host.
// B's power cycle must reach PXE and the deploy agent, and B must then
// install, reboot into its own OS and phone home.
func TestAbortThenRedeployReachesPXE(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	const name = "abort-then-redeploy"
	systemUUID, mac := matrixIdentity(name)
	machineID := uuid.New()
	slug := name

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:         qemutest.UEFI,
		MAC:              mac,
		Network:          network,
		Memory:           "4096",
		SMBIOSUUID:       systemUUID,
		DiskSize:         "10G",
		NetworkBootFirst: true,
		// Powered off until the executor's PowerOn for deploy A, so the
		// host boots exactly once into A.
		StartPaused: true,
		BMCQMPPath:  filepath.Join(t.TempDir(), "qmp-bmc.sock"),
	})
	bmc := qemutest.NewFakeBMC(t, vm.BMCQMPPath())

	machine := qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		BMCIP:         bmc.Host(),
		State:         model.MachineStateCommissioned,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: snapshotStorageScript(storageSingle),
	}
	env.Hub.Push(slug, env.NewSnapshot(slug, network).AddMachine(machine).Build())

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
		PowerFactory: testPowerFactory,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	vm.Start(ctx) // prelaunch (-S) until the executor powers it on

	deployParams := func(destructive bool) json.RawMessage {
		raw, err := json.Marshal(model.OpParams{
			BMCRuntime:  &model.BMCRuntime{Username: qemutest.FakeBMCUser, Password: qemutest.FakeBMCPass},
			Destructive: destructive,
		})
		if err != nil {
			t.Fatalf("marshal deploy params: %v", err)
		}
		return raw
	}

	// Deploy A. The executor claims it, arms PXE and powers the host on.
	opA := qemutest.NewOperation(machineID, model.OpDeploy)
	opA.Params = deployParams(false)
	pushPendingOp(t, env, slug, network, machine, opA)

	// The bootloader step is done: the boot entry exists, and before the
	// fix BootNext already pointed at the half-finished install. The agent
	// is now in the finalizing phase, where the real abort landed.
	vm.WaitForSerial(ctx, `"msg":"systemd-boot installed"`, qemutest.Budgets.DeployComplete)
	afterA := vm.Serial.Cursor()

	// The operator aborts A and deploys again.
	env.Hub.FailOperation(slug, opA.ID, model.MachineStateFailed)
	failed := machine
	failed.State = model.MachineStateFailed
	opB := qemutest.NewOperation(machineID, model.OpDeploy)
	opB.Params = deployParams(true) // A left the disk partitioned
	env.Hub.Push(slug, env.NewSnapshot(slug, network).Seq(2).AddMachine(failed).AddOperation(opB).Build())

	// B's power cycle has to land in the deploy boot script. A host that
	// boots A's disk here never prints it: that is the bug.
	if err := vm.Serial.WaitForAfter(ctx, afterA, "slagworks: deploy boot for mac", qemutest.Budgets.PXEBootMarker); err != nil {
		t.Fatalf("the redeploy's power cycle did not reach the deploy boot script: %v\n"+
			"BMC actions: %+v\n"+
			"the host booted something other than PXE, most likely the aborted deploy's disk through a BootNext "+
			"that deploy left armed", err, bmc.Actions())
	}
	if !setBootBeforeReset(bmc.Actions(), "Pxe", "ForceRestart") {
		t.Errorf("the redeploy did not arm PXE before its power cycle; BMC actions: %+v", bmc.Actions())
	}

	// B then runs to completion on the host: install, reboot, phone home.
	if err := vm.Serial.WaitForAfter(ctx, afterA, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for the redeploy to complete: %v", err)
	}
	obs, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID)
	if err != nil {
		t.Fatalf("waiting for the redeployed host to phone home: %v", err)
	}
	if obs.Obs.OperationID != opB.ID {
		t.Errorf("phone_home names operation %s, want the redeploy %s (aborted deploy: %s)",
			obs.Obs.OperationID, opB.ID, opA.ID)
	}
	for _, o := range env.Hub.Observations(slug) {
		if o.Obs.OperationID == opA.ID && (o.Obs.Event == "deploy_complete" || o.Obs.Event == "phone_home") {
			t.Errorf("the hub received %s for the aborted deploy: %+v", o.Obs.Event, o.Obs)
		}
	}
}
