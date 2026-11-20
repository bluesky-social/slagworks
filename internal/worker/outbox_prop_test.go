package worker

// Model-based property test for the site worker's durable outbox.
//
// System under test: one "worker process" = LocalDB (SQLite file) + Cache
// (its outbox) + WSConn (its ackPending map and handleObservationAck). The
// harness drives the real production methods; the only re-implemented piece
// is the WS send loop of WSConn.drainOnce (it needs a live *ws.Conn), which
// the adapter below copies call-for-call so a fault schedule can fail the
// k-th send deterministically. wsconn_outbox_prop_test.go covers the real
// drainOnce against a real ws.Hub.
//
// Reference model (independent of production helpers): every observation
// has a logical key (encoded in its MachineID). Unacked observations live in
// one of four states:
//
//	queued   in memory, due now
//	held     in memory, backed off after a non-terminal rejection (the
//	         harness's backoff base is one hour, so only a tick makes it due)
//	inflight sent on the current connection, awaiting rpc_response
//	limbo    on disk only (sent on a connection that died, or answered with
//	         an unparseable rpc_response); the next reconnect reloads it
//
// Op updates are a volatile FIFO (documented best-effort: outbox.go says a
// crash may lose them), so the model drops them on crash and records the
// loss; every other update must be delivered exactly in order.
//
// Contract sources: outbox.go type comment (durability contract), the
// drain/requeue/requeueRejected/reloadFromDisk doc comments, wsconn.go
// handleObservationAck, docs/dev/architecture.md "Outbox handling".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

var obPropNS = uuid.MustParse("6f1d7f8e-3c55-4b55-9d2e-0b8a3c4d5e6f")

func obKeyID(k int) uuid.UUID { return uuid.NewSHA1(obPropNS, []byte(fmt.Sprintf("obs-%d", k))) }

type obRowState int

const (
	obQueued obRowState = iota
	obHeld
	obInflight
	obLimbo
)

func (s obRowState) String() string {
	return [...]string{"queued", "held", "inflight", "limbo"}[s]
}

type obModelRow struct {
	key   int
	obs   model.Observation // as appended (the payload oracle)
	state obRowState
	reqID string
}

// obStats counts interesting events across all rapid cases so a run can
// report what the generator actually reached.
type obStats struct {
	cases, steps                                     atomic.Int64
	appends, updates, drains, sendFailObs, sendFailU atomic.Int64
	respOK, respTerm, respTransient, respMalformed   atomic.Int64
	dupResp, staleResp, ticks, reconnects, crashes   atomic.Int64
	lostUpdates, heldAtDrain, redeliveries           atomic.Int64
	midAppends                                       atomic.Int64
	maxRows, maxInflight                             atomic.Int64
}

func (s *obStats) bumpMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

func (s *obStats) String() string {
	return fmt.Sprintf("cases=%d steps=%d appends=%d updates=%d drains=%d sendFailObs=%d sendFailUpd=%d "+
		"respOK=%d respTerminal=%d respTransient=%d respMalformed=%d dupResp=%d staleResp=%d ticks=%d "+
		"reconnects=%d crashes=%d lostUpdates=%d heldAtDrain=%d redeliveries=%d maxRows=%d maxInflight=%d midAppends=%d",
		s.cases.Load(), s.steps.Load(), s.appends.Load(), s.updates.Load(), s.drains.Load(),
		s.sendFailObs.Load(), s.sendFailU.Load(), s.respOK.Load(), s.respTerm.Load(),
		s.respTransient.Load(), s.respMalformed.Load(), s.dupResp.Load(), s.staleResp.Load(),
		s.ticks.Load(), s.reconnects.Load(), s.crashes.Load(), s.lostUpdates.Load(),
		s.heldAtDrain.Load(), s.redeliveries.Load(), s.maxRows.Load(), s.maxInflight.Load(), s.midAppends.Load())
}

// obProc is one worker process lifetime.
type obProc struct {
	db    *LocalDB
	cache *Cache
	w     *WSConn
}

