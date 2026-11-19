// Package storageapply realizes the storage plan on a deploy target's
// disks: inventory the block devices, resolve the per-machine storage
// layout (script-driven), wipe stale RAID/filesystem state, and run the
// planned partition/mkfs/mdadm commands.
package storageapply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// RunCommand executes one storage-plan command, with bounded retry for
// `blockdev --rereadpt`. Right after sgdisk repartitions a disk, systemd-udevd
// opens it to probe the new partitions; if BLKRRPART races that open it fails
// EBUSY ("Device or resource busy") and the deploy aborts in the storage phase.
// We can't `udevadm settle` it away because the agent runs in a chroot where
// settle no-ops ("Running in chroot, ignoring request"). A short backoff lets
// udev's probe finish and the disk free up (observed clearing within ~1-2s on
// the Dell R640 oob-gws). Only rereadpt is retried — every other
// storage command stays fail-fast so a real error isn't masked by retries.
//
// A failed `mdadm --create` is explained by raidCreateError; every other
// failure is returned as is.
func RunCommand(ctx *agent.Context, cmd storage.Command) error {
	if level, ok := raidCreateLevel(cmd); ok {
		if err := ctx.Exec.Run(cmd.Name, cmd.Args...); err != nil {
			return raidCreateError(ctx, cmd.Args[1], level, err)
		}
		return nil
	}
	isRereadpt := cmd.Name == "blockdev" && len(cmd.Args) >= 1 && cmd.Args[0] == "--rereadpt"
	if !isRereadpt {
		return ctx.Exec.Run(cmd.Name, cmd.Args...)
	}
	const maxAttempts = 15
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = ctx.Exec.Run(cmd.Name, cmd.Args...); err == nil {
			if attempt > 1 {
				ctx.Log.Info("blockdev --rereadpt succeeded after retry (udev settle race)",
					"attempts", attempt, "args", cmd.Args)
			}
			return nil
		}
		// EBUSY is the udev race; back off and retry. Other failures are
		// unlikely to clear on retry but the bounded loop is cheap and we
		// surface the last error if it never succeeds.
		time.Sleep(time.Second)
	}
	return fmt.Errorf("blockdev --rereadpt still failing after %d attempts: %w", maxAttempts, err)
}

// raidCreateLevel returns the RAID level of a planned
// `mdadm --create <md> --level=N ...` command, and false for any other
// command.
func raidCreateLevel(cmd storage.Command) (int, bool) {
	if cmd.Name != "mdadm" || len(cmd.Args) < 2 || cmd.Args[0] != "--create" {
		return 0, false
	}
	for _, a := range cmd.Args[2:] {
		if v, ok := strings.CutPrefix(a, "--level="); ok {
			level, err := strconv.Atoi(v)
			return level, err == nil
		}
	}
	return 0, false
}

// raidCreateError explains a failed `mdadm --create`. The way a level the
// planner accepts fails in practice is a boot image whose deploy initrd
// lacks the level's md personality module: the kernel loads it by running
// modprobe in the initramfs, not in the agent's chroot, and mdadm reports
// only "RUN_ARRAY failed: Invalid argument". The kernel log names the cause
// outright: md_run requests module md-level-N and, when no personality
// registers, logs "md: personality for level N is not loaded!". The
// Personalities line of /proc/mdstat cannot stand in for it, because
// personalities load lazily and a create that failed before RUN_ARRAY
// never asked for one. Either way the member disks are already
// repartitioned, so the retry needs --destructive.
func raidCreateError(ctx *agent.Context, mdDev string, level int, err error) error {
	const retry = "redeploy with --destructive: the member disks were already repartitioned and may hold partial md superblocks"
	module, known := storage.RAIDModule(level)
	if !known {
		// Plan rejects every level RAIDModule does not know.
		return fmt.Errorf("mdadm --create %s (RAID %d): %w", mdDev, level, err)
	}
	notLoaded := fmt.Sprintf("md: personality for level %d is not loaded", level)
	kernelLog, readErr := ctx.Exec.Output("dmesg")
	if readErr == nil && strings.Contains(kernelLog, notLoaded) {
		return fmt.Errorf("mdadm --create %s (RAID %d): %w; the kernel logged %q: it could not load module %s "+
			"from the boot image's deploy initrd. Rebuild the image with %s in assets/initramfs/hooks/slagworks, then %s",
			mdDev, level, err, notLoaded, module, module, retry)
	}
	unread := ""
	if readErr != nil {
		unread = fmt.Sprintf(" (the kernel log could not be read to check for a missing %s module: %v)", module, readErr)
	}
	return fmt.Errorf("mdadm --create %s (RAID %d): %w; mdadm's stderr in the exec log entry before this one names "+
		"the cause. Fix it, then %s%s", mdDev, level, err, retry, unread)
}

