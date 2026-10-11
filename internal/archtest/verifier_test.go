package archtest

import (
	"context"
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

var verifierDenied = []string{whatsmeowModule, module + "/" + engineDir, module + "/" + sessionDir, module + "/" + appDir, ageModule}

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
	verifier := within(f.dir, inspectDir) || f.rel == verifierFile || f.rel == checkerFile
	var out []string
	for _, imp := range f.imports {
		if within(imp.path, module+"/"+inspectDir) && f.dir != cmdDir {
			out = append(out, f.at(imp.node, "only %s may import %q, the backup verifier's database checks", cmdDir, imp.path))
		}
		if verifier && slices.ContainsFunc(verifierDenied, func(denied string) bool { return within(imp.path, denied) }) {
			out = append(out, f.at(imp.node, "%s belongs to the backup verifier, which may not import %q: it must never construct a WhatsApp client or decrypt", f.rel, imp.path))
		}
	}
	return out
}

func TestTheBackupVerifierCannotReachAClientOrAKey(t *testing.T) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-deps", "-f", "{{.ImportPath}}", module+"/"+inspectDir).Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, module+"/"+inspectDir) {
		t.Fatalf("go list -deps did not list %s itself: %q", inspectDir, deps)
	}
	for _, dep := range deps {
		if slices.ContainsFunc(verifierDenied, func(denied string) bool { return within(dep, denied) }) {
			t.Errorf("%s depends on %s, so the backup verifier could construct a WhatsApp client or decrypt", inspectDir, dep)
		}
	}
}
