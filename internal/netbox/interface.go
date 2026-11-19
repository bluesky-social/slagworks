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
		// idempotency, so this must skip the row: never a modeling-conflict
		// error and never a field patch. The one write it may make is
		// finishing that prior run's MAC write when it assigned the MAC
		// object but failed before marking it primary (the MAC match
		// below goes through GetMAC, which accepts an assigned non-primary
		// object); ensureInterfaceMAC is a no-op once the primary is set.
		// This check must run BEFORE the byMAC match
		// below: on shared-LOM hardware an unprotected host row can
		// carry the same MAC as the BMC, and byMAC (which excludes
		// protected rows) would resolve to that host row instead,
		// attempting to rename it to the protected name and failing —
		// making the outcome depend on NIC processing order. Checking
		// here first is order-independent.
		if nic.MgmtOnly {
			if protected, exists := findProtectedInterfaceByName(existing, nic.Name); exists &&
				protected.MgmtOnly && nic.MAC != "" && strings.EqualFold(protected.GetMAC(), nic.MAC) {
				if err := c.ensureInterfaceMAC(ctx, protected, nic.MAC); err != nil {
					return fmt.Errorf("setting MAC on interface %q: %w", nic.Name, err)
				}
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

		// Resolve the existing row: MAC first (physical identity),
		// name second (MAC-less rows, virtual interfaces, or a MAC
		// row another NIC already claimed — duplicate MACs happen on
		// bonded/virtual NICs). A row already claimed by an earlier
		// NIC counts as no-match at BOTH stages, so the fallback can
		// still adopt a name row instead of POSTing into NetBox's
		// device+name uniqueness constraint.
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
			match, ok = byMAC[strings.ToLower(nic.MAC)]
			if ok && consumed[match.ID] {
				ok = false
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

		var target Interface // the row nic is reconciled onto
		if ok {
			consumed[match.ID] = true
			target = match
			if match.Name != nic.Name && operatorNames {
				// The PATCH below would carry the kernel name; drop it so
				// only the non-name fields update.
				delete(body, "name")
				c.log.Info("keeping operator-owned interface name",
					"name", match.Name, "commission_name", nic.Name, "mac", nic.MAC, "device_id", deviceID)
			} else if match.Name != nic.Name {
				// A rename can collide with a protected row's name just
				// as easily as a fresh create can (see the POST-branch
				// comment below): protected rows are excluded from
				// byName, so the squatter lookup just below can never
				// see one. Check existing directly first, or the PATCH
				// falls through to NetBox's device+name uniqueness 400
				// instead of the loud modeling-conflict error.
				if protected, exists := findProtectedInterfaceByName(existing, nic.Name); exists {
					return fmt.Errorf("renaming interface %q to %q: name held by protected interface %q (id=%d): modeling conflict, resolve in NetBox",
						match.Name, nic.Name, protected.Name, protected.ID)
				}
				// NetBox enforces device+name uniqueness, so a rename
				// must first evict any other row squatting on the
				// target name (e.g. a MAC-less "eth0" placeholder next
				// to the MAC-matched ens2f0np0 we're renaming). Only
				// MAC-less, unprotected, unclaimed rows are evicted —
				// a MAC-bearing squatter may be a later NIC's match,
				// and anything protected/claimed holding the name is a
				// modeling conflict worth failing loudly on. (The
				// isProtectedInterface(squatter) check just below is now
				// unreachable — protected rows never appear in byName —
				// but stays as defense in depth.)
				if squatter, exists := byName[nic.Name]; exists && squatter.ID != match.ID {
					if consumed[squatter.ID] || isProtectedInterface(squatter) || squatter.GetMAC() != "" {
						return fmt.Errorf("renaming interface %q to %q: name held by interface id=%d (protected, already claimed, or MAC-bearing)",
							match.Name, nic.Name, squatter.ID)
					}
					path := fmt.Sprintf("/api/dcim/interfaces/%d/", squatter.ID)
					if err := c.Delete(ctx, path); err != nil {
						return fmt.Errorf("deleting placeholder interface %q (id=%d) blocking rename: %w",
							nic.Name, squatter.ID, err)
					}
					consumed[squatter.ID] = true // claimed-by-deletion; skip in the stale sweep
					c.log.Info("deleted placeholder interface blocking rename",
						"name", nic.Name, "id", squatter.ID, "device_id", deviceID)
				}
				c.log.Info("renaming interface to commission-observed name",
					"from", match.Name, "to", nic.Name, "mac", nic.MAC, "device_id", deviceID)
			}
			path := fmt.Sprintf("/api/dcim/interfaces/%d/", match.ID)
			var updated Interface
			if err := c.Patch(ctx, path, body, &updated); err != nil {
				return fmt.Errorf("updating interface %q: %w", nic.Name, err)
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
			var created Interface
			if err := c.Post(ctx, "/api/dcim/interfaces/", body, &created); err != nil {
				return fmt.Errorf("creating interface %q: %w", nic.Name, err)
			}
			consumed[created.ID] = true
			target = created
			c.log.Info("created interface", "name", nic.Name, "device_id", deviceID)
		}

		if nic.MAC != "" {
			if err := c.ensureInterfaceMAC(ctx, target, nic.MAC); err != nil {
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

// ensureInterfaceMAC makes wantMAC the primary MAC of iface. NetBox v4.3+
// requires MAC addresses as separate objects assigned to the interface.
// iface is the row as last listed, or as returned by its create (avoids an
// extra API call).
//
// Setting a new MAC takes two writes, creating the MAC object and then
// marking it primary, so a run that fails between them (or loses the
// create response) leaves the object assigned but not primary. The next
// run finds that object in iface.MACAddresses and marks it primary rather
// than creating a duplicate. The check is on the primary, not GetMAC,
// whose fallback to the first assigned object would read that half-done
// state as finished on every later run.
func (c *Client) ensureInterfaceMAC(ctx context.Context, iface Interface, wantMAC string) error {
	if strings.EqualFold(iface.primaryMACAddress(), wantMAC) {
		return nil
	}

	var macID int
	found := false
	for _, m := range iface.MACAddresses {
		if strings.EqualFold(m.MACAddress, wantMAC) {
			macID, found = m.ID, true
			break
		}
	}
	if !found {
		var macObj MACAddressRef
		if err := c.Post(ctx, "/api/dcim/mac-addresses/", map[string]any{
			"mac_address":          wantMAC,
			"assigned_object_type": "dcim.interface",
			"assigned_object_id":   iface.ID,
		}, &macObj); err != nil {
			return fmt.Errorf("creating MAC address: %w", err)
		}
		macID = macObj.ID
	}

	var updated Interface
	path := fmt.Sprintf("/api/dcim/interfaces/%d/", iface.ID)
	if err := c.Patch(ctx, path, map[string]any{
		"primary_mac_address": macID,
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
