package clipsync

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	config "github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
)

const (
	Port         = 7755
	MagicHeader  = "CLPSYNC2"
	PollInterval = 500 * time.Millisecond

	// maxSyncFileSize is the per-file size limit for clipboard sync.
	// Files larger than this are skipped to avoid OOM and transfer timeouts.
	maxSyncFileSize = 50 * 1024 * 1024 // 50 MB

	// maxRequestBodySize caps the /sync endpoint body.
	// Allows up to ~20 files at maxSyncFileSize with base64 overhead (~33%).
	maxRequestBodySize = maxSyncFileSize * 20 * 4 / 3
)

type FileRef struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	Data     []byte `json:"data,omitempty"` // file content embedded in payload; avoids a pull-back when sender blocks incoming connections
}

type Payload struct {
	Type     string    `json:"type"` // "text", "image", "file"
	MimeType string    `json:"mime_type,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Files    []FileRef `json:"files,omitempty"`
}

type Node struct {
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
	lastPayload     []byte
	notifyCh        chan struct{}

	currentFilesMu sync.Mutex
	currentFiles   []string // absolute paths of files in filesDir tied to current clipboard content
}

// Start initializes the mesh node based on the provided configuration.
func Start(cfg config.ClipsyncCfg) (*Node, error) {
	// Defaults
	port := Port
	magicHeader := MagicHeader
	pollInterval := PollInterval

	home, _ := os.UserHomeDir()
	filesDir := filepath.Join(home, ".mesh", "clipsync")
	os.MkdirAll(filesDir, 0755)

	_, portStr, _ := net.SplitHostPort(cfg.Bind)

	fmt.Sscanf(portStr, "%d", &port)

	n := &Node{
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

	go n.runHTTPServer()

	if cfg.LANDiscovery {
		go n.runUDPServer(magicHeader, port)
		go n.runUDPBeacon(magicHeader, port)
		go n.cleanupPeers()
	}

	state.Global.Update("clipsync", cfg.Bind, state.Listening, "")
	for _, addr := range cfg.StaticPeers {
		state.Global.Update("clipsync-peer", cfg.Bind+"|"+addr, state.Connected, "static")
	}

	go n.pollClipboard(pollInterval)
	return n, nil
}

// ─── Network & ACL Logic ─────────────────────────────────────────────────────

func (n *Node) canSendTo(addr string, isUDP bool) bool {
	if contains(n.config.AllowSendTo, "none") {
		return false
	}
	if contains(n.config.AllowSendTo, "all") {
		return true
	}
	if isUDP && contains(n.config.AllowSendTo, "udp") {
		return true
	}
	host, _, _ := net.SplitHostPort(addr)
	return contains(n.config.AllowSendTo, host) || contains(n.config.AllowSendTo, addr)
}

func (n *Node) canReceiveFrom(addr string) bool {
	if contains(n.config.AllowReceive, "none") {
		return false
	}
	if contains(n.config.AllowReceive, "all") {
		return true
	}
	host, _, _ := net.SplitHostPort(addr)
	return contains(n.config.AllowReceive, host) || contains(n.config.AllowReceive, addr)
}

func (n *Node) Broadcast(payload Payload) {
	data, _ := json.Marshal(payload)

	n.stateMu.Lock()
	n.lastPayload = data
	n.stateMu.Unlock()

	select {
	case n.notifyCh <- struct{}{}:
	default:
	}

	// Send to Dynamic UDP Peers
	n.peersMu.RLock()
	for addr := range n.peers {
		if n.canSendTo(addr, true) {
			slog.Debug("Pushing payload via HTTP POST to dynamic peer", "peer", addr)
			go n.postHTTP(addr, data)
		}
	}
	n.peersMu.RUnlock()

	// Send to Static Peers (SSH Tunnels or explicit IP)
	for _, addr := range n.config.StaticPeers {
		if n.canSendTo(addr, false) {
			slog.Debug("Pushing payload via HTTP POST to static peer", "peer", addr)
			go n.postHTTP(addr, data)
		}
	}
}

func (n *Node) postHTTP(addr string, data []byte) {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/sync", addr), bytes.NewBuffer(data))
	resp, err := n.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ─── HTTP Server & File Handling ─────────────────────────────────────────────

func (n *Node) processPayload(p Payload, peerHostPort string) {
	switch p.Type {
	case "text":
		n.clearCurrentFiles()
		n.setWrittenHash(hashBytes(p.Data))
		writeText(string(p.Data))
	case "image":
		n.clearCurrentFiles()
		n.setWrittenHash(hashBytes(p.Data))
		writeImage(p.Data, p.MimeType)
	case "file":
		var writtenPaths []string
		for _, f := range p.Files {
			destPath := filepath.Join(n.filesDir, f.FileName)
			if len(f.Data) > 0 {
				if err := os.WriteFile(destPath, f.Data, 0644); err != nil {
					slog.Warn("Failed to save clipboard file", "file", f.FileName, "error", err)
					continue
				}
			} else if err := n.downloadFile(f.FileID, f.FileName, peerHostPort); err != nil {
				slog.Warn("Failed to download clipboard file", "file", f.FileName, "peer", peerHostPort, "error", err)
				continue
			}
			writtenPaths = append(writtenPaths, destPath)
		}
		if len(writtenPaths) > 0 {
			n.setCurrentFiles(writtenPaths) // replaces old files, tracks new ones
			n.setWrittenHash(hashFilePaths(writtenPaths))
			writeFiles(writtenPaths)
		}
	}
}

func (n *Node) pullHTTP(peerAddr string) {
	slog.Debug("Making outbound HTTP GET pull request", "peer", peerAddr)
	resp, err := n.httpClient.Get(fmt.Sprintf("http://%s/clip", peerAddr))
	if err != nil || resp.StatusCode != 200 {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		slog.Debug("Failed to pull from peer", "peer", peerAddr, "error", err, "status", status)
		return
	}
	defer resp.Body.Close()

	var p Payload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		slog.Debug("Failed to decode pulled payload", "peer", peerAddr, "error", err)
		return
	}

	slog.Info("Successfully pulled and ingested payload", "type", p.Type, "peer", peerAddr)
	n.processPayload(p, peerAddr)
}

func (n *Node) runHTTPServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			if err.Error() == "http: request body too large" {
				slog.Warn("Rejected oversized sync payload", "from", r.RemoteAddr)
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			}
			return
		}

		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peerHostPort := net.JoinHostPort(host, fmt.Sprintf("%d", n.port))

		slog.Info("Received pushed payload via HTTP POST", "type", p.Type, "from", r.RemoteAddr)
		n.processPayload(p, peerHostPort)
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

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	// Serve files for peers to download
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(n.filesDir))))

	srv := &http.Server{Addr: n.config.Bind, Handler: mux}
	slog.Info("Clipsync HTTP listening", "bind", n.config.Bind)
	srv.ListenAndServe()
}

func (n *Node) downloadFile(fileID, fileName, peerAddr string) error {
	resp, err := n.httpClient.Get(fmt.Sprintf("http://%s/files/%s", peerAddr, fileID))
	if err != nil || resp.StatusCode != 200 {
		return err
	}
	defer resp.Body.Close()

	dst, err := os.Create(filepath.Join(n.filesDir, fileName))
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, resp.Body)
	return err
}

// ─── OS Clipboard Monitor ────────────────────────────────────────────────────

func (n *Node) pollClipboard(pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	for range ticker.C {
		// 1. Check Files
		paths := readFiles()
		if len(paths) > 0 {
			h := hashFilePaths(paths)
			if n.checkHash(h) {
				slog.Debug("Local clipboard files changed", "hash", h, "count", len(paths))
				n.handleFileBroadcast(paths)
			}
			continue
		}

		// No files on clipboard: any previously tracked files are now orphaned.
		n.clearCurrentFiles()

		// 2. Check Text
		text := readText()
		if text != "" {
			h := hashBytes([]byte(text))
			if n.checkHash(h) {
				slog.Debug("Local clipboard text changed", "hash", h)
				n.Broadcast(Payload{Type: "text", Data: []byte(text)})
			}
			continue
		}

		// 3. Check Image
		img, ext := readImage()
		if len(img) > 0 {
			h := hashBytes(img)
			if n.checkHash(h) {
				slog.Debug("Local clipboard image changed", "hash", h, "ext", ext)
				n.Broadcast(Payload{Type: "image", MimeType: ext, Data: img})
			}
		}
	}
}

func (n *Node) handleFileBroadcast(paths []string) {
	if len(paths) == 0 {
		return
	}
	n.clearCurrentFiles() // delete files cached from the previous clipboard content
	var files []FileRef
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
		input, err := os.ReadFile(src)
		if err != nil {
			slog.Warn("Failed to read clipboard file", "path", src, "error", err)
			continue
		}
		if err := os.WriteFile(dest, input, 0644); err != nil {
			slog.Warn("Failed to store clipboard file", "path", dest, "error", err)
			continue
		}
		files = append(files, FileRef{FileID: fileID, FileName: fileName, Data: input})
		storedPaths = append(storedPaths, dest)
	}
	if len(files) > 0 {
		n.setCurrentFiles(storedPaths) // track so they're deleted when clipboard changes
		n.Broadcast(Payload{Type: "file", Files: files})
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
		os.Remove(filepath.Join(n.filesDir, e.Name()))
	}
	if len(entries) > 0 {
		slog.Debug("Purged leftover clipsync files from previous session", "count", len(entries))
	}
}

// setWrittenHash records the hash of content written from a peer so that
// the next poll cycle does not re-broadcast it as a local change.
func (n *Node) setWrittenHash(h string) {
	n.stateMu.Lock()
	n.lastHash = h
	n.lastWrittenHash = h
	n.stateMu.Unlock()
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashBytes(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

// hashFilePaths returns a stable, order-independent hash of a set of file paths.
func hashFilePaths(paths []string) string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)
	h := md5.Sum([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(h[:])
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

func readText() string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbpaste")
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Get-Clipboard")
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard", "-o")
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			cmd = exec.Command("wl-paste", "-t", "text/plain")
		} else {
			return ""
		}
	default:
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func writeText(text string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip")
	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard", "-i")
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			return
		}
	default:
		return
	}
	cmd.Stdin = strings.NewReader(text)
	cmd.Run()
}

func readImage() ([]byte, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(os.TempDir(), fmt.Sprintf("clipsync_%d", os.Getpid()))
		script := fmt.Sprintf(`
		try
			set imgData to (the clipboard as «class PNGf»)
			set ext to "png"
		on error
			try
				set imgData to (the clipboard as «class TIFF»)
				set ext to "tiff"
			on error
				return ""
			end try
		end try
		set fp to "%s." & ext
		set f to open for access (POSIX file fp) with write permission
		set eof f to 0
		write imgData to f
		close access f
		return ext`, base)
		out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
		if err != nil {
			return nil, ""
		}
		ext := strings.TrimSpace(string(out))
		if ext == "" {
			return nil, ""
		}
		tmp := base + "." + ext
		defer os.Remove(tmp)
		data, _ := os.ReadFile(tmp)
		return data, ext

	case "windows":
		script := `Add-Type -AssemblyName System.Windows.Forms; $img = [Windows.Forms.Clipboard]::GetImage(); if ($img) { $ms = New-Object System.IO.MemoryStream; $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); [Convert]::ToBase64String($ms.ToArray()) }`
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
		if err != nil || len(out) == 0 {
			return nil, ""
		}
		data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(out)))
		if err != nil {
			return nil, ""
		}
		return data, "png"

	case "linux":
		var cmd *exec.Cmd
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			cmd = exec.CommandContext(ctx, "wl-paste", "--type", "image/png")
		} else {
			return nil, ""
		}
		data, err := cmd.Output()
		if err != nil || len(data) == 0 {
			return nil, ""
		}
		return data, "png"
	}
	return nil, ""
}

func writeImage(data []byte, ext string) {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("clp_write_%d.%s", os.Getpid(), ext))
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return
	}
	defer os.Remove(tmpFile)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		cls := "PNGf"
		if ext == "tiff" || ext == "tif" {
			cls = "TIFF"
		}
		script := fmt.Sprintf(`set the clipboard to (read (POSIX file "%s") as «class %s»)`, tmpFile, cls)
		exec.CommandContext(ctx, "osascript", "-e", script).Run()

	case "windows":
		script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $img = [System.Drawing.Image]::FromFile('%s'); [Windows.Forms.Clipboard]::SetImage($img)`, tmpFile)
		exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Run()

	case "linux":
		if _, err := exec.LookPath("xclip"); err == nil {
			f, err := os.Open(tmpFile)
			if err != nil {
				return
			}
			cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-i")
			cmd.Stdin = f
			cmd.Run()
			f.Close()
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			f, err := os.Open(tmpFile)
			if err != nil {
				return
			}
			cmd := exec.CommandContext(ctx, "wl-copy", "--type", "image/png")
			cmd.Stdin = f
			cmd.Run()
			f.Close()
		}
	}
}

