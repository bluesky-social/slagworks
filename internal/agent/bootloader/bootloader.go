// Package bootloader installs the deployed host's bootloader:
// systemd-boot into the ESP on UEFI hosts, legacy i386-pc GRUB on BIOS
// hosts, plus the UEFI Boot#### registration and efivarfs plumbing the
// AMI Aptio firmware family requires.
package bootloader

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// FetchKernelParams resolves the per-machine kernel cmdline string from
// the site worker. The source of truth is the NetBox device_type
// custom field; the site worker mirrors it via snapshot. A 204 (empty
// body) means the device-type's slagworks_kernel_params is unset, which
// is a hard fail (deploy refused).
//
// The value is trimmed and then refused if it still contains a control
// character. Both bootloader configs carry the kernel params on a single
// line (the systemd-boot entry's options line, and the linux line that
// update-grub writes into grub.cfg), so a line break would add a line of
// its own and a tab would not reach the kernel as written.
//
// Empty machineID is a development-only stdout-mode convenience; the
// production deploy path always sets it on the cmdline. Fall back to a
// sane default so manual debug deploys don't need a NetBox round-trip.
func FetchKernelParams(ctx *agent.Context, machineID string) (string, error) {
	if machineID == "" {
		return "console=ttyS1,115200n8", nil
	}
	kpURL := fmt.Sprintf("http://%s/api/v1/machines/%s/kernel-params", ctx.Server, machineID)
	kpBytes, err := agent.HTTPGet(kpURL)
	if err != nil {
		return "", fmt.Errorf("fetch kernel-params: %w", err)
	}
	if len(kpBytes) == 0 {
		return "", fmt.Errorf("kernel-params: device-type has no slagworks_kernel_params; refusing deploy")
	}
	kp := strings.TrimSpace(string(kpBytes))
	for i, r := range kp {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("kernel-params %q: control character %U at byte %d; refusing deploy. "+
				"Set the NetBox device-type custom field slagworks_kernel_params to one line of "+
				"space-separated kernel arguments", kp, r, i)
		}
	}
	return kp, nil
}

// download fetches a URL to a local path via wget, with retries.
func download(ctx *agent.Context, url, dest string) error {
	ctx.Log.Info("downloading", "url", url)
	if err := ctx.Exec.Run("wget", "-nv", "--tries=3", "--retry-connrefused", "--waitretry=2", "-O", dest, url); err != nil {
		return fmt.Errorf("wget %s: %w", url, err)
	}
	return nil
}