type obHarness struct {
	t     *rapid.T
	path  string
	proc  obProc
	stats *obStats
	quiet *slog.Logger

	// model
	nextKey   int
	rows      map[int]*obModelRow
	acked     map[int]bool
	updates   []model.OperationUpdate // queued, in send order
	delivered map[int]int             // key -> deliveries seen by the hub
	gotUpd    []model.OperationUpdate // hub-side receive order
	wantUpd   []model.OperationUpdate // every update the model says must arrive, in order
	answered  []string                // reqIDs already answered (duplicate delivery pool)
	stale     []string                // reqIDs from dead connections (stale delivery pool)
	trace     []string
}

func newOBHarness(t *rapid.T, dir string, stats *obStats) *obHarness {
	h := &obHarness{
		t:         t,
		path:      filepath.Join(dir, "worker.db"),
		stats:     stats,
		quiet:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		rows:      map[int]*obModelRow{},
		acked:     map[int]bool{},
		delivered: map[int]int{},
	}
	h.start()
	return h
}

func (h *obHarness) start() {
	db, err := OpenLocalDB(h.path)
	if err != nil {
		h.t.Fatalf("open local db: %v", err)
	}
	db.SetLogger(h.quiet)
	c := NewCache(db)
	c.outbox.RejectBackoffBase = time.Hour
	c.outbox.RejectBackoffMax = 2 * time.Hour
	w := NewWSConn(WSConnConfig{}, c, db, nil)
	w.SetLogger(h.quiet)
	h.proc = obProc{db: db, cache: c, w: w}
}

