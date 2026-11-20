package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/google/uuid"
)

// SiteSnapshot is the per-site view global produces on demand from live
// NetBox + live Postgres. Plan 2 wires it into the new WS sync protocol.
//
// Storage scripts are not carried as a site-wide collection: global
// performs the make/model match per machine at snapshot-build time and
// attaches the pre-chosen script to each MachineSnapshot. The worker
// never runs the match logic.
type SiteSnapshot struct {
	SeqNum   int64             `json:"seq_num"`
	Site     SiteInfo          `json:"site"`
	DHCP     DHCPConfig        `json:"dhcp"`
	Machines []MachineSnapshot `json:"machines"`
	Images   []Image           `json:"images"`
	SSHKeys  []string          `json:"ssh_keys"`

	// Operations are NOT carried in the snapshot (Tier 2 Phase 2). They
	// left the identity snapshot entirely and flow on the dedicated
	// op-event stream (ws.OpDelta/OpResync/OpRemoved). Machine.State
	// remains the durable resting-state baseline; transient op-driven
	// states ride the op stream's machine-state overlay. See
	// docs/plans/2026-07-05-ops-out-of-snapshot.md.

	// OOB is the OOB-segment DHCP configuration, sourced by the builder
	// from the `slagworks-oob` prefix (subnet + gateway_ip CF), its
	// `slagworks-oob-dynamic` ip-range (the dynamic pool), and the DNS/NTP
	// the OOB segment advertises. Zero-valued (Subnet=="") when the site
	// has no slagworks-oob prefix (pre-cutover). The site worker derives
	// its own serverIP + bind interface locally (the local NIC whose IP is
	// inside OOB.Subnet) — those are host-local and not carried here.
	OOB OOBConfig `json:"oob"`

	// Reservations is the full OOB DHCP static reservation set: every
	// NetBox interface (INCLUDING mgmt_only/BMC interfaces) whose assigned
	// IP falls in a `slagworks-oob`-tagged prefix, across ALL site devices
	// — not just slagworks machines. This is what lets the site worker
	// replace dnsmasq as the OOB DHCP server: BMCs, PDUs, switch-mgmt
	// ports, and every other OOB host gets its reserved IP. Distinct from
	// the per-machine interface IPs in MachineSnapshot.Interfaces (which
	// feed the same ReserveIP lookup but are scoped to slagworks machines
	// and exclude ipmi). A MAC whose interfaces carry more than one OOB
	// address, or whose address another MAC also carries, has no entry:
	// the builder leaves it out and logs it rather than guess. Empty on
	// sites with no slagworks-oob prefix.
	Reservations []Reservation `json:"reservations,omitempty"`
}

// Reservation is one OOB DHCP static reservation: a MAC and the IP it
// must always be offered. MAC is lowercased colon-form; IP is a bare
// address (no CIDR). Sourced from NetBox at snapshot-build time.
type Reservation struct {
	MAC string `json:"mac"`
	IP  string `json:"ip"`
}

type SiteInfo struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type DHCPConfig struct {
	Subnet             string      `json:"subnet"`
	Gateway            string      `json:"gateway"`
	EphemeralPoolStart string      `json:"ephemeral_pool_start"`
	EphemeralPoolEnd   string      `json:"ephemeral_pool_end"`
	RelayPools         []RelayPool `json:"relay_pools,omitempty"`
	Promiscuous        bool        `json:"promiscuous"`
	// DNS are the resolvers advertised on the fabric segment (option 6),
	// from the slagworks-production prefix dns_servers custom field
	// (comma-separated), mirroring OOBConfig.DNS. Empty = handler default.
	DNS []string `json:"dns,omitempty"`
	// LeaseSeconds is the lease duration (option 51) from the prefix
	// dhcp_lease_seconds custom field. 0 = handler default.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
	// Workers are the IDs of the site workers registered to this site
	// config, sorted. Every worker builds its pools from this one
	// snapshot, so the worker at position i allocates only from share i
	// of the ephemeral range and of each relay pool's range (a /31 relay
	// pool is not split): see dhcp.PoolConfig.WorkerShare. A registered
	// worker that is offline or on standby keeps its share, so the shares
	// do not move when a worker disconnects or reconnects.
	Workers []string `json:"workers,omitempty"`
}

