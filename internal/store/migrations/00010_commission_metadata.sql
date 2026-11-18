-- +goose Up
ALTER TABLE machines ADD COLUMN commission_source TEXT DEFAULT NULL;
ALTER TABLE machines ADD COLUMN commissioned_at TIMESTAMPTZ DEFAULT NULL;

UPDATE machines
   SET commission_source = 'commission',
       commissioned_at = created_at
 WHERE commission_data IS NOT NULL;

-- +goose Down
ALTER TABLE machines DROP COLUMN IF EXISTS commission_source;
ALTER TABLE machines DROP COLUMN IF EXISTS commissioned_at;
