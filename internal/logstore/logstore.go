// Package logstore wraps the global daemon's log_envelopes table.
// Reads/writes are deliberately kept off the main Store interface to
// keep the hot-path log pipeline isolated from machine/operation
// state CRUD; this package can be retargeted (Loki HTTP push, an
// alternative backend) without touching every caller of Store.
package logstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Store reads and writes log_envelopes rows.
type Store struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Store { return &Store{db: db} }

// envelopeRow mirrors the log_envelopes table.
type envelopeRow struct {
	ID               int64           `gorm:"primaryKey;autoIncrement"`
	TS               time.Time       `gorm:"column:ts;not null"`
	GlobalReceivedAt time.Time       `gorm:"column:global_received_at;not null;default:now()"`
	ReceivedAt       time.Time       `gorm:"column:received_at;not null"`
	Level            string          `gorm:"not null"`
	Source           string          `gorm:"not null"`
	Host             string          `gorm:"column:host"`
	MachineID        *uuid.UUID      `gorm:"column:machine_id;type:uuid"`
	OpID             *uuid.UUID      `gorm:"column:op_id;type:uuid"`
	TraceID          *uuid.UUID      `gorm:"column:trace_id;type:uuid"`
	Phase            string          `gorm:"column:phase"`
	Msg              string          `gorm:"column:msg;not null"`
	Payload          json.RawMessage `gorm:"column:payload;type:jsonb;not null"`
}

func (envelopeRow) TableName() string { return "log_envelopes" }

// InsertBatch persists envelopes from a worker. The site stamps
// received_at on each envelope before forwarding; if missing we fall
// back to time.Now(). Empty batches are a no-op.
func (s *Store) InsertBatch(ctx context.Context, envelopes []slogkit.Envelope) error {
	if len(envelopes) == 0 {
		return nil
	}
	rows := make([]envelopeRow, len(envelopes))
	for i, e := range envelopes {
		row, err := toRow(e)
		if err != nil {
			return fmt.Errorf("envelope %d: %w", i, err)
		}
		rows[i] = row
	}
	return s.db.WithContext(ctx).Create(&rows).Error
}

// InsertTx persists a single envelope using the supplied transaction
// handle, so the row commits atomically with whatever else tx is doing.
// This is the seam global's state-transition primitives use to emit
// op-lifecycle envelopes (claim / terminal) inside the same transaction
// as the operations/machines mutation — see the structured-logs plan's
// D6: the row becomes visible to /logs/stream exactly when the mutation
// commits, and never on rollback. It's a package function, not a method,
// because the insert rides the caller's tx and needs no Store handle —
// logstore stays the single owner of the log_envelopes schema (the
// envelopeRow mapping lives here), but the store doesn't take a sink.
func InsertTx(tx *gorm.DB, e slogkit.Envelope) error {
	row, err := toRow(e)
	if err != nil {
		return err
	}
	return tx.Create(&row).Error
}

// toRow maps a canonical envelope to its table row. received_at defaults
// to now() when the envelope doesn't carry one (global-originated
// envelopes have no upstream site stamp).
func toRow(e slogkit.Envelope) (envelopeRow, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return envelopeRow{}, fmt.Errorf("marshal envelope: %w", err)
	}
	row := envelopeRow{
		TS:         e.TS,
		ReceivedAt: time.Now().UTC(),
		Level:      string(e.Level),
		Source:     string(e.Source),
		Host:       e.Host,
		Phase:      e.Phase,
		Msg:        e.Msg,
		Payload:    payload,
	}
	if e.ReceivedAt != nil {
		row.ReceivedAt = *e.ReceivedAt
	}
	if e.MachineID != uuid.Nil {
		id := e.MachineID
		row.MachineID = &id
	}
	if e.OpID != uuid.Nil {
		id := e.OpID
		row.OpID = &id
	}
	if e.TraceID != uuid.Nil {
		id := e.TraceID
		row.TraceID = &id
	}
	return row, nil
}

// QueryFilter narrows backfill + live-tail reads.
type QueryFilter struct {
	TraceID     uuid.UUID
	MachineID   uuid.UUID
	MachineName string
	OpID        uuid.UUID
	Sources     []string
	MinLevel    string
	Since       time.Time

	// LastSeenID, when > 0, restricts to rows with id > LastSeenID.
	LastSeenID int64

	// Limit caps the rows returned by a single Query call.
	Limit int

	// LiveTailWatermark, if set, restricts to rows with
	// global_received_at <= now() - watermark. The plan's 500ms
	// watermark.
	LiveTailWatermark time.Duration
}

