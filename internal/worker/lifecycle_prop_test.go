package worker

// Virtual-time state-machine property test for the site worker's link to
// global: the real WSConn.Run (with its reconnect backoff), Cache and LocalDB
// against the real ws.Hub, wired through in-memory pipes inside a
// testing/synctest bubble so a 30-second backoff step or a 150-second image
// download costs nothing to wait out.
//
// The worker "process" runs under a model of its systemd unit's restart
// policy (deploy/systemd/slagworksd-site.service: Restart=always,
// RestartSec=5): it is restarted after a crash, and after a start that
// fails. Readiness (READY=1) is reported by the site command, not by the
// link, so the start timeout is not modeled here; the process-level harness
// in cmd/slagworksd checks it. A generated history changes what global does
// (unreachable, 401, snapshot build failing or hanging, the op resync
// failing, the hello never answered), publishes new snapshot generations,
// resets the site's sequence counter, makes image downloads slow, stuck or
// failing, crashes the worker, damages rows of its LocalDB, and probes the
// worker through the hub.
//
// Checked after every step, with every goroutine parked:
//
//   - the cache view is one whole snapshot generation (machines and meta
//     from the same generation), and one global actually published;
//   - within a process lifetime the served generation never goes back, and a
//     restarted process serves exactly what the previous one served: a
//     snapshot is persisted before anything slow (an image download) starts;
//   - the worker never reports itself connected without a hub connection,
//     and a hub that routes an RPC to the worker gets an answer, image
//     download in flight or not;
//   - the op set a restarted worker rehydrated is not handed to the executor
//     before global has resynced it.
//
// Checked at settle points (a quiet stretch longer than every timer):
//
//   - with global healthy the worker is connected, holds global's current
//     generation and the site's pending op, whatever its image download is
//     doing;
//   - reconnect attempts stay on the backoff ladder, whatever global does.
//
// Three behaviours of the link that the properties above would otherwise
// report are tolerated, each where it is detected (see vtTol*): they are
// counted in the coverage line and the rest of the history is still checked.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

const (
	vtSlug     = "site-a"
	vtWorkerID = "worker-a"
	vtToken    = "vt-token"

	vtRestartDelay = 5 * time.Second // RestartSec= in the packaged unit
	// vtSettle outlasts the restart delay, the hub's RebuildTimeout and
	// several backoff steps at the cap (30s plus up to 20% jitter), so
	// whatever the worker was doing it has had the chance to reconnect to a
	// healthy global.
	vtSettle = 140 * time.Second

	// vtFirstSeq is the sequence number of generation 1: high enough that a
	// restore has somewhere to go back to.
	vtFirstSeq = 6
)

// --- global ----------------------------------------------------------------

type vtMode int

const (
	vtOK          vtMode = iota
	vtRefuse             // dial fails
	vtAuth401            // token not recognized
	vtRebuildFail        // the site's snapshot cannot be built (matters while the hub is cold)
	vtRebuildHang        // the build never returns; the hub gives up after RebuildTimeout
	vtResyncFail         // hello answered, then the op resync fails and the hub drops the worker
	vtTouchFail          // recording the hello in site_workers fails
	vtHelloHang          // upgrade accepted, hello never answered
	vtBlackhole          // dial succeeds, nothing ever answers
	vtNumModes
)

func (m vtMode) String() string {
	return [...]string{"ok", "refuse", "auth-401", "rebuild-fail", "rebuild-hang", "resync-fail", "touch-fail", "hello-hang", "blackhole"}[m]
}

type vtImage int

const (
	vtImageInstant vtImage = iota
	vtImageSlow            // a download that takes longer than a settle
	vtImageStuck           // a download that never finishes
	vtImageError
	vtNumImage
)

func (m vtImage) String() string { return [...]string{"instant", "slow", "stuck", "error"}[m] }

const vtSlowImage = 150 * time.Second

func vtMachineID(gen, i int) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "vt-machine-%d-%d", gen, i))
}

func vtMarker(gen int) string { return "ssh-ed25519 AAAA gen-" + strconv.Itoa(gen) }

// vtSnapshot is generation gen's snapshot. Every generation has its own
// machines, marker and image, so a view mixing two generations is visible.
func vtSnapshot(gen int, seq int64) model.SiteSnapshot {
	s := model.SiteSnapshot{
		SeqNum:  seq,
		Site:    model.SiteInfo{Name: vtSlug, Slug: vtSlug},
		DHCP:    model.DHCPConfig{Subnet: "10.9.0.0/24", Gateway: "10.9.0.1", EphemeralPoolStart: "10.9.0.100", EphemeralPoolEnd: "10.9.0.200", LeaseSeconds: 300 + gen},
		SSHKeys: []string{vtMarker(gen)},
		Images:  []model.Image{{Name: "img-" + strconv.Itoa(gen), URL: "http://images.invalid/img.tgz", SHA256: strconv.Itoa(gen)}},
	}
	for i := range 2 + gen%2 {
		s.Machines = append(s.Machines, model.MachineSnapshot{
			ID: vtMachineID(gen, i), Hostname: fmt.Sprintf("h%d-%d", gen, i), State: model.MachineStateRunning,
			Interfaces: []model.SnapshotInterface{{Name: "eth0", Type: "physical", MAC: fmt.Sprintf("02:00:00:%02x:%02x:%02x", gen>>8&0xff, gen&0xff, i)}},
		})
	}
	return s
}

