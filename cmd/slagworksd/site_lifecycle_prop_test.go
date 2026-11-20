package main

// Process-level property test for the site worker's startup, readiness and
// degraded operation. Each case runs the real `slagworksd site` entry point
// (this test binary re-executed, calling main) as a child process under a
// fake systemd: a NOTIFY_SOCKET the test listens on, a start timeout, and
// signals for stop and kill. The child talks to a fake global built from the
// real ws.Hub with scripted collaborators, so every refusal the worker sees
// is one the production hub produces.
//
// A case is a short history of boots, global-side fault changes, snapshot
// bumps (some carrying a NetBox datum the worker's DHCP config cannot parse),
// stops and LocalDB damage. The model below predicts, without calling any
// production helper, what the worker should be doing at each checkpoint:
//
//   - alive: the process never exits because of something global said or
//     could not say (unreachable, 401, rebuild_failed, a hello left
//     unanswered, a refused serial, a malformed custom field in the snapshot).
//     The one thing that may end it is a worker.db SQLite cannot open, which
//     is fatal at start by design;
//   - ready: it tells systemd READY=1 within readyBudget of starting,
//     whatever global is doing, because the boot plane does not need global;
//   - serving: its HTTP boot server answers, and what it serves is the
//     snapshot generation the model says it holds: global's current one when
//     connected, else the last one it persisted (never an older one);
//   - connected: /metrics and /readyz say "connected" exactly when the fake
//     global is accepting hellos.
//
// The test is opt-in (SLAGWORKS_PROP_SLOW=1): a case takes about 25 seconds
// of real time, most of it runSite's own 5-second wait for global. Run it
// unprivileged: as root the child processes would bind DHCP and TFTP on
// loopback. SLAGWORKS_PROP_SHARDS sets how many cases run in parallel, and
// SLAGWORKS_PROP_DEBUG=1 adds the worker's goroutine stacks to a failure.

import (
	"context"
	"crypto/sha1" //nolint:gosec // the WebSocket handshake's accept hash is SHA-1 by specification
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

const (
	lcChildEnv = "SLAGWORKSD_LIFECYCLE_CHILD"
	lcSlug     = "site-a"
	lcWorkerID = "worker-a"
	lcToken    = "lifecycle-prop-token"

	// lcReadyBudget is how long a boot may take to report READY=1. runSite
	// waits at most 5s for a first snapshot before starting its services, so
	// a worker that reports readiness on what it can do locally is well
	// inside this even on a loaded host; systemd's own budget is 90s.
	lcReadyBudget = 20 * time.Second
)

// TestSiteWorkerChild is the child half of the lifecycle harness, not a
// test: with lcChildEnv set it becomes `slagworksd site`.
func TestSiteWorkerChild(t *testing.T) {
	if os.Getenv(lcChildEnv) != "1" {
		t.Skip("entry point for the child processes of TestProp_SiteLifecycle")
	}
	os.Args = []string{"slagworksd", "site"}
	main()
	os.Exit(0)
}

// --- fake global -----------------------------------------------------------

type lcMode int

const (
	lcOK            lcMode = iota // hello answered with the current snapshot
	lcUnreachable                 // the connection dies before the upgrade
	lcAuthRefused                 // 401 on the upgrade
	lcRebuildFailed               // cold hub, the site's snapshot cannot be built
	lcRebuildHang                 // cold hub, the build never returns (hub gives up after RebuildTimeout)
	lcSerialRefused               // serial enforcement on, the worker's serial does not resolve
	lcHelloHang                   // upgrade accepted, hello never answered
	lcNumModes
)

func (m lcMode) String() string {
	return [...]string{"ok", "unreachable", "auth-refused", "rebuild-failed", "rebuild-hang", "serial-refused", "hello-hang"}[m]
}

// lcShape is the DHCP part of a generated snapshot. The non-valid shapes are
// what global's builder emits, unvalidated, from free-text NetBox custom
// fields on the site's production prefix.
type lcShape int

const (
	lcShapeValid       lcShape = iota
	lcShapeGatewayCIDR         // gateway_ip typed with its mask
	lcShapeGatewayNone         // gateway_ip cleared
	lcShapeDNSNotIP            // dns_servers separated with ';' instead of ','
	lcNumShapes
)

func (s lcShape) String() string {
	return [...]string{"valid", "gateway-cidr", "gateway-empty", "dns-not-ip"}[s]
}

func lcSnapshot(gen int, shape lcShape) model.SiteSnapshot {
	dhcp := model.DHCPConfig{
		Subnet:             "10.77.0.0/24",
		Gateway:            "10.77.0.1",
		EphemeralPoolStart: "10.77.0.100",
		EphemeralPoolEnd:   "10.77.0.150",
	}
	switch shape {
	case lcShapeGatewayCIDR:
		dhcp.Gateway = "10.77.0.1/24"
	case lcShapeGatewayNone:
		dhcp.Gateway = ""
	case lcShapeDNSNotIP:
		dhcp.DNS = []string{"10.77.0.2; 10.77.0.3"}
	case lcShapeValid, lcNumShapes:
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "lc-machine-%d", gen))
	return model.SiteSnapshot{
		SeqNum: int64(gen),
		Site:   model.SiteInfo{Name: lcSlug, Slug: lcSlug},
		DHCP:   dhcp,
		Machines: []model.MachineSnapshot{{
			ID:       id,
			Hostname: fmt.Sprintf("host-%d", gen),
			State:    model.MachineStateRunning,
			Interfaces: []model.SnapshotInterface{{
				Name: "eth0", Type: "physical", MAC: fmt.Sprintf("02:00:00:00:%02x:%02x", gen>>8&0xff, gen&0xff), IP: "10.77.0.10/24",
			}},
		}},
		// The marker travels through the snapshot, LocalDB and the HTTP boot
		// server (/boot/ssh-keys), so reading it there says which generation
		// the boot plane is serving.
		SSHKeys: []string{lcMarker(gen)},
	}
}

