package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	cryptossh "golang.org/x/crypto/ssh"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
	"github.com/bluesky-social/slagworks/internal/snapshot"
	"github.com/bluesky-social/slagworks/internal/storage"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Server is the global HTTP API. See docs/dev/api.md for the target endpoint
// inventory. Plan 1 trims the surface heavily; Plan 2 re-adds the WS sync
// protocol against the new SiteSnapshot; Plan 3 adds slagctl-facing
// tooling like machine purge and NetBox refresh.
type Server struct {
	echo               *echo.Echo
	store              *store.Store
	log                *slog.Logger
	opTimeout          time.Duration
	pendingOpTimeout   time.Duration
	phoneHomeTimeout   time.Duration
	orphanedClaimGrace time.Duration
	verifier           tokenverify.Verifier
	authz              tokenverify.Authorizer
	// credAuthz is the extra check credentialAuth applies to the routes
	// that return a stored device credential in plaintext. nil: none, so
	// every operator who passes authz may read credentials.
	credAuthz tokenverify.Authorizer
	// oidcIssuer/oidcAudience are retained to enrich the auth-rejection log
	// line (the "expected" values shown next to a rejected token's own
	// iss/aud) and to answer GET /client-config. They are the same values
	// baked into the verifier; the verifier remains the sole authority for
	// the decision.
	oidcIssuer       string
	oidcAudience     string
	instanceID       string
	version          string
	externalURL      string
	webhookSecret    string
	netbox           *netbox.Client
	bmcSecrets       awssecrets.Reader
	monitorBootstrap MonitorBootstrapper
	tailscaleMinter  TailscaleMinter
	webhookHandler   *netbox.WebhookHandler
	readiness        Readiness
	promRegistry     *prometheus.Registry
	opsMetrics       *opsmetrics.Metrics // nil = sweeps unobserved
	wsHub            http.Handler
	logStore         LogStreamStore
	consoleStore     ConsoleStreamStore
	invalidator      Invalidator
	opPublisher      OpPublisher
	powerStatusRPC   PowerStatusRPC
	snapshotBuilder  SnapshotBuilder
	workerLiveness   WorkerLiveness
	workerTools      WorkerTools
	expireTicker     *time.Ticker
	bgWG             sync.WaitGroup
	// preflightKey is the server-local HMAC key used to sign and verify
	// preflight tokens issued by the PDU outlet resolve endpoint. It is
	// either derived from webhookSecret (if non-empty) or randomly
	// generated at startup. Tokens bound to this key become invalid on
	// server restart, which is fine: a 5-minute preflight window never
	// survives a restart in practice, and fail-closed on expiry is correct.
	preflightKey []byte
}

type Config struct {
	Addr  string
	Store *store.Store
	// OpTimeout bounds claimed/running ops from claimed_at. Default 30m.
	// Excludes phase=awaiting_phone_home (handled by PhoneHomeTimeout).
	OpTimeout time.Duration
	// PendingOpTimeout bounds pending (never-claimed) ops from
	// created_at. Default 24h. Long because operator-scheduled work
	// may legitimately wait while a worker is offline.
	PendingOpTimeout time.Duration
	// OrphanedClaimGrace bounds ops in status=claimed with NO phase
	// from claimed_at (B4: the claiming worker died between the claim
	// commit and its first actuation). Default 2m — a healthy claim
	// records phase power_cycle within seconds, and a lost-reply
	// worker re-claims within one executor tick (5s), so 2m of
	// claimed-no-phase means the worker is gone.
	OrphanedClaimGrace time.Duration
	// PhoneHomeTimeout bounds deploy ops in phase=awaiting_phone_home
	// from phase_entered_at. Default 30m.
	PhoneHomeTimeout time.Duration

	// OIDC operator auth. Verifier validates Authentik access tokens; the
	// production verifier is built by NewServer from OIDCIssuer/OIDCAudience.
	// Tests may inject a fake Verifier directly to exercise the middleware
	// with self-minted JWTs. RequiredGroup gates every operator endpoint
	// (no default: an empty list refuses every token).
	// OIDCIssuer is the `iss` value to require on tokens — the Authentik host
	// the operator's browser hits (tailnet/public). OIDCJWKSURL is where
	// slagworksd fetches signing keys, which is the IN-CLUSTER Authentik
	// service (reachable from EKS); it is intentionally a different host from
	// the issuer (split-horizon — see oidcauth.go). If OIDCJWKSURL is empty it
	// defaults to the issuer's standard /jwks/ path (single-host/dev case).
	OIDCIssuer   string
	OIDCJWKSURL  string
	OIDCAudience string
	// RequiredGroups is the operator allowlist by Authentik group (any-of).
	// Comma-separated string OR a pre-split slice — NewServer accepts the
	// slice form; the cobra/global layer parses the env and requires at least one.
	RequiredGroups []string
	// CredentialGroups, when non-empty, additionally gates the routes that
	// return a stored device credential in plaintext (any-of): the caller
	// must pass RequiredGroups AND carry one of these. Empty leaves those
	// routes under RequiredGroups alone.
	CredentialGroups []string
	// Verifier, if non-nil, is used as-is (test injection). If nil and
	// OIDCIssuer is set, NewServer builds the verifier via tokenverify.New.
	Verifier tokenverify.Verifier
	// Authz, if non-nil, overrides the RequiredGroups-derived authorizer
	// (test injection / custom policy).
	Authz tokenverify.Authorizer

	Version       string
	ExternalURL   string
	WebhookSecret string
	NetBox        *netbox.Client
	BMCSecrets    awssecrets.Reader
	// MonitorBootstrap creates the per-pop monitor secret at site-config
	// creation (the *awssecrets.Client satisfies it). nil disables the
	// bootstrap (local-dev / no AWS); site-config create still succeeds.
	MonitorBootstrap MonitorBootstrapper
	WebhookHandler   *netbox.WebhookHandler

	// TailscaleMinter mints single-use tailnet auth keys for deploy-time
	// host enrollment. nil disables enrollment (dev/test, or no OAuth
	// client configured) — deploys proceed without it.
	TailscaleMinter TailscaleMinter

	// Readiness exposes snapshot reconciler state to /healthz. nil is
	// allowed (Plan 1 intermediate state) and falls back to a minimal
	// Postgres-ping handler.
	Readiness Readiness
	// PrometheusRegistry is served at /metrics. nil is allowed (no
	// /metrics endpoint registered).
	PrometheusRegistry *prometheus.Registry
	// WSHub is the site-worker WebSocket hub, mounted at
	// /api/v1/ws/site. nil disables the endpoint (e.g. in unit tests
	// that don't exercise the WS path).
	WSHub http.Handler

	// LogStore is the log_envelopes read store, mounted at
	// /api/v1/logs/stream as the slagctl logs backend.
	// nil disables the endpoint.
	LogStore LogStreamStore

	// ConsoleStore is the read surface for the SOL asciinema
	// pipeline, mounted at /api/v1/console/stream as the
	// `slagctl console` backend. nil disables the endpoint.
	ConsoleStore ConsoleStreamStore

	// Invalidator is called after admin-path mutations (machine
	// deploy/commission/lock/etc., site-config create/update) so
	// connected workers see the new state without waiting for the
	// 5-minute reconciler tick. nil is allowed (tests); handlers
	// silently skip the notification.
	Invalidator Invalidator

	// OpPublisher fans op-stream messages to workers (Tier 2 Phase 2):
	// op-create publishes an op_delta (pending row); abort + the expiry
	// sweeps publish op_removed per affected op. Identity invalidation is
	// reserved for durable machines.state writes. nil is allowed (tests):
	// op-create/abort/sweeps then rely on the worker's next op_resync.
	OpPublisher OpPublisher

	// PowerStatusRPC is the hub's synchronous global→worker power-
	// status read (ws.Hub.PowerStatus). nil disables the endpoint
	// (returns 503) — tests and configurations without a WS hub.
	PowerStatusRPC PowerStatusRPC

	// SnapshotBuilder builds a site's snapshot READ-ONLY for op-creation
	// preflight (the *snapshot.Dispatcher). It runs the exact gather+Build
	// the boot path uses, so the preflight validator inspects precisely
	// what the boot would consume. nil disables preflight (deploy/commission
	// proceed unguarded) — allowed in tests / NetBox-off configurations.
	SnapshotBuilder SnapshotBuilder

	// WorkerLiveness reports whether a site worker is currently connected
	// over WS (ws.Hub.IsWorkerConnected). The site-worker delete path uses
	// it to refuse deleting a live connection's row. nil is allowed (tests):
	// the handler then skips the connected-worker guard (the in-flight-op
	// guard still applies).
	WorkerLiveness WorkerLiveness

	// WorkerTools supplies live SMCIPMITool status for GET /workers.
	// nil (tests) omits those fields.
	WorkerTools WorkerTools
}

// SnapshotBuilder is the subset of *snapshot.Dispatcher op-creation
// preflight uses: a read-only build of a site config's snapshot with no
// hash/push/SeqNum-bump side effects.
type SnapshotBuilder interface {
	BuildSnapshot(ctx context.Context, slug string) (model.SiteSnapshot, error)
}

// PowerStatusRPC is the subset of *ws.Hub the synchronous BMC-read paths
// (power status, boot status) use. An interface rather than *ws.Hub so
// handler tests can stub the RPC without standing up a live hub + worker
// connection. (The api package already imports internal/ws for the
// log-stream wire types, so referencing the message structs costs nothing.)
type PowerStatusRPC interface {
	PowerStatus(ctx context.Context, siteSlug string, machineID uuid.UUID, bmcRuntime, amtRuntime json.RawMessage) (ws.PowerStatusResponseMsg, error)
	BootStatus(ctx context.Context, siteSlug string, machineID uuid.UUID, bmcRuntime json.RawMessage) (ws.BootStatusResponseMsg, error)
	// ConnectedWorkerSerials returns the verified hardware serials of the
	// workers currently connected for a slug (one per connection, "" for an
	// unverified worker). The P4b PDU-outlet POST uses it for the
	// op-creation-time eligible-partner check on a non-AllowSelf self-feed
	// cycle/on (advisory routing, not the safety gate). Satisfied by *ws.Hub.
	ConnectedWorkerSerials(slug string) []string
	// DHCPLeases fans a dhcp_leases RPC to every connected worker for the
	// slug and collects each worker's answer (or error). Satisfied by
	// *ws.Hub. Used by GET /site-configs/:slug/dhcp-leases — a distinct
	// read surface from power/boot status: it's keyed by site slug
	// directly (no per-machine BMC routing) and fans to ALL workers, not
	// one.
	DHCPLeases(ctx context.Context, siteSlug string) []ws.WorkerDHCPLeases
}

// Invalidator is the subset of *snapshot.Registry the API uses.
type Invalidator interface {
	Invalidate(slug string)
}

// OpPublisher is the op-event publish surface (satisfied by *ws.Hub).
type OpPublisher interface {
	PublishOpDelta(op model.Operation, machineState model.MachineState)
	PublishOpRemoved(op model.Operation, machineState model.MachineState)
}

// WorkerLiveness is the subset of *ws.Hub the site-worker delete admin
// path uses to refuse deleting a currently-connected worker. Satisfied by
// *ws.Hub; an interface so handler tests can stub it without a live hub.
type WorkerLiveness interface {
	IsWorkerConnected(siteSlug, workerID string) bool
}

// WorkerTools is the subset of *ws.Hub GET /workers uses to report whether
// each connected worker has the operator-installed Supermicro SMCIPMITool,
// and whether its site needs it. Satisfied by *ws.Hub.
type WorkerTools interface {
	WorkerSMCIPMITool(siteSlug, workerID string) (status string, connected bool)
	SupermicroMachineCount(slug string) int
}

