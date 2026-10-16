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
	envMCPDebug        = "MCPGODEBUG"
	envSchemaDebug     = "JSONSCHEMAGODEBUG"
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
	envBackupTime      = "WAWARDEN_BACKUP_TIME"
	envBackupKeep      = "WAWARDEN_BACKUP_KEEP"
	envReadPerMinute   = "WAWARDEN_READ_PER_CLIENT_PER_MINUTE"
	envSearchPerMinute = "WAWARDEN_SEARCH_PER_CLIENT_PER_MINUTE"
	envUnsafeRateCaps  = "WAWARDEN_UNSAFE_RATE_CAPS"
	envAuditRetention  = "WAWARDEN_AUDIT_RETENTION_DAYS"
	envSendGlobal      = "WAWARDEN_SEND_GLOBAL_PER_HOUR"
	envSendPerMinute   = "WAWARDEN_SEND_PER_CLIENT_PER_MINUTE"
	envUnsafeSendCaps  = "WAWARDEN_UNSAFE_SEND_CAPS"

	envMediaMax         = "WAWARDEN_MEDIA_MAX_BYTES"
	envMediaRetention   = "WAWARDEN_MEDIA_REF_RETENTION_DAYS"
	envMediaPerMinute   = "WAWARDEN_MEDIA_PER_CLIENT_PER_MINUTE"
	envMediaGlobal      = "WAWARDEN_MEDIA_GLOBAL_PER_HOUR"
	envMediaGlobalBytes = "WAWARDEN_MEDIA_GLOBAL_BYTES_PER_HOUR"

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

	DefaultReadPerMinute   = 600
	DefaultSearchPerMinute = 60
	MaxUnsafeRatePerMinute = 1000000

	DefaultSendGlobalPerHour   = 30
	MaxSendGlobalPerHour       = 60
	MaxUnsafeSendGlobalPerHour = 600
	DefaultSendPerMinute       = 10
	MaxSendPerMinute           = 10
	MaxUnsafeSendPerMinute     = 60

	DefaultBackupMinute = 3 * 60
	DefaultBackupKeep   = 14
	MaxBackupKeep       = 365

	DefaultAuditRetentionDays = 90
	MinAuditRetentionDays     = 7
	MaxAuditRetentionDays     = 3650

	MaxMediaMaxBytes               = 2 << 30
	DefaultMediaRefRetentionDays   = 30
	MaxMediaRefRetentionDays       = 365
	DefaultMediaPerMinute          = 6
	MaxMediaPerMinute              = 30
	DefaultMediaGlobalPerHour      = 60
	MaxMediaGlobalPerHour          = 600
	DefaultMediaGlobalBytesPerHour = 4 << 30
	MaxMediaGlobalBytesPerHour     = 64 << 30

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
	reasonLibraryDebugSet          = "library_debug_set"
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
	reasonBackupTimeInvalid        = "backup_time_invalid"
	reasonBackupKeepInvalid        = "backup_keep_invalid"
	reasonBackupOptionNoRecipient  = "backup_option_without_recipient"
	reasonUnsafeRateCapsInvalid    = "unsafe_rate_caps_invalid"
	reasonReadPerMinuteInvalid     = "read_per_minute_invalid"
	reasonSearchPerMinuteInvalid   = "search_per_minute_invalid"
	reasonAuditRetentionInvalid    = "audit_retention_invalid"
	reasonUnsafeSendCapsInvalid    = "unsafe_send_caps_invalid"
	reasonSendGlobalInvalid        = "send_global_per_hour_invalid"
	reasonSendPerMinuteInvalid     = "send_per_client_per_minute_invalid"
	reasonMediaMaxBytesInvalid     = "media_max_bytes_invalid"
	reasonMediaRetentionInvalid    = "media_ref_retention_days_invalid"
	reasonMediaPerMinuteInvalid    = "media_per_client_per_minute_invalid"
	reasonMediaGlobalInvalid       = "media_global_per_hour_invalid"
	reasonMediaGlobalBytesInvalid  = "media_global_bytes_per_hour_invalid"
)

