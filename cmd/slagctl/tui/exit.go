package tui

// This file holds the hold-on-terminal-state decision (plan D5 + the
// asymmetric exit-behavior rule): a pure function of (terminal state,
// interactivity, flags), extracted from the program runner so it's testable
// without a tea.Program. The process exit code is just "non-zero iff failed"
// and lives at the call site.

// HoldPolicy is the inputs to the hold decision.
type HoldPolicy struct {
	Failed      bool // op reached a failure terminal (kept for the caller's exit code; no longer gates the hold)
	Interactive bool // stdin is a TTY AND not a detected non-interactive env (CI)
	ForceHold   bool // --hold: force a hold even under a non-interactive invocation
	ForceNoHold bool // --no-hold: never hold, even interactively
}

// decideHold resolves the exit behavior. The operator's rule: an interactive
// human at a TTY explicitly quits the monitor — the screen does NOT vanish on
// its own when the op finishes (you lose the final lifecycle/summary, and a
// completed deploy disappearing out from under you is jarring). So:
//
//   - interactive (TTY, not CI): HOLD on ANY terminal state, success or
//     failure, until the operator presses q. This is the common case.
//   - non-interactive (piped / CI / a PTY wrapper with no human): auto-exit on
//     any terminal state, so a script invoking -w never hangs forever on a
//     keypress that won't come.
//   - --no-hold: force immediate teardown even interactively (a human who
//     wants the old auto-exit, or to read the exit code + stderr and move on).
//   - --hold: force a hold even non-interactively (record a run under CI).
//
// --no-hold wins over --hold if both are set (the conservative "don't block"
// choice — a caller that asked not to hang must not hang).
func decideHold(p HoldPolicy) bool {
	if p.ForceNoHold {
		return false
	}
	if p.ForceHold {
		return true
	}
	return p.Interactive
}
