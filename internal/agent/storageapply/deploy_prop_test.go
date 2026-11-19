//go:build !integration

package storageapply

// Model-based property test of the agent's storage path: generate a host
// (disks with prior state: partition tables, filesystems, md members,
// auto-assembled arrays, DDF containers, disks that cannot be probed or
// carry two filesystem signatures, preserved arrays, retained data disks,
// /dev/disk/by-id links), a layout and the --destructive/--wipe-all-disks
// flags; drive the same sequence cmd/slagworks-agent's runDeploy drives
// (Inventory, Plan, CheckDisksClean, CheckStaleArrayMembers,
// PreflightPreserve, WipeStaleState, WipeBystanderDisks, StopAutoAssembly,
// the plan's commands, VerifyPreserveArrays) against fakeHost; then check
// the safety contracts against the host's PHYSICAL state, never against
// the names the agent used.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

const preserveUUID = "5eed0000111122223333444455556666"

type hostDiskSpec struct {
	Name   string
	Bytes  int64
	Sector int64
	Tran   string
	Role   string // target, bare, retain, preserve, bystander
	Prior  string // clean, gpt-fs, md-part, md-whole, ddf, two-fs, unreadable, preserve-whole, preserve-part, preserve-p3
	// ActiveAs names the md device the initrd auto-assembled this disk's
	// prior array as ("" for none).
	ActiveAs string
	ByID     string // /dev/disk/by-id link name for the disk
}

type hostCase struct {
	Disks       []hostDiskSpec
	Layout      storage.Layout
	Destructive bool
	WipeAll     bool
	NoWipefs    bool
}

func (hc hostCase) build() *fakeHost {
	h := newFakeHost()
	for i, ds := range hc.Disks {
		d := h.addDisk(ds.Name, ds.Bytes, ds.Tran)
		d.model = fmt.Sprintf("MODEL-%d", i)
		d.sector = ds.Sector
		if ds.ByID != "" {
			h.links[ds.ByID] = ds.Name
			h.links[ds.ByID+"-part1"] = kernelPartName(ds.Name, 1)
		}
		switch ds.Prior {
		case "gpt-fs":
			h.addPartition(ds.Name, 1, "vfat")
			h.addPartition(ds.Name, 2, "ext4")
		case "md-part":
			h.addPartition(ds.Name, 1, "vfat")
			h.addPartition(ds.Name, 2, "md:"+oldArrayUUID(ds.ActiveAs))
		case "md-whole":
			d.whole.sigs = []string{"md:" + oldArrayUUID(ds.ActiveAs)}
		case "ddf":
			d.whole.sigs = []string{"ddf"}
		case "two-fs":
			d.whole.sigs = []string{"ext4", "xfs"}
		case "unreadable":
			d.whole.sigs = []string{"ext4"}
			d.unreadable = true
		case "preserve-whole":
			d.whole.sigs = []string{"md:" + preserveUUID}
		case "preserve-part":
			h.addPartition(ds.Name, 1, "md:"+preserveUUID)
		case "preserve-p3":
			h.addPartition(ds.Name, 1, "vfat")
			h.addPartition(ds.Name, 2, "ext4")
			h.addPartition(ds.Name, 3, "md:"+preserveUUID)
			if ds.ByID != "" {
				h.links[ds.ByID+"-part3"] = kernelPartName(ds.Name, 3)
			}
		}
	}
	// Auto-assembled arrays.
	groups := map[string][]string{}
	for _, ds := range hc.Disks {
		if ds.ActiveAs == "" {
			continue
		}
		switch ds.Prior {
		case "md-part", "preserve-part":
			n := 2
			if ds.Prior == "preserve-part" {
				n = 1
			}
			groups[ds.ActiveAs] = append(groups[ds.ActiveAs], kernelPartName(ds.Name, n))
		case "md-whole", "preserve-whole":
			groups[ds.ActiveAs] = append(groups[ds.ActiveAs], ds.Name)
		case "ddf":
			h.arrays["md127"] = &hArray{name: "md127", members: []string{ds.Name}, container: true}
			h.arrays["md126"] = &hArray{name: "md126", members: nil, parent: "md127"}
		}
	}
	for name, members := range groups {
		if _, taken := h.arrays[name]; taken {
			continue
		}
		u := oldArrayUUID(name)
		if strings.HasPrefix(name, "md12") && hasPreservePrior(hc, members) {
			u = preserveUUID
		}
		h.arrays[name] = &hArray{name: name, members: members, uuid: u}
	}
	h.preserved[preserveUUID] = hArray{fs: "xfs", fsUUID: "preserved-fs"}
	h.noWipefs = hc.NoWipefs
	return h
}

