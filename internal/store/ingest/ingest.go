// Package ingest owns archive.db: its schema, and the write side through which the engine records WhatsApp traffic with every identifier a canonical chat.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

type Profile = db.Profile

const (
	ProfileLocal = db.Local
	ProfileNFS   = db.NFS
)

type Refusal = db.Refusal

type Options struct {
	DataDir      string
	UID          int
	Profile      Profile
	MinFreeBytes uint64
	Logger       *slog.Logger
}

type Store struct {
	db      *db.DB
	version int
	floor   uint64

	mu     sync.Mutex
	paused bool
}

func Open(ctx context.Context, opts Options) (*Store, error) {
	d, err := db.Open(ctx, db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger})
	if err != nil {
		return nil, err
	}
	version, err := d.Migrate(ctx, migrations)
	if err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return &Store{db: d, version: version, floor: opts.MinFreeBytes}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Healthy() bool { return s.db.Healthy() }

func (s *Store) SchemaVersion() int { return s.version }

func (s *Store) Profile() Profile { return s.db.Profile() }

func (s *Store) OFDLocking() bool { return s.db.OFDLocking() }

func (s *Store) Backup(ctx context.Context, staging string, w io.Writer) error {
	return s.db.Backup(ctx, staging, w)
}

type Reader struct {
	ctx context.Context
	q   db.Querier
}

type Tx struct{ Reader }

func (s *Store) Read(ctx context.Context, op string, fn func(*Reader) error) error {
	return s.db.Read(ctx, op, func(ctx context.Context, q db.Querier) error {
		return fn(&Reader{ctx: ctx, q: q})
	})
}

func (s *Store) Write(ctx context.Context, op string, fn func(*Tx) error) error {
	return s.db.Write(ctx, op, func(ctx context.Context, q db.Querier) error {
		return fn(&Tx{Reader{ctx: ctx, q: q}})
	})
}

var (
	ErrInvalid     = errors.New("ingest: invalid input")
	ErrNotFound    = errors.New("ingest: no such row")
	ErrWrongChat   = errors.New("ingest: the reference belongs to another chat")
	ErrRevoked     = errors.New("ingest: the message is revoked")
	errCorruptChat = errors.New("ingest: a stored identifier is not canonical")
)

func invalid(what string) error { return fmt.Errorf("%w: %s", ErrInvalid, what) }

var (
	messageID = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	mediaType = regexp.MustCompile(`^[a-z0-9][a-z0-9.+/-]{0,63}$`)
)

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func formatMS(t time.Time) string { return strconv.FormatInt(ms(t), 10) }

func user(c policy.CanonicalChat) bool {
	return c.Kind() == policy.PhoneChat || c.Kind() == policy.LIDChat
}

func stored(jid string, kind policy.ChatKind) (policy.CanonicalChat, error) {
	c, ok := policy.Normalize(jid)
	if !ok || c.Kind() != kind || c.JID() != jid {
		return policy.CanonicalChat{}, errCorruptChat
	}
	return c, nil
}

func kindCode(k policy.ChatKind) int64 {
	switch k {
	case policy.PhoneChat:
		return 1
	case policy.LIDChat:
		return 2
	case policy.GroupChat:
		return 3
	}
	return 0
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
