-- +goose Up
-- +goose StatementBegin

-- Collapse the two-image model to a single PXE image. deploy and commission
-- are the SAME artifact booted in different modes (the slagworks.mode= kernel
-- cmdline, chosen per-operation by the boot handler), not different images.
-- An earlier change converged the BUILD (one PXE tarball published to both flavors); the
-- separate noble-deploy / noble-commission Image rows let the two pin different
-- SHAs and drift (deploy rotted 28 days behind commission, booting a pre-
-- bnxt_en initrd). One row, one SHA, no drift possible.
--
-- Drop the kind column and remove the legacy per-kind rows. The operator
-- re-establishes the single row with `slagctl image follow-latest` (now
-- kind-less), which upserts a "noble-pxe" row. Slagworks is pre-production with
-- no rollback target older than this; deleting the old rows is fine (the next
-- follow-latest recreates the row, and site workers re-sync on the next tick).

DELETE FROM images WHERE kind IN ('deploy', 'commission');
ALTER TABLE images DROP COLUMN IF EXISTS kind;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE images ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'deploy';

-- +goose StatementEnd
