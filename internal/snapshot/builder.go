// Package snapshot builds per-site SiteSnapshots on demand from live
// NetBox data joined with live Postgres state. See
// docs/dev/architecture.md ("NetBox as source of truth").
//
// The package has three layers:
//   - Build (this file): pure function over Inputs → SiteSnapshot. No
//     I/O, no goroutines. Easy to test.
//   - Dispatcher: drives Build by pulling from NetBox + Store, hashes
//     the output, pushes to a sink on content change.
//   - Invalidator + Reconciler: fire rebuilds on webhook/tick/Postgres
//     events with per-site dirty-flag/debounce/dirtyGen retry.
package snapshot

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// Inputs is the complete data set needed to build one SiteSnapshot.
// Build is deterministic on Inputs — a unit test can construct Inputs
// directly without any mocks.
type Inputs struct {
	Site           netbox.Site
	Config         model.SiteConfig
	Prefixes       []netbox.Prefix  // tag=slagworks-production; a location-scoped config's own location's first (see dhcpPrefix)
	OOBPrefixes    []netbox.Prefix  // tag=slagworks-oob; empty pre-cutover
	OOBRanges      []netbox.IPRange // role=slagworks-oob-dynamic; empty pre-cutover
	Devices        []netbox.Device
	DeviceTypes    []netbox.DeviceType // for slagworks_kernel_params lookup
	Interfaces     []netbox.Interface
	IPs            []netbox.IPAddress
	Machines       []model.Machine
	Images         []model.Image
	Scripts        []model.StorageScript
	NetworkScripts []model.NetworkScript
	SSHKeys        []string

	// ConsolePorts are the console ports of the scoped devices, each with
	// its cable-path far end. Build resolves a machine's cabled
	// console-server endpoint (MachineSnapshot.Console) from these.
	ConsolePorts []netbox.ConsolePort
	// ConsoleServers are the devices the console ports' far ends sit on
	// (the oob-gws), looked up site-wide rather than from the scoped
	// Devices so a console server outside a location-scoped claim still
	// resolves. Build reads only their serials.
	ConsoleServers []netbox.Device

	// BMCAdminSecret is the parsed pop/<slug>/bmc-creds-admin secret
	// for this site, keyed by device serial: the ONLY source of
	// per-machine BMCUser/BMCPass. Nil is fine — Build then leaves
	// every machine's BMC cred empty.
	BMCAdminSecret *awssecrets.AdminSecret

	// HostnameTemplate derives each bound machine's FQDN from its NetBox
	// device name. The zero value leaves the FQDN as the device name.
	HostnameTemplate HostnameTemplate
}

// Build joins the inputs into a SiteSnapshot. Returns an error if the
// inputs are internally inconsistent (e.g. no production prefix).
// SeqNum is left zero; the dispatcher sets it after content-hashing.
//
// Build also performs storage-script selection per machine: the raw
// scripts from the global store are parsed once, then each machine's
// commission data is matched against script rules. The winning script
// is attached to MachineSnapshot.StorageScript. Workers never run the
// match; they just carry the pre-chosen script to the deploy agent.
//
// A bound machine's interface with more than one active address never
// fails the build: chooseInterfaceAddress picks one. Build drops the record
// of those choices; the dispatcher builds through buildWithChoices and
// reports them.
func Build(in Inputs) (model.SiteSnapshot, error) {
	snap, _, err := buildWithChoices(in)
	return snap, err
}

// AddressChoice records one bound machine's interface that carried more
// than one active NetBox address, and the one address its facts got.
type AddressChoice struct {
	Device      string // NetBox device name
	Interface   string // NetBox interface name
	InterfaceID int    // NetBox interface ID
	Chosen      string // the address in SnapshotInterface.IP, CIDR form
	Active      int    // how many active addresses the interface carries
}

