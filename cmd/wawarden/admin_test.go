package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/token"
)

type fakeListener struct {
	mu       sync.Mutex
	requests []string
	auth     []string
	bodies   []string
	types    []string
	status   int
	body     string
}

func newFakeListener(t *testing.T, status int, body string) (*fakeListener, string) {
	t.Helper()
	f := &fakeListener{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, string(b))
		f.types = append(f.types, r.Header.Get("Content-Type"))
		status, body := f.status, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeListener) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func invokeWith(t *testing.T, args, environ []string, stdin io.Reader) (int, string, string) {
	t.Helper()
	stdout, stderr := newOutput(), newOutput()
	code := run(t.Context(), args, environ, stdin, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

const statusBody = `{"state":"connected","reason":"","paired":true,"counts":{"chats":3,"messages":120,"history_blobs_pending":1,` +
	`"history_blobs_quarantined":0,"inbox_backlog":2,"inbox_quarantined":0},"clients":{"active":2,"expired":1,"revoked":0,"all_chats_active":0},` +
	`"warnings":[],"last_ingest_at":"2026-10-05T08:00:00Z","version":"v0.2.0"}`

func TestAdminStatusPrintsTheAnswer(t *testing.T) {
	secret := token.NewAdmin()
	f, addr := newFakeListener(t, http.StatusOK, statusBody)
	code, stdout, stderr := invokeWith(t, []string{"admin", "status", "--addr", addr, "--token-file", tokenFile(t, secret+"\n")}, nil, nil)
	want := "state: connected\nreason: none\npaired: true\nchats: 3\nmessages: 120\nhistory blobs pending: 1\nhistory blobs quarantined: 0\n" +
		"inbox backlog: 2\ninbox quarantined: 0\nclients active: 2\nclients expired: 1\nclients revoked: 0\nall-chats clients active: 0\n" +
		"last ingest: 2026-10-05T08:00:00Z\nversion: v0.2.0\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("admin status = %d %q %q, want 0 %q", code, stdout, stderr, want)
	}
	_, warned := newFakeListener(t, http.StatusOK, strings.Replace(strings.Replace(statusBody, `"all_chats_active":0`, `"all_chats_active":1`, 1), `"warnings":[]`, `"warnings":["all_chats_client"]`, 1))
	code, stdout, stderr = invokeWith(t, []string{"admin", "status", "--addr", warned, "--token-file", tokenFile(t, secret)}, nil, nil)
	if code != 0 || !strings.Contains(stdout, "all-chats clients active: 1\n") || stderr != "admin status: warning: all_chats_client\n" {
		t.Fatalf("admin status with an all-chats client = %d %q %q", code, stdout, stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.requests, ",") != "GET /admin/v1/status" || f.auth[0] != "Bearer "+secret || f.bodies[0] != "" {
		t.Fatalf("requests %v with %v and bodies %q", f.requests, f.auth, f.bodies)
	}
}

func TestAdminMutationsPostAnEmptyObject(t *testing.T) {
	secret := token.NewAdmin()
	for _, tt := range []struct {
		command, body, stdout string
	}{
		{command: "pair", body: `{"code":"ABCD-1234"}`, stdout: "pairing code: ABCD-1234\n"},
		{command: "reconnect", body: `{"status":"accepted"}`, stdout: "reconnect requested\n"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			f, addr := newFakeListener(t, http.StatusOK, tt.body)
			code, stdout, _ := invokeWith(t, []string{"admin", tt.command, "--addr", addr + "/", "--token-stdin"}, nil, strings.NewReader(secret))
			if code != 0 || stdout != tt.stdout {
				t.Fatalf("admin %s = %d %q, want 0 %q", tt.command, code, stdout, tt.stdout)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if strings.Join(f.requests, ",") != "POST /admin/v1/"+tt.command || f.bodies[0] != "{}" || f.types[0] != "application/json" {
				t.Fatalf("requests %v, bodies %q, content types %q", f.requests, f.bodies, f.types)
			}
		})
	}
}

func TestAdminOutputFromTheServerIsSanitised(t *testing.T) {
	secret := token.NewAdmin()
	hostile := `\u001b]52;c;aGk=\u0007\u001b[2J\u202esynthetic`
	for _, tt := range []struct {
		command, status, body string
		code                  int
	}{
		{command: "status", body: strings.Replace(statusBody, `"v0.2.0"`, `"`+hostile+`"`, 1), code: http.StatusOK},
		{command: "pair", body: `{"code":"` + hostile + `"}`, code: http.StatusOK},
		{command: "pair", body: `{"error":"` + hostile + `"}`, code: http.StatusConflict},
	} {
		_, addr := newFakeListener(t, tt.code, tt.body)
		_, stdout, stderr := invokeWith(t, []string{"admin", tt.command, "--addr", addr, "--token-stdin"}, nil, strings.NewReader(secret))
		if strings.ContainsAny(stdout+stderr, "\x1b\x07\u202e") || !strings.Contains(stdout+stderr, "synthetic") {
			t.Fatalf("%s printed %q %q: control characters from the server reached the terminal", tt.command, stdout, stderr)
		}
	}
}

func TestAdminExitCodes(t *testing.T) {
	secret := token.NewAdmin()
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed to %s", r.URL)
	}))
	t.Cleanup(redirected.Close)
	redirecting := httptest.NewServer(http.RedirectHandler(redirected.URL+"/admin/v1/status", http.StatusTemporaryRedirect))
	t.Cleanup(redirecting.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	tests := []struct {
		name   string
		addr   func(t *testing.T) string
		code   int
		stderr string
	}{
		{name: "unauthorized", addr: answering(http.StatusUnauthorized, `{"error":"unauthorized"}`), code: exitDenied, stderr: "refused with unauthorized (HTTP 401)"},
		{name: "throttled", addr: answering(http.StatusTooManyRequests, `{"error":"too_many_requests"}`), code: exitDenied, stderr: "too_many_requests"},
		{name: "already paired", addr: answering(http.StatusConflict, `{"error":"already_paired"}`), code: exitRefused, stderr: "already_paired"},
		{name: "rate limited", addr: answering(http.StatusTooManyRequests, `{"error":"rate_limited"}`), code: exitRefused, stderr: "rate_limited"},
		{name: "engine unavailable", addr: answering(http.StatusServiceUnavailable, `{"error":"engine_unavailable"}`), code: exitFailed, stderr: "engine_unavailable"},
		{name: "internal error", addr: answering(http.StatusInternalServerError, `not json`), code: exitFailed, stderr: "refused with unknown (HTTP 500)"},
		{name: "unexpected answer", addr: answering(http.StatusOK, `{"something":"else"}`), code: exitFailed, stderr: "not what the admin listener sends"},
		{name: "answer over the cap", addr: answering(http.StatusOK, `{"code":"`+strings.Repeat("a", maxAnswerBytes)+`"}`), code: exitFailed, stderr: "larger than 65536 bytes"},
		{name: "redirect", addr: func(*testing.T) string { return redirecting.URL }, code: exitFailed, stderr: "HTTP 307"},
		{name: "nothing listening", addr: func(*testing.T) string { return closed.URL }, code: exitFailed, stderr: "cannot connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := invokeWith(t, []string{"admin", "pair", "--addr", tt.addr(t), "--token-stdin"}, nil, strings.NewReader(secret))
			if code != tt.code || stdout != "" || !strings.Contains(stderr, tt.stderr) || strings.Contains(stderr, secret) {
				t.Fatalf("admin pair = %d %q %q, want %d with %q on stderr", code, stdout, stderr, tt.code, tt.stderr)
			}
		})
	}
}

