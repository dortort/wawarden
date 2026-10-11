package keys

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func secretOf(seed byte) []byte {
	b := make([]byte, masterSize)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func idOf(t *testing.T, secret []byte) string {
	t.Helper()
	m, r := derive(secret)
	if r != nil {
		t.Fatalf("derive: %v", r)
	}
	return m.ID()
}

func retiredDir(t *testing.T, data string, mode fs.FileMode) string {
	t.Helper()
	dir := filepath.Join(data, dirName, retiredName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	chmod(t, dir, mode|fs.ModeDir)
	return dir
}

func writeRetired(t *testing.T, dir, name string, secret []byte, mode fs.FileMode) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	chmod(t, path, mode)
}

func withRetired(t *testing.T, secrets ...[]byte) (data, dir string) {
	t.Helper()
	data = keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	dir = retiredDir(t, data, 0o700)
	for _, s := range secrets {
		writeRetired(t, dir, idOf(t, s), s, 0o600)
	}
	return data, dir
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

func TestLoadKeepsRetiredKeysForLookupOnly(t *testing.T) {
	older, old := secretOf(40), secretOf(80)
	data, dir := withRetired(t, older, old)
	writeRetired(t, dir, ".master-123", secretOf(1), 0o644)
	m := mustLoad(t, data)
	if m.ID() != vectorID {
		t.Fatalf("current key %s, want %s", m.ID(), vectorID)
	}
	want := []string{idOf(t, older), idOf(t, old)}
	slices.Sort(want)
	var got []string
	for _, k := range m.Retired() {
		got = append(got, k.ID())
		if len(k.Retired()) != 0 {
			t.Fatal("a retired key carries retired keys of its own")
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("retired ids %q, want %q in order", got, want)
	}
	for _, s := range [][]byte{older, old} {
		k, ok := m.Lookup(idOf(t, s))
		want, _ := derive(s)
		if !ok || !bytes.Equal(k.AuditChainKey(), want.AuditChainKey()) || !bytes.Equal(k.CursorSealKey(), want.CursorSealKey()) {
			t.Fatalf("Lookup(%s) = %v, %v; want the retired key's derived keys", idOf(t, s), k, ok)
		}
	}
	if k, ok := m.Lookup(m.ID()); !ok || k != m {
		t.Fatal("Lookup of the current id does not give the current key")
	}
	if k, ok := m.Lookup("00000000"); ok || k != nil {
		t.Fatal("Lookup of an unknown id found a key")
	}
	m.Retired()[0] = nil
	if m.Retired()[0] == nil {
		t.Fatal("changing the returned list changed the master's retired keys")
	}
	if got := mustLoad(t, keysDir(t, 0o700)).Retired(); len(got) != 0 {
		t.Fatalf("a keys directory without retired/ gave retired keys %v", got)
	}
}

func TestRetiredKeyRefusals(t *testing.T) {
	uid := os.Geteuid()
	foreign := func(name string) func(fs.FileInfo) (int, bool) {
		return func(fi fs.FileInfo) (int, bool) {
			if fi.Name() == name {
				return uid + 1, true
			}
			return uid, true
		}
	}
	retired := secretOf(80)
	id := idOf(t, retired)
	tests := []struct {
		name   string
		setup  func(t *testing.T) string
		owner  func(fs.FileInfo) (int, bool)
		reason string
	}{
		{name: "retired is a file", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeRetired(t, filepath.Join(data, dirName), retiredName, retired, 0o600)
			return data
		}, reason: reasonRetiredDirNotDirectory},
		{name: "retired is a symbolic link to a private directory", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			_, elsewhere := withRetired(t, retired)
			symlinkTo(t, elsewhere, filepath.Join(data, dirName, retiredName))
			return data
		}, reason: reasonRetiredDirNotDirectory},
		{name: "retired owned by another user", setup: func(t *testing.T) string {
			data, _ := withRetired(t)
			return data
		}, owner: foreign(retiredName), reason: reasonRetiredDirForeignOwner},
		{name: "retired mode 0750", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			chmod(t, dir, 0o750|fs.ModeDir)
			return data
		}, reason: reasonRetiredDirPermissions},
		{name: "retired mode 0700 with the setgid bit", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			chmod(t, dir, 0o700|fs.ModeDir|fs.ModeSetgid)
			return data
		}, reason: reasonRetiredDirPermissions},
		{name: "an entry named in upper case", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, strings.ToUpper("abcdef01"), retired, 0o600)
			return data
		}, reason: reasonRetiredName},
		{name: "an entry with seven digits", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, id[:7], retired, 0o600)
			return data
		}, reason: reasonRetiredName},
		{name: "an entry named master", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, masterName, retired, 0o600)
			return data
		}, reason: reasonRetiredName},
		{name: "33 retired keys", setup: func(t *testing.T) string {
			var secrets [][]byte
			for i := range maxRetired + 1 {
				secrets = append(secrets, secretOf(byte(100+i)))
			}
			data, _ := withRetired(t, secrets...)
			return data
		}, reason: reasonRetiredTooMany},
		{name: "an entry that is a directory", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			if err := os.Mkdir(filepath.Join(dir, id), 0o700); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			return data
		}, reason: reasonRetiredNotRegular},
		{name: "an entry that is a symbolic link to a valid key", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			_, elsewhere := withRetired(t, retired)
			symlinkTo(t, filepath.Join(elsewhere, id), filepath.Join(dir, id))
			return data
		}, reason: reasonRetiredNotRegular},
		{name: "an entry owned by another user", setup: func(t *testing.T) string {
			data, _ := withRetired(t, retired)
			return data
		}, owner: foreign(id), reason: reasonRetiredForeignOwner},
		{name: "an entry readable by its group", setup: func(t *testing.T) string {
			data, dir := withRetired(t, retired)
			chmod(t, filepath.Join(dir, id), 0o640)
			return data
		}, reason: reasonRetiredPermissions},
		{name: "an entry with the setuid bit", setup: func(t *testing.T) string {
			data, dir := withRetired(t, retired)
			chmod(t, filepath.Join(dir, id), 0o600|fs.ModeSetuid)
			return data
		}, reason: reasonRetiredPermissions},
		{name: "an entry one byte short", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, id, retired[1:], 0o600)
			return data
		}, reason: reasonRetiredSize},
		{name: "an entry one byte long", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, id, append(slices.Clone(retired), 0), 0o600)
			return data
		}, reason: reasonRetiredSize},
		{name: "an entry under another key's id", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, idOf(t, secretOf(81)), retired, 0o600)
			return data
		}, reason: reasonRetiredIDMismatch},
		{name: "an entry holding the current key", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, vectorID, vectorMaster(), 0o600)
			return data
		}, reason: reasonRetiredCurrent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setup(t)
			owner := tt.owner
			if owner == nil {
				owner = fileOwner
			}
			m, r := load(data, uid, owner)
			if r == nil || m != nil {
				t.Fatalf("load = %v, %v; want the refusal %s", m, r, tt.reason)
			}
			if r.Reason != tt.reason {
				t.Fatalf("refusal %q (%v), want %q", r.Reason, r, tt.reason)
			}
			if !strings.HasPrefix(r.Error(), "keys: "+retiredLabel) || strings.Contains(r.Error(), data) {
				t.Fatalf("refusal text %q must start with keys: %s and must not repeat the data directory's path", r.Error(), retiredLabel)
			}
		})
	}
	data, _ := withRetired(t)
	for i := range maxRetired {
		s := secretOf(byte(100 + i))
		writeRetired(t, filepath.Join(data, dirName, retiredName), idOf(t, s), s, 0o600)
	}
	if got := len(mustLoad(t, data).Retired()); got != maxRetired {
		t.Fatalf("32 retired keys loaded as %d", got)
	}
}

