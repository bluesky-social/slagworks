package netbox

import "encoding/json"

// NetBox object types used across the package.
// Only the fields slagworks reads or writes are included.

type Site struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type Manufacturer struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type DeviceType struct {
	ID           int             `json:"id"`
	Manufacturer *Manufacturer   `json:"manufacturer,omitempty"`
	Model        string          `json:"model"`
	Slug         string          `json:"slug"`
	CustomFields json.RawMessage `json:"custom_fields,omitempty"`
}

type DeviceRole struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// DeviceStatus represents the NetBox device status field.
type DeviceStatus string

const (
	DeviceStatusOffline         DeviceStatus = "offline"
	DeviceStatusActive          DeviceStatus = "active"
	DeviceStatusPlanned         DeviceStatus = "planned"
	DeviceStatusStaged          DeviceStatus = "staged"
	DeviceStatusFailed          DeviceStatus = "failed"
	DeviceStatusInventory       DeviceStatus = "inventory"
	DeviceStatusDecommissioning DeviceStatus = "decommissioning"
)

type Device struct {
	ID       int          `json:"id"`
	Name     string       `json:"name"`
	Serial   string       `json:"serial,omitempty"`
	AssetTag *string      `json:"asset_tag,omitempty"`
	Status   *NestedValue `json:"status,omitempty"`
	Site     *NestedRef   `json:"site,omitempty"`
	// Location is the brief location ref NetBox embeds on a device. It
	// carries id/name/slug but NOT the parent chain, so subtree
	// membership must be resolved against a separately-fetched location
	// tree (see Client.ListLocationsForSite) — never off this field.
	Location   *NestedRef `json:"location,omitempty"`
	DeviceType *NestedRef `json:"device_type,omitempty"`
	Role       *NestedRef `json:"role,omitempty"`
	Rack       *NestedRef `json:"rack,omitempty"`
	Position   *float64   `json:"position,omitempty"`
	PrimaryIP4 *NestedRef `json:"primary_ip4,omitempty"`
	// OOBIP is the device's out-of-band management IP (NetBox Device
	// `oob_ip`), the canonical BMC-presence-and-address signal. Nil or
	// empty when the device has no BMC (e.g. a BMC-less switch whose
	// management NIC only carries mgmt_only). Snapshot Build derives
	// MachineSnapshot.BMCIP solely from this field.
	OOBIP        *NestedIPAddress `json:"oob_ip,omitempty"`
	Comments     string           `json:"comments,omitempty"`
	CustomFields json.RawMessage  `json:"custom_fields,omitempty"`
	Tags         []NestedTag      `json:"tags,omitempty"`
	Interfaces   []Interface      `json:"interfaces,omitempty"`
}

// NestedIPAddress mirrors the compact IP-address ref NetBox embeds on
// device-level fields like primary_ip4 and oob_ip. Address is a CIDR
// string ("10.0.0.1/24"); callers strip the mask where a bare host
// address is needed.
type NestedIPAddress struct {
	ID      int    `json:"id"`
	Address string `json:"address"`
}

// NestedRef is a compact ID+name reference NetBox uses for foreign keys.
type NestedRef struct {
	ID   int    `json:"id"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// NestedValue is used for choice fields (status, type) that return {value, label}.
type NestedValue struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// ChoiceString holds a NetBox choice field that is inconsistently
// serialized across object types: most choice fields return a
// {value,label} object, but some (notably dcim.Cable.type) return a
// bare string like "aoc", and others return null. UnmarshalJSON accepts
// all three shapes and stores the choice value, so a struct field never
// fails to parse just because the API picked the string form. The empty
// string means absent/null.
type ChoiceString string

func (cs *ChoiceString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*cs = ""
		return nil
	}
	// Bare string form: "aoc".
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*cs = ChoiceString(s)
		return nil
	}
	// Object form: {"value":"aoc","label":"AOC"}.
	var nv NestedValue
	if err := json.Unmarshal(b, &nv); err != nil {
		return err
	}
	*cs = ChoiceString(nv.Value)
	return nil
}

type NestedTag struct {
	ID   int    `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug"`
}

