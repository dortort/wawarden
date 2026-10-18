package main

import (
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

func TestAdminBackfill(t *testing.T) {
	secret := token.NewAdmin()
	chats := `{"chats":[{"id":"120363000000000001@g.us","kind":"group","ref":"` + groupRef + `","name":null}],"truncated":false}`
	for _, tt := range []struct {
		name     string
		args     []string
		answer   [2]string
		code     int
		stdout   string
		stderr   string
		requests string
		body     string
	}{
		{name: "an identifier", args: []string{"--chat", "120363000000000001@g.us"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", stderr: "admin status counts", requests: "POST /admin/v1/backfill", body: `{"chat":"120363000000000001@g.us","count":50}`},
		{name: "a reference", args: []string{"--chat", groupRef, "--count", "10"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", requests: "GET /admin/v1/chats?match=" + groupRef + ",POST /admin/v1/backfill", body: `{"chat":"120363000000000001@g.us","count":10}`},
		{name: "a phone number", args: []string{"--chat", "+15550100001", "--count", "1"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", requests: "POST /admin/v1/backfill", body: `{"chat":"15550100001@s.whatsapp.net","count":1}`},
		{name: "the quota", args: []string{"--chat", "120363000000000001@g.us"}, answer: [2]string{"429", `{"error":"backfill_quota"}`},
			code: exitRefused, stderr: "admin backfill: refused with backfill_quota (HTTP 429)", requests: "POST /admin/v1/backfill"},
		{name: "no anchor", args: []string{"--chat", "120363000000000001@g.us"}, answer: [2]string{"422", `{"error":"backfill_no_anchor"}`},
			code: exitRefused, stderr: "admin backfill: refused with backfill_no_anchor (HTTP 422)", requests: "POST /admin/v1/backfill"},
		{name: "not connected", args: []string{"--chat", "120363000000000001@g.us"}, answer: [2]string{"503", `{"error":"not_connected"}`},
			code: exitFailed, stderr: "admin backfill: refused with not_connected (HTTP 503)", requests: "POST /admin/v1/backfill"},
		{name: "an unknown reference", args: []string{"--chat", strings.Repeat("e", 32)}, code: exitRefused, stderr: "admin backfill: no chat of the archive has that reference",
			requests: "GET /admin/v1/chats?match=" + strings.Repeat("e", 32)},
		{name: "the newest anchor", args: []string{"--chat", "120363000000000001@g.us", "--anchor", "newest"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", requests: "POST /admin/v1/backfill", body: `{"chat":"120363000000000001@g.us","count":50,"anchor":"newest"}`},
		{name: "the newest anchor by reference", args: []string{"--anchor=newest", "--chat", groupRef, "--count", "3"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", requests: "GET /admin/v1/chats?match=" + groupRef + ",POST /admin/v1/backfill", body: `{"chat":"120363000000000001@g.us","count":3,"anchor":"newest"}`},
		{name: "the oldest anchor", args: []string{"--chat", "120363000000000001@g.us", "--anchor", "oldest"}, answer: [2]string{"202", `{"request_id":"3EB0REQUEST"}`},
			stdout: "backfill requested: 3EB0REQUEST\n", requests: "POST /admin/v1/backfill", body: `{"chat":"120363000000000001@g.us","count":50}`},
		{name: "an anchor a v1.2.0 server refuses", args: []string{"--chat", "120363000000000001@g.us", "--anchor", "newest"}, answer: [2]string{"400", `{"error":"invalid_body"}`},
			code: exitFailed, stderr: "admin backfill: refused with invalid_body (HTTP 400)", requests: "POST /admin/v1/backfill"},
		{name: "an unknown anchor", args: []string{"--chat", "120363000000000001@g.us", "--anchor", "latest"}, code: exitUsage, stderr: "--anchor must be oldest or newest"},
		{name: "an empty anchor", args: []string{"--chat", "120363000000000001@g.us", "--anchor", ""}, code: exitUsage, stderr: "--anchor must be oldest or newest"},
		{name: "an upper-case anchor", args: []string{"--chat", groupRef, "--anchor", "NEWEST"}, code: exitUsage, stderr: "--anchor must be oldest or newest"},
		{name: "no chat", args: []string{"--count", "5"}, code: exitUsage, stderr: "--chat needs a value"},
		{name: "no count", args: []string{"--chat", "120363000000000001@g.us", "--count", "0"}, code: exitUsage, stderr: "--count must be from 1 to 50"},
		{name: "too many", args: []string{"--chat", "120363000000000001@g.us", "--count", "51"}, code: exitUsage, stderr: "--count must be from 1 to 50"},
		{name: "a name", args: []string{"--chat", "Synthetic Group"}, code: exitUsage, stderr: "--chat is not a chat identifier"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, addr := newRoutedListener(t, map[string][2]string{
				"POST /admin/v1/backfill":                              tt.answer,
				"GET /admin/v1/chats?match=" + groupRef:                {"200", chats},
				"GET /admin/v1/chats?match=" + strings.Repeat("e", 32): {"200", `{"chats":[],"truncated":false}`},
			})
			code, stdout, stderr := invokeWith(t, append(append([]string{"admin", "backfill"}, tt.args...), "--addr", addr, "--token-stdin"), nil, strings.NewReader(secret))
			if code != tt.code || stdout != tt.stdout || !strings.Contains(stderr, tt.stderr) || strings.Contains(stderr, secret) {
				t.Fatalf("admin backfill %q = %d %q %q, want %d %q %q", tt.args, code, stdout, stderr, tt.code, tt.stdout, tt.stderr)
			}
			requests, bodies := f.seen()
			if got := strings.Join(requests, ","); got != tt.requests {
				t.Fatalf("requests %q, want %q", got, tt.requests)
			}
			if tt.body != "" && bodies[len(bodies)-1] != tt.body {
				t.Fatalf("body %q, want %q", bodies[len(bodies)-1], tt.body)
			}
		})
	}
}
