package dhcp

// Property test for two site workers answering DHCP on one segment.
//
// Two real Handlers, each with its own lease table and LeaseStore, serve a
// pool of RFC 2131 clients. The clients are the ground truth: the property
// that matters on the wire is what addresses the hosts end up configured
// with, not what either server's table says.
//
// Deployment shapes, drawn per case:
//
//   - both workers active (every broadcast reaches both, a unicast renewal
//     only the server that granted the lease), or active/standby behind a VIP
//     (one server at a time; a handoff builds the new holder's Handler from
//     its own LeaseStore, as startSiteServices does);
//   - the dynamic range split into operator-assigned per-worker slices (the
//     OOB shape, where a worker also adopts a lease for an address in its
//     own slice), or into the shares each worker derives from its position
//     among the site's registered workers (PoolConfig.WorkerShare: the
//     fabric ephemeral and relay pools);
//   - a direct segment, where a server's ARP probe sees a configured host, or
//     a relayed one, where it cannot.
//
// Properties, after every step:
//
//	dup-address          no two hosts are configured with one address, in
//	                     every shape: each dynamic address has one worker
//	                     that may allocate it
//	reservation          a host whose reservation the answering server knows
//	                     is given exactly that address, before and after a
//	                     handoff or restart
//	ack-other-address    an ACK carries the address the REQUEST asked for
//	discover-unanswered  a DISCOVER that reaches a serving worker with a free
//	                     address is offered one
//	selected-offer-not-acked
//	                     the worker whose OFFER the host selects ACKs it
//	reply-from-unselected-server
//	                     a REQUEST that names one worker's server identifier
//	                     is answered by that worker only
//
// DHCP_PAIR_SHAPE=oob|fabric|vip pins the shape for a deep run.

import (
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"pgregory.net/rapid"
)

var (
	pairStatMu sync.Mutex
	pairStat   = map[string]int64{}
)

func pairCount(key string) {
	pairStatMu.Lock()
	pairStat[key]++
	pairStatMu.Unlock()
}

const (
	pairLease   = 10 * time.Minute
	pairClients = 5
	pairIface   = "oob0"
)

var (
	pairSubnet  = net.IPNet{IP: net.IPv4(10, 9, 0, 0).To4(), Mask: net.CIDRMask(24, 32)}
	pairGateway = net.IPv4(10, 9, 0, 1).To4()
)

func pairAddr(d byte) net.IP { return net.IPv4(10, 9, 0, d).To4() }

type pairClient struct {
	mac    net.HardwareAddr
	ip     net.IP // configured address, nil when unconfigured
	server int    // server that granted the lease
	rem    time.Duration
	silent bool // does not answer ARP (booting, firewalled)
}

type pairServer struct {
	idx      int
	serverIP net.IP
	up       bool
	h        *Handler
	store    *propStore
	resv     map[string]net.IP // this worker's view of the reservations
	lo, hi   byte              // the dynamic range in this worker's pool config
}

type pairWorld struct {
	t          *rapid.T
	bothActive bool
	sliced     bool
	relayed    bool
	holder     int
	handoffs   int
	srv        []*pairServer
	cl         []*pairClient
	truth      map[string]net.IP // the reservations global holds
	trace      []string
}

func (w *pairWorld) pick(n int, label string) int {
	if n <= 1 {
		return 0
	}
	nbits := bits.Len(uint(n-1)) + 3
	v := 0
	for i := 0; i < nbits; i++ {
		if rapid.Bool().Draw(w.t, label) {
			v |= 1 << i
		}
	}
	return v % n
}

func (w *pairWorld) logf(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf(format, args...))
}

func (w *pairWorld) violate(class, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.t.Fatalf("PROPERTY VIOLATION [%s]: %s\nshape: bothActive=%v sliced=%v relayed=%v\n--- trace ---\n%s",
		class, msg, w.bothActive, w.sliced, w.relayed, strings.Join(w.trace, "\n"))
}

// arp is what an ARP probe on the segment observes: a configured host that
// answers. A relayed segment is not on the worker's L2, so the probe sees
// nothing.
func (w *pairWorld) arp(ip net.IP) bool {
	if w.relayed {
		return false
	}
	for _, c := range w.cl {
		if c.ip != nil && c.ip.Equal(ip) && !c.silent {
			return true
		}
	}
	return false
}

