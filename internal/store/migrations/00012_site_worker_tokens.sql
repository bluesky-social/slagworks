-- +goose Up
-- +goose StatementBegin

-- Plan 2: site workers authenticate to the global WS hub via a per-worker
-- bearer token. Global stores only sha256(token); plaintext is returned
-- once at registration time and never persisted. Rotation is
-- re-registration.
--
-- The unique index is partial on token_hash <> '' so rows created before
-- a token is minted (shouldn't happen in practice — RegisterSiteWorker
-- mints on first call — but defensive) don't collide.

ALTER TABLE site_workers ADD COLUMN token_hash text NOT NULL DEFAULT '';
ALTER TABLE site_workers ALTER COLUMN token_hash DROP DEFAULT;
CREATE UNIQUE INDEX idx_site_workers_token_hash
    ON site_workers (token_hash)
    WHERE token_hash <> '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Not supported by design. Restore from pg_dump if you need to roll back.
SELECT 'down migration not supported' AS note;
-- +goose StatementEnd
