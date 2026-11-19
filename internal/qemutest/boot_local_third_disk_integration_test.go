//go:build integration && proptest_findings

package qemutest_test

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ -run TestDeploy_RebootsWhenBootDiskIsThird
//
// Needs VMConfig.NetBootIndex and VMConfig.ExtraArgs (added with the storage
// property suite): without a bootindex on the NIC, OVMF boots the installed
// disk directly and never takes the PXE path production firmware takes.

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

// largestDiskStorageScript installs onto the largest disk and leaves the
// others alone, the way a script that picks its boot disk by model does.
const largestDiskStorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disk = max(inv["disks"], key=lambda d: d["size_bytes"])["name"]
layout = {
    "layout": {
        disk: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{disk}1", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{disk}2", "fs": "ext4", "size": "rest"},
            ],
        },
    },
    "mounts": {"/": {"device": f"{disk}2"}, "/boot/efi": {"device": f"{disk}1"}},
}
json.dump(layout, sys.stdout)
`

// TestDeploy_RebootsWhenBootDiskIsThird installs onto the third of three
// disks and then restarts the installed system. The first boot after the
// deploy goes through the one-shot UEFI BootNext the agent sets; every later
// boot goes PXE -> boot-local -> `sanboot --drive 0x80 || --drive 0x81`,
// which never reaches the third disk. The embedded iPXE script retries the
// chain twenty times and drops to a shell, where the machine stays.
func TestDeploy_RebootsWhenBootDiskIsThird(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID, mac := matrixIdentity("boot-disk-is-third")
	machineID := uuid.New()
	const slug = "boot-disk-is-third"
	machine := qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		State:         model.MachineStateDeploying,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: &model.SnapshotStorageScript{Name: "largest-disk", Content: largestDiskStorageScript},
	}
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	env.Hub.Push(slug, env.NewSnapshot(slug, network).AddMachine(machine).AddOperation(deployOp).Build())
	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	// Two small blank disks first, the boot disk last. The NIC is first in
	// the firmware boot order, as on the production fleet.
	dir := t.TempDir()
	var diskArgs []string
	for i, size := range []string{"4G", "5G", "10G"} {
		path := filepath.Join(dir, fmt.Sprintf("disk%d.qcow2", i))
		env.CreateDisk(t, path, size)
		diskArgs = append(diskArgs,
			"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=d%d", path, i),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=d%d,bootindex=%d", i, i+2))
	}
	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:      qemutest.UEFI,
		MAC:           mac,
		Network:       network,
		Memory:        "4096",
		SMBIOSUUID:    systemUUID,
		NoPrimaryDisk: true,
		NetBootIndex:  1,
		ExtraArgs:     diskArgs,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	vm.Start(ctx)

	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for deploy complete: %v\n%s", err, serialDump(vm))
	}
	awaiting := deployOp
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	env.Hub.Push(slug, env.NewSnapshot(slug, network).Seq(2).AddMachine(machine).AddOperation(awaiting).Build())
	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("first boot: the installed system did not phone home: %v\n%s", err, serialDump(vm))
	}

	// Power-cycle the machine. The installed kernel announces itself on the
	// serial console when it starts; the boot-local failure has its own
	// marker, so the wrong outcome fails at once.
	cursor := vm.Serial.Cursor()
	if err := vm.QMP.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		lines := vm.Serial.Lines()[cursor:]
		var tried []string
		for _, l := range lines {
			if strings.Contains(l, "SAN device") && len(tried) < 8 {
				tried = append(tried, l)
			}
			if strings.Contains(l, "Linux version ") {
				return
			}
			if strings.Contains(l, "PXE boot failed after") || strings.Contains(l, "slagworks: PXE attempt 3") {
				t.Fatalf("second boot: the machine did not reach its installed system; boot-local tried:\n%s",
					strings.Join(tried, "\n"))
			}
		}
	}
	t.Fatalf("second boot: neither the installed kernel nor a boot-local failure appeared within 3m\n%s", serialDump(vm))
}
