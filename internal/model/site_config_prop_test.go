package model_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
)

// Property coverage for model.SiteConfig.Validate.
//
// The oracle is independent of production: it parses with net/netip (a
// different parser from the net.ParseIP/net.ParseCIDR pair Validate uses)
// and does the range arithmetic on uint32s it derives itself. It encodes the
// documented contract (docs/user/configuration.md "Site-config model" and the
// Validate comments): slug and netbox_site required; ephemeral pool start/end
// IPv4 with start <= end; each relay pool's range and gateway IPv4 inside an
// IPv4 subnet, start <= end, the gateway outside the range, and on a subnet
// of /30 or wider the range clear of the network and broadcast addresses.
//
// Accepted configs are then fed to the real consumer, the DHCP handler, the
// same way cmd/slagworksd buildPoolConfig converts them, to check that what
// the validator lets through is something the consumer serves sanely.

func init() {
	// The DHCP handler logs every offer at Info; keep property output readable.
	slog.SetDefault(slog.New(slog.DiscardHandler))
}

// ---- generators ----

// ipForm renders a v4 address in one of several spellings. Only "dotted" is
// a strict IPv4 literal; the others probe what Validate accepts.
var ipForms = []string{"dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted", "dotted",
	"mapped", "leading0", "space", "v6", "garbage", "empty"}

// exoticForms gates the non-dotted spellings; drawSiteConfig turns them off
// for most cases so that acceptance (the interesting side of the oracle) is
// common. Tests here do not run in parallel.
var exoticForms = true

func renderIP(t *rapid.T, v uint32, label string) string {
	a := u32ToAddr(v)
	if !exoticForms {
		return a.String()
	}
	switch rapid.SampledFrom(ipForms).Draw(t, label+"Form") {
	case "mapped":
		return "::ffff:" + a.String()
	case "leading0":
		b := a.As4()
		return fmt.Sprintf("%03d.%d.%d.%d", b[0], b[1], b[2], b[3])
	case "space":
		return " " + a.String()
	case "v6":
		return "fd00::" + fmt.Sprintf("%x", v&0xffff)
	case "garbage":
		return rapid.SampledFrom([]string{"not-an-ip", "10.0.0", "10.0.0.256", "1.2.3.4/24", "-"}).Draw(t, label+"Garbage")
	case "empty":
		return ""
	default:
		return a.String()
	}
}

var subnetForms = []string{"canon", "canon", "canon", "canon", "canon", "canon", "canon", "canon", "hostbits", "mapped", "v6", "garbage"}

func renderSubnet(t *rapid.T, base uint32, bits int) string {
	a := u32ToAddr(base)
	if !exoticForms {
		return fmt.Sprintf("%s/%d", a, bits)
	}
	switch rapid.SampledFrom(subnetForms).Draw(t, "subnetForm") {
	case "hostbits":
		// net.ParseCIDR and netip.ParsePrefix both accept host bits set.
		hb := uint32(0)
		if bits < 32 {
			hb = rapid.Uint32Range(0, (1<<(32-bits))-1).Draw(t, "hostbitsVal")
		}
		return fmt.Sprintf("%s/%d", u32ToAddr(base|hb), bits)
	case "mapped":
		return fmt.Sprintf("::ffff:%s/%d", a, 96+bits)
	case "v6":
		return fmt.Sprintf("fd00::/%d", 64+bits/4)
	case "garbage":
		return rapid.SampledFrom([]string{"not-a-cidr", "10.0.0.0", "10.0.0.0/33", "10.0.0.0/-1"}).Draw(t, "subnetGarbage")
	default:
		return fmt.Sprintf("%s/%d", a, bits)
	}
}

func u32ToAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func addrToU32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// inSubnet draws an address inside base/bits, biased toward the edges
// (network, broadcast, first/last host) because those are the values a
// validator most often mishandles.
func inSubnet(t *rapid.T, base uint32, bits int, label string) uint32 {
	size := uint64(1) << (32 - bits)
	edge := rapid.IntRange(0, 5).Draw(t, label+"Edge")
	switch edge {
	case 0:
		return base
	case 1:
		return base + uint32(size-1)
	case 2:
		if size > 2 {
			return base + 1
		}
	case 3:
		if size > 2 {
			return base + uint32(size-2)
		}
	}
	return base + uint32(rapid.Uint64Range(0, size-1).Draw(t, label+"Off"))
}

