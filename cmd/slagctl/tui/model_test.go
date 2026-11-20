package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// send pushes one message through Update and returns the updated model, the
// pattern the plan's Tier-1 strategy prescribes (scripted Update() calls, no
// goroutine, no wall clock). The returned tea.Cmd is intentionally dropped —
// in the pure tests we feed any follow-up message ourselves.
func send(m *tuiModel, msg tea.Msg) *tuiModel {
	next, _ := m.Update(msg)
	return next.(*tuiModel)
}

// newTestModel builds a model with a fixed clock and a sized window, ready to
// render the 3-pane layout.
func newTestModel(now func() time.Time) *tuiModel {
	m := newModel(model.OpDeploy, "kworker-l34-1.pop1", "8b2e3f1a", time.UTC, now)
	m = send(m, tea.WindowSizeMsg{Width: minTermCols, Height: minRows})
	return m
}

// viewText is the visible text of the model's rendered View.
func viewText(m *tuiModel) string {
	return plain(m.View().Content)
}

// TestModel_MidDeployFrame drives the model with the same scripted phase /
// log / console inputs as the frame golden and asserts the rendered View
// matches the mockup's mid-deploy state. This is the integration golden: the
// reducer + panes + renderFrame composed end to end, deterministically.
func TestModel_MidDeployFrame(t *testing.T) {
	now := fixedClock(at(77 * time.Second))
	m := newModel(model.OpDeploy, "kworker-l34-1.pop1", "8b2e3f1a", time.UTC, now)
	// startedAt is now() at construction; pin it to t0 for the elapsed math.
	m.startedAt = at(0)
	m.command = "machine deploy kworker-l34-1.pop1 --destructive"
	// A claim envelope carries a worker UUID; the resolver maps it to a hostname.
	m.resolveWorker = func(id string) string {
		if id == "11111111-2222-3333-4444-555555555555" {
			return "pop1-sw-2"
		}
		return id
	}
	m = send(m, tea.WindowSizeMsg{Width: minTermCols, Height: minRows})
	m = send(m, claimedMsg{worker: "11111111-2222-3333-4444-555555555555"})

	// Backfill done with no pending bytes -> not catching up.
	m = send(m, consoleBackfillDoneMsg{})

	for _, p := range []struct {
		name string
		d    time.Duration
	}{
		{"power_cycle", 0}, {"pxe_booting", 2 * time.Second},
		{"dhcp_ack", 16 * time.Second}, {"ipxe_dhcp", 17 * time.Second},
		{"ipxe_loaded", 19 * time.Second}, {"agent_starting", 27 * time.Second},
		{"partitioning", 30 * time.Second},
	} {
		m = send(m, phaseMsg{phase: p.name, at: at(p.d)})
	}

	m = send(m, consoleBatchMsg{data: []byte(
		"slagworks-agent: found 4 disk(s), using nvme0n1\r\n" +
			"slagworks-agent: storage layout planned (12 commands)\r\n" +
			"Creating new GPT entries in memory.\r\n")})

	view := viewText(m)
	if !strings.Contains(view, spinnerFrames[0]+" partitioning") {
		t.Errorf("current phase missing from View:\n%s", view)
	}
	if !strings.Contains(view, "(47s)") {
		t.Errorf("partitioning elapsed not 47s in View:\n%s", view)
	}
	// Worker is shown once, in the summary box.
	if !strings.Contains(view, "pop1-sw-2") {
		t.Errorf("worker missing from summary:\n%s", view)
	}
	if !strings.Contains(view, "found 4 disk(s)") {
		t.Errorf("console output missing from View:\n%s", view)
	}
	if strings.Contains(view, "catching up") {
		t.Errorf("should not show catch-up after backfill done + drained:\n%s", view)
	}
	goldenScreen(t, "model_mid_deploy", view)
}

// TestModel_CatchUpPlaceholder proves the D2 catch-up behavior: while a
// backfill is draining (backfillDone not yet seen, or bytes still pending),
// the console box shows "catching up…", not the partial emulator screen.
func TestModel_CatchUpPlaceholder(t *testing.T) {
	m := newTestModel(fixedClock(at(time.Second)))
	// Fresh model assumes a backfill until told otherwise: catchingUp=true.
	m = send(m, consoleBatchMsg{data: []byte("partial boot output"), fromBackfill: true})
	view := viewText(m)
	if !strings.Contains(view, "catching up") {
		t.Errorf("expected catch-up placeholder during backfill:\n%s", view)
	}
	// 19 bytes of backfill -> byte indicator (progress hint, D2).
	if !strings.Contains(view, "19 B") {
		t.Errorf("expected backfill byte indicator in placeholder:\n%s", view)
	}
	if strings.Contains(view, "partial boot output") {
		t.Errorf("must not flash partial emulator screen during backfill:\n%s", view)
	}

	// Live edge reached and buffer drained -> placeholder clears.
	m = send(m, consoleBackfillDoneMsg{})
	view = viewText(m)
	if strings.Contains(view, "catching up") {
		t.Errorf("placeholder should clear once backfill done + drained:\n%s", view)
	}
	if !strings.Contains(view, "partial boot output") {
		t.Errorf("emulator screen should show after catch-up:\n%s", view)
	}
}

