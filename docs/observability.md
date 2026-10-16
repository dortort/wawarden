# Observability reference

WaWarden reports everything on standard output as JSON lines: log events,
notification events, audit lines and, optionally, metrics in CloudWatch's
embedded metric format (EMF). This page describes those lines, the webhook that also receives
notification events, the Prometheus endpoint, and the audit chain that records
every client change and request. Terms such as chain head, shipped head and
key id are explained in the [glossary](glossary.md).

## Contents

- [Logging](#logging)
  - [Protocol library logs](#protocol-library-logs)
  - [Pseudonyms and dropped lines](#pseudonyms-and-dropped-lines)
- [Notifications](#notifications)
  - [Webhook](#webhook)
- [Metrics](#metrics)
  - [Panic names](#panic-names)
  - [Embedded metric format](#embedded-metric-format)
- [Audit chain](#audit-chain)
  - [What verification proves](#what-verification-proves)
  - [Audit retention](#audit-retention)
  - [Audit key changes](#audit-key-changes)
  - [Audit growth](#audit-growth)

## Logging

The service writes JSON lines to standard output, one object per line. Every line
has these keys:

| Key | Content |
|---|---|
| `time` | RFC 3339 timestamp in UTC with fractional seconds |
| `level` | `DEBUG`, `INFO`, `WARN` or `ERROR` |
| `msg` | A human-readable sentence; it may change between releases |
| `event` | A stable, machine-readable event name; match on this, not on `msg` |

The [audit lines](#audit-chain) are the one exception: they carry the `event`
`audit` and their own fixed keys, with no `time`, `level` or `msg`.

`WAWARDEN_LOG_LEVEL` sets the lowest level written, with exceptions that ignore
the setting. The `startup_refused` line is written before the configuration is
accepted. The `dev_build`, `fake_engine`, `listener_not_loopback` and
`backup_disabled` warnings are written at every level, `error` included, because
they are the only signal that a development binary is running, that it runs the
fake engine, that a listener is reachable beyond loopback, or that no backup
is taken. So are the [notification events](#notifications), which an operator must see,
the `notify_failed` and `notify_dropped` warnings, and the `unsafe_debug` and `unsafe_debug_ended`
notices of a [debug window](#protocol-library-logs). [EMF](#embedded-metric-format)
lines have none of the four keys.

| Event | Level | Other keys | Written when |
|---|---|---|---|
| `startup_refused` | `ERROR` | `reason`, `error` | The configuration or the [master key](configuration.md#master-key) is refused; exit `2`. |
| `keys_loaded` | `INFO` | `key_id`, `retired` | The master key is loaded and the log pseudonyms are keyed with it; `key_id` is the 8-digit key id, never a key, and `retired` the number of [retired keys](configuration.md#master-key) loaded. |
| `memory_limit` | `INFO` | `source` (`env`, `cgroup` or `none`), `soft_limit_bytes` | At start, the Go runtime's soft memory limit in effect and where it came from (see [Container hardening](hardening.md#container-hardening)); `soft_limit_bytes` is 9223372036854775807 when no limit applies. |
| `db_lock_wait` | `WARN` | `database` (`archive` or `session`), `within` | Another process holds the lock of the archive or the device store; `serve` retries until `within` (`5m0s`) has passed. Logged once per database and start. |
| `archive_opened` | `INFO` | `schema_version`, `profile`, `ofd_locking`, `recent_starts` | The [archive](configuration.md#message-archive) is open and locked. `ofd_locking` is `true` when open-file-description locks are in use, and `recent_starts` counts the starts of the last ten minutes, this one included, at most 64. |
| `session_opened` | `INFO` | `paired` | The [device store](configuration.md#device-store) is open, locked and up to date; `paired` is `true` when it holds a linked device. |
| `fake_engine` | `WARN`, at every log level | `wrong_account` | A development build runs the [fake engine](internals.md#fake-engine) in place of WhatsApp and opens no device store; `wrong_account` is `true` when the fake pairs a number that is not the owner's. |
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
| `db_deadline` | `ERROR` | `database`, `operation`, `timeout_ms`, `goroutines` | A read of the archive ran beyond 2 seconds, a write beyond 10 or a rewrite of its full-text index beyond 5 minutes. It is written when the deadline passes, while the call still runs, and the call is interrupted and fails, unless it is waiting on the filesystem or committing, or the deadline passed just as a statement started; see [Message archive](configuration.md#message-archive). `operation` names the call in the code, and `goroutines` holds the goroutine profile of the process (function names and source positions, cut at 32 KiB). |
| `db_lost` | `ERROR` | `database` | The connection that held the lock of the archive or the device store is gone, and the service refuses to open another; `/healthz` answers `503` from then on. |
| `log_dropped` | `WARN` | `reason`: `xml` or `too_long` | Replaces a line that carried XML (`xml`) or was longer than 65,536 bytes (`too_long`); see [Pseudonyms and dropped lines](#pseudonyms-and-dropped-lines). It is written in place of a line that passed the log level, whatever that line's level was. |
| `unpaired` | `WARN`, at every log level | | A [notification event](#notifications). |
| `engine_state` | `INFO` | `state`, `reason` | The [engine's state](internals.md#engine-states) changed; `reason` is empty unless the state is `disconnected`. |
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
| `backup_pruned` | `INFO`, at every log level | `removed`, `kept` | A [notification event](#notifications). |
| `backup_prune_failed` | `WARN`, at every log level | `reason` | A [notification event](#notifications). |
| `backup_skipped` | `INFO` | `reason`: `unchanged` | Neither database was written since this process's last [backup](configuration.md#backups), so the nightly backup of this slot is skipped. Logged once per slot. |
| `audit_pruned` | `INFO` | `through`, `rows`, `anchor_head` | The nightly [audit retention](#audit-retention) deleted `rows` audit rows; the audit anchor now names row `through`, and `anchor_head` is the first 16 bytes of that row's HMAC, as its line's `chain_head` was. Not logged when it deleted none. |
| `audit_prune_refused` | `WARN` | `reason`: `chain_mismatch` | The nightly [audit retention](#audit-retention) found that the audit anchor, or a row it was about to remove, does not verify under the master key and the retired keys, so it moved and removed nothing more and tries again at the next slot; an `audit_pruned` line the same night reports what earlier batches removed; run [`audit verify`](cli.md#audit-verify). Logged once per slot. |
| `history_blobs_pruned` | `INFO` | `rows` | The nightly prune deleted `rows` [history blob](internals.md#history-sync) rows processed more than 30 days before. Not logged when it deleted none. |
| `ingest_resumed` | `INFO` | `free_bytes`, `floor_bytes` | The free space reached 1.25 times the floor again. |
| `free_space_unknown` | `WARN` | `error_type` | The free space of the data directory could not be read. |
| `index_rewrite_pending` | `WARN` | `operation` | A change committed, but the full-text index rewrite it requires failed; it stays due and runs at the next start. |
| `history_ack_failed` | `WARN` | `error_type` | Sending the history receipt failed; the blob is processed anyway. |
| `history_file_kept` | `WARN` | `error_type` | A history file could not be deleted, or the directory could not be flushed after a deletion. The engine deletes a file whose blob is no longer waiting at its next start and once an hour while it runs. |
| `expiry_sweep_failed` | `WARN` | `error_type` | Removing the text of expired messages failed; the sweep runs again a minute later. |
| `whatsmeow_log` | the line's own: `ERROR`, `WARN`, `INFO`, or `DEBUG` in a debug window | `module`, `detail` | A log line of the protocol library; see [Protocol library logs](#protocol-library-logs). |
| `unsafe_debug` | `WARN`, at every log level | `minutes`, `until` | `WAWARDEN_UNSAFE_DEBUG` opened a debug window at the start. |
| `unsafe_rate_caps` | `WARN`, at every log level | `read_per_minute`, `search_per_minute` | `WAWARDEN_UNSAFE_RATE_CAPS=1` lifted the upper caps of the [read budgets](api.md#read-rate-limits). Logged once per start. |
| `unsafe_send_caps` | `WARN`, at every log level | `send_global_per_hour`, `send_per_client_per_minute` | `WAWARDEN_UNSAFE_SEND_CAPS=1` lifted the upper caps of the [send limits](api.md#send-limits). Logged once per start. |
| `unsafe_debug_ended` | `WARN`, at every log level | | The debug window has passed, and the protocol library's debug output is discarded again. Logged at the first debug line after the window. |
| `audit_key_changed` | `INFO` or `WARN`, at every log level | `previous_key_id`, `key_id`, `previous_key` | A [notification event](#notifications). |
| `audit_key_unread` | `WARN` | `error_type` | At start, the key id of the newest audit row could not be read, so `audit_key_changed` was not checked. |
| `admin_mutation` | `INFO`, at every log level | `action`, `outcome` | A [notification event](#notifications). |
| `admin_auth_failure` | `WARN`, at every log level | `count` | A [notification event](#notifications). |
| `send_outcome_unknown` | `WARN`, at every log level | `reason` | A [notification event](#notifications). |
| `send_unrecorded` | `WARN`, at every log level | | A [notification event](#notifications). |
| `notify_failed` | `WARN`, at every log level | `attempts`, `reason`, `status` | The [webhook](#webhook) did not take an event, which is dropped. |
| `notify_dropped` | `WARN`, at every log level | `count` | At shutdown, the [webhook](#webhook) dropped `count` events it had not delivered, and `serve` exits `1`. |
| `emf_failed` | `WARN` | `error_type` | Writing the [EMF](#embedded-metric-format) lines failed. |
| `backup_disabled` | `WARN`, at every log level | | `WAWARDEN_BACKUP_AGE_RECIPIENT` is not set, or the [fake engine](internals.md#fake-engine) runs and opens no device store, so no [backup](configuration.md#backups) is ever taken. Logged once per start. |
| `backup_check_failed` | `WARN` | `error_type` | A step of the [backup](configuration.md#backups) scheduler failed: reading or recording whether a backup is due, the prune of old backups (also reported as `backup_prune_failed`), the [audit retention](#audit-retention) or the prune of history blob rows; the scheduler runs again 30 seconds later. |
| `audit` | none, written at every log level | `ts`, `client`, `action`, `chat_hmac`, `ok`, `reason`, `key_id`, `chain_head` | An [audit row](#audit-chain) was committed; the line has no `time`, `level` or `msg`. |

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

### Protocol library logs

The protocol library logs through an adapter into the same writer as every other
line, as `whatsmeow_log` events: `module` names the protocol library's component, such as
`whatsmeow/Client/Socket`, and `detail` holds its formatted text after five
changes. First every `http`, `https`, `ws` or `wss` URL becomes `[url]`, every
media direct path becomes `[path]`, and the value of any `hash`, `mms-type`,
`oh`, `oe`, `__wa-mms` or `_nc_`-prefixed query parameter left over becomes
`[param]`, so that the protocol library's media download warnings carry no
media location or encrypted-file hash. A direct path is a path of the media
servers' `/v/t<digit>`, `/o1/v/t<digit>` or `/m1/v/t<digit>` shape, a slash-led
word that carries a query, or the rest of the text after the protocol library's
error for a path that does not start with a slash; quotes and trailing
punctuation around a URL or path are kept. Then every client or admin token in
it is redacted as the writer does (see [Pseudonyms and dropped lines](#pseudonyms-and-dropped-lines)), so
that masking its digits cannot leave the rest of its secret unrecognised. Then
every run of six or more digits, with `.` and `:` between them, that is
not directly followed by `@` becomes `[number]`, so that a phone number or a
user-and-device address outside the `user@server` form does not pass; a run
followed by `@` is an identifier, which the writer turns into a pseudonym. Then
text beyond 1 KiB is cut at the last white space before that limit and ends in
` [truncated]`, so that an error quoting a server's response stays short and no
identifier is cut in half; a text without white space in its first 1 KiB becomes
`[truncated]`. Finally the writer pseudonymises identifiers and drops lines
carrying XML, such as the protocol library's dumps of protocol stanzas, as for any other
line. Its error, warning and information lines follow `WAWARDEN_LOG_LEVEL`,
during a `WAWARDEN_UNSAFE_DEBUG` window too.

Its debug output is discarded at every log level unless `WAWARDEN_UNSAFE_DEBUG`
opens a window, in minutes, at the start. During the window the protocol library's debug
lines are written at level `DEBUG`, whatever `WAWARDEN_LOG_LEVEL` is set to; the
start logs `unsafe_debug` with the window's length and end, and the first debug
line after the window logs `unsafe_debug_ended` instead and closes it for good.
The protocol library's debug lines name contacts and groups, carry push names and dump
every stanza it sends and receives: the writer drops the stanza dumps and
pseudonymises identifiers, but a push name or other text it does not recognise
reaches the log. Open a window only to diagnose a fault, keep it short, and treat
the log of that period as holding chat data. The protocol library's second logging
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
changes it in the four ways below before it is written. The writer writes a line
only when its newline arrives, so a line written in several pieces is checked as
a whole, and a last line without a newline is never written. The loggers that
feed the writer also write values fail-closed, as [Values are written fail-closed](#values-are-written-fail-closed) describes.

#### Tokens are redacted

Every part of a line with the exact shape of a client
token, `ww_`, the 8-character client id, `_`, 43 characters of `A` to `Z`, `a` to
`z`, `0` to `9`, `_` and `-`, `_` and 8 lower-case hexadecimal digits, becomes
`ww_<client id>_[redacted]`, and every part with the shape of an admin token,
`wwadm_` and the same 43 characters, `_` and 8 hexadecimal digits, becomes
`wwadm_[redacted]`. This runs before identifiers become pseudonyms, wherever
the token stands in the line, also glued to other text, and the
[protocol library's lines](#protocol-library-logs) get it before their numbers
are masked. A token cut short, or otherwise not of that exact shape, is written
as it is: nothing guesses at partial tokens.

#### Identifiers become pseudonyms

Every part of a line shaped like a WhatsApp
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
HMAC-SHA256, under the `log-redact` key derived from the [master key](configuration.md#master-key),
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

#### Lines carrying XML are dropped

A line carries XML when it contains:

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

#### Overlong lines are dropped

A line longer than 65,536 bytes, not counting its
newline, is replaced by the same fixed line with `"reason":"too_long"`, whether it
was written at once or in pieces.

#### Values are written fail-closed

In the events above every value is a
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
| `unpaired` | `WARN` | | The engine starts without a paired device, so it makes no connection until pairing is requested, also when the [restart budget](internals.md#engine-states) holds the start, or a paired device is lost: WhatsApp logged it out, or a rejected device was logged out. |
| `disconnected` | `WARN` | `reason` | The engine entered `disconnected` for any reason but `shutdown`; `reason` is one of the [disconnect reasons](runbook.md#disconnect-reasons). |
| `pair_rejected` | `WARN` | `stage`: `before_save`, `after_pairing` | Pairing linked or tried to link an account other than the owner's; see [Pairing](internals.md#pairing). |
| `logout_failed` | `WARN` | `attempt`, `error_type` | Logging out a rejected device failed; the engine tries again. `error_type` is the Go type of the error. |
| `quarantine` | `WARN` | `queue`: `inbox`, `history`; `attempts` | An inbox row or a history blob was set aside after three failed attempts. |
| `rekey_conflict` | `WARN` | `conflict`: `mapping_contradicts`, `both_chats_have_messages`, `message_collision`, `scoped_chat` | A LID mapping was refused; the event that carried it is still applied. Reported once per refused mapping, as described under [Ingest](internals.md#ingest). |
| `ingest_paused` | `WARN` | `free_bytes`, `floor_bytes` | The data directory fell below its [free-space floor](configuration.md#free-space). |
| `admin_mutation` | `INFO` | `action`: `pair`, `reconnect`, `backfill`, `client_create`, `client_revoke`; `outcome` | A `POST` [admin route](api.md#admin-routes) was called with the admin token; `outcome` is `ok` or the error code it answered, such as `already_paired`, `name_taken` or `invalid_body`. The event never names the client: the [audit chain](#audit-chain) records its id. |
| `backup_done` | `INFO` | `bytes`, `archive_bytes`, `session_bytes`, `duration_ms` | A [backup](configuration.md#backups) was written: `bytes` is the size of the encrypted file, the others the sizes of the two database copies and the time it took. |
| `backup_failed` | `WARN` | `reason` | A [backup](configuration.md#backups) failed and what it had written was removed; `reason` as in the table under [Backups](configuration.md#backups). It is reported on every failure, retries included. |
| `backup_pruned` | `INFO` | `removed`, `kept` | After a successful [backup](configuration.md#backups), backups beyond `WAWARDEN_BACKUP_KEEP` were deleted: `removed` files, `kept` remain. Not reported when nothing was deleted. |
| `backup_prune_failed` | `WARN` | `reason`: `directory`, `unexpected_entry`, `remove`, `sync` | Deleting old backups failed or was refused; see [Backup retention](configuration.md#backup-retention). |
| `audit_key_changed` | `INFO` when `previous_key` is `retired`, `WARN` otherwise | `previous_key_id`, `key_id`, `previous_key`: `retired`, `missing` | At start, the newest [audit row](#audit-chain) carries the key id `previous_key_id`, not the master key's `key_id`, as after [`keys rotate`](cli.md#keys-rotate) or a replaced master key. `previous_key` is `retired` when that key is in `keys/retired/`, and `missing` when it is not, so `audit verify` reports the rows written under it as `unknown_key` unless it is given a copy. Reported at each start until a row is written under the master key. |
| `send_outcome_unknown` | `WARN` | `reason`: `ack_timeout`, `lease_expired`, `restart` | A [send](api.md#sending) may have reached WhatsApp without an acknowledgement: the engine call ended without one (`ack_timeout`, which covers a timeout, a drop after the message was written and any error the engine cannot place), a request found its key still reserved past the 60-second lease (`lease_expired`), or the start found a send left running by the previous process (`restart`, one event per send). The send is stored as failed and never resent. |
| `send_unrecorded` | `WARN` | | The outcome of a [send](api.md#sending) could not be stored after three retries, or arrived after its lease had been settled, or the key of a send that never left could not be freed after three retries: the key answers `send_outcome_unknown` once its lease has ended, although the message may have been delivered. |
| `admin_auth_failure` | `WARN` | `count` | Authentication failed on the admin listener. The first failure is reported at once with `count` `1`; further failures within the next minute are held back, and reported together once the minute has passed, within ten seconds, or at shutdown. `count` is the number of failures since the previous `admin_auth_failure`. |

No event carries an identifier, a name, message text, a token, a key or a
pairing code. Every value is a number or a fixed code: a value that is not a
lower-case code of at most 64 characters (or, for `error_type`, a Go type name,
and for `previous_key_id` and `key_id`, 8 lower-case hexadecimal digits) is
written as `invalid`. A key id is the 4-byte `key-id` value derived from a
master key, already logged by `keys_loaded`; it is not a key. For example:

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

#### Verifying a delivery

A receiver should, before it acts on a request:

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

#### Webhook delivery

Events wait in a queue of 64 for one sender, so that a slow or
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
queued within the [grace period](cli.md#shutdown), then gives up on the rest: it
cancels the attempt in progress, logs `notify_dropped` with the number of
events dropped at shutdown, and `serve` exits `1`.

## Metrics

Metrics are served on the admin listener, at `GET /metrics`, with the admin
token. The format is the Prometheus text exposition format 0.0.4
(`Content-Type: text/plain; version=0.0.4; charset=utf-8`), sorted by metric name.
With `WAWARDEN_METRICS_EMF=1` the main ones are also written on standard output
in [EMF](#embedded-metric-format), which needs no token.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wawarden_auth_failures_total` | counter | | Failed authentications on the client listener, including those answered `429`. |
| `wawarden_admin_auth_failures_total` | counter | | Failed authentications on the admin listener, including those answered `429`. |
| `wawarden_policy_denials_total` | counter | | Client requests that the client's grant did not allow. Always `0` in this build: the client API resolves a chat outside the scope and a missing chat by the same query, so it cannot count one apart from the other; a send to a chat the client reads but may not write to is counted in `wawarden_sends_rejected_total` as `scope`. |
| `wawarden_sends_rejected_total` | counter | `reason` | [Sends](api.md#sending) refused: `scope` (a chat the client reads but may not write to), `first_contact`, `budget_minute`, `budget_hour`, `budget_global`, `pacer` (the pacer, or another send that was running). Every reason is present from the start, at `0`. Other refusals, such as a malformed text or key, are not counted. |
| `wawarden_panics_total` | counter | `name` | Panics recovered, by handler or goroutine name. Absent until the first panic. |
| `wawarden_notify_dropped_total` | counter | `reason` | Notification events the [webhook](#webhook) did not deliver: `queue_full`, `failed`, `shutdown`. |
| `wawarden_build_info` | gauge | `version`, `revision`, `dev` | Always `1`; the labels describe the running binary. |
| `wawarden_backup_last_success_timestamp_seconds` | gauge | | Unix time at which the newest successful [backup](configuration.md#backups) began, read from the archive at start; `0` until a backup succeeds. It is not written to the [EMF](#embedded-metric-format) lines. |

The [WhatsApp engine](internals.md#whatsapp-engine) adds these:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wawarden_paired` | gauge | | `1` while a device is paired, `0` otherwise. |
| `wawarden_connected` | gauge | | `1` while the engine is `connected`, `0` otherwise. |
| `wawarden_messages_ingested_total` | counter | | Messages, reactions and poll votes stored in the archive, live and from history. |
| `wawarden_ingest_dropped_total` | counter | `reason` | Events dropped by an [ingest rule](internals.md#ingest) or a [history-sync](internals.md#history-sync) check: `chat_rejected`, `sender_rejected`, `invalid`, `unknown_kind`, `no_target`, `foreign_reference`, `target_unknown`, `target_kind`, `stale_edit`, `not_original_sender`, `not_admin`, `admin_unknown`, `owner_unknown`, `not_paired`, `too_large`, `history_not_primary`. |
| `wawarden_ingest_refused_total` | counter | `reason` | Events the engine refused to write and left unacknowledged, which loses them (see [Delivery](internals.md#delivery)): `backlog_full`, `paused`, `store_error`. |
| `wawarden_ingest_quarantined_total` | counter | `queue` | Inbox rows (`inbox`) and history blobs (`history`) quarantined. |
| `wawarden_rekey_conflicts_total` | counter | `conflict` | LID mappings refused, by conflict as in `rekey_conflict`. |

Labelled counters appear once they count their first event.

Anything that scrapes `/metrics` holds the full admin token. That token can
also start pairing, ask for a reconnect, read the status, list the chats with
their identifiers and names, create and revoke clients, and request backfills.
Treat a scrape configuration as holding the admin credential, and prefer the
EMF lines, which need no token, for alerting.

### Panic names

Panic names: `api.client`, `api.admin` and `api.health` for the handlers,
`api.mcp` for an MCP tool call;
`listeners.client`, `listeners.admin`, `listeners.health`, `listeners.shutdown`
and `signals` for goroutines. The engine adds `engine.supervisor`,
`engine.ingest` and `engine.logout`; the first two also count a panic of one
version fetch, connection attempt, or pairing's dial or code request
(`engine.supervisor`) or of one inbox row's or history blob's attempt
(`engine.ingest`), which the engine treats as a failed attempt. A panic while a
reconnect closes the connection being dialled also counts as
`engine.supervisor`, and the reconnect goes ahead. Its adapter adds `wa.connect`, which ends a connection attempt whose
caller gave up, `wa.keepalive`, which closes a connection whose keepalives
failed or that WhatsApp asked to log in again, `wa.event` for a panic while
it translates one of the protocol library's events or the engine handles it,
which refuses the event, and `wa.media.watchdog` for a panic in the watchdog
that times one attachment download, which ends that download. Other code on
goroutines that the protocol library starts itself is not covered: a panic there ends the process. The notifier adds
`notify.flush` and `notify.webhook`, the EMF writer
`metrics.emf`, the [backup](configuration.md#backups) scheduler `backup.schedule`, and each
database call the watcher `db.deadline`, which logs a call that runs past its
deadline. In development builds, the [fake engine](internals.md#fake-engine) adds
`fake.connection`.

### Embedded metric format

With `WAWARDEN_METRICS_EMF=1`, the service writes metrics in CloudWatch's
[embedded metric format (EMF)](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format_Specification.html)
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

A metric that also has a label, in this build `Panics` by `name` and
`SendsRejected` by `reason`, is additionally written, for each label value whose count grew, in a line of its
own with that label as its only dimension. The dimensionless line holds the
total, and no line carries the metric twice, so an alarm without dimensions
matches the total:

```json
{"Panics":1,"_aws":{"Timestamp":1791201600000,"CloudWatchMetrics":[{"Namespace":"WaWarden","Dimensions":[["name"]],"Metrics":[{"Name":"Panics","Unit":"Count"}]}]},"name":"engine.ingest"}
```

These lines have no `time`, `level`, `msg` or `event` key. The first line counts
from the start of the process; a restart starts the counts again.

## Audit chain

The archive's `audit` table records:

- every change of a client: creating one (`client_create`) and revoking one
  (`client_revoke`, once; repeating it records nothing);
- every request a client makes to the [client API](api.md#read-audit) or the
  [MCP endpoint](api.md#mcp-audit) that its read budget admits.

Failed authentications are not recorded. A row holds:

- an id one more than the previous row's, and the time in milliseconds;
- the client id, the action, the chat (none for client changes) and its
  `chat-hmac` value;
- whether the action was allowed, and a reason code (`ok` for client
  changes);
- the peer address when one is known;
- the key id: the 4-byte value, written as 8 hexadecimal digits, that names
  the master key the row was written under (see
  [Master key](configuration.md#master-key));
- the row's HMAC, HMAC-SHA256 under the `audit-chain` key derived from the
  master key, over the row and the previous row's HMAC. The exact layout is
  under [Audit row HMAC](internals.md#audit-row-hmac).

A client change's row is appended inside the same write transaction as the
change, so a change whose row cannot be written does not happen. A client
request's row is appended in a write transaction of its own before the answer
is sent, waiting at most 2 seconds for the connection. A request whose row
cannot be written is answered `503` (`busy`) when that wait ran out and `500`
(`internal_error`) otherwise. Triggers refuse to update a row, to delete one
that the audit anchor does not cover or that is the newest row, or to insert
one with the id of an existing row, which an `INSERT OR REPLACE` or a
`REPLACE` would otherwise overwrite.

After the transaction commits, the service writes one line on standard output,
through the same scrubbing writer as every log line:

```json
{"event":"audit","ts":"2026-10-06T12:00:00.000Z","client":"aaaqeaye","action":"client_create","chat_hmac":null,"ok":true,"reason":"ok","key_id":"6a53922c","chain_head":"3fa8c2d14be0917a5c6e2b8f0d4a7c19"}
```

- `chat_hmac` is the first 16 bytes, in hexadecimal, of HMAC-SHA256 under the
  `chat-hmac` key over the chat's canonical identifier, or `null`.
- `key_id` is the id of the key the row was written under.
- `chain_head` is the row's chain head: the first 16 bytes of its HMAC. A
  chain head that reaches a log store off the host is a shipped head.

The line names no chat, person or message, and the peer address stays in the
archive.

### What verification proves

[`audit verify`](cli.md#audit-verify) recomputes every row's HMAC from a copy
of the archive, the master key and the retired keys, each row under the key
its key id names. It reports:

- a row whose content, HMAC or id sequence was changed;
- a row removed from the middle, as an `id_gap` on the next row;
- a row written under a key it was not given;
- a row written under a retired key after one written under the master key;
- an audit anchor that was changed, forged without a key, or removed with the
  rows after it, as an `id_gap` on the first retained row.

The triggers do not stop someone who holds the database file. Whoever holds
`keys/master` can recompute the whole chain after changing it, or remove rows
from its end without leaving a trace in the table. The chain heads on standard
output are the defence against both: ship the service's standard output to a
store the host cannot rewrite, and give a capture of it to
`audit verify --log`, which reports every shipped head the copy lacks. Rows
removed from the end of the chain are detectable only this way.

Whoever holds a retired key can write rows under it in place of every row
under the master key, once all of those are older than the retention's
cutoff. Such rows are detectable this way only in a backup taken before the
removal that reached them, which signs them (Audit anchoring in the
[threat model's residual risks](threat-model.md#residual-risks)). The master
key and the retired keys are not part of a
[backup](configuration.md#backups): keep a copy of them apart from the backups
to verify them.

### Audit retention

Each night the service removes the audit rows written more than
`WAWARDEN_AUDIT_RETENTION_DAYS` days before (90 by default, at least 7). When
it runs:

- once the [backup](configuration.md#backups) of the night's slot is taken,
  and never while that backup is still owed. A slot skipped because nothing
  changed counts as taken;
- at once at the slot when no backup recipient is configured;
- also for the first time ten minutes after the backup scheduler's first check
  after a start.

With a backup recipient, nothing is removed before the first backup succeeds:
not while no device is paired, while the history sync settles, or while the
first backup fails and is retried.

How it removes rows:

- It works from the oldest row, in transactions of at most 10,000 rows. Each
  transaction first moves the audit anchor to the last row it removes, then
  deletes the rows up to it, never the newest row.
- It stops when no older row is left or after 15 minutes, and goes on the next
  night.
- Rows are taken in id order, so a row whose time is a few seconds out of
  order goes with its neighbours.

The anchor keeps:

- the last removed row's id, key id and HMAC;
- the anchor's time: the latest time of the rows removed so far. An
  out-of-order row can make it later than the last row's, but never more than
  10 seconds after the removal's cutoff. That is the archive's write timeout,
  and so the longest a row's time can precede its commit;
- the time of the removal and the id of the current master key;
- an HMAC-SHA256 under that key's `audit-chain` key, laid out as under
  [Audit anchor HMAC](internals.md#audit-anchor-hmac).

Before it moves the anchor, each transaction checks what it signs with the
master key and the retired keys, by the rules of
[`audit verify`](cli.md#audit-verify):

- the MAC of the anchor already there, with the key it names;
- that the first row behind an anchor made under a retired key was not
  written under the master key;
- the HMAC of every row it is about to remove, from the anchor's HMAC (or from
  the chain's start), under the key the row names, with consecutive ids and no
  row under a retired key once the anchor or an earlier row names the master
  key.

When a check fails, the transaction moves and removes nothing. The service
logs `audit_prune_refused` with reason `chain_mismatch`, and the retention
stops until the next night, when it checks again. Rows under a key that is
neither the master key nor in `keys/retired/` are never removed. The check
reads each row once more: about 40 ms for 10,000 rows. Removing 1,000,000
rows took 23 seconds instead of 19, measured on an SSD.

Triggers refuse to move the anchor back or to remove it. One `audit_pruned`
line reports each night's removal. The verification starts from the anchor. A
shipped head whose time is not after the anchor's counts as pruned, not
missing, unless the anchor was made under a retired key and the head's line
names the master key or no key. Those rows can be checked only in a backup
taken before they were removed, which is why the retention waits for the
night's backup.

### Audit key changes

After [`keys rotate`](cli.md#keys-rotate) or a replaced master key, rows keep
the key id they were written under. Each start with the new key emits
`audit_key_changed` (see [Notifications](#notifications)) until a row is
written under it. `audit verify` needs the retired keys to verify the older
rows and an anchor made under an older key. So does the retention, which
removes no row it cannot verify; the next removal makes the anchor again
under the current key. While the anchor is under an older key, the head of a
removed row on a line without a `key_id` counts as missing.

### Audit growth

A row takes about 150 bytes of the archive, measured over ten
million rows. Client changes add a handful of rows; a client that reads
continuously at the default read budget of 600 requests a minute adds 864,000
rows, about 131 MB, a day, and each further client as much again, so the
default retention of 90 days holds up to about 12 GB per such client; at 6
requests a minute it is about 118 MB. A request refused by the read budget
adds no row. The archive file does not shrink when rows are removed: later
rows reuse the freed pages.
