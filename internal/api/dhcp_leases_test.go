package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// fakeDHCPLeasesRPC is a PowerStatusRPC stub that only exercises
// DHCPLeases; the other methods are unused by these tests.
type fakeDHCPLeasesRPC struct {
	results map[string][]ws.WorkerDHCPLeases
}

func (f fakeDHCPLeasesRPC) PowerStatus(context.Context, string, uuid.UUID, json.RawMessage, json.RawMessage) (ws.PowerStatusResponseMsg, error) {
	return ws.PowerStatusResponseMsg{}, nil
}
func (f fakeDHCPLeasesRPC) BootStatus(context.Context, string, uuid.UUID, json.RawMessage) (ws.BootStatusResponseMsg, error) {
	return ws.BootStatusResponseMsg{}, nil
}
func (f fakeDHCPLeasesRPC) ConnectedWorkerSerials(string) []string { return nil }
func (f fakeDHCPLeasesRPC) DHCPLeases(_ context.Context, slug string) []ws.WorkerDHCPLeases {
	return f.results[slug]
}

func dhcpLeasesFixtureServer(t *testing.T, rpc PowerStatusRPC) (*Server, *store.Store) {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	s := &Server{store: st, powerStatusRPC: rpc, echo: echo.New()}
	s.echo.GET("/api/v1/site-configs/:slug/dhcp-leases", s.dhcpLeasesSiteConfig)
	return s, st
}

// TestDHCPLeasesSiteConfig_UnknownSlug pins the 404 on a slug with no
// site_config row — the handler must reject before ever touching the
// RPC layer.
func TestDHCPLeasesSiteConfig_UnknownSlug(t *testing.T) {
	s, _ := dhcpLeasesFixtureServer(t, fakeDHCPLeasesRPC{})
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop9/dhcp-leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDHCPLeasesSiteConfig_NoRPCConfigured pins the unconfigured-server
// outcome: a clean 503, not a nil-deref.
func TestDHCPLeasesSiteConfig_NoRPCConfigured(t *testing.T) {
	s, st := dhcpLeasesFixtureServer(t, nil)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestDHCPLeasesSiteConfig_NoWorkersConnected pins the empty-cage
// outcome: a 200 with an empty workers array, not an error — an empty
// HA cage is an answerable question.
func TestDHCPLeasesSiteConfig_NoWorkersConnected(t *testing.T) {
	s, st := dhcpLeasesFixtureServer(t, fakeDHCPLeasesRPC{})
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body DHCPLeasesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Workers == nil || len(body.Workers) != 0 {
		t.Errorf("Workers = %+v, want a non-nil empty slice", body.Workers)
	}
}

// TestDHCPLeasesSiteConfig_WorkerErrorPassthrough pins that a
// per-worker error from the hub fan-out reaches the wire response
// (rather than being swallowed or turned into an HTTP error), sitting
// alongside another worker's successful leases.
func TestDHCPLeasesSiteConfig_WorkerErrorPassthrough(t *testing.T) {
	rpc := fakeDHCPLeasesRPC{results: map[string][]ws.WorkerDHCPLeases{
		"pop1": {
			{WorkerID: "worker-1", FQDN: "worker-1.pop1", Err: "site worker has no DHCP handler configured"},
			{WorkerID: "worker-2", FQDN: "worker-2.pop1", Leases: []ws.DHCPLeaseInfo{
				{MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.100", Kind: "dynamic", Pool: "10.99.1.0/24"},
			}},
		},
	}}
	s, st := dhcpLeasesFixtureServer(t, rpc)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/site-configs/pop1/dhcp-leases")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body DHCPLeasesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Workers) != 2 {
		t.Fatalf("got %d workers, want 2: %+v", len(body.Workers), body.Workers)
	}
	byID := make(map[string]DHCPLeasesWorkerResult, 2)
	for _, w := range body.Workers {
		byID[w.WorkerID] = w
	}
	w1 := byID["worker-1"]
	if !strings.Contains(w1.Error, "no DHCP handler") {
		t.Errorf("worker-1: Error = %q, want it to carry the RPC error", w1.Error)
	}
	w2 := byID["worker-2"]
	if w2.Error != "" || len(w2.Leases) != 1 || w2.Leases[0].MAC != "aa:bb:cc:00:00:01" {
		t.Errorf("worker-2 (healthy) result = %+v", w2)
	}
}