func lcMarker(gen int) string { return fmt.Sprintf("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 gen-%d", gen) }

type lcAuth struct{ ok bool }

func (a lcAuth) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	sum := sha256.Sum256([]byte(lcToken))
	if !a.ok || h != hex.EncodeToString(sum[:]) {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: lcSlug, WorkerID: lcWorkerID}, nil
}

func (lcAuth) TouchSiteWorker(slug, workerID, fqdn string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (lcAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

type lcRebuilder struct {
	g    *lcGlobal
	mode lcMode
}

func (r lcRebuilder) RebuildAndWait(ctx context.Context, _ string) (model.SiteSnapshot, error) {
	switch r.mode {
	case lcRebuildFailed:
		return model.SiteSnapshot{}, errors.New("site site-a: no slagworks-production prefix")
	case lcRebuildHang:
		<-ctx.Done()
		return model.SiteSnapshot{}, ctx.Err()
	default:
		return *r.g.snap.Load(), nil
	}
}

type lcNoSerial struct{}

func (lcNoSerial) Resolve(context.Context, string, string) error { return ws.ErrSerialUnknown }

// lcGlobal is the fake global: one listener whose behaviour follows mode.
// Every mode change replaces the hub with a cold one and drops the
// connections of the old, which is what a global restart into that
// condition looks like from the worker.
type lcGlobal struct {
	ln   net.Listener
	srv  *http.Server
	mu   sync.Mutex
	mode lcMode
	hub  *ws.Hub
	snap atomic.Pointer[model.SiteSnapshot]

	upgrades atomic.Int64 // connection attempts that reached the handler
	hung     []net.Conn
}

func newLCGlobal(snap model.SiteSnapshot, mode lcMode) (*lcGlobal, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	g := &lcGlobal{ln: ln}
	g.snap.Store(&snap)
	g.setMode(mode)
	g.srv = &http.Server{Handler: http.HandlerFunc(g.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = g.srv.Serve(ln) }()
	return g, nil
}

func (g *lcGlobal) url() string { return "http://" + g.ln.Addr().String() }

func (g *lcGlobal) setMode(m lcMode) {
	cfg := ws.HubConfig{
		Auth:      lcAuth{ok: m != lcAuthRefused},
		Rebuilder: lcRebuilder{g: g, mode: m},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Hub.Version stays empty: with a version the worker would compare
		// builds, and a mismatch is what starts its self-upgrade.
		RebuildTimeout: 3 * time.Second,
	}
	if m == lcSerialRefused {
		cfg.SerialResolver = lcNoSerial{}
	}
	g.mu.Lock()
	old, hung := g.hub, g.hung
	g.mode, g.hub, g.hung = m, ws.NewHub(cfg), nil
	g.mu.Unlock()
	if old != nil {
		old.Close()
	}
	for _, c := range hung {
		_ = c.Close()
	}
}

// push publishes a new snapshot the way the dispatcher does: cache it in the
// hub and fan it out to the connected workers.
func (g *lcGlobal) push(snap model.SiteSnapshot) {
	g.snap.Store(&snap)
	g.mu.Lock()
	hub, mode := g.hub, g.mode
	g.mu.Unlock()
	if mode == lcOK {
		hub.Push(lcSlug, snap)
	}
}

func (g *lcGlobal) serve(w http.ResponseWriter, r *http.Request) {
	g.upgrades.Add(1)
	g.mu.Lock()
	mode, hub := g.mode, g.hub
	g.mu.Unlock()
	switch mode {
	case lcUnreachable, lcHelloHang:
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if mode == lcUnreachable {
			_ = conn.Close()
			return
		}
		// Complete the upgrade by hand and then say nothing, like a hub
		// stuck before its hello reply.
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1Accept(key)
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", sum)
		g.mu.Lock()
		g.hung = append(g.hung, conn)
		g.mu.Unlock()
	default:
		hub.ServeHTTP(w, r)
	}
}

// sha1Accept is the Sec-WebSocket-Accept value for a client key (RFC 6455).
func sha1Accept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // see import
	return base64.StdEncoding.EncodeToString(h[:])
}

func (g *lcGlobal) close() {
	g.mu.Lock()
	hub, hung := g.hub, g.hung
	g.hung = nil
	g.mu.Unlock()
	hub.Close()
	for _, c := range hung {
		_ = c.Close()
	}
	_ = g.srv.Close()
}

// --- fake systemd unit -----------------------------------------------------

type lcUnit struct {
	dir      string // per-case scratch
	dataDir  string // SLAGWORKS_DATA_DIR, survives restarts
	apiURL   string
	standby  bool
	httpPort int
	health   int

	cmd      *exec.Cmd
	started  time.Time
	notify   *net.UnixConn
	readyAt  atomic.Int64 // ns after started; 0 = not yet
	exited   chan struct{}
	exitCode int
	boots    int
}

func lcFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func newLCUnit(apiURL string) (*lcUnit, error) {
	// A unix socket path is limited to about 100 bytes; t.TempDir() under a
	// long test name can exceed that.
	dir, err := os.MkdirTemp("", "swlc")
	if err != nil {
		return nil, err
	}
	u := &lcUnit{dir: dir, dataDir: filepath.Join(dir, "data"), apiURL: apiURL}
	for _, d := range []string{u.dataDir, filepath.Join(dir, "tftp"), filepath.Join(dir, "images")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (u *lcUnit) dbPath() string  { return filepath.Join(u.dataDir, "worker.db") }
func (u *lcUnit) logPath() string { return filepath.Join(u.dir, fmt.Sprintf("boot-%d.log", u.boots)) }

func (u *lcUnit) running() bool {
	if u.cmd == nil {
		return false
	}
	select {
	case <-u.exited:
		return false
	default:
		return true
	}
}

func (u *lcUnit) start() error {
	if u.running() {
		return errors.New("unit already running")
	}
	u.boots++
	var err error
	if u.httpPort, err = lcFreePort(); err != nil {
		return err
	}
	if u.health, err = lcFreePort(); err != nil {
		return err
	}
	sock := filepath.Join(u.dir, fmt.Sprintf("n%d", u.boots))
	u.notify, err = net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify socket: %w", err)
	}
	u.readyAt.Store(0)

	logf, err := os.Create(u.logPath())
	if err != nil {
		return err
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSiteWorkerChild$", "-test.timeout=0", "-test.count=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	iface := "lo"
	if _, err := net.InterfaceByName("lo0"); err == nil {
		iface = "lo0"
	}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"),
		lcChildEnv + "=1",
		"NOTIFY_SOCKET=" + sock,
		"SLAGWORKS_API_URL=" + u.apiURL,
		"SLAGWORKS_TOKEN=" + lcToken,
		"SLAGWORKS_WORKER_ID=" + lcWorkerID,
		"SLAGWORKS_SERVER_IP=127.0.0.1",
		"SLAGWORKS_IFACE=" + iface,
		"SLAGWORKS_HTTP_PORT=" + strconv.Itoa(u.httpPort),
		"SLAGWORKS_HEALTH_ADDR=127.0.0.1:" + strconv.Itoa(u.health),
		"SLAGWORKS_DATA_DIR=" + u.dataDir,
		"SLAGWORKS_TFTP_DIR=" + filepath.Join(u.dir, "tftp"),
		"SLAGWORKS_IMAGES_DIR=" + filepath.Join(u.dir, "images"),
		"SLAGWORKS_AGENT_BIN=" + os.Args[0],
	}
	if u.standby {
		// 127.0.0.1 is always "held", so the standby loop activates at once.
		cmd.Env = append(cmd.Env, "SLAGWORKS_STANDBY=true", "SLAGWORKS_VIP=127.0.0.1")
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	u.cmd, u.started, u.exited = cmd, time.Now(), make(chan struct{})
	started, conn, exited := u.started, u.notify, u.exited
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "READY=1") {
				u.readyAt.CompareAndSwap(0, int64(time.Since(started))+1)
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		u.exitCode = 0
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			u.exitCode = ee.ExitCode()
		}
		_ = logf.Close()
		close(exited)
	}()
	return nil
}

// stop ends the unit with sig and waits for it. SIGTERM is `systemctl stop`,
// SIGKILL a crash or power cut.
func (u *lcUnit) stop(sig syscall.Signal) {
	if u.cmd == nil {
		return
	}
	if u.running() {
		_ = u.cmd.Process.Signal(sig)
		select {
		case <-u.exited:
		case <-time.After(20 * time.Second):
			_ = u.cmd.Process.Kill()
			<-u.exited
		}
	}
	_ = u.notify.Close()
	u.cmd = nil
}

func (u *lcUnit) cleanup() {
	u.stop(syscall.SIGKILL)
	_ = os.RemoveAll(u.dir)
}

func (u *lcUnit) logTail() string {
	b, err := os.ReadFile(u.logPath())
	if err != nil {
		return err.Error()
	}
	keep := 3000
	if os.Getenv("SLAGWORKS_PROP_DEBUG") == "1" {
		keep = 1 << 20
	}
	if len(b) > keep {
		b = b[len(b)-keep:]
	}
	return string(b)
}

var lcHTTP = &http.Client{Timeout: 2 * time.Second}

func lcGet(url string) (int, string, error) {
	resp, err := lcHTTP.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), nil
}

// servedGen reads which snapshot generation the HTTP boot server is serving:
// 0 for "no snapshot" (204), -1 when the server does not answer.
func (u *lcUnit) servedGen() int {
	code, body, err := lcGet(fmt.Sprintf("http://127.0.0.1:%d/boot/ssh-keys", u.httpPort))
	if err != nil {
		return -1
	}
	if code == http.StatusNoContent {
		return 0
	}
	_, after, ok := strings.Cut(body, " gen-")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(after))
	if err != nil {
		return -1
	}
	return n
}

