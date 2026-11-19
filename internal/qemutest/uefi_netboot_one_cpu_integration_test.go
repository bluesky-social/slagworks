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

// TestUEFINetbootOnOneHostCPU network-boots a UEFI VM whose QEMU process is
// confined to a single host CPU and waits for the kernel to start: firmware
// PXE, the iPXE chainload, then the kernel and the 65 MB initrd over HTTP.
//
// On one CPU the QEMU I/O thread and the vCPU can never run at the same
// time. With QEMU's default iPXE option ROM driving the NIC, that corrupted
// the ROM driver's heap during the initrd download every time: the firmware
// stopped in virtio-net.efidrv (an "X64 Exception" dump on the serial
// console, or a silent spin) and the fetch never finished. A hosted CI
// runner produced the same starvation by chance, which showed up as a
// different UEFI test each time timing out with the serial console ending
// at ".../initrd...". The harness now boots UEFI VMs without that ROM (see
// buildArgs); this test fails if a change puts such a driver back under the
// boot path.
func TestUEFINetbootOnOneHostCPU(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	const slug = "uefi-one-cpu"
	machineID := uuid.New()
	systemUUID := uuid.New().String()
	mac := "52:54:00:1c:b0:01"

	// A machine with a deploy in flight gets the deploy boot script, whose
	// kernel and initrd fetches are the load that matters here. The test
	// stops once the kernel is running, before the agent does anything.
	snap := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateDeploying,
			PXEEligible: true,
			SystemUUID:  systemUUID,
		}).
		AddOperation(qemutest.NewOperation(machineID, model.OpDeploy)).
		Build()
	env.Hub.Push(slug, snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:      qemutest.UEFI,
		MAC:           mac,
		Network:       network,
		Memory:        "4096",
		SMBIOSUUID:    systemUUID,
		SingleHostCPU: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	vm.Start(ctx)

	if err := vm.Serial.WaitFor(ctx, "Linux version", qemutest.Budgets.KernelStartOneCPU); err != nil {
		if vm.Serial.Contains("X64 Exception") {
			t.Fatalf("the UEFI firmware crashed before the kernel started (exception dump on the serial console above; "+
				"it names the image that faulted): %v", err)
		}
		t.Fatalf("the netbooted kernel never started; the serial console above ends where the boot stopped: %v", err)
	}
}
