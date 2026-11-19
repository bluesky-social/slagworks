package storage

// A small simulator of the block layer the storage plan drives: GPT disks
// (sgdisk), the kernel's partition table view (blockdev --rereadpt), mkfs,
// signature clearing (wipefs -a, dd over the boot sector), md arrays
// (mdadm --create/--assemble), directories and the mount table.
// Plan's command list is executed against it and the realized machine is
// compared with the layout's INTENT, computed independently from the Layout
// structure: a partition declared at position i under disk D is physical
// partition (D, i+1); a raid's members are the physical devices its member
// names declare; a mount path is the absolute target path. Nothing in this
// file calls a production helper (partName, resolveDevPath, buildMDDevMap,
// partitionTypeCode, mkfsCommand...): the kernel naming rule, the sgdisk
// size grammar and the GPT type codes are restated from their own
// specifications.

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// blk identifies a physical block device: a whole disk (part 0), a
// partition of one, or an md array (md != "").
type blk struct {
	disk string
	part int
	md   string
}

func (b blk) String() string {
	switch {
	case b.md != "":
		return b.md
	case b.part == 0:
		return b.disk
	default:
		return fmt.Sprintf("%s#%d", b.disk, b.part)
	}
}

type simPart struct {
	num        int
	start, end int64 // inclusive sectors
	code       string
	label      string
}

type simDisk struct {
	name       string
	sectorSize int64
	sectors    int64
	table      map[int]*simPart // on-disk GPT
	kernel     map[int]bool     // partitions the kernel currently exposes
}

// A GPT keeps a protective MBR and a header at the front, a header at the
// back, and a 128-entry (16 KiB) partition array next to each header.
func (d *simDisk) entrySectors() int64 { return (16384 + d.sectorSize - 1) / d.sectorSize }
func (d *simDisk) firstUsable() int64  { return 2 + d.entrySectors() }
func (d *simDisk) lastUsable() int64   { return d.sectors - 2 - d.entrySectors() }
func (d *simDisk) align() int64        { return (1 << 20) / d.sectorSize }

// content is what a block device carries.
type content struct {
	fs       string // vfat ext4 xfs swap, "" for none
	fsGen    int    // distinguishes successive mkfs on the same device
	mdUUID   string // md superblock (member of an array with this uuid)
	corrupt  bool   // overwritten by a whole-disk consumer
	preserve bool   // pre-existing data that must survive
}

type simMD struct {
	path    string
	level   int
	members []blk
	uuid    string
	created bool // false: assembled from existing superblocks
}

type simMount struct {
	point string // effective (kernel-resolved) mount point
	dev   blk
	fsKey string // filesystem identity (dev + generation)
}

type simMachine struct {
	disks    map[string]*simDisk
	contents map[blk]*content
	mds      map[string]*simMD
	dirs     map[string]bool // fsKey + "\x00" + relative path
	mounts   []simMount      // visible mount stack, in mount order
	writes   map[string]bool // physical disks written by any command
	mounted  map[blk]int
	gen      int
	trace    []string
	// preservedFS is the filesystem each pre-existing array (by uuid)
	// carries, surfaced when it is assembled.
	preservedFS map[string]content
	// shadowed are mounts hidden by a later mount at or above them.
	shadowed []simMount
	// initial snapshots every device's content before the plan runs.
	initial map[blk]content
	// stale holds, per disk and start sector, what a partition carried when
	// its table entry was deleted. sgdisk rewrites tables only, so a new
	// partition starting at the same sector shows those bytes again.
	stale map[string]map[int64]content
}

func (m *simMachine) snapshot() {
	m.initial = map[blk]content{}
	for b, c := range m.contents {
		m.initial[b] = *c
	}
}

func newSimMachine() *simMachine {
	return &simMachine{
		disks:    map[string]*simDisk{},
		contents: map[blk]*content{},
		mds:      map[string]*simMD{},
		dirs:     map[string]bool{},
		writes:   map[string]bool{},
		mounted:  map[blk]int{},

		preservedFS: map[string]content{},
		stale:       map[string]map[int64]content{},
	}
}

// kernelNodeName is the Linux partition node naming rule
// (block/partitions/core.c): "p" separates the number only when the disk
// name itself ends in a digit.
func kernelNodeName(disk string, num int) string {
	if c := disk[len(disk)-1]; c >= '0' && c <= '9' {
		return disk + "p" + strconv.Itoa(num)
	}
	return disk + strconv.Itoa(num)
}

