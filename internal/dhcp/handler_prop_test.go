package dhcp

// Model-based property test for the DHCP handler.
//
// A rapid-generated history of client packets (DISCOVER / REQUEST in every
// RFC 2131 client state / RELEASE / DECLINE / INFORM), clock advances,
// reservation edits, pool hot-reloads, ARP squatters and restarts is replayed
// against the real Handler and against propModel, an independent reference
// model written from the handler's doc comments and docs/user/boot-process.md.
// The model works on uint32 addresses and integer clock ticks and calls no
// production helper.
//
// Two kinds of check run after every step:
//
//   - differential: reply type, yiaddr, every reply option, boot pointer,
//     lease table (Leases, HasLease, MACForIP, LeaseCountsByPool), the
//     persisted store and the exact OnLeaseEvent stream must equal the
//     model's prediction;
//   - global invariants that hold whatever the model says: no address is
//     leased to two MACs, a reservation is honored and never handed to
//     another MAC, an address outside the serving subnet/range is never
//     offered, an ACK never carries a different address than the one the
//     REQUEST asked for, and a MAC that must not netboot never gets a boot
//     pointer.
//
// One configuration the handler deliberately does not guard is generated on
// purpose (reservationsMayOverlapRange): a reservation that lies inside a
// dynamic range. Until the reserved MAC asks for it, the allocator and
// adopt-in-place may lease that address to another MAC; granting the
// reservation then revokes that lease. The model reproduces this, and the
// "one address, one MAC" invariant holds throughout.

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Address universe (uint32, independent of the handler's net.IP helpers).

type pIP uint32

func a4(a, b, c, d byte) pIP { return pIP(a)<<24 | pIP(b)<<16 | pIP(c)<<8 | pIP(d) }

func (p pIP) ip() net.IP {
	if p == 0 {
		return nil
	}
	return net.IPv4(byte(p>>24), byte(p>>16), byte(p>>8), byte(p))
}

func (p pIP) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(p>>24), byte(p>>16), byte(p>>8), byte(p))
}

func toPIP(ip net.IP) pIP {
	v4 := ip.To4()
	if v4 == nil {
		return 0
	}
	return a4(v4[0], v4[1], v4[2], v4[3])
}

func maskOf(bits int) pIP { return pIP(^uint32(0) << (32 - uint(bits))) }

const (
	propTick     = 10 * time.Minute
	propHTTPPort = 8080
	propOOBIface = "oob0"
)

var (
	propBaseServer = a4(10, 1, 0, 2)
	propOOBServer  = a4(10, 9, 0, 2)
	propForeignSrv = a4(10, 1, 0, 99)
	propDNSBase    = []pIP{a4(1, 1, 1, 1)}
	propDNSOOB     = []pIP{a4(10, 9, 0, 53), a4(10, 9, 0, 54)}
	propNTPOOB     = []pIP{a4(10, 9, 0, 1)}

	// Addresses a REQUEST may ask for and a reservation may point at.
	propStaticIPs  = []pIP{a4(10, 1, 0, 5), a4(10, 1, 0, 6), a4(10, 9, 0, 5), a4(10, 9, 0, 6), a4(10, 2, 0, 5), a4(10, 3, 0, 1), a4(10, 77, 0, 5)}
	propOverlapIPs = []pIP{a4(10, 1, 0, 10), a4(10, 1, 0, 11), a4(10, 9, 0, 10), a4(10, 9, 0, 11), a4(10, 9, 0, 20), a4(10, 2, 0, 10)}
	propWantIPs    = func() []pIP {
		out := []pIP{a4(10, 1, 0, 1), a4(10, 1, 0, 9), a4(10, 9, 0, 9), a4(10, 9, 0, 21), a4(10, 9, 0, 22), a4(10, 3, 0, 0), a4(255, 255, 255, 255), a4(192, 168, 1, 10)}
		for d := byte(10); d <= 15; d++ {
			out = append(out, a4(10, 1, 0, d), a4(10, 9, 0, d), a4(10, 2, 0, d))
		}
		out = append(out, propStaticIPs...)
		return out
	}()
)

const propNumMACs = 6

func propMAC(i int) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0, 0, 0, 0, byte(i + 1)}
}

