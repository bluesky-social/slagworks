package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ErrConsoleCastNotFound is returned when a cast lookup finds no row
// for the given machine.
var ErrConsoleCastNotFound = errors.New("console cast not found")

// CastResumeOffset is the starting t_us a freshly-spawning site
// worker pump should use for its next emitted frame. See the design
// doc for the math.
type CastResumeOffset struct {
	CastID        int64
	CastStartedAt time.Time
	Width         int
	Height        int
	// PrevMaxUs is MAX(t_us) for this cast, plus the wall-clock gap
	// between last_event.received_at and now() (gap floored at 0 so
	// a backward NTP step on global can't drag the offset below
	// MAX(t_us)). For empty casts (no events yet) it's
	// GREATEST(0, (now() - cast_started_at).Microseconds()).
	PrevMaxUs int64
}

// EnsureConsoleCast returns the console_cast row for machineID,
// creating it on first sight. Idempotent and safe to call repeatedly:
// the underlying upsert uses ON CONFLICT DO NOTHING + a SELECT
// fallback so the conflict path doesn't generate a dead tuple every
// reconcile.
func (s *Store) EnsureConsoleCast(machineID uuid.UUID) (*model.ConsoleCast, error) {
	// Truncate to microseconds before insert: pg's TIMESTAMPTZ is
	// microsecond-precision, so a Go time.Now() with nanoseconds
	// would survive the in-memory round-trip on insert but read
	// back truncated on the SELECT fallback path. Idempotency tests
	// (and any caller comparing the two values) would see a
	// spurious mismatch on the order of nanoseconds.
	cast := model.ConsoleCast{
		MachineID:     machineID,
		CastStartedAt: time.Now().Truncate(time.Microsecond),
	}
	// gorm's clause.OnConflict with DoNothing + Returning is the
	// in-house equivalent of `ON CONFLICT DO NOTHING RETURNING`,
	// which postgres returns no rows for on conflict — gorm folds
	// that into RowsAffected=0 with no error.
	res := s.db.Clauses(onConflictDoNothing()).Create(&cast)
	if res.Error != nil {
		return nil, fmt.Errorf("create console_cast: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return &cast, nil
	}
	// Conflict path: fetch existing.
	var existing model.ConsoleCast
	if err := s.db.Where("machine_id = ?", machineID).First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrConsoleCastNotFound
		}
		return nil, fmt.Errorf("select console_cast: %w", err)
	}
	return &existing, nil
}

// CastResumeOffsetForMachine computes the prev_max_us a fresh pump
// should use for its first frame, plus enough metadata to feed the
// asciicast header on the API side. Implements the design doc's
// resume-offset math (DB-clock gap, floored at 0).
func (s *Store) CastResumeOffsetForMachine(machineID uuid.UUID) (CastResumeOffset, error) {
	var out CastResumeOffset
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var cast model.ConsoleCast
		if err := tx.Where("machine_id = ?", machineID).First(&cast).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConsoleCastNotFound
			}
			return err
		}
		out.CastID = cast.ID
		out.CastStartedAt = cast.CastStartedAt
		out.Width = cast.Width
		out.Height = cast.Height

		// Find the latest event (if any) for the gap math.
		var last model.ConsoleCastEvent
		err := tx.Where("cast_id = ?", cast.ID).
			Order("t_us DESC").
			Limit(1).
			First(&last).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT now()").Scan(&now).Error; err != nil {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// No events yet — anchor on cast_started_at.
			gap := max(now.Sub(cast.CastStartedAt), 0)
			out.PrevMaxUs = gap.Microseconds()
			return nil
		}
		gap := max(now.Sub(last.ReceivedAt), 0)
		out.PrevMaxUs = last.TUs + gap.Microseconds()
		return nil
	})
	return out, err
}

// CastEventBatch is one chunk of events landing on the relay-receive
// path. Global persists these via AppendConsoleCastEvents; the
// site-worker side never touches Postgres directly.
type CastEventBatch struct {
	CastID int64
	Events []CastEventInput
}

