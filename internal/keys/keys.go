// Package keys owns the keys directory of the data directory: it creates and checks the master key and derives every purpose key from it.
package keys

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	dirName    = "keys"
	masterName = "master"
	masterSize = 32
	keySize    = 32
	idSize     = 4
	infoPrefix = "wawarden/v1/"
	idInfo     = infoPrefix + "key-id"
	logInfo    = infoPrefix + "log-redact"
	chatInfo   = infoPrefix + "chat-hmac"
	cursorInfo = infoPrefix + "cursor-seal"
	mrefInfo   = infoPrefix + "mref"
	auditInfo  = infoPrefix + "audit-chain"
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
)

const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

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
}

func (m *Master) ID() string { return m.id }

func (m *Master) LogRedactKey() []byte { return []byte(*m.logRedact) }

func (m *Master) ChatHMACKey() []byte { return []byte(*m.chatHMAC) }

func (m *Master) CursorSealKey() []byte { return []byte(*m.cursorSeal) }

func (m *Master) MessageRefKey() []byte { return []byte(*m.mref) }

func (m *Master) AuditChainKey() []byte { return []byte(*m.auditChain) }

func Load(dataDir string, uid int) (*Master, *Refusal) {
	return load(dataDir, uid, fileOwner)
}

func load(dataDir string, uid int, owner func(fs.FileInfo) (int, bool)) (*Master, *Refusal) {
	dir := filepath.Join(dataDir, dirName)
	if r := checkDir(dir, uid, owner); r != nil {
		return nil, r
	}
	secret, r := readOrCreate(dir, uid, owner)
	if r != nil {
		return nil, r
	}
	return derive(secret)
}

func LoadFile(path string) (*Master, *Refusal) {
	const name = "the master key file"
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be opened: " + cause(err)}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be inspected: " + cause(err)}
	case !fi.Mode().IsRegular():
		return nil, &Refusal{Reason: reasonMasterNotRegular, detail: name + " is not a regular file"}
	}
	secret, err := io.ReadAll(io.LimitReader(f, masterSize+1))
	if err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be read: " + cause(err)}
	}
	if len(secret) != masterSize {
		return nil, &Refusal{Reason: reasonMasterSize, detail: name + " must hold exactly 32 bytes"}
	}
	return derive(secret)
}

func checkDir(dir string, uid int, owner func(fs.FileInfo) (int, bool)) *Refusal {
	const name = dirName + "/"
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return &Refusal{Reason: reasonDirUnusable, detail: name + " cannot be created: " + cause(err)}
	}
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		return &Refusal{Reason: reasonDirUnusable, detail: name + " cannot be inspected: " + cause(err)}
	case !fi.IsDir():
		return &Refusal{Reason: reasonDirNotDirectory, detail: name + " is not a directory"}
	case !ownedBy(fi, uid, owner):
		return &Refusal{Reason: reasonDirForeignOwner, detail: name + " is not owned by the current user"}
	case fi.Mode()&(fs.ModePerm|specialBits) != 0o700:
		return &Refusal{Reason: reasonDirPermissions, detail: name + " must have mode 0700: full access for its owner, none for group or others, and no setuid, setgid or sticky bit"}
	}
	return nil
}

func readOrCreate(dir string, uid int, owner func(fs.FileInfo) (int, bool)) ([]byte, *Refusal) {
	const name = dirName + "/" + masterName
	path := filepath.Join(dir, masterName)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if r := create(dir, path); r != nil {
			return nil, r
		}
		fi, err = os.Lstat(path)
	}
	if err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be inspected: " + cause(err)}
	}
	if !fi.Mode().IsRegular() {
		return nil, &Refusal{Reason: reasonMasterNotRegular, detail: name + " is not a regular file"}
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be opened: " + cause(err)}
	}
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be inspected: " + cause(err)}
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, &Refusal{Reason: reasonMasterNotRegular, detail: name + " is not a regular file"}
	case !ownedBy(fi, uid, owner):
		return nil, &Refusal{Reason: reasonMasterForeignOwner, detail: name + " is not owned by the current user"}
	case fi.Mode()&(0o077|specialBits) != 0:
		return nil, &Refusal{Reason: reasonMasterPermissions, detail: name + " must grant no access to group or others and have no setuid, setgid or sticky bit"}
	}
	secret, err := io.ReadAll(io.LimitReader(f, masterSize+1))
	if err != nil {
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be read: " + cause(err)}
	}
	if len(secret) != masterSize {
		return nil, &Refusal{Reason: reasonMasterSize, detail: name + " must hold exactly 32 bytes"}
	}
	return secret, nil
}

func create(dir, path string) *Refusal {
	const name = dirName + "/" + masterName
	tmp, err := os.CreateTemp(dir, "."+masterName+"-*")
	if err != nil {
		return &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be created: " + cause(err)}
	}
	secret := make([]byte, masterSize)
	rand.Read(secret)
	_, err = tmp.Write(secret)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Link(tmp.Name(), path)
	}
	_ = os.Remove(tmp.Name())
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be created: " + cause(err)}
	}
	if err := syncDir(dir); err != nil {
		return &Refusal{Reason: reasonMasterUnusable, detail: name + " cannot be made durable: " + cause(err)}
	}
	return nil
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
		return nil, &Refusal{Reason: reasonMasterUnusable, detail: dirName + "/" + masterName + ": no key can be derived from it"}
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
			return nil, &Refusal{Reason: reasonMasterUnusable, detail: dirName + "/" + masterName + ": no key can be derived from it"}
		}
		text := string(k)
		*purpose.key = &text
	}
	return m, nil
}

func ownedBy(fi fs.FileInfo, uid int, owner func(fs.FileInfo) (int, bool)) bool {
	got, ok := owner(fi)
	return ok && got == uid
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
