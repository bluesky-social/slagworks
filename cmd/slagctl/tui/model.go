package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// This file is the bubbletea Model: the reducer that ties the three panes and
// renderFrame together. Per the plan's Tier-1 strategy it is a pure function
// of (state, msg) — every external input arrives as a scripted tea.Msg, so the
// whole model is golden-testable by calling Update() directly with no
// goroutine and no wall clock (the clock is injected, plan D9).
//
// The WS goroutines that PRODUCE these messages (console cast + logs stream),
// the op-row liveness poll, and the D6 server prerequisite that makes
// completion reliable all live in the wiring slice (model_wire.go, next). This
// file only defines the message vocabulary and the reduction.

// --- message vocabulary ---

// consoleBatchMsg carries a batch of decoded SOL bytes from the console-stream
// goroutine. Per D2 the goroutine batches at ~30Hz and NEVER touches the
// emulator; Update owns the emulator and drains these bytes under a per-tick
// time budget. fromBackfill marks bytes that are part of the initial
// history replay (so the model can hold the catch-up placeholder until the
// backfill drains rather than flashing intermediate screens).
type consoleBatchMsg struct {
	data         []byte
	fromBackfill bool
}

// consoleBackfillDoneMsg signals the initial history replay has been fully
// received by the goroutine (live edge reached). The model clears the
// catch-up placeholder once its pending buffer is also drained.
type consoleBackfillDoneMsg struct{}

// logEnvelopeMsg carries one control-plane log envelope (id + envelope) parsed
// off the logs stream. The model appends it to the log pane (which formats it
// through the injected timezone) and, if it carries a phase, advances the
// lifecycle. Carrying the envelope — not a pre-formatted string — keeps the
// D9 timezone seam inside the pane, the single place that owns formatting.
// otherOp marks an envelope of a different op on the same machine
// (translateEnvelope): it is logged, but its phase is not this op's.
type logEnvelopeMsg struct {
	id      int64
	env     slogkit.Envelope
	otherOp bool
}

// phaseMsg advances the lifecycle spine. Phase is a field on log envelopes
// (D3), but terminal transitions (completed/failed) and the claim event arrive
// as distinct semantic signals, so the wiring layer translates envelopes into
// the right message rather than the model re-parsing strings.
type phaseMsg struct {
	phase string
	at    time.Time
}

// opTerminalMsg ends the run. failed=false is success (auto-exit); failed=true
// holds the screen when interactive (gated in the wiring layer). reason is the
// one-line cause pinned in the error pane.
type opTerminalMsg struct {
	failed bool
	reason string
	at     time.Time
}

// claimedMsg sets the claiming site worker once it's known (D6 claim envelope,
// or the startup op-row backfill).
type claimedMsg struct{ worker string }

// consoleReconnectMsg toggles the console-pane reconnect banner (D4/S3).
type consoleReconnectMsg struct{ reconnecting bool }

// drainTickMsg is the model's self-scheduled follow-up when the console buffer
// still has bytes after a budgeted drain — it asks for another drain pass on
// the next frame so a backlog catches up across consecutive frames (D2).
type drainTickMsg struct{}

// --- model ---

// consoleDrainBudget bounds how long Update spends writing pending console
// bytes into the emulator per tick (D2): a multi-MB backfill written in one
// emu.Write would freeze the loop for seconds. 8ms keeps each Update cheap and
// well under a 30Hz (33ms) frame.
const consoleDrainBudget = 8 * time.Millisecond

type tuiModel struct {
	// identity / header
	opType    model.OperationType
	machine   string
	opID      string
	command   string // human-readable invocation; shown in place of the raw op UUID
	worker    string // claiming worker, resolved to hostname
	startedAt time.Time

	// resolveWorker maps a claim envelope's worker UUID to its hostname. Nil in
	// tests and when the caller didn't supply a resolver.
	resolveWorker func(string) string

	// panes
	console   *consolePane
	lifecycle *lifecyclePane
	logs      *logPane

	// console drain state (D2)
	pending      []byte // bytes received but not yet written to the emulator
	catchingUp   bool   // initial backfill still draining
	backfillSeen int    // bytes of backfill received (for the progress hint)
	backfillDone bool   // goroutine reported live edge reached

	consoleReconnecting bool

	// lastEventAt is the wall-clock time of the most recent inbound activity:
	// any console byte, control-plane log line, phase advance, claim, or
	// terminal signal. The summary's "last update <N>s ago" is computed against
	// it (now() - lastEventAt) and ticks up via the heartbeat when nothing
	// arrives — the single clearest "is anything still happening" signal for a
	// stalled host. Reset on every inbound event in Update. Zero until the first
	// event, which renders as "—" (nothing observed yet).
	lastEventAt time.Time

	// terminal state
	done       bool
	failed     bool
	failReason string
	endTime    time.Time

	// holdOnTerminal, when true, keeps the screen up on ANY terminal state
	// (success or failure) until the operator presses q, instead of
	// auto-Quitting. Resolved once by the runner from (interactivity,
	// --hold/--no-hold) via decideHold: true for an interactive operator (who
	// quits explicitly), false for a piped/CI invocation (so scripts don't
	// hang). The reducer applies it on opTerminalMsg.
	holdOnTerminal bool

	// viewport size
	width  int
	height int

	// D9 seams
	now        func() time.Time
	spinnerF   string // current spinner glyph (advanced by the UI tick; fixed in tests)
	spinnerIdx int    // index into spinnerFrames, advanced each uiTickMsg
}