// TestModel_CommissionAwaitingReport is the integration test: a
// commission op that boots to ipxe_loaded but whose agent never reports must
// render an explicit "awaiting agent report" line in the summary with a live
// elapsed clock — not a frozen all-pending spine.
func TestModel_CommissionAwaitingReport(t *testing.T) {
	now := fixedClock(at(90 * time.Second))
	m := newModel(model.OpCommission, "hot-spare-1.pop2", "f5cb281e", time.UTC, now)
	m.startedAt = at(0)
	m = send(m, tea.WindowSizeMsg{Width: minTermCols, Height: minRows})
	m = send(m, consoleBackfillDoneMsg{})

	for _, p := range []struct {
		name string
		d    time.Duration
	}{
		{"power_cycle", 0}, {"pxe_booting", 2 * time.Second},
		{"dhcp_ack", 16 * time.Second}, {"ipxe_dhcp", 17 * time.Second},
		{"ipxe_loaded", 19 * time.Second},
	} {
		m = send(m, phaseMsg{phase: p.name, at: at(p.d)})
	}

	view := viewText(m)
	if !strings.Contains(view, "awaiting agent report") {
		t.Errorf("summary should surface awaiting-agent-report state:\n%s", view)
	}
	// Boot prefix animated: power_cycle done, ipxe_loaded spinning.
	if !strings.Contains(view, "✓ power_cycle") {
		t.Errorf("boot prefix should be animated (power_cycle done):\n%s", view)
	}
	if !strings.Contains(view, spinnerFrames[0]+" ipxe_loaded") {
		t.Errorf("ipxe_loaded should be the spinning phase:\n%s", view)
	}
	// since clock is live: ipxe_loaded arrived at 19s, now is 90s -> 1m11s.
	if !strings.Contains(view, "1m11s") {
		t.Errorf("expected live elapsed (1m11s) in summary since:\n%s", view)
	}
}

// TestModel_RunResult covers the exit-summary projection for the three
// outcomes: completed, failed, and incomplete (watch ended with the op still in
// flight — the timed-out-waiting case).
func TestModel_RunResult(t *testing.T) {
	build := func() *tuiModel {
		now := fixedClock(at(5 * time.Minute))
		m := newModel(model.OpCommission, "hot-spare-1.pop2", "f5cb281e", time.UTC, now)
		m.startedAt = at(0)
		for _, p := range []string{"power_cycle", "pxe_booting", "dhcp_ack", "ipxe_dhcp", "ipxe_loaded"} {
			m = send(m, phaseMsg{phase: p, at: at(0)})
		}
		return m
	}

	t.Run("incomplete", func(t *testing.T) {
		m := build()
		r := m.runResult()
		if r.Outcome != OutcomeIncomplete {
			t.Errorf("Outcome = %v, want OutcomeIncomplete", r.Outcome)
		}
		if r.LastPhase != "awaiting agent report" {
			t.Errorf("LastPhase = %q, want awaiting agent report", r.LastPhase)
		}
		if r.Elapsed != 5*time.Minute {
			t.Errorf("Elapsed = %v, want 5m", r.Elapsed)
		}
	})

	t.Run("completed", func(t *testing.T) {
		m := build()
		m = send(m, opTerminalMsg{failed: false, at: at(5 * time.Minute)})
		r := m.runResult()
		if r.Outcome != OutcomeCompleted {
			t.Errorf("Outcome = %v, want OutcomeCompleted", r.Outcome)
		}
	})

	t.Run("failed", func(t *testing.T) {
		m := build()
		m = send(m, opTerminalMsg{failed: true, reason: "no bootif match", at: at(4 * time.Minute)})
		r := m.runResult()
		if r.Outcome != OutcomeFailed {
			t.Errorf("Outcome = %v, want OutcomeFailed", r.Outcome)
		}
		if r.LastPhase != "ipxe_loaded" {
			t.Errorf("LastPhase = %q, want ipxe_loaded (failed phase)", r.LastPhase)
		}
	})
}

// TestModel_DrainBudget proves the per-tick time budget bounds work and yields
// (D2): with a clock that advances past the budget on the first check, a large
// pending buffer drains only one chunk per tick and asks for a follow-up.
func TestModel_DrainBudget(t *testing.T) {
	// Clock that jumps 1s every call — so the very first post-write check is
	// already past the 8ms budget, forcing a one-chunk-per-tick drain.
	calls := 0
	clock := func() time.Time {
		calls++
		return at(time.Duration(calls) * time.Second)
	}
	m := newTestModel(clock)
	big := make([]byte, 4096*5) // 5 chunks
	for i := range big {
		big[i] = 'x'
	}
	_, cmd := m.Update(consoleBatchMsg{data: big})
	// Budget exceeded after the first chunk -> remainder pending + follow-up.
	if len(m.pending) != 4096*4 {
		t.Errorf("expected 4 chunks left pending, got %d bytes", len(m.pending))
	}
	if cmd == nil {
		t.Error("expected a follow-up drainTick command when bytes remain")
	}
	// Feed drain ticks until empty; must terminate (not spin forever).
	for i := 0; i < 10 && len(m.pending) > 0; i++ {
		m.Update(drainTickMsg{})
	}
	if len(m.pending) != 0 {
		t.Errorf("buffer did not drain across ticks: %d left", len(m.pending))
	}
}

