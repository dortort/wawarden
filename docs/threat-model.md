# WaWarden threat model

> **Status: draft for v1.0.** This document describes the security design that
> v1.0 will ship. As of milestone **M0**, the binary is a scaffold: a loopback
> health endpoint; a client listener that has no routes and refuses every request;
> and, when an admin token hash is configured, an admin listener that serves
> metrics to the admin token. There is no WhatsApp engine, storage, client
> management, REST route or MCP route yet; they arrive in milestones M1 to M3.
>
> Every control carries a status. **M0** means it is in place as of the M0
> release. **M1, on `main`** means it is in place on the `main` branch and ships
> with the M1 release. **M1**, **M2** or **M3** alone means it is planned for that
> milestone and does not exist yet. The residual-risks section describes the state
> at v1.0, and the controls it names carry their milestone too.
> [`configuration.md`](configuration.md) documents the behaviour of the current
> build in detail.

## System summary

WaWarden is one process that links to one personal WhatsApp account as a
companion device and exposes that account to the owner's own agents and
applications. Each client holds a bearer token that grants read access, write
access or both, limited to an allowlist of chats.

The process opens at most three listeners, all plain HTTP, and nothing else:

| Listener | Address | Serves | Status |
|---|---|---|---|
| Client | `WAWARDEN_LISTEN`, default `127.0.0.1:8080` | REST `/v1` and MCP `/mcp` (M2, M3). As of M0 there are no routes and no client tokens, so every request is refused. | M0 |
| Admin | `WAWARDEN_ADMIN_LISTEN`, default `127.0.0.1:8082`; opened only when an admin token hash is configured | Metrics (M0); pairing and status (M1); client management (M2) | M0 |
| Health | `WAWARDEN_HEALTH_LISTEN`, default `127.0.0.1:8081`; loopback addresses only | `/healthz`, without authentication | M0 |

Outbound traffic is limited to the WhatsApp websocket, media and history
downloads (M1), and an optional notification webhook (M1). As of M0, `serve`
makes no outbound connection.

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
| `session.db` | The linked device's keys. Whoever holds it can act as the account until the device is unlinked on the phone. | M1 |
| `archive.db` and history blobs | Every archived chat, in plaintext. | M1 |
| Admin credential | Gates the admin listener (M0), starts pairing (M1) and mints clients (M2). It does not expire. The service holds only its SHA-256. | M0 |
| Client tokens | Bearer credentials: whoever holds one and can reach the client listener has its access. The service stores only their hashes. | M2 |
| Master key (`keys/master` in the data directory) | The key of the log pseudonyms is derived from it (M1, on `main`), and so will be the chat-reference key of notification events (M1) and the cursor-sealing and audit keys (M2). Whoever holds it can test a guessed identifier against the pseudonyms in the logs. | M1, on `main` |
| Outbound integrity | Messages sent through WaWarden appear as sent by the account owner. | M3 |
| Account standing | WhatsApp can restrict or ban an account that links an unofficial client. | M1 |
| Backups | Encrypted copies of the two databases. | M1 |
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
- Readers of backups, when the operator chooses unencrypted backups.
- Delivery receipts: the protocol library sends them automatically.
- Deletion residuals in backups and in filesystem blocks the service cannot scrub.
- Agents that pass message text into tools. WaWarden marks the text as untrusted;
  what an agent does with it is the agent's responsibility.

## Trust boundaries

