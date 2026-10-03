# WaWarden

A self-hosted WhatsApp gateway: one personal account, linked as a device, exposed to your own AI agents and applications with per-client, per-chat scoped access (read-only or read-write).

WaWarden issues tokens and enforces what each token may read and write. It does not restrict network access: it listens on loopback by default and speaks plain HTTP, so securing the network path to it (a VPN, a private subnet, a reverse proxy with TLS) is the deployer's responsibility. Never expose it to the public internet.

Status: pre-release, under construction.
