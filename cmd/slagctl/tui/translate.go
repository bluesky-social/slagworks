package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// This file translates raw stream inputs into the model's message vocabulary.
// It is the seam between the WS goroutines (which produce slogkit.Envelopes and
// asciicast bytes) and the pure reducer. Keeping translation here — separate
// from both the goroutine plumbing and the reducer — means the "what does this
// envelope mean" logic is unit-testable without a socket or a tea.Program.
//
// The key interpretation is completion (plan D6): global emits source=global
// lifecycle envelopes carrying status in Extra. A status=completed/failed
// envelope is the authoritative terminal signal and becomes an opTerminalMsg;
// a claim envelope becomes a claimedMsg. Everything else with content is a
// control-plane log line (logEnvelopeMsg), which the reducer routes to the log
// pane and — if it carries a phase field — the lifecycle spine.
//
// Those signals only count for the watched op. The logs stream ORs machine_id
// and op_id (logstore.Query), so it also carries every envelope of any other
// op on the same machine, including that op's claim and terminal envelopes.
// An envelope stamped with a different op_id is shown as a log line and
// nothing more: it never ends the watch, sets the worker, or moves the spine.
// An envelope with no op_id (machine-scoped DHCP/TFTP lines stamped before
// the op is resolved) is attributed to the watched op.

// translateEnvelope converts one log-stream envelope into the model message it
// should drive for a watch of op watched. Returns nil for envelopes the TUI
// ignores. SOL bytes never appear on this stream — they ride the separate
// console cast pipeline.
func translateEnvelope(id int64, env slogkit.Envelope, watched uuid.UUID) tea.Msg {
	if env.OpID != uuid.Nil && env.OpID != watched {
		return logEnvelopeMsg{id: id, env: env, otherOp: true}
	}
	// D6 global lifecycle envelopes carry a status in Extra. A terminal status
	// is the completion signal; the claim envelope carries claimed_by.
	if env.Source == slogkit.SourceGlobal {
		if status := env.StringExtra("status"); status != "" {
			switch status {
			case "completed":
				return opTerminalMsg{failed: false, at: env.TS}
			case "failed":
				return opTerminalMsg{failed: true, reason: env.StringExtra("error"), at: env.TS}
			}
		}
		if worker := env.StringExtra("claimed_by"); worker != "" {
			return claimedMsg{worker: worker}
		}
	}

	// Otherwise it's a control-plane log line. The reducer appends it to the
	// log pane and advances the lifecycle off env.Phase if present.
	return logEnvelopeMsg{id: id, env: env}
}
