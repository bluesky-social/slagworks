package api

// The operator streams behind internal/proptest/faultproxy, which stands in
// for the load balancer and reverse proxy in front of global.
//
// TestLogsStreamResumeThroughFaultProxy_Prop: /api/v1/logs/stream resumed
// with last_seen_id across connection faults delivers every stored row
// exactly once and in id order. The faults are drops with FIN or RST, a
// blackholed path ending in an RST, a connection lifetime limit and the byte
// stream split into tiny writes, while rows keep arriving; the client
// resumes from the last id it received, the way slagctl does.
//
// TestOperatorStreamsOutliveProxyIdleTimeout_Prop: a logs or console stream
// with nothing to send stays open behind a proxy idle timeout of a few
// keepalive intervals.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/proptest/faultproxy"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// lowerDefaultRapidChecks runs the calling test's rapid.Check with n cases
// unless -rapid.checks was given on the command line, which then scales the
// test up for a deep local run. For properties whose cases are expensive.
func lowerDefaultRapidChecks(t *testing.T, n int) {
	t.Helper()
	const name = "rapid.checks"
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == name })
	if explicit {
		return
	}
	// Value.Set, not flag.Set: flag.Set would make the flag look explicit
	// to the next caller.
	v := flag.Lookup(name).Value
	prev := v.String()
	if err := v.Set(fmt.Sprint(n)); err != nil {
		t.Fatalf("set -%s: %v", name, err)
	}
	t.Cleanup(func() { _ = v.Set(prev) })
}

