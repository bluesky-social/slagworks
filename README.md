# slagworks

A bare-metal provisioning system, built and run by
[Bluesky](https://bsky.social) for its own data-center fleet. A single Go binary (`slagworksd`) runs as
either a central control plane or a per-site worker, handling PXE boot, OS
imaging, hardware commissioning, power management, and NetBox inventory sync.

```text
            Central                                Site
     ┌──────────────────────┐            ┌───────────────────────┐
     │  slagworksd global   │            │  slagworksd site      │
     │  ├── REST API        │◄───WS────► │  ├── DHCP/PXE/TFTP    │
     │  ├── NetBox mirror   │            │  ├── HTTP boot        │
     │  └── snapshot build  │            │  ├── Image server     │
     └───────────┬──────────┘            │  ├── SOL capture      │
                 │                       │  └── Power mgmt       │
     ┌───────────┴──────────┐            │      (IPMI/Redfish/   │
     │  Postgres            │            │       vendor CLI/PDU) │
     └──────────────────────┘            └───────────┬───────────┘
                                                     │ PXE boot
     ┌──────────────────────┐                 ┌──────┴──────┐
     │  NetBox              │                 │  Bare metal │
     │  (source of truth)   │                 │  machines   │
     └──────────────────────┘                 └─────────────┘
```

NetBox is the source of truth for physical inventory (devices, interfaces,
IPs, sites). Slagworks mirrors it, builds per-site snapshots, and pushes them
to site workers over a WebSocket. Site workers operate independently for all
critical-path operations and report observations back up. Operators drive
everything through `slagctl`.

## Status

Slagworks provisions Bluesky's production hardware, and it is pre-1.0: wire
formats, the database schema and the CLI change between releases without
compatibility shims. It assumes NetBox for inventory, an OIDC identity
provider for operators, Postgres for the control plane, and Ubuntu on the
machines it deploys. BMC and vendor support covers the hardware listed in
[tested hardware](docs/user/hardware/tested-hardware.md); other hardware needs a
storage script and possibly a power driver
([adding new hardware](docs/user/hardware/adding-new-hardware.md)).

## Components

| Binary | Description |
|--------|-------------|
| `slagworksd global` | Central API server (Postgres-backed), NetBox mirror, snapshot builder |
| `slagworksd site` | Per-site worker: DHCP, PXE, TFTP, HTTP boot, power, SOL capture, image serving |
| `slagctl` | Operator CLI (OIDC-authenticated) |
| `slagworks-agent` | Static binary downloaded by machines at boot; runs commission or deploy |

## Documentation

User docs (operating slagworks):

- [Getting started](docs/user/getting-started.md) — first commission and
  deploy, end to end
- [Installation](docs/user/installation.md) — binaries, packages,
  infrastructure requirements
- [Configuration](docs/user/configuration.md) — env vars, site-configs,
  secrets layout
- [Operations](docs/user/operations.md) — the operator verb set, logs, power
- [slagctl reference](docs/user/slagctl-reference.md) — generated from the
  command tree (`just gen-cli-docs`)
- [Boot process](docs/user/boot-process.md) — DHCP → iPXE → initrd → agent,
  BIOS vs UEFI
- [Images](docs/user/images.md) — image pipeline, pinning, rollout
- [Storage scripts](docs/user/storage-scripts.md) — hardware-aware disk
  layout
- [Adding a site worker](docs/user/adding-a-site-worker.md)
- [Adding new hardware](docs/user/hardware/adding-new-hardware.md) — plus
  [tested hardware](docs/user/hardware/tested-hardware.md) and vendor
  references: [BMC matrix](docs/user/hardware/bmc-vendor-matrix.md),
  [iPXE local-boot idioms](docs/user/hardware/ipxe-local-boot-idioms.md),
  [NC-SI/shared-LAN BMCs](docs/user/hardware/ncsi-shared-lan-bmc.md),
  [Dell R640/iDRAC](docs/user/hardware/dell-r640-idrac.md)
- [Troubleshooting](docs/user/troubleshooting.md)
- [Glossary](docs/user/glossary.md)

Developer docs (changing slagworks):

- [Architecture](docs/dev/architecture.md) — system design, package map,
  data flow
- [State machine](docs/dev/state-machine.md) — machine states, operation
  lifecycle, boot-action decision table
- [Database](docs/dev/database.md) — schema, migrations, store layer
- [Testing](docs/dev/testing.md) — unit/component/NetBox/QEMU tiers, CI
- [Build and release](docs/dev/build-and-release.md) — artifacts, packaging,
  CI checks, releases, installing slagctl
- [Extensibility](docs/dev/extensibility.md) — storage scripts, netscripts,
  BMC vendors, operation types
- [API](docs/dev/api.md) — HTTP/WS endpoint reference

## Development

Requires: Go (version per `go.mod`), [just](https://github.com/casey/just),
Docker (for dev NetBox and DB-backed tests).

```sh
just test                  # Unit + component tests
just lint                  # golangci-lint
just build                 # Compile check
just gen-cli-docs          # Regenerate docs/user/slagctl-reference.md
just gen-licenses          # Regenerate THIRD-PARTY-LICENSES.md after a dependency change
```

Full-stack QEMU integration tests run in CI on GitHub-hosted runners; see [docs/dev/testing.md](docs/dev/testing.md) for the tiers and how
to run one test ad hoc.

### Dev NetBox

A persistent local NetBox instance for development (separate from the test
harness):

```sh
just netbox-up             # Start (http://localhost:8000, admin/admin)
just netbox-down           # Stop (preserves data)
just netbox-reset          # Destroy + recreate
```

### Site scripts

Disk layouts and host network configuration are scripts uploaded to global
and matched per machine (`slagctl storage-script`, `slagctl network-script`).
`assets/storage/` holds layouts keyed on hardware model. `assets/network/`
holds the scripts for Bluesky's own sites: they encode one fabric and are
there as tested, worked examples to adapt, not as defaults.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems privately,
as [SECURITY.md](SECURITY.md) describes.

## License

Dual-licensed under [MIT](LICENSE-MIT.txt) and [Apache 2.0](LICENSE-APACHE.txt);
see [LICENSE.txt](LICENSE.txt). Third-party notices are in [NOTICE](NOTICE) and
[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md).
