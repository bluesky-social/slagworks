//go:build integration

package qemutest_test

// Storage property suite, part 4: the case generator.
//
// A VM boot per step makes shrinking impractical, so this is a seeded
// generator rather than a rapid property: the seed is the subtest name, and
// the driver logs the whole generated case as one PROPCASE JSON line, which
// SLAGWORKS_STORAGE_PROP_REPLAY can run again verbatim.
//
// The generator carries a small model of the disks (does the whole-disk
// device carry a signature the safety check can see, which arrays exist and
// are intact, which disks still hold a member of a pinned array) and uses it
// to say what each step must do. The model knows the documented rules only:
// the planner refuses a layout it cannot realize, the safety check refuses a
// signed disk without --destructive, a pinned array UUID must not also live
// on a disk the deploy leaves unwritten, preserve needs its array, and
// everything else must install and boot. Where an interrupted step leaves
// the model unsure what is on a disk, the next step's expectation lists
// every outcome the rules allow.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// propGenRootRAID456 lets the plain and preserve-heavy families put the root
// filesystem on RAID5 or RAID6. Data arrays draw those levels regardless, and
// the wide family (propWideSeed) draws parity roots regardless. It stays off
// so the seeds of the older families keep generating the cases they always
// have; the pinned uefi-root-raid5/6 and bios-root-raid5/6 histories have
// booted parity-RAID roots under both firmwares.
const propGenRootRAID456 = false

// The model's three-valued knowledge of a fact about a disk or array.
const (
	propYes   = "yes"
	propMaybe = "maybe"
	propNo    = "no"
)

type propArrState struct {
	a propArray
	// intact is propYes for an array a completed deploy created and nothing
	// has rewritten since, propMaybe when an interrupted or possibly-refused
	// step created or rewrote it, propNo once a step certainly rewrote one
	// of its disks.
	intact string
}

type propGen struct {
	rng  *rand.Rand
	c    *propCase
	big  map[string]bool
	sig  map[string]string // disk ID -> none | dirty | unknown
	arrs map[string]*propArrState
	// holders records, per pinned array UUID, every disk that carries
	// (propYes) or may carry (propMaybe) a member superblock of any
	// generation of that array and has not been wiped since.
	holders map[string]map[string]string
	// heavy biases the case towards data arrays that are created and then
	// preserved: seeds from propHeavySeed up. The plain mix reaches a
	// preserve step about once in seventy.
	heavy bool
	// wide draws the combinations the plain mix avoids: seeds from
	// propWideSeed up. See propGenCase.
	wide bool
	// mdsb records, per disk, whether it carries (propYes) or may carry
	// (propMaybe) an md member superblock of any array, which an
	// `mdadm --assemble --scan` in the deploy environment can assemble.
	mdsb map[string]string
	next int
}

func (g *propGen) pick(weights ...int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := g.rng.Intn(total)
	for i, w := range weights {
		if n < w {
			return i
		}
		n -= w
	}
	return len(weights) - 1
}

func (g *propGen) chance(pct int) bool { return g.rng.Intn(100) < pct }

func (g *propGen) choose(opts ...string) string { return opts[g.rng.Intn(len(opts))] }

func (g *propGen) addDisk(bus string, big bool, lbs int, discard bool, seed string) string {
	id := fmt.Sprintf("d%d", g.next)
	gib := 4 + g.rng.Intn(2)
	if big {
		gib = 8 + 2*g.rng.Intn(2)
	}
	d := propD(id, bus, gib, g.next, lbs, seed)
	d.Discard = discard
	g.next++
	g.c.Disks = append(g.c.Disks, d)
	g.big[id] = big
	g.sig[id] = "none"
	if seed != "" {
		g.sig[id] = "dirty"
	}
	return id
}

type propRootScheme struct {
	grub      bool
	esp       bool // an ESP slot exists
	espSize   string
	espOthers string // what the ESP slot holds on the other root disks: raw, vfat, none
	swap      bool
	rootSize  string
	tail      *propPart
}

func (s propRootScheme) parts(first bool, root propPart) ([]propPart, int) {
	var parts []propPart
	if s.grub {
		parts = append(parts, propPart{Size: "1M", BIOSGrub: true})
	}
	if s.esp {
		switch {
		case first:
			parts = append(parts, propPart{Size: s.espSize, FS: "vfat", ESP: true, Mount: "/boot/efi"})
		case s.espOthers == "raw":
			parts = append(parts, propPart{Size: s.espSize})
		case s.espOthers == "vfat":
			parts = append(parts, propPart{Size: s.espSize, FS: "vfat"})
		}
	}
	if s.swap {
		parts = append(parts, propPart{Size: "256M", FS: "swap"})
	}
	root.Size = s.rootSize
	parts = append(parts, root)
	idx := len(parts)
	if s.tail != nil {
		parts = append(parts, *s.tail)
	}
	return parts, idx
}