func vtOpID(gen int) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "vt-op-%d", gen))
}

// vtPipe returns the two ends of an in-memory connection whose writes never
// block, like a TCP connection with room in its buffers. net.Pipe is
// unbuffered: a hub write to a worker that is not reading would park while
// holding the connection's write mutex, and a second writer waiting on that
// mutex is not "durably blocked" to synctest, so the bubble's clock, and with
// it the write's own timeout, would stop.
func vtPipe() (net.Conn, net.Conn) {
	a2b, b2a := &vtBuf{wake: make(chan struct{}, 1)}, &vtBuf{wake: make(chan struct{}, 1)}
	return &vtConn{rd: b2a, wr: a2b, done: make(chan struct{})}, &vtConn{rd: a2b, wr: b2a, done: make(chan struct{})}
}

type vtBuf struct {
	mu      sync.Mutex
	data    []byte
	wclosed bool // the writing end closed: EOF once drained
	rclosed bool // the reading end closed: writes fail
	wake    chan struct{}
}

func (b *vtBuf) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

type vtConn struct {
	rd, wr *vtBuf
	once   sync.Once
	done   chan struct{}

	dmu      sync.Mutex
	deadline time.Time
	dchange  chan struct{} // closed when the read deadline changes
}

type vtTimeout struct{}

func (vtTimeout) Error() string   { return "i/o timeout" }
func (vtTimeout) Timeout() bool   { return true }
func (vtTimeout) Temporary() bool { return true }

func (c *vtConn) Read(p []byte) (int, error) {
	for {
		c.rd.mu.Lock()
		if len(c.rd.data) > 0 {
			n := copy(p, c.rd.data)
			c.rd.data = c.rd.data[n:]
			c.rd.mu.Unlock()
			return n, nil
		}
		eof := c.rd.wclosed
		c.rd.mu.Unlock()
		if eof {
			return 0, errors.New("EOF: peer closed the connection")
		}
		c.dmu.Lock()
		dl := c.deadline
		if c.dchange == nil {
			c.dchange = make(chan struct{})
		}
		changed := c.dchange
		c.dmu.Unlock()
		var expire <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, vtTimeout{}
			}
			t := time.NewTimer(d)
			defer t.Stop()
			expire = t.C
		}
		select {
		case <-c.rd.wake:
		case <-changed:
		case <-expire:
			return 0, vtTimeout{}
		case <-c.done:
			return 0, net.ErrClosed
		}
	}
}

func (c *vtConn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	c.wr.mu.Lock()
	if c.wr.rclosed {
		c.wr.mu.Unlock()
		return 0, errors.New("write: broken pipe")
	}
	c.wr.data = append(c.wr.data, p...)
	c.wr.mu.Unlock()
	c.wr.signal()
	return len(p), nil
}

func (c *vtConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.wr.mu.Lock()
		c.wr.wclosed = true
		c.wr.mu.Unlock()
		c.wr.signal()
		c.rd.mu.Lock()
		c.rd.rclosed = true
		c.rd.mu.Unlock()
	})
	return nil
}

func (c *vtConn) SetReadDeadline(t time.Time) error {
	c.dmu.Lock()
	c.deadline = t
	if c.dchange != nil {
		close(c.dchange)
		c.dchange = nil
	}
	c.dmu.Unlock()
	return nil
}

func (c *vtConn) SetDeadline(t time.Time) error  { return c.SetReadDeadline(t) }
func (*vtConn) SetWriteDeadline(time.Time) error { return nil }
func (*vtConn) LocalAddr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }
func (*vtConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3} }

type vtListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *vtListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, errors.New("listener closed")
	}
}
func (l *vtListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *vtListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

type vtGlobal struct {
	start time.Time

	mu      sync.Mutex
	mode    vtMode
	hub     *ws.Hub
	hangCh  chan struct{} // closed when the current global instance goes away
	black   []net.Conn
	gen     int
	seq     int64
	pushed  int             // generation the dispatcher last pushed (or a restart made moot)
	dials   []time.Duration // every worker connection attempt, as an offset from start
	hellos  int             // hellos the hub recorded (TouchSiteWorker with a serial)
	ln      *vtListener
	srv     *http.Server
	tr      *http.Transport
	prevTr  http.RoundTripper
	stopped bool
}

func (g *vtGlobal) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sum := sha256.Sum256([]byte(vtToken))
	if g.mode == vtAuth401 || h != hex.EncodeToString(sum[:]) {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: vtSlug, WorkerID: vtWorkerID}, nil
}

func (g *vtGlobal) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if serial != nil {
		if g.mode == vtTouchFail {
			return nil, errors.New("pq: connection refused")
		}
		g.hellos++
	}
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (g *vtGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g *vtGlobal) RebuildAndWait(ctx context.Context, _ string) (model.SiteSnapshot, error) {
	g.mu.Lock()
	mode, gen, seq := g.mode, g.gen, g.seq
	g.mu.Unlock()
	switch mode {
	case vtRebuildFail:
		return model.SiteSnapshot{}, errors.New("site site-a: no slagworks-production prefix")
	case vtRebuildHang:
		<-ctx.Done()
		return model.SiteSnapshot{}, ctx.Err()
	default:
		return vtSnapshot(gen, seq), nil
	}
}

