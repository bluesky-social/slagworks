package dhcp

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// RelaySubnet defines an additional DHCP pool served via a relay agent.
// When the handler receives a packet with giaddr set, it matches against
// configured relay subnets to determine which pool to allocate from.
type RelaySubnet struct {
	Subnet     net.IPNet
	RangeStart net.IP
	RangeEnd   net.IP
	Gateway    net.IP // The relay's IP on this subnet (matches giaddr)
}

// PoolConfig is the snapshot-derived, hot-reloadable subset of the
// handler's configuration: everything that can change when global
// pushes a new site snapshot (pools, relay pools, promiscuous mode).
// The rest of Config is bound to the process (sockets, callbacks,
// host-local env) and legitimately requires a restart.
type PoolConfig struct {
	Subnet     net.IPNet
	RangeStart net.IP // First IP in pool (inclusive); nil = no dynamic range (reservations only)
	RangeEnd   net.IP // Last IP in pool (inclusive)
	Gateway    net.IP

	// RelaySubnets defines additional pools for clients behind DHCP relays.
	// When a request arrives with giaddr set, the handler finds the relay
	// subnet containing giaddr and allocates from that pool. A renew the
	// client unicasts straight to the server carries no giaddr and is
	// matched by its ciaddr instead (see poolForRequest).
	RelaySubnets []RelaySubnet

	// Promiscuous disables the KnownMACs filter: every MAC is served.
	// The filter func itself lives in Config (it's a process-lifetime
	// callback into the cache); this flag is what the snapshot toggles.
	Promiscuous bool
}

// WorkerShare returns p with each dynamic range narrowed to one worker's
// share: the base range and every relay pool's range are split into
// workers contiguous parts, and the worker keeps part index. Every worker
// of a site builds p from the same snapshot, and a relayed DISCOVER
// reaches all of them. If each allocated from the whole range (the lowest
// address free in its own lease table), two workers would lease one
// address to two hosts: their tables diverge as soon as a host takes one
// worker's offer and not the other's, and the ARP probe cannot see a
// relayed subnet. With disjoint shares each address has one allocating
// worker. A REQUEST for an address in another worker's share that this
// worker holds no lease for is NAKed (classifyRequest), so the client
// re-DISCOVERs.
//
// A /31 relay pool is left whole: relayPoolForRequest derives its one
// client address from giaddr, so it serves one host per link and cannot
// lease that address to two hosts, and halving it would leave the host
// with no offer while the owning worker is down.
//
// An index outside [0, workers) leaves every split range empty (nil
// bounds): the worker then serves reservations and /31 pools only. A part
// smaller than one address is empty too, when a range has fewer addresses
// than there are workers.
func (p PoolConfig) WorkerShare(index, workers int) PoolConfig {
	out := p
	out.RangeStart, out.RangeEnd = shareOfRange(p.RangeStart, p.RangeEnd, index, workers)
	out.RelaySubnets = make([]RelaySubnet, len(p.RelaySubnets))
	for i, rs := range p.RelaySubnets {
		if ones, bits := rs.Subnet.Mask.Size(); ones != 31 || bits != 32 {
			rs.RangeStart, rs.RangeEnd = shareOfRange(rs.RangeStart, rs.RangeEnd, index, workers)
		}
		out.RelaySubnets[i] = rs
	}
	return out
}

// shareOfRange returns part index of the inclusive IPv4 range [start, end]
// split into n contiguous parts whose sizes differ by at most one, or
// nil, nil when that part is empty or the inputs are not a valid range.
// Identical ranges give identical parts, so a range listed both as the
// base pool and as a relay pool is split the same way in both places.
func shareOfRange(start, end net.IP, index, n int) (net.IP, net.IP) {
	s4, e4 := start.To4(), end.To4()
	if s4 == nil || e4 == nil || n < 1 || index < 0 || index >= n {
		return nil, nil
	}
	s, e := binary.BigEndian.Uint32(s4), binary.BigEndian.Uint32(e4)
	if e < s {
		return nil, nil
	}
	size := uint64(e-s) + 1
	lo := size * uint64(index) / uint64(n)
	hi := size * uint64(index+1) / uint64(n) // exclusive
	if hi == lo {
		return nil, nil
	}
	first, last := make(net.IP, net.IPv4len), make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(first, s+uint32(lo))
	binary.BigEndian.PutUint32(last, s+uint32(hi-1))
	return first, last
}

// Config holds all configuration for the DHCP handler. All fields are explicit;
// nothing is auto-detected. The PoolConfig subset is the INITIAL pool state;
// UpdatePools replaces it at runtime when a new snapshot changes it.
//
// ServerIP/DNS/NTP/PoolConfig here are the BASE (default) values, used for
// packets that arrive on an interface with no per-interface override (the
// single-interface case, and the fabric/relay interface). Per-interface
// overrides are registered via SetIfaceParams and selected by the arrival
// interface name passed to HandlePacketOn — see IfaceParams.
type Config struct {
	ServerIP      net.IP
	DNS           []net.IP
	NTP           []net.IP // option 42; empty = not advertised
	LeaseDuration time.Duration
	HTTPBootPort  int

	PoolConfig

	// KnownMACs, when non-nil, restricts DHCP responses to registered MACs
	// (unless PoolConfig.Promiscuous). Requests from unknown MACs are
	// silently dropped at Debug level.
	KnownMACs func(mac string) bool

	// ReserveIP, when non-nil, returns a static IP reservation for the given MAC.
	// Called before dynamic pool allocation. If it returns a non-nil IP, that IP
	// is used regardless of whether it falls within the dynamic pool range. A
	// live lease another MAC holds on that IP is revoked when the reservation
	// is granted; that MAC is NAKed at its next renew.
	ReserveIP func(mac string) net.IP

	// LeaseStore, when non-nil, provides lease persistence across restarts.
	// Leases are saved on allocate/renew and loaded on handler creation.
	LeaseStore LeaseStore

	// ARPProbe, when non-nil, is called before allocating a new IP from the
	// dynamic pool. Returns true if the IP is already in use on the network.
	// Used to detect devices with static IPs or leases from another DHCP server.
	ARPProbe func(ip net.IP) bool

	// OnUnknownMAC, when non-nil, is called asynchronously when a lease is
	// served to a MAC not recognized by KnownMACs. Used by the site worker
	// to register unknown devices with the global server.
	OnUnknownMAC func(mac string)

	// OnACK, when non-nil, is called asynchronously after sending a DHCP ACK,
	// with the kind of client the ACK went to. The site worker turns ACKs to
	// netboot firmware and to slagworks iPXE into a booting machine's
	// dhcp_ack and ipxe_dhcp phases; an ACK to any other client is not a
	// boot phase.
	OnACK func(mac string, client ClientKind)

	// MachineIDForMAC, when non-nil, resolves a MAC to a machine UUID
	// string ("" when unknown). Used to stamp `machine_id` onto every
	// DHCP log envelope so per-machine log views (slagctl logs <host>)
	// surface DHCP traffic during PXE — the only phase where the host
	// has no agent and produces no other signal. Unresolved MACs leave
	// the envelope un-scoped (acceptable; the bug is resolved-but-still-
	// un-scoped).
	MachineIDForMAC func(mac string) string

	// BootEligible, when non-nil, gates whether a reply carries PXE boot
	// options (bootfile + next-server + the iPXE URL). It returns true iff
	// the MAC should be booted now — i.e. has an active booting op. It is
	// SEGMENT-AGNOSTIC: the same per-MAC gate applies on every interface
	// (a switch images off OOB, a server off fabric), so it takes only the
	// MAC, not the interface. Nil means "always eligible" — the current
	// behavior (every reply carries a bootfile), preserved for the
	// single-purpose provisioning case and all existing tests. When the OOB
	// interface runs promiscuous, this gate is what stops a bootfile being
	// handed to every idle BMC/PDU. See PLA OOB DHCP takeover.
	BootEligible func(mac string) bool

	// Metrics, when non-nil, receives Prometheus counters/gauges for
	// every handled packet (NewMetrics). Nil disables instrumentation.
	Metrics *Metrics

	// OnLeaseEvent, when non-nil, is called SYNCHRONOUSLY — in lease-
	// mutation order, under h.mu — from every lease-table mutation site
	// (allocate, renew, delete: expiry sweep, DECLINE, or reservation
	// revoke). This is deliberately NOT the OnACK pattern (`go callback()`)
	// and NOT a drop-on-full channel: the ledger this feeds (see
	// docs/plans/2026-07-13-dhcp-lease-visibility.md Phase 3) folds
	// events in order, and a dropped allocate/delete is a hole in the
	// product's core deliverable (lease history), not a cosmetic gap.
	// Priced deviation: the worker wiring does a SQLite outbox insert
	// inside this callback, putting a disk write under the handler
	// mutex — accepted because segment volume is trivial (~50
	// devices/segment) and the outbox already degrades to in-memory-
	// only on SQLite error rather than blocking. The callback must
	// stay fast: it runs inside the same critical section that decides
	// every other lease request. NOT called for NewHandler's load-time
	// purge of expired persisted leases — those predate this process
	// and are pre-history noise, not a live mutation.
	OnLeaseEvent func(ev LeaseEvent)
}