func readFiles() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		script := `
		use framework "AppKit"
		set pb to current application's NSPasteboard's generalPasteboard()
		set fileType to current application's NSPasteboardTypeFileURL
		if (pb's availableTypeFromArray:{fileType}) is missing value then return ""
		set urls to pb's readObjectsForClasses:{current application's NSURL} options:(missing value)
		if urls is missing value then return ""
		set cnt to (urls's |count|()) as integer
		if cnt = 0 then return ""
		set pathList to ""
		repeat with i from 1 to cnt
			set u to (urls's objectAtIndex:(i - 1))
			if (u's isFileURL()) as boolean then
				set p to (u's |path|()) as text
				set pathList to pathList & p & linefeed
			end if
		end repeat
		return pathList`
		out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
		if err != nil {
			return nil
		}
		return parsePathList(string(out))

	case "windows":
		script := `(Get-Clipboard -Format FileDropList).FullName`
		out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
		if err != nil {
			return nil
		}
		return parsePathList(string(out))

	case "linux":
		var out []byte
		if _, err := exec.LookPath("xclip"); err == nil {
			out, _ = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o").Output()
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			out, _ = exec.CommandContext(ctx, "wl-paste", "--type", "text/uri-list").Output()
		}
		if len(out) == 0 {
			return nil
		}
		return parseURIList(string(out))
	}
	return nil
}

