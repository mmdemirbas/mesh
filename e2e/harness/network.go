//go:build e2e

package harness

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

// Network is a per-test docker bridge network. Each scenario creates its own
// so aliases ("client", "server", "bastion", "stub") stay scoped and UDP
// broadcast between containers — which clipsync discovery needs — works
// without cross-test contamination.
type Network struct {
	Name string
	raw  *testcontainers.DockerNetwork
}

// NewNetwork creates an isolated bridge network for the test and registers
// removal with t.Cleanup.
func NewNetwork(ctx context.Context, t testing.TB) *Network {
	t.Helper()
	n, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("e2e: create network: %v", err)
	}
	t.Cleanup(func() {
		// Use a detached context so cleanup still runs when the test
		// context has already been cancelled.
		_ = n.Remove(context.Background())
	})
	return &Network{Name: n.Name, raw: n}
}