// genLayout builds a layout over the attached disks. keep lists data arrays
// from the previous step to carry over (preserved or re-created) instead of
// re-rolling their disks.
func (g *propGen) genLayout(attach []string, keep []propArray, prev *propLayout) propLayout {
	var l propLayout
	used := map[string]bool{}
	mounts := map[string]bool{}

	// Carried-over arrays claim their disks first.
	for _, a := range keep {
		for _, m := range a.Members {
			used[m.Disk] = true
		}
		if a.Mount != "" {
			mounts[a.Mount] = true
		}
		l.Arrays = append(l.Arrays, a)
		if a.Type != "raid" {
			continue
		}
		// A re-created array needs its member disks declared as targets,
		// with the partitions its members name.
		seen := map[string]bool{}
		for _, m := range a.Members {
			if seen[m.Disk] {
				continue
			}
			seen[m.Disk] = true
			dl := propDiskLayout{Disk: m.Disk}
			if m.Part != 0 && prev != nil {
				for _, p := range prev.Disks {
					if p.Disk == m.Disk {
						dl.Parts = append([]propPart(nil), p.Parts...)
					}
				}
			}
			l.Disks = append(l.Disks, dl)
		}
	}

	var bigs []string
	for _, id := range attach {
		if g.big[id] && !used[id] {
			bigs = append(bigs, id)
		}
	}
	if len(bigs) == 0 {
		panic("prop: no root-capable disk attached")
	}
	bios := g.c.Firmware == "bios"

	// Root topology.
	type topo struct {
		level, n, w int
	}
	topos := []topo{{-1, 1, 40}}
	if len(bigs) >= 2 {
		topos = append(topos, topo{1, 2, 45}, topo{0, 2, 10})
	}
	if len(bigs) >= 3 {
		topos = append(topos, topo{1, 3, 10})
		if propGenRootRAID456 || g.wide {
			topos = append(topos, topo{5, 3, 30})
		}
	}
	if len(bigs) >= 4 {
		topos = append(topos, topo{10, 4, 15})
		if propGenRootRAID456 || g.wide {
			topos = append(topos, topo{6, 4, 20})
		}
	}
	ws := make([]int, len(topos))
	for i, t := range topos {
		ws[i] = t.w
	}
	tp := topos[g.pick(ws...)]

	// On BIOS the firmware boots the first disk it is given, so the first
	// attached disk is always a root disk. On UEFI any disks will do.
	rootDisks := []string{bigs[0]}
	rest := append([]string(nil), bigs[1:]...)
	if !bios {
		g.rng.Shuffle(len(bigs), func(i, j int) { bigs[i], bigs[j] = bigs[j], bigs[i] })
		rootDisks = []string{bigs[0]}
		rest = append([]string(nil), bigs[1:]...)
	}
	g.rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	rootDisks = append(rootDisks, rest[:tp.n-1]...)
	if bios && tp.level >= 0 && tp.level != 1 {
		// GRUB reads the root array through the firmware's disk services,
		// and SeaBIOS (like most legacy BIOSes) cannot read a 4Kn disk. A
		// mirror boots from its 512-byte member alone; a striped or parity
		// root needs every 4Kn member it spans, so it is kept to 512-byte
		// disks (g2060: a BIOS RAID10 over two 512-byte and two 4Kn disks
		// stopped at "Entering rescue mode"). A limit of the firmware, not
		// a contract of the system under test.
		for _, id := range rootDisks {
			if g.c.disk(id).LBS == 4096 {
				tp = topo{1, 2, 0}
				rootDisks = rootDisks[:2]
				break
			}
		}
	}

	sch := propRootScheme{
		grub:      bios || g.chance(60),
		esp:       !bios || g.chance(70),
		espSize:   []string{"512M", "512M", "512M", "1G", "256M"}[g.rng.Intn(5)],
		espOthers: []string{"raw", "raw", "raw", "vfat", "none"}[g.rng.Intn(5)],
		swap:      g.chance(15),
		rootSize:  "rest",
	}
	if g.chance(35) {
		sch.rootSize = g.choose("5G", "pct:60", "6G")
		if g.chance(70) {
			tail := propPart{Size: g.choose("rest", "512M", "1G")}
			if g.chance(80) {
				tail.FS = g.choose("ext4", "xfs")
				if g.chance(75) {
					tail.Mount = "/srv/local"
				}
			}
			sch.tail = &tail
		}
	}
	rootFS := g.choose("ext4", "ext4", "xfs")
	var rootMembers []propMember
	for i, id := range rootDisks {
		used[id] = true
		root := propPart{}
		if tp.level < 0 {
			root = propPart{FS: rootFS, Mount: "/"}
		}
		sc := sch
		if i > 0 && sc.tail != nil {
			// The tail data partition is mounted once, from the first disk.
			t := *sc.tail
			t.Mount = ""
			sc.tail = &t
		}
		parts, idx := sc.parts(i == 0, root)
		l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: parts})
		rootMembers = append(rootMembers, propMember{Disk: id, Part: idx})
	}
	if tp.level >= 0 {
		l.Arrays = append(l.Arrays, propArray{
			Name: "mdroot", Type: "raid", Level: tp.level, FS: rootFS, Mount: "/", Members: rootMembers,
		})
	}

	// Everything else.
	var members []string
	for _, id := range attach {
		if used[id] {
			continue
		}
		role := g.pick(30, 45, 10, 7, 8)
		if g.heavy {
			role = g.pick(10, 75, 5, 5, 5)
		}
		if g.wide {
			role = g.pick(30, 40, 10, 10, 10)
		}
		switch role {
		case 0: // bystander
		case 1:
			members = append(members, id)
		case 2: // standalone data disk
			mp := "/srv/" + id
			parts := []propPart{{Size: "rest", FS: g.choose("ext4", "xfs"), Mount: mp}}
			if g.chance(40) {
				parts = []propPart{{Size: "1G", FS: g.choose("ext4", "xfs", "swap", "")}, parts[0]}
			}
			l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: parts})
		case 3:
			l.Retain = append(l.Retain, id)
		case 4: // declared but empty: the layout only wipes it
			l.Disks = append(l.Disks, propDiskLayout{Disk: id})
		}
	}
	taken := map[string]bool{}
	for _, a := range keep {
		taken[a.Name] = true
	}
	for len(members) >= 2 {
		take := len(members)
		if take >= 4 && g.chance(30) {
			take = 2
		}
		grp := members[:take]
		members = members[take:]
		var level int
		switch {
		case take == 2:
			level = []int{0, 1}[g.rng.Intn(2)]
		case take == 3:
			level = []int{0, 1, 5}[g.rng.Intn(3)]
		default:
			level = []int{0, 5, 6, 10}[g.rng.Intn(4)]
		}
		// The first free name: mddata, mddata2, ...
		slot := 1
		name := "mddata"
		for taken[name] {
			slot++
			name = fmt.Sprintf("mddata%d", slot)
		}
		taken[name] = true
		a := propArray{Name: name, Type: "raid", Level: level}
		if g.chance(80) {
			a.FS = g.choose("xfs", "ext4")
			mp := "/data"
			if slot > 1 {
				mp = fmt.Sprintf("/data%d", slot)
			}
			if g.chance(85) && !mounts[mp] {
				a.Mount = mp
				mounts[mp] = true
			}
		}
		if g.heavy || g.chance(80) {
			// Shipped scripts derive array_uuid from a per-host value, so
			// the same name gets the same UUID in every step of a case.
			a.UUID = fmt.Sprintf("5d1f0c1e-7a3b-4c2d-9e8f-0000000000%02x", slot)
		}
		whole := g.chance(60)
		for _, id := range grp {
			if whole {
				l.Disks = append(l.Disks, propDiskLayout{Disk: id})
				a.Members = append(a.Members, propMember{Disk: id})
			} else {
				l.Disks = append(l.Disks, propDiskLayout{Disk: id, Parts: []propPart{{Size: "rest"}}})
				a.Members = append(a.Members, propMember{Disk: id, Part: 1})
			}
		}
		l.Arrays = append(l.Arrays, a)
	}
	// A lone would-be member stays a bystander.
	return l
}

