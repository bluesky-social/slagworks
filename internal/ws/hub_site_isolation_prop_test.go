package ws

// Property test: a site whose snapshot global cannot build must cost only
// that site. One hub serves two sites. Site "broken" has no buildable
// snapshot (its rebuild fails at once or hangs until the hub's
// RebuildTimeout); site "healthy" builds. A generated history interleaves
// hellos from the broken site's workers (which pile up inside the hub's
// hello handling), hellos from healthy-site workers, snapshot pushes and op
// publications for the healthy site, fleet-wide op removals, and operator
// reads through the hub.
//
// Checked:
//
//   - every healthy-site hello is answered (snapshot, then op_resync) within
//     a bound far below RebuildTimeout, however many broken-site hellos are
//     pending;
//   - every healthy-site connection receives every push, op_delta and
//     op_removed published after its hello, in bounded time;
//   - a broken-site connection is never sent an operation row (they carry
//     per-site credentials) nor a snapshot, and its hello ends in the
//     rebuild_failed error;
//   - the hub never offers a broken-site connection as an RPC target: a read
//     for that site returns "no worker" at once instead of waiting on a
//     worker whose hello was never answered.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

const (
	isoBroken  = "broken"
	isoHealthy = "healthy"
	isoSecret  = "broken-site-secret"
	// isoRebuildTimeout is the hub's RebuildTimeout in this test, and
	// isoBound the time a healthy-site step may take: an order of magnitude
	// apart, so a healthy step that waited on a broken-site rebuild fails.
	isoRebuildTimeout = 3 * time.Second
	isoBound          = 1500 * time.Millisecond
)

type isoRebuilder struct {
	hang    atomic.Bool
	healthy atomic.Pointer[model.SiteSnapshot]
	pending atomic.Int64 // broken-site rebuild waits in flight
	peak    atomic.Int64
}

func (r *isoRebuilder) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	if slug == isoHealthy {
		return *r.healthy.Load(), nil
	}
	n := r.pending.Add(1)
	defer r.pending.Add(-1)
	for {
		if p := r.peak.Load(); n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if r.hang.Load() {
		<-ctx.Done()
		return model.SiteSnapshot{}, ctx.Err()
	}
	return model.SiteSnapshot{}, errors.New("site broken: no slagworks-production prefix")
}

type isoActiveOps struct {
	ops atomic.Pointer[[]model.Operation]
}

func (a *isoActiveOps) ListActiveOperations() ([]model.Operation, error) {
	if p := a.ops.Load(); p != nil {
		return *p, nil
	}
	return nil, nil
}

// isoConn is one worker connection and everything it was sent.
type isoConn struct {
	slug   string
	conn   *Conn
	msgs   chan Envelope // closed when the hub closes the connection
	leaked atomic.Bool   // a message carried the broken site's op params
}

func isoDial(t *rapid.T, srv *httptest.Server, token, workerID, slug string) *isoConn {
	dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, err := Dial(dctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	dcancel()
	if err != nil {
		t.Fatalf("dial for %s failed: %v", slug, err)
	}
	conn.SetReadLimit(MaxMessageBytes)
	c := &isoConn{slug: slug, conn: conn, msgs: make(chan Envelope, 256)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: workerID, Proto: OpStreamProto}); err != nil {
		t.Fatalf("hello for %s: %v", slug, err)
	}
	go func() {
		defer close(c.msgs)
		for {
			env, err := conn.ReadMessage(context.Background())
			if err != nil {
				return
			}
			if strings.Contains(string(env.Data), isoSecret) {
				c.leaked.Store(true)
			}
			c.msgs <- env
		}
	}()
	return c
}

// await reads c's messages until one satisfies want, failing after isoBound.
func (c *isoConn) await(t *rapid.T, what string, want func(Envelope) bool) {
	deadline := time.After(isoBound)
	for {
		select {
		case env, ok := <-c.msgs:
			if !ok {
				t.Fatalf("healthy-site connection was closed while waiting for %s", what)
			}
			if want(env) {
				return
			}
		case <-deadline:
			t.Fatalf("healthy-site connection did not receive %s within %v", what, isoBound)
		}
	}
}

