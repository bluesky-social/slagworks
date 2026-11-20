//go:build integration

package qemutest

// propWorker is the history tests' site worker: the production Cache,
// LocalDB, WSConn, Executor, DHCP handler and HTTP boot server, wired the
// way cmd/slagworksd/site.go wires them (including the DHCP OnACK phase
// hook the stock qemutest SiteWorker omits), with three additions:
//
//   - every request to the HTTP boot server passes through a recorder that
//     logs the boot script served / the agent report and its HTTP status,
//     and can fire a fault at a chosen request ("before the worker handles
//     deploy_complete", "after boot-local is served");
//   - Kill/Start model a process restart: everything in memory is dropped
//     and a fresh Cache rehydrates from the same LocalDB;
//   - a "sim" mode with no netns/DHCP/TFTP, where a simulated host calls the
//     same HTTP handler in-process, so the identical interpreter and oracle
//     run a history in seconds without a VM.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/logring"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// propHTTPEvent is one request observed at the worker's HTTP boot server.
type propHTTPEvent struct {
	Seq       int
	At        time.Time
	Inc       int    // worker incarnation that served it
	Kind      string // boot | report | dhcp | mac_phone_home
	MAC       string
	Machine   string
	Op        string
	Boot      string // local | deploy | commission | refuse
	Phase     string
	Event     string
	Failed    bool
	Status    int  // 0 = connection aborted (worker killed mid-request)
	Synthetic bool // injected by the harness, not sent by the host
	Connected bool // worker's WebSocket to global was up when served
	// Diag is the worker's own view right after it answered (effective
	// machine state, the op it holds): diagnosis only, never an oracle
	// input.
	Diag string
	// Began is when the worker's handler started (after any "before"
	// faults armed on this request ran), and Took how long it ran.
	Began time.Time
	Took  time.Duration
}

func (e propHTTPEvent) String() string {
	s := fmt.Sprintf("%s #%d inc%d %-6s", e.At.Format("15:04:05.000"), e.Seq, e.Inc, e.Kind)
	switch e.Kind {
	case "boot":
		s += fmt.Sprintf(" mac=%s -> %s", e.MAC, e.Boot)
	case "report":
		s += fmt.Sprintf(" phase=%q event=%q failed=%v", e.Phase, e.Event, e.Failed)
	case "mac_phone_home":
		s += " mac=" + e.MAC
	}
	if e.Op != "" {
		s += " op=" + e.Op[:8]
	}
	s += fmt.Sprintf(" status=%d", e.Status)
	if e.Synthetic {
		s += " synthetic"
	}
	if !e.Connected {
		s += " ws-down"
	}
	if e.Took > 100*time.Millisecond {
		s += fmt.Sprintf(" took=%s", e.Took.Round(time.Millisecond))
	}
	if e.Diag != "" {
		s += " [" + e.Diag + "]"
	}
	return s
}

// propTrigger fires an action at a matching request, once.
type propTrigger struct {
	name   string
	before bool
	match  func(e *propHTTPEvent) bool
	action func(e *propHTTPEvent)
	nth    int32
	seen   atomic.Int32
	fired  atomic.Bool
}

type propRecorder struct {
	mu       sync.Mutex
	events   []propHTTPEvent
	triggers []*propTrigger
	changed  chan struct{}
	// lastProgress: see LastProgress.
	lastProgress time.Time

	// dead aborts every request: the worker process is gone.
	dead atomic.Bool
	// inflight counts requests the worker is still handling.
	inflight atomic.Int32
}

func newPropRecorder() *propRecorder {
	return &propRecorder{changed: make(chan struct{})}
}

func (r *propRecorder) Events() []propHTTPEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]propHTTPEvent(nil), r.events...)
}

// LastProgress is the time of the last request that was not a rejected
// report.
func (r *propRecorder) LastProgress() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastProgress
}

