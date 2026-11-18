//go:build integration

package qemutest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/logring"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// SiteWorker is the in-process site worker: real Cache, real
// DHCP/TFTP/HTTP servers, real wsconn dialed at FakeHub. One per
// test per Network — DHCP/TFTP/HTTP bind to Network.GatewayIP and
// collide if two SiteWorkers share the same network.
type SiteWorker struct {
	Slug     string
	WorkerID string
	FQDN     string
	Token    string
	HubURL   string
	Cache    *worker.Cache
	LocalDB  *worker.LocalDB
	WSConn   *worker.WSConn
	LogRing  *logring.Ring

	Images       *ImageDir
	Network      *Network
	HTTPPort     int
	defaultImage string

	ctx    context.Context
	cancel context.CancelFunc
	// wg tracks the DHCP/HTTP/wsconn goroutines so Shutdown can
	// wait for them to drain. Required for Restart to know when
	// ports have actually been released by the prior
	// ListenAndServe calls. TFTP runs as a subprocess and is
	// tracked separately via tftpProc / tftpDrainDone.
	wg sync.WaitGroup
	t  *testing.T

	// tftpProc is the running qemutest-tftpd subprocess. nil when
	// not started. Shutdown sends SIGTERM (which the binary handles
	// cleanly via srv.Shutdown), waits for the drain goroutine to
	// EOF on the pipe, then flushes tftpStdout via t.Log.
	tftpProc      *exec.Cmd
	tftpStdout    *bytes.Buffer
	tftpStdoutMu  sync.Mutex
	tftpDrainDone chan struct{}
}

// SiteWorkerConfig configures the per-test site worker. Slug is
// required; the rest have sensible defaults.
type SiteWorkerConfig struct {
	Slug     string
	WorkerID string // default: uuid.New().String()
	FQDN     string // default: "worker.<slug>.test"
	Network  *Network
	Images   *ImageDir

	// HTTPPort for httpboot. Bound to Network.GatewayIP inside the
	// per-test network namespace. Defaults to 8080 (production
	// default). Host-level *:8080 listeners (jetstream, prometheus,
	// etc.) no longer collide because the listener lives in its own
	// netns.
	HTTPPort int

	// DefaultImage is the image directory name under ImagesDir.
	// Defaults to "noble".
	DefaultImage string

	// PowerFactory, when non-nil, wires the real worker power seam:
	// WSConn.SetPowerControllerFactory (so hub-originated power_status
	// RPCs build real controllers) AND a worker.Executor driven off the
	// real cache with WSConn as claimer at a short test interval. The
	// seam is the production powerControllerFactory path (or a test
	// equivalent using power.BuildBMCDialOptions + power.NewBMCController);
	// nil leaves the site worker with no BMC actuation/status path (the
	// historical behavior, preserved for tests that don't exercise BMC).
	PowerFactory worker.PowerControllerFactory
}

