package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// TestSyncedSince_LockstepAcrossAnchors is the bug-2 guard: timers with
// DIFFERENT sub-second anchor fractions must flip to their next whole second at
// the SAME instant (when now() crosses a whole-second boundary), not each at
// its own anchor+0.5 moment. We sweep now() across a second boundary and assert
// both timers step together.
func TestSyncedSince_LockstepAcrossAnchors(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	anchorA := base                             // .000 fraction
	anchorB := base.Add(300 * time.Millisecond) // .300 fraction
	anchorC := base.Add(900 * time.Millisecond) // .900 fraction

	// now sweeps from base+5.0s to base+6.0s. With naive Round(), A would flip
	// at x.5, B at x.8, C at x.4 — staggered. With syncedSince all three flip
	// exactly when now crosses a whole second.
	prevA := syncedSince(base.Add(5*time.Second), anchorA)
	prevB := syncedSince(base.Add(5*time.Second), anchorB)
	prevC := syncedSince(base.Add(5*time.Second), anchorC)
	for ms := 5000; ms <= 6000; ms += 50 {
		now := base.Add(time.Duration(ms) * time.Millisecond)
		a := syncedSince(now, anchorA)
		b := syncedSince(now, anchorB)
		c := syncedSince(now, anchorC)
		// Whenever ANY timer increments from its previous value, ALL must have
		// incremented by the same step (they share the now-truncation boundary).
		stepA := a - prevA
		stepB := b - prevB
		stepC := c - prevC
		if stepA != stepB || stepB != stepC {
			t.Errorf("at %dms timers stepped out of sync: A+%s B+%s C+%s", ms, stepA, stepB, stepC)
		}
		prevA, prevB, prevC = a, b, c
	}
}

// midDeployFrame builds a frameInput at the mockup's mid-deploy moment:
// phases 1-6 done, partitioning current at 47s, a handful of log lines, a
// console pane with some boot output. Deterministic via fixed clock + glyph.
func midDeployFrame(w, h int) frameInput {
	lc := newLifecyclePane(model.OpDeploy)
	lc.Observe("power_cycle", at(0))
	lc.Observe("pxe_booting", at(2*time.Second))
	lc.Observe("dhcp_ack", at(16*time.Second))
	lc.Observe("ipxe_dhcp", at(17*time.Second))
	lc.Observe("ipxe_loaded", at(19*time.Second))
	lc.Observe("agent_starting", at(27*time.Second))
	lc.Observe("partitioning", at(30*time.Second))
	now := fixedClock(at(30*time.Second + 47*time.Second))
	lc.now = now

	con := newConsolePane()
	con.Apply([]byte("slagworks-agent: found 4 disk(s), using nvme0n1\r\n" +
		"slagworks-agent: storage layout planned (12 commands)\r\n" +
		"Creating new GPT entries in memory.\r\n"))

	lp := newLogPane(time.UTC)
	lp.Append(1, mkEnv("agent", at(72*time.Second), "partitioning", "exec",
		map[string]any{"argv": "sgdisk --zap-all /dev/nvme0n1", "duration_ms": float64(212)}))
	lp.Append(2, mkEnv("site", at(73*time.Second), "partitioning", "progress report received", nil))
	lp.Append(3, mkEnv("global", at(74*time.Second), "", "snapshot rebuilt gen=4471", nil))

	return frameInput{
		opType:    model.OpDeploy,
		machine:   "kworker-l34-1.pop1",
		opID:      "8b2e3f1a",
		command:   "machine deploy kworker-l34-1.pop1 --destructive",
		worker:    "pop1-sw-2",
		startedAt: at(0),
		console:   con,
		lifecycle: lc,
		logLines:  lp.Lines(),
		width:     w,
		height:    h,
		spinner:   "⠙",
		now:       now,
	}
}

// headerLine returns the first line of a rendered frame (the header), for
// assertions that must scope to the header and not the summary box (which now
// owns the worker).
func headerLine(frame string) string {
	return strings.SplitN(frame, "\n", 2)[0]
}

