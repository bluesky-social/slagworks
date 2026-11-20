package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// resolveBMCRuntime resolves the machine's BMC runtime block and stamps
// it onto params (params.BMCRuntime). Returns a credNote: empty when a
// usable credential was resolved, otherwise a human-readable explanation
// of why the envelope carries no creds (what was checked and what was
// missing).
//
// Every BMC-actuating op (deploy, commission, power) rejects on a
// non-empty credNote at creation time — a credless op can only fail
// later, asynchronously and confusingly, at the site worker, so the
// instant slagctl error with the actual reason is strictly better.
// (Deploy/commission used to soft-fail "for pop bringup before secret
// backfill"; that window is over, and the one observed survival —
// a host already PXE-looping when the op landed — is not a case
// worth designing for.)
//
// Hard failures (NetBox unreachable, machine missing) still bubble as
// errors; the credNote covers only the no-credential outcomes.
func (s *Server) resolveBMCRuntime(ctx context.Context, machineID uuid.UUID, params *model.OpParams) (string, error) {
	rt, _, credNote, err := s.resolveBMCRuntimeWithDevice(ctx, machineID)
	if err != nil {
		return "", err
	}
	params.BMCRuntime = &rt
	return credNote, nil
}

// resolveBMCRuntimeWithDevice is resolveBMCRuntime's core: it builds
// the runtime block and also returns the NetBox device it resolved
// (nil when NetBox is off or the machine is unbound). The power-status
// read path needs the device for its Site.Slug — that's how the RPC
// is routed to the right site worker — without paying a second NetBox
// round-trip on an interactive path.
func (s *Server) resolveBMCRuntimeWithDevice(ctx context.Context, machineID uuid.UUID) (model.BMCRuntime, *netbox.Device, string, error) {
	rt := model.BMCRuntime{ExtraConfig: map[string]any{}}

	if s.netbox == nil {
		// Without NetBox we can't resolve serial. Hand back an empty
		// runtime block; site worker will refuse the op on empty
		// serial. This path is hit only in tests / development
		// configurations — credNote stays empty so power ops aren't
		// rejected at creation (the site-worker factory falls back to
		// snapshot creds, which is how dev/test setups actuate).
		return rt, nil, "", nil
	}

	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return rt, nil, "", fmt.Errorf("resolve bmc runtime: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		// Soft-decommissioned or never-bound machine: no serial path.
		return rt, nil, "machine is not bound to a NetBox device, so neither the chassis serial nor the pop's bmc-creds-admin secret can be consulted", nil
	}
	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return rt, nil, "", fmt.Errorf("resolve bmc runtime: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev != nil {
		rt.ExpectedChassisSerial = dev.Serial
	}

	// The device's serial-keyed kind=bmc entry in
	// pop/<pop>/bmc-creds-admin is the ONLY BMC credential: it supplies
	// the runtime creds and extra_config. The pop slug comes from the
	// device's Site; the lookup key is the device serial.
	pop, serial := "", ""
	if dev != nil {
		serial = dev.Serial
		if dev.Site != nil {
			pop = dev.Site.Slug
		}
	}
	var adminHost *awssecrets.AdminHost
	var secretState string
	switch {
	case s.bmcSecrets == nil:
		secretState = "no secrets backend configured (dev/test mode)"
	case dev == nil || pop == "":
		secretState = "the device has no site in NetBox, so the pop's bmc-creds-admin secret cannot be located"
	default:
		secret, serr := s.bmcSecrets.GetAdmin(ctx, pop)
		if serr != nil {
			s.log.Warn("bmc runtime: admin secret fetch failed",
				"pop", pop, "machine_id", machineID, "err", serr)
		}
		adminHost, secretState = lookupBMCAdminHost(pop, dev, secret, serr)
	}

	if adminHost == nil {
		return rt, dev, bmcCredNote(pop, serial, secretState), nil
	}
	rt.Username, rt.Password = adminHost.Username, adminHost.Password
	if adminHost.ExtraConfig != nil {
		rt.ExtraConfig = adminHost.ExtraConfig
	}
	return rt, dev, "", nil
}

// lookupBMCAdminHost resolves the kind=bmc admin-secret entry for a
// device, returning either the usable host or a human-readable reason
// it isn't usable. Pure given its inputs, so the no-BMC vs bad-creds
// discrimination is unit-testable without a secrets backend.
func lookupBMCAdminHost(pop string, dev *netbox.Device, secret *awssecrets.AdminSecret, fetchErr error) (*awssecrets.AdminHost, string) {
	switch {
	case fetchErr != nil:
		return nil, fmt.Sprintf("reading pop %s's bmc-creds-admin secret failed (%v)", pop, fetchErr)
	case dev.Serial == "":
		return nil, fmt.Sprintf("NetBox has no chassis serial for device %q, and pop %s's bmc-creds-admin secret is keyed by serial", dev.Name, pop)
	}
	host := secret.Lookup(dev.Serial)
	switch {
	case host == nil:
		return nil, fmt.Sprintf("pop %s's bmc-creds-admin secret has no entry for serial %q (device %q)", pop, dev.Serial, dev.Name)
	case host.Kind == awssecrets.KindAMT:
		// An AMT endpoint must never be driven as IPMI/Redfish (plan D1).
		// Reached only on an explicit --via bmc; an omitted --via
		// auto-selects amt for this entry.
		return nil, fmt.Sprintf("the secret entry for serial %q is kind=amt: this device's OOB endpoint is Intel AMT, not an IPMI/Redfish BMC; use --via amt or omit --via", dev.Serial)
	case host.Kind != awssecrets.KindBMC:
		// Kind guard: the secret holds both bmc and pdu entries keyed
		// by serial. A BMC op must never pick up a pdu entry's cred
		// (e.g. if a NetBox serial collision ever pointed here), so we
		// require kind=bmc explicitly rather than trusting serials to
		// be unique across device classes.
		return nil, fmt.Sprintf("the secret entry for serial %q is kind=%q, not bmc — this serial belongs to a different device class", dev.Serial, host.Kind)
	case host.Username == "" || host.Password == "":
		return nil, fmt.Sprintf("the secret entry for serial %q has an incomplete credential", dev.Serial)
	}
	return host, ""
}

