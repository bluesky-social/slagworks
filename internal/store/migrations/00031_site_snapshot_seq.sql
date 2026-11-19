-- +goose Up
-- +goose StatementBegin

-- Snapshot SeqNums come from one sequence instead of a per-row counter.
-- A site's workers keep their tokens and their persisted SeqNum when the
-- site_configs row is deleted, and they drop any snapshot whose SeqNum is
-- not above the one they hold. A per-row counter restarted at 1 when an
-- operator deleted and re-created a site config, so its workers silently
-- ignored global until the new count caught up with the old one. Values
-- drawn from a sequence never repeat, so a slug's SeqNum keeps rising
-- across a delete and re-create; per-slug values are no longer contiguous,
-- which nothing relies on.
--
-- Seeded above every SeqNum already issued. OWNED BY ties the sequence's
-- lifetime to the column it feeds; deleting rows does not touch it.
CREATE SEQUENCE site_snapshot_seq OWNED BY site_configs.last_snapshot_seq;
SELECT setval('site_snapshot_seq',
              COALESCE((SELECT MAX(last_snapshot_seq) FROM site_configs), 0) + 1,
              false);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP SEQUENCE site_snapshot_seq;
-- +goose StatementEnd
