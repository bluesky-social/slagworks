package worker

// Property tests for the worker side of DHCP lease state:
//
//   - TestProp_LeaseStoreRoundTrip: the SQLite-backed dhcp.LeaseStore
//     (LocalDBLeaseStore) against a map, through saves, overwrites, deletes,
//     loads and process restarts (close + reopen of the same file).
//   - TestProp_HandlerRestartsFromLocalDB: the real dhcp.Handler persisting
//     into the real LocalDB; a restart must resume with exactly the leases
//     the previous process held.
//   - TestProp_LedgerTracksHandler: handler -> DHCPLeaseForwarder (renew
//     debounce) -> outbox -> the ledger fold, under a fake clock
//     (testing/synctest), checking that what global would show agrees with
//     what the handler is actually leasing.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"testing"
	"testing/synctest"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
)

// closeLocalDBForProp closes the SQLite handle the way process exit does, so
// a reopen of the same file is a real restart.
func closeLocalDBForProp(db *LocalDB) error {
	sqlDB, err := db.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func leasePropMAC(i int) net.HardwareAddr { return net.HardwareAddr{0x02, 0, 0, 0, 0, byte(i + 1)} }

// ---------------------------------------------------------------------------

func TestProp_LeaseStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Expiries are written under several UTC offsets: a lease saved before a
	// DST or host time zone change must be judged by its instant, not by how
	// its timestamp reads as text.
	zones := []*time.Location{time.UTC, time.Local, time.FixedZone("east", 14*3600), time.FixedZone("west", -12*3600)}
	var saves, loads, reopens, expiredSeen int

	rapid.Check(t, func(rt *rapid.T) {
		db.PurgeDHCPLeases()
		store := db.NewLeaseStore()
		type row struct {
			ip      string
			expires time.Time
		}
		model := map[string]row{}

		for step, n := 0, rapid.IntRange(1, 30).Draw(rt, "steps"); step < n; step++ {
			mac := leasePropMAC(rapid.IntRange(0, 4).Draw(rt, "mac"))
			switch rapid.IntRange(0, 5).Draw(rt, "op") {
			case 0, 1, 2:
				ip := net.IPv4(10, 9, 0, byte(rapid.IntRange(10, 14).Draw(rt, "ip")))
				// The store judges expiry against the wall clock, so keep
				// every expiry well away from now; the load step skips the
				// case if one comes close anyway (a stalled runner).
				offset := time.Duration(rapid.SampledFrom([]int{-86400, -3600, -600, 600, 3600, 86400}).Draw(rt, "expiresInSeconds")) * time.Second
				expires := time.Now().Add(offset).Add(time.Duration(rapid.IntRange(0, 999999999).Draw(rt, "nanos"))).In(rapid.SampledFrom(zones).Draw(rt, "zone"))
				store.SaveLease(mac, ip, expires)
				model[mac.String()] = row{ip.String(), expires}
				saves++
			case 3:
				store.DeleteLease(mac)
				delete(model, mac.String())
			case 4:
				if err := closeLocalDBForProp(db); err != nil {
					rt.Fatalf("close: %v", err)
				}
				db, err = OpenLocalDB(path)
				if err != nil {
					rt.Fatalf("reopen: %v", err)
				}
				db.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
				store = db.NewLeaseStore()
				reopens++
			case 5:
				before := time.Now()
				got := map[string]string{}
				for _, l := range store.LoadLeases() {
					got[l.MAC.String()] = l.IP.String() + " " + l.ExpiresAt.UTC().Format(time.RFC3339Nano)
				}
				active := db.CountActiveDHCPLeases()
				after := time.Now()
				want := map[string]string{}
				for k, r := range model {
					switch {
					case r.expires.After(after.Add(2 * time.Second)):
						want[k] = r.ip + " " + r.expires.UTC().Format(time.RFC3339Nano)
					case r.expires.Before(before.Add(-2 * time.Second)):
						// Load purges expired rows from disk.
						delete(model, k)
						expiredSeen++
					default:
						rt.Skip("a lease expired while the store was being read")
					}
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					rt.Fatalf("step %d: LoadLeases\n  store: %v\n  model: %v", step, got, want)
				}
				if int(active) != len(want) {
					rt.Fatalf("step %d: CountActiveDHCPLeases = %d, want %d", step, active, len(want))
				}
				loads++
			}
		}
	})
	t.Logf("coverage: %d saves, %d loads, %d reopens, %d expired rows observed at load", saves, loads, reopens, expiredSeen)
}

// ---------------------------------------------------------------------------

