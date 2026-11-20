package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/envutil"
	"github.com/bluesky-social/slagworks/internal/healthserver"
	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/logring"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
	"github.com/bluesky-social/slagworks/internal/sdnotify"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/tftp"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// siteConfig holds the parsed environment for the site worker process.
// Site identity (slug, subnet, gateway) is NOT here — it comes from the
// snapshot the hub pushes after we authenticate with our token. The
// token carries site-slug binding server-side.
type siteConfig struct {
	apiURL     string
	token      string
	workerID   string // UUID, resolved from LocalDB or env override
	workerFQDN string // current FQDN, refreshed on startup
	// serial is the worker host's hardware serial (SMBIOS), read once at
	// startup and normalized (placeholders -> ""). Reported in the WS
	// hello for hardware-attested identity; "" means unverified.
	serial       string
	serverIP     string
	iface        string
	httpPort     int
	tftpDir      string
	imagesDir    string
	defaultImage string
	standby      bool
	vip          string
	healthAddr   string
	// oobDHCPEnabled is the cutover safety valve (SLAGWORKS_OOB_DHCP_ENABLED).
	// When false (the default), the worker binds only the fabric/relay leg
	// and never touches the OOB segment — so dnsmasq stays the OOB DHCP
	// authority. Flip to true to have slagworks take over OOB DHCP; flip
	// back to false (+ restart) to instantly fall back to dnsmasq. Gates
	// ONLY the OOB DHCP/TFTP/HTTP listeners, never the fabric leg or the
	// snapshot/WS machinery.
	oobDHCPEnabled bool
	// smcTool is the operator-installed Supermicro SMCIPMITool config
	// (SLAGWORKS_SMCIPMITOOL, SLAGWORKS_SMCIPMITOOL_BIN). Slagworks does
	// not ship the tool; see smcipmitool.InstallDoc.
	smcTool smcipmitool.Tool
}

type siteServiceConfig struct {
	cfg     siteConfig
	cache   *worker.Cache
	log     *slog.Logger
	localDB *worker.LocalDB
	wsConn  *worker.WSConn
	logRing *logring.Ring

	// Prometheus instrumentation, registered once per process in
	// runSite. Site services (re)created per VIP transition consume
	// these shared collectors; dhcpHandler is the live handler for
	// scrape-time lease counts (nil while services are down).
	metrics     *worker.SiteMetrics
	dhcpMetrics *dhcp.Metrics
	dhcpHandler atomic.Pointer[dhcp.Handler]

	// oobConflict is the process-lifetime latch for OOB DHCP conflict
	// probing. Created once in runSite so it survives standby VIP
	// transitions, consumed by startSiteServices (probe interlock) and
	// by the readiness handler + gauges.
	oobConflict *oobConflictGate

	// fabricPoolErr is non-nil while the fabric DHCP leg serves no pool
	// because the snapshot's DHCP config did not parse when the services
	// started. buildDHCPConfig sets it, reloadFabricPools clears it once
	// a parseable config is installed, service teardown clears it with
	// the handler, and readiness reports it.
	fabricPoolErr atomic.Pointer[string]
}

// agentBinPath is the path to the slagworks-agent binary placed by
// the slagworks .deb at apt-install time on the gateway host. The
// /files/bin/slagworks-agent HTTP handler serves this file directly.
// Configurable via SLAGWORKS_AGENT_BIN for non-deb test environments.
const defaultAgentBinPath = "/usr/bin/slagworks-agent"

func siteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Run the site worker (DHCP/TFTP/HTTP boot + snapshot sync)",
		RunE:  runSite,
	}
	cmd.AddCommand(workerIDCmd())
	return cmd
}

// workerIDCmd prints the persistent worker identity stored in the local DB.
func workerIDCmd() *cobra.Command {
	var reset bool
	cmd := &cobra.Command{
		Use:   "worker-id",
		Short: "Show or reset the persistent worker identity",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := envutil.Or("SLAGWORKS_DATA_DIR", "/var/lib/slagworks")
			db, err := worker.OpenLocalDB(filepath.Join(dataDir, "worker.db"))
			if err != nil {
				return fmt.Errorf("open local db: %w", err)
			}
			if reset {
				db.ClearWorkerIdentity()
				fmt.Println("worker identity cleared; a new UUID will be assigned on next startup")
				return nil
			}
			id, fqdn := db.GetWorkerIdentity()
			if id == "" {
				fmt.Println("no worker identity set (will be generated on next startup)")
				return nil
			}
			fmt.Printf("worker_id: %s\nfqdn:      %s\n", id, fqdn)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reset, "reset", false, "clear the stored identity so a new UUID is generated on next startup")
	return cmd
}

// resolveWorkerIdentity returns the persistent worker UUID and current FQDN.
// If SLAGWORKS_WORKER_ID is set it is used as an override. Otherwise the UUID
// is loaded from (or generated and saved to) the local DB. The FQDN is always
// refreshed to the current value so hostname changes are reflected.
func resolveWorkerIdentity(db *worker.LocalDB) (id, fqdn string) {
	fqdn = resolveFQDN()

	if v := os.Getenv("SLAGWORKS_WORKER_ID"); v != "" {
		db.SetWorkerIdentity(v, fqdn)
		return v, fqdn
	}

	storedID, _ := db.GetWorkerIdentity()
	if storedID != "" {
		db.SetWorkerIdentity(storedID, fqdn)
		return storedID, fqdn
	}

	newID := uuid.New().String()
	db.SetWorkerIdentity(newID, fqdn)
	return newID, fqdn
}

func resolveFQDN() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	if cname, err := net.LookupCNAME(hostname); err == nil {
		fqdn := strings.TrimSuffix(cname, ".")
		if fqdn != hostname {
			return fqdn
		}
	}
	if ips, err := net.LookupHost(hostname); err == nil && len(ips) > 0 {
		if names, err := net.LookupAddr(ips[0]); err == nil && len(names) > 0 {
			return strings.TrimSuffix(names[0], ".")
		}
	}
	return hostname
}

func loadSiteConfig() (siteConfig, error) {
	httpPort := 8080
	if v := os.Getenv("SLAGWORKS_HTTP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return siteConfig{}, configErrorf("invalid SLAGWORKS_HTTP_PORT %q: %v", v, err)
		}
		httpPort = p
	}

	apiURL, err := requireEnv("SLAGWORKS_API_URL")
	if err != nil {
		return siteConfig{}, err
	}
	token, err := requireEnv("SLAGWORKS_TOKEN")
	if err != nil {
		return siteConfig{}, err
	}
	serverIP, err := requireEnv("SLAGWORKS_SERVER_IP")
	if err != nil {
		return siteConfig{}, err
	}
	iface, err := requireEnv("SLAGWORKS_IFACE")
	if err != nil {
		return siteConfig{}, err
	}
	smcMode, err := smcipmitool.ParseMode(os.Getenv("SLAGWORKS_SMCIPMITOOL"))
	if err != nil {
		return siteConfig{}, configErrorf("%v", err)
	}

	return siteConfig{
		apiURL:         apiURL,
		token:          token,
		serverIP:       serverIP,
		iface:          iface,
		httpPort:       httpPort,
		tftpDir:        envutil.Or("SLAGWORKS_TFTP_DIR", "/srv/tftp"),
		imagesDir:      envutil.Or("SLAGWORKS_IMAGES_DIR", "/srv/images"),
		defaultImage:   envutil.Or("SLAGWORKS_DEFAULT_IMAGE", "noble-pxe"),
		standby:        os.Getenv("SLAGWORKS_STANDBY") == "true",
		vip:            os.Getenv("SLAGWORKS_VIP"),
		healthAddr:     envutil.Or("SLAGWORKS_HEALTH_ADDR", ":9090"),
		oobDHCPEnabled: os.Getenv("SLAGWORKS_OOB_DHCP_ENABLED") == "true",
		smcTool: smcipmitool.Tool{
			Mode: smcMode,
			Bin:  os.Getenv("SLAGWORKS_SMCIPMITOOL_BIN"),
		},
	}, nil
}

