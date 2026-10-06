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
  non-loopback health address, running as root, a data directory or anything in
  it (the master key, the message archive, the device store, their journals,
  `keys/`, `history/` or `backups/`) that is not private to the service's user,
  or the default storage profile on a network filesystem.
- **Browser-originated requests are refused.** Any request to the client or admin
  listener carrying an `Origin` or `Sec-Fetch-Site` header gets `403`.
- **Your WhatsApp account is at risk.** See the residual risks in the
  [threat model](docs/threat-model.md#residual-risks).
- **The protocol library sends some traffic by itself.** While a device is
  connected, it sends WhatsApp, with no way to switch any of it off: an
  acknowledgement of every stanza it receives; a delivery receipt, in its
  inactive form, for every message it decrypts; retry receipts, at most 4 per
  message while the process runs, for a message it cannot decrypt, and a
  request to the owner's phone to send again a message that WhatsApp marks as
  unavailable; an announcement that the device is active, at every connection;
  pre-key uploads; application-state fetches; and session telemetry once, after
  pairing.
  WaWarden never sends read receipts, presence, typing indicators or status
  updates (see
  [the configuration reference](docs/configuration.md#traffic-the-protocol-library-sends-by-itself)).
- **A message refused or interrupted before it is stored is lost, not
  redelivered.** The protocol library decrypts a message, which moves its keys
  on, before WaWarden writes it to its inbox, and acknowledges it only after
  that write. A message WaWarden refuses (a full inbox, ingest paused for lack
  of disk space, a failed write), or one cut short by a stop, comes back from
  WhatsApp in a form the library can no longer decrypt, and never reaches the
  archive; the owner's phone keeps its own copy. Refusals are counted and
  reported, so a loss can be alerted on (see
  [delivery](docs/configuration.md#delivery)).

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

The container contract, in full in
[the configuration reference](docs/configuration.md#container-image):

- **Binary and user.** The binary is `/wawarden`, the entrypoint, with `serve`
  as the default command; the image runs as `65532:65532` and has no shell.
- **Directories.** `/data` is the only path the service writes and the only one
  that must be writable: a directory owned by `65532:65532` with mode `0700`,
  which a new named volume mounted there inherits. In it, the service creates
  `keys/`, `history/` and `backups/` with mode `0700` and every file with mode
  `0600`, and refuses to start on any that is not private to its user.
- **Read-only root filesystem.** Run the container with one, as the example
  does with `--read-only`, so that `/data` is also the only writable path.
- **Health check.** `["/wawarden","healthcheck"]`, in exec form in Compose or
  Kubernetes. The image declares none, and `docker run --health-cmd` wraps the
  command in a shell the image does not have.
- **Listeners.** Client `8080`, admin `8082` (only with an admin token hash) and
  health `8081`, plain HTTP and on `127.0.0.1` by default. A listener on
  `127.0.0.1` cannot be reached through a published port, so the example binds
  the client and admin listeners to `0.0.0.0` and publishes them on the host's
  loopback only; the service logs a `listener_not_loopback` warning for each,
  and other containers on the same Docker network can reach them. The health
  listener stays on loopback, where the health check reaches it.
- **Network.** Outbound only: WhatsApp over HTTPS and a WebSocket on port 443,
  while a device is paired or pairing was requested, and the webhook when one
  is set. Nothing needs to reach the container from outside but the clients and
  the operator, and never from the public internet.
- **Licence.** From M1 on, WaWarden is `GPL-3.0-or-later`, as the image's
  `org.opencontainers.image.licenses` label says; the licence texts of
  WaWarden, the Go standard library and every linked module are under
  `/licenses` in the image (see [Licence](#licence)).

## First run with a WhatsApp account

Read the [residual risks](docs/threat-model.md#residual-risks) before linking an
account: WhatsApp can restrict, log out or ban an account that links an
unofficial client. These steps link the owner's account to a build of `main`,
made [from source](#from-source). In a container, pass the same variables with
`-e`, and run the admin commands inside it with the token on standard input:
`docker exec -i wawarden /wawarden admin status --token-stdin < admin.token`.

### 1. Prepare the secrets

In a directory that only you can read, generate the admin token and keep it and
its hash in separate files. The service is given the hash only; keep a copy of
the token in a secret manager, from which `--token-command` can also read it:

```sh
umask 077
./wawarden admin init > admin.txt
sed -n 's/^sha256: //p' admin.txt > admin.sha256
sed -n 's/^token: //p' admin.txt > admin.token
rm admin.txt
```

On another machine, create the key pair that backups are encrypted to with the
[age](https://age-encryption.org) tools, and keep `backup-identity.txt` there.
The service needs only the public key, `age1...`, that `age-keygen` prints:

```sh
age-keygen -o backup-identity.txt
```

For a webhook, also create its signing secret, readable only by the service's
user: `openssl rand -hex 32 > notify.secret`.

### 2. Configure and start

Set the owner's number in E.164 form, the hash file, the backup recipient and a
data directory whose parent exists, then start the service under a user other
than root:

```sh
export WAWARDEN_DATA_DIR="$HOME/wawarden-data"
export WAWARDEN_OWNER_PHONE=+15550100001
export WAWARDEN_ADMIN_TOKEN_SHA256_FILE="$PWD/admin.sha256"
export WAWARDEN_BACKUP_AGE_RECIPIENT='age1...'
./wawarden serve
```

Add `WAWARDEN_NOTIFY_URL` and `WAWARDEN_NOTIFY_SECRET_FILE` for the webhook, and
`WAWARDEN_METRICS_EMF=1` for metrics on standard output; the
[configuration reference](docs/configuration.md#environment-variables) lists
every variable, and any other `WAWARDEN_` variable stops the start. Run the
service under a supervisor that keeps its standard output, where every event
is written, and gives it more than 10 seconds to stop.

The start writes JSON lines such as `keys_loaded`, `archive_opened`,
`session_opened` with `"paired":false`, one `listening` per listener and
`ready`. With no device stored, the engine reports `unpaired` and makes no
connection to WhatsApp. Without a backup recipient, every start warns
`backup_disabled`.

### 3. Pair

```sh
./wawarden admin pair --token-file admin.token
```

It prints `pairing code: <code>`, and on standard error where to enter it. On
the owner's phone, in WhatsApp, open Linked devices, choose Link a device, then
Link with phone number instead, and enter the code. WaWarden links as a
`Chrome (Linux)` device. The code goes to you only; it is never logged or kept.

| Refusal | Exit code | What to do |
|---|---|---|
| `owner_phone_missing` | `5` | Set `WAWARDEN_OWNER_PHONE` and restart. |
| `already_paired` | `5` | A device is stored already; see `admin status`. |
| `rate_limited` | `5` | Three attempts were made in the last hour; wait. |
| `pair_failed` | `1` | The connection failed or no code came within 25 seconds; check outbound access to `web.whatsapp.com` and try again. |

Only the account whose number is `WAWARDEN_OWNER_PHONE` can be linked. A code
entered on another account's phone is refused before anything is stored, or,
should that pairing complete, the new device is logged out again; either way
the service reports `pair_rejected`.

### 4. Watch the status

```sh
./wawarden admin status --token-file admin.token
```

```text
state: connected
reason: none
paired: true
chats: 12
messages: 3456
history blobs pending: 0
history blobs quarantined: 0
inbox backlog: 0
inbox quarantined: 0
last ingest: 2026-10-05T08:00:00Z
version: <version>
```

`state` is `unpaired`, `connecting`, `connected` or `disconnected`, the last with
a `reason`. Right after pairing, WhatsApp asks the new device to log in again,
which the engine handles as a dropped connection: expect a short `connecting`
before `connected`. Each change is logged as `engine_state`, the gauges
`wawarden_paired` and `wawarden_connected` follow it, and every stop that waits
for you is reported as a `disconnected` event, on standard output and to the
webhook.

### 5. The first history sync

Once paired, the owner's phone sends the chat history in blobs.
`history blobs pending` rises as they are announced and returns to `0` as each
is downloaded into `history/`, applied to the archive and deleted; `chats`,
`messages` and `last ingest` grow with it, and so does
`wawarden_messages_ingested_total`. Blobs are downloaded only while connected,
and a restart takes up those still waiting. A blob larger than
`WAWARDEN_HISTORY_MAX_BYTES` (32 MiB by default), or one that fails three
times, is quarantined: the `quarantine` event reports it with `queue`
`history`, and `history blobs quarantined` counts it. See
[history sync](docs/configuration.md#history-sync).

### 6. The first backup

With a backup recipient, the service takes one backup once no history blob is
waiting and 10 minutes have passed since the later of the pairing and the
latest blob's arrival. It writes `backups/<UTC time>.age`, such as
`backups/20261005T120000Z.age`, and reports `backup_done` with `bytes`,
`archive_bytes`, `session_bytes` and `duration_ms`, or `backup_failed` with a
reason, which is tried again only at the next start. It is the only backup for
this pairing: there is no schedule or retention yet (M3). Copy it off the host
and check that it decrypts with the identity file, as
[backups](docs/configuration.md#backups) describes.

### 7. When the engine disconnects

A disconnection never stops the process, and `/healthz` still answers `200`.
The engine retries an ordinary drop by itself, waiting up to 5 minutes between
attempts. It stays `disconnected` for these reasons until you act:

| `reason` | What happened | What to do |
|---|---|---|
| `outdated` | No protocol version could be fetched from `web.whatsapp.com` at the start, or WhatsApp reported the client as outdated and no newer version could be fetched. | Check outbound access, then `admin reconnect`, which fetches again, or restart. After WhatsApp reported the client as outdated, only a strictly newer version helps: try again later, or upgrade WaWarden. |
| `replaced` | Another client took over the session, such as a second service started from a copy of `session.db`. | Stop the other one, then `admin reconnect`. |
| `logged_out` | The device was logged out, from the phone or by WhatsApp; it is gone and `paired` is `false`. | Pair again, as in step 3. |
| `temporary_ban` | WhatsApp banned the account temporarily. | Wait for the ban to end, then `admin reconnect`. |
| `cat_refresh` | A connection token could not be refreshed. | `admin reconnect`. |
| `connect_failure` | WhatsApp refused the connection. | `admin reconnect`. |
| `restart_budget` | The service started more than 5 times in 10 minutes, so the engine did not connect. | Find out from the logs why it restarts, then `admin reconnect`, or `admin pair` when no device is stored; or restart once the 10 minutes have passed. |
| `owner_mismatch` | The stored device's number is not `WAWARDEN_OWNER_PHONE`. | Correct `WAWARDEN_OWNER_PHONE` and restart. When the device belongs to another account, as after a failed logout, stop the service, delete `session.db` and `session.db-journal` from the data directory, remove the device from that account's Linked devices, then start and pair again. |
| `shutdown` | The service is stopping. | Nothing. |

`admin reconnect` answers `already_connected` while the engine is connected and
`not_paired` when no device is stored (exit code `5`); see
[engine states](docs/configuration.md#engine-states).

### 8. Unpairing and pairing again

There is no admin command that unpairs. To unlink WaWarden, log the device out
on the owner's phone: Linked devices, select the device, Log out. WhatsApp logs
it out at once while it is connected, or at its next connection: the engine
reports `disconnected` with reason `logged_out`, then `unpaired`; `admin status`
shows `paired: false`, and both gauges fall to `0`. The device is deleted from
`session.db`, the archive keeps everything stored so far, and nothing more
arrives. After a restart, the engine starts `unpaired`.

To pair again, run `admin pair` as in step 3, for the same account. The new
device gets its own history sync and, once that has settled, its own backup.

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
| MIT | `github.com/beeper/argo-go`, `github.com/dustin/go-humanize`, `github.com/elliotchance/orderedmap/v3`, `github.com/mattn/go-colorable`, `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`, `github.com/rs/zerolog`, `github.com/vektah/gqlparser/v2` |
| BSD-3-Clause | the Go standard library, the backup encryption `filippo.io/age` with `filippo.io/hpke`, `filippo.io/edwards25519`, `github.com/google/uuid`, `github.com/remyoudompheng/bigfft`, `golang.org/x/crypto`, `golang.org/x/exp`, `golang.org/x/net`, `golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/text`, `google.golang.org/protobuf`, and the SQLite driver `modernc.org/sqlite` with `modernc.org/libc`, `modernc.org/mathutil` and `modernc.org/memory`, which also carry the licences of the C code they translate, SQLite's public-domain dedication among them |

Every release ships their licence files, and WaWarden's, as
`wawarden_<version>_licenses.tar.gz` and under
`/licenses` in the image; [`RELEASING.md`](RELEASING.md#licences) describes how
they are collected.
