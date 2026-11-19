package netbox

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// NICData represents a NIC from commission data, ready for NetBox reconciliation.
type NICData struct {
	Name        string
	MAC         string
	Speed       int // kbps
	Driver      string
	FWVersion   string
	PCIAddress  string
	SRIOVMaxVFs int
	IntfType    string // agent-classified NetBox type slug; overrides speed-based
	MgmtOnly    bool   // true for BMC/IPMI interfaces
	XcvrType    string // transceiver form factor, e.g., "SFP28", "QSFP28"
}

// InterfaceType returns the NetBox interface type value based on speed.
func InterfaceType(speedKbps int) string {
	switch {
	case speedKbps >= 100_000_000: // 100G
		return "100gbase-x-qsfp28"
	case speedKbps >= 50_000_000: // 50G
		return "50gbase-x-sfp28"
	case speedKbps >= 40_000_000: // 40G
		return "40gbase-x-qsfpp"
	case speedKbps >= 25_000_000: // 25G
		return "25gbase-x-sfp28"
	case speedKbps >= 10_000_000: // 10G
		return "10gbase-x-sfpp"
	case speedKbps >= 1_000_000: // 1G
		return "1000base-t"
	default:
		return "other"
	}
}

// BuildInterfaceDescription returns the transceiver form factor (e.g., "SFP28",
// "QSFP28") when present, empty string otherwise.
func BuildInterfaceDescription(nic NICData) string {
	return nic.XcvrType
}

