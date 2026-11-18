package cliupdate

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Masterminds/semver/v3"
)

// Hook holds the parent-side state for one tool's update hook.
type Hook struct {
	cfg Config
	// ts is the tool state loaded once at construction.
	ts ToolState
}

// NewHook creates a Hook and loads the current tool state.
// Construction never fails: read errors are treated as "never checked."
func NewHook(cfg Config) *Hook {
	return &Hook{
		cfg: cfg,
		ts:  readToolState(cfg.Tool),
	}
}

// PrintPendingNotice writes at most one line to w, in priority order:
//   - If a newer release is available (LatestSeen > CurrentVersion), the
//     upgrade notice — rate-limited to nudgeInterval.
//   - Else if the repo is private and the last check found no GitHub token,
//     the token nudge — rate-limited to nudgeInterval.
//
// After printing, marks the relevant state so the notice is not repeated
// before its interval elapses.
func (h *Hook) PrintPendingNotice(w io.Writer) {
	// The helper child runs with stdio to /dev/null; skip printing so it
	// doesn't consume (and discard) notices the parent should show on the next
	// user-facing invocation.
	if os.Getenv(envHelperGuard) == "1" {
		return
	}
	ts := h.ts
	tool := h.cfg.Tool

	// Upgrade notice: a newer release exists. The major-version gate (startup)
	// already refuses when a full major behind, so in practice this surfaces
	// minor/patch upgrades; it also covers same-major upgrades on a binary the
	// gate let through.
	if h.upgradeAvailable() {
		if time.Since(ts.LastUpgradeNudge) >= nudgeInterval {
			_, _ = fmt.Fprintf(w,
				"%s: %s available (you have %s) — run '%s'\n",
				tool, ts.LatestSeen, h.cfg.CurrentVersion, h.cfg.UpgradeCommand)
			_ = updateToolState(tool, func(t *ToolState) {
				t.LastUpgradeNudge = time.Now()
			})
		}
		return
	}

	if h.cfg.Visibility == RepoPrivate && ts.SkipReason == "no-token" {
		if time.Since(ts.LastNudge) >= nudgeInterval {
			_, _ = fmt.Fprintf(w,
				"%s: GitHub token required for update checks — run 'gh auth login', or set GITHUB_TOKEN (create: %s)\n",
				tool, tokenNudgeURL(tool))
			_ = updateToolState(tool, func(t *ToolState) {
				t.LastNudge = time.Now()
			})
		}
	}
}

// upgradeAvailable reports whether LatestSeen is a strictly newer version than
// CurrentVersion. Dev builds and unparseable/empty versions never nag.
func (h *Hook) upgradeAvailable() bool {
	cur := h.cfg.CurrentVersion
	if cur == "" || cur == "dev" || h.ts.LatestSeen == "" {
		return false
	}
	curV, err1 := semver.NewVersion(cur)
	latestV, err2 := semver.NewVersion(h.ts.LatestSeen)
	if err1 != nil || err2 != nil {
		return false
	}
	return latestV.GreaterThan(curV)
}

// ShouldSpawnChild returns true when the 24h check is due and the detached
// helper should be spawned.
func (h *Hook) ShouldSpawnChild() bool {
	if shouldSkip() {
		return false
	}
	// The helper guard prevents the child's own exit hook from re-spawning.
	if os.Getenv(envHelperGuard) == "1" {
		return false
	}
	return time.Since(h.ts.LastCheck) >= checkInterval
}

// MaybeSpawnChild spawns the detached helper child when ShouldSpawnChild
// returns true. exePath is the path to the running binary (pass os.Args[0] or
// the os.Executable result). The parent never waits for the child.
func (h *Hook) MaybeSpawnChild(exePath string) {
	if !h.ShouldSpawnChild() {
		return
	}
	spawnDetached(exePath, h.cfg.Tool)
}

// envHelperGuard is set in the child environment to prevent the helper's own
// exit hook from spawning more children when the helper is running.
const envHelperGuard = "BSKY_CLI_UPDATE_HELPER"

// spawnDetached forks <exePath> internal-update-helper <tool> in a new
// session. Stdin, stdout and stderr are left nil, so the child reads from and
// writes to the null device.
func spawnDetached(exePath, tool string) {
	cmd := exec.Command(exePath, HelperCommandName, tool)
	cmd.Env = append(os.Environ(), envHelperGuard+"=1")
	// New session so the child outlives the parent's terminal session
	// and a SIGINT to the parent doesn't propagate.
	cmd.SysProcAttr = detachedSysProcAttr()
	// Do not Wait; parent exits immediately.
	_ = cmd.Start()
}

// shouldSkip returns true when the background check must not run.
func shouldSkip() bool {
	if os.Getenv("BSKY_CLI_NO_AUTOUPDATE") == "1" {
		return true
	}
	if os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true" {
		return true
	}
	// Skip when running under `go run` — the binary is a temp shim, not a
	// regular installed file. Detect via os.Executable + os.Lstat.
	exe, err := os.Executable()
	if err != nil {
		return true
	}
	fi, err := os.Lstat(exe)
	if err != nil {
		return true
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// Symlinked install: the child resolves and handles it.
		return false
	}
	if !fi.Mode().IsRegular() {
		// Not a regular file and not a symlink — likely a `go run` shim.
		return true
	}
	return false
}
