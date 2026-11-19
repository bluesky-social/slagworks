package storageapply

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// WipeStaleState removes RAID superblocks and filesystem signatures
// from the disks the layout targets, so a prior install can't leave a
// fossil array that the kernel auto-assembles at the next boot. Order
// matters:
//
//  1. Stop every currently-assembled md array. A target disk whose
//     partition is an active md member can't be wiped while the array
//     holds it open.
//  2. For each target disk, zero the md superblock on the whole disk
//     and on each existing partition (mdadm stores 1.2 metadata inside
//     the member, which sgdisk --zap-all does not touch), then wipefs
//     the disk to clear any residual fs/partition-table magic.
//
// Best-effort throughout: every step is expected to fail on a disk that
// has no array/superblock/signature (that's the common clean-disk case),
// so errors are logged and execution continues. The subsequent
// sgdisk/mdadm --create commands from the plan are the authoritative
// writers; this is purely about removing state that would otherwise
// resurrect itself.
func WipeStaleState(ctx *agent.Context, layout storage.Layout) {
	ctx.Log.Info("--destructive: clearing stale RAID/filesystem state on target disks")

	// Stop any assembled arrays first so member partitions are released.
	if err := ctx.Exec.Run("mdadm", "--stop", "--scan"); err != nil {
		ctx.Log.Warn("mdadm --stop --scan (no arrays to stop is expected)", "err", err)
	}
	// `--stop --scan` does not reliably tear down EXTERNAL-metadata
	// containers (DDF / Intel IMSM): these are monitored by mdmon and a
	// member subarray must be stopped before its container, which --scan's
	// single pass can miss. The Dell R640 oob-gws ship with a factory DDF
	// container (md126 = subarray, md127 = the ddf0 container) that
	// auto-assembles at initrd start and holds the boot SAS disk OPEN — so
	// even after we repartition it, `blockdev --rereadpt` fails with
	// EBUSY (BLKRRPART "Device or resource busy"). Explicitly stop every
	// remaining md device, subarrays before containers, so the disk is
	// released before the plan partitions it. (Hit live on
	// oob-gw-3.pop2.)
	StopResidualMDDevices(ctx)

	for name, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		diskPath := "/dev/" + name
		// Zero the superblock on each existing partition AND the whole
		// disk. We discover live partitions rather than trusting the
		// layout, because the layout describes what we will create, not
		// what is already there.
		for _, target := range append(LivePartitions(ctx, name), diskPath) {
			if err := ctx.Exec.Run("mdadm", "--zero-superblock", "--force", target); err != nil {
				// Overwhelmingly "no superblock" on a clean target.
				ctx.Log.Info("mdadm --zero-superblock (no superblock is expected on a clean target)",
					"target", target, "err", err)
			}
		}
		if err := ctx.Exec.Run("wipefs", "-a", diskPath); err != nil {
			ctx.Log.Info("wipefs (no signatures is expected on a clean disk)",
				"disk", diskPath, "err", err)
		}
	}
}

// mdDevice is one md device parsed from /proc/mdstat.
//
// /proc/mdstat lines look like:
//
//	md126 : inactive sda[0]
//	      937164800 blocks super external:/md127/0
//	md127 : inactive sda[0](S)
//	      527704 blocks super external:ddf
//
// The "mdNNN :" line names the device; the following indented line carries
// the "super external:..." marker. A device whose super is "external:ddf"
// or "external:imsm" (no leading slash) is a CONTAINER; "external:/md127/0"
// (leading slash → references a container) is a subarray member.
type mdDevice struct {
	name        string
	isContainer bool // external-metadata container (DDF/IMSM), not a member subarray
}

// parseMdstat extracts md devices from /proc/mdstat contents, classifying
// each as a container (external:ddf / external:imsm — no leading slash) or
// a member subarray (external:/mdNNN/X). Pure function for testability.
func parseMdstat(out string) []mdDevice {
	var devs []mdDevice
	var cur *mdDevice
	for line := range strings.SplitSeq(out, "\n") {
		if name, _, ok := strings.Cut(line, " : "); ok && strings.HasPrefix(name, "md") {
			devs = append(devs, mdDevice{name: strings.TrimSpace(name)})
			cur = &devs[len(devs)-1]
			continue
		}
		// Continuation line: classify the current device. A container's
		// super metadata is "external:ddf" / "external:imsm" (no slash);
		// a member subarray is "external:/mdNNN/X".
		if cur != nil && strings.Contains(line, "super external:") {
			marker := line[strings.Index(line, "super external:")+len("super external:"):]
			if !strings.HasPrefix(strings.TrimSpace(marker), "/") {
				cur.isContainer = true
			}
		}
	}
	return devs
}

