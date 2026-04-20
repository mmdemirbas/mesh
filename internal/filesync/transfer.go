package filesync

import (
	"archive/tar"
	"bytes"
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
	"github.com/mmdemirbas/mesh/internal/zstdutil"
	"google.golang.org/protobuf/proto"
)

const (
	maxTempFileAge  = 24 * time.Hour
	maxSyncFileSize = 4 * 1024 * 1024 * 1024 // 4 GB per file
	diskSpaceMargin = 64 * 1024 * 1024       // G2: 64 MB safety margin on disk space checks
)

// checkDiskSpace returns an error if the filesystem containing path has less
// than needed+diskSpaceMargin bytes available. Returns nil when the check
// cannot be performed (unsupported platform, stat error) — best effort only.
func checkDiskSpace(path string, needed int64) error {
	if needed <= 0 {
		return nil
	}
	avail, ok := availableBytes(path)
	if !ok {
		return nil
	}
	required := uint64(needed) + diskSpaceMargin
	if avail < required {
		return fmt.Errorf("insufficient disk space: need %d bytes, have %d bytes available", required, avail)
	}
	return nil
}

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
// C3: when the sender exposes /blocksigs, each 128 KB block is hashed as it
// arrives. On block mismatch, the temp file is truncated back to the last
// verified boundary and the remainder is re-requested with &offset=, up to
// maxBlockRetries attempts. Peers without /blocksigs fall back to the
// whole-file path. A final whole-file hash check always runs as a safety net.
//
// L5: all filesystem operations go through os.Root to prevent symlink TOCTOU.
// H1: temp file name includes a peer-derived suffix so concurrent downloads
// of the same file from different peers get separate temp files.
func downloadToVerifiedTemp(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, expectedHash Hash256, root *os.Root, limiter *rate.Limiter) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
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
	hashHex := expectedHash.String()
	tmpName := ".mesh-tmp-" + hashHex[:16] + "-" + peerSuffix(peerAddr)
	tmpRelPath := filepath.Join(filepath.Dir(relPath), tmpName)

	// C3: try per-block verified download first; fall back on any error.
	if sigs, ok := tryFetchBlockSignatures(ctx, client, peerAddr, folderID, relPath); ok {
		if err := checkDiskSpace(root.Name(), sigs.GetFileSize()); err != nil {
			return "", fmt.Errorf("download %s: %w", relPath, err)
		}
		if err := downloadWithBlockVerify(ctx, client, peerAddr, folderID, relPath, sigs, root, limiter, tmpRelPath); err != nil {
			_ = root.Remove(tmpRelPath)
			return "", err
		}
	} else {
		if err := downloadWhole(ctx, client, peerAddr, folderID, relPath, root, limiter, tmpRelPath); err != nil {
			_ = root.Remove(tmpRelPath)
			return "", err
		}
	}

	// Final whole-file hash check — catches sender-side races (file changed
	// between /blocksigs and /file) and sanity-checks both paths.
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

// maxBlockSigsResponse caps the /blocksigs response body. At 128 KB blocks,
// 1 MB of hashes covers a 4 GB file (the maxSyncFileSize ceiling); the 16 MB
// bound tolerates smaller blockSize values without OOM risk.
const maxBlockSigsResponse = 16 * 1024 * 1024

// maxBlockRetries bounds the number of per-block refetch attempts within a
// single downloadToVerifiedTemp call. Once exhausted, the caller's
// retryTracker takes over and may quarantine the (path, peer) pair.
const maxBlockRetries = 3

// tryFetchBlockSignatures fetches the sender's per-block hashes for relPath.
// Returns (nil, false) on any error — the caller treats this as "peer doesn't
// support /blocksigs" and falls back to whole-file verify.
func tryFetchBlockSignatures(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string) (*pb.BlockSignatures, bool) {
	u := fmt.Sprintf("https://%s/blocksigs?folder=%s&path=%s",
		peerAddr,
		url.QueryEscape(folderID),
		url.QueryEscape(relPath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBlockSigsResponse))
	if err != nil {
		return nil, false
	}
	var sigs pb.BlockSignatures
	if err := proto.Unmarshal(body, &sigs); err != nil {
		return nil, false
	}
	if sigs.GetFileSize() < 0 || sigs.GetFileSize() > maxSyncFileSize {
		return nil, false
	}
	// Validate the chunk layout tiles [0, file_size) with non-zero,
	// bounded lengths and each carries a full SHA-256 hash.
	var covered int64
	for _, b := range sigs.GetBlocks() {
		if b.GetOffset() != covered {
			return nil, false
		}
		if b.GetLength() <= 0 || int(b.GetLength()) > fastCDCMax {
			return nil, false
		}
		if len(b.GetHash()) != 32 {
			return nil, false
		}
		covered += int64(b.GetLength())
	}
	if covered != sigs.GetFileSize() {
		return nil, false
	}
	return &sigs, true
}