// TestModel_TerminalSuccessNonInteractiveExits: with holdOnTerminal=false
// (the non-interactive / piped / CI case), a success terminal auto-exits so a
// script invoking -w doesn't hang on a keypress that won't come.
func TestModel_TerminalSuccessNonInteractiveExits(t *testing.T) {
	m := newTestModel(fixedClock(at(6 * time.Minute)))
	m.startedAt = at(0)
	m.holdOnTerminal = false
	next, cmd := m.Update(opTerminalMsg{failed: false, at: at(6 * time.Minute)})
	m = next.(*tuiModel)
	if !m.done || m.failed {
		t.Errorf("success terminal: done=%v failed=%v", m.done, m.failed)
	}
	if cmd == nil {
		t.Error("non-interactive success should auto-exit (Quit cmd)")
	}
	if !strings.Contains(viewText(m), "completed in") {
		t.Errorf("success header missing:\n%s", viewText(m))
	}
}

// TestModel_TerminalSuccessInteractiveHolds: with holdOnTerminal=true (an
// interactive operator), a SUCCESS terminal does NOT auto-exit — the operator
// quits explicitly so the final lifecycle/summary stays on screen.
func TestModel_TerminalSuccessInteractiveHolds(t *testing.T) {
	m := newTestModel(fixedClock(at(6 * time.Minute)))
	m.holdOnTerminal = true
	_, cmd := m.Update(opTerminalMsg{failed: false, at: at(6 * time.Minute)})
	if cmd != nil {
		t.Error("interactive success should hold (no auto-Quit) until the operator presses q")
	}
}

// TestModel_TerminalFailureHolds: when holdOnTerminal is set (interactive run),
// failure does NOT return Quit (the screen holds for reading), marks the
// lifecycle phase failed, and renders the error pane.
func TestModel_TerminalFailureHolds(t *testing.T) {
	m := newTestModel(fixedClock(at(42 * time.Second)))
	m.startedAt = at(0)
	m.holdOnTerminal = true // interactive run: hold on failure
	m = send(m, phaseMsg{phase: "power_cycle", at: at(0)})
	m = send(m, phaseMsg{phase: "partitioning", at: at(30 * time.Second)})
	next, cmd := m.Update(opTerminalMsg{failed: true, reason: "sgdisk exit 2", at: at(42 * time.Second)})
	m = next.(*tuiModel)
	if !m.done || !m.failed {
		t.Errorf("failure terminal: done=%v failed=%v", m.done, m.failed)
	}
	if cmd != nil {
		t.Error("holdOnTerminal failure should hold (no auto-Quit)")
	}
	view := viewText(m)
	if !strings.Contains(view, "FAILED at partitioning") {
		t.Errorf("failure header missing failing phase:\n%s", view)
	}
	if !strings.Contains(view, "─ error ─") {
		t.Errorf("error pane not shown:\n%s", view)
	}
	if !strings.Contains(view, "sgdisk exit 2") {
		t.Errorf("failure reason not pinned:\n%s", view)
	}
}

// TestModel_TerminalFailureNoHoldQuits: without holdOnTerminal (non-interactive
// / --no-hold), a failure tears down immediately (returns Quit) so a PTY
// wrapper with no human doesn't block forever. The error still rendered live.
func TestModel_TerminalFailureNoHoldQuits(t *testing.T) {
	m := newTestModel(fixedClock(at(42 * time.Second)))
	m.holdOnTerminal = false
	_, cmd := m.Update(opTerminalMsg{failed: true, reason: "x", at: at(42 * time.Second)})
	if cmd == nil {
		t.Error("non-holding failure should auto-Quit (no human to press q)")
	}
}

// TestModel_UITickAnimatesAndReschedules: the heartbeat advances the spinner
// and returns a follow-up tick while running — this is what keeps the View
// repainting (and elapsed timers live) on an idle host, where no console/log
// message arrives to drive a re-render.
func TestModel_UITickAnimatesAndReschedules(t *testing.T) {
	m := newTestModel(fixedClock(at(time.Second)))
	before := m.spinnerF
	_, cmd := m.Update(uiTickMsg{})
	if m.spinnerF == before {
		t.Errorf("ui tick did not advance the spinner glyph (still %q)", before)
	}
	if cmd == nil {
		t.Error("ui tick should reschedule itself while running")
	}
}

