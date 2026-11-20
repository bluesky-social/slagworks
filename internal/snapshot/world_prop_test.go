package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
)

// This file is the model world for the snapshot-pipeline property test
// (pipeline_prop_test.go): a small in-memory NetBox (sites, a location
// forest, devices, interfaces, IPs, prefixes) and a small in-memory Postgres
// (site_configs, machines, in-flight op counts), plus fakes that expose them
// through the exact interfaces the dispatcher, reconciler, claim resolver
// and mirror consume.
//
// Unlike fakes_test.go's fakeNetBox, propNB honours every query filter
// (site, device ids, interface ids, prefix scope + tag), so a production
// caller that asks for the wrong thing gets the wrong answer instead of the
// whole table. It can also return every list in a per-call shuffled order
// and fail call k of a schedule.
//
// The oracles in the property tests read the propWorld structs directly and
// never call production helpers (resolveScope, subtreeIDs, Scope.Claims*).

type pwSite struct {
	ID   int
	Slug string
}

type pwLoc struct {
	ID     int
	Slug   string
	Site   string
	Parent int // 0 = root
}

type pwDev struct {
	ID      int
	Name    string
	Serial  string
	Site    string
	Loc     int // 0 = no location
	DType   int
	OOB     string // CIDR, "" = no BMC
	Primary bool
}

type pwIface struct {
	ID   int
	Dev  int
	Name string
	MAC  string
	Mgmt bool
}

type pwIP struct {
	ID       int
	Iface    int
	Addr     string
	Reserved bool
}

type pwPrefix struct {
	ID    int
	CIDR  string
	Site  int // scope site id, 0 if location-scoped
	Loc   int // scope location id, 0 if site-scoped
	Tag   string
	GW    string
	Lease int
}

type pwDType struct {
	ID    int
	Slug  string
	Model string
	KP    string
}

type propWorld struct {
	sites    map[string]*pwSite
	locs     map[int]*pwLoc
	devs     map[int]*pwDev
	ifaces   map[int]*pwIface
	ips      map[int]*pwIP
	prefixes map[int]*pwPrefix
	dtypes   map[int]*pwDType
	nextID   int
}

func newPropWorld() *propWorld {
	return &propWorld{
		sites: map[string]*pwSite{}, locs: map[int]*pwLoc{}, devs: map[int]*pwDev{},
		ifaces: map[int]*pwIface{}, ips: map[int]*pwIP{}, prefixes: map[int]*pwPrefix{},
		dtypes: map[int]*pwDType{}, nextID: 100,
	}
}

func (w *propWorld) id() int { w.nextID++; return w.nextID }

