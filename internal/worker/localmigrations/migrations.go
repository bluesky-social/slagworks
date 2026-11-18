// Package localmigrations embeds the goose migration SQL for the site
// worker's local SQLite database. 00001_initial_schema.sql is the
// baseline; it adopts in place the tables that GORM AutoMigrate used to
// create on prod oob-gws (see the migration header). SQLite is the only
// dialect: the local DB is always SQLite (pure-Go glebarez/modernc
// driver), never Postgres. Global's schema lives in
// internal/store/migrations instead.
package localmigrations

import (
	"embed"
)

//go:embed *.sql
var FS embed.FS
