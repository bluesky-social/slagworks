package storage

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// intent is the layout's meaning, derived from its structure alone.
type intent struct {
	layout   Layout
	targets  map[string]bool
	retained map[string]bool
	// decl maps a partition name to every (disk, position) declaring it.
	decl map[string][]blk
}

func newIntent(l Layout) intent {
	in := intent{layout: l, targets: map[string]bool{}, retained: map[string]bool{}, decl: map[string][]blk{}}
	for name, d := range l.Devices {
		switch d.Type {
		case "disk":
			in.targets[name] = true
			for i, p := range d.Partitions {
				in.decl[p.Name] = append(in.decl[p.Name], blk{disk: name, part: i + 1})
			}
		case "retain":
			in.retained[name] = true
		}
	}
	return in
}

// physical resolves a layout name to the physical device the layout means.
// ok=false for names that denote no single physical device (raid/preserve
// names, unknown or ambiguous names).
func (in intent) physical(name string) (blk, bool) {
	if d, ok := in.layout.Devices[name]; ok {
		if d.Type == "disk" || d.Type == "retain" {
			return blk{disk: name}, true
		}
		return blk{}, false
	}
	if ds := in.decl[name]; len(ds) == 1 {
		return ds[0], true
	}
	return blk{}, false
}

// wellFormedMountPath: absolute, lexically clean (a single trailing slash
// tolerated), no "." or ".." segments.
func wellFormedMountPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	if p == "/" {
		return true
	}
	q := strings.TrimSuffix(p, "/")
	return path.Clean(q) == q
}

func expectedTypeCode(p Partition) string {
	switch {
	case p.BIOSGrub:
		return "EF02"
	case p.Bootable:
		return "EF00"
	case p.FS == "swap":
		return "8200"
	}
	return "8300"
}

type violation struct {
	cat    string
	detail string
}

