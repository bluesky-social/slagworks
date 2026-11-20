//go:build integration

package qemutest

// Histories: an operation/fault alphabet over one or two hosts on one site
// worker, a backend-independent interpreter, and the oracle.
//
// A history is a list of steps. A step either runs immediately, in order,
// or is ARMED on a moment — "after the worker serves host A a deploy boot",
// "before the worker handles A's deploy_complete report" — and runs when
// that request passes through the worker's HTTP boot server. Moments are
// what make the delicate interleavings (between deploy_complete and the
// first disk boot, during the first disk boot before phone-home, ...)
// reachable on purpose instead of by luck.
//
// The oracle only looks at the system from outside: the boot scripts and
// report statuses the worker actually served (propHTTPEvent), what global's
// store did and why (propHubEvent), the power actuations the executor
// issued, and the hosts' disks. Its properties:
//
//   - P1 installer boots: an installer is served only for an active
//     lifecycle op of exactly that machine, only by a worker that has
//     resynced since it started, and never once the worker has been told
//     the op ended; a host that never had a lifecycle op never has its disk
//     written.
//   - P2 no re-wipe: no deploy boot for an op after its deploy_complete was
//     accepted.
//   - P3 reports: the worker accepts a report only for that machine's
//     active op (a phone-home retry from an already running machine is
//     acknowledged without effect), a phone-home only after the op's
//     deploy_complete, and a phone-home that names no op never; every
//     deploy global completes traces to that op's own install phoning home
//     after its deploy_complete.
//   - P4 agreement: global's machine state matches what the host did, the
//     worker and global agree once the system is at rest, and no op
//     outlives the documented timeouts.
//   - P5 power: every actuation backs a claimed op that calls for it, the
//     host ends in the power state the last action implies, a completed
//     power op was actuated and a timed-out one was not.
//   - P6 liveness of the worker: it always shuts down, and while up and
//     connected it claims pending ops.
//   - LIVE: after the history, a fresh destructive deploy succeeds.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// propMoment selects a request at the worker's HTTP boot server.
type propMoment struct {
	Before bool   `json:"before,omitempty"`
	Host   int    `json:"host"`
	Kind   string `json:"kind"`            // boot | report | dhcp (a DHCP ACK; Phase dhcp_ack or ipxe_dhcp; before only)
	Boot   string `json:"boot,omitempty"`  // local | deploy | commission
	Phase  string `json:"phase,omitempty"` // report phase
	Event  string `json:"event,omitempty"` // report event
	Nth    int    `json:"nth,omitempty"`
}

func (m propMoment) String() string {
	when := "after"
	if m.Before {
		when = "before"
	}
	what := ""
	switch {
	case m.Kind == "boot":
		what = "boot=" + m.Boot
	case m.Kind == "dhcp":
		what = "dhcp=" + m.Phase
	case m.Event != "":
		what = "event=" + m.Event
	default:
		what = "phase=" + m.Phase
	}
	s := fmt.Sprintf("%s %s(h%d)", when, what, m.Host)
	if m.Nth > 1 {
		s += fmt.Sprintf("#%d", m.Nth)
	}
	return s
}

// propStep is one element of a history.
type propStep struct {
	Do   string      `json:"do"`
	Host int         `json:"host,omitempty"`
	Arg  string      `json:"arg,omitempty"`
	Ms   int         `json:"ms,omitempty"`
	Flag bool        `json:"flag,omitempty"`
	At   *propMoment `json:"at,omitempty"`
}

func (s propStep) String() string {
	var b strings.Builder
	if s.At != nil {
		b.WriteString("@" + s.At.String() + ": ")
	}
	b.WriteString(s.Do)
	fmt.Fprintf(&b, "(h%d", s.Host)
	if s.Arg != "" {
		b.WriteString("," + s.Arg)
	}
	if s.Ms != 0 {
		fmt.Fprintf(&b, ",%dms", s.Ms)
	}
	if s.Flag {
		b.WriteString(",flag")
	}
	b.WriteString(")")
	return b.String()
}

// propSetup is the initial condition of a history.
type propSetup struct {
	// SubjectNew starts host 0 in `new` (never commissioned) instead of
	// `commissioned`.
	SubjectNew bool `json:"subject_new,omitempty"`
	// Bystander adds host 1, which never gets a lifecycle op:
	// running | commissioned | failed | new | pxe_off | unknown ("" = none).
	// pxe_off is a commissioned machine with pxe_eligible=false; unknown is
	// a MAC global has no machine for.
	Bystander string `json:"bystander,omitempty"`
	// VM tier only.
	UEFI  bool `json:"uefi,omitempty"`
	RAID1 bool `json:"raid1,omitempty"`
}

type propHistory struct {
	Name string `json:"name"`
	// About says what a committed history checks.
	About string     `json:"about,omitempty"`
	Seed  int64      `json:"seed,omitempty"`
	Setup propSetup  `json:"setup"`
	Steps []propStep `json:"steps"`
}

func (h propHistory) String() string {
	parts := make([]string, len(h.Steps))
	for i, s := range h.Steps {
		parts[i] = s.String()
	}
	return fmt.Sprintf("setup=%+v: %s", h.Setup, strings.Join(parts, "; "))
}

func (h propHistory) JSON() string {
	b, _ := json.Marshal(h)
	return string(b)
}

// propClaimStall bounds how long a healthy worker may leave a pending op
// unclaimed: the executor's claim RPC timeout (30s, hardcoded in
// worker.Executor.claimOp) plus its 5s tick.
const propClaimStall = 36 * time.Second

// propReconnectWait bounds the wait for the worker to get back on the
// stream once faults are lifted: its reconnect backoff tops out at 30s.
const propReconnectWait = 40 * time.Second

// propTiming is the backend's time scale.
type propTiming struct {
	Poll      time.Duration
	Propagate time.Duration // let a hub-side change reach the worker
	Quiet     time.Duration // no events for this long = trailing activity done
	Idle      time.Duration // no events for this long with an op active = stuck
	Settle    time.Duration // max wait for natural completion
	Live      time.Duration // budget for the final fresh deploy
	Grace     time.Duration // slack for cross-goroutine timestamp comparisons
	// Actuate bounds how long a healthy executor may hold a claimed op
	// before its first power call; until then the op is in progress, not
	// stuck.
	Actuate time.Duration
	// Budget caps a history's wall time (0 = none): past it, remaining
	// steps and the liveness deploy are skipped so the oracle still runs
	// before the test binary's own timeout.
	Budget time.Duration
}

type propViolation struct {
	Class string // P1..P6, LIVE
	Msg   string
}

func (v propViolation) String() string { return v.Class + ": " + v.Msg }

type propRun struct {
	t      *testing.T
	g      *propGlobal
	w      *propWorker
	hosts  []propHost
	ids    []uuid.UUID
	timing propTiming

	faults        sync.WaitGroup
	pendingFaults atomic.Int32

	mu         sync.Mutex
	violations []propViolation
	notes      []string
	stats      map[string]int
	armed      []*propTrigger
	direct     []propPowerAction // hardware faults injected directly (vm_reset/off/on)
	started    time.Time
}

func newPropRun(t *testing.T, g *propGlobal, w *propWorker, hosts []propHost, ids []uuid.UUID, timing propTiming) *propRun {
	return &propRun{t: t, g: g, w: w, hosts: hosts, ids: ids, timing: timing, stats: map[string]int{}, started: time.Now()}
}

func (r *propRun) overBudget() bool {
	return r.timing.Budget > 0 && time.Since(r.started) > r.timing.Budget
}