func (r *propRecorder) append(e *propHTTPEvent) {
	r.mu.Lock()
	e.Seq = len(r.events)
	r.events = append(r.events, *e)
	if e.Kind != "report" || (e.Status >= 200 && e.Status < 300) {
		r.lastProgress = e.At
	}
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

// Arm registers a trigger.
func (r *propRecorder) Arm(t *propTrigger) {
	if t.nth <= 0 {
		t.nth = 1
	}
	r.mu.Lock()
	r.triggers = append(r.triggers, t)
	r.mu.Unlock()
}

func (r *propRecorder) fire(before bool, e *propHTTPEvent) {
	r.mu.Lock()
	var due []*propTrigger
	for _, t := range r.triggers {
		if t.before != before || t.fired.Load() || !t.match(e) {
			continue
		}
		if t.seen.Add(1) != t.nth {
			continue
		}
		t.fired.Store(true)
		due = append(due, t)
	}
	r.mu.Unlock()
	for _, t := range due {
		t.action(e)
	}
}

// WaitFor blocks until an already-recorded or future event matches.
func (r *propRecorder) WaitFor(match func(e *propHTTPEvent) bool, from int, timeout time.Duration) (propHTTPEvent, bool) {
	deadline := time.After(timeout)
	for {
		r.mu.Lock()
		for i := from; i < len(r.events); i++ {
			if match(&r.events[i]) {
				e := r.events[i]
				r.mu.Unlock()
				return e, true
			}
		}
		from = len(r.events)
		ch := r.changed
		r.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return propHTTPEvent{}, false
		}
	}
}

var (
	propOpArg      = regexp.MustCompile(`slagworks\.op=([0-9a-f-]{36})`)
	propMachineArg = regexp.MustCompile(`slagworks\.machine=([0-9a-f-]{36})`)
)

func propClassifyBootScript(body string) (kind, machine, op string) {
	switch {
	case strings.Contains(body, "slagworks: deploy boot"):
		kind = "deploy"
	case strings.Contains(body, "slagworks: commission boot"):
		kind = "commission"
	case strings.Contains(body, "slagworks: boot-local"):
		kind = "local"
	default:
		kind = "refuse"
	}
	if m := propOpArg.FindStringSubmatch(body); m != nil {
		op = m[1]
	}
	if m := propMachineArg.FindStringSubmatch(body); m != nil {
		machine = m[1]
	}
	return kind, machine, op
}

type propCaptureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *propCaptureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *propCaptureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len() < 64<<10 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (r *propRecorder) wrap(next http.Handler, inc int, connected func() bool, diag func(mac, machine string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.dead.Load() {
			panic(http.ErrAbortHandler)
		}
		ev := propHTTPEvent{At: time.Now(), Inc: inc, Synthetic: req.Header.Get("X-Prop-Synthetic") != ""}
		switch req.URL.Path {
		case "/boot/ipxe":
			ev.Kind = "boot"
			ev.MAC = strings.ToLower(req.URL.Query().Get("mac"))
		case "/api/v1/phone-home":
			ev.Kind = "mac_phone_home"
			ev.MAC = strings.ToLower(req.URL.Query().Get("mac"))
		case "/api/v1/report":
			body, _ := io.ReadAll(io.LimitReader(req.Body, 16<<20))
			req.Body = io.NopCloser(bytes.NewReader(body))
			var rep model.ProgressReport
			if json.Unmarshal(body, &rep) == nil && (rep.Phase != "" || rep.Event != "" || rep.Failed) {
				ev.Kind = "report"
				ev.Machine, ev.Op = rep.MachineID, rep.OperationID
				ev.Phase, ev.Event, ev.Failed = rep.Phase, rep.Event, rep.Failed
			}
		}
		if ev.Kind == "" {
			next.ServeHTTP(w, req)
			return
		}
		ev.Connected = connected()
		r.fire(true, &ev)
		if r.dead.Load() {
			r.append(&ev)
			panic(http.ErrAbortHandler)
		}
		cw := &propCaptureWriter{ResponseWriter: w}
		began := time.Now()
		r.inflight.Add(1)
		func() {
			defer r.inflight.Add(-1)
			next.ServeHTTP(cw, req)
		}()
		ev.Began, ev.Took = began, time.Since(began)
		ev.Status = cw.status
		if ev.Status == 0 {
			ev.Status = http.StatusOK
		}
		if ev.Kind == "boot" {
			ev.Boot, ev.Machine, ev.Op = propClassifyBootScript(cw.body.String())
		}
		if diag != nil && (ev.Kind == "boot" || ev.Event != "") {
			ev.Diag = diag(ev.MAC, ev.Machine)
		}
		r.append(&ev)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		r.fire(false, &ev)
	})
}

// propHost is a machine as the power plane and the oracle see it.
type propHost interface {
	Name() string
	MAC() string
	PowerCycle() error
	PowerOff() error
	PowerOn() error
	PowerState() (power.State, error)
	// DiskWrites is the number of write operations the primary disk has
	// seen since the host was created (-1 when unobservable).
	DiskWrites() int64
}

// propPowerAction is one actuation the executor (or a status RPC) performed
// through the power controller factory.
type propPowerAction struct {
	At      time.Time
	Machine uuid.UUID
	Action  string // cycle | on | off | set_boot
}