// NewSiteWorker registers with FakeHub, starts Cache/wsconn, waits
// for the first snapshot to apply, then brings up DHCP/TFTP/HTTP.
// Mirrors the production site.go bootstrap order.
func (e *Env) NewSiteWorker(t *testing.T, cfg SiteWorkerConfig) *SiteWorker {
	t.Helper()
	if cfg.Slug == "" {
		t.Fatalf("qemutest: SiteWorkerConfig.Slug is required")
	}
	if cfg.Network == nil {
		t.Fatalf("qemutest: SiteWorkerConfig.Network is required")
	}
	if cfg.Images == nil {
		t.Fatalf("qemutest: SiteWorkerConfig.Images is required")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = uuid.New().String()
	}
	if cfg.FQDN == "" {
		cfg.FQDN = "worker." + cfg.Slug + ".test"
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "noble"
	}

	log := slog.New(slog.NewTextHandler(testLogWriter{t: t},
		&slog.HandlerOptions{Level: slog.LevelInfo}))

	token := e.Hub.RegisterWorker(cfg.Slug, cfg.WorkerID, cfg.FQDN)

	// LocalDB in a per-test tmp dir.
	localDBPath := filepath.Join(t.TempDir(), "worker.db")
	localDB, err := worker.OpenLocalDB(localDBPath)
	if err != nil {
		t.Fatalf("qemutest: open LocalDB: %v", err)
	}
	localDB.SetLogger(log)

	cache := worker.NewCache(localDB)
	cache.ImagesDir = cfg.Images.Path

	logRing := logring.New(16<<20, time.Hour)
	cache.SetLogSink(logRing)

	wsConn := worker.NewWSConn(worker.WSConnConfig{
		GlobalURL: e.Hub.URL,
		Token:     token,
		WorkerID:  cfg.WorkerID,
		FQDN:      cfg.FQDN,
		Version:   testVersion(),
	}, cache, localDB, logRing)
	wsConn.SetLogger(log)
	// Must be set before wsConn.Run: startSiteServices can serve
	// power_status RPCs as soon as the connection is live, and the
	// executor shares the same factory.
	if cfg.PowerFactory != nil {
		wsConn.SetPowerControllerFactory(cfg.PowerFactory)
	}

	ctx, cancel := context.WithCancel(context.Background())

	sw := &SiteWorker{
		Slug:         cfg.Slug,
		WorkerID:     cfg.WorkerID,
		FQDN:         cfg.FQDN,
		Token:        token,
		HubURL:       e.Hub.URL,
		Cache:        cache,
		LocalDB:      localDB,
		WSConn:       wsConn,
		LogRing:      logRing,
		Images:       cfg.Images,
		Network:      cfg.Network,
		HTTPPort:     cfg.HTTPPort,
		defaultImage: cfg.DefaultImage,
		ctx:          ctx,
		cancel:       cancel,
		t:            t,
	}

	sw.wg.Add(1)
	go func() {
		defer sw.wg.Done()
		if err := wsConn.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("wsconn terminated", "err", err)
		}
	}()

	// Wait for the initial snapshot. Test must have Push'd one
	// before calling NewSiteWorker, or the hub's Rebuilder falls
	// back to an empty snapshot.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if err := wsConn.WaitForSync(syncCtx); err != nil {
		sw.Shutdown()
		t.Fatalf("qemutest: wait for initial sync: %v", err)
	}

	// Start services now that Cache has DHCP/site info.
	sw.startServices(log, cfg.DefaultImage)

	// Start the executor with the real power factory (when wired), using
	// the real cache, WSConn as claimer, and a short test interval so a
	// pushed pending op is driven in ms rather than the production 5s tick.
	if cfg.PowerFactory != nil {
		executor := worker.NewExecutor(worker.ExecutorConfig{
			Cache:    cache,
			Factory:  cfg.PowerFactory,
			Claimer:  wsConn,
			WorkerID: cfg.WorkerID,
			Interval: bmcExecutorInterval,
		})
		executor.SetLogger(log)
		sw.wg.Add(1)
		go func() {
			defer sw.wg.Done()
			if err := executor.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("executor terminated unexpectedly", "err", err)
			}
		}()
	}

	t.Cleanup(sw.Shutdown)
	return sw
}

// bmcExecutorInterval is the test executor poll interval. Short enough
// that a pushed pending op is driven quickly, long enough not to spin.
const bmcExecutorInterval = 200 * time.Millisecond

