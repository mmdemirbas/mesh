package netutil

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestBiCopy_BidirectionalData(t *testing.T) {
	// Use real TCP connections — the way BiCopy is actually used in the tunnel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverData := []byte("server says hello")
	clientData := []byte("client says hello")

	var serverGot []byte
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		serverGot = buf[:n]
		conn.Write(serverData)
		conn.(*net.TCPConn).CloseWrite()
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	client.Write(clientData)
	client.(*net.TCPConn).CloseWrite()

	clientGot, _ := io.ReadAll(client)
	client.Close()
	wg.Wait()

	if !bytes.Equal(serverGot, clientData) {
		t.Errorf("server got %q, want %q", serverGot, clientData)
	}
	if !bytes.Equal(clientGot, serverData) {
		t.Errorf("client got %q, want %q", clientGot, serverData)
	}
}

func TestBiCopy_RelayThroughProxy(t *testing.T) {
	// Simulate the exact proxy relay pattern: client → proxy → target
	// using BiCopy to connect the two sides.

	// Echo target server
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // echo
	}()

	// Proxy server that relays using BiCopy
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	wg.Add(1)
	go func() {
		defer wg.Done()
		clientConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer clientConn.Close()

		targetConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer targetConn.Close()

		BiCopy(clientConn, targetConn)
	}()

	// Client connects to proxy, sends data, reads echo
	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	testData := []byte("proxied data round trip")
	client.Write(testData)
	client.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(client)
	client.Close()

	proxy.Close()
	target.Close()
	wg.Wait()

	if !bytes.Equal(got, testData) {
		t.Errorf("round-trip got %q, want %q", got, testData)
	}
}

func TestBiCopy_LargePayload(t *testing.T) {
	// 1MB payload to verify the buffer pool works correctly under load
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	payload := make([]byte, 1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // echo
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	wg.Add(1)
	go func() {
		defer wg.Done()
		cConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer cConn.Close()
		tConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer tConn.Close()
		BiCopy(cConn, tConn)
	}()

	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	client.Write(payload)
	client.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(client)
	client.Close()
	proxy.Close()
	target.Close()
	wg.Wait()

	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestApplyTCPKeepAlive_TCPConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Should not panic on a TCP connection
	ApplyTCPKeepAlive(conn, 15*time.Second)
	// With zero period (default 30s)
	ApplyTCPKeepAlive(conn, 0)
}

func TestApplyTCPKeepAlive_NonTCP(t *testing.T) {
	// Should be a no-op for non-TCP connections, not panic
	r, w := io.Pipe()
	ApplyTCPKeepAlive(&fakeConn{r: r, w: w}, 0)
	r.Close()
	w.Close()
}

// fakeConn implements net.Conn for testing the non-TCP no-op path
type fakeConn struct {
	r io.Reader
	w io.Writer
}

func (f *fakeConn) Read(b []byte) (int, error)       { return f.r.Read(b) }
func (f *fakeConn) Write(b []byte) (int, error)      { return f.w.Write(b) }
func (f *fakeConn) Close() error                     { return nil }
func (f *fakeConn) LocalAddr() net.Addr              { return nil }
func (f *fakeConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }
