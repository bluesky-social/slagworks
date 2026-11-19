package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Phase 3 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage:
// GET /site-configs/:slug/dhcp-lease-history. Unlike dhcp-leases
// (dhcp_leases_test.go) this endpoint never touches the WS hub — it's
// a plain store query, so these tests build a Server with no
// powerStatusRPC at all.

func dhcpLeaseHistoryFixtureServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	s := &Server{store: st, echo: echo.New()}
	s.echo.GET("/api/v1/site-configs/:slug/dhcp-lease-history", s.dhcpLeaseHistorySiteConfig)
	return s, st
}

// TestDHCPLeaseHistorySiteConfig_UnknownSlug pins the 404 on a slug
// with no site_config row.
func TestDHCPLeaseHistorySiteConfig_UnknownSlug(t *testing.T) {
	s, _ := dhcpLeaseHistoryFixtureServer(t)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop9/dhcp-lease-history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDHCPLeaseHistorySiteConfig_EmptyHistory pins the empty-history
// outcome: a 200 with an empty (non-nil) entries array, not an error.
func TestDHCPLeaseHistorySiteConfig_EmptyHistory(t *testing.T) {
	s, st := dhcpLeaseHistoryFixtureServer(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-lease-history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body DHCPLeaseHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Entries == nil || len(body.Entries) != 0 {
		t.Errorf("Entries = %+v, want a non-nil empty slice", body.Entries)
	}
}

// TestDHCPLeaseHistorySiteConfig_FiltersAndDefaultOrder pins that
// seeded ledger rows come back newest-first, and mac/ip query params
// filter as expected. Also proves the read works even though no WS
// hub / powerStatusRPC is configured — it's a pure store query.
func TestDHCPLeaseHistorySiteConfig_FiltersAndDefaultOrder(t *testing.T) {
	s, st := dhcpLeaseHistoryFixtureServer(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}

	ctx := context.Background()
	expiry := time.Now().Add(5 * time.Minute)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:01", "10.99.1.100", "dynamic", "10.99.1.0/24", nil, expiry); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:02", "10.99.1.101", "dynamic", "10.99.1.0/24", nil, expiry); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-lease-history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body DHCPLeaseHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(body.Entries), body.Entries)
	}
	if body.Entries[0].MAC != "aa:bb:cc:00:00:02" {
		t.Errorf("entries[0].MAC = %q, want the more-recently-touched mac2 (newest-first)", body.Entries[0].MAC)
	}

	resp2, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-lease-history?mac=aa:bb:cc:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var filtered DHCPLeaseHistoryResponse
	if err := json.NewDecoder(resp2.Body).Decode(&filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Entries) != 1 || filtered.Entries[0].MAC != "aa:bb:cc:00:00:01" {
		t.Fatalf("mac filter got %+v, want exactly mac1's row", filtered.Entries)
	}
}

// TestDHCPLeaseHistorySiteConfig_InvalidLimitBadRequest pins that a
// non-numeric ?limit= is rejected with a 400, not silently ignored or
// a 500.
func TestDHCPLeaseHistorySiteConfig_InvalidLimitBadRequest(t *testing.T) {
	s, st := dhcpLeaseHistoryFixtureServer(t)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-lease-history?limit=not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
