package db

import "syscall"

var networkMagic = map[uint32]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x00c36400: "ceph",
	0x5346414f: "afs",
	0x6b414653: "kafs",
	0x73757245: "coda",
	0x564c:     "ncp",
	0x01021997: "9p",
}

func networkFilesystem(st *syscall.Statfs_t) bool {
	_, ok := networkMagic[uint32(st.Type)] //nolint:gosec // G115: f_type is a 32-bit magic number that 64-bit platforms widen
	return ok
}

func availableBytes(st *syscall.Statfs_t) uint64 {
	unit := st.Frsize
	if unit <= 0 {
		unit = st.Bsize
	}
	return st.Bavail * uint64(max(unit, 0))
}