// publishOpDelta fans an op_delta if a publisher is wired. No-op in tests.
func (s *Server) publishOpDelta(op model.Operation, st model.MachineState) {
	if s.opPublisher != nil {
		s.opPublisher.PublishOpDelta(op, st)
	}
}

// publishOpRemoved fans an op_removed if a publisher is wired. No-op in tests.
func (s *Server) publishOpRemoved(op model.Operation, st model.MachineState) {
	if s.opPublisher != nil {
		s.opPublisher.PublishOpRemoved(op, st)
	}
}

// invalidateSite notifies the dispatcher that a specific slug needs a
// fresh snapshot build. No-op if no invalidator wired (tests).
func (s *Server) invalidateSite(slug string) {
	if s.invalidator == nil || slug == "" {
		return
	}
	s.invalidator.Invalidate(slug)
}

// invalidateAllSites fans out Invalidate across every configured
// site. Used by machine mutations where we don't have the slug to
// hand — machines carry NetBoxDeviceID, not a site binding. The
// dispatcher's content-hash skip makes per-site rebuilds cheap on
// unrelated slugs, so fanning out is safe.
func (s *Server) invalidateAllSites() {
	if s.invalidator == nil {
		return
	}
	cfgs, err := s.store.ListSiteConfigs()
	if err != nil {
		s.log.Warn("invalidate all sites: listing site_configs failed", "err", err)
		return
	}
	for _, cfg := range cfgs {
		s.invalidator.Invalidate(cfg.Slug)
	}
}

func NewServer(cfg Config) *Server {
	if cfg.OpTimeout == 0 {
		cfg.OpTimeout = 30 * time.Minute
	}
	if cfg.PendingOpTimeout == 0 {
		cfg.PendingOpTimeout = 24 * time.Hour
	}
	if cfg.PhoneHomeTimeout == 0 {
		cfg.PhoneHomeTimeout = 30 * time.Minute
	}
	if cfg.OrphanedClaimGrace == 0 {
		cfg.OrphanedClaimGrace = 2 * time.Minute
	}
	// Build the OIDC verifier unless one was injected (tests). NewRemoteKeySet
	// defers the JWKS fetch to first use, so this never blocks on Authentik.
	verifier := cfg.Verifier
	if verifier == nil && cfg.OIDCIssuer != "" {
		jwksURL := cfg.OIDCJWKSURL
		if jwksURL == "" {
			// Single-host fallback: derive the standard jwks path from the
			// issuer. Production sets OIDCJWKSURL to the in-cluster service.
			jwksURL = strings.TrimRight(cfg.OIDCIssuer, "/") + "/jwks/"
		}
		verifier = tokenverify.New(context.Background(), cfg.OIDCIssuer, jwksURL, cfg.OIDCAudience)
	}
	// Authorizer: explicit override (tests) or any-of the required groups.
	authz := cfg.Authz
	if authz == nil {
		authz = tokenverify.RequireAnyGroup(cfg.RequiredGroups...)
	}
	// RequireAnyGroup with no groups denies everyone, so build the
	// credential check only when groups are configured.
	var credAuthz tokenverify.Authorizer
	if len(cfg.CredentialGroups) > 0 {
		credAuthz = tokenverify.RequireAnyGroup(cfg.CredentialGroups...)
	}
	instanceID, _ := cfg.Store.GetInstanceID()

	// preflightKey: derive from webhookSecret if available (same config
	// surface, no extra knob); otherwise generate a random 32-byte key at
	// startup. Tokens keyed here are short-lived (≤5min TTL) and tied to the
	// server process — a restart invalidates outstanding tokens, which is fine.
	preflightKey := []byte(cfg.WebhookSecret)
	if len(preflightKey) == 0 {
		preflightKey = make([]byte, 32)
		if _, err := rand.Read(preflightKey); err != nil {
			// rand.Read only fails on an OS entropy source failure;
			// treat it as fatal — a server without a signing key is misconfigured.
			panic(fmt.Sprintf("generate preflight HMAC key: %v", err))
		}
	}

	s := &Server{
		echo:               echo.New(),
		store:              cfg.Store,
		log:                slog.Default(),
		opTimeout:          cfg.OpTimeout,
		pendingOpTimeout:   cfg.PendingOpTimeout,
		phoneHomeTimeout:   cfg.PhoneHomeTimeout,
		orphanedClaimGrace: cfg.OrphanedClaimGrace,
		verifier:           verifier,
		authz:              authz,
		credAuthz:          credAuthz,
		oidcIssuer:         cfg.OIDCIssuer,
		oidcAudience:       cfg.OIDCAudience,
		instanceID:         instanceID,
		version:            cfg.Version,
		externalURL:        cfg.ExternalURL,
		webhookSecret:      cfg.WebhookSecret,
		netbox:             cfg.NetBox,
		bmcSecrets:         cfg.BMCSecrets,
		monitorBootstrap:   cfg.MonitorBootstrap,
		tailscaleMinter:    cfg.TailscaleMinter,
		webhookHandler:     cfg.WebhookHandler,
		readiness:          cfg.Readiness,
		promRegistry:       cfg.PrometheusRegistry,
		wsHub:              cfg.WSHub,
		logStore:           cfg.LogStore,
		consoleStore:       cfg.ConsoleStore,
		invalidator:        cfg.Invalidator,
		opPublisher:        cfg.OpPublisher,
		powerStatusRPC:     cfg.PowerStatusRPC,
		snapshotBuilder:    cfg.SnapshotBuilder,
		workerLiveness:     cfg.WorkerLiveness,
		workerTools:        cfg.WorkerTools,
		preflightKey:       preflightKey,
	}

	// Every operator-facing JSON response is encoded here, which is what
	// keeps op-param credentials out of them (see operatorJSONSerializer).
	s.echo.JSONSerializer = operatorJSONSerializer{}
	s.echo.Use(middleware.Recover())
	s.echo.Use(middleware.BodyLimit(12 << 20)) // 12 MiB
	s.echo.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:  true,
		LogURI:     true,
		LogStatus:  true,
		LogLatency: true,
		Skipper: func(c *echo.Context) bool {
			// Health probes hit several times a second; logging them at
			// Info would flood the logs with no diagnostic value.
			p := c.Path()
			return p == "/healthz" || p == "/readyz" || p == "/metrics"
		},
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			args := []any{
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				"latency", v.Latency,
			}
			if v.Error != nil {
				args = append(args, "err", v.Error)
			}
			s.log.Info("api request", args...)
			return nil
		},
	}))

	s.registerRoutes()
	return s
}

func (s *Server) SetLogger(l *slog.Logger) { s.log = l }

func (s *Server) Echo() *echo.Echo { return s.echo }

func (s *Server) registerRoutes() {
	// Health + metrics live at the root, unauthenticated: load-balancer
	// and kubelet probes and the Prometheus scraper carry no operator
	// token. If the caller wired a
	// Readiness (production), use the SSoT-aware handler; otherwise
	// fall back to a minimal Postgres-ping handler so pure unit tests
	// don't need to construct a reconciler.
	if s.readiness != nil {
		RegisterHealthz(s.echo, s.readiness, s.version, s.instanceID)
	} else {
		s.echo.GET("/healthz", s.fallbackHealthz)
		s.echo.GET("/readyz", s.fallbackHealthz)
	}
	if s.promRegistry != nil {
		RegisterMetrics(s.echo, s.promRegistry)
	}

	api := s.echo.Group("/api/v1")

	// Unauthenticated by design: a CLI reads it before it can sign in.
	api.GET("/client-config", s.clientConfig)

	// Read-only endpoints (admin token).
	read := api.Group("", s.oidcAuth)
	read.GET("/machines", s.listMachines)
	read.GET("/machines/:id", s.getMachine)
	// Power status is a read: no op row, answered synchronously via a
	// WS RPC to the machine's site worker. The actuation verbs stay
	// on POST /machines/:id/power below.
	read.GET("/machines/:id/power", s.powerStatusMachine)
	read.GET("/machines/:id/boot", s.bootStatusMachine)
	// Preflight is a read: it runs the same op-creation validation
	// (deploy by default; ?op=commission for the commission variant)
	// WITHOUT creating an op, so an operator can check a host's NetBox
	// model before firing a deploy.
	read.GET("/machines/:id/preflight", s.preflightMachineHandler)
	// Match is a read: it reports which config scripts (network + storage)
	// the host resolves to, by building the owning worker's snapshot and
	// reading the authoritative match off it. Answers "what would this host
	// get?" before a deploy, creating no op.
	read.GET("/machines/:id/match", s.matchMachineHandler)
	read.GET("/machines/:id/netbox-diff", s.netboxDiffMachine)
	// The one route that returns a stored device credential in plaintext;
	// credentialAuth adds the optional credential-group check after
	// oidcAuth. Routes that only use credentials server-side (power, boot,
	// op creation) stay under the operator check alone.
	read.GET("/devices/:ident/creds", s.getDeviceCreds, s.credentialAuth)
	// PDU outlet resolve: non-mutating preflight that reverse-traces the
	// outlet's cable and returns the downstream device identity, feed
	// classification (redundant/sole), warning text, and a short-lived
	// preflight token. The CLI uses this to display the warning before
	// prompting the operator for confirmation.
	read.GET("/pdus/:ident/outlet/:n/power/resolve", s.resolvePDUOutletPower)
	read.GET("/operations", s.listOperations)
	read.GET("/operations/:id", s.getOperation)
	read.GET("/images", s.listImages)
	read.GET("/ssh-keys", s.listSSHKeys)
	read.GET("/storage-scripts", s.listStorageScripts)
	read.GET("/storage-scripts/:name/history", s.storageScriptHistory)
	read.GET("/network-scripts", s.listNetworkScripts)
	read.GET("/network-scripts/:name/history", s.networkScriptHistory)
	read.GET("/site-configs", s.listSiteConfigs)
	read.GET("/site-configs/:slug", s.getSiteConfig)
	// dhcp-leases is a read: no op row. It fans a dhcp_leases RPC to
	// every connected worker for the slug (an HA cage's two workers
	// each hold their own dynamic slice's leases) and returns each
	// worker's answer, including per-worker errors.
	read.GET("/site-configs/:slug/dhcp-leases", s.dhcpLeasesSiteConfig)
	// dhcp-lease-history is the ledger read (Phase 3): unlike
	// dhcp-leases this never touches a live worker — it's a plain
	// Postgres query, so it works even with every site worker offline.
	read.GET("/site-configs/:slug/dhcp-lease-history", s.dhcpLeaseHistorySiteConfig)
	read.GET("/workers", s.listWorkers)

	// Admin-only mutating endpoints.
	admin := api.Group("", s.oidcAuth)
	admin.PATCH("/machines/:id", s.updateMachine)
	admin.POST("/machines/:id/deploy", s.deployMachine)
	admin.POST("/machines/:id/commission", s.commissionMachine)
	admin.POST("/machines/:id/lock", s.lockMachine)
	admin.POST("/machines/:id/unlock", s.unlockMachine)
	admin.POST("/machines/:id/activate", s.activateMachine)
	admin.POST("/machines/:id/deactivate", s.deactivateMachine)
	admin.POST("/machines/:id/power", s.powerMachine)
	// PDU outlet power: creates a power op anchored on the PDU's own
	// Machine row (model E). Validates the preflight token from the
	// resolve endpoint, re-resolves current topology, and refuses if
	// anything changed since the token was minted.
	admin.POST("/pdus/:ident/outlet/:n/power", s.powerPDUOutlet)
	admin.POST("/machines/:id/boot", s.setBootMachine)
	admin.POST("/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
	admin.POST("/machines/:id/abort", s.abortMachine)
	admin.POST("/images", s.createImage)
	admin.PUT("/images/:name", s.upsertImage)
	admin.DELETE("/images/:name", s.deleteImage)
	admin.POST("/ssh-keys", s.createSSHKey)
	admin.DELETE("/ssh-keys/:id", s.deleteSSHKey)
	admin.POST("/storage-scripts", s.createStorageScript)
	admin.PUT("/storage-scripts/:name", s.updateStorageScript)
	admin.DELETE("/storage-scripts/:id", s.deleteStorageScript)
	admin.POST("/network-scripts", s.createNetworkScript)
	admin.PUT("/network-scripts/:name", s.updateNetworkScript)
	admin.DELETE("/network-scripts/:id", s.deleteNetworkScript)
	admin.POST("/site-configs", s.createSiteConfig)
	admin.PATCH("/site-configs/:slug", s.updateSiteConfig)
	admin.DELETE("/site-configs/:slug", s.deleteSiteConfig)
	admin.POST("/site-workers", s.registerSiteWorker)
	admin.DELETE("/site-workers/:ident", s.deleteSiteWorker)
	admin.POST("/admin/netbox/bootstrap", s.netboxBootstrap)
	admin.POST("/admin/netbox/migrate-chassis-serial", s.netboxMigrateChassisSerial)

	// Operation claim flows over the WS channel (see ws.Hub's
	// handleClaimOp); there is no REST endpoint. The connection's
	// authenticated identity is used as the claim's worker_id, so
	// no client-provided field can spoof another worker.

	// NetBox webhook: authenticated by its HMAC signature, not a bearer
	// token. With no webhook secret configured it rejects every delivery.
	if s.webhookHandler != nil {
		api.POST("/hooks/netbox", s.webhookHandler.HandleWebhook)
	}

	// Site-worker WebSocket hub. The hub authenticates via ?token=
	// internally against site_workers.token_hash, so we don't wrap it
	// in the adminAuth middleware — that's a different token class.
	// GET is the only method a browser/client would use to upgrade.
	if s.wsHub != nil {
		api.GET("/ws/site", echo.WrapHandler(s.wsHub))
	}

	// Structured-log stream for `slagctl machine logs` and the watch
	// view. Reads log_envelopes via logstore.Store, with a 500ms
	// watermark for live-tail (defends against pg
	// sequence-id-allocated-pre-commit reordering). Operator auth like
	// every other read: the bearer token rides the upgrade request's
	// Authorization header.
	if s.logStore != nil {
		read.GET("/logs/stream", s.handleLogsStream)
	}

	// Console-stream endpoint: SOL asciinema cast events (raw serial
	// output, including snapshot mode). Operator auth, as above.
	if s.consoleStore != nil {
		read.GET("/console/stream", s.handleConsoleStream)
		// Console probe verification: scan recent
		// console_cast_events for a probe nonce. The agent emits
		// the nonce during commission; this endpoint answers
		// "did the bytes arrive?".
		read.GET("/machines/:id/console-probe", s.consoleProbe)
	}
}

// ListenAndServe starts the HTTP server and shuts down on ctx cancellation.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	s.log.Info("API server starting", "addr", addr)

	// Three-tier op timeout sweep:
	//   - PendingOpTimeout (default 24h): pending → failed (no state move)
	//   - OpTimeout (default 30m): claimed/running → failed,
	//     excluding phase=awaiting_phone_home
	//   - PhoneHomeTimeout (default 30m): claimed/running in
	//     phase=awaiting_phone_home → failed (anchored on phase_entered_at)
	// All three converge on FailOperation, which applies the per-op-status
	// branching (initial-commission → revert to new, etc.).
	//
	// Each tick then strips credentials from terminal ops' stored params,
	// after the sweeps so ops they just failed are cleaned on the same tick;
	// the scrub also runs once now, before serving, which cleans every row
	// that went terminal while global was down (and all history on the
	// first rollout). A terminal op therefore keeps its credentials in the
	// table for at most one tick (a minute) while global is up.
	s.scrubTerminalCredentials()
	s.expireTicker = time.NewTicker(time.Minute)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.expireTicker.C:
				s.expireOperations()
				s.scrubTerminalCredentials()
			}
		}
	}()

	// Start serves until ctx is cancelled, then shuts down gracefully (10s)
	// and returns nil; it returns early only on a listen or serve error.
	err := echo.StartConfig{
		Address:         addr,
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 10 * time.Second,
	}.Start(ctx, s.echo)
	s.expireTicker.Stop()
	if err != nil {
		return err
	}
	s.bgWG.Wait()
	return nil
}