// TestModel_UITickStopsWhenDone: once terminal, the heartbeat stops
// rescheduling (frozen clock, nothing to animate, don't wake the loop while
// the operator reads the final screen).
func TestModel_UITickStopsWhenDone(t *testing.T) {
	m := newTestModel(fixedClock(at(time.Minute)))
	m = send(m, opTerminalMsg{failed: false, at: at(time.Minute)})
	_, cmd := m.Update(uiTickMsg{})
	if cmd != nil {
		t.Error("ui tick should NOT reschedule after the op is done")
	}
}

// TestModel_ElapsedAdvancesOnHeartbeat is the guard: with ZERO log /
// console / phase activity, the displayed elapsed/total must tick up purely from
// the heartbeat advancing the wall clock — this is the "frozen at 2m44s on a
// stalled host" bug. elapsed() reads now() each render, so as long as the
// heartbeat keeps re-rendering against a live clock, the value advances. We
// model the live clock by advancing it between heartbeats and asserting the
// rendered total moves.
func TestModel_ElapsedAdvancesOnHeartbeat(t *testing.T) {
	clockT := at(0)
	m := newTestModel(func() time.Time { return clockT })
	m.startedAt = at(0)

	clockT = at(2*time.Minute + 44*time.Second)
	if !strings.Contains(viewText(m), "elapsed 2m44s") {
		t.Fatalf("setup: expected elapsed 2m44s:\n%s", viewText(m))
	}
	// Heartbeat fires with the clock advanced 10s and NO other message; the
	// heartbeat must reschedule (keeping the loop alive) and the rendered
	// elapsed must advance to 2m54s.
	clockT = at(2*time.Minute + 54*time.Second)
	_, cmd := m.Update(uiTickMsg{})
	if cmd == nil {
		t.Error("heartbeat must reschedule while running, or elapsed freezes")
	}
	if !strings.Contains(viewText(m), "elapsed 2m54s") {
		t.Errorf("elapsed should advance on the heartbeat with no other input:\n%s", viewText(m))
	}
}

// TestModel_LastUpdateResetsOnEvent is the reset guard: any inbound
// activity (here a log envelope) stamps lastEventAt at the current clock, so the
// summary's "last update <N>s ago" resets to ~0 on activity. The heartbeat does
// NOT stamp it — so between events the age grows.
func TestModel_LastUpdateResetsOnEvent(t *testing.T) {
	clockT := at(0)
	m := newTestModel(func() time.Time { return clockT })
	m.startedAt = at(0)

	// An event at t0 stamps lastEventAt.
	clockT = at(10 * time.Second)
	m = send(m, phaseMsg{phase: "power_cycle", at: at(10 * time.Second)})

	// 8s later, no further event: the heartbeat fires (advancing the spinner)
	// but must NOT reset the staleness clock. Age should read 8s.
	clockT = at(18 * time.Second)
	m = send(m, uiTickMsg{})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 8*time.Second {
		t.Errorf("after a heartbeat with no event, age should be 8s (ok=%v got=%s)", ok, ago)
	}

	// A new event resets it to 0.
	clockT = at(25 * time.Second)
	m = send(m, phaseMsg{phase: "pxe_booting", at: at(25 * time.Second)})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 0 {
		t.Errorf("after a fresh event, age should reset to 0 (ok=%v got=%s)", ok, ago)
	}
}

// envAt builds a phase-carrying log envelope stamped at ts, for the staleness
// tests that need the envelope's real timestamp to drive lastEventAt.
func envAt(phase string, ts time.Time) slogkit.Envelope {
	return slogkit.Envelope{TS: ts, Source: slogkit.SourceSite, Phase: phase, Msg: phase}
}

// TestModel_LastUpdateUsesEnvelopeTime is a regression guard:
// on attach to a running op the backfilled log envelopes carry their REAL (old)
// server timestamps. The staleness clock must reflect the true age of the last
// activity, not reset to ~0 just because the envelope was received at attach
// time. A log envelope whose TS is 90s before now() must read "90s ago", and a
// later-arriving but OLDER envelope must not drag the clock backward.
func TestModel_LastUpdateUsesEnvelopeTime(t *testing.T) {
	clockT := at(100 * time.Second) // "now" = attach time, 100s in
	m := newTestModel(func() time.Time { return clockT })
	m.startedAt = at(0)

	// Backfilled envelope: its activity actually happened at t=10s (90s ago),
	// even though we receive it now (t=100s). Age must be 90s, not 0.
	m = send(m, logEnvelopeMsg{id: 1, env: envAt("dhcp_ack", at(10*time.Second))})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 90*time.Second {
		t.Errorf("backfilled envelope: age should be 90s from its TS (ok=%v got=%s)", ok, ago)
	}

	// A second backfilled envelope that is OLDER (t=5s) must not move the clock
	// backward — we keep the latest activity time seen.
	m = send(m, logEnvelopeMsg{id: 2, env: envAt("power_cycle", at(5*time.Second))})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 90*time.Second {
		t.Errorf("older envelope must not drag the clock backward (ok=%v got=%s)", ok, ago)
	}

	// A newer envelope (t=95s) advances it: age 5s.
	m = send(m, logEnvelopeMsg{id: 3, env: envAt("ipxe_loaded", at(95*time.Second))})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 5*time.Second {
		t.Errorf("newer envelope should advance the clock to 5s ago (ok=%v got=%s)", ok, ago)
	}
}

