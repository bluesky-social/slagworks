# Database

Status: live (generated from code, 2026-07-03)

Slagworks runs two independent databases. Nothing is replicated between
them; each side owns a different problem.

- **Global Postgres** — the authoritative record of machines, operations,
  images, scripts, site configs, logs, and console casts. Consumed by the
  global `slagworksd` daemon and every operator API path (`slagctl`).
- **Site SQLite** — a per-worker cache for warm restart, DHCP lease
  recovery, and the outbox that ships observations up to global. It
  holds no authoritative state; a wipe on a site worker recovers from
  the next global snapshot.

## Global Postgres

### Migration system

Goose (`github.com/pressly/goose/v3`) drives Postgres migrations. The
SQL files live under `internal/store/migrations/`, embedded into the
binary via `//go:embed *.sql` in `internal/store/migrations/migrations.go`.
`store.Migrate(*gorm.DB)` runs them on startup; see
`internal/store/migrations.go`.

Rules from the code:

- Postgres is the only supported dialect. `Migrate` refuses anything
  else — a previous SQLite fallback silently drifted from prod on every
  schema change and was deleted.
- Migrations are numbered `NNNNN_short_name.sql` and applied in order.
  Standard goose `-- +goose Up` / `-- +goose Down` markers wrap each
  half; multi-statement blocks are wrapped in `-- +goose StatementBegin`
  / `-- +goose StatementEnd`.
- `Down` is written for the trivial cases (`DROP COLUMN`, `DROP TABLE`),
  but destructive reshapes (00011 NetBox SSoT, 00013 state-machine
  redesign) intentionally leave a `SELECT 'down migration not supported'`
  marker. Rollback path is `pg_restore` from a snapshot, not a Down
  migration.
- Adding a new migration: create the next-numbered file under
  `internal/store/migrations/`. `embed.FS` picks it up automatically.
  Tests run against the same file set (see `testpg` below), so a
  broken migration fails locally before it ships.

### Store layer

`internal/store/store.go` wraps a `*gorm.DB` (`store.Store`) and
exposes a hand-written method surface — one method per query. GORM
handles the SQL glue; nothing is generated. Patterns to know:

- Errors: `ErrMachineNotFound` and `ErrMachineLocked` are the two
  sentinel errors. Everything else surfaces as raw `gorm.ErrRecordNotFound`
  or a wrapped `fmt.Errorf`.
- IDs: UUIDs everywhere for user-visible entities (machines,
  operations, images, scripts). `bigserial` where slagworks itself
  drives the sequence (log envelopes, console cast events).
- Advisory locks: `Store.WithPopAdvisoryLock(ctx, popSlug, fn)` wraps
  a `pg_advisory_xact_lock` keyed on the pop slug. Used to serialize
  per-pop reconciliation so two admin actions on the same pop don't
  interleave.
- Deletions on operator-editable rows (`machines`, `operations`,
  `ssh_keys`, `dhcp_pools`, `storage_scripts`, `images`,
  `network_scripts`) fire an `AFTER DELETE` trigger that emits a
  `pg_notify('slagworks', '<table>:deleted:<id>')`. The snapshot
  reconciler LISTENs on that channel and rebuilds affected sites.

### Schema tour

The current shape is 00001 minus what 00011 dropped, plus everything
added through 00025. Read the migration files if you need column-level
detail; this is the map.

- `meta(instance_id text)` — one row, generated at first-boot. Site
  workers refuse to reuse a local cache if the global's instance_id
  doesn't match (see `LocalDB.ResetSyncState`).
- `machines(id uuid, netbox_serial text, netbox_device_id bigint,
  state, desired_os, current_os, commission_data jsonb,
  commissioned_at, locked, pxe_eligible, created_at)`. Identity is
  `netbox_serial` (unique index); slagworks owns runtime state only.
  Hostname, site, BMC IP, and NIC list all live in NetBox after
  migration 00011. No BMC credential: `bmc_user`/`bmc_pass` were
  dropped in 00030 (the device's `pop/<pop>/bmc-creds-admin` entry is
  the only BMC credential).
