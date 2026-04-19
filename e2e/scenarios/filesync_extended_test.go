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

// wrapEntrypoint returns a shell command slice that resolves a peer hostname
// to an IP, patches the config placeholder, and execs mesh. This is needed
// because filesync peer validation is IP-based; docker DNS aliases alone
// don't match.
func wrapEntrypoint(node, peer, placeholder string) []string {
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

// startFilesyncPair starts two filesync peers on a shared network. The first
// node (nodeA) starts with a noop wait strategy (since it blocks on DNS
// resolution of nodeB); after nodeB is ready, the caller must poll nodeA's
// admin API.
func startFilesyncPair(ctx context.Context, t *testing.T, net *harness.Network,
	aliasA, cfgFileA, peerPlaceholderA string,
	aliasB, cfgFileB, peerPlaceholderB string,
	filesA, filesB []harness.File,
) (nodeA, nodeB *harness.Node) {
	t.Helper()

	cfgA, err := harness.LoadTemplate(cfgFileA)
	if err != nil {
		t.Fatal(err)
	}
	cfgB, err := harness.LoadTemplate(cfgFileB)
	if err != nil {
		t.Fatal(err)
	}

	noopWait := wait.ForExec([]string{"true"}).WithStartupTimeout(10 * time.Second)

	nodeA = harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      aliasA,
		Config:     cfgA,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrapEntrypoint(aliasA, aliasB, peerPlaceholderA)[1:],
		WaitFor:    noopWait,
		Files:      filesA,
	})
	nodeB = harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:      aliasB,
		Config:     cfgB,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        wrapEntrypoint(aliasB, aliasA, peerPlaceholderB)[1:],
		Files:      filesB,
	})

	// nodeB is up; wait for nodeA's admin API (its getent loop resolved).
	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond,
		fmt.Sprintf("%s admin responding", aliasA),
		func() (bool, string) {
			if _, err := nodeA.AdminGET(ctx, "/api/state"); err != nil {
				return false, err.Error()
			}
			return true, ""
		})
	harness.DumpOnFailure(t, nodeA, nodeB)

	// Wait for folder registration on both peers.
	for _, p := range []*harness.Node{nodeA, nodeB} {
		harness.Eventually(ctx, t, 15*time.Second, 250*time.Millisecond,
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

	return nodeA, nodeB
}

// TestFilesyncSendOnly verifies that in send-only / receive-only mode:
//  1. Files created on the sender propagate to the receiver.
//  2. Files created on the receiver do NOT propagate back to the sender.
//  3. Deletes on the sender propagate to the receiver.
func TestFilesyncSendOnly(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	sender, receiver := startFilesyncPair(ctx, t, net,
		"sender", "configs/s2-sender.yaml", "RECEIVER_IP",
		"receiver", "configs/s2-receiver.yaml", "SENDER_IP",
		[]harness.File{keepFile}, []harness.File{keepFile},
	)

	// Phase 1 — sender → receiver propagation.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("/root/sync/doc-%d.txt", i)
		if err := sender.WriteFile(ctx, path, []byte(fmt.Sprintf("from sender %d\n", i)), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "3 files on receiver",
		func() (bool, string) {
			out := receiver.MustExec(ctx, "sh", "-c", "ls /root/sync/doc-*.txt 2>/dev/null | wc -l")
			if strings.TrimSpace(out) == "3" {
				return true, ""
			}
			return false, fmt.Sprintf("count=%s", strings.TrimSpace(out))
		})

	// Phase 2 — receiver-created file must NOT appear on sender.
	if err := receiver.WriteFile(ctx, "/root/sync/local-only.txt", []byte("should not sync\n"), 0o600); err != nil {
		t.Fatalf("write local-only: %v", err)
	}
	// Wait two full sync cycles (scan_interval=2s, sync interval=30s default,
	// but we only need to confirm it does NOT sync). Wait long enough that
	// if it were going to sync, it would have.
	// Write another file on sender to prove sync is working, then verify
	// the receiver file didn't leak.
	if err := sender.WriteFile(ctx, "/root/sync/probe.txt", []byte("probe\n"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "probe reaches receiver",
		func() (bool, string) {
			code, _, _ := receiver.Exec(ctx, "test", "-f", "/root/sync/probe.txt")
			if code == 0 {
				return true, ""
			}
			return false, "not yet"
		})
	// Now verify local-only.txt never reached sender.
	code, _, _ := sender.Exec(ctx, "test", "-f", "/root/sync/local-only.txt")
	if code == 0 {
		t.Fatal("receiver-created file leaked to sender in send-only mode")
	}

	// Phase 3 — delete on sender propagates to receiver.
	if _, _, err := sender.Exec(ctx, "rm", "/root/sync/doc-0.txt"); err != nil {
		t.Fatalf("delete on sender: %v", err)
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "delete reaches receiver",
		func() (bool, string) {
			code, _, _ := receiver.Exec(ctx, "test", "-f", "/root/sync/doc-0.txt")
			if code != 0 {
				return true, ""
			}
			return false, "still present"
		})
}

// TestFilesyncPermissions verifies that file permission bits (L1) are
// preserved across sync: an executable on peer1 retains its execute bit
// on peer2.
func TestFilesyncPermissions(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	peer1, peer2 := startFilesyncPair(ctx, t, net,
		"peer1", "configs/s2-peer1.yaml", "PEER2_IP",
		"peer2", "configs/s2-peer2.yaml", "PEER1_IP",
		[]harness.File{keepFile}, []harness.File{keepFile},
	)

	// Create an executable script on peer1.
	script := []byte("#!/bin/sh\necho hello\n")
	if err := peer1.WriteFile(ctx, "/root/sync/run.sh", script, 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Wait for it to appear on peer2.
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "run.sh on peer2",
		func() (bool, string) {
			code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/run.sh")
			if code == 0 {
				return true, ""
			}
			return false, "not yet"
		})

	// Verify the execute bit is set.
	code, out, err := peer2.Exec(ctx, "stat", "-c", "%a", "/root/sync/run.sh")
	if err != nil || code != 0 {
		t.Fatalf("stat: code=%d err=%v out=%s", code, err, out)
	}
	mode := strings.TrimSpace(out)
	if !strings.Contains(mode, "7") && !strings.Contains(mode, "5") {
		t.Fatalf("expected executable mode, got %s", mode)
	}
	// Also verify the script actually runs.
	code, out, err = peer2.Exec(ctx, "/root/sync/run.sh")
	if err != nil || code != 0 {
		t.Fatalf("execute script: code=%d err=%v out=%s", code, err, out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("unexpected script output: %q", out)
	}

	// Also create a read-only file and verify it stays non-executable.
	if err := peer1.WriteFile(ctx, "/root/sync/data.txt", []byte("read only\n"), 0o644); err != nil {
		t.Fatalf("write data.txt: %v", err)
	}
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "data.txt on peer2",
		func() (bool, string) {
			code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/data.txt")
			if code == 0 {
				return true, ""
			}
			return false, "not yet"
		})
	code, out, err = peer2.Exec(ctx, "stat", "-c", "%a", "/root/sync/data.txt")
	if err != nil || code != 0 {
		t.Fatalf("stat data.txt: code=%d err=%v", code, err)
	}
	dataMode := strings.TrimSpace(out)
	if dataMode != "644" {
		t.Fatalf("expected 644, got %s", dataMode)
	}
}

// TestFilesyncRenameWithEdit exercises the R1 Phase 2 hint path end-to-end:
// peer1 renames a file and edits a small portion of it in the same step;
// peer2 must end up with the new path holding the edited content, the old
// path gone, no leftover temp files, and a non-zero
// mesh_filesync_files_renamed_total on the receiver (proving the hint was
// applied locally instead of a blind re-download).
func TestFilesyncRenameWithEdit(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	peer1, peer2 := startFilesyncPair(ctx, t, net,
		"peer1", "configs/s2-peer1.yaml", "PEER2_IP",
		"peer2", "configs/s2-peer2.yaml", "PEER1_IP",
		[]harness.File{keepFile}, []harness.File{keepFile},
	)

	// Seed a file large enough to cross the incremental-hash threshold so
	// the receiver's /delta path is eligible. 2 MB of a repeating pattern
	// keeps block hashes deterministic while leaving room for a surgical
	// edit near the tail.
	const size = 2 * 1024 * 1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('A' + (i % 26))
	}
	if err := peer1.WriteFile(ctx, "/root/sync/report.bin", payload, 0o600); err != nil {
		t.Fatalf("seed report.bin: %v", err)
	}

	harness.Eventually(ctx, t, 60*time.Second, 500*time.Millisecond, "report.bin reaches peer2",
		func() (bool, string) {
			out := peer2.MustExec(ctx, "sh", "-c", "stat -c %s /root/sync/report.bin 2>/dev/null || echo 0")
			if strings.TrimSpace(out) == fmt.Sprintf("%d", size) {
				return true, ""
			}
			return false, fmt.Sprintf("size=%s, want %d", strings.TrimSpace(out), size)
		})

	// Rename + edit in one step: the peer1 scanner picks up the new path
	// with the old inode and tags the new entry with PrevPath so the sender
	// emits prev_path=old on the wire.
	peer1.MustExec(ctx, "sh", "-c", "mv /root/sync/report.bin /root/sync/report-final.bin")
	peer1.MustExec(ctx, "sh", "-c", "printf 'APPENDED' >> /root/sync/report-final.bin")

	wantSize := int64(size + len("APPENDED"))
	harness.Eventually(ctx, t, 90*time.Second, 500*time.Millisecond, "peer2 has renamed+edited file",
		func() (bool, string) {
			out := peer2.MustExec(ctx, "sh", "-c", "stat -c %s /root/sync/report-final.bin 2>/dev/null || echo 0")
			if strings.TrimSpace(out) == fmt.Sprintf("%d", wantSize) {
				return true, ""
			}
			return false, fmt.Sprintf("size=%s, want %d", strings.TrimSpace(out), wantSize)
		})

	// Old path must be gone on peer2 — the hint path tombstones it locally
	// as part of the rename, and the separate ActionDelete would finish the
	// job even if the hint had not fired.
	harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond, "old path removed on peer2",
		func() (bool, string) {
			code, _, _ := peer2.Exec(ctx, "test", "-f", "/root/sync/report.bin")
			if code != 0 {
				return true, ""
			}
			return false, "old path still present"
		})

	// Content check: the tail must contain the appended marker so we know
	// the receiver applied the edit rather than resurrecting old content.
	out := peer2.MustExec(ctx, "sh", "-c", "tail -c 8 /root/sync/report-final.bin")
	if strings.TrimSpace(out) != "APPENDED" {
		t.Fatalf("peer2 tail=%q, want APPENDED", out)
	}

	// No leftover temp files: hint rename is a local move + /delta over the
	// existing path, so neither .mesh-tmp-* nor *.mesh-delta-tmp-* should
	// remain after sync settles.
	for _, p := range []*harness.Node{peer1, peer2} {
		leftover := p.MustExec(ctx, "sh", "-c",
			"find /root/sync \\( -name '.mesh-tmp-*' -o -name '*.mesh-delta-tmp-*' \\) -type f | wc -l")
		if strings.TrimSpace(leftover) != "0" {
			t.Fatalf("%s: leftover temp files: %s", p.Alias, strings.TrimSpace(leftover))
		}
	}

	// The receiver's rename counter must have ticked — either via the pre-
	// existing planRenames path (if the post-rename hash happened to match
	// pre-rename, unlikely here) or via the Step 4 hint path.
	metrics := peer2.MustExec(ctx, "sh", "-c",
		"curl -sS http://127.0.0.1:7777/api/metrics | grep mesh_filesync_files_renamed_total || true")
	if !strings.Contains(metrics, "mesh_filesync_files_renamed_total{folder=\"shared\"}") {
		t.Fatalf("peer2 metrics missing files_renamed: %q", metrics)
	}
	// Extract the counter value; any non-zero count means a rename was
	// applied locally instead of a full re-download.
	var renamed int
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mesh_filesync_files_renamed_total{folder=\"shared\"}") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				fmt.Sscanf(fields[len(fields)-1], "%d", &renamed)
			}
		}
	}
	if renamed < 1 {
		t.Fatalf("peer2 files_renamed=%d, want >=1 (hint path did not fire)\nmetrics:\n%s", renamed, metrics)
	}
}

