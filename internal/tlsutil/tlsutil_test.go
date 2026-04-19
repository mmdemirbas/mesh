package tlsutil

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoCert_GeneratesOnMissingFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, fp, err := AutoCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected non-empty certificate chain")
	}
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 {
		t.Errorf("unexpected fingerprint format: %q", fp)
	}
}

func TestAutoCert_LoadsExistingValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	_, fp1, err := AutoCert(certPath, keyPath, "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, err := AutoCert(certPath, keyPath, "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed on reload: %s → %s", fp1, fp2)
	}
}

func TestAutoCert_RegeneratesNearExpiry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// Write a cert expiring in 10 days (within renewBefore = 30 days).
	shortValidity := 10 * 24 * time.Hour
	if _, _, err := generateWithValidity(certPath, keyPath, "mesh-test", shortValidity); err != nil {
		t.Fatal(err)
	}
	cert1, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := Fingerprint(cert1)

	_, fp2, err := AutoCert(certPath, keyPath, "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp2 {
		t.Error("expected new fingerprint after near-expiry regeneration, got same")
	}
}

func TestAutoCert_KeyPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if _, _, err := AutoCert(filepath.Join(dir, "cert.pem"), keyPath, "mesh-test"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("key permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestAutoCert_CreatesMissingParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "tls")
	_, _, err := AutoCert(filepath.Join(nested, "cert.pem"), filepath.Join(nested, "key.pem"), "mesh-test")
	if err != nil {
		t.Fatalf("should create missing parent directories: %v", err)
	}
}

// TestFingerprint_PinnedPEM is the algorithm anchor for Fingerprint.
//
// The expected value is the SHA-256 of testdata/pinned.crt's DER bytes,
// computed out-of-band with `openssl x509 -noout -fingerprint -sha256`.
// Every other fingerprint test in this file compares values produced by
// Fingerprint against each other, so a hash-algorithm change (or a bug
// in sha256sum) would go unnoticed. This test anchors the algorithm.
func TestFingerprint_PinnedPEM(t *testing.T) {
	t.Parallel()
	cert, err := tls.LoadX509KeyPair("testdata/pinned.crt", "testdata/pinned.key")
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:626f6f09e36cd1e092ec5aad40b57a6eb385ced03f826a73fb78bc1449c74f90"
	if got := Fingerprint(cert); got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
}

func TestFingerprint_Stable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, fp1, err := AutoCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	if fp2 := Fingerprint(cert); fp1 != fp2 {
		t.Errorf("Fingerprint not stable: %s vs %s", fp1, fp2)
	}
}

func TestFingerprint_UniquePerCert(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, fp1, err := AutoCert(filepath.Join(dir, "a.pem"), filepath.Join(dir, "a.key"), "mesh-a")
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, err := AutoCert(filepath.Join(dir, "b.pem"), filepath.Join(dir, "b.key"), "mesh-b")
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp2 {
		t.Error("different certs produced the same fingerprint")
	}
}

func TestServerTLS_MinVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, _, err := AutoCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := ServerTLS(cert)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2 (%d)", cfg.MinVersion, tls.VersionTLS12)
	}
}

func TestClientTLS_NoFingerprint_Connects(t *testing.T) {
	t.Parallel()
	cert, _, ln := mustServer(t)
	defer ln.Close()

	go acceptOne(ln)

	conn, err := tls.Dial("tcp", ln.Addr().String(), ClientTLS(""))
	if err != nil {
		t.Fatalf("ClientTLS(\"\") should connect without fingerprint: %v", err)
	}
	_ = conn.Close()
	_ = cert
}

func TestClientTLS_CorrectFingerprint_Connects(t *testing.T) {
	t.Parallel()
	_, fp, ln := mustServer(t)
	defer ln.Close()

	go acceptOne(ln)

	conn, err := tls.Dial("tcp", ln.Addr().String(), ClientTLS(fp))
	if err != nil {
		t.Fatalf("correct fingerprint should connect: %v", err)
	}
	_ = conn.Close()
}

func TestClientTLS_WrongFingerprint_Rejected(t *testing.T) {
	t.Parallel()
	_, _, ln := mustServer(t)
	defer ln.Close()

	go acceptOne(ln)

	wrongFP := "sha256:" + strings.Repeat("00", 32)
	_, err := tls.Dial("tcp", ln.Addr().String(), ClientTLS(wrongFP))
	if err == nil {
		t.Fatal("wrong fingerprint should be rejected")
	}
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Errorf("error should be ErrFingerprintMismatch, got: %v", err)
	}
}

func TestClientTLS_NoPeerCert_Rejected(t *testing.T) {
	t.Parallel()
	cfg := ClientTLS("sha256:" + strings.Repeat("aa", 32))
	err := cfg.VerifyPeerCertificate(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("expected 'no certificate' error, got: %v", err)
	}
}

// TestWritePEM_MkdirFails pins the error path when the parent directory
// cannot be created (a path component exists as a regular file).
func TestWritePEM_MkdirFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	// writePEM tries to MkdirAll(dir/blocker/sub), which fails because
	// "blocker" already exists as a regular file.
	target := filepath.Join(blocker, "sub", "cert.pem")
	err := writePEM(target, "CERTIFICATE", []byte("ignored"), 0600)
	if err == nil {
		t.Fatal("expected error when parent path component is a regular file")
	}
}

// TestWritePEM_OpenFails pins the error path when MkdirAll succeeds but
// OpenFile cannot open the target (target path is a directory).
func TestWritePEM_OpenFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Target is a directory, not a file — OpenFile with O_WRONLY fails.
	err := writePEM(dir, "CERTIFICATE", []byte("ignored"), 0600)
	if err == nil {
		t.Fatal("expected error when target path is a directory")
	}
}

// TestGenerateCustom_WritePathUnwritable pins the generateCustom "write
// cert" failure branch by routing the cert path through a regular-file
// path component so MkdirAll fails inside writePEM.
func TestGenerateCustom_WritePathUnwritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(blocker, "sub", "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	_, _, err := generateCustom(certPath, keyPath, "mesh-test", time.Hour)
	if err == nil {
		t.Fatal("expected generateCustom to fail when cert path is unwritable")
	}
	if !strings.Contains(err.Error(), "write cert") {
		t.Errorf("error %q should mention write cert", err.Error())
	}
}

// --- helpers ---

func mustServer(t *testing.T) (tls.Certificate, string, net.Listener) {
	t.Helper()
	dir := t.TempDir()
	cert, fp, err := AutoCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), "mesh-test")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLS(cert))
	if err != nil {
		t.Fatal(err)
	}
	return cert, fp, ln
}

func acceptOne(ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	// Complete the TLS handshake before closing so the client receives the cert.
	if tc, ok := conn.(*tls.Conn); ok {
		_ = tc.Handshake()
	}
	_ = conn.Close()
}

// generateWithValidity writes a cert with custom validity for near-expiry tests.
// Same package so it calls generateCustom directly.
func generateWithValidity(certPath, keyPath, cn string, validity time.Duration) (tls.Certificate, string, error) {
	return generateCustom(certPath, keyPath, cn, validity)
}
