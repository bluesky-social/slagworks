package worker

// The site-worker link carried through a fault-injecting proxy: the real
// WSConn (connectAndRun, drainLoop, drainOnce, handleObservationAck, the
// reconnect reset, the heartbeat and idle-read liveness) against a real
// ws.Hub, with internal/proptest/faultproxy between them standing in for the
// load balancer and reverse proxy that front global in a deployment.
//
// TestWSConnThroughFaultProxy_Prop interleaves faults with traffic:
// connection drops with FIN and with RST, half-closed connections,
// blackholed paths that end in an RST, proxy restarts, 429/502/503 on the
// upgrade, a connection lifetime limit, the byte stream split into writes of
// a few bytes, hub-side kills and worker crashes. Properties, checked at
// quiescence:
//
//   - every observation is eventually acked by global, unless global
//     rejected it terminally, and nothing acked is redelivered afterwards;
//   - op updates reach global in order and without duplicates, and every
//     update appended while no fault could intervene is delivered;
//   - the worker ends on the last snapshot global pushed, whatever was lost
//     on the way;
//   - every upgrade the proxy saw carried the worker token in the
//     Authorization header and nowhere in the request line.
//
// TestWSConnLinkLiveness_Prop checks the liveness contract both ends keep
// (ws.HeartbeatInterval, ws.IdleTimeout), with the timers shortened: an idle
// link outlives a proxy idle timeout of a few heartbeat intervals, and a
// path that goes silent in either direction without a FIN or RST is
// replaced, at both ends, about one idle timeout later and not before.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/proptest/faultproxy"
)

// pxEnv is the e2e environment with a fault proxy between worker and hub.
type pxEnv struct {
	*e2eEnv
	px *faultproxy.Proxy
}

// newPxEnv starts a hub, a proxy in front of it and a worker dialing the
// proxy. heartbeat and idle shorten the link's liveness timers at both ends;
// zero keeps the production values, which no fault in a short test reaches.
func newPxEnv(t fataler, dir string, heartbeat, idle time.Duration) *pxEnv {
	e := newE2EGlobalSide(t, dir, heartbeat, idle)
	px, err := faultproxy.New(strings.TrimPrefix(e.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	e.globalURL = px.URL()
	e.startWorker()
	return &pxEnv{e2eEnv: e, px: px}
}

func (e *pxEnv) close() {
	e.stopWorker()
	e.px.Close()
	e.hub.Close()
	e.srv.Close()
}

func (e *pxEnv) lastConnErr() string {
	if p := e.lastErr.Load(); p != nil {
		return *p
	}
	return "<none>"
}

// registrations is how many connections global holds for the site.
func (e *pxEnv) registrations() int { return len(e.hub.ConnectedWorkerSerials("site-a")) }

// lowerDefaultRapidChecks runs the calling test's rapid.Check with n cases
// unless -rapid.checks was given on the command line, which then scales the
// test up for a deep local run. For properties whose cases are expensive.
func lowerDefaultRapidChecks(t *testing.T, n int) {
	t.Helper()
	const name = "rapid.checks"
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == name })
	if explicit {
		return
	}
	// Value.Set, not flag.Set: flag.Set would make the flag look explicit
	// to the next caller.
	v := flag.Lookup(name).Value
	prev := v.String()
	if err := v.Set(fmt.Sprint(n)); err != nil {
		t.Fatalf("set -%s: %v", name, err)
	}
	t.Cleanup(func() { _ = v.Set(prev) })
}

// genBigObsData occasionally produces a multi-MiB observation so frames far
// larger than one TCP segment cross the splitting proxy.
func genBigObsData(t *rapid.T) json.RawMessage {
	if rapid.IntRange(0, 15).Draw(t, "bigObs") != 0 {
		return genObsData(t)
	}
	n := rapid.IntRange(1<<20, 6<<20).Draw(t, "bigLen")
	b, _ := json.Marshal(map[string]string{"blob": strings.Repeat("q", n)})
	return b
}

type pxStats struct {
	cases, appends, bigAppends, updates, pushes, bigPushes, drops, resets, halfCloses, blackholes,
	restarts, refusals, lifetimes, chunked, hubKills, crashes, redeliveries, upgrades atomic.Int64
}