func runSite(cmd *cobra.Command, args []string) error {
	level := slog.LevelInfo
	if v := strings.ToLower(os.Getenv("SLAGWORKS_LOG_LEVEL")); v != "" {
		switch v {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	hostname, _ := os.Hostname()

	// Structured-log envelope ring. Site is a stateless forwarder:
	// the slog handler, the in-process SOL pump, and the agent's
	// /logs/ingest endpoint all push directly into this ring; the
	// WS forwarder drains it to global. 100 MiB or 1 hour, whichever
	// fills first; drop-oldest under pressure (drop counter tracks
	// loss).
	logRing := logring.New(100<<20, time.Hour)

	log := slog.New(slogkit.NewHandler(slogkit.HandlerOptions{
		Source: slogkit.SourceSite,
		Host:   hostname,
		Level:  level,
		Stderr: os.Stderr,
		Sink:   logRing,
	}))
	slog.SetDefault(log)
	log.Info("starting slagworksd site", "version", version, "log_level", level.String())

	cfg, err := loadSiteConfig()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); logRing.Close() }()

	dataDir := envutil.Or("SLAGWORKS_DATA_DIR", "/var/lib/slagworks")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	localDB, err := worker.OpenLocalDB(filepath.Join(dataDir, "worker.db"))
	if err != nil {
		return fmt.Errorf("opening local database: %w", err)
	}
	localDB.SetLogger(log)

	workerID, workerFQDN := resolveWorkerIdentity(localDB)
	cfg.workerID = workerID
	cfg.workerFQDN = workerFQDN
	// Hardware-attested self-identity: read the SMBIOS serial once at
	// startup and report it in the WS hello. A "" result (dmidecode absent
	// or a placeholder serial) does NOT block startup — global decides
	// eligibility. See internal/worker.ReadSelfSerial.
	cfg.serial = worker.ReadSelfSerial(log)
	log.Info("worker identity", "worker_id", workerID, "fqdn", workerFQDN, "serial", cfg.serial)

	// Thread the worker's own verified serial into the cache so the P4b
	// worker-side self-power interlock (pendingPowerOps) can skip an outlet
	// op that would cut this worker's own feed.
	cache := worker.NewCacheWithSerial(localDB, cfg.serial)
	cache.ImagesDir = cfg.imagesDir
	cache.SetLogSink(logRing)

	wsConn := worker.NewWSConn(worker.WSConnConfig{
		GlobalURL: cfg.apiURL,
		Token:     cfg.token,
		WorkerID:  cfg.workerID,
		FQDN:      cfg.workerFQDN,
		Version:   version,
		Serial:    cfg.serial,
		// Re-evaluated per hello and per status read, so installing the
		// tool takes effect on the next reconnect without a restart.
		SMCIPMIToolStatus: cfg.smcTool.Status,
	}, cache, localDB, logRing)
	wsConn.SetLogger(log)
	// Power-status RPCs from the hub reuse the same controller factory
	// as the op executor. Wired on both active and standby workers:
	// status is a read, and the hub picks ANY connected worker for the
	// site, so a standby must be able to answer.
	powerFactory := newPowerControllerFactory(cfg.smcTool, log)
	wsConn.SetPowerControllerFactory(powerFactory)
	go func() {
		if err := wsConn.Run(ctx); err != nil {
			log.Error("wsconn error", "err", err)
		}
	}()

	// Try for a fresh snapshot over WS, but if the global is
	// unreachable (or the site worker booted before the global),
	// proceed with whatever the Cache loaded from LocalDB.
	// wsConn.Run is already retrying in the background; services
	// will pick up a live snapshot whenever the connection succeeds
	// and ApplySnapshot fires.
	syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
	defer syncCancel()
	if err := wsConn.WaitForSync(syncCtx); err != nil {
		if cache.GetDHCPConfig().Subnet == "" {
			log.Warn("no cached snapshot available and WS not connected; "+
				"DHCP/TFTP will not serve until WS catches up",
				"err", err)
		} else {
			log.Warn("WS not connected yet; serving DHCP/TFTP from cached snapshot",
				"err", err, "seq", cache.LastSeqNum())
		}
	}

	// One-shot summary of what's loaded after the first sync (or
	// cache-fallback). Subsequent snapshot applies have their own log
	// lines via WSConn; this is the "did slagworksd come up with the
	// state I expected?" answer for the operator after a restart.
	logSnapshotSummary(log, cache, localDB)

	sc := &siteServiceConfig{
		cfg:     cfg,
		cache:   cache,
		log:     log,
		localDB: localDB,
		wsConn:  wsConn,
		logRing: logRing,
		// Process-lifetime OOB DHCP conflict latch (survives standby VIP
		// transitions). Set before registerSiteGauges/readiness read it.
		oobConflict: newOOBConflictGate(),
	}

	// Prometheus registry, served from the health server's /metrics.
	// Registered ONCE here — startSiteServices can run repeatedly
	// (standby VIP transitions) and MustRegister panics on duplicates,
	// so collectors live on sc and services consume them.
	promReg := prometheus.NewRegistry()
	sc.metrics = worker.NewSiteMetrics(promReg)
	sc.dhcpMetrics = dhcp.NewMetrics(promReg)
	dhcp.RegisterLeaseCollector(promReg, sc.dhcpHandler.Load)
	registerSiteGauges(promReg, sc)

	healthSrv := healthserver.New(cfg.healthAddr, func(_ context.Context) error {
		if !wsConn.Connected() {
			return fmt.Errorf("not connected to global worker")
		}
		if cache.ImageCount() == 0 {
			return fmt.Errorf("no images synced from global")
		}
		// systemd-bootx64.efi comes from imagesDir (placed by the slagworks
		// .deb). overlay.cpio is gone — the BOOT hook is baked into the
		// initrd at build time. The slagworks-agent binary is served from the
		// path the .deb installs at; verify it exists at startup so we 503
		// fast if the host is misconfigured.
		for _, name := range []string{"systemd-bootx64.efi"} {
			path := filepath.Join(cfg.imagesDir, name)
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("missing boot asset: %s", name)
			}
		}
		agentBin := envutil.Or("SLAGWORKS_AGENT_BIN", defaultAgentBinPath)
		if _, err := os.Stat(agentBin); err != nil {
			return fmt.Errorf("missing slagworks-agent binary at %s: %w", agentBin, err)
		}
		// OOB DHCP conflict interlock: report unready with a stable reason
		// (including the foreign server identifier when one was observed)
		// once a foreign OFFER or a probe error latched OOB DHCP off. Other
		// OOB TFTP/HTTP, fabric, health, and metrics stay up.
		if err := oobConflictReadinessError(sc.oobConflict); err != nil {
			return err
		}
		if reason := sc.fabricPoolErr.Load(); reason != nil {
			return errors.New(*reason)
		}
		return nil
	})
	healthSrv.SetMetricsRegistry(promReg)
	healthSrv.SetLogger(log)
	go func() {
		if err := healthSrv.ListenAndServe(ctx); err != nil {
			log.Error("health server error", "err", err)
		}
	}()

	// Count snapshot applies for the whole process lifetime (the other
	// snapshot-applied subscribers live in startSiteServices and churn
	// with VIP transitions).
	_ = cache.OnSnapshotApplied(func() { sc.metrics.SnapshotAppliesTotal.Inc() })
	_ = cache.OnSnapshotApplied(newSMCToolWarner(cfg.smcTool, cache.SupermicroMachineCount, log))

	if !cfg.standby {
		executor := worker.NewExecutor(worker.ExecutorConfig{
			Cache:    cache,
			Factory:  powerFactory,
			Claimer:  wsConn, // WSConn.ClaimOp implements Claimer
			WorkerID: cfg.workerID,
			Metrics:  sc.metrics,
		})
		executor.SetLogger(log)
		go func() {
			if err := executor.Run(ctx); err != nil {
				log.Error("executor error", "err", err)
			}
		}()
	}

	if cfg.standby && cfg.vip == "" {
		log.Warn("SLAGWORKS_STANDBY=true but SLAGWORKS_VIP is empty, starting in active mode")
	}

	// Serial console capture for BMC-less machines (switches) whose NetBox
	// console port is cabled to one of THIS host's ttys. Process-lifetime,
	// not VIP-scoped like the BMC CastManager in startSiteServices: the
	// line is a local device node only this host can read, so a standby
	// worker must capture its own lines too. SerialCastLister lists only
	// machines whose console server's serial is this worker's, and BMC
	// machines never qualify, so the two managers' sets are disjoint.
	serialCastDone := make(chan struct{})
	{
		serialCast := worker.NewCastManager(worker.CastManagerConfig{
			Dialer: &worker.TransportCastDialer{
				Cache:  cache,
				Serial: &worker.SerialTTYDialer{Cache: cache, Logger: log},
			},
			Sink:   wsConn,
			Resume: wsConn,
			Cache:  worker.SerialCastLister{Cache: cache},
			Log:    log,
		})
		serialCast.Start(ctx)
		unsub := cache.OnSnapshotApplied(serialCast.Reconcile)
		serialCast.Reconcile()
		go func() {
			defer close(serialCastDone)
			<-ctx.Done()
			unsub()
			serialCast.Shutdown()
		}()
	}

	// In active mode, svcDone closes once every site service has shut
	// down — including CastManager reaping its ipmitool process groups.
	// Exiting without waiting orphans SOL children mid-stream, skipping
	// SOL deactivate and wedging the BMC's scarce session slots (AMI
	// BMCs hold ~5 and don't free them on abrupt disconnect). The
	// standby path does the same wait in runSiteStandby.
	var svcDone <-chan struct{}
	if cfg.standby && cfg.vip != "" {
		log.Info("starting in standby mode, waiting for VIP",
			"worker_id", cfg.workerID,
			"vip", cfg.vip,
		)
		done, err := runSiteStandby(ctx, sc)
		if err != nil {
			return err
		}
		svcDone = done
	} else {
		if cfg.vip != "" {
			go monitorVIPActive(ctx, cfg.vip, log)
		}
		svcCtx, svcCancel := context.WithCancel(ctx)
		defer svcCancel()
		done, err := startSiteServices(svcCtx, sc)
		if err != nil {
			return err
		}
		svcDone = done
		siteInfo := cache.GetSiteInfo()
		log.Info("slagworksd site running",
			"worker_id", cfg.workerID,
			"site_slug", siteInfo.Slug,
			"server_ip", cfg.serverIP,
		)
	}

	// Report readiness for what this process does on its own: its site
	// services are running (a standby is watching for its VIP), from the
	// cached snapshot if global has not answered within the wait above.
	// It deliberately does not wait for global: the unit is Type=notify
	// with systemd's start timeout, so tying READY=1 to an accepted hello
	// would have systemd kill and restart a serving worker for as long as
	// global is unreachable or refuses it. Sent only after the start-up
	// error returns above, so a worker that cannot start its services
	// still fails its start. The connection to global is reported by
	// /readyz and slagworks_site_ws_connected.
	if err := sdnotify.Send(sdnotify.Ready); err != nil {
		log.Warn("sd_notify READY=1 failed", "err", err)
	}

	<-ctx.Done()
	log.Info("shutting down")
	if svcDone != nil {
		<-svcDone
	}
	<-serialCastDone
	return nil
}