func hasPreservePrior(hc hostCase, members []string) bool {
	for _, ds := range hc.Disks {
		for _, m := range members {
			if parentDisk(m) == ds.Name && strings.HasPrefix(ds.Prior, "preserve") {
				return true
			}
		}
	}
	return false
}

// oldArrayUUID is the UUID of the prior array auto-assembled under the given
// md name: the name's bytes, zero-padded to 128 bits, so two arrays never
// share one.
func oldArrayUUID(activeAs string) string {
	var id [16]byte
	copy(id[:], activeAs)
	return fmt.Sprintf("%x", id)
}

var hostSizes = []int64{480103981056, 1920383410176, 3840755982336, 15360747397120}

func genHostCase(t *rapid.T, k hostKnobs) hostCase {
	var hc hostCase
	used := map[string]bool{}
	n := rapid.IntRange(1, 6).Draw(t, "nDisks")
	for i := 0; i < n; i++ {
		var name, tran string
		for {
			if k.oddNames && rapid.IntRange(0, 4).Draw(t, "odd") == 0 {
				name, tran = fmt.Sprintf("pmem%d", rapid.IntRange(0, 3).Draw(t, "pmemIdx")), ""
			} else if rapid.Bool().Draw(t, "nvme") {
				name, tran = fmt.Sprintf("nvme%dn1", rapid.IntRange(0, 9).Draw(t, "nvmeIdx")), "nvme"
			} else {
				name, tran = fmt.Sprintf("sd%c", 'a'+rapid.IntRange(0, 7).Draw(t, "sdIdx")), "sata"
			}
			if !used[name] {
				used[name] = true
				break
			}
		}
		ds := hostDiskSpec{Name: name, Bytes: rapid.SampledFrom(hostSizes).Draw(t, "bytes"), Tran: tran,
			Sector: rapid.SampledFrom([]int64{512, 4096}).Draw(t, "sector")}
		ds.Role = rapid.SampledFrom([]string{"target", "target", "bare", "retain", "preserve", "bystander", "bystander"}).Draw(t, "role")
		if i == 0 {
			ds.Role = "target"
		}
		switch ds.Role {
		case "preserve":
			ds.Prior = rapid.SampledFrom([]string{"preserve-whole", "preserve-part"}).Draw(t, "preservePrior")
			if rapid.Bool().Draw(t, "preserveActive") {
				ds.ActiveAs = "md125"
			}
		case "retain":
			ds.Prior = "gpt-fs"
		default:
			priors := []string{"clean", "clean", "gpt-fs", "md-part", "md-whole", "ddf"}
			if k.faultyDisks {
				priors = append(priors, "two-fs", "unreadable")
			}
			ds.Prior = rapid.SampledFrom(priors).Draw(t, "prior")
			if ds.Role == "target" && k.collide && rapid.IntRange(0, 5).Draw(t, "collide") == 0 {
				ds.Prior = "preserve-p3"
			}
			if ds.Prior == "md-part" || ds.Prior == "md-whole" {
				names := []string{"", "md127", "md126"}
				if k.lowMDNames {
					names = append(names, "md0", "md1")
				}
				ds.ActiveAs = rapid.SampledFrom(names).Draw(t, "activeAs")
			}
			if ds.Prior == "ddf" {
				ds.ActiveAs = "ddf"
			}
		}
		if k.byID && rapid.Bool().Draw(t, "hasByID") {
			ds.ByID = fmt.Sprintf("/dev/disk/by-id/nvme-SERIAL%04d", i)
		}
		hc.Disks = append(hc.Disks, ds)
	}
	hc.Destructive = rapid.Bool().Draw(t, "destructive")
	hc.WipeAll = rapid.Bool().Draw(t, "wipeAll")
	hc.NoWipefs = k.noWipefs && rapid.IntRange(0, 9).Draw(t, "noWipefs") == 0

	devices := map[string]storage.Device{}
	mounts := map[string]storage.Mount{}
	var bare, preserveMembers []string
	rootDone := false
	for _, ds := range hc.Disks {
		if ds.Prior == "preserve-p3" {
			// A preserve member on an undeclared partition of a disk the
			// plan zaps: static validation cannot see it; the runtime
			// guard must refuse before anything is written.
			m := kernelPartName(ds.Name, 3)
			switch {
			case ds.ByID != "" && rapid.Bool().Draw(t, "p3ByID"):
				m = ds.ByID + "-part3"
			case rapid.Bool().Draw(t, "p3DevPrefix"):
				m = "/dev/" + m
			}
			preserveMembers = append(preserveMembers, m)
		}
		switch ds.Role {
		case "target":
			p1, p2 := kernelPartName(ds.Name, 1), kernelPartName(ds.Name, 2)
			parts := []storage.Partition{
				{Name: p1, Size: "512M", FS: "vfat", Bootable: true},
				{Name: p2, Size: "rest", FS: "ext4"},
			}
			if rapid.IntRange(0, 3).Draw(t, "rawSlot") == 0 {
				// An unformatted, unmounted partition at slot 2: whatever
				// old bytes sit at that offset survive the plan.
				p3 := kernelPartName(ds.Name, 3)
				parts = []storage.Partition{
					{Name: p1, Size: "512M", FS: "vfat", Bootable: true},
					{Name: p2, Size: "1G"},
					{Name: p3, Size: "rest", FS: "ext4"},
				}
				p2 = p3
			}
			devices[ds.Name] = storage.Device{Type: "disk", PTable: "gpt", Partitions: parts}
			if !rootDone {
				mounts["/"] = storage.Mount{Device: p2}
				mounts["/boot/efi"] = storage.Mount{Device: p1}
				rootDone = true
			}
		case "bare":
			devices[ds.Name] = storage.Device{Type: "disk"}
			bare = append(bare, ds.Name)
		case "retain":
			devices[ds.Name] = storage.Device{Type: "retain"}
		case "preserve":
			var m string
			if ds.Prior == "preserve-part" {
				m = kernelPartName(ds.Name, 1)
			} else {
				m = ds.Name
			}
			switch {
			case ds.ByID != "" && rapid.Bool().Draw(t, "memberByID"):
				m = ds.ByID
				if ds.Prior == "preserve-part" {
					m += "-part1"
				}
			case rapid.Bool().Draw(t, "memberDevPrefix"):
				m = "/dev/" + m
			}
			preserveMembers = append(preserveMembers, m)
		}
	}
	if len(bare) >= 2 {
		devices["mddata"] = storage.Device{Type: "raid", Level: 0, Members: bare, FS: "xfs"}
		mounts["/srv"] = storage.Mount{Device: "mddata"}
	}
	if len(preserveMembers) > 0 {
		devices["mdkeep"] = storage.Device{Type: "preserve", Members: preserveMembers, FS: "xfs", ArrayUUID: preserveUUID}
		mounts["/data"] = storage.Mount{Device: "mdkeep"}
	}
	hc.Layout = storage.Layout{Devices: devices, Mounts: mounts}
	return hc
}

