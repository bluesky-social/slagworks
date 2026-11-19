package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Property-based coverage for storage.Plan. The structural invariants are:
//   - sgdisk --zap-all is the first command that touches each managed disk
//   - partition numbers are sequential per disk
//   - blockdev --rereadpt follows partitioning and precedes partition mkfs
//   - mounts are depth-ordered, with parents before children
//   - digit-ending disk names use a p partition separator; SATA names do not
//   - RAID members are never formatted directly
//   - mdadm creates an array before its md device is formatted
//   - mkdir -p creates every mount target before mount uses it
//   - every partition and RAID array the plan creates and does not format
//     is cleared with wipefs -a (a partition also has its first 4 KiB
//     zeroed) once it exists and before mdadm uses it, and nothing else (a
//     preserve array or member, a retained disk) is
//
// The generator must only produce valid layouts. Each predicate derives its
// expectation from the generated Layout rather than echoing Plan's output.

type scenario struct {
	layout    Layout
	inventory []BlockDevice
}

// partNameLocal mirrors the documented separator rule: a disk whose name
// ends in a digit (nvme0n1, mmcblk0) separates partition numbers with 'p',
// a letter-terminated SATA name (sda) does not. Used to construct valid
// partition names and, independently, to derive expected device paths.
func partNameLocal(disk string, num int) string {
	sep := ""
	if len(disk) > 0 && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		sep = "p"
	}
	return fmt.Sprintf("%s%s%d", disk, sep, num)
}

// drawSize picks a partition size, allowing at most one "rest" per disk so
// the generated layout always satisfies validateDisk.
func drawSize(t *rapid.T, restUsed *bool) string {
	s := rapid.SampledFrom([]string{"100G", "200G", "512M", "rest"}).Draw(t, "partSize")
	if s == "rest" {
		if *restUsed {
			s = "100G"
		} else {
			*restUsed = true
		}
	}
	return s
}

func genParts(t *rapid.T, disk string, forceMountable bool) []Partition {
	minP := 0
	if forceMountable {
		minP = 1
	}
	n := rapid.IntRange(minP, 4).Draw(t, "numParts")
	parts := make([]Partition, 0, n)
	restUsed := false
	for i := 0; i < n; i++ {
		var kind string
		if i == 0 && forceMountable {
			kind = rapid.OneOf(rapid.Just("esp"), rapid.Just("fs")).Draw(t, "mountableKind")
		} else {
			kind = rapid.SampledFrom([]string{"esp", "fs", "swap", "biosgrub", "raid"}).Draw(t, "partKind")
		}
		p := Partition{Name: partNameLocal(disk, i+1)}
		switch kind {
		case "esp":
			p.Size, p.FS, p.Bootable = "512M", "vfat", true
		case "fs":
			p.FS = rapid.SampledFrom([]string{"ext4", "xfs"}).Draw(t, "fs")
			p.Size = drawSize(t, &restUsed)
		case "swap":
			p.FS = "swap"
			p.Size = drawSize(t, &restUsed)
		case "biosgrub":
			p.Size, p.BIOSGrub = "1M", true
		case "raid":
			p.Size = "100G"
		}
		parts = append(parts, p)
	}
	return parts
}

