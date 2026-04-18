// Package tlsutil provides auto-generated TLS certificates and configs
// for mesh's internal HTTP servers (filesync, clipsync).
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	certValidity   = 10 * 365 * 24 * time.Hour // 10 years
	renewBefore    = 30 * 24 * time.Hour       // regenerate within 30 days of expiry
	fingerprintPfx = "sha256:"
)

// AutoCert loads the cert+key at certPath/keyPath. If either file is missing,
// unreadable, or the cert expires within 30 days, a new ECDSA P-256
// self-signed cert is generated and written to those paths (0600 key, 0644 cert).
// Returns the certificate and its fingerprint ("sha256:<hex>").
func AutoCert(certPath, keyPath, cn string) (tls.Certificate, string, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		leaf, parseErr := x509.ParseCertificate(cert.Certificate[0])
		if parseErr == nil && time.Until(leaf.NotAfter) > renewBefore {
			return cert, Fingerprint(cert), nil
		}
	}
	return generate(certPath, keyPath, cn)
}

// Fingerprint returns "sha256:<hex>" of the leaf cert's DER encoding.
func Fingerprint(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return fingerprintPfx + hex.EncodeToString(sum[:])
}

// ServerTLS returns a *tls.Config for a TLS server using the given cert.
// Minimum version: TLS 1.2. Preferred curves: P-256, X25519.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.CurveP256,
			tls.X25519,
		},
	}
}

// ClientTLS returns a *tls.Config for a TLS client.
//
// When fingerprint is empty, the connection is encrypted but the peer cert is
// not verified beyond structural validity (InsecureSkipVerify). Combined with
// the IP allowlist in each component, this stops passive eavesdropping.
//
// When fingerprint is "sha256:<hex>", VerifyPeerCertificate checks that the
// server's leaf cert matches exactly. A mismatch returns an actionable error.
// The caller should surface this as "peer cert fingerprint mismatch".
func ClientTLS(fingerprint string) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verified via VerifyPeerCertificate when fingerprint is set
	}
	if fingerprint == "" {
		return cfg
	}
	want := fingerprint
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("peer presented no certificate")
		}
		got := fingerprintPfx + hex.EncodeToString(sha256sum(rawCerts[0]))
		if got != want {
			return fmt.Errorf("peer cert fingerprint mismatch: got %s, want %s", got, want)
		}
		return nil
	}
	return cfg
}

// generate creates a new ECDSA P-256 self-signed cert and writes it to disk.
func generate(certPath, keyPath, cn string) (tls.Certificate, string, error) {
	return generateCustom(certPath, keyPath, cn, certValidity)
}

// generateCustom is the internal implementation shared by generate and tests.
func generateCustom(certPath, keyPath, cn string, validity time.Duration) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute), // small back-date for clock skew
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("write cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0600); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("write key: %w", err)
	}

	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("load generated cert: %w", err)
	}
	return cert, Fingerprint(cert), nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
