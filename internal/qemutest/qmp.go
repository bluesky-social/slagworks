//go:build integration

package qemutest

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// QMP is a minimal client for the QEMU Machine Protocol. Commands are
// sent as JSON requests and the matching "return" is decoded; any
// asynchronous events that arrive before that return are read and
// discarded.
type QMP struct {
	conn net.Conn
	dec  *json.Decoder
	mu   sync.Mutex
}

type qmpRequest struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type qmpMessage struct {
	Return  json.RawMessage `json:"return,omitempty"`
	QMP     json.RawMessage `json:"QMP,omitempty"`
	Event   string          `json:"event,omitempty"`
	Execute string          `json:"execute,omitempty"`
	Error   *qmpError       `json:"error,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func newQMP(socketPath string) (*QMP, error) {
	var conn net.Conn
	var err error
	for i := 0; i < 20; i++ {
		conn, err = net.DialTimeout("unix", socketPath, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to QMP socket %s: %w", socketPath, err)
	}
	q := &QMP{conn: conn, dec: json.NewDecoder(conn)}
	if _, err := q.readMessage(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading QMP greeting: %w", err)
	}
	if err := q.execute("qmp_capabilities"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp_capabilities: %w", err)
	}
	return q, nil
}

// execute sends a no-argument command and waits for its return.
func (q *QMP) execute(command string) error {
	return q.executeArgs(command, nil, nil)
}

// executeArgs sends a command, optionally with arguments, decodes the
// return payload into out (when non-nil), and ignores any asynchronous
// QMP events that arrive before the matching return.
func (q *QMP) executeArgs(command string, args map[string]any, out any) error {
	// A QMP connection is a request/response stream without request IDs in
	// this minimal client. Serialize the complete write/read transaction so
	// concurrent FakeBMC HTTP handlers cannot consume one another's replies.
	q.mu.Lock()
	defer q.mu.Unlock()

	req := qmpRequest{Execute: command, Arguments: args}
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshalling QMP command %s: %w", command, err)
	}
	// A trailing newline is not strictly required by QMP's grammar,
	// but QEMU's streaming JSON parser is more reliable about
	// flushing single-command reads when fed a terminator.
	raw = append(raw, '\n')
	_ = q.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := q.conn.Write(raw); err != nil {
		return err
	}
	for {
		resp, err := q.readMessage()
		if err != nil {
			return err
		}
		if resp.Error != nil {
			return fmt.Errorf("QMP error %s: %s", resp.Error.Class, resp.Error.Desc)
		}
		if resp.Return == nil {
			// Asynchronous event; keep reading until the return arrives.
			continue
		}
		if out != nil {
			if err := json.Unmarshal(resp.Return, out); err != nil {
				return fmt.Errorf("decoding QMP %s return: %w", command, err)
			}
		}
		return nil
	}
}

func (q *QMP) readMessage() (*qmpMessage, error) {
	_ = q.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var msg qmpMessage
	if err := q.dec.Decode(&msg); err != nil {
		return nil, fmt.Errorf("decoding QMP message: %w", err)
	}
	return &msg, nil
}

func (q *QMP) Reset() error { return q.execute("system_reset") }
func (q *QMP) Quit() error  { return q.execute("quit") }
func (q *QMP) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conn.Close()
}

// queryStatusResult is the decodable subset of `query-status`.
type queryStatusResult struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

// QueryStatus returns QEMU's current VM run-state string ("running",
// "paused", "prelaunch", "inmigrate", "shutdown", ...).
func (q *QMP) QueryStatus() (string, error) {
	var out queryStatusResult
	if err := q.executeArgs("query-status", nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// Stop pauses the VM (the deterministic test representation of chassis
// power-off).
func (q *QMP) Stop() error { return q.execute("stop") }

// Cont resumes a paused VM (the deterministic test representation of
// chassis power-on).
func (q *QMP) Cont() error { return q.execute("cont") }

// SetBootOrder changes the next-boot device order via the HMP
// human-monitor-command "boot_set <target>" (e.g. "n" for network,
// "c" for first disk).
func (q *QMP) SetBootOrder(target string) error {
	return q.executeArgs("human-monitor-command", map[string]any{
		"command-line": "boot_set " + target,
	}, nil)
}
