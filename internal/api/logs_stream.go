package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// LogStreamStore is the read surface logsStream needs from logstore.
type LogStreamStore interface {
	Query(ctx context.Context, filter logstore.QueryFilter) ([]logstore.Result, error)
}

// liveTailWatermark is the lag every stream query applies past
// global_received_at to defend against pg sequence-id-allocated-pre-
// commit reordering: a transaction can hold a lower id while a later
// writer commits a higher one, and a query that returned the higher id
// would move the cursor past the lower id for good. Backfill pages need
// it as much as live polls, since a backfill page also reads up to the
// newest committed row. global_received_at is the writer's transaction
// start time, so a transaction held open longer than the watermark can
// still be overtaken.
const liveTailWatermark = 500 * time.Millisecond

// pollInterval bounds how often live-tail polls pg between hits.
// Operator-facing latency target is "feels live"; 250ms is well
// inside human reaction time.
const pollInterval = 250 * time.Millisecond

// acceptOperatorStream upgrades an operator stream request that has
// already passed oidcAuth. Origin rule: an upgrade with no Origin header
// (slagctl and other non-browser clients) is accepted; a browser upgrade
// is accepted only when its Origin host equals the request's Host, so a
// page on another origin cannot open a stream. This is coder/websocket's
// default check (403 otherwise); never set InsecureSkipVerify or a
// wildcard OriginPatterns here.
func acceptOperatorStream(c *echo.Context) (*websocket.Conn, error) {
	return websocket.Accept(c.Response(), c.Request(), &websocket.AcceptOptions{})
}

// operatorStreamKeepalive is how often the logs and console streams ping
// their client. A stream with nothing new to send writes nothing else, and
// a proxy or load balancer in front of global closes a connection that
// carries no bytes for its idle timeout (60s by default for an AWS ALB and
// for nginx's proxy_read_timeout). docs/user/installation.md states the
// resulting minimum idle timeout.
const operatorStreamKeepalive = 30 * time.Second

// keepOperatorStreamAlive pings conn every s.streamKeepalive until ctx
// ends. ctx must come from conn.CloseRead: its reader goroutine is what
// receives the client's pongs, and it ends ctx when the connection closes,
// so this goroutine stops with the handler's deferred CloseNow.
//
// An unanswered ping is not an error. A client stops reading, and so stops
// answering, whenever its output backs up (`slagctl machine logs -f |
// less` while the operator pages, a terminal paused with ^S); it is still
// connected, and the stream already ends when the client closes or drops
// the connection (CloseRead) or a write fails. Waiting for the pong only
// bounds each ping. coder/websocket does close the connection when a ping
// frame it has started to write is still blocked five seconds after the
// ping began, which takes a client that has stopped reading for long
// enough to fill the socket buffers between it and the server.
func (s *Server) keepOperatorStreamAlive(ctx context.Context, conn *websocket.Conn) {
	interval := s.streamKeepalive
	if interval <= 0 {
		interval = operatorStreamKeepalive
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, interval)
		_ = conn.Ping(pingCtx)
		cancel()
	}
}

