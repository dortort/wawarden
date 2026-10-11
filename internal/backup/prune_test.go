package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestTheSpacePreflightNeedsTheFloorTheLargerCopyAndBothCopies(t *testing.T) {
	a, s := fakeArchive(), fakeSession()
	const floor = 4096
	var need uint64
	if sum := floor + max(a.size, s.size) + a.size + s.size; sum > 0 {
		need = uint64(sum)
	}
	for _, tt := range []struct {
		name   string
		space  func() (uint64, uint64, error)
		failed bool
	}{
		{name: "exactly enough", space: func() (uint64, uint64, error) { return need, floor, nil }},
		{name: "one byte short", space: func() (uint64, uint64, error) { return need - 1, floor, nil }, failed: true},
		{name: "no floor on this profile", space: func() (uint64, uint64, error) { return 0, 0, nil }},
		{name: "a floor near the top of the range", space: func() (uint64, uint64, error) { return 1<<64 - 1, 1<<64 - 2, nil }, failed: true},
		{name: "no space function"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tk, rec := taker(t, dir, identity(t).Recipient().String(), a, s)
			tk.space = tt.space
			name, err := tk.Take(t.Context())
			if tt.failed {
				if err == nil || !errors.Is(err, errSpace) || !slices.Equal(rec.failed, []string{reasonSpace}) {
					t.Fatalf("Take = %q, %v with events %v, want backup_failed space", name, err, rec.failed)
				}
				if _, err := os.Lstat(filepath.Join(dir, Dir)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a refused backup touched backups/: %v", err)
				}
				return
			}
			if err != nil || name != backupName || len(rec.done) != 1 {
				t.Fatalf("Take = %q, %v with events %v", name, err, rec.failed)
			}
		})
	}
}

func TestASizeThatCannotBeReadFailsThePreflight(t *testing.T) {
	a := fakeArchive()
	a.fail = errors.New("synthetic failure")
	tk, rec := taker(t, t.TempDir(), identity(t).Recipient().String(), a, fakeSession())
	tk.space = func() (uint64, uint64, error) { return 1 << 40, 1, nil }
	if _, err := tk.Take(t.Context()); err == nil || !slices.Equal(rec.failed, []string{reasonSpace}) {
		t.Fatalf("Take = %v with events %v, want backup_failed space", err, rec.failed)
	}
}

func stamp(days int) string {
	return epoch.AddDate(0, 0, days).Format(nameLayout) + fileSuffix
}

