//go:build integration

package qemutest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestMatrix is the table-driven full-stack deploy lifecycle test
// across (firmware, storage, failure-mode) tuples. Each cell:
//
//  1. Boots a VM via PXE under the named firmware.
//  2. Drives the deploy agent to "deploy complete".
//  3. Optionally injects a failure mode. Two flavors:
//     a. Hard failure (dirty-disk): the agent should abort with a
//     FATAL marker; "deploy complete" should NOT appear.
//     b. Recoverable failure (missing-efivarfs): the agent should
//     detect and remediate, then complete normally — proves
//     self-healing behavior we depend on in production.
//  4. For success cells: pushes the awaiting_phone_home snapshot,
//     waits for the deployed system to boot from disk and POST
//     phone_home.
//
// This subsumes:
//
//   - TestDeploy / TestDeploy_StorageScriptRAID1 (single-firmware,
//     "deploy complete" smoke; replaced by clean cells of every
//     firmware/storage cross-product, with phone_home as the
//     end-to-end assertion).
//   - TestDeployFullLifecycle_UEFI / TestDeployFullLifecycle_UEFI_RAID1
//     (single-firmware lifecycle; replaced by the matching matrix
//     cells).
//   - TestDeploy_RefusesNonBlankDisk (single dirty-disk cell;
//     replaced by failureMode=dirtyDisk cells under both firmwares).
//   - TestDeploy_IdentityMismatch (still standalone; the matrix
//     focuses on storage/firmware/efivarfs, identity-mismatch is a
//     different axis).
//
// Group 1: 7 cells (acceptance bar is >= 8; we squeak in
// with the missing-efivarfs cell included). Group 2 follow-ups
// (network-drop, mixed-vendor, SecureBoot, RAID10, disk-yank) ship
// in a follow-up PR.
//
// Runtime: deploy_complete lands in 54-81s per cell at -parallel 32
// (2026-07-04 full-suite data); clean cells add a reboot + phone-home
// tail (~10-30s). The whole matrix runs in roughly the time of the
// slowest single cell.
func TestMatrix(t *testing.T) {
	cells := []matrixCell{
		{
			name:            "uefi-single-clean",
			firmware:        qemutest.UEFI,
			storage:         storageSingle,
			failure:         failureNone,
			expectPhoneHome: true,
		},
		{
			name:            "uefi-raid1-clean",
			firmware:        qemutest.UEFI,
			storage:         storageRAID1,
			failure:         failureNone,
			expectPhoneHome: true,
		},
		{
			name:            "uefi-raid1-missing-efivarfs",
			firmware:        qemutest.UEFI,
			storage:         storageRAID1,
			failure:         failureMissingEfivarfs,
			expectPhoneHome: true,
			// efivarfs recovery cell: the BOOT hook honors
			// `slagworks.efivarfs=skip` and does NOT mount efivarfs,
			// reproducing the pop1 H13 boot state where the kernel
			// comes up with /sys/firmware/efi populated but
			// /sys/firmware/efi/efivars unmounted. The agent's
			// registerEFIBootEntry then calls ensureEfivarfs() which
			// probes /proc/mounts, modprobes the module, and mounts
			// efivarfs at /sys/firmware/efi/efivars before efibootmgr
			// runs. Deploy must complete cleanly and the host must
			// phone home post-reboot — anything less is a regression
			// in the efivarfs fix and would silently re-introduce the
			// pop1 ship-blocker.
		},
		{
			name:             "uefi-single-dirty",
			firmware:         qemutest.UEFI,
			storage:          storageSingle,
			failure:          failureDirtyDisk,
			expectAgentFatal: "FATAL during safety check",
		},
		{
			// Reproduces the pop1-kworker bug: the site worker serves
			// no storage script (204) for this machine. The agent must
			// HARD-FAIL at layout resolution rather than silently
			// falling back to a default "first disk wins" layout that
			// would install the OS onto an arbitrary (often wrong) disk.
			// Two disks present so a silent fallback would have a real
			// chance to pick the wrong one — the failure must be loud,
			// not a coin flip.
			name:             "uefi-no-storage-script",
			firmware:         qemutest.UEFI,
			storage:          storageNone,
			failure:          failureNone,
			expectAgentFatal: "FATAL during resolving storage layout",
		},
		{
			name:             "bios-single-dirty",
			firmware:         qemutest.BIOS,
			storage:          storageSingle,
			failure:          failureDirtyDisk,
			expectAgentFatal: "FATAL during safety check",
		},
		{
			// Legacy/BIOS deploy installs GRUB i386-pc into
			// the disk's bios_grub (EF02) partition. The storage
			// scripts are hybrid (bios_grub + ESP + root); the agent
			// picks the bootloader matching the host firmware via
			// isUEFI(). Asserts the BIOS clean path now reaches
			// phone_home post-reboot, which it could not before the
			// legacy installGRUBBIOS path landed.
			name:            "bios-single-clean",
			firmware:        qemutest.BIOS,
			storage:         storageSingle,
			failure:         failureNone,
			expectPhoneHome: true,
		},
		{
			// RAID1 root on BIOS. GRUB is installed on BOTH
			// member disks' bios_grub partitions so the array stays
			// bootable if either disk is lost.
			name:            "bios-raid1-clean",
			firmware:        qemutest.BIOS,
			storage:         storageRAID1,
			failure:         failureNone,
			expectPhoneHome: true,
		},
	}

	// Concurrency cap removed (was maxParallel=2). Background:
	// It was originally capped at 2 because 4+ post-deploy
	// VMs star-wedged on networking — all cells reached
	// `deploy complete`, rebooted, then hung at first-boot Ubuntu
	// before phone-home reached the hub. Root cause was cloud-init
	// fallback netplan racing apt-daily.timer with no working
	// network (deployed system never had a netplan config, relied
	// on cloud-init's coincidental fallback).
	//
	// writeNetplan + disableFirstBootApt shipped
	// in this PR fix the underlying issue: the deployed system now
	// has a deterministic /etc/netplan/99-slagworks.yaml from the
	// agent, and apt-daily/unattended-upgrades are masked. Phone-home
	// arrives in ~10s flat with no contention. The cap is gone; let
	// the matrix run as wide as `go test -parallel` allows.
	for _, c := range cells {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runMatrixCell(t, c)
		})
	}
}

