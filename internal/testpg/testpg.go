// Package testpg runs a shared Postgres container for unit tests that
// need a real Postgres instance. One container per test binary via
// sync.Once, reused across tests with TRUNCATE between invocations.
//
// testpg deliberately does NOT import internal/store to avoid an
// import cycle (store's tests consume testpg). Consumers wrap the
// returned *gorm.DB in store.New themselves, or ship a tiny helper
// in their own package (see internal/store's testStore helper for
// the pattern).
//
// The migrations-apply step is injected by the caller via a Migrate
// func parameter; this keeps testpg generic.
//
// Consuming packages must define a TestMain that calls testpg.Shutdown
// after m.Run so the container is torn down — the container's
// AutoRemove only fires when its main process exits, and Postgres
// runs forever.
//
// For integration tests that need both Postgres and NetBox, use
// internal/netboxtest instead. testpg is scoped to unit-test needs.
package testpg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/dockermirror"
)

// MigrateFn applies schema migrations to the shared DB. Supplied by
// the consumer (typically a thin wrapper around store.Migrate) so
// testpg itself stays free of any project-specific imports.
type MigrateFn func(*gorm.DB) error

var (
	once          sync.Once
	db            *gorm.DB
	dsnStr        atomic.Pointer[string]
	err           error
	docker        *mobyclient.Client
	pool          dockertest.ClosablePool
	cachedMigrate MigrateFn
)

// DB returns the shared *gorm.DB, TRUNCATEd between calls. On first
// invocation it brings up Postgres via dockertest and applies the
// supplied migrate function. Subsequent calls reuse the container
// but TRUNCATE all tables tracked in tablesToTruncate.
//
// Requires Docker running.
func DB(t *testing.T, migrate MigrateFn) *gorm.DB {
	t.Helper()
	once.Do(func() {
		cachedMigrate = migrate
		setup()
	})
	if err != nil {
		t.Fatalf("testpg setup: %v", err)
	}
	truncateAll(t, db)
	return db
}

// Shutdown purges the shared container. Idempotent; safe to call from
// packages that never invoked Store(t).
//
// Consuming packages should call this from TestMain:
//
//	func TestMain(m *testing.M) {
//	    code := m.Run()
//	    testpg.Shutdown()
//	    os.Exit(code)
//	}
func Shutdown() {
	if pool != nil {
		_ = pool.Close(context.Background())
		pool = nil
	}
	if docker != nil {
		_ = docker.Close()
		docker = nil
	}
}

// DSN returns the Postgres connection string for the shared
// container. Only valid after DB() has been called at least once.
// Returns empty string otherwise. Loads via atomic to avoid racing
// with setup().
func DSN() string {
	if p := dsnStr.Load(); p != nil {
		return *p
	}
	return ""
}

// DockerClient connects to the docker daemon named by DOCKER_HOST or,
// when that is unset, the first common socket path that accepts a
// connection (Colima and Docker Desktop do not serve
// /var/run/docker.sock by default), falling back to the client
// library's default. dockertest v4 takes no endpoint of its own: hand
// the client to dockertest.WithMobyClient and close it after the pool.
func DockerClient(ctx context.Context) (*mobyclient.Client, error) {
	opts := []mobyclient.Opt{mobyclient.FromEnv}
	if os.Getenv("DOCKER_HOST") == "" {
		if sock := localSocket(); sock != "" {
			opts = append(opts, mobyclient.WithHost(sock))
		}
	}
	c, newErr := mobyclient.New(opts...)
	if newErr != nil {
		return nil, newErr
	}
	if _, pingErr := c.Ping(ctx, mobyclient.PingOptions{}); pingErr != nil {
		_ = c.Close()
		return nil, pingErr
	}
	return c, nil
}

// localSocket returns the first common docker socket path that accepts
// a connection, as a unix:// URL, or empty string if none does.
func localSocket() string {
	home, _ := os.UserHomeDir()
	for _, path := range []string{
		"/var/run/docker.sock",
		home + "/.colima/default/docker.sock",
		home + "/.docker/run/docker.sock",
	} {
		c, dialErr := net.DialTimeout("unix", path, 500*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			return "unix://" + path
		}
	}
	return ""
}

// Expire bounds how long a container outlives a test process that dies
// before its cleanup runs (SIGKILL, a go test timeout): it asks the
// daemon to stop the container with SIGWINCH, which the container
// ignores, and a timeout of d, so the daemon kills it once d has passed
// and AutoRemove then deletes it. The daemon keeps the stop pending
// after this process exits. This is dockertest v3's Resource.Expire,
// which v4 dropped.
func Expire(c *mobyclient.Client, containerID string, d time.Duration) {
	timeout := int(d / time.Second)
	go func() {
		_, _ = c.ContainerStop(context.Background(), containerID, mobyclient.ContainerStopOptions{
			Signal:  "SIGWINCH",
			Timeout: &timeout,
		})
	}()
}

