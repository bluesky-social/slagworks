# slagworks API

Reference for every HTTP route and WebSocket message served by
`slagworksd global`. The route table is `registerRoutes` in
`internal/api/server.go` (plus `RegisterHealthz` and `RegisterMetrics`);
WebSocket message types are in `internal/ws/messages.go`. If a route is
registered there and missing here, or listed here and not registered, this
page is wrong.

---

## Server

- One HTTP server (Echo) inside `slagworksd global`, listening on
  `LISTEN_ADDR` / `--addr` (default `:8081`).
- `/healthz`, `/readyz` and `/metrics` live at the root; everything else is
  under `/api/v1`.
- Request bodies are capped at 12 MiB.
- Several routes exist only when the matching component is wired. In
  `slagworksd global` those components are built only when NetBox is
  configured (`NETBOX_URL` and `NETBOX_TOKEN`). Without NetBox the
  webhook, the site-worker WebSocket, the log stream, the console stream
  and `console-probe` are not registered, and `/healthz` / `/readyz` fall
  back to a Postgres ping.

Site workers (`slagworksd site`) run a separate HTTP server on the
provisioning network for iPXE, image files, the agent binary and agent
reports. Those endpoints are described in
[`docs/user/boot-process.md`](../user/boot-process.md), not here.

## Auth

The Auth column in the tables below uses these classes:

| Class | Mechanism |
|---|---|
| `read`, `admin` | OIDC bearer token: `Authorization: Bearer <JWT>`, on WebSocket upgrades too. Verified against `SLAGWORKS_OIDC_ISSUER`, `SLAGWORKS_OIDC_AUDIENCE` and the JWKS (`SLAGWORKS_OIDC_JWKS_URL`, default `<issuer>/jwks/`); the token must carry at least one of `SLAGWORKS_OIDC_REQUIRED_GROUPS` (required; global refuses to start without one). 401 for a missing, malformed, invalid or expired token; 403 for a valid token outside the required groups. |
| `creds` | As `read`, and when `SLAGWORKS_OIDC_CREDENTIAL_GROUPS` is set the token must also carry at least one of those groups; otherwise 403 with a message naming that setting (the body echoes no group names; the refusal is logged at Info with the subject and both group lists). Unset, the same as `read`. |
| `worker` | Per-worker token minted by `POST /api/v1/site-workers`, passed as `?token=` on the WebSocket upgrade. Checked against `site_workers.token_hash` (SHA-256); a bad token gets 401 before the upgrade. |
| `hmac` | `X-Hook-Signature` header: hex HMAC-SHA512 of the body keyed with `SLAGWORKS_WEBHOOK_SECRET`; a missing or wrong signature gets 403. When that secret is empty every delivery gets 503 and nothing is processed. |
| `none` | No authentication. |

`read` and `admin` are two route groups in `server.go`, but both use the
same middleware and the same group check. There is no read-only role: any
token that passes the check can call every `read` and `admin` route. The
one finer check is `creds`, on the only route that returns a stored device
credential in plaintext (`GET /devices/:ident/creds`); its route-level
`credentialAuth` middleware runs after the operator check. Routes that use
a credential only server-side (operation creation, power and boot status)
are not `creds`.

The operator WebSocket streams (`/api/v1/logs/stream`,
`/api/v1/console/stream`) are `read` routes: the token goes in the upgrade
request's `Authorization` header, never in the URL, and a bad token gets
401 or 403 before the upgrade. An upgrade with no `Origin` header
(`slagctl`) is accepted; a browser upgrade is accepted only from the API's
own origin (its `Origin` host equals the request `Host`), and any other
origin gets 403.

## Conventions

- Errors: `{"message": "<text>"}` with the HTTP status (Echo's default
  error shape).
- Request and response bodies are JSON.
- Machine identifiers: every `/machines/:id` route except `netbox-diff`
  accepts either the slagworks machine UUID or the NetBox device name
  (anything containing a `.`, e.g. `kworker-l34-1.pop1`). Name lookup
  needs NetBox; without it a name returns 503.
- Operation-creating routes return `201` with the new operation row. An
  operation row includes `params`, which names the transport resolved at
  creation (`bmc_runtime`, `pdu_runtime`, `amt_runtime`, `monitor_runtime`,
  `tailscale_runtime`); the credentials inside are redacted in responses
  (see Operations).

