package tlsutil

import (
	"crypto/tls"
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
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Errorf("error should mention fingerprint mismatch, got: %v", err)
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