- `machine_interfaces` — added in 00001, dropped in 00011. NetBox owns
  the NIC list now.
- `operations(id uuid, machine_id, type, status, phase, phase_entered_at,
  claimed_by, claimed_at, created_at, completed_at, params jsonb,
  result jsonb, trace_id uuid)`. `trace_id` (00015) is
  stamped into the iPXE kernel cmdline and every log envelope for a
  1:1 op ↔ trace binding. `phase_entered_at` (00014) drives per-phase
  timeouts (phone-home in particular). The unique partial index
  `idx_one_active_op_per_machine` guarantees at most one pending /
  claimed / running op per machine.
- `images(id uuid, name, version, arch, sha256, url, created_at)`.
  Collapsed to one PXE image row by 00022 — deploy and
  commission boot the same bytes and differ only by the
  `slagworks.mode=` kernel cmdline. The `kind` column is gone.
- `storage_scripts` / `network_scripts` — NetBox-identity-matched
  shell scripts. `priority` is a match tiebreaker, not a unique key
  (00021 dropped the unique constraint that wrongly rejected disjoint
  matchers at the same priority). `name` stays unique.
- `site_configs(slug PK, netbox_site, netbox_location, ephemeral_pool_start,
  ephemeral_pool_end, promiscuous, relay_pools jsonb,
  last_snapshot_seq, last_snapshot_hash, created_at)`. One row per
  worker identity. `slug` is the worker key; `netbox_site` +
  `netbox_location` (00019) narrow which NetBox scope the worker
  mirrors, so two workers can serve disjoint cages of one NetBox site.
  `relay_pools` (00018) is the list of relayed DHCP scopes the worker
  serves in addition to its directly-attached ephemeral pool.
- `site_workers(id, site_slug, worker_id, fqdn, first_seen_at,
  last_seen_at, token_hash)`. `token_hash` (00012) is `sha256(bearer)`;
  the plaintext is returned once at registration and never persisted.
- `dhcp_pools` — dropped in 00011. Pool configuration moved into
  `site_configs`.
- `ssh_keys(id, public_key, comment, created_at)` — public keys the
  site worker installs into deployed hosts. `public_key` is unique.
- `deploy_logs` — original per-line log table from 00001. Superseded
  by `log_envelopes`; dropped in 00023. The `sync_seq` columns,
  trigger, and sequence from the pre-WS polling era were dropped in
  00025.
- `log_envelopes(id bigserial, ts, global_received_at, received_at,
  level, source, host, machine_id, op_id, trace_id, phase, msg,
  payload jsonb)` — one row per structured log envelope from agents,
  SOL pumps, site workers, and global itself. Live-tail queries lag
  by 500ms past `now()` so an in-flight transaction can't allocate a
  sequence id inside the read window and get skipped. Emitter clocks
  can be arbitrarily skewed (agents pre-NTP); the watermark uses
  `global_received_at`, which is server-assigned.
- `console_casts(id bigserial, machine_id UUID UNIQUE, cast_started_at,
  width, height)` and `console_cast_events(id bigserial, cast_id,
  t_us bigint, received_at, data bytea)` — one perpetual asciicast
  per machine, appended forever. `t_us` is monotonic-per-cast
  microseconds; the site worker computes it as `prev_max_us +
  monotonic_since_pump_start` and the on-demand resume-offset RPC
  keeps it monotonic across pump restarts. `data` is `bytea` because
  SOL bytes are not guaranteed UTF-8.

### testpg

`internal/testpg/testpg.go` is the shared-Postgres harness for unit
tests that need real Postgres.

- One container per test binary, brought up on first `testpg.DB(t, migrate)`
  call via `sync.Once`. Reused across tests in that binary via
  `TRUNCATE ... RESTART IDENTITY CASCADE` between calls.
