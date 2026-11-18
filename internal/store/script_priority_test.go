package store

import (
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestNetworkScriptsShareePriority is the regression for the priority
// unique-index bug (migration 00021): two network scripts that match disjoint
// host sets must be allowed to share a priority. configscript.Match treats
// priority as a tiebreaker (sort desc, first matching rule-set wins), so a
// global UNIQUE constraint on priority is wrong — it rejected the pop2
// hot-spare L2 bond script because the pop1 L3-kworker script already held
// priority 100. Both must persist and both must be listable.
func TestNetworkScriptsSharePriority(t *testing.T) {
	s := testStore(t)

	first := &model.NetworkScript{Name: "pop1-l3-kworker", Priority: 100, Content: "# a"}
	if err := s.CreateNetworkScript(first); err != nil {
		t.Fatalf("create first network script: %v", err)
	}
	second := &model.NetworkScript{Name: "hot-spare-pop2", Priority: 100, Content: "# b"}
	if err := s.CreateNetworkScript(second); err != nil {
		t.Fatalf("second network script at same priority must be allowed, got: %v", err)
	}

	got, err := s.ListNetworkScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 network scripts at priority 100, got %d", len(got))
	}

	// Name IS still unique: a duplicate name is rejected, and the error names
	// the name (not priority).
	dup := &model.NetworkScript{Name: "pop1-l3-kworker", Priority: 200, Content: "# c"}
	err = s.CreateNetworkScript(dup)
	if err == nil {
		t.Fatal("duplicate network script name must be rejected")
	}
	if !strings.Contains(err.Error(), "pop1-l3-kworker") {
		t.Errorf("duplicate-name error should name the script, got: %v", err)
	}
}

// TestStorageScriptsSharePriority is the same regression for storage scripts,
// which carried the identical unique-priority bug.
func TestStorageScriptsSharePriority(t *testing.T) {
	s := testStore(t)

	first := &model.StorageScript{Name: "asus-rs501a", Priority: 100, Content: "# a"}
	if err := s.CreateStorageScript(first); err != nil {
		t.Fatalf("create first storage script: %v", err)
	}
	second := &model.StorageScript{Name: "another-box", Priority: 100, Content: "# b"}
	if err := s.CreateStorageScript(second); err != nil {
		t.Fatalf("second storage script at same priority must be allowed, got: %v", err)
	}

	got, err := s.ListStorageScripts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 storage scripts at priority 100, got %d", len(got))
	}
}
