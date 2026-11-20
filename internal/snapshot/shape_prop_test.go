package snapshot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// Production-shape properties of the site snapshot build.
//
// The worlds come from shape_gen_prop_test.go (drawn to the shapes surveyed
// in a real deployment) or from a recording of one
// (shape_record_prop_test.go). Every build goes through the real Dispatcher
// (scope resolution, prefix discovery, gather, Build); only NetBox and
// Postgres are replaced, by shapeworld.NB and shapeworld.Store.
//
// Properties:
//
//   - model: a config's snapshot equals an independent recomputation from the
//     world (machine set, per-machine facts, reservations, fabric and OOB
//     segment config, relay pools), whatever order NetBox and the store list
//     things in;
//   - totality: after any device-confined NetBox edit, every config still
//     builds. One datum on one device must not cost a site its snapshot,
//     because a config that does not build freezes its worker's view and
//     makes every commission and deploy preflight in the site fail. That
//     includes edits that make an OOB reservation ambiguous (a second OOB
//     address on an interface, an address on two MACs): those cost the MACs
//     involved their reservation and nothing else;
//   - blast radius: such an edit changes only that device's machine, its
//     reservations and its links;
//   - differential: whatever an older release built, this one builds too
//     (SLAGWORKS_SHAPE_OLDSNAP names an oldsnap binary compiled against the
//     older release, see shapeworld/oldsnap).

type shapeStats struct {
	cases, builds, edits, editsNoop, editsConflict atomic.Int64
	shuffled                                       atomic.Int64
	wholeSite, locScoped                           atomic.Int64
	maxDevices, maxIfaces, maxIPs, maxMachines     atomic.Int64
	maxIPsPerIface, maxSharedMAC, maxReservations  atomic.Int64
	maxRelayPools                                  atomic.Int64
	multiAddrIfaces, bothEndLinks, consoles        atomic.Int64
	unclaimedDevices, noSerial, noPrimary, noBMC   atomic.Int64
	scriptMatched, scriptUnmatched                 atomic.Int64
	oldBuilds, oldDiffer                           atomic.Int64

	mu    sync.Mutex
	kinds map[string]int
	diffs map[string]int
}

func (s *shapeStats) kind(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kinds == nil {
		s.kinds = map[string]int{}
	}
	s.kinds[k]++
}

func (s *shapeStats) diff(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diffs == nil {
		s.diffs = map[string]int{}
	}
	s.diffs[k]++
}

