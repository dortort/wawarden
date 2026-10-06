package config

import (
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/dortort/wawarden/internal/buildinfo"
	"github.com/dortort/wawarden/internal/policy"
	"github.com/dortort/wawarden/internal/token"
)

const (
	testUID   = 4242
	otherUID  = 4343
	validHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func owned(uid int) func(fs.FileInfo) (int, bool) {
	return func(fs.FileInfo) (int, bool) { return uid, true }
}

func testOptions() Options {
	return Options{UIDs: uids(testUID, testUID), FileOwner: owned(testUID)}
}

func uids(ruid, euid int) func() (int, int) {
	return func() (int, int) { return ruid, euid }
}

func environ(vars map[string]string) []string {
	out := []string{"HOME=/home/synthetic", "PATH=/usr/bin", "wawarden_listen=lowercase-is-another-variable"}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		out = append(out, k+"="+vars[k])
	}
	return out
}

func withDataDir(t *testing.T, vars map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{envDataDir: filepath.Join(t.TempDir(), "data")}
	maps.Copy(out, vars)
	return out
}

const syntheticSecret = "synthetic-webhook-secret-of-forty-two-bytes"

var syntheticAgeSecret, syntheticAgeRecipient = func() (string, string) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		panic(err)
	}
	return id.String(), id.Recipient().String()
}()

func secretFile(t *testing.T, mode fs.FileMode) string {
	t.Helper()
	path := writeFile(t, syntheticSecret+"\n")
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	return path
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hash")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func linkTo(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	return link
}

func dirWithMode(t *testing.T, mode fs.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if got := mustLstat(t, path).Mode() &^ fs.ModeDir; got != mode {
		t.Fatalf("the directory has mode %v after Chmod, want %v", got, mode)
	}
	return path
}

func TestDefaults(t *testing.T) {
	if defaultDataDir != "/data" || defaultMinFreeBytes != 268435456 || DefaultHistoryMaxBytes != 33554432 || MaxHistoryMaxBytes != 268435456 {
		t.Fatalf("default data directory = %q, free-space floor = %d, history cap = %d up to %d, want /data, 268435456, 33554432 and 268435456",
			defaultDataDir, defaultMinFreeBytes, DefaultHistoryMaxBytes, MaxHistoryMaxBytes)
	}
	vars := withDataDir(t, nil)
	cfg, r := Load(environ(vars), testOptions())
	if r != nil {
		t.Fatalf("Load: %v", r)
	}
	want := Config{
		DataDir:         vars[envDataDir],
		UID:             testUID,
		Listen:          netip.MustParseAddrPort("127.0.0.1:8080"),
		AdminListen:     netip.MustParseAddrPort("127.0.0.1:8082"),
		HealthListen:    netip.MustParseAddrPort("127.0.0.1:8081"),
		LogLevel:        slog.LevelInfo,
		StorageProfile:  StorageLocal,
		MinFreeBytes:    268435456,
		HistoryMaxBytes: 33554432,
		ReadPerMinute:   600,
		SearchPerMinute: 60,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load = %+v, want %+v", cfg, want)
	}
}

