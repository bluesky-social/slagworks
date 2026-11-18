package storageapply

import (
	"log/slog"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
)

// testCtx builds a minimal agent.Context for tests that never exec
// (the fetch/inventory seams are injected).
func testCtx(server string) *agent.Context {
	return &agent.Context{
		Log:    slog.Default(),
		Exec:   &execkit.Exec{Log: slog.Default()},
		Server: server,
	}
}

// emptyInventory satisfies ResolveLayout's liveInventory seam for tests
// whose fetch path errors before the inventory is consumed.
func emptyInventory() ([]byte, error) { return []byte("{}"), nil }
