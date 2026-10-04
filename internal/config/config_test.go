package config

import (
	"io/fs"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if defaultDataDir != "/data" {
		t.Fatalf("default data directory = %q, want /data", defaultDataDir)
	}
	vars := withDataDir(t, nil)
	cfg, r := Load(environ(vars), testOptions())
	if r != nil {
		t.Fatalf("Load: %v", r)
	}
	want := Config{
		DataDir:      vars[envDataDir],
		Listen:       netip.MustParseAddrPort("127.0.0.1:8080"),
		AdminListen:  netip.MustParseAddrPort("127.0.0.1:8082"),
		HealthListen: netip.MustParseAddrPort("127.0.0.1:8081"),
		LogLevel:     slog.LevelInfo,
	}
	if cfg != want {
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
		{name: "dev variable", vars: map[string]string{"WAWARDEN_DEV_FAKE_ENGINE": "1"}, reason: devVariableReason, variable: "WAWARDEN_DEV_FAKE_ENGINE"},
		{name: "dev variable before unknown names", vars: map[string]string{"WAWARDEN_AAA": "x", "WAWARDEN_DEV_X": ""}, reason: devVariableReason, variable: firstOfDevAndUnknown},

		{name: "traceback all", vars: map[string]string{"GOTRACEBACK": "all"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback system", vars: map[string]string{"GOTRACEBACK": "system"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback crash", vars: map[string]string{"GOTRACEBACK": "crash"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback wer", vars: map[string]string{"GOTRACEBACK": "wer"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 0, which still prints every goroutine", vars: map[string]string{"GOTRACEBACK": "0"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 1", vars: map[string]string{"GOTRACEBACK": "1"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback 2", vars: map[string]string{"GOTRACEBACK": "2"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},
		{name: "traceback in another case", vars: map[string]string{"GOTRACEBACK": "Single"}, reason: "traceback_level_unsafe", variable: "GOTRACEBACK"},

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
			if cfg != (Config{}) {
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
	if r != nil || cfg.DataDir != path {
		t.Fatalf("Load = %+v, %v; want the private directory owned by the current user accepted", cfg, r)
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
		{"WAWARDEN_UNKNOWN": secret},
		{"GOTRACEBACK": secret},
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