func (w *pairWorld) start(s *pairServer) {
	resv := func(mac string) net.IP { return s.resv[mac] }
	pool := PoolConfig{Subnet: pairSubnet, RangeStart: pairAddr(s.lo), RangeEnd: pairAddr(s.hi), Gateway: pairGateway}
	cfg := Config{
		ServerIP: s.serverIP, DNS: []net.IP{pairGateway}, LeaseDuration: pairLease, HTTPBootPort: 8080,
		ReserveIP: resv, LeaseStore: s.store, ARPProbe: w.arp,
	}
	if !w.sliced {
		// Both workers build one pool config from the site snapshot and
		// each keeps its share, as the site daemon does.
		cfg.PoolConfig = pool.WorkerShare(s.idx, len(w.srv))
	}
	s.h = NewHandler(cfg)
	s.h.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w.sliced {
		s.h.SetIfaceParams(pairIface, IfaceParams{
			ServerIP: s.serverIP, ARPProbe: w.arp, PoolConfig: pool,
			DynamicSlices: []DynamicSlice{{Start: pairAddr(100), End: pairAddr(107)}, {Start: pairAddr(108), End: pairAddr(115)}},
		})
	}
	s.up = true
}

func (w *pairWorld) handle(s *pairServer, req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	// Through the wire encoding, as the socket layer delivers it.
	parsed, err := dhcpv4.FromBytes(req.ToBytes())
	if err != nil {
		w.t.Fatalf("encode request: %v", err)
	}
	if w.sliced {
		return s.h.HandlePacketOn(parsed, pairIface)
	}
	return s.h.HandlePacket(parsed)
}

// receivers are the servers a broadcast from the segment reaches.
func (w *pairWorld) receivers() []*pairServer {
	var out []*pairServer
	for _, s := range w.srv {
		if !s.up {
			continue
		}
		if w.bothActive || s.idx == w.holder {
			out = append(out, s)
		}
	}
	return out
}

func (w *pairWorld) packet(mt dhcpv4.MessageType, c *pairClient, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	all := []dhcpv4.Modifier{dhcpv4.WithMessageType(mt), dhcpv4.WithHwAddr(c.mac)}
	if w.relayed {
		all = append(all, dhcpv4.WithGatewayIP(pairAddr(250)))
	}
	p, err := dhcpv4.New(append(all, mods...)...)
	if err != nil {
		w.t.Fatalf("build packet: %v", err)
	}
	return p
}

func (w *pairWorld) configure(c *pairClient, s *pairServer, ack *dhcpv4.DHCPv4, asked net.IP) {
	got := ack.YourIPAddr.To4()
	if asked != nil && !got.Equal(asked) {
		w.violate("ack-other-address", "server %d ACKed %s to %s, which asked for %s", s.idx, got, c.mac, asked)
	}
	if r := s.resv[c.mac.String()]; r != nil && !got.Equal(r) {
		w.violate("reservation", "server %d holds reservation %s for %s and ACKed %s", s.idx, r, c.mac, got)
	}
	c.ip, c.server, c.rem = got, s.idx, pairLease
	pairCount("client.configured")
	w.logf("  %s configured with %s by server %d", c.mac, got, s.idx)
}

