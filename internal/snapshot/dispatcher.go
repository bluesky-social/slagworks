package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// NetBoxReader is the subset of the NetBox client the dispatcher calls.
// Kept as a named interface so tests can provide a fake without pulling
// in the full HTTP-backed client.
type NetBoxReader interface {
	GetSiteBySlug(ctx context.Context, slug string) (*netbox.Site, error)
	GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error)
	ListDevicesForSite(ctx context.Context, slug string) ([]netbox.Device, error)
	ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error)
	ListInterfacesForDevices(ctx context.Context, deviceIDs []int) ([]netbox.Interface, error)
	ListIPAddressesForInterfaces(ctx context.Context, ifaceIDs []int) ([]netbox.IPAddress, error)
	ListSitePrefixes(ctx context.Context, slug, tag string) ([]netbox.Prefix, error)
	ListLocationPrefixes(ctx context.Context, locationID int, tag string) ([]netbox.Prefix, error)
	ListIPRangesByRole(ctx context.Context, roleSlug, parentPrefix string) ([]netbox.IPRange, error)
	ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]netbox.DeviceType, error)
	ListConsolePortsForDevices(ctx context.Context, deviceIDs []int) ([]netbox.ConsolePort, error)
}

// StoreReader is the subset of the Store the dispatcher reads.
type StoreReader interface {
	GetSiteConfig(slug string) (*model.SiteConfig, error)
	ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error)
	ListMachinesByDeviceIDs(deviceIDs []int) ([]model.Machine, error)
	ListImages() ([]model.Image, error)
	ListStorageScripts() ([]model.StorageScript, error)
	ListNetworkScripts() ([]model.NetworkScript, error)
	ListSSHPublicKeys() ([]string, error)
	ListSiteWorkers(siteSlug string) ([]model.SiteWorker, error)
	BumpSiteSnapshotMeta(slug, newHash string) (int64, error)
}

// PushSink is where the dispatcher sends fresh snapshots. In production
// it's the WS hub (Plan 2); tests use a counting sink.
type PushSink interface {
	Push(slug string, snap model.SiteSnapshot)
}

// BMCSecretsReader is the subset of awssecrets.Client the dispatcher
// uses to fetch per-pop admin device creds for stamping into snapshots.
// Named so tests can supply a fake (awssecrets.FakeReader works).
type BMCSecretsReader interface {
	GetAdmin(ctx context.Context, pop string) (*awssecrets.AdminSecret, error)
}

// Dispatcher orchestrates a single-site rebuild: gather inputs, Build,
// content-hash, decide whether to push + bump SeqNum.
type Dispatcher struct {
	nb         NetBoxReader
	st         StoreReader
	sink       PushSink
	bmcSecrets BMCSecretsReader // optional; nil = no admin-cred prestamp
	metrics    *Metrics         // optional; nil disables Prometheus instrumentation
	hostnames  HostnameTemplate // zero value = FQDN is the NetBox device name

	// reportedChoices is, per config slug, the multi-address interface
	// list last logged, so a rebuild over unchanged NetBox data does not
	// log it again. Guarded by choicesMu: slugs rebuild concurrently.
	choicesMu       sync.Mutex
	reportedChoices map[string]string
}

