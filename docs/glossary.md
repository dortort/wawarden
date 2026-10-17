# Glossary

The terms the WaWarden documents use, in alphabetical order. Each entry links
to the document that describes the term in full.

## Contents

[Admin token](#admin-token) ·
[All-chats client](#all-chats-client) ·
[Archive](#archive) ·
[Attachment job](#attachment-job) ·
[Audit anchor](#audit-anchor) ·
[Audit chain](#audit-chain) ·
[Backfill](#backfill) ·
[Canonical identifier](#canonical-identifier) ·
[Chain head](#chain-head) ·
[Chat reference](#chat-reference) ·
[Client](#client) ·
[Content policy](#content-policy) ·
[Device store](#device-store) ·
[Download pause](#download-pause) ·
[EMF](#emf) ·
[Engine](#engine) ·
[First contact](#first-contact) ·
[Grant](#grant) ·
[History blob](#history-blob) ·
[History sync](#history-sync) ·
[Idempotency key](#idempotency-key) ·
[Inbox](#inbox) ·
[Key id](#key-id) ·
[LID](#lid) ·
[Linked device](#linked-device) ·
[Master key](#master-key) ·
[MCP](#mcp) ·
[Media reference](#media-reference) ·
[Message reference](#message-reference) ·
[Owner](#owner) ·
[Pacer](#pacer) ·
[Pairing code](#pairing-code) ·
[Part](#part) ·
[Protocol library](#protocol-library) ·
[Pseudonym](#pseudonym) ·
[Quarantine](#quarantine) ·
[Read chat](#read-chat) ·
[Re-key](#re-key) ·
[Retired key](#retired-key) ·
[Revoke tombstone](#revoke-tombstone) ·
[Shipped head](#shipped-head) ·
[Spool](#spool) ·
[Write chat](#write-chat)

## Admin token

The credential of the admin listener, of the form `wwadm_...`.
`wawarden admin init` prints it once, with its SHA-256. The service is
configured with the hash only and refuses the token itself. It never expires.
See [`admin init`](cli.md#admin-init) and
[rotating the admin token](runbook.md#rotating-the-admin-token).

## All-chats client

A [client](#client) created with `--all-chats`. It reads every chat, including
chats that appear later, and can never write. `admin status` counts these
clients under `all-chats clients active`.

## Archive

`archive.db` in the data directory: the SQLite database that holds the chats,
messages, the [inbox](#inbox), the clients and the [audit chain](#audit-chain).
One connection holds it under an exclusive lock while the service runs. See
[the message archive](configuration.md#message-archive).

## Attachment job

One client's fetch of one message's attachment, started by
`POST /v1/messages/{mref}/media` or by `get_attachment` and shared by both. It
fetches the file from WhatsApp once, passes it through the
[content policy](#content-policy), and serves it until it is erased: at its
first whole delivery, at the end of its life, by the minute sweep or by a
[download pause](#download-pause). See [Attachments](api.md#attachments).

## Audit anchor

At most one row of the [archive](#archive) that records the last
[audit chain](#audit-chain) row the nightly retention removed. It holds that
row's id, time, [key id](#key-id) and HMAC, and is signed under the
[master key](#master-key). Verification of the remaining chain starts from it.
See [audit retention](observability.md#audit-retention).

## Audit chain

The archive's `audit` table. A row records each client change and each client
request that the read budget admits. Each row carries an HMAC over its fields
and the previous row's HMAC, under a key derived from the
[master key](#master-key). `wawarden audit verify` recomputes it. See
[the audit chain](observability.md#audit-chain).

## Backfill

A request to the owner's phone for older messages of one chat, made with
`wawarden admin backfill`. The phone answers, when it does, with a
[history blob](#history-blob). Requests have a quota of one every 5 minutes
and 20 a day. See [`admin backfill`](cli.md#admin-backfill).

## Canonical identifier

The one normalised form of a WhatsApp chat or user identifier:
`<digits>@s.whatsapp.net` for a phone-number user, `<digits>@lid` for a
[LID](#lid) user and `<digits>@g.us` or `<digits>-<digits>@g.us` for a group.
The strict normaliser drops agent and device numbers and maps `c.us` to
`s.whatsapp.net`. It is the only source of a chat that a [grant](#grant) can
allow. See [the threat model](threat-model.md#secure-by-construction).

## Chain head

The first 16 bytes, in hexadecimal, of an [audit chain](#audit-chain) row's
HMAC. The service writes it on standard output in the `audit` line of each
row, as `chain_head`.

## Chat reference

A chat's `ref`: 32 random lower-case hexadecimal digits that the archive
assigns when it first stores the chat. The client API names chats by it, and
`admin chats list` prints it. It never reveals a phone number and stays the
same across a [re-key](#re-key). See
[chat references and identifiers](api.md#chat-references-and-identifiers).

## Client

An API client holding a WaWarden token of the form `ww_<client id>_...`. It
reads only its [read chats](#read-chat) and writes only to its
[write chats](#write-chat), until it expires or is revoked. On the WhatsApp
side, WaWarden itself is a [linked device](#linked-device). See
[the client API](api.md#client-api) and [the guide for agents](agents.md).

## Content policy

What the service does to an attachment's bytes before serving them, once per
[attachment job](#attachment-job) and the same for both transports: it takes
the served type from the bytes, re-encodes images without their metadata,
blanks a video's metadata boxes, and serves audio and other documents as
fetched. See [Delivery](api.md#delivery).

## Device store

`session.db` in the data directory: the SQLite database where the
[protocol library](#protocol-library) keeps the linked device's identity,
session keys and pre-keys. Backups include it. See
[the device store](configuration.md#device-store).

## Download pause

A switch on the admin listener, `POST /admin/v1/media`, that stops attachment
downloads at once and erases every [attachment job](#attachment-job). It lives
in memory, so a restart resumes downloads. See
[Pausing downloads](api.md#pausing-downloads).

## EMF

Embedded metric format: CloudWatch's format for metrics written as log lines.
With `WAWARDEN_METRICS_EMF=1`, the service writes its metrics in EMF on
standard output every 60 seconds. See
[embedded metric format](observability.md#embedded-metric-format).

## Engine

The part of the service that links the account, connects to WhatsApp and
ingests what arrives. It reports a state: `unpaired`, `connecting`,
`connected` or `disconnected`. A development build can run a scripted fake
engine instead. See [engine states](internals.md#engine-states) and
[disconnect reasons](runbook.md#disconnect-reasons).

## First contact

A send to a direct chat that has never written to the account: the archive
holds no message from the other person, of any kind. It is refused with `403`
`first_contact_refused` unless the client was created with
`--allow-first-contact`. Groups are exempt. See
[the ban-risk notes](ban-risk.md).

## Grant

A value that the policy core mints for one request after deciding that the
client may do it: a read grant, a write grant or an admin grant. Every read of
the archive and every send takes one. Grants have unexported fields, so no
other code can build one. See
[the threat model](threat-model.md#secure-by-construction).

## History blob

One part of the chat history that the owner's phone sends, announced by a
notification. The engine downloads it into `history/` in the data directory,
applies it to the archive and deletes it. See
[history sync](internals.md#history-sync).

## History sync

The transfer of the chat history from the owner's phone in
[history blobs](#history-blob), after pairing or in answer to a
[backfill](#backfill). `admin status` shows `history blobs pending` and
`history blobs quarantined`.

## Idempotency key

The required `Idempotency-Key` header of a send: 8 to 128 printable ASCII
characters, such as a UUID. The service keeps each key for 24 hours per
client. A repeat with the same chat, text and reply target sends nothing and
answers as the first did. See [sending](api.md#sending).

## Inbox

The table of the archive where the engine writes each live message before
WhatsApp is acknowledged. One worker applies it in order. A row that fails
three times is [quarantined](#quarantine).

## Key id

8 hexadecimal digits derived from a [master key](#master-key). It names the key
of each audit row, of the audit anchor, of each cursor and message reference,
and the file of a [retired key](#retired-key). The `keys_loaded` event logs it.

## LID

A WhatsApp user identifier that is not a phone number, written
`<digits>@lid`. WhatsApp uses it in place of a phone number in some chats. The
engine learns which LID belongs to which phone number from live messages and
history sync, and then [re-keys](#re-key) the chat.

## Linked device

A device linked to the owner's WhatsApp account, such as WhatsApp Web or
WaWarden itself, which links as `WaWarden (Linux)`. Another linked device has the
account's full access, outside every scope and audit row of WaWarden. See
[retiring other linked devices](runbook.md#retiring-other-linked-devices).

## Master key

`keys/master` in the data directory: 32 random bytes that the first start
creates. Every key the service uses is derived from it with HKDF-SHA256. No
backup holds it, so keep a copy off the host. `wawarden keys rotate` replaces
it. See [the master key](configuration.md#master-key).

## MCP

The Model Context Protocol. The client listener serves the client API as MCP
tools at `POST /mcp`, through the MCP library, for an MCP client such as
Claude Code. See [MCP](api.md#mcp).

## Media reference

An attachment's download path, media key and hashes, sealed under a key
derived from the [master key](#master-key) and kept beside its message while
attachment downloads are on. Nothing is fetched until a client asks; a
revocation or the message's expiry removes it. See
[Media reference](internals.md#media-reference).

## Message reference

A message's `mref`: a value sealed under a key derived from the
[master key](#master-key), which names a message for one client only. A send
takes one as `reply_to`. See
[cursors and message references](api.md#cursors-and-message-references).

## Owner

The person whose WhatsApp account WaWarden links, named by
`WAWARDEN_OWNER_PHONE`. Only that account can be paired.

## Pacer

The shared limit in front of every send: one send at a time, and each send of
any client starts at least 3 seconds plus a random wait of up to 2 seconds
after the previous one was admitted. A send it refuses answers `429`
`rate_limited`. See [send limits](api.md#send-limits).

## Pairing code

The code that `wawarden admin pair` prints, to enter on the owner's phone
under Linked devices, Link with phone number instead. It is never logged or
kept. See [Getting started](getting-started.md#4-pair).

## Part

An 8 MiB slice of an attachment that `get_attachment` returns when the file is
too large to come inline; part k starts at k times 8 MiB. Joined in order, the
parts make the served file. See [Parts](api.md#parts).

## Protocol library

`go.mau.fi/whatsmeow`, the unofficial implementation of WhatsApp's multi-device
protocol that the engine uses. Only one adapter package imports it. It sends
some traffic by itself (see
[residual risks](threat-model.md#residual-risks)).

## Pseudonym

What a WhatsApp identifier becomes in a log line: `jid:` and 8 hexadecimal
digits of an HMAC under a key derived from the [master key](#master-key). The
same identifier keeps the same pseudonym while the master key stays the same.
See [pseudonyms](observability.md#pseudonyms-and-dropped-lines).

## Quarantine

What happens to a [history blob](#history-blob) or an [inbox](#inbox) row that
fails three times, or a blob larger than `WAWARDEN_HISTORY_MAX_BYTES`. Its
content is deleted, a record stays, and the `quarantine` event reports it with
its `queue`.

## Read chat

A chat a [client](#client) may read, given with `--read` when the client is
created. It is stored as a [canonical identifier](#canonical-identifier), so
it may name a chat the archive has not seen yet.

## Re-key

The move of a direct chat from a phone-number identifier to the person's
[LID](#lid), once the engine learns the mapping. It moves the chat, every
client's read and write chats and the contact's names in one transaction, and
keeps the [chat reference](#chat-reference). A mapping that would widen what a
client reads is refused as a `rekey_conflict`. See [ingest](internals.md#ingest).

## Retired key

A former [master key](#master-key) kept in `keys/retired/`, named by its
[key id](#key-id). `wawarden keys rotate` puts the old key there. The service
uses it only to open cursors and message references sealed under it, and to
verify audit rows written under it.

## Revoke tombstone

A record that a message was revoked before the archive stored it, kept for
7 days and only when the revocation came from the message's sender or a group
admin the archive records. A copy of that message stored later is stored
revoked. See [Ingest](internals.md#ingest).

## Shipped head

A [chain head](#chain-head) that left the host in the service's standard
output, kept in a store the host cannot rewrite. `wawarden audit verify --log`
reports every shipped head that a copy of the archive lacks, which detects
rows removed from the end of the chain. See
[shipped heads](cli.md#shipped-heads).

## Spool

`spool/` in the data directory, where an attachment over 16 MiB is staged
while its [attachment job](#attachment-job) lives, as ciphertext under a key
held only in memory. Every start removes every regular file in it, and no
backup includes it. See [Attachment staging](internals.md#attachment-staging).

## Write chat

A chat a [client](#client) may send to, given with `--write` when the client is
created. Each write chat must also be a [read chat](#read-chat), and an
[all-chats client](#all-chats-client) has none.
