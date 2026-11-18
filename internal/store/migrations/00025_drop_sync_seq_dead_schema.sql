-- +goose Up
-- +goose StatementBegin

-- Dead schema from the pre-WS sync-polling era (fable-review Tier 3).
-- sync_seq drove the legacy HTTP sync cursor; the WS snapshot pipeline
-- hashes content instead and nothing reads sync_seq anymore.
-- notify_deploy_log's table (deploy_logs) was dropped in 00023; the
-- function was orphaned. commission_source was written once at
-- commission-complete and never read anywhere.
DROP TRIGGER IF EXISTS operations_sync_seq ON operations;
DROP TRIGGER IF EXISTS images_sync_seq ON images;
DROP FUNCTION IF EXISTS bump_sync_seq();
DROP FUNCTION IF EXISTS notify_deploy_log();
DROP SEQUENCE IF EXISTS sync_seq;
ALTER TABLE operations DROP COLUMN IF EXISTS sync_seq;
ALTER TABLE images DROP COLUMN IF EXISTS sync_seq;
ALTER TABLE machines DROP COLUMN IF EXISTS commission_source;

-- idx_machines_serial_live was predicated on a 'purged' state that no
-- longer exists, so the WHERE clause matched every row anyway. Replace
-- with a plain unique index; the one-row-per-serial invariant is
-- unchanged.
DROP INDEX IF EXISTS idx_machines_serial_live;
CREATE UNIQUE INDEX idx_machines_serial ON machines (netbox_serial);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Not supported by design. Restore from pg_dump if you need to roll back.
SELECT 'down migration not supported' AS note;
-- +goose StatementEnd
