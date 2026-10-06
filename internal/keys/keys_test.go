package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

const (
	vectorID        = "6a53922c"
	vectorLogRedact = "2b5bed1f6ae0a12443e399c345dc73267d0a36a83a3096356445bdc293a96f55"
	vectorChatHMAC  = "3a3dfe0058f126cf88a2cddae5304fb7efd5dbe350ee066c52fa8d8ebcf743b2"
	vectorCursor    = "497e790a7e9ede5e9802cb058072ec6c8e72e816d68679582c31df6704e93608"
	vectorMref      = "d38e7716d67bbe35f4280e2ff20a733c05af9007f459a8307d9fc1044d17c578"
	vectorAudit     = "ffe711ed496756b9b35266179d8289a0dab2e7b38ab4ee393f2a2fa36d6622a1"
)

var keyID = regexp.MustCompile(`^[0-9a-f]{8}$`)

func vectorMaster() []byte {
	b := make([]byte, masterSize)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func dataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	return dir
}

func keysDir(t *testing.T, mode fs.FileMode) string {
	t.Helper()
	data := dataDir(t)
	dir := filepath.Join(data, dirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	chmod(t, dir, mode|fs.ModeDir)
	return data
}

func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := fi.Mode() & (fs.ModeType | fs.ModePerm | specialBits); got != mode {
		t.Fatalf("%s has mode %v after Chmod, want %v", path, got, mode)
	}
}

func writeMaster(t *testing.T, data string, content []byte, mode fs.FileMode) string {
	t.Helper()
	path := filepath.Join(data, dirName, masterName)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	chmod(t, path, mode)
	return path
}

func mustLoad(t *testing.T, data string) *Master {
	t.Helper()
	m, r := Load(data, os.Geteuid())
	if r != nil {
		t.Fatalf("Load = %v (%s)", r, r.Reason)
	}
	return m
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestLoadCreatesAPrivateMasterKeyOnce(t *testing.T) {
	data := dataDir(t)
	first := mustLoad(t, data)
	if !keyID.MatchString(first.ID()) {
		t.Fatalf("key id %q, want 8 lower-case hexadecimal digits", first.ID())
	}
	dir := filepath.Join(data, dirName)
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s: %v %v, want a directory with mode 0700", dir, fi, err)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{masterName}) {
		t.Fatalf("%s holds %q, want only %s", dir, got, masterName)
	}
	path := filepath.Join(dir, masterName)
	fi, err = os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() != masterSize {
		t.Fatalf("%s: %v %v, want a regular file of 32 bytes with mode 0600", path, fi, err)
	}
	created, err := fs.ReadFile(os.DirFS(dir), masterName)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Equal(created, make([]byte, masterSize)) {
		t.Fatal("the master key is all zero bytes")
	}

	second := mustLoad(t, data)
	if second.ID() != first.ID() || !bytes.Equal(second.LogRedactKey(), first.LogRedactKey()) || !bytes.Equal(second.ChatHMACKey(), first.ChatHMACKey()) {
		t.Fatal("a second Load derived other keys from the same master key")
	}
	again, err := fs.ReadFile(os.DirFS(dir), masterName)
	if err != nil || !bytes.Equal(again, created) {
		t.Fatalf("a second Load changed the master key: %v", err)
	}
	if other := mustLoad(t, dataDir(t)); other.ID() == first.ID() || bytes.Equal(other.LogRedactKey(), first.LogRedactKey()) {
		t.Fatal("two data directories got the same master key")
	}
}

func TestTheTemporaryNameIsGoneWhenTheDirectoryIsFlushed(t *testing.T) {
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	var flushed [][]string
	syncDir = func(dir string) error {
		flushed = append(flushed, entries(t, dir))
		return saved(dir)
	}
	mustLoad(t, dataDir(t))
	if len(flushed) != 1 || !slices.Equal(flushed[0], []string{masterName}) {
		t.Fatalf("the keys directory held %q when it was flushed, want only %s", flushed, masterName)
	}
}