// TestModel_BackfillConsoleBytesDoNotResetStaleness: the server-rendered screen
// snapshot arrives as fromBackfill console bytes. Those represent history of
// unknown age and must NOT reset the staleness clock (else "last update" would
// snap to 0 the instant the snapshot lands on attach — the bug-1 console arm).
func TestModel_BackfillConsoleBytesDoNotResetStaleness(t *testing.T) {
	clockT := at(100 * time.Second)
	m := newTestModel(func() time.Time { return clockT })
	m.startedAt = at(0)

	// Establish a real last-activity time from a log envelope 40s ago.
	m = send(m, logEnvelopeMsg{id: 1, env: envAt("dhcp_ack", at(60*time.Second))})
	if ago, _ := m.frameInput().lastUpdateAgo(); ago != 40*time.Second {
		t.Fatalf("setup: expected 40s, got %s", ago)
	}
	// The snapshot (backfill bytes) lands. Staleness must stay 40s, not reset.
	m = send(m, consoleBatchMsg{data: []byte("\x1b[2J\x1b[Hsome screen"), fromBackfill: true})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 40*time.Second {
		t.Errorf("backfill console bytes must not reset staleness (ok=%v got=%s)", ok, ago)
	}
	// Backfill-done (live-edge reached) is a client-side signal, NOT machine
	// activity — it must not reset staleness either (roast HIGH finding).
	m = send(m, consoleBackfillDoneMsg{})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 40*time.Second {
		t.Errorf("backfill-done must not reset staleness (ok=%v got=%s)", ok, ago)
	}
	// A LIVE console byte does count as activity now -> resets to 0.
	m = send(m, consoleBatchMsg{data: []byte("x"), fromBackfill: false})
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 0 {
		t.Errorf("live console bytes should reset staleness to 0 (ok=%v got=%s)", ok, ago)
	}
}

// TestModel_LastUpdateUnsetBeforeAnyEvent: before any inbound activity the
// staleness clock has no anchor, so it reports not-ok (rendered "—"), never a
// misleading "0s ago".
func TestModel_LastUpdateUnsetBeforeAnyEvent(t *testing.T) {
	m := newTestModel(fixedClock(at(time.Minute)))
	if _, ok := m.frameInput().lastUpdateAgo(); ok {
		t.Error("last-update should be unset before any inbound event")
	}
	// A heartbeat alone must not anchor it (the heartbeat isn't inbound activity).
	m = send(m, uiTickMsg{})
	if _, ok := m.frameInput().lastUpdateAgo(); ok {
		t.Error("the heartbeat must not anchor the staleness clock")
	}
}

// TestModel_LastUpdateFrozenAfterTerminal guards the held-screen terminal case
// (roast findings): once the op is terminal the display clock freezes at endTime
// and the staleness must freeze with it. Two hazards:
//
//  1. A trailing late event must NOT restamp lastEventAt past endTime (the
//     markEvent done-guard), or the frozen age clamps to a bogus "0s ago".
//  2. The terminal stamp must anchor on msg.at (the SERVER terminal time, the
//     same base as endTime), not the client wall clock — otherwise the frozen
//     endTime - lastEventAt is a cross-clock subtraction that, under skew,
//     renders a wildly inflated age. The client clock here is deliberately far
//     ahead of the server timestamps to expose any cross-clock mixing.
func TestModel_LastUpdateFrozenAfterTerminal(t *testing.T) {
	// Client wall clock runs 1h ahead of the server envelope timestamps (skew).
	const skew = time.Hour
	clientNow := at(skew)
	m := newTestModel(func() time.Time { return clientNow })
	m.startedAt = at(0) // server op-create
	m.holdOnTerminal = true

	// Op completes at SERVER time t0+60s; client clock is at t0+1h+60s.
	clientNow = at(skew + 60*time.Second)
	m = send(m, opTerminalMsg{failed: false, at: at(60 * time.Second)})

	// Frozen display: now()=endTime=server 60s, lastEventAt=server 60s -> 0s.
	// If lastEventAt had been the client clock, this would read ~1h.
	ago, ok := m.frameInput().lastUpdateAgo()
	if !ok {
		t.Fatal("expected a last-update age after the terminal signal")
	}
	if ago != 0 {
		t.Errorf("terminal staleness should be 0s (server-clock anchored), got %s — cross-clock mixing?", ago)
	}

	// A trailing late envelope (client clock advanced another 30s) must not
	// restamp lastEventAt; the frozen age stays 0s, never negative/bogus.
	clientNow = at(skew + 90*time.Second)
	m = send(m, logEnvelopeMsg{id: 99, env: mkEnv("agent", at(90*time.Second), "", "late line", nil)})
	ago, ok = m.frameInput().lastUpdateAgo()
	if !ok || ago != 0 {
		t.Errorf("post-terminal late event must not disturb the frozen staleness (ok=%v got=%s)", ok, ago)
	}
}

