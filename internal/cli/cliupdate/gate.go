package cliupdate

import (
	"fmt"
	"os"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/internal/cli/clierr"
)

// gateExitCode is the process exit code when the major-version gate refuses to
// run. Distinct from the generic failure codes so scripts and agents can tell
// "refused: too old" apart from a normal error.
const gateExitCode = 3

// envNoVersionGate disables the refuse-to-run gate for one invocation. The
// escape hatch for "I know this build is old; the new server isn't deployed
// yet." Does not affect the background check or the notice.
const envNoVersionGate = "BSKY_CLI_NO_VERSION_GATE"

// gateExemptCommands are leaf command names that must run even on a binary
// that is a major version behind — they are how a user recovers or inspects a
// stale binary. Matched against cobra's leaf command Name().
var gateExemptCommands = map[string]bool{
	HelperCommandName:  true, // background child must run to refresh state
	"doctor":           true, // diagnosis
	"version":          true,
	"help":             true,
	"completion":       true,
	"__complete":       true, // cobra's shell-completion callback
	"__completeNoDesc": true,
}

// EnforceMajorGate refuses to run when the current binary is a full semver
// major version behind the latest release we last discovered. It is
// network-free: it reads LatestSeen from the local state file (populated by a
// prior background child), so it adds no latency and works offline.
//
// leaf is the cobra command about to run (root.ExecuteC's target, or the value
// passed to a PersistentPreRunE). Exempt commands (helper, doctor, version,
// help, completion) always proceed.
//
// Behavior matrix:
//   - dev/empty CurrentVersion         → proceed (dev builds are never gated)
//   - env override set                 → proceed
//   - LatestSeen empty (never checked) → indeterminate: FailClosedOnUnknown
//     decides (default false = proceed)
//   - major(LatestSeen) > major(current) → refuse (exit gateExitCode)
//   - otherwise                        → proceed
//
// A non-empty LatestSeen is trusted regardless of age: a stale value can only
// be behind the true latest, so it can only under-refuse, never wrongly refuse
// a binary that is actually current.
func (h *Hook) EnforceMajorGate(leaf *cobra.Command) error {
	if leaf != nil && gateExemptCommands[leaf.Name()] {
		return nil
	}
	if os.Getenv(envNoVersionGate) == "1" {
		return nil
	}

	cur := h.cfg.CurrentVersion
	if cur == "" || cur == "dev" {
		return nil
	}

	latest := h.ts.LatestSeen
	if latest == "" {
		// Indeterminate: we have never successfully discovered a latest
		// version. Governed by the compile-time posture.
		if h.cfg.FailClosedOnUnknown {
			return h.gateRefusal(cur, "unknown (could not reach GitHub)")
		}
		return nil
	}

	curV, err1 := semver.NewVersion(cur)
	latestV, err2 := semver.NewVersion(latest)
	if err1 != nil || err2 != nil {
		// Unparseable version on either side — don't brick on a parse error.
		// This is an indeterminate case; honor the posture.
		if h.cfg.FailClosedOnUnknown {
			return h.gateRefusal(cur, latest+" (version parse failed)")
		}
		return nil
	}

	if latestV.Major() > curV.Major() {
		return h.gateRefusal(cur, latest)
	}
	return nil
}

// gateRefusal builds the user-facing refuse-to-run error, wrapped with the
// gate exit code so clierr.Code surfaces it.
func (h *Hook) gateRefusal(current, latest string) error {
	err := fmt.Errorf(
		"%s build %s is a major version behind the latest release (%s) and may be incompatible; update with '%s' (or set %s=1 to override)",
		h.cfg.Tool, current, latest, h.cfg.UpgradeCommand, envNoVersionGate)
	return clierr.WithExit(err, gateExitCode)
}
