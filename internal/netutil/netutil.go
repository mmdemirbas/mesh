package netutil

import (
	"io"
	"net"
	"sync"
	"time"
)

// ApplyTCPKeepAlive enables TCP keep-alive on the connection if it is a *net.TCPConn.
// It explicitly asserts TCP_NODELAY to ensure interactive sessions don't buffer,
// and forces SetLinger(0) to ensure sockets close immediately with a RST packet
// instead of lingering politely in TIME_WAIT, keeping proxy listener ports clean.
func ApplyTCPKeepAlive(conn net.Conn, period time.Duration) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		if period == 0 {
			period = 30 * time.Second
		}
		_ = tcpConn.SetKeepAlivePeriod(period)
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetLinger(0)
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		// 32KB buffer for optimal TCP streaming and memory usage
		b := make([]byte, 32*1024)
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
		_, _ = io.CopyBuffer(a, b, *bufPtr)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		_, _ = io.CopyBuffer(b, a, *bufPtr)
		if c, ok := b.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	wg.Wait()
}