// expireOperations runs one tick of the op-timeout sweeps.
//
// Each sweep returns the affected ops' post-mutation outcomes. Publish
// op_removed per op (carrying the durable state the fail wrote, "" for
// pending-expiry), and fire ONE identity rebuild across all sites if ANY
// swept op wrote durable machines.state — per the plan's durable-state rule.
// Pending-expiry writes no state, so a tick that only expires pending ops
// publishes op_removed with no state and fires NO rebuild.
//
// A sweep keeps going past a per-op error and returns BOTH the ops it did
// fail and the joined error, so outs is published whatever err says: those
// ops are terminal in the database and no later tick selects them again, so
// skipping them would leave a connected worker holding them as active until
// it reconnects. The ops behind err are still active and the next tick
// retries them.
func (s *Server) expireOperations() {
	durable := false
	sweep := func(label string, outs []store.OpOutcome, err error) {
		if err != nil {
			s.log.Error("expiry sweep could not fail some expired operations; they stay active and are retried on the next tick",
				"sweep", label, "err", err)
		}
		if len(outs) == 0 {
			return
		}
		s.log.Info("expired operations", "sweep", label, "count", len(outs))
		s.observeSweep(label, int64(len(outs)))
		for _, o := range outs {
			s.publishOpRemoved(o.Op, o.MachineState)
			if o.MachineState != "" {
				durable = true
			}
		}
	}
	outs, err := s.store.ExpirePendingOperations(s.pendingOpTimeout)
	sweep("pending", outs, err)
	outs, err = s.store.ExpireStuckOperations(s.opTimeout)
	sweep("stuck", outs, err)
	outs, err = s.store.ExpireOrphanedClaims(s.orphanedClaimGrace)
	if len(outs) > 0 {
		s.log.Error("expired orphaned claims (worker died mid-claim)", "count", len(outs))
	}
	sweep("orphaned_claim", outs, err)
	outs, err = s.store.ExpirePhoneHomeOperations(s.phoneHomeTimeout)
	sweep("phone_home", outs, err)
	if durable {
		s.invalidateAllSites()
	}
}

// scrubTerminalCredentials removes credentials from the stored params of
// terminal operations (store.ScrubTerminalOperationCredentials). A failure
// is logged and retried on the next tick; until one succeeds, those ops
// keep their credentials in the table (operator reads stay redacted).
func (s *Server) scrubTerminalCredentials() {
	n, err := s.store.ScrubTerminalOperationCredentials()
	if err != nil {
		s.log.Error("terminal operations still hold credentials in the operations table; retrying on the next sweep tick",
			"err", err)
		return
	}
	if n > 0 {
		s.log.Info("scrubbed credentials from terminal operations", "count", n)
	}
}

// resolveMachineParam resolves the :id path parameter to a machine UUID.
// Accepts either a UUID directly, or a NetBox device hostname (containing
// a "."), which is resolved via the NetBox client and joined to the
// machine row by netbox_device_id.
//
// Returns 400 for malformed input, 404 when the hostname doesn't resolve
// or there's no slagworks row for the resolved device, 503 when NetBox
// integration isn't configured but a hostname was supplied.
func (s *Server) resolveMachineParam(c *echo.Context) (uuid.UUID, error) {
	raw := c.Param("id")
	if id, err := uuid.Parse(raw); err == nil {
		return id, nil
	}
	if !strings.Contains(raw, ".") {
		return uuid.Nil, echo.NewHTTPError(http.StatusBadRequest,
			"machine id must be a UUID or a hostname (containing '.')")
	}
	if s.netbox == nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusServiceUnavailable,
			"hostname resolution requires NetBox integration; pass a machine UUID instead")
	}
	// siteID 0 means "any site" — NetBox names like `kworker-l34-1.pop1`
	// already encode the site in the hostname, so cross-site uniqueness
	// is enforced by the name itself.
	dev, err := s.netbox.FindDeviceByName(c.Request().Context(), raw, 0)
	if err != nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("netbox lookup: %v", err))
	}
	if dev == nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no NetBox device named %q", raw))
	}
	m, err := s.store.GetMachineByNetBoxDeviceID(dev.ID)
	if err != nil {
		return uuid.Nil, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no slagworks machine for NetBox device %q (id=%d)", raw, dev.ID))
	}
	return m.ID, nil
}

// --- Machine handlers ---

// listMachines returns machine rows. Filters compose:
//
//   - ?state=<state> filters by Machine.State (DB).
//   - ?serial=<serial> matches NetBoxSerial exactly (DB).
//   - ?hostname=<pattern> case-insensitive substring match against
//     NetBox device.name. Resolves devices in NetBox then joins to
//     machines by netbox_device_id. Each returned machine carries
//     its NetBox hostname so callers can disambiguate substring hits
//     without a second lookup.
//
// hostname is the only filter that requires NetBox. Without ?hostname, the
// DB supplies the base rows and NetBox hostname enrichment runs when NetBox is
// configured. Combinations work: ?hostname=cpu1&state=running narrows the
// NetBox-resolved set by state in memory.
func (s *Server) listMachines(c *echo.Context) error {
	var state *model.MachineState
	if v := c.QueryParam("state"); v != "" {
		st := model.MachineState(v)
		if !st.Valid() {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid state %q", v))
		}
		state = &st
	}
	serial := c.QueryParam("serial")
	hostname := c.QueryParam("hostname")

	if hostname != "" {
		machines, err := s.listMachinesByHostname(c, hostname, state, serial)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, machines)
	}

	machines, err := s.store.ListMachines(state)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if serial != "" {
		filtered := machines[:0]
		for _, m := range machines {
			if m.NetBoxSerial == serial {
				filtered = append(filtered, m)
			}
		}
		machines = filtered
	}
	if err := s.enrichMachineHostnames(c.Request().Context(), machines); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("netbox hostname enrichment: %v", err))
	}
	return c.JSON(http.StatusOK, machines)
}

// enrichMachineHostnames populates the transient Machine.Hostname field from
// NetBox Device.name for rows with a NetBox binding. Unbound machines remain
// empty so callers can render them explicitly as hostname-less.
func (s *Server) enrichMachineHostnames(ctx context.Context, machines []model.Machine) error {
	if s.netbox == nil || len(machines) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(machines))
	ids := make([]int, 0, len(machines))
	for _, m := range machines {
		if m.NetBoxDeviceID == nil {
			continue
		}
		id := *m.NetBoxDeviceID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	devices, err := s.netbox.ListDevicesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	nameByID := make(map[int]string, len(devices))
	for _, d := range devices {
		nameByID[d.ID] = d.Name
	}
	for _, id := range ids {
		if _, ok := nameByID[id]; !ok {
			return fmt.Errorf("NetBox device id %d not found", id)
		}
	}
	for i := range machines {
		if machines[i].NetBoxDeviceID == nil {
			continue
		}
		machines[i].Hostname = nameByID[*machines[i].NetBoxDeviceID]
	}
	return nil
}

