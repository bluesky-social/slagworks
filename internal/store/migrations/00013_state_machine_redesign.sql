-- +goose Up
-- +goose StatementBegin

-- State machine redesign: collapse the organic-growth state set to six
-- forward-path states. See docs/archeology/specs/2026-05-23-state-machine-redesign.md.
--
-- Renames:
--   ready    -> commissioned (better name; happened-not-property-of-world)
--   verified -> running      (one canonical "OS is up" state)
--   deployed -> running      ("agent reported deploy_complete" alone is no
--                              longer a state; phone-home is what gates
--                              `running`. Existing `deployed` rows are
--                              best-mapped to `running` since we have no
--                              way to retroactively know if the OS booted)
--
-- Drops (rows in these states are deleted, FK children too):
--   unknown, unmanaged, decommissioned, purged
--
-- For the dropped states we explicitly delete child rows in
-- `operations` (no cascade) before deleting the parent in `machines`.
-- `machine_interfaces` has ON DELETE CASCADE already. `deploy_logs`
-- has no FK to machines, so its rows are orphaned but harmless;
-- delete them too to keep the audit trail clean.

-- Step 1: rename state values still in active use.
UPDATE machines SET state = 'commissioned' WHERE state = 'ready';
UPDATE machines SET state = 'running'      WHERE state = 'verified';
UPDATE machines SET state = 'running'      WHERE state = 'deployed';

-- Step 2: gather victim machine IDs (rows in dropped states) and clean
-- up child rows before deleting machines.
DELETE FROM operations
 WHERE machine_id IN (
   SELECT id FROM machines
    WHERE state IN ('unknown', 'unmanaged', 'decommissioned', 'purged')
 );

DELETE FROM deploy_logs
 WHERE machine_id IN (
   SELECT id FROM machines
    WHERE state IN ('unknown', 'unmanaged', 'decommissioned', 'purged')
 );

-- Step 3: delete the parent machines. machine_interfaces cascades.
DELETE FROM machines
 WHERE state IN ('unknown', 'unmanaged', 'decommissioned', 'purged');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Not supported by design. Restore from pg_dump if you need to roll back.
SELECT 'down migration not supported' AS note;
-- +goose StatementEnd