// propFAT32MinBytes is the smallest vfat partition the planner accepts on a
// disk with the given logical sector size. mkfs.fat formats a smaller one
// with too few clusters to be FAT32, which UEFI firmware cannot read; the
// documented minimum is 33M at 512-byte sectors and 257M at 4096.
func propFAT32MinBytes(lbs int) int64 {
	if lbs == 4096 {
		return 257 * propMiB
	}
	return 33 * propMiB
}

// propPlanRefusal says why the planner must refuse the step's layout before
// any disk is touched, or "" when the layout is realizable. Two documented
// rules can catch a generated layout: a vfat partition below the FAT32
// minimum for its disk's sector size, and partitions that do not fit their
// disk (the first starts 1 MiB in, the backup GPT is counted as 1 MiB, and a
// "rest" partition needs at least 1 MiB).
func propPlanRefusal(c propCase, st propStep) string {
	for _, dl := range st.Layout.Disks {
		d := c.disk(dl.Disk)
		need := 2 * propMiB
		for i, p := range dl.Parts {
			size, fixed := propSizeBytes(p.Size, d.Bytes)
			if !fixed {
				size = propMiB
			}
			need += size
			if need > d.Bytes/propMiB*propMiB {
				return fmt.Sprintf("partition %d of disk %s does not fit the disk", i+1, dl.Disk)
			}
			if p.FS == "vfat" && fixed && size < propFAT32MinBytes(d.LBS) {
				return fmt.Sprintf("vfat partition %d of disk %s is below the FAT32 minimum for %d-byte sectors", i+1, dl.Disk, d.LBS)
			}
		}
	}
	return ""
}

// staleHolder reports whether a disk this deploy leaves unwritten carries a
// member superblock with an array UUID a raid device in the layout pins:
// propYes, propMaybe (the model is unsure the superblock is there) or propNo.
// The agent refuses such a deploy, because at boot the stale member can claim
// the array ahead of its real members. Disks the layout targets are written,
// and so are bystanders under --wipe-all-disks; retained disks and the disks
// of preserve members are left alone under either flag.
func (g *propGen) staleHolder(st propStep) string {
	target := map[string]bool{}
	named := map[string]bool{}
	for _, dl := range st.Layout.Disks {
		target[dl.Disk] = true
		named[dl.Disk] = true
	}
	for _, a := range st.Layout.Arrays {
		for _, m := range a.Members {
			named[m.Disk] = true
		}
	}
	for _, id := range st.Layout.Retain {
		named[id] = true
	}
	worst := propNo
	for _, a := range st.Layout.Arrays {
		if a.Type != "raid" || a.UUID == "" {
			continue
		}
		for _, id := range st.Attach {
			if target[id] || (st.WipeAll && !named[id]) {
				continue
			}
			switch g.holders[a.UUID][id] {
			case propYes:
				return propYes
			case propMaybe:
				worst = propMaybe
			}
		}
	}
	return worst
}

// scannedBystander returns a disk the step leaves unwritten that carries, or
// may carry, an md member superblock while the layout uses a /dev/mdN node,
// or "" when there is none. With PreScan such a disk's array is assembled
// before the agent's checks, and which node it lands on depends on mdadm's
// naming (the superblock's name and the deploy environment's hostname), so
// the model only says the md name check may refuse. Members of the layout's
// preserve arrays are left out: the check releases an array made of them.
func (g *propGen) scannedBystander(st propStep) string {
	if len(st.Layout.Arrays) == 0 {
		return ""
	}
	skip := map[string]bool{}
	for _, dl := range st.Layout.Disks {
		skip[dl.Disk] = true
	}
	for _, a := range st.Layout.Arrays {
		if a.Type != "preserve" {
			continue
		}
		for _, m := range a.Members {
			skip[m.Disk] = true
		}
	}
	for _, id := range st.Attach {
		if !skip[id] && g.mdsb[id] != "" && g.mdsb[id] != propNo {
			return id
		}
	}
	return ""
}

