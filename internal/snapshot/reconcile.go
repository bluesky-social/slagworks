package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// MirrorOps is the subset of *mirror.Mirror the reconciler drives.
type MirrorOps interface {
	OnDeviceCreated(ctx context.Context, d mirror.Device) error
	OnDeviceUpdated(ctx context.Context, d mirror.Device) error
	OnDeviceDeleted(ctx context.Context, deviceID int, siteSlug string) error
}

// ReconcileStore is the subset of the Store the reconciler reads.
type ReconcileStore interface {
	ListSiteConfigs() ([]model.SiteConfig, error)
	ListMachines(state *model.MachineState) ([]model.Machine, error)
	ListMachinesByDeviceIDs(deviceIDs []int) ([]model.Machine, error)
}

// Reconciler is the webhook-loss safety net: every interval it walks
// each site_config, pulls NetBox Devices for the site, feeds them
// through the mirror (catching lost webhooks), and invalidates the
// site to trigger a fresh snapshot rebuild.
//
// Missing Devices get confirmed-gone via GetDeviceByID before we
// soft-decommission — a Device that moved to another site is owned
// by that site's reconcile pass, not ours.
type Reconciler struct {
	nb       NetBoxReader
	st       ReconcileStore
	inv      InvalidatorFunc
	mir      MirrorOps
	interval time.Duration
	log      *slog.Logger
	metrics  *Metrics // optional; nil disables Prometheus instrumentation

	mu    sync.Mutex
	stale map[string]string // slug -> reason

	firstDone atomic.Bool
}

// InvalidatorFunc lets the reconciler accept either the concrete
// *Registry (production) or a fake (tests). Matches the mirror's
// Invalidator contract; the concrete *Registry satisfies it.
type InvalidatorFunc interface {
	Invalidate(slug string)
}

func NewReconciler(nb NetBoxReader, st ReconcileStore, inv InvalidatorFunc, mir MirrorOps) *Reconciler {
	return &Reconciler{
		nb:       nb,
		st:       st,
		inv:      inv,
		mir:      mir,
		interval: 5 * time.Minute,
		log:      slog.Default(),
		stale:    make(map[string]string),
	}
}

// WithInterval overrides the reconcile interval (tests).
func (r *Reconciler) WithInterval(d time.Duration) *Reconciler { r.interval = d; return r }

func (r *Reconciler) SetLogger(log *slog.Logger) { r.log = log }

// SetMetrics wires a Metrics bundle. The reconciler publishes
// site_stale gauge transitions; nil disables instrumentation.
func (r *Reconciler) SetMetrics(m *Metrics) { r.metrics = m }

// Run blocks until ctx is cancelled. Fires an immediate tick on start.
func (r *Reconciler) Run(ctx context.Context) {
	r.tick(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// ManualTick runs a single reconcile pass out-of-band. Used by tests
// and by the `slagctl netbox refresh` admin command.
func (r *Reconciler) ManualTick(ctx context.Context) { r.tick(ctx) }

// IsStale reports whether the given slug is currently flagged stale.
func (r *Reconciler) IsStale(slug string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.stale[slug]
	return ok
}

// StaleSites returns a copy of the stale map (slug -> reason) for
// /healthz and slagctl reporting.
func (r *Reconciler) StaleSites() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.stale))
	maps.Copy(out, r.stale)
	return out
}

// FirstReconcileComplete reports whether at least one full tick has
// processed every non-stale site to completion. Used by /healthz
// readiness: empty-config startup returns true immediately; if every
// site is stale, we stay not-ready.
func (r *Reconciler) FirstReconcileComplete() bool { return r.firstDone.Load() }

