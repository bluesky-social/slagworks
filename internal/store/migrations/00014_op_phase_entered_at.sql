-- +goose Up
-- +goose StatementBegin

-- Three-tier op timeout model needs phase-entry tracking to time
-- phone-home (and any future per-phase deadlines) from when the phase
-- was last entered, not from when the op was first claimed. See
-- docs/archeology/specs/2026-05-23-state-machine-redesign.md and
-- the impl plan, commit 4.
--
-- Default to created_at so existing rows pre-migration have a sane
-- non-NULL value and the PhoneHomeTimeout sweep doesn't immediately
-- fail every awaiting_phone_home op the first tick after rollout.

ALTER TABLE operations
    ADD COLUMN phase_entered_at timestamptz NOT NULL DEFAULT now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE operations DROP COLUMN phase_entered_at;
-- +goose StatementEnd
