package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// midRequestBody is a request body that runs hook on the handler's first
// read of it. It stands for a concurrent writer that commits while the
// PATCH body is still arriving (a slow client or proxy, or
// `Expect: 100-continue`, where Go sends the 100 only on that first read).
// A handler that loaded the machine row before binding the body holds a
// stale copy by the time it writes.
type midRequestBody struct {
	hook func()
	r    io.Reader
	done bool
}

func (b *midRequestBody) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		b.hook()
	}
	return b.r.Read(p)
}

func patchMachineWhile(t *testing.T, st *store.Store, id uuid.UUID, body string, hook func()) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{store: st, log: slog.Default(), echo: echo.New()}
	s.echo.PATCH("/api/v1/machines/:id", s.updateMachine)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/machines/"+id.String(),
		&midRequestBody{hook: hook, r: strings.NewReader(body)})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	return rec
}

// PATCH /machines/:id applies only desired_os, so it must write only that
// column. A deploy that completes while the request is in flight
// (deploying -> running, auto-lock) has to survive the PATCH: reverting it
// would leave the machine `deploying` with a completed op, which no sweep,
// abort or new op can move.
func TestUpdateMachineDoesNotRevertATransitionThatLandsMidRequest(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-patch-race", State: model.MachineStateCommissioned, PXEEligible: true}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, nil, model.MachineStateCommissioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	rec := patchMachineWhile(t, st, m.ID, `{"desired_os":"noble"}`, func() {
		// The deployed host phones home: deploying -> running, auto-lock.
		if out, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil); err != nil || out == nil {
			t.Errorf("mid-request completion: outcome set=%v err=%v", out != nil, err)
		}
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}

	got, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotOp, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOp.Status != model.OpCompleted {
		t.Fatalf("setup: deploy op is %s, want completed", gotOp.Status)
	}
	if got.DesiredOS != "noble" {
		t.Errorf("desired_os = %q, want noble", got.DesiredOS)
	}
	if got.State != model.MachineStateRunning {
		t.Errorf("machine state is %q after its deploy completed; want running (the PATCH wrote back the state it read before the completion)", got.State)
	}
	if !got.Locked {
		t.Errorf("machine lost the in-service lock its deploy completion set")
	}
}

// A machine deleted while the PATCH is in flight (its NetBox device was
// deleted) must stay deleted, and the PATCH must report 404 rather than
// re-create the row.
func TestUpdateMachineDoesNotResurrectAMachineDeletedMidRequest(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	dev := 4242
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-patch-delete", NetBoxDeviceID: &dev, State: model.MachineStateCommissioned}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	rec := patchMachineWhile(t, st, m.ID, `{"desired_os":"noble"}`, func() {
		if err := st.DeleteMachine(m.ID); err != nil {
			t.Errorf("mid-request delete: %v", err)
		}
	})

	if _, err := st.GetMachine(m.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("machine row exists again after being deleted mid-PATCH (GetMachine err=%v); want it to stay deleted", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH of a machine deleted mid-request: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

// A body carrying `state` is rejected without writing anything, including
// a desired_os sent alongside it.
func TestUpdateMachineRejectsStateWithoutWriting(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-patch-state", State: model.MachineStateCommissioned, DesiredOS: "jammy"}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	rec := patchMachineWhile(t, st, m.ID, `{"desired_os":"noble","state":"running"}`, func() {})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with state: %d %s, want 400", rec.Code, rec.Body.String())
	}
	got, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MachineStateCommissioned || got.DesiredOS != "jammy" {
		t.Errorf("rejected PATCH wrote the row: state=%q desired_os=%q, want commissioned/jammy", got.State, got.DesiredOS)
	}
}