func (m *simMachine) addDisk(name string, sectorSize, bytes int64) *simDisk {
	d := &simDisk{name: name, sectorSize: sectorSize, sectors: bytes / sectorSize,
		table: map[int]*simPart{}, kernel: map[int]bool{}}
	m.disks[name] = d
	return d
}

// resolve maps a /dev path to the device the kernel would open.
func (m *simMachine) resolve(p string) (blk, bool) {
	if !strings.HasPrefix(p, "/dev/") {
		return blk{}, false
	}
	name := strings.TrimPrefix(p, "/dev/")
	if md, ok := m.mds[p]; ok {
		return blk{md: md.path}, true
	}
	if _, ok := m.disks[name]; ok {
		return blk{disk: name}, true
	}
	for _, d := range m.disks {
		for num := range d.kernel {
			if kernelNodeName(d.name, num) == name {
				return blk{disk: d.name, part: num}, true
			}
		}
	}
	return blk{}, false
}

func (m *simMachine) content(b blk) *content {
	c, ok := m.contents[b]
	if !ok {
		c = &content{}
		m.contents[b] = c
	}
	return c
}

// physicalDisks returns the whole disks a device's bytes live on.
func (m *simMachine) physicalDisks(b blk) []string {
	if b.md != "" {
		var out []string
		for _, mem := range m.mds[b.md].members {
			out = append(out, m.physicalDisks(mem)...)
		}
		return out
	}
	return []string{b.disk}
}

func (m *simMachine) markWrite(b blk) {
	for _, d := range m.physicalDisks(b) {
		m.writes[d] = true
	}
}

// busy reports whether a device is held open: mounted, an active md member,
// or a whole disk one of whose kernel partitions is held.
func (m *simMachine) busy(b blk) bool {
	if m.mounted[b] > 0 {
		return true
	}
	for _, md := range m.mds {
		for _, mem := range md.members {
			if mem == b {
				return true
			}
		}
	}
	if b.md == "" && b.part == 0 {
		for num := range m.disks[b.disk].kernel {
			if m.busy(blk{disk: b.disk, part: num}) {
				return true
			}
		}
	}
	return false
}

// clobberChildren marks every partition of a whole disk corrupt: a whole-
// disk consumer (mkfs, md superblock + data area) overwrites them.
func (m *simMachine) clobberChildren(b blk) {
	if b.md != "" || b.part != 0 {
		return
	}
	d := m.disks[b.disk]
	for num := range d.table {
		m.content(blk{disk: b.disk, part: num}).corrupt = true
	}
}

// sgdiskSectors parses sgdisk's size grammar: an unsigned integer with an
// optional K/M/G/T/P binary suffix (any case); no suffix means sectors.
func sgdiskSectors(s string, sectorSize int64) (int64, bool) {
	if s == "" {
		return 0, false
	}
	mult := int64(0)
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	case "P":
		mult = 1 << 50
	}
	num := s
	if mult != 0 {
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(num, 10, 63)
	if err != nil {
		return 0, false
	}
	if mult == 0 {
		return int64(n), true
	}
	if n > uint64((1<<62)/mult) {
		return 0, false
	}
	return int64(n) * mult / sectorSize, true
}

// freeBlocks returns the free extents of a disk's GPT, in sector order.
func (d *simDisk) freeBlocks() [][2]int64 {
	var used [][2]int64
	for _, p := range d.table {
		used = append(used, [2]int64{p.start, p.end})
	}
	sort.Slice(used, func(i, j int) bool { return used[i][0] < used[j][0] })
	var free [][2]int64
	cur := d.firstUsable()
	for _, u := range used {
		if u[0] > cur {
			free = append(free, [2]int64{cur, u[0] - 1})
		}
		if u[1]+1 > cur {
			cur = u[1] + 1
		}
	}
	if cur <= d.lastUsable() {
		free = append(free, [2]int64{cur, d.lastUsable()})
	}
	return free
}

type simErr struct {
	idx    int
	cmd    string
	reason string
}

func (e *simErr) Error() string { return fmt.Sprintf("cmd[%d] %s: %s", e.idx, e.cmd, e.reason) }

func (m *simMachine) fail(idx int, c Command, format string, args ...any) error {
	return &simErr{idx: idx, cmd: c.String(), reason: fmt.Sprintf(format, args...)}
}

func (m *simMachine) fsKey(b blk) string {
	return fmt.Sprintf("%s@%d", b, m.content(b).fsGen)
}