// chooseInterfaceAddress picks the one address a machine's interface facts
// carry out of the interface's active addresses (at least one). The rule
// depends only on the addresses, never on the order NetBox listed them in:
//
//   - the device's primary IPv4 (NetBox primary_ip4, matched by address ID;
//     0 for none) when it is one of them;
//   - otherwise an IPv4 address, preferring the interface's own addresses
//     (NetBox role unset or loopback, see netbox.IPAddress.IsOwnAddress)
//     over secondary and shared (anycast, VIP, FHRP) ones, and among those
//     the numerically lowest, ties broken by the shorter prefix length, then
//     the lower NetBox ID;
//   - an interface with no IPv4 address gets an IPv6 address by the same
//     ordering. An address that does not parse sorts after every one that
//     does.
//
// IPv4 comes first because the facts hold one address per interface and
// every shipped network script and the worker's DHCP reservation read it as
// IPv4. The primary comes first because it is the address the operator
// already named as the host's own; on a storage host's ten-address data
// interface it is the host address and the rest are service addresses. An
// own address comes before a lower shared one because NetBox already says
// the shared one is not this host's: the shipped L3 scripts render lo's /32
// as the BGP router-id, which must not be a router's anycast address.
func chooseInterfaceAddress(addrs []netbox.IPAddress, primaryIP4 int) netbox.IPAddress {
	type key struct {
		class int // 0 own IPv4, 1 other IPv4, 2 own IPv6, 3 other IPv6, 4 unparseable
		addr  netip.Addr
		bits  int
	}
	keyOf := func(ip netbox.IPAddress) key {
		var addr netip.Addr
		var bits int
		if p, err := netip.ParsePrefix(ip.Address); err == nil {
			addr, bits = p.Addr().Unmap(), p.Bits()
		} else if a, err := netip.ParseAddr(ip.Address); err == nil {
			addr, bits = a.Unmap(), a.Unmap().BitLen()
		} else {
			return key{class: 4}
		}
		class := 0
		if !addr.Is4() {
			class = 2
		}
		if !ip.IsOwnAddress() {
			class++
		}
		return key{class, addr, bits}
	}
	best, bestKey := addrs[0], keyOf(addrs[0])
	for _, ip := range addrs {
		if primaryIP4 != 0 && ip.ID == primaryIP4 {
			return ip
		}
		k := keyOf(ip)
		c := cmp.Or(
			cmp.Compare(k.class, bestKey.class),
			k.addr.Compare(bestKey.addr),
			cmp.Compare(k.bits, bestKey.bits),
			cmp.Compare(ip.Address, best.Address), // orders unparseable addresses
			cmp.Compare(ip.ID, best.ID),
		)
		if c < 0 {
			best, bestKey = ip, k
		}
	}
	return best
}

// dhcpPrefix picks the slagworks-production prefix the fabric DHCP subnet,
// gateway, DNS servers and lease time come from: the most specific prefix
// containing the config's ephemeral pool start, among
//
//   - every prefix, for a whole-site config (the dispatcher passes it only
//     the prefixes scoped to the site itself);
//   - the prefixes scoped to the same location as Prefixes[0], for a
//     location-scoped config. The dispatcher lists the config's own location
//     first, so its prefix supplies the subnet even when a child location's
//     prefix also holds the pool.
//
// The pool decides, not list order, because a scope carries more than one
// production prefix once an operator tags another so hosts in it resolve a
// gateway, and NetBox lists prefixes by address: the first one listed can be
// a prefix the pool is not in, and the worker would hand out pool addresses
// with that prefix's mask and gateway. When no candidate contains the pool
// start the snapshot is inconsistent whichever is chosen, so the first
// candidate (NetBox's lowest address) is kept and a warning logged.
func dhcpPrefix(in Inputs) netbox.Prefix {
	first := in.Prefixes[0]
	best, bestBits := first, -1
	if pool, err := netip.ParseAddr(in.Config.EphemeralPoolStart); err == nil {
		pool = pool.Unmap()
		for _, p := range in.Prefixes {
			if in.Config.LocationScoped() && (p.ScopeType != first.ScopeType || p.ScopeID != first.ScopeID) {
				continue
			}
			if pfx, err := netip.ParsePrefix(p.Prefix); err == nil && pfx.Bits() > bestBits && pfx.Contains(pool) {
				best, bestBits = p, pfx.Bits()
			}
		}
	}
	if bestBits < 0 {
		slog.Warn("snapshot: the ephemeral pool start is in none of the slagworks-production prefixes the fabric DHCP subnet can come from; serving the first one, so pool addresses go out with a mask and gateway that do not fit them. Move the pool into one of those prefixes (slagctl site-config update), or tag slagworks-production the prefix it lies in, scoped to the config's site or own location",
			"site", in.Site.Slug, "config", in.Config.Slug,
			"ephemeral_pool_start", in.Config.EphemeralPoolStart, "prefix", first.Prefix)
	}
	return best
}

