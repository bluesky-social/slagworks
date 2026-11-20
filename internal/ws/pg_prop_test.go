package ws

// Postgres-backed properties (internal/testpg; needs Docker).
//
//	TestProp_ClaimRaceRealStore  claim_op RPCs racing from several workers
//	                             through the real hub into the real
//	                             store.ClaimOperation: exactly one worker
//	                             wins each pending op, the row agrees, and
//	                             every connection of the site converges on
//	                             it.
//	TestProp_ListenerReconnect   the LISTEN/NOTIFY listener: notifications
//	                             arrive once and in order; only the tail of
//	                             what was sent before a connection loss can
//	                             go missing, and every loss is bracketed by
//	                             OnDisconnect/OnConnect so a resync hooked
//	                             on OnConnect can recover it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestMain tears down the shared Postgres container that testpg starts for
// the database-backed properties in this file.
func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}

// pgCheck is rapid.Check with a quarter of the configured number of checks.
// Every case of the properties in this file pays for real Postgres round
// trips, and a listener reconnect takes a second, so they run fewer cases
// than the in-memory properties by default; -rapid.checks still scales them.
func pgCheck(t *testing.T, prop func(*rapid.T)) {
	t.Helper()
	if f := flag.Lookup("rapid.checks"); f != nil {
		orig := f.Value.String()
		if n, err := strconv.Atoi(orig); err == nil && n >= 4 {
			if err := flag.Set("rapid.checks", strconv.Itoa(n/4)); err != nil {
				t.Fatalf("harness: set rapid.checks: %v", err)
			}
			defer func() { _ = flag.Set("rapid.checks", orig) }()
		}
	}
	rapid.Check(t, prop)
}

type pgRebuilder struct {
	mu   sync.Mutex
	snap map[string]model.SiteSnapshot
}

func (r *pgRebuilder) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap[slug], nil
}

// pgHeldOps replays a connection's log through the documented consumer
// semantics (op_resync full-replaces, op_delta is last-writer-wins by
// Version, op_removed tombstones) and returns the op versions it holds.
func pgHeldOps(lg []propRecv) map[uuid.UUID]int {
	held := map[uuid.UUID]int{}
	tomb := map[uuid.UUID]int{}
	for _, r := range lg {
		switch r.typ {
		case TypeOpResync:
			var m OpResyncMsg
			_ = json.Unmarshal(r.data, &m)
			held = map[uuid.UUID]int{}
			for _, op := range m.Ops {
				held[op.ID] = op.Version
			}
		case TypeOpDelta:
			var m OpDeltaMsg
			_ = json.Unmarshal(r.data, &m)
			if tv, dead := tomb[m.Operation.ID]; dead && m.Operation.Version <= tv {
				continue
			}
			if v, ok := held[m.Operation.ID]; !ok || m.Operation.Version > v {
				held[m.Operation.ID] = m.Operation.Version
			}
		case TypeOpRemoved:
			var m OpRemovedMsg
			_ = json.Unmarshal(r.data, &m)
			id, _ := uuid.Parse(m.OperationID)
			tomb[id] = m.Version
			delete(held, id)
		}
	}
	return held
}

