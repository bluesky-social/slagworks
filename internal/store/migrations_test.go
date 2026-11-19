package store

import (
	"context"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/store/migrations"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// emptyDB creates a database with nothing in it inside the shared test
// container and returns a connection to it. The shared slagworks_test
// database is already migrated and is truncated between tests, so it
// cannot show what Migrate does to an empty database on first boot.
func emptyDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	_ = testStore(t) // starts the shared container so testpg.DSN is set
	admin, err := gorm.Open(postgres.Open(testpg.DSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error; err != nil {
		t.Fatalf("drop stale database %s: %v", name, err)
	}
	if err := admin.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	dsn := strings.Replace(testpg.DSN(), "dbname=slagworks_test", "dbname="+name, 1)
	if dsn == testpg.DSN() {
		t.Fatalf("testpg DSN %q does not name slagworks_test; cannot point it at %s", dsn, name)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// metaRows returns every instance_id in the meta table.
func metaRows(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var ids []string
	if err := db.Raw("SELECT instance_id FROM meta").Scan(&ids).Error; err != nil {
		t.Fatalf("read meta: %v", err)
	}
	return ids
}

// Migrate is what creates the global's instance ID: GetInstanceID only
// reads it, and /healthz, /readyz and `slagctl version` report it. A fresh
// install must therefore come out of Migrate with exactly one meta row.
func TestMigrateSeedsInstanceID(t *testing.T) {
	db := emptyDB(t, "migrate_fresh_instance_id")
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	id, err := New(db).GetInstanceID()
	if err != nil || id == "" {
		t.Fatalf("GetInstanceID on a freshly migrated database: id=%q err=%v", id, err)
	}
	if rows := metaRows(t, db); len(rows) != 1 || rows[0] != id {
		t.Fatalf("meta rows after Migrate = %q, want exactly [%q]", rows, id)
	}
}

// Rolling the seed migration back and forward again keeps the ID: its Down
// leaves the row, and its Up only inserts into an empty table.
func TestMigrateDownUpKeepsInstanceID(t *testing.T) {
	db := emptyDB(t, "migrate_downup_instance_id")
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	before, err := New(db).GetInstanceID()
	if err != nil {
		t.Fatalf("GetInstanceID after Migrate: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("extract sql.DB: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := p.DownTo(context.Background(), 31); err != nil {
		t.Fatalf("down to 31: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}

	after, err := New(db).GetInstanceID()
	if err != nil || after != before {
		t.Fatalf("GetInstanceID after down/up: id=%q err=%v, want %q", after, err, before)
	}
	if rows := metaRows(t, db); len(rows) != 1 {
		t.Fatalf("meta rows after down/up = %q, want only %q", rows, before)
	}
}

// A database that already holds an instance ID keeps it: the ID is the
// global's identity, and Migrate must not replace it or add a second row.
func TestMigrateKeepsExistingInstanceID(t *testing.T) {
	db := emptyDB(t, "migrate_existing_instance_id")
	const existing = "existing-instance-id"
	if err := db.Exec("CREATE TABLE meta (instance_id text NOT NULL, PRIMARY KEY (instance_id))").Error; err != nil {
		t.Fatalf("create meta: %v", err)
	}
	if err := db.Exec("INSERT INTO meta (instance_id) VALUES (?)", existing).Error; err != nil {
		t.Fatalf("insert existing instance ID: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	id, err := New(db).GetInstanceID()
	if err != nil || id != existing {
		t.Fatalf("GetInstanceID after Migrate: id=%q err=%v, want %q", id, err, existing)
	}
	if rows := metaRows(t, db); len(rows) != 1 {
		t.Fatalf("meta rows after Migrate = %q, want only %q", rows, existing)
	}
}