func (r *propRun) note(format string, args ...any) {
	s := time.Now().Format("15:04:05.000") + " " + fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.notes = append(r.notes, s)
	r.mu.Unlock()
}

func (r *propRun) stat(k string) {
	r.mu.Lock()
	r.stats[k]++
	r.mu.Unlock()
}

// propReportViolations caps how many violations a report spells out: a
// loop produces hundreds of the same one.
const propReportViolations = 20

func (r *propRun) violate(class, format string, args ...any) {
	v := propViolation{Class: class, Msg: fmt.Sprintf(format, args...)}
	r.mu.Lock()
	r.violations = append(r.violations, v)
	n := len(r.violations)
	r.mu.Unlock()
	if n <= propReportViolations {
		r.note("VIOLATION %s", v)
	}
}

func (r *propRun) hostOf(machine string) int {
	for i, id := range r.ids {
		if id.String() == machine {
			return i
		}
	}
	return -1
}

func (r *propRun) hostOfMAC(mac string) int {
	for i, h := range r.hosts {
		if strings.EqualFold(h.MAC(), mac) {
			return i
		}
	}
	return -1
}

func (r *propRun) matcher(m propMoment) func(e *propHTTPEvent) bool {
	return func(e *propHTTPEvent) bool {
		if e.Synthetic || e.Kind != m.Kind {
			return false
		}
		switch m.Kind {
		case "boot":
			return r.hostOfMAC(e.MAC) == m.Host && (m.Before || e.Boot == m.Boot)
		case "dhcp":
			return r.hostOfMAC(e.MAC) == m.Host && e.Phase == m.Phase
		case "report":
			if r.hostOf(e.Machine) != m.Host {
				return false
			}
			if m.Event != "" {
				return e.Event == m.Event
			}
			return e.Phase == m.Phase && e.Event == "" && !e.Failed
		}
		return false
	}
}

// Run executes the history. It never fails the test itself; the caller
// decides what to do with Violations().
func (r *propRun) Run(h propHistory) {
	for i, s := range h.Steps {
		if r.overBudget() {
			r.note("over budget after %s: skipping steps %d..%d", time.Since(r.started).Round(time.Second), i, len(h.Steps)-1)
			r.stat("budget_truncated")
			break
		}
		if s.Do == "await" {
			// Block the history until the moment occurs (or give up).
			r.note("step %d: %s", i, s)
			from := len(r.w.rec.Events())
			if _, ok := r.w.rec.WaitFor(r.matcher(*s.At), from, r.timing.Settle); !ok {
				r.note("  await: moment never occurred")
				r.stat("await_missed")
			}
			continue
		}
		if s.At != nil {
			r.arm(i, s)
			continue
		}
		r.note("step %d: %s", i, s)
		r.do(s, false)
	}
}

func (r *propRun) arm(i int, s propStep) {
	step := s
	step.At = nil
	tr := &propTrigger{
		name:   fmt.Sprintf("step %d: %s", i, s),
		before: s.At.Before,
		match:  r.matcher(*s.At),
		nth:    int32(s.At.Nth),
	}
	tr.action = func(e *propHTTPEvent) {
		r.note("FIRED %s (on %s)", tr.name, e)
		r.stat("fired:" + step.Do + "@" + propMomentKey(*s.At))
		r.do(step, true)
	}
	r.mu.Lock()
	r.armed = append(r.armed, tr)
	r.mu.Unlock()
	r.w.rec.Arm(tr)
	r.note("armed %s", tr.name)
}

func propMomentKey(m propMoment) string {
	when := "after"
	if m.Before {
		when = "before"
	}
	switch {
	case m.Kind == "boot":
		return when + ":boot=" + m.Boot
	case m.Kind == "dhcp":
		return when + ":dhcp=" + m.Phase
	case m.Event != "":
		return when + ":" + m.Event
	default:
		return when + ":" + m.Phase
	}
}

// async runs fn as a tracked background fault.
func (r *propRun) async(fn func()) {
	r.faults.Add(1)
	r.pendingFaults.Add(1)
	go func() {
		defer r.faults.Done()
		defer r.pendingFaults.Add(-1)
		fn()
	}()
}

func (r *propRun) lastTerminalOp(host int) (uuid.UUID, bool) {
	ops := r.g.Ops()
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].MachineID == r.ids[host] && !propActive(ops[i].Status) &&
			(ops[i].Type == model.OpDeploy || ops[i].Type == model.OpCommission) {
			return ops[i].ID, true
		}
	}
	return uuid.Nil, false
}