type genRelay struct {
	pool model.RelayPool
	// The intended numeric values, used only for coverage accounting.
	base       uint32
	bits       int
	start, end uint32
	gw         uint32
}

func drawRelay(t *rapid.T, i int) genRelay {
	bits := rapid.SampledFrom([]int{8, 16, 22, 23, 24, 24, 24, 25, 28, 29, 30, 30, 31, 32}).Draw(t, fmt.Sprintf("r%dBits", i))
	// Small pool of /8 bases so pools on one config can collide.
	raw := uint32(10)<<24 | uint32(rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("r%dB", i)))<<16 |
		uint32(rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("r%dC", i)))<<8
	mask := uint32(0xffffffff) << (32 - bits)
	if bits == 0 {
		mask = 0
	}
	base := raw & mask
	s := inSubnet(t, base, bits, fmt.Sprintf("r%dStart", i))
	e := inSubnet(t, base, bits, fmt.Sprintf("r%dEnd", i))
	switch rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("r%dOrder", i)) {
	case 0: // as drawn, possibly reversed
	case 1: // off-by-one reversed boundary
		if s > base {
			e = s - 1
		}
	default:
		if s > e {
			s, e = e, s
		}
	}
	var gw uint32
	switch rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("r%dGwKind", i)) {
	case 0: // gateway at the first host, the conventional place
		gw = base + 1
		if bits >= 31 {
			gw = base
		}
	case 1: // gateway somewhere in the range
		if e >= s {
			gw = s + uint32(rapid.Uint64Range(0, uint64(e-s)).Draw(t, fmt.Sprintf("r%dGwOff", i)))
		} else {
			gw = s
		}
	case 2: // anywhere in the subnet
		gw = inSubnet(t, base, bits, fmt.Sprintf("r%dGw", i))
	default: // outside the subnet
		gw = base ^ 0x00800000
	}
	return genRelay{
		pool: model.RelayPool{
			Subnet:     renderSubnet(t, base, bits),
			RangeStart: renderIP(t, s, fmt.Sprintf("r%dS", i)),
			RangeEnd:   renderIP(t, e, fmt.Sprintf("r%dE", i)),
			Gateway:    renderIP(t, gw, fmt.Sprintf("r%dG", i)),
		},
		base: base, bits: bits, start: s, end: e, gw: gw,
	}
}

func drawSiteConfig(t *rapid.T) (model.SiteConfig, []genRelay) {
	exoticForms = rapid.IntRange(0, 9).Draw(t, "exotic") < 3
	defer func() { exoticForms = true }()
	cfg := model.SiteConfig{
		Slug:           rapid.SampledFrom([]string{"pop1", "pop2-old-cage", "pöp3", "a b", "pop1", ""}).Draw(t, "slug"),
		NetBoxSite:     rapid.SampledFrom([]string{"pop1", "pop2", "pop2", "pop1", "pop2", ""}).Draw(t, "netboxSite"),
		NetBoxLocation: rapid.SampledFrom([]string{"", "", "cage-a"}).Draw(t, "netboxLocation"),
		Promiscuous:    rapid.Bool().Draw(t, "promisc"),
	}
	s := rapid.Uint32().Draw(t, "ephStart")
	e := s + uint32(rapid.IntRange(-3, 300).Draw(t, "ephLen"))
	cfg.EphemeralPoolStart = renderIP(t, s, "ephS")
	cfg.EphemeralPoolEnd = renderIP(t, e, "ephE")
	n := rapid.IntRange(0, 3).Draw(t, "nRelay")
	var relays []genRelay
	for i := range n {
		r := drawRelay(t, i)
		relays = append(relays, r)
		cfg.RelayPools = append(cfg.RelayPools, r.pool)
	}
	return cfg, relays
}

// ---- independent oracle ----

func strictV4(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Addr{}, false
	}
	return a, true
}

func strictV4Prefix(s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, false
	}
	return p.Masked(), true
}