func (sw *SiteWorker) startServices(log *slog.Logger, defaultImage string) {
	sw.t.Helper()

	// runService runs a server's ListenAndServe in a goroutine,
	// tracked by sw.wg so Shutdown can wait for it to return
	// (which releases the bound port). If the server errors while
	// the SiteWorker's ctx is still alive, that's a bind failure
	// (or similar) — fail the test loudly instead of logging into
	// the void. After ctx is cancelled, any error is expected
	// shutdown and silently ignored.
	runService := func(name string, fn func(context.Context) error) {
		sw.wg.Add(1)
		go func() {
			defer sw.wg.Done()
			err := fn(sw.ctx)
			if err == nil || sw.ctx.Err() != nil {
				return
			}
			sw.t.Errorf("qemutest: %s server terminated unexpectedly: %v", name, err)
		}()
	}

	// DHCP. Listener bound inside the test netns via NSListenPacket;
	// SO_BINDTODEVICE (the production iface bind) is skipped because
	// netns isolation gives us the equivalent — the bridge is the
	// only interface that can deliver DISCOVER broadcasts to us.
	//
	// The relay-reply path (sendRelayReply -> DialUDP) gets the same
	// InNetns treatment via SetRelayDial; today's matrix doesn't
	// exercise relay yet, but leaving a known leak
	// would contradict per-test netns isolation's whole point. See plan doc DHCP audit.
	dhcpCfg := buildDHCPConfig(sw, log)
	dhcpHandler := dhcp.NewHandler(dhcpCfg)
	dhcpHandler.SetLogger(log)
	dhcpServer := dhcp.NewServer(sw.Network.Bridge, dhcpHandler)
	dhcpServer.SetConnHook(func() (net.PacketConn, error) {
		conn, err := NSListenPacket(sw.Network.NSName, "udp4", ":67")
		if err != nil {
			return nil, err
		}
		// DHCP server has to broadcast OFFER/ACK to 255.255.255.255
		// because the client doesn't have an IP yet. Default
		// net.ListenPacket sockets don't set SO_BROADCAST; the
		// production server4 path does. Replicate it here so the
		// WithConn path matches.
		if udpConn, ok := conn.(*net.UDPConn); ok {
			rawConn, rerr := udpConn.SyscallConn()
			if rerr != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("dhcp conn SyscallConn: %w", rerr)
			}
			var sockErr error
			if cerr := rawConn.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}); cerr != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("dhcp conn control: %w", cerr)
			}
			if sockErr != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("dhcp set SO_BROADCAST: %w", sockErr)
			}
		}
		return conn, nil
	})
	dhcpServer.SetRelayDial(func(dest *net.UDPAddr) (net.Conn, error) {
		var dialed net.Conn
		err := InNetns(sw.Network.NSName, func() error {
			c, derr := net.DialUDP("udp4", nil, dest)
			if derr != nil {
				return derr
			}
			dialed = c
			return nil
		})
		if err != nil {
			return nil, err
		}
		return dialed, nil
	})
	runService("DHCP", dhcpServer.ListenAndServe)

	// TFTP runs as a subprocess inside the netns. See
	// cmd/qemutest-tftpd/main.go and the plan doc's TFTP section
	// for the rationale: pin/tftp opens per-transfer ephemeral
	// sockets on internal handler goroutines we can't pin to a
	// netns-locked OS thread, so an in-process server inside our
	// test goroutine would land its data sockets in the host netns
	// and silently drop transfers.
	//
	// The KnownIPs filter (dhcpHandler.HasLease) is intentionally
	// dropped here: netns isolation already restricts the listener
	// to VMs on the same bridge in the same netns. No host or
	// other-test process can reach gateway:69.
	sw.startTFTPSubprocess(log)

	// HTTP boot. AgentBinPath points at the agent binary the qemutest
	// harness pre-built into sw.Images.Path. Production wires this to
	// /usr/bin/slagworks-agent (placed by the slagworks .deb on the
	// gateway).
	//
	// The listener is opened inside the test's netns via NSListen
	// (see nsexec.go). Once the fd exists it carries the netns; the
	// serving goroutine that calls Accept can run on any thread.
	agentBinPath := filepath.Join(sw.Images.Path, "slagworks-agent")
	httpAddr := fmt.Sprintf("%s:%d", sw.Network.Gateway, sw.HTTPPort)
	httpLn, err := NSListen(sw.Network.NSName, "tcp", httpAddr)
	if err != nil {
		sw.t.Fatalf("qemutest: NSListen http %s in netns %s: %v",
			httpAddr, sw.Network.NSName, err)
	}
	httpServer := httpboot.NewServer(
		httpAddr,
		httpboot.ServerConfig{
			ImagesDir:    sw.Images.Path,
			DefaultImage: defaultImage,
			HTTPPort:     sw.HTTPPort,
			DeployMode:   true,
			Provider:     sw.Cache,
			LogSink:      sw.LogRing,
			AgentBinPath: func() string { return agentBinPath },
		},
	)
	httpServer.SetLogger(log)
	runService("HTTP", func(ctx context.Context) error {
		return httpServer.Serve(ctx, httpLn)
	})
}