func shapeCounts(m map[string]int) string {
	var parts []string
	for k, n := range m {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (s *shapeStats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf(`cases=%d builds=%d (whole-site=%d location-scoped=%d shuffled=%d)
 edits: applied=%d not-applicable=%d leaving-an-oob-conflict=%d
 edit kinds: %s
 sizes: max_devices=%d max_interfaces=%d max_addresses=%d max_machines=%d max_reservations=%d max_relay_pools=%d
 shapes: max_addresses_on_one_interface=%d max_machines_sharing_a_mac=%d multi_address_interfaces=%d links_with_both_ends=%d consoles=%d
 devices: unclaimed=%d no_serial=%d no_primary_ip4=%d no_oob_ip=%d
 machines: network_script_matched=%d unmatched=%d
 differential: old_builds=%d snapshots_differing=%d by field: %s`,
		s.cases.Load(), s.builds.Load(), s.wholeSite.Load(), s.locScoped.Load(), s.shuffled.Load(),
		s.edits.Load(), s.editsNoop.Load(), s.editsConflict.Load(),
		shapeCounts(s.kinds),
		s.maxDevices.Load(), s.maxIfaces.Load(), s.maxIPs.Load(), s.maxMachines.Load(), s.maxReservations.Load(), s.maxRelayPools.Load(),
		s.maxIPsPerIface.Load(), s.maxSharedMAC.Load(), s.multiAddrIfaces.Load(), s.bothEndLinks.Load(), s.consoles.Load(),
		s.unclaimedDevices.Load(), s.noSerial.Load(), s.noPrimary.Load(), s.noBMC.Load(),
		s.scriptMatched.Load(), s.scriptUnmatched.Load(),
		s.oldBuilds.Load(), s.oldDiffer.Load(), shapeCounts(s.diffs))
}

// observeWorld records the shapes a world reached.
func (s *shapeStats) observeWorld(w *shapeworld.World, sd *shapeworld.StoreData) {
	m := shapeModel{w, sd}
	nDev := 0
	for _, devs := range w.Devices {
		nDev += len(devs)
		for _, d := range devs {
			claimed := false
			for _, cfg := range sd.Configs {
				claimed = claimed || m.claims(cfg, d)
			}
			if !claimed {
				s.unclaimedDevices.Add(1)
			}
			if d.Serial == "" {
				s.noSerial.Add(1)
			}
			if d.PrimaryIP4 == nil {
				s.noPrimary.Add(1)
			}
			if d.OOBIP == nil {
				s.noBMC.Add(1)
			}
		}
	}
	propMax(&s.maxDevices, nDev)
	propMax(&s.maxIfaces, len(w.Interfaces))
	propMax(&s.maxIPs, len(w.IPs))
	propMax(&s.maxMachines, len(sd.Machines))
	perIface := map[int]int{}
	for _, ip := range w.IPs {
		if ip.AssignedObjectID != nil && shapeActive(ip) {
			perIface[*ip.AssignedObjectID]++
		}
	}
	for _, n := range perIface {
		propMax(&s.maxIPsPerIface, n)
		if n > 1 {
			s.multiAddrIfaces.Add(1)
		}
	}
	macDevs := map[string]map[int]bool{}
	for _, i := range w.Interfaces {
		if mac := shapeMAC(i); mac != "" && i.Device != nil {
			if macDevs[mac] == nil {
				macDevs[mac] = map[int]bool{}
			}
			macDevs[mac][i.Device.ID] = true
		}
	}
	for _, devs := range macDevs {
		propMax(&s.maxSharedMAC, len(devs))
	}
	ends := map[string]int{}
	for _, ip := range w.IPs {
		if p, err := netip.ParsePrefix(ip.Address); err == nil && p.Bits() == 31 && shapeActive(ip) {
			ends[p.Masked().String()]++
		}
	}
	for _, n := range ends {
		if n > 1 {
			s.bothEndLinks.Add(1)
		}
	}
}

func (s *shapeStats) observeSnap(cfg model.SiteConfig, snap model.SiteSnapshot) {
	s.builds.Add(1)
	if cfg.NetBoxLocation == "" {
		s.wholeSite.Add(1)
	} else {
		s.locScoped.Add(1)
	}
	propMax(&s.maxReservations, len(snap.Reservations))
	propMax(&s.maxRelayPools, len(snap.DHCP.RelayPools))
	for _, ms := range snap.Machines {
		if ms.Console != nil {
			s.consoles.Add(1)
		}
		if ms.NetworkScript != nil {
			s.scriptMatched.Add(1)
		} else {
			s.scriptUnmatched.Add(1)
		}
	}
}

// shapeCase is one drawn world plus a sequence of edits.
type shapeCase struct {
	rt     *rapid.T
	st     *shapeStats
	w      *shapeworld.World
	sd     *shapeworld.StoreData
	trace  []string
	edited []int // devices the applied edits touched
}

// editOnce draws and applies one device-confined edit, and counts the ones
// that leave some scope's OOB reservation set ambiguous.
func (c *shapeCase) editOnce(kinds []string) (dev int, fp shapeFootprint, applied bool) {
	m := shapeModel{c.w, c.sd}
	cfg := rapid.SampledFrom(c.sd.Configs).Draw(c.rt, "editConfig")
	devs := m.claimed(cfg)
	if len(devs) == 0 {
		c.st.editsNoop.Add(1)
		return 0, fp, false
	}
	dev = rapid.SampledFrom(devs).Draw(c.rt, "editDevice").ID
	// Uniform over the kinds: SampledFrom leans towards the head of a slice.
	kind := kinds[int(rapid.Uint32().Draw(c.rt, "editKind"))%len(kinds)]
	fp.add(c.w, dev)
	next := shapeClone(c.w)
	desc := newShapeEditor(c.rt, next, c.sd).apply(kind, cfg, dev)
	if desc == "" {
		c.st.editsNoop.Add(1)
		return dev, fp, false
	}
	nm := shapeModel{next, c.sd}
	for _, cc := range c.sd.Configs {
		if nm.oobConflict(cc) != "" {
			c.st.editsConflict.Add(1)
			break
		}
	}
	fp.add(next, dev)
	c.w = next
	c.trace = append(c.trace, desc)
	c.edited = append(c.edited, dev)
	c.st.edits.Add(1)
	c.st.kind(kind)
	return dev, fp, true
}

func (c *shapeCase) fatalf(format string, args ...any) {
	c.rt.Helper()
	c.rt.Fatalf("%s\nedits applied:\n  %s", fmt.Sprintf(format, args...), strings.Join(c.trace, "\n  "))
}

// checkModel builds every config and compares it with the model, in NetBox
// order and (seed != 0) with every list reordered.
func (c *shapeCase) checkModel(seed int64, prefixesToo bool) {
	m := shapeModel{c.w, c.sd}
	for _, cfg := range c.sd.Configs {
		snap, err := shapeBuild(c.w, c.sd, cfg.Slug, 0)
		if err != nil {
			c.fatalf("config %s does not build: %v", cfg.Slug, err)
		}
		c.st.observeSnap(cfg, snap)
		if d := shapeDiff(m.expect(cfg), shapeProject(snap)); d != "" {
			c.fatalf("config %s: snapshot disagrees with the model: %s", cfg.Slug, d)
		}
		c.st.shuffled.Add(1)
		build := shapeBuild
		if prefixesToo {
			build = shapeBuildAnyOrder
		}
		again, err := build(c.w, c.sd, cfg.Slug, seed)
		if err != nil {
			c.fatalf("config %s builds in NetBox order but not with lists reordered (seed %d): %v", cfg.Slug, seed, err)
		}
		if propCanon(again) != propCanon(snap) {
			c.fatalf("config %s: snapshot depends on list order (seed %d): %s", cfg.Slug, seed, shapeDiff(shapeProject(snap), shapeProject(again)))
		}
	}
}

// shapeRunModel checks worlds as generated plus up to maxEdits
// device-confined edits against the model, each config built once in NetBox
// order and once with every list reordered (the prefix lists too when
// prefixesToo).
func shapeRunModel(t *testing.T, k shapeKnobs, prefixesToo bool, maxEdits int) {
	shapeQuiet(t)
	st := &shapeStats{}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		g := genShapeWorld(rt, k)
		c := &shapeCase{rt: rt, st: st, w: g.w, sd: g.sd}
		for range rapid.IntRange(0, maxEdits).Draw(rt, "nEdits") {
			c.editOnce(shapeEditKinds)
		}
		st.observeWorld(c.w, c.sd)
		c.checkModel(rapid.Int64Range(1, 1<<30).Draw(rt, "shuffleSeed"), prefixesToo)
	})
	t.Logf("coverage:\n%s", st)
}

