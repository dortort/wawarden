package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

type output struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	events chan map[string]any
}

func newOutput() *output { return &output{events: make(chan map[string]any, 64)} }

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var rec map[string]any
	if json.Unmarshal(p, &rec) == nil {
		select {
		case o.events <- rec:
		default:
		}
	}
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) waitFor(t *testing.T, event string) map[string]any {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case rec := <-o.events:
			if rec["event"] == event {
				return rec
			}
		case <-deadline:
			t.Fatalf("no %s event in %q", event, o.String())
		}
	}
}

func invoke(t *testing.T, args, environ []string) (int, string, string) {
	t.Helper()
	stdout, stderr := newOutput(), newOutput()
	code := run(t.Context(), args, environ, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := invoke(t, []string{"version"}, nil)
	want := "wawarden dev\nrevision "
	if code != 0 || !strings.HasPrefix(stdout, want) || stderr != "" {
		t.Fatalf("version = %d %q %q, want 0 and output starting %q", code, stdout, stderr, want)
	}
	if flavour := "\ndev build " + strconv.FormatBool(buildinfo.Dev) + "\n"; !strings.HasSuffix(stdout, flavour) {
		t.Fatalf("version output %q does not end with %q", stdout, flavour)
	}
}

func TestAdminInit(t *testing.T) {
	code, stdout, stderr := invoke(t, []string{"admin", "init"}, nil)
	if code != 0 {
		t.Fatalf("admin init = %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want a token line and a hash line", stdout)
	}
	secret, okToken := strings.CutPrefix(lines[0], "token: ")
	hash, okHash := strings.CutPrefix(lines[1], "sha256: ")
	if !okToken || !okHash {
		t.Fatalf("stdout = %q", stdout)
	}
	if hash != token.Hash(secret) {
		t.Fatalf("printed hash %s, want the token's SHA-256 %s", hash, token.Hash(secret))
	}
	cred, err := policy.ParseAdminCredential(hash)
	if err != nil {
		t.Fatalf("the printed hash does not parse as an admin credential: %v", err)
	}
	if _, ok := policy.DecideAdmin(cred, secret); !ok {
		t.Fatalf("the printed token %q fails the syntax and checksum check against its own hash", secret)
	}
	if n := strings.Count(stdout+stderr, secret); n != 1 {
		t.Fatalf("the token is printed %d times, want once", n)
	}
	if strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "secret manager") || !strings.Contains(stderr, "WAWARDEN_ADMIN_TOKEN_SHA256") {
		t.Fatalf("stderr = %q, want a one-line reminder", stderr)
	}
	if _, again, _ := invoke(t, []string{"admin", "init"}, nil); strings.Contains(again, secret) {
		t.Fatal("two runs printed the same token")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"},
		{"admin"},
		{"admin", "bogus"},
		{"admin", "init", "extra"},
		{"admin", "status"},
		{"version", "extra"},
		{"healthcheck", "extra"},
		{"serve", "extra"},
		{"serve", "--bogus"},
		{"--bogus"},
		{""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := invoke(t, args, nil)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
				t.Fatalf("run(%q) = %d %q %q, want 2 with usage on stderr", args, code, stdout, stderr)
			}
		})
	}
}

func TestServeRefusal(t *testing.T) {
	secret := token.NewAdmin()
	tests := []struct {
		name    string
		args    []string
		environ []string
		reason  string
	}{
		{name: "unknown variable", args: []string{"serve"}, environ: []string{"WAWARDEN_LISTN=127.0.0.1:8080"}, reason: "unknown_variable"},
		{name: "plaintext admin token, serve by default", environ: []string{"WAWARDEN_ADMIN_TOKEN=" + secret}, reason: "plaintext_admin_token"},
		{name: "host name", args: []string{"--allow-root"}, environ: []string{"WAWARDEN_LISTEN=localhost:8080"}, reason: "listen_address_invalid"},
		{name: "invalid hash", args: []string{"serve"}, environ: []string{"WAWARDEN_ADMIN_TOKEN_SHA256=" + secret}, reason: "admin_hash_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, tt.args, tt.environ)
			if code != 2 {
				t.Fatalf("serve = %d, want 2; stdout %q stderr %q", code, stdout, stderr)
			}
			if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout+stderr, secret) {
				t.Fatalf("stdout = %q, want exactly one line that does not echo values", stdout)
			}
			var rec struct {
				Level  string `json:"level"`
				Event  string `json:"event"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
				t.Fatalf("not JSON: %v: %q", err, stdout)
			}
			if rec.Level != "ERROR" || rec.Event != "startup_refused" || rec.Reason != tt.reason {
				t.Fatalf("log = %+v, want an ERROR startup_refused line with reason %q", rec, tt.reason)
			}
		})
	}
}

func TestServeRefusesRootUnlessAllowed(t *testing.T) {
	saved := uids
	t.Cleanup(func() { uids = saved })
	uids = func() (int, int) { return 0, 0 }
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	environ := []string{"WAWARDEN_DATA_DIR=" + notADirectory}
	tests := []struct {
		args   []string
		reason string
	}{
		{args: nil, reason: "running_as_root"},
		{args: []string{"serve"}, reason: "running_as_root"},
		{args: []string{"--allow-root"}, reason: "data_dir_not_directory"},
		{args: []string{"serve", "--allow-root"}, reason: "data_dir_not_directory"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(append([]string{"run"}, tt.args...), " "), func(t *testing.T) {
			code, stdout, stderr := invoke(t, tt.args, environ)
			var rec struct {
				Event  string `json:"event"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stdout), &rec); err != nil || code != 2 || rec.Event != "startup_refused" || rec.Reason != tt.reason {
				t.Fatalf("run(%q) as UID 0 = %d %q %q, want 2 with a startup_refused line for %s", tt.args, code, stdout, stderr, tt.reason)
			}
		})
	}
}

