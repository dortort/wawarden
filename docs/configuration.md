# Configuration reference

This is the authoritative reference for configuring and running WaWarden. It
describes the current build on `main`, which holds milestone **M1** and is not
released yet; the latest release, `v0.1.0`, is milestone **M0**. To M0's
configuration checks, listeners, admin token and metrics, M1 adds the
[master key](#master-key), the pseudonyms and dropped lines in the
[logs](#pseudonyms-and-dropped-lines), the
[request-body decoder](#request-bodies), the
[message archive](#message-archive), the [device store](#device-store), the
[WhatsApp engine](#whatsapp-engine) with its adapter to the WhatsApp protocol
library, the [admin routes](#admin-routes) with the
[admin commands](#admin-status-admin-pair-and-admin-reconnect) that call them,
[notifications](#notifications),
[metrics in embedded metric format](#embedded-metric-format),
[backups](#backups) and, in development builds only, the
[fake engine](#fake-engine). The client API, client tokens and sending come
with M2 and M3. A service without a paired device makes no connection to
WhatsApp until `wawarden admin pair` requests pairing. Everything listed here
is implemented, and nothing else is. Settings planned for later milestones are
listed under [Reserved names](#reserved-names) and are refused by this build.

## Unknown variables stop the service

> **`wawarden serve` refuses to start when the environment contains any variable
> whose name begins with `WAWARDEN_` and that this build does not implement.** A
> typo, a variable meant for a later release, or a leftover from another
> deployment stops the service with the reason code `unknown_variable` instead of
> being ignored. An empty value counts as set:
> `WAWARDEN_SEND_GLOBAL_PER_HOUR=` is refused as well.

Only the names in [Environment variables](#environment-variables) are accepted.
Names are matched exactly, so a lower-case `wawarden_listen` is not recognised as a
WaWarden variable and has no effect.

### Reserved names

These names are reserved for later milestones. Do not set them before the
release that implements them: until then, each one makes `serve` refuse to start
with `unknown_variable`.

| Variable | Planned purpose | Becomes valid in |
|---|---|---|
| `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` | Per-client send rate limit | M3 |
| `WAWARDEN_SEND_GLOBAL_PER_HOUR` | Global send rate limit | M3 |

Further names are planned and not final yet: `UNSAFE_` overrides of the
rate-limit hard caps (M2 for read and search limits, M3 for send limits). Any of
them set as a `WAWARDEN_` variable is refused with `unknown_variable` until the
release that implements it.

`WAWARDEN_DEV_*` names are reserved for development builds; a release build
refuses every one of them with `dev_variable_in_release`. A development build
implements `WAWARDEN_DEV_FAKE_ENGINE` (see [Fake engine](#fake-engine)) and
refuses any other `WAWARDEN_DEV_*` name with `unknown_variable`.

## Subcommands

```text
wawarden [serve] [--allow-root]  run the gateway (the default)
wawarden healthcheck             exit 0 only when the local /healthz answers 200
wawarden version                 print the version and build flavour
wawarden admin init              generate an admin token and its SHA-256
wawarden admin status|pair|reconnect [--addr URL] --token-file PATH | --token-stdin | --token-command COMMAND
                                 call the admin listener (default --addr http://127.0.0.1:8082)
```

### `serve`

Runs the service. It is the default: `wawarden`, `wawarden serve`,
`wawarden --allow-root` and `wawarden serve --allow-root` all run it. It reads
the [environment variables](#environment-variables), applies every
[startup check](#startup-refusals), opens the [listeners](#listeners) and runs
until it receives `SIGTERM` or `SIGINT` (see [Shutdown](#shutdown)).

Flags:

| Flag | Default | Effect |
|---|---|---|
| `--allow-root` | off | Permits running with a real or effective user ID of 0. Without it, `serve` refuses to start as root (`running_as_root`). With it, the [data directory](#data-directory) must still be owned by the effective user ID, so a root process needs one owned by root with mode `0700`. |

Flags follow Go's `flag` package: `-allow-root`, `--allow-root` and
`--allow-root=true` are equivalent. Any other flag or argument is a usage error.

### `healthcheck`

Sends `GET http://<health address>/healthz` and exits `0` only when the answer is
`200`. Every other outcome exits `1` with one line on standard error: another
status, a redirect (redirects are not followed), a connection error, no answer
within 2 seconds, or an invalid health address.

It reads only `WAWARDEN_HEALTH_LISTEN` (default `127.0.0.1:8081`) and ignores
every other variable, so it can run with the same environment as `serve`. The
address must pass the same checks as for `serve`: an IP literal on a loopback
address. It is the container health check command: `["/wawarden","healthcheck"]`.

### `version`

Prints three lines on standard output and exits `0`:

```text
wawarden <version>
revision <commit>
dev build <true|false>
```

`<version>` is the release version for release binaries and `dev` for any other
build. `<commit>` is the VCS revision recorded by `go build`, followed by
`(modified)` when the working tree was dirty, or `unknown` when the build recorded
none. `dev build` is `true` only for binaries built with the `dev` build tag.

### `admin init`

Generates an admin token offline and exits `0`. It reads no configuration and
contacts no service. Standard output receives exactly two lines:

```text
token: wwadm_<43 characters>_<8 hexadecimal characters>
sha256: <64 hexadecimal characters>
```

Standard error receives a one-line reminder. The token is printed once and never
again; store it in a secret manager and configure only its SHA-256, in
`WAWARDEN_ADMIN_TOKEN_SHA256` or `WAWARDEN_ADMIN_TOKEN_SHA256_FILE`.

The token is `wwadm_`, then 32 bytes from the operating system's cryptographic
random source in unpadded base64url, then `_` and the CRC-32 (IEEE) of everything
before that last underscore as eight lower-case hexadecimal digits. The admin
listener accepts only tokens of exactly this form; see
[Admin authentication](#admin-authentication).

The shell, not WaWarden, creates the file when you redirect this output; set a
restrictive `umask` or write it into a private directory.

The other `admin` subcommands of this build call the admin listener; see
below. Client management (M2) and `backfill` (M3) do not exist in this build and
are usage errors.

### `admin status`, `admin pair` and `admin reconnect`

These commands are HTTP clients of the [admin listener](#admin-routes). Each
makes one request with the admin token and prints the answer:

| Command | Request | Prints on standard output |
|---|---|---|
| `admin status` | `GET /admin/v1/status` | One `key: value` line each for `state`, `reason` (`none` when empty), `paired`, `chats`, `messages`, `history blobs pending`, `history blobs quarantined`, `inbox backlog`, `inbox quarantined`, `last ingest` (`never` before the first message) and `version`. |
| `admin pair` | `POST /admin/v1/pair` with the body `{}` | `pairing code: <code>`. Standard error says where to enter it on the phone: Linked devices, Link a device, then Link with phone number instead. |
| `admin reconnect` | `POST /admin/v1/reconnect` with the body `{}` | `reconnect requested` |

Flags:

| Flag | Default | Effect |
|---|---|---|
| `--addr URL` | `http://127.0.0.1:8082` | The admin listener's URL: `http` or `https`, with a host, without credentials, a query or a fragment. A path is kept, so a reverse proxy can serve the listener under a prefix. With `http` and a host that is neither `localhost` nor a loopback address, the command warns on standard error that the token crosses the network unencrypted, and goes on. |
| `--token-file PATH` | | Reads the token from a file. Symbolic links are followed; the file must be a regular file of at most 4096 bytes. |
| `--token-stdin` | off | Reads the token from standard input, at most 4096 bytes. Refused when standard input is a terminal or another character device: pipe the token in instead of typing it. |
| `--token-command COMMAND` | | Runs `COMMAND` and reads the token from its standard output, for example `--token-command 'op read op://vault/wawarden/token'`. |

Give exactly one of `--token-file`, `--token-stdin` and `--token-command`, the
first and the last with a value that is not empty. The
token is never taken from a command-line argument or an environment variable,
and the commands read no `WAWARDEN_` variable, so they can run in the same
environment as `serve`. White space around the token is removed; what remains
must have the exact form `admin init` prints, or the command stops before it
sends anything. No error message repeats the file's content or the command's
output. A flag the command does not know, or one given wrongly, such as a value
for `--token-stdin`, is reported with a fixed message that names at most one of
the four flags, never with what was typed, followed by the usage text.

The token command is split into words without a shell: single and double
quotes group words, and a backslash outside single quotes escapes the next
character. For a pipeline, name a shell yourself (`sh -c '...'`); the
container image has none. The command runs with standard input and standard
error connected to nothing, so run it by hand to see why it fails; with every
`WAWARDEN_` variable removed from its environment; in its own process group,
which is killed once it has finished; for at most 30 seconds; and its output is
capped at 4096 bytes. A command that leaves a process holding its output open
fails after 2 seconds.

The HTTP client ignores the proxy variables, follows no redirect (a redirect
is reported as a refusal with its `3xx` status and exits `1`), uses TLS 1.2 or later for `https`, gives
up after 40 seconds, and reads at most 64 KiB of an answer. Everything it prints
from an answer, a refusal's code included, passes through a terminal sanitiser
that replaces every control, format, line-separator and paragraph-separator
character and every invalid byte with U+FFFD, so an answer cannot move the
cursor, rewrite the screen, set a link or write the clipboard.

A refusal prints `admin <command>: refused with <code> (HTTP <status>)` on
standard error; see the [exit codes](#exit-codes).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | `serve` stopped cleanly after `SIGTERM` or `SIGINT`; `healthcheck` got `200`; `version` and `admin init` printed their output; `admin status`, `pair` or `reconnect` got `200` and printed the answer. |
| `1` | `serve` failed after its configuration was accepted: the [archive](#message-archive) or the [device store](#device-store) could not be opened or its lock was not acquired within five minutes, the device store could not be brought up to date, or a listener could not be opened (`startup_failed`), a listener failed while running (`listener_failed`), or shutdown ended with errors, for example when the grace period ran out. `healthcheck` failed for any reason, including extra arguments. `version` or `admin init` could not write to standard output. An admin command's request failed: no connection, no answer within 40 seconds, an answer over 64 KiB or not in the expected shape, a redirect, or any status other than those of codes `0`, `4` and `5`, such as `engine_unavailable` (`503`), `pair_failed` (`502`) or `internal_error` (`500`). |
| `2` | `serve` refused to start (`startup_refused`, see [Startup refusals](#startup-refusals)). Or a usage error: an unknown subcommand, or an unknown flag or extra argument given to `serve`, `version` or `admin`, an admin command without exactly one token source or with an empty `--token-file` or `--token-command`, or an invalid `--addr`; the usage text goes to standard error. |
| `3` | An admin command could not use its token: the file or standard input could not be read, held more than 4096 bytes or was a terminal; the token command could not start, failed, timed out, left a process holding its output or printed more than 4096 bytes; or what it read is not an admin token. Nothing was sent. |
| `4` | The admin listener refused the token: `401` (`unauthorized`), or `429` with `too_many_requests` (the failure budget is spent, see [Failed authentication](#failed-authentication)). |
| `5` | The service refused the operation in its current state: `409` (`already_paired`, `already_connected`, `not_paired`, `owner_phone_missing` or `owner_mismatch`) or `429` with `rate_limited`; see [Admin routes](#admin-routes). |

`healthcheck` never exits `2`, because container runtimes reserve that code in
health checks. A process stopped by a second signal during shutdown ends with
that signal, not with an exit code (see [Shutdown](#shutdown)).

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
| `WAWARDEN_OWNER_PHONE` | unset | The phone number of the WhatsApp account that WaWarden may link to, in E.164 form: `+`, then 7 to 15 digits, the first not `0`, and nothing else (no spaces or separators), for example `+15550100001`. Unset, the service starts but refuses to pair; see [Pairing](#pairing). | `owner_phone_invalid` |
| `WAWARDEN_HISTORY_MAX_BYTES` | `33554432` (32 MiB) | The largest history-sync blob, in bytes, before and after decompression: a number in decimal digits from `1` to `268435456` (256 MiB), with no sign, unit, separator or white space. The memory that history sync needs grows with it; see [History sync](#history-sync). | `history_max_bytes_invalid` |
| `WAWARDEN_UNSAFE_DEBUG` | unset | A window, in minutes, during which the protocol library's debug output is logged: a number in decimal digits from `1` to `60`. Unset, that output is discarded at every log level. See [Protocol library logs](#protocol-library-logs) before setting it. | `unsafe_debug_invalid` |
| `WAWARDEN_METRICS_EMF` | unset | `1` writes [metrics in embedded metric format](#embedded-metric-format) on standard output; `0` or unset writes none. | `metrics_emf_invalid` |
| `WAWARDEN_NOTIFY_URL` | unset | The [webhook](#webhook) that receives every notification event: an `https` URL with a host, optionally a port, a path and a query, without credentials, a fragment or an IPv6 zone. Treat it as a secret when its path or query holds one; the service never logs it. Unset, events go to standard output only. | `notify_url_invalid` |
| `WAWARDEN_NOTIFY_SECRET_FILE` | unset | A path to the webhook's signing secret, required with `WAWARDEN_NOTIFY_URL` and refused without it: a regular file (symbolic links are followed) with no write permission for its group and no permission at all for others (`0400`, `0440`, `0600` and `0640` pass; `0644` and `0444` do not), of at most 4096 bytes, holding at least 32 bytes apart from surrounding white space. Generate one with `openssl rand -hex 32`. | `notify_url_missing`, `notify_secret_missing`, `notify_secret_unreadable`, `notify_secret_permissions`, `notify_secret_invalid` |
| `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | unset | `1` lets the webhook reach loopback, private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`) and shared (`100.64.0.0/10`, which mesh VPNs use) addresses; `0` or unset refuses them. Only with `WAWARDEN_NOTIFY_URL`. | `notify_allow_private_invalid`, `notify_url_missing` |
| `WAWARDEN_BACKUP_AGE_RECIPIENT` | unset | The one [age](https://age-encryption.org) recipient that [backups](#backups) are encrypted to: an X25519 recipient (`age1` followed by 58 characters, as `age-keygen` prints it) or a post-quantum hybrid one (`age1pq1...`), with no white space, comment or second recipient. An SSH key, a plugin recipient or an age secret key is refused, and the refusal never repeats the value. Unset, no backup is ever taken and `backup_disabled` is logged at start. | `backup_recipient_invalid` |
| `WAWARDEN_DEV_FAKE_ENGINE` | unset | Development builds only; a release build refuses it with `dev_variable_in_release`. `1` runs the [fake engine](#fake-engine) in place of WhatsApp, `wrong_account` runs it pairing a number that is not the owner's, and `0` or unset runs the WhatsApp engine. | `dev_fake_engine_invalid` |

Rules that apply to all of them:

- A variable set to the empty string is set, and is validated like any other
  value: `WAWARDEN_LISTEN=` is refused, it does not mean the default.
- When the environment holds a name more than once, the first occurrence is used.
- Set at most one of `WAWARDEN_ADMIN_TOKEN_SHA256` and
  `WAWARDEN_ADMIN_TOKEN_SHA256_FILE`. With neither, the admin listener is not
  opened at all. The hash and the file are read once, at start; to rotate the
  admin token, replace the hash and restart.
- `WAWARDEN_ADMIN_TOKEN` is never accepted, with any value: the service takes the
  admin credential only as a hash (`plaintext_admin_token`).

`serve` also checks one variable outside the `WAWARDEN_` namespace:

| Variable | Accepted values | Reason code |
|---|---|---|
| `GOTRACEBACK` | unset, empty, `none` or `single` | `traceback_level_unsafe` |

Every other value is refused, numeric levels included, `0` and `1` as well. The
named levels `all`, `system`, `crash` and `wer` make a crash print the stack of
every goroutine. `serve` sets the level to `single` itself before doing anything
else, but the Go runtime does not let a program lower the level that this
variable sets, and combined with `single`, every other refused value (`0`, any
other number, or a name the runtime does not know) prints every goroutine too.

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

The checks run in this order:

| # | Reason code | Variable named | Refused when |
|---|---|---|---|
| 1 | `plaintext_admin_token` | `WAWARDEN_ADMIN_TOKEN` | `WAWARDEN_ADMIN_TOKEN` is set, to any value. |
| 2 | `dev_variable_in_release` | the first such name, in sorted order | A release build sees any `WAWARDEN_DEV_*` variable. |
| 3 | `unknown_variable` | the first such name, in sorted order | Any other `WAWARDEN_` name is not one of the [environment variables](#environment-variables). |
| 4 | `traceback_level_unsafe` | `GOTRACEBACK` | `GOTRACEBACK` is set to anything other than empty, `none` or `single`, a numeric level included. |
| 5 | `listen_address_invalid` | the listen variable | `WAWARDEN_LISTEN`, `WAWARDEN_ADMIN_LISTEN` or `WAWARDEN_HEALTH_LISTEN` (checked in that order) is not a valid [listen address](#listen-addresses). |
| 6 | `health_address_not_loopback` | `WAWARDEN_HEALTH_LISTEN` | The health address is not a loopback address. |
| 7 | `admin_hash_sources_conflict` | `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` | Both admin hash variables are set. |
| 8 | `admin_hash_file_unreadable` | `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` | The file cannot be opened or read, or is not a regular file (a directory, a device or a named pipe, for example). |
| 9 | `admin_hash_invalid` | the hash variable | The hash is not exactly 64 hexadecimal characters, or the file holds more than 4096 bytes. |
| 10 | `listen_address_shared` | the later listener | Two enabled listeners overlap. |
| 11 | `log_level_invalid` | `WAWARDEN_LOG_LEVEL` | The level is not `debug`, `info`, `warn` or `error`. |
| 12 | `storage_profile_invalid` | `WAWARDEN_STORAGE_PROFILE` | The profile is not `local` or `nfs`. |
| 13 | `min_free_bytes_invalid` | `WAWARDEN_MIN_FREE_BYTES` | The value is not a number of bytes in decimal digits that fits in 64 bits. |
| 14 | `owner_phone_invalid` | `WAWARDEN_OWNER_PHONE` | The value is set but is not an E.164 number: `+` and 7 to 15 digits, the first not `0`. |
| 15 | `history_max_bytes_invalid` | `WAWARDEN_HISTORY_MAX_BYTES` | The value is not a number of bytes in decimal digits from 1 to 268435456. |
| 16 | `unsafe_debug_invalid` | `WAWARDEN_UNSAFE_DEBUG` | The value is not a number of minutes in decimal digits from 1 to 60. |
| 17 | `metrics_emf_invalid` | `WAWARDEN_METRICS_EMF` | The value is not `0` or `1`. |
| 18 | `notify_url_invalid` | `WAWARDEN_NOTIFY_URL` | The value is not an `https` URL with a host, or it holds credentials, a fragment or an IPv6 zone. |
| 19 | `notify_allow_private_invalid` | `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | The value is not `0` or `1`. |
| 20 | `notify_url_missing` | `WAWARDEN_NOTIFY_SECRET_FILE`, else `WAWARDEN_NOTIFY_ALLOW_PRIVATE` | That variable is set without `WAWARDEN_NOTIFY_URL`. |
| 21 | `notify_secret_missing` | `WAWARDEN_NOTIFY_SECRET_FILE` | `WAWARDEN_NOTIFY_URL` is set without a secret file. |
| 22 | `notify_secret_unreadable` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file cannot be opened or read, or is not a regular file. |
| 23 | `notify_secret_permissions` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file grants write permission to its group or any permission to others. |
| 24 | `notify_secret_invalid` | `WAWARDEN_NOTIFY_SECRET_FILE` | The file holds more than 4096 bytes, or less than 32 bytes apart from surrounding white space. |
| 25 | `backup_recipient_invalid` | `WAWARDEN_BACKUP_AGE_RECIPIENT` | The value is not exactly one age X25519 or hybrid recipient: it is empty, holds white space, a comment, a second recipient or an age secret key, or fails age's parser. The value is never repeated. |
| 26 | `dev_fake_engine_invalid` | `WAWARDEN_DEV_FAKE_ENGINE` | A development build sees a value other than `0`, `1` or `wrong_account`. A release build refuses the name itself at check 2. |
| 27 | `running_as_root` | none | The real or the effective user ID is 0 and `--allow-root` was not given. |
| 28 | `data_dir_unusable` | `WAWARDEN_DATA_DIR` | The directory does not exist and cannot be created, or cannot be inspected; the path is empty. |
| 29 | `data_dir_not_directory` | `WAWARDEN_DATA_DIR` | The path is not a directory, or its last component is a symbolic link. |
| 30 | `data_dir_foreign_owner` | `WAWARDEN_DATA_DIR` | The directory is not owned by the process's effective user ID. |
| 31 | `data_dir_permissions` | `WAWARDEN_DATA_DIR` | The directory's mode is not exactly `0700`. |
| 32 | `history_dir_unusable` | none; the error names `history/` | `history` exists in the data directory but cannot be inspected. |
| 33 | `history_dir_not_directory` | none; the error names `history/` | `history` exists but is not a directory, or is a symbolic link. |
| 34 | `history_dir_foreign_owner` | none; the error names `history/` | `history` is not owned by the process's effective user ID. |
| 35 | `history_dir_permissions` | none; the error names `history/` | The mode of `history` is not exactly `0700`. |
| 36 | `backups_dir_unusable` | none; the error names `backups/` | `backups` exists in the data directory but cannot be inspected. |
| 37 | `backups_dir_not_directory` | none; the error names `backups/` | `backups` exists but is not a directory, or is a symbolic link. |
| 38 | `backups_dir_foreign_owner` | none; the error names `backups/` | `backups` is not owned by the process's effective user ID. |
| 39 | `backups_dir_permissions` | none; the error names `backups/` | The mode of `backups` is not exactly `0700`. |
| 40 | `keys_dir_unusable` | none; the error names `keys/` | `keys` in the data directory does not exist and cannot be created, or cannot be inspected. |
| 41 | `keys_dir_not_directory` | none; the error names `keys/` | `keys` is not a directory, or is a symbolic link. |
| 42 | `keys_dir_foreign_owner` | none; the error names `keys/` | `keys` is not owned by the process's effective user ID. |
| 43 | `keys_dir_permissions` | none; the error names `keys/` | The mode of `keys` is not exactly `0700`. |
| 44 | `master_key_unusable` | none; the error names `keys/master` | `keys/master` does not exist and cannot be created, or cannot be inspected, opened or read. |
| 45 | `master_key_not_regular` | none; the error names `keys/master` | `keys/master` is not a regular file: a symbolic link (even to a valid key), a directory or a named pipe, for example. |
| 46 | `master_key_foreign_owner` | none; the error names `keys/master` | `keys/master` is not owned by the process's effective user ID. |
| 47 | `master_key_permissions` | none; the error names `keys/master` | `keys/master` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 48 | `master_key_size` | none; the error names `keys/master` | `keys/master` does not hold exactly 32 bytes. |
| 49 | `storage_filesystem_unknown` | none | The filesystem of the data directory cannot be inspected (`statfs`). |
| 50 | `storage_network_filesystem` | none | The storage profile is `local` and the data directory is on a network filesystem; see [Storage profiles](#storage-profiles). |
| 51 | `archive_db_unusable` | none; the error names `archive.db` | `archive.db` does not exist and cannot be created, or cannot be inspected. |
| 52 | `archive_db_not_regular` | none; the error names `archive.db` | `archive.db` is not a regular file: a symbolic link or a directory, for example. |
| 53 | `archive_db_foreign_owner` | none; the error names `archive.db` | `archive.db` is not owned by the process's effective user ID. |
| 54 | `archive_db_permissions` | none; the error names `archive.db` | `archive.db` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 55 | `archive_journal_unusable` | none; the error names `archive.db-journal` | `archive.db-journal` exists but cannot be inspected. |
| 56 | `archive_journal_not_regular` | none; the error names `archive.db-journal` | `archive.db-journal` exists but is not a regular file. |
| 57 | `archive_journal_foreign_owner` | none; the error names `archive.db-journal` | `archive.db-journal` is not owned by the process's effective user ID. |
| 58 | `archive_journal_permissions` | none; the error names `archive.db-journal` | `archive.db-journal` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 59 | `storage_ofd_unavailable` | none | On Linux, the storage profile is `local` and the kernel or the data directory's filesystem refused open-file-description locks; see [Storage profiles](#storage-profiles). |
| 60 | `archive_schema_newer` | none; the error names `archive.db` | The archive's schema version is newer than this build knows: a newer release wrote it. |
| 61 | `session_db_unusable` | none; the error names `session.db` | `session.db` does not exist and cannot be created, or cannot be inspected. |
| 62 | `session_db_not_regular` | none; the error names `session.db` | `session.db` is not a regular file: a symbolic link or a directory, for example. |
| 63 | `session_db_foreign_owner` | none; the error names `session.db` | `session.db` is not owned by the process's effective user ID. |
| 64 | `session_db_permissions` | none; the error names `session.db` | `session.db` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 65 | `session_journal_unusable` | none; the error names `session.db-journal` | `session.db-journal` exists but cannot be inspected. |
| 66 | `session_journal_not_regular` | none; the error names `session.db-journal` | `session.db-journal` exists but is not a regular file. |
| 67 | `session_journal_foreign_owner` | none; the error names `session.db-journal` | `session.db-journal` is not owned by the process's effective user ID. |
| 68 | `session_journal_permissions` | none; the error names `session.db-journal` | `session.db-journal` grants any access to group or others, or has the setuid, setgid or sticky bit. |

When the data directory is missing, it is created only after checks 1 to 27 pass,
so a start refused by checks 1 to 27 leaves nothing behind. `history/` and
`backups/` are checked only when they exist; `serve` creates neither at start (see
[Data directory](#data-directory)). `keys/` and the master key are created, when
missing, only after checks 28 to 39 pass, an empty `archive.db` only after
checks 28 to 50 pass, and an empty `session.db` only once the archive is open.
Checks 61 to 68 run on `session.db` after the archive is open, and not at all
when the [fake engine](#fake-engine) runs; the
[storage profile](#storage-profiles) is checked again for it, which passes once it
passed for the archive. A refusal by a later check (on a filesystem that forces
its own ownership or mode, for example), or a `startup_failed` exit, leaves what
was created in place: the data directory, `keys/`, the master key, `archive.db`
and `session.db`.

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

The checks run once, at start. When they exist, `history/` and `backups/` in the
data directory must also be directories, not symbolic links, owned by the
effective user ID with mode exactly `0700` (refusals 32 to 39). Then `serve`
creates the [master key](#master-key) when it is missing and opens the
[message archive](#message-archive), `archive.db`, and the
[device store](#device-store), `session.db`. The
[engine](#whatsapp-engine) creates `history/` with mode `0700` when it first
downloads a [history-sync blob](#history-sync), and keeps each blob there as
`history/<id>.bin`, mode `0600`, only until it is processed or quarantined.
A [backup](#backups) creates `backups/` and `backups/tmp/` with mode `0700` when
it is first taken, and `serve` removes `backups/tmp/` at every start once
`archive.db` is locked. The directory must be on a writable, persistent
filesystem that supports hard links and that only the service's user can read.

Mechanisms that add group permissions or the setgid bit to a volume, such as
Kubernetes `fsGroup`, make the directory fail the mode check.

### Master key

`keys/master` in the data directory holds the service's master key: exactly 32
bytes. Right after the data directory checks, `serve`:

1. creates `keys/` with mode `0700` when it does not exist;
2. checks `keys/` (refusals 40 to 43): a directory, not a symbolic link, owned by
   the effective user ID, with mode exactly `0700`;
3. when `keys/master` does not exist, writes 32 bytes from the operating system's
   cryptographic random source to a new temporary file `keys/.master-<random>`
   with mode `0600`, flushes it to disk, hard-links it as `keys/master`, removes
   the temporary name and flushes the directory. When two processes start at once,
   the second link fails, and both use the key that was linked first. An existing
   key is never replaced;
4. checks `keys/master` (refusals 44 to 48): a regular file, not a symbolic link,
   owned by the effective user ID, without any permission for group or others and
   without the setuid, setgid or sticky bit, holding exactly 32 bytes. Mode `0600`
   and mode `0400` are both accepted.

The service derives every key it uses from the master key with HKDF-SHA256 (no
salt, the information string `wawarden/v1/` followed by the purpose) and keeps
only the derived keys:

| Purpose | Length | Use in this build |
|---|---|---|
| `key-id` | 4 bytes | The key id: logged as 8 hexadecimal digits in the `keys_loaded` event, so that log lines can be grouped by the key their pseudonyms were made with. |
| `log-redact` | 32 bytes | The key of the [log pseudonyms](#pseudonyms-and-dropped-lines). |
| `chat-hmac` | 32 bytes | Derived but not used: no [notification event](#notifications) of this build names a chat. It is reserved for chat references in events and audit records. |

Apart from the log pseudonyms, the key id is the only value derived from the
master key that the service writes out. No key reaches a log, an event, a
metric or a response; the service writes the master key only to `keys/master`,
through the temporary name in step 3, and never writes a derived key. Replacing or
deleting the master key changes the key id and every log pseudonym; this build
has no command to rotate it. To provide your own key, write
32 random bytes to `keys/master` with mode `0600` or `0400`, owned by the service's
user, before the first start. A crash or power loss during the first start can
leave a `keys/.master-<random>` file behind, with mode `0600`. It holds either an
unused key or, after a crash right after the link, a second name of the current
master key, so delete it together with `keys/master` when you replace the key; it
can be deleted at any time while the service is stopped.

### Message archive

`archive.db` in the data directory is the message archive: a SQLite database
written by the SQLite engine compiled into the binary (`modernc.org/sqlite`).
`serve` creates it, applies its schema and records each start in it, and the
[engine](#whatsapp-engine) writes WhatsApp traffic into it once a device is
paired.
Right after the master key, `serve`:

1. checks the data directory's filesystem against the
   [storage profile](#storage-profiles) (refusals 49 and 50);
2. creates `archive.db` empty with mode `0600` when it does not exist, and
   refuses (51 to 54) one that is not a regular file, belongs to another user,
   or grants any access to group or others; then refuses (55 to 58) an
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
   is refused in turn (59) instead of falling back to classic locks;
5. brings the schema up to date (an archive written by a newer release is
   refused, 60), completes a rewrite of the full-text index that a stop or a
   failed rewrite left due (see below) and logs `archive_opened`.

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
10 seconds and every rewrite of its full-text index within 5 minutes. A call
that runs out of time is logged as `db_deadline` when its deadline passes, while
it still runs, with the profile of every goroutine of the process (function
names and source positions only), and is interrupted: SQLite stops its statement
between two steps, and the call fails. A call that waits on the filesystem, such
as a read that hangs on a network filesystem, or that is committing, is not
interrupted and runs until that wait or the commit ends; a write whose commit
ends succeeds. A deadline that passes just as a statement starts can be lost in
the database driver: that statement then runs to its end before the call fails.

Revoked, edited and expired message text is removed from the database file,
its journal and the full-text index, as described in the
[threat model](threat-model.md#security-invariants) (I-8); backups are
outside that guarantee, as are the same text, or any three-character run of it,
held elsewhere in the archive (by another message, a message waiting in the
inbox, a push name, a chat name or group subject, an identifier or the schema),
and so is a revocation or edit that arrives before
history sync has stored its target (see [Ingest](#ingest)). The index stores every three-character run of the text
and finds them through page keys, which copy the start of the first run on each
of its pages, and deleting a run keeps its key. When a deletion leaves a key that
is a whole run of the old text and no other message holds that run, the service
rewrites the entire index right after the deletion commits, in a transaction of
its own. The rewrite holds the archive while it runs, so other calls wait and
may reach their own deadlines; it writes the new index before it frees the old
one, so it needs free space of about the index's size. When the rewrite does
not commit, because the process stopped or the rewrite failed, the next start
rewrites the index before it logs `archive_opened`. A key that holds less than a
whole run is not rewritten.

### Device store

`session.db` in the data directory is the device store: the keys and state of the
linked device that the WhatsApp protocol library (`go.mau.fi/whatsmeow`) keeps,
in its own tables, through the same SQLite engine. Right after the archive,
`serve` opens it with every step the archive gets: the storage profile is
checked, the file is created empty with mode `0600` when it does not exist, the
file and its journal must pass refusals 61 to 68, the connection reads back the
same fixed settings and takes the same exclusive lock, kept the same way, with
`db_lock_wait` naming `session` and `db_lost` too. Then the protocol library
brings its tables up to date, within one minute. A store written by a newer
version of the library is used unchanged when it declares itself compatible
with the version this build links, and stops the start with `startup_failed`
when it does not. `serve` logs
`session_opened` with `paired` set to whether the store holds a linked device.
A development build running the [fake engine](#fake-engine) opens no device
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
off. The [engine](#whatsapp-engine) measures the free space when it starts and
every 30 seconds, also while it works through its inbox or history sync: it
checks whether a measurement is due before each inbox row, each history blob
and each batch of a blob. While ingest is paused it writes no message and no
history-sync notification and leaves them unacknowledged, which loses them (see
[Delivery](#delivery)), and applies nothing from its inbox or from history sync;
a blob that the pause
interrupts stays waiting without using an attempt and is applied from its start
once ingest resumes. Pausing reports the
[notification event](#notifications) `ingest_paused`, and resuming logs
`ingest_resumed`. With profile `nfs` the floor
never applies, because a network filesystem such as Amazon EFS grows on demand;
watch its own capacity metrics instead.

### Backups

With `WAWARDEN_BACKUP_AGE_RECIPIENT` set, the service writes encrypted backups of
the [archive](#message-archive) and the [device store](#device-store) to
`backups/` in the data directory. It holds only the
[age](https://age-encryption.org) recipient, a public key: it encrypts to it and
can never read a backup back. Without the variable no backup is ever taken, and
every start logs the warning `backup_disabled`. The [fake engine](#fake-engine)
of a development build opens no device store, so it takes no backup either and
logs the same warning even with the variable set.

**When.** This release takes exactly one backup automatically for each paired
device, once its initial [history sync](#history-sync) has settled, and nothing
else: no schedule, no retention, no admin route and no restore tool. Every 30
seconds the service checks whether the owner's device is paired. The first time
it sees it paired, it records that moment in the archive's `sync_state` table as
`backup_paired_at`, so a restart does not start the wait again; a device paired
before this release counts from the first start of this release. The backup is
taken once no history blob is waiting to be processed (a
[quarantined](#history-sync) blob does not count) and 10 minutes have passed
since the later of that moment and the arrival of the latest history blob. A
successful backup is recorded as `backup_taken_at`, and no further backup is
taken for that device. When a check finds the device no longer paired, or the
engine has reported an unpairing since the previous check (a device unpaired
and paired again between two checks), both keys are cleared, so the next paired
device is backed up once in turn, without a restart. A failed backup reports
`backup_failed` and is tried again only after the next start, or for the next
paired device.

**How.** Each database is copied online, in steps of 256 pages on its own
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

| `backups/` entry | Mode | Holds |
|---|---|---|
| `backups/` | `0700` | Created at the first backup; checked at start like the data directory when it exists. |
| `backups/tmp/` | `0700` | Only while a backup runs: one plaintext staging copy at a time and the encrypted file being written. |
| `backups/<time>.age` | `0600` | One finished backup: a binary (not armoured) age file. |

Decrypted, a backup is a tar archive of three files: `archive.db`, `session.db`
and `manifest.json`. The manifest holds `format` (`1`), the build `version`, the
`created` time in UTC and, for each database, its `name`, `schema_version` and
size in `bytes`; it holds no identifier. The two databases hold everything the
archive and the device store hold, the linked device's keys included.

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

**Decrypting and inspecting a backup offline.** Create the key pair on a machine
other than the service's host with the [age](https://github.com/FiloSottile/age)
tools, keep the identity file there, and set the public key it prints as the
recipient:

```sh
age-keygen -o backup-identity.txt     # prints "Public key: age1..."
```

Copy a backup off the host and, on that machine:

```sh
mkdir restore
age --decrypt -i backup-identity.txt 20261005T120000Z.age | tar -x -C restore
cat restore/manifest.json
sqlite3 -readonly restore/archive.db 'PRAGMA integrity_check'
sqlite3 -readonly restore/session.db 'PRAGMA integrity_check'
```

A truncated or altered file fails to decrypt. The decrypted files hold the
whole archive and the linked device's keys: keep them as private as the data
directory, and delete them when done.

**Restoring.** There is no restore tool yet. Restore only into a stopped
service: stop it, replace `archive.db` and `session.db` in the data directory
with the two files of one backup, mode `0600` and owned by the service's user,
remove any `archive.db-journal` and `session.db-journal`, then start it. Never
start two instances from one `session.db`, for example a restored copy while the
original still runs: both would act as the same linked device.

## WhatsApp engine

The engine links the service to WhatsApp as a companion device, keeps the
connection, and writes what arrives into the [message archive](#message-archive).
It talks to WhatsApp through an adapter over the WhatsApp protocol library,
`go.mau.fi/whatsmeow`, which keeps the linked device in the
[device store](#device-store).

Without a paired device the engine makes no connection of any kind, not even
the version fetch below: it stays `unpaired` and reports the
[notification event](#notifications) `unpaired` once, until
`wawarden admin pair` requests [pairing](#pairing). It reports `unpaired` again
whenever a paired device is lost, for example when WhatsApp logs it out or a
rejected device is logged out.

A disconnection never stops the process, and `/healthz` does not depend on the
engine: an unpaired or disconnected engine still answers `200`.

### Engine states

| State | Meaning |
|---|---|
| `unpaired` | No device is linked; the engine waits for pairing. |
| `connecting` | The engine is fetching the protocol version, connecting, or waiting before it reconnects. |
| `connected` | The engine is connected and receiving. |
| `disconnected` | The engine is not connected and does not reconnect on its own. The reason is one of `outdated`, `replaced`, `logged_out`, `temporary_ban`, `cat_refresh`, `connect_failure`, `restart_budget`, `owner_mismatch` or `shutdown`. |

Every change is logged as `engine_state`; entering `disconnected` for any reason
but `shutdown` also reports the notification event `disconnected`. The gauges
`wawarden_paired` and `wawarden_connected` follow the state.

- **Restart budget.** When the archive has recorded more than 5 starts in the
  last ten minutes, this one included (`recent_starts` in `archive_opened`), the
  engine starts in `disconnected` with reason `restart_budget` and makes no
  connection, so that a crash loop does not become a reconnect storm; a stored
  device that fails the [owner check](#pairing), which comes first, starts it in
  `owner_mismatch` instead. A start
  recorded at a later time than the current one, as after the system clock was
  set back, is forgotten, so it cannot hold the engine in `restart_budget`
  until the clock catches up.
- **Protocol version.** Otherwise, when a device is stored and it passes the
  [owner check](#pairing), the engine first fetches the current WhatsApp Web
  version from `https://web.whatsapp.com`, at most four times: once, then after 1, 2 and 4 seconds. It
  takes a newer version, keeps its own when the fetched one is equal, never takes
  an older one, and changes the version only while disconnected. When WhatsApp
  reports the client as outdated, the engine fetches again the same way and then
  needs a strictly newer version. When no fetch succeeds, it closes the
  protocol library's connection, so that nothing reconnects behind it, and
  stays in `disconnected` with reason `outdated`; an explicit reconnect fetches again
  before it connects, with the same need: a strictly newer version after
  WhatsApp reported the client as outdated, an equal one too after the fetches
  at the start failed, for example because the network was not up yet.
- **Reconnection.** After an ordinary drop or a failed connection attempt, the
  engine reconnects after a delay that starts at 2 seconds, doubles after each
  failed attempt up to 5 minutes, and is randomised to between half and all of
  that. A connection that stays up for at least a minute resets it, and so does
  the owner's pairing, so the reconnection that follows a pairing waits no
  longer than 2 seconds. A connection that
  drops within a minute counts as another failed attempt, so that a server that accepts
  connections and drops them at once is dialled ever more slowly, down to once
  every 2.5 to 5 minutes. Only the connection being opened
  counts: a late report that the dropped connection came up changes nothing, and
  a drop reported while a connection is being opened is followed by another
  attempt. An explicit reconnect while the engine is `connecting` closes the
  connection being opened and opens a new one without waiting. A version fetch
  or a connection attempt that panics fails like any other. The protocol
  library's own reconnection is switched off, so these delays are the only
  ones: when WhatsApp asks a device to log in again, as it does right after
  pairing, the adapter closes the connection instead of letting the library
  re-dial, and that counts as a drop. A connection whose keepalives have failed
  for 3 minutes is closed and counts as a drop too.
- **Stops that wait for the operator.** When another client replaces the session
  (`replaced`), WhatsApp bans the account temporarily (`temporary_ban`), a
  connection token cannot be refreshed (`cat_refresh`) or WhatsApp refuses the
  connection (`connect_failure`), the engine disconnects and waits for an
  explicit reconnect (`wawarden admin reconnect`). When WhatsApp logs the
  device out (`logged_out`), it waits for pairing (`wawarden admin pair`). A
  connection event that arrives late, after one of these, changes nothing.

### Pairing

Pairing links a new device by a code that the owner enters on the phone.
`wawarden admin pair` requests it through the [admin route](#admin-routes)
`POST /admin/v1/pair`. It is refused while a device is paired
(`already_paired`), while `WAWARDEN_OWNER_PHONE` is unset
(`owner_phone_missing`), and after three attempts within the last hour
(`rate_limited`); a refused or failed request counts as an attempt once it
passed the first two checks. The engine connects, waits up to 30 seconds for
WhatsApp to offer a login session, and asks for a code for the number in
`WAWARDEN_OWNER_PHONE` and no other, as a `Chrome (Linux)` device with a
notification on the phone. The route gives the engine 25 seconds in all, so
that its answer leaves before the listener's 30-second write timeout; a code
that does not arrive in time fails the request with `pair_failed`. WhatsApp
also sends login QR codes, which the adapter drops at once without keeping
them. The pairing code goes to the caller only: it is never logged, counted,
kept or put into an event. Before the protocol library saves anything, the
adapter refuses an account whose number is not exactly the one in
`WAWARDEN_OWNER_PHONE`: the library sends WhatsApp a pairing error and stores
nothing, and the engine reports `pair_rejected` with `stage` `before_save`; it
takes that report only while no device is stored, so one that arrives late
cannot unpair the owner's device. When pairing fails in the library for another
reason, such as an answer from WhatsApp that fails its checks, a device that
cannot be saved or a confirmation that cannot be sent, the library closes the
connection and the engine returns to `unpaired`; the next pairing request
connects again with a fresh device. When pairing completes, the linked account must have
the number in `WAWARDEN_OWNER_PHONE`; otherwise the engine reports
`pair_rejected` with `stage` `after_pairing`, stays
`unpaired` and logs the new device out. A logout that fails is reported as
`logout_failed` and tried again after the same delays as a
[reconnection](#engine-states), until it succeeds or the device is gone. Until
then the engine never connects the device, an explicit reconnect included, and
drops what its connection delivers before it is written (see
[Ingest](#ingest)); pairing stays refused while it is stored.

Before every connection the engine also checks that the stored device's number
is exactly the one in `WAWARDEN_OWNER_PHONE`: at the start before the restart
budget and before it fetches the protocol version, so that neither the budget
nor a failed fetch can hide a mismatch behind `restart_budget` or `outdated`; on
an explicit reconnect, also one from `outdated` or
`restart_budget`; and after a drop. The stored number is compared and never
logged. When it is not, as for a device whose logout failed before the process
stopped, or when `WAWARDEN_OWNER_PHONE` names another number than the one that
was paired, the engine stays `disconnected` with reason `owner_mismatch` and
reports `disconnected`. It refuses an explicit reconnect (`owner_mismatch`), drops what
a connection would deliver, and does not log the device out, because the cause
can be a mistake in the configuration as well as another account: correct
`WAWARDEN_OWNER_PHONE` or remove the stored device, then restart the service.
With `WAWARDEN_OWNER_PHONE` unset this check is skipped and a stored device is
connected as it is, because pairing could link it only while the number was
set. A report that pairing completed for the owner's number changes nothing
while the engine is `connected`.

A logout asks WhatsApp to remove the device, which needs a connection; when that
fails, the adapter closes the connection and deletes the device from
`session.db` itself. After a logout, or after WhatsApp logged the device out, the
next connection starts with a new, unpaired device. The protocol library keeps
the identity mappings and privacy tokens it learned in `session.db` after a
device is deleted.

### Ingest

Each message that WhatsApp delivers is first written to the archive's inbox, in
the same file as the archive, and only then acknowledged. Each group change is
written to the inbox too, but the protocol library acknowledges it as it
arrives, before the engine has written it (see [Delivery](#delivery)). The
engine refuses to write a message or a group change when the inbox already
holds 5,000 rows that wait to be applied, when ingest is
[paused](#free-space), or when the write fails; each case counts in
`wawarden_ingest_refused_total`, and a refused message or group change is lost
(see [Delivery](#delivery)). Traffic of a chat that is not a phone-number
user, a LID user or a group (status updates, broadcast lists, newsletters and
every other kind) is acknowledged and dropped before it reaches the inbox. So
is every message, group change and history-sync notification that arrives
while the engine is `unpaired`, while no device is stored, such as after
WhatsApp logged the device out, while a device that [pairing](#pairing)
rejected is still stored, or while the stored device is not the owner's
(`owner_mismatch`), all counted as `not_paired`, and every message whose
identifiers, push name, text and quoted text add up to more than 512 KiB
(`too_large`). Accepting and applying one message therefore allocates at most
48 MiB of Go memory, besides SQLite's own; only text made of control characters
comes near that, because the inbox writes each of them as six bytes, while
ordinary text needs about a seventh of it.

One worker applies the inbox in order. It records each attempt in the archive
before applying the row, then applies the row and removes it from the inbox in
one transaction, so that a row is applied once even when the process stops in
between. A failed attempt, an attempt that panics included, is retried after 1
and then 2 seconds; after three failed attempts, attempts cut short by a crash
included, the row is quarantined:
its content is removed, the row stays as a record, and the notification event
`quarantine` is reported with `queue` `inbox`. A row whose content the archive refuses,
such as a message identifier with spaces, is dropped at once.

The rules the worker applies, each counted in `wawarden_ingest_dropped_total`
when it drops an event:

| Rule | Drop reason |
|---|---|
| The chat must be a phone-number user, a LID user or a group. | `chat_rejected` |
| The sender must be a phone-number or LID user. | `sender_rejected` |
| An event of no known kind is dropped; in particular it never counts as a revocation. | `unknown_kind` |
| An edit, revocation, reaction or poll vote names its target by a key. A key without a message identifier, or a group key without the target's sender, is dropped. | `no_target` |
| A key, or the reference of a reply, that names a chat is followed only when it names the chat the event arrived in, or, in a direct chat, the owner's own number, which is how the other side names that chat. Any other chat is never followed: the event is dropped, and a reply is stored without its reference. | `foreign_reference` |
| The target is looked up only inside the event's chat, by its identifier and its sender; in a direct chat, a key that does not name the event's sender as the author names the other side. | `target_unknown`, and `owner_unknown` when the other side is the owner and `WAWARDEN_OWNER_PHONE` is unset |
| An edit or revocation must come from the target's sender. In a group, a revocation by someone else is applied only when the archive records that member as an admin of the group; a member it does not record is refused. | `not_original_sender`, `not_admin`, `admin_unknown` |
| An edit needs new text and a target that is neither revoked, a reaction nor a poll vote. | `invalid`, `target_kind` |
| An edit must be newer than the last edit applied to its target, so that an edit delivered late or again never brings back text the sender has since replaced. | `stale_edit` |

A revocation or edit whose target is not in the archive yet is dropped
(`target_unknown`) and not kept for later. When history sync stores the target
afterwards, as it can during the initial sync, because live traffic is applied
while blobs wait for their download, the target keeps its original text: a
revoked message its text, an edited one its earlier wording (see the
[threat model's residual risks](threat-model.md#residual-risks)).

Further, the worker:

- stores a reply's reference to the message it quotes when that message is
  found in the same chat, and marks the quote unverified when the quoted text
  differs from the stored original; the quoted text itself is never stored;
- stores reactions and poll votes as rows of their kind that point at their
  target, without their content;
- stores the push name of a sender other than the owner for that contact, and
  as the name of a direct chat, and a group's subject as its name, each with its
  source;
- stores `text_display`, the text without control, bidirectional, zero-width and
  tag characters, beside the text;
- stores whether a message came live or from history sync, and whether the
  owner sent it;
- sets the expiry of a message sent with a disappearing timer; every minute,
  also between the rows and batches it applies, the engine removes the text of
  expired messages from the archive as described under
  [Message archive](#message-archive);
- learns which LID belongs to which phone number only from the alternate
  identifiers WhatsApp's servers attach to live messages and from history sync,
  and re-keys a direct chat from the number to the LID. A mapping that
  contradicts one already learned, or that would merge two chats that both hold
  messages, is refused and counted in `wawarden_rekey_conflicts_total` every
  time, and reported as the [notification event](#notifications)
  `rekey_conflict` the first time the engine refuses it since the service
  started; the engine remembers up to 1,024 refused mappings, and forgets them
  all when that many are reached.

### Delivery

**A message refused or interrupted before it is stored is lost, not
redelivered.** Once written to the inbox, a message is applied to the archive,
after a restart if need be, or [quarantined](#ingest) when applying it keeps
failing; before that, WaWarden gets one chance at it.

The protocol library decrypts a message, which advances and saves its session
keys in `session.db`, and then hands it to the engine. Only once the engine has
written the message to the inbox does the library acknowledge it to WhatsApp and
send the delivery receipt. This gives:

- A message the engine wrote reaches the archive even when the process stops
  before applying it: the inbox is applied again at the next start.
- A message the engine refuses (a full inbox, a pause or a failed write), or
  whose handling panics in the adapter or the engine, is not acknowledged and gets no delivery receipt. WhatsApp sends it again later, but
  the library can no longer decrypt that copy, because its keys
  have moved on: it drops it without passing it on, then acknowledges it and
  sends the delivery receipt. A refused message is therefore lost to the archive.
  So is a history-sync notification refused during a pause, whose blob is then
  never downloaded.
- A message is lost the same way when the process stops after the library
  decrypted it and before the engine wrote it.
- The library acknowledges a group change as it arrives, whatever the engine
  answers, so a refused group change is lost as well.
- The library can keep each decrypted message in `session.db` until the engine
  confirms it, which would let a refused or interrupted message be decrypted
  again. WaWarden does not turn that on, so that `session.db` holds no message
  plaintext, and an architecture test refuses the call that would.

What a loss costs, and what limits it:

- Only the archive misses the message: the owner's phone and the account's
  other linked devices receive their own copies.
- The inbox is written before the acknowledgement, in the same file as the
  archive, so a crash or a stop after that write does not lose the message.
- The [free-space floor](#free-space) pauses ingest before a full disk makes
  every write fail, and reports `ingest_paused` so that the operator can free
  space while little is lost; ingest resumes on its own.
- Every refusal counts in `wawarden_ingest_refused_total` by reason
  (`backlog_full`, `paused`, `store_error`), and `admin status` shows the
  inbox backlog that leads to `backlog_full` at 5,000 rows, so a loss can be
  alerted on.

### History sync

The owner's phone sends the chat history in blobs, each announced by a
notification. The engine accepts a notification only from the owner's own
primary device (device 0) and drops any other (`history_not_primary`). It
records the notification in the archive and acknowledges it. Then, while
connected, it downloads the blob into `history/<id>.part` in the data directory
through a writer that stops at `WAWARDEN_HISTORY_MAX_BYTES`, refusing a blob
announced as larger without downloading it, flushes the file to disk, renames it
to `history/<id>.bin`, flushes the directory and only then sends WhatsApp the
history receipt. A small first blob can arrive inside the notification; it is
kept in the archive's record, needs no download, is refused without being
decompressed when it is larger than `WAWARDEN_HISTORY_MAX_BYTES`, and gets its
receipt when the engine takes it up. After a restart, the receipt of a blob that is still waiting
is sent again.

The download goes through the protocol library, which asks WhatsApp for the
media hosts, fetches the encrypted blob over an HTTP client of the adapter's,
with timeouts, no proxy, no redirect and a response cap of
`WAWARDEN_HISTORY_MAX_BYTES` plus 32 bytes for the encryption's padding and
tag, checks its hashes and decrypts it. The adapter holds it in memory, at most
`WAWARDEN_HISTORY_MAX_BYTES` plus 32 bytes, through a buffer that refuses every
write past that cap, before the engine writes it to `history/<id>.part`. The
history receipt goes out through the library only when the engine asks for it;
the library's own receipt on arrival is switched off.

For each blob the engine records an attempt, decompresses it, refusing more
than `WAWARDEN_HISTORY_MAX_BYTES` of output, decodes it, and applies it with
the rules above in transactions of 100 messages, with origin `history`; group
subjects and members, push names and LID mappings in the blob are stored too,
except where a live event has already recorded a newer value: history never
replaces a group subject, chat name or push name that a live event set, never
changes a member that a live group change named, and leaves the member list of
a group whose whole list arrived live as it is.
It then marks the blob processed, which also removes its record's download
reference and inline content, deletes its file and flushes the directory. Blobs
not yet processed are taken up again at the next start, and each start of the
engine first deletes every `history/<id>.bin` and `history/<id>.part` whose blob
is not waiting to be processed, such as a file that a crash left behind after
its blob was marked processed or quarantined. An attempt that panics while it
downloads, decodes or applies a blob fails like any other. After three failed attempts, attempts cut
short by a crash included, a blob is quarantined: its reference and
files are deleted and the [notification event](#notifications) `quarantine` is
reported with `queue` `history`. A graceful stop that interrupts a blob gives its attempt back, like
a [pause](#free-space): the blob stays waiting and is applied from its start at
the next start, keeping the batches it had already applied.

The engine processes one blob at a time. Reading a downloaded blob from
`history/` and decompressing it, which it does in two passes so that it can
size the output exactly, allocates at most 2 times `WAWARDEN_HISTORY_MAX_BYTES`
plus 1 MiB: about 65 MiB at the default and 513 MiB at the maximum of 256 MiB. A
blob that decompresses to more than the cap is refused after the first pass,
which allocates at most 1 MiB. Decoding the blob and holding its
conversations while they are applied come on top of that and grow with the
blob's content, so leave the process's memory limit well above this figure when
raising the cap. The adapter decodes a blob with a nesting limit of 28 levels:
24 for each message and its quotes, below the blob's own four. It reads each
conversation's rows under the conversation's own chat, never under the chat a
row's key names; it keeps an edit row's own identifier, so that it is applied as
an edit of its target; and it passes the blob's phone-number-to-LID mappings and
push names to the engine, which applies them by the rules above. A message that
the owner's phone sends again on request, which the protocol library builds the
same way, keeps an edit's own identifier too.

### Traffic the protocol library sends by itself

While connected, the protocol library sends WhatsApp, without being asked and
without a setting to stop it:

- an acknowledgement of every stanza it receives;
- a delivery receipt for every message it decrypts, in its inactive form, which
  a sender's phone does not show as delivered while the device has never
  announced itself as available, and WaWarden never does; receipts for the
  owner's own messages go to the owner's devices;
- retry receipts, at most 4 per message while the process runs, when a message
  cannot be decrypted, and a request to the owner's phone to send again a
  message that WhatsApp marks as unavailable;
- an announcement that the device is active, at every connection;
- uploads of new pre-keys when the server holds too few;
- fetches of the application state (contacts, chat settings) when the server or
  the owner's phone says it changed;
- session telemetry once, after pairing.

It never sends read receipts, presence, typing indicators or status updates on
its own, and WaWarden never asks it to: an architecture test refuses those
calls.

### Fake engine

A development build, one built with the `dev` build tag, replaces the protocol
adapter with a scripted fake when `WAWARDEN_DEV_FAKE_ENGINE` is `1` or
`wrong_account`, to try pairing, ingest, the admin commands, the events and the
metrics without a WhatsApp account. Everything around the fake is the real
engine: the same [states](#engine-states), pairing guards,
[ingest rules](#ingest), inbox, [history worker](#history-sync) and caps. The
fake opens no network connection and no [device store](#device-store), so it
takes no [backup](#backups) and logs `backup_disabled` even when
`WAWARDEN_BACKUP_AGE_RECIPIENT` is set. It starts its goroutines through the
same panic-recovering helper as the rest of the service, and makes `serve` log
the warning `fake_engine`, at every log level, with `wrong_account` set to
whether it pairs the wrong account.

- **Pairing.** The fake keeps its pairing in memory only, so every start begins
  `unpaired`, and pairing needs `WAWARDEN_OWNER_PHONE` as it does with WhatsApp.
  `wawarden admin pair` prints the fixed code `FAKE-C0DE`; a tenth of a second
  later the fake pairs the owner's number and connects. With `wrong_account` it
  pairs `+15550100029` instead, which the engine rejects with `pair_rejected`
  (stage `after_pairing`) and logs out, and nothing is ingested.
- **Live events.** Once connected, the fake delivers one event every 100
  milliseconds: a group with its subject and admins; direct and group texts,
  one from a sender addressed by LID; a quote that matches the quoted text, a
  forged one and one naming another chat; an edit by the sender and one by
  another member; the owner's revocation, a group admin's and a plain member's;
  a revocation naming another chat; a reaction; a poll and a vote; a
  disappearing message; a photo with a caption; a status broadcast and a
  newsletter post; a revocation without a target; and a member joining the
  group. After the first ten events it drops the connection once, and the
  engine reconnects with its usual backoff. An event the engine leaves
  unacknowledged is offered again at the next step, as WhatsApp would deliver it
  again.
- **History sync.** Among the live events, five history notifications: an
  inline bootstrap blob with a LID mapping and a contact's name, a blob to
  download, one from the owner's device 5 (dropped, as it is not from the
  primary device), a blob that inflates past `WAWARDEN_HISTORY_MAX_BYTES`, and
  a poison blob that the fake's decoder refuses. The last two fail three times
  and are quarantined, each with a `quarantine` event.

When the script has played, within about 15 seconds of pairing, the fake stays
connected and sends nothing more: `wawarden admin status` reports `connected`,
3 chats, 17 messages and 2 history blobs quarantined. Every identity the fake
uses is synthetic: its contacts are `+15550100021`, `+15550100022` and
`+15550100023`, and an owner number among them, or `+15550100029`, stops the
start with `startup_failed`.

Release builds do not contain the fake: its package compiles only with the
`dev` tag; the dev-only-fake-engine architecture rule refuses any file of the
package, and any file that imports it, that the go tool could build without
that tag; a release build refuses `WAWARDEN_DEV_FAKE_ENGINE`, like every
`WAWARDEN_DEV_*` name, with `dev_variable_in_release`; and the reproducible
build fails when a release binary holds the fake's marker string or any symbol
of its package.

## Listeners

The service opens these listeners and no other socket. All three speak plain HTTP
without TLS. WaWarden does not restrict who can connect: network access control
and TLS are the deployer's responsibility, and none of the listeners may face the
public internet.

| Listener | Address | Opened | Serves |
|---|---|---|---|
| `client` | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | Always | The client API. This build has no client routes and no client tokens, which come with M2: every request is refused. |
| `admin` | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082` | Only when an admin token hash is configured | `GET /metrics` and the [admin routes](#admin-routes) `GET /admin/v1/status`, `POST /admin/v1/pair` and `POST /admin/v1/reconnect`, all with the admin token. |
| `health` | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`, loopback only | Always | `GET /healthz`, without authentication. |

Every start logs a `listening` event for each listener with its bound address,
and a `listener_not_loopback` warning for each client or admin listener bound to
an address that is not loopback. The warning is written whatever
`WAWARDEN_LOG_LEVEL` is set to; see [Logging](#logging).

Each listener's HTTP server has these limits: 5 seconds to read the request
headers, 15 seconds to read the whole request, 30 seconds to write the response,
60 seconds of idle time on a kept-alive connection, and 16 KiB of request
headers.

Outbound, the service connects to WhatsApp, and only while a device is
paired or pairing was requested: the websocket at `web.whatsapp.com`, the
version fetch from the same host, and history downloads from the media hosts
WhatsApp names. Its only other outbound connections go to the
[webhook](#webhook), when `WAWARDEN_NOTIFY_URL` is set. Each connection to
WhatsApp is made directly: the proxy variables
(`HTTPS_PROXY`, `HTTP_PROXY`) are ignored, every step has a timeout (10 seconds to
connect and for the TLS handshake, 20 seconds for response headers; the
websocket's opening 30 seconds, a version fetch 20 seconds and a download 2
minutes in all), TLS is 1.2 or later, and redirects are refused, except to the
same host over HTTPS for the version fetch.

### Client and admin requests

Requests to the client and admin listeners pass these checks in order:

1. A request carrying an `Origin` or a `Sec-Fetch-Site` header is refused with
   `403` (`forbidden`), whatever else it carries. Browsers send these headers; no
   legitimate client of WaWarden is a web page.
2. An `OPTIONS` request, including `OPTIONS *`, is refused with `405`
   (`method_not_allowed`).
3. The request is authenticated. A failure is answered `401` (`unauthorized`)
   with `WWW-Authenticate: Bearer`, or `429` (`too_many_requests`, without that
   header) when the failure budget is spent; see
   [Failed authentication](#failed-authentication).
4. Only an authenticated request reaches routing: an unknown path is `404`
   (`not_found`) and a known path with another method is `405`
   (`method_not_allowed`) with an `Allow` header.

No request body is read before authentication succeeds. When a refused request
announces a body, the response carries `Connection: close` and the connection is
closed instead of being read.

The credential is taken from exactly one `Authorization` header of the form
`Bearer <token>`: the scheme in any letter case, one space, and a token without
spaces or tabs. A request with two `Authorization` headers is not authenticated.

**Client listener.** This build knows no client tokens, so every request that passes
checks 1 and 2 is answered `401` (or `429`):

```text
HTTP/1.1 401 Unauthorized
Cache-Control: no-store
Content-Length: 24
Content-Type: application/json; charset=utf-8
Www-Authenticate: Bearer
X-Content-Type-Options: nosniff
Date: <date>

{"error":"unauthorized"}
```

#### Admin authentication

The admin listener accepts a bearer token only when both hold:

- it has the exact form `wawarden admin init` generates: `wwadm_`, 43 characters
  of unpadded base64url encoding 32 bytes, `_`, and the matching CRC-32 in
  lower-case hexadecimal;
- its SHA-256 equals the configured hash, compared in constant time.

A string that is not in this form is refused even when its SHA-256 matches the
configured hash. Configuring the hash of anything other than a generated token
therefore leaves the admin listener unusable.

The admin listener serves `GET /metrics` (Go's router also answers `HEAD` on
it; see [Metrics](#metrics)) and the admin routes below.

#### Admin routes

Each route answers JSON with `Cache-Control: no-store`. Every call of a `POST`
route that passes authentication, whatever its outcome, reports the
[notification event](#notifications) `admin_mutation` with the route's action
and its outcome: `ok` or the error code it answered.

| Route | Request body | Answer |
|---|---|---|
| `GET /admin/v1/status` | none | `200` with the fixed object below. |
| `POST /admin/v1/pair` | exactly `{}`, see [Request bodies](#request-bodies) | `200` `{"code":"<pairing code>"}`; see [Pairing](#pairing). |
| `POST /admin/v1/reconnect` | exactly `{}` | `200` `{"status":"accepted"}`: the engine connects again at once, after an explicit reconnect as described under [Engine states](#engine-states). |

The status object carries no identifier, name or text:

```json
{"state":"connected","reason":"","paired":true,"counts":{"chats":12,"messages":3456,"history_blobs_pending":0,"history_blobs_quarantined":0,"inbox_backlog":0,"inbox_quarantined":0},"last_ingest_at":"2026-10-05T08:00:00Z","version":"v0.2.0"}
```

| Key | Content |
|---|---|
| `state`, `reason` | The [engine state](#engine-states); `reason` is empty unless the state is `disconnected`. |
| `paired` | Whether a device is stored. |
| `counts` | Chats and messages in the archive; history blobs waiting to be processed and quarantined; inbox rows waiting to be applied and quarantined. |
| `last_ingest_at` | When the engine last stored a message, in UTC to the second, or `null` before the first. |
| `version` | The build's version, as `wawarden version` prints it. |

The routes refuse with fixed codes:

| Answer | Route | When |
|---|---|---|
| `409` `{"error":"already_paired"}` | pair | A device is stored. |
| `409` `{"error":"owner_phone_missing"}` | pair | `WAWARDEN_OWNER_PHONE` is unset. |
| `429` `{"error":"rate_limited"}` | pair | Three pairing attempts were made in the last hour. |
| `502` `{"error":"pair_failed"}` | pair | Connecting or requesting the code failed, or no code arrived within 25 seconds. |
| `409` `{"error":"already_connected"}` | reconnect | The engine is connected. |
| `409` `{"error":"not_paired"}` | reconnect | No device is stored, or the stored device was rejected and is being logged out. |
| `409` `{"error":"owner_mismatch"}` | reconnect | The stored device's number is not `WAWARDEN_OWNER_PHONE`; see [Pairing](#pairing). |
| `503` `{"error":"engine_unavailable"}` | pair, reconnect | The engine has stopped because the service is shutting down. |
| `500` `{"error":"internal_error"}` | all three | Anything else, such as an archive read that failed; the cause is never sent. |

#### Failed authentication

The client listener and the admin listener each count failed authentications, in
`wawarden_auth_failures_total` and `wawarden_admin_auth_failures_total`; the
admin listener's also report the [notification event](#notifications)
`admin_auth_failure`, at most once a minute. Each
also has one failure budget shared by all callers: 30 failures, refilled at one
per second up to 30. While the budget is empty, failures are answered `429`
instead of `401`.

The budget changes only the answer to a failure. Authentication still runs for
every request, and a valid credential is accepted whether or not the budget is
empty, so legitimate callers cannot be locked out. For the same reason the budget
does not slow down guessing: what protects the admin token against guessing is its
256 random bits. Anyone who can reach a listener can keep its budget empty, after
which a caller presenting a wrong token sees `429` rather than `401`.

Failed authentications are counted, and those of the admin listener reported
in `admin_auth_failure` events, which carry a count and nothing about the
requests.

#### Request bodies

The admin routes `pair` and `reconnect` read their body, which must be the empty
object `{}`, and later client routes will read theirs, with one decoder. Only a
route handler can call it, so it runs only after authentication and after the
route's grant was decided. It answers with a fixed error and reads no further when:

| Answer | Refused when |
|---|---|
| `415` `{"error":"unsupported_media_type"}` | The request does not carry exactly one `Content-Type` header, or that header is not `application/json`, optionally with the single parameter `charset=utf-8` (media type and charset in any letter case). A body the request announces is not read, and the connection is closed. |
| `413` `{"error":"body_too_large"}` | The body is longer than 16384 bytes, as announced by `Content-Length` (the body is not read) or found while reading; the connection is closed. |
| `400` `{"error":"invalid_body"}` | The body is not valid UTF-8, starts with a byte-order mark, or is not exactly one JSON object with nothing but white space after it; it nests objects and arrays more than 8 levels deep, counting the outer object; an object holds a key that is not lower-case `snake_case` (a letter `a` to `z`, then letters, digits and `_`), or holds the same key twice once escapes are decoded (`"te\u0078t"` is `"text"`); a key is not a field of the route's request; or a value does not fit its field. |

### Health listener

| Request | Answer |
|---|---|
| `GET /healthz` while the service is serving and holds the [archive](#message-archive) and the [device store](#device-store) | `200` `{"status":"ok"}` |
| `GET /healthz` once shutdown has begun, or after the connection of the archive or the device store was lost (`db_lost`) | `503` `{"status":"unavailable"}` |
| Any other method on `/healthz`, including `HEAD` | `405` `{"error":"method_not_allowed"}` with `Allow: GET` |
| Any other path | `404` `{"error":"not_found"}` |

The health listener requires no credential, so its address must be loopback; the
service refuses any other (`health_address_not_loopback`). `200` means the
process is up, its listeners are serving, and the archive and the device store
are open and locked by the service; the state of the
[WhatsApp engine](#whatsapp-engine) does not change it. The health listener
opens only after both databases, so while `serve` waits for a lock, health
checks fail to connect.

### Responses

Every response that the service's handlers write, on every listener, carries
`Cache-Control: no-store` and `X-Content-Type-Options: nosniff`, and none carries
an `Access-Control-*` header. Their error bodies are fixed JSON objects with
`Content-Type: application/json; charset=utf-8`:

| Body | Status |
|---|---|
| `{"error":"forbidden"}` | `403` |
| `{"error":"unauthorized"}` | `401` |
| `{"error":"too_many_requests"}` | `429` |
| `{"error":"not_found"}` | `404` |
| `{"error":"method_not_allowed"}` | `405` |
| `{"error":"internal_error"}` | `500`, when a handler fails or panics |
| `{"error":"unsupported_media_type"}`, `{"error":"body_too_large"}`, `{"error":"invalid_body"}` | `415`, `413`, `400`, from a route that reads a body (`pair` and `reconnect`); see [Request bodies](#request-bodies) |
| `{"error":"already_paired"}`, `{"error":"already_connected"}`, `{"error":"not_paired"}`, `{"error":"owner_phone_missing"}`, `{"error":"owner_mismatch"}`, `{"error":"rate_limited"}`, `{"error":"pair_failed"}`, `{"error":"engine_unavailable"}` | `409`, `429`, `502` or `503`, from the [admin routes](#admin-routes) |

Responses never echo request content, header values or tokens.

Go's HTTP server answers some requests itself, before any handler runs: for
example a request without a `Host` header (`400`), with headers over the 16 KiB
limit (`431`), with an unsupported `Expect` header (`417`) or an unsupported
protocol version (`505`). Those answers have a plain-text or empty body and do
not carry the two headers above.

## Logging

The service writes JSON lines to standard output, one object per line. Every line
has these keys:

| Key | Content |
|---|---|
| `time` | RFC 3339 timestamp in UTC with fractional seconds |
| `level` | `DEBUG`, `INFO`, `WARN` or `ERROR` |
| `msg` | A human-readable sentence; it may change between releases |
| `event` | A stable, machine-readable event name; match on this, not on `msg` |

`WAWARDEN_LOG_LEVEL` sets the lowest level written, with exceptions that ignore
the setting. The `startup_refused` line is written before the configuration is
accepted. The `dev_build`, `fake_engine`, `listener_not_loopback` and
`backup_disabled` warnings are written at every level, `error` included, because
they are the only signal that a development binary is running, that it runs the
fake engine, that a listener is reachable beyond loopback, or that no backup
will ever be taken.
So are the [notification events](#notifications), which an operator must see,
the `notify_failed` and `notify_dropped` warnings, and the `unsafe_debug` and `unsafe_debug_ended`
notices of a [debug window](#protocol-library-logs). Lines in
[embedded metric format](#embedded-metric-format) have none of the four keys.

| Event | Level | Other keys | Written when |
|---|---|---|---|
| `startup_refused` | `ERROR` | `reason`, `error` | The configuration or the [master key](#master-key) is refused; exit `2`. |
| `keys_loaded` | `INFO` | `key_id` | The master key is loaded and the log pseudonyms are keyed with it; `key_id` is the 8-digit key id, never a key. |
| `db_lock_wait` | `WARN` | `database` (`archive` or `session`), `within` | Another process holds the lock of the archive or the device store; `serve` retries until `within` (`5m0s`) has passed. Logged once per database and start. |
| `archive_opened` | `INFO` | `schema_version`, `profile`, `ofd_locking`, `recent_starts` | The [archive](#message-archive) is open and locked. `ofd_locking` is `true` when open-file-description locks are in use, and `recent_starts` counts the starts of the last ten minutes, this one included, at most 64. |
| `session_opened` | `INFO` | `paired` | The [device store](#device-store) is open, locked and up to date; `paired` is `true` when it holds a linked device. |
| `fake_engine` | `WARN`, at every log level | `wrong_account` | A development build runs the [fake engine](#fake-engine) in place of WhatsApp and opens no device store; `wrong_account` is `true` when the fake pairs a number that is not the owner's. |
| `startup_failed` | `ERROR` | `error` | The archive or the device store cannot be opened or its lock was not acquired in time, the device store cannot be brought up to date, or a listener cannot be opened, for example because its address is in use; exit `1`. |
| `starting` | `INFO` | `version`, `revision`, `modified`, `dev` | The listeners are open. |
| `dev_build` | `WARN`, at every log level | | The binary was built with the `dev` tag. |
| `listening` | `INFO` | `listener`, `address` | Once per open listener. |
| `listener_not_loopback` | `WARN`, at every log level | `listener`, `address` | Once per client or admin listener bound to a non-loopback address. |
| `ready` | `INFO` | | The listeners are serving. |
| `listener_failed` | `ERROR` | `error` | A listener stopped serving on its own; shutdown follows. |
| `shutdown_started` | `INFO` | | Shutdown begins. |
| `stopped` | `INFO`, or `ERROR` with `error` | | Shutdown ended. |
| `panic` | `ERROR` | `name`, `panic_type`, `stack` | A panic was recovered in a handler or a goroutine; `name` is as in `wawarden_panics_total`. |
| `http_server_error` | `WARN` | `listener` | Go's HTTP server reported an error of its own, such as a failed accept; `msg` holds the server's text. |
| `db_deadline` | `ERROR` | `database`, `operation`, `timeout_ms`, `goroutines` | A read of the archive ran beyond 2 seconds, a write beyond 10 or a rewrite of its full-text index beyond 5 minutes. It is written when the deadline passes, while the call still runs, and the call is interrupted and fails, unless it is waiting on the filesystem or committing, or the deadline passed just as a statement started; see [Message archive](#message-archive). `operation` names the call in the code, and `goroutines` holds the goroutine profile of the process (function names and source positions, cut at 32 KiB). |
| `db_lost` | `ERROR` | `database` | The connection that held the lock of the archive or the device store is gone, and the service refuses to open another; `/healthz` answers `503` from then on. |
| `log_dropped` | `WARN` | `reason`: `xml` or `too_long` | Replaces a line that carried XML (`xml`) or was longer than 65,536 bytes (`too_long`); see [Pseudonyms and dropped lines](#pseudonyms-and-dropped-lines). It is written in place of a line that passed the log level, whatever that line's level was. |
| `unpaired` | `WARN`, at every log level | | A [notification event](#notifications). |
| `engine_state` | `INFO` | `state`, `reason` | The [engine's state](#engine-states) changed; `reason` is empty unless the state is `disconnected`. |
| `disconnected` | `WARN`, at every log level | `reason` | A [notification event](#notifications). |
| `version_updated` | `INFO` | `version` | The engine took a newer WhatsApp Web version, such as `2.3000.1027000000`. |
| `version_refresh_failed` | `WARN` | `attempt` | A version fetch failed or returned no usable version. |
| `connect_failed` | `WARN` | `attempt`, `error_type` | A connection attempt failed; the engine retries. `error_type` is the Go type of the error, never its text. |
| `pairing_started` | `INFO` | | A pairing attempt passed the refusals above. The code is never logged. |
| `pair_failed` | `WARN` | `error_type` | Connecting or requesting the pairing code failed. |
| `pair_rejected` | `WARN`, at every log level | `stage` | A [notification event](#notifications). |
| `logout_failed` | `WARN`, at every log level | `attempt`, `error_type` | A [notification event](#notifications). |
| `ingest_failed` | `WARN` | `queue`: `inbox` or `history`, `attempt` when an attempt failed, `error_type` | Reading, applying or quarantining an inbox row or a history blob failed. |
| `quarantine` | `WARN`, at every log level | `queue`, `attempts` | A [notification event](#notifications). |
| `rekey_conflict` | `WARN`, at every log level | `conflict` | A [notification event](#notifications). |
| `ingest_paused` | `WARN`, at every log level | `free_bytes`, `floor_bytes` | A [notification event](#notifications). |
| `backup_done` | `INFO`, at every log level | `bytes`, `archive_bytes`, `session_bytes`, `duration_ms` | A [notification event](#notifications). |
| `backup_failed` | `WARN`, at every log level | `reason` | A [notification event](#notifications). |
| `ingest_resumed` | `INFO` | `free_bytes`, `floor_bytes` | The free space reached 1.25 times the floor again. |
| `free_space_unknown` | `WARN` | `error_type` | The free space of the data directory could not be read. |
| `index_rewrite_pending` | `WARN` | `operation` | A change committed, but the full-text index rewrite it requires failed; it stays due and runs at the next start. |
| `history_ack_failed` | `WARN` | `error_type` | Sending the history receipt failed; the blob is processed anyway. |
| `history_file_kept` | `WARN` | `error_type` | A history file could not be deleted, or the directory could not be flushed after a deletion. The next start of the engine deletes a file whose blob is no longer waiting. |
| `expiry_sweep_failed` | `WARN` | `error_type` | Removing the text of expired messages failed; the sweep runs again a minute later. |
| `whatsmeow_log` | the line's own: `ERROR`, `WARN`, `INFO`, or `DEBUG` in a debug window | `module`, `detail` | A log line of the protocol library; see [Protocol library logs](#protocol-library-logs). |
| `unsafe_debug` | `WARN`, at every log level | `minutes`, `until` | `WAWARDEN_UNSAFE_DEBUG` opened a debug window at the start. |
| `unsafe_debug_ended` | `WARN`, at every log level | | The debug window has passed, and the protocol library's debug output is discarded again. Logged at the first debug line after the window. |
| `admin_mutation` | `INFO`, at every log level | `action`, `outcome` | A [notification event](#notifications). |
| `admin_auth_failure` | `WARN`, at every log level | `count` | A [notification event](#notifications). |
| `notify_failed` | `WARN`, at every log level | `attempts`, `reason`, `status` | The [webhook](#webhook) did not take an event, which is dropped. |
| `notify_dropped` | `WARN`, at every log level | `count` | At shutdown, the [webhook](#webhook) dropped `count` events it had not delivered, and `serve` exits `1`. |
| `emf_failed` | `WARN` | `error_type` | Writing the [embedded-metric-format](#embedded-metric-format) lines failed. |
| `backup_disabled` | `WARN`, at every log level | | `WAWARDEN_BACKUP_AGE_RECIPIENT` is not set, or the [fake engine](#fake-engine) runs and opens no device store, so no [backup](#backups) is ever taken. Logged once per start. |
| `backup_check_failed` | `WARN` | `error_type` | Reading or recording whether the [backup](#backups) is due failed; the check runs again 30 seconds later. |

What is never logged: requests (there is no access log), request bodies, header
values, tokens, failed authentications other than the count in
`admin_auth_failure`, the admin token's hash, the webhook's URL and secret, the values of
refused variables (a refusal names the variable only), panic values (only their
Go type and the stack), the master key and the keys derived from it (only the key
id), pairing codes and pairing QR codes, message text and push names (the engine logs states, reasons,
counts and the Go types of errors, never their text; the protocol library's lines are
treated as [described below](#protocol-library-logs)), WhatsApp identifiers in their `user@server` form when no ASCII letter or
digit follows the server name (they become
[pseudonyms](#pseudonyms-and-dropped-lines)) and lines carrying XML in one of
the [recognised shapes](#pseudonyms-and-dropped-lines). The listen addresses are the only configuration values that
are logged: the `listening` and `listener_not_loopback` events carry the bound
address, and the `error` texts of `startup_failed` and `listener_failed` come
from the operating system and can contain a listen address.

The CLI writes to standard error only its usage text, the flag parser's one-line
error when `serve` gets an unknown flag or an invalid flag value (it repeats the flag as typed,
for example `flag provided but not defined: -nope`), `healthcheck` failures,
the `admin init` reminder, and the fixed messages of the
[admin commands](#admin-status-admin-pair-and-admin-reconnect): their warning
about plain HTTP, the flag they could not parse, named but never repeated as
typed, the reason a token could not be used, the reason a request
failed, a refusal's code and status, and the hint where to enter a pairing
code. The Go runtime writes crash output to standard error.

### Protocol library logs

The protocol library logs through an adapter into the same writer as every other
line, as `whatsmeow_log` events: `module` names the library's component, such as
`whatsmeow/Client/Socket`, and `detail` holds its formatted text after three
changes. Every run of six or more digits, with `.` and `:` between them, that is
not directly followed by `@` becomes `[number]`, so that a phone number or a
user-and-device address outside the `user@server` form does not pass; a run
followed by `@` is an identifier, which the writer turns into a pseudonym. Then
text beyond 1 KiB is cut at the last white space before that limit and ends in
` [truncated]`, so that an error quoting a server's response stays short and no
identifier is cut in half; a text without white space in its first 1 KiB becomes
`[truncated]`. Finally the writer pseudonymises identifiers and drops lines
carrying XML, such as the library's dumps of protocol stanzas, as for any other
line. Its error, warning and information lines follow `WAWARDEN_LOG_LEVEL`,
during a `WAWARDEN_UNSAFE_DEBUG` window too.

Its debug output is discarded at every log level unless `WAWARDEN_UNSAFE_DEBUG`
opens a window, in minutes, at the start. During the window the library's debug
lines are written at level `DEBUG`, whatever `WAWARDEN_LOG_LEVEL` is set to; the
start logs `unsafe_debug` with the window's length and end, and the first debug
line after the window logs `unsafe_debug_ended` instead and closes it for good.
The library's debug lines name contacts and groups, carry push names and dump
every stanza it sends and receives: the writer drops the stanza dumps and
pseudonymises identifiers, but a push name or other text it does not recognise
reaches the log. Open a window only to diagnose a fault, keep it short, and treat
the log of that period as holding chat data. The library's second logging
channel, a logger attached to a context, stays off: nothing in WaWarden attaches
one, and an architecture test refuses the calls that would.

The signal library that the protocol library uses for its encryption logs
through the same adapter, as `whatsmeow_log` events with `module` `libsignal`
and its source file and line at the start of `detail`. Its error and warning
lines follow `WAWARDEN_LOG_LEVEL`; its debug and information lines are
discarded at every log level, during a `WAWARDEN_UNSAFE_DEBUG` window too,
because they print key material. An error line can quote up to 64 bytes of a
malformed message as it arrived: the quoted bytes stay inside `detail`, and the
changes described above apply to them like any other text.

### Pseudonyms and dropped lines

Every line `serve` writes to standard output passes through one writer, which
changes it in the three ways below before it is written. The writer writes a line
only when its newline arrives, so a line written in several pieces is checked as
a whole, and a last line without a newline is never written. The loggers that
feed the writer also write values fail-closed, as the last paragraph describes.

**Identifiers become pseudonyms.** Every part of a line shaped like a WhatsApp
identifier is replaced by `jid:` and 8 lower-case hexadecimal digits. The shape is
a run of letters, digits, `.`, `_`, `:`, `+` and `-`, then `@`, then one of the
servers `s.whatsapp.net`, `c.us`, `lid`, `g.us`, `broadcast`, `newsletter`,
`hosted`, `hosted.lid`, `bot`, `msgr` or `interop` in any letter case, followed by
a character that is not an ASCII letter or digit, or by the end of the line. The
identifier is the longest ending of the run that starts with a digit and holds
only digits, `.`, `_`, `:` and `-`, or the whole run when it has no such ending;
what precedes it is kept, so `sender:15550100001@s.whatsapp.net` becomes
`sender:jid:` and 8 digits. A JSON escape sequence such as `\n` just before an
identifier is kept as well, so the line stays valid JSON. The 8 digits are the first 4 bytes of an
HMAC-SHA256, under the `log-redact` key derived from the [master key](#master-key),
of the user cut at its first `.` or `:` (which drops an agent and a device
number) and lower-cased, then `@`, then the server lower-cased with `c.us` written
as `s.whatsapp.net`. So `15550100001@s.whatsapp.net`,
`15550100001:4@s.whatsapp.net`, `15550100001.0:4@s.whatsapp.net` and
`15550100001@c.us` all become the same pseudonym, and they keep it as long as the
master key is the same.

- Before the master key is loaded, the writer writes `jid:unkeyed` instead; only a
  `startup_refused` line can be written that early.
- Text that only looks like an address on one of these server names, such as
  `someone@lid.example`, is replaced too. Go module paths such as
  `example.com/module@v1.2.3` are not.
- Identifiers written another way are not recognised: a phone number on its own,
  a user and device number without `@` and a server, or an identifier whose
  server name is followed directly by an ASCII letter or digit, such as
  `15550100001@g.us2`.
- Pseudonyms are 32 bits long, so two identifiers can share one; among some
  65,000 identifiers a shared pseudonym becomes likely.

**Lines carrying XML are dropped.** A line carries XML when it contains:

- a closing tag (`</x>`);
- a self-closing tag without attributes (`<x/>`);
- the start of a tag with an attribute: `<`, a name, white space (also written
  as the JSON escape `\n`, `\r` or `\t`), a name, `=` and a quote (`<x a="`),
  whatever follows it, so a quote inside the value or a missing end of the tag
  does not matter;
- a tag directly followed by another tag (`<x><y`), with nothing in between;
- `<!--`, `<![CDATA[` or `<?xml`.

White space is allowed before the `>` of a closing tag, before `/>` and around
`=`. Such a line is replaced by one fixed line, also when its quotes are written as
`\"` and its tags' `<` and `>` as the JSON escapes
`\u003c` and `\u003e`:

```json
{"time":"2026-10-04T08:09:24.446116295Z","level":"WARN","msg":"log line dropped","event":"log_dropped","reason":"xml"}
```

Text such as `<nil>`, `[<nil> <nil>]`, `<autogenerated>` or `a < b` does not count
as XML, and neither does a tag without attributes followed by text and nothing
else from the list, such as `<message> without attributes`.

**Overlong lines are dropped.** A line longer than 65,536 bytes, not counting its
newline, is replaced by the same fixed line with `"reason":"too_long"`, whether it
was written at once or in pieces.

**Values are written fail-closed.** In the events above every value is a
string, a number, a boolean or a time. Should any other value reach a log line, a
byte slice of any type, or a pointer to one, is written as its length
(`[32 bytes]`), even when it has a `String` or `Error` method, as a raw JSON value
does; an error or a value with a
`String` method as its text, which is then pseudonymised like the rest of the
line; and a protocol message or a URL, whatever its methods, and every other
value as its Go type in brackets, such as `[seal.Chat]` or `[*waE2E.Message]`,
never as its content.

## Notifications

The service reports what an operator must act on as notification events. Each
event is one JSON line on standard output, written through the same writer as
every other line and at every log level, with the `event` key and a fixed set of
other keys; when `WAWARDEN_NOTIFY_URL` is set, the same event is also posted to
the [webhook](#webhook).

| Event | Level | Other keys | Reported when |
|---|---|---|---|
| `unpaired` | `WARN` | | The engine starts without a paired device, so it makes no connection until pairing is requested, also when the [restart budget](#engine-states) holds the start, or a paired device is lost: WhatsApp logged it out, or a rejected device was logged out. |
| `disconnected` | `WARN` | `reason` | The engine entered `disconnected` for any reason but `shutdown`; `reason` as under [Engine states](#engine-states). |
| `pair_rejected` | `WARN` | `stage`: `before_save`, `after_pairing` | Pairing linked or tried to link an account other than the owner's; see [Pairing](#pairing). |
| `logout_failed` | `WARN` | `attempt`, `error_type` | Logging out a rejected device failed; the engine tries again. `error_type` is the Go type of the error. |
| `quarantine` | `WARN` | `queue`: `inbox`, `history`; `attempts` | An inbox row or a history blob was set aside after three failed attempts. |
| `rekey_conflict` | `WARN` | `conflict`: `mapping_contradicts`, `both_chats_have_messages`, `message_collision` | A LID mapping was refused; the event that carried it is still applied. Reported once per refused mapping, as described under [Ingest](#ingest). |
| `ingest_paused` | `WARN` | `free_bytes`, `floor_bytes` | The data directory fell below its [free-space floor](#free-space). |
| `admin_mutation` | `INFO` | `action`: `pair`, `reconnect`; `outcome` | A `POST` [admin route](#admin-routes) was called with the admin token; `outcome` is `ok` or the error code it answered, such as `already_paired` or `invalid_body`. |
| `backup_done` | `INFO` | `bytes`, `archive_bytes`, `session_bytes`, `duration_ms` | A [backup](#backups) was written: `bytes` is the size of the encrypted file, the others the sizes of the two database copies and the time it took. |
| `backup_failed` | `WARN` | `reason` | A [backup](#backups) failed and what it had written was removed; `reason` as in the table under [Backups](#backups). |
| `admin_auth_failure` | `WARN` | `count` | Authentication failed on the admin listener. The first failure is reported at once with `count` `1`; further failures within the next minute are held back, and reported together once the minute has passed, within ten seconds, or at shutdown. `count` is the number of failures since the previous `admin_auth_failure`. |

No event carries an identifier, a name, message text, a token, a key or a
pairing code. Every value is a number or a fixed code: a value that is not a
lower-case code of at most 64 characters (or, for `error_type`, a Go type name)
is written as `invalid`. For example:

```json
{"time":"2026-10-05T08:12:40.002931604Z","level":"WARN","msg":"the engine is disconnected from WhatsApp","event":"disconnected","reason":"replaced"}
{"time":"2026-10-05T08:15:02.518840121Z","level":"INFO","msg":"an admin route that changes the service's state was called","event":"admin_mutation","action":"reconnect","outcome":"ok"}
```

### Webhook

With `WAWARDEN_NOTIFY_URL` and `WAWARDEN_NOTIFY_SECRET_FILE` set, every
notification event is also sent as `POST` to that URL, with this body, the
line's event and other keys plus an `id` and the `time`:

```json
{"id":"evt_5f0c2a9de1b34c7a8e6f1d2c3b4a5968","event":"disconnected","time":"2026-10-05T08:12:40.002931604Z","reason":"replaced"}
```

and these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `wawarden` |
| `WaWarden-Event-Id` | The event's id, `evt_` and 32 hexadecimal digits, the same on every attempt. |
| `WaWarden-Timestamp` | The time of this attempt, in Unix seconds. |
| `WaWarden-Signature` | `v1=` and the lower-case hexadecimal HMAC-SHA256, keyed with the secret, of the event id, `.`, the timestamp, `.` and the body's bytes. Each attempt is signed again. |

The secret is the content of `WAWARDEN_NOTIFY_SECRET_FILE` without surrounding
white space. It is read once, at start, and never logged.

**Verifying a delivery.** A receiver should, before it acts on a request:

1. read the body as raw bytes, before parsing it;
2. refuse the request when `WaWarden-Timestamp` is more than 5 minutes away from
   its own clock;
3. compute `v1=` and the hexadecimal HMAC-SHA256 of
   `<WaWarden-Event-Id>.<WaWarden-Timestamp>.<body>` with the shared secret, and
   compare it, in constant time, with each comma-separated value of
   `WaWarden-Signature`, accepting the request when one matches;
4. answer `2xx`, and ignore an event id it has already handled: a retry, or a
   delivery whose answer was lost, carries the same id.

For example, in Python:

```python
import hashlib, hmac, time

def verified(secret: bytes, headers, body: bytes) -> bool:
    event_id, timestamp = headers["WaWarden-Event-Id"], headers["WaWarden-Timestamp"]
    if abs(time.time() - int(timestamp)) > 300:
        return False
    signed = f"{event_id}.{timestamp}.".encode() + body
    expected = "v1=" + hmac.new(secret, signed, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(expected, v.strip()) for v in headers["WaWarden-Signature"].split(","))
```

**Delivery.** Events wait in a queue of 64 for one sender, so that a slow or
failing receiver never holds up the engine, the admin routes or shutdown; an
event that finds the queue full is dropped. Each attempt:

- connects directly, ignoring the proxy variables, with TLS 1.2 or later, and
  follows no redirect: a `3xx` answer is a failure;
- has 5 seconds to connect and for the TLS handshake, 10 seconds for the
  answer's headers and 15 seconds in all; at most 4096 bytes of the answer are
  read, and none of it is logged;
- succeeds on any `2xx`. A network error, `408`, `425`, `429` or a `5xx` is
  tried again, up to 5 attempts in all, after a delay that starts at about a
  second and doubles, randomised to between half and all of it, or after the
  answer's `Retry-After` in seconds, at most a minute. Any other answer is
  final.

Before it connects, the sender checks the address it resolved. It always
refuses link-local addresses (`169.254.0.0/16`, `fe80::/10`, which hold the
cloud metadata and container credential endpoints), the metadata addresses
`fd00:ec2::254`, `100.100.100.200` and `192.0.0.192`, multicast, broadcast,
reserved and unspecified addresses, every IPv6 address with a zone (such as
`fe80::1%eth0`), and the local-use NAT64 prefix `64:ff9b:1::/48`, whose
embedded IPv4 address depends on the network; it refuses loopback, private and shared
(`100.64.0.0/10`) addresses unless `WAWARDEN_NOTIFY_ALLOW_PRIVATE` is `1`. An
address in the well-known NAT64 prefix `64:ff9b::/96` is checked as the IPv4
address it embeds, so `64:ff9b::a9fe:a9fe` counts as `169.254.169.254`. A
refused destination is not tried again. The check runs on every connection,
after name resolution, so a name that resolves to a refused address is refused
too.

An event that was not delivered is counted in `wawarden_notify_dropped_total`,
with `reason` `queue_full`, `failed` (after its last attempt, or a final answer
or refused destination, logged as `notify_failed` with `attempts`, `reason`:
`destination_refused`, `http_status` or `network_error`, and `status`, `0`
without an answer) or `shutdown`. At shutdown the sender delivers what is
queued within the [grace period](#shutdown), then gives up on the rest: it
cancels the attempt in progress, logs `notify_dropped` with the number of
events dropped at shutdown, and `serve` exits `1`.

## Metrics

Metrics are served on the admin listener, at `GET /metrics`, with the admin
token. The format is the Prometheus text exposition format 0.0.4
(`Content-Type: text/plain; version=0.0.4; charset=utf-8`), sorted by metric name.
With `WAWARDEN_METRICS_EMF=1` the main ones are also written on standard output
in [embedded metric format](#embedded-metric-format), which needs no token.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wawarden_auth_failures_total` | counter | | Failed authentications on the client listener, including those answered `429`. |
| `wawarden_admin_auth_failures_total` | counter | | Failed authentications on the admin listener, including those answered `429`. |
| `wawarden_policy_denials_total` | counter | | Client requests that the client's grant did not allow. Always `0` in this build, which has no client tokens. |
| `wawarden_sends_rejected_total` | counter | | Sends refused by a scope, a budget or a rate limit. Always `0` in this build, which does not send. |
| `wawarden_panics_total` | counter | `name` | Panics recovered, by handler or goroutine name. Absent until the first panic. |
| `wawarden_notify_dropped_total` | counter | `reason` | Notification events the [webhook](#webhook) did not deliver: `queue_full`, `failed`, `shutdown`. |
| `wawarden_build_info` | gauge | `version`, `revision`, `dev` | Always `1`; the labels describe the running binary. |

The [WhatsApp engine](#whatsapp-engine) adds these:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wawarden_paired` | gauge | | `1` while a device is paired, `0` otherwise. |
| `wawarden_connected` | gauge | | `1` while the engine is `connected`, `0` otherwise. |
| `wawarden_messages_ingested_total` | counter | | Messages, reactions and poll votes stored in the archive, live and from history. |
| `wawarden_ingest_dropped_total` | counter | `reason` | Events dropped by an [ingest rule](#ingest) or a [history-sync](#history-sync) check: `chat_rejected`, `sender_rejected`, `invalid`, `unknown_kind`, `no_target`, `foreign_reference`, `target_unknown`, `target_kind`, `stale_edit`, `not_original_sender`, `not_admin`, `admin_unknown`, `owner_unknown`, `not_paired`, `too_large`, `history_not_primary`. |
| `wawarden_ingest_refused_total` | counter | `reason` | Events the engine refused to write and left unacknowledged, which loses them (see [Delivery](#delivery)): `backlog_full`, `paused`, `store_error`. |
| `wawarden_ingest_quarantined_total` | counter | `queue` | Inbox rows (`inbox`) and history blobs (`history`) quarantined. |
| `wawarden_rekey_conflicts_total` | counter | `conflict` | LID mappings refused, by conflict as in `rekey_conflict`. |

Labelled counters appear once they count their first event.

Panic names: `api.client`, `api.admin` and `api.health` for the handlers;
`listeners.client`, `listeners.admin`, `listeners.health`, `listeners.shutdown`
and `signals` for goroutines. The engine adds `engine.supervisor`,
`engine.ingest` and `engine.logout`; the first two also count a panic of one
version fetch or connection attempt (`engine.supervisor`) or of one inbox row's
or history blob's attempt (`engine.ingest`), which the engine treats as a failed
attempt. Its adapter adds `wa.connect`, which ends a connection attempt whose
caller gave up, `wa.keepalive`, which closes a connection whose keepalives
failed or that WhatsApp asked to log in again, and `wa.event` for a panic while
it translates one of the protocol library's events or the engine handles it,
which refuses the event. Other code on goroutines that the protocol library
starts itself is not covered: a panic there ends the process. The notifier adds
`notify.flush` and `notify.webhook`, the embedded-metric-format writer
`metrics.emf`, and the [backup](#backups) check `backup.initial`. In
development builds, the [fake engine](#fake-engine) adds `fake.connection`.

Anything that scrapes `/metrics` holds the full admin token, which can also
start pairing, ask for a reconnect and read the status, and from M2 create
clients. Treat a scrape configuration as holding the admin credential, and
prefer the embedded-metric-format lines, which need no token, for alerting.

### Embedded metric format

With `WAWARDEN_METRICS_EMF=1`, the service writes metrics in CloudWatch's
[embedded metric format](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format_Specification.html)
on standard output, through the same writer as every other line: every 60
seconds after the start, and once more at shutdown, after the engine has
stopped. Each time it writes one line in namespace `WaWarden` without
dimensions (`"Dimensions":[[]]`) that carries all eight metrics, zero when
nothing changed:

| Metric | Unit | Value | Source |
|---|---|---|---|
| `Paired` | `None` | The current `0` or `1` | `wawarden_paired` |
| `Connected` | `None` | The current `0` or `1`; the line at shutdown reports `0` | `wawarden_connected` |
| `MessagesIngested` | `Count` | Since the previous line | `wawarden_messages_ingested_total` |
| `PolicyDenials` | `Count` | Since the previous line | `wawarden_policy_denials_total` |
| `Panics` | `Count` | Since the previous line, all names together | `wawarden_panics_total` |
| `SendsRejected` | `Count` | Since the previous line | `wawarden_sends_rejected_total` |
| `AuthFailures` | `Count` | Since the previous line | `wawarden_auth_failures_total` |
| `AdminAuthFailures` | `Count` | Since the previous line | `wawarden_admin_auth_failures_total` |

```json
{"AdminAuthFailures":0,"AuthFailures":0,"Connected":1,"MessagesIngested":42,"Paired":1,"Panics":1,"PolicyDenials":0,"SendsRejected":0,"_aws":{"Timestamp":1791201600000,"CloudWatchMetrics":[{"Namespace":"WaWarden","Dimensions":[[]],"Metrics":[{"Name":"Paired","Unit":"None"},{"Name":"Connected","Unit":"None"},{"Name":"MessagesIngested","Unit":"Count"},{"Name":"PolicyDenials","Unit":"Count"},{"Name":"Panics","Unit":"Count"},{"Name":"SendsRejected","Unit":"Count"},{"Name":"AuthFailures","Unit":"Count"},{"Name":"AdminAuthFailures","Unit":"Count"}]}]}}
```

A metric that also has a label, in this build `Panics` by `name`, is
additionally written, for each label value whose count grew, in a line of its
own with that label as its only dimension. The dimensionless line holds the
total, and no line carries the metric twice, so an alarm without dimensions
matches the total:

```json
{"Panics":1,"_aws":{"Timestamp":1791201600000,"CloudWatchMetrics":[{"Namespace":"WaWarden","Dimensions":[["name"]],"Metrics":[{"Name":"Panics","Unit":"Count"}]}]},"name":"engine.ingest"}
```

These lines have no `time`, `level`, `msg` or `event` key. The first line counts
from the start of the process; a restart starts the counts again.

## Shutdown

`SIGTERM` or `SIGINT` starts a graceful shutdown:

1. `/healthz` starts answering `503`.
2. The client and admin listeners stop accepting connections, and requests in
   flight are allowed to finish.
3. The [WhatsApp engine](#whatsapp-engine) disconnects from WhatsApp and its
   workers stop after the row or batch in hand.
4. A [backup](#backups) still copying stops after the step or the read in
   hand, removes what it had written and reports `backup_failed` with reason
   `cancelled`; one already flushing its file to disk finishes first.
5. With `WAWARDEN_METRICS_EMF=1`, the last
   [embedded-metric-format](#embedded-metric-format) lines are written.
6. Held-back admin authentication failures are reported as
   `admin_auth_failure`, and the [webhook](#webhook) delivers what is queued;
   whatever remains when the grace period runs out is dropped, counted and
   reported in `notify_dropped`.
7. The [archive](#message-archive) and the [device store](#device-store) are
   closed, which releases their locks. Each waits up to 1 second for a call
   still in progress to give its connection back, then closes it.
8. The health listener stops.

One grace period of 10 seconds bounds the whole shutdown, apart from those waits
of up to 1 second for each database. When it runs out, the remaining
connections are closed and `serve` exits `1`; otherwise it exits `0`. A
database whose connection a call still held after its wait is reported in the
`stopped` event and makes `serve` exit `1` too.
Give the process more than 10 seconds between the stop signal and a forced kill;
Docker's default stop timeout is exactly 10 seconds.

A second `SIGTERM` or `SIGINT` received after shutdown has begun ends the process
at once.

## Container image

Release images are published as `ghcr.io/dortort/wawarden` for `linux/amd64` and
`linux/arm64`, by the release workflow only. Deploy them by digest;
[`RELEASING.md`](../RELEASING.md) explains how to verify one. The latest
release, `v0.1.0`, is milestone M0: its image has no WhatsApp engine and is
licensed under MIT. Until M1 is released, build `main` from source to run what
this document describes. The contract below applies to release images from M1
on:

| Item | Value |
|---|---|
| Base | `gcr.io/distroless/static-debian13:nonroot`, pinned by digest: no shell, no package manager |
| Binary | `/wawarden`, mode `0555` |
| Entrypoint and command | `ENTRYPOINT ["/wawarden"]`, `CMD ["serve"]`; pass another subcommand as the command, for example `admin init` |
| User | `65532:65532` |
| Writable path | `/data`, the only path the service writes and the only one that must be writable: an empty directory owned by `65532:65532` with mode `0700`; mount a volume there |
| Directories and modes | In `/data` the service creates `keys/`, `history/`, `backups/` and `backups/tmp/` with mode `0700`, and every file (`keys/master`, `archive.db`, `session.db` and their journals, history blobs, backups) with mode `0600`; every directory must keep mode `0700` and the service's owner, see [Data directory](#data-directory) |
| Root filesystem | Mount it read-only (`docker run --read-only`, or `readOnlyRootFilesystem: true` in Kubernetes) so that `/data` is also the only writable path; the service writes nothing outside the data directory |
| Listeners | Three, plain HTTP, by default client `127.0.0.1:8080`, admin `127.0.0.1:8082` (only with an admin token hash) and health `127.0.0.1:8081` (loopback only); see [Listeners](#listeners) |
| Network | Inbound: the three listeners only. Outbound: name resolution; WhatsApp over HTTPS and a WebSocket on port 443, only while a device is paired or pairing was requested; and the [webhook](#webhook)'s host and port when `WAWARDEN_NOTIFY_URL` is set. Nothing else, and never through a proxy |
| Health check | `["/wawarden","healthcheck"]`, in exec form; the image declares no health check of its own |
| Exposed ports | None declared |
| Labels | `org.opencontainers.image.source`, `licenses` (`GPL-3.0-or-later`), `version`, `revision` |
| Licence | WaWarden is `GPL-3.0-or-later`; the image links modules under GPL-3.0, MPL-2.0, Apache-2.0, ISC, MIT and BSD-3-Clause, listed in the [README](../README.md#licence) |
| Licence texts | `/licenses`: the licence files of the Go standard library (`std@<Go version>`), of WaWarden and of every other Go module linked into `/wawarden`, one directory per module and version; see [`RELEASING.md`](../RELEASING.md#licences) |

Notes:

- **Data volume.** An empty named Docker volume mounted on `/data` takes the
  image directory's owner and mode, so it passes the
  [data directory](#data-directory) checks as is. A volume on a network
  filesystem, such as Amazon EFS, needs `WAWARDEN_STORAGE_PROFILE=nfs`; see
  [Storage profiles](#storage-profiles). A bind-mounted host directory
  must be owned by `65532:65532` with mode `0700`, or `serve` refuses it
  (`data_dir_foreign_owner` or `data_dir_permissions`).
- **Health check form.** Use the exec form: `test: ["CMD", "/wawarden",
  "healthcheck"]` in Compose, an `exec` probe with
  `command: ["/wawarden", "healthcheck"]` in Kubernetes. `docker run --health-cmd`
  wraps the command in a shell, which this image does not have, so that check
  always fails. The check runs inside the container, where the health listener's
  loopback address is reachable.
- **Reaching the listeners.** Inside a container, a listener on `127.0.0.1` is
  reachable only from the same network namespace, for example from a sidecar that
  forwards to it. Port publishing does not reach it. To publish a port, set
  `WAWARDEN_LISTEN=0.0.0.0:8080` (and `WAWARDEN_ADMIN_LISTEN=0.0.0.0:8082` if
  needed); the service then logs `listener_not_loopback` at every start, and every
  host or container that can reach the container's address can reach the
  listener.
- **Running as root.** When the runtime overrides the user to root, `serve`
  refuses to start (`running_as_root`) unless it is given `--allow-root`. With
  `--allow-root`, the data directory must be owned by root with mode `0700`.
  The image's `/data`, and a named volume initialised from it, are owned by
  `65532:65532`, so `serve` refuses them with `data_dir_foreign_owner`.
