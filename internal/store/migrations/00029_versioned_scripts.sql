-- +goose Up
-- +goose StatementBegin

-- Storage/network scripts move from single-row clobber-on-update to
-- versioned storage: the script row is the stable identity
-- (id, name, head_version, created_at) and every upload/update mints an
-- immutable version row (priority, content) rather than overwriting
-- content in place. This is what makes "update" safe to run without
-- losing history, and gives a head pointer to advance instead of a row
-- to clobber.
--
-- Existing rows are migrated in place: each script's current
-- priority/content becomes its version-1 row before the columns are
-- dropped from the base table, so no live script (prod has real ones)
-- is lost — just relocated.

ALTER TABLE storage_scripts ADD COLUMN head_version bigint NOT NULL DEFAULT 1;
ALTER TABLE network_scripts ADD COLUMN head_version bigint NOT NULL DEFAULT 1;

CREATE TABLE storage_script_versions (
    script_id  uuid   NOT NULL REFERENCES storage_scripts(id) ON DELETE CASCADE,
    version    bigint NOT NULL,
    priority   bigint NOT NULL,
    content    text   NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (script_id, version)
);

CREATE TABLE network_script_versions (
    script_id  uuid   NOT NULL REFERENCES network_scripts(id) ON DELETE CASCADE,
    version    bigint NOT NULL,
    priority   bigint NOT NULL,
    content    text   NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (script_id, version)
);

-- Backfill: one version-1 row per existing script, carrying its current
-- priority/content/created_at forward so history starts truthful rather
-- than at "unknown".
INSERT INTO storage_script_versions (script_id, version, priority, content, created_at)
SELECT id, 1, priority, content, COALESCE(created_at, now()) FROM storage_scripts;

INSERT INTO network_script_versions (script_id, version, priority, content, created_at)
SELECT id, 1, priority, content, COALESCE(created_at, now()) FROM network_scripts;

-- Content/priority now live exclusively in the version tables; the
-- script row is pure identity + head pointer.
ALTER TABLE storage_scripts DROP COLUMN priority;
ALTER TABLE storage_scripts DROP COLUMN content;
ALTER TABLE network_scripts DROP COLUMN priority;
ALTER TABLE network_scripts DROP COLUMN content;

-- The old (non-unique) priority indexes no longer apply to a dropped
-- column; drop them explicitly rather than let the column drop take
-- them along implicitly.
DROP INDEX IF EXISTS idx_storage_scripts_priority;
DROP INDEX IF EXISTS idx_network_scripts_priority;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE storage_scripts ADD COLUMN priority bigint;
ALTER TABLE storage_scripts ADD COLUMN content text;
ALTER TABLE network_scripts ADD COLUMN priority bigint;
ALTER TABLE network_scripts ADD COLUMN content text;

UPDATE storage_scripts s SET priority = v.priority, content = v.content
    FROM storage_script_versions v
    WHERE v.script_id = s.id AND v.version = s.head_version;
UPDATE network_scripts s SET priority = v.priority, content = v.content
    FROM network_script_versions v
    WHERE v.script_id = s.id AND v.version = s.head_version;

ALTER TABLE storage_scripts ALTER COLUMN priority SET NOT NULL;
ALTER TABLE storage_scripts ALTER COLUMN content SET NOT NULL;
ALTER TABLE network_scripts ALTER COLUMN priority SET NOT NULL;
ALTER TABLE network_scripts ALTER COLUMN content SET NOT NULL;

CREATE INDEX idx_storage_scripts_priority ON storage_scripts (priority);
CREATE INDEX idx_network_scripts_priority ON network_scripts (priority);

DROP TABLE IF EXISTS network_script_versions;
DROP TABLE IF EXISTS storage_script_versions;
ALTER TABLE network_scripts DROP COLUMN IF EXISTS head_version;
ALTER TABLE storage_scripts DROP COLUMN IF EXISTS head_version;

-- +goose StatementEnd
