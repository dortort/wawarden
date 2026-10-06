package scoped

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

const MaxGrantChats = 1000

var (
	ErrGrantTooLarge = errors.New("scoped: the read grant names more than 1,000 chats, more than one query binds")
	errUnscoped      = errors.New("scoped: the query carries no scope clause")
	scopeMarker      = regexp.MustCompile(`\{scope ([a-z]+\.[a-z_]+)\}`)
)

type scope struct {
	all  bool
	jids []string
}

func scopeOf(g policy.ReadGrant) (scope, error) {
	switch {
	case !g.Valid():
		return scope{}, nil
	case g.All():
		return scope{all: true}, nil
	}
	chats := g.Chats()
	jids := make([]string, 0, len(chats))
	for c := range chats {
		if c.Valid() {
			jids = append(jids, c.JID())
		}
	}
	slices.Sort(jids)
	jids = slices.Compact(jids)
	if len(jids) > MaxGrantChats {
		return scope{}, ErrGrantTooLarge
	}
	return scope{jids: jids}, nil
}

func (s scope) expand(query string, bound int) (string, []any, error) {
	if !scopeMarker.MatchString(query) {
		return "", nil, errUnscoped
	}
	switch {
	case s.all:
		return scopeMarker.ReplaceAllLiteralString(query, " AND 1"), nil, nil
	case len(s.jids) == 0:
		return scopeMarker.ReplaceAllLiteralString(query, " AND 0"), nil, nil
	}
	var list strings.Builder
	args := make([]any, len(s.jids))
	for i, jid := range s.jids {
		if i > 0 {
			list.WriteByte(',')
		}
		list.WriteByte('?')
		list.WriteString(strconv.Itoa(bound + 1 + i))
		args[i] = jid
	}
	return scopeMarker.ReplaceAllString(query, " AND ${1} IN ("+list.String()+")"), args, nil
}

type querier struct {
	raw   db.Querier
	scope scope
	trace func(string)
}

func (q querier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	text, bound, err := q.scope.expand(query, len(args))
	if err != nil {
		return nil, err
	}
	if q.trace != nil {
		q.trace(text)
	}
	return q.raw.QueryContext(ctx, text, slices.Concat(args, bound)...)
}

func (q querier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if q.trace != nil {
		q.trace(query)
	}
	return q.raw.QueryRowContext(ctx, query, args...)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type rowsOf[T any] struct {
	q    querier
	scan func(rowScanner) (T, error)
}

func (r rowsOf[T]) QueryContext(ctx context.Context, query string, args ...any) ([]T, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []T
	for rows.Next() {
		v, err := r.scan(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, v)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
