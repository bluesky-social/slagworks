package commission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// handleBMCReconcileReport persists the outcome of a remote bmc_reconcile
// op and finalizes the op. A report that carries a monitor password
// generated one; this is the only place it is written, and only to
// pop/<pop>/bmc-creds-monitor: as the device's password, or, when the
// worker could not learn whether the BMC applied it
// (BMCUserCred.PasswordUnconfirmed), beside the password already stored
// (upsertMonitorCred). A report of success that carries none drops the
// entry's unconfirmed passwords: the BMC holds the stored password, so none
// of them is live. Storage keys on the password being present,
// not on monitor_rotated, so a worker still on the previous release (which
// always rotates and has no such field) is stored during the minute or two
// workers lag global on a rollout. A report without one re-asserted the
// password already stored there (the op carried it), so nothing is
// written: a same-value write would still cut a new secret version. The op
// row receives report.Result() — the error and non-secret facts — and
// nothing here logs the report body.
//
// Order: store the password first, then finalize the op. The password is
// already live on the BMC, so a stored-but-op-still-open state (a store
// error after the upsert) is the recoverable one: the worker replays, the
// upsert is idempotent, and the op then finalizes.
//
// Binding: the op must be an active bmc_reconcile op for this machine.
// Anything else (fabricated, cross-host, or an op already finalized or
// timed out) is dropped terminally WITHOUT touching the secret, so a stale
// report can never overwrite the password of a newer reconcile.
func (h *Handler) handleBMCReconcileReport(ctx context.Context, obs model.Observation) error {
	if _, err := h.store.ValidateOperationBinding(
		obs.OperationID, obs.MachineID, model.OpBMCReconcile, nil,
	); err != nil {
		return reportBindingError("validate bmc_reconcile operation binding", err)
	}
	var rep model.BMCReconcileReport
	if err := json.Unmarshal(obs.Data, &rep); err != nil {
		// json errors quote at most a token; never the whole payload.
		return fmt.Errorf("decode bmc_reconcile_report: %w: %w", err, ws.ErrTerminal)
	}

	result := rep.Result()
	switch {
	case rep.Monitor == nil && rep.Error == "":
		result.Error = "bmc reconcile reported success without reporting the monitor account; re-run `slagctl machine bmc-reconcile`"
	case rep.MonitorRotated && (rep.Monitor == nil || rep.Monitor.Password == ""):
		// Storing it would blank the host's monitor entry. A re-run
		// re-asserts the stored password (or generates one if none is
		// stored), which converges the BMC either way.
		result.Error = "bmc reconcile reported a rotated monitor password but carried none, so nothing was stored; " +
			"re-run `slagctl machine bmc-reconcile`"
		if rep.Error != "" {
			result.Error = rep.Error + "; " + result.Error
		}
	case rep.Monitor != nil && rep.Monitor.Password != "":
		if err := h.storeReconciledMonitor(ctx, obs, *rep.Monitor); err != nil {
			var stuck *monitorNotStoredError
			if !errors.As(err, &stuck) {
				return err // transient: the worker retries, the op stays open
			}
			result.Error = stuck.Error()
			if rep.Error != "" {
				result.Error = rep.Error + "; " + result.Error
			}
		}
	case rep.Error == "":
		if err := h.dropUnconfirmedMonitor(ctx, obs); err != nil {
			return err // transient: the worker retries, the op stays open
		}
	}

	raw := result.Marshal()
	var outcome *opOutcome
	var err error
	if result.Error != "" {
		outcome, err = h.failOp(obs, raw)
	} else {
		outcome, err = h.completeOp(obs, raw)
	}
	if err != nil {
		return fmt.Errorf("finalize bmc_reconcile op %s: %w", obs.OperationID, err)
	}
	h.log.Info("bmc_reconcile finalized",
		"op_id", obs.OperationID, "machine_id", obs.MachineID,
		"failed", result.Error != "", "error", result.Error,
		"monitor_slot", result.BMCReconcile.MonitorSlot,
		"monitor_rotated", result.BMCReconcile.MonitorRotated,
		"monitor_password_written", result.BMCReconcile.MonitorPasswordWritten,
		"leftovers_disabled", result.BMCReconcile.LeftoversDisabled,
		"ntp_configured", result.BMCReconcile.NTPConfigured,
		"timezone_set", result.BMCReconcile.TimezoneSet,
		"clock_stepped", result.BMCReconcile.ClockStepped,
		"clock_skew_seconds", result.BMCReconcile.ClockSkewSeconds,
		"time_error", result.BMCReconcile.TimeError)
	if outcome != nil && h.publisher != nil {
		h.publisher.PublishOpRemoved(outcome.op, outcome.state)
	}
	return nil
}