// OOBConfig is the OOB-segment DHCP configuration the site worker binds in
// addition to the fabric/relay leg (replacing dnsmasq). All fields are
// segment-wide (same on every oob-gw) and sourced from NetBox; the
// per-gateway serverIP and bind interface are derived host-locally by the
// worker (the local NIC whose IP is inside Subnet). Zero value (Subnet=="")
// means "no OOB segment" — the worker binds only the fabric leg.
type OOBConfig struct {
	// Subnet is the OOB CIDR (the slagworks-oob prefix), e.g.
	// "10.254.240.0/22". The worker matches its own interface IPs against
	// this to find the bind interface + serverIP.
	Subnet string `json:"subnet,omitempty"`
	// Gateway is the OOB default gateway (the prefix's gateway_ip CF),
	// advertised as DHCP option 3.
	Gateway string `json:"gateway,omitempty"`
	// DNS are the OOB resolvers (option 6). For pop1 these are the oob-gws
	// themselves; dnsmasq keeps the DNS role. Empty = advertise none.
	DNS []string `json:"dns,omitempty"`
	// NTP are the OOB NTP servers (option 42). Empty = advertise none.
	NTP []string `json:"ntp,omitempty"`
	// DynamicSlices are the per-gateway dynamic-pool slices (active/active
	// split-pool, design D4). Each slice is owned by exactly one gateway
	// (Owner = that gateway's OOB serverIP); a gateway originates fresh
	// leases ONLY from its own slice, and NAKs a broadcast request for an
	// address in a PEER's slice (forcing re-DISCOVER). The worker selects
	// its slice by matching Owner against its derived serverIP. Empty = no
	// dynamic pool (reservations-only OOB segment). Sourced from the
	// slagworks-oob-dynamic ip-ranges (one per gateway), keyed by the
	// slagworks_oob_owner custom field.
	DynamicSlices []OOBDynamicSlice `json:"dynamic_slices,omitempty"`

	// LeaseSeconds is the DHCP lease duration the OOB segment advertises
	// (option 51), from the prefix dhcp_lease_seconds custom field. 0 = use
	// the handler default. Kept short (300s/5m) during rollout so a bad
	// dynamic lease self-corrects fast; turned up later for steady state.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
}

// OOBDynamicSlice is one gateway's slice of the OOB dynamic pool.
type OOBDynamicSlice struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// Owner is the OOB serverIP of the gateway that originates leases from
	// this slice (the slagworks_oob_owner ip-range custom field). The
	// worker whose derived OOB serverIP equals Owner serves this slice as
	// its own; the other gateway treats it as a peer slice (NAK on a
	// broadcast request for an address in it).
	Owner string `json:"owner"`
}

// SnapshotInterface is a NIC as it appears in a site snapshot. The
// "Snapshot" prefix avoids collision with Go's interface keyword and
// keeps greps unambiguous.
type SnapshotInterface struct {
	NetBoxID int    `json:"netbox_id"`
	Name     string `json:"name"`
	MAC      string `json:"mac"`
	Type     string `json:"type"` // "physical" | "ipmi" | "lag"
	// IP is one status=active NetBox address of the interface, in CIDR
	// form. When the interface has several, Build picks the device's
	// primary_ip4 if it is among them, else the numerically lowest IPv4
	// address (the lowest IPv6 address when there is no IPv4), whatever
	// order NetBox listed them in. Within a family, addresses whose NetBox
	// role is unset or loopback come before those with another role
	// (secondary, anycast, vip, vrrp, hsrp, glbp, carp).
	IP string `json:"ip,omitempty"`

	// The fields below carry the extra topology an L2 bond host needs and
	// an L3 routed-NIC host derives from its addressing. All omitempty so
	// an L3 host's interface facts are byte-for-byte unchanged (the L3
	// network-config script never reads them).

	// LAG is the parent bond interface name when this interface is a bond
	// member (NetBox iface.lag.name). Empty for non-members and for the
	// bond interface itself. The bond network-config script groups members
	// by matching their LAG against the bond interface's Name.
	LAG string `json:"lag,omitempty"`

	// VLAN is the interface's untagged (native) VLAN id (NetBox
	// untagged_vlan.vid). 0 = none modeled. This is the UNTAGGED vid, so it
	// is non-zero even on a plain untagged segment (e.g. PVID 1) — it must
	// NOT be used to key tagged-subinterface creation.
	VLAN int `json:"vlan,omitempty"`

	// Gateway is the default gateway for this interface's IP, resolved from
	// the gateway_ip custom field of the longest-prefix-matching NetBox
	// prefix. Empty when no containing prefix carries a gateway_ip. The
	// bond script installs the default route via this address.
	Gateway string `json:"gateway,omitempty"`

	// MTU is the interface MTU from NetBox, 0 when unmodeled. The
	// network-config script applies its own per-segment default when 0.
	MTU int `json:"mtu,omitempty"`
}

