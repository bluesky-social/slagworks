# Adding a site worker

Status: live (2026-07-03; deployment-automation specifics live outside this
repo)

A **site worker** is a `slagworksd site` process (the `slagworksd-site`
systemd unit) running on a per-site gateway host. It connects to the global
slagworksd over a WebSocket, receives pre-built snapshots, and drives the
site's hardware: DHCP/TFTP/HTTP boot, BMC power/SOL, image sync.

This doc covers standing up a **new** site worker: prerequisites, token mint,
and host configuration. It assumes the host is OS-provisioned (by slagworks
itself, or otherwise) and reachable.

## How a site worker authenticates

The worker authenticates to global with a **per-worker bearer token**:

- `slagctl worker register --slug <site-config-slug> --worker-id <id>`
  calls `RegisterSiteWorker` on global, which mints a token, stores **only
  its sha256 hash** in the `site_workers` Postgres table, and returns the
  plaintext **exactly once**.
- The worker reads the plaintext from `SLAGWORKS_TOKEN` in
  `/etc/slagworks/site.env` and sends it in the WebSocket upgrade's
  `Authorization: Bearer` header, never in the URL, so it stays out of
  request and proxy logs. A proxy in front of global must pass that
  header through. The WS hub rejects a `hello` whose `worker_id`
  doesn't match the registration the token was minted for.
- Re-registering the same `(slug, worker_id)` pair **rotates** the token
  (mints a fresh one, invalidates the old). The plaintext is unrecoverable
  after mint — Postgres holds only the hash — so if you lose it, you
  re-register.
- The token is as sensitive as the site's device credentials: global sends
  a connected worker every operation for its site with the BMC, PDU, AMT
  and monitor passwords and the tailnet auth key in it. When
  `SLAGWORKS_OIDC_CREDENTIAL_GROUPS` is set, registering (and rotating)
  needs one of those groups as well as an operator group, the same as
  `slagctl machine creds`.

Because the plaintext is shown once, persist it somewhere your config
management can template it into `site.env` reproducibly (a secrets manager
your provisioning tooling reads).

> `--slug` is the **site-config slug**, which is NOT always the NetBox site
> slug. A whole-site config's slug equals the NetBox site; a location-scoped
> config has its own slug. Register against the slug from
> `slagctl site-config list` — the same slug the worker's snapshots are
> built for. (The `slagctl worker register --slug` flag help says "NetBox
> site slug"; that's stale — it's the site-config slug.)

## Prerequisites

Before the worker will function, the site needs:

1. **A site-config** for the slug (`slagctl site-config list` shows it). If
   absent, `slagctl site-config create` it first — that also bootstraps the
   site's BMC monitor secret.
2. **The site's BMC secrets** (admin and monitor — see
   [configuration.md](configuration.md) for the secret layout). Preflight
   refuses deploys without them; the monitor secret is auto-created at
   site-config create, the admin secret is owned by your credential-rotation
   tooling and must be bootstrapped there.
3. **A bindable IP on the boot network.** The worker REQUIRES
   `SLAGWORKS_SERVER_IP` + `SLAGWORKS_IFACE` — it binds TFTP/HTTP/DHCP to
   that address on that interface. The address must be directly connected
   and up before the worker starts.

## Steps

1. **Register the worker (mint the token):**

   ```bash
   slagctl worker register \
     --slug <site-config-slug> \
     --worker-id <host-fqdn> \
     --fqdn <host-fqdn> \
     --json
   ```

   Capture the `token` from the JSON output — it is shown **once**. Use a
   stable, meaningful `worker-id`; the host FQDN is the convention.

2. **Store the token** in your secrets manager as a bare string (no JSON
   wrapper), for config management to template into `site.env`.

   > Reusing an existing worker's token: if a worker is already running and
   > you only need to make its config reproducible (no rotation), harvest
   > the live value from its `/etc/slagworks/site.env` `SLAGWORKS_TOKEN=`
   > line and store THAT, rather than re-registering (which rotates and
   > forces a restart).

3. **Configure the host.** Install the `slagworks` package, write
   `/etc/slagworks/site.env` (see `deploy/site.env.example` for the full
   variable surface), and enable `slagworksd-site`. If the worker is the
   DHCP authority for its segment, set `SLAGWORKS_OOB_DHCP_ENABLED=true` and
   make sure no other DHCP server binds :67 on that interface.

4. **Verify:**

   ```bash
   slagctl worker list   # worker shows online, under its site-config slug
   ```

   On the host, `systemctl is-active slagworksd-site` is `active` and its
   logs show the WS `hello` accepted and the first snapshot synced.

## Notes / gotchas

- **Worker delete.** The `site_workers` registry is register/rotate/delete.
  `slagctl worker delete <ident>` removes a worker's registration row by
  uuid, worker-id, or fqdn — but refuses while the worker is currently
  connected or has an in-flight op claimed, so a live worker must be
  stopped before it can be deleted.
- **worker_id ↔ token binding is strict.** If `SLAGWORKS_WORKER_ID` in
  `site.env` doesn't match the id the token was registered for, the hub
  rejects the connection.
- **HA pairs.** A redundant gateway pair each runs its own worker with its
  own token and bind address; DHCP authority follows the externally-managed
  VIP (`SLAGWORKS_VIP` is watched, not managed — run keepalived or
  equivalent). Register and configure each worker independently.
