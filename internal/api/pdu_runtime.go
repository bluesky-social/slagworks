package api

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
)

// outletNameRe matches the NetBox power-outlet naming convention exactly:
// "Outlet <n>" with a 1-based integer. Anchored and fail-closed on
// purpose — a sloppy trailing-digit grab is how you actuate the wrong
// outlet. Verified uniform across the APDU9965 device-type template and
// all live instances (all 24 outlets are "Outlet N", space, no zero-pad).
var outletNameRe = regexp.MustCompile(`^Outlet (\d+)$`)

// parseOutletIndex extracts the 1-based outlet index from a NetBox
// outlet name, or an error if the name doesn't match the exact
// convention. Index 0 is rejected (outlets are 1-based on the APC CLI).
func parseOutletIndex(name string) (int, error) {
	m := outletNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0, fmt.Errorf("outlet name %q does not match expected %q", name, `Outlet <n>`)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("outlet name %q: %w", name, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("outlet name %q: index must be >= 1, got %d", name, n)
	}
	return n, nil
}

// resolvePDURuntime resolves the machine's PDU outlet set + creds and
// stamps a PDURuntime block onto params (params.PDURuntime). It walks the
// device's power-ports → connected outlets → parent PDU devices, resolves
// each PDU's mgmt IP and serial-keyed apc cred.
//
// Failure modes match resolveBMCRuntime's contract:
//
//   - "hard" failures (NetBox unreachable, a power-port that resolves to
//     no usable outlet, an unparseable outlet name, a PDU with no mgmt
//     IP): bubble up. Unlike BMC's soft-fail-on-missing-serial, PDU
//     resolution is only ever invoked on an explicit `--via pdu`, so the
//     operator asked for PDU control specifically — if we can't build a
//     complete outlet set we fail loudly rather than silently degrade to
//     a partial actuation that might cut some feeds and not others.
//
//   - secret-fetch transport error: bubble up for the same reason
//     (operator explicitly wants PDU; a missing cred means the op can't
//     run, so don't paper over it).
//
// Returns an error if the machine has no NetBox device or no power-ports
// (you can't PDU-control a device whose power topology isn't modeled).
func (s *Server) resolvePDURuntime(ctx context.Context, machineID uuid.UUID, params *model.OpParams) error {
	var rt model.PDURuntime

	if s.netbox == nil {
		return fmt.Errorf("resolve pdu runtime: NetBox not configured")
	}

	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return fmt.Errorf("resolve pdu runtime: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		return fmt.Errorf("resolve pdu runtime: machine %s has no NetBox device, cannot resolve PDU outlets", machineID)
	}

	ports, err := s.netbox.ListPowerPortsForDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return fmt.Errorf("resolve pdu runtime: list power-ports for device %d: %w", *m.NetBoxDeviceID, err)
	}
	if len(ports) == 0 {
		return fmt.Errorf("resolve pdu runtime: device %d has no power-ports modeled in NetBox", *m.NetBoxDeviceID)
	}

	// Resolve the pop's admin secret once (it carries the pdu-kind
	// creds keyed by PDU serial). pop slug comes from the target
	// device's site.
	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return fmt.Errorf("resolve pdu runtime: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	// Secret resolution. When a secret reader is configured (production),
	// it is REQUIRED: a device with no resolvable pop slug is a hard
	// error, not a silent credential-less pass. Only a nil reader
	// (dev/test) leaves secret nil and skips cred resolution entirely.
	var secret *awssecrets.AdminSecret
	if s.bmcSecrets != nil {
		if dev == nil || dev.Site == nil || dev.Site.Slug == "" {
			return fmt.Errorf("resolve pdu runtime: device %d has no site slug, cannot determine which pop's admin secret to read", *m.NetBoxDeviceID)
		}
		secret, err = s.bmcSecrets.GetAdmin(ctx, dev.Site.Slug)
		if err != nil {
			return fmt.Errorf("resolve pdu runtime: fetch admin secret for pop %s: %w", dev.Site.Slug, err)
		}
	}

	// Cache PDU device lookups (mgmt IP + serial) so a device with two
	// feeds on the same PDU only hits NetBox once per PDU.
	type pduInfo struct {
		host   string
		serial string
	}
	pduCache := map[int]pduInfo{}

	for _, port := range ports {
		if !port.ConnectedEndpointsReachable {
			return fmt.Errorf("resolve pdu runtime: power-port %q (id %d) has no reachable outlet (dangling cable in NetBox)", port.Name, port.ID)
		}
		if len(port.ConnectedEndpoints) == 0 {
			return fmt.Errorf("resolve pdu runtime: power-port %q (id %d) has no connected outlet", port.Name, port.ID)
		}
		// A PSU feed terminates at exactly one outlet. If NetBox somehow
		// returns more, that's a modeling error we don't want to guess at.
		if len(port.ConnectedEndpoints) > 1 {
			return fmt.Errorf("resolve pdu runtime: power-port %q (id %d) connects to %d outlets, expected 1", port.Name, port.ID, len(port.ConnectedEndpoints))
		}
		outlet := port.ConnectedEndpoints[0]
		if outlet.Device == nil {
			return fmt.Errorf("resolve pdu runtime: outlet %q (id %d) has no parent PDU device", outlet.Name, outlet.ID)
		}
		idx, err := parseOutletIndex(outlet.Name)
		if err != nil {
			return fmt.Errorf("resolve pdu runtime: %w", err)
		}

		pduDevID := outlet.Device.ID
		info, ok := pduCache[pduDevID]
		if !ok {
			pduDev, err := s.netbox.GetDevice(ctx, pduDevID)
			if err != nil {
				return fmt.Errorf("resolve pdu runtime: get PDU device %d: %w", pduDevID, err)
			}
			if pduDev == nil {
				return fmt.Errorf("resolve pdu runtime: PDU device %d not found", pduDevID)
			}
			host, err := s.netboxMgmtIPForDevice(ctx, pduDevID)
			if err != nil {
				return fmt.Errorf("resolve pdu runtime: lookup mgmt IP for PDU %d: %w", pduDevID, err)
			}
			if host == "" {
				return fmt.Errorf("resolve pdu runtime: PDU %q (device %d) has no mgmt IP in NetBox", pduDev.Name, pduDevID)
			}
			info = pduInfo{host: host, serial: pduDev.Serial}
			pduCache[pduDevID] = info
		}

		o := model.PDUOutlet{
			PDUHost: info.host,
			Outlet:  idx,
			Serial:  info.serial,
		}
		// Resolve the apc cred from the serial-keyed admin secret,
		// filtering on kind=pdu (the secret holds both bmc and pdu
		// entries; a pdu op must never pick up a bmc entry's cred).
		//
		// Fail-loud contract: PDU resolution runs only on explicit
		// `--via pdu`, so a configured-but-missing cred is a hard error
		// (fail here with a clear message rather than later with an
		// obscure SSH auth failure). The ONE soft case is "no secret
		// configured at all" (s.bmcSecrets == nil, i.e. dev/test) —
		// matching how the BMC path tolerates a nil secret reader.
		if secret != nil {
			h := secret.Lookup(info.serial)
			switch {
			case info.serial == "":
				return fmt.Errorf("resolve pdu runtime: PDU %q (device %d) has no NetBox serial, cannot resolve its credential", info.host, pduDevID)
			case h == nil:
				return fmt.Errorf("resolve pdu runtime: no admin-secret entry for PDU serial %q (%s)", info.serial, info.host)
			case h.Kind != awssecrets.KindPDU:
				return fmt.Errorf("resolve pdu runtime: admin-secret entry for serial %q is kind %q, expected pdu", info.serial, h.Kind)
			case h.Username == "" || h.Password == "":
				return fmt.Errorf("resolve pdu runtime: admin-secret entry for PDU serial %q has incomplete credential", info.serial)
			}
			o.Username = h.Username
			o.Password = h.Password
		}
		rt.Outlets = append(rt.Outlets, o)
	}

	// Stable order (PDUHost, Outlet) so the envelope is deterministic
	// regardless of NetBox's power-port return order.
	sort.Slice(rt.Outlets, func(i, j int) bool {
		if rt.Outlets[i].PDUHost != rt.Outlets[j].PDUHost {
			return rt.Outlets[i].PDUHost < rt.Outlets[j].PDUHost
		}
		return rt.Outlets[i].Outlet < rt.Outlets[j].Outlet
	})

	params.PDURuntime = &rt
	return nil
}