// predict says what the step must do, from the documented rules alone, in
// the order the agent applies them: plan, safety check, stale array member
// check, preserve preflight, then the deploy itself. The result joins every
// allowed outcome with "|".
func (g *propGen) predict(st propStep) (expect, why string) {
	if reason := propPlanRefusal(*g.c, st); reason != "" {
		return "fatal:planning storage layout", reason
	}
	attached := map[string]bool{}
	for _, id := range st.Attach {
		attached[id] = true
	}
	var alts []string
	var whys []string
	open := func(outcome, reason string) {
		alts = append(alts, outcome)
		whys = append(whys, reason)
	}
	closed := func(outcome, reason string) (string, string) {
		open(outcome, reason)
		return strings.Join(alts, "|"), strings.Join(whys, "; or ")
	}

	if !st.Destructive {
		unsure := false
		for _, dl := range st.Layout.Disks {
			switch g.sig[dl.Disk] {
			case "dirty":
				return closed("fatal:safety check", "disk "+dl.Disk+" carries a signature and the deploy is not --destructive")
			case "unknown":
				unsure = true
			}
		}
		if unsure {
			open("fatal:safety check", "an interrupted deploy may have left a signature on a target disk")
		}
	}

	if st.PreScan && !st.Destructive {
		if id := g.scannedBystander(st); id != "" {
			open("fatal:md name check", "the deploy environment may have assembled an array from "+id+
				", a disk this deploy leaves alone, at a /dev/mdN the layout needs, and the deploy is not --destructive")
		}
	}

	switch g.staleHolder(st) {
	case propYes:
		return closed("fatal:stale array member check", "a disk this deploy leaves unwritten holds a member of a pinned array UUID")
	case propMaybe:
		open("fatal:stale array member check", "an interrupted deploy may have left a member of a pinned array UUID on a disk this deploy leaves unwritten")
	}

	for _, a := range st.Layout.Arrays {
		if a.Type != "preserve" {
			continue
		}
		s := g.arrs[a.Name]
		state := propNo
		if s != nil {
			state = s.intact
		}
		if state != propNo && (propHex(s.a.UUID) != propHex(a.UUID) || len(s.a.Members) != len(a.Members)) {
			state = propNo
		}
		if state != propNo {
			have := map[propMember]bool{}
			for _, m := range s.a.Members {
				have[m] = true
			}
			for _, m := range a.Members {
				if !have[m] || !attached[m.Disk] {
					state = propNo
				}
			}
		}
		switch state {
		case propNo:
			return closed("fatal:preserve preflight", "preserve array "+a.Name+" does not exist as described")
		case propMaybe:
			open("fatal:preserve preflight", "preserve array "+a.Name+" may not have survived an interrupted deploy")
		}
	}

	switch {
	case st.FatalAt != "":
		return closed("fatal_at", "stopped at phase "+st.FatalAt)
	case st.KillAtCmd != 0:
		return closed("killed", "power lost during the storage commands")
	}
	return closed("ok", "valid layout; the disks are clean or the deploy is --destructive")
}

// apply moves the model past a step.
func (g *propGen) apply(st propStep) {
	// ran: did the agent get past its preflight checks and start writing?
	// done: did the storage commands run to completion?
	refused, wrote, done := false, false, false
	for _, alt := range strings.Split(st.Expect, "|") {
		switch {
		case alt == "ok", alt == "fatal_at" && st.FatalAt != "partitioning":
			wrote, done = true, true
		case alt == "killed":
			wrote = true
		default:
			refused = true
		}
	}
	if !wrote {
		return
	}
	ran := propYes
	if refused {
		ran = propMaybe
	}
	// certain: the model knows exactly what the step left on its disks.
	certain := ran == propYes && done

	target := map[string]bool{}
	named := map[string]bool{}
	for _, dl := range st.Layout.Disks {
		target[dl.Disk] = true
		named[dl.Disk] = true
	}
	for _, a := range st.Layout.Arrays {
		for _, m := range a.Members {
			named[m.Disk] = true
		}
	}
	for _, id := range st.Layout.Retain {
		named[id] = true
	}
	// touched: the disks the deploy wipes before its first storage command,
	// that is its targets and, under --wipe-all-disks, every bystander.
	touched := map[string]bool{}
	for id := range target {
		touched[id] = true
	}
	if st.WipeAll {
		for _, id := range st.Attach {
			if !named[id] {
				touched[id] = true
				g.sig[id] = "none"
				if !certain {
					g.sig[id] = "unknown"
				}
			}
		}
	}

	// Member superblocks on a touched disk are gone if the step ran, and
	// possibly gone if it may have been refused.
	for _, h := range g.holders {
		for id := range touched {
			if h[id] == "" {
				continue
			}
			if ran == propYes {
				delete(h, id)
			} else {
				h[id] = propMaybe
			}
		}
	}
	for id := range touched {
		if g.mdsb[id] == "" {
			continue
		}
		if ran == propYes {
			delete(g.mdsb, id)
		} else {
			g.mdsb[id] = propMaybe
		}
	}
	// Arrays living on a touched disk likewise.
	for _, s := range g.arrs {
		for _, m := range s.a.Members {
			if !touched[m.Disk] || s.intact == propNo {
				continue
			}
			if ran == propYes {
				s.intact = propNo
			} else {
				s.intact = propMaybe
			}
		}
	}

	wholeMember := map[string]bool{}
	for _, a := range st.Layout.Arrays {
		if a.Type != "raid" {
			continue
		}
		for _, m := range a.Members {
			if m.Part == 0 {
				wholeMember[m.Disk] = true
			}
		}
		for _, m := range a.Members {
			if certain {
				g.mdsb[m.Disk] = propYes
			} else if g.mdsb[m.Disk] != propYes {
				g.mdsb[m.Disk] = propMaybe
			}
		}
		// The new array's members carry its superblock once mdadm has run,
		// which an interrupted step may not have reached.
		if a.UUID != "" {
			if g.holders[a.UUID] == nil {
				g.holders[a.UUID] = map[string]string{}
			}
			for _, m := range a.Members {
				if certain {
					g.holders[a.UUID][m.Disk] = propYes
				} else {
					g.holders[a.UUID][m.Disk] = propMaybe
				}
			}
		}
		// Only an array that went through a completed deploy and a clean
		// shutdown is known to be preservable: one cut off mid-creation may
		// refuse to assemble for mdadm's own reasons (a RAID5 that never
		// finished its first rebuild).
		intact := propMaybe
		if st.Expect == "ok" {
			intact = propYes
		}
		g.arrs[a.Name] = &propArrState{a: a, intact: intact}
	}
	for _, dl := range st.Layout.Disks {
		switch {
		case !certain:
			g.sig[dl.Disk] = "unknown"
		case len(dl.Parts) > 0 || wholeMember[dl.Disk]:
			g.sig[dl.Disk] = "dirty"
		default:
			g.sig[dl.Disk] = "none"
		}
	}
}

