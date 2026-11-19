package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

func env(src slogkit.Source, d time.Duration, msg string) slogkit.Envelope {
	return slogkit.Envelope{
		TS:     t0.Add(d),
		Source: src,
		Msg:    msg,
	}
}

// mkEnv builds an envelope with an explicit timestamp, phase, and Extra map —
// the shape the frame tests need for realistic log lines.
func mkEnv(src string, ts time.Time, phase, msg string, extra map[string]any) slogkit.Envelope {
	return slogkit.Envelope{
		TS:     ts,
		Source: slogkit.Source(src),
		Phase:  phase,
		Msg:    msg,
		Extra:  extra,
	}
}

// TestLogPane_DedupByID guards reconnect resume (D4): the model reconnects with
// last_seen_id = MaxID, but a server may replay the boundary envelope. Lines at
// or behind maxID are dropped, so a reconnect doesn't double-render.
func TestLogPane_DedupByID(t *testing.T) {
	p := newLogPane(time.UTC)
	p.Append(5, env(slogkit.SourceSite, 0, "five"))
	p.Append(6, env(slogkit.SourceSite, time.Second, "six"))
	if p.MaxID() != 6 {
		t.Errorf("MaxID = %d, want 6", p.MaxID())
	}
	// Replay of 6 (and an older 3) on reconnect: both dropped.
	if p.Append(6, env(slogkit.SourceSite, time.Second, "six again")) {
		t.Error("duplicate id 6 should not append")
	}
	if p.Append(3, env(slogkit.SourceSite, 0, "stale three")) {
		t.Error("behind-cursor id 3 should not append")
	}
	if got := len(p.Lines()); got != 2 {
		t.Errorf("ring has %d lines, want 2 after dedup", got)
	}
}

// TestLogPane_RingCap proves the strict cap (D7): the ring never exceeds
// logRingCap, and eviction drops the oldest, keeping the newest tail.
func TestLogPane_RingCap(t *testing.T) {
	p := newLogPane(time.UTC)
	total := logRingCap + 50
	for i := 1; i <= total; i++ {
		p.Append(int64(i), env(slogkit.SourceAgent, time.Duration(i)*time.Second, "line"))
	}
	lines := p.Lines()
	if len(lines) != logRingCap {
		t.Fatalf("ring grew past cap: %d > %d", len(lines), logRingCap)
	}
	// The newest envelope must be present; the oldest must be gone. maxID is
	// unaffected by eviction (it's a monotonic cursor, not a ring index).
	if p.MaxID() != int64(total) {
		t.Errorf("MaxID = %d, want %d", p.MaxID(), total)
	}
}

// TestLogPane_Format renders the standard fields and the reused refinements:
// phase surfaced inline, and the agent exec-argv expansion.
// Timestamps format through the injected UTC location (D9).
func TestLogPane_Format(t *testing.T) {
	p := newLogPane(time.UTC)

	// Plain global line with a phase field.
	e := env(slogkit.SourceGlobal, 2*time.Second, "operation claimed")
	e.Phase = "power_cycle"
	p.Append(1, e)

	// Agent exec envelope: bare msg "exec", real command in Extra.
	ex := env(slogkit.SourceAgent, 3*time.Second, "exec")
	ex.Extra = map[string]any{
		"argv":        "sgdisk --zap-all /dev/nvme0n1",
		"exit_code":   float64(0),
		"duration_ms": float64(212),
	}
	p.Append(2, ex)

	// Exec that failed and ran long.
	bad := env(slogkit.SourceAgent, 4*time.Second, "exec")
	bad.Extra = map[string]any{
		"argv":        "mkfs.ext4 /dev/md0",
		"exit_code":   float64(1),
		"duration_ms": float64(1843),
		"err":         "device busy",
	}
	p.Append(3, bad)

	lines := p.Lines()
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "phase=power_cycle operation claimed") {
		t.Errorf("phase not surfaced inline: %q", lines[0])
	}
	if !strings.HasPrefix(lines[0], "10:14:02 [global") {
		t.Errorf("timestamp/source prefix wrong (UTC): %q", lines[0])
	}
	if !strings.Contains(lines[1], "exec: sgdisk --zap-all /dev/nvme0n1") {
		t.Errorf("exec argv not expanded: %q", lines[1])
	}
	// exit 0 and sub-second duration are omitted (only nonzero exit / >1s dur).
	if strings.Contains(lines[1], "exit") || strings.Contains(lines[1], "ms)") {
		t.Errorf("clean fast exec should not show exit/duration: %q", lines[1])
	}
	if !strings.Contains(lines[2], "→ exit 1") || !strings.Contains(lines[2], "(1843ms)") {
		t.Errorf("failed slow exec missing exit/duration: %q", lines[2])
	}
	if !strings.Contains(lines[2], "err=device busy") {
		t.Errorf("exec err not rendered: %q", lines[2])
	}

	goldenScreen(t, "logpane_format", strings.Join(lines, "\n"))
}

