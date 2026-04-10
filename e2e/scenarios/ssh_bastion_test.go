//go:build e2e

package scenarios

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/e2e/harness"
)

// requireImage short-circuits scenario tests when mesh-e2e:local is missing.
// Keeping it local (not in harness) avoids pulling exec/docker into the
// harness-level smoke tests.
func requireImage(t testing.TB, image string) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("e2e: image %s not present, run `task build:e2e-image` first", image)
	}
}

// TestSSHBastionTunnel wires client → bastion → server and asserts end-to-end
// that a local forward on the client reaches the server's sshd through the
// bastion. It also exercises the retry path by stopping and restarting the
// bastion container.
func TestSSHBastionTunnel(t *testing.T) {
	requireImage(t, harness.DefaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	net := harness.NewNetwork(ctx, t)

	// One client keypair reused for server + bastion authorized_keys. Each
	// sshd gets its own host key.
	clientKey := harness.GenerateKeyPair(t, "client@mesh-e2e")
	serverHost := harness.GenerateKeyPair(t, "server@mesh-e2e")
	bastionHost := harness.GenerateKeyPair(t, "bastion@mesh-e2e")

	serverCfg, err := harness.LoadTemplate("configs/s1-server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bastionCfg, err := harness.LoadTemplate("configs/s1-bastion.yaml")
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := harness.LoadTemplate("configs/s1-client.yaml")
	if err != nil {
		t.Fatal(err)
	}

	keyFiles := func(host harness.KeyPair) []harness.File {
		return []harness.File{
			{Path: "/root/.mesh/keys/host_key", Content: host.PrivatePEM, Mode: 0o600},
			{Path: "/root/.mesh/keys/authorized_keys", Content: clientKey.AuthorizedLine, Mode: 0o600},
		}
	}

	server := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:  "server",
		Config: serverCfg,
		Files:  keyFiles(serverHost),
	})
	bastion := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:  "bastion",
		Config: bastionCfg,
		Files:  keyFiles(bastionHost),
	})
	client := harness.StartNode(ctx, t, net, harness.NodeOptions{
		Alias:  "client",
		Config: clientCfg,
		Files: []harness.File{
			{Path: "/root/.mesh/keys/client_key", Content: clientKey.PrivatePEM, Mode: 0o600},
		},
	})
	harness.DumpOnFailure(t, server, bastion, client)

	// 1. Client connection to bastion reaches Connected and the forward
	//    :2222 reaches Listening.
	harness.WaitForComponent(ctx, t, client, "connection", "bastion", "connected", 30*time.Second)
	forward := harness.WaitForComponent(ctx, t, client, "forward", "ssh-to-server", "listening", 15*time.Second)

	// 2. End-to-end SSH: client's local :2222 → bastion → server's sshd.
	sshCmd := []string{
		"ssh",
		"-q", // suppress "Warning: Permanently added ..." banner
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		"-i", "/root/.mesh/keys/client_key",
		"-p", "2222",
		"root@127.0.0.1",
		"whoami",
	}
	// Give the tunnel a moment to settle after reaching Listening; the
	// first connection through a fresh forward occasionally races the
	// channel setup.
	harness.Eventually(ctx, t, 20*time.Second, 500*time.Millisecond, "ssh whoami returns root",
		func() (bool, string) {
			code, out, err := client.Exec(ctx, sshCmd...)
			if err != nil {
				return false, err.Error()
			}
			if code != 0 {
				return false, fmt.Sprintf("exit=%d output=%q", code, out)
			}
			// Match "root" as a standalone line so any late-arriving
			// ssh warning does not invalidate the test.
			for _, line := range strings.Split(out, "\n") {
				if strings.TrimSpace(line) == "root" {
					return true, ""
				}
			}
			return false, fmt.Sprintf("output=%q want line=root", out)
		})

	// 3. Metrics: forward tx or rx should be non-zero after the ssh round
	//    trip. Mesh records bytes per forward component keyed by id, which
	//    matches the StateSnapshot component.
	body, err := client.AdminMetrics(ctx)
	if err != nil {
		t.Fatalf("admin metrics: %v", err)
	}
	sel := fmt.Sprintf("id=%q", forward.ID)
	tx, okTx := harness.MetricValue(body, "mesh_bytes_tx_total", sel)
	rx, okRx := harness.MetricValue(body, "mesh_bytes_rx_total", sel)
	if !okTx || !okRx {
		t.Fatalf("forward metrics missing: tx=%v/%v rx=%v/%v body:\n%s", tx, okTx, rx, okRx, body)
	}
	if tx == 0 && rx == 0 {
		t.Fatalf("forward metrics are zero after ssh round trip: tx=%v rx=%v", tx, rx)
	}

	// 4. Kill the bastion: client connection should drop to retrying.
	if err := bastion.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("stop bastion: %v", err)
	}
	harness.WaitForComponent(ctx, t, client, "connection", "bastion", "retrying", 30*time.Second)

	// 5. Restart bastion: client should re-establish Connected. We have to
	//    wait up to the mesh retry interval (2s per s1-client.yaml) plus
	//    SSH handshake time.
	if err := bastion.Start(ctx); err != nil {
		t.Fatalf("restart bastion: %v", err)
	}
	harness.WaitForComponent(ctx, t, client, "connection", "bastion", "connected", 60*time.Second)
}