func TestServeFailsWhenItCannotBind(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	environ := []string{
		"WAWARDEN_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"WAWARDEN_LISTEN=" + busy.Addr().String(),
		"WAWARDEN_HEALTH_LISTEN=" + freeAddr(t),
	}
	code, stdout, _ := invoke(t, []string{"serve", "--allow-root"}, environ)
	if code != 1 || !strings.Contains(stdout, `"event":"startup_failed"`) {
		t.Fatalf("serve = %d %q, want 1 with a startup_failed event", code, stdout)
	}
}

func TestServeExitsOneOnARuntimeFailure(t *testing.T) {
	failure := errors.New("synthetic runtime failure")
	saved := runApp
	t.Cleanup(func() { runApp = saved })
	runApp = func(a *app.App, ctx context.Context) error {
		stopped, stop := context.WithCancel(ctx)
		stop()
		return errors.Join(saved(a, stopped), failure)
	}
	environ := []string{
		"WAWARDEN_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"WAWARDEN_LISTEN=" + freeAddr(t),
		"WAWARDEN_HEALTH_LISTEN=" + freeAddr(t),
	}
	code, stdout, _ := invoke(t, []string{"serve", "--allow-root"}, environ)
	if code != 1 || !strings.Contains(stdout, `"event":"stopped"`) {
		t.Fatalf("serve = %d %q, want 1 after the app stopped with a failure", code, stdout)
	}
}

func TestServeAndHealthcheck(t *testing.T) {
	health := freeAddr(t)
	environ := []string{
		"HOME=/home/synthetic",
		"WAWARDEN_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"WAWARDEN_LISTEN=" + freeAddr(t),
		"WAWARDEN_HEALTH_LISTEN=" + health,
	}
	healthEnv := []string{"WAWARDEN_HEALTH_LISTEN=" + health, "WAWARDEN_UNKNOWN=ignored"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout, stderr := newOutput(), newOutput()
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"serve", "--allow-root"}, environ, stdout, stderr) }()
	stdout.waitFor(t, "ready")

	if code, _, stderrText := invoke(t, []string{"healthcheck"}, healthEnv); code != 0 {
		t.Fatalf("healthcheck against the running app = %d (%q), want 0", code, stderrText)
	}
	cancel()
	if code := <-exited; code != 0 {
		t.Fatalf("serve after its context was cancelled = %d, want 0; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.waitFor(t, "stopped")
	if code, _, stderrText := invoke(t, []string{"healthcheck"}, healthEnv); code != 1 || stderrText == "" {
		t.Fatalf("healthcheck with nothing listening = %d (%q), want 1", code, stderrText)
	}
}

func serveStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestHealthcheckFailures(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirect.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := redirect.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/elsewhere" {
		t.Fatalf("a redirect-following client got %d from %s, so the redirect case proves nothing", resp.StatusCode, resp.Request.URL.Path)
	}

	tests := []struct {
		name    string
		environ []string
		reason  string
	}{
		{name: "503", environ: []string{"WAWARDEN_HEALTH_LISTEN=" + serveStatus(t, http.StatusServiceUnavailable)}, reason: "status 503"},
		{name: "204", environ: []string{"WAWARDEN_HEALTH_LISTEN=" + serveStatus(t, http.StatusNoContent)}, reason: "status 204"},
		{name: "redirect to a 200", environ: []string{"WAWARDEN_HEALTH_LISTEN=" + redirect.Listener.Addr().String()}, reason: "status 302"},
		{name: "nothing listening", environ: []string{"WAWARDEN_HEALTH_LISTEN=" + freeAddr(t)}, reason: "connection refused"},
		{name: "not loopback", environ: []string{"WAWARDEN_HEALTH_LISTEN=0.0.0.0:8081"}, reason: "must be a loopback address"},
		{name: "host name", environ: []string{"WAWARDEN_HEALTH_LISTEN=localhost:8081"}, reason: "must be an IP literal"},
		{name: "zoned", environ: []string{"WAWARDEN_HEALTH_LISTEN=[::1%lo0]:8081"}, reason: "without a zone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, _, stderr := invoke(t, []string{"healthcheck"}, tt.environ); code != 1 || !strings.Contains(stderr, tt.reason) {
				t.Fatalf("healthcheck = %d (%q), want 1 with %q on stderr", code, stderr, tt.reason)
			}
		})
	}
}

func TestHealthcheckGivesUpAfterTwoSeconds(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stalled:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stalled) })

	type result struct {
		code    int
		stderr  string
		elapsed time.Duration
	}
	finished := make(chan result, 1)
	began := time.Now()
	go func() {
		code, _, stderr := invoke(t, []string{"healthcheck"}, []string{"WAWARDEN_HEALTH_LISTEN=" + srv.Listener.Addr().String()})
		finished <- result{code, stderr, time.Since(began)}
	}()
	select {
	case r := <-finished:
		if r.code != 1 || !strings.Contains(r.stderr, "deadline exceeded") {
			t.Fatalf("healthcheck against a stalled server = %d (%q), want 1 after its deadline", r.code, r.stderr)
		}
		if r.elapsed < 2*time.Second {
			t.Fatalf("healthcheck gave up after %v, want its 2 s timeout", r.elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("healthcheck is still waiting after 5 s, want it to give up after its 2 s timeout")
	}
}