// oracleRelay returns "" when the pool satisfies the documented contract,
// else a reason.
func oracleRelay(p model.RelayPool) string {
	pfx, ok := strictV4Prefix(p.Subnet)
	if !ok {
		return "subnet not an IPv4 CIDR"
	}
	s, ok1 := strictV4(p.RangeStart)
	e, ok2 := strictV4(p.RangeEnd)
	g, ok3 := strictV4(p.Gateway)
	if !ok1 || !ok2 {
		return "range not IPv4"
	}
	if !ok3 {
		return "gateway not IPv4"
	}
	if !pfx.Contains(s) || !pfx.Contains(e) {
		return "range outside subnet"
	}
	if !pfx.Contains(g) {
		return "gateway outside subnet"
	}
	su, eu, gu := addrToU32(s), addrToU32(e), addrToU32(g)
	if su > eu {
		return "range reversed"
	}
	// The DHCP handler leases every address in the range and cannot
	// ARP-probe a relayed subnet, so the range must hold only addresses a
	// client can own.
	if su <= gu && gu <= eu {
		return "gateway inside range"
	}
	if bits := pfx.Bits(); bits <= 30 {
		network := addrToU32(pfx.Addr())
		broadcast := network + uint32((uint64(1)<<(32-bits))-1)
		if su == network || eu == broadcast {
			return "range covers network or broadcast address"
		}
	}
	return ""
}

func oracleConfig(c model.SiteConfig) string {
	if c.Slug == "" {
		return "slug"
	}
	if c.NetBoxSite == "" {
		return "netbox_site"
	}
	s, ok1 := strictV4(c.EphemeralPoolStart)
	e, ok2 := strictV4(c.EphemeralPoolEnd)
	if !ok1 || !ok2 {
		return "ephemeral not IPv4"
	}
	if addrToU32(s) > addrToU32(e) {
		return "ephemeral reversed"
	}
	for i, p := range c.RelayPools {
		if r := oracleRelay(p); r != "" {
			return fmt.Sprintf("relay %d: %s", i, r)
		}
	}
	return ""
}

// isMappedSpelling reports whether s is an IPv4-mapped IPv6 spelling of an
// address ("::ffff:a.b.c.d"). Validate's net.ParseIP(..).To4() accepts
// these for pool and gateway addresses; whether that is intended is left
// open rather than failed on. A relay subnet spelled that way is not left
// open: Validate must reject it, as the oracle does.
func isMappedSpelling(s string) bool {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().Is4In6()
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Is4In6()
	}
	return false
}

func configHasMapped(c model.SiteConfig) bool {
	if isMappedSpelling(c.EphemeralPoolStart) || isMappedSpelling(c.EphemeralPoolEnd) {
		return true
	}
	for _, p := range c.RelayPools {
		if isMappedSpelling(p.RangeStart) || isMappedSpelling(p.RangeEnd) || isMappedSpelling(p.Gateway) {
			return true
		}
	}
	return false
}

// TestPropSiteConfigValidateMatchesOracle: Validate accepts exactly the
// configs the documented contract accepts. Configs with an IPv4-mapped
// address spelling are counted but not compared (see isMappedSpelling).
func TestPropSiteConfigValidateMatchesOracle(t *testing.T) {
	var accepted, rejected, mapped atomic.Int64
	rapid.Check(t, func(t *rapid.T) {
		cfg, _ := drawSiteConfig(t)
		got := cfg.Validate()
		want := oracleConfig(cfg)
		if configHasMapped(cfg) {
			mapped.Add(1)
			return
		}
		if got == nil {
			accepted.Add(1)
		} else {
			rejected.Add(1)
		}
		if (got == nil) != (want == "") {
			t.Fatalf("Validate=%v oracle=%q for %+v", got, want, cfg)
		}
	})
	t.Logf("accepted=%d rejected=%d mapped-spelling=%d", accepted.Load(), rejected.Load(), mapped.Load())
}