// registerSiteGauges wires the point-in-time gauges that read live
// process state at scrape time (GaugeFunc/CounterFunc): connection
// status, queue depths, snapshot seq, log-ring drops. Counters with
// per-event increments live in worker.SiteMetrics / dhcp.Metrics.
func registerSiteGauges(reg prometheus.Registerer, sc *siteServiceConfig) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "slagworks_site_ws_connected",
		Help: "1 when the WebSocket to global is connected and synced, else 0.",
	}, func() float64 {
		if sc.wsConn.Connected() {
			return 1
		}
		return 0
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "slagworks_site_outbox_depth",
		Help: "Observations queued for push to global. Sustained growth = global unreachable or rejecting.",
	}, func() float64 { return float64(sc.cache.OutboxDepth()) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "slagworks_site_ops_active",
		Help: "Operations in the snapshot currently pending, claimed, or running.",
	}, func() float64 { return float64(sc.cache.ActiveOpCount()) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "slagworks_site_snapshot_seq",
		Help: "Sequence number of the last applied site snapshot. Compare against global's slagworks_snapshot_seq for staleness.",
	}, func() float64 { return float64(sc.cache.LastSeqNum()) }))
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "slagworks_site_logring_dropped_total",
		Help: "Log envelopes dropped from the forwarding ring under pressure (drop-oldest), past max age, or too large for one log_batch.",
	}, func() float64 { return float64(sc.logRing.Stats().Dropped) }))

	// OOB DHCP conflict interlock metrics: the conflict gauge and the
	// fail-closed probe-error counter (label-free, read from the
	// process-lifetime gate at scrape time).
	registerOOBDHCPConflictMetrics(reg, sc.oobConflict)
}

// logSnapshotSummary emits a single Info log line summarizing what the
// site worker has loaded after the first snapshot sync (or fallback to
// the on-disk cache). Called once per startup; subsequent reloads have
// their own per-apply log line. The "everything is zero" case logs
// explicitly so an empty cache is visible rather than silent.
func logSnapshotSummary(log *slog.Logger, cache *worker.Cache, localDB *worker.LocalDB) {
	machines := cache.MachineCount()
	images := cache.ImageCount()
	leases := int64(0)
	if localDB != nil {
		leases = localDB.CountActiveDHCPLeases()
	}
	seq := cache.LastSeqNum()
	site := cache.GetSiteInfo().Slug
	if machines == 0 && images == 0 && leases == 0 && seq == 0 {
		log.Info("snapshot summary: empty cache (no snapshot applied yet)",
			"site_slug", site,
			"machines", 0, "images", 0, "leases", 0, "seq", 0,
		)
		return
	}
	log.Info("snapshot summary",
		"site_slug", site,
		"machines", machines,
		"images", images,
		"leases", leases,
		"seq", seq,
	)
}

