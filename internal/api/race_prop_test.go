package api

// Concurrency properties of the operator API.
//
// TestProp_ConcurrentRequestsLinearize fires a generated set of requests at
// one machine simultaneously and requires the outcome (each status code and
// the final machine and operation state) to equal the outcome of SOME
// one-at-a-time ordering of the same requests under a sequential reference
// model. In particular two simultaneous creates must not leave two active
// operations, and no interleaving may answer 5xx.
//
// TestProp_PatchInterleavings covers the two PATCH handlers. A request body
// that blocks its first Read parks the handler before it has bound the
// request, so the history that runs while the PATCH is in flight is
// generated deterministically instead of raced for.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ---------------------------------------------------------------- sequential model of one machine

type cmState struct {
	state     model.MachineState
	locked    bool
	pxe       bool
	desiredOS string
	hasKey    bool
	active    bool // an active (pending) operation exists
	created   int  // operations created so far
}

type cmReq struct {
	kind string // deploy commission power boot abort lock unlock activate deactivate patch
	arg  string
}

func (q cmReq) http(id uuid.UUID) propReq {
	base := "/api/v1/machines/" + id.String()
	switch q.kind {
	case "deploy":
		return propReq{method: http.MethodPost, path: base + "/deploy", body: `{"destructive":true}`}
	case "commission":
		return propReq{method: http.MethodPost, path: base + "/commission", body: `{}`}
	case "power":
		return propReq{method: http.MethodPost, path: base + "/power", body: `{"action":"cycle"}`}
	case "boot":
		return propReq{method: http.MethodPost, path: base + "/boot", body: `{"device":"pxe"}`}
	case "patch":
		return propReq{method: http.MethodPatch, path: base, body: `{"desired_os":"` + q.arg + `"}`}
	default: // abort lock unlock activate deactivate
		return propReq{method: http.MethodPost, path: base + "/" + q.kind}
	}
}

// apply is the one-at-a-time meaning of a request (no NetBox, no claims:
// every operation stays pending, so abort never moves machine state).
func (s cmState) apply(q cmReq) (cmState, int) {
	create := func(sources []model.MachineState, lifecycle bool) (cmState, int) {
		if lifecycle && (s.locked || !s.pxe || !pmStateIn(s.state, sources)) {
			return s, http.StatusConflict
		}
		if s.active {
			return s, http.StatusConflict
		}
		s.active = true
		s.created++
		return s, http.StatusCreated
	}
	switch q.kind {
	case "deploy":
		if !s.hasKey {
			return s, http.StatusFailedDependency
		}
		return create(pmLifecycleSources(model.OpDeploy), true)
	case "commission":
		return create(pmLifecycleSources(model.OpCommission), true)
	case "power", "boot":
		return create(nil, false)
	case "abort":
		s.active = false
	case "lock":
		s.locked = true
	case "unlock":
		s.locked = false
	case "activate":
		if s.locked { // a locked machine cannot be PXE-enabled
			return s, http.StatusConflict
		}
		s.pxe = true
	case "deactivate":
		s.pxe = false
	case "patch":
		s.desiredOS = q.arg
	}
	return s, http.StatusOK
}

type cmObserved struct {
	locked, pxe bool
	desiredOS   string
	state       model.MachineState
	active      int
	total       int
}

func (s cmState) matches(o cmObserved) bool {
	active := 0
	if s.active {
		active = 1
	}
	return s.locked == o.locked && s.pxe == o.pxe && s.desiredOS == o.desiredOS && s.state == o.state &&
		active == o.active && s.created == o.total
}

// cmLinearizable reports whether some ordering of reqs, applied one at a
// time from init, yields exactly codes (per request) and the observed state.
func cmLinearizable(init cmState, reqs []cmReq, codes []int, obs cmObserved) bool {
	used := make([]bool, len(reqs))
	var rec func(s cmState, done int) bool
	rec = func(s cmState, done int) bool {
		if done == len(reqs) {
			return s.matches(obs)
		}
		for i := range reqs {
			if used[i] {
				continue
			}
			next, code := s.apply(reqs[i])
			if code != codes[i] {
				continue
			}
			used[i] = true
			if rec(next, done+1) {
				return true
			}
			used[i] = false
		}
		return false
	}
	return rec(init, 0)
}