func TestExitCodesMatchTheDocumentedTable(t *testing.T) {
	for name, code := range map[string][2]int{
		"exitFailed":     {exitFailed, 1},
		"exitUsage":      {exitUsage, 2},
		"exitToken":      {exitToken, 3},
		"exitDenied":     {exitDenied, 4},
		"exitRefused":    {exitRefused, 5},
		"exitUnverified": {exitUnverified, 6},
	} {
		if code[0] != code[1] {
			t.Errorf("%s = %d, the exit codes table in docs/configuration.md says %d", name, code[0], code[1])
		}
	}
}

func answering(status int, body string) func(t *testing.T) string {
	return func(t *testing.T) string {
		_, addr := newFakeListener(t, status, body)
		return addr
	}
}

func TestAdminUsageErrors(t *testing.T) {
	secret := token.NewAdmin()
	file := tokenFile(t, secret)
	_, addr := newFakeListener(t, http.StatusOK, statusBody)
	for _, args := range [][]string{
		{"admin", "status"},
		{"admin", "status", "--token-file", file, "--token-stdin"},
		{"admin", "status", "--token-file", file, "--token-command", "true"},
		{"admin", "status", "--token-stdin=false"},
		{"admin", "status", "--token", secret},
		{"admin", "status", "--token-file", file, secret},
		{"admin", "status", "--token-file", file, "--addr", "ftp://127.0.0.1:8082"},
		{"admin", "status", "--token-file", file, "--addr", (&url.URL{Scheme: "http", User: url.UserPassword("synthetic", "password"), Host: "127.0.0.1:8082"}).String()},
		{"admin", "status", "--token-file", file, "--addr", "http://127.0.0.1:8082/?token=x"},
		{"admin", "status", "--token-file", file, "--addr", "http://127.0.0.1:8082/?"},
		{"admin", "status", "--token-file", file, "--addr", "http://127.0.0.1:8082/#x"},
		{"admin", "status", "--token-file", file, "--addr", "127.0.0.1:8082"},
		{"admin", "status", "--token-file", file, "--addr", "http:///admin"},
		{"admin", "unpair", "--token-file", file, "--addr", addr},
		{"admin", "status", "--token-stdin=" + secret},
		{"admin", "status", "-=" + secret},
		{"admin", "status", "--" + secret},
		{"admin", "status", "--addr", addr, "--token-file", file, "--" + secret + "=x"},
		{"admin", "status", "--addr", addr, "--token-file", file, secret},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			code, stdout, stderr := invokeWith(t, args, nil, strings.NewReader(secret))
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, "usage:") {
				t.Fatalf("run(%q) = %d %q %q, want 2 with usage on stderr", args, code, stdout, stderr)
			}
			if strings.Contains(stderr, "password") || strings.Contains(stderr, "token=x") || strings.Contains(stderr, secret) || strings.Contains(stderr, "wwadm_") {
				t.Fatalf("stderr %q repeats an argument", stderr)
			}
		})
	}
}

