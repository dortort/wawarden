// Package config turns an injected environment into a validated configuration or a startup refusal.
package config

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	envDataDir        = "WAWARDEN_DATA_DIR"
	envListen         = "WAWARDEN_LISTEN"
	envAdminListen    = "WAWARDEN_ADMIN_LISTEN"
	envHealthListen   = "WAWARDEN_HEALTH_LISTEN"
	envAdminHash      = "WAWARDEN_ADMIN_TOKEN_SHA256"
	envAdminHashFile  = "WAWARDEN_ADMIN_TOKEN_SHA256_FILE"
	envLogLevel       = "WAWARDEN_LOG_LEVEL"
	envPlaintextAdmin = "WAWARDEN_ADMIN_TOKEN"

	prefix    = "WAWARDEN_"
	devPrefix = "WAWARDEN_DEV_"

	defaultDataDir      = "/data"
	defaultListen       = "127.0.0.1:8080"
	defaultAdminListen  = "127.0.0.1:8082"
	defaultHealthListen = "127.0.0.1:8081"
	defaultLogLevel     = "info"

	maxHashFileBytes = 4096
)

const (
	reasonUnknownVariable          = "unknown_variable"
	reasonPlaintextAdminToken      = "plaintext_admin_token"
	reasonDevVariableInRelease     = "dev_variable_in_release"
	reasonListenAddressInvalid     = "listen_address_invalid"
	reasonHealthAddressNotLoopback = "health_address_not_loopback"
	reasonListenAddressShared      = "listen_address_shared"
	reasonAdminHashInvalid         = "admin_hash_invalid"
	reasonAdminHashFileUnreadable  = "admin_hash_file_unreadable"
	reasonAdminHashSourcesConflict = "admin_hash_sources_conflict"
	reasonLogLevelInvalid          = "log_level_invalid"
	reasonRunningAsRoot            = "running_as_root"
	reasonDataDirUnusable          = "data_dir_unusable"
	reasonDataDirNotDirectory      = "data_dir_not_directory"
	reasonDataDirForeignOwner      = "data_dir_foreign_owner"
	reasonDataDirPermissions       = "data_dir_permissions"
)

var known = []string{envDataDir, envListen, envAdminListen, envHealthListen, envAdminHash, envAdminHashFile, envLogLevel}

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

type Config struct {
	DataDir         string
	Listen          netip.AddrPort
	AdminListen     netip.AddrPort
	HealthListen    netip.AddrPort
	AdminCredential *policy.AdminCredential
	LogLevel        slog.Level
}

type Options struct {
	AllowRoot bool
	UIDs      func() (ruid, euid int)
	FileOwner func(fs.FileInfo) (uid int, ok bool)
}

type Refusal struct {
	Reason   string
	Variable string
	detail   string
}

func (r *Refusal) Error() string {
	if r.Variable == "" {
		return "config: " + r.detail
	}
	return "config: " + r.Variable + ": " + r.detail
}

func Load(environ []string, opts Options) (Config, *Refusal) {
	env := variables(environ)
	if r := checkNames(env); r != nil {
		return Config{}, r
	}
	var cfg Config
	var r *Refusal
	if cfg.Listen, r = listenAddress(env, envListen, defaultListen); r != nil {
		return Config{}, r
	}
	if cfg.AdminListen, r = listenAddress(env, envAdminListen, defaultAdminListen); r != nil {
		return Config{}, r
	}
	if cfg.HealthListen, r = healthAddress(env); r != nil {
		return Config{}, r
	}
	if cfg.AdminCredential, r = adminCredential(env); r != nil {
		return Config{}, r
	}
	if r = checkShared(cfg); r != nil {
		return Config{}, r
	}
	if cfg.LogLevel, r = logLevel(env); r != nil {
		return Config{}, r
	}
	uids := processUIDs
	if opts.UIDs != nil {
		uids = opts.UIDs
	}
	ruid, euid := uids()
	if (ruid == 0 || euid == 0) && !opts.AllowRoot {
		return Config{}, &Refusal{Reason: reasonRunningAsRoot, detail: "running with a real or effective UID of 0 needs the serve flag --allow-root"}
	}
	owner := statOwner
	if opts.FileOwner != nil {
		owner = opts.FileOwner
	}
	if cfg.DataDir, r = dataDirectory(env, euid, owner); r != nil {
		return Config{}, r
	}
	return cfg, nil
}

func HealthListen(environ []string) (netip.AddrPort, *Refusal) {
	return healthAddress(variables(environ))
}

func variables(environ []string) map[string]string {
	env := make(map[string]string)
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, seen := env[name]; !seen {
			env[name] = value
		}
	}
	return env
}

func lookup(env map[string]string, name, fallback string) string {
	if v, ok := env[name]; ok {
		return v
	}
	return fallback
}

func checkNames(env map[string]string) *Refusal {
	if _, ok := env[envPlaintextAdmin]; ok {
		return &Refusal{Reason: reasonPlaintextAdminToken, Variable: envPlaintextAdmin, detail: "a plaintext admin token is refused; configure its SHA-256 in " + envAdminHash + " or " + envAdminHashFile}
	}
	names := slices.Sorted(maps.Keys(env))
	if !buildinfo.Dev {
		for _, name := range names {
			if strings.HasPrefix(name, devPrefix) {
				return &Refusal{Reason: reasonDevVariableInRelease, Variable: name, detail: "development variables are refused by a release build"}
			}
		}
	}
	for _, name := range names {
		if !slices.Contains(known, name) {
			return &Refusal{Reason: reasonUnknownVariable, Variable: name, detail: "unknown variable"}
		}
	}
	return nil
}