// InstallSystemdBoot installs systemd-boot into the ESP for a
// UEFI-booted host: the EFI binary at the removable-media fallback path
// AND EFI/systemd/, kernel + local initrd under ESP/slagworks/, a
// loader.conf + boot entry carrying the per-machine kernel cmdline, and
// a registered UEFI Boot#### variable (required on AMI Aptio firmware).
func InstallSystemdBoot(ctx *agent.Context, image, rootDev, espDev, machineID string) error {
	ctx.Log.Info("installing systemd-boot")

	espDir := "/target/boot/efi"
	baseURL := fmt.Sprintf("http://%s", ctx.Server)

	// Download systemd-boot EFI binary from worker
	efiTmp := "/tmp/systemd-bootx64.efi"
	if err := download(ctx, baseURL+"/files/bin/systemd-bootx64.efi", efiTmp); err != nil {
		return err
	}

	// ESP/EFI/BOOT/BOOTX64.EFI -- fallback path that UEFI firmware finds automatically
	bootDir := filepath.Join(espDir, "EFI", "BOOT")
	os.MkdirAll(bootDir, 0755)
	if err := ctx.Exec.Run("cp", efiTmp, filepath.Join(bootDir, "BOOTX64.EFI")); err != nil {
		return fmt.Errorf("copy systemd-boot: %w", err)
	}

	// Also install to ESP/EFI/systemd/ for bootctl status
	sdDir := filepath.Join(espDir, "EFI", "systemd")
	os.MkdirAll(sdDir, 0755)
	if err := ctx.Exec.Run("cp", efiTmp, filepath.Join(sdDir, "systemd-bootx64.efi")); err != nil {
		return fmt.Errorf("copy systemd-boot (systemd dir): %w", err)
	}

	// Download kernel + initrd from worker into ESP. Use initrd-local (the
	// stock Ubuntu initrd) rather than the PXE initrd -- the local initrd has
	// the standard init that handles root=UUID= and mdadm RAID assembly.
	kernelDir := filepath.Join(espDir, "slagworks")
	os.MkdirAll(kernelDir, 0755)
	if err := download(ctx, fmt.Sprintf("%s/files/images/%s/vmlinuz", baseURL, image), filepath.Join(kernelDir, "vmlinuz")); err != nil {
		return err
	}
	if err := download(ctx, fmt.Sprintf("%s/files/images/%s/initrd-local", baseURL, image), filepath.Join(kernelDir, "initrd.img")); err != nil {
		return err
	}

	// Get root partition UUID for kernel cmdline
	rootUUID, err := ctx.Exec.Output("blkid", "-s", "UUID", "-o", "value", rootDev)
	if err != nil {
		return fmt.Errorf("blkid root: %w", err)
	}

	// Write loader.conf
	loaderDir := filepath.Join(espDir, "loader")
	os.MkdirAll(loaderDir, 0755)
	loaderConf := "default ubuntu.conf\ntimeout 3\n"
	if err := os.WriteFile(filepath.Join(loaderDir, "loader.conf"), []byte(loaderConf), 0644); err != nil {
		return fmt.Errorf("write loader.conf: %w", err)
	}

	// Resolve the per-machine kernel cmdline. The "none"
	// sentinel signals "device has no SOL" — GenerateBootEntry drops
	// the console= entirely.
	kp, err := FetchKernelParams(ctx, machineID)
	if err != nil {
		return err
	}

	// Write boot entry
	entriesDir := filepath.Join(loaderDir, "entries")
	os.MkdirAll(entriesDir, 0755)
	if err := os.WriteFile(filepath.Join(entriesDir, "ubuntu.conf"), []byte(GenerateBootEntry(rootUUID, kp)), 0644); err != nil {
		return fmt.Errorf("write boot entry: %w", err)
	}

	// Register a UEFI Boot#### variable pointing at our bootloader.
	// Without this, AMI Aptio firmware on Supermicro AS-1115HS-TNR
	// won't boot from the ESP — it only honors disks referenced by
	// an active Boot#### entry, and the \EFI\BOOT\BOOTX64.EFI
	// removable-media fallback path is inconsistently followed for
	// fixed disks. Ubuntu's shim-install / grub-install does this
	// automatically when run from inside the OS; we do it here from
	// the deploy initrd because we never run grub-install (we ship
	// systemd-boot pre-built). BootNext is NOT set here: the deploy
	// arms it once deploy_complete has been accepted.
	if err := RegisterEFIBootEntry(ctx, espDev); err != nil {
		// Hard failure: AMI Aptio (Supermicro AS-1115HS-TNR / pop1
		// kworkers) refuses to boot from disk without an active
		// Boot#### entry. A silent Warn here is what hid the pop1
		// ship-blocker for so long — the deploy "completed" but the
		// host couldn't post-install boot. Promote to a real error
		// so the deploy fails fast and recovery sshd
		// becomes reachable.
		//
		// On UEFI firmwares that DO honor the \EFI\BOOT\BOOTX64.EFI
		// removable-media fallback this is a false positive — but
		// "false positive on a small slice of hardware" is far less
		// expensive than "silent regression on the dominant
		// hardware family." Once we have a per-model quirk table
		// (Supermicro wisdom doc §3) we can opt-in skip on the
		// known-good families.
		return fmt.Errorf("efibootmgr registration: %w (host will not boot from disk on AMI Aptio firmware; "+
			"check that efivarfs is mounted under /sys/firmware/efi/efivars)", err)
	}

	ctx.Log.Info("systemd-boot installed")
	return nil
}

