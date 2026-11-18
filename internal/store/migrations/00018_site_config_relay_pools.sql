-- +goose Up
-- Relay DHCP pools: per-site list of {subnet, range_start, range_end,
-- gateway} served to PXE clients behind dhcp-helper relays on leaf
-- bridges. Empty/null means the site serves only its directly-attached
-- ephemeral pool (the MVP single-subnet behavior).
ALTER TABLE site_configs ADD COLUMN relay_pools JSONB;

-- +goose Down
ALTER TABLE site_configs DROP COLUMN relay_pools;