// startTFTPSubprocess launches cmd/qemutest-tftpd via NSExec inside
// the network's namespace. Blocks until the binary emits its "ready"
// line on stdout (or 5s elapses, in which case the test fatals).
//
// Stdout drain: we cannot forward each request log directly to
// t.Logf because the drain goroutine outlives the test goroutine —
// any t.Log call after the test returns panics with
// "Log in goroutine after TestX has completed". Instead we drain
// into a thread-safe bytes.Buffer; Shutdown kills the process,
// waits for drain to EOF, then flushes the buffer via t.Log under
// the test goroutine.
func (sw *SiteWorker) startTFTPSubprocess(log *slog.Logger) {
	sw.t.Helper()

	binPath := filepath.Join(sw.Images.Path, "qemutest-tftpd")
	addr := fmt.Sprintf("%s:69", sw.Network.Gateway)

	cmd := NSExec(sw.Network.NSName, binPath,
		"--addr", addr,
		"--dir", sw.Images.Path,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sw.t.Fatalf("qemutest: tftp StdoutPipe: %v", err)
	}
	// Stderr lands directly in the test process's stderr (and thus
	// the GH Actions log). Using a pipe here would compete for the
	// same drain pattern; flag-style errors and the readiness
	// failure path both go to stderr so this is fine.
	// (A nil Stderr is /dev/null in os/exec — every tftpd error
	// message would vanish and a dead tftpd would surface minutes
	// later as a PXE-fetch timeout that reads like a product bug.)
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		sw.t.Fatalf("qemutest: start qemutest-tftpd (%s): %v", binPath, err)
	}

	sw.tftpProc = cmd
	sw.tftpStdout = &bytes.Buffer{}
	sw.tftpDrainDone = make(chan struct{})

	// Readiness handshake: bufio.Scanner.Scan() the first line on
	// the pipe. The subprocess writes "ready\n" only AFTER
	// net.ListenUDP succeeds. Without this, the VM PXE-fetches
	// before bind completes and we flake on iPXE retries.
	//
	// Buffer the first line in our own scanner-on-pipe; on success
	// we hand the pipe to the drain goroutine. Use a bounded
	// timeout to fail loud if the subprocess crashes during bind.
	type readyResult struct {
		line string
		err  error
	}
	readyCh := make(chan readyResult, 1)
	// bufio.ReadString, not a raw Read: a kernel scheduling race can
	// deliver "ready\n" in a partial chunk ("rea"), which a raw Read
	// would fail as `first line was "rea"` — a rigging failure that
	// reads like a tftpd bug. Same fix the serial-relay handshake got
	// (bmcsim.go); this one predated it and drifted.
	bufReader := bufio.NewReader(stdout)
	go func() {
		line, err := bufReader.ReadString('\n')
		if err != nil {
			readyCh <- readyResult{err: fmt.Errorf("read first line: %w", err)}
			return
		}
		readyCh <- readyResult{line: line}
	}()

	select {
	case rr := <-readyCh:
		if rr.err != nil {
			_ = cmd.Process.Kill()
			sw.t.Fatalf("qemutest: tftp readiness handshake: %v", rr.err)
		}
		if strings.TrimRight(rr.line, "\n") != "ready" {
			_ = cmd.Process.Kill()
			sw.t.Fatalf("qemutest: tftp first line was %q, expected \"ready\\n\"", rr.line)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		sw.t.Fatalf("qemutest: tftp readiness handshake timed out (5s); subprocess hung before binding %s", addr)
	}

	// Drain goroutine: blocks on the pipe until the subprocess
	// closes its stdout (via exit). Writes go to the buffer under
	// a mutex so concurrent log line emission is safe.
	go func() {
		defer close(sw.tftpDrainDone)
		// Scan from bufReader so bytes it buffered past "ready\n"
		// still reach the log.
		scanner := bufio.NewScanner(bufReader)
		// Match qemutest-tftpd's `log.Printf` line lengths; default
		// 64KB is generous for our request logs.
		for scanner.Scan() {
			sw.tftpStdoutMu.Lock()
			sw.tftpStdout.WriteString(scanner.Text())
			sw.tftpStdout.WriteByte('\n')
			sw.tftpStdoutMu.Unlock()
		}
	}()

	log.Info("qemutest: tftp subprocess started", "addr", addr, "ns", sw.Network.NSName)
}

// stopTFTPSubprocess sends SIGTERM, waits for clean exit (or
// SIGKILLs after 5s), waits for the stdout drain goroutine to
// finish, then flushes the buffered output via t.Log so the test
// log preserves request traces. Idempotent. MUST be called from
// the test goroutine because t.Log is not safe from arbitrary
// goroutines.
func (sw *SiteWorker) stopTFTPSubprocess() {
	if sw.tftpProc == nil {
		return
	}
	proc := sw.tftpProc
	sw.tftpProc = nil

	if proc.Process != nil {
		// SIGTERM gives the binary a chance to call srv.Shutdown
		// and flush its final log lines. SIGKILL would race the
		// drain goroutine and lose tail output.
		_ = proc.Process.Signal(syscall.SIGTERM)
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- proc.Wait() }()
	select {
	case <-exitCh:
		// Clean exit.
	case <-time.After(5 * time.Second):
		// Wedged. SIGKILL and reap.
		if proc.Process != nil {
			_ = proc.Process.Kill()
		}
		<-exitCh
	}

	// Drain blocks on stdout pipe; pipe closes when the subprocess
	// exits, so this should be near-instant after Wait returns.
	if sw.tftpDrainDone != nil {
		<-sw.tftpDrainDone
	}

	// Flush captured output. Only safe from the test goroutine.
	sw.tftpStdoutMu.Lock()
	out := sw.tftpStdout.String()
	sw.tftpStdoutMu.Unlock()
	if out != "" {
		sw.t.Logf("qemutest-tftpd stdout:\n%s", out)
	}
}

