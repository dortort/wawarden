# Changelog

Each release of WaWarden and what it introduced. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[the versioning rules in `RELEASING.md`](RELEASING.md#versioning).

`v0.1.0` to `v0.3.0` were development releases: each replaced the one before
it and promised no compatibility. The compatibility promise starts at
`v1.0.0`. Only the latest minor release line is supported (see
[`SECURITY.md`](SECURITY.md)).

Every release's notes on the
[Releases](https://github.com/dortort/wawarden/releases) page give its commit,
its image digest and the commands to verify and reproduce it.
`wawarden version` prints a binary's version.

## [1.0.0] - 2026-10-12

Built from commit `5021d13` with go1.27.2.

### Added

- Sending text over REST (`POST /v1/chats/{ref}/messages`) and the MCP tool
  `send_message`, optionally as a reply in the same chat. Only a client with
  write chats sees `send_message`.
- Idempotency keys for sends, kept for 24 hours per client.
- The pacer, per-client and global send limits, and the first-contact rule,
  with `--write` and `--allow-first-contact` on `admin clients create`.
- `admin backfill`, which asks the owner's phone for a chat's older history,
  with a quota.
- Nightly encrypted backups at `WAWARDEN_BACKUP_TIME`, with catch-up, retries
  and retention (`WAWARDEN_BACKUP_KEEP`), and `backup verify`.
- Audit retention (`WAWARDEN_AUDIT_RETENTION_DAYS`) behind a signed audit
  anchor.
- Master key rotation with `keys rotate`, and retired keys in `keys/retired/`.
- The events `backup_pruned`, `backup_prune_failed`, `audit_key_changed`,
  `send_outcome_unknown` and `send_unrecorded`.
- The [compatibility promise](RELEASING.md#versioning), tested against
  `internal/archtest/testdata/v1-surface.txt`.
- The [hardening guide](docs/hardening.md), the
  [ban-risk notes](docs/ban-risk.md) and the [runbook](docs/runbook.md).

### Security

- Client and admin tokens are redacted from logs, events and command errors.

## [0.3.0] - 2026-10-11

Built from commit `d2d2534` with go1.27.2.

### Added

- Clients with per-chat read scopes and expiring tokens:
  `admin clients create`, `list`, `show` and `revoke`, and `admin chats list`.
- The read API over REST: chats, messages, search and a change feed, in pages
  with sealed cursors and message references.
- The same reads as MCP tools at `POST /mcp`.
- Per-client read and search budgets.
- Owner-saved contact names, shown only to a client that may read the
  contact's direct chat.
- The hash-chained audit chain, its chain heads on standard output, and
  `audit verify`.
- The scoped store, its single `IN` builder, uniform resolution and bounded
  reads, with property tests.
- Golden files for the MCP `tools/list` answers.
- The [guide for agents](docs/agents.md).

## [0.2.0] - 2026-10-06

Built from commit `da456bf` with go1.27.1.

### Added

- The WhatsApp engine, through the protocol library `go.mau.fi/whatsmeow`.
- Pairing by code, guarded by an account check against
  `WAWARDEN_OWNER_PHONE`.
- History sync, under a size cap (`WAWARDEN_HISTORY_MAX_BYTES`).
- The device store and the message archive in SQLite, with verified settings
  and removal of revoked, edited and expired text from the disk.
- `admin status`, `admin pair` and `admin reconnect`.
- Notification events on standard output and a signed webhook
  (`WAWARDEN_NOTIFY_URL`).
- Metrics on standard output in embedded metric format (EMF)
  (`WAWARDEN_METRICS_EMF`).
- One encrypted backup per paired device, to an age recipient
  (`WAWARDEN_BACKUP_AGE_RECIPIENT`).
- The log writer's pseudonyms for WhatsApp identifiers, keyed by the master
  key.
- The strict chat identifier normaliser and the seal package for grants and
  chats.
- The fake engine for development builds (`WAWARDEN_DEV_FAKE_ENGINE`).

### Changed

- The licence changed from MIT to `GPL-3.0-or-later`, because the protocol
  library depends on a component licensed under GPL-3.0. `v0.1.0` stays under
  MIT (see [Licence](README.md#licence)).

## [0.1.0] - 2026-10-04

Built from commit `d0d5514` with go1.27.1. Released under the MIT licence.

### Added

- Configuration checks and startup refusals, including the refusal of any
  unknown `WAWARDEN_` variable.
- The client, admin and health listeners, on loopback by default.
- The admin token, configured by its SHA-256 only, and `admin init`.
- Prometheus metrics on the admin listener.
- `healthcheck` and `version`.
- The policy core: read, write and admin grants from decision functions, and
  route registration that requires a policy class.
- The container image and verifiable, reproducible releases.

[1.0.0]: https://github.com/dortort/wawarden/releases/tag/v1.0.0
[0.3.0]: https://github.com/dortort/wawarden/releases/tag/v0.3.0
[0.2.0]: https://github.com/dortort/wawarden/releases/tag/v0.2.0
[0.1.0]: https://github.com/dortort/wawarden/releases/tag/v0.1.0
