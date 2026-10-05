package archtest

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

const (
	backupDirName  = "backups"
	backupDirConst = "Dir"
	configDir      = "internal/config"
)

var backupsDirectoryRule = rule{
	name:  "backups-directory",
	check: checkBackupsDirectory,
	cases: []snippet{
		{name: "the backups directory named outside internal/backup", rel: "internal/app/x.go", want: 6, src: `package app

import (
	"path/filepath"

	"github.com/dortort/wawarden/internal/backup"
)

const staged = "/data/backups/"

func f(dir string) []string {
	return []string{
		filepath.Join(dir, "backups", "x.age"),
		dir + "/backups/tmp",
		filepath.Join(dir, "back"+"ups"),
		"backups",
		filepath.Join(dir, backup.Dir, "x.age"),
	}
}
`},
		{name: "the backups directory named as a literal in internal/config", rel: configDir + "/x.go", want: 1, src: `package config

var dataSubdirectories = []string{"history", "backups"}
`},
		{name: "internal/config inspects it by the backup package's name", rel: configDir + "/x.go", src: `package config

import "github.com/dortort/wawarden/internal/backup"

var dataSubdirectories = []string{"history", backup.Dir}
`},
		{name: "internal/backup names it", rel: backupDir + "/x.go", src: `package backup

const Dir = "backups"
`},
		{name: "a test outside internal/backup", rel: "internal/app/x_test.go", src: `package app

import "path/filepath"

func f(data string) string { return filepath.Join(data, "backups") }
`},
		{name: "the backup package used, and words near the directory name", rel: "internal/app/x.go", src: `package app

import "github.com/dortort/wawarden/internal/backup"

var _ = []string{"backup_done", "backups are taken", "backups.go", "backup", "backups-dir", "tmp"}

var _ = backup.RemoveStaging
`},
	},
}

func checkBackupsDirectory(f *sourceFile) []string {
	if f.test || within(f.dir, backupDir) {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		literalRuns(decl, func(at ast.Node, s string) {
			if slices.Contains(strings.Split(s, "/"), backupDirName) {
				out = append(out, f.at(at, "%q names the backups directory of the data directory, under which only %s writes", s, backupDir))
			}
		})
		if f.dir == configDir {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if e, ok := n.(*ast.SelectorExpr); ok {
				if sel, p := f.ref(e); sel != nil && p == module+"/"+backupDir && sel.Sel.Name == backupDirConst {
					out = append(out, f.at(sel, "%s names the backups directory, under which only %s writes and which only %s inspects at start", backupDir+"."+backupDirConst, backupDir, configDir))
				}
			}
			return true
		})
	}
	return out
}