// downloadWithBlockVerify streams relPath from the peer, verifying each
// FastCDC chunk against sigs.Blocks as it arrives. On chunk mismatch,
// it truncates the temp file back to the last verified boundary and
// reissues the request with &offset=. Bounded by maxBlockRetries.
func downloadWithBlockVerify(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, sigs *pb.BlockSignatures, root *os.Root, limiter *rate.Limiter, tmpRelPath string) error {
	totalSize := sigs.GetFileSize()
	expectedBlocks := sigs.GetBlocks()

	// Resume at the last verified chunk boundary (any partial chunk is
	// discarded — we don't know if it was truncated mid-write).
	var verifiedOffset int64
	if info, statErr := root.Stat(tmpRelPath); statErr == nil && info.Size() > 0 {
		for _, b := range expectedBlocks {
			end := b.GetOffset() + int64(b.GetLength())
			if end > info.Size() {
				break
			}
			verifiedOffset = end
		}
		if verifiedOffset > totalSize {
			verifiedOffset = totalSize
		}
	}

	f, err := root.OpenFile(tmpRelPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	if err := f.Truncate(verifiedOffset); err != nil {
		_ = f.Close()
		return fmt.Errorf("truncate temp: %w", err)
	}
	if _, err := f.Seek(verifiedOffset, io.SeekStart); err != nil {
		_ = f.Close()
		return fmt.Errorf("seek temp: %w", err)
	}

	// Locate the chunk index to resume from.
	nextBlock := func(offset int64) int {
		for i, b := range expectedBlocks {
			if b.GetOffset() == offset {
				return i
			}
		}
		return len(expectedBlocks)
	}

	buf := make([]byte, fastCDCMax)
	attempts := 0

	for verifiedOffset < totalSize {
		u := fmt.Sprintf("https://%s/file?folder=%s&path=%s&offset=%d",
			peerAddr,
			url.QueryEscape(folderID),
			url.QueryEscape(relPath),
			verifiedOffset,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("create request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("download %s: %w", relPath, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			_ = f.Close()
			return fmt.Errorf("file not found on peer: %s", relPath)
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			_ = f.Close()
			return fmt.Errorf("peer returned %d for %s", resp.StatusCode, relPath)
		}

		reader := newRateLimitedReader(ctx, io.LimitReader(resp.Body, maxSyncFileSize), limiter)
		restart := false

		for i := nextBlock(verifiedOffset); i < len(expectedBlocks) && verifiedOffset < totalSize; i++ {
			b := expectedBlocks[i]
			want := int64(b.GetLength())
			n, readErr := io.ReadFull(reader, buf[:want])
			if int64(n) != want {
				slog.Warn("C3: short read during block verify",
					"folder", folderID, "path", relPath, "peer", peerAddr,
					"block", i, "want", want, "got", n, "err", readErr)
				restart = true
				break
			}
			h := sha256.Sum256(buf[:n])
			if !bytes.Equal(h[:], b.GetHash()) {
				slog.Warn("C3: block hash mismatch, will retry",
					"folder", folderID, "path", relPath, "peer", peerAddr,
					"block", i)
				restart = true
				break
			}
			if _, err := f.Write(buf[:n]); err != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				_ = f.Close()
				return fmt.Errorf("write block: %w", err)
			}
			verifiedOffset += int64(n)
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if !restart {
			break
		}

		attempts++
		if attempts > maxBlockRetries {
			_ = f.Close()
			return fmt.Errorf("block verify failed after %d attempts for %s", maxBlockRetries, relPath)
		}
		if err := f.Truncate(verifiedOffset); err != nil {
			_ = f.Close()
			return fmt.Errorf("truncate on retry: %w", err)
		}
		if _, err := f.Seek(verifiedOffset, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("seek on retry: %w", err)
		}
	}

	// L2: check Close error — on NFS/FUSE the server-side write can fail here.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return nil
}

// downloadWhole streams the entire body into tmpRelPath with no per-block
// verification. Used as the fallback when the peer doesn't expose /blocksigs.
// The final whole-file hash check in downloadToVerifiedTemp catches any
// corruption.
func downloadWhole(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, root *os.Root, limiter *rate.Limiter, tmpRelPath string) error {
	var offset int64
	if info, err := root.Stat(tmpRelPath); err == nil {
		offset = info.Size()
	}

	u := fmt.Sprintf("https://%s/file?folder=%s&path=%s",
		peerAddr,
		url.QueryEscape(folderID),
		url.QueryEscape(relPath),
	)
	if offset > 0 {
		u += fmt.Sprintf("&offset=%d", offset)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", relPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file not found on peer: %s", relPath)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %d for %s", resp.StatusCode, relPath)
	}

	// G2: pre-flight disk-space check. ContentLength already excludes the
	// resumed offset. Best-effort — skipped when unknown.
	if err := checkDiskSpace(root.Name(), resp.ContentLength); err != nil {
		return fmt.Errorf("download %s: %w", relPath, err)
	}

	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := root.OpenFile(tmpRelPath, flag, 0600)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}

	reader := newRateLimitedReader(ctx, io.LimitReader(resp.Body, maxSyncFileSize), limiter)
	if _, err := io.Copy(f, reader); err != nil {
		_ = f.Close()
		return fmt.Errorf("write file data: %w", err)
	}
	// L2: check Close error.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return nil
}

