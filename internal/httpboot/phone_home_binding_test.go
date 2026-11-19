package httpboot_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// These tests drive the boot HTTP server against the real site-worker cache,
// so the phone-home binding rules in Cache.HandleProgressReport apply. A
// phone-home completes a deploy op and moves the machine to `running`, which
// flips its next PXE boot to the local disk, so the server must accept it
// only in the bound form the installed OS sends (machine_id + op_id + event
// on POST /api/v1/report), never keyed on a MAC alone.

const phoneHomeMAC = "52:54:00:aa:bb:01"

var phoneHomeMachine = uuid.MustParse("11111111-1111-4111-8111-111111111111")

func newPhoneHomeServer(t *testing.T, state model.MachineState) (*worker.Cache, http.Handler) {
	t.Helper()
	c := worker.NewCache(nil)
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
		ID: phoneHomeMachine, State: state, PXEEligible: true,
		SystemUUID:   "4c4c4544-0042-3510-8051-b4c04f4a4433",
		KernelParams: "console=ttyS1,115200n8",
		Interfaces:   []model.SnapshotInterface{{Name: "eth0", MAC: phoneHomeMAC}},
	}}})
	c.ApplyOpResync(nil, nil)
	srv := httpboot.NewServer(":0", httpboot.ServerConfig{
		DefaultImage: "noble-pxe", HTTPPort: 8080, DeployMode: true, Provider: c,
	})
	srv.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, srv.Build()
}

func postPhoneHome(h http.Handler, target, body string) int {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	h.ServeHTTP(w, r)
	return w.Code
}

func completedUpdate(updates []model.OperationUpdate, opID uuid.UUID) bool {
	for _, u := range updates {
		if u.OperationID == opID.String() && u.Status == string(model.OpCompleted) {
			return true
		}
	}
	return false
}

// A MAC-only phone-home names no operation, so it must not complete a deploy
// that is still writing the disk: doing so marks the machine running and
// boots its next PXE cycle into a half-installed OS.
func TestMACOnlyPhoneHome_DoesNotCompleteAnInstallingDeploy(t *testing.T) {
	c, h := newPhoneHomeServer(t, model.MachineStateCommissioned)
	opID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: phoneHomeMachine, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateDeploying)
	if err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: phoneHomeMachine.String(), Phase: "partitioning",
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.BootActionForMAC(phoneHomeMAC); got != model.BootDeploy {
		t.Fatalf("precondition: boot action = %v, want BootDeploy (%v)", got, model.BootDeploy)
	}
	c.DrainOutbox()

	if code := postPhoneHome(h, "/api/v1/phone-home?mac="+phoneHomeMAC, ""); code >= 200 && code < 300 {
		t.Errorf("MAC-only phone-home answered %d, want a non-2xx rejection", code)
	}

	if _, updates := c.DrainOutbox(); completedUpdate(updates, opID) {
		t.Errorf("MAC-only phone-home completed deploy op %s while it was in phase partitioning", opID)
	}
	if got := c.BootActionForMAC(phoneHomeMAC); got != model.BootDeploy {
		t.Errorf("boot action after MAC-only phone-home = %v, want BootDeploy (%v)", got, model.BootDeploy)
	}
	if st := c.MachineState(phoneHomeMachine); st != string(model.MachineStateDeploying) {
		t.Errorf("machine state after MAC-only phone-home = %q, want deploying", st)
	}
}

// A machine with no deploy at all must not be moved to running either: the
// running overlay turns a claimed commission's BootCommission into BootLocal,
// which the bound path refuses (a phone_home against a commission op is
// rejected by Cache.HandleProgressReport).
func TestMACOnlyPhoneHome_DoesNotMarkACommissioningMachineRunning(t *testing.T) {
	c, h := newPhoneHomeServer(t, model.MachineStateNew)
	opID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: phoneHomeMachine, Type: model.OpCommission, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateCommissioning)
	if got := c.BootActionForMAC(phoneHomeMAC); got != model.BootCommission {
		t.Fatalf("precondition: boot action = %v, want BootCommission (%v)", got, model.BootCommission)
	}

	if code := postPhoneHome(h, "/api/v1/phone-home?mac="+phoneHomeMAC, ""); code >= 200 && code < 300 {
		t.Errorf("MAC-only phone-home answered %d, want a non-2xx rejection", code)
	}

	if got := c.BootActionForMAC(phoneHomeMAC); got != model.BootCommission {
		t.Errorf("boot action after MAC-only phone-home = %v, want BootCommission (%v)", got, model.BootCommission)
	}
	if st := c.MachineState(phoneHomeMachine); st != string(model.MachineStateCommissioning) {
		t.Errorf("machine state after MAC-only phone-home = %q, want commissioning", st)
	}
}

// Control: the bound form the installed OS's slagworks-phone-home.service
// sends (target.GeneratePhoneHomeUnit) still completes a finished deploy
// through the same server.
func TestBoundPhoneHome_CompletesAFinishedDeploy(t *testing.T) {
	c, h := newPhoneHomeServer(t, model.MachineStateCommissioned)
	opID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: phoneHomeMachine, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateDeploying)
	bound := func(event string) string {
		return `{"machine_id":"` + phoneHomeMachine.String() + `","op_id":"` + opID.String() + `","event":"` + event + `"}`
	}
	if code := postPhoneHome(h, "/api/v1/report", bound("deploy_complete")); code != http.StatusOK {
		t.Fatalf("deploy_complete report answered %d, want 200", code)
	}
	c.DrainOutbox()

	if code := postPhoneHome(h, "/api/v1/report", bound("phone_home")); code != http.StatusOK {
		t.Fatalf("bound phone_home report answered %d, want 200", code)
	}

	if _, updates := c.DrainOutbox(); !completedUpdate(updates, opID) {
		t.Errorf("bound phone_home did not queue op_update completed for %s; updates=%+v", opID, updates)
	}
	if got := c.BootActionForMAC(phoneHomeMAC); got != model.BootLocal {
		t.Errorf("boot action after bound phone_home = %v, want BootLocal (%v)", got, model.BootLocal)
	}
	if st := c.MachineState(phoneHomeMachine); st != string(model.MachineStateRunning) {
		t.Errorf("machine state after bound phone_home = %q, want running", st)
	}
}
