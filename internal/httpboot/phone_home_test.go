package httpboot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// TestPhoneHome_CompletesOnlyTheBoundDeployAwaitingIt pins which requests on
// the site worker's boot HTTP server can complete a deploy. A phone-home is
// the installed OS saying it booted, so it may complete a deploy only through
// the report that names that deploy op (POST /api/v1/report with machine_id,
// op_id and event=phone_home, the body the agent-written unit sends), and
// only once the op is at awaiting_phone_home: the agent has reported
// deploy_complete and the host is booting its freshly written disk.
//
// Completing at any other point records a deploy as successful over a disk
// that was never (fully) written: the worker queues status=completed, global
// marks the machine running and locks it, and the host's next PXE gets
// BootLocal. A request that names no op (the MAC-only POST
// /api/v1/phone-home?mac=) must not complete or change anything, whatever the
// phase; it used to complete whichever deploy op the machine had, and flip a
// commissioning machine's overlay to running.
func TestPhoneHome_CompletesOnlyTheBoundDeployAwaitingIt(t *testing.T) {
	const mac = "52:54:00:aa:bb:09"

	requests := []struct {
		name  string
		bound bool
		build func(machineID, opID uuid.UUID) *http.Request
	}{
		{"MAC only", false, func(_, _ uuid.UUID) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/api/v1/phone-home?mac="+mac, nil)
		}},
		{"bound report", true, func(machineID, opID uuid.UUID) *http.Request {
			body := `{"machine_id":"` + machineID.String() + `","op_id":"` + opID.String() + `","event":"phone_home"}`
			r := httptest.NewRequest(http.MethodPost, "/api/v1/report", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			return r
		}},
	}

	for _, tc := range []struct {
		name     string
		baseline model.MachineState
		opType   model.OperationType
		status   model.OperationStatus
		phase    string
		// awaiting is true when the op is the deploy waiting for this
		// phone-home: only the bound report may complete it.
		awaiting bool
	}{
		{"pending deploy, never claimed", model.MachineStateCommissioned, model.OpDeploy, model.OpPending, "", false},
		{"claimed deploy, mid-install", model.MachineStateDeploying, model.OpDeploy, model.OpClaimed, "rsyncing_os", false},
		{"claimed commission", model.MachineStateCommissioning, model.OpCommission, model.OpClaimed, "ipxe_loaded", false},
		{"pending deploy, install finished", model.MachineStateCommissioned, model.OpDeploy, model.OpPending, "awaiting_phone_home", true},
		{"claimed deploy, awaiting phone-home", model.MachineStateDeploying, model.OpDeploy, model.OpClaimed, "awaiting_phone_home", true},
	} {
		for _, req := range requests {
			t.Run(tc.name+"/"+req.name, func(t *testing.T) {
				machineID, opID := uuid.New(), uuid.New()
				c := worker.NewCache(nil)
				c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
					ID: machineID, State: tc.baseline, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}},
				}}})
				c.ApplyOpResync([]model.Operation{{
					ID: opID, MachineID: machineID, Type: tc.opType, Status: tc.status, Phase: tc.phase, Version: 4,
				}}, nil)
				bootBefore := c.BootActionForMAC(mac)
				mux := NewServer(":0", ServerConfig{DefaultImage: "noble", DeployMode: true, Provider: c}).buildMux()

				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req.build(machineID, opID))

				obs, updates := takeOutbox(c)
				var completed, phoneHomeObs bool
				for _, u := range updates {
					if u.OperationID == opID.String() && u.Status == string(model.OpCompleted) {
						completed = true
					}
				}
				for _, o := range obs {
					if o.Obs.Event == "phone_home" {
						phoneHomeObs = true
					}
				}

				if req.bound && tc.awaiting {
					if rec.Code != http.StatusOK {
						t.Fatalf("HTTP %d (%s), want 200: the installed OS keeps retrying a healthy host's phone-home", rec.Code, strings.TrimSpace(rec.Body.String()))
					}
					if !completed || !phoneHomeObs {
						t.Errorf("completed op_update queued = %v, phone_home observation queued = %v, want both: the deploy never completes", completed, phoneHomeObs)
					}
					if got := c.MachineState(machineID); got != string(model.MachineStateRunning) {
						t.Errorf("machine state = %q, want %q", got, model.MachineStateRunning)
					}
					return
				}

				if rec.Code >= 200 && rec.Code < 300 {
					t.Errorf("HTTP %d, want a rejection: a 2xx tells the caller its phone-home was accepted", rec.Code)
				}
				if completed {
					t.Errorf("worker queued status=completed for %s op %s (status %s, phase %q) from a %s request: global marks the op completed and the machine running+locked",
						tc.opType, opID, tc.status, tc.phase, req.name)
				}
				if phoneHomeObs {
					t.Errorf("worker queued a phone_home observation, the durable channel global completes a deploy from")
				}
				if got := c.MachineState(machineID); got != string(tc.baseline) {
					t.Errorf("machine state = %q, want %q (unchanged)", got, tc.baseline)
				}
				if got := c.BootActionForMAC(mac); got != bootBefore {
					t.Errorf("boot action = %v, want %v (unchanged)", got, bootBefore)
				}
				if tc.status == model.OpPending {
					if n := len(c.PendingLifecycleOps()); n != 1 {
						t.Errorf("pending lifecycle ops visible to the executor = %d, want 1", n)
					}
				}
			})
		}
	}
}
