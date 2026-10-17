<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/hero-dark.svg">
    <img alt="WaWarden — a self-hosted WhatsApp gateway for your own AI agents" src="docs/assets/hero-light.svg" width="100%">
  </picture>
</h1>

<p align="center">
  <a href="https://wawarden.dortort.dev/">Documentation</a> ·
  <a href="docs/getting-started.md">Getting started</a> ·
  <a href="https://github.com/dortort/wawarden/releases">Releases</a>
</p>

WaWarden is a self-hosted WhatsApp gateway. It links to one personal WhatsApp
account as a companion device and keeps that account's messages in a local
archive. It exposes that archive to your own AI agents and applications, over
REST and MCP, with access scoped per client and per chat. Each client gets a
token that may read only the chats on its allowlist, and write only to the
chats allowed for it.

It is not:

- **an official WhatsApp API**, and it is not affiliated with WhatsApp or Meta.
  It is built on an unofficial implementation of WhatsApp's multi-device
  protocol. WhatsApp can restrict, log out or ban an account that links an
  unofficial client;
- **a hosted or multi-account service**: one process serves one account for its
  owner;
- **a network security layer**: it terminates no TLS, keeps no IP allowlist and
  has no user accounts;
- **a bulk messaging tool**: sending is limited to the chats a client may
  write to, paced and budgeted. It makes no first contact unless a client is
  explicitly allowed it (see [the ban-risk notes](docs/ban-risk.md)).

## Contents

