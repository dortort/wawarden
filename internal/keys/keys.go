// Package keys owns the keys directory of the data directory: it creates and checks the master key, keeps the retired ones, replaces the master on rotation and derives every purpose key.
package keys

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	dirName     = "keys"
	masterName  = "master"
	retiredName = "retired"
	masterSize  = 32
	keySize     = 32
	idSize      = 4
	maxRetired  = 32
	infoPrefix  = "wawarden/v1/"
	idInfo      = infoPrefix + "key-id"
	logInfo     = infoPrefix + "log-redact"
	chatInfo    = infoPrefix + "chat-hmac"
	cursorInfo  = infoPrefix + "cursor-seal"
	mrefInfo    = infoPrefix + "mref"
	auditInfo   = infoPrefix + "audit-chain"

	keysLabel    = dirName + "/"
	masterLabel  = dirName + "/" + masterName
	retiredLabel = dirName + "/" + retiredName + "/"
)

const (
	reasonDirUnusable        = "keys_dir_unusable"
	reasonDirNotDirectory    = "keys_dir_not_directory"
	reasonDirForeignOwner    = "keys_dir_foreign_owner"
	reasonDirPermissions     = "keys_dir_permissions"
	reasonMasterUnusable     = "master_key_unusable"
	reasonMasterNotRegular   = "master_key_not_regular"
	reasonMasterForeignOwner = "master_key_foreign_owner"
	reasonMasterPermissions  = "master_key_permissions"
	reasonMasterSize         = "master_key_size"

	reasonRetiredDirUnusable     = "retired_keys_dir_unusable"
	reasonRetiredDirNotDirectory = "retired_keys_dir_not_directory"
	reasonRetiredDirForeignOwner = "retired_keys_dir_foreign_owner"
	reasonRetiredDirPermissions  = "retired_keys_dir_permissions"
	reasonRetiredName            = "retired_key_name"
	reasonRetiredTooMany         = "retired_keys_too_many"
	reasonRetiredUnusable        = "retired_key_unusable"
	reasonRetiredNotRegular      = "retired_key_not_regular"
	reasonRetiredForeignOwner    = "retired_key_foreign_owner"
	reasonRetiredPermissions     = "retired_key_permissions"
	reasonRetiredSize            = "retired_key_size"
	reasonRetiredIDMismatch      = "retired_key_id_mismatch"
	reasonRetiredCurrent         = "retired_key_current"

	reasonServiceRunning  = "service_running"
	reasonRetiredConflict = "retired_key_conflict"
	reasonRotateCollision = "rotate_key_collision"
)

const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

type dirReasons struct{ unusable, notDirectory, foreignOwner, permissions string }

type fileReasons struct{ unusable, notRegular, foreignOwner, permissions, size string }

var (
	keysDirReasons    = dirReasons{reasonDirUnusable, reasonDirNotDirectory, reasonDirForeignOwner, reasonDirPermissions}
	retiredDirReasons = dirReasons{reasonRetiredDirUnusable, reasonRetiredDirNotDirectory, reasonRetiredDirForeignOwner, reasonRetiredDirPermissions}
	masterReasons     = fileReasons{reasonMasterUnusable, reasonMasterNotRegular, reasonMasterForeignOwner, reasonMasterPermissions, reasonMasterSize}
	retiredReasons    = fileReasons{reasonRetiredUnusable, reasonRetiredNotRegular, reasonRetiredForeignOwner, reasonRetiredPermissions, reasonRetiredSize}
)

type Refusal struct {
	Reason string
	detail string
}

func (r *Refusal) Error() string { return "keys: " + r.detail }

type Master struct {
	id string
	// Behind *string because fmt prints a nested *string as an address but a nested *[32]byte as its bytes.
	logRedact  *string
	chatHMAC   *string
	cursorSeal *string
	mref       *string
	auditChain *string
	retired    []*Master
}

func (m *Master) ID() string { return m.id }

func (m *Master) LogRedactKey() []byte { return []byte(*m.logRedact) }

func (m *Master) ChatHMACKey() []byte { return []byte(*m.chatHMAC) }

func (m *Master) CursorSealKey() []byte { return []byte(*m.cursorSeal) }

func (m *Master) MessageRefKey() []byte { return []byte(*m.mref) }

func (m *Master) AuditChainKey() []byte { return []byte(*m.auditChain) }

func (m *Master) Retired() []*Master { return slices.Clone(m.retired) }

func (m *Master) Lookup(id string) (*Master, bool) {
	if id == m.id {
		return m, true
	}
	for _, r := range m.retired {
		if r.id == id {
			return r, true
		}
	}
	return nil, false
}

type ownership struct {
	uid   int
	owner func(fs.FileInfo) (int, bool)
}

func (o *ownership) owns(fi fs.FileInfo) bool {
	got, ok := o.owner(fi)
	return ok && got == o.uid
}

func Load(dataDir string, uid int) (*Master, *Refusal) {
	return load(dataDir, uid, fileOwner)
}

func load(dataDir string, uid int, owner func(fs.FileInfo) (int, bool)) (*Master, *Refusal) {
	o := &ownership{uid: uid, owner: owner}
	dir := filepath.Join(dataDir, dirName)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, &Refusal{Reason: reasonDirUnusable, detail: keysLabel + " cannot be created: " + cause(err)}
	}
	if r := checkDir(dir, keysLabel, keysDirReasons, o); r != nil {
		return nil, r
	}
	secret, r := readOrCreate(dir, o)
	if r != nil {
		return nil, r
	}
	m, r := derive(secret)
	if r != nil {
		return nil, r
	}
	retiredDir := filepath.Join(dir, retiredName)
	if _, err := os.Lstat(retiredDir); errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if m.retired, r = loadRetired(retiredDir, retiredLabel, m.id, o); r != nil {
		return nil, r
	}
	return m, nil
}

func LoadFile(path string) (*Master, *Refusal) {
	secret, r := readSecret(path, "the master key file", masterReasons, nil)
	if r != nil {
		return nil, r
	}
	return derive(secret)
}

func LoadFiles(masterPath, retiredDir string) (*Master, *Refusal) {
	m, r := LoadFile(masterPath)
	if r != nil || retiredDir == "" {
		return m, r
	}
	if m.retired, r = loadRetired(retiredDir, retiredName+"/", m.id, nil); r != nil {
		return nil, r
	}
	return m, nil
}

func RetiredDirOf(masterPath string) string {
	return filepath.Join(filepath.Dir(masterPath), retiredName)
}

func Rotate(dataDir string, uid int, stopped func() error) (previous, current string, r *Refusal) {
	return rotate(dataDir, &ownership{uid: uid, owner: fileOwner}, stopped)
}

func rotate(dataDir string, o *ownership, stopped func() error) (string, string, *Refusal) {
	if err := stopped(); err != nil {
		return "", "", &Refusal{Reason: reasonServiceRunning, detail: "the master key is replaced only while the service is stopped: " + err.Error()}
	}
	dir := filepath.Join(dataDir, dirName)
	if r := checkDir(dir, keysLabel, keysDirReasons, o); r != nil {
		return "", "", r
	}
	path := filepath.Join(dir, masterName)
	secret, r := readSecret(path, masterLabel, masterReasons, o)
	if r != nil {
		return "", "", r
	}
	old, r := derive(secret)
	if r != nil {
		return "", "", r
	}
	retiredDir := filepath.Join(dir, retiredName)
	switch err := os.Mkdir(retiredDir, 0o700); {
	case err == nil:
		if err := syncDir(dir); err != nil {
			return "", "", &Refusal{Reason: reasonRetiredDirUnusable, detail: retiredLabel + " cannot be made durable: " + cause(err)}
		}
	case !errors.Is(err, fs.ErrExist):
		return "", "", &Refusal{Reason: reasonRetiredDirUnusable, detail: retiredLabel + " cannot be created: " + cause(err)}
	}
	retired, r := loadRetired(retiredDir, retiredLabel, "", o)
	if r != nil {
		return "", "", r
	}
	others := slices.DeleteFunc(retired, func(k *Master) bool { return k.id == old.id })
	if len(others) >= maxRetired {
		return "", "", &Refusal{Reason: reasonRetiredTooMany, detail: retiredLabel + " already holds 32 retired keys, the most a start accepts"}
	}
	next := newSecret()
	fresh, r := derive(next)
	if r != nil {
		return "", "", r
	}
	if fresh.id == old.id || slices.ContainsFunc(others, func(k *Master) bool { return k.id == fresh.id }) {
		return "", "", &Refusal{Reason: reasonRotateCollision, detail: "the new master key's id is already in use; run the rotation again"}
	}
	if r := retire(retiredDir, old.id, secret, o); r != nil {
		return "", "", r
	}
	if err := replace(dir, path, next); err != nil {
		return "", "", &Refusal{Reason: reasonMasterUnusable, detail: masterLabel + " cannot be replaced: " + cause(err)}
	}
	return old.id, fresh.id, nil
}

