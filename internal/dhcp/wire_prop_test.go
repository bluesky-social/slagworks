package dhcp

// Malformed / boundary packet coverage. server4 parses each datagram with
// dhcpv4.FromBytes on its read loop and runs the handler on a bare
// goroutine with no recover, so a panic anywhere on this path takes the
// whole site worker down for anyone who can put a frame on the segment.
//
// Two drivers share one checker:
//   - TestProp_WireMutations: rapid builds a valid client packet, then
//     damages it the ways real and hostile clients do (truncation, lying
//     hlen, zero-length and maximum-length options, option overload,
//     duplicated options, giant RFC 3396 concatenations);
//   - FuzzHandlerWire: Go native fuzzing over raw bytes, seeded with one
//     valid packet per client class.

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"pgregory.net/rapid"
)

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var wireIfaces = []string{"", propOOBIface, "eth9"}

// wireHandler is a handler with every feature switched on and a few leases
// already in place, so a damaged packet can reach renew, adopt, NAK, relay
// and reservation paths.
func wireHandler() *Handler {
	h := NewHandler(Config{
		ServerIP:      propBaseServer.ip(),
		DNS:           netIPs(propDNSBase),
		LeaseDuration: 30 * time.Minute,
		HTTPBootPort:  propHTTPPort,
		PoolConfig: PoolConfig{
			Subnet:     net.IPNet{IP: a4(10, 1, 0, 0).ip(), Mask: net.CIDRMask(24, 32)},
			RangeStart: a4(10, 1, 0, 10).ip(),
			RangeEnd:   a4(10, 1, 0, 13).ip(),
			Gateway:    a4(10, 1, 0, 1).ip(),
			RelaySubnets: []RelaySubnet{
				{Subnet: net.IPNet{IP: a4(10, 2, 0, 0).ip(), Mask: net.CIDRMask(24, 32)}, RangeStart: a4(10, 2, 0, 10).ip(), RangeEnd: a4(10, 2, 0, 11).ip(), Gateway: a4(10, 2, 0, 1).ip()},
				{Subnet: net.IPNet{IP: a4(10, 3, 0, 0).ip(), Mask: net.CIDRMask(31, 32)}, RangeStart: a4(10, 3, 0, 1).ip(), RangeEnd: a4(10, 3, 0, 1).ip(), Gateway: a4(10, 3, 0, 0).ip()},
			},
		},
		ReserveIP: func(mac string) net.IP {
			switch mac {
			case propMAC(1).String():
				return a4(10, 1, 0, 5).ip()
			case propMAC(2).String():
				return a4(10, 9, 0, 5).ip()
			}
			return nil
		},
		BootEligible: func(mac string) bool { return mac != propMAC(2).String() },
		ARPProbe:     func(ip net.IP) bool { return toPIP(ip) == a4(10, 1, 0, 11) },
		OnACK:        func(string, bool) {},
		OnLeaseEvent: func(LeaseEvent) {},
		LeaseStore:   &propStore{rows: map[string]PersistedLease{}},
	})
	h.SetLogger(slogDiscard())
	h.SetIfaceParams(propOOBIface, IfaceParams{
		ServerIP: propOOBServer.ip(),
		NTP:      netIPs(propNTPOOB),
		PoolConfig: PoolConfig{
			Subnet:      net.IPNet{IP: a4(10, 9, 0, 0).ip(), Mask: net.CIDRMask(24, 32)},
			RangeStart:  a4(10, 9, 0, 10).ip(),
			RangeEnd:    a4(10, 9, 0, 12).ip(),
			Gateway:     a4(10, 9, 0, 1).ip(),
			Promiscuous: true,
		},
		DynamicSlices: []DynamicSlice{{Start: a4(10, 9, 0, 10).ip(), End: a4(10, 9, 0, 12).ip()}, {Start: a4(10, 9, 0, 20).ip(), End: a4(10, 9, 0, 22).ip()}},
	})
	for i := 0; i < 2; i++ {
		req, _ := dhcpv4.New(dhcpv4.WithHwAddr(propMAC(i)), dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover))
		h.HandlePacket(req)
		h.HandlePacketOn(req, propOOBIface)
	}
	return h
}

// wireGrantable is every address wireHandler may legitimately hand out.
func wireGrantable(ip pIP) bool {
	switch {
	case ip >= a4(10, 1, 0, 10) && ip <= a4(10, 1, 0, 13):
	case ip >= a4(10, 2, 0, 10) && ip <= a4(10, 2, 0, 11):
	case ip == a4(10, 3, 0, 0) || ip == a4(10, 3, 0, 1):
	case ip >= a4(10, 9, 0, 10) && ip <= a4(10, 9, 0, 12):
	case ip == a4(10, 1, 0, 5) || ip == a4(10, 9, 0, 5):
	default:
		return false
	}
	return true
}

