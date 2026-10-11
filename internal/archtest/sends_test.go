package archtest

import (
	"go/ast"
	"regexp"
)

const (
	ingestDir  = storeDir + "/ingest"
	sendsFile  = ingestDir + "/sends.go"
	auditPrune = storeDir + "/admin/prune.go"
)

var (
	sendHandles      = set("Store", "Tx", "Reader")
	grantFreeSends   = set("Store.RecoverSends", "Tx.SweepSends")
	sentByFiles      = set(ingestDir+"/schema.go", sendsFile, scopedDir+"/messages.go")
	sentByColumn     = regexp.MustCompile(`(?i)\bsent_by\b`)
	auditDeleteQuery = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:\w+\s*\.\s*)?["'\x60\[]?audit\b`)
)

var sendGrantRule = rule{
	name:  "send-grant-first",
	check: checkSendGrantFirst,
	cases: []snippet{
		{name: "store methods of the send file without a write grant first", rel: sendsFile, want: 7, src: `package ingest

import (
	"context"

	"github.com/dortort/wawarden/internal/policy"
)

type Store struct{}

type Tx struct{}

type Reader struct{}

func (s *Store) ReserveSend(ctx context.Context, g policy.WriteGrant) {}
func (tx *Tx) RecordSent(m string)                                    {}
func (s *Store) SettleSend(g *policy.WriteGrant)                      {}
func (s *Store) ReleaseSend(g policy.ReadGrant)                       {}
func (r *Reader) Peek()                                               {}
func (s Store) Count() int                                            { return 0 }
func (tx *Tx) RecoverSends()                                          {}
`},
		{name: "a grant type of another package", rel: sendsFile, want: 1, src: `package ingest

import policy "example.com/policy"

type Store struct{}

func (s *Store) ReserveSend(g policy.WriteGrant) {}
`},
		{name: "grants first, the two recovery methods, value types, unexported methods and functions", rel: sendsFile, src: `package ingest

import (
	"context"
	"time"

	p "github.com/dortort/wawarden/internal/policy"
)

type Store struct{}

type Tx struct{}

type RateReason uint8

func (s *Store) ReserveSend(g p.WriteGrant, ctx context.Context)      {}
func (tx *Tx) RecordSent(g, h p.WriteGrant)                           {}
func (s *Store) RecoverSends(ctx context.Context, now time.Time) int { return 0 }
func (tx *Tx) SweepSends(now time.Time) int                          { return 0 }
func (tx *Tx) admit(client string) error                             { return nil }
func (r RateReason) String() string                                  { return "" }
func Fingerprint(text string) [32]byte                               { return [32]byte{} }
`},
		{name: "a test of the send file's package", rel: ingestDir + "/sends_test.go", src: `package ingest

func (s *Store) ReserveSend() {}
`},
		{name: "another file of the ingest package", rel: ingestDir + "/messages.go", src: `package ingest

func (tx *Tx) InsertMessage(m Message) {}
`},
	},
}

func checkSendGrantFirst(f *sourceFile) []string {
	if f.test || f.rel != sendsFile {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() || !sendHandles[receiverName(fn)] || grantFreeSends[receiverName(fn)+"."+fn.Name.Name] {
			continue
		}
		params := fn.Type.Params.List
		if len(params) == 0 || isPointer(params[0].Type) || !f.isType(params[0].Type, policyPath, "WriteGrant") {
			out = append(out, f.at(fn, "%s.%s does not take a policy.WriteGrant as its first parameter: every send method of %s takes the writing client's grant first", receiverName(fn), fn.Name.Name, sendsFile))
		}
	}
	return out
}

