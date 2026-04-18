package filesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"github.com/mmdemirbas/mesh/internal/gziputil"
	"google.golang.org/protobuf/proto"
)

const (
	maxTempFileAge  = 24 * time.Hour
	maxSyncFileSize = 4 * 1024 * 1024 * 1024 // 4 GB per file
)

// peerSuffix returns a short deterministic suffix for a peer address, used
// to isolate per-peer temp files and prevent concurrent download corruption.
func peerSuffix(peerAddr string) string {
	// Use last 8 chars of the address hash for brevity + uniqueness.
	h := sha256.Sum256([]byte(peerAddr))
	return hex.EncodeToString(h[:4])
}

// validateRelPath checks that relPath is a valid relative path without
// traversal components. This is a fast syntactic check — actual path
// containment is enforced atomically by os.Root (L5).
func validateRelPath(relPath string) error {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, "\x00") {
		return fmt.Errorf("invalid file path: %q", relPath)
	}
	return nil
}

// downloadToVerifiedTemp fetches a file from a peer, writes it to a temp file,
// and verifies the hash. Returns the relative temp path within root without
// renaming to the final destination. Used by conflict resolution (B13) to
// ensure the download succeeds before moving the local file aside.
//
// L5: all filesystem operations go through os.Root to prevent symlink TOCTOU.
// H1: temp file name includes a peer-derived suffix so concurrent downloads
// of the same file from different peers get separate temp files.
func downloadToVerifiedTemp(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash string, root *os.Root, limiter *rate.Limiter) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
	}

	if len(expectedHash) < 16 {
		return "", fmt.Errorf("invalid hash for %q: too short (%d chars)", relPath, len(expectedHash))
	}

	// Ensure parent directory exists.
	if dir := filepath.Dir(relPath); dir != "." {
		if err := root.MkdirAll(dir, 0750); err != nil {
			return "", fmt.Errorf("create parent dir: %w", err)
		}
	}

	// Temp file for atomic write + resume.
	// H1: include peer suffix to prevent concurrent download collision
	// when multiple peers serve the same file.
	tmpName := ".mesh-tmp-" + expectedHash[:16] + "-" + peerSuffix(peerAddr)
	tmpRelPath := filepath.Join(filepath.Dir(relPath), tmpName)

	// Check if we can resume from an existing temp file.
	var offset int64
	if info, err := root.Stat(tmpRelPath); err == nil {
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
	f, err := root.OpenFile(tmpRelPath, flag, 0600)
	if err != nil {
		return "", fmt.Errorf("open temp file: %w", err)
	}

	reader := newRateLimitedReader(ctx, io.LimitReader(resp.Body, maxSyncFileSize), limiter)
	if _, err := io.Copy(f, reader); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write file data: %w", err)
	}
	// L2: check Close error — on NFS/FUSE the server-side write can fail
	// here. Without this check, the corrupt temp produces a misleading
	// "hash mismatch" instead of the real I/O error.
	if err := f.Close(); err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("close temp file: %w", err)
	}

	// Verify hash of completed file.
	actualHash, err := hashFileRoot(root, tmpRelPath)
	if err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("hash temp file: %w", err)
	}
	if actualHash != expectedHash {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash)
	}

	return tmpRelPath, nil
}

// downloadFile fetches a file from a peer and writes it to the folder.
// Returns the relative path of the completed file within root.
func downloadFile(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash string, root *os.Root, limiter *rate.Limiter) (string, error) {
	tmpRelPath, err := downloadToVerifiedTemp(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	if err != nil {
		return "", err
	}

	if err := renameReplaceRoot(root, tmpRelPath, relPath); err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("rename to final path: %w", err)
	}

	return relPath, nil
}

// safePath validates a relative path against traversal and resolves it within
// folderRoot. Returns the absolute path or an error.
//
// Deprecated: use validateRelPath + os.Root operations instead (L5).
// Kept for read-only paths in the admin API (conflict diff).
func safePath(folderRoot, relPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, "\x00") {
		return "", fmt.Errorf("invalid file path: %q", relPath)
	}
	full := filepath.Join(folderRoot, clean)

	// Resolve the root through symlinks for a canonical prefix.
	absRoot, err := filepath.Abs(folderRoot)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("eval symlinks root: %w", err)
	}

	// First check: lexical prefix before the file exists (for new file creation).
	absPath, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal: %q resolves outside root", relPath)
	}

	// Second check: if the path exists on disk, resolve symlinks and
	// verify the real path is still inside the real root. This catches
	// symlinks inside the folder that point outside the root.
	if evalPath, evalErr := filepath.EvalSymlinks(absPath); evalErr == nil {
		if evalPath != evalRoot && !strings.HasPrefix(evalPath, evalRoot+string(filepath.Separator)) {
			return "", fmt.Errorf("path traversal via symlink: %q resolves to %q outside root", relPath, evalPath)
		}
	}

	return full, nil
}