func (w *pairWorld) discover(c *pairClient) {
	recv := w.receivers()
	if len(recv) == 0 {
		return
	}
	c.ip = nil // a client restarting DHCP gives up its address
	type offer struct {
		s  *pairServer
		ip net.IP
	}
	var offers []offer
	for _, s := range recv {
		if rep := w.handle(s, w.packet(dhcpv4.MessageTypeDiscover, c)); rep != nil && rep.MessageType() == dhcpv4.MessageTypeOffer {
			offers = append(offers, offer{s, rep.YourIPAddr.To4()})
		}
	}
	w.logf("%s DISCOVER -> %d offer(s)", c.mac, len(offers))
	if len(offers) == 0 {
		w.violate("discover-unanswered", "%s sent DISCOVER to %d serving worker(s) and got no OFFER", c.mac, len(recv))
		return
	}
	pairCount("client.discover")
	if len(offers) > 1 && !offers[0].ip.Equal(offers[1].ip) {
		pairCount("client.offers_differ")
	}
	chosen := offers[w.pick(len(offers), "offer")]
	req := w.packet(dhcpv4.MessageTypeRequest, c,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(chosen.ip)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(chosen.s.serverIP)))
	var ack *dhcpv4.DHCPv4
	for _, s := range recv {
		rep := w.handle(s, req)
		if rep == nil {
			continue
		}
		if s != chosen.s {
			// RFC 2131 4.3.2: a server the client did not select stays
			// silent. A NAK from it would restart the client, and with two
			// workers answering every REQUEST no host would ever configure.
			w.violate("reply-from-unselected-server", "%s selected server %d (REQUEST carries its server identifier) and server %d answered with %s",
				c.mac, chosen.s.idx, s.idx, rep.MessageType())
			continue
		}
		if rep.MessageType() == dhcpv4.MessageTypeAck {
			ack = rep
		}
	}
	if ack == nil {
		// Nothing happens between the OFFER and the REQUEST that selects it.
		w.violate("selected-offer-not-acked", "%s selected server %d's offer of %s and got no ACK from it", c.mac, chosen.s.idx, chosen.ip)
		return
	}
	w.configure(c, chosen.s, ack, chosen.ip)
}

// request is a REQUEST for the address the client holds: a unicast renewal
// to the granting server, or a broadcast (REBINDING, INIT-REBOOT) to all.
func (w *pairWorld) request(c *pairClient, broadcast bool) {
	if c.ip == nil {
		return
	}
	var recv []*pairServer
	if broadcast {
		recv = w.receivers()
	} else {
		for _, s := range w.receivers() {
			// The VIP names whoever holds it; a both-active worker is
			// addressed by its own server identifier.
			if !w.bothActive || s.idx == c.server {
				recv = append(recv, s)
			}
		}
	}
	var replies []*dhcpv4.DHCPv4
	var from []*pairServer
	for _, s := range recv {
		if rep := w.handle(s, w.packet(dhcpv4.MessageTypeRequest, c, dhcpv4.WithClientIP(c.ip))); rep != nil {
			replies, from = append(replies, rep), append(from, s)
		}
	}
	w.logf("%s REQUEST %s (broadcast=%v) -> %d reply(ies)", c.mac, c.ip, broadcast, len(replies))
	if len(replies) == 0 {
		return
	}
	i := w.pick(len(replies), "reply")
	if replies[i].MessageType() == dhcpv4.MessageTypeAck {
		pairCount("client.renewed")
		w.configure(c, from[i], replies[i], c.ip)
		return
	}
	pairCount("client.naked")
	w.logf("  %s NAKed by server %d, restarting", c.mac, from[i].idx)
	c.ip = nil
}

func (w *pairWorld) age(d time.Duration) {
	for _, s := range w.srv {
		if s.h != nil {
			s.h.mu.Lock()
			for _, l := range s.h.leases {
				l.Expiry = l.Expiry.Add(-d)
			}
			s.h.mu.Unlock()
		}
		s.store.mu.Lock()
		for k, r := range s.store.rows {
			r.ExpiresAt = r.ExpiresAt.Add(-d)
			s.store.rows[k] = r
		}
		s.store.mu.Unlock()
	}
	for _, c := range w.cl {
		if c.ip == nil {
			continue
		}
		if c.rem -= d; c.rem <= 0 {
			w.logf("%s lease on %s ran out, address dropped", c.mac, c.ip)
			c.ip = nil
		}
	}
}

func (w *pairWorld) check() {
	byIP := map[string][]*pairClient{}
	for _, c := range w.cl {
		if c.ip != nil {
			byIP[c.ip.String()] = append(byIP[c.ip.String()], c)
		}
	}
	ips := make([]string, 0, len(byIP))
	for ip := range byIP {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		cs := byIP[ip]
		if len(cs) < 2 {
			continue
		}
		w.violate("dup-address", "%s (lease from server %d) and %s (lease from server %d) are both configured with %s",
			cs[0].mac, cs[0].server, cs[1].mac, cs[1].server, ip)
	}
}