var sentByRule = rule{
	name:  "sent-by-confinement",
	check: checkSentBy,
	cases: []snippet{
		{name: "the column named in SQL of another store file", rel: ingestDir + "/x.go", want: 4, src: `package ingest

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

const (
	mark   = "UPDATE messages SET sent_by = ?1"
	read   = "SELECT " + "SENT_BY FROM messages"
	prefix = "UPDATE messages SET sent"
	suffix = "_by = ?1"
)

func f(ctx context.Context, q querier) {
	_, _ = q.ExecContext(ctx, prefix+suffix)
	_, _ = q.ExecContext(ctx, "SELECT m.sent_by FROM messages m")
}
`},
		{name: "the column named outside the store", rel: "internal/api/x.go", want: 1, src: `package api

type message struct {
	Origin string ` + "`json:\"sent_by\"`" + `
}
`},
		{name: "the schema, the send file and the scoped message columns", rel: sendsFile, src: `package ingest

const markSentBy = "UPDATE messages SET sent_by = ?1 WHERE sent_by IS NULL"
`},
		{name: "the scoped message columns", rel: scopedDir + "/messages.go", src: `package scoped

const messageColumns = "m.seq, m.sent_by"
`},
		{name: "the schema", rel: ingestDir + "/schema.go", src: `package ingest

const schemaV4 = "ALTER TABLE messages ADD COLUMN sent_by TEXT"
`},
		{name: "a test", rel: ingestDir + "/x_test.go", src: `package ingest

const q = "SELECT sent_by FROM messages"
`},
		{name: "words near the column name", rel: ingestDir + "/x.go", src: `package ingest

type Message struct{ SentBy string }

var _ = []string{"sent_bytes", "presented_by", "sent by a client", "sent", "by"}
`},
	},
}

func checkSentBy(f *sourceFile) []string {
	if f.test || sentByFiles[f.rel] {
		return nil
	}
	var out []string
	literalRuns(f.file, func(at ast.Node, s string) {
		if sentByColumn.MatchString(s) {
			out = append(out, f.at(at, "%q names the messages.sent_by column, which only %s writes and only the schema and the scoped message columns name", s, sendsFile))
		}
	})
	assembledSQL(f.file, sentByColumn, func(at ast.Expr, text, _ string) {
		out = append(out, f.at(at, "%q, assembled from constants of this file, names the messages.sent_by column, which only %s writes", text, sendsFile))
	})
	return out
}

var auditDeleteRule = rule{
	name:  "audit-delete-confinement",
	check: checkAuditDelete,
	cases: []snippet{
		{name: "audit rows deleted outside the prune file", rel: storeDir + "/admin/audit.go", want: 5, src: `package admin

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

const (
	prune  = "DELETE FROM audit WHERE id <= ?1"
	folded = "delete  from\n\taudit WHERE id = ?1"
	quoted = "DELETE FROM \"audit\""
	schema = "DELETE FROM main.audit"
	verb   = "DELETE FROM "
	table  = "audit WHERE id = ?1"
)

func f(ctx context.Context, q querier) {
	_, _ = q.ExecContext(ctx, verb+table)
}
`},
		{name: "audit rows deleted by the prune file", rel: auditPrune, src: `package admin

const prune = "DELETE FROM audit WHERE id <= ?1"
`},
		{name: "the anchor, the trigger and words near the statement", rel: ingestDir + "/schema.go", src: `package ingest

const (
	anchor  = "DELETE FROM audit_anchor"
	trigger = "CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit"
	words   = "audit rows are append-only"
)
`},
		{name: "a test", rel: storeDir + "/admin/x_test.go", src: `package admin

const q = "DELETE FROM audit WHERE id = 12"
`},
	},
}

func checkAuditDelete(f *sourceFile) []string {
	if f.test || f.rel == auditPrune {
		return nil
	}
	var out []string
	literalRuns(f.file, func(at ast.Node, s string) {
		if auditDeleteQuery.MatchString(s) {
			out = append(out, f.at(at, "%q deletes audit rows outside %s, the one file that prunes the audit chain up to its signed anchor", s, auditPrune))
		}
	})
	assembledSQL(f.file, auditDeleteQuery, func(at ast.Expr, text, _ string) {
		out = append(out, f.at(at, "%q, assembled from constants of this file, deletes audit rows outside %s", text, auditPrune))
	})
	return out
}
