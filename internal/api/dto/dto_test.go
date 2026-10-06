package dto_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/api/dto"
	"github.com/dortort/wawarden/internal/metrics"
)

var forbiddenKeys = []string{"raw", "media_meta", "sender_alt", "seq", "token"}

func samples() []dto.Response {
	last := "2026-10-05T12:00:00Z"
	return []dto.Response{
		dto.Error{Code: "not_found"},
		dto.Health{Status: "ok"},
		dto.Metrics(metrics.NewRegistry()),
		dto.Status{State: "connected", Paired: true, Counts: dto.StatusCounts{Chats: 1}, Clients: dto.StatusClients{AllChatsActive: 1}, Warnings: []string{"all_chats_client"}, LastIngestAt: &last, Version: "dev"},
		dto.Pairing{Code: "ABCD1234"},
		dto.Accepted{Status: "accepted"},
		sampleClient(),
		dto.ClientCreated{Client: sampleClient(), Credential: "ww_synthetic"},
		dto.ClientList{Clients: []dto.ClientSummary{{ID: "aaaaaaaa", Name: "agent", State: "revoked", RevokedAt: &last, ReadChatCount: 1}}},
		dto.AdminChats{Chats: []dto.AdminChat{{ID: "120363000000000001@g.us", Kind: "group", Ref: strings.Repeat("a", 32)}}, Truncated: true},
	}
}

func sampleClient() dto.Client {
	name := "Synthetic Group"
	return dto.Client{
		ID: "aaaaaaaa", Name: "agent", State: "active", CreatedAt: "2026-10-05T12:00:00Z", ExpiresAt: "2027-01-03T12:00:00Z",
		ReadChats:  []dto.ClientChat{{ID: "120363000000000001@g.us", Kind: "group", Known: true, Name: &name}},
		WriteChats: []dto.ClientChat{},
	}
}

func TestOnlyTheCreateResponseCarriesACredential(t *testing.T) {
	owners := map[string]int{}
	visited := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		if visited[typ] {
			return
		}
		visited[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if strings.EqualFold(name, "credential") || name == "" && strings.EqualFold(f.Name, "credential") {
					owners[typ.Name()]++
				}
				walk(f.Type)
			}
		}
	}
	for _, r := range samples() {
		walk(reflect.TypeOf(r))
	}
	if len(owners) != 1 || owners["ClientCreated"] != 1 {
		t.Fatalf("the key credential is declared by %v, want once by ClientCreated only", owners)
	}
	for _, r := range samples() {
		_, body, err := dto.Encode(r)
		if err != nil {
			t.Fatalf("Encode(%T): %v", r, err)
		}
		var v any
		if json.Unmarshal(body, &v) != nil {
			continue
		}
		n := 0
		for _, key := range objectKeys(v) {
			if strings.EqualFold(key, "credential") {
				n++
			}
		}
		if _, created := r.(dto.ClientCreated); created && n != 1 || !created && n != 0 {
			t.Fatalf("Encode(%T) produced the key credential %d times", r, n)
		}
	}
}

