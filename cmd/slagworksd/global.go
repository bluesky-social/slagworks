package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/commission"
	"github.com/bluesky-social/slagworks/internal/envutil"
	"github.com/bluesky-social/slagworks/internal/hubhandlers"
	"github.com/bluesky-social/slagworks/internal/logring"
	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
	"github.com/bluesky-social/slagworks/internal/rdsiam"
	"github.com/bluesky-social/slagworks/internal/snapshot"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/tailscale"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// globalConfig captures all the inputs buildGlobal needs. Extracted
// from globalCmd so tests can drive the same wiring without the
// cobra layer.
type globalConfig struct {
	DSN          string
	OIDCIssuer   string
	OIDCJWKSURL  string
	OIDCAudience string
	OIDCGroups   []string
	// OIDCCredentialGroups additionally gates the plaintext device-credential
	// route (api.Config.CredentialGroups). Empty: every operator may read
	// credentials.
	OIDCCredentialGroups []string
	ExternalURL          string
	WebhookSecret        string
	NetBoxURL            string
	NetBoxToken          string
	// TailscaleOAuthClientID/Secret are the slagworks-deploy OAuth client
	// (auth_keys scope, owns tag:pop1/2/3) used to mint per-op tailnet auth
	// keys for deploy-time host enrollment. Sourced from kubes1/slagworks/creds
	// via env. Both empty disables enrollment (deploys proceed without it).
	TailscaleOAuthClientID     string
	TailscaleOAuthClientSecret string
	// HostnameTemplate derives each deployed machine's hostname from its
	// NetBox device name (snapshot.HostnameTemplate). Empty: the hostname is
	// the device name.
	HostnameTemplate string
}

// globalStatementTimeout bounds database statements that do not yet carry
// request contexts. Context propagation remains the preferred per-operation
// cancellation mechanism.
const globalStatementTimeout = 30 * time.Second

// openGlobalDB returns a GORM database backed by a pgx database/sql pool.
// The caller owns the returned pool. Failed setup closes it before returning.
// A DSN with an RDS host and no password logs in with RDS IAM tokens
// (internal/rdsiam); migrations share the pool, so they do too.
func openGlobalDB(ctx context.Context, dsn string, stmtTimeout time.Duration) (*gorm.DB, *sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing database DSN: %w", err)
	}
	// A startup parameter applies to every connection created by the pool.
	cfg.RuntimeParams["statement_timeout"] = stmtTimeout.String()
	var opts []stdlib.OptionOpenDB
	iamLogin, err := rdsiam.Configure(ctx, cfg, nil)
	if err != nil {
		return nil, nil, err
	}
	if iamLogin != nil {
		opts = append(opts, stdlib.OptionBeforeConnect(iamLogin))
	}
	pool := stdlib.OpenDB(*cfg, opts...)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// TranslateError maps driver-specific errors (e.g. Postgres 23505) onto
		// gorm's sentinel errors so store.go's errors.Is(err, ErrDuplicatedKey)
		// checks actually fire — without it, unique-violation handling is dead
		// code and a duplicate surfaces as a raw SQLSTATE string.
		TranslateError: true,
	})
	if err != nil {
		_ = pool.Close()
		return nil, nil, fmt.Errorf("connecting to database: %w", err)
	}
	return db, pool, nil
}

