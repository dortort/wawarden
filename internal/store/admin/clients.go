package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/safego"
	"github.com/dortort/wawarden/internal/store/internal/db"
	"github.com/dortort/wawarden/internal/token"
)

const (
	StateActive  = "active"
	StateExpired = "expired"
	StateRevoked = "revoked"

	actionClientCreate = "client_create"
	actionClientRevoke = "client_revoke"
	reasonOK           = "ok"
	idAttempts         = 4
)

var (
	ErrNotFound    = errors.New("admin: no such client")
	ErrUnavailable = errors.New("admin: the client list is being reloaded")
	errCorruptRow  = errors.New("admin: a stored client row is not one the service writes")
	errIDExhausted = errors.New("admin: no unused client id was drawn")
)

type ClientChat struct {
	Chat  policy.CanonicalChat
	Known bool
	Name  string
}

type Client struct {
	ID                string
	Name              string
	State             string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RevokedAt         time.Time
	AllChats          bool
	AllowFirstContact bool
	ReadCount         int64
	WriteCount        int64
	Read              []ClientChat
	Write             []ClientChat
}

type ClientCounts struct {
	Active         int64
	Expired        int64
	Revoked        int64
	AllChatsActive int64
}

type Clients struct {
	db    *db.DB
	now   func() time.Time
	audit *Audit
	wait  time.Duration

	dummy  token.Digest
	digest func(string) token.Digest
	match  func(stored, presented token.Digest) bool

	mu      sync.Mutex
	gen     uint64
	snap    *snapshot
	loading bool
	changed chan struct{}
}

type snapshot struct {
	gen     uint64
	clients map[string]credential
}

type credential struct {
	digest token.Digest
	client *policy.Client
}

func NewClients(d *db.DB, audit *Audit, now func() time.Time) *Clients {
	if now == nil {
		now = time.Now
	}
	return &Clients{db: d, now: now, audit: audit, wait: d.ReadTimeout(), dummy: token.RandomDigest(), digest: token.DigestOf, match: token.Match, changed: make(chan struct{})}
}

