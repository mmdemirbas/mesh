package clipsync

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
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
	"sync/atomic"
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

// ClipFormat holds a single clipboard representation keyed by MIME type.
type ClipFormat struct {
	MimeType string `json:"mime_type"` // "text/plain", "text/html", "text/rtf", "image/png", "image/tiff"
	Data     []byte `json:"data"`
}

type Payload struct {
	Formats []ClipFormat `json:"formats,omitempty"` // multi-format clipboard content
	Files   []FileRef    `json:"files,omitempty"`
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
	originAddr      string // peer address that last pushed content to us; cleared on local broadcast
	lastPayload     []byte
	notifyCh        chan struct{}

	currentFilesMu sync.Mutex
	currentFiles   []string // absolute paths of files in filesDir tied to current clipboard content

	clipMu sync.Mutex // serializes all clipboard read/write process spawning
}

// Start initializes the mesh node based on the provided configuration.
func Start(cfg config.ClipsyncCfg) (*Node, error) {
	// Defaults
	port := Port
	magicHeader := MagicHeader
	pollInterval := PollInterval

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("clipsync: cannot determine home directory: %w", err)
	}
	filesDir := filepath.Join(home, ".mesh", "clipsync")
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return nil, fmt.Errorf("clipsync: cannot create files directory: %w", err)
	}

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
	return containsIP(n.config.AllowSendTo, host) || contains(n.config.AllowSendTo, addr)
}

func (n *Node) canReceiveFrom(addr string) bool {
	if contains(n.config.AllowReceive, "none") {
		return false
	}
	if contains(n.config.AllowReceive, "all") {
		return true
	}
	host, _, _ := net.SplitHostPort(addr)
	return containsIP(n.config.AllowReceive, host) || contains(n.config.AllowReceive, addr)
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

func (n *Node) Broadcast(payload Payload) {
	data, _ := json.Marshal(payload)

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
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s/sync", addr), bytes.NewBuffer(data))
	resp, err := n.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ─── HTTP Server & File Handling ─────────────────────────────────────────────

func (n *Node) processPayload(p Payload, peerHostPort string) {
	if len(p.Files) > 0 {
		var writtenPaths []string
		for _, f := range p.Files {
			// Sanitize filename to prevent path traversal (e.g., "../../etc/passwd")
			safeName := filepath.Base(f.FileName)
			if safeName == "." || safeName == ".." || safeName == "" {
				slog.Warn("Rejected clipboard file with unsafe name", "file", f.FileName)
				continue
			}
			destPath := filepath.Join(n.filesDir, safeName)
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
			n.clipMu.Lock()
			n.setCurrentFiles(writtenPaths)
			n.setWrittenHash(hashFilePaths(writtenPaths), peerHostPort)
			writeFiles(writtenPaths)
			n.clipMu.Unlock()
		}
	} else if len(p.Formats) > 0 {
		n.clipMu.Lock()
		n.clearCurrentFiles()
		n.setWrittenHash(hashFormats(p.Formats), peerHostPort)
		writeClipboardFormats(p.Formats)
		n.clipMu.Unlock()
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBodySize)).Decode(&p); err != nil {
		slog.Debug("Failed to decode pulled payload", "peer", peerAddr, "error", err)
		return
	}

	slog.Info("Successfully pulled and ingested payload", "formats", len(p.Formats), "files", len(p.Files), "peer", peerAddr)
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
			} else {
				http.Error(w, "bad request", http.StatusBadRequest)
			}
			return
		}

		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peerHostPort := net.JoinHostPort(host, fmt.Sprintf("%d", n.port))

		slog.Info("Received pushed payload via HTTP POST", "formats", len(p.Formats), "from", r.RemoteAddr)
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

	resp, err := n.httpClient.Get(fmt.Sprintf("http://%s/files/%s", peerAddr, safeID))
	if err != nil || resp.StatusCode != 200 {
		return err
	}
	defer resp.Body.Close()

	dst, err := os.Create(filepath.Join(n.filesDir, safeName))
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, io.LimitReader(resp.Body, maxSyncFileSize))
	return err
}

// ─── OS Clipboard Monitor ────────────────────────────────────────────────────

