//go:build integration

package qemutest_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestSyncStress pushes many snapshots in quick succession at the
// site worker and verifies the final state reflects the last push.
// No VM involved — this exercises the hub.Push → wsconn → Cache
// path under load.
func TestSyncStress(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	// Seed initial snapshot with one known machine so the site
	// worker's initial sync completes.
	seed := env.NewSnapshot("pop3-test", network).
		AddMachine(qemutest.Machine{
			ID:          uuid.New(),
			MAC:         "52:54:00:de:ad:b0",
			State:       model.MachineStateCommissioned,
			PXEEligible: true,
		}).
		Build()
	env.Hub.Push("pop3-test", seed)

	// DefaultImage deliberately omitted: this test never boots a VM
	// through the image, so the "noble" fallback is never dereferenced.
	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:    "pop3-test",
		Network: network,
		Images:  images,
	})

	// Push 100 snapshots, each incrementing SeqNum and swapping
	// the single machine's state. The final snapshot moves the
	// machine to MachineStateRunning.
	machineID := uuid.New()
	mac := "52:54:00:de:ad:b1"
	for i := int64(2); i <= 101; i++ {
		state := model.MachineStateCommissioned
		if i == 101 {
			state = model.MachineStateRunning
		}
		snap := env.NewSnapshot("pop3-test", network).
			Seq(i).
			AddMachine(qemutest.Machine{
				ID:          machineID,
				MAC:         mac,
				State:       state,
				PXEEligible: true,
			}).
			Build()
		env.Hub.Push("pop3-test", snap)
	}

	// The worker applies snapshots asynchronously over the WS
	// connection. Give it a generous window for 100 applies.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sw.Cache.LastSeqNum() == 101 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if got := sw.Cache.LastSeqNum(); got != 101 {
		t.Errorf("cache LastSeqNum: got %d, want 101", got)
	}

	// Final machine state must reflect the last push.
	final := sw.Cache.MachineByID(machineID)
	if final == nil {
		t.Fatalf("machine %s missing from cache after stress", machineID)
	}
	if final.State != model.MachineStateRunning {
		t.Errorf("final machine state: got %s, want running", final.State)
	}
}
