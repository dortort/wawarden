package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

type routedListener struct {
	mu       sync.Mutex
	answers  map[string][2]string
	requests []string
	bodies   []string
}

func newRoutedListener(t *testing.T, answers map[string][2]string) (*routedListener, string) {
	t.Helper()
	f := &routedListener{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.mu.Lock()
		f.requests = append(f.requests, key)
		f.bodies = append(f.bodies, string(b))
		answer, ok := f.answers[key]
		f.mu.Unlock()
		status := http.StatusOK
		if !ok {
			answer = [2]string{"404", `{"error":"not_found"}`}
		}
		switch answer[0] {
		case "404":
			status = http.StatusNotFound
		case "409":
			status = http.StatusConflict
		case "422":
			status = http.StatusUnprocessableEntity
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer[1])
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *routedListener) seen() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]string(nil), f.bodies...)
}

const (
	groupRef     = "0123456789abcdef0123456789abcdef"
	clientJSON   = `{"id":"aaaqeaye","name":"agent","state":"active","created_at":"2026-10-06T12:00:00Z","expires_at":"2027-01-04T12:00:00Z","revoked_at":null,"all_chats":false,"allow_first_contact":false,"read_chats":[{"id":"120363000000000001@g.us","kind":"group","known":true,"name":"Synthetic Group"},{"id":"15550100001@s.whatsapp.net","kind":"phone","known":false,"name":null}],"write_chats":[{"id":"120363000000000001@g.us","kind":"group","known":true,"name":"Synthetic Group"}]}`
	clientOutput = "id: aaaqeaye\nname: agent\nstate: active\ncreated: 2026-10-06T12:00:00Z\nexpires: 2027-01-04T12:00:00Z\nrevoked: never\nall chats: false\nallow first contact: false\n" +
		"read chat: 120363000000000001@g.us (group, seen) Synthetic Group\nread chat: 15550100001@s.whatsapp.net (phone, not yet seen)\n" +
		"write chat: 120363000000000001@g.us (group, seen) Synthetic Group\n"
)

func TestClientsCreatePrintsTheTokenOnce(t *testing.T) {
	secret := token.NewAdmin()
	_, credential := token.NewClient()
	f, addr := newRoutedListener(t, map[string][2]string{
		"GET /admin/v1/chats?match=" + groupRef: {"200", `{"chats":[{"id":"120363000000000001@g.us","kind":"group","ref":"` + groupRef + `","name":"Synthetic Group"}],"truncated":false}`},
		"POST /admin/v1/clients":                {"200", `{"client":` + clientJSON + `,"credential":"` + credential + `"}`},
	})
	code, stdout, stderr := invokeWith(t, []string{
		"admin", "clients", "create", "--addr", addr, "--token-stdin", "--name", "agent",
		"--read", groupRef, "--read", "+15550100001", "--read", "15550100002:3@c.us", "--write", groupRef, "--expires-days", "30",
	}, nil, strings.NewReader(secret))
	if code != 0 || stdout != clientOutput+"token: "+credential+"\n" {
		t.Fatalf("admin clients create = %d\n%s\nstderr %q", code, stdout, stderr)
	}
	if n := strings.Count(stdout+stderr, credential); n != 1 || strings.Contains(stderr, credential[12:55]) {
		t.Fatalf("the credential appears %d times across stdout and stderr, want once on stdout", n)
	}
	if !strings.Contains(stderr, "it is not shown again") || !strings.Contains(stderr, "has not seen the read chat 15550100001@s.whatsapp.net") {
		t.Fatalf("stderr %q, want the reminder and the warning about a chat not yet seen", stderr)
	}
	requests, bodies := f.seen()
	if strings.Join(requests, ",") != "GET /admin/v1/chats?match="+groupRef+",GET /admin/v1/chats?match="+groupRef+",POST /admin/v1/clients" {
		t.Fatalf("requests %v", requests)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[2]), &sent); err != nil {
		t.Fatalf("the create body %q is not JSON", bodies[2])
	}
	want := `{"all_chats":false,"allow_first_contact":false,"expires_in_days":30,"name":"agent",` +
		`"read_chats":["120363000000000001@g.us","15550100001@s.whatsapp.net","15550100002@s.whatsapp.net"],"write_chats":["120363000000000001@g.us"]}`
	if got, _ := json.Marshal(sent); string(got) != want {
		t.Fatalf("the create body %s, want %s", got, want)
	}
}

func TestClientsCreateRefusesBeforeSending(t *testing.T) {
	secret := token.NewAdmin()
	f, addr := newRoutedListener(t, nil)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"--read", "15550100001@s.whatsapp.net"}, want: "--name needs a value"},
		{args: []string{"--name", "a", "--read", "15550100001@s.whatsapp.net", "--write", "not a chat"}, want: "--write value 1 is not a chat identifier"},
		{args: []string{"--name", "a", "--read", "15550100001@s.whatsapp.net", "--read", "+0123"}, want: "--read value 2 is not a chat identifier"},
		{args: []string{"--name", "a", "--read", "0123456789ABCDEF0123456789ABCDEF"}, want: "--read value 1 is not a chat identifier"},
		{args: []string{"--name", "a", "extra"}, want: "takes no arguments"},
		{args: []string{"--name", "a", "--expires-days", "x"}, want: "--expires-days is not given as it must be"},
	} {
		code, stdout, stderr := invokeWith(t, append([]string{"admin", "clients", "create", "--addr", addr, "--token-stdin"}, tt.args...), nil, strings.NewReader(secret))
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, tt.want) || !strings.Contains(stderr, "usage:") {
			t.Fatalf("create %q = %d %q %q, want 2 with %q", tt.args, code, stdout, stderr, tt.want)
		}
		if strings.Contains(stderr, "not a chat\n") || strings.Contains(stderr, "+0123") {
			t.Fatalf("stderr %q repeats what was typed", stderr)
		}
	}
	if requests, _ := f.seen(); len(requests) != 0 {
		t.Fatalf("requests %v were sent for refused commands", requests)
	}
}

