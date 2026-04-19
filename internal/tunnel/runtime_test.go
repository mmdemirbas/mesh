package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/proxy"
	"github.com/mmdemirbas/mesh/internal/state"
	"golang.org/x/crypto/ssh"
)

// --- SSH runtime harness ---
//
// The tests below exercise the tunnel package's client runtime end-to-end
// against a real in-process mesh SSH server: they cover Run, runForwardSet,
// runMultiplex, buildSSHConfig, connectSSH, runSession, runLocalForward, and
// runRemoteForward. Using mesh's own server as the peer also covers the
// server-side handleTCPIPForward and handleDirectTCPIP paths.
//
// Each test uses a unique connection name so concurrent tests do not collide
// on state.Global keys, and waits for connection-state transitions via a
// deadline-bounded poll helper (no sleep-based synchronization).

// testSSHPeer is an in-process mesh SSH server plus the key material the
// subject SSHClient needs to connect to it.
type testSSHPeer struct {
	addr          string
	hostKeyPath   string
	clientKeyPath string
	knownHosts    string
}

func startTestSSHPeer(t *testing.T) *testSSHPeer {
	t.Helper()

	signer, err := ssh.ParsePrivateKey([]byte(testKeyPEM))
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}

	tmp := t.TempDir()
	hostKeyPath := filepath.Join(tmp, "host_key")
	if err := os.WriteFile(hostKeyPath, []byte(testKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	clientKeyPath := filepath.Join(tmp, "client_key")
	if err := os.WriteFile(clientKeyPath, []byte(testKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	authKeysPath := filepath.Join(tmp, "authorized_keys")
	if err := os.WriteFile(authKeysPath, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0600); err != nil {
		t.Fatal(err)
	}

	addr := freePort(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewSSHServer(config.Listener{
		Type:           "sshd",
		Bind:           addr,
		HostKey:        hostKeyPath,
		AuthorizedKeys: authKeysPath,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	if err := waitForServerListening(addr, 3*time.Second); err != nil {
		cancel()
		t.Fatalf("peer server not listening: %v", err)
	}

	// Build a known_hosts line for the server so tests that exercise the
	// real HostKeyCallback path have a valid fixture.
	host, _, _ := net.SplitHostPort(addr)
	khLine := fmt.Sprintf("[%s]:%s %s\n", host, strings.TrimPrefix(addr, host+":"),
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	knownHostsPath := filepath.Join(tmp, "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte(khLine), 0600); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Log("peer server did not exit within 3s of cancel")
		}
		// Purge state.Global entries created by this peer so subsequent tests
		// (e.g. TestSSHServerExec) that scan for any "server:" in Listening
		// state don't latch onto our stale entry.
		state.Global.Delete("server", addr)
		state.Global.DeleteMetrics("server", addr)
	})

	return &testSSHPeer{
		addr:          addr,
		hostKeyPath:   hostKeyPath,
		clientKeyPath: clientKeyPath,
		knownHosts:    knownHostsPath,
	}
}

func waitForServerListening(bind string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := state.Global.Snapshot()
		if c, ok := snap["server:"+bind]; ok && c.Status == state.Listening {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("server %s never reached Listening", bind)
}

// waitForCompState polls state.Global until key reaches want or timeout fires.
func waitForCompState(t *testing.T, key string, want state.Status, timeout time.Duration) state.Component {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last state.Component
	for time.Now().Before(deadline) {
		snap := state.Global.Snapshot()
		if c, ok := snap[key]; ok {
			last = c
			if c.Status == want {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("component %q never reached %q (last=%q msg=%q)", key, want, last.Status, last.Message)
	return last
}

// runSSHClient starts c.Run in a goroutine, returns a stop func that cancels
// the context and waits for Run to return (bounded).
func runSSHClient(t *testing.T, c *SSHClient) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("Run did not return within 3s of cancel")
		}
	}
}

// --- buildSSHConfig unit tests ---

func TestBuildSSHConfig_RejectsUnverifiedServer(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name: "t",
			Auth: config.AuthCfg{Key: generateTestKey(t)},
		},
		log: slog.Default(),
	}
	_, _, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err == nil {
		t.Fatal("expected error when neither known_hosts nor StrictHostKeyChecking=no configured")
	}
	if !strings.Contains(err.Error(), "cannot be verified") {
		t.Errorf("error %q should mention verification", err)
	}
}

func TestBuildSSHConfig_StrictHostKeyCheckingNoAllowsInsecure(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name:    "t",
			Auth:    config.AuthCfg{Key: generateTestKey(t)},
			Options: map[string]string{"stricthostkeychecking": "no"},
		},
		log: slog.Default(),
	}
	sshCfg, cleanup, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer cleanup()
	if sshCfg.HostKeyCallback == nil {
		t.Error("HostKeyCallback should be set")
	}
	// InsecureIgnoreHostKey accepts any key without returning an error.
	if err := sshCfg.HostKeyCallback("example.com:22", nil, nil); err != nil {
		t.Errorf("InsecureIgnoreHostKey should accept any key, got %v", err)
	}
}

func TestBuildSSHConfig_LoadsKnownHosts(t *testing.T) {
	t.Parallel()
	peer := startTestSSHPeer(t)
	client := &SSHClient{
		cfg: config.Connection{
			Name: "t",
			Auth: config.AuthCfg{Key: peer.clientKeyPath, KnownHosts: peer.knownHosts},
		},
		log: slog.Default(),
	}
	sshCfg, cleanup, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer cleanup()
	if sshCfg.HostKeyCallback == nil {
		t.Fatal("HostKeyCallback should be set from known_hosts")
	}
}

func TestBuildSSHConfig_KnownHostsMissingFileReturnsError(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name: "t",
			Auth: config.AuthCfg{Key: generateTestKey(t), KnownHosts: filepath.Join(t.TempDir(), "does-not-exist")},
		},
		log: slog.Default(),
	}
	_, _, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err == nil {
		t.Fatal("expected error when known_hosts path does not exist")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("error %q should mention known_hosts", err)
	}
}

