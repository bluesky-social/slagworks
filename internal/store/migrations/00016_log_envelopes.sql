-- +goose Up
-- +goose StatementBegin

-- Structured log envelopes flowing in from agents, SOL pumps, site
-- workers, and global itself. One row per envelope. The full
-- envelope JSON lives in payload; columns mirror the indexed fields
-- so live-tail queries don't need json key-extraction at scan time.
--
-- global_received_at is server-assigned (DEFAULT now()) and used by
-- the live-tail watermark in slagctl logs follow: queries lag the
-- read by 500ms past now() so an in-flight transaction allocating
-- a sequence id can't be skipped.
--
-- ts is emitter-supplied. Agents pre-NTP can have wildly skewed
-- clocks; that's why the watermark uses global_received_at, not ts.
--
-- See docs/archeology/plans/2026-05-24-structured-logs.md.

CREATE TABLE log_envelopes (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ NOT NULL,
    global_received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    received_at TIMESTAMPTZ NOT NULL,
    level TEXT NOT NULL,
    source TEXT NOT NULL,
    host TEXT,
    machine_id UUID,
    op_id UUID,
    trace_id UUID,
    phase TEXT,
    msg TEXT NOT NULL,
    payload JSONB NOT NULL
);

CREATE INDEX log_envelopes_machine ON log_envelopes (machine_id, ts DESC);
CREATE INDEX log_envelopes_trace   ON log_envelopes (trace_id, ts DESC);
CREATE INDEX log_envelopes_recv    ON log_envelopes (global_received_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE log_envelopes;
-- +goose StatementEnd
