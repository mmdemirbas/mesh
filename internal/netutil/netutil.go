package netutil

import (
	"context"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// ApplyTCPKeepAlive enables TCP keep-alive on the connection if it is a *net.TCPConn.
// It explicitly asserts TCP_NODELAY to ensure interactive sessions don't buffer,
// and forces SetLinger(0) to ensure sockets close immediately with a RST packet
// instead of lingering politely in TIME_WAIT, keeping proxy listener ports clean.
func ApplyTCPKeepAlive(conn net.Conn, period time.Duration) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		if period == 0 {
			period = 30 * time.Second
		}
		tcpConn.SetKeepAlivePeriod(period)
		tcpConn.SetNoDelay(true)
		tcpConn.SetLinger(0)
	}
}

var copyBufPool = sync.Pool{
	New: func() any {
		// 32KB buffer for optimal TCP streaming and memory usage
		b := make([]byte, 32*1024)
		return &b
	},
}

// ListenReusable creates a TCP listener that automatically asserts SO_REUSEADDR.
// This eliminates OS-level TIME_WAIT collisions when rapidly re-binding local ports.
func ListenReusable(ctx context.Context, network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.Listen(ctx, network, address)
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