func TestPropHubBrokenSiteDoesNotAffectOthers(t *testing.T) {
	// Opt-in: the timing bounds need real time, so a case takes over a
	// second (each broken-site hello is waited out to its RebuildTimeout).
	if os.Getenv("SLAGWORKS_PROP_SLOW") != "1" {
		t.Skip("real-time property, about 1.5s a case; set SLAGWORKS_PROP_SLOW=1 to run it")
	}
	var stats struct {
		cases, brokenHellos, healthyHellos, pushes, deltas, removals, reads, hangFlips int
		peakPending                                                                    int64
	}
	rapid.Check(t, func(rt *rapid.T) {
		stats.cases++
		auth := newFakeAuth()
		const nWorkers = 6
		for i := range nWorkers {
			auth.register(fmt.Sprintf("tok-b-%d", i), isoBroken, fmt.Sprintf("broken-%d", i))
			auth.register(fmt.Sprintf("tok-h-%d", i), isoHealthy, fmt.Sprintf("healthy-%d", i))
		}
		healthyMachine, brokenMachine := uuid.New(), uuid.New()
		seq := int64(1)
		healthySnap := func() model.SiteSnapshot {
			return model.SiteSnapshot{SeqNum: seq, Site: model.SiteInfo{Slug: isoHealthy},
				Machines: []model.MachineSnapshot{{ID: healthyMachine, State: model.MachineStateRunning}}}
		}
		reb := &isoRebuilder{}
		first := healthySnap()
		reb.healthy.Store(&first)
		reb.hang.Store(rapid.Bool().Draw(rt, "startHanging"))
		active := &isoActiveOps{}
		// The fleet's active set always holds an op of the broken site: the
		// hub must never put it on a healthy-site connection either.
		brokenOp := model.Operation{ID: uuid.New(), MachineID: brokenMachine, Type: model.OpPowerOn, Status: model.OpPending,
			Params: json.RawMessage(`{"bmc_runtime":{"password":"` + isoSecret + `"}}`)}
		ops := []model.Operation{brokenOp}
		active.ops.Store(&ops)
		hub := NewHub(HubConfig{Auth: auth, Rebuilder: reb, ActiveOps: active, RebuildTimeout: isoRebuildTimeout,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		srv := httptest.NewServer(hub)
		var broken, healthy []*isoConn
		defer func() {
			for _, c := range append(broken, healthy...) {
				_ = c.conn.CloseNow()
			}
			hub.Close()
			srv.Close()
		}()

		steps := rapid.IntRange(3, 14).Draw(rt, "steps")
		for range steps {
			switch rapid.SampledFrom([]string{"broken-hello", "broken-hello", "broken-hello", "healthy-hello", "healthy-hello", "push", "delta", "removed", "read", "flip"}).Draw(rt, "step") {
			case "broken-hello":
				if len(broken) < nWorkers {
					i := len(broken)
					broken = append(broken, isoDial(rt, srv, fmt.Sprintf("tok-b-%d", i), fmt.Sprintf("broken-%d", i), isoBroken))
					stats.brokenHellos++
				}
			case "healthy-hello":
				if len(healthy) < nWorkers {
					i := len(healthy)
					c := isoDial(rt, srv, fmt.Sprintf("tok-h-%d", i), fmt.Sprintf("healthy-%d", i), isoHealthy)
					healthy = append(healthy, c)
					stats.healthyHellos++
					c.await(rt, "its hello's op_resync", func(e Envelope) bool { return e.Type == TypeOpResync })
				}
			case "push":
				seq++
				snap := healthySnap()
				reb.healthy.Store(&snap)
				hub.Push(isoHealthy, snap)
				stats.pushes++
				for _, c := range healthy {
					want := seq
					c.await(rt, fmt.Sprintf("pushed snapshot seq %d", want), func(e Envelope) bool {
						var m SnapshotMsg
						return e.Type == TypeSnapshot && json.Unmarshal(e.Data, &m) == nil && m.Snapshot.SeqNum == want
					})
				}
			case "delta":
				op := model.Operation{ID: uuid.New(), MachineID: healthyMachine, Type: model.OpPowerOn, Status: model.OpPending}
				hub.PublishOpDelta(op, "")
				stats.deltas++
				for _, c := range healthy {
					c.await(rt, "an op_delta of its site", func(e Envelope) bool {
						var m OpDeltaMsg
						return e.Type == TypeOpDelta && json.Unmarshal(e.Data, &m) == nil && m.Operation.ID == op.ID
					})
				}
			case "removed":
				// Fleet-wide, unfiltered: it is offered to the broken site's
				// half-open connections too and must not wait on them.
				gone := model.Operation{ID: uuid.New(), MachineID: healthyMachine, Version: 3}
				hub.PublishOpRemoved(gone, "")
				stats.removals++
				for _, c := range healthy {
					c.await(rt, "an op_removed", func(e Envelope) bool {
						var m OpRemovedMsg
						return e.Type == TypeOpRemoved && json.Unmarshal(e.Data, &m) == nil && m.OperationID == gone.ID.String()
					})
				}
			case "read":
				stats.reads++
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), isoBound)
				res := hub.DHCPLeases(ctx, isoBroken)
				cancel()
				if len(res) != 0 || time.Since(start) > isoBound/2 {
					rt.Fatalf("a read for the broken site was routed to %d worker(s) and took %v; no worker of that site has an answered hello", len(res), time.Since(start))
				}
			case "flip":
				reb.hang.Store(!reb.hang.Load())
				stats.hangFlips++
			}
		}
		stats.peakPending = max(stats.peakPending, reb.peak.Load())

		// Every broken-site hello ends in rebuild_failed, having been sent
		// nothing else.
		for i, c := range broken {
			deadline := time.After(isoRebuildTimeout + isoBound)
			sawError := false
		drain:
			for {
				select {
				case env, ok := <-c.msgs:
					if !ok {
						break drain
					}
					if env.Type != TypeError {
						rt.Fatalf("broken-site connection %d was sent a %s: %s", i, env.Type, env.Data)
					}
					sawError = true
				case <-deadline:
					rt.Fatalf("broken-site connection %d was still open %v after its hello", i, isoRebuildTimeout+isoBound)
				}
			}
			if !sawError {
				rt.Fatalf("broken-site connection %d was closed without the rebuild_failed error", i)
			}
		}
		// No healthy-site connection was ever sent the broken site's op.
		for i, c := range healthy {
			if c.leaked.Load() {
				rt.Fatalf("healthy-site connection %d was sent the broken site's op params", i)
			}
		}
	})
	t.Logf("coverage: cases=%d broken-site hellos=%d (peak pending in the hub at once=%d) healthy-site hellos=%d pushes=%d op_deltas=%d op_removals=%d broken-site reads=%d fail/hang flips=%d",
		stats.cases, stats.brokenHellos, stats.peakPending, stats.healthyHellos, stats.pushes, stats.deltas, stats.removals, stats.reads, stats.hangFlips)
}
