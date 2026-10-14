# Getting started

This guide links WaWarden to a real WhatsApp account. It takes you from an
empty host to a paired service with a verified backup and a first client
registered in Claude Code. To try WaWarden without an account first, use
[the fake engine demo in the README](../README.md#try-it-without-an-account).

> **Read the [residual risks](threat-model.md#residual-risks) and the
> [ban-risk notes](ban-risk.md) before linking an account.** WhatsApp can
> restrict, log out or ban an account that links an unofficial client.

The commands use `wawarden`, installed as [Install](#install) describes, and
the default addresses on `127.0.0.1`. To run in a container instead, read
[Running in a container](#running-in-a-container) alongside each step.

## Contents

- [Prerequisites](#prerequisites)
- [Install](#install)
- [1. Prepare the secrets](#1-prepare-the-secrets)
- [2. Configure and start](#2-configure-and-start)
- [3. Put it behind HTTPS](#3-put-it-behind-https)
- [4. Pair](#4-pair)
- [5. Watch the status](#5-watch-the-status)
- [6. The first history sync](#6-the-first-history-sync)
- [7. The first backup](#7-the-first-backup)
- [8. The first client](#8-the-first-client)
- [Running in a container](#running-in-a-container)
- [You now have](#you-now-have)
- [What next](#what-next)

## Prerequisites

- [ ] **WaWarden**, installed as [Install](#install) describes: a release
  binary, the image, or a build from source.
- [ ] **A user other than root** to run the service. `serve` refuses to start
  as root.
- [ ] **The [age](https://age-encryption.org) tools on a second machine**, to
  hold the key that decrypts backups.
- [ ] **openssl**, if you want the webhook, to create its signing secret.
- [ ] **A supervisor** that keeps the service's standard output and gives it
  more than 10 seconds to stop, such as systemd (see
  [the hardening guide's unit](hardening.md#systemd)) or a container runtime.
- [ ] **The owner's phone**, with WhatsApp signed in to the account you link.
- [ ] **A way to reach the service over HTTPS** from the agent's host: a mesh
  VPN such as Tailscale, a private subnet with TLS, or a reverse proxy with TLS
  (see [network patterns](hardening.md#network-patterns)).
- [ ] **Claude Code**, or another MCP client, on the agent's host.

## Install

### Supported platforms

| Platform | Release artifacts | From source |
|---|---|---|
| Linux on `amd64` (`x86_64`) and `arm64` (`aarch64`) | Binaries and the container image | Supported |
| macOS 13 Ventura or later, Apple silicon and Intel | None | Builds; CI does not test it |
| Linux on other architectures | None | May build; not tested |
| Windows, FreeBSD, OpenBSD and other systems | None | Does not build |

Deploy the service on Linux, under [systemd](hardening.md#systemd) or as a
[container](configuration.md#container-image). On Windows, use a Linux
virtual machine; WSL2 and Docker Desktop have not been tested.

### Release binary

Each release ships a static binary per platform,
`wawarden_<version>_linux_<arch>`, with `SHA256SUMS` and a Sigstore bundle of
their build provenance, `wawarden_<version>_provenance.sigstore.json`. These
commands download the latest release's binary for this host into a new empty
directory, check its checksum and provenance, and install it as
`/usr/local/bin/wawarden`. They stop at the first step that fails, so nothing
is installed unless both checks pass. They need [`gh`](https://cli.github.com)
but no GitHub account:

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

To install another release, set `version` to its tag. The binary is
downloaded on its own, so on a machine whose architecture has no binary `gh`
stops with `no assets match the file pattern`. `--ignore-missing` checks only
the files you downloaded; without it, the check fails on the other files
`SHA256SUMS` lists. `gh attestation verify` checks the binary against the
release's own bundle: that the release workflow on `main` of this repository
built it. [Verifying a release](../RELEASING.md#verifying-a-release)
describes these checks and the others.

Without `gh`, download the same files with curl, in place of the two
`gh release download` commands. The provenance check still needs `gh`:

```sh
base=https://github.com/dortort/wawarden/releases/download/$version
curl -fL --remote-name-all "$base/$binary" "$base/SHA256SUMS" "$base/$bundle"
```

The binaries' names carry the version. Only `SHA256SUMS` has a URL that
always names the latest release:
`https://github.com/dortort/wawarden/releases/latest/download/SHA256SUMS`.

### With go install

```sh
CGO_ENABLED=0 go install -trimpath github.com/dortort/wawarden/cmd/wawarden@latest
```

This builds the latest release from the Go module proxy and installs it as
`wawarden` in `$(go env GOPATH)/bin`, or in `GOBIN` when that is set; that
directory must be on your `PATH`, or copy the binary to `/usr/local/bin` with
`sudo install -m 0755`. It needs Go 1.26.0 or later; with `GOTOOLCHAIN=auto`,
an older Go downloads a newer one. It builds with that Go rather than the
toolchain the releases use, so the binary is not byte-identical to a release
binary and has no provenance attestation. `go version -m "$(command -v wawarden)"`
shows the release it was built from. `wawarden version` shows it too, except
for `v1.0.0` and earlier, which print `dev`.

### Container image

Each release publishes a multi-arch image for `linux/amd64` and `linux/arm64`.
Pull it by the digest in the release's notes,
`ghcr.io/dortort/wawarden@sha256:<digest>`. Each image is also tagged with its
version, `ghcr.io/dortort/wawarden:<version>`. No `latest` tag exists, by
design: a deployment names the release it runs, and the signature and
attestations cover the digest. Pulling needs no registry login. Check the
image's signature and provenance as
[Verifying a release](../RELEASING.md#verifying-a-release) describes:
`cosign verify` needs no GitHub account, and the provenance check needs
`gh auth login`.
[Running in a container](#running-in-a-container) gives the commands that
replace the steps below.

### Build from source

You need git and Go. Build a release tag, not `main`:

```sh
git clone https://github.com/dortort/wawarden
cd wawarden
git checkout "$(git tag --list 'v*' --sort=-v:refname | head -n 1)"
CGO_ENABLED=0 go build -trimpath -o wawarden ./cmd/wawarden
sudo install -m 0755 wawarden /usr/local/bin/wawarden
```

`CGO_ENABLED=0` builds without cgo, like the release; on Linux that makes a
static binary. The `go` command, 1.21 or later, downloads the Go version on the
`toolchain` line of `go.mod` when the installed one is older. Some
distributions' Go packages, such as Fedora's and Alpine's, set
`GOTOOLCHAIN=local` in their `go.env`, which turns that off. The build then
needs Go 1.26.0 or later, the `go` line of `go.mod`, and uses the installed Go.
`go env GOTOOLCHAIN` shows the setting, and `GOTOOLCHAIN=auto` in front of the
build command turns the download back on.

`wawarden version` of a clean checkout of a release tag prints that release,
except for `v1.0.0` and earlier, which print `dev`; any other source build
prints `dev`. `go version -m wawarden` shows the module version. The
binary is not byte-identical to the release's;
[Reproducing a release](../RELEASING.md#reproducing-a-release) gives the
procedure that is.

### The service user

The [systemd unit](hardening.md#systemd) runs `/usr/local/bin/wawarden` as the
system user and group `wawarden`, and creates the data directory,
`/var/lib/wawarden`. Create the user without a login shell; its home is that
directory:

```sh
sudo useradd --system --user-group --home-dir /var/lib/wawarden --no-create-home --shell /usr/sbin/nologin wawarden
```

The rest of this guide runs `wawarden` as your own user, in the foreground.
The unit takes over for a lasting deployment.

## 1. Prepare the secrets

In a directory that only you can read, generate the admin token. Keep the
token and its hash in separate files. The service is given the hash only. Keep
a copy of the token in a secret manager, from which `--token-command` can
also read it:

```sh
umask 077
wawarden admin init > admin.txt
sed -n 's/^sha256: //p' admin.txt > admin.sha256
sed -n 's/^token: //p' admin.txt > admin.token
rm admin.txt
```

On the second machine, create the key pair that backups are encrypted to.
Keep `backup-identity.txt` there. The service needs only the public key,
`age1...`, that `age-keygen` prints:

```sh
age-keygen -o backup-identity.txt
```

For a webhook, also create its signing secret, readable only by the service's
user:

```sh
openssl rand -hex 32 > notify.secret
```

## 2. Configure and start

Set the owner's number in E.164 form, the hash file, the backup recipient and
a data directory whose parent exists. Then start the service:

```sh
export WAWARDEN_DATA_DIR="$HOME/wawarden-data"
export WAWARDEN_OWNER_PHONE=+15550100001
export WAWARDEN_ADMIN_TOKEN_SHA256_FILE="$PWD/admin.sha256"
export WAWARDEN_BACKUP_AGE_RECIPIENT='age1...'
wawarden serve
```

`serve` runs in the foreground, so run the admin commands of the next steps
in a second terminal. For a lasting deployment, run it under a supervisor
instead; [the systemd unit](hardening.md#systemd) sets the same variables.

Optional variables:

- `WAWARDEN_NOTIFY_URL` and `WAWARDEN_NOTIFY_SECRET_FILE` send every event to a
  signed webhook.
- `WAWARDEN_METRICS_EMF=1` writes metrics on standard output in embedded
  metric format (EMF).

[Environment variables](configuration.md#environment-variables) lists every
variable. Any other `WAWARDEN_` variable stops the start (see
[startup refusals](configuration.md#startup-refusals)).

The start writes JSON lines such as `keys_loaded`, `archive_opened`,
`session_opened` with `"paired":false`, one `listening` per listener and
`ready`. With no device stored, the engine reports `unpaired` and makes no
connection to WhatsApp. Without a backup recipient, every start warns
`backup_disabled`.

The first start creates the master key, `keys/master`, in the data directory.
No backup holds it, so keep a copy of it off the host. Without it, a restored
archive gets a new key: pseudonyms, cursors and message references change, and
`audit verify` reports the older audit rows as `unknown_key` (see
[Restoring a backup](runbook.md#restoring-a-backup)).

## 3. Put it behind HTTPS

The listeners speak plain HTTP on loopback. The agent's host reaches the
client listener through something that encrypts the path and decides who may
connect. Never expose WaWarden to the public internet.

Choose one of the [network patterns](hardening.md#network-patterns):

- [Mesh or VPN forwarding to loopback](hardening.md#mesh-or-vpn-forwarding-to-loopback),
  such as Tailscale Serve. It keeps the service on its defaults. For example,
  `tailscale serve --bg 8080` serves the client listener at
  `https://<machine>.<tailnet>.ts.net/`. Never use `tailscale funnel`, which
  exposes a port to the whole internet.
- [Private subnet](hardening.md#private-subnet), with TLS on top unless you
  control the segment.
- [Reverse proxy with TLS](hardening.md#reverse-proxy-with-tls), optionally
  with client certificates. Read its list of constraints on bodies, timeouts
  and headers.

Expose the client listener, port `8080`, only. Keep the admin listener on the
service's host, or gate it separately (see
[the hardening guide](hardening.md#gate-the-admin-listener-separately)).

Check from the agent's host. Without a token, the service answers `401`:

```sh
curl -i https://<your-host>/v1/me
```

## 4. Pair

```sh
wawarden admin pair --token-file admin.token
```

It prints `pairing code: <code>`, and on standard error where to enter it. On
the owner's phone, in WhatsApp, open Linked devices, choose Link a device,
then Link with phone number instead, and enter the code. The phone's
notification names a `Chrome (Linux)` device; Linked devices then lists it as
`WaWarden (Linux)`. The code goes to you only; it is never logged or kept.

| Refusal | Exit code | What to do |
|---|---|---|
| `owner_phone_missing` | `5` | Set `WAWARDEN_OWNER_PHONE` and restart. |
| `already_paired` | `5` | A device is stored already; see `admin status`. |
| `rate_limited` | `5` | Three attempts were made in the last hour; wait. |
| `pair_failed` | `1` | The connection failed or no code came within 25 seconds. Check outbound access to `web.whatsapp.com` and try again. |

Only the account whose number is `WAWARDEN_OWNER_PHONE` can be linked. A code
entered on another account's phone is refused before anything is stored. If
such a pairing completes, the new device is logged out again. Either way the
service reports `pair_rejected`.

## 5. Watch the status

```sh
wawarden admin status --token-file admin.token
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
backfills today: 0
clients active: 0
clients expired: 0
clients revoked: 0
all-chats clients active: 0
last ingest: 2026-10-05T08:00:00Z
version: <version>
```

`state` is `unpaired`, `connecting`, `connected` or `disconnected`, the last
with a `reason`. Right after pairing, WhatsApp asks the new device to log in
again. The engine handles that as a dropped connection, so expect a short
`connecting` before `connected`.

Each change is logged as `engine_state`, and the gauges `wawarden_paired` and
`wawarden_connected` follow it. Every stop that waits for you is reported as a
`disconnected` event, on standard output and to the webhook.
[Disconnect reasons](runbook.md#disconnect-reasons) says what to do for each
`reason`.

## 6. The first history sync

Once paired, the owner's phone sends the chat history in blobs:

- `history blobs pending` rises as they are announced. It returns to `0` as
  each is downloaded into `history/`, applied to the archive and deleted.
- `chats`, `messages` and `last ingest` grow with it, and so does
  `wawarden_messages_ingested_total`.
- Blobs are downloaded only while connected. A restart takes up those still
  waiting.
- A blob larger than `WAWARDEN_HISTORY_MAX_BYTES` (32 MiB by default), or one
  that fails three times, is quarantined. The `quarantine` event reports it
  with `queue` `history`, and `history blobs quarantined` counts it.

[History sync](internals.md#history-sync) gives the details.

## 7. The first backup

With a backup recipient, the service takes a first backup once both hold:

- no history blob is waiting;
- 10 minutes have passed since the later of the pairing and the latest blob's
  arrival.

It writes `backups/<UTC time>.age`, such as `backups/20261005T120000Z.age`,
and reports `backup_done`, or `backup_failed` with a reason. A failed backup
is tried again after 15 minutes, then after waits that double up to 4 hours.
From then on it takes one backup each night at `WAWARDEN_BACKUP_TIME`,
`03:00` UTC by default, and keeps the newest `WAWARDEN_BACKUP_KEEP`, 14 by
default (see [backups](configuration.md#backups)).

Copy the backup off the host, then check it on the machine that holds the age
identity:

```sh
set -o pipefail
mkdir -m 700 verify
age --decrypt -i backup-identity.txt 20261005T120000Z.age |
  wawarden backup verify --work-dir verify -
```

It prints counts only and ends with `verified: yes`.
[Restoring a backup](runbook.md#restoring-a-backup) gives the restore
procedure.

## 8. The first client

List the chats the agent may read, then create a client for them:

```sh
wawarden admin chats list --match <part of a name> --token-file admin.token
wawarden admin clients create --token-file admin.token \
  --name claude-code --read <id, +E.164 number or ref> --expires-days 90
```

- `--read` takes a chat's `id`, a `+E.164` number or the `ref` that
  `admin chats list` prints. It can be repeated.
- The client expires after 90 days unless `--expires-days` says otherwise,
  365 at most.
- The command prints the client's lines, then `token: ww_...`, once.

Keep the token in a secret manager, or in the agent host's environment as
`WAWARDEN_TOKEN`. Never put it in a repository or in the service's
environment (see
[Registering with Claude Code](api.md#registering-with-claude-code)).

`admin status` now counts the client under `clients active`. Each request it
makes is written on standard output with its chain head (see
[the audit chain](observability.md#audit-chain)).

### A client that sends

A client that sends needs `--write` for each chat it may send to. Each write
chat must also be one of its `--read` chats, and an all-chats client cannot
write:

```sh
wawarden admin clients create --token-file admin.token \
  --name claude-code-reply --read <chat> --write <chat>
```

Sending to a direct chat that has never written to the account is refused
unless the client was created with `--allow-first-contact`. Leave it off
unless the agent must start conversations (see
[the ban-risk notes](ban-risk.md)).

### Register with Claude Code

On the agent's host, register the endpoint at the HTTPS address from
[step 3](#3-put-it-behind-https):

```sh
claude mcp add --transport http whatsapp https://<your-host>/mcp --header "Authorization: Bearer $WAWARDEN_TOKEN"
```

`claude mcp get whatsapp` shows it as connected. `/mcp` in a Claude Code
session lists five tools: `get_changes`, `get_chat`, `get_messages`,
`list_chats` and `search_messages`. A client with write chats also gets
`send_message`.

This form stores the token itself in `~/.claude.json`.
[Registering with Claude Code](api.md#registering-with-claude-code) says what
each `--scope` writes, and how to keep the token out of files with `.mcp.json`
or a `headersHelper`.

Before the agent reads anything, read [the guide for agents](agents.md):
every name and text it returns is third-party text.

## Running in a container

The image runs as `65532:65532`, keeps its data in the `/data` volume and has
no shell. Leave `WAWARDEN_DATA_DIR` unset, pass the hash itself as
`WAWARDEN_ADMIN_TOKEN_SHA256`, and pass the other variables with `-e`. The
full contract is in [the container image](configuration.md#container-image),
and the [Docker Compose example](configuration.md#docker-compose-example)
shows the same settings as a Compose file.

Prepare the secrets with the image, as in [step 1](#1-prepare-the-secrets):

```sh
image=ghcr.io/dortort/wawarden@sha256:<digest>
umask 077
docker run --rm "$image" admin init > admin.txt
sed -n 's/^sha256: //p' admin.txt > admin.sha256
sed -n 's/^token: //p' admin.txt > admin.token
rm admin.txt
```

Start the service. The client listener binds `0.0.0.0` inside the container
and is published on the host's loopback only, for the HTTPS front of
[step 3](#3-put-it-behind-https). The service logs `listener_not_loopback` for
it at every start. The admin listener stays on the container's loopback:

```sh
docker volume create wawarden-data
docker run -d --name wawarden --read-only --stop-timeout 15 \
  --mount type=volume,src=wawarden-data,dst=/data \
  -e WAWARDEN_ADMIN_TOKEN_SHA256="$(cat admin.sha256)" \
  -e WAWARDEN_OWNER_PHONE=+15550100001 \
  -e WAWARDEN_BACKUP_AGE_RECIPIENT='age1...' \
  -e WAWARDEN_LISTEN=0.0.0.0:8080 \
  -p 127.0.0.1:8080:8080 \
  "$image"
docker logs wawarden
docker exec wawarden /wawarden healthcheck && echo healthy
```

Run the admin commands inside the container, with the token on standard
input:

```sh
docker exec -i wawarden /wawarden admin pair --token-stdin < admin.token
docker exec -i wawarden /wawarden admin status --token-stdin < admin.token
docker exec -i wawarden /wawarden admin chats list --token-stdin < admin.token
docker exec -i wawarden /wawarden admin clients create --token-stdin \
  --name claude-code --read <id, +E.164 number or ref> < admin.token
```

Copy a backup out of the volume to check it, as in
[step 7](#7-the-first-backup):

```sh
docker cp wawarden:/data/backups/20261005T120000Z.age .
```

For a webhook, mount its secret file read-only and point
`WAWARDEN_NOTIFY_SECRET_FILE` at it inside the container. It must be readable
by `65532:65532`, and grant no write permission to its group and no
permission to others:

```sh
sudo chown 65532:65532 notify.secret
sudo chmod 0400 notify.secret
# add to the docker run command:
#   --mount type=bind,src="$PWD/notify.secret",dst=/run/secrets/notify.secret,readonly
#   -e WAWARDEN_NOTIFY_URL=https://<webhook>
#   -e WAWARDEN_NOTIFY_SECRET_FILE=/run/secrets/notify.secret
```

The image declares no health check. Use `["/wawarden","healthcheck"]` in exec
form in Compose or Kubernetes; `docker run --health-cmd` needs a shell the
image does not have. [Container hardening](hardening.md#container-hardening)
covers a sidecar for Tailscale and the other settings.

## You now have

Each item has a command that confirms it. In a container, run the
`wawarden` commands with `docker exec -i wawarden /wawarden ... --token-stdin < admin.token`.

- [ ] **A healthy service under a supervisor.**
  `wawarden healthcheck && echo healthy` prints `healthy`.
- [ ] **A paired, connected account.**
  `wawarden admin status --token-file admin.token | grep -E '^(state|paired):'`
  prints `state: connected` and `paired: true`.
- [ ] **A settled history sync.** The same status shows
  `history blobs pending: 0`.
- [ ] **A verified backup off the host.** `ls "$WAWARDEN_DATA_DIR/backups"`
  lists a `.age` file, and `backup verify` printed `verified: yes` on the
  second machine.
- [ ] **A copy of the master key off the host.**
  `sha256sum "$WAWARDEN_DATA_DIR/keys/master"` matches the copy's checksum.
- [ ] **The client listener behind HTTPS only.**
  `curl -i https://<your-host>/v1/me` from the agent's host answers
  `401 Unauthorized`.
- [ ] **A scoped client.**
  `wawarden admin clients list --token-file admin.token` shows it as
  `state: active`, with its `expires` date.
- [ ] **The MCP endpoint registered.** `claude mcp get whatsapp` shows it as
  connected.

## What next

- [Disconnect reasons](runbook.md#disconnect-reasons): what each `reason` of a
  `disconnected` engine means and what to do.
- [Unpairing and pairing again](runbook.md#unpairing-and-pairing-again).
- [Rotating a client token](runbook.md#rotating-a-client-token) before it
  expires.
- [Retiring other linked devices](runbook.md#retiring-other-linked-devices)
  that the agent used before.
- [The hardening guide's checklist](hardening.md#checklist) and its
  [alarms](hardening.md#alarms).
- [The runbook](runbook.md) for upgrades, restores and incidents.
