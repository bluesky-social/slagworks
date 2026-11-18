//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// fixedDataArrayUUID is the md array UUID pinned into BOTH the create and the
// preserve storage scripts. On real hardware the array_uuid is derived from a
// stable per-host value (system UUID); the test hardcodes a constant so the
// create-then-preserve identity match is exact and reproducible. mdadm accepts
// a standard RFC-4122 UUID for --create/--assemble --uuid.
const fixedDataArrayUUID = "e8c9aa81-1c2d-4e3f-8a5b-6c7d8e9f0a1b"

// TestDeployPreserveDataDisk is the full-stack acceptance test for the
// preserve-data feature. It proves, on real PXE-booted VMs, that a
// redeploy which names the data array `type: "preserve"` leaves the array's
// filesystem and contents intact and remounts them in the freshly booted OS.
//
// Disk shape: 1 boot disk (single-disk hybrid bios_grub+ESP+ext4 root) + 2
// data disks (virtio) forming a RAID0 `/data` array — a scaled-down mirror of
// the ASUS RS501A 2-boot + 2-data cohort. The boot disk is the largest so the
// storage scripts can split boot-vs-data by size deterministically.
//
// Lifecycle: TWO VMs, each doing exactly ONE deploy, sharing the same two
// data qcow2 files:
//
//	VM-A (CREATE): fresh boot disk + fresh data disks -> raid0 /data created
//	  + a marker token T1 written into /data by the first-boot marker unit ->
//	  reboot -> phone_home -> VM-A is torn down (Stop()) so QEMU releases the
//	  qcow2 image lock on the data disks.
//	VM-B (per cell): a BRAND NEW VM/machine, fresh boot disk, but its two data
//	  disks are the SAME qcow2 files VM-A wrote its array onto -> deploy #2's
//	  storage script and params vary by cell -> reboot -> phone_home -> the
//	  deployed OS echoes the surviving marker to serial.
//
// This sidesteps the redeploy choreography an earlier one-VM version of this
// test needed (op-completion ordering, QMP.Reset re-PXE, the post-phone_home
// `running`-state overlay) — see
// docs/archeology/handoffs/2026-07-20-preserve-data-disks-qemu-choreography.md
// for why that approach was abandoned. Two VMs also matches how the feature
// is actually used: a redeploy of a box whose data disks persist is, from the
// data disks' point of view, indistinguishable from a fresh machine that
// happens to inherit already-populated disks.
//
// The marker write happens on VM-A's create path and is only READ after
// VM-B's deploy, so a broken preserve cannot "pass" by rewriting what it
// should have kept (the write is if-absent and tagged with the deploy's own
// unique token; see GenerateDataMarkerUnit).
//
// Cells:
//   - preserve: VM-B uses type:"preserve" -> T1 MUST survive, and the array
//     must be reassembled+remounted at boot (mdstat + findmnt asserts).
//   - recreate (NEG): VM-B uses type:"raid" (create) -> T1 MUST be gone (a
//     different token, VM-B's own). Proves the test detects destruction and
//     isn't vacuous.
//   - wipeall (NEG): VM-B uses type:"preserve" WITH --wipe-all-disks
//     (+--destructive) -> T1 MUST survive. Proves Phase-3's bystander
//     exclusion protects preserve members from the opt-in wipe.
func TestDeployPreserveDataDisk(t *testing.T) {
	cells := []preserveCell{
		{name: "preserve", deploy2: deploy2Preserve, expectSurvives: true},
		{name: "recreate", deploy2: deploy2Recreate, expectSurvives: false},
		{name: "wipeall", deploy2: deploy2WipeAll, expectSurvives: true},
	}
	for _, c := range cells {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runPreserveCell(t, c)
		})
	}
}

// deploy2Kind selects the storage script + params for VM-B's deploy.
type deploy2Kind int

