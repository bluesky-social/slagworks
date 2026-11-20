package worker

// Model-based property test for the site worker's durable outbox.
//
// System under test: one "worker process" = LocalDB (SQLite file) + Cache
// (its outbox) + WSConn (its ackPending map and handleOutboxAck). The
// harness drives the real production methods; the only re-implemented piece
// is the WS send loop of WSConn.drainOnce (it needs a live *ws.Conn), which
// the adapter below copies call-for-call so a fault schedule can fail the
// k-th send deterministically. wsconn_outbox_prop_test.go covers the real
// drainOnce against a real ws.Hub.
//
// Reference model (independent of production helpers): every entry, an
// observation or an op update, has a logical key. Unacked entries live in
// one of three states:
//
//	queued   due now (it may still wait behind its op's chain, see below)
//	held     backed off after a non-terminal rejection or an unparseable
//	         rpc_response (the harness's backoff base is one hour, so only
//	         a tick makes it due)
//	inflight sent on the current connection, awaiting rpc_response
//
// An op's ordered chain is its op updates plus the observations that
// complete or fail it (phone_home, commission_report, bmc_reconcile_report
// naming the op). Within a chain an entry is handed out only when every
// earlier entry of the chain has been acknowledged; other entries go out as
// soon as they are queued. Nothing is lost: a crash rehydrates every
// unacknowledged entry from SQLite, a reconnect resends every in-flight one.
//
// Contract sources: outbox.go type comment (durability contract and order),
// the drain/requeue/requeueRejected/reloadFromDisk/ack doc comments,
// wsconn.go handleOutboxAck, docs/dev/architecture.md "Outbox handling".

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

// obOpIDs is the small op ID space shared by observations and op updates,
// so their chains interleave.
var obOpIDs = []string{
	"00000000-0000-0000-0000-00000000000a",
	"00000000-0000-0000-0000-00000000000b",
	"00000000-0000-0000-0000-00000000000c",
}

type obRowState int

const (
	obQueued obRowState = iota
	obHeld
	obInflight
)

func (s obRowState) String() string {
	return [...]string{"queued", "held", "inflight"}[s]
}

type obModelRow struct {
	key   int
	rowID uint64
	obs   model.Observation      // as appended (the payload oracle), observations
	upd   *model.OperationUpdate // as appended, op updates
	chain string                 // the op whose ordered chain the entry is in, "" for none
	state obRowState
	reqID string
}

// obChain is the model's statement of which entries form an op's ordered
// chain.
func obChain(obs model.Observation, upd *model.OperationUpdate) string {
	if upd != nil {
		return upd.OperationID
	}
	switch obs.Event {
	case "phone_home", "commission_report", "bmc_reconcile_report":
		if obs.OperationID != uuid.Nil {
			return obs.OperationID.String()
		}
	}
	return ""
}

// obStats counts interesting events across all rapid cases so a run can
// report what the generator actually reached.
type obStats struct {
	cases, steps                                   atomic.Int64
	appends, updates, drains, sendFails            atomic.Int64
	respOK, respTerm, respTransient, respMalformed atomic.Int64
	dupResp, staleResp, ticks, reconnects, crashes atomic.Int64
	heldAtDrain, chainWaits, redeliveries          atomic.Int64
	midAppends                                     atomic.Int64
	maxRows, maxInflight                           atomic.Int64
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
	return fmt.Sprintf("cases=%d steps=%d appends=%d updates=%d drains=%d sendFails=%d "+
		"respOK=%d respTerminal=%d respTransient=%d respMalformed=%d dupResp=%d staleResp=%d ticks=%d "+
		"reconnects=%d crashes=%d heldAtDrain=%d chainWaits=%d redeliveries=%d maxRows=%d maxInflight=%d midAppends=%d",
		s.cases.Load(), s.steps.Load(), s.appends.Load(), s.updates.Load(), s.drains.Load(),
		s.sendFails.Load(), s.respOK.Load(), s.respTerm.Load(),
		s.respTransient.Load(), s.respMalformed.Load(), s.dupResp.Load(), s.staleResp.Load(),
		s.ticks.Load(), s.reconnects.Load(), s.crashes.Load(),
		s.heldAtDrain.Load(), s.chainWaits.Load(), s.redeliveries.Load(), s.maxRows.Load(), s.maxInflight.Load(), s.midAppends.Load())
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
	rows      map[int]*obModelRow // unacked entries
	byRow     map[uint64]int      // row ID -> key
	acked     map[int]bool
	delivered map[int]int // key -> deliveries seen by the hub
	answered  []string    // reqIDs already answered (duplicate delivery pool)
	stale     []string    // reqIDs from dead connections (stale delivery pool)
	trace     []string
}