func TestAdminFlagErrorsNameOnlyTheFlag(t *testing.T) {
	secret := token.NewAdmin()
	for _, tt := range []struct{ arg, want string }{
		{"--token-stdin=" + secret, "admin: --token-stdin is not given as it must be\n"},
		{"--token-file", "admin: --token-file is not given as it must be\n"},
		{"--addr", "admin: --addr is not given as it must be\n"},
		{"--" + secret, "admin: an argument is not a flag of this command\n"},
		{"-=" + secret, "admin: an argument is not a flag of this command\n"},
		{"--token-file=", "admin: --token-file needs a value\n"},
		{"--token-command=", "admin: --token-command needs a value\n"},
		{"--help", ""},
		{"-h", ""},
	} {
		code, stdout, stderr := invokeWith(t, []string{"admin", "status", tt.arg}, nil, strings.NewReader(secret))
		if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, tt.want+"usage:") || strings.Contains(stderr, secret) {
			t.Errorf("admin status %q = %d %q %q, want 2 with %q and the usage", tt.arg, code, stdout, stderr, tt.want)
		}
	}
}

func TestAdminReadsNoCredentialFromTheEnvironment(t *testing.T) {
	secret := token.NewAdmin()
	f, addr := newFakeListener(t, http.StatusOK, statusBody)
	environ := []string{"WAWARDEN_ADMIN_TOKEN=" + secret, "WAWARDEN_TOKEN=" + secret, "WAWARDEN_ADDR=" + addr}
	code, stdout, stderr := invokeWith(t, []string{"admin", "status"}, environ, strings.NewReader(secret))
	if code != exitUsage || stdout != "" || strings.Contains(stderr, secret) || len(f.seen()) != 0 {
		t.Fatalf("admin status with the token only in the environment = %d %q %q, requests %v: want a usage error and no request", code, stdout, stderr, f.seen())
	}
}

