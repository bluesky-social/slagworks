package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot"
)

// PreflightResult is the outcome of an op-creation preflight: OK when the
// host's modeled config carries everything the boot needs, otherwise one
// gap string per missing precondition.
type PreflightResult struct {
	Machine uuid.UUID `json:"machine"`
	OpType  string    `json:"op_type"`
	OK      bool      `json:"ok"`
	Gaps    []string  `json:"gaps,omitempty"`
}

// preflightMachine validates that the NetBox/Postgres model carries
// everything the boot for opType requires, BEFORE any op is persisted or
// any PXE/power work happens. It runs the SAME gather+Build the boot path
// uses (via the read-only SnapshotBuilder) and inspects the resulting
// MachineSnapshot, so it cannot pass while the real boot fails.
//
// Returns (result, nil) on a completed check — result.OK distinguishes
// pass from fail; result.Gaps lists what to fix. A non-nil error means the
// preflight itself could not run (NetBox unreachable, machine missing,
// device unclaimed) — distinct from "the check ran and found gaps".
//
// When the server has no SnapshotBuilder wired (NetBox-off / unit-test
// configurations), preflight is a no-op pass: there is nothing to validate
// against, and the existing late hard-fails still backstop the boot.
//
// Test coverage: the validation LOGIC is unit-tested exhaustively in
// snapshot.PreflightMachine (every gap + the fully-modeled pass), and the
// read-only build reuse in TestDispatcherBuildSnapshotIsReadOnly. This
// method is I/O orchestration over the concrete *store.Store and
// *netbox.Client (which the whole Server is built on as concrete types, not
// interfaces), so exercising it directly would need a live Postgres + NetBox.
// The pure-function + dispatcher tests cover the behavior that can drift;
// the glue here is straight-line fetch-and-delegate.
func (s *Server) preflightMachine(ctx context.Context, machineID uuid.UUID, opType model.OperationType) (PreflightResult, error) {
	res := PreflightResult{Machine: machineID, OpType: string(opType)}

	if s.snapshotBuilder == nil || s.netbox == nil {
		res.OK = true
		return res, nil
	}

	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return res, fmt.Errorf("preflight: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		// Unbound machine: no device to validate against. This is itself a
		// terminal gap — the boot path has no NetBox identity to act on.
		res.OK = false
		res.Gaps = []string{"the machine is not bound to a NetBox device; bind it before deploy/commission"}
		return res, nil
	}

	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return res, fmt.Errorf("preflight: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev == nil {
		return res, fmt.Errorf("preflight: netbox device %d not found", *m.NetBoxDeviceID)
	}

	// Resolve which site_config (worker identity) claims this device. The
	// snapshot is per-config, so we must build the right one. This is the
	// same predicate the dispatcher + reconciler use, so the snapshot we
	// inspect is exactly the one the owning worker would receive.
	slug, err := snapshot.ResolveOwningConfigSlug(ctx, s.netbox, s.store, *dev)
	if err != nil {
		if errors.Is(err, snapshot.ErrNoOwningConfig) {
			res.OK = false
			res.Gaps = []string{fmt.Sprintf("no site_config claims NetBox device %q (site %s); the host is in no managed site/location scope, so no worker would ever boot it", dev.Name, siteSlug(dev))}
			return res, nil
		}
		return res, fmt.Errorf("preflight: resolve owning config for device %d: %w", dev.ID, err)
	}

	snap, err := s.snapshotBuilder.BuildSnapshot(ctx, slug)
	if err != nil {
		return res, fmt.Errorf("preflight: build snapshot for site %s: %w", slug, err)
	}

	// Find this machine in the built snapshot. nil ms means the device
	// dropped out of scope between resolve and build (or a build edge case);
	// PreflightMachine reports it as a terminal gap.
	var ms *model.MachineSnapshot
	for i := range snap.Machines {
		if snap.Machines[i].ID == machineID {
			ms = &snap.Machines[i]
			break
		}
	}

	// Whether ANY script of each class is configured site-wide, to
	// disambiguate "no script matched because none exist" (plain-DHCP /
	// default-layout intent, OK) from "scripts exist but none matched this
	// host" (a real modeling gap). Build already ran the authoritative
	// match; these counts only classify a nil result.
	netScripts, err := s.store.ListNetworkScripts()
	if err != nil {
		return res, fmt.Errorf("preflight: list network scripts: %w", err)
	}
	storScripts, err := s.store.ListStorageScripts()
	if err != nil {
		return res, fmt.Errorf("preflight: list storage scripts: %w", err)
	}

	// AWS-secret presence for the device's pop. Probe only when a secrets
	// backend is wired; leave the pointers nil otherwise so PreflightMachine
	// skips the checks (local-dev / NetBox-off keep working). A probe ERROR
	// (auth/network, not "absent") fails the whole preflight — we'd rather
	// surface "couldn't verify secrets" than silently skip and let the op hit
	// the late failure. dev.Site.Slug is the pop.
	adminPresent, monitorPresent, err := s.preflightSecretPresence(ctx, dev)
	if err != nil {
		return res, err
	}

	gaps := snapshot.PreflightMachine(snapshot.PreflightInput{
		Machine:              ms,
		Machines:             snap.Machines,
		Device:               dev,
		Images:               snap.Images,
		HasNetworkScripts:    len(netScripts) > 0,
		HasStorageScripts:    len(storScripts) > 0,
		AdminSecretPresent:   adminPresent,
		MonitorSecretPresent: monitorPresent,
	}, opType)

	res.Gaps = gaps
	res.OK = len(gaps) == 0
	return res, nil
}

