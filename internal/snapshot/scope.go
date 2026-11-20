package snapshot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// ConfigOwnerStore is the subset of the Store the owning-config resolver
// reads: the site_configs that mirror a given NetBox site.
type ConfigOwnerStore interface {
	ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error)
}

// ConfigOwnerNetBox is the subset of NetBox the owning-config resolver
// needs: the location tree for a site (to resolve each config's scope).
type ConfigOwnerNetBox interface {
	ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error)
}

// ErrNoOwningConfig means no site_config claims the device — either the
// device's NetBox site isn't managed by any worker, or it's a
// location-scoped site and the device sits in an unclaimed location (or
// has no location). The caller can't route a worker RPC for such a device.
var ErrNoOwningConfig = fmt.Errorf("no site_config claims this device")

// ErrAmbiguousOwningConfig means more than one site_config claims the
// device. CheckOverlap refuses such configs when one is created or updated,
// but a later NetBox edit can still produce them: moving one claimed
// location under another on the same site nests the two scopes. The caller
// can't route a worker RPC for such a device until an operator restores the
// location tree or narrows a config.
var ErrAmbiguousOwningConfig = errors.New("more than one site_config claims this device")

// ResolveOwningConfigSlug maps a NetBox device to the slug of the
// site_config (= the site-worker identity) that claims it. This is the
// authoritative "which worker owns this device" resolver: worker RPC
// routing (power/boot/BMC) keys on the config slug because that is how the
// WS hub indexes connected workers, NOT on the device's NetBox site slug
// (those differ for a location-scoped site like pop2-old-cage whose
// netbox_site is pop2). The snapshot builder and reconciler already resolve
// config -> Scope -> ClaimsDevice; this shares that exact predicate so all
// three agree on the device -> config-slug mapping.
//
// The device's NetBox site (dev.Site.Slug) selects the candidate configs;
// each candidate's Scope is resolved against the site's location tree and
// the unique one whose Scope.ClaimsDevice(dev) holds wins. Returns
// ErrNoOwningConfig if none claims it, and ErrAmbiguousOwningConfig if
// several do: picking one would make the answer depend on the order the
// store lists configs in, which Postgres does not fix.
func ResolveOwningConfigSlug(ctx context.Context, nb ConfigOwnerNetBox, st ConfigOwnerStore, dev netbox.Device) (string, error) {
	if dev.Site == nil || dev.Site.Slug == "" {
		return "", fmt.Errorf("device has no NetBox site")
	}
	nbSite := dev.Site.Slug
	configs, err := st.ListSiteConfigsByNetBoxSite(nbSite)
	if err != nil {
		return "", fmt.Errorf("list site_configs for netbox_site %s: %w", nbSite, err)
	}
	if len(configs) == 0 {
		return "", ErrNoOwningConfig
	}

	// One location-tree fetch covers every candidate config (they all share
	// the NetBox site). Only needed to resolve a LOCATION-SCOPED config's
	// subtree, and only meaningful for a device that HAS a location — a
	// location-scoped config can never claim a no-location device
	// (Scope.ClaimsDevice returns false), so skip the fetch entirely for one.
	// That keeps a transient NetBox location-list failure from turning an
	// unlocated device (which can at most be claimed by a whole-site config)
	// into a hard routing error instead of a clean ErrNoOwningConfig.
	var locations []netbox.Location
	needLocations := false
	if dev.Location != nil {
		for i := range configs {
			if configs[i].LocationScoped() {
				needLocations = true
				break
			}
		}
	}
	if needLocations {
		locations, err = nb.ListLocationsForSite(ctx, nbSite)
		if err != nil {
			return "", fmt.Errorf("list locations for netbox_site %s: %w", nbSite, err)
		}
	}

	var claimers []string
	for _, cfg := range configs {
		scope, err := resolveScope(cfg, nbSite, locations)
		if err != nil {
			// A stale config (location gone/re-parented) can't claim; skip it
			// rather than fail the whole resolution — another config may still
			// own the device. (Snapshot/reconcile surface staleness separately.)
			continue
		}
		if scope.ClaimsDevice(dev) {
			claimers = append(claimers, cfg.Slug)
		}
	}
	switch len(claimers) {
	case 0:
		return "", ErrNoOwningConfig
	case 1:
		return claimers[0], nil
	}
	slices.Sort(claimers)
	return "", fmt.Errorf("%w: NetBox device %d is claimed by site_configs %s, whose scopes now overlap (typically a NetBox location was moved under another); move the location back in NetBox, or narrow or delete all but one of these configs",
		ErrAmbiguousOwningConfig, dev.ID, strings.Join(claimers, ", "))
}