// buildPoolConfig translates the snapshot's DHCPConfig into the
// handler's hot-reloadable PoolConfig. Called at service start AND on
// every snapshot apply (via the OnSnapshotApplied subscription), so
// pool / relay-pool / promiscuous edits take effect without a restart.
//
// An error means the whole pool set is unusable. Neither caller treats
// it as fatal: the gateway is the gateway_ip custom field on a NetBox
// prefix, free text global copies without parsing, and the snapshot
// carrying it is persisted and re-served, so failing the process would
// fail every restart too. Service start runs without a fabric pool and
// reload keeps the pools it has.
func buildPoolConfig(snap model.DHCPConfig) (dhcp.PoolConfig, error) {
	var pc dhcp.PoolConfig
	_, subnet, err := net.ParseCIDR(snap.Subnet)
	if err != nil {
		return pc, fmt.Errorf("parsing subnet %q: %w", snap.Subnet, err)
	}
	pc.Subnet = *subnet
	pc.RangeStart = net.ParseIP(snap.EphemeralPoolStart)
	pc.RangeEnd = net.ParseIP(snap.EphemeralPoolEnd)
	pc.Gateway = net.ParseIP(snap.Gateway)
	pc.Promiscuous = snap.Promiscuous
	if pc.RangeStart == nil || pc.RangeEnd == nil || pc.Gateway == nil {
		return pc, fmt.Errorf("snapshot DHCP config has invalid IPs: start=%q end=%q gw=%q",
			snap.EphemeralPoolStart, snap.EphemeralPoolEnd, snap.Gateway)
	}

	// Relay pools: subnets served via dhcp-helper on leaf bridges
	// (giaddr-selected). Global validated these at site-config write
	// time; re-parse defensively and reject the whole pool set rather
	// than serving a half-configured one.
	for _, rp := range snap.RelayPools {
		_, rpNet, err := net.ParseCIDR(rp.Subnet)
		if err != nil {
			return pc, fmt.Errorf("relay pool subnet %q: %w", rp.Subnet, err)
		}
		start, end, gw := net.ParseIP(rp.RangeStart), net.ParseIP(rp.RangeEnd), net.ParseIP(rp.Gateway)
		if start == nil || end == nil || gw == nil {
			return pc, fmt.Errorf("relay pool %s has invalid IPs: start=%q end=%q gw=%q",
				rp.Subnet, rp.RangeStart, rp.RangeEnd, rp.Gateway)
		}
		pc.RelaySubnets = append(pc.RelaySubnets, dhcp.RelaySubnet{
			Subnet:     *rpNet,
			RangeStart: start,
			RangeEnd:   end,
			Gateway:    gw,
		})
	}
	return pc, nil
}

// fabricPoolFix names where the values buildPoolConfig rejects come from.
const fabricPoolFix = "Correct the gateway_ip custom field on the site's slagworks-production prefix in NetBox, " +
	"or the site config's ephemeral or relay pools."

// fabricPoolReason is the readiness reason while the fabric DHCP leg
// serves no pool because the snapshot's DHCP config did not parse.
func fabricPoolReason(err error) string {
	return fmt.Sprintf("fabric DHCP is serving no pool: %v. %s", err, fabricPoolFix)
}

// reloadFabricPools is the snapshot-applied hot reload of the fabric
// DHCP pools, so site-config edits (relay pools, pool ranges,
// promiscuous) take effect without a service restart. A snapshot with
// no or invalid DHCP config leaves the handler's pools as they are:
// losing config mid-flight should degrade to stale-but-working, not to
// serving nothing. Installing pools clears the fabricPoolErr a start on
// an invalid config left; a further invalid config refreshes its reason.
func reloadFabricPools(sc *siteServiceConfig, h *dhcp.Handler) {
	snap := sc.cache.GetDHCPConfig()
	if snap.Subnet == "" {
		return
	}
	pc, err := buildPoolConfig(snap)
	if err != nil {
		if old := sc.fabricPoolErr.Load(); old != nil {
			reason := fabricPoolReason(err)
			sc.fabricPoolErr.CompareAndSwap(old, &reason)
		}
		sc.log.Error("snapshot DHCP config invalid; fabric DHCP pools unchanged. "+fabricPoolFix, "err", err)
		return
	}
	h.UpdatePools(pc)
	sc.fabricPoolErr.Store(nil)
}

// buildDHCPConfig constructs the DHCP handler configuration from the
// snapshot's DHCPConfig. Promiscuous state rides inside
// Config.PoolConfig (hot-reloadable; see buildPoolConfig).
//
// The snapshot carries one DHCP config per site — subnet/gateway from
// the NetBox "slagworks-production" prefix, ephemeral pool range +
// relay pools from site_config.
func buildDHCPConfig(sc *siteServiceConfig, serverIP net.IP) (dhcp.Config, error) {
	log := sc.log

	dhcpCfg := dhcp.Config{
		ServerIP:      serverIP,
		DNS:           []net.IP{net.ParseIP("1.1.1.1")},
		LeaseDuration: 5 * time.Minute,
		HTTPBootPort:  sc.cfg.httpPort,
		Metrics:       sc.dhcpMetrics,
	}

	// DNS + lease from the snapshot (slagworks-production prefix custom
	// fields dns_servers / dhcp_lease_seconds) override the defaults above.
	// Unlike pools these are Config-level (not PoolConfig), so a CF edit
	// takes effect on worker restart, not on snapshot hot-reload — same
	// lifetime as ServerIP. (The OOB segment's DNS/lease DO hot-reload,
	// via SetIfaceParams.)
	//
	// dns_servers is free text global splits on commas without parsing,
	// so an entry that is not an IP is skipped with an error rather than
	// failing the start (see buildPoolConfig for why that would loop).
	if snapCfg := sc.cache.GetDHCPConfig(); len(snapCfg.DNS) > 0 || snapCfg.LeaseSeconds > 0 {
		if len(snapCfg.DNS) > 0 {
			var dns []net.IP
			for _, s := range snapCfg.DNS {
				ip := net.ParseIP(s)
				if ip == nil {
					log.Error("snapshot DHCP dns_servers entry is not an IP address; not handing it out. "+
						"Correct the dns_servers custom field (comma-separated IPs) on the site's "+
						"slagworks-production prefix in NetBox, then restart the worker: "+
						"fabric DNS servers are read only at start",
						"value", s)
					continue
				}
				dns = append(dns, ip)
			}
			if len(dns) > 0 {
				dhcpCfg.DNS = dns
			} else {
				log.Error("no snapshot DHCP dns_servers entry is an IP address; handing out the default DNS server",
					"dns", dhcpCfg.DNS)
			}
		}
		if snapCfg.LeaseSeconds > 0 {
			dhcpCfg.LeaseDuration = time.Duration(snapCfg.LeaseSeconds) * time.Second
		}
	}

	// Everything process-bound (lease store, ARP probe socket, cache
	// callbacks) is wired UNCONDITIONALLY — even when the cache is cold
	// and the pool config is empty. Pools can arrive later via the
	// snapshot-applied hot-reload (UpdatePools), and a handler whose
	// pools went live with nil callbacks would serve unfiltered leases
	// with no reservations, no ARP probing, and no phase reporting.
	if sc.localDB != nil {
		dhcpCfg.LeaseStore = sc.localDB.NewLeaseStore()
	}

	iface, err := net.InterfaceByName(sc.cfg.iface)
	if err != nil {
		return dhcp.Config{}, fmt.Errorf("get interface %s: %w", sc.cfg.iface, err)
	}
	dhcpCfg.ARPProbe = dhcp.NewARPProber(iface, 500*time.Millisecond, log)

	// Always wire the filter; the handler consults it only when the
	// CURRENT pool state is non-promiscuous, so a snapshot flipping the
	// flag at runtime works in both directions.
	dhcpCfg.KnownMACs = func(mac string) bool {
		return sc.cache.MachineByMAC(mac) != nil
	}

	sc.fabricPoolErr.Store(nil)
	snap := sc.cache.GetDHCPConfig()
	if snap.Subnet == "" {
		log.Info("snapshot has no DHCP config, DHCP will not serve leases until one arrives")
	} else if pc, err := buildPoolConfig(snap); err != nil {
		// Same outcome as a cold cache: the handler starts with no pool
		// and reloadFabricPools installs one from the first snapshot that
		// parses. Everything else (OOB, TFTP, HTTP boot, agent endpoints,
		// the executor) starts as usual.
		reason := fabricPoolReason(err)
		sc.fabricPoolErr.Store(&reason)
		log.Error("snapshot DHCP config invalid; starting without the fabric DHCP pool, which serves no leases "+
			"until a snapshot with a valid config arrives. "+fabricPoolFix, "err", err)
	} else {
		dhcpCfg.PoolConfig = pc

		log.Info("DHCP configured from snapshot",
			"subnet", snap.Subnet,
			"pool", snap.EphemeralPoolStart+"-"+snap.EphemeralPoolEnd,
			"gateway", snap.Gateway,
			"relay_pools", len(snap.RelayPools),
			"promiscuous", snap.Promiscuous,
		)
		if snap.Promiscuous {
			log.Warn("promiscuous DHCP enabled for site")
		}
	}

	dhcpCfg.ReserveIP = sc.cache.ReservedIPForMAC

	// Stamp machine_id on every DHCP log envelope when the
	// MAC resolves, so per-machine log views surface DHCP traffic
	// during PXE.
	dhcpCfg.MachineIDForMAC = func(mac string) string {
		m := sc.cache.MachineByMAC(mac)
		if m == nil {
			return ""
		}
		return m.ID.String()
	}

	// BootEligible gates whether a reply carries the PXE bootfile/next-
	// server. It is a NEGATIVE gate: deny the bootfile ONLY to a MAC that is
	// a known OOB device (has an OOB reservation) but is NOT a slagworks
	// machine — i.e. a BMC, PDU, or switch-management port. Everything else
	// boots. This is segment-agnostic and matters only on the promiscuous
	// OOB segment:
	//   - a slagworks machine (kworker on fabric, leaf-6 on OOB), deploying
	//     OR running, gets the bootfile -> chains to /boot/ipxe -> httpboot
	//     routes it to the deploy script or bootLocalScript. Preserves the
	//     steady-state reboot path (a running host must still get a bootfile
	//     to reach bootlocal; dropping it triggers a PXE-E16 loop on
	//     Supermicro, see the dhcp handler).
	//   - a brand-new UNKNOWN MAC (no reservation, no machine) still gets a
	//     bootfile — the httpboot layer decides whether to serve a boot script
	//     or 404 based on the operator-owned machine registration flow.
	//   - an idle BMC/PDU/switch-mgmt port (reserved, not a machine) gets its
	//     reserved address but NO bootfile — the blast-radius control that
	//     stops a boot URL going to every idle BMC on the OOB segment.
	dhcpCfg.BootEligible = func(mac string) bool {
		return !sc.cache.IsOOBManagedNonMachine(mac)
	}

	// DHCP phase reporting for booting machines (deploy or commission, which
	// share the boot-prefix spine). Firmware DHCP → "dhcp_ack", iPXE DHCP
	// (after chainload) → "ipxe_dhcp". Gating on the active booting op rather
	// than machine state covers both deploying and commissioning hosts so the
	// commission watch TUI animates these phases like deploy does.
	dhcpCfg.OnACK = func(mac string, isIPXE bool) {
		m := sc.cache.MachineByMAC(mac)
		if m == nil {
			return
		}
		op := sc.cache.ActiveBootingOpForMachine(m.ID)
		if op == nil {
			return
		}
		phase := "dhcp_ack"
		if isIPXE {
			phase = "ipxe_dhcp"
		}
		sc.cache.UpdateOpPhase(op.ID.String(), phase)
	}

	// Lease-history ledger (Phase 3): every lease mutation flows to
	// global as a dhcp_lease observation via the outbox, debounced so
	// a short-lease OOB segment doesn't write a ledger row per renew
	// tick. leaseDuration is read AFTER the snapshot-derived override
	// above, so the debounce window matches whatever lease duration is
	// actually in effect for this segment.
	dhcpCfg.OnLeaseEvent = worker.NewDHCPLeaseForwarder(sc.cache, dhcpCfg.LeaseDuration).OnLeaseEvent

	return dhcpCfg, nil
}

