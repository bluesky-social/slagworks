-- +goose Up
-- +goose StatementBegin

-- NetBox SSoT refactor: drop tables whose contents live in NetBox, reshape
-- machines to carry only slagworks-owned runtime state, introduce
-- site_configs keyed by NetBox site slug.
--
-- Destructive by design: pop3 is re-bootstrapped from NetBox post-migration.
-- Slag-owned data that must survive the cutover (storage_scripts, ssh_keys,
-- images, BMC creds) is preserved via the Plan 3 slagctl migrate reload
-- flow, not by this migration.

-- Step 1: truncate FK-dependent tables first so the reshape below doesn't
-- trip fk_operations_machine (no ON DELETE CASCADE on the legacy schema).
TRUNCATE TABLE deploy_logs;
TRUNCATE TABLE operations;
TRUNCATE TABLE machine_interfaces;
TRUNCATE TABLE machines CASCADE;
TRUNCATE TABLE dhcp_pools;
TRUNCATE TABLE site_workers;
TRUNCATE TABLE sites CASCADE;

-- Step 2: drop legacy per-table triggers on tables we are about to reshape
-- or remove. Keep the global sync_seq sequence and the triggers on
-- operations and images; those still drive SyncSeq until Plan 2.
DROP TRIGGER IF EXISTS machines_sync_seq ON machines;
DROP TRIGGER IF EXISTS sites_sync_seq ON sites;
DROP TRIGGER IF EXISTS machines_delete_notify ON machines;
DROP TRIGGER IF EXISTS dhcp_pools_delete_notify ON dhcp_pools;

-- Step 3: drop the dependent tables.
DROP TABLE IF EXISTS machine_interfaces;
DROP TABLE IF EXISTS dhcp_pools;

-- Step 4: drop the sites table (site_workers.site_id FK must go first).
ALTER TABLE site_workers DROP CONSTRAINT IF EXISTS site_workers_site_id_fkey;
ALTER TABLE machines DROP CONSTRAINT IF EXISTS fk_machines_site;
DROP TABLE IF EXISTS sites;

-- Step 5: reshape machines.
ALTER TABLE machines DROP COLUMN IF EXISTS hostname;
ALTER TABLE machines DROP COLUMN IF EXISTS site_id;
ALTER TABLE machines DROP COLUMN IF EXISTS bmc_ip;
ALTER TABLE machines DROP COLUMN IF EXISTS sync_seq;
ALTER TABLE machines DROP COLUMN IF EXISTS netbox_synced_at;
DROP INDEX IF EXISTS idx_site_hostname;
DROP INDEX IF EXISTS idx_machines_net_box_device_id;

-- netbox_device_id is redefined as a nullable bigint with a unique partial index.
ALTER TABLE machines DROP COLUMN IF EXISTS netbox_device_id;
ALTER TABLE machines ADD COLUMN netbox_device_id bigint;
CREATE UNIQUE INDEX idx_machines_netbox_device_id ON machines (netbox_device_id)
    WHERE netbox_device_id IS NOT NULL;

-- netbox_serial is new: non-empty required, unique among non-purged rows.
ALTER TABLE machines ADD COLUMN netbox_serial text NOT NULL DEFAULT '';
ALTER TABLE machines ALTER COLUMN netbox_serial DROP DEFAULT;
ALTER TABLE machines ADD CONSTRAINT machines_serial_not_empty CHECK (netbox_serial <> '');
CREATE UNIQUE INDEX idx_machines_serial_live ON machines (netbox_serial)
    WHERE state <> 'purged';

-- Step 6: reshape site_workers: site_id UUID -> site_slug text.
ALTER TABLE site_workers DROP COLUMN IF EXISTS site_id;
ALTER TABLE site_workers ADD COLUMN site_slug text NOT NULL DEFAULT '';
ALTER TABLE site_workers ALTER COLUMN site_slug DROP DEFAULT;
DROP INDEX IF EXISTS idx_site_worker_unique;
CREATE UNIQUE INDEX idx_site_worker_unique ON site_workers (site_slug, worker_id);

-- Step 7: new site_configs table.
CREATE TABLE site_configs (
    slug                 text        NOT NULL PRIMARY KEY,
    ephemeral_pool_start text        NOT NULL,
    ephemeral_pool_end   text        NOT NULL,
    auto_commission      boolean     NOT NULL DEFAULT false,
    promiscuous          boolean     NOT NULL DEFAULT false,
    last_snapshot_seq    bigint      NOT NULL DEFAULT 0,
    last_snapshot_hash   text        NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Not supported by design. Restore from pg_dump if you need to roll back.
SELECT 'down migration not supported' AS note;
-- +goose StatementEnd
