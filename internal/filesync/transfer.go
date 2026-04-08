package filesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/time/rate"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

const (
	maxTempFileAge  = 24 * time.Hour
	maxSyncFileSize = 4 * 1024 * 1024 * 1024 // 4 GB per file
)

// downloadFile fetches a file from a peer and writes it to the folder,
// resuming from an existing temp file if present.
// Returns the local path of the completed file.
func downloadFile(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash string, folderRoot string, limiter *rate.Limiter) (string, error) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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

	reader := newRateLimitedReader(ctx, io.LimitReader(resp.Body, maxSyncFileSize), limiter)
	if _, err := io.Copy(f, reader); err != nil {
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

// downloadFileDelta downloads a file using block-level delta transfer.
// It computes block signatures of the local file, sends them to the peer,
// and reconstructs the file from unchanged local blocks + received delta blocks.
// Falls back to full download if the local file doesn't exist.
func downloadFileDelta(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash, folderRoot string, limiter *rate.Limiter) (string, error) {
	destPath, err := safePath(folderRoot, relPath)
	if err != nil {
		return "", err
	}

	// If local file doesn't exist, fall back to full download.
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, folderRoot, limiter)
	}

	// Compute block signatures of local file.
	blockSize := int64(defaultBlockSize)
	localHashes, err := computeBlockSignatures(destPath, blockSize)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, folderRoot, limiter)
	}

	localInfo, err := os.Stat(destPath)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, folderRoot, limiter)
	}

	// Send block signatures to peer.
	req := &pb.BlockSignatures{
		FolderId:    folderID,
		Path:        relPath,
		BlockSize:   blockSize,
		FileSize:    localInfo.Size(),
		BlockHashes: localHashes,
	}
	reqData, err := proto.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal block sigs: %w", err)
	}

	u := fmt.Sprintf("http://%s/delta", peerAddr)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqData))
	if err != nil {
		return "", fmt.Errorf("create delta request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("delta request to %s: %w", peerAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Fall back to full download on error. Drain the delta response body first
	// so the connection can be reused instead of leaking.
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, folderRoot, limiter)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxSyncFileSize))
	if err != nil {
		return "", fmt.Errorf("read delta response: %w", err)
	}

	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(respBody, &deltaResp); err != nil {
		return "", fmt.Errorf("unmarshal delta: %w", err)
	}

	// Convert proto blocks to internal format.
	blocks := make([]deltaBlock, len(deltaResp.GetBlocks()))
	for i, b := range deltaResp.GetBlocks() {
		blocks[i] = deltaBlock{index: b.GetIndex(), data: b.GetData()}
	}

	// Apply delta to reconstruct the file.
	tmpPath, err := applyDelta(destPath, destPath, blockSize, deltaResp.GetFileSize(), blocks)
	if err != nil {
		return "", fmt.Errorf("apply delta: %w", err)
	}

	// Verify hash.
	actualHash, err := hashFile(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hash delta result: %w", err)
	}
	if actualHash != expectedHash {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("delta hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash)
	}

	// Atomic rename.
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename delta result: %w", err)
	}

	return destPath, nil
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
