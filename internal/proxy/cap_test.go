package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// TestServeSocks_ConnectionCap pins the per-listener connection cap.
// With the cap lowered to 2, two clients that stall in the handshake
// hold both slots; a third dial is accepted by the kernel but immediately
// closed by the server, surfacing as EOF to the client.
//
// Deliberately not t.Parallel() — the test mutates the package-level
// maxProxyConns var, which would race with other tests reading it
// inside ServeSocks/ServeHTTPProxyWithDialer.
func TestServeSocks_ConnectionCap(t *testing.T) {
	orig := maxProxyConns
	maxProxyConns = 2
	t.Cleanup(func() { maxProxyConns = orig })

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeSocks(ctx, proxyLn, nil, slog.Default(), nil)

	var blockers []net.Conn
	defer func() {
		for _, c := range blockers {
			_ = c.Close()
		}
	}()
	for i := range 2 {
		c, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
		if err != nil {
			t.Fatalf("blocker %d dial: %v", i, err)
		}
		blockers = append(blockers, c)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		over, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
		if err != nil {
			// Server closed the conn right after accept; kernel surfaced
			// it as a connect-time RST. Still a drop.
			return
		}
		_ = over.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		buf := make([]byte, 1)
		_, readErr := over.Read(buf)
		_ = over.Close()
		if errors.Is(readErr, io.EOF) {
			return
		}
	}
	t.Fatal("over-cap dial was never dropped within deadline")
}