func (h *obHarness) stop() {
	o := h.proc.cache.outbox
	o.mu.Lock()
	if o.retryTimer != nil {
		o.retryTimer.Stop()
	}
	o.mu.Unlock()
	sqlDB, err := h.proc.db.db.DB()
	if err != nil {
		h.t.Fatalf("extract sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		h.t.Fatalf("close sql.DB: %v", err)
	}
}

func (h *obHarness) logf(format string, args ...any) {
	h.trace = append(h.trace, fmt.Sprintf(format, args...))
}

// keyOf maps an observation back to its logical key via MachineID.
func (h *obHarness) keyOf(obs model.Observation) int {
	for k := 0; k < h.nextKey; k++ {
		if obKeyID(k) == obs.MachineID {
			return k
		}
	}
	h.t.Fatalf("observation with unknown machine id %s", obs.MachineID)
	return -1
}

// --- generators ---

var obEvents = []string{"commission_report", "deploy_complete", "phone_home", "dhcp_lease", "bmc_reconcile_report", "inventory_import"}

func genObsData(t *rapid.T) json.RawMessage {
	switch rapid.IntRange(0, 5).Draw(t, "dataKind") {
	case 0:
		return nil
	case 1:
		return json.RawMessage(`{}`)
	case 2:
		return json.RawMessage(`null`)
	case 3:
		// credentials-shaped payload: must survive SQLite byte-for-byte
		pw := rapid.StringMatching(`[ -~\x{00e9}\x{4e2d}"\\]{0,24}`).Draw(t, "password")
		b, _ := json.Marshal(map[string]any{"monitor": map[string]any{"user": "monitor", "password": pw}})
		return b
	case 4:
		// whitespace-bearing JSON (json.Marshal of a RawMessage compacts it)
		return json.RawMessage("{ \"a\" : [ 1 , 2 ] ,\n \"b\" : \"x y\" }")
	default:
		n := rapid.IntRange(0, 3000).Draw(t, "blobLen")
		b, _ := json.Marshal(map[string]string{"blob": string(bytes.Repeat([]byte{'z'}, n))})
		return b
	}
}

func genOpUpdate(t *rapid.T) model.OperationUpdate {
	opIDs := []string{
		"00000000-0000-0000-0000-00000000000a",
		"00000000-0000-0000-0000-00000000000b",
		"00000000-0000-0000-0000-00000000000c",
	}
	u := model.OperationUpdate{OperationID: rapid.SampledFrom(opIDs).Draw(t, "updOp")}
	switch rapid.IntRange(0, 3).Draw(t, "updKind") {
	case 0:
		u.Phase = rapid.SampledFrom([]string{"pxe_booting", "partitioning", "rsyncing_os", "awaiting_phone_home"}).Draw(t, "phase")
	case 1:
		u.Status = string(model.OpCompleted)
	case 2:
		u.Status = string(model.OpFailed)
		u.Phase = "partitioning"
		u.Result = model.OpResult{Error: "boom", Phase: "partitioning"}.Marshal()
	default:
		u.Status = string(model.OpRunning)
	}
	return u
}

// --- actions ---

func (h *obHarness) appendObs(t *rapid.T) {
	k := h.nextKey
	h.nextKey++
	obs := model.Observation{
		MachineID: obKeyID(k),
		Event:     rapid.SampledFrom(obEvents).Draw(t, "event"),
		Data:      genObsData(t),
	}
	if rapid.Bool().Draw(t, "withOp") {
		obs.OperationID = uuid.NewSHA1(obPropNS, []byte(fmt.Sprintf("op-%d", k)))
	}
	if rapid.Bool().Draw(t, "withTS") {
		obs.Timestamp = time.Date(2026, 11, 18, 12, 0, k%60, 123456789, time.UTC)
	}
	h.proc.cache.appendObservation(obs)
	h.rows[k] = &obModelRow{key: k, obs: obs, state: obQueued}
	h.stats.appends.Add(1)
	h.stats.bumpMax(&h.stats.maxRows, int64(len(h.rows)))
	h.logf("append k=%d event=%s", k, obs.Event)
}

func (h *obHarness) appendUpdate(t *rapid.T) {
	u := genOpUpdate(t)
	if rapid.Bool().Draw(t, "viaRecordOpUpdate") && u.Phase == "" && u.Result == nil {
		h.proc.cache.RecordOpUpdate(u.OperationID, u.Status, nil)
	} else {
		h.proc.cache.appendOpUpdate(u)
	}
	h.updates = append(h.updates, u)
	h.wantUpd = append(h.wantUpd, u)
	h.stats.updates.Add(1)
	h.logf("update %s status=%q phase=%q", u.OperationID[len(u.OperationID)-1:], u.Status, u.Phase)
}

// drain runs one WSConn.drainOnce pass. failObs/failUpd are the index of
// the first failing send (-1 = none). The body mirrors drainOnce exactly.
// mid, when non-nil, runs after DrainOutbox returns and before the first
// send: it models producers (HTTP handlers, the executor) appending while
// drainOnce is mid-batch.
func (h *obHarness) drainWith(failObs, failUpd int, mid func()) {
	w := h.proc.w
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	obs, updates := w.cache.DrainOutbox()
	h.stats.drains.Add(1)

	// Oracle 1: the drained observation set is exactly the model's queued
	// set, each once; held rows are not drained.
	wantQ := map[int]bool{}
	held := 0
	for k, r := range h.rows {
		switch r.state {
		case obQueued:
			wantQ[k] = true
		case obHeld:
			held++
		case obInflight, obLimbo:
		}
	}
	if held > 0 {
		h.stats.heldAtDrain.Add(1)
	}
	gotQ := map[int]bool{}
	for _, p := range obs {
		k := h.keyOf(p.Obs)
		if gotQ[k] {
			h.fail("drain returned key %d twice", k)
		}
		gotQ[k] = true
		if h.acked[k] {
			h.fail("drain returned already-acked key %d", k)
		}
		h.checkPayload(k, p.Obs)
	}
	if !sameKeySet(wantQ, gotQ) {
		h.fail("drained set %v, model queued set %v", sortedKeys(gotQ), sortedKeys(wantQ))
	}
	// Oracle 2: op updates come out in exact FIFO order.
	if !sameUpdates(updates, h.updates) {
		h.fail("drained updates %+v, model %+v", updates, h.updates)
	}
	h.updates = nil
	if mid != nil {
		mid()
	}

	for i, p := range obs {
		reqID := uuid.New().String()
		w.trackAck(reqID, p)
		if i == failObs {
			w.untrackAck(reqID)
			w.cache.RequeueOutbox(obs[i:], updates)
			h.updates = append(append([]model.OperationUpdate(nil), updates...), h.updates...)
			h.stats.sendFailObs.Add(1)
			h.logf("drain: obs send %d/%d failed", i, len(obs))
			return
		}
		k := h.keyOf(p.Obs)
		if h.acked[k] {
			h.fail("redelivery of acked key %d", k)
		}
		if h.delivered[k] > 0 {
			h.stats.redeliveries.Add(1)
		}
		h.delivered[k]++
		r := h.rows[k]
		r.state = obInflight
		r.reqID = reqID
	}
	for i, u := range updates {
		if i == failUpd {
			w.cache.RequeueOutbox(nil, updates[i:])
			h.updates = append(append([]model.OperationUpdate(nil), updates[i:]...), h.updates...)
			h.stats.sendFailU.Add(1)
			h.logf("drain: sent %d obs, update send %d/%d failed", len(obs), i, len(updates))
			return
		}
		h.gotUpd = append(h.gotUpd, u)
	}
	h.logf("drain: sent %d obs, %d updates (held=%d)", len(obs), len(updates), held)
}

func (h *obHarness) drain(t *rapid.T) {
	nq := 0
	for _, r := range h.rows {
		if r.state == obQueued {
			nq++
		}
	}
	failObs, failUpd := -1, -1
	if rapid.IntRange(0, 3).Draw(t, "obsFaultDie") == 0 {
		failObs = rapid.IntRange(0, nq).Draw(t, "failObsAt")
	}
	if rapid.IntRange(0, 3).Draw(t, "updFaultDie") == 0 {
		failUpd = rapid.IntRange(0, len(h.updates)).Draw(t, "failUpdAt")
	}
	var mid func()
	if n := rapid.IntRange(0, 6).Draw(t, "midAppends"); n > 3 {
		mid = func() {
			for i := 3; i < n; i++ {
				if rapid.Bool().Draw(t, "midIsUpdate") {
					h.appendUpdate(t)
				} else {
					h.appendObs(t)
				}
			}
			h.stats.midAppends.Add(1)
			h.logf("  (mid-drain appends above)")
		}
	}
	// The drain loop is wake-driven: drainOnce runs only after it
	// receives from cache.Wake. With no wake pending, the loop is parked.
	select {
	case <-h.proc.cache.Wake:
	default:
		t.Skip("drain loop parked: no wake pending")
	}
	h.drainWith(failObs, failUpd, mid)
	h.stats.bumpMax(&h.stats.maxInflight, int64(len(h.inflightKeys())))
}

func (h *obHarness) inflightKeys() []int {
	var ks []int
	for k, r := range h.rows {
		if r.state == obInflight {
			ks = append(ks, k)
		}
	}
	sort.Ints(ks)
	return ks
}

func rpcResp(kind string) json.RawMessage {
	var b []byte
	switch kind {
	case "ok":
		b, _ = json.Marshal(ws.RPCResponseMsg{})
	case "terminal":
		b, _ = json.Marshal(ws.RPCResponseMsg{Error: "machine not found: terminal", Terminal: true})
	case "transient":
		b, _ = json.Marshal(ws.RPCResponseMsg{Error: "netbox unavailable"})
	case "malformed":
		b = []byte(`{"error":`)
	}
	return b
}

func (h *obHarness) respond(t *rapid.T) {
	ks := h.inflightKeys()
	if len(ks) == 0 {
		t.Skip("nothing in flight")
	}
	k := ks[rapid.IntRange(0, len(ks)-1).Draw(t, "respIdx")]
	kind := rapid.SampledFrom([]string{"ok", "ok", "ok", "terminal", "transient", "transient", "malformed"}).Draw(t, "respKind")
	h.respondKey(k, kind)
}

func (h *obHarness) respondKey(k int, kind string) {
	r := h.rows[k]
	if !h.proc.w.handleObservationAck(r.reqID, rpcResp(kind)) {
		h.fail("handleObservationAck(%s) for in-flight key %d returned false", r.reqID, k)
	}
	h.answered = append(h.answered, r.reqID)
	switch kind {
	case "ok", "terminal":
		delete(h.rows, k)
		h.acked[k] = true
		if kind == "ok" {
			h.stats.respOK.Add(1)
		} else {
			h.stats.respTerm.Add(1)
		}
	case "transient":
		r.state = obHeld
		r.reqID = ""
		h.stats.respTransient.Add(1)
	case "malformed":
		r.state = obLimbo
		r.reqID = ""
		h.stats.respMalformed.Add(1)
	}
	h.logf("respond k=%d %s", k, kind)
}

func (h *obHarness) duplicateResponse(t *rapid.T) {
	if len(h.answered) == 0 {
		t.Skip("no answered requests")
	}
	reqID := h.answered[rapid.IntRange(0, len(h.answered)-1).Draw(t, "dupIdx")]
	kind := rapid.SampledFrom([]string{"ok", "terminal", "transient", "malformed"}).Draw(t, "dupKind")
	if h.proc.w.handleObservationAck(reqID, rpcResp(kind)) {
		h.fail("duplicate rpc_response %s (%s) was consumed as an observation ack", reqID, kind)
	}
	h.stats.dupResp.Add(1)
	h.logf("duplicate response %s", kind)
}

func (h *obHarness) staleResponse(t *rapid.T) {
	if len(h.stale) == 0 {
		t.Skip("no stale requests")
	}
	reqID := h.stale[rapid.IntRange(0, len(h.stale)-1).Draw(t, "staleIdx")]
	kind := rapid.SampledFrom([]string{"ok", "terminal", "transient"}).Draw(t, "staleKind")
	if h.proc.w.handleObservationAck(reqID, rpcResp(kind)) {
		h.fail("stale rpc_response %s from a dead connection was consumed", reqID)
	}
	h.stats.staleResp.Add(1)
	h.logf("stale response %s", kind)
}

// tick makes every backed-off row due (models the backoff deadline
// passing). Production reads time.Now(); the harness rewinds NextAttempt.
func (h *obHarness) tick() {
	o := h.proc.cache.outbox
	o.mu.Lock()
	for i := range o.pending {
		o.pending[i].NextAttempt = time.Time{}
	}
	// Stop reports whether the timer was still pending, i.e. armed by a
	// drain that held rows back and not yet fired.
	armed := o.retryTimer != nil && o.retryTimer.Stop()
	o.mu.Unlock()
	anyHeld := false
	for _, r := range h.rows {
		if r.state == obHeld {
			r.state = obQueued
			anyHeld = true
		}
	}
	// The deadline passing fires the retry timer drain armed for it.
	if anyHeld && armed {
		o.wake()
	}
	h.stats.ticks.Add(1)
	h.logf("tick")
}

// reconnect mirrors connectAndRun's reconnect reset. finalDrain models the
// old connection's ctx.Done drainOnce pass against a dead socket (every
// send fails).
func (h *obHarness) reconnect(finalDrain bool) {
	if finalDrain {
		h.drainWith(0, 0, nil)
	}
	w := h.proc.w
	w.drainMu.Lock()
	w.ackPendingMu.Lock()
	w.ackPending = make(map[string]PendingOutbox)
	w.ackPendingMu.Unlock()
	w.cache.ReloadOutboxFromDisk()
	w.drainMu.Unlock()
	for _, r := range h.rows {
		if r.state == obInflight {
			h.stale = append(h.stale, r.reqID)
			r.state = obQueued
			r.reqID = ""
		}
		if r.state == obLimbo {
			r.state = obQueued
		}
	}
	h.stats.reconnects.Add(1)
	h.logf("reconnect finalDrain=%v", finalDrain)
}

func (h *obHarness) crash() {
	h.stop()
	h.start()
	for _, r := range h.rows {
		if r.state == obInflight {
			h.stale = append(h.stale, r.reqID)
		}
		r.state = obQueued
		r.reqID = ""
	}
	if n := len(h.updates); n > 0 {
		h.stats.lostUpdates.Add(int64(n))
		// Documented best-effort loss: drop the lost suffix from wantUpd.
		h.wantUpd = h.wantUpd[:len(h.wantUpd)-n]
		h.updates = nil
	}
	h.stats.crashes.Add(1)
	h.logf("crash+restart")
}

// --- invariants ---

func (h *obHarness) fail(format string, args ...any) {
	h.t.Fatalf("%s\ntrace:\n  %s", fmt.Sprintf(format, args...), joinTrace(h.trace))
}

func joinTrace(tr []string) string {
	var b bytes.Buffer
	for i, s := range tr {
		if i > 0 {
			b.WriteString("\n  ")
		}
		b.WriteString(s)
	}
	return b.String()
}

func (h *obHarness) checkPayload(k int, got model.Observation) {
	want := h.rows[k].obs
	if got.Event != want.Event || got.MachineID != want.MachineID || got.OperationID != want.OperationID {
		h.fail("key %d identity changed: got %+v want %+v", k, got, want)
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		h.fail("key %d timestamp %v, want %v", k, got.Timestamp, want.Timestamp)
	}
	if !jsonEquivalent(got.Data, want.Data) {
		h.fail("key %d data %q, want %q", k, got.Data, want.Data)
	}
}

// jsonEquivalent compares two RawMessages semantically; nil and empty are
// the same "absent" value (omitempty drops both).
func jsonEquivalent(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

func (h *obHarness) check() {
	// Disk: read the outbox table directly (not via ListPendingOutbox).
	type row struct {
		ID      uint
		Payload string
	}
	var diskRows []row
	if err := h.proc.db.db.Raw("SELECT id, payload FROM outbox ORDER BY id").Scan(&diskRows).Error; err != nil {
		h.fail("read outbox table: %v", err)
	}
	disk := map[int]uint{}
	for _, r := range diskRows {
		var o model.Observation
		if err := json.Unmarshal([]byte(r.Payload), &o); err != nil {
			h.fail("undecodable outbox row %d: %v", r.ID, err)
		}
		k := h.keyOf(o)
		if _, dup := disk[k]; dup {
			h.fail("key %d persisted twice", k)
		}
		disk[k] = r.ID
		if _, ok := h.rows[k]; !ok {
			h.fail("disk holds key %d that the model says is acked=%v", k, h.acked[k])
		}
		h.checkPayload(k, o)
	}
	for k := range h.rows {
		if _, ok := disk[k]; !ok {
			h.fail("unacked key %d (state %s) missing from disk", k, h.rows[k].state)
		}
	}

	// Memory: pending holds exactly queued+held rows, once each, with the
	// right due/held status and the row id that persists them.
	o := h.proc.cache.outbox
	o.mu.Lock()
	pend := append([]PendingOutbox(nil), o.pending...)
	o.mu.Unlock()
	now := time.Now()
	seen := map[int]bool{}
	for _, p := range pend {
		k := h.keyOf(p.Obs)
		if seen[k] {
			h.fail("key %d queued in memory twice", k)
		}
		seen[k] = true
		r, ok := h.rows[k]
		if !ok {
			h.fail("acked key %d still queued in memory", k)
		}
		if r.state != obQueued && r.state != obHeld {
			h.fail("key %d in memory but model state %s", k, r.state)
		}
		if p.RowID != uint64(disk[k]) {
			h.fail("key %d memory row id %d, disk id %d", k, p.RowID, disk[k])
		}
		isHeld := p.NextAttempt.After(now)
		if isHeld != (r.state == obHeld) {
			h.fail("key %d held=%v in memory, model state %s", k, isHeld, r.state)
		}
	}
	for k, r := range h.rows {
		if (r.state == obQueued || r.state == obHeld) && !seen[k] {
			h.fail("key %d model state %s but not queued in memory", k, r.state)
		}
	}

	// ackPending holds exactly the in-flight rows.
	w := h.proc.w
	w.ackPendingMu.Lock()
	ap := map[string]PendingOutbox{}
	for id, e := range w.ackPending {
		ap[id] = e
	}
	w.ackPendingMu.Unlock()
	inflight := 0
	for k, r := range h.rows {
		if r.state != obInflight {
			continue
		}
		inflight++
		e, ok := ap[r.reqID]
		if !ok {
			h.fail("in-flight key %d (req %s) not tracked in ackPending", k, r.reqID)
		}
		if h.keyOf(e.Obs) != k {
			h.fail("ackPending[%s] holds key %d, want %d", r.reqID, h.keyOf(e.Obs), k)
		}
	}
	if len(ap) != inflight {
		h.fail("ackPending has %d entries, model has %d in flight", len(ap), inflight)
	}

	// Wake liveness: the drain loop only runs on a wake, so due work
	// (a queued row or op update) must always have a wake pending.
	due := len(h.updates)
	for _, r := range h.rows {
		if r.state == obQueued {
			due++
		}
	}
	if due > 0 && len(h.proc.cache.Wake) == 0 {
		h.fail("%d due outbox items but no drain wake pending (the WS drain loop stays parked)", due)
	}
}

// quiesce drives the system to an empty outbox with no faults and checks
// liveness: every unacked row is eventually delivered and acked, and every
// non-lost op update arrived exactly once, in order.
func (h *obHarness) quiesce() {
	h.reconnect(false)
	for round := 0; round < 8 && (len(h.rows) > 0 || len(h.updates) > 0); round++ {
		h.tick()
		select {
		case <-h.proc.cache.Wake:
		default:
			h.fail("outbox holds %d unacked rows and %d op updates but the drain loop is parked with no wake pending", len(h.rows), len(h.updates))
		}
		h.drainWith(-1, -1, nil)
		for _, k := range h.inflightKeys() {
			h.respondKey(k, "ok")
		}
		h.check()
	}
	if len(h.rows) != 0 {
		h.fail("outbox did not drain under a fault-free schedule: %d rows left", len(h.rows))
	}
	for k := range h.acked {
		if h.delivered[k] == 0 {
			h.fail("key %d acked without ever being delivered", k)
		}
	}
	if !sameUpdates(h.gotUpd, h.wantUpd) {
		h.fail("hub received updates %+v, want %+v", h.gotUpd, h.wantUpd)
	}
}

func sameKeySet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[int]bool) []int {
	var ks []int
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

func sameUpdates(a, b []model.OperationUpdate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].OperationID != b[i].OperationID || a[i].Status != b[i].Status || a[i].Phase != b[i].Phase ||
			!bytes.Equal(a[i].Result, b[i].Result) {
			return false
		}
	}
	return true
}

