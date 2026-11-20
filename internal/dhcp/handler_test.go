package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

func testConfig() Config {
	return Config{
		ServerIP: net.ParseIP("10.99.1.1"),
		PoolConfig: PoolConfig{
			Subnet:     net.IPNet{IP: net.ParseIP("10.99.1.0"), Mask: net.CIDRMask(24, 32)},
			RangeStart: net.ParseIP("10.99.1.100"),
			RangeEnd:   net.ParseIP("10.99.1.200"),
			Gateway:    net.ParseIP("10.99.1.1"),
		},
		DNS:           []net.IP{net.ParseIP("1.1.1.1")},
		LeaseDuration: 10 * time.Minute,
		HTTPBootPort:  8080,
	}
}

func mustDiscover(t *testing.T, mac net.HardwareAddr, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	pkt, err := dhcpv4.NewDiscovery(mac, mods...)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

func mustRequestFromOffer(t *testing.T, offer *dhcpv4.DHCPv4, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	pkt, err := dhcpv4.NewRequestFromOffer(offer, mods...)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

func withArch(arch iana.Arch) dhcpv4.Modifier {
	return dhcpv4.WithOption(dhcpv4.OptClientArch(arch))
}

func withUserClass(class string) dhcpv4.Modifier {
	return dhcpv4.WithUserClass(class, false)
}

// withVendorClass sets DHCP option 60 (Class Identifier). UEFI HTTP-boot
// firmware sends this beginning with "HTTPClient".
func withVendorClass(class string) dhcpv4.Modifier {
	return dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte(class))
}

// withPXEClient sets option 60 the way a PXE ROM does (PXE 2.1 requires the
// "PXEClient" prefix; this is QEMU's iPXE/SeaBIOS ROM string). bootFileFor
// only serves a TFTP boot file to requests carrying it.
func withPXEClient() dhcpv4.Modifier {
	return withVendorClass("PXEClient:Arch:00000:UNDI:002001")
}

func TestHandleDiscover_BasicOffer(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("expected Offer, got %v", reply.MessageType())
	}
	if reply.YourIPAddr.Equal(net.IPv4zero) {
		t.Fatal("expected a non-zero offered IP")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected first IP in range (10.99.1.100), got %v", reply.YourIPAddr)
	}
}

func TestHandleDiscover_SetsStandardOptions(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}

	// Subnet mask
	mask := reply.SubnetMask()
	if mask == nil {
		t.Fatal("expected subnet mask option")
	}
	expected := net.CIDRMask(24, 32)
	if !net.IP(mask).Equal(net.IP(expected)) {
		t.Fatalf("expected subnet mask %v, got %v", expected, mask)
	}

	// Router
	routers := reply.Router()
	if len(routers) == 0 {
		t.Fatal("expected router option")
	}
	if !routers[0].Equal(net.ParseIP("10.99.1.1")) {
		t.Fatalf("expected router 10.99.1.1, got %v", routers[0])
	}

	// DNS
	dns := reply.DNS()
	if len(dns) == 0 {
		t.Fatal("expected DNS option")
	}
	if !dns[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Fatalf("expected DNS 1.1.1.1, got %v", dns[0])
	}

	// Lease time
	leaseTime := reply.IPAddressLeaseTime(0)
	if leaseTime != 10*time.Minute {
		t.Fatalf("expected lease time 10m, got %v", leaseTime)
	}

	// Next server (siaddr)
	if !reply.ServerIPAddr.Equal(net.ParseIP("10.99.1.1")) {
		t.Fatalf("expected siaddr 10.99.1.1, got %v", reply.ServerIPAddr)
	}
}

func TestHandleDiscover_UEFIArchitecture(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	for _, arch := range []iana.Arch{iana.EFI_BC, iana.EFI_X86_64} {
		discover := mustDiscover(t, mac, withArch(arch), withPXEClient())
		reply := h.HandlePacket(discover)

		if reply == nil {
			t.Fatalf("arch %v: expected an offer, got nil", arch)
		}
		if reply.BootFileName != "snponly.efi" {
			t.Fatalf("arch %v: expected boot file snponly.efi, got %q", arch, reply.BootFileName)
		}
	}
}

func TestHandleDiscover_BIOSArchitecture(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.INTEL_X86PC), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName != "undionly.kpxe" {
		t.Fatalf("expected boot file undionly.kpxe, got %q", reply.BootFileName)
	}
}

func TestHandleDiscover_MissingArch_DefaultsBIOS(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// No withArch modifier -- option 93 is absent
	discover := mustDiscover(t, mac, withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName != "undionly.kpxe" {
		t.Fatalf("expected boot file undionly.kpxe for missing arch, got %q", reply.BootFileName)
	}
}

func TestHandleDiscover_UnrecognizedArch_DefaultsUEFI(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.Arch(99)), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName != "snponly.efi" {
		t.Fatalf("expected boot file snponly.efi for unknown arch, got %q", reply.BootFileName)
	}
}