func retire(dir, id string, secret []byte, o *ownership) *Refusal {
	label := retiredLabel + id
	path := filepath.Join(dir, id)
	switch err := link(dir, path, secret); {
	case errors.Is(err, fs.ErrExist):
		kept, r := readSecret(path, label, retiredReasons, o)
		if r != nil {
			return r
		}
		if subtle.ConstantTimeCompare(kept, secret) != 1 {
			return &Refusal{Reason: reasonRetiredConflict, detail: label + " holds another key than the current master key"}
		}
	case err != nil:
		return &Refusal{Reason: reasonRetiredUnusable, detail: label + " cannot be written: " + cause(err)}
	}
	if err := syncDir(dir); err != nil {
		return &Refusal{Reason: reasonRetiredUnusable, detail: label + " cannot be made durable: " + cause(err)}
	}
	return nil
}

func checkDir(dir, label string, rs dirReasons, o *ownership) *Refusal {
	stat := os.Lstat
	if o == nil {
		stat = os.Stat
	}
	fi, err := stat(dir)
	switch {
	case err != nil:
		return &Refusal{Reason: rs.unusable, detail: label + " cannot be inspected: " + cause(err)}
	case !fi.IsDir():
		return &Refusal{Reason: rs.notDirectory, detail: label + " is not a directory"}
	case o == nil:
	case !o.owns(fi):
		return &Refusal{Reason: rs.foreignOwner, detail: label + " is not owned by the current user"}
	case fi.Mode()&(fs.ModePerm|specialBits) != 0o700:
		return &Refusal{Reason: rs.permissions, detail: label + " must have mode 0700: full access for its owner, none for group or others, and no setuid, setgid or sticky bit"}
	}
	return nil
}

func loadRetired(dir, label, current string, o *ownership) ([]*Master, *Refusal) {
	if r := checkDir(dir, label, retiredDirReasons, o); r != nil {
		return nil, r
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, &Refusal{Reason: reasonRetiredDirUnusable, detail: label + " cannot be read: " + cause(err)}
	}
	var names []string
	for _, e := range list {
		switch name := e.Name(); {
		case strings.HasPrefix(name, "."):
		case !isKeyID(name):
			return nil, &Refusal{Reason: reasonRetiredName, detail: label + " holds an entry whose name is not a key id of 8 lower-case hexadecimal digits"}
		default:
			names = append(names, name)
		}
	}
	if len(names) > maxRetired {
		return nil, &Refusal{Reason: reasonRetiredTooMany, detail: label + " holds more than 32 retired keys"}
	}
	retired := make([]*Master, 0, len(names))
	for _, name := range names {
		secret, r := readSecret(filepath.Join(dir, name), label+name, retiredReasons, o)
		if r != nil {
			return nil, r
		}
		k, r := derive(secret)
		switch {
		case r != nil:
			return nil, r
		case k.id != name:
			return nil, &Refusal{Reason: reasonRetiredIDMismatch, detail: label + name + " holds a key whose id is not its name"}
		case k.id == current:
			return nil, &Refusal{Reason: reasonRetiredCurrent, detail: label + name + " holds the current master key"}
		}
		retired = append(retired, k)
	}
	return retired, nil
}