func propMACIndex(s string) int {
	for i := 0; i < propNumMACs; i++ {
		if propMAC(i).String() == s {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Model configuration.

type mPool struct {
	net      pIP // network address
	bits     int
	gateway  pIP
	hasRange bool
	lo, hi   pIP
}

func (p mPool) cidr() string    { return fmt.Sprintf("%s/%d", p.net, p.bits) }
func (p mPool) has(ip pIP) bool { return ip&maskOf(p.bits) == p.net }
func (p mPool) inRange(ip pIP) bool {
	return p.hasRange && ip >= p.lo && ip <= p.hi
}

type mPoolCfg struct {
	configured  bool
	direct      mPool
	relays      []mPool
	promiscuous bool
}

type mIface struct {
	registered bool
	serverIP   pIP // 0 = inherit base
	dns        bool
	ntp        bool
	leaseTicks int // 0 = inherit base
	sliced     bool
	arp        bool // has its own prober
	pools      mPoolCfg
}

type mView struct {
	serverIP   pIP
	dns, ntp   []pIP
	leaseTicks int
	sliced     bool
	arp        bool
	pools      mPoolCfg
}

type propWorld struct {
	base       mPoolCfg
	baseTicks  int
	oob        mIface
	gateKnown  bool
	gateBoot   bool
	arp        bool
	known      [propNumMACs]bool
	ineligible [propNumMACs]bool
	reserve    map[int]pIP
	squat      map[pIP]bool
}

func (w *propWorld) view(iface string) mView {
	v := mView{serverIP: propBaseServer, dns: propDNSBase, leaseTicks: w.baseTicks, arp: w.arp, pools: w.base}
	if iface != propOOBIface || !w.oob.registered {
		return v
	}
	if w.oob.serverIP != 0 {
		v.serverIP = w.oob.serverIP
	}
	if w.oob.dns {
		v.dns = propDNSOOB
	}
	if w.oob.ntp {
		v.ntp = propNTPOOB
	}
	if w.oob.leaseTicks != 0 {
		v.leaseTicks = w.oob.leaseTicks
	}
	if w.oob.arp {
		v.arp = true
	}
	v.sliced = w.oob.sliced
	v.pools = w.oob.pools
	return v
}

// servingPool is the documented poolForRequest resolution: nil pool when the
// segment is unconfigured or a relayed packet's giaddr matches nothing; a
// giaddr inside the direct subnet uses the direct pool; a /31 relay subnet
// derives gateway=giaddr and the single client address from the packet. A
// packet without giaddr uses the direct pool, unless renewCiaddr (a
// REQUEST's ciaddr; pass 0 for any other packet) lies in a relay subnet and
// not the direct one: a renew unicast from behind a relay, served from that
// relay subnet, whose relay on a /31 is the client's peer address.
func servingPool(pc mPoolCfg, giaddr, renewCiaddr pIP) *mPool {
	if !pc.configured {
		return nil
	}
	relayPool := func(r mPool, relay pIP) *mPool {
		p := r
		if r.bits == 31 {
			p.gateway = relay
			p.hasRange = true
			p.lo, p.hi = relay^1, relay^1
		}
		return &p
	}
	if giaddr == 0 {
		if renewCiaddr != 0 && !pc.direct.has(renewCiaddr) {
			for _, r := range pc.relays {
				if r.has(renewCiaddr) {
					return relayPool(r, renewCiaddr^1)
				}
			}
		}
		p := pc.direct
		return &p
	}
	if pc.direct.has(giaddr) {
		p := pc.direct
		return &p
	}
	for _, r := range pc.relays {
		if r.has(giaddr) {
			return relayPool(r, giaddr)
		}
	}
	return nil
}

// renewCiaddr is the ciaddr poolForRequest resolves a pool by: a REQUEST's,
// and nothing for any other packet.
func (pk propPacket) renewCiaddr() pIP {
	if pk.typ == dhcpv4.MessageTypeRequest {
		return pk.ciaddr
	}
	return 0
}

func (w *propWorld) label(ip pIP) string {
	if w.base.configured && w.base.direct.has(ip) {
		return w.base.direct.cidr()
	}
	for _, r := range w.base.relays {
		if r.has(ip) {
			return r.cidr()
		}
	}
	if w.oob.registered {
		if w.oob.pools.configured && w.oob.pools.direct.has(ip) {
			return w.oob.pools.direct.cidr()
		}
		for _, r := range w.oob.pools.relays {
			if r.has(ip) {
				return r.cidr()
			}
		}
	}
	return "none"
}

// ---------------------------------------------------------------------------
// Reference model.

type mLease struct {
	ip  pIP
	exp int
}

type mEvent struct {
	kind string
	mac  int
	ip   pIP
}

type propModel struct {
	w        *propWorld
	now      int
	leases   map[int]*mLease
	declined map[pIP]bool // addresses withheld after a DECLINE
	events   []mEvent
}

func (m *propModel) drop(mac int) {
	m.events = append(m.events, mEvent{"delete", mac, m.leases[mac].ip})
	delete(m.leases, mac)
}

func (m *propModel) live(mac int) *mLease {
	if l, ok := m.leases[mac]; ok && m.now < l.exp {
		return l
	}
	return nil
}

func (m *propModel) holder(ip pIP, except int) int {
	for mac, l := range m.leases {
		if mac != except && l.ip == ip && m.now < l.exp {
			return mac
		}
	}
	return -1
}

func (m *propModel) set(mac int, ip pIP, ticks int) {
	kind := "allocate"
	if l := m.live(mac); l != nil && l.ip == ip {
		kind = "renew"
	}
	m.leases[mac] = &mLease{ip: ip, exp: m.now + ticks}
	m.events = append(m.events, mEvent{kind, mac, ip})
}

func (m *propModel) reservation(mac int, p *mPool) (pIP, bool) {
	r, ok := m.w.reserve[mac]
	if !ok || !p.has(r) {
		return 0, false
	}
	return r, true
}

// allocate is the documented allocateIP: reservation first, revoking any
// other MAC's live lease on that address; a reservations-only pool serves
// nothing else; a live lease inside the pool's range is renewed; otherwise
// expired entries are swept and the lowest free, un-declined, un-squatted
// address of the range is granted.
func (m *propModel) allocate(mac int, res pIP, hasRes bool, p *mPool, v mView) (pIP, bool) {
	if hasRes {
		if other := m.holder(res, mac); other >= 0 {
			m.drop(other)
		}
		m.set(mac, res, v.leaseTicks)
		return res, true
	}
	if !p.hasRange {
		return 0, false
	}
	if l := m.live(mac); l != nil && p.inRange(l.ip) {
		l.exp = m.now + v.leaseTicks
		m.events = append(m.events, mEvent{"renew", mac, l.ip})
		return l.ip, true
	}
	for k, l := range m.leases {
		if m.now >= l.exp {
			m.events = append(m.events, mEvent{"delete", k, l.ip})
			delete(m.leases, k)
		}
	}
	for ip := p.lo; ip <= p.hi; ip++ {
		if m.holder(ip, -1) >= 0 {
			continue
		}
		if m.declined[ip] {
			continue
		}
		if v.arp && m.w.squat[ip] {
			continue
		}
		m.set(mac, ip, v.leaseTicks)
		return ip, true
	}
	return 0, false
}

type propArrival struct {
	iface  string
	giaddr pIP
}

type mReply struct {
	kind string // "none", "offer", "ack", "nak"
	ip   pIP
	pool *mPool
	view mView
}

func (m *propModel) admitted(mac int, v mView) bool {
	return !m.w.gateKnown || v.pools.promiscuous || m.w.known[mac]
}

func (m *propModel) discover(mac int, a propArrival) mReply {
	v := m.w.view(a.iface)
	if !m.admitted(mac, v) {
		return mReply{kind: "none"}
	}
	p := servingPool(v.pools, a.giaddr, 0)
	if p == nil {
		return mReply{kind: "none"}
	}
	res, hasRes := m.reservation(mac, p)
	ip, ok := m.allocate(mac, res, hasRes, p, v)
	if !ok {
		return mReply{kind: "none"}
	}
	return mReply{kind: "offer", ip: ip, pool: p, view: v}
}

// request is the documented handleRequest + classifyRequest: silent for
// another server's SELECTING request; a requested address is answered at
// exactly that address or NAKed; a REQUEST with no requested address
// allocates like a DISCOVER.
func (m *propModel) request(mac int, a propArrival, sid, opt50, ciaddr pIP) mReply {
	v := m.w.view(a.iface)
	if !m.admitted(mac, v) {
		return mReply{kind: "none"}
	}
	if sid != 0 && sid != v.serverIP {
		return mReply{kind: "none"}
	}
	p := servingPool(v.pools, a.giaddr, ciaddr)
	if p == nil {
		return mReply{kind: "none"}
	}
	res, hasRes := m.reservation(mac, p)
	want := opt50
	if want == 0 {
		want = ciaddr
	}
	nak := mReply{kind: "nak", view: v, pool: p}
	if want != 0 {
		switch {
		case hasRes:
			if res != want {
				return nak
			}
		case m.live(mac) != nil:
			l := m.live(mac)
			if l.ip != want {
				return nak
			}
			if !p.inRange(want) {
				// The MAC's live lease is not servable from this pool
				// (reservation removed, range reloaded away, client on
				// another segment): NAKed. The lease is not released; it
				// ends at expiry or when the client's re-DISCOVER
				// overwrites it.
				return nak
			}
		case !p.inRange(want):
			return nak
		case m.holder(want, mac) >= 0:
			return nak
		case !v.sliced:
			return nak
		default:
			m.leases[mac] = &mLease{ip: want, exp: m.now + v.leaseTicks}
			m.events = append(m.events, mEvent{"allocate", mac, want})
			return mReply{kind: "ack", ip: want, pool: p, view: v}
		}
	}
	ip, ok := m.allocate(mac, res, hasRes, p, v)
	if !ok {
		return mReply{kind: "none"}
	}
	return mReply{kind: "ack", ip: ip, pool: p, view: v}
}

// decline is RFC 2131 4.3.3: when the named address is this MAC's lease
// (expired but unswept counts), release it and keep it out of dynamic
// allocation. The quarantine lasts one lease duration of wall-clock time,
// which the harness's clock (it shifts lease expiries) cannot advance, so
// the model holds a quarantine until restart, which forgets it.
func (m *propModel) decline(mac int, a propArrival, sid, opt50 pIP) {
	v := m.w.view(a.iface)
	if !m.admitted(mac, v) || (sid != 0 && sid != v.serverIP) {
		return
	}
	l, ok := m.leases[mac]
	if !ok || opt50 == 0 || l.ip != opt50 {
		return
	}
	m.drop(mac)
	m.declined[opt50] = true
}

func (m *propModel) restart() {
	m.declined = map[pIP]bool{}
	for k, l := range m.leases {
		if m.now >= l.exp {
			delete(m.leases, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Client classes and the boot-pointer oracle.

type propClass struct {
	name      string
	opt60     string
	userClass string
	arch      int // -1 = option 93 absent
}

var propClasses = []propClass{
	{"plain", "", "", -1},
	{"pxe-bios", "PXEClient:Arch:00000:UNDI:002001", "", 0},
	{"pxe-uefi", "PXEClient:Arch:00007:UNDI:003001", "", 7},
	{"pxe-efi-bc", "PXEClient:Arch:00009:UNDI:003001", "", 9},
	{"pxe-noarch", "PXEClient", "", -1},
	{"pxe-arm64", "PXEClient:Arch:00011:UNDI:003000", "", 11},
	{"http-uefi", "HTTPClient:Arch:00016:UNDI:003001", "", 16},
	{"http-noarch", "HTTPClient", "", -1},
	{"slag-ipxe", "PXEClient:Arch:00007:UNDI:003001", "slagworks-ipxe", 7},
	{"slag-ipxe-bare", "", "slagworks-ipxe", -1},
	{"generic-ipxe", "PXEClient:Arch:00000:UNDI:002001", "iPXE", 0},
	{"apc-pdu", "APC", "", -1},
	{"bmc", "udhcp 1.31.1", "", -1},
}

func (c propClass) isIPXE() bool { return strings.Contains(c.userClass, "slagworks-ipxe") }

// kind is the documented OnACK classification: slagworks iPXE, then PXE or
// HTTP-boot firmware by option 60, then anything else.
func (c propClass) kind() ClientKind {
	switch {
	case c.isIPXE():
		return ClientIPXE
	case strings.HasPrefix(c.opt60, "PXEClient"), strings.HasPrefix(c.opt60, "HTTPClient"):
		return ClientFirmware
	}
	return ClientOther
}

// bootPointer is the documented bootFileFor result: nothing for a MAC the
// negative gate excludes or a client that is not a netboot client; the boot
// script URL for slagworks iPXE; the iPXE binary over HTTP for HTTPClient
// firmware (with option 60 echoed); a bare TFTP filename for PXEClient.
func (w *propWorld) bootPointer(mac int, c propClass, server pIP) (file string, echo60 bool) {
	if w.gateBoot && w.ineligible[mac] {
		return "", false
	}
	binary := "snponly.efi"
	if c.arch == -1 || c.arch == 0 {
		binary = "undionly.kpxe"
	}
	http := strings.HasPrefix(c.opt60, "HTTPClient")
	switch {
	case c.isIPXE():
		return fmt.Sprintf("http://%s:%d/boot/ipxe", server, propHTTPPort), http
	case http:
		return fmt.Sprintf("http://%s:%d/files/ipxe/%s", server, propHTTPPort, binary), true
	case strings.HasPrefix(c.opt60, "PXEClient"):
		return binary, false
	}
	return "", false
}

// ---------------------------------------------------------------------------
// System under test wiring.

type propStore struct {
	mu   sync.Mutex
	rows map[string]PersistedLease
}

func (s *propStore) SaveLease(mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[mac.String()] = PersistedLease{MAC: mac, IP: ip, ExpiresAt: expiry}
}

func (s *propStore) DeleteLease(mac net.HardwareAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, mac.String())
}

func (s *propStore) LoadLeases() []PersistedLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PersistedLease, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out
}

type propACK struct {
	mac  string
	kind ClientKind
}

type propSUT struct {
	w      *propWorld
	h      *Handler
	store  *propStore
	events []LeaseEvent
	acks   chan propACK
	probed []pIP
}

func netPool(p mPool) (net.IPNet, net.IP, net.IP, net.IP) {
	sub := net.IPNet{IP: p.net.ip(), Mask: net.CIDRMask(p.bits, 32)}
	if !p.hasRange {
		return sub, nil, nil, p.gateway.ip()
	}
	return sub, p.lo.ip(), p.hi.ip(), p.gateway.ip()
}

func netPoolCfg(pc mPoolCfg) PoolConfig {
	if !pc.configured {
		return PoolConfig{Promiscuous: pc.promiscuous}
	}
	out := PoolConfig{Promiscuous: pc.promiscuous}
	out.Subnet, out.RangeStart, out.RangeEnd, out.Gateway = netPool(pc.direct)
	for _, r := range pc.relays {
		rs := RelaySubnet{}
		rs.Subnet, rs.RangeStart, rs.RangeEnd, rs.Gateway = netPool(r)
		out.RelaySubnets = append(out.RelaySubnets, rs)
	}
	return out
}

func netIPs(ps []pIP) []net.IP {
	out := make([]net.IP, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ip())
	}
	return out
}

func (s *propSUT) ifaceParams() IfaceParams {
	o := s.w.oob
	p := IfaceParams{PoolConfig: netPoolCfg(o.pools)}
	if o.serverIP != 0 {
		p.ServerIP = o.serverIP.ip()
	}
	if o.dns {
		p.DNS = netIPs(propDNSOOB)
	}
	if o.ntp {
		p.NTP = netIPs(propNTPOOB)
	}
	if o.leaseTicks != 0 {
		p.LeaseDuration = time.Duration(o.leaseTicks) * propTick
	}
	if o.sliced {
		// This gateway's slice plus a peer's, as oobIfaceParams builds it.
		if o.pools.direct.hasRange {
			p.DynamicSlices = append(p.DynamicSlices, DynamicSlice{Start: o.pools.direct.lo.ip(), End: o.pools.direct.hi.ip()})
		}
		p.DynamicSlices = append(p.DynamicSlices, DynamicSlice{Start: a4(10, 9, 0, 20).ip(), End: a4(10, 9, 0, 22).ip()})
	}
	if o.arp {
		p.ARPProbe = s.probe
	}
	return p
}

func (s *propSUT) probe(ip net.IP) bool {
	p := toPIP(ip)
	s.probed = append(s.probed, p)
	return s.w.squat[p]
}

// boot builds a fresh Handler over the current world and the surviving
// store, the way startSiteServices does at process start.
func (s *propSUT) boot() {
	cfg := Config{
		ServerIP:      propBaseServer.ip(),
		DNS:           netIPs(propDNSBase),
		LeaseDuration: time.Duration(s.w.baseTicks) * propTick,
		HTTPBootPort:  propHTTPPort,
		PoolConfig:    netPoolCfg(s.w.base),
		LeaseStore:    s.store,
		ReserveIP: func(mac string) net.IP {
			if r, ok := s.w.reserve[propMACIndex(mac)]; ok {
				return r.ip()
			}
			return nil
		},
		OnACK:        func(mac string, kind ClientKind) { s.acks <- propACK{mac, kind} },
		OnLeaseEvent: func(ev LeaseEvent) { s.events = append(s.events, ev) },
	}
	if s.w.gateKnown {
		cfg.KnownMACs = func(mac string) bool {
			i := propMACIndex(mac)
			return i >= 0 && s.w.known[i]
		}
	}
	if s.w.gateBoot {
		cfg.BootEligible = func(mac string) bool {
			i := propMACIndex(mac)
			return i < 0 || !s.w.ineligible[i]
		}
	}
	if s.w.arp {
		cfg.ARPProbe = s.probe
	}
	s.h = NewHandler(cfg)
	s.h.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.w.oob.registered {
		s.h.SetIfaceParams(propOOBIface, s.ifaceParams())
	}
}

// age moves the handler's clock forward by d: the handler reads time.Now()
// directly and only ever compares it with lease expiries, so shifting every
// expiry (in memory and in the store) back by d is the same observation.
func (s *propSUT) age(d time.Duration) {
	s.h.mu.Lock()
	for _, l := range s.h.leases {
		l.Expiry = l.Expiry.Add(-d)
	}
	s.h.mu.Unlock()
	s.store.mu.Lock()
	for k, r := range s.store.rows {
		r.ExpiresAt = r.ExpiresAt.Add(-d)
		s.store.rows[k] = r
	}
	s.store.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Packet construction (through the wire encoding, as server4 delivers it).

type propPacket struct {
	typ     dhcpv4.MessageType
	mac     int
	arrival propArrival
	class   propClass
	sid     pIP
	opt50   pIP
	ciaddr  pIP
	extra   int // index into propExtras
}

// propExtras are harmless-by-contract additions real clients send.
var propExtras = []struct {
	name string
	mods []dhcpv4.Modifier
}{
	{"none", nil},
	{"hostname", []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptHostName("bmc-a1"))}},
	{"giant-hostname", []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptHostName(strings.Repeat("h", 255)))}},
	{"client-id", []dhcpv4.Modifier{dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte{1, 2, 0, 0, 0, 0, 9}))}},
	{"relay-agent-info", []dhcpv4.Modifier{dhcpv4.WithGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 4, 'e', 't', 'h', '1'})}},
	{"broadcast", []dhcpv4.Modifier{dhcpv4.WithBroadcast(true)}},
	{"empty-opt50", []dhcpv4.Modifier{dhcpv4.WithGeneric(dhcpv4.OptionRequestedIPAddress, nil)}},
	{"short-server-id", []dhcpv4.Modifier{dhcpv4.WithGeneric(dhcpv4.OptionServerIdentifier, []byte{10, 1})}},
	{"param-list", []dhcpv4.Modifier{dhcpv4.WithRequestedOptions(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionBootfileName, dhcpv4.OptionTFTPServerName)}},
}

