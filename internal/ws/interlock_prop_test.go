package ws

// Property tests for the P4b self-power interlock (interlock.go) and the
// claim_op RPC path around it (hub.go handleClaimOp).
//
// The reference decision is written from the invariants in interlock.go's
// doc comments, over a params blob the test assembles as raw JSON itself
// (never by marshalling model.OpParams and never by calling DecodeParams),
// so a shared decoding mistake cannot hide in both sides.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

type ilShape int

const (
	ilNoParams        ilShape = iota // nil / empty params
	ilEmptyObject                    // {}
	ilBMCOnly                        // a BMC power/deploy op
	ilPDUMachine                     // machine --via pdu: pdu_runtime without the outlet marker
	ilOutlet                         // operator outlet op: pdu_runtime.outlet == true
	ilOutletFalse                    // pdu_runtime.outlet explicitly false
	ilOutletCorrupt                  // outlet marker present, rest of the block undecodable
	ilNonObject                      // array / string / number / null
	ilTruncated                      // not JSON at all
	ilPDUWrongType                   // pdu_runtime is a string
	ilOutletWrongType                // pdu_runtime.outlet is a string
	ilPDUNull                        // pdu_runtime is null
)

const ilShapes = int(ilPDUNull) + 1

var ilShapeNames = [...]string{"no_params", "empty_object", "bmc_only", "pdu_machine", "outlet", "outlet_false",
	"outlet_corrupt", "non_object", "truncated", "pdu_wrong_type", "outlet_wrong_type", "pdu_null"}

type ilCase struct {
	shape      ilShape
	opType     model.OperationType
	downstream string
	allowSelf  bool
	claimant   string // verified serial of the claiming worker ("" = unverified)
	registered []string
	raw        json.RawMessage
}

var ilSerials = []string{"", "W1", "W2", "D9", "w1"}

func ilQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func drawILCase(t *rapid.T) ilCase {
	c := ilCase{
		shape:      ilShape(rapid.IntRange(0, ilShapes-1).Draw(t, "shape")),
		opType:     rapid.SampledFrom(model.AllOperationTypes).Draw(t, "opType"),
		downstream: rapid.SampledFrom(ilSerials).Draw(t, "downstream"),
		allowSelf:  rapid.Bool().Draw(t, "allowSelf"),
		claimant:   rapid.SampledFrom(ilSerials).Draw(t, "claimant"),
	}
	// Bias toward the shape and types the interlock is about.
	if rapid.IntRange(0, 2).Draw(t, "forceOutlet") > 0 {
		c.shape = ilOutlet
		c.opType = rapid.SampledFrom([]model.OperationType{model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle}).Draw(t, "powerType")
	}
	for _, s := range ilSerials[1:] {
		if rapid.Bool().Draw(t, "registered_"+s) {
			c.registered = append(c.registered, s)
		}
	}
	extra := ""
	if rapid.Bool().Draw(t, "unknownField") {
		extra = `,"future_field":{"x":[1,2,3]}`
	}
	outlets := `[{"pdu_host":"10.0.0.9","outlet":5,"password":"pdu-secret"}]`
	ds := ""
	if c.downstream != "" || rapid.Bool().Draw(t, "explicitEmptyDownstream") {
		ds = `,"downstream_serial":` + ilQuote(c.downstream)
	}
	as := ""
	if c.allowSelf {
		as = `,"allow_self":true`
	}
	switch c.shape {
	case ilNoParams:
		c.raw = nil
	case ilEmptyObject:
		c.raw = json.RawMessage(`{}`)
	case ilBMCOnly:
		c.raw = json.RawMessage(`{"bmc_runtime":{"user":"admin","password":"x"}` + extra + `}`)
	case ilPDUMachine:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlets":` + outlets + ds + as + `}` + extra + `}`)
	case ilOutlet:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlets":` + outlets + `,"outlet":true` + ds + as + `}` + extra + `}`)
	case ilOutletFalse:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlets":` + outlets + `,"outlet":false` + ds + as + `}` + extra + `}`)
	case ilOutletCorrupt:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlets":"not-a-list","outlet":true` + ds + as + `}` + extra + `}`)
	case ilNonObject:
		c.raw = json.RawMessage(rapid.SampledFrom([]string{`[]`, `"x"`, `42`, `null`, `true`}).Draw(t, "nonObject"))
	case ilTruncated:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlet":tr`)
	case ilPDUWrongType:
		c.raw = json.RawMessage(`{"pdu_runtime":"outlet"}`)
	case ilOutletWrongType:
		c.raw = json.RawMessage(`{"pdu_runtime":{"outlet":"true"` + ds + `}}`)
	case ilPDUNull:
		c.raw = json.RawMessage(`{"pdu_runtime":null}`)
	}
	return c
}

