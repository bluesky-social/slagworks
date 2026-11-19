package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// This file is the lifecycle pane: the vertical phase checklist to the right
// of the SOL console. It is a pure state machine over the op's phase spine
// (deploy or commission) — fed phase observations off the logs envelope stream
// (phase is a field on site/agent/global envelopes, not a source — plan D3),
// it derives each phase's status and duration and renders the runway as plain
// text. Color, the amber stuck-phase highlight (S2), and the border box are
// composition-layer concerns and land with the composed model, not here —
// matching the console pane's "plain screen, styling deferred" split.
//
// The spine is rendered in full from the start (plan O5) so the operator sees
// the whole runway, not just where the op has been. Phases arrive roughly in
// order; a jump past an unseen phase backfills it as "skipped" rather than
// leaving it pending forever (handles the conditional bmc_reconcile /
// console_probe phases without a special case).

// terminalCompleted is the spine's terminal success phase. It is observed like
// any other phase but never becomes the "current" (spinning) phase — reaching
// it finalizes the prior phase and marks itself done.
const terminalCompleted = "completed"

// agentHandoffPhase is the last phase the SITE WORKER emits during the boot
// prefix; every phase after it is emitted by the on-machine agent once it
// boots and phones home. While the current phase is this one, progress depends
// on the agent making its first report — so a current phase stuck here for a
// while means "agent booted (iPXE fetched its script) but hasn't reported in".
// Both spines share this boundary (the boot prefix is identical). The TUI
// surfaces an explicit "awaiting agent report (elapsed)" state at this phase so
// a commission that boots but stalls before its first report shows a live
// clock, not a frozen all-pending spine.
const agentHandoffPhase = "ipxe_loaded"

// deploySpine is the authoritative deploy phase order, traced from the
// emission points (plan, verified 2026-05-29). Phases 11-12 (bmc_reconcile,
// awaiting_phone_home) are the part the dead knownDeployPhases list got wrong.
// bmc_reconcile is conditional (skipped when the agent has no reporter); the
// skip falls out of the backfill rule.
var deploySpine = []string{
	"power_cycle",
	"pxe_booting",
	"dhcp_ack",
	"ipxe_dhcp",
	"ipxe_loaded",
	"agent_starting",
	"partitioning",
	"rsyncing_os",
	"installing_bootloader",
	"finalizing",
	"bmc_reconcile",
	"awaiting_phone_home",
	terminalCompleted,
}

// commissionSpine shares the boot-up prefix (phases 1-6) with deploy, then
// diverges at the agent (plan O3). console_probe is conditional (skipped when
// machineID == "", the debug/new-hardware path); same skip handling as
// bmc_reconcile.
var commissionSpine = []string{
	"power_cycle",
	"pxe_booting",
	"dhcp_ack",
	"ipxe_dhcp",
	"ipxe_loaded",
	"agent_starting",
	"collecting_inventory",
	"reporting",
	"console_probe",
	terminalCompleted,
}

// bmcReconcileSpine is the remote BMC account reconcile: no boot, one
// worker-emitted phase, then completion (finalized by global from the
// worker's report).
var bmcReconcileSpine = []string{
	"bmc_reconcile",
	terminalCompleted,
}

// spineFor returns the phase spine for an op type. Deploy, commission, and
// bmc_reconcile have their own spines (power ops don't move through
// phases); anything else falls back to the deploy spine so the pane
// degrades to "render what we know" rather than panicking.
func spineFor(t model.OperationType) []string {
	switch t {
	case model.OpCommission:
		return commissionSpine
	case model.OpBMCReconcile:
		return bmcReconcileSpine
	default:
		return deploySpine
	}
}

type phaseStatus int

const (
	phasePending phaseStatus = iota // not yet reached
	phaseCurrent                    // running now (spinner)
	phaseDone                       // completed, with duration
	phaseSkipped                    // jumped past without observing
	phaseFailed                     // the op failed in this phase
)

type phaseRow struct {
	name     string
	status   phaseStatus
	arrived  time.Time     // first-observed time; zero unless reached
	duration time.Duration // set when the phase is finalized (done/failed)
}

// lifecyclePane tracks phase progress for one operation. Like consolePane it
// is single-owned by the update loop; no mutex. now is the injectable clock
// (plan D9) — only the current phase's live elapsed reads it; finished phases'
// durations come from arrival deltas, so they're stable.
type lifecyclePane struct {
	rows        []phaseRow
	index       map[string]int // phase name -> spine position
	curIdx      int            // current (spinning) phase, or -1
	highWater   int            // furthest spine index reached
	failed      bool
	failedPhase string // name of the phase Fail() landed on, "" if none
	now         func() time.Time
}

