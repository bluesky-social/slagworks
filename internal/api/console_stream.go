package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
)

// ConsoleStreamStore is the read+write surface the console-stream
// endpoint needs from store.Store. The endpoint is read-only on the
// API surface, but it does ensure the cast row exists on first
// connect for a known machine (so a client that hits us before the
// site worker has opened a pump still gets a real header back rather
// than a 404). An id that names no machine gets a 404 before any write.
type ConsoleStreamStore interface {
	MachineExists(machineID uuid.UUID) (bool, error)
	EnsureConsoleCast(machineID uuid.UUID) (*model.ConsoleCast, error)
	CastEventsSince(ctx context.Context, castID int64, floorTUs int64, limit int) ([]model.ConsoleCastEvent, error)
	SinceFloorByDuration(ctx context.Context, castID int64, d time.Duration) (int64, error)
	LiveEdgeCastFloor(ctx context.Context, castID int64) (int64, error)
	// Console probe verification: scan recent events for a
	// nonce. Returns ErrConsoleCastNotFound when no cast exists yet.
	ScanCastEventsForNonce(ctx context.Context, machineID uuid.UUID, nonce string, lookback time.Duration) (bool, error)
}

// idleTimeLimit is what we put in the asciicast v2 header so any
// standard player consuming our stream doesn't hang on long-silent
// prefixes (machines that haven't emitted bytes for hours/days).
// 2 seconds matches the design doc; gaps > 2s render as 2s in
// playback. Our own slagctl console viewer ignores t deltas
// entirely (ModeLive) so the cap is for external-player UX.
const idleTimeLimit = 2.0

// consolePollInterval bounds how often the live-tail loop polls pg
// when caught up. Same shape as logs_stream's poll. 250ms feels
// live to a human.
const consolePollInterval = 250 * time.Millisecond

