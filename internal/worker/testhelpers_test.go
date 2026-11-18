package worker

import (
	"path/filepath"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

// seedOpsResync feeds ops to the cache's op layer via an op_resync,
// deriving the transient machine-state overlay each claimed/running
// lifecycle op implies (mirrors the hub's resync derivation). Since
// operations left SiteSnapshot (Tier 2 Phase 2), tests that used to set
// snap.Operations now call this AFTER ApplySnapshot to deliver the same
// ops on the stream. The resync also clears the untrusted flag, so a
// freshly-seeded cache actuates normally.
func seedOpsResync(c *Cache, ops ...model.Operation) {
	states := map[string]model.MachineState{}
	for _, op := range ops {
		if op.Status == model.OpClaimed || op.Status == model.OpRunning {
			if target, _ := opstate.ClaimTransition(op.Type); target != "" {
				states[op.MachineID.String()] = target
			}
		}
	}
	c.ApplyOpResync(ops, states)
}

// openTestDB creates a temporary SQLite database for testing.
// Previously lived next to deploy_logs and sol_lines tests; preserved
// here for the surviving cache / executor / localdb tests.
func openTestDB(t *testing.T) *LocalDB {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	return db
}
