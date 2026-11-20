# Configuration

This page lists every configuration surface each slagworks binary
exposes: environment variables, command-line flags, database-driven site
configuration, image and secret conventions, and the OIDC settings
`slagctl` needs to authenticate. It is the reference companion to
[`installation.md`](installation.md).

Both daemons exit with status 78 (`EX_CONFIG`) when required
configuration is missing or unparseable. The packaged systemd units treat
78 as permanent and stop restarting.

## `slagworksd global`

The central control-plane process. Defined in `cmd/slagworksd/global.go`;
the example env file is `deploy/global.env.example`, installed as
`/etc/slagworks/global.env.example`.

Every setting is a flag whose default comes from the environment
variable shown, so a flag on the command line overrides the variable.

### Required

- **`SLAGWORKS_OIDC_ISSUER`** / `--oidc-issuer` — the `iss` value
  required on operator tokens: the identity provider URL operators'
  browsers reach.
- **`SLAGWORKS_OIDC_AUDIENCE`** / `--oidc-audience` — the `aud` value
  required on operator access tokens (the slagworks OIDC client ID).

### Database

- **`DATABASE_URL`** / `--dsn` — Postgres connection string, URL or
  `key=value` form. Default
  `host=localhost user=slagworks password=slagworks dbname=slagworks sslmode=disable`,
  which only suits local development. Migrations run at startup. Every
  connection gets `statement_timeout=30s`.
- **RDS IAM login.** When the DSN's host ends in `.rds.amazonaws.com` and
  the DSN has no password, global logs in with a fresh RDS IAM token per
  connection, signed with the default AWS credential chain; the region is
  taken from the hostname. This mode requires a user in the DSN and
  `sslmode=verify-full`, and verifies the server against an embedded RDS
  CA bundle unless the DSN names an `sslrootcert`. Any other DSN logs in as
  written.

### Optional

- **`LISTEN_ADDR`** / `--addr` — API and WebSocket listen address.
  Default `:8081`.
- **`NETBOX_URL`** / `--netbox-url` and **`NETBOX_TOKEN`** /
  `--netbox-token` — NetBox base URL and API token. The URL may carry a
  path, such as `https://netbox.example.com/netbox` for a NetBox served
  under `BASE_PATH` or behind a proxy prefix. Both must be set;
  either alone logs a warning and leaves NetBox disabled. Without NetBox,
  global serves the REST API but builds no snapshots, does not register
  the site-worker WebSocket (so no site worker can connect), the NetBox
  webhook, or the log and console streams, and does not enforce site-worker
  hardware serials.
- **`NETBOX_API_VERSION`** (environment only) — the NetBox API version
  slagworks sends in its `Accept` header. Default `4.6`. NetBox rejects
  requests whose version does not match its own major.minor, so set this
  when running against a different NetBox release.
- **`SLAGWORKS_OIDC_JWKS_URL`** / `--oidc-jwks-url` — where global fetches
  token signing keys, for deployments where global reaches the identity
  provider at a different address than the issuer. Default
  `<issuer>/jwks/`.
- **`SLAGWORKS_OIDC_REQUIRED_GROUPS`** / `--oidc-required-groups` —
  comma-separated groups; a token is accepted when it carries any of them.
  Required: global refuses to start without at least one group. Blank
  entries are dropped. Every operator endpoint uses this check; there is
  no separate read-only role. The device-credential endpoint and
  site-worker registration can also require
  `SLAGWORKS_OIDC_CREDENTIAL_GROUPS`.