func (pk propPacket) String() string {
	s := fmt.Sprintf("%s mac=%d iface=%q", pk.typ, pk.mac, pk.arrival.iface)
	if pk.arrival.giaddr != 0 {
		s += " giaddr=" + pk.arrival.giaddr.String()
	}
	if pk.sid != 0 {
		s += " sid=" + pk.sid.String()
	}
	if pk.opt50 != 0 {
		s += " opt50=" + pk.opt50.String()
	}
	if pk.ciaddr != 0 {
		s += " ciaddr=" + pk.ciaddr.String()
	}
	return s + " class=" + pk.class.name + " extra=" + propExtras[pk.extra].name
}

func (pk propPacket) build(t *rapid.T) *dhcpv4.DHCPv4 {
	mods := []dhcpv4.Modifier{
		dhcpv4.WithHwAddr(propMAC(pk.mac)),
		dhcpv4.WithMessageType(pk.typ),
	}
	if pk.arrival.giaddr != 0 {
		mods = append(mods, dhcpv4.WithGatewayIP(pk.arrival.giaddr.ip()))
	}
	if pk.class.opt60 != "" {
		mods = append(mods, dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte(pk.class.opt60)))
	}
	if pk.class.userClass != "" {
		mods = append(mods, dhcpv4.WithUserClass(pk.class.userClass, false))
	}
	if pk.class.arch >= 0 {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(pk.class.arch))))
	}
	// Extras first so an explicit server-id / option 50 below wins.
	if pk.sid == 0 && pk.opt50 == 0 || pk.extra < 6 || pk.extra > 7 {
		mods = append(mods, propExtras[pk.extra].mods...)
	}
	if pk.sid != 0 {
		mods = append(mods, dhcpv4.WithServerIP(net.IPv4zero), dhcpv4.WithOption(dhcpv4.OptServerIdentifier(pk.sid.ip())))
	}
	if pk.opt50 != 0 {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(pk.opt50.ip())))
	}
	if pk.ciaddr != 0 {
		mods = append(mods, dhcpv4.WithClientIP(pk.ciaddr.ip()))
	}
	req, err := dhcpv4.New(mods...)
	if err != nil {
		t.Fatalf("building %s: %v", pk, err)
	}
	wire, err := dhcpv4.FromBytes(req.ToBytes())
	if err != nil {
		t.Fatalf("re-parsing %s: %v", pk, err)
	}
	return wire
}

