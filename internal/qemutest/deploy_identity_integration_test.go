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

// TestDeploy_IdentityMismatch verifies the slagworks.expect-uuid
// kernel cmdline check: if the VM's SMBIOS UUID doesn't match the
// SystemUUID baked into the boot script, the agent refuses to
// proceed.
//
// Lives outside TestMatrix because the identity check is a
// separate axis from firmware/storage/failure-mode — adding
// "identity-mismatch" as a failure-mode would force every cell to
// also vary expect-uuid vs. smbios, which dominates the
// combinatorics with no extra coverage.
func TestDeploy_IdentityMismatch(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	// Snapshot declares one UUID; VM boots with a different one.
	expectedUUID := "11111111-1111-1111-1111-111111111111"
	vmUUID := "22222222-2222-2222-2222-222222222222"
	mac := "52:54:00:de:ad:60"
	machineID := uuid.New()

	// Fail-closed (Tier 2): a transient deploying state needs a matching
	// claimed deploy op in opState or the worker serves BootLocal and the
	// VM never chainloads to reach the identity check. Mirror
	// ClaimOperation's invariant.
	snap := env.NewSnapshot("pop3-test", network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateDeploying,
			PXEEligible: true,
			SystemUUID:  expectedUUID,
			StorageScript: &model.SnapshotStorageScript{
				Name:    "default",
				Content: defaultStorageScript,
			},
		}).
		AddOperation(model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			Type:      model.OpDeploy,
			Status:    model.OpClaimed,
			Version:   1,
		}).
		Build()
	env.Hub.Push("pop3-test", snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         "pop3-test",
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: vmUUID, // deliberately mismatched
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx)

	// Same FATAL-marker shape as the matrix safety-check cells: the
	// success path prints `slagworks: identity verified`, not
	// `identity check`. Matching the FATAL marker keeps us
	// resilient to future success-path strings that might happen
	// to contain `identity check`.
	if err := vm.Serial.WaitFor(ctx, "FATAL during identity check", qemutest.Budgets.FATALMarker); err != nil {
		t.Fatalf("waiting for identity check FATAL: %v", err)
	}
	// Never got to "deploy complete".
	if vm.Serial.Contains(`"msg":"deploy complete"`) {
		t.Error("deploy should have failed identity check")
	}
}