// connected reads slagworks_site_ws_connected off the worker's /metrics:
// 1, 0, or -1 when the health server does not answer.
func (u *lcUnit) connected() int {
	_, body, err := lcGet(fmt.Sprintf("http://127.0.0.1:%d/metrics", u.health))
	if err != nil {
		return -1
	}
	for line := range strings.SplitSeq(body, "\n") {
		if v, ok := strings.CutPrefix(line, "slagworks_site_ws_connected "); ok {
			if strings.TrimSpace(v) == "1" {
				return 1
			}
			return 0
		}
	}
	return -1
}

func lcEventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- history and model -----------------------------------------------------

type lcOpKind int

const (
	lcOpBoot    lcOpKind = iota // start (or restart) the unit and check it
	lcOpMode                    // change what global does
	lcOpBump                    // global publishes a new snapshot generation
	lcOpStop                    // stop the unit with a signal
	lcOpCorrupt                 // damage worker.db while the unit is down
)

type lcOp struct {
	Kind    lcOpKind
	Mode    lcMode
	Shape   lcShape
	Kill    bool // SIGKILL instead of SIGTERM
	Corrupt int  // 0 garbage file, 1 truncated to half, 2 snapshot row replaced with non-JSON
}

func (o lcOp) String() string {
	switch o.Kind {
	case lcOpBoot:
		return "boot"
	case lcOpMode:
		return "mode=" + o.Mode.String()
	case lcOpBump:
		return "bump(" + o.Shape.String() + ")"
	case lcOpStop:
		if o.Kill {
			return "kill"
		}
		return "stop"
	default:
		return fmt.Sprintf("corrupt(%d)", o.Corrupt)
	}
}

