package dhcp

import (
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/arp"
)

// NewARPProber returns an ARPProbe function that sends an ARP request on the
// given interface and waits up to timeout for a reply. Returns true if the
// IP is in use on the network.
func NewARPProber(iface *net.Interface, timeout time.Duration, log *slog.Logger) func(ip net.IP) bool {
	return func(ip net.IP) bool {
		client, err := arp.Dial(iface)
		if err != nil {
			log.Warn("ARP probe: failed to open socket", "iface", iface.Name, "err", err)
			return false
		}
		defer func() { _ = client.Close() }()

		if err := client.SetDeadline(time.Now().Add(timeout)); err != nil {
			log.Warn("ARP probe: failed to set deadline", "err", err)
			return false
		}

		addr, ok := netip.AddrFromSlice(ip.To4())
		if !ok {
			log.Warn("ARP probe: invalid IP", "ip", ip.String())
			return false
		}

		if err := client.Request(addr); err != nil {
			log.Debug("ARP probe: send failed", "ip", ip.String(), "err", err)
			return false
		}

		for {
			pkt, _, err := client.Read()
			if err != nil {
				// Timeout or read error = no reply = IP not in use
				return false
			}
			if pkt.Operation != arp.OperationReply {
				continue
			}
			if pkt.SenderIP != addr {
				continue // reply from a different IP, keep waiting
			}
			log.Warn("ARP probe: IP already in use on network",
				"ip", ip.String(),
				"owner_mac", pkt.SenderHardwareAddr.String(),
				"iface", iface.Name)
			return true
		}
	}
}