// TestPropSiteConfigDefectInjection: start from a valid config, break
// exactly one field, require rejection.
func TestPropSiteConfigDefectInjection(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg := model.SiteConfig{
			Slug: "pop1", NetBoxSite: "pop1",
			EphemeralPoolStart: "10.9.0.100", EphemeralPoolEnd: "10.9.0.200",
			RelayPools: []model.RelayPool{
				{Subnet: "10.1.0.0/24", RangeStart: "10.1.0.100", RangeEnd: "10.1.0.200", Gateway: "10.1.0.1"},
				{Subnet: "10.2.0.0/31", RangeStart: "10.2.0.0", RangeEnd: "10.2.0.0", Gateway: "10.2.0.1"},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("base config rejected: %v", err)
		}
		i := rapid.IntRange(0, 1).Draw(t, "pool")
		bad := rapid.SampledFrom([]string{"", "x", "fd00::1", "300.1.1.1", "10.1.0", "1.2.3.4/32"}).Draw(t, "badValue")
		switch rapid.IntRange(0, 9).Draw(t, "defect") {
		case 0:
			cfg.Slug = ""
		case 1:
			cfg.NetBoxSite = ""
		case 2:
			cfg.EphemeralPoolStart = bad
		case 3:
			cfg.EphemeralPoolEnd = bad
		case 4:
			cfg.EphemeralPoolStart, cfg.EphemeralPoolEnd = "10.9.0.201", "10.9.0.200"
		case 5:
			cfg.RelayPools[i].Subnet = bad
		case 6:
			cfg.RelayPools[i].RangeStart = bad
		case 7:
			cfg.RelayPools[i].Gateway = "10.77.0.1" // outside both subnets
		case 8:
			cfg.RelayPools[i].RangeEnd = "10.77.0.2"
		case 9:
			p := &cfg.RelayPools[0]
			p.RangeStart, p.RangeEnd = p.RangeEnd, p.RangeStart
		}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("defective config accepted: %+v", cfg)
		}
	})
}

// relaySubnetFromModel is cmd/slagworksd buildPoolConfig's per-pool
// conversion, restated: ParseCIDR the subnet, ParseIP the three addresses.
func relaySubnetFromModel(rp model.RelayPool) (dhcp.RelaySubnet, error) {
	_, n, err := net.ParseCIDR(rp.Subnet)
	if err != nil {
		return dhcp.RelaySubnet{}, err
	}
	s, e, g := net.ParseIP(rp.RangeStart), net.ParseIP(rp.RangeEnd), net.ParseIP(rp.Gateway)
	if s == nil || e == nil || g == nil {
		return dhcp.RelaySubnet{}, fmt.Errorf("bad ip")
	}
	return dhcp.RelaySubnet{Subnet: *n, RangeStart: s, RangeEnd: e, Gateway: g}, nil
}

// offerFor runs one relayed DISCOVER through a fresh handler serving only
// this relay pool and returns the offer. No ARP probe is wired: the worker's
// prober egresses its local segment and cannot see a relayed subnet, so for
// relayed clients it never reports an address in use.
func offerFor(t *rapid.T, rs dhcp.RelaySubnet, giaddr net.IP) *dhcpv4.DHCPv4 {
	cfg := dhcp.Config{
		ServerIP:      net.ParseIP("192.0.2.1"),
		LeaseDuration: time.Minute,
		HTTPBootPort:  8080,
		PoolConfig: dhcp.PoolConfig{
			Subnet:       net.IPNet{IP: net.ParseIP("192.0.2.0"), Mask: net.CIDRMask(24, 32)},
			RangeStart:   net.ParseIP("192.0.2.100"),
			RangeEnd:     net.ParseIP("192.0.2.200"),
			Gateway:      net.ParseIP("192.0.2.1"),
			RelaySubnets: []dhcp.RelaySubnet{rs},
		},
	}
	h := dhcp.NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:12:34:56")
	d, err := dhcpv4.NewDiscovery(mac, dhcpv4.WithOption(dhcpv4.OptClientArch(iana.EFI_X86_64)),
		func(d *dhcpv4.DHCPv4) { d.GatewayIPAddr = giaddr })
	if err != nil {
		t.Fatal(err)
	}
	return h.HandlePacketOn(d, "")
}

// servedRelay draws relay pools until Validate accepts one (giving up after a
// few attempts, in which case the offer is nil) and serves one relayed
// DISCOVER for it with giaddr = the pool's gateway.
func servedRelay(t *rapid.T) (genRelay, *dhcpv4.DHCPv4) {
	var r genRelay
	ok := false
	for range 8 {
		exoticForms = rapid.IntRange(0, 9).Draw(t, "exotic") < 2
		r = drawRelay(t, 0)
		exoticForms = true
		cfg := model.SiteConfig{Slug: "s", NetBoxSite: "s", EphemeralPoolStart: "10.200.0.1", EphemeralPoolEnd: "10.200.0.9",
			RelayPools: []model.RelayPool{r.pool}}
		if cfg.Validate() == nil {
			ok = true
			break
		}
	}
	if !ok {
		return r, nil
	}
	rs, err := relaySubnetFromModel(r.pool)
	if err != nil {
		t.Fatalf("Validate accepted a pool the worker cannot convert: %v (%+v)", err, r.pool)
	}
	offer := offerFor(t, rs, net.ParseIP(r.pool.Gateway))
	if offer == nil {
		t.Fatalf("no DHCP offer for accepted relay pool %+v", r.pool)
	}
	return r, offer
}

