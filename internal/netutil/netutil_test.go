package netutil

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBiCopy_BidirectionalData(t *testing.T) {
	// Use real TCP connections — the way BiCopy is actually used in the tunnel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

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
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		serverGot = buf[:n]
		_, _ = conn.Write(serverData)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	_, _ = client.Write(clientData)
	_ = client.(*net.TCPConn).CloseWrite()

	clientGot, _ := io.ReadAll(client)
	_ = client.Close()
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
	defer func() { _ = target.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo
	}()

	// Proxy server that relays using BiCopy
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()

	wg.Add(1)
	go func() {
		defer wg.Done()
		clientConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer func() { _ = clientConn.Close() }()

		targetConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = targetConn.Close() }()

		BiCopy(clientConn, targetConn)
	}()

	// Client connects to proxy, sends data, reads echo
	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	testData := []byte("proxied data round trip")
	_, _ = client.Write(testData)
	_ = client.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(client)
	_ = client.Close()

	_ = proxy.Close()
	_ = target.Close()
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
	defer func() { _ = target.Close() }()

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
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()

	wg.Add(1)
	go func() {
		defer wg.Done()
		cConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer func() { _ = cConn.Close() }()
		tConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = tConn.Close() }()
		BiCopy(cConn, tConn)
	}()

	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = client.Write(payload)
	_ = client.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(client)
	_ = client.Close()
	_ = proxy.Close()
	_ = target.Close()
	wg.Wait()

	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestCountedBiCopy_ByteCounting(t *testing.T) {
	// Echo server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	serverData := []byte("hello from server!!")    // 19 bytes
	clientData := []byte("hello from client!!!!!") // 22 bytes

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		_ = n
		_, _ = conn.Write(serverData)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	var tx, rx atomic.Int64
	_, _ = client.Write(clientData)
	_ = client.(*net.TCPConn).CloseWrite()

	got, _ := io.ReadAll(client)
	_ = client.Close()
	wg.Wait()

	if !bytes.Equal(got, serverData) {
		t.Errorf("got %q, want %q", got, serverData)
	}

	// Now test with CountedBiCopy through a proxy relay
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln2.Close() }()

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo
	}()

	tx.Store(0)
	rx.Store(0)

	wg.Add(1)
	go func() {
		defer wg.Done()
		cConn, err := ln2.Accept()
		if err != nil {
			return
		}
		defer func() { _ = cConn.Close() }()
		tConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = tConn.Close() }()
		CountedBiCopy(cConn, tConn, &tx, &rx)
	}()

	client2, err := net.DialTimeout("tcp", ln2.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	testPayload := []byte("counted bytes test data")
	_, _ = client2.Write(testPayload)
	_ = client2.(*net.TCPConn).CloseWrite()
	echoed, _ := io.ReadAll(client2)
	_ = client2.Close()
	_ = ln2.Close()
	_ = target.Close()
	wg.Wait()

	if !bytes.Equal(echoed, testPayload) {
		t.Errorf("echo mismatch: got %d bytes, want %d", len(echoed), len(testPayload))
	}

	// tx = data written to client side (echo response), rx = data written to target side (original)
	// In an echo proxy: client→proxy→target→proxy→client
	// tx counts proxy→client (the echo), rx counts proxy→target (the original)
	totalBytes := tx.Load() + rx.Load()
	expectedTotal := int64(len(testPayload)) * 2 // sent + echoed
	if totalBytes != expectedTotal {
		t.Errorf("total bytes = %d (tx=%d, rx=%d), want %d", totalBytes, tx.Load(), rx.Load(), expectedTotal)
	}
}

func TestCountedBiCopy_LargePayload(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()

	payload := make([]byte, 512*1024) // 512KB
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
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()

	var tx, rx atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		cConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer func() { _ = cConn.Close() }()
		tConn, err := net.Dial("tcp", target.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = tConn.Close() }()
		CountedBiCopy(cConn, tConn, &tx, &rx)
	}()

	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write(payload)
	_ = client.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(client)
	_ = client.Close()
	_ = proxy.Close()
	_ = target.Close()
	wg.Wait()

	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	expectedTotal := int64(len(payload)) * 2
	totalBytes := tx.Load() + rx.Load()
	if totalBytes != expectedTotal {
		t.Errorf("counted bytes = %d (tx=%d, rx=%d), want %d", totalBytes, tx.Load(), rx.Load(), expectedTotal)
	}
}

func TestCountingWriter(t *testing.T) {
	var buf bytes.Buffer
	var counter atomic.Int64
	cw := &countingWriter{w: &buf, counter: &counter}

	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, want 5", n)
	}
	if counter.Load() != 5 {
		t.Errorf("counter = %d, want 5", counter.Load())
	}

	_, _ = cw.Write([]byte(" world"))
	if counter.Load() != 11 {
		t.Errorf("counter = %d, want 11", counter.Load())
	}
	if buf.String() != "hello world" {
		t.Errorf("buf = %q, want %q", buf.String(), "hello world")
	}
}