// TestFrame_MidDeploy goldens the full happy-path frame at the min terminal
// size. This is the layout's primary golden — header, console box, lifecycle +
// summary right column, logs box.
func TestFrame_MidDeploy(t *testing.T) {
	frame := plain(renderFrame(midDeployFrame(minTermCols, minRows)))
	if !strings.Contains(frame, "serial console") {
		t.Error("console box title missing")
	}
	if !strings.Contains(frame, "lifecycle") {
		t.Error("lifecycle box title missing")
	}
	if !strings.Contains(frame, "⠙ partitioning") {
		t.Error("current phase not in frame")
	}
	// Worker is shown ONCE, in the summary box, not the header.
	if !strings.Contains(frame, "pop1-sw-2") {
		t.Error("summary missing worker")
	}
	if strings.Contains(headerLine(frame), "pop1-sw-2") {
		t.Errorf("worker must not appear in the header (shown once, in summary):\n%s", headerLine(frame))
	}
	if !strings.Contains(frame, "machine deploy kworker-l34-1.pop1 --destructive") {
		t.Error("header missing reconstructed command line")
	}
	if strings.Contains(frame, "op 8b2e3f1a") {
		t.Error("header should show the command, not the raw op id, when command is set")
	}
	if !strings.Contains(frame, "logs (control plane, SOL excluded)") {
		t.Error("logs box title missing")
	}
	// No row may exceed the terminal width (box alignment guard).
	for line := range strings.SplitSeq(frame, "\n") {
		if w := lipgloss.Width(line); w > minTermCols {
			t.Errorf("frame line exceeds width %d: %d cells\n%q", minTermCols, w, line)
		}
	}
	goldenScreen(t, "frame_mid_deploy", frame)
}

// TestFrame_SmallTerminal is the O1 path: below the minimum, a single
// state-aware message, not a degraded layout.
func TestFrame_SmallTerminal(t *testing.T) {
	in := midDeployFrame(80, 24)
	got := plain(renderFrame(in))
	if strings.Contains(got, "serial console") {
		t.Errorf("small terminal should not render the 3-pane layout:\n%s", got)
	}
	if !strings.Contains(got, "enlarge") {
		t.Errorf("small terminal message missing enlarge prompt:\n%s", got)
	}
	if !strings.Contains(got, "have 80×24") {
		t.Errorf("small terminal message should report actual size:\n%s", got)
	}
}

// TestFrame_SmallTerminalRevealsOutcome guards the O1 state-awareness: a
// finished op must surface its outcome even when too small to draw, so the
// operator isn't stuck watching "enlarge" forever.
func TestFrame_SmallTerminalRevealsOutcome(t *testing.T) {
	in := midDeployFrame(80, 24)
	in.done = true
	in.failed = true
	in.failReason = "sgdisk exit 2 (device busy?)"
	got := plain(renderFrame(in))
	if !strings.Contains(got, "FAILED") {
		t.Errorf("failed op not surfaced at small size:\n%s", got)
	}
	if !strings.Contains(got, "press q to quit") {
		t.Errorf("no quit affordance at small size:\n%s", got)
	}
}

// TestFrame_Failure swaps the logs box to the error pane and pins the cause on
// top; the header reads "✗ FAILED at <phase>".
func TestFrame_Failure(t *testing.T) {
	lc := newLifecyclePane(model.OpDeploy)
	lc.Observe("power_cycle", at(0))
	lc.Observe("partitioning", at(30*time.Second))
	now := fixedClock(at(42 * time.Second))
	lc.now = now
	lc.Fail(at(42 * time.Second))

	in := frameInput{
		opType:     model.OpDeploy,
		machine:    "kworker-l34-1.pop1",
		opID:       "8b2e3f1a",
		startedAt:  at(0),
		console:    newConsolePane(),
		lifecycle:  lc,
		logLines:   []string{"10:14:14 [agent ] FATAL storage: sgdisk → exit 2"},
		done:       true,
		failed:     true,
		failReason: "partitioning failed: sgdisk exit 2 (device busy?)",
		width:      minTermCols,
		height:     minRows,
		spinner:    "⠙",
		now:        now,
	}
	got := plain(renderFrame(in))
	if !strings.Contains(got, "FAILED at partitioning") {
		t.Errorf("header should name the failing phase:\n%s", got)
	}
	if !strings.Contains(got, "─ error ─") {
		t.Errorf("logs box should retitle to error:\n%s", got)
	}
	if !strings.Contains(got, "partitioning failed: sgdisk exit 2") {
		t.Errorf("failure cause not pinned:\n%s", got)
	}
	if strings.Contains(got, "✗ partitioning") {
		// the lifecycle column should show the failed phase marker
	} else {
		t.Errorf("lifecycle should mark partitioning failed:\n%s", got)
	}
	goldenScreen(t, "frame_failure", got)
}