func TestHandleDiscover_iPXEClient_HTTPBootFilename(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withUserClass("slagworks-ipxe"))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	expected := "http://10.99.1.1:8080/boot/ipxe"
	if reply.BootFileName != expected {
		t.Fatalf("expected boot file %q, got %q", expected, reply.BootFileName)
	}
}

func TestHandleDiscover_GenericIPXE_GetsTFTPNotHTTP(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withUserClass("iPXE"), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName != "snponly.efi" {
		t.Fatalf("generic iPXE ROM should get TFTP filename, got %q", reply.BootFileName)
	}
}

func TestHandleDiscover_NonIPXEClient_TFTPBootFilename(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withUserClass("SomeOtherClient"), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName != "snponly.efi" {
		t.Fatalf("expected TFTP boot file snponly.efi, got %q", reply.BootFileName)
	}
}

// UEFI HTTP-boot firmware (option 60 "HTTPClient...") must get an HTTP URL to
// the iPXE binary, not a bare TFTP filename, AND the reply must echo option 60
// "HTTPClient" or the firmware ignores the URL.
func TestHandleDiscover_HTTPBootClient_GetsHTTPURLAndEchoesOption60(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// Real firmware sends e.g. "HTTPClient:Arch:00016:UNDI:003000".
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64),
		withVendorClass("HTTPClient:Arch:00016:UNDI:003000"))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	expected := "http://10.99.1.1:8080/files/ipxe/snponly.efi"
	if reply.BootFileName != expected {
		t.Fatalf("HTTP-boot client should get HTTP URL %q, got %q", expected, reply.BootFileName)
	}
	// The reply must confirm HTTP boot by echoing option 60 = "HTTPClient".
	if got := reply.ClassIdentifier(); got != "HTTPClient" {
		t.Fatalf("reply must echo option 60 \"HTTPClient\", got %q", got)
	}
}

// A plain (non-HTTP-boot) firmware reply must NOT carry the HTTPClient option
// 60 echo — that's only for HTTP-boot clients.
func TestHandleDiscover_PlainPXE_NoHTTPClientEcho(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if got := reply.ClassIdentifier(); got != "" {
		t.Fatalf("plain PXE reply must not echo HTTPClient option 60, got %q", got)
	}
}

// All DHCP Offers and Acks must include a PXE bootfile, including for
// machines that should ultimately boot from local disk. The httpboot
// handler routes the resulting iPXE chainload to bootLocalScript when the
// machine's BootAction is BootLocal.
//
// History: an earlier design returned an empty bootfile to "let firmware
// fall through to disk." Supermicro UEFI firmware (and likely others) does
// not fall through gracefully; it loops on PXE-E16 ("No valid offer
// received"), adding 60-90s of operator-visible delay to every reboot of a
// deployed machine. Always serving a bootfile and routing in iPXE is the
// fix. Don't reintroduce the empty-bootfile shortcut.
func TestHandleDiscover_AlwaysIncludesBootfile(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withPXEClient())
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if reply.BootFileName == "" {
		t.Fatal("DHCP Offer had empty bootfile; firmware will loop on PXE-E16. " +
			"Expected the iPXE TFTP/HTTP filename so iPXE can chainload and " +
			"serve a boot-local script.")
	}
}

func TestHandleRequest_AlwaysIncludesBootfile(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withPXEClient())
	offer := h.HandlePacket(discover)
	if offer == nil {
		t.Fatal("expected an offer, got nil")
	}
	request := mustRequestFromOffer(t, offer, withPXEClient())
	ack := h.HandlePacket(request)
	if ack == nil {
		t.Fatal("expected an ack, got nil")
	}
	if ack.BootFileName == "" {
		t.Fatal("DHCP Ack had empty bootfile; firmware will not chainload iPXE")
	}
}

func TestDORA_FullCycle(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// Discover
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer := h.HandlePacket(discover)
	if offer == nil {
		t.Fatal("expected offer")
	}
	if offer.MessageType() != dhcpv4.MessageTypeOffer {
		t.Fatalf("expected Offer, got %v", offer.MessageType())
	}

	// Request
	request := mustRequestFromOffer(t, offer)
	ack := h.HandlePacket(request)
	if ack == nil {
		t.Fatal("expected ack")
	}
	if ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("expected Ack, got %v", ack.MessageType())
	}

	// Same IP in offer and ack
	if !offer.YourIPAddr.Equal(ack.YourIPAddr) {
		t.Fatalf("offer IP %v != ack IP %v", offer.YourIPAddr, ack.YourIPAddr)
	}
}

func TestLeaseTracking_SameMAC_SameIP(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	d1 := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer1 := h.HandlePacket(d1)

	d2 := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer2 := h.HandlePacket(d2)

	if !offer1.YourIPAddr.Equal(offer2.YourIPAddr) {
		t.Fatalf("same MAC should get same IP: %v vs %v", offer1.YourIPAddr, offer2.YourIPAddr)
	}
}