func (r *Reconciler) tick(ctx context.Context) {
	cfgs, err := r.st.ListSiteConfigs()
	if err != nil {
		r.log.Error("list site_configs", "err", err)
		return
	}

	// Phase 1: per-site ListDevicesForSite + OnDeviceUpdated, collecting
	// the union of device IDs seen anywhere. Missing-device detection is
	// global (phase 2) because machines don't carry a site slug on the
	// row — doing the check per-site would call GetDeviceByID N*S times
	// for machines outside the current site.
	anyProcessed := false
	allSeen := make(map[int]bool)
	type siteCtx struct {
		slug string
	}
	var processed []siteCtx
	for _, cfg := range cfgs {
		seen, ok := r.reconcileSiteDevices(ctx, cfg)
		if !ok {
			continue
		}
		anyProcessed = true
		processed = append(processed, siteCtx{slug: cfg.Slug})
		for id := range seen {
			allSeen[id] = true
		}
	}

	// Phase 2: walk machines once, soft-decommission those whose
	// NetBoxDeviceID isn't in any processed site's seen set AND
	// NetBox confirms the device is truly gone. The per-device 404
	// from GetDeviceByID is what makes a delete safe; allSeen only
	// saves lookups. So a config that failed this tick (stale, or a
	// NetBox blip) does not gate the sweep: its machines are missing
	// from allSeen, but their devices still resolve (skip) or the
	// lookup errors (skip). Gating on every config succeeding would
	// let one stale config, which can stay stale indefinitely, stop
	// fleet-wide cleanup of devices deleted while a webhook was lost.
	//
	// The sweep still needs at least one config to have reconciled.
	// When none did, NetBox may not be the instance we think it is
	// (repointed at an empty or wrong instance, every managed site
	// missing), and there every lookup 404s and the sweep would delete
	// every machine.
	if anyProcessed {
		r.sweepMissingDevices(ctx, allSeen)
	} else if len(cfgs) > 0 {
		r.log.Warn("reconcile: no site_config reconciled this tick; skipping the missing-device sweep",
			"configs", len(cfgs))
	}

	for _, p := range processed {
		r.inv.Invalidate(p.slug)
	}

	if anyProcessed || len(cfgs) == 0 {
		r.firstDone.Store(true)
	}
}

// reconcileSiteDevices returns the set of in-scope NetBox device IDs
// for the given config, or (nil, false) if the site is stale / the
// NetBox call failed. Staleness is keyed on the config slug (the worker
// identity), but all NetBox lookups key on cfg.NetBoxSite. Devices are
// scoped to the config's claim (location subtree for a location-scoped
// config) BEFORE mirroring, so a dual-cage site's two configs each
// mirror only their own cage's devices — no cross-cage double-mirror.
func (r *Reconciler) reconcileSiteDevices(ctx context.Context, cfg model.SiteConfig) (map[int]bool, bool) {
	slug := cfg.Slug
	nbSite := cfg.NetBoxSite
	siteRef, err := r.nb.GetSiteBySlug(ctx, nbSite)
	if err != nil {
		r.log.Error("netbox site lookup", "config", slug, "netbox_site", nbSite, "err", err)
		r.markStale(slug, err.Error())
		return nil, false
	}
	if siteRef == nil {
		r.markStale(slug, fmt.Sprintf("netbox site %s not found", nbSite))
		return nil, false
	}

	// Resolve scope (and enforce the belongs-to invariant). A
	// location-scoped config whose location is gone / re-parented is
	// stale, not silently empty.
	locations, err := r.nb.ListLocationsForSite(ctx, nbSite)
	if err != nil {
		r.log.Error("list locations", "config", slug, "netbox_site", nbSite, "err", err)
		return nil, false
	}
	scope, err := resolveScope(cfg, nbSite, locations)
	if err != nil {
		r.markStale(slug, err.Error())
		return nil, false
	}
	r.clearStale(slug)

	allDevices, err := r.nb.ListDevicesForSite(ctx, nbSite)
	if err != nil {
		r.log.Error("list devices", "config", slug, "netbox_site", nbSite, "err", err)
		return nil, false
	}
	devices := make([]netbox.Device, 0, len(allDevices))
	for _, d := range allDevices {
		if scope.ClaimsDevice(d) {
			devices = append(devices, d)
		}
	}

	// Build the set of device IDs we already have machines for. Devices
	// NetBox knows about but we don't (e.g. existed before the site was
	// managed, or a create webhook was lost) need OnDeviceCreated so the
	// reconciler can backfill. Devices we already track get
	// OnDeviceUpdated — the webhook-loss equivalent of a bare "touch".
	deviceIDs := make([]int, len(devices))
	for i, d := range devices {
		deviceIDs[i] = d.ID
	}
	tracked := make(map[int]bool)
	if len(deviceIDs) > 0 {
		machines, err := r.st.ListMachinesByDeviceIDs(deviceIDs)
		if err != nil {
			r.log.Error("list machines by device ids", "slug", slug, "err", err)
			return nil, false
		}
		for _, m := range machines {
			if m.NetBoxDeviceID != nil {
				tracked[*m.NetBoxDeviceID] = true
			}
		}
	}

	seen := make(map[int]bool, len(devices))
	for _, d := range devices {
		seen[d.ID] = true
		md := mirrorDeviceFromNetBox(d, slug)
		if tracked[d.ID] {
			if err := r.mir.OnDeviceUpdated(ctx, md); err != nil {
				r.log.Warn("reconcile on-device-updated", "device_id", d.ID, "err", err)
			}
		} else {
			if err := r.mir.OnDeviceCreated(ctx, md); err != nil {
				// Serial-required is operator-fixable; log at info so it
				// doesn't drown the log on first bringup of a site that
				// hasn't had serials populated yet.
				if errors.Is(err, mirror.ErrSerialRequired) {
					r.log.Info("reconcile: device missing serial, skipping",
						"device_id", d.ID, "name", d.Name)
				} else {
					r.log.Warn("reconcile on-device-created", "device_id", d.ID, "err", err)
				}
			}
		}
	}
	return seen, true
}

