//go:build integration && proptest_findings

package qemutest_test

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags integration,proptest_findings ./internal/qemutest/ \
//	    -run TestDeploy_SafetyCheckRefusesConflictingSignatures

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeploy_SafetyCheckRefusesConflictingSignatures deploys WITHOUT
// --destructive onto a disk that holds an ext4 filesystem and, on top of it,
// the labels of a ZFS pool. `blkid -p`, which the safety check relies on,
// prints nothing for such a disk because its safe-probe mode refuses to
// choose between two signatures, so the agent logs "disk is clean" and
// partitions it. The check must refuse, exactly as it does for the same disk
// carrying ext4 alone (the uefi-single-dirty matrix cell).
func TestDeploy_SafetyCheckRefusesConflictingSignatures(t *testing.T) {
	t.Parallel()

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	// ext4 over the whole device, then ZFS uberblocks in labels 0 and 1.
	// `zpool create` leaves the first 8 KiB of a label alone, so on a real
	// disk the ext4 superblock at offset 1024 survives the same way.
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.raw")
	disk := filepath.Join(dir, "disk.qcow2")
	f, err := os.Create(raw)
	if err != nil {
		t.Fatalf("create raw disk: %v", err)
	}
	if err := f.Truncate(8 << 30); err != nil {
		t.Fatalf("size raw disk: %v", err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", raw).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	uberblock := make([]byte, 8)
	binary.LittleEndian.PutUint64(uberblock, 0x00bab10c)
	for label := int64(0); label < 2; label++ {
		for i := int64(0); i < 16; i++ {
			if _, err := f.WriteAt(uberblock, label*256*1024+128*1024+i*1024); err != nil {
				t.Fatalf("write uberblock: %v", err)
			}
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close raw disk: %v", err)
	}

	// Rigging self-check: the disk verifiably holds a filesystem. wipefs
	// lists every signature; if it does not show ext4 the test proves
	// nothing about the safety check.
	sigs, err := exec.Command("wipefs", raw).CombinedOutput()
	if err != nil || !strings.Contains(string(sigs), "ext4") || !strings.Contains(string(sigs), "zfs_member") {
		t.Fatalf("seeded disk does not carry both ext4 and zfs_member (err=%v):\n%s", err, sigs)
	}
	if out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", raw, disk).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}

	systemUUID, mac := matrixIdentity("safety-conflicting-signatures")
	machineID := uuid.New()
	const slug = "safety-conflicting-signatures"

	// No params: this deploy is NOT destructive.
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	snap := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:            machineID,
			MAC:           mac,
			State:         model.MachineStateDeploying,
			PXEEligible:   true,
			SystemUUID:    systemUUID,
			StorageScript: &model.SnapshotStorageScript{Name: "default", Content: defaultStorageScript},
		}).
		AddOperation(deployOp).
		Build()
	env.Hub.Push(slug, snap)
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
		ExtraDisks:    []qemutest.ExtraDisk{{Type: qemutest.DiskVirtio, Path: disk}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	vm.Start(ctx)

	// The run ends one of two ways. Watch for both so the wrong one fails
	// at once instead of after the whole budget.
	for deadline := time.Now().Add(qemutest.Budgets.FATALMarker); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if vm.Serial.Contains("FATAL during safety check") {
			return
		}
		if vm.Serial.Contains(`"argv":"sgdisk --zap-all /dev/vda"`) {
			t.Fatalf("the agent wiped the partition table of a disk that holds an ext4 filesystem, "+
				"on a deploy without --destructive; the safety check said:\n%s",
				grepSerial(vm, "disk is clean"))
		}
	}
	t.Fatalf("neither the safety check FATAL nor a partitioning command appeared within %s\n%s",
		qemutest.Budgets.FATALMarker, serialDump(vm))
}
