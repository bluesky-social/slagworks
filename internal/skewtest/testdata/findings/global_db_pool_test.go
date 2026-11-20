package main

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestOpenGlobalDB_BurstStaysWithinTheDatabasesConnectionLimit reproduces
// the outbox replay that follows a global restart. A reconnecting site
// worker sends every queued observation at once and the hub runs one
// handler goroutine per observation, so global issues as many concurrent
// statements as the outbox holds (142 dhcp_lease observations from one
// worker in the incident this pins). The pool global opens must queue them
// on the connections it has, not open one connection each: the database
// accepts only so many, and every refused connection is a failed handler.
func TestOpenGlobalDB_BurstStaysWithinTheDatabasesConnectionLimit(t *testing.T) {
	_ = testpg.DB(t, store.Migrate)
	adminDSN := testpg.DSN()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// A login that may hold 40 connections: far more than global needs,
	// far fewer than a burst asks for.
	const connectionLimit = 40
	for _, stmt := range []string{
		`DROP ROLE IF EXISTS burst_login`,
		`CREATE ROLE burst_login LOGIN PASSWORD 'burst' CONNECTION LIMIT 40`,
	} {
		if _, err := admin.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = a.Close() }()
		_, _ = a.Exec(`DROP ROLE IF EXISTS burst_login`)
	})
	dsn := strings.NewReplacer("user=postgres", "user=burst_login", "password=test", "password=burst").Replace(adminDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, pool, err := openGlobalDB(ctx, dsn, globalStatementTimeout)
	if err != nil {
		t.Fatalf("openGlobalDB: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// 150 handlers at once, each holding its connection for 50ms.
	const handlers = 150
	var wg sync.WaitGroup
	errs := make(chan error, handlers)
	for i := 0; i < handlers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.ExecContext(ctx, `SELECT pg_sleep(0.05)`); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	var failed int
	var first error
	for err := range errs {
		if first == nil {
			first = err
		}
		failed++
	}
	if failed != 0 {
		t.Fatalf("%d of %d concurrent statements failed against a login limited to %d connections; want 0: the pool must queue a burst, not open a connection per statement. First error: %v",
			failed, handlers, connectionLimit, first)
	}
	if got := pool.Stats().MaxOpenConnections; got <= 0 || got > connectionLimit {
		t.Fatalf("pool.Stats().MaxOpenConnections = %d, want a bound between 1 and %d", got, connectionLimit)
	}
}
