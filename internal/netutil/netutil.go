package netutil

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ApplyTCPKeepAlive enables TCP keep-alive on the connection if it is a *net.TCPConn.
// It explicitly asserts TCP_NODELAY to ensure interactive sessions don't buffer,
// and forces SetLinger(0) to ensure sockets close immediately with a RST packet
// instead of lingering politely in TIME_WAIT, keeping proxy listener ports clean.
// Trade-off: RST on close may discard unsent data on real networks. Acceptable here
// because this is applied to proxy connections, not user-facing streams.
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

// countingWriter wraps an io.Writer and atomically counts bytes written.
type countingWriter struct {
	w       io.Writer
	counter *atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.counter.Add(int64(n))
	return n, err
}

// CountedBiCopy is like BiCopy but counts bytes transferred through atomic counters.
// tx counts bytes from a→b, rx counts bytes from b→a.
func CountedBiCopy(a, b io.ReadWriteCloser, tx, rx *atomic.Int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		_, _ = io.CopyBuffer(&countingWriter{w: a, counter: tx}, b, *bufPtr)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		_, _ = io.CopyBuffer(&countingWriter{w: b, counter: rx}, a, *bufPtr)
		if c, ok := b.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	wg.Wait()
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
