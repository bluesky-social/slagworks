package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// helloAndDrainSerial is helloAndDrain with an explicit reported hardware
// serial, so the interlock tests can control c.verifiedSerial (the default
// noop SerialResolver accepts any non-empty serial and the hub records it).
func helloAndDrainSerial(t *testing.T, conn *Conn, workerID, serial string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: workerID, LastSeqNum: 1, Proto: OpStreamProto, Serial: serial}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil { // snapshot/no_change
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil { // op_resync
		t.Fatal(err)
	}
}

// outletOp builds a pending outlet power op of the given type with a
// PDURuntime carrying the outlet marker + downstream serial (+ allowSelf).
func outletOp(t *testing.T, opType model.OperationType, downstreamSerial string, allowSelf bool) *model.Operation {
	t.Helper()
	rt := model.PDURuntime{
		Outlets:          []model.PDUOutlet{{PDUHost: "10.0.0.1", Outlet: 5}},
		DownstreamSerial: downstreamSerial,
		Outlet:           true,
		AllowSelf:        allowSelf,
	}
	raw, err := json.Marshal(model.OpParams{PDURuntime: &rt})
	if err != nil {
		t.Fatal(err)
	}
	return &model.Operation{
		ID:        uuid.New(),
		MachineID: uuid.New(),
		Type:      opType,
		Status:    model.OpPending,
		Params:    raw,
	}
}

// claimAndRead sends a claim_op for opID and returns the parsed
// ClaimOpResponse (and any rpc-level error string). It drains any op_delta
// that a successful claim fans out.
func claimAndRead(t *testing.T, conn *Conn, opID uuid.UUID) (ClaimOpResponse, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reqID := "claim-" + opID.String()
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{OperationID: opID.String()}); err != nil {
		t.Fatal(err)
	}
	for {
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type == TypeOpDelta {
			continue // a successful claim also fans a delta; skip it
		}
		if env.Type != TypeClaimOpResp || env.RequestID != reqID {
			t.Fatalf("unexpected envelope: %+v", env)
		}
		var resp RPCResponseMsg
		if err := unmarshalData(env.Data, &resp); err != nil {
			t.Fatal(err)
		}
		var claimResp ClaimOpResponse
		if resp.Data != nil {
			_ = unmarshalData(resp.Data, &claimResp)
		}
		return claimResp, resp.Error
	}
}

// interlockHub wires a hub whose claimer returns pendingOp for the pre-CAS
// read AND op for a successful claim, with the given registered-worker set.
func interlockHub(t *testing.T, pendingOp *model.Operation, registered []model.SiteWorker) (*Hub, *fakeOpClaimer, *httptest.Server) {
	t.Helper()
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	if registered != nil {
		auth.setSiteWorkers("pop3", registered...)
	}
	// A successful claim returns a claimed copy of the pending op.
	var claimed *model.Operation
	if pendingOp != nil {
		cp := *pendingOp
		cp.Status = model.OpClaimed
		cp.ClaimedBy = "worker-1"
		cp.Version = 1
		claimed = &cp
	}
	claimer := &fakeOpClaimer{op: claimed, pendingOp: pendingOp}
	h := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer})
	// Seed a snapshot so the claim's op_delta fan-out has slug membership
	// (harmless if the claim is declined — no delta then).
	if pendingOp != nil {
		h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
			Machines: []model.MachineSnapshot{{ID: pendingOp.MachineID}}})
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, claimer, srv
}

// TestInterlock_SelfFeedingCycle_SelfDeclined_PartnerAllowed: a self-feeding
// cycle op is declined for the self-worker (claim stays "already_claimed",
// ClaimOperation never called), but a DIFFERENT worker's claim passes the
// gate and reaches the claimer.
func TestInterlock_SelfFeedingCycle_SelfDeclined_PartnerAllowed(t *testing.T) {
	op := outletOp(t, model.OpPowerCycle, "SN-SELF", false)
	_, claimer, srv := interlockHub(t, op, nil)

	// Self-worker (serial == downstream) is declined.
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-SELF")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("declined claim must not be an rpc error, got %q", errStr)
	}
	if !resp.AlreadyClaimed {
		t.Error("self-feeding cycle: expected AlreadyClaimed decline")
	}
	if resp.Operation != nil {
		t.Error("self-feeding cycle: expected nil op on decline")
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("ClaimOperation must NOT be called on interlock decline, got %d", n)
	}

	// A different worker (serial != downstream) passes the gate and claims.
	conn2 := dialHub(t, srv, "tok-1")
	if conn2 == nil {
		t.Fatal("dial failed")
	}
	defer conn2.CloseNow()
	helloAndDrainSerial(t, conn2, "worker-1", "SN-PARTNER")
	resp2, errStr2 := claimAndRead(t, conn2, op.ID)
	if errStr2 != "" {
		t.Fatalf("partner claim errored: %q", errStr2)
	}
	if resp2.Operation == nil {
		t.Error("partner claim: expected a claimed op, got none")
	}
	if n := len(claimer.calls()); n != 1 {
		t.Errorf("partner claim must reach ClaimOperation exactly once, got %d", n)
	}
}

