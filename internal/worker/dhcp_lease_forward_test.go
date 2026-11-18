package worker

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
)

// Phase 3 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage:
// DHCPLeaseForwarder's renew debounce and classification/enrichment,
// exercised directly against OnLeaseEvent (the same call shape the
// dhcp handler makes from inside its own mutex).

func drainDHCPLeaseObs(t *testing.T, c *Cache) []dhcpLeaseObservation {
	t.Helper()
	pending, _ := c.DrainOutbox()
	out := make([]dhcpLeaseObservation, 0, len(pending))
	for _, p := range pending {
		if p.Obs.Event != "dhcp_lease" {
			continue
		}
		var d dhcpLeaseObservation
		if err := json.Unmarshal(p.Obs.Data, &d); err != nil {
			t.Fatalf("unmarshal dhcp_lease observation: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// TestDHCPLeaseForwarder_AllocateAlwaysEmits pins that an allocate
// event always produces one dhcp_lease observation.
func TestDHCPLeaseForwarder_AllocateAlwaysEmits(t *testing.T) {
	cache := NewCache(nil)
	f := NewDHCPLeaseForwarder(cache, 10*time.Minute)

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x01}
	ip := net.ParseIP("10.99.1.100")
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "allocate", MAC: mac, IP: ip, Expiry: time.Now().Add(10 * time.Minute), Pool: "10.99.1.0/24"})

	obs := drainDHCPLeaseObs(t, cache)
	if len(obs) != 1 {
		t.Fatalf("got %d dhcp_lease observations, want 1: %+v", len(obs), obs)
	}
	if obs[0].Event != "allocate" || obs[0].MAC != mac.String() || obs[0].IP != ip.String() || obs[0].Kind != "dynamic" || obs[0].Pool != "10.99.1.0/24" {
		t.Errorf("observation = %+v, want allocate/dynamic for %s/%s", obs[0], mac, ip)
	}
}

// TestDHCPLeaseForwarder_RenewDebounced pins the debounce contract: a
// second renew for the same (MAC, IP) inside half the lease duration
// is suppressed; one issued after the window elapses is not.
func TestDHCPLeaseForwarder_RenewDebounced(t *testing.T) {
	cache := NewCache(nil)
	leaseDur := 100 * time.Millisecond // half = 50ms debounce window
	f := NewDHCPLeaseForwarder(cache, leaseDur)

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x02}
	ip := net.ParseIP("10.99.1.101")
	ev := func() dhcp.LeaseEvent {
		return dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(leaseDur), Pool: "10.99.1.0/24"}
	}

	f.OnLeaseEvent(ev())
	f.OnLeaseEvent(ev()) // inside the debounce window -> suppressed
	f.OnLeaseEvent(ev()) // still inside -> suppressed

	obs := drainDHCPLeaseObs(t, cache)
	if len(obs) != 1 {
		t.Fatalf("got %d renew observations before the window elapsed, want 1: %+v", len(obs), obs)
	}

	time.Sleep(leaseDur/2 + 20*time.Millisecond) // past the debounce window
	f.OnLeaseEvent(ev())

	obs = drainDHCPLeaseObs(t, cache)
	if len(obs) != 1 {
		t.Fatalf("got %d renew observations after the window elapsed, want 1: %+v", len(obs), obs)
	}
	if obs[0].Event != "renew" {
		t.Errorf("observation = %+v, want renew", obs[0])
	}
}

// TestDHCPLeaseForwarder_AllocateResetsDebounce pins that an allocate
// for a (MAC, IP) pair that previously had a debounced renew entry
// clears that entry — so a FRESH lease's first renew is never wrongly
// suppressed by a stale debounce window from a prior lease on the
// same pair (e.g. the pair was released and reassigned).
func TestDHCPLeaseForwarder_AllocateResetsDebounce(t *testing.T) {
	cache := NewCache(nil)
	leaseDur := time.Hour // long window, so a debounce hit would be obvious
	f := NewDHCPLeaseForwarder(cache, leaseDur)

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x03}
	ip := net.ParseIP("10.99.1.102")

	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(leaseDur), Pool: "p"})
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "allocate", MAC: mac, IP: ip, Expiry: time.Now().Add(leaseDur), Pool: "p"})
	// A renew immediately after the allocate must emit (allocate reset
	// the debounce clock) rather than being suppressed by the earlier
	// renew's timestamp.
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(leaseDur), Pool: "p"})

	obs := drainDHCPLeaseObs(t, cache)
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3 (renew, allocate, renew): %+v", len(obs), obs)
	}
	if obs[0].Event != "renew" || obs[1].Event != "allocate" || obs[2].Event != "renew" {
		t.Fatalf("events = %v, want [renew allocate renew]", []string{obs[0].Event, obs[1].Event, obs[2].Event})
	}
}