func newOBHarness(t *rapid.T, dir string, stats *obStats) *obHarness {
	h := &obHarness{
		t:         t,
		path:      filepath.Join(dir, "worker.db"),
		stats:     stats,
		quiet:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		rows:      map[int]*obModelRow{},
		byRow:     map[uint64]int{},
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

// keyOf maps an entry back to its logical key via the row ID that
// persists it.
func (h *obHarness) keyOf(p PendingOutbox) int {
	k, ok := h.byRow[p.RowID]
	if !ok {
		h.fail("entry with unknown row id %d: %+v", p.RowID, p)
	}
	return k
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
	u := model.OperationUpdate{OperationID: rapid.SampledFrom(obOpIDs).Draw(t, "updOp")}
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

// record registers the entry the last append queued: the newest in the
// outbox.
func (h *obHarness) record(k int, obs model.Observation, upd *model.OperationUpdate) {
	o := h.proc.cache.outbox
	o.mu.Lock()
	last := o.entries[len(o.entries)-1]
	o.mu.Unlock()
	if last.RowID == 0 {
		h.fail("append of key %d was not persisted", k)
	}
	h.byRow[last.RowID] = k
	h.rows[k] = &obModelRow{key: k, rowID: last.RowID, obs: obs, upd: upd, chain: obChain(obs, upd), state: obQueued}
	h.stats.bumpMax(&h.stats.maxRows, int64(len(h.rows)))
}

func (h *obHarness) appendObs(t *rapid.T) {
	k := h.nextKey
	h.nextKey++
	obs := model.Observation{
		MachineID: obKeyID(k),
		Event:     rapid.SampledFrom(obEvents).Draw(t, "event"),
		Data:      genObsData(t),
	}
	if rapid.Bool().Draw(t, "withOp") {
		obs.OperationID = uuid.MustParse(rapid.SampledFrom(obOpIDs).Draw(t, "obsOp"))
	}
	if rapid.Bool().Draw(t, "withTS") {
		obs.Timestamp = time.Date(2026, 11, 18, 12, 0, k%60, 123456789, time.UTC)
	}
	h.proc.cache.appendObservation(obs)
	h.record(k, obs, nil)
	h.stats.appends.Add(1)
	h.logf("append k=%d event=%s op=%s", k, obs.Event, obs.OperationID.String()[35:])
}

func (h *obHarness) appendUpdate(t *rapid.T) {
	k := h.nextKey
	h.nextKey++
	u := genOpUpdate(t)
	if rapid.Bool().Draw(t, "viaRecordOpUpdate") && u.Phase == "" && u.Result == nil {
		h.proc.cache.RecordOpUpdate(u.OperationID, u.Status, nil)
	} else {
		h.proc.cache.appendOpUpdate(u)
	}
	h.record(k, model.Observation{}, &u)
	h.stats.updates.Add(1)
	h.logf("update k=%d op=%s status=%q phase=%q", k, u.OperationID[len(u.OperationID)-1:], u.Status, u.Phase)
}

// orderedKeys returns the unacked keys in record order.
func (h *obHarness) orderedKeys() []int {
	ks := make([]int, 0, len(h.rows))
	for k := range h.rows {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return h.rows[ks[i]].rowID < h.rows[ks[j]].rowID })
	return ks
}

// dueKeys is the model's answer to "what does the next drain return":
// queued entries in record order, except a chain entry behind an earlier
// unacked entry of its chain.
func (h *obHarness) dueKeys() (due []int, chainWait bool) {
	blocked := map[string]bool{}
	for _, k := range h.orderedKeys() {
		r := h.rows[k]
		if r.chain != "" && blocked[r.chain] {
			if r.state == obQueued {
				chainWait = true
			}
			continue
		}
		if r.state == obQueued {
			due = append(due, k)
		}
		if r.chain != "" {
			blocked[r.chain] = true
		}
	}
	return due, chainWait
}

// drainWith runs one WSConn.drainOnce pass. failAt is the index of the
// first failing send (-1 = none). The body mirrors drainOnce exactly.
// mid, when non-nil, runs after DrainOutbox returns and before the first
// send: it models producers (HTTP handlers, the executor) appending while
// drainOnce is mid-batch.
func (h *obHarness) drainWith(failAt int, mid func()) {
	w := h.proc.w
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	wantDue, chainWait := h.dueKeys()
	got := w.cache.DrainOutbox()
	h.stats.drains.Add(1)
	if chainWait {
		h.stats.chainWaits.Add(1)
	}
	for _, r := range h.rows {
		if r.state == obHeld {
			h.stats.heldAtDrain.Add(1)
			break
		}
	}

	// Oracle 1: the drained entries are exactly the model's due set, in
	// record order, each once.
	var gotKeys []int
	for _, p := range got {
		k := h.keyOf(p)
		if h.acked[k] {
			h.fail("drain returned already-acked key %d", k)
		}
		h.checkPayload(k, p)
		gotKeys = append(gotKeys, k)
	}
	if fmt.Sprint(gotKeys) != fmt.Sprint(wantDue) {
		h.fail("drained keys %v, model due keys %v (record order, chain-gated)", gotKeys, wantDue)
	}
	for _, k := range gotKeys {
		h.rows[k].state = obInflight
	}
	if mid != nil {
		mid()
	}

	for i, p := range got {
		reqID := uuid.New().String()
		w.trackAck(reqID, p)
		if i == failAt {
			w.untrackAck(reqID)
			w.cache.RequeueOutbox(got[i:])
			for _, q := range got[i:] {
				h.rows[h.keyOf(q)].state = obQueued
			}
			h.stats.sendFails.Add(1)
			h.logf("drain: send %d/%d failed", i, len(got))
			return
		}
		k := h.keyOf(p)
		h.checkChainHead(k)
		if h.delivered[k] > 0 {
			h.stats.redeliveries.Add(1)
		}
		h.delivered[k]++
		h.rows[k].reqID = reqID
	}
	h.logf("drain: sent %v", gotKeys)
}

// checkChainHead: an entry of an op's chain reaches global only when no
// earlier entry of the chain is still unacknowledged, so global applies a
// chain in record order and a resend can only repeat its newest entry.
func (h *obHarness) checkChainHead(k int) {
	r := h.rows[k]
	if r.chain == "" {
		return
	}
	for _, o := range h.rows {
		if o.chain == r.chain && o.rowID < r.rowID {
			h.fail("key %d of op %s's chain sent while the earlier key %d (state %s) is unacknowledged",
				k, r.chain[len(r.chain)-1:], o.key, o.state)
		}
	}
}

func (h *obHarness) drain(t *rapid.T) {
	due, _ := h.dueKeys()
	failAt := -1
	if rapid.IntRange(0, 3).Draw(t, "sendFaultDie") == 0 {
		failAt = rapid.IntRange(0, len(due)).Draw(t, "failAt")
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
	h.drainWith(failAt, mid)
	h.stats.bumpMax(&h.stats.maxInflight, int64(len(h.inflightKeys())))
}

func (h *obHarness) inflightKeys() []int {
	var ks []int
	for k, r := range h.rows {
		if r.state == obInflight && r.reqID != "" {
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
	if !h.proc.w.handleOutboxAck(r.reqID, rpcResp(kind)) {
		h.fail("handleOutboxAck(%s) for in-flight key %d returned false", r.reqID, k)
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
	case "transient", "malformed":
		r.state = obHeld
		r.reqID = ""
		if kind == "transient" {
			h.stats.respTransient.Add(1)
		} else {
			h.stats.respMalformed.Add(1)
		}
	}
	h.logf("respond k=%d %s", k, kind)
}

func (h *obHarness) duplicateResponse(t *rapid.T) {
	if len(h.answered) == 0 {
		t.Skip("no answered requests")
	}
	reqID := h.answered[rapid.IntRange(0, len(h.answered)-1).Draw(t, "dupIdx")]
	kind := rapid.SampledFrom([]string{"ok", "terminal", "transient", "malformed"}).Draw(t, "dupKind")
	if h.proc.w.handleOutboxAck(reqID, rpcResp(kind)) {
		h.fail("duplicate rpc_response %s (%s) was consumed as an outbox ack", reqID, kind)
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
	if h.proc.w.handleOutboxAck(reqID, rpcResp(kind)) {
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
	for i := range o.entries {
		o.entries[i].NextAttempt = time.Time{}
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
		h.drainWith(0, nil)
	}
	w := h.proc.w
	w.drainMu.Lock()
	w.ackPendingMu.Lock()
	w.ackPending = make(map[string]PendingOutbox)
	w.ackPendingMu.Unlock()
	if err := w.cache.ReloadOutboxFromDisk(); err != nil {
		w.drainMu.Unlock()
		h.fail("reload outbox: %v", err)
	}
	w.drainMu.Unlock()
	for _, r := range h.rows {
		if r.state == obInflight {
			if r.reqID != "" {
				h.stale = append(h.stale, r.reqID)
			}
			r.state = obQueued
			r.reqID = ""
		}
	}
	h.stats.reconnects.Add(1)
	h.logf("reconnect finalDrain=%v", finalDrain)
}

// crash replaces the process. Every unacked entry comes back from SQLite,
// due at once (the backoff state lives in memory only).
func (h *obHarness) crash() {
	h.stop()
	h.start()
	for _, r := range h.rows {
		if r.state == obInflight && r.reqID != "" {
			h.stale = append(h.stale, r.reqID)
		}
		r.state = obQueued
		r.reqID = ""
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

func (h *obHarness) checkPayload(k int, got PendingOutbox) {
	r := h.rows[k]
	if r == nil {
		h.fail("payload check for unknown or acked key %d", k)
	}
	if (got.Update != nil) != (r.upd != nil) {
		h.fail("key %d changed kind: got update=%v, want update=%v", k, got.Update != nil, r.upd != nil)
	}
	if want := r.upd; want != nil {
		u := got.Update
		if u.OperationID != want.OperationID || u.Status != want.Status || u.Phase != want.Phase ||
			!jsonEquivalent(u.Result, want.Result) {
			h.fail("key %d update changed: got %+v want %+v", k, *u, *want)
		}
		return
	}
	want := r.obs
	if got.Obs.Event != want.Event || got.Obs.MachineID != want.MachineID || got.Obs.OperationID != want.OperationID {
		h.fail("key %d identity changed: got %+v want %+v", k, got.Obs, want)
	}
	if !got.Obs.Timestamp.Equal(want.Timestamp) {
		h.fail("key %d timestamp %v, want %v", k, got.Obs.Timestamp, want.Timestamp)
	}
	if !jsonEquivalent(got.Obs.Data, want.Data) {
		h.fail("key %d data %q, want %q", k, got.Obs.Data, want.Data)
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
		Kind    string
		Payload string
	}
	var diskRows []row
	if err := h.proc.db.db.Raw("SELECT id, kind, payload FROM outbox ORDER BY id").Scan(&diskRows).Error; err != nil {
		h.fail("read outbox table: %v", err)
	}
	onDisk := map[int]bool{}
	for _, r := range diskRows {
		k, ok := h.byRow[uint64(r.ID)]
		if !ok {
			h.fail("disk holds unknown row %d", r.ID)
		}
		if _, ok := h.rows[k]; !ok {
			h.fail("disk holds key %d that the model says is acked=%v", k, h.acked[k])
		}
		e := PendingOutbox{RowID: uint64(r.ID)}
		var err error
		switch r.Kind {
		case "observation":
			err = json.Unmarshal([]byte(r.Payload), &e.Obs)
		case "op_update":
			e.Update = &model.OperationUpdate{}
			err = json.Unmarshal([]byte(r.Payload), e.Update)
		default:
			h.fail("row %d has kind %q", r.ID, r.Kind)
		}
		if err != nil {
			h.fail("undecodable outbox row %d: %v", r.ID, err)
		}
		h.checkPayload(k, e)
		onDisk[k] = true
	}
	for k := range h.rows {
		if !onDisk[k] {
			h.fail("unacked key %d (state %s) missing from disk", k, h.rows[k].state)
		}
	}

	// Memory: the outbox holds exactly the unacked entries, in record
	// order, with the right in-flight and held status.
	o := h.proc.cache.outbox
	o.mu.Lock()
	mem := append([]PendingOutbox(nil), o.entries...)
	o.mu.Unlock()
	now := time.Now()
	var memKeys []int
	for _, p := range mem {
		k := h.keyOf(p)
		r, ok := h.rows[k]
		if !ok {
			h.fail("acked key %d still in the outbox", k)
		}
		if p.inFlight != (r.state == obInflight) {
			h.fail("key %d inFlight=%v in memory, model state %s", k, p.inFlight, r.state)
		}
		if held := p.NextAttempt.After(now); held != (r.state == obHeld) {
			h.fail("key %d held=%v in memory, model state %s", k, held, r.state)
		}
		h.checkPayload(k, p)
		memKeys = append(memKeys, k)
	}
	if fmt.Sprint(memKeys) != fmt.Sprint(h.orderedKeys()) {
		h.fail("outbox holds keys %v, model unacked keys in record order %v", memKeys, h.orderedKeys())
	}

	// ackPending holds exactly the in-flight rows that were sent.
	w := h.proc.w
	w.ackPendingMu.Lock()
	ap := map[string]PendingOutbox{}
	for id, e := range w.ackPending {
		ap[id] = e
	}
	w.ackPendingMu.Unlock()
	inflight := h.inflightKeys()
	for _, k := range inflight {
		r := h.rows[k]
		e, ok := ap[r.reqID]
		if !ok {
			h.fail("in-flight key %d (req %s) not tracked in ackPending", k, r.reqID)
		}
		if h.keyOf(e) != k {
			h.fail("ackPending[%s] holds key %d, want %d", r.reqID, h.keyOf(e), k)
		}
	}
	if len(ap) != len(inflight) {
		h.fail("ackPending has %d entries, model has %d in flight", len(ap), len(inflight))
	}

	// Wake liveness: the drain loop only runs on a wake, so due work
	// must always have a wake pending.
	if due, _ := h.dueKeys(); len(due) > 0 && len(h.proc.cache.Wake) == 0 {
		h.fail("due outbox keys %v but no drain wake pending (the WS drain loop stays parked)", due)
	}
}

// quiesce drives the system to an empty outbox with no faults and checks
// liveness: every unacked entry, observation or op update, is eventually
// delivered and acked, whatever crashes and dropped connections came
// before.
func (h *obHarness) quiesce() {
	h.reconnect(false)
	// Each round delivers at least the head of every chain, so the number
	// of entries bounds the rounds needed.
	rounds := len(h.rows) + 8
	for round := 0; round < rounds && len(h.rows) > 0; round++ {
		h.tick()
		select {
		case <-h.proc.cache.Wake:
		default:
			h.fail("outbox holds %d unacked entries but the drain loop is parked with no wake pending", len(h.rows))
		}
		h.drainWith(-1, nil)
		for _, k := range h.inflightKeys() {
			h.respondKey(k, "ok")
		}
		h.check()
	}
	if len(h.rows) != 0 {
		h.fail("outbox did not drain under a fault-free schedule: %d entries left", len(h.rows))
	}
	for k := range h.acked {
		if h.delivered[k] == 0 {
			h.fail("key %d acked without ever being delivered", k)
		}
	}
	if len(h.acked) != h.nextKey {
		h.fail("%d entries appended, %d acked: an entry was lost", h.nextKey, len(h.acked))
	}
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
			"appendObs":     h.appendObs,
			"appendObs2":    h.appendObs,
			"appendUpdate":  h.appendUpdate,
			"appendUpdate2": h.appendUpdate,
			"drain":         h.drain,
			"drain2":        h.drain,
			"respond":       h.respond,
			"respond2":      h.respond,
			"tick":          func(*rapid.T) { h.tick() },
			"dupResponse":   h.duplicateResponse,
			"staleResp":     h.staleResponse,
			"":              func(*rapid.T) { stats.steps.Add(1); h.check() },
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
