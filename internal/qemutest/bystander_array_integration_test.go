//go:build integration

package qemutest_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeploy_BystanderArrayStaysUnassembled: a first deploy builds a RAID1
// /data array on two small disks; a second, --destructive deploy roots the
// machine on a RAID1 over two other disks and leaves the small disks out of
// its layout. Nothing wipes them (no --wipe-all-disks), so they still carry
// the old array, whose superblock asks for /dev/md0, the name the new root
// array has too. If the installed system's initramfs assembles the old array
// first, the root array is refused ("mdadm: /dev/md0 is already in use") and
// the boot stops in the initramfs; if the root wins, the old array comes up
// beside it under another name. The installed system must assemble exactly
// the arrays its layout names: here, the root array alone.
//
// Two VMs share the data disk images, the pattern TestDeployPreserveDataDisk
// uses. The data disks come first in PCI order so their members reach udev
// before the root array's, the order in which the old array takes md0. The
// second deploy's first boot is the one-shot UEFI BootNext into the
// installed system, so the order does not stop the firmware finding it.
func TestDeploy_BystanderArrayStaysUnassembled(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)
	const slug = "bystander-array"

	dir := t.TempDir()
	data0, data1 := filepath.Join(dir, "data0.qcow2"), filepath.Join(dir, "data1.qcow2")
	env.CreateDisk(t, data0, "4G")
	env.CreateDisk(t, data1, "4G")
	dataDisks := []qemutest.ExtraDisk{
		{Type: qemutest.DiskVirtio, Path: data0},
		{Type: qemutest.DiskVirtio, Path: data1},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	var sw *qemutest.SiteWorker
	var seq int64
	// deploy runs one deploy on a fresh machine identity through
	// "deploy complete" and returns the VM, still running into its first
	// boot of the installed system.
	deploy := func(name, script string, params model.OpParams, disks []qemutest.ExtraDisk) (*qemutest.VM, uuid.UUID) {
		systemUUID, mac := matrixIdentity(slug + "-" + name)
		machine := qemutest.Machine{
			ID: uuid.New(), MAC: mac, State: model.MachineStateDeploying, PXEEligible: true, SystemUUID: systemUUID,
			StorageScript: &model.SnapshotStorageScript{Name: "bystander-" + name, Content: script},
		}
		op := qemutest.NewOperation(machine.ID, model.OpDeploy)
		op.Params = mustMarshalParams(t, params)
		push := func(o model.Operation) {
			seq++
			env.Hub.Push(slug, env.NewSnapshot(slug, network).Seq(seq).AddMachine(machine).AddOperation(o).Build())
		}
		push(op)
		if sw == nil {
			sw = env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
				Slug: slug, Network: network, Images: images, DefaultImage: "noble-pxe",
			})
		}
		waitBootDeployMAC(ctx, t, sw, mac)

		vm := env.NewVM(t, qemutest.VMConfig{
			Firmware: qemutest.UEFI, MAC: mac, Network: network, Memory: "4096", SMBIOSUUID: systemUUID,
			NoPrimaryDisk: true, ExtraDisks: disks,
		})
		vm.Start(ctx)
		if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
			t.Fatalf("deploy %s: waiting for deploy complete: %v\n%s", name, err, serialDump(vm))
		}
		// Push awaiting_phone_home at once so a PXE fetch on the reboot is
		// answered with boot-local, not the deploy again.
		awaiting := op
		awaiting.Status = model.OpClaimed
		awaiting.Phase = "awaiting_phone_home"
		push(awaiting)
		if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machine.ID); err != nil {
			t.Fatalf("deploy %s: deploy_complete observation: %v", name, err)
		}
		return vm, machine.ID
	}

	// Deploy A: root on the large disk, /data a RAID1 over the two small
	// ones. Its installed system must boot before the disks move on.
	vmA, idA := deploy("a", bystanderCreateScript, model.OpParams{Destructive: true},
		append(slices.Clone(dataDisks), qemutest.ExtraDisk{Type: qemutest.DiskVirtio, Size: "8G"}))
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", idA); err != nil {
		t.Fatalf("deploy a: the installed system did not phone home: %v\n%s", err, serialDump(vmA))
	}
	vmA.Stop()

	// Deploy B: a root mirror over two fresh disks; the data disks are
	// attached but outside the layout.
	token := "bystander-" + uuid.NewString()[:8]
	vmB, idB := deploy("b", bystanderMirrorScript, model.OpParams{Destructive: true, DataMarker: token},
		append(slices.Clone(dataDisks),
			qemutest.ExtraDisk{Type: qemutest.DiskVirtio, Size: "8G"},
			qemutest.ExtraDisk{Type: qemutest.DiskVirtio, Size: "8G"}))

	// The data-marker unit prints /proc/mdstat's array names on every boot
	// of the installed system. A boot that loses the root array never gets
	// there: it stops in the initramfs once the root wait runs out, which
	// takes longer than a good boot's phone home, hence the longer wait.
	const bootWait = 5 * time.Minute
	var line string
	for deadline := time.Now().Add(bootWait); line == "" && time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		for _, l := range vmB.Serial.Lines() {
			if strings.Contains(l, "PRESERVE_MARKER install="+token) {
				line = l
			}
			if strings.Contains(l, "Dropping to a shell") {
				t.Fatalf("the redeployed machine stopped in the initramfs: %s\nmdadm lines:\n%s",
					strings.TrimSpace(l), serialLinesWith(vmB, "mdadm", "md0", "md127"))
			}
		}
	}
	if line == "" {
		t.Fatalf("the redeployed machine's installed system printed no data-marker line within %s\nmdadm lines:\n%s\n%s",
			bootWait, serialLinesWith(vmB, "mdadm", "md0", "md127"), serialDump(vmB))
	}
	if got := strings.Fields(markerField(line, "mdstat")); !slices.Equal(got, []string{"md0"}) {
		t.Fatalf("the installed system assembled md arrays %v, want only the layout's root array [md0]: "+
			"an array on a disk outside the layout was assembled\nmarker: %s\nmdadm lines:\n%s",
			got, strings.TrimSpace(line), serialLinesWith(vmB, "mdadm", "md0", "md127"))
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", idB); err != nil {
		t.Fatalf("deploy b: the installed system did not phone home: %v\n%s", err, serialDump(vmB))
	}
}