// TestPropSiteConfigAcceptedMaskOnWire: the subnet-mask option the handler
// sends for an accepted relay pool is that pool's IPv4 mask.
func TestPropSiteConfigAcceptedMaskOnWire(t *testing.T) {
	var served atomic.Int64
	rapid.Check(t, func(t *rapid.T) {
		r, offer := servedRelay(t)
		if offer == nil {
			return
		}
		served.Add(1)
		wantMask := net.IP(net.CIDRMask(r.bits, 32)).To4()
		gotMask := net.IP(offer.SubnetMask()).To4()
		if !gotMask.Equal(wantMask) {
			t.Fatalf("subnet-mask option %v, want %v for accepted pool %+v", net.IP(offer.SubnetMask()), wantMask, r.pool)
		}
	})
	t.Logf("served=%d", served.Load())
}

// TestPropSiteConfigAcceptedNeverOffersRouter: an accepted relay pool never
// leads the handler to offer a client the router address the same offer
// names (the leaf VIP).
func TestPropSiteConfigAcceptedNeverOffersRouter(t *testing.T) {
	var served atomic.Int64
	rapid.Check(t, func(t *rapid.T) {
		r, offer := servedRelay(t)
		if offer == nil {
			return
		}
		served.Add(1)
		yi := offer.YourIPAddr.To4()
		pfx := netip.PrefixFrom(u32ToAddr(r.base), r.bits)
		ya, _ := netip.AddrFromSlice(yi)
		if !pfx.Contains(ya) {
			t.Fatalf("offered %v outside subnet %v", yi, pfx)
		}
		for _, rt := range offer.Router() {
			if rt.Equal(yi) {
				t.Fatalf("offered the router address %v to a client (pool %+v)", yi, r.pool)
			}
		}
	})
	t.Logf("served=%d", served.Load())
}

// TestPropSiteConfigAcceptedNeverOffersNetworkOrBroadcast: for subnets with
// a distinct network and broadcast address (prefix <= /30), an accepted
// relay pool never leads to offering either.
func TestPropSiteConfigAcceptedNeverOffersNetworkOrBroadcast(t *testing.T) {
	var served atomic.Int64
	rapid.Check(t, func(t *rapid.T) {
		r, offer := servedRelay(t)
		if offer == nil {
			return
		}
		served.Add(1)
		if r.bits > 30 {
			return
		}
		ya, _ := netip.AddrFromSlice(offer.YourIPAddr.To4())
		bcast := r.base + uint32((uint64(1)<<(32-r.bits))-1)
		if v := addrToU32(ya); v == r.base || v == bcast {
			t.Fatalf("offered the network/broadcast address %v of %v/%d (pool %+v)", ya, u32ToAddr(r.base), r.bits, r.pool)
		}
	})
	t.Logf("served=%d", served.Load())
}

// TestPropSiteConfigJSONRoundTrip: SiteConfig survives the API's JSON
// encoding exactly, including nil vs empty RelayPools and unicode.
func TestPropSiteConfigJSONRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg, _ := drawSiteConfig(t)
		cfg.LastSnapshotSeq = rapid.Int64().Draw(t, "seq")
		cfg.LastSnapshotHash = rapid.String().Draw(t, "hash")
		// encoding/json refuses years past 9999; stay inside RFC 3339.
		cfg.CreatedAt = time.Unix(rapid.Int64Range(-62135596800, 253402300799).Draw(t, "sec"), rapid.Int64Range(0, 999999999).Draw(t, "ns")).UTC()
		if rapid.Bool().Draw(t, "emptyPools") && cfg.RelayPools == nil {
			cfg.RelayPools = []model.RelayPool{}
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var back model.SiteConfig
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !back.CreatedAt.Equal(cfg.CreatedAt) {
			t.Fatalf("created_at %v != %v", back.CreatedAt, cfg.CreatedAt)
		}
		back.CreatedAt = cfg.CreatedAt
		if !reflect.DeepEqual(back, cfg) {
			t.Fatalf("round trip:\n got %#v\nwant %#v", back, cfg)
		}
	})
}
