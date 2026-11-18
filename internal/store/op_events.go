package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// This file emits global's op-lifecycle transitions onto the structured-logs
// pipeline (plan D6). Before this, global-side op state changes only went to
// stderr via slog, invisible to `slagctl logs` and the deploy TUI — so the TUI
// could not learn claimed_by or completion from the stream, and an op that
// failed before any worker claimed it (pending-expiry) produced no phase
// envelope at all.
//
// The emission lives INSIDE the store's transition transactions, at the
// central primitives (ClaimOperation, FailOperation, CompleteOperationWithState
// / CompleteActiveOperation), not at the sweep call sites in server.go. That
// guarantees 100% terminal-state coverage: every path that ends or claims an
// op, by any caller, emits — a synchronous FailOperation (claim-time validation
// failure, a future manual-cancel) can't mutate state to terminal and emit
// nothing.
//
// Transactionally-correct point (plan D6): the envelope row is INSERTed into
// log_envelopes in the SAME tx as the state mutation. Not a live-sink callback
// (which would broadcast a phantom terminal event on rollback) and not a
// post-commit hook (which would lose the event on a crash between commit and
// hook). /logs/stream's live tail polls the table on a cursor, so a row written
// in the mutation's tx becomes visible exactly when the mutation commits, and
// never otherwise: rollback → no row → no event.
//
// host is the constant identifying global as the emitter. The structured-logs
// schema requires a non-empty source; "global" is the canonical value.
const opEventHost = "slagworksd-global"

// emitClaimEvent records a source=global "operation claimed" envelope carrying
// the claiming worker, so the TUI learns claimed_by off the stream. Called
// from ClaimOperation inside the claim tx, only after the pending→claimed CAS
// has actually flipped a row (so a lost claim race emits nothing).
func emitClaimEvent(tx *gorm.DB, op *model.Operation, now time.Time) error {
	env := slogkit.Envelope{
		TS:        now,
		Level:     slogkit.LevelInfo,
		Source:    slogkit.SourceGlobal,
		Host:      opEventHost,
		Msg:       "operation claimed",
		MachineID: op.MachineID,
		OpID:      op.ID,
		TraceID:   op.TraceID,
		Extra:     map[string]any{"claimed_by": op.ClaimedBy},
	}
	return logstore.InsertTx(tx, env)
}

// emitTerminalEvent records a source=global terminal envelope (status +
// optional error) when an op reaches completed or failed. Called from the
// completion / failure primitives inside their tx, only after the guarded
// UPDATE has flipped a row.
//
// errMsg is the failure cause for failed ops ("" for success). The TUI keys
// completion off this envelope: status=completed → success teardown,
// status=failed → error pane + non-zero exit.
func emitTerminalEvent(tx *gorm.DB, op *model.Operation, status model.OperationStatus, errMsg string, now time.Time) error {
	extra := map[string]any{"status": string(status)}
	level := slogkit.LevelInfo
	msg := "operation completed"
	if status == model.OpFailed {
		level = slogkit.LevelError
		msg = "operation failed"
		if errMsg != "" {
			extra["error"] = errMsg
		}
	}
	env := slogkit.Envelope{
		TS:        now,
		Level:     level,
		Source:    slogkit.SourceGlobal,
		Host:      opEventHost,
		Msg:       msg,
		MachineID: op.MachineID,
		OpID:      op.ID,
		TraceID:   op.TraceID,
		Extra:     extra,
	}
	return logstore.InsertTx(tx, env)
}

// failResultMessage pulls the human error string out of a FailOperation result
// payload, which callers shape as {"error": "..."}. Returns "" for a nil or
// unparseable result, so the terminal envelope just omits the error field.
func failResultMessage(result []byte) string {
	if len(result) == 0 {
		return ""
	}
	var r struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.Error
}