// wireCheck feeds one datagram through parse -> handle -> encode, twice (the
// second pass meets the state the first left behind), and reports what it
// reached. It fails on a panic (by not recovering) or a reply that is not a
// well-formed answer to the request.
func wireCheck(t interface{ Fatalf(string, ...any) }, data []byte, sel uint8) (parsed, replied bool) {
	req, err := dhcpv4.FromBytes(data)
	if err != nil {
		return false, false
	}
	h := wireHandler()
	iface := wireIfaces[int(sel)%len(wireIfaces)]
	for pass := 0; pass < 2; pass++ {
		reply := h.HandlePacketOn(req, iface)
		if reply == nil {
			continue
		}
		replied = true
		out := reply.ToBytes()
		rt, err := dhcpv4.FromBytes(out)
		if err != nil {
			t.Fatalf("reply does not parse: %v\nrequest: %x", err, data)
		}
		mt := reply.MessageType()
		if mt != dhcpv4.MessageTypeOffer && mt != dhcpv4.MessageTypeAck && mt != dhcpv4.MessageTypeNak {
			t.Fatalf("reply type %s\nrequest: %x", mt, data)
		}
		if req.OpCode != dhcpv4.OpcodeBootRequest {
			// A server acts only on op = BOOTREQUEST (RFC 951 / RFC 2131).
			t.Fatalf("answered a packet whose BOOTP opcode is %d, with opcode %d\nrequest: %x", req.OpCode, reply.OpCode, data)
		}
		if reply.OpCode != dhcpv4.OpcodeBootReply {
			t.Fatalf("reply opcode %d\nrequest: %x", reply.OpCode, data)
		}
		if reply.TransactionID != req.TransactionID || !bytes.Equal(reply.ClientHWAddr, req.ClientHWAddr) {
			t.Fatalf("reply does not answer the request\nrequest: %x", data)
		}
		if !bytes.Equal(rt.ClientHWAddr, req.ClientHWAddr) || rt.MessageType() != mt || !rt.YourIPAddr.Equal(reply.YourIPAddr) {
			t.Fatalf("reply changes across the wire\nrequest: %x", data)
		}
		yi := toPIP(reply.YourIPAddr)
		if mt == dhcpv4.MessageTypeNak {
			if yi != 0 || reply.BootFileName != "" {
				t.Fatalf("NAK carries an address or boot file\nrequest: %x", data)
			}
		} else if !wireGrantable(yi) {
			t.Fatalf("granted %s, which no pool or reservation contains\nrequest: %x", yi, data)
		}
		if len(reply.BootFileName) > 127 || bytes.ContainsAny([]byte(reply.BootFileName), "\x00\r\n") {
			t.Fatalf("boot file %q is not a clean header string\nrequest: %x", reply.BootFileName, data)
		}
	}
	seen := map[string]string{}
	for _, li := range h.Leases() {
		if other, dup := seen[li.IP]; dup {
			t.Fatalf("%s leased to %q and %q\nrequest: %x", li.IP, other, li.MAC, data)
		}
		seen[li.IP] = li.MAC
	}
	h.LeaseCountsByPool()
	return true, replied
}

var wireOptionCodes = []byte{0, 1, 12, 43, 50, 51, 52, 53, 54, 55, 57, 60, 61, 66, 67, 77, 82, 93, 94, 97, 119, 175, 255}

