//go:build integration

package qemutest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeploy_ESPOn4KnDisk covers the FAT32 minimum on a disk with
// 4096-byte logical sectors, where it is 257M. A 256M ESP there used to
// install cleanly (mkfs.fat -F 32 only warned that the cluster count was
// below the FAT32 minimum) and then fail to boot, because the firmware could
// not read the ESP. The agent must now refuse a 256M ESP before touching the
// disk, and a 257M ESP must install, boot from disk and phone home.
func TestDeploy_ESPOn4KnDisk(t *testing.T) {
	cells := []struct {
		name    string
		espSize string
		fatal   bool
	}{
		{name: "esp-256M-refused", espSize: "256M", fatal: true},
		{name: "esp-257M-boots", espSize: "257M"},
	}
	for _, c := range cells {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runESPOn4KnCell(t, "esp4kn-"+c.name, c.espSize, c.fatal)
		})
	}
}

// esp4KnStorageScript lays out the first disk as a UEFI ESP of the given
// size and an ext4 root on the rest.
func esp4KnStorageScript(espSize string) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disk = inv["disks"][0]["name"]
json.dump({
    "layout": {
        disk: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{disk}1", "fs": "vfat", "size": %q, "bootable": True},
                {"name": f"{disk}2", "fs": "ext4", "size": "rest"},
            ],
        },
    },
    "mounts": {
        "/":         {"device": f"{disk}2"},
        "/boot/efi": {"device": f"{disk}1"},
    },
}, sys.stdout)
`, espSize)
}

func runESPOn4KnCell(t *testing.T, slug, espSize string, fatal bool) {
	t.Helper()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID, mac := matrixIdentity(slug)
	machineID := uuid.New()
	script := &model.SnapshotStorageScript{Name: "esp-4kn", Content: esp4KnStorageScript(espSize)}
	machine := qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		State:         model.MachineStateDeploying,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: script,
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
		Firmware:      qemutest.UEFI,
		MAC:           mac,
		Network:       network,
		Memory:        "4096",
		SMBIOSUUID:    systemUUID,
		NoPrimaryDisk: true,
		ExtraDisks: []qemutest.ExtraDisk{
			{Type: qemutest.DiskVirtio, Size: "8G", LogicalBlockSize: 4096},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	vm.Start(ctx)

	if fatal {
		if err := vm.Serial.WaitFor(ctx, "FATAL during planning storage layout", qemutest.Budgets.FATALMarker); err != nil {
			t.Fatalf("waiting for the planner to refuse a %s ESP on a 4Kn disk: %v", espSize, err)
		}
		if !vm.Serial.Contains("make the partition at least 257M") {
			t.Errorf("the planning failure should name the 257M minimum for a 4Kn disk")
		}
		if vm.Serial.Contains(`"msg":"deploy complete"`) {
			t.Errorf("a %s ESP on a 4Kn disk was installed", espSize)
		}
		return
	}

	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for deploy complete: %v", err)
	}
	// Flip to awaiting_phone_home before the reboot's PXE fetch so the
	// worker answers it with BootLocal (see runMatrixCell).
	awaiting := deployOp
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	env.Hub.Push(slug, env.NewSnapshot(slug, network).Seq(2).AddMachine(machine).AddOperation(awaiting).Build())

	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("waiting for phone_home: the firmware did not boot the %s ESP on the 4Kn disk: %v", espSize, err)
	}
}
