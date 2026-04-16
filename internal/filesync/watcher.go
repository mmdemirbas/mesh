package filesync

import (
	"context"
	"errors"
	"sync"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchStats is per-root evidence captured during addRecursive so one run
// surfaces which folders exhaust FDs and which trees are huge despite ignores.
type watchStats struct {
	root         string
	dirsWalked   int
	dirsIgnored  int
	dirsAdded    int
	addErrors    int
	fdExhaustion int // ENFILE / EMFILE count
	firstErr     error
	firstErrPath string
	walkDuration time.Duration
}

const (
	// debounceInterval batches rapid filesystem events into a single scan trigger.
	debounceInterval = 500 * time.Millisecond

	// staleWatchInterval controls how often we clean up watches for deleted directories.
	staleWatchInterval = 5 * time.Minute

	// defaultMaxWatches caps the number of fsnotify watches to prevent FD exhaustion.
	// On macOS, kqueue uses one FD per watched directory. When exceeded, the
	// watcher stops adding new watches and relies on periodic scanning.
	defaultMaxWatches = 4096
)

// folderWatcher monitors multiple folder roots for filesystem changes and
// signals when a scan should be triggered.
type folderWatcher struct {
	watcher *fsnotify.Watcher
	// dirtyCh is signaled (non-blocking) when any watched file changes.
	dirtyCh    chan struct{}
	roots      []string
	ignore     map[string]*ignoreMatcher // folderRoot -> matcher
	watchCount int                       // current number of active watches
	maxWatches int                       // cap on fsnotify watches
	capped     bool                      // true if maxWatches was reached

	// dirtyRoots accumulates which folder roots had events since the last
	// drain.  Protected by dirtyMu.  Drained by drainDirtyRoots().
	dirtyMu    sync.Mutex
	dirtyRoots map[string]bool
}

// newFolderWatcher creates a watcher that monitors the given folder roots.
// Returns nil, error if the directory count exceeds maxWatches, in which
// case the caller should rely on periodic scanning only.
func newFolderWatcher(roots []string, ignoreMap map[string]*ignoreMatcher, maxWatches int) (*folderWatcher, error) {
	if maxWatches <= 0 {
		maxWatches = defaultMaxWatches
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &folderWatcher{
		watcher:    w,
		dirtyCh:    make(chan struct{}, 1),
		roots:      roots,
		ignore:     ignoreMap,
		maxWatches: maxWatches,
		dirtyRoots: make(map[string]bool),
	}
	totalWalked, totalIgnored, totalAdded, totalAddErrors, totalFD := 0, 0, 0, 0, 0
	for _, root := range roots {
		s := fw.addRecursive(root)
		totalWalked += s.dirsWalked
		totalIgnored += s.dirsIgnored
		totalAdded += s.dirsAdded
		totalAddErrors += s.addErrors
		totalFD += s.fdExhaustion
		// Per-root INFO so we can see which folder is heaviest / most error-prone.
		attrs := []any{
			"root", s.root,
			"duration", s.walkDuration,
			"dirs_walked", s.dirsWalked,
			"dirs_ignored", s.dirsIgnored,
			"dirs_added", s.dirsAdded,
			"add_errors", s.addErrors,
			"fd_exhaustion", s.fdExhaustion,
		}
		if s.firstErr != nil {
			attrs = append(attrs, "first_error", s.firstErr.Error(), "first_error_path", s.firstErrPath)
		}
		slog.Info("fsnotify watch setup", attrs...)
	}
	if fw.capped {
		// Too many directories — close the watcher and fall back to periodic scan.
		_ = w.Close()
		return nil, fmt.Errorf("directory count exceeds fsnotify limit (%d), using periodic scan only", maxWatches)
	}
	slog.Info("fsnotify watching directories",
		"total_watches", fw.watchCount,
		"dirs_walked", totalWalked,
		"dirs_ignored", totalIgnored,
		"dirs_added", totalAdded,
		"add_errors", totalAddErrors,
		"fd_exhaustion", totalFD,
	)
	return fw, nil
}

// addRecursive adds a directory and all its subdirectories to the watcher,
// returning per-root stats so callers can attribute FD pressure and huge
// trees to a specific folder.
func (fw *folderWatcher) addRecursive(root string) watchStats {
	s := watchStats{root: root}
	start := time.Now()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		s.dirsWalked++

		// Check ignore rules.
		if matcher, ok := fw.findMatcher(path); ok {
			rel, relErr := filepath.Rel(fw.rootFor(path), path)
			if relErr == nil && rel != "." {
				rel = filepath.ToSlash(rel)
				if matcher.shouldIgnore(rel, true) {
					s.dirsIgnored++
					return filepath.SkipDir
				}
			}
		}

		if fw.watchCount >= fw.maxWatches {
			if !fw.capped {
				fw.capped = true
				slog.Warn("fsnotify watch limit reached, relying on periodic scan for remaining directories",
					"limit", fw.maxWatches, "path", path)
			}
			return filepath.SkipDir
		}

		if err := fw.watcher.Add(path); err != nil {
			s.addErrors++
			if isFDExhaustion(err) {
				s.fdExhaustion++
			}
			if s.firstErr == nil {
				s.firstErr = err
				s.firstErrPath = path
			}
		} else {
			fw.watchCount++
			s.dirsAdded++
		}
		return nil
	})
	s.walkDuration = time.Since(start)
	return s
}

