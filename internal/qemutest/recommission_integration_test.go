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

// TestReCommissionFromReady verifies that a machine in Ready state
// can be re-commissioned by pushing a new snapshot with
// State=Commissioning. The VM reboots and runs the commission flow
// a second time; the second commission_report arrives at FakeHub.
func TestReCommissionFromReady(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	mac := "52:54:00:de:ad:30"
	machineID := uuid.New()

	// Fail-closed (Tier 2): a transient commissioning state needs a
	// matching claimed commission op in opState or the worker serves
	// BootLocal. Mirror ClaimOperation's invariant. The re-commission
	// below (snap2) claims a fresh op, exactly as the operator-triggered
	// path does in production.
	snap1 := env.NewSnapshot("pop3-test", network).
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
	env.Hub.Push("pop3-test", snap1)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
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

	// Wait for first commission_report via the cursor API so we
	// can wait for a NEW one after the reset.
	_, cursor, err := env.Hub.WaitForObservationAfter(
		ctx, "pop3-test", "commission_report", machineID, 0,
	)
	if err != nil {
		t.Fatalf("first commission_report: %v", err)
	}

	// Push a second snapshot moving the machine back to
	// Commissioning (as if the operator triggered re-commission).
	// Re-commission claims a fresh commission op (new op ID), the same
	// way the operator-triggered path does in production — the prior op
	// left the active set on completion.
	snap2 := env.NewSnapshot("pop3-test", network).
		Seq(2).
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
	env.Hub.Push("pop3-test", snap2)

	// Reset VM. It will re-PXE-boot.
	if err := vm.QMP.Reset(); err != nil {
		t.Fatalf("qmp reset: %v", err)
	}

	// Wait for the SECOND commission_report, strictly after the
	// first one's cursor.
	if _, _, err := env.Hub.WaitForObservationAfter(
		ctx, "pop3-test", "commission_report", machineID, cursor,
	); err != nil {
		t.Fatalf("second commission_report: %v", err)
	}
}