func TestWSConnThroughFaultProxy_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end WS proxy property is slow")
	}
	// A case is a hub, a proxy and a worker on its own SQLite file, up to 30
	// steps and two waits for quiescence.
	lowerDefaultRapidChecks(t, 40)
	root := t.TempDir()
	var n atomic.Int64
	st := &pxStats{}
	rapid.Check(t, func(rt *rapid.T) {
		dir := filepath.Join(root, fmt.Sprintf("p%d", n.Add(1)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rt.Fatalf("mkdir: %v", err)
		}
		e := newPxEnv(rt, dir, 0, 0)
		defer e.close()
		e.px.SetSeed(rapid.Uint64().Draw(rt, "proxySeed"))
		st.cases.Add(1)
		if !e2eWaitFor(20*time.Second, e.w.Connected) {
			rt.Fatalf("worker never connected within 20s (last connect error: %s)", e.lastConnErr())
		}

		var keys []uuid.UUID
		terminal := map[uuid.UUID]bool{}
		var appended []model.OperationUpdate
		lostFloor := 0
		faultHellos := int64(-1)
		lifetimeOn := false
		seq := int64(1) // the rebuilder's snapshot; pushes count up from it
		var trace []string
		fault := func(name string) {
			lostFloor = len(appended)
			faultHellos = e.auth.hellos.Load()
			trace = append(trace, name)
		}

		steps := rapid.IntRange(1, 30).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 18).Draw(rt, "op") {
			case 0, 1, 2:
				k := uuid.New()
				keys = append(keys, k)
				e.global.mu.Lock()
				switch rapid.IntRange(0, 5).Draw(rt, "fate") {
				case 0:
					e.global.terminal[k] = true
					terminal[k] = true
				case 1, 2:
					e.global.reject[k] = rapid.IntRange(1, 3).Draw(rt, "rejects")
				}
				e.global.mu.Unlock()
				data := genBigObsData(rt)
				if len(data) > 1<<20 {
					st.bigAppends.Add(1)
				}
				e.cache.appendObservation(model.Observation{MachineID: k, Event: rapid.SampledFrom(obEvents).Draw(rt, "event"), Data: data})
				st.appends.Add(1)
				trace = append(trace, "append")
			case 3, 4:
				u := genOpUpdate(rt)
				mayBeLost := lifetimeOn || (faultHellos >= 0 && e.auth.hellos.Load() <= faultHellos)
				e.cache.appendOpUpdate(u)
				appended = append(appended, u)
				if mayBeLost {
					lostFloor = len(appended)
				}
				st.updates.Add(1)
				trace = append(trace, "update")
			case 5:
				e.px.DropAll()
				st.drops.Add(1)
				fault("proxy drop (FIN)")
			case 6:
				e.px.ResetAll()
				st.resets.Add(1)
				fault("proxy reset (RST)")
			case 7:
				e.px.HalfCloseDownAll()
				st.halfCloses.Add(1)
				fault("proxy half-close toward worker")
			case 8:
				e.px.BlackholeAll()
				fault("proxy blackhole")
				time.Sleep(time.Duration(rapid.IntRange(0, 40).Draw(rt, "blackholeMs")) * time.Millisecond)
				e.px.ResetAll()
				st.blackholes.Add(1)
				trace = append(trace, "...then RST")
			case 9:
				down := time.Duration(rapid.IntRange(0, 60).Draw(rt, "restartDownMs")) * time.Millisecond
				fault(fmt.Sprintf("proxy restart (down %v)", down))
				if err := e.px.Restart(down); err != nil {
					rt.Fatalf("proxy restart: %v", err)
				}
				st.restarts.Add(1)
			case 10:
				k := rapid.IntRange(1, 4).Draw(rt, "refusals")
				codes := make([]int, k)
				for j := range codes {
					codes[j] = rapid.SampledFrom([]int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable}).Draw(rt, "refuseCode")
				}
				e.px.RefuseNext(codes...)
				st.refusals.Add(1)
				trace = append(trace, fmt.Sprintf("refuse next %v", codes))
			case 11:
				if lifetimeOn {
					e.px.SetMaxLifetime(0)
					lifetimeOn = false
					fault("lifetime limit off")
				} else {
					e.px.SetMaxLifetime(time.Duration(rapid.IntRange(30, 300).Draw(rt, "lifetimeMs")) * time.Millisecond)
					lifetimeOn = true
					st.lifetimes.Add(1)
					fault("lifetime limit on")
				}
			case 12:
				if rapid.Bool().Draw(rt, "chunkOn") {
					e.px.SetChunking(rapid.IntRange(1, 4096).Draw(rt, "chunkMax"), 0)
					st.chunked.Add(1)
					trace = append(trace, "chunking on")
				} else {
					e.px.SetChunking(0, 0)
					trace = append(trace, "chunking off")
				}
			case 13:
				e.hub.Close()
				st.hubKills.Add(1)
				fault("hub-side kill")
			case 14:
				e.stopWorker()
				e.startWorker()
				lostFloor = len(appended)
				faultHellos = -1
				st.crashes.Add(1)
				trace = append(trace, "crash+restart")
			case 15, 16:
				// A snapshot push, one in six of them padded to megabytes
				// so a hub-to-worker frame far larger than a segment
				// crosses the splitter too.
				seq++
				snap := model.SiteSnapshot{SeqNum: seq, Site: model.SiteInfo{Slug: "site-a"}}
				if rapid.IntRange(0, 5).Draw(rt, "bigPush") == 0 {
					snap.SSHKeys = []string{strings.Repeat("k", rapid.IntRange(1<<20, 4<<20).Draw(rt, "pushLen"))}
					st.bigPushes.Add(1)
				}
				e.hub.Push("site-a", snap)
				st.pushes.Add(1)
				trace = append(trace, fmt.Sprintf("push seq %d (%d key bytes)", seq, len(strings.Join(snap.SSHKeys, ""))))
			default:
				time.Sleep(time.Duration(rapid.IntRange(0, 25).Draw(rt, "sleepMs")) * time.Millisecond)
				trace = append(trace, "sleep")
			}
		}

		// Clear every standing fault before quiescing. A half-closed or
		// blackholed pair may still be open; reset it so nothing waits on a
		// path that will never deliver.
		if lifetimeOn {
			e.px.SetMaxLifetime(0)
			lostFloor = len(appended)
		}
		e.px.SetChunking(0, 0)

		ok := e2eWaitFor(30*time.Second, func() bool {
			if e.outboxRows() != 0 || e.cache.LastSeqNum() != seq {
				return false
			}
			e.global.mu.Lock()
			defer e.global.mu.Unlock()
			for _, k := range keys {
				if terminal[k] {
					if !e.global.termAcked[k] {
						return false
					}
				} else if !e.global.okAcked[k] {
					return false
				}
			}
			return true
		})
		if !ok {
			e.global.mu.Lock()
			var missing []string
			for _, k := range keys {
				if !e.global.okAcked[k] && !e.global.termAcked[k] {
					missing = append(missing, fmt.Sprintf("%s(deliveries=%d,rejectsLeft=%d)", k.String()[:8], e.global.deliveries[k], e.global.reject[k]))
				}
			}
			e.global.mu.Unlock()
			rt.Fatalf("not quiescent after 30s: outbox rows=%d unacked=%v worker snapshot seq=%d (global pushed %d) connected=%v proxy=%+v last connect error=%s\ntrace=%v",
				e.outboxRows(), missing, e.cache.LastSeqNum(), seq, e.w.Connected(), e.px.Stats(), e.lastConnErr(), trace)
		}

		_ = e2eWaitFor(5*time.Second, func() bool {
			e.global.mu.Lock()
			defer e.global.mu.Unlock()
			tail := appended[lostFloor:]
			n := len(e.global.updates)
			return n >= len(tail) && sameUpdates(e.global.updates[n-len(tail):], tail)
		})
		e.global.mu.Lock()
		got := append([]model.OperationUpdate(nil), e.global.updates...)
		e.global.mu.Unlock()
		j := 0
		for _, u := range got {
			for j < len(appended) && !sameUpdates([]model.OperationUpdate{u}, []model.OperationUpdate{appended[j]}) {
				j++
			}
			if j == len(appended) {
				rt.Fatalf("hub received update %+v out of order, duplicated, or never appended\ngot=%+v\nappended=%+v\ntrace=%v", u, got, appended, trace)
			}
			j++
		}
		tail := appended[lostFloor:]
		if len(got) < len(tail) || !sameUpdates(got[len(got)-len(tail):], tail) {
			rt.Fatalf("updates appended after the last fault were not all delivered\ngot=%+v\ntail=%+v\ntrace=%v", got, tail, trace)
		}

		before := e.global.totalDeliveries()
		hellos := e.auth.hellos.Load()
		e.px.ResetAll()
		if !e2eWaitFor(20*time.Second, func() bool { return e.auth.hellos.Load() > hellos && e.w.Connected() }) {
			rt.Fatalf("worker did not reconnect within 20s after a proxy reset (last connect error: %s)", e.lastConnErr())
		}
		time.Sleep(30 * time.Millisecond)
		if after := e.global.totalDeliveries(); after != before {
			rt.Fatalf("%d deliveries after quiescence (acked rows redelivered)\ntrace=%v", after-before, trace)
		}

		// Every upgrade carried the token in the header and never in the URL.
		for _, r := range e.px.Requests() {
			st.upgrades.Add(1)
			if r.RequestURI != "/api/v1/ws/site" {
				rt.Fatalf("worker upgrade request line %q, want exactly /api/v1/ws/site with no query", r.RequestURI)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+e.token {
				rt.Fatalf("upgrade Authorization = %q, want the worker bearer token", got)
			}
		}

		e.global.mu.Lock()
		for _, d := range e.global.deliveries {
			if d > 1 {
				st.redeliveries.Add(int64(d - 1))
			}
		}
		e.global.mu.Unlock()
	})
	t.Logf("proxy coverage: cases=%d appends=%d big=%d updates=%d pushes=%d bigPushes=%d drops=%d resets=%d halfCloses=%d blackholes=%d restarts=%d refusals=%d lifetimes=%d chunked=%d hubKills=%d crashes=%d redeliveries=%d upgrades=%d",
		st.cases.Load(), st.appends.Load(), st.bigAppends.Load(), st.updates.Load(), st.pushes.Load(), st.bigPushes.Load(), st.drops.Load(), st.resets.Load(), st.halfCloses.Load(),
		st.blackholes.Load(), st.restarts.Load(), st.refusals.Load(), st.lifetimes.Load(), st.chunked.Load(), st.hubKills.Load(),
		st.crashes.Load(), st.redeliveries.Load(), st.upgrades.Load())
}