// bmcCredNote explains an empty-cred envelope: why the admin entry was
// unusable (secretState) and the fix. The device's kind=bmc entry in
// pop/<pop>/bmc-creds-admin, keyed by its NetBox serial, is the only BMC
// credential slagworks uses, so that entry is always the remedy. An
// unknown pop or serial renders as a placeholder.
func bmcCredNote(pop, serial, secretState string) string {
	adminEntryPath := "pop/<pop>/bmc-creds-admin"
	if pop != "" {
		adminEntryPath = "pop/" + pop + "/bmc-creds-admin"
	}
	key := "the device's NetBox serial"
	if serial != "" {
		key = fmt.Sprintf("its NetBox serial %q", serial)
	}
	return fmt.Sprintf("%s; slagworks authenticates to BMCs only with the device's kind=bmc entry in %s (AWS Secrets Manager), "+
		"so add or correct that entry, keyed by %s", secretState, adminEntryPath, key)
}

// noBMCCredMsg renders the 422 body for an op rejected because no BMC
// credential could be resolved. opKind is the verb shown to the
// operator ("deploy", "commission", "power"); credNote is
// resolveBMCRuntime's explanation of what was checked and the fix.
func noBMCCredMsg(opKind, credNote string) string {
	return fmt.Sprintf("refusing %s: no BMC credential could be resolved for this machine: %s.", opKind, credNote)
}

// transportGapError marks a transport-resolution failure that NetBox or the
// admin secret answered definitively: the device's record rules the
// transport out (no power-ports, an uncabled outlet, no oob_ip, no matching
// credential entry, ...), and asking again changes nothing until someone
// fixes that record. The PDU and AMT resolvers return one for every such
// gap; their other errors mean a dependency could not be asked.
type transportGapError struct{ err error }

func (e *transportGapError) Error() string { return e.err.Error() }
func (e *transportGapError) Unwrap() error { return e.err }

// transportGapf formats a transportGapError.
func transportGapf(format string, args ...any) error {
	return &transportGapError{err: fmt.Errorf(format, args...)}
}

// httpResolveErr converts an error from a transport resolver (resolveVia,
// resolveBMCRuntime, resolvePDURuntime, resolveAMTRuntime) into an echo
// HTTP error. The request is refused before anything is created either
// way. A transportGapError is 422: retrying cannot help until the NetBox
// record or admin entry it names is fixed. Anything else is 503: NetBox,
// the secret store or the database could not be asked (or NetBox is not
// configured), and the operator can retry once it is healthy. The message
// is the resolver's own, which already names the resolver.
func httpResolveErr(err error) error {
	var gap *transportGapError
	if errors.As(err, &gap) {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
}

// netboxMgmtIPForDevice returns the active IP attached to a device's
// mgmt_only interface, or "" if there is no such interface or no active IP
// is assigned. NetBox returns CIDR strings ("10.254.241.9/24"); we
// strip the mask the same way the snapshot builder does.
//
// "mgmt IP" deliberately rather than "BMC IP": the same mgmt_only
// interface is a BMC on a server and an NMC on a PDU, and this helper
// serves both the BMC runtime path and the PDU runtime / device-creds
// paths.
//
// Mirrors the snapshot builder's logic deliberately: we don't share
// code yet because the snapshot builder operates on a batched fetch
// for an entire site, while this endpoint serves a single device
// lookup. Pulling them onto a common helper is a worthwhile follow-up
// once a third caller appears.
func (s *Server) netboxMgmtIPForDevice(ctx context.Context, deviceID int) (string, error) {
	ifaces, err := s.netbox.ListInterfacesForDevices(ctx, []int{deviceID})
	if err != nil {
		return "", err
	}
	mgmtIfaces := make(map[int]bool)
	for _, ni := range ifaces {
		if !ni.MgmtOnly {
			continue
		}
		mgmtIfaces[ni.ID] = true
	}
	if len(mgmtIfaces) == 0 {
		return "", nil
	}
	ifaceIDs := make([]int, 0, len(mgmtIfaces))
	for id := range mgmtIfaces {
		ifaceIDs = append(ifaceIDs, id)
	}
	ips, err := s.netbox.ListIPAddressesForInterfaces(ctx, ifaceIDs)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if ip.Address == "" {
			continue
		}
		if ip.AssignedObjectType != "dcim.interface" {
			continue
		}
		if ip.AssignedObjectID == nil || !mgmtIfaces[*ip.AssignedObjectID] {
			continue
		}
		// Only an active address is live. A renumbered BMC/NMC keeps its
		// deprecated old address on the same interface until cleanup, and
		// returning it dials a dead IP.
		if !ip.IsActive() {
			continue
		}
		if i := strings.Index(ip.Address, "/"); i >= 0 {
			return ip.Address[:i], nil
		}
		return ip.Address, nil
	}
	return "", nil
}
