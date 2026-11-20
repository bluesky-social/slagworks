package storage

import (
	"fmt"
	"sort"
	"strings"

	"pgregory.net/rapid"
)

// Generators for the plan simulator.

// simDiskSpec is one physical disk in a generated machine.
type simDiskSpec struct {
	Name       string
	Bytes      int64
	SectorSize int64
	Transport  string
	Role       string // target, retain, foreign
	// Prior partitions already on disk (foreign/retain/target): sizes in MiB.
	Prior []int64
	// PriorMD is the uuid of an md superblock this disk (PriorMDPart==0) or
	// its partition PriorMDPart carries, "" for none.
	PriorMD     string
	PriorMDPart int
}

type simCase struct {
	Disks  []simDiskSpec
	Layout Layout
	// Absent names disks the layout references that are not physically
	// present (neither in the inventory nor in the machine).
	Absent map[string]bool
	// Mutations applied to an otherwise realizable layout, for reporting.
	Mutations []string
}

func (sc simCase) inventory() []BlockDevice {
	var inv []BlockDevice
	for _, d := range sc.Disks {
		if sc.Absent[d.Name] {
			continue
		}
		inv = append(inv, BlockDevice{Name: d.Name, SizeBytes: d.Bytes, LogicalSectorBytes: d.SectorSize, Transport: d.Transport})
	}
	return inv
}

// machine builds the simulator's initial state for a case.
func (sc simCase) machine() *simMachine {
	m := newSimMachine()
	for _, ds := range sc.Disks {
		if sc.Absent[ds.Name] {
			continue
		}
		d := m.addDisk(ds.Name, ds.SectorSize, ds.Bytes)
		cur := d.align()
		for i, sz := range ds.Prior {
			n := sz * mib / ds.SectorSize
			if cur+n-1 > d.lastUsable() {
				break
			}
			d.table[i+1] = &simPart{num: i + 1, start: cur, end: cur + n - 1, code: "8300", label: "old"}
			d.kernel[i+1] = true
			m.gen++
			m.contents[blk{disk: ds.Name, part: i + 1}] = &content{fs: "ext4", fsGen: m.gen, preserve: true}
			cur += n
		}
		if ds.PriorMD != "" {
			b := blk{disk: ds.Name, part: ds.PriorMDPart}
			if ds.PriorMDPart == 0 || d.kernel[ds.PriorMDPart] {
				m.contents[b] = &content{mdUUID: ds.PriorMD, preserve: true}
			}
		}
	}
	for _, dev := range sc.Layout.Devices {
		if dev.Type == "preserve" {
			m.preservedFS[dev.ArrayUUID] = content{fs: dev.FS, fsGen: -1, preserve: true}
		}
	}
	m.snapshot()
	return m
}

var simSizes = []int64{
	1 * mib, 8 * mib, 64 * mib, 600 * mib, 1 << 30, 32 << 30, 100 << 30, 480103981056,
	1920383410176, 3840755982336, 7681501126656, 15360747397120, 30720 << 30,
	1<<41 + 4096, 1<<44 - 512*7, (1 << 46) + 12345*4096,
}

// knobs is the per-case swarm configuration.
type knobs struct {
	mutate          bool
	partName        float64
	restAny         float64
	oversize        float64
	badSize         float64
	memberAbuse     float64
	mountPath       float64
	mountAbuse      float64
	retainTrick     float64
	preserveCollide float64
	retainCollide   float64
	missingDisk     float64
	smallVFAT       float64
}

// drawKnobs picks, per case, how likely each class of defect is. Drawing
// the probabilities (zero included) rather than fixing them makes some cases
// dense in one class and free of the others, so a defect that Plan rejects
// early cannot mask the rest.
func drawKnobs(t *rapid.T, mutate bool) knobs {
	k := knobs{mutate: mutate}
	if !mutate {
		return k
	}
	p := func(label string) float64 {
		return rapid.SampledFrom([]float64{0, 0, 0.05, 0.2, 0.5}).Draw(t, "knob-"+label)
	}
	k.partName = p("partname")
	k.restAny = p("restany")
	k.oversize = p("oversize")
	k.badSize = p("badsize")
	k.memberAbuse = p("member")
	k.mountPath = p("mountpath")
	k.mountAbuse = p("mount")
	k.retainTrick = p("retaintrick")
	k.preserveCollide = p("preservecollide")
	k.retainCollide = p("retaincollide")
	k.missingDisk = p("missingdisk")
	k.smallVFAT = p("smallvfat")
	return k
}