// genScenario produces a (possibly) richly-populated but always valid
// Layout plus the matching physical inventory. It covers 1-8 SATA/NVMe
// disks, sequentially named partitions with real filesystems, ESP and
// BIOS-grub partitions, RAID-member partitions, optional RAID0/1/10 arrays
// with valid member counts (formatted, or unformatted and unmounted), an
// optional retained disk, an optional preserve array on disks outside the
// layout, and nested mount paths.
func genScenario() *rapid.Generator[scenario] {
	return rapid.Custom(func(t *rapid.T) scenario {
		nDisks := rapid.IntRange(1, 8).Draw(t, "numDisks")
		type diskBuild struct {
			name      string
			transport string
			parts     []Partition
		}
		var disks []diskBuild
		nvmeIdx, sataIdx := 0, 0
		for i := 0; i < nDisks; i++ {
			var name, transport string
			if rapid.Bool().Draw(t, "diskStyle") {
				name = fmt.Sprintf("nvme%dn1", nvmeIdx)
				transport = "nvme"
				nvmeIdx++
			} else {
				name = fmt.Sprintf("sd%c", 'a'+sataIdx)
				transport = "sata"
				sataIdx++
			}
			disks = append(disks, diskBuild{name, transport, genParts(t, name, i == 0)})
		}

		devices := map[string]Device{}
		inventory := []BlockDevice{}
		var mountableParts, raidCandidates []string
		for _, d := range disks {
			devices[d.name] = Device{Type: "disk", PTable: "gpt", Partitions: d.parts}
			inventory = append(inventory, BlockDevice{
				Name: d.name, SizeBytes: 107374182400, Transport: d.transport,
			})
			if len(d.parts) == 0 {
				raidCandidates = append(raidCandidates, d.name) // whole-disk RAID member
				continue
			}
			for _, p := range d.parts {
				switch p.FS {
				case "vfat", "ext4", "xfs":
					mountableParts = append(mountableParts, p.Name)
				case "swap":
					// swap is created but never mounted
				default:
					if !p.BIOSGrub {
						raidCandidates = append(raidCandidates, p.Name)
					}
				}
			}
		}

		memberOrder := rapid.Permutation(raidCandidates).Draw(t, "raidMemberOrder")
		nRaid := rapid.IntRange(0, 2).Draw(t, "numRaids")
		var raidFS []string
		for r := 0; r < nRaid; r++ {
			level := rapid.SampledFrom([]int{0, 1, 10}).Draw(t, "raidLevel")
			need := map[int]int{0: 2, 1: 2, 10: 4}[level]
			if len(memberOrder) < need {
				continue
			}
			k := rapid.IntRange(need, len(memberOrder)).Draw(t, "raidMemberCount")
			members := append([]string{}, memberOrder[:k]...)
			memberOrder = memberOrder[k:]
			raidName := fmt.Sprintf("md%d", r)
			fs := rapid.SampledFrom([]string{"ext4", "xfs", ""}).Draw(t, "raidFS")
			devices[raidName] = Device{Type: "raid", Level: level, Members: members, FS: fs}
			if fs != "" {
				raidFS = append(raidFS, raidName)
			}
		}

		// Data the plan must leave alone. The generated disks are at most
		// sda..sdh and nvme0n1..nvme7n1, so sdy and sdz never collide.
		if rapid.Bool().Draw(t, "retainDisk") {
			devices["sdz"] = Device{Type: "retain"}
			inventory = append(inventory, BlockDevice{Name: "sdz", SizeBytes: 107374182400, Transport: "sata"})
		}
		if rapid.Bool().Draw(t, "preserveArray") {
			devices["mdkeep"] = Device{
				Type: "preserve", Level: 1, Members: []string{"sdy1", "/dev/sdy2"},
				FS:        rapid.SampledFrom([]string{"xfs", ""}).Draw(t, "preserveFS"),
				ArrayUUID: "22222222-2222-2222-2222-222222222222",
			}
		}

		mountable := append([]string{}, mountableParts...)
		mountable = append(mountable, raidFS...) // disk[0] guarantees non-empty
		rootIdx := rapid.IntRange(0, len(mountable)-1).Draw(t, "rootDevice")
		mounts := map[string]Mount{"/": {Device: mountable[rootIdx]}}
		rest := append([]string{}, mountable[:rootIdx]...)
		rest = append(rest, mountable[rootIdx+1:]...)

		pathPool := []string{"/boot", "/boot/efi", "/var", "/var/lib", "/var/lib/docker", "/data", "/srv/data", "/home"}
		maxExtra := len(rest)
		if len(pathPool) < maxExtra {
			maxExtra = len(pathPool)
		}
		nExtra := rapid.IntRange(0, maxExtra).Draw(t, "numExtraMounts")
		mountPaths := rapid.Permutation(pathPool).Draw(t, "mountPaths")
		for i := 0; i < nExtra; i++ {
			mounts[mountPaths[i]] = Mount{Device: rest[i]}
		}

		return scenario{Layout{Devices: devices, Mounts: mounts}, inventory}
	})
}

// --- predicates: each derives an expectation from the Layout and checks the plan ---