// TestFrame_ErrorCauseSurvivesLongHistory guards that the pinned failure cause
// stays visible even when the log history is longer than the error pane's row
// budget. The error pane tails the history to make room for the cause, so the
// cause is never chopped off the top (the bug: errorPaneBody prepended the cause
// and renderLogBody then blindly tailed, dropping it).
func TestFrame_ErrorCauseSurvivesLongHistory(t *testing.T) {
	lc := newLifecyclePane(model.OpDeploy)
	lc.Observe("power_cycle", at(0))
	lc.Observe("partitioning", at(30*time.Second))
	now := fixedClock(at(42 * time.Second))
	lc.now = now
	lc.Fail(at(42 * time.Second))

	// Far more log lines than any plausible error-pane height.
	var history []string
	for i := range 200 {
		history = append(history, fmt.Sprintf("10:14:%02d [agent ] log line %d", i%60, i))
	}
	in := frameInput{
		opType:     model.OpDeploy,
		machine:    "kworker-l34-1.pop1",
		opID:       "8b2e3f1a",
		startedAt:  at(0),
		console:    newConsolePane(),
		lifecycle:  lc,
		logLines:   history,
		done:       true,
		failed:     true,
		failReason: "partitioning failed: sgdisk exit 2 (device busy?)",
		width:      minTermCols,
		height:     minRows,
		spinner:    "⠙",
		now:        now,
	}
	got := plain(renderFrame(in))
	if !strings.Contains(got, "partitioning failed: sgdisk exit 2") {
		t.Errorf("failure cause must survive a long log history (it was tailed off the top):\n%s", got)
	}
}

// TestErrorPaneBody_WrapsMultiLineCause guards the agent failure shape: a
// stage prefix, then the failing config script's stderr on following lines,
// ending in what to fix. The cause must be wrapped to the pane width, one row
// per line, never clamped to a single ellipsized row (which cut off the
// actionable text) nor left as one entry with embedded newlines (which broke
// the box's row budget). Log history tails into the rows that remain.
func TestErrorPaneBody_WrapsMultiLineCause(t *testing.T) {
	in := frameInput{
		failReason: "net: script failed\nERROR: no bond. Fix the NetBox model.",
		logLines:   []string{"log 1", "log 2", "log 3", "log 4"},
	}
	got := errorPaneBody(in, 20, 5)
	want := []string{
		"net: script failed",
		"ERROR: no bond. Fix ",
		"the NetBox model.",
		"log 3",
		"log 4",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("errorPaneBody =\n%q\nwant\n%q", got, want)
	}

	// A cause taller than the pane keeps the stage line and the END (where
	// the script's decisive ERROR is), marks the cut, and leaves no log rows.
	in.failReason = "net: script failed\nWARNING: one\nWARNING: two\nWARNING: three\nERROR: fix the model"
	got = errorPaneBody(in, 80, 4)
	want = []string{"net: script failed", causeOmittedMarker, "WARNING: three", "ERROR: fix the model"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("errorPaneBody (4 rows) =\n%q\nwant\n%q", got, want)
	}
	got = errorPaneBody(in, 80, 2)
	want = []string{"net: script failed", "ERROR: fix the model"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("errorPaneBody (2 rows) = %q, want %q", got, want)
	}
}

// TestWrapLines_WideCharacters pins cell-aware wrapping: a script's stderr is
// arbitrary text, and a row of wide characters wrapped by rune count would
// render at twice the pane width and break the row budget.
func TestWrapLines_WideCharacters(t *testing.T) {
	for _, line := range wrapLines(strings.Repeat("界", 15), 10) {
		if w := lipgloss.Width(line); w > 10 {
			t.Errorf("wrapped row %q is %d cells wide, want <= 10", line, w)
		}
	}
}