// SnapshotDiskIdentity is the stable, non-path identity retained from the
// last accepted commission result. It lets a site reject a disk reported by
// one machine when that serial/WWN is already owned by another.
type SnapshotDiskIdentity struct {
	Serial string `json:"serial,omitempty"`
	WWN    string `json:"wwn,omitempty"`
}

type MachineSnapshot struct {
	ID             uuid.UUID `json:"id"`
	NetBoxDeviceID *int      `json:"netbox_device_id,omitempty"`
	NetBoxSerial   string    `json:"netbox_serial,omitempty"`
	Hostname       string    `json:"hostname,omitempty"`

	// FQDN is the machine's deployed hostname, derived from the NetBox
	// device name by global's hostname template (--hostname-template): with
	// "{node}.internal.{zone}.example.net", a device named "oob-gw-1.pop1"
	// gets "oob-gw-1.internal.pop1.example.net". With no template it is the
	// device name. The deploy agent writes this to /etc/hostname +
	// /etc/hosts on the target. Empty when the template needs {node}/{zone}
	// and Hostname has no dot to split at, or when the result is not a valid
	// hostname (agent then leaves the image default).
	FQDN        string              `json:"fqdn,omitempty"`
	Interfaces  []SnapshotInterface `json:"interfaces,omitempty"`
	BMCIP       string              `json:"bmc_ip,omitempty"`
	State       MachineState        `json:"state"`
	DesiredOS   string              `json:"desired_os,omitempty"`
	CurrentOS   string              `json:"current_os,omitempty"`
	Locked      bool                `json:"locked"`
	PXEEligible bool                `json:"pxe_eligible"`
	// BMCUser/BMCPass are the device's serial-keyed bmc-creds-admin
	// entry (kind=bmc, or kind=amt when AMT), stamped by global at
	// snapshot build — the same credential op envelopes carry.
	BMCUser string `json:"bmc_user,omitempty"`
	BMCPass string `json:"bmc_pass,omitempty"`

	// AMT is true when the device's OOB endpoint (BMCIP, the NetBox
	// oob_ip) is Intel AMT rather than an IPMI/Redfish BMC: its serial-
	// keyed bmc-creds-admin entry is kind=amt. BMCUser/BMCPass then hold
	// the AMT digest credential. Every IPMI-shaped path (ipmitool SOL,
	// commission bmc_ipmitool, deploy bmc_reconcile, chassis-serial
	// verification) is skipped for an AMT machine; the cast pump dials AMT
	// redirection instead.
	AMT bool `json:"amt,omitempty"`

	// SystemSerial, ChassisSerial, and SystemUUID are the SMBIOS identity
	// collected at commission time. The serials let preflight corroborate the
	// persisted inventory against both the stable machine binding and current
	// NetBox device; SystemUUID is also passed to deploy as
	// slagworks.expect-uuid so the agent refuses hardware drift before touching
	// disks. Raw commission inventory does not ship in the snapshot — global
	// extracts only these identity fields at snapshot-build time.
	SystemSerial  string                 `json:"system_serial,omitempty"`
	ChassisSerial string                 `json:"chassis_serial,omitempty"`
	SystemUUID    string                 `json:"system_uuid,omitempty"`
	Disks         []SnapshotDiskIdentity `json:"disks,omitempty"`

	// Manufacturer is the device-type manufacturer slug from NetBox
	// (device.device_type.manufacturer.slug, e.g. "supermicro"),
	// extracted global-side at snapshot-build time like KernelParams.
	// The site worker uses it to pick the vendor-first BMC path: a
	// Supermicro host routes power/boot/serial through SMCIPMITool when
	// the operator has installed it (see IsSupermicro). Empty means
	// "vendor unknown" and takes the generic bmclib path.
	Manufacturer string `json:"manufacturer,omitempty"`

	// Model is the device-type model string from NetBox
	// (device.device_type.model, e.g. "AS-1115HS-TNR"). Carried
	// alongside Manufacturer for operator-facing diagnostics and any
	// future model-specific dispatch.
	Model string `json:"model,omitempty"`

	// KernelParams is the per-hardware kernel cmdline string sourced
	// from NetBox device_type.custom_fields.slagworks_kernel_params.
	// It is the SOURCE OF TRUTH for the `console=ttyS?,baud` shape
	// that determines which UART carries kernel output post-boot AND
	// during the deploy-initrd. Commission probes this value to
	// confirm the BMC SOL actually relays bytes from the configured
	// tty before letting the host onto the deploy path.
	//
	// Empty means the device-type's slagworks_kernel_params field
	// is unset. Deploy refuses such machines: every
	// production host must declare its console explicitly.
	//
	// Sentinel: a literal "none" value indicates the device has no
	// SOL access (e.g. mellanox switches with no BMC). The agent
	// skips probing on this value and writes no console= at all.
	KernelParams string `json:"kernel_params,omitempty"`

	// StorageScript is the pre-selected storage layout script for this
	// machine, chosen global-side by matching the machine's commission
	// data against Script.Rules. Nil when no script matched (agent
	// falls back to a default layout). The agent re-runs the inventory
	// collector against live hardware at deploy time and pipes the
	// result to this script — so the script sees current state, not
	// a cached commission blob.
	StorageScript *SnapshotStorageScript `json:"storage_script,omitempty"`

	// NetworkScript is the pre-selected host network-config script for
	// this machine, chosen global-side by matching the machine's NetBox
	// identity (site / device_type / hostname) against Script.Rules. Nil
	// when no script matched — the agent then falls back to its built-in
	// all-en*-DHCP netplan, same 204-means-fallback semantics as
	// KernelParams. The agent runs the script in the deploy initrd with
	// the machine's NetBox interface facts injected (SLAGWORKS_NETWORK_FACTS);
	// the script emits a file+unit bundle the agent applies to /target.
	// All topology interpretation (/31 legs, loopback, FRR) lives in the
	// script — slagworks stays network-model-agnostic, exactly as it is
	// storage-layout-agnostic.
	NetworkScript *SnapshotNetworkScript `json:"network_script,omitempty"`

	// Console is the console-server port this machine's NetBox console
	// port is cabled to (dcim/console-ports → connected console-server
	// port), extracted global-side at snapshot-build time. Nil when the
	// machine has no console port or it is uncabled. A site worker whose
	// own hardware serial equals Console.ServerSerial streams the line
	// into the console cast from its local tty, for machines with no BMC
	// (BMC SOL / AMT keep precedence). See docs/user/operations.md
	// ("Console transports").
	Console *SnapshotConsole `json:"console,omitempty"`
}