// listMachinesByHostname implements the ?hostname=<pattern> filter.
// Looks up NetBox devices by case-insensitive substring on name, then
// joins to slagworks rows by netbox_device_id. Returns each match with
// the NetBox hostname populated so the client can disambiguate.
func (s *Server) listMachinesByHostname(c *echo.Context, pattern string, state *model.MachineState, serial string) ([]model.Machine, error) {
	if s.netbox == nil {
		return nil, echo.NewHTTPError(http.StatusServiceUnavailable,
			"hostname search requires NetBox integration")
	}
	devices, err := s.netbox.FindDevicesByNameSubstring(c.Request().Context(), pattern, 0)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("netbox lookup: %v", err))
	}
	if len(devices) == 0 {
		return []model.Machine{}, nil
	}
	ids := make([]int, len(devices))
	nameByID := make(map[int]string, len(devices))
	for i, d := range devices {
		ids[i] = d.ID
		nameByID[d.ID] = d.Name
	}
	machines, err := s.store.ListMachinesByDeviceIDs(ids)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := machines[:0]
	for _, m := range machines {
		if state != nil && m.State != *state {
			continue
		}
		if serial != "" && m.NetBoxSerial != serial {
			continue
		}
		if m.NetBoxDeviceID != nil {
			m.Hostname = nameByID[*m.NetBoxDeviceID]
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Server) getMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	m, err := s.store.GetMachine(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}
	// Enrich with the NetBox hostname so a by-id fetch (slagctl machine show,
	// the deploy-TUI header's machineLabel) gets a real name instead of falling
	// back to a truncated UUID. Hostname is a transient response field (not in
	// Postgres); the list endpoint already enriches on the ?hostname= path, this
	// gives the single-machine path parity. Best-effort: a missing device id, a
	// nil NetBox client, or a NetBox error leaves Hostname empty rather than
	// failing the core fetch.
	if m.NetBoxDeviceID != nil && s.netbox != nil {
		if dev, derr := s.netbox.GetDevice(c.Request().Context(), *m.NetBoxDeviceID); derr == nil && dev != nil {
			m.Hostname = dev.Name
		}
	}
	return c.JSON(http.StatusOK, m)
}

// updateMachine accepts only slagworks-owned runtime fields. Identity
// (hostname, MACs, BMC IP, site) lives in NetBox; operators edit those
// there. See docs/dev/api.md.
func (s *Server) updateMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	existing, err := s.store.GetMachine(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if v, ok := body["desired_os"].(string); ok {
		existing.DesiredOS = v
	}
	if _, ok := body["state"]; ok {
		// State is owned by the operation lifecycle (claim/complete/abort
		// transitions in the store); a direct PATCH would bypass transition
		// legality and the lock guard.
		return echo.NewHTTPError(http.StatusBadRequest,
			"state is not directly settable; drive it via operations (commission/deploy/abort)")
	}
	if err := s.store.UpdateMachine(existing); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	s.invalidateAllSites()
	return c.JSON(http.StatusOK, existing)
}

// --- Machine action handlers ---

// opCreateSpec carries the parts of op creation that VARY across the
// deploy / commission / power handlers. The shared driver
// (Server.createOperation) runs the invariant tail those three share:
// resolve the requested power transport into op params, reject a BMC op
// whose credential can't be resolved, run any preflight, persist the op,
// then log + invalidate + 201.
//
// Everything that legitimately differs between the handlers — body shape,
// param validation, and the ORDER of machine-param resolution vs body
// parse (deploy/commission validate `via` before resolving the machine;
// power resolves the machine first, so a missing machine is a 404 there
// but a resolve-time 503 for deploy/commission) — stays in each handler.
// Only the tail below is shared. setBootMachine is deliberately NOT a
// caller: it is BMC-only (no --via transport choice) and its no-credential
// message omits the "deploy via PDU" suffix, so routing it through here
// would need a flag that exists only to toggle one string.
type opCreateSpec struct {
	machineID uuid.UUID
	// via is the validated transport discriminator ("", "bmc", "pdu",
	// "amt"). "pdu" routes power through the device's PDU outlets, "amt"
	// through Intel AMT, "bmc" through the BMC; "" auto-selects amt for a
	// machine whose admin-secret entry is kind=amt and bmc otherwise
	// (resolveVia). The handler validates it before building the spec.
	via string
	// opKind is the operator-facing verb in the no-credential 422 body and
	// the preflight-gap 422 body ("deploy"/"commission"/"power"). It is also
	// the verb spliced into the "<opKind> it via its PDU outlets" hint.
	opKind string
	// baseParams is the op-type-specific params BEFORE the transport runtime
	// block is stamped in (deploy's destructive/wipe/recovery flags, power's
	// boot_device). The zero value for a params-less op (commission). The
	// transport resolvers stamp BMCRuntime/PDURuntime and (deploy)
	// TailscaleRuntime onto this same struct.
	baseParams model.OpParams
	// enrollTailscale stamps a single-use tailnet auth key into the params
	// (deploy only). Soft failure: a mint error is logged, not fatal.
	enrollTailscale bool
	// preflightOp, when non-empty, runs the NetBox-model preflight for that
	// op type and rejects with a 422 gap list on a modeled gap. Empty skips
	// preflight (power ops don't preflight).
	preflightOp model.OperationType
	// create persists the op with the resolved params. This is the one
	// genuine behavioral branch: deploy/commission call CreateMachineOp with
	// their allowed-source states (active-op + lock + state guards), power
	// calls CreatePowerOp (no state guard, so a power op is always issuable).
	create func(params json.RawMessage) (*model.Operation, error)
	// logAttrs are extra structured-log key/value pairs for the "operation
	// created" line (power's boot_device). nil for deploy/commission.
	logAttrs []any
}

// createOperation runs the shared op-creation tail for deploy, commission,
// and power. See opCreateSpec for the split between what varies (in the
// spec / the handler) and what's invariant (here).
func (s *Server) createOperation(c *echo.Context, spec opCreateSpec) error {
	ctx := c.Request().Context()

	params := spec.baseParams
	via, err := s.resolveVia(ctx, spec.machineID, spec.via)
	if err != nil {
		return httpResolveErr(err)
	}
	switch via {
	case viaPDU:
		// PDU transport: resolve the device's outlets + apc creds into a
		// pdu_runtime envelope; the executor routes on its presence and the
		// PDU controller no-ops SetBootDevice (host must be permanent-PXE in
		// firmware). No BMC cred required. Hard-fails if the topology isn't
		// modeled (operator asked for PDU; don't silently degrade).
		if err := s.resolvePDURuntime(ctx, spec.machineID, &params); err != nil {
			return httpResolveErr(err)
		}
	case viaAMT:
		// AMT transport (explicit --via amt, or auto-selected because the
		// machine's admin-secret entry is kind=amt): resolve the oob_ip
		// endpoint + digest creds into an amt_runtime envelope; the
		// executor routes on its presence. AMT can only arm a one-time PXE
		// override, so a disk override is rejected here with the reason
		// rather than failing asynchronously at the worker.
		if params.BootDevice == "disk" {
			return echo.NewHTTPError(http.StatusBadRequest,
				"boot_device disk is not supported over Intel AMT (AMT can only arm a one-time pxe override)")
		}
		if err := s.resolveAMTRuntime(ctx, spec.machineID, &params); err != nil {
			return httpResolveErr(err)
		}
	default:
		credNote, err := s.resolveBMCRuntime(ctx, spec.machineID, &params)
		if err != nil {
			return httpResolveErr(err)
		}
		if credNote != "" {
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				noBMCCredMsg(spec.opKind, credNote)+
					" If the device has no BMC (e.g. a leaf switch), "+spec.opKind+" it via its PDU outlets instead: --via pdu.")
		}
	}

	if spec.enrollTailscale {
		// Mint a single-use tailnet auth key and stamp a tailscale_runtime
		// block so the host enrolls onto the tailnet during deploy. SOFT
		// failure: a missing minter / mint error logs and leaves params
		// unchanged (no enrollment); it does not block the op.
		if err := s.resolveTailscaleRuntime(ctx, spec.machineID, &params); err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "tailscale runtime: "+err.Error())
		}
	}

	if spec.preflightOp != "" {
		// Preflight the host's NetBox model BEFORE persisting the op: a
		// required-config gap (no primary_ip4, no matching network/storage
		// script, no boot NIC MAC, no image) would stall or hard-fail the
		// boot minutes in. Reject at creation with the gaps named. Runs the
		// real gather+Build, so it can't drift from the boot.
		pf, err := s.preflightMachine(ctx, spec.machineID, spec.preflightOp)
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "preflight: "+err.Error())
		}
		if !pf.OK {
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				preflightGapMsg(spec.opKind, spec.machineID.String(), pf.Gaps))
		}
	}

	rawParams, err := json.Marshal(params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "marshal op params: "+err.Error())
	}
	op, err := spec.create(rawParams)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	logArgs := append([]any{"op_id", op.ID, "type", op.Type, "machine_id", op.MachineID}, spec.logAttrs...)
	s.log.Info("operation created", logArgs...)
	// A new op is a PENDING row that changed no machine state: publish it
	// on the op stream (op_delta, no machine_state) so workers see it and
	// the executor's pending-op wake fires — NO identity rebuild (op-create
	// invalidateAllSites disappears in Tier 2). (Lifecycle ops deliberately
	// create no separate power-cycle op: the executor power-cycles the host
	// itself after setting PXE boot; a second op would violate
	// idx_one_active_op_per_machine and was always a no-op WARN.)
	s.publishOpDelta(*op, "")
	return c.JSON(http.StatusCreated, op)
}

func (s *Server) deployMachine(c *echo.Context) error {
	var body struct {
		Destructive  bool `json:"destructive"`
		WipeAllDisks bool `json:"wipe_all_disks"`
		Recovery     bool `json:"recovery"`
		// Via selects the power transport, same discriminator as the power
		// op. "" / "bmc" uses the BMC (default). "pdu" routes the deploy's
		// power-cycle to the device's PDU outlets — required for a BMC-less
		// host (e.g. a leaf switch).
		Via string `json:"via,omitempty"`
	}
	_ = c.Bind(&body)
	if !validVia(body.Via) {
		return echo.NewHTTPError(http.StatusBadRequest, invalidViaMsg(body.Via))
	}

	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}

	// A deploy with zero registered SSH keys produces a host with
	// no way for any operator to log in: the agent's writeSSHKeys
	// step pulls from the same /boot/ssh-keys list this validates,
	// and root has no password set. Fail the op at submit time
	// rather than producing a headless machine.
	keys, err := s.store.ListSSHKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(keys) == 0 {
		return echo.NewHTTPError(http.StatusFailedDependency,
			"refusing deploy: no SSH keys registered (deployed host would have no way to log in). "+
				"Add at least one with `slagctl ssh-key add`.")
	}

	// wipe_all_disks without destructive is a contradiction (wiping
	// bystander disks while the target-disk safety check is armed);
	// slagctl never sends that shape, so reject rather than guess.
	if body.WipeAllDisks && !body.Destructive {
		return echo.NewHTTPError(http.StatusBadRequest,
			"wipe_all_disks requires destructive: true")
	}
	return s.createOperation(c, opCreateSpec{
		machineID: id,
		via:       body.Via,
		opKind:    "deploy",
		baseParams: model.OpParams{
			Destructive:  body.Destructive,
			WipeAllDisks: body.WipeAllDisks,
			Recovery:     body.Recovery,
		},
		enrollTailscale: true,
		preflightOp:     model.OpDeploy,
		create: func(params json.RawMessage) (*model.Operation, error) {
			// Deploy allowed sources: commissioned, failed, running.
			// `running` is a redeploy; a running machine is auto-locked on
			// entry, so the operator must `slagctl machine unlock` it first
			// (the lock gate in CreateMachineOp rejects otherwise). State
			// advances at claim time.
			return s.store.CreateMachineOp(id, model.OpDeploy, params,
				model.MachineStateCommissioned, model.MachineStateFailed,
				model.MachineStateRunning)
		},
	})
}

