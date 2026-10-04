package db

import (
	"syscall"
	"testing"
)

const statfsUnit = 4096

func networkStatfs(st *syscall.Statfs_t) { st.Type = 0x6969 }

func localStatfs(st *syscall.Statfs_t) {
	st.Type = 0xef53
	st.Bsize, st.Frsize = 1<<20, statfsUnit
}

func withType(magic uint32) *syscall.Statfs_t {
	var st syscall.Statfs_t
	setType(&st.Type, magic)
	return &st
}

func setType[T int32 | int64](field *T, magic uint32) { *field = T(magic) }

func TestNetworkFilesystems(t *testing.T) {
	for magic, name := range map[uint32]string{
		0x6969: "nfs", 0x517b: "smb", 0xff534d42: "cifs", 0xfe534d42: "smb2", 0x00c36400: "ceph",
		0x5346414f: "afs", 0x6b414653: "kafs", 0x73757245: "coda", 0x564c: "ncp", 0x01021997: "9p",
	} {
		if !networkFilesystem(withType(magic)) {
			t.Errorf("%s (%#x) is not recognised as a network filesystem", name, magic)
		}
	}
	for magic, name := range map[uint32]string{
		0xef53: "ext4", 0x58465342: "xfs", 0x9123683e: "btrfs", 0x01021994: "tmpfs", 0x794c7630: "overlayfs", 0x65735546: "fuse",
	} {
		if networkFilesystem(withType(magic)) {
			t.Errorf("%s (%#x) is taken for a network filesystem", name, magic)
		}
	}
}

func TestAvailableBytesFallsBackToTheBlockSize(t *testing.T) {
	if got := availableBytes(&syscall.Statfs_t{Bavail: 3, Bsize: 512}); got != 1536 {
		t.Fatalf("availableBytes without a fragment size = %d, want 1536", got)
	}
}