// propPowerCtl is the power.Controller the executor drives.
type propPowerCtl struct {
	w       *propWorker
	machine uuid.UUID
	host    propHost
}

func (c *propPowerCtl) act(a string, fn func() error) error {
	if c.w.bmcDown.Load() {
		return errors.New("prop BMC unreachable")
	}
	c.w.actuating.Add(1)
	defer c.w.actuating.Add(-1)
	// A slow BMC: the call returns, eventually (bmclib retries providers
	// with a 30s timeout each).
	if d := time.Duration(c.w.bmcDelay.Load()); d > 0 && a != "set_boot" {
		time.Sleep(d)
	}
	c.w.powerMu.Lock()
	c.w.powerLog = append(c.w.powerLog, propPowerAction{At: time.Now(), Machine: c.machine, Action: a})
	c.w.powerMu.Unlock()
	return fn()
}

func (c *propPowerCtl) PowerOn(context.Context) error    { return c.act("on", c.host.PowerOn) }
func (c *propPowerCtl) PowerOff(context.Context) error   { return c.act("off", c.host.PowerOff) }
func (c *propPowerCtl) PowerCycle(context.Context) error { return c.act("cycle", c.host.PowerCycle) }
func (c *propPowerCtl) PowerStatus(context.Context) (power.State, error) {
	if c.w.bmcDown.Load() {
		return power.StateUnknown, errors.New("prop BMC unreachable")
	}
	return c.host.PowerState()
}
func (c *propPowerCtl) Transport() string { return "prop" }
func (c *propPowerCtl) SetBootDevice(context.Context, power.BootDevice) error {
	// PXE is first in the persistent boot order on every host under test
	// (as in production), so a one-time override changes nothing.
	return c.act("set_boot", func() error { return nil })
}
func (c *propPowerCtl) ChassisSerial(context.Context) (string, error) { return "", nil }
func (c *propPowerCtl) Close(context.Context) error                   { return nil }

type propWorkerConfig struct {
	Slug         string
	Global       *propGlobal
	Sim          bool
	Network      *Network
	Images       *ImageDir
	DefaultImage string
	// HostFor routes a machine ID to the host its BMC controls.
	HostFor func(uuid.UUID) propHost
	// SyncWait bounds the wait for the first hub sync before services
	// start anyway on the cached snapshot (production: 5s).
	SyncWait     time.Duration
	ExecInterval time.Duration
}

type propWorker struct {
	t       *testing.T
	cfg     propWorkerConfig
	token   string
	id      string
	localDB *worker.LocalDB
	logRing *logring.Ring
	rec     *propRecorder
	log     *slog.Logger
	logBuf  *propLogBuf

	lifeMu sync.Mutex
	up     bool
	inc    int
	cancel context.CancelFunc
	wg     sync.WaitGroup
	tftp   *SiteWorker

	cache   atomic.Pointer[worker.Cache]
	wsConn  atomic.Pointer[worker.WSConn]
	handler atomic.Pointer[http.Handler]

	bmcDown  atomic.Bool
	bmcDelay atomic.Int64 // nanoseconds every power actuation takes
	// actuating counts BMC calls in flight: the executor is busy, not idle.
	actuating atomic.Int32
	powerMu   sync.Mutex
	powerLog  []propPowerAction
	// incStart[i] is when incarnation i+1 of the worker process started.
	incStart []time.Time

	hungMu sync.Mutex
	hung   []string
}

// propLogBuf keeps the worker's log lines for the failure report.
type propLogBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *propLogBuf) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	b.mu.Lock()
	b.lines = append(b.lines, s)
	if len(b.lines) > 4000 {
		b.lines = b.lines[len(b.lines)-3000:]
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *propLogBuf) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.lines) {
		n = len(b.lines)
	}
	return append([]string(nil), b.lines[len(b.lines)-n:]...)
}