// buildGlobal wires up the production global server: connects to the
// DB, constructs the snapshot/WS/commission pipeline when NetBox is
// configured, and returns the api.Server ready to serve. Cleanup
// goroutines (reconciler, hub-close-on-ctx) are bound to ctx.
// ctx being cancelled will stop all background goroutines; callers
// are responsible for subsequent shutdown of the returned server.
func buildGlobal(ctx context.Context, log *slog.Logger, cfg globalConfig) (*api.Server, error) {
	if cfg.OIDCIssuer == "" || cfg.OIDCAudience == "" {
		return nil, configErrorf("OIDC config is required: set --oidc-issuer (SLAGWORKS_OIDC_ISSUER) and --oidc-audience (SLAGWORKS_OIDC_AUDIENCE)\n  operator auth is OIDC-only (Authentik); see docs/user/configuration.md")
	}
	if len(cfg.OIDCGroups) == 0 {
		return nil, configErrorf("no operator group is configured: set --oidc-required-groups (SLAGWORKS_OIDC_REQUIRED_GROUPS) to the identity-provider group(s) whose members may operate slagworks\n  without it every token would be refused; see docs/user/configuration.md")
	}
	hostnames, err := snapshot.ParseHostnameTemplate(cfg.HostnameTemplate)
	if err != nil {
		return nil, configErrorf("invalid --hostname-template (SLAGWORKS_HOSTNAME_TEMPLATE) %q: %v\n  fix the template or unset it to use NetBox device names as hostnames; see docs/user/configuration.md", cfg.HostnameTemplate, err)
	}

	db, pool, err := openGlobalDB(ctx, cfg.DSN, globalStatementTimeout)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}
	st := store.New(db)

	promReg := prometheus.NewRegistry()
	metrics := snapshot.NewMetrics(promReg)
	opsM := opsmetrics.New(promReg)

	apiCfg := api.Config{
		Store:              st,
		Version:            version,
		OIDCIssuer:         cfg.OIDCIssuer,
		OIDCJWKSURL:        cfg.OIDCJWKSURL,
		OIDCAudience:       cfg.OIDCAudience,
		RequiredGroups:     cfg.OIDCGroups,
		CredentialGroups:   cfg.OIDCCredentialGroups,
		ExternalURL:        cfg.ExternalURL,
		WebhookSecret:      cfg.WebhookSecret,
		PrometheusRegistry: promReg,
	}
	if n := len(cfg.OIDCCredentialGroups); n > 0 {
		log.Info("device credential reads restricted to SLAGWORKS_OIDC_CREDENTIAL_GROUPS", "groups", n)
	} else {
		log.Info("device credential reads open to every operator: SLAGWORKS_OIDC_CREDENTIAL_GROUPS is unset")
	}

	// BMC admin secret reader: per-pop bmc-creds-admin, used by
	// resolveBMCRuntime when stamping op envelopes. Initializing
	// fails when the pod has no IRSA / no AWS creds at all; in that
	// case we leave bmcSecrets nil and resolveBMCRuntime falls back
	// to empty extra_config (the operator gets a warning, not a
	// hard failure, so local-dev runs without AWS still work).
	bmcSecrets, err := awssecrets.NewClient(ctx, awssecrets.Config{})
	if err != nil {
		log.Warn("AWS secrets disabled: cannot load aws config (BMC extra_config will be empty)", "err", err)
	} else {
		apiCfg.BMCSecrets = bmcSecrets
		// Same Client bootstraps the per-pop monitor secret at site-config
		// create (EnsureMonitorSecret). Requires the IRSA CreateSecret grant on
		// pop/*/bmc-creds-monitor-*.
		apiCfg.MonitorBootstrap = bmcSecrets
		log.Info("BMC admin secret reader enabled")
	}

	// Tailscale deploy-enrollment minter: mints a single-use tailnet auth
	// key per deploy op. Enabled only when both OAuth client halves are
	// present; otherwise left nil and deploys proceed without tailnet
	// enrollment (ansible enrolls on converge as before). No AWS dependency
	// here — the creds arrive via env from kubes1/slagworks/creds, same as
	// DATABASE_URL / NETBOX_TOKEN.
	if cfg.TailscaleOAuthClientID != "" && cfg.TailscaleOAuthClientSecret != "" {
		apiCfg.TailscaleMinter = tailscale.NewMinter(
			cfg.TailscaleOAuthClientID, cfg.TailscaleOAuthClientSecret)
		log.Info("tailscale deploy-enrollment minter enabled")
	} else {
		log.Info("tailscale deploy-enrollment disabled (no OAuth client configured)")
	}

	if cfg.NetBoxURL != "" && cfg.NetBoxToken != "" {
		nb := netbox.NewClient(netbox.ClientConfig{
			BaseURL: cfg.NetBoxURL,
			Token:   cfg.NetBoxToken,
			// Escape hatch for NetBox upgrades: the Accept header must
			// match the server's major.minor or every request 406s.
			APIVersion: os.Getenv("NETBOX_API_VERSION"),
			Timeout:    30 * time.Second,
		})
		nb.SetLogger(log)
		apiCfg.NetBox = nb
		log.Info("NetBox integration enabled", "url", cfg.NetBoxURL)

		// Wire the snapshot + WS pipeline. Construction is cyclic —
		// registry → dispatcher → hub → registry — so everything that
		// needs the registry before it exists gets a late-binding
		// adapter over the `reg` pointer instead:
		//   1. `inv` adapters close over reg (nil until step 5).
		//   2. Mirror / commission / op-update handlers use inv.
		//   3. Hub uses store (auth) + a Rebuilder adapter + the
		//      handlers; Dispatcher uses the hub as its PushSink.
		//   4. Registry is built from the dispatcher — the only
		//      holder of the bumping rebuild path.
		//   5. Bind reg, start everything. Nothing invokes the
		//      adapters until the HTTP server / reconciler start,
		//      which happens after the bind.
		var reg *snapshot.Registry

		inv := invalidatorFunc(func(slug string) {
			reg.Invalidate(slug)
		})

		mir := mirror.New(st, inv)
		mir.SetLogger(log)

		// Single membership predicate shared by the webhook + commission
		// paths: resolves a raw device to the config(s) claiming it (site +
		// location-subtree), reusing the reconciler's resolveScope. The
		// reconciler scopes inline so it doesn't need this.
		claimResolver := snapshot.NewClaimResolver(nb, st)

		commHandler := commission.New(st, nb, inv)
		commHandler.SetLogger(log)
		commHandler.SetClaimResolver(claimResolver)
		// Console determination sweep: resolve which candidate tty's
		// probe nonce reached the cast stream, then write the
		// discovered slagworks_kernel_params to the device-type.
		commHandler.SetCastScanner(st)
		// Wire the AWS Secrets Manager monitor-cred writer for the
		// deploy_complete and bmc_reconcile_report observation
		// handlers. Same Client as the admin reader; the IRSA role
		// grants both Get and Put on pop/*/bmc-creds-monitor-* (added
		// 2026-05-20). nil bmcSecrets (local-dev / no AWS) leaves
		// deploy_complete in "warn-and-drop monitor" mode.
		if bmcSecrets != nil {
			commHandler.SetMonitorWriter(bmcSecrets)
		}

		opUpdateHandler := hubhandlers.NewOpUpdate(st)
		opUpdateHandler.SetMetrics(opsM)
		opUpdateHandler.SetLogger(log)
		// Tier 2: op phase transitions publish op_delta on the op stream
		// (wired via SetPublisher below, after the hub is built). The
		// identity rebuild fires ONLY on a DURABLE machines.state write
		// (completion → running/commissioned). Fan out across all
		// site_configs, mirroring the API server's invalidateAllSites; the
		// invalidator is debounced so a burst coalesces.
		durableInvalidate := func() {
			// Run async: HandleOpUpdate executes in the WS hub's handler
			// read loop, and ListSiteConfigs is a synchronous DB query.
			// Doing it inline would block the hub on every terminal update
			// (and the store layer takes no context, so a slow
			// query stalls the caller). The goroutine is short-lived and
			// terminal transitions are low-rate; inv.Invalidate is itself
			// non-blocking (it arms a debounced AfterFunc), so the rebuilds
			// still coalesce.
			go func() {
				cfgs, err := st.ListSiteConfigs()
				if err != nil {
					log.Warn("op_update invalidate: listing site_configs failed", "err", err)
					return
				}
				for _, cfg := range cfgs {
					inv.Invalidate(cfg.Slug)
				}
			}()
		}
		opUpdateHandler.SetInvalidator(durableInvalidate)

		// Structured-log pipeline: site workers push envelope batches
		// over the WS hub. Persist them to log_envelopes (the source
		// of truth for `slagctl logs`), re-emit each as JSONL
		// to stdout so alloy-logs picks them up for Loki, and push
		// into globalLogSink so /api/v1/logs/stream subscribers see
		// the live tail without re-reading pg.
		logSt := logstore.New(st.DB())
		// Daily retention sweep for log_envelopes (30d agent/site/sol,
		// 90d global). console_cast_events is exempt — SOL casts are
		// kept forever as an audit record; the cast-size gauge below is
		// the runaway-console tripwire instead.
		go logSt.RunRetention(ctx, logstore.DefaultRetention(), 24*time.Hour, log)
		go runCastSizeGauge(ctx, st, metrics, log)
		// Daily retention sweep for dhcp_lease_ledger (Phase 3 of
		// docs/plans/2026-07-13-dhcp-lease-visibility.md): closed rows
		// older than 90 days are pruned; open rows are never touched
		// regardless of age.
		go runDHCPLedgerRetention(ctx, st, 24*time.Hour, log)
		globalLogSink := logring.New(16<<20, time.Hour)
		go func() { <-ctx.Done(); globalLogSink.Close() }()
		logBatchHandler := hubhandlers.NewLogBatch(logSt, os.Stdout, globalLogSink)
		logBatchHandler.SetLogger(log)

		// SOL asciinema pipeline: persist cast frames pushed
		// by site workers into console_cast_events, and answer the
		// resume-offset RPC pumps fire on every session start.
		castStore := hubhandlers.StoreAdapter{S: st}
		castBatchHandler := hubhandlers.NewCastBatch(castStore)
		castBatchHandler.SetLogger(log)
		castResumeHandler := hubhandlers.NewCastResume(castStore)
		castResumeHandler.SetLogger(log)

		hub := ws.NewHub(ws.HubConfig{
			Auth: st,
			Rebuilder: rebuilderFunc(func(ctx context.Context, slug string) (model.SiteSnapshot, error) {
				return reg.RebuildAndWait(ctx, slug)
			}),
			ObsHandler:        commHandler,
			OpUpdateHandler:   opUpdateHandler,
			LogBatchHandler:   logBatchHandler,
			CastBatchHandler:  castBatchHandler,
			CastResumeHandler: castResumeHandler,
			OpClaimer:         st, // *store.Store satisfies ws.OpClaimer
			// Tier 2: the hub publishes op-stream messages (op_delta on
			// claim, op_resync on hello) instead of invalidating identity
			// on a claim. Invalidator is now called only on DURABLE
			// machines.state writes (completion / abort / sweep fail).
			Invalidator: inv,
			// ActiveOps supplies the full active-op set for the hello
			// resync. *store.Store satisfies ws.ActiveOpsReader.
			ActiveOps: st,
			// SerialResolver enforces hardware-attested worker identity:
			// the reported SMBIOS serial must join to exactly
			// one NetBox device in the worker's site scope, else the hello
			// is refused. Reuses the same serial-join + claim-scope
			// predicates as enrollment.
			SerialResolver: serialResolver{nb: nb, claim: claimResolver},
			Logger:         log,
			Version:        version,
			PackageVersion: packageVersion,
			Metrics:        opsM,
		})
		apiCfg.WSHub = hub
		// The hub is the op-event publisher for the op_update handler
		// (phase op_delta / terminal op_removed), the commission handler
		// (commission-complete op_removed), the API server (op-create
		// op_delta, abort/sweep op_removed), and the mirror (B7 drift-fail
		// op_removed — without it a worker holds a zombie active op until
		// reconnect).
		opUpdateHandler.SetPublisher(hub)
		commHandler.SetPublisher(hub)
		mir.SetPublisher(hub)
		apiCfg.OpPublisher = hub
		// Synchronous power-status reads route through the hub to a
		// connected site worker (BMCs are reachable only from the
		// site's OOB network).
		apiCfg.PowerStatusRPC = hub
		// The site-worker delete path refuses to delete a currently-connected
		// worker's registration row; the hub answers the liveness question.
		apiCfg.WorkerLiveness = hub
		// GET /workers reports each connected worker's SMCIPMITool status.
		apiCfg.WorkerTools = hub
		apiCfg.LogStore = logSt
		// The console-stream endpoint reads from console_casts /
		// console_cast_events; the api package's adapter wraps
		// *store.Store to expose the small ConsoleStreamStore surface.
		apiCfg.ConsoleStore = api.NewConsoleStoreAdapter(st)
		go func() {
			<-ctx.Done()
			hub.Close()
		}()

		dispatcher := snapshot.NewDispatcher(nb, st, hub)
		dispatcher.SetMetrics(metrics)
		dispatcher.SetHostnameTemplate(hostnames)
		if hostnames.String() == "" {
			log.Info("hostname template unset: deployed hostnames are NetBox device names")
		} else {
			log.Info("hostname template set", "template", hostnames.String())
		}
		// Pre-stamp admin BMC creds onto each MachineSnapshot so the
		// site worker's Cache carries creds for every BMC in the pop,
		// independent of any active op envelope. Same Client as the
		// resolveBMCRuntime / commission monitor-writer path.
		if bmcSecrets != nil {
			dispatcher.SetBMCSecrets(bmcSecrets)
		}

		// Bind the registry: the sole holder of the dispatcher's
		// bumping rebuild path. Every earlier `inv`/Rebuilder adapter
		// resolves to this pointer at call time.
		reg = snapshot.NewRegistry(dispatcher)
		reg.SetLogger(log)

		reconciler := snapshot.NewReconciler(nb, st, inv, mir)
		reconciler.SetLogger(log)
		reconciler.SetMetrics(metrics)

		wh := netbox.NewWebhookHandler(mir, claimResolver, cfg.WebhookSecret)
		wh.SetLogger(log)
		if cfg.WebhookSecret == "" {
			log.Warn("NetBox webhook endpoint disabled until SLAGWORKS_WEBHOOK_SECRET is set: " +
				"POST /api/v1/hooks/netbox rejects every delivery with 503; " +
				"the 5-minute NetBox reconcile still mirrors device changes")
		}
		apiCfg.WebhookHandler = wh
		apiCfg.Readiness = reconciler
		apiCfg.Invalidator = inv
		// Op-creation preflight builds the owning site's snapshot read-only
		// (no push/hash/bump) to validate a host's NetBox model before
		// persisting a deploy/commission op. Same dispatcher the live boot
		// path drives, so the check can't drift from the real boot.
		apiCfg.SnapshotBuilder = dispatcher

		reg.Start(ctx)
		go func() {
			<-ctx.Done()
			reg.Stop()
		}()
		// Proactive warm-up: build every known slug's snapshot at
		// startup so workers reconnecting after a global restart get a
		// hot cache instead of each blocking on a cold rebuild. A hello
		// that races this batches onto the in-flight warm-up rebuild
		// via RebuildAndWait rather than duplicating it.
		if cfgs, err := st.ListSiteConfigs(); err != nil {
			log.Warn("startup snapshot warm-up: listing site_configs failed", "err", err)
		} else {
			for _, c := range cfgs {
				reg.Invalidate(c.Slug)
			}
		}
		go reconciler.Run(ctx)
	} else if cfg.NetBoxURL != "" || cfg.NetBoxToken != "" {
		log.Warn("NetBox integration NOT enabled: both --netbox-url and --netbox-token are required",
			"url_set", cfg.NetBoxURL != "", "token_set", cfg.NetBoxToken != "")
	}

	srv := api.NewServer(apiCfg)
	srv.SetOpsMetrics(opsM)
	srv.SetLogger(log)
	return srv, nil
}