// hostKnobs switches classes of host oddity on.
type hostKnobs struct {
	byID, oddNames, faultyDisks, noWipefs, lowMDNames, collide bool
}

var allHostKnobs = hostKnobs{byID: true, oddNames: true, faultyDisks: true, noWipefs: true, lowMDNames: true, collide: true}

// deploy mirrors cmd/slagworks-agent/deploy.go's storage section.
func deploy(ctx *agent.Context, h *fakeHost, hc hostCase) (string, error) {
	disks, err := Inventory(ctx)
	if err != nil {
		return "inventory", err
	}
	cmds, err := storage.Plan(hc.Layout, disks)
	if err != nil {
		return "plan", err
	}
	if !hc.Destructive {
		if err := CheckDisksClean(ctx, hc.Layout); err != nil {
			return "safety", err
		}
	}
	if err := CheckStaleArrayMembers(ctx, hc.Layout, hc.WipeAll); err != nil {
		return "stale-members", err
	}
	if err := PreflightPreserve(ctx, hc.Layout); err != nil {
		return "preflight", err
	}
	if hc.Destructive {
		WipeStaleState(ctx, hc.Layout)
	}
	if hc.WipeAll {
		if !hc.Destructive {
			return "flags", errors.New("--wipe-all-disks requires --destructive")
		}
		if err := WipeBystanderDisks(ctx, hc.Layout); err != nil {
			return "wipe-all-disks", err
		}
	}
	if !hc.Destructive && HasPreserveDevices(hc.Layout) {
		StopAutoAssembly(ctx)
	}
	mountPhase := len(cmds)
	for i, c := range cmds {
		if c.Name == "mkdir" || c.Name == "mount" {
			mountPhase = i
			break
		}
	}
	run := func(c storage.Command) error {
		if c.Name == "blockdev" {
			return h.Run(c.Name, c.Args...) // RunCommand would retry with 1s sleeps
		}
		return RunCommand(ctx, c)
	}
	for _, c := range cmds[:mountPhase] {
		if err := run(c); err != nil {
			return "storage: " + c.String(), err
		}
	}
	if err := VerifyPreserveArrays(ctx, hc.Layout); err != nil {
		return "preserve verify", err
	}
	for _, c := range cmds[mountPhase:] {
		if err := run(c); err != nil {
			return "storage: " + c.String(), err
		}
	}
	return "done", nil
}