// ListActiveOperations is the fleet's active set: one pending power-on for
// the first machine of the current generation.
func (g *vtGlobal) ListActiveOperations() ([]model.Operation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode == vtResyncFail {
		return nil, errors.New("pq: canceling statement due to statement timeout")
	}
	return []model.Operation{{ID: vtOpID(g.gen), MachineID: vtMachineID(g.gen, 0), Type: model.OpPowerOn, Status: model.OpPending}}, nil
}

func (g *vtGlobal) newHub() *ws.Hub {
	return ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, ActiveOps: g, Logger: quietLog})
}

func newVTGlobal() *vtGlobal {
	g := &vtGlobal{start: time.Now(), gen: 1, seq: vtFirstSeq, pushed: 1, hangCh: make(chan struct{})}
	g.hub = g.newHub()
	g.ln = &vtListener{ch: make(chan net.Conn), closed: make(chan struct{})}
	g.srv = &http.Server{Handler: http.HandlerFunc(g.serve)}
	go func() { _ = g.srv.Serve(g.ln) }()
	g.tr = &http.Transport{DisableKeepAlives: true, DialContext: g.dial}
	// WSConn dials through http.DefaultClient and has no seam for another
	// client, so the bubble swaps the default client's transport for the
	// duration of the case. The test is not parallel.
	g.prevTr = http.DefaultClient.Transport
	http.DefaultClient.Transport = g.tr
	return g
}

func (g *vtGlobal) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	g.mu.Lock()
	g.dials = append(g.dials, time.Since(g.start))
	mode := g.mode
	if mode == vtRefuse || g.stopped {
		g.mu.Unlock()
		return nil, errors.New("connect: connection refused")
	}
	c, s := vtPipe()
	if mode == vtBlackhole {
		g.black = append(g.black, s)
		g.mu.Unlock()
		return c, nil
	}
	g.mu.Unlock()
	select {
	case g.ln.ch <- s:
		return c, nil
	case <-g.ln.closed:
		return nil, errors.New("connect: connection refused")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *vtGlobal) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	mode, hub, hang := g.mode, g.hub, g.hangCh
	g.mu.Unlock()
	if mode == vtHelloHang {
		conn, err := ws.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-hang
		_ = conn.CloseNow()
		return
	}
	hub.ServeHTTP(w, r)
}

// setMode changes what global does. With restart the hub is replaced by a
// cold one and every connection of the old instance is dropped; without it
// only new connections see the change.
func (g *vtGlobal) setMode(m vtMode, restart bool) {
	g.mu.Lock()
	g.mode = m
	if !restart {
		g.mu.Unlock()
		// A build that was failing succeeds on the registry's next retry.
		g.pushIfBuildable()
		return
	}
	old, hang, black := g.hub, g.hangCh, g.black
	g.hub, g.hangCh, g.black = g.newHub(), make(chan struct{}), nil
	// A restarted global pushes nothing by itself: hellos rebuild.
	g.pushed = g.gen
	g.mu.Unlock()
	old.Close()
	close(hang)
	for _, c := range black {
		_ = c.Close()
	}
}

func vtBuildable(m vtMode) bool { return m != vtRebuildFail && m != vtRebuildHang }

// bump publishes the next generation and returns the sequence number it
// got. back > 0 first moves the sequence that far back, which is what
// restoring global's database from a backup does to it (the Postgres
// sequence returns to its value at backup time).
func (g *vtGlobal) bump(back int64) int64 {
	g.mu.Lock()
	g.gen++
	g.seq = max(0, g.seq-back) + 1
	seq := g.seq
	g.mu.Unlock()
	g.pushIfBuildable()
	return seq
}

// pushIfBuildable is the dispatcher: it pushes the current generation once
// it can build it, and not again.
func (g *vtGlobal) pushIfBuildable() {
	g.mu.Lock()
	if !vtBuildable(g.mode) || g.pushed == g.gen {
		g.mu.Unlock()
		return
	}
	g.pushed = g.gen
	snap, hub := vtSnapshot(g.gen, g.seq), g.hub
	g.mu.Unlock()
	hub.Push(vtSlug, snap)
}

func (g *vtGlobal) cur() (hub *ws.Hub, mode vtMode, gen int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hub, g.mode, g.gen
}

func (g *vtGlobal) dialsSince(t time.Duration) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, d := range g.dials {
		if d >= t {
			n++
		}
	}
	return n
}

func (g *vtGlobal) close() {
	g.mu.Lock()
	g.stopped = true
	hub, hang, black := g.hub, g.hangCh, g.black
	g.black = nil
	g.mu.Unlock()
	hub.Close()
	close(hang)
	for _, c := range black {
		_ = c.Close()
	}
	_ = g.srv.Close()
	g.tr.CloseIdleConnections()
	http.DefaultClient.Transport = g.prevTr
}

// --- worker process under its unit -------------------------------------------

type vtProc struct {
	n       int
	db      *LocalDB
	cache   *Cache
	w       *WSConn
	cancel  context.CancelFunc
	done    chan struct{}
	killed  chan struct{}
	started time.Time
	syncing atomic.Int32 // image syncs in flight
	// synced counts image syncs this process started. An apply hands its
	// image set to the syncer before it swaps the view, and the first one
	// always starts a sync, so 0 means the view is still the one rehydrated
	// from LocalDB.
	synced  atomic.Int32
	applied atomic.Int32 // snapshot applies that ran to completion (persisted)
	// lastApplied is the generation of the last apply that ran to completion.
	lastApplied atomic.Int32
}

