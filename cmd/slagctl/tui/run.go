package tui

import (
	"context"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// This file is the program runner and its goroutine plumbing — the Tier-2/3
// layer that drives the pure reducer with live streams. It owns the
// tea.Program; the WS dialing and asciicast/envelope parsing stay in the cmd
// package (shared with `slagctl console` / `slagctl logs`) and are injected as
// stream-opener callbacks, so this package never imports the websocket client
// or the API token globals. Message production lives here because the model's
// message types are unexported.

// ConsoleSource opens the console cast stream and invokes onBytes for each
// decoded SOL chunk until ctx is cancelled. fromBackfill is true while
// replaying history; the source calls onLiveEdge once when it reaches the live
// edge (so the model can clear the catch-up placeholder). A returned error
// other than context cancellation is surfaced as a reconnect (the source is
// expected to retry internally for transient drops; returning signals a
// terminal console failure, which is non-fatal to the run).
type ConsoleSource func(ctx context.Context, onBytes func(data []byte, fromBackfill bool), onLiveEdge func()) error

// LogSource opens the logs stream from lastSeenID and invokes onEnvelope for
// each envelope until ctx is cancelled. It is expected to reconnect internally
// with the cursor (D4); the model tracks MaxID and the source resumes from it.
type LogSource func(ctx context.Context, onEnvelope func(id int64, env slogkit.Envelope)) error

// OpPoller fetches the current op row; used as the liveness backstop (D6's
// "slow op-row poll"). Returns (terminal, failed, reason) — terminal=false
// means still in flight. The runner polls it every pollInterval and fires an
// opTerminalMsg if it reports terminal before the stream did.
type OpPoller func(ctx context.Context) (terminal, failed bool, reason string, err error)

// RunConfig is everything the runner needs. The cmd package fills the stream
// openers + flags; the runner owns the model and program.
type RunConfig struct {
	OpType  model.OperationType
	Machine string
	// OpID is the watched op. The logs stream also carries other ops on the
	// same machine; only envelopes stamped with this id (or with no op id)
	// drive the lifecycle, the worker and the outcome (translateEnvelope).
	// The header shows its first 8 characters when Command is empty.
	OpID    uuid.UUID
	Command string // human-readable invocation (e.g. "machine deploy host --destructive")
	Worker  string // claimed_by hostname if known at startup (attach-after-claim backfill)
	Loc     *time.Location

	// OpStartedAt is the operation's server-side create time (op.CreatedAt). The
	// header "elapsed" anchors on it so it shares a time base with the phase
	// durations (server envelope timestamps) — without this the total is
	// measured from client attach time and a single phase can read longer than
	// the whole op. Zero means "unknown"; the model then falls back to attach
	// time.
	OpStartedAt time.Time

	// ResolveWorker maps a worker UUID (carried by a mid-stream claim envelope)
	// to its hostname. Nil leaves claim-envelope worker IDs unresolved (the TUI
	// shortens the UUID itself). The startup Worker field is already resolved by
	// the caller; this handles the claim-after-attach case.
	ResolveWorker func(string) string

	Console ConsoleSource
	Logs    LogSource
	Poll    OpPoller

	// Interactive is true when stdin is a TTY and we're not in a detected
	// non-interactive env; gates hold-on-failure. Hold/NoHold are the --hold /
	// --no-hold overrides.
	Interactive  bool
	Hold, NoHold bool
}

// consoleBatchInterval is the goroutine's coalescing window (D2: batch at
// ~30Hz, never touch the emulator). Bytes accumulated in this window ship as
// one consoleBatchMsg; the model drains them under its own time budget.
const consoleBatchInterval = 33 * time.Millisecond

// opPollInterval is the liveness backstop cadence (D6: "slow ~5s op-row poll
// purely as a liveness backstop" — the stream drives the UI, the poll only
// ever fires the teardown the stream should have).
const opPollInterval = 5 * time.Second

// RunOutcome is how a watch run ended, for the one-line terminal summary the
// caller prints after the alt-screen tears down. It lets the operator
// tell "completed" / "failed" / "timed out / interrupted before any terminal
// signal" apart without a separate `operation show`.
type RunOutcome int

const (
	// OutcomeCompleted: the op reached a success terminal while watching.
	OutcomeCompleted RunOutcome = iota
	// OutcomeFailed: the op reached a failure terminal while watching.
	OutcomeFailed
	// OutcomeIncomplete: the watch exited (ctx timeout, q, or interrupt) with
	// the op still in flight — no terminal signal was ever observed. This is the
	// "timed out waiting" case that previously exited 0 with no signal.
	OutcomeIncomplete
)

// RunResult summarizes how the watch ended. LastPhase is the last phase the
// lifecycle pane saw (current spinning phase, else the furthest reached),
// "awaiting agent report" when the op booted but the agent never reported, or
// "" if no phase was ever observed. Elapsed is the wall time the op spent from
// its start to the watch's end.
type RunResult struct {
	Outcome   RunOutcome
	LastPhase string
	Elapsed   time.Duration
}

// Run builds the model, starts the program, wires the stream goroutines, and
// returns how the run ended (for the caller's exit code AND its one-line exit
// summary) plus any program error. The model's View asks for the alt-screen
// only — no mouse capture, so native terminal text selection keeps working
// (see tuiModel.View). The hold policy is applied by translating success into an
// auto-Quit (the reducer already does this) and leaving failure on screen until
// q when decideHold says so.
func Run(ctx context.Context, cfg RunConfig) (result RunResult, err error) {
	m := newModelAt(cfg.OpType, cfg.Machine, cfg.OpID.String()[:8], cfg.OpStartedAt, cfg.Loc, time.Now)
	m.worker = cfg.Worker
	m.command = cfg.Command
	m.resolveWorker = cfg.ResolveWorker
	// The model assumes a console backfill is in flight (catchingUp=true) until a
	// consoleBackfillDoneMsg clears it. With no console source that message never
	// arrives, so clear it up front — otherwise the console box would show the
	// "catching up…" placeholder forever.
	if cfg.Console == nil {
		m.catchingUp = false
	}
	// Resolve the hold decision once: an interactive operator quits explicitly,
	// so the screen holds on any terminal state; a piped/CI run auto-exits.
	m.holdOnTerminal = decideHold(HoldPolicy{
		Interactive: cfg.Interactive,
		ForceHold:   cfg.Hold,
		ForceNoHold: cfg.NoHold,
	})

	// Derive a cancelable context the stream goroutines select on, and cancel it
	// when Run returns. tea.WithContext only lets bubbletea observe an EXTERNAL
	// cancel; it derives its own child internally and cancels THAT on Quit, never
	// the ctx we pass in (Go context cancellation is parent->child only). So
	// without this, a q/quit that ends the program would leave the console WS and
	// the forever-reconnecting logs loop running until the whole process exits.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Render to stderr (tea.WithOutput): the watch monitor is status output, and
	// the caller gates interactivity on stderr being a TTY (interactiveStderr in
	// cmd/tui_run.go), printing its one-line exit summary to stderr too. Bubble
	// Tea otherwise defaults its output to os.Stdout and derives the initial
	// terminal size, the SIGWINCH resize watcher (both key off p.ttyOutput) AND
	// the color profile it downsamples the frame to from that output. When stdout
	// is piped or redirected while stderr stays a TTY (a normal operator
	// pipeline), that default leaves ttyOutput nil: no WindowSizeMsg is ever sent
	// (width/height stay 0, so the view is stuck on the "please enlarge"
	// placeholder forever), resize is never observed, and the alt-screen escape
	// codes scribble into the redirect target instead of the terminal the
	// operator is looking at — which is exactly the reported min-size dead-end /
	// no resize, and the keys that appear dead because no coherent frame ever
	// repaints. Pinning output to the stderr TTY we already verified makes size
	// detection, SIGWINCH resize, color, key handling, and the heartbeat repaint
	// all land where they should. Input stays on os.Stdin (Bubble Tea falls back
	// to /dev/tty when stdin isn't a TTY), which is correct.
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(os.Stderr))

	// Stream goroutines. Each sends messages into the program; they exit when
	// ctx is cancelled (the defer above cancels it when Run returns).
	if cfg.Console != nil {
		go runConsoleGoroutine(ctx, p, cfg.Console)
	}
	if cfg.Logs != nil {
		go func() {
			_ = cfg.Logs(ctx, func(id int64, env slogkit.Envelope) {
				if msg := translateEnvelope(id, env, cfg.OpID); msg != nil {
					p.Send(msg)
				}
			})
		}()
	}
	if cfg.Poll != nil {
		go runOpPoll(ctx, p, cfg.Poll)
	}

	final, runErr := p.Run()
	if runErr != nil {
		return RunResult{}, runErr
	}
	fm := final.(*tuiModel)
	return fm.runResult(), nil
}