const (
	// deploy2Preserve: type:"preserve" data array, no wipe-all.
	deploy2Preserve deploy2Kind = iota
	// deploy2Recreate: type:"raid" data array (re-created, destroying data).
	deploy2Recreate
	// deploy2WipeAll: type:"preserve" data array WITH --wipe-all-disks
	// (+--destructive).
	deploy2WipeAll
)

type preserveCell struct {
	name    string
	deploy2 deploy2Kind
	// expectSurvives is true when VM-A's marker token must still be the
	// content of /data/PRESERVE_MARKER after VM-B's boot.
	expectSurvives bool
}

func runPreserveCell(t *testing.T, c preserveCell) {
	t.Helper()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)
	slug := "preserve-" + c.name

	// The two data qcow2s live in a cell-scoped dir OUTSIDE any single VM's
	// t.TempDir() (both VM-A and VM-B attach the same files via
	// ExtraDisk.Path, and VM-A's own tempdir is torn down with VM-A).
	diskDir := t.TempDir()
	dataDisk0 := filepath.Join(diskDir, "data0.qcow2")
	dataDisk1 := filepath.Join(diskDir, "data1.qcow2")
	env.CreateDisk(t, dataDisk0, "6G")
	env.CreateDisk(t, dataDisk1, "6G")

	// Two distinct identities: VM-A and VM-B are different machines from the
	// worker's point of view, matching the two-VM design (a fresh machine
	// that happens to inherit already-populated data disks).
	systemUUIDA, macA := matrixIdentity("preserve-" + c.name + "-a")
	machineIDA := uuid.New()
	systemUUIDB, macB := matrixIdentity("preserve-" + c.name + "-b")
	machineIDB := uuid.New()

	// VM-A's unique marker token. Written into /data on the create path and
	// echoed on every subsequent boot.
	token1 := "T1-" + c.name + "-" + machineIDA.String()[:8]

	// --- VM-A: CREATE the data array + write the marker. ---
	deployOp1 := qemutest.NewOperation(machineIDA, model.OpDeploy)
	deployOp1.Params = mustMarshalParams(t, model.OpParams{
		Destructive: true, // the VM's data disks are blank, but be explicit
		DataMarker:  token1,
	})

	snap1 := env.NewSnapshot(slug, network).
		AddMachine(preserveMachine(machineIDA, macA, systemUUIDA, createStorageScript)).
		AddOperation(deployOp1).
		Build()
	env.Hub.Push(slug, snap1)

	// One site worker serves BOTH VMs (same slug/network) — matches how a
	// real site worker serves an entire pop's fleet, not one machine. Kept
	// so VM-B's start can be gated on the worker's cache having actually
	// applied snap2 (see the wait before vmB.Start below) — Hub.Push fans
	// snapshots to connected workers asynchronously, so nothing guarantees
	// snap2 has landed by the time VM-B would otherwise PXE.
	sw := env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	// 1 boot disk (10G, largest -> root) + 2 data disks (6G each -> raid0).
	// The scripts split boot-vs-data by size, so the boot disk MUST be the
	// largest.
	vmA := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.UEFI,
		MAC:        macA,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUIDA,
		DiskSize:   "10G",
		ExtraDisks: []qemutest.ExtraDisk{
			{Type: qemutest.DiskVirtio, Path: dataDisk0},
			{Type: qemutest.DiskVirtio, Path: dataDisk1},
		},
	})

	// Generous outer ctx; per-marker/observation budgets are the real
	// timeouts. Spans both VMs' full deploy+reboot lifecycles.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	vmA.Start(ctx)

	// VM-A's deploy to completion + reboot + phone_home.
	if err := vmA.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("VM-A deploy: waiting for deploy complete: %v\n%s", err, serialDump(vmA))
	}
	pushAwaitingPhoneHome(env, network, slug, machineIDA, macA, systemUUIDA, deployOp1, 2, createStorageScript)
	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineIDA); err != nil {
		t.Fatalf("VM-A deploy: deploy_complete observation: %v", err)
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineIDA); err != nil {
		t.Fatalf("VM-A deploy: phone_home (post-install boot failed): %v\n%s", err, serialDump(vmA))
	}
	// Prove VM-A actually created + mounted the array and wrote T1 before we
	// hand the disks to VM-B. The marker unit echoes on this first boot too.
	if err := vmA.Serial.WaitFor(ctx, "PRESERVE_MARKER install="+token1, qemutest.Budgets.PhoneHomeObs); err != nil {
		t.Fatalf("VM-A deploy: never saw the first-boot marker line for %q: %v\n%s", token1, err, serialDump(vmA))
	}
	line := lastMarkerLine(vmA)
	if got := markerField(line, "content"); got != token1 {
		t.Fatalf("VM-A deploy: /data/PRESERVE_MARKER content=%q, want the just-written token %q\nline: %s", got, token1, line)
	}
	if src := markerField(line, "source"); !strings.HasPrefix(src, "/dev/md") {
		t.Fatalf("VM-A deploy: /data source=%q, want an md device (array must be mounted)\nline: %s", src, line)
	}

	// Tear VM-A down fully before starting VM-B: qcow2 holds an exclusive
	// image lock on the data disks while the owning QEMU process is alive,
	// so VM-B's start would fail to open dataDisk0/dataDisk1 otherwise. The
	// marker script's `sync` (see internal/agent/target/datamarker.go) makes
	// sure T1 is actually on disk before we get here, not just in page cache.
	vmA.Stop()

	// --- VM-B: per-cell storage script + params, sharing VM-A's data disks. ---
	token2 := "T2-" + c.name + "-" + machineIDB.String()[:8]
	script2, params2 := deploy2Config(t, c.deploy2, token2)

	// VM-B is a BRAND NEW machine from the worker's point of view (its own
	// machine ID / MAC / system UUID) — this is a first deploy for VM-B, not
	// a redeploy, so deployOp2 is a plain pending op exactly like deployOp1
	// was for VM-A. No claimed/awaiting_phone_home priming and no
	// running-overlay dance are needed here (that machinery, in the old
	// single-VM version of this test, existed only to make ONE machine
	// re-PXE past its own post-phone_home state — a problem that doesn't
	// exist when VM-B has never phoned home).
	deployOp2 := qemutest.NewOperation(machineIDB, model.OpDeploy)
	deployOp2.Params = params2

	// snap2 deliberately does NOT re-add machine A or op1. Machine A's op1
	// sits claimed@awaiting_phone_home in the worker's local op map forever
	// (the op layer is an additive delta stream — see opstate_cache.go), but
	// that's harmless: activeLifecycleOpLocked/deployOpForMAC filter by
	// MachineID, so a lingering op1 can never be served to machine B's MAC.
	// VM-A is dead anyway, so nothing will ever ask for its boot action
	// again. Seq 3 (snap1=1, awaiting1=2, snap2=3, awaiting2=4).
	snap2 := env.NewSnapshot(slug, network).
		Seq(3).
		AddMachine(preserveMachine(machineIDB, macB, systemUUIDB, script2)).
		AddOperation(deployOp2).
		Build()
	env.Hub.Push(slug, snap2)

	// Push fans snap2 to the worker asynchronously (a goroutine per
	// connection); nothing else here blocks until the worker's cache has
	// actually applied it. Without this wait, VM-B could PXE before macB is
	// known to the worker, resolve to BootLocal (unknown MACs always local-
	// boot a blank disk), and the test would hang or fail in a way that
	// reads like a product bug instead of a harness race.
	waitBootDeployMAC(ctx, t, sw, macB)

	vmB := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.UEFI,
		MAC:        macB,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUIDB,
		DiskSize:   "10G",
		ExtraDisks: []qemutest.ExtraDisk{
			{Type: qemutest.DiskVirtio, Path: dataDisk0},
			{Type: qemutest.DiskVirtio, Path: dataDisk1},
		},
	})
	vmB.Start(ctx)

	if err := vmB.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("VM-B deploy (%s): waiting for deploy complete: %v\n%s", c.name, err, serialDump(vmB))
	}
	pushAwaitingPhoneHome(env, network, slug, machineIDB, macB, systemUUIDB, deployOp2, 4, script2)
	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineIDB); err != nil {
		t.Fatalf("VM-B deploy (%s): deploy_complete observation: %v", c.name, err)
	}

	// VM-B's cold boot from the installed disk: wait for THIS deploy's
	// marker line (tagged install=token2), which only prints once the
	// deployed OS booted, reassembled/remounted /data (fstab), and ran the
	// marker unit. Waiting on install=token2 (not just "PRESERVE_MARKER") is
	// what distinguishes this boot's line from VM-A's still-buffered line —
	// both VMs' serial streams are read via their own vm.Serial, so there's
	// no cross-VM buffering, but the tag keeps the assertion self-documenting
	// and matches the marker contract used across the file.
	if err := vmB.Serial.WaitFor(ctx, "PRESERVE_MARKER install="+token2, qemutest.Budgets.PhoneHomeObs); err != nil {
		t.Fatalf("VM-B deploy (%s): never saw the post-deploy marker line for %q "+
			"(deployed OS didn't boot, or /data never mounted): %v\n%s",
			c.name, token2, err, serialDump(vmB))
	}

	line = deploy2MarkerLine(vmB, token2)
	content := markerField(line, "content")
	source := markerField(line, "source")
	mdstat := markerField(line, "mdstat")

	if c.expectSurvives {
		// The preserved data survived: the marker content is STILL VM-A's
		// token (the if-absent write on VM-B's boot was a no-op because
		// /data/PRESERVE_MARKER already existed).
		if content != token1 {
			t.Fatalf("VM-B deploy (%s): expected preserved marker %q to survive, but content=%q "+
				"(the data array was NOT preserved)\nline: %s", c.name, token1, content, line)
		}
		// Reassembly + remount, not just byte survival.
		if !strings.HasPrefix(source, "/dev/md") {
			t.Fatalf("VM-B deploy (%s): /data source=%q, want an md device — the array must be reassembled+remounted at boot, not a stale mount\nline: %s",
				c.name, source, line)
		}
		if !strings.Contains(mdstat, "md") {
			t.Fatalf("VM-B deploy (%s): /proc/mdstat shows no md array (mdstat=%q); reassembly failed\nline: %s",
				c.name, mdstat, line)
		}
	} else {
		// The recreate path destroyed VM-A's data: /data was re-created
		// empty, so the if-absent write on VM-B's boot landed token2 (this
		// deploy's own token), NOT token1. This is the honest "the test
		// detects destruction" control — a vacuous test would still read
		// token1 here.
		if content == token1 {
			t.Fatalf("VM-B deploy (%s): marker content=%q == VM-A's token, but this cell RE-CREATES the array — "+
				"either the destruction didn't happen or the test is vacuous\nline: %s", c.name, content, line)
		}
		if content != token2 {
			t.Fatalf("VM-B deploy (%s): after recreate, marker content=%q, want VM-B's fresh token %q "+
				"(the recreated array should be empty then re-marked)\nline: %s", c.name, content, token2, line)
		}
	}
}

