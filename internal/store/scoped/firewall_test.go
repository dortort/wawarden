package scoped_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/store/scoped"
)

var rowTypes = []reflect.Type{
	reflect.TypeFor[scoped.Chat](),
	reflect.TypeFor[scoped.ChatPage](),
	reflect.TypeFor[scoped.ChatPosition](),
	reflect.TypeFor[scoped.ChangePage](),
	reflect.TypeFor[scoped.ChangePosition](),
	reflect.TypeFor[scoped.Message](),
	reflect.TypeFor[scoped.MessagePage](),
	reflect.TypeFor[scoped.MessagePosition](),
	reflect.TypeFor[scoped.Query](),
	reflect.TypeFor[scoped.Reader](),
	reflect.TypeFor[scoped.SearchPage](),
	reflect.TypeFor[scoped.SearchPosition](),
}

var opaqueTypes = []reflect.Type{reflect.TypeFor[time.Time](), reflect.TypeFor[policy.CanonicalChat]()}

func TestNoRowFieldReachesJSON(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Slice || typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || slices.Contains(opaqueTypes, typ) {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if f.Tag.Get("json") != "-" {
				t.Errorf("%s.%s has no json:\"-\" tag: rows reach a response only through the DTO builders", typ, f.Name)
			}
			walk(f.Type)
		}
	}
	for _, typ := range rowTypes {
		walk(typ)
	}
	for _, typ := range rowTypes {
		if typ == reflect.TypeFor[scoped.Reader]() {
			continue
		}
		out, err := json.Marshal(reflect.New(typ).Interface())
		if err != nil || string(out) != "{}" {
			t.Errorf("json.Marshal(%s) = %s, %v, want {}", typ, out, err)
		}
	}
}

func TestEveryExportedStructIsCheckedByTheFirewall(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok && spec.Name.IsExported() {
				if _, ok := spec.Type.(*ast.StructType); ok {
					declared = append(declared, spec.Name.Name)
				}
			}
			return true
		})
	}
	var checked []string
	for _, typ := range rowTypes {
		checked = append(checked, typ.Name())
	}
	slices.Sort(declared)
	slices.Sort(checked)
	if !slices.Equal(declared, checked) {
		t.Fatalf("exported structs %q, checked %q", declared, checked)
	}
}

func TestRowsCarryNoInternalColumn(t *testing.T) {
	for _, typ := range rowTypes {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, banned := range []string{"raw", "alt", "media_meta", "mediameta", "origin"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s exposes a column that no response carries", typ, typ.Field(i).Name)
				}
			}
		}
	}
}