func TestRacingCreatorsEndWithOneKey(t *testing.T) {
	const rounds, racers = 20, 16
	for range rounds {
		data := dataDir(t)
		start := make(chan struct{})
		masters := make([]*Master, racers)
		refusals := make([]*Refusal, racers)
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				masters[i], refusals[i] = Load(data, os.Geteuid())
			}()
		}
		close(start)
		wg.Wait()
		for i, r := range refusals {
			if r != nil {
				t.Fatalf("racer %d: %v (%s)", i, r, r.Reason)
			}
			if masters[i].ID() != masters[0].ID() || !bytes.Equal(masters[i].LogRedactKey(), masters[0].LogRedactKey()) {
				t.Fatalf("racer %d loaded key %s, racer 0 loaded %s", i, masters[i].ID(), masters[0].ID())
			}
		}
		if got := entries(t, filepath.Join(data, dirName)); !slices.Equal(got, []string{masterName}) {
			t.Fatalf("after the race the keys directory holds %q, want only %s", got, masterName)
		}
		if got := mustLoad(t, data).ID(); got != masters[0].ID() {
			t.Fatalf("the key on disk is %s, the racers loaded %s", got, masters[0].ID())
		}
	}
}

func hkdfSHA256(secret []byte, info string, length int) []byte {
	extract := hmac.New(sha256.New, make([]byte, sha256.Size))
	extract.Write(secret)
	prk := extract.Sum(nil)
	var out, block []byte
	for counter := byte(1); len(out) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		expand.Write(block)
		expand.Write([]byte(info))
		expand.Write([]byte{counter})
		block = expand.Sum(nil)
		out = append(out, block...)
	}
	return out[:length]
}

func TestDerivationVectors(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	m := mustLoad(t, data)
	tests := []struct {
		name, got, want, info string
		length                int
	}{
		{name: "key id", got: m.ID(), want: vectorID, info: "wawarden/v1/key-id", length: idSize},
		{name: "log-redact", got: hex.EncodeToString(m.LogRedactKey()), want: vectorLogRedact, info: "wawarden/v1/log-redact", length: keySize},
		{name: "chat-hmac", got: hex.EncodeToString(m.ChatHMACKey()), want: vectorChatHMAC, info: "wawarden/v1/chat-hmac", length: keySize},
		{name: "cursor-seal", got: hex.EncodeToString(m.CursorSealKey()), want: vectorCursor, info: "wawarden/v1/cursor-seal", length: keySize},
		{name: "mref", got: hex.EncodeToString(m.MessageRefKey()), want: vectorMref, info: "wawarden/v1/mref", length: keySize},
		{name: "audit-chain", got: hex.EncodeToString(m.AuditChainKey()), want: vectorAudit, info: "wawarden/v1/audit-chain", length: keySize},
	}
	for _, tt := range tests {
		if independent := hex.EncodeToString(hkdfSHA256(vectorMaster(), tt.info, tt.length)); independent != tt.want {
			t.Fatalf("%s: the HKDF written out in this test gives %s, the recorded vector is %s", tt.name, independent, tt.want)
		}
		if tt.got != tt.want {
			t.Fatalf("%s = %s, want %s: a derived key changed, which changes every pseudonym and HMAC made with it", tt.name, tt.got, tt.want)
		}
	}
	purposes := []func() []byte{m.LogRedactKey, m.ChatHMACKey, m.CursorSealKey, m.MessageRefKey, m.AuditChainKey}
	for i, a := range purposes {
		for _, b := range purposes[i+1:] {
			if bytes.Equal(a(), b()) {
				t.Fatal("two purpose keys are equal")
			}
		}
		want := hex.EncodeToString(a())
		k := a()
		clear(k)
		if hex.EncodeToString(a()) != want {
			t.Fatal("changing a returned key changed the key the master holds")
		}
	}
}

