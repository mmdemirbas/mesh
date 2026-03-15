package clipsync

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
)

func TestContains(t *testing.T) {
	tests := []struct {
		slice []string
		item  string
		want  bool
	}{
		{[]string{"a", "b", "c"}, "b", true},
		{[]string{"a", "b", "c"}, "d", false},
		{[]string{}, "a", false},
		{nil, "a", false},
		{[]string{"all"}, "all", true},
		{[]string{"none"}, "all", false},
	}

	for _, tt := range tests {
		got := contains(tt.slice, tt.item)
		if got != tt.want {
			t.Errorf("contains(%v, %q) = %v, want %v", tt.slice, tt.item, got, tt.want)
		}
	}
}

func TestContainsIP(t *testing.T) {
	tests := []struct {
		name  string
		slice []string
		host  string
		want  bool
	}{
		{"exact match", []string{"192.168.1.1"}, "192.168.1.1", true},
		{"no match", []string{"192.168.1.1"}, "10.0.0.1", false},
		{"IPv6-mapped IPv4", []string{"192.168.1.5"}, "::ffff:192.168.1.5", true},
		{"IPv6 match", []string{"::1"}, "::1", true},
		{"empty slice", []string{}, "192.168.1.1", false},
		{"non-IP fallback", []string{"hostname"}, "hostname", true},
		{"non-IP no match", []string{"host1"}, "host2", false},
		{"multiple entries", []string{"10.0.0.1", "192.168.1.1"}, "192.168.1.1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsIP(tt.slice, tt.host)
			if got != tt.want {
				t.Errorf("containsIP(%v, %q) = %v, want %v", tt.slice, tt.host, got, tt.want)
			}
		})
	}
}

func TestHashBytes(t *testing.T) {
	input := []byte("hello world")
	h := md5.Sum(input)
	want := hex.EncodeToString(h[:])

	got := hashBytes(input)
	if got != want {
		t.Errorf("hashBytes(%q) = %q, want %q", input, got, want)
	}
}

func TestHashBytesEmpty(t *testing.T) {
	got := hashBytes(nil)
	h := md5.Sum(nil)
	want := hex.EncodeToString(h[:])
	if got != want {
		t.Errorf("hashBytes(nil) = %q, want %q", got, want)
	}
}

func TestHashBytesDeterministic(t *testing.T) {
	input := []byte("test data")
	h1 := hashBytes(input)
	h2 := hashBytes(input)
	if h1 != h2 {
		t.Error("hashBytes not deterministic")
	}
}

func TestHashFilePaths(t *testing.T) {
	// Order-independent
	h1 := hashFilePaths([]string{"/a/b", "/c/d"})
	h2 := hashFilePaths([]string{"/c/d", "/a/b"})
	if h1 != h2 {
		t.Error("hashFilePaths should be order-independent")
	}
}

func TestHashFilePathsDifferentSets(t *testing.T) {
	h1 := hashFilePaths([]string{"/a", "/b"})
	h2 := hashFilePaths([]string{"/a", "/c"})
	if h1 == h2 {
		t.Error("different file sets should produce different hashes")
	}
}

func TestHashFilePathsEmpty(t *testing.T) {
	h := hashFilePaths([]string{})
	if h == "" {
		t.Error("hashFilePaths of empty slice should return a valid hash")
	}
}

func TestHashFormats(t *testing.T) {
	formats := []ClipFormat{
		{MimeType: "text/plain", Data: []byte("hello")},
		{MimeType: "text/html", Data: []byte("<b>hello</b>")},
	}

	// Order-independent
	reversed := []ClipFormat{formats[1], formats[0]}
	h1 := hashFormats(formats)
	h2 := hashFormats(reversed)
	if h1 != h2 {
		t.Error("hashFormats should be order-independent (sorted by MimeType)")
	}
}

func TestHashFormatsDifferentData(t *testing.T) {
	f1 := []ClipFormat{{MimeType: "text/plain", Data: []byte("hello")}}
	f2 := []ClipFormat{{MimeType: "text/plain", Data: []byte("world")}}
	if hashFormats(f1) == hashFormats(f2) {
		t.Error("different data should produce different hashes")
	}
}