// TestFrame_MultiLineFailureKeepsLayout renders a full frame with a multi-line
// cause longer than the pane width and checks the whole cause is visible and
// the frame height is unchanged from a one-line cause.
func TestFrame_MultiLineFailureKeepsLayout(t *testing.T) {
	frame := func(reason string) string {
		lc := newLifecyclePane(model.OpDeploy)
		lc.Observe("power_cycle", at(0))
		lc.Observe("finalizing", at(30*time.Second))
		now := fixedClock(at(42 * time.Second))
		lc.now = now
		lc.Fail(at(42 * time.Second))
		return plain(renderFrame(frameInput{
			opType:     model.OpDeploy,
			machine:    "kworker-l34-1.pop1",
			opID:       "8b2e3f1a",
			startedAt:  at(0),
			console:    newConsolePane(),
			lifecycle:  lc,
			logLines:   []string{"10:14:14 [agent ] FATAL during applying network config"},
			done:       true,
			failed:     true,
			failReason: reason,
			width:      minTermCols,
			height:     minRows,
			spinner:    "⠙",
			now:        now,
		}))
	}
	long := "applying network config: network script failed: exit status 1; script stderr:\n" +
		"ERROR: kworker-l34-1 does not have exactly one bond interface (type=lag with an IP); " +
		"found 0. Fix the NetBox model: create the bond, set both member NICs' lag field to " +
		"it, assign the host IP to the bond, then redeploy.\n" +
		"END-OF-CAUSE"
	got := frame(long)
	for _, want := range []string{"script stderr:", "ERROR: kworker-l34-1", "END-OF-CAUSE"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame missing %q from the cause:\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Errorf("cause must wrap, not ellipsize:\n%s", got)
	}
	if gotH, wantH := lipgloss.Height(got), lipgloss.Height(frame("one-line cause")); gotH != wantH {
		t.Errorf("frame height = %d with a multi-line cause, want %d (layout must not grow)", gotH, wantH)
	}
}

// TestFrame_FailureCauseCarriageReturnKeepsLayout: a failed config script's
// cause carries the script's stderr verbatim, and progress-style output in it
// (curl, dd status=progress) redraws a line with a bare "\r". The error pane
// split the cause into rows on "\n" only, so the "\r" reached the frame, where
// the renderer returns to column 0 and draws the rest of the row over the
// error box's left border. A bare "\r" is a row break, like "\n".
func TestFrame_FailureCauseCarriageReturnKeepsLayout(t *testing.T) {
	in := frameInput{
		failReason: "network script failed: exit status 1; script stderr:\n" +
			"  5% fetched\r100% fetched\r\nERROR: no bond",
	}
	got := errorPaneBody(in, 80, 6)
	want := []string{
		"network script failed: exit status 1; script stderr:",
		"  5% fetched",
		"100% fetched",
		"ERROR: no bond",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("errorPaneBody =\n%q\nwant\n%q", got, want)
	}

	lc := newLifecyclePane(model.OpDeploy)
	lc.Observe("power_cycle", at(0))
	lc.Observe("finalizing", at(30*time.Second))
	now := fixedClock(at(42 * time.Second))
	lc.now = now
	lc.Fail(at(42 * time.Second))
	frame := renderFrame(frameInput{
		opType:     model.OpDeploy,
		machine:    "kworker-l34-1.pop1",
		startedAt:  at(0),
		console:    newConsolePane(),
		lifecycle:  lc,
		done:       true,
		failed:     true,
		failReason: in.failReason,
		width:      minTermCols,
		height:     minRows,
		spinner:    "⠙",
		now:        now,
	})
	if strings.Contains(frame, "\r") {
		t.Errorf("frame contains a carriage return; the renderer would draw the rest of the row over the box border:\n%q", frame)
	}
	if n := len(strings.Split(frame, "\n")); n != minRows {
		t.Errorf("frame is %d rows on a %d-row terminal", n, minRows)
	}
}