// matrixCell is one row in the TestMatrix table.
//
// Exactly one of expectPhoneHome and expectAgentFatal must be set.
// expectPhoneHome=true cells assert the deploy ran cleanly AND the
// post-install reboot succeeded (phone_home observation arrived).
// expectAgentFatal=<marker> cells assert the agent failed loud
// during the deploy initrd run, with `<marker>` appearing on the
// serial console.
type matrixCell struct {
	name             string
	firmware         qemutest.Firmware
	storage          storageShape
	failure          failureMode
	expectPhoneHome  bool
	expectAgentFatal string
	// skipReason, when non-empty, t.Skips the cell. Used to keep
	// known-broken cells visible in the table (matrix as living
	// documentation of coverage gaps) without burning CI time on
	// guaranteed failures. Pair every skipReason with a Linear
	// ticket — once the ticket lands, the skip goes away.
	skipReason string
}

type storageShape int

const (
	storageSingle storageShape = iota
	storageRAID1
	// storageNone means the site worker serves NO storage script for
	// the machine (204). Used to assert the agent hard-fails at layout
	// resolution instead of guessing an install target.
	storageNone
)

type failureMode int

const (
	failureNone failureMode = iota
	failureDirtyDisk
	failureMissingEfivarfs
)

func runMatrixCell(t *testing.T, c matrixCell) {
	t.Helper()

	if c.skipReason != "" {
		t.Skip(c.skipReason)
	}

	// Invariant: exactly one of expectPhoneHome / expectAgentFatal
	// is set. A cell that forgets expectAgentFatal would silently
	// take the success path and time out at 18 min waiting for
	// "deploy complete" — confusing failure for what's really a
	// table-author bug. Catch it up front.
	if c.expectPhoneHome == (c.expectAgentFatal != "") {
		t.Fatalf("cell %q: exactly one of expectPhoneHome (=%v) and expectAgentFatal (=%q) must be set",
			c.name, c.expectPhoneHome, c.expectAgentFatal)
	}

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	// Per-cell unique systemUUID and MAC. Derive from the cell
	// name's hash so a parallel re-run produces stable values.
	systemUUID, mac := matrixIdentity(c.name)
	machineID := uuid.New()
	slug := "matrix-" + c.name

	// snapshotScript is nil for storageNone (site worker serves 204),
	// otherwise the chosen script. Reused across both snapshot pushes
	// so init and awaiting carry identical storage selection.
	snapshotScript := snapshotStorageScript(c.storage)

	// Build the deploy op's params: include test knobs as needed.
	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	if c.failure == failureMissingEfivarfs {
		raw, err := json.Marshal(map[string]any{"efivarfs": "skip"})
		if err != nil {
			t.Fatalf("marshal deploy params: %v", err)
		}
		deployOp.Params = raw
	}

	snapInit := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:            machineID,
			MAC:           mac,
			State:         model.MachineStateDeploying,
			PXEEligible:   true,
			SystemUUID:    systemUUID,
			StorageScript: snapshotScript,
		}).
		AddOperation(deployOp).
		Build()
	env.Hub.Push(slug, snapInit)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vmCfg := qemutest.VMConfig{
		Firmware:   c.firmware,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	}
	if c.storage == storageRAID1 || c.storage == storageNone {
		// RAID1 wants two equally-sized disks. storageNone also uses
		// two disks so a (buggy) silent "first disk wins" fallback
		// would have a real chance to pick the wrong one — proving the
		// hard-fail isn't just masking a single-disk happy path. Use 6G
		// each so total VM disk usage matches the RAID1 lifecycle test.
		vmCfg.DiskSize = "6G"
		vmCfg.ExtraDisks = []qemutest.ExtraDisk{
			{Type: qemutest.DiskVirtio, Size: "6G"},
		}
	}
	if c.failure == failureDirtyDisk {
		vmCfg.PreFormatDisk = true
	}
	vm := env.NewVM(t, vmCfg)

	// Generous outer safety net; the REAL timeouts are the per-marker
	// budgets (Budgets.DeployComplete etc.) and the budgeted
	// observation waits — this only catches a wedge in un-budgeted
	// glue. Observed full-cell time at -parallel 32: ~100-130s.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	vm.Start(ctx)

	if c.expectAgentFatal != "" {
		// Failure cell: the agent should hit the named FATAL
		// marker. The test fails if we either miss the marker
		// (timeout) or see "deploy complete" (which would mean
		// the failure didn't actually trigger). The agent logs slogkit
		// JSON envelopes to stderr (captured on the serial console), so
		// the marker is the envelope's msg substring, e.g.
		// {"...","msg":"deploy complete",...}.
		if err := vm.Serial.WaitFor(ctx, c.expectAgentFatal, qemutest.Budgets.FATALMarker); err != nil {
			t.Fatalf("waiting for failure marker %q: %v", c.expectAgentFatal, err)
		}
		if vm.Serial.Contains(`"msg":"deploy complete"`) {
			t.Errorf("expected agent to FATAL on %q, but saw 'deploy complete' first", c.failure.String())
		}
		return
	}

	// Success cell: drive through deploy + post-install reboot +
	// phone-home (the lifecycle pattern — the
	// minimum bar for a deploy test that actually proves the
	// deployed system boots).
	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for deploy complete: %v", err)
	}

	// Flip phase to awaiting_phone_home so the post-install PXE
	// returns BootLocal. Pushed IMMEDIATELY on the serial marker —
	// BEFORE waiting for the deploy_complete observation — because
	// the marker means the agent is about to reboot, and the reboot's
	// PXE re-fetch races this snapshot apply: if the fetch wins, the
	// boot handler serves the DEPLOY script again and the second pass
	// FATALs on the now-dirty disk (these ops carry no destructive
	// param). Pushing at the earliest possible moment minimizes that
	// window; the worker dedups by SeqNum so an early push is safe.
	deployOpAwaiting := deployOp
	deployOpAwaiting.Status = model.OpClaimed
	deployOpAwaiting.Phase = "awaiting_phone_home"

	snapAwaiting := env.NewSnapshot(slug, network).
		Seq(2).
		AddMachine(qemutest.Machine{
			ID:            machineID,
			MAC:           mac,
			State:         model.MachineStateDeploying,
			PXEEligible:   true,
			SystemUUID:    systemUUID,
			StorageScript: snapshotScript,
		}).
		AddOperation(deployOpAwaiting).
		Build()
	env.Hub.Push(slug, snapAwaiting)

	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}

	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("waiting for phone_home (post-install reboot failed): %v\n"+
			"common causes: (1) iPXE boot-local script can't reach the ESP "+
			"via UEFI SimpleFileSystemProtocol (Supermicro firmware quirk; "+
			"sanboot vs `chain file:///`); (2) bootloader install missed disk "+
			"(see efibootmgr coverage); (3) deployed Ubuntu didn't reach "+
			"firstboot service; (4) for RAID1 cells, UEFI firmware reading "+
			"FAT off a raid1 member depends on md metadata 1.0 placement; "+
			"(5) the awaiting_phone_home snapshot apply LOST the race with "+
			"the reboot's PXE re-fetch — look for a SECOND deploy pass "+
			"FATALing on the dirty disk in the serial log.",
			err)
	}
}