func TestLoadFilesReadsACopyWithoutOwnerOrModeChecks(t *testing.T) {
	dir := t.TempDir()
	master := filepath.Join(dir, "master")
	if err := os.WriteFile(master, vectorMaster(), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	chmod(t, master, 0o644)
	retired := RetiredDirOf(master)
	if retired != filepath.Join(dir, retiredName) {
		t.Fatalf("RetiredDirOf = %s, want the retired directory beside the master key file", retired)
	}
	if err := os.Mkdir(retired, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	chmod(t, retired, 0o755|fs.ModeDir)
	old := secretOf(80)
	writeRetired(t, retired, idOf(t, old), old, 0o644)
	m, r := LoadFiles(master, retired)
	if r != nil {
		t.Fatalf("LoadFiles: %v", r)
	}
	if k, ok := m.Lookup(idOf(t, old)); m.ID() != vectorID || !ok || k.ID() != idOf(t, old) {
		t.Fatalf("LoadFiles gave %s with retired %v", m.ID(), m.Retired())
	}
	if m, r := LoadFiles(master, ""); r != nil || len(m.Retired()) != 0 {
		t.Fatalf("LoadFiles without a retired directory = %v, %v; want the master alone", m, r)
	}
	if m, r := LoadFiles(master, filepath.Join(dir, "absent")); m != nil || r == nil || r.Reason != reasonRetiredDirUnusable {
		t.Fatalf("LoadFiles with a missing retired directory = %v, %v; want %s", m, r, reasonRetiredDirUnusable)
	}
	if m, r := LoadFiles(filepath.Join(dir, "absent"), retired); m != nil || r == nil || r.Reason != reasonMasterUnusable {
		t.Fatalf("LoadFiles with a missing master key = %v, %v; want %s", m, r, reasonMasterUnusable)
	}
	writeRetired(t, retired, vectorID, vectorMaster(), 0o644)
	if m, r := LoadFiles(master, retired); m != nil || r == nil || r.Reason != reasonRetiredCurrent || strings.Contains(r.Error(), dir) {
		t.Fatalf("LoadFiles with the current key among the retired = %v, %v; want %s without the path", m, r, reasonRetiredCurrent)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{"master", retiredName}) {
		t.Fatalf("LoadFiles left %q", got)
	}
}

func stoppedService() error { return nil }

func TestRotateRetiresTheMasterAndInstallsANewOne(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	previous, current, r := Rotate(data, os.Geteuid(), stoppedService)
	if r != nil {
		t.Fatalf("Rotate: %v", r)
	}
	if previous != vectorID || !keyID.MatchString(current) || current == previous {
		t.Fatalf("Rotate = %s, %s; want %s and a new key id", previous, current, vectorID)
	}
	dir := filepath.Join(data, dirName)
	if got := entries(t, dir); !slices.Equal(got, []string{masterName, retiredName}) {
		t.Fatalf("keys/ holds %q after the rotation", got)
	}
	if got := entries(t, filepath.Join(dir, retiredName)); !slices.Equal(got, []string{vectorID}) {
		t.Fatalf("keys/retired/ holds %q, want only %s", got, vectorID)
	}
	for path, mode := range map[string]fs.FileMode{
		filepath.Join(dir, retiredName):           fs.ModeDir | 0o700,
		filepath.Join(dir, retiredName, vectorID): 0o600,
		filepath.Join(dir, masterName):            0o600,
	} {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&(fs.ModeType|fs.ModePerm|specialBits) != mode {
			t.Fatalf("%s: %v %v, want mode %v", path, fi, err, mode)
		}
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, retiredName, vectorID)), vectorMaster()) {
		t.Fatal("keys/retired/ does not hold the previous master key")
	}
	fresh := readFile(t, filepath.Join(dir, masterName))
	if len(fresh) != masterSize || bytes.Equal(fresh, vectorMaster()) || idOf(t, fresh) != current {
		t.Fatal("keys/master does not hold a new key with the printed id")
	}
	m := mustLoad(t, data)
	if old, ok := m.Lookup(previous); m.ID() != current || !ok || hex.EncodeToString(old.AuditChainKey()) != vectorAudit {
		t.Fatalf("a start after the rotation loads %s with retired %v", m.ID(), m.Retired())
	}

	again, third, r := Rotate(data, os.Geteuid(), stoppedService)
	if r != nil || again != current || third == current || third == previous {
		t.Fatalf("a second Rotate = %s, %s, %v", again, third, r)
	}
	m = mustLoad(t, data)
	if len(m.Retired()) != 2 || m.ID() != third {
		t.Fatalf("after two rotations the start loads %s with %d retired keys", m.ID(), len(m.Retired()))
	}
}