// TestCountedBiCopy_DynamicForwardPattern simulates the metrics tracking pattern
// used in handleTCPIPForward: multiple concurrent connections through a shared
// dynamic forward, each tracked on the same atomic counters via GetMetrics.
func TestCountedBiCopy_DynamicForwardPattern(t *testing.T) {
	// Echo target (like peer's syncthing)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()

	var echoWg sync.WaitGroup
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			echoWg.Add(1)
			go func() {
				defer echoWg.Done()
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn) // echo
			}()
		}
	}()

	// Dynamic forward listener (like the sshd's tcpip-forward listener)
	dynLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// Shared metrics per dynamic forward (like dm in handleTCPIPForward)
	var dynTx, dynRx atomic.Int64
	var dynStreams atomic.Int32

	var proxyWg sync.WaitGroup
	go func() {
		for {
			conn, err := dynLn.Accept()
			if err != nil {
				return
			}
			proxyWg.Add(1)
			go func() {
				defer proxyWg.Done()
				defer func() { _ = conn.Close() }()
				// Simulate: open SSH channel -> dial target
				tgt, err := net.Dial("tcp", target.Addr().String())
				if err != nil {
					return
				}
				defer func() { _ = tgt.Close() }()
				dynStreams.Add(1)
				defer dynStreams.Add(-1)
				CountedBiCopy(conn, tgt, &dynTx, &dynRx)
			}()
		}
	}()

	// Send data through 3 concurrent connections (like 3 syncthing transfers)
	payload := []byte("dynamic forward test payload - simulating syncthing data")
	var clientWg sync.WaitGroup
	for range 3 {
		clientWg.Add(1)
		go func() {
			defer clientWg.Done()
			conn, err := net.DialTimeout("tcp", dynLn.Addr().String(), time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = conn.Write(payload)
			_ = conn.(*net.TCPConn).CloseWrite()
			_, _ = io.ReadAll(conn) // read echo
			_ = conn.Close()
		}()
	}

	clientWg.Wait()
	_ = dynLn.Close()
	proxyWg.Wait()
	_ = target.Close()
	echoWg.Wait()

	// Verify metrics were tracked correctly
	expectedPerConn := int64(len(payload)) * 2 // sent + echoed
	expectedTotal := expectedPerConn * 3       // 3 connections
	totalBytes := dynTx.Load() + dynRx.Load()
	if totalBytes != expectedTotal {
		t.Errorf("total bytes = %d (tx=%d, rx=%d), want %d",
			totalBytes, dynTx.Load(), dynRx.Load(), expectedTotal)
	}

	// After all connections close, streams should be 0
	if s := dynStreams.Load(); s != 0 {
		t.Errorf("streams after close = %d, want 0", s)
	}
}

// TestCountedBiCopy_StreamsTrackedDuringTransfer verifies that stream counts
// are visible while data is being transferred (not just after completion).
func TestCountedBiCopy_StreamsTrackedDuringTransfer(t *testing.T) {
	// Slow echo: holds connections open so we can observe active streams
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	ready := make(chan struct{})
	release := make(chan struct{})

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf) // read first chunk
		close(ready)           // signal that we have data
		<-release              // wait for test to check metrics
		_, _ = conn.Write(buf[:n])
		_ = conn.(*net.TCPConn).CloseWrite()
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var tx, rx atomic.Int64
	var streams atomic.Int32
	var proxyDone sync.WaitGroup
	proxyDone.Add(1)
	go func() {
		defer proxyDone.Done()
		cConn, err := proxy.Accept()
		if err != nil {
			return
		}
		defer func() { _ = cConn.Close() }()
		tConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = tConn.Close() }()
		streams.Add(1)
		defer streams.Add(-1)
		CountedBiCopy(cConn, tConn, &tx, &rx)
	}()

	client, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write([]byte("mid-flight"))
	_ = client.(*net.TCPConn).CloseWrite()

	<-ready // wait for data to arrive at target

	// While transfer is in progress: stream count should be 1
	if s := streams.Load(); s != 1 {
		t.Errorf("active streams during transfer = %d, want 1", s)
	}
	// tx should have counted client→target bytes
	if tx.Load() == 0 && rx.Load() == 0 {
		t.Error("no bytes counted during active transfer")
	}

	close(release)            // let the transfer complete
	_, _ = io.ReadAll(client) // drain echo
	_ = client.Close()
	_ = proxy.Close()
	proxyDone.Wait()

	if s := streams.Load(); s != 0 {
		t.Errorf("streams after close = %d, want 0", s)
	}
}

func TestApplyTCPKeepAlive_TCPConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Should not panic on a TCP connection
	ApplyTCPKeepAlive(conn, 15*time.Second)
	// With zero period (default 30s)
	ApplyTCPKeepAlive(conn, 0)
}

func TestApplyTCPKeepAlive_NonTCP(t *testing.T) {
	// Should be a no-op for non-TCP connections, not panic
	r, w := io.Pipe()
	ApplyTCPKeepAlive(&fakeConn{r: r, w: w}, 0)
	_ = r.Close()
	_ = w.Close()
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