// deploy2Config returns the storage script content + op params for VM-B's
// deploy.
func deploy2Config(t *testing.T, kind deploy2Kind, token2 string) (script string, params json.RawMessage) {
	t.Helper()
	switch kind {
	case deploy2Preserve:
		return preserveStorageScript, mustMarshalParams(t, model.OpParams{
			Destructive: true,
			DataMarker:  token2,
		})
	case deploy2Recreate:
		return createStorageScript, mustMarshalParams(t, model.OpParams{
			Destructive: true,
			DataMarker:  token2,
		})
	case deploy2WipeAll:
		return preserveStorageScript, mustMarshalParams(t, model.OpParams{
			Destructive:  true,
			WipeAllDisks: true,
			DataMarker:   token2,
		})
	default:
		t.Fatalf("unknown deploy2Kind %d", int(kind))
		return "", nil
	}
}

// preserveMachine builds the machine snapshot entry for a preserve cell.
func preserveMachine(machineID uuid.UUID, mac, systemUUID, script string) qemutest.Machine {
	return qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		State:         model.MachineStateDeploying,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: &model.SnapshotStorageScript{Name: "preserve-data", Content: script},
	}
}

// pushAwaitingPhoneHome flips the deploy op to claimed+awaiting_phone_home so
// the post-install PXE returns BootLocal (the matrix pattern). Pushed on the
// deploy-complete serial marker to beat the reboot's PXE re-fetch.
func pushAwaitingPhoneHome(
	env *qemutest.Env, network *qemutest.Network,
	slug string, machineID uuid.UUID, mac, systemUUID string,
	op model.Operation, seq int64, script string,
) {
	awaiting := op
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	snap := env.NewSnapshot(slug, network).
		Seq(seq).
		AddMachine(preserveMachine(machineID, mac, systemUUID, script)).
		AddOperation(awaiting).
		Build()
	env.Hub.Push(slug, snap)
}