func chance(t *rapid.T, p float64, label string) bool {
	if p <= 0 {
		return false
	}
	return rapid.Float64Range(0, 1).Draw(t, label) < p
}

func genDiskName(t *rapid.T, used map[string]bool, idx int) (string, string) {
	for {
		var name, tr string
		switch rapid.IntRange(0, 4).Draw(t, "family") {
		case 0, 1:
			name, tr = fmt.Sprintf("nvme%dn1", rapid.IntRange(0, 11).Draw(t, "nvmeIdx")), "nvme"
		case 2:
			name, tr = fmt.Sprintf("sd%c", 'a'+rapid.IntRange(0, 25).Draw(t, "sdIdx")), "sata"
		case 3:
			name, tr = fmt.Sprintf("vd%c", 'a'+rapid.IntRange(0, 5).Draw(t, "vdIdx")), "virtio"
		default:
			name, tr = fmt.Sprintf("mmcblk%d", rapid.IntRange(0, 2).Draw(t, "mmcIdx")), "mmc"
		}
		if !used[name] {
			used[name] = true
			return name, tr
		}
	}
}

// priorFits reports whether pre-existing partitions of the given MiB sizes
// fit on the disk with 1 MiB-aligned starts.
func priorFits(ds simDiskSpec, sizes []int64) bool {
	cur := int64(mib)
	for _, s := range sizes {
		cur += s * mib
	}
	return cur <= ds.Bytes-mib
}

// genPart is a partition under construction: its size stays a number until
// the layout is rendered, so it can be shrunk to fit.
type genPart struct {
	p        Partition
	mibs     int64 // fixed size; unused when rest
	rest     bool
	useG     bool   // spell a whole number of GiB as "<n>G"
	override string // a mutation's verbatim size
}

func (gp genPart) size() string {
	switch {
	case gp.override != "":
		return gp.override
	case gp.rest:
		return "rest"
	case gp.useG && gp.mibs > 0 && gp.mibs%1024 == 0:
		return fmt.Sprintf("%dG", gp.mibs/1024)
	}
	return fmt.Sprintf("%dM", gp.mibs)
}

// fat32MinMiB is the documented smallest vfat partition: FAT32 needs 65525
// clusters of at least one sector each, which is 33 MiB at 512-byte sectors
// and 257 MiB at 4096.
func fat32MinMiB(sectorSize int64) int64 {
	if sectorSize == 4096 {
		return 257
	}
	return 33
}

// layoutFits is the documented fit rule: 1 MiB before the first partition,
// each fixed size rounded up to 1 MiB, 1 MiB at least for a "rest"
// partition, and 1 MiB kept for the backup GPT. A vfat "rest" partition must
// also be left the FAT32 minimum.
func layoutFits(ds simDiskSpec, parts []genPart) bool {
	need := int64(2)
	for _, gp := range parts {
		switch {
		case !gp.rest:
			need += gp.mibs
		case gp.p.FS == "vfat":
			need += fat32MinMiB(ds.SectorSize)
		default:
			need++
		}
	}
	return need <= ds.Bytes/mib
}

