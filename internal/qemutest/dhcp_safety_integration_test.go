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

// TestDHCPInactiveMachineServesBootLocal used to live here; deleted
// 2026-07-04 (fable-review Tier 5 pyramid rebalance) — every layer it
// chained is unit-covered: Cache.BootActionForMAC returns BootLocal
// for running machines (worker/cache_test), the DHCP handler leases to
// known MACs (dhcp/handler_test DORA), and the boot-local script bytes
// are golden-pinned (httpboot TestBootScriptGolden).

// TestDHCPUnknownMACNoPXE verifies that non-promiscuous DHCP does
// not lease to MACs the snapshot doesn't know. The VM's DHCP
// DISCOVER is silently dropped by the site worker; the firmware
// retries a couple of times, gives up, falls through to local
// disk. The key assertion: no boot script is ever served for the
// unknown MAC.
func TestDHCPUnknownMACNoPXE(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	// Snapshot contains one machine with a DIFFERENT MAC from the VM
	// we'll start.
	knownMac := "52:54:00:de:ad:10"
	snap := env.NewSnapshot("pop3-test", network).
		AddMachine(qemutest.Machine{
			ID:          uuid.New(),
			MAC:         knownMac,
			State:       model.MachineStateCommissioned,
			PXEEligible: true,
		}).
		Build()
	env.Hub.Push("pop3-test", snap)

	// DefaultImage deliberately omitted: this test never serves a
	// boot script (that's the point), so the "noble" fallback is
	// never dereferenced.
	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:    "pop3-test",
		Network: network,
		Images:  images,
	})

	// VM uses a MAC no snapshot machine advertises.
	unknownMac := "52:54:00:de:ad:11"
	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware: qemutest.BIOS,
		MAC:      unknownMac,
		Network:  network,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	vm.Start(ctx)

	// SeaBIOS → iPXE ROM → iPXE DHCP times out (no offer for unknown
	// MAC) → iPXE gives up → SeaBIOS falls through to disk/CD/floppy
	// → "No bootable device".
	vm.WaitForSerial(ctx, "No bootable device", qemutest.Budgets.DHCPNoBootableDevice)

	// iPXE logs its attempts on the VM serial console. If DHCP
	// never succeeded, we expect to see it try and fail. Look for
	// iPXE's "No configuration methods succeeded" or similar. And
	// the site worker must not have emitted any boot script — any
	// HTTP boot script fetch would have produced a serial line
	// like "http://<ip>:8080/boot/ipxe...".
	serial := strings.Join(vm.Serial.Lines(), "\n")
	if strings.Contains(serial, "/boot/ipxe") {
		t.Error("unknown MAC appears to have fetched a boot script (saw /boot/ipxe in serial)")
	}

	// FakeHub should not have seen any observation for this MAC.
	obs := env.Hub.Observations("pop3-test")
	for _, o := range obs {
		t.Errorf("unexpected observation: event=%s machine=%s", o.Obs.Event, o.Obs.MachineID)
	}
}