// mustMarshalParams marshals OpParams for an op.Params blob.
func mustMarshalParams(t *testing.T, p model.OpParams) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal op params: %v", err)
	}
	return raw
}

// markerFieldRE extracts a `key=value` field from a PRESERVE_MARKER line.
// Values run to the next space or (for mdstat=[...]) the closing bracket;
// content/source/install are space-free tokens.
func markerField(line, key string) string {
	// mdstat is bracketed: mdstat=[md0 ].
	if key == "mdstat" {
		re := regexp.MustCompile(`mdstat=\[([^\]]*)\]`)
		if m := re.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
		return ""
	}
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `=(\S+)`)
	if m := re.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// lastMarkerLine returns the most recent serial line containing
// "PRESERVE_MARKER install=", for asserting on VM-A's boot.
func lastMarkerLine(vm *qemutest.VM) string {
	var last string
	for _, l := range vm.Serial.Lines() {
		if strings.Contains(l, "PRESERVE_MARKER install=") {
			last = l
		}
	}
	return last
}

// deploy2MarkerLine returns the serial line for VM-B's marker (tagged
// install=token2), so a stale line can never be read by mistake.
func deploy2MarkerLine(vm *qemutest.VM, token2 string) string {
	want := "PRESERVE_MARKER install=" + token2
	for _, l := range vm.Serial.Lines() {
		if strings.Contains(l, want) {
			return l
		}
	}
	return ""
}

