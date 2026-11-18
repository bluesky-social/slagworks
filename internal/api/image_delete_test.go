package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

func imageDeleteFixtureServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	s := &Server{store: st, echo: echo.New()}
	s.echo.DELETE("/api/v1/images/:name", s.deleteImage)
	return s, st
}

func seedAPIImage(t *testing.T, st *store.Store, name string) *model.Image {
	t.Helper()
	img := &model.Image{
		Name:    name,
		Version: "24.04",
		Arch:    "amd64",
		SHA256:  "ddd4444444444444444444444444444444444444444444444444444444444444",
		URL:     "https://example.com/" + name + "/sha.tar.gz",
	}
	if err := st.CreateImage(img); err != nil {
		t.Fatalf("seed image %q: %v", name, err)
	}
	return img
}

func TestDeleteImage_NotFound(t *testing.T) {
	s, _ := imageDeleteFixtureServer(t)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/images/ghost", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteImage_Succeeds(t *testing.T) {
	s, st := imageDeleteFixtureServer(t)
	seedAPIImage(t, st, "noble-pxe")
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/images/noble-pxe", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	images, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("expected 0 images after delete, got %d", len(images))
	}
}

// TestDeleteImage_ByUUID: the DELETE :name param also accepts a uuid,
// since DeleteImage resolves either.
func TestDeleteImage_ByUUID(t *testing.T) {
	s, st := imageDeleteFixtureServer(t)
	img := seedAPIImage(t, st, "noble-pxe")
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/images/"+img.ID.String(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	images, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("expected 0 images after delete, got %d", len(images))
	}
}
