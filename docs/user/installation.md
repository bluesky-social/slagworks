# Installation

Slagworks is a bare-metal provisioning system delivered as a small set of
Go binaries. This document describes what those binaries are, how they
are built, how they get onto hosts, and what surrounding infrastructure
each running mode needs. Configuration knobs (env vars and flags) are
covered separately in [`configuration.md`](configuration.md).

## Binaries

Three binaries, shipped in two Debian packages. All are built with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` (see `scripts/build-deb.sh`), so
they need no C library at runtime and cross-compile from any platform
with a Go toolchain.

- **`slagworksd`** — the daemon. One binary, two modes selected by
  subcommand: `slagworksd global` (central control plane) and
  `slagworksd site` (per-site worker). Source: `cmd/slagworksd/`.
- **`slagctl`** — the operator CLI. Talks to `slagworksd global` over
  HTTP(S) with an OIDC access token. Source: `cmd/slagctl/`.
- **`slagworks-agent`** — the agent that runs inside the boot image on a
  target host during commission and deploy. **Operators do not install it
  on targets.** The `slagworks-agent` package puts it on each site worker,
  the site worker serves it at `/files/bin/slagworks-agent`, and the
  initrd downloads and runs it. Source: `cmd/slagworks-agent/`.

Optional and not shipped: **`SMCIPMITool`**, Supermicro's BMC tool.
Install it yourself on site workers that manage Supermicro hosts; without
it those hosts use the generic IPMI/Redfish path. See
[Supermicro SMCIPMITool](hardware/supermicro-smcipmitool.md).

## Building from source

Building produces the two `.deb` packages. Requires a Go toolchain
(version in `go.mod`), [`nfpm`](https://nfpm.goreleaser.com/), and git
history with a reachable `v*` tag: the package version is
`X.Y.Z+git.N.g<sha>` from `git describe`, or `X.Y.Z` on a release-please
release commit, and the script fails when no tag is reachable.

```sh
scripts/build-deb.sh
```

The script compiles the three binaries with `-trimpath` and
`-ldflags="-X main.version=<git-sha> -X main.packageVersion=<deb-version>"`, runs `nfpm package` with the two
package definitions in the repo root, and then deletes the intermediate
binaries:

- `nfpm-agent.yaml` — `slagworks-agent`: the agent binary at
  `/usr/bin/slagworks-agent`.
- `nfpm.yaml` — `slagworks`: `slagworksd` and `slagctl`, the
  `slagworks-upgrade` helper and its sudoers entry, both systemd units, the
  two env example files under `/etc/slagworks/`, and the iPXE binaries
  (`snponly.efi`, `undionly.kpxe`) under `/srv/tftp`. It depends on
  `systemd-boot-efi` and on `slagworks-agent` at exactly the same version.

The version string embedded in `-ldflags` is the git SHA of the build
tree; `slagworksd version` and `slagctl version` print it. Site workers
compare that value against the global server's over the WebSocket
control channel, which drives the auto-upgrade path (see
"Site worker auto-upgrade" below). The package version is the deb
version the script builds (`X.Y.Z` on a release commit,
`X.Y.Z+git.N.g<sha>` otherwise); the global server sends it with its
SHA so an upgrading worker knows exactly which package to install. A
binary built any other way (`go build`, `just build`) has an empty
package version.

The repo's `Dockerfile` builds a container image for `slagworksd global`
from the two packages (listening on 8081, health-checked on `/readyz`).

For a compile check without packaging:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

## Installing on a host

Publish the two `.deb` files to an apt repository the hosts can reach and
install with `apt-get install slagworks`, which pulls in
`slagworks-agent`.

The package's post-install script (`deploy/postinstall.sh`) creates a
`slagworks` system user, creates `/var/lib/slagworks/images` and
`/var/lib/slagworks/data` owned by it, restricts `/etc/slagworks/*.env*`
to mode 0640 group `slagworks`, and links
`/usr/lib/systemd/boot/efi/systemd-bootx64.efi` into
`/var/lib/slagworks/images/`. It does **not** enable either unit, because
a host may run global, a site worker, or both. On an upgrade it restarts
whichever units were running. Enable the unit you need after writing its
env file:

```sh
install -m 0640 -g slagworks site.env /etc/slagworks/site.env
systemctl enable --now slagworksd-site
```

Two systemd units ship (`deploy/systemd/`):

- `slagworksd-global.service` — runs `/usr/bin/slagworksd global` as
  user `slagworks` with env from `/etc/slagworks/global.env`. Hardened
  (`ProtectSystem=strict`, `ProtectHome=yes`, `PrivateDevices=yes`,
  `PrivateTmp=yes`, `NoNewPrivileges=yes`). `Restart=on-failure`.
- `slagworksd-site.service` — runs `/usr/bin/slagworksd site` as
  **root** with env from `/etc/slagworks/site.env`. `Type=notify` (the
  worker signals readiness once its boot services are running, whether or
  not global is reachable) and `Restart=always`. It runs
  as root because it binds raw DHCP sockets and ports 67/69, reads the
  host's SMBIOS serial, and replaces its own package during auto-upgrade.

Both units load their env file with `EnvironmentFile=-...` (a missing file
does not fail the unit before the daemon can report it) and set
`RestartPreventExitStatus=78`, so a missing or invalid setting stops the
unit instead of restart-looping.

### Site worker auto-upgrade

With `SLAGWORKS_AUTO_UPGRADE=1` in `site.env`, a site worker whose build
SHA differs from global's runs
`sudo -n /usr/bin/slagworks-upgrade <sha> <package-version>`, both values
as global reported them, and retries every 2 minutes while the mismatch
lasts. The helper (`deploy/upgrade/slagworks-upgrade`, shipped in the
`slagworks` package):

1. validates both arguments;
2. exits early if `slagworksd version` already reports that SHA;
3. installs `slagworks` and `slagworks-agent` at exactly that package
   version with `apt-get install --allow-downgrades`, inside a transient
   `systemd-run` unit so the transaction survives the service restart the
   post-install script performs.

The daemon and the agent therefore move together, and rolling global back
rolls the workers back. For workers to follow it, global must run a
`scripts/build-deb.sh` build (the container image the `Dockerfile`
assembles from those packages is one), and the same package versions must
be in an apt repository the workers can reach. When global reports no
package version, the worker logs that it cannot auto-upgrade to an
unpackaged build and stays put. Without the variable the worker logs the
mismatch and stays on its version.

A worker learns global's build only once global has accepted its
connection. A worker that global refuses (an older build across a
release whose changelog marks a breaking change to the worker protocol
or its authentication, for example) never sees that build and cannot
auto-upgrade: install the matching packages on it by hand, as the
release's breaking-change note says. To find such workers, look in
global's log: it logs a `refusing site worker connection` warning with
the `remote_ip` of each worker it refuses before the WebSocket upgrade,
saying why and what to do. The worker's own `WebSocket disconnected,
reconnecting` line carries the reason global gave only when the worker
already runs a build that reads it.

## Infrastructure required per mode

### `slagworksd global`

The central control plane, one process per environment. Site workers and
operators reach it over HTTP(S); the default listen address is `:8081`
(`LISTEN_ADDR` / `--addr`). It needs:

- **Postgres** — the authoritative store. Migrations run at startup. Set
  `DATABASE_URL` (or `--dsn`); the default only suits local development.
  RDS with IAM authentication is supported (see
  [configuration](configuration.md#database)).
- **NetBox** — the inventory source of truth (version 4.6 by default; see
  `NETBOX_API_VERSION`). Slagworks mirrors devices, interfaces, IP
  prefixes and locations out of NetBox to build per-site snapshots, and
  writes commission results back. Configure `NETBOX_URL` and
  `NETBOX_TOKEN`. Without both, global serves its API but site workers
  cannot connect, so it is only useful for development.
  `slagctl netbox bootstrap` creates the custom fields, tag, role and
  webhook slagworks expects.
- **AWS Secrets Manager** in `us-east-2`, through the default AWS
  credential chain:
  - `pop/<pop>/bmc-creds-admin` (one per pop) holds the out-of-band
    credentials for every device in the pop (BMC, Intel AMT, PDU, console),
    keyed by device serial. Slagworks only reads it.
  - `pop/<pop>/bmc-creds-monitor` (one per pop) holds the per-host
    monitor-account passwords slagworks provisions on BMCs. Global reads
    and writes it, and creates it when a site config is created for the
    pop.

  Without working AWS access, operations that need a BMC credential are
  refused.
- **Image hosting** — boot image tarballs at immutable per-build URLs.
  The `Image` database row pins one URL and its SHA-256, and site workers
  download it with a plain HTTP GET, so the URL must be readable without
  credentials from every site worker (for example a public-read bucket).
  `slagctl image follow-latest` additionally reads a `latest-sha.txt`
  pointer from the same location.
- **An OIDC identity provider** — operator authentication. Global refuses
  to start without `--oidc-issuer` and `--oidc-audience`. Access tokens
  must be RS256-signed JWTs carrying the configured audience and a
  `groups` claim. Details in
  [`configuration.md`](configuration.md#operator-authentication).
- **Tailscale** (optional) — an OAuth client for deploy-time tailnet
  enrollment.
- **A proxy or load balancer** (optional) in front of global must pass
  WebSocket upgrades and the `Authorization` header through, with an idle
  timeout of at least 60 seconds. Global pings the operator streams
  (`slagctl machine logs -f`, `slagctl machine logs --sol -f` and the watch
  view) every 30 seconds so that a quiet stream stays open; a shorter idle
  timeout can still close one. 60 seconds is the default for an AWS ALB and
  for nginx's `proxy_read_timeout`.

### `slagworksd site`

A per-site process on a gateway host in the target site. It keeps a
WebSocket to global, receives the site snapshot, and drives the site's
hardware. It binds:

- **DHCP** (UDP 67) on `SLAGWORKS_IFACE`, using raw sockets.
- **TFTP** (UDP 69) on `SLAGWORKS_SERVER_IP`, serving iPXE binaries from
  `SLAGWORKS_TFTP_DIR` (default `/srv/tftp`, where the package installs
  `snponly.efi` and `undionly.kpxe`).
- **HTTP boot** on `SLAGWORKS_SERVER_IP`:`SLAGWORKS_HTTP_PORT` (default
  8080): iPXE scripts, image files from `SLAGWORKS_IMAGES_DIR`, the iPXE
  binaries, the `slagworks-agent` binary from `SLAGWORKS_AGENT_BIN`, and
  the agent report and log endpoints.
- **Health and metrics** on `SLAGWORKS_HEALTH_ADDR` (default `:9090`):
  `/healthz`, `/readyz`, `/metrics`.
- Optionally a second DHCP/TFTP/HTTP set on the out-of-band management
  segment when `SLAGWORKS_OOB_DHCP_ENABLED=true`, on whichever local
  interface has an address in the snapshot's OOB subnet.

Requirements on the host and its position:

- **Registered and modeled.** Register the worker with
  `slagctl worker register` and put the token and worker ID in
  `site.env`. When global has NetBox configured, the worker host itself
  must be a NetBox device, with its real SMBIOS serial, inside the site
  config's scope: global refuses the connection otherwise.
- **Reachability.** An outbound path to global (for the WebSocket) and to
  the image URLs. Layer-2 or DHCP-relay reach to the PXE NICs of the hosts
  it provisions, and IP reach to their BMCs, AMT endpoints and PDUs.
- **Tools on `PATH`.** `ipmitool` for BMC console (SOL) capture, the IPMI
  power path, `bmc-reconcile`, and the chassis-serial fallback; without it
  BMC console capture is disabled and those operations fail. `dmidecode`
  for the host's own serial.
- **Redundancy (optional).** Two workers can serve one site config,
  either active/standby with a VIP (`SLAGWORKS_STANDBY`, `SLAGWORKS_VIP`)
  or both active, with the OOB dynamic pool split into per-worker slices
  defined in NetBox. The fabric ephemeral range and relay pools are split
  into per-worker shares automatically, one per registered worker, so
  delete the registration of a worker you retire. Register and configure
  each worker separately.

State:

- **SQLite** at `SLAGWORKS_DATA_DIR/worker.db` (default
  `/var/lib/slagworks/worker.db`): the last snapshot, the worker
  identity, DHCP leases and the observation outbox. A worker that starts
  while global is unreachable, or refuses it, serves from the cached
  snapshot. It waits up to 5 seconds for global's first snapshot, starts
  its services, and then reports ready to systemd (`Type=notify`) whether
  or not it is connected, so systemd's start timeout never depends on
  global. A worker with no cached snapshot and no connection is therefore
  `active` while serving nothing; `/readyz` and the
  `slagworks_site_ws_connected` metric say whether it is connected.
- **Image cache** in `SLAGWORKS_IMAGES_DIR` (default `/srv/images`; set it
  to `/var/lib/slagworks/images` for package installs, see
  [configuration](configuration.md#slagworksd-site)). On every snapshot apply
  the worker compares each image's on-disk version link with the
  database row's SHA-256 and downloads on mismatch, in the background: the worker
  connects and reports ready without waiting for the download, and holds
  commission and deploy operations until it ends.

### `slagworks-agent`

Not installed by operators. The `slagworks-agent` package is a dependency
of `slagworks` on the **site worker**, not on targets; the site worker
serves the binary to the initrd. Rolling an agent change means upgrading
the packages on every site worker that serves the affected boots
(auto-upgrade does this when enabled).

## Verifying an install

- `slagworksd version` prints the embedded git SHA.
- `systemctl status slagworksd-global` / `slagworksd-site` should show
  the unit `active (running)`.
- On a global host: `curl -sf http://localhost:8081/healthz` (or the
  `--addr` you set). With NetBox configured it returns 503 until the
  first NetBox reconcile has finished.
- On a site worker: `curl -s http://localhost:9090/readyz`. It returns 200
  once the worker is connected to global, its snapshot lists at least one
  image, `systemd-bootx64.efi` is present in the images directory, the
  agent binary exists, no foreign OOB DHCP server has been detected, and
  the fabric DHCP leg is not running without its pool because the
  snapshot's DHCP config does not parse (see
  [configuration](configuration.md#site-config-model)).
  Otherwise it returns 503 with the failing condition in `reason`.
- On an operator machine: `slagctl auth login`, then `slagctl worker list`
  should list every registered site worker.