func TestOutboxModel_Prop(t *testing.T) {
	stats := &obStats{}
	root := t.TempDir()
	var caseN atomic.Int64
	rapid.Check(t, func(rt *rapid.T) {
		dir := filepath.Join(root, fmt.Sprintf("case-%d", caseN.Add(1)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rt.Fatalf("mkdir: %v", err)
		}
		defer os.RemoveAll(dir)
		h := newOBHarness(rt, dir, stats)
		defer h.stop()
		stats.cases.Add(1)

		// Swarm testing: each case enables a random subset of fault
		// operations so some histories grow deep queues while others churn.
		crashOn := rapid.Bool().Draw(rt, "swarmCrash")
		reconnectOn := rapid.Bool().Draw(rt, "swarmReconnect")
		// Duplicate keys weight the uniform action choice toward growth
		// and responses so queues get deep and backoff holds rows.
		actions := map[string]func(*rapid.T){
			"appendObs":    h.appendObs,
			"appendObs2":   h.appendObs,
			"appendUpdate": h.appendUpdate,
			"drain":        h.drain,
			"drain2":       h.drain,
			"respond":      h.respond,
			"respond2":     h.respond,
			"tick":         func(*rapid.T) { h.tick() },
			"dupResponse":  h.duplicateResponse,
			"staleResp":    h.staleResponse,
			"":             func(*rapid.T) { stats.steps.Add(1); h.check() },
		}
		if crashOn {
			actions["crash"] = func(*rapid.T) { h.crash() }
		}
		if reconnectOn {
			actions["reconnect"] = func(t *rapid.T) { h.reconnect(rapid.Bool().Draw(t, "finalDrain")) }
		}
		rt.Repeat(actions)
		h.quiesce()
	})
	t.Logf("outbox model coverage: %s", stats)
}