// TestInterlock_SelfFeedingOff_DeclinedForEveryone: off whose downstream is a
// registered worker is refused for every claimant — including a partner whose
// serial differs — and even when that registered worker is disconnected
// (seeded only in ListSiteWorkers, not connected).
func TestInterlock_SelfFeedingOff_DeclinedForEveryone(t *testing.T) {
	op := outletOp(t, model.OpPowerOff, "SN-DISCONNECTED-WORKER", false)
	// The downstream serial belongs to a REGISTERED (disconnected) worker.
	registered := []model.SiteWorker{{SiteSlug: "pop3", WorkerID: "w-down", Serial: "SN-DISCONNECTED-WORKER"}}
	_, claimer, srv := interlockHub(t, op, registered)

	// A partner (different serial) still must not serve the off.
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-PARTNER")
	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("declined off must not be an rpc error, got %q", errStr)
	}
	if !resp.AlreadyClaimed {
		t.Error("off on registered worker: expected decline for the partner")
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("off on registered worker must never reach ClaimOperation, got %d", n)
	}
}

// TestInterlock_SerialCaseMismatchDoesNotDisarm: the hello gate verifies a
// worker's SMBIOS serial by asking NetBox for devices with that serial, and
// NetBox's device serial filter is case-insensitive (iexact). A worker whose
// board reports "abc123" is therefore verified as the NetBox device whose
// serial is "ABC123", and site_workers.serial holds the reported spelling.
// The op's downstream_serial is NetBox's spelling. The interlock must treat
// the two spellings as the same device, or it is silently inert for that
// worker.
func TestInterlock_SerialCaseMismatchDoesNotDisarm(t *testing.T) {
	const netboxSerial = "ABC123"   // dcim.device.serial, stamped into the op as downstream_serial
	const reportedSerial = "abc123" // what the worker's dmidecode prints

	cases := []struct {
		name       string
		opType     model.OperationType
		registered []model.SiteWorker
		claimant   string
	}{
		{
			// Both INV-1 and INV-2 apply: the worker would cut its own feed.
			name:       "self off",
			opType:     model.OpPowerOff,
			registered: []model.SiteWorker{{SiteSlug: "pop3", WorkerID: "worker-1", Serial: reportedSerial}},
			claimant:   reportedSerial,
		},
		{
			// INV-2 only: no registered row, a non-AllowSelf cycle by the
			// downstream device itself.
			name:     "self cycle",
			opType:   model.OpPowerCycle,
			claimant: reportedSerial,
		},
		{
			// INV-1 only: a partner worker (a hot spare at a multi-worker
			// site) must not power off the outlet feeding a registered worker.
			name:       "partner off",
			opType:     model.OpPowerOff,
			registered: []model.SiteWorker{{SiteSlug: "pop3", WorkerID: "w-down", Serial: reportedSerial}},
			claimant:   "SN-PARTNER",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := newFakeAuth()
			auth.register("tok-1", "pop3", "worker-1")
			if tc.registered != nil {
				auth.setSiteWorkers("pop3", tc.registered...)
			}

			op := outletOp(t, tc.opType, netboxSerial, false)
			claimed := *op
			claimed.Status = model.OpClaimed
			claimed.ClaimedBy = "worker-1"
			claimed.Version = 1
			claimer := &fakeOpClaimer{pendingOp: op, op: &claimed}

			// The production resolver accepts any serial NetBox returns exactly
			// one in-scope device for; with an iexact filter that includes a
			// case-only variant.
			resolver := newFakeSerialResolver()
			hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer, SerialResolver: resolver})
			hub.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
				Machines: []model.MachineSnapshot{{ID: op.MachineID}}})
			srv := httptest.NewServer(hub)
			defer srv.Close()
			conn := dialHub(t, srv, "tok-1")
			if conn == nil {
				t.Fatal("dial failed")
			}
			defer conn.CloseNow()
			helloAndDrainSerial(t, conn, "worker-1", tc.claimant)

			resp, errStr := claimAndRead(t, conn, op.ID)
			if errStr != "" {
				t.Fatalf("unexpected rpc error: %s", errStr)
			}
			if resp.Operation != nil || len(claimer.calls()) != 0 {
				t.Fatalf("claimant %q was granted an outlet %s whose downstream is NetBox device %q, "+
					"the worker whose board reports %q (store claims attempted: %d)",
					tc.claimant, tc.opType, netboxSerial, reportedSerial, len(claimer.calls()))
			}
			if !resp.AlreadyClaimed {
				t.Fatal("an interlock decline must be reported as already_claimed")
			}
		})
	}
}