func TestAdminWarnsAboutPlainHTTPBeyondLoopback(t *testing.T) {
	secret := token.NewAdmin()
	file := tokenFile(t, secret)
	const warning = "plain HTTP"
	for addr, warns := range map[string]bool{
		"http://admin.example.test:1":  true,
		"http://admin.example:1":       true,
		"https://admin.example.test:1": false,
		"http://127.0.0.1:1":           false,
		"http://[::1]:1":               false,
		"http://localhost:1":           false,
	} {
		t.Run(addr, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			stdout, stderr := newOutput(), newOutput()
			code := run(ctx, []string{"admin", "status", "--addr", addr, "--token-file", file}, nil, nil, stdout, stderr)
			if code != exitFailed || strings.Contains(stderr.String(), warning) != warns {
				t.Fatalf("admin status --addr %s = %d %q, want 1 and a warning %v", addr, code, stderr, warns)
			}
		})
	}
}

func TestAdminTokenProblemsStopBeforeAnyRequest(t *testing.T) {
	const canary = "CANARY-not-a-token"
	f, addr := newFakeListener(t, http.StatusOK, statusBody)
	dir := t.TempDir()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	tests := []struct {
		name   string
		args   []string
		stdin  io.Reader
		stderr string
	}{
		{name: "missing file", args: []string{"--token-file", filepath.Join(dir, "missing")}, stderr: "cannot be read"},
		{name: "directory", args: []string{"--token-file", dir}, stderr: "not a regular file"},
		{name: "not a token", args: []string{"--token-file", tokenFile(t, canary)}, stderr: "not an admin token"},
		{name: "two tokens", args: []string{"--token-file", tokenFile(t, token.NewAdmin()+"\n"+token.NewAdmin())}, stderr: "not an admin token"},
		{name: "oversized file", args: []string{"--token-file", tokenFile(t, strings.Repeat("a", maxTokenSourceBytes+1))}, stderr: "more than 4096 bytes"},
		{name: "stdin not a token", args: []string{"--token-stdin"}, stdin: strings.NewReader(canary), stderr: "not an admin token"},
		{name: "stdin a terminal or device", args: []string{"--token-stdin"}, stdin: devNull, stderr: "is a terminal"},
		{name: "command printing something else", args: []string{"--token-command", "printf %s " + canary}, stderr: "not an admin token"},
		{name: "command failing", args: []string{"--token-command", "sh -c 'echo " + canary + "; exit 3'"}, stderr: "exit status 3"},
		{name: "command printing too much", args: []string{"--token-command", "head -c 10000 /dev/zero"}, stderr: "more than 4096 bytes"},
		{name: "command missing", args: []string{"--token-command", filepath.Join(dir, "missing")}, stderr: "could not be started"},
		{name: "command with an open quote", args: []string{"--token-command", "printf '%s"}, stderr: "unterminated quote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"admin", "status", "--addr", addr}, tt.args...)
			code, stdout, stderr := invokeWith(t, args, nil, tt.stdin)
			if code != exitToken || stdout != "" || !strings.Contains(stderr, tt.stderr) || strings.Contains(stderr, "CANARY") {
				t.Fatalf("admin status %q = %d %q %q, want 3 with %q and no echo", tt.args, code, stdout, stderr, tt.stderr)
			}
		})
	}
	if seen := f.seen(); len(seen) != 0 {
		t.Fatalf("requests %v were sent without a valid token", seen)
	}
}