// LeaseEvent describes one lease-table mutation, emitted to
// Config.OnLeaseEvent when non-nil. Event is "allocate" (a MAC's
// lease is written for the first time, or moves to a different IP —
// including adopt-in-place), "renew" (a MAC's existing lease on the
// SAME IP is extended), or "delete" (an expired entry is purged by
// the sweep in allocateIP, a client DECLINEs its leased address — see
// handleDecline — or another MAC's live lease is revoked because its
// address is being granted as a reservation; the revoke's "delete"
// precedes the reservation holder's "allocate"). Pool is the
// same subnet-CIDR label
// poolLabelForIP produces for the dhcp_leases RPC (Leases) — computed
// here, under h.mu, rather than by the worker-side callback: the
// callback runs INSIDE the same critical section (see OnLeaseEvent),
// so it cannot re-enter the handler to ask for it without deadlocking
// on h.mu.
type LeaseEvent struct {
	Event  string
	MAC    net.HardwareAddr
	IP     net.IP
	Expiry time.Time
	Pool   string
}

// IfaceParams is the per-interface override set for the multi-interface
// bind. A request that arrives on an interface registered (via
// SetIfaceParams) selects that interface's ServerIP / DNS / NTP / direct
// PoolConfig / ARPProbe in place of the handler's base Config. Everything
// NOT in IfaceParams — the lease table, KnownMACs, ReserveIP, BootEligible,
// LeaseStore, the metrics, OnACK/OnUnknownMAC — stays SHARED across all
// interfaces (a MAC belongs to exactly one segment, so one shared
// MAC-keyed lease table gives "same MAC -> same lease" for free).
//
// Pool, gateway, netmask, and the relay subnets all live in PoolConfig, so
// an OOB interface (direct broadcast, its own /22 + dynamic pool) and the
// fabric interface (relay subnets) are fully described by their respective
// PoolConfig. The OOB interface sets Promiscuous=true (serve any MAC); the
// fabric interface leaves it false (KnownMACs-filtered).
type IfaceParams struct {
	ServerIP net.IP
	DNS      []net.IP
	NTP      []net.IP
	ARPProbe func(ip net.IP) bool
	// LeaseDuration overrides the base Config.LeaseDuration for this
	// interface (0 = inherit base). The OOB segment uses a short lease
	// during rollout so a bad dynamic lease self-corrects fast.
	LeaseDuration time.Duration
	// DynamicSlices is the FULL set of per-gateway dynamic-pool slices for
	// this segment (active/active split-pool). PoolConfig.RangeStart/End is
	// THIS gateway's own slice (where it originates fresh leases); the
	// remaining slices belong to peer gateways. The handler uses the full
	// set to classify a client-requested address on a broadcast REQUEST: in
	// my slice → renew; in a peer slice → NAK (force re-DISCOVER); a
	// reservation → ACK; outside the subnet → NAK. Empty = no split-pool
	// (single gateway or reservations-only), and the handler skips the
	// peer-slice NAK logic entirely.
	DynamicSlices []DynamicSlice
	PoolConfig
}

// DynamicSlice is one inclusive [Start,End] IPv4 range of an active/active
// split dynamic pool. See IfaceParams.DynamicSlices.
type DynamicSlice struct {
	Start net.IP
	End   net.IP
}

// LeaseStore persists DHCP leases for crash recovery.
type LeaseStore interface {
	SaveLease(mac net.HardwareAddr, ip net.IP, expiry time.Time)
	DeleteLease(mac net.HardwareAddr)
	LoadLeases() []PersistedLease
}

// PersistedLease is a lease loaded from storage.
type PersistedLease struct {
	MAC       net.HardwareAddr
	IP        net.IP
	ExpiresAt time.Time
}

type lease struct {
	MAC    net.HardwareAddr
	IP     net.IP
	Expiry time.Time
}

// Handler implements the DHCP DORA flow for PXE boot. The core logic is in
// HandlePacket, which is a pure function (packet in, packet out) suitable for
// unit testing without sockets.
type Handler struct {
	cfg               Config
	mu                sync.Mutex
	pools             PoolConfig             // base/default pool state; replaced by UpdatePools
	ifaceParams       map[string]IfaceParams // per-interface overrides keyed by arrival iface; "" never present (base used)
	leases            map[string]*lease      // keyed by MAC string (shared across all interfaces)
	declined          map[string]time.Time   // IP string -> end of its quarantine after a DECLINE; see handleDecline
	log               *slog.Logger
	exhaustedLogTimes map[string]time.Time // MAC -> last time we logged "pool exhausted" for this MAC
}

// ifaceView is the resolved per-request configuration: either the base
// Config (single-interface / fabric / unknown iface) or an interface's
// registered IfaceParams overlaid on it. Built once per packet so the
// handler logic reads one consistent view regardless of arrival interface.
type ifaceView struct {
	serverIP      net.IP
	dns           []net.IP
	ntp           []net.IP
	arpProbe      func(ip net.IP) bool
	leaseDuration time.Duration
	dynamicSlices []DynamicSlice
	pools         PoolConfig
}

// pool holds the parameters for a single DHCP address pool. Extracted from
// either the main config (direct clients) or a RelaySubnet (relayed clients).
type pool struct {
	rangeStart net.IP
	rangeEnd   net.IP
	gateway    net.IP
	mask       net.IPMask
	label      string // subnet CIDR, the Prometheus "pool" label
}

// contains reports whether ip lies inside the pool's subnet (gateway/mask).
// A pool without a usable gateway+mask can't answer, so it reports true:
// callers then keep honoring whatever they were given.
func (p *pool) contains(ip net.IP) bool {
	if p == nil || p.gateway == nil || p.mask == nil {
		return true
	}
	network := p.gateway.Mask(p.mask)
	if network == nil {
		return true
	}
	return network.Equal(ip.Mask(p.mask))
}

// reservationFor returns the MAC's static reservation if it can be served on
// pool p, else nil. A reservation outside p's subnet is ignored: ReserveIP is
// keyed by MAC only, and the worker cache prefers a machine's NetBox
// interface IP, which may belong to a different subnet that shares this
// segment's L2 (e.g. a 1G NIC addressed in a copper /22 PXE-booting on the
// OOB segment). Offering it would hand the client an address with this
// pool's mask and gateway, which is unusable. Resolved once per request so
// the skip is logged once even though REQUEST consults it twice.
func (h *Handler) reservationFor(mac net.HardwareAddr, p *pool, log *slog.Logger) net.IP {
	if h.cfg.ReserveIP == nil {
		return nil
	}
	r := h.cfg.ReserveIP(mac.String())
	if r == nil {
		return nil
	}
	if !p.contains(r) {
		log.Info("ignoring static reservation",
			"reserved_ip", r.String(),
			"pool", p.label,
			"reason", "reservation outside serving subnet")
		return nil
	}
	return r
}

// relayPoolForRequest resolves the effective pool for one relayed packet.
// A /31 has exactly two usable endpoints under RFC 3021: giaddr is the relay
// (leaf) endpoint and the other address is the client. Deriving both values
// from the packet avoids depending on which endpoint's NetBox record happened
// to be encountered first while the site snapshot was deduplicated.
func relayPoolForRequest(rs *RelaySubnet, giaddr net.IP) *pool {
	p := &pool{
		rangeStart: rs.RangeStart,
		rangeEnd:   rs.RangeEnd,
		gateway:    rs.Gateway,
		mask:       rs.Subnet.Mask,
		label:      rs.Subnet.String(),
	}

	ones, bits := rs.Subnet.Mask.Size()
	relay := giaddr.To4()
	if ones != 31 || bits != 32 || relay == nil {
		return p
	}

	gateway := append(net.IP(nil), relay...)
	client := append(net.IP(nil), relay...)
	client[3] ^= 1
	p.rangeStart = client
	p.rangeEnd = client
	p.gateway = gateway
	return p
}

// relayPoolForClient resolves the relay pool for a request that came from a
// client behind a relay without passing through it (a RENEWING unicast; see
// poolForRequest), keyed by ciaddr, the client's own address. On a /31 the
// relay is the other endpoint, so the pool is the one relayPoolForRequest
// derives from that endpoint as giaddr.
func relayPoolForClient(rs *RelaySubnet, ciaddr net.IP) *pool {
	ones, bits := rs.Subnet.Mask.Size()
	client := ciaddr.To4()
	if ones != 31 || bits != 32 || client == nil {
		return relayPoolForRequest(rs, rs.Gateway)
	}
	relay := append(net.IP(nil), client...)
	relay[3] ^= 1
	return relayPoolForRequest(rs, relay)
}

