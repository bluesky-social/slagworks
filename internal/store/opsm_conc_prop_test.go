package store

// Concurrency half of the op/machine harness: a statement-level interleaving
// explorer.
//
// Two store calls run in two goroutines against the real Postgres. A gorm
// callback parks each goroutine before every SQL statement it is about to
// send; a scheduler decides which goroutine may send its next statement, so
// a schedule is an explicit, replayable sequence of choices rather than
// whatever the Go runtime happens to do. A goroutine that blocks inside
// Postgres on a row lock is detected through pg_stat_activity and the other
// goroutine is run until the lock is released, exactly as it would in
// production.
//
// The oracle is linearizability against the reference model: the two
// observed results and the final database state must equal what the model
// predicts for "A then B" or for "B then A". Anything else is a hybrid.
//
//	go test ./internal/store/ -run TestOpsmConc -count=1 -v

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// --- the scheduler ------------------------------------------------------------

type schedActorKey struct{}

type schedEvent struct {
	actor int
	done  bool
}

type schedActor struct {
	idx   int
	name  string
	evt   chan schedEvent
	grant chan struct{}
	stop  <-chan struct{} // closed when the scheduler gives up on the schedule

	mu    sync.Mutex
	stmts []string // SQL this actor has executed, in order (from the gorm logger)
}

// atStatement parks the calling goroutine until the scheduler lets it send
// its next statement. Once the scheduler has given up it no longer parks:
// the statement runs on a canceled context and fails, so the actor unwinds
// and releases whatever locks it holds.
func (a *schedActor) atStatement() {
	select {
	case a.evt <- schedEvent{actor: a.idx}:
	case <-a.stop:
		return
	}
	select {
	case <-a.grant:
	case <-a.stop:
	}
}

var schedHooksOnce sync.Once

// installSchedHooks registers the parking callback in front of every gorm
// statement kind. It is inert for statements whose context carries no actor,
// i.e. for every other test in this package.
func installSchedHooks(db *gorm.DB) {
	schedHooksOnce.Do(func() {
		hook := func(tx *gorm.DB) {
			if tx.Statement == nil || tx.Statement.Context == nil {
				return
			}
			if a, ok := tx.Statement.Context.Value(schedActorKey{}).(*schedActor); ok && a != nil {
				a.atStatement()
			}
		}
		cb := db.Callback()
		must := func(err error) {
			if err != nil {
				panic(fmt.Sprintf("install scheduler hook: %v", err))
			}
		}
		must(cb.Create().Before("*").Register("opsm:sched", hook))
		must(cb.Query().Before("*").Register("opsm:sched", hook))
		must(cb.Update().Before("*").Register("opsm:sched", hook))
		must(cb.Delete().Before("*").Register("opsm:sched", hook))
		must(cb.Row().Before("*").Register("opsm:sched", hook))
		must(cb.Raw().Before("*").Register("opsm:sched", hook))
	})
}

// stmtLogger records each executed statement on its actor, so a failing
// schedule can be printed as the interleaved SQL it actually was.
type stmtLogger struct{ logger.Interface }

func (l stmtLogger) LogMode(logger.LogLevel) logger.Interface { return l }

func (l stmtLogger) Trace(ctx context.Context, _ time.Time, fc func() (string, int64), err error) {
	a, ok := ctx.Value(schedActorKey{}).(*schedActor)
	if !ok || a == nil {
		return
	}
	sqlText, rows := fc()
	if len(sqlText) > 110 {
		sqlText = sqlText[:110] + "..."
	}
	line := fmt.Sprintf("%s (rows=%d)", sqlText, rows)
	if err != nil {
		line += " ERR " + err.Error()
	}
	a.mu.Lock()
	a.stmts = append(a.stmts, line)
	a.mu.Unlock()
	schedTraceMu.Lock()
	schedTrace = append(schedTrace, a.name+": "+line)
	schedTraceMu.Unlock()
}

var (
	schedTraceMu sync.Mutex
	schedTrace   []string // global order of executed statements for the run in progress
)