// do executes one step. inRequest is true when running inside a worker
// HTTP request (an armed step): nothing here may wait for that request.
func (r *propRun) do(s propStep, inRequest bool) {
	r.stat("do:" + s.Do)
	g, w := r.g, r.w
	id := uuid.Nil
	var host propHost
	if s.Host >= 0 && s.Host < len(r.ids) {
		id, host = r.ids[s.Host], r.hosts[s.Host]
	}
	hubSide := false
	switch s.Do {
	case "commission":
		if _, err := g.CreateCommission(id); err != nil {
			r.note("  commission rejected: %v", err)
			r.stat("rejected:commission")
		}
		hubSide = true
	case "deploy":
		if m := g.Machine(id); m.Locked {
			// The operator's redeploy flow: unlock, then deploy.
			g.SetLocked(id, false)
			r.stat("redeploy_unlock")
		}
		if _, err := g.CreateDeploy(id, s.Flag, s.Arg); err != nil {
			r.note("  deploy rejected: %v", err)
			r.stat("rejected:deploy")
		}
		hubSide = true
	case "power":
		t := map[string]model.OperationType{"cycle": model.OpPowerCycle, "off": model.OpPowerOff, "on": model.OpPowerOn}[s.Arg]
		if _, err := g.CreatePower(id, t); err != nil {
			r.note("  power %s rejected: %v", s.Arg, err)
			r.stat("rejected:power")
		}
		hubSide = true
	case "abort":
		n := g.Abort(id)
		r.note("  aborted %d op(s)", n)
		hubSide = true
	case "lock":
		g.SetLocked(id, s.Flag)
		hubSide = true
	case "pxe":
		if err := g.SetPXEEligible(id, s.Flag); err != nil {
			r.note("  pxe rejected: %v", err)
			r.stat("rejected:pxe")
		}
		hubSide = true
	case "vm_reset":
		r.directPower(id, "cycle")
		if err := host.PowerCycle(); err != nil {
			r.note("  vm_reset: %v", err)
		}
	case "vm_off":
		// Power loss. With Ms, power returns after that long; without,
		// the host stays dark until something powers it on.
		r.directPower(id, "off")
		if err := host.PowerOff(); err != nil {
			r.note("  vm_off: %v", err)
		}
		if s.Ms > 0 {
			back := time.Duration(s.Ms) * time.Millisecond
			r.async(func() {
				time.Sleep(back)
				r.directPower(id, "on")
				if err := host.PowerOn(); err != nil {
					r.note("  power restore: %v", err)
				}
			})
		}
	case "vm_on":
		r.directPower(id, "on")
		if err := host.PowerOn(); err != nil {
			r.note("  vm_on: %v", err)
		}
	case "worker_kill":
		// The process is gone from this instant: the request that fired
		// this (if any) is aborted, like every one after it.
		w.rec.dead.Store(true)
		g.MarkDisconnect()
		down := time.Duration(s.Ms) * time.Millisecond
		r.async(func() {
			w.Kill()
			time.Sleep(down)
			w.Start()
			r.note("  worker restarted (incarnation %d)", w.inc)
		})
	case "ws_drop":
		g.MarkDisconnect()
		if c := w.wsConn.Load(); c != nil {
			c.ForceDisconnect()
		}
	case "hub_down":
		g.down.Store(true)
		g.MarkDisconnect()
		if c := w.wsConn.Load(); c != nil {
			c.ForceDisconnect()
		}
		down := time.Duration(s.Ms) * time.Millisecond
		r.async(func() {
			time.Sleep(down)
			g.down.Store(false)
			r.note("  hub back up")
		})
	case "hub_crash":
		// Global dies with frames read but not committed, then is
		// unreachable for a while. Its database survives.
		g.blackhole.Store(true)
		down := time.Duration(s.Ms) * time.Millisecond
		r.async(func() {
			time.Sleep(4 * r.timing.Propagate)
			g.down.Store(true)
			g.MarkDisconnect()
			if c := w.wsConn.Load(); c != nil {
				c.ForceDisconnect()
			}
			time.Sleep(down)
			g.blackhole.Store(false)
			g.down.Store(false)
			r.note("  hub back up after crash")
		})
	case "sweep":
		g.AdvanceClock(time.Duration(s.Ms) * time.Millisecond)
		n := g.RunSweeps("sweep")
		r.note("  sweep after +%s expired %d op(s)", time.Duration(s.Ms)*time.Millisecond, n)
		hubSide = true
	case "hold_ops":
		g.pub.Hold()
	case "release_ops":
		n := g.pub.Release(s.Flag)
		r.note("  released %d held op message(s) reverse=%v", n, s.Flag)
		hubSide = true
	case "replay_ops":
		n := g.pub.Replay(max(s.Ms, 1), s.Flag, g.connSince)
		r.note("  replayed %d op message(s)", n)
		hubSide = true
	case "hold_push":
		g.holdPush.Store(true)
	case "release_push":
		g.ReleasePushes()
		hubSide = true
	case "rebuild":
		g.mu.Lock()
		g.lastHash = "" // force a push even when nothing changed
		g.mu.Unlock()
		g.invalidate()
		hubSide = true
	case "bmc":
		w.bmcDown.Store(s.Flag)
	case "bmc_slow":
		w.bmcDelay.Store(int64(time.Duration(s.Ms) * time.Millisecond))
	case "stale_report":
		r.staleReport(s)
	case "mac_phone_home":
		st, err := w.postMACPhoneHome(host.MAC())
		r.note("  MAC-only phone-home POST for %s: status=%d err=%v", host.MAC(), st, err)
	case "wait":
		time.Sleep(time.Duration(s.Ms) * time.Millisecond)
	case "settle":
		if inRequest {
			return
		}
		r.settle(s.Flag)
	default:
		r.t.Fatalf("prop: unknown step %q", s.Do)
	}
	if hubSide && inRequest {
		// Let the change reach the worker before the request proceeds, so
		// "before X" means the worker has seen it. A lifecycle step's Ms
		// overrides the pause: a couple of milliseconds puts the request's
		// own phase update right behind the executor's claim on the wire.
		pause := r.timing.Propagate
		if (s.Do == "deploy" || s.Do == "commission") && s.Ms > 0 {
			pause = time.Duration(s.Ms) * time.Millisecond
		}
		time.Sleep(pause)
	}
}

func (r *propRun) directPower(machine uuid.UUID, action string) {
	r.mu.Lock()
	r.direct = append(r.direct, propPowerAction{At: time.Now(), Machine: machine, Action: action})
	r.mu.Unlock()
}

// staleReport posts an agent report that a previous (terminal) operation's
// agent or install could still send. Arg: phase:<p> | failed |
// deploy_complete | phone_home. Flag: use the other host's machine ID with
// this host's stale op (a confused or malicious sender).
func (r *propRun) staleReport(s propStep) {
	op, ok := r.lastTerminalOp(s.Host)
	if !ok {
		r.note("  stale_report: host %d has no terminal op yet; skipped", s.Host)
		r.stat("skipped:stale_report")
		return
	}
	sender := s.Host
	if s.Flag {
		if len(r.ids) < 2 {
			r.note("  stale_report: no other host to send as; skipped")
			r.stat("skipped:stale_report")
			return
		}
		sender = 1 - s.Host
	}
	rep := model.ProgressReport{MachineID: r.ids[sender].String(), OperationID: op.String()}
	switch {
	case strings.HasPrefix(s.Arg, "phase:"):
		rep.Phase = strings.TrimPrefix(s.Arg, "phase:")
	case s.Arg == "failed":
		rep.Failed, rep.Phase, rep.Error = true, "partitioning", "stale agent failure"
	default:
		rep.Event = s.Arg
	}
	st, err := r.w.postReport(rep, true)
	r.note("  stale report op=%s as host %d %s -> status=%d err=%v", propShortID(op), sender, s.Arg, st, err)
}

// disarm retires every armed step that has not fired: a settle ends the
// episode, and a fault left armed would fire in some later operation
// (ultimately the liveness deploy) that it was never aimed at.
func (r *propRun) disarm() {
	for _, tr := range r.armedTriggers() {
		if tr.fired.CompareAndSwap(false, true) {
			r.stat("unfired_trigger")
			r.note("  disarmed (never fired): %s", tr.name)
		}
	}
}

// heal lifts every persistent fault: held op messages and identity pushes
// are released, global is reachable and committing, the BMC answers, the
// worker process is running. It reports whether it changed anything.
func (r *propRun) heal() bool {
	r.faults.Wait()
	changed := r.g.blackhole.Swap(false)
	changed = r.g.down.Swap(false) || changed
	changed = r.g.pub.Release(false) > 0 || changed
	changed = r.g.holdPush.Load() || changed
	r.g.ReleasePushes()
	changed = r.w.bmcDown.Swap(false) || changed
	changed = r.w.bmcDelay.Swap(0) != 0 || changed
	if !r.w.Alive() {
		changed = true
	}
	r.w.Start()
	return changed
}

// lastActivity is the time of the last event that represents progress. A
// rejected report does not count: an install whose operation is gone
// retries its phone-home forever, and that must not read as activity.
func (r *propRun) lastActivity() time.Time {
	last := r.w.rec.LastProgress()
	if hub := r.g.LastProgress(); hub.After(last) {
		last = hub
	}
	return last
}

