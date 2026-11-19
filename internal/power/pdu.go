package power

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// PDUOutletSpec describes one switched PDU outlet feeding one of a
// machine's PSUs: which PDU (OOB mgmt IP), which 1-based outlet index,
// and the NMC credential. A redundant-PSU device has two specs, usually
// on two different PDUs. This is the power-package-local shape; the
// worker maps its envelope (worker.PDUOutlet) onto it.
type PDUOutletSpec struct {
	Host     string
	Outlet   int
	Username string
	Password string
}

// pduConn is a connection to a single PDU (one host + cred). Defined as
// an interface so the controller's multi-feed orchestration is testable
// with a fake, exactly as bmcClient makes BMCController testable.
//
//   - cli runs one APC NMC CLI command over SSH (the only actuation
//     transport this firmware exposes — Redfish has no atomic outlet
//     cycle). Returns an error on transport failure OR an APC error
//     code (the NMC reports E000 on success, E1xx on failure; cli
//     parses that and turns a non-E000 into an error).
//   - outletState reads one outlet's switched state via Redfish
//     (Oem.SwitchedState), the clean structured status surface.
type pduConn interface {
	cli(ctx context.Context, command string) (string, error)
	outletState(ctx context.Context, outlet int) (State, error)
	close() error
}

// pduConnFactory builds a connection to a PDU host with a credential.
// Production uses newSSHRedfishConn; tests inject a fake.
type pduConnFactory func(host, user, pass string) pduConn

// outletConn pairs an outlet index with its PDU connection.
type outletConn struct {
	host   string
	outlet int
	conn   pduConn
}

// PDUController controls power for a (possibly multi-feed) machine via
// its switched PDU outlets. Not safe for concurrent use.
//
// Actuation is the APC NMC CLI over SSH; status is Redfish. PowerCycle
// uses the firmware-atomic olDlyReboot (never a controller-composed
// off+on) so a device carrying our own OOB control path can't be
// stranded mid-cycle. See docs/dev/pdu-power-control.md.
type PDUController struct {
	outlets        []outletConn
	connFactory    pduConnFactory
	rebootDuration int           // olRbootTime value (seconds) set before each atomic cycle
	pollInterval   time.Duration // Redfish poll cadence while waiting for recovery
	pollTimeout    time.Duration // max wait for a cycled outlet to read On again
	log            *slog.Logger
}

// PDUOption configures a PDUController.
type PDUOption func(*PDUController)

// WithPDUConnFactory injects the connection factory (tests use this to
// supply a fake transport).
func WithPDUConnFactory(f pduConnFactory) PDUOption {
	return func(p *PDUController) { p.connFactory = f }
}

// WithRebootDuration overrides the per-outlet off-window (seconds) used
// for PowerCycle. Default 60s — long enough that dual-feed dispatch skew
// is irrelevant (validated live: 60s gave ~56s overlap at 3.4s skew).
func WithRebootDuration(seconds int) PDUOption {
	return func(p *PDUController) { p.rebootDuration = seconds }
}

// WithPollTimeout overrides how long PowerCycle waits for cycled outlets
// to read On again before giving up.
func WithPollTimeout(d time.Duration) PDUOption {
	return func(p *PDUController) { p.pollTimeout = d }
}

// NewPDUController builds a controller over one or more outlet specs.
// Returns an error if no specs are given or any spec is malformed.
func NewPDUController(_ context.Context, specs []PDUOutletSpec, log *slog.Logger, opts ...PDUOption) (*PDUController, error) {
	if len(specs) == 0 {
		return nil, errors.New("pdu: no outlets given")
	}
	if log == nil {
		log = slog.Default()
	}
	p := &PDUController{
		rebootDuration: 60,
		pollInterval:   3 * time.Second,
		pollTimeout:    150 * time.Second,
		log:            log,
	}
	p.connFactory = newSSHRedfishConn
	for _, o := range opts {
		o(p)
	}
	for _, s := range specs {
		if s.Host == "" {
			return nil, fmt.Errorf("pdu: outlet spec has empty host")
		}
		if s.Outlet < 1 {
			return nil, fmt.Errorf("pdu %s: outlet must be >= 1, got %d", s.Host, s.Outlet)
		}
		p.outlets = append(p.outlets, outletConn{
			host:   s.Host,
			outlet: s.Outlet,
			conn:   p.connFactory(s.Host, s.Username, s.Password),
		})
	}
	return p, nil
}

