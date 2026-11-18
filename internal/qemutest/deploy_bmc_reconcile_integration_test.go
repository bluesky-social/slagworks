//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeployBMCReconcile is the phase 2.4 full-VM in-band integration
// test for the BMC reconcile flow:
//
//   - VM boots from PXE with a simulated BMC attached via QEMU's
//     ipmi-bmc-extern + isa-ipmi-kcs (backed by an out-of-process
//     ipmi_sim subprocess).
//   - Agent runs the normal deploy flow; after finalizing it loads
//     the kernel's ipmi_si + ipmi_devintf modules, opens /dev/ipmi0,
//     and runs bmcreconcile against it.
//   - Reconcile provisions the monitor (USER) user with a fresh
//     password and sets persistent PXE boot order. It provisions no
//     slagworks account: slagworksd drives BMCs with the slot-2
//     bmc-creds-admin account.
//   - Agent posts a single deploy_complete with BMCCreds to the site
//     worker, which queues the observation to the FakeHub.
//   - Test asserts the observation arrived, decodes BMCCreds, checks
//     monitor got a 12-char password and a non-reserved slot.
//
// This is the test the deleted phase-2.1 LAN integration test was a
// stand-in for. The in-band /dev/ipmi0 path doesn't go through RMCP+
// auth, so the ipmi_sim LAN-auth-from-Go-subprocess bug doesn't
// apply here.
func TestDeployBMCReconcile(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ipmi_sim"); err != nil {
		t.Skip("ipmi_sim not installed; skipping full-VM BMC reconcile test")
	}

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID := "12345678-1234-1234-1234-aaaaaaaaaaaa"
	mac := "52:54:00:de:ad:50"
	machineID := uuid.New()
	slug := "pop3-bmc-reconcile"

	// Create a deploy operation in "running" status so:
	//   - DeployOpForMAC returns it (status != completed/failed) and
	//     the iPXE boot script includes slagworks.machine= and
	//     slagworks.op= on the kernel cmdline.
	//   - Agent constructs an agentReporter (reporter != nil), so
	//     the bmc_reconcile phase actually fires.
	//   - status=running (not pending) mirrors a mid-flight op; the
	//     qemutest SiteWorker starts no executor at all, so nothing
	//     would try to BMC-power-cycle the simulated VM either way.
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	deployOp.Status = model.OpRunning

	snap := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateDeploying,
			PXEEligible: true,
			SystemUUID:  systemUUID,
			StorageScript: &model.SnapshotStorageScript{
				Name:    "default",
				Content: defaultStorageScript,
			},
		}).
		AddOperation(deployOp).
		Build()
	env.Hub.Push(slug, snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	// Start the simulated BMC before the VM so the QEMU chardev's
	// reconnect-loop attaches on first try (rather than relying on
	// QEMU's reconnect=10 fallback). Pass Network so ipmi_sim runs
	// inside the per-test netns — qemu (also netns'd)
	// connects to 127.0.0.1:<port>, which is the netns's lo, and
	// both processes must share it.
	bmc := env.NewBMCSim(t, qemutest.BMCSimConfig{
		// Default seed (slot 1 anon + slot 2 ADMIN). Reconcile must
		// add monitor on top.
		Network: network,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	bmc.Start(ctx)

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
		BMC:        bmc,
	})

	vm.Start(ctx)

	// "deploy complete" fires AFTER the agent has reported
	// deploy_complete + BMCCreds — we can't quite race-rely on that
	// ordering at the serial level (the deploy-complete print is the
	// agent's last action), so we wait for the observation directly
	// and let the serial wait handle the "did the VM finish" check.
	vm.WaitForSerial(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete)

	obs, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID)
	if err != nil {
		t.Fatalf("wait for deploy_complete observation: %v", err)
	}
	if len(obs.Obs.Data) == 0 {
		t.Fatal("deploy_complete observation has empty Data; expected BMCCreds")
	}

	var creds model.BMCCreds
	if err := json.Unmarshal(obs.Obs.Data, &creds); err != nil {
		t.Fatalf("decode BMCCreds from observation Data: %v\nraw=%s", err, string(obs.Obs.Data))
	}

	if !creds.BootOrderPersisted {
		t.Error("BootOrderPersisted: expected true (reconcile always writes boot order on success)")
	}

	if creds.Monitor == nil {
		t.Fatal("Monitor: expected non-nil (monitor user is missing on a fresh BMC, must be created)")
	}
	if creds.Monitor.Username != "monitor" {
		t.Errorf("Monitor.Username: got %q, want monitor", creds.Monitor.Username)
	}
	if got := len(creds.Monitor.Password); got != 12 {
		t.Errorf("Monitor.Password length: got %d, want 12", got)
	}
	if creds.Monitor.IPMIUserSlot < 3 {
		t.Errorf("Monitor.IPMIUserSlot: got %d, want >= 3 (slot 1 anon, slot 2 admin reserved)",
			creds.Monitor.IPMIUserSlot)
	}
}
