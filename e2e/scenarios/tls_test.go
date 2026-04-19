//go:build e2e

package scenarios

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/e2e/harness"
	"github.com/mmdemirbas/mesh/internal/tlsutil"
	"github.com/testcontainers/testcontainers-go/wait"
)

// generateTLSMaterial produces a fresh ECDSA P-256 self-signed cert under a
// per-test temp dir and returns the PEM bytes plus the "sha256:<hex>"
// fingerprint. The certs are injected into containers via NodeOptions.Files so
// each peer's listener identity is known up-front — no need to query a running
// peer for its fingerprint before configuring the other side.
func generateTLSMaterial(t *testing.T, cn string) (certPEM, keyPEM []byte, fingerprint string) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "filesync.crt")
	keyPath := filepath.Join(dir, "filesync.key")
	_, fp, err := tlsutil.AutoCert(certPath, keyPath, cn)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	certPEM, err = os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	keyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	return certPEM, keyPEM, fp
}

// bogusFingerprint returns a well-formed but never-used fingerprint suitable
// for the mismatch test. Uses a fixed byte pattern so failures are easy to
// identify in logs.
func bogusFingerprint() string {
	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = 0xAB
	}
	return "sha256:" + hex.EncodeToString(sum)
}

// peerStateEntry mirrors the subset of state.Component fields that the TLS
// assertions care about. The harness.ComponentState type omits tls_status and
// tls_fingerprint, so the scenario decodes the admin API response itself.
type peerStateEntry struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Status         string `json:"status"`
	Message        string `json:"message,omitempty"`
	TLSStatus      string `json:"tls_status,omitempty"`
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
}

func fetchState(ctx context.Context, t *testing.T, n *harness.Node) map[string]peerStateEntry {
	t.Helper()
	var raw map[string]peerStateEntry
	if err := n.AdminJSON(ctx, "/api/state", &raw); err != nil {
		t.Fatalf("%s: admin state: %v", n.Alias, err)
	}
	return raw
}

// startTLSPair mirrors startFilesyncPair but accepts rendered YAML bodies
// (the TLS scenarios substitute fingerprints at render time, not via sed)
// and per-peer extra files (certificate + key).
func startTLSPair(ctx context.Context, t *testing.T, net *harness.Network,
	aliasA, cfgA, peerPlaceholderA string, filesA []harness.File,
	aliasB, cfgB, peerPlaceholderB string, filesB []harness.File,
) (nodeA, nodeB *harness.Node) {
	t.Helper()

	noopWait := wait.ForExec([]string{"true"}).WithStartupTimeout(10 * time.Second)

	nodeA = harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      aliasA,
		Config:     cfgA,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrapEntrypoint(aliasA, aliasB, peerPlaceholderA)[1:],
		WaitFor:    noopWait,
		Files:      filesA,
	})
	nodeB = harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      aliasB,
		Config:     cfgB,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrapEntrypoint(aliasB, aliasA, peerPlaceholderB)[1:],
		Files:      filesB,
	})

	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
		fmt.Sprintf("%s admin responding", aliasA),
		func() (bool, string) {
			if _, err := nodeA.AdminGET(ctx, "/api/state"); err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	harness.DumpOnFailure(t, nodeA, nodeB)

	for _, p := range []*harness.Node{nodeA, nodeB} {
		harness.Eventually(ctx, t, 15*time.Second, 250*time.Millisecond,
			fmt.Sprintf("%s: folder 'shared' registered", p.Alias),
			func() (bool, string) {
				var folders []struct {
					ID string `json:"id"`
				}
				if err := p.AdminJSON(ctx, "/api/filesync/folders", &folders); err != nil {
					return false, err.Error()
				}
				for _, f := range folders {
					if f.ID == "shared" {
						return true, ""
					}
				}
				return false, fmt.Sprintf("folders=%v", folders)
			})
	}
	return nodeA, nodeB
}