// preflightSecretPresence probes whether the device's pop has its BMC admin
// and monitor secrets in AWS Secrets Manager. Returns (admin, monitor) as
// *bool: nil when no secrets backend is wired (skip the check), else a
// non-nil pointer to the presence result. A probe error (auth/network, NOT
// "secret absent") is returned so the caller fails the preflight rather than
// silently skipping. The pop is dev.Site.Slug.
func (s *Server) preflightSecretPresence(ctx context.Context, dev *netbox.Device) (admin, monitor *bool, err error) {
	if s.bmcSecrets == nil {
		// No secrets backend (local-dev / NetBox-off): can't probe; skip.
		return nil, nil, nil
	}
	pop := siteSlug(dev)
	if dev.Site == nil || dev.Site.Slug == "" {
		// No pop to key the secret name on. The device-has-no-site case is
		// already a terminal gap elsewhere (owning-config resolution), so don't
		// double-report; just skip the secret probe.
		return nil, nil, nil
	}
	a, err := s.bmcSecrets.AdminSecretExists(ctx, pop)
	if err != nil {
		return nil, nil, fmt.Errorf("preflight: check admin secret for pop %s: %w", pop, err)
	}
	m, err := s.bmcSecrets.MonitorSecretExists(ctx, pop)
	if err != nil {
		return nil, nil, fmt.Errorf("preflight: check monitor secret for pop %s: %w", pop, err)
	}
	return &a, &m, nil
}

// preflightMachineHandler answers GET /machines/:id/preflight: run the
// op-creation preflight read-only and return the result. The op kind
// defaults to deploy; ?op=commission selects the commission variant.
// Always 200 on a completed check (the body's `ok` field carries
// pass/fail); 4xx/5xx only when the check itself couldn't run.
func (s *Server) preflightMachineHandler(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	var opType model.OperationType
	switch c.QueryParam("op") {
	case "", "deploy":
		opType = model.OpDeploy
	case "commission":
		opType = model.OpCommission
	default:
		return echo.NewHTTPError(http.StatusBadRequest,
			"op must be 'deploy' or 'commission'")
	}
	res, err := s.preflightMachine(c.Request().Context(), id, opType)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "preflight: "+err.Error())
	}
	return c.JSON(http.StatusOK, res)
}

// siteSlug returns a device's NetBox site slug, or "<no site>" for
// diagnostics.
func siteSlug(dev *netbox.Device) string {
	if dev.Site == nil || dev.Site.Slug == "" {
		return "<no site>"
	}
	return dev.Site.Slug
}

// preflightGapMsg renders the 422 body for a deploy/commission rejected by
// preflight, listing every gap. The leading line names the host + verb so
// the operator sees what was refused; each gap is a "- <what>; <fix>" line.
//
// A locked machine is refused for the lock, not for its model, so that case
// leads with the lock and the command that lifts it; any modeling gaps
// follow under their own heading.
func preflightGapMsg(opKind, host string, gaps []string) string {
	var b strings.Builder
	if i := slices.Index(gaps, snapshot.GapMachineLocked); i >= 0 {
		gaps = slices.Delete(slices.Clone(gaps), i, i+1)
		fmt.Fprintf(&b, "refusing %s: %s is locked (a machine is locked automatically when it enters service). Check that it is safe to %s it, then run `slagctl machine unlock %s` and retry.", opKind, host, opKind, host)
		if len(gaps) == 0 {
			return b.String()
		}
		fmt.Fprintf(&b, "\nIt also has an incomplete NetBox model (%d issue(s)):", len(gaps))
	} else {
		fmt.Fprintf(&b, "refusing %s: %s has an incomplete NetBox model (%d issue(s)):", opKind, host, len(gaps))
	}
	for _, g := range gaps {
		b.WriteString("\n  - ")
		b.WriteString(g)
	}
	return b.String()
}