func TestLoadFileReadsACopyWithoutCreatingOrChanging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "copy")
	if err := os.WriteFile(path, vectorMaster(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	chmod(t, path, 0o444)
	m, r := LoadFile(path)
	if r != nil {
		t.Fatalf("LoadFile: %v", r)
	}
	if m.ID() != vectorID || hex.EncodeToString(m.AuditChainKey()) != vectorAudit || hex.EncodeToString(m.ChatHMACKey()) != vectorChatHMAC {
		t.Fatal("LoadFile derived other keys than Load")
	}
	if got := entries(t, dir); !slices.Equal(got, []string{"copy"}) {
		t.Fatalf("LoadFile left %q in the directory", got)
	}
	tests := []struct {
		name, reason string
		setup        func() string
	}{
		{name: "missing", reason: reasonMasterUnusable, setup: func() string { return filepath.Join(dir, "absent") }},
		{name: "directory", reason: reasonMasterNotRegular, setup: func() string { return dir }},
		{name: "short", reason: reasonMasterSize, setup: func() string {
			short := filepath.Join(t.TempDir(), "short")
			if err := os.WriteFile(short, vectorMaster()[1:], 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return short
		}},
		{name: "long", reason: reasonMasterSize, setup: func() string {
			long := filepath.Join(t.TempDir(), "long")
			if err := os.WriteFile(long, append(vectorMaster(), 0), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return long
		}},
		{name: "fifo", reason: reasonMasterNotRegular, setup: func() string {
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatalf("Mkfifo: %v", err)
			}
			return fifo
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup()
			if m, r := LoadFile(path); m != nil || r == nil || r.Reason != tt.reason {
				t.Fatalf("LoadFile(%s) = %v, %v, want refusal %s", tt.name, m, r, tt.reason)
			}
			if _, err := os.Lstat(path); tt.name == "missing" && err == nil {
				t.Fatal("LoadFile created the missing file")
			}
		})
	}
}

func TestReadOnlyMasterKeyIsAccepted(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o400)
	if got := mustLoad(t, data).ID(); got != vectorID {
		t.Fatalf("key id %s, want %s", got, vectorID)
	}
}

func symlinkTo(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	uid := os.Geteuid()
	wrongFiles := func(fi fs.FileInfo) (int, bool) {
		if fi.IsDir() {
			return uid, true
		}
		return uid + 1, true
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T) string
		uid    int
		owner  func(fs.FileInfo) (int, bool)
		reason string
	}{
		{name: "missing data directory", setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, reason: reasonDirUnusable},
		{name: "data directory is a file", setup: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return path
		}, reason: reasonDirUnusable},
		{name: "keys is a file", setup: func(t *testing.T) string {
			data := dataDir(t)
			if err := os.WriteFile(filepath.Join(data, dirName), vectorMaster(), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return data
		}, reason: reasonDirNotDirectory},
		{name: "keys is a symbolic link to a private directory", setup: func(t *testing.T) string {
			data := dataDir(t)
			elsewhere := keysDir(t, 0o700)
			writeMaster(t, elsewhere, vectorMaster(), 0o600)
			symlinkTo(t, filepath.Join(elsewhere, dirName), filepath.Join(data, dirName))
			return data
		}, reason: reasonDirNotDirectory},
		{name: "keys owned by another user", setup: dataDir, uid: uid + 1, reason: reasonDirForeignOwner},
		{name: "keys without a known owner", setup: dataDir, owner: func(fs.FileInfo) (int, bool) { return uid, false }, reason: reasonDirForeignOwner},
		{name: "keys mode 0750", setup: func(t *testing.T) string { return keysDir(t, 0o750) }, reason: reasonDirPermissions},
		{name: "keys mode 0705", setup: func(t *testing.T) string { return keysDir(t, 0o705) }, reason: reasonDirPermissions},
		{name: "keys mode 0500", setup: func(t *testing.T) string { return keysDir(t, 0o500) }, reason: reasonDirPermissions},
		{name: "keys mode 0700 with the sticky bit", setup: func(t *testing.T) string { return keysDir(t, 0o700|fs.ModeSticky) }, reason: reasonDirPermissions},
		{name: "master is a directory", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			if err := os.Mkdir(filepath.Join(data, dirName, masterName), 0o700); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			return data
		}, reason: reasonMasterNotRegular},
		{name: "master is a symbolic link to a valid key", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			target := writeMaster(t, keysDir(t, 0o700), vectorMaster(), 0o600)
			symlinkTo(t, target, filepath.Join(data, dirName, masterName))
			return data
		}, reason: reasonMasterNotRegular},
		{name: "master is a dangling symbolic link", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			symlinkTo(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(data, dirName, masterName))
			return data
		}, reason: reasonMasterNotRegular},
		{name: "master is a named pipe", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			if err := syscall.Mkfifo(filepath.Join(data, dirName, masterName), 0o600); err != nil {
				t.Fatalf("Mkfifo: %v", err)
			}
			return data
		}, reason: reasonMasterNotRegular},
		{name: "master owned by another user", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o600)
			return data
		}, owner: wrongFiles, reason: reasonMasterForeignOwner},
		{name: "master readable by its group", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o640)
			return data
		}, reason: reasonMasterPermissions},
		{name: "master writable by its group", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o620)
			return data
		}, reason: reasonMasterPermissions},
		{name: "master readable by others", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o604)
			return data
		}, reason: reasonMasterPermissions},
		{name: "master executable by others", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o601)
			return data
		}, reason: reasonMasterPermissions},
		{name: "master with the setuid bit", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o600|fs.ModeSetuid)
			return data
		}, reason: reasonMasterPermissions},
		{name: "empty master", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, nil, 0o600)
			return data
		}, reason: reasonMasterSize},
		{name: "master one byte short", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster()[1:], 0o600)
			return data
		}, reason: reasonMasterSize},
		{name: "master one byte long", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, append(vectorMaster(), 0), 0o600)
			return data
		}, reason: reasonMasterSize},
		{name: "master of 64 bytes", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, append(vectorMaster(), vectorMaster()...), 0o600)
			return data
		}, reason: reasonMasterSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setup(t)
			want := tt.uid
			if want == 0 {
				want = uid
			}
			owner := tt.owner
			if owner == nil {
				owner = fileOwner
			}
			m, r := load(data, want, owner)
			if r == nil || m != nil {
				t.Fatalf("load = %v, %v; want the refusal %s", m, r, tt.reason)
			}
			if r.Reason != tt.reason {
				t.Fatalf("refusal %q (%v), want %q", r.Reason, r, tt.reason)
			}
			if !strings.HasPrefix(r.Error(), "keys: ") || strings.Contains(r.Error(), data) {
				t.Fatalf("refusal text %q must start with keys: and must not repeat the data directory's path", r.Error())
			}
		})
	}
}

