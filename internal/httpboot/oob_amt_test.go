package httpboot

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestBootScripts_OOBAMTToken: an AMT machine's commission AND deploy boot
// scripts carry slagworks.oob=amt (the agent's D2 gate for bmc_ipmitool /
// bmc_reconcile); a non-AMT machine's never do.
func TestBootScripts_OOBAMTToken(t *testing.T) {
	for _, amt := range []bool{true, false} {
		for _, mode := range []string{"deploy", "commission"} {
			p := newTestProviderKnownMAC()
			machineID, _ := p.MachineIDForMAC(testMAC)
			p.SetAMT(machineID, amt)
			if mode == "commission" {
				p.SetCommissioning(machineID)
				p.SetCommissionOp(machineID, &model.Operation{
					ID: uuid.New(), MachineID: machineID, Type: model.OpCommission,
					Status: model.OpClaimed, Params: json.RawMessage(`{}`),
				})
			}
			h := NewHandler(HandlerConfig{DefaultImage: "noble", HTTPPort: 8080, DeployMode: true}, p)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil))
			body := w.Body.String()
			if !strings.Contains(body, "slagworks.mode=commission") != (mode == "deploy") {
				t.Fatalf("%s: boot script is not the %s script: %q", mode, mode, body)
			}
			if got := strings.Contains(body, "slagworks.oob=amt"); got != amt {
				t.Errorf("%s amt=%v: slagworks.oob=amt present=%v, script %q", mode, amt, got, body)
			}
		}
	}
}