// Retry calls fn once a second until it succeeds or the pool's max wait
// runs out. On timeout the error carries fn's last error as well:
// pool.Retry alone returns only the context error, which hides why the
// service never became ready.
func Retry(ctx context.Context, pool dockertest.Pool, fn func() error) error {
	var last error
	retryErr := pool.Retry(ctx, 0, func() error {
		last = fn()
		return last
	})
	if retryErr == nil || last == nil || errors.Is(retryErr, last) {
		return retryErr
	}
	return fmt.Errorf("%w; last attempt: %w", retryErr, last)
}

func setup() {
	ctx := context.Background()
	docker, err = DockerClient(ctx)
	if err != nil {
		err = fmt.Errorf("connect to docker: %w", err)
		return
	}
	pool, err = dockertest.NewPool(ctx, "",
		dockertest.WithMobyClient(docker),
		dockertest.WithMaxWait(30*time.Second))
	if err != nil {
		err = fmt.Errorf("create dockertest pool: %w", err)
		return
	}

	// The image comes through SLAGWORKS_DOCKER_HUB_MIRROR when a CI
	// system sets it (Docker Hub rate-limits anonymous pulls), and from
	// Docker Hub otherwise.
	repo := dockermirror.Image("library/postgres")
	const tag = "18-alpine"
	if err = dockermirror.Pull(ctx, docker, repo, tag); err != nil {
		err = fmt.Errorf("start postgres: %w", err)
		return
	}
	resource, runErr := pool.Run(ctx, repo,
		dockertest.WithTag(tag),
		dockertest.WithEnv([]string{
			"POSTGRES_PASSWORD=test",
			"POSTGRES_DB=slagworks_test",
			"listen_addresses='*'",
		}),
		// dockertest otherwise hands back any container of the same
		// repository:tag this process already started.
		dockertest.WithoutReuse(),
		dockertest.WithHostConfig(func(config *container.HostConfig) {
			config.AutoRemove = true
			config.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyDisabled}
		}))
	if runErr != nil {
		err = fmt.Errorf("start postgres: %w", runErr)
		return
	}

	// Safety net: if the test process is SIGKILL'd before Shutdown
	// runs, the daemon kills the container after 1h, then AutoRemove
	// reaps it. Bounded local leak.
	Expire(docker, resource.ID(), time.Hour)

	dsn := fmt.Sprintf("host=localhost port=%s user=postgres password=test dbname=slagworks_test sslmode=disable",
		resource.GetPort("5432/tcp"))
	dsnStr.Store(&dsn)

	if retryErr := Retry(ctx, pool, func() error {
		var openErr error
		db, openErr = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard, TranslateError: true})
		if openErr != nil {
			return openErr
		}
		sqlDB, sqlErr := db.DB()
		if sqlErr != nil {
			return sqlErr
		}
		return sqlDB.Ping()
	}); retryErr != nil {
		err = fmt.Errorf("connect to postgres: %w", retryErr)
		return
	}

	if cachedMigrate != nil {
		if migrateErr := cachedMigrate(db); migrateErr != nil {
			err = fmt.Errorf("migrate: %w", migrateErr)
			return
		}
	}
}

// truncateExcept names the tables truncateAll must NOT clear: the goose
// migration-version bookkeeping, which has to survive between tests so the
// shared container keeps its applied-schema state.
var truncateExcept = map[string]bool{
	"goose_db_version": true,
}

// truncateAll wipes every base table in the public schema (except the
// goose bookkeeping) before handing the shared DB to the next test.
//
// The table set is discovered from information_schema at runtime rather
// than hardcoded: a static allowlist silently drifted as migrations added
// tables (log_envelopes, network_scripts, console_casts, machine_interfaces,
// dhcp_pools, sites were all missing), so rows leaked across DB() calls and
// any test asserting an exact row count flaked under reuse. Discovery means
// a new migration's table is cleaned automatically — no list to maintain.
func truncateAll(t *testing.T, db *gorm.DB) {
	t.Helper()

	var tables []string
	if scanErr := db.Raw(
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public'`,
	).Scan(&tables).Error; scanErr != nil {
		t.Fatalf("list tables to truncate: %v", scanErr)
	}

	quoted := make([]string, 0, len(tables))
	for _, tbl := range tables {
		if truncateExcept[tbl] {
			continue
		}
		// Quote each identifier so a table named like a keyword can't
		// break the statement. These come from pg_tables, not user
		// input, but quoting is the correct habit regardless.
		quoted = append(quoted, `"`+tbl+`"`)
	}
	if len(quoted) == 0 {
		return
	}

	// One statement so CASCADE resolves FKs across the whole set at once
	// and RESTART IDENTITY resets every serial — tests that assert on
	// auto-increment ids (e.g. log_envelopes.id) get a clean sequence.
	stmt := "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE"
	if truncErr := db.Exec(stmt).Error; truncErr != nil {
		t.Fatalf("truncate all: %v", truncErr)
	}
}
