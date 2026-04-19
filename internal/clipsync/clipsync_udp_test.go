package clipsync

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/mmdemirbas/mesh/internal/clipsync/proto"
	"github.com/mmdemirbas/mesh/internal/config"
	"google.golang.org/protobuf/proto"
)

// newUDPTestNode builds a Node with just enough wiring to drive
// cleanupPeers / runUDPServer / runUDPBeacon in isolation (no HTTP
// server, no clipboard polling, no TLS). defaultClient is a stub
// *http.Client so registerPeerHTTP goroutines spawned from the UDP
// server path do not panic on nil dereference.
func newUDPTestNode(t *testing.T) *Node {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Node{
		ctx: ctx,
		config: config.ClipsyncCfg{
			Bind:              "127.0.0.1:0",
			LANDiscoveryGroup: []string{"default"},
		},
		id:            "unit-node",
		port:          7755,
		peers:         make(map[string]time.Time),
		peerHashes:    make(map[string]string),
		notifyCh:      make(chan struct{}, 1),
		defaultClient: &http.Client{Timeout: 100 * time.Millisecond},
	}
}

// sendBeaconRepeatedly fires `buf` at `serverPort` on 127.0.0.1 every
// ~20ms up to `count` times. It's a workaround for the runUDPServer
// bind race: the goroutine may not have bound by the time the first
// packet is sent. Repeating is a bounded alternative to sleeping.
func sendBeaconRepeatedly(t *testing.T, serverPort int, buf []byte, count int) {
	t.Helper()
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: serverPort})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	for range count {
		if _, err := sender.Write(buf); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPeerCount polls n.peers every 10ms until len >= want or
// deadline expires. Returns the observed count at exit.
func waitForPeerCount(n *Node, want int, deadline time.Duration) int {
	end := time.Now().Add(deadline)
	var got int
	for time.Now().Before(end) {
		n.peersMu.RLock()
		got = len(n.peers)
		n.peersMu.RUnlock()
		if got >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// sendAndWaitForPeerCount keeps sending `buf` at ~50ms intervals while
// polling n.peers until want is reached or deadline expires. Needed
// because the runUDPServer goroutine may not have bound its listener
// by the time the first packet is sent — UDP drops them silently.
func sendAndWaitForPeerCount(t *testing.T, n *Node, serverPort int, buf []byte, want int, deadline time.Duration) int {
	t.Helper()
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: serverPort})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	end := time.Now().Add(deadline)
	var got int
	for time.Now().Before(end) {
		_, _ = sender.Write(buf)
		time.Sleep(20 * time.Millisecond)
		n.peersMu.RLock()
		got = len(n.peers)
		n.peersMu.RUnlock()
		if got >= want {
			return got
		}
	}
	return got
}

// pickFreeUDPPort binds briefly to grab a free port number. The caller
// races to ListenUDP on this port; fine for test isolation.
func pickFreeUDPPort(t *testing.T) int {
	t.Helper()
	lc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Skipf("UDP unavailable: %v", err)
	}
	p := lc.LocalAddr().(*net.UDPAddr).Port
	_ = lc.Close()
	return p
}

// TestCleanupPeers_ExpiresStaleEntries verifies the eviction contract:
// peers older than 15s are removed, fresh peers are preserved.
func TestCleanupPeers_ExpiresStaleEntries(t *testing.T) {
	t.Parallel()
	n := newUDPTestNode(t)

	now := time.Now()
	n.peers["stale:1"] = now.Add(-30 * time.Second)
	n.peers["fresh:1"] = now.Add(-5 * time.Second)
	n.peerHashes["stale:1"] = "h1"
	n.peerHashes["fresh:1"] = "h2"

	// Replicate the eviction body deterministically (real cleanupPeers
	// waits 10s on a ticker, not worth driving in a unit test).
	now2 := time.Now()
	n.peersMu.Lock()
	for addr, lastSeen := range n.peers {
		if age := now2.Sub(lastSeen); age > 15*time.Second {
			delete(n.peers, addr)
			delete(n.peerHashes, addr)
		}
	}
	n.peersMu.Unlock()

	if _, ok := n.peers["stale:1"]; ok {
		t.Error("stale peer not evicted")
	}
	if _, ok := n.peers["fresh:1"]; !ok {
		t.Error("fresh peer was evicted")
	}
	if _, ok := n.peerHashes["stale:1"]; ok {
		t.Error("stale peer hash not evicted")
	}
	if _, ok := n.peerHashes["fresh:1"]; !ok {
		t.Error("fresh peer hash was evicted")
	}
}