func TestLeaseTracking_DifferentMACs_DifferentIPs(t *testing.T) {
	h := NewHandler(testConfig())
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")

	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	offer1 := h.HandlePacket(d1)

	d2 := mustDiscover(t, mac2, withArch(iana.EFI_X86_64))
	offer2 := h.HandlePacket(d2)

	if offer1.YourIPAddr.Equal(offer2.YourIPAddr) {
		t.Fatalf("different MACs should get different IPs: both got %v", offer1.YourIPAddr)
	}
}

func TestLeaseTracking_PoolExhaustion(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.101") // Only 2 IPs
	h := NewHandler(cfg)

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	mac3, _ := net.ParseMAC("52:54:00:aa:bb:03")

	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	if h.HandlePacket(d1) == nil {
		t.Fatal("expected offer for mac1")
	}

	d2 := mustDiscover(t, mac2, withArch(iana.EFI_X86_64))
	if h.HandlePacket(d2) == nil {
		t.Fatal("expected offer for mac2")
	}

	d3 := mustDiscover(t, mac3, withArch(iana.EFI_X86_64))
	reply := h.HandlePacket(d3)
	if reply != nil {
		t.Fatalf("expected nil (pool exhausted), got offer for %v", reply.YourIPAddr)
	}
}

func TestLeaseTracking_ExpiredLeaseReused(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.100") // Only 1 IP
	cfg.LeaseDuration = 1 * time.Millisecond  // Expire fast
	h := NewHandler(cfg)

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	offer1 := h.HandlePacket(d1)
	if offer1 == nil {
		t.Fatal("expected offer for mac1")
	}

	// Wait for lease to expire
	time.Sleep(5 * time.Millisecond)

	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	d2 := mustDiscover(t, mac2, withArch(iana.EFI_X86_64))
	offer2 := h.HandlePacket(d2)
	if offer2 == nil {
		t.Fatal("expected offer for mac2 after lease expiry")
	}
	if !offer2.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected reused IP 10.99.1.100, got %v", offer2.YourIPAddr)
	}
}

func TestHandlePacket_Filtering_DropsUnknownMAC(t *testing.T) {
	knownMAC := "52:54:00:aa:bb:01"
	unknownMAC := "52:54:00:ff:ff:ff"

	cfg := testConfig()
	cfg.KnownMACs = func(mac string) bool { return mac == knownMAC }
	h := NewHandler(cfg)

	mac, _ := net.ParseMAC(unknownMAC)
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	if h.HandlePacket(discover) != nil {
		t.Fatal("expected nil for unknown MAC when KnownMACs is set")
	}

	mac, _ = net.ParseMAC(knownMAC)
	discover = mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	if h.HandlePacket(discover) == nil {
		t.Fatal("expected offer for known MAC when KnownMACs is set")
	}
}

func TestHandlePacket_Promiscuous_AllowsUnknownMAC(t *testing.T) {
	cfg := testConfig()
	cfg.KnownMACs = nil // promiscuous -- no filter
	h := NewHandler(cfg)

	mac, _ := net.ParseMAC("52:54:00:ff:ff:ff")
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	if h.HandlePacket(discover) == nil {
		t.Fatal("expected offer for any MAC when KnownMACs is nil (promiscuous)")
	}
}

func relayConfig() Config {
	cfg := testConfig()
	cfg.RelaySubnets = []RelaySubnet{{
		Subnet:     net.IPNet{IP: net.ParseIP("10.99.2.0"), Mask: net.CIDRMask(24, 32)},
		RangeStart: net.ParseIP("10.99.2.100"),
		RangeEnd:   net.ParseIP("10.99.2.200"),
		Gateway:    net.ParseIP("10.99.2.1"),
	}}
	return cfg
}

func withGiaddr(ip net.IP) dhcpv4.Modifier {
	return func(d *dhcpv4.DHCPv4) {
		d.GatewayIPAddr = ip
	}
}

func TestRelay_AllocatesFromRelayPool(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected offer")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.2.100")) {
		t.Fatalf("expected IP from relay pool (10.99.2.100), got %v", reply.YourIPAddr)
	}
}

func TestRelay_SetsCorrectGatewayAndMask(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected offer")
	}

	routers := reply.Router()
	if len(routers) == 0 || !routers[0].Equal(net.ParseIP("10.99.2.1")) {
		t.Fatalf("expected router 10.99.2.1, got %v", routers)
	}

	mask := reply.SubnetMask()
	if !net.IP(mask).Equal(net.IP(net.CIDRMask(24, 32))) {
		t.Fatalf("expected /24 mask, got %v", mask)
	}
}

