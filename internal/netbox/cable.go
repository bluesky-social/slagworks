package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// LLDPNeighbor represents a single LLDP neighbor from commission data.
type LLDPNeighbor struct {
	LocalInterface string // our interface name
	ChassisName    string // remote switch hostname
	ChassisMAC     string // remote chassis (switch system) MAC — diagnostics only; NOT used to resolve the port
	PortName       string // remote port name
}

// ReconcileCables adds cables discovered by LLDP. It is strictly additive:
// it never deletes or modifies an existing cable. siteID is used to prefer
// switches at the same site when resolving LLDP chassis names.
//
// It used to delete every interface cable on the device and recreate them
// from LLDP. LLDP in the commission initrd is not a complete view of the
// wiring: an SN2700's fabric ports are down there, so its LLDP saw only the
// shared OOB management segment, and every leaf<->spine, router<->spine and
// kworker host cable on pop1 leaf-3/4/5 and spine-1/2 was deleted
// (2026-10-23/24; all pop2 fabric switches in Aug). An absent LLDP neighbor
// is no evidence a cable is gone; only an operator removes cables.
//
// A cable is created only when BOTH the local and the resolved remote
// interface are currently uncabled. Neighbors are skipped (and logged) when
// the local interface is a management interface, carries the MAC of a
// management row, or hears more than one LLDP neighbor: the OOB management
// network is an unmanaged L2 segment that floods LLDP, so a mgmt NIC hears
// arbitrary other hosts, which produced physically impossible cables like
// spine-1:eno1 <-> leaf-3:eth0. A remote interface claimed by several local
// interfaces is ambiguous and also skipped. An existing cable that
// disagrees with LLDP is logged and left alone.
func (c *Client) ReconcileCables(ctx context.Context, deviceID int, siteID int, neighbors []LLDPNeighbor) error {
	ifaces, err := c.ListDeviceInterfaces(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	ifaceByName := make(map[string]Interface, len(ifaces))
	mgmtMACs := make(map[string]string) // lowercased MAC -> mgmt row name
	for _, iface := range ifaces {
		ifaceByName[iface.Name] = iface
		if isMgmtInterface(iface) {
			if mac := strings.ToLower(iface.GetMAC()); mac != "" {
				mgmtMACs[mac] = iface.Name
			}
		}
	}
	neighborsPerLocal := make(map[string]int, len(neighbors))
	for _, n := range neighbors {
		neighborsPerLocal[n.LocalInterface]++
	}

	// Every neighbor's outcome is logged so an operator watching the
	// commission can see what wired up and what didn't. A
	// reconcile that resolves zero of N neighbors is almost certainly a
	// systemic resolution bug, so the closing summary makes that obvious.
	c.log.Info("reconciling cables from LLDP", "device_id", deviceID, "neighbors", len(neighbors))
	type candidate struct {
		n      LLDPNeighbor
		local  Interface
		remote *Interface
	}
	var cands []candidate
	claimsPerRemote := make(map[int]int)
	var created, existing, skipped, failed int
	for _, n := range neighbors {
		remoteLabel := n.ChassisName + ":" + n.PortName
		localIface, ok := ifaceByName[n.LocalInterface]
		if !ok {
			c.log.Warn("cable skipped: local interface not on device",
				"local", n.LocalInterface, "remote", remoteLabel)
			failed++
			continue
		}
		if isMgmtInterface(localIface) {
			c.log.Info("cable skipped: local interface is a management interface (shared OOB segment)",
				"local", n.LocalInterface, "remote", remoteLabel)
			skipped++
			continue
		}
		if owner, isMgmtMAC := mgmtMACs[strings.ToLower(localIface.GetMAC())]; isMgmtMAC {
			c.log.Info("cable skipped: local interface carries a management interface's MAC (shared OOB segment)",
				"local", n.LocalInterface, "mgmt_interface", owner, "remote", remoteLabel)
			skipped++
			continue
		}
		if cnt := neighborsPerLocal[n.LocalInterface]; cnt > 1 {
			c.log.Info("cable skipped: local interface hears multiple LLDP neighbors (shared segment)",
				"local", n.LocalInterface, "neighbors", cnt, "remote", remoteLabel)
			skipped++
			continue
		}

		remoteIface, err := c.resolveRemoteInterface(ctx, siteID, n)
		if err != nil {
			c.log.Warn("cable skipped: could not resolve remote interface",
				"local", n.LocalInterface, "remote", remoteLabel, "err", err)
			failed++
			continue
		}
		cands = append(cands, candidate{n: n, local: localIface, remote: remoteIface})
		claimsPerRemote[remoteIface.ID]++
	}

	// Second pass, once every candidate is resolved, so the outcome does not
	// depend on neighbor order (ParseLLDP walks a map). Each local appears at
	// most once (multi-neighbor locals were skipped above); a remote claimed
	// by several locals is ambiguous and gets no cable.
	for _, cand := range cands {
		n, remoteLabel := cand.n, cand.n.ChassisName+":"+cand.n.PortName
		localCable, remoteCable := cableID(cand.local), cableID(*cand.remote)
		if localCable != 0 && localCable == remoteCable {
			c.log.Info("cable already present",
				"local", n.LocalInterface, "remote", remoteLabel, "cable_id", localCable)
			existing++
			continue
		}
		if localCable != 0 || remoteCable != 0 {
			c.log.Warn("existing cable disagrees with LLDP; leaving NetBox unchanged",
				"local", n.LocalInterface, "local_cable_id", localCable,
				"remote", remoteLabel, "remote_iface_id", cand.remote.ID, "remote_cable_id", remoteCable)
			existing++
			continue
		}
		if cnt := claimsPerRemote[cand.remote.ID]; cnt > 1 {
			c.log.Warn("cable skipped: remote interface claimed by multiple local interfaces",
				"local", n.LocalInterface, "remote", remoteLabel, "remote_iface_id", cand.remote.ID, "claims", cnt)
			skipped++
			continue
		}

		body := map[string]any{
			"a_terminations": []map[string]any{
				{"object_type": "dcim.interface", "object_id": cand.local.ID},
			},
			"b_terminations": []map[string]any{
				{"object_type": "dcim.interface", "object_id": cand.remote.ID},
			},
			"status": "connected",
		}

		var cable Cable
		if err := c.Post(ctx, "/api/dcim/cables/", body, &cable); err != nil {
			c.log.Warn("cable create failed",
				"local", n.LocalInterface, "remote", remoteLabel,
				"remote_iface_id", cand.remote.ID, "err", err)
			failed++
			continue
		}
		created++
		c.log.Info("cable created",
			"local", n.LocalInterface, "remote", remoteLabel,
			"remote_iface_id", cand.remote.ID)
	}

	c.log.Info("cable reconcile complete",
		"device_id", deviceID, "created", created, "existing", existing,
		"skipped", skipped, "failed", failed, "neighbors", len(neighbors))
	return nil
}

// cableID returns the ID of the cable attached to iface, or 0 if uncabled.
func cableID(iface Interface) int {
	if iface.Cable == nil {
		return 0
	}
	return iface.Cable.ID
}

func (c *Client) resolveRemoteInterface(ctx context.Context, siteID int, n LLDPNeighbor) (*Interface, error) {
	// Resolve the remote SWITCH by its LLDP chassis name, then its PORT by
	// the LLDP port name on that switch. We deliberately do NOT fall back to
	// the chassis MAC: the LLDP chassis MAC is the switch's *system* MAC,
	// which in NetBox is assigned to the switch's management interface
	// (eth0), NOT the swp host-fabric port the cable actually lands on. A
	// MAC fallback therefore resolves every host cable to leaf:eth0 — a
	// physically impossible host-fabric link. The port name from
	// LLDP is the only correct source for the remote interface.
	if n.ChassisName == "" {
		return nil, fmt.Errorf("LLDP neighbor has no chassis name (local=%s)", n.LocalInterface)
	}

	// Prefer a same-site match, then fall back to a global search.
	device, err := c.findDeviceByAnyName(ctx, n.ChassisName, siteID)
	if err != nil {
		return nil, err
	}
	if device == nil {
		device, err = c.findDeviceByAnyName(ctx, n.ChassisName, 0)
		if err != nil {
			return nil, err
		}
	}
	if device == nil {
		return nil, fmt.Errorf("remote switch %q not found in NetBox", n.ChassisName)
	}

	iface, err := c.findInterfaceOnDevice(ctx, device.ID, n.PortName)
	if err != nil {
		return nil, err
	}
	if iface == nil {
		return nil, fmt.Errorf("port %q not found on switch %q (device_id=%d)",
			n.PortName, n.ChassisName, device.ID)
	}
	return iface, nil
}

// findDeviceByAnyName resolves a device by its NetBox name, tolerating the
// FQDN/short-name mismatch between LLDP and NetBox. LLDP reports the remote
// switch's chassis name as an FQDN (e.g. "leaf-5.internal.pop1.example.net")
// while the NetBox device is named with the short form ("leaf-5.pop1"), so an
// exact name= match on the FQDN finds nothing. We try the name as
// given first, then retry on the leading dotted label ("leaf-5") matched as a
// prefix (name__isw), picking the same-site / shortest-name candidate.
func (c *Client) findDeviceByAnyName(ctx context.Context, name string, siteID int) (*Device, error) {
	exact, err := c.queryDevices(ctx, url.Values{"name": {name}}, siteID)
	if err != nil {
		return nil, err
	}
	if len(exact) > 0 {
		return &exact[0], nil
	}

	// FQDN fallback: match on the leading label as a name prefix. LLDP's
	// "leaf-5.internal.pop1.example.net" and NetBox's "leaf-5.pop1" share
	// the "leaf-5" leading label.
	label, _, found := strings.Cut(name, ".")
	if !found || label == "" || label == name {
		return nil, nil
	}
	cands, err := c.queryDevices(ctx, url.Values{"name__isw": {label + "."}}, siteID)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	// Prefer the shortest matching name to avoid latching onto a longer
	// device that merely shares the prefix (e.g. "leaf-5.pop1" over a
	// hypothetical "leaf-5x.pop1").
	best := 0
	for i := 1; i < len(cands); i++ {
		if len(cands[i].Name) < len(cands[best].Name) {
			best = i
		}
	}
	return &cands[best], nil
}

func (c *Client) queryDevices(ctx context.Context, q url.Values, siteID int) ([]Device, error) {
	if siteID > 0 {
		q.Set("site_id", fmt.Sprint(siteID))
	}
	var results []Device
	if err := c.List(ctx, "/api/dcim/devices/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (c *Client) findInterfaceOnDevice(ctx context.Context, deviceID int, portName string) (*Interface, error) {
	var results []Interface
	q := url.Values{"device_id": {fmt.Sprint(deviceID)}, "name": {portName}}
	if err := c.List(ctx, "/api/dcim/interfaces/", q, &results); err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return &results[0], nil
	}
	return nil, nil
}

// ParseLLDP extracts LLDP neighbors from the raw lldpctl JSON output.
func ParseLLDP(raw json.RawMessage) []LLDPNeighbor {
	if len(raw) == 0 {
		return nil
	}

	// lldpctl -f json output structure:
	// {"lldp": {"interface": {"eth0": {"chassis": {...}, "port": {...}}, ...}}}
	// or with multiple neighbors per interface:
	// {"lldp": {"interface": [{"eth0": {...}}, {"eth1": {...}}]}}
	var doc struct {
		LLDP struct {
			Interface json.RawMessage `json:"interface"`
		} `json:"lldp"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}

	var neighbors []LLDPNeighbor

	// Try as object first (single or named interfaces)
	var ifaceMap map[string]json.RawMessage
	if json.Unmarshal(doc.LLDP.Interface, &ifaceMap) == nil {
		for name, data := range ifaceMap {
			if n := parseLLDPInterface(name, data); n != nil {
				neighbors = append(neighbors, *n)
			}
		}
		return neighbors
	}

	// Try as array (multiple interface objects)
	var ifaceArray []json.RawMessage
	if json.Unmarshal(doc.LLDP.Interface, &ifaceArray) == nil {
		for _, item := range ifaceArray {
			var m map[string]json.RawMessage
			if json.Unmarshal(item, &m) == nil {
				for name, data := range m {
					if n := parseLLDPInterface(name, data); n != nil {
						neighbors = append(neighbors, *n)
					}
				}
			}
		}
	}

	return neighbors
}

func parseLLDPInterface(localName string, data json.RawMessage) *LLDPNeighbor {
	var info struct {
		Chassis map[string]json.RawMessage `json:"chassis"`
		Port    struct {
			ID struct {
				Value string `json:"value"`
			} `json:"id"`
			Descr string `json:"descr"`
		} `json:"port"`
	}
	if json.Unmarshal(data, &info) != nil {
		return nil
	}

	n := &LLDPNeighbor{
		LocalInterface: localName,
	}

	// Port name: prefer descr, fall back to id.value
	n.PortName = info.Port.Descr
	if n.PortName == "" {
		n.PortName = info.Port.ID.Value
	}

	// Chassis: the key is the chassis name, value has id subtype
	for name, raw := range info.Chassis {
		n.ChassisName = name
		var chassisInfo struct {
			ID struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"id"`
		}
		if json.Unmarshal(raw, &chassisInfo) == nil {
			if strings.EqualFold(chassisInfo.ID.Type, "mac") {
				n.ChassisMAC = chassisInfo.ID.Value
			}
		}
		break // only one chassis per neighbor
	}

	if n.PortName == "" {
		return nil
	}
	return n
}
