package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/dortort/wawarden/internal/policy"
)

const (
	SendLease = 60 * time.Second
	SendTTL   = 24 * time.Hour

	sendMinute        = time.Minute
	sendHour          = time.Hour
	sendHandlerBudget = 25 * time.Second
	minInProgressWait = time.Second
	maxInProgressWait = 5 * time.Second
	minRetryAfter     = time.Second
	maxRetryAfter     = time.Hour

	statusSent     = 201
	statusRejected = 502
	statusUnknown  = 504
	CodeRejected   = "send_rejected"
	CodeUnknown    = "send_outcome_unknown"

	statePending = "pending"
	stateDone    = "done"
	stateFailed  = "failed"
)

const (
	sweepSendKey = "DELETE FROM idempotency WHERE client_id = ?1 AND key = ?2 AND expires_at <= ?3"
	insertSend   = `INSERT INTO idempotency (client_id, key, state, fingerprint, msg_id, reserved_at, next_ok_at, counted, expires_at)
		VALUES (?1, ?2, 'pending', ?3, ?4, ?5, ?6, 1, ?7)
		ON CONFLICT (client_id, key) DO NOTHING RETURNING 1`
	selectSend        = "SELECT state, fingerprint, msg_id, result, reserved_at FROM idempotency WHERE client_id = ?1 AND key = ?2"
	expireSendLease   = "UPDATE idempotency SET state = 'failed', settled_at = ?3, result = ?4 WHERE client_id = ?1 AND key = ?2 AND state = 'pending'"
	selectSentBy      = "SELECT sender_jid FROM messages WHERE chat_jid = ?1 AND id = ?2 AND sent_by = ?3"
	selectInbound     = "SELECT EXISTS (SELECT 1 FROM messages WHERE chat_jid = ?1 AND from_me = 0)"
	selectSendWindows = `SELECT
		(SELECT count(*) FROM idempotency WHERE client_id = ?1 AND counted = 1 AND reserved_at > ?2),
		(SELECT count(*) FROM idempotency WHERE client_id = ?1 AND counted = 1 AND reserved_at > ?3),
		(SELECT count(*) FROM idempotency WHERE counted = 1 AND reserved_at > ?3),
		(SELECT coalesce(max(next_ok_at), 0) FROM idempotency WHERE counted = 1 AND reserved_at > ?3 AND NOT (client_id = ?1 AND key = ?4))`
	selectClientAdmit = `SELECT reserved_at FROM idempotency WHERE client_id = ?1 AND counted = 1 AND reserved_at > ?2 AND key != ?3
		ORDER BY reserved_at LIMIT 1 OFFSET ?4`
	selectGlobalAdmit = `SELECT reserved_at FROM idempotency WHERE counted = 1 AND reserved_at > ?2 AND NOT (client_id = ?1 AND key = ?3)
		ORDER BY reserved_at LIMIT 1 OFFSET ?4`
	settleSend   = "UPDATE idempotency SET state = ?3, settled_at = ?4, result = ?5 WHERE client_id = ?1 AND key = ?2 AND state = 'pending' RETURNING msg_id"
	releaseSend  = "DELETE FROM idempotency WHERE client_id = ?1 AND key = ?2 AND state = 'pending'"
	recoverSends = "UPDATE idempotency SET state = 'failed', settled_at = ?1, result = ?2 WHERE state = 'pending'"
	sweepSends   = "DELETE FROM idempotency WHERE expires_at <= ?1"
	markSentBy   = "UPDATE messages SET sent_by = ?1 WHERE chat_jid = ?2 AND id = ?3 AND sender_jid = ?4 AND sent_by IS NULL"
)

var (
	ErrKeyReused    = errors.New("ingest: the idempotency key was used for another request")
	ErrFirstContact = errors.New("ingest: the chat has never written to the account and the client may not make first contact")
	ErrNotReady     = errors.New("ingest: the engine cannot send now")
	ErrNotPending   = errors.New("ingest: no pending send holds the idempotency key")
	ErrNotAllowed   = errors.New("ingest: the write grant does not cover the chat")
	errCorruptSend  = errors.New("ingest: a stored send record is not well formed")
)

type SendLimits struct {
	PerMinute     int
	GlobalPerHour int
	Holders       int
}

func (l SendLimits) share() int { return max(1, l.GlobalPerHour/max(l.Holders, 1)) }

type SendReservation struct {
	Key         Key
	Chat        policy.CanonicalChat
	Fingerprint [fingerprintSize]byte
	MsgID       string
	Now         time.Time
	Limits      SendLimits
	PacerGap    time.Duration
	Ready       func() bool
}