func writeBackups(t *testing.T, dir string, names ...string) string {
	t.Helper()
	backups := filepath.Join(dir, Dir)
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		path := filepath.Join(backups, name)
		if err := os.WriteFile(path, []byte("synthetic backup"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return backups
}

func chmod(t *testing.T, path string, m fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, m); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	got := entries(t, dir)
	slices.Sort(got)
	return got
}

func TestPruneKeepsTheNewestByNameWhateverTheModificationTimes(t *testing.T) {
	dir := t.TempDir()
	all := []string{stamp(0), stamp(1), stamp(2), stamp(3), stamp(4)}
	backups := writeBackups(t, dir, all...)
	for i, name := range all {
		at := epoch.AddDate(0, 0, 10-i)
		if err := os.Chtimes(filepath.Join(backups, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	syncs := 0
	tk.syncDir = func(d string) error {
		syncs++
		return syncDir(d)
	}
	removed, kept, err := tk.Prune(t.Context(), 2, stamp(4))
	if err != nil || removed != 3 || kept != 2 {
		t.Fatalf("Prune = %d removed, %d kept, %v", removed, kept, err)
	}
	if got := names(t, backups); !slices.Equal(got, all[3:]) {
		t.Fatalf("backups/ holds %v, want %v", got, all[3:])
	}
	if syncs != 1 || !slices.Equal(rec.pruned, [][2]int{{3, 2}}) || len(rec.pruneFailed) != 0 {
		t.Fatalf("%d directory syncs, events %v and %v, want one sync and backup_pruned 3 2", syncs, rec.pruned, rec.pruneFailed)
	}
}

func TestPruneKeepsTheProtectedBackupWhenTheClockWentBack(t *testing.T) {
	dir := t.TempDir()
	backups := writeBackups(t, dir, stamp(-5), stamp(1), stamp(2), stamp(3))
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	removed, kept, err := tk.Prune(t.Context(), 2, stamp(-5))
	if err != nil || removed != 1 || kept != 3 {
		t.Fatalf("Prune = %d removed, %d kept, %v", removed, kept, err)
	}
	if got := names(t, backups); !slices.Equal(got, []string{stamp(-5), stamp(2), stamp(3)}) {
		t.Fatalf("backups/ holds %v", got)
	}
	if !slices.Equal(rec.pruned, [][2]int{{1, 3}}) {
		t.Fatalf("events %v", rec.pruned)
	}
}

func TestPruneAfterRealBackupsWithAMovingClock(t *testing.T) {
	dir := t.TempDir()
	now := epoch
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	tk.now = func() time.Time { return now }
	var taken []string
	for day := range 4 {
		now = epoch.AddDate(0, 0, day)
		name, err := tk.Take(t.Context())
		if err != nil {
			t.Fatalf("Take on day %d: %v", day, err)
		}
		taken = append(taken, name)
		if _, _, err := tk.Prune(t.Context(), 2, name); err != nil {
			t.Fatalf("Prune: %v", err)
		}
	}
	backups := filepath.Join(dir, Dir)
	if got := names(t, backups); !slices.Equal(got, taken[2:]) {
		t.Fatalf("backups/ holds %v, want %v", got, taken[2:])
	}
	now = epoch.AddDate(0, 0, -1)
	name, err := tk.Take(t.Context())
	if err != nil {
		t.Fatalf("Take with the clock set back: %v", err)
	}
	if _, kept, err := tk.Prune(t.Context(), 2, name); err != nil || kept != 3 {
		t.Fatalf("Prune after a backwards clock kept %d, %v", kept, err)
	}
	if got := names(t, backups); !slices.Equal(got, []string{name, taken[2], taken[3]}) {
		t.Fatalf("backups/ holds %v", got)
	}
	if !slices.Equal(rec.pruned, [][2]int{{1, 2}, {1, 2}}) || len(rec.done) != 5 {
		t.Fatalf("events pruned %v, %d backups done", rec.pruned, len(rec.done))
	}
}

func TestAFailedBackupNeverDeletesAnEarlierOne(t *testing.T) {
	dir := t.TempDir()
	earlier := []string{stamp(-3), stamp(-2), stamp(-1)}
	backups := writeBackups(t, dir, earlier...)
	a := fakeArchive()
	a.fail = errors.New("synthetic failure")
	tk, rec := taker(t, dir, identity(t).Recipient().String(), a, fakeSession())
	if _, err := tk.Take(t.Context()); err == nil {
		t.Fatal("Take succeeded")
	}
	if got := names(t, backups); !slices.Equal(got, earlier) || len(rec.pruned) != 0 {
		t.Fatalf("backups/ holds %v after a failed backup, events %v", got, rec.pruned)
	}
}

func TestPruneLeavesTheStagingDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	backups := writeBackups(t, dir, stamp(0), stamp(1), stamp(2))
	staged := filepath.Join(backups, stagingDir, "archive"+stageSuffix)
	if err := os.Mkdir(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, _ := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	if removed, kept, err := tk.Prune(t.Context(), 1, stamp(2)); err != nil || removed != 2 || kept != 1 {
		t.Fatalf("Prune = %d removed, %d kept, %v", removed, kept, err)
	}
	if got := names(t, backups); !slices.Equal(got, []string{stamp(2), stagingDir, filepath.Join(stagingDir, "archive"+stageSuffix)}) {
		t.Fatalf("backups/ holds %v", got)
	}
}

func TestPruneWithFewerBackupsThanKeptDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	backups := writeBackups(t, dir, stamp(0), stamp(1))
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	syncs := 0
	tk.syncDir = func(string) error {
		syncs++
		return nil
	}
	if removed, kept, err := tk.Prune(t.Context(), 14, stamp(1)); err != nil || removed != 0 || kept != 2 {
		t.Fatalf("Prune = %d removed, %d kept, %v", removed, kept, err)
	}
	if got := names(t, backups); len(got) != 2 || syncs != 0 || len(rec.pruned) != 0 || len(rec.pruneFailed) != 0 {
		t.Fatalf("backups/ holds %v, %d syncs, events %v %v", got, syncs, rec.pruned, rec.pruneFailed)
	}
}

func TestPruneWithoutABackupsDirectoryCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	if removed, kept, err := tk.Prune(t.Context(), 1, ""); err != nil || removed != 0 || kept != 0 {
		t.Fatalf("Prune = %d removed, %d kept, %v", removed, kept, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, Dir)); !errors.Is(err, fs.ErrNotExist) || len(rec.pruned)+len(rec.pruneFailed) != 0 {
		t.Fatalf("Prune created backups/ (%v) or reported %v %v", err, rec.pruned, rec.pruneFailed)
	}
}

func TestPruneRefusesToKeepNothing(t *testing.T) {
	dir := t.TempDir()
	backups := writeBackups(t, dir, stamp(0))
	tk, _ := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	for _, keep := range []int{0, -1} {
		if _, _, err := tk.Prune(t.Context(), keep, ""); !errors.Is(err, errKeep) {
			t.Fatalf("Prune keeping %d = %v", keep, err)
		}
	}
	if got := names(t, backups); len(got) != 1 {
		t.Fatalf("backups/ holds %v", got)
	}
}

type foreignInfo struct {
	fs.FileInfo
	sys *syscall.Stat_t
}

func (f foreignInfo) Sys() any { return f.sys }

func TestAnUnexpectedEntryAbortsTheWholePrune(t *testing.T) {
	old := []string{stamp(0), stamp(1), stamp(2), stamp(3)}
	for _, tt := range []struct {
		name    string
		reason  string
		uid     int
		foreign string
		add     func(t *testing.T, backups string)
	}{
		{name: "a symbolic link with a backup name", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			if err := os.Symlink(filepath.Join(backups, stamp(0)), filepath.Join(backups, stamp(9))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a backup readable by others", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			chmod(t, filepath.Join(backups, stamp(1)), 0o644)
		}},
		{name: "a backup with the setuid bit", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			chmod(t, filepath.Join(backups, stamp(1)), 0o600|fs.ModeSetuid)
		}},
		{name: "another directory", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			mkdirMode(t, filepath.Join(backups, "restore"), 0o700)
		}},
		{name: "a directory with a backup name", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			mkdirMode(t, filepath.Join(backups, stamp(9)), 0o700)
		}},
		{name: "a staging directory open to others", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			mkdirMode(t, filepath.Join(backups, stagingDir), 0o755)
		}},
		{name: "a staging file in place of the directory", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			if err := os.WriteFile(filepath.Join(backups, stagingDir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a name with an impossible date", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), "20261332T000000Z.age")
		}},
		{name: "a name with an impossible time", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), "20261005T246000Z.age")
		}},
		{name: "a copy kept beside the backups", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), stamp(0)+".bak")
		}},
		{name: "a lowercase suffix variant", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), "20261009T120000z.age")
		}},
		{name: "a hidden file", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), ".keep")
		}},
		{name: "a staged member left at the top", reason: reasonUnexpected, add: func(t *testing.T, backups string) {
			writeBackups(t, filepath.Dir(backups), "archive"+stageSuffix)
		}},
		{name: "files owned by another user", reason: reasonDirectory, uid: os.Geteuid() + 1},
		{name: "a backup owned by another user", reason: reasonUnexpected, foreign: stamp(1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			backups := writeBackups(t, dir, old...)
			if tt.add != nil {
				tt.add(t, backups)
			}
			before := names(t, backups)
			tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
			if tt.uid != 0 {
				tk.uid = tt.uid
			}
			if tt.foreign != "" {
				tk.lstat = func(path string) (fs.FileInfo, error) {
					fi, err := os.Lstat(path)
					if err != nil || filepath.Base(path) != tt.foreign {
						return fi, err
					}
					st := *fi.Sys().(*syscall.Stat_t)
					st.Uid++
					return foreignInfo{FileInfo: fi, sys: &st}, nil
				}
			}
			removed, _, err := tk.Prune(t.Context(), 1, stamp(3))
			if err == nil || removed != 0 {
				t.Fatalf("Prune = %d removed, %v, want a refusal", removed, err)
			}
			if got := names(t, backups); !slices.Equal(got, before) {
				t.Fatalf("backups/ holds %v, want %v untouched", got, before)
			}
			if !slices.Equal(rec.pruneFailed, []string{tt.reason}) || len(rec.pruned) != 0 {
				t.Fatalf("events %v %v, want backup_prune_failed %s", rec.pruned, rec.pruneFailed, tt.reason)
			}
		})
	}
}