// TestInterlock_EmptySerialClaimant_Declined: an outlet op claimed by a worker
// whose verified serial is "" (unverified) is declined fail-closed.
func TestInterlock_EmptySerialClaimant_Declined(t *testing.T) {
	op := outletOp(t, model.OpPowerCycle, "SN-SOMEWHERE", false)
	_, claimer, srv := interlockHub(t, op, nil)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "") // no verified serial

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("declined claim must not be an rpc error, got %q", errStr)
	}
	if !resp.AlreadyClaimed {
		t.Error("empty-serial claimant: expected fail-closed decline")
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("empty-serial claimant must not reach ClaimOperation, got %d", n)
	}
}

// TestInterlock_AllowSelfCycle_SelfAllowed: a self-feeding CYCLE carrying
// AllowSelf is served by the self-worker (INV-2 override).
func TestInterlock_AllowSelfCycle_SelfAllowed(t *testing.T) {
	op := outletOp(t, model.OpPowerCycle, "SN-SELF", true)
	_, claimer, srv := interlockHub(t, op, nil)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-SELF")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("allow-self cycle errored: %q", errStr)
	}
	if resp.Operation == nil {
		t.Error("allow-self cycle: expected the self-worker to claim")
	}
	if n := len(claimer.calls()); n != 1 {
		t.Errorf("allow-self cycle must reach ClaimOperation once, got %d", n)
	}
}

// TestInterlock_AllowSelfOff_StillDeclined: AllowSelf never applies to off —
// a self-feeding off (downstream is a registered worker) stays declined even
// with AllowSelf set.
func TestInterlock_AllowSelfOff_StillDeclined(t *testing.T) {
	op := outletOp(t, model.OpPowerOff, "SN-SELF", true) // AllowSelf set, but off
	registered := []model.SiteWorker{{SiteSlug: "pop3", WorkerID: "w-self", Serial: "SN-SELF"}}
	_, claimer, srv := interlockHub(t, op, registered)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-SELF")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("declined off must not be an rpc error, got %q", errStr)
	}
	if !resp.AlreadyClaimed {
		t.Error("allow-self off: INV-1 must still decline")
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("allow-self off must never reach ClaimOperation, got %d", n)
	}
}

// TestInterlock_NonOutletOp_Unaffected: a normal (non-outlet) op — no
// PDURuntime, or PDURuntime without the Outlet marker — bypasses the interlock
// and claims as today, even from an empty-serial worker.
func TestInterlock_NonOutletOp_Unaffected(t *testing.T) {
	// A plain deploy op with no PDURuntime at all.
	op := &model.Operation{
		ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpPending,
	}
	_, claimer, srv := interlockHub(t, op, nil)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "") // unverified — irrelevant for non-outlet

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("non-outlet claim errored: %q", errStr)
	}
	if resp.Operation == nil {
		t.Error("non-outlet op must claim unaffected by the interlock")
	}
	if n := len(claimer.calls()); n != 1 {
		t.Errorf("non-outlet op must reach ClaimOperation once, got %d", n)
	}
}

