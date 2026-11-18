//go:build integration

package qemutest_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeploy_MS01RouterStack is the full-stack QEMU reproduction of the
// router1.pop3 hardware stack (Minisforum MS-01): UEFI firmware, two NVMe
// disks, the REAL shipped storage script (150-minisforum-ms-01, NVMe boot
// RAID1) and the REAL shipped network script (150-pop3-router, static OOB
// + /31 legs + loopback, WAN excluded), driven through deploy -> reboot ->
// phone-home.
//
// Motivation (2026-07-14): the first router1.pop3 reimage reached
// awaiting_phone_home and the box never phoned home — and with no BMC and
// broken firmware PXE (I226-V vs snponly.efi SNP wedge), every diagnostic
// round-trip was a blind PDU cycle on production transit hardware. This is
// the test that should have existed BEFORE that maintenance: it proves the
// whole software side of the stack (storage layout -> bootloader -> deployed
// netplan -> first-boot phone-home) in silico, so a hardware session starts
// from "the software is right; suspect firmware/cabling".
//
// What this asserts that the kworker network-script test cannot: full
// post-reboot phone-home over the script's own netplan. The kworker script
// renders only fabric legs (QEMU has no BGP), so that test stops at
// bundle-applied. The router script renders a static-addressed OOB leg —
// the deployed VM has a live path back to the worker, so phone_home is the
// end-to-end assertion here.
//
// Fidelity to the real box, and the deliberate divergences:
//   - Disks: two NVMe (real: Samsung 990 PRO 2TB; here 6G qcow2) plus the
//     harness's mandatory primary virtio disk. The real storage script must
//     select exactly the NVMe pair and ignore the virtio disk — that
//     selection IS part of what's under test.
//   - NIC facts: real router1 names/MACs/addressing for the legs, loopback,
//     and WAN (the WAN entries prove the exclusion logic). The OOB entry
//     carries the VM's primary MAC and an address inside the test /24 so
//     the rendered static config is actually routable to the worker.
//   - The script's transitional default route points at the real pop3 OOB
//     VIP (10.252.240.1), which is off-link in the test subnet. The
//     route carries on-link: true (see the script), so networkd installs it
//     bound to the interface instead of wedging the link in `configuring` —
//     an unresolvable static-route gateway is a networkd configuration
//     FAILURE that blocks wait-online -> network-online -> phone-home. That
//     wedge (not a flake) is why this test failed on every run from its
//     introduction until 2026-07-22. Phone-home itself rides the on-link
//     /24 to the worker.
func TestDeploy_MS01RouterStack(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	storageScript := readAssetScript(t, "storage", "150-minisforum-ms-01")
	networkScript := readAssetScript(t, "network", "150-pop3-router")

	mac := "52:54:00:7f:7e:e2" // stable, MS-01-suffixed
	machineID := uuid.New()
	systemUUID := "cde85900-3a4b-11f0-b982-1c125f769d00" // real router1 SMBIOS UUID

	// The OOB static address the netscript will render: .10 in the test
	// /24 — below the ephemeral pool (.129+), never the gateway (.1).
	oobIP := fmt.Sprintf("%s/24", nthIP(network.Subnet, 10))

	// router1.pop3's post-commission facts shape, real names/MACs. The OOB
	// entry maps onto the VM's real NIC (primary MAC + test-subnet IP);
	// everything else is exactly the production model, including the WAN
	// /30s whose exclusion the netscript must get right.
	extraNICs := []model.SnapshotInterface{
		{Name: "lo", Type: "physical", IP: "10.252.31.1/32"},
		{Name: "enp88s0", MAC: mac, Type: "physical", IP: oobIP},
		{Name: "enp90s0", MAC: "58:47:ca:7f:7e:e3", Type: "physical"},
		{Name: "enp3s0f0np0", MAC: "58:47:ca:7f:7e:e0", Type: "physical", IP: "10.252.25.9/31"},
		{Name: "enp3s0f1np1", MAC: "58:47:ca:7f:7e:e1", Type: "physical", IP: "10.252.25.13/31"},
		{Name: "enp1s0f0", MAC: "98:b7:85:1f:35:0e", Type: "physical", IP: "155.204.43.106/30"},
		{Name: "enp1s0f1", MAC: "98:b7:85:1f:35:0f", Type: "physical", IP: "155.204.43.110/30"},
	}

	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)

	machine := qemutest.Machine{
		ID:          machineID,
		MAC:         mac,
		State:       model.MachineStateDeploying,
		PXEEligible: true,
		SystemUUID:  systemUUID,
		ExtraNICs:   extraNICs,
		StorageScript: &model.SnapshotStorageScript{
			Name:    "150-minisforum-ms-01",
			Content: storageScript,
		},
		NetworkScript: &model.SnapshotNetworkScript{
			Name:    "150-pop3-router",
			Content: networkScript,
		},
	}

	slug := "pop3-ms01"
	snapInit := env.NewSnapshot(slug, network).
		AddMachine(machine).
		AddOperation(deployOp).
		Build()
	env.Hub.Push(slug, snapInit)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.UEFI, // the MS-01 is UEFI-only
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		// Primary virtio disk (harness-mandatory) stays OUT of the layout;
		// the two NVMe disks mirror the real box's 990 PRO pair.
		DiskSize: "4G",
		ExtraDisks: []qemutest.ExtraDisk{
			{Type: qemutest.DiskNVMe, Size: "6G", Serial: "S7KHNJ0Y902003X"},
			{Type: qemutest.DiskNVMe, Size: "6G", Serial: "S7KHNJ0Y902006H"},
		},
		// The two /31 leaf-downlink NICs must EXIST in the VM, not just in
		// the snapshot facts: the rendered netplan MAC-matches them as
		// managed non-optional interfaces, so netplan's generated
		// systemd-networkd-wait-online drop-in waits on them. Without the
		// hardware, wait-online blocks for its full internal 120s timeout,
		// network-online.target (which gates the phone-home unit) arrives
		// after the test's 2-minute PhoneHomeObs budget, and the test fails
		// on every run — this is why the test never passed between 2026-07-14
		// (introduction) and 2026-07-22. The real router1 has these NICs
		// cabled with carrier, so wait-online completes immediately there;
		// giving the VM the same NICs restores that fidelity. (The WAN and
		// spare NICs stay VM-absent: the netplan deliberately doesn't render
		// them, so networkd doesn't manage or wait on them.)
		ExtraNICs: []qemutest.ExtraNIC{
			{MAC: "58:47:ca:7f:7e:e0"},
			{MAC: "58:47:ca:7f:7e:e1"},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	vm.Start(ctx)

	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for deploy complete: %v", err)
	}

	// The network bundle must have been fetched, run, and applied — the
	// DHCP fallback would ALSO phone home here (the OOB leg is on the DHCP
	// subnet), so phone_home alone can't distinguish the real netplan from
	// the fallback. These markers can.
	for _, want := range []string{
		`"msg":"network config applied"`,
		`"path":"/etc/netplan/99-slagworks.yaml"`,
	} {
		if !vm.Serial.Contains(want) {
			t.Errorf("expected serial to show %q (real bundle applied), but it did not", want)
		}
	}
	if vm.Serial.Contains("no network script matched") {
		t.Error("agent took the DHCP-fallback path despite the router NetworkScript on the snapshot")
	}

	// Flip to awaiting_phone_home immediately (see matrix: the reboot's
	// PXE re-fetch races this push).
	deployOpAwaiting := deployOp
	deployOpAwaiting.Status = model.OpClaimed
	deployOpAwaiting.Phase = "awaiting_phone_home"
	snapAwaiting := env.NewSnapshot(slug, network).
		Seq(2).
		AddMachine(machine).
		AddOperation(deployOpAwaiting).
		Build()
	env.Hub.Push(slug, snapAwaiting)

	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}

	// The money assertion: the deployed system booted off the NVMe RAID1
	// via the agent-registered EFI entry, brought up the REAL rendered
	// netplan (static OOB address, renamed NIC), and POSTed phone_home.
	// This is exactly the sequence that went dark on the physical box.
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("waiting for phone_home (post-install reboot on the MS-01 stack failed): %v\n"+
			"debug order: (1) did the VM boot from NVMe at all (serial shows firmware "+
			"re-PXE -> BootLocal)? (2) did networkd apply the static OOB address "+
			"(rename by MAC to enp88s0)? (3) did the off-link transitional default "+
			"route abort interface config (it must not)?",
			err)
	}
}

// readAssetScript loads a shipped asset script's content from the repo
// tree (two dirs up from internal/qemutest).
func readAssetScript(t *testing.T, kind, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", kind, name)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read shipped asset %s/%s: %v", kind, name, err)
	}
	return string(body)
}

// nthIP returns the subnet's base address + n (e.g. 198.18.5.0/24, 10 ->
// 198.18.5.10).
func nthIP(subnet *net.IPNet, n byte) string {
	ip := make(net.IP, 4)
	copy(ip, subnet.IP.To4())
	ip[3] += n
	return ip.String()
}