// spinnerFrames is the braille spinner (bubbles/spinner MiniDot). The model
// advances through these on the UI tick rather than embedding a
// spinner.Model, so the glyph stays a plain string the golden tests pin.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// uiTickInterval drives the heartbeat re-render: without it the View only
// repaints when a console/log/phase message arrives, so on an idle host (and
// between events mid-deploy) the elapsed timers freeze and the spinner stalls.
// 10Hz is smooth for the spinner and far finer than the 1s elapsed display
// needs.
const uiTickInterval = 100 * time.Millisecond

// uiTickMsg is the periodic heartbeat. It advances the spinner and forces a
// re-render so time-derived fields (elapsed, phase-since) stay live.
type uiTickMsg struct{}

// uiTick schedules the next heartbeat. Real timer — not invoked by the pure
// golden tests, which drive Update directly with scripted messages.
func uiTick() tea.Cmd {
	return tea.Tick(uiTickInterval, func(time.Time) tea.Msg { return uiTickMsg{} })
}

// newModel builds the reducer with the op anchored to the client's attach time.
// Prefer newModelAt with the server op-create time; this overload exists for
// tests that don't care about the header total's epoch.
func newModel(opType model.OperationType, machine, opID string, loc *time.Location, now func() time.Time) *tuiModel {
	return newModelAt(opType, machine, opID, time.Time{}, loc, now)
}

// newModelAt builds the reducer. opStartedAt is the operation's authoritative
// server-side start time (op.CreatedAt); the header "elapsed" total and the
// summary "total" both anchor on it, so they share the same time base as the
// phase durations (which come from server envelope timestamps). This is what
// guarantees total elapsed >= any single phase duration when an operator
// attaches to an already-running op. A zero opStartedAt falls back to the
// client attach time (now()) — only when the caller genuinely has no op-create
// time. now defaults to time.Now; tests inject a fixed clock. The panes are
// constructed here so the model owns them (single-owner invariant for the
// emulator).
func newModelAt(opType model.OperationType, machine, opID string, opStartedAt time.Time, loc *time.Location, now func() time.Time) *tuiModel {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.Local
	}
	startedAt := opStartedAt
	if startedAt.IsZero() {
		startedAt = now()
	}
	return &tuiModel{
		opType:     opType,
		machine:    machine,
		opID:       opID,
		startedAt:  startedAt,
		console:    newConsolePane(),
		lifecycle:  newLifecyclePaneClock(opType, now),
		logs:       newLogPane(loc),
		catchingUp: true, // assume a backfill until the goroutine says otherwise
		now:        now,
		spinnerF:   spinnerFrames[0],
	}
}

func (m *tuiModel) Init() tea.Cmd { return uiTick() }

