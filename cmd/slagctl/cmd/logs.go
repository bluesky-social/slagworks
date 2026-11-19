package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// machineLogsCmd is `slagctl machine logs <machine>` — the single
// machine-scoped log surface. It tails the structured log-envelope
// stream (/api/v1/logs/stream) for one machine; envelopes flow from
// agents (deploy/commission), site workers, and global itself, one
// trace_id correlating an op's full lifecycle. Deploy logs are a
// subset of this stream, so this subsumes the old deploy-logs view.
//
// With --sol the command instead streams the machine's BMC SOL
// (serial console) byte feed from /api/v1/console/stream — raw serial
// output, not structured envelopes. The two modes share the positional
// <machine> arg and the follow/backfill flags but speak different
// endpoints, so --sol-only flags error when used without --sol.
//
// The root `logs` (envelope tailer), the old `machine
// logs` (deploy-logs view), and the root `console` (SOL feed) were merged into this
// one command.
func machineLogsCmd() *cobra.Command {
	var (
		// envelope-stream flags
		traceID    string
		opID       string
		sourcesCSV string
		minLevel   string
		since      string
		follow     bool
		// SOL-mode flags
		sol       bool
		fromStart bool
		tail      string
		raw       bool
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "tail a machine's structured log envelopes (or --sol for the BMC serial console)",
		Long: `Tail logs for a single machine.

Default mode opens a WebSocket to /api/v1/logs/stream on the global API
and tails structured log envelopes scoped to this machine. Envelopes
flow from agents (deploy/commission), site workers, and global itself;
one trace_id correlates the full lifecycle of an op. Deploy logs are a
subset of this stream. By default it backfills the last --since window
(10m if unset) and exits; pass -f to follow live after backfill.

With --sol it instead streams the machine's BMC SOL (serial console)
byte feed from /api/v1/console/stream — raw serial output rather than
structured envelopes. --from-start replays history, --tail backfills a
duration, -f follows the live edge.

Examples:
  slagctl machine logs kworker-l34-1.pop1            # last 10m, exit
  slagctl machine logs kworker-l34-1.pop1 --since 1h
  slagctl machine logs kworker-l34-1.pop1 -f         # live tail after backfill
  slagctl machine logs kworker-l34-1.pop1 --op 8b2e3f --source agent
  slagctl machine logs kworker-l34-1.pop1 --sol -f   # live BMC serial console
  slagctl machine logs kworker-l34-1.pop1 --sol --from-start`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cctx := cmd.Context()
			r, err := resolveMachine(cctx, apiClient, args[0])
			if err != nil {
				return err
			}

			if sol {
				// Symmetric guard: envelope-stream filters have no meaning on
				// the SOL byte feed (it's not structured/queryable). Fail
				// loudly rather than silently ignoring them — an operator who
				// typed `--sol --since 1h` expecting backfill should learn it
				// needs --tail, not get a flagless live stream. `follow` is
				// shared by both modes, so it's not in this list.
				if traceID != "" || opID != "" || sourcesCSV != "" || minLevel != "" || jsonOut || cmd.Flags().Changed("since") {
					return fmt.Errorf("envelope-stream flags (--trace, --op, --source, --level, --json, --since) cannot be combined with --sol; use --tail for the SOL backfill window")
				}
				return runConsoleStream(cctx, r.Slot(), tail, fromStart, follow, raw)
			}

			// Guard against SOL-only flags silently no-op'ing in envelope
			// mode — fail loudly so the operator knows the flag was ignored.
			if fromStart {
				return fmt.Errorf("--from-start requires --sol (only the BMC serial console supports history replay)")
			}
			if tail != "" {
				return fmt.Errorf("--tail requires --sol (use --since for the envelope stream backfill window)")
			}
			if raw {
				return fmt.Errorf("--raw requires --sol")
			}

			if since == "" {
				since = "10m"
			}
			sub := ws.LogStreamSubscribeMsg{
				TraceID:        traceID,
				MachineID:      r.Slot(),
				OpID:           opID,
				Sources:        splitSources(sourcesCSV),
				MinLevel:       minLevel,
				SinceTimestamp: parseSince(since),
			}
			renderer := humanRenderer(os.Stdout)
			if jsonOut {
				renderer = jsonRenderer(os.Stdout)
			}
			return runLogsStream(cctx, sub, renderer, follow)
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow live after backfill (default: exit when caught up)")
	cmd.Flags().StringVar(&since, "since", "", "backfill window (default 10m; e.g. 5m, 1h, RFC3339 timestamp)")
	cmd.Flags().StringVar(&traceID, "trace", "", "trace UUID to filter by")
	cmd.Flags().StringVar(&opID, "op", "", "operation UUID to filter by")
	cmd.Flags().StringVar(&sourcesCSV, "source", "", "comma-separated sources (agent,site,global)")
	cmd.Flags().StringVar(&minLevel, "level", "", "minimum level (debug|info|warn|error)")
	// SOL mode.
	cmd.Flags().BoolVar(&sol, "sol", false, "stream the machine's BMC SOL serial console instead of log envelopes")
	cmd.Flags().BoolVar(&fromStart, "from-start", false, "(--sol) replay from cast_started_at (potentially long history)")
	cmd.Flags().StringVar(&tail, "tail", "", "(--sol) start at now-DURATION (e.g. 10m, 90s; a bare number = seconds); empty = live edge or --from-start")
	cmd.Flags().BoolVar(&raw, "raw", false, "(--sol) write raw bytes to stdout without alt-screen/cursor handling")
	return cmd
}

func splitSources(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSince accepts shorthand durations ("5m", "1h", "30s") or an
// RFC3339 timestamp. Empty in → empty out (no backfill).
func parseSince(in string) string {
	if in == "" {
		return ""
	}
	if d, err := time.ParseDuration(in); err == nil {
		return time.Now().Add(-d).UTC().Format(time.RFC3339Nano)
	}
	// Otherwise pass through; server-side parser accepts RFC3339.
	return in
}

type envelopeRenderer func(env slogkit.Envelope, id int64) error

func humanRenderer(w *os.File) envelopeRenderer {
	return func(env slogkit.Envelope, _ int64) error {
		ts := env.TS.UTC().Format("15:04:05.000")
		// SOL bytes no longer flow through log envelopes (they
		// ride the cast pipeline; source=sol is rejected at ingest), so
		// there's no SOL-specific rendering here anymore.
		// Build the message body first, then prepend the phase prefix once.
		// The exec expansion replaces the body, so prefixing after it keeps
		// phase context on exec lines.
		body := env.Msg
		// Agent exec envelopes carry argv + exit_code +
		// duration_ms in Extra. The bare "exec" message is useless to a
		// human; render the actual command so the log line tells the
		// operator what was run and how it went without forcing them to
		// switch to --json mode.
		if env.Source == slogkit.SourceAgent && env.Msg == "exec" {
			if argv := env.StringExtra("argv"); argv != "" {
				exitInfo := ""
				if code, ok := env.IntExtra("exit_code"); ok && code != 0 {
					exitInfo = fmt.Sprintf(" → exit %d", code)
				}
				if dur, ok := env.IntExtra("duration_ms"); ok && dur > 1000 {
					exitInfo += fmt.Sprintf(" (%dms)", dur)
				}
				body = "exec: " + argv + exitInfo
				if errStr := env.StringExtra("err"); errStr != "" {
					body += " err=" + errStr
				}
			}
		}
		line := body
		if env.Phase != "" {
			line = fmt.Sprintf("phase=%s %s", env.Phase, body)
		}
		host := env.MachineName
		if host == "" {
			host = env.Host
		}
		// Every string field is text the sending host chose (agent envelopes
		// arrive through the site's unauthenticated /logs/ingest), so the line
		// is escaped as a whole before it reaches the operator's terminal.
		_, err := fmt.Fprintln(w, format.Untrusted(fmt.Sprintf("%s [%s] %s op=%s %s",
			ts, env.Source, host, abbreviate(env.OpID.String()), line)))
		return err
	}
}

func jsonRenderer(w *os.File) envelopeRenderer {
	enc := json.NewEncoder(w)
	return func(env slogkit.Envelope, id int64) error {
		return enc.Encode(struct {
			ID       int64            `json:"id"`
			Envelope slogkit.Envelope `json:"envelope"`
		}{ID: id, Envelope: env})
	}
}

func abbreviate(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// runLogsStream connects to /api/v1/logs/stream, sends the subscribe
// frame, and pumps envelopes through render. Returns nil on EOF when
// follow=false; otherwise blocks until ctx cancels.
func runLogsStream(ctx context.Context, sub ws.LogStreamSubscribeMsg, render envelopeRenderer, follow bool) error {
	streamURL, err := buildLogsStreamURL(apiURL)
	if err != nil {
		return err
	}
	conn, resp, err := websocket.Dial(ctx, streamURL, streamDialOptions(apiToken))
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connect to global /api/v1/logs/stream: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck

	if err := writeWSMessage(ctx, conn, ws.TypeLogStreamSubscribe, sub); err != nil {
		return err
	}

	// In non-follow mode we need to detect "backfill exhausted" via
	// trailing silence, but conn.Read blocks indefinitely without a
	// deadline. Apply a per-read 2s deadline; treat a deadline-only
	// timeout as the exit signal (any genuine ctx cancel returns first
	// because it propagates through the parent). 2s tolerates normal
	// server-side query latency on cold-start while still feeling
	// snappy on a quiet stream.
	const idleExit = 2 * time.Second
	for {
		readCtx := ctx
		var cancel context.CancelFunc
		if !follow {
			readCtx, cancel = context.WithTimeout(ctx, idleExit)
		}
		_, raw, err := conn.Read(readCtx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !follow && readCtx.Err() == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		var env ws.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Type {
		case ws.TypeSubscribed:
			// ack — start the read loop. continue.
		case ws.TypeLogStreamEnvelope:
			var msg ws.LogStreamEnvelopeMsg
			if err := json.Unmarshal(env.Data, &msg); err != nil {
				continue
			}
			if err := render(msg.Envelope, msg.ID); err != nil {
				return err
			}
		case ws.TypeLogStreamMeta:
			var msg ws.LogStreamMetaMsg
			if err := json.Unmarshal(env.Data, &msg); err == nil {
				_, _ = fmt.Fprintf(os.Stderr, "[meta] %s\n", msg.Message)
			}
		case ws.TypeError:
			var msg ws.ErrorMsg
			if err := json.Unmarshal(env.Data, &msg); err == nil {
				return fmt.Errorf("server error: %s: %s", msg.Code, msg.Message)
			}
			return fmt.Errorf("server error")
		}
	}
}

func buildLogsStreamURL(apiBase string) (string, error) {
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
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/logs/stream"
	return u.String(), nil
}

func writeWSMessage(ctx context.Context, conn *websocket.Conn, msgType string, data any) error {
	buf, err := ws.MarshalEnvelope(msgType, "", data)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, buf)
}
