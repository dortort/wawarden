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

## Contents

- [What WhatsApp's published documents say](#what-whatsapps-published-documents-say)
- [What the protocol library knows](#what-the-protocol-library-knows)
- [What WaWarden does to lower the risk](#what-wawarden-does-to-lower-the-risk)
- [What the operator must do](#what-the-operator-must-do)
- [What happens on a ban or a logout](#what-happens-on-a-ban-or-a-logout)

## What WhatsApp's published documents say

The pages below were read on 2026-10-11 and are paraphrased, not quoted; the
pages themselves are authoritative and can change.

- **Terms of Service** (<https://www.whatsapp.com/legal/terms-of-service>). The
  section on legal and acceptable use allows only legal, authorised and
  acceptable use, and rules out bulk messaging, auto-messaging and
  auto-dialing, and use that is not personal unless WhatsApp authorises it. The
  section on harm to WhatsApp and its users rules out reverse engineering,
  accessing the service by unauthorised or automated means, making the service
  available over a network to several devices at once except through
  WhatsApp's own tools, and offering third parties software or interfaces that
  work substantially like WhatsApp's without authorisation. The section on
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
  scale without authorisation leads to limits or removal. WaWarden is not that
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
terms or risk: the protocol library gives no assurance either way.

The protocol library decodes five reason codes for a temporary ban. They come
from its authors' reverse engineering, not from a WhatsApp policy page, and are listed
here as decoded by whatsmeow:

| Code | Reason, as decoded by whatsmeow | What in WaWarden bears on it |
|---|---|---|
| 101 | Sent too many messages to people who do not have the account in their address book | The first-contact rule: a client sends to a direct chat that has never written to the account only when the operator allowed it first contact |
| 102 | Too many people blocked the account | Send only where the recipient expects a message; the operator chooses each write chat |
| 103 | Created too many groups with people who do not have the account in their address book | WaWarden has no code that creates a group, and an architecture test refuses every call of the protocol library that the gateway does not make |
| 104 | Sent the same message to too many people | The pacer and the send limits; an agent must never send one text to many chats |
| 106 | Sent too many messages to a broadcast list | WaWarden has no code that sends to a broadcast list |

WaWarden does not pass the code on: a temporary ban appears as the engine's
`disconnected` state with reason `temporary_ban`, and the phone shows
WhatsApp's own notice.

Community reports in the protocol library's issue tracker describe in-app warnings
about third-party tools and bulk messages, bans after a burst of a few sends,
and bans of virtual or landline numbers. They are reports, not policy, and
this project has not verified them. Whether running from a datacenter address
raises the risk is likewise unknown.

## What WaWarden does to lower the risk

| Measure | Where it is described |
|---|---|
| One linked device per service. Pairing is refused while a device is paired, allowed at most 3 times an hour, and only for the number in `WAWARDEN_OWNER_PHONE`; a device that turns out to belong to another account is logged out again and reported as `pair_rejected`. | [Pairing](internals.md#pairing), [pairing refusals](threat-model.md#pairing-refusals) |
| It links as a desktop browser companion through the phone-number pairing of a `Chrome (Linux)` client, and Linked devices lists it as `WaWarden (Linux)`. | [`admin pair`](cli.md#admin-status-admin-pair-and-admin-reconnect), [Pairing](internals.md#pairing) |
| Read-only by default. A client sends only to the chats the operator listed for it with `--write`, and a client that reads every chat can never send. | [Admin clients](cli.md#admin-clients-and-admin-chats) |
| No first contact. A send to a direct chat that has never written to the account is refused `403` unless the client was created with `--allow-first-contact`. | [Sending](api.md#sending) |
| Paced and budgeted sends. One send at a time across all clients, and at least 3 seconds plus up to 2 more between the starts of two sends; 10 sends a minute per client; 30 an hour for all clients together, shared out among the clients that may write; caps of 10 and 60 that only `WAWARDEN_UNSAFE_SEND_CAPS` lifts. A send over a limit is refused `429`, never queued. | [Send limits](api.md#send-limits) |
| At most once. One idempotency key sends at most one message, and a send whose outcome is unknown is never sent again by the service. | [Sending](api.md#sending) |
| Text only. Sends carry text, optionally as a reply in the same chat: no media, groups, broadcasts or status. | [What a send does on the wire](internals.md#what-a-send-does-on-the-wire) |
| Bounded history requests. `admin backfill` is allowed once every 5 minutes and 20 times a UTC day, for at most 50 messages. A request before the chat's oldest archived message and one before its newest are the same history request to the owner's phone and share these caps. | [`admin backfill`](cli.md#admin-backfill) |
| Attachment downloads on demand only, and off by default (`WAWARDEN_MEDIA_MAX_BYTES=0`). An attachment is fetched only when a client created with `allow_media` asks for it, never when a message arrives, and view-once media is never fetched. A fetch moves at most its declared length plus 32 bytes per response and twice that over its at most 6 requests. Each client starts at most 6 fetches a minute by default, with a burst of 2, and holds at most 2 jobs; all clients together start at most 60 fetches and 4 GiB an hour by default; no unsafe caps switch lifts these. A 429 or 5xx from WhatsApp ends the fetch and holds every new one for WhatsApp's `Retry-After`, kept between 30 seconds and 10 minutes, or 1 minute when it gives none. A 403, 404 or 410 marks the attachment expired, and it is not fetched again, even when a backfill re-delivers its message. An attachment that answered so under a release before v1.3.0 kept no mark, so after a re-delivery its next fetch reaches the media host once more and marks it (see the backfill residual under [Account and protocol](threat-model.md#account-and-protocol)). No media retry receipt is ever sent. | [Attachments](api.md#attachments), [Media transport](internals.md#media-transport) |
| No read receipts, presence, typing indicators or status updates; an architecture test refuses the protocol library calls that send them. | [Traffic the protocol library sends by itself](internals.md#traffic-the-protocol-library-sends-by-itself) |
| Measured reconnection. The protocol library's own reconnection is off; the engine waits 2 seconds, doubling to 5 minutes, and stops for the operator on a replaced session, a ban, a logout or an outdated client. More than 5 starts in 10 minutes keep it from connecting. | [Engine states](internals.md#engine-states) |
| A current protocol version, fetched when the engine starts with a paired device and again when WhatsApp reports the client as outdated, with bounded retries; after no fetch succeeded, an explicit reconnect fetches before it connects. | [Engine states](internals.md#engine-states) |
| The traffic the protocol library sends by itself, such as acknowledgements and inactive delivery receipts, is listed. | [Traffic the protocol library sends by itself](internals.md#traffic-the-protocol-library-sends-by-itself) |

## What the operator must do

- Keep the owner's phone online and use WhatsApp on it regularly; a linked
  device is logged out when the phone stays unused too long (see the Help
  Center above). A linked device cannot see whether the phone is online; the
  `Paired` alarm is the only sign.
- Run one service per account. A second process started from a copy of
  `session.db` takes over the session of the first, which reports `replaced`.
- Personal use only: no marketing, customer or bulk messaging.
- Keep clients with write chats few and narrow, give each only the chats it must answer,
  and leave `--allow-first-contact` off unless the task needs it.
- Never let an agent send the same text to many chats, and keep its prompt
  from turning one inbound message into several sends
  ([agents](agents.md#sending)).
- Log out other linked devices that did the same job
  ([hardening](hardening.md#unlink-other-devices)).
- Do not re-pair in a loop after a logout.
- Pause attachment downloads when a ban is suspected or WhatsApp keeps
  throttling fetches ([pausing downloads](runbook.md#pausing-downloads)). The
  pause lasts until a restart; `WAWARDEN_MEDIA_MAX_BYTES=0` keeps downloads off
  across restarts.
- Leave `WAWARDEN_UNSAFE_SEND_CAPS` unset.

## What happens on a ban or a logout

The engine stops and waits for the operator. The process keeps running,
clients keep reading the archive, and `/healthz` stays `200`. List pages,
`/v1/me` and MCP results carry `session.state` as `disconnected` or
`unpaired`. The events and metrics to alarm on are `unpaired`, `disconnected` with its
`reason`, `Paired` below 1 and `Connected` at 0
([hardening](hardening.md#alarms)).

Every `reason` and what to do about it is in the runbook under
[Disconnect reasons](runbook.md#disconnect-reasons). The reasons that bear on
a ban:

- **`temporary_ban`**: WhatsApp banned the account for a while. Wait for the
  ban to end, as the phone shows, then reconnect:
  `wawarden admin reconnect --token-file admin.token`.
- **`logged_out`**: the device was logged out, by the phone or by WhatsApp. It
  is deleted and the service is `unpaired`. Find out why before pairing again
  (below).
- **`connect_failure`**: WhatsApp refused the connection. Look at the phone
  before you reconnect.
- **`replaced`**: another linked device or protocol client took over the
  session, such as a second service started from a copy of `session.db`. This
  is not WhatsApp acting on the account: stop the other one, then reconnect.

WhatsApp's reason for a logout is not passed on: a logout from the phone and
one that WhatsApp imposed, whatever the cause, both arrive as `logged_out`.
Before pairing again:

1. Look at the owner's phone and in WhatsApp for a notice of a restriction or a
   ban. Pairing a banned account does not work, and the service allows only 3
   pairing attempts an hour.
2. Revoke the clients that can send while the cause is unknown:
   `wawarden admin clients revoke --id <id> --token-file admin.token`.
3. With attachment downloads on, pause them, so that nothing is fetched once
   the device is paired again
   ([pausing downloads](runbook.md#pausing-downloads)).
4. Keep a backup of the archive: it holds the history, and stays readable
   whatever happens to the account.
5. Contact WhatsApp support through the app if the account is restricted or
   banned; the steps are WhatsApp's, not this project's.
6. Pair again only once the phone works normally
   ([runbook](runbook.md#suspected-ban)).
