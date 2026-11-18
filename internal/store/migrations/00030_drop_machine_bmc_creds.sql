-- +goose Up
-- +goose StatementBegin

-- The device's serial-keyed entry in AWS Secrets Manager
-- pop/<pop>/bmc-creds-admin is the only BMC credential slagworks uses.
-- The Postgres "operator" tier (a `slagworks` BMC account the deploy agent
-- created and rotated on every deploy, stored here) is gone: that account
-- needed ADMINISTRATOR anyway, so it bought no privilege split, and the
-- second copy drifted from the BMC in prod (oob-gw-3/4.pop2 held a stale
-- password while the BMC matched the admin secret). Destructive by design:
-- the stored passwords have no remaining reader.
ALTER TABLE machines DROP COLUMN bmc_user;
ALTER TABLE machines DROP COLUMN bmc_pass;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE machines ADD COLUMN bmc_user text;
ALTER TABLE machines ADD COLUMN bmc_pass text;
-- +goose StatementEnd
