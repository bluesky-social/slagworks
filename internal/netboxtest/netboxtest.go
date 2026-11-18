//go:build integration

// Package netboxtest spins a per-test integration environment: a
// NetBox container (backed by Postgres + Redis) plus a separate
// slagworks Postgres, all via dockertest.
//
// Usage:
//
//	func TestThing(t *testing.T) {
//	    env := netboxtest.New(t)
//	    site := env.SeedSite("pop")
//	    // ... mirror/snapshot/commission against env.NetBox + env.Store
//	}
//
// All containers live for the duration of the test and are torn down
// via t.Cleanup.
package netboxtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	mobynetwork "github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Constants carried over from the deleted QEMU harness's netbox.go.
// These must match whatever pepper the fixture dumps were produced
// with.
const (
	netboxTag     = "v4.5.4-4.0.1"
	netboxSecret  = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop"
	fixturePepper = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP"
	fixtureToken  = "nbt_x7nkTxVLDuGQ.B499uwAZ8wNLzovGJZN3jhchee6JnUdnbg7okWhj"
)

// Env is a running integration test environment. The containers are
// owned by this Env — they live until t.Cleanup fires.
type Env struct {
	NetBox *netbox.Client
	Store  *store.Store
	Log    *slog.Logger

	t            *testing.T
	namespace    string
	defaults     *envDefaults // lazy NetBox object cache; see seed.go
	docker       *mobyclient.Client
	netboxRes    dockertest.Resource
	workerRes    dockertest.Resource
	receiverHost string
}

// stack is the Docker plumbing every container of one test's
// environment shares.
type stack struct {
	pool    dockertest.Pool
	docker  *mobyclient.Client
	network dockertest.Network
}

// run starts a fresh container from repository:tag on the stack's
// network, pulling the image with the mirror's creds first. extraHosts
// adds /etc/hosts entries. The pool removes the container when the test
// ends; Expire bounds its life if the test process dies first.
func (s *stack) run(ctx context.Context, repository, tag string, extraHosts []string, opts ...dockertest.RunOption) (dockertest.Resource, error) {
	if err := pullMirrored(ctx, s.docker, repository, tag); err != nil {
		return nil, err
	}
	opts = append(opts,
		dockertest.WithTag(tag),
		// dockertest otherwise hands back any container of the same
		// repository:tag this process already started, so both Postgres
		// containers would be one.
		dockertest.WithoutReuse(),
		dockertest.WithHostConfig(func(config *container.HostConfig) {
			config.NetworkMode = container.NetworkMode(s.network.Inspect().Name)
			config.ExtraHosts = extraHosts
			config.AutoRemove = true
			config.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyDisabled}
		}))
	res, err := s.pool.Run(ctx, repository, opts...)
	if err != nil {
		return nil, err
	}
	testpg.Expire(s.docker, res.ID(), 20*time.Minute)
	return res, nil
}

// New brings up the baseline fixture: fresh NetBox + Postgres + Redis
// stack plus a slagworks Postgres, all as per-test dockertest
// containers. Fails the test if Docker isn't available or bringup
// takes too long.
func New(t *testing.T) *Env {
	t.Helper()
	return newWithFixture(t, "baseline", false)
}

// NewWithWebhookWorker is New plus the NetBox RQ worker required to
// execute queued webhook deliveries. Most integration tests do not need
// the extra container, so webhook delivery is deliberately opt-in.
func NewWithWebhookWorker(t *testing.T) *Env {
	t.Helper()
	return newWithFixture(t, "baseline", true)
}

// NewWithFixture is like New but uses the named fixture. Currently
// only "baseline" exists. The slot is kept open so future fixtures
// (e.g. a regenerated prod-shaped dump) can be plumbed through
// without reshaping the API.
func NewWithFixture(t *testing.T, fixture string) *Env {
	t.Helper()
	return newWithFixture(t, fixture, false)
}

// Namespace returns a slug scoped to this test. Used by seed helpers
// so leftover objects from a crashed prior run don't collide with a
// fresh run against the same NetBox. Defensive given per-test stacks
// mean cross-test collisions are structurally impossible.
func (e *Env) Namespace(s string) string {
	return s + "-" + e.namespace
}