func NewHandler(cfg Config) *Handler {
	h := &Handler{
		cfg:               cfg,
		pools:             cfg.PoolConfig,
		ifaceParams:       make(map[string]IfaceParams),
		leases:            make(map[string]*lease),
		declined:          make(map[string]time.Time),
		log:               slog.Default(),
		exhaustedLogTimes: make(map[string]time.Time),
	}
	cfg.Metrics.setPoolGauges(cfg.PoolConfig)

	// Load persisted leases from previous run. Deliberately does NOT
	// call OnLeaseEvent for either branch: a loaded-live lease isn't a
	// fresh mutation (it happened in a prior process, possibly already
	// ledgered), and a loaded-expired purge is startup cleanup of
	// pre-history state, not a live "delete" the ledger should record
	// as having just occurred.
	if cfg.LeaseStore != nil {
		now := time.Now()
		loaded := 0
		expired := 0
		for _, pl := range cfg.LeaseStore.LoadLeases() {
			if now.After(pl.ExpiresAt) {
				cfg.LeaseStore.DeleteLease(pl.MAC)
				expired++
				continue
			}
			h.leases[pl.MAC.String()] = &lease{
				MAC:    pl.MAC,
				IP:     pl.IP,
				Expiry: pl.ExpiresAt,
			}
			loaded++
		}
		if loaded > 0 || expired > 0 {
			h.log.Info("loaded persisted DHCP leases", "active", loaded, "expired_purged", expired)
		}
	}

	return h
}

// SetLogger replaces the default logger.
func (h *Handler) SetLogger(l *slog.Logger) {
	h.log = l
}

// UpdatePools replaces the handler's pool state (direct pool, relay
// pools, promiscuous flag) at runtime. Called on every snapshot apply,
// so pool edits take effect without a service restart; a no-change
// call is a silent no-op (snapshots apply frequently — op phases,
// machine state — and almost never change pools). Existing leases are
// kept until they expire, but a REQUEST to renew one that the new pools no
// longer cover is NAKed (classifyRequest), so the client re-DISCOVERs into
// the new range.
func (h *Handler) UpdatePools(p PoolConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if reflect.DeepEqual(h.pools, p) {
		return
	}
	h.pools = p
	h.cfg.Metrics.setPoolGauges(p)
	h.log.Info("DHCP pools updated",
		"subnet", p.Subnet.String(),
		"pool", p.RangeStart.String()+"-"+p.RangeEnd.String(),
		"gateway", p.Gateway.String(),
		"relay_pools", len(p.RelaySubnets),
		"promiscuous", p.Promiscuous,
	)
}

// SetIfaceParams registers (or replaces) the per-interface override for a
// named arrival interface. Packets arriving on that interface use this
// IfaceParams' ServerIP/DNS/NTP/PoolConfig/ARPProbe instead of the base
// Config. Safe to call at runtime (snapshot hot-reload). An interface
// with no registered params uses the base Config — that's the
// single-interface case and the fabric leg.
func (h *Handler) SetIfaceParams(iface string, p IfaceParams) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ifaceParams[iface] = p
}

// viewForIface resolves the configuration for a packet that arrived on the
// named interface. An empty iface, or one with no registered IfaceParams,
// yields the base view (today's single-interface behavior, byte-identical).
// Called under no lock by the caller; takes the lock itself.
func (h *Handler) viewForIface(iface string) ifaceView {
	h.mu.Lock()
	defer h.mu.Unlock()
	base := ifaceView{
		serverIP:      h.cfg.ServerIP,
		dns:           h.cfg.DNS,
		ntp:           h.cfg.NTP,
		arpProbe:      h.cfg.ARPProbe,
		leaseDuration: h.cfg.LeaseDuration,
		pools:         h.pools,
	}
	p, ok := h.ifaceParams[iface]
	if !ok {
		return base
	}
	// Per-field fallback to base for unset fields, so a PARTIAL
	// IfaceParams (e.g. ServerIP set but DNS left nil) can't produce a
	// reply with nil/empty options — it inherits the base value instead.
	// A registered override is meant to change specific segment facts, not
	// silently blank out the rest.
	view := base
	if p.ServerIP != nil {
		view.serverIP = p.ServerIP
	}
	if p.DNS != nil {
		view.dns = p.DNS
	}
	if p.NTP != nil {
		view.ntp = p.NTP
	}
	if p.ARPProbe != nil {
		view.arpProbe = p.ARPProbe
	}
	if p.LeaseDuration != 0 {
		view.leaseDuration = p.LeaseDuration
	}
	view.dynamicSlices = p.DynamicSlices
	// PoolConfig is all-or-nothing: a registered interface's pool fully
	// describes its segment (subnet/range/gateway/relay/promiscuous). An
	// unset RangeStart means "no pool configured for this segment yet"
	// (cold start) — poolForRequest already treats nil RangeStart as
	// serve-nothing, which is the correct behavior, so we take p.PoolConfig
	// verbatim rather than falling back to the base (fabric) pool, which
	// would wrongly serve fabric addresses on the OOB segment.
	view.pools = p.PoolConfig
	return view
}

// AnyPromiscuous reports whether ANY bound segment is currently
// promiscuous (the base pool or any registered per-interface pool). The
// TFTP/HTTP-boot KnownIPs gate consults this: those servers don't know
// which DHCP segment a fetching client came from, so "serve if it has a
// lease OR any segment is promiscuous" is the correct, interface-agnostic
// gate. In the single-interface case this is exactly the old
// Promiscuous() — the base pool's flag.
func (h *Handler) AnyPromiscuous() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pools.Promiscuous {
		return true
	}
	for _, p := range h.ifaceParams {
		if p.Promiscuous {
			return true
		}
	}
	return false
}

// HandlePacket processes a DHCP request received with no interface context
// (the single-interface / unscoped case) and returns a response, or nil if
// the request should be ignored. Equivalent to HandlePacketOn(req, "") —
// preserved as the pure (packet→packet) entry point existing tests and the
// single-interface deployment use.
func (h *Handler) HandlePacket(req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	return h.HandlePacketOn(req, "")
}

// HandlePacketOn processes a DHCP request that arrived on the named
// interface. The interface selects the per-interface configuration view
// (ServerIP / DNS / NTP / pool / ARP prober) via SetIfaceParams; an empty
// or unregistered iface uses the base Config. This is the multi-interface
// entry point: the arrival interface is the ONLY disambiguator for a direct
// broadcast (giaddr=0), so it must be threaded in from the socket layer.
// The lease table, KnownMACs, ReserveIP, and BootEligible are shared across
// all interfaces.
func (h *Handler) HandlePacketOn(req *dhcpv4.DHCPv4, iface string) *dhcpv4.DHCPv4 {
	mac := req.ClientHWAddr
	frameType := req.MessageType().String()

	// Stamp machine_id on the per-request logger when the MAC
	// resolves. slogkit hoists `machine_id` to envelope.MachineID,
	// so per-machine log views see DHCP traffic during PXE — the
	// only phase where the host has no agent and produces no other
	// signal. Unresolved MACs leave the envelope un-scoped.
	logAttrs := []any{
		"client_mac", mac.String(),
		"xid", req.TransactionID.String(),
		"frame_type", frameType,
	}
	if iface != "" {
		logAttrs = append(logAttrs, "iface", iface)
	}
	if h.cfg.MachineIDForMAC != nil {
		if id := h.cfg.MachineIDForMAC(mac.String()); id != "" {
			logAttrs = append(logAttrs, "machine_id", id)
		}
	}
	if giaddr := req.GatewayIPAddr; giaddr != nil && !giaddr.IsUnspecified() {
		logAttrs = append(logAttrs, "giaddr", giaddr.String())
	}
	log := h.log.With(logAttrs...)

	h.cfg.Metrics.countRequest(frameType)

	// RFC 2131 / RFC 951: a server acts only on op=BOOTREQUEST. The socket
	// layer hands over any datagram that parses, and option 53 alone does
	// not make a packet a client request: answering an op=BOOTREPLY (or
	// junk-op) DISCOVER would allocate a lease and emit an OFFER that the
	// reply builder stamps op=BOOTREQUEST. Checked before the MAC filter
	// because it holds on every segment, promiscuous or not.
	if req.OpCode != dhcpv4.OpcodeBootRequest {
		log.Warn("dropping DHCP packet that is not a BOOTREQUEST", "op", uint8(req.OpCode))
		h.cfg.Metrics.drop(DropNotBootRequest)
		return nil
	}

	view := h.viewForIface(iface)
	if !view.pools.Promiscuous && h.cfg.KnownMACs != nil && !h.cfg.KnownMACs(mac.String()) {
		log.Debug("dropping DHCP request from unregistered MAC")
		h.cfg.Metrics.drop(DropUnknownMAC)
		return nil
	}

	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		return h.handleDiscover(req, mac, view, log)
	case dhcpv4.MessageTypeRequest:
		return h.handleRequest(req, mac, view, log)
	case dhcpv4.MessageTypeDecline:
		h.handleDecline(req, mac, view, log)
		return nil
	case dhcpv4.MessageTypeRelease:
		log.Info("received DHCP Release, ignoring")
		h.cfg.Metrics.drop(DropUnhandledType)
		return nil
	default:
		log.Warn("received unexpected DHCP message type")
		h.cfg.Metrics.drop(DropUnhandledType)
		return nil
	}
}