type vtUnit struct {
	e      *vtEnv
	dbPath string

	mu     sync.Mutex
	proc   *vtProc // nil while the unit is between restarts
	crash  chan struct{}
	stop   chan struct{}
	exited chan struct{}

	// Counters, written by the unit's goroutine and read by the test's.
	procs         atomic.Int64
	startFailures atomic.Int64 // process could not start (LocalDB would not open)
}

func (u *vtUnit) current() *vtProc {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.proc
}

func (u *vtUnit) startProc() (*vtProc, error) {
	db, err := OpenLocalDB(u.dbPath)
	if err != nil {
		return nil, err
	}
	db.SetLogger(quietLog)
	p := &vtProc{n: int(u.procs.Add(1)), db: db, done: make(chan struct{}), killed: make(chan struct{}), started: time.Now()}
	p.cache = NewCache(db)
	p.cache.ImagesDir = filepath.Join(filepath.Dir(u.dbPath), "images")
	p.cache.imageSyncFn = func(string, map[string]*model.Image) error { return u.e.imageSync(p) }
	_ = p.cache.OnSnapshotApplied(func() {
		p.applied.Add(1)
		p.lastApplied.Store(int32(vtViewGen(p.cache)))
	})
	p.w = NewWSConn(WSConnConfig{GlobalURL: "http://global.invalid", Token: vtToken, WorkerID: vtWorkerID}, p.cache, db, nil)
	p.w.SetLogger(quietLog)
	p.w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo { return []dhcp.LeaseInfo{} })
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		defer close(p.done)
		_ = p.w.Run(ctx)
	}()
	return p, nil
}

func (u *vtUnit) killProc(p *vtProc) {
	close(p.killed)
	p.cancel()
	<-p.done
	// The read loop, the only path that writes LocalDB on its own, has
	// returned; the per-connection goroutines still unwinding touch only
	// in-memory queues, and an image download in flight ends itself (see
	// imageSync).
	closeQuiet(p.db)
}

// run is the unit's lifetime: start the process and restart it after
// RestartSec whenever it dies or fails to start.
func (u *vtUnit) run() {
	defer close(u.exited)
	for {
		p, err := u.startProc()
		if err != nil {
			u.startFailures.Add(1)
		} else {
			u.mu.Lock()
			u.proc = p
			u.mu.Unlock()
			stopped := false
			select {
			case <-u.crash:
			case <-u.stop:
				stopped = true
			}
			u.mu.Lock()
			u.proc = nil
			u.mu.Unlock()
			u.killProc(p)
			if stopped {
				return
			}
		}
		select {
		case <-time.After(vtRestartDelay):
		case <-u.stop:
			return
		}
	}
}

// --- environment -------------------------------------------------------------

type vtEnv struct {
	g *vtGlobal
	u *vtUnit

	imgMu   sync.Mutex
	image   vtImage
	imageCh chan struct{} // closed when the image mode changes
}

func (e *vtEnv) setImage(m vtImage) {
	e.imgMu.Lock()
	e.image = m
	close(e.imageCh)
	e.imageCh = make(chan struct{})
	e.imgMu.Unlock()
}

// imageSync stands in for imagepull.SyncImages, which the cache runs on its
// background syncer goroutine.
func (e *vtEnv) imageSync(p *vtProc) error {
	p.synced.Add(1)
	p.syncing.Add(1)
	defer p.syncing.Add(-1)
	for {
		e.imgMu.Lock()
		mode, changed := e.image, e.imageCh
		e.imgMu.Unlock()
		var timer <-chan time.Time
		switch mode {
		case vtImageInstant:
			return nil
		case vtImageError:
			return errors.New("download returned 503")
		case vtImageSlow:
			timer = time.After(vtSlowImage)
		case vtImageStuck, vtNumImage:
		}
		select {
		case <-timer:
			return nil
		case <-changed: // re-evaluate under the new mode
		case <-p.killed:
			// The process died mid-download. End the syncer goroutine
			// here, so that nothing after the download runs: a killed
			// process does nothing more.
			runtime.Goexit()
		}
	}
}

// vtViewGen reads which generation's marker the cache serves: 0 when it
// holds no snapshot meta.
func vtViewGen(c *Cache) int {
	keys := c.GetSSHKeys()
	if len(keys) == 0 {
		return 0
	}
	_, after, ok := strings.Cut(keys[0], " gen-")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(after)
	if err != nil {
		return -1
	}
	return n
}

// vtMachineGens returns the generations the cache's machines belong to.
func vtMachineGens(c *Cache, maxGen int) map[int]int {
	out := map[int]int{}
	for gen := 1; gen <= maxGen; gen++ {
		for i := range 3 {
			if c.MachineByID(vtMachineID(gen, i)) != nil {
				out[gen]++
			}
		}
	}
	return out
}

// --- history -----------------------------------------------------------------

type vtEventKind int

const (
	vtEvAdvance vtEventKind = iota
	vtEvMode
	vtEvBump
	vtEvReset
	vtEvImage
	vtEvCrash
	vtEvDamage
	vtEvProbe
	vtEvDrop
	vtEvSettle
	vtNumEvents
)