// runResult projects the final model into a RunResult for the caller's exit
// summary. Pure (no I/O), so it's golden-testable off a model built with
// scripted messages. The outcome is read from the terminal flags the reducer
// set; an exit with neither done flag means the watch was cut short (ctx
// timeout / q / interrupt) before any terminal signal — the OutcomeIncomplete
// "timed out waiting" case.
func (m *tuiModel) runResult() RunResult {
	r := RunResult{LastPhase: m.lastPhaseLabel()}
	switch {
	case m.done && m.failed:
		r.Outcome = OutcomeFailed
	case m.done:
		r.Outcome = OutcomeCompleted
	default:
		r.Outcome = OutcomeIncomplete
	}
	// Elapsed anchors on the op's start; freeze at the terminal time if we have
	// one (so a held screen the operator reads for a while doesn't inflate it),
	// else measure to now (the incomplete/timeout case).
	end := m.now()
	if m.done && !m.endTime.IsZero() {
		end = m.endTime
	}
	if d := end.Sub(m.startedAt).Round(time.Second); d > 0 {
		r.Elapsed = d
	}
	return r
}

// lastPhaseLabel is the phase name for the exit summary: the failed phase if the
// op failed, the explicit "awaiting agent report" wait if the op booted but the
// agent never reported, the current spinning phase, else the furthest phase
// reached (so a timed-out op still names where it got to), or "" if no phase was
// ever observed.
func (m *tuiModel) lastPhaseLabel() string {
	if p := m.lifecycle.FailedPhase(); p != "" {
		return p
	}
	if !m.done && m.lifecycle.AwaitingAgentReport() {
		return "awaiting agent report"
	}
	if p := m.lifecycle.CurrentPhase(); p != "" {
		return p
	}
	return m.lifecycle.HighWaterPhase()
}

