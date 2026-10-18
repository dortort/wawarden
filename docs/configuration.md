# Configuration reference

This reference covers how to configure and deploy WaWarden. It lists the
environment variables, the checks `serve` makes before it opens a socket, the
data directory and what it holds, backups, and the container image. It
describes the code at the commit or tag you are reading.
[`wawarden version`](cli.md#version) prints a binary's version, and the
[changelog](../CHANGELOG.md) names the release that introduced each part.

The rest of the reference is split by topic:

- [cli.md](cli.md): subcommands, flags, exit codes and shutdown.
- [api.md](api.md): the listeners, the client API, MCP, the admin routes and
  the shared responses.
- [observability.md](observability.md): logs, notification events, the
  webhook, metrics and the audit chain.
- [internals.md](internals.md): the WhatsApp engine, ingest, history sync, the
  archive's schema, byte layouts, memory bounds and design rationale.

These five files describe every feature of this build; the service has no
other. A service without a paired device makes no connection to WhatsApp until
`wawarden admin pair` requests [pairing](internals.md#pairing). Terms such as LID, re-key
and key id are explained in the [glossary](glossary.md).

## Contents

- [Environment variables](#environment-variables)
  - [Rules for all variables](#rules-for-all-variables)
  - [Go runtime and library variables](#go-runtime-and-library-variables)
  - [Unknown variables stop the service](#unknown-variables-stop-the-service)
  - [Reserved names](#reserved-names)
  - [Listen addresses](#listen-addresses)
  - [Example environment file](#example-environment-file)
- [Startup refusals](#startup-refusals)
- [Data directory](#data-directory)
  - [Master key](#master-key)
  - [Message archive](#message-archive)
  - [Device store](#device-store)
  - [Storage profiles](#storage-profiles)
  - [Free space](#free-space)
- [Backups](#backups)
  - [Backup schedule](#backup-schedule)
  - [How a backup is written](#how-a-backup-is-written)
  - [Backup files](#backup-files)
  - [Backup retention](#backup-retention)
  - [Backup failure reasons](#backup-failure-reasons)
  - [Checking a backup offline](#checking-a-backup-offline)
  - [Restores](#restores)
- [Container image](#container-image)
  - [Data volume](#data-volume)
  - [Health check form](#health-check-form)
  - [Reaching the listeners](#reaching-the-listeners)
  - [Running as root](#running-as-root)
  - [Docker Compose example](#docker-compose-example)

## Environment variables

These are all the variables this build reads.

| Variable | Default | Accepted values | Reason codes |
|---|---|---|---|
| `WAWARDEN_DATA_DIR` | `/data` | A path to a directory; see [Data directory](#data-directory). | `data_dir_unusable`, `data_dir_not_directory`, `data_dir_foreign_owner`, `data_dir_permissions` |
| `WAWARDEN_LISTEN` | `127.0.0.1:8080` | The client listener's address; see [Listen addresses](#listen-addresses). | `listen_address_invalid`, `listen_address_shared` |
| `WAWARDEN_ADMIN_LISTEN` | `127.0.0.1:8082` | The admin listener's address. It is validated even while the admin listener is disabled. | `listen_address_invalid`, `listen_address_shared` |
| `WAWARDEN_HEALTH_LISTEN` | `127.0.0.1:8081` | The health listener's address; it must be a loopback address. | `listen_address_invalid`, `health_address_not_loopback`, `listen_address_shared` |
| `WAWARDEN_ADMIN_TOKEN_SHA256` | unset | The admin token's SHA-256: exactly 64 hexadecimal characters, either case, nothing else (no surrounding whitespace). | `admin_hash_invalid`, `admin_hash_sources_conflict` |
| `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` | unset | A path to a regular file of at most 4096 bytes that holds the hash. Whitespace around the hash is removed. Symbolic links are followed. | `admin_hash_file_unreadable`, `admin_hash_invalid`, `admin_hash_sources_conflict` |
| `WAWARDEN_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`, in lower case. | `log_level_invalid` |
| `WAWARDEN_STORAGE_PROFILE` | `local` | `local` or `nfs`, in lower case; see [Storage profiles](#storage-profiles). | `storage_profile_invalid` |
| `WAWARDEN_MIN_FREE_BYTES` | `268435456` (256 MiB) | A number of bytes in decimal digits, from `0` to `18446744073709551615`: no sign, unit, separator or white space. `0` turns the floor off. See [Free space](#free-space). | `min_free_bytes_invalid` |
| `WAWARDEN_OWNER_PHONE` | unset | The phone number of the WhatsApp account that WaWarden may link to, in E.164 form: `+`, then 7 to 15 digits, the first not `0`, and nothing else (no spaces or separators), for example `+15550100001`. Unset, the service starts but refuses to pair; see [Pairing](internals.md#pairing). | `owner_phone_invalid` |
| `WAWARDEN_HISTORY_MAX_BYTES` | `33554432` (32 MiB) | The largest history-sync blob, in bytes, before and after decompression: a number in decimal digits from `1` to `268435456` (256 MiB), with no sign, unit, separator or white space. The memory that history sync needs grows with it; see [History sync](internals.md#history-sync). | `history_max_bytes_invalid` |
| `WAWARDEN_UNSAFE_DEBUG` | unset | A window, in minutes, during which the protocol library's debug output is logged: a number in decimal digits from `1` to `60`. Unset, that output is discarded at every log level. See [Protocol library logs](observability.md#protocol-library-logs) before setting it. | `unsafe_debug_invalid` |
| `WAWARDEN_METRICS_EMF` | unset | `1` writes metrics in [embedded metric format (EMF)](observability.md#embedded-metric-format) on standard output; `0` or unset writes none. | `metrics_emf_invalid` |
| `WAWARDEN_NOTIFY_URL` | unset | The [webhook](observability.md#webhook) that receives every notification event: an `https` URL with a host, optionally a port, a path and a query, without credentials, a fragment or an IPv6 zone. Treat it as a secret when its path or query holds one; the service never logs it. Unset, events go to standard output only. | `notify_url_invalid` |
| `WAWARDEN_NOTIFY_SECRET_FILE` | unset | A path to the webhook's signing secret, required with `WAWARDEN_NOTIFY_URL` and refused without it: a regular file (symbolic links are followed) with no write permission for its group and no permission at all for others (`0400`, `0440`, `0600` and `0640` pass; `0644` and `0444` do not), of at most 4096 bytes, holding at least 32 bytes apart from surrounding white space. Generate one with `openssl rand -hex 32`. | `notify_url_missing`, `notify_secret_missing`, `notify_secret_unreadable`, `notify_secret_permissions`, `notify_secret_invalid` |
| `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | unset | `1` lets the webhook reach loopback, private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`) and shared (`100.64.0.0/10`, which mesh VPNs use) addresses; `0` or unset refuses them. Only with `WAWARDEN_NOTIFY_URL`. | `notify_allow_private_invalid`, `notify_url_missing` |
| `WAWARDEN_BACKUP_AGE_RECIPIENT` | unset | The one [age](https://age-encryption.org) recipient that [backups](#backups) are encrypted to: an X25519 recipient (`age1` followed by 58 characters, as `age-keygen` prints it) or a post-quantum hybrid one (`age1pq1...`), with no white space, comment or second recipient. An SSH key, a plugin recipient or an age secret key is refused, and the refusal never repeats the value. Unset, no backup is ever taken and `backup_disabled` is logged at start. | `backup_recipient_invalid` |
| `WAWARDEN_BACKUP_TIME` | `03:00` | The time of day, in UTC and written `HH:MM`, of the nightly [backup](#backups) slot. Only with `WAWARDEN_BACKUP_AGE_RECIPIENT`. | `backup_time_invalid`, `backup_option_without_recipient` |
| `WAWARDEN_BACKUP_KEEP` | `14` | How many [backups](#backups) `backups/` keeps: after each successful backup, the oldest beyond this count, by the time in their names, are deleted. A whole number from 1 to 365. Only with `WAWARDEN_BACKUP_AGE_RECIPIENT`. | `backup_keep_invalid`, `backup_option_without_recipient` |
| `WAWARDEN_AUDIT_RETENTION_DAYS` | `90` | How many days the [audit chain](observability.md#audit-chain) keeps its rows: each night, after the backup, rows written more than this many days before are removed behind a signed anchor. A whole number from 7 to 3650 in decimal digits. Unset means 90; no setting keeps rows forever. | `audit_retention_invalid` |
| `WAWARDEN_READ_PER_CLIENT_PER_MINUTE` | `600` | Each client's [read budget](api.md#read-rate-limits): requests per minute, from 1 to 600, refilled evenly with a burst of at most 60. Every authenticated client request, whatever its route or answer, costs one. | `read_per_minute_invalid` |
| `WAWARDEN_SEARCH_PER_CLIENT_PER_MINUTE` | `60` | Each client's [search budget](api.md#read-rate-limits): searches per minute, from 1 to 60, refilled evenly with a burst of at most 6. A search costs one from both budgets. | `search_per_minute_invalid` |
| `WAWARDEN_UNSAFE_RATE_CAPS` | unset | `1` lifts the upper caps of the two read budgets to 1000000 per minute and logs `unsafe_rate_caps` at start; `0` or unset keeps them. Zero or a negative budget stays refused. It never lifts the send limits or the attachment limits. | `unsafe_rate_caps_invalid` |
| `WAWARDEN_UNSAFE_SEND_CAPS` | unset | `1` lifts the upper caps of the two [send limits](api.md#send-limits) to 600 an hour and 60 a minute and logs `unsafe_send_caps` at every start; `0` or unset keeps them. Zero or a negative limit stays refused. It never lifts the attachment limits. | `unsafe_send_caps_invalid` |
| `WAWARDEN_SEND_GLOBAL_PER_HOUR` | `30` | The [global send limit](api.md#send-limits): sends a sliding hour, all clients together, from 1 to 60, or to 600 with `WAWARDEN_UNSAFE_SEND_CAPS=1`. Each client's hourly share is this divided by the number of clients that may write, at least 1. | `send_global_per_hour_invalid` |
| `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` | `10` | Each client's [send limit](api.md#send-limits) per sliding minute, from 1 to 10, or to 60 with `WAWARDEN_UNSAFE_SEND_CAPS=1`. | `send_per_client_per_minute_invalid` |
| `WAWARDEN_MEDIA_MAX_BYTES` | `0` (off) | The largest attachment, in bytes, whose reference the service keeps so that it can be downloaded later: `0`, or a number in decimal digits from `1` to `2147483648` (2 GiB), with no sign, unit, separator or white space. `0` turns attachment downloads off: no reference is captured, and the expiry sweep, which runs every minute, removes every stored one. Above `0`, a reference is kept only for an attachment whose declared size is at most this cap, and at most 16 MiB for an image or 1 MiB for a sticker, and never for a view-once message. References are kept only for attachments received while this is above `0`, or re-delivered by a [backfill](api.md#backfill-requests) while it is: an attachment received before downloads were on, or while they were off, has none until a newest-anchored backfill (`admin backfill --anchor newest`) re-delivers its message, up to 50 messages a request and only within `WAWARDEN_MEDIA_REF_RETENTION_DAYS`. At or below `16777216` (16 MiB), a fetched attachment is held in memory only and never written to disk; above it, one over 16 MiB is staged as ciphertext in [`spool/`](#data-directory). `104857600` (100 MiB) is the suggested value when you turn downloads on; then give the container `--memory=768m` instead of `512m` (see [Container hardening](hardening.md#container-hardening)). Neither unsafe caps switch changes it. A client created with `allow_media` downloads attachments through the [attachment routes](api.md#attachments) or the MCP tool [`get_attachment`](api.md#get_attachment), which is listed only while this is above `0`; nothing is fetched until a client asks. | `media_max_bytes_invalid` |
| `WAWARDEN_MEDIA_REF_RETENTION_DAYS` | `30` | How many days an attachment reference is kept: a whole number from 1 to 365 written in decimal digits. A reference expires at its message's time plus this many days, or at the message's own expiry when that comes first, and none is kept for a message already past that point. The expiry sweep removes a reference once it expires or once it has been stored this many days. | `media_ref_retention_days_invalid` |
| `WAWARDEN_MEDIA_PER_CLIENT_PER_MINUTE` | `6` | The [attachment fetches](api.md#attachments) each client may start per minute, from 1 to 30, refilled evenly with a burst of 2, or 1 when the limit is 1. Neither unsafe caps switch lifts this cap. | `media_per_client_per_minute_invalid` |
| `WAWARDEN_MEDIA_GLOBAL_PER_HOUR` | `60` | The [attachment fetches](api.md#attachments) over a sliding hour, all clients together, from 1 to 600. Neither unsafe caps switch lifts this cap. | `media_global_per_hour_invalid` |
| `WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR` | `4294967296` (4 GiB) | The attachment bytes over a sliding hour, all clients together: a number in decimal digits from `1` to `68719476736` (64 GiB), and not below `WAWARDEN_MEDIA_MAX_BYTES`. A fetch is charged its declared size when it starts, and any bytes fetched beyond that size when it ends (see [Attachments](api.md#attachments)). Neither unsafe caps switch lifts this cap. | `media_global_bytes_per_hour_invalid` |
| `WAWARDEN_DEV_FAKE_ENGINE` | unset | Development builds only; a release build refuses it with `dev_variable_in_release`. `1` runs the [fake engine](internals.md#fake-engine) in place of WhatsApp, `wrong_account` runs it pairing a number that is not the owner's, and `0` or unset runs the WhatsApp engine. | `dev_fake_engine_invalid` |

### Rules for all variables

- A variable set to the empty string is set, and is validated like any other
  value: `WAWARDEN_LISTEN=` is refused, it does not mean the default.
- When the environment holds a name more than once, the first occurrence is used.
- Set at most one of `WAWARDEN_ADMIN_TOKEN_SHA256` and
  `WAWARDEN_ADMIN_TOKEN_SHA256_FILE`. With neither, the admin listener is not
  opened at all. The hash and the file are read once, at start; to rotate the
  admin token, replace the hash and restart.
- `WAWARDEN_ADMIN_TOKEN` is never accepted, with any value: the service takes the
  admin credential only as a hash (`plaintext_admin_token`).

### Go runtime and library variables

`serve` also checks three variables outside the `WAWARDEN_` namespace:

| Variable | Accepted values | Reason code |
|---|---|---|
| `GOTRACEBACK` | unset, empty, `none` or `single` | `traceback_level_unsafe` |
| `MCPGODEBUG` | unset | `library_debug_set` |
| `JSONSCHEMAGODEBUG` | unset | `library_debug_set` |

Every other value is refused, numeric levels included, `0` and `1` as well. The
named levels `all`, `system`, `crash` and `wer` make a crash print the stack of
every goroutine. `serve` sets the level to `single` itself before doing anything
else, but the Go runtime does not let a program lower the level that this
variable sets, and combined with `single`, every other refused value (`0`, any
other number, or a name the runtime does not know) prints every goroutine too.

`MCPGODEBUG` and `JSONSCHEMAGODEBUG` switch compatibility behaviour of the MCP
library and of its JSON Schema library, such as how tool schemas and results are
encoded, so a set value, even an empty one, is refused. The MCP library reads
`MCPGODEBUG` when the program starts, before `serve` checks anything: a value
that is not a comma-separated list of `key=value` pairs makes the binary panic
in every subcommand, with exit status 2, no `startup_refused` event and a
message that repeats the malformed part, so leave the variable unset.

`GOMEMLIMIT` is the Go runtime's own soft memory limit, and `serve` does not
check it. When it is set to any value, `off` included, WaWarden changes
nothing. When it is unset or empty, WaWarden sets the soft limit at start to
80% of the container's cgroup memory limit, and sets none when the container
has no limit. At the `info` log level, every start logs `memory_limit` with
the limit in effect and its source. The Go runtime parses the variable before
WaWarden runs and aborts on a malformed value.
[Container hardening](hardening.md#container-hardening) sizes `--memory` from
the soft limit.

### Unknown variables stop the service

> **`wawarden serve` refuses to start when the environment contains any variable
> whose name begins with `WAWARDEN_` and that this build does not implement.** A
> typo, a variable meant for a later release, or a leftover from another
> deployment stops the service with the reason code `unknown_variable` instead of
> being ignored. An empty value counts as set:
> `WAWARDEN_SEND_RATE=` is refused as well.

Only the names in [Environment variables](#environment-variables) are accepted.
Names are matched exactly, so a lower-case `wawarden_listen` is not recognised as a
WaWarden variable and has no effect.

`WAWARDEN_TOKEN` belongs on an agent's host, never in the service's
environment; see [Registering with Claude Code](api.md#registering-with-claude-code).

### Reserved names

`WAWARDEN_DEV_*` names are reserved for development builds; a release build
refuses every one of them with `dev_variable_in_release`. A development build
implements `WAWARDEN_DEV_FAKE_ENGINE` (see [Fake engine](internals.md#fake-engine)) and
refuses any other `WAWARDEN_DEV_*` name with `unknown_variable`.

### Listen addresses

A listen address is an IP literal and a port from 1 to 65535: `127.0.0.1:8080`,
`[::1]:8080`, `0.0.0.0:8080`, `[::]:8080`.

- Host names such as `localhost`, an empty host (`:8080`), port `0`, URLs and IPv6
  zones (`[fe80::1%eth0]:8080`) are refused with `listen_address_invalid`.
- An IPv4-mapped IPv6 address (`[::ffff:127.0.0.1]:8080`) is treated as the IPv4
  address.
- An IPv6 listener accepts IPv6 connections only; `[::]` does not also accept
  IPv4.
- The client and admin listeners may use an unspecified address (`0.0.0.0` or
  `[::]`). Every start then logs a `listener_not_loopback` warning for that
  listener, as for any other non-loopback address, whatever
  `WAWARDEN_LOG_LEVEL` is set to.
- Two enabled listeners may not overlap: same port and same address family, with
  equal addresses or one of them unspecified (`listen_address_shared`). The
  refusal names the later of the two in the order client, health, admin. The admin
  address is compared only when the admin listener is enabled.

### Example environment file

A complete environment file for a host install, such as the
`EnvironmentFile=` of a [systemd unit](hardening.md#systemd). Replace each
value in angle brackets: a placeholder left in place is refused at start.
systemd and Compose's `env_file` both accept whole-line comments and blank
lines. Variables left out keep their defaults; here the read and send limits
do.

```sh
# Data directory and the WhatsApp account WaWarden may link to
WAWARDEN_DATA_DIR=/var/lib/wawarden
WAWARDEN_OWNER_PHONE=+15550100001

# Listeners: the defaults, written out
WAWARDEN_LISTEN=127.0.0.1:8080
WAWARDEN_ADMIN_LISTEN=127.0.0.1:8082
WAWARDEN_HEALTH_LISTEN=127.0.0.1:8081

# The admin token's SHA-256 only, as printed by `wawarden admin init`
WAWARDEN_ADMIN_TOKEN_SHA256_FILE=/etc/wawarden/admin.sha256

WAWARDEN_LOG_LEVEL=info
WAWARDEN_STORAGE_PROFILE=local

# Nightly backups, encrypted to an age public key
WAWARDEN_BACKUP_AGE_RECIPIENT=<age1... public key printed by age-keygen>
WAWARDEN_BACKUP_TIME=03:00
WAWARDEN_BACKUP_KEEP=14
WAWARDEN_AUDIT_RETENTION_DAYS=90

# Notification events go to standard output, and also to this webhook
WAWARDEN_NOTIFY_URL=https://alerts.example.com/wawarden
WAWARDEN_NOTIFY_SECRET_FILE=/etc/wawarden/notify.secret

# Metrics in embedded metric format on standard output
WAWARDEN_METRICS_EMF=1
```

## Startup refusals

`serve` validates its whole configuration, the [master key](#master-key), the
[archive](#message-archive) and the [device store](#device-store) before opening any socket. The first failed check stops it: it writes one log line with
`"event":"startup_refused"`, the reason code in `reason` and a fixed explanation in
`error`, and exits `2`. A refusal names the variable or the file in the data
directory involved, never a configured value or the data directory's path. For
example:

```json
{"time":"2026-10-04T08:09:24.446116295Z","level":"ERROR","msg":"startup refused","event":"startup_refused","reason":"data_dir_foreign_owner","error":"config: WAWARDEN_DATA_DIR: is not owned by the current user"}
{"time":"2026-10-04T08:11:02.518840121Z","level":"ERROR","msg":"startup refused","event":"startup_refused","reason":"master_key_permissions","error":"keys: keys/master must grant no access to group or others and have no setuid, setgid or sticky bit"}
{"time":"2026-10-04T08:12:40.002931604Z","level":"ERROR","msg":"startup refused","event":"startup_refused","reason":"archive_db_permissions","error":"db: archive.db must grant no access to group or others and have no setuid, setgid or sticky bit"}
```

The checks run in this order. What to do about each refusal is in the
[runbook](runbook.md#startup-refusals).

| # | Reason code | Variable named | Refused when |
|---|---|---|---|
| 1 | `plaintext_admin_token` | `WAWARDEN_ADMIN_TOKEN` | `WAWARDEN_ADMIN_TOKEN` is set, to any value. |
| 2 | `dev_variable_in_release` | the first such name, in sorted order | A release build sees any `WAWARDEN_DEV_*` variable. |
| 3 | `unknown_variable` | the first such name, in sorted order | Any other `WAWARDEN_` name is not one of the [environment variables](#environment-variables). |
| 4 | `traceback_level_unsafe` | `GOTRACEBACK` | `GOTRACEBACK` is set to anything other than empty, `none` or `single`, a numeric level included. |
| 5 | `library_debug_set` | the variable | `MCPGODEBUG` or `JSONSCHEMAGODEBUG` is set, to any value, an empty one included. A value of `MCPGODEBUG` that is not a comma-separated list of `key=value` pairs never reaches this check: the MCP library panics while the program initialises, in every subcommand, and the process exits 2 without a `startup_refused` event (see [Go runtime and library variables](#go-runtime-and-library-variables)). |
| 6 | `listen_address_invalid` | the listen variable | `WAWARDEN_LISTEN`, `WAWARDEN_ADMIN_LISTEN` or `WAWARDEN_HEALTH_LISTEN` (checked in that order) is not a valid [listen address](#listen-addresses). |
| 7 | `health_address_not_loopback` | `WAWARDEN_HEALTH_LISTEN` | The health address is not a loopback address. |
| 8 | `admin_hash_sources_conflict` | `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` | Both admin hash variables are set. |
| 9 | `admin_hash_file_unreadable` | `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` | The file cannot be opened or read, or is not a regular file (a directory, a device or a named pipe, for example). |
| 10 | `admin_hash_invalid` | the hash variable | The hash is not exactly 64 hexadecimal characters, or the file holds more than 4096 bytes. |
| 11 | `listen_address_shared` | the later listener | Two enabled listeners overlap. |
| 12 | `log_level_invalid` | `WAWARDEN_LOG_LEVEL` | The level is not `debug`, `info`, `warn` or `error`. |
| 13 | `storage_profile_invalid` | `WAWARDEN_STORAGE_PROFILE` | The profile is not `local` or `nfs`. |
| 14 | `min_free_bytes_invalid` | `WAWARDEN_MIN_FREE_BYTES` | The value is not a number of bytes in decimal digits that fits in 64 bits. |
| 15 | `owner_phone_invalid` | `WAWARDEN_OWNER_PHONE` | The value is set but is not an E.164 number: `+` and 7 to 15 digits, the first not `0`. |
| 16 | `history_max_bytes_invalid` | `WAWARDEN_HISTORY_MAX_BYTES` | The value is not a number of bytes in decimal digits from 1 to 268435456. |
| 17 | `unsafe_debug_invalid` | `WAWARDEN_UNSAFE_DEBUG` | The value is not a number of minutes in decimal digits from 1 to 60. |
| 18 | `metrics_emf_invalid` | `WAWARDEN_METRICS_EMF` | The value is not `0` or `1`. |
| 19 | `notify_url_invalid` | `WAWARDEN_NOTIFY_URL` | The value is not an `https` URL with a host, or it holds credentials, a fragment or an IPv6 zone. |
| 20 | `notify_allow_private_invalid` | `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | The value is not `0` or `1`. |
| 21 | `notify_url_missing` | `WAWARDEN_NOTIFY_SECRET_FILE`, else `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | That variable is set without `WAWARDEN_NOTIFY_URL`. |
| 22 | `notify_secret_missing` | `WAWARDEN_NOTIFY_SECRET_FILE` | `WAWARDEN_NOTIFY_URL` is set without a secret file. |
| 23 | `notify_secret_unreadable` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file cannot be opened or read, or is not a regular file. |
| 24 | `notify_secret_permissions` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file grants write permission to its group or any permission to others. |
| 25 | `notify_secret_invalid` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file holds more than 4096 bytes, or less than 32 bytes apart from surrounding white space. |
| 26 | `backup_recipient_invalid` | `WAWARDEN_BACKUP_AGE_RECIPIENT` | The value is not exactly one age X25519 or hybrid recipient: it is empty, holds white space, a comment, a second recipient or an age secret key, or fails age's parser. The value is never repeated. |
| 27 | `backup_time_invalid` | `WAWARDEN_BACKUP_TIME` | The value is not a time of day written `HH:MM` with two digits each, from `00:00` to `23:59`: it is empty, has one digit for the hour, seconds, a zone or another separator. |
| 28 | `backup_keep_invalid` | `WAWARDEN_BACKUP_KEEP` | The value is not a whole number from 1 to 365 written in decimal digits: it is empty, `0`, above `365`, signed, has a leading zero or a unit. |
| 29 | `backup_option_without_recipient` | `WAWARDEN_BACKUP_TIME`, or `WAWARDEN_BACKUP_KEEP` when only it is set | `WAWARDEN_BACKUP_TIME` or `WAWARDEN_BACKUP_KEEP` is set while `WAWARDEN_BACKUP_AGE_RECIPIENT` is not. |
| 30 | `audit_retention_invalid` | `WAWARDEN_AUDIT_RETENTION_DAYS` | The value is not a whole number from 7 to 3650 written in decimal digits: it is empty, `0`, below 7, above 3650, signed, or has a leading zero, white space or a unit. |
| 31 | `unsafe_rate_caps_invalid` | `WAWARDEN_UNSAFE_RATE_CAPS` | The value is not `0` or `1`. |
| 32 | `read_per_minute_invalid` | `WAWARDEN_READ_PER_CLIENT_PER_MINUTE` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 600, or to 1000000 with `WAWARDEN_UNSAFE_RATE_CAPS=1`. Zero, a negative number and an empty value are always refused. |
| 33 | `search_per_minute_invalid` | `WAWARDEN_SEARCH_PER_CLIENT_PER_MINUTE` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 60, or to 1000000 with `WAWARDEN_UNSAFE_RATE_CAPS=1`. Zero, a negative number and an empty value are always refused. |
| 34 | `unsafe_send_caps_invalid` | `WAWARDEN_UNSAFE_SEND_CAPS` | The value is not `0` or `1`. |
| 35 | `send_global_per_hour_invalid` | `WAWARDEN_SEND_GLOBAL_PER_HOUR` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 60, or to 600 with `WAWARDEN_UNSAFE_SEND_CAPS=1`. Zero, a negative number and an empty value are always refused; `WAWARDEN_UNSAFE_RATE_CAPS` does not lift this cap. |
| 36 | `send_per_client_per_minute_invalid` | `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 10, or to 60 with `WAWARDEN_UNSAFE_SEND_CAPS=1`. Zero, a negative number and an empty value are always refused; `WAWARDEN_UNSAFE_RATE_CAPS` does not lift this cap. |
| 37 | `media_max_bytes_invalid` | `WAWARDEN_MEDIA_MAX_BYTES` | The value is not `0` or a whole number from 1 to 2147483648 written in decimal digits: it is empty, negative, above 2147483648, signed, or has a leading zero, white space or a unit. Neither unsafe caps switch lifts this cap. |
| 38 | `media_ref_retention_days_invalid` | `WAWARDEN_MEDIA_REF_RETENTION_DAYS` | The value is not a whole number from 1 to 365 written in decimal digits: it is empty, `0`, above 365, signed, or has a leading zero, white space or a unit. |
| 39 | `media_per_client_per_minute_invalid` | `WAWARDEN_MEDIA_PER_CLIENT_PER_MINUTE` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 30. Zero, a negative number and an empty value are refused; neither `WAWARDEN_UNSAFE_RATE_CAPS` nor `WAWARDEN_UNSAFE_SEND_CAPS` lifts this cap. |
| 40 | `media_global_per_hour_invalid` | `WAWARDEN_MEDIA_GLOBAL_PER_HOUR` | The value is not a whole number written in decimal digits, without a sign or leading zero, from 1 to 600. Zero, a negative number and an empty value are refused; neither `WAWARDEN_UNSAFE_RATE_CAPS` nor `WAWARDEN_UNSAFE_SEND_CAPS` lifts this cap. |
| 41 | `media_global_bytes_per_hour_invalid` | `WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR` | The value is not a whole number of bytes written in decimal digits, without a sign or leading zero, from 1 to 68719476736, or it is below `WAWARDEN_MEDIA_MAX_BYTES`. Zero, a negative number and an empty value are refused; neither unsafe caps switch lifts this cap. |
| 42 | `dev_fake_engine_invalid` | `WAWARDEN_DEV_FAKE_ENGINE` | A development build sees a value other than `0`, `1` or `wrong_account`. A release build refuses the name itself at check 2. |
| 43 | `running_as_root` | none | The real or the effective user ID is 0 and `--allow-root` was not given. |
| 44 | `data_dir_unusable` | `WAWARDEN_DATA_DIR` | The directory does not exist and cannot be created, or cannot be inspected; the path is empty. |
| 45 | `data_dir_not_directory` | `WAWARDEN_DATA_DIR` | The path is not a directory, or its last component is a symbolic link. |
| 46 | `data_dir_foreign_owner` | `WAWARDEN_DATA_DIR` | The directory is not owned by the process's effective user ID. |
| 47 | `data_dir_permissions` | `WAWARDEN_DATA_DIR` | The directory's mode is not exactly `0700`. |
| 48 | `history_dir_unusable` | none; the error names `history/` | `history` exists in the data directory but cannot be inspected. |
| 49 | `history_dir_not_directory` | none; the error names `history/` | `history` exists but is not a directory, or is a symbolic link. |
| 50 | `history_dir_foreign_owner` | none; the error names `history/` | `history` is not owned by the process's effective user ID. |
| 51 | `history_dir_permissions` | none; the error names `history/` | The mode of `history` is not exactly `0700`. |
| 52 | `backups_dir_unusable` | none; the error names `backups/` | `backups` exists in the data directory but cannot be inspected. |
| 53 | `backups_dir_not_directory` | none; the error names `backups/` | `backups` exists but is not a directory, or is a symbolic link. |
| 54 | `backups_dir_foreign_owner` | none; the error names `backups/` | `backups` is not owned by the process's effective user ID. |
| 55 | `backups_dir_permissions` | none; the error names `backups/` | The mode of `backups` is not exactly `0700`. |
| 56 | `spool_dir_unusable` | none; the error names `spool/` | `spool` exists in the data directory but cannot be inspected. |
| 57 | `spool_dir_not_directory` | none; the error names `spool/` | `spool` exists but is not a directory, or is a symbolic link. |
| 58 | `spool_dir_foreign_owner` | none; the error names `spool/` | `spool` is not owned by the process's effective user ID. |
| 59 | `spool_dir_permissions` | none; the error names `spool/` | The mode of `spool` is not exactly `0700`. |
| 60 | `keys_dir_unusable` | none; the error names `keys/` | `keys` in the data directory does not exist and cannot be created, or cannot be inspected. |
| 61 | `keys_dir_not_directory` | none; the error names `keys/` | `keys` is not a directory, or is a symbolic link. |
| 62 | `keys_dir_foreign_owner` | none; the error names `keys/` | `keys` is not owned by the process's effective user ID. |
| 63 | `keys_dir_permissions` | none; the error names `keys/` | The mode of `keys` is not exactly `0700`. |
| 64 | `master_key_unusable` | none; the error names `keys/master` | `keys/master` does not exist and cannot be created, or cannot be inspected, opened or read. |
| 65 | `master_key_not_regular` | none; the error names `keys/master` | `keys/master` is not a regular file: a symbolic link (even to a valid key), a directory or a named pipe, for example. |
| 66 | `master_key_foreign_owner` | none; the error names `keys/master` | `keys/master` is not owned by the process's effective user ID. |
| 67 | `master_key_permissions` | none; the error names `keys/master` | `keys/master` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 68 | `master_key_size` | none; the error names `keys/master` | `keys/master` does not hold exactly 32 bytes. |
| 69 | `retired_keys_dir_unusable` | none; the error names `keys/retired/` | `keys/retired` exists but cannot be inspected or listed. |
| 70 | `retired_keys_dir_not_directory` | none; the error names `keys/retired/` | `keys/retired` is not a directory, or is a symbolic link. |
| 71 | `retired_keys_dir_foreign_owner` | none; the error names `keys/retired/` | `keys/retired` is not owned by the process's effective user ID. |
| 72 | `retired_keys_dir_permissions` | none; the error names `keys/retired/` | The mode of `keys/retired` is not exactly `0700`. |
| 73 | `retired_key_name` | none; the error names `keys/retired/` | An entry of `keys/retired` whose name does not start with `.` is not named by a key id: 8 lower-case hexadecimal digits derived from the key (see [Master key](#master-key)). |
| 74 | `retired_keys_too_many` | none; the error names `keys/retired/` | `keys/retired` holds more than 32 keys. |
| 75 | `retired_key_unusable` | none; the error names the retired key | A retired key cannot be inspected, opened or read. |
| 76 | `retired_key_not_regular` | none; the error names the retired key | A retired key is not a regular file: a symbolic link (even to a valid key) or a directory, for example. |
| 77 | `retired_key_foreign_owner` | none; the error names the retired key | A retired key is not owned by the process's effective user ID. |
| 78 | `retired_key_permissions` | none; the error names the retired key | A retired key grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 79 | `retired_key_size` | none; the error names the retired key | A retired key does not hold exactly 32 bytes. |
| 80 | `retired_key_id_mismatch` | none; the error names the retired key | The key id derived from a retired key is not its name. |
| 81 | `retired_key_current` | none; the error names the retired key | A retired key is the current master key, as a [`keys rotate`](cli.md#keys-rotate) interrupted before it replaced `keys/master` leaves it; run `keys rotate` again. |
| 82 | `storage_filesystem_unknown` | none | The filesystem of the data directory cannot be inspected (`statfs`). |
| 83 | `storage_network_filesystem` | none | The storage profile is `local` and the data directory is on a network filesystem; see [Storage profiles](#storage-profiles). |
| 84 | `archive_db_unusable` | none; the error names `archive.db` | `archive.db` does not exist and cannot be created, or cannot be inspected. |
| 85 | `archive_db_not_regular` | none; the error names `archive.db` | `archive.db` is not a regular file: a symbolic link or a directory, for example. |
| 86 | `archive_db_foreign_owner` | none; the error names `archive.db` | `archive.db` is not owned by the process's effective user ID. |
| 87 | `archive_db_permissions` | none; the error names `archive.db` | `archive.db` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 88 | `archive_journal_unusable` | none; the error names `archive.db-journal` | `archive.db-journal` exists but cannot be inspected. |
| 89 | `archive_journal_not_regular` | none; the error names `archive.db-journal` | `archive.db-journal` exists but is not a regular file. |
| 90 | `archive_journal_foreign_owner` | none; the error names `archive.db-journal` | `archive.db-journal` is not owned by the process's effective user ID. |
| 91 | `archive_journal_permissions` | none; the error names `archive.db-journal` | `archive.db-journal` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 92 | `storage_ofd_unavailable` | none | On Linux, the storage profile is `local` and the kernel or the data directory's filesystem refused open-file-description locks; see [Storage profiles](#storage-profiles). |
| 93 | `archive_schema_newer` | none; the error names `archive.db` | The archive's schema version is newer than this build knows: a newer release wrote it. |
| 94 | `session_db_unusable` | none; the error names `session.db` | `session.db` does not exist and cannot be created, or cannot be inspected. |
| 95 | `session_db_not_regular` | none; the error names `session.db` | `session.db` is not a regular file: a symbolic link or a directory, for example. |
| 96 | `session_db_foreign_owner` | none; the error names `session.db` | `session.db` is not owned by the process's effective user ID. |
| 97 | `session_db_permissions` | none; the error names `session.db` | `session.db` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 98 | `session_journal_unusable` | none; the error names `session.db-journal` | `session.db-journal` exists but cannot be inspected. |
| 99 | `session_journal_not_regular` | none; the error names `session.db-journal` | `session.db-journal` exists but is not a regular file. |
| 100 | `session_journal_foreign_owner` | none; the error names `session.db-journal` | `session.db-journal` is not owned by the process's effective user ID. |
| 101 | `session_journal_permissions` | none; the error names `session.db-journal` | `session.db-journal` grants any access to group or others, or has the setuid, setgid or sticky bit. |

The table is in the order the checks run. Checks 1 to 43 read only the
environment and the process; checks 44 to 101 inspect the data directory.

- Checks 69 to 81 run only when `keys/retired` exists. Checks 75 to 81 run for
  each retired key in turn, in order of name.
- Checks 94 to 101 run on `session.db` after the archive is open, and not at
  all when the [fake engine](internals.md#fake-engine) runs. The
  [storage profile](#storage-profiles) is checked again for `session.db`,
  which passes once it passed for the archive.

What a refused start leaves behind:

- When the data directory is missing, `serve` creates it only after checks 1
  to 43 pass, so a start refused by any of them leaves nothing behind.
- `history/`, `backups/` and `spool/` are checked only when they exist;
  `serve` creates none of them at start (see [Data directory](#data-directory)).
- `keys/` and the master key are created, when missing, only after checks 44
  to 59 pass.
- An empty `archive.db` is created only after checks 44 to 83 pass, and an
  empty `session.db` only once the archive is open.
- A refusal by a later check (on a filesystem that forces its own ownership or
  mode, for example), or a `startup_failed` exit, leaves what was created in
  place: the data directory, `keys/`, the master key, `archive.db` and
  `session.db`.

## Data directory

The data directory is `WAWARDEN_DATA_DIR`, by default `/data`.

- Use an absolute path. A relative path is resolved against the process's working
  directory.
- When the directory does not exist, `serve` creates it with mode `0700`. Only the
  last path component is created; its parent must exist.
- Its last path component must not be a symbolic link. Symbolic links earlier in
  the path are followed.
- It must be a directory owned by the effective user ID of the process.
- Its mode must be exactly `0700`: full access for the owner, nothing for group or
  others, and no setuid, setgid or sticky bit. `0750`, `0755`, `0701` and `0500`
  are all refused.

The checks run once, at start. When they exist, `history/`, `backups/` and
`spool/` in the data directory must also be directories, not symbolic links,
owned by the effective user ID with mode exactly `0700` (refusals 48 to 59). Then `serve`
creates the [master key](#master-key) when it is missing and opens the
[message archive](#message-archive), `archive.db`, and the
[device store](#device-store), `session.db`. The
[engine](internals.md#whatsapp-engine) creates `history/` with mode `0700` when it first
downloads a [history-sync blob](internals.md#history-sync), and keeps each blob there as
`history/<id>.bin`, mode `0600`, only until it is processed or quarantined.
A [backup](#backups) creates `backups/` and `backups/tmp/` with mode `0700` when
it is first taken, and `serve` removes `backups/tmp/` at every start once
`archive.db` is locked. After each successful backup the service deletes backups
beyond `WAWARDEN_BACKUP_KEEP` from `backups/`, and refuses to delete anything
when that directory holds an entry it did not write. The directory must be on a writable, persistent
filesystem that supports hard links and that only the service's user can read.

`spool/` holds attachments staged on disk. Only an attachment over 16 MiB,
fetched while `WAWARDEN_MEDIA_MAX_BYTES` is above 16 MiB, is staged there, and
the first one creates `spool/` with mode `0700`. A staged file has mode `0600` and holds only ciphertext under a
key kept in memory. On the `local` [profile](#storage-profiles) its name is
removed as soon as it is created; on `nfs` it stays until its job ends. Once
`archive.db` is locked, `serve` removes every regular file in `spool/` at every
start, whatever `WAWARDEN_MEDIA_MAX_BYTES` is, and while that is above 0 it
removes every hour the staged files that no running job holds. A
[backup](#backups) never includes `spool/`.

Mechanisms that add group permissions or the setgid bit to a volume, such as
Kubernetes `fsGroup`, make the directory fail the mode check.

### Master key

`keys/master` in the data directory holds the service's master key: exactly 32
bytes. Right after the data directory checks, `serve`:

1. creates `keys/` with mode `0700` when it does not exist;
2. checks `keys/` (refusals 60 to 63): a directory, not a symbolic link, owned by
   the effective user ID, with mode exactly `0700`;
3. when `keys/master` does not exist, writes 32 bytes from the operating system's
   cryptographic random source to a new temporary file `keys/.master-<random>`
   with mode `0600`, flushes it to disk, hard-links it as `keys/master`, removes
   the temporary name and flushes the directory. When two processes start at once,
   the second link fails, and both use the key that was linked first. `serve`
   never replaces an existing key; only [`keys rotate`](cli.md#keys-rotate) does;
4. checks `keys/master` (refusals 64 to 68): a regular file, not a symbolic link,
   owned by the effective user ID, without any permission for group or others and
   without the setuid, setgid or sticky bit, holding exactly 32 bytes. Mode `0600`
   and mode `0400` are both accepted;
5. when `keys/retired` exists, checks it and the retired keys in it (refusals 69
   to 81): a directory, not a symbolic link, owned by the effective user ID, with
   mode exactly `0700`, holding at most 32 keys besides names that start with
   `.`, each named by its key id and checked as `keys/master` is, none of them
   the current master key. `keys rotate` puts each master key it replaces there.

The service derives every key it uses from the master key with HKDF-SHA256 (no
salt, the information string `wawarden/v1/` followed by the purpose) and keeps
only the derived keys, of the master key and of each retired key:

| Purpose | Length | Use in this build |
|---|---|---|
| `key-id` | 4 bytes | The key id: logged as 8 hexadecimal digits in the `keys_loaded` event, so that log lines can be grouped by the key their pseudonyms were made with, and in each [audit line](observability.md#audit-chain). It names a retired key's file, and the key of each audit row, of the audit anchor, and of each cursor and message reference. |
| `log-redact` | 32 bytes | The key of the [log pseudonyms](observability.md#pseudonyms-and-dropped-lines). |
| `chat-hmac` | 32 bytes | The `chat_hmac` of [audit lines](observability.md#audit-chain): the first 16 bytes of HMAC-SHA256 under this key over the chat's canonical identifier. It also keys their `message_hmac`, over a separate domain (see [Audit row HMAC](internals.md#audit-row-hmac)). No [notification event](observability.md#notifications) names a chat. |
| `cursor-seal` | 32 bytes | Seals the `next` cursors of the [client API](api.md#cursors-and-message-references). |
| `mref` | 32 bytes | Seals the [message references](api.md#cursors-and-message-references) of the client API. |
| `media-ref` | 32 bytes | Seals the [media reference](internals.md#media-reference) the engine keeps for an attachment while `WAWARDEN_MEDIA_MAX_BYTES` is above 0; a retired key's opens what it sealed. The engine opens one only to fetch the attachment a client asked for, and only for the message it was sealed for. |
| `audit-chain` | 32 bytes | The key of the [audit chain](observability.md#audit-chain)'s row HMACs. |

What leaves the service:

- Apart from the log pseudonyms, the chat HMACs and the chain heads of audit
  lines, the key id is the only value derived from the master key that the
  service writes out. No key reaches a log, an event, a metric or a response.
- `serve` writes the master key only to `keys/master`, through the temporary
  name in step 3. [`keys rotate`](cli.md#keys-rotate) writes keys only to
  `keys/master` and `keys/retired/`, through temporary names. Neither writes a
  derived key.

Replacing or deleting the master key changes the key id and every log
pseudonym. Audit rows written afterwards carry the new key id, so
`audit verify` reports the older rows as `unknown_key` unless it is given the
old key as a retired key: keep a copy of every master key the service ran
with.

[`keys rotate`](cli.md#keys-rotate) replaces the master key and keeps the old
one in `keys/retired/`. Everything new is sealed, chained and keyed under the
master key. The service uses a retired key only:

- to open what was sealed under it: a cursor or a message reference whose key
  id is a retired key's;
- to verify the audit rows and the audit anchor made under it, before the
  [audit retention](observability.md#audit-retention) removes them.

To provide your own key, write 32 random bytes to `keys/master` with mode
`0600` or `0400`, owned by the service's user, before the first start.

A crash or power loss during the first start can leave a
`keys/.master-<random>` file behind, with mode `0600`. It holds either an
unused key or, after a crash right after the link, a second name of the
current master key. Delete it together with `keys/master` when you replace the
key; it can be deleted at any time while the service is stopped.

### Message archive

`archive.db` in the data directory is the message archive: a SQLite database
written by the SQLite engine compiled into the binary (`modernc.org/sqlite`).
`serve` creates it, applies its schema and records each start in it, and the
[engine](internals.md#whatsapp-engine) writes WhatsApp traffic into it once a device is
paired.
Right after the master key, `serve`:

1. checks the data directory's filesystem against the
   [storage profile](#storage-profiles) (refusals 82 and 83);
2. creates `archive.db` empty with mode `0600` when it does not exist, and
   refuses (84 to 87) one that is not a regular file, belongs to another user,
   or grants any access to group or others; then refuses (88 to 91) an
   `archive.db-journal` that exists and fails the same checks;
3. opens one connection and reads back every setting it applies, refusing the
   connection on any difference: foreign keys on, the rollback journal in
   `TRUNCATE` mode, `synchronous` `FULL`, exclusive locking, temporary storage
   in memory, `secure_delete` `ON` (not `FAST`) and a 5-second busy timeout.
   None of these is configurable; write-ahead logging, `PERSIST` and every
   other journal mode are refused, and a database left in write-ahead-log mode
   is converted back to a rollback journal;
4. takes an exclusive lock on the database, which no other process can read or
   write while the service runs. When another process holds it, `serve` logs
   `db_lock_wait` once and retries with growing pauses for up to five minutes,
   then exits `1` with `startup_failed`. A stop signal ends the wait within one
   busy timeout and also exits `1`. On Linux, profile `local` takes it with
   open-file-description locks, and a kernel or filesystem that refuses them
   is refused in turn (83) instead of falling back to classic locks;
5. brings the schema up to date (an archive written by a newer release is
   refused, 84): each schema version is applied in one transaction that has no
   deadline, because it runs only here, under the lock, and can take seconds on
   a large archive (about 8 seconds for 500,000 messages on a laptop). Moving
   to version 3 then numbers the changes of the messages already stored, in
   transactions of 10,000 messages that each run under the 5-minute rewrite
   deadline, and a stop in between leaves the rest to the next start. Version 4
   follows version 3 and only adds a column, tables, indexes and triggers and
   rebuilds the identity-mapping table, which takes one to two seconds per
   500,000 messages on a laptop, most of it checking the new column's
   constraint against every stored message. Version 5 follows version 4 and
   adds tables, columns and indexes and clears the media type of revoked
   messages, which takes under a second per 500,000 messages on a laptop. Then
   `serve` completes a rewrite of the full-text index that a stop or a failed
   rewrite left due (see the end of this section) and logs `archive_opened`.

The connection that holds the lock is the only one the service ever opens to
the archive. It survives a call that runs out of time, and should it ever be
lost, the service refuses to open another, logs `db_lost` and answers `503` on
`/healthz` from then on; restart it. No other code in the service opens the
database file, and on Linux, profile `local` uses open-file-description locks,
so that closing some other descriptor of the file cannot drop the lock; it
refuses to start (`storage_ofd_unavailable`) where the kernel or the filesystem
rejects them, because SQLite would otherwise fall back to classic locks without
a sign.

The journal, `archive.db-journal`, appears at the first write and is truncated to
zero bytes after every transaction; SQLite gives it the database file's mode.
Temporary data stays in memory, so the service writes no other file next to the
archive.

Every read of the archive must finish within 2 seconds, every write within
10 seconds and every rewrite of its full-text index within 5 minutes; a schema
migration has no deadline (step 5 above). A call
that runs out of time is logged as `db_deadline` when its deadline passes, while
it still runs, with the profile of every goroutine of the process (function
names and source positions only), and is interrupted: SQLite stops its statement
between two steps, and the call fails. A call that waits on the filesystem, such
as a read that hangs on a network filesystem, or that is committing, is not
interrupted and runs until that wait or the commit ends; a write whose commit
ends succeeds. A deadline that passes just as a statement starts can be lost in
the database driver: that statement then runs to its end before the call fails.

Revoked, edited and expired message text is removed from the database file,
its journal and the full-text index, and the media type and media reference of
a revoked or expired message from the database file and its journal, as
described in the [threat model](threat-model.md#security-invariants) (I-8).
Outside that guarantee are:

- backups, which keep the old text, media type and reference until
  [backup retention](#backup-retention) deletes them;
- the same text, or any three-character run of it, or the same media type,
  held elsewhere in the archive: by another message, a message waiting in the
  inbox, a push name, a chat name or group subject, an identifier or the
  schema;
- an edit, or a revocation the archive would have refused for a stored
  target, that arrives before history sync has stored its target, and a late
  copy of a revoked message whose tombstone has lapsed or was dropped, or whose
  sender's canonical identifier changed in between (see
  [Ingest](internals.md#ingest)).

To keep removed text out of the full-text index, a deletion can make the
service rewrite the whole index right after it commits. A rewrite holds the
archive while it runs, so other calls wait and may reach their own deadlines.
It needs free space of about the index's size. A rewrite that did not commit
runs again at the next start, before `archive_opened` is logged.
[Full-text index rewrites](internals.md#full-text-index-rewrites) explains when one is
needed.

The archive's tables are listed under [Archive schema](internals.md#archive-schema).

### Device store

`session.db` in the data directory is the device store: the keys and state of the
linked device that the WhatsApp protocol library (`go.mau.fi/whatsmeow`) keeps,
in its own tables, through the same SQLite engine. Right after the archive,
`serve` opens it with every step the archive gets: the storage profile is
checked, the file is created empty with mode `0600` when it does not exist, the
file and its journal must pass refusals 94 to 101, the connection reads back the
same fixed settings and takes the same exclusive lock, kept the same way, with
`db_lock_wait` naming `session` and `db_lost` too. Then the protocol library
brings its tables up to date, within one minute. A store written by a newer
version of the protocol library is used unchanged when it declares itself compatible
with the version this build links, and stops the start with `startup_failed`
when it does not. `serve` logs
`session_opened` with `paired` set to whether the store holds a linked device.
A development build running the [fake engine](internals.md#fake-engine) opens no device
store: it neither creates nor reads `session.db`.

The protocol library runs its own queries on that connection, so the read and
write deadlines of the archive do not apply to them, and it keeps only its own
log lines out of the store: the store gets no logger, and its few log lines are
discarded. A store holds at most one device; a second one stops the start. Never
copy `session.db` to a second running service: two services that hold the same
device replace each other's connection (`replaced`).

### Storage profiles

| Profile | Use it for | Locks | Free-space floor |
|---|---|---|---|
| `local` (default) | A local disk or a container's own filesystem | Open-file-description locks on Linux, where a kernel or filesystem that refuses them is refused (`storage_ofd_unavailable`); classic POSIX record locks elsewhere | Applies |
| `nfs` | A network filesystem such as Amazon EFS | Classic POSIX record locks | Does not apply |

With profile `local`, `serve` refuses a data directory on a network filesystem
(`storage_network_filesystem`). On Linux it recognises NFS (Amazon EFS
included), SMB and CIFS, Ceph, AFS, Coda, NCP and 9P by the filesystem type that
`statfs` reports; on macOS, any mount without the local flag. FUSE filesystems
are not recognised, so do not run profile `local` on a FUSE mount of remote
storage. Profile `nfs` accepts any filesystem. Both profiles use the same
rollback journal and exclusive locking, and neither ever uses write-ahead
logging. Running SQLite on a network filesystem remains outside SQLite's
recommended configurations, and only one process may use the data directory at
a time.

### Free space

`WAWARDEN_MIN_FREE_BYTES` sets a floor on the free space of the data
directory's filesystem, for profile `local` only: below it ingest pauses, and it
resumes once the free space reaches 1.25 times the floor. `0` turns the floor
off. The [engine](internals.md#whatsapp-engine) measures the free space when it starts and
every 30 seconds, also while it works through its inbox or history sync: it
checks whether a measurement is due before each inbox row, each history blob
and each batch of a blob. While ingest is paused it writes no message and no
history-sync notification and leaves them unacknowledged, which loses them (see
[Delivery](internals.md#delivery)), and applies nothing from its inbox or from history sync;
a blob that the pause
interrupts stays waiting without using an attempt and is applied from its start
once ingest resumes. Pausing reports the
[notification event](observability.md#notifications) `ingest_paused`, and resuming logs
`ingest_resumed`. A [backup](#backups) refuses to start, with
`backup_failed` reason `space`, when its copies could take the free space
below the floor. With profile `nfs` the floor
never applies, because a network filesystem such as Amazon EFS grows on demand;
watch its own capacity metrics instead.

## Backups

With `WAWARDEN_BACKUP_AGE_RECIPIENT` set, the service writes encrypted backups of
the [archive](#message-archive) and the [device store](#device-store) to
`backups/` in the data directory. It holds only the
[age](https://age-encryption.org) recipient, a public key: it encrypts to it and
can never read a backup back. Without the variable no backup is ever taken, and
every start logs the warning `backup_disabled`. The [fake engine](internals.md#fake-engine)
of a development build opens no device store, so it takes no backup either and
logs the same warning even with the variable set.

### Backup schedule

Every 30 seconds the backup scheduler checks, in this order, whether
the initial backup and whether a nightly backup are due.

- *Initial backup.* One backup is taken for each paired device once its
  initial [history sync](internals.md#history-sync) has settled. The first time the
  scheduler sees the owner's device paired, it records that moment in the
  archive's `sync_state` table as `backup_paired_at`, so a restart does not
  start the wait again. The backup is taken once no history blob is waiting to
  be processed (a [quarantined](internals.md#history-sync) blob does not count) and 10
  minutes have passed since the later of that moment and the arrival of the
  latest history blob. Its success is recorded as `backup_taken_at`. When a
  check finds the device no longer paired, or the engine has reported an
  unpairing since the previous check (a device unpaired and paired again
  between two checks), both keys are cleared, so the next paired device is
  backed up once in turn, without a restart.
- *Nightly backup.* Once a backup has succeeded, one backup is owed per day at
  the slot `WAWARDEN_BACKUP_TIME`, a UTC time of day (default `03:00`). Every
  successful backup, initial or nightly, records the time it began as
  `backup_last_at`, which unpairing does not clear. A `backup_taken_at` without
  a `backup_last_at` counts as one. A backup is owed when the newest success
  began before the most recent slot, or is recorded later than the current
  time, so that a clock set back cannot silence backups. This is also the
  catch-up: after downtime across one or more slots, the first check after the
  start takes one backup at once, while a start shortly before the slot, with
  the previous slot's backup done, waits for the slot. A service that has
  never taken a backup, such as one never paired, takes none. The nightly
  backup does not depend on the engine: an unpaired service goes on backing
  up its archive. When neither database has been written since the last
  backup this process took and recorded, the slot is skipped and
  `backup_skipped` is logged once; the first slot after a start always backs
  up, and a paired, connected service writes often enough that it practically
  never skips. A write that lands between the end of a backup's copy and its
  bookkeeping is noticed only with the next write.
- *Failures.* A failed backup, initial or nightly, reports `backup_failed` and
  is tried again 15 minutes later, then after 30 minutes, 1, 2 and 4 hours,
  then every 4 hours, until one succeeds and its time is recorded, which
  resets the wait. The wait is held in memory: after a restart a backup that
  is due is tried at once. A backup cut short by shutdown (`cancelled`) is not
  retried before the next start.

At each slot, and at the first check after a start, the scheduler then deletes
the rows of [history blobs](internals.md#history-sync) processed more than 30 days before,
in batches of 10,000 rows for at most 15 minutes, and logs
`history_blobs_pruned` when it deleted any. It does so with or without a
recipient.

### How a backup is written

First, on the `local` [storage profile](#storage-profiles) with a
non-zero [free-space floor](#free-space), the backup checks that the data
directory has at least the floor plus the size of the larger database plus the
sizes of both databases free, since its staging copy and its output come out of
the space ingest needs; otherwise it fails with reason `space` and writes
nothing. Each database is copied online, in steps of 256 pages on its own
connection, each step bound by the 10-second write deadline, so ingest goes on
between steps. A copy that stops early, for a failure or a cancellation, is
closed on that connection as soon as the connection is free, waiting out other
calls if it must, so that no copy stays attached to the database unless the
database is closed first. The archive is copied first, then
the device store, so the two
copies are separate points in time. Each copy is staged as a plaintext file of
mode `0600` in `backups/tmp/` (`archive.stage`, then `session.stage`, one at a
time) and then streamed, without a second plaintext copy, through age into
`backups/tmp/<time>.age`. That file is flushed to disk with `fsync`, renamed to
`backups/<time>.age` and the directory is flushed in turn; `backups/tmp/` is then
removed. `<time>` is the UTC time the backup began, as `20261005T120000Z`. A
backup whose name already exists is refused. Any failure removes `backups/tmp/`
with the staging copies and the partial file, and a file renamed into place but
not flushed, then reports `backup_failed`; success reports `backup_done`.

### Backup files

| `backups/` entry | Mode | Holds |
|---|---|---|
| `backups/` | `0700` | Created at the first backup; checked at start like the data directory when it exists. |
| `backups/tmp/` | `0700` | Only while a backup runs: one plaintext staging copy at a time and the encrypted file being written. |
| `backups/<time>.age` | `0600` | One finished backup: a binary (not armoured) age file. |

Decrypted, a backup is a tar archive of three files: `archive.db`, `session.db`
and `manifest.json`. The manifest holds `format` (`1`), the build `version`, the
`created` time in UTC and, for each database, its `name`, `schema_version` and
size in `bytes`; it holds no identifier. The two databases hold everything the
archive and the device store hold, the linked device's keys included. The
[master key](#master-key) `keys/master` and the retired keys of `keys/retired/`
are not in a backup.

### Backup retention

After each successful backup, and only then, the service deletes old backups.
It keeps:

- the newest `WAWARDEN_BACKUP_KEEP` backups (default `14`), ordered by the
  time in their names, never by the files' modification times;
- the file just written, even when a clock set back gave it an older name.

It never keeps fewer than one. It deletes only regular files of mode `0600`,
owned by the service's user, whose name is a backup's name with a valid time.
It accepts `backups/tmp/` only as a `0700` directory of that user, which it
does not enter. Any other entry in `backups/` makes it delete nothing and
report `backup_prune_failed` with reason `unexpected_entry`, after every
backup until the entry is removed. Such an entry is, for example, a symbolic
link, another directory, a copy under another name, or a backup with another
mode or owner.

A full disk never makes the service delete backups: it reports
`ingest_paused` or `backup_failed` with reason `space` and leaves the decision
to the operator. On an active service the backups cover the last
`WAWARDEN_BACKUP_KEEP` nights. Copies of the data directory made by the
storage, such as snapshots or recovery points, hold deleted backups for as
long as they are kept.

### Backup failure reasons

`backup_prune_failed` gives one of these reasons:

| `backup_prune_failed` reason | Old backups were not all deleted because |
|---|---|
| `directory` | `backups/` cannot be read or inspected, or is not a directory owned by the service's user with mode exactly `0700`. Nothing was deleted. |
| `unexpected_entry` | `backups/` holds an entry the service did not write, as above. Nothing was deleted. |
| `remove` | Deleting a file failed; the files deleted before it stay deleted and the rest are kept until the next backup. |
| `sync` | Flushing `backups/` to disk after the deletions failed. |

`backup_failed` gives one of these reasons:

| `backup_failed` reason | The backup failed because |
|---|---|
| `directory` | `backups/` or `backups/tmp/` cannot be created or inspected, or is not a directory owned by the service's user with mode exactly `0700` (a symbolic link is refused). |
| `exists` | A file with the backup's name already exists in `backups/`, or its absence cannot be checked. |
| `output` | The encrypted file cannot be created in `backups/tmp/`. |
| `archive` | Copying `archive.db` or writing it into the encrypted file failed, for example on a full disk. |
| `session` | The same for `session.db`. |
| `manifest` | Writing the manifest failed. |
| `encrypt` | Starting or finishing the encryption failed, for example when its last chunk cannot be written. |
| `sync` | Flushing the file or `backups/` to disk failed. |
| `rename` | Renaming the file into `backups/` failed. |
| `cleanup` | Removing `backups/tmp/` failed after the file was in place; the file is removed too. |
| `cancelled` | The service began shutting down during the backup. |
| `space` | The data directory has less free space than the floor plus the larger database plus both databases, or the free space or a database's size cannot be read. Only on the `local` profile with a non-zero floor. |

### Checking a backup offline

Create the key pair on a machine other than the service's host with the
[age](https://github.com/FiloSottile/age) tools. Keep the identity file there,
and set the public key it prints as the recipient:

```sh
age-keygen -o backup-identity.txt     # prints "Public key: age1..."
```

Copy a backup off the host and check it on that machine with
[`backup verify`](cli.md#backup-verify), which reads what `age` decrypts and holds
no key itself; that section shows the pipeline. A truncated or altered file
fails to decrypt, and so does a file given the wrong identity. With
`--extract-to`, `backup verify` leaves `archive.db` and `session.db` in a new
directory, for a restore or for [`audit verify`](cli.md#audit-verify).

To inspect a backup by hand instead, `age --decrypt ... | tar -x` gives the
three files. `sqlite3` needs version 3.34 or later with FTS5 to read the
archive's full-text index. The decrypted files hold the whole archive and the
linked device's keys: keep them as private as the data directory, and delete
them when done.

### Restores

No command restores a backup. A restore is done by hand, into a stopped
service, by the procedure in the [runbook](runbook.md#restoring-a-backup),
which also lists what a restore takes back to the time of the backup. Two
facts from this page shape it:

- A backup holds `archive.db` and `session.db` as two points in time, the
  archive's first. Restore both from the same backup, never one from another.
- No backup holds `keys/master` or `keys/retired/`. Keep your own copy of
  them. Without the master key, the restored service creates a new one, and
  audit rows written under the old key verify as `unknown_key` (see
  [Master key](#master-key)).

## Container image

Release images are published as `ghcr.io/dortort/wawarden` for `linux/amd64` and
`linux/arm64`, by the release workflow only. Deploy them by digest;
[`RELEASING.md`](../RELEASING.md) explains how to verify one, and
[the hardening guide](hardening.md#container-hardening) gives the flags to run
one with. Release images follow this contract:

| Item | Value |
|---|---|
| Base | `gcr.io/distroless/static-debian13:nonroot`, pinned by digest: no shell, no package manager |
| Binary | `/wawarden`, mode `0555` |
| Entrypoint and command | `ENTRYPOINT ["/wawarden"]`, `CMD ["serve"]`; pass another subcommand as the command, for example `admin init` |
| User | `65532:65532` |
| Writable path | `/data`, the only path the service writes and the only one that must be writable: an empty directory owned by `65532:65532` with mode `0700`; mount a volume there |
| Directories and modes | In `/data` the service creates `keys/`, `history/`, `backups/` and `backups/tmp/` with mode `0700`, and every file (`keys/master`, `archive.db`, `session.db` and their journals, history blobs, backups) with mode `0600`, and [`keys rotate`](cli.md#keys-rotate) creates `keys/retired/` with mode `0700` and the retired keys in it with mode `0600`; every directory must keep mode `0700` and the service's owner, see [Data directory](#data-directory) |
| Root filesystem | Mount it read-only (`docker run --read-only`, or `readOnlyRootFilesystem: true` in Kubernetes) so that `/data` is also the only writable path; the service writes nothing outside the data directory |
| Listeners | Three, plain HTTP, by default client `127.0.0.1:8080`, admin `127.0.0.1:8082` (only with an admin token hash) and health `127.0.0.1:8081` (loopback only); see [Listeners](api.md#listeners) |
| Network | Inbound: the three listeners only. Outbound: name resolution; WhatsApp over HTTPS and a WebSocket on port 443, only while a device is paired or pairing was requested; and the [webhook](observability.md#webhook)'s host and port when `WAWARDEN_NOTIFY_URL` is set. Nothing else, and never through a proxy |
| Health check | `["/wawarden","healthcheck"]`, in exec form; the image declares no health check of its own |
| Exposed ports | None declared |
| Labels | `org.opencontainers.image.source`, `licenses` (`GPL-3.0-or-later`), `version`, `revision` |
| Licence | WaWarden is `GPL-3.0-or-later`; the image links modules under GPL-3.0, MPL-2.0, Apache-2.0, ISC, MIT and BSD-3-Clause, listed in the [README](../README.md#licence) |
| Licence texts | `/licenses`: the licence files of the Go standard library (`std@<Go version>`), of WaWarden and of every other Go module linked into `/wawarden`, one directory per module and version, and also those of the modules only the macOS binaries link, such as `github.com/ncruces/go-strftime`; see [`RELEASING.md`](../RELEASING.md#licences) |


### Data volume

An empty named Docker volume mounted on `/data` takes the image directory's
owner and mode, so it passes the [data directory](#data-directory) checks as
is. A volume on a network filesystem, such as Amazon EFS, needs
`WAWARDEN_STORAGE_PROFILE=nfs`; see [Storage profiles](#storage-profiles). A
bind-mounted host directory must be owned by `65532:65532` with mode `0700`,
or `serve` refuses it (`data_dir_foreign_owner` or `data_dir_permissions`).

### Health check form

Use the exec form: `test: ["CMD", "/wawarden", "healthcheck"]` in Compose, an
`exec` probe with `command: ["/wawarden", "healthcheck"]` in Kubernetes.
`docker run --health-cmd` wraps the command in a shell, which this image does
not have, so that check always fails. The check runs inside the container,
where the health listener's loopback address is reachable.

### Reaching the listeners

Inside a container, a listener on `127.0.0.1` is reachable only from the same
network namespace, for example from a sidecar that forwards to it. Port
publishing does not reach it. To publish a port, set
`WAWARDEN_LISTEN=0.0.0.0:8080` (and `WAWARDEN_ADMIN_LISTEN=0.0.0.0:8082` if
needed). The service then logs `listener_not_loopback` at every start, and
every host or container that can reach the container's address can reach the
listener. Publish the port on the host's loopback address only, as the
Compose example below does.

### Running as root

When the runtime overrides the user to root, `serve` refuses to start
(`running_as_root`) unless it is given `--allow-root`. With `--allow-root`,
the data directory must be owned by root with mode `0700`. The image's
`/data`, and a named volume initialised from it, are owned by `65532:65532`,
so `serve` refuses them with `data_dir_foreign_owner`.

### Docker Compose example

A Compose service that follows the contract above and the
[hardening guide](hardening.md#container-hardening): a read-only root
filesystem, user `65532:65532`, a named volume on `/data`, the exec-form
health check, and ports published on the host's loopback address only. The
client and admin listeners bind `0.0.0.0` inside the container so that the
published ports reach them, which logs `listener_not_loopback` at every start.

```yaml
services:
  wawarden:
    image: ghcr.io/dortort/wawarden@sha256:<digest>
    user: "65532:65532"
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    pids_limit: 256
    mem_limit: 512m
    stop_grace_period: 15s
    restart: unless-stopped
    environment:
      WAWARDEN_OWNER_PHONE: "+15550100001"
      WAWARDEN_ADMIN_TOKEN_SHA256: "<the sha256 line printed by wawarden admin init>"
      WAWARDEN_BACKUP_AGE_RECIPIENT: "<age1... public key printed by age-keygen>"
      WAWARDEN_LISTEN: "0.0.0.0:8080"
      WAWARDEN_ADMIN_LISTEN: "0.0.0.0:8082"
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:8082:8082"
    volumes:
      - wawarden-data:/data
    healthcheck:
      test: ["CMD", "/wawarden", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s

volumes:
  wawarden-data: {}
```

- `WAWARDEN_DATA_DIR` keeps its default, `/data`, the volume's mount point.
- The health listener keeps its loopback default and its port is not
  published: the health check runs inside the container.
- `stop_grace_period: 15s` gives the service more than its 10-second
  [shutdown](cli.md#shutdown) grace period. Compose's default is 10 seconds.
- `mem_limit: 512m` leaves room for history sync at the default
  `WAWARDEN_HISTORY_MAX_BYTES`; see [Memory bounds](internals.md#memory-bounds).
  With `GOMEMLIMIT` unset, WaWarden sets a soft memory limit of 80% of it,
  about 410 MiB; [container hardening](hardening.md#container-hardening) gives
  the sizing rule. Raise it to `768m` when you set `WAWARDEN_MEDIA_MAX_BYTES`
  above 0 with the other attachment settings at their defaults.
- Admin commands run inside the container and read the token on standard
  input. `-T` gives the command no terminal, so `--token-stdin` reads the
  redirected file:

  ```sh
  docker compose exec -T wawarden /wawarden admin status --token-stdin < admin.token
  ```