// InstallGRUBBIOS installs legacy (i386-pc) GRUB onto the given disks
// for a BIOS-booted host. Unlike the UEFI path — which
// hand-writes a systemd-boot loader entry and ships a prebuilt EFI
// binary — this drives the deployed OS's OWN grub-pc tooling inside a
// chroot, so the menu, kernel discovery, and core.img all come from the
// distro's machinery (robust against kernel-version drift). The bind-
// mount + chroot pattern mirrors target.ConfigureRAID.
//
// disks are whole-disk paths (e.g. "/dev/sda"); GRUB's core.img lands
// in each disk's GPT bios_grub (EF02) partition. Installing on every
// such disk keeps a RAID1 root bootable if either disk is lost.
func InstallGRUBBIOS(ctx *agent.Context, disks []string, machineID string) error {
	ctx.Log.Info("installing legacy GRUB (i386-pc)", "disks", disks)

	// Resolve the per-machine kernel cmdline and bake it into
	// the target's /etc/default/grub so update-grub emits it for every
	// menu entry. "none" => device has no SOL, so no console= is added
	// (the deployed OS keeps grub's default tty0 console).
	kp, err := FetchKernelParams(ctx, machineID)
	if err != nil {
		return err
	}
	cmdline := ""
	if kp != "none" {
		cmdline = "console=tty0 " + kp
	}
	if err := SetGrubDefaultCmdline("/target/etc/default/grub", cmdline); err != nil {
		return fmt.Errorf("set GRUB cmdline: %w", err)
	}

	// Bind-mount pseudo-filesystems for chroot operations (same set
	// target.ConfigureRAID uses). grub-install needs /dev to probe the
	// disk; update-grub needs /proc + /sys. The cleanup defer is
	// registered BEFORE the loop and unmounts only what actually mounted
	// (in reverse), so a partial-mount failure mid-loop doesn't leak the
	// earlier mounts into /target.
	var mounted []string
	defer func() {
		for _, m := range slices.Backward(mounted) {
			_ = ctx.Exec.Run("umount", m)
		}
	}()
	for _, m := range []struct{ src, dst string }{
		{"/dev", "/target/dev"},
		{"/proc", "/target/proc"},
		{"/sys", "/target/sys"},
	} {
		if err := os.MkdirAll(m.dst, 0755); err != nil {
			return fmt.Errorf("create bind-mount target %s: %w", m.dst, err)
		}
		if err := ctx.Exec.Run("mount", "--bind", m.src, m.dst); err != nil {
			return fmt.Errorf("bind-mount %s: %w", m.dst, err)
		}
		mounted = append(mounted, m.dst)
	}

	if err := grubInstallOnDisks(ctx, "/target", disks); err != nil {
		return err
	}

	// Generate the menu (discovers the installed kernel + initrd, emits
	// root=UUID, applies GRUB_CMDLINE_LINUX from /etc/default/grub).
	if err := ctx.Exec.Run("chroot", "/target", "update-grub"); err != nil {
		return fmt.Errorf("update-grub: %w", err)
	}

	ctx.Log.Info("legacy GRUB installed")
	return nil
}

// grubBIOSModuleDir is where grub-install --target=i386-pc puts GRUB's
// modules, core.img and boot.img, relative to the target root.
const grubBIOSModuleDir = "boot/grub/i386-pc"

// grubInstallOnDisks runs the target's grub-install once per disk, with
// target as the chroot, and removes the module directory before each run.
//
// grub-install copies the same modules into /boot/grub/i386-pc every time.
// When it finds modules already there it renames each one to a "~" backup,
// copies the set again and deletes the backups. On an XFS root those
// deletions can empty a block in the middle of the directory; XFS frees the
// block and the directory keeps a hole there. GRUB 2.12 as packaged for
// Ubuntu 24.04 reads a hole as a block of entries for inode 0 and fails the
// lookup with "not a correct XFS inode", so every module listed after the
// hole is out of reach and the machine stops at "grub rescue>". With that
// package's modules the hole is there after three runs and after five. A
// directory that has only ever been filled has no holes, so every run gets
// one.
func grubInstallOnDisks(ctx *agent.Context, target string, disks []string) error {
	moduleDir := filepath.Join(target, grubBIOSModuleDir)
	for _, disk := range disks {
		if err := os.RemoveAll(moduleDir); err != nil {
			return fmt.Errorf("remove %s before grub-install on %s: %w; "+
				"check that the target root is mounted read-write and redeploy", moduleDir, disk, err)
		}
		// --target=i386-pc forces legacy BIOS GRUB even though the disk
		// is GPT (grub-install would otherwise guess from the running
		// firmware, which inside the deploy initrd is itself BIOS here,
		// but being explicit is the contract). --no-floppy skips the
		// slow nonexistent-floppy probe on these headless boxes.
		if err := ctx.Exec.Run("chroot", target, "grub-install",
			"--target=i386-pc", "--no-floppy", "--recheck", disk); err != nil {
			return fmt.Errorf("grub-install on %s: %w", disk, err)
		}
	}
	return nil
}