// Result pairs the canonical envelope with the row id, for resume.
type Result struct {
	ID       int64
	Envelope slogkit.Envelope
}

// Query reads matching rows in ts ASC order, capped at filter.Limit.
// Empty result is allowed; callers re-invoke for the next batch using
// the largest returned ID as LastSeenID.
func (s *Store) Query(ctx context.Context, filter QueryFilter) ([]Result, error) {
	q := s.db.WithContext(ctx).Model(&envelopeRow{})
	if filter.TraceID != uuid.Nil {
		q = q.Where("trace_id = ?", filter.TraceID)
	}
	// machine_id and op_id are OR'd (not AND'd) when both are present: when
	// watching an operation the operator wants ALL recent activity for the
	// target machine — including this machine's pre-op infra lines that carry
	// machine_id but no op_id (DHCP/TFTP, stamped before the op is resolved for
	// the MAC) AND a concurrent/conflicting op on the same machine,
	// which is exactly the kind of thing you want surfaced mid-deploy. The
	// caller bounds the noise with `since` (the TUI passes the op's create
	// time), so this is "recent activity for the machine or op", not all of
	// history. A bare machine or bare op filter (only one set) reduces to the
	// single equality.
	switch {
	case filter.MachineID != uuid.Nil && filter.OpID != uuid.Nil:
		// Parenthesize the OR: GORM joins successive .Where() calls with AND
		// and does NOT auto-group a raw-string condition, so an unparenthesized
		// "machine_id = ? OR op_id = ?" would bind as
		// "machine_id = ? OR (op_id = ? AND ts >= since AND id > cursor)" —
		// letting every machine_id match bypass the since-bound and the
		// pagination cursor and re-stream the machine's whole history. The
		// explicit parens keep the OR a single AND-term (roast HIGH, 2026-06-17).
		q = q.Where("(machine_id = ? OR op_id = ?)", filter.MachineID, filter.OpID)
	case filter.MachineID != uuid.Nil:
		q = q.Where("machine_id = ?", filter.MachineID)
	case filter.OpID != uuid.Nil:
		q = q.Where("op_id = ?", filter.OpID)
	}
	if filter.MachineName != "" {
		q = q.Where("payload->>'machine_name' = ?", filter.MachineName)
	}
	if len(filter.Sources) > 0 {
		q = q.Where("source IN ?", filter.Sources)
	}
	if filter.MinLevel != "" {
		levels := levelsAtOrAbove(filter.MinLevel)
		if len(levels) > 0 {
			q = q.Where("level IN ?", levels)
		}
	}
	if !filter.Since.IsZero() {
		q = q.Where("ts >= ?", filter.Since)
	}
	if filter.LastSeenID > 0 {
		q = q.Where("id > ?", filter.LastSeenID)
	}
	if filter.LiveTailWatermark > 0 {
		q = q.Where("global_received_at <= now() - ?::interval",
			fmt.Sprintf("%d milliseconds", filter.LiveTailWatermark.Milliseconds()))
	}
	q = q.Order("id ASC")
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	var rows []envelopeRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Result, len(rows))
	for i, r := range rows {
		var env slogkit.Envelope
		if err := json.Unmarshal(r.Payload, &env); err != nil {
			return nil, fmt.Errorf("unmarshal envelope id=%d: %w", r.ID, err)
		}
		out[i] = Result{ID: r.ID, Envelope: env}
	}
	return out, nil
}

// DeleteOlderThan removes rows whose ts is before the given cutoff,
// optionally filtered by source. Used by the daily TTL job.
// Returns the number of rows deleted.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time, source string) (int64, error) {
	q := s.db.WithContext(ctx).Where("ts < ?", cutoff)
	if source != "" {
		q = q.Where("source = ?", source)
	}
	res := q.Delete(&envelopeRow{})
	return res.RowsAffected, res.Error
}

func levelsAtOrAbove(min string) []string {
	min = strings.ToLower(min)
	all := []string{"debug", "info", "warn", "error"}
	for i, l := range all {
		if l == min {
			return all[i:]
		}
	}
	return nil
}
