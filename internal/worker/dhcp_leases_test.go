package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// TestRunDHCPLeases_NoSourceConfigured pins the nil-source error path
// (OOB-only or test configurations that run no DHCP handler on this
// process): the response must carry an error, never a bogus empty
// success.
func TestRunDHCPLeases_NoSourceConfigured(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, NewCache(nil), nil, nil)
	w.SetLogger(testLogger())

	resp := w.runDHCPLeases()
	if resp.Error == "" {
		t.Fatal("expected an error when no lease source is configured")
	}
	if resp.Error != "site worker has no DHCP handler configured" {
		t.Errorf("got %q", resp.Error)
	}
}

// TestRunDHCPLeases_ClassifyAndEnrich pins the classification +
// identity-enrichment contract: a lease at a MAC's reservation IP is
// "static", everything else is "dynamic"; MachineID/Hostname populate
// from the cache when the MAC resolves to a known machine, and stay
// empty for an unresolved MAC (legal on a promiscuous OOB segment).
func TestRunDHCPLeases_ClassifyAndEnrich(t *testing.T) {
	cache := NewCache(nil)

	knownID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{
		ID:       knownID,
		Hostname: "kworker-1.pop1",
		Interfaces: []model.SnapshotInterface{
			{MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.50/24"},
		},
	})

	w := NewWSConn(WSConnConfig{}, cache, nil, nil)
	w.SetLogger(testLogger())

	now := time.Now().Truncate(time.Second)
	w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo {
		return []dhcp.LeaseInfo{
			// Known machine, leased AT its reservation -> static, enriched.
			{MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.50", Expiry: now, Pool: "10.99.1.0/24"},
			// Known machine's MAC but a DIFFERENT leased IP than its
			// reservation -> dynamic (classification follows the actual
			// leased address, not "does a reservation exist").
			{MAC: "aa:bb:cc:00:00:02", IP: "10.99.1.150", Expiry: now, Pool: "10.99.1.0/24"},
		}
	})

	resp := w.runDHCPLeases()
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.DHCPLeasesResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Leases) != 2 {
		t.Fatalf("got %d leases, want 2: %+v", len(out.Leases), out.Leases)
	}

	byMAC := make(map[string]ws.DHCPLeaseInfo, len(out.Leases))
	for _, l := range out.Leases {
		byMAC[l.MAC] = l
	}

	known := byMAC["aa:bb:cc:00:00:01"]
	if known.Kind != "static" {
		t.Errorf("known machine at its reservation: Kind = %q, want static", known.Kind)
	}
	if known.MachineID != knownID.String() {
		t.Errorf("MachineID = %q, want %q", known.MachineID, knownID.String())
	}
	if known.Hostname != "kworker-1.pop1" {
		t.Errorf("Hostname = %q, want kworker-1.pop1", known.Hostname)
	}
	if !known.ExpiresAt.Equal(now) {
		t.Errorf("ExpiresAt = %v, want %v", known.ExpiresAt, now)
	}
	if known.Pool != "10.99.1.0/24" {
		t.Errorf("Pool = %q, want 10.99.1.0/24", known.Pool)
	}

	unknown := byMAC["aa:bb:cc:00:00:02"]
	if unknown.Kind != "dynamic" {
		t.Errorf("known MAC leased off-reservation: Kind = %q, want dynamic", unknown.Kind)
	}
	if unknown.MachineID != "" || unknown.Hostname != "" {
		t.Errorf("unrelated lease must not be enriched: %+v", unknown)
	}
}

// TestRunDHCPLeases_UnresolvedMAC pins the promiscuous-OOB-segment
// case: a MAC with no reservation and no cache entry still reports as
// a dynamic lease with empty identity fields, not an error.
func TestRunDHCPLeases_UnresolvedMAC(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, NewCache(nil), nil, nil)
	w.SetLogger(testLogger())
	w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo {
		return []dhcp.LeaseInfo{
			{MAC: "de:ad:be:ef:00:01", IP: "10.99.1.200", Expiry: time.Now(), Pool: "10.99.1.0/24"},
		}
	})

	resp := w.runDHCPLeases()
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.DHCPLeasesResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Leases) != 1 {
		t.Fatalf("got %d leases, want 1", len(out.Leases))
	}
	l := out.Leases[0]
	if l.Kind != "dynamic" || l.MachineID != "" || l.Hostname != "" {
		t.Errorf("unresolved MAC: got %+v, want dynamic with empty identity", l)
	}
}

// TestRunDHCPLeases_SourceReturnsNil pins the standby-teardown case: a
// wired source that reports "no live handler right now" (the
// atomic-pointer indirection in cmd/slagworksd/site.go returns nil
// when a VIP release has cleared the handler) must produce the same
// error as no source at all — never a bogus empty success, which
// would hide a stopped worker from slagctl (roast finding,
// 2026-07-13). dhcp.Handler.Leases always returns a non-nil slice, so
// nil is an unambiguous "no handler" signal.
func TestRunDHCPLeases_SourceReturnsNil(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, NewCache(nil), nil, nil)
	w.SetLogger(testLogger())
	w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo { return nil })

	resp := w.runDHCPLeases()
	if resp.Error == "" {
		t.Fatal("expected an error when the source reports no live handler (nil), got a success")
	}
	if resp.Error != "site worker has no DHCP handler configured" {
		t.Errorf("got %q", resp.Error)
	}
}

// TestRunDHCPLeases_ConcurrentSetAndRead exercises SetDHCPLeaseSource
// racing with runDHCPLeases under -race: standby VIP transitions
// rewire the source while the WS read loop is already live answering
// RPCs on other goroutines (roast finding, 2026-07-13 — a plain
// function field there was a data race). This doesn't assert on
// output; it exists so `go test -race` catches a regression.
func TestRunDHCPLeases_ConcurrentSetAndRead(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, NewCache(nil), nil, nil)
	w.SetLogger(testLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo {
				return []dhcp.LeaseInfo{{MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.1"}}
			})
		}
	}()
	for i := 0; i < 100; i++ {
		w.runDHCPLeases()
	}
	<-done
}