type vtEvent struct {
	Kind    vtEventKind
	D       time.Duration
	Mode    vtMode
	Restart bool
	Image   vtImage
	Damage  int
	Back    int
}

func (e vtEvent) String() string {
	switch e.Kind {
	case vtEvAdvance:
		return "advance(" + e.D.String() + ")"
	case vtEvMode:
		if e.Restart {
			return "restart-global(" + e.Mode.String() + ")"
		}
		return "mode(" + e.Mode.String() + ")"
	case vtEvBump:
		return "bump"
	case vtEvReset:
		return "restore-seq(-" + strconv.Itoa(e.Back) + ")"
	case vtEvImage:
		return "image(" + e.Image.String() + ")"
	case vtEvCrash:
		return "crash"
	case vtEvDamage:
		return "damage(" + strconv.Itoa(e.Damage) + ")"
	case vtEvProbe:
		return "probe"
	case vtEvDrop:
		return "drop-conns"
	default:
		return "settle"
	}
}

var vtAdvances = []time.Duration{100 * time.Millisecond, time.Second, 3 * time.Second, 6 * time.Second, 20 * time.Second, 45 * time.Second, 100 * time.Second}

func vtGenEvents(t *rapid.T) []vtEvent {
	n := rapid.IntRange(3, 16).Draw(t, "nEvents")
	// Swarm: each case leans on a subset of the event kinds.
	weights := make([]int, vtNumEvents)
	for k := range weights {
		weights[k] = rapid.SampledFrom([]int{0, 1, 1, 2, 4}).Draw(t, "w")
	}
	weights[vtEvAdvance] += 2
	weights[vtEvMode] += 2
	var pool []vtEventKind
	for k, w := range weights {
		for range w {
			pool = append(pool, vtEventKind(k))
		}
	}
	faultModes := []vtMode{vtRefuse, vtAuth401, vtRebuildFail, vtRebuildHang, vtResyncFail, vtTouchFail, vtHelloHang, vtBlackhole}
	evs := make([]vtEvent, 0, n+12)
	for range n {
		// Targeted patterns, embedded in whatever random prefix and suffix
		// the rest of the history provides.
		switch rapid.IntRange(0, 11).Draw(t, "pattern") {
		case 0:
			// Warm, then an outage, then a restart into it: the worker must
			// come back serving what it persisted.
			evs = append(evs,
				vtEvent{Kind: vtEvMode, Mode: vtOK, Restart: rapid.Bool().Draw(t, "restart")},
				vtEvent{Kind: vtEvAdvance, D: 45 * time.Second},
				vtEvent{Kind: vtEvBump},
				vtEvent{Kind: vtEvMode, Mode: rapid.SampledFrom(faultModes).Draw(t, "mode"), Restart: rapid.Bool().Draw(t, "restart")},
				vtEvent{Kind: vtEvCrash},
				vtEvent{Kind: vtEvAdvance, D: 6 * time.Second},
				vtEvent{Kind: vtEvProbe})
			continue
		case 1:
			// A push whose image download is slow or stuck, observed while
			// it is in flight.
			evs = append(evs,
				vtEvent{Kind: vtEvImage, Image: rapid.SampledFrom([]vtImage{vtImageSlow, vtImageStuck}).Draw(t, "image")},
				vtEvent{Kind: vtEvBump},
				vtEvent{Kind: vtEvAdvance, D: 3 * time.Second},
				vtEvent{Kind: vtEvProbe})
			if rapid.IntRange(0, 2).Draw(t, "unblock") > 0 {
				// Usually the download then gets through, so the rest of
				// the history is not spent behind it.
				evs = append(evs, vtEvent{Kind: vtEvImage, Image: vtImageInstant})
			}
			continue
		case 2:
			// A fault held long enough for every timer to matter.
			evs = append(evs,
				vtEvent{Kind: vtEvMode, Mode: rapid.SampledFrom(faultModes).Draw(t, "mode"), Restart: rapid.Bool().Draw(t, "restart")},
				vtEvent{Kind: vtEvSettle})
			continue
		default:
		}
		ev := vtEvent{Kind: rapid.SampledFrom(pool).Draw(t, "kind")}
		switch ev.Kind {
		case vtEvAdvance:
			ev.D = rapid.SampledFrom(vtAdvances).Draw(t, "d")
		case vtEvMode:
			ev.Mode = vtMode(rapid.IntRange(0, int(vtNumModes)-1).Draw(t, "mode"))
			ev.Restart = rapid.Bool().Draw(t, "restart")
		case vtEvImage:
			ev.Image = rapid.SampledFrom([]vtImage{vtImageInstant, vtImageInstant, vtImageInstant, vtImageSlow, vtImageStuck, vtImageError}).Draw(t, "image")
		case vtEvDamage:
			// 5 (the file is no longer a database) ends the useful part of
			// a history, so it is the rare one.
			ev.Damage = rapid.SampledFrom([]int{0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5}).Draw(t, "damage")
		case vtEvReset:
			ev.Back = rapid.IntRange(1, 4).Draw(t, "back")
		default:
		}
		evs = append(evs, ev)
	}
	// Every history ends with global healthy and a settle, so recovery from
	// whatever came before is always checked; half of them also clear the
	// image fault first.
	if rapid.Bool().Draw(t, "clearImage") {
		evs = append(evs, vtEvent{Kind: vtEvImage, Image: vtImageInstant})
	}
	return append(evs, vtEvent{Kind: vtEvMode, Mode: vtOK, Restart: rapid.Bool().Draw(t, "finalRestart")}, vtEvent{Kind: vtEvSettle})
}

