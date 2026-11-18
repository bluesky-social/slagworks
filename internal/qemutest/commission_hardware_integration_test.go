//go:build integration

package qemutest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestCommission_HardwareVariants runs commission against a matrix
// of disk types and memory sizes. Each subtest pins one hardware
// shape the production fleet actually ships.
//
// The agent's hardware inventory collector must handle virtio,
// NVMe, and SATA attach paths identically; this test catches
// regressions where one disk type doesn't appear in the
// commission_report.
func TestCommission_HardwareVariants(t *testing.T) {
	variants := []struct {
		name   string
		disk   qemutest.DiskType
		memory string
	}{
		{"virtio_2g", qemutest.DiskVirtio, "2048"},
		{"nvme_2g", qemutest.DiskNVMe, "2048"},
		{"sata_2g", qemutest.DiskSATA, "2048"},
		{"virtio_4g", qemutest.DiskVirtio, "4096"},
	}

	for i, v := range variants {
		v := v
		i := i
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			env := qemutest.New(t)
			network := env.NewNetwork(t)
			images := env.UseCanonicalImages(t)

			// Each subtest's VMs get a unique MAC so they don't
			// collide if multiple subtests happen to draw into the
			// same shared bridge (they don't — per-subtest Env).
			// 0xa0+i with %02x stays valid as a 2-hex-char MAC
			// octet through i=95 (`a0`..`ff`). The older
			// rune-arithmetic shape silently produced ":", ";",
			// etc. for the 11th-and-onward variant; the previous
			// `a%x` fix worked through i=15 but produced 3-char
			// segments past that — load-bearing comment + arithmetic
			// agree now.
			mac := fmt.Sprintf("52:54:00:de:ad:%02x", 0xa0+i)
			machineID := uuid.New()

			// Fail-closed (Tier 2): the transient commissioning
			// state needs a matching claimed commission op in
			// opState or the worker serves BootLocal. Mirror
			// ClaimOperation's invariant.
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
				Memory:   v.memory,
				ExtraDisks: []qemutest.ExtraDisk{
					{Type: v.disk, Size: "4G"},
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
			if len(obs.Obs.Data) == 0 {
				t.Error("commission_report has empty data")
			}
		})
	}
}
