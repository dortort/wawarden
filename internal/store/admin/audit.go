package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
	"github.com/dortort/wawarden/internal/token"
)

const (
	rowDomain   = "wawarden/audit-row/v1\x00"
	headBytes   = 16
	maxPeer     = 64
	verifyLimit = 30 * time.Minute

	ReasonIDGap        = "id_gap"
	ReasonHMACMismatch = "hmac_mismatch"
	ReasonUnknownKey   = "unknown_key"
)

var (
	auditCode       = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	errAuditUnkeyed = errors.New("admin: the audit chain has no key")
	errAuditEvent   = errors.New("admin: the audit event is not one the chain records")
	genesis         = make([]byte, sha256.Size)
)

type Audit struct {
	db    *db.DB
	keyID string
	chain *string
	chat  *string
	out   io.Writer
	mu    sync.Mutex
}

func NewAudit(d *db.DB, master *keys.Master, out io.Writer) *Audit {
	a := &Audit{db: d, out: out}
	if master != nil {
		chain, chat := string(master.AuditChainKey()), string(master.ChatHMACKey())
		a.keyID, a.chain, a.chat = master.ID(), &chain, &chat
	}
	return a
}

type Event struct {
	At     time.Time
	Client string
	Action string
	Chat   policy.CanonicalChat
	OK     bool
	Reason string
	Peer   string
}

type Line struct{ text []byte }

type auditLine struct {
	TS        string  `json:"ts"`
	Client    string  `json:"client"`
	Action    string  `json:"action"`
	ChatHMAC  *string `json:"chat_hmac"`
	OK        bool    `json:"ok"`
	Reason    string  `json:"reason"`
	ChainHead string  `json:"chain_head"`
}

type row struct {
	id       int64
	ts       int64
	client   string
	action   string
	chat     sql.NullString
	chatHMAC sql.NullString
	ok       bool
	reason   string
	peer     sql.NullString
	keyID    string
}

