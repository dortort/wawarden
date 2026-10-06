package archtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	storeDir        = "internal/store"
	dbDir           = storeDir + "/internal/db"
	sessionDir      = storeDir + "/session"
	sqliteMod       = "modernc.org/sqlite"
	libcModule      = "modernc.org/libc"
	whatsmeowModule = "go.mau.fi/whatsmeow"
	protobufModule  = "google.golang.org/protobuf"
	signalModule    = "go.mau.fi/libsignal"
	rapidModule     = "pgregory.net/rapid"
)

type confinement struct {
	pkg  string
	dirs []string
}

var confinements = []confinement{
	{pkg: "database/sql", dirs: []string{storeDir}},
	{pkg: sqliteMod, dirs: []string{dbDir}},
	{pkg: whatsmeowModule, dirs: []string{adapterDir, sessionDir}},
	{pkg: protobufModule, dirs: []string{adapterDir}},
	{pkg: signalModule, dirs: []string{adapterDir}},
	{pkg: ageModule, dirs: []string{backupDir}},
	{pkg: rapidModule},
}

var (
	sessionImports = set(whatsmeowModule+"/store", whatsmeowModule+"/store/sqlstore")
	adapterSignal  = set(signalModule + "/logger")
)

var confinementRule = rule{
	name:  "confined-imports",
	check: checkConfinement,
	cases: []snippet{
		{name: "the database packages and the driver outside the store", rel: "internal/app/x.go", want: 4, src: `package app

import (
	"database/sql"
	"database/sql/driver"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)
`},
		{name: "the driver in a store package other than db", rel: "internal/store/ingest/x.go", want: 1, src: `package ingest

import (
	"database/sql"
	_ "modernc.org/sqlite"
)
`},
		{name: "a directory that only starts like the store", rel: "internal/storex/x.go", want: 1, src: `package storex

import "database/sql"
`},
		{name: "the db package", rel: "internal/store/internal/db/x.go", src: `package db

import (
	"database/sql"
	"database/sql/driver"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)
`},
		{name: "another store package", rel: "internal/store/admin/x.go", src: `package admin

import "database/sql"
`},
		{name: "a test", rel: "internal/app/x_test.go", src: `package app

import (
	"database/sql"
	_ "modernc.org/sqlite"
)
`},
		{name: "neighbouring import paths", rel: "internal/app/x.go", src: `package app

import (
	_ "database/sqlx"
	_ "modernc.org/sqlitex"
	_ "go.mau.fi/whatsmeowx"
	_ "google.golang.org/protobufx"
	_ "filippo.io/agex"
)
`},
		{name: "age outside the backup package", rel: "internal/app/x.go", want: 2, src: `package app

import (
	"filippo.io/age"
	"filippo.io/age/armor"
)
`},
		{name: "the backup package", rel: "internal/backup/x.go", src: `package backup

import (
	"filippo.io/age"
	"filippo.io/age/armor"
)
`},
		{name: "the protocol library and protobuf outside the adapter", rel: "internal/app/x.go", want: 4, src: `package app

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)
`},
		{name: "the adapter", rel: "internal/engine/wa/x.go", src: `package wa

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)
`},
		{name: "the session store imports its two packages", rel: "internal/store/session/x.go", src: `package session

import (
	"database/sql"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
)
`},
		{name: "the session store imports more of the protocol library and protobuf", rel: "internal/store/session/x.go", want: 5, src: `package session

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore/upgrades"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)
`},
		{name: "a protocol library test elsewhere", rel: "internal/app/x_test.go", src: `package app

import "go.mau.fi/whatsmeow/types"
`},
		{name: "the signal library outside the adapter", rel: "internal/store/session/x.go", want: 2, src: `package session

import (
	"go.mau.fi/libsignal/logger"
	"go.mau.fi/libsignal/session"
)
`},
		{name: "the adapter imports the signal library beyond its logger", rel: "internal/engine/wa/x.go", want: 2, src: `package wa

import (
	"go.mau.fi/libsignal"
	"go.mau.fi/libsignal/logger"
	"go.mau.fi/libsignal/protocol"
	_ "go.mau.fi/libsignalx"
)
`},
		{name: "a signal library test in the adapter", rel: "internal/engine/wa/x_test.go", src: `package wa

import "go.mau.fi/libsignal/protocol"
`},
		{name: "a test-only module outside tests", rel: "internal/store/scoped/x.go", want: 2, src: `package scoped

import (
	"pgregory.net/rapid"
	_ "pgregory.net/rapid/sub"
	_ "pgregory.net/rapidx"
)
`},
		{name: "a test-only module in tests", rel: "internal/store/scoped/x_test.go", src: `package scoped

import "pgregory.net/rapid"
`},
	},
}

func checkConfinement(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var out []string
	for _, imp := range f.imports {
		var best *confinement
		for i := range confinements {
			c := &confinements[i]
			if within(imp.path, c.pkg) && (best == nil || len(c.pkg) > len(best.pkg)) {
				best = c
			}
		}
		if best != nil && len(best.dirs) == 0 {
			out = append(out, f.at(imp.node, "%q is a test-only module: only test files may import it, so it is never linked into the binary", imp.path))
			continue
		}
		if best != nil && !slices.ContainsFunc(best.dirs, func(d string) bool { return within(f.dir, d) }) {
			out = append(out, f.at(imp.node, "%q may be imported only under %s", imp.path, strings.Join(best.dirs, ", ")))
			continue
		}
		if within(f.dir, sessionDir) && within(imp.path, whatsmeowModule) && !sessionImports[imp.path] {
			out = append(out, f.at(imp.node, "%s may import only the protocol library's device store packages, not %q", sessionDir, imp.path))
		}
		if within(f.dir, adapterDir) && within(imp.path, signalModule) && !adapterSignal[imp.path] {
			out = append(out, f.at(imp.node, "%s may import only the signal library's logger package, to route its lines through the scrubbing writer, not %q", adapterDir, imp.path))
		}
	}
	return out
}

func requiredVersion(gomod, module string) string {
	for line := range strings.Lines(gomod) {
		code, _, _ := strings.Cut(line, "//")
		fields := strings.Fields(code)
		if len(fields) > 0 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) == 2 && fields[0] == module {
			return fields[1]
		}
	}
	return ""
}

func TestRequiredVersion(t *testing.T) {
	const gomod = "module x\n\nrequire example.com/a v1.0.0\n\nrequire (\n\texample.com/b v1.2.3 // indirect\n\texample.com/bb v9.9.9\n)\n"
	for module, want := range map[string]string{"example.com/a": "v1.0.0", "example.com/b": "v1.2.3", "example.com/c": ""} {
		if got := requiredVersion(gomod, module); got != want {
			t.Errorf("requiredVersion(%s) = %q, want %q", module, got, want)
		}
	}
}

func TestSQLiteRuntimeIsTheDriversOwn(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Path}} {{.Version}} {{.GoMod}}", sqliteMod, libcModule)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	versions, gomods := map[string]string{}, map[string]string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("unexpected go list line %q", line)
		}
		versions[fields[0]], gomods[fields[0]] = fields[1], fields[2]
	}
	data, err := os.ReadFile(filepath.Clean(gomods[sqliteMod]))
	if err != nil {
		t.Fatalf("read the go.mod of %s: %v", sqliteMod, err)
	}
	want := requiredVersion(string(data), libcModule)
	if want == "" || versions[libcModule] != want {
		t.Fatalf("this module builds %s %s with %s %s, but the driver requires %s %q: its documentation requires exactly the driver's version", sqliteMod, versions[sqliteMod], libcModule, versions[libcModule], libcModule, want)
	}
}