func leasePropHandler(store dhcp.LeaseStore, onEvent func(dhcp.LeaseEvent), lease time.Duration, reserve func(string) net.IP) *dhcp.Handler {
	h := dhcp.NewHandler(dhcp.Config{
		ServerIP:      net.IPv4(10, 9, 0, 2),
		LeaseDuration: lease,
		LeaseStore:    store,
		OnLeaseEvent:  onEvent,
		ReserveIP:     reserve,
		PoolConfig: dhcp.PoolConfig{
			Subnet:     net.IPNet{IP: net.IPv4(10, 9, 0, 0), Mask: net.CIDRMask(24, 32)},
			RangeStart: net.IPv4(10, 9, 0, 10),
			RangeEnd:   net.IPv4(10, 9, 0, 12),
			Gateway:    net.IPv4(10, 9, 0, 1),
		},
	})
	h.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

func leasePropPacket(typ dhcpv4.MessageType, mac net.HardwareAddr, ciaddr net.IP) *dhcpv4.DHCPv4 {
	mods := []dhcpv4.Modifier{dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(typ)}
	if ciaddr != nil {
		mods = append(mods, dhcpv4.WithClientIP(ciaddr))
	}
	pkt, err := dhcpv4.New(mods...)
	if err != nil {
		panic(err)
	}
	return pkt
}

func leaseTable(h *dhcp.Handler) map[string]string {
	out := map[string]string{}
	for _, l := range h.Leases() {
		out[l.MAC] = l.IP + " " + l.Expiry.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func TestProp_HandlerRestartsFromLocalDB(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	var restarts, grants int
	rapid.Check(t, func(rt *rapid.T) {
		db.PurgeDHCPLeases()
		reserved := map[string]net.IP{}
		reserve := func(mac string) net.IP { return reserved[mac] }
		h := leasePropHandler(db.NewLeaseStore(), nil, time.Hour, reserve)
		for step, n := 0, rapid.IntRange(1, 25).Draw(rt, "steps"); step < n; step++ {
			mac := leasePropMAC(rapid.IntRange(0, 4).Draw(rt, "mac"))
			switch rapid.IntRange(0, 4).Draw(rt, "op") {
			case 0, 1:
				if r := h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeDiscover, mac, nil)); r != nil {
					grants++
				}
			case 2:
				h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeRequest, mac, net.IPv4(10, 9, 0, byte(rapid.IntRange(5, 13).Draw(rt, "ciaddr")))))
			case 3:
				if rapid.Bool().Draw(rt, "reserve") {
					reserved[mac.String()] = net.IPv4(10, 9, 0, byte(5+rapid.IntRange(0, 4).Draw(rt, "reservedHost")))
					for other, ip := range reserved {
						if other != mac.String() && ip.Equal(reserved[mac.String()]) {
							delete(reserved, other)
						}
					}
				} else {
					delete(reserved, mac.String())
				}
			case 4:
				before := leaseTable(h)
				if rapid.Bool().Draw(rt, "reopenFile") {
					if err := closeLocalDBForProp(db); err != nil {
						rt.Fatalf("close: %v", err)
					}
					if db, err = OpenLocalDB(path); err != nil {
						rt.Fatalf("reopen: %v", err)
					}
				}
				h = leasePropHandler(db.NewLeaseStore(), nil, time.Hour, reserve)
				if after := leaseTable(h); fmt.Sprint(after) != fmt.Sprint(before) {
					rt.Fatalf("step %d: leases changed across a restart\n  before: %v\n  after:  %v", step, before, after)
				}
				restarts++
			}
		}
	})
	t.Logf("coverage: %d restarts, %d grants", restarts, grants)
}

// ---------------------------------------------------------------------------

type ledgerPropOp struct {
	kind    int // 0 discover, 1 request, 2 DORA, 3 advance, 4 restart
	mac     int
	want    int
	advance time.Duration
}

type ledgerPropRow struct {
	ip      string
	expires time.Time
}

