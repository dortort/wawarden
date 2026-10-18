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

- [Unreleased](#unreleased)
- [1.3.0 - 2026-10-18](#130---2026-10-18)
- [1.2.0 - 2026-10-18](#120---2026-10-18)
- [1.1.0 - 2026-10-15](#110---2026-10-15)
- [1.0.0 - 2026-10-12](#100---2026-10-12)
- [0.3.0 - 2026-10-11](#030---2026-10-11)
- [0.2.0 - 2026-10-06](#020---2026-10-06)
- [0.1.0 - 2026-10-04](#010---2026-10-04)

## [Unreleased]

### Fixed

- The 1.3.0 documentation said that a newest-anchored backfill gives a media
  reference to any re-delivered attachment the archive holds without one.
  Against a live account, in group chats, only the attachments of messages
  from before the device was linked gained one; those of messages the device
  had received itself, while downloads were off, gained none. The README, the
  runbook, the CLI, configuration, API and internals references and the
  threat model now say so (see
  [Backfill requests](docs/api.md#backfill-requests)).

## [1.3.0] - 2026-10-18

Built from commit `26eb050` with go1.27.2.

### Added

- A backfill can be anchored at a chat's newest archived message.
  `POST /admin/v1/backfill` takes an optional `anchor`, `"oldest"` (the
  default, as before) or `"newest"`, and `wawarden admin backfill` takes
  `--anchor oldest|newest`. With `newest` the owner's phone is asked for up to
  50 messages before the chat's newest message, and a re-delivered attachment
  that the archive holds without a media reference gains one while downloads
  are on, under the same size and view-once rules as at receipt, if it is
  neither revoked nor expired, was never marked gone after a `403`, `404` or
  `410` from its media host, and is younger than
  `WAWARDEN_MEDIA_REF_RETENTION_DAYS` (see
  [Backfill requests](docs/api.md#backfill-requests)). Both anchors share the
  one quota of a request every 5 minutes and 20 a UTC day.

### Changed

- The archive is at schema version 6. A fetch that the media host answers
  with `403`, `404` or `410` now also marks the message, and no later copy of
  it, a backfill's answer included, gains a media reference again. Before,
  the mark went with the reference when the minute sweep removed it.
- Dev builds: the fake engine answers a backfill with the messages it
  delivered before the anchor in that chat, when there are any.

### Fixed

- The documentation did not say that a media reference is kept only for an
  attachment received while downloads are on. The runbook, the configuration
  reference and the API reference now say that one received before downloads
  were on, or while they were off, answers `media_unavailable` until a
  newest-anchored backfill re-delivers its message (see
  [the runbook](docs/runbook.md#other-incidents)).

### Upgrade notes

- The first start migrates the archive to schema version 6. The change is
  forward-only: an earlier release refuses the upgraded archive. Take a
  backup before upgrading; going back needs that backup restored into the
  earlier release (see [the runbook](docs/runbook.md#upgrading)).
- An attachment whose media host answered `403`, `404` or `410` under an
  earlier release keeps no mark once the sweep removed its reference: a
  newest-anchored backfill can give it a reference again, and its next fetch
  marks it.
- Run `wawarden admin backfill --anchor newest` against a v1.3.0 server. A
  v1.2.0 server refuses the `anchor` key with `invalid_body`; without
  `--anchor`, or with `--anchor oldest`, the command sends no `anchor` key and
  works against both.

## [1.2.0] - 2026-10-18

Built from commit `a18eb3a` with go1.27.2.

### Added

- On-demand attachment downloads, off by default. With
  `WAWARDEN_MEDIA_MAX_BYTES` above `0`, a client created with `allow_media`
  (`wawarden admin clients create --allow-media`) downloads the attachment of
  a photo, video, voice note or other audio, document or sticker, fetched from
  WhatsApp only when it asks (see [Attachments](docs/api.md#attachments)):
  - over REST, `POST /v1/messages/{mref}/media` starts a fetch job, and `GET`
    or `HEAD` on the same path reports on it and then delivers the bytes, with
    single-range support;
  - over MCP, the tool `get_attachment` returns the attachment's status and,
    when the file is small enough and held in memory, the file inline: a
    re-encoded image up to 3.5 MiB as an image block, audio up to 8 MiB as an
    audio block, and video, PDF or another document up to 8 MiB as an
    embedded blob. Its optional `part` argument, from 0 to 255, returns any
    ready attachment in 8 MiB parts to be joined in order; a part's result
    gives the number of parts and the joined length, which can be shorter
    than the declared size.

  Both transports share one job per client and message, the budgets and the
  per-job delivery caps.
- The settings, with their defaults: `WAWARDEN_MEDIA_MAX_BYTES` (`0`, off),
  the reference retention `WAWARDEN_MEDIA_REF_RETENTION_DAYS` (30),
  `WAWARDEN_MEDIA_PER_CLIENT_PER_MINUTE` (6, with a burst of 2, or 1 when the
  limit is 1), `WAWARDEN_MEDIA_GLOBAL_PER_HOUR` (60) and
  `WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR` (4 GiB), each with its own refusal
  reason. Neither unsafe caps switch lifts them.
- `spool/` in the data directory, where an attachment over 16 MiB is staged,
  with the refusals `spool_dir_unusable`, `spool_dir_not_directory`,
  `spool_dir_foreign_owner` and `spool_dir_permissions`, and the
  `spool_sweep_failed` log event.
- `POST /admin/v1/media` with `{"paused":true}` or `{"paused":false}`. It
  pauses attachment downloads at once, without a restart, and erases every
  attachment job, or it resumes them. While downloads are paused, a fetch
  answers `busy` with a 60-second retry hint. The switch lives in memory, so a
  restart resumes downloads, and `admin_mutation` reports it with the action
  `media_pause` (see [Pausing downloads](docs/api.md#pausing-downloads)).
- The admin status field `downloads` (`off`, `on` or `paused`), the counts
  `media_refs_live`, `media_refs_expired` and `revoke_tombstones`, and the
  warning `media_write_client` while an active client with `allow_media` has a
  write chat. `wawarden admin status` prints them, and `wawarden backup
  verify` prints the archive's live and expired media references and its
  revoke tombstones.
- The codes `media_unavailable`, `media_not_requested`, `media_expired`,
  `media_too_large`, `media_unsupported`, `media_failed` and
  `range_not_satisfiable`.
- The audit actions `rest.media.fetch`, `rest.media`, `rest.media.sent` and
  `mcp.get_attachment`. An audit row can name a message by a keyed
  `message_hmac` and carry a byte count, and the `audit_failed` log event and
  `wawarden_audit_failures_total` report a row that could not be written.
- The metrics `wawarden_media_fetches_total`,
  `wawarden_media_fetched_bytes_total`, `wawarden_media_deliveries_total`,
  `wawarden_media_delivered_bytes_total` and `wawarden_media_jobs`, present
  while downloads are on, and the panic names `api.media.job`,
  `api.media.sweep`, `api.mcp.orphan`, `engine.media`, `wa.media.watchdog` and
  `stage.sweep`.
- A soft memory limit at start: with `GOMEMLIMIT` unset or empty, WaWarden
  sets the Go runtime's soft limit to 80% of the container's cgroup memory
  limit, and the start event `memory_limit` gives the value and its source.
- A revocation of a message not stored yet leaves a 7-day tombstone when it
  comes from the message's sender or a recorded group admin, so a later copy
  of that message is stored already revoked.
- Agent guidance on attachments as third-party content, on parts and on what
  an MCP host keeps of a result, in [agents](docs/agents.md).
- Dev builds: the fake engine serves synthetic attachments of every kind,
  among them two that come in parts, one that the normaliser refuses and one
  that answers expired.

### Security

- Nothing is fetched when a message arrives, and view-once media is never
  fetched. Architecture rules confine the protocol client's downloads to two
  typed seams, one for history and one for media, and the media seam's source
  can be built only from a sealed reference that `FetchMedia` opened; only the
  engine's `FetchMedia` and the media service reach it.
- A media key reaches the engine only inside a media reference sealed under
  the new `media-ref` key, which opens only for its own message. `FetchMedia`
  also refuses a reference whose kind, declared length, chat or sender differ
  from the requested message's.
- Every attachment request and `get_attachment` call re-opens the reference
  and re-checks the client's grant and scope, and every denial gets the same
  `404` as an unknown route, whether downloads are on or off. Jobs are never
  shared between clients.
- A fetch moves at most the declared length plus 32 bytes per response and
  twice that over its at most 6 requests, and stops after a complete body. A
  429 or 5xx ends it and holds every new fetch for WhatsApp's `Retry-After`,
  kept between 30 seconds and 10 minutes. Fetch slots, staged objects,
  per-client jobs, the three budgets and the per-job delivery caps bound what
  clients can start or pull.
- The content policy runs once per job before any byte is served, the same for
  both transports: the served type comes from the bytes, never the sender;
  images, stickers and image documents are re-encoded without their metadata
  by decoders confined to `internal/attach`, within 50 megapixels and a
  128 MiB budget; a video's metadata boxes are blanked; and a document the
  normaliser refuses is served only as `application/octet-stream`.
- Attachment bytes go out only with a fixed header set: an allow-listed type
  per kind, `Content-Disposition: attachment`, `nosniff`, a sandboxing content
  security policy and `Cross-Origin-Resource-Policy: same-origin`, after a
  committed audit row and under a write deadline. An MCP image block is built
  only from re-encoded bytes, and every `get_attachment` result marks the file
  as untrusted.
- An attachment over 16 MiB is staged in `spool/` only as AES-GCM ciphertext
  under a key held in memory, in a private file erased when its job ends.
  `spool/` must be private to the service's user, every start removes every
  regular file in it, and no backup includes it. At or below 16 MiB no
  attachment byte touches the disk.
- The deletion guarantee covers the media type and media reference of revoked
  and expired messages, including messages revoked before the upgrade. A
  revocation of a message not stored yet is refused for a target its sender
  could not revoke.
- The protocol library's log lines no longer carry any `http`, `https`, `ws`
  or `wss` URL, media download path or encrypted-file hash, and audit lines
  carry a keyed HMAC of a message, never its id.
- The threat model lists the new residuals: a delivery or a `get_attachment`
  result already under way when its client is revoked; copies an MCP host or
  an agent saves; two clients holding every download slot; prompt injection
  through pixels, speech and documents; and metadata in attachments served as
  fetched.

### Changed

- The archive is at schema version 5 and new inbox rows are payload
  version 2. A revocation removes the whole message, its media type and media
  reference included, whether downloads are on or off; a revoked message, and
  an expired one once the expiry pass reaches it, answers `media_type` `null`.
- The minute expiry sweep also purges media references and revoke
  tombstones, and `expiry_sweep_failed` covers them.
- `audit` log lines always include `message_hmac` and `bytes`, `null` when the
  row names no message.
- A `POST` to `/v1/messages/{mref}/media` is audited as `rest.media.fetch`,
  and a `GET` or `HEAD` as `rest.media`, instead of `rest.unrouted`, whether
  downloads are on or off.
- `wawarden_ingest_dropped_total` counts a refused revocation of a message not
  stored yet under its refusal reason as well as `target_unknown`.
- The MCP tool list depends on the media grant: a client with `allow_media`
  sees `get_attachment` while downloads are on, and every other client sees the
  same list as before.
- The refusal table in the [configuration](docs/configuration.md) is
  renumbered; the reason strings are unchanged.
- The [container hardening](docs/hardening.md#container-hardening) guide sizes
  `--memory` by a stated rule: `512m` holds with downloads off, and with them
  on at the defaults the container needs `--memory=768m`.
- The runbook and the [ban-risk notes](docs/ban-risk.md) say to pause
  downloads when a ban is suspected, when fetches keep being throttled and
  before pairing again.
- Dev builds: the fake engine's script ingests more messages, synthetic
  attachments among them.

### Fixed

- `wawarden_paired` and `wawarden_connected` could stay stale when the connect
  step's update and a `Connected` event's update overlapped.

### Upgrade notes

- The first start migrates the archive to schema version 5 and writes new
  inbox rows as payload version 2. Both changes are forward-only: an earlier
  release refuses the upgraded archive. Take a backup before upgrading; going
  back needs that backup restored into the earlier release (see
  [the runbook](docs/runbook.md#upgrading)).
- Remove every `WAWARDEN_MEDIA_*` variable before going back to an earlier
  release, which refuses to start with `unknown_variable`.
- With downloads off, a revocation now removes a message's media type, and a
  revocation that arrives before its message leaves a tombstone when it comes
  from the original sender or a recorded group admin.
- Before turning downloads on, give the container `--memory=768m`, and behind
  a reverse proxy configure `GET /v1/messages/*/media` (response buffering
  off, a whole response of at least 30 minutes, no response-size limit below
  `WAWARDEN_MEDIA_MAX_BYTES`, `Range` and `If-Range` passed through, no caching
  or compression) and allow `POST /mcp` responses of 16 MiB lasting 55 seconds
  (see [Reverse proxy with TLS](docs/hardening.md#reverse-proxy-with-tls)).
- Run `wawarden admin status` and `wawarden admin clients` from the same
  release as the server. `admin status` refuses a status answer without
  `downloads` or the three new counts, and `admin clients` refuses a client
  answer without the `allow_media` field, so against an earlier server
  `admin clients revoke` revokes the client but reports a failure.

## [1.1.0] - 2026-10-15

Built from commit `b4d9062` with go1.27.2.

### Added

- Binaries for macOS 13 Ventura or later: `wawarden_v1.1.0_darwin_arm64` for
  Apple silicon and `wawarden_v1.1.0_darwin_amd64` for Intel, with their SPDX
  SBOMs. `SHA256SUMS` and the provenance bundle cover them, and they are not
  notarized by Apple. On macOS, WaWarden is supported for the commands run
  away from the service, such as `backup verify` and `audit verify`, and for
  trying it; `serve` needs `WAWARDEN_DATA_DIR` there (see
  [macOS](docs/getting-started.md#macos)).
- The licence archive and the image's `/licenses` also hold the licence of
  `github.com/ncruces/go-strftime`, which only the macOS binaries link.
- `hack/repro-build.sh` builds the darwin binaries and checks them like the
  Linux ones. On a Mac, `binaries` mode runs the darwin binary for its
  architecture.
- Install steps in the README and in
  [Install](docs/getting-started.md#install): the release binary, checked
  against `SHA256SUMS` and the release's provenance bundle without a GitHub
  account, `go install`, the container image and a build from source. The
  release notes give the binary's steps and say that no `latest` image tag is
  published.
- The [documentation site](https://wawarden.dortort.dev/), built from
  `docs/`, with the [getting-started guide](docs/getting-started.md), the
  [glossary](docs/glossary.md), [`CONTRIBUTING.md`](CONTRIBUTING.md) and this
  changelog.
- Runbook procedures for rotating the backup recipient and the webhook secret,
  upgrading a container or a binary under systemd, moving to a new host, and
  the pairing, admin authentication, re-key and notification incidents.

### Security

- The release workflow rebuilds on an Apple silicon and an Intel macOS runner,
  runs the darwin binary for each, and publishes nothing unless their
  `SHA256SUMS` equals the build job's. The rebuild job compares the darwin
  SBOMs with the build job's, and the publishing job attests them to each
  binary.
- CI builds and runs the race tests on Apple silicon and Intel macOS, and
  reproduces the darwin binaries on both. `govulncheck` scans the darwin build
  as well as the Linux one, in CI and in the daily scan.

### Changed

- The linked device registers as `WaWarden (<OS>)`, such as
  `WaWarden (Linux)`, so Linked devices on the phone lists it under that name
  instead of `whatsmeow`. The pairing notification still names
  `Chrome (Linux)`.
- The configuration reference is split into
  [configuration](docs/configuration.md), [CLI](docs/cli.md),
  [API](docs/api.md), [observability](docs/observability.md) and
  [internals](docs/internals.md).
- The threat model gives each secure-by-construction row its own section,
  with its mechanism, what it protects, what enforces it, its limits and the
  release that introduced it.
- [Verifying a release](RELEASING.md#verifying-a-release) checks the binaries
  without a GitHub account and the image without a registry login, and names
  the cosign versions that verify the image signature.

### Fixed

- A `go install` of a release, or a `go build` of a clean release tag,
  reports that release instead of `dev` wherever WaWarden names its version,
  such as `wawarden version`, the `wawarden_build_info` metric and the MCP
  servers' `serverInfo`. A version set at link time still takes precedence,
  and other builds still report `dev`.
- The binary checks in the release notes download the release's files before
  checking them, and check the binaries against the release's provenance
  bundle, so they need no GitHub account. The image's provenance check still
  needs `gh auth login`.
- The systemd unit in the [hardening guide](docs/hardening.md) sets
  `WAWARDEN_BACKUP_AGE_RECIPIENT`, without which the service takes no backups.

### Upgrade notes

- A device paired by an earlier release keeps the name `whatsmeow` under
  Linked devices until it pairs again.

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

[Unreleased]: https://github.com/dortort/wawarden/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/dortort/wawarden/releases/tag/v1.3.0
[1.2.0]: https://github.com/dortort/wawarden/releases/tag/v1.2.0
[1.1.0]: https://github.com/dortort/wawarden/releases/tag/v1.1.0
[1.0.0]: https://github.com/dortort/wawarden/releases/tag/v1.0.0
[0.3.0]: https://github.com/dortort/wawarden/releases/tag/v0.3.0
[0.2.0]: https://github.com/dortort/wawarden/releases/tag/v0.2.0
[0.1.0]: https://github.com/dortort/wawarden/releases/tag/v0.1.0
