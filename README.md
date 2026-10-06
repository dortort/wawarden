# WaWarden

WaWarden is a self-hosted WhatsApp gateway. It links to one personal WhatsApp
account as a companion device and keeps that account's messages in a local
archive. When complete, it will expose that account to your own AI agents and
applications, over REST and MCP, with access scoped per client and per chat:
each client will get a token that may read, or read and write, only the chats
on its allowlist. The link and the archive (milestone M1) are on `main` and not
released yet; the REST and MCP interfaces are planned for M2 and M3; the latest
release, `v0.1.0`, is the M0 scaffold (see [Status](#status)).

It is not:

- **an official WhatsApp API**, and it is not affiliated with WhatsApp or Meta.
  It is built on an unofficial implementation of WhatsApp's multi-device protocol,
  and WhatsApp can restrict, log out or ban an account that links an unofficial
  client;
- **a hosted or multi-account service**: one process serves one account for its
  owner;
- **a network security layer**: it terminates no TLS, keeps no IP allowlist and
  has no user accounts;
- **a bulk messaging tool**: sending will be limited to the chats a client may
  write to, paced and budgeted, and with no first contact unless a client is
  explicitly allowed it (M3).

## Security posture

- **Tokens are bearer credentials.** Whoever holds a token and can reach the
  service has that token's access. Tokens are not bound to a host or a network
  address.
- **The service listens on loopback and speaks plain HTTP.** By default the client
  listener is on `127.0.0.1:8080`, the admin listener on `127.0.0.1:8082` and the
  health listener on `127.0.0.1:8081`.
- **Network access control and TLS are your responsibility as the deployer.** Put
  the service behind something that decides who may reach it and encrypts the
  path: a VPN or mesh network that forwards to loopback, a private subnet, or a
  reverse proxy that terminates TLS. **Never expose WaWarden to the public
  internet.** A listener bound to a non-loopback address makes the service log a
  warning at every start, whatever `WAWARDEN_LOG_LEVEL` is set to.
- **The admin listener is guarded by the admin token alone.** It is opened only
  when the token's SHA-256 is configured; the service never accepts the token
  itself in its configuration. Restrict who can reach it more tightly than the
  client listener.
- **The service refuses to start rather than run misconfigured**: on any
  `WAWARDEN_` variable it does not implement, a plaintext admin token, a
  non-loopback health address, running as root, a data directory, master key or
  message archive that is not private to the service's user, or the default
  storage profile on a network filesystem.
- **Browser-originated requests are refused.** Any request to the client or admin
  listener carrying an `Origin` or `Sec-Fetch-Site` header gets `403`.
- **Your WhatsApp account is at risk.** See the residual risks in the
  [threat model](docs/threat-model.md#residual-risks).
- **The protocol library sends some traffic by itself.** Once a device is
  linked, it acknowledges what it receives and sends WhatsApp, with no way to
  switch them off: delivery receipts in their inactive form, retry receipts for
  messages it cannot decrypt, acknowledgements, session telemetry after pairing,
  pre-key uploads and application-state fetches. WaWarden never sends read
  receipts, presence or typing indicators.

## Status

The latest release, `v0.1.0`, is milestone **M0**, a scaffold that makes no
connection to WhatsApp. `main` holds milestone **M1**, which is not released
yet: built from `main`, WaWarden links to the owner's WhatsApp account and
archives its messages, but serves no client API.

| Milestone | State | Scope |
|---|---|---|
| M0 | Released as `v0.1.0` | Configuration checks and startup refusals; the client, admin and health listeners; the admin token; Prometheus metrics; `healthcheck` and `version`; the policy core; the container image and verifiable releases. |
| M1 | On `main`, not released | The WhatsApp engine: pairing guarded by an account check, history sync, the session and the message archive in SQLite; `admin status`, `pair` and `reconnect`; notification events and a signed webhook; metrics on standard output; one encrypted backup per paired device. |
| M2 | Planned | Clients with per-chat read scopes and expiring tokens; the read API over REST (chats, messages, search, change feed) and MCP; the audit trail. |
| M3 | Planned | Sending over REST and MCP, with idempotency, pacing, per-client budgets and a first-contact rule; nightly encrypted backups with retention; the v1.0 documentation. |

What `main` does:

- **Start.** `wawarden serve` validates its configuration and refuses to start
  on anything it does not implement. It creates a master key in the data
  directory on first start; opens the message archive, `archive.db`, and the
  device store, `session.db`, in SQLite, each through one connection that holds
  an exclusive lock and whose settings it verifies; and opens the client and
  health listeners, and the admin listener when an admin token hash is
  configured. `/healthz` answers `200` only while it holds both locks; a second
  instance waits for them. See
  [the data directory](docs/configuration.md#data-directory).
- **Engine.** The WhatsApp engine, through the protocol library
  `go.mau.fi/whatsmeow`, links the account whose number is
  `WAWARDEN_OWNER_PHONE`, and no other, by a pairing code. It refreshes the
  protocol version, reconnects with backoff, and stops for the operator on a
  logout, a replaced session or a ban. It writes every message to a durable
  inbox before acknowledging it, applies edits, revocations, reactions and poll
  votes only inside their own chat, takes the history the phone sends under a
  size cap (`WAWARDEN_HISTORY_MAX_BYTES`), and removes revoked, edited and
  expired text from the disk. Without a paired device it makes no connection to
  WhatsApp until pairing is requested. See
  [the WhatsApp engine](docs/configuration.md#whatsapp-engine).
- **Admin.** The admin listener serves `GET /metrics` and the engine's status,
  pairing and reconnection to the admin token. `wawarden admin status`, `pair`
  and `reconnect` call them with the token taken from a file, standard input or
  a command, never from an argument or the environment. See
  [admin routes](docs/configuration.md#admin-routes).
- **Events and metrics.** Operational events such as `unpaired`,
  `disconnected`, `quarantine`, `backup_done` and `admin_mutation` are written
  as JSON lines on standard output and, with `WAWARDEN_NOTIFY_URL`, posted with
  an HMAC signature to a webhook (see
  [notifications](docs/configuration.md#notifications)). With
  `WAWARDEN_METRICS_EMF=1`, metrics are also written on standard output in
  CloudWatch's embedded metric format, which needs no token (see
  [metrics](docs/configuration.md#embedded-metric-format)).
- **Backups.** With `WAWARDEN_BACKUP_AGE_RECIPIENT`, an age public key, `serve`
  writes one encrypted backup of `archive.db` and `session.db` to `backups/`
  once a paired device's initial history sync has settled. It holds only the
  recipient and cannot read a backup; without it, no backup is taken. There is
  no schedule, retention or restore tool yet (see
  [backups](docs/configuration.md#backups)).
- **Logs.** Every log line passes through one writer that turns WhatsApp
  identifiers into pseudonyms keyed by the master key and drops lines carrying
  XML in the recognised shapes. The protocol library's debug output is
  discarded unless `WAWARDEN_UNSAFE_DEBUG` opens a window of a few minutes (see
  [logging](docs/configuration.md#pseudonyms-and-dropped-lines)).
- **Development builds.** Built with the `dev` tag, WaWarden can run a scripted
  fake engine in place of WhatsApp (`WAWARDEN_DEV_FAKE_ENGINE=1`) that pairs,
  connects and plays synthetic messages and history offline. Release builds
  refuse the variable and contain no part of the fake (see
  [the fake engine](docs/configuration.md#fake-engine) and
  [below](#with-the-fake-engine)).
- **Internally.** The policy core that mints read, write and admin grants;
  route registration that requires a policy class; the strict normaliser for
  WhatsApp chat identifiers (phone-number users, LID users and groups), which
  the engine applies to what it ingests and which is the only source of a chat
  that a grant can allow, with a fuzz target (see the
  [threat model](docs/threat-model.md#secure-by-construction), row 4);
  sanitisers for display text and terminal output; a strict JSON decoder for
  request bodies; and the architecture tests and lint rules that enforce them.

Not in this build: the client listener has no routes and no client tokens and
refuses every request, with `401` for anything that reaches authentication;
no client can read the archive (M2) or send a message (M3).

## Quick start

These steps start the service without linking an account; to link one, continue
with [First run with a WhatsApp account](#first-run-with-a-whatsapp-account).
The commands below use the default ports `8080`, `8081` and `8082` on
`127.0.0.1`. If one is taken, choose other addresses with `WAWARDEN_LISTEN`,
`WAWARDEN_HEALTH_LISTEN` or `WAWARDEN_ADMIN_LISTEN` (see the
[configuration reference](docs/configuration.md)) and apply them to every command,
not only `serve`: export the variables in the shell, so that `healthcheck` reads
the same `WAWARDEN_HEALTH_LISTEN`, and change the ports in the `curl` URLs to
match.

### From source

You need git, curl and Go. The build uses the Go version on the `toolchain` line
of `go.mod`; with its default settings, the `go` command (1.21 or later) downloads
that version when the installed one is older. Dependencies are never vendored:
once the module has any, the `go` command downloads them as well, through the Go
module proxy by default, and checks each one against `go.sum`.

```sh
git clone https://github.com/dortort/wawarden
cd wawarden
go build -trimpath -o wawarden ./cmd/wawarden
./wawarden version
```

Generate an admin token. It is printed once, together with its SHA-256; the
service is configured with the hash only. `mktemp -d` creates a directory that
only you can read:

```sh
demo=$(mktemp -d)
./wawarden admin init > "$demo/admin.txt"
sed -n 's/^sha256: //p' "$demo/admin.txt" > "$demo/admin.sha256"
sed -n 's/^token: /Authorization: Bearer /p' "$demo/admin.txt" > "$demo/admin.header"
```

Run the service with a data directory. It creates the directory with mode `0700`
and refuses to start as root:

```sh
WAWARDEN_DATA_DIR="$demo/data" \
WAWARDEN_ADMIN_TOKEN_SHA256_FILE="$demo/admin.sha256" \
  ./wawarden serve &
```

It writes JSON log lines to standard output; wait for the line with
`"event":"ready"`. Then check its health, send a client request, and read the
metrics with the admin token:

```sh
./wawarden healthcheck && echo healthy
curl -i http://127.0.0.1:8080/v1/me
curl -H @"$demo/admin.header" http://127.0.0.1:8082/metrics
```

This build has no client routes and no client tokens yet (they come with M2), so
every client request is refused:

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

The metrics are the failed-authentication counters, the build information and
the engine's counters and gauges:

```text
# HELP wawarden_admin_auth_failures_total Failed admin authentications.
# TYPE wawarden_admin_auth_failures_total counter
wawarden_admin_auth_failures_total 0
# HELP wawarden_auth_failures_total Failed client authentications.
# TYPE wawarden_auth_failures_total counter
wawarden_auth_failures_total 1
# HELP wawarden_build_info Build metadata of the running binary.
# TYPE wawarden_build_info gauge
wawarden_build_info{version="dev",revision="<commit>",dev="false"} 1
# HELP wawarden_connected 1 while the engine is connected to WhatsApp, 0 otherwise.
# TYPE wawarden_connected gauge
wawarden_connected 0
# HELP wawarden_messages_ingested_total Messages, reactions and poll updates stored in the archive, live and from history.
# TYPE wawarden_messages_ingested_total counter
wawarden_messages_ingested_total 0
# HELP wawarden_paired 1 while a WhatsApp device is paired, 0 otherwise.
# TYPE wawarden_paired gauge
wawarden_paired 0
# HELP wawarden_policy_denials_total Client requests that the client's grant did not allow.
# TYPE wawarden_policy_denials_total counter
wawarden_policy_denials_total 0
# HELP wawarden_sends_rejected_total Sends refused by a scope, budget or rate limit.
# TYPE wawarden_sends_rejected_total counter
wawarden_sends_rejected_total 0
```

The admin commands call the admin listener with the token from a file; this one
prints the engine's state and the archive's counts. With no device paired, the
state is `unpaired` and the service makes no connection to WhatsApp:

```sh
sed -n 's/^token: //p' "$demo/admin.txt" > "$demo/admin.token"
./wawarden admin status --token-file "$demo/admin.token"
```

Stop the service; it shuts down gracefully and exits `0`. Then delete the
temporary directory, which holds the admin token and the data directory:

```sh
kill %1
wait
rm -rf "$demo"
```

### With the fake engine

A development build can run a scripted fake in place of WhatsApp, to try
pairing, ingest and the admin commands without an account and without
contacting WhatsApp. Build it with the `dev` tag and set up a token as above:

```sh
go build -trimpath -tags dev -o wawarden-dev ./cmd/wawarden
fake=$(mktemp -d)
./wawarden-dev admin init > "$fake/admin.txt"
sed -n 's/^sha256: //p' "$fake/admin.txt" > "$fake/admin.sha256"
sed -n 's/^token: //p' "$fake/admin.txt" > "$fake/admin.token"
```

Run it with the fake engine and a synthetic owner number. It logs the warnings
`dev_build` and `fake_engine`, opens no `session.db`, and starts unpaired:

```sh
WAWARDEN_DATA_DIR="$fake/data" \
WAWARDEN_ADMIN_TOKEN_SHA256_FILE="$fake/admin.sha256" \
WAWARDEN_OWNER_PHONE=+15550100009 \
WAWARDEN_DEV_FAKE_ENGINE=1 \
  ./wawarden-dev serve &
```

Once it logs `"event":"ready"`, pair it. The fake answers with the fixed code
`FAKE-C0DE`, pairs the owner a moment later, connects, and plays its script:
live messages that exercise the ingest rules, a dropped connection, and history
blobs, two of which are built to be quarantined:

```sh
./wawarden-dev admin pair --token-file "$fake/admin.token"
sleep 15
./wawarden-dev admin status --token-file "$fake/admin.token"
```

The status then shows `state: connected`, `chats: 3`, `messages: 17` and
`history blobs quarantined: 2`; the log shows the two `quarantine` events, and
`/metrics` the drops by reason. `WAWARDEN_DEV_FAKE_ENGINE=wrong_account` pairs
another number instead, which the engine rejects and logs out. Stop the service
and remove the directory as above (`kill %1`, `wait`, `rm -rf "$fake"`).

The fake exists only in development builds: release builds refuse
`WAWARDEN_DEV_FAKE_ENGINE`, and the release build checks that no release binary
holds the fake's code (see [the fake engine](docs/configuration.md#fake-engine)).

### From the container image

Images are published to `ghcr.io/dortort/wawarden` by the release workflow only.
Take the image index digest from a release's notes, and verify the image as
[`RELEASING.md`](RELEASING.md#verifying-a-release) describes. The latest
release, `v0.1.0`, is M0: its image has no WhatsApp engine and no
`admin status`, `pair` or `reconnect`, so until M1 is released, use the steps
from source to run `main`. The example below works with either.

```sh
image=ghcr.io/dortort/wawarden@sha256:<digest>
demo=$(mktemp -d)
docker run --rm "$image" admin init > "$demo/admin.txt"
sed -n 's/^sha256: //p' "$demo/admin.txt" > "$demo/admin.sha256"
sed -n 's/^token: /Authorization: Bearer /p' "$demo/admin.txt" > "$demo/admin.header"

docker volume create wawarden-data
docker run -d --name wawarden --read-only --stop-timeout 15 \
  --mount type=volume,src=wawarden-data,dst=/data \
  -e WAWARDEN_ADMIN_TOKEN_SHA256="$(cat "$demo/admin.sha256")" \
  -e WAWARDEN_LISTEN=0.0.0.0:8080 \
  -e WAWARDEN_ADMIN_LISTEN=0.0.0.0:8082 \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8082:8082 \
  "$image"

docker exec wawarden /wawarden healthcheck && echo healthy
curl -i http://127.0.0.1:8080/v1/me
curl -H @"$demo/admin.header" http://127.0.0.1:8082/metrics
docker stop wawarden

docker rm wawarden
docker volume rm wawarden-data
rm -rf "$demo"
```

The last three commands remove the container, its data volume and the temporary
directory that holds the admin token.

The image runs as `65532:65532` with `serve` as its default command. `/data` is
the only path the service writes and the only one that must be writable; a new
named volume mounted there gets the right owner and mode. Run the container with
a read-only root filesystem, as the example does with `--read-only`, so that
`/data` is also the only writable path.

Inside the container, a listener on `127.0.0.1` cannot be reached through a
published port, so the example binds the client and admin listeners to `0.0.0.0`
and publishes them on the host's loopback only. The service logs a
`listener_not_loopback` warning for each of them, and other containers on the
same Docker network can reach them. The image has no shell, so `docker run
--health-cmd` cannot run its health check; use the exec form
`["/wawarden","healthcheck"]` in Compose or Kubernetes.
[The configuration reference](docs/configuration.md#container-image) describes
the full container contract.

## Documentation

| Document | Content |
|---|---|
| [`docs/configuration.md`](docs/configuration.md) | Every environment variable, flag, subcommand, exit code, startup refusal, listener, log event and metric of the current build, and the container contract. |
| [`docs/threat-model.md`](docs/threat-model.md) | Assets, actors, trust boundaries, invariants, the mechanisms that enforce them with their milestones, and residual risks. |
| [`SECURITY.md`](SECURITY.md) | How to report a vulnerability, supported versions and scope. |
| [`RELEASING.md`](RELEASING.md) | How releases are built, what they contain, and how to verify and reproduce one. |

## Running the tests

`go test -race ./...` runs the suite, and `go test -race -tags dev ./...` runs it
against a development build, which adds an end-to-end test that runs `serve`
with the fake engine and pairs it through the admin CLI. No test contacts WhatsApp: the protocol adapter is
tested with hand-built events, protobuf fixtures and injected transports, and its
dialer, like every transport it builds whatever dial function it is handed,
refuses every connection inside a test binary. `hack/offline-test.sh`
proves it: it fetches the modules that `go.sum` pins, then runs both suites in a
container that has no network, so it needs Docker.

## Verifying a release

Every release is built from `main` by a workflow that needs the maintainer's
approval, rebuilt independently and compared bit for bit, attested and signed.
[`RELEASING.md`](RELEASING.md#verifying-a-release) gives the commands to verify the
image, the binaries and their SBOMs, and to reproduce a release from source.

## Licence

Copyright (C) 2026 Francis Eytan Dortort.

From milestone M1 on, WaWarden is licensed under the GNU General Public License,
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
| Apache-2.0 | `github.com/petermattis/goid` |
| ISC | `github.com/coder/websocket` |
| MIT | `github.com/beeper/argo-go`, `github.com/dustin/go-humanize`, `github.com/elliotchance/orderedmap/v3`, `github.com/mattn/go-colorable`, `github.com/mattn/go-isatty`, `github.com/rs/zerolog`, `github.com/vektah/gqlparser/v2` |
| BSD-3-Clause | the Go standard library, `filippo.io/edwards25519`, `github.com/google/uuid`, `github.com/remyoudompheng/bigfft`, `golang.org/x/crypto`, `golang.org/x/exp`, `golang.org/x/net`, `golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/text`, `google.golang.org/protobuf`, and the SQLite driver `modernc.org/sqlite` with `modernc.org/libc`, `modernc.org/mathutil` and `modernc.org/memory`, which also carry the licences of the C code they translate, SQLite's public-domain dedication among them |

Every release ships their licence files, and WaWarden's, as
`wawarden_<version>_licenses.tar.gz` and under
`/licenses` in the image; [`RELEASING.md`](RELEASING.md#licences) describes how
they are collected.