// makeRealizable shrinks a generated partition list in place until the disk
// can hold it: vfat partitions are raised to the FAT32 minimum, then the
// largest partition is cut to its minimum until the layout fits, and on a
// disk too small for FAT32 the vfat partitions become ext4.
func makeRealizable(ds simDiskSpec, parts []genPart) {
	minOf := func(gp genPart) int64 {
		if gp.p.FS == "vfat" {
			return fat32MinMiB(ds.SectorSize)
		}
		return 1
	}
	for {
		for i := range parts {
			if !parts[i].rest && parts[i].p.FS == "vfat" && parts[i].mibs > 0 && parts[i].mibs < minOf(parts[i]) {
				parts[i].mibs = minOf(parts[i])
			}
		}
		if layoutFits(ds, parts) {
			return
		}
		big := -1
		for i, gp := range parts {
			if !gp.rest && gp.mibs > minOf(gp) && (big < 0 || gp.mibs > parts[big].mibs) {
				big = i
			}
		}
		if big >= 0 {
			parts[big].mibs = minOf(parts[big])
			continue
		}
		converted := false
		for i := range parts {
			if parts[i].p.FS == "vfat" {
				parts[i].p.FS, parts[i].p.Bootable = "ext4", false
				if !parts[i].rest {
					parts[i].mibs = 1
				}
				converted = true
				break
			}
		}
		if !converted {
			return
		}
	}
}

