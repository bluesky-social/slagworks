//go:build integration

package qemutest_test

// Storage property suite, part 4: the case generator.
//
// A VM boot per step makes shrinking impractical, so this is a seeded
// generator rather than a rapid property: the seed is the subtest name, and
// the driver logs the whole generated case as one PROPCASE JSON line, which
// TestStoragePropReplay can run again verbatim.
//
// The generator carries a small model of the disks (does the whole-disk
// device carry a signature blkid can see, which arrays exist and are intact)
// and uses it to say what each step must do. The model knows the documented
// rules only: the safety check refuses a signed disk without --destructive,
// preserve needs its array, and everything else must install and boot.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// propGenRAID456 lets the generator draw RAID5 and RAID6. It is off because
// the deploy initrd cannot create either (the raid456 module is not baked
// in; see TestStoragePropFindings/uefi-raid5-data), so every such case would
// stop at the same known failure. Turn it on once that is fixed.
const propGenRAID456 = false

type propArrState struct {
	a      propArray
	intact bool
}

type propGen struct {
	rng  *rand.Rand
	c    *propCase
	big  map[string]bool
	sig  map[string]string // disk ID -> none | dirty | unknown
	arrs map[string]*propArrState
	// holders records, per pinned array UUID, every disk that carries a
	// member superblock of any generation of that array and has not been
	// wiped since.
	holders map[string]map[string]bool
	// memberOf records, per disk, that it carries a member superblock of
	// some array and has not been wiped since.
	memberOf map[string]bool
	// heavy biases the case towards data arrays that are created and then
	// preserved: seeds from propHeavySeed up. The plain mix reaches a
	// preserve step about once in seventy.
	heavy bool
	next  int
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
		if propGenRAID456 {
			topos = append(topos, topo{5, 3, 15})
		}
	}
	if len(bigs) >= 4 {
		topos = append(topos, topo{10, 4, 15})
		if propGenRAID456 {
			topos = append(topos, topo{6, 4, 8})
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
		if !propGenRAID456 && (level == 5 || level == 6) {
			level = []int{0, 1, 10}[g.rng.Intn(3)]
			if take < 4 && level == 10 {
				level = 1
			}
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

// predict says what the step must do, from the documented rules alone.
func (g *propGen) predict(st propStep) (expect, why string) {
	attached := map[string]bool{}
	for _, id := range st.Attach {
		attached[id] = true
	}
	open := false
	if !st.Destructive {
		for _, dl := range st.Layout.Disks {
			switch g.sig[dl.Disk] {
			case "dirty":
				return "fatal:safety check", "disk " + dl.Disk + " carries a signature and the deploy is not --destructive"
			case "unknown":
				open = true
			}
		}
	}
	for _, a := range st.Layout.Arrays {
		if a.Type != "preserve" {
			continue
		}
		s := g.arrs[a.Name]
		ok := s != nil && s.intact && propHex(s.a.UUID) == propHex(a.UUID) && len(s.a.Members) == len(a.Members)
		if ok {
			have := map[propMember]bool{}
			for _, m := range s.a.Members {
				have[m] = true
			}
			for _, m := range a.Members {
				if !have[m] || !attached[m.Disk] {
					ok = false
				}
			}
		}
		if !ok {
			if open {
				return "fatal:safety check|fatal:preserve preflight", "open safety outcome, then a preserve array that does not exist"
			}
			return "fatal:preserve preflight", "preserve array " + a.Name + " does not exist as described"
		}
	}
	final := "ok"
	why = "valid layout; the disks are clean or the deploy is --destructive"
	switch {
	case st.FatalAt != "":
		final, why = "fatal_at", "stopped at phase "+st.FatalAt
	case st.KillAtCmd != 0:
		final, why = "killed", "power lost during the storage commands"
	}
	if open {
		return final + "|fatal:safety check", "an interrupted deploy may or may not have left a signature"
	}
	return final, why
}

// hazard names a known finding the step walks into, or "". The driver logs
// the step's tree violations as known instead of failing on them.
//
// stale-uuid-member: the layout has an array with a pinned UUID while a disk
// outside the layout, which this deploy will not wipe, still carries a member
// superblock with that UUID. At boot the stale member can claim the array
// before the real members do (TestStoragePropFindings/uefi-stale-uuid-member).
func (g *propGen) hazard(st propStep) string {
	wiped := map[string]bool{}
	named := map[string]bool{}
	for _, dl := range st.Layout.Disks {
		wiped[dl.Disk] = true
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
	for _, id := range st.Attach {
		if st.WipeAll && !named[id] {
			wiped[id] = true
		}
	}
	for _, a := range st.Layout.Arrays {
		if a.UUID == "" {
			continue
		}
		member := map[string]bool{}
		for _, m := range a.Members {
			member[m.Disk] = true
		}
		for _, id := range st.Attach {
			if g.holders[a.UUID][id] && !member[id] && !wiped[id] {
				return "stale-uuid-member"
			}
		}
	}
	// bystander-array: the layout has an array of its own while a disk it
	// neither wipes nor names as a member carries an array member. Both
	// arrays want /dev/md0 at boot and the wrong one can win
	// (TestStoragePropFindings/uefi-bystander-array-takes-md0).
	if len(st.Layout.Arrays) > 0 {
		member := map[string]bool{}
		for _, a := range st.Layout.Arrays {
			for _, m := range a.Members {
				member[m.Disk] = true
			}
		}
		for _, id := range st.Attach {
			if g.memberOf[id] && !member[id] && !wiped[id] {
				return "bystander-array"
			}
		}
	}
	return ""
}

// apply moves the model past a step.
func (g *propGen) apply(st propStep) {
	writes := false
	for _, alt := range strings.Split(st.Expect, "|") {
		if alt == "ok" || alt == "killed" || (alt == "fatal_at" && st.FatalAt != "partitioning") {
			writes = true
		}
	}
	if !writes {
		return
	}
	// certain: the storage commands ran to completion.
	certain := st.Expect == "ok" || st.Expect == "fatal_at" || st.Expect == "ok|noboot"
	target := map[string]bool{}
	for _, dl := range st.Layout.Disks {
		target[dl.Disk] = true
	}
	named := map[string]bool{}
	for id := range target {
		named[id] = true
	}
	for _, a := range st.Layout.Arrays {
		for _, m := range a.Members {
			named[m.Disk] = true
		}
	}
	for _, id := range st.Layout.Retain {
		named[id] = true
	}
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
	for _, h := range g.holders {
		for id := range touched {
			delete(h, id)
		}
	}
	for id := range touched {
		delete(g.memberOf, id)
	}
	for _, a := range st.Layout.Arrays {
		if a.Type == "raid" {
			for _, m := range a.Members {
				g.memberOf[m.Disk] = true
			}
		}
	}
	for _, a := range st.Layout.Arrays {
		if a.Type != "raid" || a.UUID == "" {
			continue
		}
		if g.holders[a.UUID] == nil {
			g.holders[a.UUID] = map[string]bool{}
		}
		for _, m := range a.Members {
			g.holders[a.UUID][m.Disk] = true
		}
	}
	// Arrays living on a disk this step rewrote are gone.
	for _, s := range g.arrs {
		for _, m := range s.a.Members {
			if touched[m.Disk] {
				s.intact = false
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
		// Only an array that was shut down cleanly is preserved later: one
		// cut off mid-creation may refuse to assemble for mdadm's own
		// reasons (a RAID5 that never finished its first rebuild).
		g.arrs[a.Name] = &propArrState{a: a, intact: st.Expect == "ok"}
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
		holders: map[string]map[string]bool{}, memberOf: map[string]bool{},
	}
	g.heavy = seed >= propHeavySeed
	c.Firmware = "uefi"
	if g.chance(40) && !g.heavy {
		c.Firmware = "bios"
	}
	bios := c.Firmware == "bios"
	nDisks := 1 + g.pick(8, 25, 27, 20, 10, 10)
	if g.heavy {
		nDisks = 3 + g.pick(40, 35, 25)
	}
	bus := []string{"virtio", "nvme", "sata", "scsi", "mixed"}[g.pick(35, 28, 12, 13, 12)]
	fourK := g.chance(20)
	discard := g.chance(50)
	seeded := g.chance(30)
	nBig := 1
	switch {
	case nDisks == 2:
		nBig = 1 + g.pick(30, 70)
	case nDisks >= 3:
		nBig = 1 + g.pick(20, 40, 20, 20)
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
		// root on NVMe. A limit of the test firmware, not a finding.
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
		if fourK && b != "sata" && !(bios && big) {
			lbs = 4096
		}
		seed := ""
		if seeded && i > 0 && g.chance(35) {
			// "ambiv" is left to the handwritten safety-ambiv case: the
			// safety check lets such a disk through (see that case), and
			// drawing it here would fail every case that targets one.
			seed = g.choose("ext4", "luks", "dos")
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
			if name == "mdroot" || s.a.UUID == "" || !s.intact {
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
				if !g.heavy && g.chance(15) {
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

		// Flags.
		if i == 0 {
			st.Destructive = g.chance(40)
		} else {
			st.Destructive = g.chance(80)
		}
		st.WipeAll = st.Destructive && g.chance(30)
		// Interruptions, never on the last step.
		if !last && g.chance(30) && !(g.heavy && i == 0) {
			if g.chance(60) {
				st.FatalAt = []string{"partitioning", "rsyncing_os", "rsyncing_os", "installing_bootloader", "finalizing", "finalizing"}[g.rng.Intn(6)]
			} else {
				st.KillAtCmd = 1 + g.rng.Intn(propPlanLen(st.Layout))
			}
		}
		st.Expect, st.Why = g.predict(st)
		st.Hazard = g.hazard(st)
		// Known finding: with a top-level mount besides "/", the agent may
		// try to unmount the root first, fail, and reboot with it mounted
		// (TestStoragePropFindings/bios-topmounts-*). On BIOS, GRUB then
		// reads a filesystem whose journal was never replayed and may not
		// find its own modules, so the outcome is open.
		if st.Expect == "ok" && st.Hazard != "" {
			// Either hazard can leave the machine in the initramfs.
			st.Expect = "ok|noboot"
			st.Why = "valid layout, but hazard " + st.Hazard + " (known finding)"
		}
		if bios && st.Expect == "ok" && propHasTopLevelMount(st.Layout) {
			st.Expect = "ok|noboot"
			st.Why = "valid layout, but the root may be left mounted at reboot (known finding)"
		}
		// A second boot goes PXE, then the boot-local script, which on UEFI
		// tries the firmware's first two disks and boots whatever ESP it
		// finds there, stale or not (two known findings, see
		// TestStoragePropFindings). Until those are fixed the reboot check
		// runs only when the ESP is on the firmware's first disk.
		espFirst := bios
		for _, dl := range st.Layout.Disks {
			for _, p := range dl.Parts {
				if p.ESP && dl.Disk == st.Attach[0] {
					espFirst = true
				}
			}
		}
		st.Reboot = st.Expect == "ok" && espFirst && g.chance(50)
		g.apply(st)
		c.Steps = append(c.Steps, st)
		lay := st.Layout
		prev = &lay
	}
	return c
}

// propHasTopLevelMount reports whether the layout mounts anything directly
// under the root, such as /data.
func propHasTopLevelMount(l propLayout) bool {
	top := func(mp string) bool { return mp != "" && mp != "/" && strings.Count(mp, "/") == 1 }
	for _, dl := range l.Disks {
		for _, p := range dl.Parts {
			if top(p.Mount) {
				return true
			}
		}
	}
	for _, a := range l.Arrays {
		if top(a.Mount) {
			return true
		}
	}
	return false
}

// propPlanLen is the number of storage commands a layout plans, counted from
// the layout itself: per disk one zap, one sgdisk per partition, a re-read
// when it has partitions and one mkfs per formatted partition; per array one
// mdadm and one mkfs when formatted; two commands per mount.
func propPlanLen(l propLayout) int {
	n := 0
	for _, dl := range l.Disks {
		n++
		if len(dl.Parts) > 0 {
			n++
		}
		for _, p := range dl.Parts {
			n++
			if p.FS != "" {
				n++
			}
			if p.Mount != "" {
				n += 2
			}
		}
	}
	for _, a := range l.Arrays {
		n++
		if a.FS != "" && a.Type == "raid" {
			n++
		}
		if a.Mount != "" {
			n += 2
		}
	}
	return n
}

const propGenSeeds = 2000

// propHeavySeed is the first seed of the preserve-heavy family.
const propHeavySeed = 1000

func TestStoragePropGen(t *testing.T) {
	for seed := int64(0); seed < propGenSeeds; seed++ {
		c := propGenCase(seed)
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}

// TestStoragePropGenShape needs no VM. It runs every generated storage script
// against a fake inventory and feeds the result to the real planner, so a
// generator that emits layouts the planner rejects is caught in seconds, and
// it prints how the generated cases are distributed.
func TestStoragePropGenShape(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	count := map[string]int{}
	bump := func(k string) { count[k]++ }
	// 400 seeds of the plain mix and 200 of the preserve-heavy family.
	var seeds []int64
	for seed := int64(0); seed < 400; seed++ {
		seeds = append(seeds, seed)
	}
	for seed := int64(propHeavySeed); seed < propHeavySeed+200; seed++ {
		seeds = append(seeds, seed)
	}
	for _, seed := range seeds {
		c := propGenCase(seed)
		if seed >= propHeavySeed {
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
				name := fmt.Sprintf("vd%c", 'a'+k)
				if c.disk(id).Bus == "nvme" {
					name = fmt.Sprintf("nvme%dn1", k)
				}
				inv.Disks = append(inv.Disks, invDisk{Name: name, SizeBytes: c.disk(id).Bytes})
				blk = append(blk, storage.BlockDevice{Name: name, SizeBytes: c.disk(id).Bytes})
			}
			invPath := filepath.Join(dir, "inv.json")
			b, _ := json.Marshal(inv)
			if err := os.WriteFile(invPath, b, 0600); err != nil {
				t.Fatal(err)
			}
			scriptPath := filepath.Join(dir, "script.py")
			if err := os.WriteFile(scriptPath, []byte(propRenderScript(c, st)), 0700); err != nil {
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
			if err != nil {
				t.Errorf("%s step %d: generated layout rejected by the planner: %v\n%s", c.Name, i, err, c.JSON())
				continue
			}
			if got, want := len(cmds), propPlanLen(st.Layout); got != want {
				t.Errorf("%s step %d: planner emits %d commands, propPlanLen says %d", c.Name, i, got, want)
			}
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("PROPSHAPE %-40s %d", k, count[k])
	}
}

// TestStoragePropReplay runs the cases in testdata/prop-replay.json, each a
// PROPCASE line copied from an earlier run's log. A strategy change can
// alter what a seed generates; a saved case cannot drift.
func TestStoragePropReplay(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "prop-replay.json"))
	if os.IsNotExist(err) {
		t.Skip("no testdata/prop-replay.json")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cases []propCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse testdata/prop-replay.json: %v", err)
	}
	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runPropCase(t, c)
		})
	}
}