// SetGrubDefaultCmdline sets GRUB_CMDLINE_LINUX in the target's
// /etc/default/grub to exactly cmdline. If a GRUB_CMDLINE_LINUX line
// already exists it's replaced; otherwise the line is appended. The
// stock noble file ships GRUB_CMDLINE_LINUX="" so the replace branch is
// the common case.
//
// The file is a shell fragment that update-grub sources as root, so the
// value is single-quoted, with each embedded single quote written as
// close-quote, backslash-quote, reopen-quote. The shell then reads it back
// byte for byte and expands nothing. Double quotes would leave $, ` and \
// live.
func SetGrubDefaultCmdline(path, cmdline string) error {
	// gosec G304/G703: path is a parameter but the sole production
	// caller passes the constant /target/etc/default/grub (tests pass a
	// t.TempDir path). Not attacker-controlled.
	line := "GRUB_CMDLINE_LINUX='" + strings.ReplaceAll(cmdline, "'", `'\''`) + "'"
	existing, err := os.ReadFile(path) //nolint:gosec // constant path in prod
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// No file (unexpected on a noble target, but don't fail the
		// deploy over it) — create a minimal one.
		return os.WriteFile(path, []byte(line+"\n"), 0644) //nolint:gosec // constant path in prod
	}
	lines := strings.Split(string(existing), "\n")
	replaced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "GRUB_CMDLINE_LINUX=") {
			lines[i] = line
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	// gosec G703 flags path-traversal because path is a parameter; the
	// sole production caller passes the constant /target/etc/default/grub
	// (tests pass a t.TempDir path). Not attacker-controlled.
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644) //nolint:gosec // constant path in prod
}

// RegisterEFIBootEntry creates a UEFI Boot#### variable pointing at
// the bootloader. Idempotent across redeploys: drops any existing
// "Slagworks" entries first to avoid pile-up in BootOrder.
//
// It does not set BootNext. Registering the entry is part of installing
// the bootloader and happens long before the deploy is complete; BootNext
// commits the machine's next boot to the installed OS, so it is set only
// once the site worker has accepted deploy_complete (SetBootNextToInstalledOS).
func RegisterEFIBootEntry(ctx *agent.Context, espDev string) error {
	if !IsUEFI() {
		ctx.Log.Info("not booted in UEFI mode, skipping efibootmgr")
		return nil
	}
	return registerEFIBootEntry(ctx, espDev)
}

// registerEFIBootEntry is RegisterEFIBootEntry on a host known to be UEFI.
func registerEFIBootEntry(ctx *agent.Context, espDev string) error {
	// efivarfs is required for efibootmgr to read/write
	// EFI variables. On AMI Aptio firmware (pop1 H13 hosts) the
	// kernel comes up with /sys/firmware/efi populated but
	// /sys/firmware/efi/efivars NOT mounted, and efibootmgr emits
	// "EFI variables are not supported on this system" then exits 2.
	// Probe and auto-mount before trying efibootmgr.
	if err := EnsureEfivarfs(ctx); err != nil {
		// Log but proceed: efibootmgr's own error is the signal we
		// want operators to read. EnsureEfivarfs only returns an
		// error when EVERY mount attempt failed; in that case we
		// expect efibootmgr to immediately error too, with a
		// clearer message.
		ctx.Log.Warn("efivarfs probe failed; proceeding into efibootmgr anyway", "err", err)
	}

	disk, partNum, err := SplitDiskPart(espDev)
	if err != nil {
		return fmt.Errorf("parse esp device %q: %w", espDev, err)
	}

	// Drop any pre-existing Slagworks entries. Match on the LABEL field
	// being exactly "Slagworks" (fields[1]), not a substring of the whole
	// line: an unrelated entry whose device PATH happens to contain
	// "Slagworks" must not be deleted. (Same exact-label rule as
	// parseSlagworksBootNum.)
	out, lerr := ctx.Exec.Output("efibootmgr")
	if lerr == nil {
		for line := range strings.SplitSeq(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.HasPrefix(fields[0], "Boot") {
				continue
			}
			if fields[1] != "Slagworks" {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(fields[0], "Boot"), "*")
			if len(id) != 4 {
				continue
			}
			_ = ctx.Exec.Run("efibootmgr", "-b", id, "-B")
		}
	}

	// `-c` creates a new entry and prepends it to BootOrder.
	return ctx.Exec.Run("efibootmgr",
		"-c",
		"-d", disk,
		"-p", fmt.Sprint(partNum),
		"-L", "Slagworks",
		"-l", `\EFI\BOOT\BOOTX64.EFI`,
	)
}