func (f failureMode) String() string {
	switch f {
	case failureNone:
		return "none"
	case failureDirtyDisk:
		return "dirty-disk"
	case failureMissingEfivarfs:
		return "missing-efivarfs"
	default:
		return fmt.Sprintf("unknown(%d)", int(f))
	}
}

// matrixIdentity derives a stable per-cell SystemUUID and MAC. The
// MAC's last octet is built from the first byte of the cell name so
// distinct cells running concurrently never collide on the bridge
// (each cell's Env+Network is isolated, but unique MACs make
// collision-cause-of-failure debugging easier).
//
// We hand-build rather than uuid.NewSHA1 because go's testing
// harness shows the cell name in failures already; a stable UUID
// per cell across reruns helps when correlating with hub
// observation logs.
func matrixIdentity(name string) (systemUUID, mac string) {
	// Hash the cell name to a 16-byte UUID-shaped value. Use
	// uuid.NewSHA1 against a fixed namespace so the result is
	// deterministic and well-formed.
	ns := uuid.NameSpaceDNS
	systemUUID = uuid.NewSHA1(ns, []byte("slagworks-matrix-cell:"+name)).String()
	// MAC: 52:54:00 (QEMU/libvirt OUI) + 3 bytes derived from the
	// name. Hash the name and take the first 3 bytes.
	macHash := uuid.NewSHA1(ns, []byte("slagworks-matrix-cell-mac:"+name))
	mac = fmt.Sprintf("52:54:00:%02x:%02x:%02x", macHash[10], macHash[14], macHash[15])
	return systemUUID, mac
}

// matrixStorage describes a storage layout for the matrix.
type matrixStorage struct {
	name    string
	content string
}

func storageScriptFor(s storageShape) matrixStorage {
	switch s {
	case storageSingle:
		return matrixStorage{name: "default", content: defaultStorageScript}
	case storageRAID1:
		return matrixStorage{name: "raid1", content: raid1StorageScript}
	default:
		panic(fmt.Sprintf("unknown storage shape %d", int(s)))
	}
}

// snapshotStorageScript returns the per-machine storage script to embed
// in the snapshot, or nil for storageNone (the site worker then serves
// a 204, reproducing the no-script-matched condition).
func snapshotStorageScript(s storageShape) *model.SnapshotStorageScript {
	if s == storageNone {
		return nil
	}
	sc := storageScriptFor(s)
	return &model.SnapshotStorageScript{Name: sc.name, Content: sc.content}
}