- Discovers the table set from `pg_tables` at truncate time — a
  static allowlist silently drifted as migrations added tables, so
  discovery is now the pattern.
- Doesn't import `internal/store` to avoid an import cycle; the
  caller passes `store.Migrate` as a `MigrateFn` parameter.
- Requires Docker. On developer laptops Colima works; the harness
  auto-discovers via `DOCKER_HOST`, `/var/run/docker.sock`,
  `~/.colima/default/docker.sock`, and `~/.docker/run/docker.sock`.
- Pulls `postgres:18-alpine` through the ECR pull-through cache in
  CI (via `internal/dockermirror`) so Docker Hub anonymous-pull
  rate limits don't flake the suite. With
  `SLAGWORKS_DOCKER_HUB_MIRROR` unset, pulls go to Docker Hub.
- Consuming packages must call `testpg.Shutdown()` from `TestMain`
  so the container is torn down at end of run — the container's
  `AutoRemove` only fires when its main process exits, and Postgres
  runs forever. As a safety net the setup also calls
  `testpg.Expire(..., time.Hour)` so a SIGKILL'd test process leaves
  at most an hour of stray container.

## Site SQLite

`internal/worker/localdb.go` is the site worker's on-disk cache. Not
authoritative: the worker rebuilds everything it needs from the
global's snapshot stream on reconnect, and the local file's job is
to keep the worker useful during the reconnect window and across
restarts.

Single-writer (the worker process); opened WAL-mode via
`glebarez/sqlite` (pure-Go, no CGO — matches the toolchain constraint
described in `docs/dev/testing.md`). Schema is `db.AutoMigrate`'d at
open time — there is no goose-style migration file set on the site
side, and there deliberately isn't one: the local file is disposable.

Tables (all defined by GORM structs in `localdb.go`):

- `kv(key PK, value)` — the worker's own identity (`worker_id`,
  `worker_fqdn`), the sync cursor (`sync_cursor`), the global's
  `instance_id`, and the non-machine half of the last snapshot
  (`snapshot_meta` — site info, DHCP config, images, ssh keys,
  operations, seq — as a JSON blob).
- `outbox(id autoincrement, payload text, created_at)` — every
  observation the worker sends to global is persisted here first
  and only deleted (`AckOutboxRow`) when global's
  `rpc_response{ok:true}` or a terminal rejection arrives, or when the
  drain finds the observation too large to send (over
  `ws.MaxMessageBytes`) or impossible to encode. Retry state
  (`Attempts`, `NextAttempt`) is in-memory only; a worker restart
  resets the backoff ladder intentionally.
- `cached_machines(machine_id PK, state, data)` — the machine
  snapshot list, one row each. Written in the same transaction as
  the `snapshot_meta` kv row (`SaveSnapshot`) so the machine list
  and the seq cursor can't disagree after a mid-write crash.
- `dhcp_leases(mac PK, ip, expires_at)` — active leases persisted
  through restart. Expired rows are lazily deleted by
  `LoadDHCPLeases`.
- `current_deploy_ops(machine_id PK, operation_id)` — gates
  progress reports against ops the cache hasn't yet seen via
  snapshot. Kept after the `deploy_logs` and `sol_lines` tables were
  removed for the structured-logs pipeline.

## Running against production

`just sql-prod "<sql>"` runs one query and `just sql-prod-it` opens an
interactive `psql` against the production global database. psql runs in a
self-expiring client pod in the `slagworks` namespace that reads the app's
database credentials from its Kubernetes Secret, so no port is forwarded and
no credential reaches your machine; see README, "Querying the production
database". Reads are fine, mutations follow the ordinary consent rule in
`CLAUDE.md`. Don't stand up a socat proxy or port-forward to the database
instead: that is what these recipes replaced, and a forgotten proxy pod is an
open path to production.
