package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestCreateSiteConfig(t *testing.T) {
	st := testStore(t)
	cfg := &model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	if err := st.CreateSiteConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestGetSiteConfig(t *testing.T) {
	st := testStore(t)
	cfg := &model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	if err := st.CreateSiteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "pop3" {
		t.Fatalf("got slug %q", got.Slug)
	}
}

// TestSiteConfigLocationRoundTrip verifies the netbox_site/netbox_location
// columns persist and read back, and that a location-scoped config is
// distinguishable from a whole-site one.
func TestSiteConfigLocationRoundTrip(t *testing.T) {
	st := testStore(t)
	loc := &model.SiteConfig{
		Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "pop2-01-005330",
		EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
	whole := &model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.1.0.100", EphemeralPoolEnd: "10.1.0.200",
	}
	if err := st.CreateSiteConfig(loc); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSiteConfig(whole); err != nil {
		t.Fatal(err)
	}
	gotLoc, err := st.GetSiteConfig("pop2-old-cage")
	if err != nil {
		t.Fatal(err)
	}
	if gotLoc.NetBoxSite != "pop2" || gotLoc.NetBoxLocation != "pop2-01-005330" {
		t.Fatalf("location-scoped round-trip mismatch: site=%q location=%q", gotLoc.NetBoxSite, gotLoc.NetBoxLocation)
	}
	if !gotLoc.LocationScoped() {
		t.Error("config with a location should report LocationScoped()")
	}
	gotWhole, err := st.GetSiteConfig("pop1")
	if err != nil {
		t.Fatal(err)
	}
	if gotWhole.NetBoxLocation != "" || gotWhole.LocationScoped() {
		t.Errorf("whole-site config should have empty location; got %q", gotWhole.NetBoxLocation)
	}
}

func TestBumpSiteSnapshotMeta(t *testing.T) {
	st := testStore(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}); err != nil {
		t.Fatal(err)
	}
	first, err := st.BumpSiteSnapshotMeta("pop3", "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if first <= 0 {
		t.Fatalf("first bump should return a positive seq, got %d", first)
	}
	second, err := st.BumpSiteSnapshotMeta("pop3", "hash-b")
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("second bump returned %d, not above the first %d", second, first)
	}
	got, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSnapshotHash != "hash-b" || got.LastSnapshotSeq != second {
		t.Fatalf("got hash=%q seq=%d, want hash=%q seq=%d", got.LastSnapshotHash, got.LastSnapshotSeq, "hash-b", second)
	}
}

