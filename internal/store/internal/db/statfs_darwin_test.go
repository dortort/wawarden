package db

import (
	"syscall"
	"testing"
)

const statfsUnit = 4096

func networkStatfs(st *syscall.Statfs_t) { st.Flags = 0 }

func localStatfs(st *syscall.Statfs_t) {
	st.Flags = mntLocal
	st.Bsize = statfsUnit
}

func TestNetworkFilesystems(t *testing.T) {
	if !networkFilesystem(&syscall.Statfs_t{}) {
		t.Error("a mount without MNT_LOCAL is not recognised as a network filesystem")
	}
	if networkFilesystem(&syscall.Statfs_t{Flags: mntLocal | 0x1}) {
		t.Error("a mount with MNT_LOCAL is taken for a network filesystem")
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(t.TempDir(), &st); err != nil || networkFilesystem(&st) {
		t.Fatalf("the test's temporary directory is taken for a network filesystem (%v)", err)
	}
}
