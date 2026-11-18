# Security policy

Slagworks holds BMC credentials, drives power and PXE boot, and writes disks.
A flaw in it can take over or destroy the machines it manages.

## Reporting a vulnerability

Email **security@bsky.app**. Do not open a public issue or pull request.

Include the affected component (`slagworksd global`, `slagworksd site`,
`slagctl`, `slagworks-agent`, the boot image), the version or commit, and
steps to reproduce. Bluesky's security team will acknowledge the report and
work with you on a fix and on disclosure timing.

## Scope

In scope: this repository's code and the packages and images built from it.

Out of scope: Bluesky's own deployment and infrastructure. Do not test
against systems you do not own.

## Supported versions

Slagworks is pre-1.0. Fixes land on `main` and ship in the next release;
older releases are not patched.