func TestRotateMakesEachStepDurableInOrder(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	dir := filepath.Join(data, dirName)
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	type flush struct {
		dir     string
		entries []string
		master  []byte
	}
	var flushed []flush
	syncDir = func(d string) error {
		flushed = append(flushed, flush{dir: d, entries: entries(t, d), master: readFile(t, filepath.Join(dir, masterName))})
		return saved(d)
	}
	_, current, r := Rotate(data, os.Geteuid(), stoppedService)
	if r != nil {
		t.Fatalf("Rotate: %v", r)
	}
	retired := filepath.Join(dir, retiredName)
	if len(flushed) != 3 {
		t.Fatalf("Rotate flushed %d directories, want keys/ for retired/, retired/ for the old key, keys/ for the new master", len(flushed))
	}
	for i, want := range []flush{
		{dir: dir, entries: []string{masterName, retiredName}, master: vectorMaster()},
		{dir: retired, entries: []string{vectorID}, master: vectorMaster()},
		{dir: dir, entries: []string{masterName, retiredName}},
	} {
		got := flushed[i]
		if got.dir != want.dir || !slices.Equal(got.entries, want.entries) {
			t.Fatalf("flush %d: %s holding %q, want %s holding %q", i, got.dir, got.entries, want.dir, want.entries)
		}
		if want.master != nil && !bytes.Equal(got.master, want.master) {
			t.Fatalf("flush %d happened after keys/master was replaced", i)
		}
	}
	if idOf(t, flushed[2].master) != current {
		t.Fatal("the last flush did not follow the replacement of keys/master")
	}
}

