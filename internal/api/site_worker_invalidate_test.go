package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// A site's snapshot lists its registered workers, and each worker takes its
// share of the fabric dynamic ranges by its position in that list. So
// registering or deleting a worker must rebuild that site's snapshot: until
// it does, the new worker is missing from the list (and serves no dynamic
// share) and the other workers keep shares that overlap its. A slug with no
// site config is not rebuilt, since that rebuild can only fail; creating
// the config builds its snapshot.
func TestSiteWorkerRegisterAndDelete_InvalidateTheirSite(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug:               "pop-a",
		NetBoxSite:         "pop-a",
		EphemeralPoolStart: "10.0.0.10",
		EphemeralPoolEnd:   "10.0.0.250",
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}

	inv := &fakeInvalidator{}
	s := &Server{store: st, invalidator: inv, echo: echo.New()}
	s.echo.POST("/api/v1/site-workers", s.registerSiteWorker)
	s.echo.DELETE("/api/v1/site-workers/:ident", s.deleteSiteWorker)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	register := func(slug, workerID string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"site_slug": slug, "worker_id": workerID})
		resp, err := http.Post(srv.URL+"/api/v1/site-workers", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register %s/%s: status %d", slug, workerID, resp.StatusCode)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.ID
	}

	id := register("pop-a", "worker-a")
	if got := inv.seen(); !slices.Equal(got, []string{"pop-a"}) {
		t.Fatalf("after registering a worker, invalidated %v, want [pop-a]", got)
	}

	register("unconfigured", "worker-x")
	if got := inv.seen(); !slices.Equal(got, []string{"pop-a"}) {
		t.Fatalf("registering a worker for a slug with no site config invalidated %v, want only the earlier [pop-a]", got)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/site-workers/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if got := inv.seen(); !slices.Equal(got, []string{"pop-a", "pop-a"}) {
		t.Fatalf("after deleting the worker, invalidated %v, want [pop-a pop-a]", got)
	}
}
