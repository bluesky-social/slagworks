package store

import (
	"os"
	"testing"

	"github.com/bluesky-social/slagworks/internal/testpg"
)

// testStore returns a *Store backed by the shared dockertest Postgres
// in internal/testpg, with all tables truncated so each test starts
// clean. Wraps testpg.DB so consumers keep the short call shape.
func testStore(t *testing.T) *Store {
	t.Helper()
	db := testpg.DB(t, Migrate)
	return New(db)
}

// TestMain ensures the shared Postgres container is purged when the
// store package's tests finish. Without this, the container leaks
// until Docker reaps it via testpg's Expire(3600) safety net.
func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}
