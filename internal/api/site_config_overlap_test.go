package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// overlapNetBox serves the NetBox reads site-config validation makes for
// whole-site configs: every site exists, has no locations, and has one
// production prefix.
func overlapNetBox(t *testing.T) *netbox.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/sites/", func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		writeJSONList(w, []netbox.Site{{ID: 1, Name: slug, Slug: slug}})
	})
	mux.HandleFunc("/api/dcim/locations/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONList(w, []netbox.Location{})
	})
	mux.HandleFunc("/api/ipam/prefixes/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONList(w, []netbox.Prefix{{ID: 1, Prefix: "10.0.0.0/24"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return netbox.NewClient(netbox.ClientConfig{BaseURL: srv.URL, Token: "t"})
}

// waitForLockWaiters polls until at least n backends of this test's
// database are waiting on a lock, or done is closed. It reports whether the
// waiters were seen.
func waitForLockWaiters(t *testing.T, db *gorm.DB, n int64, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int64
		if err := db.Raw(`
			SELECT count(DISTINCT l.pid) FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()`).Scan(&waiting).Error; err != nil {
			t.Fatalf("query pg_locks: %v", err)
		}
		if waiting >= n {
			return true
		}
		select {
		case <-done:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %d backends to block on a lock", n)
	return false
}

// The overlap check and the write are one step with respect to other
// site-config writes. c1 is whole-site on s2. A PATCH moving c1 to
// whole-site s1 passes its overlap check and is held just before its row
// write; a POST creating c2 whole-site on s1 arrives in that window. Checked
// separately from the writes, both pass (neither sees the other on s1) and
// both land, leaving two configs that each claim every device in s1.
//
// The test holds the PATCH at its write by locking c1's row from a
// transaction of its own, so the interleaving does not depend on timing.
func TestSiteConfigOverlapCheckIsAtomicWithWrite(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "c1", NetBoxSite: "s2", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, netbox: overlapNetBox(t), log: slog.Default(), echo: echo.New()}
	s.echo.POST("/api/v1/site-configs", s.createSiteConfig)
	s.echo.PATCH("/api/v1/site-configs/:slug", s.updateSiteConfig)
	serve := func(method, path, body string) chan *httptest.ResponseRecorder {
		out := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.echo.ServeHTTP(rec, req)
			out <- rec
		}()
		return out
	}

	// Hold c1's row so the PATCH's UPDATE blocks after its overlap check.
	hold := db.Begin()
	if hold.Error != nil {
		t.Fatal(hold.Error)
	}
	released := false
	defer func() {
		if !released {
			hold.Rollback()
		}
	}()
	var row model.SiteConfig
	if err := hold.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "slug = ?", "c1").Error; err != nil {
		t.Fatalf("lock c1: %v", err)
	}

	patch := serve(http.MethodPatch, "/api/v1/site-configs/c1", `{"netbox_site":"s1"}`)
	never := make(chan struct{})
	waitForLockWaiters(t, db, 1, never)

	// The POST either completes (the bug: nothing held it off) or queues
	// behind the PATCH.
	postDone := make(chan struct{})
	postRec := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := <-serve(http.MethodPost, "/api/v1/site-configs",
			`{"slug":"c2","netbox_site":"s1","ephemeral_pool_start":"10.0.1.100","ephemeral_pool_end":"10.0.1.200"}`)
		close(postDone)
		postRec <- rec
	}()
	waitForLockWaiters(t, db, 2, postDone)

	if err := hold.Commit().Error; err != nil {
		t.Fatalf("release c1: %v", err)
	}
	released = true
	patchRes, postRes := <-patch, <-postRec

	cfgs, err := st.ListSiteConfigs()
	if err != nil {
		t.Fatal(err)
	}
	var onS1 []string
	for _, c := range cfgs {
		if c.NetBoxSite == "s1" {
			onS1 = append(onS1, c.Slug)
		}
	}
	if len(onS1) != 1 {
		t.Errorf("whole-site configs on s1 = %v, want exactly one (PATCH %d %s; POST %d %s)",
			onS1, patchRes.Code, patchRes.Body.String(), postRes.Code, postRes.Body.String())
	}
	if patchRes.Code != http.StatusOK {
		t.Errorf("PATCH c1 -> s1 (first to check): %d %s, want 200", patchRes.Code, patchRes.Body.String())
	}
	if postRes.Code != http.StatusConflict {
		t.Errorf("POST c2 on s1 while c1 was moving there: %d %s, want 409", postRes.Code, postRes.Body.String())
	}
}
