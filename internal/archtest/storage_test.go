package archtest

import (
	"go/ast"
	"go/token"
	"regexp"
	"slices"
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

const migrateCall = "Migrate"

func sqlArguments(file *ast.File, visit func(sel *ast.SelectorExpr, texts []ast.Expr)) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == migrateCall && len(call.Args) > 1 {
			lit, ok := ast.Unparen(call.Args[1]).(*ast.CompositeLit)
			if !ok {
				visit(sel, call.Args[1:2])
				return true
			}
			texts := make([]ast.Expr, 0, len(lit.Elts))
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					e = kv.Value
				}
				texts = append(texts, e)
			}
			visit(sel, texts)
			return true
		}
		if i, ok := sqlCalls[sel.Sel.Name]; ok && len(call.Args) > i {
			visit(sel, call.Args[i:i+1])
		}
		return true
	})
}

func fileConstants(file *ast.File) map[string]ast.Expr {
	values := map[string]ast.Expr{}
	declared := map[string]int{}
	declare := func(ids ...*ast.Ident) {
		for _, id := range ids {
			declared[id.Name]++
		}
	}
	fields := func(list *ast.FieldList) {
		if list != nil {
			for _, field := range list.List {
				declare(field.Names...)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.GenDecl:
			var last []ast.Expr
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					declare(s.Names...)
					if d.Tok != token.CONST {
						continue
					}
					if len(s.Values) > 0 {
						last = s.Values
					}
					for i, id := range s.Names {
						if i < len(last) {
							values[id.Name] = last[i]
						}
					}
				case *ast.TypeSpec:
					declare(s.Name)
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				declare(d.Name)
			}
			fields(d.Recv)
		case *ast.FuncType:
			fields(d.TypeParams)
			fields(d.Params)
			fields(d.Results)
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE {
				for _, e := range d.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						declare(id)
					}
				}
			}
		case *ast.RangeStmt:
			if d.Tok == token.DEFINE {
				for _, e := range []ast.Expr{d.Key, d.Value} {
					if id, ok := e.(*ast.Ident); ok {
						declare(id)
					}
				}
			}
		}
		return true
	})
	for name := range values {
		if declared[name] != 1 {
			delete(values, name)
		}
	}
	return values
}

func constantText(e ast.Expr, consts map[string]ast.Expr, resolving map[string]bool) (text string, runs []string, ok bool) {
	var b, run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			runs = append(runs, run.String())
			run.Reset()
		}
	}
	for _, operand := range addOperands(e) {
		if s, ok := stringLit(operand); ok {
			b.WriteString(s)
			run.WriteString(s)
			continue
		}
		id, ok := ast.Unparen(operand).(*ast.Ident)
		if !ok || consts[id.Name] == nil || resolving[id.Name] {
			return "", nil, false
		}
		flush()
		resolving[id.Name] = true
		s, inner, ok := constantText(consts[id.Name], consts, resolving)
		delete(resolving, id.Name)
		if !ok {
			return "", nil, false
		}
		b.WriteString(s)
		runs = append(runs, inner...)
	}
	flush()
	return b.String(), runs, true
}

