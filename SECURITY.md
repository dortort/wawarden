# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability reporting
on this repository: open the **Security** tab and choose **Report a vulnerability**
(direct link: <https://github.com/dortort/wawarden/security/advisories/new>).
There is no security email address.

Do not open a public issue, pull request or discussion for a suspected
vulnerability.

A useful report contains:

- the affected version, or the image digest you ran;
- the configuration that matters (environment variables and flags; redact secrets);
- steps to reproduce, and what an attacker gains;
- whether you believe it is being exploited.

Do not include real message content, phone numbers, chat identifiers or tokens.
Use a test account you control and redact anything else. Never test against a
WhatsApp account or a WaWarden deployment that is not yours.

## What happens next

- **Acknowledgement within 72 hours** of the report.
- Triage and a fix are worked on in the private advisory. You are invited to it
  and can follow and comment on the fix.
- **Coordinated disclosure, 90 days by default**, counted from the report. The
  advisory is published when a fixed release is available or when the 90 days
  end, whichever comes first. The date can move earlier if the issue is being
  exploited, or later by agreement with you.
- A CVE is requested through the GitHub advisory when the issue warrants one. You
  are credited in the advisory unless you ask not to be.

## Supported versions

Only the latest minor release line receives security fixes, and nothing is
backported to an older line. [Supported versions](RELEASING.md#supported-versions)
in `RELEASING.md` is the canonical statement, with the
[compatibility promise](RELEASING.md#compatibility-promise) that makes upgrading
to the latest minor release safe. Upgrade to the latest release before
reporting, if you can.

Every binary and image links third-party Go modules, among them the WhatsApp
protocol library, the Signal protocol library, the SQLite driver and the age
encryption library. [`README.md`](README.md#licence) lists them with their
licences, and [`RELEASING.md`](RELEASING.md#licences) describes how releases
ship them. They are compiled into the binary, so a fix to one of them reaches a
deployment only through a new WaWarden release. The GPL-3.0-or-later licence
also lets you rebuild a release from its corresponding source with a fixed
module. The result then no longer matches the release's checksums, signature
and attestations.

## Scope

- [`docs/threat-model.md`](docs/threat-model.md) describes the security
  design, its invariants and the release that introduced each control.
- The reference documents describe the exact behaviour of the code at a commit
  or tag: [configuration](docs/configuration.md), [CLI](docs/cli.md),
  [API](docs/api.md), [observability](docs/observability.md) and
  [internals](docs/internals.md).
- [`docs/hardening.md`](docs/hardening.md) describes the deployment the design
  assumes.
- [`CHANGELOG.md`](CHANGELOG.md) names the release that introduced each part.

Reports against any supported release, against `main` and against the
documented design are welcome.

### In scope

- Bypass of per-client or per-chat scoping: reading, searching or receiving
  changes from a chat outside a client's read scope, sending to a chat outside its
  write chats, or learning whether an out-of-scope chat or message exists.
- Bypass of the send controls: a send to a direct chat that has never written
  to the account by a client not allowed first contact, a send past the
  per-client or global send limits or the pacer, one idempotency key causing
  two sends, or a send whose outcome is unknown being sent again.
- Forging, widening or reusing a read, write or admin grant outside the decision
  that mints it.
- Flaws in handling client tokens or the admin credential: generation, storage,
  comparison, expiry and revocation checks, or any path that logs, returns or
  persists one in plaintext.
- Leaks of message content or chat identities into logs, HTTP responses, MCP tool
  definitions, metrics or notifications.
- Ingest validation flaws: a message or protocol event from one chat (an edit,
  revoke, reaction or poll update) changing data in another chat, crafted protocol
  messages or history payloads that crash the process, corrupt the archive or
  re-key chats.
- Pairing flaws: linking, or keeping linked, an account other than the one in
  `WAWARDEN_OWNER_PHONE`, or a pairing code reaching anyone but the caller of
  the admin route that requested it.
- Message text left on disk after a revocation, an edit or an expiry beyond the
  exceptions the threat model lists.
- Traffic to WhatsApp that the threat model says WaWarden never causes, such as
  read receipts or presence, or a connection from a service without a paired
  device before pairing is requested.
- A notification webhook post that reaches an address its destination check
  refuses.
- A backup that can be decrypted without the age identity it was encrypted to,
  or a plaintext copy of a database that outlives a backup beyond the cases the
  threat model lists.
- Supply-chain and release-integrity issues: a release artifact that does not
  match its source, a provenance attestation or signature that verifies for the
  wrong workflow, a release that cannot be reproduced with the toolchain and
  builder documented in `RELEASING.md`, a release binary or image that contains
  the development-only fake engine, or weaknesses in the CI, release and
  dependency-update workflows.
- Anything that defeats an invariant documented in `docs/threat-model.md`.

### Out of scope

- Consequences of how the deployer exposes the listeners, such as a token
  intercepted on a network path left as plain HTTP, or a listener reachable from
  an untrusted network. WaWarden listens on loopback by default and speaks plain
  HTTP; network access control and TLS are the deployer's responsibility. A flaw
  that lets a caller without a valid token, or outside its token's scope, act or
  learn something is in scope wherever the listener is reachable from.
- Compromise of the host that runs WaWarden (root, or the same user ID as the
  service), or of the phone that owns the WhatsApp account.
- Actions WhatsApp takes against an account, such as restrictions, logouts or
  bans. Linking an unofficial client carries that risk; see the residual risks in
  the threat model.
- Dev builds (built with the `dev` build tag) and behaviour that requires a
  setting or flag the documentation marks as unsafe.
- Vulnerabilities in the third-party modules WaWarden links, such as
  `go.mau.fi/whatsmeow`, `go.mau.fi/libsignal`, `modernc.org/sqlite` or
  `filippo.io/age`, unless WaWarden's use of them makes the issue exploitable.
  Report those upstream.
- Everything else the threat model lists as out of scope: processes running as the
  same user on a client host, a compromised device linked to the same account,
  whoever holds both a backup and the age identity it was encrypted to, delivery
  receipts and the other traffic the protocol library sends by itself, deletion
  residuals in backups and filesystem blocks, and what an agent does with
  message text it reads.

## Verifying a release

Every release ships with build provenance attestations, a keyless signature on
the container image, SPDX SBOMs and checksums, and can be rebuilt bit for bit
from source. Releases since `v0.2.0` also ship the licence files of every
linked module.
[Verifying a release](RELEASING.md#verifying-a-release) in `RELEASING.md` lists
what each release contains, the commands and their prerequisites.
