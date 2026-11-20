package snapshot

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Stateful property test for the global snapshot pipeline:
//
//	NetBox change -> webhook (real netbox.WebhookHandler, HMAC and all)
//	  -> ClaimResolver -> mirror.Mirror -> Invalidate -> Dispatcher.rebuild
//	  -> ContentHash -> push / no_change
//	plus Reconciler ticks as the safety net.
//
// Every production component is real; only NetBox (propNB) and Postgres
// (propStore) are fakes, and both are driven from one propWorld the oracles
// read directly. A generated history mutates the world (device create /
// delete / re-create with the same serial / rename / move between locations
// and sites, interface and IP edits, prefix edits, location tree edits,
// site_config create / update / delete, machine state edits, in-flight ops),
// delivers each device change as a webhook that may be dropped, duplicated,
// or deferred and delivered stale, injects NetBox failures, and interleaves
// reconcile ticks.
//
// Oracles (all independent of resolveScope / Scope / ContentHash):
//
//   - rebuild succeeds iff the model says the config is buildable;
//   - after every successful rebuild, the last pushed snapshot is the same
//     snapshot a from-scratch build produces now (no missed dispatch), and
//     pushed SeqNums only go up;
//   - the snapshot's machine set, per-machine identity, interface set and
//     primary prefix equal a recomputation from the world;
//   - ClaimResolver and ResolveOwningConfigSlug agree with a brute-force
//     ancestor walk, and no device is claimed twice; where a location
//     re-parent nests two admitted configs, a device both claim has no
//     owner and neither config builds or reconciles;
//   - a machine row is only ever deleted or re-pointed once its NetBox
//     device is gone; UUID <-> serial never changes;
//   - an in-order webhook leaves every slug's pushed snapshot fresh and the
//     device it names enrolled, without waiting for a reconcile tick;
//   - after reconcile ticks with no further change the system is at a fixed
//     point (idempotent), every claimed serialled device is enrolled, every
//     slug's pushed snapshot is fresh, and the reconciler flags exactly the
//     configs that cannot build for their scope (stale or overlapping).
//
// propKnobs select input classes. The tests at the bottom of the file run
// the combinations; two of them are skipped, each naming the behavior that
// fails it. Run deeper with -rapid.checks=N.

type propKnobs struct {
	Shuffle    bool // NetBox + store lists come back in a new order each call
	StaleHooks bool // webhooks may be dropped, duplicated, or delivered late
	Faults     bool // NetBox calls fail on a schedule
	// LocReparent lets a location be re-parented under another. That can
	// nest two configs' scopes after both were admitted, so a device can
	// have two claimers; both configs must then be refused.
	LocReparent bool
	// SerialReuse lets a device's serial change after it is set, including
	// to a serial that a machine row bound to another live device holds.
	SerialReuse bool
	// KeepIdentity requires a serial to keep its machine UUID across a
	// device delete + re-create.
	KeepIdentity bool
}

type propStats struct {
	cases, ops                                     atomic.Int64
	hooksNow, hooksDropped, hooksDup, hooksLate    atomic.Int64
	rebuildPush, rebuildSkip, rebuildErr           atomic.Int64
	rebuildFault                                   atomic.Int64
	ticks, settles                                 atomic.Int64
	enrolled, deleted, rebound, frozen             atomic.Int64
	moves, movesAcrossConfigs, recreatesSameSN     atomic.Int64
	cfgCreate, cfgReject, cfgDelete, cfgUpdate     atomic.Int64
	claimed0, claimed1, claimed2                   atomic.Int64
	maxDevs, maxMachines, maxLocDepth, maxSlugs    atomic.Int64
	staleCfgRebuilds, locScopedBuilds, wholeBuilds atomic.Int64
	overlapCfgRebuilds, nbFaults                   atomic.Int64
	opKinds                                        [32]atomic.Int64
}

func (s *propStats) String() string {
	var kinds []string
	for i := range s.opKinds {
		if n := s.opKinds[i].Load(); n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s=%d", propOpNames[i], n))
		}
	}
	return fmt.Sprintf(`cases=%d ops=%d
 hooks: now=%d dropped=%d dup=%d late=%d
 rebuilds: push=%d skip_no_change=%d model_error=%d injected_fault=%d (loc-scoped=%d whole-site=%d stale-config=%d overlapping-config=%d)
 reconcile: ticks=%d settles=%d nb_faults=%d
 machines: enrolled=%d deleted=%d rebound=%d frozen_mid_op=%d
 devices: moves=%d moves_across_configs=%d recreates_same_serial=%d
 configs: create=%d rejected_overlap=%d delete=%d update=%d
 claim counts per (device,step): 0=%d 1=%d 2+=%d
 sizes: max_devices=%d max_machines=%d max_loc_depth=%d max_configs=%d
 ops: %s`,
		s.cases.Load(), s.ops.Load(),
		s.hooksNow.Load(), s.hooksDropped.Load(), s.hooksDup.Load(), s.hooksLate.Load(),
		s.rebuildPush.Load(), s.rebuildSkip.Load(), s.rebuildErr.Load(), s.rebuildFault.Load(),
		s.locScopedBuilds.Load(), s.wholeBuilds.Load(), s.staleCfgRebuilds.Load(), s.overlapCfgRebuilds.Load(),
		s.ticks.Load(), s.settles.Load(), s.nbFaults.Load(),
		s.enrolled.Load(), s.deleted.Load(), s.rebound.Load(), s.frozen.Load(),
		s.moves.Load(), s.movesAcrossConfigs.Load(), s.recreatesSameSN.Load(),
		s.cfgCreate.Load(), s.cfgReject.Load(), s.cfgDelete.Load(), s.cfgUpdate.Load(),
		s.claimed0.Load(), s.claimed1.Load(), s.claimed2.Load(),
		s.maxDevs.Load(), s.maxMachines.Load(), s.maxLocDepth.Load(), s.maxSlugs.Load(),
		strings.Join(kinds, " "))
}

func propMax(a *atomic.Int64, v int) {
	for {
		cur := a.Load()
		if int64(v) <= cur || a.CompareAndSwap(cur, int64(v)) {
			return
		}
	}
}

const propHookSecret = "prop-secret"

type propPipe struct {
	rt *rapid.T
	k  propKnobs
	st *propStats

	w    *propWorld
	nb   *propNB
	db   *propStore
	inv  *propInv
	sink *propSink

	disp     *Dispatcher
	mir      *mirror.Mirror
	rec      *Reconciler
	resolver *ClaimResolver
	hook     *netbox.WebhookHandler

	deferred       [][]byte
	hookClean      bool
	maxSeq         map[string]int64
	explicit       map[uuid.UUID]bool // machines the current op mutated on purpose
	goneSerials    map[string]bool    // serials of devices deleted earlier in the case
	serialUUID     map[string]uuid.UUID
	inflightBefore map[uuid.UUID]int
	// hookDevs are the devices the current step sent a webhook about.
	hookDevs []int
	// multiDelivery is set when the current step can run the mirror's
	// delete path more than once for one machine (duplicate webhook, or
	// several reconcile ticks).
	multiDelivery bool
	// liveOnly is set when the current step drives only the reconciler,
	// which reads live NetBox: anything it enrolls must be claimed now.
	// (A webhook step may legitimately enroll from a stale payload.)
	liveOnly bool
	// pendingUpdate is a site_config row loaded by an in-flight PATCH, and
	// pendingLocs the site's locations as its NetBox validation read them.
	pendingUpdate *model.SiteConfig
	pendingLocs   []netbox.Location
}

func newPropPipe(rt *rapid.T, k propKnobs, st *propStats) *propPipe {
	p := &propPipe{
		rt: rt, k: k, st: st, w: newPropWorld(), db: newPropStore(), inv: &propInv{}, sink: newPropSink(),
		hookClean: true, maxSeq: map[string]int64{},
		explicit: map[uuid.UUID]bool{}, goneSerials: map[string]bool{}, serialUUID: map[string]uuid.UUID{},
	}
	seed := rapid.Int64Range(0, 1<<30).Draw(rt, "orderSeed")
	p.nb = &propNB{w: p.w, shuffle: k.Shuffle, seed: seed}
	p.db.shuffle, p.db.seed = k.Shuffle, seed
	p.disp = NewDispatcher(p.nb, p.db, p.sink)
	p.mir = mirror.New(p.db, p.inv)
	p.resolver = NewClaimResolver(p.nb, p.db)
	p.rec = NewReconciler(p.nb, p.db, p.inv, p.mir)
	p.hook = netbox.NewWebhookHandler(p.mir, p.resolver, propHookSecret)
	return p
}