// propGenCase generates the case for a seed.
func propGenCase(seed int64) propCase {
	c := propCase{Name: fmt.Sprintf("g%04d", seed)}
	g := &propGen{
		rng: rand.New(rand.NewSource(seed)), c: &c,
		big: map[string]bool{}, sig: map[string]string{}, arrs: map[string]*propArrState{},
		holders: map[string]map[string]string{}, mdsb: map[string]string{},
	}
	g.heavy = seed >= propHeavySeed && seed < propWideSeed
	g.wide = seed >= propWideSeed
	c.Firmware = "uefi"
	if g.wide {
		if g.chance(50) {
			c.Firmware = "bios"
		}
	} else if g.chance(40) && !g.heavy {
		c.Firmware = "bios"
	}
	bios := c.Firmware == "bios"
	nDisks := 1 + g.pick(8, 25, 27, 20, 10, 10)
	if g.heavy {
		nDisks = 3 + g.pick(40, 35, 25)
	}
	if g.wide {
		nDisks = 3 + g.pick(25, 40, 35)
	}
	bus := []string{"virtio", "nvme", "sata", "scsi", "mixed"}[g.pick(35, 28, 12, 13, 12)]
	fourK := g.chance(20)
	discard := g.chance(50)
	seeded := g.chance(30)
	if g.wide {
		seeded = g.chance(60)
	}
	nBig := 1
	switch {
	case nDisks == 2:
		nBig = 1 + g.pick(30, 70)
	case nDisks >= 3:
		nBig = 1 + g.pick(20, 40, 20, 20)
		if g.wide {
			// Three or four root-capable disks reach the parity-RAID roots.
			nBig = 1 + g.pick(10, 20, 35, 35)
		}
		if nBig > nDisks {
			nBig = nDisks
		}
	}
	sata := 0
	newDisk := func(i int, big bool) string {
		b := bus
		if b == "mixed" {
			b = g.choose("virtio", "nvme", "sata", "scsi")
			if big {
				b = g.choose("virtio", "nvme")
			}
		}
		// SeaBIOS here does not expose QEMU's NVMe disks as BIOS drives
		// ("Boot failed: could not read the boot disk" on an NVMe-only
		// machine with a valid boot sector), so a BIOS case never puts its
		// root on NVMe. A limit of the test firmware, not of the system
		// under test.
		if bios && big && b == "nvme" {
			b = "virtio"
		}
		// The VM has five free SATA ports.
		if b == "sata" {
			if sata == 5 {
				b = "scsi"
			} else {
				sata++
			}
		}
		lbs := 512
		// SeaBIOS cannot boot a 4Kn disk, so on BIOS only data disks get one.
		if fourK && b != "sata" && (!bios || !big) {
			lbs = 4096
		}
		if g.wide {
			// Sector sizes mix within a case, and on BIOS every disk but
			// the firmware's first may be 4Kn: a root mirror then has GRUB
			// installed on a 4Kn disk beside the 512-byte one that boots.
			lbs = 512
			if b != "sata" && (!bios || i != 0) && g.chance(35) {
				lbs = 4096
			}
		}
		seed := ""
		if seeded && i > 0 && g.chance(35) {
			// The plain mix leaves "ambiv" (conflicting signatures) to the
			// handwritten safety-ambiv and destructive-over-zfs cases.
			if g.wide {
				seed = g.choose("ext4", "luks", "dos", "ambiv", "ambiv")
			} else {
				seed = g.choose("ext4", "luks", "dos")
			}
		}
		return g.addDisk(b, big, lbs, discard, seed)
	}
	var attach []string
	for i := 0; i < nDisks; i++ {
		attach = append(attach, newDisk(i, i < nBig))
	}

	nSteps := 1 + g.pick(15, 50, 30, 5)
	var prev *propLayout
	for i := 0; i < nSteps; i++ {
		last := i == nSteps-1
		// Hardware changes between deploys.
		if i > 0 && g.chance(25) {
			switch g.pick(40, 20, 20, 20) {
			case 0: // reorder: every kernel name can change
				if bios {
					tail := attach[1:]
					g.rng.Shuffle(len(tail), func(a, b int) { tail[a], tail[b] = tail[b], tail[a] })
				} else {
					g.rng.Shuffle(len(attach), func(a, b int) { attach[a], attach[b] = attach[b], attach[a] })
				}
			case 1: // a small disk is pulled
				for k := len(attach) - 1; k > 0; k-- {
					if !g.big[attach[k]] {
						attach = append(attach[:k:k], attach[k+1:]...)
						break
					}
				}
			case 2: // a disk is replaced with a blank one
				if k := 1 + g.rng.Intn(len(attach)); k < len(attach) {
					attach[k] = newDisk(1, g.big[attach[k]])
				}
			case 3: // a disk is added
				if len(attach) < 6 {
					attach = append(attach, newDisk(1, g.chance(30)))
				}
			}
		}
		// The wide family's targeted pattern: the host moves to blank disks
		// and the old disks, arrays and all, stay attached as bystanders.
		// Only on UEFI: on BIOS the root has to stay on the firmware's first
		// disk.
		var moveTo []string
		if g.wide && !bios && i > 0 && len(g.mdsb) > 0 && len(attach) < 6 && g.chance(40) {
			moveTo = append(moveTo, newDisk(0, true))
			if len(attach)+len(moveTo) < 6 && g.chance(60) {
				moveTo = append(moveTo, newDisk(0, g.chance(50)))
			}
			attach = append(attach, moveTo...)
		}
		st := propStep{Attach: append([]string(nil), attach...)}

		attached := map[string]bool{}
		for _, id := range attach {
			attached[id] = true
		}
		// Which existing data arrays could this step carry over? Any array
		// the model believes intact, pinned to a UUID and fully attached,
		// whether the previous layout named it or left it on bystanders.
		var keepable []propArray
		var names []string
		for name := range g.arrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			s := g.arrs[name]
			if name == "mdroot" || s.a.UUID == "" || s.intact != propYes {
				continue
			}
			here := true
			for _, m := range s.a.Members {
				// On BIOS the first attached disk stays free for the root.
				if !attached[m.Disk] || (bios && m.Disk == attach[0]) {
					here = false
				}
			}
			if here {
				keepable = append(keepable, s.a)
			}
		}
		var keep []propArray
		mode := 1 // 0 same layout again, 1 fresh layout, 2 fresh root around kept arrays
		switch {
		case prev == nil:
		case len(keepable) > 0 && g.heavy:
			mode = 2
		case len(keepable) > 0:
			mode = g.pick(10, 30, 60)
		default:
			mode = g.pick(15, 85)
		}
		switch mode {
		case 0:
			st.Layout = *prev
			for _, dl := range prev.Disks {
				if !attached[dl.Disk] {
					mode = 1
				}
			}
			for _, a := range prev.Arrays {
				for _, m := range a.Members {
					if !attached[m.Disk] {
						mode = 1
					}
				}
			}
			for _, id := range prev.Retain {
				if !attached[id] {
					mode = 1
				}
			}
		case 2:
			for _, a := range keepable {
				// A kept array left out becomes a bystander array: on
				// disks the layout does not name, intact, assembled by a
				// scan. The wide family draws that more often.
				skip := 15
				if g.wide {
					skip = 35
				}
				if !g.heavy && g.chance(skip) {
					continue
				}
				// Re-creating needs the member disks declared with the
				// partitions the members name; only the previous layout
				// can supply those.
				canRecreate := true
				for _, m := range a.Members {
					if m.Part == 0 {
						continue
					}
					found := false
					for _, p := range prev.Disks {
						if p.Disk == m.Disk && len(p.Parts) >= m.Part {
							found = true
						}
					}
					if !found {
						canRecreate = false
					}
				}
				k := a
				k.Members = append([]propMember(nil), a.Members...)
				k.Type = "preserve"
				switch {
				case canRecreate && g.chance(30):
					k.Type = "raid"
				case g.chance(8):
					// Near-valid: the script names the wrong array.
					k.UUID = k.UUID[:len(k.UUID)-1] + "f"
				case len(k.Members) >= 2 && g.chance(8):
					// Near-valid: the script leaves a member out.
					k.Members = k.Members[:len(k.Members)-1]
				}
				keep = append(keep, k)
			}
		}
		if mode != 0 {
			st.Layout = g.genLayout(attach, keep, prev)
		}
		if moveTo != nil {
			// Arrays mode 2 kept come along (preserved arrays then sit
			// among bystander disks); the rest of the layout uses only the
			// new disks.
			st.Layout = g.genLayout(moveTo, keep, prev)
		}

		// Flags.
		switch {
		case i == 0:
			st.Destructive = g.chance(40)
		case moveTo != nil:
			// The targets are blank: no flag is needed to write them.
			st.Destructive = g.chance(25)
		default:
			st.Destructive = g.chance(80)
		}
		st.WipeAll = st.Destructive && g.chance(30)
		// Interruptions, never on the last step.
		if !last && g.chance(30) && (!g.heavy || i != 0) {
			if g.chance(60) {
				st.FatalAt = []string{"partitioning", "rsyncing_os", "rsyncing_os", "installing_bootloader", "finalizing", "finalizing"}[g.rng.Intn(6)]
			} else {
				st.KillAtCmd = 1 + g.rng.Intn(propPlanLen(st.Layout))
			}
		}
		if g.wide {
			st.PreScan = g.chance(45)
		}
		st.Expect, st.Why = g.predict(st)
		// A second boot goes PXE, then the boot-local script, the path every
		// later boot takes; the first boot after a UEFI deploy used the
		// one-shot BootNext instead.
		st.Reboot = st.Expect == "ok" && g.chance(50)
		g.apply(st)
		c.Steps = append(c.Steps, st)
		lay := st.Layout
		prev = &lay
	}
	return c
}