// newLifecyclePane builds a pane with the default wall clock. Tests override
// .now directly; the model injects at construction via newLifecyclePaneClock.
func newLifecyclePane(opType model.OperationType) *lifecyclePane {
	return newLifecyclePaneClock(opType, time.Now)
}

// newLifecyclePaneClock builds a pane reading time from now (plan D9 seam).
func newLifecyclePaneClock(opType model.OperationType, now func() time.Time) *lifecyclePane {
	if now == nil {
		now = time.Now
	}
	spine := spineFor(opType)
	rows := make([]phaseRow, len(spine))
	index := make(map[string]int, len(spine))
	for i, name := range spine {
		rows[i] = phaseRow{name: name, status: phasePending}
		index[name] = i
	}
	return &lifecyclePane{
		rows:      rows,
		index:     index,
		curIdx:    -1,
		highWater: -1,
		now:       now,
	}
}

// Observe records a phase arrival at time at. Out-of-order or duplicate
// observations behind the high-water mark are ignored; a jump forward
// finalizes the prior phase (deriving its duration) and backfills any unseen
// earlier phases as skipped. Unknown phases are ignored — the spine is the
// authority on what's renderable.
//
// Once the op has failed the spine is final and observations are ignored: a
// held screen keeps receiving envelopes, and a phase reported after the
// failure would otherwise start spinning under the FAILED header and
// skip-mark the runway Fail leaves pending. Completion needs no such guard:
// observing terminalCompleted leaves no pending phase for a later one to claim.
func (l *lifecyclePane) Observe(phase string, at time.Time) {
	if l.failed {
		return
	}
	idx, ok := l.index[phase]
	if !ok {
		return
	}
	// Already seen this slot (duplicate current, or a stale re-report behind
	// us): no-op. Re-finalizing would zero a real duration.
	if l.rows[idx].status != phasePending {
		return
	}

	// Finalize the phase we're leaving.
	if l.curIdx >= 0 {
		cur := &l.rows[l.curIdx]
		cur.status = phaseDone
		cur.duration = at.Sub(cur.arrived).Round(time.Second)
	}

	// Backfill: any still-pending spine phase before this one was jumped over.
	for i := range idx {
		if l.rows[i].status == phasePending {
			l.rows[i].status = phaseSkipped
		}
	}

	l.rows[idx].arrived = at
	if phase == terminalCompleted {
		// Terminal success never spins; it's done the moment it arrives.
		l.rows[idx].status = phaseDone
		l.curIdx = -1
	} else {
		l.rows[idx].status = phaseCurrent
		l.curIdx = idx
	}
	if idx > l.highWater {
		l.highWater = idx
	}
}

// CurrentPhase returns the name of the spinning phase, or "" if none is
// current (not yet started, completed, or failed). The composed frame uses it
// for the summary box's "phase" / "since" rows.
func (l *lifecyclePane) CurrentPhase() string {
	if l.curIdx < 0 {
		return ""
	}
	return l.rows[l.curIdx].name
}

// CurrentPhaseSince returns how long the current phase has been running, read
// against the injected clock. Zero if no phase is current.
func (l *lifecyclePane) CurrentPhaseSince() time.Duration {
	if l.curIdx < 0 {
		return 0
	}
	// syncedSince so the summary "since" ticks in lockstep with the header
	// "elapsed" and the "update" age (bug 2), all flipping on the same whole
	// second rather than each on its own anchor's sub-second fraction.
	return syncedSince(l.now(), l.rows[l.curIdx].arrived)
}

// AwaitingAgentReport reports whether the op has booted to the agent-handoff
// phase (ipxe_loaded) and is now waiting on the on-machine agent's first
// report — i.e. the current spinning phase is the last worker-emitted boot
// phase. The summary box uses this to render an explicit "awaiting agent
// report (elapsed)" line instead of just showing ipxe_loaded spinning, so an
// operator can tell "the box booted but the agent hasn't checked in" apart
// from normal mid-boot progress. False once any agent phase arrives (the
// current phase advances past the handoff) or before boot reaches it.
func (l *lifecyclePane) AwaitingAgentReport() bool {
	return l.curIdx >= 0 && l.rows[l.curIdx].name == agentHandoffPhase
}

