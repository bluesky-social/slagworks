package dhcp

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// A static reservation is honored only when it lies inside the subnet of the
// pool serving the request. ReserveIP is MAC-keyed and the worker cache
// prefers a machine's NetBox interface IP, so a NIC addressed in a different
// subnet that shares the segment's L2 (pop3: a 1G eno1 in a copper /22
// PXE-booting onto the OOB /22) would otherwise be offered its foreign
// address with this segment's mask and router — an unusable lease.

const reservedMAC = "52:54:00:00:0e:01"

func reserveFor(ip string) func(string) net.IP {
	return func(mac string) net.IP {
		if mac == reservedMAC {
			return net.ParseIP(ip)
		}
		return nil
	}
}

// reservationsOnlyParams is splitPoolParams with no dynamic range: the
// gateway serves reservations only.
func reservationsOnlyParams() IfaceParams {
	p := splitPoolParams()
	p.DynamicSlices = nil
	p.PoolConfig.RangeStart = nil
	p.PoolConfig.RangeEnd = nil
	return p
}

// p2pRelayConfig serves a /31 fabric leg by relay: giaddr 10.253.28.52 is
// the leaf, 10.253.28.53 the client endpoint (see relayPoolForRequest).
func p2pRelayConfig() Config {
	cfg := testConfig()
	cfg.RelaySubnets = []RelaySubnet{{
		Subnet:     net.IPNet{IP: net.ParseIP("10.253.28.52"), Mask: net.CIDRMask(31, 32)},
		RangeStart: net.ParseIP("10.253.28.52"),
		RangeEnd:   net.ParseIP("10.253.28.52"),
		Gateway:    net.ParseIP("10.253.28.53"),
	}}
	return cfg
}

func TestReservation_SubnetCheck_Discover(t *testing.T) {
	p2pGiaddr := net.ParseIP("10.253.28.52")
	tests := []struct {
		name     string
		cfg      Config
		params   *IfaceParams // registered on eno3 when non-nil
		reserved string
		giaddr   net.IP
		wantIP   string // exact offer; "" with wantNil=false means "any address in [dynStart,dynEnd]"
		dynStart string
		dynEnd   string
		wantNil  bool
		wantGW   string
	}{
		{
			name:     "in-subnet reservation offered",
			reserved: "10.254.240.50",
			wantIP:   "10.254.240.50",
			wantGW:   "10.254.240.1",
		},
		{
			name:     "out-of-subnet reservation falls through to dynamic slice",
			reserved: "10.254.244.41",
			dynStart: "10.254.241.100",
			dynEnd:   "10.254.241.149",
			wantGW:   "10.254.240.1",
		},
		{
			name:     "out-of-subnet reservation on reservations-only pool gets no offer",
			params:   ptr(reservationsOnlyParams()),
			reserved: "10.254.244.41",
			wantNil:  true,
		},
		{
			name:     "in-subnet reservation on reservations-only pool offered",
			params:   ptr(reservationsOnlyParams()),
			reserved: "10.254.240.50",
			wantIP:   "10.254.240.50",
			wantGW:   "10.254.240.1",
		},
		{
			name:     "relay /31 client-endpoint reservation honored",
			cfg:      p2pRelayConfig(),
			reserved: "10.253.28.53",
			giaddr:   p2pGiaddr,
			wantIP:   "10.253.28.53",
			wantGW:   "10.253.28.52",
		},
		{
			name:     "relay /31 foreign reservation falls through to the client endpoint",
			cfg:      p2pRelayConfig(),
			reserved: "10.254.240.50",
			giaddr:   p2pGiaddr,
			wantIP:   "10.253.28.53",
			wantGW:   "10.253.28.52",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if cfg.ServerIP == nil {
				cfg = testConfig()
			}
			cfg.ReserveIP = reserveFor(tt.reserved)
			h := NewHandler(cfg)
			iface := ""
			if tt.giaddr == nil {
				params := splitPoolParams()
				if tt.params != nil {
					params = *tt.params
				}
				iface = "eno3"
				h.SetIfaceParams(iface, params)
			}
			mac, _ := net.ParseMAC(reservedMAC)
			mods := []dhcpv4.Modifier{withArch(iana.EFI_X86_64)}
			if tt.giaddr != nil {
				mods = append(mods, withGiaddr(tt.giaddr))
			}

			reply := h.HandlePacketOn(mustDiscover(t, mac, mods...), iface)
			if tt.wantNil {
				if reply != nil {
					t.Fatalf("expected no offer, got %v", reply.YourIPAddr)
				}
				return
			}
			if reply == nil {
				t.Fatal("expected offer, got nil")
			}
			got := reply.YourIPAddr
			if tt.wantIP != "" {
				if !got.Equal(net.ParseIP(tt.wantIP)) {
					t.Fatalf("offered %v, want %s", got, tt.wantIP)
				}
			} else if !ipInRange(got, net.ParseIP(tt.dynStart), net.ParseIP(tt.dynEnd)) {
				t.Fatalf("offered %v, want a dynamic address in %s-%s", got, tt.dynStart, tt.dynEnd)
			}
			if routers := reply.Router(); len(routers) != 1 || !routers[0].Equal(net.ParseIP(tt.wantGW)) {
				t.Fatalf("routers = %v, want %s", routers, tt.wantGW)
			}
		})
	}
}