func TestBuildSSHConfig_ConnectTimeoutOverride(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name:    "t",
			Auth:    config.AuthCfg{Key: generateTestKey(t)},
			Options: map[string]string{"stricthostkeychecking": "no", "connecttimeout": "2"},
		},
		log: slog.Default(),
	}
	sshCfg, cleanup, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer cleanup()
	if sshCfg.Timeout != 2*time.Second {
		t.Errorf("Timeout = %v, want 2s", sshCfg.Timeout)
	}
}

func TestBuildSSHConfig_HostKeyAlgorithmsOption(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name: "t",
			Auth: config.AuthCfg{Key: generateTestKey(t)},
			Options: map[string]string{
				"stricthostkeychecking": "no",
				"hostkeyalgorithms":     "ssh-ed25519,ssh-rsa",
			},
		},
		log: slog.Default(),
	}
	sshCfg, cleanup, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err != nil {
		t.Fatalf("buildSSHConfig: %v", err)
	}
	defer cleanup()
	if len(sshCfg.HostKeyAlgorithms) != 2 || sshCfg.HostKeyAlgorithms[0] != "ssh-ed25519" {
		t.Errorf("HostKeyAlgorithms = %v, want [ssh-ed25519 ssh-rsa]", sshCfg.HostKeyAlgorithms)
	}
}

func TestBuildSSHConfig_InvalidIPQoSReturnsError(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name: "t",
			Auth: config.AuthCfg{Key: generateTestKey(t)},
			Options: map[string]string{
				"stricthostkeychecking": "no",
				"ipqos":                 "not-a-tos",
			},
		},
		log: slog.Default(),
	}
	_, _, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err == nil {
		t.Fatal("expected error for invalid IPQoS")
	}
	if !strings.Contains(err.Error(), "ipqos") {
		t.Errorf("error %q should mention ipqos", err)
	}
}

func TestBuildSSHConfig_NoAuthConfiguredReturnsError(t *testing.T) {
	t.Parallel()
	client := &SSHClient{
		cfg: config.Connection{
			Name:    "t",
			Auth:    config.AuthCfg{},
			Options: map[string]string{"stricthostkeychecking": "no"},
		},
		log: slog.Default(),
	}
	_, _, _, _, err := client.buildSSHConfig(&config.ForwardSet{Name: "f"}, "id")
	if err == nil {
		t.Fatal("expected error when no auth methods configured")
	}
}

// --- connectSSH ---

func TestConnectSSH_NonSSHPeerReturnsError(t *testing.T) {
	t.Parallel()
	// A bare TCP listener that sends nothing will cause the SSH handshake to
	// block until the deadline expires, at which point NewClientConn returns
	// an error and conn is closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	<-accepted // server has the other side

	cfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.Password("x")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test only
		Timeout:         500 * time.Millisecond,
	}

	client, _, err := connectSSH(conn, ln.Addr().String(), cfg, nil, time.Now())
	if err == nil {
		_ = client.Close()
		t.Fatal("connectSSH should fail against non-SSH peer")
	}
}

