package archtest

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
)

var databaseFileName = regexp.MustCompile(`(?i)\.(db|db3|sqlite|sqlite3)(-journal|-wal|-shm)?$`)

var databaseFileRule = rule{
	name:  "database-files",
	check: checkDatabaseFiles,
	cases: []snippet{
		{name: "database files named outside the db package", rel: "internal/backup/x.go", want: 6, src: `package backup

import (
	"os"
	"path/filepath"
)

const staged = "/data/archive.db"

func f(dir, name string) {
	_ = os.Remove(filepath.Join(dir, "session.db"))
	_, _ = os.Stat(dir + "/archive.db-journal")
	_, _ = os.Open(filepath.Join(dir, name+".db"))
	_ = os.Remove(filepath.Join(dir, "x.SQLITE3-wal"))
	_ = "archive" + ".db"
}
`},
		{name: "database files named in the db package", rel: "internal/store/internal/db/x.go", src: `package db

func f(label string) string { return label + ".db" }
`},
		{name: "database files named in a test", rel: "internal/store/ingest/x_test.go", src: `package ingest

const archive = "archive.db"
`},
		{name: "words near database file names", rel: "internal/app/x.go", src: `package app

import _ "example.com/x.db"

var _ = []string{"archive_opened", "db_deadline", "the archive database", "dbx", "x.dbx", "x.db.bak", "db", "db/"}
`},
	},
}

func checkDatabaseFiles(f *sourceFile) []string {
	if f.test || within(f.dir, dbDir) {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		literalRuns(decl, func(at ast.Node, s string) {
			for _, elem := range strings.Split(s, "/") {
				if databaseFileName.MatchString(elem) {
					out = append(out, f.at(at, "%q names a database file, which only %s may open, inspect or remove: closing any descriptor of a database file drops the lock", s, dbDir))
					return
				}
			}
		})
	}
	return out
}

var sqlCalls = map[string]int{
	"Exec": 0, "Query": 0, "QueryRow": 0, "Prepare": 0,
	"ExecContext": 1, "QueryContext": 1, "QueryRowContext": 1, "PrepareContext": 1,
}

var constantSQLRule = rule{
	name:  "constant-sql",
	check: checkConstantSQL,
	cases: []snippet{
		{name: "SQL built from values outside the db package", rel: "internal/store/ingest/x.go", want: 8, src: `package ingest

import (
	"context"
	"fmt"
	"strings"
)

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
	QueryRowContext(context.Context, string, ...any) any
	Query(string, ...any) (any, error)
}

var table = "messages"

func f(ctx context.Context, q querier, column string, parts []string) {
	_, _ = q.ExecContext(ctx, "DELETE FROM "+table)
	_ = q.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM messages", column))
	_, _ = q.Query(strings.Join(parts, " "))
	query := "SELECT 1"
	_, _ = q.Query(query)
	_, _ = q.ExecContext(ctx, table)
	exec := q.ExecContext
	_, _ = exec(ctx, "SELECT 1")
	_ = (querier).QueryRowContext
	run(func(query string) { _, _ = q.Query(query) })
}

func run(func(string)) {}
`},
		{name: "constant SQL", rel: "internal/store/admin/x.go", src: `package admin

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
	QueryRowContext(context.Context, string, ...any) any
	Query(string, ...any) (any, error)
	Exec(string, ...any) (any, error)
}

const (
	selectOne = "SELECT 1"
	where     = " WHERE id = ?"
)

func f(ctx context.Context, q querier, id int64) {
	const local = "SELECT 2"
	_ = q.QueryRowContext(ctx, selectOne)
	_ = q.QueryRowContext(ctx, "SELECT x FROM t"+where, id)
	_, _ = q.ExecContext(ctx, ("DELETE FROM t" + where), id)
	_, _ = q.Query(local)
	_, _ = q.Exec(` + "`SELECT 3`" + `)
}
`},
		{name: "calls of the same names without SQL", rel: "internal/app/x.go", src: `package app

import "net/url"

func f(u *url.URL) url.Values { return u.Query() }
`},
		{name: "the db package and the IN builder", rel: "internal/store/internal/db/x.go", src: `package db

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

func f(ctx context.Context, q querier, query string) { _, _ = q.ExecContext(ctx, query) }
`},
		{name: "a test", rel: "internal/store/ingest/x_test.go", src: `package ingest

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

func f(ctx context.Context, q querier, table string) { _, _ = q.ExecContext(ctx, "SELECT count(*) FROM "+table) }
`},
	},
}

func checkConstantSQL(f *sourceFile) []string {
	if f.test || within(f.dir, dbDir) || f.rel == inBuilder {
		return nil
	}
	consts := map[string]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if gen, ok := n.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					consts[id.Name] = true
				}
			}
		}
		return true
	})
	var out []string
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		i, ok := sqlCalls[sel.Sel.Name]
		if !ok || len(call.Args) <= i {
			return true
		}
		called[sel] = true
		if !constantSQL(call.Args[i], consts) {
			out = append(out, f.at(call.Args[i], "the SQL text of %s is not a constant of this file: build no SQL from values, and keep generic SQL plumbing in %s", sel.Sel.Name, dbDir))
		}
		return true
	})
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || called[sel] {
			return true
		}
		if i, ok := sqlCalls[sel.Sel.Name]; ok && i == 1 {
			out = append(out, f.at(sel, "%s used as a value hides the SQL text of its calls from this rule", sel.Sel.Name))
		}
		return true
	})
	return out
}

func constantSQL(e ast.Expr, consts map[string]bool) bool {
	for _, operand := range addOperands(e) {
		switch o := ast.Unparen(operand).(type) {
		case *ast.BasicLit:
			if o.Kind != token.STRING {
				return false
			}
		case *ast.Ident:
			if !consts[o.Name] {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var sqliteStatement = regexp.MustCompile(`(?i)\b(?:PRAGMA|VACUUM)\b|\b(?:ATTACH|DETACH)\s+(?:DATABASE\b|['"?:@$(])`)

var sqliteStatementRule = rule{
	name:  "sqlite-statements",
	check: checkSQLiteStatements,
	cases: []snippet{
		{name: "pragmas and file statements outside the db package", rel: "internal/store/ingest/x.go", want: 6, src: `package ingest

const (
	a = "PRAGMA journal_mode = DELETE"
	b = "pragma secure_delete=off"
	c = "VACUUM INTO ?"
	d = "ATTACH DATABASE ? AS other"
	e = "attach 'file:/data/x' as y"
)

var f = "DETACH" + " DATABASE other"
`},
		{name: "the db package", rel: "internal/store/internal/db/x.go", src: `package db

const a = "PRAGMA user_version"
`},
		{name: "a test", rel: "internal/store/ingest/x_test.go", src: `package ingest

const a = "PRAGMA secure_delete = OFF"
`},
		{name: "prose and table-valued pragma functions", rel: "internal/app/x.go", src: `package app

var _ = []string{"SELECT name FROM pragma_table_info('messages')", "attach the logger", "a vacuumed floor", "detached"}
`},
	},
}

func checkSQLiteStatements(f *sourceFile) []string {
	if f.test || within(f.dir, dbDir) {
		return nil
	}
	var out []string
	literalRuns(f.file, func(at ast.Node, s string) {
		if m := sqliteStatement.FindString(s); m != "" {
			out = append(out, f.at(at, "%q runs %s, which only %s may: it can change a verified pragma or open another database file", s, strings.ToUpper(strings.Fields(m)[0]), dbDir))
		}
	})
	return out
}