func TestProp_ClaimRaceRealStore(t *testing.T) {
	// One database, hub and set of connected workers for the whole property:
	// truncating every table and redoing the handshakes per case costs far
	// more than the case itself. Each case creates its own machines and ops
	// and pushes a snapshot holding exactly those machines, so the ops of
	// earlier cases are out of the site's scope and stay out of the way.
	const slug = "site-a"
	const workerCount = 4
	st := store.New(testpg.DB(t, store.Migrate))
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: slug, NetBoxSite: slug,
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.20"}); err != nil {
		t.Fatalf("harness: site config: %v", err)
	}
	seq := int64(1)
	reb := &pgRebuilder{snap: map[string]model.SiteSnapshot{slug: {SeqNum: seq, Site: model.SiteInfo{Slug: slug}}}}
	hub := NewHub(HubConfig{Auth: st, Rebuilder: reb, OpClaimer: st, ActiveOps: st, Logger: propDiscardLogger()})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	var workers []*propClient
	for i := 0; i < workerCount; i++ {
		id := fmt.Sprintf("worker-%d", i)
		_, token, err := st.RegisterSiteWorker(slug, id, id+".example.test")
		if err != nil {
			t.Fatalf("harness: register worker: %v", err)
		}
		conn, err := propDial(srv, token)
		if err != nil {
			t.Fatalf("harness: dial: %v", err)
		}
		conn.SetReadLimit(MaxMessageBytes)
		ctx, cancel := context.WithCancel(context.Background())
		c := &propClient{worker: propWorker{slug: slug, id: id}, conn: conn, cancel: cancel, notify: make(chan struct{}, 1)}
		go c.run(ctx)
		t.Cleanup(func() {
			cancel()
			_ = conn.CloseNow()
		})
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = conn.WriteTyped(wctx, TypeHello, "", HelloMsg{WorkerID: id, Proto: OpStreamProto})
		wcancel()
		if err != nil {
			t.Fatalf("harness: hello: %v", err)
		}
		if ok, _ := c.waitFor(15*time.Second, propHasType(TypeOpResync)); !ok {
			t.Fatalf("worker %s: no op_resync", id)
		}
		workers = append(workers, c)
	}

	var stats struct {
		cases, ops, claimsSent, grants, reclaims, lost, failedFirst int
	}
	pgCheck(t, func(rt *rapid.T) {
		stats.cases++
		caseID := stats.cases
		nOps := rapid.IntRange(1, 3).Draw(rt, "ops")
		// The workers that race in this case: a run of 2 to 4 of them; the
		// rest only watch.
		nRacers := rapid.IntRange(2, workerCount).Draw(rt, "workers")
		first := rapid.IntRange(0, workerCount-1).Draw(rt, "firstWorker")
		var racers []*propClient
		for i := 0; i < nRacers; i++ {
			racers = append(racers, workers[(first+i)%workerCount])
		}

		seq++
		snap := model.SiteSnapshot{SeqNum: seq, Site: model.SiteInfo{Slug: slug}}
		var ops []*model.Operation
		for i := 0; i < nOps; i++ {
			m := &model.Machine{NetBoxSerial: fmt.Sprintf("SER-%d-%d", caseID, i), State: model.MachineStateNew, PXEEligible: true}
			if err := st.CreateMachine(m); err != nil {
				rt.Fatalf("harness: machine: %v", err)
			}
			snap.Machines = append(snap.Machines, model.MachineSnapshot{ID: m.ID})
			var op *model.Operation
			var err error
			if rapid.Bool().Draw(rt, "lifecycle") {
				op, err = st.CreateMachineOp(m.ID, model.OpCommission, json.RawMessage(`{}`), model.MachineStateNew)
			} else {
				op, err = st.CreatePowerOp(m.ID, model.OpPowerCycle, json.RawMessage(`{"bmc_runtime":{"password":"x"}}`))
			}
			if err != nil {
				rt.Fatalf("harness: create op: %v", err)
			}
			ops = append(ops, op)
		}
		hub.Push(slug, snap)
		stats.ops += nOps

		// Optionally fail one op concurrently with the claims (operator
		// abort / sweep racing the claim).
		failIdx := -1
		if rapid.IntRange(0, 3).Draw(rt, "raceFail") == 0 {
			failIdx = rapid.IntRange(0, nOps-1).Draw(rt, "failOp")
		}
		// Every racer claims every op, some twice, all at once.
		type sent struct {
			w     *propClient
			op    *model.Operation
			reqID string
		}
		var all []sent
		for oi, op := range ops {
			for wi, w := range racers {
				copies := 1
				if rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("dup_%d_%d", oi, wi)) == 0 {
					copies = 2
				}
				for k := 0; k < copies; k++ {
					all = append(all, sent{w, op, fmt.Sprintf("c-%d-%d-%d-%d", caseID, oi, wi, k)})
				}
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range all {
			wg.Add(1)
			go func(s sent) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_ = s.w.conn.WriteTyped(ctx, TypeClaimOp, s.reqID, ClaimOpMsg{OperationID: s.op.ID.String()})
			}(s)
		}
		var failOutcome *store.OpOutcome
		if failIdx >= 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := st.FailOperationOutcome(ops[failIdx].ID, []byte(`{"error":"aborted"}`))
				if err == nil && out != nil {
					failOutcome = out
					hub.PublishOpRemoved(out.Op, out.MachineState)
				}
			}()
		}
		close(start)
		wg.Wait()
		stats.claimsSent += len(all)

		// Collect every response.
		winners := map[uuid.UUID]map[string]int{}
		for _, s := range all {
			var rpc RPCResponseMsg
			ok, _ := s.w.waitFor(20*time.Second, func(lg []propRecv) bool {
				for i := len(lg) - 1; i >= 0; i-- {
					if r := lg[i]; r.typ == TypeClaimOpResp && r.reqID == s.reqID {
						_ = json.Unmarshal(r.data, &rpc)
						return true
					}
				}
				return false
			})
			if !ok {
				rt.Fatalf("worker %s: no response to claim %s", s.w.worker.id, s.reqID)
			}
			if rpc.Error != "" {
				rt.Fatalf("worker %s: claim of %s returned a hard error under contention: %s", s.w.worker.id, s.op.ID, rpc.Error)
			}
			var cr ClaimOpResponse
			_ = json.Unmarshal(rpc.Data, &cr)
			if cr.Operation != nil {
				if cr.Operation.ClaimedBy != s.w.worker.id {
					rt.Fatalf("worker %s was handed a row claimed_by %q", s.w.worker.id, cr.Operation.ClaimedBy)
				}
				if winners[s.op.ID] == nil {
					winners[s.op.ID] = map[string]int{}
				}
				winners[s.op.ID][s.w.worker.id]++
				stats.grants++
			} else {
				stats.lost++
			}
		}

		want := map[uuid.UUID]int{} // this case's ops still active in the store
		for oi, op := range ops {
			row, err := st.GetOperation(op.ID)
			if err != nil {
				rt.Fatalf("read op: %v", err)
			}
			if row.Status == model.OpPending || row.Status == model.OpClaimed || row.Status == model.OpRunning {
				want[op.ID] = row.Version
			}
			w := winners[op.ID]
			if len(w) > 1 {
				rt.Fatalf("op %s was granted to %d different workers: %v (row: status=%s claimed_by=%q)", op.ID, len(w), w, row.Status, row.ClaimedBy)
			}
			failed := oi == failIdx && failOutcome != nil
			switch {
			case len(w) == 1:
				var winner string
				var grants int
				for id, n := range w {
					winner, grants = id, n
				}
				if grants > 1 {
					stats.reclaims += grants - 1
				}
				if row.ClaimedBy != winner {
					rt.Fatalf("op %s: worker %s was told it won, the row says claimed_by=%q status=%s", op.ID, winner, row.ClaimedBy, row.Status)
				}
				if !failed && row.Status != model.OpClaimed {
					rt.Fatalf("op %s: granted to %s but the row is %s", op.ID, winner, row.Status)
				}
			case failed:
				stats.failedFirst++
				if row.Status != model.OpFailed {
					rt.Fatalf("op %s: failed concurrently, never granted, row is %s", op.ID, row.Status)
				}
			default:
				rt.Fatalf("op %s was pending and claimed by %d workers at once, yet nobody won (row: status=%s claimed_by=%q)", op.ID, nRacers, row.Status, row.ClaimedBy)
			}
		}

		// Quiescence: every worker of the site, racing or not, holds exactly
		// the store's active set of this case's ops. Each connection's
		// messages leave through one ordered queue, so once the sentinel has
		// arrived everything published before it has been written too.
		sid := uuid.New()
		hub.PublishOpRemoved(model.Operation{ID: sid}, "")
		for _, w := range workers {
			ok, closed := w.waitFor(20*time.Second, func(lg []propRecv) bool {
				for i := len(lg) - 1; i >= 0; i-- {
					if r := lg[i]; r.typ == TypeOpRemoved {
						var m OpRemovedMsg
						if json.Unmarshal(r.data, &m) == nil && m.OperationID == sid.String() {
							return true
						}
					}
				}
				return false
			})
			if !ok {
				rt.Fatalf("worker %s never drained (closed=%v)", w.worker.id, closed)
			}
		}
		for _, w := range workers {
			lg, _ := w.snapshotLog()
			held := pgHeldOps(lg)
			for _, op := range ops {
				v, active := want[op.ID]
				hv, has := held[op.ID]
				switch {
				case active && (!has || hv != v):
					rt.Fatalf("worker %s holds op %s at v%d (present=%v), the store has v%d", w.worker.id, op.ID, hv, has, v)
				case !active && has:
					rt.Fatalf("worker %s still holds op %s, which is no longer active in the store", w.worker.id, op.ID)
				}
			}
		}
	})
	t.Logf("coverage: cases=%d ops=%d claims_sent=%d grants=%d (idempotent re-claims=%d) lost=%d ops_failed_before_any_claim=%d",
		stats.cases, stats.ops, stats.claimsSent, stats.grants, stats.reclaims, stats.lost, stats.failedFirst)
}

