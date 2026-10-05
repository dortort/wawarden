// Package config turns an injected environment into a validated configuration or a startup refusal.
package config

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dortort/wawarden/internal/backup"
	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/notify"
	"github.com/dortort/wawarden/internal/policy"
)

const (
	envDataDir         = "WAWARDEN_DATA_DIR"
	envListen          = "WAWARDEN_LISTEN"
	envAdminListen     = "WAWARDEN_ADMIN_LISTEN"
	envHealthListen    = "WAWARDEN_HEALTH_LISTEN"
	envAdminHash       = "WAWARDEN_ADMIN_TOKEN_SHA256"
	envAdminHashFile   = "WAWARDEN_ADMIN_TOKEN_SHA256_FILE"
	envLogLevel        = "WAWARDEN_LOG_LEVEL"
	envPlaintextAdmin  = "WAWARDEN_ADMIN_TOKEN"
	envTraceback       = "GOTRACEBACK"
	envStorageProfile  = "WAWARDEN_STORAGE_PROFILE"
	envMinFreeBytes    = "WAWARDEN_MIN_FREE_BYTES"
	envOwnerPhone      = "WAWARDEN_OWNER_PHONE"
	envHistoryMax      = "WAWARDEN_HISTORY_MAX_BYTES"
	envUnsafeDebug     = "WAWARDEN_UNSAFE_DEBUG"
	envMetricsEMF      = "WAWARDEN_METRICS_EMF"
	envNotifyURL       = "WAWARDEN_NOTIFY_URL"
	envNotifySecret    = "WAWARDEN_NOTIFY_SECRET_FILE"
	envNotifyPrivate   = "WAWARDEN_NOTIFY_ALLOW_PRIVATE"
	envBackupRecipient = "WAWARDEN_BACKUP_AGE_RECIPIENT"

	prefix    = "WAWARDEN_"
	devPrefix = "WAWARDEN_DEV_"

	defaultDataDir      = "/data"
	defaultListen       = "127.0.0.1:8080"
	defaultAdminListen  = "127.0.0.1:8082"
	defaultHealthListen = "127.0.0.1:8081"
	defaultLogLevel     = "info"
	defaultMinFreeBytes = 256 << 20

	DefaultHistoryMaxBytes = 32 << 20
	MaxHistoryMaxBytes     = 256 << 20

	MaxUnsafeDebug = 60 * time.Minute

	maxHashFileBytes = 4096

	maxNotifySecretBytes = 4096
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
	reasonTracebackLevelUnsafe     = "traceback_level_unsafe"
	reasonStorageProfileInvalid    = "storage_profile_invalid"
	reasonMinFreeBytesInvalid      = "min_free_bytes_invalid"
	reasonOwnerPhoneInvalid        = "owner_phone_invalid"
	reasonHistoryMaxBytesInvalid   = "history_max_bytes_invalid"
	reasonUnsafeDebugInvalid       = "unsafe_debug_invalid"
	reasonMetricsEMFInvalid        = "metrics_emf_invalid"
	reasonNotifyURLInvalid         = "notify_url_invalid"
	reasonNotifyPrivateInvalid     = "notify_allow_private_invalid"
	reasonNotifyURLMissing         = "notify_url_missing"
	reasonNotifySecretMissing      = "notify_secret_missing"
	reasonNotifySecretUnreadable   = "notify_secret_unreadable"
	reasonNotifySecretPermissions  = "notify_secret_permissions"
	reasonNotifySecretInvalid      = "notify_secret_invalid"
	reasonBackupRecipientInvalid   = "backup_recipient_invalid"
)

var known = []string{envDataDir, envListen, envAdminListen, envHealthListen, envAdminHash, envAdminHashFile, envLogLevel, envStorageProfile, envMinFreeBytes,
	envOwnerPhone, envHistoryMax, envUnsafeDebug, envMetricsEMF, envNotifyURL, envNotifySecret, envNotifyPrivate, envBackupRecipient}

type StorageProfile string

const (
	StorageLocal StorageProfile = "local"
	StorageNFS   StorageProfile = "nfs"
)

var dataSubdirectories = []string{"history", backup.Dir}

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