func lcGenOps(t *rapid.T) []lcOp {
	faults := []lcMode{lcUnreachable, lcAuthRefused, lcRebuildFailed, lcRebuildHang, lcSerialRefused, lcHelloHang}
	shapes := []lcShape{lcShapeValid, lcShapeValid, lcShapeGatewayCIDR, lcShapeGatewayNone, lcShapeDNSNotIP}
	n := rapid.IntRange(1, 5).Draw(t, "nOps")
	ops := make([]lcOp, 0, 4*n+1)
	for range n {
		// Targeted patterns, between whatever random steps surround them.
		switch rapid.IntRange(0, 5).Draw(t, "pattern") {
		case 0:
			// Warm, change the snapshot under the running worker, then an
			// outage and a restart into it: the worker must come back
			// serving what it persisted.
			ops = append(ops, lcOp{Kind: lcOpMode, Mode: lcOK}, lcOp{Kind: lcOpBoot})
			for range rapid.IntRange(0, 2).Draw(t, "bumps") {
				ops = append(ops, lcOp{Kind: lcOpBump, Shape: rapid.SampledFrom(shapes).Draw(t, "shape")})
			}
			ops = append(ops, lcOp{Kind: lcOpMode, Mode: rapid.SampledFrom(faults).Draw(t, "mode")})
			if rapid.Bool().Draw(t, "stopFirst") {
				ops = append(ops, lcOp{Kind: lcOpStop, Kill: rapid.Bool().Draw(t, "kill")})
			}
			ops = append(ops, lcOp{Kind: lcOpBoot})
			continue
		case 1:
			// Start into a fault, then global recovers under the running
			// worker.
			ops = append(ops,
				lcOp{Kind: lcOpMode, Mode: rapid.SampledFrom(faults).Draw(t, "mode")},
				lcOp{Kind: lcOpBoot},
				lcOp{Kind: lcOpMode, Mode: lcOK})
			continue
		default:
		}
		kind := rapid.SampledFrom([]lcOpKind{lcOpBoot, lcOpBoot, lcOpBoot, lcOpMode, lcOpMode, lcOpBump, lcOpBump, lcOpStop, lcOpCorrupt}).Draw(t, "kind")
		op := lcOp{Kind: kind}
		switch kind {
		case lcOpMode:
			// ok is over-weighted so histories reach a persisted snapshot.
			op.Mode = rapid.SampledFrom([]lcMode{lcOK, lcOK, lcOK, lcUnreachable, lcAuthRefused, lcRebuildFailed, lcRebuildHang, lcSerialRefused, lcHelloHang}).Draw(t, "mode")
		case lcOpBump:
			op.Shape = rapid.SampledFrom([]lcShape{lcShapeValid, lcShapeValid, lcShapeGatewayCIDR, lcShapeGatewayNone, lcShapeDNSNotIP}).Draw(t, "shape")
		case lcOpStop:
			op.Kill = rapid.Bool().Draw(t, "kill")
		case lcOpCorrupt:
			op.Corrupt = rapid.IntRange(0, 2).Draw(t, "corrupt")
		case lcOpBoot:
		}
		ops = append(ops, op)
	}
	// Every history ends with a boot, so the state the earlier steps built
	// is always observed.
	return append(ops, lcOp{Kind: lcOpBoot})
}

