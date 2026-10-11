package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dortort/wawarden/internal/token"
)

func TestNoFailingCommandPrintsAToken(t *testing.T) {
	admin := token.NewAdmin()
	_, client := token.NewClient()
	secrets := []string{admin[len("wwadm_"):], client[len("ww_")+9:]}
	_, unauthorized := newFakeListener(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	_, failing := newFakeListener(t, http.StatusInternalServerError, `{"error":"`+client+`"}`)
	_, garbled := newFakeListener(t, http.StatusOK, `{"state":"`+admin+`"`)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"admin", "status", "--addr", unauthorized, "--token-stdin"},
		{"admin", "status", "--addr", failing, "--token-stdin"},
		{"admin", "status", "--addr", garbled, "--token-stdin"},
		{"admin", "pair", "--addr", "http://127.0.0.1:1", "--token-stdin"},
		{"admin", "clients", "show", "--id", client, "--addr", unauthorized, "--token-stdin"},
		{"admin", "clients", "create", "--name", client, "--all-chats", "--addr", failing, "--token-stdin"},
		{"admin", "chats", "list", "--match", admin, "--addr", failing, "--token-stdin"},
		{"admin", "status", "--addr", unauthorized, "--token-file", filepath.Join(dir, admin)},
		{"audit", "verify", "--db", filepath.Join(dir, client), "--master-key-file", filepath.Join(dir, admin)},
		{"audit", "verify", "--db", client, "--master-key-file", admin, "--log", client},
		{"keys", "rotate", "--data-dir", filepath.Join(dir, admin)},
		{"keys", "rotate", "--" + admin},
		{"backup", "verify", "--work-dir", filepath.Join(dir, client), "-"},
		{client, admin},
	} {
		code, stdout, stderr := invokeWith(t, args, nil, strings.NewReader(admin))
		if code == 0 {
			t.Fatalf("run(%v) succeeded", len(args))
		}
		for _, secret := range secrets {
			if strings.Contains(stdout+stderr, secret) {
				t.Fatalf("a failing %s %s printed a token: %q %q", args[0], args[1], stdout, stderr)
			}
		}
	}
}