// downloadFileDelta downloads a file using block-level delta transfer.
// It computes block signatures of the local file, sends them to the peer,
// and reconstructs the file from unchanged local blocks + received delta blocks.
// Falls back to full download if the local file doesn't exist.
func downloadFileDelta(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash string, root *os.Root, limiter *rate.Limiter) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
	}

	// If local file doesn't exist, fall back to full download.
	if _, err := root.Stat(relPath); os.IsNotExist(err) {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}

	// Compute block signatures of local file.
	blockSize := int64(defaultBlockSize)
	localHashes, err := computeBlockSignaturesRoot(root, relPath, blockSize)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}

	localInfo, err := root.Stat(relPath)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}

	// Send block signatures to peer.
	sigReq := &pb.BlockSignatures{
		FolderId:    folderID,
		Path:        relPath,
		BlockSize:   blockSize,
		FileSize:    localInfo.Size(),
		BlockHashes: localHashes,
	}
	reqData, err := proto.Marshal(sigReq)
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
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}

	// Cap delta response at 256 MB — delta transfers should be much smaller
	// than full files since they only contain changed blocks.
	const maxDeltaResponseSize = 256 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxDeltaResponseSize))
	if err != nil {
		return "", fmt.Errorf("read delta response: %w", err)
	}

	var deltaResp pb.DeltaResponse
	if err := proto.Unmarshal(respBody, &deltaResp); err != nil {
		return "", fmt.Errorf("unmarshal delta: %w", err)
	}

	// N4: validate remote file size before allocating loops/memory.
	remoteFileSize := deltaResp.GetFileSize()
	if remoteFileSize <= 0 || remoteFileSize > maxSyncFileSize {
		return "", fmt.Errorf("delta file size out of range for %s: %d", relPath, remoteFileSize)
	}

	// Convert proto blocks to internal format.
	blocks := make([]deltaBlock, len(deltaResp.GetBlocks()))
	for i, b := range deltaResp.GetBlocks() {
		blocks[i] = deltaBlock{index: b.GetIndex(), data: b.GetData()}
	}

	// Apply delta to reconstruct the file.
	tmpRelPath, err := applyDeltaRoot(root, relPath, peerSuffix(peerAddr), blockSize, remoteFileSize, blocks)
	if err != nil {
		return "", fmt.Errorf("apply delta: %w", err)
	}

	// Verify hash.
	actualHash, err := hashFileRoot(root, tmpRelPath)
	if err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("hash delta result: %w", err)
	}
	if actualHash != expectedHash {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("delta hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash)
	}

	// Atomic rename.
	if err := renameReplaceRoot(root, tmpRelPath, relPath); err != nil {
		_ = root.Remove(tmpRelPath)
		return "", fmt.Errorf("rename delta result: %w", err)
	}

	return relPath, nil
}

// bundleEntry describes one file expected from a bundle download.
type bundleEntry struct {
	Path         string
	ExpectedHash string
	RemoteSize   int64
	RemoteMode   uint32
}