type Interface struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	Type         *NestedValue    `json:"type,omitempty"`
	MACAddress   string          `json:"mac_address,omitempty"`
	PrimaryMAC   *MACAddressRef  `json:"primary_mac_address,omitempty"`
	MACAddresses []MACAddressRef `json:"mac_addresses,omitempty"`
	Speed        *int            `json:"speed,omitempty"` // kbps
	Enabled      bool            `json:"enabled"`
	Description  string          `json:"description,omitempty"`
	Device       *NestedRef      `json:"device,omitempty"`
	Tags         []NestedTag     `json:"tags,omitempty"`
	// LAG references the parent bond interface when this interface is a
	// bond member. NetBox models bond membership as a child→parent ref
	// (the member's `lag` field points at the bond's interface). Used to
	// render the kworker LACP bond netplan (members matched by MAC). Nil
	// for non-member interfaces (including the bond interface itself).
	LAG *NestedRef `json:"lag,omitempty"`
	// UntaggedVLAN is the interface's native/untagged VLAN (NetBox
	// `untagged_vlan`), carrying the VLAN id (VID). Used by the L2 bond
	// network-config script to record the untagged segment the bond sits
	// on. Nil when the interface has no untagged VLAN assigned.
	UntaggedVLAN *NestedVLAN `json:"untagged_vlan,omitempty"`
	// MTU is the interface MTU as modeled in NetBox. Nil when unset (the
	// network-config script then applies its own per-segment default).
	MTU *int `json:"mtu,omitempty"`
	// MgmtOnly is NetBox's canonical marker for management
	// interfaces — BMCs, IPMI dedicated NICs, lights-out, etc.
	// Slagworks uses this to identify the BMC interface for
	// power-control routing. Naming conventions for these
	// interfaces vary across vendors (ipmi, ipmi0, bmc, mgmt, lom,
	// dedicated), so name-based matching is fragile; mgmt_only is
	// explicit operator intent.
	MgmtOnly bool `json:"mgmt_only,omitempty"`
	// Cable is the cable attached to this interface, nil when uncabled.
	// Commission's LLDP cable reconcile only adds a cable between two
	// uncabled interfaces.
	Cable *NestedRef `json:"cable,omitempty"`
}

// GetMAC returns the interface's MAC address, preferring the primary MAC,
// falling back to the first entry in mac_addresses (NetBox v4.3+).
func (i Interface) GetMAC() string {
	if i.MACAddress != "" {
		return i.MACAddress
	}
	if i.PrimaryMAC != nil && i.PrimaryMAC.MACAddress != "" {
		return i.PrimaryMAC.MACAddress
	}
	if len(i.MACAddresses) > 0 {
		return i.MACAddresses[0].MACAddress
	}
	return ""
}

// ConsolePort is a device's console port (dcim/console-ports). Only the
// fields the snapshot builder reads to resolve the cabled console-server
// endpoint are carried.
type ConsolePort struct {
	ID     int        `json:"id"`
	Name   string     `json:"name"`
	Device *NestedRef `json:"device,omitempty"`
	// Speed is the port's baud rate ({value,label} choice; null when unset).
	Speed *NestedIntValue `json:"speed,omitempty"`
	// ConnectedEndpointsType names the far-end object type of the cable
	// path ("dcim.consoleserverport" for a console server); empty when
	// the port is uncabled.
	ConnectedEndpointsType string `json:"connected_endpoints_type,omitempty"`
	// ConnectedEndpoints is the far end of the cable path. For a
	// console-server endpoint each entry is a console-server port whose
	// Name is the tty node on that device and whose Device is the
	// console server.
	ConnectedEndpoints []ConsolePortEndpoint `json:"connected_endpoints,omitempty"`
	// ConnectedEndpointsReachable is false when the cable path is
	// incomplete; the endpoint is then not trusted.
	ConnectedEndpointsReachable bool `json:"connected_endpoints_reachable,omitempty"`
}

// ConsolePortEndpoint is the compact far-end ref NetBox embeds in a
// console port's connected_endpoints.
type ConsolePortEndpoint struct {
	ID     int        `json:"id"`
	Name   string     `json:"name"`
	Device *NestedRef `json:"device,omitempty"`
}

// NestedIntValue is a choice field whose value is an integer (e.g. a
// console port's speed: {"value":115200,"label":"115.2 kbps"}).
type NestedIntValue struct {
	Value int    `json:"value"`
	Label string `json:"label,omitempty"`
}

type MACAddressRef struct {
	ID         int    `json:"id"`
	MACAddress string `json:"mac_address"`
}