// opOutcome is the subset of store.OpOutcome the report path publishes.
type opOutcome struct {
	op    model.Operation
	state model.MachineState
}

func (h *Handler) completeOp(obs model.Observation, result []byte) (*opOutcome, error) {
	// No target state: a BMC account reconcile never moves the machine's
	// lifecycle state.
	o, err := h.store.CompleteOperationWithState(obs.OperationID, "", result)
	if err != nil || o == nil {
		return nil, err
	}
	return &opOutcome{op: o.Op, state: o.MachineState}, nil
}

func (h *Handler) failOp(obs model.Observation, result []byte) (*opOutcome, error) {
	o, err := h.store.FailOperationOutcome(obs.OperationID, result)
	if err != nil || o == nil {
		return nil, err
	}
	return &opOutcome{op: o.Op, state: o.MachineState}, nil
}

// monitorNotStoredError means the rotated monitor password is live on the
// BMC but could not be stored, for a reason retrying cannot fix. Its text
// becomes the op's failure reason, so it says what happened and what to do.
type monitorNotStoredError struct{ reason string }

func (e *monitorNotStoredError) Error() string {
	return "the BMC's monitor password was (or may have been) rotated but could not be stored: " + e.reason +
		"; ipmi_exporter scrapes of this BMC fail until you fix that and re-run `slagctl machine bmc-reconcile`"
}

// storeReconciledMonitor upserts a bmc_reconcile report's monitor cred.
// Returns *monitorNotStoredError for operator-fixable failures (the caller
// fails the op with it and acks) and a plain error for transient ones (the
// caller returns it so the worker retries).
func (h *Handler) storeReconciledMonitor(ctx context.Context, obs model.Observation, cred model.BMCUserCred) error {
	if h.monitorAWS == nil {
		return &monitorNotStoredError{reason: "global has no monitor-secret writer configured (no AWS credentials)"}
	}
	dev, err := h.machineDevice(ctx, obs.MachineID)
	switch {
	case errors.Is(err, ErrMachineNotFound), errors.Is(err, ErrDeviceNotBound):
		return &monitorNotStoredError{reason: err.Error()}
	case err != nil:
		return err
	case dev == nil:
		return &monitorNotStoredError{reason: "the machine's NetBox device no longer exists"}
	}
	err = h.upsertMonitorCred(ctx, dev, cred)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, awssecrets.ErrMonitorSecretMissing):
		return &monitorNotStoredError{reason: err.Error() + " (bootstrap the pop's monitor secret first)"}
	case errors.Is(err, errMonitorKey):
		return &monitorNotStoredError{reason: err.Error()}
	default:
		return fmt.Errorf("upsert monitor cred for %s: %w", dev.Name, err)
	}
}

