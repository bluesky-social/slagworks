-- +goose Up
-- +goose StatementBegin

-- Per-op monotonic version for the op-delta stream (Tier 2, Phase 0 of
-- docs/plans/2026-07-05-ops-out-of-snapshot.md). Every UPDATE that mutates
-- an operation row (claim, phase, complete, fail/sweep/abort) bumps
-- version = version + 1 in the same statement; INSERTs leave it at the
-- default 0. Consumers of the future op-delta / op-resync stream apply
-- last-writer-wins by this version so a reconnect-overlap or a concurrent
-- publisher race cannot regress an op's observed state.
--
-- Default 0 gives pre-migration rows a sane starting value and matches the
-- create-time semantics (a freshly inserted op is version 0 until its first
-- mutation).

ALTER TABLE operations
    ADD COLUMN version int NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE operations DROP COLUMN version;
-- +goose StatementEnd