// Behaviours of the link that are tolerated where a property would report
// them; each is described where it is detected.
const (
	vtTolHelloHang = "hello-reply-has-no-deadline"
	vtTolSeqReuse  = "reissued-seq-hides-content-change"
	vtTolHalfHello = "fan-out-taken-for-hello-reply"
)

type vtStats struct {
	cases, events, procs, startFailures  int
	settles, settlesHealthy, converged   int
	probes, probesRouted, probesAnswered int
	restartsFromDisk, restartsServing    int
	maxGen, resets, damages, crashes     int
	modeSeen                             [vtNumModes]int
	imageSeen                            [vtNumImage]int
	tolerated                            map[string]int
}

type vtRun struct {
	t     *rapid.T
	stats *vtStats
	e     *vtEnv
	trail []string

	// Model. gen/mode mirror what the history did to global; the rest is
	// what the worker has been seen to do.
	mode      vtMode
	image     vtImage
	gen       int
	fileDead  bool          // worker.db was replaced with something that is not a database
	damaged   bool          // rows of worker.db were damaged since the last completed apply
	hadFault  bool          // global was other than healthy at some point since the last settle
	lastProc  int           // process the fields below describe
	served    int           // generation that process was last seen serving
	persisted int           // generation the last completed apply (of any process) left on disk
	seqOf     map[int]int64 // sequence number each generation was published under
	// hungSince is the process that was running when global last accepted
	// connections without answering them (hello-hang, blackhole) and was
	// not restarted since; 0 when there is none. Such a connection is never
	// closed by anyone, so only a worker-side deadline ends the wait.
	hungSince int
}

func (r *vtRun) failf(format string, args ...any) {
	u := r.e.u
	if os.Getenv("SLAGWORKS_PROP_DEBUG") == "1" {
		// Where every goroutine of the bubble is parked at the failure.
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
	}
	r.t.Fatalf("%s\nhistory: %s\nmodel: mode=%v image=%v gen=%d persisted=%d served=%d damaged=%v\nunit: procs=%d start-failures=%d",
		fmt.Sprintf(format, args...), strings.Join(r.trail, " -> "), r.mode, r.image, r.gen, r.persisted, r.served, r.damaged,
		u.procs.Load(), u.startFailures.Load())
}

// tolerate counts one occurrence of a tolerated behaviour (vtTol*).
func (r *vtRun) tolerate(id string) { r.stats.tolerated[id]++ }

// check runs the invariants that must hold whenever every goroutine is
// parked.
func (r *vtRun) check() {
	synctest.Wait()
	p := r.e.u.current()
	if p == nil {
		return
	}
	hub, _, _ := r.e.g.cur()
	if r.mode == vtHelloHang || r.mode == vtBlackhole {
		r.hungSince = p.n
	}
	c := p.cache
	served := vtViewGen(c)
	machines := vtMachineGens(c, r.gen)

	// One whole generation, and one that exists.
	if served < 0 || served > r.gen {
		r.failf("worker serves generation %d, which global never published (current %d)", served, r.gen)
	}
	want := map[int]int{}
	if served > 0 {
		want[served] = len(vtSnapshot(served, 1).Machines)
	}
	if !r.damaged && !vtSameCounts(machines, want) {
		r.failf("torn view: marker says generation %d but the machines present are %v (generation -> count)", served, machines)
	}
	if r.damaged {
		// Damaged rows may be missing; rows of a generation the file never
		// held may not appear.
		for g := range machines {
			if g > r.persisted && g != served {
				r.failf("after LocalDB damage the worker serves machines of generation %d; the file held at most %d", g, r.persisted)
			}
		}
	}

	if p.n != r.lastProc {
		// First look at a new process: before any connection completes it
		// serves what the last completed apply left on disk.
		r.stats.restartsFromDisk++
		if p.synced.Load() == 0 && !r.damaged {
			if served != r.persisted {
				r.failf("restarted worker serves generation %d from LocalDB; the last completed apply was generation %d", served, r.persisted)
			}
			if served > 0 {
				r.stats.restartsServing++
			}
			if served < r.served {
				r.failf("restarted worker serves generation %d; the previous process was serving %d (applied in memory, not persisted when it died)", served, r.served)
			}
			// A rehydrated op set must not reach the executor before a resync.
			if n := len(c.PendingPowerOps()) + len(c.PendingLifecycleOps()); n > 0 && !p.w.Connected() {
				r.failf("restarted worker offers %d pending op(s) to the executor before any op_resync", n)
			}
		}
		r.lastProc, r.served = p.n, served
	}
	if served < r.served {
		r.failf("served generation went back from %d to %d within one process", r.served, served)
	}
	r.served = served
	if p.applied.Load() > 0 {
		r.persisted = int(p.lastApplied.Load())
		if r.persisted == served {
			r.damaged = false
		}
	}

	// The worker never believes in a connection the hub does not have.
	if p.w.Connected() && !hub.IsWorkerConnected(vtSlug, vtWorkerID) {
		r.failf("worker reports connected but the hub has no connection for it (image syncs in flight: %d)", p.syncing.Load())
	}
}

