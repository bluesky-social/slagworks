package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Property-based coverage for the deploy-watch reducer: the lifecycle spine,
// the composed frame and the run result, driven by random histories of the
// messages the log stream, the op-row poll and the terminal deliver. The
// contracts checked after every message are:
//   - the spine matches an independent reference model of the documented
//     phase contract (lifecycle.go and model.go's terminal handling), with at
//     most one spinning phase and no reached phase behind a pending one
//   - once the op is terminal the screen is final: nothing spins, a late
//     phase or a second terminal changes neither the spine nor the frozen
//     total and cause, and a completed op shows every phase done or skipped
//   - only the watched op's envelopes (or envelopes with no op id) move the
//     spine, set the worker or end the watch; another op's are log lines
//   - every distinct log envelope is one log pane row, and no row carries a
//     line break or a terminal control
//   - the composed frame fits the terminal, shows the newest log line, and
//     carries no escape sequence other than slagctl's own SGR styling
//   - the run result agrees with the terminal signals delivered
//
// The reference spines below are transcribed from docs/dev/architecture.md and
// the emission points, not read from deploySpine/commissionSpine, so a spine
// edit that drops or reorders a phase is caught rather than mirrored.
var (
	refDeploySpine = []string{
		"power_cycle", "pxe_booting", "dhcp_ack", "ipxe_dhcp", "ipxe_loaded",
		"agent_starting", "partitioning", "rsyncing_os", "installing_bootloader",
		"finalizing", "bmc_reconcile", "awaiting_phone_home", "completed",
	}
	refCommissionSpine = []string{
		"power_cycle", "pxe_booting", "dhcp_ack", "ipxe_dhcp", "ipxe_loaded",
		"agent_starting", "collecting_inventory", "reporting", "console_probe",
		"completed",
	}
	refBMCSpine = []string{"bmc_reconcile", "completed"}
)

func refSpine(t model.OperationType) []string {
	switch t {
	case model.OpCommission:
		return refCommissionSpine
	case model.OpBMCReconcile:
		return refBMCSpine
	default:
		return refDeploySpine
	}
}