// check compares the realized machine with the layout intent.
func (in intent) check(sc simCase, m *simMachine) []violation {
	var vs []violation
	add := func(cat, f string, a ...any) { vs = append(vs, violation{cat, fmt.Sprintf(f, a...)}) }

	for d := range m.writes {
		if !in.targets[d] {
			add("write-outside-targets", "disk %s was written but is not a disk-type layout target", d)
		}
	}
	// Pre-existing data must survive on every non-target disk.
	for b, c := range m.contents {
		if b.md == "" && !in.targets[b.disk] && (c.corrupt || (!c.preserve && (c.fs != "" || c.mdUUID != ""))) {
			add("foreign-data-changed", "%s on non-target disk now %+v", b, *c)
		}
	}

	// Partition tables.
	for name := range in.targets {
		dev := in.layout.Devices[name]
		d := m.disks[name]
		if len(d.table) != len(dev.Partitions) {
			add("partition-table", "%s has %d partitions, layout declares %d", name, len(d.table), len(dev.Partitions))
			continue
		}
		var prevEnd int64 = -1
		for i, p := range dev.Partitions {
			sp := d.table[i+1]
			if sp == nil {
				add("partition-table", "%s partition %d missing", name, i+1)
				continue
			}
			if sp.label != p.Name {
				add("partition-table", "%s#%d label %q, want %q", name, i+1, sp.label, p.Name)
			}
			if sp.code != expectedTypeCode(p) {
				add("partition-table", "%s#%d type %s, want %s", name, i+1, sp.code, expectedTypeCode(p))
			}
			if sp.start < d.firstUsable() || sp.end > d.lastUsable() || sp.start > sp.end {
				add("partition-bounds", "%s#%d [%d,%d] outside usable [%d,%d]", name, i+1, sp.start, sp.end, d.firstUsable(), d.lastUsable())
			}
			if sp.start%d.align() != 0 {
				add("partition-align", "%s#%d start %d not 1MiB aligned", name, i+1, sp.start)
			}
			if sp.start <= prevEnd {
				add("partition-overlap", "%s#%d starts at %d, previous ends at %d", name, i+1, sp.start, prevEnd)
			}
			prevEnd = sp.end
			bytes := (sp.end - sp.start + 1) * d.sectorSize
			if p.FS == "vfat" && bytes < fat32MinMiB(d.sectorSize)*mib {
				add("fat32-too-small", "%s#%d is a %d-byte vfat partition, under the FAT32 minimum at %d-byte sectors", name, i+1, bytes, d.sectorSize)
			}
			if p.Size == "rest" {
				if sp.end != d.lastUsable() {
					add("partition-size", "%s#%d rest ends at %d, want last usable %d", name, i+1, sp.end, d.lastUsable())
				}
			} else if want, ok := intendedBytes(p.Size); !ok {
				add("size-grammar", "%s#%d size %q is outside the documented grammar but was realized as %d bytes", name, i+1, p.Size, bytes)
			} else if want != bytes {
				add("partition-size", "%s#%d is %d bytes, want %d (%s)", name, i+1, bytes, want, p.Size)
			}
		}
	}

	// Arrays: the md realized for each raid has exactly the intended members.
	mdFor := map[string]*simMD{}
	for name, dev := range in.layout.Devices {
		if dev.Type != "raid" && dev.Type != "preserve" {
			continue
		}
		var want []blk
		resolvable := true
		for _, mem := range dev.Members {
			if dev.Type == "preserve" {
				b, ok := in.preserveMember(sc, mem)
				if !ok {
					resolvable = false
					break
				}
				want = append(want, b)
				continue
			}
			b, ok := in.physical(mem)
			if !ok {
				resolvable = false
				break
			}
			want = append(want, b)
		}
		if !resolvable {
			add("raid-intent", "%s members %v do not denote physical devices", name, dev.Members)
			continue
		}
		var found *simMD
		for _, md := range m.mds {
			if sameBlks(md.members, want) {
				found = md
			}
		}
		if found == nil {
			add("raid-members", "no active array built from %s's intended members %v", name, want)
			continue
		}
		if dev.Type == "raid" && (!found.created || found.level != dev.Level || (dev.ArrayUUID != "" && found.uuid != dev.ArrayUUID)) {
			add("raid-identity", "%s realized as %+v", name, *found)
		}
		if dev.Type == "preserve" && (found.created || found.uuid != dev.ArrayUUID) {
			add("preserve", "%s realized as %+v", name, *found)
		}
		mdFor[name] = found
		if dev.FS != "" && m.content(blk{md: found.path}).fs != dev.FS {
			add("raid-fs", "%s fs %q, want %q", name, m.content(blk{md: found.path}).fs, dev.FS)
		}
	}

	// Partition contents (only where the layout leaves the partition for
	// its own filesystem: raid members carry a superblock instead).
	members := map[blk]bool{}
	for _, dev := range in.layout.Devices {
		if dev.Type == "raid" {
			for _, mem := range dev.Members {
				if b, ok := in.physical(mem); ok {
					members[b] = true
				}
			}
		}
	}
	for name := range in.targets {
		for i, p := range in.layout.Devices[name].Partitions {
			b := blk{disk: name, part: i + 1}
			if members[b] {
				continue
			}
			if c := m.content(b); p.FS == "" {
				// An unformatted partition must not show what an earlier
				// layout left at its offset.
				if c.fs != "" || c.mdUUID != "" {
					add("stale-signature", "%s is declared unformatted but carries %+v from before the deploy", b, *c)
				}
			} else if c.fs != p.FS || c.corrupt {
				add("partition-content", "%s carries %+v, layout declares fs %q", b, *c, p.FS)
			}
		}
	}

	// Mounts: the visible mount table equals the intended one.
	visible := map[string]simMount{}
	for _, mt := range m.mounts {
		visible[mt.point] = mt
		if mt.point != "/target" && !strings.HasPrefix(mt.point, "/target/") {
			add("mount-escape", "%s mounted at %s, outside /target", mt.dev, mt.point)
		}
	}
	for p, mnt := range in.layout.Mounts {
		if !wellFormedMountPath(p) {
			add("mount-path", "malformed mount path %q accepted", p)
			continue
		}
		point := "/target"
		if p != "/" {
			point += strings.TrimSuffix(p, "/")
		}
		mt, ok := visible[point]
		if !ok {
			add("mount-missing", "nothing visible at %s (layout mounts %s there)", point, mnt.Device)
			continue
		}
		var want blk
		var wantFS string
		if md, ok := mdFor[mnt.Device]; ok {
			want, wantFS = blk{md: md.path}, in.layout.Devices[mnt.Device].FS
		} else if b, ok := in.physical(mnt.Device); ok {
			want = b
			if b.part > 0 {
				wantFS = in.layout.Devices[b.disk].Partitions[b.part-1].FS
			}
		} else {
			add("mount-intent", "mount %s device %q denotes nothing", p, mnt.Device)
			continue
		}
		if mt.dev != want {
			add("mount-device", "%s has %s mounted, layout means %s", point, mt.dev, want)
		}
		if got := m.content(mt.dev).fs; got != wantFS {
			add("mount-fs", "%s has fs %q mounted, layout declares %q", point, got, wantFS)
		}
		for _, d := range m.physicalDisks(mt.dev) {
			assembled := mt.dev.md != "" && !m.mds[mt.dev.md].created
			if !in.targets[d] && !assembled {
				add("write-outside-targets", "%s (on non-target disk %s) is mounted at %s and will receive the OS image", mt.dev, d, point)
			}
		}
	}
	return vs
}