// propPlanLen is the number of storage commands a layout plans, counted from
// the layout itself: per disk one zap, one sgdisk per partition, a re-read
// when it has partitions, then one mkfs per formatted partition and a wipefs
// plus a boot-sector zero per unformatted one; per array one mdadm and, when
// it is created rather than preserved, one mkfs or wipefs; two commands per
// mount.
func propPlanLen(l propLayout) int {
	n := 0
	for _, dl := range l.Disks {
		n++
		if len(dl.Parts) > 0 {
			n++
		}
		for _, p := range dl.Parts {
			n += 2
			if p.FS == "" {
				n++
			}
			if p.Mount != "" {
				n += 2
			}
		}
	}
	for _, a := range l.Arrays {
		n++
		if a.Type == "raid" {
			n++
		}
		if a.Mount != "" {
			n += 2
		}
	}
	return n
}

// propHeavySeed is the first seed of the preserve-heavy family.
const propHeavySeed = 1000

// propWideSeed is the first seed of the wide family: three to five disks,
// root on RAID5 and RAID6 as well as the plain topologies, sector sizes mixed
// within a case (on BIOS too, past the firmware's first disk), conflicting
// signatures among the seeded disks, kept arrays left as bystanders more
// often, and storage scripts that assemble every array they can find before
// the agent's checks (propStep.PreScan).
const propWideSeed = 2000

