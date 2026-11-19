package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/store/migrations"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Migration checks against a fresh database on the shared test container:
//   - Migrate applies cleanly from empty;
//   - a second Migrate is a no-op and leaves the schema byte-identical;
//   - for a random version k, DownTo(k) then Up returns to the same schema
//     (each Down undoes exactly what its Up did).

func freshDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	_ = testStore(t) // brings the shared container up
	admin := testpg.DSN()
	adb, err := gorm.Open(postgres.Open(admin), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := adb.Exec("DROP DATABASE IF EXISTS " + name).Error; err != nil {
		t.Fatal(err)
	}
	if err := adb.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatal(err)
	}
	dsn := strings.Replace(admin, "dbname=slagworks_test", "dbname="+name, 1)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard, TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = adb.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
		if sqlDB, err := adb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// schemaFingerprint lists every column, constraint and index in public,
// excluding goose bookkeeping.
func schemaFingerprint(t interface{ Fatal(...any) }, db *gorm.DB) []string {
	var cols []string
	if err := db.Raw(`
SELECT table_name || '.' || column_name || ' ' || data_type || ' null=' || is_nullable || ' default=' || COALESCE(column_default, '')
FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> 'goose_db_version'
ORDER BY 1`).Scan(&cols).Error; err != nil {
		t.Fatal(err)
	}
	var idx []string
	if err := db.Raw(`
SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'goose_db_version' ORDER BY 1`).Scan(&idx).Error; err != nil {
		t.Fatal(err)
	}
	var cons []string
	if err := db.Raw(`
SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
FROM pg_constraint WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'goose_db_version'
ORDER BY 1`).Scan(&cons).Error; err != nil {
		t.Fatal(err)
	}
	return append(append(cols, idx...), cons...)
}

func TestMigrationsIdempotentFromEmpty(t *testing.T) {
	db := freshDB(t, "mig_idem")
	if err := Migrate(db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	first := schemaFingerprint(t, db)
	if len(first) == 0 {
		t.Fatal("empty schema after migrate")
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if second := schemaFingerprint(t, db); !reflect.DeepEqual(first, second) {
		t.Fatalf("schema changed on re-run")
	}
}

func TestMigrationsDownUpRoundTrip(t *testing.T) {
	db := freshDB(t, "mig_downup")
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	want := schemaFingerprint(t, db)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, s := range p.ListSources() {
		versions = append(versions, s.Version)
	}
	// 00011, 00012, 00013 and 00025 document their Down as unsupported
	// (a no-op SELECT), so only targets at or after 00025 have a Down chain
	// that claims to be real. The domain is a handful of targets, so it is
	// covered exhaustively rather than sampled: random repetition only
	// re-adds and re-drops the same columns until Postgres's 1600-column
	// limit (which counts dropped columns) trips.
	for _, k := range versions {
		if k < 25 {
			continue
		}
		if _, err := p.DownTo(context.Background(), k); err != nil {
			t.Fatalf("down to %d: %v", k, err)
		}
		if _, err := p.Up(context.Background()); err != nil {
			t.Fatalf("up from %d: %v", k, err)
		}
		got := schemaFingerprint(t, db)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("schema after down-to-%d/up differs:\n%s", k, diffLines(want, got))
		}
	}
}

func diffLines(a, b []string) string {
	in := func(s []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range s {
			m[x] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	var out []string
	for _, x := range a {
		if !mb[x] {
			out = append(out, "- "+x)
		}
	}
	for _, x := range b {
		if !ma[x] {
			out = append(out, "+ "+x)
		}
	}
	return fmt.Sprint(strings.Join(out, "\n"))
}
