// Package cmd: BMC SOL asciinema cast streaming. The
// operator surface is `slagctl machine logs --sol <machine>`;
// this file holds the stream/render machinery that command drives.
package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// runConsoleStream dials the console-stream WebSocket, parses the
// header line + asciicast event lines, and writes decoded bytes to
// stdout. ModeLive semantics — we do not sleep on t deltas. Backfill
// renders at full speed; live frames render as they arrive.
func runConsoleStream(ctx context.Context, machineID, tail string, fromStart, follow, raw bool) error {
	streamURL, err := buildConsoleStreamURL(apiURL, machineID, tail, fromStart, follow, false /*snapshot*/)
	if err != nil {
		return err
	}
	conn, err := dialStream(ctx, streamURL)
	if err != nil {
		return fmt.Errorf("connect to /api/v1/console/stream: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck

	// Bridge ws frames into a byte stream that bufio.Scanner can chew.
	// defer pr.Close() so an early return from runConsoleStream (header
	// error, asciicast line write error, etc.) doesn't leak the
	// goroutine — without the close, pw.Write would block forever
	// waiting for a reader that's already given up.
	pr, pw := io.Pipe()
	defer pr.Close() //nolint:errcheck
	go func() {
		// Defer close-with-error so abnormal disconnects propagate
		// out of the main loop as a non-EOF error (operator gets a
		// non-zero exit and an explanation). Normal closure -> EOF
		// via the no-arg pw.Close in the bare return path.
		var pipeErr error
		defer func() {
			if pipeErr != nil {
				_ = pw.CloseWithError(pipeErr)
				return
			}
			_ = pw.Close()
		}()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
					pipeErr = err
				}
				return
			}
			// Frames are one asciicast line each (header on first
			// frame, then events). Strip any trailing newline and
			// re-add exactly one for the scanner.
			raw = []byte(strings.TrimRight(string(raw), "\n"))
			if _, err := pw.Write(append(raw, '\n')); err != nil {
				pipeErr = err
				return
			}
		}
	}()

	br := bufio.NewReaderSize(pr, 64<<10)

	// Read the header line (we don't act on it, but it has to be
	// consumed so the scanner is positioned at the events).
	headerLine, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerLine, &header); err != nil {
		return fmt.Errorf("parse header (got %q): %w", headerLine, err)
	}
	// The asciicast v2 header always carries a "version" field; if
	// it's missing, what we got back is the server's error envelope
	// (writeConsoleErr in api/console_stream.go) rendered as the
	// first frame. Surface it as an error rather than entering the
	// event loop, which would silently exit with code 0 the moment
	// the server hangs up.
	if _, ok := header["version"]; !ok {
		return fmt.Errorf("server returned non-asciicast header: %s", strings.TrimRight(string(headerLine), "\n"))
	}

	out, restore := setupTerminal(raw)
	defer restore()

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if writeErr := writeAsciicastLine(out, line); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// writeAsciicastLine parses one event line as [t, "o", data] and
// writes data to out. Lines we can't parse are reported on stderr
// and skipped — they're typically server-side error envelopes that
// trail a normal close, not a fatal error for our reader loop.
func writeAsciicastLine(out io.Writer, line []byte) error {
	var ev [3]any
	if !tryParseAsciicastLine(line, &ev) {
		fmt.Fprintf(os.Stderr, "console: skipping unrecognized line: %s", line)
		return nil
	}
	kind, _ := ev[1].(string)
	data, _ := ev[2].(string)
	if kind != "o" {
		return nil
	}
	_, err := io.WriteString(out, data)
	return err
}

func tryParseAsciicastLine(line []byte, ev *[3]any) bool {
	return json.Unmarshal(line, ev) == nil
}

// setupTerminal switches the terminal to alt-screen + hides the
// cursor (unless raw=true), and returns a restore func that's safe
// to call multiple times. Also installs a signal handler so a
// Ctrl-C doesn't leave the operator's terminal stuck in alt-screen
// mode without a cursor.
func setupTerminal(raw bool) (io.Writer, func()) {
	const (
		altOn      = "\x1b[?1049h"
		altOff     = "\x1b[?1049l"
		hideCursor = "\x1b[?25l"
		showCursor = "\x1b[?25h"
		clear      = "\x1b[2J\x1b[H"
	)
	if raw {
		// Raw mode: write straight to stdout, no setup. Restore is
		// a no-op.
		return os.Stdout, func() {}
	}
	_, _ = io.WriteString(os.Stdout, altOn+clear+hideCursor)
	var once sync.Once
	restore := func() {
		once.Do(func() {
			_, _ = io.WriteString(os.Stdout, showCursor+altOff)
		})
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigs
		restore()
		// Reset our handler and re-raise via the process-level Signal
		// API. This shape avoids a type-assertion panic on non-syscall
		// signals and works on every platform Go targets (slagworks is
		// Linux/Darwin-only today, but no reason to bake that in here).
		signal.Reset(s)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(s)
		}
	}()
	return os.Stdout, restore
}

func buildConsoleStreamURL(apiBase, machineID, tail string, fromStart, follow, snapshot bool) (string, error) {
	u, err := url.Parse(apiBase)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/console/stream"
	q := u.Query()
	q.Set("machine", machineID)
	if follow {
		q.Set("follow", "true")
	}
	switch {
	case snapshot:
		// Server replays the whole cast into its own emulator and sends one
		// reconstructed-screen frame, then tails live. Mutually
		// exclusive with fromStart/tail; snapshot always reflects full history.
		q.Set("snapshot", "true")
	case fromStart:
		q.Set("since_us", "0")
	case tail != "":
		// The server parses `tail` as a Go time.Duration. An operator
		// typing `--tail 80` (bare number, no unit) reasonably means
		// "80 seconds" — but time.ParseDuration rejects unitless input,
		// so the server would 400 the WS handshake. Normalize
		// a bare number to seconds, and validate any explicit-unit value
		// client-side so a typo surfaces as a local error naming the bad
		// value rather than an opaque "handshake got 400".
		norm, err := normalizeTailDuration(tail)
		if err != nil {
			return "", err
		}
		q.Set("tail", norm)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// normalizeTailDuration coerces the --tail value into a server-parseable
// Go duration string. A bare number (e.g. "80" or "1.5") is interpreted
// as seconds, since that's the most natural reading of `--tail 80` and
// time.ParseDuration would otherwise reject it ("missing unit"), making
// the server 400 the WS handshake. A value that already
// carries a unit (e.g. "10m", "90s") is validated and passed through
// unchanged. Anything else is a client-side error naming the offending
// value, so the operator sees a clear message instead of an opaque WS
// handshake failure.
func normalizeTailDuration(tail string) (string, error) {
	orig := strings.TrimSpace(tail)
	tail = orig
	// Bare number -> seconds. Accept ints and floats ("80", "1.5").
	// strconv.ParseFloat also accepts NaN/Inf/1e3/0x10, which
	// time.ParseDuration rejects even with an "s" appended — the
	// validation below catches those and reports `orig` (not the
	// mutated value) so the operator sees what they typed.
	if _, err := strconv.ParseFloat(tail, 64); err == nil {
		tail += "s"
	}
	if _, err := time.ParseDuration(tail); err != nil {
		return "", fmt.Errorf("invalid --tail %q: want a duration like 10m, 90s, or a bare number of seconds", orig)
	}
	return tail, nil
}
