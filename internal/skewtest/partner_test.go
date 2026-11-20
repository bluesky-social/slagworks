package skewtest

// Two workers on one site, one of them cut off from global.
//
// A site with two active workers answers a machine's DHCP and serves its
// boot script from whichever worker wins the race. Both hold the site's
// operations from the op stream. A worker that loses global (its uplink is
// down, global refuses its build, its token was rotated) keeps serving
// boots from what it last knew, by design; what it last knew about an
// operation its partner is driving goes stale the moment the connection
// drops.
//
// TestPartitionedPartnerWorker builds the case where that matters: a deploy
// in flight when one worker is cut off, finished through the other worker,
// and then a reboot of the installed host that lands on the cut-off one. It
// checks the half two real processes can show in seconds: the connected
// worker follows global (boot-local after the abort and after the install),
// the deploy completes through it, and the cut-off worker keeps answering
// without disturbing what global recorded.

import (
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tcpProxy forwards connections to target until cut.
type tcpProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	down  bool
}

func newTCPProxy(f fataler, target string) *tcpProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.Fatalf("harness: proxy listen: %v", err)
	}
	p := &tcpProxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.down {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	return p
}

func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

// cut drops every connection and refuses new ones: the worker behind the
// proxy can no longer reach global.
func (p *tcpProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *tcpProxy) close() { p.cut(); _ = p.ln.Close() }

