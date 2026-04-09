package clipsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	config "github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
	"github.com/mmdemirbas/mesh/internal/gziputil"
	"github.com/mmdemirbas/mesh/internal/nodeutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

func gzipEncode(data []byte) ([]byte, error) {
	return gziputil.Encode(data)
}

func gzipDecode(data []byte) ([]byte, error) {
	return gziputil.Decode(data, maxClipboardPayload*2)
}

const (
	Port         = 7755
	MagicHeader  = "CLPSYNC2"
	PollInterval = 3 * time.Second // Default clipboard polling interval

	// maxSyncFileSize is the per-file size limit for clipboard sync.
	// Files larger than this are skipped to avoid OOM and transfer timeouts.
	maxSyncFileSize = 50 * 1024 * 1024 // 50 MB

	// maxClipboardPayload caps the total size of all clipboard formats read locally.
	// Prevents OOM when a large image or multiple large items are on the clipboard.
	maxClipboardPayload = 100 * 1024 * 1024 // 100 MB

	// maxRequestBodySize caps the /sync endpoint body.
	// Allows up to ~20 files at maxSyncFileSize with base64 overhead (~33%).
	maxRequestBodySize = maxSyncFileSize * 20 * 4 / 3

	// maxPeers limits the number of dynamically discovered peers to prevent
	// OOM from an attacker flooding unique source addresses via UDP.
	// Kept very low — typical LAN setups have 2-10 peers.
	maxPeers = 32

	// activityHistorySize is the number of recent clipboard activities to retain.
	activityHistorySize = 20
)

// ClipActivity describes a single clipboard sync event.
type ClipActivity struct {
	Direction string    `json:"direction"` // "send" or "receive"
	Size      int64     `json:"size"`      // payload size in bytes
	Formats   []string  `json:"formats"`   // MIME types or file names
	Peer      string    `json:"peer"`      // peer address (receive only)
	Time      time.Time `json:"time"`
}

// activeNodes tracks running clipsync nodes for admin API access.
var activeNodes nodeutil.Registry[Node]

// GetActivities returns recent clipboard activities across all active nodes.
func GetActivities() []ClipActivity {
	var result []ClipActivity
	activeNodes.ForEach(func(n *Node) {
		n.activityMu.RLock()
		result = append(result, n.activities...)
		n.activityMu.RUnlock()
	})
	// Sort by time descending (most recent first).
	sort.Slice(result, func(i, j int) bool {
		return result[i].Time.After(result[j].Time)
	})
	if len(result) > activityHistorySize {
		result = result[:activityHistorySize]
	}
	return result
}

type Node struct {
	ctx    context.Context // parent context; cancelled on shutdown
	config config.ClipsyncCfg
	id     string
	port   int

	peers      map[string]time.Time // Tracks dynamic UDP peers
	peerHashes map[string]string    // Tracks last seen hash per peer
	peersMu    sync.RWMutex

	httpClient *http.Client
	filesDir   string

	stateMu         sync.Mutex
	lastHash        string
	lastWrittenHash string // hash of content written from a peer, to suppress echo re-broadcast
	originAddr      string // peer address that last pushed content to us; cleared on local broadcast
	lastPayload     []byte
	notifyCh        chan struct{}

	currentFilesMu sync.Mutex
	currentFiles   []string // absolute paths of files in filesDir tied to current clipboard content

	clipMu sync.Mutex // serializes all clipboard read/write process spawning

	activityMu sync.RWMutex
	activities []ClipActivity // ring buffer, most recent last
}

