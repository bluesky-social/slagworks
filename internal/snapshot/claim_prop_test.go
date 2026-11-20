package snapshot

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Property tests for site-config claim rules over one NetBox site.
//
// Oracle (independent of scope.go): a config's claim set is computed by
// walking parent links upward from every location (an ancestor walk, where
// production walks child lists downward), plus a pseudo-location 0 for a
// device with no location. Whole-site claims everything; a location-scoped
// config claims the locations whose ancestor chain (inclusive) contains its
// location. Two configs overlap iff their claim sets intersect.
//
// Properties:
//   - CheckOverlap admits a candidate iff the oracle says it overlaps no
//     admitted config (soundness and completeness of the create/update rule);
//   - after admission, ClaimingConfigs and ResolveOwningConfigSlug agree with
//     the oracle for a device in every location (the claim is computed
//     identically everywhere), and resolveClaimScope accepts every admitted
//     config;
//   - after a NetBox location re-parent, the one mutation that changes claim
//     sets without touching site_configs, the claim computation still agrees
//     with the oracle, a device several configs claim has no owner whatever
//     order the store lists the configs in, and resolveClaimScope (the
//     snapshot builder's and reconciler's gate) refuses exactly the configs
//     that are stale or overlap another, so no device is claimed by more
//     than one config that still builds.

type claimWorld struct {
	locs    []netbox.Location
	configs []model.SiteConfig
}

const claimSite = "pop9"

func drawLocTree(t *rapid.T) []netbox.Location {
	n := rapid.IntRange(1, 7).Draw(t, "nLocs")
	locs := make([]netbox.Location, n)
	for i := range n {
		locs[i] = netbox.Location{ID: i + 1, Slug: fmt.Sprintf("l%d", i+1), Site: &netbox.NestedRef{Slug: claimSite}}
		if i > 0 {
			if p := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("parent%d", i)); p > 0 {
				locs[i].Parent = &netbox.NestedRef{ID: p}
			}
		}
	}
	return locs
}

func parentOf(locs []netbox.Location, id int) int {
	for _, l := range locs {
		if l.ID == id && l.Parent != nil {
			return l.Parent.ID
		}
	}
	return 0
}

func locIDBySlug(locs []netbox.Location, slug string) int {
	for _, l := range locs {
		if l.Slug == slug {
			return l.ID
		}
	}
	return -1
}

// oracleClaims returns the set of device locations (0 = no location) the
// config claims, or nil when the config is stale (location not in site).
func oracleClaims(locs []netbox.Location, c model.SiteConfig) map[int]bool {
	out := map[int]bool{}
	if c.NetBoxLocation == "" {
		out[0] = true
		for _, l := range locs {
			out[l.ID] = true
		}
		return out
	}
	root := locIDBySlug(locs, c.NetBoxLocation)
	if root < 0 {
		return nil
	}
	for _, l := range locs {
		for cur, steps := l.ID, 0; cur != 0 && steps <= len(locs); cur, steps = parentOf(locs, cur), steps+1 {
			if cur == root {
				out[l.ID] = true
				break
			}
		}
	}
	return out
}

func oracleOverlap(locs []netbox.Location, a, b model.SiteConfig) bool {
	ca, cb := oracleClaims(locs, a), oracleClaims(locs, b)
	for k := range ca {
		if cb[k] {
			return true
		}
	}
	return false
}

func drawCandidate(t *rapid.T, i int, locs []netbox.Location) model.SiteConfig {
	c := model.SiteConfig{Slug: fmt.Sprintf("cfg%d", i), NetBoxSite: claimSite}
	if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("whole%d", i)) > 0 {
		c.NetBoxLocation = fmt.Sprintf("l%d", rapid.IntRange(1, len(locs)).Draw(t, fmt.Sprintf("loc%d", i)))
	}
	return c
}