func TestReservation_SubnetCheck_Request(t *testing.T) {
	tests := []struct {
		name     string
		reserved string
		reqIP    string // INIT-REBOOT requested address
		wantACK  bool
		wantIP   string
	}{
		{
			name:     "in-subnet reservation requested is ACKed",
			reserved: "10.254.240.50",
			reqIP:    "10.254.240.50",
			wantACK:  true,
			wantIP:   "10.254.240.50",
		},
		{
			name:     "in-subnet reservation still NAKs a different address",
			reserved: "10.254.240.50",
			reqIP:    "10.254.241.110",
			wantACK:  false,
		},
		{
			name:     "out-of-subnet reservation does not NAK a dynamic request",
			reserved: "10.254.244.41",
			reqIP:    "10.254.241.110",
			wantACK:  true,
			wantIP:   "10.254.241.110",
		},
		{
			name:     "out-of-subnet reservation requested is not ACKed",
			reserved: "10.254.244.41",
			reqIP:    "10.254.244.41",
			wantACK:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ReserveIP = reserveFor(tt.reserved)
			h := NewHandler(cfg)
			h.SetIfaceParams("eno3", splitPoolParams())
			mac, _ := net.ParseMAC(reservedMAC)

			reply := h.HandlePacketOn(initReboot(t, mac, tt.reqIP), "eno3")
			if !tt.wantACK {
				if !isNAK(reply) {
					t.Fatalf("expected NAK, got %v", reply)
				}
				return
			}
			if !isACK(reply) {
				t.Fatalf("expected ACK, got %v", reply)
			}
			if !reply.YourIPAddr.Equal(net.ParseIP(tt.wantIP)) {
				t.Fatalf("ACKed %v, want %s", reply.YourIPAddr, tt.wantIP)
			}
		})
	}
}

// TestReservation_SubnetCheck_DORA runs the full exchange so the REQUEST
// path's allocateIP (not just classifyRequest) sees the filtered reservation.
func TestReservation_SubnetCheck_DORA(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		reserved string
		giaddr   net.IP
		wantIP   string // "" = any address in my dynamic slice
	}{
		{name: "out-of-subnet reservation", cfg: testConfig(), reserved: "10.254.244.41"},
		{name: "in-subnet reservation", cfg: testConfig(), reserved: "10.254.240.50", wantIP: "10.254.240.50"},
		{
			name:     "relay /31 reservation",
			cfg:      p2pRelayConfig(),
			reserved: "10.253.28.53",
			giaddr:   net.ParseIP("10.253.28.52"),
			wantIP:   "10.253.28.53",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ReserveIP = reserveFor(tt.reserved)
			h := NewHandler(cfg)
			iface := ""
			if tt.giaddr == nil {
				iface = "eno3"
				h.SetIfaceParams(iface, splitPoolParams())
			}
			mac, _ := net.ParseMAC(reservedMAC)
			mods := []dhcpv4.Modifier{withArch(iana.EFI_X86_64)}
			if tt.giaddr != nil {
				mods = append(mods, withGiaddr(tt.giaddr))
			}

			offer := h.HandlePacketOn(mustDiscover(t, mac, mods...), iface)
			if offer == nil {
				t.Fatal("expected offer")
			}
			request := mustRequestFromOffer(t, offer)
			if tt.giaddr != nil {
				request.GatewayIPAddr = tt.giaddr
			}
			ack := h.HandlePacketOn(request, iface)
			if !isACK(ack) {
				t.Fatalf("expected ACK, got %v", ack)
			}
			if !ack.YourIPAddr.Equal(offer.YourIPAddr) {
				t.Fatalf("ACKed %v, offered %v", ack.YourIPAddr, offer.YourIPAddr)
			}
			if tt.wantIP != "" {
				if !ack.YourIPAddr.Equal(net.ParseIP(tt.wantIP)) {
					t.Fatalf("ACKed %v, want %s", ack.YourIPAddr, tt.wantIP)
				}
			} else if !ipInRange(ack.YourIPAddr, net.ParseIP("10.254.241.100"), net.ParseIP("10.254.241.149")) {
				t.Fatalf("ACKed %v, want a dynamic address in my slice", ack.YourIPAddr)
			}
		})
	}
}

func TestPoolContains(t *testing.T) {
	oob := &pool{gateway: net.ParseIP("10.252.240.1"), mask: net.CIDRMask(22, 32)}
	tests := []struct {
		name string
		p    *pool
		ip   string
		want bool
	}{
		{"inside /22", oob, "10.252.243.254", true},
		{"adjacent /22", oob, "10.252.244.41", false},
		{"/31 client endpoint", &pool{gateway: net.ParseIP("10.253.28.52"), mask: net.CIDRMask(31, 32)}, "10.253.28.53", true},
		{"outside /31", &pool{gateway: net.ParseIP("10.253.28.52"), mask: net.CIDRMask(31, 32)}, "10.253.28.54", false},
		{"nil pool honors", nil, "10.252.244.41", true},
		{"no gateway honors", &pool{mask: net.CIDRMask(22, 32)}, "10.252.244.41", true},
		{"no mask honors", &pool{gateway: net.ParseIP("10.252.240.1")}, "10.252.244.41", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.contains(net.ParseIP(tt.ip)); got != tt.want {
				t.Fatalf("contains(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