- [Documentation](#documentation)
- [Security posture](#security-posture)
- [Install](#install)
- [Getting started](#getting-started)
- [What it does](#what-it-does)
- [Releases](#releases)
- [Running the tests](#running-the-tests)
- [Verifying a release](#verifying-a-release)
- [Licence](#licence)

## Documentation

The documentation is published at **<https://wawarden.dortort.dev/>**.
The same pages are in this repository:

| Document | Content |
|---|---|
| [Getting started](docs/getting-started.md) | Link a real account: prerequisites, secrets, HTTPS, pairing, the first backup and the first client. |
| [Configuration](docs/configuration.md) | Environment variables, startup refusals, the data directory, storage, backups and the container image. |
| [CLI](docs/cli.md) | Every subcommand, its flags and the exit codes. |
| [API](docs/api.md) | The client API, the admin routes, the MCP tools, registering with Claude Code, and responses. |
| [Observability](docs/observability.md) | Logs and pseudonyms, events, the webhook, metrics, EMF and the audit chain. |
| [Internals](docs/internals.md) | The WhatsApp engine, ingest, delivery, history sync, design rationale and byte layouts. |
| [Agents](docs/agents.md) | How an agent should treat what it reads, page, send, retry and stop, and how to scope its token. |
| [Hardening](docs/hardening.md) | How to deploy the service: network patterns, egress, containers, systemd, secrets, backups and alarms. |
| [Ban risk](docs/ban-risk.md) | What WhatsApp says about unofficial clients, what WaWarden does, and what the operator must do. |
| [Runbook](docs/runbook.md) | Routine operations and incidents: clients, tokens, keys, backups, restores, upgrades and disconnects. |
| [Threat model](docs/threat-model.md) | Assets, actors, trust boundaries, invariants, the mechanisms that enforce them, and residual risks. |
| [Glossary](docs/glossary.md) | The terms these documents use. |
| [`SECURITY.md`](SECURITY.md) | How to report a vulnerability, supported versions and scope. |
| [`RELEASING.md`](RELEASING.md) | How releases are built, verified and reproduced, and what a version promises. |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Building, testing, linting, the architecture tests and commit style. |
| [`CHANGELOG.md`](CHANGELOG.md) | What each release introduced. |

## Security posture

- **Tokens are bearer credentials.** Whoever holds a token and can reach the
  service has that token's access, from any host or address.
- **Loopback and plain HTTP; TLS and access control are yours.** The listeners
  bind `127.0.0.1` and speak plain HTTP. Put them behind a VPN, a private
  subnet or a TLS proxy, and never expose them to the public internet (see
  [the hardening guide](docs/hardening.md)). The admin listener opens only when
  the admin token's SHA-256 is configured, and that token alone guards it.
- **Misconfiguration stops the start.** An unknown `WAWARDEN_` variable, a
  plaintext admin token, running as root or a data directory that is not
  private are refused (see
  [startup refusals](docs/configuration.md#startup-refusals)).
- **Browser-originated requests are refused.** A request carrying an `Origin`
  or `Sec-Fetch-Site` header gets `403`.
- **Your WhatsApp account is at risk.** Read the
  [residual risks](docs/threat-model.md#residual-risks) and
  [the ban-risk notes](docs/ban-risk.md) before linking an account.
- **The protocol library sends some traffic by itself**, such as receipts and
  pre-key uploads (see [residual risks](docs/threat-model.md#residual-risks)).
- **A message refused or interrupted before it is stored is lost**, not
  redelivered (see [residual risks](docs/threat-model.md#residual-risks)).
- **Attachments are third-party content.** Downloads are off by default. When
  they are on, text in an image, speech or a document can steer an agent that
  reads it, and a copy an MCP host saves of an attachment is outside
  WaWarden's revocation and expiry (see
  [clients and agents](docs/threat-model.md#clients-and-agents)).

## Install

Releases ship static binaries for Linux on `amd64` and `arm64`, binaries for
macOS 13 Ventura or later on Apple silicon and Intel from `v1.1.0` on, and a
multi-arch Linux container image, `ghcr.io/dortort/wawarden`.

On macOS, and on a Linux machine that does not run the service, install with
[Homebrew](https://brew.sh):

```sh
brew install dortort/tap/wawarden
```

Homebrew checks the release's prebuilt binary against the release's
`SHA256SUMS`, and the tap publishes the formula for a release only after
checking the release's provenance. No `xattr` step is needed on macOS, and
`brew upgrade wawarden` installs newer releases.
[Homebrew](docs/getting-started.md#homebrew) shows how to check the installed
file's provenance yourself.

On the host that runs the service, download the latest Linux binary, check
it and install it, with [`gh`](https://cli.github.com) and no GitHub account:

```sh
cd "$(mktemp -d)"
version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/dortort/wawarden/releases/latest)
version=${version##*/}
case $(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
binary=wawarden_${version}_linux_$arch
bundle=wawarden_${version}_provenance.sigstore.json
gh release download "$version" --repo dortort/wawarden --pattern "$binary" &&
  gh release download "$version" --repo dortort/wawarden --pattern SHA256SUMS --pattern "$bundle" &&
  sha256sum --ignore-missing -c SHA256SUMS &&
  gh attestation verify "$binary" --repo dortort/wawarden --bundle "$bundle" \
    --cert-identity https://github.com/dortort/wawarden/.github/workflows/release.yml@refs/heads/main &&
  sudo install -m 0755 "$binary" /usr/local/bin/wawarden &&
  wawarden version
```

On macOS without Homebrew, see [macOS](docs/getting-started.md#macos).

With Go, `CGO_ENABLED=0 go install -trimpath github.com/dortort/wawarden/cmd/wawarden@latest`
builds and installs the latest release; see
[Install](docs/getting-started.md#with-go-install).

[Install](docs/getting-started.md#install) in Getting started covers the
supported platforms, Homebrew, downloading without `gh`, the image, which has
no `latest` tag, building from source and the service user.

## Getting started

To link a real WhatsApp account, follow
[Getting started](docs/getting-started.md). It lists the prerequisites, then
walks through the secrets, the first start, HTTPS, pairing, the first backup
and the first client. Read the [residual risks](docs/threat-model.md#residual-risks)
first: WhatsApp can ban an account that links an unofficial client.

### Try it without an account

A development build can run a scripted fake engine in place of WhatsApp. It
pairs, connects and plays synthetic messages and history, offline. You need
git, curl and Go 1.21 or later, which downloads the toolchain that `go.mod`
names when the installed one is older; with `GOTOOLCHAIN=local`, as some
distributions set it, you need Go 1.26.0 or later.
Run these steps as a user other than root.

Build the development binary and generate an admin token. `mktemp -d` creates
a directory that only you can read:

```sh
git clone https://github.com/dortort/wawarden
cd wawarden
go build -trimpath -tags dev -o wawarden-dev ./cmd/wawarden

dir=$(mktemp -d)
./wawarden-dev admin init > "$dir/admin.txt"
sed -n 's/^sha256: //p' "$dir/admin.txt" > "$dir/admin.sha256"
sed -n 's/^token: //p' "$dir/admin.txt" > "$dir/admin.token"
sed -n 's/^token: /Authorization: Bearer /p' "$dir/admin.txt" > "$dir/admin.header"
```

Start the service with the fake engine and a synthetic owner number. Its JSON
log goes to a file; wait for the `ready` line:

```sh
WAWARDEN_DATA_DIR="$dir/data" \
WAWARDEN_ADMIN_TOKEN_SHA256_FILE="$dir/admin.sha256" \
WAWARDEN_OWNER_PHONE=+15550100009 \
WAWARDEN_DEV_FAKE_ENGINE=1 \
  ./wawarden-dev serve > "$dir/serve.log" 2>&1 &
until grep -q '"event":"ready"' "$dir/serve.log"; do sleep 1; done
```

The log warns `fake_engine` and `dev_build`, and the engine starts `unpaired`.
Pair it. The fake answers with the fixed code `FAKE-C0DE`, pairs the owner,
connects and plays its script. Two of its history blobs are built to be
quarantined, so wait for them:

```sh
./wawarden-dev admin pair --token-file "$dir/admin.token"
until ./wawarden-dev admin status --token-file "$dir/admin.token" |
  grep -q 'history blobs quarantined: 2'; do sleep 1; done
./wawarden-dev admin status --token-file "$dir/admin.token"
```

The status shows `state: connected`, `chats: 3`, `messages: 24` and
`history blobs quarantined: 2`. The metrics agree:

```sh
curl -s -H @"$dir/admin.header" http://127.0.0.1:8082/metrics |
  grep -E '^wawarden_(connected|messages_ingested_total|ingest_quarantined_total)'
```

```text
wawarden_connected 1
wawarden_ingest_quarantined_total{queue="history"} 2
wawarden_messages_ingested_total 24
```

List the chats, then create a client that may read the synthetic group.
`--read` accepts a raw chat identifier, a `+E.164` number or a chat's `ref`.
The command prints the client and its token once, and a reminder on standard
error. A chat the archive has not seen yet is accepted, with a warning on
standard error:

```sh
./wawarden-dev admin chats list --token-file "$dir/admin.token"
./wawarden-dev admin clients create --token-file "$dir/admin.token" \
  --name demo --read 120363000000000021@g.us > "$dir/client.txt"
sed -n 's/^token: /Authorization: Bearer /p' "$dir/client.txt" > "$dir/client.header"
```

Read through the REST API. `/v1/chats` lists the group by its reference, which
the next request takes:

```sh
curl -s -H @"$dir/client.header" http://127.0.0.1:8080/v1/me
curl -s -H @"$dir/client.header" http://127.0.0.1:8080/v1/chats
ref=$(curl -s -H @"$dir/client.header" http://127.0.0.1:8080/v1/chats |
  grep -o '"id":"[0-9a-f]\{32\}"' | head -n 1 | cut -d '"' -f 4)
curl -s -H @"$dir/client.header" "http://127.0.0.1:8080/v1/chats/$ref/messages?limit=5"
curl -s -H @"$dir/client.header" -G --data-urlencode 'q=Alice' http://127.0.0.1:8080/v1/search
```

Read through MCP with the same token. This call lists the chats:

```sh
curl -s -H @"$dir/client.header" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_chats","arguments":{}}}' \
  http://127.0.0.1:8080/mcp
```

To try it from Claude Code, register the endpoint. The demo token goes in a
shell variable whose name does not start with `WAWARDEN_`. Never export a
`WAWARDEN_TOKEN` in the shell that runs `serve`: the service refuses any
`WAWARDEN_` variable it does not implement.

```sh
demo_token=$(sed -n 's/^token: //p' "$dir/client.txt")
claude mcp add --transport http whatsapp-demo http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer $demo_token"
```

`/mcp` in a Claude Code session then lists five tools: `get_changes`,
`get_chat`, `get_messages`, `list_chats` and `search_messages`.

Clean up. The service stops gracefully and exits `0`:

```sh
claude mcp remove whatsapp-demo
kill %1; wait
unset demo_token
rm -rf "$dir"
```

`WAWARDEN_DEV_FAKE_ENGINE=wrong_account` pairs another number instead, which
the engine rejects and logs out. Release builds refuse
`WAWARDEN_DEV_FAKE_ENGINE` and contain no part of the fake (see
[the fake engine](docs/internals.md#fake-engine)).

## What it does

- **Start.** `wawarden serve` checks its configuration, creates a master key
  and opens the archive and the device store (see
  [the data directory](docs/configuration.md#data-directory)).
- **Engine.** It links only the account whose number is `WAWARDEN_OWNER_PHONE`,
  by a pairing code, and reconnects by itself (see
  [the WhatsApp engine](docs/internals.md#whatsapp-engine)).
- **Ingest.** It stores every message before acknowledging it, and removes
  revoked, edited and expired text from the disk (see
  [ingest](docs/internals.md#ingest)).
- **Clients.** `wawarden admin clients create` makes a scoped, expiring client
  token (see [admin clients](docs/cli.md#admin-clients-and-admin-chats) and
  [the runbook](docs/runbook.md#clients)).
- **Reading.** Clients read their chats over REST or as MCP tools, in sealed
  pages, within per-client budgets (see [the client API](docs/api.md#client-api)
  and [MCP](docs/api.md#mcp)).
- **Attachments.** Off by default. With `WAWARDEN_MEDIA_MAX_BYTES` set, a
  client created with `allow_media` downloads the attachment of a photo,
  video, voice note or other audio, document or sticker over REST, or through
  the MCP tool `get_attachment`, inline when it is small enough or else in
  8 MiB parts. It is fetched only when the client asks, within per-client and
  global budgets, with images re-encoded without their metadata, and the
  operator can pause downloads at once (see
  [attachments](docs/api.md#attachments),
  [get_attachment](docs/api.md#get_attachment) and
  [pausing downloads](docs/api.md#pausing-downloads)). A revocation removes
  the whole message, its attachment reference included.
- **Sending.** A client with write chats sends text, once per idempotency key,
  behind a pacer and send budgets (see [sending](docs/api.md#sending) and
  [ban risk](docs/ban-risk.md)).
- **Audit.** Every client change and request joins a hash-chained audit table
  that `wawarden audit verify` checks (see
  [the audit chain](docs/observability.md#audit-chain)).
- **Events and metrics.** Events go to standard output and a signed webhook;
  metrics to Prometheus and, optionally, EMF (see
  [notifications](docs/observability.md#notifications) and
  [metrics](docs/observability.md#metrics)).
- **Backups.** Nightly backups are encrypted to an age recipient and checked
  with `wawarden backup verify` (see
  [backups](docs/configuration.md#backups)).
- **Logs.** One writer redacts tokens and turns WhatsApp identifiers into
  pseudonyms (see
  [pseudonyms](docs/observability.md#pseudonyms-and-dropped-lines)).

Not built: sending anything but text, optionally as a reply in the same chat
(no media, reactions, edits or deletions); fetching view-once media; asking
the owner's phone to upload again an attachment WhatsApp no longer holds
(media retry); a restore command, since a backup is restored by hand (see
[the runbook](docs/runbook.md#restoring-a-backup)).

## Releases

Each release on the [Releases](https://github.com/dortort/wawarden/releases)
page ships Linux binaries for `amd64` and `arm64`, and from `v1.1.0` on macOS
binaries for Apple silicon and Intel, with `SHA256SUMS`, SBOMs, a provenance
bundle and, from `v0.2.0` on, the licence archive, and its notes
give the container image's digest. See [`CHANGELOG.md`](CHANGELOG.md) for what
each release introduced; `wawarden version` prints a binary's version.

## Running the tests

`go test -race ./...` and `go test -race -tags dev ./...` run the suites. No
test contacts WhatsApp; [`CONTRIBUTING.md`](CONTRIBUTING.md) covers lint, the
architecture tests, fuzzing and the offline test.

## Verifying a release

Every release is built from `main` by a workflow that needs the maintainer's
approval, rebuilt independently and compared bit for bit, attested and signed.
[`RELEASING.md`](RELEASING.md#verifying-a-release) gives the commands to verify
the image, the binaries and their SBOMs, and to reproduce a release from source.

## Licence

Copyright (C) 2026 Francis Eytan Dortort.

From `v0.2.0` on, WaWarden is licensed under the GNU General Public License,
version 3 or (at your option) any later version (`GPL-3.0-or-later`); the
licence text is in [`LICENSE`](LICENSE). `v0.1.0` was released under the MIT
licence, which still applies to that release. The licence changed because
the WhatsApp protocol library that WaWarden links depends on a component
licensed under GPL-3.0.

The binaries link the Go standard library and third-party Go modules under
their own licences:

| Licence | Linked modules |
|---|---|
| GPL-3.0 | `go.mau.fi/libsignal` |
| MPL-2.0 | `go.mau.fi/whatsmeow`, `go.mau.fi/util` |
| Apache-2.0 | `github.com/petermattis/goid`, and the MCP library `github.com/modelcontextprotocol/go-sdk`, whose code contributed before its move to Apache-2.0 stays under MIT unless its authors relicensed it (its licence file carries both texts) |
| ISC | `github.com/coder/websocket` |
| MIT | `github.com/beeper/argo-go`, `github.com/dustin/go-humanize`, `github.com/elliotchance/orderedmap/v3`, `github.com/mattn/go-colorable`, `github.com/mattn/go-isatty`, `github.com/rs/zerolog`, `github.com/vektah/gqlparser/v2`, and with the MCP library `github.com/google/jsonschema-go`, `github.com/segmentio/asm`, `github.com/segmentio/encoding`, and `github.com/ncruces/go-strftime` (macOS binaries only) |
| BSD-3-Clause | the Go standard library, the backup encryption `filippo.io/age` with `filippo.io/hpke`, `filippo.io/edwards25519`, `github.com/google/uuid`, `github.com/remyoudompheng/bigfft`, `github.com/yosida95/uritemplate/v3`, `golang.org/x/crypto`, `golang.org/x/exp`, the image library `golang.org/x/image`, `golang.org/x/net`, `golang.org/x/oauth2`, `golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/text`, `golang.org/x/time`, `google.golang.org/protobuf`, and the SQLite driver `modernc.org/sqlite` with `modernc.org/libc`, `modernc.org/mathutil` and `modernc.org/memory`, which also carry the licences of the C code they translate, SQLite's public-domain dedication among them |

The macOS binaries also link `github.com/ncruces/go-strftime`, under MIT; the
Linux binaries do not.

From `v0.2.0` on, every release ships their licence files, and WaWarden's, as
`wawarden_<version>_licenses.tar.gz` and under `/licenses` in the image; the
archive and the image's copy cover every binary of the release.
[`RELEASING.md`](RELEASING.md#licences) describes how they are collected.