// buildWithChoices is Build plus the list of multi-address interfaces it
// had to choose an address for, ordered by NetBox interface ID.
func buildWithChoices(in Inputs) (model.SiteSnapshot, []AddressChoice, error) {
	if len(in.Prefixes) == 0 {
		return model.SiteSnapshot{}, nil, fmt.Errorf("site %s: no slagworks-production prefix", in.Site.Slug)
	}
	primary := dhcpPrefix(in)
	gateway := prefixGatewayIP(primary)

	dhcp := model.DHCPConfig{
		Subnet:             primary.Prefix,
		Gateway:            gateway,
		EphemeralPoolStart: in.Config.EphemeralPoolStart,
		EphemeralPoolEnd:   in.Config.EphemeralPoolEnd,
		RelayPools:         in.Config.RelayPools,
		Promiscuous:        in.Config.Promiscuous,
		DNS:                prefixCSVField(primary, "dns_servers"),
		LeaseSeconds:       prefixIntField(primary, "dhcp_lease_seconds"),
	}
	// L3 host fabric: generate a single-address relay pool per host /31
	// leg. The leaf's dhcp-helper relays PXE DISCOVERs with giaddr = the
	// leaf side of the ingress port's /31; the only address valid to
	// offer on that link is the host side. Generated from the same
	// NetBox records the netplan renderer reads — no operator pool
	// config per host. Configured RelayPools (legacy /24s during
	// partial conversion) stay; giaddr containment matching picks the
	// /31 pool for converted ports (longest-prefix is irrelevant: the
	// giaddrs are disjoint).
	dhcp.RelayPools = append(dhcp.RelayPools, l3LegRelayPools(in.Interfaces, in.IPs)...)

	devByID := make(map[int]netbox.Device, len(in.Devices))
	for _, d := range in.Devices {
		devByID[d.ID] = d
	}

	// Index device-types by ID so per-machine lookups during the
	// machine loop are O(1). Map values are full DeviceTypes — we
	// only need slagworks_kernel_params today, but keeping the
	// whole struct lets future fields land without re-indexing.
	dtByID := make(map[int]netbox.DeviceType, len(in.DeviceTypes))
	for _, dt := range in.DeviceTypes {
		dtByID[dt.ID] = dt
	}
	ifacesByDev := make(map[int][]netbox.Interface, len(in.Devices))
	for _, iface := range in.Interfaces {
		if iface.Device == nil {
			continue
		}
		ifacesByDev[iface.Device.ID] = append(ifacesByDev[iface.Device.ID], iface)
	}
	// Every active address per interface, not just the last row NetBox
	// listed: a machine's facts carry one address per interface, so the
	// machine loop picks one with chooseInterfaceAddress rather than
	// letting list order pick the survivor.
	activeIPsByIfaceID := make(map[int][]netbox.IPAddress, len(in.IPs))
	for _, ip := range in.IPs {
		if ip.AssignedObjectID != nil && ip.AssignedObjectType == "dcim.interface" && ip.IsActive() {
			id := *ip.AssignedObjectID
			activeIPsByIfaceID[id] = append(activeIPsByIfaceID[id], ip)
		}
	}

	// Per-interface gateway resolver: longest-prefix-match an interface IP
	// against the site prefixes and read the matching prefix's gateway_ip
	// custom field. Built once over all prefixes so the machine loop is a
	// pure lookup. Empty result is fine — an L3 leg's /31 has no gateway_ip
	// and the L3 script doesn't read SnapshotInterface.Gateway anyway.
	//
	// Both production AND OOB prefixes are fed in: an OOB-leg NIC (e.g. an
	// oob-gw's eno3, or any host on the OOB /23) resolves its gateway from
	// the slagworks-oob prefix's gateway_ip (the OOB VIP), which the
	// production-only set doesn't contain. The two tag sets cover disjoint
	// subnets, so a production NIC never matches an OOB prefix and vice
	// versa — longest-prefix-match stays unambiguous. This is what lets the
	// new-cage oob-gw netscript emit a default route via the OOB VIP at
	// install time (transitional egress for deploy-time tailscale enrollment
	// + a pre-ansible host's apt/internet reach), instead of coming up
	// route-less. (Was production-only, which left every OOB NIC's Gateway
	// empty even though the OOB prefix models a gateway_ip.)
	gatewayResolver := newPrefixGatewayResolver(append(append([]netbox.Prefix{}, in.Prefixes...), in.OOBPrefixes...))

	// Parse all storage scripts once. ParseScript pulls the
	// slagworks:match / slagworks:priority / slagworks:kernel_params
	// headers out of the content; an unparseable script is logged and
	// excluded — one bad script must not break snapshot build for a
	// whole site.
	parsedScripts := make([]storage.Script, 0, len(in.Scripts))
	for _, s := range in.Scripts {
		parsed, err := storage.ParseScript(s.Name, []byte(s.Content))
		if err != nil {
			slog.Warn("storage script has unparseable headers, skipping",
				"name", s.Name, "err", err)
			continue
		}
		parsedScripts = append(parsedScripts, *parsed)
	}

	// Parse all network-config scripts once. Matched per-machine below
	// against NetBox identity (site/device_type/hostname); an unparseable
	// script is logged and excluded inside parseNetworkScripts.
	parsedNetScripts := parseNetworkScripts(in.NetworkScripts)

	consoleByDev := buildConsoleEndpoints(in.ConsolePorts, in.ConsoleServers)

	machines := make([]model.MachineSnapshot, 0, len(in.Machines))
	// choices collects every bound-machine interface with more than one
	// active address, keyed by NetBox interface ID (two machines bound to
	// one device see the same interface), for the caller to report.
	choiceByIface := make(map[int]AddressChoice)
	for _, m := range in.Machines {
		// Resolve the NetBox device (if bound) up front: its name is the
		// hostname both the storage and network matchers scope on.
		var dev *netbox.Device
		if m.NetBoxDeviceID != nil {
			if d, ok := devByID[*m.NetBoxDeviceID]; ok {
				dev = &d
			}
		}
		hostname := ""
		if dev != nil {
			hostname = dev.Name
		}
		systemSerial, chassisSerial, systemUUID := extractSystemIdentity(m.CommissionData)
		ms := model.MachineSnapshot{
			ID:             m.ID,
			NetBoxDeviceID: m.NetBoxDeviceID,
			NetBoxSerial:   m.NetBoxSerial,
			State:          m.State,
			DesiredOS:      m.DesiredOS,
			CurrentOS:      m.CurrentOS,
			Locked:         m.Locked,
			PXEEligible:    m.PXEEligible,
			SystemSerial:   systemSerial,
			ChassisSerial:  chassisSerial,
			SystemUUID:     systemUUID,
			Disks:          extractDiskIdentities(m.CommissionData),
			StorageScript:  matchStorageScript(parsedScripts, m.CommissionData, hostname),
		}
		var deviceSerial string
		if m.NetBoxDeviceID != nil {
			if d, ok := devByID[*m.NetBoxDeviceID]; ok {
				ms.Hostname = d.Name
				ms.FQDN = in.HostnameTemplate.Expand(d.Name)
				deviceSerial = d.Serial
				// Per-device KernelParams: NetBox's device → device_type
				// chain is one .ID lookup deep. Empty when the device
				// has no DeviceType, the device-type isn't in the
				// fetched batch, or the custom field is unset.
				if d.DeviceType != nil {
					if dt, ok := dtByID[d.DeviceType.ID]; ok {
						ms.KernelParams = extractKernelParams(dt.CustomFields)
						// Vendor selection data: the manufacturer slug
						// (e.g. "supermicro") drives the worker's choice
						// of vendor-first BMC tool; the model is carried
						// for diagnostics. Both empty when the device-type
						// has no manufacturer in NetBox — worker then takes
						// the generic bmclib/ipmitool path.
						if dt.Manufacturer != nil {
							ms.Manufacturer = dt.Manufacturer.Slug
						}
						ms.Model = dt.Model
					}
				}
				// Select the per-machine host network-config script by
				// matching the device's NetBox identity (site/device_type/
				// hostname). Nil when no script matched — the agent then
				// falls back to its built-in DHCP netplan. The script (not
				// slagworks) owns all topology interpretation: /31 legs,
				// loopback, FRR. The agent ships it the raw interface facts
				// in ms.Interfaces.
				ms.NetworkScript = matchNetworkScript(parsedNetScripts, d)
				// BMCIP: the device's oob_ip is the canonical
				// BMC-presence-and-address signal. This decouples BMC
				// presence from the mgmt_only interface flag — a BMC-less
				// switch's management NIC sets mgmt_only but has no oob_ip,
				// so it must not be mistaken for a BMC.
				if d.OOBIP != nil && d.OOBIP.Address != "" {
					ms.BMCIP = stripMask(d.OOBIP.Address)
				}
				ms.Console = consoleByDev[d.ID]
			}
			primaryIP4 := 0
			if dev != nil && dev.PrimaryIP4 != nil {
				primaryIP4 = dev.PrimaryIP4.ID
			}
			for _, ni := range ifacesByDev[*m.NetBoxDeviceID] {
				addrs := activeIPsByIfaceID[ni.ID]
				iface := model.SnapshotInterface{
					NetBoxID: ni.ID,
					Name:     ni.Name,
					MAC:      strings.ToLower(ni.GetMAC()),
					Type:     classifyMachineInterface(ni, ms.BMCIP != ""),
				}
				if len(addrs) > 0 {
					iface.IP = chooseInterfaceAddress(addrs, primaryIP4).Address
				}
				if len(addrs) > 1 {
					choiceByIface[ni.ID] = AddressChoice{
						Device: hostname, Interface: ni.Name, InterfaceID: ni.ID,
						Chosen: iface.IP, Active: len(addrs),
					}
				}
				// L2-bond facts (additive; absent/zero on L3 hosts whose
				// interfaces carry none of these in NetBox). LAG is the
				// parent bond name for a member; VLAN the untagged VID; MTU
				// the modeled MTU; Gateway the longest-prefix-match gateway
				// for the iface IP.
				if ni.LAG != nil {
					iface.LAG = ni.LAG.Name
				}
				if ni.UntaggedVLAN != nil {
					iface.VLAN = ni.UntaggedVLAN.VID
				}
				if ni.MTU != nil {
					iface.MTU = *ni.MTU
				}
				if iface.IP != "" {
					iface.Gateway = gatewayResolver(iface.IP)
				}
				ms.Interfaces = append(ms.Interfaces, iface)
			}
		}
		// Stamp BMCUser/BMCPass from the device's serial-keyed admin
		// entry — the same and only credential the op-envelope path
		// (resolveBMCRuntime) uses. This makes the snapshot
		// self-sufficient for any feature that needs a BMC cred (SOL
		// pump, power probes, etc.) without those features having to
		// chase per-op bmc_runtime envelopes.
		//
		// A kind=amt entry marks the machine's OOB endpoint as Intel AMT
		// (ms.AMT) and its digest credential fills BMCUser/BMCPass, exactly
		// as the op path's resolveAMTRuntime uses it. The worker's AMT SOL
		// dialer reads it from here.
		var adminEntry *awssecrets.AdminHost
		if in.BMCAdminSecret != nil && deviceSerial != "" {
			adminEntry = in.BMCAdminSecret.Lookup(deviceSerial)
		}
		if adminEntry != nil {
			if adminEntry.Kind == awssecrets.KindAMT {
				ms.AMT = true
			}
			// Kind guard: the admin secret carries bmc, pdu, and other
			// entries keyed by serial; only a kind=bmc (or, for an AMT
			// machine, kind=amt) entry may fill BMCUser/BMCPass, and only
			// when complete.
			usable := adminEntry.Kind == awssecrets.KindBMC || adminEntry.Kind == awssecrets.KindAMT
			if usable && adminEntry.Username != "" && adminEntry.Password != "" {
				ms.BMCUser = adminEntry.Username
				ms.BMCPass = adminEntry.Password
			}
		}
		machines = append(machines, ms)
	}
	choices := make([]AddressChoice, 0, len(choiceByIface))
	for _, id := range slices.Sorted(maps.Keys(choiceByIface)) {
		choices = append(choices, choiceByIface[id])
	}

	// OOB DHCP reservations: every interface (INCLUDING mgmt_only) whose
	// assigned IP falls in a slagworks-oob prefix, across ALL site devices.
	// This is what lets the worker replace dnsmasq for the OOB net. Build
	// fails loudly on a conflicting reservation set rather than serving a
	// half-bad one. Empty in.OOBPrefixes (pre-cutover) → no reservations.
	reservations, err := buildOOBReservations(in.OOBPrefixes, in.Interfaces, in.IPs)
	if err != nil {
		return model.SiteSnapshot{}, nil, fmt.Errorf("site %s: OOB reservations: %w", in.Site.Slug, err)
	}

	// OOB segment config (subnet/gateway/DNS/NTP/dynamic-pool), sourced from
	// the slagworks-oob prefix + its dynamic ip-range. Zero value when there
	// is no OOB prefix (pre-cutover). The worker derives serverIP + bind
	// iface host-locally from OOB.Subnet.
	oob, err := buildOOBConfig(in.OOBPrefixes, in.OOBRanges)
	if err != nil {
		return model.SiteSnapshot{}, nil, fmt.Errorf("site %s: OOB config: %w", in.Site.Slug, err)
	}

	return model.SiteSnapshot{
		Site: model.SiteInfo{
			Name: in.Site.Name,
			Slug: in.Site.Slug,
		},
		DHCP:         dhcp,
		OOB:          oob,
		Machines:     machines,
		Images:       in.Images,
		SSHKeys:      in.SSHKeys,
		Reservations: reservations,
	}, choices, nil
}