// phasePool is every known phase from every spine, plus junk, so an event for
// the wrong spine (a commission phase during a deploy) and unknown phases both
// appear.
var phasePool = func() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range [][]string{refDeploySpine, refCommissionSpine, refBMCSpine} {
		for _, p := range s {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return append(out, "bogus_phase", "failed", "PARTITIONING")
}()

// Row statuses are read from the rendered glyph, so the oracle reads what an
// operator sees rather than the pane's internal enum.
const (
	gPending = "○"
	gDone    = "✓"
	gFailed  = "✗"
	gSkipped = "—"
	gCurrent = "@" // the spinner glyph the harness passes to Render
)

// refLifecycle is the independent model of the documented spine contract. It
// encodes the terminal contract the operator relies on: once the op is
// terminal the screen is final (frameNow and markEventAt freeze everything
// else, and a held screen must not resume animating a phase).
type refLifecycle struct {
	spine    []string
	status   []string
	terminal bool
	failed   bool
}

func newRefLifecycle(t model.OperationType) *refLifecycle {
	sp := refSpine(t)
	st := make([]string, len(sp))
	for i := range st {
		st[i] = gPending
	}
	return &refLifecycle{spine: sp, status: st}
}

func (r *refLifecycle) idx(p string) int {
	for i, n := range r.spine {
		if n == p {
			return i
		}
	}
	return -1
}

func (r *refLifecycle) observe(p string) {
	if r.terminal {
		return
	}
	i := r.idx(p)
	if i < 0 || r.status[i] != gPending {
		return
	}
	for j := range r.status {
		if r.status[j] == gCurrent {
			r.status[j] = gDone
		}
	}
	for j := 0; j < i; j++ {
		if r.status[j] == gPending {
			r.status[j] = gSkipped
		}
	}
	if p == "completed" {
		r.status[i] = gDone
	} else {
		r.status[i] = gCurrent
	}
}

func (r *refLifecycle) terminate(failed bool) {
	if r.terminal {
		return
	}
	if failed {
		for j := range r.status {
			if r.status[j] == gCurrent {
				r.status[j] = gFailed
			}
		}
	} else {
		r.observe("completed")
	}
	r.terminal = true
	r.failed = failed
}

type tuiOp struct {
	kind   string
	phase  string
	dt     time.Duration // clock advance before the op
	tsSkew time.Duration // envelope TS relative to the clock (may be negative)
	failed bool
	other  bool // event belongs to a different op on the same machine
	noOpID bool // event of the watched op stamped with no op id
	text   string
	w, h   int
}

func (o tuiOp) String() string {
	return fmt.Sprintf("%s(phase=%q dt=%s skew=%s failed=%v other=%v noOpID=%v w=%d h=%d text=%q)",
		o.kind, o.phase, o.dt, o.tsSkew, o.failed, o.other, o.noOpID, o.w, o.h, o.text)
}

// hostileText mixes wide runes, combining marks, control characters and ANSI
// sequences (CSI, OSC 52 clipboard write, OSC 8 hyperlink, bare ESC).
var hostileText = rapid.OneOf(
	rapid.String(),
	rapid.SampledFrom([]string{
		"plain message",
		"\x1b[31mred\x1b[0m",
		"\x1b]52;c;ZWNobyBwd25lZAo=\x07",
		"\x1b]8;;http://evil/\x07click\x1b]8;;\x07",
		"\x1b[2J\x1b[H",
		"中文主机名宽字符中文主机名宽字符中文主机名宽字符中文主机名宽字符",
		"ééé",
		"tab\there\r\nnewline",
		"progress 10%\rprogress 90%\r",
		"\x1b",
		"\x00\x07\x08\x7f",
		"\u009b31m",
		strings.Repeat("x", 400),
	}),
)

func genTUIOp(opType model.OperationType) *rapid.Generator[tuiOp] {
	return rapid.Custom(func(t *rapid.T) tuiOp {
		kind := rapid.SampledFrom([]string{
			"phase", "phase", "phase", "phase", "phase", "phase",
			"global_status", "poll_terminal", "claim", "tick", "resize", "global_nonterminal",
			"console", "log", "key", "redeliver",
		}).Draw(t, "kind")
		op := tuiOp{
			kind:   kind,
			dt:     time.Duration(rapid.IntRange(0, 400).Draw(t, "dt")) * time.Second,
			tsSkew: time.Duration(rapid.IntRange(-120, 30).Draw(t, "skew")) * time.Second,
		}
		switch rapid.IntRange(0, 9).Draw(t, "attribution") {
		case 0:
			op.other = true
		case 1:
			op.noOpID = true
		}
		switch kind {
		case "phase":
			// Bias toward the op's own spine so histories progress.
			if rapid.Bool().Draw(t, "ownSpine") {
				op.phase = rapid.SampledFrom(refSpine(opType)).Draw(t, "phaseOwn")
			} else {
				op.phase = rapid.SampledFrom(phasePool).Draw(t, "phaseAny")
			}
			op.text = hostileText.Draw(t, "msg")
		case "global_status", "poll_terminal":
			op.failed = rapid.Bool().Draw(t, "failed")
			op.text = hostileText.Draw(t, "reason")
		case "claim", "log", "console":
			op.text = hostileText.Draw(t, "text")
		case "resize":
			if rapid.Bool().Draw(t, "fullSize") {
				op.w = rapid.IntRange(118, 200).Draw(t, "wFull")
				op.h = rapid.IntRange(40, 80).Draw(t, "hFull")
			} else {
				op.w = rapid.IntRange(0, 260).Draw(t, "w")
				op.h = rapid.IntRange(0, 90).Draw(t, "h")
			}
		}
		return op
	})
}

// TestModel_Property_WatchReducer runs one rapid.Check over random message
// histories, asserting the contracts listed at the top of this file after
// every message.
func TestModel_Property_WatchReducer(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		opType := rapid.SampledFrom([]model.OperationType{model.OpDeploy, model.OpCommission, model.OpBMCReconcile}).Draw(t, "opType")
		watched := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
		other := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
		machineID := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
		hold := rapid.Bool().Draw(t, "hold")
		// Terminal status is a property of the op: every terminal signal for
		// the watched op (stream envelope or poll backstop) agrees.
		opFails := rapid.Bool().Draw(t, "opFails")

		clock := t0
		now := func() time.Time { return clock }
		m := newModelAt(opType, "host", watched.String()[:8], t0, time.UTC, now)
		m.holdOnTerminal = hold
		ref := newRefLifecycle(opType)
		refWorker := ""

		if rapid.Bool().Draw(t, "startSized") {
			m.width, m.height = 140, 50
		}
		ops := rapid.SliceOfN(genTUIOp(opType), 8, 80).Draw(t, "ops")
		var nextID int64
		var lastLogMsg tea.Msg
		logIDs := map[int64]bool{}
		// The first terminal signal fixes the final screen's total and cause.
		var terminalSeen bool
		var finalEnd time.Time
		var finalReason string
		for i, op := range ops {
			clock = clock.Add(op.dt)
			ts := clock.Add(op.tsSkew)
			opID := watched
			switch {
			case op.other:
				opID = other
			case op.noOpID:
				opID = uuid.Nil
			}
			var msg tea.Msg
			switch op.kind {
			case "phase":
				nextID++
				env := slogkit.Envelope{TS: ts, Source: slogkit.SourceAgent, Msg: op.text,
					MachineID: machineID, OpID: opID, Phase: op.phase}
				msg = translateEnvelope(nextID, env, watched)
				if !op.other {
					ref.observe(op.phase)
				}
			case "global_status":
				nextID++
				failed := opFails
				if op.other {
					failed = op.failed
				}
				status := "completed"
				if failed {
					status = "failed"
				}
				env := slogkit.Envelope{TS: ts, Source: slogkit.SourceGlobal, Msg: "operation " + status,
					MachineID: machineID, OpID: opID,
					Extra: map[string]any{"status": status, "error": op.text}}
				msg = translateEnvelope(nextID, env, watched)
				if !op.other {
					ref.terminate(failed)
				}
			case "poll_terminal":
				// The op-row poll only ever reports the watched op.
				msg = opTerminalMsg{failed: opFails, reason: op.text, at: clock}
				ref.terminate(opFails)
			case "global_nonterminal":
				nextID++
				st := []string{"running", "claimed", "pending", "COMPLETED", " failed"}[int(op.dt/time.Second)%5]
				env := slogkit.Envelope{TS: ts, Source: slogkit.SourceGlobal, Msg: "status " + st,
					MachineID: machineID, OpID: opID, Extra: map[string]any{"status": st}}
				msg = translateEnvelope(nextID, env, watched)
			case "claim":
				nextID++
				worker := op.text + "x"
				env := slogkit.Envelope{TS: ts, Source: slogkit.SourceGlobal, Msg: "operation claimed",
					MachineID: machineID, OpID: opID, Extra: map[string]any{"claimed_by": worker}}
				msg = translateEnvelope(nextID, env, watched)
				if !op.other {
					refWorker = worker
				}
			case "log":
				nextID++
				src := slogkit.SourceSite
				if op.other {
					src = slogkit.SourceAgent
				}
				msg = translateEnvelope(nextID, slogkit.Envelope{TS: ts, Source: src, Msg: op.text,
					MachineID: machineID, OpID: opID, Host: op.text}, watched)
			case "redeliver":
				// Reconnect overlap: the stream resends an envelope the pane
				// already applied (same id).
				msg = lastLogMsg
				if msg == nil {
					msg = uiTickMsg{}
				}
			case "console":
				msg = consoleBatchMsg{data: []byte(op.text), fromBackfill: (op.dt/time.Second)%2 == 0}
			case "tick":
				msg = uiTickMsg{}
			case "resize":
				msg = tea.WindowSizeMsg{Width: op.w, Height: op.h}
			case "key":
				msg = tea.KeyPressMsg{Code: 'x', Text: "x"}
			}
			if lm, ok := msg.(logEnvelopeMsg); ok {
				lastLogMsg = lm
				logIDs[lm.id] = true
			}
			next, _ := m.Update(msg)
			m = next.(*tuiModel)
			checkTUIInvariants(t, i, op, m, ref, opType)
			if m.worker != refWorker {
				t.Fatalf("step %d %v: worker = %q, want %q (only the watched op's claim sets it)", i, op, m.worker, refWorker)
			}
			if n := len(m.logs.Lines()); n != min(len(logIDs), logRingCap) {
				t.Fatalf("step %d %v: log pane has %d lines, %d distinct envelopes delivered", i, op, n, len(logIDs))
			}
			switch {
			case m.done && !terminalSeen:
				terminalSeen, finalEnd, finalReason = true, m.endTime, m.failReason
			case terminalSeen && (!m.endTime.Equal(finalEnd) || m.failReason != finalReason):
				t.Fatalf("step %d %v: final screen rewritten after the terminal: end %v -> %v, reason %q -> %q",
					i, op, finalEnd, m.endTime, finalReason, m.failReason)
			}
		}
		// The run result agrees with the op's status iff a terminal for the
		// watched op was delivered.
		res := m.runResult()
		want := OutcomeIncomplete
		if ref.terminal {
			want = OutcomeCompleted
			if ref.failed {
				want = OutcomeFailed
			}
		}
		if res.Outcome != want {
			t.Fatalf("runResult outcome = %v, want %v (ref terminal=%v failed=%v)", res.Outcome, want, ref.terminal, ref.failed)
		}
	})
}