// A /31 relay pool may be built from either endpoint's NetBox IP record. The
// relay's giaddr is the authoritative leaf side, so DHCP must always offer the
// peer address and advertise giaddr itself as the router. This reproduces the
// POP2 failure where leaf records sorted before host records and firmware was
// incorrectly told to use its own offered address as its gateway.
func TestRelay_PointToPointDerivesClientAndGatewayFromGIAddr(t *testing.T) {
	cfg := testConfig()
	cfg.RelaySubnets = []RelaySubnet{{
		Subnet:     net.IPNet{IP: net.ParseIP("10.253.28.52"), Mask: net.CIDRMask(31, 32)},
		RangeStart: net.ParseIP("10.253.28.52"),
		RangeEnd:   net.ParseIP("10.253.28.52"),
		Gateway:    net.ParseIP("10.253.28.53"),
	}}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:31")
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.253.28.52")))

	reply := h.HandlePacket(discover)
	if reply == nil {
		t.Fatal("expected offer")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.253.28.53")) {
		t.Fatalf("offered address = %v, want peer 10.253.28.53", reply.YourIPAddr)
	}
	routers := reply.Router()
	if len(routers) != 1 || !routers[0].Equal(net.ParseIP("10.253.28.52")) {
		t.Fatalf("routers = %v, want relay giaddr 10.253.28.52", routers)
	}
}

func TestRelay_PreservesGiaddr(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected offer")
	}
	if !reply.GatewayIPAddr.Equal(net.ParseIP("10.99.2.1")) {
		t.Fatalf("expected giaddr 10.99.2.1, got %v", reply.GatewayIPAddr)
	}
}

func TestRelay_DropsUnknownGiaddr(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.99.1")))
	reply := h.HandlePacket(discover)

	if reply != nil {
		t.Fatalf("expected nil for unknown giaddr, got offer for %v", reply.YourIPAddr)
	}
}

// TestRelay_GiaddrInDirectSubnet_UsesDirectPool covers the case where a
// DHCP packet is relayed (giaddr set) but the giaddr falls inside the
// worker's authoritative Subnet — i.e. the worker isn't L2-adjacent to
// its own served subnet, but it IS the authority for it. This is the
// pop1 topology: slagworksd binds to a leaf↔oob-gw P2P /31, the leaf
// switches relay DHCP from the kworker subnet (which is configured as
// the worker's primary Subnet) using their bridge IP as giaddr.
//
// Without this case, the handler routes by giaddr only via RelaySubnets
// and drops everything when the relay-pool list is empty, which it is
// for single-subnet sites.
func TestRelay_GiaddrInDirectSubnet_UsesDirectPool(t *testing.T) {
	cfg := testConfig() // Subnet 10.99.1.0/24, pool .100-.200, gw .1
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// giaddr is the leaf's address inside the served subnet (a
	// hypothetical bridge IP). The handler should treat this as a
	// relayed request that lands in the direct pool.
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.1.2")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected offer for relayed request whose giaddr is inside the worker's own subnet")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected first IP from direct pool (10.99.1.100), got %v", reply.YourIPAddr)
	}

	// The reply must be addressed back through the relay (giaddr
	// preserved), otherwise the leaf can't forward the OFFER to the
	// client.
	if !reply.GatewayIPAddr.Equal(net.ParseIP("10.99.1.2")) {
		t.Fatalf("expected giaddr 10.99.1.2 preserved, got %v", reply.GatewayIPAddr)
	}
}

// TestRelay_NAKSetsBroadcastBit: RFC 2131 4.3.2 says "If 'giaddr' is set in
// the DHCPREQUEST message, the client is on a different subnet. The server
// MUST set the broadcast bit in the DHCPNAK, so that the relay agent will
// broadcast the DHCPNAK to the client". A NAK carries no yiaddr, so a relay
// that honors a clear bit has no address to unicast it to and the client
// never hears it. The request here leaves the bit clear, as an INIT-REBOOT
// client normally does, so the NAK must set it rather than copy it.
func TestRelay_NAKSetsBroadcastBit(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:41")

	// INIT-REBOOT through the relay for an address from some other network.
	req := initReboot(t, mac, "192.168.7.7")
	req.GatewayIPAddr = net.ParseIP("10.99.2.1")
	if req.IsBroadcast() {
		t.Fatal("setup: request must leave the broadcast bit clear")
	}

	reply := h.HandlePacket(req)
	if !isNAK(reply) {
		t.Fatalf("setup: want DHCPNAK for a wrong-network INIT-REBOOT, got %v", reply)
	}
	if !reply.IsBroadcast() {
		t.Fatalf("relayed DHCPNAK flags = %#04x, want broadcast bit 0x8000 set (RFC 2131 4.3.2)", reply.Flags)
	}
}

func TestRelay_DirectAndRelayedClientsGetSeparatePools(t *testing.T) {
	h := NewHandler(relayConfig())
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")

	// Direct client
	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	offer1 := h.HandlePacket(d1)
	if offer1 == nil {
		t.Fatal("expected offer for direct client")
	}
	if !offer1.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected direct pool IP 10.99.1.100, got %v", offer1.YourIPAddr)
	}

	// Relayed client
	d2 := mustDiscover(t, mac2, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	offer2 := h.HandlePacket(d2)
	if offer2 == nil {
		t.Fatal("expected offer for relayed client")
	}
	if !offer2.YourIPAddr.Equal(net.ParseIP("10.99.2.100")) {
		t.Fatalf("expected relay pool IP 10.99.2.100, got %v", offer2.YourIPAddr)
	}
}