type Config struct {
	DataDir         string
	UID             int
	Listen          netip.AddrPort
	AdminListen     netip.AddrPort
	HealthListen    netip.AddrPort
	AdminCredential *policy.AdminCredential
	LogLevel        slog.Level
	StorageProfile  StorageProfile
	MinFreeBytes    uint64
	OwnerPhone      string
	HistoryMaxBytes int64
	UnsafeDebug     time.Duration
	MetricsEMF      bool
	Notify          Notify
	BackupRecipient string
	Dev             Dev
}

type Notify struct {
	URL          string
	Secret       []byte
	AllowPrivate bool
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
	if r := checkTraceback(environ); r != nil {
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
	if cfg.StorageProfile, r = storageProfile(env); r != nil {
		return Config{}, r
	}
	if cfg.MinFreeBytes, r = minFreeBytes(env); r != nil {
		return Config{}, r
	}
	if cfg.OwnerPhone, r = ownerPhone(env); r != nil {
		return Config{}, r
	}
	if cfg.HistoryMaxBytes, r = historyMaxBytes(env); r != nil {
		return Config{}, r
	}
	if cfg.UnsafeDebug, r = unsafeDebug(env); r != nil {
		return Config{}, r
	}
	if cfg.MetricsEMF, r = metricsEMF(env); r != nil {
		return Config{}, r
	}
	if cfg.Notify, r = notifySettings(env); r != nil {
		return Config{}, r
	}
	if cfg.BackupRecipient, r = backupRecipient(env); r != nil {
		return Config{}, r
	}
	if cfg.Dev, r = devSettings(env); r != nil {
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
	for _, name := range dataSubdirectories {
		if r = checkSubdirectory(cfg.DataDir, name, euid, owner); r != nil {
			return Config{}, r
		}
	}
	cfg.UID = euid
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

func checkTraceback(environ []string) *Refusal {
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if name == envTraceback && value != "" && value != "none" && value != "single" {
			return &Refusal{Reason: reasonTracebackLevelUnsafe, Variable: envTraceback, detail: "must be unset, none or single: any other level makes a crash print every goroutine"}
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

func storageProfile(env map[string]string) (StorageProfile, *Refusal) {
	switch p := StorageProfile(lookup(env, envStorageProfile, string(StorageLocal))); p {
	case StorageLocal, StorageNFS:
		return p, nil
	}
	return "", &Refusal{Reason: reasonStorageProfileInvalid, Variable: envStorageProfile, detail: "must be local or nfs"}
}

func minFreeBytes(env map[string]string) (uint64, *Refusal) {
	v, ok := env[envMinFreeBytes]
	if !ok {
		return defaultMinFreeBytes, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, &Refusal{Reason: reasonMinFreeBytesInvalid, Variable: envMinFreeBytes, detail: "must be a number of bytes written in decimal digits, at most 18446744073709551615"}
	}
	return n, nil
}

func ownerPhone(env map[string]string) (string, *Refusal) {
	v, ok := env[envOwnerPhone]
	if !ok {
		return "", nil
	}
	if _, valid := policy.OwnerDigits(v); !valid {
		return "", &Refusal{Reason: reasonOwnerPhoneInvalid, Variable: envOwnerPhone, detail: "must be a number in E.164 form: + and 7 to 15 digits, the first not 0, nothing else"}
	}
	return v, nil
}

func historyMaxBytes(env map[string]string) (int64, *Refusal) {
	v, ok := env[envHistoryMax]
	if !ok {
		return DefaultHistoryMaxBytes, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 || n > MaxHistoryMaxBytes {
		return 0, &Refusal{Reason: reasonHistoryMaxBytesInvalid, Variable: envHistoryMax, detail: "must be a number of bytes written in decimal digits, from 1 to 268435456"}
	}
	return int64(n), nil
}

func unsafeDebug(env map[string]string) (time.Duration, *Refusal) {
	v, ok := env[envUnsafeDebug]
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 || n > uint64(MaxUnsafeDebug/time.Minute) {
		return 0, &Refusal{Reason: reasonUnsafeDebugInvalid, Variable: envUnsafeDebug, detail: "must be a number of minutes written in decimal digits, from 1 to 60"}
	}
	return time.Duration(n) * time.Minute, nil
}

func metricsEMF(env map[string]string) (bool, *Refusal) {
	v, ok := env[envMetricsEMF]
	if ok && v != "0" && v != "1" {
		return false, &Refusal{Reason: reasonMetricsEMFInvalid, Variable: envMetricsEMF, detail: "must be 0 or 1"}
	}
	return v == "1", nil
}

func notifySettings(env map[string]string) (Notify, *Refusal) {
	var n Notify
	url, withURL := env[envNotifyURL]
	if withURL && notify.CheckURL(url) != nil {
		return Notify{}, &Refusal{Reason: reasonNotifyURLInvalid, Variable: envNotifyURL, detail: "must be an https URL with a host, without credentials or a fragment"}
	}
	private, withPrivate := env[envNotifyPrivate]
	if withPrivate && private != "0" && private != "1" {
		return Notify{}, &Refusal{Reason: reasonNotifyPrivateInvalid, Variable: envNotifyPrivate, detail: "must be 0 or 1"}
	}
	n.AllowPrivate = private == "1"
	path, withSecret := env[envNotifySecret]
	switch {
	case !withURL && withSecret:
		return Notify{}, &Refusal{Reason: reasonNotifyURLMissing, Variable: envNotifySecret, detail: "is set without " + envNotifyURL}
	case !withURL && withPrivate:
		return Notify{}, &Refusal{Reason: reasonNotifyURLMissing, Variable: envNotifyPrivate, detail: "is set without " + envNotifyURL}
	case !withURL:
		return Notify{}, nil
	case !withSecret:
		return Notify{}, &Refusal{Reason: reasonNotifySecretMissing, Variable: envNotifySecret, detail: "is required with " + envNotifyURL}
	}
	secret, r := readNotifySecret(path)
	if r != nil {
		return Notify{}, r
	}
	n.URL, n.Secret = url, secret
	return n, nil
}

func backupRecipient(env map[string]string) (string, *Refusal) {
	v, ok := env[envBackupRecipient]
	if ok && backup.CheckRecipient(v) != nil {
		return "", &Refusal{Reason: reasonBackupRecipientInvalid, Variable: envBackupRecipient, detail: "must be one age recipient, age1 followed by its key, without spaces or other text"}
	}
	return v, nil
}

func readNotifySecret(path string) ([]byte, *Refusal) {
	unreadable := func(why string) *Refusal {
		return &Refusal{Reason: reasonNotifySecretUnreadable, Variable: envNotifySecret, detail: "cannot be read: " + why}
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, unreadable(cause(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, unreadable(cause(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, unreadable("not a regular file")
	}
	if fi.Mode()&0o027 != 0 {
		return nil, &Refusal{Reason: reasonNotifySecretPermissions, Variable: envNotifySecret, detail: "must grant no write access to group and no access to others"}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxNotifySecretBytes+1))
	if err != nil {
		return nil, unreadable(cause(err))
	}
	secret := bytes.TrimSpace(b)
	if len(b) > maxNotifySecretBytes || len(secret) < notify.MinSecretBytes {
		return nil, &Refusal{Reason: reasonNotifySecretInvalid, Variable: envNotifySecret, detail: "must hold a secret of at least 32 bytes, apart from surrounding white space, in a file of at most 4096 bytes"}
	}
	return secret, nil
}

func checkSubdirectory(dataDir, name string, uid int, owner func(fs.FileInfo) (int, bool)) *Refusal {
	fi, err := os.Lstat(filepath.Join(dataDir, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return &Refusal{Reason: name + "_dir_unusable", detail: name + "/ cannot be inspected: " + cause(err)}
	case !fi.IsDir():
		return &Refusal{Reason: name + "_dir_not_directory", detail: name + "/ is not a directory"}
	}
	if got, ok := owner(fi); !ok || got != uid {
		return &Refusal{Reason: name + "_dir_foreign_owner", detail: name + "/ is not owned by the current user"}
	}
	if fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0o700 {
		return &Refusal{Reason: name + "_dir_permissions", detail: name + "/ must have mode 0700: full access for its owner, none for group or others, and no setuid, setgid or sticky bit"}
	}
	return nil
}

func dataDirectory(env map[string]string, uid int, owner func(fs.FileInfo) (int, bool)) (string, *Refusal) {
	path := lookup(env, envDataDir, defaultDataDir)
	if path != "" {
		path = filepath.Clean(path)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.Mkdir(path, 0o700); err == nil {
			err = os.Chmod(path, 0o700) //nolint:gosec // G302: a directory needs its search bit; this clears the setgid bit Linux copies from a setgid parent
		}
		if err == nil {
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