// isFDExhaustion reports whether err is ENFILE or EMFILE, signalling the
// process or system has run out of file descriptors.
func isFDExhaustion(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
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
// It also adds watchers for newly created directories and removes watchers
// for deleted directories to prevent FD leaks (macOS kqueue holds one FD
// per watched path).
func (fw *folderWatcher) run(ctx context.Context) {
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time

	staleTicker := time.NewTicker(staleWatchInterval)
	defer staleTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}

			// Skip events for filesync temp files — they are transient and
			// should not trigger scans or watcher updates.
			if isTempFile(event.Name) {
				continue
			}

			// Add watcher for new directories (if under the cap).
			if event.Has(fsnotify.Create) && !fw.capped {
				if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
					fw.addRecursive(event.Name)
				}
			}

			// Remove watcher for deleted/renamed directories to free kqueue FDs.
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				if fw.watcher.Remove(event.Name) == nil {
					fw.watchCount--
				}
			}

			// Record which root was affected so scanLoop can target it.
			if root := fw.rootFor(event.Name); root != event.Name || fw.isRoot(event.Name) {
				fw.dirtyMu.Lock()
				fw.dirtyRoots[root] = true
				fw.dirtyMu.Unlock()
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

		case <-staleTicker.C:
			fw.removeStaleWatches()
		}
	}
}

// removeStaleWatches removes watches for paths that no longer exist on disk.
// This is a safety net for cases where Remove/Rename events are missed.
func (fw *folderWatcher) removeStaleWatches() {
	removed := 0
	for _, path := range fw.watcher.WatchList() {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if fw.watcher.Remove(path) == nil {
				fw.watchCount--
				removed++
			}
		}
	}
	if removed > 0 {
		slog.Debug("cleaned stale fsnotify watches", "removed", removed)
	}
}

// isTempFile returns true if the path is a filesync temp file that should
// not trigger scan events.
func isTempFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, ".mesh-tmp-") || strings.HasSuffix(base, ".mesh-delta-tmp")
}

// drainDirtyRoots returns the set of folder roots that received events since
// the last drain and resets the internal set.  Returns nil when nothing changed.
func (fw *folderWatcher) drainDirtyRoots() map[string]bool {
	fw.dirtyMu.Lock()
	defer fw.dirtyMu.Unlock()
	if len(fw.dirtyRoots) == 0 {
		return nil
	}
	out := fw.dirtyRoots
	fw.dirtyRoots = make(map[string]bool)
	return out
}

// isRoot reports whether path is one of the configured folder roots.
func (fw *folderWatcher) isRoot(path string) bool {
	for _, root := range fw.roots {
		if path == root {
			return true
		}
	}
	return false
}

// close shuts down the fsnotify watcher.
func (fw *folderWatcher) close() error {
	return fw.watcher.Close()
}