func (c ilCase) String() string {
	return fmt.Sprintf("shape=%s type=%s downstream=%q allow_self=%v claimant=%q registered=%v params=%s",
		ilShapeNames[c.shape], c.opType, c.downstream, c.allowSelf, c.claimant, c.registered, c.raw)
}

func (c ilCase) op() *model.Operation {
	return &model.Operation{ID: uuid.New(), MachineID: uuid.New(), Type: c.opType, Status: model.OpPending, Params: c.raw}
}

func (c ilCase) registeredSet() map[string]bool {
	m := map[string]bool{}
	for _, s := range c.registered {
		m[s] = true
	}
	return m
}

// ilSameSerial is the reference's serial relation: NetBox matches device
// serials ignoring letter case, and an empty serial names no device.
func ilSameSerial(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// refEligible is the reference: interlock.go's invariants, by shape.
func (c ilCase) refEligible() bool {
	switch c.shape {
	case ilOutlet:
		// continue below
	case ilOutletCorrupt:
		return false // explicitly an outlet op, but undecodable: fail closed
	default:
		return true // not explicitly an outlet op: outside the interlock
	}
	if c.downstream == "" {
		return false // invalid outlet op
	}
	if c.claimant == "" {
		return false // unverified claimant
	}
	inRegistered := false
	for _, s := range c.registered {
		if ilSameSerial(s, c.downstream) {
			inRegistered = true
		}
	}
	if c.opType == model.OpPowerOff && inRegistered {
		return false // INV-1
	}
	if ilSameSerial(c.downstream, c.claimant) {
		// INV-2: only an explicit AllowSelf cycle/on.
		return c.allowSelf && (c.opType == model.OpPowerCycle || c.opType == model.OpPowerOn)
	}
	return true
}

var ilStats struct {
	cases, outlet, eligible, declined, selfFeed, inv1, allowSelfServed atomic.Int64
	wsCases, wsClaimed, wsDeclined, wsGetErr, wsListErr, wsClaimErr    atomic.Int64
	wsForeign                                                          atomic.Int64
}

// TestProp_InterlockDecision checks the pure decision (applicability probe
// + strict eligibility) against the reference for every generated op.
func TestProp_InterlockDecision(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		c := drawILCase(rt)
		ilStats.cases.Add(1)
		op := c.op()
		got := true
		reason := ""
		if isOutletOp(op.Params) {
			got, reason = outletClaimEligibility(op, c.claimant, c.registeredSet())
		}
		want := c.refEligible()
		if c.shape == ilOutlet {
			ilStats.outlet.Add(1)
			if ilSameSerial(c.downstream, c.claimant) {
				ilStats.selfFeed.Add(1)
				if want {
					ilStats.allowSelfServed.Add(1)
				}
			}
		}
		if want {
			ilStats.eligible.Add(1)
		} else {
			ilStats.declined.Add(1)
		}
		if got != want {
			rt.Fatalf("interlock decision = %v (reason %q), reference says %v\n  %s", got, reason, want, c)
		}
		if !got && reason == "" {
			rt.Fatalf("declined with an empty reason: %s", c)
		}
		// Metamorphic: the decision for an `off` never becomes MORE
		// permissive than for the same op as a cycle, and adding the
		// downstream to the registered set never makes anything eligible.
		if c.shape == ilOutlet {
			reg := c.registeredSet()
			reg[c.downstream] = true
			op2 := *op
			if more, _ := outletClaimEligibility(&op2, c.claimant, reg); more && !got {
				rt.Fatalf("registering the downstream made the op eligible: %s", c)
			}
			off := *op
			off.Type = model.OpPowerOff
			cyc := *op
			cyc.Type = model.OpPowerCycle
			offOK, _ := outletClaimEligibility(&off, c.claimant, c.registeredSet())
			cycOK, _ := outletClaimEligibility(&cyc, c.claimant, c.registeredSet())
			if offOK && !cycOK {
				rt.Fatalf("off is eligible where cycle is not: %s", c)
			}
		}
	})
	t.Logf("coverage: cases=%d outlet=%d eligible=%d declined=%d self_feed=%d allow_self_served=%d",
		ilStats.cases.Load(), ilStats.outlet.Load(), ilStats.eligible.Load(), ilStats.declined.Load(),
		ilStats.selfFeed.Load(), ilStats.allowSelfServed.Load())
}