// Liveness timers for the tests below, in place of ws.HeartbeatInterval
// (30s) and ws.IdleTimeout (90s). The idle timeout is twenty heartbeats, not
// production's three, so a scheduling stall on a loaded test machine is not
// mistaken for a dead link.
const (
	pxHeartbeat = 50 * time.Millisecond
	pxIdle      = time.Second
	// pxSlack is how late a timer-driven event may be seen on a loaded
	// machine before a test calls it missing.
	pxSlack = 3 * time.Second
)

// TestWSConnLinkLiveness_Prop: both ends heartbeat every HeartbeatInterval
// and drop a link they have heard nothing on for IdleTimeout.
//
// While the path works, an idle link stays up, including behind a proxy
// that closes connections idle for a few heartbeat intervals, whether it
// counts bytes in both directions (a load balancer) or only from the server
// (a reverse proxy's read timeout): the heartbeats in each direction are the
// traffic.
//
// When the path stops delivering in either direction or both, with no FIN
// or RST reaching either end, the worker is on a new connection and global
// holds exactly that one about one idle timeout later, and no earlier than
// an idle timeout less a heartbeat, which would be a live link cut. What
// the worker appended meanwhile is delivered on the new connection, and a
// DHCP-lease read, which global fans out to every connection it holds for
// the site, gets one answer.
func TestWSConnLinkLiveness_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("each case waits out an idle timeout")
	}
	// A case waits out a proxy idle timeout several times over and then
	// the link's own idle timeout: about two seconds.
	lowerDefaultRapidChecks(t, 10)
	root := t.TempDir()
	var n atomic.Int64
	rapid.Check(t, func(rt *rapid.T) {
		dir := filepath.Join(root, fmt.Sprintf("l%d", n.Add(1)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rt.Fatalf("mkdir: %v", err)
		}
		e := newPxEnv(rt, dir, pxHeartbeat, pxIdle)
		defer e.close()
		e.w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo { return []dhcp.LeaseInfo{} })
		if !e2eWaitFor(20*time.Second, e.w.Connected) {
			rt.Fatalf("worker never connected within 20s (last connect error: %s)", e.lastConnErr())
		}

		// An idle link behind a proxy idle timeout.
		proxyIdle := time.Duration(rapid.IntRange(4, 8).Draw(rt, "proxyIdleHeartbeats")) * pxHeartbeat
		mode := rapid.SampledFrom([]string{"no idle timeout", "idle timeout counting both directions", "idle timeout counting server bytes only"}).Draw(rt, "proxyIdleMode")
		switch mode {
		case "idle timeout counting both directions":
			e.px.SetIdleTimeout(proxyIdle)
		case "idle timeout counting server bytes only":
			e.px.SetDownstreamIdleTimeout(proxyIdle)
		}
		time.Sleep(time.Duration(rapid.IntRange(2, 4).Draw(rt, "idleWindows")) * proxyIdle)
		if h, d := e.auth.hellos.Load(), e.px.Stats().Dropped; h != 1 || d != 0 || !e.w.Connected() || e.registrations() != 1 {
			rt.Fatalf("an idle link did not stay up behind a proxy with %s of %v (heartbeat %v, link idle timeout %v): hellos=%d, connections the proxy timed out=%d, worker connected=%v, connections global holds=%d, last connect error: %s",
				mode, proxyIdle, pxHeartbeat, pxIdle, h, d, e.w.Connected(), e.registrations(), e.lastConnErr())
		}
		e.px.SetIdleTimeout(0)
		e.px.SetDownstreamIdleTimeout(0)

		// The path goes silent.
		silence := rapid.SampledFrom([]string{"both directions", "worker to global", "global to worker", "frozen proxy"}).Draw(rt, "silence")
		start := time.Now()
		switch silence {
		case "both directions":
			e.px.BlackholeAll()
		case "worker to global":
			e.px.BlackholeUpAll()
		case "global to worker":
			e.px.BlackholeDownAll()
		case "frozen proxy":
			e.px.FreezeAll()
		}
		// Appended into the dead path: the observation is durable and must
		// arrive on the next connection.
		k := uuid.New()
		e.cache.appendObservation(model.Observation{MachineID: k, Event: "phone_home", Data: genObsData(rt)})

		replaced := func() bool { return e.auth.hellos.Load() >= 2 && e.w.Connected() && e.registrations() == 1 }
		if !e2eWaitFor(pxIdle+pxSlack, replaced) {
			rt.Fatalf("path silent (%s) for %v with a link idle timeout of %v: hellos=%d, worker connected=%v, connections global holds=%d; want the worker on a second connection and global holding only that one (last connect error: %s)",
				silence, time.Since(start).Round(time.Millisecond), pxIdle, e.auth.hellos.Load(), e.w.Connected(), e.registrations(), e.lastConnErr())
		}
		// The end that stopped hearing the other had heard from it at most
		// one heartbeat before the silence began.
		if took := time.Since(start); took < pxIdle-2*pxHeartbeat {
			rt.Fatalf("link replaced %v after its path went silent (%s); an end must hear nothing for the idle timeout of %v first", took.Round(time.Millisecond), silence, pxIdle)
		}

		acked := func() bool {
			e.global.mu.Lock()
			defer e.global.mu.Unlock()
			return e.global.okAcked[k]
		}
		if !e2eWaitFor(10*time.Second, func() bool { return acked() && e.outboxRows() == 0 }) {
			rt.Fatalf("the observation appended while the path was silent (%s) was not delivered on the new connection: outbox rows=%d", silence, e.outboxRows())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		leases := e.hub.DHCPLeases(ctx, "site-a")
		if len(leases) != 1 || leases[0].Err != "" {
			rt.Fatalf("DHCP lease read after the link was replaced (%s) = %+v, want one answer from the one worker", silence, leases)
		}
	})
}

