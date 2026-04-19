package filesync

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestIsFDExhaustion(t *testing.T) {
	t.Parallel()
	if !isFDExhaustion(syscall.EMFILE) {
		t.Error("EMFILE should be flagged as FD exhaustion")
	}
	if !isFDExhaustion(syscall.ENFILE) {
		t.Error("ENFILE should be flagged as FD exhaustion")
	}
	if isFDExhaustion(syscall.EPERM) {
		t.Error("EPERM must not be flagged as FD exhaustion")
	}
	if isFDExhaustion(nil) {
		t.Error("nil must not be flagged as FD exhaustion")
	}
	// Wrapped errors must still be detected via errors.Is.
	wrapped := &os.PathError{Op: "open", Path: "/x", Err: syscall.EMFILE}
	if !isFDExhaustion(wrapped) {
		t.Error("wrapped EMFILE should be flagged")
	}
}

func TestFolderWatcher_IsRoot(t *testing.T) {
	t.Parallel()
	fw := &folderWatcher{roots: []string{"/a/b", "/c/d"}}
	cases := []struct {
		in   string
		want bool
	}{
		{"/a/b", true},
		{"/c/d", true},
		{"/a/b/sub", false},
		{"/e", false},
		{"", false},
	}
	for _, c := range cases {
		if got := fw.isRoot(c.in); got != c.want {
			t.Errorf("isRoot(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFolderWatcher_DrainDirtyRoots(t *testing.T) {
	t.Parallel()
	fw := &folderWatcher{dirtyRoots: make(map[string]bool)}

	// First drain on empty set is nil (contract: nothing changed).
	if got := fw.drainDirtyRoots(); got != nil {
		t.Errorf("empty drain = %v, want nil", got)
	}

	fw.dirtyMu.Lock()
	fw.dirtyRoots["/a"] = true
	fw.dirtyRoots["/b"] = true
	fw.dirtyMu.Unlock()

	got := fw.drainDirtyRoots()
	if len(got) != 2 || !got["/a"] || !got["/b"] {
		t.Fatalf("drain = %v", got)
	}

	// Second drain must be nil — internal set was reset.
	if got2 := fw.drainDirtyRoots(); got2 != nil {
		t.Errorf("drain after reset = %v, want nil", got2)
	}

	// Mutating the returned map must not affect subsequent drains
	// (the caller owns the returned map).
	fw.dirtyMu.Lock()
	fw.dirtyRoots["/c"] = true
	fw.dirtyMu.Unlock()
	got3 := fw.drainDirtyRoots()
	delete(got3, "/c")
	fw.dirtyMu.Lock()
	if len(fw.dirtyRoots) != 0 {
		t.Errorf("internal map affected by caller mutation: %v", fw.dirtyRoots)
	}
	fw.dirtyMu.Unlock()
}

func TestFolderWatcher_RemoveStaleWatches(t *testing.T) {
	t.Parallel()
	// Set up two real directories with a live fsnotify watcher, delete one,
	// then call removeStaleWatches and verify only the surviving path remains
	// in the watch list.
	root := t.TempDir()
	keep := filepath.Join(root, "keep")
	stale := filepath.Join(root, "stale")
	if err := os.Mkdir(keep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stale, 0755); err != nil {
		t.Fatal(err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer w.Close()

	if err := w.Add(keep); err != nil {
		t.Fatalf("add keep: %v", err)
	}
	if err := w.Add(stale); err != nil {
		t.Fatalf("add stale: %v", err)
	}

	fw := &folderWatcher{watcher: w}
	fw.watchCount.Store(2)

	// Remove the directory on disk so its watch is stale.
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}

	fw.removeStaleWatches()

	remaining := w.WatchList()
	slices.Sort(remaining)
	if len(remaining) != 1 || remaining[0] != keep {
		t.Errorf("watch list after stale removal = %v, want [%s]", remaining, keep)
	}
	// Note: watchCount is not checked here — on platforms where fsnotify
	// auto-removes watches at the kernel/kqueue layer before we observe
	// the stale entry, the count is not decremented by this function.
	// The documented contract is WatchList convergence, not counter
	// accuracy under platform-level auto-removal.
}

func TestFolderWatcher_RemoveStaleWatches_NoopWhenAllPresent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer w.Close()
	if err := w.Add(root); err != nil {
		t.Fatal(err)
	}
	fw := &folderWatcher{watcher: w}
	fw.watchCount.Store(1)

	fw.removeStaleWatches()

	if got := fw.watchCount.Load(); got != 1 {
		t.Errorf("watchCount should be unchanged: %d", got)
	}
	if list := w.WatchList(); len(list) != 1 {
		t.Errorf("watch list = %v", list)
	}
}

func TestIsTempFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"/a/.mesh-tmp-abc", true},
		{"/a/.mesh-tmp-", true},
		{"/a/file.mesh-delta-tmp-peer", true},
		{"/a/regular.txt", false},
		{"/a/mesh-tmp-abc", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isTempFile(c.in); got != c.want {
			t.Errorf("isTempFile(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