// TestShapeModel: one production prefix per site or location, every list
// reordered.
func TestShapeModel(t *testing.T) {
	shapeRunModel(t, shapeKnobs{}, true, 4)
}

// TestShapeModelLarge: the same at the surveyed scale (up to ~200 devices,
// ~1500 interfaces in a world).
func TestShapeModelLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("large worlds")
	}
	shapeRunModel(t, shapeKnobs{Large: true}, true, 4)
}

// TestShapeModelMultiProd lets a site or location carry several production
// prefixes (disjoint, nested), with the prefix lists reordered too: the
// fabric DHCP subnet follows the config's ephemeral pool, not list order.
func TestShapeModelMultiProd(t *testing.T) {
	shapeRunModel(t, shapeKnobs{MultiProd: true}, true, 4)
}

func shapeRunBlast(t *testing.T, k shapeKnobs, kinds []string) {
	shapeQuiet(t)
	st := &shapeStats{}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		g := genShapeWorld(rt, k)
		c := &shapeCase{rt: rt, st: st, w: g.w, sd: g.sd}
		shapeCheckBlast(c, kinds)
	})
	t.Logf("coverage:\n%s", st)
}

// shapeCheckBlast builds every config, applies one edit, builds again, and
// requires the build to succeed and the difference to stay within the edited
// device.
func shapeCheckBlast(c *shapeCase, kinds []string) {
	before := map[string]model.SiteSnapshot{}
	for _, cfg := range c.sd.Configs {
		snap, err := shapeBuild(c.w, c.sd, cfg.Slug, 0)
		if err != nil {
			c.fatalf("config %s does not build before any edit: %v", cfg.Slug, err)
		}
		before[cfg.Slug] = snap
	}
	dev, fp, applied := c.editOnce(kinds)
	if !applied {
		return
	}
	c.st.observeWorld(c.w, c.sd)
	for _, cfg := range c.sd.Configs {
		after, err := shapeBuild(c.w, c.sd, cfg.Slug, 0)
		if err != nil {
			c.fatalf("one edit to device %d stops config %s from building: %v", dev, cfg.Slug, err)
		}
		c.st.observeSnap(cfg, after)
		if d := shapeBlastRadius(before[cfg.Slug], after, dev, fp); d != "" {
			c.fatalf("one edit to device %d changed config %s beyond that device: %s", dev, cfg.Slug, d)
		}
		// Whatever the edit made ambiguous gets no reservation; every other
		// MAC keeps exactly its own.
		m := shapeModel{c.w, c.sd}
		want, got := m.unambiguousReservations(cfg), shapeProject(after).Reservations
		if fmt.Sprint(want) != fmt.Sprint(got) {
			c.fatalf("config %s after an edit to device %d: reservations %v, want exactly the unambiguous ones %v", cfg.Slug, dev, got, want)
		}
	}
}

