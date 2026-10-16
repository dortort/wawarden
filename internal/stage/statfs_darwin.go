package stage

import "syscall"

func availableBytes(st *syscall.Statfs_t) uint64 { return st.Bavail * uint64(st.Bsize) }
