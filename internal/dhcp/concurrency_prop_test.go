package dhcp

// Deterministic-interleaving property for the allocator's ARP window.
//
// allocateIP releases h.mu while it ARP-probes a candidate address, so every
// probe is a point where another packet can run to completion (or to its own
// probe). server4 handles each packet on its own goroutine, so this happens
// in production whenever two clients boot at once. The property turns the
// probe into a scheduling point: each probe parks until the generated
// schedule releases it, and the schedule decides, step by step, whether to
// start the next packet or resume a parked one. Exactly one goroutine is
// runnable at any time, so a schedule replays exactly and shrinks.
//
// TestRace_HandlerParallelTraffic is the unscheduled companion: real
// parallelism for the race detector (run with -race).

import (
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"pgregory.net/rapid"
)

type schedPark struct {
	owner   int
	ip      pIP
	release chan struct{}
}

type schedDone struct {
	idx   int
	reply *dhcpv4.DHCPv4
}

type schedPacket struct {
	mac     int
	request bool
	want    pIP // REQUEST only: ciaddr
}

func TestProp_InterleavedAllocation(t *testing.T) {
	propQuietLogs(t)
	var parks, adopts, exhausted, sameMAC int
	rapid.Check(t, func(rt *rapid.T) {
		lo := a4(10, 9, 0, 10)
		size := rapid.IntRange(1, 4).Draw(rt, "rangeSize")
		hi := lo + pIP(size-1)
		sliced := rapid.Bool().Draw(rt, "sliced")
		squat := map[pIP]bool{}
		for ip := lo; ip <= hi; ip++ {
			if rapid.IntRange(0, 4).Draw(rt, "squat") == 0 {
				squat[ip] = true
			}
		}

		parked := make(chan *schedPark)
		done := make(chan schedDone)
		store := &propStore{rows: map[string]PersistedLease{}}
		var events []LeaseEvent
		// The probe runs on the packet's goroutine. Each packet arrives on
		// its own interface name, registered with its own prober closure,
		// so a park names the packet it belongs to.
		proberFor := func(owner int) func(net.IP) bool {
			return func(ip net.IP) bool {
				p := &schedPark{owner: owner, ip: toPIP(ip), release: make(chan struct{})}
				parked <- p
				<-p.release
				return squat[p.ip]
			}
		}
		h := NewHandler(Config{
			ServerIP:      propOOBServer.ip(),
			LeaseDuration: time.Hour,
			LeaseStore:    store,
			OnLeaseEvent:  func(ev LeaseEvent) { events = append(events, ev) },
		})
		h.SetLogger(slogDiscard())

		nPackets := rapid.IntRange(2, 6).Draw(rt, "packets")
		packets := make([]schedPacket, nPackets)
		for i := range packets {
			packets[i].mac = rapid.IntRange(0, 2).Draw(rt, "mac")
			if rapid.IntRange(0, 3).Draw(rt, "isRequest") == 0 {
				packets[i].request = true
				packets[i].want = lo + pIP(rapid.IntRange(0, size).Draw(rt, "want"))
			}
			// One interface registration per packet: identical segment
			// config, its own prober, so a park names its packet.
			params := IfaceParams{
				ARPProbe: proberFor(i),
				PoolConfig: PoolConfig{
					Subnet:      net.IPNet{IP: a4(10, 9, 0, 0).ip(), Mask: net.CIDRMask(24, 32)},
					RangeStart:  lo.ip(),
					RangeEnd:    hi.ip(),
					Gateway:     a4(10, 9, 0, 1).ip(),
					Promiscuous: true,
				},
			}
			if sliced {
				params.DynamicSlices = []DynamicSlice{{Start: lo.ip(), End: hi.ip()}, {Start: a4(10, 9, 0, 20).ip(), End: a4(10, 9, 0, 22).ip()}}
			}
			h.SetIfaceParams(fmt.Sprintf("p%d", i), params)
		}

		replies := make([]*dhcpv4.DHCPv4, nPackets)
		finished := make([]bool, nPackets)
		var waiting []*schedPark
		wait := func() {
			select {
			case p := <-parked:
				waiting = append(waiting, p)
				parks++
			case d := <-done:
				replies[d.idx] = d.reply
				finished[d.idx] = true
			case <-time.After(20 * time.Second):
				rt.Fatalf("deadlock: no packet made progress")
			}
		}
		next := 0
		for next < nPackets || len(waiting) > 0 {
			choices := len(waiting)
			if next < nPackets {
				choices++
			}
			c := rapid.IntRange(0, choices-1).Draw(rt, "schedule")
			if c == len(waiting) {
				idx := next
				next++
				pk := packets[idx]
				mods := []dhcpv4.Modifier{dhcpv4.WithHwAddr(propMAC(pk.mac))}
				if pk.request {
					mods = append(mods, dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest), dhcpv4.WithClientIP(pk.want.ip()))
				} else {
					mods = append(mods, dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover))
				}
				req, err := dhcpv4.New(mods...)
				if err != nil {
					rt.Fatal(err)
				}
				go func() { done <- schedDone{idx, h.HandlePacketOn(req, fmt.Sprintf("p%d", idx))} }()
			} else {
				p := waiting[c]
				waiting = append(waiting[:c], waiting[c+1:]...)
				close(p.release)
			}
			wait()
		}
		for i, f := range finished {
			if !f {
				rt.Fatalf("packet %d never finished", i)
			}
		}

		// --- invariants ----------------------------------------------------
		final := map[int]pIP{}
		byIP := map[pIP]int{}
		for _, li := range h.Leases() {
			mac := propMACIndex(li.MAC)
			ip := toPIP(net.ParseIP(li.IP))
			if other, dup := byIP[ip]; dup {
				rt.Fatalf("%s is leased to MAC %d and MAC %d at once", ip, other, mac)
			}
			byIP[ip] = mac
			final[mac] = ip
			if ip < lo || ip > hi {
				rt.Fatalf("lease %s outside the range %s-%s", ip, lo, hi)
			}
		}
		macPackets := map[int]int{}
		for i, pk := range packets {
			macPackets[pk.mac]++
			r := replies[i]
			if r == nil {
				if !pk.request {
					exhausted++
					if _, has := final[pk.mac]; !has {
						for ip := lo; ip <= hi; ip++ {
							if _, held := byIP[ip]; !held && !squat[ip] {
								rt.Fatalf("packet %d: DISCOVER from MAC %d got no offer although %s is free and unprobed-in-use", i, pk.mac, ip)
							}
						}
					}
				}
				continue
			}
			if r.MessageType() == dhcpv4.MessageTypeNak {
				continue
			}
			yi := toPIP(r.YourIPAddr)
			if got, ok := final[pk.mac]; !ok || got != yi {
				rt.Fatalf("packet %d: MAC %d was granted %s but its lease is %v (an offer no lease backs)", i, pk.mac, yi, final[pk.mac])
			}
			if pk.request {
				adopts++
				if yi != pk.want {
					rt.Fatalf("packet %d: REQUEST for %s ACKed with %s", i, pk.want, yi)
				}
			} else if squat[yi] {
				// A DISCOVER may still return a squatted address when a
				// REQUEST from the same MAC adopted it (adoption does not
				// probe, by design).
				adopted := false
				for _, other := range packets {
					if other.mac == pk.mac && other.request && other.want == yi {
						adopted = true
					}
				}
				if !adopted {
					rt.Fatalf("packet %d: DISCOVER granted %s although its ARP probe reports it in use", i, yi)
				}
			}
		}
		for _, n := range macPackets {
			if n > 1 {
				sameMAC++
			}
		}

		// Lease events fold to the final table; the store mirrors memory.
		fold := map[string]string{}
		for _, ev := range events {
			if ev.Event == "delete" {
				delete(fold, ev.MAC.String())
				continue
			}
			fold[ev.MAC.String()] = ev.IP.String()
		}
		mem := map[string]string{}
		h.mu.Lock()
		for k, l := range h.leases {
			mem[k] = l.IP.String()
		}
		h.mu.Unlock()
		disk := map[string]string{}
		for k, r := range store.rows {
			disk[k] = r.IP.String()
		}
		if fmt.Sprint(fold) != fmt.Sprint(mem) || fmt.Sprint(disk) != fmt.Sprint(mem) {
			rt.Fatalf("lease table %v, event fold %v, store %v", mem, fold, disk)
		}
	})
	t.Logf("coverage: %d probe parks, %d ACKed requests, %d unanswered DISCOVERs, %d histories with several packets from one MAC", parks, adopts, exhausted, sameMAC)
}