// TestHubKeepsOneConnectionPerWorker: a worker has one link to global. When
// it connects again while global still holds its previous connection (the
// old path died without a FIN or RST and the worker restarted, or noticed
// first), the previous connection is dead by construction, and global
// should hold, and send RPCs to, only the new one.
func TestHubKeepsOneConnectionPerWorker(t *testing.T) {
	t.Skip("global keeps an older connection of the same worker ID registered until its own idle timeout (ws.IdleTimeout, 90s): meanwhile a DHCP-lease read reports the worker twice, once as an RPC timeout, and power and boot status reads pick either connection")
	e := newPxEnv(t, t.TempDir(), pxHeartbeat, 10*pxIdle)
	defer e.close()
	if !e2eWaitFor(20*time.Second, e.w.Connected) {
		t.Fatalf("worker never connected: %s", e.lastConnErr())
	}
	// The path dies silently and the worker process restarts: its close
	// never reaches global.
	e.px.BlackholeAll()
	e.stopWorker()
	e.startWorker()
	e.w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo { return []dhcp.LeaseInfo{} })
	if !e2eWaitFor(20*time.Second, func() bool { return e.auth.hellos.Load() >= 2 && e.w.Connected() }) {
		t.Fatalf("restarted worker never connected: %s", e.lastConnErr())
	}
	if got := e.registrations(); got != 1 {
		t.Errorf("global holds %d connections for worker-a after its second hello, want 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if leases := e.hub.DHCPLeases(ctx, "site-a"); len(leases) != 1 || leases[0].Err != "" {
		t.Errorf("DHCP lease read = %+v, want one answer from the one worker", leases)
	}
}