func TestRotateRefusesARunningServiceBeforeAnyChange(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	called := false
	_, _, r := Rotate(data, os.Geteuid(), func() error {
		called = true
		return errors.New("archive.db is in use")
	})
	if !called || r == nil || r.Reason != reasonServiceRunning || !strings.Contains(r.Error(), "archive.db is in use") {
		t.Fatalf("Rotate with the service running = %v, want %s", r, reasonServiceRunning)
	}
	if got := entries(t, filepath.Join(data, dirName)); !slices.Equal(got, []string{masterName}) {
		t.Fatalf("a refused rotation left %q in keys/", got)
	}
	if !bytes.Equal(readFile(t, filepath.Join(data, dirName, masterName)), vectorMaster()) {
		t.Fatal("a refused rotation changed keys/master")
	}
}

func TestRotateFinishesAnInterruptedRotation(t *testing.T) {
	data, dir := withRetired(t)
	writeRetired(t, dir, vectorID, vectorMaster(), 0o600)
	if _, r := Load(data, os.Geteuid()); r == nil || r.Reason != reasonRetiredCurrent {
		t.Fatalf("a start after an interrupted rotation = %v, want %s", r, reasonRetiredCurrent)
	}
	previous, current, r := Rotate(data, os.Geteuid(), stoppedService)
	if r != nil || previous != vectorID {
		t.Fatalf("Rotate after an interrupted rotation = %s, %s, %v", previous, current, r)
	}
	if m := mustLoad(t, data); m.ID() != current || len(m.Retired()) != 1 {
		t.Fatalf("the start after the finished rotation loads %s with %d retired keys", m.ID(), len(m.Retired()))
	}
}