// dropUnconfirmedMonitor removes the unconfirmed passwords from the
// machine's monitor entry after a bmc_reconcile op that succeeded without
// reporting a password: the BMC holds the stored one. It writes nothing
// when the entry has none. A device or entry that cannot be found has
// nothing to drop; only a failure worth retrying is returned.
func (h *Handler) dropUnconfirmedMonitor(ctx context.Context, obs model.Observation) error {
	if h.monitorAWS == nil {
		return nil
	}
	dev, err := h.machineDevice(ctx, obs.MachineID)
	switch {
	case errors.Is(err, ErrMachineNotFound), errors.Is(err, ErrDeviceNotBound):
		return nil
	case err != nil:
		return err
	case dev == nil || dev.Serial == "" || dev.Site == nil || dev.Site.Slug == "":
		return nil
	}
	pop := dev.Site.Slug
	err = h.store.WithPopAdvisoryLock(ctx, pop, func(ctx context.Context) error {
		entry, err := h.monitorAWS.GetMonitorEntry(ctx, pop, dev.Serial)
		if err != nil || entry == nil || len(entry.UnconfirmedPasswords) == 0 {
			return err
		}
		entry.UnconfirmedPasswords = nil
		return h.monitorAWS.UpsertMonitorEntry(ctx, pop, dev.Serial, *entry)
	})
	if err != nil && !errors.Is(err, awssecrets.ErrMonitorSecretMissing) {
		return fmt.Errorf("drop unconfirmed monitor passwords for %s: %w", dev.Name, err)
	}
	return nil
}

// errMonitorKey marks a device that cannot be keyed into the monitor
// secret (no NetBox serial or site). Operator-fixable in NetBox.
var errMonitorKey = errors.New("cannot key the monitor secret entry")

// upsertMonitorCred writes a host's monitor credential into
// pop/<pop>/bmc-creds-monitor under the device's NetBox serial (the same
// key as bmc-creds-admin), recording the host's current NetBox name and
// BMC IP (oob_ip) as last_known_hostname / last_known_ip. Serialized per
// pop because Secrets Manager has no compare-and-swap. Requires
// h.monitorAWS to be non-nil.
//
// A confirmed password replaces the entry, unconfirmed passwords included:
// it is what the account holds now. An unconfirmed one
// (cred.PasswordUnconfirmed) is appended to the entry's unconfirmed
// passwords and the stored password stays, because the BMC may hold either
// and the store must not lose the one that is live. With no stored password
// to keep, it is stored as the password: no password the store knew is at
// stake, and the next bmc_reconcile op confirms it or writes it.
func (h *Handler) upsertMonitorCred(ctx context.Context, dev *netbox.Device, cred model.BMCUserCred) error {
	if dev.Serial == "" {
		return fmt.Errorf("%w: NetBox device %d (%q) has no serial, and pop/<pop>/bmc-creds-monitor is keyed by NetBox device serial; set the device's serial in NetBox",
			errMonitorKey, dev.ID, dev.Name)
	}
	if dev.Site == nil || dev.Site.Slug == "" {
		return fmt.Errorf("%w: NetBox device %d (%q) has no site, so its pop's monitor secret cannot be located; assign the device a site in NetBox",
			errMonitorKey, dev.ID, dev.Name)
	}
	pop := dev.Site.Slug
	lastIP := ""
	if dev.OOBIP != nil {
		lastIP, _, _ = strings.Cut(dev.OOBIP.Address, "/")
	}
	entry := awssecrets.BMCMonitorEntry{
		Kind:              awssecrets.KindBMC,
		Username:          cred.Username,
		Password:          cred.Password,
		ExtraConfig:       map[string]any{"ipmi_user_slot": cred.IPMIUserSlot},
		LastKnownHostname: dev.Name,
		LastKnownIP:       lastIP,
	}
	return h.store.WithPopAdvisoryLock(ctx, pop, func(ctx context.Context) error {
		if cred.PasswordUnconfirmed {
			cur, err := h.monitorAWS.GetMonitorEntry(ctx, pop, dev.Serial)
			if err != nil {
				return err
			}
			if cur != nil && cur.Password != "" {
				kept := *cur
				kept.LastKnownHostname, kept.LastKnownIP = entry.LastKnownHostname, entry.LastKnownIP
				if cred.Password != cur.Password && !slices.Contains(cur.UnconfirmedPasswords, cred.Password) {
					kept.UnconfirmedPasswords = append(slices.Clone(cur.UnconfirmedPasswords), cred.Password)
				}
				entry = kept
			}
		}
		return h.monitorAWS.UpsertMonitorEntry(ctx, pop, dev.Serial, entry)
	})
}