func (s *Server) commissionMachine(c *echo.Context) error {
	var body struct {
		// Via selects the power transport (same as deploy/power). "" / "bmc"
		// uses the BMC (default). "pdu" routes the commission power-cycle to
		// the device's PDU outlets — required for a BMC-less host.
		Via string `json:"via,omitempty"`
	}
	_ = c.Bind(&body)
	if !validVia(body.Via) {
		return echo.NewHTTPError(http.StatusBadRequest, invalidViaMsg(body.Via))
	}

	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}

	// Preflight (spec.preflightOp) needs a boot NIC MAC + commission image +
	// primary_ip4 + a network-script match; it does NOT require a storage
	// match (no commission data yet, and commission doesn't partition).
	return s.createOperation(c, opCreateSpec{
		machineID:   id,
		via:         body.Via,
		opKind:      "commission",
		preflightOp: model.OpCommission,
		create: func(params json.RawMessage) (*model.Operation, error) {
			// Commission allowed sources: new, commissioned, failed,
			// running. `running` is a re-inventory (e.g. NICs added to an
			// in-service host); like redeploy it requires the operator to
			// unlock the auto-locked running machine first. `commissioned`
			// is a re-inventory of a host that commissioned but hasn't
			// deployed yet — needed to re-run inventory/cable reconcile
			// after a commission-side fix without forcing a
			// deploy→running→unlock detour. None of new, commissioned, or
			// failed is auto-locked, so no unlock dance there. State
			// advances at claim time (claimTransition must match this set).
			return s.store.CreateMachineOp(id, model.OpCommission, params,
				model.MachineStateNew, model.MachineStateCommissioned,
				model.MachineStateFailed, model.MachineStateRunning)
		},
	})
}

func (s *Server) lockMachine(c *echo.Context) error       { return s.setMachineLock(c, true) }
func (s *Server) unlockMachine(c *echo.Context) error     { return s.setMachineLock(c, false) }
func (s *Server) activateMachine(c *echo.Context) error   { return s.setMachinePXEEligible(c, true) }
func (s *Server) deactivateMachine(c *echo.Context) error { return s.setMachinePXEEligible(c, false) }

func (s *Server) setMachinePXEEligible(c *echo.Context, eligible bool) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	if err := s.store.SetMachinePXEEligible(id, eligible); err != nil {
		if errors.Is(err, store.ErrMachineNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "machine not found")
		}
		if errors.Is(err, store.ErrMachineLocked) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	s.invalidateAllSites()
	m, err := s.store.GetMachine(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, m)
}

func (s *Server) setMachineLock(c *echo.Context, locked bool) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	if err := s.store.SetMachineLocked(id, locked); err != nil {
		if errors.Is(err, store.ErrMachineNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "machine not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	s.invalidateAllSites()
	m, err := s.store.GetMachine(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, m)
}

func (s *Server) abortMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	m, outcomes, err := s.store.AbortMachine(id)
	// Each aborted op left the active set: publish op_removed carrying the
	// durable state the fail wrote (failed/new; "" for a pending/power op).
	// If ANY op wrote durable machines.state, fire one identity rebuild so
	// the baseline catches up (per the plan's durable-state rule). This runs
	// before the error check: AbortMachine returns the ops it failed even
	// when it errors afterwards, and a retried abort will not find them.
	durable := false
	for _, o := range outcomes {
		s.publishOpRemoved(o.Op, o.MachineState)
		if o.MachineState != "" {
			durable = true
		}
	}
	if durable {
		s.invalidateAllSites()
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, m)
}

func (s *Server) powerMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Action     string `json:"action"`
		BootDevice string `json:"boot_device,omitempty"`
		// Via selects the power transport. "" / "bmc" uses the BMC
		// (default, unchanged). "pdu" resolves the device's PDU outlets
		// and routes the op to a PDU controller. Transport remains an explicit
		// operator choice rather than being inferred from an empty BMCIP.
		Via string `json:"via,omitempty"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if !validVia(body.Via) {
		return echo.NewHTTPError(http.StatusBadRequest, invalidViaMsg(body.Via))
	}
	var opType model.OperationType
	switch body.Action {
	case "on":
		opType = model.OpPowerOn
	case "off":
		opType = model.OpPowerOff
	case "cycle":
		opType = model.OpPowerCycle
	default:
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("invalid power action %q, must be on/off/cycle", body.Action))
	}
	if body.BootDevice != "" {
		switch body.BootDevice {
		case "pxe", "disk":
		default:
			return echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("invalid boot_device %q, must be pxe or disk", body.BootDevice))
		}
	}
	if _, err := s.store.GetMachine(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}
	// boot_device is meaningless over PDU (a PDU can't set boot order;
	// PDU-managed devices boot via permanent PXE). Reject the combo here,
	// before createOperation's resolvePDURuntime, so the operator gets the
	// specific message rather than a generic PDU-topology failure.
	if body.Via == "pdu" && body.BootDevice != "" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"boot_device is not supported with --via pdu (a PDU cannot set boot order; PDU-managed devices boot via permanent PXE)")
	}
	return s.createOperation(c, opCreateSpec{
		machineID:  id,
		via:        body.Via,
		opKind:     "power",
		baseParams: model.OpParams{BootDevice: body.BootDevice},
		logAttrs:   []any{"boot_device", body.BootDevice},
		create: func(params json.RawMessage) (*model.Operation, error) {
			// Power ops (on/off/cycle) go through CreatePowerOp: no state
			// guard, so a power op is always issuable even with a lifecycle
			// op pending — that's how an operator recovers a stuck machine.
			return s.store.CreatePowerOp(id, opType, params)
		},
	})
}

// setBootMachine creates a set_boot_device op: arm a BMC boot-device
// override out-of-band. Mirrors powerMachine's BMC path (resolve creds,
// create the op, invalidate). The worker fails the op "not supported" if
// the host's controller isn't boot-capable (only SMCIPMITool is). A
// pure BMC op — no --via pdu (PDUs have no boot concept).
func (s *Server) setBootMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Device     string `json:"device"`
		Persistent bool   `json:"persistent,omitempty"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	switch body.Device {
	case "pxe", "disk", "default":
	default:
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("invalid device %q, must be pxe/disk/default", body.Device))
	}
	// Clearing an override has no persistence concept; reject the
	// nonsensical combo rather than silently dropping the flag.
	if body.Device == "default" && body.Persistent {
		return echo.NewHTTPError(http.StatusBadRequest,
			"--persistent is not valid with --device default (clearing an override has no persistence)")
	}
	if _, err := s.store.GetMachine(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}

	via, err := s.resolveVia(c.Request().Context(), id, "")
	if err != nil {
		return httpResolveErr(err)
	}
	if via == viaAMT {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			"boot-device set is not supported over Intel AMT: there is no persistent or standalone override; "+
				"commission and deploy arm AMT's one-time pxe boot themselves")
	}

	params := model.OpParams{Device: body.Device, Persistent: body.Persistent}
	credNote, err := s.resolveBMCRuntime(c.Request().Context(), id, &params)
	if err != nil {
		return httpResolveErr(err)
	}
	if credNote != "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, noBMCCredMsg("boot-set", credNote))
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "marshal boot params: "+err.Error())
	}

	op, err := s.store.CreatePowerOp(id, model.OpSetBootDevice, rawParams)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	s.log.Info("operation created",
		"op_id", op.ID, "type", op.Type, "machine_id", op.MachineID,
		"device", body.Device, "persistent", body.Persistent)
	// Pending set_boot_device op: publish op_delta (no machine state), no
	// identity rebuild. See createOperation.
	s.publishOpDelta(*op, "")
	return c.JSON(http.StatusCreated, op)
}