// protectedIntact reports destroyed protected data: a pre-existing
// superblock or filesystem the run overwrote on a disk the layout does not
// target, or a preserve array's member superblock anywhere.
func (in intent) protectedIntact(m *simMachine) []violation {
	var vs []violation
	preserveUUIDs := map[string]bool{}
	for _, d := range in.layout.Devices {
		if d.Type == "preserve" {
			preserveUUIDs[d.ArrayUUID] = true
		}
	}
	for b, c := range m.initial {
		now := m.content(b)
		if c.mdUUID != "" && preserveUUIDs[c.mdUUID] && (now.mdUUID != c.mdUUID || now.corrupt) {
			vs = append(vs, violation{"preserve-destroyed", fmt.Sprintf("preserve member %s superblock %s destroyed (now %+v)", b, c.mdUUID, *now)})
		}
		if !in.targets[b.disk] && (*now != c) {
			vs = append(vs, violation{"foreign-data-changed", fmt.Sprintf("%s on non-target disk changed from %+v to %+v", b, c, *now)})
		}
	}
	for d := range m.writes {
		if in.retained[d] {
			vs = append(vs, violation{"retained-written", "retained disk " + d + " was written"})
		}
	}
	return vs
}

// preserveMember resolves a preserve member (a kernel name of a disk or
// partition outside the layout) to the physical device of the generated
// machine it names.
func (in intent) preserveMember(sc simCase, name string) (blk, bool) {
	name = strings.TrimPrefix(name, "/dev/")
	for _, ds := range sc.Disks {
		if ds.Name == name {
			return blk{disk: name}, true
		}
		for i := range ds.Prior {
			if kernelNodeName(ds.Name, i+1) == name {
				return blk{disk: ds.Name, part: i + 1}, true
			}
		}
	}
	return blk{}, false
}

func sameBlks(a, b []blk) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]blk{}, a...)
	bs := append([]blk{}, b...)
	less := func(s []blk) func(i, j int) bool {
		return func(i, j int) bool { return s[i].String() < s[j].String() }
	}
	sort.Slice(as, less(as))
	sort.Slice(bs, less(bs))
	return reflect.DeepEqual(as, bs)
}