const (
	opsmStRunning = iota // executing Go code or a statement; an event will follow
	opsmStParked         // parked before a statement, waiting for a grant
	opsmStBlocked        // inside Postgres, waiting on a lock another actor holds
	opsmStDone
)

// interleave runs fns concurrently under the scheduler. choose is called at
// every point where more than one actor could go next, with the parked
// actors' indices, and returns the index (into that slice) to run. It
// returns the number of choice points and the grant sequence.
func opsmInterleave(t opsmT, base *gorm.DB, names []string, fns []func(st *Store), choose func(parked []int) int) (grants []int) {
	installSchedHooks(base)
	sqlDB, err := base.DB()
	if err != nil {
		t.Fatalf("interleave: sql.DB: %v", err)
	}
	schedTraceMu.Lock()
	schedTrace = nil
	schedTraceMu.Unlock()

	evt := make(chan schedEvent, len(fns)*2)
	actors := make([]*schedActor, len(fns))
	state := make([]int, len(fns))
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// giveUp fails the test after making sure no actor outlives the
	// schedule: canceling the context aborts in-flight SQL and unparks every
	// actor, and the drain waits for their transactions to roll back so the
	// next case does not start behind a leftover row lock.
	giveUp := func(format string, args ...any) {
		cancel()
		drain := time.After(10 * time.Second)
		for remaining := len(state) - opsmCountState(state, opsmStDone); remaining > 0; {
			select {
			case e := <-evt:
				if e.done {
					remaining--
				}
			case <-drain:
				remaining = 0
			}
		}
		t.Fatalf(format, args...)
	}
	for i := range fns {
		a := &schedActor{idx: i, name: names[i], evt: evt, grant: make(chan struct{}), stop: runCtx.Done()}
		actors[i] = a
		ctx := context.WithValue(runCtx, schedActorKey{}, a)
		adb := base.Session(&gorm.Session{Context: ctx, Logger: stmtLogger{logger.Discard}})
		st := New(adb)
		fn := fns[i]
		state[i] = opsmStRunning
		go func() {
			defer func() { evt <- schedEvent{actor: a.idx, done: true} }()
			fn(st)
		}()
	}

	deadline := time.Now().Add(30 * time.Second)
	apply := func(e schedEvent) {
		if e.done {
			state[e.actor] = opsmStDone
		} else {
			state[e.actor] = opsmStParked
		}
	}
	count := func(want int) int { return opsmCountState(state, want) }
	for {
		// Settle: wait until every actor is parked, done, or blocked in
		// Postgres (and Postgres agrees about how many are blocked).
		for {
			if time.Now().After(deadline) {
				giveUp("interleave: schedule did not finish within 30s (states=%v)", state)
			}
			if count(opsmStRunning) == 0 && count(opsmStBlocked) == 0 {
				break
			}
			select {
			case e := <-evt:
				apply(e)
				continue
			case <-time.After(500 * time.Microsecond):
			}
			waiters := opsmLockWaiters(sqlDB)
			if count(opsmStRunning) == 0 && waiters >= count(opsmStBlocked) {
				break // everyone accounted for
			}
			if count(opsmStRunning) > 0 && waiters > count(opsmStBlocked) {
				// A running actor is in fact waiting on a row lock. With
				// several running, mark as many as Postgres reports.
				need := waiters - count(opsmStBlocked)
				for i := range state {
					if need > 0 && state[i] == opsmStRunning {
						state[i] = opsmStBlocked
						need--
					}
				}
			}
		}
		var parked []int
		for i, s := range state {
			if s == opsmStParked {
				parked = append(parked, i)
			}
		}
		if len(parked) == 0 {
			if count(opsmStDone) == len(state) {
				return grants
			}
			// Only lock-blocked actors remain (they wait on each other, or
			// on a lock about to be released). Wait for Postgres to resolve.
			select {
			case e := <-evt:
				apply(e)
			case <-time.After(5 * time.Second):
				giveUp("interleave: all remaining actors are blocked in Postgres and none resumed within 5s (states=%v)", state)
			}
			continue
		}
		pick := parked[0]
		if len(parked) > 1 {
			pick = parked[choose(parked)]
		}
		grants = append(grants, pick)
		state[pick] = opsmStRunning
		actors[pick].grant <- struct{}{}
		// Give the granted actor a chance to report before re-settling, so
		// a quick statement is seen as "parked again", not "running".
		select {
		case e := <-evt:
			apply(e)
		case <-time.After(200 * time.Microsecond):
		}
	}
}

