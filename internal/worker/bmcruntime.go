package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// extractBMCRuntime pulls the bmc_runtime block out of an op's params
// blob. Returns the zero value (empty serial, nil ExtraConfig) for ops
// with no bmc_runtime field; those are payloads that aren't JSON objects
// or objects without the block. A malformed params object (won't decode
// into model.OpParams) returns an error so the executor can fail the op
// cleanly rather than silently mis-parsing it.
func extractBMCRuntime(params json.RawMessage) (model.BMCRuntime, error) {
	p, err := model.DecodeParams(params)
	if err != nil {
		return model.BMCRuntime{}, fmt.Errorf("op params has bmc_runtime but it failed to decode: %w", err)
	}
	if p.BMCRuntime == nil {
		return model.BMCRuntime{}, nil
	}
	return *p.BMCRuntime, nil
}

// verifyChassisSerial gates a BMC operation on a serial-match check.
// Reads the controller's reported serial, normalizes whitespace +
// case, and compares to the expected value from the op envelope.
//
// Three outcomes:
//
//   - rt.ExpectedChassisSerial empty: skip the check. The global
//     server didn't know the serial when the op was created (NetBox
//     hadn't backfilled it yet). Warn and proceed: refusing would
//     block the first deploy of every host whose serial is not yet
//     in NetBox.
//
//   - controller returns ("", nil): non-BMC controller (PDU) or
//     vendor that doesn't surface a serial. Treat as "no check
//     possible"; warn but proceed.
//
//   - mismatch: refuse the op. The error names both serials and the
//     controller host so the operator can diagnose without grepping
//     logs.
func verifyChassisSerial(ctx context.Context, ctrl power.Controller, rt model.BMCRuntime, log *slog.Logger) error {
	if rt.ExpectedChassisSerial == "" {
		log.Warn("bmc serial check skipped: NetBox has no expected serial for this machine")
		return nil
	}
	got, err := ctrl.ChassisSerial(ctx)
	if err != nil {
		// Inventory failed. We don't want to block ops on transient
		// BMC quirks (one Supermicro BMC family returns 500 on
		// inventory under certain firmware), but we also don't want
		// to silently power-cycle the wrong box. Return the error;
		// the executor records it as the op failure reason.
		return fmt.Errorf("bmc serial check: read inventory: %w", err)
	}
	got = strings.TrimSpace(got)
	if got == "" {
		log.Warn("bmc serial check inconclusive: BMC returned empty serial",
			"expected", rt.ExpectedChassisSerial)
		return nil
	}
	if !strings.EqualFold(got, rt.ExpectedChassisSerial) {
		return fmt.Errorf("bmc serial mismatch: expected %q, got %q (refusing to operate on wrong physical box)",
			rt.ExpectedChassisSerial, got)
	}
	return nil
}