// sweepMissingDevices walks all live machines and soft-decommissions
// any whose NetBoxDeviceID is absent from the merged in-scope seen set
// AND confirmed gone via GetDeviceByID. One GetDeviceByID per
// genuinely-missing machine — not per machine-per-site.
//
// Deletion is claim-based and conservative: a machine is decommissioned
// only when NetBox returns 404 for its device (the device is gone from
// NetBox entirely). A device that still exists anywhere — in another
// cage's location, in a location no config claims, with no location set,
// or in a site we don't manage — is left untouched. This is what makes
// the partition safe: in a dual-cage site, cage A's machine is in
// allSeen (cage A's config saw it) so it's skipped; even if it somehow
// weren't, GetDeviceByID would find it and skip it. The sweep cannot
// eat the other cage.
func (r *Reconciler) sweepMissingDevices(ctx context.Context, allSeen map[int]bool) {
	machines, _ := r.st.ListMachines(nil)
	for _, m := range machines {
		if m.NetBoxDeviceID == nil {
			continue
		}
		if allSeen[*m.NetBoxDeviceID] {
			continue
		}
		dev, lookupErr := r.nb.GetDeviceByID(ctx, *m.NetBoxDeviceID)
		if lookupErr != nil {
			r.log.Warn("reconcile device lookup", "device_id", *m.NetBoxDeviceID, "err", lookupErr)
			continue
		}
		if dev != nil {
			// Device still exists in NetBox — just not in any config's
			// claimed scope this tick (another cage, an unclaimed
			// location, no location set, or an unmanaged site). Not ours
			// to delete; leave it.
			continue
		}
		// Empty config slug: the sweep can't cheaply resolve which
		// config claimed a now-gone device, and it doesn't need to —
		// tick() invalidates every processed config at the end of the
		// pass regardless, so the snapshot still refreshes. The empty
		// key is a deliberate no-op invalidate (see Invalidator.Invalidate).
		if err := r.mir.OnDeviceDeleted(ctx, *m.NetBoxDeviceID, ""); err != nil {
			r.log.Warn("reconcile soft-decommission", "device_id", *m.NetBoxDeviceID, "err", err)
		}
	}
}

// mirrorDeviceFromNetBox adapts a netbox.Device to the slim
// mirror.Device shape. configSlug is the worker-identity slug of the
// config claiming this device (the reconciler already scoped it before
// calling); it becomes the mirror's invalidation key.
func mirrorDeviceFromNetBox(d netbox.Device, configSlug string) mirror.Device {
	out := mirror.Device{ID: d.ID, Name: d.Name, Serial: d.Serial, ConfigSlug: configSlug}
	if d.Site != nil {
		out.SiteSlug = d.Site.Slug
	}
	return out
}

func (r *Reconciler) markStale(slug, reason string) {
	// Metric update is performed under r.mu so concurrent ManualTick +
	// Run passes for the same slug can't interleave (map-set, then
	// another caller's map-delete + gauge-clear, then our gauge-set
	// would leave gauge=1 on a slug no longer in the map).
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stale[slug] = reason
	if r.metrics != nil {
		r.metrics.SiteStale.WithLabelValues(slug).Set(1)
	}
}

func (r *Reconciler) clearStale(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stale, slug)
	if r.metrics != nil {
		r.metrics.SiteStale.WithLabelValues(slug).Set(0)
	}
}