// bundleBatches partitions entries into batches of at most maxBundlePaths
// entries and at most maxBundleTotal cumulative bytes.
func bundleBatches(entries []bundleEntry) [][]bundleEntry {
	var batches [][]bundleEntry
	var batch []bundleEntry
	var batchBytes int64

	for _, e := range entries {
		if len(batch) >= maxBundlePaths || (batchBytes+e.RemoteSize > maxBundleTotal && len(batch) > 0) {
			batches = append(batches, batch)
			batch = nil
			batchBytes = 0
		}
		batch = append(batch, e)
		batchBytes += e.RemoteSize
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

// downloadBundle fetches multiple small files in a single tar+gzip round-trip (P19).
// Returns the paths that were successfully written and the entries that need retry.
func downloadBundle(ctx context.Context, client *http.Client, peerAddr, folderID string, entries []bundleEntry, root *os.Root, limiter *rate.Limiter) (successful []string, retry []bundleEntry) {
	// Build request.
	paths := make([]string, len(entries))
	expect := make(map[string]bundleEntry, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
		expect[e.Path] = e
	}
	reqMsg := &pb.BundleRequest{
		FolderId: folderID,
		Paths:    paths,
	}
	reqData, err := proto.Marshal(reqMsg)
	if err != nil {
		return nil, entries
	}
	compressed, err := gziputil.Encode(reqData)
	if err != nil {
		return nil, entries
	}

	reqURL := fmt.Sprintf("http://%s/bundle", peerAddr)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(compressed))
	if err != nil {
		return nil, entries
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "gzip")
	// Prevent Go's http.Transport from auto-decompressing the gzip response;
	// we need the raw gzip stream to feed into tar.NewReader via gzip.NewReader.
	httpReq.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, entries
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, entries
	}

	// Read tar+gzip response.
	var reader io.Reader = resp.Body
	if limiter != nil {
		reader = newRateLimitedReader(ctx, reader, limiter)
	}
	gr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, entries
	}
	defer func() { _ = gr.Close() }()
	tr := tar.NewReader(gr)

	received := make(map[string]bool, len(entries))

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		e, ok := expect[hdr.Name]
		if !ok {
			// Unexpected entry — skip.
			continue
		}

		if err := validateRelPath(hdr.Name); err != nil {
			continue
		}

		// Write to temp, hash during copy.
		suffix := peerSuffix(peerAddr)
		tmpName := fmt.Sprintf(".mesh-tmp-%s-%s", filepath.Base(hdr.Name), suffix)
		tmpDir := filepath.Dir(hdr.Name)
		tmpRelPath := filepath.Join(tmpDir, tmpName)

		if dir := filepath.Dir(hdr.Name); dir != "." {
			if mkErr := root.MkdirAll(dir, 0750); mkErr != nil {
				continue
			}
		}

		f, err := root.OpenFile(tmpRelPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			continue
		}
		h := sha256.New()
		_, copyErr := io.Copy(f, io.TeeReader(tr, h))
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			_ = root.Remove(tmpRelPath)
			continue
		}

		actualHash := hex.EncodeToString(h.Sum(nil))
		if actualHash != e.ExpectedHash {
			slog.Warn("bundle hash mismatch", "folder", folderID, "path", hdr.Name,
				"expected", e.ExpectedHash, "actual", actualHash)
			_ = root.Remove(tmpRelPath)
			continue
		}

		// Rename to final.
		if err := renameReplaceRoot(root, tmpRelPath, hdr.Name); err != nil {
			_ = root.Remove(tmpRelPath)
			continue
		}

		// Apply permissions.
		fileMode := os.FileMode(e.RemoteMode)
		if fileMode == 0 {
			fileMode = 0644
		}
		_ = root.Chmod(hdr.Name, fileMode)

		received[hdr.Name] = true
		successful = append(successful, hdr.Name)
	}

	// Entries not received go to retry.
	for _, e := range entries {
		if !received[e.Path] {
			retry = append(retry, e)
		}
	}
	return successful, retry
}

// verifyPostWrite re-reads the file at relPath and compares its hash against
// expectedHash. Used on network filesystems (C2) where write-back caching can
// silently corrupt data. Returns nil when the hash matches. On mismatch,
// records a retry so the file is re-downloaded on the next sync cycle.
func verifyPostWrite(root *os.Root, relPath, expectedHash, folderID string, retries *retryTracker, indexMu *sync.RWMutex) error {
	actualHash, err := hashFileRoot(root, relPath)
	if err != nil {
		slog.Error("C2: post-write verification failed: cannot re-read file",
			"folder", folderID, "path", relPath, "error", err)
		return fmt.Errorf("post-write verify read: %w", err)
	}
	if actualHash != expectedHash {
		slog.Error("C2: post-write verification failed: data corruption detected",
			"folder", folderID, "path", relPath,
			"expected", expectedHash, "actual", actualHash)
		indexMu.Lock()
		retries.record(relPath, expectedHash)
		indexMu.Unlock()
		return fmt.Errorf("post-write hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash)
	}
	return nil
}

// deleteFile removes a local file within the root.
func deleteFile(root *os.Root, relPath string) error {
	if err := validateRelPath(relPath); err != nil {
		return err
	}
	err := root.Remove(relPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", relPath, err)
	}
	return nil
}