// runConsoleGoroutine coalesces decoded console bytes into ~30Hz batches and
// ships them as consoleBatchMsg (D2: the goroutine batches and never touches
// the emulator). It flushes on a ticker so a steady byte trickle still renders
// promptly, and forwards the live-edge signal as consoleBackfillDoneMsg.
func runConsoleGoroutine(ctx context.Context, p *tea.Program, src ConsoleSource) {
	type chunk struct {
		data         []byte
		fromBackfill bool
	}
	ch := make(chan chunk, 256)
	liveEdge := make(chan struct{}, 1)

	signalLiveEdge := func() {
		select {
		case liveEdge <- struct{}{}:
		default:
		}
	}
	go func() {
		// Always signal live-edge when the source returns, even on a connect
		// error before it reached the real live edge. Without this, a console
		// stream that fails before signaling would leave catchingUp stuck, so the
		// box shows "catching up…" forever. The signal is idempotent (buffered
		// cap-1 + non-blocking send), so a normal live-edge followed by this defer
		// is harmless.
		defer signalLiveEdge()
		_ = src(ctx,
			func(data []byte, fromBackfill bool) {
				// Copy: the source may reuse its buffer after the callback.
				cp := make([]byte, len(data))
				copy(cp, data)
				select {
				case ch <- chunk{cp, fromBackfill}:
				case <-ctx.Done():
				}
			},
			signalLiveEdge,
		)
	}()

	ticker := time.NewTicker(consoleBatchInterval)
	defer ticker.Stop()
	var pending []byte
	pendingBackfill := false
	flush := func() {
		if len(pending) == 0 {
			return
		}
		p.Send(consoleBatchMsg{data: pending, fromBackfill: pendingBackfill})
		pending = nil
		pendingBackfill = false
	}
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-ch:
			pending = append(pending, c.data...)
			if c.fromBackfill {
				pendingBackfill = true
			}
		case <-ticker.C:
			flush()
		case <-liveEdge:
			// Drain any backfill chunks still buffered in ch before signaling
			// done: select picks a ready case at random, so with a fast backfill
			// liveEdge can win while backfill bytes are still queued. Flushing and
			// sending done without draining would clear catchingUp early and let
			// the remaining chunks render over later frames — flashing through the
			// intermediate replay states D2 exists to hide.
			for len(ch) > 0 {
				c := <-ch
				pending = append(pending, c.data...)
				if c.fromBackfill {
					pendingBackfill = true
				}
			}
			flush() // ship all backfill bytes before signaling done
			p.Send(consoleBackfillDoneMsg{})
		}
	}
}

// runOpPoll is the liveness backstop: every opPollInterval it asks the poller
// whether the op is terminal, and fires opTerminalMsg if so. The stream
// normally beats it; this only catches the pathological case where a terminal
// envelope is stranded behind the live-tail watermark (D6).
func runOpPoll(ctx context.Context, p *tea.Program, poll OpPoller) {
	t := time.NewTicker(opPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			terminal, failed, reason, err := poll(ctx)
			if err != nil || !terminal {
				continue
			}
			p.Send(opTerminalMsg{failed: failed, reason: reason, at: time.Now()})
			return // op is terminal; the backstop's job is done
		}
	}
}
