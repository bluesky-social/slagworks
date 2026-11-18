//go:build integration

package qemutest_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestConsoleProbe_HappyPath asserts the console probe contract end-to-end:
// the snapshot's MachineSnapshot.KernelParams flows through the site
// worker into the agent at commission time, the agent emits the
// SLAG-CONSOLE-PROBE marker on the configured tty, and the bytes
// traverse the BMC SOL session via qemutest-serialrelay → tap → the
// SerialMonitor in the test process.
//
// This is the bug class we're guarding against: a kworker boots into
// commission, the agent thinks it's writing to ttyS1, but the BMC's
// SOL relay is wired elsewhere — so post-deploy the operator can't
// see kernel output. The probe makes the failure mode loud at
// commission time instead of silent at deploy time.
//
// Skips on hosts missing ipmi_sim or ipmitool; the suite-level CI
// runner has both.
func TestConsoleProbe_HappyPath(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ipmi_sim"); err != nil {
		t.Skip("ipmi_sim not installed")
	}
	if _, err := exec.LookPath("ipmitool"); err != nil {
		t.Skip("ipmitool not installed")
	}

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	mac := "52:54:00:cc:0d:01"
	machineID := uuid.New()

	// SOL routed: BMCSim spawns the serial relay subprocess in the
	// netns. The VM's ttyS1 connects to the relay's vm-listen, the
	// relay tees onto the unix tap that SerialMonitor reads.
	bmc := env.NewBMCSim(t, qemutest.BMCSimConfig{
		ExposeLAN: true,
		SOLEnable: true,
		Network:   network,
		SeedUsers: []qemutest.BMCSimUser{
			{Slot: 2, Name: "ADMIN", Password: "test", Privilege: "admin", Enabled: true},
		},
	})
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()
	bmc.Start(bootCtx)

	machine := qemutest.Machine{
		ID:           machineID,
		MAC:          mac,
		State:        model.MachineStateCommissioning,
		PXEEligible:  true,
		KernelParams: "console=ttyS1,115200n8",
	}
	// Fail-closed (Tier 2): a transient commissioning state needs a
	// matching claimed commission op in opState or the worker serves
	// BootLocal. Mirror ClaimOperation's invariant.
	snap := env.NewSnapshot("probe-test", network).
		AddMachine(machine).
		AddOperation(model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			Type:      model.OpCommission,
			Status:    model.OpClaimed,
			Version:   1,
		}).
		Build()
	env.Hub.Push("probe-test", snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         "probe-test",
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware: qemutest.UEFI,
		MAC:      mac,
		Network:  network,
		BMC:      bmc,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm.Start(ctx)

	// Wait for commission to reach probe emission. The probe runs
	// after reportCommission, so we can't gate on
	// "commission complete" — the probe is between data report and
	// the success log line. WaitFor with a generous budget watches
	// the SerialMonitor (which is reading the relay tap, i.e. the
	// actual BMC SOL stream) for the marker prefix.
	probeMarker := "SLAG-CONSOLE-PROBE-" + machineID.String()
	if err := vm.Serial.WaitFor(ctx, probeMarker, qemutest.Budgets.CommissionComplete); err != nil {
		t.Logf("--- captured serial lines ---")
		for _, line := range vm.Serial.Lines() {
			t.Logf("  %s", line)
		}
		t.Fatalf("probe marker %q never observed via BMC SOL relay: %v", probeMarker, err)
	}

	// The SOL-vs-stdio provenance guarantee is structural, not
	// re-scannable: with SOLEnable on, NewVM fatals unless the BMC
	// relay port is live, and buildArgs wires ttyS1 to that relay —
	// there is no stdio serial for the marker to arrive on. (An
	// earlier version re-scanned Serial.Lines() here, but that scans
	// the same merged stream WaitFor already matched — hits==0 was
	// unreachable, a tautology masquerading as a check.)
}

// The "none" sentinel branch is exercised by the unit-level
// parseConsoleParams_Test and the agent-side path that branches
// on kp == "none" before opening a tty. We can't observe it
// usefully from QEMU because dropping console= entirely means
// the kernel writes nothing to ttyS1, so there's no way to assert
// "the agent decided not to write" via the SerialMonitor — the
// guest's stdout is empty either way. The branch is small enough
// that unit coverage is enough.