func opsmCountState(state []int, want int) (n int) {
	for _, s := range state {
		if s == want {
			n++
		}
	}
	return n
}

// opsmLockWaiters counts sessions currently waiting on a heavyweight lock.
func opsmLockWaiters(db *sql.DB) int {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// opsmSegmentChooser turns a context-bounded plan into a choose function: run
// actor `first` for segs[0] statements, then the other for segs[1], and so
// on; once the plan is exhausted the current actor runs to completion.
func opsmSegmentChooser(first int, segs []int) func(parked []int) int {
	cur, seg, used := first, 0, 0
	return func(parked []int) int {
		for seg < len(segs) && used >= segs[seg] {
			seg++
			used = 0
			cur = 1 - cur
		}
		used++
		for i, p := range parked {
			if p == cur {
				return i
			}
		}
		return 0
	}
}

// --- linearizability check ------------------------------------------------------

// opsmPairResult is one explored schedule of one pair.
type opsmPairResult struct {
	ok       bool
	report   string
	stmts    int
	switches int
}

// runPair executes a and b concurrently under choose on top of the state the
// harness is in, and checks the outcome against both sequential orders of
// the reference model. The harness must not be used afterwards.
func (h *opsmHarness) runPair(a, b opsmAction, choose func(parked []int) int) opsmPairResult {
	var got [2]opsmObs
	acts := [2]opsmAction{a, b}
	fns := make([]func(*Store), 2)
	for i := range acts {
		i := i
		fns[i] = func(st *Store) { got[i] = opsmNormalizeObs(h.applyReal(st, acts[i])) }
	}
	grants := opsmInterleave(h.t, h.st.db, []string{"A", "B"}, fns, choose)

	machines, ops, envs := h.readDB()
	res := opsmPairResult{stmts: len(grants)}
	for i := 1; i < len(grants); i++ {
		if grants[i] != grants[i-1] {
			res.switches++
		}
	}
	var expl []string
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		m := h.ref.clone()
		var want [2]opsmObs
		for _, i := range order {
			act := acts[i]
			want[i] = opsmNormalizeObs(opsmApplyRef(m, act, h.machineOrPhantom(act.Slot), h.machineOrPhantom(act.Slot2), got[i]))
		}
		diff := ""
		switch {
		case !reflect.DeepEqual(got[0], want[0]):
			diff = fmt.Sprintf("A observed %+v, model %+v", got[0], want[0])
		case !reflect.DeepEqual(got[1], want[1]):
			diff = fmt.Sprintf("B observed %+v, model %+v", got[1], want[1])
		default:
			diff = opsmStateDiff(m, machines, ops, envs)
		}
		if diff == "" {
			res.ok = true
			if d := opsmStructuralDiff(h.strict, machines, ops); d != "" {
				res.ok = false
				res.report = "linearizable but structurally invalid: " + d
			}
			break
		}
		expl = append(expl, fmt.Sprintf("  not %s-then-%s: %s", string(rune('A'+order[0])), string(rune('A'+order[1])), diff))
	}
	if !res.ok {
		schedTraceMu.Lock()
		tr := append([]string(nil), schedTrace...)
		schedTraceMu.Unlock()
		if res.report == "" {
			res.report = "outcome matches neither sequential order (a hybrid)"
		}
		res.report = fmt.Sprintf("%s\n  A = %s\n  B = %s\n%s\n--- prefix history ---\n%s\n--- interleaved SQL ---\n%s",
			res.report, a, b, strings.Join(expl, "\n"), strings.Join(h.trace, "\n"), strings.Join(tr, "\n"))
	}
	return res
}

