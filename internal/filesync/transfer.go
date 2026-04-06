package filesync

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxTempFileAge  = 24 * time.Hour
	maxSyncFileSize = 4 * 1024 * 1024 * 1024 // 4 GB per file
)

// downloadFile fetches a file from a peer and writes it to the folder,
// resuming from an existing temp file if present.
// Returns the local path of the completed file.
func downloadFile(client *http.Client, peerAddr, folderID, relPath, expectedHash string, folderRoot string) (string, error) {
	destPath, err := safePath(folderRoot, relPath)
	if err != nil {
		return "", err
	}

	if len(expectedHash) < 16 {
		return "", fmt.Errorf("invalid hash for %q: too short (%d chars)", relPath, len(expectedHash))
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
		return "", fmt.Errorf("create parent dir: %w", err)
	}

	// Temp file for atomic write + resume.
	tmpName := ".mesh-tmp-" + expectedHash[:16]
	tmpPath := filepath.Join(filepath.Dir(destPath), tmpName)

	// Check if we can resume from an existing temp file.
	var offset int64
	if info, err := os.Stat(tmpPath); err == nil {
		offset = info.Size()
	}

	// Build request URL.
	u := fmt.Sprintf("http://%s/file?folder=%s&path=%s",
		peerAddr,
		url.QueryEscape(folderID),
		url.QueryEscape(relPath),
	)
	if offset > 0 {
		u += fmt.Sprintf("&offset=%d", offset)
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", relPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("file not found on peer: %s", relPath)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("peer returned %d for %s", resp.StatusCode, relPath)
	}

	// Open temp file for writing (append if resuming).
	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmpPath, flag, 0600) //nolint:gosec // G304: tmpPath is in user folder
	if err != nil {
		return "", fmt.Errorf("open temp file: %w", err)
	}

	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxSyncFileSize)); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write file data: %w", err)
	}
	_ = f.Close()

	// Verify hash of completed file.
	actualHash, err := hashFile(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hash temp file: %w", err)
	}
	if actualHash != expectedHash {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash)
	}

	// Atomic rename to final destination.
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename to final path: %w", err)
	}

	return destPath, nil
}

// safePath validates a relative path against traversal and resolves it within
// folderRoot. Returns the absolute path or an error.
func safePath(folderRoot, relPath string) (string, error) {
	clean := filepath.FromSlash(relPath)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("invalid file path: %q", relPath)
	}
	full := filepath.Join(folderRoot, clean)
	absRoot, err := filepath.Abs(folderRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	absPath, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal: %q resolves outside root", relPath)
	}
	return full, nil
}

// deleteFile removes a local file, creating a tombstone in the index.
func deleteFile(folderRoot, relPath string) error {
	path, err := safePath(folderRoot, relPath)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", relPath, err)
	}
	return nil
}
