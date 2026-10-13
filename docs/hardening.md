# Hardening guide

This guide is for whoever deploys WaWarden. For each topic it says what to do,
what the service already enforces, and what it cannot see. It describes the
code at the commit or tag you are reading; the exact behaviour is in
[configuration.md](configuration.md), the reasons in the
[threat model](threat-model.md), and the commands for operations and incidents
in the [runbook](runbook.md). The example configurations for other software
(Tailscale, WireGuard, Caddy, nginx, systemd, Kubernetes) follow those
projects' documentation and are starting points, not tested deployments.

## Contents

- [Scope and trust roots](#scope-and-trust-roots)
- [Never face the public internet](#never-face-the-public-internet)
- [Network patterns](#network-patterns)
  - [Mesh or VPN forwarding to loopback](#mesh-or-vpn-forwarding-to-loopback)
  - [Private subnet](#private-subnet)
  - [Reverse proxy with TLS](#reverse-proxy-with-tls)
- [Gate the admin listener separately](#gate-the-admin-listener-separately)
- [Restrict egress](#restrict-egress)
- [Container hardening](#container-hardening)
- [systemd](#systemd)
- [File modes and umask](#file-modes-and-umask)
- [Secrets](#secrets)
- [Token lifecycle](#token-lifecycle)
- [Backups with age](#backups-with-age)
- [Unlink other devices](#unlink-other-devices)
- [Alarms](#alarms)
- [Deploy by digest and verify](#deploy-by-digest-and-verify)
- [Checklist](#checklist)

## Scope and trust roots

WaWarden scopes, budgets and audits what its clients do. It cannot protect the
account from what reaches around it. The threat model's
[trust roots](threat-model.md#trust-roots) are the canonical list; in short:

- whoever controls the host, its user ID or the data directory controls the
  account, its archive and its tokens;
- one host is one trust domain: processes running as the same user can read
  each other's tokens and files;
- any other device linked to the account has the account's full access,
  outside every scope, budget and audit row;
- whoever reads the service's logs sees pseudonyms, and with the master key can
  test them against identifiers;
- whoever controls the maintainer's account and the release workflow controls
  what a release contains.

Everything below assumes the host is trusted and narrows what the network, a
token or a mistake can reach.

## Never face the public internet

The three listeners speak plain HTTP, without TLS, and bind `127.0.0.1` by
default (see [listeners](api.md#listeners)). A client or admin
listener bound to any other address makes the service log
`listener_not_loopback` at every start, whatever `WAWARDEN_LOG_LEVEL` says; a
forwarder, sidecar or proxy that exposes a loopback listener makes it log
nothing. A token is a bearer credential: whoever holds it and can reach the
listener has its access, from any address.

- **Docker publishes around host firewalls.** A port published with `-p 8080:8080`
  is reachable from every interface whatever ufw or firewalld rules the host
  has, because Docker writes its own packet-filter rules (Docker's
  "Packet filtering and firewalls" page,
  <https://docs.docker.com/engine/network/packet-filtering-firewalls/>, section
  "Docker and ufw"). Publish on the host's loopback only, as the container
  example in [getting started](getting-started.md) does with
  `-p 127.0.0.1:8080:8080`, or do not publish at all and let a sidecar reach
  the service (below).
- **Tailscale Funnel exposes a service to the whole internet.** It is off by
  default (<https://tailscale.com/kb/1223/funnel>). Never run `tailscale funnel`
  on WaWarden's ports; Tailscale Serve, below, stays inside the tailnet.
- **Health checks stay local.** The health listener must be loopback, which the
  service enforces; `wawarden healthcheck` runs next to the service.

## Network patterns

Choose one way for clients and the operator to reach the service. WaWarden
knows nothing about any of them: it sees connections to its listeners.

### Mesh or VPN forwarding to loopback

A mesh VPN such as Tailscale gives each device an identity and encrypts the
path. Tailscale Serve proxies a port of the tailnet address to a local service
at `http://127.0.0.1:<port>` and terminates TLS with a certificate for the
tailnet name (<https://tailscale.com/kb/1242/tailscale-serve>).

- In containers, run the Tailscale image as a sidecar and share its network
  namespace with WaWarden through Compose's `network_mode: "service:<sidecar>"`
  (<https://docs.docker.com/reference/compose-file/services/#network_mode>), so
  that the sidecar reaches WaWarden's loopback listeners and WaWarden keeps its
  defaults. The image's start program reads its Serve configuration from
  `TS_SERVE_CONFIG`, keeps its state in `TS_STATE_DIR`, and with
  `TS_USERSPACE` runs without a tun device.
- Gate the two ports separately with the tailnet policy's grants
  (<https://tailscale.com/kb/1324/grants>), each naming a port in its `ip`
  list such as `"tcp:443"`: the agent hosts may reach the client port, only
  the operator's device the admin port.

With WireGuard alone, a peer may send only from the addresses its configuration
allows ("Cryptokey Routing", <https://www.wireguard.com/>). Bind
`WAWARDEN_LISTEN`, and `WAWARDEN_ADMIN_LISTEN` if it is used, to the tunnel
address only, expect `listener_not_loopback` at every start, and keep the path
encrypted end to end.

### Private subnet

Bind `WAWARDEN_LISTEN` to a private address (the service logs
`listener_not_loopback`), allow only the agent hosts to reach that port in the
security group or host firewall, and give the admin port no inbound rule except
from the operator. The path is plain HTTP: anything on the subnet that can see
the traffic sees the tokens, so use this only on a segment you control, or add
TLS as below.

### Reverse proxy with TLS

A reverse proxy in front of the loopback listeners terminates TLS and can also
require client certificates: in Caddy, the `tls` directive's
`client_auth { mode require_and_verify ... }`
(<https://caddyserver.com/docs/caddyfile/directives/tls>); in nginx,
`ssl_verify_client on` with `ssl_client_certificate`
(<https://nginx.org/en/docs/http/ngx_http_ssl_module.html>). Mutual TLS binds
access to a certificate, which a bearer token alone is not. The service puts
these constraints on the proxy:

- **No `Origin` or `Sec-Fetch-Site`.** The service answers `403` to any request
  that carries either header ([client and admin requests](api.md#client-and-admin-requests)),
  so the proxy must not add them, and must not answer CORS preflights itself.
- **Bodies.** The service refuses a body over 16384 bytes, 65536 for the client
  create route and the send route, and 64 KiB for `POST /mcp`
  ([request bodies](api.md#request-bodies)); a proxy limit below
  64 KiB breaks MCP calls and sends of long or escaped texts.
- **Timeouts.** A send can take 25 seconds and the service writes a response
  within 30; give the proxy's upstream read timeout at least 30 seconds.
- **MCP is plain HTTP.** The endpoint is `POST /mcp` only, stateless and
  JSON-only: no server-sent events, no WebSocket upgrade, no session affinity.
- **Logs.** Never log the `Authorization` header. The query string of
  `GET /v1/search?q=` carries search terms; drop query strings from access
  logs, or keep those logs as private as the archive.
- **Rate limits.** Each listener has one failure budget shared by every caller
  ([failed authentication](api.md#failed-authentication)); limits
  per source address belong in the proxy.
- **Responses** are JSON with `Cache-Control: no-store`; do not cache them.

## Gate the admin listener separately

The admin listener is guarded by the admin token alone. With it, a caller can
request pairing, reconnect the engine, create clients (writing ones included),
revoke them, list chats and request a history backfill. The pairing code
crosses the path in its answer. The threat model's
[admin token](threat-model.md#the-admin-token) section gives the full list. The
listener is opened only when `WAWARDEN_ADMIN_TOKEN_SHA256` or
`WAWARDEN_ADMIN_TOKEN_SHA256_FILE` is set.

- Give it its own port and its own access rule, reachable from the operator's
  device only, never from the agent hosts.
- Encrypt the path to it, because the token and the pairing code cross it.
- When nothing needs administering, a service without an admin token hash
  opens no admin listener at all; setting the hash again needs a restart.
- Anything that scrapes `GET /metrics` holds the full admin token
  ([metrics](observability.md#metrics)). Prefer metrics in embedded metric
  format (EMF), turned on with `WAWARDEN_METRICS_EMF=1`, whose lines on
  standard output need no token
  ([embedded metric format](observability.md#embedded-metric-format)).

## Restrict egress

The service makes these outbound connections and no others
([listeners](api.md#listeners)), each directly: `HTTPS_PROXY` and
`HTTP_PROXY` are ignored, so no forward proxy can sit in the path.

| Destination | When |
|---|---|
| `wss://web.whatsapp.com/ws/chat`, port 443 | While a device is paired or pairing was requested |
| `https://web.whatsapp.com`, the protocol version | Before connecting |
| The media hosts WhatsApp names at run time, such as `mmg.whatsapp.net`, for history-sync downloads | During history sync and after a backfill request |
| The webhook host and port of `WAWARDEN_NOTIFY_URL`, HTTPS only | When it is set; private addresses are refused unless `WAWARDEN_NOTIFY_ALLOW_PRIVATE=1` |
| The system's DNS resolver | For all of the above |

A service without a paired device makes no connection to WhatsApp until
pairing is requested. WhatsApp's hosts sit behind a content delivery network
whose addresses change, so address allow-lists are impractical, and the set of
media host names a live account sees has not been recorded by this project.
The workable control is a DNS-name allow-list for `*.whatsapp.net` and
`*.whatsapp.com` plus the webhook host, or a DNS firewall in alert mode used
as a tripwire: observe for a week in alert mode, then enforce or keep alerting.
An unexpected destination is worth an alarm either way.

## Container hardening

The release image is `gcr.io/distroless/static-debian13:nonroot` pinned by
digest, runs `/wawarden` as `65532:65532`, defaults to `serve`, has no shell,
and owns `/data` with mode `0700`
([container image](configuration.md#container-image)). The process needs no
capability: its ports are above 1024 and the binary is static. Run it with:

```sh
docker run -d --name wawarden \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges=true \
  --pids-limit=256 --memory=512m --stop-timeout 15 \
  --mount type=volume,src=wawarden-data,dst=/data \
  -e WAWARDEN_ADMIN_TOKEN_SHA256="$(cat admin.sha256)" \
  -e WAWARDEN_LISTEN=0.0.0.0:8080 -e WAWARDEN_ADMIN_LISTEN=0.0.0.0:8082 \
  -p 127.0.0.1:8080:8080 -p 127.0.0.1:8082:8082 \
  ghcr.io/dortort/wawarden@sha256:<digest>
docker exec wawarden /wawarden healthcheck
```

- `--read-only` leaves `/data` as the only writable path, and
  `--cap-drop=ALL` with `no-new-privileges` removes what the process never
  uses (<https://docs.docker.com/reference/cli/docker/container/run/>).
- `--memory`: history sync needs at most 2 times `WAWARDEN_HISTORY_MAX_BYTES`
  plus 1 MiB to decompress a blob, about 65 MiB at the default, plus the
  memory to decode it ([history sync](internals.md#history-sync)). 512 MiB
  leaves room at the default; raise it with the cap, which allows up to
  256 MiB.
- `--stop-timeout 15`: the service takes up to 10 seconds to stop, which is
  Docker's default timeout exactly ([shutdown](cli.md#shutdown)).
- The health check runs in exec form, `["/wawarden","healthcheck"]`; the image
  has no shell for `--health-cmd` to use.
- Never run it `--privileged`, never mount the Docker socket into it, and never
  mount anything but the data volume (and, read-only, a secret file).

In Kubernetes, the same in a pod spec: `runAsNonRoot: true`, `runAsUser` and
`runAsGroup` `65532`, `readOnlyRootFilesystem: true`,
`allowPrivilegeEscalation: false`, `capabilities: {drop: [ALL]}`, a
`RuntimeDefault` seccomp profile, an exec probe of `/wawarden healthcheck`,
`terminationGracePeriodSeconds` of at least 15, and one replica. Do not set
`fsGroup`: it adds group permissions to the volume, which fails the data
directory's mode check
([data directory](configuration.md#data-directory)). Give the volume owner
`65532:65532` and mode `0700` before the first start instead.

## systemd

On a host without containers, run the binary as a static system user, not
`DynamicUser=`, so that the data directory keeps one owner:

```ini
[Unit]
Description=WaWarden
Wants=network-online.target
After=network-online.target

[Service]
User=wawarden
Group=wawarden
ExecStart=/usr/local/bin/wawarden serve
Environment=WAWARDEN_DATA_DIR=/var/lib/wawarden
Environment=WAWARDEN_OWNER_PHONE=+15550100000
LoadCredential=admin-hash:/etc/wawarden/admin.sha256
Environment=WAWARDEN_ADMIN_TOKEN_SHA256_FILE=%d/admin-hash
StateDirectory=wawarden
StateDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/wawarden
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6
MemoryDenyWriteExecute=yes
TimeoutStopSec=15
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
```

Every directive is described in `systemd.exec(5)`
(<https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html>);
`%d` is the credentials directory that `LoadCredential=` fills. `IPAddressAllow=`
and `IPAddressDeny=` take addresses and prefixes, not names, so they cannot
express a WhatsApp allow-list; use them at most as a coarse tripwire. Check the
result with `systemd-analyze security wawarden.service`. A restart loop is
bounded by the service too: more than 5 starts in 10 minutes keep the engine
disconnected with reason `restart_budget`.

## File modes and umask

The process sets its umask to `0077` at start, and refuses to start when
anything it keeps is not private to its user
([startup refusals](configuration.md#startup-refusals)):

- the data directory: exactly `0700`, owned by the service's user;
- `keys/`, `keys/retired/`, `history/` and `backups/`: `0700`;
- `keys/master` and each retired key: no access for group or others, such as
  `0600` or `0400`;
- `archive.db`, `session.db` and their journals: no access for group or others.

Because each of these checks is a startup refusal, a wrong mode is found at
once, not months later. What `backups/` holds is checked when a backup is
taken or old ones are pruned instead:

- `serve` removes `backups/tmp/` at start.
- A backup fails unless `backups/tmp/` is a `0700` directory of the service's
  user (`backup_failed` with reason `directory`).
- The prune after a backup deletes nothing while `backups/` holds any entry
  other than a `0700` `backups/tmp/` of the service's user and regular `0600`
  files of that user named like backups (`backup_prune_failed` with reason
  `unexpected_entry`). A file you put there, a backup of another mode or
  owner, a symbolic link or another directory all count. Remove the entry, and
  the next backup prunes again.

Mechanisms that add group bits, such as Kubernetes `fsGroup` or a setgid
parent directory, fail the checks.

## Secrets

The service holds no secret it could leak to a caller:

- **Admin credential.** The service takes only the SHA-256 of the admin token,
  in `WAWARDEN_ADMIN_TOKEN_SHA256` or a file named by
  `WAWARDEN_ADMIN_TOKEN_SHA256_FILE`, and refuses the token itself.
- **Webhook secret.** `WAWARDEN_NOTIFY_SECRET_FILE` names a file with no write
  permission for its group and no permission for others, readable by the
  service's user. Secret mounts of mode `0444` or `0644`, the defaults of some
  orchestrators, are refused: set the mount's mode, for example `0400`, and
  its owner.
- **Backups** need only the age recipient, a public key.
- **The CLI** reads the admin token from a file, standard input or a command,
  never from an argument or the environment.
- **Client tokens** belong on the agent's host, as `WAWARDEN_TOKEN` or in a
  secret manager, never in the service's environment.
  `read -rs WAWARDEN_TOKEN` keeps a pasted token out of the shell's history.

## Token lifecycle

- A client token expires after 90 days by default and 365 at most; none lives
  forever. A revocation takes effect on the client's next request.
- Rotate by creating a client with the same chats under a new name, switching
  the agent, then revoking the old client; a name stays taken after its client
  is revoked ([runbook](runbook.md#rotating-a-client-token)).
- Read-only by default. Give write chats only to clients that must send, and
  `--allow-first-contact` only when the agent must start conversations. The
  service refuses a client that reads every chat and also writes.
- Put a write token only on a host where no agent with shell or network tools
  reads untrusted input ([agents](agents.md#tokens)).
- The admin token never expires. Rotate it by generating a new one with
  `wawarden admin init`, replacing the hash and restarting; treat whoever holds
  it as the operator.
- Leave `WAWARDEN_UNSAFE_RATE_CAPS` and `WAWARDEN_UNSAFE_SEND_CAPS` unset; each
  logs a warning at every start when set.

## Backups with age

- Generate the age identity on another machine, `age-keygen -o
  backup-identity.txt` (or `age-keygen -pq` for a hybrid post-quantum
  identity; <https://github.com/FiloSottile/age>), keep it off the service's
  host, and give the service only the `age1...` recipient in
  `WAWARDEN_BACKUP_AGE_RECIPIENT`. The service cannot decrypt a backup.
- The service backs up once the first history sync has settled, then each
  night at `WAWARDEN_BACKUP_TIME` (`03:00` UTC by default), and keeps the
  newest `WAWARDEN_BACKUP_KEEP` (14 by default) in `backups/`
  ([backups](configuration.md#backups)). Copies on the same disk do not
  survive the disk: copy backups off the host.
- age does not authenticate the sender: anyone who can write `backups/` can
  add a well-formed backup. A backup holds two databases copied one after the
  other, so they are two points in time.
- Check backups where the identity is kept, with `wawarden backup verify`,
  and rehearse a restore ([runbook](runbook.md#restoring-a-backup)).
- No backup holds `keys/master` or `keys/retired/`. Keep a copy of both, as
  private as the identity: without them a restored archive's pseudonyms,
  cursors and references change and older audit rows verify as `unknown_key`.

## Unlink other devices

Once agents work through WaWarden, log out every other companion device that
was used for the same purpose, on the owner's phone under Linked devices. A
linked device has the account's full access, outside every scope, budget and
audit row of WaWarden. WhatsApp's Help Center describes the steps in "How to
unlink a device" (<https://faq.whatsapp.com/834124628020911>) and linked
devices in "About linked devices" (<https://faq.whatsapp.com/378279804439436>);
this guide names those pages without quoting them.

## Alarms

Route alarms through something other than the WaWarden webhook alone: a
webhook post can be late or lost, and a dead process sends nothing.

| Signal | Alarm when | Meaning |
|---|---|---|
| EMF `Paired` (`wawarden_paired`) | below 1 for more than a few minutes | No device is linked: logged out, banned or never paired. |
| EMF `Connected` (`wawarden_connected`) | 0 while `Paired` is 1, for more than a few minutes | Disconnected; reads still work and `/healthz` stays `200`. |
| EMF lines missing | no line for a few minutes | The process is gone; nothing else reports it. |
| EMF `Panics` | above 0 | A recovered panic; the `name` dimension says where. |
| EMF `AuthFailures`, `AdminAuthFailures` | a spike | Someone holds a wrong or revoked token, or is guessing. |
| EMF `SendsRejected` | sustained | An agent is pushing against its limits or the first-contact rule; the `reason` dimension says which. |
| EMF `MessagesIngested` | 0 for longer than the account is normally quiet | Messages are not arriving. |
| `wawarden_backup_last_success_timestamp_seconds` | older than about a day and a half | Nightly backups have stopped. |
| Log event `backup_done` | none, nor `backup_skipped`, for about a day and a half while `WAWARDEN_BACKUP_AGE_RECIPIENT` is set | Nightly backups have stopped; the signal to use with EMF alone, which carries no backup metric. |
| Events `unpaired`, `disconnected` (by `reason`), `pair_rejected`, `logout_failed` | any | The engine needs the operator ([disconnect reasons](runbook.md#disconnect-reasons)). |
| Events `quarantine`, `ingest_paused`, `rekey_conflict` | any | Ingest set something aside or stopped. |
| Events `backup_failed`, `backup_prune_failed` | any | A backup or its retention failed. |
| Events `send_outcome_unknown`, `send_unrecorded` | any | A send may have reached WhatsApp without a record of it. |
| Event `audit_key_changed` at `WARN` | any | The audit chain's key changed without the old key kept as a retired key. |
| Log event `audit_prune_refused` | any | The audit retention found rows or an anchor that do not verify and removed nothing more that night; an `audit_pruned` line reports what earlier batches removed ([runbook](runbook.md#audit-chain)). |
| Event `admin_auth_failure` | any | Someone is trying tokens on the admin listener. |
| Log events `notify_failed`, `notify_dropped` | any | Webhook posts failed or were dropped. |
| Log events `listener_not_loopback`, `unsafe_debug`, `unsafe_rate_caps`, `unsafe_send_caps` | unexpected | A setting that widens exposure is on. |

`PolicyDenials` stays `0`, because a chat outside a client's scope and a
missing chat are answered alike; probing shows as `not_found` rows in the audit
chain instead. Events are described under
[notifications](observability.md#notifications), log events under
[logging](observability.md#logging).

## Deploy by digest and verify

Deploy images by digest and binaries by checksum, and verify each release's
provenance and signature as [RELEASING.md](../RELEASING.md#verifying-a-release)
describes before running it. Read the release notes and the source changes
between the version you run and the next before upgrading, because whoever
controls a release controls the account; the
[runbook](runbook.md#upgrading) gives the order of an upgrade.

## Checklist

- [ ] No listener faces the public internet; published ports are on
      `127.0.0.1`, and Tailscale Funnel is off for them.
- [ ] Clients reach the client port through a VPN, a private subnet or a TLS
      proxy; the path is encrypted wherever others share it.
- [ ] The admin port is reachable from the operator's device only, or not
      opened at all; metrics come from EMF, not a token-holding scraper.
- [ ] A proxy, if any, adds no `Origin` or `Sec-Fetch-Site`, logs neither
      `Authorization` nor query strings, allows 64 KiB bodies and 30-second
      responses, and limits callers per address.
- [ ] Egress goes only to WhatsApp's names, the webhook and DNS, enforced or
      alerting.
- [ ] The container runs read-only, without capabilities, with
      `no-new-privileges`, a memory and process limit and
      `--stop-timeout 15`; or the systemd unit sets the equivalent.
- [ ] The data volume is owned by the service's user with mode `0700`; no
      `fsGroup`.
- [ ] The admin token's hash, not the token, is configured; the webhook secret
      file is `0400` or `0600`.
- [ ] Client tokens are read-only unless they must write, scoped to the chats
      the task needs, rotated before they expire, and kept off hosts where an
      agent with shell access reads untrusted input.
- [ ] Backups are encrypted to a recipient whose identity is off the host,
      copied off the host, verified with `backup verify`, and a restore has
      been rehearsed; copies of `keys/master` and `keys/retired/` are kept.
- [ ] Other linked devices are logged out.
- [ ] Alarms cover `Paired`, `Connected`, missing EMF lines, panics,
      authentication failures, rejected sends, ingest stalls, backups and the
      events above, through a path that does not depend on the webhook alone.
- [ ] Images run by digest, verified, and upgrades follow the runbook.