func newPropWorker(t *testing.T, cfg propWorkerConfig) *propWorker {
	t.Helper()
	if cfg.SyncWait == 0 {
		cfg.SyncWait = 5 * time.Second
	}
	if cfg.ExecInterval == 0 {
		cfg.ExecInterval = 500 * time.Millisecond
	}
	w := &propWorker{t: t, cfg: cfg, id: uuid.NewString(), rec: newPropRecorder()}
	w.logBuf = &propLogBuf{}
	w.log = slog.New(slog.NewTextHandler(w.logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w.token = cfg.Global.auth.RegisterWorker(cfg.Slug, w.id, "worker."+cfg.Slug+".test")
	db, err := worker.OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("prop: open LocalDB: %v", err)
	}
	db.SetLogger(w.log)
	w.localDB = db
	w.logRing = logring.New(16<<20, time.Hour)
	t.Cleanup(w.Kill)
	return w
}

// Alive reports whether the worker process is serving. Lock-free: hosts
// poll it while a restart is in flight.
func (w *propWorker) Alive() bool { return !w.rec.dead.Load() && w.handler.Load() != nil }

func (w *propWorker) Connected() bool {
	c := w.wsConn.Load()
	return c != nil && c.Connected()
}

// OutboxDepth is the number of observations and op updates the worker has
// recorded and global has not acknowledged yet.
func (w *propWorker) OutboxDepth() int {
	c := w.cache.Load()
	if c == nil {
		return 0
	}
	return c.OutboxDepth()
}

func (w *propWorker) powerFactory(_ context.Context, m *model.MachineSnapshot, _ model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
	if pduRT != nil || amtRT != nil {
		return nil, errors.New("prop factory: only the BMC transport is modeled")
	}
	h := w.cfg.HostFor(m.ID)
	if h == nil {
		return nil, fmt.Errorf("prop factory: no host for machine %s", m.ID)
	}
	return &propPowerCtl{w: w, machine: m.ID, host: h}, nil
}

// IncarnationStart is when the given worker incarnation (1-based) started.
func (w *propWorker) IncarnationStart(inc int) time.Time {
	w.powerMu.Lock()
	defer w.powerMu.Unlock()
	if inc < 1 || inc > len(w.incStart) {
		return time.Time{}
	}
	return w.incStart[inc-1]
}

func (w *propWorker) PowerLog() []propPowerAction {
	w.powerMu.Lock()
	defer w.powerMu.Unlock()
	return append([]propPowerAction(nil), w.powerLog...)
}

// onACK is cmd/slagworksd/site.go's dhcpCfg.OnACK.
func (w *propWorker) onACK(mac string, isIPXE bool) {
	// The ACK is a moment a history can aim at: the instant the host's
	// PXE traffic turns into a phase update on the worker.
	ev := propHTTPEvent{At: time.Now(), Inc: w.inc, Kind: "dhcp", MAC: strings.ToLower(mac), Phase: "dhcp_ack", Connected: w.Connected(), Status: 200}
	if isIPXE {
		ev.Phase = "ipxe_dhcp"
	}
	w.rec.fire(true, &ev)
	cache := w.cache.Load()
	if cache == nil {
		return
	}
	m := cache.MachineByMAC(mac)
	if m == nil {
		return
	}
	op := cache.ActiveBootingOpForMachine(m.ID)
	if op == nil {
		return
	}
	phase := "dhcp_ack"
	if isIPXE {
		phase = "ipxe_dhcp"
	}
	cache.UpdateOpPhase(op.ID.String(), phase)
}

// simDHCP answers the simulated host's DHCP exchange: a lease (and the
// OnACK hook) only when the worker is up and the MAC is a known machine.
func (w *propWorker) simDHCP(mac string, isIPXE bool) bool {
	if !w.Alive() {
		return false
	}
	cache := w.cache.Load()
	if cache == nil || cache.MachineByMAC(mac) == nil {
		return false
	}
	w.onACK(mac, isIPXE)
	return true
}

// Start brings up a fresh worker process image on the existing LocalDB.
func (w *propWorker) Start() {
	w.lifeMu.Lock()
	defer w.lifeMu.Unlock()
	if w.up {
		return
	}
	w.inc++
	w.powerMu.Lock()
	w.incStart = append(w.incStart, time.Now())
	w.powerMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	cache := worker.NewCache(w.localDB)
	if !w.cfg.Sim {
		cache.ImagesDir = w.cfg.Images.Path
	}
	cache.SetLogSink(w.logRing)
	wsConn := worker.NewWSConn(worker.WSConnConfig{
		GlobalURL: w.cfg.Global.URL,
		Token:     w.token,
		WorkerID:  w.id,
		FQDN:      "worker." + w.cfg.Slug + ".test",
		Version:   testVersion(),
	}, cache, w.localDB, w.logRing)
	wsConn.SetLogger(w.log)
	wsConn.SetPowerControllerFactory(w.powerFactory)
	w.cache.Store(cache)
	w.wsConn.Store(wsConn)

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		_ = wsConn.Run(ctx)
	}()

	// Production waits a bounded time for the first sync, then serves from
	// the LocalDB-rehydrated snapshot (runSite). The very first start has
	// nothing cached, so it must sync.
	syncCtx, syncCancel := context.WithTimeout(ctx, w.cfg.SyncWait)
	err := wsConn.WaitForSync(syncCtx)
	syncCancel()
	if err != nil && cache.GetDHCPConfig().Subnet == "" {
		syncCtx, syncCancel = context.WithTimeout(ctx, 60*time.Second)
		err = wsConn.WaitForSync(syncCtx)
		syncCancel()
		if err != nil {
			cancel()
			w.t.Errorf("prop: worker never synced and has no cached snapshot: %v", err)
			return
		}
	}

	executor := worker.NewExecutor(worker.ExecutorConfig{
		Cache:    cache,
		Factory:  w.powerFactory,
		Claimer:  wsConn,
		WorkerID: w.id,
		Interval: w.cfg.ExecInterval,
	})
	executor.SetLogger(w.log)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		_ = executor.Run(ctx)
	}()

	hs := httpboot.NewServer("", httpboot.ServerConfig{
		ImagesDir:    w.imagesPath(),
		DefaultImage: w.defaultImage(),
		HTTPPort:     8080,
		DeployMode:   true,
		Provider:     cache,
		LogSink:      w.logRing,
		AgentBinPath: func() string { return filepath.Join(w.imagesPath(), "slagworks-agent") },
	})
	hs.SetLogger(w.log)
	wrapped := w.rec.wrap(hs.Build(), w.inc, w.Connected, func(mac, machine string) string {
		var m *model.MachineSnapshot
		if mac != "" {
			m = cache.MachineByMAC(mac)
		} else if id, err := uuid.Parse(machine); err == nil {
			m = cache.MachineByID(id)
		}
		if m == nil {
			return "worker: unknown machine"
		}
		d := "worker: state=" + string(m.State)
		if op := cache.ActiveBootingOpForMachine(m.ID); op != nil {
			d += fmt.Sprintf(" op=%s %s/%s phase=%q v%d", op.ID.String()[:8], op.Type, op.Status, op.Phase, op.Version)
		} else {
			d += " op=none"
		}
		return d
	})
	w.handler.Store(&wrapped)

	if !w.cfg.Sim {
		w.startNetServices(ctx, cache, wrapped)
	}
	w.rec.dead.Store(false)
	w.up = true
}

