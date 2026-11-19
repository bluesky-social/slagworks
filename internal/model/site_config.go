package model

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// RelayPool is one DHCP pool served to clients behind a relay agent
// (dhcp-helper on a leaf bridge). giaddr-in-Subnet selects the pool;
// Gateway is the router handed to clients (the leaf-pair VRRP VIP,
// NOT the relay's own bridge address).
type RelayPool struct {
	Subnet     string `json:"subnet"`      // CIDR, e.g. 10.254.27.0/24
	RangeStart string `json:"range_start"` // first IP in pool (inclusive)
	RangeEnd   string `json:"range_end"`   // last IP in pool (inclusive)
	Gateway    string `json:"gateway"`     // router option for clients
}

// SiteConfig holds slagworks-only per-site knobs. The Slug is the
// free-form worker identity (PK); it does NOT have to be a NetBox site
// slug. NetBoxSite (required) is the NetBox site this worker mirrors,
// and NetBoxLocation (optional) narrows the scope to one location
// within that site — letting two workers (e.g. pop2-old-cage,
// pop2-new-cage) serve disjoint cages of a single NetBox site. Empty
// NetBoxLocation = whole-site scope, the pop1/pop3 shape. Site identity
// (name, subnet_cidr, gateway) comes from NetBox at snapshot-build time
// keyed on NetBoxSite.
type SiteConfig struct {
	Slug               string      `json:"slug" gorm:"column:slug;primaryKey"`
	NetBoxSite         string      `json:"netbox_site" gorm:"column:netbox_site;not null"`
	NetBoxLocation     string      `json:"netbox_location" gorm:"column:netbox_location;not null;default:''"`
	EphemeralPoolStart string      `json:"ephemeral_pool_start" gorm:"column:ephemeral_pool_start;not null"`
	EphemeralPoolEnd   string      `json:"ephemeral_pool_end" gorm:"column:ephemeral_pool_end;not null"`
	RelayPools         []RelayPool `json:"relay_pools" gorm:"column:relay_pools;type:jsonb;serializer:json"`
	Promiscuous        bool        `json:"promiscuous" gorm:"column:promiscuous;not null;default:false"`
	LastSnapshotSeq    int64       `json:"last_snapshot_seq" gorm:"column:last_snapshot_seq;not null;default:0"`
	LastSnapshotHash   string      `json:"last_snapshot_hash" gorm:"column:last_snapshot_hash;not null;default:''"`
	CreatedAt          time.Time   `json:"created_at" gorm:"autoCreateTime"`
}

func (SiteConfig) TableName() string { return "site_configs" }

// LocationScoped reports whether this config narrows to a single NetBox
// location (vs whole-site).
func (c SiteConfig) LocationScoped() bool { return c.NetBoxLocation != "" }

func (c SiteConfig) Validate() error {
	if c.Slug == "" {
		return errors.New("slug required")
	}
	if c.NetBoxSite == "" {
		return errors.New("netbox_site required")
	}
	// IPv4 only: OOB networks are v4 everywhere in the fleet. Rejecting
	// v6 loudly beats accepting it and failing at DHCP-lease time.
	start := net.ParseIP(c.EphemeralPoolStart).To4()
	end := net.ParseIP(c.EphemeralPoolEnd).To4()
	if start == nil || end == nil {
		return errors.New("ephemeral pool start/end must be valid IPv4 addresses")
	}
	if ipToUint32(start) > ipToUint32(end) {
		return errors.New("ephemeral pool start must be <= end")
	}
	for i, p := range c.RelayPools {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("relay pool %d: %w", i, err)
		}
	}
	return nil
}

func (p RelayPool) Validate() error {
	_, subnet, err := net.ParseCIDR(p.Subnet)
	if err != nil {
		return fmt.Errorf("subnet %q: %w", p.Subnet, err)
	}
	// ParseCIDR returns a 4-byte mask only for dotted-quad input. Checking
	// subnet.IP.To4() is not enough: IPv4-mapped IPv6 notation
	// (::ffff:10.254.27.0/120) converts to IPv4 but carries a 16-byte
	// mask, which the DHCPv4 subnet-mask option truncates to
	// 255.255.255.255.
	if len(subnet.Mask) != net.IPv4len {
		return fmt.Errorf("subnet %q must be an IPv4 CIDR in dotted-quad form, such as 10.254.27.0/24", p.Subnet)
	}
	start := net.ParseIP(p.RangeStart).To4()
	end := net.ParseIP(p.RangeEnd).To4()
	gw := net.ParseIP(p.Gateway).To4()
	if start == nil || end == nil {
		return errors.New("range start/end must be valid IPv4 addresses")
	}
	if gw == nil {
		return errors.New("gateway must be a valid IPv4 address")
	}
	if !subnet.Contains(start) || !subnet.Contains(end) {
		return fmt.Errorf("range %s-%s not contained in subnet %s", p.RangeStart, p.RangeEnd, p.Subnet)
	}
	if !subnet.Contains(gw) {
		return fmt.Errorf("gateway %s not contained in subnet %s", p.Gateway, p.Subnet)
	}
	s, e, g := ipToUint32(start), ipToUint32(end), ipToUint32(gw)
	if s > e {
		return errors.New("range start must be <= end")
	}
	// The DHCP handler leases the range as given, upward from RangeStart,
	// and its ARP probe runs on the worker's own segment, so it cannot see
	// the gateway or anything else on a relayed subnet. Every address in
	// the range must therefore be one a client can own.
	if s <= g && g <= e {
		return fmt.Errorf("gateway %s is inside range %s-%s, so DHCP would offer the router address to a client; move the range or the gateway so they do not overlap",
			p.Gateway, p.RangeStart, p.RangeEnd)
	}
	// A subnet wider than /31 has a network and a broadcast address, and
	// neither is a host address; a /31 (RFC 3021) or /32 has neither.
	// The mask is four bytes (checked above), so its ones count is the
	// IPv4 prefix length.
	if prefixLen, _ := subnet.Mask.Size(); prefixLen <= 30 {
		network := ipToUint32(subnet.IP.To4())
		broadcast := network | ^uint32(0)>>prefixLen
		if s == network {
			return fmt.Errorf("range %s-%s includes the network address of %s, which no host can use; start the range after it",
				p.RangeStart, p.RangeEnd, p.Subnet)
		}
		if e == broadcast {
			return fmt.Errorf("range %s-%s includes the broadcast address of %s, which no host can use; end the range before it",
				p.RangeStart, p.RangeEnd, p.Subnet)
		}
	}
	return nil
}

// ipToUint32 converts a 4-byte IPv4 slice to its big-endian uint32.
// Caller must have ensured ip is a valid IPv4 (via ip.To4() != nil).
func ipToUint32(ip net.IP) uint32 {
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