// TestFrame_LogLineBreaksKeepFrameShape guards the logs box's row budget: one
// envelope renders as one row (logPane.format), and renderLogBody fills exactly
// the rows the terminal leaves. A message with line breaks in it (a
// subprocess's multi-line output, or the bare "\r" of progress-style output)
// reached the frame intact. Each "\n" added a row, so the frame outgrew the
// terminal and the renderer clipped the bottom border and the newest log
// lines; a bare "\r" returned the cursor to column 0 and drew the rest of the
// line over the box border and timestamp.
func TestFrame_LogLineBreaksKeepFrameShape(t *testing.T) {
	for _, tc := range []struct{ name, msg, first, second string }{
		{"LF", "mdadm: cannot open /dev/sdb1\nmdadm: device busy", "cannot open /dev/sdb1", "device busy"},
		{"CRLF", "mdadm: cannot open /dev/sdb1\r\nmdadm: device busy", "cannot open /dev/sdb1", "device busy"},
		{"bare CR", "network script stderr:   5% fetched\r100% fetched", "5% fetched", "100% fetched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Fill the logs box so the line-break entry and the newest line
			// both sit in the visible tail.
			lp := newLogPane(time.UTC)
			for i := 1; i <= 20; i++ {
				lp.Append(int64(i), mkEnv("site", at(time.Duration(i)*time.Second), "", fmt.Sprintf("line %d", i), nil))
			}
			lp.Append(21, mkEnv("agent", at(21*time.Second), "", tc.msg, nil))
			lp.Append(22, mkEnv("site", at(22*time.Second), "", "newest line", nil))
			in := midDeployFrame(minTermCols, minRows)
			in.logLines = lp.Lines()
			frame := renderFrame(in)

			if strings.Contains(frame, "\r") {
				t.Errorf("frame contains a carriage return; the renderer would draw the rest of the row over the box border")
			}
			rows := strings.Split(plain(frame), "\n")
			if len(rows) != minRows {
				t.Errorf("frame is %d rows on a %d-row terminal", len(rows), minRows)
			}
			// The terminal shows only the first minRows rows.
			visible := rows[:min(len(rows), minRows)]
			if !strings.Contains(visible[len(visible)-2], "newest line") {
				t.Errorf("newest log line is not on the last row of the logs box:\n%s", strings.Join(visible, "\n"))
			}
			if !strings.HasPrefix(visible[len(visible)-1], "╰") {
				t.Errorf("bottom border of the logs box is not on the terminal's last row:\n%s", strings.Join(visible, "\n"))
			}
			oneRow := false
			for _, row := range rows {
				if strings.Contains(row, tc.first) && strings.Contains(row, tc.second) {
					oneRow = true
				}
			}
			if !oneRow {
				t.Errorf("want %q and %q on one row:\n%s", tc.first, tc.second, strings.Join(rows, "\n"))
			}
		})
	}
}

// sgr matches the Select Graphic Rendition sequences the TUI's own styles and
// box borders emit.
var sgr = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// assertNoTerminalControls fails if s, with the TUI's own styling removed,
// still carries a control character other than a line feed: such a byte came
// from streamed data and would reach the operator's terminal.
func assertNoTerminalControls(t *testing.T, s string) {
	t.Helper()
	for _, line := range strings.Split(sgr.ReplaceAllString(s, ""), "\n") {
		for _, r := range line {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Errorf("frame carries terminal control %U: %q", r, line)
				break
			}
		}
	}
}

// TestFrame_EscapesTerminalControlsFromStream: the claiming worker, log lines
// and failure cause all arrive off the log stream and the op row, and the cause
// is the target agent's own report. Escape sequences in them must reach the
// frame as visible text, at full size and on the small-terminal path. The cell
// renderer Bubble Tea puts between the frame and the terminal drops some
// sequences (OSC 52, CSI) but passes OSC 8 hyperlinks and APC strings through,
// so the frame itself must be clean.
func TestFrame_EscapesTerminalControlsFromStream(t *testing.T) {
	m := newTestModel(fixedClock(at(42 * time.Second)))
	m.startedAt = at(0)
	m.holdOnTerminal = true
	m = send(m, phaseMsg{phase: "power_cycle", at: at(0)})
	m = send(m, claimedMsg{worker: "\x1b]52;c;ZWNobyBwd25lZAo=\x07worker"})
	m = send(m, logEnvelopeMsg{id: 1, env: slogkit.Envelope{TS: at(time.Second), Source: slogkit.SourceAgent,
		Msg: "ok\x1b]8;;http://evil.example/\x07click\x1b]8;;\x07"}})
	m = send(m, opTerminalMsg{failed: true, at: at(42 * time.Second),
		reason: "net: script failed\nERROR: \x1b_apc\x1b\\missing bond\r\x1b[2K\x1b]8;;http://evil.example/\x07fix"})

	frame := m.View().Content
	assertNoTerminalControls(t, frame)
	for _, want := range []string{`\x1b]52;c;`, `ok\x1b]8;;http://evil.example/\x07click`, "net: script failed", `ERROR: \x1b_apc\x1b\missing bond`, `\x1b[2K\x1b]8;;http://evil.example/\x07fix`} {
		if !strings.Contains(plain(frame), want) {
			t.Errorf("frame missing %q:\n%s", want, plain(frame))
		}
	}

	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	small := m.View().Content
	assertNoTerminalControls(t, small)
	if !strings.Contains(small, `net: script failed\nERROR: \x1b_apc`) {
		t.Errorf("small-terminal message missing the escaped cause: %q", small)
	}
}