// TestModel_LatePhaseAfterFailureLeavesSpineFrozen: on a held screen the log
// stream keeps delivering after the terminal signal, and a phase envelope for
// the same op can arrive after its failure (an op-timeout or abort fails the op
// while the agent is still working; the op-row poll can beat the stream). The
// failed spine is final: the late phase must not start spinning under the
// FAILED header or skip-mark the runway between it and the failed phase. The
// envelope's log line still lands in the log pane.
func TestModel_LatePhaseAfterFailureLeavesSpineFrozen(t *testing.T) {
	m := newModelAt(model.OpDeploy, "host", "op", at(0), time.UTC, fixedClock(at(90*time.Second)))
	m.holdOnTerminal = true
	m = send(m, translateEnvelope(1, mkEnv("agent", at(10*time.Second), "partitioning", "partitioning", nil), watchedOp))
	m = send(m, translateEnvelope(2, mkEnv("global", at(30*time.Second), "", "operation failed",
		map[string]any{"status": "failed", "error": "op timeout"}), watchedOp))
	before := plain(m.lifecycle.Render(40, "@"))

	m = send(m, translateEnvelope(3, mkEnv("agent", at(40*time.Second), "rsyncing_os", "rsync started", nil), watchedOp))

	if got := m.lifecycle.CurrentPhase(); got != "" {
		t.Fatalf("failed op shows phase %q spinning after a late envelope", got)
	}
	if after := plain(m.lifecycle.Render(40, "@")); after != before {
		t.Fatalf("final lifecycle changed after terminal:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := m.lifecycle.FailedPhase(); got != "partitioning" {
		t.Errorf("FailedPhase() = %q, want partitioning", got)
	}
	if lines := m.logs.Lines(); len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "rsync started") {
		t.Errorf("late envelope's log line should still append to the log pane: %q", lines)
	}
}

// TestModel_SecondTerminalDoesNotRewriteFinalScreen: a held screen gets two
// terminal signals for one op, the stream's source=global envelope and the
// op-row poll backstop (runOpPoll keeps polling until it sees the op terminal,
// whichever arrives first). The first one ends the run; the second must not
// move endTime (the frozen total), replace the reason, or re-run Fail on a
// late phase and mark a second row failed.
func TestModel_SecondTerminalDoesNotRewriteFinalScreen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
	}{
		{"failed", true},
		{"completed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModelAt(model.OpDeploy, "host", "op", at(0), time.UTC, fixedClock(at(90*time.Second)))
			m.holdOnTerminal = true
			m = send(m, translateEnvelope(1, mkEnv("agent", at(10*time.Second), "partitioning", "partitioning", nil), watchedOp))
			status, reason, pollReason := "completed", "", ""
			if tc.failed {
				status, reason, pollReason = "failed", "op timeout", "op timeout (from the op row)"
			}
			m = send(m, translateEnvelope(2, mkEnv("global", at(30*time.Second), "", "operation "+status,
				map[string]any{"status": status, "error": reason}), watchedOp))
			wantResult := m.runResult()
			wantSpine := plain(m.lifecycle.Render(40, "@"))

			m = send(m, translateEnvelope(3, mkEnv("agent", at(40*time.Second), "rsyncing_os", "rsync started", nil), watchedOp))
			m = send(m, opTerminalMsg{failed: tc.failed, reason: pollReason, at: at(50 * time.Second)})

			if !m.endTime.Equal(at(30 * time.Second)) {
				t.Errorf("endTime = %s, want the first terminal's %s", m.endTime, at(30*time.Second))
			}
			if m.failReason != reason {
				t.Errorf("failReason = %q, want the first terminal's %q", m.failReason, reason)
			}
			if got := m.runResult(); got != wantResult {
				t.Errorf("runResult() = %+v, want %+v", got, wantResult)
			}
			if got := plain(m.lifecycle.Render(40, "@")); got != wantSpine {
				t.Errorf("final lifecycle changed after a second terminal:\nbefore:\n%s\nafter:\n%s", wantSpine, got)
			}
		})
	}
}

// TestModel_FrozenClockOnDone: after completion the elapsed timer stops at
// endTime rather than counting up while the operator reads the screen.
func TestModel_FrozenClockOnDone(t *testing.T) {
	clockT := at(0)
	m := newTestModel(func() time.Time { return clockT })
	m.startedAt = at(0)
	m = send(m, opTerminalMsg{failed: false, at: at(2 * time.Minute)})
	// Advance the wall clock well past endTime; the frame must still read 2m0s.
	clockT = at(10 * time.Minute)
	if !strings.Contains(viewText(m), "2m0s") {
		t.Errorf("elapsed should freeze at endTime (2m0s):\n%s", viewText(m))
	}
}

