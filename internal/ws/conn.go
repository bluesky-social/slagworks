package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const defaultReadLimit = 1 << 20 // 1MB

// HeartbeatInterval is how often each end of the site-worker link sends
// the other a heartbeat message. The worker's refreshes
// site_workers.last_seen_at; global's gives an idle site something to
// hear. Each end reads with IdleTimeout, so the heartbeats are what let it
// tell a quiet link from a dead one: a path that stops delivering without
// a FIN or RST (a load balancer or NAT that lost the flow, a wedged proxy)
// otherwise looks open until the kernel gives up retransmitting, about 15
// minutes on Linux defaults, or never when something in the path keeps
// acknowledging.
//
// These are heartbeat messages rather than WebSocket pings because
// coder/websocket answers a ping from inside the receiver's read, with 5
// seconds to get the pong onto the connection, and fails that read
// otherwise. A pong waits behind a message the receiver is still writing,
// so a ping arriving while a worker uploads a 10 MiB observation over a
// slow link would end the connection, and the replayed observation would
// meet the next ping the same way.
const HeartbeatInterval = 30 * time.Second

// IdleTimeout is how long either end of the site-worker link waits for
// bytes from the other before it drops the connection as dead: three
// heartbeat intervals, so one late heartbeat or a retransmission stall on
// a lossy path does not cost a reconnect (which loses the op_updates
// written just before it).
const IdleTimeout = 3 * HeartbeatInterval

// ErrPeerSilent is the error ReadMessageIdle wraps when nothing arrived
// from the other end within its idle timeout.
var ErrPeerSilent = errors.New("nothing received from the other end within the idle timeout; treating the connection as dead")

// idleReadChunk caps one read of a message's payload in ReadMessageIdle,
// so its idle timer restarts at least every idleReadChunk bytes however
// large the message is.
const idleReadChunk = 32 << 10

// MaxMessageBytes is the read limit both ends of the site-worker link
// set, so it is the largest message either end may send: the hub's
// snapshot (commission data for every machine at the site), each of the
// worker's outbox observations and each of its log batches travel as one
// message. A receiver closes the connection with StatusMessageTooBig on
// anything larger, without reading or answering it, so a sender must
// keep every message under this bound.
const MaxMessageBytes = 16 << 20

// MaxObservationDataBytes caps a request body that the site worker queues
// for global as an observation's data (/api/v1/inventory and
// /api/v1/report), measured as json.Marshal re-encodes it. It is the cap
// /api/v1/inventory already put on its raw body, and it leaves the rest of
// MaxMessageBytes for the observation message around the data, so a body
// the worker accepts is one it can send.
const MaxObservationDataBytes = 10 << 20

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

// Dial opens a WebSocket connection to the given URL (client-side). When
// the server refuses the upgrade, the error carries the reason it gave
// (see DialError).
func Dial(ctx context.Context, url string, opts *websocket.DialOptions) (*Conn, error) {
	c, resp, err := websocket.Dial(ctx, url, opts) //nolint:bodyclose // websocket owns the response
	if err != nil {
		return nil, DialError(err, resp)
	}
	c.SetReadLimit(defaultReadLimit)
	return &Conn{conn: c}, nil
}

// maxRefusalReason bounds how much of a refused upgrade's body DialError
// keeps. Global's refusals are one sentence; a proxy's error page is not,
// and a reconnecting client logs the error on every attempt.
const maxRefusalReason = 512

// DialError returns err, the error websocket.Dial returned with resp,
// with the reason the server gave for refusing the upgrade. A server
// answers a refused upgrade with a status and a body that says why (global
// writes "missing worker token: ..." or {"message":"invalid token"}), but
// websocket.Dial's error names only the status. The body is appended on
// one line, its whitespace collapsed, cut at maxRefusalReason bytes. err
// is returned unchanged when there is no response (the request got no
// answer) or the body is empty.
func DialError(err error, resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return err
	}
	// On a failed handshake websocket.Dial leaves the body's first
	// kilobyte readable.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*maxRefusalReason))
	reason := strings.Join(strings.Fields(string(body)), " ")
	if len(reason) > maxRefusalReason {
		reason = strings.ToValidUTF8(reason[:maxRefusalReason], "")
	}
	if reason == "" {
		return err
	}
	return fmt.Errorf("%w; the server said: %s", err, reason)
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

// ReadMessageIdle is ReadMessage on a connection whose other end sends at
// least every HeartbeatInterval: it fails with ErrPeerSilent, closing the
// connection, when no bytes arrive for idle. The timer restarts on each
// chunk of a message as it arrives, not once per message, so a large
// message on a slow link is read in full; and it runs only while this
// call reads, so time the caller spends handling the previous message is
// not counted against the other end.
// Must be called from a single goroutine.
func (c *Conn) ReadMessageIdle(ctx context.Context, idle time.Duration) (Envelope, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(idle, func() { cancel(ErrPeerSilent) })
	defer timer.Stop()

	_, r, err := c.conn.Reader(ctx)
	if err != nil {
		return Envelope{}, peerSilent(ctx, idle, err)
	}
	timer.Reset(idle)
	data, err := io.ReadAll(&arrivalReader{r: r, arrived: func() { timer.Reset(idle) }})
	if err != nil {
		return Envelope{}, peerSilent(ctx, idle, err)
	}
	return ParseEnvelope(data)
}

// peerSilent reports a read that the idle timer ended as ErrPeerSilent,
// and any other read error as it is.
func peerSilent(ctx context.Context, idle time.Duration, err error) error {
	if errors.Is(context.Cause(ctx), ErrPeerSilent) {
		return fmt.Errorf("%w (%s)", ErrPeerSilent, idle)
	}
	return err
}

// arrivalReader calls arrived after each read that returned bytes, and
// reads at most idleReadChunk bytes at a time: coder/websocket fills the
// buffer it is handed (up to the end of the frame, and a message is one
// frame) before returning, so one large read would sit on a slow link
// without reporting what had already arrived.
type arrivalReader struct {
	r       io.Reader
	arrived func()
}

func (a *arrivalReader) Read(p []byte) (int, error) {
	if len(p) > idleReadChunk {
		p = p[:idleReadChunk]
	}
	n, err := a.r.Read(p)
	if n > 0 {
		a.arrived()
	}
	return n, err
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
// sends. A caller that must know a message's size before sending it
// encodes with EncodeTyped and sends with WriteFrame.
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