// Start initializes the mesh node based on the provided configuration.
func Start(ctx context.Context, cfg config.ClipsyncCfg) (*Node, error) {
	// Defaults
	port := Port
	magicHeader := MagicHeader
	pollInterval := PollInterval
	if cfg.PollInterval != "" {
		if d, err := time.ParseDuration(cfg.PollInterval); err == nil && d > 0 {
			pollInterval = d
		} else {
			return nil, fmt.Errorf("clipsync: invalid poll_interval %q: %w", cfg.PollInterval, err)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("clipsync: cannot determine home directory: %w", err)
	}
	filesDir := filepath.Join(home, ".mesh", "clipsync")
	if err := os.MkdirAll(filesDir, 0750); err != nil {
		return nil, fmt.Errorf("clipsync: cannot create files directory: %w", err)
	}

	_, portStr, _ := net.SplitHostPort(cfg.Bind)

	_, _ = fmt.Sscanf(portStr, "%d", &port)

	n := &Node{
		ctx:        ctx,
		config:     cfg,
		id:         generateID(),
		port:       port,
		peers:      make(map[string]time.Time),
		peerHashes: make(map[string]string),
		httpClient: &http.Client{Timeout: 2 * time.Minute},
		filesDir:   filesDir,
		notifyCh:   make(chan struct{}, 1),
	}

	n.purgeFilesDir() // remove any files left over from a previous session

	go n.runHTTPServer(ctx)

	if len(cfg.LANDiscoveryGroup) > 0 {
		go n.runUDPServer(ctx, magicHeader, port)
		go n.runUDPBeacon(ctx, magicHeader, port)
		go n.cleanupPeers(ctx)
		go n.refreshHTTPRegistration(ctx)
	}

	m := state.Global.GetMetrics("clipsync", cfg.Bind)
	m.StartTime.Store(time.Now().UnixNano())
	state.Global.Update("clipsync", cfg.Bind, state.Listening, "")
	for _, addr := range cfg.StaticPeers {
		state.Global.Update("clipsync-peer", cfg.Bind+"|"+addr, state.Connected, "static")
	}

	activeNodes.Register(n)
	context.AfterFunc(ctx, func() { activeNodes.Unregister(n) })

	go n.pollClipboard(ctx, pollInterval)
	return n, nil
}

// recordActivity appends a clipboard activity to the ring buffer and updates
// the state message shown in the dashboard.
func (n *Node) recordActivity(direction string, size int64, formats []string, peer string) {
	a := ClipActivity{
		Direction: direction,
		Size:      size,
		Formats:   formats,
		Peer:      peer,
		Time:      time.Now(),
	}
	n.activityMu.Lock()
	n.activities = append(n.activities, a)
	if len(n.activities) > activityHistorySize {
		n.activities = n.activities[len(n.activities)-activityHistorySize:]
	}
	n.activityMu.Unlock()

	// Update metrics.
	m := state.Global.GetMetrics("clipsync", n.config.Bind)
	if direction == "send" {
		m.BytesTx.Add(size)
	} else {
		m.BytesRx.Add(size)
	}

	// Update component message with last activity summary.
	summary := formatActivitySummary(a)
	state.Global.Update("clipsync", n.config.Bind, state.Listening, summary)
}

// formatActivitySummary builds a compact human-readable summary of a clipboard activity.
func formatActivitySummary(a ClipActivity) string {
	dir := "sent"
	if a.Direction == "receive" {
		dir = "received"
	}
	fmts := strings.Join(a.Formats, ", ")
	if fmts == "" {
		fmts = "unknown"
	}
	return fmt.Sprintf("%s %s %s", dir, formatBytes(a.Size), fmts)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ─── Network Logic ──────────────────────────────────────────────────────────

// canSendTo returns whether the node should push clipboard data to addr.
// Sending is always allowed — the target is already in our configured/discovered peer list.
func (n *Node) canSendTo(_ string, _ bool) bool { return true }

// canReceiveFrom returns whether the node should accept clipboard data from addr.
// Trusted sources: loopback (SSH tunnels), configured static peers, UDP-discovered peers.
func (n *Node) canReceiveFrom(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	// Check static peers.
	for _, peer := range n.config.StaticPeers {
		peerHost, _, err := net.SplitHostPort(peer)
		if err != nil {
			peerHost = peer
		}
		if peerHost == host {
			return true
		}
	}
	// Check dynamically discovered peers.
	n.peersMu.RLock()
	defer n.peersMu.RUnlock()
	for peer := range n.peers {
		peerHost, _, err := net.SplitHostPort(peer)
		if err != nil {
			peerHost = peer
		}
		if peerHost == host {
			return true
		}
	}
	return false
}

// groupOverlaps returns true if the remote group matches any of our configured groups.
func (n *Node) groupOverlaps(remoteGroup string) bool {
	for _, g := range n.config.LANDiscoveryGroup {
		if g == remoteGroup {
			return true
		}
	}
	return false
}

// primaryGroup returns the first configured LAN discovery group, or empty string.
func (n *Node) primaryGroup() string {
	if len(n.config.LANDiscoveryGroup) > 0 {
		return n.config.LANDiscoveryGroup[0]
	}
	return ""
}

// containsIP checks if the IP in host matches any entry in the slice,
// handling IPv6-mapped IPv4 addresses (e.g., "::ffff:192.168.1.5" matches "192.168.1.5").
func containsIP(slice []string, host string) bool {
	hostIP := net.ParseIP(host)
	if hostIP == nil {
		// Not a valid IP, fall back to string match
		return contains(slice, host)
	}
	for _, s := range slice {
		entryIP := net.ParseIP(s)
		if entryIP != nil && hostIP.Equal(entryIP) {
			return true
		}
		if s == host {
			return true
		}
	}
	return false
}

func (n *Node) Broadcast(payload *pb.SyncPayload) {
	raw, _ := proto.Marshal(payload)
	data, err := gzipEncode(raw)
	if err != nil {
		slog.Warn("Failed to compress clipboard payload", "error", err)
		data = raw // fall back to uncompressed
	}

	// Record send activity (raw size for accurate metrics).
	formats := extractFormatNames(payload)
	n.recordActivity("send", int64(len(raw)), formats, "")

	n.stateMu.Lock()
	n.lastPayload = data
	origin := n.originAddr
	n.originAddr = "" // locally originated broadcast; clear for next cycle
	n.stateMu.Unlock()

	select {
	case n.notifyCh <- struct{}{}:
	default:
	}

	// Send to Dynamic UDP Peers
	n.peersMu.RLock()
	for addr := range n.peers {
		if addr == origin {
			continue // don't echo back to the peer we received from
		}
		if n.canSendTo(addr, true) {
			slog.Debug("Pushing payload via HTTP POST to dynamic peer", "peer", addr)
			go n.postHTTP(addr, data)
		}
	}
	n.peersMu.RUnlock()

	// Send to Static Peers (SSH Tunnels or explicit IP)
	for _, addr := range n.config.StaticPeers {
		if addr == origin {
			continue // don't echo back to the peer we received from
		}
		if n.canSendTo(addr, false) {
			slog.Debug("Pushing payload via HTTP POST to static peer", "peer", addr)
			go n.postHTTP(addr, data)
		}
	}
}

func (n *Node) postHTTP(addr string, data []byte) {
	ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://%s/sync", addr), bytes.NewReader(data))
	if err != nil {
		slog.Warn("Invalid peer address for sync push", "peer", cleanLogStr(addr), "error", err)
		return
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := n.httpClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// cleanLogStr replaces ASCII control characters (including newlines) with '?'
// to prevent log injection when logging peer-supplied strings.
func cleanLogStr(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// ─── HTTP Server & File Handling ─────────────────────────────────────────────

// extractFormatNames returns a summary of the payload content types.
func extractFormatNames(p *pb.SyncPayload) []string {
	if files := p.GetFiles(); len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.GetFileName())
		}
		return names
	}
	seen := make(map[string]bool)
	var mimes []string
	for _, f := range p.GetFormats() {
		mt := f.GetMimeType()
		if mt != "" && !seen[mt] {
			seen[mt] = true
			mimes = append(mimes, mt)
		}
	}
	return mimes
}

func (n *Node) processPayload(p *pb.SyncPayload, bodySize int, peerHostPort string) {
	// Record receive activity using the original body size to avoid re-marshaling.
	formats := extractFormatNames(p)
	n.recordActivity("receive", int64(bodySize), formats, peerHostPort)

	if len(p.GetFiles()) > 0 {
		var writtenPaths []string
		for _, f := range p.GetFiles() {
			// Sanitize filename: filepath.Base strips directory components,
			// then reject any remaining unsafe names or path separator characters.
			safeName := filepath.Base(f.GetFileName())
			if safeName == "." || safeName == ".." || safeName == "" ||
				strings.ContainsAny(safeName, "/\\") {
				slog.Warn("Rejected clipboard file with unsafe name", "file", cleanLogStr(f.GetFileName()))
				continue
			}
			destPath := filepath.Join(n.filesDir, safeName)
			if len(f.GetData()) > 0 {
				if err := os.WriteFile(destPath, f.GetData(), 0600); err != nil {
					slog.Warn("Failed to save clipboard file", "file", cleanLogStr(f.GetFileName()), "error", err)
					continue
				}
			} else if err := n.downloadFile(f.GetFileId(), f.GetFileName(), peerHostPort); err != nil {
				slog.Warn("Failed to download clipboard file", "file", cleanLogStr(f.GetFileName()), "peer", cleanLogStr(peerHostPort), "error", err) //nolint:gosec // G706: values sanitized via cleanLogStr above
				continue
			}
			writtenPaths = append(writtenPaths, destPath)
		}
		if len(writtenPaths) > 0 {
			n.clipMu.Lock()
			n.setCurrentFiles(writtenPaths)
			n.setWrittenHash(hashFilePaths(writtenPaths), peerHostPort)
			clipWriteFiles(writtenPaths)
			n.clipMu.Unlock()
		}
	} else if len(p.GetFormats()) > 0 {
		n.clipMu.Lock()
		n.clearCurrentFiles()
		n.setWrittenHash(hashFormats(p.GetFormats()), peerHostPort)
		clipWriteFormats(p.GetFormats())
		n.clipMu.Unlock()
	}
}

func (n *Node) pullHTTP(peerAddr string) {
	slog.Debug("Making outbound HTTP GET pull request", "peer", cleanLogStr(peerAddr)) //nolint:gosec // G706: sanitized via cleanLogStr
	ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/clip", peerAddr), nil) //nolint:gosec // G704: peer addresses are user-configured, not untrusted input
	if err != nil {
		return
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		slog.Debug("Failed to pull from peer", "peer", cleanLogStr(peerAddr), "error", err) //nolint:gosec // G706: sanitized via cleanLogStr
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		slog.Debug("Failed to pull from peer", "peer", cleanLogStr(peerAddr), "status", resp.StatusCode) //nolint:gosec // G706: sanitized via cleanLogStr
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	if err != nil {
		slog.Debug("Failed to read pulled payload", "peer", cleanLogStr(peerAddr), "error", err) //nolint:gosec // G706: sanitized via cleanLogStr
		return
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body, err = gzipDecode(body)
		if err != nil {
			slog.Debug("Failed to decompress pulled payload", "peer", cleanLogStr(peerAddr), "error", err) //nolint:gosec // G706: sanitized via cleanLogStr
			return
		}
	}
	var p pb.SyncPayload
	if err := proto.Unmarshal(body, &p); err != nil {
		slog.Debug("Failed to decode pulled payload", "peer", cleanLogStr(peerAddr), "error", err) //nolint:gosec // G706: sanitized via cleanLogStr
		return
	}

	slog.Info("Successfully pulled and ingested payload", "formats", len(p.GetFormats()), "files", len(p.GetFiles()), "peer", cleanLogStr(peerAddr)) //nolint:gosec // G706: sanitized via cleanLogStr
	n.processPayload(&p, len(body), peerAddr)
}

func (n *Node) runHTTPServer(ctx context.Context) {
	mux := http.NewServeMux()

	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if err.Error() == "http: request body too large" {
				slog.Warn("Rejected oversized sync payload", "from", cleanLogStr(r.RemoteAddr)) //nolint:gosec // G706: sanitized via cleanLogStr
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "bad request", http.StatusBadRequest)
			}
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			body, err = gzipDecode(body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
		var p pb.SyncPayload
		if err := proto.Unmarshal(body, &p); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peerHostPort := net.JoinHostPort(host, fmt.Sprintf("%d", n.port))

		slog.Info("Received pushed payload via HTTP POST", "formats", len(p.GetFormats()), "from", cleanLogStr(r.RemoteAddr)) //nolint:gosec // G706: sanitized via cleanLogStr
		n.processPayload(&p, len(body), peerHostPort)
	})

	mux.HandleFunc("/clip", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}

		n.stateMu.Lock()
		data := n.lastPayload
		n.stateMu.Unlock()

		if data == nil {
			http.Error(w, "No content", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(data)
	})

	// HTTP-based peer discovery for firewall-blocked networks.
	// When a peer discovers us via UDP, it calls this endpoint so we
	// also register it — even if our UDP replies cannot reach it.
	mux.HandleFunc("/discover", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var msg pb.DiscoverRequest
		if err := proto.Unmarshal(body, &msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if msg.GetId() == n.id {
			return // self-discovery
		}
		if !n.groupOverlaps(msg.GetGroup()) {
			return // different group
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peerAddr := net.JoinHostPort(host, fmt.Sprintf("%d", msg.GetPort()))

		_, needsPull := n.registerPeer(peerAddr, msg.GetHash(), "http")
		if needsPull {
			go n.pullHTTP(peerAddr)
		}
	})

	// Serve files for peers to download, with ACL check
	fileServer := http.StripPrefix("/files/", http.FileServer(http.Dir(n.filesDir)))
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              n.config.Bind,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("Clipsync HTTP listening", "bind", n.config.Bind)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("Clipsync HTTP server failed", "error", err)
	}
}

func (n *Node) downloadFile(fileID, fileName, peerAddr string) error {
	// Sanitize both fileID (used in URL) and fileName (used in local path) to prevent traversal
	safeID := filepath.Base(fileID)
	safeName := filepath.Base(fileName)
	if safeName == "." || safeName == ".." || safeName == "" {
		return fmt.Errorf("unsafe file name: %q", fileName)
	}

	resp, err := n.httpClient.Get(fmt.Sprintf("http://%s/files/%s", peerAddr, safeID)) //nolint:gosec // G704: peer addresses are user-configured, not untrusted input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: HTTP %d", safeID, resp.StatusCode)
	}

	dst, err := os.Create(filepath.Join(n.filesDir, safeName)) //nolint:gosec // G304: safeName is filepath.Base-sanitized above
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	_, err = io.Copy(dst, io.LimitReader(resp.Body, maxSyncFileSize))
	return err
}