// TestBumpSiteSnapshotMetaSurvivesRecreate covers `slagctl site-config
// delete` followed by `create` with the same slug. The slug's workers keep
// their tokens and their persisted snapshot SeqNum across that, and
// worker.Cache.ApplySnapshot drops any snapshot whose SeqNum is not above
// the one it holds. So the first SeqNum issued after the re-create must be
// above every SeqNum issued before the delete, or the workers silently
// ignore global until the new count catches up.
func TestBumpSiteSnapshotMetaSurvivesRecreate(t *testing.T) {
	st := testStore(t)
	mk := func() *model.SiteConfig {
		return &model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	}
	if err := st.CreateSiteConfig(mk()); err != nil {
		t.Fatal(err)
	}
	var last int64
	for _, h := range []string{"hash-a", "hash-b", "hash-c"} {
		seq, err := st.BumpSiteSnapshotMeta("pop3", h)
		if err != nil {
			t.Fatal(err)
		}
		if seq <= last {
			t.Fatalf("bump for %s returned %d, not above the previous %d", h, seq, last)
		}
		last = seq
	}

	if err := st.DeleteSiteConfig("pop3"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSiteConfig(mk()); err != nil {
		t.Fatal(err)
	}
	seq, err := st.BumpSiteSnapshotMeta("pop3", "hash-d")
	if err != nil {
		t.Fatal(err)
	}
	if seq <= last {
		t.Fatalf("after delete and re-create, the next SeqNum for pop3 is %d, not above the %d its workers already applied", seq, last)
	}
	got, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSnapshotSeq != seq || got.LastSnapshotHash != "hash-d" {
		t.Fatalf("row holds seq=%d hash=%q, want seq=%d hash=%q", got.LastSnapshotSeq, got.LastSnapshotHash, seq, "hash-d")
	}
}

// PATCH /site-configs/:slug loads the row, validates the merged config
// against NetBox and then calls UpdateSiteConfig. The snapshot dispatcher
// can bump last_snapshot_seq in that window. UpdateSiteConfig must not
// write the loaded seq and hash back: the next rebuild would then reissue
// a SeqNum the worker already applied, and the worker drops a push whose
// SeqNum it already holds, so the operator's change never reaches the site.
func TestUpdateSiteConfigKeepsSnapshotMetaBumpedAfterLoad(t *testing.T) {
	st := testStore(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetSiteConfig("pop3") // the PATCH handler loads the row
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := st.BumpSiteSnapshotMeta("pop3", "hash-1"); err != nil || seq != 1 { // a rebuild pushes meanwhile
		t.Fatalf("bump: seq=%d err=%v", seq, err)
	}
	loaded.Promiscuous = true
	if err := st.UpdateSiteConfig(loaded); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Promiscuous {
		t.Fatalf("setup: update not applied")
	}
	if got.LastSnapshotSeq != 1 || got.LastSnapshotHash != "hash-1" {
		t.Fatalf("UpdateSiteConfig rolled the snapshot meta back to seq=%d hash=%q; want seq=1 hash=\"hash-1\" (the next push would reissue SeqNum 1)",
			got.LastSnapshotSeq, got.LastSnapshotHash)
	}
}

// A site config deleted between the PATCH handler's load and its write
// stays deleted: UpdateSiteConfig reports ErrSiteConfigNotFound instead of
// inserting the stale copy.
func TestUpdateSiteConfigDoesNotRecreateDeletedRow(t *testing.T) {
	st := testStore(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSiteConfig("pop3"); err != nil {
		t.Fatal(err)
	}
	loaded.Promiscuous = true
	if err := st.UpdateSiteConfig(loaded); !errors.Is(err, ErrSiteConfigNotFound) {
		t.Errorf("UpdateSiteConfig of a deleted row: got %v, want ErrSiteConfigNotFound", err)
	}
	if _, err := st.GetSiteConfig("pop3"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("UpdateSiteConfig re-created the deleted row (GetSiteConfig err=%v)", err)
	}
}

func TestListSiteConfigs(t *testing.T) {
	st := testStore(t)
	for _, slug := range []string{"pop3", "pop4", "pop5"} {
		if err := st.CreateSiteConfig(&model.SiteConfig{
			Slug: slug, NetBoxSite: slug, EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListSiteConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}

// The admit callback sees every row as it stands at the write, and a
// refusal is returned unchanged and writes nothing.
func TestSiteConfigAdmitRunsAgainstCurrentRowsAndRefusalWritesNothing(t *testing.T) {
	st := testStore(t)
	mk := func(slug, site string) *model.SiteConfig {
		return &model.SiteConfig{Slug: slug, NetBoxSite: site, EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	}
	if err := st.CreateSiteConfig(mk("c1", "s1")); err != nil {
		t.Fatal(err)
	}
	errRefused := errors.New("refused")
	var saw []string
	refuse := func(existing []model.SiteConfig) error {
		saw = saw[:0]
		for _, e := range existing {
			saw = append(saw, e.Slug+"@"+e.NetBoxSite)
		}
		return errRefused
	}

	if err := st.CreateSiteConfigIfAdmitted(mk("c2", "s1"), refuse); !errors.Is(err, errRefused) {
		t.Fatalf("refused create: err=%v, want the admit error", err)
	}
	if len(saw) != 1 || saw[0] != "c1@s1" {
		t.Errorf("create admit saw %v, want [c1@s1]", saw)
	}
	if _, err := st.GetSiteConfig("c2"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("refused create left a row (GetSiteConfig err=%v)", err)
	}

	if err := st.UpdateSiteConfigIfAdmitted(mk("c1", "s2"), refuse); !errors.Is(err, errRefused) {
		t.Fatalf("refused update: err=%v, want the admit error", err)
	}
	got, err := st.GetSiteConfig("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetBoxSite != "s1" {
		t.Errorf("refused update changed netbox_site to %q", got.NetBoxSite)
	}

	admit := func([]model.SiteConfig) error { return nil }
	if err := st.UpdateSiteConfigIfAdmitted(mk("c1", "s2"), admit); err != nil {
		t.Fatalf("admitted update: %v", err)
	}
	if err := st.CreateSiteConfigIfAdmitted(mk("c2", "s1"), admit); err != nil {
		t.Fatalf("admitted create: %v", err)
	}
}