func TestUnreadableMasterKey(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o200)
	m, r := Load(data, os.Geteuid())
	if os.Geteuid() == 0 {
		if r != nil || m.ID() != vectorID {
			t.Fatalf("Load as root = %v, %v; want the key, which root can read whatever its mode", m, r)
		}
		return
	}
	if r == nil || r.Reason != reasonMasterUnusable || strings.Contains(r.Error(), data) {
		t.Fatalf("Load of a master key its owner cannot read = %v, %v; want %s without the data directory's path", m, r, reasonMasterUnusable)
	}
}

func decimal(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.Itoa(int(c))
	}
	return strings.Join(parts, " ")
}

func TestMasterPrintsNoKey(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	m := mustLoad(t, data)
	var out strings.Builder
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		fmt.Fprintf(&out, verb+"\n", m)
		fmt.Fprintf(&out, verb+"\n", *m)
	}
	encoded, err := json.Marshal([]any{m, *m})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out.Write(encoded)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("master", "m", m, "v", *m)
	slog.New(slog.NewTextHandler(&out, nil)).Info("master", "m", m, "v", *m)
	text := strings.ToLower(out.String())
	for _, secret := range [][]byte{vectorMaster(), m.LogRedactKey(), m.ChatHMACKey(), m.CursorSealKey(), m.MessageRefKey(), m.AuditChainKey()} {
		for _, form := range []string{hex.EncodeToString(secret), hex.EncodeToString(secret[:8]), string(secret[8:16]), decimal(secret[:8])} {
			if strings.Contains(text, strings.ToLower(form)) {
				t.Fatalf("printing a Master revealed key material %q:\n%s", form, out.String())
			}
		}
	}
}