func listenAddress(env map[string]string, name, fallback string) (netip.AddrPort, *Refusal) {
	ap, err := netip.ParseAddrPort(lookup(env, name, fallback))
	if err != nil || ap.Port() == 0 || ap.Addr().Zone() != "" {
		return netip.AddrPort{}, &Refusal{Reason: reasonListenAddressInvalid, Variable: name, detail: "must be an IP literal without a zone and a port from 1 to 65535"}
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

func healthAddress(env map[string]string) (netip.AddrPort, *Refusal) {
	ap, r := listenAddress(env, envHealthListen, defaultHealthListen)
	if r != nil {
		return netip.AddrPort{}, r
	}
	if !ap.Addr().IsLoopback() {
		return netip.AddrPort{}, &Refusal{Reason: reasonHealthAddressNotLoopback, Variable: envHealthListen, detail: "must be a loopback address"}
	}
	return ap, nil
}

func adminCredential(env map[string]string) (*policy.AdminCredential, *Refusal) {
	value, inline := env[envAdminHash]
	path, fromFile := env[envAdminHashFile]
	variable := envAdminHash
	switch {
	case inline && fromFile:
		return nil, &Refusal{Reason: reasonAdminHashSourcesConflict, Variable: envAdminHashFile, detail: "set only one of " + envAdminHash + " and " + envAdminHashFile}
	case fromFile:
		var r *Refusal
		if value, r = readHashFile(path); r != nil {
			return nil, r
		}
		variable = envAdminHashFile
	case !inline:
		return nil, nil
	}
	cred, err := policy.ParseAdminCredential(value)
	if err != nil {
		return nil, invalidHash(variable)
	}
	return &cred, nil
}

func invalidHash(variable string) *Refusal {
	return &Refusal{Reason: reasonAdminHashInvalid, Variable: variable, detail: "must be a SHA-256 written as 64 hexadecimal characters"}
}

func readHashFile(path string) (string, *Refusal) {
	unreadable := func(why string) *Refusal {
		return &Refusal{Reason: reasonAdminHashFileUnreadable, Variable: envAdminHashFile, detail: "cannot be read: " + why}
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", unreadable(cause(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", unreadable(cause(err))
	}
	if !fi.Mode().IsRegular() {
		return "", unreadable("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxHashFileBytes+1))
	if err != nil {
		return "", unreadable(cause(err))
	}
	if len(b) > maxHashFileBytes {
		return "", invalidHash(envAdminHashFile)
	}
	return strings.TrimSpace(string(b)), nil
}

func checkShared(cfg Config) *Refusal {
	type listener struct {
		variable string
		addr     netip.AddrPort
	}
	enabled := []listener{{envListen, cfg.Listen}, {envHealthListen, cfg.HealthListen}}
	if cfg.AdminCredential != nil {
		enabled = append(enabled, listener{envAdminListen, cfg.AdminListen})
	}
	for i, a := range enabled {
		for _, b := range enabled[:i] {
			if overlap(a.addr, b.addr) {
				return &Refusal{Reason: reasonListenAddressShared, Variable: a.variable, detail: "shares its address with " + b.variable}
			}
		}
	}
	return nil
}

func overlap(a, b netip.AddrPort) bool {
	if a.Port() != b.Port() || a.Addr().Is4() != b.Addr().Is4() {
		return false
	}
	return a.Addr() == b.Addr() || a.Addr().IsUnspecified() || b.Addr().IsUnspecified()
}

func logLevel(env map[string]string) (slog.Level, *Refusal) {
	level, ok := logLevels[lookup(env, envLogLevel, defaultLogLevel)]
	if !ok {
		return 0, &Refusal{Reason: reasonLogLevelInvalid, Variable: envLogLevel, detail: "must be one of debug, info, warn, error"}
	}
	return level, nil
}

func dataDirectory(env map[string]string, uid int, owner func(fs.FileInfo) (int, bool)) (string, *Refusal) {
	path := lookup(env, envDataDir, defaultDataDir)
	if path != "" {
		path = filepath.Clean(path)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.Mkdir(path, 0o700); err == nil {
			fi, err = os.Lstat(path)
		}
	}
	if err != nil {
		return "", &Refusal{Reason: reasonDataDirUnusable, Variable: envDataDir, detail: "cannot be created or inspected: " + cause(err)}
	}
	if !fi.IsDir() {
		return "", &Refusal{Reason: reasonDataDirNotDirectory, Variable: envDataDir, detail: "is not a directory"}
	}
	if got, ok := owner(fi); !ok || got != uid {
		return "", &Refusal{Reason: reasonDataDirForeignOwner, Variable: envDataDir, detail: "is not owned by the current user"}
	}
	if fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 {
		return "", &Refusal{Reason: reasonDataDirPermissions, Variable: envDataDir, detail: "must have mode 0700: full access for its owner, none for group or others, and no setuid, setgid or sticky bit"}
	}
	return path, nil
}

func processUIDs() (ruid, euid int) {
	return os.Getuid(), os.Geteuid()
}

func statOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func cause(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error()
	}
	return "unexpected error"
}