// frameSGR matches the Select Graphic Rendition sequences slagctl's own
// styles emit. Every other escape in the frame would have come from the data.
var frameSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// terminalControl reports a rune a terminal interprets rather than prints.
func terminalControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
}

func checkTUIInvariants(t *rapid.T, step int, op tuiOp, m *tuiModel, ref *refLifecycle, opType model.OperationType) {
	t.Helper()
	l := m.lifecycle
	out := l.Render(40, gCurrent)
	lines := strings.Split(out, "\n")
	if len(lines) != len(ref.spine) {
		t.Fatalf("step %d %v: render has %d rows, want %d", step, op, len(lines), len(ref.spine))
	}
	got := make([]string, len(lines))
	current := 0
	for i, ln := range lines {
		pl := ansi.Strip(ln)
		g := string([]rune(pl)[0])
		got[i] = g
		if g == gCurrent {
			current++
		}
		if w := ansi.StringWidth(ln); w > 40 {
			t.Fatalf("step %d: row %d width %d > 40: %q", step, i, w, pl)
		}
		if !strings.Contains(pl, ref.spine[i]) && !strings.Contains(pl, "…") {
			t.Fatalf("step %d: row %d %q does not name spine phase %q", step, i, pl, ref.spine[i])
		}
	}
	// At most one spinning phase, and CurrentPhase names it.
	if current > 1 {
		t.Fatalf("step %d %v: %d current rows: %v", step, op, current, got)
	}
	wantCur := ""
	for i, g := range got {
		if g == gCurrent {
			wantCur = ref.spine[i]
		}
	}
	if l.CurrentPhase() != wantCur {
		t.Fatalf("step %d %v: CurrentPhase()=%q but spinning row is %q", step, op, l.CurrentPhase(), wantCur)
	}
	// Runway prefix: a jump forward backfills the phases it passed as
	// skipped, and a failure leaves only the runway after it pending, so no
	// reached phase ever follows a pending one.
	seenPending := false
	for i, g := range got {
		if g == gPending {
			seenPending = true
			continue
		}
		if seenPending {
			t.Fatalf("step %d %v: row %d (%s) reached after an earlier pending row: %v", step, op, i, ref.spine[i], got)
		}
	}
	// Terminal screens.
	if m.done {
		if cp := l.CurrentPhase(); cp != "" {
			t.Fatalf("step %d %v: op is terminal but CurrentPhase()=%q (summary would show it live)", step, op, cp)
		}
		if current != 0 {
			t.Fatalf("step %d %v: op is terminal (failed=%v) but a phase is still spinning: %v", step, op, m.failed, got)
		}
		if !m.failed {
			for i, g := range got {
				if g != gDone && g != gSkipped {
					t.Fatalf("step %d %v: completed op shows row %d (%s) as %q: %v", step, op, i, ref.spine[i], g, got)
				}
			}
		}
	}
	// Differential against the reference.
	for i := range got {
		if got[i] != ref.status[i] {
			t.Fatalf("step %d %v: spine diverges from reference at %s: got %v want %v", step, op, ref.spine[i], got, ref.status)
		}
	}
	if m.done != ref.terminal {
		t.Fatalf("step %d %v: model done=%v, reference terminal=%v (failed=%v)", step, op, m.done, ref.terminal, ref.failed)
	}
	if m.done && m.failed != ref.failed {
		t.Fatalf("step %d %v: model failed=%v, reference failed=%v", step, op, m.failed, ref.failed)
	}
	// Each log pane row is one envelope on one row, with every control the
	// sender put in it made visible.
	for i, ln := range m.logs.Lines() {
		if strings.ContainsFunc(ln, terminalControl) {
			t.Fatalf("step %d %v: log pane row %d carries a terminal control: %q", step, op, i, ln)
		}
	}
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// The composed frame never panics and carries no escape sequence or
	// control other than slagctl's own styling and its row breaks.
	frame := renderFrame(m.frameInput())
	for i, ln := range strings.Split(frame, "\n") {
		if strings.ContainsFunc(frameSGR.ReplaceAllString(ln, ""), terminalControl) {
			t.Fatalf("step %d %v: frame line %d carries a terminal control from the data: %q", step, op, i, ln)
		}
	}
	if m.width < minWidthFor(opType) || m.height < minRows {
		return
	}
	// The newest control-plane line is on screen (tail, not stale). The
	// error pane of a failed op pins the cause above the tail instead.
	errorPane := m.done && m.failed
	if lines := m.logs.Lines(); len(lines) > 0 && !errorPane {
		last := lines[len(lines)-1]
		ascii := true
		for _, r := range last {
			if r < 0x20 || r > 0x7e {
				ascii = false
			}
		}
		if ascii {
			want := last[:min(len(last), 30)]
			if !strings.Contains(ansi.Strip(frame), want) {
				t.Fatalf("step %d: newest log line %q not on screen", step, want)
			}
		}
	}
	// The frame fits the terminal.
	fl := strings.Split(frame, "\n")
	if len(fl) > m.height {
		t.Fatalf("step %d %v: frame has %d lines > height %d", step, op, len(fl), m.height)
	}
	for i, ln := range fl {
		if w := ansi.StringWidth(ln); w >= m.width {
			t.Fatalf("step %d %v: frame line %d width %d >= terminal width %d: %q", step, op, i, w, m.width, ansi.Strip(ln))
		}
	}
}
