package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"gorm.io/gorm"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Model-based test for the site_configs store.
//
// Reference model: slug -> SiteConfig (the fields an operator owns) plus a
// separately tracked (seq, hash) pair that only BumpSiteSnapshotMeta may
// change. The snapshot Registry doc (internal/snapshot/registry.go) makes the
// per-slug actor the sole owner of last_snapshot_seq: "no code outside this
// package can bump a snapshot SeqNum except through an actor", and
// Cache.ApplySnapshot drops any snapshot whose SeqNum is not greater than
// the one it holds. So the invariants checked here are: a row's
// last_snapshot_seq and hash change only through BumpSiteSnapshotMeta, and
// every bump returns a SeqNum above every SeqNum issued before it, for any
// slug, including one issued to a since-deleted row of the same slug (a
// worker keeps its persisted SeqNum across a delete and re-create of its
// site config).
//
// Operations interleave the API's read-modify-write update (GetSiteConfig,
// mutate operator fields, UpdateSiteConfig) with bumps, so that an update
// built from a read taken before a bump is common, not vanishingly rare.

type refSiteCfg struct {
	cfg  model.SiteConfig // operator-owned fields
	seq  int64
	hash string
}

func drawValidSiteConfig(t *rapid.T, slug string) model.SiteConfig {
	c := model.SiteConfig{
		Slug:               slug,
		NetBoxSite:         rapid.SampledFrom([]string{"pop1", "pop2", "pöp-ü"}).Draw(t, "nbSite"),
		NetBoxLocation:     rapid.SampledFrom([]string{"", "cage-a", "cage-b"}).Draw(t, "nbLoc"),
		EphemeralPoolStart: "10.0.0." + fmt.Sprint(rapid.IntRange(1, 100).Draw(t, "ps")),
		EphemeralPoolEnd:   "10.0.0." + fmt.Sprint(rapid.IntRange(100, 254).Draw(t, "pe")),
		Promiscuous:        rapid.Bool().Draw(t, "promisc"),
	}
	switch rapid.IntRange(0, 2).Draw(t, "poolsShape") {
	case 0:
		c.RelayPools = nil
	case 1:
		c.RelayPools = []model.RelayPool{}
	default:
		n := rapid.IntRange(1, 3).Draw(t, "nPools")
		for i := range n {
			c.RelayPools = append(c.RelayPools, model.RelayPool{
				Subnet:     fmt.Sprintf("10.%d.0.0/24", i+1),
				RangeStart: fmt.Sprintf("10.%d.0.100", i+1),
				RangeEnd:   fmt.Sprintf("10.%d.0.200", i+1),
				Gateway:    fmt.Sprintf("10.%d.0.1", i+1),
			})
		}
	}
	return c
}

// operatorFields strips the slagworks-maintained columns so the model and
// the store can be compared on what an operator controls. Empty and nil
// relay pools are treated as the same operator intent.
func operatorFields(c model.SiteConfig) model.SiteConfig {
	c.LastSnapshotSeq, c.LastSnapshotHash = 0, ""
	c.CreatedAt = c.CreatedAt.UTC().Truncate(0)
	c.CreatedAt = c.CreatedAt.Truncate(1e3)
	if len(c.RelayPools) == 0 {
		c.RelayPools = nil
	}
	return c
}

type siteCfgStats struct {
	creates, updates, staleUpdates, bumps, deletes, gets, updateAfterDelete int
}