func TestEncode(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.Counter("wawarden_example_total", "Synthetic example.").Inc()
	tests := []struct {
		name            string
		response        dto.Response
		wantContentType string
		wantBody        string
	}{
		{
			name:            "error",
			response:        dto.Error{Code: "not_found"},
			wantContentType: "application/json; charset=utf-8",
			wantBody:        `{"error":"not_found"}`,
		},
		{
			name:            "health",
			response:        dto.Health{Status: "unavailable"},
			wantContentType: "application/json; charset=utf-8",
			wantBody:        `{"status":"unavailable"}`,
		},
		{
			name:            "status",
			response:        dto.Status{State: "disconnected", Reason: "replaced", Paired: true, Counts: dto.StatusCounts{Chats: 2, Messages: 3, BlobsPending: 4, BlobsQuarantined: 5, InboxBacklog: 6, InboxQuarantined: 7}, Clients: dto.StatusClients{Active: 8, Expired: 9, Revoked: 10, AllChatsActive: 1}, Warnings: []string{"all_chats_client"}, Version: "v0.2.0"},
			wantContentType: "application/json; charset=utf-8",
			wantBody:        `{"state":"disconnected","reason":"replaced","paired":true,"counts":{"chats":2,"messages":3,"history_blobs_pending":4,"history_blobs_quarantined":5,"inbox_backlog":6,"inbox_quarantined":7},"clients":{"active":8,"expired":9,"revoked":10,"all_chats_active":1},"warnings":["all_chats_client"],"last_ingest_at":null,"version":"v0.2.0"}`,
		},
		{
			name:            "pairing",
			response:        dto.Pairing{Code: "ABCD1234"},
			wantContentType: "application/json; charset=utf-8",
			wantBody:        `{"code":"ABCD1234"}`,
		},
		{
			name:            "accepted",
			response:        dto.Accepted{Status: "accepted"},
			wantContentType: "application/json; charset=utf-8",
			wantBody:        `{"status":"accepted"}`,
		},
		{
			name:            "metrics",
			response:        dto.Metrics(reg),
			wantContentType: "text/plain; version=0.0.4; charset=utf-8",
			wantBody:        "# HELP wawarden_example_total Synthetic example.\n# TYPE wawarden_example_total counter\nwawarden_example_total 1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contentType, body, err := dto.Encode(tt.response)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if contentType != tt.wantContentType || string(body) != tt.wantBody {
				t.Fatalf("Encode() = %q, %q; want %q, %q", contentType, body, tt.wantContentType, tt.wantBody)
			}
		})
	}
}

func TestEncodeRefusesMissingContent(t *testing.T) {
	tests := []struct {
		name     string
		response dto.Response
	}{
		{name: "nil response"},
		{name: "zero metrics payload", response: dto.Prometheus{}},
		{name: "metrics without a registry", response: dto.Metrics(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if contentType, body, err := dto.Encode(tt.response); err == nil {
				t.Fatalf("Encode() = %q, %q; want an error", contentType, body)
			}
		})
	}
}

type wrappedError struct {
	dto.Error
	Token string `json:"token"`
}

type wrappedPayload struct {
	*dto.Prometheus
}

func TestEncodeRefusesTypesDeclaredOutsideDTO(t *testing.T) {
	tests := []struct {
		name     string
		response dto.Response
	}{
		{name: "embedding a dto type", response: wrappedError{Error: dto.Error{Code: "not_found"}, Token: "synthetic"}},
		{name: "embedding a pointer to a dto type", response: wrappedPayload{Prometheus: new(dto.Metrics(metrics.NewRegistry()))}},
		{name: "unnamed struct embedding a dto type", response: struct{ dto.Health }{dto.Health{Status: "ok"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if contentType, body, err := dto.Encode(tt.response); err == nil {
				t.Fatalf("Encode(%T) = %q, %q; want an error", tt.response, contentType, body)
			}
		})
	}
	for _, r := range append(samples(), &dto.Error{Code: "not_found"}) {
		if _, _, err := dto.Encode(r); err != nil {
			t.Fatalf("Encode(%T): %v", r, err)
		}
	}
}

func TestNoForbiddenKeyCanBeProduced(t *testing.T) {
	visited := make(map[reflect.Type]bool)
	for _, r := range samples() {
		for _, problem := range jsonKeyProblems(reflect.TypeOf(r), reflect.TypeOf(r).Name(), visited) {
			t.Error(problem)
		}
	}

	reached := make(map[string]bool)
	pkgPath := reflect.TypeFor[dto.Error]().PkgPath()
	for typ := range visited {
		if typ.PkgPath() == pkgPath {
			reached[typ.Name()] = true
		}
	}
	for _, name := range declaredTypes(t) {
		if !reached[name] {
			t.Errorf("type dto.%s is not reached from samples(), so its JSON keys are unchecked: add it there", name)
		}
	}
}

func TestMarshalledSamplesHoldNoForbiddenKey(t *testing.T) {
	for _, r := range samples() {
		contentType, body, err := dto.Encode(r)
		if err != nil {
			t.Fatalf("Encode(%T): %v", r, err)
		}
		if !strings.HasPrefix(contentType, "application/json") {
			continue
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("Encode(%T) is not JSON: %v", r, err)
		}
		for _, key := range objectKeys(v) {
			if isForbidden(key) {
				t.Errorf("Encode(%T) produced the key %q", r, key)
			}
		}
	}
}

func TestFirewallCatchesForbiddenShapes(t *testing.T) {
	type nested struct {
		Seq int
	}
	type embedded struct {
		Token string
	}
	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{name: "field named after a forbidden key", typ: reflect.TypeFor[struct{ Raw []byte }]()},
		{name: "tag naming a forbidden key", typ: reflect.TypeFor[struct {
			Alt string `json:"sender_alt,omitempty"`
		}]()},
		{name: "tag naming a forbidden key in another case", typ: reflect.TypeFor[struct {
			Meta string `json:"Media_Meta"`
		}]()},
		{name: "nested in a slice of pointers", typ: reflect.TypeFor[struct{ Items []*nested }]()},
		{name: "promoted from an embedded struct", typ: reflect.TypeFor[struct{ embedded }]()},
		{name: "map", typ: reflect.TypeFor[struct{ Extra map[string]string }]()},
		{name: "interface", typ: reflect.TypeFor[struct{ Extra any }]()},
		{name: "custom marshaller", typ: reflect.TypeFor[struct{ Extra json.RawMessage }]()},
		{name: "exported field hidden from JSON", typ: reflect.TypeFor[struct {
			Text []byte `json:"-"`
		}]()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if problems := jsonKeyProblems(tt.typ, "fixture", make(map[reflect.Type]bool)); len(problems) == 0 {
				t.Fatalf("the firewall accepted %s", tt.typ)
			}
		})
	}
}