// admit replays the API's create path: each candidate is checked against
// the configs admitted so far.
func admit(t *rapid.T, locs []netbox.Location) claimWorld {
	w := claimWorld{locs: locs}
	n := rapid.IntRange(1, 5).Draw(t, "nCands")
	for i := range n {
		cand := drawCandidate(t, i, locs)
		err := CheckOverlap(cand, w.configs, locs)
		wantOverlap := false
		for _, e := range w.configs {
			if oracleOverlap(locs, cand, e) {
				wantOverlap = true
			}
		}
		if (err != nil) != wantOverlap {
			t.Fatalf("CheckOverlap(%+v) = %v, oracle overlap=%v against %+v (tree %v)", cand, err, wantOverlap, w.configs, treeString(locs))
		}
		if err == nil {
			w.configs = append(w.configs, cand)
		}
	}
	return w
}

func treeString(locs []netbox.Location) string {
	s := ""
	for _, l := range locs {
		s += fmt.Sprintf("%d<-%d ", l.ID, parentOf(locs, l.ID))
	}
	return s
}

// deviceLocs is every place a device can sit: each location, and 0 for no
// location.
func deviceLocs(w claimWorld) []int {
	out := []int{0}
	for _, l := range w.locs {
		out = append(out, l.ID)
	}
	return out
}

func deviceAt(loc int) netbox.Device {
	d := netbox.Device{ID: 1000 + loc, Site: &netbox.NestedRef{Slug: claimSite}}
	if loc != 0 {
		d.Location = &netbox.NestedRef{ID: loc}
	}
	return d
}

// checkAgreement compares production with the oracle for a device in every
// location and returns the largest number of configs claiming one device. A
// device claimed by several configs must have no owner, in the store's order
// and in reverse.
func checkAgreement(t *rapid.T, w claimWorld) (maxClaimers int) {
	st := &fakeClaimConfigLister{configs: w.configs}
	nb := &fakeNetBox{locations: w.locs}
	r := NewClaimResolver(nb, st)
	ctx := context.Background()
	for _, loc := range deviceLocs(w) {
		d := deviceAt(loc)
		var want []string
		for _, c := range w.configs {
			if oracleClaims(w.locs, c)[loc] {
				want = append(want, c.Slug)
			}
		}
		got, err := r.ClaimingConfigs(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
			t.Fatalf("ClaimingConfigs(loc %d) = %v, oracle %v (tree %v configs %+v)", loc, got, want, treeString(w.locs), w.configs)
		}
		if len(want) > maxClaimers {
			maxClaimers = len(want)
		}
		owner, err := ResolveOwningConfigSlug(ctx, nb, st, d)
		switch {
		case len(want) == 0:
			if !errors.Is(err, ErrNoOwningConfig) {
				t.Fatalf("ResolveOwningConfigSlug(loc %d) = %q, %v; oracle: unclaimed", loc, owner, err)
			}
		case len(want) == 1:
			if err != nil || owner != want[0] {
				t.Fatalf("ResolveOwningConfigSlug(loc %d) = %q, %v; oracle %v", loc, owner, err, want)
			}
		default:
			// More than one claimer: picking one would make the owner
			// depend on the order the store happens to return rows in, so
			// both orders must refuse.
			rev := append([]model.SiteConfig(nil), w.configs...)
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			owner2, err2 := ResolveOwningConfigSlug(ctx, nb, &fakeClaimConfigLister{configs: rev}, d)
			if !errors.Is(err, ErrAmbiguousOwningConfig) || !errors.Is(err2, ErrAmbiguousOwningConfig) {
				t.Fatalf("device at loc %d claimed by %v: want ErrAmbiguousOwningConfig in both store orders, got (%q, %v) in store order and (%q, %v) reversed",
					loc, want, owner, err, owner2, err2)
			}
		}
	}
	return maxClaimers
}