func writeFiles(paths []string) {
	if len(paths) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		var sb strings.Builder
		sb.WriteString("use framework \"AppKit\"\n")
		sb.WriteString("set pb to current application's NSPasteboard's generalPasteboard()\npb's clearContents()\n")
		sb.WriteString("set urls to current application's NSMutableArray's new()\n")
		for _, p := range paths {
			esc := strings.ReplaceAll(strings.ReplaceAll(p, "\\", "\\\\"), "\"", "\\\"")
			sb.WriteString(fmt.Sprintf("urls's addObject:(current application's NSURL's fileURLWithPath:\"%s\")\n", esc))
		}
		sb.WriteString("pb's writeObjects:urls\n")
		exec.CommandContext(ctx, "osascript", "-e", sb.String()).Run()

	case "windows":
		var sb strings.Builder
		sb.WriteString("Set-Clipboard -Path ")
		for i, p := range paths {
			sb.WriteString(fmt.Sprintf("'%s'", p))
			if i < len(paths)-1 {
				sb.WriteString(",")
			}
		}
		exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", sb.String()).Run()

	case "linux":
		var sb strings.Builder
		for _, p := range paths {
			sb.WriteString(fmt.Sprintf("file://%s\r\n", p))
		}
		uriList := sb.String()

		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i")
			cmd.Stdin = strings.NewReader(uriList)
			cmd.Run()
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd := exec.CommandContext(ctx, "wl-copy", "--type", "text/uri-list")
			cmd.Stdin = strings.NewReader(uriList)
			cmd.Run()
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

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
			path := strings.TrimPrefix(line, "file://")
			out = append(out, filepath.FromSlash(path))
		}
	}
	return out
}