// Cleanup is equivalent to t.Cleanup. Provided so seed helpers can
// register teardown via the Env they were called on.
func (e *Env) Cleanup(fn func()) {
	e.t.Cleanup(fn)
}

func newWithFixture(t *testing.T, fixture string, withWebhookWorker bool) *Env {
	t.Helper()

	log := slog.New(slog.NewTextHandler(&testLogWriter{t: t},
		&slog.HandlerOptions{Level: slog.LevelInfo}))

	docker, err := testpg.DockerClient(t.Context())
	if err != nil {
		t.Fatalf("netboxtest: connect to docker: %v", err)
	}
	// Registered before the pool so it runs after the pool's cleanup has
	// removed the containers and the network (t.Cleanup is LIFO).
	t.Cleanup(func() { _ = docker.Close() })
	// NewPoolT removes every container and network the pool created when
	// the test ends, containers first.
	pool := dockertest.NewPoolT(t, "",
		dockertest.WithMobyClient(docker),
		dockertest.WithMaxWait(3*time.Minute))

	safeName := sanitize(t.Name())

	// Per-test bridge network so NetBox can talk to its postgres/redis
	// over stable DNS names without colliding with other parallel tests.
	network, err := pool.CreateNetwork(t.Context(), "netboxtest-"+safeName, nil)
	if err != nil {
		t.Fatalf("netboxtest: create network: %v", err)
	}
	s := &stack{pool: pool, docker: docker, network: network}

	dumpPath, err := fixtureDumpPath(fixture)
	if err != nil {
		t.Fatalf("netboxtest: %v", err)
	}

	// --- slagworks Postgres (this test's Store backing) ---
	slagPG := mustRunPG(t, s, "slagpg-"+safeName, nil)
	slagDSN := fmt.Sprintf("host=localhost port=%s user=postgres password=test dbname=slagworks_test sslmode=disable",
		slagPG.GetPort("5432/tcp"))
	slagDB := mustConnect(t, pool, slagDSN)
	if err := store.Migrate(slagDB); err != nil {
		t.Fatalf("netboxtest: migrate slagworks: %v", err)
	}
	st := store.New(slagDB)

	// --- NetBox Postgres (dump pre-loaded) ---
	nbPG := mustRunPG(t, s, "nbpg-"+safeName, map[string]string{
		"POSTGRES_DB":       "netbox",
		"POSTGRES_USER":     "netbox",
		"POSTGRES_PASSWORD": "testpass",
	})
	if err := restoreDump(t.Context(), s, nbPG, dumpPath); err != nil {
		t.Fatalf("netboxtest: restore dump: %v", err)
	}

	// --- Redis ---
	redis, err := s.run(t.Context(), mirrored("library/redis"), "7-alpine", nil,
		dockertest.WithName("nbredis-"+safeName))
	if err != nil {
		t.Fatalf("netboxtest: start redis: %v", err)
	}

	// --- NetBox ---
	pgIP := nbPG.GetIPInNetwork(network)
	redisIP := redis.GetIPInNetwork(network)

	// The web process, the RQ worker that actually delivers webhooks, and
	// the fixture token all share this environment: same Postgres, same
	// Redis, same SECRET_KEY, and same API-token pepper (the fixture dump
	// tokens were minted with fixturePepper — without it the api_token
	// fixture can't be decoded and every client call 401s).
	netboxEnv := []string{
		"DB_HOST=" + pgIP,
		"DB_NAME=netbox",
		"DB_USER=netbox",
		"DB_PASSWORD=testpass",
		"REDIS_HOST=" + redisIP,
		"REDIS_PASSWORD=",
		"REDIS_DATABASE=0",
		"REDIS_CACHE_HOST=" + redisIP,
		"REDIS_CACHE_PASSWORD=",
		"REDIS_CACHE_DATABASE=1",
		"SECRET_KEY=" + netboxSecret,
		"API_TOKEN_PEPPER_1=" + fixturePepper,
		"ALLOWED_HOSTS=*",
		"GRANIAN_WORKERS=1",
	}

	nb, err := s.run(t.Context(), mirrored("netboxcommunity/netbox"), netboxTag, nil,
		dockertest.WithName("nbapp-"+safeName),
		dockertest.WithContainerConfig(func(config *container.Config) {
			config.ExposedPorts = mobynetwork.PortSet{mobynetwork.MustParsePort("8080/tcp"): {}}
		}),
		dockertest.WithCmd([]string{"/opt/netbox/launch-netbox.sh"}),
		dockertest.WithEnv(netboxEnv))
	if err != nil {
		t.Fatalf("netboxtest: start netbox: %v", err)
	}

	netboxURL := fmt.Sprintf("http://localhost:%s", nb.GetPort("8080/tcp"))
	if err := waitForNetBox(t.Context(), pool, netboxURL); err != nil {
		t.Fatalf("netboxtest: wait for netbox: %v", err)
	}

	var worker dockertest.Resource
	var workerHost string
	if withWebhookWorker {
		// --- NetBox RQ worker ---
		// NetBox only ENQUEUES webhook deliveries as background RQ jobs
		// (extras.webhooks.send_webhook runs on django-rq's 'default' queue);
		// the web process never executes them. A real delivery therefore needs
		// a worker, so start one from the same image against the same
		// Postgres/Redis/network/secret/pepper. It runs the venv rqworker
		// directly (bypassing docker-entrypoint.sh, exactly as the web
		// container bypasses it for launch-netbox.sh): the dump is already
		// migrated, so the entrypoint's migration/superuser steps are unneeded.
		workerExtraHosts := []string(nil)
		workerHost, workerExtraHosts = receiverRoute(network)
		worker, err = s.run(t.Context(), mirrored("netboxcommunity/netbox"), netboxTag, workerExtraHosts,
			dockertest.WithName("nbrq-"+safeName),
			dockertest.WithCmd([]string{
				"/opt/netbox/venv/bin/python",
				"/opt/netbox/netbox/manage.py",
				"rqworker", "default",
			}),
			dockertest.WithEnv(netboxEnv))
		if err != nil {
			t.Fatalf("netboxtest: start netbox rq worker: %v", err)
		}
		if err := waitForWorker(t.Context(), pool, redis, redisIP); err != nil {
			t.Fatalf("netboxtest: wait for netbox rq worker: %v\nworker logs:\n%s",
				err, containerLogs(docker, worker))
		}
	}

	client := netbox.NewClient(netbox.ClientConfig{
		BaseURL: netboxURL,
		Token:   fixtureToken,
		Timeout: 30 * time.Second,
		// Pin to the containerized NetBox's version (netboxTag). The client
		// default tracks PRODUCTION (4.6 as of 39d048a) and NetBox 406s an
		// Accept header naming a version the server doesn't speak — the
		// default flip broke this env until pinned. Keep in lockstep with
		// netboxTag (and waitForNetBox's health-check header) when bumping.
		APIVersion: "4.5",
	})

	// Bootstrap slagworks custom fields + tags against the fresh
	// NetBox. The baseline dump has no slagworks-specific state;
	// Bootstrap is idempotent in production so re-running it per test
	// is the same code path we use in production.
	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer bootstrapCancel()
	if _, err := client.Bootstrap(bootstrapCtx, netbox.BootstrapConfig{}); err != nil {
		t.Fatalf("netboxtest: bootstrap netbox: %v", err)
	}

	env := &Env{
		NetBox:       client,
		Store:        st,
		Log:          log,
		t:            t,
		namespace:    namespaceFor(t.Name()),
		defaults:     &envDefaults{},
		docker:       docker,
		netboxRes:    nb,
		workerRes:    worker,
		receiverHost: workerHost,
	}
	return env
}

