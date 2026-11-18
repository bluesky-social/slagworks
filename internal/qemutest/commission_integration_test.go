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

// TestCommission verifies the basic full-stack commission path:
//
//  1. Site worker dials FakeHub, gets a snapshot containing one
//     machine in Commissioning state with a known MAC.
//  2. A QEMU VM on the same bridge PXE-boots, leases a DHCP address,
//     chainloads into the commission initrd, runs the agent.
//  3. Agent collects inventory and POSTs an operation-bound commission_report
//     to /api/v1/report on the site
//     worker.
//  4. Site worker queues a commission_report observation on its
//     outbox; wsconn drains it to FakeHub.
//  5. Test asserts FakeHub received the observation with the
//     expected machine ID.
//
// No executor, no BMC simulation: the VM boots directly from
// network. Op-dispatch tests (which require power cycling) live in
// the lifecycle phase.
func TestCommission(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	mac := "52:54:00:de:ad:01"
	machine := qemutest.Machine{
		ID:          uuid.New(),
		MAC:         mac,
		State:       model.MachineStateCommissioning,
		PXEEligible: true,
	}

	// A transient commissioning state must be backed by a matching
	// active commission op in the worker's opState (Tier 2 fail-closed
	// rule): the op layer is the sole authority for lifecycle actuation,
	// so a bare commissioning baseline with no op serves BootLocal.
	// Mirror what ClaimOperation produces — a claimed OpCommission
	// created alongside the transient state.
	commissionOp := model.Operation{
		ID:        uuid.New(),
		MachineID: machine.ID,
		Type:      model.OpCommission,
		Status:    model.OpClaimed,
		Version:   1,
	}

	snap := env.NewSnapshot("pop3-test", network).
		AddMachine(machine).
		AddOperation(commissionOp).
		Build()
	env.Hub.Push("pop3-test", snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         "pop3-test",
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	// BIOS firmware: OVMF's UEFI BDS defaults to HTTPv4 boot which
	// interprets the DHCP bootfile as an HTTP URL and fails against
	// our TFTP-only flow. SeaBIOS (default when Firmware=BIOS) uses
	// the iPXE option ROM directly, which matches real-hardware
	// behavior on our fleet. The UEFI-specific boot path is
	// exercised by TestDeployFullLifecycle_UEFI and
	// TestDeployFullLifecycle_UEFI_RAID1.
	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware: qemutest.BIOS,
		MAC:      mac,
		Network:  network,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx)

	vm.WaitForSerial(ctx, `"msg":"commission complete"`, qemutest.Budgets.CommissionComplete)

	obs, err := env.Hub.WaitForObservation(ctx, "pop3-test", "commission_report", machine.ID)
	if err != nil {
		t.Fatalf("wait for commission_report observation: %v", err)
	}
	if obs.Obs.MachineID != machine.ID {
		t.Errorf("observation machine_id: got %s, want %s", obs.Obs.MachineID, machine.ID)
	}
	if obs.Obs.OperationID != commissionOp.ID {
		t.Errorf("observation op_id: got %s, want %s", obs.Obs.OperationID, commissionOp.ID)
	}
	if len(obs.Obs.Data) == 0 {
		t.Error("observation data is empty")
	}
}