// TestFilesyncTLSFingerprintPinning verifies the happy path for auto-TLS:
// two peers each pin the other's self-signed cert via tls_fingerprint,
// sync succeeds, and the per-peer state in /api/state reports
// "encrypted · verified".
func TestFilesyncTLSFingerprintPinning(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	cert1, key1, fp1 := generateTLSMaterial(t, "mesh-filesync-peer1")
	cert2, key2, fp2 := generateTLSMaterial(t, "mesh-filesync-peer2")

	cfg1, err := harness.RenderTemplate("configs/s6-tls-peer1.yaml", map[string]string{"PeerFingerprint": fp2})
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := harness.RenderTemplate("configs/s6-tls-peer2.yaml", map[string]string{"PeerFingerprint": fp1})
	if err != nil {
		t.Fatal(err)
	}

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	files1 := []harness.File{
		keepFile,
		{Path: "/root/.mesh/tls/filesync.crt", Content: cert1, Mode: 0o644},
		{Path: "/root/.mesh/tls/filesync.key", Content: key1, Mode: 0o600},
	}
	files2 := []harness.File{
		keepFile,
		{Path: "/root/.mesh/tls/filesync.crt", Content: cert2, Mode: 0o644},
		{Path: "/root/.mesh/tls/filesync.key", Content: key2, Mode: 0o600},
	}

	peer1, peer2 := startTLSPair(ctx, t, net,
		"peer1", cfg1, "PEER2_IP", files1,
		"peer2", cfg2, "PEER1_IP", files2,
	)

	// Sanity check: each peer advertises the exact cert fingerprint we
	// injected. If mesh regenerated its own cert (e.g. because the files
	// were misplaced), the rest of the test would still pass by accident —
	// pin that here so a regression in cert loading fails loudly.
	assertListenerFP := func(p *harness.Node, want string) {
		st := fetchState(ctx, t, p)
		for _, c := range st {
			if c.Type == "filesync" {
				if c.TLSFingerprint != want {
					t.Fatalf("%s: listener fingerprint=%q want %q", p.Alias, c.TLSFingerprint, want)
				}
				return
			}
		}
		t.Fatalf("%s: no filesync component in state", p.Alias)
	}
	assertListenerFP(peer1, fp1)
	assertListenerFP(peer2, fp2)

	// Drive sync by writing a file on peer1 and waiting for it on peer2.
	if err := peer1.WriteFile(ctx, "/root/sync/hello.txt", []byte("tls pinned\n"), 0o600); err != nil {
		t.Fatalf("seed hello.txt: %v", err)
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "hello.txt on peer2",
		func() (bool, string) {
			code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/hello.txt")
			if code == 0 {
				return true, ""
			}
			return false, "not yet"
		})

	// Every filesync-peer component on both sides must report
	// "encrypted · verified" — the string tlsStatusFor emits when the
	// client was built with a VerifyPeerCertificate callback.
	assertVerified := func(p *harness.Node) {
		harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond,
			fmt.Sprintf("%s: all filesync-peer entries verified", p.Alias),
			func() (bool, string) {
				st := fetchState(ctx, t, p)
				peers := 0
				for _, c := range st {
					if c.Type != "filesync-peer" {
						continue
					}
					peers++
					if c.TLSStatus != "encrypted · verified" {
						return false, fmt.Sprintf("id=%s tls_status=%q", c.ID, c.TLSStatus)
					}
				}
				if peers == 0 {
					return false, "no filesync-peer entries yet"
				}
				return true, ""
			})
	}
	assertVerified(peer1)
	assertVerified(peer2)
}

// TestFilesyncTLSFingerprintMismatch verifies the rejection path: if both
// peers are configured with fingerprints that do not match the cert the
// other actually presents, every sync attempt fails with ErrFingerprintMismatch,
// the state surfaces "CERT MISMATCH", and no files propagate.
func TestFilesyncTLSFingerprintMismatch(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	cert1, key1, _ := generateTLSMaterial(t, "mesh-filesync-peer1")
	cert2, key2, _ := generateTLSMaterial(t, "mesh-filesync-peer2")

	// Pin a fingerprint that neither peer's cert will ever match. Using the
	// same bogus value on both sides keeps the test symmetric — regardless
	// of which peer initiates, the handshake must fail.
	wrong := bogusFingerprint()
	cfg1, err := harness.RenderTemplate("configs/s6-tls-peer1.yaml", map[string]string{"PeerFingerprint": wrong})
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := harness.RenderTemplate("configs/s6-tls-peer2.yaml", map[string]string{"PeerFingerprint": wrong})
	if err != nil {
		t.Fatal(err)
	}

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	files1 := []harness.File{
		keepFile,
		{Path: "/root/.mesh/tls/filesync.crt", Content: cert1, Mode: 0o644},
		{Path: "/root/.mesh/tls/filesync.key", Content: key1, Mode: 0o600},
	}
	files2 := []harness.File{
		keepFile,
		{Path: "/root/.mesh/tls/filesync.crt", Content: cert2, Mode: 0o644},
		{Path: "/root/.mesh/tls/filesync.key", Content: key2, Mode: 0o600},
	}

	peer1, peer2 := startTLSPair(ctx, t, net,
		"peer1", cfg1, "PEER2_IP", files1,
		"peer2", cfg2, "PEER1_IP", files2,
	)

	// Write a file on peer1; it must NOT reach peer2 because peer1's client
	// refuses the TLS handshake with peer2.
	if err := peer1.WriteFile(ctx, "/root/sync/rejected.txt", []byte("should not sync\n"), 0o600); err != nil {
		t.Fatalf("seed rejected.txt: %v", err)
	}

	// Wait for the mismatch to surface on both peers' state. Each side
	// reports the rejection for its own outbound sync attempt.
	assertMismatch := func(p *harness.Node) {
		harness.Eventually(ctx, t, 45*time.Second, 500*time.Millisecond,
			fmt.Sprintf("%s: filesync-peer reports CERT MISMATCH", p.Alias),
			func() (bool, string) {
				st := fetchState(ctx, t, p)
				for _, c := range st {
					if c.Type != "filesync-peer" {
						continue
					}
					if c.TLSStatus == "CERT MISMATCH" {
						if !strings.Contains(c.Message, "fingerprint mismatch") {
							return false, fmt.Sprintf("tls_status ok but message=%q", c.Message)
						}
						return true, ""
					}
				}
				return false, "no CERT MISMATCH entry yet"
			})
	}
	assertMismatch(peer1)
	assertMismatch(peer2)

	// File must not have propagated. Poll briefly to make sure a lagging
	// successful sync isn't about to land.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/rejected.txt")
		if code == 0 {
			t.Fatal("file propagated despite fingerprint mismatch")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
