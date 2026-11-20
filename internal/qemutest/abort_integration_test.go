//go:build integration

package qemutest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestAbortStopsDeployAgent reproduces the first half of a real-hardware
// property-test history: an operator aborted a deploy that was running,
// global failed the operation, and nothing stopped the agent, which went on
// installing an OS nobody had asked for any more.
//
// A deploy runs to the OS extract, the long phase with no report of its own.
// The hub then fails the operation exactly as an abort does (op_removed to
// the worker, nothing sent to the host). The agent must find out from its
// next report to the site worker, stop without installing a bootloader, and
// exit with the status that makes the boot hook hold the host in recovery
// instead of rebooting it.
func TestAbortStopsDeployAgent(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	const name = "abort-stops-agent"
	systemUUID, mac := matrixIdentity(name)
	machineID := uuid.New()
	slug := name

	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	snap := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:            machineID,
			MAC:           mac,
			State:         model.MachineStateDeploying,
			PXEEligible:   true,
			SystemUUID:    systemUUID,
			StorageScript: snapshotStorageScript(storageSingle),
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

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.UEFI,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	vm.Start(ctx)

	// The agent logs slogkit JSON envelopes to stderr, which is the serial
	// console. This line opens the OS extract.
	vm.WaitForSerial(ctx, `"msg":"extracting root filesystem (rsync)"`, qemutest.Budgets.DeployComplete)

	env.Hub.FailOperation(slug, deployOp.ID, model.MachineStateFailed)

	// The agent's liveness report is at most five seconds away; the budget
	// covers a stalled scheduler, not the mechanism.
	vm.WaitForSerial(ctx, "this operation is no longer active", qemutest.Budgets.RecoveryMarkerShort)
	vm.WaitForSerial(ctx, "agent exited with code 3", qemutest.Budgets.RecoveryMarkerShort)

	for _, marker := range []string{
		`"msg":"installing systemd-boot"`,
		`"msg":"deploy complete"`,
		`"msg":"rebooting"`,
	} {
		if vm.Serial.Contains(marker) {
			t.Errorf("the aborted agent went on to %s", marker)
		}
	}
	if vm.Serial.Contains(`"msg":"extraction complete"`) {
		// Not a failure: the stop then came at the next phase report
		// instead of mid-extract. Worth knowing when reading a run.
		t.Log("the extract finished before the agent's liveness report; the stop came at the phase boundary")
	} else {
		t.Log("the agent stopped in the middle of the OS extract")
	}
	if obs := env.Hub.Observations(slug); len(obs) != 0 {
		for _, o := range obs {
			if o.Obs.Event == "deploy_complete" {
				t.Errorf("the hub received a deploy_complete for the aborted operation: %+v", o.Obs)
			}
		}
	}
}