// intendedBytes is the documented size grammar ("512M", "100G"): a positive
// integer with a binary K/M/G/T suffix in either case.
func intendedBytes(s string) (int64, bool) {
	if len(s) < 2 {
		return 0, false
	}
	mult := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40,
		'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30, 't': 1 << 40}[s[len(s)-1]]
	if mult == 0 {
		return 0, false
	}
	var n int64
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n > (1<<62)/mult {
			return 0, false
		}
	}
	if n == 0 {
		return 0, false
	}
	return n * mult, true
}

// runSim executes cmds; it returns the simulator error (nil when every
// command succeeded) and the index of the first destructive command.
func runSim(m *simMachine, cmds []Command) (*simErr, int) {
	firstWrite := -1
	for i, c := range cmds {
		before := len(m.writes)
		if err := m.exec(i, c); err != nil {
			var se *simErr
			errors.As(err, &se)
			return se, firstWrite
		}
		if firstWrite < 0 && len(m.writes) > before {
			firstWrite = i
		}
	}
	return nil, firstWrite
}

type simStats struct {
	mu         sync.Mutex
	cases      int
	rejected   map[string]int
	applyFail  map[string]int
	ok         int
	mutAccepts map[string]int
	maxParts   int
	maxRaids   int
	sizes      map[string]int
}

func newSimStats() *simStats {
	return &simStats{rejected: map[string]int{}, applyFail: map[string]int{}, mutAccepts: map[string]int{}, sizes: map[string]int{}}
}

var bucketRE = regexp.MustCompile(`"[^"]*"|\[[^\]]*\]|[0-9]+|/dev/[^ ]+|(nvme|sd|vd|mmcblk)[a-z0-9]*`)

// bucket collapses names and numbers so similar messages tally together.
func bucket(s string) string { return bucketRE.ReplaceAllString(s, "_") }

func firstWord(s string) string {
	if i := strings.IndexAny(s, ":("); i > 0 {
		return s[:i]
	}
	return s
}

func (st *simStats) report(t *testing.T) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t.Logf("cases=%d accepted+realized=%d maxParts=%d maxRaids=%d", st.cases, st.ok, st.maxParts, st.maxRaids)
	for _, kv := range []struct {
		name string
		m    map[string]int
	}{{"rejected", st.rejected}, {"apply-fail-after-destruction", st.applyFail}, {"mutation-accepted", st.mutAccepts}, {"sizes", st.sizes}} {
		keys := make([]string, 0, len(kv.m))
		for k := range kv.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("  %s %-60s %d", kv.name, k, kv.m[k])
		}
	}
}

func logCase(rt *rapid.T, sc simCase, cmds []Command, m *simMachine) {
	rt.Logf("mutations: %v", sc.Mutations)
	for _, d := range sc.Disks {
		rt.Logf("disk %+v", d)
	}
	rt.Logf("layout: %s", mustJSON(sc.Layout))
	for i, c := range cmds {
		rt.Logf("cmd[%d] %s", i, c)
	}
}

