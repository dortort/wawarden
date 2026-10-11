# Ban risk

**WaWarden is an unofficial client.** It links to a personal WhatsApp account
through an unofficial implementation of WhatsApp's multi-device protocol.
WhatsApp can restrict, log out or ban the account at any time and without
notice, and nothing guarantees that an appeal succeeds. Use WaWarden only on an
account you can afford to lose, and only for personal use. This page is not
legal advice.

It summarises what WhatsApp's published documents say, what the protocol
library knows about bans, what WaWarden does to lower the risk, what the
operator must do, and what happens when WhatsApp acts. It describes the code at
the commit or tag you are reading.

## What WhatsApp's published documents say

The pages below were read on 2026-10-11 and are paraphrased, not quoted; the
pages themselves are authoritative and can change.

- **Terms of Service** (<https://www.whatsapp.com/legal/terms-of-service>). The
  section on legal and acceptable use allows only legal, authorized and
  acceptable use, and rules out bulk messaging, auto-messaging and
  auto-dialing, and use that is not personal unless WhatsApp authorizes it. The
  section on harm to WhatsApp and its users rules out reverse engineering,
  accessing the service by unauthorized or automated means, making the service
  available over a network to several devices at once except through
  WhatsApp's own tools, and offering third parties software or interfaces that
  work substantially like WhatsApp's without authorization. The section on
  availability and termination lets WhatsApp suspend or end access at any time,
  for any reason, including breaking the letter or the spirit of the terms.
- **Messaging Guidelines** (<https://www.whatsapp.com/legal/messaging-guidelines>).
  They list adversarial behaviour, which includes unofficial clients and bulk
  or automated messaging or dialing used to harm WhatsApp or its users, and
  describe spam as repeated unwanted contact. The actions they name are
  warnings, temporary or permanent suspension, and restricting what an account
  can do in chats and groups. Review is automated and human, based on limited
  information such as basic profile data and messages that other users report;
  message content stays end-to-end encrypted. What triggers action is
  therefore most likely volume, unwanted contact and reports from recipients,
  not what the messages say.
- **Business Messaging Policy** (<https://business.whatsapp.com/policy>). It
  governs the WhatsApp Business Platform, the channel WhatsApp sanctions for
  automated messaging, where contacts must opt in and messaging people at
  scale without authorization leads to limits or removal. WaWarden is not that
  channel: do not use it for marketing or customer messaging.
- **Security whitepaper**
  (<https://www.whatsapp.com/security/WhatsApp-Security-Whitepaper.pdf>). Its
  section on removing companion devices says a companion can log itself out,
  be logged out by the primary device, or be logged out by WhatsApp's server.
- **Help Center.** "About linked devices"
  (<https://faq.whatsapp.com/378279804439436>) describes how linked devices
  work and when they are logged out after the primary phone has not been used
  for a while; the period is reported as 14 days, but this project has not
  quoted the page, so read the period there. "Unauthorized use of automated or
  bulk messaging on WhatsApp" (<https://faq.whatsapp.com/5957850900902049>) and
  "About restricted accounts" (<https://faq.whatsapp.com/717472490411581>)
  cover automation and restrictions. These pages are named, not quoted: read
  them in a browser.

## What the protocol library knows

WaWarden is built on `go.mau.fi/whatsmeow`. Its README says nothing about bans,
terms or risk: the library gives no assurance either way.

The library decodes five reason codes for a temporary ban. They come from the
library's reverse engineering, not from a WhatsApp policy page, and are listed
here as decoded by whatsmeow:

| Code | Reason, as decoded by whatsmeow | What in WaWarden bears on it |
|---|---|---|
| 101 | Sent too many messages to people who do not have the account in their address book | The first-contact rule: a client sends to a direct chat that has never written to the account only when the operator allowed it first contact |
| 102 | Too many people blocked the account | Send only where the recipient expects a message; the operator chooses each write chat |
| 103 | Created too many groups with people who do not have the account in their address book | WaWarden has no code that creates a group |
| 104 | Sent the same message to too many people | The pacer and the send limits; an agent must never send one text to many chats |
| 106 | Sent too many messages to a broadcast list | WaWarden has no code that sends to a broadcast list |

WaWarden does not pass the code on: a temporary ban appears as the engine's
`disconnected` state with reason `temporary_ban`, and the phone shows
WhatsApp's own notice.

Community reports in the library's issue tracker describe in-app warnings
about third-party tools and bulk messages, bans after a burst of a few sends,
and bans of virtual or landline numbers. They are reports, not policy, and
this project has not verified them. Whether running from a datacenter address
raises the risk is likewise unknown.

## What WaWarden does to lower the risk

| Measure | Where it is described |
|---|---|
| One linked device per service. Pairing is refused while a device is paired, allowed at most 3 times an hour, and only for the number in `WAWARDEN_OWNER_PHONE`; a device that turns out to belong to another account is logged out again and reported as `pair_rejected`. | [Pairing](configuration.md#pairing), [pairing refusals](threat-model.md#pairing-refusals-m1) |
| It links as a desktop browser companion named `Chrome (Linux)`. | `internal/engine/wa/client.go` |
| Read-only by default. A client sends only to the chats the operator listed for it with `--write`, and a client that reads every chat can never send. | [Admin clients](configuration.md#admin-clients-and-admin-chats) |
| No first contact. A send to a direct chat that has never written to the account is refused `403` unless the client was created with `--allow-first-contact`. | [Sending](configuration.md#sending) |
| Paced and budgeted sends. One send at a time across all clients, then 3 seconds plus up to 2 more before the next; 10 sends a minute per client; 30 an hour for all clients together, shared out among the clients that may write; caps of 10 and 60 that only `WAWARDEN_UNSAFE_SEND_CAPS` lifts. A send over a limit is refused `429`, never queued. | [Send limits](configuration.md#send-limits) |
| At most once. One idempotency key sends at most one message, and a send whose outcome is unknown is never sent again by the service. | [Sending](configuration.md#sending) |
| Text only. Sends carry text, optionally as a reply in the same chat: no media, groups, broadcasts or status. | [What a send does on the wire](configuration.md#what-a-send-does-on-the-wire) |
| Bounded history requests. `admin backfill` is allowed once every 5 minutes and 20 times a UTC day, for at most 50 messages. | [`admin backfill`](configuration.md#admin-backfill) |
| No read receipts, presence, typing indicators or status updates; an architecture test refuses the library calls that send them. | [Traffic the protocol library sends by itself](configuration.md#traffic-the-protocol-library-sends-by-itself) |
| Measured reconnection. The library's own reconnection is off; the engine waits 2 seconds, doubling to 5 minutes, and stops for the operator on a replaced session, a ban, a logout or an outdated client. More than 5 starts in 10 minutes keep it from connecting. | [Engine states](configuration.md#engine-states) |
| A current protocol version, fetched when the engine starts and on an explicit reconnect, with bounded retries. | [Engine states](configuration.md#engine-states) |
| The traffic the library sends by itself, such as acknowledgements and inactive delivery receipts, is listed. | [Traffic the protocol library sends by itself](configuration.md#traffic-the-protocol-library-sends-by-itself) |

## What the operator must do

- Keep the owner's phone online and use WhatsApp on it regularly; a linked
  device is logged out when the phone stays unused too long (see the Help
  Center above). A linked device cannot see whether the phone is online; the
  `Paired` alarm is the only sign.
- Run one service per account. A second process started from a copy of
  `session.db` takes over the session of the first, which reports `replaced`.
- Personal use only: no marketing, customer or bulk messaging.
- Keep writing clients few and narrow, give each only the chats it must answer,
  and leave `--allow-first-contact` off unless the task needs it.
- Never let an agent send the same text to many chats, and keep its prompt
  from turning one inbound message into several sends
  ([agents](agents.md#sending)).
- Log out other linked devices that did the same job
  ([hardening](hardening.md#unlink-other-devices)).
- Do not re-pair in a loop after a logout.
- Leave `WAWARDEN_UNSAFE_SEND_CAPS` unset.

## What happens on a ban or a logout

The engine stops and waits for the operator; the process keeps running,
clients keep reading the archive, `/healthz` stays `200`, and list pages,
`/v1/me` and MCP results carry `session.state` as `disconnected` or
`unpaired`.
The events and metrics to alarm on are `unpaired`, `disconnected` with its
`reason`, `Paired` below 1 and `Connected` at 0
([hardening](hardening.md#alarms)).

| `reason` | Meaning | Next step |
|---|---|---|
| `temporary_ban` | WhatsApp banned the account for a while. | Wait for the ban to end, as the phone shows, then `wawarden admin reconnect`. |
| `logged_out` | The device was logged out, by the phone or by WhatsApp; it is deleted and the service is `unpaired`. | Find out why before pairing again (below). |
| `replaced` | Another client took over the session. | Stop the other one, then `admin reconnect`. |
| `connect_failure` | WhatsApp refused the connection. | Look at the phone, then `admin reconnect`. |

WhatsApp's reason for a logout is not passed on: a logout from the phone and
one that WhatsApp imposed, whatever the cause, both arrive as `logged_out`.
Before pairing again:

1. Look at the owner's phone and in WhatsApp for a notice of a restriction or a
   ban. Pairing a banned account will not work, and the service allows only 3
   pairing attempts an hour.
2. Revoke the clients that can send while the cause is unknown
   (`wawarden admin clients revoke --id ID`).
3. Keep a backup of the archive: it holds the history, and stays readable
   whatever happens to the account.
4. Contact WhatsApp support through the app if the account is restricted or
   banned; the steps are WhatsApp's, not this project's.
5. Pair again only once the phone works normally
   ([runbook](runbook.md#suspected-ban)).
