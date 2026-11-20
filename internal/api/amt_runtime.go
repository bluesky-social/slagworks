package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Transport discriminators accepted on the op-creation `via` field.
const (
	viaBMC = "bmc"
	viaPDU = "pdu"
	viaAMT = "amt"
)

// validVia reports whether v is an accepted `via` value ("" = auto).
func validVia(v string) bool {
	switch v {
	case "", viaBMC, viaPDU, viaAMT:
		return true
	}
	return false
}

// invalidViaMsg is the 400 body for a rejected `via`.
func invalidViaMsg(v string) string {
	return fmt.Sprintf("invalid via %q, must be bmc, pdu, or amt", v)
}

// resolveVia turns the operator's `via` into the concrete transport. An
// explicit value wins. When omitted, the machine's serial-keyed
// bmc-creds-admin entry decides (plan D1): kind=amt selects amt, anything
// else selects bmc. The credential entry is the authoritative statement of
// what kind of OOB the box has, so an AMT box is never driven as IPMI just
// because the operator didn't say --via.
//
// Lookup failures that the BMC resolver will itself report (no NetBox,
// unbound machine, no secret store, secret fetch error, no entry) resolve
// to bmc, which then fails with its specific credNote. Only hard
// store/NetBox errors bubble.
func (s *Server) resolveVia(ctx context.Context, machineID uuid.UUID, via string) (string, error) {
	if via != "" {
		return via, nil
	}
	host, _, err := s.oobAdminEntry(ctx, machineID)
	if err != nil {
		return "", err
	}
	if host != nil && host.Kind == awssecrets.KindAMT {
		return viaAMT, nil
	}
	return viaBMC, nil
}

// oobAdminEntry returns the machine's bmc-creds-admin entry (any kind) and
// its NetBox device, or (nil, dev, nil) when there is no usable entry (no
// NetBox, unbound machine, no secret store, fetch failure, no entry).
func (s *Server) oobAdminEntry(ctx context.Context, machineID uuid.UUID) (*awssecrets.AdminHost, *netbox.Device, error) {
	if s.netbox == nil || s.bmcSecrets == nil {
		return nil, nil, nil
	}
	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve oob kind: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		return nil, nil, nil
	}
	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve oob kind: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev == nil || dev.Serial == "" || dev.Site == nil || dev.Site.Slug == "" {
		return nil, dev, nil
	}
	secret, err := s.bmcSecrets.GetAdmin(ctx, dev.Site.Slug)
	if err != nil {
		s.log.Warn("oob kind: secret fetch failed; treating transport as bmc (the bmc resolver reports the failure)",
			"pop", dev.Site.Slug, "machine_id", machineID, "err", err)
		return nil, dev, nil
	}
	return secret.Lookup(dev.Serial), dev, nil
}

// resolveAMTRuntime resolves the machine's AMT endpoint + digest creds and
// stamps params.AMTRuntime. Every gap is a hard error, like the PDU
// resolver: the transport is amt only because the operator asked for it
// or the credential store says the box is AMT, so a half-resolved envelope
// can only fail later at the site worker with a worse message. As there,
// a gap in the NetBox record or the admin entry is a transportGapError
// (422) and a dependency that could not be asked is a plain error (503).
func (s *Server) resolveAMTRuntime(ctx context.Context, machineID uuid.UUID, params *model.OpParams) error {
	rt, _, err := s.resolveAMTRuntimeWithDevice(ctx, machineID)
	if err != nil {
		return err
	}
	params.AMTRuntime = &rt
	return nil
}

// resolveAMTRuntimeWithDevice is resolveAMTRuntime's core; it also returns
// the NetBox device so the power-status read can route by site without a
// second lookup.
//
//   - Host is the device's NetBox oob_ip with the mask stripped (plan D2:
//     the AMT endpoint IS the device's OOB endpoint; no separate field).
//     The referenced IPAddress must pass netbox.IPAddress.IsActive, the
//     same rule BMCIP/DHCP reservations use, so a reserved renumber target
//     is never dialed.
//   - Username/Password come from the serial-keyed bmc-creds-admin entry,
//     which must be kind=amt — the same single source BMC creds use.
//     slagworks never rotates AMT accounts (bmc_reconcile is IPMI-only
//     and refused for AMT machines).
//
// A nil secret store (dev/test only) leaves the creds empty, matching the
// PDU resolver; the site-worker factory then refuses the empty credential.
func (s *Server) resolveAMTRuntimeWithDevice(ctx context.Context, machineID uuid.UUID) (model.AMTRuntime, *netbox.Device, error) {
	var rt model.AMTRuntime
	if s.netbox == nil {
		return rt, nil, fmt.Errorf("resolve amt runtime: NetBox not configured")
	}
	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return rt, nil, fmt.Errorf("resolve amt runtime: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		return rt, nil, transportGapf("resolve amt runtime: machine %s has no NetBox device, so its AMT endpoint (oob_ip) is unknown", machineID)
	}
	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return rt, nil, fmt.Errorf("resolve amt runtime: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev.OOBIP == nil || dev.OOBIP.Address == "" {
		return rt, dev, transportGapf("resolve amt runtime: device %q has no oob_ip in NetBox; set it to the AMT address before using --via amt", dev.Name)
	}
	ip, err := s.netbox.GetIPAddress(ctx, dev.OOBIP.ID)
	if err != nil {
		return rt, dev, fmt.Errorf("resolve amt runtime: device %q oob_ip: %w", dev.Name, err)
	}
	if !ip.IsActive() {
		return rt, dev, transportGapf("resolve amt runtime: device %q oob_ip %s has status %q, not active; refusing to dial a non-live address", dev.Name, ip.Address, ip.Status.Value)
	}
	rt.Host, _, _ = strings.Cut(ip.Address, "/")

	if s.bmcSecrets == nil {
		return rt, dev, nil
	}
	if dev.Site == nil || dev.Site.Slug == "" {
		return rt, dev, transportGapf("resolve amt runtime: device %q has no site slug, cannot determine which pop's admin secret to read", dev.Name)
	}
	if dev.Serial == "" {
		return rt, dev, transportGapf("resolve amt runtime: device %q has no NetBox serial, and the admin secret is keyed by serial", dev.Name)
	}
	secret, err := s.bmcSecrets.GetAdmin(ctx, dev.Site.Slug)
	if err != nil {
		return rt, dev, fmt.Errorf("resolve amt runtime: fetch admin secret for pop %s: %w", dev.Site.Slug, err)
	}
	h := secret.Lookup(dev.Serial)
	switch {
	case h == nil:
		return rt, dev, transportGapf("resolve amt runtime: pop %s's bmc-creds-admin secret has no entry for serial %q (device %q); add a kind=amt entry", dev.Site.Slug, dev.Serial, dev.Name)
	case h.Kind != awssecrets.KindAMT:
		return rt, dev, transportGapf("resolve amt runtime: the secret entry for serial %q is kind=%q, not amt; this device is not an AMT host (use --via bmc or omit --via)", dev.Serial, h.Kind)
	case h.Username == "" || h.Password == "":
		return rt, dev, transportGapf("resolve amt runtime: the kind=amt secret entry for serial %q has an incomplete credential", dev.Serial)
	}
	rt.Username, rt.Password = h.Username, h.Password
	return rt, dev, nil
}