func namespaceFor(testName string) string {
	h := sha256.Sum256([]byte(testName))
	return hex.EncodeToString(h[:3])
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func mustRunPG(t *testing.T, s *stack, name string, extraEnv map[string]string) dockertest.Resource {
	t.Helper()
	env := []string{"POSTGRES_PASSWORD=test"}
	if extraEnv == nil {
		env = append(env, "POSTGRES_DB=slagworks_test")
		env = append(env, "POSTGRES_USER=postgres")
	} else {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}

	res, err := s.run(t.Context(), mirrored("library/postgres"), "18-alpine", nil,
		dockertest.WithName(name),
		dockertest.WithEnv(env))
	if err != nil {
		t.Fatalf("netboxtest: start %s: %v", name, err)
	}
	return res
}

func mustConnect(t *testing.T, pool dockertest.Pool, dsn string) *gorm.DB {
	t.Helper()
	var db *gorm.DB
	if err := testpg.Retry(t.Context(), pool, func() error {
		var openErr error
		db, openErr = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if openErr != nil {
			return openErr
		}
		sqlDB, sqlErr := db.DB()
		if sqlErr != nil {
			return sqlErr
		}
		return sqlDB.Ping()
	}); err != nil {
		t.Fatalf("netboxtest: connect to postgres: %v", err)
	}
	return db
}

func restoreDump(ctx context.Context, s *stack, pg dockertest.Resource, dumpPath string) error {
	// Wait for the netbox database to accept queries.
	if err := testpg.Retry(ctx, s.pool, func() error {
		return execCheck(ctx, pg, "psql", "-U", "netbox", "-d", "netbox", "-c", "SELECT 1")
	}); err != nil {
		return fmt.Errorf("wait for netbox db: %w", err)
	}

	f, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("open dump: %w", err)
	}
	defer f.Close()

	// pg_restore streams stdin. dockertest's Resource.Exec attaches no
	// stdin, so this exec goes through the Docker client directly.
	exec, err := s.docker.ExecCreate(ctx, pg.ID(), mobyclient.ExecCreateOptions{
		Cmd:          []string{"pg_restore", "-U", "netbox", "-d", "netbox", "--no-owner", "--no-acl"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("create pg_restore exec: %w", err)
	}
	attach, err := s.docker.ExecAttach(ctx, exec.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}
	defer attach.Close()
	sent := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(attach.Conn, f)
		// Closing the write side is pg_restore's end of input.
		if closeErr := attach.CloseWrite(); copyErr == nil {
			copyErr = closeErr
		}
		sent <- copyErr
	}()
	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, attach.Reader); err != nil {
		return fmt.Errorf("pg_restore: read output: %w", err)
	}
	sendErr := <-sent
	inspect, err := s.docker.ExecInspect(ctx, exec.ID, mobyclient.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect pg_restore: %w", err)
	}
	// pg_restore exits 1 on harmless warnings (e.g. role-does-not-exist
	// when --no-owner strips ownership). Treat >1 as a real error.
	if inspect.ExitCode > 1 {
		return fmt.Errorf("pg_restore failed (exit %d): %s", inspect.ExitCode, out.String())
	}
	if sendErr != nil {
		return fmt.Errorf("pg_restore: send dump: %w: %s", sendErr, out.String())
	}
	return nil
}