// TestFilesyncIgnorePatterns verifies that files matching configured ignore
// patterns (*.log, .git/, build/) are not synced to the peer.
func TestFilesyncIgnorePatterns(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	keepFile := harness.File{Path: "/root/sync/.keep", Content: []byte{}, Mode: 0o600}
	node1, node2 := startFilesyncPair(ctx, t, net,
		"ignore1", "configs/s2-ignore1.yaml", "IGNORE2_IP",
		"ignore2", "configs/s2-ignore2.yaml", "IGNORE1_IP",
		[]harness.File{keepFile}, []harness.File{keepFile},
	)

	// Create a mix of syncable and ignored files on node1.
	syncable := []struct {
		path    string
		content string
	}{
		{"/root/sync/readme.txt", "should sync\n"},
		{"/root/sync/src/main.go", "package main\n"},
	}
	ignored := []struct {
		path    string
		content string
	}{
		{"/root/sync/app.log", "log line\n"},
		{"/root/sync/debug.log", "debug\n"},
		{"/root/sync/.git/config", "[core]\n"},
		{"/root/sync/build/output.bin", "binary\n"},
	}

	// Create parent dirs and files.
	for _, f := range syncable {
		node1.MustExec(ctx, "sh", "-c", fmt.Sprintf("mkdir -p $(dirname %s)", f.path))
		if err := node1.WriteFile(ctx, f.path, []byte(f.content), 0o600); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
	}
	for _, f := range ignored {
		node1.MustExec(ctx, "sh", "-c", fmt.Sprintf("mkdir -p $(dirname %s)", f.path))
		if err := node1.WriteFile(ctx, f.path, []byte(f.content), 0o600); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
	}

	// Wait for syncable files to arrive on node2.
	for _, f := range syncable {
		f := f
		harness.Eventually(ctx, t, 30*time.Second, 500*time.Millisecond,
			fmt.Sprintf("%s on node2", f.path),
			func() (bool, string) {
				code, _, _ := node2.Exec(ctx, "test", "-f", f.path)
				if code == 0 {
					return true, ""
				}
				return false, "not yet"
			})
	}

	// Verify ignored files did NOT sync. Since syncable files already
	// arrived, any ignored file that was going to sync would have by now.
	for _, f := range ignored {
		code, _, _ := node2.Exec(ctx, "test", "-e", f.path)
		if code == 0 {
			t.Errorf("ignored file synced: %s", f.path)
		}
	}

	_ = node1 // used above
}