// physicalOf resolves a layout member name to the whole disk it lives on,
// through the host's own device table and symlinks.
func physicalOf(h *fakeHost, member string) (string, bool) {
	p := member
	if !strings.HasPrefix(p, "/dev/") {
		p = "/dev/" + p
	}
	if t, ok := h.links[p]; ok {
		p = "/dev/" + t
	}
	name := strings.TrimPrefix(p, "/dev/")
	if _, ok := h.disks[name]; ok {
		return name, true
	}
	for _, d := range h.disks {
		for n := 1; n <= 8; n++ {
			if kernelPartName(d.name, n) == name {
				return d.name, true
			}
		}
	}
	return "", false
}

type hostViolation struct{ cat, detail string }

func checkHost(hc hostCase, h *fakeHost, stage string) []hostViolation {
	var vs []hostViolation
	add := func(cat, f string, a ...any) { vs = append(vs, hostViolation{cat, fmt.Sprintf(f, a...)}) }

	targets, retained, protected := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for name, d := range hc.Layout.Devices {
		switch d.Type {
		case "disk":
			targets[name] = true
		case "retain":
			retained[name] = true
		case "preserve", "raid":
			for _, m := range d.Members {
				if pd, ok := physicalOf(h, m); ok {
					if d.Type == "preserve" {
						protected[pd] = true
					}
				}
			}
		}
	}
	// Mounted non-preserve devices will receive the OS image.
	for kn := range h.mounted {
		if a, ok := h.arrays[kn]; ok && a.uuid == preserveUUID {
			continue
		}
		if pd, ok := physicalOf(h, kn); ok {
			h.writes[pd] = true
		}
	}
	for d := range h.writes {
		switch {
		case retained[d]:
			add("retained-written", "retained disk %s was written", d)
		case protected[d]:
			add("preserve-written", "disk %s holds a preserved array member and was written", d)
		case targets[d]:
		case hc.WipeAll && hc.Destructive:
		default:
			add("stray-write", "disk %s is neither a layout target nor (with --wipe-all-disks) a bystander, but was written", d)
		}
	}
	// Preserved superblocks survive, whatever happened.
	for _, ds := range hc.Disks {
		if ds.Prior != "preserve-whole" && ds.Prior != "preserve-part" {
			continue
		}
		d := h.disks[ds.Name]
		var dev *hDev
		if ds.Prior == "preserve-whole" {
			dev = d.whole
		} else if dev = d.kernel[1]; dev == nil {
			dev = d.orphan[1]
		}
		if dev == nil || dev.mdUUID() != preserveUUID {
			add("preserve-destroyed", "preserved member on %s lost its superblock (stage %s)", ds.Name, stage)
		}
	}
	for _, ds := range hc.Disks {
		if ds.Prior != "preserve-p3" {
			continue
		}
		d := h.disks[ds.Name]
		dev := d.kernel[3]
		if dev == nil {
			dev = d.orphan[3]
		}
		if dev == nil || dev.mdUUID() != preserveUUID {
			add("preserve-destroyed", "preserved member %s on target disk %s lost its superblock (stage %s)", kernelPartName(ds.Name, 3), ds.Name, stage)
		}
	}
	if stage != "done" {
		return vs
	}
	// Completed deploys: wipe completeness and the preserved array.
	created := map[string]bool{}
	for _, a := range h.arrays {
		created[a.uuid] = true
	}
	for _, ds := range hc.Disks {
		d := h.disks[ds.Name]
		devs := []*hDev{d.whole}
		for _, pd := range d.kernel {
			devs = append(devs, pd)
		}
		switch {
		case targets[ds.Name] && hc.Destructive:
			for _, dev := range devs {
				if u := dev.mdUUID(); u != "" && !created[u] {
					add("fossil-superblock", "target %s still carries md superblock %s from a prior array after a destructive deploy", ds.Name, u)
				}
			}
		case hc.WipeAll && hc.Destructive && !targets[ds.Name] && !retained[ds.Name] && !protected[ds.Name]:
			if len(d.whole.sigs) > 0 {
				add("bystander-not-wiped", "bystander %s still carries %v after --wipe-all-disks", ds.Name, d.whole.sigs)
			}
			for num, pd := range d.orphan {
				if pd.mdUUID() != "" {
					add("bystander-not-wiped", "bystander %s partition %d still carries md superblock %s", ds.Name, num, pd.mdUUID())
				}
			}
		}
	}
	if _, ok := hc.Layout.Devices["mdkeep"]; ok {
		found := false
		for kn, a := range h.arrays {
			if a.uuid == preserveUUID && h.mounted[kn] {
				found = true
			}
		}
		if !found {
			add("preserve-not-mounted", "deploy completed but the preserved array is not assembled and mounted")
		}
	}
	return vs
}