---

## Health and metrics

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/healthz` | none | 200 once the first NetBox reconcile has completed, else 503. Body `{"ready","stale_sites","version","instance_id"}`; stale sites are informational and never cause a 503. Without NetBox: Postgres ping, body `{"status","version","instance_id"}` or 503 `{"postgres": ...}`. |
| GET | `/readyz` | none | Same handler as `/healthz`. |
| GET | `/metrics` | none | Prometheus exposition. |

## Client config

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/v1/client-config` | none | `{"oidc_issuer","oidc_client_id"}`: what `slagctl auth login` needs to sign in. Unauthenticated because the CLI reads it before it has a token; both values are public. |

## Machines

All paths are under `/api/v1`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/machines` | read | List machines. Filters: `?state=`, `?serial=` (exact NetBox serial), `?hostname=` (case-insensitive substring of the NetBox device name; needs NetBox, 503 without). Rows carry a transient `hostname` from NetBox. |
| GET | `/machines/:id` | read | One machine, with `hostname` filled from NetBox when available. |
| PATCH | `/machines/:id` | admin | Update slagworks-owned fields. Only `desired_os` is applied; a body containing `state` is rejected with 400. Identity (name, MACs, BMC IP, site) is edited in NetBox. |
| GET | `/machines/:id/power` | read | Read power state through a synchronous RPC to a connected site worker; no operation is created. Returns `{"machine_id","state","transport","hint"}`, `state` one of `on`/`off`/`unknown`. 15 s budget. 422 when no credential resolves, 503 when no worker for the site is connected, 502 when the read fails. |
| POST | `/machines/:id/power` | admin | Create a power operation. Body `{"action": "on"\|"off"\|"cycle", "boot_device": "pxe"\|"disk", "via": "bmc"\|"pdu"\|"amt"}`; `boot_device` and `via` are optional. `boot_device` is rejected with `via=pdu`, and `disk` is rejected over AMT. Not gated on machine state or lock. |
| GET | `/machines/:id/boot` | read | Read the BMC boot-device override over the same RPC path. Returns `{"machine_id","supported","device","persistent","valid","raw","transport","hint"}`. Only the SMCIPMITool controller implements boot reads, so `supported` is false for every other host (and always for AMT). |
| POST | `/machines/:id/boot` | admin | Create a `set_boot_device` operation. Body `{"device": "pxe"\|"disk"\|"default", "persistent": bool}`; `persistent` is invalid with `default`. BMC only: 422 for an AMT machine. The worker fails the operation if the host's controller cannot set a boot override (only SMCIPMITool can). |
| GET | `/machines/:id/preflight` | read | Run the deploy (default) or commission (`?op=commission`) preflight without creating an operation. Returns `{"machine","op_type","ok","gaps"}`; 200 whenever the check ran, 503 when it could not run. |
| GET | `/machines/:id/match` | read | Report the network and storage scripts the host matches, read from a freshly built snapshot of its owning site. Returns `{"machine","hostname","network_script":{"name"},"storage_script":{"name"}}`; an absent script field means no script of that kind matched. 503 when the match cannot be computed. |
| GET | `/machines/:id/netbox-diff` | read | Compare the slagworks machine row with its NetBox device. `:id` must be a UUID. Returns `{"machine_id","missing","message","matched","fields":[{"name","slagworks","netbox","comparable","match"}]}`; `missing` is `netbox` or `slagworks` when one side is absent. Needs NetBox. |
| GET | `/machines/:id/console-probe` | read | `?nonce=<hex>&lookback=<seconds>` (lookback default 300, max 3600): report whether the nonce appeared in the machine's console stream. Returns `{"observed": bool}`. Registered only with the console store. |
| POST | `/machines/:id/deploy` | admin | Create a deploy operation. Body (all optional) `{"destructive": bool, "wipe_all_disks": bool, "recovery": bool, "via": "bmc"\|"pdu"\|"amt"}`. `wipe_all_disks` requires `destructive`. 424 when no SSH keys are registered; 422 when no credential resolves or preflight finds gaps; 409 when the machine's state, lock or an active operation forbids it. Allowed from `commissioned`, `failed` and `running`. |
| POST | `/machines/:id/commission` | admin | Create a commission operation. Body `{"via": ...}`. Same credential and preflight checks as deploy. Allowed from `new`, `commissioned`, `failed` and `running`. |
| POST | `/machines/:id/bmc-reconcile` | admin | Create a `bmc_reconcile` operation: the owning site worker brings the BMC's IPMI user table into line over the LAN using the device's `bmc-creds-admin` entry (one `monitor` account at USER privilege, every other non-reserved account disabled), then sets NTP, UTC and the clock over Redfish. Body `{"rotate_monitor": bool}`. Without `rotate_monitor` the password stored in `pop/<pop>/bmc-creds-monitor` is re-asserted; a new one is generated only when none is stored or on rotate. 422 for an AMT machine, without NetBox, without an admin entry, when the pop's monitor secret is missing, or when the stored monitor entry cannot be re-asserted. Not gated on state or lock. |
| POST | `/machines/:id/lock` | admin | Set `locked=true`. Returns the machine. |
| POST | `/machines/:id/unlock` | admin | Set `locked=false`. |
| POST | `/machines/:id/activate` | admin | Set `pxe_eligible=true`. 409 when locked. |
| POST | `/machines/:id/deactivate` | admin | Set `pxe_eligible=false`. 409 when locked. |
| POST | `/machines/:id/abort` | admin | Fail the machine's active operations and return the machine. |

`via` selects the power transport. Omitted, it is `amt` when the device's
`bmc-creds-admin` entry has `kind: "amt"` and `bmc` otherwise. `pdu`
switches the device's PDU outlets and is the only option for a host with
no BMC.

## PDU outlets and device credentials

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/pdus/:ident/outlet/:n/power/resolve` | read | `?verb=cycle\|on\|off`. Resolve outlet `n` (1-based) of a PDU, trace its cable in NetBox, and classify the downstream device's feeds. Returns `{"pdu_name","pdu_mgmt_ip","pdu_serial","outlet","connected","downstream_device","downstream_serial","downstream_port","classification","warning","preflight_token"}`; `classification` is `unconnected`, `sole` or `redundant_unverified`. The token is issued only for a connected outlet, is valid for 5 minutes, and is bound to the operator, verb, PDU, outlet, downstream serial and classification. |
| POST | `/pdus/:ident/outlet/:n/power` | admin | Body `{"verb","preflight_token","allow_self"}`. Re-resolves the outlet and creates a power operation on the PDU's own machine row. 409 when the token is invalid, expired, for another verb or operator, or the topology changed; 422 for an unconnected outlet, for `off` on an outlet feeding a registered site worker, or for `cycle`/`on` of a worker's feed when no other eligible worker is connected (`allow_self` overrides this last case; it is rejected with `off`). |
| GET | `/devices/:ident/creds` | creds | Return the `pop/<pop>/bmc-creds-admin` entry for any NetBox device. `:ident` is a NetBox device id, name or serial, tried in that order. Returns `{"device","netbox_id","serial","pop","kind","mgmt_ip","username","password","extra_config"}`; `kind` is the entry's own value (`bmc`, `amt`, `pdu`, `serial`, `serial-server`). 409 when a serial matches several devices; 503 without NetBox or the secret store. |