// TestWSConnRedialsWhenHelloReplyNeverArrives: the path can die between the
// upgrade and global's hello reply as well as later. The worker should give
// that connection up after a bounded wait and dial again.
func TestWSConnRedialsWhenHelloReplyNeverArrives(t *testing.T) {
	t.Skip("the worker reads the hello reply with no time limit, so a path that goes silent after the upgrade, with no FIN or RST, holds it disconnected for as long as the socket stays open; global likewise waits without limit for a hello")
	e := newPxEnv(t, t.TempDir(), pxHeartbeat, pxIdle)
	defer e.close()
	if !e2eWaitFor(20*time.Second, e.w.Connected) {
		t.Fatalf("worker never connected: %s", e.lastConnErr())
	}
	// The next connection completes the upgrade and then delivers nothing.
	e.px.SilenceNextAfterUpgrade(1)
	e.px.ResetAll()
	if !e2eWaitFor(10*time.Second, func() bool { return len(e.px.Requests()) >= 2 }) {
		t.Fatalf("worker did not redial after the reset: %s", e.lastConnErr())
	}
	if !e2eWaitFor(10*pxIdle, func() bool { return len(e.px.Requests()) >= 3 && e.w.Connected() }) {
		t.Fatalf("%v after its hello went unanswered on a silent path the worker is still waiting on that connection (upgrades=%d, connected=%v); want it to give up and connect again",
			10*pxIdle, len(e.px.Requests()), e.w.Connected())
	}
}

