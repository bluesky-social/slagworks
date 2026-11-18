//go:build integration

package qemutest_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestSiteWorkerRestartMidCommission verifies the site worker can
// be restarted mid-commission and the in-flight VM still completes.
// Exercises the reconnect + reload path. Originally motivated by a
// TFTP-filter regression where site-worker restart mid-boot dropped
// valid in-flight machines.
//
// The restart fires AFTER the squashfs download completes (the
// "STEP squashfs-downloaded" serial marker) and before the agent's
// commission report lands. Restarting during the active HTTP fetch
// is deliberately out of scope: tearing a live squashfs download
// panics the initrd on a half-written rootfs, which is expected —
// there is no HTTP resume in the boot path, and the regression this
// test guards (worker state loss across restart) doesn't need one.
// This placement still forces the worker to come back, reload its
// snapshot from the local cache, and accept the in-flight machine's
// commission report on the fresh process.
//
// Sequence:
//  1. Start site worker + VM.
//  2. Wait for the squashfs download to complete.
//  3. Restart the site worker.
//  4. VM's agent reports commission to the restarted worker and
//     reaches "commission complete".
func TestSiteWorkerRestartMidCommission(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	mac := "52:54:00:de:ad:d0"
	machineID := uuid.New()

	// Fail-closed (Tier 2): a transient commissioning state needs a
	// matching claimed commission op in opState or the worker serves
	// BootLocal. Mirror ClaimOperation's invariant. After the mid-flight
	// restart the worker rehydrates this op from LocalDB UNTRUSTED, then
	// its reconnect op_resync (WaitForSync gates on it) re-delivers the
	// active set and clears the untrusted flag — so post-restart boot
	// decisions still see the claimed commission op.
	snap := env.NewSnapshot("pop3-test", network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateCommissioning,
			PXEEligible: true,
		}).
		AddOperation(model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			Type:      model.OpCommission,
			Status:    model.OpClaimed,
			Version:   1,
		}).
		Build()
	env.Hub.Push("pop3-test", snap)

	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         "pop3-test",
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware: qemutest.BIOS,
		MAC:      mac,
		Network:  network,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx)

	// Wait until the squashfs download has fully completed — the
	// last big HTTP transfer of the boot. Restarting after this
	// marker exercises worker state reload with an in-flight
	// machine whose agent is about to report, without tearing an
	// active download (which the boot path can't resume — see the
	// scope note above).
	vm.WaitForSerial(ctx, "STEP squashfs-downloaded", qemutest.Budgets.CommissionComplete)

	// Restart the worker mid-flight.
	log := slog.Default()
	t.Log("restarting site worker mid-boot")
	sw.Restart(log)

	// Commission should still complete — either iPXE had already
	// downloaded what it needs, or it retries and the
	// freshly-started site worker serves it.
	vm.WaitForSerial(ctx, `"msg":"commission complete"`, qemutest.Budgets.CommissionComplete)
}