// TestShapeEditBlastRadius: one device-confined edit never stops a config
// from building and never changes another device's part of a snapshot.
func TestShapeEditBlastRadius(t *testing.T) {
	shapeRunBlast(t, shapeKnobs{MultiProd: true}, shapeEditKinds)
}

// TestShapeOOBConflictCostsOneDevice draws only the edits that can make an
// OOB reservation ambiguous: a second OOB address on an interface (a gateway
// VIP modelled on the gateway NIC, a renumbering in two saves), an OOB
// address modelled twice, a MAC shared by two OOB-addressed interfaces.
func TestShapeOOBConflictCostsOneDevice(t *testing.T) {
	shapeRunBlast(t, shapeKnobs{}, shapeConflictKinds)
}

// ---- differential against an older release ---------------------------------

type shapeOldProc struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	in  *json.Encoder
	out *bufio.Reader
}

type shapeOldAnswer struct {
	OK       bool                `json:"ok"`
	Err      string              `json:"err"`
	Snapshot *model.SiteSnapshot `json:"snapshot"`
}

func shapeStartOld(t *testing.T) *shapeOldProc {
	t.Helper()
	bin := os.Getenv("SLAGWORKS_SHAPE_OLDSNAP")
	if bin == "" {
		t.Skip("set SLAGWORKS_SHAPE_OLDSNAP to an oldsnap binary built against an older release (see internal/snapshot/shapeworld/oldsnap)")
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Wait()
	})
	return &shapeOldProc{cmd: cmd, in: json.NewEncoder(stdin), out: bufio.NewReaderSize(stdout, 1<<20)}
}

func (p *shapeOldProc) build(w *shapeworld.World, sd *shapeworld.StoreData, slug string) (shapeOldAnswer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ans shapeOldAnswer
	if err := p.in.Encode(map[string]any{"world": w, "store": sd, "slug": slug}); err != nil {
		return ans, fmt.Errorf("send to oldsnap: %w", err)
	}
	line, err := p.out.ReadBytes('\n')
	if err != nil {
		return ans, fmt.Errorf("read from oldsnap: %w", err)
	}
	return ans, json.Unmarshal(line, &ans)
}

