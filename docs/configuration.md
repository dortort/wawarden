# Configuration reference

This is the authoritative reference for configuring and running WaWarden. It
describes the current build on `main`: milestone **M0**, plus the parts of milestone
**M1** merged so far, which are the [master key](#master-key), the pseudonyms and
dropped lines in the [logs](#pseudonyms-and-dropped-lines), the
[request-body decoder](#request-bodies), which no route uses yet, and the
[message archive](#message-archive), which nothing writes WhatsApp traffic to yet. Everything listed
here is implemented, and nothing else is. Settings planned for later milestones are
listed under [Reserved names](#reserved-names) and are refused by this build.

## Unknown variables stop the service

> **`wawarden serve` refuses to start when the environment contains any variable
> whose name begins with `WAWARDEN_` and that this build does not implement.** A
> typo, a variable meant for a later release, or a leftover from another
> deployment stops the service with the reason code `unknown_variable` instead of
> being ignored. An empty value counts as set: `WAWARDEN_OWNER_PHONE=` is
> refused as well.

Only the names in [Environment variables](#environment-variables) are accepted.
Names are matched exactly, so a lower-case `wawarden_listen` is not recognised as a
WaWarden variable and has no effect.

### Reserved names

These names are reserved for later milestones. Do not set them before the
release that implements them: until then, each one makes `serve` refuse to start
with `unknown_variable`.

| Variable | Planned purpose | Becomes valid in |
|---|---|---|
| `WAWARDEN_OWNER_PHONE` | The account owner's number in E.164 form, checked when pairing | M1 |
| `WAWARDEN_BACKUP_AGE_RECIPIENT` | The age recipient that backups are encrypted to | M1 |
| `WAWARDEN_METRICS_EMF` | `1` writes metrics as embedded-metric-format lines on standard output | M1 |
| `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` | Per-client send rate limit | M3 |
| `WAWARDEN_SEND_GLOBAL_PER_HOUR` | Global send rate limit | M3 |

Further names are planned and not final yet: `WAWARDEN_UNSAFE_DEBUG` (M1), and
`UNSAFE_` overrides of the rate-limit hard caps (M2 for read and search limits, M3
for send limits). Any of them set as a `WAWARDEN_` variable is refused with
`unknown_variable` until the release that implements it.

`WAWARDEN_DEV_*` names are reserved for development builds; a release build
refuses them with `dev_variable_in_release`. The current development build
implements none of them either and refuses them with `unknown_variable`.

## Subcommands

```text
wawarden [serve] [--allow-root]  run the gateway (the default)
wawarden healthcheck             exit 0 only when the local /healthz answers 200
wawarden version                 print the version and build flavour
wawarden admin init              generate an admin token and its SHA-256
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

Other `admin` subcommands (`status`, `pair` and `reconnect` in M1, client
management in M2, `backfill` in M3) do not exist in this build and are usage
errors.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | `serve` stopped cleanly after `SIGTERM` or `SIGINT`; `healthcheck` got `200`; `version` and `admin init` printed their output. |
| `1` | `serve` failed after its configuration was accepted: the [archive](#message-archive) could not be opened or its lock was not acquired within five minutes, or a listener could not be opened (`startup_failed`), a listener failed while running (`listener_failed`), or shutdown ended with errors, for example when the grace period ran out. `healthcheck` failed for any reason, including extra arguments. `version` or `admin init` could not write to standard output. |
| `2` | `serve` refused to start (`startup_refused`, see [Startup refusals](#startup-refusals)). Or a usage error: an unknown subcommand, or an unknown flag or extra argument given to `serve`, `version` or `admin`; the usage text goes to standard error. |

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

`serve` validates its whole configuration, the [master key](#master-key) and the
[archive](#message-archive) before opening any socket. The first failed check stops it: it writes one log line with
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
| 14 | `running_as_root` | none | The real or the effective user ID is 0 and `--allow-root` was not given. |
| 15 | `data_dir_unusable` | `WAWARDEN_DATA_DIR` | The directory does not exist and cannot be created, or cannot be inspected; the path is empty. |
| 16 | `data_dir_not_directory` | `WAWARDEN_DATA_DIR` | The path is not a directory, or its last component is a symbolic link. |
| 17 | `data_dir_foreign_owner` | `WAWARDEN_DATA_DIR` | The directory is not owned by the process's effective user ID. |
| 18 | `data_dir_permissions` | `WAWARDEN_DATA_DIR` | The directory's mode is not exactly `0700`. |
| 19 | `history_dir_unusable` | none; the error names `history/` | `history` exists in the data directory but cannot be inspected. |
| 20 | `history_dir_not_directory` | none; the error names `history/` | `history` exists but is not a directory, or is a symbolic link. |
| 21 | `history_dir_foreign_owner` | none; the error names `history/` | `history` is not owned by the process's effective user ID. |
| 22 | `history_dir_permissions` | none; the error names `history/` | The mode of `history` is not exactly `0700`. |
| 23 | `backups_dir_unusable` | none; the error names `backups/` | `backups` exists in the data directory but cannot be inspected. |
| 24 | `backups_dir_not_directory` | none; the error names `backups/` | `backups` exists but is not a directory, or is a symbolic link. |
| 25 | `backups_dir_foreign_owner` | none; the error names `backups/` | `backups` is not owned by the process's effective user ID. |
| 26 | `backups_dir_permissions` | none; the error names `backups/` | The mode of `backups` is not exactly `0700`. |
| 27 | `keys_dir_unusable` | none; the error names `keys/` | `keys` in the data directory does not exist and cannot be created, or cannot be inspected. |
| 28 | `keys_dir_not_directory` | none; the error names `keys/` | `keys` is not a directory, or is a symbolic link. |
| 29 | `keys_dir_foreign_owner` | none; the error names `keys/` | `keys` is not owned by the process's effective user ID. |
| 30 | `keys_dir_permissions` | none; the error names `keys/` | The mode of `keys` is not exactly `0700`. |
| 31 | `master_key_unusable` | none; the error names `keys/master` | `keys/master` does not exist and cannot be created, or cannot be inspected, opened or read. |
| 32 | `master_key_not_regular` | none; the error names `keys/master` | `keys/master` is not a regular file: a symbolic link (even to a valid key), a directory or a named pipe, for example. |
| 33 | `master_key_foreign_owner` | none; the error names `keys/master` | `keys/master` is not owned by the process's effective user ID. |
| 34 | `master_key_permissions` | none; the error names `keys/master` | `keys/master` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 35 | `master_key_size` | none; the error names `keys/master` | `keys/master` does not hold exactly 32 bytes. |
| 36 | `storage_filesystem_unknown` | none | The filesystem of the data directory cannot be inspected (`statfs`). |
| 37 | `storage_network_filesystem` | none | The storage profile is `local` and the data directory is on a network filesystem; see [Storage profiles](#storage-profiles). |
| 38 | `archive_db_unusable` | none; the error names `archive.db` | `archive.db` does not exist and cannot be created, or cannot be inspected. |
| 39 | `archive_db_not_regular` | none; the error names `archive.db` | `archive.db` is not a regular file: a symbolic link or a directory, for example. |
| 40 | `archive_db_foreign_owner` | none; the error names `archive.db` | `archive.db` is not owned by the process's effective user ID. |
| 41 | `archive_db_permissions` | none; the error names `archive.db` | `archive.db` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 42 | `archive_journal_unusable` | none; the error names `archive.db-journal` | `archive.db-journal` exists but cannot be inspected. |
| 43 | `archive_journal_not_regular` | none; the error names `archive.db-journal` | `archive.db-journal` exists but is not a regular file. |
| 44 | `archive_journal_foreign_owner` | none; the error names `archive.db-journal` | `archive.db-journal` is not owned by the process's effective user ID. |
| 45 | `archive_journal_permissions` | none; the error names `archive.db-journal` | `archive.db-journal` grants any access to group or others, or has the setuid, setgid or sticky bit. |
| 46 | `storage_ofd_unavailable` | none | On Linux, the storage profile is `local` and the kernel or the data directory's filesystem refused open-file-description locks; see [Storage profiles](#storage-profiles). |
| 47 | `archive_schema_newer` | none; the error names `archive.db` | The archive's schema version is newer than this build knows: a newer release wrote it. |

When the data directory is missing, it is created only after checks 1 to 14 pass,
so a start refused by checks 1 to 14 leaves nothing behind. `history/` and
`backups/` are checked only when they exist, and are never created by this
build. `keys/` and the master key are created, when missing, only after checks 15
to 26 pass, and an empty `archive.db` only after checks 15 to 37 pass. A refusal
by a later check (on a filesystem that forces its own ownership or mode, for
example), or a `startup_failed` exit, leaves what was created in place: the data
directory, `keys/`, the master key and `archive.db`.

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
effective user ID with mode exactly `0700` (refusals 19 to 26); this build does
not create them, and later M1 releases will keep history-sync downloads and
backups there. Then `serve` creates the [master key](#master-key) when it is
missing and opens the [message archive](#message-archive), `archive.db`; this
build writes nothing else there. Later in M1 the directory also holds the
WhatsApp session. It must be on a writable, persistent filesystem that supports
hard links and that only the service's user can read.

Mechanisms that add group permissions or the setgid bit to a volume, such as
Kubernetes `fsGroup`, make the directory fail the mode check.

### Master key

`keys/master` in the data directory holds the service's master key: exactly 32
bytes. Right after the data directory checks, `serve`:

1. creates `keys/` with mode `0700` when it does not exist;
2. checks `keys/` (refusals 17 to 20): a directory, not a symbolic link, owned by
   the effective user ID, with mode exactly `0700`;
3. when `keys/master` does not exist, writes 32 bytes from the operating system's
   cryptographic random source to a new temporary file `keys/.master-<random>`
   with mode `0600`, flushes it to disk, hard-links it as `keys/master`, removes
   the temporary name and flushes the directory. When two processes start at once,
   the second link fails, and both use the key that was linked first. An existing
   key is never replaced;
4. checks `keys/master` (refusals 21 to 25): a regular file, not a symbolic link,
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
| `chat-hmac` | 32 bytes | Derived but not used yet; it is reserved for the chat references in notification events (M1). |

The key id is the only value derived from the master key that the service ever
writes. Replacing or deleting the master key changes the key id and every log
pseudonym; this build has no command to rotate it. To provide your own key, write
32 random bytes to `keys/master` with mode `0600` or `0400`, owned by the service's
user, before the first start. A crash or power loss during the first start can
leave a `keys/.master-<random>` file behind, with mode `0600`. It holds either an
unused key or, after a crash right after the link, a second name of the current
master key, so delete it together with `keys/master` when you replace the key; it
can be deleted at any time while the service is stopped.

### Message archive

`archive.db` in the data directory is the message archive: a SQLite database
written by the SQLite engine compiled into the binary (`modernc.org/sqlite`).
This build creates it, applies its schema and records each start in it; nothing
writes WhatsApp traffic into it yet, because the engine arrives later in M1.
Right after the master key, `serve`:

1. checks the data directory's filesystem against the
   [storage profile](#storage-profiles) (refusals 36 and 37);
2. creates `archive.db` empty with mode `0600` when it does not exist, and
   refuses (38 to 41) one that is not a regular file, belongs to another user,
   or grants any access to group or others; then refuses (42 to 45) an
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
   is refused in turn (46) instead of falling back to classic locks;
5. brings the schema up to date (an archive written by a newer release is
   refused, 47) and logs `archive_opened`.

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

Every read of the archive must finish within 2 seconds and every write within
10 seconds. A call that runs out of time is interrupted and logged as
`db_deadline` with the profile of every goroutine of the process (function names
and source positions only).

Revoked, edited and expired message text will be removed from the database file,
its journal and the full-text index, as described in the
[threat model](threat-model.md#security-invariants) (I-8); backups are
outside that guarantee.

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
directory's filesystem, for profile `local` only: below it ingest will pause,
and it resumes once the free space reaches 1.25 times the floor. `0` turns the
floor off. This build validates the setting, but it ingests nothing yet, so
nothing measures the free space or pauses; the engine that does arrives later
in M1. With profile `nfs` the floor never applies, because a network filesystem
such as Amazon EFS grows on demand; watch its own capacity metrics instead.

## Listeners

The service opens these listeners and no other socket. All three speak plain HTTP
without TLS. WaWarden does not restrict who can connect: network access control
and TLS are the deployer's responsibility, and none of the listeners may face the
public internet.

| Listener | Address | Opened | Serves |
|---|---|---|---|
| `client` | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | Always | The client API. M0 has no client routes and no client tokens: every request is refused. |
| `admin` | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082` | Only when an admin token hash is configured | `GET /metrics` with the admin token. |
| `health` | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`, loopback only | Always | `GET /healthz`, without authentication. |

Every start logs a `listening` event for each listener with its bound address,
and a `listener_not_loopback` warning for each client or admin listener bound to
an address that is not loopback. The warning is written whatever
`WAWARDEN_LOG_LEVEL` is set to; see [Logging](#logging).

Each listener's HTTP server has these limits: 5 seconds to read the request
headers, 15 seconds to read the whole request, 30 seconds to write the response,
60 seconds of idle time on a kept-alive connection, and 16 KiB of request
headers.

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

**Client listener.** M0 knows no client tokens, so every request that passes
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

The admin listener serves one route, `GET /metrics` (Go's router also answers
`HEAD` on it); see [Metrics](#metrics).

#### Failed authentication

The client listener and the admin listener each count failed authentications, in
`wawarden_auth_failures_total` and `wawarden_admin_auth_failures_total`. Each
also has one failure budget shared by all callers: 30 failures, refilled at one
per second up to 30. While the budget is empty, failures are answered `429`
instead of `401`.

The budget changes only the answer to a failure. Authentication still runs for
every request, and a valid credential is accepted whether or not the budget is
empty, so legitimate callers cannot be locked out. For the same reason the budget
does not slow down guessing: what protects the admin token against guessing is its
256 random bits. Anyone who can reach a listener can keep its budget empty, after
which a caller presenting a wrong token sees `429` rather than `401`.

Failed authentications are counted, not logged.

#### Request bodies

No route in this build reads a request body; the admin routes that will (`pair`
and `reconnect`, M1) and later client routes decode it with one decoder. Only a
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
| `GET /healthz` while the service is serving and holds the [archive](#message-archive) | `200` `{"status":"ok"}` |
| `GET /healthz` once shutdown has begun, or after the archive's connection was lost (`db_lost`) | `503` `{"status":"unavailable"}` |
| Any other method on `/healthz`, including `HEAD` | `405` `{"error":"method_not_allowed"}` with `Allow: GET` |
| Any other path | `404` `{"error":"not_found"}` |

The health listener requires no credential, so its address must be loopback; the
service refuses any other (`health_address_not_loopback`). `200` means the
process is up, its listeners are serving, and the archive is open and locked by
the service. The health listener opens only after the archive, so while `serve`
waits for the archive's lock, health checks fail to connect.

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
| `{"error":"unsupported_media_type"}`, `{"error":"body_too_large"}`, `{"error":"invalid_body"}` | `415`, `413`, `400`, from a route that reads a body (none in this build); see [Request bodies](#request-bodies) |

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

`WAWARDEN_LOG_LEVEL` sets the lowest level written, with three exceptions that
ignore the setting. The `startup_refused` line is written before the
configuration is accepted. The `dev_build` and `listener_not_loopback` warnings
are written at every level, `error` included, because they are the only signal
that a development binary is running or that a listener is reachable beyond
loopback.

| Event | Level | Other keys | Written when |
|---|---|---|---|
| `startup_refused` | `ERROR` | `reason`, `error` | The configuration or the [master key](#master-key) is refused; exit `2`. |
| `keys_loaded` | `INFO` | `key_id` | The master key is loaded and the log pseudonyms are keyed with it; `key_id` is the 8-digit key id, never a key. |
| `db_lock_wait` | `WARN` | `database` (`archive`), `within` | Another process holds the archive's lock; `serve` retries until `within` (`5m0s`) has passed. Logged once per start. |
| `archive_opened` | `INFO` | `schema_version`, `profile`, `ofd_locking`, `recent_starts` | The [archive](#message-archive) is open and locked. `ofd_locking` is `true` when open-file-description locks are in use, and `recent_starts` counts the starts of the last ten minutes, this one included, at most 64. |
| `startup_failed` | `ERROR` | `error` | The archive cannot be opened or its lock was not acquired in time, or a listener cannot be opened, for example because its address is in use; exit `1`. |
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
| `db_deadline` | `ERROR` | `database`, `operation`, `timeout_ms`, `goroutines` | A read of the archive ran beyond 2 seconds or a write beyond 10; the call is interrupted and fails. `operation` names the call in the code, and `goroutines` holds the goroutine profile of the process (function names and source positions, cut at 32 KiB). |
| `db_lost` | `ERROR` | `database` | The connection that held the archive's lock is gone, and the service refuses to open another; `/healthz` answers `503` from then on. |
| `log_dropped` | `WARN` | `reason`: `xml` or `too_long` | Replaces a line that carried XML (`xml`) or was longer than 65,536 bytes (`too_long`); see [Pseudonyms and dropped lines](#pseudonyms-and-dropped-lines). It is written in place of a line that passed the log level, whatever that line's level was. |

What is never logged: requests (there is no access log), request bodies, header
values, tokens, failed authentications, the admin token's hash, the values of
refused variables (a refusal names the variable only), panic values (only their
Go type and the stack), the master key and the keys derived from it (only the key
id), WhatsApp identifiers in their `user@server` form when no ASCII letter or
digit follows the server name (they become
[pseudonyms](#pseudonyms-and-dropped-lines)) and lines carrying XML in one of
the [recognised shapes](#pseudonyms-and-dropped-lines). The listen addresses are the only configuration values that
are logged: the `listening` and `listener_not_loopback` events carry the bound
address, and the `error` texts of `startup_failed` and `listener_failed` come
from the operating system and can contain a listen address.

The CLI writes to standard error only its usage text, the flag parser's one-line
error for an unknown flag or an invalid flag value (it repeats the flag as typed,
for example `flag provided but not defined: -nope`), `healthcheck` failures and
the `admin init` reminder. The Go runtime writes crash output to standard error.

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
byte slice is written as its length (`[32 bytes]`), an error or a value with a
`String` method as its text, which is then pseudonymised like the rest of the
line, and every other value as its Go type in brackets, such as `[seal.Chat]`,
never as its content.

## Metrics

Metrics are served only on the admin listener, at `GET /metrics`, with the admin
token: without an admin hash there is no way to read them. The format is the
Prometheus text exposition format 0.0.4
(`Content-Type: text/plain; version=0.0.4; charset=utf-8`), sorted by metric name.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wawarden_auth_failures_total` | counter | | Failed authentications on the client listener, including those answered `429`. |
| `wawarden_admin_auth_failures_total` | counter | | Failed authentications on the admin listener, including those answered `429`. |
| `wawarden_panics_total` | counter | `name` | Panics recovered, by handler or goroutine name. Absent until the first panic. |
| `wawarden_build_info` | gauge | `version`, `revision`, `dev` | Always `1`; the labels describe the running binary. |

Panic names in M0: `api.client`, `api.admin` and `api.health` for the handlers;
`listeners.client`, `listeners.admin`, `listeners.health`, `listeners.shutdown`
and `signals` for goroutines.

Anything that scrapes `/metrics` holds the full admin token, which from M1 can
start pairing and from M2 can create clients. Treat a scrape configuration as
holding the admin credential. Metrics on standard output in embedded metric format
(`WAWARDEN_METRICS_EMF`), planned for M1, need no token; prefer them for alerting
once they exist.

### Reserved metric and event names

**Not in this build.** These names are reserved for later milestones; the M0
binary emits none of them. Deployments may prepare alarms on them, but nothing
matches until the release that brings them.

Metrics in embedded metric format on standard output, namespace `WaWarden`,
written every 60 seconds when `WAWARDEN_METRICS_EMF` is `1`:

| Metric | Planned for |
|---|---|
| `Paired` | M1 |
| `Connected` | M1 |
| `MessagesIngested` | M1 |
| `PolicyDenials` | M2 |
| `Panics` | M1 |
| `SendsRejected` | M3 |
| `AuthFailures` | M1 |
| `AdminAuthFailures` | M1 |

Any of these metrics that is also broken down by a label, for example `Panics`
by goroutine name, is also emitted as a dimensionless total under the same name,
so an alarm can match it without dimensions.

Events on standard output, as single-line JSON objects with an `event` field like
the events under [Logging](#logging):

| Event | Planned for |
|---|---|
| `admin_mutation` | M1 |
| `unpaired` | M1 |
| `quarantine` | M1 |
| `admin_auth_failure` | M1 |

## Shutdown

`SIGTERM` or `SIGINT` starts a graceful shutdown:

1. `/healthz` starts answering `503`.
2. The client and admin listeners stop accepting connections, and requests in
   flight are allowed to finish.
3. The [archive](#message-archive) is closed, which releases its lock.
4. The health listener stops.

One grace period of 10 seconds bounds the whole shutdown. When it runs out, the
remaining connections are closed and `serve` exits `1`; otherwise it exits `0`.
Give the process more than 10 seconds between the stop signal and a forced kill;
Docker's default stop timeout is exactly 10 seconds.

A second `SIGTERM` or `SIGINT` received after shutdown has begun ends the process
at once.

## Container image

Release images are published as `ghcr.io/dortort/wawarden` for `linux/amd64` and
`linux/arm64`, by the release workflow only. Deploy them by digest;
[`RELEASING.md`](../RELEASING.md) explains how to verify one. No image exists
before the first release; until then, build from source. The contract below
applies to release images:

| Item | Value |
|---|---|
| Base | `gcr.io/distroless/static-debian13:nonroot`, pinned by digest: no shell, no package manager |
| Binary | `/wawarden`, mode `0555` |
| Entrypoint and command | `ENTRYPOINT ["/wawarden"]`, `CMD ["serve"]`; pass another subcommand as the command, for example `admin init` |
| User | `65532:65532` |
| Writable path | `/data`, the only path the service writes and the only one that must be writable: an empty directory owned by `65532:65532` with mode `0700`; mount a volume there |
| Root filesystem | Mount it read-only (`docker run --read-only`, or `readOnlyRootFilesystem: true` in Kubernetes) so that `/data` is also the only writable path; the service writes nothing outside the data directory |
| Health check | `["/wawarden","healthcheck"]`, in exec form; the image declares no health check of its own |
| Exposed ports | None declared |
| Labels | `org.opencontainers.image.source`, `licenses`, `version`, `revision` |
| Licences | `/licenses`: the licence files of every Go module linked into `/wawarden`, one directory per module and version; see [`RELEASING.md`](../RELEASING.md#licences) |

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