func propSortedIDs[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// inSubtree is the brute-force ancestor walk the claim oracle uses: is loc
// equal to root or beneath it?
func (w *propWorld) inSubtree(loc, root int) bool {
	for steps := 0; loc != 0 && steps < 64; steps++ {
		if loc == root {
			return true
		}
		l, ok := w.locs[loc]
		if !ok {
			return false
		}
		loc = l.Parent
	}
	return false
}

// locBySlug returns the id of the location with this slug in the site, 0 if
// none. The generators keep location slugs unique.
func (w *propWorld) locBySlug(site, slug string) int {
	for _, id := range propSortedIDs(w.locs) {
		if l := w.locs[id]; l.Site == site && l.Slug == slug {
			return id
		}
	}
	return 0
}

// modelClaims is the independent claim predicate: a config claims a device
// iff the device is in the config's NetBox site and (the config is
// whole-site, or the device's location is the config's location or a
// descendant of it). A config whose location does not exist claims nothing.
func (w *propWorld) modelClaims(cfg model.SiteConfig, d *pwDev) bool {
	if d.Site == "" || d.Site != cfg.NetBoxSite {
		return false
	}
	if cfg.NetBoxLocation == "" {
		return true
	}
	root := w.locBySlug(cfg.NetBoxSite, cfg.NetBoxLocation)
	if root == 0 || d.Loc == 0 {
		return false
	}
	return w.inSubtree(d.Loc, root)
}

// modelProdPrefixes returns the slagworks-production prefixes in a config's
// scope, by id: site-scoped ones for a whole-site config, ones scoped to any
// location in the subtree for a location-scoped config.
func (w *propWorld) modelProdPrefixes(cfg model.SiteConfig) []*pwPrefix {
	var out []*pwPrefix
	site := w.sites[cfg.NetBoxSite]
	if site == nil {
		return nil
	}
	root := 0
	if cfg.NetBoxLocation != "" {
		root = w.locBySlug(cfg.NetBoxSite, cfg.NetBoxLocation)
		if root == 0 {
			return nil
		}
	}
	for _, id := range propSortedIDs(w.prefixes) {
		p := w.prefixes[id]
		if p.Tag != "slagworks-production" {
			continue
		}
		if root == 0 && p.Site == site.ID {
			out = append(out, p)
		}
		if root != 0 && p.Loc != 0 && w.inSubtree(p.Loc, root) {
			out = append(out, p)
		}
	}
	return out
}

// modelPrimaryPrefix is the production prefix a config serves DHCP from: a
// whole-site config has one site-scoped prefix; a location-scoped config
// takes the one on its own location, else the one on the lowest-numbered
// location of its subtree. The generators keep one prefix per location and
// one site-scoped prefix per site. The caller has checked there is one.
func (w *propWorld) modelPrimaryPrefix(cfg model.SiteConfig) *pwPrefix {
	pf := w.modelProdPrefixes(cfg)
	own := w.locBySlug(cfg.NetBoxSite, cfg.NetBoxLocation)
	best := pf[0]
	for _, p := range pf[1:] {
		if best.Loc != own && (p.Loc == own || p.Loc < best.Loc) {
			best = p
		}
	}
	return best
}

func (w *propWorld) nbDevice(d *pwDev) netbox.Device {
	out := netbox.Device{ID: d.ID, Name: d.Name, Serial: d.Serial}
	if s := w.sites[d.Site]; s != nil {
		out.Site = &netbox.NestedRef{ID: s.ID, Slug: s.Slug}
	} else if d.Site != "" {
		out.Site = &netbox.NestedRef{ID: 9999, Slug: d.Site}
	}
	if l := w.locs[d.Loc]; l != nil {
		out.Location = &netbox.NestedRef{ID: l.ID, Slug: l.Slug}
	}
	if dt := w.dtypes[d.DType]; dt != nil {
		out.DeviceType = &netbox.NestedRef{ID: dt.ID, Slug: dt.Slug}
	}
	if d.OOB != "" {
		out.OOBIP = &netbox.NestedIPAddress{ID: 1, Address: d.OOB}
	}
	if d.Primary {
		out.PrimaryIP4 = &netbox.NestedRef{ID: 2}
	}
	return out
}

// propNB serves a propWorld through NetBoxReader.
type propNB struct {
	w *propWorld

	// shuffle, when set, returns every list in a fresh pseudo-random order
	// per call (deterministic in seed + call count).
	shuffle bool
	seed    int64
	calls   int64

	// failIn > 0 fails the failIn-th call from now with errPropNB, then
	// the following failRun-1 calls too.
	failIn  int
	failRun int
	failed  int // total injected failures (coverage)
}

var errPropNB = errors.New("injected netbox failure")

func (n *propNB) tick() error {
	n.calls++
	if n.failIn > 0 {
		n.failIn--
		if n.failIn == 0 {
			if n.failRun > 1 {
				n.failRun--
				n.failIn = 1
			}
			n.failed++
			return errPropNB
		}
	}
	return nil
}

func propShuffleSlice[T any](n *propNB, s []T) []T {
	if n.shuffle {
		r := rand.New(rand.NewSource(n.seed + n.calls*7919))
		r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
	}
	return s
}

func (n *propNB) GetSiteBySlug(_ context.Context, slug string) (*netbox.Site, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	s := n.w.sites[slug]
	if s == nil {
		return nil, nil
	}
	return &netbox.Site{ID: s.ID, Name: strings.ToUpper(s.Slug), Slug: s.Slug}, nil
}

func (n *propNB) GetDeviceByID(_ context.Context, id int) (*netbox.Device, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	d := n.w.devs[id]
	if d == nil {
		return nil, nil
	}
	out := n.w.nbDevice(d)
	return &out, nil
}

func (n *propNB) ListDevicesForSite(_ context.Context, slug string) ([]netbox.Device, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	var out []netbox.Device
	for _, id := range propSortedIDs(n.w.devs) {
		if d := n.w.devs[id]; d.Site == slug {
			out = append(out, n.w.nbDevice(d))
		}
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) ListLocationsForSite(_ context.Context, slug string) ([]netbox.Location, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	var out []netbox.Location
	for _, id := range propSortedIDs(n.w.locs) {
		l := n.w.locs[id]
		if l.Site != slug {
			continue
		}
		loc := netbox.Location{ID: l.ID, Name: l.Slug, Slug: l.Slug}
		if s := n.w.sites[l.Site]; s != nil {
			loc.Site = &netbox.NestedRef{ID: s.ID, Slug: s.Slug}
		}
		if l.Parent != 0 {
			loc.Parent = &netbox.NestedRef{ID: l.Parent}
		}
		out = append(out, loc)
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) ListInterfacesForDevices(_ context.Context, ids []int) ([]netbox.Interface, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []netbox.Interface
	for _, id := range propSortedIDs(n.w.ifaces) {
		i := n.w.ifaces[id]
		if !want[i.Dev] {
			continue
		}
		out = append(out, netbox.Interface{
			ID: i.ID, Name: i.Name, MACAddress: i.MAC, MgmtOnly: i.Mgmt,
			Device: &netbox.NestedRef{ID: i.Dev},
			Type:   &netbox.NestedValue{Value: "25gbase-x-sfp28"},
		})
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) ListIPAddressesForInterfaces(_ context.Context, ids []int) ([]netbox.IPAddress, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []netbox.IPAddress
	for _, id := range propSortedIDs(n.w.ips) {
		ip := n.w.ips[id]
		if !want[ip.Iface] {
			continue
		}
		ifID := ip.Iface
		a := netbox.IPAddress{ID: ip.ID, Address: ip.Addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &ifID}
		if ip.Reserved {
			a.Status = &netbox.NestedValue{Value: "reserved"}
		} else {
			a.Status = &netbox.NestedValue{Value: "active"}
		}
		out = append(out, a)
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) nbPrefix(p *pwPrefix) netbox.Prefix {
	cf := map[string]any{}
	if p.GW != "" {
		cf["gateway_ip"] = p.GW
	}
	if p.Lease != 0 {
		cf["dhcp_lease_seconds"] = p.Lease
	}
	raw, _ := json.Marshal(cf)
	out := netbox.Prefix{ID: p.ID, Prefix: p.CIDR, CustomFields: raw, Tags: []netbox.NestedTag{{Slug: p.Tag}}}
	if p.Loc != 0 {
		out.ScopeType, out.ScopeID = "dcim.location", p.Loc
	} else {
		out.ScopeType, out.ScopeID = "dcim.site", p.Site
	}
	return out
}

func (n *propNB) ListSitePrefixes(_ context.Context, slug, tag string) ([]netbox.Prefix, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	s := n.w.sites[slug]
	var out []netbox.Prefix
	for _, id := range propSortedIDs(n.w.prefixes) {
		p := n.w.prefixes[id]
		if s != nil && p.Loc == 0 && p.Site == s.ID && p.Tag == tag {
			out = append(out, n.nbPrefix(p))
		}
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) ListLocationPrefixes(_ context.Context, locationID int, tag string) ([]netbox.Prefix, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	var out []netbox.Prefix
	for _, id := range propSortedIDs(n.w.prefixes) {
		p := n.w.prefixes[id]
		if p.Loc == locationID && p.Tag == tag {
			out = append(out, n.nbPrefix(p))
		}
	}
	return propShuffleSlice(n, out), nil
}

func (n *propNB) ListIPRangesByRole(_ context.Context, _, _ string) ([]netbox.IPRange, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (n *propNB) ListDeviceTypesByIDs(_ context.Context, ids []int) ([]netbox.DeviceType, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	var out []netbox.DeviceType
	for _, id := range ids {
		dt := n.w.dtypes[id]
		if dt == nil {
			return nil, fmt.Errorf("get device-type %d: 404", id)
		}
		raw, _ := json.Marshal(map[string]string{"slagworks_kernel_params": dt.KP})
		out = append(out, netbox.DeviceType{ID: dt.ID, Slug: dt.Slug, Model: dt.Model, CustomFields: raw})
	}
	return out, nil
}

func (n *propNB) ListConsolePortsForDevices(_ context.Context, _ []int) ([]netbox.ConsolePort, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	return nil, nil
}

// propStore is the in-memory Postgres: site_configs and machines with the
// real schema's uniqueness rules (netbox_serial unique and non-empty,
// netbox_device_id unique among non-null), and a per-machine in-flight op
// count for the B7 drift paths. It implements StoreReader, ReconcileStore,
// claimConfigLister, ConfigOwnerStore and mirror.Store.
type propStore struct {
	mu       sync.Mutex
	configs  map[string]*model.SiteConfig
	machines map[uuid.UUID]*model.Machine
	inflight map[uuid.UUID]int
	images   []model.Image
	keys     []string

	shuffle bool
	seed    int64
	calls   int64

	// snapSeq stands in for the site_snapshot_seq Postgres sequence: one
	// counter shared by every slug, so a value is never issued twice.
	snapSeq int64
}

func newPropStore() *propStore {
	return &propStore{
		configs: map[string]*model.SiteConfig{}, machines: map[uuid.UUID]*model.Machine{},
		inflight: map[uuid.UUID]int{},
		images:   []model.Image{{ID: uuid.MustParse("00000000-0000-0000-0000-0000000000aa"), Name: "img", Version: "1", Arch: "amd64", SHA256: "x", URL: "u"}},
		keys:     []string{"ssh-ed25519 AAAA a", "ssh-ed25519 BBBB b"},
	}
}

func (s *propStore) sortedMachines() []model.Machine {
	out := make([]model.Machine, 0, len(s.machines))
	for _, m := range s.machines {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	if s.shuffle {
		s.calls++
		r := rand.New(rand.NewSource(s.seed + s.calls*104729))
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

func (s *propStore) sortedConfigs() []model.SiteConfig {
	slugs := make([]string, 0, len(s.configs))
	for k := range s.configs {
		slugs = append(slugs, k)
	}
	sort.Strings(slugs)
	out := make([]model.SiteConfig, 0, len(slugs))
	for _, k := range slugs {
		out = append(out, *s.configs[k])
	}
	return out
}

func (s *propStore) GetSiteConfig(slug string) (*model.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.configs[slug]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cc := *c
	return &cc, nil
}

func (s *propStore) ListSiteConfigs() ([]model.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedConfigs(), nil
}

func (s *propStore) ListSiteConfigsByNetBoxSite(site string) ([]model.SiteConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.SiteConfig
	for _, c := range s.sortedConfigs() {
		if c.NetBoxSite == site {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *propStore) AnySiteConfigForNetBoxSite(site string) (bool, error) {
	cfgs, _ := s.ListSiteConfigsByNetBoxSite(site)
	return len(cfgs) > 0, nil
}

func (s *propStore) BumpSiteSnapshotMeta(slug, hash string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.configs[slug]
	if !ok {
		return 0, nil // UPDATE ... RETURNING matched no row
	}
	s.snapSeq++
	c.LastSnapshotSeq = s.snapSeq
	c.LastSnapshotHash = hash
	return c.LastSnapshotSeq, nil
}

func (s *propStore) ListMachines(state *model.MachineState) ([]model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Machine
	for _, m := range s.sortedMachines() {
		if state == nil || m.State == *state {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *propStore) ListMachinesByDeviceIDs(ids []int) ([]model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []model.Machine
	for _, m := range s.sortedMachines() {
		if m.NetBoxDeviceID != nil && want[*m.NetBoxDeviceID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *propStore) ListImages() ([]model.Image, error) {
	return append([]model.Image(nil), s.images...), nil
}
func (s *propStore) ListStorageScripts() ([]model.StorageScript, error) { return nil, nil }
func (s *propStore) ListNetworkScripts() ([]model.NetworkScript, error) { return nil, nil }
func (s *propStore) ListSSHPublicKeys() ([]string, error) {
	return append([]string(nil), s.keys...), nil
}

func (s *propStore) deviceBound(id int, except uuid.UUID) bool {
	for _, m := range s.machines {
		if m.ID != except && m.NetBoxDeviceID != nil && *m.NetBoxDeviceID == id {
			return true
		}
	}
	return false
}

func (s *propStore) CreateMachine(m *model.Machine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.NetBoxSerial == "" {
		return errors.New("violates check constraint machines_serial_not_empty")
	}
	for _, o := range s.machines {
		if o.NetBoxSerial == m.NetBoxSerial {
			return errors.New("duplicate key value violates unique constraint idx_machines_serial")
		}
	}
	if m.NetBoxDeviceID != nil && s.deviceBound(*m.NetBoxDeviceID, uuid.Nil) {
		return errors.New("duplicate key value violates unique constraint idx_machines_netbox_device_id")
	}
	cp := *m
	if cp.NetBoxDeviceID != nil {
		id := *cp.NetBoxDeviceID
		cp.NetBoxDeviceID = &id
	}
	s.machines[m.ID] = &cp
	return nil
}

func (s *propStore) clone(m *model.Machine) *model.Machine {
	cp := *m
	if cp.NetBoxDeviceID != nil {
		id := *cp.NetBoxDeviceID
		cp.NetBoxDeviceID = &id
	}
	return &cp
}

func (s *propStore) GetMachine(id uuid.UUID) (*model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return s.clone(m), nil
}

func (s *propStore) GetMachineByNetBoxDeviceID(id int) (*model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
		if m.NetBoxDeviceID != nil && *m.NetBoxDeviceID == id {
			return s.clone(m), nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *propStore) GetMachineByNetBoxSerial(serial string) (*model.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
		if m.NetBoxSerial == serial {
			return s.clone(m), nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *propStore) SetMachineNetBoxDeviceID(id uuid.UUID, dev int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok {
		return nil // UPDATE matched no row
	}
	if s.deviceBound(dev, id) {
		return errors.New("duplicate key value violates unique constraint idx_machines_netbox_device_id")
	}
	m.NetBoxDeviceID = &dev
	return nil
}

// DeleteMachineIfIdle deletes the row unless the machine has an op in
// flight, as the store does in one transaction.
func (s *propStore) DeleteMachineIfIdle(id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] > 0 {
		return false, nil
	}
	delete(s.machines, id)
	delete(s.inflight, id)
	return true, nil
}

func (s *propStore) FailInFlightOpsForMachine(id uuid.UUID, _ string) ([]store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.inflight[id]
	if n == 0 {
		return nil, nil
	}
	s.inflight[id] = 0
	if m := s.machines[id]; m != nil {
		m.State = model.MachineStateFailed
	}
	out := make([]store.OpOutcome, 0, n)
	for range n {
		out = append(out, store.OpOutcome{
			Op:           model.Operation{ID: uuid.New(), MachineID: id, Type: model.OpDeploy, Status: model.OpFailed},
			MachineState: model.MachineStateFailed,
		})
	}
	return out, nil
}

// propInv is a synchronous Invalidator: it records dirty slugs for the test
// to drain through Dispatcher.rebuild, standing in for the Registry's
// per-slug actors (which registry_prop_test.go exercises on their own).
type propInv struct {
	dirty map[string]bool
	total int
}

func (i *propInv) Invalidate(slug string) {
	if slug == "" {
		return
	}
	if i.dirty == nil {
		i.dirty = map[string]bool{}
	}
	i.dirty[slug] = true
	i.total++
}

// propSink records every push per slug.
type propSink struct {
	last   map[string]model.SiteSnapshot
	pushes map[string]int
	seqs   map[string][]int64
}

func newPropSink() *propSink {
	return &propSink{last: map[string]model.SiteSnapshot{}, pushes: map[string]int{}, seqs: map[string][]int64{}}
}

func (s *propSink) Push(slug string, snap model.SiteSnapshot) {
	s.last[slug] = snap
	s.pushes[slug]++
	s.seqs[slug] = append(s.seqs[slug], snap.SeqNum)
}

// propCanon is the test's own notion of "same snapshot": the JSON form with
// SeqNum dropped and every set-like collection sorted by the full encoding
// of its elements. It deliberately shares nothing with ContentHash's
// comparators.
func propCanon(s model.SiteSnapshot) string {
	s.SeqNum = 0
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		panic(err)
	}
	sortArr := func(a any) any {
		arr, ok := a.([]any)
		if !ok {
			return a
		}
		enc := make([]string, len(arr))
		for i, e := range arr {
			b, _ := json.Marshal(e)
			enc[i] = string(b)
		}
		sort.Strings(enc)
		return enc
	}
	if ms, ok := v["machines"].([]any); ok {
		for _, m := range ms {
			if mm, ok := m.(map[string]any); ok {
				mm["interfaces"] = sortArr(mm["interfaces"])
			}
		}
	}
	for _, k := range []string{"machines", "images", "ssh_keys", "reservations"} {
		v[k] = sortArr(v[k])
	}
	if d, ok := v["dhcp"].(map[string]any); ok {
		d["relay_pools"] = sortArr(d["relay_pools"])
	}
	if o, ok := v["oob"].(map[string]any); ok {
		o["dynamic_slices"] = sortArr(o["dynamic_slices"])
	}
	out, _ := json.Marshal(v)
	return string(out)
}