// settle waits for the episode to come to rest. Armed faults stay live
// while it waits — the wait is where they fire. Once nothing has happened
// for a while the persistent faults are lifted (heal) and the wait
// restarts; with drive, an operation that is still active after that is
// expired the way global would: 31 minutes pass, the sweeps run. Triggers
// that never fired are retired on the way out.
func (r *propRun) settle(drive bool) {
	start := time.Now()
	// A backstop, not a timing: settle legitimately waits out Settle twice
	// (before and after the heal), two reconnects and a parked claim.
	begin, limit := start, 2*r.timing.Settle+2*propReconnectWait+propClaimStall
	var downSince time.Time // worker up but off the stream, no fault in flight
	// healthy is when the system last became undisturbed: worker serving
	// and on the stream, no fault still in flight. Time before that is not
	// idleness — a worker waiting out its reconnect backoff with a queued
	// completion is not a stuck operation.
	healthy := start
	healed := false
	for {
		// A BMC call or a boot-server request in flight is the worker at
		// work: it takes seconds, never the 30 minutes a driven timeout
		// stands for.
		if (healed && (!r.w.Alive() || !r.w.Connected())) || r.pendingFaults.Load() > 0 ||
			r.w.actuating.Load() > 0 || r.w.rec.inflight.Load() > 0 {
			healthy = time.Now()
		}
		active := len(r.g.ActiveOps(uuid.Nil))
		since := time.Since(r.lastActivity())
		if s := time.Since(healthy); s < since {
			since = s
		}
		over := time.Since(start) >= r.timing.Settle
		switch {
		case r.w.Connected() || !r.w.Alive() || r.pendingFaults.Load() > 0:
			downSince = time.Time{}
		case downSince.IsZero():
			downSince = time.Now()
		}
		if healed && !downSince.IsZero() && time.Since(downSince) > propReconnectWait+r.timing.Settle {
			r.violate("P6", "worker up but off the stream for %s with no fault in force",
				time.Since(downSince).Round(time.Second))
			break
		}
		if time.Since(begin) > limit {
			r.violate("P6", "the system did not come to rest within %s: requests in flight=%d, BMC calls in flight=%d, faults in flight=%d, active ops %v",
				limit, r.w.rec.inflight.Load(), r.w.actuating.Load(), r.pendingFaults.Load(), propOpSummaries(r.g.ActiveOps(uuid.Nil)))
			break
		}
		busy := r.w.actuating.Load() > 0 || r.w.rec.inflight.Load() > 0
		quiet := (active == 0 && since >= r.timing.Quiet) || (active > 0 && since >= r.timing.Idle) || (over && !busy)
		if quiet && !healed {
			// Faults stop here. If lifting them changed anything, the
			// system gets a fresh chance to finish on its own; if not, it
			// was already at rest and is judged now.
			healed = true
			changed := r.heal()
			if changed {
				r.note("  settle: quiet; persistent faults lifted")
			}
			// The worker may still be waiting out its reconnect backoff
			// (it tops out at 30s) from a fault that has already passed.
			wasDown := !r.w.Connected()
			for deadline := time.Now().Add(propReconnectWait); !r.w.Connected() && time.Now().Before(deadline); {
				time.Sleep(r.timing.Poll)
			}
			if wasDown {
				r.note("  settle: worker back on the stream")
			}
			if changed || wasDown {
				healthy = time.Now()
				start = time.Now()
				continue
			}
		}
		if quiet && r.heal() {
			// A fault armed on a moment fired after the heal above and left
			// something persistent behind (held op messages, a held push,
			// a dead BMC). Lift it and wait again: nothing may be judged
			// with a fault still in force.
			r.note("  settle: quiet again; faults that fired after the first heal lifted")
			for deadline := time.Now().Add(propReconnectWait); !r.w.Connected() && time.Now().Before(deadline); {
				time.Sleep(r.timing.Poll)
			}
			healthy = time.Now()
			start = time.Now()
			continue
		}
		if active == 0 && quiet {
			break
		}
		if active > 0 && quiet {
			if !drive {
				r.note("  settle: %d op(s) still active, not driving timeouts", active)
				break
			}
			if r.awaitingActuation() {
				time.Sleep(r.timing.Poll)
				continue
			}
			// A pending op is the executor's to claim. The executor can
			// legitimately be parked for its 30s claim-RPC timeout, so a
			// pending op is only overdue past that; then it is a liveness
			// failure of the worker, and the 24h pending sweep is what
			// would clear it.
			pending := 0
			for _, op := range r.g.ActiveOps(uuid.Nil) {
				if op.Status == model.OpPending {
					pending++
				}
			}
			if pending > 0 && time.Since(healthy) < propClaimStall {
				time.Sleep(r.timing.Poll)
				continue
			}
			if pending > 0 {
				r.violate("P6", "worker left %d pending operation(s) unclaimed for %s while up and connected: %v",
					pending, time.Since(healthy).Round(time.Second), propOpSummaries(r.g.ActiveOps(uuid.Nil)))
				r.g.AdvanceClock(24 * time.Hour)
			}
			r.g.AdvanceClock(31 * time.Minute)
			n := r.g.RunSweeps("timeout")
			r.stat("driven_timeouts")
			r.note("  settle: idle with %d active op(s); 31m pass, sweeps expired %d", active, n)
			if n == 0 {
				r.violate("P4", "operation(s) still active after the documented timeouts: %v", propOpSummaries(r.g.ActiveOps(uuid.Nil)))
				break
			}
		}
		time.Sleep(r.timing.Poll)
	}
	r.disarm()
	r.waitConverged()
}

// awaitingActuation reports whether a claimed op is still within the time
// a healthy executor takes to make its first power call after the claim.
func (r *propRun) awaitingActuation() bool {
	claims := map[uuid.UUID]time.Time{}
	for _, e := range r.g.Events() {
		if e.Kind == "claimed" || e.Kind == "reclaimed" {
			claims[e.OpID] = e.At
		}
	}
	log := r.w.PowerLog()
	for _, op := range r.g.ActiveOps(uuid.Nil) {
		at, ok := claims[op.ID]
		if !ok || op.Status != model.OpClaimed || time.Since(at) >= r.timing.Actuate {
			continue
		}
		acted := false
		for _, a := range log {
			if a.Machine == op.MachineID && a.Action != "set_boot" && !a.At.Before(at) {
				acted = true
			}
		}
		if !acted {
			return true
		}
	}
	return false
}

func propOpSummaries(ops []model.Operation) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = fmt.Sprintf("%s %s status=%s phase=%q", propShortID(op.ID), op.Type, op.Status, op.Phase)
	}
	return out
}

// divergence describes how the worker's view differs from global's.
func (r *propRun) divergence() []string {
	var out []string
	if !r.w.Connected() {
		return []string{"worker WebSocket not connected"}
	}
	cache := r.w.cache.Load()
	hubActive := len(r.g.ActiveOps(uuid.Nil))
	if n := cache.ActiveOpCount(); n != hubActive {
		out = append(out, fmt.Sprintf("worker holds %d active op(s), global has %d", n, hubActive))
	}
	for i, id := range r.ids {
		m := r.g.Machine(id)
		if !m.InSnapshot {
			continue
		}
		if ws := cache.MachineState(id); ws != string(m.State) {
			out = append(out, fmt.Sprintf("host %d: worker effective state %q, global state %q", i, ws, m.State))
		}
	}
	return out
}

func (r *propRun) waitConverged() {
	deadline := time.Now().Add(40 * r.timing.Propagate)
	if d := time.Now().Add(35 * time.Second); d.After(deadline) {
		// The worker's reconnect backoff tops out at 30s.
		deadline = d
	}
	var div []string
	for {
		div = r.divergence()
		if len(div) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(r.timing.Poll)
	}
	r.violate("P4", "worker and global disagree at quiescence: %s", strings.Join(div, "; "))
}