// handleConsoleStream serves /api/v1/console/stream as a WebSocket.
// Query params (mutually exclusive for since selection):
//
//	machine={uuid}     required
//	cast_id={int}      optional, currently informational only (one cast per machine)
//	since_us={int}     start at first event with t_us >= since_us
//	since={duration}   wall-clock duration relative to now() (e.g. "10m")
//	tail={duration}    same as since; positive convenience alias
//	follow=true        keep streaming live frames after backfill
//
// Output frames are asciicast v2 lines: a header text frame first,
// then [t,"o","data"] event lines as text frames (one per cast event
// row).
func (s *Server) handleConsoleStream(c *echo.Context) error {
	if s.consoleStore == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "console stream not available")
	}

	machineParam := c.QueryParam("machine")
	if machineParam == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing required query param: machine")
	}
	machineID, err := uuid.Parse(machineParam)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid machine: "+err.Error())
	}

	exists, err := s.consoleStore.MachineExists(machineID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "look up machine: "+err.Error())
	}
	if !exists {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no machine with id %s; pass a machine UUID from `slagctl machine list`", machineID))
	}

	cast, err := s.consoleStore.EnsureConsoleCast(machineID)
	if err != nil {
		// Most plausible cause is a transient pg blip; the operator
		// will retry. Surface the error rather than silently
		// degrading to an empty stream.
		return echo.NewHTTPError(http.StatusInternalServerError, "ensure cast: "+err.Error())
	}

	// Resolve start floor BEFORE upgrading: any error here is a
	// 4xx/5xx and should come back as an HTTP response, not a WS
	// close frame. Once we upgrade, all errors must travel as WS
	// frames or close codes.
	follow := c.QueryParam("follow") == "true" || c.QueryParam("follow") == "1"
	// snapshot mode: instead of replaying the whole ring buffer as
	// per-event frames (which floods a client emulator), the
	// server replays the cast through its own throwaway vt emulator and
	// emits ONE reconstructed-screen frame, then tails live from the live
	// edge. This is what the watch TUI uses on attach. It is mutually
	// exclusive with since/tail/since_us (those select a replay floor;
	// snapshot always reflects the full history up to the live edge), so
	// reject the combination rather than silently ignoring one.
	snapshot := c.QueryParam("snapshot") == "true" || c.QueryParam("snapshot") == "1"
	if snapshot {
		if c.QueryParam("since_us") != "" || c.QueryParam("since") != "" || c.QueryParam("tail") != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "snapshot cannot be combined with since_us/since/tail")
		}
	}
	floorTUs, err := s.resolveStartFloor(c.Request().Context(), cast.ID, c, follow)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	conn, err := acceptOperatorStream(c)
	if err != nil {
		return nil
	}
	defer conn.CloseNow() //nolint:errcheck

	// CloseRead spawns a goroutine that drains the connection's
	// incoming frames and surfaces a client-side close as ctx.Done().
	// Without this, a client that disconnects gracefully (close frame
	// over the WS) would leave us blocked on the next Write, leaking
	// the handler goroutine + connection. coder/websocket's contract
	// is "you must read from the conn even if you only write to it";
	// this is the documented one-shot API for doing exactly that on
	// a write-only stream.
	ctx := conn.CloseRead(c.Request().Context())
	go s.keepOperatorStreamAlive(ctx, conn)

	// Send the asciicast v2 header.
	header := map[string]any{
		"version":         2,
		"width":           cast.Width,
		"height":          cast.Height,
		"timestamp":       cast.CastStartedAt.Unix(),
		"idle_time_limit": idleTimeLimit,
		"env":             map[string]string{"TERM": "xterm-256color"},
	}
	headerBuf, err := json.Marshal(header)
	if err != nil {
		writeConsoleErr(ctx, conn, "internal", "marshal header: "+err.Error())
		return nil
	}
	if err := conn.Write(ctx, websocket.MessageText, headerBuf); err != nil {
		return nil
	}

	cursor := floorTUs
	const pageSize = 500

	// Snapshot mode: drain the WHOLE cast from t=0 into a
	// throwaway server-side emulator, emit ONE reconstructed-screen frame,
	// and advance the cursor to the live edge so the follow loop below
	// continues from there. This replaces sending the full ring buffer as
	// per-event frames (which freezes the client emulator). Faithful only
	// while nothing trims the cast.
	if snapshot {
		frame, drainCursor, err := s.castSnapshot(ctx, cast, pageSize)
		if err != nil {
			if ctx.Err() == nil {
				writeConsoleErr(ctx, conn, "query_error", err.Error())
			}
			return nil
		}
		if len(frame) > 0 {
			line, err := encodeSnapshotLine(frame)
			if err != nil {
				writeConsoleErr(ctx, conn, "encode_error", err.Error())
				return nil
			}
			if err := conn.Write(ctx, websocket.MessageText, line); err != nil {
				return nil
			}
		}
		// Continue from the live edge: never re-emit the events we just
		// folded into the snapshot.
		cursor = drainCursor
		if !follow {
			return nil
		}
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		rows, err := s.consoleStore.CastEventsSince(ctx, cast.ID, cursor, pageSize)
		if err != nil {
			writeConsoleErr(ctx, conn, "query_error", err.Error())
			return nil
		}
		for _, r := range rows {
			line, err := encodeCastEventLine(r, cast.CastStartedAt)
			if err != nil {
				writeConsoleErr(ctx, conn, "encode_error", err.Error())
				return nil
			}
			if err := conn.Write(ctx, websocket.MessageText, line); err != nil {
				return nil
			}
			// Cursor advances past the row: t_us+1 so the next page
			// query doesn't re-emit this row. Per-cast t_us is
			// monotonic so the +1 is safe.
			cursor = r.TUs + 1
		}
		// Caught-up gate. If we got a full page back, more rows are
		// almost certainly waiting — loop immediately to drain
		// backlog at full speed. If we got less than a page, we're
		// at the live edge: pause before polling again so a slow
		// trickle of bytes from the BMC doesn't translate into a
		// pg-query-per-loop-iteration tight loop. The same gate
		// drives the !follow exit: if we're caught up and not
		// following, we're done.
		if len(rows) < pageSize {
			if !follow {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(consolePollInterval):
			}
		}
	}
}

// resolveStartFloor turns the since_us / since / tail / (none) query
// params into a concrete t_us starting point per the design doc's
// rules. Mutually exclusive — if multiple are set we pick in this
// priority order: since_us (explicit), since (duration), tail
// (alias). With none of them: floor=0 for non-follow (replay from
// beginning), live-edge for follow (don't backfill on a tail-style
// request).
func (s *Server) resolveStartFloor(ctx context.Context, castID int64, c *echo.Context, follow bool) (int64, error) {
	if v := c.QueryParam("since_us"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid since_us: %w", err)
		}
		if n < 0 {
			n = 0
		}
		return n, nil
	}
	if v := c.QueryParam("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("invalid since: %w", err)
		}
		return s.consoleStore.SinceFloorByDuration(ctx, castID, d)
	}
	if v := c.QueryParam("tail"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("invalid tail: %w", err)
		}
		return s.consoleStore.SinceFloorByDuration(ctx, castID, d)
	}
	if follow {
		return s.consoleStore.LiveEdgeCastFloor(ctx, castID)
	}
	return 0, nil
}

