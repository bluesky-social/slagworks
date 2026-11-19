package storageapply

import (
	"fmt"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// PreflightPreserve verifies every preserve device in the layout BEFORE any
// destructive command runs. It is the guard that makes preserve safe against
// operator naming errors and against a partial-apply disaster where the boot
// disks are already zapped and root recreated but /data can't be assembled.
//
// For each preserve device it:
//
//  1. Rejects any member held by a disk-type layout target (that disk gets
//     sgdisk --zap-all'd, which destroys the member). This is Phase-1 roast
//     finding #3: a member naming an UNDECLARED partition of a managed disk
//     (e.g. nvme0n1p9 when disk nvme0n1 declares only p1-p3) is invisible to
//     static validation -- validatePreserveMemberCollisions compares full
//     partition names, not parent disks -- but is destroyed by the whole-disk
//     zap. memberDisks asks the kernel which disks hold the member, so the
//     check holds however the member is spelled (kernel name, /dev/disk/by-*
//     path); that needs the live host, so it runs here rather than in static
//     validate().
//
//  2. Assembles the array by its expected UUID from the named members
//     (assemble is non-destructive w.r.t. member data), reads mdadm --detail,
//     and FATALs if the assembled array's UUID differs from the expected
//     ArrayUUID -- the named members must actually form the expected array,
//     not a stale superblock from a prior life or a swapped disk.
//
//  3. If FSUUID is declared, blkid's the assembled device and FATALs on
//     filesystem-UUID mismatch.
//
// On ANY failure the host is left with member data untouched (assemble/stop
// are non-destructive). The array is STOPPED before returning: planPreserve's
// --assemble in the main command list is the single authoritative assemble
// that leaves it active for mount, so a preflight that left it active would
// make that assemble fail "already active" on the non-destructive path (where
// WipeStaleState's global stop never runs to clear it). Stopping is idempotent
// with WipeStaleState's later `mdadm --stop --scan` on the destructive path.
//
// The array is assembled to the SAME /dev/mdN the plan will use (via
// storage.MDDevMap). Before the targeted assemble, a single global
// `mdadm --stop --scan` releases every auto-assembled array: the deploy
// initrd commonly auto-assembles a preserve array under a scan-chosen name
// (e.g. /dev/md127), which holds the members open and would make the targeted
// assemble fail EBUSY on a perfectly valid array. Stopping is non-destructive
// (the plan's whole premise), so releasing auto-assembly here does NOT violate
// the verify-before-wipe ordering -- it deletes no member data, it just frees
// the members so we can assemble them at the name the plan will mount. It is
// idempotent with WipeStaleState's later `mdadm --stop --scan` on the
// destructive path.
func PreflightPreserve(ctx *agent.Context, layout storage.Layout) error {
	preserveNames := preserveDeviceNames(layout)
	if len(preserveNames) == 0 {
		return nil
	}

	diskTargets := diskTypeNames(layout)
	mdDevMap := storage.MDDevMap(layout)

	ctx.Log.Info(fmt.Sprintf("preserve preflight: verifying %d preserve array(s) before any destructive command", len(preserveNames)))

	// Guard pass FIRST, before any mdadm call: no preserve member may live
	// on a disk-type layout target (that disk gets sgdisk --zap-all'd). This
	// FATALs with the host untouched -- not even a stop has run yet. A member
	// the kernel cannot find also FATALs here: it cannot be checked, and the
	// assemble below would fail on it anyway.
	for _, name := range preserveNames {
		for _, member := range layout.Devices[name].Members {
			disks, err := memberDisks(ctx, member)
			if err != nil {
				return fmt.Errorf(
					"preserve %q: cannot tell which disk holds member %q (%w); "+
						"check the storage script's member names against lsblk on the target",
					name, member, err)
			}
			for _, disk := range disks {
				if diskTargets[disk] {
					return fmt.Errorf(
						"preserve %q: member %q lives on disk %q which the layout partitions (sgdisk --zap-all would destroy it); "+
							"remove %q from the disk targets or from the preserve members -- they cannot be both",
						name, member, disk, disk)
				}
			}
		}
	}

	// Release any initrd auto-assembly (arrays held open under a scan-chosen
	// md name) so each targeted assemble below can claim its members. Global
	// and non-destructive; see the function doc.
	if err := ctx.Exec.Run("mdadm", "--stop", "--scan"); err != nil {
		ctx.Log.Info("preserve preflight: mdadm --stop --scan (no auto-assembled arrays is fine)", "err", err)
	}

	for _, name := range preserveNames {
		dev := layout.Devices[name]
		md := mdDevMap[name]

		members := make([]string, 0, len(dev.Members))
		for _, m := range dev.Members {
			members = append(members, resolveMemberPath(m))
		}
		assembleArgs := append([]string{"--assemble", md, "--uuid", dev.ArrayUUID}, members...)
		if err := ctx.Exec.Run("mdadm", assembleArgs...); err != nil {
			return fmt.Errorf(
				"preserve %q: mdadm --assemble by uuid %s from members %s failed: %w; "+
					"the named members do not form the expected array (missing/swapped data disk, or wrong array_uuid in the storage script)",
				name, dev.ArrayUUID, strings.Join(members, " "), err)
		}

		if err := verifyAssembledArray(ctx, md, name, dev); err != nil {
			// Leave member data untouched; stop the array we assembled.
			_ = ctx.Exec.Run("mdadm", "--stop", md)
			return err
		}

		// Verified: stop it so planPreserve's --assemble in the main command
		// loop is the single authoritative assemble. A FAILED stop is fatal
		// HERE, while the host is still pre-destructive: leaving the array
		// active would make that later assemble fail "already active" only
		// AFTER the boot disks are zapped/recreated -- the exact partial-apply
		// disaster this preflight exists to prevent. Stopping is
		// non-destructive, so failing the deploy now leaves member data
		// untouched.
		if err := ctx.Exec.Run("mdadm", "--stop", md); err != nil {
			return fmt.Errorf(
				"preserve %q: verified array %s could not be stopped for the planned re-assemble: %w; "+
					"refusing to proceed into destructive disk work with the array still active",
				name, md, err)
		}
		ctx.Log.Info("preserve preflight: array verified", "device", name, "md", md, "array_uuid", dev.ArrayUUID)
	}
	return nil
}

// VerifyPreserveArrays re-checks every preserve array's identity AFTER the
// main storage command loop (where planPreserve re-assembled it following
// WipeStaleState's global stop). Preflight verified before any destructive
// work; this is the backstop that the re-assemble reconstructed the SAME
// array identity before the deployed OS is finalized. FATAL on mismatch.
//
// Runs after the command loop and before the root extract. The array is
// already assembled and mounted at /target<mount> by this point, but nothing
// has written to the preserved data yet (the extract rsync's --one-file-system
// does not cross into the mount), so a mismatch here still fails before any
// write to the data being preserved.
func VerifyPreserveArrays(ctx *agent.Context, layout storage.Layout) error {
	preserveNames := preserveDeviceNames(layout)
	if len(preserveNames) == 0 {
		return nil
	}
	mdDevMap := storage.MDDevMap(layout)
	for _, name := range preserveNames {
		dev := layout.Devices[name]
		if err := verifyAssembledArray(ctx, mdDevMap[name], name, dev); err != nil {
			return err
		}
		ctx.Log.Info("preserve verify: re-assembled array identity confirmed", "device", name, "md", mdDevMap[name])
	}
	return nil
}

// verifyAssembledArray confirms the array assembled at md has the expected
// array UUID (via mdadm --detail) and, when declared, the expected filesystem
// UUID (via blkid). Comparison is separator-insensitive: mdadm prints the
// array UUID colon-grouped, blkid/scripts use RFC-4122 dashes, and either is
// acceptable so long as the 32 hex nibbles match.
func verifyAssembledArray(ctx *agent.Context, md, name string, dev storage.Device) error {
	detail, err := ctx.Exec.Output("mdadm", "--detail", md)
	if err != nil {
		return fmt.Errorf("preserve %q: mdadm --detail %s failed: %w", name, md, err)
	}
	got, ok := parseMDADMDetailUUID(detail)
	if !ok {
		return fmt.Errorf("preserve %q: could not find array UUID in mdadm --detail %s output", name, md)
	}
	if !uuidsEqual(got, dev.ArrayUUID) {
		return fmt.Errorf(
			"preserve %q: assembled array %s has UUID %s but the layout expects %s; "+
				"refusing to mount -- this is a different array than the one named (swapped disk or stale superblock)",
			name, md, got, dev.ArrayUUID)
	}

	if dev.FSUUID != "" {
		fsUUID, err := ctx.Exec.Output("blkid", "-s", "UUID", "-o", "value", md)
		if err != nil {
			return fmt.Errorf("preserve %q: blkid %s failed: %w", name, md, err)
		}
		if !uuidsEqual(fsUUID, dev.FSUUID) {
			return fmt.Errorf(
				"preserve %q: filesystem on %s has UUID %s but the layout expects %s; "+
					"refusing to mount -- the array assembled but holds a different filesystem than expected",
				name, md, fsUUID, dev.FSUUID)
		}
	}
	return nil
}

// preserveDeviceNames returns the names of every preserve-type device in the
// layout. Order is not significant -- each is verified independently.
func preserveDeviceNames(layout storage.Layout) []string {
	var names []string
	for name, dev := range layout.Devices {
		if dev.Type == "preserve" {
			names = append(names, name)
		}
	}
	return names
}

// HasPreserveDevices reports whether the layout contains any preserve-type
// device. Callers use it to gate preserve-specific steps (preflight, the
// pre-assemble stop) so a plain deploy never perturbs md state.
func HasPreserveDevices(layout storage.Layout) bool {
	return len(preserveDeviceNames(layout)) > 0
}

// StopAutoAssembly tears down any md array udev may have (re-)assembled,
// so planPreserve's targeted `mdadm --assemble /dev/mdN` can claim its
// members instead of failing EBUSY on an array the kernel grabbed under a
// scan-chosen name (e.g. /dev/md127). It is the same non-destructive
// `--stop --scan` + residual-container teardown WipeStaleState runs, but
// callable on its own for the NON-destructive preserve path, where
// WipeStaleState never runs: PreflightPreserve verifies then stops the
// array, and in the window before the command loop reaches planPreserve
// the stock `mdadm --incremental` udev rules can re-assemble it from the
// member `change` uevents the stop emitted. Running this immediately
// before the command loop closes that window. Stopping is non-destructive
// (no member data is touched), so it is safe to run regardless of
// --destructive and idempotent with WipeStaleState's own stop.
func StopAutoAssembly(ctx *agent.Context) {
	if err := ctx.Exec.Run("mdadm", "--stop", "--scan"); err != nil {
		ctx.Log.Info("preserve: mdadm --stop --scan before assemble (no arrays is fine)", "err", err)
	}
	StopResidualMDDevices(ctx)
}

// diskTypeNames is the set of bare kernel names of every disk-type layout
// target -- the disks the plan partitions with sgdisk --zap-all.
func diskTypeNames(layout storage.Layout) map[string]bool {
	m := make(map[string]bool)
	for name, dev := range layout.Devices {
		if dev.Type == "disk" {
			m[name] = true
		}
	}
	return m
}

// resolveMemberPath converts a preserve member name to a /dev path, matching
// the planner's resolveDevPath (a "/dev/"-prefixed name is passed through).
func resolveMemberPath(name string) string {
	if strings.HasPrefix(name, "/dev/") {
		return name
	}
	return "/dev/" + name
}

// memberDisks returns the whole disks (lsblk TYPE "disk") that hold a preserve
// member, as the kernel reports them. Member names cannot be parsed for this:
// a /dev/disk/by-* path does not contain its disk's name, and the kernel's
// partition naming depends on the driver (pmem0 is a whole disk, pmem0p1 its
// partition). `lsblk -s` (inverse) lists the member and then every device
// beneath it, following symlinks and any partition or device-mapper layer
// down to the disks. NAME is the column bystanderDisks enumerates, so the
// answers compare directly. A member with no disk beneath it (a loop device)
// yields none. An error means the member is not a block device on this host,
// or lsblk answered in a shape we cannot read; callers fail closed on it.
func memberDisks(ctx *agent.Context, member string) ([]string, error) {
	path := resolveMemberPath(member)
	out, err := ctx.Exec.Output("lsblk", "-snlo", "NAME,TYPE", path)
	if err != nil {
		return nil, fmt.Errorf("lsblk %s: %w", path, err)
	}
	var disks []string
	rows := 0
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("lsblk %s: unexpected row %q", path, line)
		}
		rows++
		if f[1] == "disk" {
			disks = append(disks, f[0])
		}
	}
	if rows == 0 {
		return nil, fmt.Errorf("lsblk %s: no output", path)
	}
	return disks, nil
}

// parseMDADMDetailUUID extracts the array UUID token from `mdadm --detail`
// output. The relevant line is `           UUID : e8c9...:...:...:...`. Returns
// the raw token (colon-grouped hex) and whether it was found.
func parseMDADMDetailUUID(detail string) (string, bool) {
	for line := range strings.SplitSeq(detail, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "UUID" {
			continue
		}
		if v := strings.TrimSpace(val); v != "" {
			return v, true
		}
	}
	return "", false
}

// uuidsEqual compares two UUIDs by their hex nibbles alone, ignoring case and
// separators. mdadm prints the array UUID colon-grouped (e8c9aa81:...:...:...),
// blkid and storage scripts use RFC-4122 dashes; both encode the same 128
// bits, so normalizing to the bare 32-nibble hex string is the robust
// comparison. Empty/short inputs never compare equal.
func uuidsEqual(a, b string) bool {
	na, nb := normalizeUUIDHex(a), normalizeUUIDHex(b)
	if len(na) != 32 || len(nb) != 32 {
		return false
	}
	return na == nb
}

// normalizeUUIDHex lowercases s and strips every non-hex-digit character
// (dashes, colons, whitespace), yielding just the hex nibbles.
func normalizeUUIDHex(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