// TestInterlock_NonOutletOpMalformedParams_Unaffected: a NON-outlet op whose
// params blob won't decode must NOT be stranded by the interlock — it passes
// through to the normal claim machinery (the applicability probe classifies it
// as not-an-outlet-op, never fail-closes on a params quirk). Guards the
// interlock's blast-radius boundary.
func TestInterlock_NonOutletOpMalformedParams_Unaffected(t *testing.T) {
	op := &model.Operation{
		ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpPending,
		// A syntactically-valid object that DecodeParams rejects (bmc_runtime
		// must be an object, not a number) but that isOutletOp probes as
		// not-an-outlet-op.
		Params: json.RawMessage(`{"bmc_runtime": 42}`),
	}
	_, claimer, srv := interlockHub(t, op, nil)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-PARTNER")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("non-outlet claim errored: %q", errStr)
	}
	if resp.Operation == nil {
		t.Error("non-outlet op with malformed params must still claim (not stranded by the interlock)")
	}
	if n := len(claimer.calls()); n != 1 {
		t.Errorf("non-outlet op must reach ClaimOperation once, got %d", n)
	}
}

// TestInterlock_StoreErrorResolvingRegistered_FailClosed: a store error
// resolving the registered-serial set for an outlet op fails closed (never
// falls through to ClaimOperation, never grants) and is answered as a
// retryable rpc error, not a decline: every worker on the slug would hit the
// same failing read, so a decline would leave the op pending with no worker
// left to claim it.
func TestInterlock_StoreErrorResolvingRegistered_FailClosed(t *testing.T) {
	op := outletOp(t, model.OpPowerCycle, "SN-SOMEWHERE", false)
	auth := newFakeAuth()
	auth.register("tok-1", "site-c", "worker-1")
	auth.listErr = errors.New("postgres exploded")
	cp := *op
	cp.Status = model.OpClaimed
	claimer := &fakeOpClaimer{op: &cp, pendingOp: op}
	h := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer})
	// The op is on this worker's site, so the claim reaches the interlock.
	h.Push("site-c", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "site-c"},
		Machines: []model.MachineSnapshot{{ID: op.MachineID}}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-PARTNER")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if resp.Operation != nil {
		t.Fatal("store error resolving registered serials must not grant the claim")
	}
	if errStr == "" || resp.AlreadyClaimed {
		t.Errorf("store error resolving registered serials was answered already_claimed=%v error=%q; "+
			"want a retryable rpc error", resp.AlreadyClaimed, errStr)
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("fail-closed claim must not reach ClaimOperation, got %d", n)
	}
}

// TestInterlock_HelloThatCannotPersistSerialRefused: INV-1 decides which
// devices are serving site workers from the serials stored in site_workers,
// not from live connections, and a worker's hello is what writes its verified
// serial there (TouchSiteWorker). If that write fails and the hub keeps the
// connection anyway, the worker is connected and serving under a serial INV-1
// cannot see, so an outlet off for its own feed is granted to its partner.
// The hub must refuse such a hello (fail-closed), and accept the worker's
// reconnect once the store recovers.
func TestInterlock_HelloThatCannotPersistSerialRefused(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	auth.register("tok-2", "pop3", "worker-2")
	// What the store holds: worker-1's serial from an earlier hello;
	// worker-2's row has no serial yet (first connect, or a board swap).
	auth.setSiteWorkers("pop3",
		model.SiteWorker{SiteSlug: "pop3", WorkerID: "worker-1", Serial: "W1"},
		model.SiteWorker{SiteSlug: "pop3", WorkerID: "worker-2", Serial: ""})

	op := outletOp(t, model.OpPowerOff, "W2", false) // the outlet feeding worker-2
	claimed := *op
	claimed.Status = model.OpClaimed
	claimed.ClaimedBy = "worker-1"
	claimed.Version = 1
	claimer := &fakeOpClaimer{pendingOp: op, op: &claimed}
	hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer})
	hub.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: op.MachineID}}})
	srv := httptest.NewServer(hub)
	defer srv.Close()

	conn1 := dialHub(t, srv, "tok-1")
	if conn1 == nil {
		t.Fatal("dial worker-1 failed")
	}
	defer conn1.CloseNow()
	helloAndDrainSerial(t, conn1, "worker-1", "W1")

	// The store fails the write on worker-2's hello (a dropped pooled
	// connection, a lock timeout, a read-only primary after failover).
	auth.mu.Lock()
	auth.touchErr = errors.New("driver: bad connection")
	auth.mu.Unlock()

	conn2 := dialHub(t, srv, "tok-2")
	if conn2 == nil {
		t.Fatal("dial worker-2 failed")
	}
	defer conn2.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn2.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-2", LastSeqNum: 1, Proto: OpStreamProto, Serial: "W2"}); err != nil {
		t.Fatal(err)
	}
	env, err := conn2.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read worker-2 hello reply: %v", err)
	}
	if env.Type != TypeError {
		resp, _ := claimAndRead(t, conn1, op.ID)
		t.Fatalf("worker-2's hello was accepted (reply %q) although its serial W2 was never written to "+
			"site_workers; connected serials are %v, and worker-1's claim of an outlet off feeding W2 "+
			"was granted=%t", env.Type, hub.ConnectedWorkerSerials("pop3"), resp.Operation != nil)
	}
	var e ErrorMsg
	if err := unmarshalData(env.Data, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "serial_persist_failed" {
		t.Fatalf("refused hello error code = %q, want serial_persist_failed (message %q)", e.Code, e.Message)
	}
	for _, s := range hub.ConnectedWorkerSerials("pop3") {
		if s == "W2" {
			t.Fatal("worker-2 is registered with the hub although its hello was refused")
		}
	}

	// The store recovers; worker-2's reconnect is accepted and records W2.
	auth.mu.Lock()
	auth.touchErr = nil
	auth.mu.Unlock()
	conn2b := dialHub(t, srv, "tok-2")
	if conn2b == nil {
		t.Fatal("redial worker-2 failed")
	}
	defer conn2b.CloseNow()
	helloAndDrainSerial(t, conn2b, "worker-2", "W2")
	auth.mu.Lock()
	last := auth.touches[len(auth.touches)-1]
	auth.mu.Unlock()
	if last.workerID != "worker-2" || last.serial == nil || *last.serial != "W2" {
		t.Fatalf("worker-2's accepted hello must record serial W2, last touch was %+v", last)
	}
}

