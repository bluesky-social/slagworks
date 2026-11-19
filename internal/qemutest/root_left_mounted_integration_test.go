//go:build integration && proptest_findings

package qemutest_test

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ -run TestDeploy_UnmountsRootBeforeReboot

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeploy_UnmountsRootBeforeReboot deploys the matrix's UEFI RAID1 cell
// and requires that the agent unmounted everything it mounted before it
// reported "deploy complete" and rebooted.
//
// Today it does not. target.ConfigureRAID bind-mounts /sys into /target for
// update-initramfs and then discards the result of `umount /target/sys`,
// which on a UEFI machine fails "target is busy". /target therefore cannot be
// unmounted either; CleanupMounts logs "umount failed" as a warning, and the
// machine is rebooted with its freshly installed root filesystem, and the
// array under it, still mounted. The first boot replays the journal.
// The property suite saw this on 21 of 21 UEFI deploys that had an md array,
// and on none of the BIOS ones.
func TestDeploy_UnmountsRootBeforeReboot(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	systemUUID, mac := matrixIdentity("unmounts-root-before-reboot")
	machineID := uuid.New()
	const slug = "unmounts-root-before-reboot"
	env.Hub.Push(slug, env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:            machineID,
			MAC:           mac,
			State:         model.MachineStateDeploying,
			PXEEligible:   true,
			SystemUUID:    systemUUID,
			StorageScript: &model.SnapshotStorageScript{Name: "raid1", Content: raid1StorageScript},
		}).
		AddOperation(qemutest.NewOperation(machineID, model.OpDeploy)).
		Build())
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
		DiskSize:   "6G",
		ExtraDisks: []qemutest.ExtraDisk{{Type: qemutest.DiskVirtio, Size: "6G"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm.Start(ctx)

	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for deploy complete: %v\n%s", err, serialDump(vm))
	}
	if vm.Serial.Contains(`"msg":"umount failed"`) {
		t.Fatalf("the agent reported deploy complete with a filesystem it could not unmount:\n%s\n%s\n"+
			"earlier, with its error discarded:\n%s",
			grepSerial(vm, `"msg":"umount failed"`),
			grepSerial(vm, "target is busy"),
			grepSerial(vm, `"argv":"umount /target/sys"`))
	}
}