// TestPlanSim_Property_RealizableLayoutsRealize: every structurally valid,
// size-feasible layout is accepted by Plan, and executing the plan yields
// exactly the intended machine: right partitions (sizes, alignment, labels,
// type codes) on the right disks, arrays from the right members, the right
// filesystem visible at every mount point, and no write outside the
// layout's disk targets.
func TestPlanSim_Property_RealizableLayoutsRealize(t *testing.T) {
	st := newSimStats()
	rapid.Check(t, func(rt *rapid.T) {
		sc := genSimCase(false).Draw(rt, "case")
		cmds, err := Plan(sc.Layout, sc.inventory())
		st.mu.Lock()
		st.cases++
		st.mu.Unlock()
		if err != nil {
			logCase(rt, sc, nil, nil)
			rt.Fatalf("Plan rejected a realizable layout: %v", err)
		}
		m := sc.machine()
		if se, _ := runSim(m, cmds); se != nil {
			logCase(rt, sc, cmds, m)
			rt.Fatalf("plan for a realizable layout fails when applied: %v", se)
		}
		in := newIntent(sc.Layout)
		if vs := append(in.check(sc, m), in.protectedIntact(m)...); len(vs) > 0 {
			logCase(rt, sc, cmds, m)
			rt.Fatalf("realized machine differs from layout intent: %+v", vs)
		}
		// Determinism and inventory-order independence.
		inv := sc.inventory()
		perm := rapid.Permutation(inv).Draw(rt, "invOrder")
		again, err := Plan(sc.Layout, perm)
		if err != nil || !reflect.DeepEqual(again, cmds) {
			rt.Fatalf("Plan is not deterministic under inventory reordering: err=%v\n%v\n%v", err, cmds, again)
		}
		st.mu.Lock()
		st.ok++
		for _, d := range sc.Layout.Devices {
			if len(d.Partitions) > st.maxParts {
				st.maxParts = len(d.Partitions)
			}
		}
		nr := 0
		for _, d := range sc.Layout.Devices {
			if d.Type == "raid" || d.Type == "preserve" {
				nr++
			}
		}
		if nr > st.maxRaids {
			st.maxRaids = nr
		}
		st.mu.Unlock()
	})
	st.report(t)
}

// TestPlanSim_Property_AcceptedNeverSilentlyWrong: for near-valid layouts
// (one or more targeted defects), whatever Plan accepts must either fail
// loudly when applied or realize the layout's intent. A plan that runs to
// completion and leaves the wrong device mounted, an array built from the
// wrong members, or a write on a disk outside the layout is a silent
// wrong-disk outcome.
func TestPlanSim_Property_AcceptedNeverSilentlyWrong(t *testing.T) {
	st := newSimStats()
	rapid.Check(t, func(rt *rapid.T) {
		sc := genSimCase(true).Draw(rt, "case")
		st.mu.Lock()
		st.cases++
		st.mu.Unlock()
		cmds, err := Plan(sc.Layout, sc.inventory())
		if err != nil {
			st.mu.Lock()
			st.rejected[bucket(err.Error())]++
			st.mu.Unlock()
			return
		}
		if len(sc.Absent) > 0 {
			logCase(rt, sc, cmds, nil)
			rt.Fatalf("Plan accepted a layout naming disk(s) %v that are not in the inventory", sc.Absent)
		}
		st.mu.Lock()
		for _, mu := range sc.Mutations {
			st.mutAccepts[firstWord(mu)]++
		}
		st.mu.Unlock()
		m := sc.machine()
		se, firstWrite := runSim(m, cmds)
		if se != nil {
			// Loud failure. Partial apply (after the first destructive
			// command) is tallied: those are validator-completeness gaps.
			if firstWrite >= 0 && se.idx > firstWrite {
				st.mu.Lock()
				st.applyFail[strings.Fields(se.cmd)[0]+": "+bucket(se.reason)]++
				st.mu.Unlock()
			}
			// Even a loud failure must not have written outside the plan
			// or destroyed protected data on the way.
			for d := range m.writes {
				if !newIntent(sc.Layout).targets[d] {
					logCase(rt, sc, cmds, m)
					rt.Fatalf("plan wrote to non-target disk %s before failing at %v", d, se)
				}
			}
			if vs := newIntent(sc.Layout).protectedIntact(m); len(vs) > 0 {
				logCase(rt, sc, cmds, m)
				rt.Fatalf("plan destroyed protected data before failing at %v: %+v", se, vs)
			}
			return
		}
		in := newIntent(sc.Layout)
		if vs := append(in.check(sc, m), in.protectedIntact(m)...); len(vs) > 0 {
			logCase(rt, sc, cmds, m)
			rt.Fatalf("Plan accepted a layout whose plan runs to completion but realizes the wrong machine: %+v", vs)
		}
		st.mu.Lock()
		st.ok++
		st.mu.Unlock()
	})
	st.report(t)
}