func openFDs() int {
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

// TestWSConnReconnectsDoNotLeak drops the worker's connection a few hundred
// times through the proxy (FIN, RST, half-close toward the worker, a short
// blackhole ending in an RST, an upgrade refusal) and checks that goroutines
// and file descriptors return to where they were and that the proxy holds
// only the live connection afterwards (a half-closed worker connection is
// closed by both ends, not left open on global).
func TestWSConnReconnectsDoNotLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("hundreds of reconnects")
	}
	const rounds = 300
	e := newPxEnv(t, t.TempDir(), 0, 0)
	defer e.close()
	if !e2eWaitFor(20*time.Second, e.w.Connected) {
		t.Fatalf("never connected: %s", e.lastConnErr())
	}
	settle := func() (int, int) {
		time.Sleep(300 * time.Millisecond)
		return runtime.NumGoroutine(), openFDs()
	}
	g0, f0 := settle()
	for i := 0; i < rounds; i++ {
		hellos := e.auth.hellos.Load()
		switch i % 5 {
		case 0:
			e.px.DropAll()
		case 1:
			e.px.ResetAll()
		case 2:
			e.px.HalfCloseDownAll()
		case 3:
			e.px.BlackholeAll()
			time.Sleep(2 * time.Millisecond)
			e.px.ResetAll()
		case 4:
			e.px.RefuseNext(http.StatusServiceUnavailable)
			e.px.ResetAll()
		}
		if !e2eWaitFor(20*time.Second, func() bool { return e.auth.hellos.Load() > hellos && e.w.Connected() }) {
			t.Fatalf("round %d: worker did not reconnect (last error %s, proxy %+v)", i, e.lastConnErr(), e.px.Stats())
		}
	}
	e2eWaitFor(5*time.Second, func() bool { return e.px.Open() <= 1 })
	g1, f1 := settle()
	t.Logf("after %d reconnects: goroutines %d -> %d, fds %d -> %d, proxy %+v", rounds, g0, g1, f0, f1, e.px.Stats())
	if g1 > g0+25 {
		t.Errorf("goroutines grew from %d to %d across %d reconnects", g0, g1, rounds)
	}
	if f0 >= 0 && f1 > f0+25 {
		t.Errorf("open file descriptors grew from %d to %d across %d reconnects", f0, f1, rounds)
	}
	if open := e.px.Open(); open != 1 {
		t.Errorf("proxy holds %d open connections after the churn, want 1 (the live one)", open)
	}
}