// ─── OS Clipboard Monitor ────────────────────────────────────────────────────

func (n *Node) pollClipboard(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop() // Prevent ticker leak

	var polling atomic.Bool
	var lastSeq uint32

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Skip this tick if the previous one is still running.
		if !polling.CompareAndSwap(false, true) {
			continue
		}

		// Cross-platform sequence check.
		// On Windows, this prevents spawning powershell.exe if the clipboard hasn't changed.
		// On macOS/Linux, it returns 0 and proceeds normally.
		seq := getOSClipSeq()
		if seq != 0 && seq == lastSeq {
			polling.Store(false)
			continue
		}
		lastSeq = seq

		// Read clipboard state under the mutex so that concurrent
		// writes from processPayload cannot race with our reads.
		n.clipMu.Lock()

		// 1. Check Files
		paths := readFiles()
		if len(paths) > 0 {
			n.clipMu.Unlock()
			h := hashFilePaths(paths)
			if n.checkHash(h) {
				slog.Debug("Local clipboard files changed", "hash", h, "count", len(paths))
				n.handleFileBroadcast(paths)
			}
			polling.Store(false)
			continue
		}

		// No files on clipboard: any previously tracked files are now orphaned.
		n.clearCurrentFiles()

		// 2. Read all clipboard formats (text, html, rtf, image) in one call.
		formats := readClipboardFormats()
		n.clipMu.Unlock()
		if len(formats) > 0 {
			h := hashFormats(formats)
			if n.checkHash(h) {
				slog.Debug("Local clipboard changed", "hash", h, "formats", len(formats))
				n.Broadcast(&pb.SyncPayload{Formats: formats})
			}
		}

		polling.Store(false)
	}
}

