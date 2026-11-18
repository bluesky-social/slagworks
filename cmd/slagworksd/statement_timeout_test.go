package main

import (
	"context"
	"errors"
	"strings"
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