// ---------------------------------------------------------------------------
// Generators.

var propArrivals = []propArrival{
	{"", 0},
	{"", 0},
	{propOOBIface, 0},
	{propOOBIface, 0},
	{"eth9", 0},                     // unregistered interface -> base view
	{"", a4(10, 1, 0, 3)},           // relayed, giaddr inside the direct subnet
	{"", a4(10, 2, 0, 1)},           // relay pool
	{"", a4(10, 2, 0, 250)},         // relay pool, relay address != gateway
	{"", a4(10, 3, 0, 0)},           // /31, low endpoint relays
	{"", a4(10, 3, 0, 1)},           // /31, high endpoint relays
	{"", a4(10, 200, 0, 1)},         // giaddr nobody serves
	{propOOBIface, a4(10, 9, 0, 3)}, // relayed onto the OOB interface
	{propOOBIface, a4(10, 2, 0, 1)}, // OOB interface has no relay pools
}

func genRange(t *rapid.T, label string, netw pIP, allowNone bool) (bool, pIP, pIP) {
	if allowNone && rapid.IntRange(0, 5).Draw(t, label+"None") == 0 {
		return false, 0, 0
	}
	lo := netw + pIP(rapid.IntRange(10, 12).Draw(t, label+"Lo"))
	n := rapid.IntRange(1, 3).Draw(t, label+"Size")
	return true, lo, lo + pIP(n-1)
}