// SetBootNextToInstalledOS sets BootNext to the "Slagworks" entry so the
// IMMEDIATE next boot goes to the installed disk. On the AMI Aptio firmware
// the pop1 H13/RS501A hosts run, the OS-set BootOrder does NOT stick across
// the post-deploy reboot: the firmware network-boots first regardless of the
// order `-c` prepended, so the box re-PXEs (observed on kworker-l34-2,
// 2026-06-01). BootNext is a one-shot override AMI honors reliably, so it
// boots the freshly-installed OS once; from there the OS owns its own boot
// and phone-home completes.
//
// The firmware honors BootNext ahead of everything else, including the
// one-shot PXE override the site worker arms on the BMC before it power
// cycles a host for its next operation. That is why the caller sets it only
// after the site worker has accepted deploy_complete, immediately before the
// reboot. Set while the bootloader was being installed, it stayed armed in
// NVRAM through the rest of the deploy, so a deploy that was aborted, failed
// or timed out after that point left the host's next boot committed to the
// unfinished install: the next deploy's power cycle booted that disk instead
// of PXE and the new operation waited out its timeout for an agent that
// never came.
//
// Best-effort: without BootNext the host PXE-boots and the awaiting_phone_home
// latch sends it to its disk, or the firmware follows BootOrder to the
// registered entry. No-op on a BIOS host.
func SetBootNextToInstalledOS(ctx *agent.Context) {
	if !IsUEFI() {
		return
	}
	setBootNextToInstalledOS(ctx)
}

// setBootNextToInstalledOS is SetBootNextToInstalledOS on a host known to be
// UEFI.
func setBootNextToInstalledOS(ctx *agent.Context) {
	id, err := slagworksBootNum(ctx)
	if err != nil {
		ctx.Log.Warn("could not resolve Slagworks boot entry for BootNext; relying on the firmware boot order", "err", err)
		return
	}
	if err := ctx.Exec.Run("efibootmgr", "-n", id); err != nil {
		ctx.Log.Warn("efibootmgr -n (BootNext) failed; relying on the firmware boot order", "boot_id", id, "err", err)
		return
	}
	ctx.Log.Info("set BootNext (Slagworks), next boot is the installed OS", "boot_id", id)
}

// slagworksBootNum re-reads efibootmgr and returns the 4-hex-digit boot
// number of the (single) "Slagworks" entry. RegisterEFIBootEntry drops
// stale Slagworks entries before `-c`, so exactly one should exist;
// returns an error if zero or more than one is found (ambiguous → don't
// guess a BootNext target).
func slagworksBootNum(ctx *agent.Context) (string, error) {
	out, err := ctx.Exec.Output("efibootmgr")
	if err != nil {
		return "", fmt.Errorf("read efibootmgr: %w", err)
	}
	return parseSlagworksBootNum(out)
}

// parseSlagworksBootNum is the pure parser behind slagworksBootNum: it
// extracts the 4-hex-digit boot number of the single "Slagworks" entry
// from efibootmgr output. Returns an error on zero or >1 matches so we
// never guess an ambiguous BootNext target. Split out for unit testing.
func parseSlagworksBootNum(efibootmgrOut string) (string, error) {
	var ids []string
	for line := range strings.SplitSeq(efibootmgrOut, "\n") {
		// Match the entry line, e.g. "Boot0007* Slagworks  HD(...)".
		// Guard on the field AFTER the BootNNNN token being exactly
		// "Slagworks" so a device path that merely contains the word
		// (or a "BootOrder:"/"BootNext:" header line) can't match.
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "Boot") {
			continue
		}
		if fields[1] != "Slagworks" {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(fields[0], "Boot"), "*")
		if len(id) == 4 {
			ids = append(ids, id)
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("no Slagworks boot entry found after create")
	default:
		return "", fmt.Errorf("ambiguous: %d Slagworks boot entries found", len(ids))
	}
}

// efivarfsPath is the canonical mount point for the efivarfs
// pseudo-filesystem, where efibootmgr (and libefivar) read and write
// UEFI variables. Constant to keep tests honest.
const efivarfsPath = "/sys/firmware/efi/efivars"