// checkBuildGate compares resolveClaimScope, which the snapshot builder and
// the reconciler gate every config on, with the oracle: a config is refused
// iff it is stale or its claim set meets that of another config that is not
// stale. It then requires that no device is claimed by two configs the gate
// accepts, and returns how many configs it refused.
func checkBuildGate(t *rapid.T, w claimWorld) (refused int) {
	accepted := map[string]bool{}
	for _, c := range w.configs {
		wantRefused := oracleClaims(w.locs, c) == nil
		for _, e := range w.configs {
			if e.Slug != c.Slug && oracleClaims(w.locs, e) != nil && oracleOverlap(w.locs, c, e) {
				wantRefused = true
			}
		}
		_, err := resolveClaimScope(c, w.configs, w.locs)
		if (err != nil) != wantRefused {
			t.Fatalf("resolveClaimScope(%+v) = %v, oracle refused=%v (tree %v configs %+v)", c, err, wantRefused, treeString(w.locs), w.configs)
		}
		if err != nil {
			refused++
		} else {
			accepted[c.Slug] = true
		}
	}
	for _, loc := range deviceLocs(w) {
		var builders []string
		for _, c := range w.configs {
			if accepted[c.Slug] && oracleClaims(w.locs, c)[loc] {
				builders = append(builders, c.Slug)
			}
		}
		if len(builders) > 1 {
			t.Fatalf("a device at loc %d would be in the snapshots of %v (tree %v configs %+v)", loc, builders, treeString(w.locs), w.configs)
		}
	}
	return refused
}

func TestPropCheckOverlapMatchesOracleAndClaimsAgree(t *testing.T) {
	var admitted, multi int
	rapid.Check(t, func(t *rapid.T) {
		w := admit(t, drawLocTree(t))
		admitted += len(w.configs)
		if checkAgreement(t, w) > 1 {
			multi++
			t.Fatalf("admitted configs overlap: %+v tree %v", w.configs, treeString(w.locs))
		}
		if n := checkBuildGate(t, w); n > 0 {
			t.Fatalf("resolveClaimScope refused %d admitted config(s): %+v tree %v", n, w.configs, treeString(w.locs))
		}
	})
	t.Logf("configs admitted=%d multi-claim worlds=%d", admitted, multi)
}

// TestPropClaimsAgreeAfterReparent: re-parent one location in NetBox
// (keeping the tree acyclic), leave site_configs untouched, and require
// ClaimingConfigs and ownership to match the oracle on the new tree.
// Nesting one configured location under another makes two admitted configs
// claim the same devices without anything re-running CheckOverlap: such a
// device must have no owner, and the build gate must refuse both configs.
func TestPropClaimsAgreeAfterReparent(t *testing.T) {
	var multi, refused int
	rapid.Check(t, func(t *rapid.T) {
		w := admit(t, drawLocTree(t))
		if len(w.locs) < 2 {
			return
		}
		mv := rapid.IntRange(1, len(w.locs)).Draw(t, "move")
		// New parent: any location that is not mv or one of its descendants.
		var choices []int
		for _, l := range w.locs {
			desc := false
			for cur, steps := l.ID, 0; cur != 0 && steps <= len(w.locs); cur, steps = parentOf(w.locs, cur), steps+1 {
				if cur == mv {
					desc = true
					break
				}
			}
			if !desc {
				choices = append(choices, l.ID)
			}
		}
		choices = append(choices, 0)
		np := rapid.SampledFrom(choices).Draw(t, "newParent")
		locs := append([]netbox.Location(nil), w.locs...)
		for i := range locs {
			if locs[i].ID == mv {
				if np == 0 {
					locs[i].Parent = nil
				} else {
					locs[i].Parent = &netbox.NestedRef{ID: np}
				}
			}
		}
		w.locs = locs
		if checkAgreement(t, w) > 1 {
			multi++
		}
		refused += checkBuildGate(t, w)
	})
	t.Logf("worlds with a device claimed by several configs after the re-parent: %d; configs refused by the build gate: %d", multi, refused)
}
