//go:build e2e

package harness

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireImage skips the test when the mesh-e2e:local image is not present.
// Scenarios expect `task build:e2e-image` to have been run beforehand; this
// keeps the failure mode friendly for anyone running `go test -tags e2e`
// directly.
func requireImage(t testing.TB, image string) {
	t.Helper()
	cmd := exec.Command("docker", "image", "inspect", image)
	if err := cmd.Run(); err != nil {
		t.Skipf("e2e: image %s not present, run `task build:e2e-image` first", image)
	}
}

// TestHarnessSmoke boots a single mesh node with a minimal config and
// exercises every harness primitive: network, node lifecycle, exec, file
// I/O, admin GET, and the JSON + metrics helpers. It is the basic contract
// every scenario relies on.
func TestHarnessSmoke(t *testing.T) {
	requireImage(t, DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	net := NewNetwork(ctx, t)

	const cfg = `smoke:
  log_level: info
  admin_addr: "127.0.0.1:7777"
`
	node := StartNode(ctx, t, net, NodeOptions{
		Alias:  "smoke",
		Config: cfg,
	})
	DumpOnFailure(t, node)

	// Exec a trivial command and verify we can read the output.
	if out := node.MustExec(ctx, "sh", "-c", "echo hello"); strings.TrimSpace(out) != "hello" {
		t.Fatalf("exec echo: got %q want %q", out, "hello")
	}

	// Write a file and read it back.
	if err := node.WriteFile(ctx, "/tmp/harness-test", []byte("mesh"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	got, err := node.ReadFile(ctx, "/tmp/harness-test")
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "mesh" {
		t.Fatalf("read file: got %q want %q", string(got), "mesh")
	}

	// Admin state: should decode and contain an entry (the admin server
	// self-registers as a component) or at worst an empty map.
	snap, err := node.AdminState(ctx)
	if err != nil {
		t.Fatalf("admin state: %v", err)
	}
	if snap.Components == nil {
		t.Fatal("admin state: components map is nil")
	}

	// Admin metrics: MetricValue should find at least mesh_process_goroutines.
	body, err := node.AdminMetrics(ctx)
	if err != nil {
		t.Fatalf("admin metrics: %v", err)
	}
	if v, ok := MetricValue(body, "mesh_process_goroutines", ""); !ok || v <= 0 {
		t.Fatalf("mesh_process_goroutines missing or zero: v=%v ok=%v\nbody:\n%s", v, ok, body)
	}

	// Logs: we should get at least the "mesh starting" line from stdout.
	logs, err := node.Logs(ctx)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(string(logs), "mesh starting") {
		t.Fatalf("expected 'mesh starting' in logs, got:\n%s", string(logs))
	}
}
