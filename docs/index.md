# WaWarden

WaWarden is a self-hosted WhatsApp gateway. It links to one personal WhatsApp
account as a linked device and keeps that account's messages in a local
archive. Your own AI agents and applications reach the archive over REST and
MCP. Each client holds its own token. A token reads only the chats on its
allowlist and writes only to its write chats.

WaWarden is not an official WhatsApp API. It is not affiliated with WhatsApp or
Meta. WhatsApp can restrict, log out or ban an account that links an unofficial
client: read the [ban-risk notes](ban-risk.md) first.

## Who it is for

- **Owners of a personal account** who want an agent, such as Claude Code, to
  read and answer some of their chats, and no others.
- **Operators of one Linux host**, run under systemd or as a container, behind
  a VPN, a private network or a reverse proxy that terminates TLS.
- **Builders of agents and scripts** that need a stable API with scoped tokens
  instead of the account itself.

It is not a hosted or multi-account service, a network security layer or a bulk
messaging tool.

## Documentation

<div class="grid cards" markdown>

-   **[Getting started](getting-started.md)**

    ---

    From prerequisites to a paired account, a first backup and a first client.

-   **[Configuration](configuration.md)**

    ---

    Every environment variable, the startup refusals, the data directory,
    storage and backups.

-   **[CLI](cli.md)**

    ---

    Every subcommand, its flags and the exit codes.

-   **[API](api.md)**

    ---

    The client API, the admin routes, MCP and registering with Claude Code.

-   **[Operations](runbook.md)**

    ---

    The runbook for routine work and incidents, and the
    [ban-risk notes](ban-risk.md).

-   **[Security](threat-model.md)**

    ---

    The [threat model](threat-model.md) and the
    [hardening guide](hardening.md).

-   **[For agents](agents.md)**

    ---

    How an agent treats what it reads, pages, retries, sends and stops.

-   **[Reference](glossary.md)**

    ---

    The [glossary](glossary.md), the [internals](internals.md) and
    [observability](observability.md).

-   **[Project](../CONTRIBUTING.md)**

    ---

    [Contributing](../CONTRIBUTING.md), the [changelog](../CHANGELOG.md),
    [releasing](../RELEASING.md) and the [security policy](../SECURITY.md).

</div>
