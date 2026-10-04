# WaWarden

WaWarden is a self-hosted WhatsApp gateway whose link to a WhatsApp account and
REST and MCP interfaces are planned and not built yet: the current milestone,
M0, is a scaffold (see [Status](#status)). When complete, it will link to one
personal WhatsApp account as a companion device and expose that account to your
own AI agents and applications, over REST and MCP, with access scoped per client
and per chat: each client will get a token that may read, or read and write,
only the chats on its allowlist.

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
  non-loopback health address, running as root, or a data directory that is not
  private to the service's user.
- **Browser-originated requests are refused.** Any request to the client or admin
  listener carrying an `Origin` or `Sec-Fetch-Site` header gets `403`.
- **Your WhatsApp account is at risk.** See the residual risks in the
  [threat model](docs/threat-model.md#residual-risks).

## Status

WaWarden is at milestone **M0**, a scaffold. It does not connect to WhatsApp yet.

What works today:

- `wawarden serve` validates its configuration, refuses to start on anything it
  does not implement, and opens the client and health listeners, and the admin
  listener when an admin token hash is configured.
- The client listener has no routes and no client tokens yet: it refuses every
  request, with `401` for anything that reaches authentication.
- The admin listener, opened only when an admin token hash is configured, serves
  Prometheus metrics at `GET /metrics` to the admin token.
- The health listener answers `GET /healthz`.
- `wawarden admin init` generates the admin token; `wawarden healthcheck` is the
  container health check; `wawarden version` prints the build.
- Internally: the policy core that mints read, write and admin grants, route
  registration that requires a policy class, and architecture tests and lint rules
  that enforce both.
- On `main`, ahead of the M1 release: a strict normaliser for WhatsApp chat
  identifiers (phone-number users, LID users and groups), the only source of a
  chat that a grant can allow, with a fuzz target. Nothing calls it yet. See the
  [threat model](docs/threat-model.md#secure-by-construction), row 4.

Planned:

| Milestone | Scope |
|---|---|
| M1 | The WhatsApp engine: pairing guarded by an account check, history sync, the session and the message archive in SQLite, `admin status`, `pair` and `reconnect`, notification events, metrics on standard output, encrypted backups. |
| M2 | Clients with per-chat read scopes and expiring tokens; the read API over REST (chats, messages, search, change feed) and MCP; the audit trail. |
| M3 | Sending over REST and MCP, with idempotency, pacing, per-client budgets and a first-contact rule; nightly encrypted backups with retention; the v1.0 documentation. |

## Quick start (M0)

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
that version when the installed one is older.

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

M0 has no client routes and no client tokens, so every client request is refused:

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

The metrics are the failed-authentication counters and the build information:

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
```

Stop the service; it shuts down gracefully and exits `0`. Then delete the
temporary directory, which holds the admin token and the data directory:

```sh
kill %1
wait
rm -rf "$demo"
```

### From the container image

Images are published to `ghcr.io/dortort/wawarden` by the release workflow only.
Take the image index digest from a release's notes, and verify the image as
[`RELEASING.md`](RELEASING.md#verifying-a-release) describes. If no release is
listed yet, use the steps from source.

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

## Verifying a release

Every release is built from `main` by a workflow that needs the maintainer's
approval, rebuilt independently and compared bit for bit, attested and signed.
[`RELEASING.md`](RELEASING.md#verifying-a-release) gives the commands to verify the
image, the binaries and their SBOMs, and to reproduce a release from source.

## Licence

MIT. See [`LICENSE`](LICENSE).
