package storageapply

import (
	"errors"
	"fmt"
	"os/exec"
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
	status      string // the rest of the device's line: "active raid1 sdb[1] sda[0]"
	isContainer bool   // external-metadata container (DDF/IMSM), not a member subarray
}

// parseMdstat extracts md devices from /proc/mdstat contents, classifying
// each as a container (external:ddf / external:imsm — no leading slash) or
// a member subarray (external:/mdNNN/X). Pure function for testability.
func parseMdstat(out string) []mdDevice {
	var devs []mdDevice
	var cur *mdDevice
	for line := range strings.SplitSeq(out, "\n") {
		if name, status, ok := strings.Cut(line, " : "); ok && strings.HasPrefix(name, "md") {
			devs = append(devs, mdDevice{name: strings.TrimSpace(name), status: strings.TrimSpace(status)})
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
	claimed, err := wipeAllDisksExclusions(ctx, layout)
	if err != nil {
		return nil, fmt.Errorf("wipe-all-disks: %w; refusing to wipe any disk", err)
	}
	return parseBystanderDisks(out, claimed), nil
}

// wipeAllDisksExclusions returns the whole disks --wipe-all-disks leaves
// alone: layoutClaimedDisks plus every disk that holds a preserve member.
// The disks holding preserve members come from the kernel (memberDisks),
// not from the member names, because a member may be spelled as a
// /dev/disk/by-* path or with a driver's own partition naming. A member
// the kernel cannot place is an error: guessing wrong destroys the data
// the layout preserves. bystanderDisks and CheckStaleArrayMembers both use
// this set, so the stale-member check examines exactly the disks the
// bystander wipe will not zero.
func wipeAllDisksExclusions(ctx *agent.Context, layout storage.Layout) (map[string]bool, error) {
	claimed := layoutClaimedDisks(layout)
	for _, name := range preserveDeviceNames(layout) {
		for _, member := range layout.Devices[name].Members {
			disks, err := memberDisks(ctx, member)
			if err != nil {
				return nil, fmt.Errorf("cannot tell which disk holds preserve %q member %q: %w", name, member, err)
			}
			for _, disk := range disks {
				claimed[disk] = true
			}
		}
	}
	return claimed, nil
}

// parseBystanderDisks is the testable selector behind bystanderDisks:
// given `lsblk -dnlo NAME,TYPE` output, return every whole disk not in
// claimed.
func parseBystanderDisks(lsblkOut string, claimed map[string]bool) []string {
	var disks []string
	for _, name := range parseWholeDisks(lsblkOut) {
		if claimed[name] {
			continue // layout target, raid member's parent disk, or holds a preserve member
		}
		disks = append(disks, name)
	}
	return disks
}

// parseWholeDisks returns the kernel name of every whole disk in
// `lsblk -dnlo NAME,TYPE` output, in lsblk's order.
func parseWholeDisks(lsblkOut string) []string {
	var disks []string
	for line := range strings.SplitSeq(lsblkOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "disk" {
			continue
		}
		disks = append(disks, fields[0])
	}
	return disks
}

// layoutClaimedDisks returns the set of every whole-disk kernel name (bare,
// no "/dev/" prefix) the layout claims by name: a disk or retained disk's own
// name, or the parent whole disk of every raid member. With the disks that
// hold preserve members, this is the exclusion set for
// --wipe-all-disks (wipeAllDisksExclusions). Retained disks are protected
// data; raid members are about to be (re)assembled. Raid members are layout names (validate requires
// it) and may not exist yet, so they are parsed rather than looked up.
func layoutClaimedDisks(layout storage.Layout) map[string]bool {
	claimed := make(map[string]bool, len(layout.Devices))
	for name, dev := range layout.Devices {
		if dev.Type == "disk" || dev.Type == "retain" {
			claimed[name] = true
		}
	}
	for _, dev := range layout.Devices {
		if dev.Type != "raid" {
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
// already be a whole disk (returned unchanged) or a partition of one. It is
// for raid members, which are layout names; preserve members name existing
// devices and are resolved by the kernel instead (memberDisks). It
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
	parts, _ := listPartitions(ctx, disk)
	return parts
}

// listPartitions is LivePartitions with the lsblk error returned, for a
// check that must not treat a disk it could not list as clean.
func listPartitions(ctx *agent.Context, disk string) ([]string, error) {
	out, err := ctx.Exec.Output("lsblk", "-nlo", "NAME", "/dev/"+disk)
	if err != nil {
		return nil, err
	}
	var parts []string
	for line := range strings.FieldsSeq(out) {
		if line == disk {
			continue // the whole-disk entry itself
		}
		parts = append(parts, "/dev/"+line)
	}
	return parts, nil
}

// CheckDisksClean is the safety check of a deploy without --destructive:
// it refuses every layout disk that carries any signature (partition
// table, filesystem, RAID/LVM/ZFS member, LUKS header), so a deploy never
// partitions a machine that already holds an OS or data. A disk passes only
// when it is known to be blank; a probe that cannot run fails the check.
//
// The probe is wipefs, not `blkid -p`. In its safe-probe mode blkid prints
// nothing on stdout for a disk carrying two signatures it will not choose
// between (an ext4 disk later given to ZFS without a wipe), just as for a
// blank disk, and it exits 2 ("nothing found") for a blank disk and for one
// it could not open or read alike; a check that read any blkid failure as
// clean passed all of them. wipefs lists every signature libblkid
// recognizes, partition tables included (it also finds a GPT whose
// protective MBR is gone), prints nothing and exits 0 on a blank disk, and
// exits non-zero only when it cannot probe.
//
// Only the whole-disk node is probed. Any partition the kernel sees comes
// from a partition table, which wipefs reports on the whole disk; a
// filesystem left inside a partition whose table was erased has no device
// node at all, so no probe could reach it.
func CheckDisksClean(ctx *agent.Context, layout storage.Layout) error {
	for name, dev := range layout.Devices {
		if dev.Type != "disk" {
			continue
		}
		diskPath := "/dev/" + name
		out, err := ctx.Exec.Output("wipefs", "--noheadings", "--output", "TYPE,OFFSET", diskPath)
		if err != nil {
			return fmt.Errorf("cannot tell whether disk %s holds data: listing its signatures failed: %s; "+
				"refusing to partition it. Check that the disk is present and readable (lsblk, dmesg) and that "+
				"wipefs is in the deploy image, then redeploy; --destructive skips this check and overwrites the disk",
				diskPath, probeError(err))
		}
		if sigs := wipefsSignatures(out); len(sigs) > 0 {
			return fmt.Errorf("disk %s already holds data (%s); refusing to partition it -- this machine may have an active OS. "+
				"Redeploy with --destructive to overwrite it",
				diskPath, strings.Join(sigs, ", "))
		}
		ctx.Log.Info("disk is clean", "disk", diskPath)
	}
	return nil
}

// wipefsSignatures summarizes `wipefs --noheadings --output TYPE,OFFSET`
// as one entry per signature type, in the order wipefs lists them: "ext4 at
// 0x438", or "zfs_member at 0x23c00 and 28 more" for a type that recurs
// (wipefs lists every ZFS uberblock it finds). Every non-blank line counts:
// a line without the expected two columns is kept verbatim, so an
// unexpected format can only make the safety check refuse, never pass.
func wipefsSignatures(out string) []string {
	var types []string
	first := map[string]string{}
	count := map[string]int{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		typ, desc := strings.TrimSpace(line), strings.TrimSpace(line)
		if len(fields) == 2 {
			typ, desc = fields[0], fields[0]+" at "+fields[1]
		}
		if count[typ] == 0 {
			types = append(types, typ)
			first[typ] = desc
		}
		count[typ]++
	}
	sigs := make([]string, 0, len(types))
	for _, typ := range types {
		sig := first[typ]
		if more := count[typ] - 1; more > 0 {
			sig += fmt.Sprintf(" and %d more", more)
		}
		sigs = append(sigs, sig)
	}
	return sigs
}

// CheckMDNamesFree is the second safety check of a deploy without
// --destructive: it refuses, before any disk is touched, when an md array is
// already assembled at a /dev/mdN node the plan creates a raid device at.
//
// The deploy environment's udev assembles every md array it finds, and an
// array whose superblock asks for /dev/md0 gets it. On this path the
// layout's own disks are blank (CheckDisksClean), so an assembled array's
// members are all on disks the deploy does not write. `mdadm --create` fails
// on a node in use, and by then the plan has partitioned the layout's disks:
// the deploy would fail half applied, and the retry would need --destructive
// for disks that held nothing.
//
// The agent refuses rather than stopping the array or creating the raid at
// another node:
//
//   - Stopping it is what --destructive authorizes (WipeStaleState stops
//     every assembled array). A deploy without it leaves md state outside
//     the layout as found.
//   - The node is not the agent's to choose. /dev/mdN is fixed by the layout
//     (storage.MDDevMap) and is the node the plan formats and mounts and the
//     installed system's mdadm.conf names.
//
// A layout with a preserve device is not checked: PreflightPreserve and
// StopAutoAssembly release every auto-assembled array on that path, before
// the plan runs, so the preserved array can be assembled at its own node.
//
// /proc/mdstat does not exist until the md driver is loaded, which on a host
// with no arrays is not until the first mdadm --create. A failed read
// therefore passes when the file is confirmed absent, and only then: a
// mdstat that exists and cannot be read leaves the nodes' state unknown,
// which fails the check as an unprobeable disk fails CheckDisksClean.
func CheckMDNamesFree(ctx *agent.Context, layout storage.Layout) error {
	if HasPreserveDevices(layout) {
		return nil
	}
	planned := storage.MDDevMap(layout)
	if len(planned) == 0 {
		return nil
	}
	out, err := ctx.Exec.Output("cat", "/proc/mdstat")
	if err != nil {
		if _, absentErr := ctx.Exec.Output("test", "!", "-e", "/proc/mdstat"); absentErr == nil {
			ctx.Log.Info("no /proc/mdstat: the md driver is not loaded, so no arrays are assembled")
			return nil
		}
		return fmt.Errorf("cannot tell whether the /dev/mdN nodes the layout creates raid devices at are free: "+
			"/proc/mdstat exists but reading it failed: %s; refusing to partition any disk. "+
			"Check the deploy environment from a --recovery boot (cat /proc/mdstat), then redeploy; "+
			"--destructive skips this check and stops every assembled array", probeError(err))
	}
	assembled := map[string]string{}
	for _, d := range parseMdstat(out) {
		assembled["/dev/"+d.name] = d.name + " : " + d.status
	}
	names := make([]string, 0, len(planned))
	for name := range planned {
		names = append(names, name)
	}
	sort.Strings(names)
	var taken []string
	for _, name := range names {
		if line, ok := assembled[planned[name]]; ok {
			taken = append(taken, fmt.Sprintf("raid %q is created at %s, where an array is already assembled (%s)", name, planned[name], line))
		}
	}
	if len(taken) == 0 {
		return nil
	}
	return fmt.Errorf("%s. The deploy environment assembled it from disks this deploy does not write, and a deploy without "+
		"--destructive does not stop arrays, so mdadm --create would fail after the layout's disks were partitioned; "+
		"nothing has been changed. Redeploy with --destructive: it stops every assembled array before partitioning and "+
		"writes only to the disks the layout targets, so the old array's members keep their data. "+
		"If the old array is no longer needed and its disks are outside the layout, add --wipe-all-disks to zero its superblocks too",
		strings.Join(taken, "; "))
}

// CheckStaleArrayMembers refuses the deploy, before any disk is touched,
// when a device the deploy leaves unwritten carries an md member superblock
// with the array UUID that a raid device in the layout pins (array_uuid).
//
// A pinned UUID is the same on every create of that array, so a member
// superblock left by an earlier create cannot be told apart, by UUID, from
// the array this deploy builds. The trigger is a redeploy that builds the
// array over different disks: the disk it no longer uses keeps a member
// superblock with that UUID. At boot, when udev offers that disk to mdadm
// first, the stale member claims the array, the real members are refused,
// the mount fails and the boot stops in emergency mode. Which disk udev
// offers first is a race, so such a deploy is not bootable.
//
// The devices examined are every whole disk that is not a disk-type layout
// target, and its partitions. Disk-type targets are skipped: --destructive
// zeroes their superblocks (WipeStaleState), and without it CheckDisksClean
// refuses any target that is not blank. Under --wipe-all-disks the
// bystander disks are skipped too, because WipeBystanderDisks zeroes their
// superblocks; the disks it leaves alone (retained disks and the disks of
// preserve members) are still examined.
//
// The agent refuses rather than zeroing the stale member itself because the
// flags do not authorize that write: --destructive covers the disks the
// layout targets, and only --wipe-all-disks extends the wipe to disks
// outside the layout (see "The --destructive and --wipe-all-disks flags" in
// docs/dev/state-machine.md). A stale member is half of a former array and
// can be the last copy of its data.
//
// mdadm --examine exits non-zero on a device with no md superblock, the
// common case, so a failed examine counts as no superblock: a device mdadm
// cannot read now is one boot-time assembly cannot read either.
func CheckStaleArrayMembers(ctx *agent.Context, layout storage.Layout, wipeAllDisks bool) error {
	pinned := pinnedArrayUUIDs(layout)
	if len(pinned) == 0 {
		return nil
	}
	out, err := ctx.Exec.Output("lsblk", "-dnlo", "NAME,TYPE")
	if err != nil {
		return fmt.Errorf("lsblk: %w; cannot list the disks outside the layout to check them for "+
			"stale members of a pinned array_uuid; rerun the deploy once lsblk works", err)
	}
	targets := diskTypeNames(layout)
	claimed, err := wipeAllDisksExclusions(ctx, layout)
	if err != nil {
		return fmt.Errorf("%w; cannot tell which disks --wipe-all-disks leaves alone to check them for "+
			"stale members of a pinned array_uuid; check the storage script's preserve member names against lsblk on the target", err)
	}
	var wipeable, protected []string
	for _, disk := range parseWholeDisks(out) {
		if targets[disk] || (wipeAllDisks && !claimed[disk]) {
			continue
		}
		parts, err := listPartitions(ctx, disk)
		if err != nil {
			return fmt.Errorf("lsblk /dev/%s: %w; cannot list its partitions to check them for "+
				"stale members of a pinned array_uuid; rerun the deploy once lsblk works", disk, err)
		}
		for _, dev := range append([]string{"/dev/" + disk}, parts...) {
			examine, err := ctx.Exec.Output("mdadm", "--examine", dev)
			if err != nil {
				continue
			}
			for _, got := range parseExamineArrayUUIDs(examine) {
				for _, p := range pinned {
					if !uuidsEqual(got, p.uuid) {
						continue
					}
					conflict := fmt.Sprintf("%s (raid %q, array UUID %s)", dev, p.name, p.uuid)
					if claimed[disk] {
						protected = append(protected, conflict)
					} else {
						wipeable = append(wipeable, conflict)
					}
				}
			}
		}
	}
	if len(wipeable) == 0 && len(protected) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("a device this deploy does not write carries an md member superblock with an array UUID the layout pins; " +
		"at boot that stale member can claim the array ahead of its real members and stop the boot in emergency mode")
	if len(wipeable) > 0 {
		fmt.Fprintf(&b, ". %s: rerun the deploy with --wipe-all-disks, which zeroes md superblocks on disks outside the layout, "+
			"or, if that old array's data is not needed, zero it with `mdadm --zero-superblock <device>` from a --recovery boot and redeploy",
			strings.Join(wipeable, ", "))
	}
	if len(protected) > 0 {
		fmt.Fprintf(&b, ". %s: --wipe-all-disks leaves retained and preserve-member disks alone; "+
			"if that old array's data is not needed, zero it with `mdadm --zero-superblock <device>` from a --recovery boot and redeploy, "+
			"or detach the disk",
			strings.Join(protected, ", "))
	}
	return errors.New(b.String())
}

// pinnedArray is one raid device that pins its array UUID.
type pinnedArray struct{ name, uuid string }

// pinnedArrayUUIDs returns every raid device that sets array_uuid, sorted
// by device name so error messages are stable.
func pinnedArrayUUIDs(layout storage.Layout) []pinnedArray {
	var pinned []pinnedArray
	for name, dev := range layout.Devices {
		if dev.Type == "raid" && dev.ArrayUUID != "" {
			pinned = append(pinned, pinnedArray{name: name, uuid: dev.ArrayUUID})
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].name < pinned[j].name })
	return pinned
}

// parseExamineArrayUUIDs returns the array UUIDs in `mdadm --examine`
// output: the "Array UUID" line of 1.x metadata and the "UUID" line of 0.90
// (whose value can carry a trailing "(local to host ...)"). "Device UUID"
// names the member, not the array, and is ignored.
func parseExamineArrayUUIDs(examine string) []string {
	var uuids []string
	for line := range strings.SplitSeq(examine, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if k := strings.TrimSpace(key); k != "Array UUID" && k != "UUID" {
			continue
		}
		if fields := strings.Fields(val); len(fields) > 0 {
			uuids = append(uuids, fields[0])
		}
	}
	return uuids
}

// probeError describes a failed probe, preferring the tool's own stderr
// (which names the cause, e.g. an I/O error) over a bare exit status.
func probeError(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return fmt.Sprintf("%v: %s", err, msg)
		}
	}
	return err.Error()
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
