package archtest

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

const (
	stageDir      = "internal/stage"
	spoolDirName  = "spool"
	stageDirConst = "Dir"
)

var spoolDirectoryRule = rule{
	name:  "spool-directory",
	check: checkSpoolDirectory,
	cases: []snippet{
		{name: "the staging directory named outside internal/stage", rel: "internal/app/x.go", want: 8, src: `package app

import (
	"path/filepath"

	"github.com/dortort/wawarden/internal/stage"
)

const staged = "/data/spool/"

const spo, ol = "spo", "ol"

func f(dir string) []string {
	return []string{
		filepath.Join(dir, "spool", "sealed-00"),
		dir + "/spool/tmp",
		filepath.Join(dir, "spo"+"ol"),
		"spool",
		filepath.Join(dir, stage.Dir, "sealed-00"),
		filepath.Join(dir, spo+ol),
		dir + "/" + spo + "ol/tmp",
	}
}
`},
		{name: "the staging directory named as a literal in internal/config", rel: configDir + "/x.go", want: 1, src: `package config

var dataSubdirectories = []string{"history", "spool"}
`},
		{name: "internal/config inspects it by the stage package's name", rel: configDir + "/x.go", src: `package config

import (
	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/stage"
)

var dataSubdirectories = []string{"history", backup.Dir, stage.Dir}

var reasons = []string{stage.Dir + "_dir_unusable", stage.Dir + "_dir_permissions"}
`},
		{name: "internal/stage names it", rel: stageDir + "/x.go", src: `package stage

import "path/filepath"

const Dir = "spool"

func dir(data string) string { return filepath.Join(data, "spool") }
`},
		{name: "a test outside internal/stage", rel: "internal/app/x_test.go", src: `package app

import "path/filepath"

func f(data string) string { return filepath.Join(data, "spool") }
`},
		{name: "the stage package used, and words near the directory name", rel: "internal/app/x.go", src: `package app

import "github.com/dortort/wawarden/internal/stage"

var _ = []string{"spooler", "spool_dir_unusable", "spool.go", "spooled bytes", "spool-dir", "a spool of thread", "/spools/"}

var _ = stage.Purge
`},
		{name: "the media kind and the media route", rel: "internal/api/x.go", src: `package api

type Kind string

const KindMedia Kind = "media"

var routes = []string{"POST /v1/messages/{mref}/media", "GET /v1/media/{job}"}
`},
		{name: "the media kind in the engine", rel: "internal/engine/x.go", src: `package engine

const KindMedia = "media"

var path = "/v1/messages/{mref}/media"
`},
	},
}

func checkSpoolDirectory(f *sourceFile) []string {
	if f.test || within(f.dir, stageDir) {
		return nil
	}
	names := func(s string) bool { return slices.Contains(strings.Split(s, "/"), spoolDirName) }
	consts := fileConstants(f.file)
	var out []string
	for _, decl := range f.file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		literalRuns(decl, func(at ast.Node, s string) {
			if names(s) {
				out = append(out, f.at(at, "%q names the staging directory of the data directory, under which only %s writes", s, stageDir))
			}
		})
		constantRuns(decl, consts, func(at ast.Node, s string, runs []string) {
			if names(s) && !slices.ContainsFunc(runs, names) {
				out = append(out, f.at(at, "%q, assembled from constants of this file, names the staging directory of the data directory, under which only %s writes", s, stageDir))
			}
		})
		if f.dir == configDir {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if e, ok := n.(*ast.SelectorExpr); ok {
				if sel, p := f.ref(e); sel != nil && p == module+"/"+stageDir && sel.Sel.Name == stageDirConst {
					out = append(out, f.at(sel, "%s names the staging directory, under which only %s writes and which only %s inspects at start", stageDir+"."+stageDirConst, stageDir, configDir))
				}
			}
			return true
		})
	}
	return out
}
