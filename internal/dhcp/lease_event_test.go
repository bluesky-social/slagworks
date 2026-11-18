package dhcp

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/iana"
)

// Phase 3 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage:
// Config.OnLeaseEvent emission order and classification. The callback
// runs synchronously under h.mu from every lease-mutation site — these
// tests exercise it the same way the worker wiring will: as a plain
// func collecting events into a slice.

// eventRecorder is a thread-safe LeaseEvent sink for tests. A mutex is
// used (not just "single-threaded, no lock needed") because
// OnLeaseEvent is invoked from inside h.mu, and appending to a shared
// slice from a callback under a DIFFERENT package's lock is exactly
// the kind of thing -race should be able to catch if the handler ever
// grew a second call path that races.
type eventRecorder struct {
	mu     sync.Mutex
	events []LeaseEvent
}

func (r *eventRecorder) record(ev LeaseEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) snapshot() []LeaseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LeaseEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestLeaseEvent_NilCallbackSafe pins that a nil OnLeaseEvent (the
// default) never panics across every mutation path: allocate, renew,
// expiry-sweep delete, and adopt-in-place.
func TestLeaseEvent_NilCallbackSafe(t *testing.T) {
	h := splitPoolHandler() // testConfig(), OnLeaseEvent unset (nil)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x02, 0x01}

	offer := h.HandlePacketOn(mustDiscover(t, mac, withArch(iana.EFI_X86_64)), "eno3")
	if offer == nil {
		t.Fatal("expected an offer")
	}
	req := mustRequestFromOffer(t, offer)
	req.ClientHWAddr = mac
	if reply := h.HandlePacketOn(req, "eno3"); !isACK(reply) {
		t.Fatalf("expected ACK, got %v", reply)
	}

	// Adopt-in-place path too.
	mac2 := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x02, 0x02}
	if reply := h.HandlePacketOn(initReboot(t, mac2, "10.254.241.120"), "eno3"); !isACK(reply) {
		t.Fatalf("expected adopt ACK, got %v", reply)
	}
}

// TestLeaseEvent_AllocateThenRenew: a fresh DISCOVER+REQUEST emits
// exactly one "allocate"; a subsequent renew (same MAC, same IP, still
// unexpired) via allocateIP's existing-lease branch emits "renew", not
// a second "allocate".
func TestLeaseEvent_AllocateThenRenew(t *testing.T) {
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x03, 0x01}

	offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected an offer")
	}
	ip := offer.YourIPAddr

	req := mustRequestFromOffer(t, offer)
	req.ClientHWAddr = mac
	if reply := h.HandlePacket(req); !isACK(reply) {
		t.Fatalf("expected ACK, got %v", reply)
	}

	// The DISCOVER's OFFER path and the REQUEST's ACK path both call
	// allocateIP: DISCOVER hits the free-IP branch (allocate), REQUEST
	// hits the existing-lease branch (renew) since the lease from the
	// DISCOVER is already unexpired and in-range.
	events := rec.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (allocate, renew): %+v", len(events), events)
	}
	if events[0].Event != "allocate" || !events[0].IP.Equal(ip) || events[0].MAC.String() != mac.String() {
		t.Errorf("event[0] = %+v, want allocate for %s/%s", events[0], mac, ip)
	}
	if events[1].Event != "renew" || !events[1].IP.Equal(ip) || events[1].MAC.String() != mac.String() {
		t.Errorf("event[1] = %+v, want renew for %s/%s", events[1], mac, ip)
	}
}

// TestLeaseEvent_AdoptInPlaceEmitsAllocate: adopt-in-place (a fresh
// MAC->IP binding with no prior lease) emits "allocate", the same
// classification a plain fresh DORA allocate gets — adopt is a
// different code path (classifyRequest -> setLease) but the same
// semantic event.
func TestLeaseEvent_AdoptInPlaceEmitsAllocate(t *testing.T) {
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", splitPoolParams())

	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x03, 0x02}
	reqIP := "10.254.241.130"
	if reply := h.HandlePacketOn(initReboot(t, mac, reqIP), "eno3"); !isACK(reply) {
		t.Fatalf("expected adopt ACK, got %v", reply)
	}

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (allocate): %+v", len(events), events)
	}
	if events[0].Event != "allocate" || events[0].IP.String() != reqIP || events[0].MAC.String() != mac.String() {
		t.Errorf("event = %+v, want allocate for %s/%s", events[0], mac, reqIP)
	}
}

