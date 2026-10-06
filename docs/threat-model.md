# WaWarden threat model

> **Status: draft for v1.0.** This document describes the security design that
> v1.0 will ship and marks which parts are in place. The latest release,
> `v0.1.0`, is milestone **M0**, a scaffold: a loopback health endpoint; a client
> listener that has no routes and refuses every request; and, when an admin
> token hash is configured, an admin listener that serves metrics to the admin
> token. `main` holds milestone **M1**, which is not released yet. Built from
> `main`, `serve` also opens and locks the message archive and the device store,
> `session.db`; runs the WhatsApp engine, which every release build links with
> the WhatsApp protocol library and always uses, through an adapter to that
> library (only a development build can run a scripted fake in its place, row
> 21); links the owner's account, and no other, when the administrator requests
> pairing through the admin listener, which also serves the engine's status and
> reconnection and which `wawarden admin` calls; writes operational events to
> standard output and, optionally, to a signed webhook; can write metrics on
> standard output in embedded metric format; and, when an age recipient is
> configured, writes one encrypted backup of both databases per paired device.
> A service without a paired device makes no connection to WhatsApp until
> pairing is requested. There is no client management, REST route or MCP route
> yet; they arrive in milestones M2 and M3.
>
> Every control carries a status. **M0** means it is in place in the M0 release.
> **M1** means it is in place on `main` and ships with the M1 release. **M2** or
> **M3** means it is planned for that milestone and does not exist yet. The
> residual-risks section describes the state at v1.0, and the controls it names
> carry their milestone too. [`configuration.md`](configuration.md) documents
> the behaviour of the current build in detail.

## System summary

WaWarden is one process that links to one personal WhatsApp account as a
companion device and exposes that account to the owner's own agents and
applications. Each client holds a bearer token that grants read access, write
access or both, limited to an allowlist of chats.

The process opens at most three listeners, all plain HTTP, and nothing else:

| Listener | Address | Serves | Status |
|---|---|---|---|
| Client | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | REST `/v1` and MCP `/mcp` (M2, M3). Until M2 there are no routes and no client tokens, so every request is refused. | M0 |
| Admin | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082`; opened only when an admin token hash is configured | Metrics (M0); status, pairing and reconnection (M1); client management (M2) | M0 |
| Health | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`; loopback addresses only | `/healthz`, without authentication | M0 |

Outbound traffic is limited to the WhatsApp websocket, the version fetch and
history downloads from WhatsApp's hosts (M1), and an optional
notification webhook (M1). Without a paired device, and until pairing
is requested, `serve` makes no connection to WhatsApp.

WaWarden does not restrict who can reach its listeners and does not terminate TLS.
Reaching the listeners from other hosts, encrypting that path and deciding who may
use it belong to the deployer: a VPN or mesh network forwarding to loopback, a
private subnet, or a reverse proxy that terminates TLS. The service must never face
the public internet.

Loopback is not an access boundary. Every process in the service's network
namespace can reach all three listeners: processes of other users on the same
host, and other containers in the same pod or task, including a forwarding
sidecar. Those processes must be trusted, or the tokens alone must be enough
protection against them.

## Assets

Most sensitive first.

| Asset | Why it matters | Present from |
|---|---|---|
| `session.db` | The linked device's keys and the protocol library's state: signal sessions, sender keys, message secrets, identity mappings and contact names. Whoever holds it can act as the account until the device is unlinked on the phone. | M1 |
| `archive.db` and history blobs | Every archived chat, in plaintext. `serve` creates and locks `archive.db` and runs the engine, which, once a device is paired, writes WhatsApp traffic into it and keeps each history blob under `history/` until it is processed or quarantined. Until a device is paired through the admin route that starts pairing, the engine makes no connection and writes no WhatsApp traffic. | M1 |
| Admin credential | Gates the admin listener: metrics (M0), the engine's status, pairing and reconnection (M1), and minting clients (M2). It does not expire. The service holds only its SHA-256. | M0 |
| Client tokens | Bearer credentials: whoever holds one and can reach the client listener has its access. The service stores only their hashes. | M2 |
| Master key (`keys/master` in the data directory) | The key of the log pseudonyms is derived from it (M1), and so will be the cursor-sealing and audit keys (M2); a chat-reference key is derived but not used, since no notification event names a chat. Whoever holds it can test a guessed identifier against the pseudonyms in the logs. | M1 |
| Webhook signing secret (`WAWARDEN_NOTIFY_SECRET_FILE`) | Shared with the receiver of notification events. Whoever holds it can forge events that the receiver accepts. The service reads it once, at start, and never writes it out. | M1 |
| Outbound integrity | Messages sent through WaWarden appear as sent by the account owner. | M3 |
| Account standing | WhatsApp can restrict or ban an account that links an unofficial client. | M1 |
| Backups (`backups/` in the data directory) | One age-encrypted file per backup holds copies of `archive.db` and `session.db`, so whoever holds a backup and the age identity it was encrypted to holds both databases, the linked device's keys included, and can act as the account like a holder of `session.db`. The service holds only the recipient, a public key, and cannot read a backup. While a backup runs, `backups/tmp/` holds a plaintext copy of one database at a time. | M1 |
| Release artifacts | The binaries and images deployers run. | M0 |

## Actors

### In scope

- **Any WhatsApp peer or group member.** Sends floods, crafted protocol messages,
  edits, revokes, reactions and poll updates aimed at other chats, and text written
  to manipulate the agents that will read it (prompt injection).
- **A malicious or prompt-injected client with a valid scoped token.** Tries to
  read or write outside its scope, to learn whether out-of-scope chats or messages
  exist, or to exhaust shared send budgets.
- **Anyone with network reach to a listener but no token**, including other
  processes in the service's network namespace and web pages open in a browser on
  a host that can reach a listener.
- **A thief of one token.** The token is contained by its chat scope, expiry,
  revocation and rate limits, not by where it is used from.
- **Readers of logs**: anyone who can read the process's standard output or the
  log pipeline it feeds.
- **Readers of backups.**
- **A compromised dependency or a malicious pull request.**
- **A well-meaning contributor** who adds a route, a goroutine, a listener or a
  query without knowing the rules. The construction checks below exist for this
  actor.
- **An operator running defaults.** Defaults must be safe without tuning.

### Out of scope

These are stated so that nobody relies on WaWarden for them:

- **Network access control and transport encryption.** The service listens on
  loopback by default and speaks plain HTTP. Who can reach it, and whether the path
  is encrypted, is the deployer's decision.
- Root or same-user-ID access on the host that runs WaWarden.
- Processes running as the same user on a client host (they can read each other's
  tokens).
- A compromised phone, or a compromised other device linked to the same account.
- Whoever holds both a backup and the age identity it was encrypted to.
  Generating the identity away from the service's host, and keeping it there,
  is the operator's task; there is no unencrypted backup.
- Delivery receipts and the other traffic the protocol library sends by itself
  (see residual risks).
- Deletion residuals in backups, in filesystem blocks the service cannot scrub,
  in full-text index page keys shorter than a trigram, in other stored text
  (names, identifiers, messages waiting in the inbox) that holds the same text,
  and in messages that history sync stores after their revocation or edit
  arrived.
- Agents that pass message text into tools. WaWarden marks the text as untrusted;
  what an agent does with it is the agent's responsibility.

## Trust boundaries

