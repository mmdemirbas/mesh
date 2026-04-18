//go:build e2e

package scenarios

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/e2e/harness"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestFilesyncTwoPeer stands up two mesh nodes with a bidirectional
// send-receive folder and walks the core filesync matrix: initial
// propagation, reverse edit, delete, conflict detection, and convergence
// after a peer restart.
func TestFilesyncTwoPeer(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	peer1Cfg, err := harness.LoadTemplate("configs/s2-peer1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	peer2Cfg, err := harness.LoadTemplate("configs/s2-peer2.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// Mesh's filesync peer validation is IP-based (no DNS resolution), so
	// the container hostnames "peer1"/"peer2" in the configured peer list
	// never match the docker-assigned bridge IP of the incoming request.
	// Use a sh wrapper that resolves the peer hostname with getent at
	// startup, rewrites the config placeholder, then execs mesh.
	wrap := func(node, peer, placeholder string) []string {
		// peer1 starts before peer2 exists in docker's embedded DNS, so
		// the first container has to poll getent until the other peer
		// registers. 60 * 0.5s gives a 30s ceiling which comfortably
		// covers testcontainers' per-container startup time.
		script := fmt.Sprintf(
			"i=0; while [ $i -lt 60 ]; do "+
				"IP=$(getent hosts %[1]s 2>/dev/null | awk '{print $1}'); "+
				"if [ -n \"$IP\" ]; then break; fi; "+
				"i=$((i+1)); sleep 0.5; "+
				"done; "+
				"if [ -z \"$IP\" ]; then echo 'resolve %[1]s failed' >&2; exit 1; fi; "+
				"sed -i \"s/%[2]s/$IP/g\" /root/.mesh/conf/mesh.yaml && "+
				"exec /usr/local/bin/mesh -f /root/.mesh/conf/mesh.yaml up %[3]s",
			peer, placeholder, node)
		return []string{"/bin/sh", "-c", script}
	}

	// peer1 must start with a no-op wait strategy because its sh wrapper
	// blocks on `getent peer2` until peer2 exists. Once peer2 is up we
	// wait on peer1's admin API manually below.
	noopWait := wait.ForExec([]string{"true"}).WithStartupTimeout(10 * time.Second)

	peer1 := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "peer1",
		Config:     peer1Cfg,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrap("peer1", "peer2", "PEER2_IP")[1:],
		WaitFor:    noopWait,
		Files: []harness.File{
			{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600},
		},
	})
	peer2 := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "peer2",
		Config:     peer2Cfg,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrap("peer2", "peer1", "PEER1_IP")[1:],
		Files: []harness.File{
			{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600},
		},
	})
	// Now that peer2 is up (its admin API passed), peer1's getent loop
	// will resolve and mesh will start. Wait for peer1's admin API too.
	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond, "peer1 admin responding",
		func() (bool, string) {
			if _, err := peer1.AdminGET(ctx, "/api/state"); err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	harness.DumpOnFailure(t, peer1, peer2)

	// Wait for filesync to register the folder on both peers.
	waitFolder := func(p *harness.Node) {
		harness.Eventually(ctx, t, 15*time.Second, 250*time.Millisecond,
			fmt.Sprintf("%s: folder 'shared' registered", p.Alias),
			func() (bool, string) {
				var folders []struct {
					ID   string `json:"id"`
					Path string `json:"path"`
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
	waitFolder(peer1)
	waitFolder(peer2)

	// Phase 1 — initial propagation: 10 files on peer1 appear on peer2.
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("/root/sync/file-%02d.txt", i)
		content := []byte(fmt.Sprintf("initial content %d\n", i))
		if err := peer1.WriteFile(ctx, path, content, 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "10 files on peer2",
		func() (bool, string) {
			out := peer2.MustExec(ctx, "sh", "-c", "ls /root/sync/file-*.txt 2>/dev/null | wc -l")
			count := strings.TrimSpace(out)
			if count == "10" {
				return true, ""
			}
			return false, fmt.Sprintf("peer2 file count=%s", count)
		})

	// Phase 2 — reverse edit: a change on peer2 reaches peer1 and leaves
	// no leftover temp files.
	if err := peer2.WriteFile(ctx, "/root/sync/file-00.txt", []byte("edited on peer2\n"), 0o600); err != nil {
		t.Fatalf("edit on peer2: %v", err)
	}
	// peer1 must initiate its own sync to discover peer2's edit; its
	// scanTrigger won't fire (no local changes), so it waits for the 30s
	// ticker. Budget two cycles to avoid racing with the tick boundary.
	harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond, "peer1 sees peer2 edit",
		func() (bool, string) {
			code, out, err := peer1.Exec(ctx, "cat", "/root/sync/file-00.txt")
			if err != nil || code != 0 {
				return false, fmt.Sprintf("cat err=%v code=%d", err, code)
			}
			if strings.Contains(out, "edited on peer2") {
				return true, ""
			}
			return false, fmt.Sprintf("content=%q", out)
		})
	// No leftover temp files on either peer.
	for _, p := range []*harness.Node{peer1, peer2} {
		out := p.MustExec(ctx, "sh", "-c", "find /root/sync -name '.mesh-tmp-*' -type f | wc -l")
		if strings.TrimSpace(out) != "0" {
			t.Fatalf("%s: leftover .mesh-tmp-* files: %s", p.Alias, out)
		}
	}

	// Phase 3 — delete propagation.
	if _, _, err := peer1.Exec(ctx, "rm", "/root/sync/file-09.txt"); err != nil {
		t.Fatalf("delete on peer1: %v", err)
	}
	// Same as Phase 2: peer2 must discover the tombstone via its own sync
	// cycle (peer1's exchange doesn't push actions to peer2).
	harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond, "delete reaches peer2",
		func() (bool, string) {
			code, _, err := peer2.Exec(ctx, "test", "-f", "/root/sync/file-09.txt")
			if err != nil {
				return false, err.Error()
			}
			if code != 0 {
				return true, ""
			}
			return false, "still present"
		})

	// Phase 4 — conflict detection: stop peer2, edit same file on both
	// sides, start peer2, expect a .sync-conflict-* file to appear on
	// at least one peer.
	if err := peer2.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("stop peer2: %v", err)
	}
	if err := peer1.WriteFile(ctx, "/root/sync/file-01.txt", []byte("peer1 wins\n"), 0o600); err != nil {
		t.Fatalf("peer1 conflict write: %v", err)
	}
	// Bound the restart so a slow Docker Desktop doesn't consume the
	// entire test budget.
	startCtx4, startCancel4 := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel4()
	if err := peer2.Start(startCtx4); err != nil {
		t.Fatalf("start peer2: %v", err)
	}
	// Wait for peer2 admin API to come back before driving it.
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "peer2 admin responding",
		func() (bool, string) {
			_, err := peer2.AdminGET(ctx, "/api/state")
			if err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	// Peer2's version of file-01.txt is still the pre-conflict content,
	// because it was offline. Edit it before the sync pulls peer1's copy.
	if _, _, err := peer2.Exec(ctx, "sh", "-c", "echo 'peer2 wins' > /root/sync/file-01.txt"); err != nil {
		t.Fatalf("peer2 conflict write: %v", err)
	}
	harness.Eventually(ctx, t, 60*time.Second, 1*time.Second, ".sync-conflict-* appears on some peer",
		func() (bool, string) {
			for _, p := range []*harness.Node{peer1, peer2} {
				out := p.MustExec(ctx, "sh", "-c", "find /root/sync -name '*.sync-conflict-*' -type f | head -1")
				if strings.TrimSpace(out) != "" {
					return true, ""
				}
			}
			return false, "no conflict file found"
		})

	// Phase 5 — restart convergence: peer2 restarts and a new file on
	// peer1 still propagates without manual intervention.
	if err := peer2.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("stop peer2 (phase 5): %v", err)
	}
	if err := peer1.WriteFile(ctx, "/root/sync/late.txt", []byte("after peer2 down\n"), 0o600); err != nil {
		t.Fatalf("late write on peer1: %v", err)
	}
	startCtx5, startCancel5 := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel5()
	if err := peer2.Start(startCtx5); err != nil {
		t.Fatalf("start peer2 (phase 5): %v", err)
	}
	harness.Eventually(ctx, t, 60*time.Second, 1*time.Second, "late file reaches peer2 after restart",
		func() (bool, string) {
			code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/late.txt")
			if code == 0 {
				return true, ""
			}
			return false, "not yet"
		})
}
