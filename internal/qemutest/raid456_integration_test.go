//go:build integration && proptest_findings

package qemutest_test

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ -run TestDeploy_RAID5DataArray

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// raid5DataStorageScript puts the root on the largest disk and a RAID5 /data
// array across the other three, the way the shipped scripts lay out a data
// array (members also declared as empty disk targets).
const raid5DataStorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(inv["disks"], key=lambda d: d["size_bytes"])
if len(disks) != 4:
    print(f"expected 4 disks (1 boot + 3 data), got {len(disks)}", file=sys.stderr)
    sys.exit(1)
data = sorted(d["name"] for d in disks[:3])
boot = disks[3]["name"]

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
        data[0]: {"type": "disk", "ptable": "gpt", "partitions": []},
        data[1]: {"type": "disk", "ptable": "gpt", "partitions": []},
        data[2]: {"type": "disk", "ptable": "gpt", "partitions": []},
        "mddata": {"type": "raid", "level": 5, "members": data, "fs": "xfs"},
    },
    "mounts": {
        "/":         {"device": f"{boot}3"},
        "/boot/efi": {"device": f"{boot}2"},
        "/data":     {"device": "mddata"},
    },
}
json.dump(layout, sys.stdout)
`

// TestDeploy_RAID5DataArray deploys a layout with a RAID5 data array, a level
// docs/user/storage-scripts.md lists as supported and storage.Plan accepts.
// The deploy must complete and the installed system must boot. Today
// `mdadm --create --level=5` fails in the deploy initrd with "RUN_ARRAY
// failed: Invalid argument" because the raid456 module is not in it, and the
// agent FATALs with the disks already repartitioned.
func TestDeploy_RAID5DataArray(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID, mac := matrixIdentity("raid5-data-array")
	machineID := uuid.New()
	const slug = "raid5-data-array"
	machine := qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		State:         model.MachineStateDeploying,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: &model.SnapshotStorageScript{Name: "raid5-data", Content: raid5DataStorageScript},
	}
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	env.Hub.Push(slug, env.NewSnapshot(slug, network).AddMachine(machine).AddOperation(deployOp).Build())
	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.UEFI,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
		ExtraDisks: []qemutest.ExtraDisk{
			{Type: qemutest.DiskVirtio, Size: "4G"},
			{Type: qemutest.DiskVirtio, Size: "4G"},
			{Type: qemutest.DiskVirtio, Size: "4G"},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	vm.Start(ctx)

	// Fail on the FATAL as soon as it appears instead of waiting out the
	// deploy budget.
	done := false
	for deadline := time.Now().Add(qemutest.Budgets.DeployComplete); time.Now().Before(deadline) && !done; time.Sleep(250 * time.Millisecond) {
		if vm.Serial.Contains("FATAL during ") {
			t.Fatalf("deploy of a RAID5 data array failed:\n%s\n%s",
				grepSerial(vm, "FATAL during "), grepSerial(vm, "RUN_ARRAY"))
		}
		done = vm.Serial.Contains(`"msg":"deploy complete"`)
	}
	if !done {
		t.Fatalf("deploy did not complete within %s\n%s", qemutest.Budgets.DeployComplete, serialDump(vm))
	}

	awaiting := deployOp
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	env.Hub.Push(slug, env.NewSnapshot(slug, network).Seq(2).AddMachine(machine).AddOperation(awaiting).Build())
	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("the installed system with a RAID5 /data did not boot and phone home: %v\n%s", err, serialDump(vm))
	}
}
