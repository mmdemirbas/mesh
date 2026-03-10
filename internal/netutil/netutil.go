package netutil

import (
	"io"
	"net"
	"sync"
	"time"
)

// ApplyTCPKeepAlive enables TCP keep-alive on the connection if it is a *net.TCPConn.
// It also explicitly asserts TCP_NODELAY to ensure interactive sessions don't buffer.
// For non-TCP connections (e.g. SSH channel wrappers from client.Listen), this is a
// silent no-op by design — keepalives are managed at the SSH layer for those.
func ApplyTCPKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true)
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		// 64KB buffer for efficient multiplexing bursts
		b := make([]byte, 64*1024)
		return &b
	},
}

// BiCopy symmetrically copies data between a and b, and closes the write half if supported.
func BiCopy(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		io.CopyBuffer(a, b, *bufPtr)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		io.CopyBuffer(b, a, *bufPtr)
		if c, ok := b.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
	}()
	wg.Wait()
}