func genWirePacket(t *rapid.T) []byte {
	pk := propPacket{
		typ:     rapid.SampledFrom([]dhcpv4.MessageType{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeDecline, dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeInform}).Draw(t, "type"),
		mac:     rapid.IntRange(0, 3).Draw(t, "mac"),
		arrival: propArrival{giaddr: rapid.SampledFrom([]pIP{0, 0, a4(10, 1, 0, 3), a4(10, 2, 0, 1), a4(10, 3, 0, 0), a4(10, 3, 0, 1), a4(10, 200, 0, 1)}).Draw(t, "giaddr")},
		class:   rapid.SampledFrom(propClasses).Draw(t, "class"),
		extra:   rapid.IntRange(0, len(propExtras)-1).Draw(t, "extra"),
	}
	if pk.typ == dhcpv4.MessageTypeRequest {
		ip := rapid.SampledFrom(propWantIPs).Draw(t, "want")
		switch rapid.IntRange(0, 2).Draw(t, "state") {
		case 0:
			pk.sid, pk.opt50 = rapid.SampledFrom([]pIP{propBaseServer, propOOBServer}).Draw(t, "sid"), ip
		case 1:
			pk.opt50 = ip
		default:
			pk.ciaddr = ip
		}
	}
	data := pk.build(t).ToBytes()

	for i, n := 0, rapid.IntRange(0, 3).Draw(t, "mutations"); i < n; i++ {
		switch rapid.IntRange(0, 7).Draw(t, "mutation") {
		case 0: // truncate anywhere
			data = data[:rapid.IntRange(0, len(data)).Draw(t, "truncateAt")]
		case 1: // hardware address length lies
			if len(data) > 2 {
				data[2] = byte(rapid.SampledFrom([]int{0, 1, 5, 6, 7, 16, 17, 255}).Draw(t, "hlen"))
			}
		case 2: // header bytes
			if len(data) > 12 {
				off := rapid.IntRange(0, 11).Draw(t, "headerOff")
				data[off] = rapid.Byte().Draw(t, "headerByte")
			}
		case 3: // flip bytes in the options area
			if len(data) > 241 {
				for j, m := 0, rapid.IntRange(1, 6).Draw(t, "flips"); j < m; j++ {
					data[rapid.IntRange(240, len(data)-1).Draw(t, "flipOff")] = rapid.Byte().Draw(t, "flipByte")
				}
			}
		case 4, 5: // replace the options with generated TLVs
			if len(data) >= 240 {
				opts := []byte{53, 1, byte(pk.typ)}
				for j, m := 0, rapid.IntRange(0, 12).Draw(t, "tlvs"); j < m; j++ {
					code := rapid.SampledFrom(wireOptionCodes).Draw(t, "code")
					if rapid.IntRange(0, 9).Draw(t, "randomCode") == 0 {
						code = rapid.Byte().Draw(t, "anyCode")
					}
					if code == 0 || code == 255 {
						opts = append(opts, code)
						continue
					}
					claim := rapid.SampledFrom([]int{0, 0, 1, 2, 3, 4, 5, 16, 64, 255}).Draw(t, "claimLen")
					have := claim
					if rapid.IntRange(0, 7).Draw(t, "shortBody") == 0 {
						have = rapid.IntRange(0, claim).Draw(t, "haveLen")
					}
					body := rapid.SliceOfN(rapid.Byte(), have, have).Draw(t, "body")
					opts = append(opts, code, byte(claim))
					opts = append(opts, body...)
				}
				if rapid.Bool().Draw(t, "endMarker") {
					opts = append(opts, 255)
				}
				data = append(append([]byte(nil), data[:240]...), opts...)
			}
		case 6: // giant concatenated option (RFC 3396)
			code := rapid.SampledFrom([]byte{12, 60, 61, 77, 82, 93, 50, 54}).Draw(t, "giantCode")
			chunk := bytes.Repeat([]byte{code, 255}, 1)
			var giant []byte
			for j, m := 0, rapid.IntRange(1, 14).Draw(t, "giantChunks"); j < m; j++ {
				giant = append(giant, chunk...)
				giant = append(giant, bytes.Repeat([]byte{byte('a' + j)}, 255)...)
			}
			if len(data) >= 240 {
				end := bytes.LastIndexByte(data, 255)
				if end < 240 {
					end = len(data)
				}
				data = append(append(append([]byte(nil), data[:end]...), giant...), 255)
			}
		case 7: // option overload pointing into sname/file
			if len(data) >= 240 {
				copy(data[44:], bytes.Repeat([]byte{60, 9, 'P', 'X', 'E', 'C', 'l', 'i', 'e', 'n', 't'}, 5))
				data = append(append([]byte(nil), data[:240]...), 53, 1, byte(pk.typ), 52, 1, byte(rapid.IntRange(0, 4).Draw(t, "overload")), 255)
			}
		}
	}
	return data
}

func TestProp_WireMutations(t *testing.T) {
	propQuietLogs(t)
	var total, parsed, replied int
	rapid.Check(t, func(rt *rapid.T) {
		data := genWirePacket(rt)
		p, r := wireCheck(rt, data, rapid.Byte().Draw(rt, "iface"))
		total++
		if p {
			parsed++
		}
		if r {
			replied++
		}
	})
	t.Logf("coverage: %d datagrams, %d parsed, %d answered", total, parsed, replied)
}

func FuzzHandlerWire(f *testing.F) {
	prev := slog.Default()
	slog.SetDefault(slogDiscard())
	f.Cleanup(func() { slog.SetDefault(prev) })
	for _, c := range propClasses {
		for _, typ := range []dhcpv4.MessageType{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeRequest} {
			mods := []dhcpv4.Modifier{dhcpv4.WithHwAddr(propMAC(0)), dhcpv4.WithMessageType(typ)}
			if c.opt60 != "" {
				mods = append(mods, dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte(c.opt60)))
			}
			if c.userClass != "" {
				mods = append(mods, dhcpv4.WithUserClass(c.userClass, false))
			}
			if typ == dhcpv4.MessageTypeRequest {
				mods = append(mods, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(a4(10, 1, 0, 10).ip())),
					dhcpv4.WithOption(dhcpv4.OptServerIdentifier(propBaseServer.ip())))
			}
			pkt, err := dhcpv4.New(mods...)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(pkt.ToBytes(), uint8(0))
			pkt.GatewayIPAddr = a4(10, 3, 0, 0).ip()
			f.Add(pkt.ToBytes(), uint8(1))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, sel uint8) {
		wireCheck(t, data, sel)
	})
}
