package main

import (
	"fmt"
	"log/slog"
	"sync"
)

// exitOpInactive is the agent's exit status when it stops because the site
// worker reports its operation is over. It differs from a fatal error's 1 so
// the boot hook's "agent exited with code N" line on the serial console
// tells an aborted run from a failed one. Like every nonzero status it makes
// the hook hold the host in the recovery environment instead of rebooting.
const exitOpInactive = 3

// runControl is how a deploy or commission run reports progress and how it
// ends early. It owns the three ways out short of success: fatal (the run
// failed), halt (the operation was taken away) and the SLAGWORKS_FATAL_AT
// test hook. Each ends in exit, which is os.Exit in production and never
// returns; tests substitute a function that panics.
type runControl struct {
	log *slog.Logger
	// reporter is nil when the agent has no machine and operation identity.
	reporter *agentReporter
	// stopProcs kills every process the run has started, with its children,
	// and refuses new ones (execkit.Group.Stop on the agent's one group).
	stopProcs func()
	// flushLogs gives queued log envelopes a bounded chance to reach the
	// site worker before the process exits. May be nil.
	flushLogs func()
	// done stops the reporter's flush loop and the dmesg reader.
	done chan struct{}
	// fatalAtPhase is SLAGWORKS_FATAL_AT: a TEST-ONLY knob. When set to a
	// phase name (e.g. "installing_bootloader"), setPhase fails the run the
	// moment it enters that phase. The integration suite uses it to
	// exercise the recovery-sshd hook against a deliberately failed
	// late-stage agent without engineering a real fault. Unset in
	// production.
	fatalAtPhase string
	exit         func(code int)

	// ending is taken by whichever of fatal, halt and the FATAL_AT hook
	// runs first and is never released: the holder is exiting the process.
	// A second caller blocks on it instead of racing the first to exit,
	// which is what keeps a command halt just killed from being reported as
	// the run's own failure.
	ending    sync.Mutex
	closeDone sync.Once
}

// setPhase reports a phase to the site worker. A report answered with "the
// operation is over" does not return here: the reporter calls halt.
func (c *runControl) setPhase(phase string) {
	if c.reporter != nil {
		c.reporter.ReportPhase(phase)
		if c.reporter.opInactive() {
			// The flush loop's report drew the answer first and halt is
			// already running there. Wait in it instead of returning into
			// the phase's work.
			c.halt()
		}
	}
	if c.fatalAtPhase != "" && phase == c.fatalAtPhase {
		c.log.Error(fmt.Sprintf("SLAGWORKS_FATAL_AT=%s honored, exiting non-zero", phase))
		if c.reporter != nil {
			c.reporter.ReportFailure(phase, "SLAGWORKS_FATAL_AT="+phase)
		}
		c.ending.Lock()
		c.end(1)
	}
}

// fatal fails the run: it reports the failure, which fails the operation at
// the site worker, and exits nonzero so the boot hook drops to recovery.
func (c *runControl) fatal(stage string, err error) {
	if c.reporter != nil && c.reporter.opInactive() {
		// The operation was taken away and halt is ending the run. The
		// error in hand is most likely the command halt killed; reporting
		// it would blame the run for the abort.
		c.halt()
	}
	msg := fmt.Sprintf("%s: %v", stage, err)
	c.log.Error("FATAL during "+stage, "err", err)
	if c.reporter != nil {
		c.reporter.ReportFailure(stage, msg)
	}
	c.ending.Lock()
	c.end(1)
}

// halt stops the run because the site worker answered a report with "this
// operation is over": an operator aborted it, a timeout sweep failed it, or
// it failed some other way. Nobody is waiting for the install any more, so
// the agent stops changing the host at once: the processes it started are
// killed (commands in flight, config scripts, the tailscale daemon), no boot
// entry is registered, nothing more is written to the BMC, and the host is
// not rebooted into a half-written disk. Exiting nonzero
// makes the boot hook hold the host in the recovery environment (the same
// place a failed run ends up) until an operator or the next operation's
// power cycle takes it from there.
//
// It is the reporter's onInactive, so it runs on whichever goroutine's
// report drew the answer: the run itself at a phase boundary, or the flush
// loop's liveness report in the middle of a long command.
func (c *runControl) halt() {
	c.ending.Lock()
	// Kill first: everything after this line only explains the stop.
	if c.stopProcs != nil {
		c.stopProcs()
	}
	c.log.Error("the site worker reports this operation is no longer active (aborted, failed or timed out); " +
		"stopping without further changes to the host and without rebooting. " +
		"The host stays in the recovery environment; start a new operation to reuse it")
	if c.flushLogs != nil {
		c.flushLogs()
	}
	c.end(exitOpInactive)
}

// end stops the background reporters and exits. The caller holds ending.
func (c *runControl) end(code int) {
	c.closeDone.Do(func() { close(c.done) })
	c.exit(code)
}
