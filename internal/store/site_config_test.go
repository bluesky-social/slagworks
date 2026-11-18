package store

import (
	"testing"

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
	seq, err := st.BumpSiteSnapshotMeta("pop3", "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("first bump should return 1, got %d", seq)
	}
	seq, err = st.BumpSiteSnapshotMeta("pop3", "hash-b")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("second bump should return 2, got %d", seq)
	}
	got, err := st.GetSiteConfig("pop3")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSnapshotHash != "hash-b" || got.LastSnapshotSeq != 2 {
		t.Fatalf("got hash=%q seq=%d", got.LastSnapshotHash, got.LastSnapshotSeq)
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