// Liveness: after everything, a fresh destructive deploy of the host must
// succeed (commissioning it first if it never was).
func (r *propRun) Liveness(host int) {
	id := r.ids[host]
	r.settle(true)
	if r.overBudget() {
		r.note("over budget: liveness deploy skipped")
		r.stat("liveness_skipped")
		return
	}
	r.note("liveness: fresh deploy of host %d", host)
	m := r.g.Machine(id)
	if m.Locked {
		r.g.SetLocked(id, false)
	}
	if !m.PXEEligible {
		if err := r.g.SetPXEEligible(id, true); err != nil {
			r.violate("LIVE", "re-enabling PXE on host %d was refused: %v", host, err)
			return
		}
	}
	waitState := func(want model.MachineState, what string) bool {
		begin := time.Now()
		deadline := begin.Add(r.timing.Live)
		for {
			if r.g.Machine(id).State == want && len(r.g.ActiveOps(id)) == 0 {
				return true
			}
			// The budget starts when the executor claims the op: it may
			// be parked in a claim RPC for up to propClaimStall first.
			ops := r.g.ActiveOps(id)
			if len(ops) > 0 && ops[0].Status == model.OpPending && time.Since(begin) < propClaimStall {
				deadline = time.Now().Add(r.timing.Live)
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(r.timing.Poll)
		}
		r.violate("LIVE", "fresh %s of host %d did not reach %s: global state=%s active=%v host=%s",
			what, host, want, r.g.Machine(id).State, propOpSummaries(r.g.ActiveOps(id)), propDescribeHost(r.hosts[host]))
		return false
	}
	if m.State == model.MachineStateNew || m.SystemUUID == "" {
		if _, err := r.g.CreateCommission(id); err != nil {
			r.violate("LIVE", "fresh commission of host %d rejected: %v", host, err)
			return
		}
		if !waitState(model.MachineStateCommissioned, "commission") {
			return
		}
	}
	if _, err := r.g.CreateDeploy(id, true, ""); err != nil {
		r.violate("LIVE", "fresh deploy of host %d rejected: %v", host, err)
		return
	}
	if waitState(model.MachineStateRunning, "deploy") {
		r.stat("liveness_ok")
	}
	r.settle(true)
}

func propDescribeHost(h propHost) string {
	st, _ := h.PowerState()
	s := fmt.Sprintf("%s power=%s", h.Name(), st)
	if sh, ok := h.(*propSimHost); ok {
		s += " stage=" + sh.Stage()
	}
	return s
}

// --- oracle ---

type propOpLife struct {
	op        model.Operation
	created   time.Time
	terminal  time.Time // zero while active
	cause     string
	status    string
	removedAt time.Time // when op_removed went on the wire
}

func (r *propRun) opLives() map[string]*propOpLife {
	lives := map[string]*propOpLife{}
	for _, op := range r.g.Ops() {
		lives[op.ID.String()] = &propOpLife{op: op}
	}
	for _, e := range r.g.Events() {
		l := lives[e.OpID.String()]
		if l == nil {
			continue
		}
		switch e.Kind {
		case "op_created":
			l.created = e.At
		case "completed", "failed":
			l.terminal, l.cause, l.status = e.At, e.Cause, e.Kind
		}
	}
	r.g.pub.mu.Lock()
	for _, m := range r.g.pub.sentLog {
		if l := lives[m.op]; l != nil && m.removed && l.removedAt.IsZero() {
			l.removedAt = m.at
		}
	}
	r.g.pub.mu.Unlock()
	return lives
}

// shouldKnow reports whether the worker had been told (op_removed on a
// live connection, or a resync) that the op was terminal, at least Grace
// before t. An op_removed only counts if the connection it went out on was
// resynced before it and stayed up for Grace after it: one published into
// a dead or dying connection tells the worker nothing, and the resync of
// the next connection is when it learns.
func (r *propRun) shouldKnow(l *propOpLife, t time.Time, connected bool) bool {
	if l.terminal.IsZero() || !connected {
		return false
	}
	told := time.Time{}
	if !l.removedAt.IsZero() && r.g.connLive(l.removedAt, l.removedAt.Add(r.timing.Grace)) {
		told = l.removedAt
	}
	for _, rs := range r.g.Resyncs() {
		if rs.After(l.terminal) && (told.IsZero() || rs.Before(told)) {
			told = rs
		}
	}
	return !told.IsZero() && t.Sub(told) > r.timing.Grace
}

// Check evaluates the safety properties over everything observed so far.
func (r *propRun) Check() {
	lives := r.opLives()
	events := r.w.rec.Events()

	// P6: the worker must always shut down.
	for _, h := range r.w.Hung() {
		r.violate("P6", "%s", h)
	}

	rewipes := map[string]int{}         // op -> deploy boots served after its deploy_complete
	phoneHome := map[string]time.Time{} // op -> first accepted real phone_home
	// Events are recorded as their handler returns, so a request that
	// stalled in the handler can be recorded after one that came later;
	// order is judged by when handlers ran. For the first accepted
	// deploy_complete of each op, dcStarted is when its handler started and
	// dcDone when it returned (the latch is set somewhere in between).
	dcStarted, dcDone := map[string]time.Time{}, map[string]time.Time{}
	for _, e := range events {
		if e.Kind == "report" && e.Event == "deploy_complete" && !e.Synthetic && e.Status >= 200 && e.Status < 300 {
			if t, ok := dcStarted[e.Op]; !ok || e.Began.Before(t) {
				dcStarted[e.Op], dcDone[e.Op] = e.Began, e.Began.Add(e.Took)
			}
		}
	}
	for _, e := range events {
		switch e.Kind {
		case "boot":
			host := r.hostOfMAC(e.MAC)
			if e.Boot != "deploy" && e.Boot != "commission" {
				continue
			}
			r.stat("boot:" + e.Boot)
			// P1 (untrusted-until-resync): a worker process that has not
			// had an op_resync since it started is running on whatever op
			// set it left in LocalDB, and must not hand out an installer on
			// that basis (docs/dev/state-machine.md: "may force BootLocal
			// but never actuate").
			if start := r.w.IncarnationStart(e.Inc); !start.IsZero() {
				resynced := false
				for _, rs := range r.g.Resyncs() {
					if rs.After(start) && rs.Before(e.At) {
						resynced = true
					}
				}
				if !resynced {
					r.violate("P1", "host %d served a %s boot by a restarted worker that had not resynced with global: %s", host, e.Boot, e)
				}
			}
			// P1: an installer boot must be backed by an active lifecycle
			// op for exactly this machine.
			l := lives[e.Op]
			switch {
			case host < 0:
				r.violate("P1", "installer boot served to a MAC that is not a test host: %s", e)
			case l == nil:
				r.violate("P1", "host %d served a %s boot naming an operation global never created: %s", host, e.Boot, e)
			case l.op.MachineID != r.ids[host]:
				r.violate("P1", "host %d served a %s boot bound to another machine's operation: %s", host, e.Boot, e)
			case string(l.op.Type) != e.Boot:
				r.violate("P1", "host %d served a %s boot for a %s operation: %s", host, e.Boot, l.op.Type, e)
			case r.shouldKnow(l, e.At, e.Connected):
				r.violate("P1", "host %d served a %s boot for operation %s, which global ended (%s, %s) %s earlier: %s",
					host, e.Boot, propShortID(l.op.ID), l.status, l.cause, e.At.Sub(l.terminal).Round(time.Millisecond), e)
			case !l.terminal.IsZero() && e.At.After(l.terminal):
				r.stat("stale_boot_while_partitioned")
			}
			// P2: no deploy boot for an op after its deploy_complete.
			if dc, ok := dcDone[e.Op]; ok && e.Boot == "deploy" && e.Began.After(dc) {
				rewipes[e.Op]++
				if rewipes[e.Op] > 1 {
					continue
				}
				// Name the mechanism where the store's phase trail shows it.
				how := "phase lost"
				sawAwaiting := false
				for _, he := range r.g.Events() {
					if he.Kind != "phase" || he.OpID.String() != e.Op {
						continue
					}
					if he.Phase == "awaiting_phone_home" {
						sawAwaiting = true
					} else if sawAwaiting && (he.Phase == "power_cycle" || he.Phase == "pxe_booting") {
						how = "executor phase write after awaiting_phone_home"
					}
				}
				r.violate("P2", "host %d served a DEPLOY boot for operation %s after that operation's deploy_complete was accepted (re-wipe; %s): +%s %s",
					host, e.Op[:8], how, e.Began.Sub(dc).Round(time.Millisecond), e)
			}
		case "report":
			if e.Status < 200 || e.Status >= 300 {
				continue
			}
			l := lives[e.Op]
			host := r.hostOf(e.Machine)
			hubRunning := host >= 0 && r.g.MachineStateAt(r.ids[host], e.At) == model.MachineStateRunning
			// A phone-home that arrives after its machine is already
			// running is acknowledged without effect (the unit retries
			// until it gets a 2xx); the genuine one precedes completion.
			// The worker acknowledges, without effect, a phone-home from a
			// machine it already holds running, whatever op it names (the
			// unit retries until it gets a 2xx). Anything else is judged.
			ackOnly := e.Event == "phone_home" && hubRunning &&
				(l == nil || l.op.MachineID.String() != e.Machine || (!l.terminal.IsZero() && e.At.After(l.terminal)))
			// P3: a report naming an op that is not this machine's active
			// op must not be accepted (the phone-home retry ack for an
			// already-running machine is the documented exception).
			switch {
			case ackOnly:
			case l == nil:
				r.violate("P3", "worker accepted a report naming an operation global never created: %s", e)
			case l.op.MachineID.String() != e.Machine:
				r.violate("P3", "worker accepted a report whose operation belongs to another machine: %s", e)
			case r.shouldKnow(l, e.At, e.Connected):
				r.violate("P3", "worker accepted a report for operation %s, which global ended (%s, %s) %s earlier: %s",
					propShortID(l.op.ID), l.status, l.cause, e.At.Sub(l.terminal).Round(time.Millisecond), e)
			}
			if e.Synthetic {
				continue
			}
			if e.Phase != "" {
				r.stat("phase:" + e.Phase)
			}
			switch e.Event {
			case "phone_home":
				if ackOnly {
					break
				}
				// P3: the phone-home completes the deploy, so it is valid
				// only once the agent's deploy_complete has been accepted
				// (the op is awaiting_phone_home).
				if dc, ok := dcStarted[e.Op]; !ok || !dc.Before(e.Began.Add(e.Took)) {
					r.violate("P3", "worker accepted a phone-home for operation %s before its agent's deploy_complete was accepted: %s", e.Op[:8], e)
				}
				if _, ok := phoneHome[e.Op]; !ok {
					phoneHome[e.Op] = e.At
				}
			}
		case "mac_phone_home":
			// P3: POST /api/v1/phone-home?mac= names no operation and must
			// complete or change nothing; the route is gone.
			if e.Status >= 200 && e.Status < 300 {
				r.violate("P3", "worker accepted a MAC-only phone-home that names no operation: %s", e)
			}
		}
	}

	for op, n := range rewipes {
		if n > 1 {
			r.violate("P2", "operation %s: the installer was served %d times after deploy_complete (re-wipe LOOP)", op[:8], n)
		}
	}

	// P3: every deploy completion global recorded on the worker's word (its
	// op_update, or the durable phone_home observation) must trace to that
	// operation's own install phoning home.
	for _, e := range r.g.Events() {
		if (e.Cause != "worker" && e.Cause != "phone_home") || e.OpType != model.OpDeploy {
			continue
		}
		switch e.Kind {
		case "completed":
			if _, ok := phoneHome[e.OpID.String()]; !ok {
				r.violate("P3", "global completed deploy %s (machine -> running) but that operation's install never phoned home; last phase %q",
					propShortID(e.OpID), e.Phase)
			} else if _, ok := dcStarted[e.OpID.String()]; !ok {
				r.violate("P3", "global completed deploy %s but its agent never reported deploy_complete", propShortID(e.OpID))
			}
		}
	}

	// P4 (reality): global's machine state must match what the host did.
	// Judged per deploy, at the end of that deploy's epoch — the moment
	// the next lifecycle op for the machine is created, or now — because a
	// later op legitimately moves the state on.
	hubEvents := r.g.Events()
	for host, id := range r.ids {
		m := r.g.Machine(id)
		if !m.InSnapshot {
			continue
		}
		// The machine's lifecycle ops in creation order.
		var ops []*propOpLife
		for _, l := range lives {
			if l.op.MachineID == id && (l.op.Type == model.OpDeploy || l.op.Type == model.OpCommission) {
				ops = append(ops, l)
			}
		}
		sort.Slice(ops, func(i, j int) bool { return ops[i].created.Before(ops[j].created) })
		for i, l := range ops {
			if l.op.Type != model.OpDeploy {
				continue
			}
			opID := l.op.ID.String()
			dc, installed := dcDone[opID]
			ph, phoned := phoneHome[opID]
			// Epoch end and the machine state global held there.
			end, stateAtEnd, active := time.Now(), m.State, len(r.g.ActiveOps(id)) > 0
			if i+1 < len(ops) {
				end, active = ops[i+1].created, false
				for _, e := range hubEvents {
					if e.Kind == "op_created" && e.OpID == ops[i+1].op.ID {
						stateAtEnd = e.State
					}
				}
			}
			if active {
				continue
			}
			// A deploy boot after deploy_complete rewrote the disk: the
			// install that phoned home may no longer be what is on it.
			rewritten := false
			for _, e := range events {
				if installed && e.Kind == "boot" && e.Boot == "deploy" && r.hostOfMAC(e.MAC) == host && e.Began.After(dc) && e.Began.Before(end) {
					rewritten = true
				}
			}
			if installed && phoned && ph.Before(end) && !rewritten && stateAtEnd != model.MachineStateRunning {
				// Legitimate only if an operator or sweep ended the op
				// before the install phoned home (then accepting the
				// phone-home was the error, and P3 reports it).
				// An abort or an injected sweep that raced the phone-home is
				// the operator's (or the clock's) call and stands.
				if l.status == "completed" || (l.terminal.After(ph) && strings.HasPrefix(l.cause, "timeout")) {
					r.violate("P4", "host %d booted operation %s's install and phoned home (accepted %s), but global ended that deploy as %s/%s and held the machine in %q",
						host, propShortID(l.op.ID), ph.Format("15:04:05.000"), l.status, l.cause, stateAtEnd)
				}
			}
			if l.status == "completed" && !phoned {
				// Reported under P3 as well; this is the state half of it.
				r.violate("P4", "global completed deploy %s and held host %d in %q, but that install never phoned home", propShortID(l.op.ID), host, stateAtEnd)
			}
		}
		if (m.State == model.MachineStateDeploying || m.State == model.MachineStateCommissioning) && len(r.g.ActiveOps(id)) == 0 {
			r.violate("P4", "host %d stuck in %s with no active operation", host, m.State)
		}
	}

	r.checkPower(lives)
	r.checkBystanders()
}

func (r *propRun) armedTriggers() []*propTrigger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*propTrigger(nil), r.armed...)
}

