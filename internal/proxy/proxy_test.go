package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/state"
)

func TestDialViaSocks5_Success(t *testing.T) {
	t.Parallel()
	// Start a mock SOCKS5 server
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socksLn.Close() }()

	// Start a target echo server
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mockSocks5Server(t, conn, targetLn.Addr().String())
	}()

	conn, err := DialViaSocks5(net.Dial, socksLn.Addr().String(), targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5 failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	testData := []byte("through socks5")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("round-trip got %q, want %q", got, testData)
	}
}

// TestDialViaSocks5_IPv6BindResponse verifies the client handles a SOCKS5
// server that returns an IPv6 bind address in its CONNECT response (atyp=0x04).
// This exercises the 18-byte read path; a 10-byte buffer would panic here.
func TestDialViaSocks5_IPv6BindResponse(t *testing.T) {
	t.Parallel()
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socksLn.Close() }()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mockSocks5ServerIPv6Bind(t, conn, targetLn.Addr().String())
	}()

	conn, err := DialViaSocks5(net.Dial, socksLn.Addr().String(), targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5 with IPv6 bind addr failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	testData := []byte("ipv6-bind-response")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("round-trip got %q, want %q", got, testData)
	}
}

func TestDialViaSocks5_ConnectionRefused(t *testing.T) {
	t.Parallel()
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socksLn.Close() }()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mockSocks5ServerReject(conn)
	}()

	_, err = DialViaSocks5(net.Dial, socksLn.Addr().String(), "192.168.1.1:80")
	if err == nil {
		t.Fatal("expected error for rejected connection")
	}
}

func TestDialViaSocks5_DialFailure(t *testing.T) {
	t.Parallel()
	failDialer := func(network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: io.EOF}
	}

	_, err := DialViaSocks5(failDialer, "127.0.0.1:9999", "target:80")
	if err == nil {
		t.Fatal("expected error for dial failure")
	}
}

func TestDialViaSocks5_InvalidTarget(t *testing.T) {
	t.Parallel()
	// No host:port → SplitHostPort fails
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socksLn.Close() }()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read greeting + respond
		buf := make([]byte, 258)
		_, _ = io.ReadFull(conn, buf[:2])
		_, _ = io.ReadFull(conn, buf[:buf[1]])
		_, _ = conn.Write([]byte{0x05, 0x00})
		// Client will fail before sending connect request
	}()

	_, err = DialViaSocks5(net.Dial, socksLn.Addr().String(), "no-port")
	if err == nil {
		t.Fatal("expected error for invalid target")
	}
}

func TestServeSocks_EndToEnd(t *testing.T) {
	t.Parallel()
	// Echo target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	// SOCKS5 proxy using ServeSocks
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socksLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeSocks(ctx, socksLn, nil, slog.Default(), nil)

	// Client dials through the SOCKS5 proxy
	conn, err := DialViaSocks5(net.Dial, socksLn.Addr().String(), targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5 failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	testData := []byte("end-to-end socks test")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("got %q, want %q", got, testData)
	}
}

func TestServeHTTPProxy_CONNECT(t *testing.T) {
	t.Parallel()
	// Target server that sends a response
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Read whatever the client sends, then respond
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n]) // echo
	}()

	// HTTP proxy
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTPProxy(ctx, proxyLn, "", slog.Default(), nil)

	// Connect to proxy, send HTTP CONNECT, then tunnel data
	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Send CONNECT request
	connectReq := "CONNECT " + targetLn.Addr().String() + " HTTP/1.1\r\nHost: " + targetLn.Addr().String() + "\r\n\r\n"
	_, _ = conn.Write([]byte(connectReq))

	// Read response (should be 200 Connection established)
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read connect response: %v", err)
	}
	resp := string(buf[:n])
	if resp[:12] != "HTTP/1.1 200" {
		t.Fatalf("expected 200, got: %s", resp)
	}

	// Now send data through the tunnel
	testData := []byte("tunneled data")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("tunneled data got %q, want %q", got, testData)
	}
}