type lcStats struct {
	mu                                          sync.Mutex
	cases, boots, bootsDegraded, bootsFromDisk  int
	modeSeen                                    [lcNumModes]int
	shapeSeen                                   [lcNumShapes]int
	corrupt, kills, terms, bumpsLive, reconnect int
	standby, dbFatal                            int
}

func (s *lcStats) add(f func(*lcStats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type lcFataler interface {
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

type lcRun struct {
	t     lcFataler
	stats *lcStats
	g     *lcGlobal
	u     *lcUnit

	// Model. Nothing here is read from the worker.
	mode      lcMode
	gen       int       // global's current generation
	shapes    []lcShape // shape of each generation, index = gen
	persisted int       // generation worker.db holds; 0 = none
	dbBroken  bool      // worker.db was damaged since it was last written
	connGen   int       // generation the running worker adopted on a live connection; 0 = none this boot
	trail     []string
}

func (r *lcRun) failf(format string, args ...any) {
	if os.Getenv("SLAGWORKS_PROP_DEBUG") == "1" && r.u.running() {
		// The Go runtime answers SIGQUIT with every goroutine's stack on
		// stderr, which is the worker's log.
		_ = r.u.cmd.Process.Signal(syscall.SIGQUIT)
		<-r.u.exited
	}
	r.t.Fatalf("%s\nhistory: %s\nmodel: mode=%v gen=%d persisted=%d dbBroken=%v\n--- worker log tail ---\n%s",
		fmt.Sprintf(format, args...), strings.Join(r.trail, " -> "), r.mode, r.gen, r.persisted, r.dbBroken, r.u.logTail())
}

func (r *lcRun) boot() {
	if r.u.running() || r.u.cmd != nil {
		r.u.stop(syscall.SIGTERM)
	}
	r.connGen = 0
	attemptsBefore := r.g.upgrades.Load()
	if err := r.u.start(); err != nil {
		r.t.Fatalf("start unit: %v", err)
	}
	r.stats.add(func(s *lcStats) {
		s.boots++
		s.modeSeen[r.mode]++
		if r.mode != lcOK {
			s.bootsDegraded++
			if r.persisted > 0 && !r.dbBroken {
				s.bootsFromDisk++
			}
		}
		if r.u.standby {
			s.standby++
		}
	})

	// Alive and ready: nothing global does, and nothing it put in a
	// snapshot, is a reason for the boot plane to go away or for the unit to
	// miss its start timeout.
	budget := lcReadyBudget
	ready := lcEventually(budget, func() bool { return r.u.readyAt.Load() != 0 || !r.u.running() })
	if !r.u.running() {
		if !r.dbBroken {
			r.failf("worker exited with code %d under global mode %v", r.u.exitCode, r.mode)
		}
		// A worker.db that SQLite cannot open is fatal at start, with a
		// non-zero exit: the damaged file stays where it is, and nothing
		// changes until an operator looks at it.
		if r.u.exitCode == 0 {
			r.failf("worker exited with code 0 on a damaged worker.db, want a failure exit")
		}
		r.stats.add(func(s *lcStats) { s.dbFatal++ })
		r.u.stop(syscall.SIGKILL)
		return
	}
	if !ready || r.u.readyAt.Load() == 0 {
		r.failf("no READY=1 within %v of starting (global mode %v): systemd would kill the unit at its start timeout and restart it for as long as that lasts", budget, r.mode)
	}

	// Serving: the HTTP boot server answers and serves the generation the
	// model says the worker holds.
	lo, hi := 0, 0
	switch {
	case r.mode == lcOK:
		lo, hi = r.gen, r.gen
	case r.dbBroken:
		// The damage may or may not have reached the snapshot rows: serving
		// nothing, or anything the file ever held, is acceptable; a
		// generation it never held is not.
		hi = r.persisted
	case r.persisted > 0:
		lo, hi = r.persisted, r.persisted
	}
	var got int
	if !lcEventually(10*time.Second, func() bool { got = r.u.servedGen(); return (got >= lo && got <= hi) || !r.u.running() }) {
		r.failf("HTTP boot serves generation %d, want %d..%d (global mode %v)", got, lo, hi, r.mode)
	}
	if !r.u.running() {
		r.failf("worker exited with code %d while starting its services (global mode %v)", r.u.exitCode, r.mode)
	}
	if r.dbBroken && r.mode != lcOK {
		// What it serves is what the file holds now.
		r.persisted, r.dbBroken = got, false
	}

	// No retry storm: while global refuses, the worker's attempts follow its
	// backoff ladder (1s doubling), so they grow with the logarithm of the
	// time waited, not with the time.
	if r.mode != lcOK {
		elapsed := time.Since(r.u.started).Seconds()
		allowed := int64(2 + math.Log2(1+elapsed))
		if n := r.g.upgrades.Load() - attemptsBefore; n > allowed {
			r.failf("%d connection attempts in the first %.1fs under global mode %v, want at most %d", n, elapsed, r.mode, allowed)
		}
	}

	// Connected: the worker's own view matches what global is doing.
	wantConn := 0
	if r.mode == lcOK {
		wantConn = 1
	}
	var conn int
	if !lcEventually(5*time.Second, func() bool { conn = r.u.connected(); return conn == wantConn }) {
		r.failf("slagworks_site_ws_connected = %d, want %d (global mode %v)", conn, wantConn, r.mode)
	}
	if r.mode == lcOK {
		r.adopted()
	}
}

// adopted records that the running worker holds global's current generation
// from a live connection, and gives its LocalDB write time to land before a
// later step kills the process.
func (r *lcRun) adopted() {
	r.connGen = r.gen
	time.Sleep(150 * time.Millisecond)
	r.persisted, r.dbBroken = r.gen, false
}

func (r *lcRun) setMode(m lcMode) {
	prev := r.mode
	r.mode = m
	r.g.setMode(m)
	if !r.u.running() {
		return
	}
	// The mode change dropped the worker's connection. It must notice, keep
	// serving what it had, and stay up.
	if prev == lcOK {
		var conn int
		// The bound is loose: the worker's read loop persists each snapshot
		// to worker.db before it reads on, so on a host whose disk is busy
		// it can be seconds behind the socket.
		if !lcEventually(15*time.Second, func() bool { conn = r.u.connected(); return conn == 0 || m == lcOK }) {
			r.failf("global dropped the connection (now %v) but slagworks_site_ws_connected = %d", m, conn)
		}
	}
	if m == lcOK {
		// Recovery. After an outage the worker is somewhere on its backoff
		// ladder (capped at 30s); after a plain drop the ladder restarts at
		// 1s. Either way reconnection is bounded by the cap.
		if prev != lcOK {
			r.stats.add(func(s *lcStats) { s.reconnect++ })
		}
		time.Sleep(300 * time.Millisecond) // let the worker notice the drop
		var conn int
		if !lcEventually(40*time.Second, func() bool { conn = r.u.connected(); return conn == 1 || !r.u.running() }) || !r.u.running() {
			r.failf("global has been answering for 40s but the worker did not reconnect (connected=%d, running=%v)", conn, r.u.running())
		}
		var got int
		if !lcEventually(5*time.Second, func() bool { got = r.u.servedGen(); return got == r.gen }) {
			r.failf("reconnected worker serves generation %d, want %d", got, r.gen)
		}
		r.adopted()
		return
	}
	want := r.connGen
	if want == 0 && r.persisted > 0 && !r.dbBroken {
		want = r.persisted
	}
	if got := r.u.servedGen(); got != want {
		r.failf("after global went %v the worker serves generation %d, want %d (what it held)", m, got, want)
	}
}

func (r *lcRun) bump(shape lcShape) {
	r.gen++
	r.shapes = append(r.shapes, shape)
	r.stats.add(func(s *lcStats) { s.shapeSeen[shape]++ })
	r.g.push(lcSnapshot(r.gen, shape))
	if !r.u.running() || r.mode != lcOK || r.connGen == 0 {
		return
	}
	r.stats.add(func(s *lcStats) { s.bumpsLive++ })
	// A connected worker applies the push. A DHCP config it cannot parse
	// costs it the pool update, not the process.
	var got int
	if !lcEventually(5*time.Second, func() bool { got = r.u.servedGen(); return got == r.gen || !r.u.running() }) {
		r.failf("connected worker serves generation %d after a push of %d", got, r.gen)
	}
	if !r.u.running() {
		r.failf("worker exited with code %d on a pushed snapshot (shape %v)", r.u.exitCode, shape)
	}
	r.adopted()
}

func (r *lcRun) corrupt(kind int) {
	if r.u.running() || r.u.cmd != nil {
		r.u.stop(syscall.SIGKILL)
	}
	path := r.u.dbPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return // never created: nothing to damage
	}
	r.stats.add(func(s *lcStats) { s.corrupt++ })
	switch kind {
	case 0:
		// Not a database at all; its write-ahead log goes with it.
		b = []byte(strings.Repeat("not a sqlite database\n", 400))
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
	case 1:
		b = b[:len(b)/2]
	default:
		// Flip the page-1 header's schema format number to a value SQLite
		// refuses, leaving the rest of the file intact.
		if len(b) > 48 {
			b[44], b[45], b[46], b[47] = 0xff, 0xff, 0xff, 0xff
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		r.t.Fatalf("damage worker.db: %v", err)
	}
	r.dbBroken = true
}

func (r *lcRun) apply(op lcOp) {
	r.trail = append(r.trail, op.String())
	switch op.Kind {
	case lcOpBoot:
		r.boot()
	case lcOpMode:
		r.setMode(op.Mode)
	case lcOpBump:
		r.bump(op.Shape)
	case lcOpStop:
		if !r.u.running() {
			return
		}
		sig := syscall.SIGTERM
		if op.Kill {
			sig = syscall.SIGKILL
		}
		r.stats.add(func(s *lcStats) {
			if op.Kill {
				s.kills++
			} else {
				s.terms++
			}
		})
		r.u.stop(sig)
		if !op.Kill && r.u.exitCode != 0 {
			r.failf("worker exited with code %d on SIGTERM, want 0", r.u.exitCode)
		}
	case lcOpCorrupt:
		r.corrupt(op.Corrupt)
	}
}

func lcRunCase(t lcFataler, stats *lcStats, ops []lcOp, startMode lcMode, standby bool) {
	g, err := newLCGlobal(lcSnapshot(1, lcShapeValid), startMode)
	if err != nil {
		t.Fatalf("fake global: %v", err)
	}
	defer g.close()
	u, err := newLCUnit(g.url())
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	u.standby = standby
	defer u.cleanup()
	r := &lcRun{
		t: t, stats: stats,
		g: g, u: u, mode: startMode, gen: 1, shapes: []lcShape{lcShapeValid, lcShapeValid},
	}
	r.trail = append(r.trail, "mode="+startMode.String())
	stats.add(func(s *lcStats) { s.cases++ })
	for _, op := range ops {
		r.apply(op)
	}
}

func lcShards() int {
	if v, err := strconv.Atoi(os.Getenv("SLAGWORKS_PROP_SHARDS")); err == nil && v > 0 {
		return v
	}
	return 2
}

func TestProp_SiteLifecycle(t *testing.T) {
	if os.Getenv("SLAGWORKS_PROP_SLOW") != "1" {
		t.Skip("starts real worker processes and waits out their startup timers (about 25s a case); set SLAGWORKS_PROP_SLOW=1 to run it")
	}
	stats := &lcStats{}
	// Cases are slow (a degraded boot waits out runSite's 5s sync window), so
	// they run as parallel shards, each its own rapid.Check with its own seed.
	t.Run("shards", func(t *testing.T) {
		for i := range lcShards() {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				rapid.Check(t, func(rt *rapid.T) {
					startMode := rapid.SampledFrom([]lcMode{lcOK, lcOK, lcUnreachable, lcAuthRefused, lcRebuildFailed, lcRebuildHang, lcSerialRefused, lcHelloHang}).Draw(rt, "startMode")
					standby := rapid.IntRange(0, 4).Draw(rt, "standby") == 0
					ops := lcGenOps(rt)
					lcRunCase(rt, stats, ops, startMode, standby)
				})
			})
		}
	})
	stats.mu.Lock()
	defer stats.mu.Unlock()
	t.Logf("coverage: cases=%d boots=%d (degraded=%d, of those from a persisted snapshot=%d, standby=%d) reconnects-after-outage=%d live-bumps=%d corruptions=%d kills=%d stops=%d",
		stats.cases, stats.boots, stats.bootsDegraded, stats.bootsFromDisk, stats.standby, stats.reconnect, stats.bumpsLive, stats.corrupt, stats.kills, stats.terms)
	t.Logf("boots by global mode: %v", func() string {
		var b strings.Builder
		for m := range lcNumModes {
			fmt.Fprintf(&b, "%v=%d ", m, stats.modeSeen[m])
		}
		return b.String()
	}())
	t.Logf("bumps by shape: %v", func() string {
		var b strings.Builder
		for s := range lcNumShapes {
			fmt.Fprintf(&b, "%v=%d ", s, stats.shapeSeen[s])
		}
		return b.String()
	}())
	t.Logf("starts that failed on a worker.db SQLite could not open (fatal by design): %d", stats.dbFatal)
}