type ReserveKind uint8

const (
	Reserved ReserveKind = iota + 1
	ReplayedDone
	ReplayedFailed
	InProgress
	LeaseExpired
)

type SentRow struct {
	Chat     policy.CanonicalChat
	ID       string
	Sender   policy.CanonicalChat
	Inserted bool
}

type Reservation struct {
	Kind       ReserveKind
	MsgID      string
	Sent       SentRow
	Status     int
	Code       string
	WACode     int
	AckedAt    time.Time
	RetryAfter time.Duration
}

type RateReason uint8

const (
	ReasonMinute RateReason = iota + 1
	ReasonHour
	ReasonGlobal
	ReasonPacer
)

func (r RateReason) String() string {
	switch r {
	case ReasonMinute:
		return "budget_minute"
	case ReasonHour:
		return "budget_hour"
	case ReasonGlobal:
		return "budget_global"
	case ReasonPacer:
		return "pacer"
	}
	return "unknown"
}

type RateLimitError struct {
	Reason     RateReason
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return "ingest: the send is refused by the " + e.Reason.String() + " limit"
}

type sendResult struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	WA     int    `json:"wa,omitempty"`
	TS     int64  `json:"ts,omitempty"`
}

func encodeResult(r sendResult) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func decodeResult(s sql.NullString) (sendResult, error) {
	var r sendResult
	if !s.Valid || json.Unmarshal([]byte(s.String), &r) != nil || r.Status == 0 {
		return sendResult{}, errCorruptSend
	}
	return r, nil
}

func (in SendReservation) check(g policy.WriteGrant) error {
	switch {
	case !g.Valid() || !in.Chat.Valid() || !g.Allows(in.Chat):
		return ErrNotAllowed
	case !in.Key.Valid():
		return invalid("idempotency key")
	case !messageID.MatchString(in.MsgID):
		return invalid("message id")
	case in.Now.IsZero():
		return invalid("time")
	case in.Limits.PerMinute <= 0 || in.Limits.GlobalPerHour <= 0 || in.Limits.Holders < 0:
		return invalid("send limits")
	case in.PacerGap < 0:
		return invalid("pacer gap")
	case in.Ready == nil:
		return invalid("readiness probe")
	}
	return nil
}

func (s *Store) ReserveSend(g policy.WriteGrant, ctx context.Context, in SendReservation) (Reservation, error) {
	if err := in.check(g); err != nil {
		return Reservation{}, err
	}
	var res Reservation
	err := s.Write(ctx, "ingest.send_reserve", func(tx *Tx) error {
		var err error
		res, err = tx.reserveSend(g, in)
		return err
	})
	if err != nil {
		return Reservation{}, err
	}
	return res, nil
}

func (tx *Tx) reserveSend(g policy.WriteGrant, in SendReservation) (Reservation, error) {
	client, now := g.Client(), ms(in.Now)
	if _, err := tx.q.ExecContext(tx.ctx, sweepSendKey, client, in.Key.s, now); err != nil {
		return Reservation{}, err
	}
	var one int
	err := tx.q.QueryRowContext(tx.ctx, insertSend, client, in.Key.s, in.Fingerprint[:], in.MsgID, now,
		ms(in.Now.Add(in.PacerGap)), ms(in.Now.Add(SendTTL))).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tx.replaySend(client, in)
	case err != nil:
		return Reservation{}, err
	}
	chat, err := tx.Canonical(in.Chat)
	if err != nil {
		return Reservation{}, err
	}
	var inbound bool
	if user(chat) {
		if err := tx.q.QueryRowContext(tx.ctx, selectInbound, chat.JID()).Scan(&inbound); err != nil {
			return Reservation{}, err
		}
	}
	if !policy.MayContact(chat, inbound, g.AllowFirstContact()) {
		return Reservation{}, ErrFirstContact
	}
	if !in.Ready() {
		return Reservation{}, ErrNotReady
	}
	if err := tx.admit(client, in); err != nil {
		return Reservation{}, err
	}
	return Reservation{Kind: Reserved, MsgID: in.MsgID}, nil
}

