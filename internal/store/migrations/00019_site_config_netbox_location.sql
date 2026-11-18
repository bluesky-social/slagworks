-- +goose Up
-- Location-scoped site configs (pop2 dual-cage). Split the worker
-- identity (slug, PK) from the NetBox scope: netbox_site (required) is
-- the NetBox site this worker mirrors; netbox_location (optional, ''
-- means whole-site) narrows to one location within that site so two
-- workers can serve disjoint cages of a single NetBox site. See
-- docs/archeology/plans/2026-06-12-location-scoped-site-configs.md.
--
-- Backfill: existing rows are whole-site configs whose slug already IS
-- the NetBox site slug (pop1/pop3), so netbox_site = slug and
-- netbox_location stays ''. Hard cut — no compat shim.
ALTER TABLE site_configs ADD COLUMN netbox_site TEXT NOT NULL DEFAULT '';
ALTER TABLE site_configs ADD COLUMN netbox_location TEXT NOT NULL DEFAULT '';
UPDATE site_configs SET netbox_site = slug WHERE netbox_site = '';

-- +goose Down
ALTER TABLE site_configs DROP COLUMN netbox_location;
ALTER TABLE site_configs DROP COLUMN netbox_site;