// bmcReconcileMachine creates a bmc_reconcile op: the site worker that owns
// the BMC's OOB network reconciles its IPMI users over the LAN with the
// device's bmc-creds-admin account (see model.OpBMCReconcile). Shaped like
// setBootMachine: resolve the admin runtime exactly as power ops do,
// refuse with the credNote if there is none, then CreatePowerOp — no state
// or lock guard, because reconciling BMC accounts on an in-service host is
// the op's main use and never touches the host OS.
//
// The monitor password: the device's entry in pop/<pop>/bmc-creds-monitor
// (keyed by NetBox serial) is authoritative. When it exists the op carries
// its password (MonitorRuntime, resolved here exactly like the admin
// runtime and never logged) and the worker re-asserts it; the oob-gw
// exporters, which read that entry only when Ansible re-renders their
// config, keep working. Only a device with no entry (a new BMC), or
// rotate_monitor, gets a freshly generated password, which global stores
// from the worker's report.
//
// Refusals beyond the power-op shape: no NetBox (the dev-mode empty
// runtime) is refused — the worker's slot-2 safety gate needs the exact
// admin username; a missing pop/<pop>/bmc-creds-monitor secret is refused
// up front rather than discovered after the BMC already holds a password
// nobody can store; and a stored entry that cannot be re-asserted (another
// account's name, or no password) is refused rather than silently rotated
// over, naming --rotate-monitor as the override.
func (s *Server) bmcReconcileMachine(c *echo.Context) error {
	var body struct {
		// RotateMonitor forces a freshly generated monitor password even
		// when one is stored (`slagctl machine bmc-reconcile --rotate-monitor`).
		RotateMonitor bool `json:"rotate_monitor"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid bmc-reconcile body: "+err.Error())
	}
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	if _, err := s.store.GetMachine(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}
	ctx := c.Request().Context()
	via, err := s.resolveVia(ctx, id, "")
	if err != nil {
		return httpResolveErr(err)
	}
	if via == viaAMT {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			"refusing bmc-reconcile: this device's OOB endpoint is Intel AMT (its bmc-creds-admin entry is kind=amt), "+
				"which has no IPMI user table to reconcile")
	}
	rt, dev, credNote, err := s.resolveBMCRuntimeWithDevice(ctx, id)
	if err != nil {
		return httpResolveErr(err)
	}
	if credNote == "" && (rt.Username == "" || rt.Password == "") {
		credNote = bmcCredNote("", "", "NetBox integration is not configured, so the device's admin entry cannot be resolved")
	}
	if credNote != "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, noBMCCredMsg("bmc-reconcile", credNote))
	}
	// A non-empty cred implies resolveBMCRuntimeWithDevice found the device,
	// its site (with a non-empty serial), and the secrets backend.
	params := model.OpParams{BMCRuntime: &rt, RotateMonitor: body.RotateMonitor}
	monitor, err := s.resolveMonitorRuntime(ctx, dev, body.RotateMonitor)
	if err != nil {
		return err
	}
	params.MonitorRuntime = monitor

	rawParams, err := json.Marshal(params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "marshal bmc_reconcile params: "+err.Error())
	}
	op, err := s.store.CreatePowerOp(id, model.OpBMCReconcile, rawParams)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	s.log.Info("operation created",
		"op_id", op.ID, "type", op.Type, "machine_id", op.MachineID, "serial", dev.Serial,
		"rotate_monitor", body.RotateMonitor, "monitor_stored", monitor != nil)
	s.publishOpDelta(*op, "")
	return c.JSON(http.StatusCreated, op)
}

// resolveMonitorRuntime reads the device's entry in
// pop/<pop>/bmc-creds-monitor for a bmc_reconcile op and returns the
// runtime that carries its password to the worker: nil when there is no
// entry (a new BMC) or the op rotates. It refuses (as an HTTP error) when
// the pop's monitor secret is missing, since a generated password could not
// be stored, and when a stored entry cannot be re-asserted. The password is
// never logged or put in an error.
func (s *Server) resolveMonitorRuntime(ctx context.Context, dev *netbox.Device, rotate bool) (*model.MonitorRuntime, error) {
	pop := dev.Site.Slug
	path := "pop/" + pop + "/bmc-creds-monitor"
	entry, err := s.bmcSecrets.GetMonitorEntry(ctx, pop, dev.Serial)
	switch {
	case errors.Is(err, awssecrets.ErrMonitorSecretMissing):
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf(
			"refusing bmc-reconcile: %s does not exist in AWS Secrets Manager, so a generated monitor password could not be stored; "+
				"bootstrap the pop's monitor secret (creating the pop's site-config does this) and retry", path))
	case err != nil:
		return nil, echo.NewHTTPError(http.StatusServiceUnavailable, fmt.Sprintf("read %s: %v", path, err))
	case rotate || entry == nil:
		return nil, nil
	case entry.Username != bmcreconcile.MonitorUser || entry.Password == "":
		return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf(
			"refusing bmc-reconcile: the %s entry for serial %q (device %q) cannot be re-asserted on the BMC's %q account "+
				"(its username is %q, its password is %s); fix the entry, or pass --rotate-monitor to replace it with a freshly generated password",
			path, dev.Serial, dev.Name, bmcreconcile.MonitorUser, entry.Username, presence(entry.Password)))
	}
	return &model.MonitorRuntime{Password: entry.Password}, nil
}

// presence describes whether a secret value is set, without revealing it.
func presence(secret string) string {
	if secret == "" {
		return "empty"
	}
	return "set"
}

// routeSlugForDevice resolves the site-worker (config) slug that owns a
// device, for routing a synchronous BMC RPC to the connected worker. The WS
// hub indexes workers by their site-CONFIG slug (the `--slug` a worker
// registered with), which for a location-scoped site differs from the
// device's NetBox site slug (e.g. config pop2-old-cage, netbox_site pop2).
// Routing on dev.Site.Slug therefore misses the connection; resolve
// the owning config slug instead, shared with the snapshot/reconcile claim
// logic. Returns an HTTP error ready to bubble up.
func (s *Server) routeSlugForDevice(ctx context.Context, action string, dev *netbox.Device) (string, error) {
	slug, err := snapshot.ResolveOwningConfigSlug(ctx, s.netbox, s.store, *dev)
	if err != nil {
		if errors.Is(err, snapshot.ErrNoOwningConfig) {
			return "", echo.NewHTTPError(http.StatusServiceUnavailable,
				fmt.Sprintf("cannot route %s: no site_config (worker) claims this machine's NetBox device (site %q, location-scoped sites need a matching netbox_location); check `slagctl worker list`", action, dev.Site.Slug))
		}
		return "", echo.NewHTTPError(http.StatusServiceUnavailable,
			fmt.Sprintf("cannot route %s: resolving the owning site worker failed: %v", action, err))
	}
	return slug, nil
}

// PowerStatusResponse is the wire shape of GET /machines/:id/power.
// Transport names what answered the read ("redfish"/"ipmi"/vendor
// provider name); slagctl renders both for the operator.
type PowerStatusResponse struct {
	MachineID uuid.UUID `json:"machine_id"`
	State     string    `json:"state"`
	Transport string    `json:"transport"`
	// Hint is an optional operator-facing note from the site worker
	// (e.g. install SMCIPMITool for this Supermicro host).
	Hint string `json:"hint,omitempty"`
}

// powerStatusTimeout bounds the whole status read: NetBox resolve +
// WS RPC + the site worker's BMC read. Long enough for a slow AMI
// BMC (the worker's own budget is 12s), short enough that slagctl
// gets a real error instead of hanging.
const powerStatusTimeout = 15 * time.Second

// powerStatusMachine is the read path of the power surface: no op
// row, no state transition. It resolves bmc_runtime exactly like the
// actuation verbs (same 422 on no-credential), then synchronously
// RPCs a connected site worker for the machine's site over the WS
// hub and returns the BMC-reported state.
func (s *Server) powerStatusMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	if s.powerStatusRPC == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"power status requires the site-worker WebSocket hub, which is not configured on this server")
	}
	if _, err := s.store.GetMachine(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), powerStatusTimeout)
	defer cancel()

	// Transport follows the credential kind (plan D1): an AMT machine's
	// status is read over WS-Man, never IPMI/Redfish. Exactly one of the
	// two runtime blocks rides the RPC.
	via, err := s.resolveVia(ctx, id, "")
	if err != nil {
		return httpResolveErr(err)
	}
	var (
		dev              *netbox.Device
		bmcJSON, amtJSON json.RawMessage
	)
	if via == viaAMT {
		var amtRT model.AMTRuntime
		amtRT, dev, err = s.resolveAMTRuntimeWithDevice(ctx, id)
		if err != nil {
			return httpResolveErr(err)
		}
		amtJSON, err = json.Marshal(amtRT) //nolint:gosec // intentional: amt_runtime creds travel to the site worker over the authed WS, same shape as op params
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "marshal amt_runtime: "+err.Error())
		}
	} else {
		var (
			rt       model.BMCRuntime
			credNote string
		)
		rt, dev, credNote, err = s.resolveBMCRuntimeWithDevice(ctx, id)
		if err != nil {
			return httpResolveErr(err)
		}
		if credNote != "" {
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				noBMCCredMsg("power status", credNote))
		}
		bmcJSON, err = json.Marshal(rt) //nolint:gosec // intentional: bmc_runtime creds travel to the site worker over the authed WS, same shape as op params
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "marshal bmc_runtime: "+err.Error())
		}
	}
	if dev == nil || dev.Site == nil || dev.Site.Slug == "" {
		// Routing needs the site slug — without NetBox (or with an
		// unbound device) we don't know which site worker owns the BMC.
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"cannot route power status: the machine's NetBox device has no site, so no site worker can be selected")
	}

	routeSlug, err := s.routeSlugForDevice(ctx, "power status", dev)
	if err != nil {
		return err
	}

	resp, err := s.powerStatusRPC.PowerStatus(ctx, routeSlug, id, bmcJSON, amtJSON)
	if err != nil {
		if errors.Is(err, ws.ErrNoWorkerForSite) {
			return echo.NewHTTPError(http.StatusServiceUnavailable,
				fmt.Sprintf("no site worker is connected for site-config %q; check `slagctl worker list` and the slagworksd-site service on the oob-gw hosts", routeSlug))
		}
		return echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("power status read failed: %v", err))
	}
	return c.JSON(http.StatusOK, PowerStatusResponse{
		MachineID: id,
		State:     resp.State,
		Transport: resp.Transport,
		Hint:      resp.Hint,
	})
}

// BootStatusResponse is the GET /machines/:id/boot reply. Supported is
// false when the machine's BMC controller doesn't implement the boot
// capability (only the SMCIPMITool controller does, on Supermicro hosts);
// the other fields are then zero and the CLI prints "not supported on this
// host", plus Hint when installing SMCIPMITool would make it supported.
type BootStatusResponse struct {
	MachineID  uuid.UUID `json:"machine_id"`
	Supported  bool      `json:"supported"`
	Device     string    `json:"device"`
	Persistent bool      `json:"persistent"`
	Valid      bool      `json:"valid"`
	Raw        string    `json:"raw"`
	Transport  string    `json:"transport"`
	Hint       string    `json:"hint,omitempty"`
}

// bootStatusMachine reads a machine's BMC boot-device override. Mirrors
// powerStatusMachine: resolve creds, route by NetBox site, RPC the worker.
// A read — no op row, no chassis-serial gate (the worker skips it too).
func (s *Server) bootStatusMachine(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	if s.powerStatusRPC == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"boot status requires the site-worker WebSocket hub, which is not configured on this server")
	}
	if _, err := s.store.GetMachine(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "machine not found")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), powerStatusTimeout)
	defer cancel()

	// AMT has no boot-override read (it only arms a one-time PXE on
	// commission/deploy/power ops), so an AMT machine answers
	// Supported:false without touching the worker or its endpoint.
	via, err := s.resolveVia(ctx, id, "")
	if err != nil {
		return httpResolveErr(err)
	}
	if via == viaAMT {
		return c.JSON(http.StatusOK, BootStatusResponse{MachineID: id, Supported: false, Transport: viaAMT})
	}

	rt, dev, credNote, err := s.resolveBMCRuntimeWithDevice(ctx, id)
	if err != nil {
		return httpResolveErr(err)
	}
	if credNote != "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			noBMCCredMsg("boot status", credNote))
	}
	if dev == nil || dev.Site == nil || dev.Site.Slug == "" {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"cannot route boot status: the machine's NetBox device has no site, so no site worker can be selected")
	}

	routeSlug, err := s.routeSlugForDevice(ctx, "boot status", dev)
	if err != nil {
		return err
	}

	rtJSON, err := json.Marshal(rt) //nolint:gosec // intentional: bmc_runtime creds travel to the site worker over the authed WS, same shape as op params
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "marshal bmc_runtime: "+err.Error())
	}
	resp, err := s.powerStatusRPC.BootStatus(ctx, routeSlug, id, rtJSON)
	if err != nil {
		if errors.Is(err, ws.ErrNoWorkerForSite) {
			return echo.NewHTTPError(http.StatusServiceUnavailable,
				fmt.Sprintf("no site worker is connected for site-config %q; check `slagctl worker list` and the slagworksd-site service on the oob-gw hosts", routeSlug))
		}
		return echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("boot status read failed: %v", err))
	}
	return c.JSON(http.StatusOK, BootStatusResponse{
		MachineID:  id,
		Supported:  resp.Supported,
		Device:     resp.Device,
		Persistent: resp.Persistent,
		Valid:      resp.Valid,
		Raw:        resp.Raw,
		Transport:  resp.Transport,
		Hint:       resp.Hint,
	})
}

// --- Operation handlers ---

func (s *Server) listOperations(c *echo.Context) error {
	var machineID *uuid.UUID
	if v := c.QueryParam("machine"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid machine id")
		}
		machineID = &id
	}
	ops, err := s.store.ListOperations(machineID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := s.enrichOperationMachineNames(c.Request().Context(), ops); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("machine name enrichment: %v", err))
	}
	return c.JSON(http.StatusOK, ops)
}

// enrichOperationMachineNames populates the transient Operation.MachineName
// field so `slagctl operation list` / `machine ops` can show a human machine
// name instead of a raw UUID. The name is the machine's NetBox hostname,
// falling back to its serial when NetBox has no name (or isn't configured).
// Best-effort on the NetBox leg: a NetBox failure leaves names at their serial
// fallback rather than failing the list. A Postgres failure is fatal (the same
// store that produced the ops just became unreadable).
func (s *Server) enrichOperationMachineNames(ctx context.Context, ops []model.Operation) error {
	if len(ops) == 0 {
		return nil
	}
	// One machine lookup for the distinct set of machine IDs across the ops.
	seen := make(map[uuid.UUID]struct{}, len(ops))
	ids := make([]uuid.UUID, 0, len(ops))
	for _, op := range ops {
		if op.MachineID == uuid.Nil {
			continue
		}
		if _, ok := seen[op.MachineID]; ok {
			continue
		}
		seen[op.MachineID] = struct{}{}
		ids = append(ids, op.MachineID)
	}
	if len(ids) == 0 {
		return nil
	}
	machines, err := s.store.ListMachinesByIDs(ids)
	if err != nil {
		return err
	}
	// Enrich hostnames from NetBox best-effort; on failure fall through with
	// serial-only names so the list still renders something human-readable.
	_ = s.enrichMachineHostnames(ctx, machines)

	nameByID := make(map[uuid.UUID]string, len(machines))
	for _, m := range machines {
		switch {
		case m.Hostname != "":
			nameByID[m.ID] = m.Hostname
		case m.NetBoxSerial != "":
			nameByID[m.ID] = m.NetBoxSerial
		}
	}
	for i := range ops {
		if name, ok := nameByID[ops[i].MachineID]; ok {
			ops[i].MachineName = name
		}
	}
	return nil
}

func (s *Server) getOperation(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid operation id")
	}
	op, err := s.store.GetOperation(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "operation not found")
	}
	return c.JSON(http.StatusOK, op)
}

// --- Image handlers ---

func (s *Server) listImages(c *echo.Context) error {
	images, err := s.store.ListImages()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, images)
}

func (s *Server) createImage(c *echo.Context) error {
	var img model.Image
	if err := c.Bind(&img); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if img.Name == "" || img.Version == "" || img.SHA256 == "" || img.URL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name, version, sha256, and url are required")
	}
	if err := s.store.CreateImage(&img); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	// Image rows are global (every site's snapshot carries the image list),
	// so a new image must push a fresh snapshot to all site workers — their
	// imagepull.SyncImages runs on snapshot apply. Without this, a newly
	// registered image only reaches workers on the next UNRELATED invalidation
	// (an op, a webhook), which is why pinning an image used to require firing
	// a throwaway deploy to "trigger" the sync.
	s.invalidateAllSites()
	return c.JSON(http.StatusCreated, img)
}

// upsertImage creates or updates the image row matching :name. Used by
// `slagctl image follow-latest` to repoint a row at the current
// `<git-sha>.tar.gz` on S3 without caring whether the row already
// exists. The :name path param wins over any name in the request body
// to avoid a mismatch where the path says one row and the payload
// updates another.
func (s *Server) upsertImage(c *echo.Context) error {
	var img model.Image
	if err := c.Bind(&img); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	img.Name = c.Param("name")
	if img.Name == "" || img.Version == "" || img.SHA256 == "" || img.URL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name (in path), version, sha256, and url are required")
	}
	if err := s.store.UpsertImage(&img); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Repointing the image row (the `follow-latest` path) must propagate to
	// every site worker immediately — push a fresh snapshot to all sites so
	// their imagepull.SyncImages pulls the new tarball on the next apply,
	// instead of waiting for an unrelated invalidation. This is what makes
	// `slagctl image follow-latest` actually roll the image to the fleet.
	s.invalidateAllSites()
	return c.JSON(http.StatusOK, img)
}

// deleteImage removes an image row by name (or, if the caller resolved a
// uuid client-side, that also works since DeleteImage accepts either). No
// invalidateAllSites call: workers only need to pick this up on their
// normal snapshot-apply cadence; a delete has no equivalent urgency to
// createImage's fleet-rollout trigger (nothing to distribute).
func (s *Server) deleteImage(c *echo.Context) error {
	name := c.Param("name")
	if err := s.store.DeleteImage(name); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Storage scripts ---

func (s *Server) listStorageScripts(c *echo.Context) error {
	scripts, err := s.store.ListStorageScripts()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, scripts)
}

func (s *Server) createStorageScript(c *echo.Context) error {
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.Name == "" || body.Content == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name and content are required")
	}
	parsed, err := storage.ParseScript(body.Name, []byte(body.Content))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse script headers: "+err.Error())
	}
	script := model.StorageScript{Name: body.Name, Priority: parsed.Priority, Content: body.Content}
	if err := s.store.CreateStorageScript(&script); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusCreated, script)
}

// scriptWithVersion is the update-response shape: the head read-model row
// plus the version number the update just minted.
type scriptWithVersion struct {
	model.StorageScript
	Version int `json:"version"`
}

type networkScriptWithVersion struct {
	model.NetworkScript
	Version int `json:"version"`
}

// updateStorageScript inserts a new immutable version (head+1) with content
// re-parsed for priority, and advances the script's head pointer. Old
// versions are never touched. No snapshot invalidation here, matching
// createStorageScript/deleteStorageScript: matching picks the change up on
// the next rebuild rather than forcing one.
func (s *Server) updateStorageScript(c *echo.Context) error {
	name := c.Param("name")
	var body struct {
		Content string `json:"content"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.Content == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "content is required")
	}
	parsed, err := storage.ParseScript(name, []byte(body.Content))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse script headers: "+err.Error())
	}
	script, version, err := s.store.UpdateStorageScriptByName(name, parsed.Priority, body.Content)
	if errors.Is(err, store.ErrScriptNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "updating storage script")
	}
	return c.JSON(http.StatusOK, scriptWithVersion{StorageScript: *script, Version: version})
}