func checkZapFirst(layout Layout, cmds []Command) error {
	for _, d := range diskNames(layout) {
		path := "/dev/" + d
		first := -1
		for i, c := range cmds {
			for _, a := range c.Args {
				if a == path {
					first = i
					break
				}
			}
			if first >= 0 {
				break
			}
		}
		if first == -1 {
			return fmt.Errorf("disk %q: no command references %s", d, path)
		}
		c := cmds[first]
		if c.Name != "sgdisk" || len(c.Args) < 2 || c.Args[0] != "--zap-all" || c.Args[1] != path {
			return fmt.Errorf("disk %q: first command is %s, want sgdisk --zap-all %s", d, c, path)
		}
	}
	return nil
}

func checkSequentialPartitions(layout Layout, cmds []Command) error {
	for _, d := range diskNames(layout) {
		dev := layout.Devices[d]
		if len(dev.Partitions) == 0 {
			continue
		}
		path := "/dev/" + d
		var nums []string
		for _, c := range cmds {
			if c.Name == "sgdisk" && len(c.Args) >= 4 && c.Args[len(c.Args)-1] == path && c.Args[0] == "-n" {
				nums = append(nums, strings.SplitN(c.Args[1], ":", 2)[0])
			}
		}
		want := make([]string, len(dev.Partitions))
		for i := range want {
			want[i] = fmt.Sprintf("%d", i+1)
		}
		if !equalStrings(nums, want) {
			return fmt.Errorf("disk %q: sgdisk partition numbers %v, want %v", d, nums, want)
		}
	}
	return nil
}

func isMkfs(c Command) bool {
	switch c.Name {
	case "mkfs.fat", "mkfs.ext4", "mkfs.xfs", "mkswap":
		return true
	}
	return false
}

func checkBlockdevOrder(layout Layout, cmds []Command) error {
	for _, d := range diskNames(layout) {
		dev := layout.Devices[d]
		if len(dev.Partitions) == 0 {
			continue
		}
		path := "/dev/" + d
		blockIdx := -1
		for i, c := range cmds {
			if c.Name == "blockdev" && len(c.Args) == 2 && c.Args[0] == "--rereadpt" && c.Args[1] == path {
				blockIdx = i
				break
			}
		}
		if blockIdx == -1 {
			return fmt.Errorf("disk %q: missing blockdev --rereadpt", d)
		}
		lastSg := -1
		for i, c := range cmds {
			if c.Name == "sgdisk" && len(c.Args) >= 4 && c.Args[len(c.Args)-1] == path && c.Args[0] == "-n" {
				lastSg = i
			}
		}
		if lastSg >= blockIdx {
			return fmt.Errorf("disk %q: last sgdisk at %d not before blockdev at %d", d, lastSg, blockIdx)
		}
		for i, p := range dev.Partitions {
			if p.FS == "" {
				continue
			}
			pp := "/dev/" + partNameLocal(d, i+1)
			for j, c := range cmds {
				if isMkfs(c) && len(c.Args) > 0 && c.Args[len(c.Args)-1] == pp && j < blockIdx {
					return fmt.Errorf("disk %q: mkfs of %s at %d precedes blockdev at %d", d, pp, j, blockIdx)
				}
			}
		}
	}
	return nil
}

