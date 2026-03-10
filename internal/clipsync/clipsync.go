package clipsync

import (
	"bytes"
	"context"
	"crypto/md5"
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
)

type Payload struct {
	Type     string `json:"type"` // "text", "image", "file"
	MimeType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
	FileName string `json:"file_name,omitempty"`
	FileID   string `json:"file_id,omitempty"`
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

	stateMu     sync.Mutex
	lastHash    string
	lastPayload []byte
	isLocked    bool
	notifyCh    chan struct{}
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
		httpClient: &http.Client{Timeout: 5 * time.Second},
		filesDir:   filesDir,
		notifyCh:   make(chan struct{}, 1),
	}

	go n.runHTTPServer()

	if cfg.LANDiscovery {
		go n.runUDPServer(magicHeader, port)
		go n.runUDPBeacon(magicHeader, port)
		go n.cleanupPeers()
	}

	state.Global.Update("clipsync", cfg.Bind, state.Listening, "discovery: "+fmt.Sprintf("%v", cfg.LANDiscovery))

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
	n.stateMu.Lock()
	n.isLocked = true
	n.stateMu.Unlock()

	defer func() {
		n.stateMu.Lock()
		n.isLocked = false
		n.stateMu.Unlock()
	}()

	switch p.Type {
	case "text":
		n.setHash(hashBytes(p.Data))
		writeText(string(p.Data))
	case "image":
		n.setHash(hashBytes(p.Data))
		writeImage(p.Data, p.MimeType)
	case "file":
		if err := n.downloadFile(p.FileID, p.FileName, peerHostPort); err == nil {
			path := filepath.Join(n.filesDir, p.FileName)
			n.setHash(hashBytes([]byte(path)))
			writeFiles([]string{path})
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

		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
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
		n.stateMu.Lock()
		if n.isLocked {
			n.stateMu.Unlock()
			continue
		}
		n.stateMu.Unlock()

		// 1. Check Files
		paths := readFiles()
		if len(paths) > 0 {
			h := hashBytes([]byte(strings.Join(paths, "")))
			if n.checkHash(h) {
				slog.Debug("Local clipboard files changed", "hash", h, "count", len(paths))
				n.handleFileBroadcast(paths)
			}
			continue
		}

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
	// For simplicity in a single file, handle the first copied file.
	if len(paths) == 0 {
		return
	}
	src := paths[0]
	fileName := filepath.Base(src)
	fileID := generateID() + filepath.Ext(fileName)

	dest := filepath.Join(n.filesDir, fileID)

	// Copy file to local static hosting dir
	input, _ := os.ReadFile(src)
	os.WriteFile(dest, input, 0644)

	n.Broadcast(Payload{
		Type:     "file",
		FileName: fileName,
		FileID:   fileID,
	})
}

// ─── Helpers & OS Bindings (Text/Files only shown for brevity) ───────────────

func (n *Node) checkHash(h string) bool {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	if n.lastHash != h {
		n.lastHash = h
		return true
	}
	return false
}

func (n *Node) setHash(h string) {
	n.stateMu.Lock()
	n.lastHash = h
	n.stateMu.Unlock()
}

func generateID() string {
	b := make([]byte, 4)
	md5.New().Write([]byte(time.Now().String()))
	return hex.EncodeToString(b)
}

func hashBytes(b []byte) string {
	h := md5.Sum(b)
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
			f, _ := os.Open(tmpFile)
			cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-i")
			cmd.Stdin = f
			cmd.Run()
			f.Close()
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			f, _ := os.Open(tmpFile)
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
			}
		}
		n.peersMu.Unlock()
	}
}