// serialLinesWith returns the VM's serial lines that contain any of subs.
func serialLinesWith(vm *qemutest.VM, subs ...string) string {
	var out []string
	for _, l := range vm.Serial.Lines() {
		for _, s := range subs {
			if strings.Contains(l, s) {
				out = append(out, strings.TrimSpace(l))
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// bystanderCreateScript roots the machine on its largest disk and builds a
// RAID1 /data over the two smaller ones, with no pinned array UUID. The data
// disks are also empty disk targets, the shape createStorageScript uses.
const bystanderCreateScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(inv["disks"], key=lambda d: d["size_bytes"])
if len(disks) != 3:
    print(f"expected 3 disks, got {[d['name'] for d in disks]}", file=sys.stderr)
    sys.exit(1)
data = sorted(d["name"] for d in disks[:2])
boot = disks[2]["name"]
layout = {
    "layout": {
        boot: {"type": "disk", "ptable": "gpt", "boot": True, "partitions": [
            {"name": f"{boot}1", "size": "1M", "bios_grub": True},
            {"name": f"{boot}2", "fs": "vfat", "size": "512M", "bootable": True},
            {"name": f"{boot}3", "fs": "ext4", "size": "rest"},
        ]},
        data[0]: {"type": "disk", "ptable": "gpt", "partitions": []},
        data[1]: {"type": "disk", "ptable": "gpt", "partitions": []},
        "mddata": {"type": "raid", "level": 1, "members": data, "fs": "xfs"},
    },
    "mounts": {"/": {"device": f"{boot}3"}, "/boot/efi": {"device": f"{boot}2"}, "/data": {"device": "mddata"}},
}
json.dump(layout, sys.stdout)
`

// bystanderMirrorScript roots the machine on a RAID1 over its two largest
// disks, the ESP on the first of them, and leaves every other disk out of
// the layout.
const bystanderMirrorScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(inv["disks"], key=lambda d: d["size_bytes"])
if len(disks) != 4:
    print(f"expected 4 disks, got {[d['name'] for d in disks]}", file=sys.stderr)
    sys.exit(1)
d0, d1 = sorted(d["name"] for d in disks[2:])
layout = {
    "layout": {
        d0: {"type": "disk", "ptable": "gpt", "boot": True, "partitions": [
            {"name": f"{d0}1", "size": "1M", "bios_grub": True},
            {"name": f"{d0}2", "fs": "vfat", "size": "512M", "bootable": True},
            {"name": f"{d0}3", "size": "rest"},
        ]},
        d1: {"type": "disk", "ptable": "gpt", "partitions": [
            {"name": f"{d1}1", "size": "1M", "bios_grub": True},
            {"name": f"{d1}2", "size": "512M"},
            {"name": f"{d1}3", "size": "rest"},
        ]},
        "mdroot": {"type": "raid", "level": 1, "members": [f"{d0}3", f"{d1}3"], "fs": "ext4"},
    },
    "mounts": {"/": {"device": "mdroot"}, "/boot/efi": {"device": f"{d0}2"}},
}
json.dump(layout, sys.stdout)
`