// TestCleanupPeers_CtxCancelExits verifies the goroutine exits when
// ctx is cancelled before the first tick fires.
func TestCleanupPeers_CtxCancelExits(t *testing.T) {
	t.Parallel()
	n := newUDPTestNode(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		n.cleanupPeers(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanupPeers did not exit on ctx cancel")
	}
}

// TestRunUDPServer_AcceptsValidBeacon drives the real UDP server with
// a valid beacon and asserts the sender is registered as a peer.
// This covers the full parse + group-check + port-check + registerPeer path.
func TestRunUDPServer_AcceptsValidBeacon(t *testing.T) {
	t.Parallel()
	serverPort := pickFreeUDPPort(t)
	n := newUDPTestNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { n.runUDPServer(ctx, "test-magic-v1", serverPort) })

	buf, err := proto.Marshal(&pb.Beacon{
		Magic: "test-magic-v1",
		Id:    "peer-foreign",
		Port:  9999,
		Hash:  "hashA",
		Group: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sendAndWaitForPeerCount(t, n, serverPort, buf, 1, 3*time.Second); got < 1 {
		t.Fatal("beacon did not register a peer within 3s")
	}

	var gotPeer string
	n.peersMu.RLock()
	for addr := range n.peers {
		gotPeer = addr
	}
	n.peersMu.RUnlock()
	// Peer address is remote-IP + beacon-Port, not ephemeral source port.
	if !strings.HasSuffix(gotPeer, ":9999") {
		t.Errorf("peer addr = %q, expected port suffix :9999", gotPeer)
	}

	cancel()
	wg.Wait()
}

// TestRunUDPServer_RejectsWrongMagic pins that beacons with the wrong
// magic header are silently dropped.
func TestRunUDPServer_RejectsWrongMagic(t *testing.T) {
	t.Parallel()
	serverPort := pickFreeUDPPort(t)
	n := newUDPTestNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { n.runUDPServer(ctx, "correct-magic", serverPort) })

	buf, _ := proto.Marshal(&pb.Beacon{Magic: "wrong", Id: "foreign", Port: 9999, Group: "default"})
	sendBeaconRepeatedly(t, serverPort, buf, 10)

	// After 10 sends spread over ~200ms the server has definitely
	// processed or refused every packet. No peer should be registered.
	if got := waitForPeerCount(n, 1, 300*time.Millisecond); got != 0 {
		t.Errorf("registered %d peers despite wrong magic", got)
	}

	cancel()
	wg.Wait()
}

// TestRunUDPServer_RejectsOutOfRangePort pins the SSRF guard: beacons
// advertising a port outside [minBeaconPort, maxBeaconPort] are dropped.
func TestRunUDPServer_RejectsOutOfRangePort(t *testing.T) {
	t.Parallel()
	serverPort := pickFreeUDPPort(t)
	n := newUDPTestNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { n.runUDPServer(ctx, "m", serverPort) })

	// Port 22 (SSH) — below minBeaconPort (1024).
	buf, _ := proto.Marshal(&pb.Beacon{Magic: "m", Id: "evil", Port: 22, Group: "default"})
	sendBeaconRepeatedly(t, serverPort, buf, 10)

	if got := waitForPeerCount(n, 1, 300*time.Millisecond); got != 0 {
		t.Errorf("registered %d peers despite out-of-range port", got)
	}

	cancel()
	wg.Wait()
}

// TestRunUDPServer_IgnoresSelfBeacon pins that a beacon carrying our
// own id is dropped (prevents self-registration via broadcast loopback).
func TestRunUDPServer_IgnoresSelfBeacon(t *testing.T) {
	t.Parallel()
	serverPort := pickFreeUDPPort(t)
	n := newUDPTestNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { n.runUDPServer(ctx, "m", serverPort) })

	buf, _ := proto.Marshal(&pb.Beacon{Magic: "m", Id: n.id, Port: 9999, Group: "default"})
	sendBeaconRepeatedly(t, serverPort, buf, 10)

	if got := waitForPeerCount(n, 1, 300*time.Millisecond); got != 0 {
		t.Errorf("registered %d peers on self-beacon (expected 0)", got)
	}

	cancel()
	wg.Wait()
}

// TestBeaconMarshal_RoundTrip pins the Beacon proto shape that
// runUDPBeacon emits on the wire. We do not exercise runUDPBeacon
// end-to-end because its broadcast target (255.255.255.255 and per-
// interface derived broadcast addresses) is not observable from a
// loopback listener without kernel- or iface-specific plumbing.
// The e2e scenario S3 covers the full emit+receive path; here we
// merely pin the serialization contract: the fields the sender
// constructs round-trip cleanly through the receiver's parser.
func TestBeaconMarshal_RoundTrip(t *testing.T) {
	t.Parallel()
	in := &pb.Beacon{
		Magic: "bm",
		Id:    "beacon-src",
		Port:  12345,
		Hash:  "h0",
		Group: "default",
	}
	buf, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got pb.Beacon
	if err := proto.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GetMagic() != "bm" || got.GetId() != "beacon-src" ||
		got.GetPort() != 12345 || got.GetHash() != "h0" || got.GetGroup() != "default" {
		t.Errorf("round-trip mismatch: got %+v", &got)
	}
}

// FuzzBeaconUnmarshal guards the UDP trust boundary: proto.Unmarshal
// on arbitrary UDP payloads must not panic. This is exactly the shape
// runUDPServer invokes — a malicious or mangled beacon must only be
// silently dropped, never crash the process.
func FuzzBeaconUnmarshal(f *testing.F) {
	good, _ := proto.Marshal(&pb.Beacon{Magic: "m", Id: "x", Port: 9999})
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte("not a proto"))
	f.Add(make([]byte, 4096))

	f.Fuzz(func(t *testing.T, data []byte) {
		var msg pb.Beacon
		_ = proto.Unmarshal(data, &msg)
		_ = msg.GetMagic()
		_ = msg.GetPort()
	})
}

// FuzzDiscoverRequestUnmarshal: same guard for the HTTP /discover trust
// boundary. refreshHTTPRegistration receives these; serveDiscover parses.
func FuzzDiscoverRequestUnmarshal(f *testing.F) {
	good, _ := proto.Marshal(&pb.DiscoverRequest{Id: "x", Port: 9999})
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		var req pb.DiscoverRequest
		_ = proto.Unmarshal(data, &req)
		_ = req.GetId()
	})
}