func genBasePools(t *rapid.T, allowCold bool) mPoolCfg {
	if allowCold && rapid.IntRange(0, 9).Draw(t, "baseCold") == 0 {
		return mPoolCfg{promiscuous: rapid.Bool().Draw(t, "basePromiscuous")}
	}
	pc := mPoolCfg{configured: true, promiscuous: rapid.Bool().Draw(t, "basePromiscuous")}
	pc.direct = mPool{net: a4(10, 1, 0, 0), bits: 24, gateway: a4(10, 1, 0, 1)}
	pc.direct.hasRange, pc.direct.lo, pc.direct.hi = genRange(t, "base", a4(10, 1, 0, 0), false)
	if rapid.IntRange(0, 3).Draw(t, "baseRelay") > 0 {
		r := mPool{net: a4(10, 2, 0, 0), bits: 24, gateway: a4(10, 2, 0, 1)}
		r.hasRange, r.lo, r.hi = genRange(t, "relay", a4(10, 2, 0, 0), false)
		pc.relays = append(pc.relays, r)
	}
	if rapid.IntRange(0, 3).Draw(t, "baseP2P") > 0 {
		// As the snapshot carries a /31: one endpoint is the gateway, the
		// other the range. The handler must not depend on which.
		gwHigh := rapid.Bool().Draw(t, "p2pGatewayHigh")
		r := mPool{net: a4(10, 3, 0, 0), bits: 31, hasRange: true}
		if gwHigh {
			r.gateway, r.lo, r.hi = a4(10, 3, 0, 1), a4(10, 3, 0, 0), a4(10, 3, 0, 0)
		} else {
			r.gateway, r.lo, r.hi = a4(10, 3, 0, 0), a4(10, 3, 0, 1), a4(10, 3, 0, 1)
		}
		pc.relays = append(pc.relays, r)
	}
	return pc
}

func genOOB(t *rapid.T) mIface {
	o := mIface{registered: true}
	if rapid.IntRange(0, 5).Draw(t, "oobServerInherit") > 0 {
		o.serverIP = propOOBServer
	}
	o.dns = rapid.Bool().Draw(t, "oobDNS")
	o.ntp = rapid.Bool().Draw(t, "oobNTP")
	o.leaseTicks = rapid.IntRange(0, 2).Draw(t, "oobLeaseTicks")
	o.arp = rapid.Bool().Draw(t, "oobARP")
	if rapid.IntRange(0, 9).Draw(t, "oobCold") == 0 {
		return o
	}
	o.pools = mPoolCfg{configured: true, promiscuous: rapid.IntRange(0, 4).Draw(t, "oobPromiscuous") > 0}
	o.pools.direct = mPool{net: a4(10, 9, 0, 0), bits: 24, gateway: a4(10, 9, 0, 1)}
	o.pools.direct.hasRange, o.pools.direct.lo, o.pools.direct.hi = genRange(t, "oob", a4(10, 9, 0, 0), true)
	o.sliced = rapid.IntRange(0, 2).Draw(t, "oobSliced") > 0
	return o
}

func genPacket(t *rapid.T, m *propModel, lastOffer map[int]mReply, typ dhcpv4.MessageType) propPacket {
	pk := propPacket{typ: typ}
	pk.mac = rapid.IntRange(0, propNumMACs-1).Draw(t, "mac")
	pk.arrival = rapid.SampledFrom(propArrivals).Draw(t, "arrival")
	pk.class = rapid.SampledFrom(propClasses).Draw(t, "class")
	if rapid.IntRange(0, 3).Draw(t, "extraOn") == 0 {
		pk.extra = rapid.IntRange(0, len(propExtras)-1).Draw(t, "extra")
	}
	if typ != dhcpv4.MessageTypeRequest {
		return pk
	}
	pickIP := func(label string) pIP {
		cands := []pIP{}
		if l, ok := m.leases[pk.mac]; ok {
			cands = append(cands, l.ip, l.ip, l.ip)
		}
		if o, ok := lastOffer[pk.mac]; ok {
			cands = append(cands, o.ip, o.ip)
		}
		if r, ok := m.w.reserve[pk.mac]; ok {
			cands = append(cands, r, r)
		}
		for mac, l := range m.leases {
			if mac != pk.mac {
				cands = append(cands, l.ip)
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i] < cands[j] })
		cands = append(cands, propWantIPs...)
		return rapid.SampledFrom(cands).Draw(t, label)
	}
	switch rapid.IntRange(0, 9).Draw(t, "reqState") {
	case 0, 1, 2: // SELECTING, answering the last offer on the same path
		if o, ok := lastOffer[pk.mac]; ok && rapid.IntRange(0, 4).Draw(t, "selectingSamePath") > 0 {
			pk.sid, pk.opt50 = o.view.serverIP, o.ip
			break
		}
		pk.sid, pk.opt50 = m.w.view(pk.arrival.iface).serverIP, pickIP("selectingIP")
	case 3: // SELECTING, accepted another server's offer
		pk.sid, pk.opt50 = rapid.SampledFrom([]pIP{propForeignSrv, propBaseServer, propOOBServer}).Draw(t, "foreignSid"), pickIP("foreignIP")
	case 4, 5: // INIT-REBOOT
		pk.opt50 = pickIP("initRebootIP")
	case 6, 7, 8: // RENEWING / REBINDING
		pk.ciaddr = pickIP("renewIP")
	case 9: // no requested address at all
	}
	return pk
}

// ---------------------------------------------------------------------------
// The property.

type propStats struct {
	mu      sync.Mutex
	counts  map[string]int
	maxLen  int
	maxLive int
}

func (s *propStats) inc(k string) {
	s.mu.Lock()
	s.counts[k]++
	s.mu.Unlock()
}

var propCoverage = &propStats{counts: map[string]int{}}

func propQuietLogs(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestProp_HandlerMatchesModel(t *testing.T) {
	propQuietLogs(t)
	rapid.Check(t, propHandlerHistory)
	propCoverage.mu.Lock()
	defer propCoverage.mu.Unlock()
	keys := make([]string, 0, len(propCoverage.counts))
	for k := range propCoverage.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-34s %d\n", k, propCoverage.counts[k])
	}
	t.Logf("coverage (events across all generated histories, max history %d steps, max live leases %d):\n%s",
		propCoverage.maxLen, propCoverage.maxLive, b.String())
}

