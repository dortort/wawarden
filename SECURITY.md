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

Only the latest minor release line receives security fixes. While WaWarden is at
v0.x, each milestone release replaces the previous one; upgrade to the latest
release before reporting, if you can.

| Version | Supported |
|---|---|
| Latest minor line (`vX.Y.*` of the newest release) | Yes |
| Anything older | No |

## Scope

The security design, its invariants and the parts already implemented are
described in [`docs/threat-model.md`](docs/threat-model.md); the exact behaviour
of the current build is in [`docs/configuration.md`](docs/configuration.md).
WaWarden is under construction: some of the areas below have no code yet. Flaws in
the documented design are welcome as reports too.

### In scope

- Bypass of per-client or per-chat scoping: reading, searching or receiving
  changes from a chat outside a client's read scope, sending to a chat outside its
  write scope, or learning whether an out-of-scope chat or message exists.
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
- Supply-chain and release-integrity issues: a release artifact that does not
  match its source, a provenance attestation or signature that verifies for the
  wrong workflow, a release that cannot be reproduced with the toolchain and
  builder documented in `RELEASING.md`, or weaknesses in the CI and release
  workflows.
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
- Vulnerabilities in dependencies such as `go.mau.fi/whatsmeow`, unless WaWarden's
  use of them makes the issue exploitable. Report those upstream.
- Everything else the threat model lists as out of scope: processes running as the
  same user on a client host, a compromised device linked to the same account,
  readers of backups the operator chose not to encrypt, delivery receipts,
  deletion residuals in backups and filesystem blocks, and what an agent does with
  message text it reads.

## Verifying a release

Every release ships with build provenance attestations for the image and the
binaries, a keyless signature on the container image, SPDX SBOMs for the binaries
and checksums. With the toolchain and builder the release pins, it can be rebuilt
bit for bit from source. [`RELEASING.md`](RELEASING.md#verifying-a-release) lists
the commands and prerequisites.