// PowerOff turns ALL feeds off. On a partial failure (one feed off, one
// errored) it ROLLS BACK — issues olOn to every feed it cut — so a
// failed PowerOff returns the box to its known-good fully-redundant
// state rather than stranding it half-off. The rollback action only ever
// powers toward up, so it is itself safe. See doc §D.
func (p *PDUController) PowerOff(ctx context.Context) error {
	var cut []outletConn
	for _, oc := range p.outlets {
		if _, err := oc.conn.cli(ctx, fmt.Sprintf("olOff %d", oc.outlet)); err != nil {
			// Roll back the feeds we already cut, then report. Use a
			// context DETACHED from ctx's cancellation: if the PowerOff
			// failed *because* ctx was cancelled/timed out, reusing ctx
			// would make the rollback no-op instantly — defeating the
			// rollback exactly when it's most needed. We still bound it
			// with a fresh timeout so a wedged rollback can't hang.
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
			rbErr := p.rollbackOn(rbCtx, cut)
			rbCancel()
			if rbErr != nil {
				return fmt.Errorf("pdu power off %s outlet %d: %w; AND rollback failed: %v (box may be running on partial feeds)",
					oc.host, oc.outlet, err, rbErr)
			}
			return fmt.Errorf("pdu power off %s outlet %d: %w (rolled back %d already-cut feed(s) to restore redundancy)",
				oc.host, oc.outlet, err, len(cut))
		}
		cut = append(cut, oc)
	}
	return nil
}

// rollbackOn issues olOn to each given outlet, restoring power. Used by
// PowerOff on partial failure. Aggregates any errors; the box only ever
// ends up more-powered, never darker, so this is the safe direction.
func (p *PDUController) rollbackOn(ctx context.Context, outlets []outletConn) error {
	var errs []error
	for _, oc := range outlets {
		if _, err := oc.conn.cli(ctx, fmt.Sprintf("olOn %d", oc.outlet)); err != nil {
			errs = append(errs, fmt.Errorf("%s outlet %d: %w", oc.host, oc.outlet, err))
		}
	}
	return errors.Join(errs...)
}