// TestDHCPLeaseForwarder_DeleteAlwaysEmitsAndClearsDebounce pins that
// delete always emits (never debounced) and clears any pending
// debounce entry for the pair.
func TestDHCPLeaseForwarder_DeleteAlwaysEmitsAndClearsDebounce(t *testing.T) {
	cache := NewCache(nil)
	leaseDur := time.Hour
	f := NewDHCPLeaseForwarder(cache, leaseDur)

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x04}
	ip := net.ParseIP("10.99.1.103")

	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now(), Pool: "p"})
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "delete", MAC: mac, IP: ip, Expiry: time.Now(), Pool: "p"})
	// A renew right after delete must emit — delete cleared the debounce entry.
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(leaseDur), Pool: "p"})

	obs := drainDHCPLeaseObs(t, cache)
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3 (renew, delete, renew): %+v", len(obs), obs)
	}
	if obs[1].Event != "delete" {
		t.Errorf("obs[1] = %+v, want delete", obs[1])
	}
	if obs[2].Event != "renew" {
		t.Errorf("obs[2] = %+v, want renew (not suppressed by delete-cleared debounce)", obs[2])
	}
}

// TestDHCPLeaseForwarder_DebounceWindowFollowsEventExpiry pins that
// the debounce window derives from THIS renew's own Expiry, not the
// forwarder's base leaseDuration (roast finding, 2026-07-13: a
// per-interface override — e.g. the OOB segment's short lease — can
// be shorter than the base fabric lease, and one shared forwarder
// serves every interface; debouncing against the base window would
// suppress a short-lease segment's renews for longer than its lease
// is actually valid). Constructed with a LONG base leaseDuration but
// events carrying a SHORT expiry: a renew after the short lease's own
// half-window must emit, even though it's still well inside the base
// window.
func TestDHCPLeaseForwarder_DebounceWindowFollowsEventExpiry(t *testing.T) {
	cache := NewCache(nil)
	f := NewDHCPLeaseForwarder(cache, time.Hour) // long base window

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x07}
	ip := net.ParseIP("10.254.241.100")
	shortLease := 60 * time.Millisecond // half = 30ms

	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(shortLease), Pool: "oob"})
	time.Sleep(shortLease/2 + 20*time.Millisecond) // past the SHORT window, well inside the base hour
	f.OnLeaseEvent(dhcp.LeaseEvent{Event: "renew", MAC: mac, IP: ip, Expiry: time.Now().Add(shortLease), Pool: "oob"})

	obs := drainDHCPLeaseObs(t, cache)
	if len(obs) != 2 {
		t.Fatalf("got %d renew observations, want 2 (the base leaseDuration must not have masked the short lease's own window): %+v", len(obs), obs)
	}
}

// TestDHCPLeaseForwarder_ClassifyAndResolveMachine pins the
// static/dynamic classification (matches runDHCPLeases' rule: static
// iff the leased IP equals the MAC's reservation) and machine-identity
// resolution (uuid.Nil for an unresolved MAC — legal on the
// promiscuous OOB segment).
func TestDHCPLeaseForwarder_ClassifyAndResolveMachine(t *testing.T) {
	cache := NewCache(nil)
	knownID := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{
		ID:       knownID,
		Hostname: "kworker-1.pop1",
		Interfaces: []model.SnapshotInterface{
			{MAC: "aa:bb:cc:00:00:05", IP: "10.99.1.50/24"},
		},
	})
	f := NewDHCPLeaseForwarder(cache, time.Hour)

	// Known machine, leased AT its reservation -> static, resolved.
	f.OnLeaseEvent(dhcp.LeaseEvent{
		Event: "allocate", MAC: net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x05},
		IP: net.ParseIP("10.99.1.50"), Expiry: time.Now(), Pool: "p",
	})
	// Unknown MAC -> dynamic, unresolved (uuid.Nil).
	f.OnLeaseEvent(dhcp.LeaseEvent{
		Event: "allocate", MAC: net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 0x06},
		IP: net.ParseIP("10.99.1.150"), Expiry: time.Now(), Pool: "p",
	})

	pending, _ := cache.DrainOutbox()
	if len(pending) != 2 {
		t.Fatalf("got %d observations, want 2", len(pending))
	}

	var d0, d1 dhcpLeaseObservation
	if err := json.Unmarshal(pending[0].Obs.Data, &d0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pending[1].Obs.Data, &d1); err != nil {
		t.Fatal(err)
	}

	if d0.Kind != "static" {
		t.Errorf("known machine at its reservation: Kind = %q, want static", d0.Kind)
	}
	if pending[0].Obs.MachineID != knownID {
		t.Errorf("MachineID = %s, want %s", pending[0].Obs.MachineID, knownID)
	}
	if d1.Kind != "dynamic" {
		t.Errorf("unknown MAC: Kind = %q, want dynamic", d1.Kind)
	}
	if pending[1].Obs.MachineID != uuid.Nil {
		t.Errorf("unknown MAC: MachineID = %s, want uuid.Nil", pending[1].Obs.MachineID)
	}
}