func (w *propWorker) imagesPath() string {
	if w.cfg.Images == nil {
		return ""
	}
	return w.cfg.Images.Path
}

func (w *propWorker) defaultImage() string {
	if w.cfg.DefaultImage != "" {
		return w.cfg.DefaultImage
	}
	return "noble-pxe"
}

// startNetServices binds DHCP, TFTP and HTTP inside the test netns.
func (w *propWorker) startNetServices(ctx context.Context, cache *worker.Cache, handler http.Handler) {
	nw := w.cfg.Network
	snap := cache.GetDHCPConfig()
	_, subnet, err := net.ParseCIDR(snap.Subnet)
	if err != nil {
		w.t.Errorf("prop: snapshot subnet %q: %v", snap.Subnet, err)
		return
	}
	dcfg := dhcp.Config{
		ServerIP:      nw.GatewayIP,
		LeaseDuration: 5 * time.Minute,
		HTTPBootPort:  8080,
		LeaseStore:    w.localDB.NewLeaseStore(),
		KnownMACs:     func(mac string) bool { return cache.MachineByMAC(mac) != nil },
		ReserveIP:     cache.ReservedIPForMAC,
		MachineIDForMAC: func(mac string) string {
			if m := cache.MachineByMAC(mac); m != nil {
				return m.ID.String()
			}
			return ""
		},
		BootEligible: func(mac string) bool { return !cache.IsOOBManagedNonMachine(mac) },
		OnACK:        w.onACK,
	}
	dcfg.Subnet = *subnet
	dcfg.RangeStart = net.ParseIP(snap.EphemeralPoolStart)
	dcfg.RangeEnd = net.ParseIP(snap.EphemeralPoolEnd)
	dcfg.Gateway = net.ParseIP(snap.Gateway)
	dcfg.OnLeaseEvent = worker.NewDHCPLeaseForwarder(cache, dcfg.LeaseDuration).OnLeaseEvent
	dh := dhcp.NewHandler(dcfg)
	dh.SetLogger(w.log)
	ds := dhcp.NewServer(nw.Bridge, dh)
	ds.SetConnHook(func() (net.PacketConn, error) {
		conn, err := NSListenPacket(nw.NSName, "udp4", ":67")
		if err != nil {
			return nil, err
		}
		if udpConn, ok := conn.(*net.UDPConn); ok {
			rawConn, rerr := udpConn.SyscallConn()
			if rerr != nil {
				_ = conn.Close()
				return nil, rerr
			}
			var sockErr error
			if cerr := rawConn.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}); cerr != nil {
				_ = conn.Close()
				return nil, cerr
			}
			if sockErr != nil {
				_ = conn.Close()
				return nil, sockErr
			}
		}
		return conn, nil
	})
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if err := ds.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			w.t.Errorf("prop: DHCP server terminated: %v", err)
		}
	}()

	w.tftp = &SiteWorker{Images: w.cfg.Images, Network: nw, t: w.t}
	w.tftp.startTFTPSubprocess(w.log)

	addr := fmt.Sprintf("%s:8080", nw.Gateway)
	var ln net.Listener
	for i := 0; i < 50; i++ {
		ln, err = NSListen(nw.NSName, "tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		w.t.Errorf("prop: listen %s: %v", addr, err)
		return
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	w.wg.Add(2)
	go func() {
		defer w.wg.Done()
		_ = srv.Serve(ln)
	}()
	go func() {
		defer w.wg.Done()
		<-ctx.Done()
		// A killed process closes its sockets; it does not drain them.
		_ = srv.Close()
	}()
}

// Kill drops the worker process: every goroutine stops, in-memory state is
// gone, LocalDB stays on disk. A shutdown that does not finish is reported
// (deadlock).
func (w *propWorker) Kill() {
	w.lifeMu.Lock()
	defer w.lifeMu.Unlock()
	if !w.up {
		return
	}
	w.rec.dead.Store(true)
	w.cancel()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		w.hungMu.Lock()
		w.hung = append(w.hung, fmt.Sprintf("worker incarnation %d did not shut down within 90s", w.inc))
		w.hungMu.Unlock()
	}
	if w.tftp != nil {
		w.tftp.stopTFTPSubprocess()
		w.tftp = nil
	}
	w.handler.Store(nil)
	w.up = false
}