// buildOOBConfig assembles the OOB-segment DHCP config from the
// slagworks-oob prefix and its dynamic ip-range. The first OOB prefix is
// authoritative for subnet/gateway/DNS/NTP (there is exactly one OOB
// /22 per site); the dynamic pool comes from the first OOB range. Returns
// the zero value (Subnet=="") when there is no OOB prefix — pre-cutover,
// or a site that doesn't run OOB DHCP. DNS/NTP are read from comma-
// separated prefix custom fields (dns_servers/ntp_servers), mirroring the
// gateway_ip CF pattern; missing/empty = advertise none.
func buildOOBConfig(oobPrefixes []netbox.Prefix, oobRanges []netbox.IPRange) (model.OOBConfig, error) {
	if len(oobPrefixes) == 0 {
		return model.OOBConfig{}, nil
	}
	if len(oobPrefixes) > 1 {
		// A site has exactly one OOB /22. More than one slagworks-oob
		// prefix is a NetBox data error — picking [0] would be order-
		// dependent and could advertise the wrong gateway/DNS. Fail loud.
		ps := make([]string, len(oobPrefixes))
		for i, p := range oobPrefixes {
			ps[i] = p.Prefix
		}
		return model.OOBConfig{}, fmt.Errorf("expected exactly one slagworks-oob prefix, got %d: %v", len(oobPrefixes), ps)
	}
	p := oobPrefixes[0]
	cfg := model.OOBConfig{
		Subnet:       p.Prefix,
		Gateway:      prefixGatewayIP(p),
		DNS:          prefixCSVField(p, "dns_servers"),
		NTP:          prefixCSVField(p, "ntp_servers"),
		LeaseSeconds: prefixIntField(p, "dhcp_lease_seconds"),
	}
	// Each dynamic ip-range is one gateway's split-pool slice, owned by the
	// gateway named in its slagworks_oob_owner custom field. Emit all of
	// them; the worker selects its own by matching its derived serverIP.
	// A slice missing its owner CF is a data error (the worker couldn't tell
	// whose it is), so fail loud. Zero ranges = reservations-only OOB.
	for _, r := range oobRanges {
		owner := iprangeStringCF(r, "slagworks_oob_owner")
		if owner == "" {
			return model.OOBConfig{}, fmt.Errorf("OOB dynamic range %s-%s has no slagworks_oob_owner custom field",
				r.StartAddress, r.EndAddress)
		}
		cfg.DynamicSlices = append(cfg.DynamicSlices, model.OOBDynamicSlice{
			Start: stripMask(r.StartAddress),
			End:   stripMask(r.EndAddress),
			Owner: owner,
		})
	}
	// Reject duplicate slice owners (two ranges claiming the same gateway is
	// ambiguous — which slice does that gateway serve?).
	seenOwner := make(map[string]bool, len(cfg.DynamicSlices))
	for _, s := range cfg.DynamicSlices {
		if seenOwner[s.Owner] {
			return model.OOBConfig{}, fmt.Errorf("two OOB dynamic slices claim owner %s", s.Owner)
		}
		seenOwner[s.Owner] = true
	}
	return cfg, nil
}

