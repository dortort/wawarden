package archtest

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

const (
	keysDir     = "internal/keys"
	keysDirName = "keys"
)

var keysDirectoryRule = rule{
	name:  "keys-directory",
	check: checkKeysDirectory,
	cases: []snippet{
		{name: "the keys directory named outside internal/keys", rel: "internal/backup/x.go", want: 5, src: `package backup

import "path/filepath"

const staged = "/data/keys/"

func f(dir string) []string {
	return []string{
		filepath.Join(dir, "keys", "master"),
		filepath.Join(dir, "keys/master"),
		dir + "/keys",
		filepath.Join(dir, "ke"+"ys"),
	}
}
`},
		{name: "the keys directory named in an internal/keys subpackage", rel: "internal/keys/sub/x.go", src: `package sub

const dir = "keys"
`},
		{name: "the keys directory named in internal/keys", rel: "internal/keys/x.go", src: `package keys

const dir = "keys"
`},
		{name: "a test outside internal/keys", rel: "cmd/wawarden/x_test.go", src: `package main

import "path/filepath"

func f(data string) string { return filepath.Join(data, "keys") }
`},
		{name: "the keys package imported, and words near the directory name", rel: "cmd/wawarden/x.go", src: `package main

import "github.com/dortort/wawarden/internal/keys"

var _ = []string{"keys_loaded", "key_id", "monkeys", "keys-dir", "the keys directory", "keys.go", "master"}

var _ = keys.Load
`},
	},
}

func checkKeysDirectory(f *sourceFile) []string {
	if f.test || within(f.dir, keysDir) {
		return nil
	}
	var out []string
	for _, decl := range f.file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		literalRuns(decl, func(at ast.Node, s string) {
			if slices.Contains(strings.Split(s, "/"), keysDirName) {
				out = append(out, f.at(at, "%q names the keys directory of the data directory, which only %s may read or write", s, keysDir))
			}
		})
	}
	return out
}