const (
	selectAuditTail = `SELECT id, row_hmac FROM audit ORDER BY id DESC LIMIT 1`
	insertAudit     = `INSERT INTO audit (id, ts, client_id, action, chat, chat_hmac, ok, reason, peer, key_id, row_hmac) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	selectAuditRows = `SELECT id, ts, client_id, action, chat, chat_hmac, ok, reason, peer, key_id, row_hmac FROM audit ORDER BY id`
)

func (a *Audit) Append(ctx context.Context, q db.Querier, e Event) (Line, error) {
	if a == nil || a.chain == nil {
		return Line{}, errAuditUnkeyed
	}
	if e.At.IsZero() || !token.WellFormedClientID(e.Client) || !auditCode.MatchString(e.Action) || !auditCode.MatchString(e.Reason) || len(e.Peer) > maxPeer {
		return Line{}, errAuditEvent
	}
	var last int64
	prev := genesis
	switch err := q.QueryRowContext(ctx, selectAuditTail).Scan(&last, &prev); {
	case errors.Is(err, sql.ErrNoRows):
		prev = genesis
	case err != nil:
		return Line{}, err
	}
	r := row{id: last + 1, ts: e.At.UnixMilli(), client: e.Client, action: e.Action, ok: e.OK, reason: e.Reason, keyID: a.keyID}
	var chatHMAC *string
	if e.Chat.Valid() {
		h := a.ChatHMAC(e.Chat)
		r.chat, r.chatHMAC, chatHMAC = sql.NullString{String: e.Chat.JID(), Valid: true}, sql.NullString{String: h, Valid: true}, &h
	}
	if e.Peer != "" {
		r.peer = sql.NullString{String: e.Peer, Valid: true}
	}
	mac := rowMAC([]byte(*a.chain), r, prev)
	if _, err := q.ExecContext(ctx, insertAudit, r.id, r.ts, r.client, r.action, r.chat, r.chatHMAC, boolInt(r.ok), r.reason, r.peer, r.keyID, mac); err != nil {
		return Line{}, err
	}
	text, err := json.Marshal(auditLine{
		TS: e.At.UTC().Format("2006-01-02T15:04:05.000Z07:00"), Client: r.client, Action: r.action, ChatHMAC: chatHMAC,
		OK: r.ok, Reason: r.reason, ChainHead: hex.EncodeToString(mac[:headBytes]),
	})
	if err != nil {
		return Line{}, err
	}
	return Line{text: append(text, '\n')}, nil
}

func (a *Audit) Ship(l Line) {
	if a == nil || a.out == nil || len(l.text) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.out.Write(l.text)
}

func (a *Audit) Record(ctx context.Context, e Event) error {
	if a == nil || a.db == nil {
		return errAuditUnkeyed
	}
	var line Line
	if err := a.db.Write(ctx, "admin.audit", func(ctx context.Context, q db.Querier) error {
		var err error
		line, err = a.Append(ctx, q, e)
		return err
	}); err != nil {
		return err
	}
	a.Ship(line)
	return nil
}

func (a *Audit) ChatHMAC(chat policy.CanonicalChat) string {
	m := hmac.New(sha256.New, []byte(*a.chat))
	m.Write([]byte(chat.JID()))
	return hex.EncodeToString(m.Sum(nil)[:headBytes])
}

func rowMAC(key []byte, r row, prev []byte) []byte {
	b := []byte(rowDomain)
	b, _ = binary.Append(b, binary.BigEndian, [2]int64{r.id, r.ts})
	b = field(b, true, r.client)
	b = field(b, true, r.action)
	b = field(b, r.chat.Valid, r.chat.String)
	b = field(b, r.chatHMAC.Valid, r.chatHMAC.String)
	b = field(b, r.ok, "")
	b = field(b, true, r.reason)
	b = field(b, r.peer.Valid, r.peer.String)
	b = field(b, true, r.keyID)
	b = append(b, prev...)
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return m.Sum(nil)
}

func field(dst []byte, present bool, s string) []byte {
	if !present {
		return append(dst, 0)
	}
	dst = append(dst, 1)
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

type Report struct {
	Rows         int64
	Verified     int64
	Bad          int64
	FirstBad     int64
	Problem      string
	Head         string
	Heads        int
	HeadsMissing int
	FirstMissing string
}

func (r Report) OK() bool { return r.Bad == 0 && r.HeadsMissing == 0 }

func Verify(ctx context.Context, path string, master *keys.Master, heads []string) (Report, error) {
	if master == nil {
		return Report{}, errAuditUnkeyed
	}
	d, err := db.OpenCopy(ctx, path, verifyLimit)
	if err != nil {
		return Report{}, err
	}
	shipped := make(map[string]bool, len(heads))
	for _, h := range heads {
		shipped[h] = false
	}
	key := master.AuditChainKey()
	var rep Report
	err = d.Read(ctx, "admin.audit_verify", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectAuditRows)
		if err != nil {
			return err
		}
		prev, prevID := genesis, int64(0)
		for rows.Next() {
			var r row
			var stored []byte
			var ok int64
			if err := rows.Scan(&r.id, &r.ts, &r.client, &r.action, &r.chat, &r.chatHMAC, &ok, &r.reason, &r.peer, &r.keyID, &stored); err != nil {
				return errors.Join(err, rows.Close())
			}
			r.ok = ok == 1
			rep.Rows++
			var problem string
			switch {
			case r.id != prevID+1:
				problem = ReasonIDGap
			case r.keyID != master.ID():
				problem = ReasonUnknownKey
			case !hmac.Equal(rowMAC(key, r, prev), stored):
				problem = ReasonHMACMismatch
			default:
				rep.Verified++
			}
			if problem != "" {
				rep.Bad++
				if rep.FirstBad == 0 {
					rep.FirstBad, rep.Problem = r.id, problem
				}
			}
			if len(stored) >= headBytes {
				head := hex.EncodeToString(stored[:headBytes])
				if _, ok := shipped[head]; ok {
					shipped[head] = true
				}
				rep.Head = head
			}
			prev, prevID = stored, r.id
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Report{}, fmt.Errorf("admin: read the audit chain: %w", err)
	}
	rep.Heads = len(shipped)
	for _, h := range heads {
		if !shipped[h] {
			rep.HeadsMissing++
			if rep.FirstMissing == "" {
				rep.FirstMissing = h
			}
			shipped[h] = true
		}
	}
	return rep, nil
}
