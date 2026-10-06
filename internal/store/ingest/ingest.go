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

	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/internal/db"
	"github.com/dortort/wawarden/internal/store/scoped"
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

	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	RewriteTimeout time.Duration
	ReadSlots      int

	Master   *keys.Master
	AuditOut io.Writer
	Now      func() time.Time
}

type Store struct {
	db      *db.DB
	version int
	floor   uint64
	admin   *admin.Reader
	scoped  *scoped.Reader
	audit   *admin.Audit
	clients *admin.Clients

	mu     sync.Mutex
	paused bool
}

func Open(ctx context.Context, opts Options) (*Store, error) {
	d, err := db.Open(ctx, db.Archive, db.Options{DataDir: opts.DataDir, UID: opts.UID, Profile: opts.Profile, Logger: opts.Logger, ReadTimeout: opts.ReadTimeout, WriteTimeout: opts.WriteTimeout, RewriteTimeout: opts.RewriteTimeout})
	if err != nil {
		return nil, err
	}
	version, err := migrate(ctx, d)
	if err == nil {
		err = backfillChanges(ctx, d)
	}
	if err != nil {
		return nil, errors.Join(err, d.Close())
	}
	var slots []scoped.Option
	if opts.ReadSlots > 0 {
		slots = append(slots, scoped.WithReadSlots(opts.ReadSlots))
	}
	audit := admin.NewAudit(d, opts.Master, opts.AuditOut)
	s := &Store{db: d, version: version, floor: opts.MinFreeBytes, admin: admin.New(d), scoped: scoped.New(d, slots...),
		audit: audit, clients: admin.NewClients(d, &admin.Generation{}, audit, opts.Now)}
	if err := s.finishRewrite(ctx); err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Healthy() bool { return s.db.Healthy() }

func (s *Store) SchemaVersion() int { return s.version }

func (s *Store) Profile() Profile { return s.db.Profile() }

func (s *Store) OFDLocking() bool { return s.db.OFDLocking() }

func (s *Store) Admin() *admin.Reader { return s.admin }

func (s *Store) Scoped() *scoped.Reader { return s.scoped }

func (s *Store) Audit() *admin.Audit { return s.audit }

func (s *Store) Clients() *admin.Clients { return s.clients }

func (s *Store) Backup(ctx context.Context, staging string, write func(name string, size int64, r io.Reader) error) error {
	return s.db.Backup(ctx, staging, write)
}

type Reader struct {
	ctx context.Context
	q   db.Querier
}

type Tx struct {
	Reader
	forgotten bool
	rescoped  bool
}

func (s *Store) Read(ctx context.Context, op string, fn func(*Reader) error) error {
	return s.db.Read(ctx, op, func(ctx context.Context, q db.Querier) error {
		return fn(&Reader{ctx: ctx, q: q})
	})
}

func (s *Store) Write(ctx context.Context, op string, fn func(*Tx) error) error {
	var stale bool
	if err := s.db.Write(ctx, op, func(ctx context.Context, q db.Querier) error {
		tx := &Tx{Reader: Reader{ctx: ctx, q: q}}
		if err := fn(tx); err != nil {
			return err
		}
		var err error
		if stale, err = tx.markStaleKeys(); err == nil && tx.rescoped {
			s.clients.Invalidate()
		}
		return err
	}); err != nil || !stale {
		return err
	}
	if err := s.rewriteIndex(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrRewritePending, err)
	}
	return nil
}

var (
	ErrInvalid        = errors.New("ingest: invalid input")
	ErrNotFound       = errors.New("ingest: no such row")
	ErrWrongChat      = errors.New("ingest: the reference belongs to another chat")
	ErrRevoked        = errors.New("ingest: the message is revoked")
	ErrRewritePending = errors.New("ingest: the write committed, but the full-text index rewrite it requires failed and stays due")
	errCorruptChat    = errors.New("ingest: a stored identifier is not canonical")
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