// ReconcileInterfaces updates a device's interfaces to match the commission data.
//
// Identity is the MAC address, not the name: the commission agent runs
// in the PXE initrd where no udev renames NICs, so it reports kernel
// names (eth0/eth1) while NetBox rows created by operators or
// provisioning scripts carry persistent names (ens2f0np0). The MAC is
// the one identity both sides agree on. Matching by MAC means a
// pre-existing row is RENAMED in place — preserving its `lag`
// membership, assigned IPs, and cables — instead of being deleted and
// recreated bare (the failure that stripped kworker bond members' LAG
// links and dropped deployed hosts onto single-NIC DHCP fallback,
// kworker-l56-1.pop1 2026-06-11). MAC-less NICs (and MAC-less existing
// rows) fall back to name matching.
//
// A name can pass from one NIC to another between commissions: a card
// is removed or moved, path-based names shift after a PCIe renumber, or
// ethN probe order changes. So every NIC's row is resolved before
// anything is written, and each name a row must take is freed first
// (planNameMoves): a stale row holding it is deleted, and a row holding
// it that is itself being renamed is moved to a temporary name.
// Refusing such a MAC-bearing holder instead failed every commission of
// the host until NetBox was edited by hand.
//
// Rows matching neither MAC nor name of any observed NIC are deleted,
// except mgmt/BMC interfaces, bonds, and virtual interfaces (including
// "lo") (see the guard below).
//
// Protected rows (isProtectedInterface) are invisible to reconcile's
// matching entirely, not just to the stale-delete sweep: they are
// excluded from both byMAC and byName below, so an observed NIC can
// never MAC-match or name-match its way into claiming, patching, or
// renaming one. Without this, a modeled lag row (Linux bond) sharing
// its member NIC's MAC, or a mgmt_only BMC row sharing a MAC with a
// host NIC on shared-LOM hardware, would be silently overwritten by the
// match loop below — destroying the bond/BMC modeling that da3e1ac's
// stale-delete guard already protects from the sweep. Commission's
// reconcile manages only host-enumerable physical NICs; protected rows
// carry operator intent commission cannot see and must never observe.
// (da3e1ac fixed the DELETE-safety half, this is the
// MATCH-safety half.)
//
// The one place a protected row remains visible is the collision check
// below the match loop: if an observed NIC's name collides with a
// protected row's name and no unprotected candidate exists, NetBox's
// device+name uniqueness constraint would reject a POST with a raw 400
// (the protected row still holds the name in NetBox's DB even though
// it's invisible to our maps) — that case must fail loudly instead.
//
// operatorNames (see interfaceNamesOperatorOwned) switches the device to
// enrich-only mode: matched rows get their non-name fields and MAC
// updated, but reconcile never renames, creates, or deletes a row, and a
// NIC carrying a management row's MAC is skipped. On SN2700 switches the
// commission initrd's kernel names (enp3s0npN, eno1) are not the
// operational names (swpN via the ansible-installed udev rule, eth0 for
// mgmt) that NetBox, the docs, and smart-hands work use; renaming swpN to
// enp3s0npN and creating an eno1 twin of the mgmt eth0 row is what
// commission did to every pop1/pop2 fabric switch (2026-10-23/24, Aug).
func (c *Client) ReconcileInterfaces(ctx context.Context, deviceID int, nics []NICData, operatorNames bool) error {
	existing, err := c.ListDeviceInterfaces(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}

	byName := make(map[string]Interface, len(existing))
	byMAC := make(map[string]Interface, len(existing))
	mgmtMACs := make(map[string]string) // lowercased MAC -> mgmt row name
	for _, iface := range existing {
		if isMgmtInterface(iface) {
			if mac := strings.ToLower(iface.GetMAC()); mac != "" {
				mgmtMACs[mac] = iface.Name
			}
		}
		if isProtectedInterface(iface) {
			continue
		}
		byName[iface.Name] = iface
		if mac := strings.ToLower(iface.GetMAC()); mac != "" {
			byMAC[mac] = iface
		}
	}

	consumed := make(map[int]bool) // existing-row IDs claimed by an observed NIC

	// Resolve every NIC's row before writing anything, so the name moves
	// below are planned over the whole claim set rather than discovered
	// one NIC at a time.
	var writes []ifaceWrite
	for _, nic := range nics {
		desc := BuildInterfaceDescription(nic)
		ifType := nic.IntfType
		if ifType == "" {
			ifType = InterfaceType(nic.Speed)
		}

		body := map[string]any{
			"device":      deviceID,
			"name":        nic.Name,
			"type":        ifType,
			"enabled":     true,
			"mgmt_only":   nic.MgmtOnly,
			"description": desc,
		}
		if nic.Speed > 0 {
			body["speed"] = nic.Speed
		}

		// A mgmt_only NIC matching an existing protected management row
		// by name AND MAC is a rerun of a prior reconcile's own
		// creation (e.g. the synthetic "ipmi" NICData SyncCommissionData
		// emits every run for the BMC MAC) — SyncCommissionData promises
		// idempotency, so this must be a no-op skip, never an error and
		// never a patch. This check must run BEFORE the byMAC match
		// below: on shared-LOM hardware an unprotected host row can
		// carry the same MAC as the BMC, and byMAC (which excludes
		// protected rows) would resolve to that host row instead,
		// attempting to rename it to the protected name and failing —
		// making the outcome depend on NIC processing order. Checking
		// here first is order-independent.
		if nic.MgmtOnly {
			if protected, exists := findProtectedInterfaceByName(existing, nic.Name); exists &&
				protected.MgmtOnly && nic.MAC != "" && strings.EqualFold(protected.GetMAC(), nic.MAC) {
				continue
			}
		}

		// On an operator-named device, a NIC carrying a management row's
		// MAC is that management interface under its commission-env
		// kernel name (SN2700: NetBox "eth0", initrd "eno1"). It must
		// not become a second row with the same MAC. Scoped to
		// operatorNames because on shared-LOM servers the host NIC
		// legitimately shares the BMC row's MAC and must be modeled
		// (TestReconcileInterfaces_ProtectedBMCSurvivesMACCollision).
		if operatorNames && !nic.MgmtOnly && nic.MAC != "" {
			if owner, isMgmtMAC := mgmtMACs[strings.ToLower(nic.MAC)]; isMgmtMAC {
				c.log.Info("skipping NIC: MAC belongs to management interface on operator-named device",
					"nic", nic.Name, "mac", nic.MAC, "mgmt_interface", owner, "device_id", deviceID)
				continue
			}
		}

		// Resolve the existing row: an exact name+MAC match first,
		// then MAC alone (physical identity), then name alone (MAC-less
		// rows, virtual interfaces, or a MAC row another NIC already
		// claimed — duplicate MACs happen on bonded/virtual NICs). A
		// row already claimed by an earlier NIC counts as no-match at
		// every stage, so the fallback can still adopt a name row
		// instead of POSTing into NetBox's device+name uniqueness
		// constraint.
		//
		// The exact match must come first because byMAC holds only one
		// row per MAC. Once two same-MAC NICs have been reconciled,
		// NetBox has a row for each carrying that MAC; a MAC-first
		// lookup could hand NIC eth0 the eth1 row, and the rename
		// below would then refuse eth0's own MAC-bearing row as a
		// squatter, failing every later commission of the host.
		//
		// A mgmt_only NIC never participates in this ordinary matching
		// at all: its only legitimate match is the idempotency check
		// above (its own prior protected row). Letting it fall through
		// to byMAC/byName here would claim an UNPROTECTED host row on
		// the first-ever reconcile of shared-LOM hardware (no protected
		// BMC row exists yet to make the idempotency check fire) and
		// rename that host interface to the BMC's name with
		// mgmt_only=true — the same modeling corruption row protection exists
		// to prevent, from the opposite direction. If the idempotency
		// check didn't match, treat the mgmt NIC as always-new: skip to
		// create, where the protected-name-collision check (below)
		// still guards against colliding with an existing protected row
		// under a changed BMC MAC.
		var match Interface
		var ok bool
		if !nic.MgmtOnly {
			if row, exists := byName[nic.Name]; exists && !consumed[row.ID] &&
				nic.MAC != "" && strings.EqualFold(row.GetMAC(), nic.MAC) {
				match, ok = row, true
			}
			if !ok {
				match, ok = byMAC[strings.ToLower(nic.MAC)]
				if ok && consumed[match.ID] {
					ok = false
				}
			}
			if !ok || nic.MAC == "" {
				match, ok = byName[nic.Name]
				if ok && consumed[match.ID] {
					ok = false
				}
			}
		}

		if !ok && operatorNames {
			// A kernel-named row would sit beside the operator's row for
			// the same port (e.g. enp3s0np8 next to a MAC-less swp8).
			c.log.Info("skipping NIC with no matching row on operator-named device",
				"nic", nic.Name, "mac", nic.MAC, "device_id", deviceID)
			continue
		}

		w := ifaceWrite{nic: nic, body: body, row: match, matched: ok}
		if ok {
			consumed[match.ID] = true
			if match.Name != nic.Name && operatorNames {
				// The PATCH below would carry the kernel name; drop it so
				// only the non-name fields update.
				delete(body, "name")
				c.log.Info("keeping operator-owned interface name",
					"name", match.Name, "commission_name", nic.Name, "mac", nic.MAC, "device_id", deviceID)
			} else if match.Name != nic.Name {
				// A rename can collide with a protected row's name just
				// as easily as a fresh create can (see the create-branch
				// comment below): protected rows are excluded from
				// byName, so planNameMoves can never see one. Check
				// existing directly, or the PATCH falls through to
				// NetBox's device+name uniqueness 400 instead of the loud
				// modeling-conflict error.
				if protected, exists := findProtectedInterfaceByName(existing, nic.Name); exists {
					return fmt.Errorf("renaming interface %q to %q: name held by protected interface %q (id=%d): modeling conflict, resolve in NetBox",
						match.Name, nic.Name, protected.Name, protected.ID)
				}
			}
		} else {
			// No unprotected row matched by MAC or name. Before
			// POSTing, check whether a protected row (excluded from
			// byName above) already holds this exact name — NetBox's
			// device+name uniqueness constraint would otherwise reject
			// the POST with an opaque 400. The mgmt-only idempotency
			// rerun case is handled earlier, before match resolution
			// (order-independent w.r.t. shared-LOM MAC collisions), so
			// any protected-name collision reaching here is a genuine
			// modeling conflict — fail loudly, and never delete or
			// rename the protected row to make room.
			if protected, exists := findProtectedInterfaceByName(existing, nic.Name); exists {
				return fmt.Errorf("interface %q collides with protected interface %q (id=%d): modeling conflict, resolve in NetBox",
					nic.Name, protected.Name, protected.ID)
			}
		}
		writes = append(writes, w)
	}

	evict, aside, err := planNameMoves(writes, byName, consumed, nics)
	if err != nil {
		return err
	}
	for _, row := range evict {
		if err := c.Delete(ctx, fmt.Sprintf("/api/dcim/interfaces/%d/", row.ID)); err != nil {
			return fmt.Errorf("deleting interface %q (id=%d) to free its name for an observed NIC: %w",
				row.Name, row.ID, err)
		}
		consumed[row.ID] = true // claimed-by-deletion; skip in the stale sweep
		c.log.Info("deleted stale interface holding an observed NIC's name",
			"name", row.Name, "id", row.ID, "mac", row.GetMAC(), "device_id", deviceID)
	}
	for _, row := range aside {
		tmp := renameAsideName(row.ID)
		var updated Interface
		if err := c.Patch(ctx, fmt.Sprintf("/api/dcim/interfaces/%d/", row.ID), map[string]any{"name": tmp}, &updated); err != nil {
			return fmt.Errorf("renaming interface %q (id=%d) aside to %q to free its name: %w",
				row.Name, row.ID, tmp, err)
		}
		c.log.Info("renamed interface aside to free its name",
			"name", row.Name, "temporary_name", tmp, "id", row.ID, "device_id", deviceID)
	}

	for _, w := range writes {
		nic := w.nic
		var ifaceID int
		var currentMAC string
		if w.matched {
			ifaceID = w.row.ID
			currentMAC = w.row.GetMAC()
			if w.newName() != "" {
				c.log.Info("renaming interface to commission-observed name",
					"from", w.row.Name, "to", nic.Name, "mac", nic.MAC, "device_id", deviceID)
			}
			var updated Interface
			if err := c.Patch(ctx, fmt.Sprintf("/api/dcim/interfaces/%d/", w.row.ID), w.body, &updated); err != nil {
				return fmt.Errorf("updating interface %q: %w", nic.Name, err)
			}
		} else {
			var created Interface
			if err := c.Post(ctx, "/api/dcim/interfaces/", w.body, &created); err != nil {
				return fmt.Errorf("creating interface %q: %w", nic.Name, err)
			}
			consumed[created.ID] = true
			ifaceID = created.ID
			c.log.Info("created interface", "name", nic.Name, "device_id", deviceID)
		}

		if nic.MAC != "" {
			if err := c.ensureInterfaceMAC(ctx, ifaceID, currentMAC, nic.MAC); err != nil {
				return fmt.Errorf("setting MAC on interface %q: %w", nic.Name, err)
			}
		}
	}

	// Delete rows no observed NIC claimed, but never touch protected
	// interfaces (BMC management, the NetBox-modelled bond): those
	// carry operator intent (BMC IP assignment, bond IP) that
	// commission cannot see. On an operator-named device the row set is
	// operator-owned too: a row the commission env didn't see (fabric
	// port whose driver isn't up, MAC-less template row) still carries
	// IPs and cables.
	if operatorNames {
		return nil
	}
	for name, iface := range byName {
		if consumed[iface.ID] {
			continue
		}
		if isProtectedInterface(iface) {
			continue
		}
		path := fmt.Sprintf("/api/dcim/interfaces/%d/", iface.ID)
		if err := c.Delete(ctx, path); err != nil {
			c.log.Warn("failed to delete stale interface", "name", name, "err", err)
			continue
		}
		c.log.Info("deleted stale interface", "name", name, "device_id", deviceID)
	}

	return nil
}

