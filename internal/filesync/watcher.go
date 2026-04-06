package filesync

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// debounceInterval batches rapid filesystem events into a single scan trigger.
	debounceInterval = 500 * time.Millisecond
)

// folderWatcher monitors multiple folder roots for filesystem changes and
// signals when a scan should be triggered.
type folderWatcher struct {
	watcher *fsnotify.Watcher
	// dirtyCh is signaled (non-blocking) when any watched file changes.
	dirtyCh chan struct{}
	mu      sync.Mutex
	roots   []string
	ignore  map[string]*ignoreMatcher // folderRoot -> matcher
}

// newFolderWatcher creates a watcher that monitors the given folder roots.
func newFolderWatcher(roots []string, ignoreMap map[string]*ignoreMatcher) (*folderWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &folderWatcher{
		watcher: w,
		dirtyCh: make(chan struct{}, 1),
		roots:   roots,
		ignore:  ignoreMap,
	}
	for _, root := range roots {
		fw.addRecursive(root)
	}
	return fw, nil
}

// addRecursive adds a directory and all its subdirectories to the watcher.
func (fw *folderWatcher) addRecursive(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		// Check ignore rules.
		if matcher, ok := fw.findMatcher(path); ok {
			rel, relErr := filepath.Rel(fw.rootFor(path), path)
			if relErr == nil && rel != "." {
				rel = filepath.ToSlash(rel)
				if matcher.shouldIgnore(rel, true) {
					return filepath.SkipDir
				}
			}
		}

		if err := fw.watcher.Add(path); err != nil {
			slog.Debug("fsnotify: failed to watch directory", "path", path, "error", err)
		}
		return nil
	})
}

// rootFor finds which configured root contains the given path.
func (fw *folderWatcher) rootFor(path string) string {
	for _, root := range fw.roots {
		if rel, err := filepath.Rel(root, path); err == nil && !filepath.IsAbs(rel) {
			return root
		}
	}
	return path
}

// findMatcher returns the ignore matcher for the root that contains path.
func (fw *folderWatcher) findMatcher(path string) (*ignoreMatcher, bool) {
	root := fw.rootFor(path)
	m, ok := fw.ignore[root]
	return m, ok
}

// run processes fsnotify events, debounces them, and signals dirtyCh.
// It also adds watchers for newly created directories.
func (fw *folderWatcher) run(ctx context.Context) {
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}

			// Add watcher for new directories.
			if event.Has(fsnotify.Create) {
				if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
					fw.addRecursive(event.Name)
				}
			}

			// Signal dirty with debounce.
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(debounceInterval)
				debounceC = debounceTimer.C
			} else {
				debounceTimer.Reset(debounceInterval)
			}

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("fsnotify error (falling back to periodic scan)", "error", err)

		case <-debounceC:
			debounceTimer = nil
			debounceC = nil
			// Non-blocking signal.
			select {
			case fw.dirtyCh <- struct{}{}:
			default:
			}
		}
	}
}

// close shuts down the fsnotify watcher.
func (fw *folderWatcher) close() error {
	return fw.watcher.Close()
}