// HighWaterPhase returns the name of the furthest spine phase ever reached, or
// "" if none has been observed. Used by the exit summary so a watch that timed
// out without a current spinning phase (e.g. a phase finalized but the next
// never arrived) still names where the op got to.
func (l *lifecyclePane) HighWaterPhase() string {
	if l.highWater < 0 {
		return ""
	}
	return l.rows[l.highWater].name
}

// FailedPhase returns the phase the op failed in, or "" if it hasn't failed.
// The header renders "✗ FAILED at <phase>" from it.
func (l *lifecyclePane) FailedPhase() string { return l.failedPhase }

// Fail marks the current phase failed at time at. The remaining runway stays
// pending (○), matching the failure mockup: we don't skip-mark phases the op
// never got to, and Observe ignores phases reported after this. If no phase is
// current (failure before any phase arrived, or after completion) it only
// records the failed flag.
func (l *lifecyclePane) Fail(at time.Time) {
	if l.curIdx >= 0 {
		cur := &l.rows[l.curIdx]
		cur.status = phaseFailed
		cur.duration = at.Sub(cur.arrived).Round(time.Second)
		l.failedPhase = cur.name
		l.curIdx = -1
	}
	l.failed = true
}

// Render lays out the runway as plain text at the given inner width, one phase
// per line. spinner is the glyph for the current phase (the model passes a
// live spinner frame; tests pass a fixed glyph for determinism). No color —
// the composition layer styles whole rows, which doesn't change visible width.
func (l *lifecyclePane) Render(width int, spinner string) string {
	lines := make([]string, len(l.rows))
	for i, r := range l.rows {
		lines[i] = l.renderRow(r, width, spinner)
	}
	return strings.Join(lines, "\n")
}

// stuckPhaseFloor is the S2 threshold: a current phase running longer than
// this turns amber, a cheap "something may be wrong" signal well before the
// 30m op timeout. A flat const for now (no historical-percentile dependency);
// tighten per-phase once there's a corpus of completed ops (plan S1).
const stuckPhaseFloor = 5 * time.Minute

func (l *lifecyclePane) renderRow(r phaseRow, width int, spinner string) string {
	var glyph, right string
	style := styleDim // pending/skipped default
	switch r.status {
	case phaseDone:
		glyph = "✓"
		if r.name != terminalCompleted {
			right = shortDur(r.duration)
		}
		style = styleDone
	case phaseFailed:
		glyph = "✗"
		right = shortDur(r.duration)
		style = styleFailed
	case phaseCurrent:
		glyph = spinner
		since := syncedSince(l.now(), r.arrived)
		right = "(" + shortDur(since) + ")"
		style = styleActive
		if since >= stuckPhaseFloor {
			style = styleAmber // S2: stuck-phase warning
		}
	case phaseSkipped:
		glyph = "—"
		right = "skipped"
	default: // phasePending
		glyph = "○"
	}
	return style.Render(layoutRow(glyph, r.name, right, width))
}

// layoutRow composes "<glyph> <name> ... <right>" into a fixed width with the
// right field flush to the edge, ellipsizing the name if the line would
// overflow. A glyph is one display cell; name and right are ASCII. Width too
// small to hold even a truncated name returns the best-effort left portion.
func layoutRow(glyph, name, right string, width int) string {
	// "<glyph> " prefix = 2 cells. Reserve a space before right when present.
	const prefix = 2
	rightW := 0
	if right != "" {
		rightW = len(right) + 1 // leading space separates name from right
	}
	avail := width - prefix - rightW
	if avail < 1 {
		// Degenerate width: drop right, show what name fits. Slice on runes —
		// glyph is a multi-byte box char (✓/✗/○/—), so a byte slice would
		// split it and emit invalid UTF-8.
		rs := []rune(glyph + " " + name)
		if width >= 0 && len(rs) > width {
			return string(rs[:width])
		}
		return string(rs)
	}
	if len(name) > avail {
		if avail >= 1 {
			name = name[:avail-1] + "…"
		}
	}
	left := glyph + " " + name
	// Pad so right is flush. left's visible width: glyph(1)+space(1)+name.
	leftVisible := prefix + len([]rune(name))
	pad := max(width-leftVisible-rightW, 1)
	if right == "" {
		return left
	}
	return left + strings.Repeat(" ", pad) + right
}

// shortDur renders a duration compactly: "12s", "2m14s", "3m41s". Sub-minute
// durations show only seconds; minute-plus always shows both. Matches the
// mockup's lifecycle column.
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%ds", m, s)
}