// Update is the pure reducer. It returns the model and any follow-up command;
// in tests the command is ignored and messages are fed directly.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
		return m, nil

	case uiTickMsg:
		// Heartbeat: advance the spinner and re-render so time-derived fields
		// stay live. Stop ticking once the op is terminal — the clock is
		// frozen at endTime then, so there's nothing to animate and no reason
		// to keep waking the loop while the operator reads the final screen.
		if m.done {
			return m, nil
		}
		m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
		m.spinnerF = spinnerFrames[m.spinnerIdx]
		return m, uiTick()

	case consoleBatchMsg:
		// Only LIVE bytes count as "activity now" for the staleness clock.
		// Backfill bytes (the server-rendered screen snapshot on attach)
		// represent history of unknown age — stamping lastEventAt=now() for
		// them would falsely reset "last update" to 0 on attach (the bug 1
		// report). The real last-activity time comes from the log stream's
		// envelope timestamps; live console bytes refine it from there.
		if !msg.fromBackfill {
			m.markEvent()
		}
		m.pending = append(m.pending, msg.data...)
		if msg.fromBackfill {
			m.backfillSeen += len(msg.data)
		}
		return m, m.drain()

	case drainTickMsg:
		return m, m.drain()

	case consoleBackfillDoneMsg:
		// Do NOT markEvent here: backfill-done is a purely client-side signal
		// that the snapshot/history replay drained and we've reached the live
		// edge — it carries no machine activity and no timestamp. Stamping the
		// staleness clock here would falsely reset "last update" to 0s on attach
		// to an idle op, the same bug-1 trap the consoleBatchMsg handler avoids
		// for backfill bytes. The real last-activity time stays driven by the log
		// stream's envelope timestamps and live console bytes.
		m.backfillDone = true
		// If nothing is pending we're caught up immediately; otherwise the
		// next drain clears catchingUp when the buffer empties.
		if len(m.pending) == 0 {
			m.catchingUp = false
		}
		return m, nil

	case consoleReconnectMsg:
		m.markEvent()
		m.consoleReconnecting = msg.reconnecting
		return m, nil

	case logEnvelopeMsg:
		// Stamp the staleness clock with the envelope's REAL server timestamp,
		// not now() — on attach the backfilled envelopes carry their true
		// (possibly minutes-old) timestamps, so "last update" reflects the true
		// age of the last activity rather than resetting to ~0 on connect (bug 1).
		m.markEventAt(msg.env.TS)
		m.logs.Append(msg.id, msg.env)
		if msg.env.Phase != "" && !msg.otherOp {
			m.lifecycle.Observe(msg.env.Phase, msg.env.TS)
		}
		return m, nil

	case phaseMsg:
		m.markEventAt(msg.at)
		m.lifecycle.Observe(msg.phase, msg.at)
		return m, nil

	case claimedMsg:
		m.markEvent()
		// The claim envelope carries a worker UUID; resolve to a hostname when a
		// resolver is wired (nil in tests / when unresolvable).
		w := msg.worker
		if m.resolveWorker != nil {
			w = m.resolveWorker(w)
		}
		m.worker = w
		return m, nil

	case opTerminalMsg:
		m.done = true
		m.failed = msg.failed
		m.failReason = msg.reason
		m.endTime = msg.at
		// Anchor the staleness clock to the SAME time base as endTime (the
		// server-provided terminal timestamp), not the client wall clock. The
		// frozen frame reads now()=endTime (frameNow), so computing the age as
		// endTime - lastEventAt must keep both on the server clock — stamping
		// lastEventAt with the client now() instead would subtract across clocks
		// and, under skew, render a bogus inflated age or a misleading "0s ago".
		// This mirrors the elapsed-anchoring invariant (both derive from server
		// timestamps, never the attach-time client clock).
		m.lastEventAt = msg.at
		if msg.failed {
			m.lifecycle.Fail(msg.at)
		} else {
			// Completion is a source=global terminal envelope (D6), not a
			// phase=completed entry, so drive the spine's terminal phase here
			// to close the runway (finalizes the last in-flight phase and
			// marks `completed` done).
			m.lifecycle.Observe(terminalCompleted, msg.at)
		}
		// Hold decision (resolved once by the runner, the same for success and
		// failure): an interactive operator explicitly quits with q, so the
		// screen holds on ANY terminal state and does not vanish on its own.
		// Non-interactive invocations auto-exit so scripts don't hang. The
		// held screen stays up until the KeyPressMsg case Quits.
		if m.holdOnTerminal {
			return m, nil
		}
		return m, tea.Quit
	}
	return m, nil
}

// drain writes pending console bytes into the emulator under the per-tick time
// budget (D2). It writes in small chunks, checking the clock between them, and
// stops when the budget is spent — leaving the remainder for the next tick and
// scheduling an immediate follow-up drainTick so a backlog catches up across
// frames. Returns nil when the buffer is empty.
func (m *tuiModel) drain() tea.Cmd {
	if len(m.pending) == 0 {
		if m.backfillDone {
			m.catchingUp = false
		}
		return nil
	}
	// Chunk size: small enough that a single emu.Write can't overrun the
	// budget by much even on slow ANSI, big enough to make progress. 4KB is a
	// few terminal screens of bytes.
	const chunk = 4096
	deadline := m.now().Add(consoleDrainBudget)
	for len(m.pending) > 0 {
		n := min(chunk, len(m.pending))
		m.console.Apply(m.pending[:n])
		m.pending = m.pending[n:]
		if m.now().After(deadline) {
			break
		}
	}
	if len(m.pending) > 0 {
		// More to do — ask for another pass next frame.
		return func() tea.Msg { return drainTickMsg{} }
	}
	if m.backfillDone {
		m.catchingUp = false
	}
	return nil
}