// TestInterlock_StoreErrorReadingOp_Retryable: the interlock reads the op row
// before it can tell whether the op is an outlet op, so that read runs for
// every claim, deploys included. A store failure on it must be answered as an
// rpc error: the worker treats that as retryable (it rolls its local claimed
// mark back to pending and claims again on its next tick). Answering
// already_claimed instead tells the worker another worker owns the op, so it
// stops asking and the op sits pending with nothing driving it. No claim is
// granted either way, and the retry claims normally once the store recovers.
func TestInterlock_StoreErrorReadingOp_Retryable(t *testing.T) {
	params, err := json.Marshal(model.OpParams{BMCRuntime: &model.BMCRuntime{Username: "admin", Password: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpPending, Params: params,
	}
	_, claimer, srv := interlockHub(t, op, nil)
	claimer.setGetErr(errors.New("driver: bad connection"))

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-PARTNER")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if resp.Operation != nil {
		t.Fatal("a failed pre-claim read must not grant the claim")
	}
	if errStr == "" || resp.AlreadyClaimed {
		t.Fatalf("store read failure on a pending deploy op was answered already_claimed=%v error=%q; "+
			"want an rpc error so the worker retries instead of abandoning a still-pending op",
			resp.AlreadyClaimed, errStr)
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("a failed pre-claim read must not reach ClaimOperation, got %d", n)
	}

	// The store recovers; the worker's next claim attempt wins the op.
	claimer.setGetErr(nil)
	resp, errStr = claimAndRead(t, conn, op.ID)
	if errStr != "" || resp.Operation == nil {
		t.Fatalf("retry after the store recovered: error=%q granted=%v, want the op granted", errStr, resp.Operation != nil)
	}
}

// TestInterlock_OutletOpEmptyDownstreamSerial_Declined: an outlet op marked
// Outlet=true but with an empty downstream serial is invalid and declined
// fail-closed at claim.
func TestInterlock_OutletOpEmptyDownstreamSerial_Declined(t *testing.T) {
	op := outletOp(t, model.OpPowerCycle, "", false) // Outlet=true, serial ""
	_, claimer, srv := interlockHub(t, op, nil)

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrainSerial(t, conn, "worker-1", "SN-PARTNER")

	resp, errStr := claimAndRead(t, conn, op.ID)
	if errStr != "" {
		t.Fatalf("declined claim must not be an rpc error, got %q", errStr)
	}
	if !resp.AlreadyClaimed {
		t.Error("invalid outlet op (empty downstream serial) must be declined")
	}
	if n := len(claimer.calls()); n != 0 {
		t.Errorf("invalid outlet op must not reach ClaimOperation, got %d", n)
	}
}