func TestAFailedRemoveOrSyncIsReported(t *testing.T) {
	synthetic := errors.New("synthetic failure")
	for _, tt := range []struct {
		name    string
		hook    func(tk *Taker)
		reason  string
		removed int
	}{
		{name: "remove", reason: reasonRemove, removed: 1, hook: func(tk *Taker) {
			calls := 0
			tk.remove = func(path string) error {
				if calls++; calls == 2 {
					return synthetic
				}
				return os.Remove(path)
			}
		}},
		{name: "sync", reason: reasonSync, removed: 3, hook: func(tk *Taker) {
			tk.syncDir = func(string) error { return synthetic }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBackups(t, dir, stamp(0), stamp(1), stamp(2), stamp(3))
			tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
			tt.hook(tk)
			removed, _, err := tk.Prune(t.Context(), 1, stamp(3))
			if !errors.Is(err, synthetic) || removed != tt.removed {
				t.Fatalf("Prune = %d removed, %v", removed, err)
			}
			if !slices.Equal(rec.pruneFailed, []string{tt.reason}) || len(rec.pruned) != 0 {
				t.Fatalf("events %v %v, want backup_prune_failed %s", rec.pruned, rec.pruneFailed, tt.reason)
			}
		})
	}
}

func TestAStopDuringPruneEndsItWithoutAnEvent(t *testing.T) {
	dir := t.TempDir()
	backups := writeBackups(t, dir, stamp(0), stamp(1), stamp(2), stamp(3))
	tk, rec := taker(t, dir, identity(t).Recipient().String(), fakeArchive(), fakeSession())
	ctx, cancel := context.WithCancel(t.Context())
	tk.remove = func(path string) error {
		cancel()
		return os.Remove(path)
	}
	removed, _, err := tk.Prune(ctx, 1, stamp(3))
	if !errors.Is(err, context.Canceled) || removed != 1 {
		t.Fatalf("Prune = %d removed, %v", removed, err)
	}
	if got := names(t, backups); len(got) != 3 || len(rec.pruned)+len(rec.pruneFailed) != 0 {
		t.Fatalf("backups/ holds %v, events %v %v", got, rec.pruned, rec.pruneFailed)
	}
}
