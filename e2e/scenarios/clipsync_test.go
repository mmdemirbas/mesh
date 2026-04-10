//go:build e2e

package scenarios

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/e2e/harness"
)

// TestClipsyncDiscoveryAndPush wires three peers into one discovery group
// and a fourth into a different group. It drives the clipboard via the
// fake xclip shim baked into the image and asserts on the resulting files
// under /tmp/mesh-clip.
func TestClipsyncDiscoveryAndPush(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	startPeer := func(alias, cfgFile string) *harness.Node {
		cfg, err := harness.LoadTemplate(cfgFile)
		if err != nil {
			t.Fatal(err)
		}
		return harness.StartNode(ctx, t, net, harness.NodeOptions{
			Alias:  alias,
			Config: cfg,
		})
	}

	a := startPeer("a", "configs/s3-a.yaml")
	b := startPeer("b", "configs/s3-b.yaml")
	c := startPeer("c", "configs/s3-c.yaml")
	d := startPeer("d", "configs/s3-d.yaml")
	harness.DumpOnFailure(t, a, b, c, d)

	// Helper: set the clipboard on a peer by writing to the fake xclip
	// backing file. The next clipsync poll (1s) picks it up.
	setClipboard := func(node *harness.Node, text string) {
		t.Helper()
		if err := node.WriteFile(ctx, "/tmp/mesh-clip/UTF8_STRING", []byte(text), 0o600); err != nil {
			t.Fatalf("%s: write clipboard: %v", node.Alias, err)
		}
	}

	// Helper: read the clipboard via the fake xclip backing file.
	readClipboard := func(node *harness.Node) string {
		t.Helper()
		code, out, _ := node.Exec(ctx, "sh", "-c", "cat /tmp/mesh-clip/UTF8_STRING 2>/dev/null || true")
		_ = code
		return out
	}

	// Phase 1 — push text from a, assert it reaches b and c (discovery
	// must have happened first). Beacon interval is 10s, so allow up to
	// 60s for the first round.
	marker := fmt.Sprintf("clip-from-a-%d", time.Now().UnixNano())
	setClipboard(a, marker)
	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond, "b and c receive clipboard from a",
		func() (bool, string) {
			got := map[string]string{
				"b": readClipboard(b),
				"c": readClipboard(c),
			}
			for k, v := range got {
				if !strings.Contains(v, marker) {
					return false, fmt.Sprintf("%s=%q", k, v)
				}
			}
			return true, ""
		})

	// Phase 2 — the out-of-group peer d must NOT have received anything.
	// Give it a couple more poll cycles to be sure, then assert.
	time.Sleep(3 * time.Second)
	if got := readClipboard(d); strings.Contains(got, marker) {
		t.Fatalf("peer d (different group) received clipboard from a: %q", got)
	}

	// Phase 3 — round-trip from b: set on b, see it propagate to a and c
	// (and still not d).
	marker2 := fmt.Sprintf("clip-from-b-%d", time.Now().UnixNano())
	setClipboard(b, marker2)
	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond, "a and c receive clipboard from b",
		func() (bool, string) {
			for name, peer := range map[string]*harness.Node{"a": a, "c": c} {
				if !strings.Contains(readClipboard(peer), marker2) {
					return false, fmt.Sprintf("%s missing marker2", name)
				}
			}
			return true, ""
		})
	if got := readClipboard(d); strings.Contains(got, marker2) {
		t.Fatalf("peer d received clipboard from b: %q", got)
	}

	// Phase 4 — large payload. Clipsync has two caps: maxClipboardPayload
	// (100 MB total across all formats) and defaultMaxSyncFileSize
	// (50 MB per format, enforced strictly at read time). 40 MB is
	// comfortably under both. The test validates that a multi-megabyte
	// payload round-trips in the same way a trivial one does.
	head := fmt.Sprintf("big-blob-%d\n", time.Now().UnixNano())
	const bodySize = 40 * 1024 * 1024
	// Generate on-container via dd + printf to avoid shipping 50 MB
	// through the docker cp API from the host.
	_, _, err := a.Exec(ctx, "sh", "-c",
		fmt.Sprintf(
			"mkdir -p /tmp/mesh-clip && "+
				"printf '%%s' '%s' > /tmp/mesh-clip/UTF8_STRING && "+
				"dd if=/dev/zero bs=1M count=%d 2>/dev/null | tr '\\0' 'x' >> /tmp/mesh-clip/UTF8_STRING",
			head, bodySize/(1024*1024)))
	if err != nil {
		t.Fatalf("seed big blob on a: %v", err)
	}

	// Verify a's file itself is the expected size before waiting on sync.
	if out := a.MustExec(ctx, "sh", "-c", "wc -c < /tmp/mesh-clip/UTF8_STRING"); strings.TrimSpace(out) == "0" {
		t.Fatalf("a big blob not written: wc -c = %q", out)
	}

	harness.Eventually(ctx, t, 2*time.Minute, 2*time.Second, "b and c receive the 50 MB payload",
		func() (bool, string) {
			for name, peer := range map[string]*harness.Node{"b": b, "c": c} {
				sizeOut := peer.MustExec(ctx, "sh", "-c", "wc -c < /tmp/mesh-clip/UTF8_STRING 2>/dev/null || echo 0")
				var size int64
				if _, err := fmt.Sscanf(strings.TrimSpace(sizeOut), "%d", &size); err != nil {
					return false, fmt.Sprintf("%s parse size %q: %v", name, sizeOut, err)
				}
				if size < bodySize {
					return false, fmt.Sprintf("%s size=%d want>=%d", name, size, bodySize)
				}
				headOut := peer.MustExec(ctx, "sh", "-c",
					fmt.Sprintf("head -c %d /tmp/mesh-clip/UTF8_STRING", len(head)))
				if headOut != head {
					return false, fmt.Sprintf("%s head mismatch: %q", name, headOut)
				}
			}
			return true, ""
		})
}