// Inventory returns the list of physical block devices suitable for
// installation. Skips loop, ram, and optical devices.
func Inventory(ctx *agent.Context) ([]storage.BlockDevice, error) {
	out, err := ctx.Exec.Output("lsblk", "-Jb", "-d", "-o", "NAME,SIZE,TRAN,MODEL,LOG-SEC")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}

	// Numeric columns are strings or numbers depending on the util-linux
	// version.
	var raw struct {
		Blockdevices []struct {
			Name   string `json:"name"`
			Size   any    `json:"size"`
			Tran   string `json:"tran"`
			Model  string `json:"model"`
			LogSec any    `json:"log-sec"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("lsblk parse: %w", err)
	}

	var disks []storage.BlockDevice
	for _, bd := range raw.Blockdevices {
		if strings.HasPrefix(bd.Name, "loop") || strings.HasPrefix(bd.Name, "ram") || strings.HasPrefix(bd.Name, "sr") {
			continue
		}
		disks = append(disks, storage.BlockDevice{
			Name:               bd.Name,
			SizeBytes:          lsblkInt(bd.Size),
			LogicalSectorBytes: lsblkInt(bd.LogSec),
			Transport:          bd.Tran,
			Model:              strings.TrimSpace(bd.Model),
		})
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no suitable disks found")
	}
	return disks, nil
}

// lsblkInt reads a numeric lsblk JSON column, which util-linux emits as a
// number or a string depending on its version. Zero means absent.
func lsblkInt(v any) int64 {
	var n int64
	switch v := v.(type) {
	case float64:
		n = int64(v)
	case string:
		fmt.Sscanf(v, "%d", &n)
	}
	return n
}

// ScriptFetcher fetches the per-machine storage script from the site
// worker. Returns nil/empty bytes for a 204 (no script matched at
// snapshot-build time), or an error if the fetch itself failed.
// Injected so ResolveLayout's decision logic is unit-testable
// without a live HTTP server.
type ScriptFetcher func(url string) ([]byte, error)

// ResolveLayout returns the storage layout to use for deploy.
// liveInventory supplies the live-hardware inventory JSON the storage
// script consumes; injected because inventory collection lives with the
// commission code in cmd/slagworks-agent, not here.
//
// There are two distinct paths with deliberately different failure
// semantics:
//
//   - machineID == "" (MAC-based bootstrap, no machine record yet):
//     fall back to a default single-disk layout. This path has no
//     access to per-machine storage-script selection, so a default
//     is the only option. It REFUSES multi-disk hosts rather than
//     guessing which disk is the OS target (see BootstrapDefaultLayout).
//
//   - machineID != "" (normal deploy): the site worker serves the
//     pre-chosen storage script. If it serves a 204 (no script matched
//     the machine's commission data) or the fetch errors, we HARD-FAIL.
//     We do NOT fall back to a default layout here: a machine with a
//     model-matched script in the catalog that silently gets a
//     "first disk wins" install is exactly the pop1-kworker bug
//     (OS landed on a U.2 data disk, wiping the intended boot RAID's
//     bare contract). A loud failure is recoverable; a wrong-disk
//     install is destructive and silent.
func ResolveLayout(ctx *agent.Context, machineID string, disks []storage.BlockDevice, fetch ScriptFetcher, liveInventory func() ([]byte, error)) (storage.Layout, error) {
	if machineID == "" {
		ctx.Log.Info("no machine ID available, using default storage layout")
		return BootstrapDefaultLayout(disks)
	}

	scriptURL := fmt.Sprintf("http://%s/api/v1/machines/%s/storage-script", ctx.Server, machineID)

	scriptContent, err := fetch(scriptURL)
	if err != nil {
		return storage.Layout{}, fmt.Errorf(
			"fetching storage script for machine %s: %w; refusing to guess an install target "+
				"(check the site worker is reachable at %s)", machineID, err, ctx.Server)
	}
	if len(scriptContent) == 0 {
		return storage.Layout{}, fmt.Errorf(
			"no storage script matched machine %s (site worker served no script); "+
				"refusing to guess an install target. This usually means the machine has no "+
				"commission data, or no script's match rules fit its hardware. Commission the "+
				"machine and confirm a storage script matches before deploying", machineID)
	}
	ctx.Log.Info("storage script available, collecting live inventory")

	// Re-run the inventory collector against live hardware at deploy
	// time. The script operates on current state (actual disk presence,
	// SMART data, udev topology), not a cached commission snapshot.
	// This matters because hardware can drift between commission time
	// and deploy time — drives swap, firmware updates land, etc.
	commData, err := liveInventory()
	if err != nil {
		return storage.Layout{}, fmt.Errorf("collect live inventory: %w", err)
	}

	// Write the script + live inventory to temp files and run it; the
	// shared harness injects SLAGWORKS_INVENTORY and returns stdout. Storage
	// scripts have no companion library, so no sidecars.
	layoutJSON, err := execkit.RunConfigScript(ctx.Procs, ctx.Log, "storage", scriptContent, "SLAGWORKS_INVENTORY", commData, nil)
	if err != nil {
		return storage.Layout{}, err
	}

	var layout storage.Layout
	if err := json.Unmarshal(layoutJSON, &layout); err != nil {
		return storage.Layout{}, fmt.Errorf("parse layout JSON from script: %w", err)
	}
	ctx.Log.Info("storage script produced layout")
	return layout, nil
}

// BootstrapDefaultLayout builds the single-disk default layout for the
// machineID == "" bootstrap path. It REFUSES to choose when more than
// one disk is present: silently picking disks[0] on a multi-NVMe host
// is how the pop1 kworkers landed their OS on a U.2 data disk
// (NVMe namespace numbering is not stable across boots, so "first disk"
// is non-deterministic and unsafe). With exactly one disk there is no
// ambiguity; with zero the caller already errored before reaching here.
func BootstrapDefaultLayout(disks []storage.BlockDevice) (storage.Layout, error) {
	switch len(disks) {
	case 0:
		return storage.Layout{}, fmt.Errorf("no disks available for default layout")
	case 1:
		return storage.DefaultLayout(disks[0].Name), nil
	default:
		names := make([]string, len(disks))
		for i, d := range disks {
			names[i] = d.Name
		}
		return storage.Layout{}, fmt.Errorf(
			"refusing to auto-select an install target among %d disks (%s): "+
				"the default single-disk layout is only safe on a single-disk host. "+
				"Commission the machine and assign a storage script that selects the "+
				"boot disk explicitly", len(disks), strings.Join(names, ", "))
	}
}

// SummarizeLayout renders a human-readable, log-friendly summary of the
// resolved storage layout and the exact ordered commands the agent is
// about to run. Pure (no I/O) so it's unit-testable and so the caller
// controls where the lines go. The mount lines resolve RAID names to
// their /dev/md* path exactly as Plan/execution will, so the logged
// root/ESP target is the real target.
func SummarizeLayout(layout storage.Layout, cmds []storage.Command) []string {
	var lines []string
	mountDevs := storage.ResolveMountDevices(layout)

	// Sort mount points for stable output (root first by depth).
	paths := make([]string, 0, len(mountDevs))
	for p := range mountDevs {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
	for _, p := range paths {
		lines = append(lines, fmt.Sprintf("layout: mount %s -> %s", p, mountDevs[p]))
	}

	lines = append(lines, fmt.Sprintf("layout: %d command(s) to execute:", len(cmds)))
	for i, c := range cmds {
		lines = append(lines, fmt.Sprintf("layout:   [%d] %s", i+1, c.String()))
	}
	return lines
}