func (w *propWorker) Hung() []string {
	w.hungMu.Lock()
	defer w.hungMu.Unlock()
	return append([]string(nil), w.hung...)
}

// serveDirect runs one request through the recorded handler in-process.
// A killed worker (or one killed mid-request) reads as a refused or reset
// connection.
func (w *propWorker) serveDirect(req *http.Request) (status int, body string, err error) {
	h := w.handler.Load()
	if h == nil || w.rec.dead.Load() {
		return 0, "", errors.New("connection refused")
	}
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				status, body, err = 0, "", errors.New("connection reset")
				return
			}
			panic(p)
		}
	}()
	rr := httptest.NewRecorder()
	(*h).ServeHTTP(rr, req)
	return rr.Code, rr.Body.String(), nil
}

func (w *propWorker) postReport(rep model.ProgressReport, synthetic bool) (int, error) {
	body, _ := json.Marshal(rep)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if synthetic {
		req.Header.Set("X-Prop-Synthetic", "1")
	}
	st, _, err := w.serveDirect(req)
	return st, err
}

func (w *propWorker) postMACPhoneHome(mac string) (int, error) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/phone-home?mac="+mac, nil)
	req.Header.Set("X-Prop-Synthetic", "1")
	st, _, err := w.serveDirect(req)
	return st, err
}

// --- simulated host ---

// propSimHost models a machine's firmware, the deploy/commission agent and the
// installed OS's phone-home unit closely enough to drive every request the
// real ones send, in the same order, with the same retry rules
// (cmd/slagworks-agent, internal/agent/target.GeneratePhoneHomeUnit),
// including the agent halting on the 410 that says its operation is over.
type propSimHost struct {
	name    string
	mac     string
	smbios  string
	machine uuid.UUID
	w       *propWorker
	step    time.Duration

	mu      sync.Mutex
	powered bool
	gen     int
	// Disk contents.
	os         string // op ID of the bootable install, "" if none
	phonedHome bool   // /var/lib/slagworks/.phoned-home exists
	dirty      bool   // disk carries partitions/signatures
	writes     int64
	stage      string // what the host is doing, for failure reports
}

func newPropSimHost(name, mac string, machine uuid.UUID, w *propWorker, step time.Duration) *propSimHost {
	return &propSimHost{name: name, mac: mac, smbios: uuid.NewString(), machine: machine, w: w, step: step}
}