// pgListenerEvent is one entry of the listener's callback log: a delivered
// notification, or an OnConnect / OnDisconnect call. The listener makes all
// three from one goroutine, so the log orders them.
type pgListenerEvent struct {
	kind string // "notify" | "connect" | "disconnect"
	n    Notification
}

// TestProp_ListenerReconnect drives the LISTEN/NOTIFY listener through
// generated schedules of notifications and backend kills.
//
// Postgres does not promise that a notification committed just before the
// listening backend dies reaches the client, so the reference allows the
// tail of each interrupted stretch to be lost; nothing else may be.
func TestProp_ListenerReconnect(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	dsn := testpg.DSN()
	var delivered, lostTail, connects, disconnects, killed atomic.Int64
	pgCheck(t, func(rt *rapid.T) {
		var mu sync.Mutex
		var log []pgListenerEvent
		record := func(ev pgListenerEvent) {
			mu.Lock()
			log = append(log, ev)
			mu.Unlock()
		}
		l := NewListener(dsn, func(n Notification) { record(pgListenerEvent{kind: "notify", n: n}) })
		l.SetLogger(propDiscardLogger())
		l.OnConnect = func() {
			record(pgListenerEvent{kind: "connect"})
			connects.Add(1)
		}
		l.OnDisconnect = func() {
			record(pgListenerEvent{kind: "disconnect"})
			disconnects.Add(1)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = l.Run(ctx); close(done) }()
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				cancel()
				<-done
			}
		}
		defer stop()

		count := func(kind string) int {
			mu.Lock()
			defer mu.Unlock()
			n := 0
			for _, e := range log {
				if e.kind == kind {
					n++
				}
			}
			return n
		}
		waitCount := func(kind string, n int, what string) {
			deadline := time.Now().Add(20 * time.Second)
			for count(kind) < n {
				if time.Now().After(deadline) {
					rt.Fatalf("%s: %d %s events, want %d", what, count(kind), kind, n)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		waitCount("connect", 1, "listener did not connect")

		// The listener waits a second before it reconnects, so backend kills
		// are kept to a minority of the cases.
		mayKill := rapid.IntRange(0, 4).Draw(rt, "mayKill") == 0
		sent := [][]Notification{nil} // per connected stretch, in send order
		steps := rapid.IntRange(1, 8).Draw(rt, "steps")
		kills := 0
		seq := 0
		for i := 0; i < steps; i++ {
			if mayKill && kills < 2 && rapid.IntRange(0, 2).Draw(rt, "kill") == 0 {
				// Kill the listener's backend, then notify into the gap.
				if err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query = 'LISTEN slagworks' AND pid <> pg_backend_pid()`).Error; err != nil {
					rt.Fatalf("harness: terminate: %v", err)
				}
				kills++
				killed.Add(1)
				waitCount("disconnect", kills, "listener never noticed its backend was terminated")
				seq++
				if err := db.Exec(`SELECT pg_notify('slagworks', ?)`, fmt.Sprintf("machines:gap-%d", seq)).Error; err != nil {
					rt.Fatalf("harness: notify: %v", err)
				}
				waitCount("connect", kills+1, "listener did not reconnect")
				sent = append(sent, nil)
				continue
			}
			seq++
			table := rapid.SampledFrom([]string{"machines", "ssh_keys", "site_configs"}).Draw(rt, "table")
			payload := fmt.Sprintf("%s:id-%d", table, seq)
			if rapid.Bool().Draw(rt, "deleted") {
				payload = fmt.Sprintf("%s:deleted:id-%d", table, seq)
			}
			if err := db.Exec(`SELECT pg_notify('slagworks', ?)`, payload).Error; err != nil {
				rt.Fatalf("harness: notify: %v", err)
			}
			sent[len(sent)-1] = append(sent[len(sent)-1], ParseNotification(payload))
		}

		// Everything sent in the last stretch must arrive: nothing cut it
		// short. Wait for it, give a duplicate a moment to show up, then
		// stop the listener so the log is final.
		last := sent[len(sent)-1]
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			n := 0
			for i := len(log) - 1; i >= 0 && log[i].kind == "notify"; i-- {
				n++
			}
			mu.Unlock()
			if n >= len(last) || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)
		stop()

		mu.Lock()
		defer mu.Unlock()
		// Split the log into connected stretches. The callbacks must
		// alternate: connect (disconnect connect)*, then the final
		// disconnect from stopping the listener.
		var got [][]Notification
		var shape []string
		for _, e := range log {
			switch e.kind {
			case "connect":
				got = append(got, nil)
				shape = append(shape, e.kind)
			case "disconnect":
				shape = append(shape, e.kind)
			default:
				if len(shape) == 0 || shape[len(shape)-1] != "connect" {
					rt.Fatalf("notification %+v delivered while the listener reported itself disconnected; callbacks so far %v", e.n, shape)
				}
				got[len(got)-1] = append(got[len(got)-1], e.n)
			}
		}
		for i, k := range shape {
			wantKind := "connect"
			if i%2 == 1 {
				wantKind = "disconnect"
			}
			if k != wantKind {
				rt.Fatalf("OnConnect/OnDisconnect are not strictly alternating: %v", shape)
			}
		}
		if len(shape) != 2*(kills+1) {
			rt.Fatalf("%d backend kills produced callbacks %v; want one disconnect and one connect per kill (so a resync on connect covers each gap) and a final disconnect", kills, shape)
		}
		for i := range sent {
			if len(got[i]) > len(sent[i]) || fmt.Sprint(got[i]) != fmt.Sprint(sent[i][:len(got[i])]) {
				rt.Fatalf("connected stretch %d delivered something other than a prefix of what was sent in it (a duplicate, a reordering, a hole, or a notification from a gap):\n  got:  %v\n  sent: %v", i, got[i], sent[i])
			}
			delivered.Add(int64(len(got[i])))
			lostTail.Add(int64(len(sent[i]) - len(got[i])))
		}
		if len(got[len(got)-1]) != len(last) {
			rt.Fatalf("the listener stayed connected, yet notifications were lost:\n  got:  %v\n  sent: %v", got[len(got)-1], last)
		}
	})
	t.Logf("coverage: delivered=%d lost_before_a_kill=%d connects=%d disconnects=%d backend_kills=%d",
		delivered.Load(), lostTail.Load(), connects.Load(), disconnects.Load(), killed.Load())
}
