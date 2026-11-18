# Slagworks: project instructions

Slagworks is a bare-metal provisioning system. Go. Architecture:
`docs/dev/architecture.md`; state machine: `docs/dev/state-machine.md`; full
doc index in `README.md`. Read the architecture and state-machine docs before
making architectural decisions.

## Compatibility

Slagworks is pre-1.0 and makes no backward-compatibility promises between
releases. Do not build compatibility layers. When a function signature
changes, change all call sites; when an interface replaces a concrete type,
require the interface everywhere; when a wire format evolves, evolve every
producer and consumer in the same change. If you find yourself adding a
fallback, an `if legacy { ... }` branch, or a comment that says "for backward
compat", stop, delete the old thing, and move every caller to the new one in
the same commit.

The one narrow exception is an in-flight rollout within a single change: for
example, shipping a new field with `omitempty` so a worker reading a snapshot
persisted by an older binary doesn't crash mid-deploy.

Do not invent forward-compatibility scaffolding either: no feature flags to
decouple a producer from a consumer that has not shipped, and no fields left
optional on the receiving side until a second rollout. Prefer the simpler
design even when it needs a destructive schema change. Call out a breaking
change in the commit message (`feat!:` or a `BREAKING CHANGE:` footer) so
other contributors' rebases and operators' upgrades fail loudly.

## Tasks

| Task | What it does |
|------|-------------|
| `just test` | Unit and component tests (database-backed tests need Docker) |
| `just lint` | golangci-lint |
| `just build` | Compile check |
| `just gen-cli-docs` | Regenerate `docs/user/slagctl-reference.md` |

Run `just --list` for the full set.

## Working rules

- Question the task before starting it. If the decomposition is wrong, say so.
- Make it work, make it right, stop. No capabilities the task didn't ask for.
- No placeholders or TODOs in shipped code. Either do it now or file an issue.
- Decisions need stated rationale: "I chose X over Y because Z, accepting
  tradeoff W." If it's not written down, the decision hasn't been made.
- Patterns copied from other codebases need evaluation; "that's how another
  project does it" is context, not justification.
- Every external-system touch point needs a test or a documented reason why
  one isn't feasible.
- Every state transition needs: trigger, preconditions, failure handling,
  rollback path.
- Error messages say what happened and what to do.
- Test-first is the default.
- No emojis in source code, logs, or commit messages.
- Nothing organization-specific in the tree: no internal hostnames, account
  IDs, bucket names, ticket IDs or defaults that name a deployment.
  `scripts/ci/public-clean.sh` enforces this.

Standards can be deviated from, but priced. State the standard, why it can't
be met, the risk accepted, and the mitigation and revisit conditions, and get
agreement before proceeding.

## Commits and pull requests

Pull requests are squash-merged and the title becomes the commit, so the
title needs a `type:` prefix (`feat:`, `fix(api):`, `docs:`); see
`CONTRIBUTING.md`. Shared surfaces (database migrations, WebSocket message
types, API routes) attract merge conflicts: check for a competing migration
number before authoring one.

## Tests

Unit and component tests run locally with `just test`.

Full-stack QEMU tests (`internal/qemutest/...`, build tag `integration`) need
a Linux host with `/dev/kvm` and a boot image; CI runs them in shards on
GitHub-hosted runners (`.github/workflows/test-qemu.yaml`). The
other integration tests fan out through the `discover` and `run-test` matrix
in `.github/workflows/test-integration.yaml`; the qemutest skip is hard-coded
in `scripts/ci/list-integration-tests.sh`. See `docs/dev/testing.md`.

Integration tests boot the same images the image build produces. Initrds
already carry the slagworks boot hook, so test setup performs no initrd
repack.

### Subnet isolation

Tests use dynamically allocated subnets from a per-invocation pool (`10.{random}.{seq}.0/24`). Do NOT hardcode subnet addresses in test files — use `harness.NewNetwork(t)` which allocates from the pool. Bridge and netns names include a per-invocation run ID to prevent collisions between concurrent test suite runs.

Never pipe test output through grep or any filter: filters swallow errors,
panics, and useful context. Save full output to a file first, then filter the
file.

## Two log feeds

Structured envelopes (`slagctl machine logs <machine>`) carry the agent's
`slog` output plus site-worker and global events. The serial console
(`slagctl machine logs --sol <machine>`) carries raw bytes off the BMC:
firmware POST, bootloader, kernel, and the agent's stdout and stderr. A
`fmt.Print*` in the agent reaches only the serial console, so use `log.*` for
anything worth seeing remotely.
