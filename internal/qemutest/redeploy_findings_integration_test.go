//go:build integration && proptest_findings

package qemutest_test

// Two regression tests that need a REDEPLOY: a second machine inherits the
// disks a first deploy wrote, with nothing wiping them in between. Both fail
// on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ \
//	    -run 'TestDeploy_StaleESPInUnformattedSlot|TestDeploy_StaleMemberWithPinnedArrayUUID'
//
// They need VMConfig.NetBootIndex and VMConfig.ExtraArgs (added with the
// storage property suite) so the firmware boots PXE first, as the production
// fleet does, even when a disk already carries a bootloader.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// redeployRig deploys onto one set of disk images with a fresh VM per deploy.
type redeployRig struct {
	t       *testing.T
	env     *qemutest.Env
	network *qemutest.Network
	images  *qemutest.ImageDir
	sw      *qemutest.SiteWorker
	slug    string
	seq     int64
	disks   []string
}

func newRedeployRig(t *testing.T, slug string, sizes ...string) *redeployRig {
	env := qemutest.New(t)
	r := &redeployRig{t: t, env: env, network: env.NewNetwork(t), images: env.UseCanonicalImages(t), slug: slug}
	dir := t.TempDir()
	for i, size := range sizes {
		path := filepath.Join(dir, fmt.Sprintf("disk%d.qcow2", i))
		env.CreateDisk(t, path, size)
		r.disks = append(r.disks, path)
	}
	return r
}

// deploy runs one deploy to its first phone-home and returns the running VM.
func (r *redeployRig) deploy(ctx context.Context, name, script string, params model.OpParams) *qemutest.VM {
	t := r.t
	systemUUID, mac := matrixIdentity(r.slug + "-" + name)
	machine := qemutest.Machine{
		ID: uuid.New(), MAC: mac, State: model.MachineStateDeploying, PXEEligible: true, SystemUUID: systemUUID,
		StorageScript: &model.SnapshotStorageScript{Name: name, Content: script},
	}
	op := qemutest.NewOperation(machine.ID, model.OpDeploy)
	op.Params = mustMarshalParams(t, params)
	push := func(o model.Operation) {
		r.seq++
		r.env.Hub.Push(r.slug, r.env.NewSnapshot(r.slug, r.network).Seq(r.seq).AddMachine(machine).AddOperation(o).Build())
	}
	push(op)
	if r.sw == nil {
		r.sw = r.env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
			Slug: r.slug, Network: r.network, Images: r.images, DefaultImage: "noble-pxe",
		})
	}
	waitBootDeployMAC(ctx, t, r.sw, mac)

	var args []string
	for i, path := range r.disks {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=d%d", path, i),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=d%d,bootindex=%d", i, i+2))
	}
	vm := r.env.NewVM(t, qemutest.VMConfig{
		Firmware: qemutest.UEFI, MAC: mac, Network: r.network, Memory: "4096", SMBIOSUUID: systemUUID,
		NoPrimaryDisk: true, NetBootIndex: 1, ExtraArgs: args,
	})
	vm.Start(ctx)
	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("%s: waiting for deploy complete: %v\n%s", name, err, serialDump(vm))
	}
	awaiting := op
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	push(awaiting)
	if _, err := r.env.Hub.WaitForObservation(ctx, r.slug, "deploy_complete", machine.ID); err != nil {
		t.Fatalf("%s: deploy_complete observation: %v", name, err)
	}
	// The first boot can sit in a 90s device timeout before it phones home.
	var err error
	for i := 0; i < 3; i++ {
		if _, err = r.env.Hub.WaitForObservation(ctx, r.slug, "phone_home", machine.ID); err == nil {
			break
		}
	}
	if err != nil {
		// Say why when the console shows it: a mount that never got its
		// device leaves the first boot in emergency mode, and a machine in
		// emergency mode does not phone home.
		t.Fatalf("%s: the installed system did not phone home: %v\nfailed mounts: %s\nmd state: %s\n%s",
			name, err, grepSerial(vm, "Dependency failed for"), grepSerial(vm, "md0 : "), serialDump(vm))
	}
	return vm
}

