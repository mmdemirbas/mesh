//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ArtifactDir is the directory where Dump writes failure artefacts. It is
// overridable via MESH_E2E_ARTIFACTS; the default is ./e2e/build/artifacts.
func ArtifactDir() string {
	if v := os.Getenv("MESH_E2E_ARTIFACTS"); v != "" {
		return v
	}
	return filepath.Join("build", "artifacts")
}

// Dump writes diagnostic artefacts for a single node into a per-test
// subdirectory. Scenarios call this via DumpOnFailure so the files only
// appear when a test actually fails.
//
// Files written:
//
//	<alias>.docker.log   full docker log stream
//	<alias>.mesh.log     mesh's own log file from /root/.mesh/log/<alias>.log
//	<alias>.state.json   response body of GET /api/state
//	<alias>.metrics.txt  response body of GET /api/metrics
//
// Any file that cannot be collected is replaced by an error note so the
// dump is always best-effort and never fatal.
func Dump(ctx context.Context, t testing.TB, node *Node, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("e2e: create artifact dir %s: %v", dir, err)
		return
	}

	base := filepath.Join(dir, node.Alias)

	if logs, err := node.Logs(ctx); err == nil {
		_ = os.WriteFile(base+".docker.log", logs, 0o644)
	} else {
		_ = os.WriteFile(base+".docker.log", []byte(fmt.Sprintf("logs error: %v\n", err)), 0o644)
	}

	meshLog := fmt.Sprintf("/root/.mesh/log/%s.log", node.Alias)
	if content, err := node.ReadFile(ctx, meshLog); err == nil {
		_ = os.WriteFile(base+".mesh.log", content, 0o644)
	} else {
		_ = os.WriteFile(base+".mesh.log", []byte(fmt.Sprintf("read %s: %v\n", meshLog, err)), 0o644)
	}

	if body, err := node.AdminGET(ctx, "/api/state"); err == nil {
		_ = os.WriteFile(base+".state.json", []byte(body), 0o644)
	} else {
		_ = os.WriteFile(base+".state.json", []byte(fmt.Sprintf("admin state: %v\n", err)), 0o644)
	}

	if body, err := node.AdminMetrics(ctx); err == nil {
		_ = os.WriteFile(base+".metrics.txt", []byte(body), 0o644)
	} else {
		_ = os.WriteFile(base+".metrics.txt", []byte(fmt.Sprintf("admin metrics: %v\n", err)), 0o644)
	}
}

// DumpOnFailure registers a t.Cleanup that writes artefacts only if the
// test failed. Scenarios should call this immediately after starting each
// node so failures at any later point produce a complete dump.
func DumpOnFailure(t testing.TB, nodes ...*Node) {
	t.Helper()
	// Capture dir now so renames at teardown time still land next to the
	// test run.
	dir := filepath.Join(ArtifactDir(), t.Name(), time.Now().Format("20060102-150405"))
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, n := range nodes {
			Dump(ctx, t, n, dir)
		}
		t.Logf("e2e: artefacts written to %s", dir)
	})
}