// TestProp_ClaimOpThroughHub drives the claim_op RPC over a real WebSocket
// with generated ops, claimant serials, registered-worker sets, site
// membership and store fault schedules, and checks the documented outcome:
//
//   - the store CAS is attempted iff the op's machine is in the claimant's
//     site, the reference says the claimant is eligible and the pre-claim
//     reads succeeded, always under the WS-authenticated worker id;
//   - a granted claim returns the row; a routine loss, a site-scope decline
//     or an interlock decline returns already_claimed with no rpc error;
//   - a store failure is never reported as a grant or as already_claimed:
//     it travels as an rpc error, so the worker retries.
func TestProp_ClaimOpThroughHub(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		c := drawILCase(rt)
		ilStats.wsCases.Add(1)
		getErr := rapid.IntRange(0, 9).Draw(rt, "getErr") == 0
		listErr := rapid.IntRange(0, 9).Draw(rt, "listErr") == 0
		claimErr := rapid.IntRange(0, 9).Draw(rt, "claimErr") == 0
		casWins := rapid.IntRange(0, 4).Draw(rt, "casWins") > 0
		opGone := rapid.IntRange(0, 9).Draw(rt, "opGone") == 0
		inSite := rapid.IntRange(0, 5).Draw(rt, "foreignSite") != 0

		auth := newFakeAuth()
		auth.register("tok-1", "site-a", "worker-1")
		var regs []model.SiteWorker
		for _, s := range c.registered {
			regs = append(regs, model.SiteWorker{SiteSlug: "site-a", WorkerID: "w-" + s, Serial: s})
		}
		auth.setSiteWorkers("site-a", regs...)
		if listErr {
			auth.listErr = errors.New("injected: list site workers failed")
		}
		op := c.op()
		claimed := *op
		claimed.Status = model.OpClaimed
		claimed.ClaimedBy = "worker-1"
		claimed.Version = 1
		claimer := &fakeOpClaimer{pendingOp: op}
		if opGone {
			claimer.pendingOp = nil
		}
		if c.shape == ilTruncated {
			casWins = false // invalid JSON cannot be a stored (jsonb) row a CAS returns
		}
		if casWins && !opGone {
			claimer.op = &claimed
		}
		if getErr {
			claimer.getErr = errors.New("injected: read op failed")
		}
		if claimErr {
			claimer.err = errors.New("injected: claim failed")
		}
		h := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer, Logger: propDiscardLogger()})
		snap := model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "site-a"},
			Machines: []model.MachineSnapshot{{ID: uuid.New()}}}
		if inSite {
			snap.Machines = append(snap.Machines, model.MachineSnapshot{ID: op.MachineID})
		}
		h.Push("site-a", snap)
		srv := httptest.NewServer(h)
		defer srv.Close()
		conn, err := propDial(srv, "tok-1")
		if err != nil {
			rt.Fatalf("harness: dial: %v", err)
		}
		defer conn.CloseNow()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", Proto: OpStreamProto, Serial: c.claimant}); err != nil {
			rt.Fatalf("harness: hello: %v", err)
		}
		for {
			env, err := conn.ReadMessage(ctx)
			if err != nil {
				rt.Fatalf("harness: hello exchange: %v", err)
			}
			if env.Type == TypeOpResync {
				break
			}
		}
		if err := conn.WriteTyped(ctx, TypeClaimOp, "req", ClaimOpMsg{OperationID: op.ID.String()}); err != nil {
			rt.Fatalf("harness: claim: %v", err)
		}
		var rpc RPCResponseMsg
		for {
			env, err := conn.ReadMessage(ctx)
			if err != nil {
				rt.Fatalf("no claim_op_response: %v\n  %s", err, c)
			}
			if env.Type != TypeClaimOpResp {
				continue
			}
			if env.RequestID != "req" {
				rt.Fatalf("claim_op_response request_id = %q", env.RequestID)
			}
			if err := json.Unmarshal(env.Data, &rpc); err != nil {
				rt.Fatalf("decode rpc_response: %v", err)
			}
			break
		}
		var resp ClaimOpResponse
		if len(rpc.Data) > 0 {
			if err := json.Unmarshal(rpc.Data, &resp); err != nil {
				rt.Fatalf("decode claim response: %v", err)
			}
		}
		calls := claimer.calls()

		// --- reference outcome ---
		// The gates run in this order: the op read, the site scope, the
		// interlock (which reads the registered workers for an outlet op
		// only), then the CAS. An absent op passes the gates and loses the CAS.
		outletMarked := c.shape == ilOutlet || c.shape == ilOutletCorrupt
		foreign := !getErr && !opGone && !inSite
		readFailed := getErr || (!foreign && !opGone && outletMarked && listErr)
		eligible := !foreign && (opGone || c.refEligible())
		wantCAS := !readFailed && eligible
		desc := fmt.Sprintf("%s in_site=%v get_err=%v list_err=%v claim_err=%v cas_wins=%v op_gone=%v -> rpc.error=%q already_claimed=%v granted=%v store_claims=%d",
			c, inSite, getErr, listErr, claimErr, casWins, opGone, rpc.Error, resp.AlreadyClaimed, resp.Operation != nil, len(calls))

		if wantCAS != (len(calls) == 1) || len(calls) > 1 {
			rt.Fatalf("store CAS attempted %d time(s), reference wants attempted=%v\n  %s", len(calls), wantCAS, desc)
		}
		for _, call := range calls {
			if call.WorkerID != "worker-1" || call.OperationID != op.ID {
				rt.Fatalf("CAS called for (%s, %q), want the authenticated worker and the requested op\n  %s", call.OperationID, call.WorkerID, desc)
			}
		}
		granted := resp.Operation != nil
		switch {
		case readFailed:
			ilStats.wsDeclined.Add(1)
			if getErr {
				ilStats.wsGetErr.Add(1)
			} else {
				ilStats.wsListErr.Add(1)
			}
			// messages.go, ClaimOpResponse: "Hard failures (DB error,
			// malformed request) still travel as RPCResponseMsg.Error" so
			// the worker rolls the op back to pending and retries.
			// Answering already_claimed would make it give the op up.
			if granted || rpc.Error == "" || resp.AlreadyClaimed {
				rt.Fatalf("a failed pre-claim read must travel as an rpc error, neither a grant nor already_claimed\n  %s", desc)
			}
		case !eligible:
			ilStats.wsDeclined.Add(1)
			if foreign {
				ilStats.wsForeign.Add(1)
			}
			if granted || rpc.Error != "" || !resp.AlreadyClaimed {
				rt.Fatalf("a site-scope or interlock decline must be already_claimed with no error\n  %s", desc)
			}
		case claimErr:
			ilStats.wsClaimErr.Add(1)
			if granted || rpc.Error == "" || resp.AlreadyClaimed {
				rt.Fatalf("a store claim error must travel as an rpc error\n  %s", desc)
			}
		case casWins && !opGone:
			ilStats.wsClaimed.Add(1)
			if !granted || rpc.Error != "" || resp.AlreadyClaimed {
				rt.Fatalf("a won CAS must return the op row\n  %s", desc)
			}
			if resp.Operation.ID != op.ID || resp.Operation.ClaimedBy != "worker-1" {
				rt.Fatalf("granted row is %s claimed_by %q\n  %s", resp.Operation.ID, resp.Operation.ClaimedBy, desc)
			}
			if string(resp.Operation.Params) != string(compactJSON(op.Params)) {
				rt.Fatalf("granted row params changed on the wire: %s\n  %s", resp.Operation.Params, desc)
			}
		default:
			if granted || rpc.Error != "" || !resp.AlreadyClaimed {
				rt.Fatalf("a lost CAS must be already_claimed with no error\n  %s", desc)
			}
		}
	})
	t.Logf("coverage: ws_cases=%d granted=%d declined=%d (foreign_site=%d) get_err=%d list_err=%d claim_err=%d",
		ilStats.wsCases.Load(), ilStats.wsClaimed.Load(), ilStats.wsDeclined.Load(), ilStats.wsForeign.Load(),
		ilStats.wsGetErr.Load(), ilStats.wsListErr.Load(), ilStats.wsClaimErr.Load())
}

// compactJSON is the wire form of a params blob: encoding/json compacts a
// RawMessage when it marshals the enclosing struct.
func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return raw
	}
	return b
}