func vtSameCounts(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// probe is an operator's synchronous read through the hub (`slagctl dhcp
// leases`, power status): either the hub says no worker is connected (the
// API's 503) or the worker it picked answers.
func (r *vtRun) probe() {
	hub, _, _ := r.e.g.cur()
	p := r.e.u.current()
	r.stats.probes++
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	res := hub.DHCPLeases(ctx, vtSlug)
	cancel()
	if len(res) == 0 {
		if p != nil && p.w.Connected() {
			synctest.Wait()
			if p.w.Connected() {
				if !hub.IsWorkerConnected(vtSlug, vtWorkerID) {
					r.failf("worker reports connected but the hub has no worker to route an RPC to")
				}
				// The connection exists but the hub has not answered its
				// hello: it is still waiting on the site's rebuild. The
				// connection registers for fan-out before its hello is
				// answered, a pushed snapshot reached the worker first, and
				// the worker takes the first snapshot on a connection for
				// the hello reply, so it reports connected while the hub
				// routes it nothing yet.
				r.tolerate(vtTolHalfHello)
			}
		}
		return
	}
	r.stats.probesRouted++
	after := r.e.u.current()
	for _, w := range res {
		switch {
		case w.Err == "":
			r.stats.probesAnswered++
		case p == nil || after != p:
			// The process died under the RPC.
		default:
			r.failf("hub routed an RPC to the worker and got no answer (image syncs in flight: %d): %s", p.syncing.Load(), w.Err)
		}
	}
}

func (r *vtRun) settle() {
	u := r.e.u
	since := time.Since(r.e.g.start)
	failsBefore := u.startFailures.Load()
	time.Sleep(vtSettle)
	r.check()
	r.stats.settles++

	// A worker.db that SQLite cannot open is fatal at start by design: the
	// unit restart-loops until an operator looks at the file. Nothing else
	// may keep the process from starting.
	if d := u.startFailures.Load() - failsBefore; d > 0 && !r.fileDead {
		r.failf("the worker failed to start %d time(s) although its LocalDB file was never replaced", d)
	}

	// Retry envelope. A ladder at its cap makes one attempt per 30s (plus
	// jitter, which only lengthens a wait); a restarted process begins a
	// new one (attempts at 0, 1, 3, 7, 15, 31, 61, 91, 121s at the
	// earliest), and the ladder returns to its first step only after a
	// connection that lasted 30s. So a quiet stretch this long holds a
	// handful of attempts from one ladder and a whole new one at most:
	// under twenty, against 140 for one per second.
	attempts := r.e.g.dialsSince(since)
	if limit := 20; attempts > limit {
		r.failf("%d connection attempts in %v under mode %v, want at most %d", attempts, vtSettle, r.mode, limit)
	}

	if r.mode != vtOK {
		r.hadFault = true
		return
	}
	r.stats.settlesHealthy++
	p := u.current()
	hub, _, _ := r.e.g.cur()
	switch {
	case r.fileDead:
		// The process cannot start (see above).
	case p == nil:
		r.failf("global has been healthy for %v but no worker process is running", vtSettle)
	case !p.w.Connected() || !hub.IsWorkerConnected(vtSlug, vtWorkerID) || vtViewGen(p.cache) != r.gen:
		held := vtViewGen(p.cache)
		switch {
		case !p.w.Connected() && r.hungSince == p.n:
			// The worker reads its hello reply with no deadline, so it
			// still waits on a connection that an earlier, wedged global
			// accepted and never answered or closed. A restart of that
			// global, or anything in the path closing the connection, ends
			// the wait; nothing on the worker's side does.
			r.tolerate(vtTolHelloHang)
		case p.w.Connected() && held != r.gen && r.seqOf[held] == r.seqOf[r.gen]:
			// After a restore of global's database moved the site's
			// sequence back, the current generation was published under
			// the number the worker already holds. Hello and push compare
			// sequence numbers only, so the hello is answered no_change
			// and the push dropped as a duplicate: the worker keeps the
			// older content until the site's next change.
			r.tolerate(vtTolSeqReuse)
		default:
			r.failf("global has been healthy for %v but the worker is not connected and current: connected=%v hub-connected=%v gen=%d want %d (image syncs in flight: %d)",
				vtSettle, p.w.Connected(), hub.IsWorkerConnected(vtSlug, vtWorkerID), held, r.gen, p.syncing.Load())
		}
	default:
		r.stats.converged++
		// Ops are routable: the site's pending op reached the executor.
		ops := p.cache.PendingPowerOps()
		if len(ops) != 1 || ops[0].ID != vtOpID(r.gen) {
			r.failf("connected and current, but the executor sees %d pending op(s), want the site's one (%s; image syncs in flight: %d)", len(ops), vtOpID(r.gen), p.syncing.Load())
		}
	}
	r.hadFault = false
}

func (r *vtRun) damage(kind int) {
	u := r.e.u
	// Damage happens to a file nobody has open: take the unit down, damage,
	// and let Restart= bring it back.
	select {
	case u.crash <- struct{}{}:
		synctest.Wait()
	default: // between restarts: nothing has the file open
	}
	db, err := OpenLocalDB(u.dbPath)
	if err != nil {
		return // already not a database
	}
	r.stats.damages++
	switch kind {
	case 0:
		db.db.Exec(`UPDATE kv SET value = '{"seq_num": 7, "site"' WHERE key = 'snapshot_meta'`)
	case 1:
		db.db.Exec(`UPDATE cached_machines SET data = '{"id": "trunc' WHERE rowid = (SELECT min(rowid) FROM cached_machines)`)
	case 2:
		db.db.Exec(`DELETE FROM kv WHERE key = 'snapshot_meta'`)
	case 3:
		db.db.Exec(`DELETE FROM cached_machines`)
	case 4:
		db.db.Exec(`UPDATE kv SET value = '[1,2' WHERE key = 'op_state'`)
	default:
		closeQuiet(db)
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(u.dbPath + suffix)
		}
		if err := os.WriteFile(u.dbPath, []byte(strings.Repeat("not a sqlite database\n", 300)), 0o644); err != nil {
			r.t.Fatalf("replace worker.db: %v", err)
		}
		r.fileDead = true
		return
	}
	closeQuiet(db)
	r.damaged = true
}