// NestedVLAN is the compact VLAN ref NetBox embeds on an interface's
// untagged_vlan / tagged_vlans. VID is the 802.1Q VLAN id.
type NestedVLAN struct {
	ID   int    `json:"id"`
	VID  int    `json:"vid"`
	Name string `json:"name,omitempty"`
}

type ModuleTypeProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type ModuleType struct {
	ID            int             `json:"id"`
	Manufacturer  *NestedRef      `json:"manufacturer,omitempty"`
	Model         string          `json:"model"`
	Profile       *NestedRef      `json:"profile,omitempty"`
	PartNumber    string          `json:"part_number,omitempty"`
	AttributeData json.RawMessage `json:"attribute_data,omitempty"`
}

type ModuleBay struct {
	ID       int        `json:"id"`
	Name     string     `json:"name"`
	Position string     `json:"position,omitempty"`
	Device   *NestedRef `json:"device,omitempty"`
}

type Module struct {
	ID           int             `json:"id"`
	Device       *NestedRef      `json:"device,omitempty"`
	ModuleBay    *NestedRef      `json:"module_bay,omitempty"`
	ModuleType   *NestedRef      `json:"module_type,omitempty"`
	Serial       string          `json:"serial,omitempty"`
	Status       *NestedValue    `json:"status,omitempty"`
	CustomFields json.RawMessage `json:"custom_fields,omitempty"`
}

type Cable struct {
	ID            int           `json:"id"`
	ATerminations []Termination `json:"a_terminations,omitempty"`
	BTerminations []Termination `json:"b_terminations,omitempty"`
	// Type is a NetBox choice field that, for cables, is serialized as a
	// bare string ("aoc", "cat6") rather than the {value,label} object
	// most choice fields use — and is null when unset. ChoiceString
	// tolerates all three. (Was *NestedValue, which failed to unmarshal
	// the string form and broke commission cable reconcile on any host
	// with a typed fabric cable, e.g. leaf-6's AOC uplinks.)
	Type   ChoiceString `json:"type,omitempty"`
	Status *NestedValue `json:"status,omitempty"`
}

type Termination struct {
	ObjectType string `json:"object_type"`
	ObjectID   int    `json:"object_id"`
}

// PowerPort is a device's power input (a PSU feed). NetBox models the
// chain device → power-port → cable → power-outlet → PDU device. We read
// power-ports with connected_endpoints populated to resolve which PDU
// outlet feeds each PSU. Only the fields PDU power control needs are here.
type PowerPort struct {
	ID     int        `json:"id"`
	Name   string     `json:"name"` // e.g. "PSU1"
	Device *NestedRef `json:"device,omitempty"`
	// ConnectedEndpoints is NetBox's pre-traced far end of the cable
	// path: the power-outlet(s) this PSU feed terminates at, each
	// carrying its parent PDU device. NetBox traces through any
	// intermediate panels/couplers, so this is the actual PDU outlet,
	// not just the next hop.
	ConnectedEndpoints []PowerOutlet `json:"connected_endpoints,omitempty"`
	// ConnectedEndpointsReachable is false when the cable path is
	// incomplete (a dangling cable). We skip unreachable endpoints.
	ConnectedEndpointsReachable bool `json:"connected_endpoints_reachable,omitempty"`
}

// PowerOutlet is one outlet on a PDU. As a connected_endpoint of a
// PowerPort it carries the outlet name ("Outlet 5") and the parent PDU
// device reference; we resolve the PDU's serial + mgmt IP from that ref.
// When fetched directly via /api/dcim/power-outlets/ the
// ConnectedEndpoints field is populated with the downstream power-port(s)
// (PSU feeds) the outlet cables to, mirroring the PowerPort side.
type PowerOutlet struct {
	ID     int        `json:"id"`
	Name   string     `json:"name"` // e.g. "Outlet 5"
	Device *NestedRef `json:"device,omitempty"`
	// ConnectedEndpoints is NetBox's pre-traced far end of this outlet's
	// cable path: the downstream power-port(s) (PSU feeds) it powers, each
	// carrying its parent device. Mirror of PowerPort.ConnectedEndpoints in
	// the outlet->device direction. NetBox traces through intermediate
	// panels/couplers, so this is the actual downstream device's PSU.
	ConnectedEndpoints []PowerPort `json:"connected_endpoints,omitempty"`
	// ConnectedEndpointsReachable is false when the cable path is incomplete
	// (a dangling cable) or the outlet is uncabled.
	ConnectedEndpointsReachable bool `json:"connected_endpoints_reachable,omitempty"`
}

