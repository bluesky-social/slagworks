//go:build integration

package qemutest_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestBMCSim_Standalone validates the BMCSim harness's QEMU wiring
// in isolation: the simulator starts and binds, BMCSocket() returns a
// loopback host:port, and the QEMUArgs() output references the same
// port plus the ipmi-bmc-extern + isa-ipmi-kcs devices the in-band
// reconcile path needs.
//
// What this does NOT do: drive ipmitool against the simulator. The
// current BMCSim config doesn't expose a `lan` channel (production
// reconcile uses in-band IPMI from inside the VM, where QEMU bridges
// through to the simulator), so a host-side ipmitool would have
// nothing to dial.
//
// Doesn't require KVM or a VM — just the host's ipmi_sim. Skips on
// hosts without ipmi_sim installed.
func TestBMCSim_Standalone(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ipmi_sim"); err != nil {
		t.Skip("ipmi_sim not installed; skipping BMC harness sanity test")
	}
	// ipmitool is intentionally NOT a prereq here: the test body
	// asserts QEMU wiring, not a live ipmitool round trip. Skipping
	// on a missing ipmitool would be a B1-shape silent-skip on an
	// unrelated prereq. There is no workflow-level
	// presence check for ipmi_sim itself yet.

	env := qemutest.New(t)
	bmc := env.NewBMCSim(t, qemutest.BMCSimConfig{
		// Leaving ChassisSerial empty/default skips the FRU file
		// path. Phase 2.1 tests don't exercise the chunk-4 chassis
		// serial check; that's a separate harness concern (see
		// bmcsim.go renderCmds for why).
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bmc.Start(ctx)

	// The simulator's "serial 15 ... codec VM" listener is for QEMU,
	// not for direct ipmitool use. To talk to the simulator from the
	// host we'd need a `lan` channel configured. The current config
	// doesn't expose LAN by design (production agent reconcile uses
	// in-band IPMI from inside a VM, where the QEMU chardev bridges
	// through to the simulator).
	//
	// So the standalone test confirms two things only:
	//   1. ipmi_sim starts and binds to its TCP port without error.
	//   2. The QEMU args produced by BMCSim.QEMUArgs() reference the
	//      port BMCSocket() returns.
	addr := bmc.BMCSocket()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("BMCSocket: unexpected address %q", addr)
	}
	_, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("BMCSocket %q has no host:port", addr)
	}

	args := bmc.QEMUArgs()
	if len(args) == 0 {
		t.Fatal("QEMUArgs returned no args")
	}
	joined := strings.Join(args, " ")
	// QEMU's chardev socket syntax separates host and port with
	// `host=...,port=...` rather than the host:port form BMCSocket
	// returns; assert the port matches and the device hookup looks
	// right.
	if !strings.Contains(joined, "port="+portStr) {
		t.Errorf("QEMUArgs (%v) does not reference port %s from BMCSocket %q", args, portStr, addr)
	}
	if !strings.Contains(joined, "ipmi-bmc-extern") {
		t.Errorf("QEMUArgs missing ipmi-bmc-extern device: %v", args)
	}
	if !strings.Contains(joined, "isa-ipmi-kcs") {
		t.Errorf("QEMUArgs missing isa-ipmi-kcs device: %v", args)
	}
}
