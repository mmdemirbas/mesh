package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// bufferedConn wraps a net.Conn with a buffered reader and implements CloseWrite.
type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ServeHTTPProxy accepts connections and handles HTTP CONNECT proxy requests.
// Each CONNECT request is forwarded either directly or through an upstream SOCKS5 proxy.
func ServeHTTPProxy(ctx context.Context, listener net.Listener, upstream string, log *slog.Logger) {
	dialer := func(addr string) (net.Conn, error) {
		if upstream != "" {
			return dialViaSocks5(net.Dial, upstream, addr)
		}
		return net.Dial("tcp", addr)
	}
	ServeHTTPProxyWithDialer(ctx, listener, dialer, log)
}

// ServeHTTPProxyWithDialer accepts connections and uses the provided dialer for upstream targets.
func ServeHTTPProxyWithDialer(ctx context.Context, listener net.Listener, dialer func(string) (net.Conn, error), log *slog.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Debug("HTTP proxy accept error (transient)", "error", err)
			time.Sleep(50 * time.Millisecond) // backoff on transient errors
			continue
		}
		ApplyTCPKeepAlive(conn)
		go handleHTTPProxy(conn, dialer, log)
	}
}

// handleHTTPProxy handles a single HTTP CONNECT proxy connection.
func handleHTTPProxy(conn net.Conn, dialer func(string) (net.Conn, error), log *slog.Logger) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	if req.Method != http.MethodConnect {
		resp := &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			ProtoMajor: 1, ProtoMinor: 1,
			Header: make(http.Header),
		}
		resp.Header.Set("Allow", "CONNECT")
		resp.Write(conn)
		return
	}

	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	remote, err := dialer(target)
	if err != nil {
		log.Debug("HTTP CONNECT failed", "target", target, "error", err)
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer remote.Close()

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		remote.Close()
		return
	}
	bc := &bufferedConn{
		Conn: conn,
		r:    io.MultiReader(br, conn),
	}
	BiCopy(bc, remote)
}

// dialViaSocks5 connects to target through a SOCKS5 proxy, using baseDialer to reach SOCKS.
func dialViaSocks5(baseDialer func(string, string) (net.Conn, error), socksAddr, target string) (net.Conn, error) {
	conn, err := baseDialer("tcp", socksAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial: %w", err)
	}

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil { // v5, 1 method, no auth
		conn.Close()
		return nil, fmt.Errorf("socks5 greeting write: %w", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		conn.Close()
		return nil, err
	}
	if buf[0] != 0x05 || buf[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5: server rejected no-auth method (got %#x %#x)", buf[0], buf[1])
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5: invalid port %q: %w", portStr, err)
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect write: %w", err)
	}

	resp := make([]byte, 10)
	if _, err := io.ReadFull(conn, resp[:4]); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5: connect failed (status %d)", resp[1])
	}

	switch resp[3] {
	case 0x01: // IPv4
		io.ReadFull(conn, resp[:4+2])
	case 0x03: // Domain
		io.ReadFull(conn, resp[:1])
		domain := make([]byte, resp[0]+2)
		io.ReadFull(conn, domain)
	case 0x04: // IPv6
		io.ReadFull(conn, resp[:16+2])
	}

	return conn, nil
}