func isKeyID(s string) bool {
	if len(s) != 2*idSize {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func readOrCreate(dir string, o *ownership) ([]byte, *Refusal) {
	path := filepath.Join(dir, masterName)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if r := create(dir, path); r != nil {
			return nil, r
		}
	}
	return readSecret(path, masterLabel, masterReasons, o)
}

func readSecret(path, label string, rs fileReasons, o *ownership) ([]byte, *Refusal) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if o != nil {
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, &Refusal{Reason: rs.unusable, detail: label + " cannot be inspected: " + cause(err)}
		}
		if !fi.Mode().IsRegular() {
			return nil, &Refusal{Reason: rs.notRegular, detail: label + " is not a regular file"}
		}
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(filepath.Clean(path), flags, 0)
	if err != nil {
		return nil, &Refusal{Reason: rs.unusable, detail: label + " cannot be opened: " + cause(err)}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, &Refusal{Reason: rs.unusable, detail: label + " cannot be inspected: " + cause(err)}
	case !fi.Mode().IsRegular():
		return nil, &Refusal{Reason: rs.notRegular, detail: label + " is not a regular file"}
	case o == nil:
	case !o.owns(fi):
		return nil, &Refusal{Reason: rs.foreignOwner, detail: label + " is not owned by the current user"}
	case fi.Mode()&(0o077|specialBits) != 0:
		return nil, &Refusal{Reason: rs.permissions, detail: label + " must grant no access to group or others and have no setuid, setgid or sticky bit"}
	}
	secret, err := io.ReadAll(io.LimitReader(f, masterSize+1))
	if err != nil {
		return nil, &Refusal{Reason: rs.unusable, detail: label + " cannot be read: " + cause(err)}
	}
	if len(secret) != masterSize {
		return nil, &Refusal{Reason: rs.size, detail: label + " must hold exactly 32 bytes"}
	}
	return secret, nil
}

func create(dir, path string) *Refusal {
	if err := link(dir, path, newSecret()); err != nil && !errors.Is(err, fs.ErrExist) {
		return &Refusal{Reason: reasonMasterUnusable, detail: masterLabel + " cannot be created: " + cause(err)}
	}
	if err := syncDir(dir); err != nil {
		return &Refusal{Reason: reasonMasterUnusable, detail: masterLabel + " cannot be made durable: " + cause(err)}
	}
	return nil
}

func link(dir, path string, secret []byte) error {
	tmp, err := writeTemp(dir, filepath.Base(path), secret)
	if err == nil {
		err = os.Link(tmp, path)
	}
	_ = os.Remove(tmp)
	return err
}

func replace(dir, path string, secret []byte) error {
	tmp, err := writeTemp(dir, filepath.Base(path), secret)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func writeTemp(dir, name string, secret []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(secret)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	return tmp.Name(), err
}

var newSecret = func() []byte {
	secret := make([]byte, masterSize)
	rand.Read(secret)
	return secret
}

var syncDir = func(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}

func derive(secret []byte) (*Master, *Refusal) {
	id, idErr := hkdf.Key(sha256.New, secret, nil, idInfo, idSize)
	if idErr != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: masterLabel + ": no key can be derived from it"}
	}
	m := &Master{id: hex.EncodeToString(id)}
	for _, purpose := range []struct {
		info string
		key  **string
	}{
		{logInfo, &m.logRedact}, {chatInfo, &m.chatHMAC}, {cursorInfo, &m.cursorSeal}, {mrefInfo, &m.mref}, {auditInfo, &m.auditChain},
	} {
		k, err := hkdf.Key(sha256.New, secret, nil, purpose.info, keySize)
		if err != nil {
			return nil, &Refusal{Reason: reasonMasterUnusable, detail: masterLabel + ": no key can be derived from it"}
		}
		text := string(k)
		*purpose.key = &text
	}
	return m, nil
}

func fileOwner(fi fs.FileInfo) (int, bool) {
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
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err.Error()
	}
	return "unexpected error"
}