// serialDump renders the VM's captured serial for a failure message, matching
// the matrix's rich-failure convention.
func serialDump(vm *qemutest.VM) string {
	return "--- serial dump ---\n" + strings.Join(vm.Serial.Lines(), "\n")
}

// waitBootDeployMAC polls the site worker's boot-action decision for mac
// until it reports BootDeploy, so a VM is only started once the worker has
// actually applied the snapshot naming its machine/op — Hub.Push fans
// snapshots to connected workers asynchronously (one goroutine per
// connection), so there is no synchronous guarantee the push has landed by
// the time the caller would otherwise boot the VM. Fatals if the decision
// hasn't flipped within the deadline.
func waitBootDeployMAC(ctx context.Context, t *testing.T, sw *qemutest.SiteWorker, mac string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if sw.Cache.BootActionForMAC(mac) == model.BootDeploy {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitBootDeployMAC: context done before boot action flipped to BootDeploy for %s: %v", mac, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("waitBootDeployMAC: worker never served BootDeploy for %s within 30s "+
		"(the snapshot naming this machine/op never landed); last action=%v",
		mac, sw.Cache.BootActionForMAC(mac))
}

// createStorageScript lays out 1 boot disk (single-disk hybrid
// bios_grub+ESP+ext4 root, like defaultStorageScript) + a RAID0 `/data` array
// across the two data disks, CREATING the array with the fixed array_uuid so a
// later preserve script can assemble by the same identity. Boot vs data is
// split by size: the largest disk is the boot disk, the rest are data members.
//
// The data disks are listed as BOTH empty `disk`-type targets AND raid
// members — the exact shape the production ASUS RS501A script uses
// (assets/storage/100-asus-rs501a). That dual listing is load-bearing on the
// RECREATE (create-over-existing) path: WipeStaleState zaps a disk-type
// target's md superblock before `mdadm --create` runs, so the create never
// hits the interactive "appears to be part of a raid array — continue?" prompt
// (the agent runs mdadm with no tty; an EOF answer would abort the storage
// phase). On VM-A the disks are blank so the zap is a harmless no-op.
// (The preserve script deliberately does NOT list data as disk targets — a
// preserve member that is also a disk target is a validation/preflight FATAL.)
//
// Reads inventory from $SLAGWORKS_INVENTORY.
var createStorageScript = fmt.Sprintf(`#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(inv["disks"], key=lambda d: d["size_bytes"])
if len(disks) != 3:
    print(f"expected 3 disks (1 boot + 2 data), got {len(disks)}: "
          f"{[d['name'] for d in disks]}", file=sys.stderr)
    sys.exit(1)

# Largest disk = boot; the two smaller = data RAID0 members.
data = sorted(d["name"] for d in disks[:2])
boot = disks[2]["name"]

layout = {
    "layout": {
        boot: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot}1", "size": "1M", "bios_grub": True},
                {"name": f"{boot}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot}3", "fs": "ext4", "size": "rest"},
            ],
        },
        # Data disks as empty disk targets so WipeStaleState clears any prior
        # md superblock before mdadm --create (matches the asus script).
        data[0]: {"type": "disk", "ptable": "gpt", "partitions": []},
        data[1]: {"type": "disk", "ptable": "gpt", "partitions": []},
        "mddata": {
            "type": "raid", "level": 0,
            "members": [data[0], data[1]],
            "fs": "xfs",
            "array_uuid": %q,
        },
    },
    "mounts": {
        "/":         {"device": f"{boot}3"},
        "/boot/efi": {"device": f"{boot}2"},
        "/data":     {"device": "mddata"},
    },
}
json.dump(layout, sys.stdout)
`, fixedDataArrayUUID)