func TestHandleDiscover_IncludesVendorOption(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}

	vendor := reply.Options.Get(dhcpv4.OptionVendorSpecificInformation)
	if vendor == nil {
		t.Fatal("expected vendor-specific option (43) in DHCP response")
	}
	if string(vendor) != VendorMagic {
		t.Fatalf("vendor option = %q, want %q", string(vendor), VendorMagic)
	}
}

func TestRelay_IncludesVendorOption(t *testing.T) {
	h := NewHandler(relayConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}

	vendor := reply.Options.Get(dhcpv4.OptionVendorSpecificInformation)
	if vendor == nil {
		t.Fatal("expected vendor-specific option (43) in relayed DHCP response")
	}
	if string(vendor) != VendorMagic {
		t.Fatalf("vendor option = %q, want %q", string(vendor), VendorMagic)
	}
}

func TestReservation_TakesPrecedenceOverPool(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:01" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.50")) {
		t.Fatalf("expected reserved IP 10.99.1.50, got %v", reply.YourIPAddr)
	}
}

func TestReservation_NonReservedMAC_FallsToPool(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:99" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected an offer, got nil")
	}
	// Non-reserved MAC gets first pool IP
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected pool IP 10.99.1.100, got %v", reply.YourIPAddr)
	}
}

func TestReservation_HasLease_WorksForReservedIP(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:01" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// DORA cycle
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer := h.HandlePacket(discover)
	if offer == nil {
		t.Fatal("expected offer")
	}
	request := mustRequestFromOffer(t, offer)
	ack := h.HandlePacket(request)
	if ack == nil {
		t.Fatal("expected ack")
	}

	if !h.HasLease("10.99.1.50") {
		t.Fatal("expected HasLease to return true for reserved IP")
	}
}

func TestReservation_SameMAC_ConsistentIP(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:01" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	d1 := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer1 := h.HandlePacket(d1)

	d2 := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer2 := h.HandlePacket(d2)

	if !offer1.YourIPAddr.Equal(offer2.YourIPAddr) {
		t.Fatalf("reserved MAC should get same IP on every discover: %v vs %v",
			offer1.YourIPAddr, offer2.YourIPAddr)
	}
}

func TestReservation_WithRelay(t *testing.T) {
	cfg := relayConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:01" {
			return net.ParseIP("10.99.2.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))
	reply := h.HandlePacket(discover)

	if reply == nil {
		t.Fatal("expected offer")
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.2.50")) {
		t.Fatalf("expected reserved IP 10.99.2.50, got %v", reply.YourIPAddr)
	}

	// Pool gateway and mask should still be set from the relay subnet
	routers := reply.Router()
	if len(routers) == 0 || !routers[0].Equal(net.ParseIP("10.99.2.1")) {
		t.Fatalf("expected router 10.99.2.1, got %v", routers)
	}
}

func TestReservation_DoesNotConsumePoolIP(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.100") // Only 1 pool IP
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:aa:bb:01" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)

	// Reserved MAC gets its reservation
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	if reply := h.HandlePacket(d1); !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.50")) {
		t.Fatalf("expected reserved IP, got %v", reply.YourIPAddr)
	}

	// Pool MAC should still get the pool IP (reservation didn't consume it)
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	d2 := mustDiscover(t, mac2, withArch(iana.EFI_X86_64))
	if reply := h.HandlePacket(d2); !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
		t.Fatalf("expected pool IP 10.99.1.100, got %v", reply.YourIPAddr)
	}
}

func TestHandlePacket_IgnoresNonDHCP(t *testing.T) {
	h := NewHandler(testConfig())
	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// Build a Release packet -- handler should return nil
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))
	offer := h.HandlePacket(discover)
	if offer == nil {
		t.Fatal("expected offer")
	}

	release, err := dhcpv4.NewReleaseFromACK(offer)
	if err != nil {
		t.Fatal(err)
	}
	reply := h.HandlePacket(release)
	if reply != nil {
		t.Fatal("expected nil for Release, got a response")
	}
}

