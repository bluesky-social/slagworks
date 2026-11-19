package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// This file composes the three panes (console, lifecycle, logs) plus a header
// and summary box into one full-terminal frame. It is the net-new layout code
// from plan D1: sizing, the O1 small-terminal path, the bordered boxes, and
// the success/failure header variants.
//
// renderFrame is a pure function of frameInput -> string, mirroring the panes'
// golden-testable discipline. No color: the boxes are plain box-drawing so
// goldens stay diffable; row/state coloring is a thin styling pass over the
// composed string and doesn't change visible width. The tea.Model Update loop,
// WS goroutines, viewport scroll, and exit behavior land in the next slice and
// drive this function.

// Layout constants derived from the mockup. The console pane is the cast's
// fixed 80x24; everything else sizes around it.
const (
	// minTermCols/minRows gate the 3-pane view. The top row is the console box
	// (consoleWidth 80 + 2 borders = 82 cells) beside the right column
	// (lifecycleColW inner + 2 borders). The right column's inner width is
	// derived from the longest phase label + duration column (lifecycleColW); for
	// the deploy spine that is 33 inner / 35 outer, so the top row is 117 cells
	// wide. The gate must clear that or JoinHorizontal produces rows wider than
	// the terminal and the logs box (which spans the top width) overflows. 118
	// leaves a one-column margin, matching the prior gate's small slack over the
	// exact pane math.
	minTermCols = 118
	minRows     = 40
)

// Lifecycle/summary column sizing. The right column's inner width is computed
// from content rather than hardcoded so the longest known phase label
// ("installing_bootloader", 21) renders in full beside its duration instead of
// ellipsizing. Deriving it keeps the box correct-by-construction as the spine
// evolves: add a longer phase name and the column grows to fit.
const (
	// phaseRowPrefix is layoutRow's "<glyph> " lead (glyph + space).
	phaseRowPrefix = 2
	// phaseDurColW is the widest duration field a row's right column can show,
	// including its leading separator space. The in-progress form is the binding
	// case: "(" + shortDur + ")" with a leading space, e.g. " (59m59s)" = 9
	// cells. Finalized durations ("59m59s" + leading space = 7) are narrower, so
	// 9 covers both. shortDur caps minutes at two digits in practice (the op
	// 30m timeout is well under 100m); 59m59s is the conservative ceiling.
	phaseDurColW = 9
	// phaseRowInterPad is the minimum gap layoutRow keeps between the name and
	// the right field (its pad floor of 1).
	phaseRowInterPad = 1
)

// lifecycleColW returns the inner width for the lifecycle and summary boxes:
// wide enough that the longest label in the op's phase spine renders in full
// next to a worst-case duration column, with no ellipsis. The summary box
// shares this width; its longest content (a phase name in the "phase" row) is
// bounded by the same label set plus a narrower label column, so the lifecycle
// row's requirement dominates.
func lifecycleColW(opType model.OperationType) int {
	longest := 0
	for _, name := range spineFor(opType) {
		if name == terminalCompleted {
			continue // terminalCompleted never shows a duration and isn't the binding label
		}
		if n := len(name); n > longest {
			longest = n
		}
	}
	return phaseRowPrefix + longest + phaseRowInterPad + phaseDurColW
}

// box draws a rounded border around content sized to an exact inner width,
// then splices a title into the top edge ("╭─ serial console ──...──╮"). The
// content is pre-laid-out to innerW columns; we don't re-wrap.
//
// Content sits flush against the left border (no inner padding). The mockup
// shows a one-column inset, but the console pane is an exact 80-column terminal
// grid that must map 1:1 — padding it would corrupt the cursor math. Padding
// only the side boxes would make the console text flush while everything else
// is inset, which reads worse than a consistent flush layout, so nothing pads.
func box(title, content string, innerW int) string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	// Lip Gloss sizes the whole block, border included, so add the border cells
	// to the inner width.
	rendered := style.
		Width(innerW + style.GetHorizontalBorderSize()).
		Render(content)
	return embedTitle(rendered, title)
}

