package stage

import "syscall"

func availableBytes(st *syscall.Statfs_t) uint64 {
	unit := st.Frsize
	if unit <= 0 {
		unit = st.Bsize
	}
	return st.Bavail * uint64(max(unit, 0))
}