// shapeIntendedDiffs removes from two projected snapshots the differences
// between releases that are intended, so that what remains must be equal:
//
//   - which address a multi-address interface's facts carry (the older
//     release kept whichever NetBox listed last), and the gateway resolved
//     from it;
//   - which end of a /31 a generated relay pool offers when both are
//     modelled, and the pools' order;
//   - a mgmt_only bond on a device with no BMC, "physical" before and "lag"
//     now;
//   - which production prefix supplies the fabric DHCP config when the
//     scope carries more than one (the older release took the first listed,
//     and for child locations whichever a map iteration reached first);
//   - the registered workers, which the older release did not list.
//
// A scope whose OOB data is ambiguous, or with a dynamic range that has no
// owner of its own, needs no entry: the older release refused to build it.
func shapeIntendedDiffs(w *shapeworld.World, sd *shapeworld.StoreData, cfg model.SiteConfig, e *shapeExpect) {
	m := shapeModel{w, sd}
	if len(m.prefixes(cfg, shapeTagProd)) > 1 {
		// Subnet, gateway, DNS and lease follow the prefix; the pool and the
		// promiscuous flag are the config's and must still agree.
		parts := strings.Split(e.DHCP, "|")
		for _, i := range []int{0, 1, 5, 6} {
			parts[i] = "*"
		}
		e.DHCP = strings.Join(parts, "|")
	}
	e.Workers = nil
	multi := map[string]bool{}
	bmclessBond := map[string]bool{}
	for _, ifc := range w.Interfaces {
		if len(m.activeIPs(ifc.ID)) > 1 {
			multi[fmt.Sprint(ifc.ID)] = true
		}
		if ifc.MgmtOnly && ifc.Type != nil && ifc.Type.Value == "lag" {
			bmclessBond[fmt.Sprint(ifc.ID)] = true
		}
	}
	for id, f := range e.Machines {
		for i, line := range f.Interfaces {
			parts := strings.Split(line, "|")
			if multi[parts[0]] {
				parts[4], parts[8] = "*", "gw=*"
			}
			if bmclessBond[parts[0]] && parts[3] != "ipmi" {
				parts[3] = "*"
			}
			f.Interfaces[i] = strings.Join(parts, "|")
		}
		e.Machines[id] = f
	}
	for i, p := range e.RelayPools {
		parts := strings.Split(p, "|")
		if strings.HasSuffix(parts[0], "/31") {
			e.RelayPools[i] = parts[0]
		}
	}
	sort.Strings(e.RelayPools)
}

func shapeCheckOld(c *shapeCase, old *shapeOldProc) {
	for _, cfg := range c.sd.Configs {
		ans, err := old.build(c.w, c.sd, cfg.Slug)
		if err != nil {
			c.rt.Fatalf("oldsnap: %v", err)
		}
		snap, newErr := shapeBuild(c.w, c.sd, cfg.Slug, 0)
		if ans.OK {
			c.st.oldBuilds.Add(1)
		}
		if ans.OK && newErr != nil {
			c.fatalf("config %s built on the older release and does not build now: %v", cfg.Slug, newErr)
		}
		if !ans.OK || newErr != nil {
			continue
		}
		c.st.observeSnap(cfg, snap)
		was, now := shapeProject(*ans.Snapshot), shapeProject(snap)
		if d := shapeDiff(was, now); d != "" {
			c.st.oldDiffer.Add(1)
			c.st.diff(strings.Fields(strings.SplitN(d, ":", 2)[0])[0])
		}
		shapeIntendedDiffs(c.w, c.sd, cfg, &was)
		shapeIntendedDiffs(c.w, c.sd, cfg, &now)
		if d := shapeDiff(was, now); d != "" {
			c.fatalf("config %s: this release's snapshot differs from the older release's beyond the intended changes: %s", cfg.Slug, d)
		}
	}
}

// TestShapeDifferentialOld: over generated worlds plus edits, the older
// release building implies this one builds, and the two snapshots agree
// outside the intended changes.
func TestShapeDifferentialOld(t *testing.T) {
	old := shapeStartOld(t)
	shapeQuiet(t)
	st := &shapeStats{}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		g := genShapeWorld(rt, shapeKnobs{MultiProd: true})
		c := &shapeCase{rt: rt, st: st, w: g.w, sd: g.sd}
		for range rapid.IntRange(0, 3).Draw(rt, "nEdits") {
			c.editOnce(shapeEditKinds)
		}
		st.observeWorld(c.w, c.sd)
		shapeCheckOld(c, old)
	})
	t.Logf("coverage:\n%s", st)
}

