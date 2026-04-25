package filesync

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"github.com/mmdemirbas/mesh/internal/zstdutil"
	"google.golang.org/protobuf/proto"
)

func zstdEncode(data []byte) []byte {
	return zstdutil.Encode(data)
}

func zstdDecode(data []byte) ([]byte, error) {
	return zstdutil.Decode(data, maxIndexPayload*4)
}

// writeProtoZstd marshals msg, zstd-compresses, and writes to w.
func writeProtoZstd(w http.ResponseWriter, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	compressed := zstdEncode(data)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "zstd")
	_, err = w.Write(compressed)
	return err
}

const (
	maxBundlePaths    = 1000              // P19: max files per bundle request
	maxBundleFileSize = 4 * 1024 * 1024   // P19: 4 MB — files larger go through individual download
	maxBundleTotal    = 128 * 1024 * 1024 // P19: 128 MB — total response body cap
	maxIndexPayload   = 10 * 1024 * 1024  // 10 MB — per-page limit
	indexPageSize     = 10_000            // max files per index page
	pendingTTL        = 5 * time.Minute   // stale pending exchange cleanup threshold
	maxTotalPages     = 200               // caps incoming TotalPages to prevent OOM (200 × 10k = 2M files)

	// protocolVersion is the current filesync wire protocol version.
	// Every outgoing IndexExchange sets this; receivers reject mismatches
	// with HTTP 400. There is no v0 — mesh filesync debuted at v1 and
	// never shipped anything older. See docs/filesync/DESIGN-v1.md.
	protocolVersion uint32 = 1
)

// server handles filesync HTTP endpoints.
type server struct {
	node    *Node
	pending sync.Map // key: "deviceID|folderID" -> *pendingExchange
}

// pendingExchange accumulates multi-page index uploads from a peer.
type pendingExchange struct {
	mu         sync.Mutex
	files      []*pb.FileInfo
	received   map[int32]bool
	totalPages int32
	deviceID   string
	folderID   string
	sequence   int64
	since      int64

	// Built after all client pages are received.
	responsePages []*pb.IndexExchange

	createdAt time.Time
}

// evictStalePending removes pending exchange entries older than pendingTTL.
// This catches both incomplete uploads and response caches abandoned by
// disconnected clients.
func (s *server) evictStalePending() {
	now := time.Now()
	s.pending.Range(func(k, v any) bool {
		pe := v.(*pendingExchange)
		pe.mu.Lock()
		stale := now.Sub(pe.createdAt) > pendingTTL
		pe.mu.Unlock()
		if stale {
			s.pending.Delete(k)
		}
		return true
	})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/index", s.handleIndex)
	mux.HandleFunc("/file", s.handleFile)
	mux.HandleFunc("/delta", s.handleDelta)
	mux.HandleFunc("/blocksigs", s.handleBlockSigs)
	mux.HandleFunc("/bundle", s.handleBundle)
	mux.HandleFunc("/status", s.handleStatus)
	return mux
}

// validatePeer checks if a request is from a trusted peer.
// Loopback connections (127.0.0.1, ::1) are always trusted — they arrive via
// SSH tunnels where the tunnel itself provides authentication.
// Non-loopback connections must match a configured peer address.
func (s *server) validatePeer(w http.ResponseWriter, r *http.Request) (string, bool) {
	peerAddr := addrFromRequest(r)
	if isLoopback(peerAddr) {
		return peerAddr, true
	}
	if s.node.isPeerConfigured(peerAddr) {
		return peerAddr, true
	}
	slog.Warn("filesync peer rejected", "peer", peerAddr, "remote", r.RemoteAddr, "path", r.URL.Path)
	http.Error(w, "unknown peer", http.StatusForbidden)
	return peerAddr, false
}