// TestLogPane_EscapesTerminalControls: every string on an envelope is text the
// sending host chose (agent envelopes arrive through the site's
// unauthenticated /logs/ingest). The formatted line must carry escape
// sequences and line breaks as visible text, one envelope per row, so they
// cannot repaint the frame, plant a hyperlink or forge a log row.
func TestLogPane_EscapesTerminalControls(t *testing.T) {
	p := newLogPane(time.UTC)
	e := env(slogkit.Source("agent\x1b[2J"), 0, "ok\x1b]8;;http://evil.example/\x07click\x1b]8;;\x07\nforged row")
	e.Phase = "partitioning\x1b[1A"
	p.Append(1, e)
	ex := env(slogkit.SourceAgent, time.Second, "exec")
	ex.Extra = map[string]any{"argv": "sgdisk \x1b_apc\x1b\\", "err": "boom\r\u009d52;c;cHduZWQ=\u009c"}
	p.Append(2, ex)

	for _, line := range p.Lines() {
		assertNoTerminalControls(t, line)
		if strings.Contains(line, "\n") {
			t.Errorf("one envelope rendered as several rows: %q", line)
		}
	}
	lines := strings.Join(p.Lines(), "\n")
	for _, want := range []string{`ok\x1b]8;;http://evil.example/\x07click`, `\nforged row`, `phase=partitioning\x1b[1A`, `sgdisk \x1b_apc\x1b\`, `boom\r\u009d52`} {
		if !strings.Contains(lines, want) {
			t.Errorf("log pane missing the escaped text %q:\n%s", want, lines)
		}
	}
}

// TestLogPane_PhaseSurvivesExec is the regression for the exec-overwrites-phase
// bug: an agent exec envelope that ALSO carries a phase field must keep the
// phase prefix, not have it clobbered by the argv expansion. The mockup shows
// "phase=partitioning exec: sgdisk ...".
func TestLogPane_PhaseSurvivesExec(t *testing.T) {
	p := newLogPane(time.UTC)
	e := env(slogkit.SourceAgent, 0, "exec")
	e.Phase = "partitioning"
	e.Extra = map[string]any{"argv": "sgdisk --zap-all /dev/nvme0n1"}
	p.Append(1, e)

	line := p.Lines()[0]
	if !strings.Contains(line, "phase=partitioning") {
		t.Errorf("phase prefix clobbered by exec expansion: %q", line)
	}
	if !strings.Contains(line, "exec: sgdisk") {
		t.Errorf("exec argv missing: %q", line)
	}
	// Order: phase prefix precedes the exec body.
	if strings.Index(line, "phase=") > strings.Index(line, "exec:") {
		t.Errorf("phase should precede exec body: %q", line)
	}
}

// TestLogPane_TimezoneInjection is the D9 guard proper: the same fixed-UTC
// envelope formats to different wall-clock strings under different locations,
// so a hardcoded time.Local would flap goldens between PST dev and UTC CI.
// Injecting the zone makes the rendered line deterministic.
func TestLogPane_TimezoneInjection(t *testing.T) {
	e := env(slogkit.SourceSite, 0, "x") // t0 = 10:14:00 UTC

	utc := newLogPane(time.UTC)
	utc.Append(1, e)

	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	pst := newLogPane(la)
	pst.Append(1, e)

	if utc.Lines()[0] == pst.Lines()[0] {
		t.Errorf("expected zone to change rendered time, both = %q", utc.Lines()[0])
	}
	if !strings.HasPrefix(utc.Lines()[0], "10:14:00") {
		t.Errorf("UTC render wrong: %q", utc.Lines()[0])
	}
	if !strings.HasPrefix(pst.Lines()[0], "03:14:00") { // UTC-7 in May (PDT)
		t.Errorf("PST render wrong: %q", pst.Lines()[0])
	}
}