func globalCmd() *cobra.Command {
	var cfg globalConfig
	var addr string
	var oidcGroupsRaw, oidcCredentialGroupsRaw string

	cmd := &cobra.Command{
		Use:   "global",
		Short: "Run the central API server (Postgres-backed)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			slog.SetDefault(log)
			log.Info("starting slagworksd global", "version", version)

			cfg.OIDCGroups = parseGroupList(oidcGroupsRaw)
			cfg.OIDCCredentialGroups = parseGroupList(oidcCredentialGroupsRaw)

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			srv, err := buildGlobal(ctx, log, cfg)
			if err != nil {
				return err
			}

			log.Info("slagworksd global starting", "addr", addr)
			return srv.ListenAndServe(ctx, addr)
		},
	}

	cmd.Flags().StringVar(&cfg.DSN, "dsn", envutil.Or("DATABASE_URL", "host=localhost user=slagworks password=slagworks dbname=slagworks sslmode=disable"), "Postgres DSN; with an RDS host and no password, logs in with RDS IAM tokens (sslmode=verify-full required)")
	cmd.Flags().StringVar(&addr, "addr", envutil.Or("LISTEN_ADDR", ":8081"), "listen address")
	cmd.Flags().StringVar(&cfg.NetBoxURL, "netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL (enables SSoT snapshot pipeline)")
	cmd.Flags().StringVar(&cfg.NetBoxToken, "netbox-token", os.Getenv("NETBOX_TOKEN"), "NetBox API token")
	cmd.Flags().StringVar(&cfg.OIDCIssuer, "oidc-issuer", os.Getenv("SLAGWORKS_OIDC_ISSUER"), "OIDC issuer (`iss`) to require on tokens: the identity provider URL operators' browsers reach (required)")
	cmd.Flags().StringVar(&cfg.OIDCJWKSURL, "oidc-jwks-url", os.Getenv("SLAGWORKS_OIDC_JWKS_URL"), "JWKS URL to fetch signing keys from, when the server reaches the identity provider at a different address than the issuer; empty derives <issuer>/jwks/")
	cmd.Flags().StringVar(&cfg.OIDCAudience, "oidc-audience", os.Getenv("SLAGWORKS_OIDC_AUDIENCE"), "OIDC audience to require on access tokens (the slagworks client ID)")
	cmd.Flags().StringVar(&oidcGroupsRaw, "oidc-required-groups", os.Getenv("SLAGWORKS_OIDC_REQUIRED_GROUPS"), "identity-provider group(s) allowed operator access, comma-separated, any-of (required)")
	cmd.Flags().StringVar(&oidcCredentialGroupsRaw, "oidc-credential-groups", os.Getenv("SLAGWORKS_OIDC_CREDENTIAL_GROUPS"), "identity-provider group(s) allowed to read stored device credentials in plaintext, comma-separated, any-of, in addition to an operator group; empty lets every operator read them")
	cmd.Flags().StringVar(&cfg.ExternalURL, "external-url", os.Getenv("SLAGWORKS_EXTERNAL_URL"), "externally reachable URL for webhooks")
	cmd.Flags().StringVar(&cfg.WebhookSecret, "webhook-secret", os.Getenv("SLAGWORKS_WEBHOOK_SECRET"), "HMAC secret for NetBox webhooks; empty rejects every webhook delivery")
	cmd.Flags().StringVar(&cfg.TailscaleOAuthClientID, "tailscale-oauth-client-id", os.Getenv("TAILSCALE_OAUTH_CLIENT_ID"), "Tailscale OAuth client ID (auth_keys scope) for deploy-time tailnet enrollment; empty disables")
	cmd.Flags().StringVar(&cfg.TailscaleOAuthClientSecret, "tailscale-oauth-client-secret", os.Getenv("TAILSCALE_OAUTH_CLIENT_SECRET"), "Tailscale OAuth client secret for deploy-time tailnet enrollment; empty disables")
	cmd.Flags().StringVar(&cfg.HostnameTemplate, "hostname-template", os.Getenv("SLAGWORKS_HOSTNAME_TEMPLATE"), "template for deployed hostnames from the NetBox device name: {name} is the whole name, {node} and {zone} the parts before and after its last dot (e.g. {node}.internal.{zone}.example.net); empty uses the device name")

	return cmd
}