1. **Network to client listener (M0).** Before anything else, a request carrying
   an `Origin` or `Sec-Fetch-Site` header is refused with `403`, so a web page
   cannot use a browser as a client, and an `OPTIONS` request is refused with
   `405`. Every other request is authenticated before any route handler runs and
   before any byte of its body is read; a refused request that announces a body
   gets its answer at once and the connection is closed. As of M0 no client token
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
   counted in `wawarden_admin_auth_failures_total` and throttled, but not logged.
   What the token must satisfy, and the controls planned to detect its misuse, are
   under [The admin token](#the-admin-token).
3. **Loopback health (M0).** The health listener has no authentication, so the
   service refuses to start when its address is not loopback, and checks again
   before opening it. It answers only `GET /healthz`, with whether the process is
   up and serving (M0) and its databases open (M1).
4. **WhatsApp to engine (M1).** Everything that arrives from WhatsApp is untrusted,
   including payloads attributed to the account's own other devices, except where
   the server asserts a field. Protocol messages are applied only inside the chat
   they belong to. History-sync notifications are accepted only from the primary
   device (device 0), are size-capped when decompressed, and are persisted before
   parsing so that a poison payload is quarantined rather than replayed forever.
5. **Service to clients (M2).** Message text, contact names and group subjects are
   written by third parties. Every message object carries `untrusted: true` and its
   `origin`, and a `text_display` field with control, bidirectional-override,
   zero-width and tag characters removed. The function that removes them is on
   `main` (M1, on `main`): it keeps newline and tab, also removes every other
   format character, and replaces invalid UTF-8. The field arrives with the archive
   (M1). MCP tool names, descriptions and schemas are compile-time constants, so no
   chat-controlled string reaches a tool definition.
6. **Service to logs, metrics and notifications.** Chat identities leave the
   process only as keyed HMACs, and message text, query text, tokens and protocol
   messages never leave it. As of M0: no request is logged; failed authentications
   are counted, not logged; a startup refusal names the variable involved, never
   its value; recovered panics are logged with their type and stack, never their
   value; and the metrics carry only build information, failure counts and panic
   counts. As of M1, on `main`: every line `serve` writes to standard output passes
   through one writer that replaces each WhatsApp identifier in `user@server` form
   with a pseudonym keyed by the master key and drops every line carrying XML, and
   a log value that is not a string, number, boolean, time, error or value with a
   `String` method is written as its length when it is a byte slice and as its
   type name otherwise, never its content (see
   [Logging](configuration.md#pseudonyms-and-dropped-lines)). Identifiers in other
   forms, such as a phone number on its own, pass unchanged, so code must not log
   them. The protocol library's logger will feed that writer through an adapter
   (M1); the MCP library's logger will too (M2).
7. **Service to data directory and backups (M0, M1).** The data directory must be
   owned by the service's user with mode `0700`, and the service creates it that
   way (M0). The service creates `keys/` and the master key in it, and refuses to
   start unless they are a directory with mode exactly `0700` and a regular file
   of 32 bytes with no permission for group or others, both owned by the service's
   user and neither a symbolic link (M1, on `main`). `session.db`, `archive.db`
   and `history/` are checked the same way (M1). Backups are encrypted
   to an operator-supplied age recipient, so the service can write them but not
   read them (M1).
8. **Source to release artifacts (M0).** Releases are built only from `main` by a
   manually dispatched workflow that waits for the maintainer's approval, rebuilt
   independently and compared bit for bit, attested and signed. The approval gate
   and the immutability of published releases come from repository settings that
   are in place: a protected `release` environment, immutable releases, and a tag
   ruleset. The workflow checks the environment before building and fails after
   publishing a release that is not immutable; it cannot check the tag ruleset.
   See [`RELEASING.md`](../RELEASING.md).

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
- **Detecting its misuse.** As of M0, failed admin authentications are counted in
  `wawarden_admin_auth_failures_total`, which only the admin token can read, and
  nothing is logged. Planned: an `admin_auth_failure` event on standard output and
  the `AdminAuthFailures` metric in embedded metric format for repeated failures,
  and a notify event for every admin mutation (M1); a hash-chained audit table of
  admin and client actions (M2).

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
- **I-8 Deletions are honoured on disk.** After a revoke, an edit or an expiry, the
  old text is gone from the database, its journal and the full-text index (backups
  excepted, see residual risks).
- **I-9 Panics are contained.** A panic in a goroutine that WaWarden starts, or in
  one of its HTTP handlers, is recovered, counted and logged without its value.
  Goroutines that dependencies start are not covered (see residual risks).

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
| 1 | **Grant types.** `ReadGrant`, `WriteGrant` and `AdminGrant` are declared, with only unexported fields, in `internal/policy/internal/seal`, and package policy exports them under the same names. Outside the seal package, a valid grant can only come from the decision functions in `internal/policy/decide.go`. Decisions deny by default: a missing, revoked, expired or never-expiring client gets no grant; a client without write chats, or one that may read every chat, gets no write grant; the zero value of every grant allows nothing. Grants copy the client's chat sets, so changing a client afterwards does not change a grant. | I-1, I-2, I-7 | Compile time: the fields belong to the seal package, so the compiler refuses, in every other package and in package policy itself, a grant literal with fields, a conversion from a look-alike type, a field write and a field's address; fixtures prove it for a literal and a field write outside `internal/policy`, and for the forms row 4 lists inside package policy. The seal-constructors architecture test (row 4) lets each grant constructor be called only in its decision function, and the rules row 4 names with its guarantee keep code from going around the type system. Unit tests for every decision, a reflection test that the grant types have no exported fields, and tests that zero-value grants, and grants built in the seal package without their validity flag, allow nothing. | M0 for the grants and decisions. M1, on `main`, for the seal package. M2 for the scoped store, whose every method takes a grant as its first argument and which exports nothing without one. |
| 2 | **Registration helpers.** Client and admin routes are registered only through the unexported helpers `read`, `write` and `admin` in `internal/api`. Each one obtains a grant from the matching decision before its handler runs and answers the same `404` as an unknown route when the decision denies. The mux is unexported and only `internal/api/register.go` may create one or register on it, so a route without a policy class cannot be added from outside. Authentication runs before the mux. The health listener serves only `/healthz`. MCP tools will use the same pattern (`ReadTool`, `WriteTool`). | I-0, I-1, I-2, I-4 | Compile time (fixtures that reach the router from another package fail to compile); an architecture test on mux ownership; a coverage test that compares every registered route and its class with a golden list; pipeline tests proving that authentication precedes routing and that no body is read before it, form-encoded and multipart bodies included; an architecture test that refuses, in every non-test file, any reference in Go source to a method named like one of the `net/http` form-parsing methods (`ParseForm`, `ParseMultipartForm`, `FormValue`, `PostFormValue`, `FormFile`, `MultipartReader`) and any string literal, or concatenation of literals, in which one of those names appears as a word. A name assembled from named constants or at run time, or written outside Go source, such as in a template file embedded with `go:embed`, passes that test; `internal/api` may import neither `text/template` nor `html/template`. Only `internal/api` serves requests, and its API reads JSON only. | M0 for the HTTP helpers and the coverage test. M2 for the MCP helpers. |
| 3 | **Import fences.** `internal/api` may not import `database/sql`, `text/template`, `html/template`, the SQLite driver, the ingest store or the WhatsApp protocol library; only `internal/api/admin` may import the admin store. No non-test file may import `mime/multipart`. `internal/policy` and its seal package may import only an allow-list of standard-library packages, which excludes networking, operating-system and filesystem packages; package policy may also import the seal package, and no other package may: Go's `internal` rule refuses it outside `internal/policy`, which a fixture proves, and the seal-constructors architecture test (row 4) refuses it in every file, test files included, except the files of package policy and those in the seal package's directory, so an external test package `policy_test` may not import it either. `net/http/pprof`, `expvar`, `net/http/cgi`, `net/http/fcgi`, `plugin`, `unsafe` and cgo are banned everywhere. Outside test files, only `internal/api/dto` may import `reflect`, and no file, test files included, may use a selector named `NewAt`, `SetPointer`, `UnsafeAddr` or `UnsafePointer`, the `reflect` functions and methods that read or write memory through an unsafe pointer without importing `unsafe`. A module outside the standard library may be imported only when it is on a reviewed list of importable modules, and `go.mod` may require only those modules and, marked `// indirect`, the modules on a second reviewed list of indirect-only requirements; both lists are empty. A `//nolint` comment may not silence `depguard` or `forbidigo`. The non-test files of `internal/keys`, `internal/logx` and `internal/sanitize` may import only the standard library. | I-1, I-5, I-6 | `depguard` in `golangci-lint`, and the architecture tests; the banned-import and third-party checks, and the reflection test's selector check, cover test files too. | M0. M1, on `main`, for the seal package, the two module lists and the standard-library-only packages. |
| 3a | **One database handle.** The raw `*sql.DB` lives only in `internal/store/internal/db`. | I-1 | Go's `internal` package rule (toolchain). | M1 |
| 4 | **Canonical chat identifiers.** `CanonicalChat` is `seal.Chat`, declared in `internal/policy/internal/seal` with unexported fields and a validity flag, and its zero value is invalid. Outside the seal package, a valid chat can only come from `Normalize`, in `internal/policy/normalize.go`. It accepts the servers `s.whatsapp.net`, `lid` and `g.us`, and `c.us`, which it maps to `s.whatsapp.net`, in lower case only; every other server is rejected, `broadcast`, `newsletter`, `hosted`, `hosted.lid`, `bot`, `msgr` and `interop` included. Before the `@` of a phone-number or LID user it accepts `USER`, `USER:DEVICE` or `USER.AGENT:DEVICE`, where `USER` is 1 to 24 digits, `AGENT` 1 to 3 digits of value at most 255 and `DEVICE` 1 to 5 digits of value at most 65535; it drops the agent and the device, and rejects an out-of-range value instead of wrapping it. Before the `@` of a group it accepts 1 to 24 digits, or two such runs joined by `-`. Digits are ASCII only. Any other byte, the empty string and an input longer than 128 bytes are rejected, and a rejection returns the zero value. A rejected input allocates nothing; an accepted one allocates only to build and intern its result. The result is the user or group followed by `@` and its server, and normalising it again returns the same value. A chat holds its identifier as a `unique.Handle[string]`, so `fmt` and the text handler of `log/slog`, which print unexported fields, print a pointer in its place, and `encoding/json` and the JSON handler of `log/slog` print `{}`. It exposes the identifier only through `JID()`, its kind (phone-number user, LID user or group), which `Normalize` records when it builds the chat, through `Kind()` and its validity through `Valid()`, and declares no `String`, `GoString`, `Format`, `MarshalText`, `MarshalJSON`, `MarshalBinary`, `LogValue` or `Error` method. Decisions drop the zero value from every scope, and grants never allow it. | I-1, I-2 | Compile time: the fields belong to the seal package, so the compiler refuses, in every other package and in package policy itself, a literal with fields in any syntactic form, a conversion from a look-alike type, a field write and a field's address. Fixtures added to package policy prove it for an explicit chat literal, an elided chat literal inside a named slice type, a grant literal inside a generic container, a grant built through a type parameter, a look-alike struct converted to a chat, a look-alike pointer written over a grant, a field write to a grant and to a chat's identifier, and a grant field's address, and a positive control proves the fixtures are compiled into the package. The seal-constructors architecture test, which reads every file outside the seal package's directory, test files included, accepts a reference to a function the seal package exports only as a direct call in the body of its one permitted function, outside any function literal: `NewChat` in `Normalize`; `NewReadGrant`, `NewWriteGrant` and `NewAdminGrant` in `DecideRead`, `DecideWrite` and `DecideAdmin`; `NewAdminCredential` in `ParseAdminCredential`; and the constant-time digest comparison `MatchAdminCredential` in `DecideAdmin`. It refuses renamed, dot and blank imports of the seal package, and another test requires its table to name exactly the functions and variables that the seal package exports. The guarantee is therefore: outside `internal/policy/internal/seal`, a valid chat can only come from `Normalize` and a grant only from a decision function, enforced by the compiler plus one constructor-reference rule, together with the rules that keep code from going around the type system: the bans on `unsafe`, which `go:linkname` needs to call a constructor by its symbol name, and on cgo, the reflection rule's ban on the unsafe-pointer functions and methods of `reflect` (row 3), and the walk's refusal of assembly, C and object files (above). Weakening any of these rules weakens the guarantee. Inside the seal package anything is possible: its code can build any value, so it holds only the types, their accessors, one constructor per type and the digest comparison, and review, not a test, keeps it that way. An architecture test refuses those methods on `seal.Chat`, and any alias of `seal.Chat` declared in the seal package, in every file of the seal package, test files included; the compiler refuses methods declared on `CanonicalChat` or any other alias outside it. A test checks that `fmt` with the verbs `%v`, `%+v`, `%#v`, `%s`, `%q`, `%x`, `%X` and `%d`, both `log/slog` handlers and `encoding/json` never print a valid chat's identifier. A table test of accepted and rejected identifiers; tests that every accepted result is canonical and normalises to itself, and of the allocations; a fuzz target (row 8). A test asserts that the zero value is denied for every client shape. | M0 for decisions and grants. M1, on `main`, for `Normalize` and the seal package. M2 for the SQL builder rejecting the zero value. |
| 5 | **One `IN` builder.** All chat-set SQL goes through one builder: an empty set yields `AND 0`, "all chats" yields `AND 1`. | I-1 | An architecture test rejects an `IN (` string literal, including one assembled by concatenation, in every file except the builder's. | Rule in force from M0; the builder arrives in M2. |
| 6 | **DTO firewall.** Response bodies are types of `internal/api/dto`, built field by field. Every field of a database row struct carries `json:"-"`. | I-5 | Compile time (the response interface has an unexported method, so a type declared elsewhere cannot be returned; a fixture proves it) and a run-time check in the encoder; a test that fails if a response type can produce the keys `raw`, `media_meta`, `sender_alt`, `seq` or `token`; a reflection test over the row structs. | M0 for response types and the key test. M2 for the row structs. |
| 7 | **Property tests.** Random archives (aliases, phone-number and LID pairs, revokes) crossed with random scopes, run through every scoped method and every MCP tool: every returned row is in scope, a revoked canary never appears in search or the change feed, and denied and missing responses are byte-identical. | I-1, I-3, I-4, I-8 | Property-based tests in CI. | M2 |
| 8 | **Fuzzing.** The admin token syntax check (never panics; anything accepted has the generated form). `Normalize` (never panics; accepts exactly what a regular-expression model of the forms in row 4 accepts, with the agent and device ranges applied, and returns the identifier the model derives; a rejection returns the zero value; an accepted input is at most 128 bytes, and its result has exactly one `@`, no `:` or `.` before it, one of the servers `s.whatsapp.net`, `lid` and `g.us` with the matching kind, and normalises to itself). The display and terminal sanitisers (output is valid UTF-8, holds none of the characters each one removes or replaces, and sanitising it again changes nothing; the terminal sanitiser keeps one character for each character or invalid byte of its input; text with nothing to change is returned unchanged). Strict JSON decoding of request bodies (refusals are only the three fixed errors; an accepted body is valid UTF-8 without a byte-order mark and at most 16384 bytes, one JSON object at most 8 levels deep whose keys are all lower-case `snake_case`, and what it decodes to is accepted again once re-encoded). Cursor opening, the full-text query builder (never an unquoted operator), ingest of crafted protocol messages; idempotency keys. | I-1, I-3, I-5, I-7, I-9 | Go native fuzzing; seed corpora replay in every test run, and CI fuzzes every target for 60 seconds. | M0 for the admin token check. M1, on `main`, for `Normalize`, the sanitisers and JSON decoding. M2 for the others; M3 for idempotency keys. |
| 9 | **`safego.Go` for every goroutine WaWarden starts.** It recovers a panic, logs the panic's type and stack (never its value) and counts it per goroutine name. WaWarden's HTTP handlers recover the same way and answer a fixed `500`. | I-5, I-9 | An architecture test rejects, in non-test files outside `internal/safego`, a `go` statement, `time.AfterFunc`, `context.AfterFunc`, `runtime.AddCleanup`, `runtime.SetFinalizer`, and any method named `Go` or `RegisterOnShutdown`. Goroutines that other standard-library code starts internally, such as `http.Server`'s per-connection goroutines, are not covered by the test; the handlers that run on them recover as described. | M0 |
| 10 | **Listener inventory.** Only `internal/listeners` opens sockets, only `internal/app` may use it, and only `internal/listeners` builds `http.Server` values, each with an explicit handler. `net.Listen` and related calls outside it, `http.ListenAndServe`, `http.Serve`, the package-level `http.Handle` and `http.HandleFunc`, and `http.DefaultServeMux` are banned, so a dependency that registers handlers on the default mux exposes nothing. | I-6 | `forbidigo` with type analysis and the architecture tests (static); a test that inspects the test process's open sockets and expects exactly the client, admin and health listeners; tests that `/debug/` paths answer `404` on every handler and on every live listener, with handlers registered on the default mux to prove the point. | M0 |
| 11 | **Golden files for MCP tools.** Tool names, descriptions and input schemas are compile-time constants compared against committed golden files. | I-5 | Golden-file test. | M2 |
| 12 | **Constant-time credential comparison.** The admin credential type cannot be compared with `==`, and the admin digest is compared only through `crypto/subtle`, in the seal package's `MatchAdminCredential`. | I-7 | Compile time (a fixture comparing two credentials fails to compile); an architecture test on comparisons in `internal/policy`, the seal package included, and `internal/token`. Because that test recognises calls by name, another architecture test refuses, in non-test files under those two packages, any declaration other than a label (receiver type parameters and import names included) named like a package the file imports or like a predeclared identifier such as `len`. | M0 |
| 13 | **One log writer.** `serve` wraps standard output in one `logx.Writer` before it logs anything, loads the master key right after the configuration and data directory checks, and keys the writer with the `log-redact` key derived from it. Every logger is built by `logx.New` on that writer: `app.New` takes it, so the app's logger, its alert logger, the HTTP servers' error logs and, once `app.New` has installed its logger, the panic reports of `safego` all write through it. A panic recovered before that, in the signal goroutine `main` starts first, is reported by Go's default logger on standard error. The writer replaces identifiers in `user@server` form with keyed pseudonyms, keeps a JSON line valid when an identifier follows an escape sequence, drops every line carrying XML, and writes each line whole under one lock; `logx.New` renders a value that is not a string, number, boolean, time, error or value with a `String` method as its length when it is a byte slice and as its type name otherwise. Text that code formats into a message is only pseudonymised and checked for XML. | I-5 | Compile time (`app.New` and `logx.New` take a `*logx.Writer`); an architecture test that refuses `slog.NewJSONHandler` and `slog.NewTextHandler` in non-test files outside `internal/logx` (a custom `slog.Handler` written elsewhere would pass it). Tests: every identifier syntax becomes the pseudonym an HMAC written out in the test gives, and every form of one identifier the same one; module paths, `<nil>` and `<autogenerated>` are kept; every XML pattern drops its line; a canary identifier passed as a string, byte slice, error, `Stringer`, struct, map, slice, array, pointer, chat, grant and `LogValuer`, as a message, a key and a group, through `With`, `WithGroup`, the `log` package bridge and direct writes never reaches the output, which fails when values are not rendered fail-closed; lines stay valid JSON next to escapes; concurrent loggers and writers produce whole lines; `serve` reports a goroutine named after an identifier under the pseudonym keyed by the data directory's master key. | M1, on `main` |
| 14 | **The keys directory belongs to `internal/keys`.** Only `internal/keys` creates, checks and reads `keys/` and the master key in the data directory (see [Startup refusals](#startup-refusals)), and it derives every purpose key with HKDF-SHA256. A master key value prints no key material through `fmt`, `encoding/json` or `log/slog`. | I-5, I-7 | An architecture test refuses, in non-test files outside `internal/keys`, a string literal outside import declarations, or a run of concatenated literals, with a path element named `keys`; a name assembled at run time passes it. Tests of every refusal, of 16 racing creators ending with one key and no temporary file, of derivation vectors checked against an HKDF written out in the test, and that every `fmt` verb, `encoding/json` and both `log/slog` handlers print no key material from a master key. | M1, on `main` |

Other tests that guard the invariants:

- A byte scan of `archive.db`, its journal and the full-text index's shadow tables
  for a revoked canary (I-8). M2.
- Tests that a startup refusal never repeats the configured value, and that the
  token `admin init` prints appears exactly once in its output (I-5, I-7). M0.
- Release builds prove that the `dev` build tag is absent: the build settings
  embedded in the binary show no `dev` tag, and the binary does not contain a
  marker string that only dev builds carry. A release build also refuses to start
  when it sees a `WAWARDEN_DEV_*` variable. M0.

## Startup refusals

`wawarden serve` refuses to start, with a stable reason code, when any check
below fails. [`configuration.md`](configuration.md#startup-refusals) lists the
reason codes of the checks in the current build.

| Condition | Status |
|---|---|
| The environment holds a `WAWARDEN_` variable the build does not implement. | M0 |
| The admin credential is supplied in plaintext (`WAWARDEN_ADMIN_TOKEN`). Only `WAWARDEN_ADMIN_TOKEN_SHA256` or `WAWARDEN_ADMIN_TOKEN_SHA256_FILE` (a SHA-256 in hexadecimal) is accepted. Without a hash, the admin listener is disabled, never open. | M0 |
| The admin hash is not 64 hexadecimal characters, both hash sources are set, or the hash file cannot be read or is not a regular file. | M0 |
| A release build sees a `WAWARDEN_DEV_*` variable. Only builds with the `dev` tag will contain the fake engine those variables configure. | M0 |
| `GOTRACEBACK` is set to anything other than unset, empty, `none` or `single`; numeric levels are refused too, `0` included. The named levels `all`, `system`, `crash` and `wer` print every goroutine's stack in crash output, and so does every other refused value once the service sets the `single` level itself. | M0 |
| A listen address is not an IP literal with a port from 1 to 65535, the health address is not loopback, or two enabled listeners share an address. | M0 |
| The process runs with a real or effective user ID of 0 without `--allow-root`. | M0 |
| The data directory cannot be created, is not a directory, is a symbolic link, is not owned by the process's effective user, or does not have mode `0700` exactly. | M0 |
| `keys/` in the data directory or the master key in it cannot be created or read, is a symbolic link or not a directory or regular file, is not owned by the process's effective user, or has the wrong mode (`keys/` exactly `0700`; the master key no permission for group or others and no setuid, setgid or sticky bit); or the master key does not hold exactly 32 bytes. | M1, on `main` |
| `session.db`, `archive.db` or `history/` is readable by group or others, or owned by another user. | M1 |
| Write-ahead logging is requested on a network filesystem, or `journal_mode` is `PERSIST` (it keeps deleted pages). | M1 |
| Protocol-library debug logging is enabled in a release build without `WAWARDEN_UNSAFE_DEBUG=<minutes>` (which reverts automatically and prints a banner). | M1 |
| Backups of `session.db` are configured without an age recipient. | M1 |
| A rate limit is zero or negative, or above its hard cap, without an `UNSAFE_` override. `0` never means unlimited. | M2 for read and search limits; M3 for send limits (global cap 60 per hour). |

### Pairing refusals (M1)

Pairing is refused while a device is already paired (`409`), while the owner's
phone number is missing or not in E.164 form, and beyond three attempts per hour.
After pairing, the linked account must be the owner's number; otherwise the engine
logs the new device out, wipes it and raises an alert, so that a pairing completed
from someone else's account cannot bind the gateway to that account.

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
| Request bodies are read only by a route handler, after authentication and the route's decision, through one strict JSON decoder: at most 16 KiB, exactly one `Content-Type` of `application/json`, valid UTF-8 without a byte-order mark, one object at most 8 levels deep with lower-case `snake_case` keys, no duplicate key once escapes are decoded, no unknown field and no trailing data; refusals are fixed `415`, `413` and `400` answers that never repeat the request. | M1, on `main`, for the decoder, which no route uses yet; M1 for the admin routes that will; M2 for client routes. |
| Secrets reach the service as `_FILE` paths, never as command-line arguments. The CLI reads the admin token from a `_FILE` path, standard input or a token command, never from command-line arguments or environment variables. | M0 for the admin hash file. M1 for the CLI. |
| The service holds no injected secret: the admin credential is a hash, client tokens are hashed in the database, and every other key is derived from a master key generated on first start (mode `0600`). | M0 for the admin credential. M1, on `main`, for the master key and the log pseudonym key. M2 for client tokens and the cursor and audit keys. |
| Database files are created with mode `0600`. | M1 |
| Every log line passes through one writer: WhatsApp identifiers in `user@server` form become `jid:` and 8 hexadecimal digits of an HMAC keyed from the master key, lines carrying XML are dropped, and log values of other types than strings, numbers, booleans, times, errors and values with a `String` method are written as their length (byte slices) or their type name. Protocol-library and MCP-library logs will reach the writer through adapters; tool arguments are never logged. | M1, on `main`, for the writer; M1 for the protocol-library adapter; M2 for the MCP library. |
| Text that the admin CLI prints from a server answer passes through a terminal sanitiser that replaces every control, format, line-separator and paragraph-separator character and every invalid byte with U+FFFD, so an answer cannot move the cursor, rewrite the screen, set a link or write the clipboard. | M1, on `main`, for the sanitiser; M1 for the CLI commands that use it. |
| Status, broadcast and newsletter traffic is dropped at ingest; raw protocol messages are not retained unless configured, and their media keys and message secrets are stripped first. | M1 |
| Never sends read receipts, presence or typing indicators. | M1 |
| No first contact (a DM with no prior inbound message) unless the client is allowed it; sends are paced, idempotent and budgeted per client, with `429` rather than queueing. | M3 |
| Disconnections from WhatsApp never exit the process; repeated restarts within a short window start the engine disconnected, to avoid reconnect storms. | M1 |
| Backups are encrypted to an age recipient; plaintext backups of `archive.db` alone require an explicit flag. | M1 |

## Residual risks

These remain at v1.0, after every control above is in place.

- **Unofficial protocol and account standing.** WhatsApp offers no official API for
  a personal account. WaWarden is a linked-device client built on an unofficial
  implementation of the multi-device protocol, and WhatsApp can restrict, log out
  or ban the account. The defaults reduce the signal (one linked session,
  read-mostly use, no receipts or presence, no reconnect storms and guarded
  pairing, M1; paced sends to explicit chats and no first contact, M3) but cannot
  remove it. The phone must also come online at least every 14 days, or WhatsApp
  logs out every linked device.
- **Protocol churn.** A protocol change can stop WhatsApp connectivity until a new
  release ships. Reads of the archive keep working. The engine refreshes the client
  version at runtime for the common case (M1).
- **Upstream dependency.** The protocol library has a single maintainer and no
  tagged releases. It will be pinned to an exact pseudo-version, and `go.sum`
  will pin its content: the go command checks every download against that hash
  and, with default settings, checks a new hash against the Go checksum database
  before recording it. Modules are never vendored (the build script, the CI
  `modules` job and an architecture test refuse a `vendor` directory), so a
  build needs the Go module proxy, or whatever `GOPROXY` names, and each update
  will be reviewed from the upstream commit log, diffstat and diff of the
  watched paths that a planned bump workflow will put into its pull request (M1,
  when the dependency is added).
- **Deletion residuals.** Backups taken before a message was revoked, edited or
  expired keep its old text until they leave retention. Filesystem blocks,
  snapshots and storage-level copies are outside the service's control.
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
  admin token can read the metrics (M0), start pairing (M1) and create clients
  (M2). The token does not expire. As of M0, failed attempts are only counted;
  events and an audit trail arrive in M1 and M2 (see
  [The admin token](#the-admin-token)). Keep the admin listener reachable from
  fewer places than the client listener. Metrics are readable only with the admin
  token, so a Prometheus scraper of `/metrics` holds the full admin credential,
  and scrape configurations are often widely readable. Treat any scrape
  configuration as holding the admin token, and prefer metrics on standard output
  in embedded metric format (M1), which need no token, for alerting.
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
  `user@server` form are recognised: a phone number on its own, or a
  protocol-library address of a user and a device without a server, passes
  unchanged, so code must not log one (M1, on `main`).
- **Panics in dependency goroutines.** A panic in a goroutine that a dependency
  starts (the protocol library, from M1) is not recovered: the process exits, and
  the Go runtime prints the panic value to standard error first. That value can
  carry chat data, so I-5 does not hold for crash output.
- **SQLite on a network filesystem** is outside SQLite's recommended
  configurations. The network-filesystem storage profile (rollback journal,
  exclusive locking, no write-ahead log, a single process; M1) reduces the risk; it
  does not remove it.
- **Delivery receipts** are sent automatically, so senders can see that a linked
  device received their messages.

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
   devices. WaWarden accepts history only from the primary device and keeps its own
   identity map from server-asserted fields (M1); unlink companion devices that
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