// poolForRequest returns the address pool to use based on whether the packet
// was relayed (giaddr set) or received directly. Returns nil if a relayed
// packet's giaddr doesn't match any pool the worker is authoritative for.
//
// Resolution order when giaddr is set:
//
//  1. giaddr lies inside the worker's own Subnet. The worker is authoritative
//     for that subnet but isn't L2-adjacent to it (typical fabric topology:
//     slagworksd binds to a leaf↔gateway P2P link, leaf switches relay DHCP
//     from the served subnet using their bridge IP as giaddr). Use the
//     direct pool.
//  2. giaddr matches one of the configured RelaySubnets. Use that subnet's
//     pool. This covers multi-subnet sites where the worker serves several
//     deploy networks beyond its own.
//  3. Neither: drop the packet — we don't know how to allocate from a subnet
//     we aren't responsible for.
//
// A packet without giaddr gets the direct pool, unless it is a REQUEST whose
// ciaddr lies in one of the RelaySubnets (and not in the worker's own
// Subnet). That is a RENEWING client behind a relay: RFC 2131 4.3.2 has it
// unicast the renew straight to the server, so no relay stamps giaddr, and
// the server trusts ciaddr instead. Resolving the direct pool would NAK the
// renew, since the lease lies outside that pool's range.
func poolForRequest(req *dhcpv4.DHCPv4, pools PoolConfig) *pool {
	// Truly-unconfigured segment (cold start before the first snapshot):
	// serve nothing. We key this on the SUBNET being unset, NOT the dynamic
	// range: a reservations-only segment (e.g. an OOB gateway that owns no
	// dynamic slice) has a valid Subnet+Gateway but a nil RangeStart, and it
	// MUST still serve its static reservations. allocateIP skips the dynamic
	// loop when rangeStart is nil and serves only the reservation, so a nil
	// range yields a reservations-only pool (valid gateway/mask), not a
	// dropped packet.
	if pools.Subnet.IP == nil {
		return nil
	}
	giaddr := req.GatewayIPAddr
	if giaddr != nil && !giaddr.IsUnspecified() {
		if pools.Subnet.Contains(giaddr) {
			return &pool{
				rangeStart: pools.RangeStart,
				rangeEnd:   pools.RangeEnd,
				gateway:    pools.Gateway,
				mask:       pools.Subnet.Mask,
				label:      pools.Subnet.String(),
			}
		}
		for i := range pools.RelaySubnets {
			rs := &pools.RelaySubnets[i]
			if rs.Subnet.Contains(giaddr) {
				return relayPoolForRequest(rs, giaddr)
			}
		}
		return nil
	}
	if ci := req.ClientIPAddr; req.MessageType() == dhcpv4.MessageTypeRequest &&
		ci != nil && !ci.IsUnspecified() && !pools.Subnet.Contains(ci) {
		for i := range pools.RelaySubnets {
			rs := &pools.RelaySubnets[i]
			if rs.Subnet.Contains(ci) {
				return relayPoolForClient(rs, ci)
			}
		}
	}
	return &pool{
		rangeStart: pools.RangeStart,
		rangeEnd:   pools.RangeEnd,
		gateway:    pools.Gateway,
		mask:       pools.Subnet.Mask,
		label:      pools.Subnet.String(),
	}
}

// logPoolExhausted logs a pool exhaustion error at most once per MAC per minute.
// This prevents log spam when a busy OOB network has multiple devices sending
// discovers every few seconds after the pool is exhausted.
func (h *Handler) logPoolExhausted(mac net.HardwareAddr, log *slog.Logger, msgType string) {
	const rateLimitWindow = 60 * time.Second

	h.mu.Lock()
	defer h.mu.Unlock()

	macStr := mac.String()
	lastLog, exists := h.exhaustedLogTimes[macStr]
	now := time.Now()

	if !exists || now.Sub(lastLog) >= rateLimitWindow {
		log.Error("DHCP pool exhausted, dropping " + msgType)
		h.exhaustedLogTimes[macStr] = now
	}
}

func (h *Handler) handleDiscover(req *dhcpv4.DHCPv4, mac net.HardwareAddr, view ifaceView, log *slog.Logger) *dhcpv4.DHCPv4 {
	p := poolForRequest(req, view.pools)
	if p == nil {
		log.Warn("no pool for relayed request, dropping", "giaddr", req.GatewayIPAddr)
		h.cfg.Metrics.drop(DropNoPool)
		return nil
	}

	reserved := h.reservationFor(mac, p, log)
	ip := h.allocateIP(mac, reserved, p, view.arpProbe, view.leaseDuration)
	if ip == nil {
		h.logPoolExhausted(mac, log, "discover")
		h.cfg.Metrics.drop(DropExhausted)
		return nil
	}

	if h.cfg.OnUnknownMAC != nil {
		mac := mac.String()
		go h.cfg.OnUnknownMAC(mac)
	}

	// Serve a PXE bootfile only when the MAC is boot-eligible (has an
	// active booting op), via the segment-agnostic BootEligible gate. Nil
	// gate = always eligible (single-purpose provisioning, the default).
	// For boot-eligible machines that should boot from local disk
	// (state=running etc.), the httpboot handler routes the resulting iPXE
	// chainload to bootLocalScript. Gating here is what stops a promiscuous
	// OOB interface handing a boot URL to every idle BMC/PDU.
	bootFile := h.bootFileFor(req, mac, view, log)
	mods := h.replyModifiers(ip, bootFile, p, isHTTPBootClient(req), view)
	mods = append(mods, dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer))

	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		log.Error("failed to build DHCP Offer", "err", err)
		return nil
	}

	log.Info("DHCP Offer",
		"client_ip", ip.String(),
		"boot_file", bootFile,
		"next_server", nextServerFor(bootFile, view),
		"server_id", view.serverIP.String(),
		"gateway", p.gateway.String(),
	)
	h.cfg.Metrics.offer(p.label)
	return reply
}

func (h *Handler) handleRequest(req *dhcpv4.DHCPv4, mac net.HardwareAddr, view ifaceView, log *slog.Logger) *dhcpv4.DHCPv4 {
	// RFC 2131 §4.3.2: a REQUEST in SELECTING state carries option 54
	// (server identifier) naming the server whose OFFER the client
	// accepted; every other server MUST stay silent. With two site
	// workers both relayed every request (dual-target dhcp-helper),
	// answering a REQUEST addressed to the other worker would ACK a
	// conflicting IP from this worker's own lease table. The server-id is
	// matched against THIS interface's serverIP (each interface advertises
	// its own).
	sid := req.ServerIdentifier()
	hasServerID := sid != nil && !sid.IsUnspecified()
	if hasServerID && !sid.Equal(view.serverIP) {
		log.Debug("REQUEST names another server, staying silent",
			"server_id", sid.String())
		h.cfg.Metrics.drop(DropOtherServer)
		return nil
	}

	p := poolForRequest(req, view.pools)
	if p == nil {
		log.Warn("no pool for relayed request, dropping", "giaddr", req.GatewayIPAddr)
		h.cfg.Metrics.drop(DropNoPool)
		return nil
	}

	// RFC 2131 §4.3.2: classify the requested address (option 50, or ciaddr
	// for RENEWING/REBINDING — see requestedAddr) before touching the pool.
	// This is the fix for the unknown-renew bug: previously an unrecognized
	// requested address fell straight to allocateIP below, which ignores the
	// requested address entirely and can return a DIFFERENT free IP — a
	// RENEWING/REBINDING client never re-reads yiaddr, so the client keeps
	// its old address while the lease table silently diverges onto a new
	// one. classifyRequest enforces the invariant this establishes: every
	// REQUEST with a requested address is answered at exactly that address
	// (fallthrough to the normal renew, or adopt-in-place) or refused with a
	// NAK — never ACK'd with a different yiaddr. A REQUEST with no requested
	// address at all (no option 50, no ciaddr) skips classification and
	// keeps the old degenerate behavior: allocateIP may hand it a fresh IP.
	reserved := h.reservationFor(mac, p, log)
	var ip net.IP
	if reqIP := requestedAddr(req); reqIP != nil {
		verdict, reason := h.classifyRequest(mac, reqIP, reserved, p, view)
		switch verdict {
		case requestNAK:
			// buildNAK counts the drop; don't double-count here.
			log.Info("DHCP NAK", "requested_ip", reqIP.String(), "reason", reason)
			return h.buildNAK(req, view, log)
		case requestAdopt:
			// Lease already written by classifyRequest, atomically with the
			// conflict checks that justified it. No ARP probe: a RENEWING/
			// REBINDING client already has reqIP configured and answers ARP
			// for it itself, so a probe would detect the requester and
			// wrongly refuse its own address (the prober returns bool, not
			// the answering MAC — it cannot tell requester from squatter).
			ip = reqIP
			log.Info("DHCP adopt-in-place", "requested_ip", reqIP.String())
		case requestFallthrough:
			// Normal renew (existing lease matches) or a reservation —
			// allocateIP below serves either identically to today.
		}
	}
	if ip == nil {
		ip = h.allocateIP(mac, reserved, p, view.arpProbe, view.leaseDuration)
	}
	if ip == nil {
		h.logPoolExhausted(mac, log, "request")
		h.cfg.Metrics.drop(DropExhausted)
		return nil
	}

	bootFile := h.bootFileFor(req, mac, view, log)
	mods := h.replyModifiers(ip, bootFile, p, isHTTPBootClient(req), view)
	mods = append(mods, dhcpv4.WithMessageType(dhcpv4.MessageTypeAck))

	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		log.Error("failed to build DHCP Ack", "err", err)
		return nil
	}

	log.Info("DHCP Ack",
		"client_ip", ip.String(),
		"boot_file", bootFile,
		"next_server", nextServerFor(bootFile, view),
		"server_id", view.serverIP.String(),
		"gateway", p.gateway.String(),
	)

	h.cfg.Metrics.ack(p.label)
	if h.cfg.OnACK != nil {
		go h.cfg.OnACK(mac.String(), clientKind(req))
	}

	return reply
}