func propHandlerHistory(t *rapid.T) {
	cov := propCoverage
	w := &propWorld{reserve: map[int]pIP{}, squat: map[pIP]bool{}}
	w.base = genBasePools(t, true)
	w.baseTicks = rapid.IntRange(2, 4).Draw(t, "baseLeaseTicks")
	if rapid.IntRange(0, 4).Draw(t, "oobRegistered") > 0 {
		w.oob = genOOB(t)
	}
	w.gateKnown = rapid.IntRange(0, 4).Draw(t, "gateKnown") > 0
	w.gateBoot = rapid.IntRange(0, 4).Draw(t, "gateBoot") > 0
	w.arp = rapid.Bool().Draw(t, "arp")
	overlap := rapid.IntRange(0, 3).Draw(t, "reservationsMayOverlapRange") == 0
	for i := 0; i < propNumMACs; i++ {
		w.known[i] = rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("known%d", i)) > 0
		w.ineligible[i] = rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("ineligible%d", i)) == 0
	}
	resIPs := append([]pIP(nil), propStaticIPs...)
	if overlap {
		resIPs = append(resIPs, propOverlapIPs...)
	}
	setReservation := func(mac int, ip pIP) {
		for other, r := range w.reserve {
			if r == ip && other != mac {
				delete(w.reserve, other) // the address moves
			}
		}
		w.reserve[mac] = ip
	}
	for i, n := 0, rapid.IntRange(0, 3).Draw(t, "initialReservations"); i < n; i++ {
		setReservation(rapid.IntRange(0, propNumMACs-1).Draw(t, "resMAC"), rapid.SampledFrom(resIPs).Draw(t, "resIP"))
	}

	m := &propModel{w: w, leases: map[int]*mLease{}, declined: map[pIP]bool{}}
	s := &propSUT{w: w, store: &propStore{rows: map[string]PersistedLease{}}, acks: make(chan propACK, 8)}
	s.boot()
	lastOffer := map[int]mReply{}

	// Swarm: each history draws its own operation mix.
	weights := make([]int, 12)
	for i := range weights {
		weights[i] = rapid.SampledFrom([]int{0, 1, 1, 3}).Draw(t, fmt.Sprintf("weight%d", i))
	}
	weights[0]++ // DISCOVER is always possible
	total := 0
	for _, wgt := range weights {
		total += wgt
	}

	steps := rapid.IntRange(1, 60).Draw(t, "steps")
	cov.mu.Lock()
	if steps > cov.maxLen {
		cov.maxLen = steps
	}
	cov.mu.Unlock()

	for step := 0; step < steps; step++ {
		roll := rapid.IntRange(0, total-1).Draw(t, "op")
		op := 0
		for roll >= weights[op] {
			roll -= weights[op]
			op++
		}
		m.events = nil
		s.events = nil
		s.probed = nil
		switch op {
		case 0, 1:
			typ := dhcpv4.MessageTypeDiscover
			if op == 1 {
				typ = dhcpv4.MessageTypeRequest
			}
			pk := genPacket(t, m, lastOffer, typ)
			propExchange(t, s, m, pk, lastOffer, cov)
		case 2: // full DORA on one path
			pk := genPacket(t, m, lastOffer, dhcpv4.MessageTypeDiscover)
			if propExchange(t, s, m, pk, lastOffer, cov) == "offer" {
				o := lastOffer[pk.mac]
				pk.typ, pk.sid, pk.opt50 = dhcpv4.MessageTypeRequest, o.view.serverIP, o.ip
				if propExchange(t, s, m, pk, lastOffer, cov) != "ack" {
					t.Fatalf("step %d: REQUEST for the address just offered to the same MAC on the same path was not ACKed", step)
				}
				cov.inc("pattern: DORA completed")
			}
		case 3: // messages that never get a reply (only DECLINE changes state)
			typ := rapid.SampledFrom([]dhcpv4.MessageType{dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline, dhcpv4.MessageTypeInform, dhcpv4.MessageTypeOffer, dhcpv4.MessageTypeAck, dhcpv4.MessageTypeNone}).Draw(t, "unansweredType")
			pk := genPacket(t, m, lastOffer, typ)
			if l, ok := m.leases[pk.mac]; ok {
				if typ == dhcpv4.MessageTypeDecline {
					// A DECLINE names the address in option 50 and usually
					// the server that granted it.
					pk.opt50 = l.ip
					if rapid.Bool().Draw(t, "declineSid") {
						pk.sid = m.w.view(pk.arrival.iface).serverIP
					}
				} else {
					pk.ciaddr = l.ip
				}
			}
			if typ == dhcpv4.MessageTypeDecline {
				if l := m.live(pk.mac); l != nil && l.ip == pk.opt50 {
					cov.inc("pattern: DECLINE of the MAC's live lease")
				}
				m.decline(pk.mac, pk.arrival, pk.sid, pk.opt50)
			}
			if reply := s.h.HandlePacketOn(pk.build(t), pk.arrival.iface); reply != nil {
				t.Fatalf("step %d: %s got a reply:\n%s", step, pk, reply.Summary())
			}
			cov.inc("op: unanswered " + typ.String())
		case 4:
			d := rapid.IntRange(1, 5).Draw(t, "ageTicks")
			m.now += d
			s.age(time.Duration(d) * propTick)
			cov.inc("op: clock advance")
		case 5:
			mac := rapid.IntRange(0, propNumMACs-1).Draw(t, "resMAC")
			ip := rapid.SampledFrom(resIPs).Draw(t, "resIP")
			if h := m.holder(ip, mac); h >= 0 {
				cov.inc("pattern: reservation moved onto an address another MAC leases")
			}
			if l := m.live(mac); l != nil && l.ip != ip {
				cov.inc("pattern: reservation changed under a live lease")
			}
			setReservation(mac, ip)
			cov.inc("op: set reservation")
		case 6:
			mac := rapid.IntRange(0, propNumMACs-1).Draw(t, "unreserveMAC")
			if _, ok := w.reserve[mac]; ok && m.live(mac) != nil {
				cov.inc("pattern: reservation removed under a live lease")
			}
			delete(w.reserve, mac)
			cov.inc("op: clear reservation")
		case 7:
			w.base = genBasePools(t, false)
			s.h.UpdatePools(netPoolCfg(w.base))
			cov.inc("op: UpdatePools")
		case 8:
			w.oob = genOOB(t)
			s.h.SetIfaceParams(propOOBIface, s.ifaceParams())
			cov.inc("op: SetIfaceParams")
		case 9:
			m.restart()
			s.boot()
			cov.inc("op: restart")
		case 10:
			ip := rapid.SampledFrom(propWantIPs).Draw(t, "squatIP")
			if w.squat[ip] {
				delete(w.squat, ip)
			} else {
				w.squat[ip] = true
			}
			cov.inc("op: toggle ARP squatter")
		case 11:
			mac := rapid.IntRange(0, propNumMACs-1).Draw(t, "flipMAC")
			if rapid.Bool().Draw(t, "flipKnown") {
				w.known[mac] = !w.known[mac]
			} else {
				w.ineligible[mac] = !w.ineligible[mac]
			}
			cov.inc("op: flip known/eligible")
		}
		propCheckState(t, s, m, step, cov)
	}
}