func TestProp_LedgerTracksHandler(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	const lease = 10 * time.Minute
	advances := []time.Duration{time.Second, lease / 8, lease / 4, lease/2 - time.Second, lease/2 + time.Second, 3 * lease / 4, 7 * lease / 8, lease + 2*time.Second, 2 * lease}
	var staleSeen, suppressed, emitted, steps int

	rapid.Check(t, func(rt *rapid.T) {
		// Draw the whole history up front: rapid must not be driven from
		// inside the synctest bubble.
		ops := make([]ledgerPropOp, rapid.IntRange(1, 40).Draw(rt, "steps"))
		for i := range ops {
			ops[i] = ledgerPropOp{
				kind:    rapid.SampledFrom([]int{0, 1, 1, 2, 2, 3, 3, 3, 4}).Draw(rt, "kind"),
				mac:     rapid.IntRange(0, 3).Draw(rt, "mac"),
				want:    rapid.IntRange(0, 3).Draw(rt, "want"),
				advance: rapid.SampledFrom(advances).Draw(rt, "advance"),
			}
		}
		var failure string
		synctest.Test(t, func(*testing.T) {
			cache := NewCache(nil)
			fwd := NewDHCPLeaseForwarder(cache, lease)
			store := &memLeaseStore{rows: map[string]dhcp.PersistedLease{}}
			raw := 0
			onEvent := func(ev dhcp.LeaseEvent) {
				raw++
				fwd.OnLeaseEvent(ev)
			}
			h := leasePropHandler(store, onEvent, lease, nil)
			ledger := map[string]ledgerPropRow{} // open rows by MAC, one worker

			for step, op := range ops {
				mac := leasePropMAC(op.mac)
				switch op.kind {
				case 0:
					h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeDiscover, mac, nil))
				case 1:
					// Renew whatever the MAC holds, or ask for an address.
					ip := net.IPv4(10, 9, 0, byte(10+op.want))
					for _, l := range h.Leases() {
						if l.MAC == mac.String() && op.want < 3 {
							ip = net.ParseIP(l.IP)
						}
					}
					h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeRequest, mac, ip))
				case 2:
					if offer := h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeDiscover, mac, nil)); offer != nil {
						h.HandlePacket(leasePropPacket(dhcpv4.MessageTypeRequest, mac, offer.YourIPAddr))
					}
				case 3:
					time.Sleep(op.advance)
				case 4:
					h = leasePropHandler(store, onEvent, lease, nil)
				}

				// Deliver the outbox to the ledger, folding as
				// store.RecordDHCPLeaseEvent documents.
				pending, _ := cache.DrainOutbox()
				for _, p := range pending {
					if p.Obs.Event != "dhcp_lease" {
						continue
					}
					var d dhcpLeaseObservation
					if err := json.Unmarshal(p.Obs.Data, &d); err != nil {
						failure = fmt.Sprintf("step %d: observation does not decode: %v", step, err)
						return
					}
					expires, err := time.Parse(time.RFC3339, d.ExpiresAt)
					if err != nil {
						failure = fmt.Sprintf("step %d: expires_at %q: %v", step, d.ExpiresAt, err)
						return
					}
					emitted++
					if d.Event == "delete" {
						if r, ok := ledger[d.MAC]; ok && r.ip == d.IP {
							delete(ledger, d.MAC)
						}
						continue
					}
					for other, r := range ledger {
						if other != d.MAC && r.ip == d.IP {
							delete(ledger, other)
						}
					}
					ledger[d.MAC] = ledgerPropRow{d.IP, expires}
				}
				suppressed += raw - len(pending)
				raw = 0
				steps++

				now := time.Now()
				live := map[string]string{}
				for _, l := range h.Leases() {
					live[l.MAC] = l.IP
					row, ok := ledger[l.MAC]
					if !ok || row.ip != l.IP {
						failure = fmt.Sprintf("step %d: handler leases %s to %s but the ledger has no open row for it (ledger: %v)", step, l.IP, l.MAC, ledger)
						return
					}
					// expires_at travels as RFC 3339 seconds, so allow the
					// truncation.
					//
					// Not asserted: the forwarder's renew debounce drops a
					// renew's new expiry, so the ledger's open row for a live
					// lease can already read as expired. Only counted.
					if row.expires.Add(time.Second).Before(now) {
						staleSeen++
					}
				}
				for mac, row := range ledger {
					if row.expires.After(now) && live[mac] != row.ip {
						failure = fmt.Sprintf("step %d: ledger shows %s open on %s until %s but the handler holds no such lease (%v)", step, mac, row.ip, row.expires.Format(time.TimeOnly), live)
						return
					}
				}
			}
		})
		if failure != "" {
			rt.Fatalf("%s", failure)
		}
	})
	t.Logf("coverage: %d steps, %d observations delivered, %d lease events debounced away, %d live-lease checks that found the ledger row already expired", steps, emitted, suppressed, staleSeen)
}

type memLeaseStore struct {
	rows map[string]dhcp.PersistedLease
}

func (s *memLeaseStore) SaveLease(mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	s.rows[mac.String()] = dhcp.PersistedLease{MAC: mac, IP: ip, ExpiresAt: expiry}
}
func (s *memLeaseStore) DeleteLease(mac net.HardwareAddr) { delete(s.rows, mac.String()) }
func (s *memLeaseStore) LoadLeases() []dhcp.PersistedLease {
	keys := make([]string, 0, len(s.rows))
	for k := range s.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]dhcp.PersistedLease, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.rows[k])
	}
	return out
}
