-- +goose Up
-- +goose StatementBegin

-- network_scripts mirrors storage_scripts: NetBox-identity-matched shell
-- scripts that emit a host network-config bundle for the deploy agent.
-- See docs/archeology/plans/2026-06-13-pluggable-network-config.md.
CREATE TABLE IF NOT EXISTS network_scripts (
    id         uuid   NOT NULL,
    name       text   NOT NULL,
    priority   bigint NOT NULL,
    content    text   NOT NULL,
    created_at timestamptz,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_network_scripts_name     ON network_scripts (name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_network_scripts_priority ON network_scripts (priority);

-- Same delete-notify wiring as storage_scripts: a delete fires the
-- notify_delete() trigger so the snapshot reconciler rebuilds affected
-- sites. notify_delete() is defined in 00001_initial_schema.sql.
DO $$ BEGIN
  CREATE TRIGGER network_scripts_delete_notify
  AFTER DELETE ON network_scripts
  FOR EACH ROW EXECUTE FUNCTION notify_delete();
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS network_scripts;
-- +goose StatementEnd
