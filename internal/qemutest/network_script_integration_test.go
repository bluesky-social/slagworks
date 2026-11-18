//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// l3KworkerNetworkScript is a trimmed pop1-L3 network-config script: it
// reads SLAGWORKS_NETWORK_FACTS, decodes the loopback /32 + the two /31 legs,
// and emits the emit-bundle (netplan + frr.conf + daemons + sysctl, enable
// frr.service). It deliberately mirrors assets/network/100-pop1-l3-kworker's
// SHAPE (the parts the agent's applyNetworkConfig consumes) without the full
// FRR config body — the point of this test is the end-to-end SEAM (snapshot ->
// site /network-script + /network-facts -> agent fetch -> run -> apply bundle
// to /target), which is exactly the path that shipped broken to prod
// untested: a matching machine silently got the DHCP fallback because the
// bundle never reached the target. We assert the agent applied a REAL bundle,
// not writeDefaultNetplan.
const l3KworkerNetworkScript = `#!/usr/bin/env python3
# slagworks:match site=pop1
# slagworks:match hostname=/kworker-l(34|56)-\d+\.pop1/
# slagworks:priority 100
import ipaddress, json, os, sys

with open(os.environ["SLAGWORKS_NETWORK_FACTS"]) as f:
    ifaces = json.load(f)

legs, loopback = [], ""
for i in ifaces:
    ip, name = i.get("ip", ""), i.get("name", "")
    if not ip:
        continue
    if name == "lo" and ip.endswith("/32"):
        loopback = ip
        continue
    if i.get("type") == "physical" and ip.endswith("/31"):
        legs.append(ip)

if len(legs) < 2 or not loopback:
    # HARD FAIL, no DHCP fallback — mirrors the real pop1 script. A matched
    # kworker that isn't fully L3-modeled aborts the deploy rather than coming
    # up DHCP-only (which would silently leave it off the L3 fabric). The
    # happy-path test feeds a loopback so this never trips; a separate test
    # exercises this exit path.
    sys.stderr.write("ERROR: not fully L3-modeled (need >=2 legs + loopback)\n")
    sys.exit(1)

lo = loopback.split("/")[0]
bundle = {
    "files": [
        {"path": "/etc/netplan/99-slagworks.yaml",
         "content": "# L3 routed-NIC kworker (qemutest)\nnetwork:\n  version: 2\n", "mode": "0600"},
        {"path": "/etc/frr/frr.conf",
         "content": "router bgp 64513\n bgp router-id %s\n network %s/32\n" % (lo, lo),
         "mode": "0640", "owner": "frr", "group": "frr"},
        {"path": "/etc/frr/daemons", "content": "bgpd=yes\nbfdd=yes\n",
         "mode": "0640", "owner": "frr", "group": "frr"},
        {"path": "/etc/sysctl.d/90-l3-multipath.conf",
         "content": "net.ipv4.fib_multipath_hash_policy = 1\n", "mode": "0644"},
    ],
    "enable_units": ["frr.service"],
}
print(json.dumps(bundle))
`