func (n *Node) handleFileBroadcast(paths []string) {
	if len(paths) == 0 {
		return
	}
	n.clearCurrentFiles() // delete files cached from the previous clipboard content
	var files []*pb.FileRef
	var storedPaths []string
	for _, src := range paths {
		fileName := filepath.Base(src)

		info, err := os.Stat(src)
		if err != nil {
			slog.Warn("Failed to stat clipboard file", "path", src, "error", err)
			continue
		}
		if info.Size() > maxSyncFileSize {
			slog.Warn("Skipping clipboard file: too large for sync", "file", fileName,
				"size_mb", info.Size()>>20, "limit_mb", maxSyncFileSize>>20)
			continue
		}

		fileID := generateID() + filepath.Ext(fileName)
		dest := filepath.Join(n.filesDir, fileID)
		input, err := os.ReadFile(src) //nolint:gosec // G304: src is a local clipboard file path returned by the OS, not user input
		if err != nil {
			slog.Warn("Failed to read clipboard file", "path", src, "error", err)
			continue
		}
		if err := os.WriteFile(dest, input, 0600); err != nil { //nolint:gosec // G703: dest = filesDir+generateID()+safe-ext; no traversal possible
			slog.Warn("Failed to store clipboard file", "path", dest, "error", err)
			continue
		}
		files = append(files, &pb.FileRef{FileId: fileID, FileName: fileName, Data: input})
		storedPaths = append(storedPaths, dest)
	}
	if len(files) > 0 {
		n.setCurrentFiles(storedPaths) // track so they're deleted when clipboard changes
		n.Broadcast(&pb.SyncPayload{Files: files})
	}
}