func (n *Node) pollClipboard(pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	var polling atomic.Bool
	for range ticker.C {
		// Skip this tick if the previous one is still running.
		if !polling.CompareAndSwap(false, true) {
			continue
		}

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
				n.Broadcast(Payload{Formats: formats})
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
		n.Broadcast(Payload{Files: files})
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
func (n *Node) setWrittenHash(h, origin string) {
	n.stateMu.Lock()
	n.lastHash = h
	n.lastWrittenHash = h
	n.originAddr = origin
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

// hashFormats returns a stable hash of a set of clipboard formats.
func hashFormats(formats []ClipFormat) string {
	sorted := make([]ClipFormat, len(formats))
	copy(sorted, formats)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MimeType < sorted[j].MimeType })
	h := md5.New()
	for _, f := range sorted {
		h.Write([]byte(f.MimeType))
		h.Write([]byte{0}) // separator
		h.Write(f.Data)
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
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = utf8Env()
	return cmd
}

// linuxClipTool caches the detected Linux clipboard tool at first use.
// The available tool won't change during process lifetime, so calling
// exec.LookPath on every 500ms poll tick is wasteful.
var linuxClipTool struct {
	once sync.Once
	name string // "xclip", "wl" (for wl-clipboard), or "" (none)
}

func detectLinuxClipTool() string {
	linuxClipTool.once.Do(func() {
		if _, err := exec.LookPath("xclip"); err == nil {
			linuxClipTool.name = "xclip"
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			linuxClipTool.name = "wl"
		}
	})
	return linuxClipTool.name
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
	os.MkdirAll(dir, 0700)
	return dir
})

func clearDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
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
func readClipboardFormats() []ClipFormat {
	dir := clipTmpDir()
	clearDir(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		readClipboardDarwin(ctx, dir)
	case "windows":
		readClipboardWindows(ctx, dir)
	case "linux":
		readClipboardLinux(ctx, dir)
	}

	return loadFormatsFromDir(dir)
}

func loadFormatsFromDir(dir string) []ClipFormat {
	var formats []ClipFormat
	for _, entry := range clipFormatTable {
		data, err := os.ReadFile(filepath.Join(dir, entry.fileName))
		if err != nil || len(data) == 0 {
			continue
		}
		if len(data) > maxSyncFileSize {
			continue
		}
		formats = append(formats, ClipFormat{MimeType: entry.mimeType, Data: data})
	}

	// Windows stores CF_HTML in a wrapper; extract the fragment.
	if runtime.GOOS == "windows" {
		cfdata, err := os.ReadFile(filepath.Join(dir, "text_html_cf"))
		if err == nil && len(cfdata) > 0 {
			if frag := extractCFHTMLFragment(string(cfdata)); frag != "" {
				formats = append(formats, ClipFormat{MimeType: "text/html", Data: []byte(frag)})
			}
		}
	}
	return formats
}

func readClipboardDarwin(ctx context.Context, dir string) {
	// Build the UTI→filename pairs for the AppleScript loop.
	var pairs []string
	for _, e := range clipFormatTable {
		pairs = append(pairs, fmt.Sprintf(`{"%s", "/%s"}`, e.darwinUTI, e.fileName))
	}
	script := fmt.Sprintf(`use framework "AppKit"
set pb to current application's NSPasteboard's generalPasteboard()
set tmpDir to "%s"
set typeMap to {%s}
repeat with pair in typeMap
	set utiType to item 1 of pair
	set fName to item 2 of pair
	if (pb's availableTypeFromArray:{utiType}) is not missing value then
		set d to (pb's dataForType:utiType)
		if d is not missing value and (d's |length|()) > 0 then
			d's writeToFile:(tmpDir & fName) atomically:true
		end if
	end if
end repeat`, dir, strings.Join(pairs, ", "))
	clipCmd(ctx, "osascript", "-e", script).Run()
}

func readClipboardWindows(ctx context.Context, dir string) {
	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$d = '%s'
$text = [System.Windows.Forms.Clipboard]::GetText([System.Windows.Forms.TextDataFormat]::UnicodeText)
if ($text) { [System.IO.File]::WriteAllBytes("$d\text_plain", [System.Text.Encoding]::UTF8.GetBytes($text)) }
if ([System.Windows.Forms.Clipboard]::ContainsData('HTML Format')) {
  $obj = [System.Windows.Forms.Clipboard]::GetData('HTML Format')
  if ($obj -is [System.IO.MemoryStream]) {
    $r = New-Object System.IO.StreamReader($obj, [System.Text.Encoding]::UTF8); $cf = $r.ReadToEnd()
    [System.IO.File]::WriteAllText("$d\text_html_cf", $cf, [System.Text.Encoding]::UTF8)
  } elseif ($obj -is [string]) {
    [System.IO.File]::WriteAllText("$d\text_html_cf", $obj, [System.Text.Encoding]::UTF8)
  }
}
if ([System.Windows.Forms.Clipboard]::ContainsData([System.Windows.Forms.DataFormats]::Rtf)) {
  $rtf = [System.Windows.Forms.Clipboard]::GetData([System.Windows.Forms.DataFormats]::Rtf)
  if ($rtf -is [string]) { [System.IO.File]::WriteAllBytes("$d\text_rtf", [System.Text.Encoding]::UTF8.GetBytes($rtf)) }
}
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img) {
  $ms = New-Object System.IO.MemoryStream; $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  [System.IO.File]::WriteAllBytes("$d\image_png", $ms.ToArray()); $ms.Dispose(); $img.Dispose()
}`, dir)
	clipCmd(ctx, "powershell", "-NoProfile", "-STA", "-Command", script).Run()
}

func readClipboardLinux(ctx context.Context, dir string) {
	tool := detectLinuxClipTool()
	if tool == "" {
		return
	}

	// Discover which MIME types are available on the clipboard.
	var targetsCmd *exec.Cmd
	switch tool {
	case "xclip":
		targetsCmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	case "wl":
		targetsCmd = clipCmd(ctx, "wl-paste", "--list-types")
	}
	targetsOut, err := targetsCmd.Output()
	if err != nil {
		return
	}
	available := string(targetsOut)

	type linuxTarget struct {
		target   string // X11/Wayland MIME target
		fileName string
	}
	known := []linuxTarget{
		{"UTF8_STRING", "text_plain"},
		{"text/html", "text_html"},
		{"text/rtf", "text_rtf"},
		{"image/png", "image_png"},
	}
	// wl-paste uses standard MIME types instead of X11 atoms.
	if tool == "wl" {
		known[0] = linuxTarget{"text/plain", "text_plain"}
	}

	for _, kt := range known {
		if !strings.Contains(available, kt.target) {
			continue
		}
		var cmd *exec.Cmd
		switch tool {
		case "xclip":
			cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", kt.target, "-o")
		case "wl":
			cmd = clipCmd(ctx, "wl-paste", "-t", kt.target)
		}
		data, err := cmd.Output()
		if err == nil && len(data) > 0 {
			os.WriteFile(filepath.Join(dir, kt.fileName), data, 0600)
		}
	}
}

// writeClipboardFormats writes all formats to the OS clipboard at once.
func writeClipboardFormats(formats []ClipFormat) {
	if len(formats) == 0 {
		return
	}

	dir := clipTmpDir()
	clearDir(dir)

	// Build a lookup and write temp files.
	fmtMap := make(map[string][]byte) // mimeType → data
	for _, f := range formats {
		fmtMap[f.MimeType] = f.Data
	}
	for _, entry := range clipFormatTable {
		if data, ok := fmtMap[entry.mimeType]; ok {
			os.WriteFile(filepath.Join(dir, entry.fileName), data, 0600)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		writeClipboardDarwin(ctx, dir)
	case "windows":
		writeClipboardWindows(ctx, dir, fmtMap)
	case "linux":
		writeClipboardLinux(ctx, formats)
	}
}

func writeClipboardDarwin(ctx context.Context, dir string) {
	var pairs []string
	for _, e := range clipFormatTable {
		pairs = append(pairs, fmt.Sprintf(`{"%s", "/%s"}`, e.darwinUTI, e.fileName))
	}
	script := fmt.Sprintf(`use framework "AppKit"
set pb to current application's NSPasteboard's generalPasteboard()
pb's clearContents()
set tmpDir to "%s"
set fm to current application's NSFileManager's defaultManager()
set typeMap to {%s}
repeat with pair in typeMap
	set utiType to item 1 of pair
	set fName to item 2 of pair
	set fp to tmpDir & fName
	if (fm's fileExistsAtPath:fp) as boolean then
		set d to current application's NSData's dataWithContentsOfFile:fp
		if d is not missing value then
			pb's setData:d forType:utiType
		end if
	end if
end repeat`, dir, strings.Join(pairs, ", "))
	clipCmd(ctx, "osascript", "-e", script).Run()
}

func writeClipboardWindows(ctx context.Context, dir string, fmtMap map[string][]byte) {
	// HTML needs CF_HTML wrapping.
	if html, ok := fmtMap["text/html"]; ok {
		cfhtml := buildCFHTML(string(html))
		os.WriteFile(filepath.Join(dir, "text_html_cf"), []byte(cfhtml), 0600)
	}

	script := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$d = '%s'
$dataObj = New-Object System.Windows.Forms.DataObject
$fp = "$d\text_plain"
if (Test-Path $fp) { $dataObj.SetText([System.IO.File]::ReadAllText($fp, [System.Text.Encoding]::UTF8)) }
$fp = "$d\text_html_cf"
if (Test-Path $fp) {
  $bytes = [System.IO.File]::ReadAllBytes($fp)
  $ms = New-Object System.IO.MemoryStream(,$bytes)
  $dataObj.SetData('HTML Format', $ms)
}
$fp = "$d\text_rtf"
if (Test-Path $fp) { $dataObj.SetData([System.Windows.Forms.DataFormats]::Rtf, [System.IO.File]::ReadAllText($fp, [System.Text.Encoding]::UTF8)) }
$fp = "$d\image_png"
if (Test-Path $fp) {
  $bytes = [System.IO.File]::ReadAllBytes($fp)
  $ms = New-Object System.IO.MemoryStream(,$bytes)
  $img = [System.Drawing.Image]::FromStream($ms)
  $dataObj.SetImage($img)
}
[System.Windows.Forms.Clipboard]::SetDataObject($dataObj, $true)`, dir)
	clipCmd(ctx, "powershell", "-NoProfile", "-STA", "-Command", script).Run()
}

