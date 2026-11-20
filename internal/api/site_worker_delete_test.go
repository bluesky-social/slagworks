package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// fakeWorkerLiveness reports connectedness from a fixed (slug, workerID)
// set, satisfying WorkerLiveness without a live hub.
type fakeWorkerLiveness struct {
	connected map[string]bool // "slug/workerID" -> connected
}

func (f fakeWorkerLiveness) IsWorkerConnected(siteSlug, workerID string) bool {
	return f.connected[siteSlug+"/"+workerID]
}

func siteWorkerDeleteFixtureServer(t *testing.T, live WorkerLiveness) (*Server, *store.Store) {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	s := &Server{store: st, workerLiveness: live, echo: echo.New()}
	s.echo.DELETE("/api/v1/site-workers/:ident", s.deleteSiteWorker)
	return s, st
}

func TestDeleteSiteWorker_NotFound(t *testing.T) {
	s, _ := siteWorkerDeleteFixtureServer(t, nil)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/ghost", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDeleteSiteWorker_Ambiguous seeds two workers sharing a worker_id
// across two slugs; deleting by that shared worker_id must 409.
func TestDeleteSiteWorker_Ambiguous(t *testing.T) {
	s, st := siteWorkerDeleteFixtureServer(t, nil)
	if _, _, err := st.RegisterSiteWorker("pop3", "worker-shared", "a.pop3"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RegisterSiteWorker("pop4", "worker-shared", "b.pop4"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/worker-shared", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestDeleteSiteWorker_RefusesConnected: a worker reported connected by
// the liveness stub must not be deleted (409), and its row must survive.
func TestDeleteSiteWorker_RefusesConnected(t *testing.T) {
	live := fakeWorkerLiveness{connected: map[string]bool{"pop3/worker-a": true}}
	s, st := siteWorkerDeleteFixtureServer(t, live)
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/worker-a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if _, err := st.ResolveSiteWorker(w.ID.String()); err != nil {
		t.Fatalf("row should survive a refused delete: %v", err)
	}
}

// TestDeleteSiteWorker_RefusesInFlightOp: a non-terminal op claimed by the
// worker blocks deletion (409, op id named), and the row survives.
func TestDeleteSiteWorker_RefusesInFlightOp(t *testing.T) {
	s, st := siteWorkerDeleteFixtureServer(t, nil)
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-1", State: model.MachineStateDeploying}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		MachineID: m.ID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		ClaimedBy: "worker-a",
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/worker-a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), op.ID.String()) {
		t.Fatalf("409 body should name the in-flight op id %s, got: %s", op.ID, string(raw))
	}
	if _, err := st.ResolveSiteWorker(w.ID.String()); err != nil {
		t.Fatalf("row should survive a refused delete: %v", err)
	}
}

// TestDeleteSiteWorker_ByUUIDShapedWorkerID: slagworksd's default
// worker_id is a generated uuid. DELETE /site-workers/:ident with that
// worker_id must reach the row rather than 404 because no row has that id.
func TestDeleteSiteWorker_ByUUIDShapedWorkerID(t *testing.T) {
	s, st := siteWorkerDeleteFixtureServer(t, fakeWorkerLiveness{})
	workerID := uuid.NewString()
	w, _, err := st.RegisterSiteWorker("pop3", workerID, "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/"+workerID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE /site-workers/%s: status = %d %s, want 204", workerID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if _, err := st.ResolveSiteWorker(w.ID.String()); !errors.Is(err, store.ErrSiteWorkerNotFound) {
		t.Fatalf("row should be gone after delete, resolve err = %v", err)
	}
}

// TestDeleteSiteWorker_Succeeds: no connection, no ops -> 204 and the row
// is gone.
func TestDeleteSiteWorker_Succeeds(t *testing.T) {
	s, st := siteWorkerDeleteFixtureServer(t, fakeWorkerLiveness{})
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/worker-a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := st.ResolveSiteWorker(w.ID.String()); !errors.Is(err, store.ErrSiteWorkerNotFound) {
		t.Fatalf("row should be gone after delete, resolve err = %v", err)
	}
}

// TestRegisterSiteWorker_RotationReturnsTheRegistration: POSTing the same
// (site_slug, worker_id) twice rotates the token of the existing row, so
// the second response must describe that row: its id, which DELETE
// /site-workers/:ident accepts, and its original first_seen_at.
func TestRegisterSiteWorker_RotationReturnsTheRegistration(t *testing.T) {
	s, _ := siteWorkerDeleteFixtureServer(t, fakeWorkerLiveness{})
	s.echo.POST("/api/v1/site-workers", s.registerSiteWorker)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	type registration struct {
		ID          string    `json:"id"`
		FirstSeenAt time.Time `json:"first_seen_at"`
	}
	register := func() registration {
		t.Helper()
		body := strings.NewReader(`{"site_slug":"pop3","worker_id":"worker-a","fqdn":"a.pop3"}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/site-workers", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("register status = %d, want 201: %s", resp.StatusCode, raw)
		}
		var out registration
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := register()
	second := register() // token rotation
	if second.ID != first.ID {
		t.Errorf("rotation response id = %s, want the registration's id %s", second.ID, first.ID)
	}
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("rotation response first_seen_at = %s, want the registration's %s",
			second.FirstSeenAt, first.FirstSeenAt)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/"+second.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE by the id from the rotation response = %d, want 204", resp.StatusCode)
	}
}
