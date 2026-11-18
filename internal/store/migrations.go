package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/store/migrations"
)

// Migrate applies pending goose migrations to the given Postgres-backed
// *gorm.DB. Postgres is the only supported dialect: global always runs
// on RDS in prod, and tests spin up real Postgres via dockertest so they
// exercise the same schema (partial indexes, CHECK constraints, jsonb,
// RETURNING). The previous SQLite+AutoMigrate fallback silently drifted
// from prod on every migration.
func Migrate(db *gorm.DB) error {
	if name := db.Dialector.Name(); name != "postgres" {
		return fmt.Errorf("store.Migrate requires postgres; got %q", name)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("extracting sql.DB: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return fmt.Errorf("creating goose provider: %w", err)
	}
	results, err := provider.Up(context.Background())
	if err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	for _, r := range results {
		slog.Info("migration applied",
			"version", r.Source.Version,
			"name", r.Source.Path,
			"duration", r.Duration,
		)
	}
	if len(results) == 0 {
		slog.Info("migrations up to date")
	}
	return nil
}
