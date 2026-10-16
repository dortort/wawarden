# Runbook

The commands for routine operations and incidents. The one-time setup is in
[Getting started](getting-started.md). The settings that prevent incidents are
in the [hardening guide](hardening.md). Every variable is in the
[configuration reference](configuration.md), every subcommand and exit code in
the [CLI reference](cli.md), and every event in
[observability](observability.md). This page describes the code at the commit
or tag you are reading.

The admin commands below read the admin token from `admin.token` with
`--token-file admin.token`. Every admin command needs exactly one token source:
`--token-file PATH`, `--token-stdin` or `--token-command COMMAND`. Without one,
it exits `2`. Add `--addr URL` when the admin listener is not at
`http://127.0.0.1:8082`.

The distroless image has no `admin.token` and no shell. In a container, pass
the token on standard input from the host:

```sh
docker exec -i <container> /wawarden admin status --token-stdin < admin.token
```

Exit codes are listed under [exit codes](cli.md#exit-codes).

## Contents

- [Situations](#situations)
- [Before the first start](#before-the-first-start)
- [Clients](#clients)
- [Rotating a client token](#rotating-a-client-token)
- [Retiring other linked devices](#retiring-other-linked-devices)
- [Rotating the admin token](#rotating-the-admin-token)
- [Rotating the master key](#rotating-the-master-key)
- [Rotating the backup recipient](#rotating-the-backup-recipient)
- [Rotating the webhook secret](#rotating-the-webhook-secret)
- [Backups](#backups)
- [Restoring a backup](#restoring-a-backup)
- [Audit chain](#audit-chain)
- [Upgrading](#upgrading)
- [Moving to a new host](#moving-to-a-new-host)
- [Unpairing and pairing again](#unpairing-and-pairing-again)
- [Startup refusals](#startup-refusals)
- [Disconnect reasons](#disconnect-reasons)
- [Incidents](#incidents)

## Situations

| Situation | Section |
|---|---|
| Add, rotate or remove a client | [Clients](#clients), [Rotating a client token](#rotating-a-client-token) |
| Replace the admin token, the master key, the backup recipient or the webhook secret | [Admin token](#rotating-the-admin-token), [master key](#rotating-the-master-key), [backup recipient](#rotating-the-backup-recipient), [webhook secret](#rotating-the-webhook-secret) |
| Check backups, rehearse or perform a restore | [Backups](#backups), [Restoring a backup](#restoring-a-backup) |
| Check the audit chain | [Audit chain](#audit-chain) |
| Move to a new release or a new host | [Upgrading](#upgrading), [Moving to a new host](#moving-to-a-new-host) |
| `serve` exits `1` or `2` at start: `startup_refused`, `startup_failed` | [Startup refusals](#startup-refusals) |
| `disconnected`, `unpaired`, a ban | [Disconnect reasons](#disconnect-reasons), [suspected ban](#suspected-ban) |
| `pair_rejected`, `logout_failed` | [Pairing rejected](#pairing-rejected) |
| A token may have leaked, `admin_auth_failure` | [Token leak](#token-leak), [Failed admin authentication](#failed-admin-authentication) |
| A send answered `504` or `500`, `send_outcome_unknown`, `send_unrecorded` | [Uncertain sends](#uncertain-sends) |
| `backup_failed`, `backup_prune_failed` | [Backup failures](#backup-failures) |
| `rekey_conflict` | [Re-key conflicts](#re-key-conflicts) |
| `notify_dropped`, `notify_failed` | [Dropped notifications](#dropped-notifications) |
| `ingest_paused`, `quarantine`, `restart_budget`, `db_lost` | [Other incidents](#other-incidents) |

## Before the first start

Besides what [Getting started](getting-started.md) sets, decide these before
clients depend on the service
([environment variables](configuration.md#environment-variables)):

- `WAWARDEN_BACKUP_AGE_RECIPIENT`. Optionally also `WAWARDEN_BACKUP_TIME` (UTC,
  `03:00` by default) and `WAWARDEN_BACKUP_KEEP` (14 by default). Without a
  recipient no backup is taken, and the other two are refused.
- `WAWARDEN_AUDIT_RETENTION_DAYS`, 90 by default, 7 to 3650.
- `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE` (10 by default and at most) and
  `WAWARDEN_SEND_GLOBAL_PER_HOUR` (30 by default, 60 at most). Lower is safer
  for the account ([ban risk](ban-risk.md)).
- `WAWARDEN_MEDIA_MAX_BYTES`, `0` by default, which keeps attachment downloads
  off. Above 0 (`104857600`, 100 MiB, is the suggested value), the service
  keeps a sealed reference to each attachment up to that size for
  `WAWARDEN_MEDIA_REF_RETENTION_DAYS` (30 by default); this build fetches no
  attachment yet. At or below `16777216` (16 MiB), no attachment byte will be
  written to disk. Give the container `--memory=768m` with downloads on.
  Setting it back to 0 removes every stored reference within a minute.

Keep a copy of `keys/master` off the host once the first start has created it.
Do the same for `keys/retired/` after each
[rotation](#rotating-the-master-key).

## Clients

### Creating a client

```sh
wawarden admin chats list --match <part of a name> --token-file admin.token
wawarden admin clients create --token-file admin.token \
  --name <name> --read <chat> [--read <chat> ...] [--expires-days 90]
```

A chat is an `id`, a `+E.164` number or the `ref` that `admin chats list`
prints. A client expires after 90 days unless `--expires-days` says otherwise,
365 at most.

For a client that sends:

- Add `--write <chat>` for each chat it may send to. Each must also be a
  `--read` chat.
- A client created with `--all-chats` cannot write.
- Add `--allow-first-contact` only when the agent must write to a direct chat
  that has never written to the account.

The command prints the token once, on the `token:` line. Hand it to the
agent's host and nowhere else.

### Revoking a client

```sh
wawarden admin clients revoke --id <id> --token-file admin.token
```

The revocation takes effect on the client's next request. To see a client's
chats and state:

```sh
wawarden admin clients show --id <id> --token-file admin.token
```

## Rotating a client token

A token cannot be renewed. Before it expires, replace its client with a new
one. `admin clients list --token-file admin.token` shows each client's `id`
and `expires`.

1. Create a client with the same chats under a new name. A name stays taken
   after its client is revoked.
2. Put the new token on the agent's host and register it again.
3. Check that the agent works with the new token.
4. Revoke the old client.

For Claude Code, with the token in `WAWARDEN_TOKEN` on the agent's host:

```sh
wawarden admin clients create --token-file admin.token \
  --name claude-code-2 --read <id, +E.164 number or ref>
read -rs WAWARDEN_TOKEN && export WAWARDEN_TOKEN
claude mcp remove whatsapp
claude mcp add --transport http whatsapp https://<your-host>/mcp --header "Authorization: Bearer $WAWARDEN_TOKEN"
claude mcp get whatsapp
wawarden admin clients revoke --id <old id> --token-file admin.token
```

`read -rs` shows no prompt and echoes nothing. Paste the `ww_...` value of the
new client's `token:` line and press Enter. The token then stays out of the
shell's history file.

Revoke the old client only once `claude mcp get whatsapp` shows the new
registration as connected. Until then the old token still works and can be
registered again. A revocation takes effect on the client's next request.
[Registering with Claude Code](api.md#registering-with-claude-code)
says what each `--scope` writes, and how to keep the token out of files.

## Retiring other linked devices

An agent may have read WhatsApp through another device linked to the same
account before. Once the agent works through WaWarden, log that device out on
the owner's phone: Linked devices, select the device, Log out.

A linked device has the account's full access. It is outside every scope,
budget and audit row of WaWarden (see
[trust roots](threat-model.md#trust-roots) and
[unlink other devices](hardening.md#unlink-other-devices)).

## Rotating the admin token

The admin token never expires. To replace it:

1. Generate a new token and its SHA-256 into files of mode `0600`, and delete
   the full output. The shell creates the files, so set the `umask` first:

   ```sh
   (
     umask 077
     wawarden admin init > admin.txt
     sed -n 's/^sha256: //p' admin.txt > new.sha256
     sed -n 's/^token: //p' admin.txt > new.token
     rm admin.txt
   )
   ```

2. Put the new hash from `new.sha256` in `WAWARDEN_ADMIN_TOKEN_SHA256`, or in
   the file that `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` names.
3. Put the new token from `new.token` in the file the CLI reads.
4. Restart the service. The hash is read only at start.

## Rotating the master key

`keys rotate` works offline, on a stopped service
([`keys rotate`](cli.md#keys-rotate)):

1. Make sure a recent backup exists and you hold a copy of `keys/master`.
2. Stop the service.
3. As the service's user, run `wawarden keys rotate --data-dir <data dir>`.
   With the image, run
   `docker run --rm --mount type=volume,src=<volume>,dst=/data <image> keys rotate --data-dir /data`.
   It prints the previous and the new key id. `service_running` means
   something still holds the archive.
4. Start the service. It logs `keys_loaded` with the number of retired keys. It
   also logs `audit_key_changed` at `INFO`, with `previous_key` `retired`,
   until the first audit row is written under the new key.
5. Copy `keys/master` and `keys/retired/` off the host.

What changes:

- Log pseudonyms and new message references change.
- Cursors and references issued before keep working while their key stays in
  `keys/retired/`.
- Nothing deletes a retired key.
- `audit verify` needs every key that its `key ids` line lists.
- The audit retention needs them too: it removes no row it cannot verify.

An `audit_key_changed` at `WARN`, with `previous_key` `missing`, means the
master key was replaced without keeping the old one. Restore your copy of it
into `keys/retired/` under its key id, as `keys rotate` would have.

## Rotating the backup recipient

The service holds one age recipient, `WAWARDEN_BACKUP_AGE_RECIPIENT`, and reads
it only at start. To move backups to a new identity:

1. On the machine that keeps the identity, create a new key pair:
   `age-keygen -o backup-identity-2.txt`. It prints the new `age1...` public
   key.
2. Set `WAWARDEN_BACKUP_AGE_RECIPIENT` to the new public key and restart the
   service.
3. The next backup is encrypted to the new recipient. That is the nightly one,
   or one at once when the start finds a backup owed. Check it with the new
   identity as in [Checking a backup](#checking-a-backup).
4. Keep the old identity while any backup encrypted to it exists. Retention
   deletes the old backups in `backups/` after `WAWARDEN_BACKUP_KEEP` new ones.
   Copies off the host and storage snapshots keep theirs for as long as you
   keep them.

If the old identity leaked, delete the old backups by hand once a new backup
has been verified: in `backups/`, off the host and in snapshots. A backup holds
the whole archive and the linked device's keys. If a backup leaked together
with its identity, also [unpair and pair again](#unpairing-and-pairing-again).

## Rotating the webhook secret

The webhook secret is the content of the file that
`WAWARDEN_NOTIFY_SECRET_FILE` names, read only at start. Each delivery carries
one `v1=` signature, and a receiver accepts a request when any comma-separated
value of `WaWarden-Signature` matches
([webhook](observability.md#webhook)).

1. Create the new secret, at least 32 bytes, readable only by the service's
   user: `(umask 077; openssl rand -hex 32 > notify.secret.new)`.
2. Make the receiver accept a signature under either secret.
3. Put the new secret in the file the variable names, or point the variable at
   the new file. In a container, the file is mounted read-only; replace it on
   the host.
4. Restart the service. Events still queued at the stop are dropped and
   reported in `notify_dropped`; they stay on standard output.
5. Check that a new event verifies with the new secret. An
   `admin reconnect --token-file admin.token` while the engine is connected
   answers `already_connected`, exits `5`, and posts an `admin_mutation` event.
6. Remove the old secret from the receiver.

The start refuses a secret file with group write access or any access for
others (`notify_secret_permissions`), and a secret shorter than 32 bytes
(`notify_secret_invalid`).

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

It prints counts and `verified: yes` and exits `0`. Other results:

- `1`: the file cannot be decrypted or read to its end. The file is damaged
  or truncated, or the identity is wrong.
- `6`: the decrypted stream is not a complete, sound backup
  ([`backup verify`](cli.md#backup-verify)).

Compare its counts with `admin status --token-file admin.token` of the live
service.

### Restore drill

Rehearse a restore away from the deployment, with a copy of a backup and the
identity kept off the service's host. `backup verify` proves three things: the
identity opens the backup, both databases pass their integrity checks, and
this build reads their schemas.

1. Decrypt and extract: the pipeline above with `--extract-to restore`. It
   leaves `restore/archive.db` and `restore/session.db`.
2. Check the audit chain of the extracted archive with your copies of the keys:

   ```sh
   wawarden audit verify --db restore/archive.db --master-key-file master \
     --retired-keys-dir retired --log heads.log
   ```

   `heads.log` is the captured output cut at the backup's time, as in
   [Audit chain](#audit-chain). An uncut capture also holds the heads shipped
   after the backup. The copy lacks them, so they would count as missing.
3. To start `serve` on it, use a scratch data directory of mode `0700` that
   holds only the restored `archive.db`. The missing `session.db` is created
   empty, so the engine stays unpaired and makes no connection. Never start a
   restored `session.db` while the original service runs.
4. Delete the decrypted files when done. They hold the whole archive and the
   linked device's keys.

## Restoring a backup

There is no restore command. A restore is done by hand, into a stopped
service. The [backups reference](configuration.md#backups) describes what a
backup holds.

1. Stop the service. Make sure that no process or task still runs on the data
   directory.
2. Verify the backup with `backup verify --extract-to`, as in the
   [restore drill](#restore-drill), on a machine apart from the host.
3. Replace `archive.db` and `session.db` in the data directory with the two
   files of that one backup, never one from another. They are two points in
   time, the archive's first.
   - Give them the service's user as owner and mode `0600`.
   - Keep the data directory at `0700`.
   - Delete any `archive.db-journal` and `session.db-journal`.
   - Never restore `backups/` itself.
4. Restore your own copy of `keys/master`, which no backup holds. Restore
   `keys/retired/` too when [`keys rotate`](#rotating-the-master-key) ran.
   Without the master key the service creates a new one. Pseudonyms, cursors
   and message references then change, and audit rows written under the old
   key verify as `unknown_key` (see [Master key](configuration.md#master-key)).
   `audit verify` of the restored archive needs every key whose id its
   `key ids` line lists.
5. Note the current UTC time, then start the service.

### What a restore rolls back

A restore takes the service back to the time of the backup:

- **Clients.** Clients revoked after the backup are active again, with their
  old tokens. Clients created after it are gone.
- **Sends.** The send budgets and the record of sends already made roll back
  with the archive. A send retried after the restore can go out again.
- **Audit rows.** Rows written after the backup are missing. The rows written
  after the restart take their ids with other chain heads, so the heads shipped
  between the backup and the restart can never match.
- **Messages.** Messages received after the backup are missing. The text of
  messages revoked or edited after it comes back. So does the text of messages
  that expired after it, until the expiry sweep removes it again within a
  minute of the start.
- **The linked device.** The device store holds the device's keys and sessions
  as of the backup. The protocol library logs in with the stored keys and does
  not compare them with WhatsApp's view. If the device was unlinked since, the
  service is logged out and must be [paired again](#unpairing-and-pairing-again).
  WaWarden does not know how WhatsApp's servers and contacts treat sessions
  and pre-key numbers that went back in time; only the real service shows it.

### Before giving agents access again

1. Read the `admin_mutation` events since the backup.
2. Revoke again every client revoked since the backup.
3. Keep clients that can send stopped until you are sure what was sent.
4. Keep the capture of standard output from before the restore. It is the only
   record of the requests whose audit rows were lost.
5. Leave the heads shipped in between out of every later `--log`, as
   [Audit chain](#audit-chain) shows. Otherwise `audit verify` counts them as
   missing and exits `6`. That lasts until the retention removes the first rows
   written after the restart, about `WAWARDEN_AUDIT_RETENTION_DAYS` later.

Never start two instances from one `session.db`, for example a restored copy
while the original still runs. Both would act as the same linked device and
replace each other (`replaced`).

## Audit chain

Each request's audit line on standard output carries the chain head. Capture
standard output. From time to time, run `audit verify` on the archive of a
fresh backup, as in step 2 of the [restore drill](#restore-drill), with `--log`
pointing at the capture cut at the backup's time
([`audit verify`](cli.md#audit-verify)).

Every head shipped after the backup is missing from its copy. Keep only the
audit lines whose `ts` is at least a minute earlier than the UTC time in the
backup's file name: a request's row can be written a few seconds after its
`ts`. With `jq`, for `20261011T030000Z.age`:

```sh
jq -cR 'fromjson? | select(.event == "audit" and .ts < "2026-10-11T02:59")' \
  captured.log > heads.log
```

After a [restore](#restoring-a-backup), also leave out the heads shipped in the
restore window, and nothing more. The window runs from a minute before the
restored backup's time to the time noted just before the restored service
started. Rows removed before or after that window are then still found. For a
restore of `20261011T030000Z.age` started at 09:15 UTC that day, checked
against the backup `20261012T030000Z.age`:

```sh
jq -cR 'fromjson? | select(.event == "audit" and .ts < "2026-10-12T02:59"
  and (.ts < "2026-10-11T02:59" or .ts >= "2026-10-11T09:15"))' \
  captured.log > heads.log
```

| Result | What to do |
|---|---|
| `none` | Nothing. |
| `unknown_key` | A row was written under a key you did not pass. Find the copy of that master key, or of the retired keys, and pass it. |
| `anchor_invalid` | The anchor's key is missing, or the anchor was altered. Pass every key; if it still fails, treat it as tampering. |
| `hmac_mismatch`, `id_gap`, `retired_key` | Rows were altered, removed or reordered, or the copy is not what you think. Treat it as an incident, keep the files, and compare with older backups. |
| Shipped heads missing | The same, when the capture is cut at the backup's time and the window of every restore is left out. |

Rows older than `WAWARDEN_AUDIT_RETENTION_DAYS` are removed each night after
the backup, behind a signed anchor, and the service logs `audit_pruned`.

An `audit_prune_refused` with `chain_mismatch` means the anchor, or a row due
for removal, did not verify under the master key and the retired keys. The
removal stopped at the anchor before that row. Run `audit verify` on a fresh
backup as above, with the service's `keys/master` and `keys/retired/`:

- `unknown_key` there means a key is missing from `keys/retired/`. Restore it as
  in [Rotating the master key](#rotating-the-master-key).
- Treat any other result as the table says.

## Upgrading

A release brings the archive and the device store forward to its schema. A
release refuses an archive written by a newer one (`archive_schema_newer`). So
going back means [restoring](#restoring-a-backup) the backup taken before the
upgrade into the older release. A release that predates attachment downloads
also refuses to start while any `WAWARDEN_MEDIA_*` variable is set
(`unknown_variable`): remove them before going back to one.

### Before any upgrade

1. Read the release notes, and verify the release as
   [RELEASING.md](../RELEASING.md#verifying-a-release) describes.
2. When moving from a release that could not send to one that can, list the
   clients first:
   `wawarden admin clients list --token-file admin.token`. A client created
   with write chats becomes a live sender on the upgrade. Revoke those that
   should not send.
3. Make sure a recent backup succeeded (`backup_done`) and is copied off the
   host, with the copies of `keys/master` and `keys/retired/`.

### Container

1. Stop the container with at least 10 seconds of grace
   (`docker stop --time 15 <container>`, or `--stop-timeout 15` when it was
   created).
2. Start the new release by digest, with the same volume and settings.
3. Check the new release:
   - `docker exec <container> /wawarden version`
   - `docker exec <container> /wawarden healthcheck`
   - `docker exec -i <container> /wawarden admin status --token-stdin < admin.token`
4. Watch the events of the first minutes.

### Binary and systemd

For the unit in the [hardening guide](hardening.md#systemd), which runs
`/usr/local/bin/wawarden` and gives it `TimeoutStopSec=15`:

1. Download `wawarden_<version>_linux_<arch>`, `SHA256SUMS` and
   `wawarden_<version>_provenance.sigstore.json` into an empty directory, as
   [Release binary](getting-started.md#release-binary) shows, and check them as
   [RELEASING.md](../RELEASING.md#verifying-a-release) describes.
2. Install the new binary next to the old one:
   `sudo install -m 0755 wawarden_<version>_linux_<arch> /usr/local/bin/wawarden.new`.
3. Stop the service: `sudo systemctl stop wawarden`.
4. Keep the old binary for a rollback, and move the new one into place:

   ```sh
   sudo mv /usr/local/bin/wawarden /usr/local/bin/wawarden.old
   sudo mv /usr/local/bin/wawarden.new /usr/local/bin/wawarden
   ```

5. Start the service: `sudo systemctl start wawarden`.
6. Check the new release:
   - `wawarden version`
   - `wawarden admin status --token-file admin.token`
   - `journalctl -u wawarden` for the first minutes of events
7. Delete `wawarden.old` once you no longer need it. An older release refuses
   an archive that a newer one brought forward, so a rollback needs the backup
   from before the upgrade too.

## Moving to a new host

Move the whole data directory of a stopped service. It holds everything the
service keeps: the archive, the device store, `keys/master` and
`keys/retired/`. Backups do not hold the keys, so copying the directory loses
less than restoring a backup.

1. Prepare the new host as in [Getting started](getting-started.md) and the
   [hardening guide](hardening.md). Give it the same variables, the admin token
   hash, the backup recipient and the webhook secret. Do not start it yet.
2. Make sure a recent backup succeeded and is copied off the old host.
3. Stop the service on the old host, and keep it from starting again
   (`sudo systemctl disable --now wawarden`, or `docker update --restart=no`
   and `docker stop`).
4. Copy the data directory to the new host, including any `-journal` files
   and `keys/retired/`. `backups/` is optional.
5. Make the copy pass the start's checks:
   - every directory and file is owned by the new service's user
     (`65532:65532` for the image);
   - every directory, the data directory included, has mode `0700`;
   - every file has mode `0600`.
6. Set `WAWARDEN_DATA_DIR` to the new path, or mount the copy on `/data`. Set
   `WAWARDEN_STORAGE_PROFILE=nfs` when the new directory is on a network
   filesystem.
7. Start the service on the new host. Check that `session_opened` shows
   `"paired":true` and that `admin status --token-file admin.token` reaches
   `connected`.
8. Point the agents and the proxy at the new host. Client tokens do not change.
9. Once the new host works, delete the data directory on the old host. It holds
   the linked device's keys.

Never start both copies. Two services that hold the same device replace each
other's connection (`replaced`).

## Unpairing and pairing again

There is no admin command that unpairs. To unlink WaWarden, log the device out
on the owner's phone: Linked devices, select the device, Log out.

WhatsApp logs the device out at once while it is connected, or at its next
connection. Then:

- the engine reports `disconnected` with reason `logged_out`, then `unpaired`;
- `admin status` shows `paired: false`, and the `wawarden_paired` and
  `wawarden_connected` gauges fall to `0`;
- the device is deleted from `session.db`;
- the archive keeps everything stored so far, and nothing more arrives;
- after a restart, the engine starts `unpaired`.

To pair again, for the same account:

```sh
wawarden admin pair --token-file admin.token
```

Enter the code on the owner's phone as in [Getting started](getting-started.md).
Pairing is limited to 3 attempts an hour, and only the number in
`WAWARDEN_OWNER_PHONE` can be linked. The new device gets its own history sync
and, once that has settled, its own backup.

After a logout nobody caused, find out why before pairing again: see
[suspected ban](#suspected-ban).

## Startup refusals

`serve` checks its whole configuration, the master key, the archive and the
device store before it opens any socket. A start that stops writes one JSON
line on standard output and exits with a code
([exit codes](cli.md#exit-codes)):

| Exit code | Event | What it means |
|---|---|---|
| `2` | `startup_refused` | A check failed. `reason` gives the code, and `error` names the variable or the file in the data directory. |
| `1` | `startup_failed` | The configuration was accepted, but the archive or the device store could not be opened or brought up to date, a lock was not acquired within five minutes, the files that an earlier run left in `spool/` could not be removed, or a listener could not be opened. |

Read the line in the service's output: `journalctl -u wawarden` under systemd,
`docker logs <container>` for a container. Then:

- **`startup_refused`.** Look up `reason` in
  [Startup refusals](configuration.md#startup-refusals), which gives every code
  in the order the checks run. Fix that one cause and start again; the next
  start can stop at a later check.
- **`startup_failed` with a lock.** Another process holds the data directory,
  such as a second service or a `keys rotate`. Only one process may use it at a
  time.
- **`startup_failed` with a listener.** Another process holds the address, or
  the address cannot be bound.
- **`startup_failed` with the device store.** A store that a newer protocol
  library wrote, and that does not declare itself compatible, stops the start.
  This happens after going back to an older release: see
  [Upgrading](#upgrading).

Common refusals:

| `reason` | Cause |
|---|---|
| `unknown_variable` | A `WAWARDEN_` variable the service does not know, such as `WAWARDEN_TOKEN`, which belongs on the agent's host only. |
| `plaintext_admin_token` | `WAWARDEN_ADMIN_TOKEN` is set. Give the service the hash only. |
| `running_as_root` | The service runs as root. Run it as its own user. |
| `data_dir_foreign_owner`, `data_dir_permissions` | The data directory is not owned by the service's user, or its mode is not exactly `0700`. |
| `spool_dir_foreign_owner`, `spool_dir_permissions` | `spool/` in the data directory is not owned by the service's user, or its mode is not exactly `0700`. Fix its owner or mode, or delete it while the service is stopped: it holds nothing that outlives a run. |
| `media_max_bytes_invalid` | `WAWARDEN_MEDIA_MAX_BYTES` is not `0` or a whole number of bytes up to 2147483648, such as `100MiB` written for `104857600`. |
| `media_global_bytes_per_hour_invalid` | `WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR` is below `WAWARDEN_MEDIA_MAX_BYTES`, or outside 1 to 68719476736. |
| `media_ref_retention_days_invalid`, `media_per_client_per_minute_invalid`, `media_global_per_hour_invalid` | An attachment setting is outside its range (1 to 365 days, 1 to 30 a minute, 1 to 600 an hour). The unsafe caps switches never lift these. |
| `archive_schema_newer` | The archive was written by a newer release. |

## Disconnect reasons

This is the canonical list of the engine's disconnect reasons. A disconnection
never stops the process. `/healthz` still answers `200`, and reads keep working
from the archive. Sends answer `503` `not_connected`.

The engine retries an ordinary drop by itself, waiting up to 5 minutes between
attempts; its state is then `connecting`. It stays `disconnected` for the
reasons below until you act. `admin status` shows `state` and `reason`. Every
reason but `shutdown` is also reported as the `disconnected` event.

| `reason` | What happened | What to do |
|---|---|---|
| `outdated` | No protocol version could be fetched from `web.whatsapp.com` at the start. Or WhatsApp reported the client as outdated, and no newer version could be fetched. | Check outbound access, then `admin reconnect`, which fetches again, or restart. After WhatsApp reported the client as outdated, only a strictly newer version helps: try again later, or upgrade WaWarden. |
| `replaced` | Another linked device or protocol client took over the session, such as a second service started from a copy of `session.db`. | Stop the other one, then `admin reconnect`. |
| `logged_out` | The device was logged out, from the phone or by WhatsApp. It is deleted, and `paired` is `false`. | Find out why ([suspected ban](#suspected-ban)), then [pair again](#unpairing-and-pairing-again). |
| `temporary_ban` | WhatsApp banned the account temporarily. | Follow [suspected ban](#suspected-ban). Wait for the ban to end, then `admin reconnect`. |
| `cat_refresh` | A connection token could not be refreshed. | `admin reconnect`. |
| `connect_failure` | WhatsApp refused the connection. | Look at the phone ([suspected ban](#suspected-ban)), then `admin reconnect`. |
| `restart_budget` | The service started more than 5 times in 10 minutes, so the engine did not connect. | Find out from the logs why it restarts. Then `admin reconnect`, or `admin pair` when no device is stored. Or restart once the 10 minutes have passed. |
| `owner_mismatch` | The stored device's number is not `WAWARDEN_OWNER_PHONE`. | See [owner mismatch](#owner-mismatch). `admin reconnect` is refused. |
| `shutdown` | The service is stopping. | Nothing. |

The commands, with their token flag:

```sh
wawarden admin reconnect --token-file admin.token
wawarden admin pair --token-file admin.token
```

`admin reconnect` answers `already_connected` while the engine is connected,
and `not_paired` when no device is stored. Both exit `5`. See
[engine states](internals.md#engine-states) for the engine's behaviour.

### Owner mismatch

The engine checks the stored device's number against `WAWARDEN_OWNER_PHONE`
before every connection. When they differ, it stays `disconnected` with
`owner_mismatch` and does not log the device out. The cause can be the
configuration or another account:

- **`WAWARDEN_OWNER_PHONE` is wrong.** Correct it and restart.
- **The device belongs to another account**, as after a failed logout of a
  rejected device:
  1. Stop the service.
  2. Delete `session.db` and `session.db-journal` from the data directory.
  3. Remove the device from that account's Linked devices.
  4. Start the service and [pair again](#unpairing-and-pairing-again).

## Incidents

### Engine disconnected

`admin status --token-file admin.token` shows `state` and `reason`. Find the
reason in [Disconnect reasons](#disconnect-reasons), which gives the action for
each.

### Suspected ban

Signs: `disconnected` with `temporary_ban` or `connect_failure`, or a
`logged_out` that nobody caused from the phone.

1. Revoke every client that can send:
   `wawarden admin clients revoke --id <id> --token-file admin.token`.
2. Look at the owner's phone and in WhatsApp for a notice of a restriction or
   ban. WaWarden does not report WhatsApp's reason.
3. Check what was sent before:
   - the audit lines with the actions `rest.send` and `mcp.send_message`;
   - `wawarden_sends_rejected_total` by reason;
   - the archive's messages with `origin` `gateway:<client id>`, as a read
     client sees them, which also hold a send whose audit row the busy archive
     could not take;
   - the chats themselves on the phone.
4. For a temporary ban, wait for its end, then
   `wawarden admin reconnect --token-file admin.token`. After a logout, pair
   again only once the phone works normally. Pairing is limited to 3 attempts
   an hour.
5. Recreate clients with write chats, narrower than before.

[Ban risk](ban-risk.md#what-happens-on-a-ban-or-a-logout) explains the causes.

### Pairing rejected

Only the account whose number is `WAWARDEN_OWNER_PHONE` can be linked. The
`pair_rejected` event says another account tried, with `stage`:

- **`before_save`**: the code was entered on another account's phone. Nothing
  was stored, and the engine stays `unpaired`. Check the number in
  `WAWARDEN_OWNER_PHONE`, then run `admin pair` again and enter the code on the
  owner's phone.
- **`after_pairing`**: pairing completed for another number. The engine stays
  `unpaired` and logs the new device out.

A failed logout is reported as `logout_failed`, with `attempt` and
`error_type`. The engine tries again with the reconnection delays until it
succeeds or the device is gone. Until then, the engine never connects the
device, and pairing stays refused (`already_paired`). Check the outbound access
to WhatsApp. When the service restarts with the device still stored, it starts
in [`owner_mismatch`](#owner-mismatch); follow the steps there.

Pairing starts only from the admin route. A `pair_rejected` that you did not
cause means someone else used the admin token: see [Token leak](#token-leak).

### Token leak

**A client token.**

1. Revoke the client:
   `wawarden admin clients revoke --id <id> --token-file admin.token`. It
   fails from its next request.
2. Create a replacement with a new name.
3. Read the client's audit lines since the leak to see what it read. For a
   client with write chats, also see what it sent. The send limits and the
   first-contact rule bound what it could send, and the phone shows the
   messages as the owner's.

**The admin token.**

1. Replace it as in [Rotating the admin token](#rotating-the-admin-token).
2. Run `wawarden admin clients list --token-file admin.token`, and revoke
   every client you did not create.
3. Read the `admin_mutation` events since the leak: pairing, reconnects,
   backfills and client changes.
4. Check Linked devices on the phone.

### Failed admin authentication

`admin_auth_failure` reports failed authentications on the admin listener, with
their `count`. The first failure is reported at once. Further failures within
the next minute are reported together once the minute has passed.

- When you caused it, with a wrong or old token file, fix the file.
- Otherwise, something that can reach the admin listener holds a wrong token.
  Check who can reach it ([gate the admin listener](hardening.md#gate-the-admin-listener-separately)).
  If you suspect the token itself leaked, follow [Token leak](#token-leak).

The listener has one failure budget for all callers. While it is spent, a wrong
token is answered `429` `too_many_requests` instead of `401`, and an admin
command exits `4`. A valid token is still accepted.

### Uncertain sends

A `504` `send_outcome_unknown` means the message may have reached WhatsApp.
The service never sends it again, and every retry with the same key answers
`504`. The event `send_outcome_unknown` gives the reason: `ack_timeout`,
`lease_expired` or `restart`. Read the chat, through the API or on the phone,
before anyone sends anything else to it.

`send_unrecorded` means a send's outcome could not be stored, or the key of a
send that never left could not be freed. The key answers `409` until its
60-second lease ends, and `504` afterwards. Treat it like a `504`, and look for
the cause of the failed write, such as a full disk.

A `502` `send_rejected` is final: WhatsApp refused the message, or the
recipient cannot be reached. Retrying under the same key answers the same.

### Backup failures

`backup_failed` carries a reason ([backups](configuration.md#backups)). The
service retries after 15 minutes, then with waits that double up to 4 hours.

- `space`: free space or grow the volume. Old backups can be deleted by hand.
- `directory`, `output`, `rename`, `cleanup`: check that `backups/` and
  `backups/tmp/` are directories of the service's user with mode `0700`.
- `exists`: a file already has the backup's name; look at the clock.
- `archive`, `session`, `manifest`, `encrypt`, `sync`: usually the disk. Read
  the system's logs.
- `cancelled`: the service was stopping; nothing to do.

`backup_prune_failed` with `unexpected_entry` means `backups/` holds something
the service did not write. Until it is removed, no old backup is deleted.
`directory`, `remove` and `sync` point at permissions or the disk.

### Re-key conflicts

WhatsApp can move a direct chat from a phone number to a LID. The engine
refused such a mapping when `rekey_conflict` is reported, with `conflict`:

| `conflict` | The mapping was refused because |
|---|---|
| `mapping_contradicts` | It contradicts a mapping already learned. |
| `both_chats_have_messages` | It would merge two chats that both hold messages. |
| `message_collision` | Re-keying the sender would give two messages of one chat the same id. |
| `scoped_chat` | It would widen what a client reads: a client names only one of the two identities while the other holds messages, or the merge would remove a chat a client names. |

The message that carried the mapping is still applied. The number and the LID
stay apart, and no client's scope changes. The event is reported the first
time a mapping is refused since the start; `wawarden_rekey_conflicts_total`
counts every refusal.

For `scoped_chat`, an expired client counts until it is revoked. Revoke expired
clients whose chats hold a mapping back
(`wawarden admin clients list --token-file admin.token` shows them). See
[ingest](internals.md#ingest) for the rules.

### Dropped notifications

Every notification event is written on standard output first. The webhook only
copies it, so a dropped post loses no event from the output.

- `notify_failed` (`attempts`, `reason`, `status`): a post failed after its
  last attempt, got a final answer, or met a refused destination. `reason` is
  `destination_refused`, `http_status` or `network_error`. Check the receiver
  and the address that `WAWARDEN_NOTIFY_URL` resolves to.
- `notify_dropped` (`count`): at shutdown, the webhook dropped `count` events
  it had not delivered within the grace period, and `serve` exits `1`. Read
  those events from standard output.

`wawarden_notify_dropped_total` counts the drops by `reason`: `queue_full`,
`failed` and `shutdown`. See [webhook](observability.md#webhook).

### Other incidents

- **`ingest_paused`** (`free_bytes`, `floor_bytes`): the data directory fell
  below `WAWARDEN_MIN_FREE_BYTES`. Messages that arrive while ingest is paused
  are lost, not delayed ([delivery](internals.md#delivery)). Free space or
  grow the volume. Ingest resumes at 1.25 times the floor and logs
  `ingest_resumed`. The service never deletes backups to make room.
- **`quarantine`** (`queue`: `inbox` or `history`): an item failed three times
  and was set aside; `admin status` counts it. Read the log lines around it.
  If a crafted message could cause it, report it as
  [SECURITY.md](../SECURITY.md) describes.
- **`restart_budget`**: see [Disconnect reasons](#disconnect-reasons).
- **`db_lost`**: the connection holding a database's lock is gone, and
  `/healthz` answers `503`. Restart the service, and find out what removed or
  locked the file.
- **A history gap**: ask the phone for the 50 messages before the oldest one
  the archive holds:

  ```sh
  wawarden admin backfill --chat <chat> --count 50 --token-file admin.token
  ```

  At most one request every 5 minutes and 20 a day. `backfill_quota` and
  `backfill_no_anchor` exit `5`.