// checkPower: every actuation the executor issued must belong to an op
// global handed it for that machine (P5, wrong-host actuation), and the
// host's power state at the end must be what the last completed power
// action implies.
func (r *propRun) checkPower(lives map[string]*propOpLife) {
	hub := r.g.Events()
	log := r.w.PowerLog()
	for _, a := range log {
		if a.Action == "set_boot" {
			continue
		}
		r.stat("power:" + a.Action)
		ok := false
		for _, e := range hub {
			if (e.Kind != "claimed" && e.Kind != "reclaimed") || e.Machine != a.Machine {
				continue
			}
			d := a.At.Sub(e.At)
			if d < -r.timing.Grace || d > 2*time.Minute {
				continue
			}
			switch e.OpType {
			case model.OpDeploy, model.OpCommission:
				ok = a.Action == "cycle" || a.Action == "on"
			case model.OpPowerCycle:
				ok = a.Action == "cycle"
			case model.OpPowerOff:
				ok = a.Action == "off"
			case model.OpPowerOn:
				ok = a.Action == "on"
			default:
				// No other op type actuates power.
			}
			if ok {
				break
			}
		}
		if !ok {
			r.violate("P5", "executor issued power %s on machine %s with no claimed operation that calls for it (at %s)",
				a.Action, propShortID(a.Machine), a.At.Format("15:04:05.000"))
		}
	}
	// End state: merge executor actuations and direct hardware faults.
	type ev struct {
		at time.Time
		on bool
	}
	for host, id := range r.ids {
		var tl []ev
		r.mu.Lock()
		acts := append(append([]propPowerAction(nil), log...), r.direct...)
		r.mu.Unlock()
		for _, a := range acts {
			if a.Machine != id || a.Action == "set_boot" {
				continue
			}
			tl = append(tl, ev{a.At, a.Action != "off"})
		}
		if len(tl) == 0 {
			continue
		}
		sort.Slice(tl, func(i, j int) bool { return tl[i].at.Before(tl[j].at) })
		want := power.StateOff
		if tl[len(tl)-1].on {
			want = power.StateOn
		}
		got, err := r.hosts[host].PowerState()
		if err == nil && got != want {
			r.violate("P5", "host %d power is %s, last power action implies %s", host, got, want)
		}
	}
	// A power op global recorded as completed must have been actuated; one
	// it recorded as failed by a sweep must not have been.
	resultLost := map[uuid.UUID]bool{}
	for _, e := range hub {
		if e.Kind == "dropped_update" && e.Detail != "status=" {
			resultLost[e.OpID] = true
		}
	}
	for _, l := range lives {
		var want string
		switch l.op.Type {
		case model.OpPowerCycle:
			want = "cycle"
		case model.OpPowerOff:
			want = "off"
		case model.OpPowerOn:
			want = "on"
		default:
			continue
		}
		if l.terminal.IsZero() {
			continue
		}
		acted := false
		for _, a := range log {
			if a.Machine == l.op.MachineID && a.Action == want && a.At.After(l.created) && a.At.Before(l.terminal.Add(r.timing.Grace)) {
				acted = true
			}
		}
		if l.status == "completed" && !acted {
			r.violate("P5", "global completed power op %s (%s) but the executor never actuated it", propShortID(l.op.ID), l.op.Type)
		}
		if l.status == "failed" && strings.HasPrefix(l.cause, "timeout") && acted {
			if resultLost[l.op.ID] {
				// Known defect: a power op's result rides only the unacked
				// op_update, so when global crashes before committing it the
				// op is failed at the stuck timeout although the host was
				// actuated (deploys are covered by the durable phone_home
				// observation; power ops have no such record).
				r.stat("power_result_lost")
				continue
			}
			r.violate("P5", "power op %s (%s) was actuated on the host but global recorded it failed (%s)", propShortID(l.op.ID), l.op.Type, l.cause)
		}
	}
}