func TestHashFormatsEmpty(t *testing.T) {
	h := hashFormats(nil)
	if h == "" {
		t.Error("hashFormats(nil) should return a valid hash")
	}
}

func TestExtractCFHTMLFragment(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"standard CF_HTML",
			"Version:0.9\r\n<html><body>\r\n<!--StartFragment--><b>hello</b><!--EndFragment-->\r\n</body></html>",
			"<b>hello</b>",
		},
		{
			"with whitespace",
			"<!--StartFragment-->  some text  <!--EndFragment-->",
			"some text",
		},
		{
			"no start marker",
			"<html>no markers</html>",
			"",
		},
		{
			"no end marker",
			"<!--StartFragment-->partial",
			"",
		},
		{
			"markers reversed",
			"<!--EndFragment-->bad<!--StartFragment-->",
			"",
		},
		{
			"empty fragment",
			"<!--StartFragment--><!--EndFragment-->",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCFHTMLFragment(tt.input)
			if got != tt.want {
				t.Errorf("extractCFHTMLFragment() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCFHTML(t *testing.T) {
	fragment := "<b>hello</b>"
	result := buildCFHTML(fragment)

	// Should contain the fragment
	if !strings.Contains(result, fragment) {
		t.Error("result should contain the original fragment")
	}

	// Should contain CF_HTML markers
	if !strings.Contains(result, "Version:0.9") {
		t.Error("missing Version header")
	}
	if !strings.Contains(result, "StartHTML:") {
		t.Error("missing StartHTML header")
	}
	if !strings.Contains(result, "EndHTML:") {
		t.Error("missing EndHTML header")
	}
	if !strings.Contains(result, "<!--StartFragment-->") {
		t.Error("missing StartFragment marker")
	}
	if !strings.Contains(result, "<!--EndFragment-->") {
		t.Error("missing EndFragment marker")
	}
}

func TestBuildCFHTMLRoundTrip(t *testing.T) {
	fragment := "<p>test content</p>"
	cfhtml := buildCFHTML(fragment)
	extracted := extractCFHTMLFragment(cfhtml)

	if extracted != fragment {
		t.Errorf("roundtrip failed: got %q, want %q", extracted, fragment)
	}
}

func TestParsePathList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single path", "/home/user/file.txt", []string{"/home/user/file.txt"}},
		{"multiple paths", "/a/b\n/c/d\n/e/f", []string{"/a/b", "/c/d", "/e/f"}},
		{"with empty lines", "/a\n\n/b\n", []string{"/a", "/b"}},
		{"whitespace lines", "  /a  \n  \n  /b  ", []string{"/a", "/b"}},
		{"empty string", "", nil},
		{"only whitespace", "   \n  \n  ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePathList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseURIList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single file URI", "file:///home/user/file.txt", []string{"/home/user/file.txt"}},
		{"multiple URIs", "file:///a/b\nfile:///c/d", []string{"/a/b", "/c/d"}},
		{"skip comments", "# comment\nfile:///a/b", []string{"/a/b"}},
		{"skip empty lines", "file:///a\n\nfile:///b", []string{"/a", "/b"}},
		{"skip non-file URIs", "http://example.com\nfile:///a/b", []string{"/a/b"}},
		{"empty string", "", nil},
		{"only comments", "# comment1\n# comment2", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseURIList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCanSendTo(t *testing.T) {
	tests := []struct {
		name      string
		allowSend []string
		addr      string
		isUDP     bool
		want      bool
	}{
		{"all allows everything", []string{"all"}, "192.168.1.1:7755", false, true},
		{"none blocks everything", []string{"none"}, "192.168.1.1:7755", false, false},
		{"udp keyword allows UDP", []string{"udp"}, "192.168.1.1:7755", true, true},
		{"udp keyword blocks non-UDP", []string{"udp"}, "192.168.1.1:7755", false, false},
		{"specific IP match", []string{"192.168.1.1"}, "192.168.1.1:7755", false, true},
		{"specific IP no match", []string{"192.168.1.1"}, "10.0.0.1:7755", false, false},
		{"full addr match", []string{"192.168.1.1:7755"}, "192.168.1.1:7755", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClipsyncCfg{AllowSendTo: tt.allowSend}
			n := &Node{config: cfg, sendToIPs: parseIPList(cfg.AllowSendTo)}
			got := n.canSendTo(tt.addr, tt.isUDP)
			if got != tt.want {
				t.Errorf("canSendTo(%q, %v) = %v, want %v", tt.addr, tt.isUDP, got, tt.want)
			}
		})
	}
}

func TestCanReceiveFrom(t *testing.T) {
	tests := []struct {
		name     string
		allowRcv []string
		addr     string
		want     bool
	}{
		{"all allows everything", []string{"all"}, "192.168.1.1:7755", true},
		{"none blocks everything", []string{"none"}, "192.168.1.1:7755", false},
		{"specific IP match", []string{"192.168.1.1"}, "192.168.1.1:7755", true},
		{"specific IP no match", []string{"192.168.1.1"}, "10.0.0.1:7755", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ClipsyncCfg{AllowReceive: tt.allowRcv}
			n := &Node{config: cfg, receiveIPs: parseIPList(cfg.AllowReceive)}
			got := n.canReceiveFrom(tt.addr)
			if got != tt.want {
				t.Errorf("canReceiveFrom(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestCheckHash(t *testing.T) {
	n := &Node{}

	// First call with a hash should return true (different from initial empty)
	if !n.checkHash("abc123") {
		t.Error("first checkHash should return true")
	}

	// Same hash again should return false
	if n.checkHash("abc123") {
		t.Error("same hash should return false")
	}

	// Different hash should return true
	if !n.checkHash("def456") {
		t.Error("different hash should return true")
	}
}

// --- HTTP protocol & sync integration tests ---

// newTestNode creates a minimal Node with an HTTP server for testing sync endpoints.
// Returns the node, its base URL, and a cleanup function.
func newTestNode(t *testing.T, allowReceive []string) (*Node, string, func()) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.ClipsyncCfg{
		Bind:         "127.0.0.1:0",
		AllowSendTo:  []string{"all"},
		AllowReceive: allowReceive,
	}
	n := &Node{
		config:     cfg,
		id:         "test-node",
		port:       7755,
		sendToIPs:  parseIPList(cfg.AllowSendTo),
		receiveIPs: parseIPList(cfg.AllowReceive),
		peers:      make(map[string]time.Time),
		peerHashes: make(map[string]string),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		filesDir:   dir,
		notifyCh:   make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		peerHostPort := net.JoinHostPort(host, "7755")
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
	fileServer := http.StripPrefix("/files/", http.FileServer(http.Dir(n.filesDir)))
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if !n.canReceiveFrom(r.RemoteAddr) {
			http.Error(w, "Forbidden by ACL", http.StatusForbidden)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	srv := httptest.NewServer(mux)
	return n, srv.URL, srv.Close
}

func TestSyncEndpoint_PushFormats(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	payload := Payload{
		Formats: []ClipFormat{
			{MimeType: "text/plain", Data: []byte("hello from peer")},
		},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(url+"/sync", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sync failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /sync status = %d, want 200", resp.StatusCode)
	}

	// Verify the written hash was recorded (proves processPayload ran)
	n.stateMu.Lock()
	h := n.lastWrittenHash
	n.stateMu.Unlock()
	if h == "" {
		t.Error("lastWrittenHash not set after pushing formats")
	}
}

func TestSyncEndpoint_PushFilesEmbedded(t *testing.T) {
	// This tests the one-way connectivity case:
	// the sender embeds file data directly in the POST payload
	// so the receiver doesn't need to pull files back.
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	fileContent := []byte("file content from one-way peer")
	payload := Payload{
		Files: []FileRef{
			{FileID: "test123.txt", FileName: "document.txt", Data: fileContent},
		},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(url+"/sync", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sync failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /sync status = %d, want 200", resp.StatusCode)
	}

	// Verify the file was written to disk with the correct content
	written, err := os.ReadFile(filepath.Join(n.filesDir, "document.txt"))
	if err != nil {
		t.Fatalf("expected file not found: %v", err)
	}
	if !bytes.Equal(written, fileContent) {
		t.Errorf("file content = %q, want %q", written, fileContent)
	}
}

func TestSyncEndpoint_PushFilesEmbedded_MultipleFiles(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	payload := Payload{
		Files: []FileRef{
			{FileID: "a.txt", FileName: "first.txt", Data: []byte("content-a")},
			{FileID: "b.png", FileName: "image.png", Data: []byte("fake-png-data")},
		},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(url+"/sync", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sync failed: %v", err)
	}
	resp.Body.Close()

	for _, f := range payload.Files {
		got, err := os.ReadFile(filepath.Join(n.filesDir, f.FileName))
		if err != nil {
			t.Errorf("file %q not written: %v", f.FileName, err)
			continue
		}
		if !bytes.Equal(got, f.Data) {
			t.Errorf("file %q content = %q, want %q", f.FileName, got, f.Data)
		}
	}
}

func TestSyncEndpoint_PushFilesWithoutData_PullBack(t *testing.T) {
	// This tests the two-way case: receiver can reach the sender,
	// so files are NOT embedded — the receiver pulls them via /files/.
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	// Pre-stage a file on the "sender" node's filesDir so /files/ can serve it
	fileContent := []byte("pullable content")
	os.WriteFile(filepath.Join(n.filesDir, "pullme.txt"), fileContent, 0644)

	// Now simulate a payload where Data is empty (receiver must pull)
	payload := Payload{
		Files: []FileRef{
			{FileID: "pullme.txt", FileName: "pulled.txt"},
		},
	}

	// processPayload will try to downloadFile from peerHostPort.
	// Since peerHostPort will be the test server, we need to call processPayload
	// directly with the correct peer address.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	peerHostPort := net.JoinHostPort("127.0.0.1", port)

	n.processPayload(payload, peerHostPort)

	// Verify the file was pulled and written
	got, err := os.ReadFile(filepath.Join(n.filesDir, "pulled.txt"))
	if err != nil {
		t.Fatalf("pulled file not found: %v", err)
	}
	if !bytes.Equal(got, fileContent) {
		t.Errorf("pulled file content = %q, want %q", got, fileContent)
	}
}

func TestClipEndpoint_ReturnsLastPayload(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	// Set a payload via Broadcast (simulates local clipboard change)
	payload := Payload{
		Formats: []ClipFormat{
			{MimeType: "text/plain", Data: []byte("clipboard content")},
		},
	}
	data, _ := json.Marshal(payload)
	n.stateMu.Lock()
	n.lastPayload = data
	n.stateMu.Unlock()

	resp, err := http.Get(url + "/clip")
	if err != nil {
		t.Fatalf("GET /clip failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /clip status = %d, want 200", resp.StatusCode)
	}

	var got Payload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Formats) != 1 || got.Formats[0].MimeType != "text/plain" {
		t.Errorf("GET /clip returned unexpected payload: %+v", got)
	}
	if string(got.Formats[0].Data) != "clipboard content" {
		t.Errorf("data = %q, want %q", got.Formats[0].Data, "clipboard content")
	}
}

func TestClipEndpoint_EmptyReturns404(t *testing.T) {
	_, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	resp, err := http.Get(url + "/clip")
	if err != nil {
		t.Fatalf("GET /clip failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("GET /clip status = %d, want 404 when no content", resp.StatusCode)
	}
}

func TestSyncEndpoint_ACLBlocks(t *testing.T) {
	_, url, cleanup := newTestNode(t, []string{"none"})
	defer cleanup()

	payload := Payload{
		Formats: []ClipFormat{{MimeType: "text/plain", Data: []byte("blocked")}},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(url+"/sync", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sync failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /sync status = %d, want 403", resp.StatusCode)
	}
}

func TestFilesEndpoint_ServesFiles(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	content := []byte("served file content")
	os.WriteFile(filepath.Join(n.filesDir, "test.txt"), content, 0644)

	resp, err := http.Get(url + "/files/test.txt")
	if err != nil {
		t.Fatalf("GET /files/test.txt failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /files/test.txt status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("file content = %q, want %q", got, content)
	}
}

func TestFilesEndpoint_ACLBlocks(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"none"})
	defer cleanup()

	os.WriteFile(filepath.Join(n.filesDir, "secret.txt"), []byte("secret"), 0644)

	resp, err := http.Get(url + "/files/secret.txt")
	if err != nil {
		t.Fatalf("GET /files/secret.txt failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /files/secret.txt status = %d, want 403", resp.StatusCode)
	}
}

func TestSyncEndpoint_RejectsPathTraversal(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	payload := Payload{
		Files: []FileRef{
			{FileID: "evil.txt", FileName: "../../etc/passwd", Data: []byte("malicious")},
		},
	}
	data, _ := json.Marshal(payload)

	resp, err := http.Post(url+"/sync", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST /sync failed: %v", err)
	}
	resp.Body.Close()

	// The file should NOT be written outside filesDir
	if _, err := os.Stat(filepath.Join(n.filesDir, "..", "..", "etc", "passwd")); err == nil {
		t.Error("path traversal was not blocked")
	}

	// The sanitized name "passwd" should also not be written because ../../etc/passwd
	// gets sanitized to "passwd" by filepath.Base — which is actually fine and safe.
	// So we just verify nothing was written to the actual traversal path.
}

func TestPostHTTP_UsesZeroCopyReader(t *testing.T) {
	// Verify postHTTP sends the exact bytes without copying
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	cfg := config.ClipsyncCfg{AllowSendTo: []string{"all"}}
	n := &Node{
		config:     cfg,
		sendToIPs:  parseIPList(cfg.AllowSendTo),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	data := []byte(`{"formats":[{"mime_type":"text/plain","data":"aGVsbG8="}]}`)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	n.postHTTP(net.JoinHostPort("127.0.0.1", port), data)

	if !bytes.Equal(received, data) {
		t.Errorf("received %q, want %q", received, data)
	}
}

func TestBroadcast_SetsLastPayload(t *testing.T) {
	n := &Node{
		config:     config.ClipsyncCfg{AllowSendTo: []string{"none"}},
		sendToIPs:  nil,
		peers:      make(map[string]time.Time),
		peerHashes: make(map[string]string),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		notifyCh:   make(chan struct{}, 1),
	}

	payload := Payload{
		Formats: []ClipFormat{{MimeType: "text/plain", Data: []byte("test")}},
	}
	n.Broadcast(payload)

	n.stateMu.Lock()
	data := n.lastPayload
	n.stateMu.Unlock()

	if data == nil {
		t.Fatal("lastPayload not set after Broadcast")
	}

	var got Payload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal lastPayload: %v", err)
	}
	if len(got.Formats) != 1 || string(got.Formats[0].Data) != "test" {
		t.Errorf("lastPayload content unexpected: %+v", got)
	}
}

func TestBroadcast_PushesToPeers(t *testing.T) {
	receivedCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		receivedCh <- data
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	peerAddr := net.JoinHostPort("127.0.0.1", port)

	cfg := config.ClipsyncCfg{
		AllowSendTo: []string{"all"},
		StaticPeers: []string{peerAddr},
	}
	n := &Node{
		config:     cfg,
		sendToIPs:  parseIPList(cfg.AllowSendTo),
		peers:      make(map[string]time.Time),
		peerHashes: make(map[string]string),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		notifyCh:   make(chan struct{}, 1),
	}

	payload := Payload{
		Formats: []ClipFormat{{MimeType: "text/plain", Data: []byte("pushed")}},
	}
	n.Broadcast(payload)

	select {
	case received := <-receivedCh:
		var got Payload
		if err := json.Unmarshal(received, &got); err != nil {
			t.Fatalf("unmarshal received: %v", err)
		}
		if len(got.Formats) != 1 || string(got.Formats[0].Data) != "pushed" {
			t.Errorf("received unexpected payload: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer never received the broadcast")
	}
}

func TestBroadcast_DoesNotEchoBackToOrigin(t *testing.T) {
	// Broadcast sends to static peers via `go n.postHTTP(addr, data)`.
	// When originAddr matches the peer, the peer should be skipped entirely —
	// no goroutine is launched — so we can verify synchronously that no
	// HTTP request arrives. We use a short client timeout to bound the test.
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
	}))
	defer srv.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	peerAddr := net.JoinHostPort("127.0.0.1", port)

	cfg := config.ClipsyncCfg{
		AllowSendTo: []string{"all"},
		StaticPeers: []string{peerAddr},
	}
	n := &Node{
		config:     cfg,
		sendToIPs:  parseIPList(cfg.AllowSendTo),
		peers:      make(map[string]time.Time),
		peerHashes: make(map[string]string),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		notifyCh:   make(chan struct{}, 1),
	}

	// Simulate that we received this payload from the same peer
	n.stateMu.Lock()
	n.originAddr = peerAddr
	n.stateMu.Unlock()

	payload := Payload{
		Formats: []ClipFormat{{MimeType: "text/plain", Data: []byte("no echo")}},
	}
	n.Broadcast(payload)

	// Broadcast skips the origin peer synchronously (no goroutine launched).
	// To be thorough, also verify no goroutine fires by trying to push to
	// a second non-origin peer and waiting for that to confirm the path ran.
	// But since there's only one static peer (the origin), Broadcast returns
	// immediately with zero goroutines — the check below is deterministic.
	if callCount.Load() != 0 {
		t.Errorf("peer was called %d times, want 0 (should not echo back to origin)", callCount.Load())
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()

	if len(id1) != 16 { // 8 random bytes → 16 hex chars
		t.Errorf("generateID() length = %d, want 16", len(id1))
	}
	if id1 == id2 {
		t.Error("two generateID() calls returned the same value")
	}
}

func TestLoadFormatsFromDir(t *testing.T) {
	dir := t.TempDir()

	// Write some format files matching clipFormatTable entries
	os.WriteFile(filepath.Join(dir, "text_plain"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "text_html"), []byte("<b>hi</b>"), 0644)
	os.WriteFile(filepath.Join(dir, "image_png"), []byte("PNG-DATA"), 0644)

	formats := loadFormatsFromDir(dir)

	// Should find the 3 formats we wrote
	if len(formats) != 3 {
		t.Fatalf("loadFormatsFromDir returned %d formats, want 3", len(formats))
	}

	mimeMap := make(map[string]string)
	for _, f := range formats {
		mimeMap[f.MimeType] = string(f.Data)
	}

	if mimeMap["text/plain"] != "hello" {
		t.Errorf("text/plain = %q, want %q", mimeMap["text/plain"], "hello")
	}
	if mimeMap["text/html"] != "<b>hi</b>" {
		t.Errorf("text/html = %q, want %q", mimeMap["text/html"], "<b>hi</b>")
	}
	if mimeMap["image/png"] != "PNG-DATA" {
		t.Errorf("image/png = %q, want %q", mimeMap["image/png"], "PNG-DATA")
	}
}

func TestLoadFormatsFromDir_EmptyDir(t *testing.T) {
	formats := loadFormatsFromDir(t.TempDir())
	if len(formats) != 0 {
		t.Errorf("expected 0 formats for empty dir, got %d", len(formats))
	}
}

func TestLoadFormatsFromDir_SkipsEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "text_plain"), []byte{}, 0644)

	formats := loadFormatsFromDir(dir)
	if len(formats) != 0 {
		t.Errorf("expected 0 formats for empty file, got %d", len(formats))
	}
}

func TestLoadFormatsFromDir_IgnoresUnknownFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "unknown_format"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "text_plain"), []byte("real"), 0644)

	formats := loadFormatsFromDir(dir)
	if len(formats) != 1 {
		t.Errorf("expected 1 format, got %d", len(formats))
	}
}

func TestPullHTTP(t *testing.T) {
	n, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()

	// Set last payload (simulating a clipboard state on the "sender")
	payload := Payload{
		Formats: []ClipFormat{{MimeType: "text/plain", Data: []byte("pulled content")}},
	}
	data, _ := json.Marshal(payload)
	n.stateMu.Lock()
	n.lastPayload = data
	n.stateMu.Unlock()

	// Create a receiver node that pulls from the sender
	receiverDir := t.TempDir()
	receiver := &Node{
		config:     config.ClipsyncCfg{AllowReceive: []string{"all"}},
		receiveIPs: nil,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		filesDir:   receiverDir,
		notifyCh:   make(chan struct{}, 1),
	}

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	peerAddr := net.JoinHostPort("127.0.0.1", port)

	receiver.pullHTTP(peerAddr)

	// pullHTTP calls processPayload which sets lastWrittenHash
	receiver.stateMu.Lock()
	h := receiver.lastWrittenHash
	receiver.stateMu.Unlock()
	if h == "" {
		t.Error("pullHTTP did not process the payload (lastWrittenHash empty)")
	}
}

func TestPullHTTP_NoContent(t *testing.T) {
	_, url, cleanup := newTestNode(t, []string{"all"})
	defer cleanup()
	// lastPayload is nil → /clip returns 404

	receiver := &Node{
		config:     config.ClipsyncCfg{AllowReceive: []string{"all"}},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		filesDir:   t.TempDir(),
		notifyCh:   make(chan struct{}, 1),
	}

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	receiver.pullHTTP(net.JoinHostPort("127.0.0.1", port))

	// Should not crash and lastWrittenHash should remain empty
	receiver.stateMu.Lock()
	h := receiver.lastWrittenHash
	receiver.stateMu.Unlock()
	if h != "" {
		t.Error("pullHTTP should not set hash when /clip returns 404")
	}
}

func TestSetWrittenHash(t *testing.T) {
	n := &Node{}
	n.setWrittenHash("hash123", "192.168.1.1:7755")

	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	if n.lastHash != "hash123" {
		t.Errorf("lastHash = %q, want %q", n.lastHash, "hash123")
	}
	if n.lastWrittenHash != "hash123" {
		t.Errorf("lastWrittenHash = %q, want %q", n.lastWrittenHash, "hash123")
	}
	if n.originAddr != "192.168.1.1:7755" {
		t.Errorf("originAddr = %q, want %q", n.originAddr, "192.168.1.1:7755")
	}
}

func TestPurgeFilesDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old1.txt"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(dir, "old2.txt"), []byte("old"), 0644)

	n := &Node{filesDir: dir}
	n.purgeFilesDir()

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("purgeFilesDir left %d files, want 0", len(entries))
	}
}

func TestClearCurrentFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	os.WriteFile(f1, []byte("a"), 0644)
	os.WriteFile(f2, []byte("b"), 0644)

	n := &Node{currentFiles: []string{f1, f2}}
	n.clearCurrentFiles()

	if _, err := os.Stat(f1); err == nil {
		t.Error("file a.txt should have been deleted")
	}
	if _, err := os.Stat(f2); err == nil {
		t.Error("file b.txt should have been deleted")
	}
	if n.currentFiles != nil {
		t.Error("currentFiles should be nil after clear")
	}
}

func TestClearCurrentFiles_NoFiles(t *testing.T) {
	n := &Node{}
	n.clearCurrentFiles() // should not panic
}

func TestMatchesIPList(t *testing.T) {
	ips := parseIPList([]string{"192.168.1.1", "10.0.0.5", "not-an-ip"})
	if len(ips) != 2 {
		t.Fatalf("parseIPList returned %d IPs, want 2", len(ips))
	}

	if !matchesIPList(ips, "192.168.1.1") {
		t.Error("should match 192.168.1.1")
	}
	if !matchesIPList(ips, "::ffff:192.168.1.1") {
		t.Error("should match IPv6-mapped 192.168.1.1")
	}
	if matchesIPList(ips, "172.16.0.1") {
		t.Error("should not match 172.16.0.1")
	}
	if matchesIPList(ips, "not-an-ip") {
		t.Error("non-IP host should not match")
	}
	if matchesIPList(nil, "192.168.1.1") {
		t.Error("nil list should not match anything")
	}
}