func startSiteServices(ctx context.Context, sc *siteServiceConfig) (<-chan struct{}, error) {
	cfg := sc.cfg
	log := sc.log

	serverIP := net.ParseIP(cfg.serverIP)

	dhcpCfg, err := buildDHCPConfig(sc, serverIP)
	if err != nil {
		return nil, fmt.Errorf("building DHCP config: %w", err)
	}

	// WaitGroup tracks every long-lived goroutine (DHCP/TFTP/HTTP servers,
	// the handler-clear watcher, hot-reload watchers, CastManager). Each
	// site service does its own wg.Add(1) at spawn — the fabric leg, the
	// optional OOB leg, and the per-server TFTP/HTTP closures — so the count
	// stays correct regardless of how many interfaces are bound.
	var wg sync.WaitGroup
	dhcpHandler := dhcp.NewHandler(dhcpCfg)
	dhcpHandler.SetLogger(log)
	// Expose the live handler to the scrape-time lease collector;
	// cleared on teardown so a standby period reports no leases
	// instead of a stale handler's. The missing-pool readiness reason
	// describes this handler, so it goes with it.
	sc.dhcpHandler.Store(dhcpHandler)
	wg.Go(func() {
		<-ctx.Done()
		sc.dhcpHandler.CompareAndSwap(dhcpHandler, nil)
		sc.fabricPoolErr.Store(nil)
	})
	dhcpServer := dhcp.NewServer(cfg.iface, dhcpHandler)
	wg.Go(func() {
		if err := dhcpServer.ListenAndServe(ctx); err != nil {
			log.Error("DHCP server error", "err", err)
		}
	})

	// Wire the dhcp_leases RPC source through sc.dhcpHandler (the atomic
	// pointer above), NOT a closure directly over dhcpHandler. On a
	// standby VIP release the WS connection stays up while services
	// stop — closing over dhcpHandler directly would keep answering
	// fan-out RPCs with that handler's now-frozen lease table forever
	// (roast finding, 2026-07-13). Reading through the pointer that's
	// already cleared to nil on teardown (same rationale as the
	// scrape-time lease collector above) makes a stopped worker
	// correctly report "no DHCP handler configured" instead of stale
	// leases.
	sc.wsConn.SetDHCPLeaseSource(func() []dhcp.LeaseInfo {
		h := sc.dhcpHandler.Load()
		if h == nil {
			return nil
		}
		return h.Leases()
	})

	// Hot-reload DHCP pool state on every snapshot apply. The WS client is
	// already applying snapshots, and one applied after buildDHCPConfig
	// read the cache but before this subscription has no callback, so
	// reconcile once after subscribing: a worker that started without a
	// pool would otherwise wait for the snapshot after the corrected one.
	unsubPools := sc.cache.OnSnapshotApplied(func() { reloadFabricPools(sc, dhcpHandler) })
	reloadFabricPools(sc, dhcpHandler)
	wg.Go(func() {
		<-ctx.Done()
		unsubPools()
	})

	// machineForIP resolves a client IP to its MAC + machine UUID +
	// hostname so TFTP and HTTP boot log envelopes can be scoped to
	// the machine making the fetch. Two hops: dhcp lease
	// table maps IP→MAC, cache maps MAC→machine. Either lookup
	// missing → empty strings (acceptable; the bug is "we know who
	// it is and still didn't tag the envelope").
	machineForIP := func(ip string) (mac, machineID, hostname string) {
		mac = dhcpHandler.MACForIP(ip)
		if mac == "" {
			return "", "", ""
		}
		m := sc.cache.MachineByMAC(mac)
		if m == nil {
			return mac, "", ""
		}
		return mac, m.ID.String(), m.Hostname
	}

	// startTFTP launches a TFTP listener bound to bindIP:69. Called once
	// per served segment (the fabric serverIP, and the OOB serverIP when
	// the OOB interface is bound) — each interface's clients fetch from the
	// serverIP that interface's DHCP offer advertised, so each serverIP
	// needs its own listener. The KnownIPs gate and MachineForIP are shared
	// (lease state is one table across all interfaces).
	startTFTP := func(bindIP string) {
		tftpCfg := tftp.Config{
			BindAddr:     fmt.Sprintf("%s:%d", bindIP, 69),
			ServeDir:     cfg.tftpDir,
			MachineForIP: machineForIP,
			OnFetch: func(outcome string) {
				sc.metrics.TFTPFetchesTotal.WithLabelValues(outcome).Inc()
			},
			Logger: log,
		}
		// Lease-gate TFTP unless ANY segment is currently promiscuous —
		// consulted per request so a runtime promiscuous flip (snapshot
		// apply) takes effect without a restart.
		tftpCfg.KnownIPs = func(ip string) bool {
			return dhcpHandler.AnyPromiscuous() || dhcpHandler.HasLease(ip)
		}
		tftpServer := tftp.NewServer(tftpCfg)
		wg.Go(func() {
			if err := tftpServer.ListenAndServe(ctx); err != nil {
				log.Error("TFTP server error", "bind", bindIP, "err", err)
			}
		})
	}
	startTFTP(cfg.serverIP)

	// CastManager owns one persistent console session per active machine
	// (ipmitool sol activate under a pty for a BMC, AMT redirection for an
	// Intel AMT machine), streaming asciicast frames to global's
	// console_cast_events (the slagctl console pipeline).
	// Reconciles against the cache on every snapshot apply. Without
	// ipmitool on PATH only AMT machines get pumps (they need none).
	// BMC-less machines on a console-server tty are pumped by the
	// process-lifetime serial manager in runSite instead.
	{
		dialer := &worker.TransportCastDialer{
			Cache: sc.cache,
			AMT:   &worker.AMTSOLDialer{Cache: sc.cache, Logger: log},
		}
		var lister worker.CastMachineLister = sc.cache
		if _, err := exec.LookPath("ipmitool"); err != nil {
			log.Warn("ipmitool not found on PATH; SOL console streaming disabled for BMC machines (AMT machines unaffected)", "err", err)
			lister = worker.AMTOnlyCastLister{Cache: sc.cache}
		} else {
			dialer.IPMI = &worker.IPMIToolPTYDialer{Cache: sc.cache, Logger: log}
		}
		castMgr := worker.NewCastManager(worker.CastManagerConfig{
			Dialer: dialer,
			Sink:   sc.wsConn, // frames ride the relay to global
			Resume: sc.wsConn, // resume-offset RPC to global
			Cache:  lister,
			Log:    log,
		})
		castMgr.Start(ctx)
		// Wire snapshot-applied → Reconcile so newly added / removed
		// machines bring up / tear down pumps without polling.
		unsubCast := sc.cache.OnSnapshotApplied(castMgr.Reconcile)
		// Initial reconcile in case a snapshot is already loaded (cold
		// start from local DB before first WS sync).
		castMgr.Reconcile()
		// Tie shutdown into the WaitGroup so the site-service done
		// channel doesn't close until every ipmitool process group has
		// been killed and reaped — orphaned children hold the BMC's
		// single SOL slot. Also clear the snapshot-applied callback so a
		// late ApplySnapshot (WSConn lingering in standby after VIP
		// loss) doesn't drive Reconcile on a cancelled manager.
		wg.Go(func() {
			<-ctx.Done()
			unsubCast()
			castMgr.Shutdown()
		})
	}

	// startHTTPBoot launches an HTTP-boot listener bound to bindIP:httpPort.
	// Like TFTP, one per served segment: an iPXE client fetches /boot/ipxe
	// and the iPXE binary from the serverIP its DHCP offer named, so the
	// OOB serverIP needs its own HTTP listener in addition to the fabric's.
	// Unknown MACs are dropped at the DHCP layer (or, on a promiscuous OOB
	// segment, served an address but no bootfile); enrollment is
	// NetBox-driven (operator creates device → webhook → mirror → snapshot).
	startHTTPBoot := func(bindIP string) {
		serverCfg := httpboot.ServerConfig{
			ImagesDir: cfg.imagesDir,
			// IPXEDir = the TFTP serve dir; the iPXE binaries (snponly.efi,
			// undionly.kpxe) are served over HTTP too, for UEFI HTTP-boot
			// firmware (DHCP hands HTTP-boot clients /files/ipxe/<name>).
			IPXEDir:      cfg.tftpDir,
			DefaultImage: cfg.defaultImage,
			HTTPPort:     cfg.httpPort,
			DeployMode:   true,
			Provider:     sc.cache,
			// LogSink receives envelopes posted by deploy / commission
			// agents to /logs/ingest. The ring is the same buffer the
			// site's slog handler and SOL pump push directly into; the
			// WS forwarder drains all three sources to global.
			LogSink: sc.logRing,
			// /files/bin/slagworks-agent serves the binary placed by the
			// slagworks .deb at apt-install time on the gateway host.
			// SLAGWORKS_AGENT_BIN overrides for test rigs.
			AgentBinPath: func() string { return envutil.Or("SLAGWORKS_AGENT_BIN", defaultAgentBinPath) },

			// Scope HTTP boot fetch envelopes to the requesting
			// machine when the source IP resolves via active DHCP lease.
			MachineForIP: machineForIP,

			OnRequest: func(route, status string) {
				sc.metrics.HTTPBootFetchesTotal.WithLabelValues(route, status).Inc()
			},
		}
		httpServer := httpboot.NewServer(
			fmt.Sprintf("%s:%d", bindIP, cfg.httpPort),
			serverCfg,
		)
		httpServer.SetLogger(log)
		wg.Go(func() {
			if err := httpServer.ListenAndServe(ctx); err != nil {
				log.Error("HTTP server error", "bind", bindIP, "err", err)
			}
		})
	}
	startHTTPBoot(cfg.serverIP)

	// OOB segment bind: when SLAGWORKS_OOB_DHCP_ENABLED and the snapshot
	// carries an OOB config AND this host has a local interface on the OOB
	// subnet, slagworks ALSO serves DHCP/TFTP/HTTP on that interface —
	// taking over the OOB DHCP role from dnsmasq. Derived entirely from
	// NetBox + the host's own interfaces; the only host-local input is the
	// enable knob (the cutover safety valve). See
	// docs/user/configuration.md ("OOB DHCP takeover").
	//
	// A DHCP conflict probe runs immediately before OOB DHCP binds (UDP/67).
	// If a foreign OOB DHCP server is detected (or the probe errors), OOB
	// DHCP is latched off for the process lifetime but OOB TFTP/HTTP, the
	// fabric leg, health, and metrics all stay up — decoupling what used to
	// be an all-or-nothing OOB DHCP/TFTP/HTTP bundle.
	//
	// bindOOB performs the one-shot OOB bind. It returns true once it has
	// either bound the OOB segment OR determined it definitively never will
	// for this process (collision, no local OOB interface, bad config, or a
	// latched conflict) — in both cases there's no point retrying. It
	// returns false ONLY when the OOB config simply isn't present yet (cold
	// start before first WS sync), so the caller knows to retry on the next
	// snapshot. Idempotent binding is guaranteed by the caller invoking it
	// at most once-to-success.
	bindOOB := func() (settled bool) {
		oob := sc.cache.GetOOBConfig()
		if oob.Subnet == "" {
			// Not synced yet — retry when a snapshot with OOB config lands.
			return false
		}
		locals, err := listLocalIfaces()
		if err != nil {
			log.Error("OOB DHCP enabled but cannot list local interfaces; not binding OOB", "err", err)
			return true
		}
		bind, ok, err := deriveOOBBind(oob.Subnet, locals)
		if err != nil {
			log.Error("OOB DHCP: bad OOB subnet; not binding OOB", "subnet", oob.Subnet, "err", err)
			return true
		}
		if !ok {
			log.Info("OOB DHCP enabled but no local interface is on the OOB subnet; not binding OOB",
				"subnet", oob.Subnet)
			return true
		}
		// Collision guard: the OOB segment must be a DIFFERENT interface AND
		// serverIP than the fabric leg. If they coincide we cannot serve two
		// distinct segment configs on one interface — SetIfaceParams on the
		// fabric iface would override the base (fabric) config with the OOB
		// (promiscuous, OOB pool) one, corrupting the fabric segment, and a
		// second TFTP/HTTP listener on the same serverIP would fail to bind.
		// In the real topology these never coincide (fabric=leaf4 P2P leg,
		// OOB=eno3); this guards against a misconfiguration rather than a
		// normal case. Refuse the OOB bind and keep the fabric leg intact.
		if oobCollidesWithFabric(bind, cfg.iface, cfg.serverIP) {
			log.Error("OOB bind collides with the fabric leg (same iface or serverIP); refusing OOB bind to protect the fabric segment",
				"oob_iface", bind.iface, "oob_server_ip", bind.serverIP.String(),
				"fabric_iface", cfg.iface, "fabric_server_ip", cfg.serverIP)
			return true
		}
		params, ok, err := oobIfaceParams(oob, bind.serverIP, bind.iface, 500*time.Millisecond, log)
		if err != nil {
			log.Error("OOB DHCP: bad OOB config; not binding OOB", "err", err)
			return true
		}
		if !ok {
			log.Info("OOB DHCP: incomplete OOB config (no subnet/gateway); not binding OOB")
			return true
		}
		_, subnet, err := net.ParseCIDR(oob.Subnet)
		if err != nil {
			log.Error("OOB DHCP: bad OOB subnet; not binding OOB", "subnet", oob.Subnet, "err", err)
			return true
		}

		// startDHCP brings up the OOB DHCP server (SetIfaceParams + the
		// UDP/67 listener + snapshot hot-reload). Only invoked when the
		// conflict probe is clean.
		startDHCP := func() {
			dhcpHandler.SetIfaceParams(bind.iface, params)
			mySlice := "none"
			if params.RangeStart != nil {
				mySlice = params.RangeStart.String() + "-" + params.RangeEnd.String()
			}
			log.Info("OOB DHCP binding",
				"iface", bind.iface, "server_ip", bind.serverIP.String(),
				"subnet", oob.Subnet, "gateway", oob.Gateway,
				"my_slice", mySlice, "total_slices", len(oob.DynamicSlices),
				"lease_seconds", oob.LeaseSeconds, "dns", oob.DNS, "ntp", oob.NTP)

			oobDHCP := dhcp.NewServer(bind.iface, dhcpHandler)
			wg.Go(func() {
				if err := oobDHCP.ListenAndServe(ctx); err != nil {
					log.Error("OOB DHCP server error", "iface", bind.iface, "err", err)
				}
			})

			// Hot-reload the OOB IfaceParams on snapshot apply so OOB pool /
			// gateway / DNS / NTP edits take effect without a restart,
			// mirroring the fabric pool hot-reload above. The bind interface
			// + serverIP don't change at runtime (they're host-local), so
			// only the params are refreshed.
			unsubOOB := sc.cache.OnSnapshotApplied(func() {
				oob := sc.cache.GetOOBConfig()
				if oob.Subnet == "" {
					return
				}
				p, ok, err := oobIfaceParams(oob, bind.serverIP, bind.iface, 500*time.Millisecond, log)
				if err != nil || !ok {
					log.Error("OOB DHCP config invalid on reload; keeping previous OOB params", "err", err)
					return
				}
				dhcpHandler.SetIfaceParams(bind.iface, p)
			})
			wg.Go(func() {
				<-ctx.Done()
				unsubOOB()
			})
		}

		// startAssist brings up OOB TFTP/HTTP regardless of the DHCP probe
		// outcome, so a latched conflict (or error) suppresses only OOB
		// DHCP, never the OOB TFTP/HTTP takeover.
		startAssist := func() {
			startTFTP(bind.serverIP.String())
			startHTTPBoot(bind.serverIP.String())
		}

		// Production conflict probe: bound to the OOB interface (SO_BIND-
		// TODEVICE / IP_BOUND_IF) so DISCOVER egress and OFFER ingress stay
		// on the OOB L2, independent of the host's default route.
		probe := func(bind oobBind, allowed map[string]struct{}) bool {
			ni, err := net.InterfaceByName(bind.iface)
			if err != nil {
				sc.oobConflict.record(probeError, nil, fmt.Errorf("resolve OOB interface %s: %w", bind.iface, err))
				return false
			}
			sink, err := newEthOOBOfferSink(ni, subnet, oobProbeTimeout, log)
			if err != nil {
				sc.oobConflict.record(probeError, nil, err)
				return false
			}
			defer func() { _ = sink.Close() }()
			return runOOBProbe(sc.oobConflict, sink, allowed, log)
		}

		bindOOBSegment(sc.oobConflict, probe, oob, bind, startDHCP, startAssist, log)
		return true
	}

	// Drive the OOB bind, gated on the enable knob. Try immediately; if the
	// OOB config isn't synced yet (cold start before first WS sync), retry
	// on each snapshot apply until bindOOB settles (binds, or determines it
	// never will). Without this retry, a worker that starts before its first
	// snapshot would skip OOB binding permanently until a restart — exactly
	// the cutover footgun (deploy with the knob on, OOB silently never binds).
	if cfg.oobDHCPEnabled && !bindOOB() {
		// Config not synced yet: retry on each snapshot apply until bindOOB
		// settles. OnSnapshotApplied callbacks run OUTSIDE the cache lock
		// (cache.go), so calling the unsubscribe from within the callback is
		// lock-safe; sync.Once makes unsubscribe idempotent across the
		// callback path and the ctx-cancel cleanup path.
		var once sync.Once
		var unsubRetry func()
		stop := func() {
			once.Do(func() {
				if unsubRetry != nil {
					unsubRetry()
				}
			})
		}
		unsubRetry = sc.cache.OnSnapshotApplied(func() {
			if bindOOB() {
				stop()
			}
		})
		wg.Go(func() {
			<-ctx.Done()
			stop()
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done, nil
}

// runSiteStandby starts the VIP-watch loop and returns a channel that
// closes once the loop has fully torn down any services it started
// (including waiting for CastManager's ipmitool children to be
// reaped). The caller must wait on it before exiting the process.
func runSiteStandby(ctx context.Context, sc *siteServiceConfig) (<-chan struct{}, error) {
	mon, err := worker.NewVIPMonitor(sc.cfg.vip, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("creating VIP monitor: %w", err)
	}
	mon.SetLogger(sc.log)
	vipCh := mon.Run(ctx)

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		var svcCancel context.CancelFunc
		var svcDone <-chan struct{}
		for {
			select {
			case <-ctx.Done():
				// Same reap-before-exit contract as active mode: wait
				// for the service done channel so ipmitool SOL children
				// are killed and reaped, not orphaned.
				if svcCancel != nil {
					svcCancel()
					if svcDone != nil {
						<-svcDone
					}
				}
				return
			case acquired, ok := <-vipCh:
				if !ok {
					return
				}
				if acquired {
					sc.log.Info("VIP acquired, activating services and executor")
					svcCtx, cancel := context.WithCancel(ctx)
					svcCancel = cancel
					done, err := startSiteServices(svcCtx, sc)
					if err != nil {
						// The VIP is keepalived's, not ours — we cannot
						// release it, and vipCh won't fire again while we
						// keep holding it. Staying up would blackhole the
						// site (VIP attracts traffic, no services behind
						// it). Exit 1 so systemd restarts us for a clean
						// retry; repeated failures are visible as unit
						// restarts instead of a silent dead site.
						sc.log.Error("failed to start site services after VIP acquired; exiting for systemd restart", "err", err)
						cancel()
						os.Exit(1)
					}
					svcDone = done
					executor := worker.NewExecutor(worker.ExecutorConfig{
						Cache:    sc.cache,
						Factory:  newPowerControllerFactory(sc.cfg.smcTool, sc.log),
						Claimer:  sc.wsConn,
						WorkerID: sc.cfg.workerID,
						Metrics:  sc.metrics,
					})
					executor.SetLogger(sc.log)
					go func() {
						if err := executor.Run(svcCtx); err != nil {
							sc.log.Error("executor terminated with error", "err", err)
						}
					}()
				} else {
					sc.log.Info("VIP released, deactivating services")
					if svcCancel != nil {
						svcCancel()
						svcCancel = nil
						if svcDone != nil {
							<-svcDone
							svcDone = nil
						}
						sc.log.Info("services stopped, ready for reactivation")
					}
				}
			}
		}
	}()
	return loopDone, nil
}

func monitorVIPActive(ctx context.Context, vip string, log *slog.Logger) {
	mon, err := worker.NewVIPMonitor(vip, 2*time.Second)
	if err != nil {
		log.Error("creating VIP monitor", "err", err)
		return
	}
	mon.SetLogger(log)
	vipCh := mon.Run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case acquired, ok := <-vipCh:
			if !ok {
				return
			}
			if !acquired {
				log.Warn("VIP lost while in active mode", "vip", vip)
			}
		}
	}
}