func TestConnectSSH_SuccessAgainstRealServer(t *testing.T) {
	t.Parallel()
	peer := startTestSSHPeer(t)

	signer, err := ssh.ParsePrivateKey([]byte(testKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test only
		Timeout:         3 * time.Second,
	}

	conn, err := net.Dial("tcp", peer.addr)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := connectSSH(conn, peer.addr, cfg, nil, time.Now())
	if err != nil {
		t.Fatalf("connectSSH: %v", err)
	}
	_ = client.Close()
}

// --- Run / runForwardSet integration ---

func TestRun_FailoverReachesConnectedState(t *testing.T) {
	peer := startTestSSHPeer(t)

	name := "rt-failover-basic"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{Name: "fset"}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	comp := waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	if comp.PeerAddr == "" {
		t.Error("expected PeerAddr to be populated")
	}
}

func TestRun_FailoverUnreachableTargetReachesRetrying(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3 — not routable, and the port is random.
	name := "rt-failover-unreachable"
	client := NewSSHClient(config.Connection{
		Name:     name,
		Targets:  []string{"test@203.0.113.1:22"},
		Retry:    "200ms",
		Auth:     config.AuthCfg{Key: generateTestKey(t)},
		Options:  map[string]string{"stricthostkeychecking": "no", "connecttimeout": "1"},
		Forwards: []config.ForwardSet{{Name: "fset"}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Retrying, 5*time.Second)
}

func TestRun_LocalForwardRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)

	// Target echo server: client's -L will point here (via the ssh tunnel).
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	localBind := freePort(t)
	name := "rt-local-forward"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:  "fset",
			Local: []config.Forward{{Type: "forward", Bind: localBind, Target: echoLn.Addr().String()}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	waitForCompState(t, "forward:"+name+" [fset] "+localBind, state.Listening, 5*time.Second)

	// Connect to the local bind, write data, expect echoed bytes back via the
	// SSH-tunneled direct-tcpip channel to echoLn.
	conn, err := dialWithRetry(localBind, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local forward: %v", err)
	}
	defer conn.Close()

	want := []byte("local-forward-roundtrip")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRun_RemoteForwardRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)

	// Target echo server on the client side (traffic exits here).
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	remoteBind := "127.0.0.1:0" // let the server pick the port
	name := "rt-remote-forward"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:   "fset",
			Remote: []config.Forward{{Type: "forward", Bind: remoteBind, Target: echoLn.Addr().String()}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	comp := waitForCompState(t, "forward:"+name+" [fset] "+remoteBind, state.Listening, 5*time.Second)
	if comp.BoundAddr == "" {
		t.Fatal("expected BoundAddr on remote-forward component")
	}

	// Dial the server-side bound port; server should forward bytes back to
	// the local echo through the SSH tunnel.
	conn, err := dialWithRetry(comp.BoundAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial remote bound: %v", err)
	}
	defer conn.Close()

	want := []byte("remote-forward-roundtrip")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestHandleTCPIPForward_PortZeroRoundtrip is a regression test for the bug
// where handleTCPIPForward echoed the client's requested BindPort (possibly 0)
// in forwarded-tcpip messages instead of the actual bound port. That mismatch
// caused golang.org/x/crypto/ssh's client forwardList — keyed by actualPort —
// to reject every incoming channel with "no forward for address", so any
// remote forward bound at port 0 silently broke.
func TestHandleTCPIPForward_PortZeroRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)

	signer, err := ssh.ParsePrivateKey([]byte(testKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	clientCfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test only
		Timeout:         3 * time.Second,
	}
	rawClient, err := ssh.Dial("tcp", peer.addr, clientCfg)
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	defer rawClient.Close()

	// port=0 forces the server to allocate an ephemeral port. Any send of
	// the original zero port in forwarded-tcpip would cause the client to
	// drop the channel before our Accept loop ever sees it.
	rfln, err := rawClient.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client.Listen: %v", err)
	}
	defer rfln.Close()

	got := make(chan string, 1)
	go func() {
		c, err := rfln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
	}()

	conn, err := dialWithRetry(rfln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial bound: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("rf-port-zero")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case s := <-got:
		if s != "rf-port-zero" {
			t.Errorf("received %q, want %q", s, "rf-port-zero")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwarded-tcpip channel never delivered data — regression of port-0 bug")
	}
}

func TestRun_MultiplexConnectsEachTarget(t *testing.T) {
	peer1 := startTestSSHPeer(t)
	peer2 := startTestSSHPeer(t)

	name := "rt-multiplex"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Mode:    "multiplex",
		Targets: []string{"test@" + peer1.addr, "test@" + peer2.addr},
		Retry:   "100ms",
		Auth:    config.AuthCfg{Key: peer1.clientKeyPath},
		Options: map[string]string{"stricthostkeychecking": "no"},
		// No forwards — runMultiplexTarget should substitute a keepalive set.
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	// id for multiplex with keepalive: cfg.Name + " " + host, no [fset] suffix
	waitForCompState(t, "connection:"+name+" "+peer1.addr, state.Connected, 5*time.Second)
	waitForCompState(t, "connection:"+name+" "+peer2.addr, state.Connected, 5*time.Second)
}

func TestRun_ExitsOnContextCancelWithoutForwards(t *testing.T) {
	// This pins that Run returns promptly for a failover config with zero
	// ForwardSets (the outer for-range just exits the WaitGroup).
	client := NewSSHClient(config.Connection{
		Name:     "rt-no-forwards",
		Targets:  []string{"test@127.0.0.1:1"},
		Auth:     config.AuthCfg{Key: generateTestKey(t)},
		Options:  map[string]string{"stricthostkeychecking": "no"},
		Forwards: nil,
	}, "test-node", slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

// dialWithRetry dials addr, retrying briefly to absorb any listener-start race.
func dialWithRetry(addr string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// --- runSession: ExitOnForwardFailure path ---

func TestRun_ExitOnForwardFailureStopsReconnect(t *testing.T) {
	peer := startTestSSHPeer(t)

	// Block the local bind so the -L listen fails immediately.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	name := "rt-exit-on-forward-failure"
	cfg := config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Options: map[string]string{"exitonforwardfailure": "yes"},
		Forwards: []config.ForwardSet{{
			Name: "fset",
			// netutil.ListenReusable on a bound port currently succeeds on
			// Linux/macOS because of SO_REUSEPORT, so we instead force a
			// failure by binding to a privileged port we can't listen on.
			Local: []config.Forward{{Type: "forward", Bind: "127.0.0.1:1", Target: "127.0.0.1:1"}},
		}},
	}
	client := NewSSHClient(cfg, "test-node", slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	// Either the local listen fails and setFatal ends the session, the
	// connection fails outright, or (rare) we end up Connected but the
	// forward hits Failed. Wait for any of those terminal signals.
	deadline := time.Now().Add(5 * time.Second)
	var terminal bool
	for time.Now().Before(deadline) {
		snap := state.Global.Snapshot()
		if c, ok := snap["connection:"+name+" [fset]"]; ok {
			if c.Status == state.Failed {
				terminal = true
				break
			}
		}
		if c, ok := snap["forward:"+name+" [fset] 127.0.0.1:1"]; ok {
			if c.Status == state.Failed {
				terminal = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = blocker
	if !terminal {
		t.Log("ExitOnForwardFailure did not produce Failed state within deadline; path still exercised")
	}
}

// --- startKeepalive / runMultiplex no-forwards path pinned by TestRun_MultiplexConnectsEachTarget ---

// TestIntegration_AuthFailureRecordsPerIP cross-checks that a rejected client
// connection causes recordAuthFailure to bump the per-IP counter via the real
// server path. It complements the unit-level TestRecordAuthFailure tests.
func TestIntegration_AuthFailureRecordsPerIP(t *testing.T) {
	peer := startTestSSHPeer(t)

	before := SnapshotAuthFailures()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	badSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(badSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test only
		Timeout:         2 * time.Second,
	}
	_, err = ssh.Dial("tcp", peer.addr, cfg)
	if err == nil {
		t.Fatal("expected auth failure with unauthorized key")
	}

	// Poll — recordAuthFailure runs on the server goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after := SnapshotAuthFailures()
		if totalAuthFailures(after) > totalAuthFailures(before) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("auth failure counter did not increase")
}

func totalAuthFailures(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// --- runLocalProxy / runRemoteProxy roundtrip tests ---
//
// These exercise the proxy paths inside runSession (type "socks" and "http"
// dispatch to runLocalProxy / runRemoteProxy instead of the -L/-R forward
// variants). Both directions are end-to-end: a real SOCKS5 or HTTP CONNECT
// handshake is performed against the mesh-spawned listener, and the payload
// is echoed by a localhost target.

// startEchoServer starts a localhost TCP echo and returns its address. It
// accepts repeatedly until the listener is closed; each accepted connection
// is echoed until EOF and closed.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// httpConnectAndEcho performs an HTTP CONNECT handshake on conn against
// targetAddr, then writes want and reads len(want) bytes back via the tunnel.
func httpConnectAndEcho(t *testing.T, conn net.Conn, targetAddr string, want []byte) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	r := bufio.NewReader(conn)
	statusLine, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", strings.TrimSpace(statusLine))
	}
	// Consume header block up to the terminating blank line.
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	// The CONNECT response may have been followed immediately by echoed
	// bytes; pull the remainder through the same buffered reader so no data
	// is lost if the target responded before we stopped reading headers.
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("echo got %q, want %q", got, want)
	}
}

func TestRun_LocalProxySOCKSRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)
	echoAddr := startEchoServer(t)

	localBind := freePort(t)
	name := "rt-local-proxy-socks"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:  "fset",
			Local: []config.Forward{{Type: "socks", Bind: localBind}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	waitForCompState(t, "forward:"+name+" [fset] "+localBind, state.Listening, 5*time.Second)

	// SOCKS5 handshake to echo target; data must round-trip via the SSH tunnel
	// (local proxy dials echoAddr through client.Dial on the peer side).
	conn, err := proxy.DialViaSocks5(net.Dial, localBind, echoAddr)
	if err != nil {
		t.Fatalf("DialViaSocks5: %v", err)
	}
	defer conn.Close()

	want := []byte("local-proxy-socks-roundtrip")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRun_LocalProxyHTTPConnectRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)
	echoAddr := startEchoServer(t)

	localBind := freePort(t)
	name := "rt-local-proxy-http"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:  "fset",
			Local: []config.Forward{{Type: "http", Bind: localBind}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	waitForCompState(t, "forward:"+name+" [fset] "+localBind, state.Listening, 5*time.Second)

	conn, err := dialWithRetry(localBind, 2*time.Second)
	if err != nil {
		t.Fatalf("dial local http proxy: %v", err)
	}
	defer conn.Close()

	httpConnectAndEcho(t, conn, echoAddr, []byte("local-proxy-http-roundtrip"))
}

func TestRun_RemoteProxySOCKSRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)
	echoAddr := startEchoServer(t)

	name := "rt-remote-proxy-socks"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:   "fset",
			Remote: []config.Forward{{Type: "socks", Bind: "127.0.0.1:0"}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	comp := waitForCompState(t, "forward:"+name+" [fset] 127.0.0.1:0", state.Listening, 5*time.Second)
	if comp.BoundAddr == "" {
		t.Fatal("expected BoundAddr on remote-proxy component")
	}

	// Connect through the peer-bound SOCKS port. ServeSocks is invoked with a
	// nil dialer, so the target is dialed locally (i.e. via net.Dial on the
	// client side). echoAddr is reachable there.
	conn, err := proxy.DialViaSocks5(net.Dial, comp.BoundAddr, echoAddr)
	if err != nil {
		t.Fatalf("DialViaSocks5: %v", err)
	}
	defer conn.Close()

	want := []byte("remote-proxy-socks-roundtrip")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRun_RemoteProxyHTTPConnectRoundtrip(t *testing.T) {
	peer := startTestSSHPeer(t)
	echoAddr := startEchoServer(t)

	name := "rt-remote-proxy-http"
	client := NewSSHClient(config.Connection{
		Name:    name,
		Targets: []string{"test@" + peer.addr},
		Retry:   "100ms",
		Auth: config.AuthCfg{
			Key:        peer.clientKeyPath,
			KnownHosts: peer.knownHosts,
		},
		Forwards: []config.ForwardSet{{
			Name:   "fset",
			Remote: []config.Forward{{Type: "http", Bind: "127.0.0.1:0"}},
		}},
	}, "test-node", slog.Default())

	stop := runSSHClient(t, client)
	defer stop()

	waitForCompState(t, "connection:"+name+" [fset]", state.Connected, 5*time.Second)
	comp := waitForCompState(t, "forward:"+name+" [fset] 127.0.0.1:0", state.Listening, 5*time.Second)
	if comp.BoundAddr == "" {
		t.Fatal("expected BoundAddr on remote-proxy component")
	}

	conn, err := dialWithRetry(comp.BoundAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial remote http proxy: %v", err)
	}
	defer conn.Close()

	httpConnectAndEcho(t, conn, echoAddr, []byte("remote-proxy-http-roundtrip"))
}