// ifaceWrite is the write ReconcileInterfaces plans for one observed NIC:
// a PATCH of the existing row it matched, or a POST when it matched none.
type ifaceWrite struct {
	nic     NICData
	body    map[string]any // no "name" key: the row keeps its name
	row     Interface      // the matched row, when matched
	matched bool
}

// newName returns the name this write gives a row that does not hold it
// yet (a rename or a create), or "" when the write takes no new name.
func (w ifaceWrite) newName() string {
	name, ok := w.body["name"].(string)
	if !ok || (w.matched && w.row.Name == name) {
		return ""
	}
	return name
}

func (w ifaceWrite) String() string {
	if w.matched {
		return fmt.Sprintf("renaming interface %q to %q", w.row.Name, w.nic.Name)
	}
	return fmt.Sprintf("creating interface %q", w.nic.Name)
}

// planNameMoves decides, before anything is written, how to free each
// name a planned write takes. NetBox enforces device+name uniqueness, so
// an unprotected row holding the name must give it up first:
//
//   - A row another NIC claimed is renamed away by that NIC's own write
//     (two NICs swapped names, or path-based names shifted after a PCIe
//     renumber). It goes in aside: renamed to a temporary name first, so
//     every write can then take its final name in any order.
//   - A row no NIC claimed is stale: its NIC is gone and the stale sweep
//     would delete it anyway. It goes in evict, deleted first. This
//     covers both a MAC-less placeholder and the row of a removed card
//     whose name the kernel now gives to another NIC.
//   - A row no NIC claimed whose MAC an observed NIC does report is not
//     stale: it shares that MAC with the row the NIC matched instead.
//     Deleting it would drop the cables and IPs modeled on it, so fail.
//
// byName and consumed describe the rows as listed and the rows the
// writes claimed; protected rows are absent from byName, and the
// caller has already failed on a protected row holding a target name.
func planNameMoves(writes []ifaceWrite, byName map[string]Interface, consumed map[int]bool, nics []NICData) (evict, aside []Interface, err error) {
	observedMACs := make(map[string]bool, len(nics))
	for _, nic := range nics {
		// A mgmt_only NIC never matches an unprotected row, so its MAC
		// does not make a row any NIC's match.
		if !nic.MgmtOnly && nic.MAC != "" {
			observedMACs[strings.ToLower(nic.MAC)] = true
		}
	}
	finalName := make(map[int]string, len(writes)) // claimed row id -> name its write leaves it with
	for _, w := range writes {
		if w.matched {
			finalName[w.row.ID] = w.row.Name
			if name := w.newName(); name != "" {
				finalName[w.row.ID] = name
			}
		}
	}

	for _, w := range writes {
		name := w.newName()
		holder, held := byName[name]
		if name == "" || !held {
			continue
		}
		switch mac := strings.ToLower(holder.GetMAC()); {
		case consumed[holder.ID] && finalName[holder.ID] != name:
			aside = append(aside, holder)
		case consumed[holder.ID]:
			return nil, nil, fmt.Errorf("%s: interface id=%d keeps that name for another observed NIC: the commission data reports two NICs named %q, check the agent's NIC inventory",
				w, holder.ID, name)
		case mac != "" && observedMACs[mac]:
			return nil, nil, fmt.Errorf("%s: name held by interface id=%d, which shares MAC %s with another row (an observed NIC reports that MAC, so the row is not stale): remove the duplicate MAC in NetBox",
				w, holder.ID, holder.GetMAC())
		default:
			evict = append(evict, holder)
		}
	}
	return evict, aside, nil
}

