# Internals

This page explains how WaWarden works inside: the WhatsApp engine and its
adapter to the protocol library (`go.mau.fi/whatsmeow`), how messages are
ingested and delivered, history sync, and what the protocol library sends on
its own. It also holds the archive's schema, the byte layouts behind the
audit chain's HMACs, the memory bounds, and the reasons behind some design
choices. The operational reference is in [configuration.md](configuration.md),
[cli.md](cli.md), [api.md](api.md) and [observability.md](observability.md);
what to do when something goes wrong is in the [runbook](runbook.md). Terms
such as LID and re-key are explained in the [glossary](glossary.md).

## Contents

- [WhatsApp engine](#whatsapp-engine)
  - [Engine states](#engine-states)
  - [Pairing](#pairing)
  - [Ingest](#ingest)
  - [Delivery](#delivery)
  - [History sync](#history-sync)
  - [Media transport](#media-transport)
  - [Attachment downloads](#attachment-downloads)
  - [Attachment staging](#attachment-staging)
  - [Traffic the protocol library sends by itself](#traffic-the-protocol-library-sends-by-itself)
  - [What a send does on the wire](#what-a-send-does-on-the-wire)
  - [Fake engine](#fake-engine)
- [Archive schema](#archive-schema)
- [Full-text index rewrites](#full-text-index-rewrites)
- [Memory bounds](#memory-bounds)
- [Byte layouts](#byte-layouts)
- [Design rationale](#design-rationale)

## WhatsApp engine

The engine links the service to WhatsApp as a companion device, keeps the
connection, and writes what arrives into the [message archive](configuration.md#message-archive).
It can also send a text message and ask the owner's phone for older history,
as [What a send does on the wire](#what-a-send-does-on-the-wire) describes.
It talks to WhatsApp through an adapter over the WhatsApp protocol library,
`go.mau.fi/whatsmeow`, which keeps the linked device in the
[device store](configuration.md#device-store).

Without a paired device the engine makes no connection of any kind, not even
the version fetch below: it stays `unpaired` and reports the
[notification event](observability.md#notifications) `unpaired` once, until
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
| `connected` | The engine is connected and receiving. Only in this state, with a paired device and a connection that the protocol library reports as logged in, does it send a message or a backfill's history request. |
| `disconnected` | The engine is not connected and does not reconnect on its own. The reason is one of `outdated`, `replaced`, `logged_out`, `temporary_ban`, `cat_refresh`, `connect_failure`, `restart_budget`, `owner_mismatch` or `shutdown`. |

Every change is logged as `engine_state`; entering `disconnected` for any reason
but `shutdown` also reports the notification event `disconnected`. The gauges
`wawarden_paired` and `wawarden_connected` follow the state.

#### Disconnect reason codes

A `disconnected` engine names its reason in `engine_state`, in the
`disconnected` event and in `admin status`. The runbook's
[Disconnect reasons](runbook.md#disconnect-reasons) is the one list of every
reason, what it means and what to do. The behaviour behind some of them is
below:

- `restart_budget`: [Restart budget](#restart-budget).
- `outdated`: [Protocol version](#protocol-version).
- `replaced`, `temporary_ban`, `cat_refresh`, `connect_failure` and
  `logged_out`: [Stops that wait for the operator](#stops-that-wait-for-the-operator).
- `owner_mismatch`: [Pairing](#pairing).

#### Restart budget

When the archive has recorded more than 5 starts in the
last ten minutes, this one included (`recent_starts` in `archive_opened`), the
engine starts in `disconnected` with reason `restart_budget` and makes no
connection, so that a crash loop does not become a reconnect storm; a stored
device that fails the [owner check](#pairing), which comes first, starts it in
`owner_mismatch` instead. A start
recorded at a later time than the current one, as after the system clock was
set back, is forgotten, so it cannot hold the engine in `restart_budget`
until the clock catches up.

#### Protocol version

When a device is stored, passes the [owner check](#pairing) and is within the
restart budget, the engine first fetches the current WhatsApp Web
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

#### Reconnection

After an ordinary drop or a failed connection attempt, the
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
pairing, the adapter closes the connection instead of letting the protocol library
re-dial, and that counts as a drop. A connection whose keepalives have failed
for 3 minutes is closed and counts as a drop too.

#### Stops that wait for the operator

After `replaced`, `temporary_ban`, `cat_refresh` or `connect_failure`, the
engine stays disconnected until an explicit reconnect
(`POST /admin/v1/reconnect`). After `logged_out`, it waits for pairing
(`POST /admin/v1/pair`). A connection event that arrives late, after one of
these, changes nothing.

### Pairing

Pairing links a new device by a code that the owner enters on the phone.
`wawarden admin pair` requests it through the [admin route](api.md#admin-routes)
`POST /admin/v1/pair`. It is refused while a device is paired
(`already_paired`), while `WAWARDEN_OWNER_PHONE` is unset
(`owner_phone_missing`), and after three attempts within the last hour
(`rate_limited`); a refused or failed request counts as an attempt once it
passed the first two checks. The engine connects, waits up to 30 seconds for
WhatsApp to offer a login session, and asks for a code for the number in
`WAWARDEN_OWNER_PHONE` and no other, with a notification on the phone that
names a `Chrome (Linux)` device. The device registers as `WaWarden` followed
by the operating system it runs on, such as `WaWarden (Linux)`, and Linked
devices lists it under that name; the name is fixed when the device pairs.
The route gives the engine 25 seconds in all, so that its answer leaves
before the listener's 30-second write timeout; a code that does not arrive in
time fails the request with `pair_failed`. WhatsApp
also sends login QR codes, which the adapter drops at once without keeping
them. The pairing code goes to the caller only: it is never logged, counted,
kept or put into an event. Before the protocol library saves anything, the
adapter refuses an account whose number is not exactly the one in
`WAWARDEN_OWNER_PHONE`: the protocol library sends WhatsApp a pairing error and stores
nothing, and the engine reports `pair_rejected` with `stage` `before_save`; it
takes that report only while no device is stored, so one that arrives late
cannot unpair the owner's device. When pairing fails in the protocol library for another
reason, such as an answer from WhatsApp that fails its checks, a device that
cannot be saved or a confirmation that cannot be sent, the protocol library closes the
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
arrives, before the engine has written it (see [Delivery](#delivery)). So is
each change to the names the owner saved for contacts.

The engine refuses to write a message, a group change or a saved name when:

- the inbox already holds 5,000 rows that wait to be applied;
- ingest is [paused](configuration.md#free-space);
- the write fails.

Each case counts in `wawarden_ingest_refused_total`, and a refused message,
group change or saved name is lost (see [Delivery](#delivery)).

A chat is a phone-number user (`<digits>@s.whatsapp.net`), a LID user
(`<digits>@lid`) or a group (`<id>@g.us`). A LID is the identifier WhatsApp
gives an account in place of its phone number; see the
[glossary](glossary.md#lid). These are acknowledged and dropped before they reach
the inbox:

- traffic of a chat of any other kind: status updates, broadcast lists,
  newsletters and every other kind;
- every message, group change, saved contact name and history-sync
  notification that arrives while the engine is `unpaired`, while no device is
  stored (such as after WhatsApp logged the device out), while a device that
  [pairing](#pairing) rejected is still stored, or while the stored device is
  not the owner's (`owner_mismatch`). All of these count as `not_paired`;
- every message whose identifiers, push name, text and quoted text add up to
  more than 512 KiB (`too_large`). [Memory bounds](#memory-bounds) gives the
  memory this cap implies.

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
| The chat must be a phone-number user, a LID user or a group; a saved contact name must belong to a phone-number or LID user. | `chat_rejected` |
| The sender must be a phone-number or LID user. | `sender_rejected` |
| An event of no known kind is dropped; in particular it never counts as a revocation. | `unknown_kind` |
| An edit, revocation, reaction or poll vote names its target by a key. A key without a message identifier, or a group key without the target's sender, is dropped. | `no_target` |
| A key, or the reference of a reply, that names a chat is followed only when it names the chat the event arrived in, or, in a direct chat, the owner's own number, which is how the other side names that chat. Any other chat is never followed: the event is dropped, and a reply is stored without its reference. | `foreign_reference` |
| The target is looked up only inside the event's chat, by its identifier and its sender; in a direct chat, a key that does not name the event's sender as the author names the other side. | `target_unknown`, and `owner_unknown` when the other side is the owner and `WAWARDEN_OWNER_PHONE` is unset |
| An edit or revocation must come from the target's sender. In a group, a revocation by someone else is applied only when the archive records that member as an admin of the group; a member it does not record is refused. | `not_original_sender`, `not_admin`, `admin_unknown` |
| An edit needs new text and a target that is neither revoked, a reaction nor a poll vote. | `invalid`, `target_kind` |
| An edit must be newer than the last edit applied to its target, so that an edit delivered late or again never brings back text the sender has since replaced. | `stale_edit` |

A revocation whose target is not in the archive yet, and that passes the check
a stored target would get (it comes from the target's sender, or in a group
from a member the archive records as an admin), is counted as `target_unknown`
and leaves a tombstone for 7 days, keyed by the chat, the target's identifier
and the canonical identifier of its sender. A copy of that message from that
sender stored within those days, live or from history sync, is stored already
revoked, without text, media type or media reference. Any other revocation of
an absent target, and every edit or reaction of one, is dropped
(`target_unknown`, with the check's own reason for a revocation it refuses) and
not kept for later. When history sync stores the target afterwards, as it can
during the initial sync, because live traffic is applied while blobs wait for
their download, an edited target keeps its earlier wording, and a revoked one
keeps its text when its tombstone has lapsed, or was dropped as the oldest
beyond 100,000, or when the canonical identifier of its sender changed in
between, as when the archive learns the sender's LID (see the
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
- stores the full and first name the owner saved for a contact when the owner's
  phone syncs a change to its contact list to this device, under the contact's
  canonical identifier, and nowhere else: a saved name names no chat and
  replaces no push name. The latest change replaces the stored name, and a
  change that carries no name clears it. The protocol library passes on only
  the changes it fetches one by one, such as a contact added or renamed after
  the device has synced the contact list, never a list it fetches whole, as it
  does the first time after pairing, and no deletion, so a contact deleted on
  the phone keeps its last saved name. Tests cover this with synthetic events
  only. Whether the phone sends these changes to a linked device, and under
  which identifier, is unverified against a live account;
- stores `text_display`, the text without control, bidirectional, zero-width and
  tag characters, beside the text;
- stores whether a message came live or from history sync, and whether the
  owner sent it;
- sets the expiry of a message sent with a disappearing timer; every minute,
  also between the rows and batches it applies, the engine removes the text,
  media type and media reference of expired messages from the archive as
  described under
  [Message archive](configuration.md#message-archive), and in the same
  transaction purges media references past their expiry or older than the
  retention, every media reference while downloads are off, and revoke
  tombstones past their 7 days or beyond the newest 100,000;
- learns which LID belongs to which phone number only from the alternate
  identifiers WhatsApp's servers attach to live messages and from history
  sync, and re-keys the direct chat (see [Re-keys](#re-keys) below).

#### Re-keys

A re-key moves a direct chat from the phone number to the LID. In the same
transaction it moves every client's read and write chats, and the contact's
push name and saved name, from the number to the LID. Where both identities
have a saved name, the newer is kept.

The engine refuses a mapping that:

- contradicts one already learned;
- would merge two chats that both hold messages;
- would widen what a client reads: a client that names only one of the two
  identities while the other holds messages, or a merge that would remove a
  chat a client names.

Revoked clients do not count for the last rule, but an expired client counts
until it is revoked: revoke an expired client whose chats hold a mapping
back. A refused mapping is counted in `wawarden_rekey_conflicts_total` every
time. It is reported as the [notification event](observability.md#notifications)
`rekey_conflict` the first time the engine refuses it since the service
started. The engine remembers up to 1,024 refused mappings, and forgets them
all when that many are reached.

### Delivery

**A message refused or interrupted before it is stored is lost, not
redelivered.** Once written to the inbox, a message is applied to the archive,
after a restart if need be, or [quarantined](#ingest) when applying it keeps
failing; before that, WaWarden gets one chance at it.

The protocol library decrypts a message, which advances and saves its session
keys in `session.db`, and then hands it to the engine. Only once the engine has
written the message to the inbox does the protocol library acknowledge it to WhatsApp and
send the delivery receipt. This gives:

- A message the engine wrote reaches the archive even when the process stops
  before applying it: the inbox is applied again at the next start.
- A message the engine refuses (a full inbox, a pause or a failed write), or
  whose handling panics in the adapter or the engine, is not acknowledged and gets no delivery receipt. WhatsApp sends it again later, but
  the protocol library can no longer decrypt that copy, because its keys
  have moved on: it drops it without passing it on, then acknowledges it and
  sends the delivery receipt. A refused message is therefore lost to the archive.
  So is a history-sync notification refused during a pause, whose blob is then
  never downloaded.
- A message is lost the same way when the process stops after the protocol library
  decrypted it and before the engine wrote it.
- The protocol library acknowledges a group change as it arrives, whatever the engine
  answers, so a refused group change is lost as well. The protocol library never
  delivers a change to the owner's contact list again either, so a refused
  saved name is lost until the owner changes that contact again.
- The protocol library can keep each decrypted message in `session.db` until the engine
  confirms it, which would let a refused or interrupted message be decrypted
  again. WaWarden leaves that off, so that `session.db` holds no message
  plaintext (see [Design rationale](#the-device-store-holds-no-message-plaintext)),
  and an architecture test refuses the call that would.

What a loss costs, and what limits it:

- Only the archive misses the message: the owner's phone and the account's
  other linked devices receive their own copies.
- The inbox is written before the acknowledgement, in the same file as the
  archive, so a crash or a stop after that write does not lose the message.
- The [free-space floor](configuration.md#free-space) pauses ingest before a full disk makes
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

The owner's phone answers the history request of a
[backfill](#what-a-send-does-on-the-wire) with a notification of the same kind, which the engine accepts only from the
primary device and takes up the same way. The archive's record of every
notification keeps its sync type and the request id it names, if any. When the
engine marks an on-demand blob, the answer to a backfill, processed, it marks
the backfill with that request id complete in the same transaction; a blob of another sync type, or one
that names no request, completes none.

The download goes through the protocol library, which asks WhatsApp for the
media hosts, fetches the encrypted blob over an HTTP client of the adapter's,
with timeouts, no proxy, no redirect and a response cap of
`WAWARDEN_HISTORY_MAX_BYTES` plus 32 bytes for the encryption's padding and
tag, checks its hashes and decrypts it. Each request for the blob ends after
2 minutes, inside the client's 40-minute backstop, as the
[media transport](#media-transport) describes. The adapter holds it in memory, at most
`WAWARDEN_HISTORY_MAX_BYTES` plus 32 bytes, through a buffer that refuses every
write past that cap, before the engine writes it to `history/<id>.part`. The
history receipt goes out through the protocol library only when the engine asks for it;
the protocol library's own receipt on arrival is switched off.

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
its blob was marked processed or quarantined; the engine repeats this once an
hour while it runs, so a file whose deletion failed does not wait for a restart.
A processed blob's row, which holds only its identifier, two times and its
attempt count, is deleted 30 days after it was processed by the nightly prune of
the [backup](configuration.md#backups) scheduler, which runs with or without a backup recipient;
a quarantined blob's row is kept. An attempt that panics while it
downloads, decodes or applies a blob fails like any other. After three failed attempts, attempts cut
short by a crash included, a blob is quarantined: its reference and
files are deleted and the [notification event](observability.md#notifications) `quarantine` is
reported with `queue` `history`. A graceful stop that interrupts a blob gives its attempt back, like
a [pause](configuration.md#free-space): the blob stays waiting and is applied from its start at
the next start, keeping the batches it had already applied.

The engine processes one blob at a time, within the memory that
[Memory bounds](#memory-bounds) gives. The adapter decodes a blob with a nesting limit of 28 levels:
24 for each message and its quotes, below the blob's own four. It reads each
conversation's rows under the conversation's own chat, never under the chat a
row's key names; it keeps an edit row's own identifier, so that it is applied as
an edit of its target; and it passes the blob's phone-number-to-LID mappings and
push names to the engine, which applies them by the rules above. A message that
the owner's phone sends again on request, which the protocol library builds the
same way, keeps an edit's own identifier too.

### Media transport

Every download the protocol library makes, such as one of a history or
application-state blob, goes over the adapter's media HTTP client, whose own
timeout of 40 minutes is only a backstop. A request that carries no attachment
job ends after 2 minutes and is cut at `WAWARDEN_HISTORY_MAX_BYTES` plus
32 bytes; its timeout gives the same kind of error as the client's own timeout,
which the protocol library retries.

An attachment job starts only when a client asks for the attachment (see
[Attachments](api.md#attachments)). When a request's context carries a job, made
from an attachment's declared length and a deadline, the client's transport
holds every request of the job to these limits:

- a response may declare, and deliver, at most the declared length plus
  32 bytes;
- at most twice that many bytes pass through over all the job's requests, and
  the job makes at most 6 requests;
- no request follows a body read to its end, so a body that then fails its hash
  or authentication check is not fetched again from the next media host;
- a request that receives nothing for 60 seconds ends with a timeout, which
  the protocol library may retry within the limits above;
- an answer of 429 or any 5xx status ends the job and reports the answer's
  `Retry-After`, given in seconds or as a date; a missing or unreadable one is
  reported as 0;
- the job ends at its deadline. The transport defines 20 seconds for an
  attachment held in memory, and for one staged on disk 30 seconds plus one
  second for each MiB declared, at most 40 minutes.

Every limit but the 60-second stall ends the whole job as a cancellation, so
the protocol library stops at once: it neither retries the request nor tries
the next media host. The transport counts the bytes that pass through it and
buffers none. A declared length below 0 or above 2 GiB ends the job before its
first request. One watchdog goroutine per job, started through the same panic
recovery as the service's other goroutines, runs the deadline and stall timers
and exits when the job ends; a panic in it is counted as `wa.media.watchdog`
and ends the job.

### Attachment downloads

While `WAWARDEN_MEDIA_MAX_BYTES` is above 0, the adapter captures, at receipt,
the reference of an attachment whose declared size is within the cap, in a
message that is not view-once, and fetches nothing. It seals the descriptor as
a [media reference](#media-reference), which the archive keeps beside the
message.

A fetch starts only when a client asks for the attachment, through the
[attachment routes](api.md#attachments) or
[`get_attachment`](api.md#get_attachment). Both go through one job registry in
the API layer, which admits the request and charges its budgets, then calls the
app's media service. The service checks the cool-down, the connection and, for
an attachment over 16 MiB, the free space that staging keeps, and asks the
engine. `FetchMedia` runs only while the engine is ready, under its panic
recovery, and calls the adapter, which:

1. opens the reference for the message id and checks its descriptor against
   the requested row (see [Media reference](#media-reference));
2. refuses a declared length over the cap;
3. builds the protocol message of the recorded kind in `downloadable`, the only
   place that builds one, and wraps it in a `mediasrc.Source`;
4. stages the attachment in memory, in a buffer of its declared length plus
   32 bytes, when that length is at most 16 MiB, and otherwise
   [on disk](#attachment-staging);
5. puts the job's limits on the request's context (see
   [Media transport](#media-transport)) and calls the media seam.

The adapter has two download seams, each typed for what it carries:
`downloadToFile` takes only a history notification, which only
`DownloadHistory` builds, and `downloadMediaFile` takes only a
`mediasrc.Source`. They are the only callers of the protocol library's
`DownloadToFile`. A `Source`'s one field is unexported in its own package, so
only `downloadable` fills one, and the media seam refuses an empty one before
the library is called. Architecture rules pin the seams and the callers of
`FetchMedia` (see
[the adapter](threat-model.md#17-the-protocol-library-behind-one-adapter)).

A 403, 404 or 410 from the media host means WhatsApp no longer holds the file:
the engine sets the reference's expiry to now, and the minute sweep removes
it. A 429 or a 5xx ends the job with WhatsApp's `Retry-After`, and the media
service holds every new fetch for it, clamped to between 30 seconds and
10 minutes, or 1 minute when it gave none. Every other failure, a body that
fails its hash or authentication check included, is `media_failed`. The error
that leaves the adapter is one of a fixed set and carries no text from the
protocol library.

After a fetch, the media service applies the content policy of
`internal/attach` once for the job, whichever transport started it or reads
it, so both transports serve the same bytes. It takes the served type from the
bytes, never from the sender; re-encodes images, stickers other than Lottie
ones, and documents whose bytes are an image, within the budget given in
[Attachments](#attachments) under memory bounds; blanks a video's metadata
boxes; and keeps audio and every other document as fetched (see
[Delivery](api.md#delivery)). Re-encoded bytes replace the fetched ones, which
are erased. A document whose image bytes the normaliser refuses keeps them as
fetched and is served as `application/octet-stream`.

### Attachment staging

An attachment over 16 MiB is staged in `spool/` under the data directory,
which the first one creates with mode `0700`. The service stages at most one
at a time, and only while the free space left after it stays at least
1.25 times `WAWARDEN_MIN_FREE_BYTES` on the `local` profile, or 256 MiB on
`nfs`, so staging never pauses ingest. The file is opened exclusively, without
following a link, with mode `0600` and a random 128-bit name, and on `local`
its name is removed at once; on `nfs` it stays until the job ends.

The file holds only ciphertext: each 32 KiB sector is sealed with AES-256-GCM
under a key drawn for the file and held only in memory, with a nonce made of the
sector's index and a counter bumped at every write. The tags and counters stay
in memory. A write past the declared length plus 32 bytes, one that would leave
a hole, or a growth by truncation is refused and ends the job. Erasing the job
drops the key and closes the file, and on `nfs` removes its name; a file a crash
leaves behind cannot be read without the key, which died with the process. At
every start, whatever `WAWARDEN_MEDIA_MAX_BYTES` is, the service removes every
regular file in `spool/`, and while downloads are on it removes every hour the
files no running job holds. A [backup](configuration.md#backups) never includes
`spool/`.

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

Around a send it also sends, by itself:

- before the message, queries for the recipient's devices or the group's
  members, a query for the account behind a phone number whose LID it does not
  know, and fetches of pre-keys for the devices it has no session with;
- with a direct message, the contact's stored privacy token or a token it
  derives, and after it, when a new seven-day period has begun since its last
  update for that contact, a privacy-token update naming the contact;
- answers to retry receipts from a recipient's device that cannot decrypt a
  sent message: it encrypts the message again from a memory cache of the last
  256 sent messages, whose text stays in the process's memory until it leaves
  the cache; a message older than that, or sent before a restart, is not sent
  again.

It never downloads an attachment by itself: a fetch happens only when a client
asks for one (see [Attachment downloads](#attachment-downloads)), and consists
only of HTTPS requests to WhatsApp's media hosts. It never sends a media retry
receipt, which would ask the owner's phone to upload an attachment again, and
an architecture test refuses that call; the request above for a message marked
as unavailable is a different one.

It never sends read receipts, presence, typing indicators or status updates on
its own, and WaWarden never asks it to: an architecture test refuses those
calls.

### What a send does on the wire

A send carries one text message. The adapter builds the recipient from the
chat's canonical identifier, a phone number (`<digits>@s.whatsapp.net`), a LID
(`<digits>@lid`) or a group (`<id>@g.us`) without a device part, and refuses
any other form before the protocol library sees it. The protocol library may still
address a phone number by the LID it knows for it, from its own store or by
asking WhatsApp; the adapter reports the sent message under the requested
chat whatever the protocol library chose. Plain text goes out as a plain message. A
reply, or a message to a chat with a disappearing timer, goes out as an
extended text message whose context names the quoted message's id and sender
and carries the quoted text only, never media, and the timer in whole seconds;
it never names another chat.
A direct message is also encrypted for the owner's other devices.

The protocol library waits at most 12 seconds for WhatsApp's acknowledgement, and the
adapter gives the whole call a deadline of 20 seconds. The caller's
cancellation never reaches the protocol library, so a client that goes away cannot cut
a send short. When the connection drops before the acknowledgement and comes
back within 5 seconds, the protocol library writes the message again under the same id;
the engine itself never sends a message twice.

Apart from the busy refusal below, a send ends in one of five outcomes:

| Outcome | When |
|---|---|
| sent | WhatsApp acknowledged the message; its time is the server's, or the service's clock when the server named none. |
| not connected | Nothing was written: the engine was not connected or not paired, the connection was not logged in, or it was gone before the message was written. |
| recipient unavailable | The account is not a member of the group, the group does not exist, or WhatsApp found no account for the phone number; nothing was written. |
| rejected | WhatsApp answered with an error, whose number is kept, or the request could not be built: a malformed chat, quote or timer, an empty id or text, or no encryption session with a device. |
| unknown | The adapter cannot rule out that WhatsApp received the message: no acknowledgement came after it was written (a timeout, a drop, the deadline, or a panic), or an error the adapter cannot place ended the call. |

The protocol library's own error text never leaves the adapter, so no outcome
carries a chat identifier. The adapter reports the owner's LID as the sender
of a message to a chat keyed by LID and the owner's phone number otherwise,
never with a device part and never the sender the protocol library names: the forms
the protocol library gives the owner's messages when it reads them from
history. When a connection opens with both known, the engine records the
owner's phone number and LID as a pair, with source `self`, so that the owner's
messages in either form belong to one sender; a pair that contradicts the
archive is refused with the `rekey_conflict` event like any other. An identity
that is not the configured owner's is dropped and counted as `owner_unknown`,
a malformed one as `invalid`.

The engine sends one message at a time and queues none: while a send or a
history request runs, another is refused at once as busy. A running call holds
its place for at most the 20-second deadline.

A backfill (`wawarden admin backfill`, `POST /admin/v1/backfill`) goes out as
a history request: a protocol message to the owner's primary device that asks
for up to 50 messages of a chat that come before a given message. It runs
under the same deadline and has the same outcomes as a send. The phone answers
with an on-demand history-sync blob. WhatsApp's
acknowledgement means only that the request was accepted for delivery: the
phone answers when it processes the request, with a
[history notification](#history-sync).

### Fake engine

A development build, one built with the `dev` build tag, replaces the protocol
adapter with a scripted fake when `WAWARDEN_DEV_FAKE_ENGINE` is `1` or
`wrong_account`, to try pairing, ingest, the admin commands, the events and the
metrics without a WhatsApp account. Everything around the fake is the real
engine: the same [states](#engine-states), pairing guards,
[ingest rules](#ingest), inbox, [history worker](#history-sync) and caps. The
fake opens no network connection and no [device store](configuration.md#device-store), so it
takes no [backup](configuration.md#backups) and logs `backup_disabled` even when
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
  newsletter post; a revocation without a target; a voice note, a video, a
  PDF, a sticker, a JPEG sent as a document, a corrupt JPEG sent as a document
  and an attachment that answers expired; and a member joining the group. After the first ten events it drops the connection once, and the
  engine reconnects with its usual backoff. An event the engine leaves
  unacknowledged is offered again at the next step, as WhatsApp would deliver it
  again.
- **History sync.** Among the live events, five history notifications: an
  inline bootstrap blob with a LID mapping and a contact's name, a blob to
  download, one from the owner's device 5 (dropped, as it is not from the
  primary device), a blob that inflates past `WAWARDEN_HISTORY_MAX_BYTES`, and
  a poison blob that the fake's decoder refuses. The last two fail three times
  and are quarantined, each with a `quarantine` event.
- **Attachments.** With `WAWARDEN_MEDIA_MAX_BYTES` above `0`, the engine
  keeps the references of the photo and of the eight attachments, and a
  [fetch](api.md#attachments) returns synthetic bytes built in memory: an 8x8
  grey JPEG for the photo and, whole, for the JPEG document; its first half for
  the corrupt one, which the normaliser refuses so that it is served raw; an
  Ogg Opus voice note without audio; a 16x16 WebP sticker; a video and a
  PDF of 8 MiB and 64 KiB each, so that `get_attachment` sends them in two
  parts; and a PDF of 8 MiB less 64 KiB, which `get_attachment` sends inline
  as a blob. The fake has no near-cap normalised image, because image encoders
  are confined to `internal/attach`. The image attachment answers expired, and
  a fetch while not connected answers not connected.
- **Identity.** The script opens with the owner's identity: the owner's number
  paired with the synthetic LID `100000000000020@lid`.
- **Sends and history requests.** While connected for the owner, a send to
  `+15550100021` or `+15550100022`, by phone number or by their synthetic LIDs,
  is acknowledged with the owner's identity as sender; a send to `+15550100023`
  ends with an unknown outcome; a send to the synthetic group or to any other
  chat ends with recipient unavailable; and while not connected, nothing is
  sent. A history request for a synthetic chat returns a request id, and a
  tenth of a second later the fake delivers an on-demand history notification
  naming it, whose blob holds as many older synthetic messages as requested,
  offered again until the engine accepts it. The fake keeps the sends that
  reached its wire in memory, for tests of development builds.

When the script has played, within about 15 seconds of pairing, the fake stays
connected and sends nothing more: `wawarden admin status` reports `connected`,
3 chats, 25 messages and 2 history blobs quarantined. Every identity the fake
uses is synthetic: its contacts are `+15550100021`, `+15550100022` and
`+15550100023`, its LIDs are `100000000000020` to `100000000000022`, and an
owner number among the contacts, or `+15550100029`, stops the start with
`startup_failed`.

Release builds do not contain the fake: its package compiles only with the
`dev` tag; the dev-only-fake-engine architecture rule refuses any file of the
package, and any file that imports it, that the go tool could build without
that tag; a release build refuses `WAWARDEN_DEV_FAKE_ENGINE`, like every
`WAWARDEN_DEV_*` name, with `dev_variable_in_release`; and the reproducible
build fails when a release binary holds the fake's marker string or any symbol
of its package.

## Archive schema

The schema is at version 5. Besides chats, messages and their full-text index,
identity mappings, push names, group members, the inbox and the history-sync
queue, it holds:

- a change number per message, which a trigger sets to one more than the
  largest stored when a message is stored, edited or revoked, or loses its text
  at expiry; a duplicate of a stored message and a re-key change none;
- a reference per chat, 32 random lower-case hexadecimal digits that a trigger
  assigns when the chat is first stored. A re-key from a phone number to a LID
  keeps it. A re-key that merges a chat without messages into the other chat
  of the same person removes the merged chat and its reference with it;
- names that the owner saved for a contact, keyed by the contact's canonical
  identifier (see [Ingest](#ingest));
- on each message, `sent_by`: the 8-character id of the client that sent it
  through the gateway, empty on every other message; only the send path writes
  it;
- `self` as the source of an identity mapping, for the pair of the owner's own
  phone number and LID;
- at most one media reference per message, in a table beside the messages: the
  sealed reference of 1 to 2,048 printable ASCII characters, the kind
  (`image`, `video`, `audio`, `document` or `sticker`), a variant that only
  its kind may carry (`gif` or `video_note` for a video, `voice_note` for
  audio, `animated` or `lottie` for a sticker), the declared size of 0 to 2
  GiB, and when the reference expires and when it was added. The engine stores
  a message's reference only while its downloads switch is on, which it is
  exactly while `WAWARDEN_MEDIA_MAX_BYTES` is above 0; its minute
  sweep runs the purge of references, and a fetch that answers that the
  attachment has expired marks that message's reference expired. A
  reference stored with a message expires at the earlier of the message's
  time plus the retention given with it and the message's own expiry.
  It is written only with a new media message that has a media type, or later
  by a copy of a stored media message that has a media type and no reference,
  is not revoked and has not expired, and never when that expiry has already
  passed as the message is stored. The transaction that revokes a message
  deletes its reference, and so does the one that purges an expired message. A
  purge of references, at most a given number per call, takes a retention and
  a flag: with the flag set, it removes the references past their expiry and
  those added longer ago than the retention, and without it, every one.
  Marking a reference expired moves its expiry to the given time, never later;
- no media type on a revoked message, nor on an expired one once the expiry
  purge reaches it: the revoke or purge clears it, and the migration to
  version 5 clears it on messages revoked before. The purge also reaches media
  without a caption, which keeps no text;
- revoke tombstones: a chat, message id and canonical original sender, each
  kept 7 days. The engine writes one only for a revocation of a message not
  stored yet that passes the check a stored target would get (see
  [Ingest](#ingest)). While one lives, a copy of
  that message from that sender is stored already revoked, without text, media
  type or reference, and removes the tombstone; a copy from any other sender is
  stored as usual. The purge of references also removes tombstones past their
  expiry and, beyond 100,000, the oldest first, with or without its flag;
- the idempotency ledger: per client and idempotency key, the send's state
  (`pending`, `done` or `failed`), a 32-byte SHA-256 fingerprint of the request,
  the message id, when it was reserved, the earliest time the next send may
  follow it, when it was settled, whether it counts against the send budget, a
  result that holds only a status, an error code with the server's error
  number when it gave one, and an ack time, never message text or a chat
  identifier, and an expiry 24 hours after the reservation. The rows that count are
  also the send budget's ledger, so the budget survives a restart; its windows
  are measured with the service's clock, never SQLite's;
- backfill requests: a request id, the chat, a count of 1 to 50, when the
  request was made and when it completed;
- the audit anchor, at most one row: the id, time, key id and HMAC of the last
  audit row removed, when it was removed, and an HMAC over them with the id of
  its key. Triggers refuse to move it back or to remove it;
- read clients and the chats each one may read and write. A client row has
  room for a 32-byte digest of its token, not for the token; its expiry must be
  at most 366 days after its creation, and triggers refuse a write chat for a
  client that reads every chat, and an `INSERT OR REPLACE` or `REPLACE` that
  would swap a client row under its chats. A client's `allow_media` flag is off
  unless set. A client's chats are canonical identifiers, not references to
  stored chats, so they may name a chat the archive has not seen yet, and a
  re-key moves them from the phone number to the LID (see [Ingest](#ingest));
- the audit table, whose rows triggers refuse to update or replace, and to
  delete unless the audit anchor covers the row and it is not the newest row.
  Two columns, `message_hmac`, 16 to 32 bytes, and `bytes`, a count of 0 or
  more, are empty on rows written before version 5.
  Only the nightly [audit retention](observability.md#audit-retention) moves the anchor and
  deletes rows.

The [admin routes](api.md#admin-routes) create and revoke clients, and each change
appends a row to the [audit chain](observability.md#audit-chain). The engine writes saved
names from changes to the owner's contact list (see [Ingest](#ingest)), and a
read shows one only to a client whose scope includes that contact's direct
chat (see [Client API](api.md#client-api)). The service never runs `ANALYZE` or `PRAGMA optimize` on the archive: planner
statistics could make SQLite read a whole table where its read queries now
search an index.

## Full-text index rewrites

The [message archive](configuration.md#message-archive) removes revoked, edited and expired
text from its full-text index too. The index stores every three-character run
of the text and finds them through page keys, which copy the start of the
first run on each of its pages. Deleting a run keeps its key. When a deletion
leaves a key that is a whole run of the old text and no other message holds
that run, the service rewrites the entire index right after the deletion
commits, in a transaction of its own. A key that holds less than a whole run
is not rewritten.

The rewrite holds the archive while it runs, so other calls wait and may reach
their own deadlines. It writes the new index before it frees the old one, so
it needs free space of about the index's size. When the rewrite does not
commit, because the process stopped or the rewrite failed, the next start
rewrites the index before it logs `archive_opened`.

## Memory bounds

### One message

[Ingest](#ingest) drops every message whose identifiers, push name, text and
quoted text add up to more than 512 KiB (`too_large`). Accepting and applying
one message therefore allocates at most 48 MiB of Go memory, besides SQLite's
own. Only text made of control characters comes near that, because the inbox
writes each of them as six bytes; ordinary text needs about a seventh of it.

### One history blob

The engine processes one [history blob](#history-sync) at a time. Reading a
downloaded blob from `history/` and decompressing it, which it does in two
passes so that it can size the output exactly, allocates at most 2 times
`WAWARDEN_HISTORY_MAX_BYTES` plus 1 MiB: about 65 MiB at the default and
513 MiB at the maximum of 256 MiB. A blob that decompresses to more than the
cap is refused after the first pass, which allocates at most 1 MiB. Decoding
the blob and holding its conversations while they are applied come on top of
that and grow with the blob's content. Leave the process's memory limit well
above this figure when raising the cap.

### Attachments

While `WAWARDEN_MEDIA_MAX_BYTES`, written MAX here, is above 0,
[attachment downloads](#attachment-downloads) hold at most this much Go memory
at once:

4·(16 MiB + 32 B) + 128 MiB + 7·8 MiB + 2·70 KiB + 8·256 KiB +
⌈MAX / 32 KiB⌉·20 B

- 4·(16 MiB + 32 B): at most 4 attachments held in memory, each in a buffer
  sized once to its declared length plus 32 bytes, at most 16 MiB plus
  32 bytes, and never grown. An erased attachment whose buffer waits for a
  `get_attachment` result to be written keeps its place until the buffer is
  cleared, so the term covers every buffer still alive.
- 128 MiB: one image normalisation at a time, whose budget counts the decoded
  image, 4 bytes per sample of a progressive JPEG's coefficients and the
  output; an image over it is refused. It fits a progressive JPEG of about
  14 megapixels with 4:2:0 chroma, or a baseline one up to the 50-megapixel
  cap.
- 7·8 MiB: one `get_attachment` result that carries the file or a part at a
  time: a part read from disk into its own 8 MiB buffer, while an attachment
  held in memory is not copied, and the MCP library's encoding of the result,
  estimated at 6 times 8 MiB. The result keeps its place until the library has
  written it, or until 2 seconds after the call returned when the client left.
- 2·70 KiB and 8·256 KiB: the protocol library's buffers for 2 fetches, and the
  chunk buffers of 2 REST deliveries for each of 4 attachments.
- ⌈MAX / 32 KiB⌉·20 B: the tag and counter of each 32 KiB sector of the one
  attachment [staged on disk](#attachment-staging), about 1.25 MiB at the
  2 GiB maximum.

That is about 251 MiB whatever MAX is, and about 316 MiB together with one
history blob at the default cap. [Container
hardening](hardening.md#container-hardening) turns these figures into a
`--memory` size.

## Byte layouts

### Audit row HMAC

An [audit row](observability.md#audit-chain)'s HMAC is HMAC-SHA256 under the `audit-chain`
key derived from the [master key](configuration.md#master-key), over, in this order:

1. the domain string `wawarden/audit-row/v1` and a zero byte, or
   `wawarden/audit-row/v2` and a zero byte for a row whose `message_hmac` or
   `bytes` is set;
2. the id and the time, as 8-byte big-endian integers;
3. each other field of the row, with a presence byte and its length: the
   client id, the action, the chat, its `chat-hmac` value, whether the action
   was allowed (the presence byte alone), the reason and the peer; in a
   version 2 row, then the raw `message_hmac` and the byte count in decimal
   digits; last the key id;
4. the previous row's HMAC, or 32 zero bytes before the first row.

A row that names no message keeps the version 1 layout byte for byte, so a
chain written before the two columns existed verifies unchanged, and setting,
clearing or editing either value on any row changes its HMAC.

A row's `message_hmac` is the first 16 bytes of HMAC-SHA256 under the
`chat-hmac` key over the domain string `wawarden/audit-message/v1`, a zero
byte, and the message's canonical chat identifier, its id and its sender's
canonical identifier, each with a presence byte and its length. The domain
keeps it apart from every `chat_hmac`, which covers a bare identifier.

### Audit anchor HMAC

The [audit anchor](observability.md#audit-retention)'s HMAC is HMAC-SHA256 under the
`audit-chain` key of the master key the anchor names, over, in this order:

1. the domain string `wawarden/audit-anchor/v1` and a zero byte;
2. the row's id, the anchor's time and the removal time, as 8-byte big-endian
   integers;
3. the key id, with a presence byte and its length;
4. the row's HMAC.

### Media reference

A media reference is an attachment descriptor sealed with AES-256-GCM under
the `media-ref` key derived from the [master key](configuration.md#master-key).
The adapter seals one for each attachment it captures while
`WAWARDEN_MEDIA_MAX_BYTES` is above 0, and opens it only to fetch the
attachment a client asked for. Its text is strict unpadded
base64url, at most 2048 characters, of, in this order:

1. the version byte `0x01` and the 4-byte id of the sealing key;
2. a 12-byte random nonce;
3. the sealed descriptor and its 16-byte tag.

The additional data is the version byte and the key id, then the message id and
the label `media`, each preceded by its length as an unsigned varint. A
reference opens only for the message id it was sealed for, under the current
key or a retired one; only the current key seals. `FetchMedia` also refuses a
reference whose descriptor's kind or declared length differ from the requested
row's, or whose chat and sender, each mapped to its current identifier through
the archive's LID mappings, are not the requested row's chat and sender. The
cap leaves room for a descriptor of at most 1503 bytes.

The descriptor is the version byte `0x01`, then ten fields, each preceded by
its length as an unsigned varint: the attachment's download path, its media
key, the SHA-256 of its plaintext, the SHA-256 of its ciphertext, its declared
length as 8 big-endian bytes, its kind, its variant (empty when it has none),
the chat, the sender, and one byte that is 1 when the owner sent the message.
Opening accepts only this exact encoding: an unknown version, a truncated or
trailing field, a length written in more bytes than it needs, or a declared
length or direction byte of another size refuses the reference.

## Design rationale

### Write chats must be readable

A client's write chats must also be among its read chats. A client that could
write to a chat it cannot read could learn from the answer to a reply whether
a message exists.

### The failed-authentication budget does not slow guessing

The [failure budget](api.md#failed-authentication) changes only the answer to a
failure: authentication still runs for every request, and a valid credential
is accepted whether or not the budget is empty, so legitimate callers cannot
be locked out. For the same reason the budget does not slow down guessing.
What protects the admin and client tokens against guessing are their 256
random bits.

### The device store holds no message plaintext

The protocol library can keep each decrypted message in `session.db` until the
engine confirms it, which would let a refused or interrupted message be
decrypted again. WaWarden leaves that off, so that `session.db` holds no
message plaintext, at the cost described under [Delivery](#delivery).