// TestModel_ElapsedAnchorsOnOpStart reproduces the two-epoch elapsed bug: the
// header "elapsed" total was measured from the client's attach time (now() at
// model construction), while phase durations come from server-side envelope
// timestamps that began when the op was actually created — earlier than attach.
// When an operator attaches to an in-flight op, a single phase's duration could
// exceed the header total, which is impossible.
//
// Concretely (the screenshot the bug was filed from): op created at t0, the
// operator attaches 3m15s later, pxe_booting ran 3m49s (server time). The
// header must read >= 3m49s, not 3m15s. Both numbers must derive from the same
// authoritative base (the server op-create time), so total elapsed is always
// >= any single phase duration.
func TestModel_ElapsedAnchorsOnOpStart(t *testing.T) {
	// op created at t0 (server time). The "now" clock reports a moment 3m51s
	// into the op — i.e. we attached late and time has advanced on the server
	// clock domain that op-create and the envelopes share.
	opStart := at(0)
	nowAt := at(3*time.Minute + 51*time.Second)
	m := newModelAt(model.OpDeploy, "kworker-l34-1.pop1", "2a425d55", opStart, time.UTC, fixedClock(nowAt))
	m = send(m, tea.WindowSizeMsg{Width: minTermCols, Height: minRows})
	m = send(m, consoleBackfillDoneMsg{})

	// power_cycle instant; pxe_booting runs from t+2s to t+3m51s = 3m49s; we are
	// now in dhcp_ack (current). These arrival times are server envelope TS.
	m = send(m, phaseMsg{phase: "power_cycle", at: at(0)})
	m = send(m, phaseMsg{phase: "pxe_booting", at: at(2 * time.Second)})
	m = send(m, phaseMsg{phase: "dhcp_ack", at: at(3*time.Minute + 51*time.Second)})

	view := viewText(m)

	// The finalized pxe_booting phase shows 3m49s.
	if !strings.Contains(view, "3m49s") {
		t.Errorf("expected pxe_booting duration 3m49s in view:\n%s", view)
	}
	// The header total elapsed must be >= the largest phase duration. Anchored
	// on op-create (t0) and read at nowAt, that is 3m51s — NOT the old
	// attach-relative 0s.
	if !strings.Contains(view, "elapsed 3m51s") {
		t.Errorf("header elapsed should anchor on op start (3m51s), not client attach:\n%s", view)
	}

	// Invariant, stated directly against the model: total elapsed >= every
	// finalized phase duration and >= the current phase's running duration.
	total := m.frameInput().elapsed()
	for _, r := range m.lifecycle.rows {
		if r.duration > total {
			t.Errorf("phase %q duration %s exceeds total elapsed %s (impossible)",
				r.name, r.duration, total)
		}
	}
	if since := m.lifecycle.CurrentPhaseSince(); since > total {
		t.Errorf("current phase running %s exceeds total elapsed %s (impossible)", since, total)
	}
}

// isQuit reports whether a tea.Cmd is the Quit command (it returns a
// tea.QuitMsg when invoked). A tea.Cmd is an opaque func; the only way to
// classify it is to call it. The quit command is pure (no I/O, no blocking),
// so invoking it in a test is safe.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// TestModel_Quit: q and ctrl+c both request Quit. Asserts the
// returned command is actually tea.Quit, not merely non-nil.
func TestModel_Quit(t *testing.T) {
	for _, key := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
	} {
		m := newTestModel(fixedClock(at(time.Second)))
		_, cmd := m.Update(key.msg)
		if !isQuit(cmd) {
			t.Errorf("key %q should request Quit", key.name)
		}
	}
}

// TestModel_QuitWorksAtSmallSize is the entanglement guard: even
// when the terminal is below the minimum and the View is the "please enlarge"
// placeholder, ctrl+c and q must still Quit. The reducer must never bypass key
// handling based on a placeholder/gate render state.
func TestModel_QuitWorksAtSmallSize(t *testing.T) {
	for _, key := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
	} {
		m := newModel(model.OpDeploy, "kworker-l34-1.pop1", "8b2e3f1a", time.UTC, fixedClock(at(time.Second)))
		// Below the minimum: the View is the enlarge placeholder.
		m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
		if !strings.Contains(viewText(m), "enlarge") {
			t.Fatalf("setup: expected the small-terminal placeholder:\n%s", viewText(m))
		}
		_, cmd := m.Update(key.msg)
		if !isQuit(cmd) {
			t.Errorf("key %q must Quit even from the small-terminal placeholder", key.name)
		}
	}
}

// TestModel_ResizeRecoversFromSmall is a regression test: a model that
// starts below the minimum (showing the enlarge placeholder) must flip to the
// full 3-pane layout once a WindowSizeMsg grows the terminal past the gate —
// the placeholder is not a permanent dead-end.
func TestModel_ResizeRecoversFromSmall(t *testing.T) {
	m := newModel(model.OpDeploy, "kworker-l34-1.pop1", "8b2e3f1a", time.UTC, fixedClock(at(time.Second)))
	m = send(m, consoleBackfillDoneMsg{})

	// Too small: enlarge placeholder, no 3-pane layout.
	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(viewText(m), "enlarge") {
		t.Fatalf("below the minimum should show the enlarge placeholder:\n%s", viewText(m))
	}
	if strings.Contains(viewText(m), "serial console") {
		t.Fatalf("below the minimum should not draw the 3-pane layout:\n%s", viewText(m))
	}

	// Resize past the gate: the layout must appear, placeholder gone.
	m = send(m, tea.WindowSizeMsg{Width: minTermCols, Height: minRows})
	if strings.Contains(viewText(m), "enlarge") {
		t.Errorf("after a resize past the minimum the enlarge placeholder must be gone:\n%s", viewText(m))
	}
	if !strings.Contains(viewText(m), "serial console") {
		t.Errorf("after a resize past the minimum the 3-pane layout must render:\n%s", viewText(m))
	}
}