// handleDecline implements RFC 2131 §4.3.3: the client's own duplicate-
// address check (§4.4.1) found the address this server gave it already in
// use, and the client is restarting at DISCOVER. The server MUST mark the
// address as not available. Keeping the lease instead would make the
// client's next DISCOVER take allocateIP's renew path and be offered the
// same address again, looping for as long as the conflict stands.
//
// A DECLINE acts only on the address this server last leased to this MAC
// (option 50 equals its lease-table entry, which may already have expired
// but not yet been swept: the conflict report is no less true for that).
// One naming another server (option 54) or any other address is ignored,
// so a stray or forged DECLINE cannot withdraw an address from someone
// else's lease or quarantine arbitrary pool addresses. On a match the
// lease is dropped and the address withheld from dynamic allocation for
// one lease duration, the period in which a lease on it would have needed
// renewing; after that it is offered again, and a squatter that is still
// there is caught by the next client's check. The quarantine is in memory
// only: a restart or failover forgets it and costs at most one more
// DECLINE.
//
// The quarantine applies to fresh dynamic allocation only. A static
// reservation is still offered to its MAC (the reservation is definitive),
// and classifyRequest's adopt-in-place still grants the address to a
// client that REQUESTs it while already configured with it: the likeliest
// such client is the squatter itself, holding a lease this server lost.
func (h *Handler) handleDecline(req *dhcpv4.DHCPv4, mac net.HardwareAddr, view ifaceView, log *slog.Logger) {
	if sid := req.ServerIdentifier(); sid != nil && !sid.IsUnspecified() && !sid.Equal(view.serverIP) {
		log.Debug("DECLINE names another server, ignoring", "server_id", sid.String())
		return
	}
	ip := req.RequestedIPAddress()
	if ip == nil || ip.IsUnspecified() {
		log.Info("DECLINE carries no requested address (option 50), ignoring")
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.leases[mac.String()]
	if !ok || !l.IP.Equal(ip) {
		log.Info("DECLINE for an address this server did not lease to this client, ignoring",
			"declined_ip", ip.String())
		return
	}
	h.deleteLease(mac.String(), l)
	until := time.Now().Add(view.leaseDuration)
	h.declined[ip.String()] = until
	log.Warn("client declined its address as already in use; lease dropped and address withheld from dynamic allocation; find the other host using this address",
		"declined_ip", ip.String(),
		"withheld_until", until.Format(time.RFC3339))
}

// requestedAddr returns the IPv4 address a DHCPREQUEST is asking to keep:
// option 50 (Requested IP Address) for SELECTING/INIT-REBOOT, or ciaddr for
// RENEWING/REBINDING (which carry no option 50). Returns nil if neither is
// set. This is the "the IP the client wants" used by classifyRequest —
// matching only option 50 would miss every REBINDING client (design D4).
func requestedAddr(req *dhcpv4.DHCPv4) net.IP {
	if opt := req.RequestedIPAddress(); opt != nil && !opt.IsUnspecified() {
		return opt.To4()
	}
	if ci := req.ClientIPAddr; ci != nil && !ci.IsUnspecified() {
		return ci.To4()
	}
	return nil
}

// requestVerdict is the outcome of classifyRequest.
type requestVerdict int

const (
	// requestFallthrough: reqIP is this MAC's static reservation, or its
	// existing lease inside the resolved pool's range. allocateIP's
	// reservation/renew branch serves it unchanged.
	requestFallthrough requestVerdict = iota
	// requestAdopt: no existing lease, but reqIP is safe to grant as-is.
	// classifyRequest has ALREADY written the lease (atomically with the
	// conflict check that justified it); the caller only needs to ACK.
	requestAdopt
	// requestNAK: reqIP must be refused. The caller sends a NAK.
	requestNAK
)

// classifyRequest decides how to answer a REQUEST for reqIP from mac, per
// RFC 2131 §4.3.2 and the active/active split-pool design (see
// docs/plans/2026-07-13-dhcp-lease-visibility.md Phase 1). It is the single
// place that enforces the invariant the fix establishes: a REQUEST is
// answered at reqIP (fallthrough or adopt) or refused (NAK) — never handed a
// DIFFERENT address. p is the resolved pool for this packet (poolForRequest
// result): the "dynamic address" test below is "inside p's range," which
// for the direct, non-relayed OOB requests this classification exists for
// is exactly THIS gateway's own slice (PoolConfig.RangeStart is always this
// gateway's slice — see IfaceParams.DynamicSlices).
//
// Order of checks:
//  1. reqIP is this MAC's static reservation → fallthrough. A reservation
//     mismatch (MAC has a DIFFERENT reservation than reqIP) → NAK: the
//     reservation is definitive, and silently substituting it for reqIP
//     would ACK a different address than requested (allocateIP's
//     reservation branch ignores the requested address entirely). reserved
//     is the caller's reservationFor result, so a reservation outside p's
//     subnet arrives as nil and the request is classified as unreserved.
//  2. reqIP is this MAC's existing unexpired lease and inside p's range →
//     fallthrough (normal renew). The same lease outside p's range → NAK:
//     a later snapshot removed its reservation or moved the range or slice
//     away from it, or the request resolved to a pool other than the one
//     that granted it. The lease is not released; like every lease it ends
//     at expiry or when the re-DISCOVER the NAK triggers overwrites it. A
//     DIFFERENT existing lease → NAK — never ACK at reqIP (would create a
//     second lease for one MAC) and never silently substitute the held
//     address (the client asked for something specific).
//  3. reqIP is not a dynamic address in p's resolved range (p is already
//     the correctly-scoped pool for this packet — this gateway's own slice
//     for direct OOB requests, or the matched RelaySubnet for relayed
//     requests; see poolForRequest) → NAK. This covers "wrong segment
//     entirely," "in the static-reservation band with no reservation," and
//     "in a peer gateway's slice" (p never resolves to a peer's range) in
//     one check — all three end in NAK regardless, so there's no behavior
//     to distinguish, and a check against the handler's own base
//     Subnet — rather than p's resolved range — would wrongly NAK a
//     legitimately relayed dynamic address that falls outside the DIRECT
//     subnet but inside the matched RelaySubnet's range.
//  4. reqIP is a live dynamic address already leased to a DIFFERENT MAC →
//     NAK (conflict).
//  5. Free dynamic address, no lease for this MAC: adopt-in-place ONLY when
//     this segment has split dynamic slices (view.dynamicSlices non-empty)
//     — adoption requires a deterministic single owner, which slice
//     ownership provides. A segment without DynamicSlices (the base view:
//     the fabric and relay pools) NAKs instead, forcing DISCOVER. That is
//     safe because DISCOVER allocates only from p's range: when several
//     workers serve a site, each one's ranges are its own share
//     (PoolConfig.WorkerShare), so no two workers can offer one address
//     even though their lease tables differ and the ARP probe cannot see a
//     relayed subnet. No ARP probe on the adopt path itself: a
//     RENEWING/REBINDING client already has reqIP configured and answers
//     ARP for it, so a probe would detect the requester and wrongly refuse
//     its own address.
//
// Caller holds no lock; classifyRequest takes h.mu itself, briefly, and (for
// the adopt case) writes the lease inside that same critical section so the
// conflict check and the grant are atomic.
func (h *Handler) classifyRequest(mac net.HardwareAddr, reqIP, reserved net.IP, p *pool, view ifaceView) (requestVerdict, string) {
	macStr := mac.String()

	if reserved != nil {
		if reserved.Equal(reqIP) {
			return requestFallthrough, "reservation match"
		}
		return requestNAK, "reservation mismatch"
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()

	if l, ok := h.leases[macStr]; ok && now.Before(l.Expiry) {
		if !l.IP.Equal(reqIP) {
			return requestNAK, "existing lease mismatch"
		}
		// Same range test allocateIP's renew branch applies: a fallthrough
		// for a lease outside p's range would make allocateIP allocate a
		// different address (or none on a reservations-only pool).
		if p.rangeStart == nil || !ipInRange(reqIP, p.rangeStart, p.rangeEnd) {
			return requestNAK, "existing lease outside the resolved pool's range"
		}
		return requestFallthrough, "existing lease match"
	}

	if p.rangeStart == nil || !ipInRange(reqIP, p.rangeStart, p.rangeEnd) {
		return requestNAK, "not a dynamic address in the resolved pool's range"
	}

	for other, l := range h.leases {
		if other != macStr && l.IP.Equal(reqIP) && now.Before(l.Expiry) {
			return requestNAK, "leased to another MAC"
		}
	}

	if len(view.dynamicSlices) == 0 {
		return requestNAK, "no lease here for this address, and only a segment with split-pool slices adopts one"
	}

	h.setLease(macStr, mac, reqIP, now.Add(view.leaseDuration))
	return requestAdopt, "adopted in place"
}

// buildNAK constructs a DHCPNAK for a request. A NAK carries the server-id
// (this interface's serverIP) and no address; the client discards its lease
// and restarts at DISCOVER.
//
// For a relayed request (giaddr set) the NAK always has the broadcast bit
// set, per RFC 2131 4.3.2, instead of copying the client's flags as
// NewReplyFromRequest does. The NAK has no yiaddr, so a relay that honors a
// clear bit has nowhere to unicast it. On a direct NAK the bit steers no
// relay (Server picks the destination itself), so it stays as copied.
func (h *Handler) buildNAK(req *dhcpv4.DHCPv4, view ifaceView, log *slog.Logger) *dhcpv4.DHCPv4 {
	nak, err := dhcpv4.NewReplyFromRequest(req,
		dhcpv4.WithMessageType(dhcpv4.MessageTypeNak),
		dhcpv4.WithGeneric(dhcpv4.OptionServerIdentifier, view.serverIP.To4()),
	)
	if err != nil {
		log.Error("failed to build DHCP NAK", "err", err)
		return nil
	}
	if gi := req.GatewayIPAddr; gi != nil && !gi.IsUnspecified() {
		nak.SetBroadcast()
	}
	h.cfg.Metrics.drop(DropNAK)
	return nak
}

// VendorMagic is the vendor-specific option value included in all DHCP
// responses. The commission overlay uses this to identify slagworks DHCP
// servers and reject leases from other DHCP servers on the network.
const VendorMagic = "slagworks-v1"

func (h *Handler) replyModifiers(clientIP net.IP, bootFile string, p *pool, httpBoot bool, view ifaceView) []dhcpv4.Modifier {
	mods := []dhcpv4.Modifier{
		dhcpv4.WithYourIP(clientIP),
		dhcpv4.WithRouter(p.gateway),
		dhcpv4.WithNetmask(p.mask),
		dhcpv4.WithDNS(view.dns...),
		dhcpv4.WithLeaseTime(uint32(view.leaseDuration.Seconds())),
		dhcpv4.WithGeneric(dhcpv4.OptionServerIdentifier, view.serverIP.To4()),
		dhcpv4.WithGeneric(dhcpv4.OptionVendorSpecificInformation, []byte(VendorMagic)),
	}
	// NTP servers (option 42), when configured for this segment. dnsmasq
	// advertises the OOB gateway as NTP; matching it is part of the
	// dnsmasq-takeover option parity. Guard on the ENCODED bytes, not the
	// slice length: a slice of only non-IPv4 addresses encodes to empty and
	// must not emit a zero-length (malformed) option.
	if ntpBytes := ipsToBytes(view.ntp); len(ntpBytes) > 0 {
		mods = append(mods, dhcpv4.WithGeneric(dhcpv4.OptionNTPServers, ntpBytes))
	}
	// Boot pointer (BootFileName + siaddr/next-server) only when bootFileFor
	// chose a file: an empty bootFile leaves both unset, so the reply carries
	// address/router/DNS/NTP but nothing a client could try to boot from.
	if bootFile != "" {
		mods = append(mods, dhcpv4.WithServerIP(view.serverIP), func(d *dhcpv4.DHCPv4) {
			d.BootFileName = bootFile
		})
	}
	if httpBoot && bootFile != "" {
		// UEFI HTTP-boot firmware requires the server to echo option 60
		// set to "HTTPClient" — without this confirmation the firmware
		// ignores the BootFileName URL and falls back / retries. The value
		// is the bare "HTTPClient" tag (no arch suffix needed on the reply).
		mods = append(mods, dhcpv4.WithGeneric(
			dhcpv4.OptionClassIdentifier, []byte("HTTPClient")))
	}
	return mods
}

// ipsToBytes flattens IPv4 addresses to the wire form DHCP option 42
// (and similar IP-list options) expects: 4 bytes per address, concatenated.
func ipsToBytes(ips []net.IP) []byte {
	out := make([]byte, 0, 4*len(ips))
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4...)
		}
	}
	return out
}