// Scope is the resolved NetBox scope of a SiteConfig: which NetBox site
// it mirrors and, when location-scoped, the set of location IDs whose
// devices/prefixes the config claims (the config's location plus every
// descendant — subtree-inclusive). Both the dispatcher (Build inputs)
// and the reconciler (deletion claims) resolve a config to a Scope and
// then use the same membership predicates, so the partition logic lives
// in exactly one place.
type Scope struct {
	SiteSlug string // NetBox site slug (cfg.NetBoxSite)
	// LocationID is the config's own location id, 0 for whole-site.
	LocationID int
	// LocationSlug echoes cfg.NetBoxLocation, "" for whole-site.
	LocationSlug string
	// SubtreeIDs is the set of location IDs in scope (the config's
	// location and all descendants). Nil/empty for whole-site configs,
	// which don't filter by location at all.
	SubtreeIDs map[int]bool
}

// WholeSite reports whether the scope spans the entire NetBox site
// (no location narrowing).
func (s Scope) WholeSite() bool { return s.LocationID == 0 }

// ClaimsDevice reports whether the device is in this scope. Whole-site
// scopes claim every device in the site (the caller has already filtered
// to the site). Location-scoped configs claim a device only if its
// location is in the subtree; a device with NO location set is NOT
// claimed by a location-scoped config (it is "unclaimed" — surfaced, not
// mirrored, never deleted; see ruling 5).
func (s Scope) ClaimsDevice(d netbox.Device) bool {
	if s.WholeSite() {
		return true
	}
	if d.Location == nil {
		return false
	}
	return s.SubtreeIDs[d.Location.ID]
}

// ClaimsPrefix reports whether the prefix is in this scope. Whole-site
// scopes claim prefixes scoped to the site (scope_type dcim.site).
// Location-scoped configs claim prefixes whose scope is a location in
// the subtree; a site-scoped prefix is NOT claimed by a location-scoped
// config (unclaimed — ruling 6).
func (s Scope) ClaimsPrefix(p netbox.Prefix, siteID int) bool {
	if s.WholeSite() {
		return p.ScopeType == "dcim.site" && p.ScopeID == siteID
	}
	return p.ScopeType == "dcim.location" && s.SubtreeIDs[p.ScopeID]
}

// resolveScope builds a Scope for a config from the site's location list.
// It enforces the belongs-to invariant (ruling 1): for a location-scoped
// config the named location must exist in the site's location set, else
// a stale-config error. locations is the full ListLocationsForSite result
// for cfg.NetBoxSite.
func resolveScope(cfg model.SiteConfig, siteSlug string, locations []netbox.Location) (Scope, error) {
	s := Scope{SiteSlug: siteSlug}
	if !cfg.LocationScoped() {
		return s, nil
	}
	var loc *netbox.Location
	for i := range locations {
		if locations[i].Slug == cfg.NetBoxLocation {
			loc = &locations[i]
			break
		}
	}
	if loc == nil {
		// The location either doesn't exist or has been re-parented to a
		// different site (ListLocationsForSite is site-scoped, so a moved
		// location drops out of the result). Either way the config is
		// stale; fail loudly rather than scope to nothing.
		return Scope{}, fmt.Errorf("location %s is not in site %s (stale site_config)",
			cfg.NetBoxLocation, cfg.NetBoxSite)
	}
	s.LocationID = loc.ID
	s.LocationSlug = loc.Slug
	s.SubtreeIDs = subtreeIDs(loc.ID, locations)
	return s, nil
}

// resolveClaimScope is resolveScope for the paths that act on a config's
// claim (the snapshot builder and the reconciler): it also re-checks the
// partition invariant (ruling 2) against the current location tree.
// CheckOverlap runs only when a config is created or updated, so a NetBox
// edit made afterwards can still break the invariant without touching
// site_configs: moving one claimed location under another on the same site
// nests the two scopes, and both configs then claim the inner location's
// devices. Building either config would put those devices in two workers'
// snapshots, so an overlapping config fails here like a stale one until an
// operator restores the location tree or narrows a config. Both configs
// fail, not just the outer one: the move may be the first step of folding
// one config's location into the other's, and only the operator knows which
// config should own the devices.
//
// siblings is the candidate set of other configs; cfg's own row (by slug)
// and configs on other NetBox sites are skipped. A sibling whose scope no
// longer resolves is skipped too: it is stale, claims nothing, and is
// surfaced on its own. (CheckOverlap instead refuses a candidate next to a
// stale config, because an operator is present to fix it; here that would
// let one stale config stop its healthy siblings from building.)
func resolveClaimScope(cfg model.SiteConfig, siblings []model.SiteConfig, locations []netbox.Location) (Scope, error) {
	scope, err := resolveScope(cfg, cfg.NetBoxSite, locations)
	if err != nil {
		return Scope{}, err
	}
	var overlaps []string
	for _, sib := range siblings {
		if sib.Slug == cfg.Slug || sib.NetBoxSite != cfg.NetBoxSite {
			continue
		}
		sibScope, err := resolveScope(sib, sib.NetBoxSite, locations)
		if err != nil {
			continue
		}
		if scopesOverlap(scope, sibScope) {
			overlaps = append(overlaps, fmt.Sprintf("site_config %q (%s)", sib.Slug, scopeName(sibScope)))
		}
	}
	if len(overlaps) > 0 {
		slices.Sort(overlaps)
		return Scope{}, fmt.Errorf("site_config %q (%s) overlaps %s on NetBox site %s: they claim the same devices, so none of their snapshots is built; move the location back in NetBox, or narrow or delete configs until none overlap",
			cfg.Slug, scopeName(scope), strings.Join(overlaps, ", "), cfg.NetBoxSite)
	}
	return scope, nil
}