// StopResidualMDDevices force-stops every md device still present after
// `mdadm --stop --scan`, member/subarrays BEFORE containers. This is the
// teardown `--scan` misses for EXTERNAL-metadata containers (DDF / Intel
// IMSM): mdmon holds the container open, and a container can only be
// stopped once its member subarrays are gone — so a single --scan pass
// leaves a factory DDF container (e.g. the Dell R640's md127=ddf0 +
// md126=subarray) assembled and holding the boot disk, which later makes
// `blockdev --rereadpt` fail EBUSY. We read /proc/mdstat, classify each
// device by whether its line carries the "external:" metadata marker, and
// stop non-containers first. Best-effort: every step is expected to no-op
// on a host with no md state; errors are logged, not fatal.
func StopResidualMDDevices(ctx *agent.Context) {
	out, err := ctx.Exec.Output("cat", "/proc/mdstat")
	if err != nil {
		ctx.Log.Info("read /proc/mdstat (none is fine)", "err", err)
		return
	}
	devs := parseMdstat(out)
	stop := func(name string) {
		dev := "/dev/" + name
		if err := ctx.Exec.Run("mdadm", "--stop", dev); err != nil {
			ctx.Log.Info("mdadm --stop residual md (best-effort)", "dev", dev, "err", err)
		}
	}
	// Members/subarrays first so containers become idle, then containers.
	for _, d := range devs {
		if !d.isContainer {
			stop(d.name)
		}
	}
	for _, d := range devs {
		if d.isContainer {
			stop(d.name)
		}
	}
}

// WipeBystanderDisks signature-wipes every whole disk the layout does
// NOT reference: md superblocks zeroed on the disk and each live
// partition, then wipefs -a to clear fs/partition-table magic. No
// secure erase, no data overwrite — just enough that nothing on the
// disk auto-assembles or mounts again. This is the --wipe-all-disks
// behavior; the target disks are excluded because WipeStaleState
// already handled them and the layout plan is about to repartition
// them anyway.
//
// Runs after WipeStaleState, which has already issued
// `mdadm --stop --scan`, so arrays spanning bystander disks (the pop1
// case: a fossil boot-md whose phantom member was a U.2 data disk) are
// stopped and their members wiped here.
//
// Failure semantics are split per step. mdadm --zero-superblock exits
// 1 on any device that has no md superblock ("Unrecognised md
// component device") — the common clean-disk case — so its errors are
// logged and skipped, same as WipeStaleState. wipefs -a IS fatal on
// error (returned to the caller, who aborts the deploy): it exits 0 on
// a signature-less disk, it is the step that actually renders the disk
// unmountable, and it backstops a real mdadm failure (erasing the
// partition table hides any fossil superblock inside a former
// partition from auto-assembly). The operator explicitly asked for
// every bystander disk to end up unmountable; a disk whose signatures
// survive must fail the deploy.
func WipeBystanderDisks(ctx *agent.Context, layout storage.Layout) error {
	bystanders, err := bystanderDisks(ctx, layout)
	if err != nil {
		return err
	}
	if len(bystanders) == 0 {
		ctx.Log.Info("--wipe-all-disks: no bystander disks found")
		return nil
	}
	ctx.Log.Info(fmt.Sprintf("--wipe-all-disks: signature-wiping %d bystander disk(s): %s",
		len(bystanders), strings.Join(bystanders, " ")))
	for _, disk := range bystanders {
		diskPath := "/dev/" + disk
		for _, target := range append(LivePartitions(ctx, disk), diskPath) {
			if err := ctx.Exec.Run("mdadm", "--zero-superblock", "--force", target); err != nil {
				ctx.Log.Info("wipe-all-disks: mdadm --zero-superblock (no superblock is expected on a clean target)",
					"target", target, "err", err)
			}
		}
		if err := ctx.Exec.Run("wipefs", "-a", diskPath); err != nil {
			return fmt.Errorf("wipe-all-disks: wipefs %s: %w", diskPath, err)
		}
	}
	return nil
}