// --- pair generation ------------------------------------------------------------

// opsmConcCandidates lists the calls worth racing on the machine in slot 0
// and its latest op.
func opsmConcCandidates(op uuid.UUID) []opsmAction {
	return []opsmAction{
		{Kind: kClaim, Op: op, Worker: "w1"},
		{Kind: kClaim, Op: op, Worker: "w2"},
		{Kind: kSetPhase, Op: op, Phase: "partitioning"},
		{Kind: kSetPhase, Op: op, Phase: "awaiting_phone_home"},
		{Kind: kHubComplete, Op: op, Result: 5},
		{Kind: kFail, Op: op, Result: 1},
		{Kind: kCommit, Op: op, Slot2: 0, Data: 0},
		{Kind: kCommit, Op: op, Slot2: 0, Data: 2},
		{Kind: kAbort, Slot: 0},
		{Kind: kDriftFail, Slot: 0, Text: "drift"},
		{Kind: kMirrorDelete, Slot: 0},
		{Kind: kExpirePending, Window: opsmSweepAll},
		{Kind: kExpireStuck, Window: opsmSweepAll},
		{Kind: kExpireOrphans, Window: opsmSweepAll},
		{Kind: kExpirePhoneHome, Window: opsmSweepAll},
		{Kind: kSetLocked, Slot: 0, Flag: true},
		{Kind: kSetLocked, Slot: 0, Flag: false},
		{Kind: kSetPXE, Slot: 0, Flag: false},
		{Kind: kPatch, Slot: 0, Text: "noble"},
		{Kind: kRebind, Slot: 0, Device: 13},
		{Kind: kCreateLifecycle, Slot: 0, Type: "deploy", Allowed: opsmAPIAllowedSources["deploy"], Params: 2},
		{Kind: kCreateLifecycle, Slot: 0, Type: "commission", Allowed: opsmAPIAllowedSources["commission"], Params: 2},
		{Kind: kCreatePower, Slot: 0, Type: "power_cycle", Params: 2},
		{Kind: kScrub},
	}
}

// opsmAcceptedRace describes why a pair is not required to be linearizable,
// or returns "" when it is. Each entry is a behavior the store accepts on
// purpose; the random search skips those pairs.
func opsmAcceptedRace(a, b opsmAction) string {
	has := func(k opsmKind) bool { return a.Kind == k || b.Kind == k }
	switch {
	case has(kAbort) && has(kMirrorDelete):
		// AbortMachine re-reads the machine row after failing its ops; a
		// delete in between turns a successful abort into a not-found
		// error. The row is gone either way.
		return "abort re-reads a machine the mirror just deleted"
	case has(kHubComplete) && has(kCreateLifecycle):
		// CreateMachineOp reads state/locked without blocking writers. A
		// completion that commits between that read and the insert (only
		// possible for a deploy op completed while still pending) leaves a
		// new pending lifecycle op on a machine that just became running
		// and locked. The create-time gate is a fast-fail; ClaimOperation
		// re-checks state and lock under a row lock and refuses, so the op
		// can never actuate.
		return "create-time state gate is advisory; the claim gate holds"
	}
	return ""
}

// opsmConcGoals are the starting situations for slot 0.
var opsmConcGoals = []string{
	"new", "pending_commission", "commissioning", "commissioned", "pending_deploy",
	"deploying", "awaiting_phone_home", "running", "running_unlocked", "failed",
	"recommissioning", "power_pending", "power_claimed", "running_bmc_reconcile",
}

// opsmIsBatchKind reports whether a call acts on every matching operation in
// the table rather than on one machine or one op.
func opsmIsBatchKind(k opsmKind) bool {
	switch k {
	case kExpirePending, kExpireStuck, kExpireOrphans, kExpirePhoneHome, kScrub:
		return true
	default:
		return false
	}
}

