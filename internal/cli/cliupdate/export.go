package cliupdate

import (
	"fmt"
	"time"
)

// ReadToolSummary returns a human-readable one-line summary of the
// version-discovery state for a tool. An unreadable state file reads as
// never checked; no error is surfaced.
//
// Used by clicheck's BinaryProvenanceCheck to report version state
// without importing the full ToolState type.
func ReadToolSummary(tool string) string {
	ts := readToolState(tool)
	if ts.LastCheck.IsZero() {
		return "last check: never"
	}
	ago := time.Since(ts.LastCheck).Round(time.Minute)
	age := fmt.Sprintf("last check: %s ago", ago)
	if ts.FailReason != "" {
		return fmt.Sprintf("%s (last failed: %s)", age, ts.FailReason)
	}
	if ts.LatestSeen != "" {
		return fmt.Sprintf("%s (latest: %s)", age, ts.LatestSeen)
	}
	if ts.SkipReason != "" {
		return fmt.Sprintf("%s (skipped: %s)", age, ts.SkipReason)
	}
	return age
}

// LatestSeen returns the latest release tag discovered for a tool and whether
// one is recorded. Used by clicheck to compose the provenance detail line.
func LatestSeen(tool string) (string, bool) {
	ts := readToolState(tool)
	return ts.LatestSeen, ts.LatestSeen != ""
}