// propSeedsEnv names the generated cases to use instead of the defaults, as
// a comma-separated list of seeds and inclusive ranges: "7,40-59,1000-1019".
// TestStoragePropGen plays them on VMs (several minutes per case) and
// TestStoragePropGenShape checks them without one.
const propSeedsEnv = "SLAGWORKS_STORAGE_PROP_SEEDS"

// propReplayEnv names a JSON file holding an array of cases, each a PROPCASE
// line copied from an earlier run's log. TestStoragePropGen plays those
// instead of generating any: a generator change can alter what a seed
// produces, a saved case cannot drift.
const propReplayEnv = "SLAGWORKS_STORAGE_PROP_REPLAY"

// propGenDefaultSeeds are the generated cases TestStoragePropGen plays when
// propSeedsEnv is unset. One, because every step is a VM boot: a history of
// the preserve-heavy family that creates a root mirror and a data mirror,
// then redeploys the root without the mirror around the preserved data
// array and reboots the result.
var propGenDefaultSeeds = []int64{1002}

// propParseSeeds parses a propSeedsEnv value.
func propParseSeeds(spec string) ([]int64, error) {
	var seeds []int64
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.ParseInt(lo, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("seed %q: %w", part, err)
		}
		b := a
		if isRange {
			if b, err = strconv.ParseInt(hi, 10, 64); err != nil {
				return nil, fmt.Errorf("seed range %q: %w", part, err)
			}
		}
		if b < a || b-a >= 100000 {
			return nil, fmt.Errorf("seed range %q is empty or too large", part)
		}
		for s := a; s <= b; s++ {
			seeds = append(seeds, s)
		}
	}
	if len(seeds) == 0 {
		return nil, fmt.Errorf("no seeds in %q", spec)
	}
	return seeds, nil
}

// propSeeds returns the seeds from propSeedsEnv, or def when it is unset.
func propSeeds(t *testing.T, def []int64) []int64 {
	t.Helper()
	spec := os.Getenv(propSeedsEnv)
	if spec == "" {
		return def
	}
	seeds, err := propParseSeeds(spec)
	if err != nil {
		t.Fatalf("%s: %v", propSeedsEnv, err)
	}
	return seeds
}

// TestStoragePropGen plays generated deploy histories on VMs and holds each
// step to the outcome the generator's model predicts and the installed
// system to the tree its layout asks for.
func TestStoragePropGen(t *testing.T) {
	var cases []propCase
	if path := os.Getenv(propReplayEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", propReplayEnv, err)
		}
		if err := json.Unmarshal(data, &cases); err != nil {
			t.Fatalf("%s: parse %s: %v", propReplayEnv, path, err)
		}
	} else {
		for _, seed := range propSeeds(t, propGenDefaultSeeds) {
			cases = append(cases, propGenCase(seed))
		}
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}