Preflight tokens are signed with a key derived from
`SLAGWORKS_WEBHOOK_SECRET`, or a random per-process key when that is unset;
a restart then invalidates outstanding tokens.

## Operations

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/operations` | read | List operations; `?machine=<uuid>` scopes to one machine. Rows carry a transient `machine_name` (NetBox name, else serial). |
| GET | `/operations/:id` | read | One operation by UUID. |

Site workers claim operations over the WebSocket (`claim_op`), never over
REST. The claim uses the connection's authenticated worker identity, and a
worker can claim only operations on its own site's machines.

Every response that returns an operation (these two reads and the 201
from each op-creating POST) shows each credential in `params`
(`bmc_runtime.password`, `pdu_runtime.outlets[].password`,
`amt_runtime.password`, `monitor_runtime.password`,
`tailscale_runtime.auth_key`) as `"[redacted]"`; once the op is terminal
those keys are removed from the stored row altogether. Site workers get
the real values over the WebSocket op stream. See
`docs/dev/state-machine.md`, "Credentials in operations".

## Images

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/images` | read | List image rows. |
| POST | `/images` | admin | Create a row. Body `{"name","version","sha256","url","arch"}`; all but `arch` (default `amd64`) are required. 409 on conflict. Pushes a fresh snapshot to every site. |
| PUT | `/images/:name` | admin | Create or replace the row named in the path (the path name wins over the body). Used by `slagctl image follow-latest`. Pushes a fresh snapshot to every site. |
| DELETE | `/images/:name` | admin | Delete by name or UUID. 204. Does not push a snapshot. |