// TestHandlePacket_IgnoresNonBootRequestOpcode pins RFC 2131 / RFC 951: a
// server acts only on BOOTP op=BOOTREQUEST (1). A datagram on UDP/67 with
// any other op byte is not a client request, even when its option 53 says
// DISCOVER or REQUEST (a corrupt relay, or another server's malformed
// reply). It must get no reply and must not allocate a lease. Answering it
// also puts a malformed packet on the wire: the library's reply builder
// flips any non-BOOTREQUEST op to BOOTREQUEST, so the OFFER/ACK would leave
// with op=1.
func TestHandlePacket_IgnoresNonBootRequestOpcode(t *testing.T) {
	ops := []dhcpv4.OpcodeType{dhcpv4.OpcodeBootReply, 0, 3, 0xff}
	types := []dhcpv4.MessageType{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeRequest}

	for _, mt := range types {
		// Control: the identical packet with op=BOOTREQUEST is served, so
		// the cases below are refused for the opcode alone.
		t.Run(mt.String()+"/op=BootRequest", func(t *testing.T) {
			h := NewHandler(testConfig())
			pkt, err := dhcpv4.New(
				dhcpv4.WithHwAddr(net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x41}),
				dhcpv4.WithMessageType(mt))
			if err != nil {
				t.Fatal(err)
			}
			if reply := h.HandlePacket(pkt); reply == nil {
				t.Fatalf("control: op=BOOTREQUEST %s got no reply", mt)
			}
		})
		for _, op := range ops {
			t.Run(fmt.Sprintf("%s/op=%d", mt, uint8(op)), func(t *testing.T) {
				h := NewHandler(testConfig())
				pkt, err := dhcpv4.New(
					dhcpv4.WithHwAddr(net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x41}),
					dhcpv4.WithMessageType(mt))
				if err != nil {
					t.Fatal(err)
				}
				pkt.OpCode = op

				if reply := h.HandlePacket(pkt); reply != nil {
					t.Errorf("op=%d %s was answered with %s (op=%s), want no reply",
						uint8(op), mt, reply.MessageType(), reply.OpCode)
				}
				if n := len(h.Leases()); n != 0 {
					t.Errorf("op=%d %s allocated %d lease(s), want 0", uint8(op), mt, n)
				}
			})
		}
	}
}

func TestPoolExhaustion_RateLimitsLogSpam(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.100") // Only 1 IP
	h := NewHandler(cfg)

	// Use a custom logger that captures log calls
	var logBuf logCapture
	h.SetLogger(logBuf.Logger())

	// Allocate the only IP to mac1
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	d1 := mustDiscover(t, mac1, withArch(iana.EFI_X86_64))
	if h.HandlePacket(d1) == nil {
		t.Fatal("expected offer for mac1")
	}

	// Now send multiple discovers from mac2 -- pool is exhausted
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	for range 5 {
		d := mustDiscover(t, mac2, withArch(iana.EFI_X86_64))
		h.HandlePacket(d)
	}

	// Count how many "DHCP pool exhausted" errors were logged for mac2
	count := logBuf.CountMatching("DHCP pool exhausted", "52:54:00:aa:bb:02")
	if count != 1 {
		t.Errorf("expected exactly 1 'pool exhausted' log for mac2, got %d", count)
	}

	// Send discovers from mac3 -- should also log once
	mac3, _ := net.ParseMAC("52:54:00:aa:bb:03")
	for range 3 {
		d := mustDiscover(t, mac3, withArch(iana.EFI_X86_64))
		h.HandlePacket(d)
	}

	count = logBuf.CountMatching("DHCP pool exhausted", "52:54:00:aa:bb:03")
	if count != 1 {
		t.Errorf("expected exactly 1 'pool exhausted' log for mac3, got %d", count)
	}
}

// logCapture is a test helper that captures slog records in memory.
type logCapture struct {
	mu        sync.Mutex
	records   *[]logRecord // shared pointer for all derived handlers
	baseAttrs []slog.Attr
}

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (lc *logCapture) Logger() *slog.Logger {
	if lc.records == nil {
		lc.records = &[]logRecord{}
	}
	return slog.New(lc)
}

func (lc *logCapture) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (lc *logCapture) Handle(_ context.Context, r slog.Record) error {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	attrs := make(map[string]string)

	// Include base attrs from WithAttrs
	for _, a := range lc.baseAttrs {
		attrs[a.Key] = a.Value.String()
	}

	// Include attrs from this record
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})

	*lc.records = append(*lc.records, logRecord{
		level: r.Level,
		msg:   r.Message,
		attrs: attrs,
	})
	return nil
}

func (lc *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{
		records:   lc.records, // share the same slice pointer
		baseAttrs: append(append([]slog.Attr{}, lc.baseAttrs...), attrs...),
	}
}

func (lc *logCapture) WithGroup(name string) slog.Handler {
	return lc
}

// CountMatching counts how many log records contain the given message and attribute value.
func (lc *logCapture) CountMatching(msgSubstr, attrValue string) int {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if lc.records == nil {
		return 0
	}

	count := 0
	for _, r := range *lc.records {
		if !strings.Contains(r.msg, msgSubstr) {
			continue
		}
		for _, v := range r.attrs {
			if strings.Contains(v, attrValue) {
				count++
				break
			}
		}
	}
	return count
}

// TestUpdatePools_RelayPoolAddedAtRuntime pins the hot-reload contract:
// a relayed DISCOVER that matches no pool is dropped, then UpdatePools
// adds the relay pool and the SAME handler serves it — no restart.
func TestUpdatePools_RelayPoolAddedAtRuntime(t *testing.T) {
	h := NewHandler(testConfig()) // no relay pools yet
	mac, _ := net.ParseMAC("52:54:00:aa:bb:31")
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64), withGiaddr(net.ParseIP("10.99.2.1")))

	if reply := h.HandlePacket(discover); reply != nil {
		t.Fatalf("expected drop before pool exists, got offer for %v", reply.YourIPAddr)
	}

	pc := testConfig().PoolConfig
	pc.RelaySubnets = []RelaySubnet{{
		Subnet:     net.IPNet{IP: net.ParseIP("10.99.2.0"), Mask: net.CIDRMask(24, 32)},
		RangeStart: net.ParseIP("10.99.2.100"),
		RangeEnd:   net.ParseIP("10.99.2.200"),
		Gateway:    net.ParseIP("10.99.2.1"),
	}}
	h.UpdatePools(pc)

	reply := h.HandlePacket(discover)
	if reply == nil {
		t.Fatal("expected offer after UpdatePools added the relay pool")
	}
	if reply.YourIPAddr.String() != "10.99.2.100" {
		t.Errorf("offer = %v, want from new relay pool (10.99.2.100)", reply.YourIPAddr)
	}
}