// TestStoragePropGenShape needs no VM. It runs every generated storage script
// against a fake inventory and feeds the result to the real planner: the
// planner must refuse exactly the layouts the model says it refuses, and plan
// as many commands as the model counts for the rest (the interruption points
// are command numbers). It also logs how the generated cases are distributed.
func TestStoragePropGenShape(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	// By default, a slice of the plain mix and one of the preserve-heavy
	// family: enough to cover every layout shape the generator draws while
	// the test stays in seconds.
	var def []int64
	for seed := int64(0); seed < 60; seed++ {
		def = append(def, seed)
	}
	for seed := int64(propHeavySeed); seed < propHeavySeed+30; seed++ {
		def = append(def, seed)
	}
	for seed := int64(propWideSeed); seed < propWideSeed+60; seed++ {
		def = append(def, seed)
	}
	dir := t.TempDir()
	count := map[string]int{}
	bump := func(k string) { count[k]++ }
	for _, seed := range propSeeds(t, def) {
		c := propGenCase(seed)
		if os.Getenv("SLAGWORKS_STORAGE_PROP_LIST") == "1" {
			t.Logf("SEED %d %s", seed, propCaseFeatures(c))
		}
		if seed >= propHeavySeed && seed < propWideSeed {
			for _, st := range c.Steps {
				for _, a := range st.Layout.Arrays {
					if a.Type == "preserve" {
						bump("heavy: preserve steps expecting " + st.Expect)
					}
				}
			}
		}
		bump("firmware=" + c.Firmware)
		bump(fmt.Sprintf("disks=%d", len(c.Disks)))
		bump(fmt.Sprintf("steps=%d", len(c.Steps)))
		for _, d := range c.Disks {
			bump("bus=" + d.Bus)
			bump(fmt.Sprintf("lbs=%d", d.LBS))
			if d.Seed != "" {
				bump("seed=" + d.Seed)
			}
		}
		for i, st := range c.Steps {
			bump("expect=" + st.Expect)
			if st.Destructive {
				bump("flag=destructive")
			}
			if st.WipeAll {
				bump("flag=wipe_all")
			}
			if st.FatalAt != "" {
				bump("interrupt=fatal_at:" + st.FatalAt)
			}
			if st.KillAtCmd != 0 {
				bump("interrupt=kill")
			}
			if st.Reboot {
				bump("flag=reboot")
			}
			named := map[string]bool{}
			preserve := false
			for _, dl := range st.Layout.Disks {
				named[dl.Disk] = true
			}
			for _, id := range st.Layout.Retain {
				named[id] = true
			}
			for _, a := range st.Layout.Arrays {
				preserve = preserve || a.Type == "preserve"
				for _, m := range a.Members {
					named[m.Disk] = true
				}
			}
			if preserve && len(named) < len(st.Attach) {
				bump("layout=preserve-beside-bystanders, expecting " + st.Expect)
			}
			if st.PreScan {
				bump("flag=prescan")
				if !st.Destructive {
					bump("prescan: non-destructive expecting " + st.Expect)
				}
			}
			if c.Firmware == "bios" {
				for _, dl := range st.Layout.Disks {
					for _, p := range dl.Parts {
						if p.BIOSGrub && c.disk(dl.Disk).LBS == 4096 {
							bump("bios: grub on a 4Kn disk")
						}
					}
				}
			}
			if len(st.Layout.Retain) > 0 {
				bump("layout=retain")
			}
			for _, a := range st.Layout.Arrays {
				kind := "data"
				if a.Name == "mdroot" {
					kind = "root"
				}
				bump(fmt.Sprintf("array=%s-%s-raid%d", kind, a.Type, a.Level))
			}
			if len(st.Layout.Arrays) == 0 {
				bump("array=none")
			}
			if len(st.Attach) > len(st.Layout.Disks) {
				bump("layout=has-unlisted-disks")
			}

			// Run the script against a fake inventory and plan the result.
			type invDisk struct {
				Name      string `json:"name"`
				SizeBytes int64  `json:"size_bytes"`
			}
			var inv struct {
				Disks []invDisk `json:"disks"`
			}
			var blk []storage.BlockDevice
			for k, id := range st.Attach {
				d := c.disk(id)
				name := fmt.Sprintf("vd%c", 'a'+k)
				if d.Bus == "nvme" {
					name = fmt.Sprintf("nvme%dn1", k)
				}
				inv.Disks = append(inv.Disks, invDisk{Name: name, SizeBytes: d.Bytes})
				blk = append(blk, storage.BlockDevice{Name: name, SizeBytes: d.Bytes, LogicalSectorBytes: int64(d.LBS)})
			}
			invPath := filepath.Join(dir, "inv.json")
			b, err := json.Marshal(inv)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(invPath, b, 0600); err != nil {
				t.Fatal(err)
			}
			scriptPath := filepath.Join(dir, "script.py")
			// The script runs on this host, not in a VM: without PreScan,
			// so it does not assemble the host's own arrays.
			hostStep := st
			hostStep.PreScan = false
			if err := os.WriteFile(scriptPath, []byte(propRenderScript(c, hostStep)), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", scriptPath)
			cmd.Env = append(os.Environ(), "SLAGWORKS_INVENTORY="+invPath)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s step %d: script failed: %v\n%s", c.Name, i, err, c.JSON())
			}
			var layout storage.Layout
			if err := json.Unmarshal(out, &layout); err != nil {
				t.Fatalf("%s step %d: layout JSON: %v", c.Name, i, err)
			}
			cmds, err := storage.Plan(layout, blk)
			refusal := propPlanRefusal(c, st)
			switch {
			case err != nil && refusal == "":
				t.Errorf("%s step %d: the planner refuses a layout the model takes for realizable: %v\n%s", c.Name, i, err, c.JSON())
			case err == nil && refusal != "":
				t.Errorf("%s step %d: the planner accepts a layout the model says it refuses (%s)\n%s", c.Name, i, refusal, c.JSON())
			case err == nil:
				if got, want := len(cmds), propPlanLen(st.Layout); got != want {
					t.Errorf("%s step %d: planner emits %d commands, the model counts %d\n%s", c.Name, i, got, want, c.JSON())
				}
			}
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("SHAPE %-40s %d", k, count[k])
	}
}

// propCaseFeatures is a one-line digest of the combinations a case reaches,
// for picking seeds to play on VMs (SLAGWORKS_STORAGE_PROP_LIST=1 with
// TestStoragePropGenShape logs it per seed).
func propCaseFeatures(c propCase) string {
	f := map[string]bool{"fw=" + c.Firmware: true}
	for _, d := range c.Disks {
		if d.Seed == "ambiv" {
			f["ambiv"] = true
		}
	}
	firstSeen := map[string]int{}
	for i, st := range c.Steps {
		fresh := len(st.Layout.Disks) > 0
		for _, id := range st.Attach {
			if _, ok := firstSeen[id]; !ok {
				firstSeen[id] = i
			}
		}
		for _, dl := range st.Layout.Disks {
			fresh = fresh && firstSeen[dl.Disk] == i
		}
		if i > 0 && fresh && st.Expect == "ok" {
			f["move"] = true
			if st.Reboot {
				f["move-reboot"] = true
			}
		}
		for _, a := range st.Layout.Arrays {
			if a.Name == "mdroot" && (a.Level == 5 || a.Level == 6) {
				f[fmt.Sprintf("root-raid%d", a.Level)] = true
			}
			if a.Type == "preserve" {
				f["preserve"] = true
			}
		}
		if c.Firmware == "bios" {
			for _, dl := range st.Layout.Disks {
				for _, p := range dl.Parts {
					if p.BIOSGrub && c.disk(dl.Disk).LBS == 4096 {
						f["bios-grub-4kn"] = true
					}
				}
			}
		}
		if st.PreScan && strings.Contains(st.Expect, "md name check") {
			f["prescan-mdname"] = true
		}
		if st.PreScan && !st.Destructive && st.Expect == "ok" {
			f["prescan-ok"] = true
		}
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("steps=%d %s", len(c.Steps), strings.Join(keys, " "))
}
