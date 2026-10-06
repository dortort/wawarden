package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dortort/wawarden/internal/app"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/config"
	"github.com/dortort/wawarden/internal/keys"
	"github.com/dortort/wawarden/internal/logx"
	"github.com/dortort/wawarden/internal/token"
)

const usage = `usage:
  wawarden [serve] [--allow-root]  run the gateway (the default)
  wawarden healthcheck             exit 0 only when the local /healthz answers 200
  wawarden version                 print the version and build flavour
  wawarden admin init              generate an admin token and its SHA-256
  wawarden admin status|pair|reconnect ADMIN
  wawarden admin clients create --name NAME (--read CHAT... | --all-chats) [--write CHAT...]
                                [--allow-first-contact] [--expires-days DAYS] ADMIN
  wawarden admin clients list ADMIN
  wawarden admin clients show|revoke --id ID ADMIN
  wawarden admin chats list [--match TEXT] ADMIN
                                   call the admin listener; ADMIN is [--addr URL] and one of
                                   --token-file PATH, --token-stdin or --token-command COMMAND
                                   (default --addr http://127.0.0.1:8082)
`

const healthcheckTimeout = 2 * time.Second

var (
	runApp = (*app.App).Run
	uids   func() (ruid, euid int)
)

func run(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return serve(ctx, args, environ, stdout, stderr)
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], environ, stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, args[1:], environ, stderr)
	case "version":
		return version(args[1:], stdout, stderr)
	case "admin":
		return admin(ctx, args[1:], environ, stdin, stdout, stderr)
	}
	return usageError(stderr)
}

func usageError(stderr io.Writer) int {
	_, _ = io.WriteString(stderr, usage)
	return exitUsage
}

func serve(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	allowRoot := flags.Bool("allow-root", false, "permit running as UID 0")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		return usageError(stderr)
	}

	out := logx.NewWriter(stdout)
	cfg, refusal := config.Load(environ, config.Options{AllowRoot: *allowRoot, UIDs: uids})
	if refusal != nil {
		return refused(out, refusal.Reason, refusal)
	}
	master, keysRefusal := keys.Load(cfg.DataDir, cfg.UID)
	if keysRefusal != nil {
		return refused(out, keysRefusal.Reason, keysRefusal)
	}
	out.SetKey(master.LogRedactKey())
	logger := logx.New(out, cfg.LogLevel)
	logger.Info("keys loaded", slog.String("event", "keys_loaded"), slog.String("key_id", master.ID()))
	a, err := app.New(ctx, cfg, out, master)
	if r, ok := errors.AsType[*app.Refusal](err); ok {
		return refused(out, r.Reason, r)
	}
	if err != nil {
		logger.Error("startup failed", slog.String("event", "startup_failed"), slog.String("error", err.Error()))
		return 1
	}
	if err := runApp(a, ctx); err != nil {
		return 1
	}
	return 0
}

func refused(out *logx.Writer, reason string, err error) int {
	logx.New(out, slog.LevelInfo).Error("startup refused",
		slog.String("event", "startup_refused"), slog.String("reason", reason), slog.String("error", err.Error()))
	return 2
}

func healthcheck(ctx context.Context, args, environ []string, stderr io.Writer) int {
	if len(args) > 0 {
		_ = usageError(stderr)
		return 1
	}
	addr, refusal := config.HealthListen(environ)
	if refusal != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck:", refusal)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr.String()+"/healthz", nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintln(stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func version(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr)
	}
	info := buildinfo.Read()
	revision := info.Revision
	if revision == "" {
		revision = "unknown"
	}
	if info.Modified {
		revision += " (modified)"
	}
	if _, err := fmt.Fprintf(stdout, "wawarden %s\nrevision %s\ndev build %t\n", info.Version, revision, info.Dev); err != nil {
		return 1
	}
	return 0
}

func admin(ctx context.Context, args, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr)
	}
	switch args[0] {
	case "init":
		if len(args) != 1 {
			return usageError(stderr)
		}
		return adminInit(stdout, stderr)
	case commandStatus, commandPair, commandReconnect:
		return adminCall(ctx, args[0], args[1:], environ, stdin, stdout, stderr)
	case "clients":
		return adminClients(ctx, args[1:], environ, stdin, stdout, stderr)
	case "chats":
		return adminChats(ctx, args[1:], environ, stdin, stdout, stderr)
	}
	return usageError(stderr)
}

func adminInit(stdout, stderr io.Writer) int {
	secret := token.NewAdmin()
	if _, err := fmt.Fprintf(stdout, "token: %s\nsha256: %s\n", secret, token.Hash(secret)); err != nil {
		return 1
	}
	_, _ = fmt.Fprintln(stderr, "Store the token in a secret manager now, it is not shown again; configure only its SHA-256, in WAWARDEN_ADMIN_TOKEN_SHA256 or WAWARDEN_ADMIN_TOKEN_SHA256_FILE.")
	return 0
}