func (h *propSimHost) Name() string { return h.name }
func (h *propSimHost) MAC() string  { return h.mac }

func (h *propSimHost) DiskWrites() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writes
}

func (h *propSimHost) Stage() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stage
}

func (h *propSimHost) PowerState() (power.State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.powered {
		return power.StateOn, nil
	}
	return power.StateOff, nil
}

func (h *propSimHost) PowerCycle() error {
	h.mu.Lock()
	h.gen++
	h.powered = true
	gen := h.gen
	h.mu.Unlock()
	go h.boot(gen)
	return nil
}

func (h *propSimHost) PowerOff() error {
	h.mu.Lock()
	h.gen++
	h.powered = false
	h.stage = "off"
	h.mu.Unlock()
	return nil
}

func (h *propSimHost) PowerOn() error {
	h.mu.Lock()
	if h.powered {
		h.mu.Unlock()
		return nil
	}
	h.gen++
	h.powered = true
	gen := h.gen
	h.mu.Unlock()
	go h.boot(gen)
	return nil
}

func (h *propSimHost) live(gen int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.powered && h.gen == gen
}

func (h *propSimHost) pause(gen int, d time.Duration) bool {
	time.Sleep(d)
	return h.live(gen)
}

func (h *propSimHost) setStage(gen int, s string) {
	h.mu.Lock()
	if h.gen == gen {
		h.stage = s
	}
	h.mu.Unlock()
}

func (h *propSimHost) report(rep model.ProgressReport) (int, error) {
	return h.w.postReport(rep, false)
}

func (h *propSimHost) boot(gen int) {
	h.setStage(gen, "firmware")
	if !h.pause(gen, h.step) {
		return
	}
	script := ""
	if h.w.simDHCP(h.mac, false) {
		if !h.pause(gen, h.step) {
			return
		}
		h.w.simDHCP(h.mac, true)
		req := httptest.NewRequest(http.MethodGet, "/boot/ipxe?mac="+h.mac, nil)
		if st, body, err := h.w.serveDirect(req); err == nil && st == http.StatusOK {
			script = body
		}
	}
	if !h.live(gen) {
		return
	}
	if script == "" {
		// No lease or no boot script: firmware falls through to disk.
		h.bootDisk(gen)
		return
	}
	kind, machine, op := propClassifyBootScript(script)
	switch kind {
	case "deploy":
		h.deployAgent(gen, script, machine, op)
	case "commission":
		h.commissionAgent(gen, machine, op)
	case "local":
		h.bootDisk(gen)
	default:
		h.setStage(gen, "ipxe shell (boot refused)")
	}
}

func (h *propSimHost) bootDisk(gen int) {
	h.mu.Lock()
	osOp, phoned := h.os, h.phonedHome
	h.mu.Unlock()
	if osOp == "" {
		h.setStage(gen, "no bootable device")
		return
	}
	h.setStage(gen, "os booting")
	if !h.pause(gen, 2*h.step) {
		return
	}
	if phoned {
		h.setStage(gen, "os up")
		return
	}
	for {
		h.setStage(gen, "os up, phoning home")
		st, err := h.report(model.ProgressReport{MachineID: h.machine.String(), OperationID: osOp, Event: "phone_home"})
		if !h.live(gen) {
			return
		}
		if err == nil && st >= 200 && st < 300 {
			h.mu.Lock()
			if h.gen == gen {
				h.phonedHome = true
				h.stage = "os up"
			}
			h.mu.Unlock()
			return
		}
		if !h.pause(gen, 12*h.step) {
			return
		}
	}
}

// agentReport is the agent's agentReporter.send: a 410 before the run's
// final report was accepted means the operation is over, and the agent
// halts there (no further reports, no reboot; the host waits in recovery).
// status is 0 when the request got no answer; ok is false once the agent
// has halted.
func (h *propSimHost) agentReport(gen int, rep model.ProgressReport) (status int, ok bool) {
	status, err := h.report(rep)
	if err != nil {
		return 0, true
	}
	if status == http.StatusGone {
		h.setStage(gen, "agent halted: operation over")
		return status, false
	}
	return status, true
}

func (h *propSimHost) fatal(gen int, machine, op, stage string) {
	_, _ = h.report(model.ProgressReport{MachineID: machine, OperationID: op, Phase: stage, Failed: true, Error: stage + ": simulated"})
	h.setStage(gen, "agent FATAL during "+stage)
}

