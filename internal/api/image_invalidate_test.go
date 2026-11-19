package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/labstack/echo/v5"
)

// fakeInvalidator records which site slugs were invalidated, so the image
// handlers' "push a fresh snapshot to all sites" behavior is observable.
type fakeInvalidator struct {
	mu    sync.Mutex
	slugs []string
}

func (f *fakeInvalidator) Invalidate(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slugs = append(f.slugs, slug)
}

func (f *fakeInvalidator) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.slugs...)
}

// fakeOpPublisher records op-stream publishes so op-create (op_delta) and
// abort/sweep (op_removed) behavior is observable in tests.
type fakeOpPublisher struct {
	mu       sync.Mutex
	deltas   []model.Operation
	deltaSt  []model.MachineState
	removed  []model.Operation
	removeSt []model.MachineState
}

func (p *fakeOpPublisher) PublishOpDelta(op model.Operation, st model.MachineState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deltas = append(p.deltas, op)
	p.deltaSt = append(p.deltaSt, st)
}

func (p *fakeOpPublisher) PublishOpRemoved(op model.Operation, st model.MachineState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, op)
	p.removeSt = append(p.removeSt, st)
}

func (p *fakeOpPublisher) deltaCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.deltas)
}

func (p *fakeOpPublisher) removedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.removed)
}

// TestImageUpsert_InvalidatesAllSites is the regression for the propagation
// gap: registering / repointing an image row (the `slagctl image follow-latest`
// path) MUST push a fresh snapshot to every site worker, so imagepull.SyncImages
// pulls the new tarball on the next apply. Before this, an image change only
// reached workers on the next UNRELATED invalidation (an op, a webhook), which
// is why rolling a new image used to require firing a throwaway deploy to
// "trigger" the sync.
func TestImageUpsert_InvalidatesAllSites(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)

	// Two site_configs so we can assert the fan-out hits every site.
	for _, slug := range []string{"pop-a", "pop-b"} {
		if err := st.CreateSiteConfig(&model.SiteConfig{
			Slug:               slug,
			NetBoxSite:         slug,
			EphemeralPoolStart: "10.0.0.10",
			EphemeralPoolEnd:   "10.0.0.250",
		}, nil); err != nil {
			t.Fatalf("seed site_config %s: %v", slug, err)
		}
	}

	inv := &fakeInvalidator{}
	s := &Server{store: st, invalidator: inv, echo: echo.New()}
	s.echo.PUT("/api/v1/images/:name", s.upsertImage)
	s.echo.POST("/api/v1/images", s.createImage)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	// follow-latest does a PUT /images/noble-pxe.
	body, _ := json.Marshal(model.Image{
		Name:    "noble-pxe",
		Version: "24.04",
		Arch:    "amd64",
		SHA256:  "a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00",
		URL:     "https://example.com/noble-pxe/abc.tar.gz",
	})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/images/noble-pxe", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT image: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT image: status %d", resp.StatusCode)
	}

	got := inv.seen()
	if len(got) != 2 {
		t.Fatalf("upsert must invalidate every site (2), got %v", got)
	}
	want := map[string]bool{"pop-a": true, "pop-b": true}
	for _, slug := range got {
		if !want[slug] {
			t.Errorf("unexpected invalidated slug %q (want pop-a/pop-b)", slug)
		}
	}
}

// TestImageWrite_RejectsInvalidName: an image name becomes a directory on
// every site worker (imagepull.SyncImages removes and replaces
// <images_dir>/<name>), so POST and PUT must refuse a name that is not a
// plain path element with a 400, store nothing, and push no snapshot.
// Before the fix both accepted "..", "." and "../victim", and the next
// snapshot made every site worker delete its data dir, its images dir, or
// a sibling of it.
func TestImageWrite_RejectsInvalidName(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug:               "pop-a",
		NetBoxSite:         "pop-a",
		EphemeralPoolStart: "10.0.0.10",
		EphemeralPoolEnd:   "10.0.0.250",
	}, nil); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	inv := &fakeInvalidator{}
	s := &Server{store: st, invalidator: inv, echo: echo.New()}
	s.echo.PUT("/api/v1/images/:name", s.upsertImage)
	s.echo.POST("/api/v1/images", s.createImage)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	send := func(method, path, name string) int {
		t.Helper()
		body, _ := json.Marshal(model.Image{
			Name:    name,
			Version: "24.04",
			SHA256:  "a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00",
			URL:     "https://example.com/noble-pxe/abc.tar.gz",
		})
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, name := range []string{"..", ".", "../victim", "noble-pxe/..", ".hidden", "Noble-PXE", "noble pxe"} {
		if code := send(http.MethodPost, "/api/v1/images", name); code != http.StatusBadRequest {
			t.Errorf("POST image name %q: status %d, want 400", name, code)
		}
	}
	// The PUT name comes from the path; ".." and "." reach the handler
	// as-is (Go's client and echo's router do not clean them).
	for _, name := range []string{"..", ".", ".hidden", "Noble-PXE"} {
		if code := send(http.MethodPut, "/api/v1/images/"+name, "noble-pxe"); code != http.StatusBadRequest {
			t.Errorf("PUT image name %q: status %d, want 400", name, code)
		}
	}

	imgs, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 0 {
		t.Errorf("refused writes stored rows: %+v", imgs)
	}
	if got := inv.seen(); len(got) != 0 {
		t.Errorf("refused writes pushed snapshots to %v", got)
	}

	// A conventional name is still accepted.
	if code := send(http.MethodPost, "/api/v1/images", "noble-pxe"); code != http.StatusCreated {
		t.Errorf("POST image name noble-pxe: status %d, want 201", code)
	}
}
