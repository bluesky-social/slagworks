-- +goose Up
-- +goose StatementBegin

-- deploy_logs was the eager-push deploy log table (created in 00001). It
-- was superseded by the structured-envelope pipeline (log_envelopes,
-- 00016); 00013 wiped its rows during the state-machine redesign but the
-- empty table was never dropped. Nothing reads or writes it anymore.

DROP TABLE IF EXISTS deploy_logs;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- No-op: the table was dead before this migration; recreating an empty
-- copy on rollback serves nothing.

-- +goose StatementEnd