// renameAsideName is the temporary name planNameMoves's aside rows hold
// until their own write renames them. It is longer than any kernel
// interface name (IFNAMSIZ allows 15 characters), so no observed NIC
// can want it, and the row id keeps it unique on the device. A run that
// fails between the two renames leaves it behind; the next run
// MAC-matches the row and renames it to its final name.
func renameAsideName(id int) string {
	return fmt.Sprintf("slagworks-rename-%d", id)
}

// findProtectedInterfaceByName scans existing (not byName — protected
// rows are excluded from that map so they're invisible to the match
// loop) for a protected row with the given name. Both the rename and
// create branches of ReconcileInterfaces call this before writing, so a
// name collision with operator-modeled intent fails loudly instead of
// surfacing NetBox's raw device+name uniqueness 400.
func findProtectedInterfaceByName(existing []Interface, name string) (Interface, bool) {
	for _, iface := range existing {
		if iface.Name == name && isProtectedInterface(iface) {
			return iface, true
		}
	}
	return Interface{}, false
}

// isProtectedInterface reports whether a NetBox interface row carries
// operator intent that commission must never delete: mgmt_only
// interfaces (BMC IP assignment), bonds (Type "lag", the kworker bond
// IP), rows named like well-known BMC interfaces, and virtual
// interfaces (Type "virtual", including "lo").
//
// The virtual guard is broader than a name check on "lo" alone because
// commission's inventory (ip_nics) only walks physical NICs by
// construction — it can never observe a virtual row, so an unclaimed
// virtual row is always operator intent, the same class as the
// mgmt/bond guards above, never staleness. Missing this deleted the
// operator-modeled "lo" row (and its identity loopback /32) during
// cpu2.pop3 commission on 2026-07-23, which then failed the subsequent
// deploy's network-config apply with "found 0 /31 leg(s) and loopback
// empty" — the L3 netscripts (100-pop1-l3-kworker, 150-pop3-oob-gw,
// 150-pop3-router, 100-pop3-cpu) hard-require that loopback.
func isProtectedInterface(iface Interface) bool {
	if iface.MgmtOnly {
		return true
	}
	if iface.Type != nil && (iface.Type.Value == "lag" || iface.Type.Value == "virtual") {
		return true
	}
	lname := strings.ToLower(iface.Name)
	if lname == "lo" {
		return true
	}
	return isBMCName(lname) || strings.HasPrefix(lname, "bond")
}