// nextServerFor is the next-server (siaddr) value a reply carries, for
// logging: the interface serverIP when a boot file is served, else "".
func nextServerFor(bootFile string, view ifaceView) string {
	if bootFile == "" {
		return ""
	}
	return view.serverIP.String()
}

// bootFileFor returns the PXE boot file for a request, or "" when the
// request should get no boot pointer. An empty return makes replyModifiers
// omit BootFileName and siaddr entirely, so the client gets an address but
// nothing to boot from. Two gates:
//
//   - BootEligible(mac) (segment-agnostic): an idle BMC/PDU on a
//     promiscuous OOB interface is not eligible. Nil BootEligible = always
//     eligible (current default behavior).
//   - The request must identify as a netboot client: our iPXE (user-class),
//     or option 60 starting with "PXEClient" (mandated by PXE 2.1 for every
//     PXE ROM, legacy and UEFI) or "HTTPClient" (UEFI HTTP boot). A
//     boot-eligible MAC can still be a non-netboot DHCP client: the APC PDUs
//     enrolled as machines send option 60 "APC" and no option 93, and
//     falling through to the TFTP default handed their NMCs undionly.kpxe,
//     which they tried to TFTP, timed out on, and re-DHCPed every few
//     minutes. Such refusals are logged at Info so a nonconforming real PXE
//     ROM is visible rather than silently not booting.
//
// When both gates pass, the boot file is chosen by client type (iPXE vs
// firmware) and architecture, and the iPXE/HTTP URLs use THIS interface's
// serverIP (view.serverIP) so the client fetches from the gateway that
// answered:
//  1. Our iPXE (embed.ipxe sets user-class slagworks-ipxe) → HTTP URL to the
//     boot script. iPXE fetches /boot/ipxe over HTTP and runs it.
//  2. UEFI HTTP-boot firmware (option 60 ClassIdentifier starts with
//     "HTTPClient") → HTTP URL to the iPXE binary over reliable HTTP. iPXE
//     then chainloads /boot/ipxe as in case 1. The reply must also echo
//     option 60 "HTTPClient" (handled in replyModifiers).
//  3. PXE firmware (option 60 starts with "PXEClient") → bare TFTP filename.
func (h *Handler) bootFileFor(req *dhcpv4.DHCPv4, mac net.HardwareAddr, view ifaceView, log *slog.Logger) string {
	if h.cfg.BootEligible != nil && !h.cfg.BootEligible(mac.String()) {
		return ""
	}
	if isIPXEClient(req) {
		return fmt.Sprintf("http://%s:%d/boot/ipxe", view.serverIP, h.cfg.HTTPBootPort)
	}
	if isHTTPBootClient(req) {
		// Serve the arch-appropriate iPXE binary over HTTP. The firmware
		// HTTP-fetches it, then iPXE's embedded script takes over and
		// chainloads /boot/ipxe (case 1). This replaces the unreliable
		// UEFI-firmware TFTP fetch of the ~1MB binary, which times out and
		// retries on these Supermicro boards.
		return fmt.Sprintf("http://%s:%d/files/ipxe/%s",
			view.serverIP, h.cfg.HTTPBootPort, h.tftpBootFilename(req, log))
	}
	if isPXEClient(req) {
		return h.tftpBootFilename(req, log)
	}
	log.Info("boot-eligible MAC is not a netboot client (option 60 lacks PXEClient/HTTPClient); serving no boot file",
		"class_id", req.ClassIdentifier(),
		"hostname", req.HostName(),
	)
	return ""
}

