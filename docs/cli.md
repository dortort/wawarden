# Command-line reference

`wawarden` is a single binary. This page lists its subcommands with their flags
and output, the exit codes they share, what the commands write to standard
error, and how `serve` shuts down. The variables `serve` reads are in
[Environment variables](configuration.md#environment-variables), and the admin routes the
`admin` commands call are in [Admin routes](api.md#admin-routes). Terms such as LID,
key id and chain head are explained in the [glossary](glossary.md).

## Contents

- [Subcommands](#subcommands)
  - [Help and usage errors](#help-and-usage-errors)
  - [`serve`](#serve)
  - [`healthcheck`](#healthcheck)
  - [`version`](#version)
  - [`admin init`](#admin-init)
  - [`admin status`, `admin pair` and `admin reconnect`](#admin-status-admin-pair-and-admin-reconnect)
  - [`admin clients` and `admin chats`](#admin-clients-and-admin-chats)
  - [`admin backfill`](#admin-backfill)
  - [`audit verify`](#audit-verify)
  - [`backup verify`](#backup-verify)
  - [`keys rotate`](#keys-rotate)
- [Exit codes](#exit-codes)
- [Standard error](#standard-error)
- [Shutdown](#shutdown)

## Subcommands

```text
wawarden [serve] [--allow-root]  run the gateway (the default)
wawarden healthcheck             exit 0 only when the local /healthz answers 200
wawarden version                 print the version and build flavour
wawarden admin init              generate an admin token and its SHA-256
wawarden admin status|pair|reconnect ADMIN
wawarden admin clients create --name NAME (--read CHAT... | --all-chats) [--write CHAT...]
                              [--allow-first-contact] [--allow-media] [--expires-days DAYS]
                              ADMIN
wawarden admin clients list ADMIN
wawarden admin clients show|revoke --id ID ADMIN
wawarden admin chats list [--match TEXT] ADMIN
wawarden admin backfill --chat CHAT [--count N] ADMIN
                                 call the admin listener; ADMIN is [--addr URL] and one of
                                 --token-file PATH, --token-stdin or --token-command COMMAND
                                 (default --addr http://127.0.0.1:8082)
wawarden audit verify --db PATH --master-key-file PATH [--retired-keys-dir PATH] [--log PATH]
                                 verify the audit chain of a copy of the archive, offline
wawarden keys rotate --data-dir DIR
                                 retire the master key and create a new one, service stopped
wawarden backup verify --work-dir DIR [--extract-to NEW] -
                                 check a decrypted backup read from standard input, offline
```

### Help and usage errors

There is no `help` subcommand and no help flag. `wawarden help`,
`wawarden -h`, `wawarden --help`, and `-h` or `--help` given to any subcommand
are usage errors. Each prints the usage text above on standard error and exits
`2`. The one exception is `healthcheck -h`, which exits `1`, because
`healthcheck` never exits `2`. A first argument that starts with `-` is a flag
of `serve`, so `wawarden -h` is the same as `wawarden serve -h`.

### `serve`

Runs the service. It is the default: `wawarden`, `wawarden serve`,
`wawarden --allow-root` and `wawarden serve --allow-root` all run it. It reads
the [environment variables](configuration.md#environment-variables), applies every
[startup check](configuration.md#startup-refusals), opens the [listeners](api.md#listeners) and runs
until it receives `SIGTERM` or `SIGINT` (see [Shutdown](#shutdown)).

Flags:

| Flag | Default | Effect |
|---|---|---|
| `--allow-root` | off | Permits running with a real or effective user ID of 0. Without it, `serve` refuses to start as root (`running_as_root`). With it, the [data directory](configuration.md#data-directory) must still be owned by the effective user ID, so a root process needs one owned by root with mode `0700`. |

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

`<version>` is the version set at link time, as release binaries have it. A build
without one reports the main module's version when that is a release version,
`vMAJOR.MINOR.PATCH`: a `go install github.com/dortort/wawarden/cmd/wawarden@<version>`
build, or a `go build` from a clean checkout of a release tag, except for `v1.0.0`
and earlier, which print `dev`. Any other build reports `dev`, such as one from a
commit after a tag, from a modified tree, or with `-buildvcs=false`. `<commit>` is
the VCS revision recorded by `go build`, followed by `(modified)` when the working
tree was dirty, or `unknown` when the build recorded none. `dev build` is `true`
only for binaries built with the `dev` build tag.

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
[Admin authentication](api.md#admin-authentication).

The shell, not WaWarden, creates the file when you redirect this output; set a
restrictive `umask` or write it into a private directory.

The other `admin` subcommands of this build call the admin listener; see
below.

### `admin status`, `admin pair` and `admin reconnect`

These commands are HTTP clients of the [admin listener](api.md#admin-routes). Each
makes one request with the admin token and prints the answer:

| Command | Request | Prints on standard output |
|---|---|---|
| `admin status` | `GET /admin/v1/status` | One `key: value` line each for `state`, `reason` (`none` when empty), `paired`, `chats`, `messages`, `history blobs pending`, `history blobs quarantined`, `inbox backlog`, `inbox quarantined`, `backfills today`, `clients active`, `clients expired`, `clients revoked`, `all-chats clients active`, `last ingest` (`never` before the first message) and `version`. |
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
standard error, with any client or admin token in the answer's code redacted as
the [log writer](observability.md#pseudonyms-and-dropped-lines) does; see the
[exit codes](#exit-codes).

`admin status` also writes `admin status: warning: <code>` on standard error
for each code in the answer's `warnings`.

### `admin clients` and `admin chats`

These commands take the same `--addr` and token flags as `admin status` and use
the same HTTP client, sanitiser and refusal message. Each makes the request of
the [admin route](api.md#admin-routes) named here:

| Command | Request | Prints on standard output |
|---|---|---|
| `admin clients create --name NAME (--read CHAT... \| --all-chats) [--write CHAT...] [--allow-first-contact] [--allow-media] [--expires-days DAYS]` | `POST /admin/v1/clients` | The client's lines, then `token: ww_...` |
| `admin clients list` | `GET /admin/v1/clients` | For each client, its lines from `id` to `allow media`, then `read chats` and `write chats` with the number of each; a blank line separates two clients. |
| `admin clients show --id ID` | `GET /admin/v1/clients/ID` | The client's lines. |
| `admin clients revoke --id ID` | `POST /admin/v1/clients/ID/revoke` with the body `{}` | The revoked client's lines. |
| `admin chats list [--match TEXT]` | `GET /admin/v1/chats`, with `?match=TEXT` when `--match` is given | For each chat, `id`, `kind`, `ref` and `name` (`none` when the chat has none); a blank line separates two chats. |

A client's lines are `id`, `name`, `state`, `created`, `expires`, `revoked`
(`never` until it is revoked), `all chats`, `allow first contact`,
`allow media`, then one `read chat` line per read chat and one `write chat`
line per write chat, each with the chat's identifier, its kind, `seen` or
`not yet seen`, and its name when the archive knows one.

`--allow-media` asks for the client's
[`allow_media`](api.md#creating-a-client); the flag is stored and printed, but
grants nothing in this build. While an active client holds it and at least one
write chat, `admin status` warns `media_write_client`.

`--read` and `--write` can be repeated, and `--expires-days` defaults to 90.
Each chat value is one of:

- a chat identifier as the [admin routes](api.md#chat-identifiers-in-admin-requests)
  accept it, such as `15550100001@s.whatsapp.net`, `100000000000001@lid` or
  `120363000000000001@g.us`. A `@lid` identifier is a LID, the identifier
  WhatsApp gives an account in place of its phone number (see the
  [glossary](glossary.md#lid));
- a phone number in `+E.164` form, such as `+15550100001`, which becomes
  `15550100001@s.whatsapp.net`;
- a chat reference, the 32 lower-case hexadecimal digits that `admin chats
  list` prints under `ref`: the command first looks it up with `GET
  /admin/v1/chats?match=<reference>`, and stops with exit `5` when no chat has
  it.

A value that is none of these stops the command with exit `2` before the client
is created. The message names the flag and the value's position, such as
`--write value 2`, never the value. `--id` must be a client id, 8 characters of
`a` to `z` and `2` to `7`, or the command stops with exit `2` before it sends
anything.

`clients create` prints the client token once, on standard output, and nothing
else ever shows it again: standard error reminds you to hand it to the client
and store it now. Standard error also warns about each chat the archive has not
seen yet and, with `--all-chats`, that the client reads every chat, including
chats that appear later; the flag is the confirmation, there is no prompt.
`chats list` lists at most 100 chats, most recent first, and says on standard
error when more match.

### `admin backfill`

`admin backfill --chat CHAT [--count N]` takes the same `--addr` and token
flags as `admin status`, uses the same HTTP client, sanitiser and refusal
message, and calls `POST /admin/v1/backfill` (see [Admin routes](api.md#admin-routes))
to ask the owner's phone for the `N` messages of a chat that come before the
oldest one the archive holds. `--chat` takes the forms that `--read` takes
under [`admin clients`](#admin-clients-and-admin-chats): a chat identifier, a
`+E.164` number or a chat reference, which is looked up first. `--count` is
from 1 to 50, 50 by default. A missing `--chat`, a value of none of these forms
or a `--count` out of range stops the command with exit `2` before it sends
anything, and an unknown reference with exit `5`.

On success it prints `backfill requested: <request id>` on standard output,
and on standard error that the phone sends the messages when it processes the
request. They arrive as [history](internals.md#history-sync), whenever the phone answers;
`admin status` prints `backfills today`, the requests made since midnight UTC.
The quota, one request every 5 minutes and 20 a day, answers `backfill_quota`
and a chat the archive holds no message of `backfill_no_anchor`; both exit `5`.
`not_connected`, `send_rejected` and `send_outcome_unknown` exit `1`; see the
[exit codes](#exit-codes).

### `audit verify`

`wawarden audit verify` checks the [audit chain](observability.md#audit-chain) of a copy of the
archive. It runs offline: it needs neither the service nor the network, and it
never changes its inputs.

Three terms recur in its flags and output:

- A [chain head](glossary.md#chain-head) is the first 16 bytes of an audit row's HMAC, in
  hexadecimal. Each audit line on standard output carries its row's head as
  `chain_head`.
- A [shipped head](glossary.md#shipped-head) is a chain head found in the `--log` capture: a head
  that left the host in the service's standard output.
- A [key id](glossary.md#key-id) is the 4-byte value derived from a master key that names it,
  printed as 8 hexadecimal digits (see [Master key](configuration.md#master-key)).

| Flag | Effect |
|---|---|
| `--db PATH` | The copy of the archive to check, for example the `archive.db` of a decrypted [backup](configuration.md#backups). It is opened read-only, without a lock: never point it at the archive of a running service. |
| `--master-key-file PATH` | A copy of the master key the rows were written with: any regular file of exactly 32 bytes. Nothing is created when it is missing. |
| `--retired-keys-dir PATH` | Optional: a directory of retired master keys, as [`keys rotate`](#keys-rotate) leaves them in `keys/retired/`: files of exactly 32 bytes, each named by its key id; names starting with `.` are skipped. Without the flag, the directory `retired` beside the master key file is read when it exists. Its owner and modes are not checked; a missing directory given with the flag, a misnamed or malformed key, a key under another key id than its name or a copy of the master key is refused. |
| `--log PATH` | Optional: a capture of the service's standard output, one JSON object per line. Each line with a `chain_head` of 32 hexadecimal digits is one shipped head, under the key its `key_id` names when that is 8 hexadecimal digits; every other line is skipped. |

It prints:

```text
rows: 1204
verified: 1204
rows failing: 0
first failing row: none
problem: none
chain head: 3fa8c2d14be0917a5c6e2b8f0d4a7c19
pruned through: 5820
anchor head: 9d01c4e7a2b35f68e0c7d1a4b9e2f305
key ids: 5c7cdf84=310 6a53922c=894
shipped heads pruned: 5789
shipped heads: 6970
shipped heads missing: 0
first missing head: none
```

#### The audit anchor

When the [audit retention](observability.md#audit-retention) removed rows, the
walk starts from the audit anchor:

- `pruned through` is the id of the last row removed, and `anchor head` the
  first 16 bytes of its HMAC. Both are `none` while nothing was removed.
- The anchor's MAC is checked with the key whose id it names, and the first
  retained row must follow it.
- An anchor made under a retired key is refused when no row is left, or when
  the first retained row was written under the master key. A removal signs
  the anchor under the key the service runs with and never removes the newest
  row. Rows under the master key were written only after the retired key was
  replaced.

`key ids` counts the retained rows by the key id they were written under, in
order of key id.

#### Shipped heads

A shipped head that the copy lacks counts under one of two lines:

- `shipped heads pruned`, when the `ts` of its line is not after the anchor's
  time, the latest time of the rows removed: the retention removed it.
- `shipped heads missing`, otherwise. This includes a line with no readable
  `ts`. It also includes, when the anchor was made under a retired key, a line
  that names the master key's id or no valid `key_id`, since only an anchor
  under the master key covers rows written under it.

The anchor's own head counts as found.

#### Problems

`problem` names the first problem found:

| `problem` | The row or anchor fails because |
|---|---|
| `id_gap` | The row's id is not one more than the previous row's, or than the anchor's. |
| `unknown_key` | The row was written under a key id that is neither the master key's nor a retired key's. |
| `hmac_mismatch` | The row's HMAC is not the one its key gives. |
| `retired_key` | The row was written under a retired key after a row written under the master key, or after an anchor at such a row. The service never writes under a retired key again, and the retention signs an anchor only over rows that pass these checks. |
| `anchor_invalid` | The anchor names a key that is not given, its MAC is not the one that key gives, or it was made under a retired key while no row is left or the first retained row was written under the master key. The first failing row is then the anchor's. |

Without `--log`, the command warns on standard error that rows removed from
the end of the chain go unnoticed. A `--log` that holds no chain head, such as
a capture whose lines a log pipeline wrapped in an envelope, compares nothing:
the command prints the same warning and exits `6`. The log must be a regular
file; a pipe or a directory is refused without waiting for a writer.

### `backup verify`

`wawarden backup verify` checks a [backup](configuration.md#backups) that `age` has decrypted
and writes to its standard input; the binary itself holds nothing that
decrypts. It runs offline, needs neither the service nor the network, and is
meant for a machine apart from the service's host, where the age identity is
kept:

```sh
set -o pipefail
mkdir -m 700 verify
age --decrypt -i backup-identity.txt 20261005T030000Z.age |
  wawarden backup verify --work-dir verify --extract-to restore -
```

`set -o pipefail` makes the pipeline fail when `age` fails, for example with
the wrong identity or on a truncated or altered file; `age` then stops its
output, which `backup verify` sees as a stream that ended early. With
`--extract-to`, `restore/` then holds `archive.db` and `session.db`.

| Argument | Effect |
|---|---|
| `--work-dir DIR` | An existing directory owned by the user running the command with mode exactly `0700` (a symbolic link is refused). The command extracts the two databases into a new directory inside it and removes that directory before it exits, whatever the outcome. |
| `--extract-to NEW` | Optional: a path that does not exist yet, on the filesystem of `--work-dir`. When every check passes, the command creates it with mode `0700` and moves the two databases there as `archive.db` and `session.db`, mode `0600`, for a restore or for [`audit verify`](#audit-verify). |
| `-` | Required, last: read the decrypted backup from standard input. |

The stream must be exactly the tar archive a backup holds: `archive.db`,
`session.db` and `manifest.json`, in that order, each a regular file with no
directory in its name, a database no larger than the free space of the work
directory and the manifest at most 64 KiB, followed by nothing but the end of
the archive. The manifest must have `format` `1`, a `created` time, and name
both databases with their sizes. Then each database is opened read-only and
SQLite's integrity check runs on it, the full-text index included; a database
that passes has its schema version read and its rows counted. The archive's
schema version must equal the manifest's and be no newer than this build
migrates to; the device store's must equal the manifest's. It prints counts,
never an identifier, a name or a key:

```text
format: 1
written by: v1.0.0
created: 2026-10-05T03:00:00Z
archive integrity: ok
archive schema version: 4
archive schema: ok
archive chats: 212
archive messages: 48210
archive history blobs pending: 0
archive history blobs quarantined: 0
archive inbox backlog: 0
archive inbox quarantined: 0
archive active clients: 2
archive audit rows: 1204
device store integrity: ok
device store schema version: 16
device store schema: ok
device store paired: yes
device store pre-keys: 812
device store sessions: 96
verified: yes
```

A database that fails the integrity check prints `integrity: failed` and up to
five of SQLite's problem lines as `integrity problem`, and no counts; one that
passes but cannot be counted prints `counts: unreadable`. A schema version
that differs prints `schema: differs from the manifest's N`, one too new
`schema: newer than this build, which reads up to N`. `written by` is the
manifest's build version, or `unrecognised` when it does not look like one. The
counts use the same rules as [`admin status`](#admin-status-admin-pair-and-admin-reconnect),
so they can be compared with the live service, whose counts are never lower
than a backup's unless messages expired or were deleted since. Problems with
the stream are explained on standard error, with a fixed text.

### `keys rotate`

`wawarden keys rotate --data-dir DIR` replaces the [master key](configuration.md#master-key)
with a new one and keeps the old one as a retired key. `DIR` is the service's
data directory; there is no default. Run it as the service's user while the
service is stopped. It:

1. opens `archive.db` in `DIR` read-only, without waiting, and reads it once:
   when another process holds its lock, as a running service does, it refuses
   with `service_running` before changing anything. A data directory without
   `archive.db` passes;
2. checks `keys/` and `keys/master` as `serve` does (refusals 60 to 63 and 64
   to 68; it creates neither), creates `keys/retired/` with mode `0700` when it
   does not exist, and checks it and every key in it as `serve` does (refusals
   69 to 80). It refuses with `retired_keys_too_many` when `keys/retired/`
   already holds 32 keys besides the current one;
3. draws 32 bytes from the operating system's cryptographic random source and
   refuses with `rotate_key_collision`, before changing anything else, when
   their key id is the current key's or a retired key's;
4. writes the current key to a temporary file `keys/retired/.<key id>-<random>`
   with mode `0600`, flushes it, hard-links it as `keys/retired/<key id>`,
   removes the temporary name and flushes the directory. When that name exists,
   from an interrupted rotation, it must hold the same key, or the rotation is
   refused with `retired_key_conflict`;
5. writes the new key to a temporary file `keys/.master-<random>` with mode
   `0600`, flushes it, renames it over `keys/master` and flushes `keys/`.

It prints both key ids, never a key, and exits `0`:

```text
previous key id: 5c7cdf84
new key id: 6a53922c
```

A refusal is one line on standard error, `keys rotate: keys:` and a fixed
explanation that names neither the data directory nor a key, and exits `1`.
Step 1 does not keep the archive locked, so start the service only once the
command has returned. A rotation interrupted between steps 4 and 5 leaves the
current key in `keys/retired/` too, which `serve` refuses with
`retired_key_current`: run `keys rotate` again to finish it.

After a rotation, the next start loads the retired key (see
[Master key](configuration.md#master-key)) and logs `audit_key_changed` until the first audit
row under the new key. Log pseudonyms, the `chat_hmac` and `message_hmac` of new audit lines and
the text of a new message reference for the same message change; cursors and
references issued before still open while their key stays in
`keys/retired/`. Keep `keys/retired/`, and a copy of it with the copy of
`keys/master`: `audit verify` needs every key the retained rows and the anchor
name. Nothing deletes a retired key; delete one only when no cursor or
reference sealed under it is still wanted and no audit row of the archive or
of a backup you may verify carries its key id (`audit verify` lists them under
`key ids`).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | `serve` stopped cleanly after `SIGTERM` or `SIGINT`; `healthcheck` got `200`; `version` and `admin init` printed their output; an admin command got `200`, or `202` for `admin backfill`, and printed the answer; `audit verify` verified every row and found every shipped head; `keys rotate` replaced the master key and printed both key ids; `backup verify` read a complete backup and every check passed. |
| `1` | `serve` failed after its configuration was accepted: the [archive](configuration.md#message-archive) or the [device store](configuration.md#device-store) could not be opened or its lock was not acquired within five minutes, the device store could not be brought up to date, or a listener could not be opened (`startup_failed`), a listener failed while running (`listener_failed`), or shutdown ended with errors, for example when the grace period ran out. `healthcheck` failed for any reason, including extra arguments. `version` or `admin init` could not write to standard output. An admin command's request failed: no connection, no answer within 40 seconds, an answer over 64 KiB or not in the expected shape, a redirect, or any status other than those of codes `0`, `4` and `5`, such as `engine_unavailable` (`503`), `pair_failed` (`502`), `invalid_query` (`400`) or `internal_error` (`500`). `audit verify` could not open the copy, the master key file, the retired keys or the log. `keys rotate` was refused, because another process holds the archive's lock or a check of `keys/` failed, or could not write a key. `backup verify` was given a `--work-dir` that is not a private directory of its user or an `--extract-to` that exists, could not read standard input to the end, found it ended early (as after a failed or wrong-key decryption), could not write a database into the work directory or had too little space for it, or could not move the verified databases to `--extract-to`. |
| `2` | `serve` refused to start (`startup_refused`, see [Startup refusals](configuration.md#startup-refusals)). Or a usage error: an unknown subcommand, `help`, `-h` or `--help` (see [Help and usage errors](#help-and-usage-errors)), or an unknown flag or extra argument given to `serve`, `version`, `admin`, `audit`, `keys` or `backup`, an admin command without exactly one token source or with an empty `--token-file` or `--token-command`, an invalid `--addr`, a `clients create` without `--name` or with a chat value it cannot use, an `admin backfill` without `--chat`, with a chat value it cannot use or with a `--count` outside 1 to 50, a `--id` that is not a client id, an `audit verify` without `--db` or `--master-key-file`, a `keys rotate` without `--data-dir`, or a `backup verify` without `--work-dir` or without `-` as its only argument; the usage text goes to standard error. |
| `3` | An admin command could not use its token: the file or standard input could not be read, held more than 4096 bytes or was a terminal; the token command could not start, failed, timed out, left a process holding its output or printed more than 4096 bytes; or what it read is not an admin token. Nothing was sent. |
| `4` | The admin listener refused the token: `401` (`unauthorized`), or `429` with `too_many_requests` (the failure budget is spent, see [Failed authentication](api.md#failed-authentication)). |
| `5` | The service refused the operation: `409` (`already_paired`, `already_connected`, `not_paired`, `owner_phone_missing`, `owner_mismatch` or `name_taken`), `422` (a [client refusal](api.md#admin-routes), or `backfill_no_anchor`), `404` (`not_found`, such as an unknown client id) or `429` with `rate_limited` or `backfill_quota`; see [Admin routes](api.md#admin-routes). `clients create` and `admin backfill` also exit `5` when no chat has a reference they were given. |
| `6` | `audit verify` read everything but found a row or an audit anchor that does not verify or a shipped head that the copy lacks and the retention did not remove, or was given a `--log` that holds no chain head. `backup verify` read the stream but it is not a complete backup (a missing, extra, misnamed, oversized, sparse or non-regular member, an inconsistent manifest or data after the archive), or a database failed its integrity check, could not be counted, or has a schema version other than the manifest's or newer than this build reads. |

`healthcheck` never exits `2`, because container runtimes reserve that code in
health checks. A process stopped by a second signal during shutdown ends with
that signal, not with an exit code (see [Shutdown](#shutdown)).

## Standard error

The CLI writes to standard error only its usage text, `healthcheck` failures,
the `admin init` reminder, and fixed messages: the flag a command could not
parse, named but never repeated as typed (for example
`serve: --allow-root is not given as it must be`, or
`serve: an argument is not a flag of this command` for an unknown flag); the
messages of the [admin commands](#admin-status-admin-pair-and-admin-reconnect):
their warning about plain HTTP, the reason a token could not be used, the
reason a request failed, a refusal's code and status, and the hint where to
enter a pairing code; and the refusals and warnings of `audit verify`,
`keys rotate` and `backup verify`, which give the operating system's reason
when a file cannot be opened or read but never its path. The Go runtime writes
crash output to standard error.

## Shutdown

`SIGTERM` or `SIGINT` starts a graceful shutdown:

1. `/healthz` starts answering `503`.
2. The client and admin listeners stop accepting connections, and requests in
   flight are allowed to finish.
3. The [WhatsApp engine](internals.md#whatsapp-engine) disconnects from WhatsApp and its
   workers stop after the row or batch in hand.
4. A [backup](configuration.md#backups) still copying stops after the step or the read in
   hand, removes what it had written and reports `backup_failed` with reason
   `cancelled`, and is not retried; one already flushing its file to disk
   finishes first. A prune of old backups stops before its next file, and a
   prune of history blob rows after the batch in hand.
5. With `WAWARDEN_METRICS_EMF=1`, the last
   [EMF](observability.md#embedded-metric-format) lines are written.
6. Held-back admin authentication failures are reported as
   `admin_auth_failure`, and the [webhook](observability.md#webhook) delivers what is queued;
   whatever remains when the grace period runs out is dropped, counted and
   reported in `notify_dropped`.
7. The [archive](configuration.md#message-archive) and the [device store](configuration.md#device-store) are
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