func TestRefusals(t *testing.T) {
	devVariableReason, firstOfDevAndUnknown := reasonDevVariableInRelease, "WAWARDEN_DEV_X"
	if buildinfo.Dev {
		devVariableReason, firstOfDevAndUnknown = reasonUnknownVariable, "WAWARDEN_AAA"
	}
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name     string
		vars     map[string]string
		opts     func(*Options)
		reason   string
		variable string
	}{
		{name: "unknown variable", vars: map[string]string{"WAWARDEN_LISTN": "127.0.0.1:8080"}, reason: "unknown_variable", variable: "WAWARDEN_LISTN"},
		{name: "unknown variable with an empty value", vars: map[string]string{"WAWARDEN_STORAGE": ""}, reason: "unknown_variable", variable: "WAWARDEN_STORAGE"},
		{name: "plaintext token file is unknown", vars: map[string]string{"WAWARDEN_ADMIN_TOKEN_FILE": "/run/secrets/token"}, reason: "unknown_variable", variable: "WAWARDEN_ADMIN_TOKEN_FILE"},
		{name: "plaintext admin token", vars: map[string]string{"WAWARDEN_ADMIN_TOKEN": "wwadm_synthetic"}, reason: "plaintext_admin_token", variable: "WAWARDEN_ADMIN_TOKEN"},
		{name: "plaintext admin token before unknown names", vars: map[string]string{"WAWARDEN_AAA": "x", "WAWARDEN_ADMIN_TOKEN": ""}, reason: "plaintext_admin_token", variable: "WAWARDEN_ADMIN_TOKEN"},
		{name: "dev variable", vars: map[string]string{"WAWARDEN_DEV_X": "1"}, reason: devVariableReason, variable: "WAWARDEN_DEV_X"},
		{name: "dev variable before unknown names", vars: map[string]string{"WAWARDEN_AAA": "x", "WAWARDEN_DEV_X": ""}, reason: devVariableReason, variable: firstOfDevAndUnknown},

		{name: "traceback all", vars: map[string]string{"GOTRACEBACK": "all"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback system", vars: map[string]string{"GOTRACEBACK": "system"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback crash", vars: map[string]string{"GOTRACEBACK": "crash"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback wer", vars: map[string]string{"GOTRACEBACK": "wer"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 0, which still prints every goroutine", vars: map[string]string{"GOTRACEBACK": "0"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 1", vars: map[string]string{"GOTRACEBACK": "1"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 2", vars: map[string]string{"GOTRACEBACK": "2"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback in another case", vars: map[string]string{"GOTRACEBACK": "Single"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "MCP library debug settings", vars: map[string]string{"MCPGODEBUG": "hintomitempty=1"}, reason: "library_debug_set", variable: "MCPGODEBUG"},
		{name: "empty MCP library debug settings", vars: map[string]string{"MCPGODEBUG": ""}, reason: "library_debug_set", variable: "MCPGODEBUG"},
		{name: "schema library debug settings", vars: map[string]string{"JSONSCHEMAGODEBUG": "typeschemasnull=1"}, reason: "library_debug_set", variable: "JSONSCHEMAGODEBUG"},
		{name: "empty schema library debug settings", vars: map[string]string{"JSONSCHEMAGODEBUG": ""}, reason: "library_debug_set", variable: "JSONSCHEMAGODEBUG"},

		{name: "host name", vars: map[string]string{envListen: "localhost:8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "dns name", vars: map[string]string{envListen: "gateway.example.invalid:8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "port 0", vars: map[string]string{envListen: "127.0.0.1:0"}, reason: "listen_address_invalid", variable: envListen},
		{name: "port above range", vars: map[string]string{envListen: "127.0.0.1:65536"}, reason: "listen_address_invalid", variable: envListen},
		{name: "negative port", vars: map[string]string{envListen: "127.0.0.1:-1"}, reason: "listen_address_invalid", variable: envListen},
		{name: "no port", vars: map[string]string{envListen: "127.0.0.1"}, reason: "listen_address_invalid", variable: envListen},
		{name: "no host", vars: map[string]string{envListen: ":8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "url", vars: map[string]string{envListen: "http://127.0.0.1:8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "unbracketed ipv6", vars: map[string]string{envListen: "::1:8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "empty", vars: map[string]string{envListen: ""}, reason: "listen_address_invalid", variable: envListen},
		{name: "zoned ipv6", vars: map[string]string{envListen: "[::1%lo0]:9000", envHealthListen: "[::1]:9000"}, reason: "listen_address_invalid", variable: envListen},
		{name: "zoned mapped ipv4", vars: map[string]string{envListen: "[::ffff:127.0.0.1%lo0]:8080"}, reason: "listen_address_invalid", variable: envListen},
		{name: "admin zoned link-local", vars: map[string]string{envAdminListen: "[fe80::1%en0]:8082"}, reason: "listen_address_invalid", variable: envAdminListen},
		{name: "health zoned", vars: map[string]string{envHealthListen: "[::1%lo0]:8081"}, reason: "listen_address_invalid", variable: envHealthListen},
		{name: "admin host name", vars: map[string]string{envAdminListen: "localhost:8082"}, reason: "listen_address_invalid", variable: envAdminListen},
		{name: "admin port 0", vars: map[string]string{envAdminListen: "127.0.0.1:0"}, reason: "listen_address_invalid", variable: envAdminListen},
		{name: "health host name", vars: map[string]string{envHealthListen: "localhost:8081"}, reason: "listen_address_invalid", variable: envHealthListen},
		{name: "health port above range", vars: map[string]string{envHealthListen: "127.0.0.1:99999"}, reason: "listen_address_invalid", variable: envHealthListen},

		{name: "health on every ipv4 address", vars: map[string]string{envHealthListen: "0.0.0.0:8081"}, reason: "health_address_not_loopback", variable: envHealthListen},
		{name: "health on every ipv6 address", vars: map[string]string{envHealthListen: "[::]:8081"}, reason: "health_address_not_loopback", variable: envHealthListen},
		{name: "health on a routable address", vars: map[string]string{envHealthListen: "192.0.2.10:8081"}, reason: "health_address_not_loopback", variable: envHealthListen},

		{name: "client on the health address", vars: map[string]string{envListen: "127.0.0.1:8081"}, reason: "listen_address_shared", variable: envHealthListen},
		{name: "client on every address of the health port", vars: map[string]string{envListen: "0.0.0.0:8081"}, reason: "listen_address_shared", variable: envHealthListen},
		{name: "health inside the client wildcard", vars: map[string]string{envListen: "0.0.0.0:9000", envHealthListen: "127.0.0.2:9000"}, reason: "listen_address_shared", variable: envHealthListen},
		{name: "admin on the client address", vars: map[string]string{envAdminHash: validHash, envAdminListen: "127.0.0.1:8080"}, reason: "listen_address_shared", variable: envAdminListen},
		{name: "admin on the health address", vars: map[string]string{envAdminHash: validHash, envAdminListen: "127.0.0.1:8081"}, reason: "listen_address_shared", variable: envAdminListen},
		{name: "admin and client on one ipv6 wildcard", vars: map[string]string{envAdminHash: validHash, envListen: "[::]:9000", envAdminListen: "[::]:9000"}, reason: "listen_address_shared", variable: envAdminListen},
		{name: "mapped ipv4 equals ipv4", vars: map[string]string{envAdminHash: validHash, envAdminListen: "[::ffff:127.0.0.1]:8080"}, reason: "listen_address_shared", variable: envAdminListen},

		{name: "short hash", vars: map[string]string{envAdminHash: validHash[:63]}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "long hash", vars: map[string]string{envAdminHash: validHash + "0"}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "non-hex hash", vars: map[string]string{envAdminHash: strings.Repeat("z", 64)}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "empty hash", vars: map[string]string{envAdminHash: ""}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "hash with whitespace", vars: map[string]string{envAdminHash: validHash + "\n"}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "token instead of hash", vars: map[string]string{envAdminHash: token.NewAdmin()}, reason: "admin_hash_invalid", variable: envAdminHash},
		{name: "bad hash in file", vars: map[string]string{envAdminHashFile: writeFile(t, "not-a-hash\n")}, reason: "admin_hash_invalid", variable: envAdminHashFile},
		{name: "empty hash file", vars: map[string]string{envAdminHashFile: writeFile(t, "")}, reason: "admin_hash_invalid", variable: envAdminHashFile},
		{name: "garbage beyond the hash file size limit", vars: map[string]string{envAdminHashFile: writeFile(t, validHash+strings.Repeat(" ", 5000)+"trailing-garbage")}, reason: "admin_hash_invalid", variable: envAdminHashFile},
		{name: "hash file one byte over the size limit", vars: map[string]string{envAdminHashFile: writeFile(t, validHash+strings.Repeat(" ", maxHashFileBytes+1-len(validHash)))}, reason: "admin_hash_invalid", variable: envAdminHashFile},
		{name: "missing hash file", vars: map[string]string{envAdminHashFile: missing}, reason: "admin_hash_file_unreadable", variable: envAdminHashFile},
		{name: "hash file is a directory", vars: map[string]string{envAdminHashFile: t.TempDir()}, reason: "admin_hash_file_unreadable", variable: envAdminHashFile},
		{name: "hash file is a device", vars: map[string]string{envAdminHashFile: os.DevNull}, reason: "admin_hash_file_unreadable", variable: envAdminHashFile},
		{name: "both hash sources", vars: map[string]string{envAdminHash: validHash, envAdminHashFile: writeFile(t, validHash)}, reason: "admin_hash_sources_conflict", variable: envAdminHashFile},

		{name: "unknown log level", vars: map[string]string{envLogLevel: "trace"}, reason: "log_level_invalid", variable: envLogLevel},
		{name: "upper-case log level", vars: map[string]string{envLogLevel: "INFO"}, reason: "log_level_invalid", variable: envLogLevel},
		{name: "empty log level", vars: map[string]string{envLogLevel: ""}, reason: "log_level_invalid", variable: envLogLevel},

		{name: "unknown storage profile", vars: map[string]string{envStorageProfile: "efs"}, reason: "storage_profile_invalid", variable: envStorageProfile},
		{name: "upper-case storage profile", vars: map[string]string{envStorageProfile: "LOCAL"}, reason: "storage_profile_invalid", variable: envStorageProfile},
		{name: "empty storage profile", vars: map[string]string{envStorageProfile: ""}, reason: "storage_profile_invalid", variable: envStorageProfile},
		{name: "storage profile with a space", vars: map[string]string{envStorageProfile: "nfs "}, reason: "storage_profile_invalid", variable: envStorageProfile},

		{name: "empty free-space floor", vars: map[string]string{envMinFreeBytes: ""}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "negative free-space floor", vars: map[string]string{envMinFreeBytes: "-1"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "signed free-space floor", vars: map[string]string{envMinFreeBytes: "+1"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "free-space floor with a unit", vars: map[string]string{envMinFreeBytes: "256MiB"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "free-space floor in exponent form", vars: map[string]string{envMinFreeBytes: "1e9"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "hexadecimal free-space floor", vars: map[string]string{envMinFreeBytes: "0x10"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "free-space floor with underscores", vars: map[string]string{envMinFreeBytes: "1_000"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "free-space floor with a space", vars: map[string]string{envMinFreeBytes: " 1"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},
		{name: "free-space floor beyond 64 bits", vars: map[string]string{envMinFreeBytes: "18446744073709551616"}, reason: "min_free_bytes_invalid", variable: envMinFreeBytes},

		{name: "empty owner phone", vars: map[string]string{envOwnerPhone: ""}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone without a plus", vars: map[string]string{envOwnerPhone: "15550100009"}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone with a leading zero", vars: map[string]string{envOwnerPhone: "+05550100009"}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone too short", vars: map[string]string{envOwnerPhone: "+155501"}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone too long", vars: map[string]string{envOwnerPhone: "+1555010000912345"}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone with spaces", vars: map[string]string{envOwnerPhone: "+1 555 010 0009"}, reason: "owner_phone_invalid", variable: envOwnerPhone},
		{name: "owner phone as an identifier", vars: map[string]string{envOwnerPhone: "15550100009@s.whatsapp.net"}, reason: "owner_phone_invalid", variable: envOwnerPhone},

		{name: "empty history cap", vars: map[string]string{envHistoryMax: ""}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "zero history cap", vars: map[string]string{envHistoryMax: "0"}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "history cap above its maximum", vars: map[string]string{envHistoryMax: "268435457"}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "signed history cap", vars: map[string]string{envHistoryMax: "+1024"}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "history cap with a unit", vars: map[string]string{envHistoryMax: "32MiB"}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "history cap beyond 64 bits", vars: map[string]string{envHistoryMax: "18446744073709551616"}, reason: "history_max_bytes_invalid", variable: envHistoryMax},
		{name: "empty debug window", vars: map[string]string{envUnsafeDebug: ""}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},
		{name: "zero debug window", vars: map[string]string{envUnsafeDebug: "0"}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},
		{name: "debug window above its maximum", vars: map[string]string{envUnsafeDebug: "61"}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},
		{name: "debug window with a unit", vars: map[string]string{envUnsafeDebug: "5m"}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},
		{name: "signed debug window", vars: map[string]string{envUnsafeDebug: "+5"}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},
		{name: "debug window as a flag", vars: map[string]string{envUnsafeDebug: "true"}, reason: "unsafe_debug_invalid", variable: envUnsafeDebug},

		{name: "metrics as a word", vars: map[string]string{envMetricsEMF: "true"}, reason: "metrics_emf_invalid", variable: envMetricsEMF},
		{name: "empty metrics switch", vars: map[string]string{envMetricsEMF: ""}, reason: "metrics_emf_invalid", variable: envMetricsEMF},
		{name: "metrics switch with a space", vars: map[string]string{envMetricsEMF: "1 "}, reason: "metrics_emf_invalid", variable: envMetricsEMF},

		{name: "rate caps as a word", vars: map[string]string{envUnsafeRateCaps: "yes"}, reason: "unsafe_rate_caps_invalid", variable: envUnsafeRateCaps},
		{name: "empty rate caps switch", vars: map[string]string{envUnsafeRateCaps: ""}, reason: "unsafe_rate_caps_invalid", variable: envUnsafeRateCaps},
		{name: "empty read rate", vars: map[string]string{envReadPerMinute: ""}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "zero read rate", vars: map[string]string{envReadPerMinute: "0"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "negative read rate", vars: map[string]string{envReadPerMinute: "-1"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "signed read rate", vars: map[string]string{envReadPerMinute: "+10"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "read rate with a leading zero", vars: map[string]string{envReadPerMinute: "060"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "read rate above its cap", vars: map[string]string{envReadPerMinute: "601"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "read rate above its cap with the caps kept", vars: map[string]string{envReadPerMinute: "601", envUnsafeRateCaps: "0"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "zero read rate with the caps lifted", vars: map[string]string{envReadPerMinute: "0", envUnsafeRateCaps: "1"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "read rate above the lifted cap", vars: map[string]string{envReadPerMinute: "1000001", envUnsafeRateCaps: "1"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "read rate beyond 64 bits", vars: map[string]string{envReadPerMinute: "18446744073709551616", envUnsafeRateCaps: "1"}, reason: "read_per_minute_invalid", variable: envReadPerMinute},
		{name: "zero search rate", vars: map[string]string{envSearchPerMinute: "0"}, reason: "search_per_minute_invalid", variable: envSearchPerMinute},
		{name: "search rate above its cap", vars: map[string]string{envSearchPerMinute: "61"}, reason: "search_per_minute_invalid", variable: envSearchPerMinute},
		{name: "search rate with a unit", vars: map[string]string{envSearchPerMinute: "60/m"}, reason: "search_per_minute_invalid", variable: envSearchPerMinute},
		{name: "negative search rate with the caps lifted", vars: map[string]string{envSearchPerMinute: "-5", envUnsafeRateCaps: "1"}, reason: "search_per_minute_invalid", variable: envSearchPerMinute},

		{name: "plain http webhook", vars: map[string]string{envNotifyURL: "http://hooks.example.test/x", envNotifySecret: secretFile(t, 0o600)}, reason: "notify_url_invalid", variable: envNotifyURL},
		{name: "webhook with credentials", vars: map[string]string{envNotifyURL: (&url.URL{Scheme: "https", User: url.UserPassword("synthetic", "synthetic"), Host: "hooks.example.test"}).String(), envNotifySecret: secretFile(t, 0o600)}, reason: "notify_url_invalid", variable: envNotifyURL},
		{name: "webhook with a fragment", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x#f", envNotifySecret: secretFile(t, 0o600)}, reason: "notify_url_invalid", variable: envNotifyURL},
		{name: "empty webhook", vars: map[string]string{envNotifyURL: ""}, reason: "notify_url_invalid", variable: envNotifyURL},
		{name: "private destinations as a word", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifyPrivate: "true"}, reason: "notify_allow_private_invalid", variable: envNotifyPrivate},
		{name: "empty private destinations", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifyPrivate: ""}, reason: "notify_allow_private_invalid", variable: envNotifyPrivate},
		{name: "secret without a webhook", vars: map[string]string{envNotifySecret: secretFile(t, 0o600)}, reason: "notify_url_missing", variable: envNotifySecret},
		{name: "private destinations without a webhook", vars: map[string]string{envNotifyPrivate: "1"}, reason: "notify_url_missing", variable: envNotifyPrivate},
		{name: "webhook without a secret", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x"}, reason: "notify_secret_missing", variable: envNotifySecret},
		{name: "missing secret file", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: missing}, reason: "notify_secret_unreadable", variable: envNotifySecret},
		{name: "secret file is a directory", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: t.TempDir()}, reason: "notify_secret_unreadable", variable: envNotifySecret},
		{name: "secret file is a device", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: os.DevNull}, reason: "notify_secret_unreadable", variable: envNotifySecret},
		{name: "world-readable secret", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: secretFile(t, 0o644)}, reason: "notify_secret_permissions", variable: envNotifySecret},
		{name: "group-writable secret", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: secretFile(t, 0o620)}, reason: "notify_secret_permissions", variable: envNotifySecret},
		{name: "short secret", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: writeFile(t, " "+strings.Repeat("s", 31)+"\n")}, reason: "notify_secret_invalid", variable: envNotifySecret},
		{name: "secret file over its size limit", vars: map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: writeFile(t, strings.Repeat("s", 4097))}, reason: "notify_secret_invalid", variable: envNotifySecret},

		{name: "empty backup recipient", vars: map[string]string{envBackupRecipient: ""}, reason: "backup_recipient_invalid", variable: envBackupRecipient},
		{name: "backup recipient that is not age", vars: map[string]string{envBackupRecipient: "ssh-ed25519 AAAASynthetic"}, reason: "backup_recipient_invalid", variable: envBackupRecipient},
		{name: "age secret key as the backup recipient", vars: map[string]string{envBackupRecipient: syntheticAgeSecret}, reason: "backup_recipient_invalid", variable: envBackupRecipient},
		{name: "two backup recipients", vars: map[string]string{envBackupRecipient: syntheticAgeRecipient + "\n" + syntheticAgeRecipient}, reason: "backup_recipient_invalid", variable: envBackupRecipient},
		{name: "backup recipient with a space", vars: map[string]string{envBackupRecipient: syntheticAgeRecipient + " "}, reason: "backup_recipient_invalid", variable: envBackupRecipient},

		{name: "uid 0 without --allow-root", opts: func(o *Options) { o.UIDs = uids(0, 0); o.FileOwner = owned(0) }, reason: "running_as_root"},
		{name: "effective uid 0 without --allow-root", opts: func(o *Options) { o.UIDs = uids(testUID, 0); o.FileOwner = owned(0) }, reason: "running_as_root"},
		{name: "real uid 0 without --allow-root", opts: func(o *Options) { o.UIDs = uids(0, testUID) }, reason: "running_as_root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions()
			if tt.opts != nil {
				tt.opts(&opts)
			}
			vars := withDataDir(t, tt.vars)
			cfg, r := Load(environ(vars), opts)
			if r == nil {
				t.Fatalf("Load accepted the configuration: %+v", cfg)
			}
			if r.Reason != tt.reason || r.Variable != tt.variable {
				t.Fatalf("refusal = %q on %q (%v), want %q on %q", r.Reason, r.Variable, r, tt.reason, tt.variable)
			}
			if !reflect.DeepEqual(cfg, Config{}) {
				t.Fatalf("a refusal came with a configuration: %+v", cfg)
			}
			if _, err := os.Lstat(vars[envDataDir]); err == nil {
				t.Fatal("a refused start created the data directory")
			}
		})
	}
}

func TestHashFileThatWouldBlockIsRefused(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	env := environ(withDataDir(t, map[string]string{envAdminHashFile: fifo}))
	done := make(chan *Refusal, 1)
	go func() {
		_, r := Load(env, testOptions())
		done <- r
	}()
	select {
	case r := <-done:
		if r == nil || r.Reason != "admin_hash_file_unreadable" || r.Variable != envAdminHashFile {
			t.Fatalf("Load = %v, want admin_hash_file_unreadable on %s", r, envAdminHashFile)
		}
	case <-time.After(5 * time.Second):
		if writer, err := os.OpenFile(filepath.Clean(fifo), os.O_WRONLY, 0); err == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatal("Load still waited for a writer on a FIFO after 5 s, want it refused at once")
	}
}

func TestDataDirectoryRefusals(t *testing.T) {
	file := writeFile(t, "")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dirWithMode(t, 0o700), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	dangling, target := filepath.Join(t.TempDir(), "dangling"), filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, dangling); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(target); err == nil {
			t.Error("a refused start created the target of a dangling symbolic link")
		}
	})
	tests := []struct {
		name   string
		path   string
		owner  int
		reason string
	}{
		{name: "empty", path: "", owner: testUID, reason: "data_dir_unusable"},
		{name: "missing parent", path: filepath.Join(t.TempDir(), "absent", "data"), owner: testUID, reason: "data_dir_unusable"},
		{name: "regular file", path: file, owner: testUID, reason: "data_dir_not_directory"},
		{name: "symbolic link", path: link, owner: testUID, reason: "data_dir_not_directory"},
		{name: "symbolic link with a trailing slash", path: link + "/", owner: testUID, reason: "data_dir_not_directory"},
		{name: "symbolic link with a trailing dot", path: link + "/.", owner: testUID, reason: "data_dir_not_directory"},
		{name: "symbolic link with a doubled slash", path: link + "//", owner: testUID, reason: "data_dir_not_directory"},
		{name: "dangling symbolic link with a trailing slash", path: dangling + "/", owner: testUID, reason: "data_dir_not_directory"},
		{name: "foreign owner", path: dirWithMode(t, 0o700), owner: otherUID, reason: "data_dir_foreign_owner"},
		{name: "root-owned", path: dirWithMode(t, 0o700), owner: 0, reason: "data_dir_foreign_owner"},
		{name: "mode 0750", path: dirWithMode(t, 0o750), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0755", path: dirWithMode(t, 0o755), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0701", path: dirWithMode(t, 0o701), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0710", path: dirWithMode(t, 0o710), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0702", path: dirWithMode(t, 0o702), owner: testUID, reason: "data_dir_permissions"},
		{name: "setuid", path: dirWithMode(t, fs.ModeSetuid|0o700), owner: testUID, reason: "data_dir_permissions"},
		{name: "setgid", path: dirWithMode(t, fs.ModeSetgid|0o700), owner: testUID, reason: "data_dir_permissions"},
		{name: "sticky", path: dirWithMode(t, fs.ModeSticky|0o700), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0500", path: dirWithMode(t, 0o500), owner: testUID, reason: "data_dir_permissions"},
		{name: "mode 0000", path: dirWithMode(t, 0), owner: testUID, reason: "data_dir_permissions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions()
			opts.FileOwner = owned(tt.owner)
			_, r := Load(environ(map[string]string{envDataDir: tt.path}), opts)
			if r == nil || r.Reason != tt.reason || r.Variable != envDataDir {
				t.Fatalf("Load = %v, want %q on %s", r, tt.reason, envDataDir)
			}
		})
	}
	t.Run("owned by the real user but not the effective one", func(t *testing.T) {
		opts := testOptions()
		opts.UIDs = uids(otherUID, testUID)
		opts.FileOwner = owned(otherUID)
		_, r := Load(environ(map[string]string{envDataDir: dirWithMode(t, 0o700)}), opts)
		if r == nil || r.Reason != "data_dir_foreign_owner" {
			t.Fatalf("Load = %v, want data_dir_foreign_owner", r)
		}
	})
	t.Run("unknown owner", func(t *testing.T) {
		opts := testOptions()
		opts.FileOwner = func(fs.FileInfo) (int, bool) { return testUID, false }
		_, r := Load(environ(map[string]string{envDataDir: dirWithMode(t, 0o700)}), opts)
		if r == nil || r.Reason != "data_dir_foreign_owner" {
			t.Fatalf("Load = %v, want data_dir_foreign_owner", r)
		}
	})
}

func TestDataSubdirectoryRefusals(t *testing.T) {
	for _, name := range []string{"history", "backups"} {
		tests := []struct {
			name   string
			setup  func(t *testing.T, path string)
			owner  func(fs.FileInfo) (int, bool)
			reason string
		}{
			{name: "a regular file", reason: "_dir_not_directory", setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}},
			{name: "a symbolic link to a private directory", reason: "_dir_not_directory", setup: func(t *testing.T, path string) {
				if err := os.Symlink(dirWithMode(t, 0o700), path); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			}},
			{name: "another owner", reason: "_dir_foreign_owner", setup: mkdirMode(0o700), owner: func(fi fs.FileInfo) (int, bool) {
				if fi.Name() == name {
					return otherUID, true
				}
				return testUID, true
			}},
			{name: "an unknown owner", reason: "_dir_foreign_owner", setup: mkdirMode(0o700), owner: func(fi fs.FileInfo) (int, bool) { return testUID, fi.Name() != name }},
			{name: "mode 0750", reason: "_dir_permissions", setup: mkdirMode(0o750)},
			{name: "mode 0705", reason: "_dir_permissions", setup: mkdirMode(0o705)},
			{name: "mode 0500", reason: "_dir_permissions", setup: mkdirMode(0o500)},
			{name: "setgid", reason: "_dir_permissions", setup: mkdirMode(fs.ModeSetgid | 0o700)},
		}
		for _, tt := range tests {
			t.Run(name+" "+tt.name, func(t *testing.T) {
				data := dirWithMode(t, 0o700)
				tt.setup(t, filepath.Join(data, name))
				opts := testOptions()
				if tt.owner != nil {
					opts.FileOwner = tt.owner
				}
				cfg, r := Load(environ(map[string]string{envDataDir: data}), opts)
				if r == nil || r.Reason != name+tt.reason || r.Variable != "" || !reflect.DeepEqual(cfg, Config{}) {
					t.Fatalf("Load = %+v, %v, want %s%s naming no variable", cfg, r, name, tt.reason)
				}
				if strings.Contains(r.Error(), data) || !strings.Contains(r.Error(), name+"/") {
					t.Fatalf("refusal %q names the data directory, or not %s/", r.Error(), name)
				}
			})
		}
	}
	t.Run("private or absent", func(t *testing.T) {
		data := dirWithMode(t, 0o700)
		mkdirMode(0o700)(t, filepath.Join(data, "history"))
		if _, r := Load(environ(map[string]string{envDataDir: data}), testOptions()); r != nil {
			t.Fatalf("Load refused a private history/ and an absent backups/: %v", r)
		}
		if _, err := os.Lstat(filepath.Join(data, "backups")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Load created backups/: %v", err)
		}
	})
}

func mkdirMode(mode fs.FileMode) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
	}
}

func TestDataDirectoryIsCreatedPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	cfg, r := Load(environ(map[string]string{envDataDir: path + "/"}), testOptions())
	if r != nil {
		t.Fatalf("Load: %v", r)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 || cfg.DataDir != path {
		t.Fatalf("data directory %q has mode %v, want a 0700 directory at %q", cfg.DataDir, fi.Mode(), path)
	}
	if _, r := Load(environ(map[string]string{envDataDir: path}), testOptions()); r != nil {
		t.Fatalf("a second start refused the directory the first created: %v", r)
	}
}

func TestDataDirectoryCreatedUnderASetgidParentIsAccepted(t *testing.T) {
	path := filepath.Join(dirWithMode(t, fs.ModeSetgid|0o700), "data")
	if _, r := Load(environ(map[string]string{envDataDir: path}), testOptions()); r != nil {
		t.Fatalf("Load refused the directory it created under a setgid parent: %v", r)
	}
	if got := mustLstat(t, path).Mode(); got != fs.ModeDir|0o700 {
		t.Fatalf("the created directory has mode %v, want exactly drwx------", got)
	}
}

func TestRealOwnershipAndUID(t *testing.T) {
	path := dirWithMode(t, 0o700)
	cfg, r := Load(environ(map[string]string{envDataDir: path}), Options{AllowRoot: true})
	if r != nil || cfg.DataDir != path || cfg.UID != os.Geteuid() {
		t.Fatalf("Load = %+v, %v; want the private directory owned by the current user accepted for the effective user ID %d", cfg, r, os.Geteuid())
	}
	if uid, ok := statOwner(mustLstat(t, path)); !ok || uid != os.Geteuid() {
		t.Fatalf("statOwner = %d, %v; want %d", uid, ok, os.Geteuid())
	}
}

func mustLstat(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	return fi
}

func TestAccepted(t *testing.T) {
	adminToken := token.NewAdmin()
	tests := []struct {
		name  string
		vars  map[string]string
		opts  func(*Options)
		check func(*testing.T, Config)
	}{
		{
			name: "uid 0 with --allow-root",
			opts: func(o *Options) { o.UIDs = uids(0, 0); o.FileOwner = owned(0); o.AllowRoot = true },
		},
		{name: "empty traceback level", vars: map[string]string{"GOTRACEBACK": ""}},
		{name: "traceback level single", vars: map[string]string{"GOTRACEBACK": "single"}},
		{name: "traceback level none", vars: map[string]string{"GOTRACEBACK": "none"}},
		{
			name: "data directory owned by the effective user",
			opts: func(o *Options) { o.UIDs = uids(otherUID, testUID) },
			check: func(t *testing.T, c Config) {
				if c.UID != testUID {
					t.Fatalf("UID = %d, want the effective user ID %d", c.UID, testUID)
				}
			},
		},
		{
			name: "admin hash inline",
			vars: map[string]string{envAdminHash: token.Hash(adminToken)},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential == nil {
					t.Fatal("the admin listener is disabled")
				}
				if _, ok := policy.DecideAdmin(*c.AdminCredential, adminToken); !ok {
					t.Fatal("the configured hash does not admit its token")
				}
			},
		},
		{
			name: "admin hash from a file with surrounding whitespace",
			vars: map[string]string{envAdminHashFile: writeFile(t, "\n  "+token.Hash(adminToken)+" \r\n")},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential == nil {
					t.Fatal("the admin listener is disabled")
				}
				if _, ok := policy.DecideAdmin(*c.AdminCredential, adminToken); !ok {
					t.Fatal("the configured hash does not admit its token")
				}
			},
		},
		{
			name: "hash file reached through a symbolic link",
			vars: map[string]string{envAdminHashFile: linkTo(t, writeFile(t, token.Hash(adminToken)))},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential == nil {
					t.Fatal("the admin listener is disabled")
				}
				if _, ok := policy.DecideAdmin(*c.AdminCredential, adminToken); !ok {
					t.Fatal("the configured hash does not admit its token")
				}
			},
		},
		{
			name: "hash file padded to the size limit",
			vars: map[string]string{envAdminHashFile: writeFile(t, token.Hash(adminToken)+strings.Repeat("\n", maxHashFileBytes-len(validHash)))},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential == nil {
					t.Fatal("the admin listener is disabled")
				}
				if _, ok := policy.DecideAdmin(*c.AdminCredential, adminToken); !ok {
					t.Fatal("the configured hash does not admit its token")
				}
			},
		},
		{
			name: "upper-case admin hash",
			vars: map[string]string{envAdminHash: strings.ToUpper(validHash)},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential == nil {
					t.Fatal("the admin listener is disabled")
				}
			},
		},
		{
			name: "admin listener disabled may repeat the client address",
			vars: map[string]string{envAdminListen: "127.0.0.1:8080"},
			check: func(t *testing.T, c Config) {
				if c.AdminCredential != nil {
					t.Fatal("the admin listener is enabled without a hash")
				}
			},
		},
		{
			name: "client and admin on unspecified addresses",
			vars: map[string]string{envAdminHash: validHash, envListen: "0.0.0.0:8080", envAdminListen: "[::]:8082"},
			check: func(t *testing.T, c Config) {
				if c.Listen != netip.MustParseAddrPort("0.0.0.0:8080") || c.AdminListen != netip.MustParseAddrPort("[::]:8082") {
					t.Fatalf("addresses = %v, %v", c.Listen, c.AdminListen)
				}
			},
		},
		{
			name: "same port on different families",
			vars: map[string]string{envAdminHash: validHash, envListen: "0.0.0.0:9000", envAdminListen: "[::1]:9000"},
		},
		{
			name: "ipv6 loopback health and a mapped client address",
			vars: map[string]string{envHealthListen: "[::1]:8081", envListen: "[::ffff:192.0.2.1]:8080"},
			check: func(t *testing.T, c Config) {
				if c.HealthListen != netip.MustParseAddrPort("[::1]:8081") || c.Listen != netip.MustParseAddrPort("192.0.2.1:8080") {
					t.Fatalf("addresses = %v, %v", c.HealthListen, c.Listen)
				}
			},
		},
		{
			name: "debug log level",
			vars: map[string]string{envLogLevel: "debug"},
			check: func(t *testing.T, c Config) {
				if c.LogLevel != slog.LevelDebug {
					t.Fatalf("LogLevel = %v", c.LogLevel)
				}
			},
		},
		{
			name: "warn log level",
			vars: map[string]string{envLogLevel: "warn"},
			check: func(t *testing.T, c Config) {
				if c.LogLevel != slog.LevelWarn {
					t.Fatalf("LogLevel = %v", c.LogLevel)
				}
			},
		},
		{
			name: "nfs storage profile and a free-space floor of 0",
			vars: map[string]string{envStorageProfile: "nfs", envMinFreeBytes: "0"},
			check: func(t *testing.T, c Config) {
				if c.StorageProfile != StorageNFS || c.MinFreeBytes != 0 {
					t.Fatalf("StorageProfile = %q, MinFreeBytes = %d", c.StorageProfile, c.MinFreeBytes)
				}
			},
		},
		{
			name: "explicit local profile and the largest free-space floor",
			vars: map[string]string{envStorageProfile: "local", envMinFreeBytes: "18446744073709551615"},
			check: func(t *testing.T, c Config) {
				if c.StorageProfile != StorageLocal || c.MinFreeBytes != 18446744073709551615 {
					t.Fatalf("StorageProfile = %q, MinFreeBytes = %d", c.StorageProfile, c.MinFreeBytes)
				}
			},
		},
		{
			name: "owner phone and the smallest history cap",
			vars: map[string]string{envOwnerPhone: "+15550100009", envHistoryMax: "1"},
			check: func(t *testing.T, c Config) {
				if c.OwnerPhone != "+15550100009" || c.HistoryMaxBytes != 1 {
					t.Fatalf("OwnerPhone = %q, HistoryMaxBytes = %d", c.OwnerPhone, c.HistoryMaxBytes)
				}
			},
		},
		{
			name: "the largest history cap",
			vars: map[string]string{envHistoryMax: "268435456"},
			check: func(t *testing.T, c Config) {
				if c.OwnerPhone != "" || c.HistoryMaxBytes != 268435456 {
					t.Fatalf("OwnerPhone = %q, HistoryMaxBytes = %d", c.OwnerPhone, c.HistoryMaxBytes)
				}
			},
		},
		{
			name: "the shortest debug window",
			vars: map[string]string{envUnsafeDebug: "1"},
			check: func(t *testing.T, c Config) {
				if c.UnsafeDebug != time.Minute {
					t.Fatalf("UnsafeDebug = %v", c.UnsafeDebug)
				}
			},
		},
		{
			name: "the longest debug window",
			vars: map[string]string{envUnsafeDebug: "60"},
			check: func(t *testing.T, c Config) {
				if c.UnsafeDebug != time.Hour {
					t.Fatalf("UnsafeDebug = %v", c.UnsafeDebug)
				}
			},
		},
		{
			name: "error log level",
			vars: map[string]string{envLogLevel: "error"},
			check: func(t *testing.T, c Config) {
				if c.LogLevel != slog.LevelError {
					t.Fatalf("LogLevel = %v", c.LogLevel)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions()
			if tt.opts != nil {
				tt.opts(&opts)
			}
			cfg, r := Load(environ(withDataDir(t, tt.vars)), opts)
			if r != nil {
				t.Fatalf("Load: %v", r)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestRateLimits(t *testing.T) {
	for _, tt := range []struct {
		name           string
		vars           map[string]string
		read, search   int
		unsafeRateCaps bool
	}{
		{name: "lowest", vars: map[string]string{envReadPerMinute: "1", envSearchPerMinute: "1"}, read: 1, search: 1},
		{name: "caps", vars: map[string]string{envReadPerMinute: "600", envSearchPerMinute: "60", envUnsafeRateCaps: "0"}, read: 600, search: 60},
		{name: "caps lifted", vars: map[string]string{envReadPerMinute: "1000000", envSearchPerMinute: "601", envUnsafeRateCaps: "1"}, read: 1000000, search: 601, unsafeRateCaps: true},
		{name: "caps lifted without a rate", vars: map[string]string{envUnsafeRateCaps: "1"}, read: 600, search: 60, unsafeRateCaps: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, r := Load(environ(withDataDir(t, tt.vars)), testOptions())
			if r != nil || cfg.ReadPerMinute != tt.read || cfg.SearchPerMinute != tt.search || cfg.UnsafeRateCaps != tt.unsafeRateCaps {
				t.Fatalf("Load = %d reads, %d searches, unsafe caps %v, %v", cfg.ReadPerMinute, cfg.SearchPerMinute, cfg.UnsafeRateCaps, r)
			}
		})
	}
}

func TestMetricsEMF(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "0": false} {
		cfg, r := Load(environ(withDataDir(t, map[string]string{envMetricsEMF: value})), testOptions())
		if r != nil || cfg.MetricsEMF != want {
			t.Fatalf("WAWARDEN_METRICS_EMF=%s: MetricsEMF %v, %v", value, cfg.MetricsEMF, r)
		}
	}
}

func TestBackupRecipient(t *testing.T) {
	cfg, r := Load(environ(withDataDir(t, nil)), testOptions())
	if r != nil || cfg.BackupRecipient != "" {
		t.Fatalf("Load without a recipient = %q, %v", cfg.BackupRecipient, r)
	}
	cfg, r = Load(environ(withDataDir(t, map[string]string{envBackupRecipient: syntheticAgeRecipient})), testOptions())
	if r != nil || cfg.BackupRecipient != syntheticAgeRecipient {
		t.Fatalf("Load with a recipient = %q, %v", cfg.BackupRecipient, r)
	}
}

func TestNotifySettings(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o400, 0o640, 0o440} {
		path := secretFile(t, mode)
		vars := withDataDir(t, map[string]string{envNotifyURL: "https://hooks.example.test:8443/x?team=a", envNotifySecret: linkTo(t, path), envNotifyPrivate: "1"})
		cfg, r := Load(environ(vars), testOptions())
		if r != nil {
			t.Fatalf("Load with a secret of mode %v: %v", mode, r)
		}
		if want := (Notify{URL: "https://hooks.example.test:8443/x?team=a", Secret: []byte(syntheticSecret), AllowPrivate: true}); !reflect.DeepEqual(cfg.Notify, want) {
			t.Fatalf("Notify = %+v, want %+v", cfg.Notify, want)
		}
	}
	vars := withDataDir(t, map[string]string{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: secretFile(t, 0o600), envNotifyPrivate: "0"})
	if cfg, r := Load(environ(vars), testOptions()); r != nil || cfg.Notify.AllowPrivate || cfg.Notify.URL == "" {
		t.Fatalf("Load = %+v, %v", cfg.Notify, r)
	}
}

func TestFirstOccurrenceWins(t *testing.T) {
	vars := withDataDir(t, nil)
	env := append(environ(vars), envListen+"=127.0.0.1:9001", envListen+"=localhost:9002")
	cfg, r := Load(env, testOptions())
	if r != nil || cfg.Listen != netip.MustParseAddrPort("127.0.0.1:9001") {
		t.Fatalf("Load = %v, %v; want the first value of a repeated variable", cfg.Listen, r)
	}
}

func TestRefusalsNeverEchoValues(t *testing.T) {
	secret := token.NewAdmin()
	tests := []map[string]string{
		{envAdminHash: secret},
		{envAdminHashFile: writeFile(t, secret)},
		{envListen: secret + ":8080"},
		{envLogLevel: secret},
		{envPlaintextAdmin: secret},
		{"WAWARDEN_DEV_X": secret},
		{"WAWARDEN_DEV_FAKE_ENGINE": secret},
		{"WAWARDEN_UNKNOWN": secret},
		{"GOTRACEBACK": secret},
		{"MCPGODEBUG": secret},
		{"JSONSCHEMAGODEBUG": secret},
		{envStorageProfile: secret},
		{envMinFreeBytes: secret},
		{envOwnerPhone: secret},
		{envHistoryMax: secret},
		{envUnsafeDebug: secret},
		{envMetricsEMF: secret},
		{envNotifyURL: "http://" + secret + ".example.test/x"},
		{envNotifyURL: "https://hooks.example.test/x", envNotifyPrivate: secret},
		{envNotifyURL: "https://hooks.example.test/x", envNotifySecret: writeFile(t, secret[:20])},
		{envBackupRecipient: secret},
		{envBackupRecipient: "age1" + secret},
	}
	for _, vars := range tests {
		_, r := Load(environ(withDataDir(t, vars)), testOptions())
		if r == nil {
			t.Fatalf("Load accepted %v", slices.Collect(maps.Keys(vars)))
		}
		if strings.Contains(r.Error(), secret) {
			t.Fatalf("refusal %q echoes the configured value", r.Error())
		}
	}
	_, r := Load(environ(withDataDir(t, map[string]string{envBackupRecipient: syntheticAgeSecret})), testOptions())
	if r == nil || strings.Contains(r.Error(), syntheticAgeSecret) || strings.Contains(r.Error(), syntheticAgeSecret[16:40]) {
		t.Fatalf("a pasted age secret key gave %v, want a refusal that does not repeat it", r)
	}
}

func TestHealthListen(t *testing.T) {
	tests := []struct {
		name   string
		env    []string
		want   netip.AddrPort
		reason string
	}{
		{name: "default", want: netip.MustParseAddrPort("127.0.0.1:8081")},
		{name: "configured", env: []string{envHealthListen + "=127.0.0.1:9081"}, want: netip.MustParseAddrPort("127.0.0.1:9081")},
		{name: "ignores every other variable", env: []string{"WAWARDEN_UNKNOWN=x", envPlaintextAdmin + "=x", envListen + "=bad"}, want: netip.MustParseAddrPort("127.0.0.1:8081")},
		{name: "not loopback", env: []string{envHealthListen + "=0.0.0.0:8081"}, reason: "health_address_not_loopback"},
		{name: "invalid", env: []string{envHealthListen + "=localhost:8081"}, reason: "listen_address_invalid"},
		{name: "zoned", env: []string{envHealthListen + "=[::1%lo0]:8081"}, reason: "listen_address_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, r := HealthListen(tt.env)
			switch {
			case tt.reason != "" && (r == nil || r.Reason != tt.reason):
				t.Fatalf("HealthListen = %v, %v; want refusal %q", got, r, tt.reason)
			case tt.reason == "" && (r != nil || got != tt.want):
				t.Fatalf("HealthListen = %v, %v; want %v", got, r, tt.want)
			}
		})
	}
}
