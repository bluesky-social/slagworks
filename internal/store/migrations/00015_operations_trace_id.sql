-- +goose Up
-- +goose StatementBegin

-- Operations get a trace_id generated at creation time. The same id
-- is rendered into the iPXE kernel cmdline (slagworks.trace=...), set
-- as a slog default attr on the agent at boot, and stamped on every
-- log envelope flowing through site -> global. One trace per op,
-- 1:1, by construction.
--
-- Existing rows pre-migration get a fresh UUID v4 each. Pre-migration
-- ops have no log envelopes carrying their trace yet (the envelope
-- pipeline ships in the same change), so the assigned id is simply
-- the value queries will use to find anything emitted from then on.
--
-- See docs/archeology/plans/2026-05-24-structured-logs.md.

ALTER TABLE operations
    ADD COLUMN trace_id uuid;

UPDATE operations
   SET trace_id = gen_random_uuid()
 WHERE trace_id IS NULL;

ALTER TABLE operations
    ALTER COLUMN trace_id SET NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE operations DROP COLUMN trace_id;
-- +goose StatementEnd