// EnsureEfivarfs probes efivarfs's mount state and mounts it if
// missing. Idempotent: a no-op when already mounted with non-empty
// contents. Logs each probe step + outcome so operators reading
// slagctl logs see exactly what state the agent observed before it
// tried efibootmgr.
//
// Failure modes seen on real hardware:
//
//   - /sys/firmware/efi missing entirely → host wasn't UEFI-booted.
//     Caller (RegisterEFIBootEntry) gates on IsUEFI() before us; we
//     return nil so the caller's gate stays the source of truth.
//   - /sys/firmware/efi present, /sys/firmware/efi/efivars dir
//     present but empty (no mount) → AMI Aptio H13 with stock Ubuntu
//     deploy initrd. modprobe efivarfs + mount it.
//   - /sys/firmware/efi/efivars dir missing → some firmware versions
//     don't expose the directory. mkdir then mount.
//   - mount fails with "no such device" → kernel doesn't have the
//     efivarfs module compiled in or available. modprobe and retry.
//
// On every observable transition, log Info; on every error path,
// log Warn with the specific stage. Returns the last error if
// nothing worked; nil if efivarfs is observed mounted by the end.
func EnsureEfivarfs(ctx *agent.Context) error {
	// Stage 1: /sys/firmware/efi sanity. Caller already gated on
	// IsUEFI but log explicitly so the slagctl logs reflect the
	// actual filesystem state at this exact moment.
	if _, err := os.Stat("/sys/firmware/efi"); err != nil {
		ctx.Log.Warn("efivarfs probe: /sys/firmware/efi missing", "err", err)
		return fmt.Errorf("/sys/firmware/efi: %w", err)
	}
	ctx.Log.Info("efivarfs probe: /sys/firmware/efi present")

	// Stage 2: efivarfs already mounted? Detect via /proc/mounts —
	// stat alone can lie because the directory exists either way.
	mounted, err := efivarfsMounted()
	if err != nil {
		ctx.Log.Warn("efivarfs probe: read /proc/mounts failed", "err", err)
		// Fall through; the worst case is we try to mount when
		// we're already mounted, which is harmless (EBUSY).
	}
	if mounted {
		ctx.Log.Info("efivarfs probe: already mounted",
			"path", efivarfsPath)
		return nil
	}
	ctx.Log.Info("efivarfs probe: not mounted, attempting mount", "path", efivarfsPath)

	// Stage 3: ensure the mount-point directory exists. Some
	// firmware/kernel combos don't pre-create it.
	if _, err := os.Stat(efivarfsPath); err != nil {
		if os.IsNotExist(err) {
			ctx.Log.Info("efivarfs probe: mount point missing, creating", "path", efivarfsPath)
			if err := os.MkdirAll(efivarfsPath, 0755); err != nil {
				ctx.Log.Warn("efivarfs probe: mkdir mount point failed", "err", err)
				return fmt.Errorf("mkdir %s: %w", efivarfsPath, err)
			}
		} else {
			ctx.Log.Warn("efivarfs probe: stat mount point failed", "err", err)
		}
	}

	// Stage 4: load the module. Already-loaded → no-op success.
	// Missing → modprobe pulls it in. Compiled-in → modprobe still
	// returns 0 because the module name maps to the built-in.
	if err := ctx.Exec.Run("modprobe", "efivarfs"); err != nil {
		ctx.Log.Warn("efivarfs probe: modprobe efivarfs failed (continuing)", "err", err)
		// Don't return — the kernel may have efivarfs built-in,
		// in which case modprobe complains but mount still works.
	}

	// Stage 5: mount it. Linux signals via syscall.Mount or via
	// the mount(8) binary; we use the binary because it matches the
	// rest of the agent's exec idioms.
	if err := ctx.Exec.Run("mount", "-t", "efivarfs", "efivarfs", efivarfsPath); err != nil {
		ctx.Log.Warn("efivarfs probe: mount failed", "err", err)
		return fmt.Errorf("mount efivarfs: %w", err)
	}

	// Stage 6: verify. A mount that "succeeded" but produced no
	// entries means we're on firmware that doesn't expose runtime
	// services for variables — efibootmgr will still fail, just
	// with a different error. Surface this so the operator's not
	// guessing.
	entries, err := os.ReadDir(efivarfsPath)
	if err != nil {
		ctx.Log.Warn("efivarfs probe: post-mount readdir failed", "err", err)
		return fmt.Errorf("readdir %s: %w", efivarfsPath, err)
	}
	ctx.Log.Info("efivarfs probe: mount complete",
		"path", efivarfsPath,
		"entries", len(entries),
	)
	if len(entries) == 0 {
		// Not a hard failure here — the caller's efibootmgr will
		// produce its own diagnostic. But log loud so operators
		// know mount-without-entries is a distinct shape.
		ctx.Log.Warn("efivarfs probe: mounted but no variable entries; firmware may not expose UEFI runtime services")
	}
	return nil
}