func (c *Clients) Load(ctx context.Context) error {
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()
	clients, err := c.load(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishLocked(&snapshot{gen: gen, clients: clients})
	return nil
}

func (c *Clients) Stage(ctx context.Context, q db.Querier) func(committed bool) {
	c.mu.Lock()
	c.gen++
	gen := c.gen
	c.mu.Unlock()
	var next *snapshot
	if clients, err := loadFrom(ctx, q); err == nil {
		next = &snapshot{gen: gen, clients: clients}
	}
	return func(committed bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if committed && next != nil {
			c.publishLocked(next)
			return
		}
		c.reloadLocked()
	}
}

func (c *Clients) Authenticate(ctx context.Context, presented string) (*policy.Client, bool, error) {
	snap, unavailable := c.current(ctx)
	id, wellFormed := token.ParseClient(presented)
	var cred credential
	found := false
	if snap != nil {
		cred, found = snap.clients[id]
	}
	stored := c.dummy
	if found {
		stored = cred.digest
	}
	matched := c.match(stored, c.digest(presented))
	if unavailable != nil {
		return nil, false, unavailable
	}
	if !wellFormed || !found || !matched {
		return nil, false, nil
	}
	if cred.client.Revoked || !c.now().Before(cred.client.ExpiresAt) {
		return nil, false, nil
	}
	return cred.client, true, nil
}

func (c *Clients) current(ctx context.Context) (*snapshot, error) {
	var timeout <-chan time.Time
	for {
		c.mu.Lock()
		if c.snap != nil && c.snap.gen == c.gen {
			s := c.snap
			c.mu.Unlock()
			return s, nil
		}
		c.reloadLocked()
		changed := c.changed
		c.mu.Unlock()
		if timeout == nil {
			t := time.NewTimer(c.wait)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-changed:
		case <-timeout:
			return nil, ErrUnavailable
		case <-ctx.Done():
			return nil, ErrUnavailable
		}
	}
}

func (c *Clients) publishLocked(s *snapshot) {
	if c.snap != nil && s.gen <= c.snap.gen {
		return
	}
	c.snap = s
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Clients) reloadLocked() {
	if c.loading {
		return
	}
	c.loading = true
	gen := c.gen
	safego.Go("admin.clients_reload", func() {
		clients, err := c.load(context.Background())
		c.mu.Lock()
		defer c.mu.Unlock()
		c.loading = false
		if err == nil {
			c.publishLocked(&snapshot{gen: gen, clients: clients})
		}
	})
}

const (
	selectAuthRows  = `SELECT id, name, token_hash, read_all, allow_first_contact, expires_at, revoked_at FROM clients`
	selectReadSets  = `SELECT client_id, chat_jid FROM client_read_chats`
	selectWriteSets = `SELECT client_id, chat_jid FROM client_write_chats`
)

func (c *Clients) load(ctx context.Context) (map[string]credential, error) {
	var out map[string]credential
	err := c.db.Read(ctx, "admin.clients_load", func(ctx context.Context, q db.Querier) error {
		var err error
		out, err = loadFrom(ctx, q)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("admin: load the clients: %w", err)
	}
	return out, nil
}

func loadFrom(ctx context.Context, q db.Querier) (map[string]credential, error) {
	out := map[string]credential{}
	rows, err := q.QueryContext(ctx, selectAuthRows)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cl policy.Client
		var hash []byte
		var expires int64
		var revoked sql.NullInt64
		if err := rows.Scan(&cl.ID, &cl.Name, &hash, &cl.ReadAll, &cl.AllowFirstContact, &expires, &revoked); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		digest, ok := token.ParseDigest(hash)
		if !ok || !token.WellFormedClientID(cl.ID) {
			return nil, errors.Join(errCorruptRow, rows.Close())
		}
		cl.ExpiresAt, cl.Revoked = fromMS(expires), revoked.Valid
		cl.Read, cl.Write = map[policy.CanonicalChat]struct{}{}, map[policy.CanonicalChat]struct{}{}
		out[cl.ID] = credential{digest: digest, client: &cl}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	add := func(pick func(*policy.Client) map[policy.CanonicalChat]struct{}) func(string, policy.CanonicalChat) error {
		return func(id string, chat policy.CanonicalChat) error {
			cred, ok := out[id]
			if !ok {
				return errCorruptRow
			}
			pick(cred.client)[chat] = struct{}{}
			return nil
		}
	}
	rows, err = q.QueryContext(ctx, selectReadSets)
	if err := scanSets(rows, err, add(func(cl *policy.Client) map[policy.CanonicalChat]struct{} { return cl.Read })); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, selectWriteSets)
	if err := scanSets(rows, err, add(func(cl *policy.Client) map[policy.CanonicalChat]struct{} { return cl.Write })); err != nil {
		return nil, err
	}
	return out, nil
}

func scanSets(rows *sql.Rows, err error, add func(string, policy.CanonicalChat) error) error {
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, jid string
		if err := rows.Scan(&id, &jid); err != nil {
			return errors.Join(err, rows.Close())
		}
		chat, ok := storedChat(jid)
		if !ok {
			return errors.Join(errCorruptRow, rows.Close())
		}
		if err := add(id, chat); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

func storedChat(jid string) (policy.CanonicalChat, bool) {
	chat, ok := policy.Normalize(jid)
	return chat, ok && chat.JID() == jid
}

const (
	selectNameTaken = `SELECT EXISTS (SELECT 1 FROM clients WHERE name = ?)`
	selectIDTaken   = `SELECT EXISTS (SELECT 1 FROM clients WHERE id = ?)`
	selectLID       = `SELECT lid FROM lid_map WHERE pn = ?`
	selectKnown     = `SELECT EXISTS (SELECT 1 FROM chats WHERE jid = ?)`
	insertClient    = `INSERT INTO clients (id, name, token_hash, read_all, allow_first_contact, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
	insertReadChat  = `INSERT INTO client_read_chats (client_id, chat_jid) VALUES (?, ?)`
	insertWriteChat = `INSERT INTO client_write_chats (client_id, chat_jid) VALUES (?, ?)`
	revokeClient    = `UPDATE clients SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`
)

func (c *Clients) Create(ctx context.Context, spec policy.ClientSpec) (Client, string, error) {
	now := c.now()
	want, err := policy.ValidateClientSpec(spec, now)
	if err != nil {
		return Client{}, "", err
	}
	var view Client
	var full string
	var line Line
	var settle func(bool)
	err = c.db.Write(ctx, "admin.client_create", func(ctx context.Context, q db.Querier) error {
		var taken bool
		if err := q.QueryRowContext(ctx, selectNameTaken, want.Name).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return policy.ErrNameTaken
		}
		read, err := canonicalSet(ctx, q, want.Read)
		if err != nil {
			return err
		}
		write, err := canonicalSet(ctx, q, want.Write)
		if err != nil {
			return err
		}
		for chat := range write {
			var known bool
			if err := q.QueryRowContext(ctx, selectKnown, chat.JID()).Scan(&known); err != nil {
				return err
			}
			if !policy.WritableChat(chat, known, want.AllowFirstContact) {
				return policy.ErrWriteChatUnknown
			}
		}
		var id string
		if id, full, err = freshToken(ctx, q); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, insertClient, id, want.Name, token.StoredDigest(full), boolInt(want.ReadAll), boolInt(want.AllowFirstContact), now.UnixMilli(), want.ExpiresAt.UnixMilli()); err != nil {
			return err
		}
		for chat := range read {
			if _, err := q.ExecContext(ctx, insertReadChat, id, chat.JID()); err != nil {
				return err
			}
		}
		for chat := range write {
			if _, err := q.ExecContext(ctx, insertWriteChat, id, chat.JID()); err != nil {
				return err
			}
		}
		if line, err = c.audit.Append(ctx, q, Event{At: now, Client: id, Action: actionClientCreate, OK: true, Reason: reasonOK}); err != nil {
			return err
		}
		if view, err = clientView(ctx, q, id, now); err != nil {
			return err
		}
		settle = c.Stage(ctx, q)
		return nil
	})
	if settle != nil {
		settle(err == nil)
	}
	if err != nil {
		return Client{}, "", err
	}
	c.audit.Ship(line)
	return view, full, nil
}

func freshToken(ctx context.Context, q db.Querier) (id, full string, err error) {
	for range idAttempts {
		id, full = token.NewClient()
		var taken bool
		if err := q.QueryRowContext(ctx, selectIDTaken, id).Scan(&taken); err != nil {
			return "", "", err
		}
		if !taken {
			return id, full, nil
		}
	}
	return "", "", errIDExhausted
}

func canonicalSet(ctx context.Context, q db.Querier, chats map[policy.CanonicalChat]struct{}) (map[policy.CanonicalChat]struct{}, error) {
	out := make(map[policy.CanonicalChat]struct{}, len(chats))
	for chat := range chats {
		if chat.Kind() == policy.PhoneChat {
			var lid string
			switch err := q.QueryRowContext(ctx, selectLID, chat.JID()).Scan(&lid); {
			case err == nil:
				mapped, ok := storedChat(lid)
				if !ok || mapped.Kind() != policy.LIDChat {
					return nil, errCorruptRow
				}
				chat = mapped
			case !errors.Is(err, sql.ErrNoRows):
				return nil, err
			}
		}
		out[chat] = struct{}{}
	}
	return out, nil
}

func (c *Clients) Revoke(ctx context.Context, id string) (Client, error) {
	if !token.WellFormedClientID(id) {
		return Client{}, ErrNotFound
	}
	now := c.now()
	var view Client
	var line Line
	var changed bool
	var settle func(bool)
	err := c.db.Write(ctx, "admin.client_revoke", func(ctx context.Context, q db.Querier) error {
		res, err := q.ExecContext(ctx, revokeClient, now.UnixMilli(), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if view, err = clientView(ctx, q, id, now); err != nil {
			return err
		}
		if changed = n > 0; changed {
			if line, err = c.audit.Append(ctx, q, Event{At: now, Client: id, Action: actionClientRevoke, OK: true, Reason: reasonOK}); err != nil {
				return err
			}
			settle = c.Stage(ctx, q)
		}
		return nil
	})
	if settle != nil {
		settle(err == nil)
	}
	if err != nil {
		return Client{}, err
	}
	if changed {
		c.audit.Ship(line)
	}
	return view, nil
}

func (c *Clients) Get(ctx context.Context, id string) (Client, error) {
	if !token.WellFormedClientID(id) {
		return Client{}, ErrNotFound
	}
	now := c.now()
	var view Client
	err := c.db.Read(ctx, "admin.client_get", func(ctx context.Context, q db.Querier) error {
		var err error
		view, err = clientView(ctx, q, id, now)
		return err
	})
	return view, err
}

const (
	selectClients = `SELECT id, name, read_all, allow_first_contact, created_at, expires_at, revoked_at,
		(SELECT count(*) FROM client_read_chats r WHERE r.client_id = c.id),
		(SELECT count(*) FROM client_write_chats w WHERE w.client_id = c.id)
		FROM clients c ORDER BY created_at, rowid`
	selectClient = `SELECT id, name, read_all, allow_first_contact, created_at, expires_at, revoked_at,
		(SELECT count(*) FROM client_read_chats r WHERE r.client_id = c.id),
		(SELECT count(*) FROM client_write_chats w WHERE w.client_id = c.id)
		FROM clients c WHERE id = ?`
	selectReadChats  = `SELECT s.chat_jid, c.jid IS NOT NULL, coalesce(c.name, '') FROM client_read_chats s LEFT JOIN chats c ON c.jid = s.chat_jid WHERE s.client_id = ? ORDER BY s.chat_jid`
	selectWriteChats = `SELECT s.chat_jid, c.jid IS NOT NULL, coalesce(c.name, '') FROM client_write_chats s LEFT JOIN chats c ON c.jid = s.chat_jid WHERE s.client_id = ? ORDER BY s.chat_jid`
	selectCounts     = `SELECT
		count(CASE WHEN revoked_at IS NULL AND expires_at > ?1 THEN 1 END),
		count(CASE WHEN revoked_at IS NULL AND expires_at <= ?1 THEN 1 END),
		count(CASE WHEN revoked_at IS NOT NULL THEN 1 END),
		count(CASE WHEN revoked_at IS NULL AND expires_at > ?1 AND read_all = 1 THEN 1 END)
		FROM clients`
)

func (c *Clients) List(ctx context.Context) ([]Client, error) {
	now := c.now()
	var out []Client
	err := c.db.Read(ctx, "admin.client_list", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, selectClients)
		if err != nil {
			return err
		}
		for rows.Next() {
			view, err := scanClient(rows, now)
			if err != nil {
				return errors.Join(err, rows.Close())
			}
			out = append(out, view)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Clients) Counts(ctx context.Context) (ClientCounts, error) {
	now := c.now()
	var n ClientCounts
	err := c.db.Read(ctx, "admin.client_counts", func(ctx context.Context, q db.Querier) error {
		return q.QueryRowContext(ctx, selectCounts, now.UnixMilli()).Scan(&n.Active, &n.Expired, &n.Revoked, &n.AllChatsActive)
	})
	if err != nil {
		return ClientCounts{}, fmt.Errorf("admin: count the clients: %w", err)
	}
	return n, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanClient(s scanner, now time.Time) (Client, error) {
	var v Client
	var created, expires int64
	var revoked sql.NullInt64
	if err := s.Scan(&v.ID, &v.Name, &v.AllChats, &v.AllowFirstContact, &created, &expires, &revoked, &v.ReadCount, &v.WriteCount); err != nil {
		return Client{}, err
	}
	v.CreatedAt, v.ExpiresAt = fromMS(created), fromMS(expires)
	switch {
	case revoked.Valid:
		v.State, v.RevokedAt = StateRevoked, fromMS(revoked.Int64)
	case !now.Before(v.ExpiresAt):
		v.State = StateExpired
	default:
		v.State = StateActive
	}
	return v, nil
}

func clientView(ctx context.Context, q db.Querier, id string, now time.Time) (Client, error) {
	v, err := scanClient(q.QueryRowContext(ctx, selectClient, id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	if v.Read, err = clientChats(q.QueryContext(ctx, selectReadChats, id)); err != nil {
		return Client{}, err
	}
	if v.Write, err = clientChats(q.QueryContext(ctx, selectWriteChats, id)); err != nil {
		return Client{}, err
	}
	return v, nil
}

func clientChats(rows *sql.Rows, err error) ([]ClientChat, error) {
	if err != nil {
		return nil, err
	}
	out := []ClientChat{}
	for rows.Next() {
		var jid string
		var cc ClientChat
		if err := rows.Scan(&jid, &cc.Known, &cc.Name); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var ok bool
		if cc.Chat, ok = storedChat(jid); !ok {
			return nil, errors.Join(errCorruptRow, rows.Close())
		}
		out = append(out, cc)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
