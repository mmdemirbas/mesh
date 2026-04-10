//go:build e2e

package harness

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

// KeyPair holds an ed25519 SSH key in OpenSSH formats: PrivatePEM is the
// serialized private key suitable for writing to a mesh auth.key file and
// AuthorizedLine is the single-line public key suitable for authorized_keys
// or known_hosts fixtures.
type KeyPair struct {
	PrivatePEM     []byte
	AuthorizedLine []byte
}

// GenerateKeyPair creates a fresh ed25519 SSH key pair. The comment is
// embedded in the OpenSSH private key header and is purely cosmetic.
func GenerateKeyPair(t testing.TB, comment string) KeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("e2e: ed25519 keygen: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		t.Fatalf("e2e: marshal private key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("e2e: new public key: %v", err)
	}
	return KeyPair{
		PrivatePEM:     pem.EncodeToMemory(block),
		AuthorizedLine: ssh.MarshalAuthorizedKey(sshPub),
	}
}
