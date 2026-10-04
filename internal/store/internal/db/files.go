package db

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

func checkFilesystem(dir string, profile Profile, statfs func(string, *syscall.Statfs_t) error) *Refusal {
	var st syscall.Statfs_t
	if err := statfs(dir, &st); err != nil {
		return &Refusal{Reason: "storage_filesystem_unknown", detail: "the data directory's filesystem cannot be inspected: " + cause(err)}
	}
	if profile == Local && networkFilesystem(&st) {
		return &Refusal{Reason: "storage_network_filesystem", detail: "the data directory is on a network filesystem, which storage profile local does not support: set WAWARDEN_STORAGE_PROFILE=nfs"}
	}
	return nil
}

func prepareFiles(name Name, path string, uid int, owner func(fs.FileInfo) (int, bool)) *Refusal {
	file, journal := name.file(), name.file()+"-journal"
	prefix, journalPrefix := name.label()+"_db_", name.label()+"_journal_"
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err = create(path); err == nil {
			fi, err = os.Lstat(path)
		}
	}
	if err != nil {
		return &Refusal{Reason: prefix + "unusable", detail: file + " cannot be created or inspected: " + cause(err)}
	}
	if r := checkFile(fi, uid, owner, prefix, file); r != nil {
		return r
	}
	fi, err = os.Lstat(path + "-journal")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return &Refusal{Reason: journalPrefix + "unusable", detail: journal + " cannot be inspected: " + cause(err)}
	}
	return checkFile(fi, uid, owner, journalPrefix, journal)
}

func checkFile(fi fs.FileInfo, uid int, owner func(fs.FileInfo) (int, bool), prefix, file string) *Refusal {
	got, ok := owner(fi)
	switch {
	case !fi.Mode().IsRegular():
		return &Refusal{Reason: prefix + "not_regular", detail: file + " is not a regular file"}
	case !ok || got != uid:
		return &Refusal{Reason: prefix + "foreign_owner", detail: file + " is not owned by the current user"}
	case fi.Mode()&(0o077|specialBits) != 0:
		return &Refusal{Reason: prefix + "permissions", detail: file + " must grant no access to group or others and have no setuid, setgid or sticky bit"}
	}
	return nil
}

func create(path string) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
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
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno.Error()
	}
	return "unexpected error"
}