func TestServeHTTPProxy_NonCONNECT(t *testing.T) {
	t.Parallel()
	// Target HTTP server that echoes the request method and path.
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer targetSrv.Close()

	// HTTP proxy
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTPProxy(ctx, proxyLn, "", slog.Default(), nil)

	// Send a plain HTTP GET through the proxy (non-CONNECT path).
	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	req := "GET " + targetSrv.URL + "/test HTTP/1.1\r\nHost: " + strings.TrimPrefix(targetSrv.URL, "http://") + "\r\nConnection: close\r\n\r\n"
	_, _ = conn.Write([]byte(req))

	got, _ := io.ReadAll(conn)
	resp := string(got)
	if !strings.Contains(resp, "200") {
		t.Fatalf("expected 200 response, got: %s", resp)
	}
	if !strings.Contains(resp, "proxied") {
		t.Errorf("response body missing expected content, got: %s", resp)
	}
}

func TestServeHTTPProxy_NonCONNECT_DialFailure(t *testing.T) {
	t.Parallel()
	// Proxy with a dialer that always fails
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dialer := func(addr string) (net.Conn, error) {
		return nil, fmt.Errorf("simulated dial failure")
	}
	go ServeHTTPProxyWithDialer(ctx, proxyLn, dialer, slog.Default(), nil)

	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	req := "GET http://unreachable.test/path HTTP/1.1\r\nHost: unreachable.test\r\nConnection: close\r\n\r\n"
	_, _ = conn.Write([]byte(req))

	got, _ := io.ReadAll(conn)
	if !strings.Contains(string(got), "502") {
		t.Errorf("expected 502 Bad Gateway, got: %s", string(got))
	}
}

// --- Mock SOCKS5 server helpers ---

func mockSocks5Server(t *testing.T, conn net.Conn, targetAddr string) {
	t.Helper()
	buf := make([]byte, 258)

	_, _ = io.ReadFull(conn, buf[:2])
	_, _ = io.ReadFull(conn, buf[:buf[1]])
	_, _ = conn.Write([]byte{0x05, 0x00})

	_, _ = io.ReadFull(conn, buf[:4])
	switch buf[3] {
	case 0x01:
		_, _ = io.ReadFull(conn, buf[:6])
	case 0x03:
		_, _ = io.ReadFull(conn, buf[:1])
		_, _ = io.ReadFull(conn, buf[:buf[0]+2])
	case 0x04:
		_, _ = io.ReadFull(conn, buf[:18])
	}

	targetConn, err := net.DialTimeout("tcp", targetAddr, time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = targetConn.Close() }()

	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	// Relay both directions; when one side closes, close the other
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(targetConn, conn)
		_ = targetConn.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(conn, targetConn)
	_ = conn.(*net.TCPConn).CloseWrite()
	<-done
}

func mockSocks5ServerReject(conn net.Conn) {
	buf := make([]byte, 258)
	_, _ = io.ReadFull(conn, buf[:2])
	_, _ = io.ReadFull(conn, buf[:buf[1]])
	_, _ = conn.Write([]byte{0x05, 0x00})

	_, _ = io.ReadFull(conn, buf[:4])
	switch buf[3] {
	case 0x01:
		_, _ = io.ReadFull(conn, buf[:6])
	case 0x03:
		_, _ = io.ReadFull(conn, buf[:1])
		_, _ = io.ReadFull(conn, buf[:buf[0]+2])
	case 0x04:
		_, _ = io.ReadFull(conn, buf[:18])
	}

	_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}

func TestHandleSocks5_MalformedGreeting(t *testing.T) {
	t.Parallel()
	// A non-SOCKS5 greeting should be rejected without hanging.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeSocks(ctx, proxyLn, nil, slog.Default(), nil)

	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Send garbage (not SOCKS5 version byte 0x05)
	_, _ = conn.Write([]byte{0x04, 0x01, 0x00})

	// The handler should close the connection promptly
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected connection to be closed by server, but got data")
	}
}

func TestHandleHTTPProxy_MalformedRequest(t *testing.T) {
	t.Parallel()
	// Garbage input should be rejected without hanging.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxyLn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeHTTPProxy(ctx, proxyLn, "", slog.Default(), nil)

	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Send garbage that isn't a valid HTTP request
	_, _ = conn.Write([]byte("NOT_HTTP\r\n\r\n"))

	// The handler should close the connection promptly
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected connection to be closed by server, but got data")
	}
}

