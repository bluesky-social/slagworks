package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultReadLimit    = 1 << 20 // 1MB
	defaultPingInterval = 30 * time.Second
	defaultPongTimeout  = 10 * time.Second
)

// MaxMessageBytes is the read limit both ends of the site-worker link
// set, so it is the largest message either end may send: the hub's
// snapshot (commission data for every machine at the site) and the
// worker's batches each travel as one message. A receiver closes the
// connection with StatusMessageTooBig on anything larger, so a sender
// that batches must keep every message under this bound.
const MaxMessageBytes = 16 << 20

// Conn wraps a coder/websocket connection with JSON envelope read/write.
type Conn struct {
	conn *websocket.Conn
}

// Accept upgrades an HTTP request to a WebSocket connection (server-side).
func Accept(w http.ResponseWriter, r *http.Request, opts *websocket.AcceptOptions) (*Conn, error) {
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(defaultReadLimit)
	return &Conn{conn: c}, nil
}

// Dial opens a WebSocket connection to the given URL (client-side).
func Dial(ctx context.Context, url string, opts *websocket.DialOptions) (*Conn, error) {
	c, _, err := websocket.Dial(ctx, url, opts) //nolint:bodyclose // websocket owns the response
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(defaultReadLimit)
	return &Conn{conn: c}, nil
}

// Wrap creates a Conn from an existing websocket.Conn.
func Wrap(c *websocket.Conn) *Conn {
	return &Conn{conn: c}
}

// SetReadLimit overrides the default read limit.
func (c *Conn) SetReadLimit(n int64) {
	c.conn.SetReadLimit(n)
}

// ReadMessage reads and parses a JSON envelope from the connection.
// Must be called from a single goroutine.
func (c *Conn) ReadMessage(ctx context.Context) (Envelope, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return Envelope{}, err
	}
	return ParseEnvelope(data)
}

// WriteMessage marshals and writes a JSON envelope to the connection.
// Safe for concurrent use (coder/websocket handles locking).
func (c *Conn) WriteMessage(ctx context.Context, env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// WriteTyped marshals a typed message into an envelope and writes it.
func (c *Conn) WriteTyped(ctx context.Context, msgType string, requestID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.WriteMessage(ctx, Envelope{
		Type:      msgType,
		RequestID: requestID,
		Data:      raw,
	})
}

// Close closes the WebSocket connection with a normal closure.
func (c *Conn) Close(status websocket.StatusCode, reason string) error {
	return c.conn.Close(status, reason)
}

// CloseNow closes the connection immediately without sending a close frame.
func (c *Conn) CloseNow() error {
	return c.conn.CloseNow()
}

// Ping sends a ping and waits for pong within the given timeout.
func (c *Conn) Ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, defaultPongTimeout)
	defer cancel()
	return c.conn.Ping(pctx)
}