// Shutdown cancels the SiteWorker context, stopping all background
// goroutines, and waits for them to finish (which releases bound
// ports). Stops the TFTP subprocess and flushes its buffered
// output to t.Log. Idempotent.
func (sw *SiteWorker) Shutdown() {
	if sw.cancel != nil {
		sw.cancel()
		sw.cancel = nil
	}
	sw.wg.Wait()
	sw.stopTFTPSubprocess()
}

// Restart tears down the SiteWorker's running services + WS and
// brings them back up with a fresh context. Used to test
// reconnect/restart behavior. Partially mimics a process restart:
// Cache is rebuilt from scratch (so it rehydrates from the same
// LocalDB just as a fresh process would), but LocalDB itself is
// reused because worker.LocalDB has no Close method — the on-disk
// file is the only source of truth the rebuilt Cache reads from,
// so this gives us meaningfully-restart-like semantics.
func (sw *SiteWorker) Restart(log *slog.Logger) {
	sw.t.Helper()
	// Shutdown waits for all service goroutines to drain; when it
	// returns, bound ports have been released by the prior
	// ListenAndServe calls' deferred close/unbind.
	sw.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	sw.ctx = ctx
	sw.cancel = cancel

	sw.Cache = worker.NewCache(sw.LocalDB)
	sw.Cache.ImagesDir = sw.Images.Path
	// Re-wire the log sink: NewSiteWorker does this on the original
	// Cache; without it a restarted worker's cache-path structured
	// logs silently stop flowing to the ring, and a future test
	// asserting post-restart log_batch content would fail in a way
	// that reads like a product log-relay regression.
	sw.Cache.SetLogSink(sw.LogRing)

	sw.WSConn = worker.NewWSConn(worker.WSConnConfig{
		GlobalURL: sw.HubURL,
		Token:     sw.Token,
		WorkerID:  sw.WorkerID,
		FQDN:      sw.FQDN,
		Version:   testVersion(),
	}, sw.Cache, sw.LocalDB, sw.LogRing)
	sw.WSConn.SetLogger(log)

	sw.wg.Add(1)
	go func() {
		defer sw.wg.Done()
		if err := sw.WSConn.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("wsconn terminated after restart", "err", err)
		}
	}()
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if err := sw.WSConn.WaitForSync(syncCtx); err != nil {
		sw.t.Fatalf("qemutest: restart: wait for sync: %v", err)
	}

	sw.startServices(log, sw.defaultImage)
}

func buildDHCPConfig(sw *SiteWorker, log *slog.Logger) dhcp.Config {
	cfg := dhcp.Config{
		ServerIP:      sw.Network.GatewayIP,
		LeaseDuration: 5 * time.Minute,
		HTTPBootPort:  sw.HTTPPort,
	}

	snap := sw.Cache.GetDHCPConfig()
	if snap.Subnet == "" {
		// A qemutest SiteWorker with no DHCP config is a rigged test
		// (the author forgot Push before NewSiteWorker, or the fake
		// Rebuilder synthesized an empty snapshot for a never-pushed
		// slug), not a valid scenario: the VM's DISCOVER would go
		// unanswered and the test would die minutes later on a PXE
		// timeout that reads like a DHCP product bug. Fail loudly at
		// the rigging layer. (Production tolerates this state — a
		// worker may connect before its site has config — but no
		// qemu test exercises that on purpose.)
		sw.t.Fatalf("qemutest: snapshot for slug %q has no DHCP config — Push a snapshot before NewSiteWorker", sw.Slug)
	}
	if sw.LocalDB != nil {
		cfg.LeaseStore = sw.LocalDB.NewLeaseStore()
	}

	_, subnet, err := net.ParseCIDR(snap.Subnet)
	if err != nil {
		sw.t.Fatalf("qemutest: parse snapshot subnet %q: %v", snap.Subnet, err)
	}
	cfg.Subnet = *subnet
	cfg.RangeStart = net.ParseIP(snap.EphemeralPoolStart)
	cfg.RangeEnd = net.ParseIP(snap.EphemeralPoolEnd)
	cfg.Gateway = net.ParseIP(snap.Gateway)

	if snap.Promiscuous {
		log.Warn("qemutest: promiscuous DHCP enabled")
	} else {
		cfg.KnownMACs = func(mac string) bool {
			return sw.Cache.MachineByMAC(mac) != nil
		}
	}
	cfg.ReserveIP = sw.Cache.ReservedIPForMAC

	return cfg
}

// testLogWriter routes slog output to t.Log.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