func genSimCase(mutate bool) *rapid.Generator[simCase] {
	return rapid.Custom(func(t *rapid.T) simCase {
		k := drawKnobs(t, mutate)
		var sc simCase
		used := map[string]bool{}
		nDisks := rapid.IntRange(1, 6).Draw(t, "nDisks")
		for i := 0; i < nDisks; i++ {
			name, tr := genDiskName(t, used, i)
			ds := simDiskSpec{
				Name:       name,
				Transport:  tr,
				Bytes:      rapid.SampledFrom(simSizes).Draw(t, "bytes"),
				SectorSize: rapid.SampledFrom([]int64{512, 4096}).Draw(t, "sector"),
				Role:       rapid.SampledFrom([]string{"target", "target", "target", "retain", "foreign"}).Draw(t, "role"),
			}
			if i == 0 {
				ds.Role = "target"
			}
			ds.Bytes -= ds.Bytes % ds.SectorSize
			if ds.Bytes < 64*mib {
				ds.Bytes = 64 * mib
			}
			for j, n := 0, rapid.IntRange(0, 3).Draw(t, "nPrior"); j < n; j++ {
				ds.Prior = append(ds.Prior, rapid.SampledFrom([]int64{1, 16, 512}).Draw(t, "priorMiB"))
			}
			for len(ds.Prior) > 0 && !priorFits(ds, ds.Prior) {
				ds.Prior = ds.Prior[:len(ds.Prior)-1]
			}
			sc.Disks = append(sc.Disks, ds)
		}

		devices := map[string]Device{}
		type partRef struct {
			disk string
			idx  int
		}
		var mountable, raw, swaps []string
		var wholeMembers []string
		partNames := map[string]partRef{}
		genParts := map[string][]genPart{}
		for _, ds := range sc.Disks {
			switch ds.Role {
			case "retain":
				devices[ds.Name] = Device{Type: "retain"}
				continue
			case "foreign":
				continue
			}
			n := rapid.IntRange(0, 5).Draw(t, "nParts")
			var parts []genPart
			restIdx := -1
			if n > 0 && rapid.Bool().Draw(t, "hasRest") {
				restIdx = n - 1
				if chance(t, k.restAny, "restAnywhere") {
					restIdx = rapid.IntRange(0, n-1).Draw(t, "restIdx")
					if restIdx != n-1 {
						sc.Mutations = append(sc.Mutations, "rest-not-last")
					}
				}
			}
			for i := 0; i < n; i++ {
				kind := rapid.SampledFrom([]string{"esp", "biosgrub", "fs", "fs", "swap", "raw", "raw"}).Draw(t, "kind")
				gp := genPart{p: Partition{Name: kernelNodeName(ds.Name, i+1)}, rest: i == restIdx}
				switch kind {
				case "esp":
					gp.p.FS, gp.p.Bootable, gp.mibs = "vfat", true, 512
				case "biosgrub":
					gp.p.BIOSGrub, gp.mibs = true, 1
				case "fs":
					gp.p.FS = rapid.SampledFrom([]string{"ext4", "xfs", "vfat"}).Draw(t, "fs")
					gp.mibs = rapid.SampledFrom([]int64{64, 1024, 4096, 102400}).Draw(t, "fsMiB")
				case "swap":
					gp.p.FS, gp.mibs = "swap", rapid.SampledFrom([]int64{1024, 8192}).Draw(t, "swapMiB")
				case "raw":
					gp.mibs = rapid.SampledFrom([]int64{64, 1024, 102400}).Draw(t, "rawMiB")
				}
				if !gp.rest {
					// Script-style "computed" size: everything but 600 MiB.
					// Scripts compute it from the inventory; on a disk under
					// 600 MiB that goes negative, which only the mutating
					// generator may produce.
					if rapid.IntRange(0, 9).Draw(t, "computed") == 0 && ((mutate && k.badSize > 0) || ds.Bytes > 1200*mib) {
						gp.mibs = (ds.Bytes - 600*mib) / mib
						if gp.mibs <= 0 {
							sc.Mutations = append(sc.Mutations, fmt.Sprintf("badsize:computed %dM", gp.mibs))
						}
					}
					gp.useG = rapid.Bool().Draw(t, "useG")
				}
				parts = append(parts, gp)
			}
			makeRealizable(ds, parts)
			if chance(t, k.oversize, "oversize") && len(parts) > 0 {
				i := rapid.IntRange(0, len(parts)-1).Draw(t, "oversizeIdx")
				parts[i].override = fmt.Sprintf("%dM", ds.Bytes/mib+1)
				sc.Mutations = append(sc.Mutations, "oversize")
			}
			if chance(t, k.badSize, "badSize") && len(parts) > 0 {
				i := rapid.IntRange(0, len(parts)-1).Draw(t, "badSizeIdx")
				parts[i].override = rapid.SampledFrom([]string{"512", "1.5G", "512MB", "-1G", "0", "0M", "ten", "100g", "Rest", " 512M", "1:2", "99999999999999999999T", "1K"}).Draw(t, "badSizeVal")
				sc.Mutations = append(sc.Mutations, "badsize:"+parts[i].override)
			}
			if chance(t, k.smallVFAT, "smallVFAT") && len(parts) > 0 {
				i := rapid.IntRange(0, len(parts)-1).Draw(t, "smallVFATIdx")
				if !parts[i].rest && !parts[i].p.BIOSGrub {
					parts[i].p.FS = "vfat"
					parts[i].mibs = rapid.SampledFrom([]int64{1, 16, 32, 33, 256}).Draw(t, "smallVFATMiB")
					sc.Mutations = append(sc.Mutations, fmt.Sprintf("smallvfat:%dM", parts[i].mibs))
				}
			}
			genParts[ds.Name] = parts
		}

		rootDisk := sc.Disks[0]
		hasMountable := false
		for _, parts := range genParts {
			for _, gp := range parts {
				hasMountable = hasMountable || (gp.p.FS != "" && gp.p.FS != "swap")
			}
		}
		if !hasMountable {
			// Guarantee a root: the first target disk gets a trailing ext4
			// partition that takes the rest of the disk.
			parts := genParts[rootDisk.Name]
			for i := range parts {
				if parts[i].rest {
					parts[i].rest, parts[i].mibs = false, 1
				}
			}
			parts = append(parts, genPart{
				p:    Partition{Name: kernelNodeName(rootDisk.Name, len(parts)+1), FS: "ext4"},
				rest: true,
			})
			makeRealizable(rootDisk, parts)
			genParts[rootDisk.Name] = parts
		}

		for _, ds := range sc.Disks {
			if ds.Role != "target" {
				continue
			}
			var parts []Partition
			for i, gp := range genParts[ds.Name] {
				p := gp.p
				p.Size = gp.size()
				parts = append(parts, p)
				partNames[p.Name] = partRef{ds.Name, i}
				switch {
				case p.BIOSGrub:
				case p.FS == "swap":
					swaps = append(swaps, p.Name)
				case p.FS != "":
					mountable = append(mountable, p.Name)
				default:
					raw = append(raw, p.Name)
				}
			}
			if len(parts) == 0 {
				wholeMembers = append(wholeMembers, ds.Name)
			}
			devices[ds.Name] = Device{Type: "disk", PTable: "gpt", Partitions: parts}
		}

		// RAID arrays from raw partitions and bare whole disks.
		pool := append(append([]string{}, raw...), wholeMembers...)
		pool = rapid.Permutation(pool).Draw(t, "raidPool")
		var raidNames []string
		for r, nr := 0, rapid.IntRange(0, 2).Draw(t, "nRaid"); r < nr; r++ {
			level := rapid.SampledFrom([]int{0, 1, 5, 6, 10}).Draw(t, "level")
			need := map[int]int{0: 2, 1: 2, 5: 3, 6: 4, 10: 4}[level]
			if len(pool) < need {
				continue
			}
			cnt := rapid.IntRange(need, len(pool)).Draw(t, "nMembers")
			members := append([]string{}, pool[:cnt]...)
			pool = pool[cnt:]
			name := rapid.SampledFrom([]string{"mdroot", "mddata", "md0", "md1", "md10", "md2"}).Draw(t, "raidName")
			if _, exists := devices[name]; exists {
				continue
			}
			fs := rapid.SampledFrom([]string{"ext4", "xfs", ""}).Draw(t, "raidFS")
			var arrayUUID string
			if rapid.Bool().Draw(t, "raidUUID") {
				arrayUUID = fmt.Sprintf("%08x-0000-4000-8000-%012x", r+1, len(members))
			}
			devices[name] = Device{Type: "raid", Level: level, Members: members, FS: fs, ArrayUUID: arrayUUID}
			raidNames = append(raidNames, name)
			if fs != "" {
				mountable = append(mountable, name)
			}
		}

		// Preserve arrays on foreign disks carrying a prior superblock.
		var foreignIdx []int
		for i, ds := range sc.Disks {
			if ds.Role == "foreign" {
				foreignIdx = append(foreignIdx, i)
			}
		}
		if len(foreignIdx) > 0 && rapid.Bool().Draw(t, "hasPreserve") {
			uuid := "0123456789abcdef0123456789abcdef"
			var members []string
			for _, fi := range foreignIdx {
				ds := &sc.Disks[fi]
				ds.PriorMD = uuid
				if len(ds.Prior) > 0 && rapid.Bool().Draw(t, "preservePartMember") {
					ds.PriorMDPart = 1
					members = append(members, kernelNodeName(ds.Name, 1))
				} else {
					ds.Prior = nil
					members = append(members, ds.Name)
				}
			}
			devices["mdpreserve"] = Device{Type: "preserve", Members: members, ArrayUUID: uuid, FS: "xfs"}
			mountable = append(mountable, "mdpreserve")
			// Collision: a preserve member that the plan would also zap
			// or recreate. Validation must reject it.
			if chance(t, k.preserveCollide, "preserveCollide") {
				var cands []string
				for n, d := range devices {
					if d.Type == "disk" {
						cands = append(cands, n, "/dev/"+n)
						for _, p := range d.Partitions {
							cands = append(cands, p.Name)
						}
					}
					if d.Type == "raid" {
						cands = append(cands, d.Members...)
					}
				}
				sort.Strings(cands)
				if len(cands) > 0 {
					c := rapid.SampledFrom(cands).Draw(t, "preserveCollideMember")
					dev := devices["mdpreserve"]
					dev.Members = append(append([]string{}, dev.Members...), c)
					devices["mdpreserve"] = dev
					sc.Mutations = append(sc.Mutations, "preservecollide:"+c)
					// The colliding member really is part of the preserved
					// array on this machine: give it the superblock, so
					// destroying it is observable.
					bare := strings.TrimPrefix(c, "/dev/")
					for i := range sc.Disks {
						ds := &sc.Disks[i]
						if ds.Name == bare {
							ds.PriorMD, ds.PriorMDPart, ds.Prior = uuid, 0, nil
						}
						for pn := 1; pn <= 6; pn++ {
							if kernelNodeName(ds.Name, pn) == bare {
								for len(ds.Prior) < pn {
									ds.Prior = append(ds.Prior, 1)
								}
								ds.PriorMD, ds.PriorMDPart = uuid, pn
							}
						}
					}
				}
			}
		}

		// Retain collision: a raid member on a retained disk.
		if chance(t, k.retainCollide, "retainCollide") && len(raidNames) > 0 {
			var retained []string
			for n, d := range devices {
				if d.Type == "retain" {
					retained = append(retained, n, kernelNodeName(n, 1), "/dev/"+n)
				}
			}
			sort.Strings(retained)
			if len(retained) > 0 {
				rn := rapid.SampledFrom(raidNames).Draw(t, "retainCollideRaid")
				c := rapid.SampledFrom(retained).Draw(t, "retainCollideMember")
				dev := devices[rn]
				dev.Members = append(append([]string{}, dev.Members...), c)
				devices[rn] = dev
				sc.Mutations = append(sc.Mutations, "retaincollide:"+c)
			}
		}

		// Member abuse: duplicate/shared members, nested raid, partitioned
		// whole-disk member, formatted member.
		if chance(t, k.memberAbuse, "memberAbuse") && len(raidNames) > 0 {
			rn := rapid.SampledFrom(raidNames).Draw(t, "abusedRaid")
			dev := devices[rn]
			var cands []string
			for n := range partNames {
				cands = append(cands, n)
			}
			for n, d := range devices {
				if n != rn && (d.Type == "disk" || d.Type == "raid") {
					cands = append(cands, n)
				}
			}
			sort.Strings(cands)
			if len(cands) > 0 {
				extra := rapid.SampledFrom(cands).Draw(t, "abuseMember")
				if rapid.Bool().Draw(t, "abuseReplace") {
					dev.Members[rapid.IntRange(0, len(dev.Members)-1).Draw(t, "abuseIdx")] = extra
				} else {
					dev.Members = append(dev.Members, extra)
				}
				devices[rn] = dev
				sc.Mutations = append(sc.Mutations, "member:"+extra)
			}
		}

		// Partition name mutation: the declared name no longer equals the
		// kernel node for its position.
		if chance(t, k.partName, "partNameMut") {
			var targets []string
			for n, d := range devices {
				if d.Type == "disk" && len(d.Partitions) > 0 {
					targets = append(targets, n)
				}
			}
			sort.Strings(targets)
			if len(targets) > 0 {
				dn := rapid.SampledFrom(targets).Draw(t, "pnDisk")
				dev := devices[dn]
				parts := append([]Partition{}, dev.Partitions...)
				i := rapid.IntRange(0, len(parts)-1).Draw(t, "pnIdx")
				old := parts[i].Name
				var otherDisks []string
				for _, ds := range sc.Disks {
					if ds.Name != dn {
						otherDisks = append(otherDisks, ds.Name)
					}
				}
				var nn string
				switch rapid.IntRange(0, 3).Draw(t, "pnKind") {
				case 0:
					j := rapid.IntRange(0, len(parts)-1).Draw(t, "pnSwap")
					parts[i].Name, parts[j].Name = parts[j].Name, parts[i].Name
					nn = parts[i].Name
				case 1:
					if len(otherDisks) > 0 {
						nn = kernelNodeName(rapid.SampledFrom(otherDisks).Draw(t, "pnOther"), rapid.IntRange(1, 3).Draw(t, "pnNum"))
					} else {
						nn = old + "x"
					}
					parts[i].Name = nn
				case 2:
					// The separator rule applied backwards: dropped where the
					// kernel adds "p", added where it does not.
					if c := dn[len(dn)-1]; c >= '0' && c <= '9' {
						nn = fmt.Sprintf("%s%d", dn, i+1)
					} else {
						nn = fmt.Sprintf("%sp%d", dn, i+1)
					}
					parts[i].Name = nn
				default:
					nn = kernelNodeName(dn, i+2)
					parts[i].Name = nn
				}
				dev.Partitions = parts
				devices[dn] = dev
				// Re-point references to the old name at the new one so the
				// layout is otherwise self-consistent.
				for n, d := range devices {
					for mi, mem := range d.Members {
						if mem == old {
							d.Members[mi] = nn
						}
					}
					devices[n] = d
				}
				for mi, mn := range mountable {
					if mn == old {
						mountable[mi] = nn
					}
				}
				if old != nn {
					sc.Mutations = append(sc.Mutations, fmt.Sprintf("partname:%s->%s", old, nn))
				}
			}
		}

		// Retain trick: a target-disk partition named like a partition of a
		// retained disk.
		if chance(t, k.retainTrick, "retainTrick") {
			var retained, targets []string
			for n, d := range devices {
				switch {
				case d.Type == "retain":
					retained = append(retained, n)
				case d.Type == "disk" && len(d.Partitions) > 0:
					targets = append(targets, n)
				}
			}
			sort.Strings(retained)
			sort.Strings(targets)
			if len(retained) > 0 && len(targets) > 0 {
				rn := rapid.SampledFrom(retained).Draw(t, "rtRetained")
				dn := rapid.SampledFrom(targets).Draw(t, "rtTarget")
				dev := devices[dn]
				parts := append([]Partition{}, dev.Partitions...)
				i := rapid.IntRange(0, len(parts)-1).Draw(t, "rtIdx")
				old := parts[i].Name
				parts[i].Name = kernelNodeName(rn, 1)
				parts[i].FS = "ext4"
				parts[i].BIOSGrub = false
				dev.Partitions = parts
				devices[dn] = dev
				for n, d := range devices {
					for mi, mem := range d.Members {
						if mem == old {
							d.Members[mi] = parts[i].Name
						}
					}
					devices[n] = d
				}
				filtered := mountable[:0]
				for _, mn := range mountable {
					if mn != old {
						filtered = append(filtered, mn)
					}
				}
				mountable = append(filtered, parts[i].Name)
				sc.Mutations = append(sc.Mutations, "retaintrick:"+parts[i].Name)
			}
		}

		mounts := map[string]Mount{}
		mountable = rapid.Permutation(mountable).Draw(t, "mountOrder")
		mounts["/"] = Mount{Device: mountable[0]}
		paths := []string{"/boot/efi", "/var", "/var/lib", "/data", "/srv", "/home", "/var/lib/docker"}
		if chance(t, k.mountPath, "mountPathMut") {
			paths = append(paths, "/var/", "data", "/var/../etc", "//", "/data//x", "/../x", "/srv/.", "/var//")
		}
		for i, dev := range mountable[1:] {
			if i >= len(paths) || !rapid.Bool().Draw(t, "mountIt") {
				continue
			}
			p := rapid.SampledFrom(paths).Draw(t, "mountPath")
			if _, dup := mounts[p]; dup {
				continue
			}
			mounts[p] = Mount{Device: dev}
		}
		if chance(t, k.mountAbuse, "mountAbuse") {
			var cands []string
			cands = append(cands, swaps...)
			cands = append(cands, raw...)
			cands = append(cands, mountable...)
			var arrays []string
			for n, d := range devices {
				if len(d.Members) > 0 {
					arrays = append(arrays, n)
				}
			}
			sort.Strings(arrays)
			for _, n := range arrays {
				cands = append(cands, devices[n].Members...)
			}
			if len(cands) > 0 {
				dev := rapid.SampledFrom(cands).Draw(t, "mountAbuseDev")
				p := rapid.SampledFrom([]string{"/mnt/a", "/mnt/b", "/swapmnt"}).Draw(t, "mountAbusePath")
				mounts[p] = Mount{Device: dev}
				sc.Mutations = append(sc.Mutations, "mount:"+dev)
			}
		}
		if chance(t, k.missingDisk, "missingDisk") {
			var named []string
			for _, ds := range sc.Disks {
				if d, ok := devices[ds.Name]; ok && (d.Type == "disk" || d.Type == "retain") {
					named = append(named, ds.Name)
				}
			}
			if len(named) > 0 {
				gone := rapid.SampledFrom(named).Draw(t, "missingDiskName")
				sc.Absent = map[string]bool{gone: true}
				sc.Mutations = append(sc.Mutations, "missingdisk:"+gone)
			}
		}
		sc.Layout = Layout{Devices: devices, Mounts: mounts}
		return sc
	})
}
