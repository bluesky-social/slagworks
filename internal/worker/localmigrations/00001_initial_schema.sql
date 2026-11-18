-- +goose Up
-- +goose StatementBegin

-- Baseline schema for the site worker's local SQLite DB.
--
-- This migration adopts DBs that were previously created by GORM
-- AutoMigrate (fable-review Tier 3): every prod oob-gw already has a
-- worker.db whose tables AutoMigrate produced. Each statement below is
-- CREATE TABLE IF NOT EXISTS with DDL byte-identical to what AutoMigrate
-- emitted (captured from a fresh AutoMigrate'd DB via sqlite_master), so:
--   - on an existing prod DB the statements are no-ops (tables already
--     present with this exact shape) and goose simply records version 1,
--     leaving all rows intact;
--   - on a fresh install the statements create the identical schema, so
--     the two paths converge on the same tables.
--
-- The PRIMARY KEY declarations implicitly create the sqlite_autoindex_*
-- indexes; the AUTOINCREMENT on outbox.id implicitly creates
-- sqlite_sequence. Neither is declared here (SQLite manages them).

CREATE TABLE IF NOT EXISTS `kv` (`key` text,`value` text,PRIMARY KEY (`key`));

CREATE TABLE IF NOT EXISTS `outbox` (`id` integer PRIMARY KEY AUTOINCREMENT,`payload` text NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `cached_machines` (`machine_id` text,`state` text NOT NULL,`data` text NOT NULL,PRIMARY KEY (`machine_id`));

CREATE TABLE IF NOT EXISTS `dhcp_leases` (`mac` text,`ip` text NOT NULL,`expires_at` datetime NOT NULL,PRIMARY KEY (`mac`));

CREATE TABLE IF NOT EXISTS `current_deploy_ops` (`machine_id` text,`operation_id` text NOT NULL,PRIMARY KEY (`machine_id`));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Deliberately irreversible. On a prod worker.db the Up above ADOPTED
-- pre-existing tables (it created nothing), so a symmetric Down would
-- drop outbox/recovery data the migration never created. No-op instead.
SELECT 1;

-- +goose StatementEnd
