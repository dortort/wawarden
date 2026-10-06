package scoped

import (
	"context"
	"errors"
	"regexp"
	"strconv"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/internal/db"
)

var Queries = map[string]string{
	"selectChat":        selectChat,
	"selectDated":       selectDated,
	"selectUndated":     selectUndated,
	"selectOlder":       selectOlder,
	"selectNewer":       selectNewer,
	"selectMessage":     selectMessage,
	"selectChanges":     selectChanges,
	"selectChatChanges": selectChatChanges,
	"selectTopChange":   selectTopChange,
	"selectLID":         selectLID,
	"selectSearch":      selectSearch,
	"selectChatSearch":  selectChatSearch,
	"selectTopSeq":      selectTopSeq,
}

const Window = window

var parameter = regexp.MustCompile(`\?(\d+)`)

func Expand(g policy.ReadGrant, query string, bound int) (string, []any, error) {
	s, err := scopeOf(g)
	if err != nil {
		return "", nil, err
	}
	return s.expand(query, bound)
}

func (r *Reader) Traced(trace func(string)) *Reader {
	c := *r
	c.trace = trace
	return &c
}

func (r *Reader) Plan(g policy.ReadGrant, ctx context.Context, query string) ([]string, error) {
	var lines []string
	err := r.read(g, ctx, "test.plan", func(ctx context.Context, q querier) error {
		text, args := query, []any(nil)
		bound := 0
		for _, m := range parameter.FindAllStringSubmatch(query, -1) {
			n, _ := strconv.Atoi(m[1])
			bound = max(bound, n)
		}
		if scopeMarker.MatchString(query) {
			var err error
			if text, args, err = q.scope.expand(query, bound); err != nil {
				return err
			}
		}
		rows, err := q.raw.QueryContext(ctx, "EXPLAIN QUERY PLAN "+text, append(make([]any, bound), args...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return errors.Join(err, rows.Close())
			}
			lines = append(lines, detail)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return lines, err
}

func (r *Reader) Probe(g policy.ReadGrant, ctx context.Context, query string) (int, error) {
	n := 0
	err := r.read(g, ctx, "test.probe", func(ctx context.Context, q querier) error {
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		for rows.Next() {
			n++
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return n, err
}

func (r *Reader) Hold(g policy.ReadGrant, ctx context.Context, held chan<- struct{}, release <-chan struct{}) error {
	return r.read(g, ctx, "test.hold", func(context.Context, querier) error {
		held <- struct{}{}
		<-release
		return nil
	})
}

func (r *Reader) Stall(g policy.ReadGrant, ctx context.Context) error {
	return r.read(g, ctx, "test.stall", func(ctx context.Context, _ querier) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

func (r *Reader) Exec(ctx context.Context, stmts ...string) error {
	return r.db.Write(ctx, "test.exec", func(ctx context.Context, q db.Querier) error {
		for _, stmt := range stmts {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Reader) Column(ctx context.Context, query string, args ...any) ([]string, error) {
	var out []string
	err := r.db.Read(ctx, "test.column", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return errors.Join(err, rows.Close())
			}
			out = append(out, v)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return out, err
}

type DumpChat struct {
	Row    int64
	JID    string
	Ref    string
	LastTS int64
	Dated  bool
}

type DumpMessage struct {
	Seq, TS, ChangeSeq int64
	Chat, ID, Sender   string
	Text               string
	Revoked            bool
}

func (r *Reader) Dump(ctx context.Context) ([]DumpChat, []DumpMessage, error) {
	var chats []DumpChat
	var messages []DumpMessage
	err := r.db.Read(ctx, "test.dump", func(ctx context.Context, q db.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT rowid, jid, ref, coalesce(last_ts, 0), last_ts IS NOT NULL FROM chats")
		if err != nil {
			return err
		}
		for rows.Next() {
			var c DumpChat
			if err := rows.Scan(&c.Row, &c.JID, &c.Ref, &c.LastTS, &c.Dated); err != nil {
				return errors.Join(err, rows.Close())
			}
			chats = append(chats, c)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if rows, err = q.QueryContext(ctx, "SELECT seq, ts, change_seq, chat_jid, id, sender_jid, coalesce(text, ''), revoked FROM messages"); err != nil {
			return err
		}
		for rows.Next() {
			var m DumpMessage
			if err := rows.Scan(&m.Seq, &m.TS, &m.ChangeSeq, &m.Chat, &m.ID, &m.Sender, &m.Text, &m.Revoked); err != nil {
				return errors.Join(err, rows.Close())
			}
			messages = append(messages, m)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return chats, messages, err
}
