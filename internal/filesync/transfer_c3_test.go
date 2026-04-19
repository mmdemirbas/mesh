package filesync

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mmdemirbas/mesh/internal/config"
	pb "github.com/mmdemirbas/mesh/internal/filesync/proto"
	"google.golang.org/protobuf/proto"
)

// C3 tests pin the per-block verify behavior in downloadToVerifiedTemp.
// They wrap the HTTP transport with a middleware that can rewrite response
// bodies, simulating network-level corruption.

// corruptingRT wraps an http.RoundTripper and lets a test callback inspect
// and mutate responses (typically the /file body) before handing them back
// to the client.
type corruptingRT struct {
	inner   http.RoundTripper
	corrupt func(req *http.Request, body []byte) []byte
}

func (c *corruptingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(req)
	if err != nil || resp == nil || c.corrupt == nil {
		return resp, err
	}
	buf, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	out := c.corrupt(req, buf)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	return resp, nil
}

// c3Setup builds a filesync server that hosts a single test file at relPath
// and returns (peerAddr, baseClient, teardown). The returned client trusts
// the test server's TLS cert.
func c3Setup(t *testing.T, relPath string, content []byte) (string, *http.Client, *httptest.Server) {
	t.Helper()
	srcDir := t.TempDir()
	writeFile(t, srcDir, relPath, string(content))

	n := &Node{
		cfg:      testCfg(srcDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "c3-sender",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(srcDir, "127.0.0.1"),
		root: openTestRoot(t, srcDir),
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), srv.Client(), srv
}

// c3ClientWith returns a client that sends the same TLS roots as srv.Client()
// but routes every /file response through the corrupt callback.
func c3ClientWith(srv *httptest.Server, corrupt func(req *http.Request, body []byte) []byte) *http.Client {
	base := srv.Client()
	return &http.Client{Transport: &corruptingRT{inner: base.Transport, corrupt: corrupt}}
}

// randomBytes returns n bytes from crypto/rand. Fails the test on error.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

// TestC3HappyPathPerBlockVerify: multi-block file, clean transport. The
// download must succeed through the /blocksigs + per-block-verify path and
// the written file must match the source byte-for-byte.
func TestC3HappyPathPerBlockVerify(t *testing.T) {
	t.Parallel()
	// 3 full 128 KB blocks + a small partial tail exercises both the full
	// and short-block read branches.
	content := randomBytes(t, defaultBlockSize*3+7331)
	expected := Hash256(sha256.Sum256(content))

	peerAddr, client, _ := c3Setup(t, "data.bin", content)

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	relPath, err := downloadFile(t.Context(), client, peerAddr, "test", "data.bin", expected, destRoot, nil)
	if err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestC3BlockCorruptionRecoverable: first /file response has one byte flipped
// inside block 1; the receiver must detect the mismatch, truncate to the
// last verified boundary, reissue /file?offset=, and complete successfully.
func TestC3BlockCorruptionRecoverable(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, defaultBlockSize*3)
	expected := Hash256(sha256.Sum256(content))

	peerAddr, _, srv := c3Setup(t, "data.bin", content)

	var fileHits atomic.Int32
	corrupt := func(req *http.Request, body []byte) []byte {
		if !strings.HasPrefix(req.URL.Path, "/file") {
			return body
		}
		hit := fileHits.Add(1)
		if hit == 1 && len(body) > defaultBlockSize+100 {
			// Flip one byte in the second block (index defaultBlockSize+50).
			corrupted := make([]byte, len(body))
			copy(corrupted, body)
			corrupted[defaultBlockSize+50] ^= 0xFF
			return corrupted
		}
		return body
	}
	client := c3ClientWith(srv, corrupt)

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	relPath, err := downloadFile(t.Context(), client, peerAddr, "test", "data.bin", expected, destRoot, nil)
	if err != nil {
		t.Fatalf("expected recovery, got error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch after recovery")
	}
	if fileHits.Load() < 2 {
		t.Fatalf("expected at least 2 /file requests (initial + retry), got %d", fileHits.Load())
	}
}

// TestC3RepeatedCorruptionQuarantines: every /file response flips the same
// block. The receiver must exhaust maxBlockRetries and surface an error so
// the caller's retryTracker can quarantine the (path, peer) pair.
func TestC3RepeatedCorruptionQuarantines(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, defaultBlockSize*2)
	expected := Hash256(sha256.Sum256(content))

	peerAddr, _, srv := c3Setup(t, "data.bin", content)

	var fileHits atomic.Int32
	corrupt := func(req *http.Request, body []byte) []byte {
		if !strings.HasPrefix(req.URL.Path, "/file") {
			return body
		}
		fileHits.Add(1)
		// Always flip a byte in the first block.
		corrupted := make([]byte, len(body))
		copy(corrupted, body)
		if len(corrupted) > 10 {
			corrupted[10] ^= 0xFF
		}
		return corrupted
	}
	client := c3ClientWith(srv, corrupt)

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	_, err := downloadFile(t.Context(), client, peerAddr, "test", "data.bin", expected, destRoot, nil)
	if err == nil {
		t.Fatalf("expected error after exhausted retries, got nil")
	}
	if !strings.Contains(err.Error(), "block verify failed") {
		t.Fatalf("expected 'block verify failed' error, got: %v", err)
	}
	// Initial attempt + maxBlockRetries refetches.
	if fileHits.Load() != int32(maxBlockRetries+1) {
		t.Fatalf("expected %d /file requests, got %d", maxBlockRetries+1, fileHits.Load())
	}
	// Temp file must be removed so the next sync cycle starts clean.
	matches, _ := filepath.Glob(filepath.Join(destDir, ".mesh-tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp file not cleaned up: %v", matches)
	}
}