// CastEventInput is the per-event payload as it crosses the relay.
type CastEventInput struct {
	TUs  int64
	Data []byte
}

// AppendConsoleCastEvents bulk-inserts a batch of events under the
// given cast_id. received_at is server-default (now()).
func (s *Store) AppendConsoleCastEvents(ctx context.Context, batch CastEventBatch) error {
	if len(batch.Events) == 0 {
		return nil
	}
	rows := make([]model.ConsoleCastEvent, len(batch.Events))
	for i, e := range batch.Events {
		rows[i] = model.ConsoleCastEvent{
			CastID: batch.CastID,
			TUs:    e.TUs,
			Data:   e.Data,
		}
	}
	return s.db.WithContext(ctx).CreateInBatches(&rows, 256).Error
}

// LiveEdgeCastFloor returns COALESCE(MAX(t_us), -1) + 1 for the
// given cast. Used as the fallback floor when a since/tail window
// found no events: streaming starts at "just past the live edge"
// so the historical scan returns zero rows (excluding the latest
// event which is outside the requested window) and the stream
// cleanly transitions into waiting for new live frames.
func (s *Store) LiveEdgeCastFloor(ctx context.Context, castID int64) (int64, error) {
	var floor sql.NullInt64
	err := s.db.WithContext(ctx).
		Raw("SELECT COALESCE(MAX(t_us), -1) + 1 FROM console_cast_events WHERE cast_id = ?", castID).
		Scan(&floor).Error
	if err != nil {
		return 0, err
	}
	if !floor.Valid {
		// COALESCE always returns a value, so this is unreachable
		// in practice — but if Scan ever produces NULL we treat it
		// as 0 (cast row with zero events).
		return 0, nil
	}
	return floor.Int64, nil
}

// SinceFloorByDuration resolves a since={duration} or tail={duration}
// query into a concrete t_us floor. Returns the t_us of the earliest
// event whose received_at falls inside the window. If the window has
// no events (machine silent for longer than duration), returns
// LiveEdgeCastFloor so the stream tails new events instead of
// quietly dumping the whole history.
//
// Uses ORDER BY received_at ASC, t_us ASC LIMIT 1 (rides the
// (cast_id, received_at) index in O(1); the t_us tie-break ensures
// a batch that shares received_at doesn't drop its earliest event
// silently).
func (s *Store) SinceFloorByDuration(ctx context.Context, castID int64, d time.Duration) (int64, error) {
	if d < 0 {
		d = 0
	}
	// Build the interval as a string — pgx's prepared-statement type
	// inference picks `text` for an int64 here and refuses, so we
	// format the value into the SQL literal directly. d.Microseconds()
	// is bounded by Go's time.Duration (max ~292 years in us) so the
	// value is safe to format inline.
	intervalArg := fmt.Sprintf("%d microseconds", d.Microseconds())
	var floor sql.NullInt64
	err := s.db.WithContext(ctx).
		Raw(`SELECT t_us FROM console_cast_events
		     WHERE cast_id = ? AND received_at >= now() - ?::interval
		     ORDER BY received_at ASC, t_us ASC
		     LIMIT 1`, castID, intervalArg).
		Scan(&floor).Error
	if err != nil {
		return 0, err
	}
	if floor.Valid {
		return floor.Int64, nil
	}
	// No events in window — fall back to live edge.
	return s.LiveEdgeCastFloor(ctx, castID)
}