func (h *propSimHost) deployAgent(gen int, script, machine, op string) {
	destructive := strings.Contains(script, " slagworks.destructive")
	fatalAt := ""
	if m := regexp.MustCompile(`slagworks\.fatal_at=(\S+)`).FindStringSubmatch(script); m != nil {
		fatalAt = m[1]
	}
	// Kernel, squashfs download, agent start: the long quiet stretch
	// between the boot script and the agent's first report.
	h.setStage(gen, "initrd")
	if !h.pause(gen, 12*h.step) {
		return
	}
	if m := regexp.MustCompile(`slagworks\.expect-uuid=(\S+)`).FindStringSubmatch(script); m == nil || m[1] != h.smbios {
		h.fatal(gen, machine, op, "identity check")
		return
	}
	phase := func(p string) bool {
		h.setStage(gen, "agent "+p)
		if _, ok := h.agentReport(gen, model.ProgressReport{MachineID: machine, OperationID: op, Phase: p}); !ok {
			return false
		}
		if fatalAt == p {
			h.fatal(gen, machine, op, p)
			return false
		}
		return h.pause(gen, h.step)
	}
	if !phase("agent_starting") {
		return
	}
	h.mu.Lock()
	unclean := h.dirty
	h.mu.Unlock()
	if !destructive && unclean {
		h.fatal(gen, machine, op, "safety check")
		return
	}
	if !phase("partitioning") {
		return
	}
	h.mu.Lock()
	if h.gen == gen {
		h.os, h.phonedHome, h.dirty = "", false, true
		h.writes++
	}
	h.mu.Unlock()
	for _, p := range []string{"rsyncing_os", "installing_bootloader", "finalizing"} {
		if !phase(p) {
			return
		}
		// Extracting the root filesystem is most of an install.
		if p == "rsyncing_os" && !h.pause(gen, 25*h.step) {
			return
		}
	}
	// The root filesystem, bootloader and phone-home unit are on disk.
	h.mu.Lock()
	if h.gen == gen {
		h.os, h.phonedHome = op, false
		h.writes++
	}
	h.mu.Unlock()
	if !phase("bmc_reconcile") {
		return
	}
	// deploy_complete: four attempts, 2xx only (agentReporter.ReportDeployComplete).
	ok := false
	backoff := h.step
	for attempt := 0; attempt < 4; attempt++ {
		h.setStage(gen, "agent deploy_complete")
		st, alive := h.agentReport(gen, model.ProgressReport{MachineID: machine, OperationID: op, Event: "deploy_complete"})
		if !alive || !h.live(gen) {
			return
		}
		if st >= 200 && st < 300 {
			ok = true
			break
		}
		if !h.pause(gen, backoff) {
			return
		}
		backoff *= 5
	}
	if !ok {
		h.fatal(gen, machine, op, "reporting deploy_complete")
		return
	}
	if !h.pause(gen, h.step) {
		return
	}
	h.rebootSelf(gen)
}

func (h *propSimHost) commissionAgent(gen int, machine, op string) {
	h.setStage(gen, "initrd")
	if !h.pause(gen, 2*h.step) {
		return
	}
	for _, p := range []string{"agent_starting", "collecting_inventory", "reporting"} {
		h.setStage(gen, "agent "+p)
		if _, ok := h.agentReport(gen, model.ProgressReport{MachineID: machine, OperationID: op, Phase: p}); !ok {
			return
		}
		if !h.pause(gen, h.step) {
			return
		}
	}
	inv, _ := json.Marshal(map[string]any{
		"system": map[string]any{"uuid": h.smbios},
		"nics":   []map[string]any{{"mac": h.mac, "driver": "virtio_net"}},
		"disks":  []map[string]any{},
	})
	ok := false
	for attempt := 0; attempt < 3; attempt++ {
		st, alive := h.agentReport(gen, model.ProgressReport{MachineID: machine, OperationID: op, Event: "commission_report", CommissionData: inv})
		if !alive || !h.live(gen) {
			return
		}
		if st == http.StatusOK {
			ok = true
			break
		}
		if !h.pause(gen, 2*h.step) {
			return
		}
	}
	if !ok {
		h.fatal(gen, machine, op, "reporting commission")
		return
	}
	if !h.pause(gen, h.step) {
		return
	}
	h.rebootSelf(gen)
}

// rebootSelf is the agent's `reboot -f`.
func (h *propSimHost) rebootSelf(gen int) {
	h.mu.Lock()
	if h.gen != gen || !h.powered {
		h.mu.Unlock()
		return
	}
	h.gen++
	next := h.gen
	h.mu.Unlock()
	go h.boot(next)
}