// ─── Helpers & OS Bindings (Text/Files only shown for brevity) ───────────────

func (n *Node) checkHash(h string) bool {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	if h == n.lastHash || h == n.lastWrittenHash {
		return false
	}
	n.lastHash = h
	return true
}

// clearCurrentFiles deletes all files in filesDir that are associated with the
// current clipboard content and resets the tracking slice. It is a no-op when
// there are no tracked files, so it is safe to call on every poll tick.
func (n *Node) clearCurrentFiles() {
	n.currentFilesMu.Lock()
	defer n.currentFilesMu.Unlock()
	if len(n.currentFiles) == 0 {
		return
	}
	for _, path := range n.currentFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Debug("Failed to remove old clipboard file", "path", path, "error", err)
		}
	}
	n.currentFiles = nil
}

// setCurrentFiles atomically replaces the tracked file set, deleting any
// previously tracked files first.
func (n *Node) setCurrentFiles(paths []string) {
	n.clearCurrentFiles()
	n.currentFilesMu.Lock()
	n.currentFiles = paths
	n.currentFilesMu.Unlock()
}

// purgeFilesDir removes every file in filesDir unconditionally. Called once at
// startup to discard files left over from a previous (possibly crashed) session.
func (n *Node) purgeFilesDir() {
	entries, err := os.ReadDir(n.filesDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(n.filesDir, e.Name()))
	}
	if len(entries) > 0 {
		slog.Debug("Purged leftover clipsync files from previous session", "count", len(entries))
	}
}

// setWrittenHash records the hash of content written from a peer so that
// the next poll cycle does not re-broadcast it as a local change.
func (n *Node) setWrittenHash(h, origin string) {
	n.stateMu.Lock()
	n.lastHash = h
	n.lastWrittenHash = h
	n.originAddr = origin
	n.stateMu.Unlock()
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hashFilePaths returns a stable, order-independent hash of a set of file paths.
func hashFilePaths(paths []string) string {
	// Fast path: single path (common case) avoids copy+sort+join.
	if len(paths) == 1 {
		h := sha256.Sum256([]byte(paths[0])) //nolint:gosec // G602: false positive — len(paths)==1 is checked above; sha256 always returns [32]byte
		return hex.EncodeToString(h[:])
	}
	// Check if already sorted.
	needsSort := false
	for i := 1; i < len(paths); i++ {
		if paths[i] < paths[i-1] {
			needsSort = true
			break
		}
	}
	if needsSort {
		sorted := make([]string, len(paths))
		copy(sorted, paths)
		sort.Strings(sorted)
		h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
		return hex.EncodeToString(h[:])
	}
	h := sha256.Sum256([]byte(strings.Join(paths, "\n")))
	return hex.EncodeToString(h[:])
}

// hashFormats returns a stable hash of a set of clipboard formats.
func hashFormats(formats []*pb.ClipFormat) string {
	// Fast path: single format (most common case) avoids copy+sort.
	if len(formats) == 1 {
		h := sha256.New()
		h.Write([]byte(formats[0].GetMimeType()))
		h.Write([]byte{0})
		h.Write(formats[0].GetData())
		return hex.EncodeToString(h.Sum(nil))
	}
	// Check if already sorted (common when formats come from a consistent source).
	sorted := formats
	needsSort := false
	for i := 1; i < len(formats); i++ {
		if formats[i].GetMimeType() < formats[i-1].GetMimeType() {
			needsSort = true
			break
		}
	}
	if needsSort {
		sorted = make([]*pb.ClipFormat, len(formats))
		copy(sorted, formats)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetMimeType() < sorted[j].GetMimeType() })
	}
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.GetMimeType()))
		h.Write([]byte{0}) // separator
		h.Write(f.GetData())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ─── OS Clipboard CLI Wrappers ───────────────────────────────────────────────