func (h *Handler) tftpBootFilename(req *dhcpv4.DHCPv4, log *slog.Logger) string {
	arches := req.ClientArch()
	if len(arches) == 0 {
		log.Debug("DHCP request missing option 93 (client arch), defaulting to BIOS")
		return "undionly.kpxe"
	}

	arch := arches[0]
	switch arch {
	case iana.INTEL_X86PC:
		return "undionly.kpxe"
	case iana.EFI_BC, iana.EFI_X86_64, iana.EFI_IA32:
		return "snponly.efi"
	default:
		log.Warn("unrecognized client architecture, defaulting to UEFI", "arch", arch.String())
		return "snponly.efi"
	}
}

// HasLease returns true if the given IP string has an active lease.
// Used by the TFTP server's KnownIPs filter to allow only machines
// that have completed DHCP with this server.
func (h *Handler) HasLease(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, l := range h.leases {
		if l.IP.String() == ip && now.Before(l.Expiry) {
			return true
		}
	}
	return false
}

// poolLabelForIP returns the pool subnet (CIDR string) containing ip —
// the direct pool, a relay pool, or a per-interface pool registered via
// SetIfaceParams (the OOB takeover path) — or "none" if ip falls
// outside every configured pool. Caller must hold h.mu. Shared by
// LeaseCountsByPool (the Prometheus gauge) and Leases (the
// dhcp_leases RPC snapshot) so both agree on what a lease's "pool" is.
//
// Checks h.pools.Subnet.IP (not RangeStart) so a reservations-only
// pool — a valid Subnet/Gateway with a nil dynamic RangeStart, e.g. an
// OOB gateway that owns no dynamic slice but still serves its static
// reservations (see poolForRequest) — labels its leases correctly
// instead of falling through to "none".
func (h *Handler) poolLabelForIP(ip net.IP) string {
	if h.pools.Subnet.IP != nil && h.pools.Subnet.Contains(ip) {
		return h.pools.Subnet.String()
	}
	for i := range h.pools.RelaySubnets {
		if h.pools.RelaySubnets[i].Subnet.Contains(ip) {
			return h.pools.RelaySubnets[i].Subnet.String()
		}
	}
	for _, p := range h.ifaceParams {
		if p.Subnet.IP != nil && p.Subnet.Contains(ip) {
			return p.Subnet.String()
		}
		for i := range p.RelaySubnets {
			if p.RelaySubnets[i].Subnet.Contains(ip) {
				return p.RelaySubnets[i].Subnet.String()
			}
		}
	}
	return "none"
}

// LeaseCountsByPool returns the number of unexpired leases grouped by
// the pool subnet (CIDR string) containing the leased IP; leases
// outside every configured pool count under "none". Called by the
// Prometheus lease collector at scrape time.
func (h *Handler) LeaseCountsByPool() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	// Pre-seed every configured pool at 0 so a drained pool still
	// emits a 0-valued sample. A label that just disappears goes
	// Prometheus-stale instead of reading 0, breaking sum() across
	// pools and "did the pool empty out?" dashboards. Gated on
	// Subnet.IP, matching poolLabelForIP's guard — NOT RangeStart, so
	// a reservations-only base pool (valid Subnet, nil dynamic range)
	// also gets its 0-valued sample instead of going Prometheus-stale
	// when its reservation leases drain (roast finding, 2026-07-13).
	counts := make(map[string]int)
	if h.pools.Subnet.IP != nil {
		counts[h.pools.Subnet.String()] = 0
	}
	for i := range h.pools.RelaySubnets {
		counts[h.pools.RelaySubnets[i].Subnet.String()] = 0
	}
	for _, l := range h.leases {
		if now.After(l.Expiry) {
			continue
		}
		counts[h.poolLabelForIP(l.IP)]++
	}
	return counts
}

// LeaseInfo is a snapshot of one unexpired lease, returned by Leases()
// for the dhcp_leases RPC (slagctl dhcp leases). It carries only what
// the handler itself knows (identity, address, expiry, pool); static
// vs. dynamic classification and machine identity enrichment are the
// caller's job (the site worker's cache holds ReserveIP/machine lookups
// the dhcp package has no access to — see internal/worker's
// runDHCPLeases).
type LeaseInfo struct {
	MAC    string
	IP     string
	Expiry time.Time
	Pool   string
}

// Leases returns a snapshot of every unexpired lease. Expired entries
// are skipped but NOT deleted here — lazy cleanup during allocation is
// unchanged; a read must not have mutation side effects.
func (h *Handler) Leases() []LeaseInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]LeaseInfo, 0, len(h.leases))
	for _, l := range h.leases {
		if now.After(l.Expiry) {
			continue
		}
		out = append(out, LeaseInfo{
			MAC:    l.MAC.String(),
			IP:     l.IP.String(),
			Expiry: l.Expiry,
			Pool:   h.poolLabelForIP(l.IP),
		})
	}
	return out
}

// MACForIP returns the MAC of the host currently leasing the given
// IP, or "" if no active lease matches. Used by TFTP and HTTP boot
// log middleware to scope per-request envelopes back to
// the requesting machine.
func (h *Handler) MACForIP(ip string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for mac, l := range h.leases {
		if l.IP.String() == ip && now.Before(l.Expiry) {
			return mac
		}
	}
	return ""
}

// ClientKind is which boot stage, if any, a DHCP client identifies as. It is
// read from the request the way bootFileFor reads it.
type ClientKind int

const (
	// ClientOther is not a netboot client: the deploy or commission
	// environment's own DHCP client (dhcpcd in the initrd, started by the
	// boot script's ip=dhcp), an installed OS, a BMC or a PDU.
	ClientOther ClientKind = iota
	// ClientFirmware is PXE firmware (option 60 "PXEClient...") or UEFI
	// HTTP-boot firmware ("HTTPClient..."). A generic iPXE ROM that has yet
	// to chainload slagworks iPXE also sends PXEClient and counts here.
	ClientFirmware
	// ClientIPXE is slagworks iPXE after the chainload (user class
	// slagworks-ipxe).
	ClientIPXE
)

// clientKind classifies a request for OnACK. Slagworks iPXE is checked
// first because it also sends option 60 PXEClient.
func clientKind(req *dhcpv4.DHCPv4) ClientKind {
	switch {
	case isIPXEClient(req):
		return ClientIPXE
	case isPXEClient(req), isHTTPBootClient(req):
		return ClientFirmware
	}
	return ClientOther
}

// isIPXEClient returns true if the DHCP request is from our slagworks iPXE
// (with the embedded script), not a generic iPXE ROM (e.g., QEMU's built-in).
// Our embed.ipxe sets user-class to "slagworks-ipxe" before the dhcp call.
// Generic iPXE ROMs send "iPXE" as the user class — these should get the TFTP
// filename so they chainload our iPXE binary first.
func isIPXEClient(req *dhcpv4.DHCPv4) bool {
	uc := req.Options.Get(dhcpv4.OptionUserClassInformation)
	if uc == nil {
		return false
	}
	return strings.Contains(string(uc), "slagworks-ipxe")
}

// isHTTPBootClient reports whether the request is from UEFI HTTP-boot
// firmware. Per the UEFI spec (and RFC 3925/PXE conventions), an HTTP-boot
// client sets DHCP option 60 (Vendor Class Identifier) to a string beginning
// with "HTTPClient" (e.g. "HTTPClient:Arch:00016:UNDI:003000"). Plain PXE
// firmware sends "PXEClient..." instead. We match the prefix, not equality,
// because the firmware appends arch/UNDI suffixes.
func isHTTPBootClient(req *dhcpv4.DHCPv4) bool {
	return strings.HasPrefix(req.ClassIdentifier(), "HTTPClient")
}

// isPXEClient reports whether the request is from PXE firmware: PXE 2.1
// requires every PXE ROM (legacy BIOS and UEFI) to send option 60 beginning
// with "PXEClient" (e.g. "PXEClient:Arch:00000:UNDI:002001").
func isPXEClient(req *dhcpv4.DHCPv4) bool {
	return strings.HasPrefix(req.ClassIdentifier(), "PXEClient")
}

