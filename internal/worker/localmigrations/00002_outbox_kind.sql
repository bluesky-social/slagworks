-- +goose Up
-- +goose StatementBegin

-- The outbox carries op updates as well as observations. kind says which
-- one a row's payload is. Every row written before this migration is an
-- observation (op updates were never persisted), so the default adopts
-- them unchanged and a worker upgraded with rows still unacked sends them
-- as it would have.
ALTER TABLE `outbox` ADD COLUMN `kind` text NOT NULL DEFAULT 'observation';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- A binary without the kind column would read an op update's payload as
-- an observation. Drop those rows with the column.
DELETE FROM `outbox` WHERE `kind` <> 'observation';
ALTER TABLE `outbox` DROP COLUMN `kind`;

-- +goose StatementEnd
