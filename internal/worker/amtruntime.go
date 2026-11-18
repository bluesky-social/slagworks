package worker

import (
	"encoding/json"
	"fmt"

	"github.com/bluesky-social/slagworks/internal/model"
)

// extractAMTRuntime pulls the amt_runtime block out of an op's params
// blob. Returns (nil, nil) for ops without one (BMC and PDU ops,
// non-object params). Returns an error only when the params object won't
// decode, so the executor fails the op instead of silently routing an AMT
// machine down the BMC path.
func extractAMTRuntime(params json.RawMessage) (*model.AMTRuntime, error) {
	p, err := model.DecodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("op params has amt_runtime but it failed to decode: %w", err)
	}
	return p.AMTRuntime, nil
}
