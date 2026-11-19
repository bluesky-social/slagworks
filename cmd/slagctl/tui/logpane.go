package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// This file is the log pane's data core: a bounded ring of pre-formatted log
// lines fed by the /api/v1/logs/stream envelope flow, plus the envelope→line
// formatting. It is the control-plane view — SOL bytes are excluded by
// construction (plan D3); they live only in the console pane.
//
// What's here is the pure, golden-testable half: ring eviction and line
// formatting with an injectable *time.Location (plan D9, so timestamps don't
// flap between a PST laptop and UTC CI). What is NOT here is the
// bubbles/viewport wiring — SetContent throttling, AtBottom auto-tail/detach,
// mouse scroll (plan D7). That is bubbletea-coupled and lands with the composed
// model; the ring deliberately exposes Lines() so the composition layer can
// rebuild the viewport from a stable snapshot on its own throttle.
//
// The ring stays at a strict cap always (plan D7): we never grow it, because an
// unbounded buffer reintroduces the O(N) SetContent freeze the cap exists to
// prevent. Full history beyond the cap is `slagctl logs --op <id>`.

// logRingCap bounds the live ring. The mockup's log pane shows a handful of
// lines; 1000 is far more scrollback than any terminal holds, and re-wrapping
// ~1k lines on a viewport rebuild is sub-millisecond (plan D7's "keep the
// buffer small" lever). Full history is the standalone `logs` command.
const logRingCap = 1000

// logLine is one rendered control-plane log line plus the envelope id it came
// from. The id is the log_envelopes.id BIGSERIAL; the pane tracks the max id
// applied so a logs-stream reconnect can resume with last_seen_id (D4) instead
// of replaying full history.
type logLine struct {
	id   int64
	text string
}

// logPane holds the bounded ring and the formatting clock/zone. Single-owned
// by the update loop like the other panes — no mutex.
type logPane struct {
	lines []logLine
	maxID int64 // highest envelope id applied; 0 = none yet
	loc   *time.Location
}

func newLogPane(loc *time.Location) *logPane {
	if loc == nil {
		loc = time.Local
	}
	return &logPane{loc: loc}
}

// Append formats and rings one envelope. Envelopes at or behind maxID are
// dropped as duplicates (a reconnect with last_seen_id shouldn't double-render).
// Returns true if the line was appended.
func (p *logPane) Append(id int64, env slogkit.Envelope) bool {
	if id != 0 && id <= p.maxID {
		return false
	}
	p.lines = append(p.lines, logLine{id: id, text: p.format(env)})
	if id > p.maxID {
		p.maxID = id
	}
	if len(p.lines) > logRingCap {
		p.lines = p.lines[len(p.lines)-logRingCap:]
	}
	return true
}

// Lines returns the current ring as plain text lines, oldest first. The
// composition layer joins these and SetContents the viewport on its throttle.
func (p *logPane) Lines() []string {
	out := make([]string, len(p.lines))
	for i, l := range p.lines {
		out[i] = l.text
	}
	return out
}

// MaxID is the resume cursor for a logs-stream reconnect (D4).
func (p *logPane) MaxID() int64 { return p.maxID }

// format renders one envelope to a single display line. Mirrors the human
// renderer in logs_follow.go but drops the per-line host/op columns: this is a
// single-machine, single-op view (the header already names them), so the
// mockup's lines are just "10:14:02 [agent ] phase=partitioning exec: …".
// Two refinements carry over: the phase field is surfaced inline, and agent
// "exec" envelopes render their actual argv + exit/duration from Extra
// rather than the useless bare "exec". The phase prefix is applied
// after the exec expansion so it survives on exec lines. Timestamps format
// through the injected location (D9).
//
// The result never contains a line break. renderLogBody gives each entry one
// row, but a message can carry several lines (a subprocess's multi-line
// output, or the bare CRs of progress output), so they are joined with a
// visible " ⏎ " and a trailing break is dropped. The unjoined text is in
// `slagctl machine logs`.
func (p *logPane) format(env slogkit.Envelope) string {
	ts := env.TS.In(p.loc).Format("15:04:05")
	// Build the message body first, then prepend the phase prefix once. The
	// exec expansion replaces the body, so prefixing after it (not before)
	// keeps phase context on exec lines -- the mockup shows
	// "phase=partitioning exec: sgdisk ...".
	body := env.Msg
	if env.Source == slogkit.SourceAgent && env.Msg == "exec" {
		if argv := env.StringExtra("argv"); argv != "" {
			info := ""
			if code, ok := env.IntExtra("exit_code"); ok && code != 0 {
				info = fmt.Sprintf(" → exit %d", code)
			}
			if dur, ok := env.IntExtra("duration_ms"); ok && dur > 1000 {
				info += fmt.Sprintf(" (%dms)", dur)
			}
			body = "exec: " + argv + info
			if errStr := env.StringExtra("err"); errStr != "" {
				body += " err=" + errStr
			}
		}
	}
	if env.Phase != "" {
		body = fmt.Sprintf("phase=%s %s", env.Phase, body)
	}
	// Every string field is text the sending host chose (agent envelopes
	// arrive through the site's unauthenticated /logs/ingest). Line breaks
	// fold to a visible marker so the envelope stays on one row, then the
	// line is escaped as a whole: any other control sequence in it renders
	// as visible text.
	line := fmt.Sprintf("%s [%-6s] %s", ts, env.Source, body)
	return format.Untrusted(strings.ReplaceAll(strings.TrimRight(lineBreaks.Replace(line), "\n"), "\n", " ⏎ "))
}