func writeClipboardLinux(ctx context.Context, formats []ClipFormat) {
	// Linux clipboard tools can only set one MIME type per invocation.
	// Write the most universally useful format.
	priority := []string{"text/plain", "text/html", "text/rtf", "image/png", "image/tiff"}
	for _, mime := range priority {
		for _, f := range formats {
			if f.MimeType != mime {
				continue
			}
			tool := detectLinuxClipTool()
			var cmd *exec.Cmd
			switch tool {
			case "xclip":
				target := mime
				if mime == "text/plain" {
					target = "UTF8_STRING"
				}
				cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", target, "-i")
			case "wl":
				cmd = clipCmd(ctx, "wl-copy", "--type", mime)
			default:
				return
			}
			cmd.Stdin = bytes.NewReader(f.Data)
			cmd.Run()
			return
		}
	}
}

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
		out, err := clipCmd(ctx, "osascript", "-e", script).Output()
		if err != nil {
			return nil
		}
		return parsePathList(string(out))

	case "windows":
		script := `(Get-Clipboard -Format FileDropList).FullName`
		out, err := clipCmd(ctx, "powershell", "-NoProfile", "-Command", script).Output()
		if err != nil {
			return nil
		}
		return parsePathList(string(out))

	case "linux":
		var out []byte
		switch detectLinuxClipTool() {
		case "xclip":
			out, _ = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-o").Output()
		case "wl":
			out, _ = clipCmd(ctx, "wl-paste", "--type", "text/uri-list").Output()
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
		clipCmd(ctx, "osascript", "-e", sb.String()).Run()

	case "windows":
		var sb strings.Builder
		sb.WriteString("Set-Clipboard -Path ")
		for i, p := range paths {
			sb.WriteString(fmt.Sprintf("'%s'", p))
			if i < len(paths)-1 {
				sb.WriteString(",")
			}
		}
		clipCmd(ctx, "powershell", "-NoProfile", "-Command", sb.String()).Run()

	case "linux":
		var sb strings.Builder
		for _, p := range paths {
			sb.WriteString(fmt.Sprintf("file://%s\r\n", p))
		}
		uriList := sb.String()

		var cmd *exec.Cmd
		switch detectLinuxClipTool() {
		case "xclip":
			cmd = clipCmd(ctx, "xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i")
		case "wl":
			cmd = clipCmd(ctx, "wl-copy", "--type", "text/uri-list")
		default:
			return
		}
		cmd.Stdin = strings.NewReader(uriList)
		cmd.Run()
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
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
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

	// Cache broadcast addresses; network interfaces rarely change at runtime.
	var cachedBcastAddrs []*net.UDPAddr
	var lastIfaceRefresh time.Time

	refreshBroadcastAddrs := func() []*net.UDPAddr {
		if time.Since(lastIfaceRefresh) < 30*time.Second {
			return cachedBcastAddrs
		}
		lastIfaceRefresh = time.Now()
		addrs := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}
		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagBroadcast == 0 {
				continue
			}
			ifAddrs, _ := iface.Addrs()
			for _, addr := range ifAddrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					ip := ipnet.IP.To4()
					mask := ipnet.Mask
					bcast := net.IPv4(ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3])
					addrs = append(addrs, &net.UDPAddr{IP: bcast, Port: port})
				}
			}
		}
		cachedBcastAddrs = addrs
		return addrs
	}

	// Pre-allocate beacon message struct to avoid map allocation each tick.
	type beaconMsg struct {
		Magic string `json:"m"`
		ID    string `json:"id"`
		Port  int    `json:"port"`
		Hash  string `json:"h"`
	}
	beacon := beaconMsg{Magic: magicHeader, ID: n.id, Port: n.port}

	for {
		select {
		case <-ticker.C:
		case <-n.notifyCh:
			slog.Debug("Broadcasting instant UDP beacon for new clipboard content")
		}

		n.stateMu.Lock()
		beacon.Hash = n.lastHash
		n.stateMu.Unlock()

		msg, _ := json.Marshal(&beacon)

		for _, addr := range refreshBroadcastAddrs() {
			conn.WriteTo(msg, addr)
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
