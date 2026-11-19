package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/bluesky-social/slagworks/internal/model"
)

// lifecycleWidth is the inner width of the lifecycle box from the mockup
// (╭─ lifecycle ──...──╮ is 26 cells wide → 24 inner after the borders).
const lifecycleWidth = 24

// fixedClock returns a now() that always reports base, for deterministic
// current-phase elapsed rendering (plan D9).
func fixedClock(base time.Time) func() time.Time {
	return func() time.Time { return base }
}

// at builds a timestamp t0 + d for terse phase-arrival scripting.
var t0 = time.Date(2026, 5, 31, 10, 14, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// TestLifecycle_DeploySpineFull renders the full deploy runway up front, before
// any phase arrives: every phase pending (○), nothing spinning. This is the
// "whole runway visible from the start" guarantee (plan O5).
func TestLifecycle_DeploySpineFull(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(t0)
	got := plain(l.Render(lifecycleWidth, "⠙"))

	if n := strings.Count(got, "\n"); n != len(deploySpine)-1 {
		t.Errorf("got %d newlines, want %d (one row per spine phase)", n, len(deploySpine)-1)
	}
	if !strings.Contains(got, "○ power_cycle") {
		t.Errorf("first phase not rendered pending:\n%s", got)
	}
	if !strings.Contains(got, "○ completed") {
		t.Errorf("terminal phase not in runway:\n%s", got)
	}
	goldenScreen(t, "lifecycle_deploy_initial", got)
}

// TestLifecycle_MidDeploy mirrors the mockup's happy-path: phases 1-6 done with
// derived durations, partitioning current and live-ticking. The injected clock
// is 47s past partitioning's arrival, so it must read (47s) — the exact mockup
// value.
func TestLifecycle_MidDeploy(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	// Arrivals chosen so the derived durations match the mockup:
	// power_cycle 2s, pxe_booting 14s, dhcp_ack 1s, ipxe_dhcp 2s,
	// ipxe_loaded 8s, agent_starting 3s, then partitioning current.
	l.Observe("power_cycle", at(0))
	l.Observe("pxe_booting", at(2*time.Second))
	l.Observe("dhcp_ack", at(16*time.Second))
	l.Observe("ipxe_dhcp", at(17*time.Second))
	l.Observe("ipxe_loaded", at(19*time.Second))
	l.Observe("agent_starting", at(27*time.Second))
	l.Observe("partitioning", at(30*time.Second))
	l.now = fixedClock(at(30*time.Second + 47*time.Second))

	got := plain(l.Render(lifecycleWidth, "⠙"))
	if !strings.Contains(got, "✓ power_cycle") {
		t.Errorf("power_cycle not done:\n%s", got)
	}
	if !strings.Contains(got, "(47s)") {
		t.Errorf("partitioning live-elapsed not 47s:\n%s", got)
	}
	if !strings.Contains(got, "⠙ partitioning") {
		t.Errorf("partitioning not spinning:\n%s", got)
	}
	// pxe_booting took 14s (16s - 2s).
	if !strings.Contains(got, "14s") {
		t.Errorf("pxe_booting duration not 14s:\n%s", got)
	}
	// Future phases stay pending.
	if !strings.Contains(got, "○ rsyncing_os") {
		t.Errorf("rsyncing_os should still be pending:\n%s", got)
	}
	goldenScreen(t, "lifecycle_deploy_mid", got)
}

// TestLifecycle_Backfill proves the late-join / conditional-phase rule (O5):
// jumping from finalizing straight to awaiting_phone_home (bmc_reconcile never
// fired — agent had no reporter) marks bmc_reconcile skipped, not stuck-pending.
func TestLifecycle_Backfill(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("finalizing", at(0))
	l.Observe("awaiting_phone_home", at(4*time.Second))

	if !strings.Contains(plain(l.Render(lifecycleWidth, "⠙")), "— bmc_reconcile") {
		t.Errorf("bmc_reconcile not marked skipped:\n%s", plain(l.Render(lifecycleWidth, "⠙")))
	}
	// Everything before finalizing also backfills skipped (we joined late).
	if !strings.Contains(plain(l.Render(lifecycleWidth, "⠙")), "— power_cycle") {
		t.Errorf("earlier phases not backfilled skipped:\n%s", plain(l.Render(lifecycleWidth, "⠙")))
	}
}

// TestLifecycle_Fail marks the current phase failed (✗) with its duration and
// leaves the unreached runway pending (○) — matching the failure mockup, which
// does NOT skip-mark phases the op never got to.
func TestLifecycle_Fail(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("power_cycle", at(0))
	l.Observe("pxe_booting", at(2*time.Second))
	l.Observe("dhcp_ack", at(16*time.Second))
	l.Observe("ipxe_dhcp", at(17*time.Second))
	l.Observe("ipxe_loaded", at(19*time.Second))
	l.Observe("agent_starting", at(27*time.Second))
	l.Observe("partitioning", at(30*time.Second))
	l.Fail(at(42 * time.Second)) // 12s into partitioning

	got := plain(l.Render(lifecycleWidth, "⠙"))
	if !strings.Contains(got, "✗ partitioning") {
		t.Errorf("partitioning not marked failed:\n%s", got)
	}
	if !strings.Contains(got, "12s") {
		t.Errorf("failed-phase duration not 12s:\n%s", got)
	}
	if !strings.Contains(got, "○ rsyncing_os") {
		t.Errorf("unreached phases should stay pending, not skipped:\n%s", got)
	}
	if !l.failed {
		t.Error("failed flag not set")
	}
	goldenScreen(t, "lifecycle_deploy_failed", got)
}

// TestLifecycle_ObserveAfterFailIsIgnored: Fail ends the spine. A phase
// observed afterwards must not start spinning, and must not skip-mark the
// pending runway between it and the failed phase, which Fail leaves pending.
func TestLifecycle_ObserveAfterFailIsIgnored(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("agent_starting", at(0))
	l.Observe("partitioning", at(10*time.Second))
	l.Fail(at(30 * time.Second))
	before := plain(l.Render(lifecycleWidth, "⠙"))

	l.Observe("installing_bootloader", at(40*time.Second))

	if got := l.CurrentPhase(); got != "" {
		t.Errorf("CurrentPhase() = %q after Fail, want none", got)
	}
	if got := plain(l.Render(lifecycleWidth, "⠙")); got != before {
		t.Errorf("spine changed after Fail:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestLifecycle_Completed renders the success terminal: every phase done with a
// duration, completed marked ✓ with no trailing duration, nothing spinning.
func TestLifecycle_Completed(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(10 * time.Minute))
	base := time.Duration(0)
	for _, p := range deploySpine {
		l.Observe(p, at(base))
		base += 5 * time.Second
	}
	got := plain(l.Render(lifecycleWidth, "⠙"))
	if strings.Contains(got, "⠙") {
		t.Errorf("nothing should spin after completion:\n%s", got)
	}
	if l.curIdx != -1 {
		t.Errorf("curIdx = %d, want -1 after completed", l.curIdx)
	}
	// completed row carries no duration.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "completed") && strings.ContainsAny(line, "0123456789") {
			t.Errorf("completed row should have no duration: %q", line)
		}
	}
}

// TestLifecycle_CommissionSpine confirms op-type awareness: commission gets its
// own divergent spine (collecting_inventory/reporting/console_probe), not the
// deploy phases.
func TestLifecycle_CommissionSpine(t *testing.T) {
	l := newLifecyclePane(model.OpCommission)
	l.now = fixedClock(t0)
	got := plain(l.Render(lifecycleWidth, "⠙"))
	if !strings.Contains(got, "○ collecting_inventory") {
		t.Errorf("commission spine missing collecting_inventory:\n%s", got)
	}
	if strings.Contains(got, "partitioning") {
		t.Errorf("commission spine should not contain deploy's partitioning:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n != len(commissionSpine)-1 {
		t.Errorf("got %d newlines, want %d", n, len(commissionSpine)-1)
	}
}

// TestLifecycle_BMCReconcileSpine: a bmc_reconcile op boots nothing, so
// its spine is just the worker's bmc_reconcile phase and completion — not
// the deploy spine (which would render ten never-visited boot phases as
// skipped around the shared bmc_reconcile phase name).
func TestLifecycle_BMCReconcileSpine(t *testing.T) {
	l := newLifecyclePane(model.OpBMCReconcile)
	l.now = fixedClock(t0)
	got := plain(l.Render(lifecycleWidth, "⠙"))
	if !strings.Contains(got, "bmc_reconcile") {
		t.Errorf("bmc_reconcile spine missing its phase:\n%s", got)
	}
	if strings.Contains(got, "pxe_booting") || strings.Contains(got, "partitioning") {
		t.Errorf("bmc_reconcile spine must not contain boot/deploy phases:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n != len(bmcReconcileSpine)-1 {
		t.Errorf("got %d newlines, want %d", n, len(bmcReconcileSpine)-1)
	}
}

// TestLifecycle_CommissionBootPrefixAnimates is a regression test: a
// commission op must animate the shared boot prefix (power_cycle → pxe_booting
// → dhcp_ack → ipxe_dhcp → ipxe_loaded) the same way deploy does, rather than
// sitting all-pending until the agent's first report. Feeding those phase
// observations onto the commission spine must light up the rows.
func TestLifecycle_CommissionBootPrefixAnimates(t *testing.T) {
	l := newLifecyclePane(model.OpCommission)
	l.Observe("power_cycle", at(0))
	l.Observe("pxe_booting", at(2*time.Second))
	l.Observe("dhcp_ack", at(16*time.Second))
	l.Observe("ipxe_dhcp", at(17*time.Second))
	l.Observe("ipxe_loaded", at(19*time.Second))
	l.now = fixedClock(at(25 * time.Second))

	got := plain(l.Render(lifecycleWidth, "⠙"))
	if !strings.Contains(got, "✓ power_cycle") {
		t.Errorf("power_cycle not done on commission spine:\n%s", got)
	}
	if !strings.Contains(got, "⠙ ipxe_loaded") {
		t.Errorf("ipxe_loaded not spinning on commission spine:\n%s", got)
	}
	// The agent-emitted phases stay pending until the agent reports.
	if !strings.Contains(got, "○ collecting_inventory") {
		t.Errorf("collecting_inventory should still be pending:\n%s", got)
	}
}

// TestLifecycle_AwaitingAgentReport proves the handoff signal: while the
// current phase is ipxe_loaded (last worker-emitted boot phase) the pane
// reports AwaitingAgentReport, and once an agent phase arrives it flips off.
func TestLifecycle_AwaitingAgentReport(t *testing.T) {
	l := newLifecyclePane(model.OpCommission)
	l.now = fixedClock(at(time.Minute))

	if l.AwaitingAgentReport() {
		t.Error("should not be awaiting agent report before booting")
	}
	l.Observe("ipxe_loaded", at(0))
	if !l.AwaitingAgentReport() {
		t.Error("should be awaiting agent report while current phase is ipxe_loaded")
	}
	// First agent report advances the current phase off the handoff boundary.
	l.Observe("agent_starting", at(5*time.Second))
	if l.AwaitingAgentReport() {
		t.Error("should no longer await agent report once agent_starting arrives")
	}
}

// TestLifecycle_HighWaterPhase confirms the exit-summary helper names the
// furthest phase reached even when no phase is currently spinning.
func TestLifecycle_HighWaterPhase(t *testing.T) {
	l := newLifecyclePane(model.OpCommission)
	if l.HighWaterPhase() != "" {
		t.Errorf("HighWaterPhase before any observation = %q, want empty", l.HighWaterPhase())
	}
	l.Observe("power_cycle", at(0))
	l.Observe("ipxe_loaded", at(5*time.Second))
	if l.HighWaterPhase() != "ipxe_loaded" {
		t.Errorf("HighWaterPhase = %q, want ipxe_loaded", l.HighWaterPhase())
	}
}

// TestLifecycle_DuplicateAndStale guards the Observe idempotency: re-observing
// the current phase, or a phase behind the high-water mark, is a no-op (must
// not zero a derived duration or re-spin a finished phase).
func TestLifecycle_DuplicateAndStale(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("power_cycle", at(0))
	l.Observe("pxe_booting", at(2*time.Second))
	// Stale re-report of power_cycle: must not resurrect it as current.
	l.Observe("power_cycle", at(50*time.Second))
	if l.curIdx != l.index["pxe_booting"] {
		t.Errorf("stale re-observe moved current phase; curIdx=%d", l.curIdx)
	}
	if l.rows[l.index["power_cycle"]].duration != 2*time.Second {
		t.Errorf("power_cycle duration corrupted by stale re-observe: %v",
			l.rows[l.index["power_cycle"]].duration)
	}
	// Duplicate current: no-op.
	l.Observe("pxe_booting", at(60*time.Second))
	if l.rows[l.index["pxe_booting"]].status != phaseCurrent {
		t.Error("duplicate observe of current phase changed its status")
	}
}

// TestLifecycle_LayoutWidth checks the row layout flushes the right field to
// the box edge and never exceeds the width, including the ellipsized long
// phase name (installing_bootloader) from the mockup.
func TestLifecycle_LayoutWidth(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("installing_bootloader", at(0))
	l.now = fixedClock(at(11 * time.Second))
	got := plain(l.Render(lifecycleWidth, "⠙"))
	for line := range strings.SplitSeq(got, "\n") {
		if w := len([]rune(line)); w > lifecycleWidth {
			t.Errorf("row exceeds width %d: %q (%d cells)", lifecycleWidth, line, w)
		}
	}
	// The long name must be ellipsized to fit alongside its (11s) elapsed.
	if !strings.Contains(got, "…") {
		t.Errorf("expected installing_bootloader to ellipsize at width %d:\n%s", lifecycleWidth, got)
	}
}

// TestLifecycle_NarrowWidthValidUTF8 guards the degenerate-width path: at a
// width too small to hold even the glyph+name, the truncation must slice on
// runes, not bytes — the glyph is a multi-byte box char (✓/○/—) and a byte
// slice would emit invalid UTF-8. Render at every tiny width and assert the
// output stays valid UTF-8.
func TestLifecycle_NarrowWidthValidUTF8(t *testing.T) {
	l := newLifecyclePane(model.OpDeploy)
	l.now = fixedClock(at(time.Minute))
	l.Observe("power_cycle", at(0))              // ✓ after we advance
	l.Observe("partitioning", at(5*time.Second)) // current (spinner)
	for w := range 8 {
		got := l.Render(w, "⠙")
		if !utf8.ValidString(got) {
			t.Errorf("width %d produced invalid UTF-8: %q", w, got)
		}
	}
}

// amberSGR is the 256-color foreground component (208) styleAmber emits under
// the ANSI256 profile. lipgloss folds the bold attribute into the same SGR
// sequence (e.g. "\x1b[1;38;5;208m"), so match the color sub-token rather than a
// full prefix. Used to assert the amber highlight is (or isn't) applied without
// scraping the glyph.
const amberSGR = "38;5;208"

// TestLifecycle_StuckPhaseTurnsAmber is the guard: a current phase
// running past stuckPhaseFloor (5m) renders amber, a cheap "something may be
// wrong" cue well before the op timeout. A phase under the floor stays the
// normal active color. Asserted as a 256-color terminal receives it (the
// goldens compare stripped text, so this is the only place color is
// observable).
func TestLifecycle_StuckPhaseTurnsAmber(t *testing.T) {
	// Fresh: partitioning current for 30s (< 5m floor) — not amber.
	fresh := newLifecyclePane(model.OpDeploy)
	fresh.now = fixedClock(at(30 * time.Second))
	fresh.Observe("partitioning", at(0))
	if got := downsample(t, fresh.Render(lifecycleWidth, "⠙"), colorprofile.ANSI256); strings.Contains(got, amberSGR) {
		t.Errorf("phase under the stuck floor should not be amber:\n%q", got)
	}

	// Stuck: partitioning current for 6m (> 5m floor) — amber.
	stuck := newLifecyclePane(model.OpDeploy)
	stuck.now = fixedClock(at(6 * time.Minute))
	stuck.Observe("partitioning", at(0))
	got := downsample(t, stuck.Render(lifecycleWidth, "⠙"), colorprofile.ANSI256)
	if !strings.Contains(got, amberSGR) {
		t.Errorf("phase past the stuck floor should render amber:\n%q", got)
	}
	// The amber styling must not perturb the visible width (color is a wrap).
	for line := range strings.SplitSeq(got, "\n") {
		if w := lipgloss.Width(line); w > lifecycleWidth {
			t.Errorf("amber line exceeds width %d: visible=%d\n%q", lifecycleWidth, w, line)
		}
	}
}