func TestPropSiteConfigStoreMatchesModel(t *testing.T) {
	s := testStore(t)
	var st siteCfgStats
	// Highest SeqNum any bump has returned; it outlives the per-history
	// cleanup, as the database sequence does.
	var maxSeq int64
	rapid.Check(t, func(rt *rapid.T) {
		if err := s.db.Exec("DELETE FROM site_configs").Error; err != nil {
			rt.Fatal(err)
		}
		slugs := []string{"pop1", "pop2-old-cage", "ü-site"}
		ref := map[string]*refSiteCfg{}
		// reads holds GetSiteConfig results the API may later write back
		// (the read half of updateSiteConfig's read-modify-write).
		reads := map[string]*model.SiteConfig{}
		n := rapid.IntRange(1, 30).Draw(rt, "nOps")
		for i := range n {
			slug := rapid.SampledFrom(slugs).Draw(rt, "slug")
			switch op := rapid.IntRange(0, 9).Draw(rt, fmt.Sprintf("op%d", i)); {
			case op <= 1: // create
				c := drawValidSiteConfig(rt, slug)
				err := s.CreateSiteConfig(&c)
				if _, ok := ref[slug]; ok {
					if err == nil {
						rt.Fatalf("duplicate create of %q succeeded", slug)
					}
					continue
				}
				if err != nil {
					rt.Fatalf("create %q: %v", slug, err)
				}
				st.creates++
				c.CreatedAt = c.CreatedAt.Truncate(1e3)
				ref[slug] = &refSiteCfg{cfg: c}
			case op <= 3: // read (the API's GetSiteConfig before a PATCH)
				got, err := s.GetSiteConfig(slug)
				r, ok := ref[slug]
				if !ok {
					if !errors.Is(err, gorm.ErrRecordNotFound) {
						rt.Fatalf("get absent %q: %v", slug, err)
					}
					continue
				}
				if err != nil {
					rt.Fatalf("get %q: %v", slug, err)
				}
				st.gets++
				if got.LastSnapshotSeq != r.seq || got.LastSnapshotHash != r.hash {
					rt.Fatalf("%q snapshot meta = (%d,%q), model (%d,%q)", slug, got.LastSnapshotSeq, got.LastSnapshotHash, r.seq, r.hash)
				}
				if !reflect.DeepEqual(operatorFields(*got), operatorFields(r.cfg)) {
					rt.Fatalf("%q fields:\n got %+v\nwant %+v", slug, operatorFields(*got), operatorFields(r.cfg))
				}
				reads[slug] = got
			case op <= 5: // write back a previous read with an operator edit
				got, ok := reads[slug]
				if !ok {
					continue
				}
				cp := *got
				cp.Promiscuous = !cp.Promiscuous
				cp.EphemeralPoolEnd = "10.0.0." + fmt.Sprint(rapid.IntRange(200, 254).Draw(rt, "newEnd"))
				r, live := ref[slug]
				if live && (r.seq != got.LastSnapshotSeq || r.hash != got.LastSnapshotHash) {
					st.staleUpdates++
				}
				err := s.UpdateSiteConfig(&cp)
				if !live {
					st.updateAfterDelete++
					if err == nil {
						if _, gerr := s.GetSiteConfig(slug); gerr == nil {
							rt.Fatalf("update of deleted %q recreated the row", slug)
						}
					}
					continue
				}
				if err != nil {
					rt.Fatalf("update %q: %v", slug, err)
				}
				st.updates++
				// The write-back carries every operator field of the read it
				// was built from (PATCH is read-modify-write without a
				// version check), so the row's operator fields become cp's.
				r.cfg.Promiscuous = cp.Promiscuous
				r.cfg.EphemeralPoolStart, r.cfg.EphemeralPoolEnd = cp.EphemeralPoolStart, cp.EphemeralPoolEnd
				r.cfg.NetBoxSite, r.cfg.NetBoxLocation, r.cfg.RelayPools = cp.NetBoxSite, cp.NetBoxLocation, cp.RelayPools
				reads[slug] = &cp
			case op <= 7: // snapshot actor bump
				hash := fmt.Sprintf("h%d", i)
				seq, err := s.BumpSiteSnapshotMeta(slug, hash)
				r, ok := ref[slug]
				if !ok {
					if err == nil && seq != 0 {
						rt.Fatalf("bump of absent %q returned seq %d", slug, seq)
					}
					continue
				}
				if err != nil {
					rt.Fatalf("bump %q: %v", slug, err)
				}
				st.bumps++
				if seq <= maxSeq {
					rt.Fatalf("bump %q returned %d, not above the highest SeqNum issued so far (%d)", slug, seq, maxSeq)
				}
				maxSeq = seq
				r.seq, r.hash = seq, hash
			case op == 8:
				if err := s.DeleteSiteConfig(slug); err != nil {
					rt.Fatalf("delete %q: %v", slug, err)
				}
				if _, ok := ref[slug]; ok {
					st.deletes++
				}
				delete(ref, slug)
			default: // list consistency
				all, err := s.ListSiteConfigs()
				if err != nil {
					rt.Fatal(err)
				}
				if len(all) != len(ref) {
					rt.Fatalf("list %d rows, model %d", len(all), len(ref))
				}
				bySite := map[string][]string{}
				for _, c := range all {
					r := ref[c.Slug]
					if r == nil || c.LastSnapshotSeq != r.seq {
						rt.Fatalf("list row %q seq=%d disagrees with model %+v", c.Slug, c.LastSnapshotSeq, r)
					}
					bySite[c.NetBoxSite] = append(bySite[c.NetBoxSite], c.Slug)
				}
				for site, want := range bySite {
					got, err := s.ListSiteConfigsByNetBoxSite(site)
					if err != nil {
						rt.Fatal(err)
					}
					var gs []string
					for _, c := range got {
						gs = append(gs, c.Slug)
					}
					sort.Strings(gs)
					sort.Strings(want)
					if !reflect.DeepEqual(gs, want) {
						rt.Fatalf("by-site %q = %v, want %v", site, gs, want)
					}
				}
			}
		}
	})
	t.Logf("%+v", st)
}

// TestPropSiteConfigDBRoundTrip: Create then Get returns every operator
// field unchanged, including unicode and nil vs empty relay pools.
func TestPropSiteConfigDBRoundTrip(t *testing.T) {
	s := testStore(t)
	var nilPools, emptyPools int
	rapid.Check(t, func(rt *rapid.T) {
		if err := s.db.Exec("DELETE FROM site_configs").Error; err != nil {
			rt.Fatal(err)
		}
		c := drawValidSiteConfig(rt, rapid.StringMatching(`[a-z0-9ü-]{1,20}`).Draw(rt, "slug"))
		if err := s.CreateSiteConfig(&c); err != nil {
			rt.Fatalf("create: %v", err)
		}
		got, err := s.GetSiteConfig(c.Slug)
		if err != nil {
			rt.Fatal(err)
		}
		if c.RelayPools == nil {
			nilPools++
			if got.RelayPools != nil {
				rt.Fatalf("nil relay pools read back as %#v", got.RelayPools)
			}
		} else if len(c.RelayPools) == 0 {
			emptyPools++
		}
		if !sameDBTime(got.CreatedAt, c.CreatedAt) {
			rt.Fatalf("created_at %v != %v", got.CreatedAt, c.CreatedAt)
		}
		if !reflect.DeepEqual(operatorFields(*got), operatorFields(c)) {
			rt.Fatalf("round trip:\n got %+v\nwant %+v", operatorFields(*got), operatorFields(c))
		}
	})
	t.Logf("nil-pools=%d empty-pools=%d", nilPools, emptyPools)
}