func assembledSQL(file *ast.File, re *regexp.Regexp, visit func(at ast.Expr, text, match string)) {
	consts := fileConstants(file)
	sqlArguments(file, func(_ *ast.SelectorExpr, texts []ast.Expr) {
		for _, e := range texts {
			text, runs, ok := constantText(e, consts, map[string]bool{})
			if ok && !slices.ContainsFunc(runs, re.MatchString) {
				if m := re.FindString(text); m != "" {
					visit(e, text, m)
				}
			}
		}
	})
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
		{name: "migration steps and constants that are not this file's alone", rel: "internal/store/ingest/x.go", want: 6, src: `package ingest

import (
	"context"
	"strings"
)

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

type migrator interface {
	Migrate(context.Context, []string) (int, error)
}

var steps = []string{"CREATE TABLE t (x)"}

const (
	v1     = "CREATE TABLE t (x)"
	query  = "SELECT 1"
	column = "x"
)

func f(ctx context.Context, q querier, d migrator, extra string) {
	_, _ = d.Migrate(ctx, steps)
	_, _ = d.Migrate(ctx, []string{v1, strings.Join([]string{"PRA", "GMA secure_delete = OFF"}, "")})
	_, _ = d.Migrate(ctx, append([]string{v1}, extra))
	migrate := d.Migrate
	_ = migrate
	_, _ = q.ExecContext(ctx, "SELECT "+column+" FROM t")
	query := extra
	_, _ = q.ExecContext(ctx, query)
}

func g() string {
	const column = "y"
	return column
}
`},
		{name: "migration steps from constants of this file", rel: "internal/store/ingest/x.go", src: `package ingest

import "context"

type migrator interface {
	Migrate(context.Context, []string) (int, error)
}

const (
	v1     = "CREATE TABLE t (x)"
	v2     = "ALTER TABLE t ADD COLUMN y" + suffix
	suffix = " INTEGER"
)

func f(ctx context.Context, d migrator) (int, error) {
	return d.Migrate(ctx, []string{v1, v2})
}
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
	consts := fileConstants(f.file)
	var out []string
	called := map[*ast.SelectorExpr]bool{}
	sqlArguments(f.file, func(sel *ast.SelectorExpr, texts []ast.Expr) {
		called[sel] = true
		for _, e := range texts {
			if _, _, ok := constantText(e, consts, map[string]bool{}); !ok {
				out = append(out, f.at(e, "the SQL text of %s is not a constant of this file: build no SQL from values, pass %s a slice literal of such constants, and keep generic SQL plumbing in %s", sel.Sel.Name, migrateCall, dbDir))
			}
		}
	})
	ast.Inspect(f.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || called[sel] {
			return true
		}
		if i, ok := sqlCalls[sel.Sel.Name]; (ok && i == 1) || sel.Sel.Name == migrateCall {
			out = append(out, f.at(sel, "%s used as a value hides the SQL text of its calls from this rule", sel.Sel.Name))
		}
		return true
	})
	return out
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
		{name: "statements assembled from constants of the file", rel: "internal/store/ingest/x.go", want: 6, src: `package ingest

import "context"

type querier interface {
	ExecContext(context.Context, string, ...any) (any, error)
}

type migrator interface {
	Migrate(context.Context, []string) (int, error)
}

const (
	pra    = "PRA"
	off    = pra + "GMA secure_delete = OFF"
	att    = "ATT"
	vacuum = "VACUUM"
	v1     = "CREATE TABLE t (x)"
)

func f(ctx context.Context, q querier, d migrator) {
	_, _ = q.ExecContext(ctx, pra+"GMA secure_delete=OFF")
	_, _ = q.ExecContext(ctx, off)
	_, _ = q.ExecContext(ctx, att+"ACH DATABASE ':memory:' AS other")
	_, _ = d.Migrate(ctx, []string{v1, pra + "GMA journal_mode = DELETE"})
	_, _ = q.ExecContext(ctx, vacuum)
	_, _ = q.ExecContext(ctx, "VAC"+"UUM")
}
`},
		{name: "SQL assembled from constants without such statements", rel: "internal/store/ingest/x.go", src: `package ingest

import "context"

type querier interface {
	QueryContext(context.Context, string, ...any) (any, error)
}

const (
	columns = "name"
	attach  = "attach"
)

func f(ctx context.Context, q querier) {
	_, _ = q.QueryContext(ctx, "SELECT "+columns+" FROM pragma_table_info('messages')")
	_, _ = q.QueryContext(ctx, "SELECT '"+attach+"ed'")
}
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
	assembledSQL(f.file, sqliteStatement, func(at ast.Expr, text, m string) {
		out = append(out, f.at(at, "%q, assembled from constants of this file, runs %s, which only %s may: it can change a verified pragma or open another database file", text, strings.ToUpper(strings.Fields(m)[0]), dbDir))
	})
	return out
}

var rawHandleRule = rule{
	name:  "raw-database-handle",
	check: checkRawHandle,
	cases: []snippet{
		{name: "the raw handle outside the session store", rel: "internal/store/ingest/x.go", want: 2, src: `package ingest

import "database/sql"

type opener interface{ RawHandle() *sql.DB }

func f(d opener) *sql.DB {
	raw := d.RawHandle
	_ = raw
	return d.RawHandle()
}
`},
		{name: "the session store and the db package", rel: "internal/store/session/x.go", src: `package session

import "database/sql"

func f(d interface{ RawHandle() *sql.DB }) *sql.DB { return d.RawHandle() }
`},
		{name: "a test", rel: "internal/store/internal/db/x_test.go", src: `package db

func f(d *DB) { _ = d.RawHandle() }
`},
	},
}

func checkRawHandle(f *sourceFile) []string {
	if f.test || within(f.dir, dbDir) || within(f.dir, sessionDir) {
		return nil
	}
	var out []string
	ast.Inspect(f.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "RawHandle" {
			out = append(out, f.at(sel, "RawHandle hands out the database handle without deadlines; only %s gives it to the protocol library's device store", sessionDir))
		}
		return true
	})
	return out
}