func TestPartitionedPartnerWorker(t *testing.T) {
	name, dsn := freshDatabase(t, t, "")
	defer dropDatabase(name)
	gAddr := freePort(t)
	g := startProc(t, toolNew, "global")
	defer g.kill()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const mac, mac2 = "02:00:00:00:00:01", "02:00:00:00:00:02"
	must("g.start", g.call("g.start", map[string]any{"dsn": dsn, "addr": gAddr, "slug": "site-a", "version": "main"}, nil))
	var seeded struct {
		Tokens   map[string]string `json:"tokens"`
		Machines []string          `json:"machines"`
	}
	must("g.seed", g.call("g.seed", map[string]any{"workers": []string{"worker-a", "worker-b"}, "machines": []map[string]any{
		{"serial": "PARTNER01", "mac": mac, "state": "commissioned"},
		{"serial": "PARTNER02", "mac": mac2, "state": "running", "locked": true},
	}}, &seeded))
	machine, machine2 := seeded.Machines[0], seeded.Machines[1]

	// Worker B reaches global through a proxy the test can cut. Only A runs
	// an executor, so A is the claimer and B knows the op from the stream.
	proxy := newTCPProxy(t, gAddr)
	defer proxy.close()
	dir := t.TempDir()
	startWorker := func(id, url string, noExecutor bool) (*proc, string) {
		p := startProc(t, toolNew, id)
		httpAddr := freePort(t)
		must("w.start "+id, p.call("w.start", map[string]any{
			"db": filepath.Join(dir, id+".db"), "url": "http://" + url, "token": seeded.Tokens[id], "worker_id": id,
			"serial": "SER-" + id, "http_addr": httpAddr, "version": "main", "no_executor": noExecutor,
		}, nil))
		return p, httpAddr
	}
	a, aHTTP := startWorker("worker-a", gAddr, false)
	defer a.kill()
	b, bHTTP := startWorker("worker-b", proxy.addr(), true)
	defer b.kill()
	status := func(p *proc, ops ...string) wStatus {
		var s wStatus
		must("w.status", p.call("w.status", map[string]any{"ops": ops, "machines": []string{machine}}, &s))
		return s
	}
	if !eventually(20*time.Second, func() bool { return status(a).Connected && status(b).Connected }) {
		t.Fatal("harness: the two workers never connected")
	}

	// A deploy starts: A claims it and power-cycles the host; the host
	// boots the installer from A and its agent reports a phase.
	var created struct {
		OpID string `json:"op_id"`
	}
	must("create deploy", g.call("g.create_op", map[string]any{"machine_id": machine, "type": "deploy"}, &created))
	op := created.OpID
	if !eventually(5*time.Second, func() bool { return status(a).Actuations[machine]["power_cycle"] == 1 }) {
		t.Fatal("harness: worker A never power-cycled the host")
	}
	if _, script := get("http://" + aHTTP + "/boot/ipxe?mac=" + mac); classify(script) != "deploy" {
		t.Fatalf("harness: worker A did not serve the installer for a claimed deploy: %q", script)
	}
	report := func(httpAddr, phase, event string) int {
		body := map[string]any{"machine_id": machine, "op_id": op}
		if phase != "" {
			body["phase"] = phase
		}
		if event != "" {
			body["event"] = event
		}
		code, _ := post("http://"+httpAddr+"/api/v1/report", body)
		return code
	}
	if code := report(aHTTP, "rsyncing_os", ""); code != 200 {
		t.Fatalf("harness: phase report to A got %d", code)
	}
	// B has seen the deploy on the op stream.
	if !eventually(5*time.Second, func() bool {
		o, ok := status(b, op).Ops[op]
		return ok && o.Phase == "rsyncing_os"
	}) {
		t.Fatalf("harness: worker B never saw the deploy's phase: %+v", status(b, op).Ops)
	}

	// A second deploy, created against the wrong host: a running, locked
	// machine the operator unlocked by mistake. Both workers hold it.
	var wrong struct {
		OpID string `json:"op_id"`
	}
	must("create second deploy", g.call("g.create_op", map[string]any{"machine_id": machine2, "type": "deploy", "unlock": true}, &wrong))
	if !eventually(5*time.Second, func() bool {
		_, ok := status(b, wrong.OpID).Ops[wrong.OpID]
		return ok && status(a).Actuations[machine2]["power_cycle"] == 1
	}) {
		t.Fatal("harness: the second deploy never reached both workers")
	}

	// B loses global. The operator aborts the mistaken deploy, and A
	// finishes the first one.
	proxy.cut()
	if !eventually(10*time.Second, func() bool { return !status(b).Connected }) {
		t.Fatal("harness: worker B still reports connected after its uplink was cut")
	}
	var aborted struct {
		Failed []string `json:"failed"`
	}
	must("abort", g.call("g.abort", map[string]any{"machine_id": machine2}, &aborted))
	if len(aborted.Failed) != 1 || aborted.Failed[0] != wrong.OpID {
		t.Fatalf("harness: abort failed %v, want the second deploy", aborted.Failed)
	}
	if !eventually(5*time.Second, func() bool {
		_, script := get("http://" + aHTTP + "/boot/ipxe?mac=" + mac2)
		return classify(script) == "local"
	}) {
		t.Fatal("worker A still serves the installer for the aborted deploy")
	}
	if code := report(aHTTP, "", "deploy_complete"); code != 200 {
		t.Fatalf("harness: deploy_complete to A got %d", code)
	}
	if _, script := get("http://" + aHTTP + "/boot/ipxe?mac=" + mac); classify(script) != "local" {
		t.Fatalf("worker A did not serve boot-local after deploy_complete: %q", script)
	}
	if code := report(aHTTP, "", "phone_home"); code != 200 {
		t.Fatalf("harness: phone_home to A got %d", code)
	}
	var d gDump
	if !eventually(10*time.Second, func() bool {
		must("g.dump", g.call("g.dump", struct{}{}, &d))
		o := d.op(op)
		return o != nil && o.Status == "completed"
	}) {
		t.Fatalf("harness: the deploy did not complete at global: %+v", d.op(op))
	}
	if m := d.machine(machine); m.State != "running" || !m.Locked {
		t.Fatalf("harness: machine is %s (locked=%v) at global, want running and locked", m.State, m.Locked)
	}

	// The installed host reboots, and this time worker B answers its DHCP.
	// B was cut off seconds ago, and a worker trusts the op set it held when
	// its connection ended for as long as the operation timeout (it serves
	// boot-local for those hosts only once the outage outlasts that window;
	// TestBootAction_LongDisconnectedWorkerDoesNotServeInstaller in
	// internal/worker covers that with the clock moved). So inside the
	// window B still answers from what it last knew. What must hold here is
	// that B keeps answering at all, and that nothing it does reaches
	// global: the finished deploy stays completed and its machine running
	// and locked.
	code, script := get("http://" + bHTTP + "/boot/ipxe?mac=" + mac)
	if code != 200 {
		t.Fatalf("worker B, cut off from global, answered HTTP %d to a boot request: boot serving must not depend on the connection", code)
	}
	accepted := report(bHTTP, "partitioning", "")
	t.Logf("inside the trust window the cut-off partner serves the completed host: %s; it answers %d to an installer agent's report", classify(script), accepted)
	code2, script2 := get("http://" + bHTTP + "/boot/ipxe?mac=" + mac2)
	if code2 != 200 {
		t.Fatalf("worker B, cut off from global, answered HTTP %d to a boot request for the second host", code2)
	}
	t.Logf("inside the trust window the cut-off partner serves the host whose deploy was aborted: %s (destructive flag on the kernel line: %v)", classify(script2), strings.Contains(script2, "slagworks.destructive"))
	time.Sleep(500 * time.Millisecond)
	must("g.dump", g.call("g.dump", struct{}{}, &d))
	if o := d.op(op); o == nil || o.Status != "completed" {
		t.Fatalf("the completed deploy changed at global after the cut-off partner took a report for it: %+v", o)
	}
	if m := d.machine(machine); m.State != "running" || !m.Locked {
		t.Fatalf("machine is %s (locked=%v) at global after the cut-off partner took a report, want running and locked", m.State, m.Locked)
	}
	if o := d.op(wrong.OpID); o == nil || o.Status != "failed" {
		t.Fatalf("the aborted deploy is not failed at global: %+v", o)
	}
}