// handleIndex receives a peer's index for a folder and responds with our own.
// Supports paginated exchange for large indices:
//   - Single page (total_pages <= 1): legacy behavior, request-response in one round trip.
//   - Multi-page upload: intermediate pages get empty ack, final page triggers processing.
//   - Fetch (fetch=true): client retrieves remaining server response pages.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.validatePeer(w, r); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIndexPayload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if r.Header.Get("Content-Encoding") == "zstd" {
		body, err = zstdDecode(body)
		if err != nil {
			http.Error(w, "decompress body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	var req pb.IndexExchange
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid protobuf: "+err.Error(), http.StatusBadRequest)
		return
	}

	if v := req.GetProtocolVersion(); v != protocolVersion {
		slog.Warn("rejecting index exchange: protocol version mismatch",
			"peer", req.GetDeviceId(), "folder", req.GetFolderId(),
			"peer_version", v, "local_version", protocolVersion)
		http.Error(w, fmt.Sprintf("protocol version mismatch: peer=%d local=%d", v, protocolVersion), http.StatusBadRequest)
		return
	}

	if req.GetFetch() {
		s.handleIndexFetch(w, &req)
		return
	}

	totalPages := req.GetTotalPages()
	if totalPages <= 1 {
		s.handleSinglePageIndex(w, &req)
		return
	}

	s.handleMultiPageIndex(w, &req)
}