func TestLogsStreamResumeThroughFaultProxy_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("network property")
	}
	// A case streams up to 1200 rows through up to eight faults, and the
	// handler polls for new rows only every pollInterval.
	lowerDefaultRapidChecks(t, 25)
	var cases, rowsTotal, reconnTotal, faultTotal, chunkedCases atomic.Int64
	defer func() {
		t.Logf("logs resume coverage: cases=%d rows=%d reconnects=%d faults=%d chunkedCases=%d",
			cases.Load(), rowsTotal.Load(), reconnTotal.Load(), faultTotal.Load(), chunkedCases.Load())
	}()
	rapid.Check(t, func(rt *rapid.T) {
		cases.Add(1)
		st := &growingLogStore{}
		s := &Server{echo: echo.New(), logStore: st}
		s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
		srv := httptest.NewServer(s.echo)
		defer srv.Close()
		px, err := faultproxy.New(strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			rt.Fatalf("proxy: %v", err)
		}
		defer px.Close()
		px.SetSeed(rapid.Uint64().Draw(rt, "seed"))

		// Rows: sparse increasing ids, some stored before the client
		// connects (backfill), the rest while it streams.
		total := rapid.IntRange(1, 1200).Draw(rt, "rows")
		pre := rapid.IntRange(0, total).Draw(rt, "pre")
		var ids []int64
		var id int64
		for i := 0; i < total; i++ {
			id += int64(rapid.IntRange(1, 3).Draw(rt, "gap"))
			ids = append(ids, id)
		}
		for _, x := range ids[:pre] {
			st.add(x, "m")
		}
		type fault struct {
			kind    int
			afterMs int
		}
		faults := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) fault {
			return fault{kind: rapid.IntRange(0, 5).Draw(t, "faultKind"), afterMs: rapid.IntRange(0, 60).Draw(t, "afterMs")}
		}), 0, 8).Draw(rt, "faults")

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		const readStall = 5 * time.Second
		var mu sync.Mutex
		var got []int64
		var last int64
		reconnects := 0
		stalled := ""
		clientDone := make(chan struct{})
		go func() {
			defer close(clientDone)
			for ctx.Err() == nil {
				mu.Lock()
				from := last
				mu.Unlock()
				c, _, err := websocket.Dial(ctx, "ws://"+px.Addr()+"/api/v1/logs/stream", nil) //nolint:bodyclose // websocket owns the response
				if err != nil {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				sub, _ := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", ws.LogStreamSubscribeMsg{LastSeenID: from})
				if c.Write(ctx, websocket.MessageText, sub) != nil {
					_ = c.CloseNow()
					continue
				}
				for {
					// Rows are owed until the last one arrives, so a read
					// that waits this long is on a dead connection. A
					// loaded machine now and then fails to tell the client
					// that the proxy closed its side (the read then hangs
					// until a TCP keepalive probe, 30s later); resuming is
					// right then too. With the proxied connection still
					// open it is the stream that stopped.
					readCtx, stop := context.WithTimeout(ctx, readStall)
					_, raw, err := c.Read(readCtx)
					timedOut := readCtx.Err() != nil && ctx.Err() == nil
					stop()
					if err != nil {
						if timedOut && px.Open() > 0 {
							mu.Lock()
							stalled = fmt.Sprintf("no frame for %v on an open connection after id %d", readStall, last)
							mu.Unlock()
							cancel()
						}
						break
					}
					var env ws.Envelope
					if json.Unmarshal(raw, &env) != nil || env.Type != ws.TypeLogStreamEnvelope {
						continue
					}
					var m ws.LogStreamEnvelopeMsg
					if json.Unmarshal(env.Data, &m) != nil {
						continue
					}
					mu.Lock()
					got = append(got, m.ID)
					if m.ID > last {
						last = m.ID
					}
					full := len(got) >= len(ids)
					mu.Unlock()
					if full {
						cancel()
					}
				}
				_ = c.CloseNow()
				mu.Lock()
				reconnects++
				mu.Unlock()
			}
		}()

		// Writer: the remaining rows, interleaved with faults.
		fi := 0
		for i, x := range ids[pre:] {
			st.add(x, "m")
			if fi < len(faults) && i%((len(ids)-pre)/(len(faults)+1)+1) == 0 {
				f := faults[fi]
				fi++
				faultTotal.Add(1)
				if f.kind == 3 {
					chunkedCases.Add(1)
				}
				time.Sleep(time.Duration(f.afterMs) * time.Millisecond)
				switch f.kind {
				case 0:
					px.DropAll()
				case 1:
					px.ResetAll()
				case 2:
					px.BlackholeAll()
					time.Sleep(10 * time.Millisecond)
					px.ResetAll()
				case 3:
					px.SetChunking(rapid.IntRange(1, 64).Draw(rt, "chunk"), 0)
				case 4:
					px.SetMaxLifetime(time.Duration(rapid.IntRange(5, 50).Draw(rt, "lifeMs")) * time.Millisecond)
				default:
					px.SetChunking(0, 0)
					px.SetMaxLifetime(0)
				}
			}
		}
		px.SetMaxLifetime(0)
		px.SetChunking(0, 0)
		<-clientDone

		mu.Lock()
		defer mu.Unlock()
		rowsTotal.Add(int64(len(ids)))
		reconnTotal.Add(int64(reconnects))
		if stalled != "" {
			rt.Fatalf("stream stalled with rows still owed: %s (received %d of %d rows, reconnects=%d)", stalled, len(got), len(ids), reconnects)
		}
		if len(got) != len(ids) {
			rt.Fatalf("client received %d rows, want %d (reconnects=%d)", len(got), len(ids), reconnects)
		}
		for i := range ids {
			if got[i] != ids[i] {
				rt.Fatalf("row %d: got id %d, want %d (duplicate, gap or reorder across a resume; reconnects=%d)", i, got[i], ids[i], reconnects)
			}
		}
	})
}

