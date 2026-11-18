package clicheck

import (
	"context"
	"fmt"
	"os"

	"github.com/Masterminds/semver/v3"

	"github.com/bluesky-social/slagworks/internal/cli/cliupdate"
)

// BinaryProvenanceCheck returns a Check that reports whether the running
// binary is a dev build or a release build, and (for release builds) whether
// a newer version is available per the last background discovery.
//
// cfg.Tool locates the discovery state, cfg.CurrentVersion is the binary's
// own build-stamped version, and cfg.UpgradeCommand is what the detail tells
// the operator to run when a newer release exists.
func BinaryProvenanceCheck(cfg cliupdate.Config) Check {
	return Check{
		Name: "binary-provenance",
		Run: func(_ context.Context) Result {
			version := cfg.CurrentVersion
			if version == "dev" || version == "" {
				return Warn("dev build — version gate + update nag inactive")
			}
			result := OK(fmt.Sprintf("%s; %s", version, cliupdate.ReadToolSummary(cfg.Tool)))
			latest, ok := cliupdate.LatestSeen(cfg.Tool)
			if !ok {
				return result
			}
			current, err := semver.NewVersion(version)
			if err != nil {
				return result
			}
			available, err := semver.NewVersion(latest)
			if err != nil || !available.GreaterThan(current) {
				return result
			}
			return OK(fmt.Sprintf("%s; %s available — run '%s'", version, latest, cfg.UpgradeCommand))
		},
	}
}

// StateDirWritableCheck returns a Check that verifies the tool's update-check
// state directory (~/.local/state/<tool>/ by default) is writable by creating
// and deleting a probe temp file.
func StateDirWritableCheck(tool string) Check {
	return Check{
		Name: "state-dir-writable",
		Run: func(_ context.Context) Result {
			dir, err := cliupdate.StateDir(tool)
			if err != nil {
				return Fail(fmt.Sprintf("resolve state dir: %v", err))
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return Fail(fmt.Sprintf("create %s: %v", dir, err))
			}
			f, err := os.CreateTemp(dir, ".doctor-probe-*")
			if err != nil {
				return Fail(fmt.Sprintf("%s not writable: %v", dir, err))
			}
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return OK(dir)
		},
	}
}

// UpdateChecks returns the checks that depend only on the tool's update
// configuration: binary provenance and state-dir writability. Doctor
// commands append their own credential and connectivity checks.
func UpdateChecks(cfg cliupdate.Config) []Check {
	return []Check{
		BinaryProvenanceCheck(cfg),
		StateDirWritableCheck(cfg.Tool),
	}
}