// propExchange sends one DISCOVER or REQUEST to both sides and compares
// everything about the answer. Returns the model's reply kind.
func propExchange(t *rapid.T, s *propSUT, m *propModel, pk propPacket, lastOffer map[int]mReply, cov *propStats) string {
	w := s.w
	before := map[int]mLease{}
	for k, l := range m.leases {
		before[k] = *l
	}
	now := m.now

	var want mReply
	if pk.typ == dhcpv4.MessageTypeDiscover {
		want = m.discover(pk.mac, pk.arrival)
	} else {
		want = m.request(pk.mac, pk.arrival, pk.sid, pk.opt50, pk.ciaddr)
	}
	req := pk.build(t)
	got := s.h.HandlePacketOn(req, pk.arrival.iface)

	cov.inc(fmt.Sprintf("reply: %s -> %s", pk.typ, want.kind))
	gotKind := "none"
	if got != nil {
		switch got.MessageType() {
		case dhcpv4.MessageTypeOffer:
			gotKind = "offer"
		case dhcpv4.MessageTypeAck:
			gotKind = "ack"
		case dhcpv4.MessageTypeNak:
			gotKind = "nak"
		default:
			gotKind = got.MessageType().String()
		}
	}
	fail := func(format string, args ...any) {
		summary := "<no reply>"
		if got != nil {
			summary = got.Summary()
		}
		t.Fatalf("%s\n  packet: %s\n  model:  %s %s\n  reply:\n%s", fmt.Sprintf(format, args...), pk, want.kind, want.ip, summary)
	}
	if gotKind != want.kind {
		fail("reply type: handler %q, model %q", gotKind, want.kind)
	}

	// --- global invariants on a granted address -------------------------
	if gotKind == "offer" || gotKind == "ack" {
		yi := toPIP(got.YourIPAddr)
		v := w.view(pk.arrival.iface)
		p := servingPool(v.pools, pk.arrival.giaddr, pk.renewCiaddr())
		if p == nil {
			fail("granted %s on a path with no serving pool", yi)
		}
		res, hasRes := w.reserve[pk.mac]
		hasRes = hasRes && p.has(res)
		if !p.has(yi) {
			fail("granted %s outside the serving subnet %s", yi, p.cidr())
		}
		if hasRes && yi != res {
			fail("MAC has reservation %s on this subnet but was granted %s", res, yi)
		}
		if !hasRes && !p.inRange(yi) {
			fail("granted %s, which is neither a reservation of this MAC nor inside the dynamic range", yi)
		}
		for other, r := range w.reserve {
			// A /31 relay leg has exactly one client address: whoever is on
			// the port gets it, reservation or not, by design.
			if other != pk.mac && r == yi && p.bits != 31 {
				if hasRes {
					t.Fatalf("generator bug: two MACs reserve %s", yi)
				}
				// Reservation inside the dynamic range: not guarded, see
				// the file comment.
				if pk.typ == dhcpv4.MessageTypeRequest && (pk.opt50 != 0 || pk.ciaddr != 0) {
					cov.inc("pattern: adopt-in-place took an address reserved for another MAC")
				} else {
					cov.inc("pattern: dynamic allocation took an address reserved for another MAC")
				}
			}
		}
		if pk.typ == dhcpv4.MessageTypeRequest {
			asked := pk.opt50
			if asked == 0 {
				asked = pk.ciaddr
			}
			if asked != 0 && yi != asked {
				fail("REQUEST for %s was ACKed with a different address %s", asked, yi)
			}
		}
		if !hasRes && v.arp && w.squat[yi] {
			l, had := before[pk.mac]
			renewed := had && now < l.exp && l.ip == yi
			asked := pk.typ == dhcpv4.MessageTypeRequest && (pk.opt50 == yi || pk.ciaddr == yi)
			if !renewed && !asked {
				fail("freshly allocated %s although the ARP probe reports it in use", yi)
			}
		}
		for other, l := range before {
			if other != pk.mac && now < l.exp && l.ip == yi {
				// That the other lease is revoked is checked on the lease
				// table itself, in propCheckState.
				cov.inc("pattern: grant of an address another MAC leased")
			}
		}
	}
	if gotKind == "none" && pk.typ == dhcpv4.MessageTypeRequest {
		// A REQUEST naming the MAC's own live lease is always answered: ACKed
		// at that address, or NAKed when the pool can no longer serve it
		// (a reservations-only pool included).
		if l, ok := before[pk.mac]; ok && now < l.exp && (pk.opt50 == l.ip || pk.opt50 == 0 && pk.ciaddr == l.ip) {
			v := w.view(pk.arrival.iface)
			if p := servingPool(v.pools, pk.arrival.giaddr, pk.renewCiaddr()); p != nil && (pk.sid == 0 || pk.sid == v.serverIP) && (!w.gateKnown || v.pools.promiscuous || w.known[pk.mac]) {
				fail("REQUEST for the MAC's own live lease %s got no answer", l.ip)
			}
		}
	}

	// --- differential: every reply field ---------------------------------
	if got != nil {
		rt, err := dhcpv4.FromBytes(got.ToBytes())
		if err != nil {
			fail("reply does not survive its own wire encoding: %v", err)
		}
		if rt.MessageType() != got.MessageType() || !rt.YourIPAddr.Equal(got.YourIPAddr) || rt.BootFileName != got.BootFileName {
			fail("reply changes across the wire: %s", rt.Summary())
		}
		if got.OpCode != dhcpv4.OpcodeBootReply || got.TransactionID != req.TransactionID || !bytes.Equal(got.ClientHWAddr, req.ClientHWAddr) {
			fail("reply header does not answer the request")
		}
		if toPIP(got.GatewayIPAddr) != pk.arrival.giaddr {
			fail("reply giaddr %s, request giaddr %s", got.GatewayIPAddr, pk.arrival.giaddr)
		}
		if sid := toPIP(got.ServerIdentifier()); sid != want.view.serverIP {
			fail("server identifier %s, want %s", sid, want.view.serverIP)
		}
		if cid := req.Options.Get(dhcpv4.OptionClientIdentifier); !bytes.Equal(got.Options.Get(dhcpv4.OptionClientIdentifier), cid) {
			fail("client identifier not echoed (RFC 6842)")
		}
	}
	switch gotKind {
	case "nak":
		if toPIP(got.YourIPAddr) != 0 || got.BootFileName != "" || toPIP(got.ServerIPAddr) != 0 {
			fail("NAK carries an address or boot pointer")
		}
		if pk.arrival.giaddr != 0 {
			cov.inc("nak via relay")
			if !got.IsBroadcast() {
				fail("NAK for a relayed REQUEST lacks the broadcast bit")
			}
		}
		cov.inc("nak")
	case "offer", "ack":
		if yi := toPIP(got.YourIPAddr); yi != want.ip {
			fail("yiaddr: handler %s, model %s", yi, want.ip)
		}
		if r := got.Router(); len(r) != 1 || toPIP(r[0]) != want.pool.gateway {
			fail("router %v, want %s", r, want.pool.gateway)
		}
		if ones, _ := got.SubnetMask().Size(); ones != want.pool.bits {
			fail("netmask /%d, want /%d", ones, want.pool.bits)
		}
		if lt := got.IPAddressLeaseTime(0); lt != time.Duration(want.view.leaseTicks)*propTick {
			fail("lease time %s, want %s", lt, time.Duration(want.view.leaseTicks)*propTick)
		}
		if dns := got.DNS(); len(dns) != len(want.view.dns) {
			fail("DNS %v, want %v", dns, want.view.dns)
		} else {
			for i := range dns {
				if toPIP(dns[i]) != want.view.dns[i] {
					fail("DNS %v, want %v", dns, want.view.dns)
				}
			}
		}
		ntp := got.Options.Get(dhcpv4.OptionNTPServers)
		if len(ntp) != 4*len(want.view.ntp) || (len(ntp) == 4 && toPIP(net.IP(ntp)) != want.view.ntp[0]) {
			fail("NTP option %v, want %v", ntp, want.view.ntp)
		}
		if string(got.Options.Get(dhcpv4.OptionVendorSpecificInformation)) != VendorMagic {
			fail("vendor magic missing")
		}
		file, echo := w.bootPointer(pk.mac, pk.class, want.view.serverIP)
		if got.BootFileName != file {
			fail("boot file %q, want %q", got.BootFileName, file)
		}
		wantNext := pIP(0)
		if file != "" {
			wantNext = want.view.serverIP
		}
		if toPIP(got.ServerIPAddr) != wantNext {
			fail("next-server %s, want %s", got.ServerIPAddr, wantNext)
		}
		gotEcho := string(got.Options.Get(dhcpv4.OptionClassIdentifier))
		if (echo && gotEcho != "HTTPClient") || (!echo && gotEcho != "") {
			fail("option 60 echo %q, want echo=%v", gotEcho, echo)
		}
		if w.gateBoot && w.ineligible[pk.mac] {
			if got.BootFileName != "" || toPIP(got.ServerIPAddr) != 0 || got.Options.Has(dhcpv4.OptionBootfileName) || got.Options.Has(dhcpv4.OptionTFTPServerName) || gotEcho != "" {
				fail("MAC excluded by the boot gate received a boot pointer")
			}
			cov.inc("boot: gate refused")
		} else if file == "" {
			cov.inc("boot: not a netboot client")
		} else {
			cov.inc("boot: " + pk.class.name)
		}
		if want.pool.bits == 31 {
			cov.inc("grant: /31 relay")
		}
		if _, ok := w.reserve[pk.mac]; ok && want.pool.has(w.reserve[pk.mac]) {
			cov.inc("grant: reservation")
		} else {
			cov.inc("grant: dynamic")
		}
	}
	if want.kind == "offer" {
		lastOffer[pk.mac] = want
	}

	// --- OnACK fires exactly for ACKs, with the right class --------------
	if gotKind == "ack" {
		select {
		case a := <-s.acks:
			if a.mac != propMAC(pk.mac).String() || a.kind != pk.class.kind() {
				fail("OnACK(%s, %v), want (%s, %v)", a.mac, a.kind, propMAC(pk.mac), pk.class.kind())
			}
		case <-time.After(5 * time.Second):
			fail("OnACK never fired")
		}
	}

	// --- pattern coverage --------------------------------------------------
	if l, ok := before[pk.mac]; ok {
		if now >= l.exp {
			cov.inc("pattern: packet from a MAC whose lease expired")
		} else if want.kind == "offer" || want.kind == "ack" {
			if want.ip == l.ip {
				cov.inc("pattern: renew in place")
			} else {
				cov.inc("pattern: live lease replaced by a different address")
			}
		}
	}
	if want.kind == "none" && pk.typ == dhcpv4.MessageTypeDiscover {
		v := w.view(pk.arrival.iface)
		if p := servingPool(v.pools, pk.arrival.giaddr, pk.renewCiaddr()); p != nil && m.admitted(pk.mac, v) {
			cov.inc("pattern: pool exhausted / reservations-only")
		}
	}
	if len(s.probed) > 0 {
		cov.inc("pattern: ARP probed")
	}
	return want.kind
}

