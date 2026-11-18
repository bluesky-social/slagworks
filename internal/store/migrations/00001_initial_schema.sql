-- +goose Up
-- +goose StatementBegin

-- Initial schema: complete table definitions as of the pre-migration-10 baseline.
-- CREATE TABLE IF NOT EXISTS is safe to apply to existing databases (no-op on
-- tables that already exist). Migrations 1-9 are all idempotent (IF NOT EXISTS,
-- CREATE OR REPLACE, etc.) so they remain safe to run after this migration on
-- both new and existing databases.
--
-- Column names use GORM's snake_case convention (e.g. SubnetCIDR → subnet_c_id_r).

CREATE TABLE IF NOT EXISTS meta (
    instance_id text NOT NULL,
    PRIMARY KEY (instance_id)
);

CREATE TABLE IF NOT EXISTS sites (
    id              uuid        NOT NULL,
    name            text        NOT NULL,
    subnet_c_id_r   text        NOT NULL,
    v_ip            text,
    api_token_hash  text        NOT NULL DEFAULT '',
    created_at      timestamptz,
    sync_seq        bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sites_name            ON sites (name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sites_api_token_hash  ON sites (api_token_hash);

CREATE TABLE IF NOT EXISTS machines (
    id                uuid        NOT NULL,
    site_id           uuid        NOT NULL,
    hostname          text        NOT NULL,
    bmc_ip            text,
    bmc_user          text,
    bmc_pass          text,
    state             text        NOT NULL DEFAULT 'new',
    desired_os        text,
    current_os        text,
    commission_data   jsonb,
    locked            boolean     NOT NULL DEFAULT false,
    pxe_eligible      boolean     NOT NULL DEFAULT false,
    netbox_device_id  bigint,
    netbox_synced_at  timestamptz,
    created_at        timestamptz,
    sync_seq          bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    CONSTRAINT fk_machines_site FOREIGN KEY (site_id) REFERENCES sites(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_site_hostname              ON machines (site_id, hostname);
CREATE        INDEX IF NOT EXISTS idx_machines_net_box_device_id ON machines (netbox_device_id);

CREATE TABLE IF NOT EXISTS machine_interfaces (
    id         uuid NOT NULL,
    machine_id uuid NOT NULL,
    name       text NOT NULL,
    mac        text NOT NULL,
    ip         text,
    type       text NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT fk_machines_interfaces FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_machine_interfaces_mac        ON machine_interfaces (mac);
CREATE        INDEX IF NOT EXISTS idx_machine_interfaces_machine_id ON machine_interfaces (machine_id);

CREATE TABLE IF NOT EXISTS operations (
    id           uuid        NOT NULL,
    machine_id   uuid        NOT NULL,
    type         text        NOT NULL,
    status       text        NOT NULL DEFAULT 'pending',
    phase        text,
    claimed_by   text,
    claimed_at   timestamptz,
    created_at   timestamptz,
    completed_at timestamptz,
    params       jsonb,
    result       jsonb,
    sync_seq     bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    CONSTRAINT fk_operations_machine FOREIGN KEY (machine_id) REFERENCES machines(id)
);

CREATE        INDEX IF NOT EXISTS idx_operations_machine_id       ON operations (machine_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_one_active_op_per_machine   ON operations (machine_id)
    WHERE status IN ('pending', 'claimed', 'running');

CREATE TABLE IF NOT EXISTS images (
    id         uuid        NOT NULL,
    name       text        NOT NULL,
    kind       text        NOT NULL DEFAULT 'deploy',
    version    text        NOT NULL,
    arch       text        NOT NULL DEFAULT 'amd64',
    sha256     text        NOT NULL,
    url        text        NOT NULL,
    created_at timestamptz,
    sync_seq   bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_images_name ON images (name);

CREATE TABLE IF NOT EXISTS storage_scripts (
    id         uuid   NOT NULL,
    name       text   NOT NULL,
    priority   bigint NOT NULL,
    content    text   NOT NULL,
    created_at timestamptz,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_storage_scripts_name     ON storage_scripts (name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_storage_scripts_priority ON storage_scripts (priority);

CREATE TABLE IF NOT EXISTS site_workers (
    id            uuid        NOT NULL,
    site_id       uuid        NOT NULL,
    worker_id     text        NOT NULL,
    fqdn          text,
    first_seen_at timestamptz,
    last_seen_at  timestamptz,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_site_worker_unique ON site_workers (site_id, worker_id);

CREATE TABLE IF NOT EXISTS dhcp_pools (
    id              uuid        NOT NULL,
    site_id         uuid        NOT NULL,
    subnet          text        NOT NULL,
    range_start     text        NOT NULL,
    range_end       text        NOT NULL,
    gateway         text        NOT NULL,
    promiscuous     boolean     NOT NULL DEFAULT false,
    auto_commission boolean     NOT NULL DEFAULT false,
    created_at      timestamptz,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_dhcp_pools_site_id ON dhcp_pools (site_id);

CREATE TABLE IF NOT EXISTS ssh_keys (
    id         uuid        NOT NULL,
    public_key text        NOT NULL,
    comment    text,
    created_at timestamptz,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ssh_keys_public_key ON ssh_keys (public_key);

CREATE TABLE IF NOT EXISTS deploy_logs (
    id           bigserial   NOT NULL,
    machine_id   uuid        NOT NULL,
    operation_id uuid        NOT NULL,
    source       text        NOT NULL,
    ts           timestamptz NOT NULL,
    line         text        NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_deploy_logs_machine_cursor ON deploy_logs (machine_id, id);
CREATE INDEX IF NOT EXISTS idx_deploy_logs_machine_op     ON deploy_logs (machine_id, operation_id, id);

-- Sequences, functions, and triggers (all idempotent).

CREATE SEQUENCE IF NOT EXISTS sync_seq;

CREATE OR REPLACE FUNCTION bump_sync_seq()
RETURNS TRIGGER AS $$
BEGIN
  NEW.sync_seq := nextval('sync_seq');
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
  CREATE TRIGGER sites_sync_seq
  BEFORE INSERT OR UPDATE ON sites
  FOR EACH ROW EXECUTE FUNCTION bump_sync_seq();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER machines_sync_seq
  BEFORE INSERT OR UPDATE ON machines
  FOR EACH ROW EXECUTE FUNCTION bump_sync_seq();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER operations_sync_seq
  BEFORE INSERT OR UPDATE ON operations
  FOR EACH ROW EXECUTE FUNCTION bump_sync_seq();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER images_sync_seq
  BEFORE INSERT OR UPDATE ON images
  FOR EACH ROW EXECUTE FUNCTION bump_sync_seq();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE OR REPLACE FUNCTION notify_deploy_log()
RETURNS TRIGGER AS $$
BEGIN
  PERFORM pg_notify('slagworks', 'deploy_logs:' || NEW.machine_id);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
  CREATE TRIGGER deploy_logs_notify
  AFTER INSERT ON deploy_logs
  FOR EACH ROW EXECUTE FUNCTION notify_deploy_log();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE OR REPLACE FUNCTION notify_delete()
RETURNS TRIGGER AS $$
BEGIN
  PERFORM pg_notify('slagworks', TG_TABLE_NAME || ':deleted:' || OLD.id);
  RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
  CREATE TRIGGER machines_delete_notify
  AFTER DELETE ON machines
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER operations_delete_notify
  AFTER DELETE ON operations
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER ssh_keys_delete_notify
  AFTER DELETE ON ssh_keys
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER dhcp_pools_delete_notify
  AFTER DELETE ON dhcp_pools
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER storage_scripts_delete_notify
  AFTER DELETE ON storage_scripts
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  CREATE TRIGGER images_delete_notify
  AFTER DELETE ON images
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS deploy_logs;
DROP TABLE IF EXISTS dhcp_pools;
DROP TABLE IF EXISTS ssh_keys;
DROP TABLE IF EXISTS site_workers;
DROP TABLE IF EXISTS storage_scripts;
DROP TABLE IF EXISTS operations;
DROP TABLE IF EXISTS machine_interfaces;
DROP TABLE IF EXISTS machines;
DROP TABLE IF EXISTS sites;
DROP TABLE IF EXISTS meta;
DROP SEQUENCE IF EXISTS sync_seq;
DROP FUNCTION IF EXISTS bump_sync_seq();
DROP FUNCTION IF EXISTS notify_deploy_log();
DROP FUNCTION IF EXISTS notify_delete();
-- +goose StatementEnd