// TestOperatorStreamsOutliveProxyIdleTimeout_Prop: global pings an operator
// stream every keepalive interval (30s; shortened here), so a stream with
// nothing to send, a log filter matching no new rows or a console that
// prints nothing, is not closed by a proxy that drops connections idle for
// a few intervals. That holds whether the proxy counts bytes in both
// directions (a load balancer) or only from the server (a reverse proxy's
// read timeout): the ping is the server's traffic and the client's pong is
// its own. A logs stream then still delivers the next row on the same
// connection.
func TestOperatorStreamsOutliveProxyIdleTimeout_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("each case waits out a proxy idle timeout several times")
	}
	lowerDefaultRapidChecks(t, 10)
	rapid.Check(t, func(rt *rapid.T) {
		st := &growingLogStore{}
		machineID := uuid.New()
		s := &Server{echo: echo.New(), logStore: st, streamKeepalive: testStreamKeepalive,
			consoleStore: &fakeConsoleStore{cast: &model.ConsoleCast{ID: 1, MachineID: machineID, Width: 80, Height: 24, CastStartedAt: time.Now()}}}
		s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
		s.echo.GET("/api/v1/console/stream", s.handleConsoleStream)
		srv := httptest.NewServer(s.echo)
		defer srv.Close()
		px, err := faultproxy.New(strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			rt.Fatalf("proxy: %v", err)
		}
		defer px.Close()

		stream := rapid.SampledFrom([]string{"logs", "console"}).Draw(rt, "stream")
		idle := time.Duration(rapid.IntRange(4, 8).Draw(rt, "proxyIdleKeepalives")) * testStreamKeepalive
		mode := rapid.SampledFrom([]string{"both directions", "server bytes only"}).Draw(rt, "proxyIdleCounts")
		windows := rapid.IntRange(2, 4).Draw(rt, "idleWindows")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		path := "/api/v1/logs/stream"
		if stream == "console" {
			path = "/api/v1/console/stream?follow=true&machine=" + machineID.String()
		}
		c, _, err := websocket.Dial(ctx, "ws://"+px.Addr()+path, nil) //nolint:bodyclose // websocket owns the response
		if err != nil {
			rt.Fatalf("dial %s stream: %v", stream, err)
		}
		defer c.CloseNow() //nolint:errcheck
		if stream == "logs" {
			sub, _ := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", ws.LogStreamSubscribeMsg{})
			if err := c.Write(ctx, websocket.MessageText, sub); err != nil {
				rt.Fatalf("subscribe: %v", err)
			}
		}
		// The handshake's one frame: the subscribed ack, or the asciicast
		// header.
		if _, _, err := c.Read(ctx); err != nil {
			rt.Fatalf("%s stream handshake: %v", stream, err)
		}
		// Keep reading, as every slagctl client does, so pings are answered.
		type frame struct {
			raw []byte
			err error
		}
		frames := make(chan frame, 1)
		go func() {
			_, raw, err := c.Read(ctx)
			frames <- frame{raw, err}
		}()

		if mode == "both directions" {
			px.SetIdleTimeout(idle)
		} else {
			px.SetDownstreamIdleTimeout(idle)
		}
		select {
		case f := <-frames:
			rt.Fatalf("idle %s stream ended or sent data within %d proxy idle timeouts of %v (counting %s; keepalive %v): err=%v frame=%s proxy=%+v",
				stream, windows, idle, mode, testStreamKeepalive, f.err, f.raw, px.Stats())
		case <-time.After(time.Duration(windows) * idle):
		}
		if got := px.Stats(); got.Dropped != 0 || got.Open != 1 {
			rt.Fatalf("proxy timed out an idle %s stream (idle timeout %v counting %s; keepalive %v): %+v", stream, idle, mode, testStreamKeepalive, got)
		}
		if stream != "logs" {
			return
		}
		st.add(7, "after the quiet spell")
		select {
		case f := <-frames:
			var env ws.Envelope
			var m ws.LogStreamEnvelopeMsg
			if f.err != nil || json.Unmarshal(f.raw, &env) != nil || json.Unmarshal(env.Data, &m) != nil || m.ID != 7 {
				rt.Fatalf("after the quiet spell the stream gave err=%v frame=%s, want row 7", f.err, f.raw)
			}
		case <-time.After(10 * time.Second):
			rt.Fatalf("row added after the quiet spell was not delivered within 10s")
		}
	})
}