// TestUpdatePools_PromiscuousFlip pins the runtime promiscuous toggle:
// with KnownMACs wired and rejecting everything, a non-promiscuous
// handler drops; flipping promiscuous=true at runtime serves the same
// MAC; flipping back re-arms the filter.
func TestUpdatePools_PromiscuousFlip(t *testing.T) {
	cfg := testConfig()
	cfg.KnownMACs = func(string) bool { return false }
	h := NewHandler(cfg)
	mac, _ := net.ParseMAC("52:54:00:aa:bb:32")
	discover := mustDiscover(t, mac, withArch(iana.EFI_X86_64))

	if h.HandlePacket(discover) != nil {
		t.Fatal("expected drop for unknown MAC in non-promiscuous mode")
	}

	pc := cfg.PoolConfig
	pc.Promiscuous = true
	h.UpdatePools(pc)
	if h.HandlePacket(discover) == nil {
		t.Fatal("expected offer after runtime flip to promiscuous")
	}

	pc.Promiscuous = false
	h.UpdatePools(pc)
	if h.HandlePacket(discover) != nil {
		t.Fatal("expected drop again after flipping promiscuous back off")
	}
}

// TestRequest_OtherServerID pins RFC 2131 §4.3.2: a REQUEST carrying a
// server identifier naming a DIFFERENT server must be silently ignored
// (multi-worker sites relay every request to all workers; only the one
// whose OFFER the client selected may ACK).
func TestRequest_OtherServerID(t *testing.T) {
	h := NewHandler(testConfig()) // ServerIP 10.99.1.1
	mac, _ := net.ParseMAC("52:54:00:aa:bb:33")

	request, err := dhcpv4.NewDiscovery(mac, withArch(iana.EFI_X86_64))
	if err != nil {
		t.Fatal(err)
	}
	request.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
	request.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.99.9.9")))

	if reply := h.HandlePacket(request); reply != nil {
		t.Fatalf("expected silence for REQUEST naming another server, got %v", reply.MessageType())
	}

	// Same REQUEST naming US must be answered.
	request.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.99.1.1")))
	if reply := h.HandlePacket(request); reply == nil {
		t.Fatal("expected ACK for REQUEST naming this server")
	}
}

// TestBootFile_RequiresNetbootClassIdentifier pins the netboot gate in
// bootFileFor: a boot-eligible MAC gets a boot pointer only when it
// identifies as a netboot client (our iPXE user-class, or option 60
// PXEClient / HTTPClient). Anything else, notably the APC PDU NMCs that
// TFTP-looped on undionly.kpxe, gets an address and nothing to boot from.
func TestBootFile_RequiresNetbootClassIdentifier(t *testing.T) {
	cases := []struct {
		name     string
		mods     []dhcpv4.Modifier
		wantBoot string // "" = no boot pointer
	}{
		{
			// Captured pop3 PDU shape: option 60 "APC", empty option 77,
			// no option 93.
			name: "APC PDU",
			mods: []dhcpv4.Modifier{
				withVendorClass("APC"),
				dhcpv4.WithGeneric(dhcpv4.OptionUserClassInformation, []byte{}),
			},
		},
		{
			name:     "PXEClient BIOS",
			mods:     []dhcpv4.Modifier{withVendorClass("PXEClient:Arch:00000:UNDI:002001"), withArch(iana.INTEL_X86PC)},
			wantBoot: "undionly.kpxe",
		},
		{
			name:     "PXEClient UEFI arch 7",
			mods:     []dhcpv4.Modifier{withVendorClass("PXEClient:Arch:00007:UNDI:003016"), withArch(iana.EFI_BC)},
			wantBoot: "snponly.efi",
		},
		{
			name:     "HTTPClient",
			mods:     []dhcpv4.Modifier{withVendorClass("HTTPClient:Arch:00016:UNDI:003000"), withArch(iana.EFI_X86_64)},
			wantBoot: "http://10.99.1.1:8080/files/ipxe/snponly.efi",
		},
		{
			// Our iPXE may not send PXEClient; the user-class alone suffices.
			name:     "slagworks iPXE user-class, no option 60",
			mods:     []dhcpv4.Modifier{withUserClass("slagworks-ipxe"), withArch(iana.EFI_X86_64)},
			wantBoot: "http://10.99.1.1:8080/boot/ipxe",
		},
		{
			name: "no option 60",
			mods: []dhcpv4.Modifier{withArch(iana.INTEL_X86PC)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.BootEligible = func(string) bool { return true }
			h := NewHandler(cfg)
			mac, _ := net.ParseMAC("00:c0:b7:aa:bb:01")

			offer := h.HandlePacket(mustDiscover(t, mac, tc.mods...))
			if offer == nil {
				t.Fatal("expected an offer, got nil")
			}
			ack := h.HandlePacket(mustRequestFromOffer(t, offer, tc.mods...))
			if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
				t.Fatalf("expected an ack, got %v", ack)
			}
			for _, reply := range []*dhcpv4.DHCPv4{offer, ack} {
				kind := reply.MessageType()
				if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.100")) {
					t.Fatalf("%v: expected address 10.99.1.100, got %v", kind, reply.YourIPAddr)
				}
				if reply.BootFileName != tc.wantBoot {
					t.Fatalf("%v: boot file %q, want %q", kind, reply.BootFileName, tc.wantBoot)
				}
				wantSiaddr := net.IPv4zero
				if tc.wantBoot != "" {
					wantSiaddr = net.ParseIP("10.99.1.1")
				}
				if !reply.ServerIPAddr.Equal(wantSiaddr) {
					t.Fatalf("%v: siaddr %v, want %v", kind, reply.ServerIPAddr, wantSiaddr)
				}
				if tc.wantBoot == "" {
					for _, code := range []dhcpv4.OptionCode{dhcpv4.OptionTFTPServerName, dhcpv4.OptionBootfileName, dhcpv4.OptionClassIdentifier} {
						if reply.Options.Has(code) {
							t.Fatalf("%v: non-netboot reply carries option %v", kind, code)
						}
					}
				}
			}
		})
	}
}

