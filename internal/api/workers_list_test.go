package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// fakeWorkerTools answers WorkerTools from fixed maps, standing in for the
// WS hub's live per-connection state.
type fakeWorkerTools struct {
	status     map[string]string // "slug/workerID" -> reported status (connected)
	supermicro map[string]int    // slug -> Supermicro machine count
}

func (f fakeWorkerTools) WorkerSMCIPMITool(siteSlug, workerID string) (string, bool) {
	s, ok := f.status[siteSlug+"/"+workerID]
	return s, ok
}

func (f fakeWorkerTools) SupermicroMachineCount(slug string) int { return f.supermicro[slug] }

func TestListWorkers_SMCIPMIToolStatus(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	for _, w := range []struct{ slug, id string }{{"pop1", "online"}, {"pop1", "offline"}, {"pop2", "dell-only"}} {
		if _, _, err := st.RegisterSiteWorker(w.slug, w.id, w.id+".example"); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st, echo: echo.New(), workerTools: fakeWorkerTools{
		status:     map[string]string{"pop1/online": "missing", "pop2/dell-only": "missing"},
		supermicro: map[string]int{"pop1": 4},
	}}
	s.echo.GET("/api/v1/workers", s.listWorkers)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []WorkerListEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]WorkerListEntry{}
	for _, w := range got {
		byID[w.WorkerID] = w
	}
	want := map[string]struct {
		status     string
		supermicro int
	}{
		"online":    {"missing", 4},
		"offline":   {"", 4},
		"dell-only": {"missing", 0},
	}
	for id, w := range want {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("worker %s missing from response", id)
		}
		if e.SMCIPMITool != w.status || e.SupermicroMachines != w.supermicro {
			t.Errorf("worker %s: smcipmitool=%q supermicro_machines=%d; want %q, %d",
				id, e.SMCIPMITool, e.SupermicroMachines, w.status, w.supermicro)
		}
		if e.SiteSlug == "" || e.FQDN != id+".example" {
			t.Errorf("worker %s: registration fields not flattened into the entry: %+v", id, e)
		}
	}
}
