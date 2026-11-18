package worker

import (
	"encoding/json"
	"fmt"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// extractPDURuntime pulls the pdu_runtime block out of an op's params
// blob. Returns (nil, nil) for ops with no pdu_runtime field (the common
// case: BMC ops, non-object params). Returns an error only when the
// params object won't decode — so the executor can fail the op cleanly
// rather than silently mis-route it.
func extractPDURuntime(params json.RawMessage) (*model.PDURuntime, error) {
	p, err := model.DecodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("op params has pdu_runtime but it failed to decode: %w", err)
	}
	return p.PDURuntime, nil
}

// PDUSpecs converts the wire outlets into power-package outlet specs.
func PDUSpecs(rt *model.PDURuntime) []power.PDUOutletSpec {
	if rt == nil {
		return nil
	}
	out := make([]power.PDUOutletSpec, 0, len(rt.Outlets))
	for _, o := range rt.Outlets {
		out = append(out, power.PDUOutletSpec{
			Host:     o.PDUHost,
			Outlet:   o.Outlet,
			Username: o.Username,
			Password: o.Password,
		})
	}
	return out
}