func TestRotateRefusals(t *testing.T) {
	uid := os.Geteuid()
	tests := []struct {
		name   string
		setup  func(t *testing.T) string
		secret func() []byte
		reason string
	}{
		{name: "no keys directory", setup: dataDir, reason: reasonDirUnusable},
		{name: "no master key", setup: func(t *testing.T) string { return keysDir(t, 0o700) }, reason: reasonMasterUnusable},
		{name: "a master key readable by others", setup: func(t *testing.T) string {
			data := keysDir(t, 0o700)
			writeMaster(t, data, vectorMaster(), 0o604)
			return data
		}, reason: reasonMasterPermissions},
		{name: "keys mode 0750", setup: func(t *testing.T) string {
			data := keysDir(t, 0o750)
			writeMaster(t, data, vectorMaster(), 0o600)
			return data
		}, reason: reasonDirPermissions},
		{name: "a retired directory mode 0755", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			chmod(t, dir, 0o755|fs.ModeDir)
			return data
		}, reason: reasonRetiredDirPermissions},
		{name: "a malformed retired entry", setup: func(t *testing.T) string {
			data, dir := withRetired(t)
			writeRetired(t, dir, "notakey", secretOf(1), 0o600)
			return data
		}, reason: reasonRetiredName},
		{name: "32 retired keys", setup: func(t *testing.T) string {
			var secrets [][]byte
			for i := range maxRetired {
				secrets = append(secrets, secretOf(byte(100+i)))
			}
			data, _ := withRetired(t, secrets...)
			return data
		}, reason: reasonRetiredTooMany},
		{name: "a new key equal to the current one", setup: func(t *testing.T) string {
			data, _ := withRetired(t)
			return data
		}, secret: vectorMaster, reason: reasonRotateCollision},
		{name: "a new key equal to a retired one", setup: func(t *testing.T) string {
			data, _ := withRetired(t, secretOf(80))
			return data
		}, secret: func() []byte { return secretOf(80) }, reason: reasonRotateCollision},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setup(t)
			if tt.secret != nil {
				saved := newSecret
				t.Cleanup(func() { newSecret = saved })
				newSecret = tt.secret
			}
			before := snapshot(t, data)
			previous, current, r := rotate(data, &ownership{uid: uid, owner: fileOwner}, stoppedService)
			if r == nil || r.Reason != tt.reason || previous != "" || current != "" {
				t.Fatalf("rotate = %q, %q, %v; want the refusal %s", previous, current, r, tt.reason)
			}
			if strings.Contains(r.Error(), data) {
				t.Fatalf("refusal text %q repeats the data directory's path", r.Error())
			}
			if after := snapshot(t, data); !slices.Equal(after, before) {
				t.Fatalf("a refused rotation changed the data directory:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func snapshot(t *testing.T, data string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(data, path)
		if d.Type().IsRegular() {
			return appendFile(&out, rel, path)
		}
		out = append(out, rel+"/")
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return out
}

func appendFile(out *[]string, rel, path string) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	*out = append(*out, rel+"="+hex.EncodeToString(b))
	return nil
}

func TestRetireRefusesAnotherKeyUnderTheSameName(t *testing.T) {
	data, dir := withRetired(t, secretOf(80))
	o := &ownership{uid: os.Geteuid(), owner: fileOwner}
	id := idOf(t, secretOf(80))
	if r := retire(dir, id, secretOf(80), o); r != nil {
		t.Fatalf("retire of an identical copy = %v, want it accepted", r)
	}
	r := retire(dir, id, secretOf(81), o)
	if r == nil || r.Reason != reasonRetiredConflict || strings.Contains(r.Error(), data) {
		t.Fatalf("retire over another key = %v, want %s", r, reasonRetiredConflict)
	}
	if !bytes.Equal(readFile(t, filepath.Join(dir, id)), secretOf(80)) {
		t.Fatal("retire replaced a retired key")
	}
	if got := entries(t, dir); !slices.Equal(got, []string{id}) {
		t.Fatalf("retire left %q", got)
	}
}

func TestRotateNeedsTheProcessToOwnTheKeys(t *testing.T) {
	data := keysDir(t, 0o700)
	writeMaster(t, data, vectorMaster(), 0o600)
	before := snapshot(t, data)
	if _, _, r := Rotate(data, os.Geteuid()+1, stoppedService); r == nil || r.Reason != reasonDirForeignOwner {
		t.Fatalf("Rotate as another user = %v, want %s", r, reasonDirForeignOwner)
	}
	if after := snapshot(t, data); !slices.Equal(after, before) {
		t.Fatal("a refused rotation changed the data directory")
	}
}