// ---- the same properties over a recorded deployment ------------------------

// TestShapeRecordedModel: the recorded deployment's snapshots equal the
// model's recomputation.
func TestShapeRecordedModel(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	m := shapeModel{w, sd}
	for i, cfg := range sd.Configs {
		snap, err := shapeBuild(w, sd, cfg.Slug, 0)
		if err != nil {
			t.Errorf("recorded config #%d does not build: %v", i, err)
			continue
		}
		if d := shapeDiff(m.expect(cfg), shapeProject(snap)); d != "" {
			t.Errorf("recorded config #%d disagrees with the model (run locally to see the detail; it names recorded objects)", i)
			t.Logf("detail kind: %s", strings.SplitN(d, ":", 2)[0])
			if os.Getenv("SLAGWORKS_SHAPE_VERBOSE") != "" {
				t.Log(d)
			}
		}
	}
}

// TestShapeRecordedEditBlastRadius draws single edits against the recorded
// deployment: is it one NetBox save away from a site that does not build?
func TestShapeRecordedEditBlastRadius(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	st := &shapeStats{}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		c := &shapeCase{rt: rt, st: st, w: w, sd: sd}
		shapeCheckBlast(c, shapeEditKinds)
	})
	t.Logf("coverage:\n%s", st)
}

// TestShapeRecordedOOBConflict is TestShapeOOBConflictCostsOneDevice against
// the recorded deployment.
func TestShapeRecordedOOBConflict(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	st := &shapeStats{}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		c := &shapeCase{rt: rt, st: st, w: w, sd: sd}
		shapeCheckBlast(c, shapeConflictKinds)
	})
	t.Logf("coverage:\n%s", st)
}

// TestShapeRecordedDifferentialOld compares the two releases over the
// recorded deployment and logs, per config, how many machines' facts differ
// and in which field.
func TestShapeRecordedDifferentialOld(t *testing.T) {
	w, sd := shapeRecorded(t)
	old := shapeStartOld(t)
	shapeQuiet(t)
	for i, cfg := range sd.Configs {
		ans, err := old.build(w, sd, cfg.Slug)
		if err != nil {
			t.Fatalf("oldsnap: %v", err)
		}
		snap, newErr := shapeBuild(w, sd, cfg.Slug, 0)
		if ans.OK && newErr != nil {
			t.Errorf("recorded config #%d built on the older release and does not build now: %v", i, newErr)
		}
		if !ans.OK || newErr != nil {
			t.Logf("recorded config #%d: older release builds=%v, this release builds=%v", i, ans.OK, newErr == nil)
			continue
		}
		was, now := shapeProject(*ans.Snapshot), shapeProject(snap)
		changed := 0
		for id, f := range was.Machines {
			if fmt.Sprintf("%+v", f) != fmt.Sprintf("%+v", now.Machines[id]) {
				changed++
			}
		}
		pools := 0
		for j := range was.RelayPools {
			if j < len(now.RelayPools) && was.RelayPools[j] != now.RelayPools[j] {
				pools++
			}
		}
		t.Logf("recorded config #%d: both releases build; %d of %d machines' facts differ; relay pool entries differing by position: %d of %d; reservations equal: %v; segment config equal: %v",
			i, changed, len(was.Machines), pools, len(was.RelayPools),
			fmt.Sprint(was.Reservations) == fmt.Sprint(now.Reservations), was.DHCP == now.DHCP && was.OOB == now.OOB)
		shapeIntendedDiffs(w, sd, cfg, &was)
		shapeIntendedDiffs(w, sd, cfg, &now)
		if d := shapeDiff(was, now); d != "" {
			t.Errorf("recorded config #%d: the releases differ beyond the intended changes (kind: %s)", i, strings.SplitN(d, ":", 2)[0])
			if os.Getenv("SLAGWORKS_SHAPE_VERBOSE") != "" {
				t.Log(d)
			}
		}
	}
}