// efivarfsMounted parses /proc/mounts and returns true if any line
// mounts efivarfs (regardless of mount point — the canonical path is
// efivarfsPath but a previously-mounted-elsewhere setup is still
// fine for our purposes).
func efivarfsMounted() (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	return mountsContainsEfivarfs(string(data)), nil
}

// mountsContainsEfivarfs is the pure parser behind efivarfsMounted,
// split out for unit testing.
func mountsContainsEfivarfs(mounts string) bool {
	for line := range strings.SplitSeq(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// /proc/mounts columns: source mount-point fstype opts
		if fields[2] == "efivarfs" {
			return true
		}
	}
	return false
}

// IsUEFI returns true when the kernel was booted via UEFI (i.e.
// /sys/firmware/efi exists). Used to gate efibootmgr operations.
func IsUEFI() bool {
	_, err := os.Stat("/sys/firmware/efi")
	return err == nil
}

// nvmeStyleRE matches whole-disk paths whose disk name itself ends
// in a digit (so partitions need an explicit "p" delimiter).
var nvmeStyleRE = regexp.MustCompile(`(nvme\d+n\d+|mmcblk\d+|loop\d+|dm-\d+|md\d+)$`)

// SplitDiskPart turns "/dev/nvme0n1p1" into ("/dev/nvme0n1", 1).
// Handles both "pN" suffix style (NVMe, mmcblk, loop, dm, md) and
// bare digit suffix style (sd*, vd*, hd*).
func SplitDiskPart(part string) (string, int, error) {
	if part == "" {
		return "", 0, fmt.Errorf("empty path")
	}
	// Reject whole-disk paths upfront (no partition encoded).
	if nvmeStyleRE.MatchString(part) {
		return "", 0, fmt.Errorf("no partition suffix in %q", part)
	}
	// nvme-style with partition: ".../<disk>p<N>". The "p" must be
	// preceded by a digit AND the disk prefix must itself match the
	// nvme-style whole-disk regex.
	for i := len(part) - 1; i >= 1; i-- {
		if part[i] != 'p' {
			continue
		}
		if part[i-1] < '0' || part[i-1] > '9' {
			break
		}
		if !nvmeStyleRE.MatchString(part[:i]) {
			break
		}
		num, err := strconv.Atoi(part[i+1:])
		if err == nil && i+1 < len(part) {
			return part[:i], num, nil
		}
		break
	}
	// sd*/vd*/hd*-style: disk name ends in a letter, partition is
	// trailing digits.
	if part[len(part)-1] < '0' || part[len(part)-1] > '9' {
		return "", 0, fmt.Errorf("no partition suffix in %q", part)
	}
	for i := len(part) - 1; i >= 0; i-- {
		if part[i] < '0' || part[i] > '9' {
			num, err := strconv.Atoi(part[i+1:])
			if err != nil {
				return "", 0, err
			}
			return part[:i+1], num, nil
		}
	}
	return "", 0, fmt.Errorf("could not split %q", part)
}

// GenerateBootEntry renders the systemd-boot loader entry for the
// installed OS. kernelParams comes from the site worker's
// /api/v1/machines/<id>/kernel-params endpoint, which mirrors
// MachineSnapshot.KernelParams (sourced from NetBox device-type
// custom field). Sentinel "none" -> no console= written. FetchKernelParams
// refuses an empty value and any control character before it gets here,
// so kernelParams cannot add a line to the entry.
func GenerateBootEntry(rootUUID, kernelParams string) string {
	cmdlineExtra := ""
	if kernelParams != "none" {
		cmdlineExtra = " console=tty0 " + kernelParams
	}
	return fmt.Sprintf("title   Ubuntu\nlinux   /slagworks/vmlinuz\ninitrd  /slagworks/initrd.img\noptions root=UUID=%s ro%s\n",
		rootUUID, cmdlineExtra)
}