// mockSocks5ServerIPv6Bind responds to CONNECT with an IPv6 bind address
// (atyp=0x04, 16-byte addr + 2-byte port) then relays traffic to targetAddr.
func mockSocks5ServerIPv6Bind(t *testing.T, conn net.Conn, targetAddr string) {
	t.Helper()
	buf := make([]byte, 258)

	_, _ = io.ReadFull(conn, buf[:2])
	_, _ = io.ReadFull(conn, buf[:buf[1]])
	_, _ = conn.Write([]byte{0x05, 0x00})

	_, _ = io.ReadFull(conn, buf[:4])
	switch buf[3] {
	case 0x01:
		_, _ = io.ReadFull(conn, buf[:6])
	case 0x03:
		_, _ = io.ReadFull(conn, buf[:1])
		_, _ = io.ReadFull(conn, buf[:buf[0]+2])
	case 0x04:
		_, _ = io.ReadFull(conn, buf[:18])
	}

	targetConn, err := net.DialTimeout("tcp", targetAddr, time.Second)
	if err != nil {
		// Reply with IPv6 bind addr but failed status
		_, _ = conn.Write(append([]byte{0x05, 0x05, 0x00, 0x04}, make([]byte, 18)...))
		return
	}
	defer func() { _ = targetConn.Close() }()

	// Success reply with IPv6 bind address (atyp=0x04): ver=5, rep=0, rsv=0, atyp=4, addr=16×0, port=2×0
	resp := make([]byte, 4+16+2)
	resp[0], resp[1], resp[2], resp[3] = 0x05, 0x00, 0x00, 0x04
	_, _ = conn.Write(resp)

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(targetConn, conn)
		_ = targetConn.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(conn, targetConn)
	_ = conn.(*net.TCPConn).CloseWrite()
	<-done
}

// freePort returns an address on 127.0.0.1 that is currently unused.
// TOCTOU-prone but standard practice for testing servers that bind by
// address rather than by existing listener.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestRunStandaloneRelays_EndToEnd pins the orchestration wrapper behavior:
// a config.Listener of type "relay" results in bytes flowing from a client
// connection through to the configured target.
func TestRunStandaloneRelays_EndToEnd(t *testing.T) {
	t.Parallel()

	// Echo target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()
	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	bind := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	RunStandaloneRelays(ctx, []config.Listener{
		{Type: "relay", Bind: bind, Target: targetLn.Addr().String()},
		{Type: "socks", Bind: "ignored"}, // filter path: must be skipped
	}, slog.Default(), &wg)

	// Wait until the relay is listening.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := state.Global.Snapshot()["relay:"+bind]
		if ok && snap.Status == state.Listening {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := net.DialTimeout("tcp", bind, time.Second)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload := []byte("hello-via-relay")
	_, _ = conn.Write(payload)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(payload) {
		t.Errorf("echoed = %q, want %q", got, payload)
	}

	cancel()
	wg.Wait()
}

// TestRunStandaloneProxies_StartsSOCKS confirms the proxy wrapper starts a
// SOCKS5 server that handles a real CONNECT, and skips non-socks/http
// entries rather than failing.
func TestRunStandaloneProxies_StartsSOCKS(t *testing.T) {
	t.Parallel()

	// Echo target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetLn.Close() }()
	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	bind := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	RunStandaloneProxies(ctx, []config.Listener{
		{Type: "socks", Bind: bind},
		{Type: "relay", Bind: "ignored"}, // filter path: must be skipped
	}, slog.Default(), &wg)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := state.Global.Snapshot()["proxy:"+bind]
		if ok && snap.Status == state.Listening {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := DialViaSocks5(net.Dial, bind, targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5: %v", err)
	}
	defer func() { _ = conn.Close() }()

	payload := []byte("hello-via-socks-wrapper")
	_, _ = conn.Write(payload)
	_ = conn.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(conn)
	if string(got) != string(payload) {
		t.Errorf("echoed = %q, want %q", got, payload)
	}

	cancel()
	wg.Wait()
}