// allocateIP returns an IP for the given MAC from the specified pool, reusing
// an existing lease if available and it falls within the pool's range. Expired
// leases are cleaned up lazily. Returns nil if the pool is exhausted.
//
// arpProbe is the per-interface ARP prober (nil = no probe); it must egress
// the interface the request arrived on, so the caller passes the view's
// prober rather than reading h.cfg.ARPProbe directly.
func (h *Handler) allocateIP(mac net.HardwareAddr, reserved net.IP, p *pool, arpProbe func(ip net.IP) bool, leaseDur time.Duration) net.IP {
	h.mu.Lock()
	defer h.mu.Unlock()

	macStr := mac.String()
	now := time.Now()

	// Static reservation (already filtered to p's subnet by reservationFor)
	// takes precedence over the dynamic pool, and over another MAC's live
	// lease on the same address: that lease is revoked first, so the table
	// never holds two leases on one address.
	if reserved != nil {
		h.revokeOtherLeases(macStr, reserved, now)
		h.setLease(macStr, mac, reserved, now.Add(leaseDur))
		return reserved
	}

	// Reservations-only pool (no dynamic range): an OOB gateway that owns no
	// dynamic slice still serves the full reservation set (handled above),
	// but has nothing to dynamically allocate. Return nil here so the
	// dynamic-allocation loop below — which would dupIP(nil)/nextIP(nil) on
	// a nil range — never runs. A non-reserved MAC on such a gateway simply
	// gets no offer from it (the slice-owning gateway answers).
	if p.rangeStart == nil {
		return nil
	}

	// Existing UNEXPIRED lease for this MAC in this pool's range? An
	// expired entry must fall through to the sweep+fresh-allocate path
	// below (setLease there classifies it as "allocate", matching its
	// own doc comment on expired-entry reuse) — without the
	// now.Before(l.Expiry) guard this fast path would revive an
	// expired lease in place and emit "renew" for an interval that had
	// already lapsed, corrupting the ledger's expiry boundary (roast
	// finding, 2026-07-13).
	if l, ok := h.leases[macStr]; ok && now.Before(l.Expiry) {
		if ipInRange(l.IP, p.rangeStart, p.rangeEnd) {
			l.Expiry = now.Add(leaseDur) // renew
			h.persistLease(mac, l.IP, l.Expiry)
			h.emitLeaseEvent("renew", mac, l.IP, l.Expiry)
			return l.IP
		}
		// Lease exists but in a different pool (e.g., MAC moved between
		// direct and relayed networks). The overwrite below replaces
		// the stale entry, freeing the old IP for other MACs. Only a
		// DISCOVER or a REQUEST naming no address gets here: a REQUEST
		// naming this lease was NAKed by classifyRequest.
	}

	// Find a free IP in the range, cleaning up expired leases and expired
	// DECLINE quarantines along the way. A quarantined address is taken.
	taken := make(map[string]bool)
	for k, l := range h.leases {
		if now.After(l.Expiry) {
			h.deleteLease(k, l)
		} else {
			taken[l.IP.String()] = true
		}
	}
	for ip, until := range h.declined {
		if now.Before(until) {
			taken[ip] = true
		} else {
			delete(h.declined, ip)
		}
	}

	for ip := dupIP(p.rangeStart); !ip.Equal(nextIP(p.rangeEnd)); ip = nextIP(ip) {
		if taken[ip.String()] {
			continue
		}
		// ARP probe: check if something else is already using this IP
		if arpProbe != nil {
			// Release the lock during ARP probe (network I/O)
			h.mu.Unlock()
			inUse := arpProbe(ip)
			h.mu.Lock()
			// Re-check THIS MAC first: a retransmitted DISCOVER from the
			// same client may have been allocated by a concurrent goroutine
			// while the lock was released. Without this, the second
			// allocation overwrites h.leases[macStr] with a different IP,
			// leaking the first (offered but no longer backed by a lease).
			if l, ok := h.leases[macStr]; ok && now.Before(l.Expiry) && ipInRange(l.IP, p.rangeStart, p.rangeEnd) {
				return l.IP
			}
			if inUse {
				h.log.Warn("ARP probe detected IP already in use, skipping",
					"ip", ip.String(), "requested_by", macStr)
				taken[ip.String()] = true
				continue
			}
			// Re-check: another goroutine may have claimed this IP while
			// lock was released, or a client may have DECLINEd it.
			until, withheld := h.declined[ip.String()]
			ipClaimed := withheld && now.Before(until)
			for _, l := range h.leases {
				if l.IP.Equal(ip) && now.Before(l.Expiry) {
					ipClaimed = true
					break
				}
			}
			if ipClaimed {
				taken[ip.String()] = true
				continue
			}
		}
		h.setLease(macStr, mac, ip, now.Add(leaseDur))
		return ip
	}

	return nil
}

// setLease writes a lease to the in-memory map, persists it if a store
// is configured, and emits an OnLeaseEvent. Caller must hold h.mu.
//
// Classifies the event as "renew" when this exact (mac, ip) pair
// already held an unexpired entry, "allocate" otherwise — a brand new
// MAC, an IP change for an existing MAC (including adopt-in-place,
// which reaches here only after classifyRequest has confirmed no
// unexpired lease matches reqIP), or reuse of an entry that had
// already expired.
func (h *Handler) setLease(macStr string, mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	event := "allocate"
	if existing, ok := h.leases[macStr]; ok && existing.IP.Equal(ip) && time.Now().Before(existing.Expiry) {
		event = "renew"
	}
	h.leases[macStr] = &lease{MAC: mac, IP: ip, Expiry: expiry}
	h.persistLease(mac, ip, expiry)
	h.emitLeaseEvent(event, mac, ip, expiry)
}

// revokeOtherLeases drops every unexpired lease on ip held by a MAC other
// than macStr. allocateIP's reservation branch calls it before granting a
// reservation, which keeps the table at one holder per address when the
// reservation was handed to a new MAC while the old holder's lease was live,
// or when it lies inside the dynamic range and a dynamic client drew it
// first. The revoked MAC holds no lease any more, so classifyRequest NAKs its
// next REQUEST for the address (the MAC's own reservation differs, or the
// address is outside the dynamic range or leased to the reservation holder)
// and it re-DISCOVERs. Until then it keeps using the
// address on the wire; this keeps the lease table honest, it does not clear
// the wire. If two MACs are both reserved the same address (a data error),
// each grant revokes the other and the warning repeats on every renew.
// Caller must hold h.mu.
func (h *Handler) revokeOtherLeases(macStr string, ip net.IP, now time.Time) {
	for other, l := range h.leases {
		if other == macStr || !l.IP.Equal(ip) || !now.Before(l.Expiry) {
			continue
		}
		h.log.Warn("revoking another MAC's live lease on a reserved address; that MAC is NAKed at its next renew. "+
			"If the address is inside the dynamic pool, move the reservation or the pool so they no longer overlap",
			"ip", ip.String(), "reserved_for", macStr, "revoked_from", other)
		h.deleteLease(other, l)
	}
}

// deleteLease removes macStr's lease l from the in-memory map and the
// LeaseStore and emits a "delete" OnLeaseEvent. Caller must hold h.mu.
func (h *Handler) deleteLease(macStr string, l *lease) {
	delete(h.leases, macStr)
	if h.cfg.LeaseStore != nil {
		h.cfg.LeaseStore.DeleteLease(l.MAC)
	}
	h.emitLeaseEvent("delete", l.MAC, l.IP, l.Expiry)
}

// persistLease writes to the LeaseStore if configured. Caller must hold h.mu.
func (h *Handler) persistLease(mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	if h.cfg.LeaseStore != nil {
		h.cfg.LeaseStore.SaveLease(mac, ip, expiry)
	}
}

// emitLeaseEvent calls Config.OnLeaseEvent if configured. Caller must
// hold h.mu — see OnLeaseEvent's doc comment for why this runs
// synchronously inside the mutation's critical section rather than as
// a goroutine or a channel send.
func (h *Handler) emitLeaseEvent(event string, mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	if h.cfg.OnLeaseEvent == nil {
		return
	}
	h.cfg.OnLeaseEvent(LeaseEvent{
		Event:  event,
		MAC:    mac,
		IP:     ip,
		Expiry: expiry,
		Pool:   h.poolLabelForIP(ip),
	})
}

func ipInRange(ip, start, end net.IP) bool {
	ip4 := ip.To4()
	s4 := start.To4()
	e4 := end.To4()
	if ip4 == nil || s4 == nil || e4 == nil {
		return false
	}
	return string(ip4) >= string(s4) && string(ip4) <= string(e4)
}

func dupIP(ip net.IP) net.IP {
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

func nextIP(ip net.IP) net.IP {
	next := dupIP(ip.To4())
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}