// ScanCastEventsForNonce searches recent events under the given
// machine's cast for `nonce` as a literal byte substring (no byte is a
// wildcard). Returns true on first match. Used by the console probe
// verification flow and the commission console sweep — the agent
// emits a marker `SLAG-CONSOLE-PROBE-<machine>-<nonce>` to a tty
// during commission, those bytes ride the BMC SOL session up into
// console_cast_events, and the caller asks "did our nonce arrive?"
// within `lookback`.
//
// An empty nonce names no marker and returns false, nil. Returns
// ErrConsoleCastNotFound when no cast row exists for the machine yet
// (no SOL bytes ever observed). Returns false, nil when the cast
// exists but no event in the window matches.
func (s *Store) ScanCastEventsForNonce(ctx context.Context, machineID uuid.UUID, nonce string, lookback time.Duration) (bool, error) {
	if nonce == "" {
		return false, nil
	}
	if lookback < 0 {
		lookback = 0
	}
	var cast model.ConsoleCast
	if err := s.db.WithContext(ctx).Where("machine_id = ?", machineID).First(&cast).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrConsoleCastNotFound
		}
		return false, fmt.Errorf("select cast: %w", err)
	}
	// Search server-side so we don't have to ship the entire
	// recent-event window over the wire just to grep it. The
	// (cast_id, received_at) index handles the time predicate; the
	// search runs only on rows the index already filtered down.
	var hit bool
	intervalArg := fmt.Sprintf("%d microseconds", lookback.Microseconds())
	// position() rather than LIKE: the nonce reaches here from
	// agent-written reports, and LIKE would treat '%' and '_' in it as
	// wildcards that match any console output. `data` is bytea, so the
	// needle must be bytea too; it is bound hex-encoded and decoded
	// server-side because gorm expands a slice placeholder that directly
	// follows '(' (its `IN (?)` form), which would split a []byte into
	// one parameter per byte.
	//
	// Aggregate before searching: the SOL stream is sliced into rows by
	// the pump's 100ms / 4KB flush timer, so a marker crossing a flush
	// boundary lands in two adjacent rows. A row-by-row search would
	// miss it. string_agg(data, ''::bytea) reassembles the window so
	// the search sees the contiguous byte stream.
	err := s.db.WithContext(ctx).
		Raw(`SELECT EXISTS (
		         SELECT 1 FROM (
		             SELECT string_agg(data, ''::bytea
		                               ORDER BY t_us, id) AS full_data
		             FROM console_cast_events
		             WHERE cast_id = ? AND received_at >= now() - ?::interval
		         ) sub WHERE position(decode(?, 'hex') IN full_data) > 0
		     )`, cast.ID, intervalArg, hex.EncodeToString([]byte(nonce))).
		Scan(&hit).Error
	if err != nil {
		return false, fmt.Errorf("scan events: %w", err)
	}
	return hit, nil
}

// CastEventsSince streams all events from `castID` whose t_us is
// >= floor, ordered by t_us asc, capped at `limit`. Used by the API
// stream endpoint to drain backlog before subscribing to live frames.
// Pass limit=0 for "all events."
func (s *Store) CastEventsSince(ctx context.Context, castID int64, floorTUs int64, limit int) ([]model.ConsoleCastEvent, error) {
	q := s.db.WithContext(ctx).
		Where("cast_id = ? AND t_us >= ?", castID, floorTUs).
		Order("t_us ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []model.ConsoleCastEvent
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// onConflictDoNothing returns the gorm clause for ON CONFLICT
// DO NOTHING, mirroring the explicit SQL idiom from the design doc.
func onConflictDoNothing() clause.Expression {
	return clause.OnConflict{DoNothing: true}
}

// CastStorageStat is per-machine console-cast storage accounting.
type CastStorageStat struct {
	MachineID uuid.UUID
	Bytes     int64
}

// CastStorageStats returns the stored SOL byte volume per machine
// (sum of event data sizes, largest first). Cast events are kept
// forever as an audit record (2026-07-03 operator ruling), so this is
// the observability hook that catches a runaway console — a machine
// spewing kernel logs onto its serial port accrues here with no other
// signal until the DB fills.
func (s *Store) CastStorageStats(ctx context.Context) ([]CastStorageStat, error) {
	var out []CastStorageStat
	err := s.db.WithContext(ctx).
		Raw(`SELECT c.machine_id, COALESCE(SUM(octet_length(e.data)), 0) AS bytes
		     FROM console_casts c
		     LEFT JOIN console_cast_events e ON e.cast_id = c.id
		     GROUP BY c.machine_id
		     ORDER BY bytes DESC`).
		Scan(&out).Error
	return out, err
}
