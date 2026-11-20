package cmd

// Global's operator streams send one WebSocket message per stored row:
// /api/v1/logs/stream one per log envelope, /api/v1/console/stream one per
// cast event. coder/websocket refuses any message over its 32 KiB default
// read limit and closes the connection, but nothing upstream holds a row to
// that size. A log envelope reaches global in a site-worker batch of up to
// 4 MiB, and a cast event encodes to as much as about 48 KiB. Each test
// below serves one such message and then an ordinary one, and checks that
// the slagctl client delivers both.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// defaultClientReadLimit is coder/websocket's read limit on a connection
// that never calls SetReadLimit.
const defaultClientReadLimit = 32768

// tailscaledOutputEnvelope is what the agent logs after a failed tailscale
// enroll: logBuffer (cmd/slagworks-agent/tailscale.go) puts all of
// tailscaled's output into one field of one envelope, and a `tailscale up`
// that runs out its timeout leaves minutes of retry lines.
func tailscaledOutputEnvelope() slogkit.Envelope {
	line := `2026/01/02 03:04:05 bootstrapDNS("derp1.example.net", 192.0.2.1) for "controlplane.example.com" error: ` +
		`Get "https://derp1.example.net/bootstrap-dns?q=controlplane.example.com": context deadline exceeded` + "\n"
	return slogkit.Envelope{
		TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent,
		Msg: "tailscale: tailscaled output", MachineID: uuid.New(), OpID: uuid.New(),
		Extra: map[string]any{"output": strings.TrimSpace(strings.Repeat(line, 200))},
	}
}

// largeLogStreamServer acks the subscribe, then sends envelope id 1 (over the
// default read limit) and id 2 (one short line) the way handleLogsStream
// writes rows, and holds the connection open like the live tail.
func largeLogStreamServer(t *testing.T) func(ctx context.Context, c *websocket.Conn) {
	t.Helper()
	after := slogkit.Envelope{TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "after"}
	var msgs [][]byte
	for i, env := range []slogkit.Envelope{tailscaledOutputEnvelope(), after} {
		buf, err := ws.MarshalEnvelope(ws.TypeLogStreamEnvelope, "", ws.LogStreamEnvelopeMsg{ID: int64(i + 1), Envelope: env})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, buf)
	}
	if n := len(msgs[0]); n <= defaultClientReadLimit || n > ws.MaxMessageBytes/4 {
		t.Fatalf("large stream message is %d bytes; it must exceed %d and fit a worker log batch (%d)",
			n, defaultClientReadLimit, ws.MaxMessageBytes/4)
	}
	ack, err := ws.MarshalEnvelope(ws.TypeSubscribed, "", ws.SubscribedMsg{})
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		for _, m := range append([][]byte{ack}, msgs...) {
			if c.Write(ctx, websocket.MessageText, m) != nil {
				return
			}
		}
		_, _, _ = c.Read(ctx)
	}
}

func TestMachineLogs_ReadsEnvelopeOverDefaultReadLimit(t *testing.T) {
	stubStreamServer(t, largeLogStreamServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ids []int64
	err := runLogsStream(ctx, ws.LogStreamSubscribeMsg{}, func(_ slogkit.Envelope, id int64) error {
		ids = append(ids, id)
		return nil
	}, false)
	if err != nil || len(ids) != 2 {
		t.Fatalf("machine logs rendered ids %v and returned %v; want [1 2] and nil (envelope 1 is over 32 KiB)", ids, err)
	}
}

// The watch TUI's log pane and `operation watch --plain` read through
// logSource, which reconnects from its cursor on any error. An envelope the
// client refuses never advances the cursor, so every reconnect is served the
// same envelope and the pane stops just before it.
func TestWatchLogSource_ReadsEnvelopeOverDefaultReadLimit(t *testing.T) {
	seen := stubStreamServer(t, largeLogStreamServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var ids []int64
	err := logSource(uuid.NewString(), uuid.NewString(), time.Time{})(ctx, func(id int64, _ slogkit.Envelope) {
		ids = append(ids, id)
		if id == 2 {
			cancel()
		}
	})
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("watch log source delivered ids %v over %d connection(s) and returned %v; want [1 2] on one connection (envelope 1 is over 32 KiB)",
			ids, len(seen), err)
	}
}

// largeConsoleFrame is the data of one cast event over the default read
// limit once encoded. A cast event is one castpump flush, under 8 KiB of raw
// bytes (FlushSize plus one ReadBufSize read), and the console stream writes
// each C0 control byte in it as a six-byte \u escape, so a flush dense with
// them (a BMC sending a run of NUL bytes, say) encodes to several times its
// raw size.
var largeConsoleFrame = strings.Repeat("\x00", 6000)

// largeConsoleStreamServer sends the asciicast header, an event over the
// default read limit and one short event, in the console stream's frame
// format, then returns (the stub closes the connection normally).
func largeConsoleStreamServer(t *testing.T) func(ctx context.Context, c *websocket.Conn) {
	t.Helper()
	header, err := json.Marshal(map[string]any{"version": 2, "width": 80, "height": 24})
	if err != nil {
		t.Fatal(err)
	}
	msgs := [][]byte{header}
	for i, data := range []string{largeConsoleFrame, "after"} {
		line, err := json.Marshal([3]any{float64(i) / 10, "o", data})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, line)
	}
	if n := len(msgs[1]); n <= defaultClientReadLimit {
		t.Fatalf("large console frame is %d bytes; it must exceed %d", n, defaultClientReadLimit)
	}
	return func(ctx context.Context, c *websocket.Conn) {
		for _, m := range msgs {
			if c.Write(ctx, websocket.MessageText, m) != nil {
				return
			}
		}
	}
}

func TestMachineLogsSOL_ReadsFrameOverDefaultReadLimit(t *testing.T) {
	stubStreamServer(t, largeConsoleStreamServer(t))
	// runConsoleStream in raw mode writes the decoded bytes to os.Stdout.
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close() //nolint:errcheck
	prevStdout := os.Stdout
	os.Stdout = out
	t.Cleanup(func() { os.Stdout = prevStdout })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = runConsoleStream(ctx, uuid.NewString(), "", true /*fromStart*/, false /*follow*/, true /*raw*/)
	os.Stdout = prevStdout
	got, readErr := os.ReadFile(out.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil || !bytes.Equal(got, []byte(largeConsoleFrame+"after")) {
		t.Fatalf("machine logs --sol wrote %d bytes and returned %v; want both frames (%d bytes) and nil (frame 1 is over 32 KiB)",
			len(got), err, len(largeConsoleFrame)+len("after"))
	}
}

func TestWatchConsoleSource_ReadsFrameOverDefaultReadLimit(t *testing.T) {
	stubStreamServer(t, largeConsoleStreamServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []byte
	err := consoleSource(uuid.NewString())(ctx, func(b []byte, _ bool) {
		got = append(got, b...)
		if bytes.HasSuffix(got, []byte("after")) {
			cancel()
		}
	}, func() {}, func(bool) {})
	if !bytes.Equal(got, []byte(largeConsoleFrame+"after")) {
		t.Fatalf("watch console source delivered %d bytes and returned %v; want both frames (%d bytes; frame 1 is over 32 KiB)",
			len(got), err, len(largeConsoleFrame)+len("after"))
	}
}