// TestDeploy_NetworkScriptBundleApplied is the integration regression guard
// for the pluggable network-config seam. It models an L3-fabric kworker (two
// /31 legs + a loopback /32 in its NetBox interface facts) with the pop1 L3
// network script attached to its snapshot, deploys it in QEMU, and asserts the
// agent fetched the script + facts from the real site httpboot endpoints, ran
// the script, and applied the emitted bundle to /target.
//
// This is the test we should have had before shipping P3: every individual
// link (configscript match, snapshot.Build, the pop1 script's output,
// applyNetBundle) had a unit test, but nothing crossed the
// snapshot -> site-endpoint -> agent-fetch -> apply seam end to end — and that
// is exactly where the prod failure lived (a matching machine got
// writeDefaultNetplan because the bundle never reached the agent).
//
// We assert on the finalize-time serial markers, NOT on a post-reboot booted
// L3 host: the emitted netplan renames NICs by MAC to legN, which a
// single-NIC QEMU VM can't satisfy, so the rebooted target couldn't phone home
// over the L3 fabric (QEMU has no BGP/leaves). The seam under test is "did the
// bundle get fetched, run, and written to /target", which all happens at
// finalize before the reboot — and is the bit that was broken.
func TestDeploy_NetworkScriptBundleApplied(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	mac := "52:54:00:de:ad:90"
	machineID := uuid.New()
	// A deploy boot requires a commissioned system-uuid: the handler emits
	// slagworks.expect-uuid= only when the machine has one, and the agent's
	// identity check FATALs without it. Set it on the machine AND as the VM's
	// SMBIOS uuid so the guest's reported uuid matches (same pattern as the
	// other deploy tests, e.g. deploy_bmc_reconcile).
	systemUUID := "12345678-1234-1234-1234-de00de00de70"

	// A running deploy op is what makes the boot handler serve a deploy
	// boot script with slagworks.machine=/op= (so completion is recorded
	// instead of the VM reboot-looping back into the deploy) and the
	// destructive flag (so the disk safety check is skipped — without it
	// the second pass FATALs on the partition table the first pass wrote).
	// Same shape as the other deploy tests (deploy_bmc_reconcile/matrix).
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	deployOp.Status = model.OpRunning
	deployParams, err := json.Marshal(map[string]any{"destructive": true})
	if err != nil {
		t.Fatalf("marshal deploy params: %v", err)
	}
	deployOp.Params = deployParams

	// L3 facts the script decodes. eth0 (the real QEMU NIC) carries the
	// primary MAC; the loopback + two /31 legs are synthetic facts fed via
	// ExtraNICs — the script only READS them (and writes config files), so
	// they need not correspond to real QEMU NICs.
	extraNICs := []model.SnapshotInterface{
		{Name: "lo", Type: "physical", IP: "10.254.31.67/32"},
		{Name: "eth1", MAC: "52:54:00:de:ad:91", Type: "physical", IP: "10.254.29.9/31"},
		{Name: "eth2", MAC: "52:54:00:de:ad:92", Type: "physical", IP: "10.254.29.11/31"},
	}

	snap := env.NewSnapshot("pop1-test", network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateDeploying,
			PXEEligible: true,
			SystemUUID:  systemUUID,
			ExtraNICs:   extraNICs,
			StorageScript: &model.SnapshotStorageScript{
				Name:    "default",
				Content: defaultStorageScript,
			},
			NetworkScript: &model.SnapshotNetworkScript{
				Name:    "pop1-l3-kworker",
				Content: l3KworkerNetworkScript,
			},
		}).
		AddOperation(deployOp).
		Build()
	env.Hub.Push("pop1-test", snap)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         "pop1-test",
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	vm.Start(ctx)

	// "network config applied" is logged at the end of applyNetworkConfig,
	// after the bundle's files are written and units enabled — i.e. only on
	// the success path. The fallback path logs "no network script matched"
	// instead and never reaches "applied". The agent emits slogkit JSON
	// envelopes to stderr (captured on the serial console), so match the
	// envelope's msg substring.
	if err := vm.Serial.WaitFor(ctx, `"msg":"network config applied"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for network config applied: %v", err)
	}

	// The bundle's files + unit must have been applied — these prove the
	// real bundle ran, not the DHCP fallback. netapply logs one envelope
	// per file (msg "wrote bundle file", extra {"mode":..,"path":..}) and
	// one per unit (msg "enabled unit", extra {"unit":..}). Extra keys
	// marshal alphabetically, so match the per-file/per-unit identifying
	// substring (`"path":"<p>"` / `"unit":"<u>"`); nothing else on the
	// success path emits these config-target paths.
	for _, want := range []string{
		`"path":"/etc/frr/frr.conf"`,
		`"path":"/etc/frr/daemons"`,
		`"path":"/etc/netplan/99-slagworks.yaml"`,
		`"unit":"frr.service"`,
	} {
		if !vm.Serial.Contains(want) {
			t.Errorf("expected serial to show %q (bundle applied), but it did not", want)
		}
	}

	// The prod-failure signature: a matching machine silently fell back to
	// the built-in DHCP netplan. That must NOT happen here.
	if vm.Serial.Contains("no network script matched") {
		t.Error("agent took the DHCP-fallback path despite a NetworkScript on the snapshot " +
			"(this is the prod bug: bundle never reached the agent)")
	}
}

// TestDeploy_NetworkScriptHardFailsWhenNotL3Modeled used to live
// here; deleted 2026-07-04 (fable-review Tier 5 pyramid rebalance) —
// the script's hard-fail on missing L3 modeling is unit-covered
// (TestPop1Script_UnmodeledHostHardFails / UnknownSubnetHardFails in
// cmd/slagworks-agent), and the agent's fatal-on-script-failure
// propagation is exercised by the matrix fatal_at tests.
