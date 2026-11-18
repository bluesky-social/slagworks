-- +goose Up
-- +goose StatementBegin

-- Lease-history ledger (Phase 3 of docs/plans/2026-07-13-dhcp-lease-
-- visibility.md). Site workers emit dhcp_lease observations over the
-- existing crash-safe outbox on every lease-table mutation; global
-- folds them into this table. released_at NULL means the row is the
-- currently-open lease for its (site_slug, worker_id, mac, ip); a
-- non-NULL released_at + release_reason ("superseded" | "deleted")
-- records how it closed.
CREATE TABLE dhcp_lease_ledger (
    id             BIGSERIAL PRIMARY KEY,
    site_slug      TEXT NOT NULL,
    worker_id      TEXT NOT NULL,
    mac            TEXT NOT NULL,
    ip             TEXT NOT NULL,
    machine_id     UUID,
    kind           TEXT NOT NULL,          -- 'static' | 'dynamic'
    pool           TEXT,                   -- subnet label, '' when unknown
    first_seen     TIMESTAMPTZ NOT NULL,
    last_seen      TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    released_at    TIMESTAMPTZ,            -- NULL = open
    release_reason TEXT                    -- 'superseded' | 'deleted'
);

CREATE INDEX dhcp_ledger_site_mac ON dhcp_lease_ledger (site_slug, mac);
CREATE INDEX dhcp_ledger_site_ip  ON dhcp_lease_ledger (site_slug, ip);

-- One open row per (site, worker, mac): an allocate/renew for a
-- different IP first closes the old row as superseded before
-- upserting the new one (see RecordDHCPLeaseEvent's fold).
CREATE UNIQUE INDEX dhcp_ledger_open_mac
    ON dhcp_lease_ledger (site_slug, worker_id, mac)
    WHERE released_at IS NULL;

-- One open row per (site, worker, ip): keeps the ledger honoring
-- DHCP's one-holder-per-IP invariant when an address is reallocated
-- to a new MAC after the old holder's lease expired without a
-- delivered delete observation (crash between expiry-sweep delete
-- and the subsequent allocate).
CREATE UNIQUE INDEX dhcp_ledger_open_ip
    ON dhcp_lease_ledger (site_slug, worker_id, ip)
    WHERE released_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE dhcp_lease_ledger;
-- +goose StatementEnd