// addressChoicesToReport returns the lines to log for a build's
// multi-address interfaces, or nil when the same list was already reported
// for this slug (or there is nothing to report). A slug whose list empties
// is forgotten, so the list is reported again if it comes back.
func (d *Dispatcher) addressChoicesToReport(slug string, choices []AddressChoice) []string {
	lines := make([]string, len(choices))
	for i, c := range choices {
		lines[i] = fmt.Sprintf("%s %s (NetBox interface %d): using %s of %d active addresses",
			c.Device, c.Interface, c.InterfaceID, c.Chosen, c.Active)
	}
	key := strings.Join(lines, "\n")
	d.choicesMu.Lock()
	defer d.choicesMu.Unlock()
	if d.reportedChoices[slug] == key {
		return nil
	}
	if key == "" {
		delete(d.reportedChoices, slug)
		return nil
	}
	if d.reportedChoices == nil {
		d.reportedChoices = make(map[string]string)
	}
	d.reportedChoices[slug] = key
	return lines
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(nb NetBoxReader, st StoreReader, sink PushSink) *Dispatcher {
	return &Dispatcher{nb: nb, st: st, sink: sink}
}

// SetMetrics wires a Metrics bundle for telemetry. Safe to call once
// at startup; not safe to call concurrently with Rebuild.
func (d *Dispatcher) SetMetrics(m *Metrics) { d.metrics = m }

// SetBMCSecrets wires the admin-cred reader. When set, every
// per-machine BMCUser/BMCPass is stamped from the device's serial-keyed
// admin entry (the only BMC credential). Site workers then have creds
// for every BMC in the pop without needing a per-op envelope. Safe to
// call once at startup; not safe to call concurrently with Rebuild.
func (d *Dispatcher) SetBMCSecrets(r BMCSecretsReader) { d.bmcSecrets = r }

// SetHostnameTemplate sets the template every build derives machine FQDNs
// from. Safe to call once at startup; not safe to call concurrently with
// Rebuild.
func (d *Dispatcher) SetHostnameTemplate(t HostnameTemplate) { d.hostnames = t }

// rebuild performs one snapshot rebuild for the given site. If the
// content hash matches the last-sent snapshot, the rebuild is a no-op:
// no push, no SeqNum bump, the snapshot is returned with its existing
// SeqNum for caller inspection. If the hash differs, SeqNum is bumped
// atomically in Postgres and the sink is called.
//
// Unexported by design: this is the only path that bumps SeqNum, and
// the Registry's per-slug actors are its only callers (NewRegistry
// receives it at construction). Serialization per slug is what makes
// the order of BumpSiteSnapshotMeta's values match gather-time order —
// see the ownership note on Registry.
//
// gen is the registry-observed dirtyGen; the dispatcher treats it as
// opaque and logs it for correlation with telemetry.
//
// Metrics (when SetMetrics has been called): RebuildsTotal counter by
// outcome + RebuildDuration histogram fire on every return path.
// DirtyGen gauge updates on every return (so alerting on
// DirtyGen > SnapshotSeq stays accurate even during error storms).
// SnapshotSeq gauge only updates on success / skipped_no_change —
// error returns don't know the current seq, and setting a stale
// value would mislead.
func (d *Dispatcher) rebuild(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error) {
	start := time.Now()
	// outcome is mutated in the success/skip/error paths and consumed
	// by the deferred metric emission. Default is a NetBox-class
	// error because most of the early returns are NetBox calls;
	// per-case assignments override before each return.
	outcome := "error_netbox"
	defer func() {
		if d.metrics == nil {
			return
		}
		d.metrics.RebuildsTotal.WithLabelValues(slug, outcome).Inc()
		d.metrics.RebuildDuration.WithLabelValues(slug).Observe(time.Since(start).Seconds())
		d.metrics.DirtyGen.WithLabelValues(slug).Set(float64(gen))
	}()

	cfg, snap, err := d.gatherAndBuild(ctx, slug, &outcome)
	if err != nil {
		return model.SiteSnapshot{}, err
	}

	hash, err := snap.ContentHash()
	if err != nil {
		return model.SiteSnapshot{}, err
	}
	if hash == cfg.LastSnapshotHash {
		// No change. Return the snapshot with its existing SeqNum so
		// callers can see what we built; don't push.
		snap.SeqNum = cfg.LastSnapshotSeq
		outcome = "skipped_no_change"
		if d.metrics != nil {
			d.metrics.SnapshotSeq.WithLabelValues(slug).Set(float64(snap.SeqNum))
		}
		return snap, nil
	}
	newSeq, err := d.st.BumpSiteSnapshotMeta(slug, hash)
	if err != nil {
		return model.SiteSnapshot{}, err
	}
	snap.SeqNum = newSeq
	d.sink.Push(slug, snap)
	outcome = "success"
	if d.metrics != nil {
		d.metrics.SnapshotSeq.WithLabelValues(slug).Set(float64(snap.SeqNum))
	}
	return snap, nil
}

// BuildSnapshot gathers inputs and builds the SiteSnapshot for a config
// slug READ-ONLY: it performs no content-hash, no SeqNum bump, and no
// push to the sink. It is the exact gather+Build the live boot path uses
// (Rebuild shares the same gatherAndBuild helper), so a caller inspecting
// the result sees precisely the matched scripts, interface facts, and
// image rows the boot would consume. Used by op-creation preflight to
// validate a host's modeled config without mutating snapshot state.
//
// SeqNum is left zero; the returned snapshot is not authoritative for
// sync purposes — only for inspection.
func (d *Dispatcher) BuildSnapshot(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	_, snap, err := d.gatherAndBuild(ctx, slug, nil)
	return snap, err
}

// gatherAndBuild pulls every NetBox + Postgres input for a site config
// and runs Build. It is the single gather path shared by Rebuild (which
// then hashes/pushes) and BuildSnapshot (which does not). outcome, when
// non-nil, is set to the metric-bucket label on the NetBox/Postgres error
// boundary so Rebuild's deferred emission reports correctly; BuildSnapshot
// passes nil.
func (d *Dispatcher) gatherAndBuild(ctx context.Context, slug string, outcome *string) (model.SiteConfig, model.SiteSnapshot, error) {
	setOutcome := func(v string) {
		if outcome != nil {
			*outcome = v
		}
	}

	cfg, err := d.st.GetSiteConfig(slug)
	if err != nil {
		setOutcome("error_postgres")
		return model.SiteConfig{}, model.SiteSnapshot{}, fmt.Errorf("get site_config %s: %w", slug, err)
	}
	// NetBox lookups key on cfg.NetBoxSite, NOT the config slug — the
	// slug is the worker identity (e.g. pop2-old-cage) and need not be a
	// NetBox site slug.
	nbSite := cfg.NetBoxSite
	siteRef, err := d.nb.GetSiteBySlug(ctx, nbSite)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, fmt.Errorf("netbox site %s: %w", nbSite, err)
	}
	if siteRef == nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, fmt.Errorf("site %s not found in NetBox (stale site_config %s)", nbSite, slug)
	}

	// Resolve the config's scope (whole-site, or a location subtree).
	// resolveClaimScope enforces the belongs-to invariant: a
	// location-scoped config whose location is absent from the site (gone,
	// or re-parented to another site) fails loudly here rather than scoping
	// to nothing. It also fails a config whose scope now overlaps another
	// config's on the same site (a location moved under another), so no
	// device lands in two workers' snapshots.
	locations, err := d.nb.ListLocationsForSite(ctx, nbSite)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	siblings, err := d.st.ListSiteConfigsByNetBoxSite(nbSite)
	if err != nil {
		setOutcome("error_postgres")
		return model.SiteConfig{}, model.SiteSnapshot{}, fmt.Errorf("list site_configs for netbox_site %s: %w", nbSite, err)
	}
	scope, err := resolveClaimScope(*cfg, siblings, locations)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}

	// Prefix discovery: whole-site configs read site-scoped prefixes;
	// location-scoped configs read prefixes scoped to ANY location in the
	// claimed subtree (NetBox 4.x polymorphic scope — `?site=` does NOT
	// include location-scoped prefixes, and a single-location query would
	// miss prefixes scoped to descendant locations that Scope.ClaimsPrefix
	// would otherwise accept). Either way Build asserts a non-empty
	// slagworks-production prefix and fails loudly if absent.
	var prefixes []netbox.Prefix
	if scope.WholeSite() {
		prefixes, err = d.nb.ListSitePrefixes(ctx, nbSite, "slagworks-production")
	} else {
		prefixes, err = d.locationSubtreePrefixes(ctx, scope)
	}
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}

	// OOB prefixes (tag=slagworks-oob): the management network slagworks
	// serves DHCP reservations from, replacing dnsmasq. Same scope rules
	// as the production prefixes. Absent (empty) on sites that haven't been
	// cut over — Build then emits no Reservations, which is correct.
	var oobPrefixes []netbox.Prefix
	if scope.WholeSite() {
		oobPrefixes, err = d.nb.ListSitePrefixes(ctx, nbSite, "slagworks-oob")
	} else {
		oobPrefixes, err = d.locationSubtreePrefixesTagged(ctx, scope, "slagworks-oob")
	}
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}

	// OOB dynamic-pool ranges (IPAM role slagworks-oob-dynamic) under each
	// OOB prefix. Empty pre-cutover. Build picks the pool bounds from these.
	var oobRanges []netbox.IPRange
	for _, p := range oobPrefixes {
		ranges, rerr := d.nb.ListIPRangesByRole(ctx, "slagworks-oob-dynamic", p.Prefix)
		if rerr != nil {
			return model.SiteConfig{}, model.SiteSnapshot{}, rerr
		}
		oobRanges = append(oobRanges, ranges...)
	}

	allDevices, err := d.nb.ListDevicesForSite(ctx, nbSite)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	// Scope devices to the config's claim. Whole-site claims everything;
	// a location-scoped config claims only its subtree. Devices in the
	// site but outside the scope (the other cage, or no location set on a
	// location-scoped config) are dropped here — and for location-scoped
	// configs the dropped-no-location count is surfaced (ruling 5) so a
	// NetBox data gap is visible rather than silently mis-mirrored.
	devices := make([]netbox.Device, 0, len(allDevices))
	unclaimedNoLocation := 0
	for _, dev := range allDevices {
		if scope.ClaimsDevice(dev) {
			devices = append(devices, dev)
			continue
		}
		if !scope.WholeSite() && dev.Location == nil {
			unclaimedNoLocation++
		}
	}
	if unclaimedNoLocation > 0 {
		slog.Warn("snapshot: devices in site have no location and are unclaimed by location-scoped config",
			"config", slug, "netbox_site", nbSite, "location", scope.LocationSlug,
			"unclaimed_no_location", unclaimedNoLocation)
	}
	deviceIDs := make([]int, len(devices))
	for i, dev := range devices {
		deviceIDs[i] = dev.ID
	}
	interfaces, err := d.nb.ListInterfacesForDevices(ctx, deviceIDs)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}

	// Pull each unique device-type once so the builder can read
	// custom_fields.slagworks_kernel_params per machine. Most pops
	// have a small number of distinct device-types (3-5) across
	// dozens of devices, so dedup before fetching.
	dtIDSeen := make(map[int]bool, len(devices))
	dtIDs := make([]int, 0, len(devices))
	for _, dev := range devices {
		if dev.DeviceType == nil {
			continue
		}
		if dtIDSeen[dev.DeviceType.ID] {
			continue
		}
		dtIDSeen[dev.DeviceType.ID] = true
		dtIDs = append(dtIDs, dev.DeviceType.ID)
	}
	deviceTypes, err := d.nb.ListDeviceTypesByIDs(ctx, dtIDs)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	// Console ports of the scoped devices, with their cabled far end, plus
	// the console-server devices those ends sit on (resolved from the
	// unscoped site list: the oob-gw can sit outside a location claim).
	consolePorts, err := d.nb.ListConsolePortsForDevices(ctx, deviceIDs)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	consoleServerIDs := make(map[int]bool)
	for _, p := range consolePorts {
		for _, ep := range p.ConnectedEndpoints {
			if ep.Device != nil {
				consoleServerIDs[ep.Device.ID] = true
			}
		}
	}
	var consoleServers []netbox.Device
	for _, dev := range allDevices {
		if consoleServerIDs[dev.ID] {
			consoleServers = append(consoleServers, dev)
		}
	}
	ifaceIDs := make([]int, len(interfaces))
	for i, iface := range interfaces {
		ifaceIDs[i] = iface.ID
	}
	ips, err := d.nb.ListIPAddressesForInterfaces(ctx, ifaceIDs)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	// From here on, remaining errors are Postgres-side. Flip the
	// default so the deferred emission reports correctly.
	setOutcome("error_postgres")
	machines, err := d.st.ListMachinesByDeviceIDs(deviceIDs)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	images, err := d.st.ListImages()
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	scripts, err := d.st.ListStorageScripts()
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	netScripts, err := d.st.ListNetworkScripts()
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	sshKeys, err := d.st.ListSSHPublicKeys()
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	siteWorkers, err := d.st.ListSiteWorkers(slug)
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, fmt.Errorf("list site workers for %s: %w", slug, err)
	}
	workerIDs := make([]string, len(siteWorkers))
	for i, w := range siteWorkers {
		workerIDs[i] = w.WorkerID
	}

	// Per-pop BMC admin creds: pre-stamped onto each MachineSnapshot
	// so the site worker's Cache holds creds for every BMC in the
	// site without needing a per-op envelope. The secret path is keyed
	// on the NetBox site (pop/<netbox_site>/bmc-creds-admin), NOT the
	// config slug — both cage workers of a dual-cage site share the one
	// per-site secret (serial-keyed, so sharing is safe). Soft-fail: if
	// the secret API errors (network, IAM), log and proceed — the
	// snapshot still ships, only the BMCUser/BMCPass fields are empty.
	// Ops are unaffected: they carry their own resolved bmc_runtime.
	var adminSecret *awssecrets.AdminSecret
	if d.bmcSecrets != nil {
		s, err := d.bmcSecrets.GetAdmin(ctx, nbSite)
		if err != nil {
			slog.Warn("snapshot: bmc admin secret fetch failed; building without admin-cred prestamp",
				"pop", nbSite, "config", slug, "err", err)
		} else {
			adminSecret = s
		}
	}

	snap, choices, err := buildWithChoices(Inputs{
		Site:             *siteRef,
		Config:           *cfg,
		Prefixes:         prefixes,
		OOBPrefixes:      oobPrefixes,
		OOBRanges:        oobRanges,
		Devices:          devices,
		DeviceTypes:      deviceTypes,
		Interfaces:       interfaces,
		IPs:              ips,
		Machines:         machines,
		Images:           images,
		Scripts:          scripts,
		NetworkScripts:   netScripts,
		SSHKeys:          sshKeys,
		Workers:          workerIDs,
		ConsolePorts:     consolePorts,
		ConsoleServers:   consoleServers,
		BMCAdminSecret:   adminSecret,
		HostnameTemplate: d.hostnames,
	})
	if err != nil {
		return model.SiteConfig{}, model.SiteSnapshot{}, err
	}
	// One warning per site, and only when the list changes: these
	// interfaces are legitimate (service addresses on a storage host's
	// data interface), so the log records the choice without repeating it
	// on every rebuild.
	if lines := d.addressChoicesToReport(slug, choices); lines != nil {
		slog.Warn("snapshot: interfaces with more than one active address; each machine interface's facts carry one (the device's primary_ip4 if on the interface, else its lowest IPv4 address). To use a different address, make it the device's primary IPv4 in NetBox",
			"config", slug, "netbox_site", nbSite, "count", len(lines), "interfaces", lines)
	}
	return *cfg, snap, nil
}