// propCheckState compares every observable piece of lease state.
func propCheckState(t *rapid.T, s *propSUT, m *propModel, step int, cov *propStats) {
	w := s.w

	// Exact OnLeaseEvent stream: sweep deletes (any order) then the grant.
	wantEv := make([]string, 0, len(m.events))
	for _, e := range m.events {
		wantEv = append(wantEv, fmt.Sprintf("%s %s %s pool=%s", e.kind, propMAC(e.mac), e.ip, w.label(e.ip)))
	}
	gotEv := make([]string, 0, len(s.events))
	for _, e := range s.events {
		gotEv = append(gotEv, fmt.Sprintf("%s %s %s pool=%s", e.Event, e.MAC, toPIP(e.IP), e.Pool))
		cov.inc("event: " + e.Event)
	}
	sortDeletes := func(evs []string) {
		n := 0
		for n < len(evs) && strings.HasPrefix(evs[n], "delete ") {
			n++
		}
		sort.Strings(evs[:n])
	}
	sortDeletes(wantEv)
	sortDeletes(gotEv)
	if strings.Join(gotEv, "\n") != strings.Join(wantEv, "\n") {
		t.Fatalf("step %d: OnLeaseEvent stream\n  handler: %v\n  model:   %v", step, gotEv, wantEv)
	}
	lastFor := map[string]int{}
	for i, e := range s.events {
		lastFor[e.MAC.String()] = i
	}
	for i, e := range s.events {
		if e.Event == "delete" || lastFor[e.MAC.String()] != i {
			continue
		}
		s.h.mu.Lock()
		l := s.h.leases[e.MAC.String()]
		s.h.mu.Unlock()
		if l == nil || !l.Expiry.Equal(e.Expiry) || !l.IP.Equal(e.IP) {
			t.Fatalf("step %d: event %+v does not describe the lease it announces (%+v)", step, e, l)
		}
	}

	// Live lease table through the public API.
	wantLive := map[string]string{}
	wantCounts := map[string]int{}
	if w.base.configured {
		wantCounts[w.base.direct.cidr()] = 0
	}
	for _, r := range w.base.relays {
		wantCounts[r.cidr()] = 0
	}
	holders := map[pIP][]int{}
	for mac, l := range m.leases {
		if m.now < l.exp {
			wantLive[propMAC(mac).String()] = l.ip.String() + " " + w.label(l.ip)
			wantCounts[w.label(l.ip)]++
			holders[l.ip] = append(holders[l.ip], mac)
		}
	}
	gotLive := map[string]string{}
	for _, li := range s.h.Leases() {
		if _, dup := gotLive[li.MAC]; dup {
			t.Fatalf("step %d: Leases() lists MAC %s twice", step, li.MAC)
		}
		gotLive[li.MAC] = li.IP + " " + li.Pool
	}
	if fmt.Sprint(gotLive) != fmt.Sprint(wantLive) {
		t.Fatalf("step %d: live leases\n  handler: %v\n  model:   %v", step, gotLive, wantLive)
	}
	if got := s.h.LeaseCountsByPool(); fmt.Sprint(got) != fmt.Sprint(wantCounts) {
		t.Fatalf("step %d: LeaseCountsByPool\n  handler: %v\n  model:   %v", step, got, wantCounts)
	}
	cov.mu.Lock()
	if len(wantLive) > cov.maxLive {
		cov.maxLive = len(wantLive)
	}
	cov.mu.Unlock()

	// One address, one MAC.
	for ip, macs := range holders {
		if len(macs) > 1 {
			t.Fatalf("step %d: %s is leased to MACs %v at once", step, ip, macs)
		}
		if got := s.h.MACForIP(ip.String()); got != propMAC(macs[0]).String() {
			t.Fatalf("step %d: MACForIP(%s) = %q, want %s", step, ip, got, propMAC(macs[0]))
		}
	}
	for _, ip := range propWantIPs {
		_, held := holders[ip]
		if s.h.HasLease(ip.String()) != held {
			t.Fatalf("step %d: HasLease(%s) = %v, model %v", step, ip, !held, held)
		}
		if !held && s.h.MACForIP(ip.String()) != "" {
			t.Fatalf("step %d: MACForIP(%s) names a MAC for an unleased address", step, ip)
		}
	}

	wantProm := w.base.promiscuous || (w.oob.registered && w.oob.pools.promiscuous)
	if s.h.AnyPromiscuous() != wantProm {
		t.Fatalf("step %d: AnyPromiscuous = %v, want %v", step, !wantProm, wantProm)
	}

	// Persistence mirrors the in-memory table exactly, so a restart at any
	// point resumes from the same leases.
	s.h.mu.Lock()
	mem := map[string]string{}
	for k, l := range s.h.leases {
		mem[k] = l.IP.String() + " " + l.Expiry.Format(time.RFC3339Nano)
	}
	s.h.mu.Unlock()
	disk := map[string]string{}
	for k, r := range s.store.rows {
		disk[k] = r.IP.String() + " " + r.ExpiresAt.Format(time.RFC3339Nano)
	}
	if fmt.Sprint(mem) != fmt.Sprint(disk) {
		t.Fatalf("step %d: persisted leases diverge from memory\n  memory: %v\n  store:  %v", step, mem, disk)
	}
}
