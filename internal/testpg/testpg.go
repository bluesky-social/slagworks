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
// after m.Run so the container is torn down — dockertest's AutoRemove
// only fires when the container's main process exits, and Postgres
// runs forever.
//
// For integration tests that need both Postgres and NetBox, use
// internal/netboxtest instead. testpg is scoped to unit-test needs.
package testpg

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
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
	pool          *dockertest.Pool
	resource      *dockertest.Resource
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
	if pool == nil || resource == nil {
		return
	}
	_ = pool.Purge(resource)
	resource = nil
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

// DockerEndpoint resolves the docker daemon endpoint. Tries DOCKER_HOST
// then common socket paths. Returns empty string if nothing works, in
// which case dockertest will fall back to its own defaults.
func DockerEndpoint() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
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

func setup() {
	pool, err = dockertest.NewPool(DockerEndpoint())
	if err != nil {
		err = fmt.Errorf("connect to docker: %w", err)
		return
	}
	pool.MaxWait = 30 * time.Second

	// The image comes through SLAGWORKS_DOCKER_HUB_MIRROR when a CI
	// system sets it (Docker Hub rate-limits anonymous pulls), and from
	// Docker Hub otherwise.
	repo := dockermirror.Image("library/postgres")
	resource, err = pool.RunWithOptions(&dockertest.RunOptions{
		Repository: repo,
		Tag:        "18-alpine",
		Env: []string{
			"POSTGRES_PASSWORD=test",
			"POSTGRES_DB=slagworks_test",
			"listen_addresses='*'",
		},
		Auth: dockermirror.Auth(),
	}, func(config *docker.HostConfig) {
		config.AutoRemove = true
		config.RestartPolicy = docker.NeverRestart()
	})
	if err != nil {
		err = fmt.Errorf("start postgres: %w", err)
		return
	}

	// Safety net: if the test process is SIGKILL'd before Shutdown
	// runs, the daemon stops the container after 1h, then AutoRemove
	// reaps it. Bounded local leak.
	_ = resource.Expire(3600)

	dsn := fmt.Sprintf("host=localhost port=%s user=postgres password=test dbname=slagworks_test sslmode=disable",
		resource.GetPort("5432/tcp"))
	dsnStr.Store(&dsn)

	if retryErr := pool.Retry(func() error {
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