var known = []string{envDataDir, envListen, envAdminListen, envHealthListen, envAdminHash, envAdminHashFile, envLogLevel, envStorageProfile, envMinFreeBytes,
	envOwnerPhone, envHistoryMax, envUnsafeDebug, envMetricsEMF, envNotifyURL, envNotifySecret, envNotifyPrivate, envBackupRecipient,
	envReadPerMinute, envSearchPerMinute, envUnsafeRateCaps, envBackupTime, envBackupKeep, envAuditRetention, envSendGlobal, envSendPerMinute,
	envUnsafeSendCaps, envMediaMax, envMediaRetention, envMediaPerMinute, envMediaGlobal, envMediaGlobalBytes}

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
	BackupMinute    int
	BackupKeep      int
	AuditRetention  int
	ReadPerMinute   int
	SearchPerMinute int
	UnsafeRateCaps  bool
	SendGlobal      int
	SendPerMinute   int
	UnsafeSendCaps  bool
	Dev             Dev

	MediaMaxBytes           int64
	MediaRefRetentionDays   int
	MediaPerMinute          int
	MediaGlobalPerHour      int
	MediaGlobalBytesPerHour int64
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
	if r := checkLibraryDebug(environ); r != nil {
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
	if cfg.BackupMinute, cfg.BackupKeep, r = backupSchedule(env); r != nil {
		return Config{}, r
	}
	if cfg.AuditRetention, r = auditRetention(env); r != nil {
		return Config{}, r
	}
	if cfg.UnsafeRateCaps, r = unsafeRateCaps(env); r != nil {
		return Config{}, r
	}
	if cfg.ReadPerMinute, r = perMinute(env, envReadPerMinute, reasonReadPerMinuteInvalid, DefaultReadPerMinute, cfg.UnsafeRateCaps); r != nil {
		return Config{}, r
	}
	if cfg.SearchPerMinute, r = perMinute(env, envSearchPerMinute, reasonSearchPerMinuteInvalid, DefaultSearchPerMinute, cfg.UnsafeRateCaps); r != nil {
		return Config{}, r
	}
	if cfg.UnsafeSendCaps, r = unsafeSendCaps(env); r != nil {
		return Config{}, r
	}
	if cfg.SendGlobal, r = sendLimit(env, envSendGlobal, reasonSendGlobalInvalid, "hour", DefaultSendGlobalPerHour, sendCap(MaxSendGlobalPerHour, MaxUnsafeSendGlobalPerHour, cfg.UnsafeSendCaps)); r != nil {
		return Config{}, r
	}
	if cfg.SendPerMinute, r = sendLimit(env, envSendPerMinute, reasonSendPerMinuteInvalid, "minute", DefaultSendPerMinute, sendCap(MaxSendPerMinute, MaxUnsafeSendPerMinute, cfg.UnsafeSendCaps)); r != nil {
		return Config{}, r
	}
	if cfg.MediaMaxBytes, r = mediaMaxBytes(env); r != nil {
		return Config{}, r
	}
	if cfg.MediaRefRetentionDays, r = mediaRefRetention(env); r != nil {
		return Config{}, r
	}
	if cfg.MediaPerMinute, r = mediaPerMinute(env); r != nil {
		return Config{}, r
	}
	if cfg.MediaGlobalPerHour, r = mediaGlobalPerHour(env); r != nil {
		return Config{}, r
	}
	if cfg.MediaGlobalBytesPerHour, r = mediaGlobalBytesPerHour(env, cfg.MediaMaxBytes); r != nil {
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

func checkLibraryDebug(environ []string) *Refusal {
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); name == envMCPDebug || name == envSchemaDebug {
			return &Refusal{Reason: reasonLibraryDebugSet, Variable: name, detail: "must be unset: it switches the MCP library's security and schema behaviour"}
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

func unsafeRateCaps(env map[string]string) (bool, *Refusal) {
	v, ok := env[envUnsafeRateCaps]
	if ok && v != "0" && v != "1" {
		return false, &Refusal{Reason: reasonUnsafeRateCapsInvalid, Variable: envUnsafeRateCaps, detail: "must be 0 or 1"}
	}
	return v == "1", nil
}

func perMinute(env map[string]string, name, reason string, limit int, unsafe bool) (int, *Refusal) {
	v, ok := env[name]
	if !ok {
		return limit, nil
	}
	if unsafe {
		limit = MaxUnsafeRatePerMinute
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v || n < 1 || n > limit {
		return 0, &Refusal{Reason: reason, Variable: name, detail: "must be a number of requests per minute written in decimal digits, from 1 to " + strconv.Itoa(limit)}
	}
	return n, nil
}

func unsafeSendCaps(env map[string]string) (bool, *Refusal) {
	v, ok := env[envUnsafeSendCaps]
	if ok && v != "0" && v != "1" {
		return false, &Refusal{Reason: reasonUnsafeSendCapsInvalid, Variable: envUnsafeSendCaps, detail: "must be 0 or 1"}
	}
	return v == "1", nil
}

func sendCap(limit, unsafeLimit int, unsafe bool) int {
	if unsafe {
		return unsafeLimit
	}
	return limit
}

func sendLimit(env map[string]string, name, reason, unit string, def, limit int) (int, *Refusal) {
	v, ok := env[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v || n < 1 || n > limit {
		return 0, &Refusal{Reason: reason, Variable: name, detail: "must be a number of sends per " + unit + " written in decimal digits, from 1 to " + strconv.Itoa(limit)}
	}
	return n, nil
}

func decimal(v string) (int64, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0 && strconv.FormatInt(n, 10) == v
}

func mediaMaxBytes(env map[string]string) (int64, *Refusal) {
	v, ok := env[envMediaMax]
	if !ok {
		return 0, nil
	}
	n, valid := decimal(v)
	if !valid || n > MaxMediaMaxBytes {
		return 0, &Refusal{Reason: reasonMediaMaxBytesInvalid, Variable: envMediaMax,
			detail: "must be 0, which turns attachment downloads off, or a number of bytes written in decimal digits, from 1 to " + strconv.FormatInt(MaxMediaMaxBytes, 10)}
	}
	return n, nil
}

func mediaRefRetention(env map[string]string) (int, *Refusal) {
	v, ok := env[envMediaRetention]
	if !ok {
		return DefaultMediaRefRetentionDays, nil
	}
	n, valid := decimal(v)
	if !valid || n < 1 || n > MaxMediaRefRetentionDays {
		return 0, &Refusal{Reason: reasonMediaRetentionInvalid, Variable: envMediaRetention,
			detail: "must be a number of days written in decimal digits, from 1 to " + strconv.Itoa(MaxMediaRefRetentionDays)}
	}
	return int(n), nil
}

func mediaPerMinute(env map[string]string) (int, *Refusal) {
	v, ok := env[envMediaPerMinute]
	if !ok {
		return DefaultMediaPerMinute, nil
	}
	n, valid := decimal(v)
	if !valid || n < 1 || n > MaxMediaPerMinute {
		return 0, &Refusal{Reason: reasonMediaPerMinuteInvalid, Variable: envMediaPerMinute,
			detail: "must be a number of attachment fetches per minute written in decimal digits, from 1 to " + strconv.Itoa(MaxMediaPerMinute)}
	}
	return int(n), nil
}

func mediaGlobalPerHour(env map[string]string) (int, *Refusal) {
	v, ok := env[envMediaGlobal]
	if !ok {
		return DefaultMediaGlobalPerHour, nil
	}
	n, valid := decimal(v)
	if !valid || n < 1 || n > MaxMediaGlobalPerHour {
		return 0, &Refusal{Reason: reasonMediaGlobalInvalid, Variable: envMediaGlobal,
			detail: "must be a number of attachment fetches per hour written in decimal digits, from 1 to " + strconv.Itoa(MaxMediaGlobalPerHour)}
	}
	return int(n), nil
}

func mediaGlobalBytesPerHour(env map[string]string, maxBytes int64) (int64, *Refusal) {
	v, ok := env[envMediaGlobalBytes]
	if !ok {
		return DefaultMediaGlobalBytesPerHour, nil
	}
	n, valid := decimal(v)
	if !valid || n < 1 || n < maxBytes || n > MaxMediaGlobalBytesPerHour {
		return 0, &Refusal{Reason: reasonMediaGlobalBytesInvalid, Variable: envMediaGlobalBytes,
			detail: "must be a number of bytes written in decimal digits, from 1 to " + strconv.FormatInt(MaxMediaGlobalBytesPerHour, 10) + " and not below " + envMediaMax}
	}
	return n, nil
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

func backupSchedule(env map[string]string) (minute, keep int, r *Refusal) {
	minute, keep = DefaultBackupMinute, DefaultBackupKeep
	if at, ok := env[envBackupTime]; ok {
		if minute = minuteOfDay(at); minute < 0 {
			return 0, 0, &Refusal{Reason: reasonBackupTimeInvalid, Variable: envBackupTime, detail: "must be a time of day in UTC written HH:MM, from 00:00 to 23:59"}
		}
	}
	if count, ok := env[envBackupKeep]; ok {
		n, err := strconv.Atoi(count)
		if err != nil || strconv.Itoa(n) != count || n < 1 || n > MaxBackupKeep {
			return 0, 0, &Refusal{Reason: reasonBackupKeepInvalid, Variable: envBackupKeep, detail: "must be a number of backups written in decimal digits, from 1 to " + strconv.Itoa(MaxBackupKeep)}
		}
		keep = n
	}
	if _, ok := env[envBackupRecipient]; ok {
		return minute, keep, nil
	}
	for _, name := range []string{envBackupTime, envBackupKeep} {
		if _, ok := env[name]; ok {
			return 0, 0, &Refusal{Reason: reasonBackupOptionNoRecipient, Variable: name, detail: "is set without " + envBackupRecipient}
		}
	}
	return minute, keep, nil
}

func auditRetention(env map[string]string) (int, *Refusal) {
	value, ok := env[envAuditRetention]
	if !ok {
		return DefaultAuditRetentionDays, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(n) != value || n < MinAuditRetentionDays || n > MaxAuditRetentionDays {
		return 0, &Refusal{Reason: reasonAuditRetentionInvalid, Variable: envAuditRetention,
			detail: "must be a number of days written in decimal digits, from " + strconv.Itoa(MinAuditRetentionDays) + " to " + strconv.Itoa(MaxAuditRetentionDays)}
	}
	return n, nil
}

func minuteOfDay(v string) int {
	if len(v) != 5 || v[2] != ':' {
		return -1
	}
	for _, i := range []int{0, 1, 3, 4} {
		if v[i] < '0' || v[i] > '9' {
			return -1
		}
	}
	h := int(v[0]-'0')*10 + int(v[1]-'0')
	m := int(v[3]-'0')*10 + int(v[4]-'0')
	if h > 23 || m > 59 {
		return -1
	}
	return h*60 + m
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
