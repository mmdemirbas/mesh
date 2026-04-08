package filesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

func gzipEncode(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipDecode(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// writeProtoGzip marshals msg, gzip-compresses, and writes to w.
func writeProtoGzip(w http.ResponseWriter, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	compressed, err := gzipEncode(data)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "gzip")
	_, err = w.Write(compressed)
	return err
}

const (
	maxIndexPayload = 10 * 1024 * 1024 // 10 MB — per-page limit
	indexPageSize   = 10_000           // max files per index page
	pendingTTL      = 5 * time.Minute  // stale pending exchange cleanup threshold
	maxTotalPages   = 200              // caps incoming TotalPages to prevent OOM (200 × 10k = 2M files)
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

	if r.Header.Get("Content-Encoding") == "gzip" {
		body, err = gzipDecode(body)
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
	if err := writeProtoGzip(w, resp); err != nil {
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

	// Evict stale pending exchange if present.
	if v, ok := s.pending.Load(key); ok {
		pe := v.(*pendingExchange)
		pe.mu.Lock()
		stale := time.Since(pe.createdAt) > pendingTTL
		pe.mu.Unlock()
		if stale {
			s.pending.Delete(key)
		}
	}

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

	pe.files = append(pe.files, req.GetFiles()...)
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

	if err := writeProtoGzip(w, respPages[0]); err != nil {
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

	if err := writeProtoGzip(w, pe.responsePages[page]); err != nil {
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

	for i := int32(0); i < totalPages; i++ {
		start := int(i) * indexPageSize
		end := start + indexPageSize
		if end > len(files) {
			end = len(files)
		}
		pages = append(pages, &pb.IndexExchange{
			DeviceId:   resp.GetDeviceId(),
			FolderId:   resp.GetFolderId(),
			Sequence:   resp.GetSequence(),
			Files:      files[start:end],
			Page:       i,
			TotalPages: totalPages,
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

	fullPath, err := safePath(folder.cfg.Path, relPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f, err := os.Open(fullPath) //nolint:gosec // G304: validated by safePath
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
	_, _ = io.Copy(writer, f)
}

// handleDelta receives block signatures from a peer and responds with only
// the blocks that differ between the peer's local version and our version.
// POST /delta — body: BlockSignatures (protobuf), response: DeltaResponse (protobuf)
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

	fullPath, err := safePath(folder.cfg.Path, req.GetPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	blockSize := req.GetBlockSize()
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}

	// Compute delta between our file and the peer's block hashes.
	delta, err := computeDeltaBlocks(fullPath, blockSize, req.GetBlockHashes())
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "compute delta: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Get full-file hash and size for verification.
	fileHash, err := hashFile(fullPath)
	if err != nil {
		http.Error(w, "hash file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fi, err := os.Stat(fullPath)
	if err != nil {
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build response.
	pbBlocks := make([]*pb.DeltaBlock, len(delta))
	for i, b := range delta {
		pbBlocks[i] = &pb.DeltaBlock{Index: b.index, Data: b.data}
	}
	resp := &pb.DeltaResponse{
		FileSize:   fi.Size(),
		FileSha256: hexToBytes(fileHash),
		Blocks:     pbBlocks,
	}

	data, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal delta: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(data)
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

// isLoopback returns true if the IP is a loopback address (127.0.0.1 or ::1).
func isLoopback(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1"
}

// sendIndex pushes our index to a peer and receives their response.
// For large indices (> indexPageSize files), the exchange is paginated:
// files are split into pages sent as separate HTTP requests.
func sendIndex(ctx context.Context, client *http.Client, peerAddr string, exchange *pb.IndexExchange) (*pb.IndexExchange, error) {
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

	for page := int32(0); page < totalPages; page++ {
		start := int(page) * indexPageSize
		end := start + indexPageSize
		if end > len(files) {
			end = len(files)
		}

		pageExchange := &pb.IndexExchange{
			DeviceId:   clientDeviceID,
			FolderId:   exchange.GetFolderId(),
			Sequence:   exchange.GetSequence(),
			Since:      exchange.GetSince(),
			Files:      files[start:end],
			Page:       page,
			TotalPages: totalPages,
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
			DeviceId: clientDeviceID,
			FolderId: firstPage.GetFolderId(),
			Page:     page,
			Fetch:    true,
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
		DeviceId: firstPage.GetDeviceId(),
		FolderId: firstPage.GetFolderId(),
		Sequence: firstPage.GetSequence(),
		Files:    allFiles,
	}, nil
}

// postIndex sends a gzip-compressed index request and returns the parsed response.
func postIndex(ctx context.Context, client *http.Client, peerAddr string, data []byte) (*pb.IndexExchange, error) {
	compressed, err := gzipEncode(data)
	if err != nil {
		return nil, fmt.Errorf("compress index: %w", err)
	}

	u := fmt.Sprintf("http://%s/index", peerAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("create index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
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

	// Empty body is a valid ack for intermediate pages.
	if len(respBody) == 0 {
		return &pb.IndexExchange{}, nil
	}

	if resp.Header.Get("Content-Encoding") == "gzip" {
		respBody, err = gzipDecode(respBody)
		if err != nil {
			return nil, fmt.Errorf("decompress response from %s: %w", peerAddr, err)
		}
	}

	var respIdx pb.IndexExchange
	if err := proto.Unmarshal(respBody, &respIdx); err != nil {
		return nil, fmt.Errorf("unmarshal response from %s: %w", peerAddr, err)
	}

	return &respIdx, nil
}

// postIndexAck sends a gzip-compressed index page and expects an empty ack.
func postIndexAck(ctx context.Context, client *http.Client, peerAddr string, data []byte) error {
	compressed, err := gzipEncode(data)
	if err != nil {
		return fmt.Errorf("compress index: %w", err)
	}

	u := fmt.Sprintf("http://%s/index", peerAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("create index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
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
func downloadFromPeer(ctx context.Context, client *http.Client, peerAddr, folderID, relPath, expectedHash, folderRoot string, limiter *rate.Limiter) error {
	_, err := downloadFileDelta(ctx, client, peerAddr, folderID, relPath, expectedHash, folderRoot, limiter)
	return err
}

// peerMatchesAddr checks if a configured peer address matches a request IP.
func peerMatchesAddr(peerAddr, requestIP string) bool {
	host, _, err := net.SplitHostPort(peerAddr)
	if err != nil {
		host = peerAddr
	}
	// Normalize localhost variants.
	if host == "localhost" {
		host = "127.0.0.1"
	}
	reqHost := requestIP
	if reqHost == "localhost" {
		reqHost = "127.0.0.1"
	}
	// Handle IPv6 loopback.
	if host == "::1" {
		host = "127.0.0.1"
	}
	if reqHost == "::1" {
		reqHost = "127.0.0.1"
	}
	return host == reqHost
}
