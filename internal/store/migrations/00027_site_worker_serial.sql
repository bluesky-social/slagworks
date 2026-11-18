-- +goose Up
-- +goose StatementBegin

-- Hardware-attested worker self-identity (P4a of the PDU
-- outlet-cycle plan). The site worker reports its SMBIOS serial in the WS
-- hello; global verifies it against the NetBox device and records the
-- verified serial here so the claim path (P4b outlet-op interlock) can read
-- a worker's serial store-side. Empty default: pre-migration rows and workers
-- that report no verified serial (unverified board serial, or an old binary
-- mid-rollout) stay empty and are ineligible for the outlet interlock.
ALTER TABLE site_workers
    ADD COLUMN serial text NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE site_workers DROP COLUMN serial;
-- +goose StatementEnd