// checkBystanders: a host that never had a lifecycle op must never have
// been served an installer (P1 covers the boot) nor had its disk written.
func (r *propRun) checkBystanders() {
	hadOp := map[uuid.UUID]bool{}
	for _, op := range r.g.Ops() {
		if op.Type == model.OpDeploy || op.Type == model.OpCommission {
			hadOp[op.MachineID] = true
		}
	}
	for host, id := range r.ids {
		if hadOp[id] {
			continue
		}
		if n := r.hosts[host].DiskWrites(); n > 0 {
			r.violate("P1", "host %d never had a lifecycle operation but its disk was written (%d write ops)", host, n)
		}
	}
}

func (r *propRun) Violations() []propViolation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]propViolation(nil), r.violations...)
}

// Report renders everything needed to replay and understand a history.
func (r *propRun) Report(h propHistory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "history %q seed=%d\n  %s\n  json: %s\n", h.Name, h.Seed, h, h.JSON())
	b.WriteString("violations:\n")
	vs := r.Violations()
	for i, v := range vs {
		if i == propReportViolations {
			fmt.Fprintf(&b, "  ... and %d more\n", len(vs)-i)
			break
		}
		fmt.Fprintf(&b, "  %s\n", v)
	}
	b.WriteString("connections (resync / torn down by a step):\n")
	for _, t := range r.g.Resyncs() {
		fmt.Fprintf(&b, "  %s resync\n", t.Format("15:04:05.000"))
	}
	r.g.mu.Lock()
	for _, t := range r.g.disconnects {
		fmt.Fprintf(&b, "  %s disconnect\n", t.Format("15:04:05.000"))
	}
	r.g.mu.Unlock()
	b.WriteString("interpreter:\n")
	r.mu.Lock()
	for _, n := range r.notes {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	r.mu.Unlock()
	b.WriteString("global store:\n")
	for _, e := range r.g.Events() {
		if e.Kind == "obs" && e.Detail == "dhcp_lease" {
			continue
		}
		fmt.Fprintf(&b, "  %s\n", e)
	}
	b.WriteString("worker http:\n")
	for _, e := range r.w.rec.Events() {
		fmt.Fprintf(&b, "  %s\n", e)
	}
	b.WriteString("power:\n")
	for _, a := range r.w.PowerLog() {
		fmt.Fprintf(&b, "  %s %s machine=%s\n", a.At.Format("15:04:05.000"), a.Action, propShortID(a.Machine))
	}
	return b.String()
}

func (r *propRun) StatLine(h propHistory) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.stats))
	for k := range r.stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q:%d", k, r.stats[k]))
	}
	return fmt.Sprintf("history stats: {\"name\":%q,\"violations\":%d,%s}", h.Name, len(r.violations), strings.Join(parts, ","))
}

// --- generator ---

var (
	propDeployPhases     = []string{"agent_starting", "partitioning", "rsyncing_os", "installing_bootloader", "finalizing", "bmc_reconcile"}
	propCommissionPhases = []string{"agent_starting", "collecting_inventory", "reporting"}
)

// propGenConfig scales the generator to a backend.
type propGenConfig struct {
	Episodes   int   // lifecycle episodes on the subject host
	MaxFaults  int   // armed faults per episode
	DownMs     []int // candidate outage lengths
	Bystanders int   // hosts beyond the subject
	// Each history enables a random subset of the fault families, so
	// families also get explored apart from one another; AllFamilies
	// enables every one.
	AllFamilies bool
	// Families restricts the subset to these fault families (nil = all).
	Families []string
	// UEFI lets the generator pick UEFI firmware for the subject. Off for
	// the VM tier's generated histories: after a UEFI install the
	// firmware boots the disk entry the agent registered, and the test
	// power controller has no way to arm OVMF's next boot for PXE the way
	// a BMC's one-time override does, so a second deploy cannot start.
	UEFI bool
}