func cmObserve(t *rapid.T, env *propEnv, id uuid.UUID) cmObserved {
	m, err := env.st.GetMachine(id)
	if err != nil {
		t.Fatalf("read machine: %v", err)
	}
	ops, err := env.st.ListOperations(&id)
	if err != nil {
		t.Fatalf("read ops: %v", err)
	}
	o := cmObserved{locked: m.Locked, pxe: m.PXEEligible, desiredOS: m.DesiredOS, state: m.State, total: len(ops)}
	for _, op := range ops {
		if op.Status == model.OpPending || op.Status == model.OpClaimed || op.Status == model.OpRunning {
			o.active++
		}
	}
	return o
}

func TestProp_ConcurrentRequestsLinearize(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)
	kinds := []string{
		"deploy", "deploy", "deploy", "commission", "commission", "power", "power", "boot", "abort",
		"lock", "unlock", "activate", "deactivate", "patch", "patch",
	}

	propCheck(t, 25, func(rt *rapid.T) {
		env := newPropEnv(t, nil)
		init := cmState{
			state: rapid.SampledFrom([]model.MachineState{
				model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateCommissioned,
				model.MachineStateFailed, model.MachineStateRunning,
			}).Draw(rt, "state"),
			locked: rapid.IntRange(0, 4).Draw(rt, "locked") == 0,
			pxe:    rapid.IntRange(0, 4).Draw(rt, "pxe") > 0,
			hasKey: rapid.IntRange(0, 9).Draw(rt, "key") > 0,
		}
		id := uuid.New()
		if err := env.st.CreateMachine(&model.Machine{ID: id, NetBoxSerial: "SN-RACE", State: init.state, Locked: init.locked, PXEEligible: init.pxe}); err != nil {
			rt.Fatalf("seed: %v", err)
		}
		if init.hasKey {
			if err := env.st.CreateSSHKey(&model.SSHKey{PublicKey: propSSHKey(0)}); err != nil {
				rt.Fatalf("seed key: %v", err)
			}
		}
		n := rapid.IntRange(2, 6).Draw(rt, "n")
		reqs := make([]cmReq, n)
		for i := range reqs {
			reqs[i].kind = rapid.SampledFrom(kinds).Draw(rt, fmt.Sprintf("kind%d", i))
			if reqs[i].kind == "patch" {
				reqs[i].arg = fmt.Sprintf("os-%d", i)
			}
		}

		codes := make([]int, n)
		bodies := make([]string, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range reqs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp := env.do(reqs[i].http(id))
				codes[i], bodies[i] = resp.code, string(resp.body)
			}()
		}
		close(start)
		wg.Wait()

		created := 0
		for i, c := range codes {
			stats.add(fmt.Sprintf("%s -> %d", reqs[i].kind, c))
			if c >= 500 {
				rt.Fatalf("%s answered %d under concurrency: %s\n  requests: %v", reqs[i].kind, c, bodies[i], reqs)
			}
			if c == http.StatusCreated {
				created++
			}
		}
		obs := cmObserve(rt, env, id)
		if obs.active > 1 {
			rt.Fatalf("machine has %d active operations after concurrent requests %v (codes %v)", obs.active, reqs, codes)
		}
		if obs.total != created {
			rt.Fatalf("%d operation rows exist but %d requests answered 201: %v (codes %v)", obs.total, created, reqs, codes)
		}
		if got := env.pub.deltaCount(); got != created {
			rt.Fatalf("%d op_delta published for %d created operations", got, created)
		}
		if !cmLinearizable(init, reqs, codes, obs) {
			rt.Fatalf("no one-at-a-time ordering explains the outcome\n  initial: %+v\n  requests: %v\n  codes: %v\n  observed: %+v",
				init, reqs, codes, obs)
		}
		if created > 1 {
			stats.add("several creates succeeded (an abort ran between them)")
		}
		conflicts := 0
		for i, c := range codes {
			if c == http.StatusConflict && (reqs[i].kind == "deploy" || reqs[i].kind == "commission" || reqs[i].kind == "power" || reqs[i].kind == "boot") {
				conflicts++
			}
		}
		if conflicts > 0 && created > 0 {
			stats.add("a create won and a simultaneous create was refused")
		}
		stats.add("cases")
	})
}

// ---------------------------------------------------------------- gated PATCH interleavings

// gatedBody is a request body whose first Read blocks until release is
// closed. reached is closed when that first Read begins, which is after the
// handler has loaded the row it is about to modify.
type gatedBody struct {
	r       io.Reader
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedBody(body string) *gatedBody {
	return &gatedBody{r: strings.NewReader(body), reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.reached) })
	<-g.release
	return g.r.Read(p)
}