// prefixCSVField reads a comma-separated text custom field off a prefix
// and returns the trimmed, non-empty entries. Used for dns_servers /
// ntp_servers (DHCP options 6 / 42), same CF idiom as gateway_ip.
func prefixCSVField(p netbox.Prefix, field string) []string {
	if len(p.CustomFields) == 0 {
		return nil
	}
	var cf map[string]any
	if err := json.Unmarshal(p.CustomFields, &cf); err != nil {
		return nil
	}
	raw, _ := cf[field].(string)
	if raw == "" {
		return nil
	}
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// prefixIntField reads an integer custom field off a prefix. Returns 0 when
// the field is absent, null, or not a number. Used for dhcp_lease_seconds.
// JSON numbers unmarshal to float64 through map[string]any, so we accept
// that and truncate.
func prefixIntField(p netbox.Prefix, field string) int {
	if len(p.CustomFields) == 0 {
		return 0
	}
	var cf map[string]any
	if err := json.Unmarshal(p.CustomFields, &cf); err != nil {
		return 0
	}
	switch v := cf[field].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

// iprangeStringCF reads a text custom field off an ip-range. Returns ""
// when absent/null. Used for slagworks_oob_owner (the split-pool slice
// owner's serverIP).
func iprangeStringCF(r netbox.IPRange, field string) string {
	if len(r.CustomFields) == 0 {
		return ""
	}
	var cf map[string]any
	if err := json.Unmarshal(r.CustomFields, &cf); err != nil {
		return ""
	}
	s, _ := cf[field].(string)
	return s
}

// buildOOBReservations derives the OOB DHCP static reservation set: for
// every interface whose assigned IP falls inside one of the slagworks-oob
// prefixes, emit {MAC, IP}. mgmt_only/BMC interfaces are INCLUDED (the
// whole point — the OOB net is mostly BMCs/PDUs/switch-mgmt ports). The
// MAC is taken via Interface.GetMAC (NetBox 4.x primary_mac_address); the
// IPs come from the raw IPAddress list, filtered to OOB addresses, because
// reservations cover every site device and the machine loop's one-address
// choice covers only bound machines.
//
// Fails loudly (per the plan's conflict-rejection rule) on:
//   - more than one OOB IP on a single interface,
//   - the same MAC mapped to two different IPs (across interfaces),
//   - the same IP mapped to two different MACs.
//
// A half-bad reservation set is worse than none — it would hand a host the
// wrong static IP. Returning an error fails the whole snapshot build so the
// operator sees the NetBox data problem instead of shipping a bad lease.
// Interfaces with no MAC or no OOB IP are silently skipped (not every iface
// is a reservation). Returns nil when there are no OOB prefixes.
func buildOOBReservations(oobPrefixes []netbox.Prefix, interfaces []netbox.Interface, ips []netbox.IPAddress) ([]model.Reservation, error) {
	if len(oobPrefixes) == 0 {
		return nil, nil
	}
	nets := make([]*net.IPNet, 0, len(oobPrefixes))
	for _, p := range oobPrefixes {
		_, n, err := net.ParseCIDR(p.Prefix)
		if err != nil {
			return nil, fmt.Errorf("parse OOB prefix %q: %w", p.Prefix, err)
		}
		nets = append(nets, n)
	}
	inOOB := func(ip net.IP) bool {
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}

	// All OOB IPs per interface (NOT last-write-wins): keeps every
	// IPAddress row so a multi-OOB-IP interface is detectable below.
	oobIPsByIface := make(map[int][]string)
	for _, ip := range ips {
		if ip.AssignedObjectID == nil || ip.AssignedObjectType != "dcim.interface" || !ip.IsActive() {
			continue
		}
		parsed, _, err := net.ParseCIDR(ip.Address)
		if err != nil {
			parsed = net.ParseIP(ip.Address)
		}
		if parsed == nil || !inOOB(parsed) {
			continue
		}
		id := *ip.AssignedObjectID
		oobIPsByIface[id] = append(oobIPsByIface[id], parsed.String())
	}

	byMAC := make(map[string]string) // mac(lower) -> ip
	byIP := make(map[string]string)  // ip -> mac(lower)
	var out []model.Reservation
	for _, iface := range interfaces {
		oobIPs := oobIPsByIface[iface.ID]
		if len(oobIPs) == 0 {
			continue
		}
		if len(oobIPs) > 1 {
			return nil, fmt.Errorf("interface %d (%s) has %d OOB IPs (%v); expected exactly one",
				iface.ID, iface.Name, len(oobIPs), oobIPs)
		}
		ipStr := oobIPs[0]
		mac := strings.ToLower(iface.GetMAC())
		if mac == "" {
			continue
		}
		if prev, ok := byMAC[mac]; ok && prev != ipStr {
			return nil, fmt.Errorf("MAC %s assigned two OOB IPs (%s and %s)", mac, prev, ipStr)
		}
		if prevMAC, ok := byIP[ipStr]; ok && prevMAC != mac {
			return nil, fmt.Errorf("OOB IP %s assigned to two MACs (%s and %s)", ipStr, prevMAC, mac)
		}
		if _, dup := byMAC[mac]; dup {
			continue // identical (mac,ip) already recorded
		}
		byMAC[mac] = ipStr
		byIP[ipStr] = mac
		out = append(out, model.Reservation{MAC: mac, IP: ipStr})
	}
	return out, nil
}

// defaultConsoleBaud is the console speed assumed when NetBox leaves a
// console port's speed unset (every console we cable today runs 115200).
const defaultConsoleBaud = 115200

// buildConsoleEndpoints maps device ID → the console-server endpoint its
// console port is cabled to. A port is used only when its cable path ends
// on exactly one console-server port; an uncabled port (or one ending on
// anything else) yields no entry. A device with more than one cabled
// console port is ambiguous — which line carries its console? — so it gets
// no entry and a warning rather than an order-dependent pick.
func buildConsoleEndpoints(ports []netbox.ConsolePort, servers []netbox.Device) map[int]*model.SnapshotConsole {
	serialByID := make(map[int]string, len(servers))
	for _, s := range servers {
		serialByID[s.ID] = s.Serial
	}
	out := make(map[int]*model.SnapshotConsole)
	ambiguous := make(map[int]bool)
	for _, p := range ports {
		if p.Device == nil || p.ConnectedEndpointsType != "dcim.consoleserverport" ||
			!p.ConnectedEndpointsReachable || len(p.ConnectedEndpoints) != 1 {
			continue
		}
		ep := p.ConnectedEndpoints[0]
		if ep.Device == nil || ep.Name == "" {
			continue
		}
		devID := p.Device.ID
		if _, dup := out[devID]; dup || ambiguous[devID] {
			if !ambiguous[devID] {
				slog.Warn("snapshot: device has more than one cabled console port; skipping console capture for it",
					"device", p.Device.Name, "device_id", devID)
			}
			ambiguous[devID] = true
			delete(out, devID)
			continue
		}
		baud := defaultConsoleBaud
		if p.Speed != nil && p.Speed.Value > 0 {
			baud = p.Speed.Value
		}
		out[devID] = &model.SnapshotConsole{
			Server:       ep.Device.Name,
			ServerSerial: serialByID[ep.Device.ID],
			Port:         ep.Name,
			Baud:         baud,
		}
	}
	return out
}

// extractKernelParams pulls the slagworks_kernel_params value out of a
// NetBox DeviceType.custom_fields blob. Returns "" when missing or the
// blob is empty/malformed — callers treat empty as "deploy refused".
func extractKernelParams(cf json.RawMessage) string {
	if len(cf) == 0 {
		return ""
	}
	var parsed struct {
		KernelParams string `json:"slagworks_kernel_params"`
	}
	if err := json.Unmarshal(cf, &parsed); err != nil {
		return ""
	}
	return parsed.KernelParams
}

// matchStorageScript runs the commission data + NetBox identity through
// storage.Match and returns a snapshot-wire-shape script, or nil if no script
// matched or commission data is missing. hostname is the NetBox device name
// (empty for an unbound machine) — it lets a model-matched storage script AND a
// defense-in-depth hostname scope so it can't claim a host that merely shares
// the chassis. Workers carry the result verbatim to the deploy agent; no
// re-matching happens on the site side.
func matchStorageScript(scripts []storage.Script, commData []byte, hostname string) *model.SnapshotStorageScript {
	if len(scripts) == 0 || len(commData) == 0 {
		return nil
	}
	matched, err := storage.Match(scripts, commData, map[string]string{"hostname": hostname})
	if err != nil || matched == nil {
		return nil
	}
	return &model.SnapshotStorageScript{
		Name:    matched.Path,
		Content: string(matched.Content),
	}
}

// classifyInterface returns "ipmi" for a management-only interface and
// "physical" otherwise. It is the topology-only classifier used where no
// device-level BMC identity is available. Machine snapshots must use
// classifyMachineInterface, which combines this flag with device.oob_ip.
//
// The NetBox Interface.Type field (1000base-t, 10gbase-sr, etc.)
// carries the NIC class for boot-config decisions; we don't
// collapse that into the snapshot — workers don't need it for
// DHCP/PXE today.
func classifyInterface(i netbox.Interface) string {
	if i.MgmtOnly {
		return "ipmi"
	}
	// A NetBox interface of type "lag" is a bond aggregate (the parent the
	// members point at). The L2 bond network-config script keys on this to
	// find the bond interface (which carries the host IP but no MAC).
	if i.Type != nil && i.Type.Value == "lag" {
		return "lag"
	}
	return "physical"
}

// classifyMachineInterface applies device-level BMC identity to an interface.
// NetBox's mgmt_only flag describes traffic purpose, not hardware ownership: on
// a BMC-less switch it marks the host OS management/PXE NIC. device.oob_ip is
// the canonical BMC-presence signal, so only a machine that actually has a BMC
// may classify its mgmt_only interfaces as ipmi. On a BMC-less device the flag
// is ignored and the interface classifies by its NetBox type alone: a NIC is
// physical (staying in the worker's machine MAC index, so the OOB DHCP safety
// gate does not suppress its PXE bootfile) and a bond is lag.
func classifyMachineInterface(i netbox.Interface, hasBMC bool) string {
	if !hasBMC {
		i.MgmtOnly = false
	}
	return classifyInterface(i)
}

// newPrefixGatewayResolver returns a function mapping an interface CIDR to
// the gateway_ip custom field of the LONGEST containing prefix, or "" when no
// prefix contains the address.
//
// Longest-prefix-match must consider ALL containing prefixes, including ones
// with no gateway_ip: the gateway of record is the most-specific containing
// prefix's gateway_ip, even when that is empty. If a host's specific /24
// deliberately carries no gateway_ip, the answer is "" — NOT the gateway of
// some enclosing /16, which is very likely off this host's L2 segment and
// unreachable. (Skipping gateway-less prefixes would let a broad supernet
// silently win.) The L2 bond host's 10.253.0.22/22 resolves to the /22's
// gateway_ip (10.253.3.254), not an enclosing /16's.
func newPrefixGatewayResolver(prefixes []netbox.Prefix) func(ifaceCIDR string) string {
	type entry struct {
		net     *net.IPNet
		ones    int
		gateway string
	}
	entries := make([]entry, 0, len(prefixes))
	for _, p := range prefixes {
		_, ipnet, err := net.ParseCIDR(p.Prefix)
		if err != nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		entries = append(entries, entry{net: ipnet, ones: ones, gateway: prefixGatewayIP(p)})
	}
	return func(ifaceCIDR string) string {
		ip, _, err := net.ParseCIDR(ifaceCIDR)
		if err != nil {
			// Tolerate a bare address (no mask) too.
			ip = net.ParseIP(ifaceCIDR)
			if ip == nil {
				return ""
			}
		}
		best := ""
		bestOnes := -1
		for _, e := range entries {
			if e.net.Contains(ip) && e.ones > bestOnes {
				best = e.gateway
				bestOnes = e.ones
			}
		}
		return best
	}
}

// prefixGatewayIP decodes the gateway_ip custom field off a NetBox prefix.
// Returns "" when the prefix has no custom fields or no gateway_ip set.
// Mirrors the inline decode Build does for the primary DHCP prefix.
func prefixGatewayIP(p netbox.Prefix) string {
	if len(p.CustomFields) == 0 {
		return ""
	}
	var cf struct {
		GatewayIP string `json:"gateway_ip"`
	}
	if err := json.Unmarshal(p.CustomFields, &cf); err != nil {
		return ""
	}
	return cf.GatewayIP
}

// stripMask returns just the host portion of a CIDR address.
// NetBox returns IPAddresses as CIDR strings ("10.0.0.20/24"); workers
// configure DHCP/PXE with bare IPs.
func stripMask(addr string) string {
	if before, _, ok := strings.Cut(addr, "/"); ok {
		return before
	}
	return addr
}

// extractSystemIdentity pulls the SMBIOS serials and system UUID out of a
// commission report. Empty commission data, malformed JSON, or a missing
// system object yields empty values; preflight distinguishes a never-
// commissioned host (valid for commission) from a deploy that must fail
// closed without persisted identity.
func extractSystemIdentity(commData []byte) (systemSerial, chassisSerial, systemUUID string) {
	if len(commData) == 0 {
		return "", "", ""
	}
	var cd struct {
		System *struct {
			SerialNumber  string `json:"serial_number"`
			ChassisSerial string `json:"chassis_serial"`
			UUID          string `json:"uuid"`
		} `json:"system"`
	}
	if err := json.Unmarshal(commData, &cd); err != nil || cd.System == nil {
		return "", "", ""
	}
	return model.NormalizeHardwareSerial(cd.System.SerialNumber),
		model.NormalizeHardwareSerial(cd.System.ChassisSerial),
		strings.TrimSpace(cd.System.UUID)
}

func extractDiskIdentities(commData []byte) []model.SnapshotDiskIdentity {
	if len(commData) == 0 {
		return nil
	}
	var v struct {
		Disks []struct {
			Serial string `json:"serial"`
			WWN    string `json:"wwn"`
		} `json:"disks"`
	}
	if json.Unmarshal(commData, &v) != nil {
		return nil
	}
	out := make([]model.SnapshotDiskIdentity, 0, len(v.Disks))
	for _, d := range v.Disks {
		serial := model.NormalizeHardwareIdentifier(d.Serial)
		wwn := model.NormalizeHardwareIdentifier(d.WWN)
		if serial == "" && wwn == "" {
			continue
		}
		out = append(out, model.SnapshotDiskIdentity{Serial: serial, WWN: wwn})
	}
	return out
}