// parseGroupList splits a comma-separated group setting, trimming each entry
// and dropping blanks, so a blank or whitespace-only value yields no groups.
func parseGroupList(raw string) []string {
	var groups []string
	for g := range strings.SplitSeq(raw, ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}

// runCastSizeGauge publishes per-machine console-cast storage volume
// hourly. Cast events are kept forever (audit record), so this gauge
// is the only signal that a machine's serial console has turned into
// a firehose before it becomes a disk-space incident.
func runCastSizeGauge(ctx context.Context, st *store.Store, metrics *snapshot.Metrics, log *slog.Logger) {
	tick := func() {
		stats, err := st.CastStorageStats(ctx)
		if err != nil {
			log.Warn("cast storage stats query failed", "err", err)
			return
		}
		// Reset before repopulating so machines that leave the result
		// set (deleted machine + cast) don't export a stale last value.
		metrics.CastStorageBytes.Reset()
		for _, s := range stats {
			metrics.CastStorageBytes.WithLabelValues(s.MachineID.String()).Set(float64(s.Bytes))
		}
	}
	tick()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// runDHCPLedgerRetention prunes closed dhcp_lease_ledger rows older
// than 90 days on the given interval (24h in production). Runs once
// immediately on startup so a long-stopped global catches up right
// away, then on the ticker. Errors are logged and retried on the next
// tick — a transient pg failure here must not crash the daemon.
func runDHCPLedgerRetention(ctx context.Context, st *store.Store, interval time.Duration, log *slog.Logger) {
	const retention = 90 * 24 * time.Hour
	tick := func() {
		n, err := st.PruneDHCPLeaseLedger(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Warn("dhcp_lease_ledger retention sweep failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("dhcp_lease_ledger retention sweep", "pruned", n)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// rebuilderFunc adapts a closure into the ws.Rebuilder interface. The
// hub needs a Rebuilder before the registry exists (their construction
// is mutually recursive through pointer indirection); a func-typed
// adapter lets us hand the hub something that resolves to the registry
// at call time, not construction time.
type rebuilderFunc func(ctx context.Context, slug string) (model.SiteSnapshot, error)

func (f rebuilderFunc) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	return f(ctx, slug)
}

// invalidatorFunc is the same late-binding trick for the many
// Invalidator consumers (mirror, commission, api, hub op-claim): they
// are constructed before the registry, so they hold a closure over the
// `reg` pointer instead of the registry itself.
type invalidatorFunc func(slug string)

func (f invalidatorFunc) Invalidate(slug string) { f(slug) }

// serialResolver implements ws.WorkerSerialResolver: it joins a
// worker-reported hardware serial to a NetBox device (by serial) and confirms
// the device is claimed by the worker's site-config scope, reusing the same
// GetDevicesBySerial + ClaimResolver predicates the enrollment path uses.
// Kept in cmd/slagworksd so internal/ws stays free of a netbox
// import.
type serialResolver struct {
	nb    *netbox.Client
	claim *snapshot.ClaimResolver
}

// Resolve returns nil iff serial resolves to exactly one NetBox device claimed
// by slug's scope. Zero/multiple/out-of-scope map to the ws sentinels the hub
// refuses on; a lookup failure returns the raw error (fail-closed — the hub
// refuses, the operator retries once NetBox recovers).
func (r serialResolver) Resolve(ctx context.Context, slug, serial string) error {
	devices, err := r.nb.GetDevicesBySerial(ctx, serial)
	if err != nil {
		return fmt.Errorf("netbox lookup by serial %q: %w", serial, err)
	}
	if len(devices) == 0 {
		return ws.ErrSerialUnknown
	}
	if len(devices) > 1 {
		return fmt.Errorf("%w: %d devices", ws.ErrSerialAmbiguous, len(devices))
	}
	claimed, err := r.claim.ClaimingConfigs(ctx, devices[0])
	if err != nil {
		return fmt.Errorf("resolve claim scope for device %q: %w", devices[0].Name, err)
	}
	for _, s := range claimed {
		if s == slug {
			return nil
		}
	}
	return fmt.Errorf("%w: device %q claimed by %v, not %q", ws.ErrSerialOutOfScope, devices[0].Name, claimed, slug)
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", configErrorf("required environment variable %s is not set", key)
	}
	return v, nil
}
