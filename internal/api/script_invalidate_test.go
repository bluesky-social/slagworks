package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Storage and network scripts reach the deploy agent only through the site
// snapshot (MachineSnapshot.StorageScript / NetworkScript). Op create and op
// claim do not rebuild the snapshot, so nothing between "operator changes a
// script" and "agent fetches it" forces a rebuild unless the handler
// invalidates: the change otherwise waits for the periodic reconcile or an
// unrelated invalidation, and a deploy started right after `slagctl
// storage-script update` runs the superseded version, although
// ScriptVersion documents Head as "the version currently served". Image and
// SSH key mutations invalidate every site for exactly this reason
// (TestImageUpsert_InvalidatesAllSites,
// TestSSHKeyMutations_InvalidateAllSites).

func scriptInvalidateServer(t *testing.T) (*httptest.Server, *fakeInvalidator, *store.Store) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	for _, slug := range []string{"pop-a", "pop-b"} {
		if err := st.CreateSiteConfig(&model.SiteConfig{Slug: slug, NetBoxSite: slug,
			EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250"}); err != nil {
			t.Fatal(err)
		}
	}
	inv := &fakeInvalidator{}
	s := &Server{store: st, invalidator: inv, echo: echo.New()}
	s.echo.POST("/api/v1/storage-scripts", s.createStorageScript)
	s.echo.PUT("/api/v1/storage-scripts/:name", s.updateStorageScript)
	s.echo.DELETE("/api/v1/storage-scripts/:id", s.deleteStorageScript)
	s.echo.POST("/api/v1/network-scripts", s.createNetworkScript)
	s.echo.PUT("/api/v1/network-scripts/:name", s.updateNetworkScript)
	s.echo.DELETE("/api/v1/network-scripts/:id", s.deleteNetworkScript)
	srv := httptest.NewServer(s.echo)
	t.Cleanup(srv.Close)
	return srv, inv, st
}

func scriptInvalidateDo(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// requireInvalidatedAll fails unless every site was invalidated since the
// last call, then resets the record.
func requireInvalidatedAll(t *testing.T, inv *fakeInvalidator, step string) {
	t.Helper()
	got := map[string]bool{}
	for _, s := range inv.seen() {
		got[s] = true
	}
	if !got["pop-a"] || !got["pop-b"] {
		t.Errorf("%s: invalidated %v; want both pop-a and pop-b rebuilt so workers serve the change", step, inv.seen())
	}
	inv.mu.Lock()
	inv.slugs = nil
	inv.mu.Unlock()
}

func TestScriptMutationsInvalidateAllSites(t *testing.T) {
	t.Skip("script create, update and delete handlers do not invalidate site snapshots, " +
		"so workers keep serving the superseded script until the next periodic reconcile")
	srv, inv, _ := scriptInvalidateServer(t)
	storage := "#!/bin/sh\n# slagworks:match system_product=X1\n# slagworks:priority 100\necho '{}'\n"
	network := "#!/bin/sh\n# slagworks:match hostname=h1\n# slagworks:priority 100\necho '{}'\n"

	for _, kind := range []struct{ path, content string }{
		{"/api/v1/storage-scripts", storage},
		{"/api/v1/network-scripts", network},
	} {
		var created struct {
			ID string `json:"id"`
		}
		if code := scriptInvalidateDo(t, http.MethodPost, srv.URL+kind.path, map[string]string{"name": "s1", "content": kind.content}, &created); code != http.StatusCreated {
			t.Fatalf("create %s: status %d", kind.path, code)
		}
		requireInvalidatedAll(t, inv, "create "+kind.path)

		if code := scriptInvalidateDo(t, http.MethodPut, srv.URL+kind.path+"/s1", map[string]string{"content": kind.content + "# v2\n"}, nil); code != http.StatusOK {
			t.Fatalf("update %s: status %d", kind.path, code)
		}
		requireInvalidatedAll(t, inv, "update "+kind.path)

		if code := scriptInvalidateDo(t, http.MethodDelete, srv.URL+kind.path+"/"+created.ID, nil, nil); code != http.StatusNoContent {
			t.Fatalf("delete %s: status %d", kind.path, code)
		}
		requireInvalidatedAll(t, inv, "delete "+kind.path)
	}
}