// TestOnACK_ReportsClientKind pins the client kind OnACK reports, which the
// site worker maps to a booting op's phase: firmware -> dhcp_ack, slagworks
// iPXE -> ipxe_dhcp, anything else -> no phase. The case that matters is the
// deploy or commission environment's own DHCP client (dhcpcd in the initrd,
// started by ip=dhcp after the boot script is served): it is not PXE
// firmware, and reporting it as firmware rewound every booting op from
// ipxe_loaded to dhcp_ack.
func TestOnACK_ReportsClientKind(t *testing.T) {
	cases := []struct {
		name string
		mods []dhcpv4.Modifier
		want ClientKind
	}{
		{
			name: "PXE firmware BIOS",
			mods: []dhcpv4.Modifier{withVendorClass("PXEClient:Arch:00000:UNDI:002001"), withArch(iana.INTEL_X86PC)},
			want: ClientFirmware,
		},
		{
			name: "PXE firmware UEFI",
			mods: []dhcpv4.Modifier{withVendorClass("PXEClient:Arch:00007:UNDI:003016"), withArch(iana.EFI_BC)},
			want: ClientFirmware,
		},
		{
			name: "UEFI HTTP-boot firmware",
			mods: []dhcpv4.Modifier{withVendorClass("HTTPClient:Arch:00016:UNDI:003000"), withArch(iana.EFI_X86_64)},
			want: ClientFirmware,
		},
		{
			// A NIC's own iPXE ROM: still the firmware stage, it chainloads
			// slagworks iPXE next.
			name: "generic iPXE ROM",
			mods: []dhcpv4.Modifier{withPXEClient(), withUserClass("iPXE"), withArch(iana.INTEL_X86PC)},
			want: ClientFirmware,
		},
		{
			name: "slagworks iPXE",
			mods: []dhcpv4.Modifier{withPXEClient(), withUserClass("slagworks-ipxe"), withArch(iana.EFI_X86_64)},
			want: ClientIPXE,
		},
		{
			name: "slagworks iPXE, no option 60",
			mods: []dhcpv4.Modifier{withUserClass("slagworks-ipxe")},
			want: ClientIPXE,
		},
		{
			// dhcpcd's default option 60.
			name: "deploy environment dhcpcd",
			mods: []dhcpv4.Modifier{withVendorClass("dhcpcd-10.0.6:Linux-6.8.0-48-generic:x86_64:GenuineIntel")},
			want: ClientOther,
		},
		{
			name: "no option 60",
			want: ClientOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acks := make(chan ClientKind, 1)
			cfg := testConfig()
			cfg.OnACK = func(_ string, client ClientKind) { acks <- client }
			h := NewHandler(cfg)
			mac, _ := net.ParseMAC("52:54:00:aa:bb:01")

			offer := h.HandlePacket(mustDiscover(t, mac, tc.mods...))
			if offer == nil {
				t.Fatal("expected an offer, got nil")
			}
			ack := h.HandlePacket(mustRequestFromOffer(t, offer, tc.mods...))
			if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
				t.Fatalf("expected an ack, got %v", ack)
			}
			select {
			case got := <-acks:
				if got != tc.want {
					t.Fatalf("OnACK client kind %d, want %d", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("OnACK never fired")
			}
		})
	}
}