// downloadFile fetches a file from a peer and writes it to the folder.
// Returns the relative path of the completed file within root.
func downloadFile(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, expectedHash Hash256, root *os.Root, limiter *rate.Limiter) (string, error) {
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
func downloadFileDelta(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, expectedHash Hash256, root *os.Root, limiter *rate.Limiter) (string, error) {
	if err := validateRelPath(relPath); err != nil {
		return "", err
	}

	// If local file doesn't exist, fall back to full download.
	if _, err := root.Stat(relPath); os.IsNotExist(err) {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}

	// Chunk the local file with FastCDC — its hashes tell the peer
	// which chunks we already have.
	localBlocks, err := signFileRoot(root, relPath)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}
	localInfo, err := root.Stat(relPath)
	if err != nil {
		return downloadFile(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	}
	pbLocal := make([]*pb.Block, len(localBlocks))
	for i, b := range localBlocks {
		pbLocal[i] = &pb.Block{
			Offset: b.Offset,
			Length: int32(b.Length),
			Hash:   append([]byte(nil), b.Hash[:]...),
		}
	}
	sigReq := &pb.BlockSignatures{
		FolderId: folderID,
		Path:     relPath,
		FileSize: localInfo.Size(),
		Blocks:   pbLocal,
	}
	reqData, err := proto.Marshal(sigReq)
	if err != nil {
		return "", fmt.Errorf("marshal block sigs: %w", err)
	}

	u := fmt.Sprintf("https://%s/delta", peerAddr)
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
	// 0 is legal (empty file) — assembleDelta writes an empty tmp.
	remoteFileSize := deltaResp.GetFileSize()
	if remoteFileSize < 0 || remoteFileSize > maxSyncFileSize {
		return "", fmt.Errorf("delta file size out of range for %s: %d", relPath, remoteFileSize)
	}

	// Convert proto blocks to sender-side chunks. Each entry must carry
	// either inline data OR a known hash the receiver can resolve from
	// its local copy.
	pbBlocks := deltaResp.GetBlocks()
	// N5: cap block count to what FastCDC could legitimately produce for
	// a file of remoteFileSize (min chunk size is fastCDCMin). A peer
	// can't force us to allocate an arbitrary-sized chunks slice.
	maxPeerBlocks := (remoteFileSize / int64(fastCDCMin)) + 1
	if int64(len(pbBlocks)) > maxPeerBlocks {
		return "", fmt.Errorf("delta response has %d blocks, exceeds max %d for file size %d", len(pbBlocks), maxPeerBlocks, remoteFileSize)
	}
	chunks := make([]senderChunk, len(pbBlocks))
	for i, b := range pbBlocks {
		if b.GetLength() <= 0 || int(b.GetLength()) > fastCDCMax {
			return "", fmt.Errorf("delta chunk %d invalid length %d", i, b.GetLength())
		}
		if len(b.GetHash()) != 32 {
			return "", fmt.Errorf("delta chunk %d missing hash", i)
		}
		c := senderChunk{
			Offset: b.GetOffset(),
			Length: int(b.GetLength()),
			Hash:   hash256FromBytes(b.GetHash()),
		}
		if data := b.GetData(); len(data) > 0 {
			// D6: payload is zstd-compressed unless the sender marked the
			// file as already-compressed (raw=true).
			plain := data
			if !b.GetRaw() {
				var decErr error
				plain, decErr = zstdutil.Decode(data, int64(fastCDCMax))
				if decErr != nil {
					return "", fmt.Errorf("delta chunk %d zstd decode: %w", i, decErr)
				}
			}
			if len(plain) != c.Length {
				return "", fmt.Errorf("delta chunk %d data len=%d want %d", i, len(plain), c.Length)
			}
			c.Data = plain
		}
		chunks[i] = c
	}

	// Apply delta to reconstruct the file.
	tmpRelPath, err := applyDeltaRoot(root, relPath, peerSuffix(peerAddr), remoteFileSize, chunks)
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
	ExpectedHash Hash256
	RemoteSize   int64
	RemoteMode   uint32
	RemoteMtime  int64 // G1: nanosecond mtime from remote index
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

// downloadBundle fetches multiple small files in a single tar+zstd round-trip (P19).
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
	compressed := zstdutil.Encode(reqData)

	reqURL := fmt.Sprintf("https://%s/bundle", peerAddr)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(compressed))
	if err != nil {
		return nil, entries
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "zstd")
	// We handle decompression below; signal an encoding we do not want Go's
	// Transport to auto-decompress ("identity" or anything other than gzip).
	// The server returns Content-Encoding: zstd regardless.
	httpReq.Header.Set("Accept-Encoding", "zstd")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, entries
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, entries
	}

	// Read tar+zstd response. Cap the compressed body at maxBundleTotal
	// (128 MB) so a malicious peer cannot force unbounded memory/disk
	// consumption via an oversized response. The server enforces the
	// same cap on the sending side.
	var reader io.Reader = io.LimitReader(resp.Body, maxBundleTotal)
	if limiter != nil {
		reader = newRateLimitedReader(ctx, reader, limiter)
	}
	zr, err := zstdutil.NewReader(reader)
	if err != nil {
		return nil, entries
	}
	defer func() { _ = zr.Close() }()
	// Cap the decompressed stream independently of the compressed cap. A
	// zstd bomb can inflate 128 MB of ciphertext into arbitrarily many
	// gigabytes; without this limit tar.Reader would pull the entire
	// decompressed payload. The slack above maxBundleTotal accommodates
	// tar header overhead (512 B per entry, up to maxBundlePaths entries
	// plus per-entry padding to the next 512-byte boundary).
	const tarOverhead = int64(maxBundlePaths) * 1024 // header + worst-case padding per entry
	tr := tar.NewReader(io.LimitReader(zr, maxBundleTotal+tarOverhead))

	received := make(map[string]bool, len(entries))
	suffix := peerSuffix(peerAddr)

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

		// G2: pre-flight disk space check per bundle entry.
		if err := checkDiskSpace(root.Name(), hdr.Size); err != nil {
			slog.Warn("bundle disk space check failed, skipping entry",
				"folder", folderID, "path", hdr.Name, "error", err)
			continue
		}

		// Write to temp, hash during copy.
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

		actualHash := hash256FromBytes(h.Sum(nil))
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

		// G1: preserve remote mtime so the next scan's fast-path skip works.
		if e.RemoteMtime > 0 {
			mt := time.Unix(0, e.RemoteMtime)
			_ = root.Chtimes(hdr.Name, mt, mt)
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
func verifyPostWrite(root *os.Root, relPath string, expectedHash Hash256, folderID, peerAddr string, retries *retryTracker, indexMu *sync.RWMutex) error {
	actualHash, err := hashFileRoot(root, relPath)
	if err != nil {
		slog.Error("C2: post-write verification failed: cannot re-read file",
			"folder", folderID, "path", relPath, "error", err)
		return fmt.Errorf("post-write verify read: %w", err)
	}
	if actualHash != expectedHash {
		slog.Error("C2: post-write verification failed: data corruption detected",
			"folder", folderID, "path", relPath, "peer", peerAddr,
			"expected", expectedHash, "actual", actualHash)
		indexMu.Lock()
		retries.record(relPath, peerAddr, expectedHash)
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
