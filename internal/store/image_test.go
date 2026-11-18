package store

import (
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
)

func seedImage(t *testing.T, st *Store, name string) *model.Image {
	t.Helper()
	img := &model.Image{
		Name:    name,
		Version: "24.04",
		Arch:    "amd64",
		SHA256:  "ccc3333333333333333333333333333333333333333333333333333333333333",
		URL:     "https://example.com/" + name + "/sha.tar.gz",
	}
	if err := st.CreateImage(img); err != nil {
		t.Fatalf("seed image %q: %v", name, err)
	}
	return img
}

func TestDeleteImage_ByName(t *testing.T) {
	st := testStore(t)
	seedImage(t, st, "noble-pxe")
	if err := st.DeleteImage("noble-pxe"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	images, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("expected 0 images after delete, got %d", len(images))
	}
}

func TestDeleteImage_ByUUID(t *testing.T) {
	st := testStore(t)
	img := seedImage(t, st, "noble-pxe")
	if err := st.DeleteImage(img.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	images, err := st.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Fatalf("expected 0 images after delete, got %d", len(images))
	}
}

func TestDeleteImage_NotFound(t *testing.T) {
	st := testStore(t)
	if err := st.DeleteImage("ghost"); err == nil {
		t.Fatal("expected error deleting unknown image, got nil")
	}
}

// TestUpsertImage_UpdatesExisting covers the happy path for
// `slagctl image follow-latest`: creating a row, then re-pinning it to
// a new SHA via UpsertImage. The second upsert must UPDATE the existing
// row (same ID), not INSERT a duplicate.
func TestUpsertImage_UpdatesExisting(t *testing.T) {
	st := testStore(t)

	first := &model.Image{
		Name:    "noble-pxe",
		Version: "24.04",
		Arch:    "amd64",
		SHA256:  "aaa1111111111111111111111111111111111111111111111111111111111111",
		URL:     "https://example.com/noble-pxe/sha-a.tar.gz",
	}
	if err := st.UpsertImage(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	originalID := first.ID

	images, err := st.ListImages()
	if err != nil {
		t.Fatalf("list after first: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}

	// Second upsert with the same name + a new SHA. Should UPDATE,
	// not INSERT — same ID, new SHA.
	second := &model.Image{
		Name:    "noble-pxe",
		Version: "24.04",
		Arch:    "amd64",
		SHA256:  "bbb2222222222222222222222222222222222222222222222222222222222222",
		URL:     "https://example.com/noble-pxe/sha-b.tar.gz",
	}
	if err := st.UpsertImage(second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// The returned struct must reflect the actual stored row, not the
	// new uuid.New() generated inside UpsertImage. This is the
	// load-bearing assertion for clause.Returning{}: an UPDATE path
	// that doesn't return must NOT silently leave a fabricated ID in
	// the struct that the API handler then ships to the operator.
	if second.ID != originalID {
		t.Errorf("returned struct's ID should match the existing row: was %s, got %s",
			originalID, second.ID)
	}

	images, err = st.ListImages()
	if err != nil {
		t.Fatalf("list after second: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image after upsert (not insert), got %d", len(images))
	}
	got := images[0]
	if got.ID != originalID {
		t.Errorf("DB row ID changed across upsert: %s -> %s", originalID, got.ID)
	}
	if got.SHA256 != second.SHA256 {
		t.Errorf("SHA256 not updated: got %s, want %s", got.SHA256, second.SHA256)
	}
	if got.URL != second.URL {
		t.Errorf("URL not updated: got %s, want %s", got.URL, second.URL)
	}
}