func TestTokenFilesAreFollowedThroughLinks(t *testing.T) {
	secret := token.NewAdmin()
	target := tokenFile(t, secret+"\r\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := tokenFromFile(link)
	if err != nil || got != secret {
		t.Fatalf("tokenFromFile through a link = %q, %v", got, err)
	}
}

func TestTokenCommand(t *testing.T) {
	secret := token.NewAdmin()
	environ := []string{"PATH=" + os.Getenv("PATH"), "WAWARDEN_LISTEN=127.0.0.1:1", "WAWARDEN_ADMIN_TOKEN_SHA256=x", "HOME=/home/synthetic"}
	got, err := tokenFromCommand(t.Context(), `sh -c '[ -z "$WAWARDEN_LISTEN$WAWARDEN_ADMIN_TOKEN_SHA256" ] && [ "$HOME" = /home/synthetic ] && printf "%s\n" `+secret+`'`, environ)
	if err != nil || got != secret {
		t.Fatalf("tokenFromCommand = %q, %v: the command sees no WAWARDEN_ variable and the rest of the environment", got, err)
	}
	if got, err := tokenFromCommand(t.Context(), `sh -c 'read -r line; [ -z "$line" ] && printf %s `+secret+`'`, environ); err != nil || got != secret {
		t.Fatalf("tokenFromCommand reading its standard input = %q, %v: standard input must be closed", got, err)
	}
}

func TestTokenCommandIsBounded(t *testing.T) {
	saved := tokenCommandTimeout
	tokenCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { tokenCommandTimeout = saved })
	secret := token.NewAdmin()
	environ := []string{"PATH=" + os.Getenv("PATH")}

	start := time.Now()
	if _, err := tokenFromCommand(t.Context(), "sleep 10", environ); err != errCommandTimeout || time.Since(start) > 5*time.Second {
		t.Fatalf("a command that does not finish = %v after %v, want a timeout", err, time.Since(start))
	}
	tokenCommandTimeout = 10 * time.Second
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	start = time.Now()
	_, err := tokenFromCommand(t.Context(), `sh -c 'sleep 60 & echo $! > `+pidFile+`; printf %s `+secret+`'`, environ)
	if err != errCommandLingered || time.Since(start) > tokenCommandWaitDelay+2*time.Second {
		t.Fatalf("a command whose child keeps its output open = %v after %v, want refused after the wait delay", err, time.Since(start))
	}
	data, err := os.ReadFile(filepath.Clean(pidFile))
	if err != nil {
		t.Fatalf("read the background child's PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("background child PID %q: %v", data, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the token command's background child survived: its process group was not killed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		`pass show wawarden/admin`:        {"pass", "show", "wawarden/admin"},
		`op read "op://vault/item/field"`: {"op", "read", "op://vault/item/field"},
		`sh -c 'pass show x | head -1'`:   {"sh", "-c", "pass show x | head -1"},
		`a\ b  c`:                         {"a b", "c"},
		`echo "a b" 'c d' e"f"g`:          {"echo", "a b", "c d", "efg"},
		`printf %s ''`:                    {"printf", "%s", ""},
		"\tcat\t/run/secrets/admin ":      {"cat", "/run/secrets/admin"},
	} {
		got, err := splitCommand(in)
		if err != nil || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("splitCommand(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{``, `   `, `echo "x`, `echo 'x`, "echo x\\"} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("splitCommand(%q) succeeded", bad)
		}
	}
}

func TestAdminClientTakesNoProxyAndNoRedirect(t *testing.T) {
	c := newAdminClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || c.CheckRedirect == nil || c.Timeout == 0 || tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion == 0 {
		t.Fatalf("admin client %+v with transport %+v", c, tr)
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v, want the redirect answered as is", err)
	}
}
