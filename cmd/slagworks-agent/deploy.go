package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/bootloader"
	"github.com/bluesky-social/slagworks/internal/agent/inventory"
	"github.com/bluesky-social/slagworks/internal/agent/netapply"
	"github.com/bluesky-social/slagworks/internal/agent/storageapply"
	"github.com/bluesky-social/slagworks/internal/agent/target"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// deployParams carries the flag-derived inputs plus the reporting
// closures main() wires up for the deploy path.
type deployParams struct {
	image        string
	machineID    string
	operationID  string
	expectUUID   string
	destructive  bool
	wipeAllDisks bool
	reporter     *agentReporter
	setPhase     func(string)
	fatal        func(string, error)
	done         chan struct{}
	// checkIdentity verifies this machine's SMBIOS system-uuid against
	// expectUUID (verifyIdentity in production). A field so tests can
	// drive runDeploy without dmidecode.
	checkIdentity func(expectUUID string) error
}

// runDeploy drives the deploy sequence: identity check, storage
// resolve/plan, network-config render, storage apply, root extraction,
// bootloader install, target finalization (fstab/cloud-init/network/
// hostname/keys), tailscale enrollment, target unmount, BMC reconcile,
// deploy_complete report, and reboot. Everything that can hard-fail on
// inputs known at start (the storage and network scripts) runs before the
// first disk is touched.
func runDeploy(ctx *agent.Context, p deployParams) {
	setPhase, fatal := p.setPhase, p.fatal

	// Verify machine identity before touching any disks
	if err := p.checkIdentity(p.expectUUID); err != nil {
		fatal("identity check", err)
	}
	ctx.Log.Info("identity verified (system-uuid matches)")

	disks, err := storageapply.Inventory(ctx)
	if err != nil {
		fatal("collecting disk inventory", err)
	}
	ctx.Log.Info(fmt.Sprintf("found %d disk(s), using %s", len(disks), disks[0].Name))

	setPhase("agent_starting")

	layout, err := storageapply.ResolveLayout(ctx, p.machineID, disks, agent.HTTPGet, func() ([]byte, error) {
		return json.Marshal(inventory.CollectGraceful(ctx.Exec, inventoryOptions()))
	})
	if err != nil {
		fatal("resolving storage layout", err)
	}
	cmds, err := storage.Plan(layout, disks)
	if err != nil {
		fatal("planning storage layout", err)
	}

	// Make the disk-target decision observable BEFORE we touch any
	// disk. On a multi-disk host an operator reading slagctl logs can
	// confirm the OS is going to the intended device (and the intended
	// commands will run) rather than discovering post-hoc that the
	// install landed on the wrong disk. This is the diagnostic that was
	// missing when the pop1 kworkers silently installed onto a U.2.
	for _, line := range storageapply.SummarizeLayout(layout, cmds) {
		ctx.Log.Info(line)
	}

	ctx.Log.Info(fmt.Sprintf("storage layout planned (%d commands)", len(cmds)))

	// Render the network config BEFORE any disk is touched. The network
	// script is a pure function of the NetBox interface facts, and it
	// hard-fails by design on an incomplete model (a missing bond, an
	// unmodeled /31 leg); run after the install, that failure left the host
	// wiped and half-deployed for a problem knowable up front. The rendered
	// bundle is written into /target in the finalizing phase.
	netCfg, err := netapply.RenderNetworkConfig(ctx, p.machineID)
	if err != nil {
		fatal("rendering network config", err)
	}

	if p.destructive {
		ctx.Log.Info("--destructive set, skipping disk safety check")
	} else if err := storageapply.CheckDisksClean(ctx, layout); err != nil {
		fatal("safety check", err)
	}

	// A raid that pins its array UUID is identified at boot by that UUID
	// alone, so a member superblock with the same UUID on a device this
	// deploy leaves unwritten (the disk a redeploy moved the array off)
	// races the real members and can stop the boot in emergency mode.
	// Refuse now, with the host as found, unless --wipe-all-disks will zero
	// that device below.
	if err := storageapply.CheckStaleArrayMembers(ctx, layout, p.wipeAllDisks); err != nil {
		fatal("stale array member check", err)
	}

	// Verify every preserve array by its expected UUID BEFORE any
	// destructive command runs. The destructive ordering the preserve
	// feature mandates is verify -> wipe -> create root -> reassemble ->
	// mount: discovering a typo/missing data disk only at Phase-2 assemble
	// time is a partial-apply disaster (boot disks already zapped, root
	// recreated, but /data can't be assembled -- on a feature sold as
	// preservation-safe). Assemble is non-destructive, so a member that
	// isn't a real member of the expected array FATALs here with the host
	// left exactly as found. This also closes Phase-1 roast finding #3
	// (a preserve member on an undeclared partition of a managed disk).
	if err := storageapply.PreflightPreserve(ctx, layout); err != nil {
		fatal("preserve preflight", err)
	}

	setPhase("partitioning")

	// Clear stale RAID superblocks and filesystem signatures from the
	// disks this layout will write, but ONLY under --destructive. A
	// fossil mdadm superblock left on a disk from a prior install causes
	// the kernel to auto-assemble a stray array at boot (observed on
	// pop1 kworkers l34-7/l34-15: a U.2 data disk got pulled into the
	// boot md as a phantom member). sgdisk --zap-all clears the GPT but
	// NOT the md superblock living inside the old partition, so we wipe
	// explicitly here. Gated on --destructive because it is, by design,
	// an unconditional state-delete; the non-destructive path relies on
	// CheckDisksClean refusing to touch a dirty disk at all.
	if p.destructive {
		storageapply.WipeStaleState(ctx, layout)
	}
	// Bystander-disk wipe runs after the target-disk wipe so every md
	// array is already stopped. The wrong-disk bug class this kills:
	// a previous imaging generation left md superblocks on disks the
	// current layout doesn't reference, the kernel auto-assembles them
	// at boot, and the new OS mounts (or installs onto) fossil state.
	if p.wipeAllDisks {
		if !p.destructive {
			fatal("flags", errors.New("--wipe-all-disks requires --destructive"))
		}
		if err := storageapply.WipeBystanderDisks(ctx, layout); err != nil {
			fatal("wipe-all-disks", err)
		}
	}

	// On the NON-destructive path WipeStaleState never ran, so nothing has
	// torn down a udev re-assembly of a preserved array since
	// PreflightPreserve stopped it -- and the stock `mdadm --incremental`
	// rules can grab it under a scan-chosen name (/dev/md127) from the stop's
	// member uevents, which would then EBUSY planPreserve's targeted
	// `--assemble /dev/mdN`. Release any such auto-assembly right before the
	// command loop. Guarded on layout having preserve devices AND the
	// destructive path NOT having already scan-stopped, so a plain deploy
	// never perturbs md state. Non-destructive (member data untouched).
	if !p.destructive && storageapply.HasPreserveDevices(layout) {
		storageapply.StopAutoAssembly(ctx)
	}

	// Split the plan at the mount-phase boundary so preserve arrays are
	// verified AFTER they are (re)assembled but BEFORE anything is mounted.
	// planMounts is the only source of mkdir/mount commands (planDisk /
	// planRAID / planPreserve never emit them), so the first mkdir-or-mount
	// is exactly where partition/assemble/mkfs ends and mounting begins.
	// Verifying before the mount matters: mounting a journaling filesystem
	// replays its journal (a WRITE), so a wrong/unexpected array must be
	// caught before mount, not after -- VerifyPreserveArrays reads the
	// /dev/mdN device directly (mdadm --detail + blkid), so it needs the
	// array assembled but NOT mounted.
	mountPhase := len(cmds)
	for i, cmd := range cmds {
		if cmd.Name == "mkdir" || cmd.Name == "mount" {
			mountPhase = i
			break
		}
	}
	for _, cmd := range cmds[:mountPhase] {
		if err := storageapply.RunCommand(ctx, cmd); err != nil {
			fatal("storage: "+cmd.Name, err)
		}
	}

	// The commands above ran planPreserve's --assemble for every preserve
	// array (WipeStaleState stopped the preflight-verified arrays via its
	// global `mdadm --stop --scan`, so this is the authoritative
	// re-assemble). Re-verify identity on the freshly re-assembled arrays
	// before any mount -- the preflight verified the pre-wipe assembly; this
	// confirms the re-assembly reconstructed the SAME array, not a stale
	// superblock resurrected by the intervening stop/scan.
	if err := storageapply.VerifyPreserveArrays(ctx, layout); err != nil {
		fatal("preserve verify", err)
	}

	for _, cmd := range cmds[mountPhase:] {
		if err := storageapply.RunCommand(ctx, cmd); err != nil {
			fatal("storage: "+cmd.Name, err)
		}
	}

	mountDevs := storage.ResolveMountDevices(layout)
	rootDev := mountDevs["/"]
	espDev := mountDevs["/boot/efi"] // empty if no ESP in layout

	setPhase("rsyncing_os")

	if err := target.MountAndExtract(ctx, layout); err != nil {
		fatal("extracting root filesystem", err)
	}

	setPhase("installing_bootloader")

	// Bootloader install is firmware-mode-dependent. The agent boots
	// through the same firmware the deployed OS will use (PXE chainload
	// happens in that firmware), so IsUEFI() at this point is an exact
	// read of the target's mode — no NetBox flag needed.
	//   - UEFI  -> systemd-boot into the ESP (espDev, from /boot/efi).
	//   - legacy -> GRUB i386-pc into each disk carrying a bios_grub
	//     (EF02) partition.
	// A deploy that "completes" with no bootloader leaves the host
	// unbootable but reports success, then reboots into nothing — the
	// exact silent ship-blocker class RegisterEFIBootEntry was hardened
	// against. So a missing bootloader target for the booted firmware
	// mode is a hard fail, not a skip.
	if bootloader.IsUEFI() {
		if espDev == "" {
			fatal("installing bootloader", errors.New("UEFI boot but layout has no /boot/efi (ESP) mount; refusing to deploy an unbootable host"))
		}
		if err := bootloader.InstallSystemdBoot(ctx, p.image, rootDev, espDev, p.machineID); err != nil {
			fatal("installing bootloader", err)
		}
	} else {
		biosDisks := storage.BIOSGrubDisks(layout)
		if len(biosDisks) == 0 {
			fatal("installing bootloader", errors.New("legacy/BIOS boot but layout has no bios_grub (EF02) partition; refusing to deploy an unbootable host"))
		}
		if err := bootloader.InstallGRUBBIOS(ctx, biosDisks, p.machineID); err != nil {
			fatal("installing bootloader", err)
		}
	}

	// The installed system's mdadm.conf decides which arrays its initramfs
	// assembles; a host whose initramfs lacks it can boot into an array the
	// layout does not name, or lose its root array to one. Hard fail.
	if err := target.ConfigureRAID(ctx, "/target", layout); err != nil {
		fatal("configuring RAID", err)
	}

	setPhase("finalizing")

	if err := target.WriteFstab(ctx, layout, mountDevs); err != nil {
		fatal("writing fstab", err)
	}

	if err := target.SeedCloudInit(ctx); err != nil {
		fatal("seeding cloud-init", err)
	}

	if err := netapply.ApplyNetworkConfig(ctx, netCfg); err != nil {
		fatal("applying network config", err)
	}

	if err := netapply.WriteHostname(ctx, p.machineID); err != nil {
		fatal("writing hostname", err)
	}

	if err := target.DisableFirstBootApt(ctx); err != nil {
		fatal("disabling first-boot apt timers", err)
	}

	if err := target.WriteSSHKeys(ctx); err != nil {
		ctx.Log.Warn("writing SSH keys", "err", err)
	}

	// Enroll onto the tailnet before reboot (if global minted a key for this
	// op — see slagworks.tailscale_authkey on the cmdline). Soft: a failure is
	// logged and the deploy proceeds; the host has a working OS and ansible
	// will enroll it on converge. The key never touches the target disk and is
	// never logged.
	if err := enrollTailscale(ctx, "/target"); err != nil {
		ctx.Log.Warn("tailscale enrollment", "err", err)
	}

	if p.machineID != "" && p.operationID != "" {
		if err := target.WritePhoneHomeService(ctx, p.machineID, p.operationID); err != nil {
			fatal("writing phone-home service", err)
		}
	}

	// TEST-ONLY: when slagworks.data_marker=<token> is on the cmdline, drop a
	// first-boot unit that writes+echoes /data/PRESERVE_MARKER so the
	// preserve-data qemu test can prove a preserved array survived. The token
	// is read straight from /proc/cmdline (visible in the chroot), the same
	// way enrollTailscale reads its auth key. Absent token => no-op, so
	// production deploys never install it.
	if token := kernelArgValue("slagworks.data_marker"); token != "" {
		if err := target.WriteDataMarkerService(ctx, token); err != nil {
			fatal("writing data-marker service", err)
		}
	}

	// Unmount the target last of all disk work, and before BMC reconcile
	// and the deploy_complete report: a filesystem that will not unmount
	// would reboot dirty, so it fails the deploy here, while the op can
	// still fail and before reconcile has rotated any BMC password that a
	// failed op would never report.
	if err := storageapply.CleanupMounts(ctx, layout); err != nil {
		fatal("unmounting target filesystems", err)
	}

	// Reconcile BMC users + persistent boot order. This is what gets
	// the `monitor` scrape account onto the BMC for the first time and
	// disables any stray account (including a `slagworks` account left
	// by an older agent). slagworksd itself drives the BMC with the
	// bmc-creds-admin account, which reconcile never touches, so a
	// failure here is soft (see below).
	//
	// Skipped entirely when reporter is nil (agent invoked manually
	// without --machine/--op for debugging). Running reconcile in
	// that mode would set a monitor password with no way to phone it
	// home. The reporter-required gate is the only safe shape.
	if p.reporter != nil {
		// An Intel AMT host (slagworks.oob=amt) has no IPMI BMC: skip
		// reconcile entirely and report deploy_complete with nil creds, the
		// same "nothing rotated" shape as a steady-state redeploy. The
		// lifecycle TUI renders the unvisited bmc_reconcile phase skipped.
		var bmcCreds *model.BMCCreds
		var err error
		if oobIsAMT() {
			ctx.Log.Info("BMC reconcile skipped: OOB endpoint is Intel AMT (slagworks.oob=amt)")
		} else {
			setPhase("bmc_reconcile")
			bmcCreds, err = runBMCReconcile(ctx)
		}
		if err != nil {
			// Some BMC firmware (Supermicro AMI on H13SSH boards
			// among them) refuses ipmitool's `user set password`
			// command entirely with "Request data field length
			// exceeded", regardless of payload size — the firmware
			// requires Redfish-based password rotation. Until the
			// agent learns to use Redfish here, treat reconcile
			// failure as a soft error and mark the op completed. The
			// host has a working OS on disk and is bootable; the cred
			// rotation is a hardening step that can run later
			// out-of-band.
			//
			// CRITICAL: do NOT nil bmcCreds here. A partial failure
			// may have written passwords to the BMC before erroring;
			// runBMCReconcile returns those alongside the error, and
			// dropping them would silently drift the live BMC from
			// slagworksd's stored creds (the exact saga provision-once
			// exists to prevent).
			if bmcCreds != nil {
				ctx.Log.Warn("BMC reconcile partially failed; reporting the creds that WERE written",
					"err", err)
			} else {
				ctx.Log.Warn("BMC reconcile failed before writing any creds; deploy proceeds with non-rotated creds",
					"err", err)
			}
		}

		// Single deploy_complete POST carries the BMCCreds payload AND
		// signals phase=awaiting_phone_home (the worker's
		// HandleProgressReport promotes the op phase on the event). One
		// round-trip eliminates the split-brain risk where the phase
		// landed but the creds were dropped, which on a rotation deploy
		// would permanently desync the BMC from slagworks's stored
		// passwords.
		if err := p.reporter.ReportDeployComplete(bmcCreds); err != nil {
			fatal("reporting deploy_complete", err)
		}
	}

	ctx.Log.Info("deploy complete")
	close(p.done)

	// Reboot into the freshly-installed OS. The boot-action's
	// "awaiting_phone_home → BootLocal" clause makes the
	// post-deploy PXE iteration sanboot from disk, the OS comes up,
	// the slagworks-phone-home.service unit POSTs to /api/v1/report,
	// and the deploy op completes (machine → running). Without this
	// reboot the host stays in initrd and no one ever phones home.
	//
	// Kernel cmdline `slagworks.no_reboot` disables this for ad-hoc
	// debugging (drop the operator into a shell at the end of deploy
	// without losing /target).
	if hasKernelArg("slagworks.no_reboot") {
		ctx.Log.Info("slagworks.no_reboot set, holding in initrd")
		select {} // park forever
	}
	rebootInto(ctx, "deploy")
}
