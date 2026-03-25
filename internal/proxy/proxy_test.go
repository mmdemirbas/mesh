package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestDialViaSocks5_Success(t *testing.T) {
	// Start a mock SOCKS5 server
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	// Start a target echo server
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		mockSocks5Server(t, conn, targetLn.Addr().String())
	}()

	conn, err := DialViaSocks5(net.Dial, socksLn.Addr().String(), targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5 failed: %v", err)
	}
	defer conn.Close()

	testData := []byte("through socks5")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("round-trip got %q, want %q", got, testData)
	}
}

func TestDialViaSocks5_ConnectionRefused(t *testing.T) {
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		mockSocks5ServerReject(conn)
	}()

	_, err = DialViaSocks5(net.Dial, socksLn.Addr().String(), "192.168.1.1:80")
	if err == nil {
		t.Fatal("expected error for rejected connection")
	}
}

func TestDialViaSocks5_DialFailure(t *testing.T) {
	failDialer := func(network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: io.EOF}
	}

	_, err := DialViaSocks5(failDialer, "127.0.0.1:9999", "target:80")
	if err == nil {
		t.Fatal("expected error for dial failure")
	}
}

func TestDialViaSocks5_InvalidTarget(t *testing.T) {
	// No host:port → SplitHostPort fails
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	go func() {
		conn, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
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
	// Echo target
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	// SOCKS5 proxy using ServeSocks
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeSocks(ctx, socksLn, nil, slog.Default(), nil)

	// Client dials through the SOCKS5 proxy
	conn, err := DialViaSocks5(net.Dial, socksLn.Addr().String(), targetLn.Addr().String())
	if err != nil {
		t.Fatalf("DialViaSocks5 failed: %v", err)
	}
	defer conn.Close()

	testData := []byte("end-to-end socks test")
	_, _ = conn.Write(testData)
	_ = conn.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(conn)
	if string(got) != string(testData) {
		t.Errorf("got %q, want %q", got, testData)
	}
}

func TestServeHTTPProxy_CONNECT(t *testing.T) {
	// Target server that sends a response
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
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
	defer proxyLn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTPProxy(ctx, proxyLn, "", slog.Default(), nil)

	// Connect to proxy, send HTTP CONNECT, then tunnel data
	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

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
	defer targetConn.Close()

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
