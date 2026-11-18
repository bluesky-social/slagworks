package worker

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
)

// DHCPLeaseForwarder turns dhcp.Handler lease-mutation callbacks into
// dhcp_lease observations on the outbox, adding the classification
// and machine-identity enrichment only the worker's cache can supply
// (the dhcp package knows only MAC/IP/expiry/pool — see
// docs/plans/2026-07-13-dhcp-lease-visibility.md Phase 3).
//
// OnLeaseEvent is wired as dhcp.Config.OnLeaseEvent and therefore runs
// SYNCHRONOUSLY, under the dhcp handler's own mutex (see that field's
// doc comment for the priced deviation). This forwarder must stay
// lean: the debounce check plus cache.RecordDHCPLease's SQLite insert
// are the only work done here. RecordDHCPLease's only lock (the
// outbox's own mutex) never calls back into the dhcp package, so
// there is no lock-ordering cycle with the handler's mutex.
type DHCPLeaseForwarder struct {
	cache *Cache

	// leaseDuration is the segment's configured DHCP lease duration.
	// The renew debounce window is half of it (a 5-min-lease OOB
	// segment must not write a ledger row per ~2.5-min renew tick);
	// entries idle longer than one full lease duration are pruned.
	leaseDuration time.Duration

	mu        sync.Mutex
	lastRenew map[string]time.Time // "mac|ip" -> last emitted renew time
}

// NewDHCPLeaseForwarder builds a forwarder for the given cache and
// lease duration. leaseDuration should match the dhcp.Config the
// forwarder is wired into (dhcpCfg.LeaseDuration after any
// snapshot-derived override) — see cmd/slagworksd/site.go's
// buildDHCPConfig.
func NewDHCPLeaseForwarder(cache *Cache, leaseDuration time.Duration) *DHCPLeaseForwarder {
	return &DHCPLeaseForwarder{
		cache:         cache,
		leaseDuration: leaseDuration,
		lastRenew:     make(map[string]time.Time),
	}
}

// OnLeaseEvent is the dhcp.Config.OnLeaseEvent callback: debounce
// renews, classify, resolve machine identity, and append the
// observation. allocate and delete ALWAYS emit (and clear any
// debounce entry for the (MAC, IP) pair — a fresh allocate's first
// renew must not be suppressed by a stale entry from a prior lease on
// the same pair).
func (f *DHCPLeaseForwarder) OnLeaseEvent(ev dhcp.LeaseEvent) {
	mac := ev.MAC.String()
	ip := ev.IP.String()
	key := mac + "|" + ip

	if ev.Event == "renew" {
		f.mu.Lock()
		now := time.Now()
		// The debounce window is derived from THIS event's own expiry
		// (ev.Expiry - now ≈ the lease duration actually in effect for
		// the interface that granted it), not the forwarder's base
		// leaseDuration: a per-interface override (e.g. the OOB
		// segment's short rollout lease, see IfaceParams.LeaseDuration)
		// can be shorter than the base fabric lease, and one shared
		// forwarder serves every interface. Debouncing against the base
		// duration would suppress OOB renews for longer than an OOB
		// lease is actually valid (roast finding, 2026-07-13). Falls
		// back to the base duration if Expiry is non-positive (defensive;
		// every real LeaseEvent carries a future expiry).
		window := ev.Expiry.Sub(now) / 2
		if window <= 0 {
			window = f.leaseDuration / 2
		}
		if last, ok := f.lastRenew[key]; ok && now.Sub(last) < window {
			f.mu.Unlock()
			return
		}
		f.lastRenew[key] = now
		f.pruneLocked(now)
		f.mu.Unlock()
	} else {
		f.mu.Lock()
		delete(f.lastRenew, key)
		f.mu.Unlock()
	}

	// Static iff the MAC's reservation IP matches the actually-leased
	// IP, matching runDHCPLeases' classification (Phase 2) — a
	// reservation-holding MAC could in principle be holding a dynamic
	// lease (reservation added after the lease).
	kind := "dynamic"
	if reserved := f.cache.ReservedIPForMAC(mac); reserved != nil && reserved.Equal(ev.IP) {
		kind = "static"
	}
	machineID := uuid.Nil
	if m := f.cache.MachineByMAC(mac); m != nil {
		machineID = m.ID
	}

	f.cache.RecordDHCPLease(machineID, ev.Event, mac, ip, kind, ev.Pool, ev.Expiry)
}

// pruneLocked drops debounce entries idle longer than one full lease
// duration: a renew inside that window would already have refreshed
// the entry, so anything older belongs to a lease that has expired or
// moved and can't affect a future decision. Caller must hold f.mu.
func (f *DHCPLeaseForwarder) pruneLocked(now time.Time) {
	for k, t := range f.lastRenew {
		if now.Sub(t) > f.leaseDuration {
			delete(f.lastRenew, k)
		}
	}
}
