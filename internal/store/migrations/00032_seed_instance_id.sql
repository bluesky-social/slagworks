-- +goose Up
-- +goose StatementBegin

-- Seed the global's instance ID. 00001 creates the meta table, but no
-- migration in this set inserted its row, so a fresh install had none:
-- GetInstanceID failed and /healthz, /readyz and `slagctl version` reported
-- an empty instance_id. A database that already holds a row keeps it; the
-- ID is the global's identity and must not change. gen_random_uuid() is
-- built in (Postgres 13+) and 00015 already relies on it.
INSERT INTO meta (instance_id)
SELECT gen_random_uuid()::text
WHERE NOT EXISTS (SELECT 1 FROM meta);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The row stays: removing it would only make a later Up mint a different
-- ID for the same database.
SELECT 'down migration keeps the instance ID' AS note;
-- +goose StatementEnd
