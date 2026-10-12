# Runbook

The commands for routine operations and incidents. The one-time setup is
[First run with a WhatsApp account](../README.md#first-run-with-a-whatsapp-account)
in the README, the settings that prevent incidents are in the
[hardening guide](hardening.md), and every flag, code and event is in the
[configuration reference](configuration.md). This page describes the code at
the commit or tag you are reading.

The admin commands below read the admin token from `admin.token`; any of
`--token-file PATH`, `--token-stdin` or `--token-command COMMAND` works, with
`--addr URL` when the admin listener is not at `http://127.0.0.1:8082`. In a
container, run the same commands with `docker exec <container> /wawarden ...`
or from the operator's machine against the admin port. Exit codes are listed
under [exit codes](configuration.md#exit-codes).

| Situation | Section |
|---|---|
| Add, rotate or remove a client | [Clients](#clients) |
| Replace the admin token or the master key | [Admin token](#rotating-the-admin-token), [master key](#rotating-the-master-key) |
| Check backups, rehearse or perform a restore | [Backups](#backups) |
| Check the audit chain | [Audit chain](#audit-chain) |
| Move to a new release | [Upgrading](#upgrading) |
| `disconnected`, `unpaired`, a ban | [Engine disconnected](#engine-disconnected), [suspected ban](#suspected-ban) |
| A token may have leaked | [Token leak](#token-leak) |
| A send answered `504` or `500`, `send_outcome_unknown`, `send_unrecorded` | [Uncertain sends](#uncertain-sends) |
| `backup_failed`, `backup_prune_failed` | [Backup failures](#backup-failures) |
| `ingest_paused`, `quarantine`, `restart_budget`, `db_lost` | [Other incidents](#other-incidents) |

## Before the first start

Besides what the README's first run sets, decide these before clients depend
on the service ([environment variables](configuration.md#environment-variables)):

- `WAWARDEN_BACKUP_AGE_RECIPIENT`, and optionally `WAWARDEN_BACKUP_TIME` (UTC,
  `03:00` by default) and `WAWARDEN_BACKUP_KEEP` (14 by default). Without a
  recipient no backup is taken, and the other two are refused.
- `WAWARDEN_AUDIT_RETENTION_DAYS`, 90 by default, 7 to 3650.
- `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` (10 by default and at most) and
  `WAWARDEN_SEND_GLOBAL_PER_HOUR` (30 by default, 60 at most). Lower is safer
  for the account ([ban risk](ban-risk.md)).

Keep a copy of `keys/master` off the host once the first start has created it,
and of `keys/retired/` after each [rotation](#rotating-the-master-key).

## Clients

### Creating a client

```sh
./wawarden admin chats list --match <part of a name> --token-file admin.token
./wawarden admin clients create --token-file admin.token \
  --name <name> --read <chat> [--read <chat> ...] [--expires-days 90]
```

A chat is an `id`, a `+E.164` number or the `ref` that `admin chats list`
prints. For a client that sends, add `--write <chat>` for each chat it may send
to; each must also be a `--read` chat, and a client created with `--all-chats`
cannot write. Add `--allow-first-contact` only when the agent must write to a
direct chat that has never written to the account. The command prints the
token once, on the `token:` line; hand it to the agent's host and nowhere else.

### Rotating a client token

Before a token expires (`admin clients list` shows `expires`):

1. Create a client with the same chats under a new name; a name stays taken
   after its client is revoked.
2. Put the new token on the agent's host and register it again, for Claude Code
   as in [Registering with Claude Code](configuration.md#registering-with-claude-code).
3. Check that the agent works with the new token.
4. `./wawarden admin clients revoke --id <old id> --token-file admin.token`.

### Revoking a client

`./wawarden admin clients revoke --id <id> --token-file admin.token` takes
effect on the client's next request. `admin clients show --id <id>` shows the
client's chats and state.

## Rotating the admin token

The admin token never expires. To replace it:

1. Generate a new token and its SHA-256 into files of mode `0600`, and delete
   the full output. The shell creates the files, so set the `umask` first:

   ```sh
   (
     umask 077
     ./wawarden admin init > admin.txt
     sed -n 's/^sha256: //p' admin.txt > new.sha256
     sed -n 's/^token: //p' admin.txt > new.token
     rm admin.txt
   )
   ```

2. Put the new hash from `new.sha256` in `WAWARDEN_ADMIN_TOKEN_SHA256` or the
   file `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` names, and the new token from
   `new.token` in the file the CLI reads.
3. Restart the service; the hash is read only at start.

## Rotating the master key

`keys rotate` works offline, on a stopped service
([`keys rotate`](configuration.md#keys-rotate)):

1. Make sure a recent backup exists and you hold a copy of `keys/master`.
2. Stop the service.
3. As the service's user, run `wawarden keys rotate --data-dir <data dir>`;
   with the image, `docker run --rm --mount type=volume,src=<volume>,dst=/data
   <image> keys rotate --data-dir /data`. It prints the previous and the new
   key id. `service_running` means something still holds the archive.
4. Start the service. It logs `keys_loaded` with the number of retired keys,
   and `audit_key_changed` at `INFO` with `previous_key` `retired` until the
   first audit row is written under the new key.
5. Copy `keys/master` and `keys/retired/` off the host.

Log pseudonyms and new message references change; cursors and references
issued before keep working while their key stays in `keys/retired/`. Nothing
deletes a retired key, and `audit verify` needs every key its `key ids` line
lists, and so does the audit retention, which removes no row it cannot
verify. An `audit_key_changed` at `WARN`, with `previous_key` `missing`, means
the master key was replaced without keeping the old one: restore your copy of
it into `keys/retired/` under its key id, as `keys rotate` would have.

## Backups

### Checking a backup

Copy the newest `backups/<UTC time>.age` off the host to where the age
identity is kept, and run:

```sh
set -o pipefail
mkdir -m 700 verify
age --decrypt -i backup-identity.txt <UTC time>.age |
  wawarden backup verify --work-dir verify -
```

It prints counts and `verified: yes` and exits `0`. It exits `1` when the
file cannot be decrypted or read to its end (a damaged or truncated file, or
the wrong identity), and `6` when the decrypted stream is not a complete,
sound backup ([`backup verify`](configuration.md#backup-verify)).
Compare its counts with `admin status` of the live service.

### Restore drill

Rehearse a restore away from the deployment, with a copy of a backup and the
identity kept off the service's host:

1. Decrypt and extract: the pipeline above with `--extract-to restore`, which
   leaves `restore/archive.db` and `restore/session.db`.
2. Check the audit chain of the extracted archive with your copies of the keys:
   `wawarden audit verify --db restore/archive.db --master-key-file master
   --retired-keys-dir retired --log heads.log`, where `heads.log` is the
   captured output cut at the backup's time as in [Audit chain](#audit-chain).
   An uncut capture also holds the heads shipped after the backup, which the
   copy lacks, so they would count as missing.
3. To start `serve` on it, use a scratch data directory of mode `0700` that
   holds only the restored `archive.db`: the missing `session.db` is created
   empty, so the engine stays unpaired and makes no connection. Never start a
   restored `session.db` while the original service runs.
4. Delete the decrypted files when done; they hold the whole archive and the
   linked device's keys.

### Restoring

There is no restore command. Follow
[Restoring](configuration.md#backups) in the configuration reference: stop the
service, verify the backup with `--extract-to`, replace `archive.db` and
`session.db` with the two files of one backup (owner the service's user, mode
`0600`), delete their journals, restore your copies of `keys/master` and
`keys/retired/`, note the UTC time, then start. Before giving agents access
again, read the `admin_mutation` events since the backup, revoke again the
clients revoked since, and keep sending clients stopped until you are sure:
the record of sends rolls back with the archive, so a retried send can go out
again.

The audit rows written between the backup and the restart are lost, and the
rows written after the restart take their ids with other chain heads, so the
heads shipped in between can never match. Keep the capture of standard output
from before the restore, the only record of those requests, and leave those
heads out of every later `--log` as [Audit chain](#audit-chain) shows;
otherwise `audit verify` counts them as missing and exits `6` until the
retention removes the first rows written after the restart, about
`WAWARDEN_AUDIT_RETENTION_DAYS` later.

## Audit chain

Each request's audit line on standard output carries the chain head. Capture
standard output, and from time to time run `audit verify` on the archive of a
fresh backup, as in step 2 of the [restore drill](#restore-drill), with
`--log` pointing at the capture cut at the backup's time
([`audit verify`](configuration.md#audit-verify)). Every head shipped after the
backup is missing from its copy, so keep only the audit lines whose `ts` is at
least a minute earlier than the UTC time in the backup's file name: a
request's row can be written a few seconds after its `ts`. With `jq`, for
`20261011T030000Z.age`:

```sh
jq -cR 'fromjson? | select(.event == "audit" and .ts < "2026-10-11T02:59")' \
  captured.log > heads.log
```

After a [restore](#restoring), also leave out the heads shipped from a minute
before the restored backup's time to the time noted just before the restored
service started, and nothing more, so that rows removed before or after that
window are still found. For a restore of `20261011T030000Z.age` started at
09:15 UTC that day, checked against the backup `20261012T030000Z.age`:

```sh
jq -cR 'fromjson? | select(.event == "audit" and .ts < "2026-10-12T02:59"
  and (.ts < "2026-10-11T02:59" or .ts >= "2026-10-11T09:15"))' \
  captured.log > heads.log
```

| Result | What to do |
|---|---|
| `none` | Nothing. |
| `unknown_key` | A row was written under a key you did not pass: find the copy of that master key, or of the retired keys, and pass it. |
| `anchor_invalid` | The anchor's key is missing, or the anchor was altered: pass every key; if it still fails, treat it as tampering. |
| `hmac_mismatch`, `id_gap`, `retired_key`, shipped heads missing from a capture cut at the backup's time, with the window of every restore left out | Rows were altered, removed or reordered, or the copy is not what you think: treat it as an incident, keep the files, and compare with older backups. |

Rows older than `WAWARDEN_AUDIT_RETENTION_DAYS` are removed each night after
the backup, behind a signed anchor, and the service logs `audit_pruned`. An
`audit_prune_refused` with `chain_mismatch` means the anchor or a row due for
removal did not verify under the master key and the retired keys, so nothing
was removed: run `audit verify` on a fresh backup as above, with the
service's `keys/master` and `keys/retired/`. `unknown_key` there means a key
is missing from `keys/retired/`; restore it as in
[Rotating the master key](#rotating-the-master-key). Treat any other result as
the table says.

## Upgrading

1. Read the release notes, and verify the release as
   [RELEASING.md](../RELEASING.md#verifying-a-release) describes.
2. When moving from a release that could not send to one that can, run
   `./wawarden admin clients list --token-file admin.token` first: a client
   created with write chats becomes a live sender on the upgrade. Revoke those
   that should not send.
3. Make sure a recent backup succeeded (`backup_done`) and is copied off the
   host, with the copies of `keys/master` and `keys/retired/`.
4. Stop the service with at least 10 seconds of grace (`--stop-timeout 15`).
5. Start the new release by digest. It brings the archive and the device store
   forward to its schema. A release refuses an archive written by a newer one
   (`archive_schema_newer`), so going back means restoring the backup from
   step 3 into the older release.
6. Check `wawarden version`, `admin status` and the health check, and watch
   the events of the first minutes.

## Incidents

### Engine disconnected

`admin status` shows `state` and `reason`. The table in
[When the engine disconnects](../README.md#7-when-the-engine-disconnects)
gives the action for each reason: `admin reconnect` for `replaced` (after
stopping the other client), `temporary_ban` (after the ban ends),
`cat_refresh` and `connect_failure`; `admin pair` after `logged_out`.
Reads keep working while the engine is disconnected; sends answer `503`
`not_connected`.

### Suspected ban

Signs: `disconnected` with `temporary_ban` or `connect_failure`, or
`logged_out` that nobody caused from the phone.

1. Revoke every client that can send:
   `./wawarden admin clients revoke --id <id> --token-file admin.token`.
2. Look at the owner's phone and in WhatsApp for a notice of a restriction or
   ban. WaWarden does not report WhatsApp's reason.
3. Check what was sent before: the audit lines with the actions `rest.send`
   and `mcp.send_message`, `wawarden_sends_rejected_total` by reason, the
   archive's messages with `origin` `gateway:<client id>` as a read client sees
   them, which also hold a send whose audit row the busy archive could not
   take, and the chats themselves on the phone.
4. For a temporary ban, wait for its end, then `admin reconnect`. After a
   logout, pair again only once the phone works normally; pairing is limited to
   3 attempts an hour.
5. Recreate writing clients narrower than before.

[Ban risk](ban-risk.md#what-happens-on-a-ban-or-a-logout) explains the causes.

### Token leak

**A client token.** Revoke the client (`admin clients revoke --id <id>`); it
fails from its next request. Create a replacement with a new name. Read the
client's audit lines since the leak to see what it read, and for a client with
write chats what it sent: the send limits and the first-contact rule bound
what it could send, and the phone shows the messages as the owner's.

**The admin token.** Replace it as in
[Rotating the admin token](#rotating-the-admin-token). Then run
`admin clients list` and revoke every client you did not create, read the
`admin_mutation` events since the leak (pairing, reconnects, backfills, client
changes), and check Linked devices on the phone.

### Uncertain sends

A `504` `send_outcome_unknown` means the message may have reached WhatsApp.
The service never sends it again, and every retry with the same key answers
`504`. The event `send_outcome_unknown` gives the reason: `ack_timeout`,
`lease_expired` or `restart`. Read the chat, through the API or on the phone,
before anyone sends anything else to it.

`send_unrecorded` means a send's outcome could not be stored, or the key of a
send that never left could not be freed. The key answers `409` until its
60-second lease ends and `504` afterwards. Treat it like a `504`, and look for
the cause of the failed write, such as a full disk.

A `502` `send_rejected` is final: WhatsApp refused the message or the
recipient cannot be reached. Retrying under the same key answers the same.

### Backup failures

`backup_failed` carries a reason ([backups](configuration.md#backups)); the
service retries after 15 minutes, then with waits that double up to 4 hours.

- `space`: free space or grow the volume; old backups can be deleted by hand.
- `directory`, `output`, `rename`, `cleanup`: check that `backups/` and
  `backups/tmp/` are directories of the service's user with mode `0700`.
- `exists`: a file already has the backup's name; look at the clock.
- `archive`, `session`, `manifest`, `encrypt`, `sync`: usually the disk; read
  the system's logs.
- `cancelled`: the service was stopping; nothing to do.

`backup_prune_failed` with `unexpected_entry` means `backups/` holds something
the service did not write; until it is removed, no old backup is deleted.
`directory`, `remove` and `sync` point at permissions or the disk.

### Other incidents

- **`ingest_paused`** (`free_bytes`, `floor_bytes`): the data directory fell
  below `WAWARDEN_MIN_FREE_BYTES`. Messages that arrive while ingest is paused
  are lost, not delayed ([delivery](configuration.md#delivery)). Free space or
  grow the volume; ingest resumes at 1.25 times the floor and logs
  `ingest_resumed`. The service never deletes backups to make room.
- **`quarantine`** (`queue`: `inbox` or `history`): an item failed three times
  and was set aside; `admin status` counts it. Read the log lines around it.
  If a crafted message could cause it, report it as
  [SECURITY.md](../SECURITY.md) describes.
- **`restart_budget`**: the service started more than 5 times in 10 minutes and
  the engine did not connect. Fix the cause of the restarts from the logs, then
  `admin reconnect`, or restart once 10 minutes have passed.
- **`db_lost`**: the connection holding a database's lock is gone and
  `/healthz` answers `503`. Restart the service; find out what removed or
  locked the file.
- **A history gap**: `./wawarden admin backfill --chat <chat> --count 50
  --token-file admin.token` asks the phone for the 50 messages before the
  oldest one the archive holds. At most one request every 5 minutes and 20 a
  day; `backfill_quota` and `backfill_no_anchor` exit `5`.
