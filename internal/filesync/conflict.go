package filesync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resolveConflict handles a file that was modified on both the local and remote
// sides. It downloads the remote version and renames the losing copy using
// Syncthing-style conflict naming.
//
// Strategy: newer mtime wins. The loser is saved as:
//
//	<name>.sync-conflict-<YYYYMMDD-HHMMSS>-<deviceShort>.<ext>
func resolveConflict(folderRoot, relPath string, localMtimeNS, remoteMtimeNS int64, remoteDeviceID string) (winner string, err error) {
	localPath := filepath.Join(folderRoot, filepath.FromSlash(relPath))

	// Determine winner by mtime. If equal, remote wins to avoid data loss.
	if localMtimeNS > remoteMtimeNS {
		// Local wins — remote copy gets the conflict name.
		return "local", nil
	}

	// Remote wins — rename local to conflict name.
	conflictName := conflictFileName(relPath, remoteDeviceID)
	conflictPath := filepath.Join(folderRoot, filepath.FromSlash(conflictName))

	if err := os.MkdirAll(filepath.Dir(conflictPath), 0750); err != nil {
		return "", fmt.Errorf("create conflict dir: %w", err)
	}

	if err := os.Rename(localPath, conflictPath); err != nil {
		return "", fmt.Errorf("rename local to conflict: %w", err)
	}

	return "remote", nil
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