## Storage and network scripts

The two script families have identical routes.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/storage-scripts` | read | List scripts (head version). |
| POST | `/storage-scripts` | admin | Create. Body `{"name","content"}`; priority comes from the `slagworks:priority` header in `content`. 400 on a header parse error, 409 on conflict. |
| PUT | `/storage-scripts/:name` | admin | Add a new immutable version. Body `{"content"}`. Returns the script plus the new `version`. |
| DELETE | `/storage-scripts/:id` | admin | Delete by UUID. 204. |
| GET | `/storage-scripts/:name/history` | read | Every version, oldest first: `[{"version","priority","content","created_at","head"}]`. |
| GET | `/network-scripts` | read | As above, for network scripts. |
| POST | `/network-scripts` | admin | |
| PUT | `/network-scripts/:name` | admin | |
| DELETE | `/network-scripts/:id` | admin | |
| GET | `/network-scripts/:name/history` | read | |

Script mutations do not push snapshots; the change is picked up on the next
snapshot build for each site.

## SSH keys

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/ssh-keys` | read | List keys. |
| POST | `/ssh-keys` | admin | Add a key. Body `{"public_key","comment"}`; the key must parse as an OpenSSH authorized-keys line. |
| DELETE | `/ssh-keys/:id` | admin | Delete by UUID. 204. |

Deploys are refused while no SSH key is registered.

## Site configs