// TestRace_HandlerParallelTraffic drives the handler from many goroutines at
// once with hot-reloads and readers mixed in. Its value is under -race; the
// end-state checks are the ones that hold for any interleaving.
func TestRace_HandlerParallelTraffic(t *testing.T) {
	propQuietLogs(t)
	store := &propStore{rows: map[string]PersistedLease{}}
	var evMu sync.Mutex
	events := 0
	pools := func(size int) PoolConfig {
		return PoolConfig{
			Subnet:     net.IPNet{IP: a4(10, 1, 0, 0).ip(), Mask: net.CIDRMask(24, 32)},
			RangeStart: a4(10, 1, 0, 10).ip(),
			RangeEnd:   a4(10, 1, 0, byte(10+size-1)).ip(),
			Gateway:    a4(10, 1, 0, 1).ip(),
		}
	}
	h := NewHandler(Config{
		ServerIP:      propBaseServer.ip(),
		LeaseDuration: time.Hour,
		PoolConfig:    pools(8),
		LeaseStore:    store,
		ARPProbe: func(net.IP) bool {
			runtime.Gosched()
			return false
		},
		OnLeaseEvent: func(LeaseEvent) { evMu.Lock(); events++; evMu.Unlock() },
		OnACK:        func(string, ClientKind) {},
	})
	h.SetLogger(slogDiscard())
	oob := func(size int) IfaceParams {
		return IfaceParams{
			ServerIP: propOOBServer.ip(),
			PoolConfig: PoolConfig{
				Subnet:      net.IPNet{IP: a4(10, 9, 0, 0).ip(), Mask: net.CIDRMask(24, 32)},
				RangeStart:  a4(10, 9, 0, 10).ip(),
				RangeEnd:    a4(10, 9, 0, byte(10+size-1)).ip(),
				Gateway:     a4(10, 9, 0, 1).ip(),
				Promiscuous: true,
			},
			DynamicSlices: []DynamicSlice{{Start: a4(10, 9, 0, 10).ip(), End: a4(10, 9, 0, byte(10+size-1)).ip()}},
		}
	}
	h.SetIfaceParams(propOOBIface, oob(8))

	const workers, perWorker = 8, 300
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w) + 1))
			for i := 0; i < perWorker; i++ {
				mac := net.HardwareAddr{0x02, 0, 0, 0, 1, byte(rng.Intn(12))}
				iface := ""
				subnet := byte(1)
				if rng.Intn(2) == 0 {
					iface, subnet = propOOBIface, 9
				}
				switch rng.Intn(10) {
				case 0:
					h.UpdatePools(pools(6 + rng.Intn(3)))
				case 1:
					h.SetIfaceParams(propOOBIface, oob(6+rng.Intn(3)))
				case 2:
					h.Leases()
					h.LeaseCountsByPool()
					h.AnyPromiscuous()
					h.HasLease("10.1.0.10")
					h.MACForIP("10.9.0.10")
				case 3, 4:
					req, _ := dhcpv4.New(dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
						dhcpv4.WithClientIP(a4(10, subnet, 0, byte(10+rng.Intn(8))).ip()))
					h.HandlePacketOn(req, iface)
				default:
					req, _ := dhcpv4.New(dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover))
					h.HandlePacketOn(req, iface)
				}
			}
		}(w)
	}
	wg.Wait()

	seen := map[string]string{}
	leases := h.Leases()
	sort.Slice(leases, func(i, j int) bool { return leases[i].MAC < leases[j].MAC })
	for _, li := range leases {
		if other, dup := seen[li.IP]; dup {
			t.Fatalf("%s is leased to %s and %s at once", li.IP, other, li.MAC)
		}
		seen[li.IP] = li.MAC
	}
	h.mu.Lock()
	mem := len(h.leases)
	h.mu.Unlock()
	store.mu.Lock()
	disk := len(store.rows)
	store.mu.Unlock()
	if mem != disk {
		t.Fatalf("%d leases in memory, %d persisted", mem, disk)
	}
	t.Logf("coverage: %d live leases, %d lease events", len(leases), events)
}
