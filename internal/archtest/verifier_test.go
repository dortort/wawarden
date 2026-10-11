package archtest

import (
	"context"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	inspectDir   = storeDir + "/inspect"
	cmdDir       = "cmd/wawarden"
	verifierFile = cmdDir + "/backup.go"
	checkerFile  = backupDir + "/verify.go"
)

var (
	clientDenied   = []string{whatsmeowModule, module + "/" + engineDir, module + "/" + sessionDir, module + "/" + appDir}
	verifierDenied = append(slices.Clone(clientDenied), ageModule)
)

var verifierFenceRule = rule{
	name:  "backup-verifier-fence",
	check: checkVerifierFence,
	cases: []snippet{
		{name: "the store checks imported outside the command", rel: "internal/app/x.go", want: 2, src: `package app

import (
	"github.com/dortort/wawarden/internal/store/inspect"
	"github.com/dortort/wawarden/internal/store/inspect/sub"
)
`},
		{name: "the store checks reach for a client or a key", rel: inspectDir + "/x.go", want: 6, src: `package inspect

import (
	"filippo.io/age"
	"go.mau.fi/whatsmeow"
	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/engine/wa"
	"github.com/dortort/wawarden/internal/store/session"
)
`},
		{name: "the verifier command reaches for a client", rel: verifierFile, want: 3, src: `package main

import (
	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/store/inspect"
	"github.com/dortort/wawarden/internal/store/session"
	"go.mau.fi/whatsmeow/types"
)
`},
		{name: "the stream checks decrypt", rel: checkerFile, want: 2, src: `package backup

import (
	"filippo.io/age"
	"filippo.io/age/armor"
)
`},
		{name: "the taker shares the stream checks' package and reaches for a client", rel: backupDir + "/backup.go", want: 4, src: `package backup

import (
	"filippo.io/age"
	"go.mau.fi/whatsmeow"
	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/engine"
	"github.com/dortort/wawarden/internal/store/session"
)
`},
		{name: "another file of the stream checks' package", rel: backupDir + "/sub/x.go", want: 1, src: `package sub

import "github.com/dortort/wawarden/internal/engine/wa"
`},
		{name: "the verifier's own imports", rel: verifierFile, src: `package main

import (
	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/sanitize"
	"github.com/dortort/wawarden/internal/store/inspect"
)
`},
		{name: "the store checks read the archive's tables", rel: inspectDir + "/x.go", src: `package inspect

import (
	"github.com/dortort/wawarden/internal/store/admin"
	"github.com/dortort/wawarden/internal/store/ingest"
	"github.com/dortort/wawarden/internal/store/internal/db"
)
`},
		{name: "the rest of the command and the taker", rel: cmdDir + "/run.go", src: `package main

import (
	"github.com/dortort/wawarden/internal/app"
	"filippo.io/age"
)
`},
		{name: "a test of the store checks", rel: inspectDir + "/x_test.go", src: `package inspect

import "github.com/dortort/wawarden/internal/store/session"
`},
		{name: "a directory that only starts like the store checks", rel: "internal/store/inspector/x.go", src: `package inspector

import "go.mau.fi/whatsmeow"
`},
	},
}

func checkVerifierFence(f *sourceFile) []string {
	if f.test {
		return nil
	}
	var denied []string
	switch {
	case within(f.dir, inspectDir) || f.rel == verifierFile || f.rel == checkerFile:
		denied = verifierDenied
	case within(f.dir, backupDir):
		denied = clientDenied
	}
	var out []string
	for _, imp := range f.imports {
		if within(imp.path, module+"/"+inspectDir) && f.dir != cmdDir {
			out = append(out, f.at(imp.node, "only %s may import %q, the backup verifier's database checks", cmdDir, imp.path))
		}
		if slices.ContainsFunc(denied, func(d string) bool { return within(imp.path, d) }) {
			out = append(out, f.at(imp.node, "%s belongs to the backup verifier or shares its package, which may not import %q: it must never construct a WhatsApp client or decrypt", f.rel, imp.path))
		}
	}
	return out
}

func TestTheBackupVerifierCannotReachAClientOrAKey(t *testing.T) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-f", "{{.ImportPath}}{{range .Deps}} {{.}}{{end}}", module+"/"+inspectDir, module+"/"+backupDir).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	denied := map[string][]string{module + "/" + inspectDir: verifierDenied, module + "/" + backupDir: clientDenied}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(denied) {
		t.Fatalf("go list printed %q, want one line for each of %v", lines, slices.Collect(maps.Keys(denied)))
	}
	for _, line := range lines {
		deps := strings.Fields(line)
		pkg := deps[0]
		refused, ok := denied[pkg]
		if !ok {
			t.Fatalf("go list printed %s twice or a package it was not asked for", pkg)
		}
		delete(denied, pkg)
		for _, dep := range deps[1:] {
			if slices.ContainsFunc(refused, func(d string) bool { return within(dep, d) }) {
				t.Errorf("%s depends on %s, so the backup verifier could construct a WhatsApp client or decrypt", pkg, dep)
			}
		}
	}
}