A site config scopes one site-worker identity to a NetBox site (and
optionally a location) and holds the DHCP pool settings. Fields are listed
in [`configuration.md`](../user/configuration.md#site-config-model).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/site-configs` | read | List. |
| GET | `/site-configs/:slug` | read | One by slug. |
| POST | `/site-configs` | admin | Create. Body is a site config; `netbox_site` defaults to `slug`. With NetBox configured: 412 when the site or location does not exist or no `slagworks-production` prefix is scoped to it, 409 when it overlaps an existing config. Then creates the pop's `bmc-creds-monitor` secret if absent (a failure is logged, not returned). |
| PATCH | `/site-configs/:slug` | admin | Update any of `netbox_site`, `netbox_location`, `ephemeral_pool_start`, `ephemeral_pool_end`, `promiscuous`, `relay_pools` (`relay_pools` replaces the whole list). Re-runs the create checks on the merged config. |
| DELETE | `/site-configs/:slug` | admin | Delete. 204, also when no such slug exists. |
| GET | `/site-configs/:slug/dhcp-leases` | read | Fan a `dhcp_leases` RPC to every connected worker for the slug. Returns `{"workers":[{"worker_id","fqdn","error","leases":[{"mac","ip","expires_at","kind","pool","machine_id","hostname"}]}]}`; a failed worker reports `error` without failing the request. 10 s budget. |
| GET | `/site-configs/:slug/dhcp-lease-history` | read | Query the Postgres lease ledger; works with every worker offline. Filters `?mac=`, `?ip=`; `?limit=` default 100, capped at 1000. Returns `{"entries":[...]}`, newest first. |

## Site workers

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/workers` | read | List registered workers: `id`, `site_slug`, `worker_id`, `fqdn`, `serial`, `first_seen_at`, `last_seen_at`, plus for connected workers `smcipmitool` (`installed`, `missing`, `missing-required`, `disabled`) and `supermicro_machines`. |
| POST | `/site-workers` | admin | Register a worker, or rotate its token. Body `{"site_slug","worker_id","fqdn"}`; `site_slug` and `worker_id` are required. Returns `{"id","site_slug","worker_id","fqdn","first_seen_at","token"}`; the plaintext token appears only in this response. |
| DELETE | `/site-workers/:ident` | admin | Delete a registration. `:ident` is the row id, `worker_id` or FQDN. 409 when ambiguous, when the worker is connected, or when it has claimed operations in flight. 204. |

## NetBox

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/admin/netbox/bootstrap` | admin | Idempotently create the custom fields, tag, device role, profiles, webhook and event rules slagworks needs in NetBox. The webhook targets `<SLAGWORKS_EXTERNAL_URL>/api/v1/hooks/netbox`. 412 unless `SLAGWORKS_EXTERNAL_URL` and `SLAGWORKS_WEBHOOK_SECRET` are set; 503 without NetBox. |
| POST | `/admin/netbox/migrate-chassis-serial` | admin | Reconcile the legacy `chassis_serial` custom field with NetBox's built-in `serial`. Body `{"dry_run": bool}`, default `true`; unknown keys are rejected. Returns `{"dry_run","examined","skipped","migrated","conflicts"}`. |
| POST | `/hooks/netbox` | hmac | NetBox webhook receiver. Acts on `dcim.device` (create, update, delete, plus the drift check on topology fields) and `dcim.site` (a slug rename of a managed site gets 409); other object types get 200 and are left to the 5-minute reconcile. 503 with a message naming `SLAGWORKS_WEBHOOK_SECRET` while that secret is unset, 403 on a bad signature, 400 on an unparseable body; most internal failures still return 200 because NetBox does not retry. |

---

## WebSocket endpoints

### `/api/v1/ws/site` (auth: `worker`)

The only channel between global and site workers. Every frame is an
envelope `{"type": "<string>", "request_id": "<id>", "data": <payload>}`.
Request/reply pairs share a `request_id`.

Connection setup:

1. The worker connects with `?token=`.
2. Its first message must be `hello`. Global rejects the connection when
   `hello.worker_id` differs from the token's worker, when `proto` is below
   the current op-stream protocol (`OpStreamProto`, 1), or, when NetBox is
   configured, when `serial` is empty or does not resolve to exactly one
   NetBox device inside the worker's site-config scope.
3. Global replies with `snapshot` (worker is behind) or `no_change`, then
   sends `op_resync`.

Worker to global:

| Type | Purpose |
|---|---|
| `hello` | `{"worker_id","fqdn","last_seq_num","version","proto","serial","smcipmitool"}`. |
| `heartbeat` | `{"worker_id"}`. Refreshes `site_workers.last_seen_at`. No reply. |
| `observation` | An outbox entry: `{"machine_id","op_id","event","timestamp","data"}`. Events: `commission_report`, `deploy_complete`, `bmc_reconcile_report`, `dhcp_lease`, `phone_home`, `inventory_import`. Acknowledged with `rpc_response`. |
| `op_update` | `{"op_id","status","phase","result"}`. Processed in order; no reply. |
| `log_batch` | `{"envelopes":[...]}`. Persisted to `log_envelopes` and re-emitted on global's stdout. |
| `claim_op` | `{"op_id"}`. Global replies with `claim_op_response`. |
| `cast_batch` | `{"frames":[{"machine_id","t_us","data"}]}`: console bytes for the console stream. |
| `cast_resume` | `{"machine_id"}`. Global replies with `rpc_response` carrying `{"cast_id","cast_started_at","width","height","prev_max_us"}`. |
| `power_status_response` | Reply to `power_status`: an `rpc_response`-shaped payload whose `data` is `{"state","transport","hint"}`. |
| `boot_status_response` | Reply to `boot_status`; `data` is `{"supported","device","persistent","valid","raw","transport","hint"}`. |
| `dhcp_leases_response` | Reply to `dhcp_leases`; `data` is `{"leases":[...]}`. |

Global to worker:

| Type | Purpose |
|---|---|
| `snapshot` | `{"snapshot","version","proto"}`: the full site snapshot plus global's build SHA. Sent on hello when the worker is behind and whenever the site's snapshot content changes. A worker with `SLAGWORKS_AUTO_UPGRADE=1` upgrades itself when `version` differs from its own. |
| `no_change` | `{"version","proto"}`. Hello reply when the worker's `last_seq_num` is current. |
| `op_resync` | `{"ops","machine_states"}`: every active operation for the site, sent after every hello reply. |
| `op_delta` | `{"operation","machine_state"}`: one operation after a mutation (create, claim, phase change). Every active operation for the site is also re-sent as an `op_delta` after each `snapshot` pushed on a content change; the worker keeps the higher `version`. |
| `op_removed` | `{"op_id","machine_id","version","machine_state"}`: an operation left the active set. |
| `power_status` | RPC request `{"machine_id","bmc_runtime","amt_runtime"}`. |
| `boot_status` | RPC request `{"machine_id","bmc_runtime"}`. |
| `dhcp_leases` | RPC request `{}`, sent to every connected worker for the site. |
| `rpc_response` | `{"data","error","terminal"}`. Reply to `observation` and `cast_resume`. `terminal: true` tells the worker to drop the entry rather than retry. |
| `claim_op_response` | `rpc_response`-shaped; `data` is `{"operation"}` when the worker won the claim or `{"already_claimed": true}` when it lost, including when the operation's machine is not in the worker's site. |
| `error` | `{"code","message","proto"}`, e.g. a rejected hello. |

`internal/ws/messages.go` also defines `op_dispatch` and `cast_resume_ok`,
which nothing sends.

### `/api/v1/logs/stream` (auth: `read`)

Structured-log tail used by `slagctl machine logs` and the `slagctl`
watch view. Registered only with the log store.

1. The client sends one `log_stream_subscribe` envelope with any of
   `trace_id`, `machine_id`, `machine_name`, `op_id`, `sources` (`agent`,
   `site`, `global`), `min_level`, `since` (RFC 3339) and `last_seen_id`.
   It must arrive within 5 seconds.
2. The server replies `subscribed`, sends matching rows from
   `log_envelopes` as `log_stream_envelope` (`{"id","envelope"}`), then
   polls for new rows every 250 ms, holding back the newest 500 ms.
3. A bad subscription or query failure sends one `error`
   (`{"code","message"}`) and closes the connection.

Clients resume after a disconnect with `last_seen_id`. The
`log_stream_meta` type is defined and rendered by `slagctl` but not sent by
the server.

### `/api/v1/console/stream` (auth: `read`)

Serial-console (SOL) tail used by `slagctl machine logs --sol` and the
`slagctl` watch view. Registered only with the console store. Not
enveloped: the server speaks asciicast v2.

Query parameters:

- `machine=<uuid>` (required). An id that names no machine gets a 404
  before the upgrade and creates nothing; for a known machine the cast
  row is created on first connect.
- `since_us=<int>`, `since=<duration>`, `tail=<duration>`: start point,
  first match wins in that order. With none of them, a non-follow request
  replays from the start and a follow request starts at the live edge.
- `follow=true|1`: keep streaming after the backlog.
- `snapshot=true|1`: replay the whole cast through a server-side terminal
  emulator and send one reconstructed screen, then follow from the live
  edge if `follow` is set. Cannot be combined with `since_us`, `since` or
  `tail`.

Frames: one header object, then `[t_seconds, "o", data]` lines with invalid
UTF-8 replaced. Errors after the upgrade arrive as a single
`{"code","message"}` frame. Parameter errors before the upgrade return an
HTTP 400.

`internal/ws/messages.go` also defines `subscribe`, `log_entries`,
`op_snapshot` and `op_status` for an operation-watch stream. No server
endpoint serves them.

---

## Commission outbox contract

The one multi-write sequence that has to survive crashes and partial
failures:

1. The agent POSTs its commission report to the site worker's
   `/api/v1/report`; the worker queues a `commission_report` observation in
   its SQLite outbox and answers 200.
2. The worker drains the outbox over the WebSocket as `observation`
   messages.
3. Global writes the inventory to NetBox (device fields, interfaces,
   modules, LLDP-derived cables), then commits the commission result to
   Postgres, then acknowledges with `rpc_response`.
4. The worker marks the entry acknowledged.

Any failure before the acknowledgement leaves the entry in the outbox and
the worker retries with backoff; the NetBox writes are idempotent, so a
crash between them and the acknowledgement is safe to replay. A
`terminal` error drops the entry. Consequence: commissioning blocks while
NetBox is unreachable. Deployed machines keep running from the last
snapshot.

---

## Not in the API

- **Machine create and delete.** Machines are created and removed by the
  NetBox mirror (webhook plus reconcile), never over REST.
- **Operation claim over REST.** Workers claim over `claim_op` on the
  WebSocket so the claim is bound to the connection's identity.
- **NetBox proxying.** Query NetBox directly.
- **Per-endpoint roles.** Every operator route uses the same group check;
  the only addition is the optional credential-group check on
  `GET /devices/:ident/creds` (see Auth).