- **`SLAGWORKS_OIDC_CREDENTIAL_GROUPS`** / `--oidc-credential-groups` —
  comma-separated groups allowed to read stored device credentials in
  plaintext: `GET /api/v1/devices/:ident/creds`, which
  `slagctl machine creds` calls, and `POST /api/v1/site-workers`, which
  `slagctl worker register` calls (a site-worker token receives the
  site's device credentials with every operation it is sent). When set, a
  caller must pass the `SLAGWORKS_OIDC_REQUIRED_GROUPS` check and also
  carry any one of these groups; other operators get 403 with a message
  naming this setting.
  Unset or blank (the default), every operator may call both.
  Blank entries are dropped. Global logs at startup whether credential
  reads are restricted and by how many groups. Endpoints that only use a
  credential server-side (operations, power and boot status) are not
  affected.
- **`SLAGWORKS_EXTERNAL_URL`** / `--external-url` — URL at which NetBox
  can reach global. Used only by `POST /api/v1/admin/netbox/bootstrap`
  (`slagctl netbox bootstrap`) to create the NetBox webhook pointing at
  `<url>/api/v1/hooks/netbox`; bootstrap refuses to run without it.
- **`SLAGWORKS_WEBHOOK_SECRET`** / `--webhook-secret` — HMAC key NetBox
  signs webhook deliveries with. Bootstrap refuses to run without it.
  When it is empty, global still starts, logs at startup that the webhook
  endpoint is disabled, and answers every delivery to
  `/api/v1/hooks/netbox` with 503 without processing it. Device changes
  then reach slagworks only through the 5-minute NetBox reconcile, and
  the webhook-only checks do not run: failing in-flight operations when a
  device's site, location, rack, type or role changes, and refusing a
  managed site's slug rename. Set it to the secret configured on the
  NetBox webhook. It also keys the PDU-outlet preflight tokens; when
  empty those use a random per-process key.
- **`TAILSCALE_OAUTH_CLIENT_ID`** / `--tailscale-oauth-client-id` and
  **`TAILSCALE_OAUTH_CLIENT_SECRET`** / `--tailscale-oauth-client-secret`
  — a Tailscale OAuth client with the `auth_keys` scope. When both are
  set, each deploy operation carries a single-use auth key (valid 2 hours)
  tagged `tag:<pop>`, where `<pop>` is the device's NetBox site slug, so
  the OAuth client must own those tags. Either empty disables enrollment;
  a mint failure is logged and the deploy proceeds without it. The same
  happens, with no key minted, when the device's tailnet hostname
  (`<name>-<pop>`, with a trailing `.<pop>` dropped from the name) is not a
  valid hostname: dot-separated labels of letters, digits and hyphens, none
  starting or ending with a hyphen.
- **`SLAGWORKS_HOSTNAME_TEMPLATE`** / `--hostname-template` — how a
  deployed machine's hostname is derived from its NetBox device name.
  The agent writes the result to `/etc/hostname` and `/etc/hosts`.
  Unset (the default), the hostname is the NetBox device name. The
  template must use at least one placeholder:
  - `{name}` — the full NetBox device name.
  - `{node}` and `{zone}` — the device name split at its **last** dot:
    `kworker-1.site1` has node `kworker-1` and zone `site1`;
    `foo.bar.site1` has node `foo.bar` and zone `site1`.

  For example, `{node}.internal.{zone}.example.net` gives a device
  named `kworker-1.site1` the hostname
  `kworker-1.internal.site1.example.net`. A device name that already
  ends with the literal text after the template's last placeholder
  (here `.example.net`) is used unchanged. When the template uses
  `{node}` or `{zone}` and a device name has no dot (or nothing on one
  side of its last dot), the machine gets no hostname from slagworks
  and keeps the deployed image's default. The same happens when the
  result is not a valid hostname: dot-separated labels of ASCII
  letters, digits and hyphens, none empty or starting or ending with
  a hyphen, at most 63 characters each and 253 in all (RFC 1123). A
  device name containing a space, `/`, `_` or a newline, for example,
  gives no hostname with or without a template. Any other `{...}`, a
  template with no placeholder, or a template whose literal text no
  device name could make a valid hostname (a character other than a
  letter, digit, hyphen or dot, an empty label, or a label starting
  or ending with a hyphen, as in `{node}.internal.{zone}.example_net`)
  is a configuration error: global refuses to start. Changing the
  template changes the hostname every machine gets on its next deploy.

### AWS

Global reads and writes the per-pop secrets described under "Secrets in
AWS Secrets Manager" below with the default AWS SDK credential chain. The
Secrets Manager region is fixed at `us-east-2`
(`internal/awssecrets/awssecrets.go`); `AWS_REGION` does not change it.

If the SDK configuration cannot be loaded, global logs a warning and runs
without a secret store: operations that need a BMC credential are refused
at creation, PDU and AMT operations (including `slagctl power outlet`) are
created without a credential, and `slagctl machine creds` returns 503. If
the configuration loads but no credentials are available, the same
operations fail when global first tries to read a secret.

## `slagworksd site`

The per-site worker process. Defined in `cmd/slagworksd/site.go`; the
example env file is `deploy/site.env.example`, installed as
`/etc/slagworks/site.env.example`.

The site worker reads only environment variables; it has no flags. The
`slagworksd site worker-id` subcommand prints the stored worker identity
(`--reset` clears it). Site identity (subnet, gateway, DHCP pools,
machines) is not configured here: it arrives in the snapshot global pushes
over the WebSocket, for the site config the worker's token is registered
against.

Boolean variables match one exact value: `SLAGWORKS_STANDBY` and
`SLAGWORKS_OOB_DHCP_ENABLED` are on only for `true`,
`SLAGWORKS_AUTO_UPGRADE` only for `1`.

### Required

- **`SLAGWORKS_API_URL`** — global's base URL, e.g.
  `https://slagworks.example.com`. The worker connects to
  `<url>/api/v1/ws/site`, using `wss://` for an `https://` URL and `ws://`
  for `http://`.
- **`SLAGWORKS_TOKEN`** — the token printed by `slagctl worker register`.
  Global stores only its SHA-256 hash; re-registering rotates it.
- **`SLAGWORKS_SERVER_IP`** — IPv4 address for the provisioning (fabric)
  leg. It is the DHCP server address handed to clients, and the TFTP and
  HTTP-boot listeners bind to it, so it must be assigned to this host when
  services start.
- **`SLAGWORKS_IFACE`** — interface the fabric DHCP server listens on
  (e.g. `eno3`, `bond0`).

### Identity

- **`SLAGWORKS_WORKER_ID`** — the worker's identity. Global rejects the
  connection unless this equals the `--worker-id` the token was
  registered with. When unset, the worker generates a UUID on first start
  and stores it in `worker.db`; `slagworksd site worker-id` prints it.
  The convention is to register with the host's FQDN and set this to the
  same value.

When NetBox is configured, global also checks the worker host's hardware
serial: the worker reads its SMBIOS serial at startup (`dmidecode`, with
a sysfs fallback), and global refuses the connection unless that serial
matches exactly one NetBox device claimed by the worker's site config.
There is no setting for this; model the worker host in NetBox with its
real serial.

### Optional

- **`SLAGWORKS_HTTP_PORT`** — HTTP-boot port. Default `8080`. A
  non-integer value is a configuration error.
- **`SLAGWORKS_TFTP_DIR`** — directory served over TFTP and, for UEFI HTTP
  boot, at `/files/ipxe/`. Default `/srv/tftp`, where the `slagworks`
  package installs `snponly.efi` and `undionly.kpxe`.
- **`SLAGWORKS_IMAGES_DIR`** — directory boot images are downloaded and
  extracted into, and from which `systemd-bootx64.efi` is served. Default
  `/srv/images`. The package links `systemd-bootx64.efi` into
  `/var/lib/slagworks/images`, not the default, and readiness fails while
  that file is missing, so package installs should set
  `SLAGWORKS_IMAGES_DIR=/var/lib/slagworks/images` (as
  `deploy/site.env.example` does).
- **`SLAGWORKS_DEFAULT_IMAGE`** — the image row boots use when the site
  has a row of this name. Otherwise boots use the site's only image row,
  whatever its name, and refuse when there are several; with no rows at
  all they use the directory of this name. Default `noble-pxe`.
- **`SLAGWORKS_DATA_DIR`** — local state directory. Default
  `/var/lib/slagworks`. The worker's SQLite database is
  `<data-dir>/worker.db`.
- **`SLAGWORKS_AGENT_BIN`** — the `slagworks-agent` binary served at
  `/files/bin/slagworks-agent`. Default `/usr/bin/slagworks-agent`, where
  the `slagworks-agent` package installs it.
- **`SLAGWORKS_HEALTH_ADDR`** — listen address for `/healthz`, `/readyz`
  and `/metrics`. Default `:9090`.
- **`SLAGWORKS_LOG_LEVEL`** — `debug`, `info`, `warn` (or `warning`), or
  `error`. Default `info`; any other value is treated as `info`.
- **`SLAGWORKS_AUTO_UPGRADE`** — when `1`, a worker whose build SHA
  differs from global's runs
  `sudo -n /usr/bin/slagworks-upgrade <sha> <package-version>` with the
  build global reported, retrying every 2 minutes while the mismatch
  lasts. Otherwise it logs the mismatch and stays on its version. It also
  stays put when global reports no package version (a global not built by
  `scripts/build-deb.sh`). See
  [installation](installation.md#site-worker-auto-upgrade) for what the
  helper does.

### HA (optional)

- **`SLAGWORKS_STANDBY`** — `true` makes this worker a standby: DHCP,
  TFTP, HTTP boot, BMC console capture and the operation executor start
  only while `SLAGWORKS_VIP` is assigned to a local interface, and stop
  when it goes away. The WebSocket connection, the health server and
  capture from console ports cabled to this host run either way.
- **`SLAGWORKS_VIP`** — the address to watch. The worker polls the host's
  interface addresses every 2 seconds; something else (for example
  keepalived; a template is in `deploy/keepalived.conf.tmpl`) must move
  the address. On a non-standby worker a set VIP only produces a warning
  when it disappears.

`SLAGWORKS_STANDBY=true` with no `SLAGWORKS_VIP` logs a warning and starts
the network services immediately, but does not start the operation
executor, so that worker never claims operations.

### Supermicro SMCIPMITool (optional)

- **`SLAGWORKS_SMCIPMITOOL`** — `auto` (default), `required`, or `off`.
  `auto` routes power, boot and chassis-serial calls for Supermicro hosts
  (NetBox manufacturer slug `supermicro`) through SMCIPMITool when it is
  installed and falls back to the generic BMC path when it is not;
  `required` fails those operations when it is missing; `off` never uses
  it. Any other value is a configuration error. See
  [Supermicro SMCIPMITool](hardware/supermicro-smcipmitool.md).
- **`SLAGWORKS_SMCIPMITOOL_BIN`** — launcher path or name. Default
  `SMCIPMITool`, looked up on `PATH` at each use.

### OOB DHCP takeover (optional)

- **`SLAGWORKS_OOB_DHCP_ENABLED`** — `true` lets the worker also serve
  DHCP, TFTP and HTTP boot on the out-of-band management segment. The
  segment comes from the snapshot's OOB config and the bind interface is
  whichever local interface has an address in that subnet; there is no
  interface setting. Nothing is bound when no local interface is on the
  subnet or when it would coincide with the fabric interface or server IP.
  Before binding DHCP the worker probes for another DHCP server on the
  segment; if one answers (or the probe fails) OOB DHCP stays off for the
  life of the process and readiness reports why, while OOB TFTP and HTTP
  still start. Default off, which leaves an existing OOB DHCP server in
  charge.

### Other inputs

- **`NOTIFY_SOCKET`** — set by systemd for the `Type=notify` site unit;
  the worker reports readiness through it.
- The worker shells out to `ipmitool` (SOL console capture, BMC user
  reconcile, the chassis-serial fallback) and `dmidecode` (hardware
  serial). Both are looked up on `PATH`.

## `slagctl`

The operator CLI. Defined in `cmd/slagctl/cmd/`.

### Common flags

- **`--api`** / `SLAGWORKS_API` — API server URL. There is no built-in
  default: slagctl uses the flag, then the environment variable, then
  the server saved by the last `slagctl auth login`
  (`$XDG_CONFIG_HOME/slagctl/config.json`). With none of the three it
  exits and tells you to run `slagctl auth login --api <url>`.
- **`--json`** — emit JSON instead of the human-readable tabular
  format.

### Operator authentication

Auth is OIDC only — the CLI does not accept bearer tokens on the
command line. slagctl learns which identity provider and client to use
from the server: global serves its own `--oidc-issuer` and
`--oidc-audience` at `GET /api/v1/client-config` (no authentication),
and `slagctl auth login` saves the answer next to the API URL. An
operator therefore configures one value, the API URL.

- The issuer must be reachable from the operator's laptop; the OpenID
  configuration is fetched from it.
- slagctl is a public client using PKCE, and the client ID is global's
  OIDC audience.
- **`SLAGWORKS_OIDC_ISSUER`** and **`SLAGWORKS_OIDC_CLIENT_ID`**, when
  both are set, replace what the server reports at login. They are for
  development and for a server too old to serve `client-config`.
- The cached token is bound to the server of the last login. A command
  aimed at another server (`--api`, `SLAGWORKS_API`) is refused until
  you `slagctl auth login --api` there, so a token is never sent to a
  server it was not issued for.
- The loopback redirect uses a fixed port (`8765`). This port must
  be registered with the IdP as an allowed redirect URI. If a local
  browser flow is not available, `slagctl auth login --device`
  uses the OIDC device-authorization grant instead.

Cached tokens land at
`$XDG_CONFIG_HOME/slagctl/token.json` (usually
`~/.config/slagctl/token.json`); `slagctl auth logout` deletes the
cache. `slagctl auth whoami` decodes and prints the current token's
identity claims.

## Site-config model

`site_configs` is the database table that scopes a site worker to a
NetBox site, optionally narrowed to one location, and carries the
per-site DHCP settings. One row per site-worker slug. Defined in
`internal/model/site_config.go`.

Fields:

- **`slug`** (primary key) — the site-config slug that worker tokens are
  registered against. Free-form; by convention the NetBox site slug for a
  whole-site config and `<site>-<cage>` for a location-scoped one.
- **`netbox_site`** — the NetBox site slug. Required; the API fills it
  from `slug` when omitted.
- **`netbox_location`** — optional NetBox location slug. When set, the
  config claims only devices in that location and its child locations,
  so several configs can split one NetBox site. Configs on the same site
  may not overlap: create and update refuse a config whose location is
  the same as, or nested with, another config's. Moving a location in
  NetBox can still nest two configured locations afterwards. Global then
  refuses both configs, as it does a config whose location is gone: it
  stops building their snapshots, so their workers keep serving the last
  one, and lists both under `stale_sites` in `/healthz`
  (`slagworks_site_stale` is 1). Deploy and commission preflight for
  their machines fails, and power status, boot status and outlet power
  for a device both configs claim fail with an error naming the configs.
  Move the location back in NetBox, or narrow or delete one of the
  configs; the next reconcile (every five minutes) clears the flags and
  rebuilds the configs that remain.
- **`ephemeral_pool_start`**, **`ephemeral_pool_end`** — the fabric DHCP
  pool. Required, IPv4, start no greater than end.
- **`relay_pools`** — pools for subnets reached through a DHCP relay,
  selected by the relay's `giaddr`. Each is
  `{"subnet","range_start","range_end","gateway"}`. The subnet must be an
  IPv4 CIDR in dotted-quad form (`10.254.27.0/24`, not IPv6 or
  IPv4-mapped notation such as `::ffff:10.254.27.0/120`); the range and
  gateway must be IPv4 inside the subnet, and the gateway must lie outside
  the range. On a subnet of /30 or wider the range must also exclude the
  network and broadcast addresses. The worker leases every address in
  the range and cannot ARP-probe a relayed subnet, so an address left in
  the range by mistake is handed to a client.
- **`promiscuous`** — when `true`, the fabric DHCP server answers MACs it
  does not know and TFTP stops requiring a lease. Default `false`.
- **`last_snapshot_seq`**, **`last_snapshot_hash`**, **`created_at`** —
  maintained by slagworks.

The subnet and gateway come from NetBox, not from this table: creating a
site config requires a prefix tagged `slagworks-production` scoped to the
site (or location). A whole-site config reads only the tagged prefixes
scoped to the site itself, never ones scoped to a location inside it, so a
location can be given its own `slagworks-production` and `slagworks-oob`
prefixes while the whole-site config is still serving. The DNS servers and lease time handed out on the
fabric leg come from that prefix's `dns_servers` and `dhcp_lease_seconds`
custom fields, defaulting to `1.1.1.1` and 5 minutes; workers read them
when their services start, so a change takes effect on restart. When the
site or location carries more than one `slagworks-production` prefix
(tagging a prefix is also how hosts in it get a gateway), the most specific
one containing `ephemeral_pool_start` supplies the subnet, gateway, DNS
servers and lease time, whatever order NetBox lists them in. When child locations of a location-scoped config also carry
`slagworks-production` prefixes, the prefix scoped to the config's own
location still supplies them, even if a child's prefix also contains the
pool. If no candidate prefix contains the pool, the first one NetBox lists
(the lowest address) is used and the snapshot build logs a warning naming
the pool and the prefix.

Manage site configs with
`slagctl site-config {list,show,create,update,delete}`. Creating one also
creates the pop's monitor secret in AWS Secrets Manager when it is absent
(see below).

## Image row

The database holds one `Image` row per image name (conventionally
`noble-pxe`) pinning an immutable tarball URL and its SHA-256
(`internal/model/image.go`). Site workers compare the on-disk `.sha256`
marker against the row on every snapshot apply and download on mismatch.
Deploy and commission boot the same image and differ only in the
`slagworks.mode=` kernel argument.

Change the pinned image by updating the row:

- `slagctl image follow-latest` — reads the `latest-sha.txt` pointer next
  to the image tarballs and creates or updates the row to point at that
  build.
- To pin a specific build (a rollback, or a candidate that is not
  `latest-sha.txt`), send `PUT /api/v1/images/<name>` with `version`,
  `sha256` and `url`. No `slagctl` command does this: `slagctl image
  create` only registers a new image name and gets 409 for an existing
  one.

Creating or updating an image row pushes a fresh snapshot to every site.

## Secrets in AWS Secrets Manager

Two secrets per pop, read by `internal/awssecrets/`. The pop is the NetBox
site slug of the device in question.

- **`pop/<pop>/bmc-creds-admin`** — the out-of-band credentials for every
  device in the pop. Entries are keyed by device serial (the NetBox
  `device.serial` field); each has a `kind`, `username`, `password` and
  free-form `extra_config`. Slagworks reads this secret but never writes or
  creates it. A top-level `_meta` key is ignored.

  The parser passes every entry through unchanged; the kinds differ in
  how callers use them:

  - **`bmc`** — the BMC's slot-2 admin account, the only credential
    slagworks uses to drive a BMC (power, boot, SOL, chassis-serial
    checks, `bmc-reconcile`). `extra_config` carries vendor quirks such as
    the IPMI cipher suite (`{"ipmi_cipher": 17}`; suite 17 is also the
    default).
  - **`amt`** — an Intel AMT endpoint. Slagworks drives it over WS-Man on
    TLS port 16993 and reads its console over redirection on port 16995,
    at the device's NetBox `oob_ip`. Its presence makes `amt` the default
    power transport for that device.
  - **`pdu`** — switched-PDU (APC NMC) credentials, used for PDU outlet
    power operations.
  - **`serial`** — a device reached through a console server port, and
    **`serial-server`** — the console server itself. Slagworks does not
    interpret these entries; it only returns them through
    `slagctl machine creds`. Their `extra_config` layout belongs to
    whatever console tooling reads them.

  Slagworks keeps no BMC account of its own, but a credential it resolves
  is copied where it is used: into the `params` of each operation row in
  Postgres (returned by the operations API), and into the site snapshot,
  which site workers hold in memory and in `worker.db`.

- **`pop/<pop>/bmc-creds-monitor`** — passwords for the read-only
  `monitor` account (IPMI USER privilege) slagworks provisions on each BMC
  for external monitoring; slagworks never logs in with it. Keyed by
  device serial, with `kind: "bmc"`, the same four fields, and
  `last_known_hostname` / `last_known_ip`. Written after a deploy's BMC
  reconcile (`deploy_complete`) and by `slagctl machine bmc-reconcile`.
  An entry can also carry `unconfirmed_passwords`: generated passwords
  whose write to the BMC failed with the outcome unknown, so the BMC holds
  either `password` or one of them. The next `bmc-reconcile` of the device
  settles which and removes the list; consumers keep reading `password`.
  Creating a site config creates this secret with a `_meta`-only body if
  it does not exist.

Both secrets share the top-level shape:

```json
{
  "_meta": {"tier": "admin", "schema_version": 2},
  "<bmc-serial>":         {"kind": "bmc", "username": "ADMIN", "password": "...", "extra_config": {"ipmi_cipher": 17}},
  "<pdu-serial>":         {"kind": "pdu", "username": "apc", "password": "...", "extra_config": {}},
  "<device-serial>":      {"kind": "serial", "username": "root", "password": "...", "extra_config": {"<serial-specific fields>": "..."}},
  "<console-srv-serial>": {"kind": "serial-server", "username": "root", "password": "...", "extra_config": {"<serial-server-specific fields>": "..."}}
}
```

Preflight (`slagctl machine preflight <host>`) checks that the pop's
admin secret exists, and for a deploy that its monitor secret exists,
using `DescribeSecret` so a missing secret is distinguished from an AWS
error. `bmc-reconcile` also refuses when the monitor secret is missing.

Global's AWS identity needs:

- `GetSecretValue`, `DescribeSecret` on `pop/*/bmc-creds-admin-*`
- `GetSecretValue`, `DescribeSecret`, `PutSecretValue`, `CreateSecret` on
  `pop/*/bmc-creds-monitor-*`

## Other secrets in global's environment

Global's remaining secrets arrive as environment variables:
`DATABASE_URL` (when it carries a password), `NETBOX_TOKEN`,
`SLAGWORKS_WEBHOOK_SECRET`, and the optional
`TAILSCALE_OAUTH_CLIENT_ID` / `TAILSCALE_OAUTH_CLIENT_SECRET`. The env
example files are installed mode 0640, group `slagworks`.

## Values not currently configurable

- **AWS region for Secrets Manager** — `us-east-2`.
- **OIDC loopback redirect port** for `slagctl auth login` — fixed at
  `8765`.
- **Operation timeouts** — a pending operation fails after 24 hours, a
  claimed or running one after 30 minutes, a deploy waiting for phone-home
  after 30 minutes in that phase, and a claim with no progress after 2
  minutes (`internal/api/server.go`, `NewServer`).
- **NetBox reconcile interval** — 5 minutes.
- **Postgres statement timeout** — 30 seconds.
