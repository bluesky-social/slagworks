package cliupdate

import (
	"context"
	"fmt"
	"time"
)

// RunHelper is the child-side entry point. It is invoked when the tool is
// called with the hidden HelperCommandName sub-command. When the 24h check is
// due it resolves a GitHub token, discovers the latest release version, and
// records the outcome in the state file.
//
// All outcomes are written to the state file — the parent reads it and
// reports on the next invocation. Nothing is printed; the process is running
// detached with stdio to /dev/null.
func RunHelper(cfg Config) {
	runHelper(cfg, discoverLatest)
}

// discoverFunc is the type of the version-discovery function. Swapped in tests
// to point at a fake GitHub server.
type discoverFunc func(ctx context.Context, cfg Config, token string) (latest string, found bool, err error)

// runHelper is the testable core of RunHelper.
func runHelper(cfg Config, discover discoverFunc) {
	// The parent decided to spawn from a state read that may be stale by now:
	// another invocation's child can have finished a check in between.
	// Re-check so back-to-back invocations don't each query the API.
	if time.Since(readToolState(cfg.Tool).LastCheck) < checkInterval {
		return
	}

	token, hasToken := resolveToken()

	// Private repos need a token to see releases; without one, record the skip
	// so the parent can emit the weekly token nudge.
	if !hasToken && cfg.Visibility == RepoPrivate {
		_ = updateToolState(cfg.Tool, func(t *ToolState) {
			t.LastCheck = time.Now()
			t.SkipReason = "no-token"
			t.FailReason = ""
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()

	latest, found, err := discover(ctx, cfg, token)
	now := time.Now()
	if err != nil {
		_ = updateToolState(cfg.Tool, func(t *ToolState) {
			t.LastCheck = now
			t.FailReason = fmt.Sprintf("discover: %s", err)
			t.SkipReason = ""
		})
		return
	}
	if !found {
		_ = updateToolState(cfg.Tool, func(t *ToolState) {
			t.LastCheck = now
			t.SkipReason = "no-release"
			t.FailReason = ""
		})
		return
	}
	_ = updateToolState(cfg.Tool, func(t *ToolState) {
		t.LastCheck = now
		t.LatestSeen = latest
		t.LatestSeenAt = now
		t.SkipReason = ""
		t.FailReason = ""
	})
}
