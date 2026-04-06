package filesync

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

const (
	maxIndexPayload = 10 * 1024 * 1024 // 10 MB
)

// server handles filesync HTTP endpoints.
type server struct {
	node *Node
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/index", s.handleIndex)
	mux.HandleFunc("/file", s.handleFile)
	mux.HandleFunc("/status", s.handleStatus)
	return mux
}

// handleIndex receives a peer's index for a folder and responds with our own.
// POST /index — body: IndexExchange (protobuf), response: IndexExchange (protobuf)
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate peer is configured.
	peerAddr := addrFromRequest(r)
	if !s.node.isPeerConfigured(peerAddr) {
		http.Error(w, "unknown peer", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIndexPayload))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req pb.IndexExchange
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid protobuf: "+err.Error(), http.StatusBadRequest)
		return
	}

	folderID := req.GetFolderId()
	folder := s.node.findFolder(folderID)
	if folder == nil {
		http.Error(w, "unknown folder: "+folderID, http.StatusNotFound)
		return
	}

	slog.Debug("received index from peer", "peer", peerAddr, "folder", folderID, "files", len(req.GetFiles()))

	// Store the remote index for the sync loop to process.
	s.node.storeRemoteIndex(peerAddr, &req)

	// Respond with our index.
	resp := s.node.buildIndexExchange(folderID)
	data, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(data)
}

// handleFile serves a file from a synced folder.
// GET /file?folder=ID&path=PATH&offset=N
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	peerAddr := addrFromRequest(r)
	if !s.node.isPeerConfigured(peerAddr) {
		http.Error(w, "unknown peer", http.StatusForbidden)
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
	if folder.cfg.Direction == "receive-only" {
		http.Error(w, "folder is receive-only", http.StatusForbidden)
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
	_, _ = io.Copy(w, f)
}

// handleStatus returns a JSON summary of the filesync state.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"device_id":%q,"folders":%d}`, s.node.deviceID, len(s.node.cfg.Folders))
}

// addrFromRequest extracts the peer's IP:port for matching against configured peers.
// We only use the IP for matching since the ephemeral source port won't match config.
func addrFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sendIndex pushes our index to a peer and receives their response.
func sendIndex(client *http.Client, peerAddr, folderID string, exchange *pb.IndexExchange) (*pb.IndexExchange, error) {
	data, err := proto.Marshal(exchange)
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}

	u := fmt.Sprintf("http://%s/index", peerAddr)
	resp, err := client.Post(u, "application/x-protobuf", strings.NewReader(string(data)))
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

	var respIdx pb.IndexExchange
	if err := proto.Unmarshal(respBody, &respIdx); err != nil {
		return nil, fmt.Errorf("unmarshal response from %s: %w", peerAddr, err)
	}

	return &respIdx, nil
}

// downloadFromPeer downloads a single file from a peer with resume support.
func downloadFromPeer(client *http.Client, peerAddr, folderID, relPath, expectedHash, folderRoot string) error {
	_, err := downloadFile(client, peerAddr, folderID, relPath, expectedHash, folderRoot)
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

// formatPeerURL builds a URL for a peer, handling potential URL encoding.
func formatPeerURL(peerAddr, path string, params map[string]string) string {
	u := fmt.Sprintf("http://%s%s", peerAddr, path)
	if len(params) > 0 {
		v := url.Values{}
		for k, val := range params {
			v.Set(k, val)
		}
		u += "?" + v.Encode()
	}
	return u
}