// opsmBystanderGoal picks the situation of the second machine (slot 1). It has a
// claimed deploy of its own, so a write that leaks across machines is
// visible; unless one of the racing calls is a table-wide batch, where a
// second in-flight op would only show that a batch is not atomic ACROSS ops
// (each op is still failed exactly once), which is not a contract.
func opsmBystanderGoal(a, b opsmAction) string {
	if opsmIsBatchKind(a.Kind) || opsmIsBatchKind(b.Kind) {
		return "commissioned"
	}
	return "deploying"
}

// opsmSetupPair builds the starting state for a pair: slot 0 fast-forwarded to
// goal, plus (when bystander is non-empty) a second machine in slot 1.
func opsmSetupPair(t opsmT, st *Store, goal, bystander string) (*opsmHarness, uuid.UUID) {
	h := newOpsmHarness(t, st, true)
	h.noReadback = true
	g := &opsmGen{h: h}
	g.advance(0, goal)
	if bystander != "" {
		g.advance(1, bystander)
	}
	op := g.lastOp[0]
	if op == uuid.Nil {
		op = opsmBogusOp
	}
	return h, op
}

var opsmConcStats = struct {
	sync.Mutex
	schedules, statements, switches int
	skippedAccepted                 int
	byPair                          map[string]int
}{byPair: map[string]int{}}

func opsmRecordPair(a, b opsmAction, res opsmPairResult) {
	opsmConcStats.Lock()
	defer opsmConcStats.Unlock()
	opsmConcStats.schedules++
	opsmConcStats.statements += res.stmts
	opsmConcStats.switches += res.switches
	k1, k2 := string(a.Kind), string(b.Kind)
	if k2 < k1 {
		k1, k2 = k2, k1
	}
	opsmConcStats.byPair[k1+"+"+k2]++
}

func opsmConcReport() string {
	opsmConcStats.Lock()
	defer opsmConcStats.Unlock()
	return fmt.Sprintf("schedules=%d distinct kind pairs=%d statements granted=%d context switches=%d skipped (accepted races)=%d",
		opsmConcStats.schedules, len(opsmConcStats.byPair), opsmConcStats.statements, opsmConcStats.switches, opsmConcStats.skippedAccepted)
}

// TestOpsmConc_RandomPairs draws a starting situation, two calls and a
// context-bounded schedule, and requires the outcome to be linearizable.
func TestOpsmConc_RandomPairs(t *testing.T) {
	st := testStore(t)
	t.Cleanup(func() { t.Logf("concurrency coverage: %s", opsmConcReport()) })
	rapid.Check(t, func(rt *rapid.T) {
		goal := rapid.SampledFrom(opsmConcGoals).Draw(rt, "goal")
		// The candidate list is positional, so the pair can be drawn
		// before the state exists.
		n := len(opsmConcCandidates(opsmBogusOp))
		ai := rapid.IntRange(0, n-1).Draw(rt, "a")
		bi := rapid.IntRange(0, n-1).Draw(rt, "b")
		proto := opsmConcCandidates(opsmBogusOp)
		if why := opsmAcceptedRace(proto[ai], proto[bi]); why != "" {
			opsmConcStats.Lock()
			opsmConcStats.skippedAccepted++
			opsmConcStats.Unlock()
			rt.Skip("accepted race: " + why)
		}
		bystander := ""
		if rapid.Bool().Draw(rt, "bystander") {
			bystander = opsmBystanderGoal(proto[ai], proto[bi])
		}
		h, op := opsmSetupPair(rt, st, goal, bystander)
		cands := opsmConcCandidates(op)
		a, b := cands[ai], cands[bi]
		first := rapid.IntRange(0, 1).Draw(rt, "first")
		segs := rapid.SliceOfN(rapid.IntRange(0, 7), 1, 4).Draw(rt, "segments")
		res := h.runPair(a, b, opsmSegmentChooser(first, segs))
		opsmRecordPair(a, b, res)
		if !res.ok {
			rt.Fatalf("goal=%s first=%d segments=%v: %s", goal, first, segs, res.report)
		}
	})
}