// PowerOn turns ALL feeds on, best-effort: it attempts every outlet even
// if an earlier one errors (stopping early would defeat redundancy by
// leaving a reachable feed un-powered), then reports the aggregate. The
// op fails if any feed failed to come on. See doc §D.
func (p *PDUController) PowerOn(ctx context.Context) error {
	var errs []error
	for _, oc := range p.outlets {
		if _, err := oc.conn.cli(ctx, fmt.Sprintf("olOn %d", oc.outlet)); err != nil {
			errs = append(errs, fmt.Errorf("%s outlet %d: %w", oc.host, oc.outlet, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pdu power on: %w", errors.Join(errs...))
	}
	return nil
}

// PowerCycle performs the firmware-atomic delayed reboot on ALL feeds,
// then BLOCKS until the box is back. For each outlet: set the off-window
// (olRbootTime), then dispatch olDlyReboot. With a long RebootDuration
// (default 60s) the per-outlet off-windows overlap massively even with
// multi-second dispatch skew between PDUs, so a redundant box loses both
// feeds simultaneously -> genuine cold restart.
//
// CRITICAL: olDlyReboot returns immediately (the cycle is async in
// firmware); the outlet stays off for RebootDuration after that. So we
// must (1) wait past the off-window, then (2) poll Redfish until every
// outlet reads On again, before returning success — otherwise the
// executor would proceed to post-op steps while the box is still
// hard-down. We never compose the cycle from off+on ourselves: that
// two-phase shape could strand a device that carries our own control
// path. See doc §D.
func (p *PDUController) PowerCycle(ctx context.Context) error {
	// Phase 1: set duration + dispatch the atomic cycle on every feed,
	// back-to-back. Dispatch is sequential (N<=2, nothing to
	// parallelize); the long RebootDuration absorbs the skew.
	for _, oc := range p.outlets {
		if _, err := oc.conn.cli(ctx, fmt.Sprintf("olRbootTime %d %d", oc.outlet, p.rebootDuration)); err != nil {
			return fmt.Errorf("pdu cycle %s outlet %d: set reboot duration: %w", oc.host, oc.outlet, err)
		}
		if _, err := oc.conn.cli(ctx, fmt.Sprintf("olDlyReboot %d", oc.outlet)); err != nil {
			return fmt.Errorf("pdu cycle %s outlet %d: dispatch olDlyReboot: %w", oc.host, oc.outlet, err)
		}
		p.log.Info("pdu atomic cycle dispatched", "host", oc.host, "outlet", oc.outlet, "off_window_s", p.rebootDuration)
	}

	// Phase 2: wait past the off-window, THEN poll for recovery. Polling
	// immediately would match the pre-cut On and falsely return at once,
	// so we sleep the full off-window first (the firmware holds the
	// outlet off for exactly that long after dispatch).
	timer := time.NewTimer(time.Duration(p.rebootDuration) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return p.waitAllOn(ctx)
}

// waitAllOn polls Redfish until every outlet reads On, or the poll
// timeout expires. Read-only Redfish (cheap, no SSH-slot contention).
func (p *PDUController) waitAllOn(ctx context.Context) error {
	deadline := time.Now().Add(p.pollTimeout)
	for {
		allOn := true
		var lastErr error
		for _, oc := range p.outlets {
			st, err := oc.conn.outletState(ctx, oc.outlet)
			if err != nil {
				lastErr = fmt.Errorf("%s outlet %d: %w", oc.host, oc.outlet, err)
				allOn = false
				continue
			}
			if st != StateOn {
				allOn = false
			}
		}
		if allOn {
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("pdu cycle: timed out waiting for outlets to power back on: last error: %w", lastErr)
			}
			return fmt.Errorf("pdu cycle: timed out after %s waiting for outlets to read On", p.pollTimeout)
		}
		timer := time.NewTimer(p.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// PowerStatus aggregates per-outlet Redfish state. Any confirmed-On feed
// pins StateOn (a dual-fed box is powered as long as one feed is live;
// reporting Off while a feed is confirmed up would be a dangerous lie).
// StateOff only when every outlet read Off. With no On confirmed, a read
// error gives StateUnknown plus that error, and an unrecognized outlet
// state gives StateUnknown. See doc §D.
func (p *PDUController) PowerStatus(ctx context.Context) (State, error) {
	anyOn, anyOff, anyErr, anyUnknown := false, false, false, false
	var lastErr error
	for _, oc := range p.outlets {
		st, err := oc.conn.outletState(ctx, oc.outlet)
		if err != nil {
			anyErr = true
			lastErr = err
			continue
		}
		switch st {
		case StateOn:
			anyOn = true
		case StateOff:
			anyOff = true
		case StateUnknown:
			// The outlet answered, but with a SwitchedState that is
			// neither On nor Off. It does not confirm On, and like an
			// unreachable feed it may be the one powering the box, so it
			// also rules out an aggregate Off.
			anyUnknown = true
		}
	}
	switch {
	case anyOn && (anyOff || anyErr):
		p.log.Warn("pdu power status: feeds disagree, reporting On (box is powered via at least one live feed)")
		return StateOn, nil
	case anyOn:
		return StateOn, nil
	case anyErr:
		// No live feed confirmed and a read failed: we cannot rule out
		// that a feed we couldn't reach is powering the box.
		return StateUnknown, fmt.Errorf("pdu power status: no live feed confirmed and a read failed: %w", lastErr)
	case anyOff && !anyUnknown:
		return StateOff, nil
	default:
		// No live feed confirmed, and at least one outlet reported a
		// state that is neither On nor Off.
		return StateUnknown, nil
	}
}

// SetBootDevice handles boot-order requests for a PDU, which cannot set
// boot order. PDU-managed devices must
// have PXE as permanent firmware boot order, so BootPXE/BootDefault are
// satisfied with no action. BootDisk is genuinely unsatisfiable and
// returns an error rather than silently lying to the executor (which
// would believe the box will boot from disk when it will in fact PXE).
// See doc §Interface-completeness.
func (p *PDUController) SetBootDevice(_ context.Context, device BootDevice) error {
	switch device {
	case BootPXE, BootDefault:
		return nil
	case BootDisk:
		return fmt.Errorf("pdu: cannot set boot device to disk (PDU-managed devices boot via permanent PXE; the network side directs provisioned MACs to local disk)")
	default:
		return fmt.Errorf("pdu: unsupported boot device: %s", device)
	}
}

// ChassisSerial returns ("", nil): a PDU knows its own serial but not the
// downstream device's chassis identity. The worker's serial-verification
// gate treats empty as "skip the check", correctly bypassing chassis
// verification for PDU-controlled devices.
func (p *PDUController) ChassisSerial(_ context.Context) (string, error) {
	return "", nil
}

// Transport identifies the PDU outlet path. Constant: every PDU
// operation actuates/reads switched outlets over the NMC's Redfish,
// but the operator-meaningful fact is "this came from the PDU, not
// the box's BMC".
func (p *PDUController) Transport() string { return "pdu" }

// Close releases every per-outlet connection, aggregating errors.
func (p *PDUController) Close(_ context.Context) error {
	var errs []error
	for _, oc := range p.outlets {
		if oc.conn != nil {
			if err := oc.conn.close(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", oc.host, err))
			}
		}
	}
	return errors.Join(errs...)
}

// parseAPCResult inspects APC NMC CLI output for the result code line
// (E000 = success, E1xx = error) and returns an error for any non-E000
// code. The NMC emits a final "E000: Success" or "E102: Parameter Error"
// line; we scan for the last E-code line. Output with no E-code line is
// treated as a failure (we can't confirm success).
func parseAPCResult(output string) error {
	var code, msg string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		// APC codes look like "E000: Success" / "E102: Parameter Error".
		if len(line) >= 5 && line[0] == 'E' && line[4] == ':' {
			code = line[:4]
			msg = strings.TrimSpace(line[5:])
		}
	}
	if code == "" {
		return fmt.Errorf("no APC result code in output: %q", strings.TrimSpace(output))
	}
	if code != "E000" {
		return fmt.Errorf("APC error %s: %s", code, msg)
	}
	return nil
}

var _ Controller = (*PDUController)(nil)
