package db

import "syscall"

const mntLocal = 0x1000

func networkFilesystem(st *syscall.Statfs_t) bool { return st.Flags&mntLocal == 0 }

func availableBytes(st *syscall.Statfs_t) uint64 { return st.Bavail * uint64(st.Bsize) }
