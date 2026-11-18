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

// TestLogRelay verifies that deploy-time agent logs are captured
// into the site worker's in-memory log ring, drained over the WS
// connection, and land as log_batch envelopes at FakeHub.
//
// We reuse the deploy flow because the agent's log forwarder is
// active for the duration of the deploy. After the VM reports deploy
// complete, we check FakeHub's recorded log_batch envelopes for one
// carrying the expected machine ID. The per-machine
// LogLines shape was replaced by a generic LogBatch carrying envelope slices,
// so the assertion now scans envelope.MachineID rather than a
// per-message MachineID field.
func TestLogRelay(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID := "aaaabbbb-cccc-dddd-eeee-ffff00001111"
	mac := "52:54:00:de:ad:a0"
	machineID := uuid.New()

	snap := env.NewSnapshot("pop3-test", network).
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
		// Seed a pending deploy op so the site worker has an
		// OperationID to associate the deploy logs with.
		AddOperation(model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			Type:      model.OpDeploy,
			Status:    model.OpPending,
			CreatedAt: time.Now(),
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
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	vm.Start(ctx)
	vm.WaitForSerial(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete)

	// Poll the hub for a log_batch envelope referencing this
	// machine. wsconn drains the ring on Ring.Wait + new envelope
	// signal; a short busy-poll avoids racing the last batch without
	// the flakiness of a fixed sleep.
	deadline := time.Now().Add(10 * time.Second)
	var found bool
	var totalBatches, totalEnvelopes int
	for time.Now().Before(deadline) {
		batches := env.Hub.LogBatches("pop3-test")
		totalBatches = len(batches)
		totalEnvelopes = 0
		for _, batch := range batches {
			totalEnvelopes += len(batch.Envelopes)
			for _, env := range batch.Envelopes {
				if env.MachineID == machineID {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		t.Errorf("no log envelope for machine %s after 10s (got %d batches / %d envelopes total)",
			machineID, totalBatches, totalEnvelopes)
	}
}
