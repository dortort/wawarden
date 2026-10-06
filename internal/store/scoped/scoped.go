// Package scoped is the archive's read side: every method takes a read grant first, resolves scope in the query itself, and returns rows that never marshal to JSON.
package scoped

import (
	"context"
	"errors"
	"fmt"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

const (
	DefaultReadSlots = 8
	MaxLimit         = 200
	window           = 20000
	maxWindows       = 5
)

var (
	ErrBusy    = errors.New("scoped: every read slot is taken or the read ran out of time")
	ErrInvalid = errors.New("scoped: invalid read")
)

type Reader struct {
	db    *db.DB
	slots chan struct{}
	trace func(string)
}

type Option func(*Reader)

func WithReadSlots(n int) Option {
	return func(r *Reader) { r.slots = make(chan struct{}, max(n, 1)) }
}

func New(d *db.DB, opts ...Option) *Reader {
	r := &Reader{db: d, slots: make(chan struct{}, DefaultReadSlots)}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *Reader) read(g policy.ReadGrant, ctx context.Context, op string, fn func(context.Context, querier) error) error {
	s, err := scopeOf(g)
	if err != nil {
		return err
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return ErrBusy
	}
	err = r.db.Read(ctx, op, func(ctx context.Context, q db.Querier) error {
		return fn(ctx, querier{raw: q, scope: s, trace: r.trace})
	})
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

func validLimit(limit int) bool { return limit >= 1 && limit <= MaxLimit }