// embedTitle rewrites the top border line of a rendered lipgloss box to splice
// "─ <title> " in after the top-left corner. Operates on runes (border glyphs
// are multi-byte) and preserves total width, so it doesn't disturb alignment.
func embedTitle(boxStr, title string) string {
	if title == "" {
		return boxStr
	}
	first, rest, hasRest := strings.Cut(boxStr, "\n")
	top := []rune(first)
	label := []rune("─ " + title + " ")
	if len(top) < len(label)+2 {
		return boxStr // title wider than the border; leave plain
	}
	out := make([]rune, 0, len(top))
	out = append(out, top[0])                // ╭
	out = append(out, label...)              // ─ title␠
	out = append(out, top[1+len(label):]...) // remaining dashes + ╮
	res := string(out)
	if hasRest {
		res += "\n" + rest
	}
	return res
}

// padToHeight pads content with blank lines to exactly n rows (or leaves it if
// already taller), so stacked boxes align. Used to grow the lifecycle content
// to match the console box height.
func padToHeight(content string, n int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// centerInGrid places a single short line roughly centered in a w×h grid of
// spaces, so the console catch-up placeholder occupies the box's shape rather
// than collapsing it to one row. Width clamped, not wrapped (the placeholder
// is always short).
func centerInGrid(s string, w, h int) string {
	s = clampWidth(s, w)
	pad := max((w-len([]rune(s)))/2, 0)
	mid := strings.Repeat(" ", pad) + s
	lines := make([]string, h)
	midRow := h / 2
	for i := range lines {
		if i == midRow {
			lines[i] = mid
		} else {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// staticShades are the block-element brightness steps used for the "no signal"
// snow, ordered sparse → dense. Weighted toward the mid-gray band (▒/▓) so the
// field reads as TV snow rather than a solid fill or scattered specks; the
// occasional space lets the black background through for contrast.
var staticShades = []rune{' ', '░', '▒', '▓', '█'}
var staticWeights = []int{15, 25, 30, 20, 10}

// staticScreen renders "no signal" TV-snow into the w×h console grid, used when
// the SOL feed has produced no bytes (see consolePane.hasSignal). Seeded by now
// so it flickers in a live TUI — the 10Hz heartbeat advances now each frame and
// re-rolls the snow — but a test's fixed clock pins the seed, keeping golden
// snapshots byte-stable. Colored dim-gray via styleDim so it reads as snow on a
// color terminal; the color is a wrap around plain block chars, so the
// ANSI-stripped golden pins the snow itself (like the rest of the palette).
func staticScreen(now time.Time, w, h int) string {
	r := rand.New(rand.NewSource(now.UnixNano())) //nolint:gosec // G404: TV-snow animation seed, not a security context; deterministic seeding is required for golden-snapshot tests
	var total int
	for _, x := range staticWeights {
		total += x
	}
	lines := make([]string, h)
	for y := range lines {
		row := make([]rune, w)
		for x := range row {
			pick := r.Intn(total)
			acc := 0
			for i, wt := range staticWeights {
				acc += wt
				if pick < acc {
					row[x] = staticShades[i]
					break
				}
			}
		}
		lines[y] = string(row)
	}
	return styleDim.Render(strings.Join(lines, "\n"))
}

// frameInput is everything renderFrame needs. The composed model fills it each
// View(); tests build it directly. now/spinner are the D9 seams (loc lives in
// the logPane, which pre-formats its lines before they reach here).
type frameInput struct {
	opType    model.OperationType
	machine   string // machine label (hostname)
	opID      string // short op id (fallback when command is empty)
	command   string // human-readable invocation, shown in the header instead of the op UUID
	worker    string // claiming site worker hostname, "" until claimed
	startedAt time.Time

	// lastEventAt is the wall-clock time of the most recent inbound activity
	// (console byte / log line / phase / claim / state change). The summary's
	// "last update <N>s ago" reads now()-lastEventAt; zero means nothing has
	// arrived yet (rendered "—"). Computed live off now() so it ticks up via the
	// heartbeat between events — a stalled host reads an ever-growing staleness.
	lastEventAt time.Time

	console   *consolePane
	lifecycle *lifecyclePane
	logLines  []string // pre-formatted control-plane lines (logPane.Lines())

	// consoleCatchUp, when set, renders a "catching up…" placeholder in the
	// console box instead of the live emulator screen (plan D2): while an
	// initial backfill is still draining under the per-tick time budget, the
	// emulator screen is mid-replay and would flash through intermediate
	// states. The model sets this until the backfill is fully applied. The
	// string carries the progress hint (e.g. "catching up… 1.2/3.4 MB").
	consoleCatchUp string

	// consoleReconnecting flags a dropped console WS (plan D4 / S3): the
	// console box title shows "⚠ console reconnecting" without tearing down
	// the logs/lifecycle panes.
	consoleReconnecting bool

	// Terminal state. done+failed pick the header variant and swap the logs
	// box title to "error" with the cause pinned on top.
	done       bool
	failed     bool
	failReason string

	width   int
	height  int
	spinner string // current spinner glyph (fixed in tests)
	now     func() time.Time
}

// syncedSince computes a whole-second elapsed (now - anchor) that ticks in
// LOCKSTEP with every other timer derived the same way. The naive
// now.Sub(anchor).Round(second) makes each timer flip to its next integer at a
// different sub-second instant — whenever (now-anchor) crosses N.5 — and since
// each anchor (op start, phase arrival, last event) has a different sub-second
// fraction, the displayed timers visibly step at different moments (bug 2). By
// truncating BOTH now and anchor to the second before subtracting, every timer
// advances on the same wall-clock boundary: the instant now() crosses into a
// new whole second, they all increment together. Negative (anchor ahead of now,
// e.g. server clock skew) clamps to 0.
func syncedSince(now, anchor time.Time) time.Duration {
	d := now.Truncate(time.Second).Sub(anchor.Truncate(time.Second))
	if d < 0 {
		return 0
	}
	return d
}

func (in frameInput) elapsed() time.Duration {
	return syncedSince(in.now(), in.startedAt)
}

// lastUpdateAgo returns how long since the most recent inbound activity, and
// whether any activity has been observed at all. ok=false (lastEventAt zero)
// means nothing has arrived yet — the summary renders "—". Read live off now()
// so it ticks up via the heartbeat when the stream goes quiet, making a stalled
// host read an ever-growing "last update <N>s ago".
func (in frameInput) lastUpdateAgo() (time.Duration, bool) {
	if in.lastEventAt.IsZero() {
		return 0, false
	}
	return syncedSince(in.now(), in.lastEventAt), true
}

// minWidthFor is the minimum terminal width the 3-pane layout needs for an op
// type. It gates the ACTUAL top-row width, not just the static minTermCols
// floor: lifecycleColW is derived from the longest phase label, so a spine with
// a longer phase name than deploy's would need a wider terminal than 118. The
// top row is the console box (consoleWidth + 2 borders) beside the right column
// (lifecycleColW + 2 borders) = consoleWidth + 4 + lifecycleColW cells. The
// right column fills to width-1 (avoiding cursor auto-wrap, see renderFrame), so
// this adds one more column (+5, not +4) to guarantee the layout still fits
// inside that width-1 budget at exactly the minimum — without it, a spine whose
// content makes this gate binding (rather than the 118 floor) would, at
// width == minW, force the right column to its content floor and push the top
// row to exactly width, tripping the auto-wrap the budget exists to prevent.
// Shared by the gate AND the small-terminal message so the "need >=N" readout
// is the real binding width, never a stale hardcoded floor.
func minWidthFor(opType model.OperationType) int {
	minW := max(consoleWidth+5+lifecycleColW(opType), minTermCols)
	return minW
}

// renderFrame produces the full-terminal frame string.
func renderFrame(in frameInput) string {
	if in.width < minWidthFor(in.opType) || in.height < minRows {
		return smallTerminalMessage(in)
	}

	// The header fits itself to width-1 internally (renderHeader truncates the
	// long command token, never elapsed), so it's already within bounds. The
	// width-1 budget avoids writing exactly `width` columns, which can trigger
	// cursor auto-wrap on some terminals and insert a blank line between frame
	// rows.
	header := renderHeader(in, in.width-1)

	// Console box content: the live emulator screen, unless a backfill is
	// still draining (show the catch-up placeholder so we don't flash through
	// intermediate replay states — D2). The placeholder fills the 24-row grid
	// so the box keeps its shape.
	consoleTitle := "serial console"
	if in.consoleReconnecting {
		consoleTitle = "serial console  ⚠ reconnecting"
	}
	consoleContent := in.console.Screen()
	if in.consoleCatchUp != "" {
		consoleContent = centerInGrid(in.consoleCatchUp, consoleWidth, consoleHeight)
	} else if !in.console.hasSignal() {
		// No SOL bytes have ever arrived — the serial console isn't wired up
		// (or the host has printed nothing). The emulator screen is 24 rows of
		// spaces, which reads as a broken empty box; render TV-snow static
		// instead so the "no signal" state is visible. Seeded by now, so the
		// 10Hz heartbeat flickers it in a live TUI while a test's fixed clock
		// keeps the golden byte-stable. The first real byte flips to the live
		// emulator screen and never returns here.
		consoleContent = staticScreen(in.now(), consoleWidth, consoleHeight)
	}
	consoleBox := box(consoleTitle, consoleContent, consoleWidth)

	// Right column: lifecycle box stacked over the summary box, padded to the
	// console box's height so the top section is a clean rectangle. The console
	// box is a fixed 80-col emulator grid (consoleWidth) + 2 border cells = 82
	// cells wide and cannot flex (1:1 cursor mapping). So the right column takes
	// ALL the remaining width to the right of it, making the lifecycle/summary
	// boxes — and, via the joined top width, the logs box below — fill the whole
	// terminal. Its inner width is (terminal width − console outer
	// − this column's own 2 borders), floored at lifecycleColW so it never
	// shrinks below the content minimum (the longest phase label + duration
	// column). The extra width lands in the summary's value column, so a long
	// worker FQDN renders in full instead of ellipsizing.
	// Fill to width-1, matching the header's budget (see above): writing exactly
	// `width` columns can trip cursor auto-wrap on some terminals and inject a
	// blank line between frame rows. So the top row (console outer + right col
	// outer) and the logs box that spans it target width-1, not width.
	const consoleOuterW = consoleWidth + 2 // box() adds a 1-cell border each side
	colW := (in.width - 1) - consoleOuterW - 2
	if min := lifecycleColW(in.opType); colW < min {
		colW = min
	}
	lifeBox := box("lifecycle", in.lifecycle.Render(colW, in.spinner), colW)
	sumBox := box("summary", renderSummary(in, colW), colW)
	rightCol := lipgloss.JoinVertical(lipgloss.Left, lifeBox, sumBox)
	rightCol = padToHeight(rightCol, lipgloss.Height(consoleBox))

	top := lipgloss.JoinHorizontal(lipgloss.Top, consoleBox, rightCol)
	topW := lipgloss.Width(top)

	// Logs/error box spans the full top width and fills ALL the vertical space
	// the header + top section leave unused, then holds that height. Compute the
	// log body's row count from the actual rendered heights rather than a magic
	// constant: terminal height minus the header, minus the top (console/right)
	// section, minus this box's own two border rows. renderLogBody pads to
	// exactly this many rows so the box stays full-height even with few lines.
	logsTitle := "logs (control plane, SOL excluded)"
	logsInnerW := topW - 2
	logRows := max(in.height-lipgloss.Height(header)-lipgloss.Height(top)-2, 1)
	logBody := in.logLines
	if in.done && in.failed {
		logsTitle = "error"
		// Pin the failure cause to the top, then tail the log history to what
		// remains — NOT errorPaneBody + a blind tail in renderLogBody, which
		// would chop the prepended cause off the top once the history exceeds
		// logRows, hiding the very line the error pane exists to show.
		logBody = errorPaneBody(in, logsInnerW, logRows)
	}
	logsBox := box(logsTitle, renderLogBody(logBody, logsInnerW, logRows), logsInnerW)

	return lipgloss.JoinVertical(lipgloss.Left, header, top, logsBox)
}

// renderHeader is the top status line, fit to maxW display cells. Layout:
//
//	<lead> <opType>  <machine>   <command…>   <right>
//
// The lead glyph is the GLOBAL liveness spinner while the op is non-terminal:
// it spins off the heartbeat independent of phase state, so "UI
// alive" is always visible even when no lifecycle phase is current. On a
// terminal state it becomes the ✓/✗ outcome glyph.
//
// The right segment (elapsed / "completed in" / "FAILED at" + "[q] quit") and
// the left identity are always preserved; only the middle command token is
// truncated when the line would overflow. The worker is NOT in
// the header — it lives once, in the summary box, which frees the
// width that previously pushed elapsed off the end.
func renderHeader(in frameInput, maxW int) string {
	lead := in.spinner
	var right string
	switch {
	case in.done && in.failed:
		lead = "✗"
		at := in.lifecycle.FailedPhase()
		if at == "" {
			at = "?"
		}
		right = styleFailed.Render(fmt.Sprintf("✗ FAILED at %s", at)) + styleDim.Render("    [q] quit")
	case in.done:
		lead = "✓"
		right = styleDone.Render(fmt.Sprintf("✓ completed in %s", shortDur(in.elapsed()))) + styleDim.Render("    [q] quit")
	default:
		right = styleActive.Render(fmt.Sprintf("elapsed %s", shortDur(in.elapsed()))) + styleDim.Render("    [q] quit")
	}

	left := fmt.Sprintf(" %s %s  %s", lead, in.opType, styleHeader.Render(in.machine))

	// The reconstructed command line (the invocation that produced this op),
	// shown in place of the raw op UUID; fall back to "op <shortid>".
	detail := in.command
	if detail == "" && in.opID != "" {
		detail = "op " + in.opID
	}

	// Reserve room for left + right (always shown) + the separators around the
	// command. Whatever remains is the command's budget; if it doesn't fit, the
	// command is truncated (it's the long, least-critical token), never elapsed.
	const sep = "   "
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	if detail == "" {
		line := left + sep + right
		return ansi.Truncate(line, maxW, "…")
	}
	// Budget for the command = maxW - left - right - two separators.
	cmdBudget := maxW - leftW - rightW - 2*len([]rune(sep))
	if cmdBudget < 1 {
		// No room for the command at all; drop it, keep identity + elapsed.
		return ansi.Truncate(left+sep+right, maxW, "…")
	}
	detail = clampWidth(detail, cmdBudget)
	return left + sep + styleDim.Render(detail) + sep + right
}

// workerLabel renders the claiming worker for the header/summary. claimed_by
// may be a worker UUID or a friendly worker name. Em dash before it's known;
// a UUID is shortened to its first 8 chars (operators map it via
// `slagctl worker list`); a friendly name is shown verbatim (the box width
// truncates if needed — we don't chop a real name at 8).
func workerLabel(worker string) string {
	if worker == "" {
		return "—"
	}
	if _, err := uuid.Parse(worker); err == nil {
		return worker[:8]
	}
	return worker
}

// renderSummary is the derived-state box under the lifecycle column: current
// phase, time in that phase, total elapsed, site worker.
func renderSummary(in frameInput, width int) string {
	phase := in.lifecycle.CurrentPhase()
	since := shortDur(in.lifecycle.CurrentPhaseSince())
	if phase == "" {
		phase, since = "—", "—"
	}
	// While the op has booted to the agent-handoff phase but the on-machine
	// agent hasn't made its first report, surface that explicitly: the operator
	// otherwise can't tell "agent booted, awaiting first report" (a possible
	// stall — e.g. commission's no-bootif-match) from normal mid-boot progress.
	// The "since" clock keeps ticking so the wait is visibly live, not frozen.
	if !in.done && in.lifecycle.AwaitingAgentReport() {
		phase = "awaiting agent report"
	}
	// "last update <N>s ago" — the staleness signal. Resets to 0 on
	// any inbound event and ticks up via the heartbeat otherwise, so a stalled
	// host reads an ever-growing age. "—" until the first event. Once terminal
	// the clock is frozen (frameNow latches endTime), so this stops advancing.
	lastUpdate := "—"
	if ago, ok := in.lastUpdateAgo(); ok {
		lastUpdate = shortDur(ago) + " ago"
	}
	rows := [][2]string{
		{"phase", phase},
		{"since", since},
		{"total", shortDur(in.elapsed())},
		{"update", lastUpdate},
		{"worker", workerLabel(in.worker)},
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(summaryRow(r[0], r[1], width))
	}
	return b.String()
}

// summaryRow lays out a " label  value" pair in the summary box. Unlike the
// lifecycle column's layoutRow (which flush-rights a short duration and
// protects the phase name), the summary has a fixed label column and the VALUE
// is what ellipsizes when long — a phase like awaiting_phone_home must not
// crowd the "phase" label down to "p…". Layout: one leading space, a
// left-aligned label padded to summaryLabelW, a space, then the value
// truncated to whatever width remains.
const summaryLabelW = 7 // widest label ("worker") + 1

func summaryRow(label, value string, width int) string {
	left := " " + label
	// Pad the label column so values align.
	for len([]rune(left)) < 1+summaryLabelW {
		left += " "
	}
	avail := width - len([]rune(left)) - 1 // 1 = space before value
	if avail < 1 {
		return clampWidth(left, width)
	}
	// Layout math done on plain strings; color is applied after so it can't
	// perturb widths (styleLabel/Render add escape codes len() would count).
	return styleLabel.Render(left) + " " + clampWidth(value, avail)
}

// renderLogBody lays out the log lines to fill exactly `rows` rows: the last
// `rows` lines that fit, each clamped to innerW, and — when there are fewer
// lines than rows — padded with blank rows so the box keeps its full height
// instead of collapsing to the line count. `rows` is the exact inner-height the
// caller computed from the terminal geometry (terminal minus header, top
// section, and this box's borders), so the logs box always spans the remaining
// height and stays there. The composition layer swaps this for a scrollable
// viewport in a follow-up; for now it's a height-filling tail.
func renderLogBody(lines []string, innerW, rows int) string {
	if rows < 1 {
		rows = 1
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	out := make([]string, rows)
	for i := range out {
		if i < len(lines) {
			// Cell-aware, to agree with the wrapped failure cause
			// (wrapLines): a rune count would cut a row that fits.
			out[i] = ansi.Truncate(lines[i], innerW, "…")
		} else {
			out[i] = "" // pad to full height
		}
	}
	return strings.Join(out, "\n")
}

// errorPaneBody pins the failure cause on top of the tail of log lines, per the
// failure mockup. It reserves rows for the cause and tails the log history to
// whatever rows remain, so a long history never pushes the pinned cause off the
// top (renderLogBody tails blindly; tailing here keeps the cause at index 0).
//
// The cause is WRAPPED to innerW, one row per line of it, not clamped to a
// single row like a log line: it is the one thing the error pane exists to
// show, and an agent failure carries the failing script's own diagnostics
// (often several lines, ending in what to fix) after the stage prefix. A
// cause taller than the pane keeps its first row (the failed stage) and its
// END (where a script's decisive ERROR is), with a marker for what was cut.
func errorPaneBody(in frameInput, innerW, rows int) []string {
	cause := wrapLines(in.failReason, innerW)
	if len(cause) > rows {
		switch {
		case rows <= 0:
			cause = nil
		case rows == 1:
			cause = cause[len(cause)-1:]
		case rows == 2:
			cause = []string{cause[0], cause[len(cause)-1]}
		default:
			cause = append([]string{cause[0], clampWidth(causeOmittedMarker, innerW)}, cause[len(cause)-(rows-2):]...)
		}
	}
	lines := in.logLines
	avail := max(rows-len(cause), 0)
	if len(lines) > avail {
		lines = lines[len(lines)-avail:]
	}
	return append(cause, lines...)
}

// causeOmittedMarker replaces the middle of a failure cause too tall for the
// error pane. The full text is in the operation's result.
const causeOmittedMarker = "[... lines omitted; full error: slagctl operation show <op-id>]"

// wrapLines splits s on newlines and hard-wraps each line to w terminal cells
// (cell-aware, so wide characters in a script's stderr cannot overflow the
// row budget). Empty s yields no lines; w < 1 disables wrapping.
func wrapLines(s string, w int) []string {
	if s == "" {
		return nil
	}
	return strings.Split(ansi.Hardwrap(strings.TrimRight(s, "\n"), w, true), "\n")
}

// clampWidth truncates a line to w display cells (rune count; log lines are
// ASCII), so a long exec command can't blow out the box border.
func clampWidth(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w < 1 {
		return ""
	}
	return string(r[:w-1]) + "…"
}

// smallTerminalMessage is the O1 path: a single state-aware centered message
// instead of a degraded layout. It must reveal a terminal outcome so the
// operator isn't stuck watching "enlarge" after the op already finished.
func smallTerminalMessage(in frameInput) string {
	// Report the ACTUAL binding minimum for this op type, not the static
	// minTermCols floor — a spine with a long phase name needs a wider terminal,
	// and a readout of "need >=118" while the gate demands more is an
	// unresolvable prompt.
	reqW := minWidthFor(in.opType)
	var msg string
	switch {
	case in.done && in.failed:
		reason := in.failReason
		if reason == "" {
			reason = in.lifecycle.FailedPhase()
		}
		msg = fmt.Sprintf("op FAILED: %s — enlarge to ≥%d×%d to see detail, or press q to quit",
			reason, reqW, minRows)
	case in.done:
		msg = fmt.Sprintf("op completed in %s — enlarge to ≥%d×%d to see detail, or press q to quit",
			shortDur(in.elapsed()), reqW, minRows)
	default:
		msg = fmt.Sprintf("please enlarge your terminal (need ≥%d×%d, have %d×%d)",
			reqW, minRows, in.width, in.height)
	}
	return msg
}
