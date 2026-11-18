package commission

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Console determination sweep resolution (the global half of the
// commission-time discovery of slagworks_kernel_params).
//
// The agent's half (consoleprobe.Sweep) emitted a distinct nonce marker
// on every candidate tty before sending the commission report; the
// bytes of whichever tty the BMC SOL is really wired to ride the SOL
// pump into console_cast_events. This half scans for the nonces and
// writes the winning console= back to the device-type custom field —
// ONLY when the field is still empty (never overwrite an operator's
// value; a concurrent commission of a same-type sibling that resolved
// first also counts as "set").
//
// Runs in a goroutine off the commission-report path: resolution
// must not delay the report ack (the nonces usually land before the
// report does, but the pump's flush and the WS hop leave a tail), and
// a miss is not an error — no serial hardware, garbled baud, or a
// BMC without SOL all legitimately produce zero hits, in which case
// the field stays empty and deploy preflight keeps refusing until the
// operator sets a value or "none".

// sweepResolveAttempts spaces the cast-table scans. First attempt is
// immediate (the common case: nonces beat the report), the tail
// covers a slow pump flush or WS reconnect.
var sweepResolveAttempts = []time.Duration{0, 5 * time.Second, 20 * time.Second}

// sweepLookback bounds the cast scan window. Wide enough to cover the
// whole commission boot; the nonces are 16 hex chars so a false
// positive across windows is not a real concern.
const sweepLookback = 15 * time.Minute

// resolveConsoleSweep scans the machine's cast stream for each
// candidate's nonce and writes the discovered console config to the
// device-type. deviceID is re-fetched (not trusted from the report
// handling) because SyncCommissionData may have just re-pointed the
// device at a freshly-minted device-type.
func (h *Handler) resolveConsoleSweep(ctx context.Context, machineID uuid.UUID, deviceID int, sweep *model.ConsoleSweep) {
	if sweep == nil || len(sweep.Candidates) == 0 {
		return
	}

	winner, err := h.scanSweepCandidates(ctx, machineID, sweep)
	if err != nil {
		h.log.Error("console sweep: cast scan failed; slagworks_kernel_params stays unset",
			"machine_id", machineID, "err", err)
		return
	}
	if winner == nil {
		h.log.Warn("console sweep: no candidate nonce reached the cast stream; "+
			"slagworks_kernel_params stays unset (set it manually, or \"none\" if the device has no SOL)",
			"machine_id", machineID, "candidates", len(sweep.Candidates))
		return
	}
	kp := fmt.Sprintf("console=%s,%dn8", winner.TTY, winner.Baud)

	// Serialize the fetch-check-write against sibling sweeps: NetBox
	// has no compare-and-swap on custom fields, so the only-if-empty
	// rule is only atomic because every automated write funnels
	// through this process under sweepMu. (An operator writing the
	// field concurrently in the NetBox UI can still race us — the
	// same accepted exposure as every other slagworks NetBox write.)
	h.sweepMu.Lock()
	defer h.sweepMu.Unlock()

	dev, err := h.nb.GetDeviceByID(ctx, deviceID)
	if err != nil || dev == nil || dev.DeviceType == nil {
		h.log.Error("console sweep: resolved a console but could not fetch the device's type; not written",
			"machine_id", machineID, "device_id", deviceID, "discovered", kp, "err", err)
		return
	}
	dt, err := h.nb.GetDeviceTypeByID(ctx, dev.DeviceType.ID)
	if err != nil {
		h.log.Error("console sweep: fetch device-type failed; not written",
			"machine_id", machineID, "device_type_id", dev.DeviceType.ID, "discovered", kp, "err", err)
		return
	}
	if existing := kernelParamsFromCF(dt.CustomFields); existing != "" {
		h.log.Info("console sweep: device-type already configured; discovered value not written",
			"device_type", dt.Model, "existing", existing, "discovered", kp)
		return
	}
	if err := h.nb.SetDeviceTypeKernelParams(ctx, dt.ID, kp); err != nil {
		h.log.Error("console sweep: device-type write failed",
			"device_type", dt.Model, "discovered", kp, "err", err)
		return
	}
	// WARN, not INFO: this is an automated write to a SHARED NetBox
	// object (every device of this model inherits it) and the operator
	// should see the evidence trail.
	h.log.Warn("console sweep: determined slagworks_kernel_params and wrote it to the device-type",
		"device_type", dt.Model, "device_type_id", dt.ID,
		"kernel_params", kp, "machine_id", machineID, "nonce", winner.Nonce)
}

// scanSweepCandidates polls the cast stream for the candidates'
// nonces across sweepResolveAttempts. Returns the first candidate
// whose nonce arrived, or nil after the last attempt.
func (h *Handler) scanSweepCandidates(ctx context.Context, machineID uuid.UUID, sweep *model.ConsoleSweep) (*model.ConsoleSweepCandidate, error) {
	var lastErr error
	for i, wait := range sweepResolveAttempts {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		for i := range sweep.Candidates {
			cand := &sweep.Candidates[i]
			hit, err := h.casts.ScanCastEventsForNonce(ctx, machineID, cand.Nonce, sweepLookback)
			if err != nil {
				// No cast row yet (machine never SOL'd) or a query
				// failure: remember and retry on the next attempt.
				lastErr = err
				continue
			}
			if hit {
				return cand, nil
			}
		}
		h.log.Debug("console sweep: no nonce hit yet",
			"machine_id", machineID, "attempt", i+1, "of", len(sweepResolveAttempts))
	}
	return nil, lastErr
}

// kernelParamsFromCF pulls slagworks_kernel_params out of a device-type
// custom-fields blob (same shape as snapshot's extractKernelParams).
func kernelParamsFromCF(cf json.RawMessage) string {
	if len(cf) == 0 {
		return ""
	}
	var parsed struct {
		KernelParams string `json:"slagworks_kernel_params"`
	}
	if err := json.Unmarshal(cf, &parsed); err != nil {
		return ""
	}
	return parsed.KernelParams
}