// utf8Env returns a cached copy of the process environment with LC_ALL
// forced to en_US.UTF-8 so that clipboard tools always produce/consume UTF-8,
// regardless of the user's locale.
var utf8Env = sync.OnceValue(func() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "LC_ALL=") {
			env = append(env, e)
		}
	}
	return append(env, "LC_ALL=en_US.UTF-8")
})

// clipCmd creates an exec.Cmd with UTF-8 locale forced.
func clipCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: intentional — runs OS clipboard tool with known fixed arguments

	// Deep copy the environment slice to prevent StartProcess memory violations
	// when the OS level iterates over the slice concurrently.
	baseEnv := utf8Env()
	envCopy := make([]string, len(baseEnv))
	copy(envCopy, baseEnv)
	cmd.Env = envCopy

	return cmd
}

// ── Multi-format clipboard read/write ────────────────────────────────────────
//
// Reads/writes ALL formats (text, html, rtf, image) in one OS call via temp dir.
// macOS: single osascript using NSPasteboard
// Windows: single PowerShell using System.Windows.Forms.Clipboard
// Linux: multiple xclip/wl-paste calls (one per available type)

// clipTmpDir is a per-process scratch directory for clipboard format exchange.
var clipTmpDir = sync.OnceValue(func() string {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("clipsync_fmt_%d", os.Getpid()))
	_ = os.MkdirAll(dir, 0700)
	return dir
})

func clearDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// clipFormatEntry maps between temp file names, MIME types, and macOS UTIs.
type clipFormatEntry struct {
	fileName  string
	mimeType  string
	darwinUTI string
}

var clipFormatTable = []clipFormatEntry{
	{"text_plain", "text/plain", "public.utf8-plain-text"},
	{"text_html", "text/html", "public.html"},
	{"text_rtf", "text/rtf", "public.rtf"},
	{"image_png", "image/png", "public.png"},
	{"image_tiff", "image/tiff", "public.tiff"},
}

// readClipboardFormats reads all known non-file formats from the OS clipboard.
// The function recovers from panics (e.g. Go runtime crashes during subprocess
// creation on Windows) so that a transient OS failure cannot kill the process.
func readClipboardFormats() (formats []*pb.ClipFormat) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered panic in readClipboardFormats", "panic", r)
			formats = nil
		}
	}()

	dir := clipTmpDir()
	clearDir(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	readClipboardPlatform(ctx, dir)

	return loadFormatsFromDir(dir)
}

func loadFormatsFromDir(dir string) []*pb.ClipFormat {
	var formats []*pb.ClipFormat
	var totalSize int
	for _, entry := range clipFormatTable {
		data, err := os.ReadFile(filepath.Join(dir, entry.fileName)) //nolint:gosec // G304: dir is the node's private filesDir; entry.fileName is a fixed constant
		if err != nil || len(data) == 0 {
			continue
		}
		if len(data) > maxSyncFileSize {
			slog.Warn("Skipping clipboard format: exceeds per-file size limit",
				"format", entry.mimeType, "size_mb", len(data)>>20, "limit_mb", maxSyncFileSize>>20)
			continue
		}
		if totalSize+len(data) > maxClipboardPayload {
			slog.Warn("Skipping remaining clipboard formats: total payload size limit reached",
				"format", entry.mimeType, "total_mb", totalSize>>20, "limit_mb", maxClipboardPayload>>20)
			break
		}
		totalSize += len(data)
		formats = append(formats, &pb.ClipFormat{MimeType: entry.mimeType, Data: data})
	}

	// Some platforms (e.g. Windows) store clipboard formats in a wrapper that
	// requires additional extraction; loadPlatformFormats handles this.
	formats = append(formats, loadPlatformFormats(dir)...)
	return formats
}

// clipWriteFormats writes clipboard formats to the OS. Tests replace this to
// avoid touching the real clipboard.
var clipWriteFormats = writeClipboardFormats

// clipWriteFiles writes file paths to the OS clipboard. Tests replace this to
// avoid touching the real clipboard.
var clipWriteFiles = writeFiles

// writeClipboardFormats writes all formats to the OS clipboard at once.
// Recovers from panics to prevent subprocess crashes from killing the process.
func writeClipboardFormats(formats []*pb.ClipFormat) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered panic in writeClipboardFormats", "panic", r)
		}
	}()

	if len(formats) == 0 {
		return
	}

	dir := clipTmpDir()
	clearDir(dir)

	// Build a lookup and write temp files.
	fmtMap := make(map[string][]byte) // mimeType → data
	for _, f := range formats {
		fmtMap[f.GetMimeType()] = f.GetData()
	}
	for _, entry := range clipFormatTable {
		if data, ok := fmtMap[entry.mimeType]; ok {
			_ = os.WriteFile(filepath.Join(dir, entry.fileName), data, 0600)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	writeClipboardPlatform(ctx, dir, formats, fmtMap)
}

func readFiles() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return readFilesPlatform(ctx)
}