func isForbidden(key string) bool {
	return slices.ContainsFunc(forbiddenKeys, func(f string) bool { return strings.EqualFold(f, key) })
}

var marshaler = reflect.TypeFor[json.Marshaler]()

func jsonKeyProblems(typ reflect.Type, path string, visited map[reflect.Type]bool) []string {
	if visited[typ] {
		return nil
	}
	visited[typ] = true
	if typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler) {
		return []string{fmt.Sprintf("%s: %s marshals itself, so its keys cannot be checked", path, typ)}
	}
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return jsonKeyProblems(typ.Elem(), path+"[]", visited)
	case reflect.Map:
		return []string{fmt.Sprintf("%s: a map can produce any key", path)}
	case reflect.Interface:
		return []string{fmt.Sprintf("%s: an interface can produce any key", path)}
	case reflect.Struct:
		var problems []string
		for i := range typ.NumField() {
			problems = append(problems, fieldProblems(typ.Field(i), path, visited)...)
		}
		return problems
	default:
		return nil
	}
}

func fieldProblems(f reflect.StructField, path string, visited map[reflect.Type]bool) []string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		if f.IsExported() {
			return []string{fmt.Sprintf("%s.%s: an exported field hidden from JSON lets any caller set content the firewall cannot check", path, f.Name)}
		}
		return nil
	}
	name, _, _ := strings.Cut(tag, ",")
	inner := f.Type
	if inner.Kind() == reflect.Pointer {
		inner = inner.Elem()
	}
	if f.Anonymous && name == "" && inner.Kind() == reflect.Struct {
		return jsonKeyProblems(f.Type, path, visited)
	}
	if !f.IsExported() {
		return nil
	}
	if name == "" {
		name = f.Name
	}
	var problems []string
	if isForbidden(name) {
		problems = append(problems, fmt.Sprintf("%s.%s: forbidden key %q", path, f.Name, name))
	}
	return append(problems, jsonKeyProblems(f.Type, path+"."+name, visited)...)
}

func objectKeys(v any) []string {
	var keys []string
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			keys = append(keys, k)
			keys = append(keys, objectKeys(child)...)
		}
	case []any:
		for _, child := range v {
			keys = append(keys, objectKeys(child)...)
		}
	}
	return keys
}

func declaredTypes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok {
				if _, isInterface := spec.Type.(*ast.InterfaceType); !isInterface {
					names = append(names, spec.Name.Name)
				}
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatal("found no type declarations in package dto")
	}
	return names
}
