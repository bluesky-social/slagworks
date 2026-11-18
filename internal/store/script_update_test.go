package store

import (
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// sameDBTime compares two timestamps at microsecond precision — Postgres
// timestamptz stores µs, while gorm populates CreatedAt Go-side at insert
// with the platform clock's full precision (ns on Linux). A create's
// in-memory CreatedAt therefore differs from any re-read of the same row in
// the sub-µs digits on Linux (macOS's µs wall clock masks it locally).
func sameDBTime(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// TestUpdateStorageScriptByName covers script versioning: update mints a new
// immutable version (never clobbers content in place), preserves ID and
// CreatedAt, re-derives priority from the new content's headers, and 404s
// (not creates) on an unknown name.
func TestUpdateStorageScriptByName(t *testing.T) {
	s := testStore(t)

	created := &model.StorageScript{Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho old"}
	if err := s.CreateStorageScript(created); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, version, err := s.UpdateStorageScriptByName("asus-rs501a", 200, "# slagworks:priority 200\necho new")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if version != 2 {
		t.Errorf("first update should mint version 2, got %d", version)
	}
	if updated.ID != created.ID {
		t.Errorf("ID must be preserved: got %s, want %s", updated.ID, created.ID)
	}
	if !sameDBTime(updated.CreatedAt, created.CreatedAt) {
		t.Errorf("CreatedAt must be preserved: got %v, want %v", updated.CreatedAt, created.CreatedAt)
	}
	if updated.Priority != 200 {
		t.Errorf("priority not updated: got %d, want 200", updated.Priority)
	}
	if updated.Content != "# slagworks:priority 200\necho new" {
		t.Errorf("content not updated: got %q", updated.Content)
	}

	got, err := s.ListStorageScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 storage script after update (identity row not duplicated), got %d", len(got))
	}
}

func TestUpdateStorageScriptByNameNotFound(t *testing.T) {
	s := testStore(t)

	_, _, err := s.UpdateStorageScriptByName("does-not-exist", 100, "echo hi")
	if err == nil {
		t.Fatal("update of unknown name must error, not create")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("not-found error should name the script, got: %v", err)
	}

	got, err := s.ListStorageScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("update on unknown name must not create a row, got %d scripts", len(got))
	}
}

// TestUpdateStorageScriptByName_VersionSequence proves two updates on the
// same script mint versions 2 and 3 in order, the head pointer advances
// each time, and the version-1 row (the original upload) is left byte-
// identical — history, not clobber.
func TestUpdateStorageScriptByName_VersionSequence(t *testing.T) {
	s := testStore(t)

	created := &model.StorageScript{Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho v1"}
	if err := s.CreateStorageScript(created); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, v, err := s.UpdateStorageScriptByName("asus-rs501a", 150, "# slagworks:priority 150\necho v2"); err != nil {
		t.Fatalf("first update: %v", err)
	} else if v != 2 {
		t.Fatalf("first update should mint version 2, got %d", v)
	}

	head, v, err := s.UpdateStorageScriptByName("asus-rs501a", 200, "# slagworks:priority 200\necho v3")
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if v != 3 {
		t.Fatalf("second update should mint version 3, got %d", v)
	}
	if head.Priority != 200 || head.Content != "# slagworks:priority 200\necho v3" {
		t.Errorf("head should reflect the latest update, got priority=%d content=%q", head.Priority, head.Content)
	}

	versions, err := s.ListStorageScriptVersions("asus-rs501a")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
	if versions[0].Version != 1 || versions[0].Content != "# slagworks:priority 100\necho v1" {
		t.Errorf("version 1 (the original upload) must be untouched, got %+v", versions[0])
	}
	if versions[0].Head {
		t.Error("version 1 must not be marked head after two updates")
	}
	if versions[1].Version != 2 || versions[1].Content != "# slagworks:priority 150\necho v2" {
		t.Errorf("version 2 must be untouched by the third update, got %+v", versions[1])
	}
	if !versions[2].Head || versions[2].Version != 3 {
		t.Errorf("version 3 must be the head, got %+v", versions[2])
	}
}

// TestUpdateNetworkScriptByName is the network-script twin of
// TestUpdateStorageScriptByName.
func TestUpdateNetworkScriptByName(t *testing.T) {
	s := testStore(t)

	created := &model.NetworkScript{Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho old"}
	if err := s.CreateNetworkScript(created); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, version, err := s.UpdateNetworkScriptByName("pop1-l3-kworker", 50, "# slagworks:priority 50\necho new")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if version != 2 {
		t.Errorf("first update should mint version 2, got %d", version)
	}
	if updated.ID != created.ID {
		t.Errorf("ID must be preserved: got %s, want %s", updated.ID, created.ID)
	}
	if !sameDBTime(updated.CreatedAt, created.CreatedAt) {
		t.Errorf("CreatedAt must be preserved: got %v, want %v", updated.CreatedAt, created.CreatedAt)
	}
	if updated.Priority != 50 {
		t.Errorf("priority not updated: got %d, want 50", updated.Priority)
	}

	got, err := s.ListNetworkScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 network script after update (identity row not duplicated), got %d", len(got))
	}
}

func TestUpdateNetworkScriptByNameNotFound(t *testing.T) {
	s := testStore(t)

	_, _, err := s.UpdateNetworkScriptByName("does-not-exist", 100, "echo hi")
	if err == nil {
		t.Fatal("update of unknown name must error, not create")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("not-found error should name the script, got: %v", err)
	}

	got, err := s.ListNetworkScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("update on unknown name must not create a row, got %d scripts", len(got))
	}
}

// TestUpdateNetworkScriptByName_VersionSequence is the network-script twin
// of TestUpdateStorageScriptByName_VersionSequence.
func TestUpdateNetworkScriptByName_VersionSequence(t *testing.T) {
	s := testStore(t)

	created := &model.NetworkScript{Name: "pop1-l3-kworker", Priority: 100, Content: "# slagworks:priority 100\necho v1"}
	if err := s.CreateNetworkScript(created); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, v, err := s.UpdateNetworkScriptByName("pop1-l3-kworker", 150, "# slagworks:priority 150\necho v2"); err != nil {
		t.Fatalf("first update: %v", err)
	} else if v != 2 {
		t.Fatalf("first update should mint version 2, got %d", v)
	}

	_, v, err := s.UpdateNetworkScriptByName("pop1-l3-kworker", 200, "# slagworks:priority 200\necho v3")
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if v != 3 {
		t.Fatalf("second update should mint version 3, got %d", v)
	}

	versions, err := s.ListNetworkScriptVersions("pop1-l3-kworker")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
	if versions[0].Version != 1 || versions[0].Content != "# slagworks:priority 100\necho v1" {
		t.Errorf("version 1 (the original upload) must be untouched, got %+v", versions[0])
	}
	if !versions[2].Head || versions[2].Version != 3 {
		t.Errorf("version 3 must be the head, got %+v", versions[2])
	}
}

// TestListStorageScriptVersions_HeadOnly proves the migration backfill
// shape stays correct going forward: a script that has never been updated
// has exactly one version (1), marked head. This is what
// 00029_versioned_scripts.sql's backfill produces for pre-existing prod
// rows (each got a version-1 row minted from its old priority/content
// columns before those columns were dropped).
func TestListStorageScriptVersions_HeadOnly(t *testing.T) {
	s := testStore(t)

	created := &model.StorageScript{Name: "asus-rs501a", Priority: 100, Content: "# slagworks:priority 100\necho v1"}
	if err := s.CreateStorageScript(created); err != nil {
		t.Fatalf("create: %v", err)
	}

	versions, err := s.ListStorageScriptVersions("asus-rs501a")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("expected exactly 1 version for a never-updated script, got %d", len(versions))
	}
	if !versions[0].Head || versions[0].Version != 1 {
		t.Errorf("version 1 must be head for a never-updated script, got %+v", versions[0])
	}
}

func TestListStorageScriptVersions_NotFound(t *testing.T) {
	s := testStore(t)

	_, err := s.ListStorageScriptVersions("does-not-exist")
	if err == nil {
		t.Fatal("history of unknown name must error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("not-found error should name the script, got: %v", err)
	}
}