func writeFiles(paths []string) {
	if len(paths) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	writeFilesPlatform(ctx, paths)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// extractCFHTMLFragment extracts the HTML fragment from Windows CF_HTML format.
func extractCFHTMLFragment(cfhtml string) string {
	const startMarker = "<!--StartFragment-->"
	const endMarker = "<!--EndFragment-->"
	start := strings.Index(cfhtml, startMarker)
	end := strings.Index(cfhtml, endMarker)
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(cfhtml[start+len(startMarker) : end])
}

// buildCFHTML wraps an HTML fragment in Windows CF_HTML clipboard format.
func buildCFHTML(fragment string) string {
	prefix := "<html><body>\r\n<!--StartFragment-->"
	suffix := "<!--EndFragment-->\r\n</body></html>"
	hdr := fmt.Sprintf("Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\nStartFragment:%010d\r\nEndFragment:%010d\r\n", 0, 0, 0, 0)
	startHTML := len(hdr)
	startFrag := startHTML + len(prefix)
	endFrag := startFrag + len(fragment)
	endHTML := endFrag + len(suffix)
	hdr = fmt.Sprintf("Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\nStartFragment:%010d\r\nEndFragment:%010d\r\n",
		startHTML, endHTML, startFrag, endFrag)
	return hdr + prefix + fragment + suffix
}

// Helper to parse newline-separated paths
func parsePathList(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Helper to parse text/uri-list
func parseURIList(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "file://") {
			out = append(out, strings.TrimPrefix(line, "file://"))
		}
	}
	return out
}

// ─── UDP Discovery & Peer Management ─────────────────────────────────────────

func (n *Node) runUDPServer(ctx context.Context, magicHeader string, port int) {
	// 1. Explicit IPv4 binding to match the WriteToUDP implementation
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		slog.Error("Clipsync UDP listen failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Unblock the blocking ReadFromUDP when the context is cancelled.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	// 2. Increase buffer slightly to prevent edge-case payload truncation
	buf := make([]byte, 2048)

	for {
		// 3. Removed the 30s deadline. A blocking read uses 0% CPU and is
		// natively managed by the OS thread scheduler.
		nBytes, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 4. Prevent CPU spinning if the Windows network stack temporarily
			// invalidates the socket (e.g., during WiFi roaming or VPN toggles).
			backoff := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				backoff.Stop()
				return
			case <-backoff.C:
			}
			continue
		}

		var msg pb.Beacon
		if err := proto.Unmarshal(buf[:nBytes], &msg); err != nil || msg.GetMagic() != magicHeader || msg.GetId() == n.id {
			continue
		}
		if !n.groupOverlaps(msg.GetGroup()) {
			continue
		}

		// 5. Zero-allocation IP extraction (bypasses SplitHostPort overhead)
		if remoteAddr == nil || remoteAddr.IP == nil {
			continue
		}
		peerAddr := fmt.Sprintf("%s:%d", remoteAddr.IP.String(), msg.GetPort())

		isNew, needsPull := n.registerPeer(peerAddr, msg.GetHash(), "udp")
		if isNew {
			// Send a unicast reply so the sender also discovers us.
			// This handles asymmetric networks where broadcast only works
			// in one direction (e.g., Windows → macOS but not macOS → Windows).
			n.stateMu.Lock()
			h := n.lastHash
			n.stateMu.Unlock()
			reply, err := proto.Marshal(&pb.Beacon{
				Magic: magicHeader,
				Id:    n.id,
				Port:  int32(n.port),
				Hash:  h,
				Group: n.primaryGroup(),
			})
			if err == nil {
				// Reply to the peer's UDP server port (same as ours), not the
				// ephemeral source port of the beacon sender.
				replyAddr := &net.UDPAddr{IP: remoteAddr.IP, Port: port}
				_, _ = conn.WriteToUDP(reply, replyAddr)
			}
			// Also register ourselves via HTTP as a firewall-safe fallback.
			// UDP unicast replies may be blocked by firewalls; HTTP connections
			// are outgoing from our side, so they pass through.
			go n.registerPeerHTTP(peerAddr) //nolint:gosec // G118: intentional background op; must outlive UDP packet handler
		}
		if needsPull {
			slog.Debug("Peer advertised new clipboard hash, triggering pull", "peer", peerAddr, "hash", msg.Hash)
			go n.pullHTTP(peerAddr)
		}
	}
}
func (n *Node) runUDPBeacon(ctx context.Context, magicHeader string, port int) {
	// 1. Explicitly use UDPConn to ensure strict memory alignment for the OS kernel
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		slog.Error("Clipsync UDP Beacon binding failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Cache broadcast addresses; network interfaces rarely change at runtime.
	var cachedBcastAddrs []*net.UDPAddr
	var lastIfaceRefresh time.Time

	refreshBroadcastAddrs := func() []*net.UDPAddr {
		if time.Since(lastIfaceRefresh) < 30*time.Second {
			return cachedBcastAddrs
		}
		lastIfaceRefresh = time.Now()

		// Global broadcast fallback
		addrs := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}

		// Some platforms (e.g. Windows) have unstable per-interface UDP broadcasts;
		// skipPerIfaceBroadcast gates the per-interface enumeration.
		if skipPerIfaceBroadcast() {
			cachedBcastAddrs = addrs
			return addrs
		}

		ifaces, err := net.Interfaces()
		if err != nil {
			return addrs
		}

		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 {
				continue
			}
			ifAddrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range ifAddrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip4 := ipnet.IP.To4()
				if ip4 == nil {
					continue
				}

				// 3. Safe IPv4 mask extraction to prevent bitwise corruption
				mask := ipnet.Mask
				if len(mask) == net.IPv6len {
					mask = mask[12:]
				}
				if len(mask) != net.IPv4len {
					continue
				}

				bcast := net.IPv4(
					ip4[0]|^mask[0],
					ip4[1]|^mask[1],
					ip4[2]|^mask[2],
					ip4[3]|^mask[3],
				)
				addrs = append(addrs, &net.UDPAddr{IP: bcast, Port: port})
			}
		}
		cachedBcastAddrs = addrs
		return addrs
	}

	// Pre-allocate beacon message to avoid allocation each tick.
	beacon := &pb.Beacon{Magic: magicHeader, Id: n.id, Port: int32(n.port), Group: n.primaryGroup()}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.notifyCh:
			slog.Debug("Broadcasting instant UDP beacon for new clipboard content")
		}

		n.stateMu.Lock()
		beacon.Hash = n.lastHash
		n.stateMu.Unlock()

		msg, err := proto.Marshal(beacon)
		if err != nil {
			continue // Prevent nil payloads from reaching WriteTo
		}

		for _, baddr := range refreshBroadcastAddrs() {
			_, _ = conn.WriteToUDP(msg, baddr)
		}
	}
}