// SnapshotConsole is a machine's cabled console-server endpoint.
type SnapshotConsole struct {
	// Server is the console-server's NetBox device name (e.g.
	// "oob-gw-1.pop1"); diagnostic only.
	Server string `json:"server"`
	// ServerSerial is the console-server device's NetBox serial: the
	// identity a site worker matches against its own verified SMBIOS
	// serial (the same serial global joins the worker's hello to). Empty
	// when the device has no serial in NetBox; then no worker matches.
	ServerSerial string `json:"server_serial,omitempty"`
	// Port is the console-server port name, which is the device node
	// under /dev on the console server (e.g. "ttyS4").
	Port string `json:"port"`
	// Baud is the console port's speed; 115200 when NetBox leaves it unset.
	Baud int `json:"baud"`
}

// IsSupermicro reports whether the machine's NetBox manufacturer slug names
// Supermicro. NetBox slugifies "Supermicro" (the DMI board vendor) to
// "supermicro"; we match that exact slug.
func (m *MachineSnapshot) IsSupermicro() bool {
	return m.Manufacturer == "supermicro"
}

// SnapshotStorageScript is the wire-shape of a storage script carried
// inside a snapshot. Separate from model.StorageScript (the DB record)
// because the snapshot carries the parsed body the agent runs.
type SnapshotStorageScript struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SnapshotNetworkScript is the wire-shape of a network-config script
// carried inside a snapshot. Separate from model.NetworkScript (the DB
// record) because the snapshot carries the parsed body the agent runs.
type SnapshotNetworkScript struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ContentHash returns a hex sha256 of the snapshot body excluding SeqNum.
// Collections are sorted before hashing so the result is invariant under
// goroutine-order / iteration-order differences at build time.
//
// Sorting is done on copies of each slice so the caller's snapshot is
// never mutated — concurrent ContentHash calls on snapshots that share
// backing arrays would otherwise race on sort.Slice.
func (s SiteSnapshot) ContentHash() (string, error) {
	copied := s
	copied.SeqNum = 0

	copied.Machines = append([]MachineSnapshot(nil), s.Machines...)
	for i := range copied.Machines {
		copied.Machines[i].Interfaces = append([]SnapshotInterface(nil), s.Machines[i].Interfaces...)
	}
	copied.Images = append([]Image(nil), s.Images...)
	copied.SSHKeys = append([]string(nil), s.SSHKeys...)
	copied.Reservations = append([]Reservation(nil), s.Reservations...)
	// OOB is a value field, but its DynamicSlices slice is sorted below for
	// hash stability (NetBox/API order isn't guaranteed). Copy the slice so
	// we don't mutate the caller's backing array. DNS/NTP order is
	// authored (CSV order in NetBox) and deterministic, so they're not
	// re-sorted — that order is intentional and stable. DHCP.RelayPools
	// isn't re-sorted either: configured pools keep their authored order
	// (the worker serves from the first pool containing giaddr), and the
	// builder emits the generated /31 pools sorted by subnet. Nor is
	// DHCP.Workers: the builder emits it sorted, and its order assigns each
	// worker's share of the dynamic ranges.
	copied.OOB.DynamicSlices = append([]OOBDynamicSlice(nil), s.OOB.DynamicSlices...)

	// Machine.ID is a UUID and guaranteed unique, so a simple comparator
	// fully orders. For the other slices we add tiebreakers: interface
	// MACs can collide on bonded NICs; Image/Script Name is unique in
	// practice but cheap to tiebreak on for safety.
	sort.Slice(copied.Machines, func(i, j int) bool {
		return copied.Machines[i].ID.String() < copied.Machines[j].ID.String()
	})
	for i := range copied.Machines {
		ifaces := copied.Machines[i].Interfaces
		sort.Slice(ifaces, func(a, b int) bool {
			if ifaces[a].MAC != ifaces[b].MAC {
				return ifaces[a].MAC < ifaces[b].MAC
			}
			if ifaces[a].Name != ifaces[b].Name {
				return ifaces[a].Name < ifaces[b].Name
			}
			return ifaces[a].IP < ifaces[b].IP
		})
	}
	sort.Strings(copied.SSHKeys)
	sort.Slice(copied.Images, func(i, j int) bool {
		if copied.Images[i].Name != copied.Images[j].Name {
			return copied.Images[i].Name < copied.Images[j].Name
		}
		return copied.Images[i].ID.String() < copied.Images[j].ID.String()
	})
	sort.Slice(copied.Reservations, func(i, j int) bool {
		if copied.Reservations[i].MAC != copied.Reservations[j].MAC {
			return copied.Reservations[i].MAC < copied.Reservations[j].MAC
		}
		return copied.Reservations[i].IP < copied.Reservations[j].IP
	})
	sort.Slice(copied.OOB.DynamicSlices, func(i, j int) bool {
		return copied.OOB.DynamicSlices[i].Owner < copied.OOB.DynamicSlices[j].Owner
	})

	data, err := json.Marshal(copied)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}