func (r *vtRun) apply(ev vtEvent) {
	r.trail = append(r.trail, ev.String())
	r.stats.events++
	switch ev.Kind {
	case vtEvAdvance:
		time.Sleep(ev.D)
	case vtEvMode:
		r.mode = ev.Mode
		r.stats.modeSeen[ev.Mode]++
		if ev.Mode != vtOK {
			r.hadFault = true
		}
		if ev.Restart {
			r.hungSince = 0 // the wedged instance and its connections are gone
		}
		r.e.g.setMode(ev.Mode, ev.Restart)
	case vtEvBump, vtEvReset:
		r.gen++
		back := int64(0)
		if ev.Kind == vtEvReset {
			r.stats.resets++
			back = int64(ev.Back)
		}
		r.seqOf[r.gen] = r.e.g.bump(back)
	case vtEvImage:
		r.image = ev.Image
		r.stats.imageSeen[ev.Image]++
		r.e.setImage(ev.Image)
	case vtEvCrash:
		r.stats.crashes++
		select {
		case r.e.u.crash <- struct{}{}:
		default: // between restarts already
		}
	case vtEvDamage:
		r.damage(ev.Damage)
	case vtEvProbe:
		r.probe()
	case vtEvDrop:
		hub, _, _ := r.e.g.cur()
		hub.Close()
	case vtEvSettle, vtNumEvents:
		r.settle()
		return
	}
	r.check()
}

func TestPropWorkerLifecycleVirtualTime(t *testing.T) {
	stats := &vtStats{tolerated: map[string]int{}}
	prevLog := slog.Default()
	slog.SetDefault(quietLog)
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	rapid.Check(t, func(rt *rapid.T) {
		events := vtGenEvents(rt)
		dir, err := os.MkdirTemp("", "swvt")
		if err != nil {
			rt.Fatalf("temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		stats.cases++

		rapid.SyncTest(rt, func(rt *rapid.T) {
			e := &vtEnv{imageCh: make(chan struct{})}
			e.g = newVTGlobal()
			e.u = &vtUnit{e: e, dbPath: filepath.Join(dir, "worker.db"),
				crash: make(chan struct{}), stop: make(chan struct{}), exited: make(chan struct{})}
			go e.u.run()
			r := &vtRun{t: rt, stats: stats, e: e, gen: 1, seqOf: map[int]int64{1: vtFirstSeq}}
			defer func() {
				// Tear down inside the bubble whatever the outcome, or the
				// bubble never drains.
				close(e.u.stop)
				<-e.u.exited
				e.g.close()
				// A hub goroutine can still be inside a timed wait (a hello
				// waiting RebuildTimeout on its rebuild). The bubble's clock
				// stops when this function returns, so wait those out here.
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				stats.procs += int(e.u.procs.Load())
				stats.startFailures += int(e.u.startFailures.Load())
				stats.maxGen = max(stats.maxGen, r.gen)
			}()
			r.check()
			for _, ev := range events {
				r.apply(ev)
			}
		})
	})
	t.Logf("coverage: cases=%d events=%d processes=%d start-failures=%d settles=%d (healthy=%d converged=%d) probes=%d (routed=%d answered=%d) restarts-observed=%d (serving a persisted snapshot=%d) resets=%d damages=%d crashes=%d max-generation=%d",
		stats.cases, stats.events, stats.procs, stats.startFailures, stats.settles, stats.settlesHealthy, stats.converged,
		stats.probes, stats.probesRouted, stats.probesAnswered, stats.restartsFromDisk, stats.restartsServing, stats.resets, stats.damages, stats.crashes, stats.maxGen)
	modes := make([]string, 0, vtNumModes)
	for m := range vtNumModes {
		modes = append(modes, fmt.Sprintf("%v=%d", m, stats.modeSeen[m]))
	}
	images := make([]string, 0, vtNumImage)
	for m := range vtNumImage {
		images = append(images, fmt.Sprintf("%v=%d", m, stats.imageSeen[m]))
	}
	t.Logf("mode changes: %s; image modes: %s", strings.Join(modes, " "), strings.Join(images, " "))
	ids := make([]string, 0, len(stats.tolerated))
	for id, n := range stats.tolerated {
		ids = append(ids, fmt.Sprintf("%s=%d", id, n))
	}
	slices.Sort(ids)
	t.Logf("tolerated behaviours hit: %s", strings.Join(ids, " "))
}