func (n *Node) cleanupPeers(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		n.peersMu.Lock()
		for addr, lastSeen := range n.peers {
			if now.Sub(lastSeen) > 15*time.Second {
				delete(n.peers, addr)
				delete(n.peerHashes, addr)
				state.Global.Delete("clipsync-peer", n.config.Bind+"|"+addr)
			}
		}
		n.peersMu.Unlock()
	}
}

// registerPeer adds or refreshes a peer in the discovery map.
// Returns isNew=true for first-time peers, needsPull=true when the
// peer's clipboard hash differs from ours.
func (n *Node) registerPeer(peerAddr, hash, source string) (isNew, needsPull bool) {
	n.peersMu.Lock()
	_, exists := n.peers[peerAddr]
	if !exists && len(n.peers) >= maxPeers {
		n.peersMu.Unlock()
		return false, false
	}
	isNew = !exists
	n.peers[peerAddr] = time.Now()

	if isNew {
		state.Global.Update("clipsync-peer", n.config.Bind+"|"+peerAddr, state.Connected, "discovered")
	}

	if hash != "" {
		if lastH, ok := n.peerHashes[peerAddr]; !ok || lastH != hash {
			n.peerHashes[peerAddr] = hash
			n.stateMu.Lock()
			if n.lastHash != hash {
				needsPull = true
			}
			n.stateMu.Unlock()
		}
	}
	n.peersMu.Unlock()

	if isNew {
		slog.Info("Discovered new peer", "peer", cleanLogStr(peerAddr), "source", cleanLogStr(source)) //nolint:gosec // G706: sanitized via cleanLogStr
	}
	return
}

// registerPeerHTTP sends an HTTP POST to the peer's /discover endpoint
// so the peer registers us as a discovered node. This is a firewall-safe
// fallback for networks where UDP unicast replies are blocked.
func (n *Node) registerPeerHTTP(peerAddr string) {
	n.stateMu.Lock()
	h := n.lastHash
	n.stateMu.Unlock()

	body, err := proto.Marshal(&pb.DiscoverRequest{
		Id:    n.id,
		Port:  int32(n.port),
		Hash:  h,
		Group: n.primaryGroup(),
	})
	if err != nil {
		return
	}

	reqCtx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", fmt.Sprintf("http://%s/discover", peerAddr), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		slog.Debug("HTTP peer registration failed", "peer", peerAddr, "error", err)
		return
	}
	_ = resp.Body.Close()
}

// refreshHTTPRegistration periodically re-registers this node with all
// known peers via HTTP. This keeps us alive in peers that cannot receive
// our UDP beacons (e.g., firewalled hosts).
func (n *Node) refreshHTTPRegistration(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.peersMu.RLock()
		addrs := make([]string, 0, len(n.peers))
		for addr := range n.peers {
			addrs = append(addrs, addr)
		}
		n.peersMu.RUnlock()

		for _, addr := range addrs {
			go n.registerPeerHTTP(addr) //nolint:gosec // G118: intentional background op; must outlive UDP beacon tick
		}
	}
}
