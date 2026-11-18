//go:build integration

package qemutest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestCommission_MultiNIC verifies a commission run with multiple
// NICs attached. The agent's inventory collector enumerates every
// interface; the commission_report body should include both MACs.
//
// Primary NIC is the one that receives DHCP + PXE-boots; extras
// come up via kernel driver + dhcpcd during the agent's
// pre-commission network stage.
func TestCommission_MultiNIC(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	primaryMac := "52:54:00:de:ad:70"
	extraMac := "52:54:00:de:ad:71"
	machineID := uuid.New()

	machine := qemutest.Machine{
		ID:          machineID,
		MAC:         primaryMac,
		State:       model.MachineStateCommissioning,
		PXEEligible: true,
		ExtraNICs: []model.SnapshotInterface{
			{Name: "eth1", MAC: extraMac, Type: "physical"},
		},
	}
	// Fail-closed (Tier 2): a transient commissioning state needs a
	// matching claimed commission op in opState or the worker serves
	// BootLocal. Mirror ClaimOperation's invariant.
	snap := env.NewSnapshot("pop3-test", network).
		AddMachine(machine).
		AddOperation(model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			Type:      model.OpCommission,
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
		Firmware: qemutest.BIOS,
		MAC:      primaryMac,
		Network:  network,
		ExtraNICs: []qemutest.ExtraNIC{
			{MAC: extraMac},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	vm.Start(ctx)
	vm.WaitForSerial(ctx, `"msg":"commission complete"`, qemutest.Budgets.CommissionComplete)

	obs, err := env.Hub.WaitForObservation(ctx, "pop3-test", "commission_report", machineID)
	if err != nil {
		t.Fatalf("wait for commission_report: %v", err)
	}

	// Commission data is an opaque JSON blob. Assert both MACs
	// appear in it — indicates the NIC enumeration saw them both.
	// Caveat: substring-match would also fire on a MAC mentioned in
	// description/comment fields. A structural assertion (decode
	// and walk nics[].mac) is the right shape.
	body := string(obs.Obs.Data)
	for _, want := range []string{primaryMac, extraMac} {
		if !strings.Contains(body, want) {
			t.Errorf("commission_report missing MAC %s", want)
		}
	}
}