// markEvent stamps lastEventAt with the current clock, recording that inbound
// activity arrived now. Every inbound message handler that represents real
// progress (console bytes, a log line, a phase, a claim, a reconnect toggle, a
// terminal signal) calls it, so the summary's "last update <N>s ago" resets to
// 0 on activity and the heartbeat ticks it up otherwise. NOT called by the
// uiTick heartbeat or the self-scheduled drainTick — those aren't new inbound
// activity, and stamping on them would peg the staleness clock at 0 forever.
//
// Skips the stamp once the op is terminal: on a held interactive screen the
// stream goroutines keep running, so a trailing late envelope could arrive
// after endTime. The display clock freezes at endTime (frameNow), so a
// post-endTime lastEventAt would make now()-lastEventAt go negative and clamp
// to a misleading "0s ago". Freezing the staleness with everything else keeps
// the final screen showing the true last-update age at completion.
func (m *tuiModel) markEvent() {
	m.markEventAt(m.now())
}

// markEventAt stamps lastEventAt with a specific activity time (an envelope's
// server timestamp), keeping the LATEST seen so an out-of-order or backfilled
// envelope can't drag the staleness clock backward. A timestamp in the future
// (client/server skew, or a zero/garbage TS) is clamped to now() so "last
// update" never reads negative or a bogus far-future age. Skips once terminal
// (see markEvent's original rationale): on a held screen the stream goroutines
// keep running, and the display clock is frozen at endTime, so a trailing late
// envelope must not restamp past it.
func (m *tuiModel) markEventAt(t time.Time) {
	if m.done {
		return
	}
	now := m.now()
	if t.IsZero() || t.After(now) {
		t = now
	}
	if t.After(m.lastEventAt) {
		m.lastEventAt = t
	}
}

// View renders the frame on the alternate screen. MouseMode stays at its zero
// value (MouseModeNone): the reducer consumes no mouse events, and mouse
// reporting would suppress the terminal's native click-drag text selection, so
// the operator couldn't copy a MAC, op id or error line out of the TUI. If
// scrollback is added and wants drag-to-scroll on the log/console panes,
// enable mouse capture behind a keybind toggle (app-owns-mouse vs
// terminal-owns-mouse) rather than unconditionally, so selection still works
// on demand.
func (m *tuiModel) View() tea.View {
	v := tea.NewView(renderFrame(m.frameInput()))
	v.AltScreen = true
	return v
}

// frameInput projects the model into the pure render input. Split out from
// View so tests can assert on derived values (e.g. the elapsed/phase-duration
// invariant) without scraping the rendered string.
func (m *tuiModel) frameInput() frameInput {
	in := frameInput{
		opType:              m.opType,
		machine:             m.machine,
		opID:                m.opID,
		command:             m.command,
		worker:              m.worker,
		startedAt:           m.startedAt,
		lastEventAt:         m.lastEventAt,
		console:             m.console,
		lifecycle:           m.lifecycle,
		logLines:            m.logs.Lines(),
		consoleReconnecting: m.consoleReconnecting,
		done:                m.done,
		failed:              m.failed,
		failReason:          m.failReason,
		width:               m.width,
		height:              m.height,
		spinner:             m.spinnerF,
		now:                 m.frameNow,
	}
	if m.catchingUp {
		in.consoleCatchUp = catchUpHint(m.backfillSeen)
	}
	return in
}

// catchUpHint renders the console placeholder with a rough byte indicator so
// the operator sees the backfill making progress rather than a static message
// (D2). Bytes are shown in KB/MB; zero bytes (backfill announced but nothing
// received yet) just shows the bare message.
func catchUpHint(bytes int) string {
	if bytes <= 0 {
		return "catching up…"
	}
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("catching up… %.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("catching up… %.0f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("catching up… %d B", bytes)
	}
}

// frameNow freezes the clock at the terminal time once the op is done, so the
// "elapsed" / phase timers stop ticking on a finished run rather than counting
// up forever while the operator reads the final screen.
func (m *tuiModel) frameNow() time.Time {
	if m.done && !m.endTime.IsZero() {
		return m.endTime
	}
	return m.now()
}