// lookupDir resolves an absolute path through the visible mount stack:
// the filesystem it lands on and the path relative to that filesystem.
func (m *simMachine) lookupDir(p string) (fsKey, rel string) {
	fsKey, rel = "agentroot", p
	best := -1
	for _, mt := range m.mounts {
		if p == mt.point || strings.HasPrefix(p, strings.TrimRight(mt.point, "/")+"/") {
			if len(mt.point) > best {
				best = len(mt.point)
				fsKey = mt.fsKey
				rel = strings.TrimPrefix(p, mt.point)
			}
		}
	}
	rel = "/" + strings.Trim(rel, "/")
	return fsKey, rel
}

func (m *simMachine) dirExists(p string) bool {
	fs, rel := m.lookupDir(p)
	return rel == "/" || m.dirs[fs+"\x00"+rel]
}

func (m *simMachine) mkdirAll(p string) {
	for q := p; q != "/" && q != "."; q = path.Dir(q) {
		fs, rel := m.lookupDir(q)
		m.dirs[fs+"\x00"+rel] = true
	}
}

// exec runs one plan command against the machine.
func (m *simMachine) exec(idx int, c Command) error {
	m.trace = append(m.trace, c.String())
	a := c.Args
	switch c.Name {
	case "sgdisk":
		if len(a) == 2 && a[0] == "--zap-all" {
			b, ok := m.resolve(a[1])
			if !ok || b.md != "" || b.part != 0 {
				return m.fail(idx, c, "zap target %q is not a whole disk", a[1])
			}
			m.markWrite(b)
			// The partitions' bytes stay where they were (see stale), but
			// their nodes are gone and the plan repartitions over them, so
			// the model counts what they held as lost: a plan that zaps a
			// disk holding data it must keep has already failed.
			d := m.disks[b.disk]
			if m.stale[d.name] == nil {
				m.stale[d.name] = map[int64]content{}
			}
			for num, p := range d.table {
				if c, ok := m.contents[blk{disk: d.name, part: num}]; ok && !c.corrupt {
					m.stale[d.name][p.start] = content{fs: c.fs, fsGen: c.fsGen, mdUUID: c.mdUUID}
				}
			}
			m.clobberChildren(b)
			d.table = map[int]*simPart{}
			return nil
		}
		if len(a) != 7 || a[0] != "-n" || a[2] != "-t" || a[4] != "-c" {
			return m.fail(idx, c, "unmodelled sgdisk invocation")
		}
		b, ok := m.resolve(a[6])
		if !ok || b.md != "" || b.part != 0 {
			return m.fail(idx, c, "sgdisk target %q is not a whole disk", a[6])
		}
		d := m.disks[b.disk]
		spec := strings.SplitN(a[1], ":", 3)
		if len(spec) != 3 || strings.Contains(spec[2], ":") {
			return m.fail(idx, c, "malformed -n spec %q", a[1])
		}
		num, err := strconv.Atoi(spec[0])
		if err != nil || num < 1 || num > 128 {
			return m.fail(idx, c, "bad partition number %q", spec[0])
		}
		if _, dup := d.table[num]; dup {
			return m.fail(idx, c, "partition %d already exists", num)
		}
		if spec[1] != "0" {
			return m.fail(idx, c, "unmodelled explicit start %q", spec[1])
		}
		free := d.freeBlocks()
		if len(free) == 0 {
			return m.fail(idx, c, "no free space on %s", d.name)
		}
		largest := free[0]
		for _, f := range free[1:] {
			if f[1]-f[0] > largest[1]-largest[0] {
				largest = f
			}
		}
		start := (largest[0] + d.align() - 1) / d.align() * d.align()
		if start > largest[1] {
			return m.fail(idx, c, "largest free block on %s too small to align", d.name)
		}
		var end int64
		switch {
		case spec[2] == "0":
			end = largest[1]
		case strings.HasPrefix(spec[2], "+"):
			n, ok := sgdiskSectors(spec[2][1:], d.sectorSize)
			if !ok {
				return m.fail(idx, c, "unparseable size %q", spec[2])
			}
			if n == 0 { // sgdisk: a zero size means the default end
				end = largest[1]
			} else {
				end = start + n - 1
			}
		default:
			return m.fail(idx, c, "unmodelled end %q", spec[2])
		}
		if end > largest[1] || end < start {
			return m.fail(idx, c, "partition %d [%d,%d] does not fit free block [%d,%d] on %s", num, start, end, largest[0], largest[1], d.name)
		}
		tn := strings.SplitN(a[3], ":", 2)
		cn := strings.SplitN(a[5], ":", 2)
		if len(tn) != 2 || tn[0] != spec[0] || len(cn) != 2 || cn[0] != spec[0] {
			return m.fail(idx, c, "type/name partition numbers disagree")
		}
		d.table[num] = &simPart{num: num, start: start, end: end, code: tn[1], label: cn[1]}
		m.markWrite(b)
		// A new table entry does not touch the partition's bytes.
		old := m.stale[d.name][start]
		m.contents[blk{disk: d.name, part: num}] = &old
		return nil

	case "blockdev":
		if len(a) != 2 || a[0] != "--rereadpt" {
			return m.fail(idx, c, "unmodelled blockdev")
		}
		b, ok := m.resolve(a[1])
		if !ok || b.md != "" || b.part != 0 {
			return m.fail(idx, c, "rereadpt target %q is not a whole disk", a[1])
		}
		d := m.disks[b.disk]
		for num := range d.kernel {
			if m.busy(blk{disk: d.name, part: num}) {
				return m.fail(idx, c, "EBUSY: partition %d of %s in use", num, d.name)
			}
		}
		d.kernel = map[int]bool{}
		for num := range d.table {
			d.kernel[num] = true
		}
		return nil

	case "mkfs.fat", "mkfs.ext4", "mkfs.xfs", "mkswap":
		fs := map[string]string{"mkfs.fat": "vfat", "mkfs.ext4": "ext4", "mkfs.xfs": "xfs", "mkswap": "swap"}[c.Name]
		if len(a) == 0 {
			return m.fail(idx, c, "no device")
		}
		b, ok := m.resolve(a[len(a)-1])
		if !ok {
			return m.fail(idx, c, "no such device %s", a[len(a)-1])
		}
		if m.busy(b) {
			return m.fail(idx, c, "device %s in use", b)
		}
		m.markWrite(b)
		m.clobberChildren(b)
		m.gen++
		cc := m.content(b)
		*cc = content{fs: fs, fsGen: m.gen}
		return nil

	case "wipefs":
		// wipefs -a erases every signature libblkid knows. It opens the
		// device O_EXCL, so a held device fails instead of being written.
		if len(a) != 2 || a[0] != "-a" {
			return m.fail(idx, c, "unmodelled wipefs")
		}
		b, ok := m.resolve(a[1])
		if !ok {
			return m.fail(idx, c, "no such device %s", a[1])
		}
		if b.md == "" && b.part == 0 {
			return m.fail(idx, c, "unmodelled wipefs of whole disk %s", b)
		}
		if m.busy(b) {
			return m.fail(idx, c, "device %s in use", b)
		}
		m.markWrite(b)
		*m.content(b) = content{}
		return nil

	case "dd":
		// The plan's only dd zeroes a device's first 4 KiB: its boot sector
		// and any filesystem superblock there. An md 1.2 superblock sits 4 KiB
		// in and survives. A path that is no device would be created as a
		// regular file, which the plan never means.
		if len(a) != 5 || a[0] != "if=/dev/zero" || !strings.HasPrefix(a[1], "of=") ||
			a[2] != "bs=4096" || a[3] != "count=1" || a[4] != "conv=fsync" {
			return m.fail(idx, c, "unmodelled dd")
		}
		b, ok := m.resolve(strings.TrimPrefix(a[1], "of="))
		if !ok {
			return m.fail(idx, c, "no such device %s", a[1])
		}
		if b.md != "" || b.part == 0 {
			return m.fail(idx, c, "unmodelled dd target %s", b)
		}
		m.markWrite(b)
		cc := m.content(b)
		*cc = content{mdUUID: cc.mdUUID}
		return nil

	case "mdadm":
		if len(a) >= 2 && a[0] == "--create" {
			mdPath := a[1]
			if _, active := m.mds[mdPath]; active {
				return m.fail(idx, c, "%s already active", mdPath)
			}
			var level, ndev int
			var uuid string
			var members []string
			for i := 2; i < len(a); i++ {
				switch {
				case strings.HasPrefix(a[i], "--level="):
					level, _ = strconv.Atoi(strings.TrimPrefix(a[i], "--level="))
				case strings.HasPrefix(a[i], "--raid-devices="):
					ndev, _ = strconv.Atoi(strings.TrimPrefix(a[i], "--raid-devices="))
				case a[i] == "--uuid":
					i++
					uuid = a[i]
				case strings.HasPrefix(a[i], "--"):
				default:
					members = append(members, a[i])
				}
			}
			if ndev != len(members) {
				return m.fail(idx, c, "raid-devices=%d but %d members", ndev, len(members))
			}
			var mems []blk
			seen := map[blk]bool{}
			for _, mp := range members {
				b, ok := m.resolve(mp)
				if !ok {
					return m.fail(idx, c, "member %s does not exist", mp)
				}
				if seen[b] {
					return m.fail(idx, c, "member %s listed twice", mp)
				}
				seen[b] = true
				if m.busy(b) {
					return m.fail(idx, c, "member %s in use", mp)
				}
				mems = append(mems, b)
			}
			// mdadm opens every member O_EXCL: a whole disk cannot be
			// claimed alongside one of its own partitions.
			for _, x := range mems {
				for _, y := range mems {
					if x.md == "" && y.md == "" && x.part == 0 && y.part != 0 && x.disk == y.disk {
						return m.fail(idx, c, "EBUSY: %s and its partition %s both members", x, y)
					}
				}
			}
			if uuid == "" {
				m.gen++
				uuid = fmt.Sprintf("generated-%d", m.gen)
			}
			for _, b := range mems {
				m.markWrite(b)
				m.clobberChildren(b)
				*m.content(b) = content{mdUUID: uuid}
			}
			m.mds[mdPath] = &simMD{path: mdPath, level: level, members: mems, uuid: uuid, created: true}
			*m.content(blk{md: mdPath}) = content{}
			return nil
		}
		if len(a) >= 4 && a[0] == "--assemble" && a[2] == "--uuid" {
			mdPath, uuid := a[1], a[3]
			if _, active := m.mds[mdPath]; active {
				return m.fail(idx, c, "%s already active", mdPath)
			}
			var mems []blk
			for _, mp := range a[4:] {
				b, ok := m.resolve(mp)
				if !ok {
					return m.fail(idx, c, "member %s does not exist", mp)
				}
				if m.content(b).mdUUID != uuid || m.content(b).corrupt {
					return m.fail(idx, c, "member %s carries no superblock for %s", mp, uuid)
				}
				if m.busy(b) {
					return m.fail(idx, c, "member %s in use", mp)
				}
				mems = append(mems, b)
			}
			m.mds[mdPath] = &simMD{path: mdPath, members: mems, uuid: uuid}
			if _, ok := m.contents[blk{md: mdPath}]; !ok {
				*m.content(blk{md: mdPath}) = m.preservedFS[uuid]
			}
			return nil
		}
		return m.fail(idx, c, "unmodelled mdadm")

	case "mkdir":
		if len(a) != 2 || a[0] != "-p" {
			return m.fail(idx, c, "unmodelled mkdir")
		}
		m.mkdirAll(path.Clean(a[1]))
		return nil

	case "mount":
		if len(a) != 2 {
			return m.fail(idx, c, "unmodelled mount")
		}
		b, ok := m.resolve(a[0])
		if !ok {
			return m.fail(idx, c, "no such device %s", a[0])
		}
		cc := m.content(b)
		if cc.corrupt || cc.fs == "" || cc.fs == "swap" || cc.mdUUID != "" {
			return m.fail(idx, c, "%s has no mountable filesystem (fs=%q corrupt=%v md=%q)", b, cc.fs, cc.corrupt, cc.mdUUID)
		}
		for _, md := range m.mds {
			for _, mem := range md.members {
				if mem == b {
					return m.fail(idx, c, "%s is an active md member", b)
				}
			}
		}
		point := path.Clean(a[1])
		if !strings.HasPrefix(a[1], "/") {
			return m.fail(idx, c, "relative mount point %q", a[1])
		}
		if !m.dirExists(point) {
			return m.fail(idx, c, "mount point %s does not exist", point)
		}
		// A mount hides whatever was visible at or below its mount point.
		kept := m.mounts[:0:0]
		for _, mt := range m.mounts {
			if mt.point == point || strings.HasPrefix(mt.point, strings.TrimRight(point, "/")+"/") {
				m.shadowed = append(m.shadowed, mt) // hidden but still held open
				continue
			}
			kept = append(kept, mt)
		}
		m.mounts = append(kept, simMount{point: point, dev: b, fsKey: m.fsKey(b)})
		m.mounted[b]++
		return nil
	}
	return m.fail(idx, c, "unmodelled command")
}