func (s *Server) deleteStorageScript(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	if err := s.store.DeleteStorageScript(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// storageScriptHistory lists every version of a storage script, oldest
// first, with the head version marked. A read: no op row, no mutation.
func (s *Server) storageScriptHistory(c *echo.Context) error {
	versions, err := s.store.ListStorageScriptVersions(c.Param("name"))
	if errors.Is(err, store.ErrScriptNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "listing storage script history")
	}
	return c.JSON(http.StatusOK, versions)
}

// --- Network scripts ---

func (s *Server) listNetworkScripts(c *echo.Context) error {
	scripts, err := s.store.ListNetworkScripts()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, scripts)
}

func (s *Server) createNetworkScript(c *echo.Context) error {
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.Name == "" || body.Content == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name and content are required")
	}
	parsed, err := snapshot.ParseNetworkScript(body.Name, []byte(body.Content))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse script headers: "+err.Error())
	}
	script := model.NetworkScript{Name: body.Name, Priority: parsed.Priority, Content: body.Content}
	if err := s.store.CreateNetworkScript(&script); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusCreated, script)
}

// updateNetworkScript is the network-script twin of updateStorageScript.
func (s *Server) updateNetworkScript(c *echo.Context) error {
	name := c.Param("name")
	var body struct {
		Content string `json:"content"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.Content == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "content is required")
	}
	parsed, err := snapshot.ParseNetworkScript(name, []byte(body.Content))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse script headers: "+err.Error())
	}
	script, version, err := s.store.UpdateNetworkScriptByName(name, parsed.Priority, body.Content)
	if errors.Is(err, store.ErrScriptNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "updating network script")
	}
	return c.JSON(http.StatusOK, networkScriptWithVersion{NetworkScript: *script, Version: version})
}

func (s *Server) deleteNetworkScript(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	if err := s.store.DeleteNetworkScript(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// networkScriptHistory is the network-script twin of storageScriptHistory.
func (s *Server) networkScriptHistory(c *echo.Context) error {
	versions, err := s.store.ListNetworkScriptVersions(c.Param("name"))
	if errors.Is(err, store.ErrScriptNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "listing network script history")
	}
	return c.JSON(http.StatusOK, versions)
}

// --- SSH keys ---

func (s *Server) listSSHKeys(c *echo.Context) error {
	keys, err := s.store.ListSSHKeys()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, keys)
}

func (s *Server) createSSHKey(c *echo.Context) error {
	var body struct {
		PublicKey string `json:"public_key"`
		Comment   string `json:"comment"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.PublicKey == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "public_key is required")
	}
	if _, _, _, _, err := cryptossh.ParseAuthorizedKey([]byte(body.PublicKey)); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid SSH public key: "+err.Error())
	}
	key := model.SSHKey{PublicKey: body.PublicKey, Comment: body.Comment}
	if err := s.store.CreateSSHKey(&key); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusCreated, key)
}

func (s *Server) deleteSSHKey(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	if err := s.store.DeleteSSHKey(id); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Site configs ---

func (s *Server) listSiteConfigs(c *echo.Context) error {
	cfgs, err := s.store.ListSiteConfigs()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, cfgs)
}

func (s *Server) getSiteConfig(c *echo.Context) error {
	cfg, err := s.store.GetSiteConfig(c.Param("slug"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "site_config not found")
	}
	return c.JSON(http.StatusOK, cfg)
}

// dhcpLeasesTimeout bounds the whole dhcp-leases read: NetBox/store
// lookups are none (routing here is by slug directly, no per-machine
// NetBox resolve), so this is purely the WS fan-out RPC's budget. Long
// enough for every connected worker's in-memory map walk (microseconds)
// plus WS round-trip latency; short enough that a wedged worker doesn't
// hang the request past what an operator will wait on.
const dhcpLeasesTimeout = 10 * time.Second

// DHCPLeasesWorkerResult is one worker's entry in the dhcp-leases
// response. Mirrors ws.WorkerDHCPLeases field-for-field; kept as a
// distinct wire type (rather than reusing ws.WorkerDHCPLeases
// directly) so the api package's response shape isn't silently
// coupled to the ws package's internal RPC bookkeeping type.
type DHCPLeasesWorkerResult struct {
	WorkerID string             `json:"worker_id"`
	FQDN     string             `json:"fqdn,omitempty"`
	Error    string             `json:"error,omitempty"`
	Leases   []ws.DHCPLeaseInfo `json:"leases"`
}

// DHCPLeasesResponse is the wire shape of GET
// /site-configs/:slug/dhcp-leases. Workers is always a non-nil slice
// (empty when no worker is connected) so slagctl doesn't need to
// distinguish "null" from "no workers".
type DHCPLeasesResponse struct {
	Workers []DHCPLeasesWorkerResult `json:"workers"`
}

// dhcpLeasesSiteConfig is the read path for a site's live DHCP lease
// state: no op row, no state transition. It fans a dhcp_leases RPC to
// every connected worker for the slug over the WS hub (an HA cage's
// two workers each hold their own dynamic slice's leases, so both
// matter) and returns each worker's answer, including per-worker
// errors — a dead/timed-out worker never fails the whole read.
func (s *Server) dhcpLeasesSiteConfig(c *echo.Context) error {
	slug := c.Param("slug")
	if _, err := s.store.GetSiteConfig(slug); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("site_config %q not found", slug))
	}
	if s.powerStatusRPC == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"dhcp leases requires the site-worker WebSocket hub, which is not configured on this server")
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), dhcpLeasesTimeout)
	defer cancel()

	results := s.powerStatusRPC.DHCPLeases(ctx, slug)
	resp := DHCPLeasesResponse{Workers: make([]DHCPLeasesWorkerResult, 0, len(results))}
	for _, r := range results {
		leases := r.Leases
		if leases == nil {
			leases = []ws.DHCPLeaseInfo{}
		}
		resp.Workers = append(resp.Workers, DHCPLeasesWorkerResult{
			WorkerID: r.WorkerID,
			FQDN:     r.FQDN,
			Error:    r.Err,
			Leases:   leases,
		})
	}
	return c.JSON(http.StatusOK, resp)
}

// dhcpLeaseHistorySiteConfig is the ledger read (Phase 3 of
// docs/plans/2026-07-13-dhcp-lease-visibility.md): unlike
// dhcpLeasesSiteConfig this never touches a live worker — it's a
// plain store query, so it answers even with every site worker
// offline. ?mac= and ?ip= are optional filters; ?limit= defaults to
// 100 and is capped at 1000 by the store layer.
func (s *Server) dhcpLeaseHistorySiteConfig(c *echo.Context) error {
	slug := c.Param("slug")
	if _, err := s.store.GetSiteConfig(slug); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("site_config %q not found", slug))
	}

	limit := 0
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid limit %q: %v", v, err))
		}
		limit = n
	}

	rows, err := s.store.ListDHCPLeaseHistory(c.Request().Context(), slug, c.QueryParam("mac"), c.QueryParam("ip"), limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("querying lease history: %v", err))
	}
	return c.JSON(http.StatusOK, DHCPLeaseHistoryResponse{Entries: rows})
}