// handleLogsStream serves /api/v1/logs/stream as a WebSocket. The
// client sends one ws.LogStreamSubscribeMsg, the server replies with
// backfill (filtered by the subscribe message) and then live-tails
// new envelopes. Cursor is the log_envelopes.id BIGSERIAL; clients
// resume past disconnect with last_seen_id. Every query, backfill
// included, holds back rows received in the last liveTailWatermark, so
// those arrive on a later poll rather than in the first page.
//
// The filter is built once and every query uses it unchanged apart from
// the cursor. The cursor moves past every id a query did not return, so
// a row is delivered only if the first query to reach its id admits it:
// a filter that admitted more rows later in the stream (as the since
// bound once did, ts-only until the first empty page) would lose the
// rows it had already stepped over.
func (s *Server) handleLogsStream(c *echo.Context) error {
	if s.logStore == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "log stream not available")
	}
	conn, err := acceptOperatorStream(c)
	if err != nil {
		return nil // Accept already wrote the error response
	}
	defer conn.CloseNow() //nolint:errcheck

	ctx := c.Request().Context()

	// First message must be subscribe.
	subscribeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, raw, err := conn.Read(subscribeCtx)
	if err != nil {
		return nil
	}
	// The subscribe is the only message a client sends. CloseRead drains
	// the connection from here on and cancels ctx when the client closes
	// or drops it. Nothing else would: the request context of a hijacked
	// connection outlives the client, and a poll loop whose filter
	// matches no new rows never writes, so it would never see the closed
	// connection and would query the store every pollInterval for the
	// life of the process.
	ctx = conn.CloseRead(ctx)
	go s.keepOperatorStreamAlive(ctx, conn)
	var env ws.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		writeErr(ctx, conn, "protocol_error", "malformed subscribe envelope: "+err.Error())
		return nil
	}
	if env.Type != ws.TypeLogStreamSubscribe {
		writeErr(ctx, conn, "protocol_error",
			"expected first message type "+ws.TypeLogStreamSubscribe+", got "+env.Type)
		return nil
	}
	var sub ws.LogStreamSubscribeMsg
	if err := json.Unmarshal(env.Data, &sub); err != nil {
		writeErr(ctx, conn, "protocol_error", "malformed subscribe data: "+err.Error())
		return nil
	}

	filter, err := buildFilter(sub)
	if err != nil {
		writeErr(ctx, conn, "bad_filter", err.Error())
		return nil
	}

	if err := writeMessage(ctx, conn, ws.TypeSubscribed, ws.SubscribedMsg{}); err != nil {
		return nil
	}

	cursor := sub.LastSeenID
	pageSize := 500
	filter.LiveTailWatermark = liveTailWatermark

	for {
		if ctx.Err() != nil {
			return nil
		}
		filter.LastSeenID = cursor
		filter.Limit = pageSize

		results, err := s.logStore.Query(ctx, filter)
		if err != nil {
			writeErr(ctx, conn, "query_error", err.Error())
			return nil
		}
		if len(results) == 0 {
			// The backlog is consumed: sleep before the next poll.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pollInterval):
				continue
			}
		}
		for _, r := range results {
			if err := writeMessage(ctx, conn, ws.TypeLogStreamEnvelope, ws.LogStreamEnvelopeMsg{
				ID:       r.ID,
				Envelope: r.Envelope,
			}); err != nil {
				return nil
			}
			cursor = r.ID
		}
	}
}

func buildFilter(sub ws.LogStreamSubscribeMsg) (logstore.QueryFilter, error) {
	f := logstore.QueryFilter{
		MachineName: sub.MachineName,
		Sources:     normalizeSources(sub.Sources),
		MinLevel:    strings.ToLower(sub.MinLevel),
		LastSeenID:  sub.LastSeenID,
	}
	if err := logstore.ValidateMinLevel(sub.MinLevel); err != nil {
		return f, err
	}
	if sub.TraceID != "" {
		id, err := uuid.Parse(sub.TraceID)
		if err != nil {
			return f, fmt.Errorf("invalid trace_id: %w", err)
		}
		f.TraceID = id
	}
	if sub.MachineID != "" {
		id, err := uuid.Parse(sub.MachineID)
		if err != nil {
			return f, fmt.Errorf("invalid machine_id: %w", err)
		}
		f.MachineID = id
	}
	if sub.OpID != "" {
		id, err := uuid.Parse(sub.OpID)
		if err != nil {
			return f, fmt.Errorf("invalid op_id: %w", err)
		}
		f.OpID = id
	}
	if sub.SinceTimestamp != "" {
		// Accept RFC3339 / RFC3339Nano. Anything else is rejected up
		// front so the operator sees the error rather than a silent
		// empty stream.
		t, err := time.Parse(time.RFC3339Nano, sub.SinceTimestamp)
		if err != nil {
			t, err = time.Parse(time.RFC3339, sub.SinceTimestamp)
			if err != nil {
				return f, fmt.Errorf("invalid since timestamp: %w", err)
			}
		}
		f.Since = t
	}
	return f, nil
}

func normalizeSources(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	allowed := map[string]struct{}{
		string(slogkit.SourceAgent):  {},
		string(slogkit.SourceSite):   {},
		string(slogkit.SourceGlobal): {},
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if _, ok := allowed[s]; !ok {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func writeMessage(ctx context.Context, conn *websocket.Conn, msgType string, data any) error {
	buf, err := ws.MarshalEnvelope(msgType, "", data)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, buf)
}

func writeErr(ctx context.Context, conn *websocket.Conn, code, msg string) {
	_ = writeMessage(ctx, conn, ws.TypeError, ws.ErrorMsg{Code: code, Message: msg})
}

// ErrLogStreamUnavailable is returned by callers/tests when the log
// stream endpoint hasn't been wired (LogStore == nil on the api.Config).
var ErrLogStreamUnavailable = errors.New("log stream not available")
