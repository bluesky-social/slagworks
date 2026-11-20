package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestOpenGlobalDB_StatementTimeout is the lane-1 safety-net test for
// statement timeouts: the connection created through the production helper must
// carry a server-side statement_timeout that actually cancels a hung
// statement, rather than just claiming to.
func TestOpenGlobalDB_StatementTimeout(t *testing.T) {
	if globalStatementTimeout != 30*time.Second {
		t.Fatalf("globalStatementTimeout = %s, want 30s", globalStatementTimeout)
	}

	// Bring up the shared container so we have the DSN (and migrations).
	_ = testpg.DB(t, store.Migrate)
	dsn := testpg.DSN()
	if dsn == "" {
		t.Fatal("testpg DSN is empty; container not started")
	}

	db, pool, err := openGlobalDB(context.Background(), dsn, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("openGlobalDB: %v", err)
	}
	defer pool.Close()

	var effective string
	if err := db.Raw("SHOW statement_timeout").Scan(&effective).Error; err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	if effective != "50ms" {
		t.Errorf("SHOW statement_timeout = %q, want %q", effective, "50ms")
	}

	start := time.Now()
	err = db.Exec("SELECT pg_sleep(1)").Error
	elapsed := time.Since(start)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("pg_sleep(1) error = %v, want SQLSTATE 57014 (query canceled)", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("pg_sleep(1) took %v, want <500ms (statement_timeout not binding)", elapsed)
	}
}

// TestOpenGlobalDB_MalformedDSN covers the helper's parse-error seam
// without needing a database: a DSN pgx cannot parse must fail with
// actionable context rather than a bare panic or silent success.
func TestOpenGlobalDB_MalformedDSN(t *testing.T) {
	_, pool, err := openGlobalDB(context.Background(), "postgres://user:p%zz@localhost:5432/db", 30*time.Second)
	if err == nil {
		t.Fatal("openGlobalDB with malformed DSN: expected error")
	}
	if pool != nil {
		t.Error("openGlobalDB with malformed DSN: expected nil pool (parse error path)")
	}
	if !strings.Contains(err.Error(), "parsing database DSN") {
		t.Errorf("error should carry parse context: %v", err)
	}
}

// TestOpenGlobalDB_BurstQueuesWithinConnectionLimit covers what happens after
// global restarts. Each reconnecting site worker replays its whole outbox at
// once, and the hub runs one handler goroutine per observation, so global
// issues as many concurrent statements as the outboxes hold. The pool must
// queue a burst on a bounded set of connections. A pool that opens a
// connection per statement runs into the database's connection limit, and
// every refused connection is a failed handler: log batches and op updates
// are sent once, so their data is lost.
func TestOpenGlobalDB_BurstQueuesWithinConnectionLimit(t *testing.T) {
	db := testpg.DB(t, store.Migrate)

	// A login that may hold 40 connections: well above what global needs,
	// well below what a burst asks for when every statement gets its own.
	const (
		connectionLimit = 40
		handlers        = 150
		lockKey         = 7246
	)
	if err := db.Exec(`DROP ROLE IF EXISTS burst_login`).Error; err != nil {
		t.Fatalf("drop role: %v", err)
	}
	if err := db.Exec(fmt.Sprintf(`CREATE ROLE burst_login LOGIN PASSWORD 'burst' CONNECTION LIMIT %d`, connectionLimit)).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = db.Exec(`DROP ROLE IF EXISTS burst_login`).Error })
	dsn := strings.NewReplacer("user=postgres", "user=burst_login", "password=test", "password=burst").Replace(testpg.DSN())
	if !strings.Contains(dsn, "user=burst_login") {
		t.Fatalf("could not point the testpg DSN at the limited login: %q", dsn)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, pool, err := openGlobalDB(ctx, dsn, globalStatementTimeout)
	if err != nil {
		t.Fatalf("openGlobalDB: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// Every handler's statement waits on an advisory lock held here, so each
	// keeps its connection until the test lets go. The burst overlaps
	// completely however fast or slow the machine is.
	adminDB, err := db.DB()
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	holder, err := adminDB.Conn(ctx)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatalf("take advisory lock: %v", err)
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey); err != nil {
				t.Errorf("release advisory lock: %v", err)
			}
		})
	}
	defer release()

	var failed atomic.Int64
	errs := make(chan error, handlers)
	var wg sync.WaitGroup
	for range handlers {
		wg.Go(func() {
			if _, err := pool.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, lockKey); err != nil {
				failed.Add(1)
				errs <- err
			}
		})
	}

	// Hold the lock until the statements beyond the limit have each queued
	// for a connection or been refused one. A pool bounded at or under the
	// limit queues them; a pool without a bound tries to connect for each.
	deadline := time.Now().Add(10 * time.Second)
	for failed.Load()+pool.Stats().WaitCount < handlers-connectionLimit && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	release()
	wg.Wait()
	close(errs)

	if n := failed.Load(); n != 0 {
		t.Fatalf("%d of %d concurrent statements failed against a login limited to %d connections; want 0: the pool must queue a burst, not open a connection per statement. First error: %v",
			n, handlers, connectionLimit, <-errs)
	}
	if got := pool.Stats().MaxOpenConnections; got < 1 || got > connectionLimit {
		t.Fatalf("pool.Stats().MaxOpenConnections = %d, want a bound between 1 and %d", got, connectionLimit)
	}
}