// TestModel_TerminalStateIsUnambiguous is the disambiguation: a
// terminal op intentionally freezes the clock and stops the heartbeat (the
// spinner does not animate), so the final screen MUST clearly read as terminal
// — not look like a frozen live screen. Assert the failed terminal frame shows
// the explicit outcome and a quit affordance, and the lead glyph is the outcome
// marker (not a spinner frame).
func TestModel_TerminalStateIsUnambiguous(t *testing.T) {
	m := newTestModel(fixedClock(at(5 * time.Minute)))
	m.holdOnTerminal = true // interactive: hold so the frame is the held terminal screen
	m = send(m, phaseMsg{phase: "partitioning", at: at(30 * time.Second)})
	m = send(m, opTerminalMsg{failed: true, reason: "sgdisk exit 2 (device busy?)", at: at(2 * time.Minute)})

	v := viewText(m)
	if !strings.Contains(v, "FAILED at partitioning") {
		t.Errorf("terminal failure frame must name the failed phase unambiguously:\n%s", v)
	}
	if !strings.Contains(v, "[q] quit") {
		t.Errorf("terminal frame must show a quit affordance:\n%s", v)
	}
	// The heartbeat is stopped once done; assert it no longer reschedules so the
	// spinner is genuinely static (this is correct behavior, and the static
	// outcome glyph + FAILED text are what disambiguate the screen).
	_, cmd := m.Update(uiTickMsg{})
	if cmd != nil {
		t.Errorf("terminal op must not keep rescheduling the heartbeat:\n%s", v)
	}
}

// TestModel_ReconnectBanner: a console reconnect toggles the box title without
// disturbing the rest of the layout. The toggle is not machine activity, so it
// leaves "last update" alone: a quiet console behind a load balancer idle
// timeout drops and redials every few minutes on a host doing nothing.
func TestModel_ReconnectBanner(t *testing.T) {
	clockT := at(100 * time.Second)
	m := newTestModel(func() time.Time { return clockT })
	m = send(m, logEnvelopeMsg{id: 1, env: envAt("dhcp_ack", at(60*time.Second))})
	m = send(m, consoleBackfillDoneMsg{})
	m = send(m, consoleReconnectMsg{reconnecting: true})
	if !strings.Contains(viewText(m), "reconnecting") {
		t.Errorf("reconnect banner missing:\n%s", viewText(m))
	}
	clockT = at(110 * time.Second)
	m = send(m, consoleReconnectMsg{reconnecting: false})
	if strings.Contains(viewText(m), "reconnecting") {
		t.Errorf("reconnect banner should clear:\n%s", viewText(m))
	}
	if ago, ok := m.frameInput().lastUpdateAgo(); !ok || ago != 50*time.Second {
		t.Errorf("reconnect toggles must not reset staleness: last update %s ago (ok=%v), want 50s from the last envelope", ago, ok)
	}
}

// TestModel_IgnoresMouse guards the invariant that justifies running the
// program WITHOUT mouse capture: the reducer consumes no mouse input, so
// capturing the mouse buys nothing and only breaks native terminal text
// selection. The View must request the alt-screen and no mouse mode, and a
// mouse event must be an inert no-op — no Quit, no observable state change in
// the rendered view. If a future change starts handling mouse events (e.g.
// drag-scroll), this test will fail, flagging that mouse capture must be
// reintroduced (behind a keybind toggle) alongside the handler.
func TestModel_IgnoresMouse(t *testing.T) {
	m := newTestModel(fixedClock(at(time.Second)))
	v := m.View()
	if !v.AltScreen {
		t.Error("View must request the alt-screen")
	}
	if v.MouseMode != tea.MouseModeNone {
		t.Errorf("View must not capture the mouse (MouseMode = %v): it breaks native text selection", v.MouseMode)
	}
	before := v.Content
	next, cmd := m.Update(tea.MouseMotionMsg{Button: tea.MouseLeft})
	// Do not invoke cmd here: a tea.Cmd is a func() tea.Msg that may block
	// (timers, I/O). This branch only runs when the test is already failing, so
	// flag the regression without executing the stray command.
	if cmd != nil {
		t.Errorf("mouse event should produce no command, got a non-nil tea.Cmd")
	}
	if got := next.(*tuiModel).View().Content; got != before {
		t.Errorf("mouse event must not change the rendered view\nbefore:\n%s\nafter:\n%s", before, got)
	}
}