// Location is a NetBox location within a site. NetBox locations nest
// (Parent), so a config scoped to a location claims that location and
// everything beneath it (subtree-inclusive). Site is the owning site;
// the dispatcher's belongs-to check compares it against the config's
// netbox_site to catch a location re-parented to a different site.
type Location struct {
	ID     int        `json:"id"`
	Name   string     `json:"name"`
	Slug   string     `json:"slug"`
	Site   *NestedRef `json:"site,omitempty"`
	Parent *NestedRef `json:"parent,omitempty"`
	Depth  int        `json:"_depth,omitempty"`
}

type Prefix struct {
	ID     int          `json:"id"`
	Prefix string       `json:"prefix"`
	Site   *NestedRef   `json:"site,omitempty"`
	Status *NestedValue `json:"status,omitempty"`
	VLAN   *NestedRef   `json:"vlan,omitempty"`
	// ScopeType/ScopeID are NetBox 4.x's polymorphic prefix scope:
	// ScopeType is "dcim.site" or "dcim.location" and ScopeID the object
	// id. A location-scoped config matches prefixes whose scope is its
	// location; a whole-site config matches scope "dcim.site". The
	// legacy Site field above is no longer populated by NetBox 4.5
	// (scope replaced it) — read scope, not Site, for new code.
	ScopeType    string          `json:"scope_type,omitempty"`
	ScopeID      int             `json:"scope_id,omitempty"`
	CustomFields json.RawMessage `json:"custom_fields,omitempty"`
	Tags         []NestedTag     `json:"tags,omitempty"`
}

type IPAddress struct {
	ID                 int          `json:"id"`
	Address            string       `json:"address"`
	Status             *NestedValue `json:"status,omitempty"`
	AssignedObjectType string       `json:"assigned_object_type,omitempty"`
	AssignedObjectID   *int         `json:"assigned_object_id,omitempty"`
}

// IsActive reports whether the address should be treated as its
// interface's live address. NetBox models future/renumber-target addresses
// as status=reserved (pop3 bring-up: every interface carries its current
// active IP AND its post-renumber reserved IP); feeding a reserved address
// into BMCIP / DHCP reservations / gateway resolution / the AMT endpoint
// points tooling at an address nothing answers on yet. A missing status
// (older fixtures, tests) counts as active — status filtering is opt-in by
// the data, not a new requirement on it.
func (ip IPAddress) IsActive() bool {
	return ip.Status == nil || ip.Status.Value == "active"
}

type IPRange struct {
	ID            int             `json:"id"`
	StartAddress  string          `json:"start_address"`
	EndAddress    string          `json:"end_address"`
	MarkPopulated bool            `json:"mark_utilized"`  // NetBox API field is "mark_utilized"
	Role          *NestedRef      `json:"role,omitempty"` // IPAM role (functional purpose), e.g. slagworks-oob-dynamic ({id,name,slug})
	CustomFields  json.RawMessage `json:"custom_fields,omitempty"`
}

type Tag struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type CustomField struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	Type         json.RawMessage `json:"type"`         // GET returns {value,label}, POST accepts string
	ObjectTypes  []string        `json:"object_types"` // e.g. ["dcim.device"]
	Label        string          `json:"label,omitempty"`
	Description  string          `json:"description,omitempty"`
	Required     bool            `json:"required"`
	SearchWeight int             `json:"search_weight,omitempty"`
}

type Webhook struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	PayloadURL      string `json:"payload_url"`
	HTTPMethod      string `json:"http_method"`
	HTTPContentType string `json:"http_content_type"`
	Secret          string `json:"secret,omitempty"`
	SSLVerification bool   `json:"ssl_verification"`
}

type EventRule struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	ObjectTypes  []string        `json:"object_types"` // e.g. ["dcim.device"]
	EventTypes   []string        `json:"event_types,omitempty"`
	Enabled      bool            `json:"enabled"`
	Conditions   json.RawMessage `json:"conditions,omitempty"`
	ActionType   json.RawMessage `json:"action_type"` // GET returns {value,label}, POST accepts string
	ActionObject *NestedRef      `json:"action_object,omitempty"`
}