// TestWorkerLabel_ShortUUIDIsCanonical: uuid.Parse also accepts the 38-byte
// braced form without checking the bytes it strips and the urn:uuid: form, so
// the short label is cut from the parsed UUID, never from the raw claimed_by:
// "\x1bc0000000-...x" parses, and its first 8 raw bytes are ESC c (reset).
func TestWorkerLabel_ShortUUIDIsCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"c0ffee00-0000-4000-8000-000000000001":          "c0ffee00",
		"{c0ffee00-0000-4000-8000-000000000001}":        "c0ffee00",
		"\x1bc0000000-0000-4000-8000-000000000001x":     "c0000000",
		"urn:uuid:c0ffee00-0000-4000-8000-000000000001": "c0ffee00",
		"site-a-sw-2\x1b]52;c;cHduZWQ=\x07":             `site-a-sw-2\x1b]52;c;cHduZWQ=\x07`,
	} {
		if got := workerLabel(in); got != want {
			t.Errorf("workerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFrame_LongPhaseNamesNotEllipsized is the regression for the
// fixed-narrow-column bug: the lifecycle/summary boxes were pinned to a
// hardcoded inner width (24) that couldn't hold the longest phase labels, so
// installing_bootloader and awaiting_phone_home rendered ellipsized
// ("installing_bootloa…") even on a wide terminal. The column width is now
// derived from the longest label + duration column, so at any width >=
// minTermCols the full names must appear, with no ellipsis anywhere in the
// frame.
func TestFrame_LongPhaseNamesNotEllipsized(t *testing.T) {
	// A deploy mid-installing_bootloader (a long-named phase is current and shows
	// a running duration in parens — the widest right-column case) and with
	// awaiting_phone_home still pending in the runway.
	lc := newLifecyclePane(model.OpDeploy)
	for _, p := range []struct {
		name string
		d    time.Duration
	}{
		{"power_cycle", 0}, {"pxe_booting", 2 * time.Second},
		{"dhcp_ack", 16 * time.Second}, {"ipxe_dhcp", 17 * time.Second},
		{"ipxe_loaded", 19 * time.Second}, {"agent_starting", 27 * time.Second},
		{"partitioning", 30 * time.Second}, {"rsyncing_os", 40 * time.Second},
		{"installing_bootloader", 50 * time.Second},
	} {
		lc.Observe(p.name, at(p.d))
	}
	now := fixedClock(at(50*time.Second + 87*time.Second)) // installing_bootloader running 1m27s
	lc.now = now

	in := frameInput{
		opType:    model.OpDeploy,
		machine:   "kworker-l34-1.pop1",
		opID:      "8b2e3f1a",
		startedAt: at(0),
		console:   newConsolePane(),
		lifecycle: lc,
		logLines:  nil,
		width:     minTermCols,
		height:    minRows,
		spinner:   "⠙",
		now:       now,
	}

	// Render across a representative span of widths from the minimum up to a wide
	// terminal; the derived column is content-sized, so full names must appear at
	// every width that clears the gate.
	for _, w := range []int{minTermCols, 140, 200} {
		in.width = w
		got := plain(renderFrame(in))
		if !strings.Contains(got, "installing_bootloader") {
			t.Errorf("width %d: installing_bootloader truncated/missing:\n%s", w, got)
		}
		if !strings.Contains(got, "awaiting_phone_home") {
			t.Errorf("width %d: awaiting_phone_home truncated/missing:\n%s", w, got)
		}
		// The summary "phase" row shows the current phase in full too.
		if !strings.Contains(got, "phase   installing_bootloader") {
			t.Errorf("width %d: summary phase row truncated installing_bootloader:\n%s", w, got)
		}
		// No ellipsis anywhere — the whole point of the fix.
		if strings.Contains(got, "…") {
			t.Errorf("width %d: frame still contains an ellipsis (truncation):\n%s", w, got)
		}
		// The current phase's running duration still renders flush.
		if !strings.Contains(got, "(1m27s)") {
			t.Errorf("width %d: current-phase running duration missing:\n%s", w, got)
		}
	}
}

// TestFrame_PanesFillWidth is a regression test: the right column
// (lifecycle + summary) and the logs box must expand to fill the full terminal
// width, not sit at the content-minimum with a gutter on the right. The console
// box is the only fixed-width pane (80-col emulator grid). We assert the bottom
// (logs) box and the right column both reach within a column of the right edge,
// at several widths above the minimum.
func TestFrame_PanesFillWidth(t *testing.T) {
	for _, w := range []int{minTermCols, 140, 200, 250} {
		in := midDeployFrame(w, minRows)
		got := plain(renderFrame(in))
		lines := strings.Split(got, "\n")
		// The widest rendered row must reach the usable width (width-1; the frame
		// budgets to width-1 to avoid cursor auto-wrap). Below the minimum the
		// console is fixed so the top row can't fill, but at/above the minimum the
		// logs box spans the joined top width and must reach the edge.
		maxW := 0
		for _, ln := range lines {
			if cw := lipgloss.Width(ln); cw > maxW {
				maxW = cw
			}
		}
		if maxW < w-1 {
			t.Errorf("width %d: panes do not fill width; widest row is %d cells (want >= %d):\n%s",
				w, maxW, w-1, got)
		}
		// No row may exceed the terminal width.
		for _, ln := range lines {
			if cw := lipgloss.Width(ln); cw > w {
				t.Errorf("width %d: row exceeds width: %d cells\n%q", w, cw, ln)
			}
		}
	}
}

// TestFrame_NoAutoWrapAtMinWidth pins the width-1 budget invariant at the exact
// gate minimum: when the right column fills the terminal,
// the widest rendered row must be width-1, never the full width — writing
// exactly `width` columns trips cursor auto-wrap on some terminals and injects a
// blank line. The gate's minW (+5 over the bare pane math) exists precisely so
// the floored right column still leaves the 1-column margin at the minimum.
func TestFrame_NoAutoWrapAtMinWidth(t *testing.T) {
	for _, opType := range []model.OperationType{model.OpDeploy, model.OpCommission} {
		// The exact gate minimum for this op type (mirrors renderFrame's minW).
		minW := max(consoleWidth+5+lifecycleColW(opType), minTermCols)
		lc := newLifecyclePane(opType)
		lc.now = fixedClock(at(time.Minute))
		in := frameInput{
			opType:    opType,
			machine:   "host.pop1",
			opID:      "abc123",
			startedAt: at(0),
			console:   newConsolePane(),
			lifecycle: lc,
			width:     minW,
			height:    minRows,
			spinner:   "⠙",
			now:       fixedClock(at(time.Minute)),
		}
		got := plain(renderFrame(in))
		if strings.Contains(got, "enlarge") {
			t.Fatalf("%s: width %d should clear the gate, got the placeholder:\n%s", opType, minW, got)
		}
		maxW := 0
		for ln := range strings.SplitSeq(got, "\n") {
			if cw := lipgloss.Width(ln); cw > maxW {
				maxW = cw
			}
		}
		if maxW > minW-1 {
			t.Errorf("%s: widest row %d exceeds the width-1 budget (%d) at the gate minimum:\n%s",
				opType, maxW, minW-1, got)
		}
	}
}

// TestFrame_WorkerNotTruncatedWhenWide is an acceptance test: a long
// worker FQDN that doesn't fit the content-minimum summary box must render in
// full once the terminal is wide enough, because the summary box now grows with
// the terminal width.
func TestFrame_WorkerNotTruncatedWhenWide(t *testing.T) {
	const longWorker = "site-worker-2.a-long-subdomain.internal.example.net"
	in := midDeployFrame(220, minRows)
	in.worker = longWorker
	got := plain(renderFrame(in))
	if !strings.Contains(got, longWorker) {
		t.Errorf("wide terminal should render the full worker hostname %q:\n%s", longWorker, got)
	}
}

// TestFrame_Success: header reads "✓ completed in Ns", logs box stays "logs".
func TestFrame_Success(t *testing.T) {
	in := midDeployFrame(minTermCols, minRows)
	in.done = true
	got := plain(renderFrame(in))
	if !strings.Contains(got, "completed in") {
		t.Errorf("success header missing:\n%s", got)
	}
	if !strings.Contains(got, "logs (control plane") {
		t.Errorf("success should keep the logs title, not error:\n%s", got)
	}
}

// TestHeader_ElapsedNeverTruncated is a regression test: the long worker
// FQDN used to push elapsed off the end of the header ("elapsed 2m4…"). Worker
// is gone from the header now, and the command (the long token) is what
// truncates — never elapsed. Drive a deliberately long command across realistic
// widths and assert the full elapsed value survives in the header every time.
func TestHeader_ElapsedNeverTruncated(t *testing.T) {
	in := midDeployFrame(minTermCols, minRows)
	in.command = "machine deploy a-really-long-machine-hostname.internal.site2.example.net --destructive --some-extra-flag"
	for _, w := range []int{minTermCols, 130, 160, 200} {
		in.width = w
		hdr := headerLine(plain(renderFrame(in)))
		// elapsed in midDeployFrame is 1m17s; the whole token must be present.
		if !strings.Contains(hdr, "elapsed 1m17s") {
			t.Errorf("width %d: elapsed truncated/missing from header:\n%q", w, hdr)
		}
		if !strings.Contains(hdr, "[q] quit") {
			t.Errorf("width %d: quit affordance truncated from header:\n%q", w, hdr)
		}
		if lipgloss.Width(hdr) > w {
			t.Errorf("width %d: header overflows: visible=%d\n%q", w, lipgloss.Width(hdr), hdr)
		}
	}
}

// TestHeader_GlobalSpinner is the guard: a global liveness spinner
// is the header's lead glyph while the op is non-terminal, independent of phase
// state — so "UI alive" shows even when no lifecycle phase is current. On a
// terminal state the lead becomes the ✓/✗ outcome glyph (not the spinner).
func TestHeader_GlobalSpinner(t *testing.T) {
	// Non-terminal, and crucially with NO current phase (all pending) — the
	// case where the per-phase spinner has nowhere to draw. The global spinner
	// must still show.
	lc := newLifecyclePane(model.OpCommission)
	lc.now = fixedClock(at(2 * time.Minute))
	in := frameInput{
		opType:    model.OpCommission,
		machine:   "hot-spare-1.pop2",
		opID:      "abc123",
		startedAt: at(0),
		console:   newConsolePane(),
		lifecycle: lc,
		width:     minTermCols,
		height:    minRows,
		spinner:   "⠙",
		now:       fixedClock(at(2 * time.Minute)),
	}
	hdr := headerLine(plain(renderFrame(in)))
	if !strings.Contains(hdr, "⠙") {
		t.Errorf("non-terminal header should carry the global spinner glyph:\n%q", hdr)
	}

	// Terminal success: lead is ✓, no spinner.
	in.done = true
	hdr = headerLine(plain(renderFrame(in)))
	if strings.Contains(hdr, "⠙") {
		t.Errorf("terminal header should not show the spinner:\n%q", hdr)
	}
	if !strings.Contains(hdr, "✓") {
		t.Errorf("terminal-success header should lead with ✓:\n%q", hdr)
	}
}

// TestSummary_LastUpdateAgo is the guard: the summary carries a
// "last update <N>s ago" staleness field, "—" before any event. midDeployFrame
// sets lastEventAt to t0+72s (the newest log line's translate would have
// stamped it; here we set it directly) and reads now at t0+77s -> "5s ago".
func TestSummary_LastUpdateAgo(t *testing.T) {
	in := midDeployFrame(minTermCols, minRows)
	in.lastEventAt = at(72 * time.Second) // last activity 5s before now (77s)
	got := plain(renderFrame(in))
	if !strings.Contains(got, "update") {
		t.Errorf("summary missing the last-update row:\n%s", got)
	}
	if !strings.Contains(got, "5s ago") {
		t.Errorf("summary last-update should read 5s ago:\n%s", got)
	}

	// No event yet -> em dash, not a bogus "0s ago".
	in.lastEventAt = time.Time{}
	got = plain(renderFrame(in))
	if strings.Contains(got, "ago") {
		t.Errorf("with no event observed, last-update should be '—', not an age:\n%s", got)
	}
}