// bystanderDisks returns the kernel names (e.g. "nvme2n1") of all
// whole disks NOT referenced by the layout. Enumeration failures are
// errors: if we can't list disks we can't honor --wipe-all-disks, and
// silently wiping nothing would let the stale-md bug class through.
func bystanderDisks(ctx *agent.Context, layout storage.Layout) ([]string, error) {
	out, err := ctx.Exec.Output("lsblk", "-dnlo", "NAME,TYPE")
	if err != nil {
		return nil, fmt.Errorf("wipe-all-disks: lsblk: %w", err)
	}
	return parseBystanderDisks(out, layout), nil
}

// parseBystanderDisks is the testable selector behind bystanderDisks:
// given `lsblk -dnlo NAME,TYPE` output, return every whole disk the
// layout does not claim.
func parseBystanderDisks(lsblkOut string, layout storage.Layout) []string {
	claimed := layoutClaimedDisks(layout)
	var disks []string
	for line := range strings.SplitSeq(lsblkOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "disk" {
			continue
		}
		name := fields[0]
		if claimed[name] {
			continue // layout target or raid/preserve member's parent disk
		}
		disks = append(disks, name)
	}
	return disks
}

// layoutClaimedDisks returns the set of every whole-disk kernel name (bare,
// no "/dev/" prefix) claimed by ANY device in the layout: a disk or retained
// disk's own name, or the parent whole disk of every raid/preserve member.
// This is the exclusion set for --wipe-all-disks. Retained disks and preserve
// members are protected data; raid members are about to be (re)assembled.
func layoutClaimedDisks(layout storage.Layout) map[string]bool {
	claimed := make(map[string]bool, len(layout.Devices))
	for name, dev := range layout.Devices {
		if dev.Type == "disk" || dev.Type == "retain" {
			claimed[name] = true
		}
	}
	for _, dev := range layout.Devices {
		if dev.Type != "raid" && dev.Type != "preserve" {
			continue
		}
		for _, member := range dev.Members {
			claimed[parentDisk(member)] = true
		}
	}
	return claimed
}

// wholeDiskFamilies matches the disk-name shape of the nvme/mmcblk/loop/
// dm-/md families, whose partition marker is an explicit "p<N>" suffix
// (nvme0n1p2, mmcblk0p1) rather than a bare trailing digit -- mirrors
// internal/agent/bootloader/bootloader.go's nvmeStyleRE (SplitDiskPart).
const wholeDiskFamilies = `nvme\d+n\d+|mmcblk\d+|loop\d+|dm-\d+|md\d+`

// wholeDiskDigitSuffix matches a bare whole-disk name from one of
// wholeDiskFamilies (nvme0n1, mmcblk0, loop0, dm-0, md0): its OWN trailing
// digit is part of the disk's identity (an nvme namespace number, etc.),
// not a partition marker, so parentDisk must return it unchanged.
var wholeDiskDigitSuffix = regexp.MustCompile(`^(?:` + wholeDiskFamilies + `)$`)

// partitionSuffix matches a "p<digits>" partition marker on one of
// wholeDiskFamilies specifically (nvme0n1p2 -> nvme0n1; mmcblk0p1 ->
// mmcblk0). The prefix is anchored to the family shape, NOT a generic
// ".+", so an sd-family disk that happens to end in the letter "p" (a
// legitimate kernel name: sda..sdz, sdaa, ..., sdp) is never misparsed --
// e.g. member "sdp1" must resolve to parent disk "sdp" via bareDigitSuffix
// below, not "sd" via a greedy generic p<N> strip.
var partitionSuffix = regexp.MustCompile(`^(` + wholeDiskFamilies + `)p\d+$`)

// bareDigitSuffix matches a trailing run of digits directly on an
// all-letters prefix -- the sd/vd family's partition marker (sda1 -> sda,
// vdb3 -> vdb, sdp1 -> sdp). Only reached after the two family-anchored
// checks above have ruled out nvme/mmcblk/loop/dm/md, so it never
// misparses e.g. "loop0"/"md0" (whole disks) as partitioned disks named
// "loo"/"md", nor "sdp1" (sd-family partition) as parent "sd".
var bareDigitSuffix = regexp.MustCompile(`^([A-Za-z]+)\d+$`)

