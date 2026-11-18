// Package cliupdate is slagctl's background release check. It never replaces
// the binary: it discovers the latest GitHub release in a detached child
// process, prints an upgrade notice on exit when a newer release exists, and
// refuses to run when the binary is a full major version behind.
//
// The caller wires in three pieces.
//
// Startup gate (in PersistentPreRunE, before the command body runs):
//
//	hook := cliupdate.NewHook(cfg)
//	if err := hook.EnforceMajorGate(cmd); err != nil { return err }
//
// Parent side (after the command finishes):
//
//	hook.PrintPendingNotice(os.Stderr) // may print one line (upgrade notice / token nudge)
//	hook.MaybeSpawnChild(os.Args[0])   // forks the detached child when a check is due
//
// Child side: the hidden command from NewHelperCommand, which calls RunHelper.
//
// The child queries the GitHub Releases API for the latest version and
// records it in a state file under the user's state directory
// (~/.local/state/<tool>/update-check.json). The parent reads the state file
// on the next invocation to drive the gate and the notice.
package cliupdate

import "time"

// RepoVisibility controls how token resolution and nudging behave.
type RepoVisibility int

const (
	// RepoPrivate means the GitHub release requires a token to see.
	// Token resolution failures trigger the weekly token nudge.
	RepoPrivate RepoVisibility = iota
	// RepoPublic means releases are visible unauthenticated. A token is used
	// if present (better API rate limits), but its absence is not a nudge.
	RepoPublic
)

// Config describes a tool's update configuration. Every field must be set by
// the caller; the package has no defaults that name a repository.
type Config struct {
	// Owner is the GitHub organization or user owning the repo.
	Owner string
	// Repo is the repository whose releases are checked.
	Repo string
	// Tool is the binary name. It keys the state directory and appears in
	// user-facing notices.
	Tool string
	// Visibility controls token resolution and nudging. See RepoVisibility.
	Visibility RepoVisibility
	// UpgradeCommand is what the upgrade notice, the gate refusal and doctor
	// tell the operator to run to get the latest release (e.g. a package
	// manager invocation).
	UpgradeCommand string
	// CurrentVersion is the running binary's version string (e.g. "v0.4.1" or
	// "dev"), as stamped into the binary at build time.
	CurrentVersion string
	// FailClosedOnUnknown controls the major-version gate's behavior when the
	// latest version cannot be determined (never checked, offline, no token,
	// unparseable). false (the default) = fail open: run normally. true = fail
	// closed: refuse to run. Only governs the indeterminate case — a definite
	// "you are a major behind" always refuses regardless of this flag.
	FailClosedOnUnknown bool
}

// checkInterval is how long between update checks per tool.
const checkInterval = 24 * time.Hour

// nudgeInterval is how long between repeats of the upgrade notice and of the
// missing-token nudge.
const nudgeInterval = 7 * 24 * time.Hour

// HelperCommandName is the hidden sub-command name that invokes child mode.
// Wire this into the tool's root command as a hidden sub-command.
const HelperCommandName = "internal-update-helper"

// tokenNudgeURL is the classic-token creation URL with prefilled scopes.
func tokenNudgeURL(tool string) string {
	return "https://github.com/settings/tokens/new?scopes=repo&description=" + tool + "-update-check"
}