var (
	propSerials = []string{"", "SA", "SB", "SC", "SD"}
	propNames   = []string{"n1.s1", "n2.s1", "n3.s2", "bare"}
	propMACs    = []string{"", "AA:00:00:00:00:01", "aa:00:00:00:00:02", "aa:00:00:00:00:03"}
	// The /31s are both ends of one link plus one end of a second, so
	// relay-pool generation sees several pools and both sides of a link.
	propAddrs = []string{"10.1.0.5/24", "10.1.0.6/24", "10.2.0.9/24", "10.3.0.1/31", "10.3.0.0/31", "10.3.0.5/31"}
	propCIDRs = []string{"10.1.0.0/24", "10.2.0.0/24", "10.3.0.0/24"}
	propSlugs = []string{"c1", "c2", "c3"}
	propSites = []string{"s1", "s2"}
)

// ---- world setup ----------------------------------------------------------

func (p *propPipe) setup() {
	rt := p.rt
	for _, slug := range propSites {
		p.w.sites[slug] = &pwSite{ID: p.w.id(), Slug: slug}
	}
	p.w.dtypes[1] = &pwDType{ID: 1, Slug: "dt-a", Model: "A", KP: "console=ttyS1,115200n8"}
	p.w.dtypes[2] = &pwDType{ID: 2, Slug: "dt-b", Model: "B", KP: ""}
	// A small location forest per site: each new location hangs off the
	// site root or any earlier location of the same site.
	nLoc := rapid.IntRange(0, 5).Draw(rt, "nLoc")
	for i := range nLoc {
		site := rapid.SampledFrom(propSites).Draw(rt, "locSite")
		parent := 0
		if cands := p.locsOf(site); len(cands) > 0 && rapid.Bool().Draw(rt, "nested") {
			parent = rapid.SampledFrom(cands).Draw(rt, "locParent")
		}
		id := p.w.id()
		p.w.locs[id] = &pwLoc{ID: id, Slug: fmt.Sprintf("loc%d", i), Site: site, Parent: parent}
	}
	// Prefixes: usually one site-scoped production prefix per site, and
	// one on some of the locations.
	for _, slug := range propSites {
		if rapid.IntRange(0, 9).Draw(rt, "sitePrefix") > 0 {
			p.addPrefix(p.w.sites[slug].ID, 0, propCIDRs[0])
		}
	}
	for _, id := range propSortedIDs(p.w.locs) {
		if rapid.IntRange(0, 9).Draw(rt, "locPrefix") > 4 {
			p.addPrefix(0, id, rapid.SampledFrom(propCIDRs).Draw(rt, "locCIDR"))
		}
	}
	if p.k.LocReparent {
		// Start from the topology a re-parent can break: two sibling
		// locations on one site, each with a production prefix and a
		// location-scoped config.
		for _, slug := range propSlugs[:2] {
			id := p.w.id()
			p.w.locs[id] = &pwLoc{ID: id, Slug: "cage-" + slug, Site: propSites[0]}
			p.addPrefix(0, id, propCIDRs[1])
			cfg := model.SiteConfig{Slug: slug, NetBoxSite: propSites[0], NetBoxLocation: "cage-" + slug, EphemeralPoolStart: "10.1.0.100", EphemeralPoolEnd: "10.1.0.200"}
			if !p.admitConfig(cfg) {
				rt.Fatalf("setup: sibling-location config %s refused", slug)
			}
			p.db.configs[slug] = &cfg
			p.inv.Invalidate(slug)
		}
	}
	for range rapid.IntRange(0, 2).Draw(rt, "nCfg") {
		p.opCfgCreate()
	}
	for range rapid.IntRange(0, 3).Draw(rt, "nDev") {
		p.opDevCreate()
	}
	p.drain()
}

func (p *propPipe) addPrefix(site, loc int, cidr string) {
	id := p.w.id()
	p.w.prefixes[id] = &pwPrefix{ID: id, CIDR: cidr, Site: site, Loc: loc, Tag: "slagworks-production", GW: strings.Replace(cidr, ".0/24", ".1", 1)}
}

func (p *propPipe) locsOf(site string) []int {
	var out []int
	for _, id := range propSortedIDs(p.w.locs) {
		if p.w.locs[id].Site == site {
			out = append(out, id)
		}
	}
	return out
}

func (p *propPipe) locHasPrefix(loc int) bool {
	for _, x := range p.w.prefixes {
		if x.Loc == loc {
			return true
		}
	}
	return false
}

func (p *propPipe) locDepth(id int) int {
	d := 0
	for id != 0 && d < 64 {
		d++
		id = p.w.locs[id].Parent
	}
	return d
}

// ---- webhook delivery -----------------------------------------------------

type propHookEnv struct {
	Event      string        `json:"event"`
	ObjectType string        `json:"object_type"`
	Data       netbox.Device `json:"data"`
	Snapshots  struct {
		PreChange  map[string]any `json:"prechange"`
		PostChange map[string]any `json:"postchange"`
	} `json:"snapshots"`
}

func (p *propPipe) devSnapshot(d *pwDev) map[string]any {
	m := map[string]any{"name": d.Name, "serial": d.Serial, "site": nil, "location": nil, "device_type": nil, "rack": nil, "role": nil}
	if s := p.w.sites[d.Site]; s != nil {
		m["site"] = s.ID
	}
	if d.Loc != 0 {
		m["location"] = d.Loc
	}
	if d.DType != 0 {
		m["device_type"] = d.DType
	}
	return m
}

func (p *propPipe) post(body []byte) {
	mac := hmac.New(sha512.New, []byte(propHookSecret))
	mac.Write(body)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/netbox", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	if err := p.hook.HandleWebhook(e.NewContext(req, rec)); err != nil {
		p.rt.Fatalf("webhook handler returned error for a well-formed signed delivery: %v", err)
	}
	if rec.Code != http.StatusOK {
		p.rt.Fatalf("webhook handler status %d for a well-formed signed delivery", rec.Code)
	}
}

// emit delivers a device event the way NetBox would, subject to the drawn
// delivery fault. It reports whether the event was applied in order.
func (p *propPipe) emit(event string, data netbox.Device, pre, post map[string]any) bool {
	p.hookDevs = append(p.hookDevs, data.ID)
	env := propHookEnv{Event: event, ObjectType: "dcim.device", Data: data}
	env.Snapshots.PreChange, env.Snapshots.PostChange = pre, post
	body, err := json.Marshal(env)
	if err != nil {
		p.rt.Fatal(err)
	}
	mode := 0
	if p.k.StaleHooks {
		mode = rapid.SampledFrom([]int{0, 0, 0, 0, 0, 0, 1, 2, 3}).Draw(p.rt, "delivery")
	}
	switch mode {
	case 1:
		p.st.hooksDropped.Add(1)
		p.hookClean = false
		return false
	case 2:
		p.st.hooksDup.Add(1)
		p.multiDelivery = true
		p.post(body)
		p.post(body)
	case 3:
		p.st.hooksLate.Add(1)
		p.deferred = append(p.deferred, body)
		p.hookClean = false
		return false
	default:
		p.st.hooksNow.Add(1)
		p.post(body)
	}
	return true
}

// ---- model predicates -----------------------------------------------------

// modelScopeError says why the reconciler / dispatcher cannot resolve the
// config's scope, "" if it can.
func (p *propPipe) modelScopeError(cfg model.SiteConfig) string {
	if p.w.sites[cfg.NetBoxSite] == nil {
		return "site missing"
	}
	if cfg.NetBoxLocation != "" && p.w.locBySlug(cfg.NetBoxSite, cfg.NetBoxLocation) == 0 {
		return "location missing"
	}
	return ""
}

// modelRefused says why the reconciler / dispatcher refuse to act on the
// config's claim, "" if they don't: its scope does not resolve, or it can
// claim a device that another config on the site whose scope resolves also
// claims. Admission keeps configs disjoint; a location re-parent can nest
// two of them afterwards.
func (p *propPipe) modelRefused(cfg model.SiteConfig) string {
	if e := p.modelScopeError(cfg); e != "" {
		return e
	}
	for _, e := range p.db.sortedConfigs() {
		if e.Slug == cfg.Slug || e.NetBoxSite != cfg.NetBoxSite || p.modelScopeError(e) != "" {
			continue
		}
		if cfg.NetBoxLocation == "" || e.NetBoxLocation == "" {
			return "overlaps " + e.Slug
		}
		cl, el := p.w.locBySlug(cfg.NetBoxSite, cfg.NetBoxLocation), p.w.locBySlug(e.NetBoxSite, e.NetBoxLocation)
		if p.w.inSubtree(cl, el) || p.w.inSubtree(el, cl) {
			return "overlaps " + e.Slug
		}
	}
	return ""
}

