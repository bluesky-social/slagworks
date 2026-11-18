-- +goose Up
-- +goose StatementBegin

-- Priority on storage_scripts / network_scripts is a MATCH TIEBREAKER, not a
-- globally-unique key: configscript.Match sorts candidates by priority desc
-- and returns the first whose rules match. Two scripts that match disjoint
-- host sets (e.g. the pop1 L3-kworker net script and the pop2 hot-spare L2
-- bond net script) can and should share a priority — that is the normal case,
-- not a conflict. The original UNIQUE index on priority wrongly rejected the
-- second script at the same priority with a 23505, blocking a legitimate
-- upload. Replace the unique indexes with plain (non-unique) indexes; name
-- stays UNIQUE (that IS the identity key).

DROP INDEX IF EXISTS idx_network_scripts_priority;
DROP INDEX IF EXISTS idx_storage_scripts_priority;

CREATE INDEX IF NOT EXISTS idx_network_scripts_priority ON network_scripts (priority);
CREATE INDEX IF NOT EXISTS idx_storage_scripts_priority ON storage_scripts (priority);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_network_scripts_priority;
DROP INDEX IF EXISTS idx_storage_scripts_priority;

CREATE UNIQUE INDEX IF NOT EXISTS idx_network_scripts_priority ON network_scripts (priority);
CREATE UNIQUE INDEX IF NOT EXISTS idx_storage_scripts_priority ON storage_scripts (priority);

-- +goose StatementEnd
