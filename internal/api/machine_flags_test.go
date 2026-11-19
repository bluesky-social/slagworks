package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

func machineFlagsServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	s := &Server{store: st, log: slog.Default(), echo: echo.New()}
	s.echo.POST("/api/v1/machines/:id/activate", s.activateMachine)
	s.echo.POST("/api/v1/machines/:id/deactivate", s.deactivateMachine)
	s.echo.POST("/api/v1/machines/:id/lock", s.lockMachine)
	s.echo.POST("/api/v1/machines/:id/unlock", s.unlockMachine)
	return s, st
}

func postMachineAction(s *Server, id uuid.UUID, action string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/machines/"+id.String()+"/"+action, nil)
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	return rec
}

// POST /machines/:id/activate on a locked machine is 409 and leaves
// pxe_eligible false (docs/dev/api.md); deactivate is still allowed.
func TestActivateMachine_LockedIsConflict(t *testing.T) {
	s, st := machineFlagsServer(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-activate-locked", State: model.MachineStateRunning, Locked: true}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}

	rec := postMachineAction(s, m.ID, "activate")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "machine is locked") {
		t.Errorf("activate on a locked machine: %d %s, want 409 naming the lock", rec.Code, rec.Body.String())
	}
	got, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PXEEligible {
		t.Errorf("activate on a locked machine set pxe_eligible=true")
	}

	if rec := postMachineAction(s, m.ID, "deactivate"); rec.Code != http.StatusOK {
		t.Errorf("deactivate on a locked machine: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// The PXE and lock endpoints report an unknown machine UUID as 404, not as
// the 500 a failed re-read of the row would give.
func TestMachineFlagEndpoints_UnknownMachineIsNotFound(t *testing.T) {
	s, _ := machineFlagsServer(t)
	missing := uuid.New()
	for _, action := range []string{"activate", "deactivate", "lock", "unlock"} {
		if rec := postMachineAction(s, missing, action); rec.Code != http.StatusNotFound {
			t.Errorf("%s on an unknown machine: %d %s, want 404", action, rec.Code, rec.Body.String())
		}
	}
}