func (p *propPipe) modelBuildError(slug string) string {
	cfg, ok := p.db.configs[slug]
	if !ok {
		return "config missing"
	}
	if e := p.modelRefused(*cfg); e != "" {
		return e
	}
	if len(p.w.modelProdPrefixes(*cfg)) == 0 {
		return "no production prefix"
	}
	// A machine interface with more than one active address does not stop
	// the build: the snapshot carries the one modelIP picks.
	return ""
}

// modelIP is the one address a machine's facts carry for an interface of
// device d, "" when the interface has no active address: the device's
// primary_ip4 (nbDevice gives a Primary device the address ID 2) when that
// is one of the interface's active addresses, otherwise the numerically
// lowest of them, ties by prefix length and then NetBox ID. Every address
// this world draws is IPv4.
func (p *propPipe) modelIP(d *pwDev, iface int) string {
	var best *pwIP
	var bestPfx netip.Prefix
	for _, id := range propSortedIDs(p.w.ips) {
		ip := p.w.ips[id]
		if ip.Iface != iface || ip.Reserved {
			continue
		}
		if d.Primary && ip.ID == 2 {
			return ip.Addr
		}
		pfx := netip.MustParsePrefix(ip.Addr)
		// IDs ascend, so an equal address and length keeps the earlier one.
		if best == nil || pfx.Addr().Less(bestPfx.Addr()) || (pfx.Addr() == bestPfx.Addr() && pfx.Bits() < bestPfx.Bits()) {
			best, bestPfx = ip, pfx
		}
	}
	if best == nil {
		return ""
	}
	return best.Addr
}

func (p *propPipe) modelClaimers(d *pwDev) []string {
	var out []string
	for _, cfg := range p.db.sortedConfigs() {
		if p.w.modelClaims(cfg, d) {
			out = append(out, cfg.Slug)
		}
	}
	return out
}

// modelLiveClaimers is modelClaimers without the configs the reconciler
// refuses (modelRefused): the claimers whose devices a reconcile tick
// mirrors.
func (p *propPipe) modelLiveClaimers(d *pwDev) []string {
	var out []string
	for _, slug := range p.modelClaimers(d) {
		if p.modelRefused(*p.db.configs[slug]) == "" {
			out = append(out, slug)
		}
	}
	return out
}

func propStrip(cidr string) string {
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}

// modelMachines recomputes, from the world alone, what a config's snapshot
// must say about each machine it claims.
func (p *propPipe) modelMachines(cfg model.SiteConfig) map[string]string {
	want := map[string]string{}
	for _, m := range p.db.machines {
		if m.NetBoxDeviceID == nil {
			continue
		}
		d := p.w.devs[*m.NetBoxDeviceID]
		if d == nil || !p.w.modelClaims(cfg, d) {
			continue
		}
		var ifs []string
		for _, id := range propSortedIDs(p.w.ifaces) {
			i := p.w.ifaces[id]
			if i.Dev != d.ID {
				continue
			}
			typ := "physical"
			if i.Mgmt && d.OOB != "" {
				typ = "ipmi"
			}
			ip := p.modelIP(d, i.ID)
			ifs = append(ifs, fmt.Sprintf("%d/%s/%s/%s/%s", i.ID, i.Name, strings.ToLower(i.MAC), typ, ip))
		}
		kp := ""
		if dt := p.w.dtypes[d.DType]; dt != nil {
			kp = dt.KP
		}
		want[m.ID.String()] = fmt.Sprintf("dev=%d serial=%s host=%s bmc=%s state=%s locked=%v pxe=%v kp=%q ifaces=%v",
			d.ID, m.NetBoxSerial, d.Name, propStrip(d.OOB), m.State, m.Locked, m.PXEEligible, kp, ifs)
	}
	return want
}

func propProjectSnap(snap model.SiteSnapshot) map[string]string {
	got := map[string]string{}
	for _, ms := range snap.Machines {
		ifs := make([]string, 0, len(ms.Interfaces))
		sorted := append([]model.SnapshotInterface(nil), ms.Interfaces...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].NetBoxID < sorted[j].NetBoxID })
		for _, i := range sorted {
			ifs = append(ifs, fmt.Sprintf("%d/%s/%s/%s/%s", i.NetBoxID, i.Name, i.MAC, i.Type, i.IP))
		}
		dev := -1
		if ms.NetBoxDeviceID != nil {
			dev = *ms.NetBoxDeviceID
		}
		key := ms.ID.String()
		if _, dup := got[key]; dup {
			key += "(duplicate)"
		}
		got[key] = fmt.Sprintf("dev=%d serial=%s host=%s bmc=%s state=%s locked=%v pxe=%v kp=%q ifaces=%v",
			dev, ms.NetBoxSerial, ms.Hostname, ms.BMCIP, ms.State, ms.Locked, ms.PXEEligible, ms.KernelParams, ifs)
	}
	return got
}