func (tx *Tx) replaySend(client string, in SendReservation) (Reservation, error) {
	var state, msgID string
	var fingerprint []byte
	var result sql.NullString
	var reserved int64
	if err := tx.q.QueryRowContext(tx.ctx, selectSend, client, in.Key.s).Scan(&state, &fingerprint, &msgID, &result, &reserved); err != nil {
		return Reservation{}, err
	}
	if !bytes.Equal(fingerprint, in.Fingerprint[:]) {
		return Reservation{}, ErrKeyReused
	}
	switch state {
	case statePending:
		age := in.Now.Sub(fromMS(reserved))
		if age < SendLease {
			return Reservation{Kind: InProgress, MsgID: msgID, RetryAfter: inProgressWait(age)}, nil
		}
		stored, err := encodeResult(sendResult{Status: statusUnknown, Code: CodeUnknown})
		if err != nil {
			return Reservation{}, err
		}
		if _, err := tx.q.ExecContext(tx.ctx, expireSendLease, client, in.Key.s, ms(in.Now), stored); err != nil {
			return Reservation{}, err
		}
		return Reservation{Kind: LeaseExpired, MsgID: msgID, Status: statusUnknown, Code: CodeUnknown}, nil
	case stateDone:
		r, err := decodeResult(result)
		if err != nil {
			return Reservation{}, err
		}
		sent, err := tx.sentRow(client, in.Chat, msgID)
		if err != nil {
			return Reservation{}, err
		}
		return Reservation{Kind: ReplayedDone, MsgID: msgID, Sent: sent, Status: r.Status, AckedAt: fromMS(r.TS)}, nil
	case stateFailed:
		r, err := decodeResult(result)
		if err != nil {
			return Reservation{}, err
		}
		return Reservation{Kind: ReplayedFailed, MsgID: msgID, Status: r.Status, Code: r.Code, WACode: r.WA}, nil
	}
	return Reservation{}, errCorruptSend
}

func (tx *Tx) sentRow(client string, requested policy.CanonicalChat, msgID string) (SentRow, error) {
	chat, err := tx.Canonical(requested)
	if err != nil {
		return SentRow{}, err
	}
	var jid string
	switch err := tx.q.QueryRowContext(tx.ctx, selectSentBy, chat.JID(), msgID, client).Scan(&jid); {
	case errors.Is(err, sql.ErrNoRows):
		return SentRow{}, ErrNotFound
	case err != nil:
		return SentRow{}, err
	}
	sender, ok := policy.Normalize(jid)
	if !ok || sender.JID() != jid || !user(sender) {
		return SentRow{}, errCorruptChat
	}
	return SentRow{Chat: chat, ID: msgID, Sender: sender}, nil
}

func inProgressWait(age time.Duration) time.Duration {
	return min(max(wholeSeconds(sendHandlerBudget-age), minInProgressWait), maxInProgressWait)
}

func wholeSeconds(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return (d + time.Second - 1) / time.Second * time.Second
}

type sendLimit struct {
	reason RateReason
	count  int64
	limit  int64
	global bool
	window time.Duration
}

func (tx *Tx) admit(client string, in SendReservation) error {
	now := ms(in.Now)
	minuteStart, hourStart := now-sendMinute.Milliseconds(), now-sendHour.Milliseconds()
	var perMinute, perHour, global, nextOK int64
	if err := tx.q.QueryRowContext(tx.ctx, selectSendWindows, client, minuteStart, hourStart, in.Key.s).Scan(&perMinute, &perHour, &global, &nextOK); err != nil {
		return err
	}
	var reason RateReason
	var wait int64
	for _, l := range []sendLimit{
		{ReasonMinute, perMinute, int64(in.Limits.PerMinute), false, sendMinute},
		{ReasonHour, perHour, int64(in.Limits.share()), false, sendHour},
		{ReasonGlobal, global, int64(in.Limits.GlobalPerHour), true, sendHour},
	} {
		if l.count <= l.limit {
			continue
		}
		oldest, err := tx.admittedAfter(l.global, client, in.Key.s, now-l.window.Milliseconds(), l.count-1-l.limit)
		if err != nil {
			return err
		}
		if w := oldest + l.window.Milliseconds() - now; reason == 0 || w > wait {
			reason, wait = l.reason, w
		}
	}
	if now < nextOK && (reason == 0 || nextOK-now > wait) {
		reason, wait = ReasonPacer, nextOK-now
	}
	if reason == 0 {
		return nil
	}
	return &RateLimitError{Reason: reason, RetryAfter: min(max(wholeSeconds(time.Duration(wait)*time.Millisecond), minRetryAfter), maxRetryAfter)}
}

func (tx *Tx) admittedAfter(global bool, client, key string, start, offset int64) (int64, error) {
	var row *sql.Row
	if global {
		row = tx.q.QueryRowContext(tx.ctx, selectGlobalAdmit, client, start, key, offset)
	} else {
		row = tx.q.QueryRowContext(tx.ctx, selectClientAdmit, client, start, key, offset)
	}
	var at int64
	err := row.Scan(&at)
	return at, err
}

