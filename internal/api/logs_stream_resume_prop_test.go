package api

// Property: /api/v1/logs/stream resumed with last_seen_id across connection
// faults delivers every stored row exactly once and in id order. The real
// handler runs behind internal/proptest/faultproxy (drops with FIN or RST, a
// blackholed path ending in an RST, a connection lifetime limit, the byte
// stream split into tiny writes) while rows keep arriving; the client
// resumes from the last id it received, the way the watch TUI does.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/proptest/faultproxy"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// memLogStore honors the cursor and limit of logstore.QueryFilter: rows
// with id > LastSeenID, ascending, at most Limit.
type memLogStore struct {
	mu   sync.Mutex
	rows []logstore.Result
}

func (s *memLogStore) add(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, logstore.Result{ID: id, Envelope: slogkit.Envelope{TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "m"}})
}

func (s *memLogStore) Query(_ context.Context, f logstore.QueryFilter) ([]logstore.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logstore.Result
	for _, r := range s.rows {
		if r.ID > f.LastSeenID {
			out = append(out, r)
			if f.Limit > 0 && len(out) == f.Limit {
				break
			}
		}
	}
	return out, nil
}

func TestLogsStreamResumeThroughFaultProxy_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("network property")
	}
	var cases, rowsTotal, reconnTotal, faultTotal, chunkedCases atomic.Int64
	defer func() {
		t.Logf("logs resume coverage: cases=%d rows=%d reconnects=%d faults=%d chunkedCases=%d",
			cases.Load(), rowsTotal.Load(), reconnTotal.Load(), faultTotal.Load(), chunkedCases.Load())
	}()
	rapid.Check(t, func(rt *rapid.T) {
		cases.Add(1)
		st := &memLogStore{}
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
			st.add(x)
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
		var mu sync.Mutex
		var got []int64
		var last int64
		reconnects := 0
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
					_, raw, err := c.Read(ctx)
					if err != nil {
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
			st.add(x)
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
