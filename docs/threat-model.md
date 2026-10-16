# WaWarden threat model

This document describes WaWarden's security design and the controls that
enforce it, in the code at the commit or tag you are reading. The
[hardening guide](hardening.md) turns it into deployment steps,
[configuration.md](configuration.md) documents the behaviour in detail, and
[agents.md](agents.md) says how an agent should treat what it reads. Each
control under [Secure by construction](#secure-by-construction) and
[Secure defaults](#secure-defaults) names the release that introduced it;
[CHANGELOG.md](../CHANGELOG.md) holds the rest of the version history. Terms
such as LID, re-key, chain head and grant are defined in the
[glossary](glossary.md).

## Contents

- [System summary](#system-summary)
- [Assets](#assets)
  - [The master key](#the-master-key)
- [Actors](#actors)
- [Trust boundaries](#trust-boundaries)
- [Bearer tokens without network binding](#bearer-tokens-without-network-binding)
  - [Failed authentication](#failed-authentication)
  - [Client tokens](#client-tokens)
  - [The admin token](#the-admin-token)
  - [Audit chain](#audit-chain)
- [Security invariants](#security-invariants)
- [Secure by construction](#secure-by-construction)
- [Startup refusals](#startup-refusals)
- [Pairing refusals](#pairing-refusals)
- [Secure defaults](#secure-defaults)
- [Residual risks](#residual-risks)
  - [Account and protocol](#account-and-protocol)
  - [Protocol library](#protocol-library)
  - [Dependencies](#dependencies)
  - [Tokens and the admin token](#tokens-and-the-admin-token)
  - [Timing and volume](#timing-and-volume)
  - [Audit chain limits](#audit-chain-limits)
  - [Sends](#sends)
  - [Clients and agents](#clients-and-agents)
  - [Logging](#logging)
  - [Deletion and storage](#deletion-and-storage)
  - [Backups](#backups)
- [Trust roots](#trust-roots)
- [Reporting](#reporting)

## System summary

WaWarden is one process that links to one personal WhatsApp account as a
companion device and exposes that account to the owner's own agents and
applications. Each client holds a bearer token that grants read access, write
access or both, limited to an allowlist of chats.

The process opens at most three listeners, all plain HTTP, and nothing else:

| Listener | Address | Serves |
|---|---|---|
| Client | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | The REST read API under `/v1` and the MCP tools at `POST /mcp`. A client with a write grant can also send, through `POST /v1/chats/{ref}/messages` and the MCP tool `send_message`. Every route needs a client token. |
| Admin | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082`; opened only when an admin token hash is configured | Metrics, status, pairing, reconnection, client management and history backfill (`POST /admin/v1/backfill`); see [The admin token](#the-admin-token). |
| Health | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`; loopback addresses only | `/healthz`, without authentication |

Outbound traffic is limited to the WhatsApp websocket, the protocol version
fetch and history downloads from WhatsApp's hosts, and an optional
notification webhook. [Secure defaults](#secure-defaults) says when the
service first connects to WhatsApp.

WaWarden does not restrict who can reach its listeners and does not terminate
TLS. Reaching the listeners from other hosts, encrypting that path and
deciding who may use it belong to the deployer: a VPN or mesh network
forwarding to loopback, a private subnet, or a reverse proxy that terminates
TLS. The service must never face the public internet.

Loopback is not an access boundary. Every process in the service's network
namespace can reach all three listeners: processes of other users on the same
host, and other containers in the same pod or task, including a forwarding
sidecar. Those processes must be trusted, or the tokens alone must be enough
protection against them.

## Assets

Most sensitive first.

| Asset | Why it matters |
|---|---|
| Device store (`session.db`) | The linked device's keys and the protocol library's state: Signal sessions, sender keys, message secrets, identity mappings and contact names. Whoever holds it can act as the account until the device is unlinked on the phone. |
| Archive (`archive.db`) and history blobs | Every archived chat, in plaintext. `serve` creates and locks the archive and runs the engine. Once a device is paired, the engine writes WhatsApp traffic into the archive. It keeps each history blob under `history/` until the blob is processed or quarantined. |
| Admin credential | Gates the admin listener; [The admin token](#the-admin-token) lists what it guards. It does not expire. The service holds only its SHA-256. |
| Client tokens | Bearer credentials: whoever holds one and can reach the client listener has its access. The service stores only their SHA-256 digests. A token is shown once, in the answer that creates its client. |
| Master key (`keys/master` in the data directory) | Every purpose key is derived from it. [The master key](#the-master-key) says what its holder, and the holder of a retired key, can do. |
| Audit chain (the `audit` table, and its chain heads on standard output) | The record of every client change and of every client request the read budget admits, REST and MCP alike. Whoever can write `archive.db` and holds the master key can rewrite it; the chain heads shipped on standard output reveal that. Rows older than `WAWARDEN_AUDIT_RETENTION_DAYS` (90 by default) are removed each night after the backup, behind an audit anchor signed under the master key. |
| Webhook signing secret (`WAWARDEN_NOTIFY_SECRET_FILE`) | Shared with the receiver of notification events. Whoever holds it can forge events that the receiver accepts. The service reads it once, at start, and never writes it out. |
| Outbound integrity | Messages sent through WaWarden appear to their recipients as sent by the account owner. The archive stores each one with the sending client's id, shown to readers of the chat as `origin` `gateway:<client id>`. The audit chain records every send request, also one whose client left before the answer. The exception is a send whose row the archive could not take in time (see [Sends](#sends)). |
| Account standing | WhatsApp can restrict or ban an account that links an unofficial client. |
| Backups (`backups/` in the data directory) | One age-encrypted file per backup holds copies of the archive and the device store. Whoever holds a backup and the age identity it was encrypted to holds both databases, the linked device's keys included, and can act as the account. The service holds only the recipient, a public key, and cannot read a backup. While a backup runs, `backups/tmp/` holds a plaintext copy of one database at a time. The newest `WAWARDEN_BACKUP_KEEP` backups are kept. |
| Release artifacts | The binaries and images deployers run. |

### The master key

These keys are derived from `keys/master` with HKDF-SHA256:

- the key of the log pseudonyms;
- the audit chain's key, and the key of the chat HMACs in audit lines;
- the keys that seal the read API's cursors and message references.

Whoever holds the master key can:

- test a guessed identifier against the pseudonyms in the logs and the chat
  HMACs in audit lines;
- recompute the audit chain and sign its audit anchor;
- open and forge cursors and message references, which still never widen a
  client's scope.

`wawarden keys rotate` replaces the master key with a new one and keeps the old
one in `keys/retired/`. The service uses a retired key only to open cursors and
references sealed under it, and to check the audit rows and audit anchor made
under it until the retention removes them. `audit verify` uses it to check rows
and audit anchors made under it.

A retired key gives its holder the same power over what was made under it.
Whoever also can write `archive.db` can, with a retired key:

- remove the audit rows of any key but the current one from the start of the
  chain;
- append rows under the retired key until a row is written under the current
  key;
- once every row written under the current key is older than the audit
  retention's cutoff, replace all of them with rows under the retired key,
  which the next removal signs.

[Audit chain limits](#audit-chain-limits) gives the details.

## Actors

### In scope

- **Any WhatsApp peer or group member.** Sends floods, crafted protocol
  messages, edits, revokes, reactions and poll updates aimed at other chats,
  and text written to manipulate the agents that read it (prompt injection).
- **A malicious or prompt-injected client with a valid scoped token.** Tries to
  read or write outside its scope, to learn whether out-of-scope chats or
  messages exist, or to exhaust shared send budgets.
- **Anyone with network reach to a listener but no token.** This includes
  other processes in the service's network namespace, and web pages open in a
  browser on a host that can reach a listener.
- **A thief of one token.** The token is contained by its chat scope, expiry,
  revocation and rate limits, not by where it is used from.
- **Readers of logs**: anyone who can read the process's standard output or the
  log pipeline it feeds.
- **Readers of backups.**
- **A compromised dependency or a malicious pull request.**
- **A well-meaning contributor** who adds a route, a goroutine, a listener or a
  query without knowing the rules. The construction checks below exist for
  this actor.
- **An operator running defaults.** Defaults must be safe without tuning.

### Out of scope

These are stated so that nobody relies on WaWarden for them:

- **Network access control and transport encryption.** The service listens on
  loopback by default and speaks plain HTTP. Who can reach it, and whether the
  path is encrypted, is the deployer's decision.
- Root or same-user-ID access on the host that runs WaWarden.
- Processes running as the same user on a client host. They can read each
  other's tokens.
- A compromised phone, or a compromised other device linked to the same
  account.
- Whoever holds both a backup and the age identity it was encrypted to.
  Generating the identity away from the service's host, and keeping it there,
  is the operator's task. There is no unencrypted backup.
- Delivery receipts and the other traffic the protocol library sends by itself
  (see [Protocol library](#protocol-library)).
- Deletion residuals in backups, in filesystem blocks the service cannot
  scrub, in full-text index page keys shorter than a trigram, in other stored
  text that holds the same text, and in messages that history sync stores after
  their revocation or edit arrived (see [Deletion and storage](#deletion-and-storage)).
  Other stored text means names, identifiers and messages waiting in the inbox.
- Agents that pass message text into tools. WaWarden marks the text as
  untrusted; what an agent does with it is the agent's responsibility.

## Trust boundaries

1. **Network to client listener.**
   - A request carrying an `Origin` or `Sec-Fetch-Site` header is refused with
     `403` before anything else, so a web page cannot use a browser as a
     client. An `OPTIONS` request is refused with `405`.
   - Every other request is authenticated before any route handler runs and
     before any byte of its body is read. A refused request that announces a
     body gets its answer at once, and the connection is closed.
   - The request carries a client token, checked as
     [Client tokens](#client-tokens) describes. Failed authentications are
     counted and throttled (see [Failed authentication](#failed-authentication)).
   - An authenticated request spends one from the client's read budget before
     routing ([row 22](#22-read-search-and-send-budgets)), and then reaches
     the routes.
   - The peer address is recorded in the audit row for forensics only, and is
     never used for a decision. The service reads no forwarding headers.
2. **Network to admin listener.** The admin listener is separate, so that the
   deployer can restrict it independently of the client listener.
   - The same browser and `OPTIONS` refusals apply.
   - The only check inside the service is the admin token. Without a
     configured hash, the listener is not opened at all.
     [The admin token](#the-admin-token) says what the token must satisfy,
     what it guards and how its misuse is detected.
   - Behind the token, the admin routes answer fixed-shape data. The status
     carries the engine's state and reason, counts, a time and the build
     version, never an identifier. Pairing returns its code to the caller and
     nowhere else.
   - The client routes and the chat lookup answer chat identifiers and names,
     to the administrator only and by design. They answer a client token only
     in the answer that creates its client.
   - `internal/api` reaches the engine and the archive only through interfaces
     that `internal/app` implements.
3. **Loopback health.** The health listener has no authentication. The service
   refuses to start when its address is not loopback, and checks again before
   opening it. It answers only `GET /healthz`: whether the process is up and
   serving, and whether it holds the archive and the device store open and
   locked.
4. **WhatsApp to engine.** Everything that arrives from WhatsApp is untrusted,
   including payloads attributed to the account's own other devices, except
   where the server asserts a field.
   - **Protocol messages.** An edit, revocation, reaction or poll vote is
     applied only to a message found in the chat it arrived in, by identifier
     and sender. An edit or revocation is applied only from the message's
     sender. In a group, a revocation is also applied from a member the archive
     records as an admin. An edit is applied only when it is newer than the
     last edit applied to that message.
   - **Chats named in a message.** A chat named in a message key or a reply
     reference is never followed unless it is the event's own chat. In a
     direct chat, the owner's number also counts, because that is how the
     other side names the chat.
   - **Identity mappings.** A LID is the identifier on WhatsApp's `lid` server
     that stands in for a phone number. Mappings from a LID to a phone number
     are learned only from server-asserted alternates of live messages and from
     history sync. A re-key then moves a chat from the phone number to the LID
     (see the [glossary](glossary.md)).
   - **Unpaired state.** Messages, group changes and history-sync
     notifications that arrive while the engine is `unpaired` are dropped
     before they are written. The engine also enters that state when pairing
     rejects an account.
   - **History sync.** History-sync notifications are accepted only from the
     owner's primary device (device 0). Blobs are downloaded through a size
     cap, persisted before the receipt and before parsing, and decompressed
     under the same cap. A blob is quarantined after three failed attempts, an
     attempt that panics included, so a poison payload is set aside rather
     than replayed forever. Inbox rows are quarantined the same way. A blob's
     plaintext file is deleted once the blob is processed or quarantined, and
     each start of the engine deletes any such file that a crash left behind.
   - **Live traffic.** Live traffic waits in a durable inbox and is
     acknowledged only once written. A message the engine refuses (a full
     inbox, a paused ingest, a failed write) is left unacknowledged, which
     loses it (see [Protocol library](#protocol-library)).
   - **Size.** A message whose identifiers, push name, text and quoted text add
     up to more than 512 KiB is dropped before it is written. This bounds the
     memory one message takes.
   - **The protocol adapter** translates the protocol library's events into
     plain data:
     - it reads a protocol message as a revocation or an edit only when its
       type is set, because the type's zero value means revoke;
     - it passes identifiers with their device parts for the engine to strip;
     - it reads history rows under their conversation's chat, never under a
       row's own key. It keeps an edit row's identifier there, and in the
       messages the owner's phone sends again on request;
     - it decodes blobs with a protobuf recursion limit;
     - before the protocol library saves a pairing, it refuses an account other
       than `WAWARDEN_OWNER_PHONE`, so nothing of a foreign account is stored;
     - it drops QR codes, which carry the device's pairing secret, where they
       arrive.
   - **Tests.** The engine core's rules are tested with plain-data events. The
     adapter is tested with a protobuf fixture for each case that research on
     the protocol library listed, round-tripped through the wire format, fed
     through a real engine and archive, and fuzzed (`internal/engine/wa`).
5. **Service to clients.** Message text, contact names and group subjects are
   written by third parties.
   - Every message object carries `untrusted: true` and its `origin`. `origin`
     is `gateway:<client id>` for a message a client sent through WaWarden,
     `owner` for any other message the owner's account sent, and `peer`
     otherwise. None of these makes the text trustworthy, since the owner can
     paste third-party text.
   - Every message object also carries a `text_display` field. It removes
     control, bidirectional-override, zero-width, tag and every other format
     character, keeps newline and tab, and replaces invalid UTF-8. The archive
     stores the field beside a message's text when it writes the message and
     when it applies an edit. The read API and the MCP tools return both.
   - MCP tool names, descriptions and schemas are built from constants and Go
     types, so no chat-controlled string reaches a tool definition
     ([row 11](#11-golden-files-for-mcp-tools)).
   - The names the owner saved for contacts come from the contact list the
     owner's phone syncs, not from the contacts themselves. A read returns one
     only when that contact's direct chat is in the client's scope, and returns
     it as the phone sent it.
   - How an agent should treat all of this is in [agents.md](agents.md).
6. **Service to logs, metrics and notifications.** Chat identities leave the
   process only as keyed HMACs. Message text, query text, tokens and protocol
   messages never leave it.
   - No request is logged. Failed authentications are counted, not logged.
   - A startup refusal names the variable involved, never its value.
   - A recovered panic is logged with its type and stack, never its value.
   - Every line `serve` writes to standard output passes through one log
     writer. [Row 13](#13-one-log-writer) is the canonical statement of its
     rules: token redaction, identifier pseudonyms, dropped XML lines and how
     values are rendered. Identifiers in other forms, such as a phone number on
     its own, pass unchanged, so code must not log them.
   - The protocol library's logger and the Signal library's logs reach that
     writer through an adapter. The MCP library is given no logger, so its log
     output is discarded ([row 24](#24-the-mcp-endpoint-behind-the-client-pipeline)).
   - The engine core's log events and metrics carry states, counts, fixed
     reason codes and the Go types of errors. They never carry an identifier,
     a name, message text or a pairing code, which a test checks with canary
     content.
   - Operational events go through one notifier
     ([row 19](#19-one-notifier)), which writes each one with a fixed set of
     fields.
   - Metrics carry build information, states, counts, fixed reason codes and
     the names of goroutines that panicked. Metrics in embedded metric format
     (EMF) are written through the log writer.
7. **Service to data directory and backups.**
   - The data directory must be owned by the service's user with mode `0700`,
     and the service creates it that way.
   - The service creates `keys/` and the master key in it. It refuses to start
     unless `keys/` is a directory with mode exactly `0700` and the master key
     a regular file of 32 bytes with no permission for group or others. Both
     must be owned by the service's user, and neither may be a symbolic link.
   - `archive.db` and its journal must be regular files owned by the service's
     user with no permission for group or others. `session.db` and its journal
     are checked the same way, and opened with the archive's settings and
     lock.
   - `history/` and `backups/`, when present, must be directories with mode
     exactly `0700` owned by that user.
   - Backups are encrypted to an operator-supplied age recipient, so the
     service can write them but not read them. Before each backup, `backups/`
     and its staging directory `backups/tmp/` must be directories, not
     symbolic links, of mode exactly `0700` owned by the service's user.
   - [Startup refusals](#startup-refusals) and
     [row 20](#20-recipient-only-backups) give the rest.
8. **Source to release artifacts.** Releases are built only from `main` by a
   manually dispatched workflow that waits for the maintainer's approval. Each
   is rebuilt independently and compared bit for bit, attested and signed. The
   approval gate and the immutability of published releases come from
   repository settings: a protected `release` environment, immutable releases
   and a tag ruleset. The workflow checks the environment before building, and
   fails after publishing a release that is not immutable. It cannot check the
   tag ruleset. See [RELEASING.md](../RELEASING.md).
9. **Service to the webhook receiver.** This is the canonical statement of the
   webhook's rules.
   - When `WAWARDEN_NOTIFY_URL` is set, each notification event is posted to
     it with the same fields as its log line, an event id and a time.
   - Each post is signed with HMAC-SHA256 over the id, a timestamp and the
     body, under an operator-supplied secret. A receiver can therefore refuse
     forged, altered and replayed deliveries.
   - The URL must be `https`, without credentials. The client takes no proxy,
     follows no redirect, and uses TLS 1.2 or later and timeouts. It reads at
     most 4 KiB of an answer, which it never logs.
   - Its dialer checks every resolved address, on every connection. It always
     refuses link-local, metadata, multicast, reserved and unspecified
     addresses, every IPv6 address with a zone, and the local-use NAT64
     prefix. It checks an address in the well-known NAT64 prefix as the IPv4
     address it embeds. It refuses loopback, private and shared addresses
     unless the operator allows them. So a misconfigured URL, or a name that
     resolves inward, cannot make the service post to the cloud metadata
     endpoint.
   - The configuration is the operator's. The check guards against mistakes,
     not against whoever can set the service's environment.
   - A bounded queue and one sender keep a slow or failing receiver from
     holding up the engine, the admin routes or shutdown. Events it cannot
     deliver are counted and dropped. Those dropped when the shutdown's grace
     period runs out are also reported in one line, and make `serve` exit `1`.
10. **Admin CLI to the admin listener.** This covers `wawarden admin status`,
    `pair`, `reconnect`, `clients`, `chats list` and `backfill`.
    - They take the token from a file, standard input or a command, never from
      an argument or the environment, and check its form before sending it.
    - They take no proxy, follow no redirect, warn when plain HTTP leaves
      loopback, and cap what they read.
    - Everything they print from an answer passes through the terminal
      sanitiser.
    - A flag they cannot parse is named, never repeated as typed, so a token
      typed as a flag's value does not reach standard error.
    - The token command runs without a shell, with its standard input and
      error closed, its output capped and a timeout. It runs in its own process
      group, is killed after it finishes, and gets no `WAWARDEN_` variable in
      its environment.
    - `admin clients create` prints the new client token once, on standard
      output, and nothing shows it again.
    - `wawarden audit verify` contacts no service. It reads a copy of the
      archive, read-only, with a copy of the master key and of the retired
      keys.
    - `wawarden keys rotate` contacts no service either. It reads `archive.db`
      once, read-only, to refuse while a service holds it, and writes only
      under `keys/`. It prints key ids, never a key.

## Bearer tokens without network binding

A token is a pure bearer credential. It is not bound to a host, a network
address or a network identity, and the service contains no network gate. This
section is the canonical statement of the trade-offs that follow:

- **A stolen token works from anywhere with network reach to the service.**
  Nothing in the service ties it to a place.
- **A mistake in the deployer's network setup is not caught by the service.**
  The service warns at start only for a client or admin listener bound to a
  non-loopback address. When a forwarder, sidecar or reverse proxy in front of
  loopback listeners exposes them too widely, the service binds loopback and
  logs nothing. The [hardening guide](hardening.md) describes deployments that
  avoid this.
- **Plain HTTP exposes bearer tokens** on any network segment the deployer
  leaves unencrypted.
- **The admin surface is guarded by its token alone** (see
  [The admin token](#the-admin-token)).

What limits a client token instead:

- its chat scope;
- a mandatory expiry: 90 days by default and 365 days at most. A token that
  never expires cannot be created;
- revocation, checked on every request;
- per-client read and search budgets
  ([row 22](#22-read-search-and-send-budgets));
- for write tokens, the send budgets and the first-contact rule;
- the failed-authentication throttle, which counts but does not stop guessing
  (below).

### Failed authentication

The client and admin listeners each count failed authentications, in
`wawarden_auth_failures_total` and `wawarden_admin_auth_failures_total`. Each
listener has one failure budget shared by all callers: 30 failures, refilled
at one per second. While it is empty, failures are answered `429` instead of
`401`.

This throttle is not a defence against guessing:

- Authentication still runs for every request, and a valid credential is
  always accepted. Legitimate callers cannot be locked out, and a correct guess
  succeeds whether or not the budget is empty.
- Anyone who can reach a listener can keep its budget empty. Callers with a
  wrong token then see `429` instead of `401`.

The entropy of the tokens is what defeats guessing. The counters exist to
alert on.

### Client tokens

- **Form and storage.** The admin creates a client through the admin listener
  only, with a name, a read scope (chosen chats, or all chats) and an expiry.
  - The service draws a token of 64 characters: `ww_`, the client's identifier
    from 5 random bytes, 32 random bytes in base64url, and a CRC-32 that lets
    secret scanners recognise it. Only its SHA-256 is stored.
  - The token is answered once, under the key `credential` of the create
    answer. That is the only response type that can carry that key, which a
    reflection test checks. The CLI prints it once on standard output.
  - No log line, event, audit line or other answer carries it. Tests of the
    store, the service and the CLI check this with the generated token as a
    canary.
- **One path for every outcome.** Authentication runs the same steps for every
  token:
  1. It parses the token and looks its identifier up in an in-memory copy of
     the clients.
  2. It hashes the presented token and compares the hash, in constant time,
     with the stored one. When the token is malformed or its identifier
     unknown, it compares with a random dummy drawn at start instead.
  3. Only then does it check revocation and expiry.

  A deterministic test counts exactly one hash and one comparison for a valid,
  malformed, unknown, wrong, revoked and expired token. The digest type cannot
  be compared with `==` ([row 12](#12-constant-time-credential-comparison)).
  The look-up never queries the database, so failed guesses cannot queue on
  the archive's connection. Every failure is the same `401` and spends the
  failure budget.
- **A copy that is not current.** While the in-memory copy is not current,
  every token is answered `503` (`busy`) with `Retry-After: 1`, after the same
  hash and a comparison with the dummy. That answer counts no failure and
  spends no failure budget.
- **Revocation.** These changes mark the in-memory copy stale and rebuild it
  inside their transaction: creating a client, revoking one, and a re-key that
  moves a client's chat from a phone number to a LID.
  - The new copy is published once the transaction commits, and before the
    change returns, so the next authentication sees it.
  - An authentication between the mark and the publication is never answered
    from the old copy. It waits for the new one, for at most the 2-second read
    deadline, and otherwise answers `503`.
  - A change that does not commit publishes nothing. The copy is then
    reloaded from the archive in the background, and authentications answer
    `503` until it is.
  - A request already authenticated finishes with the grant it holds, within
    the 2-second read deadline.
  - Tested with the real client list in `internal/store/admin/cache_test.go`,
    `internal/store/scoped/auth_http_test.go` and
    `internal/api/pipeline_test.go`.
- **Write chats are readable chats.** Write chats must be a subset of read
  chats, and a client that reads every chat holds no write chat. Database
  triggers enforce this too. One of them refuses to replace a client row,
  since a replacement would keep its chats without the update trigger running.
  A client allowed to write to a chat it cannot read could learn, from the
  answer to a reply, whether a message exists.
- **Unknown chats.** A write chat the archive has not seen is refused, unless
  it is a direct chat and first contact is allowed. A read chat not seen yet is
  allowed, and reported as not known.
- **All-chats clients.** A client that reads every chat is created only by
  asking for it explicitly. The CLI warns. The admin status reports the number
  of active ones, and the `all_chats_client` warning while one is active.
- **Rotation.** A token is never renewed or shown again. Rotating it means
  creating a client with the same scope under a new name, switching the agent
  to the new token, and revoking the old client; see
  [agents.md](agents.md#tokens).

### The admin token

This section is the canonical statement of what the admin token is, what it
guards and how its misuse shows.

- **Only generated tokens are accepted.** `wawarden admin init` prints a token
  made of `wwadm_`, 32 bytes from the operating system's cryptographic random
  source in base64url, and a CRC-32 checksum that lets secret scanners
  recognise it.
  - The admin listener refuses any presented string that does not have
    exactly this form, even when its SHA-256 matches the configured hash. A
    chosen passphrase therefore cannot serve as the admin token.
  - The service stores and compares an unsalted SHA-256, in constant time.
    That hash travels in configuration that many people and systems can read:
    container or task definitions, `docker inspect` output, CI logs. With 256
    random bits behind it, recovering the token from the hash is infeasible.
- **It does not expire.** Rotate it by generating a new token, replacing the
  configured hash and restarting the process. The hash is read only at start.
- **It is a single factor.** Within whatever network boundary the deployer
  provides, anyone who can reach the admin listener and holds the token can:
  - read the Prometheus metrics;
  - read the status: the engine's state, counts and the last ingest time,
    never an identifier;
  - request pairing codes for the owner's number while no device is paired,
    which shows a notification on the owner's phone. Pairing can link only the
    number in `WAWARDEN_OWNER_PHONE`, is refused while a device is paired, and
    is limited to three attempts an hour (see
    [Pairing refusals](#pairing-refusals));
  - request reconnections, which are not rate-limited beyond the token. A
    caller repeating them while the engine is connecting makes it dial
    WhatsApp again each time;
  - create and revoke clients, writing ones included, and list every chat's
    identifier and name;
  - request a history backfill, within its quotas.
- **The pairing code crosses the network** in plain HTTP unless the deployer
  encrypts the path. A code is useful only to link the gateway itself, on the
  owner's phone, within WhatsApp's validity window.
- **Metrics scrapers hold the admin token.** Prometheus metrics are readable
  only with it, and scrape configurations are often widely readable. Treat any
  scrape configuration as holding the admin token. Prefer the EMF metrics,
  which need no token, for alerting.
- **Detecting its misuse.** Its use is made visible, not prevented:
  - failed admin authentications are counted in
    `wawarden_admin_auth_failures_total`, and reported in the
    `admin_auth_failure` event, at most once a minute with a count and nothing
    about the requests, and in the EMF `AdminAuthFailures` metric, which needs
    no token;
  - every call of a route that changes state reports an `admin_mutation`
    event with its action and outcome;
  - both events reach the webhook when one is configured;
  - each client the token creates or revokes is recorded in the
    [audit chain](#audit-chain) with the client's identifier, which events
    never carry.

Keep the admin listener reachable from fewer places than the client listener.

### Audit chain

#### What it records

- **Client changes.** One row per client change, appended in the same write
  transaction as the change, so the change fails when the row cannot be
  written.
- **Client requests.** One row per client request that the read budget
  admits, allowed or refused, appended before the answer is sent. A request
  whose row cannot be written returns nothing it read: it is answered `503`
  (`busy`) when the row waited 2 seconds for the archive connection, and `500`
  otherwise.
- **Requests whose client left.** The row is written whether or not the client
  is still connected. Hanging up while a send waits for its acknowledgement
  therefore does not keep the send out of the chain. A send that went out
  before its row failed is a residual risk (see [Sends](#sends)).
- **What it does not record.** Failed authentications and requests refused by
  the read budget write no row. Guessing and flooding therefore cannot grow
  the table faster than the budget allows.
- **The chain.** Each row's HMAC-SHA256, under a key derived from the master
  key, covers its fields and the previous row's HMAC.
- **The chain head.** After the commit, one line on standard output carries
  the client identifier, the action, the outcome, an HMAC of the chat (never
  the chat), the key id, and the first 16 bytes of the row HMAC: the chain
  head.

#### What verification shows

`wawarden audit verify` recomputes the chain from a read-only copy, with the
master key and the retired keys, each row under the key its key id names. It
reports:

- an edited row, a forged HMAC, or a row deleted from the middle;
- a chain recomputed with another key;
- a row written under a key it was not given;
- a row written under a retired key after one written under the current key.

It checks the audit anchor's MAC with the key whose id the audit anchor names,
and starts the walk from it:

- an edited audit anchor is `anchor_invalid`;
- a removed audit anchor, or a removed first retained row, is an `id_gap`;
- rows cut from the start behind an audit anchor copied from the last of them,
  without the key, are `anchor_invalid`;
- so are rows cut from the start behind an audit anchor made under a retired
  key, when no row is left or the first one left was written under the current
  key. No removal leaves that state.

A shipped head that the copy lacks counts as pruned only when two things hold:
its log time is not after the audit anchor's, and, behind an audit anchor made
under a retired key, its line names a key other than the current one. The
verification and tamper matrices, with one and two keys, are in
`internal/store/admin/audit_test.go`, `internal/store/admin/prune_test.go` and
`cmd/wawarden/audit_test.go`.

#### Retention

- **When.** Each night, after the backup of the night's slot and never while
  it is owed, the scheduler removes the rows written more than
  `WAWARDEN_AUDIT_RETENTION_DAYS` days before. The setting runs from 7 to 3650
  and defaults to 90. No setting keeps rows forever.
- **How much.** It works in transactions of at most 10,000 rows, for at most
  15 minutes. It never removes the newest row, so the chain never restarts
  from its genesis.
- **After the first backup.** With a backup recipient, nothing is removed
  before the first backup succeeds. The retention therefore also waits while
  no device is paired and while the first backup fails.
- **The audit anchor.** Each transaction first moves the audit anchor to the
  last row it removes. The audit anchor holds:
  - the row's id, key id and HMAC;
  - the latest time of the rows removed so far, but never more than 10 seconds
    (the archive's write timeout) after the removal's cutoff;
  - the removal time;
  - an HMAC over them under the current master key's audit-chain key.
- **Checks before signing.** Before it moves the audit anchor, the transaction
  checks the audit anchor's MAC under the keys the service holds: the master
  key and the retired keys. It then recomputes the HMACs of the rows it removes,
  from the audit anchor's or from the genesis, with the rules of
  `audit verify`:
  - consecutive ids;
  - each row under the key its key id names;
  - no row under a retired key once the audit anchor or an earlier row names
    the current key;
  - no row under the current key first behind an audit anchor made under a
    retired key.
- **A failed check.** When a check fails, that transaction moves and removes
  nothing, and logs `audit_prune_refused` with reason `chain_mismatch`. The
  retention stops until the next night, when it tries again. Transactions that
  committed earlier that night keep what they removed. Rows under a key the
  service does not hold are never removed.
- **Confinement.** Architecture tests (`internal/archtest/sends_test.go`)
  keep the retention in one place:
  - only `internal/store/admin/prune.go` holds the statement that deletes
    audit rows, comments or a quoted schema name inside it included;
  - only it and the ingest schema hold SQL that writes the audit anchor;
  - only `internal/app`, the admin store and the backup taker may name a
    `Prune` method.

  The first two read string literals and statements assembled from a file's
  constants. They guard against mistakes. The audit anchor's MAC, which
  `audit verify` checks, is what detects a moved audit anchor.

#### Triggers and shipped heads

Database triggers refuse:

- an `UPDATE` or a replacing `INSERT` of a row;
- a `DELETE` of a row that the audit anchor does not cover, or that is the
  newest row;
- moving the audit anchor back, or removing it.

Whoever can write `archive.db` can drop the triggers. Whoever also holds
`keys/master` can recompute the whole chain after editing it, or delete rows
from its end, and the table then verifies. Only the chain heads already
shipped off the host reveal that, through `audit verify --log`, which reports
every shipped head the copy lacks. A test shows a truncated tail verifying
without the heads and failing with them.

The root of trust for the audit chain is therefore the log pipeline, not the
key: ship standard output where the host cannot rewrite it.

## Security invariants

These properties must hold in every release.
[Secure by construction](#secure-by-construction) lists the mechanisms that
enforce them.

- **I-0 Authentication first.** On the client and admin listeners, no route
  handler runs and no request body is read until the request presents a valid,
  unexpired, unrevoked credential.
- **I-1 Read scope.** A client receives data only from chats in its read scope.
- **I-2 Write scope.** A client sends only to chats in its write scope. A client
  that may read every chat cannot write at all.
- **I-3 Resolution in the chat.** Every entity a request refers to, such as a
  `reply_to` reference, and every protocol message from WhatsApp (edit, revoke,
  reaction, poll update) is resolved inside the chat it belongs to, never by
  identifier alone.
- **I-4 Denied equals missing.** A request for something outside the client's
  scope and a request for something that does not exist receive
  byte-identical responses.
- **I-5 Nothing identifying leaves the process.** No chat identity in plaintext
  and no message content, query text, token or protocol message reaches logs,
  metrics, notifications or MCP tool definitions.
- **I-6 Fixed network surface.** The process listens only on the client
  listener, the admin listener (when configured) and the loopback health
  listener.
- **I-7 No plaintext credentials in the service.** The admin credential is
  configured as a hash; client tokens are stored as hashes.
- **I-8 Deletions are honoured on disk.** Once the write that revokes, edits or
  expires a message has returned without error:
  - the message's row no longer holds its old text;
  - after a revoke or an expiry, the row no longer holds the message's media
    type, and the archive no longer holds its media reference;
  - the full-text index, page keys included, keeps only those of its trigrams
    (the three-character runs the index stores) that a remaining message
    holds;
  - neither that text nor any of its trigrams, nor, after a revoke or an
    expiry, that media type or media reference, is in the database or its
    journal, unless the archive still holds the same text, trigram or value
    elsewhere. Elsewhere means a remaining message, a message waiting in the
    inbox, a stored name (a push name, saved contact name, chat name or group
    subject), an identifier or another stored value, or the schema.

  The exceptions are backups, a page key that holds less than a whole trigram,
  a trigram left by a stop or a failure between the deletion and the index
  rewrite it requires, and a revocation or edit that arrives before history
  sync has stored its target (see [Deletion and storage](#deletion-and-storage)).
- **I-9 Panics are contained.** A panic is recovered, counted and logged
  without its value when it happens in a goroutine that WaWarden starts, in one
  of its HTTP handlers, or in the protocol adapter's event handler, which runs
  on the protocol library's goroutines. Other code on goroutines that
  dependencies start is not covered (see [Dependencies](#dependencies)).

## Secure by construction

Each mechanism makes a class of mistake fail to compile or fail a test, rather
than relying on review. Each subsection has a stable row number, so other
documents can cite it as "row 4". Each gives the mechanism, what enforces it,
its limits where a check can be passed around, and the release that introduced
it.

The architecture tests live in `internal/archtest`, and
[CONTRIBUTING.md](../CONTRIBUTING.md) says how to run them and fix a failure.

- They parse every Go file of the module with the standard library's
  `go/parser`.
- They build fixture packages, and fixture files added to package policy with a
  `go build` overlay, that must fail to compile with a specific error.
- Each rule has violating and conforming self-tests.
- They read only Go. So the walk that feeds them fails, and with it the test
  that runs every rule, when the module holds something the go tool can build
  in without their reading it. That is an assembly, C, C++, Objective-C,
  Fortran, SWIG or system object file, a symbolic link, or a nested module.

These rules keep the walk complete and the source honest. Each reads every
file, test files included:

- **`hidden-packages`.** The walk and the linters skip directories named
  `testdata` or `vendor`, and those whose name starts with `_` or `.`. No file
  may import a package of this module from such a directory, since it would be
  built in without being checked.
- **`generated-headers`.** No file may carry a generated-code header
  (`// Code generated ... DO NOT EDIT.`), which makes linters and reviewers
  skip the file. The module holds no generated code.
- **`nolint`.** A `//nolint` directive must name its linters, and may not
  silence `depguard` or `forbidigo`.
- **`credential-literals`.** No string literal or comment may hold a whole
  admin or client token of the generated shape. Secret scanners would read one
  as a live credential, so test vectors are assembled from parts.

### 1. Grant types

**Mechanism.**

- `ReadGrant`, `WriteGrant` and `AdminGrant` are declared with only unexported
  fields in `internal/policy/internal/seal`. Package policy exports them under
  the same names.
- Outside the seal package, a valid grant can only come from the decision
  functions in `internal/policy/decide.go`.
- Decisions deny by default. A missing, revoked, expired or never-expiring
  client gets no grant. A client without write chats, or one that may read
  every chat, gets no write grant. The zero value of every grant allows
  nothing.
- Grants copy the client's chat sets, so changing a client afterwards does not
  change a grant.
- The scoped store, `internal/store/scoped`, is how `internal/api` reads the
  archive. Every exported method of its exported types takes a
  `policy.ReadGrant` value as its first parameter, except `Query.Expression`,
  which reads nothing. Its constructor takes the database handle, which no
  package outside `internal/store` can name.
- Only such a method may call `Reader.read`, the package's one way to the
  database ([row 3a](#3a-one-database-handle)). It calls it only on its
  receiver, with its own first parameter as the grant. It may otherwise use that
  grant only as the receiver of the grant's methods: never assigned, shadowed
  or passed on.
- Only `internal/api` may reference `policy.DecideRead`, `policy.DecideWrite`
  or `policy.DecideAdmin`. Package policy names its own functions without an
  import. So the scoped store cannot decide a grant of its own.

**Protects.** I-1, I-2, I-7.

**Enforced by.**

- The compiler. The fields belong to the seal package, so every other package,
  package policy included, cannot write a grant literal with fields, convert
  from a look-alike type, write a field or take a field's address. Fixtures in
  `internal/archtest/testdata` prove it, outside and inside package policy.
- The architecture rules `seal-constructors` ([row 4](#4-chat-identifier-normaliser)),
  `scoped-grant-first`, `scoped-read-path` and `grant-decisions`.
- Unit tests of every decision, of zero-value grants and of grants built
  without their validity flag, and a reflection test that the grant types have
  no exported fields (`internal/policy/decide_test.go`).
- That each scoped method's rows honour its grant is property-tested
  ([row 7](#7-property-tests)).

**Limits.** See [row 3a](#3a-one-database-handle) for what the
scoped-read-path rule cannot see.

**Since.** v0.1.0.

### 2. Registration helpers

**Mechanism.**

- Client and admin routes are registered only through the unexported helpers
  `read`, `write` and `admin` in `internal/api`. Each obtains a grant from the
  matching decision before its handler runs. When the decision denies, it
  answers the same `404` as an unknown route.
- The mux is unexported, and only `internal/api/register.go` may create one or
  register on it. So a route without a policy class cannot be added from
  outside.
- Authentication runs before the mux. The health listener serves only
  `/healthz`.
- `POST /mcp` is registered through a fourth helper, `mcp`, with its own class
  `mcp`, behind the same pipeline.
- Each MCP read tool is registered through `readTool`, which obtains a read
  grant from `DecideRead` on every call and answers `not_found` when it is
  denied. `send_message` is registered through `writeTool`, which obtains a
  write grant from `DecideWrite` on every call the same way. The send route is
  registered through `write`.
- Only `internal/api` serves requests, and it reads JSON bodies only. No code
  names a `net/http` form-parsing method: `ParseForm`, `ParseMultipartForm`,
  `FormValue`, `PostFormValue`, `FormFile` or `MultipartReader`.
  `internal/api` may import neither `text/template` nor `html/template`.
- The MCP library's registration surface is confined. The server's `AddTool`
  method and the package's generic `AddTool` may appear only inside `readTool`
  and `writeTool` in `internal/api/tools.go`. The server's construction and
  middleware may appear only inside `newMCPEndpoint` in `internal/api/mcp.go`.
  Prompts, resources and custom methods appear nowhere. Each of the endpoint's
  two servers admits exactly the tool names its helpers recorded.

**Protects.** I-0, I-1, I-2, I-4.

**Enforced by.**

- Compile-time fixtures that reach the router from another package.
- The architecture rules `mux-ownership`, `form-parsing` and
  `mcp-registration`.
- A coverage test that compares every registered route and its class with a
  golden list (`internal/api/coverage_test.go`).
- Pipeline tests that authentication precedes routing and that no body is read
  before it, form-encoded and multipart bodies included
  (`internal/api/pipeline_test.go`).

**Limits.**

- `form-parsing` refuses, in every non-test file, a reference to those method
  names, and any string literal or concatenation of literals in which one
  appears as a word. A name assembled from named constants or at run time, or
  written outside Go source, such as a template file embedded with
  `go:embed`, passes it.
- `mcp-registration` recognises a method called, taken as a method value or as
  a method expression, under any import name, by name inside `internal/api`.
  That is the only package that may import the MCP library.

**Since.** v0.1.0.

### 3. Import fences

**Mechanism.**

- `internal/api` may not import `database/sql`, `text/template`,
  `html/template`, the SQLite driver, the protocol library, any package under
  `internal/engine`, or `internal/sender` ([row 26](#26-the-send-path)). Of
  the packages under `internal/store` it may import only `internal/store/scoped`,
  whose every read takes a grant ([row 1](#1-grant-types)). It reaches the
  engine and the sender only through interfaces that `internal/app` wires.
- Outside test files, only packages under `internal/store` may import
  `database/sql`, and only `internal/store/internal/db` the SQLite driver.
- Outside test files, only `cmd/wawarden/tokencommand.go`, the admin CLI's
  token command, may import `os/exec` or name `os.StartProcess`,
  `syscall.Exec`, `syscall.ForkExec` or `syscall.StartProcess`.
- No non-test file may import `mime/multipart`.
- `internal/policy` and its seal package may import only an allow-list of
  standard-library packages, which excludes networking, operating-system and
  filesystem packages.
- Package policy may also import the seal package, and no other package may.
  Go's `internal` rule refuses it outside `internal/policy`, which a fixture
  proves. The `seal-constructors` rule refuses it in every file except those of
  package policy and of the seal package's own directory, test files included.
  So an external test package `policy_test` may not import it either.
- `net/http/pprof`, `expvar`, `net/http/cgi`, `net/http/fcgi`, `plugin`,
  `unsafe` and cgo are banned everywhere.
- Outside test files, only `internal/api/dto` and `internal/logx` may import
  `reflect`. No file may use a selector named `NewAt`, `SetPointer`,
  `UnsafeAddr` or `UnsafePointer`: the `reflect` functions and methods that
  read or write memory through an unsafe pointer without importing `unsafe`.
- The non-test files of `internal/cursor`, `internal/keys`, `internal/logx`,
  `internal/ratelimit` and `internal/sanitize` may import only the standard
  library.
- A module outside the standard library may be imported only when it is on a
  reviewed list:
  - the SQLite driver, `modernc.org/sqlite`;
  - the protocol library, `go.mau.fi/whatsmeow`, and the Signal library it
    uses, `go.mau.fi/libsignal`;
  - protobuf, `google.golang.org/protobuf`;
  - the backup encryption library, `filippo.io/age`;
  - the Unicode text library `golang.org/x/text`, which outside test files
    only `internal/store/scoped` may import, to normalise search queries to
    NFC;
  - the MCP library, `github.com/modelcontextprotocol/go-sdk`, and its JSON
    Schema library, `github.com/google/jsonschema-go`, which outside test
    files only `internal/api` may import;
  - the property-test library `pgregory.net/rapid`, which only test files may
    import, so it is never linked into the binary.
- `go.mod` may require only those modules, and, marked `// indirect`, the 28
  modules on a second reviewed list of what they require.
  `modernc.org/libc` must be at exactly the version the driver's own `go.mod`
  names, as the driver's documentation demands.

**Protects.** I-1, I-5, I-6.

**Enforced by.**

- `depguard` in `golangci-lint`.
- The architecture rules `import-fences`, `banned-imports`, `reflection`,
  `third-party-imports`, `standard-library-only`, `process-execution` and
  `confined-imports` (`internal/archtest/imports_test.go`,
  `confinement_test.go`, `exec_test.go`). The banned-import, third-party and
  reflection-selector checks cover test files too.
- The fence's self-test accepts `internal/store/scoped` and refuses its
  subpackages, `internal/store/ingest`, `internal/store/admin`,
  `internal/store/session` and `internal/store/internal/db`.

**Since.** v0.1.0.

### 3a. One database handle

**Mechanism.**

- The raw `*sql.DB` lives only in `internal/store/internal/db`, the only
  importer of the SQLite driver. Other packages reach it through its `Read` and
  `Write` methods, each of which runs under a deadline inside a transaction.
- That package alone names a database file, opens, inspects or removes one,
  and runs `PRAGMA`, `VACUUM`, `ATTACH` or `DETACH`. So no other code of this
  module can change a verified setting, open another database through SQL, or
  close a descriptor of the database file and so drop its lock.
- Everywhere else, the SQL text passed to `Exec`, `Query`, `QueryRow` or
  `Prepare`, and their `Context` forms, is a string literal, a constant
  declared once in the same file, or a concatenation of those. So is each
  schema step passed to that package's `Migrate`, in a slice literal at the
  call. No SQL is built from values.
- In `internal/store/scoped`, only `Reader.read` reaches the handle. It
  resolves the caller's grant to a scope and hands its callback a scoped
  querier. The querier's `QueryContext`, and the row collector's, take only
  constants of the calling file. Its single-row `scalar` runs only three
  reviewed constants ([row 5](#5-one-in-list-builder)). Only `inbuilder.go`
  turns a query's scope markers into SQL.
- The connection's settings are [row 15](#15-verified-storage-and-deletion-on-disk).

**Protects.** I-1, I-3, I-8.

**Enforced by.**

- Go's `internal` package rule, and `depguard`.
- The architecture rules `confined-imports`, `database-files`,
  `sqlite-statements`, `constant-sql`, `scoped-read-path` and
  `grant-decisions` (`internal/archtest/storage_test.go`, `scoped_test.go`,
  `decisions_test.go`):
  - `sqlite-statements` reads every string literal and concatenation of
    literals, and also the whole text of each SQL argument and schema step
    with the file's constants resolved. A statement split across constants is
    therefore refused.
  - `constant-sql` refuses schema steps that are not such a slice literal, and
    those methods and `Migrate` used as values.
  - `scoped-read-path` confines each selector, type and literal that reaches
    the handle, the querier or the scope to the one function of the scoped
    store that needs it. It refuses conversions to those types, `new` of
    either, and another type name declared as either.
  - `scoped-read-path` also refuses a call through a selector, with arguments
    and on any receiver, of a method named like one of the package's read
    methods. A test holds that list equal to the methods the package declares.
    So no read method calls a sibling that way, which would read under a grant
    that is not the caller's own.

**Limits.**

- A database file name assembled at run time from parts that are not literals
  passes `database-files`.
- A read method taken as a value and called later, such as
  `f := r.Chats; f(…)`, passes `scoped-read-path`. Without type information it
  cannot be told apart from a field of the same name. Such a call would still
  need a valid grant other than the caller's own, which only a decision could
  produce, and `grant-decisions` refuses a decision there.
- The tests catch mistakes, not a deliberate edit of that package or of the
  tests themselves, which review covers.

**Since.** v0.2.0.

### 4. Chat identifier normaliser

**Mechanism.**

- `CanonicalChat` is `seal.Chat`, declared in the seal package with unexported
  fields and a validity flag. Its zero value is invalid.
- Outside the seal package, a valid chat can only come from `Normalize`, in
  `internal/policy/normalize.go`.
- **Servers.** `Normalize` accepts `s.whatsapp.net`, `lid` and `g.us`, and
  `c.us`, which it maps to `s.whatsapp.net`, in lower case only. It rejects
  every other server, `broadcast`, `newsletter`, `hosted`, `hosted.lid`, `bot`,
  `msgr` and `interop` included.
- **Users.** Before the `@` of a phone-number or LID user it accepts `USER`,
  `USER:DEVICE` or `USER.AGENT:DEVICE`. `USER` is 1 to 24 digits, `AGENT` 1 to
  3 digits of value at most 255, and `DEVICE` 1 to 5 digits of value at most
  65535. It drops the agent and the device, and rejects an out-of-range value
  instead of wrapping it.
- **Groups.** Before the `@` of a group it accepts 1 to 24 digits, or two such
  runs joined by `-`.
- **Rejections.** Digits are ASCII only. Any other byte, the empty string and
  an input longer than 128 bytes are rejected. A rejection returns the zero
  value and allocates nothing. An accepted input allocates only to build and
  intern its result.
- **Result.** The result is the user or group, `@` and its server.
  Normalising it again returns the same value.
- **Printing.** A chat holds its identifier as a `unique.Handle[string]`. So
  `fmt` and the text handler of `log/slog`, which print unexported fields,
  print a pointer in its place, and `encoding/json` and the JSON handler of
  `log/slog` print `{}`. It exposes the identifier only through `JID()`, its
  kind (phone-number user, LID user or group) through `Kind()`, and its
  validity through `Valid()`. It declares no `String`, `GoString`, `Format`,
  `MarshalText`, `MarshalJSON`, `MarshalBinary`, `LogValue` or `Error` method.
- Decisions drop the zero value from every scope, and grants never allow it.

**The guarantee.** Outside `internal/policy/internal/seal`, a valid chat can
only come from `Normalize`, and a grant only from a decision function. It rests
on:

- the compiler, which refuses a literal with fields in any syntactic form, a
  conversion from a look-alike type, a field write and a field's address;
- one constructor-reference rule, `seal-constructors`. A function the seal
  package exports may be referenced only as a direct call, outside any
  function literal, in the body of its one permitted function:
  - `NewChat` in `Normalize`;
  - `NewReadGrant`, `NewWriteGrant` and `NewAdminGrant` in `DecideRead`,
    `DecideWrite` and `DecideAdmin`;
  - `NewAdminCredential` in `ParseAdminCredential`;
  - the constant-time digest comparison `MatchAdminCredential` in
    `DecideAdmin`.

  It refuses renamed, dot and blank imports of the seal package. Another test
  requires its table to name exactly the functions and variables the seal
  package exports. A reviewed list must name exactly its exported methods whose
  results mention a sealed type: only `ReadGrant.Chats`, which returns copies
  of the grant's chats. So a method that could mint a sealed value fails the
  test until it is reviewed;
- the rules that keep code from going around the type system:
  - the ban on `unsafe`, which `go:linkname` needs to call a constructor by
    its symbol name, and the ban on cgo;
  - the reflection rule's ban on the unsafe-pointer functions and methods of
    `reflect` ([row 3](#3-import-fences));
  - the walk's refusal of assembly, C and object files.

Weakening any of these rules weakens the guarantee. Inside the seal package
anything is possible: its code can build any value. So it holds only the
types, their accessors, one constructor per type and the digest comparison,
and review, not a test, keeps it that way.

**Protects.** I-1, I-2.

**Enforced by.**

- Compile-time fixtures added to package policy, with a positive control that
  proves the fixtures are compiled into the package
  (`internal/archtest/testdata`).
- The architecture rules `seal-constructors` and `chat-methods`
  (`internal/archtest/constructors_test.go`, `chats_test.go`). `chat-methods`
  refuses the printing and marshalling methods above on `seal.Chat`, and on
  any alias of it declared in the seal package, in every file of the seal
  package, test files included. The compiler refuses methods declared on
  `CanonicalChat` or any other alias outside it.
- Table, canonical-form, allocation, printing and zero-value tests
  (`internal/policy/normalize_test.go`, `decide_test.go`), and fuzzing
  ([row 8](#8-fuzzing)).

**Since.** v0.2.0.

### 5. One IN-list builder

**Mechanism.**

- All chat-set SQL goes through `internal/store/scoped/inbuilder.go`.
- Each read query carries scope markers that only the builder expands. Three
  queries that return no message content carry none: the look-up of a phone
  number's LID in the mapping, and the highest change number and the highest
  message sequence number of the whole archive.
- An invalid grant or an empty set yields `AND 0`, and an all-chats grant
  `AND 1`. A set yields `AND <column> IN (?n, …)`, with one numbered parameter
  per chat, bound to the grant's sorted, distinct canonical identifiers. So the
  SQL text never holds an identifier, and an invalid chat is left out of a set.
- The column comes from the constant query text. The same parameters serve
  every marker of a query, so the sender-name join is restricted by the same
  set.
- A set of more than 1,000 chats is an error, never a truncation.
- The builder refuses a query without a marker, and the single-row helper
  refuses any text but those three. An unexpanded marker is not valid SQL.

**Protects.** I-1.

**Enforced by.**

- A source test. It requires the SQL text of every `QueryContext`,
  `QueryRowContext` and `scalar` call of the package to be one of its
  constants, except where the builder passes on the query it was given. Every
  such constant, and every constant holding `SELECT`, must carry a marker
  unless it is one of the three.
- The `sql-in-lists` architecture rule. In every file except the builder's, it
  refuses an `IN (` string literal, including one assembled by concatenation
  and one with any white space or SQL comments that SQLite accepts between `IN`
  and `(`. It also refuses an SQL argument whose text holds one once the
  file's constants are resolved.
- Property-tested over random grants and fuzzed over the SQL text
  (`internal/store/scoped/inbuilder_test.go`). A test proves that `AND 0` ends a
  query before its first row.

**Since.** v0.1.0.

### 5a. Uniform resolution

**Mechanism.**

- A chat reference, the chat filter of a search or of the change feed, and a
  message's chat are resolved by one query that carries the scope clause,
  never by a look-up followed by a check.
- A chat outside the grant, a missing chat and a malformed reference all
  return "not found", with the same SQL text and the same empty result.
- A message named by its chat, identifier and sender is canonicalised through
  the identity mappings. It is refused before any message query runs when the
  grant does not allow its chat. The message query carries the scope clause as
  well.

**Protects.** I-1, I-4.

**Enforced by.** Tests that an out-of-scope chat, a missing one and garbage
issue identical SQL and return identical results, and that an out-of-scope
message issues no message query (`internal/store/scoped/reads_test.go`);
property-tested ([row 7](#7-property-tests)).

**Since.** v0.3.0.

### 5b. Bounded reads

**Mechanism.**

- Every read query has a `LIMIT` of at most 201, works on index ranges, counts
  nothing, and finishes within the read deadline.
- Search walks the full-text index in windows of 20,000 sequence numbers, and
  the change feed in windows of 20,000 change numbers. Each call walks at most
  five windows, with a cancellation check between them.
- A change feed started from a time looks in the same windows for the first
  change, dated at or after that time, of a message in scope. It carries the
  time in its position until it finds one, and from there returns every change
  in scope.
- At most 8 reads run at once. A read that finds every slot taken, or that
  passes its 2-second deadline, fails with a "busy" error at once instead of
  queueing behind the engine's writes. The audit row of its answer waits at
  most 2 seconds for the connection before the answer is also "busy".
- Nothing runs `ANALYZE` or `PRAGMA optimize`, so the planner keeps choosing by
  the schema alone. Statistics can turn a cross-chat search of index entries
  into a scan of every message followed by a sort.

**Protects.** Availability of reads and of ingest, which share the archive's
one connection.

**Enforced by.**

- `EXPLAIN QUERY PLAN` golden files for every read query, with an all-chats,
  an empty, a one-chat and a three-chat grant
  (`internal/store/scoped/plans_test.go`). The test refuses a scan of a table,
  and a temporary b-tree other than the chat list's sort of at most 1,000
  granted chats and the change feed's sort of one window. A source test
  requires every `SELECT` constant of the package, and every constant passed as
  the SQL text of a query call, to have a golden plan, and refuses any other
  SQL text at such a call.
- The `no-analyze` architecture rule, which refuses `ANALYZE` and
  `PRAGMA optimize` in every non-test file.
- Tests of the read slots and of reads past their deadline, and HTTP tests
  with the real audit chain that a read with every slot held, and a read behind
  a held write, are answered `503` (`busy`) within seconds
  (`internal/store/scoped/reads_test.go`, `http_property_test.go`).

**Limits.**

- The bound is on time, not on what time reveals. Search applies the scope
  after its walk of the index, and both walks count windows over the whole
  archive. So latency and the number of pages depend on chats outside the
  scope (see [Timing and volume](#timing-and-volume)).
- The plans are those of the SQLite version the driver pins. A driver upgrade
  that changes a plan fails the golden test until it is reviewed.

**Since.** v0.3.0.

### 6. DTO firewall

**Mechanism.**

- Response bodies are types of `internal/api/dto`, built field by field. The
  response interface has an unexported method, so a type declared elsewhere
  cannot be returned. The encoder also checks this at run time.
- Every field of a database row struct carries `json:"-"`. The scoped store's
  rows carry no `raw`, `sender_alt`, `media_meta` or `origin` field.
- No response type can produce the keys `raw`, `media_meta`, `sender_alt`,
  `seq` or `token`. Only the client create answer may declare or reach the key
  `credential`.
- The read API builds each chat and message body field by field from a scoped
  row:
  - `untrusted` is a constant `true` of the builder;
  - `origin` is `gateway:<client id>` for a message a client sent through
    WaWarden, `owner` for any other message the owner's account sent, and
    `peer` otherwise;
  - `sender.name` is the owner-saved name only when the scoped read returned
    it, which it does only when the contact's direct chat is in the grant, and
    otherwise the push name.

**Protects.** I-5.

**Enforced by.**

- A compile-time fixture that returns a foreign response type.
- Reflection tests over every response type, through pointers, slices, arrays
  and embedded structs, for the forbidden keys and the `credential` key
  (`internal/api/dto/dto_test.go`).
- A reflection test over the rows, pages and positions of the scoped store:
  every exported field, nested ones included, carries `json:"-"`, and each type
  encodes to `{}` (`internal/store/scoped/firewall_test.go`). A source test
  keeps it complete by requiring it to list every exported struct of the
  package.
- API tests that every message body carries `untrusted: true`, the `origin` of
  its row, the saved or the push name, and null text when revoked
  (`internal/api/reads_test.go`).

**Since.** v0.1.0.

### 7. Property tests

**Mechanism.** Random archives are built with the real store: phone-number and
LID pairs, messages in chats and groups, edits, revokes, re-keys, and chats
without messages. They are crossed with random clients (all chats, a set,
revoked). Three properties drive them, and compare every answer with a pure
model over an unscoped copy of the archive:

- **The scoped store.** Every read returns exactly the rows in scope, in
  order, across pages of random sizes in both directions, the change feed from
  a random time included. A revoked or edited canary never appears. An
  out-of-scope chat and a missing chat return the same result through the same
  SQL. A re-key never lets a client see a message, stored before it, that the
  client could not see. A sender's saved contact name appears exactly when that
  sender's own chat is in the scope.
- **The REST read API**, through its real handler and pipeline. Listings,
  messages, the change feed and searches equal the model. A chat outside the
  scope answers the same bytes and headers, through the same SQL, as a missing
  chat on every route that takes a chat. A revoked client's listing answers as
  an unknown route, without SQL. Replayed cursors stay inside a narrowed grant.
  No response byte outside the sealed values carries a revoked or edited
  canary.
- **The MCP read tools** at `POST /mcp` of the same handler. `list_chats`,
  `get_messages`, `get_changes` and `search_messages` equal the model. On every
  tool that takes a chat, `get_chat` included, an out-of-scope chat answers the
  same bytes, through the same SQL, as a missing one. The REST property's other
  checks hold here too.

A tally fails the test when the random cases do not cover re-keys, revokes,
edits, partly visible archives, change feeds from a time, and saved names both
shown and withheld.

**Protects.** I-1, I-3, I-4, I-8.

**Enforced by.** `pgregory.net/rapid` in CI: 100 checks with the race detector
in every test run, and 1,000 without it in a separate job, which uploads the
failure files. See `internal/store/scoped/property_test.go`,
`http_property_test.go` and `mcp_property_test.go`.

**Since.** v0.3.0.

### 8. Fuzzing

**Mechanism.** These targets are fuzzed. Each checks that its input never
causes a panic, and the property named:

- the admin token syntax check: anything accepted has the generated form
  (`internal/policy/admin_test.go`);
- `Normalize`: it accepts exactly what a regular-expression model of the forms
  in [row 4](#4-chat-identifier-normaliser) accepts, and returns a canonical
  result (`internal/policy/normalize_test.go`);
- `Normalize` against the protocol library's identifier parser: every chat it
  accepts is read by the protocol library as the same user and server
  (`internal/engine/wa/fuzz_test.go`);
- the display and terminal sanitisers: output is valid UTF-8, holds nothing
  either removes, and is unchanged by a second pass
  (`internal/sanitize/sanitize_test.go`);
- strict JSON decoding of request bodies: refusals are only the three fixed
  errors, and an accepted body meets every rule of the decoder
  (`internal/api/decode_test.go`);
- crafted protocol messages, decoded with a protobuf recursion limit and
  translated for three kinds of sender: a revocation or an edit only from a
  protocol message with that type set (`internal/engine/wa/fuzz_test.go`);
- the history decompressor: it never returns more than its cap, and agrees
  with an uncapped read (`internal/engine/history_test.go`);
- the inbox payload codec: a decoded payload re-encodes stably
  (`internal/engine/pipeline_test.go`);
- the client token parser: everything accepted re-encodes to itself
  (`internal/token/token_test.go`);
- the full-text query builder: no operator or column filter reaches FTS5, and
  for ASCII queries it returns exactly the rows that hold every term
  (`internal/store/scoped/search_fuzz_test.go`);
- the `IN` builder's SQL text ([row 5](#5-one-in-list-builder));
- cursor and message-reference opening ([row 23](#23-sealed-cursors-and-message-references);
  `internal/cursor/fuzz_test.go`);
- the read API's query parsing: refusals are only `invalid_query`
  (`internal/api/reads_test.go`);
- idempotency keys (`internal/store/ingest/key_test.go`), outbound message
  text (`internal/sanitize/outbound_test.go`) and the send route's body, key
  and text checks (`internal/api/sends_test.go`): only the fixed answers, and
  nothing refused reaches the sender.

**Protects.** I-1, I-3, I-5, I-7, I-9.

**Enforced by.** Go native fuzzing. Seed corpora replay in every test run, and
CI fuzzes every target for 60 seconds.

**Since.** v0.1.0.

### 9. Panic-safe goroutines

**Mechanism.**

- Every goroutine WaWarden starts goes through `safego.Go`. It recovers a
  panic, logs the panic's type and stack (never its value), and counts it per
  goroutine name.
- WaWarden's HTTP handlers recover the same way and answer a fixed `500`.
- The protocol adapter's event handler recovers the same way, under the name
  `wa.event`, and refuses the event, so WhatsApp is not acknowledged.

**Protects.** I-5, I-9.

**Enforced by.**

- The `goroutines` architecture rule. In non-test files outside
  `internal/safego`, it refuses a `go` statement, `time.AfterFunc`,
  `context.AfterFunc`, `runtime.AddCleanup`, `runtime.SetFinalizer`, and any
  method named `Go` or `RegisterOnShutdown`.
- A test that makes the engine's handler panic on a message, and checks that
  the handler the adapter registers with the protocol library
  ([row 17](#17-the-protocol-library-behind-one-adapter)) refuses it, logs the
  panic without its value and counts it (`internal/engine/wa/client_test.go`).

**Limits.** Goroutines that other standard-library code starts internally,
such as `http.Server`'s per-connection goroutines, are not covered by the
rule. The handlers that run on them recover as described.

**Since.** v0.1.0.

### 10. Listener inventory

**Mechanism.**

- Only `internal/listeners` opens sockets, and only `internal/app` may use it.
- Only `internal/listeners` builds `http.Server` values, each with an explicit
  handler.
- `net.Listen` and related calls outside it, `http.ListenAndServe`,
  `http.Serve`, the package-level `http.Handle` and `http.HandleFunc`, and
  `http.DefaultServeMux` are banned. So a dependency that registers handlers on
  the default mux exposes nothing.
- Tests listen on loopback only. In test files outside `internal/archtest`,
  the `test-wildcard-addresses` rule refuses a wildcard address: in a string
  literal or concatenation, as `net.IPv4zero`, `net.IPv6zero`,
  `net.IPv6unspecified`, `netip.IPv4Unspecified` or `netip.IPv6Unspecified`, or
  built from zero bytes or from bytes that are not literal constants. A
  per-file allow-list names, with a count and a reason, the few tests that
  need one. An example is a test that binds `0.0.0.0` on an ephemeral port to
  prove that a non-loopback listener logs a warning.

**Protects.** I-6.

**Enforced by.**

- `forbidigo` with type analysis.
- The architecture rules `sockets`, `server-ownership`, `net-http-identifiers`
  and `test-wildcard-addresses` (`internal/archtest/ownership_test.go`,
  `wildcard_test.go`). They also refuse an alias of `*http.Server`, or,
  outside `internal/listeners`, of `listeners.Spec`.
- A test that inspects the test process's open sockets and expects exactly the
  client, admin and health listeners (`internal/app/inventory_test.go`).
- Tests that `/debug/` paths answer `404` on every handler and on every live
  listener, with handlers registered on the default mux to prove the point
  (`internal/app/app_test.go`).

**Limits.** A generic function that makes the zero value of a type parameter
inferred from a `*http.Server` passes the rules. Outside `internal/listeners`,
no listener can be opened for it to serve.

**Since.** v0.1.0.

### 11. Golden files for MCP tools

**Mechanism.** The `tools/list` answer, with every tool's name, description,
annotations and input and output schemas, is built from string constants and
Go types, never from data. It depends only on whether the client may write.
Every client sees the five read tools, `list_chats`, `get_chat`,
`get_messages`, `search_messages` and `get_changes`. A client with a write
grant also sees `send_message`.

**Protects.** I-5.

**Enforced by.** A test fetches the answer through the mounted endpoint for a
client with listed chats, an all-chats client and a write client. It compares
each with a committed golden file with sorted keys
(`internal/api/testdata/mcp`). It refuses a tool name outside `[a-z0-9_]`,
checks that no chat name or reference of the fixture appears, and refuses to
run with `MCPGODEBUG` or `JSONSCHEMAGODEBUG` set (`internal/api/mcp_test.go`).

**Since.** v0.3.0.

### 12. Constant-time credential comparison

**Mechanism.**

- The admin credential type and the client token digest type cannot be
  compared with `==`.
- The admin digest is compared only through `crypto/subtle`, in the seal
  package's `MatchAdminCredential`.
- A client token digest is compared only in `internal/token`'s `Match`, which
  every authentication calls exactly once.

**Protects.** I-7.

**Enforced by.**

- Compile-time fixtures comparing two credentials and two digests.
- A test counting one hash and one comparison per authentication outcome.
- The `secret-comparisons` architecture rule on comparisons in
  `internal/policy`, the seal package included, and `internal/token`. Because
  it recognises calls by name, the `shadowing` rule refuses, in non-test files
  under those two packages, any declaration other than a label that is named
  like a package the file imports or like a predeclared identifier such as
  `len`. Receiver type parameters and import names count as declarations
  (`internal/archtest/secrets_test.go`, `shadowing_test.go`).

**Since.** v0.1.0.

### 13. One log writer

This is the canonical statement of the log writer's rules.

**Mechanism.**

- `serve` wraps standard output in one `logx.Writer` before it logs anything.
  It loads the master key right after the configuration and data directory
  checks, and keys the writer with the `log-redact` key derived from it.
- Every logger is built by `logx.New` on that writer. `app.New` takes it, so
  these all write through it: the app's logger, its alert logger, the notifier
  ([row 19](#19-one-notifier)), the EMF lines, the HTTP servers' error logs,
  and, once `app.New` has installed its logger, the panic reports of `safego`.
- A panic recovered before that, in the signal goroutine `main` starts first,
  is recovered but neither logged nor counted. So it cannot reach Go's default
  logger on standard error.
- The writer handles each line in this order:
  1. It replaces each run with the exact shape of a client token with `ww_`,
     its client id and `_[redacted]`, and of an admin token with
     `wwadm_[redacted]`.
  2. It replaces each WhatsApp identifier in `user@server` form, when no ASCII
     letter or digit follows the server name directly, with a keyed pseudonym:
     `jid:` and 8 hexadecimal digits of an HMAC. It keeps a JSON line valid
     when an identifier follows an escape sequence.
  3. It drops every line longer than 65,536 bytes.
  4. It drops every line carrying XML in one of these shapes, also when
     written with the JSON escapes `\u003c`, `\u003e` and `\"`:
     - a closing tag;
     - a self-closing tag without attributes;
     - the start of a tag with an attribute (`<x a="`), whatever its value
       holds and whether or not the tag ends;
     - a tag directly followed by another (`<x><y`);
     - `<!--`, `<![CDATA[` or `<?xml`.

     An opening tag without attributes followed only by text is kept.
  5. It writes each line whole, under one lock, only once its newline has
     arrived. So a line written in pieces is checked as a whole.
- `logx.New` renders values fail-closed:
  - a byte slice of any type, or a pointer to one, is written as its length,
    even when it has a `String` or `Error` method, and so is a raw JSON value;
  - any other value that is not a string, number, boolean, time, error or
    value with a `String` method is written as its type name;
  - a protocol message or a URL is written as its type name, whatever its
    methods.
- Text that code formats into a message is only redacted, pseudonymised and
  checked for XML.
- **Protocol library.** Its logger feeds the writer through an adapter. The
  adapter first replaces every `http`, `https`, `ws` or `wss` URL with `[url]`,
  every media direct path with `[path]`, and the value of any `hash`,
  `mms-type`, `oh`, `oe`, `__wa-mms` or `_nc_`-prefixed query parameter left
  over with `[param]`. A direct path is a path of the media servers'
  `/v/t<digit>`, `/o1/v/t<digit>` or `/m1/v/t<digit>` shape, a slash-led word
  that carries a query, or the rest of a line after the protocol library's
  error for a path that does not start with a slash. The adapter then redacts
  tokens, masks runs of six or more digits outside the `user@server` form, cuts
  each line at a word boundary after 1 KiB, and discards debug output outside a
  `WAWARDEN_UNSAFE_DEBUG` window.
- **Signal library.** The Signal library that the protocol library links for
  its encryption prints its log lines to standard output unless it is given a
  logger. The adapter gives it one when it is built. That logger discards its
  debug and information lines, since they carry key material, and writes its
  warnings and errors through the protocol-library adapter
  ([row 17](#17-the-protocol-library-behind-one-adapter)).
- **MCP library.** It is given no logger, so its log output is discarded
  ([row 24](#24-the-mcp-endpoint-behind-the-client-pipeline)). Tool arguments
  are never logged.

**Protects.** I-5.

**Enforced by.**

- The compiler: `app.New` and `logx.New` take a `*logx.Writer`.
- The `log-handlers` architecture rule, which refuses `slog.NewJSONHandler`
  and `slog.NewTextHandler` in non-test files outside `internal/logx`.
- The `log-output` architecture rule. In non-test files it refuses Go's
  default loggers (`slog.Default`, `slog.SetDefault`, the package-level logging
  functions of `log/slog` and `log`, `log.Default`, `log.SetOutput`,
  `log.Writer` and `log.Output`), `fmt.Print`, `fmt.Printf` and `fmt.Println`,
  the built-ins `print` and `println`, `syscall.Stdout`, `syscall.Stderr` and
  `syscall.Write`. It refuses `os.Stdout`, `os.Stderr` and `os.NewFile`
  outside `cmd/wawarden/main.go`, and a string literal naming `/dev/stdout`,
  `/dev/stderr`, an entry of `/dev/fd/` or of a process's `/proc/.../fd/`
  (`internal/archtest/logging_test.go`).
- Canary tests: a canary identifier or token passed as every kind of value,
  through every logger path and split across writes, never reaches the output;
  a media URL, direct path or encrypted-file hash in the protocol library's
  download warnings never reaches the output, and lines without one are left
  unchanged; every XML shape drops its line; lines stay valid JSON; and the
  Signal library's lines reach standard output only through the writer
  (`internal/logx/logx_test.go`, `redact_test.go`,
  `internal/engine/wa/log_test.go`).

**Limits.**

- A custom `slog.Handler` written elsewhere passes `log-handlers`.
- A raw system call, or a descriptor path assembled at run time, passes
  `log-output`.
- `log-output` reads only this module's code. Code in a linked module that
  writes to standard output or standard error itself passes it. Examples are
  the Signal library's default logger, which a test covers, and the printing
  functions of the SQLite runtime's C library, such as its assertion-failure
  handler.
- The writer recognises identifiers, tokens and XML, and the protocol-library
  adapter media locations, by their shape; see [Logging](#logging).

**Since.** v0.2.0.

### 14. The keys directory

**Mechanism.**

- Only `internal/keys` creates, checks and reads `keys/`, the master key and
  the retired keys in `keys/retired/` (see [Startup refusals](#startup-refusals)).
  It derives every purpose key with HKDF-SHA256.
- Only its `Rotate` replaces the master key. The `keys rotate` command calls it
  with a probe that refuses while another process holds the archive's lock.
  `Rotate` links the old key into `keys/retired/` and renames a new one over
  `keys/master`, flushing each file and directory.
- The `keys` subcommand's name comes from `internal/keys` too.
- A master key value, with its retired keys, prints no key material through
  `fmt`, `encoding/json` or `log/slog`.

**Protects.** I-5, I-7.

**Enforced by.**

- The `keys-directory` architecture rule. In non-test files outside
  `internal/keys`, it refuses a string literal outside import declarations, a
  run of concatenated literals, or a run assembled from the file's constants,
  with a path element named `keys` (`internal/archtest/keysdir_test.go`).
- Tests of every refusal, of racing creators, of derivation vectors checked
  against an HKDF written out in the test, of printing, and of rotation and its
  refusals (`internal/keys/keys_test.go`, `retired_test.go`,
  `cmd/wawarden/keys_test.go`).

**Limits.** A name assembled at run time, such as one built by
`strings.ToLower` or a conversion, passes the rule. So does a wildcard or a
directory walk that reaches `keys/` without naming it, such as `filepath.Glob`
on `*/master` under the data directory.

**Since.** v0.2.0.

### 15. Verified storage and deletion on disk

**Mechanism.**

- **Settings.** The archive is opened with foreign keys on, a `TRUNCATE`
  rollback journal, `synchronous` `FULL`, exclusive locking, temporary storage
  in memory, `secure_delete` `ON`, and a 5-second busy timeout. A per-driver
  connection hook reads every one of them back and refuses the connection on
  any difference. So `PERSIST`, write-ahead logging and `secure_delete` `FAST`
  cannot take effect. The hook then takes the exclusive lock.
- **One connection.** The single connection stays valid after an interrupted
  call, and a second connection is refused. Otherwise the driver would discard
  the first and silently drop the lock.
- **Lock kinds.** Storage profile `local` selects open-file-description locks
  on Linux, and refuses to start when the kernel or the filesystem rejects
  them. Profile `nfs` selects classic POSIX locks. A process uses one kind.
- The DSN is built with `net/url`.
- **Deletion.** The full-text index is created with its `secure-delete`
  option. A revoke, an edit or an expiry, in one transaction, issues the
  index's `delete` command with the exact old text, nulls `text` and
  `text_display`, and writes a new or empty index row. A revoke or an expiry
  also deletes the message's media reference and nulls its media type in that
  transaction, and an expiry does so for media without a caption as well. A
  reference is stored only on a message with a media type.
- **Page keys.** That transaction also tokenises the old text in a contentless
  full-text table of the in-memory temporary database. Secure delete removes a
  term but keeps its page key. When a trigram of the old text that no
  remaining message holds is still a page key of the index, the transaction
  records that an index rewrite is due.
- **Rewrite.** After the commit, the index is rewritten whole, under a
  5-minute deadline of its own, and the record is cleared in the same
  transaction. The rewrite is `optimize`, with a filler row added before and
  deleted after, so that an index of a single segment is rewritten too. A
  start that finds the record, left by a stop or a failed rewrite, rewrites
  before it opens the archive.
- Quoted text is never stored, and reactions and poll updates store no
  content.

**Protects.** I-3, I-8.

**Enforced by.**

- Tests of each setting refused when it reads back differently, of a lock
  kept through interrupted calls (with a negative control), of each profile's
  lock kind, of file modes under `umask 0`, and of every file-mode, owner,
  filesystem and lock-wait refusal (`internal/store/internal/db/db_test.go`,
  `ofd_linux_test.go`).
- A canary deletion test with negative controls. It byte-scans `archive.db`
  and its journal, open and closed, and checks the index's vocabulary, page
  keys and `MATCH` after a revocation, an expiry and an edit, including a
  rewrite cut short or run out of time. It also byte-scans for a canary media
  type and media reference after a revocation and after an expiry, the expiry
  of media without a caption included (`internal/store/ingest/deletion_test.go`).

**Since.** v0.2.0.

### 16. A neutral engine core

**Mechanism.**

- `internal/engine` and its subpackages, other than the protocol adapter
  `internal/engine/wa`, handle plain data. They may import neither the
  protocol library nor protobuf.
- Like all code outside `internal/store`, they may import neither
  `database/sql` nor the SQLite driver. The core reaches the archive only
  through `internal/store/ingest`.
- Every goroutine it starts goes through `safego.Go`
  ([row 9](#9-panic-safe-goroutines)).

**Protects.** I-3, I-5, I-9.

**Enforced by.** The `import-fences` architecture rule on the non-test files of
those packages (`internal/archtest/imports_test.go`), and `depguard`.

**Since.** v0.2.0.

### 17. The protocol library behind one adapter

**Mechanism.**

- **Imports.** Outside test files, only `internal/engine/wa` imports
  `go.mau.fi/whatsmeow` and `google.golang.org/protobuf`, and of the Signal
  library `go.mau.fi/libsignal` only its logger package. `internal/store/session`
  imports the protocol library's two device store packages and nothing else
  of it. The device store's raw handle, which the protocol library needs, can
  be reached only there.
- **Refused calls.** These are refused in every file, test files included:
  - presence, chat presence and presence subscriptions;
  - read receipts and visible delivery receipts;
  - the history download that writes any own device's payloads into the
    device store;
  - every newsletter call and the status-message setter, inside which the
    protocol library can end the process;
  - its raw GraphQL query;
  - the QR channel, which logs the codes that carry the device's pairing
    secret;
  - the decrypted event buffer, which keeps plaintext in the device store;
  - the protocol library's internals;
  - an event handler whose answer the protocol library ignores;
  - the removal of one or all event handlers, after which the protocol library
    would acknowledge every event without delivering it;
  - the loggers that bypass the log writer: its standard-output logger, its
    zerolog adapter and a default context logger;
  - its proxy setters.

  The test refuses these names wherever they appear: as a selector, a
  declared name, a composite-literal key or a struct field, and in a string
  literal or a concatenation of string literals, in any letter case. A string
  literal is checked as written and again with its JSON `\u` escapes decoded.
  The letter-case check follows Unicode case folding, as JSON's key matching
  does.
- **Client settings.** The settings the adapter gives the client are its
  reconnection, login re-dial, history download and receipt, message
  re-request, retry store, reporting token, acknowledgement, token refresh and
  pre-pairing settings. Their names may appear only as a field selected from a
  value. Only the adapter's `installLocked`, which builds every client the
  adapter holds, may assign one, range into one or take its address, outside
  any function literal. Every other function, in every file, may only read
  them.
- **Construction.** Outside test files, only `installLocked` may name the
  protocol library's client constructor and its handler registration, each
  outside any function literal. Only it may call the pre-login, websocket and
  media HTTP client setters, as calls outside any function literal. So no later
  call, method value or method expression replaces a transport.
- **One handler.** The one handler `installLocked` may register is
  `handlerFor(<receiver>.gen)` of its own receiver: the adapter's handler for
  the current client, which returns the engine's answer. So the protocol
  library acknowledges an event and sends its delivery receipt only when the
  engine accepted it ([row 9](#9-panic-safe-goroutines)).
- **Pointers only.** A protocol client that is not a pointer is refused in
  every file, so no literal, `new` or zero value builds one. Only the
  constructor does, which only `installLocked` calls and test files may not
  name ([row 18](#18-no-test-reaches-the-network)).
- **No decoders.** Outside test files, the adapter and the device store may not
  import a document decoder or a template package: `encoding/json`,
  `encoding/gob`, `encoding/xml`, `net/rpc`, `text/template` and
  `html/template`, each with its subpackages. These reach a value's methods and
  fields by names held in a document or a template. So outside test files,
  reflection by name, a decoded document, an executed template and a literal
  cannot reach any refused name, and nothing changes the settings after
  `installLocked`.
- **Behaviour.** The adapter turns the protocol library's own reconnection off,
  the re-dial it makes when WhatsApp asks a device to log in again included. It
  reports that request and every failed pairing to the engine as a drop, so
  that no attempt is left waiting. It stubs the connection-token refresh,
  whose absence panics in a goroutine nothing recovers. It gives the protocol
  library HTTP clients with timeouts, size caps, no proxy from the environment
  and no redirect to another host.
- **Sends and history requests.** The adapter sends text messages and asks the
  owner's primary device for history through `SendMessage`, `SendPeerMessage`
  and `BuildHistorySyncRequest`, with `GenerateMessageID`, `IsConnected` and
  `IsLoggedIn` around them. It builds their protobuf messages itself, since no
  other package may import protobuf. It addresses only a canonical chat, built
  without the protocol library's JID parser. It hands the protocol library the
  caller's context without its cancellation and under its own 20-second
  deadline, so a caller that goes away cannot cut a send short.
- **Reviewed method list.** Outside test files the adapter calls no other
  method of the protocol library's client. The method set is read from the
  pinned protocol library's source. In the adapter's non-test files every method is
  refused except `ConnectContext`, `Disconnect`, `Logout`, `PairPhone`,
  `DownloadToFile`, `ParseWebMessage`, `SendProtocolMessageReceipt`, the
  handler registration, the three HTTP client setters, and the six send and
  history calls above.
  - The names are matched wherever they appear. So a method value, a method
    expression, an interface method or a method promoted through an embedded
    client is refused like a call.
  - The adapter's own methods that share a name with the protocol library's
    (`Connect`, `Disconnect`, `Logout`, `PairPhone`) may be declared.
  - Only `transmitMessage` calls `SendMessage`, and only
    `transmitPeerMessage` calls `SendPeerMessage`, each directly and outside
    any function literal.
  - Only `downloadToFile`, the history download seam that `New` passes to the
    client, calls `DownloadToFile`, directly and outside any function literal.
  - A protocol library update that adds a method finds it refused.

**Protects.** I-5, I-6, account standing.

**Enforced by.**

- The architecture rules `confined-imports`, `raw-database-handle`,
  `protocol-calls` and `protocol-client-calls`
  (`internal/archtest/protocol_test.go`, `protocolclient_test.go`,
  `storage_test.go`), and `depguard`.
- Tests of the adapter's construction, on every client it builds: the
  settings, that the protocol library's HTTP clients are the adapter's, and
  that it holds exactly one event handler (`internal/engine/wa/client_test.go`).
- Tests that every event delivered through `handlerFor` gets the engine's
  answer, of the events reported for a login request and each kind of failed
  pairing, and of sends and history requests through an injected function
  (`internal/engine/wa/engine_test.go`, `send_test.go`).

**Limits.**

- A name built at run time other than by joining string literals passes the
  test. Examples are a method or field that `reflect` reaches by its index or
  by a formatted name, or a document whose names a test assembles at run time
  and decodes into a client. Outside test files, `reflect` is confined by
  [row 3](#3-import-fences), and the decoders and templates by this row.
- The tests read the protocol library's HTTP clients and its number of
  handlers by reflection. They cannot call the handler it holds, since row 3
  refuses the reflection that reaches unexported memory. That this handler is
  `handlerFor` of the current client rests on the `protocol-calls` test alone.
- The `protocol-client-calls` test reads the adapter's files only. A client
  that the adapter handed out as an interface value could be called elsewhere,
  through an interface whose methods take no type of the protocol library,
  which no test refuses.

**Since.** v0.2.0.

### 18. No test reaches the network

**Mechanism.**

- The adapter's dialer, and every transport the adapter builds whatever dial
  function it is handed, dial through a guard. The guard refuses all
  connections when the binary is a test binary.
- A version source that its constructor did not build fetches nothing, instead
  of falling back to the protocol library's default HTTP client.
- So a test that builds the production path, or a client or version source
  over the adapter's transports, cannot reach WhatsApp through it.
- Test files may not name:
  - a WhatsApp or Meta host;
  - a URL, or a host name with a port, outside loopback and reserved example
    names;
  - an IPv4 or bracketed IPv6 address with a port, IPv4-mapped and zoned forms
    included, other than a loopback or unspecified one. The exception is the
    addresses a per-file allow-list names, each one an address that the code
    under test validates or refuses before any connection;
  - the protocol library's client constructor, its own WebSocket
    (`socket.NewFrameSocket` and `socket.FrameSocket`), its setters for the
    client's transports, or its version fetch;
  - the network calls of its client that no method of the adapter shares a
    name with: connecting with a context, downloads, sticker-pack fetches,
    uploads, application-state fetches, media retry receipts and sends, to a
    contact or to the owner's own devices.
- [Row 17](#17-the-protocol-library-behind-one-adapter) also refuses, in every
  file, a client built by a literal, `new` or a zero value, and the proxy
  setters. Outside test files it refuses the client constructor anywhere but
  the adapter's `installLocked`. So every protocol client a test can hold was
  built by the adapter, and its connect, pairing and logout calls dial through
  the guard as well.
- `hack/offline-test.sh` runs the whole suite, release and dev builds, in a
  container without a network, from a module cache filled beforehand. The
  whatsmeow bump workflow runs it before it opens or updates a pull request,
  and the maintainer runs it by hand before a release.

**Protects.** Account standing.

**Enforced by.**

- The `offline-tests` and `protocol-calls` architecture rules
  (`internal/archtest/protocol_test.go`).
- Tests that the dialer, a transport built over another dial function, the
  protocol client's HTTP clients, and a version source over such a transport
  refuse to dial from a test binary, and that a version source without its
  client asks for no connection (`internal/engine/wa/client_test.go`,
  `version_test.go`).
- A test that checks by reflection that the protocol client holds the
  adapter's HTTP clients before it connects, so that a missing setter fails
  the test before any dial.
- The offline run.

**Limits.** A test could pass the static check by opening a socket to an
address it computed at run time. That includes an IP address written without a
port and joined to one by `net.JoinHostPort`, or set in a `net.TCPAddr`. A test
could also hand the version source's test seam a transport or HTTP client of
its own making, the standard library's included. The guard covers only the
dialer and the transports the adapter builds.

**Since.** v0.2.0.

### 19. One notifier

**Mechanism.**

- Every operational event is written by `internal/notify`: `unpaired`,
  `disconnected`, `pair_rejected`, `logout_failed`, `quarantine`,
  `rekey_conflict`, `ingest_paused`, `backup_done`, `backup_failed`,
  `backup_pruned`, `backup_prune_failed`, `admin_mutation`,
  `admin_auth_failure`, `audit_key_changed`, `send_outcome_unknown` and
  `send_unrecorded`.
- There is one method per event, whose parameters are that event's fields.
  Each event is written as a line of the log writer
  ([row 13](#13-one-log-writer)) and, when configured, as a signed post to the
  webhook ([trust boundary 9](#trust-boundaries)).
- Each string field is checked against the shape of a lower-case code, an
  error type against that of a Go type name, and a key id against 8
  lower-case hexadecimal digits. A field that fails is written as `invalid`.
- The engine, the sender and the admin routes reach the notifier through small
  interfaces. No send event carries a key, a chat or a text.
- The pairing code reaches no event, log line, metric or response other than
  the pairing route's.

**Protects.** I-5.

**Enforced by.**

- The `notify-events` architecture rule. In non-test files outside
  `internal/notify`, it refuses an `"event"` key followed by the name of one
  of these events in any call, a concatenation of literals included. It checks
  its list against the constants of `internal/notify`
  (`internal/archtest/notify_test.go`).
- Tests of every event's fields, of non-codes written as `invalid` on standard
  output and in posted bodies, and of the webhook's signature, retries,
  destination check, transport settings, answer cap, queue and shutdown
  (`internal/notify/notify_test.go`, `webhook_test.go`).
- Canary tests that a pairing code reaches neither standard output, metrics,
  EMF lines, events nor response headers, from the API handlers and through a
  running service.

**Limits.** A string field filled with text that has the shape of a code, such
as a single lower-case word, passes the check.

**Since.** v0.2.0.

### 20. Recipient-only backups

**Mechanism.**

- **Encryption only.** Only `internal/backup` imports `filippo.io/age`. Outside
  test files it may use only `Encrypt`, `ParseRecipients` and the `Recipient`
  type of age's root package: no identity type or constructor, no `Decrypt`,
  and no other age package. So the service can encrypt a backup but holds
  nothing that decrypts one.
- `WAWARDEN_BACKUP_AGE_RECIPIENT` is parsed by age's own parser behind a fixed
  error. A refusal never repeats it, a pasted secret key included.
- **Staging.** Only `internal/backup` names the `backups/` directory. It stages
  each database copy with mode `0600` in a `0700` `backups/tmp/`, and streams
  it through age without a second plaintext copy. It flushes the encrypted file
  and renames it into place. On any failure it removes the staging directory,
  the partial file, and a file renamed but not flushed. `serve` removes
  `backups/tmp/` at every start, once it holds the archive's lock.
- **Manifest.** The manifest in a backup holds a build version, schema
  versions, sizes and a time, never an identifier.
- **Retention.** After a successful backup, and only then, the scheduler prunes
  `backups/`. It deletes nothing when the directory holds any entry other
  than:
  - a `0700` `backups/tmp/` owned by the service's user;
  - regular files of mode `0600`, owned by the service's user, whose name is a
    backup's name with a valid time.

  Otherwise it keeps the newest `WAWARDEN_BACKUP_KEEP` backups by that time,
  and the file just written, and deletes the rest.
- **Verifier.** `wawarden backup verify` reads what `age` decrypted, from
  standard input. Three parts do the work: the command's file, the stream
  checker `internal/backup/verify.go`, and `internal/store/inspect`, which
  opens the extracted copies read-only and returns counts, never an
  identifier. None of them imports age, the protocol library, the engine, the
  device store or the app. The rest of `internal/backup`, which shares the
  stream checker's package, imports none of them but age. So the binary still
  holds nothing that decrypts, and the verifier cannot construct a WhatsApp
  client.

**Protects.** I-5, I-7.

**Enforced by.**

- The `confined-imports` architecture rule and a `depguard` rule confine age to
  `internal/backup`. The `age-recipient-only` rule refuses every other use of
  age in non-test code (`internal/archtest/age_test.go`).
- The `backups-directory` rule. Outside `internal/backup` and tests, it
  refuses a literal path element `backups`, in a string literal, a run of
  concatenated literals or a run assembled from the file's constants. It also
  refuses any use of `backup.Dir` except the start-time check in
  `internal/config` (`internal/archtest/backups_test.go`).
- The `backup-verifier-fence` rule. It lets only `cmd/wawarden` import
  `internal/store/inspect`, and refuses the imports above in the verifier's
  files and in the rest of `internal/backup`. A test lists the transitive
  dependencies of `internal/store/inspect` and of `internal/backup` and refuses
  any of them (`internal/archtest/verifier_test.go`).
- Round-trip, failure-path, tamper, mode, recipient, prune and verifier tests
  (`internal/backup/backup_test.go`, `prune_test.go`, `verify_test.go`,
  `internal/store/inspect/inspect_test.go`).

**Limits.** A name assembled at run time passes `backups-directory`. So does a
wildcard or a directory walk that reaches `backups/` without naming it.

**Since.** v0.2.0.

### 21. A fake engine for development builds only

**Mechanism.**

- `internal/engine/fake` is a scripted stand-in for the protocol adapter. It
  pairs, connects, delivers synthetic events and history, and answers sends and
  history requests, offline. A development build runs it in place of WhatsApp
  when `WAWARDEN_DEV_FAKE_ENGINE` selects it (see
  [the fake engine](internals.md#fake-engine)).
- Every file of the package, and every file that imports it, compiles only
  with the `dev` tag. So a release build has no path to it and always runs the
  WhatsApp engine.
- Outside its tests, the fake imports only buffers, zlib, context, JSON,
  errors, I/O, slices, sync and time from the standard library, and the engine
  core, `internal/policy` and `internal/safego` from the module. Nothing it
  imports opens a socket or starts a program.
- It starts goroutines only through `safego.Go`
  ([row 9](#9-panic-safe-goroutines)), and never logs its pairing code.

**Protects.** Integrity of the archive; account standing.

**Enforced by.**

- The `dev-only-fake-engine` architecture rule. It refuses a file under
  `internal/engine/fake`, test files included, or a file that imports it,
  unless it has exactly one `//go:build` line, where the go tool reads it, that
  is false for every assignment of its other tags while `dev` is unset.
- The `fake-engine-imports` rule, which refuses any other import in the fake's
  non-test files. The walk must find the fake and its selector
  (`internal/archtest/fakeengine_test.go`).
- `hack/repro-build.sh`, which fails when a release binary contains the fake's
  marker string, which dev binaries keep, or any symbol of the package.
- A release-only test that a release build refuses `WAWARDEN_DEV_FAKE_ENGINE`
  (`internal/config/release_test.go`).
- The fake's own tests (`internal/engine/fake/fake_test.go`), and end-to-end
  tests that run `serve` with the fake, pair through the admin CLI, check that
  no pairing code, message text, chat identifier or group name reaches the
  output, and see a wrong account rejected (`cmd/wawarden/fake_test.go`).

**Limits.** Nothing stops a development build that runs the fake on a release
build's data directory from storing its synthetic messages in that archive.

**Since.** v0.2.0.

### 22. Read, search and send budgets

**Mechanism.**

- **Read budget.** Every request on the client listener that authenticates
  spends one from its client's read budget before routing: 600 a minute, burst
  60, by default. So unknown routes and `404`s cost the same as reads. Every
  `POST /mcp` spends one read the same way.
- **Search budget.** A search also spends one from the search budget: 60 a
  minute, burst 6. A `search_messages` call spends one from the same bucket as
  `/v1/search`, before its arguments are checked
  ([row 25](#25-mcp-tool-calls-fail-closed)).
- A spent budget answers `429` (`rate_limited`) with `Retry-After` at once.
  Nothing queues.
- **Buckets.** The budgets are token buckets per client id, on an injected
  clock, in `internal/ratelimit`, which may import only the standard library.
  A bucket left full is dropped. Only authenticated clients get one, so a
  caller without a token cannot grow the map.
- **Archive contention.** Reads share the archive connection with ingest,
  behind 8 read slots. These answer `503` (`busy`) with `Retry-After: 1`:
  - a read that finds the slots taken or passes its 2-second deadline;
  - a request whose audit row cannot be written within 2 seconds;
  - a request that waits more than 2 seconds for the client list to be
    republished after a change.

  Authentication never reads the archive, so no answer waits until a long
  ingest write ends.
- **Send budgets.** The send budgets are not token buckets. They are counted
  over the rows of the archive's idempotency table, in sliding windows on the
  same injected clock, so a restart does not refill them. Each client may send
  10 a minute, and its share of the global 30 an hour. The share is the global
  figure divided by the number of clients that may write, and at least 1.
- **Pacer.** A global pacer admits the next send of any client 3 seconds, plus
  a random wait under 2 seconds, after the last one was admitted. A send that
  waits for its acknowledgement longer than that gap can therefore be followed
  at once.
- **Caps.** The send limits range from 1 to 10 a minute and 1 to 60 an hour.
  Only `WAWARDEN_UNSAFE_SEND_CAPS=1` lifts them, to 60 and 600, and
  `WAWARDEN_UNSAFE_RATE_CAPS` never does.
- Every send refusal is the one `429` `rate_limited`, with the earliest admit
  time as `Retry-After`. Only a send that wrote nothing to WhatsApp gives its
  place back. The engine runs one send at a time and refuses another at once.

**Protects.** I-6, and the availability of reads and ingest; for sends, I-2
and account standing.

**Enforced by.**

- Injected-clock tests, and a property that admissions over any window never
  exceed the burst plus the rate (`internal/ratelimit/ratelimit_test.go`).
- Configuration tests for the caps and the `UNSAFE_` overrides
  (`internal/config/config_test.go`).
- Tests of the send budgets at every window boundary, across a close and a
  reopen, and across clock steps both ways
  (`internal/store/ingest/budget_test.go`).
- A property test of the sender over random clients, chats, keys, texts,
  clock steps, engine outcomes and restarts, with the real archive. It finds
  at most one engine send per key within its day, every send inside the
  windows and the pacer, and no send outside the write set
  (`internal/sender/property_test.go`).
- Tests that a valid client behind a held write answers `503`, never `401` or
  `429` (`internal/store/scoped/auth_http_test.go`).

**Since.** v0.3.0.

### 23. Sealed cursors and message references

**Mechanism.**

- **Cursors.** A `next` cursor holds only a position (a time, an arrival
  number, a change number), sealed with AES-256-GCM and a random nonce under
  the `cursor-seal` key. Its additional data binds the client id, the route,
  the chat filter, and, for search, a hash of the normalised query.
- **Message references.** A message reference holds the chat, message id and
  sender, sealed under the `mref` key and bound to the client. Its nonce is the
  first 12 bytes of an HMAC-SHA256 of the client id and the sealed values,
  under a nonce key derived from the `mref` key with a fixed label. The nonce
  key is an HMAC-SHA256 under the `mref` key of `wawarden/mref-nonce/v1`. So
  one message seals to one stable reference for a client, and to different
  references for different clients or messages.
- **Opening.** Opening caps the input (256 and 640 characters). It decodes
  strict unpadded base64url and refuses any text that does not re-encode to
  itself, since the decoder skips line breaks. It checks the version, and that
  the key id is the current key's or a retired key's, and opens under that key.
  It fails with one error. Sealing uses only the current key.
- **Grants.** The grant is taken from the client at every request, never from
  a cursor. So a replay after the scope narrowed returns nothing outside it. A
  request that names a chat checks the chat before the cursor, so an invalid
  cursor with a chat outside the scope answers `404`. A reference that does not
  open, or opens to a chat outside the scope, answers the uniform `404`.
- **Sends.** The idempotency store fingerprints a send from the chat's
  reference, the text, and the reply target's message id and archive row. It
  never uses a sealed reference or the target's sender, which a re-key
  rewrites. It stores neither a reference, the text nor the chat. A send's
  `reply_to` opens only under the sending client's key. It must name a message
  of the same chat, in the client's read scope, that is not revoked and has
  text. Any failure is the uniform `404`.

**Protects.** I-1, I-4.

**Enforced by.**

- Unit tests of tampering, binding, nonce derivation, recorded reference
  texts, retired keys and the sealers' roles, and fuzz targets for cursor and
  reference opening (`internal/cursor/cursor_test.go`, `fuzz_test.go`).
- API tests that every `404` case is byte-identical to an unknown route's and
  takes the same archive calls as a missing chat, and that misused cursors
  answer `400` (`invalid_cursor`) (`internal/api/reads_test.go`).
- The HTTP property of [row 7](#7-property-tests).
- A pinned fingerprint that survives a re-key of the reply target's sender
  (`internal/store/ingest/key_test.go`), and an end-to-end retry after that
  re-key that sends once (`internal/app/rekeyreplay_test.go`).

**Since.** v0.3.0.

### 24. The MCP endpoint behind the client pipeline

**Mechanism.**

- `POST /mcp` is one stateless, JSON-only handler of the MCP library, the MCP
  Go SDK, registered on the client router. Authentication, the read budget,
  the browser refusal and the audit row apply to every POST.
- **Body checks.** Before the MCP library reads a body, the endpoint refuses
  these with the fixed JSON errors, which the MCP library would otherwise
  accept:
  - a wrong media type;
  - a body over 64 KiB, announced or read;
  - a body that is not exactly one JSON object without duplicate keys,
    trailing data or nesting past 16 levels.
- **Methods.** The server advertises tools only. A receiving middleware
  answers method not found to every method but the handshake, `ping`,
  `server/discover`, `tools/list` and `tools/call`. It answers a fixed
  `unknown tool` error, which never repeats the name, to any tool not
  registered.
- **Authentication.** The MCP library's bearer check takes the client the
  pipeline authenticated, and never looks a token up again.
- **Host check.** The MCP library's DNS-rebinding `Host` check is off on
  purpose. It refuses every request that reaches a loopback listener through a
  proxy forwarding the original host, which is the deployment this service
  expects. The pipeline's `Origin` and `Sec-Fetch-Site` refusal and the bearer
  token cover the same browser threat.
- A writer restores `Cache-Control: no-store`, which the MCP library replaces
  on success.
- **Confinement.** The MCP library may not be imported outside `internal/api`,
  and may not build or wrap a handler outside package `api`. The service starts
  only with `MCPGODEBUG` and `JSONSCHEMAGODEBUG` unset.
- **No logger.** The MCP library is given no logger in its server or handler
  options, so its log output is discarded.
- **Two servers.** The endpoint holds two servers built at start. One has the
  five read tools, `list_chats`, `get_chat`, `get_messages`, `search_messages`
  and `get_changes`. The other adds `send_message`. The endpoint picks the
  second for a request only when `DecideWrite` grants its client. For every
  other client, a `send_message` call is the same `unknown tool` error, byte
  for byte, with the same audit reason as any unknown name.
- **One key, one send.** The idempotency store keys a send by client id and
  idempotency key alone. So one client's key names one send, whichever
  protocol carries it. Of concurrent reservations of one key, exactly one
  proceeds.

**Protects.** I-0, I-1, I-4, I-6.

**Enforced by.**

- Tests through the mounted handler with the MCP library's own client, over
  both the 2026-07-28 and the `initialize` protocol paths
  (`internal/api/mcp_test.go`). They cover the uniform refusals, the body
  checks, refused methods, `no-store`, a non-loopback `Host`, one audit row per
  POST, and concurrent reservations. They also compare a hidden
  `send_message` byte for byte with an unknown tool.
- The `confined-imports`, `handler-ownership` and `mcp-registration`
  architecture rules, and `depguard`.
- The `mcp-logger` architecture rule, which refuses a logger in the MCP
  library's server and handler options (`internal/archtest/logging_test.go`).
  A test captures the default logger and standard error, drives requests the
  MCP library logs when it has a logger, and finds nothing written.
- The startup refusal `library_debug_set`.

**Since.** v0.3.0.

### 25. MCP tool calls fail closed

**Mechanism.**

- Each read tool is registered through `readTool`, which:
  - charges the search budget for `search_messages`;
  - checks the arguments against a schema with explicit bounds (`limit` 1 to
    200, string lengths);
  - mints the read grant;
  - gives the call a 10-second deadline;
  - recovers a panic through `safego`, since the MCP library runs tools on its
    own goroutine, where an unrecovered panic would stop the process;
  - answers with one fixed code in `content[0].text`.
- The tools call the same code as the REST routes, so scope, cursor binding
  and denied-equals-missing are the routes'.
- Argument values, schema-validation text, store errors and panic values never
  reach a result. The MCP library's own argument checking, which repeats the
  caller's values, is not used.
- An idempotency key is a sealed type whose printed and logged forms are a
  fixed redaction. So formatting cannot carry it into a result or a log line.
- `send_message` goes through `writeTool`. It checks the arguments against
  bounds of its own: text of 1 to 4096 characters, a key of 8 to 128
  printable ASCII characters, and `reply_to` up to 640. It mints the write
  grant, gives the call 25 seconds, recovers a panic the same way, and answers
  the route's fixed codes. For `409`, `429` and `503` it adds
  `retry_after_seconds` in `structuredContent`.
- Every tool but `send_message` carries `readOnlyHint`.

**Protects.** I-1, I-2, I-4, I-5.

**Enforced by.**

- Tests over both protocol paths for every code, with a canary argument, a
  canary key and text, and a canary store error absent from the wire and the
  audit rows (`internal/api/mcp_test.go`, `sends_test.go`).
- Tests that denied and missing answers are byte-identical with the same
  archive calls, that a panicking tool answers `internal_error` and the
  endpoint answers the next call, and that a stalled read answers `busy` at the
  deadline.
- A test that takes every tool from `tools/list`, for a read-only and a write
  client, and checks its annotations, output schema and failure answers.
- The MCP property of [row 7](#7-property-tests), and the `mcp-registration`
  rule of [row 2](#2-registration-helpers).

**Since.** v0.3.0.

### 26. The send path

**Mechanism.**

- `internal/sender` sends one text message for a client that holds a write
  grant, at most once per idempotency key. It reaches the engine and the HTTP
  handlers only through interfaces that `internal/app` wires. It may not
  import `database/sql`, the SQLite driver, the protocol library, protobuf, or
  any package under `internal/engine` or `internal/api`.
- `internal/api` may not import `internal/sender`
  ([row 3](#3-import-fences)). Neither directly nor through another package
  may `internal/api` depend on the sender, the engine, the protocol library, or
  a store package other than the scoped store and its database package.
- Every exported method of the `Store`, `Tx` and `Reader` types in
  `internal/store/ingest/sends.go` takes a `policy.WriteGrant` value as its
  first parameter. So the send ledger cannot be reserved, settled or released,
  and a sent message cannot be recorded, without a grant from `DecideWrite`.
  The two exceptions write nothing for a client: `Store.RecoverSends` marks
  sends left pending as failed, and `Tx.SweepSends` deletes expired ledger
  rows.
- The column `messages.sent_by` holds the id of the client that sent a message
  through the gateway, which readers see as `origin` `gateway:<client id>`.
  Only `internal/store/ingest/sends.go` writes it. Outside test files, only
  that file, the ingest schema and the scoped store's message columns name it.
  So no other code can attribute a message to a client.

**Protects.** I-2, and the attribution of [outbound integrity](#assets).

**Enforced by.**

- The `send-grant-first` and `sent-by-confinement` architecture rules
  (`internal/archtest/sends_test.go`). `send-grant-first` refuses a pointer to
  a grant, and a grant type of another package, as well as a missing grant.
- The `import-fences` rule's sender and API fences
  (`internal/archtest/imports_test.go`), and a test that runs `go list -deps`
  on `internal/api` and refuses any of the dependencies above
  (`TestTheAPIReachesTheArchiveOnlyThroughTheScopedStore`).
- The sender's property test ([row 22](#22-read-search-and-send-budgets)), and
  the send store's tests (`internal/store/ingest/sends_test.go`,
  `sent_test.go`).

**Limits.**

- `send-grant-first` reads `sends.go` only. An exported send method declared
  in another file of the ingest package passes it.
- `sent-by-confinement` reads string literals and statements assembled from a
  file's constants. A column name assembled at run time passes it.

**Since.** v1.0.0.

### Other tests

Other tests that guard the invariants:

- A byte scan of `archive.db` and its journal, the full-text index's
  vocabulary and page keys, and a `MATCH` query for a revoked, an expired and
  an edited canary, and a byte scan for the media type and media reference of
  a revoked and an expired canary, with negative controls (I-8;
  [row 15](#15-verified-storage-and-deletion-on-disk)).
- Tests that a startup refusal never repeats the configured value, and that the
  token `admin init` prints appears exactly once in its output (I-5, I-7).
- Release builds prove that the `dev` build tag is absent. The build settings
  embedded in the binary show no `dev` tag, and the binary contains neither
  the marker string that only dev builds carry, nor the fake engine's marker,
  nor any symbol of `internal/engine/fake` ([row 21](#21-a-fake-engine-for-development-builds-only)).
  A release build also refuses to start when it sees a `WAWARDEN_DEV_*`
  variable.

## Startup refusals

`wawarden serve` validates its whole configuration, the master key, the
archive and the device store before it opens any socket. The first failed check
stops it with a stable reason code and exit status `2`. A refusal names the
variable or the file involved, never a configured value.
[configuration.md](configuration.md#startup-refusals) lists every check, its
reason code and its order.

The checks that matter most for security:

- **Unknown and unsafe variables.** The service refuses:
  - any `WAWARDEN_` variable the build does not implement, so a misspelt
    setting never falls back to a default unnoticed;
  - a `WAWARDEN_DEV_*` variable in a release build, such as
    `WAWARDEN_DEV_FAKE_ENGINE`, which selects the fake engine that only `dev`
    builds contain ([row 21](#21-a-fake-engine-for-development-builds-only));
  - `MCPGODEBUG` or `JSONSCHEMAGODEBUG`, set to any value, an empty one
    included. Both switch compatibility behaviour of the MCP and JSON Schema
    libraries, such as how tool schemas are inferred and encoded. A malformed
    `MCPGODEBUG` makes the MCP library panic when the program starts, before
    this check;
  - `GOTRACEBACK` set to anything other than empty, `none` or `single`,
    numeric levels included. Every other level prints every goroutine's stack
    in crash output.
- **Plaintext admin credential.** `WAWARDEN_ADMIN_TOKEN` is refused, whatever
  its value. Only a SHA-256 is accepted, from `WAWARDEN_ADMIN_TOKEN_SHA256` or
  `WAWARDEN_ADMIN_TOKEN_SHA256_FILE`. Without a hash, the admin listener is not
  opened.
- **Listeners.** A listen address that is not an IP literal with a port, a
  health address that is not loopback, and two enabled listeners that share an
  address are refused.
- **Root.** A real or effective user ID of 0 is refused without
  `--allow-root`.
- **Ownership and modes.** The data directory, `keys/`, the master key,
  `keys/retired/` and its keys, `history/`, `backups/`, `archive.db`,
  `session.db` and their journals must be owned by the service's user, have
  the modes [trust boundary 7](#trust-boundaries) gives, and not be symbolic
  links. A retired key must also be the key its name says, and not the current
  master key.
- **Storage.** The service refuses archive or device store settings that read
  back differently from the fixed ones
  ([row 15](#15-verified-storage-and-deletion-on-disk)), storage profile
  `local` on a network filesystem or without open-file-description locks, and
  an archive schema newer than the build knows.
- **Secrets in files.** A webhook secret file that grants write permission to
  its group or any permission to others, or that holds fewer than 32 or more
  than 4096 bytes, is refused. So is a backup recipient that is not exactly one
  age X25519 or hybrid recipient, such as an age secret key pasted by mistake.
  Neither refusal repeats the value.
- **Every value has a form and a range.** That includes the log level, the
  storage profile, the free-space floor, the owner's number, the history cap,
  the debug window, the webhook URL, the backup schedule, the audit retention,
  and the rate and send limits.
- **Rate limits.** A limit of zero, a negative one, or one above its hard cap
  is refused; `0` never means unlimited. `WAWARDEN_UNSAFE_RATE_CAPS=1` lifts
  the read and search caps (600 and 60 a minute per client) to 1,000,000 a
  minute, and logs `unsafe_rate_caps` at every start. The send limits have
  their own caps (10 a minute per client, 60 an hour for all clients) and
  their own override, `WAWARDEN_UNSAFE_SEND_CAPS=1`. It lifts them to 60 and
  600, never further, and logs `unsafe_send_caps` at every start. The read
  override never lifts the send caps.

An unset `WAWARDEN_OWNER_PHONE` refuses pairing, not the start.

## Pairing refusals

- **Before pairing.** Pairing is refused while a device is already paired,
  while the owner's phone number is missing, and beyond three attempts an
  hour. An owner's number that is set but not in E.164 form refuses the start.
  The admin route that starts pairing answers:
  - `409` `already_paired` while a device is paired;
  - `409` `owner_phone_missing` without the owner's number;
  - `429` `rate_limited` beyond three attempts an hour.
- **The pairing code** goes to the caller of that route only. It is never
  logged, counted, kept or put into an event.
- **After pairing.** The linked account must be the owner's number. Otherwise
  the engine raises a `pair_rejected` alert and logs the new device out. It
  retries with backoff, and raises a `logout_failed` alert, for as long as the
  logout fails. Until the device is gone, the engine does not connect it,
  neither on its own nor on an explicit reconnect, and drops what its
  connection delivers before it is written.
- **Before every connection.** Before every connection, and at the start
  before the restart budget and the version fetch, the engine checks that the
  stored device's number is exactly the owner's. When it is not, the engine
  stays disconnected (`owner_mismatch`) with an alert. It refuses an explicit
  reconnect, drops what a connection would deliver, and leaves the device in
  place for the operator, without logging it out. Two cases lead here: a
  rejected device whose logout never succeeded before a restart, and an
  owner's number that differs from the paired one.
- **When the owner's number is unset,** the stored device is not checked, and
  the stored number never reaches a log or an event.
- **In the adapter.** The protocol adapter refuses another account before the
  protocol library saves anything. It requests a pairing code for the owner's
  number only, drops QR codes, deletes the local device when a logout cannot
  reach WhatsApp, and starts from a fresh device after a logout.

A pairing completed from someone else's account therefore cannot bind the
gateway to that account. These rules are tested in the engine core and in the
protocol adapter.

## Secure defaults

| Default | Since |
|---|---|
| The client and admin listeners bind to loopback (`127.0.0.1:8080`, `127.0.0.1:8082`) unless configured otherwise. A warning is logged at every start for each of them bound elsewhere, since plain HTTP carries bearer tokens, and the log level cannot suppress it. The health listener defaults to `127.0.0.1:8081` and accepts only loopback addresses. | v0.1.0 |
| The admin listener exists only when an admin token hash is configured. | v0.1.0 |
| `umask 077` and the `single` traceback level are set by the first two statements of `main`, which an architecture test checks. | v0.1.0 |
| HTTP servers have read-header, read, write and idle timeouts and a 16 KiB header limit. Responses written by WaWarden's handlers carry `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`, and no CORS headers. Go's HTTP server answers malformed requests itself, before any handler runs, with a plain-text or empty error response without those headers. | v0.1.0 |
| Requests to the client or admin listener carrying an `Origin` or `Sec-Fetch-Site` header are refused with `403`. `OPTIONS` on those listeners is refused with `405`. | v0.1.0 |
| Every request on the client and admin listeners is authenticated before its body is read. Error bodies are fixed strings that never echo the request. | v0.1.0 |
| The container image runs as a non-root user (`65532:65532`) on a distroless static base with no shell, and works with a read-only root filesystem. | v0.1.0 |
| The data directory is created with mode `0700`. | v0.1.0 |
| Request bodies are read only by a route handler, after authentication and the route's decision, through one strict JSON decoder. It takes at most 16 KiB; the client create route and the send route take 64 KiB, so that two sets of 256 chats, or the longest text with every character escaped, fit. It requires exactly one `Content-Type` of `application/json`, valid UTF-8 without a byte-order mark, and one object at most 8 levels deep with lower-case `snake_case` keys. It refuses a duplicate key once escapes are decoded, an unknown field and trailing data. Refusals are fixed `415`, `413` and `400` answers that never repeat the request. The pairing, reconnection and revocation routes accept only the empty object. The only client REST route that reads a body is the send route, after its write grant is decided. `POST /mcp` checks its body before the MCP library reads it ([row 24](#24-the-mcp-endpoint-behind-the-client-pipeline)). | v0.2.0 |
| Secrets reach the service as `_FILE` paths, never as command-line arguments. The CLI reads the admin token from a file, standard input or a token command, never from command-line arguments or environment variables, and checks its form before sending it. | v0.1.0 |
| The service holds one injected secret: the optional webhook signing secret, which the operator supplies as a `_FILE` path and the service reads once at start. The admin credential is a hash, client tokens are hashed in the database, and every other key is derived from a master key generated on first start, with mode `0600`. | v0.1.0 |
| Database files are created with mode `0600` before SQLite opens them, and their journals take that mode. | v0.2.0 |
| Every log line passes through one log writer, which redacts tokens, pseudonymises identifiers, drops XML lines and renders values fail-closed. The protocol library's and the Signal library's logs reach it through an adapter, and the MCP library has no logger. Tool arguments are never logged. [Row 13](#13-one-log-writer) gives the rules. | v0.2.0 |
| Text that the admin CLI prints from a server answer passes through a terminal sanitiser. It replaces every control, format, line-separator and paragraph-separator character, and every invalid byte, with U+FFFD. So an answer cannot move the cursor, rewrite the screen, set a link or write the clipboard. The refusal code the CLI prints has client and admin tokens redacted as the log writer does. A test runs failing `serve`, `admin`, `audit`, `keys` and `backup` commands with tokens as their inputs and as the listener's answer, and finds no token secret in their output. | v0.2.0 |
| The notification webhook is off unless configured. When it is on, [trust boundary 9](#trust-boundaries) gives its transport and destination rules. | v0.2.0 |
| Live status, broadcast and newsletter traffic, and live traffic of every other chat that is not a phone-number user, a LID user or a group, is dropped at ingest, before it is written anywhere. Such conversations inside a history-sync blob are dropped while the blob is applied. The blob's file and its inline content, which hold them until then, are deleted once the blob is processed or quarantined. | v0.2.0 |
| Live protocol messages are not stored. The engine receives each live message as plain data, a type with no field for a media key, a message secret or the protocol message. The archive's `messages` table has no `raw` column, which a test checks. History sync is the exception. Each history blob, the protocol's own messages with their media keys and message secrets, is kept as received, in a `0600` file under `history/` or, when it arrives inline, in the archive. The archive keeps its download reference, media key included. Both are kept only until the blob is processed or quarantined, and a start deletes the files of blobs no longer pending. The protocol library keeps its own state, message secrets included, in the device store (see [Assets](#assets)). | v0.2.0 |
| The service never sends read receipts, presence, typing indicators or status updates. No code asks the protocol library for them, and an architecture test refuses the calls. The protocol library sends delivery receipts in their inactive form, and other traffic by itself (see [Protocol library](#protocol-library)). | v0.2.0 |
| A service without a paired device makes no connection to WhatsApp, not even the version fetch, until pairing is requested. It reports `unpaired` once, and again whenever a paired device is lost. | v0.2.0 |
| Outbound HTTP to WhatsApp uses no proxy from the environment, a timeout on every step, TLS 1.2 or later, response caps and no redirect to another host. The caps are 8 MiB for the version page, and the history cap plus 32 bytes for a history download. | v0.2.0 |
| No first contact, a direct chat without any stored message from the other person, unless the client is allowed it. Sends are paced, idempotent and budgeted per client, with `429` rather than queueing. A send whose outcome is unknown is stored as failed, and the service never sends it again. Outbound text with control, bidirectional or tag characters is refused, never altered. | v1.0.0 |
| Disconnections from WhatsApp never exit the process, and `/healthz` does not depend on them. More than five starts within ten minutes start the engine disconnected. Reconnection after a drop backs off exponentially, with jitter, up to five minutes, to avoid reconnect storms, with the protocol library's own reconnection switched off. A connection that drops within a minute of coming up counts as a failed attempt and does not reset the delay. A replaced session, a temporary ban, a refused connection, a failed token refresh, or a stored device of another number than the owner's waits for the operator. | v0.2.0 |
| With storage profile `local`, ingest pauses while the data directory's filesystem has less free space than `WAWARDEN_MIN_FREE_BYTES` (256 MiB by default; `0` turns the floor off). It reports `ingest_paused`, and resumes once the free space reaches 1.25 times the floor. While it is paused, the engine writes no message and no history-sync notification and leaves them unacknowledged, which loses them (see [Protocol library](#protocol-library)). It also applies nothing from its inbox or from history sync. Profile `nfs` has no floor. | v0.2.0 |
| No backup is taken without `WAWARDEN_BACKUP_AGE_RECIPIENT`, and the start says so with `backup_disabled`. Every backup is encrypted to that recipient, and there is no plaintext backup of either database. One backup is taken per paired device once its initial history sync has settled. Then one is taken per night at `WAWARDEN_BACKUP_TIME`, `03:00` UTC by default, and the newest `WAWARDEN_BACKUP_KEEP`, `14` by default, are kept. A backup that would take the data directory below its free-space floor is refused. | v0.2.0 |

## Residual risks

These remain after every control above is in place. They are grouped by
subject.

### Account and protocol

- **Unofficial protocol and account standing.** WhatsApp offers no official
  API for a personal account. WaWarden is a linked-device client built on an
  unofficial implementation of the multi-device protocol, and WhatsApp can
  restrict, log out or ban the account. The defaults reduce the signal but
  cannot remove it: one linked session, read-mostly use, no read receipts or
  presence, no reconnect storms, guarded pairing, and paced sends to explicit
  chats without first contact. The phone must also come online at least every
  14 days, or WhatsApp logs out every linked device. [ban-risk.md](ban-risk.md)
  has the details.
- **Protocol churn.** A protocol change can stop WhatsApp connectivity until a
  new release ships. Reads of the archive keep working. The engine refreshes
  the client version at run time for the common case.
- **Backfill depends on the owner's phone.** `POST /admin/v1/backfill` sends a
  history request to the owner's primary device, at most one every 5 minutes
  and 20 a day, and only the administrator can make one.
  - The phone answers when it processes the request, possibly after a long
    time or never. How long WhatsApp keeps a request for an offline phone is
    not known.
  - That the answer names the request's id, which marks the request complete,
    comes from a report by users of the protocol library that has not been
    verified here. An unanswered request stays counted and incomplete.
  - Which chat identifier form, phone number or LID, the phone expects for a
    direct chat is not verified either.
- **Limits that WhatsApp does not publish.** The text cap of 4096 characters
  and 8192 bytes is this service's policy. WhatsApp's own limit is not known,
  nor how it renders a reply to a media message, which the service never
  sends.
- **Bot accounts look like users.** WhatsApp's own bot accounts, such as its AI
  assistant, have phone-number identifiers. `Normalize` accepts them, and
  their chats are archived like any other.
- **WhatsApp behaviour not observed yet.** These are not known:
  - whether the phone honours the request for a full history sync;
  - how large real history blobs are;
  - whether WhatsApp's servers check that an admin revocation comes from an
    admin. The engine checks the archive's record either way;
  - whether every country's account identifiers equal its E.164 numbers. When
    they differ, pairing fails closed.

### Protocol library

This subsection is the canonical statement of what the protocol library
(`go.mau.fi/whatsmeow`) does that WaWarden cannot change.

- **Traffic the protocol library sends by itself.** While connected, the
  protocol library:
  - acknowledges every stanza;
  - sends a delivery receipt for every message it decrypts, in the inactive
    form that a sender's phone does not show as delivered unless the device
    announced itself as available, which WaWarden never does;
  - sends retry receipts for messages it cannot decrypt, and asks the owner's
    phone to resend messages WhatsApp marks as unavailable;
  - announces the device as active at each connection;
  - uploads pre-keys and fetches the application state;
  - sends session telemetry once after pairing.

  None of this can be switched off. All of it tells WhatsApp, and in part the
  sender, that the device is online.
- **A refused or interrupted message is lost.** The protocol library advances
  and saves its session keys when it decrypts a message, before the engine
  sees it. It acknowledges the message only after the engine wrote it to the
  inbox.
  - A message the engine refuses is delivered again by WhatsApp. Refusals are
    a full inbox of 5,000 rows, an ingest paused below the free-space floor,
    and a failed write. So is a message decrypted just before the process
    stops.
  - The protocol library can no longer decrypt that second copy, and drops it,
    so the message never reaches the archive.
  - Group changes are acknowledged as they arrive, and are lost the same way.
  - The protocol library's buffer of decrypted messages would make delivery
    at-least-once, at the price of message plaintext in the device store. It
    stays off.
- **Crafted live messages are decoded before WaWarden sees them.** The
  protocol library decodes every decrypted message with protobuf's default
  limits: 10,000 levels of nesting, and no size limit below its 16 MiB frame.
  So a peer can make the process allocate many times a message's size, about
  40 times in measurements, before any WaWarden check runs. History blobs,
  which the adapter decodes itself, are held to a nesting of 28 levels and the
  history cap.
- **The device store belongs to the protocol library.**
  - The protocol library's queries on `session.db` run without the archive's
    per-call deadlines, on the single connection that holds the lock. Whether
    its concurrent use of that one connection can stall has not been observed
    outside tests.
  - The protocol library keeps the identity mappings and privacy tokens it
    learned after a device is deleted. It accepts a contact's changed identity
    key without asking.
  - Its own identity map, which any of the owner's devices can write, is never
    read for the archive.
  - Sends address canonical chats. The adapter builds the recipient from the
    chat's canonical identifier, and reports the sent message under that chat,
    even when the protocol library rewrites a phone number to a LID through
    that map.
- **Requests answered by the owner's phone.** When WhatsApp marks a message
  unavailable, the protocol library asks the owner's phone for it. It passes
  the phone's answer on as live traffic in the chat that answer names.
  - The protocol library accepts such answers only from the owner's primary
    device, the trust that history sync gets as well.
  - It builds them the way it builds history rows. So an edit's identifier and
    message fields hold its target's identifier and the new text. The edit's
    own identifier and protocol message remain only in the raw message and the
    row the phone sent, both of which the event carries.
  - The adapter reads each answer from that row, as it reads history rows. So
    an edit is applied to its target, and never stored as the target itself,
    which a test through the engine checks.
  - Nothing in an answer shows that its sender wrote it. The archive holds
    what the owner's phone says was sent.

### Dependencies

- **Upstream dependency.** The protocol library has a single maintainer and no
  tagged releases.
  - `go.mod` pins it to an exact pseudo-version, and `go.sum` pins its
    content. The go command checks every download against that hash. With
    default settings, it checks a new hash against the Go checksum database
    before recording it.
  - Modules are never vendored: the build script, the CI `modules` job and an
    architecture test refuse a `vendor` directory. So a build needs the Go
    module proxy, or whatever `GOPROXY` names.
  - A scheduled workflow proposes each update as a pull request, and never
    merges one. The pull request carries the upstream commit log, a diffstat
    and the diff of the watched paths between the two module versions'
    sources. When GitHub's limit on a description cuts the diff, it is complete
    in an artifact of the run. Its CI runs only once a maintainer approves it,
    and a maintainer reviews and merges it by hand
    ([RELEASING.md](../RELEASING.md)).
  - The protocol library links the Signal library, `go.mau.fi/libsignal`,
    licensed under GPL-3.0. That is why WaWarden is licensed under
    GPL-3.0-or-later.
- **The MCP library.** `POST /mcp` relies on the MCP library, the MCP Go SDK,
  to parse and dispatch JSON-RPC.
  - Its own refusals, before a request reaches the server, can repeat the
    caller's method name, header values or message text, though never anything
    read from the archive.
  - It runs each request on a goroutine of its own. Tool handlers recover
    their panics ([row 25](#25-mcp-tool-calls-fail-closed)), but a panic in the
    MCP library's own code would stop the process.
  - A client that disconnects does not cancel a running tool call, which ends
    at its 10-second deadline.
  - The MCP library also checks a token's expiry against the wall clock, after
    the service checked it on its injected clock. A token that expires between
    the two checks gets the MCP library's plain-text `401`.
- **Panics in dependency goroutines.** A panic in a goroutine that a
  dependency starts, such as the protocol library, is not recovered. The
  process exits, and the Go runtime prints the panic value to standard error
  first. That value can carry chat data, so I-5 does not hold for crash
  output.
  - The protocol library handles each incoming stanza on a goroutine of its
    own, and recovers a panic in only a few places.
  - The adapter removes the one known panic, a missing connection-token
    refresh, by stubbing it. It recovers a panic in its own event handler and
    in the engine code that handler calls, and refuses the event then. A
    message whose handling panicked is therefore lost the way a refused
    message is ([Protocol library](#protocol-library)).
- **SQLite on a network filesystem** is outside SQLite's recommended
  configurations. The network-filesystem storage profile reduces the risk; it
  does not remove it. That profile uses a rollback journal, exclusive locking,
  no write-ahead log and a single process. It uses classic POSIX record locks,
  which a close of any other descriptor of the database file in the process
  would drop. The service's own code never opens the file
  ([row 3a](#3a-one-database-handle)), but a dependency could.
- **A database deadline lost in the driver.** A deadline that passes just as
  the database driver starts a statement can be lost.
  - That statement then runs to its end before the call fails, past its call's
    deadline (2 seconds for a read, 10 for a write), and nothing bounds how
    long it runs.
  - Until it ends, the archive's one connection stays taken. Other calls wait
    for it no longer than their own deadlines: a read then answers `503`
    (`busy`), and a write, an ingest write included, fails.
  - Grants and the audit chain still hold. The loss costs availability, not
    scope. See [Message archive](configuration.md#message-archive).
  - A test checks that interruption works and tolerates the race. Slow reads,
    writes and statements under a 100-millisecond deadline must each return
    within 3 seconds. A call that overruns because its interrupt was lost or
    late runs again, up to three times, and the test fails only when all three
    overrun.

### Tokens and the admin token

- **Bearer tokens are not bound to a network.** A stolen token works from
  anywhere it can reach the service, and a network mistake goes unnoticed. See
  [Bearer tokens without network binding](#bearer-tokens-without-network-binding).
- **The failed-authentication throttle does not limit guessing.** See
  [Failed authentication](#failed-authentication).
- **The admin credential is a single factor.** See
  [The admin token](#the-admin-token).
- **Revocation in flight.** A request authenticated just before a revocation
  commits finishes with its grant, within the 2-second read deadline.

### Timing and volume

- **Timing of an out-of-scope message.** A message named outside the client's
  scope is refused before any message query runs. A message inside the scope
  that does not exist costs one query. Both answers are the same, but the first
  comes sooner. The difference tells a client only that a chat lies outside its
  own scope, which its grant already tells it. Chats outside the scope and
  missing chats run the same query, and cannot be told apart by the SQL they
  issue.
- **Timing of authentication.** Every authentication hashes the presented
  token once and compares it once, whatever the outcome. So the time spent does
  not depend on how much of a secret is right. What still varies is outside the
  secret:
  - parsing stops at the first part of the token's public form that is wrong;
  - a token refused for its revocation or expiry reads the clock once its hash
    has matched;
  - a request that arrives while a client change is being published waits for
    it, for at most 2 seconds, whatever its token.

  None of these tells a caller more than the answer it gets.
- **Timing of a malformed message reference and of a cursor.** A message
  reference that does not open answers `404` without any query, sooner than a
  reference that opens. The answer is the same, and only references the service
  sealed for this client open. A request with an invalid cursor that names a
  chat looks the chat up first. So the `404` of a chat outside the scope and
  the `400` of a chat inside it cost the same query. A cursor never opens under
  another client, route, chat or query.
- **Timing of search.** A search walks the full-text index across every chat,
  in each window of 20,000 sequence numbers, and applies the client's scope to
  each match afterwards.
  - Its time therefore grows with the number of messages in the window, in
    chats outside the scope, that hold the query's trigrams: about a
    millisecond for each thousand on a development machine.
  - By comparing the time of searches that give the same answer, a client that
    reads some chats can estimate how common a term of 3 or more characters is
    in chats it cannot read. It can do so within each stretch of up to 100,000
    sequence numbers it can reach by following `next`. It learns neither the
    text nor which chat holds it.
  - Only the search budget (60 a minute by default) and the 2-second read
    deadline bound this. Walking the scope first does not remove it, since each
    probe of the index costs more for a term with more matches anywhere, and it
    is far slower for a busy chat.
  - A client that reads every chat learns nothing from it.
- **Volume through paging.** Search and the change feed walk the whole archive
  in windows of 20,000 sequence or change numbers, at most five a call. `more`
  stays `true` until the walk reaches the start of the archive, for search, or
  its newest change, for the feed. The number of calls a client needs, and the
  empty pages with `more: true` among them, therefore grow with the number of
  messages and changes in the whole archive, not only in its scope. A client
  can estimate the archive's size in steps of 100,000, and see that changes
  happen in chats it cannot read, but not where or what they are.
- **Rate limits restart with the service.** The read and search budgets live
  in memory and start full at every start. A client that can make the service
  restart gets a fresh burst; a restart costs far more than the burst it buys.
- **The pacer and the global budget are shared.** The pacer and the global
  hourly limit count every client's sends. The single `rate_limited` code never
  says which limit refused a send, and no answer names another client or its
  chat. But the `Retry-After` of a refusal shows something of other clients'
  sends:
  - A refusal by the pacer, or by the one send the engine runs at a time, tells
    a client that some client sent within the last few seconds.
  - A refusal by the global hourly limit carries a `Retry-After` from which the
    client can work out, to the second, when the oldest send still counted in
    the last hour was admitted. That holds whichever client made it, to a chat
    outside the asking client's scope included.
  - A refusal is not counted, and costs only one read from the read budget, so
    the client can ask again.
  - The global limit refuses only once it is spent. That needs more clients
    that may write than `WAWARDEN_SEND_GLOBAL_PER_HOUR`, or a change within the
    hour in that number or in the limit. After such a change, the sends already
    counted can leave less of the global limit than the new shares allow.
  - The per-minute limit and a client's hourly count take its own sends alone.
    But its hourly share is the global limit divided by the number of clients
    that may write at that moment. A client that sends up to its share can
    therefore tell how many clients may write. It sees a write client created,
    revoked or expiring when a refusal turns into an admission or back, or when
    its `Retry-After` moves.
- **The send route is visible to every client.** Any method other than `GET`,
  `HEAD` and `POST` on `/v1/chats/{ref}/messages` is answered `405` with
  `Allow: GET, HEAD, POST` before any grant is decided. A read-only client
  therefore learns that the route exists, which this documentation states
  anyway. A `POST` from a client without a write grant is the uniform `404`.

### Audit chain limits

- **Audit anchoring.** These limits follow from the
  [audit chain](#audit-chain)'s design:
  - Whoever can write `archive.db` and holds the master key can rewrite the
    audit chain or cut its end, and the table still verifies. Only chain heads
    already shipped off the host show it. A deployment that keeps standard
    output only on the host has no root of trust for the chain.
  - The audit anchor is signed under the master key. Without the key, the
    start of the chain cannot be cut behind a forged audit anchor. With it, the
    audit anchor can be moved forward over any rows. Only their shipped heads
    then show it, and only if their log times are after the audit anchor's.
  - `audit verify` accepts an audit anchor signed under a retired key, unless
    no row is left or the first one left was written under the current key.
    Behind such an audit anchor, only shipped heads whose lines name a key
    other than the current one count as pruned. No row under a retired key may
    follow one under the current key.
  - So whoever holds a retired key and can write `archive.db` can still, against
    every shipped head, remove the rows of any key but the current one from the
    start of the chain. They sit behind an audit anchor dated after them, and
    their shipped heads then count as pruned. With one retired key these are
    its own rows and those of keys missing from the key set. Across two or more
    rotations they also include the rows of another retired key.
  - After a rotation, and until a row is written under the current key, that
    holder can also append rows under the retired key. `audit_key_changed`
    reports that state at each start.
  - The retention checks the rows it removes, and the audit anchor before them,
    under the keys the service holds, before it signs. So it does not sign over
    a row renamed to a retired key. But it cannot tell rows written under a
    retired key in place of every row under the current key from rows written
    before the rotation.
  - So the one removal that first reaches the current key's rows can still be
    rewritten under a retired key, when it reaches all of them. Once every row
    written under the current key is older than the retention's cutoff, whoever
    holds a retired key and can write `archive.db` while the service is stopped
    can replace all of those rows with rows under the retired key. They can
    also replace an audit anchor that names one of them with one made under the
    retired key.
  - The next removal accepts them and signs its audit anchor under the current
    key. Their shipped heads then count as pruned. The replaced newest row
    stays under the retired key, and later rows chain on it.
  - The start after that write reports `audit_key_changed`, as after a
    rotation, until a row is written under the current key. `audit verify
    --log` on a backup taken before that removal finds the replaced rows'
    shipped heads missing.
- **Audit growth and retention.** Rows are kept
  `WAWARDEN_AUDIT_RETENTION_DAYS` days, 90 by default.
  - A client reading continuously at the default read budget adds about 131 MB
    a day, so the table can hold about 12 GB per such client. The archive file
    does not shrink when rows go.
  - Removed rows can be checked only in a backup taken before the removal,
    until that backup is deleted. With a backup recipient, the retention waits
    for the night's backup and for the first one. Without one, it removes rows
    that no backup holds.
  - `audit verify` counts a shipped head that the copy lacks as pruned when the
    time on its log line is not after the audit anchor's time. That time is the
    latest time of the rows removed, which the removal never sets more than 10
    seconds after its cutoff. So the head of a retained row logged no later
    than the audit anchor's time counts as pruned, not missing, when the row is
    gone from the copy. That is so also when someone without the key moved the
    times of rows or of the audit anchor.
- **Key rotation is offline and manual.**
  - `keys rotate` refuses while another process holds the archive's lock, but
    it does not keep that lock. So a service started while it runs can load
    the old key.
  - An interrupted rotation leaves the current key in `keys/retired/`, which
    the next start refuses until `keys rotate` runs again.
  - Retired keys are never deleted by the service. Deleting one leaves the
    audit rows and audit anchor made under it `unknown_key` or
    `anchor_invalid`, and its cursors and references unopenable. Keeping one
    keeps its power over what was made under it.
  - A message reference for the same message reads differently after a
    rotation.

### Sends

- **A lost acknowledgement.** When WhatsApp's acknowledgement of a send does
  not arrive, the service cannot tell whether the message arrived. That happens
  on a timeout, a drop after the message was written, a restart, or an expired
  60-second lease.
  - The service stores the send as failed. It answers `504`
    `send_outcome_unknown` to the request and to every replay, reports
    `send_outcome_unknown`, and never sends it again.
  - The contact may have the message or not; only reading the chat tells.
  - The protocol library itself writes a message again under the same id when
    the connection comes back within 5 seconds. Whether WhatsApp's servers drop
    such a repeat is not known.
  - When an acknowledged send cannot be stored, the request answers `500`,
    `send_unrecorded` is reported, and the key answers `504` once its lease
    ends, although the message was delivered. If history later brings the
    message, it is stored as the owner's, without the client's origin.
- **A delivered send can answer `busy`.** The audit row of a request is written
  after the send has settled, just before the answer, and waits at most 2
  seconds for the archive. So does the read of the stored message that the
  answer carries.
  - When the archive is held longer, by a long ingest write for example, a send
    that went out is answered `503` (`busy`), as an unanswered read is.
  - When it was the audit row that waited too long, the send has no row. When
    it was the read, its row says `busy`.
  - The archive still stores the message with `origin` `gateway:<client id>`,
    and the idempotency ledger still holds the send. A retry under the same key,
    which `busy` asks for, answers the stored `201` and writes the row.
  - A client that does not retry leaves the send attributed in the archive but
    not in the chain.
- **Recipients cannot tell a gateway send from the owner's.** A message sent
  through WaWarden goes out from the owner's account like any message the owner
  types, so contacts cannot see that an agent wrote it. Only readers of the
  archive see `origin` `gateway:<client id>`. Every client whose read scope
  holds the chat sees which client sent it; the id is a look-up handle, not a
  credential. The quote of a reply carries the quoted text as the archive holds
  it.
- **Replayed results.** For 24 hours, a client's idempotency key answers the
  stored outcome of its first send to whoever presents that client's token,
  without the limits.
  - A stolen token therefore learns whether a key was used, and reads the sent
    message as it is now (a revoked message without its text).
  - Keys are per client, so another client's key names nothing.
  - The ledger holds the key, a fingerprint, the message id and the outcome,
    never the text or the chat.
  - A restore from a backup takes the ledger back to the backup's time. So a
    send made since can be made again under its old key (see
    [Backups](#backups)).

### Clients and agents

- **Prompt injection through message text.** Message text, contact names and
  group subjects are written by third parties and reach agents by design.
  - WaWarden limits what a manipulated agent can do with per-chat read and
    write scopes, and no write access for read-all clients. Text is marked
    `untrusted` with its `origin`, tool definitions are constant, and every
    request has an audit row. Sends allow no first contact, at most one send
    per request and per key, and low limits behind a pacer. Only clients that
    may write see the `send_message` tool.
  - It cannot stop an agent from acting on what it reads, or from sending what
    a message told it to a chat it may write to.
  - A request outside the scope cannot be counted apart from a request for
    something missing, since both are the same `404`. So
    `wawarden_policy_denials_total` stays `0` and is no alarm. The audit rows'
    `not_found` reasons are what shows a client probing.
  - How an agent should treat what it reads is in [agents.md](agents.md).
- **References of merged chats.** When a re-key merges a chat without messages
  into the other chat of the same person, the merged chat's reference stops
  resolving and answers as missing. A merge that would remove a chat a client
  names is refused instead (`rekey_conflict` with `scoped_chat`). So only a
  reference that a client reading every chat kept is affected. The surviving
  chat stays listed.
- **Search normalisation.** The search query is normalised to NFC, but stored
  text is kept as it arrived. So a message whose text arrived decomposed (NFD)
  is not found by a query for a word in it that has a precomposed form.
- **Search terms in proxy logs.** `GET /v1/search` carries its query in the
  URL, as `q`. WaWarden never logs it, but a reverse proxy in front of the
  client listener usually logs every URL. Its access logs then hold what
  clients searched for, and keeping, trimming or protecting them is the
  deployer's concern. The MCP tool `search_messages` carries its query in the
  request body instead. The other paths and parameters hold only chat
  references, sealed cursors and message references, which name no chat or
  person.
- **Group admins as the archive knows them.** A revocation of another member's
  message in a group is applied when the archive records the revoker as an
  admin of that group.
  - That record is only as current as the group changes and history WhatsApp
    has delivered. A demotion the engine never received leaves a former admin
    able to revoke. A member the archive does not record is refused.
  - A member list from history sync never overrides what a live group change
    recorded. It cannot raise, re-add or remove a member that a live join,
    promotion, demotion or removal named. It is ignored for a group whose whole
    member list arrived live.
  - It still records the members no live change has named, so a group known
    only from history keeps its admins.
- **References from the other side of a direct chat.** The other party of a
  direct chat names it by the owner's identifier. The engine accepts the
  owner's number there. It accepts the owner's LID only once the archive has
  learned which LID belongs to that number. Until then, and whenever
  `WAWARDEN_OWNER_PHONE` is unset, such edits, revocations, reactions and
  replies are dropped, or stored without their reference.

### Logging

- **Log pseudonyms can be linked and tested.** A pseudonym stands for the same
  identifier in every log line until the master key changes, so lines about
  one chat can be linked to each other. It is 32 bits long, so two identifiers
  can share one. Whoever holds the master key, from the data directory or a
  backup, can confirm a guessed identifier against a pseudonym.
- **Identifiers are recognised by their shape.** Only identifiers in
  `user@server` form are recognised, and only when no ASCII letter or digit
  follows the server name directly. A phone number on its own, a
  protocol-library address of a user and a device without a server, or an
  identifier run into the following word passes unchanged, so code must not
  log one.
- **Tokens are recognised by their exact shape.** The log writer, the
  protocol-library adapter and the refusal code an admin command prints redact
  only a whole client or admin token of the generated shape. A token cut short,
  split by other text or written in another encoding passes unchanged, so code
  must not log one. The `token:` line of `admin clients create` and of
  `admin init` prints the new token on purpose, outside the writer. The admin
  CLI otherwise prints server answers through the terminal sanitiser only. The
  Go runtime's crash output does not pass the writer.
- **XML is recognised by its shape.** The log writer drops a line only when it
  holds one of the XML shapes listed in [row 13](#13-one-log-writer) and
  [Pseudonyms and dropped lines](observability.md#pseudonyms-and-dropped-lines).
  An opening tag without attributes followed only by text, such as a cut-off
  `<body>` element, passes. So code must not log message content, even inside
  a tag.
- **Media locations are recognised by their shape.** Only the
  protocol-library adapter replaces media URLs, direct paths and their query
  parameters, as [row 13](#13-one-log-writer) lists; the log writer itself
  does not. A URL with another scheme, a direct path of another shape that
  carries no query or is run into the preceding word, and a parameter with
  another name pass that adapter unchanged, so code must not log one.
- **Notifications can be late, lost or read by their receiver.**
  - Failed admin authentications after the first are reported up to about 70
    seconds later, together.
  - The webhook drops events when its queue of 64 is full, after five failed
    attempts, or at shutdown, counting them in `wawarden_notify_dropped_total`.
    A receiver that must not miss an event should alert on the EMF metrics as
    well.
  - The receiver learns the events, their reasons, counts and times. Anyone
    with the signing secret can forge events that the receiver accepts, so keep
    the secret file and the receiver's copy as private as the admin token.
  - The URL itself can hold a secret of the receiver's, and sits in the
    service's environment.
  - The destination check refuses addresses by range. So a public address that
    the operator did not mean, or an allowed private one, is reached as
    configured.
  - It recognises only the well-known and local-use NAT64 prefixes. On a
    network whose NAT64 uses a prefix of its own, a name whose IPv4 address is
    private or link-local is synthesised into an IPv6 address that the check
    does not map back, and is reached.

### Deletion and storage

- **Deletion residuals.**
  - Backups taken before a message was revoked, edited or expired keep its old
    text until they are deleted. Retention deletes a backup once
    `WAWARDEN_BACKUP_KEEP` newer ones exist. So on an active service, the old
    text stays in backups for that many nights after the change.
  - A service that stops backing up keeps its last backups indefinitely, and a
    prune refused for an unexpected entry deletes nothing.
  - Copies made outside `backups/`, such as storage snapshots, recovery points
    or copies the operator took off the host, keep the old text for as long as
    they are kept. Filesystem blocks, snapshots and storage-level copies are
    outside the service's control.
  - A restore brings revoked, edited and expired text back into the archive,
    the expired text until the next sweep.
  - A trigram of the old text that a remaining message also holds stays in the
    full-text index, because that message still needs it.
  - The deletion clears only the message's own text. Those bytes stay in the
    archive when the same text, or one of its trigrams, is held by a push name,
    a saved contact name, a chat name or a group subject, a message waiting in
    the inbox, an identifier or another stored value, or the schema.
  - The full-text index finds a term through page keys, and a key can be a
    prefix of the first term of its page. The index rewrite runs only for a key
    that is a whole trigram of the old text. So a key holding its first one or
    two characters, or part of a character, stays until a later rewrite.
  - A trigram that is a key also stays from the commit of the deletion until
    the rewrite commits. When the process stops or the rewrite fails in
    between, it stays until the next start or the next rewrite.
  - A revocation or an edit whose target is not yet in the archive is dropped,
    and not kept for later. History sync can store the target afterwards, as it
    can during the initial sync, where live traffic is applied while blobs wait
    for their download. The target then keeps its original text, and nothing
    that arrives later removes it.
- **An interrupted revoke.** A crash in the middle of a revoke, an edit or an
  expiry rolls it back. The old text is then in the database again until the
  change is applied again, and the journal keeps the pre-image until the next
  write truncates it.
- **Crashes count as attempts.** A history blob whose processing is cut short
  by a crash three times is quarantined like a failing one. The batches it had
  applied before each crash stay in the archive, and its remaining messages do
  not reach it. A graceful stop gives its attempt back.
- **Raw history blobs wait on disk.** A downloaded history blob, the
  protocol's own messages with their media keys and message secrets, stays in
  its file under `history/` until it is processed or quarantined. A start keeps
  the files of pending blobs. So while ingest is paused or the service is
  stopped, a blob can stay there indefinitely.

### Backups

- **Who encryption protects against.** Encrypting to a recipient only protects
  a backup from whoever reads it without the age identity: the storage it is
  copied to, a backup of the data directory, a stolen disk. It does not protect
  a backup from whoever holds the identity, nor the live databases, which are
  in plaintext on the same host.
- **No check in the service.** The service cannot check that an earlier backup
  still decrypts. `wawarden backup verify` checks a backup offline, where the
  identity is kept. Nothing checks the backups in `backups/` unless the
  operator copies them off and runs it.
- **Plaintext while a backup runs.** A plaintext copy of one database at a time
  sits in `backups/tmp/` with mode `0600`. On a filesystem that keeps deleted
  blocks or snapshots, it can outlive its removal. A crash leaves it there
  until the next start removes it.
- **age does not authenticate the sender.** Anyone who knows the recipient,
  which is a public key, can write a well-formed backup that decrypts. So a
  backup is only as genuine as the storage that holds it. The permissions of
  `backups/`, and of wherever backups are copied, must keep others from
  replacing or adding files. A truncated or altered backup is detected only
  when it is decrypted.
- **Two points in time.** The two databases are copied one after the other,
  the archive first, and the device store goes on changing after its copy. A
  restored device store holds the device's state as of the backup.
- **Restore rules are documented, not enforced.** Restore only into a stopped
  service, take both databases from one backup, and never start two instances
  from one device store. There is no command that restores; the
  [runbook](runbook.md#restoring-a-backup) gives the procedure.
- **A restore takes the archive back in time.**
  - Clients revoked after the backup are active again, with their old tokens.
  - The send budgets and the record of sends roll back, so a retried send can
    go out twice.
  - These hold until the operator revokes again from the `admin_mutation`
    events since the backup.
  - The audit rows written between the backup and the restart are lost, and
    their ids reused. So the heads shipped in that window never match again,
    and `audit verify --log` must be given a capture without them, as the
    runbook shows.
- **Keys are not in a backup.** `keys/master` and `keys/retired/` are not in a
  backup. A restore without the operator's copies changes pseudonyms, cursors
  and references, and leaves older audit rows `unknown_key`.
- **The device store going back in time.** What WhatsApp's servers and
  contacts do with a device store whose sessions and pre-key numbers went back
  in time is not known, and is testable only with a live account.
- **Retention.** Retention runs only after a successful backup. It deletes
  only what matches a backup's name, mode and owner. A prune that finds
  anything else deletes nothing, after every backup, until the entry is
  removed. So the directory grows past `WAWARDEN_BACKUP_KEEP`, and the
  operator must act on `backup_prune_failed`.
- **Skipped backups.** A backup skipped because neither database changed is
  skipped only within one process. A write that lands between a backup's copy
  and its bookkeeping is noticed only with the next write.
- **History blob rows.** The rows of history blobs processed more than 30 days
  before are deleted. Whether WhatsApp could deliver such a blob again after
  its row is gone, which would download and apply it again, is not known.

## Trust roots

WaWarden cannot compensate for the following; a deployment must address them
outside the service.

1. **Whoever controls the deployment pipeline or the host controls the
   account.** Anyone who can change the image or configuration that is
   deployed, read the data directory, or restore its backups can impersonate
   the account without passing through any WaWarden control. Limit write access
   to the deployment pipeline and its configuration. Do not give hosts that run
   agents credentials that can change them.
2. **One host is one trust domain for tokens.** Processes running as the same
   user can read each other's tokens, and nothing in WaWarden can tell them
   apart. Do not place a token that reads every chat, or a write token, on a
   host where agents with shell access read untrusted input.
   [agents.md](agents.md#tokens) gives the token rules for agents.
3. **Other linked devices bypass the gateway.** Any other client linked to the
   same account has unrestricted access to it. The protocol library accepts
   history-sync and identity-mapping payloads from any of the account's own
   devices. WaWarden accepts history only from the primary device, and learns
   its own identity map only from server-asserted fields and the primary
   device's history. Unlink the companion devices that agents can reach once
   WaWarden replaces them.
4. **Anyone who can read process logs must not learn chat identities or
   content.** Treat everyone with access to standard output, the log pipeline
   or its storage as a reader of logs. That is why I-5 holds for every line the
   process writes, with the crash-output exception under
   [Dependencies](#dependencies).
5. **The maintainer's GitHub account controls releases.** It is the only
   approver of the `release` environment. A release built from malicious source
   on `main` passes every verification in [RELEASING.md](../RELEASING.md):
   provenance, signatures and reproducibility show where and how an artifact
   was built, not that its source is benign. Deploy by digest, and review the
   source diff between the version you run and the next one before upgrading.

## Reporting

Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).