func TestClientsCommandsAndExitCodes(t *testing.T) {
	secret := token.NewAdmin()
	revoked := strings.Replace(strings.Replace(clientJSON, `"state":"active"`, `"state":"revoked"`, 1), `"revoked_at":null`, `"revoked_at":"2026-10-07T08:00:00Z"`, 1)
	f, addr := newRoutedListener(t, map[string][2]string{
		"GET /admin/v1/clients": {"200", `{"clients":[{"id":"aaaqeaye","name":"agent\u001b[2J","state":"active","created_at":"2026-10-06T12:00:00Z","expires_at":"2027-01-04T12:00:00Z",` +
			`"revoked_at":null,"all_chats":true,"allow_first_contact":false,"read_chat_count":0,"write_chat_count":0},` +
			`{"id":"bbbbbbbb","name":"other","state":"expired","created_at":"2026-10-06T12:00:00Z","expires_at":"2026-10-07T12:00:00Z",` +
			`"revoked_at":null,"all_chats":false,"allow_first_contact":true,"read_chat_count":2,"write_chat_count":1}]}`},
		"GET /admin/v1/clients/aaaqeaye":         {"200", clientJSON},
		"POST /admin/v1/clients/aaaqeaye/revoke": {"200", revoked},
		"POST /admin/v1/clients":                 {"422", `{"error":"write_not_readable"}`},
		"GET /admin/v1/chats?match=Synthetic":    {"200", `{"chats":[{"id":"120363000000000001@g.us","kind":"group","ref":"` + groupRef + `","name":"Synthetic\u202eGroup"},{"id":"15550100001@lid","kind":"lid","ref":"` + strings.Repeat("c", 32) + `","name":null}],"truncated":true}`},
	})
	tests := []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{args: []string{"clients", "list"}, stdout: "id: aaaqeaye\nname: agent�[2J\nstate: active\ncreated: 2026-10-06T12:00:00Z\nexpires: 2027-01-04T12:00:00Z\nrevoked: never\nall chats: true\nallow first contact: false\nread chats: 0\nwrite chats: 0\n\n" +
			"id: bbbbbbbb\nname: other\nstate: expired\ncreated: 2026-10-06T12:00:00Z\nexpires: 2026-10-07T12:00:00Z\nrevoked: never\nall chats: false\nallow first contact: true\nread chats: 2\nwrite chats: 1\n"},
		{args: []string{"clients", "show", "--id", "aaaqeaye"}, stdout: clientOutput},
		{args: []string{"clients", "revoke", "--id", "aaaqeaye"}, stdout: strings.Replace(strings.Replace(clientOutput, "state: active", "state: revoked", 1), "revoked: never", "revoked: 2026-10-07T08:00:00Z", 1)},
		{args: []string{"clients", "show", "--id", "zzzzzzzz"}, code: exitRefused, stderr: "admin clients show: refused with not_found (HTTP 404)"},
		{args: []string{"clients", "create", "--name", "a", "--read", "15550100001@s.whatsapp.net", "--write", "15550100002@s.whatsapp.net"}, code: exitRefused, stderr: "admin clients create: refused with write_not_readable (HTTP 422)"},
		{args: []string{"chats", "list", "--match", "Synthetic"}, stdout: "id: 120363000000000001@g.us\nkind: group\nref: " + groupRef + "\nname: Synthetic�Group\n\n" +
			"id: 15550100001@lid\nkind: lid\nref: " + strings.Repeat("c", 32) + "\nname: none\n", stderr: "more chats match than are listed"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr := invokeWith(t, append(append([]string{"admin"}, tt.args...), "--addr", addr, "--token-stdin"), nil, strings.NewReader(secret))
			if code != tt.code || stdout != tt.stdout || !strings.Contains(stderr, tt.stderr) || strings.Contains(stderr, secret) {
				t.Fatalf("admin %q = %d\n%s\nstderr %q, want %d\n%s", tt.args, code, stdout, stderr, tt.code, tt.stdout)
			}
		})
	}
	requests, bodies := f.seen()
	for i, r := range requests {
		if r == "POST /admin/v1/clients/aaaqeaye/revoke" && bodies[i] != "{}" {
			t.Fatalf("revoke sent %q, want {}", bodies[i])
		}
	}
	for _, args := range [][]string{
		{"admin", "clients"},
		{"admin", "clients", "rename"},
		{"admin", "clients", "show", "--token-stdin"},
		{"admin", "clients", "show", "--id", "../status", "--token-stdin"},
		{"admin", "clients", "revoke", "--id", "AAAQEAYE", "--token-stdin"},
		{"admin", "chats"},
		{"admin", "chats", "show", "--token-stdin"},
		{"admin", "chats", "list", "--token-stdin", "extra"},
	} {
		if code, stdout, stderr := invokeWith(t, append(args, "--addr", addr), nil, strings.NewReader(secret)); code != exitUsage || stdout != "" || !strings.Contains(stderr, "usage:") {
			t.Fatalf("run(%q) = %d %q %q, want a usage error", args, code, stdout, stderr)
		}
	}
}
