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

## Contents

- [1.0.0 - 2026-10-12](#100---2026-10-12)
- [0.3.0 - 2026-10-11](#030---2026-10-11)
- [0.2.0 - 2026-10-06](#020---2026-10-06)
- [0.1.0 - 2026-10-04](#010---2026-10-04)

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
- The send path is confined: a send decides its write grant first, only the
  sender writes `sent_by`, and `internal/api` cannot import
  `internal/sender`. Sent rows carry the origin `gateway:<client id>`.
- `reply_to` is sealed like a message reference, and each send has a
  fingerprint.
- Outbound text is checked before it is sent, and a send to a direct chat
  that never wrote to the account is refused.
- Retired keys are used by `audit verify` and to open cursors, and a key
  change reports `audit_key_changed`.
- The audit retention verifies the rows it removes before it signs the
  anchor, and reports `audit_prune_refused` when they do not verify.
- An audit row is written even when the client left before its answer.
- Startup refusals for `WAWARDEN_BACKUP_TIME`, `WAWARDEN_BACKUP_KEEP`,
  `keys/retired/`, `WAWARDEN_AUDIT_RETENTION_DAYS`, the send limits and
  `WAWARDEN_UNSAFE_SEND_CAPS`.
- Fuzzing of idempotency keys, outbound text and the send route.

### Upgrade notes

- Releases before `v1.0.0` recorded a backup only as `backup_taken_at`.
  After an upgrade, that value counts as the newest successful backup for
  the nightly schedule.
- Audit lines written before `v1.0.0` carry no `key_id`. While the audit
  anchor is under a retired key, the head of a removed row on such a line
  counts as missing in `audit verify --log` (exit `6`). The next retention
  run makes the anchor again under the current key.

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

### Security

- Write chats are a subset of a client's read chats, and a client that reads
  every chat cannot write.
- Clients are kept in memory, and a revocation takes effect on the client's
  next request.
- Every read goes through the scoped store's grant-first methods, and read
  answers are built from dedicated response types only.
- `golang.org/x/text` is confined to one package; `pgregory.net/rapid` is a
  test-only module; the cursor and rate-limit packages use the standard
  library only.
- The MCP library is given no logger, and `serve` refuses `MCPGODEBUG` and
  `JSONSCHEMAGODEBUG`.
- The cursor and audit keys are derived from the master key, and audit lines
  carry chat HMACs instead of chat identifiers.
- The client create body is capped at 64 KiB, and the MCP endpoint checks a
  body before the MCP library reads it.
- Fuzzing of the remaining targets, among them the client token parser, the
  full-text query builder, the `IN` builder, cursor and message-reference
  opening, and the read API's query parsing.

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

### Security

- A neutral engine core: only the protocol adapter imports the protocol
  library. A guard keeps every test off the network.
- Two reviewed module lists, standard-library-only packages, store and engine
  import fences, and `os/exec` confined to the token command.
- One database handle; database files are created with mode `0600`.
- The master key in `keys/`, the keys directory rule, and a log pseudonym key.
- One log writer that drops lines carrying XML, the protocol-library log
  adapter, and `WAWARDEN_UNSAFE_DEBUG`.
- One notifier; the webhook checks its destination and signs every delivery
  with a secret read from a file.
- The events `admin_auth_failure` and `admin_mutation`.
- Pairing refusals, a strict JSON decoder for request bodies and a terminal
  sanitiser.
- Backups are encrypted to the age recipient only: the service holds nothing
  that decrypts one.
- `safego` recovers a panic in the adapter's event handler.
- Fuzzing of `Normalize`, also against the protocol library's parser, the
  sanitisers, JSON decoding and crafted protocol messages.

### Changed

- The licence changed from MIT to `GPL-3.0-or-later`, because the protocol
  library depends on a component licensed under GPL-3.0. `v0.1.0` stays under
  MIT (see [Licence](README.md#licence)).
- Release images follow the
  [container contract](docs/configuration.md#container-image) from this
  release on: the distroless `nonroot` base, user `65532`, `/data`, the
  exec-form health check, `/licenses` and the image labels.

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

### Security

- Requests from browsers (`Origin`, `Sec-Fetch-Site`) and `OPTIONS` requests
  are refused. Authentication runs before any body is read.
- Fixed error bodies, HTTP timeouts, a 16 KiB header limit, and
  `Cache-Control: no-store` and `nosniff` on every answer.
- Failed-authentication counters and a budget of 30 failures, refilled at one
  per second, on the client and admin listeners.
- `umask 077` and `GOTRACEBACK=single` are the first statements of `main`.
- Startup refusals for a plaintext admin token, the admin hash's form and
  sources, `WAWARDEN_DEV_*` in release builds, `GOTRACEBACK`, the listen
  addresses, running as root, and the data directory's owner and mode.
- Import fences, response types with a forbidden-key test, `safego.Go`, the
  listener inventory and constant-time comparison.
- Fuzzing of the admin token check.
- Release builds prove the `dev` build tag absent. The container image runs
  as a non-root user on a distroless base with a read-only root filesystem.

[1.0.0]: https://github.com/dortort/wawarden/releases/tag/v1.0.0
[0.3.0]: https://github.com/dortort/wawarden/releases/tag/v0.3.0
[0.2.0]: https://github.com/dortort/wawarden/releases/tag/v0.2.0
[0.1.0]: https://github.com/dortort/wawarden/releases/tag/v0.1.0
