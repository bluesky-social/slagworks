package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/bluesky-social/slagworks/internal/ws"
)

// WatchEntry is a single log entry received from the stream, tagged with when
// the client received it (not the server-side timestamp).
type WatchEntry struct {
	ReceivedAt time.Time
	Source     string
	TS         string // server-side RFC3339 timestamp
	Line       string
}

// WatchStream connects to the global WS, subscribes for a machine/op, and
// calls onEntry for each log entry as it arrives. Blocks until a terminal
// phase (completed or failed) is received, or ctx is cancelled.
// Returns nil on "completed", non-nil error on failure or connection error.
func WatchStream(ctx context.Context, apiURL, token, machineID, opID string, onEntry func(WatchEntry)) error {
	es, err := ConnectWithRetry(ctx, apiURL, token, machineID, opID, 0)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()

	for {
		env, err := es.ReadEvent(ctx)
		if err != nil {
			return err
		}
		if env.Type != ws.TypeLogEntries {
			continue
		}
		var msg ws.LogEntriesMsg
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			continue
		}
		now := time.Now()
		for _, e := range msg.Entries {
			onEntry(WatchEntry{ReceivedAt: now, Source: e.Source, TS: e.TS, Line: e.Line})
			if e.Source == "phase" {
				if e.Line == "completed" {
					return nil
				}
				if e.Line == "failed" || strings.HasPrefix(e.Line, "failed:") {
					return fmt.Errorf("deploy failed: %s", e.Line)
				}
			}
		}
	}
}

// streamDialOptions carries the operator token on a WebSocket upgrade in
// the Authorization header, the same way the REST client sends it. The
// token never goes in the URL: URLs end up in access logs.
func streamDialOptions(token string) *websocket.DialOptions {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return &websocket.DialOptions{HTTPHeader: h}
}

// EventStream connects to the global WebSocket and streams events.
type EventStream struct {
	conn      *ws.Conn
	machineID string
	opID      string
	cursor    int64
}

// NewEventStream connects to the global WS events endpoint and sends a subscribe message.
func NewEventStream(ctx context.Context, apiURL, token, machineID, opID string, cursor int64) (*EventStream, error) {
	wsURL := buildEventsURL(apiURL)
	conn, err := ws.Dial(ctx, wsURL, streamDialOptions(token))
	if err != nil {
		return nil, fmt.Errorf("connect to WebSocket: %w", err)
	}

	es := &EventStream{
		conn:      conn,
		machineID: machineID,
		opID:      opID,
		cursor:    cursor,
	}

	sub := ws.SubscribeMsg{
		MachineID:   machineID,
		OperationID: opID,
		Cursor:      cursor,
	}
	if err := conn.WriteTyped(ctx, ws.TypeSubscribe, "", sub); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("send subscribe: %w", err)
	}

	return es, nil
}

// ReadEvent reads the next event from the WebSocket.
func (s *EventStream) ReadEvent(ctx context.Context) (ws.Envelope, error) {
	env, err := s.conn.ReadMessage(ctx)
	if err != nil {
		return ws.Envelope{}, err
	}

	// Track cursor for reconnect.
	if env.Type == ws.TypeLogEntries {
		var msg struct {
			Cursor int64 `json:"cursor"`
		}
		if err := json.Unmarshal(env.Data, &msg); err == nil && msg.Cursor > s.cursor {
			s.cursor = msg.Cursor
		}
	}

	return env, nil
}

// Close closes the WebSocket connection.
func (s *EventStream) Close() error {
	return s.conn.CloseNow()
}

// ConnectWithRetry connects with exponential backoff.
func ConnectWithRetry(ctx context.Context, apiURL, token, machineID, opID string, cursor int64) (*EventStream, error) {
	backoff := time.Second
	start := time.Now()
	var lastErr error
	// On ctx expiry, surface the last connect error — a bare "context
	// deadline exceeded" hides what was actually failing for the whole
	// retry window. The ctx error stays in the chain (second %w) so
	// errors.Is(err, context.DeadlineExceeded) still works for callers.
	giveUp := func() error {
		if lastErr != nil {
			return fmt.Errorf("could not connect to the event stream after retrying for %s: %w (%w)",
				time.Since(start).Round(time.Second), lastErr, ctx.Err())
		}
		return ctx.Err()
	}
	for {
		es, err := NewEventStream(ctx, apiURL, token, machineID, opID, cursor)
		if err == nil {
			return es, nil
		}
		// Don't let a context-flavored dial error (ctx expired mid-dial)
		// clobber the real network error from an earlier attempt.
		if lastErr == nil || ctx.Err() == nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			return nil, giveUp()
		}
		select {
		case <-ctx.Done():
			return nil, giveUp()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func buildEventsURL(apiURL string) string {
	base := apiURL
	if len(base) > 4 && base[:4] == "http" {
		base = "ws" + base[4:]
	}
	return base + "/api/v1/ws/events"
}