// planMDNameTaken reports whether a deploy without --destructive would find
// one of the /dev/mdN nodes its plan uses (md0 upward, one per raid or
// preserve device) held by an auto-assembled array that nothing stops first:
// only a layout with a preserve array stops auto-assembly on that path.
func planMDNameTaken(hc hostCase, h *fakeHost) bool {
	if hc.Destructive {
		return false
	}
	planned := 0
	for _, d := range hc.Layout.Devices {
		switch d.Type {
		case "preserve":
			return false
		case "raid":
			planned++
		}
	}
	for i := 0; i < planned; i++ {
		if _, active := h.arrays[fmt.Sprintf("md%d", i)]; active {
			return true
		}
	}
	return false
}

// initialDirty reports whether a disk-type target carries any signature
// (or cannot be probed) before the deploy starts.
func initialDirty(hc hostCase) []string {
	var out []string
	for _, ds := range hc.Disks {
		if d, ok := hc.Layout.Devices[ds.Name]; ok && d.Type == "disk" && ds.Prior != "clean" {
			out = append(out, ds.Name+"("+ds.Prior+")")
		}
	}
	return out
}

type hostStats struct {
	mu     sync.Mutex
	stages map[string]int
	priors map[string]int
}

func (s *hostStats) log(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range []map[string]int{s.stages, s.priors} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("  %-70s %d", k, m[k])
		}
	}
}