// scopesOverlap reports whether two scopes on the same NetBox site can claim
// the same device: a whole-site scope claims every device, and two location
// scopes overlap iff either subtree contains the other's root location.
func scopesOverlap(a, b Scope) bool {
	if a.WholeSite() || b.WholeSite() {
		return true
	}
	return a.SubtreeIDs[b.LocationID] || b.SubtreeIDs[a.LocationID]
}

// scopeName describes a scope for an operator-facing error.
func scopeName(s Scope) string {
	if s.WholeSite() {
		return "whole site"
	}
	return "location " + s.LocationSlug
}

// CheckOverlap reports an error if the candidate config would overlap
// any existing config over the same NetBox site, where overlap means
// the two configs could claim the same device (ruling 2, the
// create/update half of the partition invariant; resolveClaimScope
// re-checks it on every snapshot build and reconcile, because a later
// NetBox edit can nest two admitted locations). Cases:
//
//   - Either config is whole-site: a whole-site config claims every
//     device in the site, so it overlaps any other config on that site.
//   - Both location-scoped: overlap iff one location is the same as, or
//     an ancestor/descendant of, the other — because matching is
//     subtree-inclusive, a parent-location config and a child-location
//     config both claim the child's devices. Exact-slug equality is NOT
//     sufficient; this walks the tree.
//
// existing should be all current site_configs; the candidate's own row
// (matched by slug) is skipped so update is idempotent. locations is the
// candidate site's ListLocationsForSite result. The candidate's location
// (when scoped) must already have been confirmed present in locations by
// resolveScope; CheckOverlap re-resolves both sides defensively.
func CheckOverlap(candidate model.SiteConfig, existing []model.SiteConfig, locations []netbox.Location) error {
	candScope, err := resolveScope(candidate, candidate.NetBoxSite, locations)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.Slug == candidate.Slug {
			continue // same row (update)
		}
		if e.NetBoxSite != candidate.NetBoxSite {
			continue // different NetBox site, can't overlap
		}
		// Whole-site on either side overlaps everything on the site.
		if candScope.WholeSite() || !e.LocationScoped() {
			return fmt.Errorf("config %q overlaps existing config %q: a whole-site config and any other config on site %s claim the same devices",
				candidate.Slug, e.Slug, candidate.NetBoxSite)
		}
		eScope, err := resolveScope(e, e.NetBoxSite, locations)
		if err != nil {
			// An existing config on this site no longer resolves (its
			// location is gone / re-parented). Overlap against an
			// unresolvable scope can't be computed, so fail closed: block
			// the candidate and tell the operator to fix the stale config
			// first. Conservative by design — better than admitting a
			// config that might double-mirror once the stale one is fixed.
			return fmt.Errorf("existing config %q on site %s is stale and must be fixed first: %w", e.Slug, e.NetBoxSite, err)
		}
		// Both location-scoped: overlap iff either subtree contains the
		// other's root location.
		if scopesOverlap(candScope, eScope) {
			return fmt.Errorf("config %q (location %s) overlaps existing config %q (location %s): one location is the same as or nested within the other",
				candidate.Slug, candidate.NetBoxLocation, e.Slug, e.NetBoxLocation)
		}
	}
	return nil
}

// subtreeIDs returns the set of location IDs rooted at rootID: the root
// plus every transitive descendant, computed from the parent links in
// locations. Cycle-safe (NetBox enforces a tree, but a defensive visited
// set costs nothing).
func subtreeIDs(rootID int, locations []netbox.Location) map[int]bool {
	children := make(map[int][]int, len(locations))
	for _, l := range locations {
		if l.Parent != nil {
			children[l.Parent.ID] = append(children[l.Parent.ID], l.ID)
		}
	}
	out := make(map[int]bool)
	var walk func(id int)
	walk = func(id int) {
		if out[id] {
			return
		}
		out[id] = true
		for _, c := range children[id] {
			walk(c)
		}
	}
	walk(rootID)
	return out
}
