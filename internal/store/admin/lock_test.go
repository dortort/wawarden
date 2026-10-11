package admin_test

import (
	"testing"

	"github.com/dortort/wawarden/internal/store/admin"
)

func TestArchiveUnlockedOnlyOnceTheStoreIsClosed(t *testing.T) {
	f := open(t)
	if err := admin.ArchiveUnlocked(t.Context(), f.dir); err == nil {
		t.Fatal("ArchiveUnlocked while the store holds the archive = nil, want an error")
	}
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := admin.ArchiveUnlocked(t.Context(), f.dir); err != nil {
		t.Fatalf("ArchiveUnlocked after Close = %v, want nil", err)
	}
	if err := admin.ArchiveUnlocked(t.Context(), t.TempDir()); err != nil {
		t.Fatalf("ArchiveUnlocked without an archive = %v, want nil", err)
	}
}
