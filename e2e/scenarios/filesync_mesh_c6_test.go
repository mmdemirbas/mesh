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

// wrapEntrypointMulti resolves multiple peer hostnames to IPs at container
// start and rewrites matching placeholders in the config before execing mesh.
// The filesync peer validator is IP-based; docker DNS aliases alone do not
// match the bridge IP of the incoming request.
func wrapEntrypointMulti(node string, peers []struct{ hostname, placeholder string }) []string {
	var sb strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&sb,
			"i=0; IP=''; while [ $i -lt 60 ]; do "+
				"IP=$(getent hosts %[1]s 2>/dev/null | awk '{print $1}'); "+
				"if [ -n \"$IP\" ]; then break; fi; "+
				"i=$((i+1)); sleep 0.5; "+
				"done; "+
				"if [ -z \"$IP\" ]; then echo 'resolve %[1]s failed' >&2; exit 1; fi; "+
				"sed -i \"s/%[2]s/$IP/g\" /root/.mesh/conf/mesh.yaml; ",
			p.hostname, p.placeholder)
	}
	fmt.Fprintf(&sb, "exec /usr/local/bin/mesh -f /root/.mesh/conf/mesh.yaml up %s", node)
	return []string{"/bin/sh", "-c", sb.String()}
}

// TestFilesyncMeshC6 drives a three-node mesh (peer1, peer2, peer3 all
// send-receive to each other) through the C6 vector-clock cases that
// DESIGN-v1 §Test strategy requires pinned at e2e:
//
//  1. Sequential dominates / dominated edits converge with no conflict.
//  2. Concurrent writes across two peers produce a .sync-conflict-* file.
//  3. A deletion tombstone that reaches all peers can be resurrected by a
//     later write whose vector dominates; the file reappears on every peer.
//
// Rename via prev_path is already covered by TestFilesyncRenameWithEdit in a
// two-peer setup and is not duplicated here.
func TestFilesyncMeshC6(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	type peerRef = struct{ hostname, placeholder string }

	cfgs := map[string]string{
		"peer1": "configs/s5-peer1.yaml",
		"peer2": "configs/s5-peer2.yaml",
		"peer3": "configs/s5-peer3.yaml",
	}
	loaded := map[string]string{}
	for alias, file := range cfgs {
		b, err := harness.LoadTemplate(file)
		if err != nil {
			t.Fatalf("load %s: %v", file, err)
		}
		loaded[alias] = b
	}

	noopWait := wait.ForExec([]string{"true"}).WithStartupTimeout(10 * time.Second)
	seed := []harness.File{{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}}

	peer1 := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "peer1",
		Config:     loaded["peer1"],
		Entrypoint: []string{"/bin/sh"},
		Cmd: wrapEntrypointMulti("peer1", []peerRef{
			{"peer2", "PEER2_IP"},
			{"peer3", "PEER3_IP"},
		})[1:],
		WaitFor: noopWait,
		Files:   seed,
	})
	peer2 := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "peer2",
		Config:     loaded["peer2"],
		Entrypoint: []string{"/bin/sh"},
		Cmd: wrapEntrypointMulti("peer2", []peerRef{
			{"peer1", "PEER1_IP"},
			{"peer3", "PEER3_IP"},
		})[1:],
		WaitFor: noopWait,
		Files:   seed,
	})
	peer3 := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      "peer3",
		Config:     loaded["peer3"],
		Entrypoint: []string{"/bin/sh"},
		Cmd: wrapEntrypointMulti("peer3", []peerRef{
			{"peer1", "PEER1_IP"},
			{"peer2", "PEER2_IP"},
		})[1:],
		Files: seed,
	})
	harness.DumpOnFailure(t, peer1, peer2, peer3)

	all := []*harness.Node{peer1, peer2, peer3}

	// All three peers' getent loops must resolve before any admin API is
	// reachable. Poll each in turn — testcontainers already waited for
	// peer3's admin API, so peer1 and peer2 are the ones to watch.
	for _, p := range all {
		p := p
		harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
			fmt.Sprintf("%s admin responding", p.Alias),
			func() (bool, string) {
				if _, err := p.AdminGET(ctx, "/api/state"); err != nil {
					return false, err.Error()
				}
				return true, ""
			})
	}

	for _, p := range all {
		p := p
		harness.Eventually(ctx, t, 20*time.Second, 250*time.Millisecond,
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

	hasContent := func(p *harness.Node, path, want string) (bool, string) {
		code, out, err := p.Exec(ctx, "cat", path)
		if err != nil || code != 0 {
			return false, fmt.Sprintf("cat err=%v code=%d", err, code)
		}
		if strings.Contains(out, want) {
			return true, ""
		}
		return false, fmt.Sprintf("content=%q", strings.TrimSpace(out))
	}

	fileExists := func(p *harness.Node, path string) (bool, string) {
		code, _, err := p.Exec(ctx, "test", "-f", path)
		if err != nil {
			return false, err.Error()
		}
		return code == 0, "absent"
	}

	fileAbsent := func(p *harness.Node, path string) (bool, string) {
		code, _, err := p.Exec(ctx, "test", "-f", path)
		if err != nil {
			return false, err.Error()
		}
		return code != 0, "still present"
	}

	// Phase 1 — sequential edits: peer1 creates, peer2 edits, peer3 edits.
	// Each edit vector dominates the prior; no conflict file may appear on
	// any peer.
	const shared = "/root/sync/phase1.txt"

	if err := peer1.WriteFile(ctx, shared, []byte("v1 from peer1\n"), 0o600); err != nil {
		t.Fatalf("seed peer1: %v", err)
	}
	for _, p := range []*harness.Node{peer2, peer3} {
		p := p
		harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
			fmt.Sprintf("v1 reaches %s", p.Alias),
			func() (bool, string) { return hasContent(p, shared, "v1 from peer1") })
	}

	if err := peer2.WriteFile(ctx, shared, []byte("v2 from peer2\n"), 0o600); err != nil {
		t.Fatalf("edit peer2: %v", err)
	}
	for _, p := range []*harness.Node{peer1, peer3} {
		p := p
		harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond,
			fmt.Sprintf("v2 reaches %s", p.Alias),
			func() (bool, string) { return hasContent(p, shared, "v2 from peer2") })
	}

	if err := peer3.WriteFile(ctx, shared, []byte("v3 from peer3\n"), 0o600); err != nil {
		t.Fatalf("edit peer3: %v", err)
	}
	for _, p := range []*harness.Node{peer1, peer2} {
		p := p
		harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond,
			fmt.Sprintf("v3 reaches %s", p.Alias),
			func() (bool, string) { return hasContent(p, shared, "v3 from peer3") })
	}
	// Dominates path must not produce a conflict file anywhere.
	for _, p := range all {
		out := p.MustExec(ctx, "sh", "-c", "find /root/sync -name '*.sync-conflict-*' -type f | wc -l")
		if strings.TrimSpace(out) != "0" {
			t.Fatalf("%s: unexpected conflict file after sequential edits: %s",
				p.Alias, strings.TrimSpace(out))
		}
	}

	// Phase 2 — concurrent: stop peer3, write on peer1. Start peer3 and
	// write on peer3 before its sync catches up. The two vectors are
	// concurrent and must produce a .sync-conflict-* file on some peer.
	const concurrent = "/root/sync/phase2.txt"

	if err := peer1.WriteFile(ctx, concurrent, []byte("baseline\n"), 0o600); err != nil {
		t.Fatalf("baseline on peer1: %v", err)
	}
	for _, p := range []*harness.Node{peer2, peer3} {
		p := p
		harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
			fmt.Sprintf("baseline reaches %s", p.Alias),
			func() (bool, string) { return hasContent(p, concurrent, "baseline") })
	}

	if err := peer3.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("stop peer3: %v", err)
	}
	if err := peer1.WriteFile(ctx, concurrent, []byte("peer1 concurrent\n"), 0o600); err != nil {
		t.Fatalf("peer1 concurrent write: %v", err)
	}
	startCtx2, startCancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel2()
	if err := peer3.Start(startCtx2); err != nil {
		t.Fatalf("start peer3: %v", err)
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond,
		"peer3 admin responding after restart",
		func() (bool, string) {
			if _, err := peer3.AdminGET(ctx, "/api/state"); err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	if _, _, err := peer3.Exec(ctx, "sh", "-c",
		"echo 'peer3 concurrent' > "+concurrent); err != nil {
		t.Fatalf("peer3 concurrent write: %v", err)
	}

	harness.Eventually(ctx, t, 75*time.Second, 1*time.Second,
		".sync-conflict-* appears on some peer",
		func() (bool, string) {
			for _, p := range all {
				out := p.MustExec(ctx, "sh", "-c",
					"find /root/sync -name '*.sync-conflict-*' -type f | head -1")
				if strings.TrimSpace(out) != "" {
					return true, ""
				}
			}
			return false, "no conflict file found"
		})

	// Phase 3 — tombstone and resurrection: delete on peer1, wait for the
	// tombstone to reach peer2 and peer3, then create the same path on
	// peer2. peer2's adopted vector dominates the tombstone so the new
	// entry must propagate to peer1 and peer3.
	const ghost = "/root/sync/phase3.txt"

	if err := peer1.WriteFile(ctx, ghost, []byte("will be deleted\n"), 0o600); err != nil {
		t.Fatalf("seed ghost on peer1: %v", err)
	}
	for _, p := range []*harness.Node{peer2, peer3} {
		p := p
		harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
			fmt.Sprintf("ghost reaches %s", p.Alias),
			func() (bool, string) { return fileExists(p, ghost) })
	}

	if _, _, err := peer1.Exec(ctx, "rm", ghost); err != nil {
		t.Fatalf("delete ghost on peer1: %v", err)
	}
	for _, p := range []*harness.Node{peer2, peer3} {
		p := p
		harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond,
			fmt.Sprintf("tombstone reaches %s", p.Alias),
			func() (bool, string) { return fileAbsent(p, ghost) })
	}

	if err := peer2.WriteFile(ctx, ghost, []byte("resurrected on peer2\n"), 0o600); err != nil {
		t.Fatalf("resurrect on peer2: %v", err)
	}
	for _, p := range []*harness.Node{peer1, peer3} {
		p := p
		harness.Eventually(ctx, t, 75*time.Second, 500*time.Millisecond,
			fmt.Sprintf("resurrection reaches %s", p.Alias),
			func() (bool, string) {
				return hasContent(p, ghost, "resurrected on peer2")
			})
	}
}