// newPowerControllerFactory returns the site worker's
// worker.PowerControllerFactory, closing over the operator's SMCIPMITool
// configuration.
func newPowerControllerFactory(tool smcipmitool.Tool, log *slog.Logger) worker.PowerControllerFactory {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
		return buildPowerController(ctx, m, rt, pduRT, amtRT, tool, log)
	}
}

func buildPowerController(ctx context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime, tool smcipmitool.Tool, log *slog.Logger) (power.Controller, error) {
	// amt_runtime (explicit --via amt, or global auto-selected amt for a
	// kind=amt machine) routes to Intel AMT over WS-Man/TLS. Host and creds
	// come only from the envelope: global resolved them from NetBox oob_ip
	// + the kind=amt admin entry, so there is no snapshot fallback.
	if amtRT != nil {
		if pduRT != nil {
			return nil, fmt.Errorf("machine %s: op carries both pdu_runtime and amt_runtime", m.ID)
		}
		return power.NewAMTController(ctx, amtRT.Host, amtRT.Username, amtRT.Password, 30*time.Second, nil)
	}
	// Backstop for plan D2: an AMT endpoint is never driven as IPMI/Redfish.
	if pduRT == nil && m.AMT {
		return nil, fmt.Errorf("machine %s's OOB endpoint is Intel AMT but no amt_runtime was supplied; refusing to drive it as IPMI/Redfish", m.ID)
	}
	// Explicit transport selection: a pdu_runtime envelope (operator
	// passed `--via pdu`) routes to a PDU controller over its outlets.
	// An empty BMCIP does not implicitly select PDU: the operator must request
	// --via pdu so missing BMC metadata fails instead of silently changing
	// transports. See docs/dev/pdu-power-control.md.
	if pduRT != nil {
		specs := worker.PDUSpecs(pduRT)
		if len(specs) == 0 {
			return nil, fmt.Errorf("machine %s: pdu_runtime present but carries no outlets", m.ID)
		}
		return power.NewPDUController(ctx, specs, nil)
	}

	if m.BMCIP == "" {
		return nil, fmt.Errorf("machine %s has no BMC IP configured", m.ID)
	}
	// Cred selection: both sources carry the device's bmc-creds-admin
	// entry (the only BMC credential). The op envelope (rt) wins; the
	// snapshot's m.BMCUser/m.BMCPass covers envelopes without creds
	// (global running without NetBox, as in dev/test, resolves none).
	// Half-creds (one field set, the other empty) are never used — they
	// would only auth-fail.
	user, pass := rt.Username, rt.Password
	if user == "" || pass == "" {
		if m.BMCUser != "" && m.BMCPass != "" {
			user, pass = m.BMCUser, m.BMCPass
		} else {
			user, pass = "", ""
		}
	}
	if user == "" {
		// Last-ditch default — bmclib needs SOMETHING. Logged at
		// debug only; the op will likely fail at auth time.
		user = "admin"
	}

	// Vendor-first dispatch: a Supermicro host routes power/boot/serial
	// through SMCIPMITool when the operator has installed it. The vendor
	// tool is more reliable against Supermicro's AMI-based BMCs than raw
	// IPMI, which runs out of session slots and wedges under load.
	//
	// Slagworks can't redistribute the tool, so in auto mode a missing tool
	// falls back to bmclib; the worker warns on snapshot apply and status
	// reads carry a hint. In required mode a missing tool is a hard error,
	// so a fleet that depends on the vendor path can't silently degrade
	// onto raw IPMI.
	//
	// The BMC-IP "host:port" suffix (qemutest fake BMC) is bmclib-specific,
	// so strip any port before handing the bare host to the vendor tool.
	if m.IsSupermicro() && tool.Mode != smcipmitool.ModeOff {
		bin, ok := tool.Lookup()
		switch {
		case ok:
			smcHost := m.BMCIP
			if h, _, err := net.SplitHostPort(m.BMCIP); err == nil {
				smcHost = h
			}
			// Timeout 0 takes smcipmitool.DefaultTimeout.
			ctrl := power.NewSMCController(smcHost, user, pass, 0, log)
			ctrl.SetBin(bin)
			return ctrl, nil
		case tool.Mode == smcipmitool.ModeRequired:
			return nil, fmt.Errorf("machine %s is Supermicro and SLAGWORKS_SMCIPMITOOL=required, "+
				"but SMCIPMITool (%s) is not installed on this worker; see %s",
				m.ID, tool.BinName(), smcipmitool.InstallDoc)
		default:
			log.Debug("Supermicro host using the generic BMC path: SMCIPMITool not installed",
				"machine_id", m.ID, "bin", tool.BinName())
		}
	}

	host, opts := power.BuildBMCDialOptions(m.BMCIP, rt.IpmiCipher())
	ctrl, err := power.NewBMCController(ctx, host, user, pass, 30*time.Second, nil, opts...)
	if err != nil {
		return nil, err
	}
	// Pass the per-host cipher through for the ipmitool FRU fallback in
	// ChassisSerial (Redfish-less iDRACs — bmclib's ipmitool provider can't
	// do Inventory). 0 = ipmitool default.
	ctrl.SetIPMICipher(rt.IpmiCipher())
	return ctrl, nil
}