// patchAround runs a PATCH whose handler is parked between loading its row
// and writing it back while middle runs, then lets it finish.
func patchAround(t interface{ Fatalf(string, ...any) }, env *propEnv, path, body string, middle func()) propResp {
	g := newGatedBody(body)
	req := httptest.NewRequest(http.MethodPatch, path, g)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+propOperatorToken)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.h.ServeHTTP(w, req)
	}()
	select {
	case <-g.reached:
	case <-done:
		t.Fatalf("PATCH %s finished without reading its body: %d %s", path, w.Code, w.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("PATCH %s never read its body", path)
	}
	middle()
	close(g.release)
	<-done
	return propResp{code: w.Code, body: w.Body.Bytes()}
}

// TestProp_PatchInterleavings: a PATCH that changes desired_os (or a
// site-config field) must leave every column it does not own exactly as the
// writes that completed while it was in flight left them.
func TestProp_PatchInterleavings(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)
	propCheck(t, 20, func(rt *rapid.T) {
		env := newPropEnv(t, nil)
		violation := rt.Fatalf

		if rapid.Bool().Draw(rt, "siteConfig") {
			// PATCH /site-configs/:slug while the snapshot dispatcher bumps
			// the site's snapshot sequence (store.BumpSiteSnapshotMeta, the
			// only writer of last_snapshot_seq/last_snapshot_hash).
			r := env.post("/api/v1/site-configs", `{"slug":"pop-a","ephemeral_pool_start":"10.0.0.10","ephemeral_pool_end":"10.0.0.250"}`)
			if r.code != http.StatusCreated {
				rt.Fatalf("seed site-config: %s", r)
			}
			pre := rapid.IntRange(0, 3).Draw(rt, "bumpsBefore")
			mid := rapid.IntRange(0, 3).Draw(rt, "bumpsDuring")
			seq := int64(0)
			hash := ""
			bump := func(tag string, i int) {
				hash = fmt.Sprintf("%s-%d", tag, i)
				n, err := env.st.BumpSiteSnapshotMeta("pop-a", hash)
				if err != nil {
					rt.Fatalf("bump: %v", err)
				}
				seq = n
			}
			for i := range pre {
				bump("before", i)
			}
			resp := patchAround(rt, env, "/api/v1/site-configs/pop-a", `{"promiscuous":true}`, func() {
				for i := range mid {
					bump("during", i)
				}
			})
			if resp.code != http.StatusOK {
				rt.Fatalf("PATCH site-config: %s", resp)
			}
			got, err := env.st.GetSiteConfig("pop-a")
			if err != nil {
				rt.Fatalf("read site-config: %v", err)
			}
			if !got.Promiscuous {
				rt.Fatalf("PATCH site-config did not apply promiscuous=true")
			}
			if got.LastSnapshotSeq != seq || got.LastSnapshotHash != hash {
				violation("site-config snapshot sequence rolled back: last_snapshot_seq=%d hash=%q after the PATCH, the dispatcher had advanced them to %d %q (%d bump(s) while the PATCH was in flight)",
					got.LastSnapshotSeq, got.LastSnapshotHash, seq, hash, mid)
			}
			stats.add(fmt.Sprintf("site-config case, bumps during=%d", mid))
			return
		}

		// PATCH /machines/:id while the machine's lifecycle moves.
		state := rapid.SampledFrom([]model.MachineState{model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateFailed}).Draw(rt, "state")
		id := uuid.New()
		if err := env.st.CreateMachine(&model.Machine{ID: id, NetBoxSerial: "SN-PATCH", State: state, PXEEligible: true}); err != nil {
			rt.Fatalf("seed: %v", err)
		}
		if err := env.st.CreateSSHKey(&model.SSHKey{PublicKey: propSSHKey(0)}); err != nil {
			rt.Fatalf("seed key: %v", err)
		}
		// Reference: the same middle steps with no PATCH in flight, on a
		// twin machine in the same starting state.
		twin := uuid.New()
		if err := env.st.CreateMachine(&model.Machine{ID: twin, NetBoxSerial: "SN-TWIN", State: state, PXEEligible: true}); err != nil {
			rt.Fatalf("seed twin: %v", err)
		}
		// Targeted lifecycle patterns embedded in a random prefix and suffix:
		// a uniformly random step list rarely claims and completes in order.
		any := rapid.SampledFrom([]string{
			"commission", "deploy", "claim", "claim", "complete", "complete", "fail", "abort", "lock", "unlock", "activate", "deactivate",
		})
		pattern := rapid.SampledFrom([][]string{
			{},
			{"commission", "claim"},
			{"commission", "claim", "complete"},
			{"commission", "claim", "complete", "deploy", "claim"},
			{"commission", "claim", "complete", "deploy", "claim", "complete"},
			{"deploy", "claim"},
			{"deploy", "claim", "complete"},
			{"deploy", "claim", "fail"},
			{"lock"},
		}).Draw(rt, "pattern")
		steps := rapid.SliceOfN(any, 0, 2).Draw(rt, "prefix")
		steps = append(steps, pattern...)
		steps = append(steps, rapid.SliceOfN(any, 0, 2).Draw(rt, "suffix")...)
		if len(steps) == 0 {
			steps = []string{"lock"}
		}
		// The PATCH loads its row after the first `split` steps and writes
		// it back after the rest.
		split := rapid.IntRange(0, len(steps)-1).Draw(rt, "split")
		run := func(mid uuid.UUID, steps []string) {
			base := "/api/v1/machines/" + mid.String()
			activeOp := func() *model.Operation {
				ops, err := env.st.ListOperations(&mid)
				if err != nil {
					rt.Fatalf("list ops: %v", err)
				}
				for i := range ops {
					if ops[i].Status == model.OpPending || ops[i].Status == model.OpClaimed {
						return &ops[i]
					}
				}
				return nil
			}
			for _, s := range steps {
				switch s {
				case "commission":
					env.post(base+"/commission", `{}`)
				case "deploy":
					env.post(base+"/deploy", `{"destructive":true}`)
				case "claim":
					if op := activeOp(); op != nil {
						_, _ = env.st.ClaimOperation(op.ID, "w1")
					}
				case "complete":
					if op := activeOp(); op != nil && op.Status == model.OpClaimed {
						if op.Type == model.OpCommission {
							_, _, _ = env.st.CommitCommissionResult(op.ID, mid, json.RawMessage(`{"system":{"manufacturer":"P","product_name":"M"}}`))
						} else {
							_, _ = env.st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil)
						}
					}
				case "fail":
					if op := activeOp(); op != nil {
						_ = env.st.FailOperation(op.ID, []byte(`{"error":"x"}`))
					}
				default: // abort lock unlock activate deactivate
					env.post(base+"/"+s, "")
				}
			}
		}
		run(twin, steps)
		run(id, steps[:split])
		resp := patchAround(rt, env, "/api/v1/machines/"+id.String(), `{"desired_os":"noble"}`, func() { run(id, steps[split:]) })
		if resp.code != http.StatusOK {
			rt.Fatalf("PATCH machine: %s", resp)
		}
		got, err := env.st.GetMachine(id)
		if err != nil {
			rt.Fatalf("read machine: %v", err)
		}
		want, err := env.st.GetMachine(twin)
		if err != nil {
			rt.Fatalf("read twin: %v", err)
		}
		if got.DesiredOS != "noble" {
			rt.Fatalf("PATCH did not apply desired_os")
		}
		changed := want.State != state || want.Locked || !want.PXEEligible || len(want.CommissionData) > 0
		if changed {
			stats.add("machine case: the middle steps changed the row")
		}
		if got.State != want.State {
			violation("machine state reverted: state=%s after the PATCH, the steps that completed while it was in flight (%v) leave %s", got.State, steps, want.State)
		}
		if got.Locked != want.Locked {
			violation("machine lock reverted: locked=%v after the PATCH, steps %v leave locked=%v", got.Locked, steps, want.Locked)
		}
		if got.PXEEligible != want.PXEEligible {
			violation("machine pxe_eligible reverted: %v after the PATCH, steps %v leave %v", got.PXEEligible, steps, want.PXEEligible)
		}
		if (len(got.CommissionData) > 0) != (len(want.CommissionData) > 0) {
			violation("machine commission_data reverted: present=%v after the PATCH, steps %v leave present=%v", len(got.CommissionData) > 0, steps, len(want.CommissionData) > 0)
		}
		// The stuck-machine consequence, stated on its own: a transient state
		// with no operation driving it cannot be left through the API.
		if got.State == model.MachineStateDeploying || got.State == model.MachineStateCommissioning {
			ops, _ := env.st.ListOperations(&id)
			driving := false
			for _, op := range ops {
				if op.Status == model.OpClaimed || op.Status == model.OpRunning {
					driving = true
				}
			}
			if !driving {
				violation("machine stuck: state=%s with no claimed operation after steps %v", got.State, steps)
			}
		}
		stats.add("machine case")
	})
}