// encodeCastEventLine renders one console_cast_event row as an
// asciicast v2 output line: [t_seconds, "o", data]. data is the raw
// event bytes, scrubbed for invalid UTF-8 (any non-UTF-8 sequence
// becomes U+FFFD) so the resulting JSON string is valid even when
// the BMC emits binary noise.
func encodeCastEventLine(r model.ConsoleCastEvent, _ time.Time) ([]byte, error) {
	t := float64(r.TUs) / 1_000_000.0
	data := scrubUTF8(r.Data)
	arr := [3]any{round6(t), "o", data}
	buf, err := json.Marshal(arr)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// encodeSnapshotLine wraps the reconstructed-screen ANSI as a single asciicast
// v2 output event [0,"o",data] so the client decodes it through the exact same
// [t,"o",data] path as live cast events — no wire-format change. t=0 because a
// snapshot has no meaningful timestamp (the client's live monitor ignores t
// deltas anyway). The ANSI is generated by us (clear + cursor-addressed line
// writes), so it's always valid UTF-8; scrubUTF8 is belt-and-suspenders.
func encodeSnapshotLine(ansiBytes []byte) ([]byte, error) {
	arr := [3]any{0, "o", scrubUTF8(ansiBytes)}
	return json.Marshal(arr)
}

// scrubUTF8 returns a string that is guaranteed valid UTF-8.
// Non-UTF-8 byte sequences are replaced with U+FFFD. The asciicast
// v2 spec requires data be a JSON string (and therefore UTF-8); we
// store raw BYTEA in pg and scrub on the way out so ASCII (the
// 99.9% case) round-trips byte-for-byte.
func scrubUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		out = append(out, r)
		b = b[size:]
	}
	return string(out)
}

// round6 keeps t-field precision at microsecond level — beyond that
// is meaningless given the 100ms/4KB flush rule and just bloats the
// JSON.
func round6(f float64) float64 {
	return float64(int64(f*1_000_000)) / 1_000_000
}

func writeConsoleErr(ctx context.Context, conn *websocket.Conn, code, msg string) {
	type errMsg struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	buf, _ := json.Marshal(errMsg{Code: code, Message: msg})
	_ = conn.Write(ctx, websocket.MessageText, buf)
}

// NewConsoleStoreAdapter wraps *store.Store into the ConsoleStreamStore
// interface so the server config can take *store.Store directly
// without expanding api.Config's surface.
func NewConsoleStoreAdapter(s *store.Store) ConsoleStreamStore {
	return consoleStoreAdapter{S: s}
}

type consoleStoreAdapter struct{ S *store.Store }

func (a consoleStoreAdapter) MachineExists(machineID uuid.UUID) (bool, error) {
	_, err := a.S.GetMachine(machineID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (a consoleStoreAdapter) EnsureConsoleCast(machineID uuid.UUID) (*model.ConsoleCast, error) {
	return a.S.EnsureConsoleCast(machineID)
}

func (a consoleStoreAdapter) CastEventsSince(ctx context.Context, castID int64, floorTUs int64, limit int) ([]model.ConsoleCastEvent, error) {
	return a.S.CastEventsSince(ctx, castID, floorTUs, limit)
}

func (a consoleStoreAdapter) ScanCastEventsForNonce(ctx context.Context, machineID uuid.UUID, nonce string, lookback time.Duration) (bool, error) {
	return a.S.ScanCastEventsForNonce(ctx, machineID, nonce, lookback)
}

func (a consoleStoreAdapter) SinceFloorByDuration(ctx context.Context, castID int64, d time.Duration) (int64, error) {
	return a.S.SinceFloorByDuration(ctx, castID, d)
}

func (a consoleStoreAdapter) LiveEdgeCastFloor(ctx context.Context, castID int64) (int64, error) {
	return a.S.LiveEdgeCastFloor(ctx, castID)
}

// ErrConsoleStreamUnavailable is returned by callers/tests when the
// console-stream endpoint hasn't been wired (ConsoleStore == nil on
// api.Config).
var ErrConsoleStreamUnavailable = errors.New("console stream not available")