1. **Network to client listener (M0).** Before anything else, a request carrying
   an `Origin` or `Sec-Fetch-Site` header is refused with `403`, so a web page
   cannot use a browser as a client, and an `OPTIONS` request is refused with
   `405`. Every other request is authenticated before any route handler runs and
   before any byte of its body is read; a refused request that announces a body
   gets its answer at once and the connection is closed. Until M2 no client token
   exists, so every request that reaches authentication is answered `401`. Failed
   authentications are counted and throttled (see
   [Failed authentication](#failed-authentication)). From M2, a request carries a
   client token, which is looked up by its identifier, compared in constant time
   against the stored hash, and checked for revocation and expiry on every REST
   request and every MCP call. The peer address will be recorded for forensics
   only (M2) and never used for a decision. The service reads no forwarding
   headers.
2. **Network to admin listener (M0).** A separate listener, so that the deployer
   can restrict it independently of the client listener. The same browser and
   `OPTIONS` refusals apply. The only check inside the service is the admin token:
   it must have the exact form that `wawarden admin init` generates, and its
   SHA-256 must match the configured hash, compared in constant time. Without a
   configured hash the listener is not opened at all. Failed authentications are
   counted in `wawarden_admin_auth_failures_total` and throttled (M0), and
   reported in an `admin_auth_failure` event at most once a minute, with a count
   and nothing about the requests (M1). Behind the token, the admin
   routes (M1) answer fixed-shape data: the status carries the
   engine's state and reason, counts, a time and the build version, never an
   identifier; pairing returns its code to the caller and nowhere else; every
   call of a route that changes state reports an `admin_mutation` event with its
   action and outcome. `internal/api` reaches the engine and the archive only
   through interfaces that `internal/app` implements. What the token must
   satisfy, and the controls that detect its misuse, are under
   [The admin token](#the-admin-token).
3. **Loopback health (M0).** The health listener has no authentication, so the
   service refuses to start when its address is not loopback, and checks again
   before opening it. It answers only `GET /healthz`, with whether the process is
   up and serving (M0) and its databases (the archive and the device store)
   open and locked by the service (M1).
4. **WhatsApp to engine (M1).** Everything that arrives from WhatsApp is untrusted,
   including payloads attributed to the account's own other devices, except where
   the server asserts a field. An edit, revocation, reaction or poll vote is
   applied only to a message found in the chat it arrived in, by identifier and
   sender; an edit or revocation only from the message's sender, or, for a
   revocation in a group, from a member the archive records as an admin; an
   edit only when it is newer than the last edit applied to that message. A chat
   named in a message key or a reply reference is never followed unless it is
   the event's own chat (or, in a direct chat, the owner's number, which is how
   the other side names it). LID-to-number mappings are learned only from
   server-asserted alternates of live messages and from history sync. Messages,
   group changes and history-sync notifications that arrive while the engine is
   in its `unpaired` state, which it also enters when pairing rejects an
   account, are dropped before they are written.
   History-sync notifications are accepted only from the owner's primary device
   (device 0); blobs are downloaded through a size cap, persisted before the
   receipt and before parsing, decompressed under the same cap, and quarantined
   after three failed attempts, an attempt that panics included, so a poison
   payload is set aside rather than replayed forever; inbox rows are quarantined
   the same way. A blob's plaintext
   file is deleted once the blob is processed or quarantined, and each start of
   the engine deletes any such file that a crash left behind. Live traffic waits
   in a durable inbox and is acknowledged only once written; a message the
   engine refuses (a full inbox, a paused ingest, a failed write) is left
   unacknowledged, which loses it, because the protocol library has already
   advanced its keys and cannot decrypt WhatsApp's second delivery (see
   residual risks). A message whose identifiers, push name, text and quoted text
   add up to more than 512 KiB is dropped before it is written, which bounds the
   memory one message takes. The protocol adapter translates the library's
   events into plain data: it reads a protocol message as a revocation or an
   edit only when its type is set, because the type's zero value means revoke;
   it passes identifiers with their device parts for the engine to strip; it
   reads history rows under their conversation's chat, never under a row's own
   key, keeps an edit row's identifier there and in the messages the owner's
   phone sends again on request, and decodes blobs with a protobuf
   recursion limit; and before the library saves a pairing it refuses an account
   other than `WAWARDEN_OWNER_PHONE`, so nothing of a foreign account is stored.
   QR codes, which carry the device's pairing secret, are dropped where they
   arrive. These rules are tested with plain-data events in the engine core, and
   the adapter with a protobuf fixture for each case the library's research
   listed, round-tripped through the wire format, fed through a real engine and
   archive, and fuzzed.
5. **Service to clients (M2).** Message text, contact names and group subjects are
   written by third parties. Every message object carries `untrusted: true` and its
   `origin`, and a `text_display` field with control, bidirectional-override,
   zero-width and tag characters removed. The function that removes them is in
   place (M1): it keeps newline and tab, also removes every other format
   character, and replaces invalid UTF-8. The archive stores the field beside a
   message's text when it writes the message and when it applies an edit (M1);
   no route returns either before M2. MCP tool names, descriptions and schemas are
   compile-time constants, so no chat-controlled string reaches a tool definition.
6. **Service to logs, metrics and notifications.** Chat identities leave the
   process only as keyed HMACs, and message text, query text, tokens and protocol
   messages never leave it. As of M0: no request is logged; failed authentications
   are counted, not logged; a startup refusal names the variable involved, never
   its value; recovered panics are logged with their type and stack, never their
   value; and the metrics carry only build information, failure counts and panic
   counts. As of M1: every line `serve` writes to standard output passes
   through one writer that replaces each WhatsApp identifier in `user@server` form
   whose server name no ASCII letter or digit follows directly with a pseudonym
   keyed by the master key and drops every line carrying XML in one of the
   recognised shapes, and a log value that is not a string, number, boolean,
   time, error or value with a `String` method is written as its length when it
   is a byte slice or a pointer to one and as its type name otherwise, as is a
   protocol message or a URL whatever its methods, never its content (see
   [Logging](configuration.md#pseudonyms-and-dropped-lines)). Identifiers in other
   forms, such as a phone number on its own, pass unchanged, so code must not log
   them. The engine core's log events and metrics carry states, counts, fixed
   reason codes and the Go types of errors, never an identifier, a name, message
   text or a pairing code, which a test checks with canary content (M1). The
   protocol library's logger feeds that writer through an adapter
   that also masks runs of six or more digits outside the `user@server` form,
   cuts each line at a word boundary after 1 KiB, and discards debug output
   outside a `WAWARDEN_UNSAFE_DEBUG` window (M1); the MCP library's
   logger will feed it too (M2). Operational events go through one notifier
   (M1), which writes each as a line of that writer with a fixed set
   of fields: numbers and fixed codes, where a value that is not a lower-case
   code, or for an error type a Go type name, is written as `invalid`, so no
   event carries an identifier, a name, text, a token or a pairing code even when
   a caller passes one. Metrics in embedded metric format (M1) are
   written through the same writer and carry counts and the panic names that
   `wawarden_panics_total` already exposes.
7. **Service to data directory and backups (M0, M1).** The data directory must be
   owned by the service's user with mode `0700`, and the service creates it that
   way (M0). The service creates `keys/` and the master key in it, and refuses to
   start unless they are a directory with mode exactly `0700` and a regular file
   of 32 bytes with no permission for group or others, both owned by the service's
   user and neither a symbolic link (M1). `archive.db` and its
   journal must be regular files owned by the service's user with no permission
   for group or others, and `history/` and `backups/`, when present, directories
   with mode exactly `0700` owned by that user (M1); `session.db` and
   its journal are checked the same way and opened with the archive's settings
   and lock (M1). Backups are encrypted
   to an operator-supplied age recipient, so the service can write them but not
   read them, and before each backup `backups/` and its staging directory
   `backups/tmp/` must be directories, not symbolic links, of mode exactly
   `0700` owned by the service's user (M1).
8. **Source to release artifacts (M0).** Releases are built only from `main` by a
   manually dispatched workflow that waits for the maintainer's approval, rebuilt
   independently and compared bit for bit, attested and signed. The approval gate
   and the immutability of published releases come from repository settings that
   are in place: a protected `release` environment, immutable releases, and a tag
   ruleset. The workflow checks the environment before building and fails after
   publishing a release that is not immutable; it cannot check the tag ruleset.
   See [`RELEASING.md`](../RELEASING.md).
9. **Service to the webhook receiver (M1).** When
   `WAWARDEN_NOTIFY_URL` is set, each notification event is posted to it with
   the same fields as its line, an event id and a time, signed with HMAC-SHA256
   over the id, a timestamp and the body with an operator-supplied secret, so a
   receiver can refuse forged, altered and replayed deliveries. The URL must be
   `https` without credentials; the client takes no proxy, follows no redirect,
   uses TLS 1.2 or later and timeouts, and reads at most 4 KiB of an answer,
   which it never logs. Its dialer checks every resolved address and always
   refuses link-local, metadata, multicast, reserved and unspecified addresses,
   every IPv6 address with a zone and the local-use NAT64 prefix, checks an
   address in the well-known NAT64 prefix as the IPv4 address it embeds,
   and refuses loopback, private and shared addresses unless the operator allows them,
   so a misconfigured URL or a name that resolves inward cannot make the
   service post to the cloud metadata endpoint. The configuration is the
   operator's: the check guards against mistakes, not against whoever can set
   the service's environment. A bounded queue and one sender keep a slow or
   failing receiver from holding up the engine, the admin routes or shutdown;
   events it cannot deliver are counted and dropped, and those dropped when the
   shutdown's grace period runs out are also reported in one line and make
   `serve` exit `1`.
10. **Admin CLI to the admin listener (M1).** `wawarden admin
    status`, `pair` and `reconnect` take the token from a file, standard input
    or a command, never from an argument or the environment, check its form
    before sending it, take no proxy, follow no redirect, warn when plain HTTP
    leaves loopback, cap what they read, and pass everything they print from an
    answer through the terminal sanitiser. A flag they cannot parse is named,
    never repeated as typed, so a token typed as a flag's value does not reach
    standard error. The token command runs without a
    shell, with its standard input and error closed, its output capped, a
    timeout, its own process group, killed after it finishes, and no `WAWARDEN_`
    variable in its environment.

## Bearer tokens without network binding

A token is a pure bearer credential. It is not bound to a host, a network address
or a network identity, and the service contains no network gate. The accepted
trade-offs are:

- **A stolen token works from anywhere with network reach to the service.**
- **A mistake in the deployer's network setup is not caught by the service.** The
  service warns at start only for a client or admin listener bound to a
  non-loopback address. When a forwarder, sidecar or reverse proxy in front of
  loopback listeners exposes them too widely, the service binds loopback and logs
  nothing. Plain HTTP exposes bearer tokens on any network segment the deployer
  leaves unencrypted.
- **The admin surface is guarded by its token alone.**

What limits a client token instead (M2 unless noted): its chat scope; an expiry
that is mandatory (90 days by default, one year at most; tokens that never expire
are refused); revocation, checked on every request; per-client rate limits; and,
for write tokens, the send budget and the first-contact rule (M3). Failed
authentication is counted and throttled from M0.

### Failed authentication

The client and admin listeners each count failed authentications
(`wawarden_auth_failures_total`, `wawarden_admin_auth_failures_total`; M0) and
each have one failure budget shared by all callers: 30 failures, refilled at one
per second. While it is empty, failures are answered `429` instead of `401` (M0).

This throttle is not a defence against guessing. Authentication still runs for
every request and a valid credential is always accepted, so legitimate callers
cannot be locked out, and a correct guess succeeds whether or not the budget is
empty. Anyone who can reach a listener can also keep its budget empty, after which
callers with a wrong token see `429` instead of `401`. Guessing is defeated by the
entropy of the tokens; the counters exist to alert on.

### The admin token

- **It is generated, and only generated tokens are accepted (M0).**
  `wawarden admin init` prints a token made of `wwadm_`, 32 bytes from the
  operating system's cryptographic random source in base64url, and a CRC-32
  checksum that lets secret scanners recognise it. The admin listener refuses any
  presented string that does not have exactly this form, even when its SHA-256
  matches the configured hash, so a chosen passphrase cannot serve as the admin
  token. The service stores and compares an unsalted SHA-256. That hash travels in
  configuration that many people and systems can read (container or task
  definitions, `docker inspect` output, CI logs); with 256 random bits behind it,
  recovering the token from the hash is infeasible.
- **It does not expire.** Rotate it by generating a new token, replacing the
  configured hash and restarting the process; the hash is read only at start.
- **What it guards.** Metrics (M0); the status of the engine and the archive's
  counts, pairing, and reconnection (M1); client management (M2).
  Pairing can link only the number in `WAWARDEN_OWNER_PHONE`, is refused while
  a device is paired, and is limited to three attempts an hour; reconnection is
  not limited beyond the token.
- **Detecting its misuse.** Failed admin authentications are counted in
  `wawarden_admin_auth_failures_total` (M0) and reported in the
  `admin_auth_failure` event and the `AdminAuthFailures` metric in embedded
  metric format, which need no token (M1). Every call of a route that
  changes state reports an `admin_mutation` event (M1). Both reach the
  webhook when one is configured. Planned: a hash-chained audit table of admin
  and client actions (M2).

## Security invariants

These properties must hold in every release. The next section lists the
mechanisms that enforce them.

- **I-0 Authentication first.** On the client and admin listeners, no route
  handler runs and no request body is read until the request presents a valid,
  unexpired, unrevoked credential.
- **I-1 Read scope.** A client receives data only from chats in its read scope.
- **I-2 Write scope.** A client sends only to chats in its write scope. A client
  that may read every chat cannot write at all.
- **I-3 Resolution in the chat.** Every entity a request refers to (a `reply_to`
  reference; later media, reaction and edit targets) and every protocol message
  from WhatsApp (edit, revoke, reaction, poll update) is resolved inside the chat it
  belongs to, never by identifier alone.
- **I-4 Denied equals missing.** A request for something outside the client's
  scope and a request for something that does not exist receive byte-identical
  responses.
- **I-5 Nothing identifying leaves the process.** No chat identity in plaintext and
  no message content, query text, token or protocol message reaches logs, metrics,
  notifications or MCP tool definitions.
- **I-6 Fixed network surface.** The process listens only on the client listener,
  the admin listener (when configured) and the loopback health listener.
- **I-7 No plaintext credentials in the service.** The admin credential is
  configured as a hash; client tokens are stored as hashes.
- **I-8 Deletions are honoured on disk.** Once the write that revokes, edits or
  expires a message has returned without error, the message's row no longer
  holds its old text; the full-text index, page keys included, keeps only those
  of its trigrams (the three-character runs the index stores) that a remaining
  message holds; and neither that text nor any of its trigrams is in the
  database or its journal unless the archive still holds the same text or
  trigram elsewhere: in a remaining message, in a message waiting in the inbox,
  in a stored name (a push name, chat name or group subject), in an identifier
  or another stored value, or in the schema. Backups, a
  page key that holds less than a whole trigram, a trigram left by a stop or
  a failure between the deletion and the index rewrite it requires, and a
  revocation or edit that arrives before history sync has stored its target are
  the exceptions (see residual risks).
- **I-9 Panics are contained.** A panic in a goroutine that WaWarden starts, in
  one of its HTTP handlers, or in the protocol adapter's event handler, which
  runs on the protocol library's goroutines, is recovered, counted and logged
  without its value. Other code on goroutines that dependencies start is not
  covered (see residual risks).

## Secure by construction

Each mechanism makes a class of mistake fail to compile or fail a test, rather than
relying on review. The architecture tests live in `internal/archtest`: they parse
every Go file of the module with the standard library's `go/parser`, and they build
fixture packages, and fixture files added to package policy with a `go build`
overlay, that must fail to compile with a specific error. Because they read only Go,
the walk that feeds them fails, and with it the test that runs every rule, when the
module holds something the go tool can build in without their reading it: an
assembly, C, C++, Objective-C, Fortran, SWIG or system object file, a symbolic link,
or a nested module.

| # | Mechanism | Protects | How it is enforced | Status |
|---|---|---|---|---|
| 1 | **Grant types.** `ReadGrant`, `WriteGrant` and `AdminGrant` are declared, with only unexported fields, in `internal/policy/internal/seal`, and package policy exports them under the same names. Outside the seal package, a valid grant can only come from the decision functions in `internal/policy/decide.go`. Decisions deny by default: a missing, revoked, expired or never-expiring client gets no grant; a client without write chats, or one that may read every chat, gets no write grant; the zero value of every grant allows nothing. Grants copy the client's chat sets, so changing a client afterwards does not change a grant. | I-1, I-2, I-7 | Compile time: the fields belong to the seal package, so the compiler refuses, in every other package and in package policy itself, a grant literal with fields, a conversion from a look-alike type, a field write and a field's address; fixtures prove it for a literal and a field write outside `internal/policy`, and for the forms row 4 lists inside package policy. The seal-constructors architecture test (row 4) lets each grant constructor be called only in its decision function, and the rules row 4 names with its guarantee keep code from going around the type system. Unit tests for every decision, a reflection test that the grant types have no exported fields, and tests that zero-value grants, and grants built in the seal package without their validity flag, allow nothing. | M0 for the grants and decisions. M1 for the seal package. M2 for the scoped store, `internal/store/scoped`: an architecture test refuses an exported method of one of its exported types whose first parameter is not a `policy.ReadGrant` value, except `Query.Expression`, which reads nothing; its constructor takes the database handle, which no package outside `internal/store` can name. |
| 2 | **Registration helpers.** Client and admin routes are registered only through the unexported helpers `read`, `write` and `admin` in `internal/api`. Each one obtains a grant from the matching decision before its handler runs and answers the same `404` as an unknown route when the decision denies. The mux is unexported and only `internal/api/register.go` may create one or register on it, so a route without a policy class cannot be added from outside. Authentication runs before the mux. The health listener serves only `/healthz`. MCP tools will use the same pattern (`ReadTool`, `WriteTool`). | I-0, I-1, I-2, I-4 | Compile time (fixtures that reach the router from another package fail to compile); an architecture test on mux ownership; a coverage test that compares every registered route and its class with a golden list; pipeline tests proving that authentication precedes routing and that no body is read before it, form-encoded and multipart bodies included; an architecture test that refuses, in every non-test file, any reference in Go source to a method named like one of the `net/http` form-parsing methods (`ParseForm`, `ParseMultipartForm`, `FormValue`, `PostFormValue`, `FormFile`, `MultipartReader`) and any string literal, or concatenation of literals, in which one of those names appears as a word. A name assembled from named constants or at run time, or written outside Go source, such as in a template file embedded with `go:embed`, passes that test; `internal/api` may import neither `text/template` nor `html/template`. Only `internal/api` serves requests, and its API reads JSON only. | M0 for the HTTP helpers and the coverage test. M2 for the MCP helpers. |
| 3 | **Import fences.** `internal/api` may not import `database/sql`, `text/template`, `html/template`, the SQLite driver, any package under `internal/store` other than `internal/store/scoped` itself, whose every read takes a grant (row 1), any package under `internal/engine` or the WhatsApp protocol library; it reaches the engine only through interfaces that `internal/app` wires. Outside test files, only `cmd/wawarden/tokencommand.go`, the admin CLI's token command, may import `os/exec` or name `os.StartProcess`, `syscall.Exec`, `syscall.ForkExec` or `syscall.StartProcess`. Outside test files, only packages under `internal/store` may import `database/sql`, and only `internal/store/internal/db` the SQLite driver. No non-test file may import `mime/multipart`. `internal/policy` and its seal package may import only an allow-list of standard-library packages, which excludes networking, operating-system and filesystem packages; package policy may also import the seal package, and no other package may: Go's `internal` rule refuses it outside `internal/policy`, which a fixture proves, and the seal-constructors architecture test (row 4) refuses it in every file, test files included, except the files of package policy and those in the seal package's directory, so an external test package `policy_test` may not import it either. `net/http/pprof`, `expvar`, `net/http/cgi`, `net/http/fcgi`, `plugin`, `unsafe` and cgo are banned everywhere. Outside test files, only `internal/api/dto` and `internal/logx` may import `reflect`, and no file, test files included, may use a selector named `NewAt`, `SetPointer`, `UnsafeAddr` or `UnsafePointer`, the `reflect` functions and methods that read or write memory through an unsafe pointer without importing `unsafe`. A module outside the standard library may be imported only when it is on a reviewed list of importable modules, and `go.mod` may require only those modules and, marked `// indirect`, the modules on a second reviewed list of indirect-only requirements. The first list holds the SQLite driver, `modernc.org/sqlite`, the WhatsApp protocol library, `go.mau.fi/whatsmeow`, the signal library it uses, `go.mau.fi/libsignal`, protobuf, `google.golang.org/protobuf`, the backup encryption library, `filippo.io/age`, and the property-test library `pgregory.net/rapid`, which only test files may import, so it is never linked into the binary; the second, the 24 other modules that the first five require, and an architecture test requires `modernc.org/libc` at exactly the version the driver's own `go.mod` names, as the driver's documentation demands. A `//nolint` comment may not silence `depguard` or `forbidigo`. The non-test files of `internal/keys`, `internal/logx` and `internal/sanitize` may import only the standard library. | I-1, I-5, I-6 | `depguard` in `golangci-lint`, and the architecture tests; the banned-import and third-party checks, and the reflection test's selector check, cover test files too; the fence's self-test accepts `internal/store/scoped` and refuses its subpackages, `internal/store/ingest`, `internal/store/admin`, `internal/store/session` and `internal/store/internal/db`. | M0. M1 for the seal package, the two module lists, the standard-library-only packages, the store and engine fences and the confinement of `os/exec`. M2 for the scoped-store exception and the test-only module. |
| 3a | **One database handle.** The raw `*sql.DB` lives only in `internal/store/internal/db`, the only importer of the SQLite driver, which other packages reach through its `Read` and `Write` methods: each runs under a deadline inside a transaction. That package alone names a database file, opens, inspects or removes one, and runs `PRAGMA`, `VACUUM`, `ATTACH` or `DETACH`, so no other code of this module can change a verified setting, open another database through SQL, or close a descriptor of the database file and so drop its lock. Everywhere else, the SQL text passed to `Exec`, `Query`, `QueryRow` or `Prepare` and their `Context` forms, and each schema step passed to that package's `Migrate` in a slice literal at the call, is a string literal or a constant declared once in the same file, or a concatenation of those; no SQL is built from values. In `internal/store/scoped`, the read queries call `QueryContext` and `QueryRowContext` of a scoped querier, and the row collector's `QueryContext`, which the same rule holds to constants of the calling file; only `inbuilder.go` turns a query's scope markers into SQL (row 5). | I-1, I-3, I-8 | Go's `internal` package rule (toolchain); the confined-imports, database-files, sqlite-statements and constant-sql architecture tests on every non-test file outside that package, each with violating and conforming self-tests (a database file name assembled at run time from parts that are not literals passes the database-files test; the sqlite-statements test reads every string literal and concatenation of literals, and also the whole text of each such SQL argument and schema step with the file's constants resolved, so a statement split across constants is refused; the constant-sql test accepts only literals and constants declared once in the calling file, refuses schema steps that are not such a slice literal, and refuses those methods and `Migrate` used as values); `depguard`. The tests catch mistakes, not a deliberate edit of that package or of the tests themselves, which review covers. The settings of the connection are row 15. | M1 |
| 4 | **Canonical chat identifiers.** `CanonicalChat` is `seal.Chat`, declared in `internal/policy/internal/seal` with unexported fields and a validity flag, and its zero value is invalid. Outside the seal package, a valid chat can only come from `Normalize`, in `internal/policy/normalize.go`. It accepts the servers `s.whatsapp.net`, `lid` and `g.us`, and `c.us`, which it maps to `s.whatsapp.net`, in lower case only; every other server is rejected, `broadcast`, `newsletter`, `hosted`, `hosted.lid`, `bot`, `msgr` and `interop` included. Before the `@` of a phone-number or LID user it accepts `USER`, `USER:DEVICE` or `USER.AGENT:DEVICE`, where `USER` is 1 to 24 digits, `AGENT` 1 to 3 digits of value at most 255 and `DEVICE` 1 to 5 digits of value at most 65535; it drops the agent and the device, and rejects an out-of-range value instead of wrapping it. Before the `@` of a group it accepts 1 to 24 digits, or two such runs joined by `-`. Digits are ASCII only. Any other byte, the empty string and an input longer than 128 bytes are rejected, and a rejection returns the zero value. A rejected input allocates nothing; an accepted one allocates only to build and intern its result. The result is the user or group followed by `@` and its server, and normalising it again returns the same value. A chat holds its identifier as a `unique.Handle[string]`, so `fmt` and the text handler of `log/slog`, which print unexported fields, print a pointer in its place, and `encoding/json` and the JSON handler of `log/slog` print `{}`. It exposes the identifier only through `JID()`, its kind (phone-number user, LID user or group), which `Normalize` records when it builds the chat, through `Kind()` and its validity through `Valid()`, and declares no `String`, `GoString`, `Format`, `MarshalText`, `MarshalJSON`, `MarshalBinary`, `LogValue` or `Error` method. Decisions drop the zero value from every scope, and grants never allow it. | I-1, I-2 | Compile time: the fields belong to the seal package, so the compiler refuses, in every other package and in package policy itself, a literal with fields in any syntactic form, a conversion from a look-alike type, a field write and a field's address. Fixtures added to package policy prove it for an explicit chat literal, an elided chat literal inside a named slice type, a grant literal inside a generic container, a grant built through a type parameter, a look-alike struct converted to a chat, a look-alike pointer written over a grant, a field write to a grant and to a chat's identifier, and a grant field's address, and a positive control proves the fixtures are compiled into the package. The seal-constructors architecture test, which reads every file outside the seal package's directory, test files included, accepts a reference to a function the seal package exports only as a direct call in the body of its one permitted function, outside any function literal: `NewChat` in `Normalize`; `NewReadGrant`, `NewWriteGrant` and `NewAdminGrant` in `DecideRead`, `DecideWrite` and `DecideAdmin`; `NewAdminCredential` in `ParseAdminCredential`; and the constant-time digest comparison `MatchAdminCredential` in `DecideAdmin`. It refuses renamed, dot and blank imports of the seal package, and another test requires its table to name exactly the functions and variables that the seal package exports, and a reviewed list to name exactly its exported methods whose results mention a sealed type (only `ReadGrant.Chats`, which returns copies of the grant's chats), so a method that could mint a sealed value fails the test until it is reviewed. The guarantee is therefore: outside `internal/policy/internal/seal`, a valid chat can only come from `Normalize` and a grant only from a decision function, enforced by the compiler plus one constructor-reference rule, together with the rules that keep code from going around the type system: the bans on `unsafe`, which `go:linkname` needs to call a constructor by its symbol name, and on cgo, the reflection rule's ban on the unsafe-pointer functions and methods of `reflect` (row 3), and the walk's refusal of assembly, C and object files (above). Weakening any of these rules weakens the guarantee. Inside the seal package anything is possible: its code can build any value, so it holds only the types, their accessors, one constructor per type and the digest comparison, and review, not a test, keeps it that way. An architecture test refuses those methods on `seal.Chat`, and any alias of `seal.Chat` declared in the seal package, in every file of the seal package, test files included; the compiler refuses methods declared on `CanonicalChat` or any other alias outside it. A test checks that `fmt` with the verbs `%v`, `%+v`, `%#v`, `%s`, `%q`, `%x`, `%X` and `%d`, both `log/slog` handlers and `encoding/json` never print a valid chat's identifier. A table test of accepted and rejected identifiers; tests that every accepted result is canonical and normalises to itself, and of the allocations; a fuzz target (row 8). A test asserts that the zero value is denied for every client shape. | M0 for decisions and grants. M1 for `Normalize` and the seal package. M2 for the SQL builder, which turns the zero grant into `AND 0` and leaves an invalid chat out of a set (tests and a property over random grants, row 5). |
| 5 | **One `IN` builder.** All chat-set SQL goes through `internal/store/scoped/inbuilder.go`. Each read query carries scope markers that only the builder expands: an invalid grant or an empty set yields `AND 0`, an all-chats grant `AND 1`, and a set `AND <column> IN (?n, …)` with one numbered parameter per chat, bound to the grant's sorted, distinct canonical identifiers, so the SQL text never holds an identifier. The column comes from the constant query text, and the same parameters serve every marker of a query, so the sender-name join is restricted by the same set. A set of more than 1,000 chats is an error, never a truncation. The builder refuses a query without a marker, and an unexpanded marker is not valid SQL. | I-1 | An architecture test rejects an `IN (` string literal, including one assembled by concatenation, and an SQL argument whose text holds one once the file's constants are resolved, in every file except the builder's. Tests of the three cases and of the cap; a property over random grants and a fuzz target over the SQL text (every chat bound, in order, none written into the text); a test proves that `AND 0` ends a query before its first row, against a control whose rows would make SQLite fail. | M2 (the rule in force from M0). |
| 5a | **Uniform resolution.** A chat reference, the chat filter of a search or of the change feed, and a message's chat are resolved by one query that carries the scope clause, never by a lookup followed by a check: a chat outside the grant, a missing chat and a malformed reference all return "not found", with the same SQL text and the same empty result. A message named by its chat, identifier and sender is canonicalised through the identity mappings and refused, before any message query runs, when the grant does not allow its chat; the message query carries the scope clause as well. | I-1, I-4 | Tests that an out-of-scope chat, a missing one and garbage issue identical SQL and return identical results; a property over random archives and clients that repeats the comparison for an out-of-scope chat and a missing one (row 7); a test that an out-of-scope message issues no message query. | M2 |
| 5b | **Bounded reads.** Every read query has a `LIMIT` of at most 201, works on index ranges, counts nothing, and finishes within the read deadline: search walks the full-text index in windows of 20,000 sequence numbers and the change feed in windows of 20,000 change numbers, at most five windows a call, with a cancellation check between them. At most 8 reads run at once; a read that finds every slot taken, or that passes its 2-second deadline, fails with a "busy" error at once instead of queueing behind the engine's writes. Nothing runs `ANALYZE` or `PRAGMA optimize`, so the planner keeps choosing by the schema alone: statistics can turn a cross-chat search of index entries into a scan of every message followed by a sort. | Availability of reads and of ingest, which share the archive's one connection | `EXPLAIN QUERY PLAN` golden files for every read query with an all-chats, an empty, a one-chat and a three-chat grant; the test refuses a scan of a table and a temporary b-tree other than the chat list's sort of at most 1,000 granted chats and the change feed's sort of one window, and a source test requires every `SELECT` constant of the package to have a golden plan. An architecture test refuses `ANALYZE` and `PRAGMA optimize` in every non-test file. Tests of the read slots and of a read past its deadline. The plans are those of the SQLite version the driver pins; a driver upgrade that changes a plan fails the golden test until it is reviewed. | M2 |
| 6 | **DTO firewall.** Response bodies are types of `internal/api/dto`, built field by field. Every field of a database row struct carries `json:"-"`. | I-5 | Compile time (the response interface has an unexported method, so a type declared elsewhere cannot be returned; a fixture proves it) and a run-time check in the encoder; a test that fails if a response type can produce the keys `raw`, `media_meta`, `sender_alt`, `seq` or `token`; a reflection test over the rows, pages and positions of `internal/store/scoped` (every exported field, nested ones included, carries `json:"-"`, and each type encodes to `{}`), which a source test keeps complete by requiring it to list every exported struct of the package. Rows carry no `raw`, `sender_alt`, `media_meta` or `origin` field. | M0 for response types and the key test. M2 for the row structs. |
| 7 | **Property tests.** Random archives built with the real store (phone-number and LID pairs, messages in chats and groups, edits, revokes, re-keys, chats without messages) crossed with random clients (all chats, a set, revoked), run through every read of the scoped store and compared with a pure model over an unscoped copy of the archive: every read returns exactly the rows in scope, in order, across pages of random sizes in both directions; a revoked or edited canary never appears in search, the change feed or a read row; an out-of-scope chat and a missing chat return the same result through the same SQL; a re-key never lets a client see a message, stored before it, that the client could not see; and the `IN` builder's three cases. A tally fails the test when 100 checks do not cover re-keys that move and that are refused for a scope, revokes, edits and partly visible archives. | I-1, I-3, I-4, I-8 | `pgregory.net/rapid` in CI: 100 checks with the race detector in every test run, 1,000 without it in a separate job, which uploads the failure files. | M2 for the scoped store; the HTTP and MCP layers join them when they are built. |
| 8 | **Fuzzing.** The admin token syntax check (never panics; anything accepted has the generated form). `Normalize` (never panics; accepts exactly what a regular-expression model of the forms in row 4 accepts, with the agent and device ranges applied, and returns the identifier the model derives; a rejection returns the zero value; an accepted input is at most 128 bytes, and its result has exactly one `@`, no `:` or `.` before it, one of the servers `s.whatsapp.net`, `lid` and `g.us` with the matching kind, and normalises to itself). The display and terminal sanitisers (output is valid UTF-8, holds none of the characters each one removes or replaces, and sanitising it again changes nothing; the terminal sanitiser keeps one character for each character or invalid byte of its input; text with nothing to change is returned unchanged). Strict JSON decoding of request bodies (refusals are only the three fixed errors; an accepted body is valid UTF-8 without a byte-order mark and at most 16384 bytes, one JSON object at most 8 levels deep whose keys are all lower-case `snake_case`, and what it decodes to is accepted again once re-encoded). Crafted protocol messages: up to 64 KiB decoded with a protobuf recursion limit of 24 and translated for three kinds of sender (never panics; a revocation or an edit only from a protocol message with that type set; identifiers, kinds and lengths within what the input allows). `Normalize` against the protocol library's identifier parser (every chat `Normalize` accepts is read by the library as the same user and server, and its result carries no device or agent). The full-text query builder (each term becomes one double-quoted phrase with its quotes doubled, so no operator or column filter reaches FTS5, the search provokes no SQLite error, and for ASCII queries it returns exactly the rows that hold every term); the `IN` builder's SQL text (row 5). Cursor opening; idempotency keys. | I-1, I-3, I-5, I-7, I-9 | Go native fuzzing; seed corpora replay in every test run, and CI fuzzes every target for 60 seconds. | M0 for the admin token check. M1 for `Normalize`, the sanitisers, JSON decoding, crafted protocol messages and the comparison with the library's parser. M2 for the others; M3 for idempotency keys. |
| 9 | **`safego.Go` for every goroutine WaWarden starts.** It recovers a panic, logs the panic's type and stack (never its value) and counts it per goroutine name. WaWarden's HTTP handlers recover the same way and answer a fixed `500`; the protocol adapter's event handler recovers the same way under the name `wa.event` and refuses the event, so WhatsApp is not acknowledged. | I-5, I-9 | An architecture test rejects, in non-test files outside `internal/safego`, a `go` statement, `time.AfterFunc`, `context.AfterFunc`, `runtime.AddCleanup`, `runtime.SetFinalizer`, and any method named `Go` or `RegisterOnShutdown`. Goroutines that other standard-library code starts internally, such as `http.Server`'s per-connection goroutines, are not covered by the test; the handlers that run on them recover as described. A test makes the engine's handler panic on a message and checks that the handler the adapter registers with the protocol library (row 17) refuses it, logs the panic without its value and counts it. | M0. M1 for the protocol adapter's event handler. |
| 10 | **Listener inventory.** Only `internal/listeners` opens sockets, only `internal/app` may use it, and only `internal/listeners` builds `http.Server` values, each with an explicit handler. `net.Listen` and related calls outside it, `http.ListenAndServe`, `http.Serve`, the package-level `http.Handle` and `http.HandleFunc`, and `http.DefaultServeMux` are banned, so a dependency that registers handlers on the default mux exposes nothing. | I-6 | `forbidigo` with type analysis and the architecture tests (static), which also refuse an alias of `*http.Server`, or outside `internal/listeners` of `listeners.Spec` (a generic function that makes the zero value of a type parameter inferred from a `*http.Server` passes them, but outside `internal/listeners` no listener can be opened for it to serve); a test that inspects the test process's open sockets and expects exactly the client, admin and health listeners; tests that `/debug/` paths answer `404` on every handler and on every live listener, with handlers registered on the default mux to prove the point. | M0 |
| 11 | **Golden files for MCP tools.** Tool names, descriptions and input schemas are compile-time constants compared against committed golden files. | I-5 | Golden-file test. | M2 |
| 12 | **Constant-time credential comparison.** The admin credential type cannot be compared with `==`, and the admin digest is compared only through `crypto/subtle`, in the seal package's `MatchAdminCredential`. | I-7 | Compile time (a fixture comparing two credentials fails to compile); an architecture test on comparisons in `internal/policy`, the seal package included, and `internal/token`. Because that test recognises calls by name, another architecture test refuses, in non-test files under those two packages, any declaration other than a label (receiver type parameters and import names included) named like a package the file imports or like a predeclared identifier such as `len`. | M0 |
| 13 | **One log writer.** `serve` wraps standard output in one `logx.Writer` before it logs anything, loads the master key right after the configuration and data directory checks, and keys the writer with the `log-redact` key derived from it. Every logger is built by `logx.New` on that writer: `app.New` takes it, so the app's logger, its alert logger, the notifier (row 19), the embedded-metric-format lines, the HTTP servers' error logs and, once `app.New` has installed its logger, the panic reports of `safego` all write through it. A panic recovered before that, in the signal goroutine `main` starts first, is recovered but neither logged nor counted, so it cannot reach Go's default logger on standard error. The writer replaces identifiers in `user@server` form whose server name no ASCII letter or digit follows directly with keyed pseudonyms, keeps a JSON line valid when an identifier follows an escape sequence, drops every line longer than 65,536 bytes or carrying XML in one of these shapes, also written with the JSON escapes `\u003c`, `\u003e` and `\"`: a closing tag, a self-closing tag without attributes, the start of a tag with an attribute (`<x a="`) whatever its value holds and whether or not the tag ends, a tag directly followed by another (`<x><y`), `<!--`, `<![CDATA[` or `<?xml` (an opening tag without attributes followed only by text is kept), and writes each line whole under one lock, only once its newline has arrived, so a line written in pieces is checked as a whole; `logx.New` renders a byte slice of any type, or a pointer to one, as its length, even when it has a `String` or `Error` method, as a raw JSON value does, and any other value that is not a string, number, boolean, time, error or value with a `String` method as its type name, as it does a protocol message or a URL whatever its methods. Text that code formats into a message is only pseudonymised and checked for XML. The signal library that the protocol library links for its encryption prints its log lines to standard output unless it is given a logger; the adapter gives it one when it is built, which discards its debug and information lines, since they carry key material, and writes its warnings and errors through the protocol-library adapter (row 17). | I-5 | Compile time (`app.New` and `logx.New` take a `*logx.Writer`); an architecture test that refuses `slog.NewJSONHandler` and `slog.NewTextHandler` in non-test files outside `internal/logx` (a custom `slog.Handler` written elsewhere would pass it); an architecture test that refuses in non-test files Go's default loggers (`slog.Default`, `slog.SetDefault`, the package-level logging functions of `log/slog` and `log`, `log.Default`, `log.SetOutput`, `log.Writer` and `log.Output`), `fmt.Print`, `fmt.Printf` and `fmt.Println`, the built-ins `print` and `println`, `syscall.Stdout`, `syscall.Stderr` and `syscall.Write`, `os.Stdout`, `os.Stderr` and `os.NewFile` outside `cmd/wawarden/main.go`, and a string literal naming `/dev/stdout`, `/dev/stderr` or an entry of `/dev/fd/` or of a process's `/proc/.../fd/` (a raw system call, or such a path assembled at run time, would pass it). That test reads only this module's code: code in a linked module that writes to standard output or standard error itself passes it, such as the signal library's default logger, which a test below covers, or the printing functions of the SQLite runtime's C library, such as its assertion-failure handler. Tests: every identifier syntax becomes the pseudonym an HMAC written out in the test gives, and every form of one identifier the same one; module paths, `<nil>` and `<autogenerated>` are kept; every XML shape drops its line, including an attribute value holding a quote, raw, as a message and as `json.Marshal` writes it, and a tag followed by a tag without a closing tag; a canary identifier passed as a string, byte slice, raw JSON value, byte slice with a `String` or `Error` method, pointer to a raw JSON value or to such a byte slice, error, `Stringer`, protocol message, URL, struct, map, slice, array, pointer, chat, grant and `LogValuer`, as a message, a key and a group, through `With`, `WithGroup`, the `log` package bridge and direct writes never reaches the output, which fails when values are not rendered fail-closed; lines stay valid JSON next to escapes; an identifier or a tag split across two writes is pseudonymised or dropped as if written at once, and an overlong line is dropped; concurrent loggers and writers produce whole lines; `serve` reports a goroutine named after an identifier under the pseudonym keyed by the data directory's master key; while the signal library rejects a direct message of 8 bytes and a group message of 64 bytes, which it quotes in its error lines, and is asked for a debug and an information line, nothing reaches standard output, and the writer receives both error lines, each one JSON line with the quoted identifier pseudonymised and no line forged from the quoted bytes. | M1 |
| 14 | **The keys directory belongs to `internal/keys`.** Only `internal/keys` creates, checks and reads `keys/` and the master key in the data directory (see [Startup refusals](#startup-refusals)), and it derives every purpose key with HKDF-SHA256. A master key value prints no key material through `fmt`, `encoding/json` or `log/slog`. | I-5, I-7 | An architecture test refuses, in non-test files outside `internal/keys`, a string literal outside import declarations, or a run of concatenated literals, with a path element named `keys`; a name assembled at run time, such as one built by `strings.ToLower` or a conversion, passes it, and so does a wildcard or a directory walk that reaches `keys/` without naming it, such as `filepath.Glob` on `*/master` under the data directory. Tests of every refusal, of 16 racing creators ending with one key and no temporary file, of derivation vectors checked against an HKDF written out in the test, and that every `fmt` verb, `encoding/json` and both `log/slog` handlers print no key material from a master key. | M1 |
| 15 | **Verified storage and deletion on disk.** The archive is opened with foreign keys on, a `TRUNCATE` rollback journal, `synchronous` `FULL`, exclusive locking, temporary storage in memory, `secure_delete` `ON` and a 5-second busy timeout; a per-driver connection hook reads every one of them back and refuses the connection on any difference, so `PERSIST`, write-ahead logging and `secure_delete` `FAST` cannot take effect, and then takes the exclusive lock. The single connection stays valid after an interrupted call, and a second connection is refused, because the driver would otherwise discard the first and silently drop the lock; profile `local` selects open-file-description locks on Linux and refuses to start when the kernel or the filesystem rejects them, and profile `nfs` selects classic POSIX locks, one kind per process. The DSN is built with `net/url`. The full-text index is created with its `secure-delete` option, and a revoke, an edit or an expiry issues the index's `delete` command with the exact old text, then nulls `text` and `text_display` and writes a new or empty index row, in one transaction. That transaction also tokenises the old text in a contentless full-text table of the in-memory temporary database, and when one of its trigrams that no remaining message holds is still a page key of the index (secure delete removes the term but keeps the key), it records that an index rewrite is due. After the commit the index is rewritten whole (`optimize`, with a filler row added before and deleted after, so that an index of a single segment is rewritten too) under a 5-minute deadline of its own, clearing the record in the same transaction; a start that finds the record, left by a stop or a failed rewrite, rewrites before it opens the archive. Quoted text is never stored, and reactions and poll updates store no content. | I-3, I-8 | Tests: each setting refused when it reads back differently; a database in write-ahead-log mode converted; an interrupted read, write or statement keeping the lock against another process, with a negative control showing the driver's own connection handling loses it; a stray close of the database file not freeing the lock on Linux with profile `local`, and freeing it with profile `nfs`; the other profile's lock kind refused once a process has locked; profile `local` refused when the driver reports no open-file-description locks; files created `0600` under `umask 0`; every file-mode, owner, filesystem and lock-wait refusal. A canary of 1,500 two-byte letters in both cases that arrives through the inbox, is written among more than 3,000 messages and 25 companions over its alphabet, one per transaction, so that the index has page keys and one of them is a trigram of the canary, and is then revoked, expired or edited, which must rewrite the index, is absent from `MATCH`, from `fts5vocab`, from the page keys, from the message rows, and, as a whole and as each of its trigrams that the baseline database and the companions lack, from the raw bytes of `archive.db` and its journal, open and closed; the same holds for an index of a single segment, for a rewrite cut short by a stop, which the next open completes even with a write deadline of 1 ns, and for a rewrite that runs out of time, which the write reports as such after its commit and the next open completes; negative controls show the proof fails without `secure_delete`, without the index's `secure-delete` option when no rewrite follows, without its `delete` command or without the rewrite; a rewrite runs under its own deadline and not the write deadline; the record of a due rewrite cannot be set through the sync-state API. | M1 |
| 16 | **A neutral engine core.** `internal/engine` and its subpackages other than the protocol adapter `internal/engine/wa` handle plain data: they may import neither the protocol library nor protobuf, and, like all code outside `internal/store`, neither `database/sql` nor the SQLite driver; the core reaches the archive only through `internal/store/ingest`. Every goroutine it starts goes through `safego.Go` (row 9). | I-3, I-5, I-9 | An architecture test on the non-test files of those packages, with violating and conforming self-tests, and `depguard`. | M1 |
| 17 | **The protocol library behind one adapter.** Outside test files, only `internal/engine/wa` imports `go.mau.fi/whatsmeow` and `google.golang.org/protobuf`, and of the signal library `go.mau.fi/libsignal` only its logger package, and `internal/store/session` imports the library's two device store packages and nothing else of it; `session.db`'s raw handle, which the library needs, can be reached only there. Calls that would break a guarantee are refused in every file, tests included: presence, chat presence and presence subscriptions, read receipts and visible delivery receipts, the history download that writes any own device's payloads into `session.db`, every newsletter call and the status-message setter (the library can end the process inside them), its raw GraphQL query, the QR channel (it logs the codes, which carry the device's pairing secret), the decrypted event buffer (plaintext in `session.db`), the library's internals, an event handler whose answer the library ignores, the removal of one or all event handlers (the library would then acknowledge every event without delivering it), the loggers that bypass the writer (its standard-output logger, its zerolog adapter and a default context logger), and its proxy setters. The test refuses these names wherever they appear: as a selector, a declared name, a composite-literal key or a struct field, and in a string literal or a concatenation of string literals, in any letter case. The names of the settings the adapter gives the client (its reconnection, login re-dial, history download and receipt, message re-request, retry store, reporting token, acknowledgement, token refresh and pre-pairing settings) may appear only as a field selected from a value, and only the adapter's `installLocked`, which builds every client the adapter holds, may assign one, range into one or take its address, outside any function literal; every other function in every file may only read them. Outside test files, only `installLocked` may name the library's client constructor and its handler registration, each outside any function literal, and only it may call the library's pre-login, websocket and media HTTP client setters, as calls outside any function literal, so that no later call, method value or method expression replaces a transport; the one handler it may register is `handlerFor(<receiver>.gen)` of its own receiver: the adapter's handler for the current client, which returns the engine's answer, so the library acknowledges an event and sends its delivery receipt only when the engine accepted it (row 9). Outside test files, the adapter and the device store may not import a document decoder or a template package (`encoding/json`, `encoding/gob`, `encoding/xml`, `net/rpc`, `text/template` and `html/template`, each with its subpackages), which reach a value's methods and fields by names held in a document or a template, a name assembled at run time included. A string literal is checked as written and again with its JSON `\u` escapes decoded, and the letter-case check follows Unicode case folding, as JSON's key matching does. So outside test files reflection by name, a decoded document, an executed template and a literal cannot reach any of them, and nothing changes them after `installLocked`. A protocol client that is not a pointer is refused in every file, so no literal, `new` or zero value builds one: only the library's constructor does, which only `installLocked` calls and test files may not name (row 18). The adapter turns the library's own reconnection off, the re-dial it makes when WhatsApp asks a device to log in again included, reports that request and every failed pairing to the engine as a drop, so that no attempt is left waiting, stubs its connection-token refresh, whose absence panics in a goroutine nothing recovers, and gives it HTTP clients with timeouts, size caps, no proxy from the environment and no redirect to another host. | I-5, I-6, account standing | The confined-imports, raw-database-handle and protocol-calls architecture tests, each with violating and conforming self-tests, and `depguard`; tests of the adapter's construction, which check, on the clients it builds for a paired and an unpaired device, on the client it prepares for a connection and on the fresh client that replaces a logged-out device, the settings, that the library's pre-login, websocket and media HTTP clients are the adapter's, and that the library holds exactly one event handler; tests that deliver every event through `handlerFor` and check that its answer is the engine's; and tests of the events it reports for a login request and for each kind of failed pairing. A name built at run time other than by joining string literals, such as a method or field that `reflect` reaches by its index or by a formatted name, or a document whose names a test assembles at run time and decodes into a client, passes the test; outside test files, `reflect` is confined by row 3 and the decoders and templates by this row. The tests read the library's HTTP clients and its number of handlers by reflection, but cannot call the handler it holds, since row 3 refuses the reflection that reaches unexported memory: that this handler is `handlerFor` of the current client rests on the protocol-calls test alone. | M1 |
| 18 | **No test reaches the network.** The adapter's dialer, and every transport the adapter builds whatever dial function it is handed, dial through a guard that refuses all connections when the binary is a test binary, and a version source that its constructor did not build fetches nothing instead of falling back to the library's default HTTP client; so a test that builds the production path, or a client or version source over the adapter's transports, cannot reach WhatsApp through it. Test files may not name a WhatsApp or Meta host; a URL, or a host name with a port, outside loopback and reserved example names; an IPv4 or bracketed IPv6 address with a port, IPv4-mapped and zoned forms included, other than a loopback or unspecified one, except the addresses a per-file allow-list names, each one the code under test validates or refuses before any connection; the library's client constructor, its own WebSocket (`socket.NewFrameSocket` and `socket.FrameSocket`), its setters for the client's transports, its version fetch, or the network calls of its client that no method of the adapter shares a name with: connecting with a context, downloads, uploads, application-state fetches and sends. Row 17 also refuses, in every file, a client built by a literal, `new` or a zero value, and the proxy setters, and outside test files the client constructor anywhere but the adapter's `installLocked`, so every protocol client a test can hold was built by the adapter: its connect, pairing and logout calls, whose names the adapter's own methods share, dial through the guard as well. `hack/offline-test.sh` runs the whole suite, release and dev builds, in a container without a network, from a module cache filled beforehand. | Account standing | Tests that the dialer, a transport built over another dial function, the protocol client's HTTP clients and a version source over such a transport refuse to dial from a test binary, and that a version source without its client asks for no connection; a test that checks by reflection that the protocol client holds the adapter's pre-login, websocket and media HTTP clients before it connects a paired and an unpaired device, so that a missing setter fails the test before any dial; the offline-tests and protocol-calls architecture tests with self-tests; the offline run, which the whatsmeow bump workflow makes before it opens or updates a pull request, and the maintainer by hand before a release. A test that opened a socket to an address it computed at run time, an IP address written without a port and joined to one by `net.JoinHostPort` or set in a `net.TCPAddr` included, or that handed the version source's test seam a transport or HTTP client of its own making, the standard library's included, would pass the static test: the guard covers only the dialer and the transports the adapter builds. | M1 |
| 19 | **One notifier.** Every operational event (`unpaired`, `disconnected`, `pair_rejected`, `logout_failed`, `quarantine`, `rekey_conflict`, `ingest_paused`, `backup_done`, `backup_failed`, `admin_mutation`, `admin_auth_failure`) is written by `internal/notify`, through one method per event whose parameters are that event's fields, as a line of the log writer (row 13) and, when configured, a signed post to the webhook. Each string field is checked against the shape of a lower-case code, and an error type against that of a Go type name, and is written as `invalid` otherwise; the engine and the admin routes reach the notifier through small interfaces. The pairing code reaches no event, log line, metric or response other than the pairing route's. | I-5 | An architecture test refuses, in non-test files outside `internal/notify`, an `"event"` key followed by the name of one of these events in any call, a concatenation of literals included, and checks its list against the constants of `internal/notify`; tests of every event's fields, of identifiers, names and other non-codes written as `invalid` on standard output and in posted bodies, of the signature, retries, the destination check, the webhook client's proxy, redirect, timeout and TLS settings and its 4 KiB answer cap, a bounded queue behind a stalled receiver and a bounded shutdown that reports what it dropped; canary tests that a pairing code reaches neither standard output, metrics, embedded-metric-format lines, events nor response headers, from the API handlers and through a running service. A string field filled with text that has the shape of a code, such as a single lower-case word, passes the check. | M1 |
| 20 | **Recipient-only backups.** Only `internal/backup` imports `filippo.io/age`, and outside test files it may use only `Encrypt`, `ParseRecipients` and the `Recipient` type of its root package: no identity type or constructor, no `Decrypt` and no other age package, so the service can encrypt a backup but holds nothing that decrypts one. `WAWARDEN_BACKUP_AGE_RECIPIENT` is parsed by age's own parser behind a fixed error, so a refusal never repeats it, a pasted secret key included. Only `internal/backup` names the `backups/` directory: it stages each database copy with mode `0600` in a `0700` `backups/tmp/`, streams it through age without a second plaintext copy, flushes and renames the encrypted file into place, and on any failure removes the staging directory, the partial file and a file renamed but not flushed; `serve` removes `backups/tmp/` at every start once it holds the archive's lock. The manifest in a backup holds a build version, schema versions, sizes and a time, never an identifier. | I-5, I-7 | The confined-imports architecture rule and a depguard rule confine age to `internal/backup`; the age-recipient-only rule refuses every other use of age in non-test code; the backups-directory rule refuses, outside `internal/backup` and tests, any literal path element `backups`, in a string literal or a run of concatenated literals, and any use of `backup.Dir` except the start-time check in `internal/config` (a name assembled at run time passes it, and so does a wildcard or a directory walk that reaches `backups/` without naming it); each rule has violating and conforming self-tests. Tests: a round trip that decrypts with an identity generated in the test, opens both copies read-only, checks their integrity, row counts, schema versions and the manifest's keys; a backup taken while ingest writes; every failure path, from a directory of the wrong mode or a symbolic link to a full disk, a failed `fsync`, rename or cleanup and a shutdown, leaves no staging or partial file and reports one `backup_failed`; truncated, bit-flipped and extended files and another identity fail to decrypt; file and directory modes under umask `0`; recipients that are empty, malformed, more than one, an SSH or plugin recipient, or an age secret key are refused without the value in the error. | M1 |
| 21 | **A fake engine for development builds only.** `internal/engine/fake` is a scripted stand-in for the protocol adapter that pairs, connects and delivers synthetic events and history offline; a development build runs it in place of WhatsApp when `WAWARDEN_DEV_FAKE_ENGINE` selects it (see [the fake engine](configuration.md#fake-engine)). Every file of the package and every file that imports it compiles only with the `dev` tag, so a release build has no path to it and always runs the WhatsApp engine; nothing stops a development build that runs the fake on a release build's data directory from storing its synthetic messages in that archive. Outside its tests, the fake imports only buffers, zlib, context, JSON, errors, I/O, slices, sync and time from the standard library, and the engine core, `internal/policy` and `internal/safego` from the module: nothing that opens a socket or starts a program. It starts goroutines only through `safego.Go` (row 9) and never logs its pairing code. | Integrity of the archive; account standing | The dev-only-fake-engine architecture rule refuses a file under `internal/engine/fake`, tests included, or a file that imports it, unless it has exactly one `//go:build` line, where the go tool reads it, that is false for every assignment of its other tags while `dev` is unset; the fake-engine-imports rule refuses any other import in the fake's non-test files; both have violating and conforming self-tests, and the walk must find the fake and its selector. `hack/repro-build.sh` fails when a release binary contains the fake's marker string, which dev binaries keep, or any symbol of the package; a release-only test checks that a release build refuses `WAWARDEN_DEV_FAKE_ENGINE`. Tests: the fake's own (pairing only the owner's number, the script across a dropped connection, a refused event offered again, the history blobs, the marker in a dev binary), and an end-to-end test that runs `serve` with the fake, stops unless the fake is the engine running, pairs through the admin CLI, and checks the status, the metrics, the events, that no pairing code, message text, chat identifier or group name reaches the output, and the archive; a second one pairs the wrong account and sees it rejected and logged out. | M1 |

Other tests that guard the invariants:

- A byte scan of `archive.db` and its journal, the full-text index's vocabulary
  and page keys, and a `MATCH` query for a revoked, an expired and an edited
  canary, with negative controls (I-8; row 15). M1.
- Tests that a startup refusal never repeats the configured value, and that the
  token `admin init` prints appears exactly once in its output (I-5, I-7). M0.
- Release builds prove that the `dev` build tag is absent: the build settings
  embedded in the binary show no `dev` tag, and the binary does not contain a
  marker string that only dev builds carry. A release build also refuses to start
  when it sees a `WAWARDEN_DEV_*` variable. M0. The binary also holds neither
  the fake engine's marker string nor any symbol of `internal/engine/fake`
  (row 21). M1.

## Startup refusals

`wawarden serve` refuses to start, with a stable reason code, when any check
below fails. [`configuration.md`](configuration.md#startup-refusals) lists the
reason codes of the checks in the current build.

| Condition | Status |
|---|---|
| The environment holds a `WAWARDEN_` variable the build does not implement. | M0 |
| The admin credential is supplied in plaintext (`WAWARDEN_ADMIN_TOKEN`). Only `WAWARDEN_ADMIN_TOKEN_SHA256` or `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` (a SHA-256 in hexadecimal) is accepted. Without a hash, the admin listener is disabled, never open. | M0 |
| The admin hash is not 64 hexadecimal characters, both hash sources are set, or the hash file cannot be read or is not a regular file. | M0 |
| A release build sees a `WAWARDEN_DEV_*` variable, such as `WAWARDEN_DEV_FAKE_ENGINE`, which selects the fake engine that only builds with the `dev` tag contain (row 21). | M0 |
| A development build sees a `WAWARDEN_DEV_FAKE_ENGINE` value other than `0`, `1` or `wrong_account`. | M1 |
| `GOTRACEBACK` is set to anything other than unset, empty, `none` or `single`; numeric levels are refused too, `0` included. The named levels `all`, `system`, `crash` and `wer` print every goroutine's stack in crash output, and so does every other refused value once the service sets the `single` level itself. | M0 |
| A listen address is not an IP literal with a port from 1 to 65535, the health address is not loopback, or two enabled listeners share an address. | M0 |
| The process runs with a real or effective user ID of 0 without `--allow-root`. | M0 |
| The data directory cannot be created, is not a directory, is a symbolic link, is not owned by the process's effective user, or does not have mode `0700` exactly. | M0 |
| `keys/` in the data directory or the master key in it cannot be created or read, is a symbolic link or not a directory or regular file, is not owned by the process's effective user, or has the wrong mode (`keys/` exactly `0700`; the master key no permission for group or others and no setuid, setgid or sticky bit); or the master key does not hold exactly 32 bytes. | M1 |
| `archive.db` or its journal is not a regular file, is readable or writable by group or others, or is owned by another user; `history/` or `backups/` exists but is not a directory of mode `0700` owned by the service's user. | M1 |
| `session.db` or its journal is not a regular file, grants any access to group or others, or is owned by another user; or its settings read back differently from the archive's (row 15). | M1 |
| The archive's storage settings read back differently from the fixed ones (row 15): the journal mode is not `TRUNCATE` (write-ahead logging and `PERSIST`, which keeps deleted pages, are never used), `secure_delete` is not `ON`, or any other setting differs. | M1 |
| Storage profile `local` on a network filesystem, as `statfs` reports it. | M1 |
| The archive's schema is newer than the build knows. | M1 |
| `WAWARDEN_OWNER_PHONE` is set but is not an E.164 number, or `WAWARDEN_HISTORY_MAX_BYTES` is not from 1 byte to 256 MiB. Unset, the owner's number refuses pairing, not the start. | M1 |
| `WAWARDEN_UNSAFE_DEBUG` is set but is not a number of minutes from 1 to 60. Without it the protocol library's debug output is discarded at every log level; with it, the output is written for that many minutes after an `unsafe_debug` banner, and discarded again afterwards. | M1 |
| `WAWARDEN_METRICS_EMF` or `WAWARDEN_NOTIFY_ALLOW_PRIVATE` is not `0` or `1`; the webhook URL is not `https` or carries credentials, a fragment or an IPv6 zone; the webhook's secret file or private-destination switch is set without a URL, or a URL without a secret file; the secret file cannot be read, is not a regular file, grants write permission to its group or any permission to others, holds more than 4096 bytes or less than 32 apart from surrounding white space. | M1 |
| `WAWARDEN_BACKUP_AGE_RECIPIENT` is set but is not exactly one age X25519 or hybrid recipient: empty, with white space, a comment or a second recipient, an SSH or plugin recipient, or an age secret key pasted by mistake. The refusal never repeats the value. | M1 |
| A rate limit is zero or negative, or above its hard cap, without an `UNSAFE_` override. `0` never means unlimited. | M2 for read and search limits; M3 for send limits (global cap 60 per hour). |

### Pairing refusals (M1)

Pairing is refused while a device is already paired, while the owner's phone
number is missing, and beyond three attempts per hour; an owner's number that is
set but not in E.164 form refuses the start. The pairing code goes to the caller
only and is never logged. After pairing, the linked account must be the owner's
number; otherwise the engine raises a `pair_rejected` alert and logs the new
device out, retrying with backoff and a `logout_failed` alert for as long as
the logout fails. Until the device is gone, the engine connects it neither on
its own nor on an explicit reconnect, and drops what its connection delivers
before it is written. Before every connection, and at the start before the
restart budget and the version fetch, it also checks that the stored device's number is
exactly the owner's. When it is not, as for a rejected device whose
logout never succeeded before a restart, or an owner's number that differs from
the paired one, the engine stays disconnected (`owner_mismatch`) with an alert,
refuses an explicit reconnect, drops what a connection would deliver and leaves
the device in place for the operator, without logging it out. A pairing
completed from someone else's account therefore cannot bind the gateway to that
account. When the owner's number is unset, the stored device is not checked, and
the stored number never reaches a log or an event. These are enforced and tested
in the engine core and in the protocol adapter, which refuses another account
before the library saves anything, requests a pairing code for the owner's
number only, drops QR codes, deletes the local device when a logout cannot
reach WhatsApp, and starts from a fresh device after a logout. The admin route
that starts pairing answers `409` `already_paired` while a
device is paired, `409` `owner_phone_missing` without the owner's number and
`429` `rate_limited` beyond three attempts an hour, and returns the pairing
code to its caller only: it is never logged, counted, kept or put into an
event.

## Secure defaults

| Default | Status |
|---|---|
| The client and admin listeners bind to loopback (`127.0.0.1:8080`, `127.0.0.1:8082`) unless configured otherwise; a warning is logged at every start for each of them bound elsewhere (plain HTTP carries bearer tokens), and the log level cannot suppress it. The health listener defaults to `127.0.0.1:8081` and accepts only loopback addresses. | M0 |
| The admin listener exists only when an admin token hash is configured. | M0 |
| `umask 077` and the `single` traceback level are set by the first two statements of `main`, which an architecture test checks. | M0 |
| HTTP servers have read-header, read, write and idle timeouts and a 16 KiB header limit; responses written by WaWarden's handlers carry `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`, and no CORS headers. Go's HTTP server answers malformed requests itself, before any handler runs, with a plain-text or empty error response without those headers. | M0 |
| Requests to the client or admin listener carrying an `Origin` or `Sec-Fetch-Site` header are refused with `403`; `OPTIONS` on those listeners is refused with `405`. | M0 |
| Every request on the client and admin listeners is authenticated before its body is read; error bodies are fixed strings that never echo the request. | M0 |
| The container image runs as a non-root user (`65532:65532`) on a distroless static base with no shell, and works with a read-only root filesystem. | M0 |
| The data directory is created with mode `0700`. | M0 |
| Request bodies are read only by a route handler, after authentication and the route's decision, through one strict JSON decoder: at most 16 KiB, exactly one `Content-Type` of `application/json`, valid UTF-8 without a byte-order mark, one object at most 8 levels deep with lower-case `snake_case` keys, no duplicate key once escapes are decoded, no unknown field and no trailing data; refusals are fixed `415`, `413` and `400` answers that never repeat the request. The admin routes that change state accept only the empty object. | M1 for the decoder and the admin routes; M2 for client routes. |
| Secrets reach the service as `_FILE` paths, never as command-line arguments. The CLI reads the admin token from a file, standard input or a token command, never from command-line arguments or environment variables, and checks its form before sending it. | M0 for the admin hash file. M1 for the CLI and the webhook's secret file. |
| The service holds one injected secret, the optional webhook signing secret, which the operator supplies as a `_FILE` path and the service reads once at start: the admin credential is a hash, client tokens are hashed in the database, and every other key is derived from a master key generated on first start (mode `0600`). | M0 for the admin credential. M1 for the master key, the log pseudonym key and the webhook signing secret. M2 for client tokens and the cursor and audit keys. |
| Database files are created with mode `0600` before SQLite opens them, and their journals take that mode. | M1 |
| Every log line passes through one writer: WhatsApp identifiers in `user@server` form, unless an ASCII letter or digit follows the server name directly, become `jid:` and 8 hexadecimal digits of an HMAC keyed from the master key, lines carrying XML in one of the recognised shapes are dropped, byte slices of any type and pointers to them are written as their length, even with a `String` or `Error` method (a raw JSON value included), and log values of other types than strings, numbers, booleans, times, errors and values with a `String` method, protocol messages and URLs included, as their type name. Protocol-library logs reach the writer through an adapter that also masks runs of six or more digits outside the `user@server` form, cuts long lines between words and discards debug output outside a `WAWARDEN_UNSAFE_DEBUG` window, and the logs of the signal library it links reach the writer through the same adapter, which discards their debug and information lines in every case, instead of standard output; MCP-library logs will reach it through an adapter too; tool arguments are never logged. | M1 for the writer and the protocol-library adapter; M2 for the MCP library. |
| Text that the admin CLI prints from a server answer passes through a terminal sanitiser that replaces every control, format, line-separator and paragraph-separator character and every invalid byte with U+FFFD, so an answer cannot move the cursor, rewrite the screen, set a link or write the clipboard. | M1 |
| The notification webhook is off unless configured; it accepts only `https`, takes no proxy, follows no redirect, uses TLS 1.2 or later and timeouts, and refuses link-local and metadata addresses, IPv6 addresses with a zone and the local-use NAT64 prefix always, and loopback, private and shared addresses unless allowed, checked on every connection after name resolution, with a well-known NAT64 address checked as the IPv4 address it embeds. | M1 |
| Live status, broadcast and newsletter traffic, and live traffic of every other chat that is not a phone-number user, a LID user or a group, is dropped at ingest, before it is written anywhere. Such conversations inside a history-sync blob are dropped while the blob is applied; the blob's file and its inline content, which hold them until then, are deleted once the blob is processed or quarantined. | M1 |
| Live protocol messages are not stored: the engine receives each live message as plain data, a type with no field for a media key, a message secret or the protocol message, and the archive's `messages` table has no `raw` column, which a test checks. History sync is the exception: each history blob, the protocol's own messages with their media keys and message secrets, is kept as received, in a `0600` file under `history/` or, when it arrives inline, in the archive, and the archive keeps its download reference, media key included, both only until the blob is processed or quarantined; a start deletes the files of blobs no longer pending. The protocol library keeps its own state, message secrets included, in `session.db` (see [Assets](#assets)). | M1 |
| Never sends read receipts, presence, typing indicators or status updates: no code asks the protocol library for them, and an architecture test refuses the calls. The library sends delivery receipts in their inactive form and other traffic by itself (see residual risks). | M1 |
| A service without a paired device makes no connection to WhatsApp, not even the version fetch, until pairing is requested, and reports `unpaired` once, and again whenever a paired device is lost. | M1 |
| Outbound HTTP to WhatsApp uses no proxy from the environment, a timeout on every step, TLS 1.2 or later, response caps (8 MiB for the version page, the history cap plus 32 bytes for a history download) and no redirect to another host. | M1 |
| No first contact (a DM with no prior inbound message) unless the client is allowed it; sends are paced, idempotent and budgeted per client, with `429` rather than queueing. | M3 |
| Disconnections from WhatsApp never exit the process, and `/healthz` does not depend on them; more than five starts within ten minutes start the engine disconnected, and reconnection after a drop backs off exponentially, with jitter, up to five minutes, to avoid reconnect storms, with the protocol library's own reconnection switched off; a connection that drops within a minute of coming up counts as a failed attempt and does not reset the delay. A replaced session, a temporary ban, a refused connection, a failed token refresh or a stored device of another number than the owner's waits for the operator. | M1 |
| With storage profile `local`, ingest pauses while the data directory's filesystem has less free space than `WAWARDEN_MIN_FREE_BYTES` (256 MiB by default; `0` turns the floor off), reports `ingest_paused`, and resumes once the free space reaches 1.25 times the floor. While it is paused the engine writes no message and no history-sync notification and leaves them unacknowledged, which loses them (see residual risks), and applies nothing from its inbox or from history sync. Profile `nfs` has no floor. | M1 |
| No backup is taken without `WAWARDEN_BACKUP_AGE_RECIPIENT`, and the start says so with `backup_disabled`; every backup is encrypted to that recipient, and there is no plaintext backup of either database. In this release one backup is taken per paired device, once its initial history sync has settled, and nothing else. | M1 |

## Residual risks

These remain at v1.0, after every control above is in place.

- **Unofficial protocol and account standing.** WhatsApp offers no official API for
  a personal account. WaWarden is a linked-device client built on an unofficial
  implementation of the multi-device protocol, and WhatsApp can restrict, log out
  or ban the account. The defaults reduce the signal (one linked session,
  read-mostly use, no read receipts or presence, no reconnect storms and guarded
  pairing, M1; paced sends to explicit chats and no first contact, M3) but cannot
  remove it. The phone must also come online at least every 14 days, or WhatsApp
  logs out every linked device.
- **Protocol churn.** A protocol change can stop WhatsApp connectivity until a new
  release ships. Reads of the archive keep working. The engine refreshes the client
  version at runtime for the common case (M1).
- **Upstream dependency.** The protocol library has a single maintainer and no
  tagged releases. `go.mod` pins it to an exact pseudo-version, and `go.sum`
  pins its content: the go command checks every download against that hash
  and, with default settings, checks a new hash against the Go checksum database
  before recording it. Modules are never vendored (the build script, the CI
  `modules` job and an architecture test refuse a `vendor` directory), so a
  build needs the Go module proxy, or whatever `GOPROXY` names. A scheduled
  workflow proposes each update as a pull request and never merges one. The
  pull request carries the upstream commit log, a diffstat and the diff of the
  watched paths between the two module versions' sources, complete in an
  artifact of the run when GitHub's limit on a description cuts it; its CI runs
  only once a maintainer approves them, and a maintainer reviews and merges it
  by hand (`RELEASING.md`). It links `go.mau.fi/libsignal`, licensed under
  GPL-3.0, which is why WaWarden is licensed under GPL-3.0-or-later from M1 on.
- **Timing of an out-of-scope message** (M2). A message named outside the
  client's scope is refused before any message query runs, while a message
  inside the scope that does not exist costs one query; both answers are the
  same, but the first comes sooner. The difference tells a client only that a
  chat lies outside its own scope, which its grant already tells it. Chats
  outside the scope and missing chats run the same query and cannot be told
  apart by the SQL they issue.
- **References of merged chats** (M2). When a re-key merges a chat without
  messages into the other chat of the same person, the merged chat's
  reference stops resolving and answers as missing. A merge that would remove
  a chat a client names is refused instead (`rekey_conflict` with
  `scoped_chat`), so only a reference that a client reading every chat kept is
  affected; the surviving chat stays listed.
- **Search normalisation.** The search query builder does not apply Unicode
  normalisation (NFC) to the query, so a query typed in a decomposed form does
  not find the same text stored precomposed, and the reverse.
- **Deletion residuals.** Backups taken before a message was revoked, edited or
  expired keep its old text until they are deleted; this release has no
  retention, so that is the operator's task. Filesystem blocks,
  snapshots and storage-level copies are outside the service's control. A
  trigram of the old text that a remaining message also holds stays in the
  full-text index, because that message still needs it. The deletion clears
  only the message's own text: when a push name, a chat name or a group subject,
  a message waiting in the inbox, an identifier or another stored value, or the
  schema holds the same text or one of its trigrams, those bytes stay in the
  archive. The
  full-text index finds a term through page keys, and a key can be a prefix of
  the first term of its page: the index rewrite runs only for a key that is a
  whole trigram of the old text, so a key holding its first one or two
  characters, or part of a character, stays until a later rewrite. A trigram that
  is a key also stays from the commit of the deletion until the rewrite commits,
  and, when the process stops or the rewrite fails in between, until the next
  start or the next rewrite. A revocation or an edit whose target is not yet in
  the archive is dropped and not kept for later; when history sync stores the
  target afterwards, as it can during the initial sync, where live traffic is
  applied while blobs wait for their download, the target keeps its original
  text, and nothing that arrives later removes it.
- **Backups** (M1). Encrypting to a recipient only protects a backup
  from whoever reads it without the age identity: the storage it is copied to,
  a backup of the data directory, a stolen disk. It does not protect a backup
  from whoever holds the identity, nor the live databases, which are in
  plaintext on the same host, and the service cannot check that an earlier
  backup still decrypts. While a backup runs, a plaintext copy of one database
  at a time sits in `backups/tmp/` with mode `0600`, and on a filesystem that
  keeps deleted blocks or snapshots it can outlive its removal; a crash leaves
  it there until the next start removes it. age does not authenticate the
  sender: anyone who knows the recipient, which is a public key, can write a
  well-formed backup that decrypts, so a backup is only as genuine as the
  storage that holds it, and the permissions of `backups/` and of wherever
  backups are copied must keep others from replacing or adding files. A
  truncated or altered backup is detected only when it is decrypted. The two
  databases are copied one after the other, so they are two points in time,
  the archive's first, and the device store goes on changing after its copy:
  a restored `session.db` holds the device's state as of the backup. The
  restore rule (restore only into a stopped service, never start two instances
  from one `session.db`) is documented, not enforced.
- **Prompt injection through message text.** Message text, contact names and group
  subjects are written by third parties and reach agents by design. WaWarden limits
  what a manipulated agent can do (per-chat scopes, no write access for read-all
  clients, `untrusted` and `origin` marking, constant tool definitions and a
  `PolicyDenials` metric to alert on, M2; no first contact, M3), but it cannot stop
  an agent from acting on what it reads.
- **A stolen token works from anywhere it can reach the service.** Its scope,
  expiry, revocation and rate limits (M2) bound the damage; nothing in the service
  ties it to a place.
- **The admin credential is a single factor.** Within whatever network boundary
  the deployer provides, anyone who can reach the admin listener and holds the
  admin token can read the metrics (M0), start pairing (M1) and create
  clients (M2). The token does not expire. With it, a caller can also read the status
  (the engine's state, counts and the last ingest time, no identifier), request
  pairing codes for the owner's number while no device is paired, which shows
  a notification on the owner's phone, and request reconnections, which are not
  rate-limited: a caller repeating them while the engine is connecting makes it
  dial WhatsApp again each time (M1). Its use is visible as
  `admin_mutation` and `admin_auth_failure` events, not prevented by them, and
  an audit trail arrives in M2 (see [The admin token](#the-admin-token)). Keep
  the admin listener reachable from fewer places than the client listener. The
  pairing code crosses the network in plain HTTP unless the deployer encrypts
  the path; a code is useful only to link the gateway itself, on the owner's
  phone, within WhatsApp's validity window. Prometheus metrics are readable only
  with the admin token, so a scraper of `/metrics` holds the full admin
  credential, and scrape configurations are often widely readable. Treat any
  scrape configuration as holding the admin token, and prefer the metrics in
  embedded metric format (M1), which need no token, for alerting.
- **Notifications can be late, lost or read by their receiver** (M1). Failed
  admin authentications after the first are reported up to about 70 seconds
  later, together. The webhook drops events when its queue of
  64 is full, after five failed attempts or at shutdown, counting them in
  `wawarden_notify_dropped_total`; a receiver that must not miss an event should
  alert on the embedded-metric-format metrics as well. Its receiver learns the
  events, their reasons, counts and times, and anyone with the signing secret
  can forge events that the receiver accepts, so keep the secret file and the
  receiver's copy as private as the admin token. The URL itself can hold a
  secret of the receiver's and sits in the service's environment. The
  destination check refuses addresses by range, so a public address that the
  operator did not mean, or an allowed private one, is reached as configured.
  It recognises only the well-known and local-use NAT64 prefixes: on a network
  whose NAT64 uses a prefix of its own, a name whose IPv4 address is private or
  link-local is synthesised into an IPv6 address that the check does not map
  back, and is reached.
- **The failed-authentication throttle does not limit guessing.** It changes the
  answer to failures beyond its budget and feeds a counter (M0); token entropy is
  what defeats guessing (see [Failed authentication](#failed-authentication)).
- **Network mistakes go unnoticed.** The service warns at start only for a client
  or admin listener bound to a non-loopback address. When a forwarder, sidecar or
  reverse proxy in front of loopback listeners exposes them too widely, the service
  binds loopback and logs nothing. Plain HTTP exposes bearer tokens on any network
  segment the deployer leaves unencrypted.
- **Log pseudonyms can be linked and tested.** A pseudonym stands for the same
  identifier in every log line until the master key changes, so lines about one
  chat can be linked to each other. It is 32 bits long, so two identifiers can
  share one. Whoever holds the master key, from the data directory or a backup,
  can confirm a guessed identifier against a pseudonym. Only identifiers in
  `user@server` form are recognised, and only when no ASCII letter or digit
  follows the server name directly: a phone number on its own, a
  protocol-library address of a user and a device without a server, or an
  identifier run into the following word passes unchanged, so code must not log
  one (M1).
- **XML is recognised by its shape.** The log writer drops a line only when it
  holds one of the XML shapes listed in
  [Pseudonyms and dropped lines](configuration.md#pseudonyms-and-dropped-lines).
  An opening tag without attributes followed only by text, such as a cut-off
  `<body>` element, passes, so code must not log message content even inside a
  tag (M1).
- **Panics in dependency goroutines.** A panic in a goroutine that a dependency
  starts (the protocol library, M1) is not recovered: the process
  exits, and the Go runtime prints the panic value to standard error first. That
  value can carry chat data, so I-5 does not hold for crash output. The protocol
  library handles each incoming stanza on a goroutine of its own and recovers a
  panic in only a few places; the adapter removes the one known panic, a missing
  connection-token refresh, by stubbing it, and recovers a panic in its own event
  handler and in the engine code that handler calls. It refuses the event then,
  so a message whose handling panicked is lost the way a refused message is (see
  below).
- **SQLite on a network filesystem** is outside SQLite's recommended
  configurations. The network-filesystem storage profile (rollback journal,
  exclusive locking, no write-ahead log, a single process; M1) reduces
  the risk; it does not remove it. It uses classic POSIX record locks, which a
  close of any other descriptor of the database file in the process would drop;
  the service's own code never opens the file (row 3a), but a dependency could.
- **Group admins as the archive knows them.** A revocation of another member's
  message in a group is applied when the archive records the revoker as an
  admin of that group, and that record is only as current as the group changes
  and history WhatsApp has delivered: a demotion the engine never received
  leaves a former admin able to revoke (M1). A member the archive
  does not record is refused. A member list from history sync never overrides
  what a live group change recorded: it cannot raise, re-add or remove a member
  that a live join, promotion, demotion or removal named, and it is ignored
  for a group whose whole member list arrived live. It still records the
  members no live change has named, so a group known only from history keeps
  its admins.
- **References from the other side of a direct chat.** The other party of a
  direct chat names it by the owner's identifier. The engine accepts the owner's
  number there, and the owner's LID only once the archive has learned which LID
  belongs to that number; until then, and whenever `WAWARDEN_OWNER_PHONE` is
  unset, such edits, revocations, reactions and replies are dropped or stored
  without their reference (M1).
- **Crashes count as attempts.** A history blob whose processing is cut short
  by a crash three times is quarantined like a failing one: the batches it had
  applied before each crash stay in the archive, and its remaining messages do
  not reach it. A graceful stop gives its attempt back (M1).
- **Raw history blobs wait on disk.** A downloaded history blob, the protocol's
  own messages with their media keys and message secrets, stays in its file
  under `history/` until it is processed or quarantined, and a start keeps the
  files of pending blobs, so while ingest is paused or the service is stopped,
  it can stay there indefinitely (M1).
- **An interrupted revoke.** A crash in the middle of a revoke, an edit or an
  expiry rolls it back, so the old text is in the database again until the
  change is applied again, and the journal keeps the pre-image until the next
  write truncates it.
- **Traffic the protocol library sends by itself** (M1). While
  connected it acknowledges every stanza, sends a delivery receipt for every
  message it decrypts, in the inactive form that a sender's phone does not show as
  delivered unless the device announced itself as available, which WaWarden never
  does, sends retry receipts for messages it cannot decrypt and asks the owner's
  phone to resend messages WhatsApp marks as unavailable, announces the device as
  active at each connection, uploads pre-keys, fetches the application state, and
  sends session telemetry once after pairing. None of this can be switched off,
  and all of it tells WhatsApp, and in part the sender, that the device is
  online.
- **A refused or interrupted message is lost** (M1). The protocol
  library advances and saves its session keys when it decrypts a message, before
  the engine sees it, and acknowledges it only after the engine wrote it to the
  inbox. A message the engine refuses (a full inbox of 5,000 rows, an ingest
  paused below the free-space floor, a failed write), or one decrypted just
  before the process stops, is delivered again by
  WhatsApp, but the library can no longer decrypt that copy and drops it, so it
  never reaches the archive; group changes are acknowledged as they arrive and
  are lost the same way. The library's buffer of decrypted messages would make
  delivery at-least-once at the price of message plaintext in `session.db`; it
  stays off.
- **Crafted live messages are decoded before WaWarden sees them** (M1). The
  protocol library decodes every decrypted message with protobuf's
  default limits, 10,000 levels of nesting and no size limit below its 16 MiB
  frame, so a peer can make the process allocate many times a message's size
  (about 40 times in measurements) before any WaWarden check runs. History blobs,
  which the adapter decodes itself, are held to a nesting of 28 levels and the
  history cap.
- **`session.db` belongs to the protocol library** (M1). The
  library's queries run without the archive's per-call deadlines, on the single
  connection that holds the lock, and whether its concurrent use of that one
  connection can stall has not been observed outside tests. The library keeps the
  identity mappings and privacy tokens it learned after a device is deleted, and
  accepts a contact's changed identity key without asking. Its own identity map,
  which any of the owner's devices can write, is never read for the archive; from
  M3, sends must address canonical chats, because the library rewrites a phone
  number through that map.
- **Requests answered by the owner's phone** (M1). When WhatsApp
  marks a message unavailable, the library asks the owner's phone for it and
  passes the phone's answer on as live traffic in the chat that answer names. The
  library accepts such answers only from the owner's primary device, the trust
  that history sync gets as well. It builds them the way it builds history rows,
  so an edit's identifier and message fields hold its target's identifier and the
  new text; the edit's own identifier and protocol message remain only in the raw
  message and the row the phone sent, both of which the event carries. The
  adapter reads each answer from that row, as it reads history rows, so an edit
  is applied to its target and never stored as the target itself, which a test
  through the engine checks. Nothing in an answer shows that its sender wrote it:
  the archive holds what the owner's phone says was sent.
- **Bot accounts look like users** (M1). WhatsApp's own bot accounts,
  such as its AI assistant, have phone-number identifiers, so `Normalize`
  accepts them and their chats are archived like any other.
- **WhatsApp behaviour not observed yet** (M1). Whether the phone
  honours the request for a full history sync, how large real history blobs
  are, whether WhatsApp's servers check that an admin revocation comes from an
  admin (the engine checks the archive's record either way), and whether every
  country's account identifiers equal its E.164 numbers: when they differ,
  pairing fails closed.

## Trust roots

WaWarden cannot compensate for the following; a deployment must address them
outside the service.

1. **Whoever controls the deployment pipeline or the host controls the account.**
   Anyone who can change the image or configuration that is deployed, read the data
   directory, or restore its backups can impersonate the account without passing
   through any WaWarden control. Limit write access to the deployment pipeline and
   its configuration, and do not give hosts that run agents credentials that can
   change them.
2. **One host is one trust domain for tokens.** Processes running as the same user
   can read each other's tokens, and nothing in WaWarden can tell them apart. Do not
   place a token that reads every chat, or a write token, on a host where agents
   with shell access read untrusted input.
3. **Other linked devices bypass the gateway.** Any other client linked to the same
   account has unrestricted access to it, and the protocol library accepts
   history-sync and identity-mapping payloads from any of the account's own
   devices. WaWarden accepts history only from the primary device and learns its
   own identity map only from server-asserted fields and the primary device's
   history (M1); unlink companion devices that
   agents can reach once WaWarden replaces them.
4. **Anyone who can read process logs must not learn chat identities or content.**
   Treat everyone with access to standard output, the log pipeline or its storage
   as a reader of logs. That is why I-5 holds for every line the process writes,
   with the crash-output exception under residual risks.
5. **The maintainer's GitHub account controls releases.** It is the only approver
   of the `release` environment. A release built from malicious source on `main`
   passes every verification in `RELEASING.md`: provenance, signatures and
   reproducibility show where and how an artifact was built, not that its source is
   benign. Deploy by digest, and review the source diff between the version you run
   and the next one before upgrading.

## Reporting

Report vulnerabilities as described in [`SECURITY.md`](../SECURITY.md).
