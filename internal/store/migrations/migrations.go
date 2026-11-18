// Package migrations embeds slagworks' goose migration SQL.
// 00001_initial_schema.sql is the baseline; subsequent migrations
// transform it incrementally. Postgres is the only supported dialect —
// both prod and tests run against real Postgres (via dockertest), so
// partial indexes, CHECK constraints, jsonb, and RETURNING all work
// identically.
package migrations

import (
	"embed"
)

//go:embed *.sql
var FS embed.FS