func pairRun(t *rapid.T) {
	w := &pairWorld{t: t, truth: map[string]net.IP{}}
	w.bothActive = rapid.Bool().Draw(t, "bothActive")
	w.sliced = rapid.Bool().Draw(t, "sliced")
	w.relayed = rapid.Bool().Draw(t, "relayed")
	switch os.Getenv("DHCP_PAIR_SHAPE") {
	case "oob": // an OOB segment: both active, operator slices, direct
		w.bothActive, w.sliced, w.relayed = true, true, false
	case "fabric": // a relay pool: both active, derived shares, relayed
		w.bothActive, w.sliced, w.relayed = true, false, true
	case "vip":
		w.bothActive = false
	}
	pairCount(fmt.Sprintf("shape.bothActive=%v.sliced=%v.relayed=%v", w.bothActive, w.sliced, w.relayed))
	for i := 0; i < 2; i++ {
		s := &pairServer{idx: i, serverIP: pairAddr(byte(2 + i)), store: &propStore{rows: map[string]PersistedLease{}}, resv: map[string]net.IP{}, lo: 100, hi: 115}
		if !w.bothActive {
			s.serverIP = pairAddr(2) // the VIP
		}
		if w.sliced {
			// Eight addresses each: a DISCOVER holds an address on both
			// workers for one lease time, whichever offer the host takes.
			s.lo, s.hi = byte(100+8*i), byte(107+8*i)
		}
		w.srv = append(w.srv, s)
	}
	for i := 0; i < pairClients; i++ {
		w.cl = append(w.cl, &pairClient{mac: net.HardwareAddr{0x02, 0, 0, 0, 0x50, byte(i + 1)}})
	}
	// Reservations for some hosts, in the static band below the ranges.
	for i, c := range w.cl {
		if rapid.Bool().Draw(t, "reserved") {
			w.truth[c.mac.String()] = pairAddr(byte(10 + i))
		}
	}
	for _, s := range w.srv {
		for k, v := range w.truth {
			s.resv[k] = v
		}
		if w.bothActive || s.idx == w.holder {
			w.start(s)
		}
	}

	steps := rapid.IntRange(1, 60).Draw(t, "steps")
	for i := 0; i < steps; i++ {
		c := w.cl[w.pick(len(w.cl), "client")]
		switch w.pick(16, "action") {
		case 0, 1, 2, 3:
			w.discover(c)
		case 4, 5, 6:
			w.request(c, false)
		case 7, 8:
			w.request(c, true)
		case 9, 10, 11:
			d := time.Duration(1+w.pick(4, "minutes")) * 3 * time.Minute
			w.logf("clock +%s", d)
			w.age(d)
		case 12:
			if c.ip != nil {
				w.logf("%s powers off holding %s", c.mac, c.ip)
				c.ip = nil
			}
		case 13:
			c.silent = !c.silent
		case 14:
			if w.bothActive {
				// One worker restarts: a new Handler from its LeaseStore.
				s := w.srv[w.pick(2, "restart")]
				w.logf("server %d restarts", s.idx)
				pairCount("server.restart")
				w.start(s)
			} else {
				// keepalived moves the VIP: the old holder stops serving.
				w.srv[w.holder].up = false
				w.holder = 1 - w.holder
				w.handoffs++
				w.logf("VIP moves to server %d", w.holder)
				pairCount("server.handoff")
				w.start(w.srv[w.holder])
			}
		case 15:
			// A reservation appears in NetBox; the workers apply the new
			// snapshot one after the other.
			mac := c.mac.String()
			if w.truth[mac] == nil {
				w.truth[mac] = pairAddr(byte(20 + w.pick(pairClients, "newResv")))
				dup := 0
				for _, v := range w.truth {
					if v.Equal(w.truth[mac]) {
						dup++
					}
				}
				if dup > 1 {
					delete(w.truth, mac)
					break
				}
				w.logf("reservation %s -> %s", mac, w.truth[mac])
			}
			s := w.srv[w.pick(2, "resvServer")]
			if v := w.truth[mac]; v != nil {
				s.resv[mac] = v
			}
		}
		w.check()
	}
	pairCount("cases")
}

func TestProp_DHCPPair(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	rapid.Check(t, pairRun)
	pairStatMu.Lock()
	defer pairStatMu.Unlock()
	keys := make([]string, 0, len(pairStat))
	for k := range pairStat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "dhcp.%-58s %d\n", k, pairStat[k])
	}
	t.Logf("coverage (aggregate over all cases, shrinking included):\n%s", b.String())
}