// ─── UDP Discovery & Peer Management ─────────────────────────────────────────

func (n *Node) runUDPServer(magicHeader string, port int) {
	addr := &net.UDPAddr{Port: port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		slog.Error("Clipsync UDP listen failed", "error", err)
		return
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	for {
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		nBytes, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg struct {
			Magic string `json:"m"`
			ID    string `json:"id"`
			Port  int    `json:"port"`
			Hash  string `json:"h"`
		}

		if err := json.Unmarshal(buf[:nBytes], &msg); err != nil || msg.Magic != magicHeader || msg.ID == n.id {
			continue
		}

		host, _, _ := net.SplitHostPort(remoteAddr.String())
		peerAddr := fmt.Sprintf("%s:%d", host, msg.Port)

		n.peersMu.Lock()
		if _, exists := n.peers[peerAddr]; !exists {
			slog.Info("Discovered new peer via LAN UDP broadcast", "peer", peerAddr, "ID", msg.ID)
			state.Global.Update("clipsync-peer", n.config.Bind+"|"+peerAddr, state.Connected, "discovered")
		}
		n.peers[peerAddr] = time.Now()

		needsPull := false
		if msg.Hash != "" {
			if lastH, ok := n.peerHashes[peerAddr]; !ok || lastH != msg.Hash {
				n.peerHashes[peerAddr] = msg.Hash
				n.stateMu.Lock()
				if n.lastHash != msg.Hash {
					needsPull = true
				}
				n.stateMu.Unlock()
			}
		}
		n.peersMu.Unlock()

		if needsPull {
			slog.Debug("Peer advertised new clipboard hash, triggering pull", "peer", peerAddr, "hash", msg.Hash)
			go n.pullHTTP(peerAddr)
		}
	}
}

func (n *Node) runUDPBeacon(magicHeader string, port int) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		slog.Error("Clipsync UDP Beacon binding failed", "error", err)
		return
	}
	defer conn.Close()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-n.notifyCh:
			slog.Debug("Broadcasting instant UDP beacon for new clipboard content")
		}

		n.stateMu.Lock()
		h := n.lastHash
		n.stateMu.Unlock()

		msg, _ := json.Marshal(map[string]interface{}{"m": magicHeader, "id": n.id, "port": n.port, "h": h})

		globalBcast := &net.UDPAddr{IP: net.IPv4bcast, Port: port}
		conn.WriteTo(msg, globalBcast)

		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					ip := ipnet.IP.To4()
					mask := ipnet.Mask
					bcast := net.IPv4(ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3])
					conn.WriteTo(msg, &net.UDPAddr{IP: bcast, Port: port})
				}
			}
		}
	}
}

func (n *Node) cleanupPeers() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		n.peersMu.Lock()
		for addr, lastSeen := range n.peers {
			if now.Sub(lastSeen) > 15*time.Second {
				delete(n.peers, addr)
				state.Global.Delete("clipsync-peer", n.config.Bind+"|"+addr)
			}
		}
		n.peersMu.Unlock()
	}
}