// TestC3FallsBackWhenBlockSigsMissing: server mux without /blocksigs. The
// receiver must silently fall back to whole-file verify and succeed.
func TestC3FallsBackWhenBlockSigsMissing(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, defaultBlockSize+500)
	expected := Hash256(sha256.Sum256(content))

	srcDir := t.TempDir()
	writeFile(t, srcDir, "data.bin", string(content))
	n := &Node{
		cfg:      testCfg(srcDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "c3-legacy",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(srcDir, "127.0.0.1"),
		root: openTestRoot(t, srcDir),
	}
	// Build a mux that intentionally omits /blocksigs so tryFetchBlockSignatures
	// sees a 404. This models a peer on an older build.
	s := &server{node: n}
	mux := http.NewServeMux()
	mux.HandleFunc("/file", s.handleFile)
	mux.HandleFunc("/index", s.handleIndex)
	// /blocksigs deliberately not registered → 404 from the default mux.
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	relPath, err := downloadFile(t.Context(), srv.Client(),
		srv.Listener.Addr().String(), "test", "data.bin", expected, destRoot, nil)
	if err != nil {
		t.Fatalf("fallback path failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("fallback content mismatch")
	}
}

// TestC3SingleBlockFile: file smaller than one block. Exercises the partial
// last-block path (want = totalSize < blockSize).
func TestC3SingleBlockFile(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, 17)
	expected := Hash256(sha256.Sum256(content))

	peerAddr, client, _ := c3Setup(t, "tiny.bin", content)

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	relPath, err := downloadFile(t.Context(), client, peerAddr, "test", "tiny.bin", expected, destRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destDir, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch for single-block file")
	}
}

// TestC3WholeFileHashMismatchAfterBlocksPass: defends against the sender-side
// race where /blocksigs reports hashes for file version A but /file serves
// version B whose block hashes happen to match (in practice, impossible unless
// /blocksigs lies). The final whole-file hash check must still catch any
// divergence from the expected index hash.
func TestC3WholeFileHashMismatchRejected(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, defaultBlockSize+200)
	wrongExpected := testHash("not-the-real-hash")

	peerAddr, client, _ := c3Setup(t, "data.bin", content)

	destDir := t.TempDir()
	destRoot := openTestRoot(t, destDir)

	_, err := downloadFile(t.Context(), client, peerAddr, "test", "data.bin", wrongExpected, destRoot, nil)
	if err == nil {
		t.Fatalf("expected hash-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash-mismatch error, got: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, ".mesh-tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp file not cleaned up: %v", matches)
	}
}

// TestC3BlockSigsEndpoint: direct integration test of the new HTTP surface.
// Verifies block_hashes length matches ceil(file_size / block_size), each
// hash is a valid SHA-256, and the sum of block hashes reconstructs the
// whole-file hash when re-hashed from disk.
func TestC3BlockSigsEndpoint(t *testing.T) {
	t.Parallel()
	content := randomBytes(t, defaultBlockSize*2+1024)
	srcDir := t.TempDir()
	writeFile(t, srcDir, "data.bin", string(content))

	n := &Node{
		cfg:      testCfg(srcDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "c3-blocksigs",
	}
	n.folders["test"] = &folderState{
		cfg:  testFolderCfg(srcDir, "127.0.0.1"),
		root: openTestRoot(t, srcDir),
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	defer srv.Close()

	resp, err := srv.Client().Get(fmt.Sprintf("https://%s/blocksigs?folder=test&path=data.bin",
		srv.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)

	var sigs pb.BlockSignatures
	if err := proto.Unmarshal(body, &sigs); err != nil {
		t.Fatal(err)
	}
	if sigs.GetFileSize() != int64(len(content)) {
		t.Fatalf("file_size = %d, want %d", sigs.GetFileSize(), len(content))
	}
	if sigs.GetBlockSize() != defaultBlockSize {
		t.Fatalf("block_size = %d, want %d", sigs.GetBlockSize(), defaultBlockSize)
	}
	expectedBlocks := (int64(len(content)) + defaultBlockSize - 1) / defaultBlockSize
	if int64(len(sigs.GetBlockHashes())) != expectedBlocks {
		t.Fatalf("block count = %d, want %d", len(sigs.GetBlockHashes()), expectedBlocks)
	}
	// Verify each block hash: rehash the same slice and compare.
	for i, h := range sigs.GetBlockHashes() {
		start := int64(i) * defaultBlockSize
		end := min(start+defaultBlockSize, int64(len(content)))
		want := sha256.Sum256(content[start:end])
		if !bytes.Equal(h, want[:]) {
			t.Fatalf("block %d hash mismatch", i)
		}
	}
}

// TestC3BlockSigsRejectsReceiveOnly: security gate — a folder configured as
// receive-only must not leak block hashes via /blocksigs any more than it
// leaks file content via /file.
func TestC3BlockSigsRejectsReceiveOnly(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	writeFile(t, srcDir, "secret.txt", "top secret content")

	n := &Node{
		cfg:      testCfg(srcDir, "127.0.0.1"),
		folders:  make(map[string]*folderState),
		deviceID: "c3-recv-only",
	}
	n.folders["test"] = &folderState{
		cfg: config.FolderCfg{
			ID:        "test",
			Path:      srcDir,
			Direction: "receive-only",
			Peers:     []string{"127.0.0.1:7756"},
		},
		root: openTestRoot(t, srcDir),
	}
	srv := httptest.NewTLSServer((&server{node: n}).handler())
	defer srv.Close()

	resp, err := srv.Client().Get(fmt.Sprintf("https://%s/blocksigs?folder=test&path=secret.txt",
		srv.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}