// locationSubtreePrefixes fetches slagworks-production prefixes scoped to
// every location in the config's claimed subtree (the root location and
// all descendants), so a prefix scoped to a child location isn't dropped.
func (d *Dispatcher) locationSubtreePrefixes(ctx context.Context, scope Scope) ([]netbox.Prefix, error) {
	return d.locationSubtreePrefixesTagged(ctx, scope, "slagworks-production")
}

// locationSubtreePrefixesTagged is locationSubtreePrefixes generalized to
// any prefix tag (slagworks-production for the fabric, slagworks-oob for
// the management network). Results are deduped by prefix id — a prefix can
// only be scoped to one location, but the dedup keeps the function safe if
// NetBox ever returns overlapping pages.
//
// The order is fixed: the config's own location first, then the rest of
// the subtree by ascending location id, each location's prefixes in
// NetBox's order. Build serves Prefixes[0] as the fabric DHCP subnet and
// gateway, so this makes the config's own prefix (the one site-config
// create requires) the primary, and keeps a rebuild over unchanged NetBox
// data from changing the content hash.
func (d *Dispatcher) locationSubtreePrefixesTagged(ctx context.Context, scope Scope, tag string) ([]netbox.Prefix, error) {
	order := []int{scope.LocationID}
	for _, id := range slices.Sorted(maps.Keys(scope.SubtreeIDs)) {
		if id != scope.LocationID {
			order = append(order, id)
		}
	}
	var out []netbox.Prefix
	seen := make(map[int]bool)
	for _, locID := range order {
		page, err := d.nb.ListLocationPrefixes(ctx, locID, tag)
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			if seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out, nil
}