// TestLeaseEvent_ExpirySweepEmitsDelete: an expired entry purged by
// allocateIP's lazy-cleanup sweep (triggered by a later, unrelated
// allocation) emits "delete" for the purged (MAC, IP) — not for the
// entry being newly allocated.
func TestLeaseEvent_ExpirySweepEmitsDelete(t *testing.T) {
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	h := NewHandler(cfg)

	expiredMAC := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01}
	expiredIP := net.ParseIP("10.99.1.150")
	// Seed an already-expired entry directly (bypassing setLease, so no
	// event fires for the seed itself — this mirrors a lease that
	// expired naturally after being granted in an earlier, unobserved
	// test phase).
	h.mu.Lock()
	h.leases[expiredMAC.String()] = &lease{MAC: expiredMAC, IP: expiredIP, Expiry: time.Now().Add(-time.Minute)}
	h.mu.Unlock()

	// A fresh DISCOVER from a different MAC walks the pool, sweeping
	// the expired entry along the way.
	freshMAC := net.HardwareAddr{0xbb, 0, 0, 0, 0, 0x01}
	offer := h.HandlePacket(mustDiscover(t, freshMAC, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected an offer")
	}

	events := rec.snapshot()
	var sawDelete, sawAllocate bool
	for _, ev := range events {
		switch {
		case ev.Event == "delete" && ev.MAC.String() == expiredMAC.String() && ev.IP.Equal(expiredIP):
			sawDelete = true
		case ev.Event == "allocate" && ev.MAC.String() == freshMAC.String():
			sawAllocate = true
		}
	}
	if !sawDelete {
		t.Errorf("expected a delete event for the expired sweep, got %+v", events)
	}
	if !sawAllocate {
		t.Errorf("expected an allocate event for the fresh DISCOVER, got %+v", events)
	}
}

// TestLeaseEvent_ExpiredOwnLeaseReDiscoverEmitsAllocateNotRenew pins
// that a MAC whose OWN lease has already expired, re-DISCOVERing
// before any sweep has purged it, is classified as delete+allocate —
// never "renew" (roast finding, 2026-07-13: allocateIP's fast
// existing-lease path had no now.Before(l.Expiry) guard, so it
// revived the expired entry in place and reported it as a renewal,
// corrupting the ledger's expiry boundary). With the guard, the
// expired entry falls through to the sweep loop (which purges it,
// emitting "delete") before the free-IP scan reallocates the same
// address fresh (emitting "allocate") — correctly closing the expired
// interval instead of silently extending it.
func TestLeaseEvent_ExpiredOwnLeaseReDiscoverEmitsAllocateNotRenew(t *testing.T) {
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	h := NewHandler(cfg)

	mac := net.HardwareAddr{0xcc, 0, 0, 0, 0, 0x01}
	ip := net.ParseIP("10.99.1.100")
	// Seed an already-expired entry for this MAC directly (bypassing
	// setLease, so no event fires for the seed itself).
	h.mu.Lock()
	h.leases[mac.String()] = &lease{MAC: mac, IP: ip, Expiry: time.Now().Add(-time.Minute)}
	h.mu.Unlock()

	offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected an offer")
	}

	events := rec.snapshot()
	for _, ev := range events {
		if ev.Event == "renew" {
			t.Fatalf("got a renew event for an already-expired lease: %+v (full sequence: %+v)", ev, events)
		}
	}
	if len(events) != 2 || events[0].Event != "delete" || events[1].Event != "allocate" {
		t.Fatalf("events = %+v, want exactly [delete, allocate]", events)
	}
}

// TestLeaseEvent_LoadTimePurgeEmitsNothing: NewHandler's load-time
// purge of expired persisted leases is pre-history noise from before
// this process started — it must not emit ANY LeaseEvent, live or
// purged.
func TestLeaseEvent_LoadTimePurgeEmitsNothing(t *testing.T) {
	rec := &eventRecorder{}
	store := newMemLeaseStore()
	live := net.HardwareAddr{0xcc, 0, 0, 0, 0, 0x01}
	store.SaveLease(live, net.ParseIP("10.99.1.101"), time.Now().Add(time.Hour))
	expired := net.HardwareAddr{0xcc, 0, 0, 0, 0, 0x02}
	store.SaveLease(expired, net.ParseIP("10.99.1.102"), time.Now().Add(-time.Hour))

	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	cfg.LeaseStore = store
	_ = NewHandler(cfg)

	if events := rec.snapshot(); len(events) != 0 {
		t.Errorf("NewHandler's load-time purge emitted %d events, want 0: %+v", len(events), events)
	}
}

// TestLeaseEvent_ReservationRenewNotDoubleAllocated: a reserved MAC
// re-DISCOVERing after already holding its reservation lease
// classifies as "renew" via setLease's own-IP check, exercising the
// reservation branch of allocateIP (not just the dynamic-pool branch).
func TestLeaseEvent_ReservationRenewNotDoubleAllocated(t *testing.T) {
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	reservedIP := net.ParseIP("10.99.1.50")
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:00:04:01" {
			return reservedIP
		}
		return nil
	}
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x04, 0x01}

	// First DISCOVER: reservation branch of allocateIP calls setLease
	// fresh -> allocate.
	if offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64))); offer == nil {
		t.Fatal("expected an offer")
	}
	// Second DISCOVER: reservation branch calls setLease again for the
	// SAME (mac, ip) pair, still unexpired -> renew.
	if offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64))); offer == nil {
		t.Fatal("expected a second offer")
	}

	events := rec.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Event != "allocate" {
		t.Errorf("event[0] = %+v, want allocate", events[0])
	}
	if events[1].Event != "renew" {
		t.Errorf("event[1] = %+v, want renew", events[1])
	}
}
