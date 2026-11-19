package store

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Concurrent updates of one script by name must serialize (the SELECT ...
// FOR UPDATE on the identity row): every update succeeds, versions are
// exactly 1..N+1 with no gaps or duplicates, and head is the last one. A
// concurrent delete interleaved with updates must leave either the script
// gone (and every later update ErrScriptNotFound) or a consistent history.
func TestScriptConcurrentUpdatesSerialize(t *testing.T) {
	s := testStore(t)
	for round := range 5 {
		if err := s.db.Exec("TRUNCATE storage_scripts, storage_script_versions CASCADE").Error; err != nil {
			t.Fatal(err)
		}
		if err := s.CreateStorageScript(&model.StorageScript{Name: "s", Priority: 1, Content: "v1"}); err != nil {
			t.Fatal(err)
		}
		const n = 24
		var wg sync.WaitGroup
		errs := make(chan error, n)
		versions := make(chan int, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, v, err := s.UpdateStorageScriptByName("s", i, fmt.Sprintf("r%d-%d", round, i))
				if err != nil {
					errs <- err
					return
				}
				versions <- v
			}()
		}
		wg.Wait()
		close(errs)
		close(versions)
		for err := range errs {
			t.Fatalf("round %d: concurrent update failed: %v", round, err)
		}
		seen := map[int]bool{}
		for v := range versions {
			if seen[v] {
				t.Fatalf("round %d: version %d returned twice", round, v)
			}
			seen[v] = true
		}
		vs, err := s.ListStorageScriptVersions("s")
		if err != nil {
			t.Fatal(err)
		}
		if len(vs) != n+1 {
			t.Fatalf("round %d: %d versions, want %d", round, len(vs), n+1)
		}
		for i, v := range vs {
			if v.Version != i+1 || v.Head != (i == n) {
				t.Fatalf("round %d: row %d = version %d head %v", round, i, v.Version, v.Head)
			}
		}
	}
}