// preserveStorageScript is identical to createStorageScript for the boot disk,
// but the data array is `type: "preserve"` carrying the SAME fixed array_uuid
// so the agent assembles (never recreates) the existing array and remounts it
// at /data. No mkfs — the array's xfs and its contents survive.
var preserveStorageScript = fmt.Sprintf(`#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(inv["disks"], key=lambda d: d["size_bytes"])
if len(disks) != 3:
    print(f"expected 3 disks (1 boot + 2 data), got {len(disks)}: "
          f"{[d['name'] for d in disks]}", file=sys.stderr)
    sys.exit(1)

data = sorted(d["name"] for d in disks[:2])
boot = disks[2]["name"]

layout = {
    "layout": {
        boot: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{boot}1", "size": "1M", "bios_grub": True},
                {"name": f"{boot}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{boot}3", "fs": "ext4", "size": "rest"},
            ],
        },
        "mddata": {
            "type": "preserve", "level": 0,
            "members": [data[0], data[1]],
            "fs": "xfs",
            "array_uuid": %q,
        },
    },
    "mounts": {
        "/":         {"device": f"{boot}3"},
        "/boot/efi": {"device": f"{boot}2"},
        "/data":     {"device": "mddata"},
    },
}
json.dump(layout, sys.stdout)
`, fixedDataArrayUUID)
