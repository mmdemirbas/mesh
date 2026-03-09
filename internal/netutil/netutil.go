package netutil

import (
	"io"
	"net"
	"sync"
	"time"
)

// ApplyTCPKeepAlive enables TCP keep-alive on the connection if it is a *net.TCPConn.
// For non-TCP connections (e.g. SSH channel wrappers from client.Listen), this is a
// silent no-op by design — keepalives are managed at the SSH layer for those.
func ApplyTCPKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}

// BiCopy symmetrically copies data between a and b, and closes the write half if supported.
func BiCopy(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(b, a)
		if c, ok := b.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	wg.Wait()
}