// propFaultFamilies: vm (hard reset / power loss), worker (process kill and
// restart), hub (WebSocket drop, global unreachable, global crash with
// uncommitted frames), abort, sweep (expiry while the agent runs), stream
// (held / reordered / replayed op messages and identity pushes), stale
// (reports from a previous operation's agent or install), spurious (PXE
// boots nobody asked for, subject and bystander), power (power ops),
// macphonehome (a phone-home that names no operation, POST
// /api/v1/phone-home?mac=, which the worker must refuse).
//
// claimrace is not a fault at all: the host is already PXE-booting when the
// operator creates the op (it was just power-cycled, or it has nothing to
// boot and is retrying), so its boot-prefix phase updates travel right
// behind the executor's claim.
var propFaultFamilies = []string{"vm", "worker", "hub", "abort", "sweep", "stream", "stale", "spurious", "power", "claimrace", "macphonehome"}

// propGenHistory builds a history from a seed. Generation is state-aware only
// in the cheap sense that matters: it knows which lifecycle op an episode
// runs, so every armed moment is one that op can reach.
func propGenHistory(seed int64, cfg propGenConfig) propHistory {
	rng := rand.New(rand.NewSource(seed))
	h := propHistory{Name: fmt.Sprintf("seed-%d", seed), Seed: seed}
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	ms := func() int { return cfg.DownMs[rng.Intn(len(cfg.DownMs))] }

	universe := cfg.Families
	if len(universe) == 0 {
		universe = propFaultFamilies
	}
	var fams []string
	for _, f := range universe {
		if cfg.AllFamilies || rng.Intn(2) == 0 {
			fams = append(fams, f)
		}
	}
	if len(fams) == 0 {
		fams = []string{pick(universe)}
	}

	commissioned := true // subject starts commissioned
	if rng.Intn(4) == 0 {
		h.Setup.SubjectNew = true
		commissioned = false
	}
	if cfg.Bystanders > 0 {
		h.Setup.Bystander = pick([]string{"running", "running", "commissioned", "failed", "new", "pxe_off", "unknown"})
	}
	// Firmware and storage shape only matter on the VM tier.
	h.Setup.UEFI = cfg.UEFI && rng.Intn(3) == 0
	h.Setup.RAID1 = rng.Intn(4) == 0

	momentFor := func(kind string) propMoment {
		m := propMoment{Host: 0, Before: rng.Intn(2) == 0}
		if kind == "commission" {
			switch rng.Intn(5) {
			case 0:
				m.Kind, m.Boot, m.Before = "boot", "commission", false
			case 1:
				m.Kind, m.Event = "report", "commission_report"
			default:
				m.Kind, m.Phase = "report", pick(propCommissionPhases)
			}
			return m
		}
		switch rng.Intn(10) {
		case 0:
			m.Kind, m.Boot, m.Before = "boot", "deploy", false
		case 1, 2:
			m.Kind, m.Event = "report", "deploy_complete"
		case 3:
			// First disk boot after the install.
			m.Kind, m.Boot, m.Before = "boot", "local", false
		case 4, 5:
			m.Kind, m.Event = "report", "phone_home"
		default:
			m.Kind, m.Phase = "report", pick(propDeployPhases)
		}
		return m
	}

	var faultFams []string
	for _, f := range fams {
		if f != "claimrace" {
			faultFams = append(faultFams, f)
		}
	}
	if len(faultFams) == 0 {
		faultFams = []string{"spurious"}
	}
	fault := func(kind string) propStep {
		switch pick(faultFams) {
		case "vm":
			return propStep{Do: pick([]string{"vm_reset", "vm_reset", "vm_off"}), Ms: ms()}
		case "worker":
			return propStep{Do: "worker_kill", Ms: ms()}
		case "hub":
			return propStep{Do: pick([]string{"ws_drop", "hub_down", "hub_crash"}), Ms: ms()}
		case "abort":
			return propStep{Do: "abort"}
		case "sweep":
			return propStep{Do: "sweep", Ms: propPick2(rng, int((2*time.Minute+5*time.Second)/time.Millisecond), int(31*time.Minute/time.Millisecond))}
		case "stream":
			return propStep{Do: pick([]string{"hold_ops", "replay_ops", "hold_push", "rebuild"}), Ms: 1 + rng.Intn(6), Flag: rng.Intn(2) == 0}
		case "stale":
			return propStep{Do: "stale_report",
				Arg: pick([]string{"phase:partitioning", "phase:ipxe_loaded", "failed", "deploy_complete", "phone_home"})}
		case "macphonehome":
			return propStep{Do: "mac_phone_home"}
		case "spurious":
			host := 0
			if cfg.Bystanders > 0 {
				host = rng.Intn(cfg.Bystanders + 1)
			}
			return propStep{Do: "vm_reset", Host: host}
		default:
			return propStep{Do: "power", Arg: pick([]string{"cycle", "off", "on"})}
		}
	}

	for ep := 0; ep < cfg.Episodes; ep++ {
		kind := "deploy"
		if !commissioned || rng.Intn(6) == 0 {
			kind = "commission"
		}
		// Arm this episode's faults before the op exists, so the moments
		// cannot be missed.
		nf := rng.Intn(cfg.MaxFaults + 1)
		for i := 0; i < nf; i++ {
			f := fault(kind)
			m := momentFor(kind)
			f.At = &m
			h.Steps = append(h.Steps, f)
			// Paired heal for holds, so a hold is a reordering window and
			// not a permanent partition.
			switch f.Do {
			case "hold_ops":
				m2 := momentFor(kind)
				h.Steps = append(h.Steps, propStep{Do: "release_ops", Flag: rng.Intn(2) == 0, At: &m2})
			case "hold_push":
				m2 := momentFor(kind)
				h.Steps = append(h.Steps, propStep{Do: "release_push", At: &m2})
			}
		}
		opStep := propStep{Do: "deploy", Flag: rng.Intn(4) != 0}
		if kind == "commission" {
			opStep = propStep{Do: "commission"}
		}
		raced := false
		for _, f := range fams {
			raced = raced || f == "claimrace"
		}
		if raced && rng.Intn(3) == 0 {
			// The host is mid-PXE as the op is created. The op is armed
			// (and the BMC slowed) before the reset, so the moment cannot
			// pass before its trigger exists.
			m := propMoment{Before: true, Host: 0, Kind: "dhcp", Phase: pick([]string{"dhcp_ack", "ipxe_dhcp"})}
			opStep.At, opStep.Ms = &m, 1+rng.Intn(4)
			h.Steps = append(h.Steps, opStep)
			if rng.Intn(2) == 0 {
				// ...and the BMC takes its time over the executor's cycle.
				h.Steps = append(h.Steps, propStep{Do: "bmc_slow", Ms: ms()})
			}
			h.Steps = append(h.Steps, propStep{Do: "vm_reset"})
		} else {
			h.Steps = append(h.Steps, opStep)
		}
		// Sometimes interleave an un-armed fault right after creation.
		if rng.Intn(4) == 0 {
			h.Steps = append(h.Steps, propStep{Do: "wait", Ms: ms() / 4}, fault(kind))
		}
		h.Steps = append(h.Steps, propStep{Do: "settle", Flag: true})
		if kind == "commission" {
			commissioned = true
		}
		// Between episodes: idle-time events.
		for i := rng.Intn(3); i > 0; i-- {
			f := fault("idle")
			if f.Do == "sweep" || f.Do == "abort" {
				continue
			}
			h.Steps = append(h.Steps, f)
		}
		if rng.Intn(3) == 0 {
			h.Steps = append(h.Steps, propStep{Do: "settle", Flag: true})
		}
	}
	return h
}

func propPick2(rng *rand.Rand, a, b int) int {
	if rng.Intn(2) == 0 {
		return a
	}
	return b
}