func propDiffMaps(want, got map[string]string) string {
	var out []string
	for k, w := range want {
		if g, ok := got[k]; !ok {
			out = append(out, fmt.Sprintf("missing %s: want %s", k, w))
		} else if g != w {
			out = append(out, fmt.Sprintf("differs %s:\n   want %s\n   got  %s", k, w, g))
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			out = append(out, fmt.Sprintf("unexpected %s: got %s", k, g))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n  ")
}

// ---- rebuild + checks -----------------------------------------------------

func (p *propPipe) checkModel(slug string, snap model.SiteSnapshot) {
	cfg := *p.db.configs[slug]
	want := p.modelMachines(cfg)
	got := propProjectSnap(snap)
	if d := propDiffMaps(want, got); d != "" {
		p.rt.Fatalf("snapshot for %s disagrees with recomputation from the world:\n  %s", slug, d)
	}
	if snap.Site.Slug != cfg.NetBoxSite {
		p.rt.Fatalf("snapshot for %s has site slug %q, want %q", slug, snap.Site.Slug, cfg.NetBoxSite)
	}
	if pf := p.w.modelPrimaryPrefix(cfg); snap.DHCP.Subnet != pf.CIDR || snap.DHCP.Gateway != pf.GW {
		p.rt.Fatalf("snapshot for %s serves DHCP subnet %s gw %s, want the scope's primary production prefix %s gw %s",
			slug, snap.DHCP.Subnet, snap.DHCP.Gateway, pf.CIDR, pf.GW)
	}
	if snap.DHCP.EphemeralPoolStart != cfg.EphemeralPoolStart || len(snap.Images) != len(p.db.images) || len(snap.SSHKeys) != len(p.db.keys) {
		p.rt.Fatalf("snapshot for %s lost config/image/key content", slug)
	}
}

// build runs the read-only build with fault injection suspended.
func (p *propPipe) build(slug string) (model.SiteSnapshot, error) {
	saveIn, saveRun := p.nb.failIn, p.nb.failRun
	p.nb.failIn, p.nb.failRun = 0, 0
	defer func() { p.nb.failIn, p.nb.failRun = saveIn, saveRun }()
	return p.disp.BuildSnapshot(context.Background(), slug)
}

func (p *propPipe) rebuildAndCheck(slug string) {
	wantErr := p.modelBuildError(slug)
	prevPushes := p.sink.pushes[slug]
	prevLast, hadLast := p.sink.last[slug]
	prevHash := ""
	if c := p.db.configs[slug]; c != nil {
		prevHash = c.LastSnapshotHash
		if c.NetBoxLocation != "" {
			p.st.locScopedBuilds.Add(1)
		} else {
			p.st.wholeBuilds.Add(1)
		}
	}
	snap, err := p.disp.rebuild(context.Background(), slug, 0)
	if errors.Is(err, errPropNB) {
		p.st.rebuildFault.Add(1)
		p.inv.Invalidate(slug) // the registry's retry timer
		return
	}
	if (err != nil) != (wantErr != "") {
		p.rt.Fatalf("rebuild(%s): err=%v, model says %q", slug, err, wantErr)
	}
	if err != nil {
		p.st.rebuildErr.Add(1)
		if wantErr == "location missing" {
			p.st.staleCfgRebuilds.Add(1)
		}
		if strings.HasPrefix(wantErr, "overlaps ") {
			p.st.overlapCfgRebuilds.Add(1)
		}
		if p.sink.pushes[slug] != prevPushes {
			p.rt.Fatalf("rebuild(%s) failed but still pushed", slug)
		}
		return
	}
	pushed := p.sink.pushes[slug] != prevPushes
	last := p.sink.last[slug]
	if pushed {
		p.st.rebuildPush.Add(1)
		if last.SeqNum != snap.SeqNum {
			p.rt.Fatalf("rebuild(%s) returned seq %d but pushed seq %d", slug, snap.SeqNum, last.SeqNum)
		}
		if last.SeqNum <= p.maxSeq[slug] {
			p.rt.Fatalf("pushed SeqNum for %s went from %d to %d: a worker that applied the earlier snapshot drops this one as stale",
				slug, p.maxSeq[slug], last.SeqNum)
		}
		p.maxSeq[slug] = last.SeqNum
		if hadLast && prevHash != "" && propCanon(prevLast) == propCanon(last) {
			p.rt.Fatalf("rebuild(%s) pushed seq %d with content identical to the previous push", slug, last.SeqNum)
		}
	} else {
		p.st.rebuildSkip.Add(1)
		if !hadLast {
			p.rt.Fatalf("rebuild(%s) reported no_change but nothing was ever pushed for the slug", slug)
		}
	}
	fresh, ferr := p.build(slug)
	if ferr != nil {
		p.rt.Fatalf("BuildSnapshot(%s) failed right after a successful rebuild: %v", slug, ferr)
	}
	if propCanon(fresh) != propCanon(snap) {
		p.rt.Fatalf("two builds of %s over an unchanged world differ (order-dependent build):\n a=%s\n b=%s", slug, propCanon(snap), propCanon(fresh))
	}
	if propCanon(fresh) != propCanon(last) {
		p.rt.Fatalf("missed dispatch for %s: rebuild pushed=%v, but the last pushed snapshot differs from a fresh build:\n pushed=%s\n fresh =%s",
			slug, pushed, propCanon(last), propCanon(fresh))
	}
	p.checkModel(slug, fresh)
}

func (p *propPipe) drain() {
	for range 6 {
		if len(p.inv.dirty) == 0 {
			return
		}
		slugs := make([]string, 0, len(p.inv.dirty))
		for s := range p.inv.dirty {
			slugs = append(slugs, s)
		}
		sort.Strings(slugs)
		p.inv.dirty = map[string]bool{}
		for _, s := range slugs {
			p.rebuildAndCheck(s)
		}
	}
	// Still dirty only because injected faults keep failing rebuilds.
}

// checkAllFresh requires every buildable slug's last pushed snapshot to
// equal a from-scratch build.
func (p *propPipe) checkAllFresh(when string) {
	for _, cfg := range p.db.sortedConfigs() {
		if p.modelBuildError(cfg.Slug) != "" {
			continue
		}
		fresh, err := p.build(cfg.Slug)
		if err != nil {
			p.rt.Fatalf("%s: BuildSnapshot(%s): %v", when, cfg.Slug, err)
		}
		last, ok := p.sink.last[cfg.Slug]
		if !ok || propCanon(last) != propCanon(fresh) {
			p.rt.Fatalf("%s: worker for %s is serving a stale snapshot (no invalidation reached it):\n pushed=%s\n fresh =%s",
				when, cfg.Slug, propCanon(last), propCanon(fresh))
		}
		p.checkModel(cfg.Slug, fresh)
	}
}

// enrollGap says why a serialled device some config in claimers claims has
// no machine row ("" if it has one or needs none).
func (p *propPipe) enrollGap(d *pwDev, claimers []string) string {
	if d.Serial == "" || len(claimers) == 0 {
		return ""
	}
	why := "no row holds its serial"
	for _, m := range p.db.machines {
		if m.NetBoxDeviceID != nil && *m.NetBoxDeviceID == d.ID {
			return ""
		}
		if m.NetBoxSerial == d.Serial {
			why = fmt.Sprintf("row %s holds serial %s bound to device %v (exists in NetBox: %v)", m.ID, m.NetBoxSerial, *m.NetBoxDeviceID, p.w.devs[*m.NetBoxDeviceID] != nil)
		}
	}
	return fmt.Sprintf("device %d (serial %s, claimed by %v) has no machine row: %s", d.ID, d.Serial, p.modelClaimers(d), why)
}

func (p *propPipe) serialHeldByGoneDevice(serial string) bool {
	for _, m := range p.db.machines {
		if m.NetBoxSerial == serial && m.NetBoxDeviceID != nil && p.w.devs[*m.NetBoxDeviceID] == nil {
			return true
		}
	}
	return false
}

// storeViolations lists what a settled reconciler must not leave behind:
// serialled devices a config it reconciles claims with no machine row, and
// rows for vanished devices. The missing-device sweep runs when at least
// one config reconciles; a config it refuses (scope no longer resolves, or
// overlaps another) does not hold it back.
func (p *propPipe) storeViolations() []string {
	var out []string
	anyProcessed := false
	for _, cfg := range p.db.sortedConfigs() {
		if p.modelRefused(cfg) == "" {
			anyProcessed = true
		}
	}
	for _, id := range propSortedIDs(p.w.devs) {
		d := p.w.devs[id]
		if gap := p.enrollGap(d, p.modelLiveClaimers(d)); gap != "" {
			out = append(out, gap)
		}
	}
	if anyProcessed {
		for _, m := range p.db.machines {
			if m.NetBoxDeviceID != nil && p.w.devs[*m.NetBoxDeviceID] == nil {
				out = append(out, fmt.Sprintf("machine %s (serial %s) is still bound to device %d, which no longer exists", m.ID, m.NetBoxSerial, *m.NetBoxDeviceID))
			}
		}
	}
	sort.Strings(out)
	return out
}

func (p *propPipe) machinesCopy() map[uuid.UUID]model.Machine {
	out := make(map[uuid.UUID]model.Machine, len(p.db.machines))
	for id, m := range p.db.machines {
		out[id] = *p.db.clone(m)
	}
	return out
}

// checkIdentity enforces the row-level safety rules across one step.
func (p *propPipe) checkIdentity(before map[uuid.UUID]model.Machine, devsBefore map[int]bool) {
	for id, b := range before {
		a, ok := p.db.machines[id]
		if !ok {
			p.st.deleted.Add(1)
			if p.inflightBefore[id] > 0 && !p.multiDelivery {
				p.rt.Fatalf("machine %s (serial %s) was deleted in the same pass that found %d in-flight op(s) on it; B7 says fail the ops and keep the row for inspection",
					id, b.NetBoxSerial, p.inflightBefore[id])
			}
			if b.NetBoxDeviceID != nil && p.w.devs[*b.NetBoxDeviceID] != nil {
				p.rt.Fatalf("machine %s (serial %s, state %s) was deleted while its NetBox device %d still exists",
					id, b.NetBoxSerial, b.State, *b.NetBoxDeviceID)
			}
			continue
		}
		if a.NetBoxSerial != b.NetBoxSerial {
			p.rt.Fatalf("machine %s changed enrolled serial %q -> %q", id, b.NetBoxSerial, a.NetBoxSerial)
		}
		if b.NetBoxDeviceID != nil && (a.NetBoxDeviceID == nil || *a.NetBoxDeviceID != *b.NetBoxDeviceID) {
			p.st.rebound.Add(1)
			if p.w.devs[*b.NetBoxDeviceID] != nil {
				p.rt.Fatalf("machine %s was re-pointed away from device %d, which still exists", id, *b.NetBoxDeviceID)
			}
		}
		if !p.explicit[id] && (a.Locked != b.Locked || a.PXEEligible != b.PXEEligible) {
			p.rt.Fatalf("machine %s lost operator-owned columns: locked %v->%v pxe %v->%v", id, b.Locked, a.Locked, b.PXEEligible, a.PXEEligible)
		}
		if !p.explicit[id] && a.State != b.State && a.State != model.MachineStateFailed {
			p.rt.Fatalf("machine %s state changed %s -> %s without an operator action", id, b.State, a.State)
		}
		if a.State != b.State && a.State == model.MachineStateFailed {
			p.st.frozen.Add(1)
		}
	}
	for id, a := range p.db.machines {
		if _, ok := before[id]; !ok {
			p.st.enrolled.Add(1)
			if a.NetBoxSerial == "" {
				p.rt.Fatalf("machine %s enrolled with an empty serial", id)
			}
			if p.liveOnly {
				d := p.w.devs[*a.NetBoxDeviceID]
				if d == nil || len(p.modelLiveClaimers(d)) == 0 || d.Serial != a.NetBoxSerial {
					p.rt.Fatalf("reconcile enrolled machine %s (serial %s) for device %d, which no site_config it reconciles claims (or which does not carry that serial)",
						id, a.NetBoxSerial, *a.NetBoxDeviceID)
				}
			}
			if prev, seen := p.serialUUID[a.NetBoxSerial]; seen && prev != id && p.k.KeepIdentity {
				p.rt.Fatalf("serial %s was machine %s and is now machine %s: identity (state, lock, commission data) was not carried across the device re-create",
					a.NetBoxSerial, prev, id)
			}
			p.serialUUID[a.NetBoxSerial] = id
		}
	}
	p.explicit = map[uuid.UUID]bool{}
	_ = devsBefore
}

// checkClaims compares both production claim resolvers with the brute-force
// walk for every device, and enforces the partition invariant.
func (p *propPipe) checkClaims() {
	saveIn := p.nb.failIn
	p.nb.failIn = 0
	defer func() { p.nb.failIn = saveIn }()
	ctx := context.Background()
	for _, id := range propSortedIDs(p.w.devs) {
		d := p.w.devs[id]
		want := p.modelClaimers(d)
		switch len(want) {
		case 0:
			p.st.claimed0.Add(1)
		case 1:
			p.st.claimed1.Add(1)
		default:
			p.st.claimed2.Add(1)
			if !p.k.LocReparent {
				p.rt.Fatalf("device %d is claimed by %v: the partition invariant CheckOverlap is meant to enforce is broken", d.ID, want)
			}
		}
		got, err := p.resolver.ClaimingConfigs(ctx, p.w.nbDevice(d))
		if err != nil {
			p.rt.Fatalf("ClaimingConfigs(%d): %v", d.ID, err)
		}
		sort.Strings(got)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			p.rt.Fatalf("ClaimingConfigs(device %d in site %s loc %d) = %v, brute force says %v", d.ID, d.Site, d.Loc, got, want)
		}
		owner, err := ResolveOwningConfigSlug(ctx, p.nb, p.db, p.w.nbDevice(d))
		switch {
		case len(want) == 0:
			if !errors.Is(err, ErrNoOwningConfig) {
				p.rt.Fatalf("ResolveOwningConfigSlug(device %d) = %q, %v; brute force says unclaimed", d.ID, owner, err)
			}
		case len(want) == 1:
			if err != nil || owner != want[0] {
				p.rt.Fatalf("ResolveOwningConfigSlug(device %d) = %q, %v; brute force says %v", d.ID, owner, err, want)
			}
		default:
			if !errors.Is(err, ErrAmbiguousOwningConfig) {
				p.rt.Fatalf("ResolveOwningConfigSlug(device %d) = %q, %v; brute force says %v all claim it, so none may be picked as the owner", d.ID, owner, err, want)
			}
		}
	}
}

// ---- operations -----------------------------------------------------------

const (
	opDevCreate = iota
	opDevDelete
	opDevMove
	opDevRename
	opDevSerial
	opDevAttr
	opIface
	opIP
	opPrefix
	opLoc
	opCfgCreate
	opCfgDelete
	opCfgUpdate
	opMachine
	opStartOp
	opTick
	opLateHook
	opFault
	opSettle
	opTouch
	opStaleCfg
	propNumOps
)

var propOpNames = [32]string{"devCreate", "devDelete", "devMove", "devRename", "devSerial", "devAttr", "iface", "ip",
	"prefix", "loc", "cfgCreate", "cfgDelete", "cfgUpdate", "machine", "startOp", "tick", "lateHook", "fault", "settle", "touch", "staleCfg"}

func (p *propPipe) pickDev(label string) *pwDev {
	ids := propSortedIDs(p.w.devs)
	if len(ids) == 0 {
		return nil
	}
	return p.w.devs[rapid.SampledFrom(ids).Draw(p.rt, label)]
}

func (p *propPipe) pickMachine(label string) *model.Machine {
	ms := make([]string, 0, len(p.db.machines))
	for id := range p.db.machines {
		ms = append(ms, id.String())
	}
	if len(ms) == 0 {
		return nil
	}
	sort.Strings(ms)
	return p.db.machines[uuid.MustParse(rapid.SampledFrom(ms).Draw(p.rt, label))]
}

// serialFree reports whether a new or edited device may take this serial
// under the current knobs: never while another live device has it, and
// (unless SerialReuse) never while a machine row bound to a live device
// holds it.
func (p *propPipe) serialFree(serial string, self int) bool {
	if serial == "" {
		return true
	}
	for _, d := range p.w.devs {
		if d.ID != self && d.Serial == serial {
			return false
		}
	}
	if !p.k.SerialReuse {
		for _, m := range p.db.machines {
			if m.NetBoxSerial == serial && m.NetBoxDeviceID != nil && *m.NetBoxDeviceID != self && p.w.devs[*m.NetBoxDeviceID] != nil {
				return false
			}
		}
	}
	return true
}

func (p *propPipe) drawPlace(label string) (site string, loc int) {
	site = rapid.SampledFrom(propSites).Draw(p.rt, label+"Site")
	if cands := p.locsOf(site); len(cands) > 0 && rapid.IntRange(0, 4).Draw(p.rt, label+"HasLoc") > 0 {
		loc = rapid.SampledFrom(cands).Draw(p.rt, label+"Loc")
	}
	return site, loc
}

func (p *propPipe) opDevCreate() {
	if len(p.w.devs) >= 5 {
		return
	}
	serial := rapid.SampledFrom(propSerials).Draw(p.rt, "serial")
	if !p.serialFree(serial, 0) {
		serial = ""
	}
	site, loc := p.drawPlace("dev")
	d := &pwDev{ID: p.w.id(), Name: rapid.SampledFrom(propNames).Draw(p.rt, "name"), Serial: serial, Site: site, Loc: loc,
		DType: rapid.IntRange(0, 2).Draw(p.rt, "dtype"), Primary: rapid.Bool().Draw(p.rt, "primary")}
	if rapid.Bool().Draw(p.rt, "hasOOB") {
		d.OOB = "10.9.0.7/24"
	}
	p.w.devs[d.ID] = d
	if serial != "" && p.goneSerials[serial] {
		p.st.recreatesSameSN.Add(1)
	}
	// Interfaces created with the device (NetBox templates do this); no
	// separate webhook reaches slagworks for them.
	for i := range rapid.IntRange(0, 2).Draw(p.rt, "nIf") {
		id := p.w.id()
		p.w.ifaces[id] = &pwIface{ID: id, Dev: d.ID, Name: fmt.Sprintf("eth%d", i), MAC: rapid.SampledFrom(propMACs).Draw(p.rt, "mac"), Mgmt: rapid.IntRange(0, 3).Draw(p.rt, "mgmt") == 0}
	}
	p.emit("created", p.w.nbDevice(d), nil, p.devSnapshot(d))
}

func (p *propPipe) opDevDelete() {
	d := p.pickDev("delDev")
	if d == nil {
		return
	}
	data, pre := p.w.nbDevice(d), p.devSnapshot(d)
	delete(p.w.devs, d.ID)
	for _, id := range propSortedIDs(p.w.ifaces) {
		if p.w.ifaces[id].Dev == d.ID {
			for _, ipID := range propSortedIDs(p.w.ips) {
				if p.w.ips[ipID].Iface == id {
					delete(p.w.ips, ipID)
				}
			}
			delete(p.w.ifaces, id)
		}
	}
	if d.Serial != "" {
		p.goneSerials[d.Serial] = true
	}
	p.emit("deleted", data, pre, nil)
}

func (p *propPipe) opDevMove() {
	d := p.pickDev("moveDev")
	if d == nil {
		return
	}
	pre := p.devSnapshot(d)
	from := p.modelClaimers(d)
	d.Site, d.Loc = p.drawPlace("move")
	p.st.moves.Add(1)
	if to := p.modelClaimers(d); fmt.Sprint(to) != fmt.Sprint(from) {
		p.st.movesAcrossConfigs.Add(1)
	}
	p.emit("updated", p.w.nbDevice(d), pre, p.devSnapshot(d))
}

func (p *propPipe) opDevRename() {
	d := p.pickDev("renDev")
	if d == nil {
		return
	}
	pre := p.devSnapshot(d)
	d.Name = rapid.SampledFrom(propNames).Draw(p.rt, "newName")
	p.emit("updated", p.w.nbDevice(d), pre, p.devSnapshot(d))
}

func (p *propPipe) opDevSerial() {
	d := p.pickDev("serDev")
	if d == nil {
		return
	}
	serial := rapid.SampledFrom(propSerials).Draw(p.rt, "newSerial")
	// Without SerialReuse a serial, once set, stays: slagworks treats the
	// enrolled serial as the stable binding.
	if !p.serialFree(serial, d.ID) || (d.Serial != "" && !p.k.SerialReuse) {
		return
	}
	pre := p.devSnapshot(d)
	d.Serial = serial
	p.emit("updated", p.w.nbDevice(d), pre, p.devSnapshot(d))
}

func (p *propPipe) opDevAttr() {
	d := p.pickDev("attrDev")
	if d == nil {
		return
	}
	pre := p.devSnapshot(d)
	switch rapid.IntRange(0, 2).Draw(p.rt, "attr") {
	case 0:
		d.DType = rapid.IntRange(0, 2).Draw(p.rt, "newDType")
	case 1:
		if d.OOB == "" {
			d.OOB = "10.9.0.8/24"
		} else {
			d.OOB = ""
		}
	default:
		d.Primary = !d.Primary
	}
	p.emit("updated", p.w.nbDevice(d), pre, p.devSnapshot(d))
}

func (p *propPipe) opIface() {
	d := p.pickDev("ifDev")
	if d == nil {
		return
	}
	p.hookClean = false
	var mine []int
	for _, id := range propSortedIDs(p.w.ifaces) {
		if p.w.ifaces[id].Dev == d.ID {
			mine = append(mine, id)
		}
	}
	switch act := rapid.IntRange(0, 2).Draw(p.rt, "ifAct"); {
	case act == 0 && len(mine) < 3:
		id := p.w.id()
		p.w.ifaces[id] = &pwIface{ID: id, Dev: d.ID, Name: fmt.Sprintf("eth%d", len(mine)+5), MAC: rapid.SampledFrom(propMACs).Draw(p.rt, "mac")}
	case act == 1 && len(mine) > 0:
		id := rapid.SampledFrom(mine).Draw(p.rt, "ifDel")
		for _, ipID := range propSortedIDs(p.w.ips) {
			if p.w.ips[ipID].Iface == id {
				delete(p.w.ips, ipID)
			}
		}
		delete(p.w.ifaces, id)
	case len(mine) > 0:
		i := p.w.ifaces[rapid.SampledFrom(mine).Draw(p.rt, "ifEdit")]
		if rapid.Bool().Draw(p.rt, "editMAC") {
			i.MAC = rapid.SampledFrom(propMACs).Draw(p.rt, "mac")
		} else {
			i.Mgmt = !i.Mgmt
		}
	}
}

func (p *propPipe) opIP() {
	ids := propSortedIDs(p.w.ifaces)
	if len(ids) == 0 {
		return
	}
	p.hookClean = false
	ifID := rapid.SampledFrom(ids).Draw(p.rt, "ipIface")
	var mine []int
	active := 0
	for _, id := range propSortedIDs(p.w.ips) {
		if ip := p.w.ips[id]; ip.Iface == ifID {
			mine = append(mine, id)
			if !ip.Reserved {
				active++
			}
		}
	}
	const limit = 2 // up to two active addresses: the snapshot must pick one
	switch act := rapid.IntRange(0, 2).Draw(p.rt, "ipAct"); {
	case act == 0 && len(mine) < 2:
		reserved := active >= limit || rapid.IntRange(0, 3).Draw(p.rt, "reserved") == 0
		id := p.w.id()
		p.w.ips[id] = &pwIP{ID: id, Iface: ifID, Addr: rapid.SampledFrom(propAddrs).Draw(p.rt, "addr"), Reserved: reserved}
	case act == 1 && len(mine) > 0:
		delete(p.w.ips, rapid.SampledFrom(mine).Draw(p.rt, "ipDel"))
	case len(mine) > 0:
		ip := p.w.ips[rapid.SampledFrom(mine).Draw(p.rt, "ipEdit")]
		if ip.Reserved && active >= limit {
			return
		}
		ip.Reserved = !ip.Reserved
	}
}

func (p *propPipe) opPrefix() {
	p.hookClean = false
	ids := propSortedIDs(p.w.prefixes)
	switch act := rapid.IntRange(0, 2).Draw(p.rt, "pfAct"); {
	case act == 0:
		site := rapid.SampledFrom(propSites).Draw(p.rt, "pfSite")
		cidr := rapid.SampledFrom(propCIDRs).Draw(p.rt, "pfCIDR")
		if cands := p.locsOf(site); len(cands) > 0 && rapid.Bool().Draw(p.rt, "pfAtLoc") {
			loc := rapid.SampledFrom(cands).Draw(p.rt, "pfLoc")
			if p.locHasPrefix(loc) {
				// One production prefix per location: among several at
				// one location the primary is whichever NetBox lists
				// first, an order this fake does not define.
				return
			}
			p.addPrefix(0, loc, cidr)
			return
		}
		for _, x := range p.w.prefixes {
			if x.Loc == 0 && x.Site == p.w.sites[site].ID {
				return // one site-scoped production prefix per site
			}
		}
		p.addPrefix(p.w.sites[site].ID, 0, cidr)
	case act == 1 && len(ids) > 0:
		delete(p.w.prefixes, rapid.SampledFrom(ids).Draw(p.rt, "pfDel"))
	case len(ids) > 0:
		x := p.w.prefixes[rapid.SampledFrom(ids).Draw(p.rt, "pfEdit")]
		x.GW = rapid.SampledFrom([]string{"", "10.1.0.1", "10.1.0.254"}).Draw(p.rt, "pfGW")
		x.Lease = rapid.IntRange(0, 1).Draw(p.rt, "pfLease") * 300
	}
}

func (p *propPipe) opLoc() {
	p.hookClean = false
	ids := propSortedIDs(p.w.locs)
	switch act := rapid.IntRange(0, 2).Draw(p.rt, "locAct"); {
	case act == 0 && len(ids) < 6:
		site := rapid.SampledFrom(propSites).Draw(p.rt, "newLocSite")
		parent := 0
		if cands := p.locsOf(site); len(cands) > 0 && rapid.Bool().Draw(p.rt, "newLocNested") {
			parent = rapid.SampledFrom(cands).Draw(p.rt, "newLocParent")
		}
		id := p.w.id()
		p.w.locs[id] = &pwLoc{ID: id, Slug: fmt.Sprintf("loc%d", id), Site: site, Parent: parent}
	case act == 1 && len(ids) > 0:
		// NetBox only lets an empty leaf location go.
		id := rapid.SampledFrom(ids).Draw(p.rt, "locDel")
		for _, l := range p.w.locs {
			if l.Parent == id {
				return
			}
		}
		for _, d := range p.w.devs {
			if d.Loc == id {
				return
			}
		}
		for _, pid := range propSortedIDs(p.w.prefixes) {
			if p.w.prefixes[pid].Loc == id {
				delete(p.w.prefixes, pid) // NetBox nulls the scope; the prefix leaves every location query
			}
		}
		delete(p.w.locs, id)
	case len(ids) > 0 && p.k.LocReparent:
		id := rapid.SampledFrom(ids).Draw(p.rt, "locMove")
		l := p.w.locs[id]
		cands := []int{0}
		for _, c := range p.locsOf(l.Site) {
			if !p.w.inSubtree(c, id) { // no cycles
				cands = append(cands, c)
			}
		}
		l.Parent = rapid.SampledFrom(cands).Draw(p.rt, "locNewParent")
	}
}

// opStaleCfg deletes the NetBox location a location-scoped config names,
// leaving the config stale (NetBox allows it once the location is an empty
// leaf; devices in it are first moved to the site root, each with its
// webhook).
func (p *propPipe) opStaleCfg() {
	var cands []model.SiteConfig
	for _, c := range p.db.sortedConfigs() {
		if id := p.w.locBySlug(c.NetBoxSite, c.NetBoxLocation); c.NetBoxLocation != "" && id != 0 {
			leaf := true
			for _, l := range p.w.locs {
				leaf = leaf && l.Parent != id
			}
			if leaf {
				cands = append(cands, c)
			}
		}
	}
	if len(cands) == 0 {
		return
	}
	c := rapid.SampledFrom(cands).Draw(p.rt, "staleCfg")
	id := p.w.locBySlug(c.NetBoxSite, c.NetBoxLocation)
	for _, did := range propSortedIDs(p.w.devs) {
		if d := p.w.devs[did]; d.Loc == id {
			pre := p.devSnapshot(d)
			d.Loc = 0
			p.emit("updated", p.w.nbDevice(d), pre, p.devSnapshot(d))
		}
	}
	for _, pid := range propSortedIDs(p.w.prefixes) {
		if p.w.prefixes[pid].Loc == id {
			delete(p.w.prefixes, pid)
		}
	}
	delete(p.w.locs, id)
	p.hookClean = false
}

// admitConfig mirrors the API's create/update gate (validateSiteConfigNetBox
// minus the prefix precondition): the site and location must exist and
// CheckOverlap must pass.
func (p *propPipe) admitConfig(cfg model.SiteConfig) bool {
	if p.modelScopeError(cfg) != "" {
		return false
	}
	err := CheckOverlap(cfg, p.db.sortedConfigs(), p.locations(cfg.NetBoxSite))
	if want := p.modelOverlap(cfg); (err != nil) != (want != "") {
		p.rt.Fatalf("CheckOverlap(%s site=%s loc=%q) = %v, brute force says %q", cfg.Slug, cfg.NetBoxSite, cfg.NetBoxLocation, err, want)
	}
	if err != nil {
		p.st.cfgReject.Add(1)
		return false
	}
	return true
}

// locations is the site's NetBox locations as the API's validation reads
// them, with fault injection off.
func (p *propPipe) locations(site string) []netbox.Location {
	saveIn := p.nb.failIn
	p.nb.failIn = 0
	defer func() { p.nb.failIn = saveIn }()
	locs, _ := p.nb.ListLocationsForSite(context.Background(), site)
	return locs
}

// modelOverlap says why a candidate config could claim a device some other
// config on the same NetBox site also claims ("" if it cannot): either side
// whole-site, or one location inside the other's subtree. An existing config
// whose location is gone makes the answer unknowable, which the gate treats
// as a refusal.
func (p *propPipe) modelOverlap(c model.SiteConfig) string {
	for _, e := range p.db.sortedConfigs() {
		if e.Slug == c.Slug || e.NetBoxSite != c.NetBoxSite {
			continue
		}
		if c.NetBoxLocation == "" || e.NetBoxLocation == "" {
			return "whole-site vs " + e.Slug
		}
		cl, el := p.w.locBySlug(c.NetBoxSite, c.NetBoxLocation), p.w.locBySlug(e.NetBoxSite, e.NetBoxLocation)
		if el == 0 {
			return "stale " + e.Slug
		}
		if p.w.inSubtree(cl, el) || p.w.inSubtree(el, cl) {
			return "nested with " + e.Slug
		}
	}
	return ""
}

func (p *propPipe) drawScope(label string) (site, loc string) {
	site = rapid.SampledFrom(propSites).Draw(p.rt, label+"Site")
	if cands := p.locsOf(site); len(cands) > 0 && rapid.IntRange(0, 2).Draw(p.rt, label+"Scoped") > 0 {
		loc = p.w.locs[rapid.SampledFrom(cands).Draw(p.rt, label+"Loc")].Slug
	}
	return site, loc
}

func (p *propPipe) opCfgCreate() {
	slug := rapid.SampledFrom(propSlugs).Draw(p.rt, "cfgSlug")
	if _, exists := p.db.configs[slug]; exists {
		return
	}
	site, loc := p.drawScope("cfg")
	cfg := model.SiteConfig{Slug: slug, NetBoxSite: site, NetBoxLocation: loc, EphemeralPoolStart: "10.1.0.100", EphemeralPoolEnd: "10.1.0.200"}
	if !p.admitConfig(cfg) {
		return
	}
	// The API refuses a config whose scope has no production prefix; most
	// of the time make sure one exists (the rest exercises the build error).
	if loc != "" && rapid.IntRange(0, 5).Draw(p.rt, "ensurePrefix") > 0 {
		if id := p.w.locBySlug(site, loc); len(p.w.modelProdPrefixes(cfg)) == 0 {
			p.addPrefix(0, id, propCIDRs[1])
		}
	}
	p.db.configs[slug] = &cfg
	p.st.cfgCreate.Add(1)
	p.inv.Invalidate(slug) // createSiteConfig -> invalidateSite
}

func (p *propPipe) opCfgDelete() {
	cfgs := p.db.sortedConfigs()
	if len(cfgs) == 0 {
		return
	}
	slug := rapid.SampledFrom(cfgs).Draw(p.rt, "cfgDel").Slug
	delete(p.db.configs, slug)
	p.st.cfgDelete.Add(1)
	p.hookClean = false // deleteSiteConfig invalidates nothing
}

func (p *propPipe) opCfgUpdate() {
	// PATCH /site-configs/:slug loads the row, validates against NetBox,
	// then writes; other steps can land in between. This is the second
	// half of an in-flight PATCH: store.UpdateSiteConfig re-runs the
	// overlap check against the rows as they now stand (with the locations
	// read earlier), atomically with writing the operator-owned columns of
	// the row loaded earlier (never the snapshot seq and hash, which the
	// dispatcher owns), then the handler invalidates. A row deleted in
	// between stays deleted.
	if u := p.pendingUpdate; u != nil {
		locs := p.pendingLocs
		p.pendingUpdate, p.pendingLocs = nil, nil
		if err := CheckOverlap(*u, p.db.sortedConfigs(), locs); err != nil {
			p.st.cfgReject.Add(1)
			return
		}
		if cur, ok := p.db.configs[u.Slug]; ok {
			cur.NetBoxSite, cur.NetBoxLocation, cur.Promiscuous = u.NetBoxSite, u.NetBoxLocation, u.Promiscuous
			cur.EphemeralPoolStart, cur.EphemeralPoolEnd, cur.RelayPools = u.EphemeralPoolStart, u.EphemeralPoolEnd, u.RelayPools
			p.st.cfgUpdate.Add(1)
			p.inv.Invalidate(u.Slug)
		}
		return
	}
	cfgs := p.db.sortedConfigs()
	if len(cfgs) == 0 {
		return
	}
	cfg := rapid.SampledFrom(cfgs).Draw(p.rt, "cfgUpd")
	if rapid.Bool().Draw(p.rt, "updScope") {
		cfg.NetBoxSite, cfg.NetBoxLocation = p.drawScope("upd")
	} else {
		cfg.Promiscuous = !cfg.Promiscuous
	}
	if !p.admitConfig(cfg) {
		return
	}
	if rapid.Bool().Draw(p.rt, "updSlow") {
		// Loaded and validated; the write comes later.
		p.pendingUpdate = &cfg
		p.pendingLocs = p.locations(cfg.NetBoxSite)
		return
	}
	*p.db.configs[cfg.Slug] = cfg
	p.st.cfgUpdate.Add(1)
	p.inv.Invalidate(cfg.Slug) // updateSiteConfig -> invalidateSite
}

func (p *propPipe) invalidateAll() {
	for _, c := range p.db.sortedConfigs() {
		p.inv.Invalidate(c.Slug)
	}
}

func (p *propPipe) opMachine() {
	m := p.pickMachine("mach")
	if m == nil {
		return
	}
	p.explicit[m.ID] = true
	switch rapid.IntRange(0, 2).Draw(p.rt, "machField") {
	case 0:
		m.State = model.MachineState(rapid.SampledFrom([]string{"new", "commissioned", "running", "failed"}).Draw(p.rt, "state"))
	case 1:
		m.Locked = !m.Locked
	default:
		m.PXEEligible = !m.PXEEligible
	}
	p.invalidateAll() // API machine mutations fan out to every config
}

func (p *propPipe) opStartOp() {
	if m := p.pickMachine("opMach"); m != nil {
		p.db.inflight[m.ID]++
	}
}

func (p *propPipe) tick() {
	p.st.ticks.Add(1)
	p.rec.ManualTick(context.Background())
}

func (p *propPipe) opLateHook() {
	if len(p.deferred) == 0 {
		return
	}
	i := rapid.IntRange(0, len(p.deferred)-1).Draw(p.rt, "late")
	body := p.deferred[i]
	p.deferred = append(p.deferred[:i], p.deferred[i+1:]...)
	p.post(body)
}

func (p *propPipe) opFault() {
	if !p.k.Faults {
		return
	}
	p.nb.failIn = rapid.IntRange(1, 12).Draw(p.rt, "failIn")
	p.nb.failRun = rapid.IntRange(1, 3).Draw(p.rt, "failRun")
	p.hookClean = false
}

// settle lets the safety net run with no further change and checks the
// fixed point.
func (p *propPipe) settle() {
	p.st.settles.Add(1)
	p.nb.failIn, p.nb.failRun = 0, 0
	for range 3 {
		p.tick()
		p.drain()
	}
	before := p.machinesCopy()
	pushes := 0
	for _, n := range p.sink.pushes {
		pushes += n
	}
	p.tick()
	p.drain()
	after := 0
	for _, n := range p.sink.pushes {
		after += n
	}
	if after != pushes {
		p.rt.Fatalf("reconcile is not idempotent: a 4th tick over an unchanged world pushed %d more snapshot(s)", after-pushes)
	}
	now := p.machinesCopy()
	if len(now) != len(before) {
		p.rt.Fatalf("reconcile is not idempotent: a 4th tick over an unchanged world changed the machine table from %d to %d rows", len(before), len(now))
	}
	for id, b := range before {
		a, ok := now[id]
		if !ok || fmt.Sprint(a.NetBoxDeviceID != nil, a.State, a.NetBoxSerial) != fmt.Sprint(b.NetBoxDeviceID != nil, b.State, b.NetBoxSerial) ||
			(a.NetBoxDeviceID != nil && *a.NetBoxDeviceID != *b.NetBoxDeviceID) {
			p.rt.Fatalf("reconcile is not idempotent: machine %s changed on a 4th tick over an unchanged world", id)
		}
	}
	if v := p.storeViolations(); len(v) > 0 {
		p.rt.Fatalf("reconcile did not converge after 4 ticks over an unchanged world:\n  %s", strings.Join(v, "\n  "))
	}
	for _, cfg := range p.db.sortedConfigs() {
		if want, got := p.modelRefused(cfg), p.rec.IsStale(cfg.Slug); got != (want != "") {
			p.rt.Fatalf("after settle the reconciler flags %s stale=%v (%v), model says %q", cfg.Slug, got, p.rec.StaleSites(), want)
		}
	}
	for slug := range p.rec.StaleSites() {
		if p.db.configs[slug] == nil {
			p.rt.Fatalf("after settle the reconciler still flags %s stale, but no such site_config exists", slug)
		}
	}
	p.checkAllFresh("after settle")
	if len(p.inv.dirty) != 0 {
		p.rt.Fatalf("slugs still dirty after settle: %v", p.inv.dirty)
	}
	p.hookClean = true
}

func (p *propPipe) step(kind int) {
	p.st.ops.Add(1)
	p.st.opKinds[kind].Add(1)
	before := p.machinesCopy()
	p.inflightBefore = maps.Clone(p.db.inflight)
	p.hookDevs = nil
	p.multiDelivery = kind == opSettle
	p.liveOnly = kind == opTick || kind == opSettle
	wasClean := p.hookClean
	isHook := false
	switch kind {
	case opDevCreate:
		p.opDevCreate()
		isHook = true
	case opDevDelete:
		p.opDevDelete()
		isHook = true
	case opDevMove:
		p.opDevMove()
		isHook = true
	case opDevRename:
		p.opDevRename()
		isHook = true
	case opDevSerial:
		p.opDevSerial()
		isHook = true
	case opDevAttr:
		p.opDevAttr()
		isHook = true
	case opIface:
		p.opIface()
	case opIP:
		p.opIP()
	case opPrefix:
		p.opPrefix()
	case opLoc:
		p.opLoc()
	case opCfgCreate:
		p.opCfgCreate()
	case opCfgDelete:
		p.opCfgDelete()
	case opCfgUpdate:
		p.opCfgUpdate()
	case opMachine:
		p.opMachine()
	case opStartOp:
		p.opStartOp()
	case opTick:
		p.tick()
	case opLateHook:
		p.opLateHook()
	case opFault:
		p.opFault()
	case opSettle:
		p.drain()
		p.checkIdentity(before, nil)
		before = p.machinesCopy()
		p.settle()
	case opTouch:
		p.invalidateAll()
	case opStaleCfg:
		p.opStaleCfg()
	}
	p.drain()
	p.checkIdentity(before, nil)
	p.checkClaims()
	if isHook && wasClean && p.hookClean {
		// The webhook fast path must leave every worker's snapshot fresh,
		// including the config a moved device just left, and the device
		// the webhook named enrolled, without waiting for the reconcile
		// tick. (A device that became claimed some other way, such as by
		// a new site_config, is enrolled by the next tick.)
		p.checkAllFresh("after an in-order webhook delivery")
		for _, id := range p.hookDevs {
			d := p.w.devs[id]
			if d == nil || p.serialHeldByGoneDevice(d.Serial) {
				// A row kept after its device was deleted mid-operation
				// still holds the serial, and the webhook path refuses a
				// re-created device with that serial instead of rebinding
				// the row. Reconcile sweeps the row and enrolls the device
				// as a new machine (the settle checks cover that).
				continue
			}
			if gap := p.enrollGap(d, p.modelClaimers(d)); gap != "" {
				p.rt.Fatalf("after an in-order webhook delivery: %s", gap)
			}
		}
	}
	propMax(&p.st.maxDevs, len(p.w.devs))
	propMax(&p.st.maxMachines, len(p.db.machines))
	propMax(&p.st.maxSlugs, len(p.db.configs))
	for id := range p.w.locs {
		propMax(&p.st.maxLocDepth, p.locDepth(id))
	}
}

// propRunPipeline checks generated histories of up to maxOps steps.
func propRunPipeline(t *testing.T, k propKnobs, maxOps int) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(old)
	defer uuid.SetRand(nil)
	st := &propStats{}
	// Swarm: each case enables a random subset of op kinds on top of a
	// weighted base mix, so some histories are delete-heavy, some
	// config-churn-heavy, some reconcile-only.
	weights := [propNumOps]int{opDevCreate: 6, opDevDelete: 4, opDevMove: 6, opDevRename: 2, opDevSerial: 3, opDevAttr: 2,
		opIface: 2, opIP: 3, opPrefix: 2, opLoc: 3, opCfgCreate: 4, opCfgDelete: 2, opCfgUpdate: 3, opMachine: 2,
		opStartOp: 2, opTick: 4, opLateHook: 3, opFault: 2, opSettle: 2, opTouch: 1, opStaleCfg: 2}
	if k.LocReparent {
		weights[opLoc] = 9 // re-parents are a third of location edits
	}
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		uuid.SetRand(rand.New(rand.NewSource(rapid.Int64Range(0, 1<<30).Draw(rt, "uuidSeed"))))
		p := newPropPipe(rt, k, st)
		p.setup()
		mask := rapid.SliceOfN(rapid.Bool(), propNumOps, propNumOps).Draw(rt, "swarm")
		var pool []int
		for kind, w := range weights {
			if !mask[kind] && (kind == opDevCreate || kind == opCfgCreate) {
				w = 1 // always possible, so a case is never empty
			} else if !mask[kind] {
				w = 0
			}
			for range w {
				pool = append(pool, kind)
			}
		}
		n := rapid.IntRange(1, maxOps).Draw(rt, "nOps")
		for range n {
			p.step(rapid.SampledFrom(pool).Draw(rt, "op"))
		}
		before := p.machinesCopy()
		p.multiDelivery, p.liveOnly = true, true // settle runs several ticks
		p.settle()
		p.checkIdentity(before, nil)
		p.checkClaims()
		st.nbFaults.Add(int64(p.nb.failed))
	})
	t.Logf("coverage:\n%s", st)
}

