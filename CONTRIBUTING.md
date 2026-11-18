# Contributing to slagworks

Slagworks is maintained by Bluesky and developed in this repository. Pull
requests and issues from outside are welcome.

## Before you start

- For anything larger than a bug fix, open an issue first and describe the
  change. Slagworks exists to provision Bluesky's fleet, so a feature that
  serves hardware or topologies Bluesky doesn't run needs a maintainer who
  agrees to carry it.
- Read `AGENTS.md`. It holds the engineering rules for people and coding
  agents alike: no compatibility shims, stated rationale for decisions, tests
  for every external touch point.
- Read `docs/dev/architecture.md` and `docs/dev/state-machine.md` before
  changing the control plane or the operation lifecycle.

## Building and testing

You need Go (the version in `go.mod`), [just](https://github.com/casey/just)
and Docker.

```sh
just build    # compile check
just test     # unit and component tests; database-backed tests start Postgres in Docker
just lint     # golangci-lint
```

`docs/dev/testing.md` describes the test tiers. The full-stack QEMU tests need
a Linux host with `/dev/kvm`; CI runs them on GitHub-hosted runners, once a
maintainer approves the run for a pull request from a fork.

Never pipe `go test` output through a filter. Save it to a file and read the
file: filters hide panics and the context around a failure.

## Pull requests

- Give the title a `type:` prefix such as `feat:`, `fix(dhcp):` or `docs:`.
  Pull requests are squash-merged, the title becomes the commit, and the
  release notes are built from titles, so write the title as the changelog
  line you want. A breaking change takes `feat!:` or a `BREAKING CHANGE:`
  footer that says what an operator must do before upgrading.
- Keep one change per pull request, with its tests.
- Keep the tree free of organization-specific values: no internal hostnames,
  account IDs, bucket names, ticket IDs, or defaults that name a deployment.
  `scripts/ci/public-clean.sh` checks this.
- Workflows on pull requests from forks wait for a maintainer to approve
  them. Every check runs on a GitHub-hosted runner, and none gets a
  secret on a fork pull request; the jobs that publish a release or hand a
  commit to the maintainers' deployment run only on `main`
  (`docs/dev/build-and-release.md`).
- Dependencies are updated by Renovate. Don't bump them in an unrelated pull
  request. A change that adds or removes a vendored module also needs
  `just gen-licenses`; a test fails until `THIRD-PARTY-LICENSES.md` matches
  `vendor/`, and the generator refuses a license outside its allowed list.

## Hardware support

A new chassis usually needs a storage script and sometimes a power or BMC
driver; `docs/user/hardware/adding-new-hardware.md` walks through it. Say in
the pull request what hardware you tested on and how, since maintainers
usually cannot reproduce it.

## Security

Do not open a public issue for a vulnerability. See `SECURITY.md`.

## License

Contributions are accepted under the repository's dual MIT and Apache 2.0
license (`LICENSE.txt`).
