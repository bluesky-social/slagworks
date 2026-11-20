package snapshot

import (
	"context"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// claimConfigLister is the subset of the store the ClaimResolver reads:
// the candidate configs for a NetBox site. Keyed on netbox_site (not the
// PK), because a device only tells us its site/location, and several
// configs (dual-cage) can share one netbox_site.
type claimConfigLister interface {
	ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error)
}

// claimLocationReader is the subset of the NetBox client the resolver
// needs: the site's location tree, to resolve a location-scoped config's
// subtree. Satisfied by NetBoxReader (and the same fakes).
type claimLocationReader interface {
	ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error)
}

// ClaimResolver answers "which site_config(s) claim this device" — site
// AND location-subtree — reusing the exact resolveScope + Scope.ClaimsDevice
// predicates the reconciler uses. It is the single membership predicate the
// location-scoping design intended: the reconciler scopes inline (it already
// holds the cfg it iterates), and the webhook + commission paths, which see a
// raw device with no pre-scoping, route through here.
//
// CheckOverlap keeps claims over a shared netbox_site disjoint when configs
// are created or updated, but a later NetBox edit that moves one claimed
// location under another leaves a device claimed by both configs until an
// operator repairs it. The resolver then returns every claimer, so callers
// that invalidate or mirror reach both; the snapshot builder and reconciler
// refuse such configs (resolveClaimScope), so neither config's snapshot is
// rebuilt meanwhile. The empty result is the "claimed by none" case
// (unmanaged site, or an unclaimed location of a managed site).
type ClaimResolver struct {
	nb claimLocationReader
	st claimConfigLister
}

// NewClaimResolver wires a resolver. nb need only satisfy
// ListLocationsForSite; the production *netbox.Client and the dispatcher's
// NetBoxReader both do.
func NewClaimResolver(nb claimLocationReader, st claimConfigLister) *ClaimResolver {
	return &ClaimResolver{nb: nb, st: st}
}

// ClaimingConfigs returns the slugs of the site_configs whose scope claims
// the device. Empty (not an error) when the device's site is unmanaged or
// its location is outside every config's claim. A device with no site set is
// claimed by nothing.
//
// An error is returned only on a real lookup failure (store or NetBox); a
// stale config whose location no longer resolves is skipped (it can't claim
// anything coherently and the reconciler will surface its staleness), not
// fatal to evaluating its siblings.
func (r *ClaimResolver) ClaimingConfigs(ctx context.Context, d netbox.Device) ([]string, error) {
	if d.Site == nil || d.Site.Slug == "" {
		return nil, nil
	}
	cfgs, err := r.st.ListSiteConfigsByNetBoxSite(d.Site.Slug)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return nil, nil
	}
	// All candidate configs share this netbox_site, so the location tree is
	// fetched once and reused across them.
	locations, err := r.nb.ListLocationsForSite(ctx, d.Site.Slug)
	if err != nil {
		return nil, err
	}
	var claimed []string
	for _, cfg := range cfgs {
		scope, err := resolveScope(cfg, cfg.NetBoxSite, locations)
		if err != nil {
			// Stale config (its location is gone / re-parented). It claims
			// nothing coherently; skip it rather than failing the whole
			// resolution. The reconciler marks such configs stale loudly.
			continue
		}
		if scope.ClaimsDevice(d) {
			claimed = append(claimed, cfg.Slug)
		}
	}
	return claimed, nil
}
