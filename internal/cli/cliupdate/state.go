package cliupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

// ToolState is the persisted update-check record for a tool.
type ToolState struct {
	// LastCheck is when the version-discovery check was last attempted.
	LastCheck time.Time `json:"last_check,omitempty"`
	// LastNudge is when the missing-token nudge was last shown.
	LastNudge time.Time `json:"last_nudge,omitempty"`
	// LatestSeen is the latest release tag discovered by the background child
	// (e.g. "v0.3.0"). Drives the exit-time upgrade notice and the startup
	// major-version gate. Empty means never successfully discovered.
	LatestSeen string `json:"latest_seen,omitempty"`
	// LatestSeenAt is when LatestSeen was discovered. Retained for display
	// only ("last checked N ago"); the gate does NOT use it (a stale
	// LatestSeen can only under-refuse, never over-refuse).
	LatestSeenAt time.Time `json:"latest_seen_at,omitempty"`
	// LastUpgradeNudge is when the "vX available" notice was last shown.
	// Rate-limits that notice to nudgeInterval.
	LastUpgradeNudge time.Time `json:"last_upgrade_nudge,omitempty"`
	// SkipReason is the skip code from the last check ("no-token",
	// "no-release"), or empty if the last check discovered a version.
	SkipReason string `json:"skip_reason,omitempty"`
	// FailReason describes the last failure (non-empty means the check failed).
	FailReason string `json:"fail_reason,omitempty"`
}

// StateDir returns the directory holding tool's update-check state and its
// lock: <XDG state home>/<tool>, ~/.local/state/<tool> by default. The state
// is machine-local and safe to delete; the next check rebuilds it.
func StateDir(tool string) (string, error) {
	return userpath.StateDir(tool)
}

// stateFilePath returns the path to the update-check state file.
func stateFilePath(tool string) (string, error) {
	return userpath.StateFile(tool, "update-check.json")
}

// stateLockPath returns the path to the flock file that guards the state file.
func stateLockPath(tool string) (string, error) {
	return userpath.StateFile(tool, "update-check.lock")
}

// acquireStateLock acquires a blocking exclusive flock on the state file lock.
// Returns a release function (always non-nil) and whether the lock was acquired.
// On error, returns a no-op release and false — the caller should proceed
// without the lock rather than failing.
func acquireStateLock(tool string) (release func(), ok bool) {
	lockPath, err := stateLockPath(tool)
	if err != nil {
		return func() {}, false
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return func() {}, false
	}
	fl := flock.New(lockPath)
	// Blocking lock: wait for any concurrent writer to finish. Lock duration
	// is a JSON encode + disk write — typically <1ms.
	if err := fl.Lock(); err != nil {
		return func() {}, false
	}
	return func() { _ = fl.Unlock() }, true
}

// readToolState loads the state file. Returns the zero value on any error
// (missing file, corrupt JSON, etc.) — the caller should treat that as
// never-checked. Callers that need write-safety must hold acquireStateLock.
func readToolState(tool string) ToolState {
	path, err := stateFilePath(tool)
	if err != nil {
		return ToolState{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ToolState{}
	}
	var ts ToolState
	if err := json.Unmarshal(data, &ts); err != nil {
		return ToolState{}
	}
	return ts
}

// writeToolState persists the state atomically (temp file + rename).
// Callers that need write-safety must hold acquireStateLock.
func writeToolState(tool string, ts ToolState) error {
	path, err := stateFilePath(tool)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("marshalling state: %w", err)
	}
	return userpath.WriteFile(path, data)
}

// updateToolState holds the state-file lock, reads the current state,
// applies fn, and persists the result.
func updateToolState(tool string, fn func(*ToolState)) error {
	release, _ := acquireStateLock(tool)
	defer release()

	ts := readToolState(tool)
	fn(&ts)
	if err := writeToolState(tool, ts); err != nil {
		return fmt.Errorf("writing update-check state for %q: %w", tool, err)
	}
	return nil
}