// handleSinglePageIndex handles the legacy single-page index exchange.
func (s *server) handleSinglePageIndex(w http.ResponseWriter, req *pb.IndexExchange) {
	folderID := req.GetFolderId()
	folder := s.node.findFolder(folderID)
	if folder == nil {
		http.Error(w, "unknown folder: "+folderID, http.StatusNotFound)
		return
	}

	slog.Debug("received index from peer", "folder", folderID, "files", len(req.GetFiles()))

	resp := s.node.buildIndexExchange(folderID, req.GetSince())
	if err := writeProtoZstd(w, resp); err != nil {
		http.Error(w, "write response: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// handleMultiPageIndex accumulates pages of a multi-page index upload.
// On receiving the final page, it processes the complete index and returns
// the first page of the server's response (possibly paginated).
func (s *server) handleMultiPageIndex(w http.ResponseWriter, req *pb.IndexExchange) {
	folderID := req.GetFolderId()
	folder := s.node.findFolder(folderID)
	if folder == nil {
		http.Error(w, "unknown folder: "+folderID, http.StatusNotFound)
		return
	}

	key := req.GetDeviceId() + "|" + folderID

	// F8: stale pending exchanges are cleaned up by evictStalePending
	// (periodic goroutine). Removing the per-handler eviction avoids a
	// race where page N evicts a live exchange that page N-1 just created,
	// silently discarding the already-accumulated files.

	totalPages := req.GetTotalPages()
	if totalPages > maxTotalPages {
		slog.Warn("rejecting index exchange: totalPages exceeds cap",
			"peer", req.GetDeviceId(), "folder", folderID, "totalPages", totalPages, "max", maxTotalPages)
		http.Error(w, "totalPages exceeds limit", http.StatusBadRequest)
		return
	}

	v, _ := s.pending.LoadOrStore(key, &pendingExchange{
		totalPages: totalPages,
		deviceID:   req.GetDeviceId(),
		folderID:   folderID,
		sequence:   req.GetSequence(),
		since:      req.GetSince(),
		received:   make(map[int32]bool),
		createdAt:  time.Now(),
	})
	pe := v.(*pendingExchange)

	pe.mu.Lock()
	defer pe.mu.Unlock()

	// A prior multi-page upload for this (device, folder) can survive
	// past its sender — the sender crashed, the network wedged, or the
	// peer restarted and began a new exchange. Without a stale check,
	// the replacement exchange inherits the old pe.totalPages and
	// pe.files, so the completion predicate below never trips and the
	// peer's sync stalls until evictStalePending fires (~5 min). Treat
	// any totalPages or sequence mismatch as a fresh upload: reset the
	// accumulator so the new exchange makes progress from page 1.
	if pe.totalPages != totalPages || pe.sequence != req.GetSequence() {
		slog.Debug("resetting stale pending index exchange",
			"folder", folderID, "peer", req.GetDeviceId(),
			"old_total_pages", pe.totalPages, "new_total_pages", totalPages,
			"old_sequence", pe.sequence, "new_sequence", req.GetSequence(),
			"prior_received", len(pe.received))
		pe.totalPages = totalPages
		pe.sequence = req.GetSequence()
		pe.since = req.GetSince()
		pe.files = nil
		pe.received = make(map[int32]bool)
		pe.responsePages = nil
		pe.createdAt = time.Now()
	}

	// N11: cap total accumulated files to prevent OOM from a peer
	// sending maxTotalPages × indexPageSize entries.
	const maxTotalFiles = 500_000
	incoming := req.GetFiles()
	if len(pe.files)+len(incoming) > maxTotalFiles {
		s.pending.Delete(key) // sync.Map is safe to call under pe.mu
		slog.Warn("rejecting index exchange: total file count exceeds cap",
			"peer", req.GetDeviceId(), "folder", folderID, "total", len(pe.files)+len(incoming), "max", maxTotalFiles)
		http.Error(w, "total file count exceeds limit", http.StatusBadRequest)
		return
	}
	pe.files = append(pe.files, incoming...)
	pe.received[req.GetPage()] = true

	slog.Debug("received index page", "folder", folderID, "page", req.GetPage(),
		"total_pages", pe.totalPages, "received", len(pe.received), "files_so_far", len(pe.files))

	if int32(len(pe.received)) < pe.totalPages {
		// Intermediate page — ack.
		w.WriteHeader(http.StatusOK)
		return
	}

	// All pages received — process the complete index.
	s.pending.Delete(key)

	resp := s.node.buildIndexExchange(folderID, pe.since)
	respPages := paginateResponse(resp)
	pe.responsePages = respPages

	// If the response itself needs pagination, cache it for subsequent fetch requests.
	if len(respPages) > 1 {
		s.pending.Store(key+"|resp", &pendingExchange{
			responsePages: respPages,
			createdAt:     time.Now(),
		})
	}

	slog.Debug("index exchange complete", "folder", folderID,
		"received_files", len(pe.files), "response_files", len(resp.GetFiles()),
		"response_pages", len(respPages))

	if err := writeProtoZstd(w, respPages[0]); err != nil {
		http.Error(w, "write response: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// handleIndexFetch serves a cached response page to a peer that previously
// completed a multi-page upload.
func (s *server) handleIndexFetch(w http.ResponseWriter, req *pb.IndexExchange) {
	key := req.GetDeviceId() + "|" + req.GetFolderId() + "|resp"

	v, ok := s.pending.Load(key)
	if !ok {
		http.Error(w, "no pending response", http.StatusNotFound)
		return
	}
	pe := v.(*pendingExchange)

	pe.mu.Lock()
	defer pe.mu.Unlock()

	page := req.GetPage()
	if int(page) >= len(pe.responsePages) {
		http.Error(w, "page out of range", http.StatusBadRequest)
		return
	}

	// Clean up after the last page is served.
	if int(page) == len(pe.responsePages)-1 {
		s.pending.Delete(key)
	}

	if err := writeProtoZstd(w, pe.responsePages[page]); err != nil {
		http.Error(w, "write response page: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// paginateResponse splits a server response IndexExchange into pages.
func paginateResponse(resp *pb.IndexExchange) []*pb.IndexExchange {
	files := resp.GetFiles()
	if len(files) <= indexPageSize {
		resp.Page = 0
		resp.TotalPages = 1
		return []*pb.IndexExchange{resp}
	}

	totalPages := int32((len(files) + indexPageSize - 1) / indexPageSize)
	pages := make([]*pb.IndexExchange, 0, totalPages)

	for i := range totalPages {
		start := int(i) * indexPageSize
		end := min(start+indexPageSize, len(files))
		pages = append(pages, &pb.IndexExchange{
			DeviceId:        resp.GetDeviceId(),
			FolderId:        resp.GetFolderId(),
			Sequence:        resp.GetSequence(),
			Files:           files[start:end],
			Page:            i,
			TotalPages:      totalPages,
			ProtocolVersion: protocolVersion,
		})
	}
	return pages
}

// handleFile serves a file from a synced folder.
// GET /file?folder=ID&path=PATH&offset=N
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.validatePeer(w, r); !ok {
		return
	}

	folderID := r.URL.Query().Get("folder")
	relPath := r.URL.Query().Get("path")
	offsetStr := r.URL.Query().Get("offset")

	folder := s.node.findFolder(folderID)
	if folder == nil {
		http.Error(w, "unknown folder", http.StatusNotFound)
		return
	}

	// Validate direction: only serve files if we're allowed to send.
	switch folder.cfg.Direction {
	case "receive-only":
		http.Error(w, "folder is receive-only", http.StatusForbidden)
		return
	case "disabled":
		http.Error(w, "folder is disabled", http.StatusForbidden)
		return
	}

	// L5: use folder's os.Root handle to prevent symlink TOCTOU.
	if err := validateRelPath(relPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f, err := folder.root.Open(relPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "open: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	// Handle offset for resume.
	var offset int64
	if offsetStr != "" {
		offset, err = strconv.ParseInt(offsetStr, 10, 64)
		if err != nil || offset < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			http.Error(w, "seek: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	writer := newRateLimitedWriter(r.Context(), w, s.node.rateLimiter)
	n, _ := io.Copy(writer, f)
	if n > 0 {
		folder.metrics.BytesUploaded.Add(n)
	}
}

// handleBundle serves multiple small files in a single tar+zstd response (P19).
// POST /bundle — body: BundleRequest (protobuf, zstd), response: tar+zstd stream.
// Files that can't be read are silently skipped; the client detects missing
// entries by comparing received tar paths against the request.
func (s *server) handleBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.validatePeer(w, r); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIndexPayload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	decoded := body
	if r.Header.Get("Content-Encoding") == "zstd" {
		var zErr error
		decoded, zErr = zstdDecode(body)
		if zErr != nil {
			http.Error(w, "zstd decode: "+zErr.Error(), http.StatusBadRequest)
			return
		}
	}

	var req pb.BundleRequest
	if err := proto.Unmarshal(decoded, &req); err != nil {
		http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Paths) > maxBundlePaths {
		http.Error(w, fmt.Sprintf("too many paths: %d > %d", len(req.Paths), maxBundlePaths), http.StatusBadRequest)
		return
	}

	folder := s.node.findFolder(req.FolderId)
	if folder == nil {
		http.Error(w, "unknown folder", http.StatusNotFound)
		return
	}

	switch folder.cfg.Direction {
	case "receive-only":
		http.Error(w, "folder is receive-only", http.StatusForbidden)
		return
	case "disabled":
		http.Error(w, "folder is disabled", http.StatusForbidden)
		return
	}

	// Build set of indexed paths to prevent probing for arbitrary files.
	folder.indexMu.RLock()
	indexedPaths := make(map[string]bool, len(req.Paths))
	for _, p := range req.Paths {
		if entry, ok := folder.index.Get(p); ok && !entry.Deleted {
			indexedPaths[p] = true
		}
	}
	folder.indexMu.RUnlock()

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Encoding", "zstd")
	zw, err := zstdutil.NewWriter(w)
	if err != nil {
		http.Error(w, "zstd writer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = zw.Close() }()
	tw := tar.NewWriter(zw)
	defer func() { _ = tw.Close() }()

	writer := newRateLimitedWriter(r.Context(), tw, s.node.rateLimiter)
	var totalBytes int64

	for _, relPath := range req.Paths {
		if err := validateRelPath(relPath); err != nil {
			continue
		}
		if !indexedPaths[relPath] {
			continue
		}

		f, err := folder.root.Open(relPath)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		size := info.Size()
		if size > maxBundleFileSize {
			_ = f.Close()
			continue
		}
		if totalBytes+size > maxBundleTotal {
			_ = f.Close()
			break
		}

		hdr := &tar.Header{
			Name: relPath,
			Size: size,
			Mode: int64(info.Mode() & os.ModePerm),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			break
		}
		n, err := io.Copy(writer, f)
		_ = f.Close()
		if err != nil {
			break
		}
		totalBytes += n
		folder.metrics.BytesUploaded.Add(n)
	}
}

// handleDelta receives block signatures from a peer and responds with only
// the blocks that differ between the peer's local version and our version.
// POST /delta — body: BlockSignatures (protobuf), response: DeltaResponse (protobuf)
//
// Compression is per-block inside the protobuf (DeltaBlock.data is zstd-encoded
// unless the incompressibility probe marks the file raw), not body-level. The
// response therefore has no Content-Encoding header — the receiver decodes each
// block individually based on DeltaBlock.raw.
func (s *server) handleDelta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.validatePeer(w, r); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIndexPayload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req pb.BlockSignatures
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid protobuf: "+err.Error(), http.StatusBadRequest)
		return
	}

	folder := s.node.findFolder(req.GetFolderId())
	if folder == nil {
		http.Error(w, "unknown folder", http.StatusNotFound)
		return
	}
	switch folder.cfg.Direction {
	case "receive-only":
		http.Error(w, "folder is receive-only", http.StatusForbidden)
		return
	case "disabled":
		http.Error(w, "folder is disabled", http.StatusForbidden)
		return
	}

	// L5: use folder's os.Root handle to prevent symlink TOCTOU.
	deltaRelPath := req.GetPath()
	if err := validateRelPath(deltaRelPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fi, err := folder.root.Stat(deltaRelPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// N5: cap the number of peer-supplied block signatures to what the
	// local file could possibly produce under FastCDC (bounded by min
	// chunk size). Without this, a peer could flood us with signatures.
	maxBlocks := (fi.Size() + int64(fastCDCMin) - 1) / int64(fastCDCMin)
	if maxBlocks < 1 {
		maxBlocks = 1
	}
	peerBlocks := req.GetBlocks()
	if int64(len(peerBlocks)) > maxBlocks {
		peerBlocks = peerBlocks[:maxBlocks]
	}
	peerHashes := make(map[Hash256]struct{}, len(peerBlocks))
	for _, b := range peerBlocks {
		h := b.GetHash()
		if len(h) != 32 {
			continue
		}
		peerHashes[hash256FromBytes(h)] = struct{}{}
	}

	// Compute delta between our file and the peer's chunk hashes.
	delta, err := computeDeltaRoot(folder.root, deltaRelPath, peerHashes)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "compute delta: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// D6: magic-byte probe — if the file's leading bytes indicate an
	// already-compressed format, mark every block raw and skip zstd.
	raw, err := probeIncompressibleRoot(folder.root, deltaRelPath)
	if err != nil {
		http.Error(w, "probe: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build response. For compressible files we zstd-encode each chunk's
	// data; for raw files we emit data verbatim with raw=true.
	pbBlocks := make([]*pb.DeltaBlock, len(delta))
	for i, c := range delta {
		var payload []byte
		if len(c.Data) > 0 {
			if raw {
				payload = c.Data
			} else {
				payload = zstdutil.Encode(c.Data)
			}
		}
		// Hash: reuse delta's backing array rather than copying. delta
		// is alive until after proto.Marshal returns; Marshal copies
		// bytes into its wire buffer, so no lifetime hazard.
		pbBlocks[i] = &pb.DeltaBlock{
			Offset: c.Offset,
			Length: int32(c.Length),
			Hash:   delta[i].Hash[:],
			Data:   payload,
			Raw:    raw,
		}
	}
	resp := &pb.DeltaResponse{
		FileSize: fi.Size(),
		Blocks:   pbBlocks,
	}

	data, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal delta: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	n, _ := w.Write(data)
	if n > 0 {
		folder.metrics.BytesUploaded.Add(int64(n))
	}
}

// handleBlockSigs returns SHA-256 hashes of each sequential block in a file.
// Used by receivers (C3) to verify incoming bytes per block so a single
// corrupted block can be re-requested without restarting the whole file.
// The hashes come from the sender's authoritative on-disk view at the time
// of the call; callers should cross-check the resulting whole-file hash
// against their expected index hash to catch files that changed between
// /blocksigs and /file.
func (s *server) handleBlockSigs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := s.validatePeer(w, r); !ok {
		return
	}

	folderID := r.URL.Query().Get("folder")
	relPath := r.URL.Query().Get("path")

	folder := s.node.findFolder(folderID)
	if folder == nil {
		http.Error(w, "unknown folder", http.StatusNotFound)
		return
	}
	switch folder.cfg.Direction {
	case "receive-only":
		http.Error(w, "folder is receive-only", http.StatusForbidden)
		return
	case "disabled":
		http.Error(w, "folder is disabled", http.StatusForbidden)
		return
	}

	if err := validateRelPath(relPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fi, err := folder.root.Stat(relPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sigBlocks, err := signFileRoot(folder.root, relPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "compute block sigs: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pbBlocks := make([]*pb.Block, len(sigBlocks))
	for i, b := range sigBlocks {
		pbBlocks[i] = &pb.Block{
			Offset: b.Offset,
			Length: int32(b.Length),
			Hash:   sigBlocks[i].Hash[:],
		}
	}
	resp := &pb.BlockSignatures{
		FolderId: folderID,
		Path:     relPath,
		FileSize: fi.Size(),
		Blocks:   pbBlocks,
	}
	data, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal block sigs: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	if _, err := w.Write(data); err != nil {
		return
	}
}

// handleStatus returns a JSON summary of the filesync state.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_id": s.node.deviceID,
		"folders":   len(s.node.cfg.ResolvedFolders),
	})
}

// addrFromRequest extracts the peer's IP for matching against configured peers.
func addrFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopback returns true if the IP is a loopback address.
// Handles 127.0.0.1, ::1, and IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1).
func isLoopback(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return parsed.IsLoopback()
}

// sendIndex pushes our index to a peer and receives their response.
// For large indices (> indexPageSize files), the exchange is paginated:
// files are split into pages sent as separate HTTP requests.
func sendIndex(ctx context.Context, client *http.Client, peerAddr string, exchange *pb.IndexExchange) (*pb.IndexExchange, error) {
	// Defensive: callers construct IndexExchange via buildIndexExchange
	// which stamps the version, but tests and other entry points may not.
	// Stamp here so every wire message carries the current version.
	if exchange.GetProtocolVersion() == 0 {
		exchange.ProtocolVersion = protocolVersion
	}
	files := exchange.GetFiles()
	if len(files) <= indexPageSize {
		return sendSingleIndex(ctx, client, peerAddr, exchange)
	}
	return sendPaginatedIndex(ctx, client, peerAddr, exchange)
}

// sendSingleIndex sends a complete index in a single HTTP request (legacy path).
func sendSingleIndex(ctx context.Context, client *http.Client, peerAddr string, exchange *pb.IndexExchange) (*pb.IndexExchange, error) {
	exchange.Page = 0
	exchange.TotalPages = 1

	data, err := proto.Marshal(exchange)
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}

	return postIndex(ctx, client, peerAddr, data)
}

// sendPaginatedIndex splits the index into pages and sends them sequentially.
// Intermediate pages get an empty ack. The final page returns the first page of
// the server's response. If the server's response is also paginated, remaining
// pages are fetched via follow-up requests.
func sendPaginatedIndex(ctx context.Context, client *http.Client, peerAddr string, exchange *pb.IndexExchange) (*pb.IndexExchange, error) {
	files := exchange.GetFiles()
	totalPages := int32((len(files) + indexPageSize - 1) / indexPageSize)
	clientDeviceID := exchange.GetDeviceId()

	slog.Debug("sending paginated index", "peer", peerAddr,
		"folder", exchange.GetFolderId(), "files", len(files), "pages", totalPages)

	var firstResp *pb.IndexExchange

	for page := range totalPages {
		start := int(page) * indexPageSize
		end := min(start+indexPageSize, len(files))

		pageExchange := &pb.IndexExchange{
			DeviceId:        clientDeviceID,
			FolderId:        exchange.GetFolderId(),
			Sequence:        exchange.GetSequence(),
			Since:           exchange.GetSince(),
			Files:           files[start:end],
			Page:            page,
			TotalPages:      totalPages,
			ProtocolVersion: protocolVersion,
		}

		data, err := proto.Marshal(pageExchange)
		if err != nil {
			return nil, fmt.Errorf("marshal index page %d: %w", page, err)
		}

		if page < totalPages-1 {
			// Intermediate page — expect empty ack.
			if err := postIndexAck(ctx, client, peerAddr, data); err != nil {
				return nil, fmt.Errorf("send index page %d/%d to %s: %w", page, totalPages, peerAddr, err)
			}
		} else {
			// Final page — expect server's response (first page).
			resp, err := postIndex(ctx, client, peerAddr, data)
			if err != nil {
				return nil, fmt.Errorf("send final index page to %s: %w", peerAddr, err)
			}
			firstResp = resp
		}
	}

	if firstResp == nil {
		return nil, fmt.Errorf("no response from %s", peerAddr)
	}

	// If the server's response is paginated, fetch remaining pages.
	// Use the client's device ID for fetch requests (matches the server's cache key).
	return fetchResponsePages(ctx, client, peerAddr, clientDeviceID, firstResp)
}

// fetchResponsePages fetches remaining server response pages after a paginated
// index exchange. clientDeviceID must match the device ID used during the upload
// phase so the server can locate the cached response.
func fetchResponsePages(ctx context.Context, client *http.Client, peerAddr, clientDeviceID string, firstPage *pb.IndexExchange) (*pb.IndexExchange, error) {
	if firstPage.GetTotalPages() <= 1 {
		return firstPage, nil
	}

	allFiles := append([]*pb.FileInfo{}, firstPage.GetFiles()...)

	for page := int32(1); page < firstPage.GetTotalPages(); page++ {
		fetchReq := &pb.IndexExchange{
			DeviceId:        clientDeviceID,
			FolderId:        firstPage.GetFolderId(),
			Page:            page,
			Fetch:           true,
			ProtocolVersion: protocolVersion,
		}

		data, err := proto.Marshal(fetchReq)
		if err != nil {
			return nil, fmt.Errorf("marshal fetch request page %d: %w", page, err)
		}

		resp, err := postIndex(ctx, client, peerAddr, data)
		if err != nil {
			return nil, fmt.Errorf("fetch response page %d from %s: %w", page, peerAddr, err)
		}
		allFiles = append(allFiles, resp.GetFiles()...)
	}

	return &pb.IndexExchange{
		DeviceId:        firstPage.GetDeviceId(),
		FolderId:        firstPage.GetFolderId(),
		Sequence:        firstPage.GetSequence(),
		Files:           allFiles,
		ProtocolVersion: firstPage.GetProtocolVersion(),
	}, nil
}

// postIndex sends a zstd-compressed index request and returns the parsed response.
func postIndex(ctx context.Context, client *http.Client, peerAddr string, data []byte) (*pb.IndexExchange, error) {
	compressed := zstdEncode(data)

	u := fmt.Sprintf("https://%s/index", peerAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("create index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post index to %s: %w", peerAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("peer %s returned %d: %s", peerAddr, resp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexPayload))
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", peerAddr, err)
	}

	// postIndex callers (sendSingleIndex, sendPaginatedIndex final page,
	// fetchResponsePages) all expect a populated IndexExchange. An empty
	// body here is a peer bug or adversarial response — never mistake it
	// for an ack. Intermediate-page acks go through postIndexAck instead.
	if len(respBody) == 0 {
		return nil, fmt.Errorf("peer %s returned empty index response", peerAddr)
	}

	if resp.Header.Get("Content-Encoding") == "zstd" {
		respBody, err = zstdDecode(respBody)
		if err != nil {
			return nil, fmt.Errorf("decompress response from %s: %w", peerAddr, err)
		}
	}

	var respIdx pb.IndexExchange
	if err := proto.Unmarshal(respBody, &respIdx); err != nil {
		return nil, fmt.Errorf("unmarshal response from %s: %w", peerAddr, err)
	}

	if v := respIdx.GetProtocolVersion(); v != protocolVersion {
		return nil, fmt.Errorf("peer %s protocol version mismatch: peer=%d local=%d", peerAddr, v, protocolVersion)
	}

	return &respIdx, nil
}

// postIndexAck sends a zstd-compressed index page and expects an empty ack.
func postIndexAck(ctx context.Context, client *http.Client, peerAddr string, data []byte) error {
	compressed := zstdEncode(data)

	u := fmt.Sprintf("https://%s/index", peerAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("create index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post index to %s: %w", peerAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s returned %d", peerAddr, resp.StatusCode)
	}
	return nil
}

// downloadFromPeer downloads a single file from a peer with resume support.
func downloadFromPeer(ctx context.Context, client *http.Client, peerAddr, folderID, relPath string, expectedHash Hash256, root *os.Root, limiter *rate.Limiter) error {
	_, err := downloadFileDelta(ctx, client, peerAddr, folderID, relPath, expectedHash, root, limiter)
	return err
}

// peerMatchesAddr checks if a configured peer address matches a request IP.
// Both sides are parsed with net.ParseIP and compared via net.IP.Equal so that
// differently formatted but numerically identical IPv6 addresses (e.g.
// "2001:db8::1" vs. "2001:db8:0:0:0:0:0:1") match correctly. When either side
// does not parse as an IP (hostname, literal mismatch), falls back to a case-
// insensitive string compare so the existing hostname path keeps working until
// B8 introduces real DNS resolution.
func peerMatchesAddr(peerAddr, requestIP string) bool {
	host, _, err := net.SplitHostPort(peerAddr)
	if err != nil {
		host = peerAddr
	}
	host = canonicalizeLocalhost(host)
	req := canonicalizeLocalhost(requestIP)
	if peerIPsEqual(host, req) {
		return true
	}
	return strings.EqualFold(host, req)
}

// canonicalizeLocalhost rewrites the literal "localhost" to 127.0.0.1 so the
// downstream IP comparison treats it consistently regardless of /etc/hosts.
func canonicalizeLocalhost(s string) string {
	if strings.EqualFold(s, "localhost") {
		return "127.0.0.1"
	}
	return s
}

// peerIPsEqual parses both sides as IPs and returns true when they are the
// same address. Loopback is canonicalized so 127.0.0.1, ::1, and ::ffff:127.0.0.1
// all compare equal.
func peerIPsEqual(a, b string) bool {
	ipA := net.ParseIP(a)
	ipB := net.ParseIP(b)
	if ipA == nil || ipB == nil {
		return false
	}
	if ipA.IsLoopback() && ipB.IsLoopback() {
		return true
	}
	return ipA.Equal(ipB)
}