// parentDisk returns the whole-disk kernel name that owns member, which may
// already be a whole disk (returned unchanged) or a partition of one. It
// inverts internal/storage/plan.go's partName across three checks, in
// order: (1) a name matching one of the nvme/mmcblk/loop/dm/md whole-disk
// shapes is returned unchanged (nvme0n1, mmcblk0, loop0, dm-0, md0 are
// whole disks, not partitions); (2) failing that, a "p<N>" suffix on one
// of those SAME families strips to its prefix (nvme0n1p2 -> nvme0n1;
// mmcblk0p1 -> mmcblk0); (3) failing that, a bare trailing digit run on an
// all-letters prefix is the sd/vd family's partition marker (sda1 -> sda,
// sdp1 -> sdp). Anchoring step 2 to the family shape (rather than a
// generic ".+p\d+$") is what keeps it from misparsing "loop0" (whole disk,
// contains a literal "p") or "sdp1" (sd-family disk name ending in "p") --
// mirrors internal/agent/bootloader/bootloader.go's SplitDiskPart, which
// validates the prefix against the same whole-disk shape before accepting
// a "p" as a partition delimiter.
func parentDisk(member string) string {
	name := strings.TrimPrefix(member, "/dev/")
	if wholeDiskDigitSuffix.MatchString(name) {
		return name
	}
	if m := partitionSuffix.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	if m := bareDigitSuffix.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// LivePartitions returns the /dev paths of the existing partitions of a
// whole disk (e.g. nvme2n1 -> [/dev/nvme2n1p1 /dev/nvme2n1p2]) as the
// kernel currently sees them. Best-effort: returns nil on any lsblk
// error or if the disk has no partitions.
func LivePartitions(ctx *agent.Context, disk string) []string {
	out, err := ctx.Exec.Output("lsblk", "-nlo", "NAME", "/dev/"+disk)
	if err != nil {
		return nil
	}
	var parts []string
	for line := range strings.FieldsSeq(out) {
		if line == disk {
			continue // the whole-disk entry itself
		}
		parts = append(parts, "/dev/"+line)
	}
	return parts
}

// CheckDisksClean verifies that none of the disks in the layout contain
// existing filesystem or partition table signatures. This prevents
// accidentally wiping a machine that already has an OS installed.
func CheckDisksClean(ctx *agent.Context, layout storage.Layout) error {
	for name, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		diskPath := "/dev/" + name
		// blkid exits 2 on a signature-less (clean) disk, so err != nil
		// is the clean case and err == nil with output is the dirty one.
		out, err := ctx.Exec.Output("blkid", "-p", diskPath)
		if err == nil && out != "" {
			return fmt.Errorf("disk %s has existing data (%s); refusing to partition -- this machine may have an active OS",
				diskPath, out)
		}
		// blkid exit code 2 = no signatures found (clean disk)
		ctx.Log.Info("disk is clean", "disk", diskPath)
	}
	return nil
}

// CleanupMounts unmounts every layout mount under /target before reboot,
// deepest first, so no mount is attempted while another still sits beneath
// it; the root, /target itself, goes last. Depth ties are broken lexically
// so the order never depends on map iteration.
//
// A mount that will not unmount is an error the caller must fail the deploy
// on: rebooting with a filesystem still mounted leaves it dirty, and a
// bootloader that does not replay a journal (GRUB reading an xfs root) then
// cannot find its own files. umount refuses a mount with anything still
// mounted beneath it, so a successful unmount of /target also proves no
// helper left a mount (a chroot bind, a tmpfs) behind.
func CleanupMounts(ctx *agent.Context, layout storage.Layout) error {
	ctx.Log.Info("unmounting target filesystems")

	paths := make([]string, 0, len(layout.Mounts))
	for mp := range layout.Mounts {
		paths = append(paths, path.Join("/target", mp))
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di > dj
		}
		return paths[i] < paths[j]
	})

	for _, mp := range paths {
		if err := ctx.Exec.Run("umount", mp); err != nil {
			return fmt.Errorf("umount %s: %w; rebooting with it mounted would leave the filesystem dirty, "+
				"so the deploy stops here. Something beneath %s is still mounted or in use: "+
				"inspect it from the recovery shell (findmnt -R %s) and redeploy", mp, err, mp, mp)
		}
	}
	return nil
}