// newSMCToolWarner returns a snapshot-applied callback that tells the
// operator when the site has Supermicro hosts but SMCIPMITool isn't
// installed on this worker. It logs on the first snapshot and then only
// when the Supermicro count or the tool status changes, so a busy site's
// snapshot churn doesn't repeat it.
func newSMCToolWarner(tool smcipmitool.Tool, supermicroCount func() int, log *slog.Logger) func() {
	var mu sync.Mutex
	lastCount, lastStatus := -1, ""
	return func() {
		n := supermicroCount()
		status := tool.Status()
		mu.Lock()
		changed := n != lastCount || status != lastStatus
		lastCount, lastStatus = n, status
		mu.Unlock()
		if !changed || n == 0 {
			return
		}
		attrs := []any{"supermicro_machines", n, "bin", tool.BinName(), "install_doc", smcipmitool.InstallDoc}
		switch status {
		case smcipmitool.StatusInstalled:
			log.Info("SMCIPMITool found; Supermicro hosts will use it", attrs...)
		case smcipmitool.StatusMissingRequired:
			log.Error("site has Supermicro hosts but SMCIPMITool is not installed; "+
				"power operations on them will fail until it is (SLAGWORKS_SMCIPMITOOL=required)", attrs...)
		case smcipmitool.StatusMissing:
			log.Warn("site has Supermicro hosts but SMCIPMITool is not installed; "+
				"they will use the generic IPMI path, which is less reliable on Supermicro BMCs", attrs...)
		}
	}
}
