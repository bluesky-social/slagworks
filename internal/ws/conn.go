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

// MaxWorkerFrameBytes is the largest frame the hub reads from a site
// worker. A longer frame fails the hub's read and closes the connection
// before the hub sees its request_id, so it cannot be answered, only
// dropped by the worker: the worker's outbox drain checks every
// observation frame against this limit and never sends a longer one.
const MaxWorkerFrameBytes = 1 << 20

// MaxObservationDataBytes caps a request body that the site worker queues
// for global as an observation's data (/api/v1/inventory and
// /api/v1/report), measured as json.Marshal re-encodes it. It is 4 KiB
// under MaxWorkerFrameBytes to leave room for the observation envelope
// around the body, so a body the worker accepts is one it can send.
const MaxObservationDataBytes = MaxWorkerFrameBytes - 4<<10

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
	frame, err := EncodeTyped(msgType, requestID, payload)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, frame)
}

// EncodeTyped marshals a typed message into the envelope bytes WriteTyped
// sends. A caller that must know a frame's size before sending it encodes
// with EncodeTyped and sends with WriteFrame.
func EncodeTyped(msgType string, requestID string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	frame, err := json.Marshal(Envelope{
		Type:      msgType,
		RequestID: requestID,
		Data:      raw,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return frame, nil
}

// WriteFrame writes envelope bytes produced by EncodeTyped.
func (c *Conn) WriteFrame(ctx context.Context, frame []byte) error {
	return c.conn.Write(ctx, websocket.MessageText, frame)
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