func checkMountDepthOrder(layout Layout, cmds []Command) error {
	paths := make([]string, 0, len(layout.Mounts))
	for p := range layout.Mounts {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
	want := make([]string, 0, len(paths))
	for _, p := range paths {
		tgt := "/target" + p
		if tgt != "/target" {
			tgt = strings.TrimRight(tgt, "/")
		}
		want = append(want, tgt)
	}
	var got []string
	for _, c := range cmds {
		if c.Name == "mount" && len(c.Args) == 2 {
			got = append(got, c.Args[1])
		}
	}
	if !equalStrings(got, want) {
		return fmt.Errorf("mount target order %v, want %v", got, want)
	}
	return nil
}

func checkPartitionSeparators(layout Layout, cmds []Command) error {
	for _, d := range diskNames(layout) {
		dev := layout.Devices[d]
		if len(dev.Partitions) == 0 {
			continue
		}
		path := "/dev/" + d
		expected := make(map[string]bool)
		for i := range dev.Partitions {
			expected["/dev/"+partNameLocal(d, i+1)] = true
		}
		for _, c := range cmds {
			for _, a := range c.Args {
				if strings.HasPrefix(a, path) && a != path && !expected[a] {
					return fmt.Errorf("disk %q: unexpected partition path %q (want one of %v)", d, a, sortedKeys(expected))
				}
			}
		}
	}
	return nil
}

func checkRAIDMembersNotFormatted(layout Layout, cmds []Command) error {
	for _, dev := range layout.Devices {
		if dev.Type != "raid" {
			continue
		}
		for _, m := range dev.Members {
			rp := "/dev/" + stripDevPrefix(m)
			for _, c := range cmds {
				if isMkfs(c) && len(c.Args) > 0 && c.Args[len(c.Args)-1] == rp {
					return fmt.Errorf("raid member %q (%s) formatted directly: %s", m, rp, c)
				}
			}
		}
	}
	return nil
}

// propMDDevPath derives the /dev/mdN node the plan assigns to a raid or
// preserve device from the sorted set of such device names.
func propMDDevPath(layout Layout, name string) string {
	var names []string
	for n, d := range layout.Devices {
		if d.Type == "raid" || d.Type == "preserve" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for i, n := range names {
		if n == name {
			return fmt.Sprintf("/dev/md%d", i)
		}
	}
	return ""
}

func checkMDAdmBeforeMkfs(layout Layout, cmds []Command) error {
	for name, dev := range layout.Devices {
		if dev.Type != "raid" || dev.FS == "" {
			continue
		}
		mdPath := propMDDevPath(layout, name)
		mdadmIdx, mkfsIdx := -1, -1
		for i, c := range cmds {
			if c.Name == "mdadm" && len(c.Args) >= 2 && c.Args[0] == "--create" && c.Args[1] == mdPath {
				mdadmIdx = i
			}
			if isMkfs(c) && len(c.Args) > 0 && c.Args[len(c.Args)-1] == mdPath {
				mkfsIdx = i
			}
		}
		if mdadmIdx == -1 || mkfsIdx == -1 || mdadmIdx >= mkfsIdx {
			return fmt.Errorf("raid %q (%s): mdadm idx=%d must precede mkfs idx=%d", name, mdPath, mdadmIdx, mkfsIdx)
		}
	}
	return nil
}

// checkUnformattedCleared derives from the layout every block device the plan
// creates and leaves unformatted: each FS-less partition of a managed disk
// and each FS-less raid array. Each must get exactly one wipefs -a, and each
// such partition also exactly one dd zeroing its first 4 KiB: a partition's
// after its disk's blockdev --rereadpt and before any mdadm, an array's after
// its mdadm --create. No other device may be written by wipefs or dd, which
// keeps them off preserve arrays, their members, and retained disks.
func checkUnformattedCleared(layout Layout, cmds []Command) error {
	firstMdadm := len(cmds)
	for i, c := range cmds {
		if c.Name == "mdadm" {
			firstMdadm = i
			break
		}
	}
	indexOf := func(pred func(Command) bool) int {
		for i, c := range cmds {
			if pred(c) {
				return i
			}
		}
		return -1
	}

	// expected maps each device to clear to the window its clearing must
	// fall in and whether its boot sector is zeroed too.
	type target struct {
		after, before int
		zero          bool
	}
	expected := map[string]target{}
	for _, d := range diskNames(layout) {
		path := "/dev/" + d
		reread := indexOf(func(c Command) bool {
			return c.Name == "blockdev" && len(c.Args) == 2 && c.Args[0] == "--rereadpt" && c.Args[1] == path
		})
		for i, p := range layout.Devices[d].Partitions {
			if p.FS == "" {
				expected["/dev/"+partNameLocal(d, i+1)] = target{reread, firstMdadm, true}
			}
		}
	}
	for name, dev := range layout.Devices {
		if dev.Type != "raid" || dev.FS != "" {
			continue
		}
		md := propMDDevPath(layout, name)
		create := indexOf(func(c Command) bool {
			return c.Name == "mdadm" && len(c.Args) >= 2 && c.Args[0] == "--create" && c.Args[1] == md
		})
		expected[md] = target{create, len(cmds), false}
	}

	wiped, zeroed := map[string]int{}, map[string]int{}
	for i, c := range cmds {
		var dev string
		switch c.Name {
		case "wipefs":
			if len(c.Args) != 2 || c.Args[0] != "-a" {
				return fmt.Errorf("cmd[%d] %s: want wipefs -a <device>", i, c)
			}
			dev = c.Args[1]
			wiped[dev]++
		case "dd":
			if len(c.Args) != 5 || c.Args[0] != "if=/dev/zero" || !strings.HasPrefix(c.Args[1], "of=") ||
				c.Args[2] != "bs=4096" || c.Args[3] != "count=1" || c.Args[4] != "conv=fsync" {
				return fmt.Errorf("cmd[%d] %s: want dd if=/dev/zero of=<device> bs=4096 count=1 conv=fsync", i, c)
			}
			dev = strings.TrimPrefix(c.Args[1], "of=")
			zeroed[dev]++
		default:
			continue
		}
		w, ok := expected[dev]
		if !ok {
			return fmt.Errorf("cmd[%d] %s clears a device the plan does not create unformatted", i, c)
		}
		if w.after < 0 || i <= w.after || i >= w.before {
			return fmt.Errorf("cmd[%d] %s outside its window (after %d, before %d)", i, c, w.after, w.before)
		}
	}
	for dev, w := range expected {
		if wiped[dev] != 1 {
			return fmt.Errorf("%s: wipefs ran %d times, want once", dev, wiped[dev])
		}
		want := 0
		if w.zero {
			want = 1
		}
		if zeroed[dev] != want {
			return fmt.Errorf("%s: boot sector zeroed %d times, want %d", dev, zeroed[dev], want)
		}
	}
	return nil
}

func checkMkdirBeforeMount(layout Layout, cmds []Command) error {
	for path := range layout.Mounts {
		tgt := "/target" + path
		if tgt != "/target" {
			tgt = strings.TrimRight(tgt, "/")
		}
		mkdirIdx, mountIdx := -1, -1
		for i, c := range cmds {
			if c.Name == "mkdir" && len(c.Args) == 2 && c.Args[0] == "-p" && c.Args[1] == tgt {
				mkdirIdx = i
			}
			if c.Name == "mount" && len(c.Args) == 2 && c.Args[1] == tgt {
				mountIdx = i
			}
		}
		if mkdirIdx == -1 || mountIdx == -1 || mkdirIdx >= mountIdx {
			return fmt.Errorf("mount %q: mkdir idx=%d must precede mount idx=%d", path, mkdirIdx, mountIdx)
		}
	}
	return nil
}

// --- shared helpers ---

func diskNames(layout Layout) []string {
	var names []string
	for n, d := range layout.Devices {
		if d.Type == "disk" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func replayLog(rt *rapid.T, layout Layout, inventory []BlockDevice, cmds []Command) {
	rt.Logf("layout: %s", mustJSON(layout))
	rt.Logf("inventory: %s", mustJSON(inventory))
	for i, c := range cmds {
		rt.Logf("cmd[%d]: %s", i, c)
	}
}

// TestPlan_Property_StructuralContracts runs one rapid.Check asserting the
// structural contracts below.
func TestPlan_Property_StructuralContracts(t *testing.T) {
	checks := []struct {
		name string
		fn   func(Layout, []Command) error
	}{
		{"zap is the first command per managed disk", checkZapFirst},
		{"partition numbers are sequential per disk", checkSequentialPartitions},
		{"blockdev --rereadpt follows sgdisk and precedes partition mkfs", checkBlockdevOrder},
		{"mount targets are depth ordered", checkMountDepthOrder},
		{"digit-ending disks use p separators, SATA does not", checkPartitionSeparators},
		{"RAID members are never formatted directly", checkRAIDMembersNotFormatted},
		{"mdadm creation precedes mkfs of its md device", checkMDAdmBeforeMkfs},
		{"mkdir -p for a target precedes its mount", checkMkdirBeforeMount},
		{"exactly the unformatted devices the plan creates are cleared, in order", checkUnformattedCleared},
	}

	rapid.Check(t, func(rt *rapid.T) {
		s := genScenario().Draw(rt, "scenario")
		cmds, err := Plan(s.layout, s.inventory)
		if err != nil {
			replayLog(rt, s.layout, s.inventory, cmds)
			rt.Fatalf("Plan error on generated layout: %v", err)
		}
		for _, ch := range checks {
			if err := ch.fn(s.layout, cmds); err != nil {
				replayLog(rt, s.layout, s.inventory, cmds)
				rt.Fatalf("contract %q violated: %v", ch.name, err)
			}
		}
	})
}
