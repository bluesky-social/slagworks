-- +goose Up
-- +goose StatementBegin

-- auto_commission was the per-site knob that let the httpboot handler
-- auto-register unknown MACs and boot them into commission on first PXE.
-- The operator's standing hard rule is no auto-commission (see
-- feedback_no_auto_commission memory + docs/dev/state-machine.md); the
-- prod wiring was deliberately deleted in the May 2026 SSoT rewrite and
-- nothing has read this column since. Drop it.

ALTER TABLE site_configs DROP COLUMN IF EXISTS auto_commission;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- No-op: the column was dead before this migration; recreating it on
-- rollback serves nothing.

-- +goose StatementEnd
