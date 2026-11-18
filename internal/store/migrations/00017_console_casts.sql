-- +goose Up
-- +goose StatementBegin

-- One perpetual cast per machine, lifetime equal to the machine's
-- lifetime. The cast row is created on first ever frame for a
-- machine and never deleted (events are TTL'd separately).
--
-- See docs/archeology/specs/2026-05-26-sol-asciinema-pipeline-design.md
-- for the full rationale.

CREATE TABLE console_casts (
    id BIGSERIAL PRIMARY KEY,
    machine_id UUID NOT NULL UNIQUE,
    cast_started_at TIMESTAMPTZ NOT NULL,
    width INT NOT NULL DEFAULT 80,
    height INT NOT NULL DEFAULT 24
);

CREATE TABLE console_cast_events (
    id BIGSERIAL PRIMARY KEY,
    cast_id BIGINT NOT NULL REFERENCES console_casts(id) ON DELETE CASCADE,
    -- t_us is monotonic-per-cast microseconds since cast_started_at,
    -- assigned by the site worker on emit (prev_max_us +
    -- monotonic_since_pump_start). Strict-monotonic across pumps and
    -- restarts thanks to the on-demand resume-offset RPC.
    t_us BIGINT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- BYTEA, not TEXT. SOL bytes aren't required to be UTF-8; the
    -- API layer scrubs to U+FFFD when JSON-encoding cast events.
    data BYTEA NOT NULL
);

-- Resume-from-offset (since_us=N, ORDER BY t_us ASC) and ordered
-- backfill scans.
CREATE INDEX console_cast_events_cast_t ON console_cast_events (cast_id, t_us);

-- Wall-clock anchored queries: since={duration} / tail={duration}.
-- The composite (cast_id, received_at) covers the WHERE+ORDER BY
-- on `received_at >= now() - $duration ORDER BY received_at ASC`.
CREATE INDEX console_cast_events_cast_received ON console_cast_events (cast_id, received_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE console_cast_events;
DROP TABLE console_casts;
-- +goose StatementEnd
