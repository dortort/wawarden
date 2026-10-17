# API reference

The service answers HTTP on three listeners. This page describes them: the
client API that agents call with a client token, the MCP endpoint that serves
the same reads and sends as tools, the admin routes behind the admin token,
the health check, and the fixed responses they share. How an agent should
treat what it reads is in [agents.md](agents.md). Terms such as chat
reference, canonical identifier, LID and pacer are explained in the
[glossary](glossary.md).

## Contents

- [Listeners](#listeners)
  - [Outbound connections](#outbound-connections)
- [Client and admin requests](#client-and-admin-requests)
  - [Client authentication](#client-authentication)
  - [Admin authentication](#admin-authentication)
  - [Failed authentication](#failed-authentication)
  - [Request bodies](#request-bodies)
- [Client API](#client-api)
  - [Chat references and identifiers](#chat-references-and-identifiers)
  - [Sending](#sending)
  - [Query parameters and pages](#query-parameters-and-pages)
  - [Cursors and message references](#cursors-and-message-references)
  - [Attachments](#attachments)
  - [Read rate limits](#read-rate-limits)
  - [Send limits](#send-limits)
  - [Busy reads](#busy-reads)
  - [Read audit](#read-audit)
- [MCP](#mcp)
  - [MCP requests](#mcp-requests)
  - [MCP tools](#mcp-tools)
  - [MCP audit](#mcp-audit)
- [Registering with Claude Code](#registering-with-claude-code)
- [Admin routes](#admin-routes)
  - [Client objects](#client-objects)
  - [Creating a client](#creating-a-client)
  - [Backfill requests](#backfill-requests)
  - [Chat identifiers in admin requests](#chat-identifiers-in-admin-requests)
  - [Status object](#status-object)
  - [Admin refusals](#admin-refusals)
- [Health listener](#health-listener)
- [Responses](#responses)

## Listeners

The service opens these listeners and no other socket. All three speak plain HTTP
without TLS. WaWarden does not restrict who can connect: network access control
and TLS are the deployer's responsibility, and none of the listeners may face the
public internet.

| Listener | Address | Opened | Serves |
|---|---|---|---|
| `client` | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | Always | The [client API](#client-api) and the [MCP endpoint](#mcp) `POST /mcp`, with a client token. |
| `admin` | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082` | Only when an admin token hash is configured | `GET /metrics` and the [admin routes](#admin-routes): the status, pairing and reconnection, the clients (create, list, show, revoke), the chat lookup and backfill, all with the admin token. |
| `health` | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`, loopback only | Always | `GET /healthz`, without authentication. |

Every start logs a `listening` event for each listener with its bound address,
and a `listener_not_loopback` warning for each client or admin listener bound to
an address that is not loopback. The warning is written whatever
`WAWARDEN_LOG_LEVEL` is set to; see [Logging](observability.md#logging).

Each listener's HTTP server has these limits: 5 seconds to read the request
headers, 15 seconds to read the whole request, 30 seconds to write the response,
60 seconds of idle time on a kept-alive connection, and 16 KiB of request
headers. An attachment delivery is the one exception to the write limit: it
renews a 30-second write deadline before each 256 KiB chunk, never past the
job's end (see [Attachments](#attachments)), and a `get_attachment` call that
returns the file or a part renews it once, to 30 seconds, before its result is
written
(see [MCP tools](#mcp-tools)).

### Outbound connections

Outbound, the service connects to WhatsApp, and only while a device is
paired or pairing was requested: the websocket at `web.whatsapp.com`, the
version fetch from the same host, and history and [attachment](#attachments)
downloads from the media hosts WhatsApp names. Its only other outbound connections go to the
[webhook](observability.md#webhook), when `WAWARDEN_NOTIFY_URL` is set. Each connection to
WhatsApp is made directly: the proxy variables
(`HTTPS_PROXY`, `HTTP_PROXY`) are ignored, every step has a timeout (10 seconds to
connect and for the TLS handshake, 20 seconds for response headers; the
websocket's opening 30 seconds, a version fetch 20 seconds, a history download 2
minutes in all, and an attachment fetch 20 seconds when it is held in memory, or
30 seconds plus one second per MiB declared, at most 40 minutes, when it is
staged on disk, with no request left 60 seconds without a byte), TLS is 1.2 or later, and redirects are refused, except to the
same host over HTTPS for the version fetch.

## Client and admin requests

Requests to the client and admin listeners pass these checks in order:

1. A request carrying an `Origin` or a `Sec-Fetch-Site` header is refused with
   `403` (`forbidden`), whatever else it carries. Browsers send these headers; no
   legitimate client of WaWarden is a web page.
2. An `OPTIONS` request, including `OPTIONS *`, is refused with `405`
   (`method_not_allowed`).
3. The request is authenticated. A failure is answered `401` (`unauthorized`)
   with `WWW-Authenticate: Bearer`, or `429` (`too_many_requests`, without that
   header) when the failure budget is spent; see
   [Failed authentication](#failed-authentication). On the client listener, a
   request that finds the copy of the clients being republished after a
   change for more than 2 seconds, as described under [Client authentication](#client-authentication), is answered `503`
   (`busy`) with `Retry-After: 1` instead, whatever its token.
4. On the client listener, the request then costs one from the client's
   [read budget](#read-rate-limits), every `POST /mcp` included; when the budget
   is spent, it is answered `429` (`rate_limited`) with `Retry-After`, before
   routing.
5. Only an authenticated request reaches routing: an unknown path is `404`
   (`not_found`) and a known path with another method is `405`
   (`method_not_allowed`) with an `Allow` header.

No request body is read before authentication succeeds. When a request
announces a body the service does not read, as with every refusal, every `404`
and `405`, and every read route, the response carries `Connection: close` and
the connection is closed instead of being read.

The credential is taken from exactly one `Authorization` header of the form
`Bearer <token>`: the scheme in any letter case, one space, and a token without
spaces or tabs. A request with two `Authorization` headers is not authenticated.

### Client authentication

The client listener accepts the token of a client the
[admin routes](#admin-routes) created: `ww_`, the client id (8 characters of `a`
to `z` and `2` to `7`, from 5 random bytes), `_`, 43 characters of unpadded
base64url encoding 32 random bytes, `_`, and the CRC-32 of everything before it
in 8 lower-case hexadecimal digits: 64 characters in all, in exactly this form.
The archive keeps only the token's SHA-256. Every request takes the same path:
the token is parsed, its id looked up in a copy of the clients held in memory,
the presented token hashed once and compared once, in constant time, with the
stored digest, or with a random digest drawn at start when the token is
malformed or its id unknown; only then are revocation and expiry checked. A
malformed, unknown, wrong, revoked or expired token is one failure, answered as
below. The copy is loaded when the archive opens, and authentication never
reads the archive. Three changes rebuild the copy: creating a client,
revoking one, and a re-key that moves a client's chat. A re-key moves a direct
chat from a phone number to the LID WhatsApp uses for it (see
[Ingest](internals.md#ingest)). Each change rebuilds the copy inside its own
write and publishes it once that write commits and before the change returns,
so a revocation takes effect on the next request; a request that was already authenticated finishes with the
grant it holds, within the 2-second read deadline, except an attachment
delivery, which runs until the minute sweep drops its job (see
[Attachments](#attachments)), and a `get_attachment` result that already
carries the file, which is still written, at most 25 seconds after the call
started plus 30 seconds to write it (see [MCP tools](#mcp-tools)). A request that arrives while
a change is being published, or after a change that did not commit and before
the copy has been reloaded in the background, waits for the new copy for up to
2 seconds and is then answered `503` (`busy`) with `Retry-After: 1`, whatever
token it presents; that answer is not a failure and spends no failure budget.
An authenticated request goes on to the [client API](#client-api); every other
request that passes checks 1 and 2 is answered `401` (or `429`):

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

### Admin authentication

The admin listener accepts a bearer token only when both hold:

- it has the exact form `wawarden admin init` generates: `wwadm_`, 43 characters
  of unpadded base64url encoding 32 bytes, `_`, and the matching CRC-32 in
  lower-case hexadecimal;
- its SHA-256 equals the configured hash, compared in constant time.

A string that is not in this form is refused even when its SHA-256 matches the
configured hash. Configuring the hash of anything other than a generated token
therefore leaves the admin listener unusable.

The admin listener serves `GET /metrics` (Go's router also answers `HEAD` on
it; see [Metrics](observability.md#metrics)) and the [admin routes](#admin-routes).

### Failed authentication

The client listener and the admin listener each count failed authentications, in
`wawarden_auth_failures_total` and `wawarden_admin_auth_failures_total`; the
admin listener's also report the [notification event](observability.md#notifications)
`admin_auth_failure`, at most once a minute. Each
also has one failure budget shared by all callers: 30 failures, refilled at one
per second up to 30. While the budget is empty, failures are answered `429`
instead of `401`.

The budget changes only the answer to a failure:

- Authentication still runs for every request, and a valid credential is
  accepted whether or not the budget is empty.
- A client list that is being republished answers `503` without spending the
  budget.
- Anyone who can reach a listener can keep its budget empty, after which a
  caller presenting a wrong token sees `429` rather than `401`.

[Design rationale](internals.md#the-failed-authentication-budget-does-not-slow-guessing)
explains why the budget does not slow guessing.

On the client listener, a revoked or expired client's token is a failure like
any other, and spends the budget. Failed authentications write no
[audit](observability.md#audit-chain) row. They are counted, and those of the
admin listener are reported in `admin_auth_failure` events, which carry a
count and nothing about the requests.

### Request bodies

The admin routes `pair`, `reconnect` and the client revoke route read their
body, which must be the empty object `{}`, the client create route reads the
client to create, the backfill route its request, and the client send route
`POST /v1/chats/{ref}/messages` the message, all with one decoder. No other
client REST route reads a body. Only a
route handler can call it, so it runs only after authentication and after the
route's grant was decided. It answers with a fixed error and reads no further when:

| Answer | Refused when |
|---|---|
| `415` `{"error":"unsupported_media_type"}` | The request does not carry exactly one `Content-Type` header, or that header is not `application/json`, optionally with the single parameter `charset=utf-8` (media type and charset in any letter case). A body the request announces is not read, and the connection is closed. |
| `413` `{"error":"body_too_large"}` | The body is longer than 16384 bytes, or 65536 bytes for the client create route so that two sets of 256 chats fit and for the send route so that the longest text fits with every character written as a `\u` escape, as announced by `Content-Length` (the body is not read) or found while reading; the connection is closed. |
| `400` `{"error":"invalid_body"}` | The body is not valid UTF-8, starts with a byte-order mark, or is not exactly one JSON object with nothing but white space after it; it nests objects and arrays more than 8 levels deep, counting the outer object; an object holds a key that is not lower-case `snake_case` (a letter `a` to `z`, then letters, digits and `_`), or holds the same key twice once escapes are decoded (`"te\u0078t"` is `"text"`); a key is not a field of the route's request; or a value does not fit its field. |

`POST /mcp` checks its bodies with the same media type rule and its own size,
depth and key rules; see [MCP requests](#mcp-requests).

## Client API

The client listener serves these routes with a client token. A client reads
only the chats its read scope names (or every chat, for an all-chats client),
and sends only to the chats its write scope names; an all-chats client never
writes.

| Route | Query | Answer |
|---|---|---|
| `GET /v1/me` | none | `{client:{id, name, expires_at, read:{all, chats:[{id, kind}]}, write:{chats:[{id, kind}]}}, session}`: the client's own scope, chats by canonical identifier, sorted |
| `GET /v1/chats` | `cursor`, `limit` | `{chats:[Chat], next, truncated, session}`, the chats in scope by their last message, newest first, then the chats without a message |
| `GET /v1/chats/{ref}` | none | one `Chat` |
| `GET /v1/chats/{ref}/messages` | `cursor`, `limit` | `{messages:[Message], next, truncated, session}`, newest first by time, then by arrival |
| `GET /v1/search` | `q`, `chat`, `cursor`, `limit` | `{messages:[Message], next, more, truncated, session}`, newest first by arrival, without rank or count |
| `GET /v1/changes` | `since`, `chat`, `limit` | `{messages:[Message], next, more, truncated, session}`, every message in scope that was added, edited, revoked or expired, in the order of its latest change, oldest first |
| `GET /v1/messages/{mref}` | none | one `Message` |
| `POST /v1/messages/{mref}/media` | none | `202` or `200`, the [attachment](#attachments)'s status, starting a fetch job when there is none |
| `GET /v1/messages/{mref}/media`, and `HEAD` | none | `202`, the [attachment](#attachments)'s status while it fetches, then `200` or `206` with its bytes |
| `POST /v1/chats/{ref}/messages` | none; a body and an `Idempotency-Key` header, see [Sending](#sending) | `201` `{message, replayed, session}`, the sent `Message` |

A `Chat` is `{id, kind, name, name_source, last_message_at}`:

- `id` is the chat's reference, 32 hexadecimal digits. It never reveals a
  phone number, and it stays the same when WhatsApp re-keys the chat: when
  the engine learns the LID behind a phone number and moves the direct chat
  from the number to the LID. A LID is the identifier WhatsApp gives an
  account in place of its phone number (`<digits>@lid`); see the
  [glossary](glossary.md#lid). `{ref}` and `chat=` take the reference exactly as
  `/v1/chats` lists it.
- `kind` is `phone`, `lid` or `group`.
- `name` is the chat's name in the archive, or `null`.
- `name_source` says where `name` came from: `push_name` for a direct chat
  named by the contact's push name, `group_subject` for a group named by its
  subject, or `null` when the chat has no name. A push name is the name a
  WhatsApp user sets on their own profile, which WhatsApp sends with their
  messages.
- `last_message_at` is the time of the chat's newest message, or `null`.

A `Message` is `{mref, chat, sender:{id, name}, from_me, ts, kind, text,
text_display, text_truncated, media_type, reply_to, quote_verified, edited_at,
revoked, origin, untrusted}`:

- `mref` is the message's [message reference](#cursors-and-message-references).
- `chat` is the chat's reference; `sender.id` is the sender's canonical user
  identifier (`<digits>@s.whatsapp.net` or `<digits>@lid`).
- `sender.name` is the name the owner saved for that contact when the contact's
  direct chat is also in the client's scope, otherwise the sender's push name,
  or `null`.
- `from_me` is `true` when the owner's account sent the message.
- `ts` and `edited_at` are RFC 3339 times in UTC with milliseconds. `edited_at`
  is `null` for a message never edited.
- `kind` is `text`, `media`, `reaction`, `poll_update` or `other`.
- `text_display` is `text` without control, bidirectional, zero-width and tag
  characters, for showing to a person; neither is safe to act on (see
  [docs/agents.md](agents.md)).
- `text` and `text_display` are `null` for a revoked message, which is answered
  with `revoked: true`, and when the message has no text. Each is cut on a
  character boundary so that it encodes to at most 32 KiB, and `text_truncated`
  says whether either was cut.
- `media_type` is the MIME type of a media message, such as `image/jpeg`, or
  its kind (`image`, `video`, `audio`, `document` or `sticker`) when the MIME
  type is missing or malformed; `null` for other messages, for a revoked
  message, and for an expired one once the engine's expiry pass, which runs
  every minute, has reached it.
- `reply_to` is the message reference of the quoted message when it is in the
  archive, or `null`; `quote_verified` passes on whether the quote matched it.
- `origin` is `gateway:<client id>` for a message a client sent through this
  service, shown to every client whose scope holds the chat; `owner` for any
  other message the owner's account sent; and `peer` for the rest. A message
  that history also delivers keeps its `gateway:` origin.
- `untrusted` is always `true`: every string in a message, and every chat and
  sender name, is third-party text, a gateway send's included.

No answer carries a database sequence number, an alternate sender identifier,
raw protocol data or other media metadata; only an [attachment](#attachments)
status, on the routes or in a `get_attachment` result, carries `kind`,
`variant`, `served_type`, `format`, `normalised` and `bytes`, all derived by
WaWarden. `session` is `{state}` alone, one of
`unpaired`, `connecting`, `connected` and `disconnected`, as under
[Engine states](internals.md#engine-states).

For example, a page of `GET /v1/chats/{ref}/messages` with one message (the
`mref` and `next` values are shortened; real ones are longer):

```json
{"messages":[{"mref":"m1_q8ZbN3xVd0kT5hLw2pRj7sYc4uFe9aGo","chat":"5b0e8c1f2a3d4e6f7a8b9c0d1e2f3a4b","sender":{"id":"15550100002@s.whatsapp.net","name":"Synthetic Contact"},"from_me":false,"ts":"2026-10-05T08:00:00.000Z","kind":"text","text":"See you at noon","text_display":"See you at noon","text_truncated":false,"media_type":null,"reply_to":null,"quote_verified":false,"edited_at":null,"revoked":false,"origin":"peer","untrusted":true}],"next":"Xk3v9Qm2...","truncated":false,"session":{"state":"connected"}}
```

### Chat references and identifiers

The key `id` names two different things, depending on where it appears:

| Where | `id` holds | Example |
|---|---|---|
| A `Chat` of the client API, `{ref}`, `chat=` and the MCP `chat` argument | The chat reference | `5b0e8c1f2a3d4e6f7a8b9c0d1e2f3a4b` |
| `GET /v1/me`, under `read.chats` and `write.chats` | The chat's canonical identifier | `120363000000000001@g.us` |
| The [admin routes](#admin-routes), for chats | The chat's canonical identifier; the reference is `ref` | `15550100002@s.whatsapp.net` |
| `GET /v1/me` under `client`, and the admin routes for clients | The client id, 8 characters of `a` to `z` and `2` to `7` | `aaaqeaye` |

A canonical identifier is the form the archive stores a chat or user under:
`<digits>@s.whatsapp.net` for a phone number, `<digits>@lid` for a LID, or
`<id>@g.us` for a group, without a device part. A client sees its own scope by
canonical identifier in `GET /v1/me`, but reads and sends name chats only by
reference.

### Sending

`POST /v1/chats/{ref}/messages` sends one text message to a chat in the
client's write scope. The body is `{"text": ..., "reply_to": ...}`, read by the
[strict decoder](#request-bodies); `reply_to` is optional and `null` is the
same as absent. The header `Idempotency-Key` is required, exactly once, and
holds 8 to 128 printable ASCII characters (`!` to `~`), for example a UUID.

For example, with `curl`, keeping the key so that a retry can reuse it:

```sh
KEY=$(uuidgen)
curl -sS -X POST "https://<your-host>/v1/chats/5b0e8c1f2a3d4e6f7a8b9c0d1e2f3a4b/messages" \
  -H "Authorization: Bearer $WAWARDEN_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  --data '{"text":"On my way","reply_to":null}'
```

A sent message answers `201`:

```json
{"message":{"mref":"m1_Vt7cKp2LmQ9xWb4nRz8eHs3jYd6uFa0g","chat":"5b0e8c1f2a3d4e6f7a8b9c0d1e2f3a4b","sender":{"id":"15550100001@s.whatsapp.net","name":null},"from_me":true,"ts":"2026-10-05T08:01:12.345Z","kind":"text","text":"On my way","text_display":"On my way","text_truncated":false,"media_type":null,"reply_to":null,"quote_verified":false,"edited_at":null,"revoked":false,"origin":"gateway:aaaqeaye","untrusted":true},"replayed":false,"session":{"state":"connected"}}
```

After a `503` `busy`, a `409` or a lost connection, retry with the same
`$KEY`: the service never sends one key's message twice. After a `504`, read
the chat before sending anything else ([Idempotency](#idempotency)).

The checks run in this order, and the first that fails answers:

1. The client must hold a write grant at the time of the request: a client
   without write chats, an all-chats client, and an expired or revoked one get
   the same `404` as an unknown route, before any body is read.
2. The body: `415`, `413` or `400` `invalid_body` as for every body, under a
   65536-byte cap, and `invalid_body` when `text` is missing, `null` or not a
   string, or `reply_to` is not a string. The cap counts the bytes sent, so it
   is wide enough for the longest text the next rule allows written entirely
   in `\u` escapes, as Python's `json` module and Go's `encoding/json` write
   some characters by default; the text's own length is checked after
   decoding.
3. The `Idempotency-Key`: missing, repeated or malformed answers `400`
   `invalid_idempotency_key`.
4. The text: 1 to 4096 characters, at most 8192 bytes of UTF-8, and none of
   the control characters other than line feed and tab, the bidirectional
   controls U+202A to U+202E and U+2066 to U+2069, the marks U+200E, U+200F
   and U+061C, U+FEFF, the separators U+2028 and U+2029, or the tag characters
   U+E0000 to U+E007F; otherwise `400` `invalid_text`. Text is refused, never
   altered, so what is sent is what was asked for. Zero-width joiners,
   non-joiners and variation selectors stay, for emoji. Text of white space
   only passes.
5. The chat: `{ref}` must be a chat the client may read and write to;
   otherwise the uniform `404`, whether the chat is missing, unreadable or
   readable but outside the write scope.
6. The reply: `reply_to` must be a message reference, sealed for this client,
   of a message in the same chat that is not revoked and has text; otherwise
   the uniform `404`. The reply quotes that message's text only, under its
   stored sender, and is stored as a verified reply.
7. The send itself, under the [send limits](#send-limits).

| Status | `error` | When |
|---|---|---|
| `201` | | Sent, or a replay of a sent message; `replayed` says which. |
| `404` | `not_found` | No write grant, a chat missing or outside the scope, a reply target that fails. |
| `400` | `invalid_body` | The body does not decode, or `text` or `reply_to` has the wrong type. |
| `400` | `invalid_idempotency_key` | The key is missing, given twice or malformed. |
| `400` | `invalid_text` | The text breaks the length or character rules. |
| `413`, `415` | `body_too_large`, `unsupported_media_type` | As for every body. |
| `403` | `first_contact_refused` | The chat is a direct chat that has never written to the account (no stored message from the other person, of any kind, history included), and the client was created without `allow_first_contact`. Groups are exempt. |
| `409` | `request_in_progress` | Another request with the same key is still running. `Retry-After` gives 1 to 5 seconds. |
| `422` | `idempotency_key_reused` | The key was used for another request: another chat, text or reply target. Also while the first is still running. |
| `429` | `rate_limited` | A [send limit](#send-limits) or the [pacer](glossary.md#pacer), the minimum gap between two sends of any client, or another send that is running; one code whatever the cause, which never names another client. `Retry-After` gives the whole seconds until the send would be admitted, 1 to 3600, so a wait set by the pacer or the global hourly limit follows from other clients' sends too (see the [threat model's residual risks](threat-model.md#residual-risks)). |
| `503` | `busy` | The archive did not answer within its deadline, or the client list was being republished. `Retry-After: 1`. Usually nothing was sent, but see [Idempotency](#idempotency). |
| `503` | `not_connected` | The engine is not connected or not paired; nothing was sent. `Retry-After: 5`. |
| `502` | `send_rejected` | WhatsApp refused the message, or the recipient cannot be reached (not a member of the group, a missing group, a phone number without a WhatsApp account). Stored, never retried. |
| `504` | `send_outcome_unknown` | The message may have reached WhatsApp, but no acknowledgement came: a timeout, a drop after the message was written, a restart or an expired lease. Stored, never retried. |
| `500` | `internal_error` | Anything else, such as an acknowledged send whose result could not be stored. |

#### Idempotency

The service keeps each key for 24 hours from its first use, per client. It
fingerprints the request by the chat's reference, the text, and the reply
target's message id and archive row, which a re-key of the target's sender
leaves unchanged. The same key reaches the same send from REST and from the
MCP tool.

A later request with the same key and the same chat, text and reply target
sends nothing and answers what the first did:

- `201` with the same message, read again from the archive, or the same `502`
  or `504`;
- with the header `Idempotent-Replayed: true` and, on a `201`,
  `replayed: true` in the body.

A replay is answered whatever the limits. It reads the message as it is now,
so a message revoked since shows no text. The reply target is checked before
the key is looked up, so a retry whose target was revoked or lost its text at
expiry since answers `404` and sends nothing, as a new request with that
target would.

What a key holds after each answer:

| First answer | The key afterwards |
|---|---|
| `400`, `403`, `404`, `429` or `503` `not_connected` | Nothing was stored: the same request can be made again under the key once the cause is gone. |
| `409` | The first request's answer follows. |
| `422` | The key stays bound to its first request. |
| `503` `busy` | Usually nothing was stored. It also answers a send that went out and was stored when the archive is too busy to read the message back, or to write the request's audit row, within 2 seconds. Retry under the same key: a send that never left is then made, and one that went out answers `201` with `Idempotent-Replayed: true` and writes its audit row. |
| `500` from an acknowledged send whose result could not be stored, or from a send that never left whose key could not be freed | The key answers `409` until its lease ends and `504` afterwards, and the service reports [`send_unrecorded`](observability.md#notifications). |

A send is at most once: when the outcome is unknown, the service never sends
again by itself, and answers `504` to the request and to every replay. A
`504` therefore means the contact may or may not have the message; read the
chat to find out before sending anything else. A send whose acknowledgement
came is stored as the owner's message with `origin` `gateway:<client id>`,
under the requested chat, in the same write that settles the key; in a chat
with a disappearing timer it carries the timer of the chat's newest message
that has one, and expires with it.

The handler gives a send 25 seconds: the engine waits at most 12 seconds for
WhatsApp's acknowledgement within a 20-second call, and the result is stored
within 5 more seconds, even when the client has gone. A key stays reserved
while its send runs, for at most 60 seconds; a key still reserved after that,
or by a send cut short by a restart, is settled as `send_outcome_unknown` at
the next request with it or at the next start, which reports
[`send_outcome_unknown`](observability.md#notifications) with the reason `lease_expired` or
`restart`. The engine runs one send at a time and queues none: a send while
another runs, or while a backfill runs, is refused at once with `429`
`rate_limited` and `Retry-After: 1`, and gives its budget back.

Any other method on `/v1/chats/{ref}/messages` is answered `405` with
`Allow: GET, HEAD, POST`, for every client.

### Query parameters and pages

Query parsing is strict: a parameter the route does not take, a parameter given
twice, an empty value or a malformed query string is `400` (`invalid_query`).
`limit` is a whole number from 1 to 200 in decimal digits, without a sign or a
leading zero; it defaults to 50. A page holds at most `limit` items and is cut
earlier, with `truncated: true` and a `next` cursor that resumes at the first
item left out, when its body would pass 256 KiB; it always holds at least one
item.

`q` is 3 to 128 bytes of UTF-8 without control characters, normalised to NFC,
of 1 to 8 terms separated by white space, each of at least 3 characters (code
points); a term matches anywhere inside the text, case folded the way the
full-text index folds it, and every term must match. Anything else is `400`
(`invalid_query`). Search walks the archive in windows of about 20,000 messages,
newest first, so a page can hold fewer than `limit` messages, or none, with
`more: true`: follow `next` until `more` is `false`. The query string,
including `q`, appears in the logs of any reverse proxy in front of the
service; keeping those logs is the deployer's concern.

`/v1/changes` takes `since`, either a `next` cursor of an earlier call or, on
the first call, an RFC 3339 time: the feed then starts at the first change of a
message dated at or after it. Without `since`, it starts at the beginning. Its
`next` is always set, so a client can poll with it; `more: true` means the
feed has not yet walked every change in the archive, so call again at once,
even when the page is empty: the walk covers every chat, and a client that
reads few chats can get several empty pages before it is up to date.

### Cursors and message references

`next` is an opaque cursor, at most 256 characters, or `null` at the end. It
holds only a position (a time and an arrival number, and for the change feed a
change number), sealed with AES-256-GCM under the `cursor-seal` key derived
from the [master key](configuration.md#master-key), and is bound to the client, the route, the
chat (or all chats) and, for search, the query: a cursor that was altered,
truncated, issued to another client or for another route, chat or query, or
sealed under a previous master key, is `400` (`invalid_cursor`). The scope is
taken from the client's grant at every request, never from the cursor, so a
cursor replayed after the client's scope narrowed returns nothing outside the
new scope.

A message reference (`mref`, `reply_to`) is `m1_` followed by the sealed chat,
message id and sender under the `mref` key, bound to the client, at most 640
characters. Its nonce is derived from the key, the client and the sealed
values, so the same message always gets the same reference for the same
client: a client can use it to recognise a message it has seen before, while
two clients get different references for one message. The reference changes
only when WhatsApp re-keys the message's chat or sender, or the master key is
replaced, and every reference issued to the client opens for it while the
master key stays the same, or while it stays in `keys/retired/` after
[`keys rotate`](cli.md#keys-rotate) replaced it; a reference issued after the
rotation reads differently for the same message. It is opened, its chat mapped to the chat's current
identifier, and the client's scope checked before the message is looked up.

#### Not found

An unknown, out-of-scope or malformed chat reference in the
path or in `chat=`, and an unknown, out-of-scope, foreign or malformed message
reference, are all answered with the same `404` (`not_found`) as an unknown
route, byte for byte. A request that names a chat checks the chat before its
cursor: an invalid cursor with a chat outside the scope is `404`, not `400`.
`404` means "not found or not yours".

### Attachments

A client created with [`allow_media`](#creating-a-client) fetches a media
message's attachment in two steps while downloads are on
(`WAWARDEN_MEDIA_MAX_BYTES` above 0; see the
[environment variables](configuration.md#environment-variables)):
`POST /v1/messages/{mref}/media` starts a fetch job, and `GET` or `HEAD` on the
same path reports on the job and then delivers its bytes. Nothing is fetched
when a message arrives. Both routes take no query (`400` `invalid_query`) and
ignore a request body.

Every request opens the message reference, decides the client's grant again
and looks the message up in the client's scope. A client without
`allow_media`, a chat outside the scope, a missing message and a foreign or
malformed reference all get the same `404` (`not_found`) as an unknown route,
whether downloads are on or off (see [Not found](#not-found)). A message in
scope then answers `404` (`media_unavailable`) when downloads are off, or when
it has no attachment reference: it is not a media message, it was view-once or
revoked, or its reference is past its expiry. A request that finds the message
no longer eligible also drops the client's job for it.

A job belongs to one client and one message; two clients asking for the same
attachment get two jobs and two fetches. The MCP tool
[`get_attachment`](#get_attachment) uses the same jobs: a job it started is
reported and served by these routes, and a job a `POST` started is answered by
the tool. `POST` answers:

| Answer | When |
|---|---|
| `202` status with `Retry-After: 2` | A new job started, or the job is still fetching. |
| `200` status | The job is ready. |
| The job's code | The job failed; see the codes below. |
| `422` `media_too_large` | The declared size is over the effective cap, the smaller of `WAWARDEN_MEDIA_MAX_BYTES` and 16 MiB for an image or 1 MiB for a sticker. |
| `503` `not_connected` with `Retry-After: 5` | WhatsApp is not connected. |
| `503` `busy` with `Retry-After` | WhatsApp throttled a fetch and the cool-down that follows is running (the seconds left of it); a fetch over 16 MiB would leave less free disk space than staging keeps (see [Free space](configuration.md#free-space); 60 seconds); or the fetch slots or staged objects below are taken (1 second). |
| `429` `rate_limited` | The client already has two jobs fetching or ready (no `Retry-After`), or a budget below is spent (`Retry-After`, the whole seconds until it admits the fetch). |

A status answer, a refusal and a failed job's code charge nothing; only a
`POST` that starts a job is charged. The budgets are kept in memory, so a
restart refills them, and `WAWARDEN_UNSAFE_RATE_CAPS` lifts none of them:

| Budget | Limit |
|---|---|
| Each client | `WAWARDEN_MEDIA_PER_CLIENT_PER_MINUTE`, 6 jobs a minute, evenly, with a burst of 2 |
| All clients, count | `WAWARDEN_MEDIA_GLOBAL_PER_HOUR`, 60 jobs in any hour |
| All clients, bytes | `WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR`, 4 GiB in any hour, charged with the declared size when the job starts; bytes fetched beyond it are charged when the fetch ends |

All three are checked before any is charged. At most 2 fetches run at once, at
most 1 of them over 16 MiB, and at most 4 jobs hold fetched bytes, at most 1 of
them on disk. When WhatsApp throttles a fetch, every new fetch waits for its
`Retry-After`, clamped to between 30 seconds and 10 minutes, or 60 seconds when
it gave none. Every request also costs one [read](#read-rate-limits).

`GET` and `HEAD` answer:

| Answer | When |
|---|---|
| `409` `media_not_requested` | The client has no job for the message: none was started, or it was erased. |
| `202` status with `Retry-After: 2` | The job is still fetching. |
| `410` `media_expired` | WhatsApp no longer holds the file. |
| `502` `media_failed` | The fetch failed: an integrity check, a size or time limit, or a staging error. |
| `422` `media_unsupported` | The content policy refused an image, a sticker or a video. |
| `422` `media_too_large` | The adapter refused the file's declared size as over its cap. |
| `404` `media_unavailable` | The reference no longer opens for the message. |
| `503` `busy` with `Retry-After` | WhatsApp throttled the fetch; the code lasts until the cool-down ends. |
| `200`, `206` or `416` | The job is ready: the delivery below. |

A failed job keeps its code for 30 minutes, the throttled one only until its
`Retry-After`, and answers `POST` with it too, without a new fetch. A fetch
that ends because the connection to WhatsApp dropped keeps no code: `GET` then
answers `409`, and a new `POST` starts, and is charged as, a new job.

A ready job lives 10 minutes when its bytes are held in memory and 30 minutes
when they are staged on disk, from the moment it turned ready (`expires_at` in
its status). It is erased at the first delivery of the whole file: a `200` or a
`206` whose range covers every byte, an inline `get_attachment` result, or the
last [part](#parts) not yet delivered; at the end of its life; by a sweep that
runs every minute and drops a job whose message is no longer eligible or whose
client was revoked or has expired; and at shutdown. Once erased, no request
finds it, and a delivery in flight stops at its next chunk.

#### Delivery

A delivery carries the job's bytes as fetched and passed through the content
policy, the same for every request: images, stickers other than Lottie ones,
and documents whose bytes are an image re-encoded without their metadata; a
video's metadata boxes blanked; and a document whose image bytes the
normaliser refuses kept as it arrived, served as `application/octet-stream`.
Its headers are:

- `Content-Type`, the served type that the content policy derived from the
  bytes, never the sender's; `Content-Length`; `Content-Range` on a `206`;
  `Accept-Ranges: bytes`;
- `ETag`, 128 random bits drawn for the job;
- `Content-Disposition: attachment; filename="wawarden-<kind>.<ext>"`, with the
  extension of the served type;
- `Content-Security-Policy: default-src 'none'; sandbox; frame-ancestors 'none'`
  and `Cross-Origin-Resource-Policy: same-origin`, besides `no-store` and
  `nosniff`. No `Content-Encoding` and no `Last-Modified`.

Only one range is honoured, `bytes=a-b`, `bytes=a-` or `bytes=-n`; several
ranges answer `200` with the whole file. `If-Range` is honoured only when it
matches the `ETag` exactly, and no other conditional header is read. An
unsatisfiable range answers `416` (`range_not_satisfiable`) with
`Content-Range: bytes */<size>`. Per job, across requests and `get_attachment`
results, inline or in parts, at most 2 deliveries run at once and at most 4
times the file's size is delivered in all; beyond either, `GET` answers `429`
(`rate_limited`) without `Retry-After`, and `get_attachment` `rate_limited`. A
`HEAD` counts toward neither.

The body goes out in chunks of 256 KiB, each under a write deadline of 30
seconds renewed before it and never later than the job's end. A reverse proxy
in front of the client listener should, on `GET /v1/messages/*/media`: turn
response buffering off (`proxy_buffering off` or `proxy_max_temp_file_size 0`
on nginx, whose defaults otherwise spool up to 1 GiB of plaintext to its
disk); allow a whole response at least 30 minutes; set no response-size limit
below `WAWARDEN_MEDIA_MAX_BYTES`; pass `Range` and `If-Range` through; and
neither cache nor compress the answer. On `POST /mcp` it should allow a
response of at least 16 MiB, which a `get_attachment` part needs.

#### Status

A status is `{kind, variant, state, served_type, format, normalised, bytes,
size_declared, delivery, reason, parts, offset, length, rest_route,
expires_at, retry_after_seconds, untrusted, notice}`, every value derived by
WaWarden; it carries no reference, message id or third-party string:

- `kind` is `image`, `video`, `audio`, `document` or `sticker`, and `variant`
  is `gif` or `video_note` for a video, `voice_note` for audio, `animated` or
  `lottie` for a sticker, or empty.
- `state` is `fetching` or `ready`; `reason` is `fetching` while it fetches,
  otherwise `null`. A `get_attachment` answer for a file over its inline limit
  starts no job: its `state` is empty and its `reason` is
  `too_large_for_inline`.
- Once ready, `served_type` is the delivery's `Content-Type`, `format` a word
  for the bytes (`image`, `audio`, `video`, `pdf`, `text`, `zip`, `ole2`,
  `archive`, `executable` or `unknown`), `normalised` whether the content
  policy re-encoded an image, `bytes` the served size, `parts` that size in
  8 MiB parts, rounded up, and `expires_at` the end of the job's life. While it
  fetches they are empty, `false`, `0` and `null`.
- `size_declared` is the size the message declared.
- On the routes `delivery` is `metadata`, and `offset` and `length` are `0`. A
  `get_attachment` result sets them as described under
  [get_attachment](#get_attachment).
- `rest_route` is `/v1/messages/{mref}/media`.
- `retry_after_seconds` is `2` while the job fetches, and absent once it is
  ready.
- `untrusted` is always `true`, and `notice` is a fixed sentence: the bytes
  are third-party content.

### Read rate limits

Each client has a read budget and a search budget, kept in memory and never
queued:

| Budget | Default | Refill | Burst |
|---|---|---|---|
| Reads | `WAWARDEN_READ_PER_CLIENT_PER_MINUTE`, 600 a minute | evenly, one every 100 ms at the default | 60, or the rate when it is lower |
| Searches | `WAWARDEN_SEARCH_PER_CLIENT_PER_MINUTE`, 60 a minute | evenly, one a second at the default | 6, or the rate when it is lower |

Every authenticated request on the client listener costs one read, whatever its
route or its answer, unknown routes and `404`s included, so probing spends the
budget; a search costs one read and one search. The [MCP endpoint](#mcp) draws
on the same two budgets: every `POST /mcp` costs one read, whatever it carries,
and a `search_messages` call also costs one search, charged before its
arguments are checked. A spent budget is answered `429`
(`rate_limited`) with `Retry-After`, the whole seconds until the next request is
admitted. Failed authentications cost nothing here; they have their own
[budget](#failed-authentication). Each value is capped at its default unless
`WAWARDEN_UNSAFE_RATE_CAPS=1`, which lifts the caps to 1,000,000 a minute and
logs `unsafe_rate_caps` at every start. The budgets start full at every start of
the service.

### Send limits

Sends have their own limits, apart from the read budgets: they are counted
over the rows of the archive's idempotency table, so a restart does not refill
them, in sliding windows measured with the service's clock.

| Limit | Default | Cap | Lifted cap |
|---|---|---|---|
| Each client, per minute | `WAWARDEN_SEND_PER_CLIENT_PER_MINUTE`, 10 | 10 | 60 |
| All clients, per hour | `WAWARDEN_SEND_GLOBAL_PER_HOUR`, 30 | 60 | 600 |
| Each client, per hour | the hourly limit divided by the number of clients that may write (active, not all-chats, with a write chat), at least 1 | | |
| Pacer | one send at a time, and the next send of any client starts at least 3 seconds plus a random wait of up to 2 seconds after the last one was admitted | | |

A send is counted from the moment it is admitted: the time is read once the
admitting write holds the archive, so a request that waited for the archive
is counted from the end of its wait, and its next send waits the full gap. A send that wrote nothing
to WhatsApp (`not_connected`, or `rate_limited` behind a running send) gives
its place back; a rejected send and one with an unknown outcome stay counted,
because WhatsApp may have seen them. A refused send answers `429`
`rate_limited` with `Retry-After`, the whole seconds until the earliest moment
it would be admitted, at least 1 and at most 3600; nothing queues. A request
also costs one read from the [read budget](#read-rate-limits), like every
request. `WAWARDEN_UNSAFE_SEND_CAPS=1` lifts the caps as in the table and logs
`unsafe_send_caps` at every start; `WAWARDEN_UNSAFE_RATE_CAPS` never lifts
them. `wawarden_sends_rejected_total` counts the refusals by limit.

### Busy reads

Reads share the archive's single database connection with ingest. At most 8
reads run at once: a read that finds them all taken, or that passes its
2-second read deadline, stops at once instead of waiting and is answered `503`
(`busy`) with `Retry-After: 1`. Every answer, `503` included, first writes its
[audit row](#read-audit) on the same connection and waits at most 2 seconds for
it; a row not written in that time makes the answer `503` (`busy`) with
`Retry-After: 1`. Behind a long ingest write, a request therefore waits at most
its 2-second read deadline and then 2 seconds for its row, never until the write
ends.

### Read audit

Every request the read budget admits writes one [audit](observability.md#audit-chain) row and
one standard-output line before it is answered, allowed or not. The action is
`rest.me`, `rest.chats`, `rest.chat`, `rest.messages`, `rest.search`,
`rest.changes`, `rest.message`, `rest.media.fetch`, `rest.media` or `rest.send` for the routes above, in that order, and
`rest.unrouted` for an unknown path or method, a send of a client without a
write grant included; the reason is `ok` (for a `201` too) or the error
code of the answer (`not_found`, `invalid_query`, `invalid_cursor`,
`rate_limited`, `busy`, `method_not_allowed`, one of the [sending](#sending)
codes, or `internal_error`, which a handler
that panics also records). The row names
a chat only when the answer is about one chat (`rest.chat`, `rest.messages` and
`rest.message` that succeed, `rest.media.fetch` and `rest.media` once the message
is found in scope, whatever the answer, and `rest.send` once the chat is found in the
write scope, whatever the send's outcome); a `404` `not_found` names none. No row holds the
text or the idempotency key. Every `POST /mcp` writes one
row too, with the actions and reasons listed under [MCP audit](#mcp-audit). A request whose row cannot be
written within 2 seconds is answered `503` (`busy`) with `Retry-After: 1`, and
one whose row fails for another reason `500` (`internal_error`); either way
nothing of the read leaves the service, although a send may already have gone
out (see [sending](#sending)). The row is written also when the client has
closed the connection before its answer, with the outcome the request reached:
a send whose client left during the acknowledgement wait is recorded with the
send's own result. A request refused by the read budget, and a failed
authentication, write no row.

A row can also name one message and a byte count. The message appears only as
its keyed `message_hmac`, never as its id (see
[Audit chain](observability.md#audit-chain)), and only on a row that names the
message's chat, so a row that names no chat, such as a `404`, names no message
either. A response that streams a body can
write a second, completion row once the body is sent: it repeats the first
row's client, chat, message and peer, with its own action and reason and the
number of bytes the connection accepted. It is written at most once, only after
the first row committed, and also when the client has left. A completion row
that cannot be written does not change the answer, which is already out: it is
counted in `wawarden_audit_failures_total` and logged as `audit_failed` (see
[Logging](observability.md#logging)). A `206` is recorded as `ok` and a `416`
as `range_not_satisfiable`. Only the [attachment](#attachments) routes and
the `get_attachment` tool name a message: their rows carry the message once it
is found in scope. A
`rest.media.fetch` row records the declared size when its request started, and
was charged for, a job, and `0` otherwise. A `rest.media` row of a `GET` that delivers
records the planned body length, and its completion row, `rest.media.sent`,
the bytes the connection accepted, with the reason `ok`, `client_gone`,
`write_timeout`, `expired` (the job's life ended), `revoked` (the job was
dropped) or `read_failed`.

## MCP

The client listener also serves the client API as [Model Context
Protocol](https://modelcontextprotocol.io) tools at `POST /mcp`, through the MCP
Go SDK `v1.8.0`, over its streamable HTTP transport: stateless, every answer one
`application/json` body, no session (`Mcp-Session-Id` is ignored and never
sent) and no server-sent event stream. The endpoint is registered on the client
router and passes the same [checks](#client-and-admin-requests) as every client
request first: the client token, the browser refusal, the read budget and the
audit row. `GET`, `PUT` and `DELETE /mcp` are answered `405`
(`method_not_allowed`) with `Allow: POST`, like any other known path with
another method. Both protocol paths work: the 2026-07-28 revision, where each
request carries `Mcp-Protocol-Version`, `Mcp-Method` (and `Mcp-Name` for a tool
call) headers and `_meta`, starting with `server/discover`, and the earlier
`initialize` handshake.

### MCP requests

Before the MCP library reads anything, the endpoint checks the body and answers
with the fixed JSON errors of the [request bodies](#request-bodies) table:

| Answer | Refused when |
|---|---|
| `415` `{"error":"unsupported_media_type"}` | The request does not carry exactly one `Content-Type` header of `application/json`, optionally with `charset=utf-8`; the body is not read and the connection is closed. |
| `413` `{"error":"body_too_large"}` | The body is longer than 65536 bytes, as announced by `Content-Length` (the body is not read and the connection is closed) or found while reading. |
| `400` `{"error":"invalid_body"}` | The body is not valid UTF-8, starts with a byte-order mark, or is not exactly one JSON object with nothing but white space after it, so a batch or a second message is refused; it nests objects and arrays more than 16 levels deep; or an object holds the same key twice once escapes are decoded, in the envelope or in tool arguments. Keys are not limited to `snake_case`: MCP uses `camelCase` and `_meta`. |

The MCP library then requires an `Accept` header listing both `application/json`
and `text/event-stream`, and a well-formed JSON-RPC 2.0 message. It answers the
requests it refuses itself, such as a malformed message, an unknown method name
or a protocol version it does not support, with a plain-text `400` or a JSON-RPC
error; those answers may repeat the caller's own method name, header values or
message text, never anything read from the archive.

The server advertises the `tools` capability only, without list changes; it has
no logging, resources, prompts or completions. It answers `initialize`,
`notifications/initialized`, `ping`, `server/discover`, `tools/list` and
`tools/call`. Any other method the MCP library knows, such as `resources/list`,
`prompts/list`, `logging/setLevel` or `subscriptions/listen`, is answered with
the JSON-RPC error `-32601` (method not found), with HTTP `404` on the
2026-07-28 path and `200` before it; for `subscriptions/listen` before
2026-07-28, the MCP library frames that one error as a single server-sent event and
ends the response. A notification is answered `202` without a body. A
`tools/call` naming any other tool is answered with the JSON-RPC error `-32602`
and the fixed message `unknown tool`, with HTTP `400` on the 2026-07-28 path
and `200` before it, and never repeats the name.

The MCP library's check that a request arriving on a loopback address names a
loopback `Host` is turned off: the service sits behind a loopback proxy that
forwards the original host name, which that check would refuse. The browser
refusal and the client token protect the endpoint instead. The MCP library also
checks the token's expiry against the wall clock, after the service has checked
it, and refuses an expired one with a plain-text `401`.

Every answer carries `Cache-Control: no-store` and `X-Content-Type-Options:
nosniff`. The MCP library's own log output is discarded.

### MCP tools

The tool definitions are constants: their names, descriptions and schemas never
contain a chat name or anything else read from the archive. Every client gets
the five read tools. A client that holds a write grant at the time of the
request also gets `send_message`, and a client created with
[`allow_media`](#creating-a-client) also gets `get_attachment` while downloads
are on (`WAWARDEN_MEDIA_MAX_BYTES` above 0). A call of a tool the client does
not get is answered exactly as a call of a tool that does not exist, byte for
byte, with the audit reason `unknown_tool`. The endpoint holds the four lists
as four servers built at start, one for each combination of the two grants,
and picks one for each request; the 2026-07-28 revision lets `tools/list` vary
with the authorization a request presents. Each tool but `get_attachment`
calls the same code as its route and answers the same JSON, which the
[client API](#client-api) describes:

| Tool | Arguments | Answer |
|---|---|---|
| `list_chats` | `cursor`, `limit` | As `GET /v1/chats`: `{chats, next, truncated, session}` |
| `get_chat` | `chat` (required) | `{chat, session}`, where `chat` is the `Chat` that `GET /v1/chats/{ref}` returns |
| `get_messages` | `chat` (required), `cursor`, `limit` | As `GET /v1/chats/{ref}/messages`: `{messages, next, truncated, session}` |
| `search_messages` | `query` (required), `chat`, `cursor`, `limit` | As `GET /v1/search`: `{messages, next, more, truncated, session}` |
| `get_changes` | `since`, `chat`, `limit` | As `GET /v1/changes`: `{messages, next, more, truncated, session}` |
| `send_message` | `chat`, `text`, `idempotency_key` (all required), `reply_to` | As `POST /v1/chats/{ref}/messages`: `{message, replayed, session}` |
| `get_attachment` | `message` (required) | The attachment's [status](#status), and the file when it comes inline; see [get_attachment](#get_attachment) |

There is no tool for `GET /v1/me` or `GET /v1/messages/{mref}`.

| Argument | Schema | Meaning |
|---|---|---|
| `chat` | string, 1 to 64 characters | A chat's `id` as `list_chats` returns it. |
| `cursor` | string, 1 to 256 characters | The `next` value of the previous page of the same tool, chat and query. |
| `since` | string, 1 to 256 characters | The `next` value of the previous `get_changes` call, or an RFC 3339 time for the first one, as for `GET /v1/changes`. |
| `query` | string, 3 to 128 characters | The search query, under the rules of `GET /v1/search`: 3 to 128 bytes after NFC normalisation, 1 to 8 terms of at least 3 characters each. |
| `limit` | integer, 1 to 200 | Items per page; 50 when omitted. |
| `text` | string, 1 to 4096 characters | The message to send, under the [text rules](#sending). |
| `reply_to` | string, 1 to 640 characters | The `mref` of a message of the same chat to reply to. |
| `message` | string, 1 to 640 characters | A message's `mref` as `get_messages`, `search_messages` or `get_changes` return it. |
| `idempotency_key` | string, 8 to 128 characters matching `^[\x21-\x7e]{8,128}$` | The key `Idempotency-Key` carries on the route: unique per intended message, repeated only to retry the same call. |

Each input schema is an object that allows no other property, and the arguments
are checked against it before anything is read. Omitted or `null` arguments are
an empty object. Cursors are the route's: a `next` value from a tool continues
on its route and the other way round, under the same [cursor](#cursors-and-message-references) rules.
Every read tool carries the annotation `readOnlyHint: true`, and
`get_attachment` also `idempotentHint: true` and `openWorldHint: true`;
`send_message` carries `readOnlyHint: false`, `destructiveHint: false`,
`idempotentHint: true` and `openWorldHint: true`, and its description asks for
at most one send per trigger. Every tool has an `outputSchema` inferred from
the answer's type.

A result carries the answer in `structuredContent` and the same JSON as text in
`content[0]`; a `get_attachment` result is laid out as described under
[get_attachment](#get_attachment). A refused call is a result with `isError: true`, the fixed code
alone as the text of `content[0]`, and in `structuredContent` the answer's shape
with empty data and the current `session`, for example
`{"chats":null,"next":null,"truncated":false,"session":{"state":"connected"}}`.
No error text of the service or of the MCP library, and no argument value, reaches a
result. A refused `send_message` call carries `retry_after_seconds`, the whole
seconds the route's `Retry-After` would give, in its `structuredContent` for
`request_in_progress`, `rate_limited`, `busy` and `not_connected`, and
`replayed: true` when it repeats the stored `send_rejected` or
`send_outcome_unknown` of an earlier call with the same key. A refused
`get_attachment` call carries the empty status, with only `rest_route`,
`untrusted` and `notice` set, and `retry_after_seconds` when the route would
send `Retry-After`.

| Code | Answered when |
|---|---|
| `not_found` | As on the routes: the chat is outside the client's scope or does not exist, including with a cursor or `since` value that does not open; or the client gets no read grant at the time of the call. A denied chat and a missing one give byte-identical answers. |
| `invalid_arguments` | The arguments do not fit the input schema: a missing required argument, another property, a wrong type, an empty string, a value out of bounds or a non-integral `limit` or `part`. |
| `invalid_query` | The query passes the schema but not the search rules. |
| `invalid_cursor` | As on the routes. |
| `rate_limited` | The client's search budget is spent, for `send_message` a [send limit](#send-limits), or for `get_attachment` the client's two jobs, an [attachment budget](#attachments) or a per-job delivery cap. A tool result carries no `Retry-After`; at the default rate a search is admitted again within a second. A spent read budget refuses the whole `POST` with `429` before the call. |
| `busy` | As on the routes, or the call passed its deadline: 10 seconds for a read tool, 25 seconds for `send_message` and `get_attachment`. For `get_attachment` also while another call's inline result or part is built or written, since one is handled at a time across the service, or when the job ended while the call read it. |
| `media_unavailable`, `media_expired`, `media_failed`, `media_unsupported`, `media_too_large` | `get_attachment` only, when the [attachment routes](#attachments) would answer that code. |
| `range_not_satisfiable` | `get_attachment` only, for a `part` at or past the ready job's `parts`. |
| `invalid_text`, `invalid_idempotency_key`, `first_contact_refused`, `request_in_progress`, `idempotency_key_reused`, `not_connected`, `send_rejected`, `send_outcome_unknown` | `send_message` only, when the route would answer that code; `not_connected` also for `get_attachment` when a fetch would start while WhatsApp is not connected. A text or key that breaks the input schema, such as an empty text or a seven-character key, is `invalid_arguments` instead; control characters and the 8192-byte cap reach the text check. |
| `internal_error` | Anything else failed, a panic in the tool included; the panic is counted as `api.mcp` and the endpoint keeps serving. |

Pages and texts are cut as on the routes. Because a result carries the page
twice, as structured content and as text, a full page makes an answer of about
twice its size; Claude Code saves a tool result over 50,000 characters to a file
instead of passing it to the model, so a smaller `limit` keeps answers inline.

#### get_attachment

`get_attachment` takes a message's `mref`, and optionally a `part` from 0 to
255, and runs the checks of the [attachment routes](#attachments): it opens the
reference, decides the client's grant again and looks the message up in scope,
so a missing, foreign or out-of-scope message is `not_found` and a message
without a usable reference `media_unavailable`. It then uses the client's job
for the message:

- No job: without `part`, a file declared over its inline limit (the smaller
  of `WAWARDEN_MEDIA_MAX_BYTES` and 16 MiB for an image, 1 MiB for a sticker
  and 8 MiB for anything else) is answered with its status alone, `delivery`
  `part` and `reason` `too_large_for_inline`; nothing is fetched and nothing is
  charged, and the file is fetched in parts or through the routes instead.
  Otherwise, and always with `part`, the call starts a job exactly as `POST`
  does, with the same refusals and budgets, and waits for it.
- A job fetching in memory (declared at most 16 MiB): the call waits for it
  within its 25-second deadline, and answers the fetching status if it has not
  ended by then. A job fetching to disk is answered with its fetching status at
  once.
- A failed job: its code, as on the routes.
- A ready job: with `part`, that part of the file, as described under
  [Parts](#parts). Without it, the file comes inline when its bytes are held in
  memory and its served type and size allow a block; otherwise the status
  alone, with `delivery` `part`, and `reason` `too_large_for_inline` when the
  size is over the inline cap.

The block follows the served type, never the sender's:

| Served type | Inline cap | Block |
|---|---|---|
| `image/jpeg`, `image/png`, only when the content policy re-encoded the image (`normalised` `true`): images, stickers other than Lottie ones, and documents whose bytes are an image | 3.5 MiB | `image` |
| `audio/ogg`, `audio/mpeg`, `audio/mp4` | 8 MiB | `audio`; an embedded `resource` blob of the same type for a client on the 2024-11-05 protocol, which has no audio block |
| `video/mp4`, `application/pdf`, `application/octet-stream` | 8 MiB | an embedded `resource` blob of that type with the URI `wawarden:attachment` |
| any, for a call with `part` | 8 MiB per part | an embedded `resource` blob of type `application/octet-stream` with the URI `wawarden:attachment-part` |

An image the content policy did not re-encode never comes as an image block:
an image served as `image/jpeg` or `image/png` without `normalised` gets its
status alone, and a document whose image bytes the normaliser refused is
served as `application/octet-stream`, with `format` `image` and `normalised`
`false`, as a blob. Audio served as `application/octet-stream` comes as a blob
too. Some MCP hosts write a blob to a file; Claude Code does so for PDF, Office
and audio blobs.

##### Parts

When `delivery` is `part`, call `get_attachment` again with `part` from `0` to
`parts` − 1, one call at a time, and join the parts in that order: the joined
length equals `bytes`. Part k is the slice of the served file at offset k × 8
MiB, 8 MiB long or the rest of the file for the last one. `part` works on any
ready job, so a file that would also come inline is one part. A part comes as
an embedded `resource` blob of type `application/octet-stream` with the URI
`wawarden:attachment-part`, whatever the served type, and its status sets
`delivery` `part` with the part's `offset` and `length`. A `part` at or past
`parts` answers `range_not_satisfiable` and leaves the job as it was. A part is
read from the job's bytes and fetches nothing; while a job fetches to disk, a
call with `part` answers the fetching status, with `retry_after_seconds`. An 8
MiB part makes a response of about 11 MiB, under the 16 MiB that Claude Code
accepts from an MCP server. The same file is also available through `POST` then
`GET /v1/messages/{mref}/media` with the same token.

A result carries the status in `structuredContent` and its `content` is, in
order: a constant text that introduces the attachment as third-party content,
the status as JSON text, the block when the file comes inline, and a constant
closing text. The block carries `_meta` `{"wawarden/untrusted": true}`. When
the file comes inline, `delivery` is `inline`, `offset` is `0` and `length` is
the file's size; for a part, `delivery` is `part` and they are the part's;
otherwise `delivery` is `part` or `metadata` and both are `0`. No argument
value, the part index included, and no text read from the message appears in
the result.

The service handles one inline result or part at a time: a call that would
return a file or a part while another call's inline result or part is built or
written answers `busy`, so an agent should ask for one attachment or part at a
time. Before such a result is returned the call renews the connection's write
deadline to 30 seconds; when it cannot, or the request has ended, the call
answers the status alone. A result that carries the file or a part counts
toward the job's per-job delivery caps, shared with the routes. Once the MCP
library has written it in full and its audit row has committed, an inline
result completes the job, which is then erased, and a part joins the job's
delivered parts; the last part not yet delivered completes the job, and a
repeated part does not. A result that was not written in full leaves the job
ready and its part undelivered. When the request ends before the result is
written, the call's hold on the job ends 2 seconds after the call returns,
without completing the job, and the job's bytes are later dropped without being
overwritten, since the MCP library may still have been reading them. A job
erased while a result that carries its file or a part is being written, by a
whole-body `GET`, its end of life, the sweep, a revocation or shutdown, keeps
its bytes in memory until that write ends, at most 25 seconds after the call
started plus 30 seconds to write; the result is still written.

The server has no resources capability, although the MCP specification
recommends one for embedded resources: a blob is not a resource a client can
read again, and `resources/read` is answered as method not found. There is no
resource link either, since the route needs the bearer token.

### MCP audit

Every `POST /mcp` that the read budget admits writes one [audit](#read-audit)
row. The action is `mcp.list_chats`, `mcp.get_chat`, `mcp.get_messages`,
`mcp.search_messages`, `mcp.get_changes`, `mcp.send_message` or
`mcp.get_attachment` for a call of that tool, and
`mcp.protocol` for everything else: the handshake, `tools/list`, `ping`,
notifications, refused methods, unknown tools and refused bodies. The reason is
`ok`, the tool's code, the body refusal (`unsupported_media_type`,
`body_too_large` or `invalid_body`), `method_not_found`, `unknown_tool`, or
`bad_request` for a message the MCP library refused before it reached the server. A
notification answered `202` is `ok`. A row names a chat only for a successful
`get_chat` or `get_messages`, and for a `send_message` call once the chat is
found in the write scope. A `get_attachment` row names the chat and the
message once the message is found in scope, whatever the answer. Its reason is
`inline` when the result carries the file, with the file's size as its bytes;
`part` when it carries a part, with the part's length as its bytes; `metadata`
for the status alone and `too_large_for_inline` for a file over its inline
limit, both with `0` bytes; all four are recorded as allowed. Otherwise
it is the tool's code. When the client has left before the answer, the
MCP library writes none, and the row records the call's own outcome.

## Registering with Claude Code

1. Find the chats the agent may read with
   [`admin chats list`](cli.md#admin-clients-and-admin-chats), then create a read
   client for them. `--read` takes a chat identifier, a `+E.164` number or a
   chat reference, and can be repeated:

   ```sh
   wawarden admin chats list --match Synthetic --token-file admin.token
   wawarden admin clients create --token-file admin.token \
     --name claude-code --read 120363000000000001@g.us --read +15550100002 \
     --expires-days 90
   ```

   The token is printed once, on the `token:` line. Keep it in a secret
   manager or in the agent host's environment as `WAWARDEN_TOKEN`, never in a
   repository.

   > **`WAWARDEN_TOKEN` is read by the agent's host only.** Never put it in
   > the service's environment: `serve` refuses every `WAWARDEN_` name it
   > does not implement, so it stops with `unknown_variable` (see
   > [Unknown variables stop the service](configuration.md#unknown-variables-stop-the-service)).
2. Register the endpoint, here as `whatsapp`, at the address of the proxy that
   serves the client listener. The name and the URL come before `--header`:

   ```sh
   claude mcp add --transport http whatsapp https://<your-host>/mcp --header "Authorization: Bearer $WAWARDEN_TOKEN"
   ```

   The shell expands `$WAWARDEN_TOKEN` when the command runs, so Claude Code
   stores the token itself in `~/.claude.json`: for the current project with
   the default scope `local`, for every project with `--scope user`. Do not
   combine a literal token with `--scope project`, which writes `.mcp.json`, a
   file meant to be committed. To keep the token out of every file, write the
   entry in `.mcp.json` with a reference that Claude Code expands when it
   connects:

   ```json
   {"mcpServers":{"whatsapp":{"type":"http","url":"https://<your-host>/mcp","headers":{"Authorization":"Bearer ${WAWARDEN_TOKEN}"}}}}
   ```

   or replace `headers` with `headersHelper`, a command that prints the headers
   as a JSON object and that Claude Code runs at each connection and again
   after a `401` or `403`. Claude Code asks once, when `claude` starts in that
   project, whether to use a server from `.mcp.json`; until it is approved,
   `claude mcp list` shows it as pending approval.
3. `claude mcp get whatsapp` or `claude mcp list` shows the server as
   connected (a server from `.mcp.json` once it is approved). `/mcp` in a
   session lists its five tools: `get_changes`, `get_chat`, `get_messages`,
   `list_chats` and `search_messages`, and `send_message` as a sixth for a
   client with write chats. A wrong, expired or revoked token shows as a
   failed connection rather than as a server that needs authentication: the
   endpoint offers no OAuth flow.
4. Rotate the token before it expires, as
   [Rotating a client token](runbook.md#rotating-a-client-token) describes.
   For this registration, switching to the new token means running
   `claude mcp remove whatsapp` and adding the endpoint again with the new
   token, then checking it as in step 3.
5. When the agent read WhatsApp through another device linked to the same
   account before, log that device out on the owner's phone once the agent
   works through WaWarden: a linked device has the account's full access,
   outside every scope, budget and audit row of WaWarden (see
   [the threat model](threat-model.md#trust-roots) and
   [Retiring other linked devices](runbook.md#retiring-other-linked-devices)).

Every name and text a tool returns is third-party content: an agent must
never act on it. [docs/agents.md](agents.md) says how an agent should read,
page and retry, and how to scope its token.

## Admin routes

Each route answers JSON with `Cache-Control: no-store`. Every call of a `POST`
route that passes authentication, whatever its outcome, reports the
[notification event](observability.md#notifications) `admin_mutation` with the route's action
and its outcome: `ok` or the error code it answered.

In the admin routes, a chat's `id` is its canonical identifier, such as
`15550100002@s.whatsapp.net`, and its chat reference is `ref`. This differs
from the client API, where a chat's `id` is its reference (see
[Chat references and identifiers](#chat-references-and-identifiers)). A
client's `id` is its client id.

| Route | Request body | Answer |
|---|---|---|
| `GET /admin/v1/status` | none | `200` with the fixed object below. |
| `POST /admin/v1/pair` | exactly `{}`, see [Request bodies](#request-bodies) | `200` `{"code":"<pairing code>"}`; see [Pairing](internals.md#pairing). |
| `POST /admin/v1/reconnect` | exactly `{}` | `200` `{"status":"accepted"}`: the engine connects again at once, after an explicit reconnect as described under [Engine states](internals.md#engine-states). |
| `POST /admin/v1/clients` | the client to create, below | `200` `{"client":<client>,"credential":"ww_..."}`. The JSON field `credential` holds the client token; this answer is the only one that carries it. |
| `GET /admin/v1/clients` | none | `200` `{"clients":[...]}`, oldest first: each client's `id`, `name`, `state`, `created_at`, `expires_at`, `revoked_at`, `all_chats`, `allow_first_contact`, `allow_media`, `read_chat_count` and `write_chat_count`. |
| `GET /admin/v1/clients/{id}` | none | `200` with the client. |
| `POST /admin/v1/clients/{id}/revoke` | exactly `{}` | `200` with the revoked client. Revoking a revoked client changes nothing and answers the same. |
| `POST /admin/v1/backfill` | `{"chat":..., "count":...}`, below | `202` `{"request_id":"<id>"}`: the backfill went to WhatsApp under that id; the phone answers when it processes it. See [Backfill requests](#backfill-requests). |
| `GET /admin/v1/chats` | none; the query may hold one `match` | `200` `{"chats":[{"id","kind","ref","name"}...],"truncated":false}`: at most 100 chats, most recent message first, whose name contains `match` (ignoring the letter case of ASCII letters), whose identifier contains it, or whose reference is it; every chat without `match`. `truncated` is `true` when more match. |

### Client objects

A client is:

```json
{"id":"aaaqeaye","name":"agent","state":"active","created_at":"2026-10-06T12:00:00Z","expires_at":"2027-01-04T12:00:00Z","revoked_at":null,"all_chats":false,"allow_first_contact":false,"allow_media":false,"read_chats":[{"id":"120363000000000001@g.us","kind":"group","known":true,"name":"Synthetic Group"}],"write_chats":[]}
```

`state` is `active`, `expired` once `expires_at` has passed, or `revoked`.
Each chat carries its canonical identifier, its `kind` (`phone`, `lid` or
`group`), whether the archive has seen it (`known`), and its name in the
archive, or `null`. The administrator sees chat identifiers and names by
design; no client route ever answers them.

### Creating a client

The body of `POST /admin/v1/clients` is an object with these keys, every one
optional:

| Key | Content |
|---|---|
| `name` | 1 to 64 characters without control characters, unique among all clients, revoked ones included, ignoring the letter case of ASCII letters. |
| `read_chats` | The chats the client may read: at most 256 chat identifiers. |
| `write_chats` | The chats the client may [send](#sending) to: at most 256, each also in `read_chats`. |
| `all_chats` | `true` lets the client read every chat, including chats that appear later; then `read_chats` and `write_chats` must be empty. |
| `allow_first_contact` | `true` lets a write chat be a direct chat the archive has not seen yet. |
| `allow_media` | `true` lets the client download [attachments](#attachments) while downloads are on (`WAWARDEN_MEDIA_MAX_BYTES` above 0); no key means `false`. While downloads are off it is stored and reported but grants nothing. While an active client holds it and at least one write chat, the [status](#status-object) carries the `media_write_client` warning. |
| `expires_in_days` | 1 to 365; `0` or no key means 90. A client never lives longer than 365 days, so a token cannot be made that never expires. |

### Backfill requests

A backfill asks the owner's phone for older messages of one chat. On the wire
it is a history request, a message to the owner's primary device (see
[What a send does on the wire](internals.md#what-a-send-does-on-the-wire)).
The phone answers with an on-demand history-sync blob, which arrives like any
other [history](internals.md#history-sync) blob.

The body of `POST /admin/v1/backfill` holds `chat`, a chat identifier as
below or a chat reference (the 32 hexadecimal digits that `GET
/admin/v1/chats` answers as `ref`), and optionally `count`, from 1 to 50, 50
when omitted. The request asks the owner's primary device for the `count`
messages that come before the chat's oldest message in the archive. At most
one request is made every 5 minutes and 20 each UTC day, all chats together;
a request that wrote nothing to WhatsApp (`not_connected`, or `rate_limited`
while a send runs) does not count, and one that may have reached it counts
whatever its answer. When a history notification answers it, the engine marks
the request complete. The route reports `admin_mutation` with the action
`backfill`.

### Chat identifiers in admin requests

A chat identifier is `<digits>@s.whatsapp.net`, `<digits>@lid` or
`<digits>[-<digits>]@g.us`; `@c.us` is read as `@s.whatsapp.net`, and a device
or agent suffix (`:<device>`) is dropped. A phone number that the archive has
already mapped to a LID is stored as the LID. A read chat the archive has not
seen yet is allowed and answered with `known` `false`: the client reads it once
messages arrive. For a client with `allow_first_contact`, the create also stores
each unseen direct write chat as an empty chat, so it has a reference; the
create answers it with `known` `false`, and later answers say `true`. A write
chat must also be a read chat; [Design rationale](internals.md#write-chats-must-be-readable)
explains why.

### Status object

The status object carries no identifier, name or text:

```json
{"state":"connected","reason":"","paired":true,"counts":{"chats":12,"messages":3456,"history_blobs_pending":0,"history_blobs_quarantined":0,"inbox_backlog":0,"inbox_quarantined":0,"backfills_today":1},"clients":{"active":2,"expired":0,"revoked":1,"all_chats_active":1},"warnings":["all_chats_client"],"last_ingest_at":"2026-10-05T08:00:00Z","version":"v1.0.0"}
```

| Key | Content |
|---|---|
| `state`, `reason` | The [engine state](internals.md#engine-states); `reason` is empty unless the state is `disconnected`. |
| `paired` | Whether a device is stored. |
| `counts` | Chats and messages in the archive; history blobs waiting to be processed and quarantined; inbox rows waiting to be applied and quarantined; backfills requested since midnight UTC. |
| `clients` | Clients that are active, expired and not revoked, and revoked; and active clients that read every chat. |
| `warnings` | Fixed codes, empty when nothing needs attention: `all_chats_client` while a client that reads every chat is active, and `media_write_client` while an active client holds `allow_media` and at least one write chat. |
| `last_ingest_at` | When the engine last stored a message, in UTC to the second, or `null` before the first. |
| `version` | The build's version, as `wawarden version` prints it. |

### Admin refusals

The routes refuse with fixed codes:

| Answer | Route | When |
|---|---|---|
| `409` `{"error":"already_paired"}` | pair | A device is stored. |
| `409` `{"error":"owner_phone_missing"}` | pair | `WAWARDEN_OWNER_PHONE` is unset. |
| `429` `{"error":"rate_limited"}` | pair | Three pairing attempts were made in the last hour. |
| `502` `{"error":"pair_failed"}` | pair | Connecting or requesting the code failed, or no code arrived within 25 seconds. |
| `409` `{"error":"already_connected"}` | reconnect | The engine is connected. |
| `409` `{"error":"not_paired"}` | reconnect | No device is stored, or the stored device was rejected and is being logged out. |
| `409` `{"error":"owner_mismatch"}` | reconnect | The stored device's number is not `WAWARDEN_OWNER_PHONE`; see [Pairing](internals.md#pairing). |
| `503` `{"error":"engine_unavailable"}` | pair, reconnect | The engine has stopped because the service is shutting down. |
| `422` `{"error":"name_invalid"}` | create client | The name is empty, longer than 64 characters, not UTF-8 or holds a control character. |
| `422` `{"error":"expiry_out_of_range"}` | create client | `expires_in_days` is negative or above 365. |
| `422` `{"error":"too_many_chats"}` | create client | `read_chats` or `write_chats` holds more than 256 values. |
| `422` `{"error":"chat_invalid"}` | create client | A value is not a chat identifier. |
| `422` `{"error":"all_chats_with_write"}` | create client | `all_chats` comes with a write chat. |
| `422` `{"error":"read_scope_conflict"}` | create client | `all_chats` comes with a read chat. |
| `422` `{"error":"read_scope_missing"}` | create client | Neither `all_chats` nor a read chat is given. |
| `422` `{"error":"write_not_readable"}` | create client | A write chat is not among the read chats. |
| `409` `{"error":"name_taken"}` | create client | Another client has the name. |
| `422` `{"error":"write_chat_unknown"}` | create client | The archive has not seen a write chat, and it is a group or `allow_first_contact` is not set. |
| `404` `{"error":"not_found"}` | show and revoke client | No client has the id. |
| `400` `{"error":"invalid_body"}` | backfill | `chat` is neither a chat identifier nor a chat reference, or `count` is not from 1 to 50. |
| `404` `{"error":"not_found"}` | backfill | No chat has the reference. |
| `422` `{"error":"backfill_no_anchor"}` | backfill | The archive holds no message of the chat, so there is nothing to ask for history before. |
| `429` `{"error":"backfill_quota"}` | backfill | A request was made less than 5 minutes ago, or 20 were made this UTC day. `Retry-After` gives the whole seconds until the next is allowed: up to 300, or until midnight UTC for the daily quota. |
| `503` `{"error":"not_connected"}` | backfill | The engine is not connected or not paired; nothing was sent and the quota is unchanged. `Retry-After: 5`. |
| `429` `{"error":"rate_limited"}` | backfill | A send or another backfill is running; nothing was sent. `Retry-After: 1`. |
| `502` `{"error":"send_rejected"}` | backfill | WhatsApp refused the request. It counts against the quota. |
| `504` `{"error":"send_outcome_unknown"}` | backfill | The request was written and no acknowledgement came. It counts against the quota and is not repeated. |
| `400` `{"error":"invalid_query"}` | list chats | The query holds another key, `match` twice, a malformed escape, or a `match` longer than 64 characters, not UTF-8 or with a control character. |
| `500` `{"error":"internal_error"}` | all | Anything else, such as an archive read that failed; the cause is never sent. |

After the body is decoded, the create route checks these in the order of the
table and answers the first that applies.

## Health listener

| Request | Answer |
|---|---|
| `GET /healthz` while the service is serving and holds the [archive](configuration.md#message-archive) and the [device store](configuration.md#device-store) | `200` `{"status":"ok"}` |
| `GET /healthz` once shutdown has begun, or after the connection of the archive or the device store was lost (`db_lost`) | `503` `{"status":"unavailable"}` |
| Any other method on `/healthz`, including `HEAD` | `405` `{"error":"method_not_allowed"}` with `Allow: GET` |
| Any other path | `404` `{"error":"not_found"}` |

The health listener requires no credential, so its address must be loopback; the
service refuses any other (`health_address_not_loopback`). `200` means the
process is up, its listeners are serving, and the archive and the device store
are open and locked by the service; the state of the
[WhatsApp engine](internals.md#whatsapp-engine) does not change it. The health listener
opens only after both databases, so while `serve` waits for a lock, health
checks fail to connect.

## Responses

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
| `{"error":"internal_error"}` | `500`, when a handler fails or panics, or a client request's [audit row](#read-audit) fails for a reason other than its 2-second wait |
| `{"error":"busy"}` | `503` with `Retry-After: 1`, from the client listener: a [busy read](#busy-reads), a request whose audit row cannot be written within 2 seconds, or a client list being republished (see [Client authentication](#client-authentication)); the [attachment](#attachments) routes also answer it with a longer `Retry-After` |
| `{"error":"unsupported_media_type"}`, `{"error":"body_too_large"}`, `{"error":"invalid_body"}` | `415`, `413`, `400`, from a route that reads a body (`pair`, `reconnect`, `POST /admin/v1/clients`, `POST /admin/v1/clients/{id}/revoke`, `POST /admin/v1/backfill` and `POST /v1/chats/{ref}/messages`) and from `POST /mcp`; see [Request bodies](#request-bodies) and [MCP requests](#mcp-requests) |

The other codes belong to particular routes and are listed with them:

| Codes | Listed under |
|---|---|
| `rate_limited` | [Read rate limits](#read-rate-limits), [Send limits](#send-limits) and [Attachments](#attachments) on the client listener; pairing and backfill under [Admin refusals](#admin-refusals) |
| `media_unavailable`, `media_not_requested`, `media_expired`, `media_too_large`, `media_unsupported`, `media_failed`, `range_not_satisfiable` | [Attachments](#attachments) |
| `invalid_query`, `invalid_cursor` | [Query parameters and pages](#query-parameters-and-pages) and [Cursors and message references](#cursors-and-message-references); `invalid_query` also from `GET /admin/v1/chats` |
| `invalid_idempotency_key`, `invalid_text`, `first_contact_refused`, `request_in_progress`, `idempotency_key_reused`, `not_connected`, `send_rejected`, `send_outcome_unknown` | [Sending](#sending); the last three also from `POST /admin/v1/backfill`, and `not_connected` also from [Attachments](#attachments) |
| `already_paired`, `already_connected`, `not_paired`, `owner_phone_missing`, `owner_mismatch`, `pair_failed`, `engine_unavailable`, the client refusals, `backfill_quota`, `backfill_no_anchor` | [Admin refusals](#admin-refusals) |
| MCP tool codes such as `invalid_arguments` | [MCP tools](#mcp-tools) |

Responses never echo request content, header values or tokens, with one
exception: the MCP library's own refusals at `POST /mcp`, which may repeat the
caller's method name, header values or message text; see [MCP
requests](#mcp-requests). Results of MCP tool calls are JSON-RPC answers with
`Content-Type: application/json`.

Go's HTTP server answers some requests itself, before any handler runs: for
example a request without a `Host` header (`400`), with headers over the 16 KiB
limit (`431`), with an unsupported `Expect` header (`417`) or an unsupported
protocol version (`505`). Those answers have a plain-text or empty body and do
not carry the two headers above.