// mirrorScript is the qemutest raid1 script with the ESP disk chosen by the
// caller: a root mirror over two disks, the ESP on one, an unformatted slot
// of the same size on the other.
func mirrorScript(espDisk int) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(d["name"] for d in inv["disks"])
esp, other = disks[%d], disks[%d]
layout = {
    "layout": {
        esp: {"type": "disk", "ptable": "gpt", "boot": True, "partitions": [
            {"name": f"{esp}1", "fs": "vfat", "size": "512M", "bootable": True},
            {"name": f"{esp}2", "size": "rest"},
        ]},
        other: {"type": "disk", "ptable": "gpt", "partitions": [
            {"name": f"{other}1", "size": "512M"},
            {"name": f"{other}2", "size": "rest"},
        ]},
        "mdroot": {"type": "raid", "level": 1, "members": [f"{esp}2", f"{other}2"], "fs": "ext4"},
    },
    "mounts": {"/": {"device": "mdroot"}, "/boot/efi": {"device": f"{esp}1"}},
}
json.dump(layout, sys.stdout)
`, espDisk, 1-espDisk)
}

// TestDeploy_StaleESPInUnformattedSlot: deploy a root mirror with the ESP on
// the first disk, then redeploy it --destructive with the ESP on the second.
// The first disk's partition 1 is now an unformatted slot at the offset the
// old ESP had. The stale-state wipe clears the whole-disk signatures only, so
// the old ESP, with the old bootloader and its root=UUID of a filesystem that
// no longer exists, is still there. The first boot works through the
// one-shot BootNext; the next one goes PXE -> boot-local -> the firmware's
// first disk, boots the old ESP, and stops in the initramfs.
func TestDeploy_StaleESPInUnformattedSlot(t *testing.T) {
	t.Parallel()
	r := newRedeployRig(t, "stale-esp-slot", "8G", "8G")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	vmA := r.deploy(ctx, "a", mirrorScript(0), model.OpParams{})
	vmA.Stop()
	vmB := r.deploy(ctx, "b", mirrorScript(1), model.OpParams{Destructive: true})

	cursor := vmB.Serial.Cursor()
	if err := vmB.QMP.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		for _, l := range vmB.Serial.Lines()[cursor:] {
			if strings.Contains(l, "Reached target") && strings.Contains(l, "Multi-User System") {
				return
			}
			if strings.Contains(l, "does not exist.  Dropping to a shell") {
				t.Fatalf("second boot of the redeployed machine booted a stale ESP and found no root: %s", strings.TrimSpace(l))
			}
			if strings.Contains(l, "PXE boot failed after") {
				t.Fatalf("second boot of the redeployed machine found nothing to boot: %s", strings.TrimSpace(l))
			}
		}
	}
	t.Fatalf("second boot: no outcome within 4m\n%s", serialDump(vmB))
}

const pinnedDataUUID = "5d1f0c1e-7a3b-4c2d-9e8f-0a1b2c3d4e5f"

// pinnedDataScript roots on the largest disk and builds a RAID1 /data with a
// pinned array_uuid over two of the three smaller disks, chosen by index in
// size order.
func pinnedDataScript(m0, m1 int) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = [d["name"] for d in sorted(inv["disks"], key=lambda d: d["size_bytes"])]
boot, small = disks[3], disks[:3]
members = [small[%d], small[%d]]
layout = {
    "layout": {
        boot: {"type": "disk", "ptable": "gpt", "boot": True, "partitions": [
            {"name": f"{boot}1", "fs": "vfat", "size": "512M", "bootable": True},
            {"name": f"{boot}2", "fs": "ext4", "size": "rest"},
        ]},
        members[0]: {"type": "disk", "ptable": "gpt", "partitions": []},
        members[1]: {"type": "disk", "ptable": "gpt", "partitions": []},
        "mddata": {"type": "raid", "level": 1, "members": members, "fs": "xfs", "array_uuid": %q},
    },
    "mounts": {"/": {"device": f"{boot}2"}, "/boot/efi": {"device": f"{boot}1"}, "/data": {"device": "mddata"}},
}
json.dump(layout, sys.stdout)
`, m0, m1, pinnedDataUUID)
}

// TestDeploy_StaleMemberWithPinnedArrayUUID: /data is a RAID1 with a pinned
// array_uuid, the way the shipped RS501A script pins it. The first deploy
// builds it over small disks 0 and 1; the redeploy, --destructive, builds it
// over 1 and 2. Disk 0 is now outside the layout and still carries a member
// superblock with the pinned UUID, and nothing wipes it. When udev offers
// that disk to mdadm before the real members, it claims the array, the real
// members are refused, /data never appears, and the boot sits in a device
// timeout, falls to emergency mode and continues without /data.
//
// Which disk udev offers first is a race, so one boot can pass by luck: the
// property suite's TestStoragePropStaleUUIDRepeat saw the stale member win on
// 5 of 12 boots. This test checks six boots, which all pass by luck about one
// run in twenty.
func TestDeploy_StaleMemberWithPinnedArrayUUID(t *testing.T) {
	t.Parallel()
	r := newRedeployRig(t, "stale-pinned-member", "10G", "4G", "4100M", "4200M")
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()

	vmA := r.deploy(ctx, "a", pinnedDataScript(0, 1), model.OpParams{})
	vmA.Stop()
	token := "T-" + uuid.NewString()[:8]
	vmB := r.deploy(ctx, "b", pinnedDataScript(1, 2), model.OpParams{Destructive: true, DataMarker: token})

	// The data-marker unit reports, on every boot, where /data is mounted
	// from. A boot that loses the array either reports NOT-MOUNTED or never
	// gets that far: it times out on the /data device and stops in
	// emergency mode.
	cursor := 0
	const boots = 6
	for boot := 1; boot <= boots; boot++ {
		var line string
		for deadline := time.Now().Add(6 * time.Minute); line == "" && time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			for _, l := range vmB.Serial.Lines()[cursor:] {
				if strings.Contains(l, "PRESERVE_MARKER install="+token) {
					line = l
				}
				if strings.Contains(l, "Dependency failed for") && strings.Contains(l, "data.mount") {
					t.Fatalf("boot %d of the redeployed machine: the /data array did not assemble and the boot fell to emergency mode: %s\n%s",
						boot, strings.TrimSpace(l), grepSerial(vmB, "md0 : "))
				}
			}
		}
		if line == "" {
			t.Fatalf("boot %d: no data-marker line within 6m\n%s", boot, serialDump(vmB))
		}
		if src := markerField(line, "source"); !strings.HasPrefix(src, "/dev/md") {
			t.Fatalf("boot %d of the redeployed machine: /data is not mounted from the array (source=%s).\n"+
				"marker: %s\nmd state at that boot: %s",
				boot, src, strings.TrimSpace(line), markerField(line, "mdstat"))
		}
		cursor = vmB.Serial.Cursor()
		if boot < boots {
			if err := vmB.QMP.Reset(); err != nil {
				t.Fatalf("reset: %v", err)
			}
		}
	}
}