// TestPropPipeline runs the full history space with NetBox and the store
// returning their lists in a new order on every call.
func TestPropPipeline(t *testing.T) {
	propRunPipeline(t, propKnobs{Shuffle: true, StaleHooks: true, Faults: true}, 40)
}

// TestPropPipelineOrdered runs shorter histories with stable list order and
// reliable delivery, so the checks that follow an in-order webhook run after
// every device change.
func TestPropPipelineOrdered(t *testing.T) {
	propRunPipeline(t, propKnobs{}, 20)
}

// TestPropPipelineLocReparent adds re-parenting of NetBox locations.
func TestPropPipelineLocReparent(t *testing.T) {
	propRunPipeline(t, propKnobs{Shuffle: true, StaleHooks: true, LocReparent: true}, 20)
}

// TestPropPipelineSerialReuse lets a device's serial change after it is set.
func TestPropPipelineSerialReuse(t *testing.T) {
	t.Skip("a corrected NetBox serial is never mirrored: the machine row keeps the old serial, and the device that really carries it is refused enrollment for as long as the row lives")
	propRunPipeline(t, propKnobs{SerialReuse: true, StaleHooks: true}, 40)
}

// TestPropPipelineKeepIdentity requires a serial to keep its machine across
// a NetBox device delete + re-create.
func TestPropPipelineKeepIdentity(t *testing.T) {
	t.Skip("deleting a NetBox device and re-creating it with the same serial enrolls a new machine: the old row is deleted with the device, so there is nothing left to rebind")
	propRunPipeline(t, propKnobs{KeepIdentity: true}, 40)
}
