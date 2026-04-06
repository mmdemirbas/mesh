package filesync

import (
	"crypto/sha256"
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
	transferTimeout = 10 * time.Minute
	connectTimeout  = 10 * time.Second
	maxTempFileAge  = 24 * time.Hour
)

// downloadFile fetches a file from a peer and writes it to the folder,
// resuming from an existing temp file if present.
// Returns the local path of the completed file.
func downloadFile(client *http.Client, peerAddr, folderID, relPath, expectedHash string, folderRoot string) (string, error) {
	// Validate the path to prevent traversal.
	clean := filepath.FromSlash(relPath)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("invalid file path: %q", relPath)
	}

	if len(expectedHash) < 16 {
		return "", fmt.Errorf("invalid hash for %q: too short (%d chars)", relPath, len(expectedHash))
	}

	destPath := filepath.Join(folderRoot, clean)

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

	if _, err := io.Copy(f, resp.Body); err != nil {
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

// deleteFile removes a local file, creating a tombstone in the index.
func deleteFile(folderRoot, relPath string) error {
	clean := filepath.FromSlash(relPath)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return fmt.Errorf("invalid file path: %q", relPath)
	}
	path := filepath.Join(folderRoot, clean)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", relPath, err)
	}
	return nil
}

// verifyFile checks if a file at path matches the expected SHA-256 hash.
func verifyFile(path, expectedHash string) error {
	h := sha256.New()
	f, err := os.Open(path) //nolint:gosec // G304: path from user folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", h.Sum(nil))
	if actual != expectedHash {
		return fmt.Errorf("hash mismatch: expected %s, got %s", expectedHash, actual)
	}
	return nil
}