func execCheck(ctx context.Context, res dockertest.Resource, cmd ...string) error {
	_, err := execOutput(ctx, res, cmd...)
	return err
}

func waitForNetBox(ctx context.Context, pool dockertest.Pool, url string) error {
	return testpg.Retry(ctx, pool, func() error {
		req, err := http.NewRequestWithContext(ctx, "GET", url+"/api/status/", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Token "+fixtureToken)
		req.Header.Set("Accept", "application/json; version=4.5")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("netbox api status %d", resp.StatusCode)
		}
		return nil
	})
}

// fixtureDumpPath returns the absolute path to the requested fixture.
// Walks up from this file's directory to the repo root.
func fixtureDumpPath(fixture string) (string, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	// internal/netboxtest/netboxtest.go -> repo root
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	var name string
	switch fixture {
	case "baseline":
		name = "netbox-baseline.dump"
	default:
		// "prod" used to live here. The dump it referenced was
		// removed once it became stale relative to the
		// device-type custom fields slagworks now expects. Regenerate
		// via `just netbox-snapshot` and re-add the case if a test
		// needs prod-shaped data again.
		return "", fmt.Errorf("unknown fixture %q (want baseline)", fixture)
	}
	path := filepath.Join(root, "dev", "netbox", name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("fixture %s not found at %s: %w", fixture, path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// testLogWriter bridges slog output to t.Log so it shows up in test
// output alongside failures. Go test dedups log flushing per test.
type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// ReceiverHost returns the host address the NetBox RQ worker container
// uses to reach an in-process Go HTTP server on this machine (the webhook
// receiver). The workspace test opens its listener on 0.0.0.0 and builds
// its reachable URL as "http://<ReceiverHost>:<port>/...".
//
// The value is environment-specific because the worker and the Go test
// process live in different network namespaces:
//
//   - Linux (CI on ARC runners): the test runner and DinD sidecar share
//     their Kubernetes pod network namespace. The per-test bridge gateway
//     created by DinD therefore routes back to an 0.0.0.0-bound listener
//     in the runner. Read it dynamically; never hardcode a subnet.
//   - macOS (Colima and Docker Desktop): the bridge gateway sits inside
//     the Docker VM and does NOT reach the macOS host, so we route via
//     the reserved host.docker.internal name, which Docker maps to the
//     host. The worker gets the matching extra_hosts entry.
func (e *Env) ReceiverHost() string {
	return e.receiverHost
}

// NetBoxLogs returns the trailing combined log output of the NetBox web
// container, for wiring into timeout diagnostics.
func (e *Env) NetBoxLogs() string {
	return containerLogs(e.docker, e.netboxRes)
}

// WorkerLogs returns the trailing combined log output of the NetBox RQ
// worker container, for wiring into timeout diagnostics.
func (e *Env) WorkerLogs() string {
	if e.workerRes == nil {
		return "(rq worker not started)"
	}
	return containerLogs(e.docker, e.workerRes)
}

// receiverRoute computes, for the given platform and bridge gateway, the
// host address the RQ worker container should use to reach the in-process
// webhook receiver, plus any extra_hosts entries the worker needs to
// resolve it. See Env.ReceiverHost for the per-environment rationale.
func receiverRoute(network dockertest.Network) (host string, extraHosts []string) {
	if runtime.GOOS == "linux" {
		if gw := networkGateway(network); gw != "" {
			return gw, nil
		}
	}
	return "host.docker.internal", []string{"host.docker.internal:host-gateway"}
}

// networkGateway returns the IPv4 gateway of the first IPAM config on the
// given bridge network. On Linux this gateway is an address on the host,
// reachable from every container on that network.
func networkGateway(network dockertest.Network) string {
	if network == nil {
		return ""
	}
	for _, cfg := range network.Inspect().IPAM.Config {
		if cfg.Gateway.IsValid() {
			return cfg.Gateway.String()
		}
	}
	return ""
}

// waitForWorker blocks until an RQ worker has registered with Redis,
// proving the worker process connected to the queue the web process
// enqueues webhook jobs on (django-rq 'default', Redis DB 0). Checks the
// rq:worker:* keyspace through the shared Redis container's redis-cli.
func waitForWorker(ctx context.Context, pool dockertest.Pool, redis dockertest.Resource, redisIP string) error {
	return testpg.Retry(ctx, pool, func() error {
		out, err := execOutput(ctx, redis, "redis-cli", "-h", redisIP, "-n", "0", "keys", "rq:worker:*")
		if err != nil {
			return fmt.Errorf("query rq workers: %w", err)
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("no rq worker registered with redis yet")
		}
		return nil
	})
}

// execOutput runs a command inside a container and returns its combined
// output alongside any non-zero-exit error (execCheck discards the output;
// this variant keeps it for diagnostics and readiness predicates). The
// output is stdout followed by stderr.
func execOutput(ctx context.Context, res dockertest.Resource, cmd ...string) (string, error) {
	result, err := res.Exec(ctx, cmd)
	out := result.StdOut + result.StdErr
	if err != nil {
		return out, err
	}
	if result.ExitCode != 0 {
		return out, fmt.Errorf("%v exit %d: %s", cmd, result.ExitCode, out)
	}
	return out, nil
}

// containerLogs returns the trailing combined stdout/stderr of a container
// (best-effort; a log-read failure yields an empty string). Used to make
// webhook-delivery timeouts diagnosable instead of a bare channel timeout.
func containerLogs(c *mobyclient.Client, res dockertest.Resource) string {
	if c == nil || res == nil {
		return ""
	}
	logs, err := c.ContainerLogs(context.Background(), res.ID(), mobyclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
	})
	if err != nil {
		return ""
	}
	defer logs.Close()
	var buf strings.Builder
	_, _ = stdcopy.StdCopy(&buf, &buf, logs)
	return buf.String()
}