func quietCtx(h *fakeHost) *agent.Context {
	return &agent.Context{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Exec: h}
}

// TestDeploy_Property_StorageSafety: across generated hosts and flags,
//   - without --destructive, a deploy whose targets carry any signature (or
//     cannot be probed) writes nothing;
//   - no disk other than a layout disk target is ever written, except
//     bystanders under --destructive --wipe-all-disks;
//   - retained disks and disks holding preserve members are never written,
//     and a preserved array's member superblocks always survive;
//   - a host whose targets are clean (or --destructive) and whose preserved
//     array is present deploys to completion.
func TestDeploy_Property_StorageSafety(t *testing.T) {
	k := allHostKnobs
	st := &hostStats{stages: map[string]int{}, priors: map[string]int{}}
	rapid.Check(t, func(rt *rapid.T) {
		hc := genHostCase(rt, k)
		h := hc.build()
		mdNameTaken := planMDNameTaken(hc, h)
		stage, err := deploy(quietCtx(h), h, hc)
		st.mu.Lock()
		st.stages[fmt.Sprintf("stage=%s destructive=%v wipeAll=%v", firstField(stage), hc.Destructive, hc.WipeAll)]++
		for _, ds := range hc.Disks {
			st.priors["prior="+ds.Role+"/"+ds.Prior]++
		}
		st.mu.Unlock()
		fail := func(f string, a ...any) {
			rt.Logf("case: %+v", hc.Disks)
			rt.Logf("layout: %s", mustJSONHost(hc.Layout))
			rt.Logf("flags: destructive=%v wipeAll=%v noWipefs=%v", hc.Destructive, hc.WipeAll, hc.NoWipefs)
			rt.Logf("stopped at %q: %v", stage, err)
			for _, c := range h.calls {
				rt.Logf("  exec %s", c)
			}
			rt.Fatalf(f, a...)
		}
		if vs := checkHost(hc, h, stage); len(vs) > 0 {
			fail("storage safety violated: %+v", vs)
		}
		dirty := initialDirty(hc)
		if !hc.Destructive && len(dirty) > 0 && len(h.writes) > 0 {
			fail("non-destructive deploy wrote %v although targets %v carried existing data", keys(h.writes), dirty)
		}
		// Liveness: a deploy the operator is entitled to (clean targets, or
		// --destructive) on a healthy host must complete.
		collides := false
		for _, ds := range hc.Disks {
			collides = collides || ds.Prior == "preserve-p3"
		}
		flagsValid := hc.Destructive || !hc.WipeAll
		// Not expected to complete: a deploy without --destructive whose
		// layout has no preserve array never stops the arrays the initrd
		// auto-assembled, so when one of them sits on a /dev/mdN the plan
		// creates an array at, mdadm --create fails after the target disks
		// were partitioned. The safety checks above still cover that case.
		expectDone := (hc.Destructive || len(dirty) == 0) && flagsValid && !hc.NoWipefs && !collides && !mdNameTaken
		if expectDone && stage != "done" {
			fail("deploy should have completed but stopped at %q: %v", stage, err)
		}
	})
	st.log(t)
}

func firstField(s string) string {
	if i := strings.IndexAny(s, " :"); i > 0 {
		return s[:i]
	}
	return s
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustJSONHost(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