type SendOutcome uint8

const (
	SendAcked SendOutcome = iota + 1
	SendRejected
	SendUnknown
)

type Settlement struct {
	Key     Key
	Outcome SendOutcome
	Now     time.Time
	AckedAt time.Time
	WACode  int
	Sent    Message
	Reply   *ReplyTarget
}

func (in Settlement) record() (state, result string, err error) {
	switch {
	case !in.Key.Valid():
		return "", "", invalid("idempotency key")
	case in.Now.IsZero():
		return "", "", invalid("time")
	case in.WACode < 0:
		return "", "", invalid("server code")
	}
	var r sendResult
	switch in.Outcome {
	case SendAcked:
		if in.AckedAt.IsZero() {
			return "", "", invalid("ack time")
		}
		state, r = stateDone, sendResult{Status: statusSent, TS: ms(in.AckedAt)}
	case SendRejected:
		state, r = stateFailed, sendResult{Status: statusRejected, Code: CodeRejected, WA: in.WACode}
	case SendUnknown:
		state, r = stateFailed, sendResult{Status: statusUnknown, Code: CodeUnknown}
	default:
		return "", "", invalid("outcome")
	}
	result, err = encodeResult(r)
	return state, result, err
}

func (s *Store) SettleSend(g policy.WriteGrant, ctx context.Context, in Settlement) (SentRow, error) {
	if !g.Valid() {
		return SentRow{}, ErrNotAllowed
	}
	state, result, err := in.record()
	if err != nil {
		return SentRow{}, err
	}
	var row SentRow
	err = s.Write(ctx, "ingest.send_settle", func(tx *Tx) error {
		var msgID string
		switch err := tx.q.QueryRowContext(tx.ctx, settleSend, g.Client(), in.Key.s, state, ms(in.Now), result).Scan(&msgID); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotPending
		case err != nil:
			return err
		}
		if in.Outcome != SendAcked {
			return nil
		}
		if in.Sent.ID != msgID {
			return invalid("message id")
		}
		var err error
		row, err = tx.RecordSent(g, in.Sent, in.Reply)
		return err
	})
	if err != nil {
		return SentRow{}, err
	}
	return row, nil
}

func (s *Store) ReleaseSend(g policy.WriteGrant, ctx context.Context, key Key) error {
	switch {
	case !g.Valid():
		return ErrNotAllowed
	case !key.Valid():
		return invalid("idempotency key")
	}
	return s.Write(ctx, "ingest.send_release", func(tx *Tx) error {
		res, err := tx.q.ExecContext(tx.ctx, releaseSend, g.Client(), key.s)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			err = ErrNotPending
		}
		return err
	})
}

func (s *Store) RecoverSends(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, invalid("time")
	}
	result, err := encodeResult(sendResult{Status: statusUnknown, Code: CodeUnknown})
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.Write(ctx, "ingest.send_recover", func(tx *Tx) error {
		res, err := tx.q.ExecContext(tx.ctx, recoverSends, ms(now), result)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

func (tx *Tx) SweepSends(now time.Time) (int, error) {
	if now.IsZero() {
		return 0, invalid("time")
	}
	res, err := tx.q.ExecContext(tx.ctx, sweepSends, ms(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (tx *Tx) RecordSent(g policy.WriteGrant, m Message, reply *ReplyTarget) (SentRow, error) {
	switch {
	case !g.Valid() || !m.Chat.Valid() || !g.Allows(m.Chat):
		return SentRow{}, ErrNotAllowed
	case !m.FromMe || m.Origin != OriginLive || m.Kind != KindText:
		return SentRow{}, invalid("a sent message is the account's own live text")
	case m.Quote != nil:
		return SentRow{}, invalid("quote")
	}
	if reply != nil {
		found, ok, err := tx.ResolveInChat(m.Chat, reply.ID, reply.Sender)
		if err != nil {
			return SentRow{}, err
		}
		if ok {
			m.Quote = &Quote{Ref: found.Ref, Verified: true}
		}
	}
	ref, inserted, err := tx.InsertMessage(m)
	if err != nil {
		return SentRow{}, err
	}
	sender, err := tx.Canonical(m.Sender)
	if err != nil {
		return SentRow{}, err
	}
	if _, err := tx.q.ExecContext(tx.ctx, markSentBy, g.Client(), ref.chat.JID(), m.ID, sender.JID()); err != nil {
		return SentRow{}, err
	}
	return SentRow{Chat: ref.chat, ID: m.ID, Sender: sender, Inserted: inserted}, nil
}