// isMgmtInterface reports whether a row is a management interface: a
// mgmt_only row (BMC, or a switch's host-OS mgmt NIC) or a row named like
// a well-known BMC interface. Its MAC identifies the management NIC, so a
// host NIC carrying that MAC is on the shared OOB segment. Bonds share
// their members' MAC and are deliberately not included.
func isMgmtInterface(iface Interface) bool {
	return iface.MgmtOnly || isBMCName(strings.ToLower(iface.Name))
}

func isBMCName(lname string) bool {
	return strings.HasPrefix(lname, "ipmi") ||
		strings.HasPrefix(lname, "bmc") ||
		strings.HasPrefix(lname, "idrac") ||
		strings.HasPrefix(lname, "ilo") ||
		strings.HasPrefix(lname, "drac")
}

// operatorNamedRoles are the NetBox device roles whose interface rows are
// operator-owned. Device role, not device type: the SN2700 family is
// spread across mellanox-sn2700, mellanox-sn2700-cs2fo and msn2700 types
// (plus mellanox-sn2410), and commission itself has rewritten a switch's
// device_type before (sync.go), while every leaf/spine in pop1-3 carries
// role leaf or spine and servers, routers and oob-gws carry other roles.
var operatorNamedRoles = map[string]bool{
	"leaf":       true,
	"spine":      true,
	"oob-switch": true,
}

