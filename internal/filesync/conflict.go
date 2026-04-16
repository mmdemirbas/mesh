package filesync

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resolveConflict determines which version of a file wins (local vs remote)
// and returns the winner ("local" or "remote") along with the conflict file
// path for the loser. Does NOT rename or modify any files — the caller handles
// the file operations (B13: download must succeed before any rename).
//
// Strategy: newer mtime wins. The loser is saved as:
//
//	<name>.sync-conflict-<YYYYMMDD-HHMMSS>-<deviceShort>.<ext>
func resolveConflict(folderRoot, relPath string, localMtimeNS, remoteMtimeNS int64, remoteDeviceID string) (winner, conflictPath string) {
	localPath := filepath.Join(folderRoot, filepath.FromSlash(relPath))

	// Use the file's current on-disk mtime rather than the scan-time value —
	// the user may have edited the file between scan and resolution, and
	// renaming the (newly-latest) local copy to a conflict file would silently
	// discard their edits. Fall back to the passed-in index mtime on stat
	// error so we still make a reasonable decision.
	if info, statErr := os.Stat(localPath); statErr == nil {
		localMtimeNS = info.ModTime().UnixNano()
	}

	// Determine winner by mtime. If equal, remote wins to avoid data loss.
	if localMtimeNS > remoteMtimeNS {
		return "local", ""
	}

	// Remote wins — compute conflict name for the local file.
	// N7: check for collision and append a counter if needed, since
	// second-granularity timestamps can collide under rapid edits.
	conflictName := conflictFileName(relPath, remoteDeviceID)
	cPath := filepath.Join(folderRoot, filepath.FromSlash(conflictName))
	if _, err := os.Stat(cPath); err == nil {
		// Collision — try up to 99 suffixed names.
		base := cPath
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		found := false
		for i := 2; i <= 100; i++ {
			candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				cPath = candidate
				found = true
				break
			}
		}
		if !found {
			// All 99 counter names taken — fall back to random suffix.
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			cPath = stem + "-" + hex.EncodeToString(b) + ext
		}
	}

	return "remote", cPath
}

// conflictFileName generates a Syncthing-style conflict file name.
// Example: "docs/report.docx" -> "docs/report.sync-conflict-20260406-143022-abc123.docx"
func conflictFileName(relPath, deviceID string) string {
	dir := filepath.Dir(relPath)
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	ts := time.Now().Format("20060102-150405")
	short := deviceID
	if len(short) > 6 {
		short = short[:6]
	}

	conflictBase := fmt.Sprintf("%s.sync-conflict-%s-%s%s", name, ts, short, ext)

	if dir == "." {
		return conflictBase
	}
	return filepath.ToSlash(filepath.Join(dir, conflictBase))
}