// DHCPLeaseHistoryResponse is the wire shape of GET
// /site-configs/:slug/dhcp-lease-history.
type DHCPLeaseHistoryResponse struct {
	Entries []model.DHCPLeaseLedgerEntry `json:"entries"`
}

// MonitorBootstrapper creates a pop's BMC monitor secret if absent. The
// *awssecrets.Client satisfies it (EnsureMonitorSecret). Defined as a narrow
// interface so the API package depends only on the one method it calls and
// tests can inject a fake.
type MonitorBootstrapper interface {
	EnsureMonitorSecret(ctx context.Context, pop string) error
}

func (s *Server) createSiteConfig(c *echo.Context) error {
	var cfg model.SiteConfig
	if err := c.Bind(&cfg); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	// Backfill netbox_site from the slug for the legacy whole-site shape:
	// a caller that omits netbox_site is creating a pop1/pop3-style config
	// where the slug IS the NetBox site slug. (slagctl always sets it
	// explicitly; this keeps the raw API forgiving.)
	if cfg.NetBoxSite == "" {
		cfg.NetBoxSite = cfg.Slug
	}
	if herr := s.validateSiteConfigNetBox(c, &cfg); herr != nil {
		return herr
	}
	if err := s.store.CreateSiteConfig(&cfg); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	// Bootstrap the pop's AWS dependencies that slagworks needs but can't run
	// without and that aren't created elsewhere. Today that's the per-pop BMC
	// monitor secret (pop/<pop>/bmc-creds-monitor): the deploy_complete handler
	// upserts each host's monitor cred into it, and a missing secret made that
	// fail-replay forever. Create the _meta-only stub now so the first
	// deploy in this pop doesn't hit that. The pop is the NetBox site slug
	// (the same key deploy_complete uses, dev.Site.Slug). Best-effort: a
	// failure here is logged but does NOT fail site-config create — preflight
	// independently refuses a deploy whose monitor secret is missing, so a
	// bootstrap miss surfaces as a clear pre-deploy gap, not a silent hole.
	// (The admin secret is intentionally NOT created here: it holds real BMC
	// creds managed by the rotation tooling, not slagworks's to mint.)
	if s.monitorBootstrap != nil {
		if err := s.monitorBootstrap.EnsureMonitorSecret(c.Request().Context(), cfg.NetBoxSite); err != nil {
			s.log.Warn("site-config create: failed to bootstrap monitor secret; deploys will be refused by preflight until it exists",
				"slug", cfg.Slug, "pop", cfg.NetBoxSite, "err", err)
		} else {
			s.log.Info("site-config create: ensured BMC monitor secret", "pop", cfg.NetBoxSite)
		}
	}

	s.invalidateSite(cfg.Slug)
	return c.JSON(http.StatusCreated, cfg)
}

// validateSiteConfigNetBox runs the NetBox-backed preconditions for a
// create or update: the netbox_site must exist; the config's scope
// (whole-site or its location) must have a slagworks-production prefix
// so the first snapshot build can't fail with "no production prefix";
// a location-scoped config's location must belong to the site; and the
// config must not overlap an existing config on the same site (ruling 2
// create/update half). Returns a *echo.HTTPError on failure, nil on
// success. No-op when NetBox isn't wired (tests).
func (s *Server) validateSiteConfigNetBox(c *echo.Context, cfg *model.SiteConfig) *echo.HTTPError {
	if s.netbox == nil {
		return nil
	}
	ctx := c.Request().Context()
	site, err := s.netbox.GetSiteBySlug(ctx, cfg.NetBoxSite)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "netbox: "+err.Error())
	}
	if site == nil {
		return echo.NewHTTPError(http.StatusPreconditionFailed,
			fmt.Sprintf("netbox site %q not found; create the site in NetBox first", cfg.NetBoxSite))
	}

	locations, err := s.netbox.ListLocationsForSite(ctx, cfg.NetBoxSite)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "netbox: "+err.Error())
	}
	// Belongs-to: a location-scoped config's location must be in the site.
	if cfg.LocationScoped() {
		found := false
		var locID int
		for _, l := range locations {
			if l.Slug == cfg.NetBoxLocation {
				found, locID = true, l.ID
				break
			}
		}
		if !found {
			return echo.NewHTTPError(http.StatusPreconditionFailed,
				fmt.Sprintf("netbox location %q not found in site %q", cfg.NetBoxLocation, cfg.NetBoxSite))
		}
		prefixes, err := s.netbox.ListLocationPrefixes(ctx, locID, "slagworks-production")
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "netbox: "+err.Error())
		}
		if len(prefixes) == 0 {
			return echo.NewHTTPError(http.StatusPreconditionFailed,
				fmt.Sprintf("netbox location %q has no prefix tagged 'slagworks-production' scoped to it; tag+scope one before site-config create", cfg.NetBoxLocation))
		}
	} else {
		prefixes, err := s.netbox.ListSitePrefixes(ctx, cfg.NetBoxSite, "slagworks-production")
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "netbox: "+err.Error())
		}
		if len(prefixes) == 0 {
			return echo.NewHTTPError(http.StatusPreconditionFailed,
				fmt.Sprintf("netbox site %q has no prefix tagged 'slagworks-production'; create one (with gateway_ip custom field) before site-config create", cfg.NetBoxSite))
		}
	}

	// Overlap: reject a config that could claim the same devices as an
	// existing one on this site.
	existing, err := s.store.ListSiteConfigs()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := snapshot.CheckOverlap(*cfg, existing, locations); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return nil
}

func (s *Server) updateSiteConfig(c *echo.Context) error {
	cfg, err := s.store.GetSiteConfig(c.Param("slug"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "site_config not found")
	}
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if v, ok := body["netbox_site"].(string); ok {
		cfg.NetBoxSite = v
	}
	if v, ok := body["netbox_location"].(string); ok {
		cfg.NetBoxLocation = v
	}
	if v, ok := body["ephemeral_pool_start"].(string); ok {
		cfg.EphemeralPoolStart = v
	}
	if v, ok := body["ephemeral_pool_end"].(string); ok {
		cfg.EphemeralPoolEnd = v
	}
	if v, ok := body["promiscuous"].(bool); ok {
		cfg.Promiscuous = v
	}
	if raw, ok := body["relay_pools"]; ok {
		// Full-replace semantics: the request's relay_pools list becomes
		// the site's list (empty list clears). Round-trip through JSON to
		// get typed structs out of the map-shaped body.
		b, err := json.Marshal(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "relay_pools: "+err.Error())
		}
		var pools []model.RelayPool
		if err := json.Unmarshal(b, &pools); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "relay_pools: "+err.Error())
		}
		cfg.RelayPools = pools
	}
	// Re-run NetBox preconditions (scope existence, prefix, overlap) on
	// the merged config — an update that changes netbox_site/location
	// must satisfy the same invariants a create does. CheckOverlap skips
	// this config's own row by slug, so a no-scope-change update passes.
	if herr := s.validateSiteConfigNetBox(c, cfg); herr != nil {
		return herr
	}
	if err := s.store.UpdateSiteConfig(cfg); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	s.invalidateSite(cfg.Slug)
	return c.JSON(http.StatusOK, cfg)
}

func (s *Server) deleteSiteConfig(c *echo.Context) error {
	if err := s.store.DeleteSiteConfig(c.Param("slug")); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Workers (read-only) ---

// WorkerListEntry is one row of GET /workers: the registration row plus
// live state the WS hub knows for connected workers.
type WorkerListEntry struct {
	model.SiteWorker
	// SMCIPMITool is the connected worker's reported Supermicro
	// SMCIPMITool status ("installed", "missing", "missing-required",
	// "disabled"); "" when the
	// worker is offline or didn't report it.
	SMCIPMITool string `json:"smcipmitool,omitempty"`
	// SupermicroMachines is how many Supermicro hosts the worker's site
	// has — i.e. whether SMCIPMITool matters on this worker.
	SupermicroMachines int `json:"supermicro_machines,omitempty"`
}

func (s *Server) listWorkers(c *echo.Context) error {
	workers, err := s.store.ListSiteWorkers("")
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]WorkerListEntry, len(workers))
	counts := map[string]int{}
	for i, w := range workers {
		out[i] = WorkerListEntry{SiteWorker: w}
		if s.workerTools == nil {
			continue
		}
		out[i].SMCIPMITool, _ = s.workerTools.WorkerSMCIPMITool(w.SiteSlug, w.WorkerID)
		n, ok := counts[w.SiteSlug]
		if !ok {
			n = s.workerTools.SupermicroMachineCount(w.SiteSlug)
			counts[w.SiteSlug] = n
		}
		out[i].SupermicroMachines = n
	}
	return c.JSON(http.StatusOK, out)
}

// registerSiteWorker mints a new bearer token for a (site_slug, worker_id) pair,
// returning the plaintext token in the response exactly once. Re-registration
// rotates the token. The operator hands the plaintext to the site worker via
// its SLAGWORKS_TOKEN env var.
func (s *Server) registerSiteWorker(c *echo.Context) error {
	var req struct {
		SiteSlug string `json:"site_slug"`
		WorkerID string `json:"worker_id"`
		FQDN     string `json:"fqdn"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if req.SiteSlug == "" || req.WorkerID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "site_slug and worker_id required")
	}
	w, token, err := s.store.RegisterSiteWorker(req.SiteSlug, req.WorkerID, req.FQDN)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"id":            w.ID,
		"site_slug":     w.SiteSlug,
		"worker_id":     w.WorkerID,
		"fqdn":          w.FQDN,
		"first_seen_at": w.FirstSeenAt,
		"token":         token,
	})
}

// deleteSiteWorker removes a site worker's registration row. Refuses
// (409) if the worker is currently connected over WS — deleting the row
// authenticating a live connection would strand it — or if any
// non-terminal operation is claimed by it — deleting the row that
// authenticates the claiming worker would orphan the claim. Motivated by
// the router1.pop3 teardown, which needed a raw DB delete because this
// verb didn't exist.
func (s *Server) deleteSiteWorker(c *echo.Context) error {
	ident := c.Param("ident")
	w, err := s.store.ResolveSiteWorker(ident)
	if errors.Is(err, store.ErrSiteWorkerNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	var ambiguous *store.AmbiguousSiteWorkerError
	if errors.As(err, &ambiguous) {
		return echo.NewHTTPError(http.StatusConflict, ambiguous.Error())
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if s.workerLiveness != nil && s.workerLiveness.IsWorkerConnected(w.SiteSlug, w.WorkerID) {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("site worker %s/%s is currently connected; disconnect it before deleting", w.SiteSlug, w.WorkerID))
	}
	active, err := s.store.ListActiveOperationsClaimedBy(w.WorkerID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if len(active) > 0 {
		ids := make([]string, len(active))
		for i, op := range active {
			ids[i] = op.ID.String()
		}
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("site worker %s/%s has in-flight operations claimed: %s", w.SiteSlug, w.WorkerID, strings.Join(ids, ", ")))
	}
	if err := s.store.DeleteSiteWorker(w.ID); err != nil {
		// Only a concurrent delete between ResolveSiteWorker and here yields
		// not-found; any other error is a real store failure, not a 404.
		if errors.Is(err, store.ErrSiteWorkerNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// fallbackHealthz is used only when the server is constructed without
// a Readiness (tests, bring-up before the reconciler is wired). It
// reports Postgres reachability; no snapshot-pipeline visibility. The
// production wiring uses RegisterHealthz + the reconciler.
func (s *Server) fallbackHealthz(c *echo.Context) error {
	sqlDB, err := s.store.DB().DB()
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"postgres": "no connection pool"})
	}
	if err := sqlDB.PingContext(c.Request().Context()); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"postgres": "unreachable"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"status":      "ready",
		"version":     s.version,
		"instance_id": s.instanceID,
	})
}