// interfaceNamesOperatorOwned reports whether a device with the given
// NetBox role slug keeps its interface names (and row set) under operator
// control during commission; see ReconcileInterfaces.
func interfaceNamesOperatorOwned(roleSlug string) bool {
	return operatorNamedRoles[roleSlug]
}

func (c *Client) ListDeviceInterfaces(ctx context.Context, deviceID int) ([]Interface, error) {
	var results []Interface
	q := url.Values{"device_id": {fmt.Sprint(deviceID)}}
	if err := c.List(ctx, "/api/dcim/interfaces/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// ensureInterfaceMAC creates a MAC address object and sets it as primary on the
// interface. NetBox v4.3+ requires MAC addresses as separate objects.
// currentMAC is the MAC already on the interface (avoids an extra API call).
func (c *Client) ensureInterfaceMAC(ctx context.Context, ifaceID int, currentMAC, wantMAC string) error {
	if strings.EqualFold(currentMAC, wantMAC) {
		return nil
	}

	var macObj MACAddressRef
	if err := c.Post(ctx, "/api/dcim/mac-addresses/", map[string]any{
		"mac_address":          wantMAC,
		"assigned_object_type": "dcim.interface",
		"assigned_object_id":   ifaceID,
	}, &macObj); err != nil {
		return fmt.Errorf("creating MAC address: %w", err)
	}

	var updated Interface
	path := fmt.Sprintf("/api/dcim/interfaces/%d/", ifaceID)
	if err := c.Patch(ctx, path, map[string]any{
		"primary_mac_address": macObj.ID,
	}, &updated); err != nil {
		return fmt.Errorf("setting primary MAC: %w", err)
	}
	return nil
}

// FindInterfaceByName looks up a single interface on a device by name.
func (c *Client) FindInterfaceByName(ctx context.Context, deviceID int, name string) (*Interface, error) {
	var results []Interface
	q := url.Values{"device_id": {fmt.Sprint(deviceID)}, "name": {name}}
	if err := c.List(ctx, "/api/dcim/interfaces/", q, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return &results[0], nil
}
