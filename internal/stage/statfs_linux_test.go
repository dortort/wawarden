package stage

import (
	"syscall"
	"testing"
)

func TestAvailableBytesUnits(t *testing.T) {
	if got := availableBytes(&syscall.Statfs_t{Bavail: 3, Bsize: 1 << 20, Frsize: 512}); got != 1536 {
		t.Fatalf("availableBytes with a fragment size = %d, want 1536", got)
	}
	if got := availableBytes(&syscall.Statfs_t{Bavail: 3, Bsize: 512}); got != 1536 {
		t.Fatalf("availableBytes without a fragment size = %d, want 1536", got)
	}
}
