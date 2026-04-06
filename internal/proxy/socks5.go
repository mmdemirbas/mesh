package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/mmdemirbas/mesh/internal/netutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

// ServeSocks accepts connections on listener and handles SOCKS5 for each.
func ServeSocks(ctx context.Context, listener net.Listener, dialer func(string, string) (net.Conn, error), log *slog.Logger, metrics *state.Metrics) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Debug("SOCKS accept error (transient)", "error", err)
			time.Sleep(50 * time.Millisecond) // backoff on transient errors
			continue
		}
		netutil.ApplyTCPKeepAlive(conn, 0)
		go handleSocks5(conn, dialer, log, metrics)
	}
}

// handleSocks5 handles a single SOCKS5 connection.
func handleSocks5(conn net.Conn, dialer func(string, string) (net.Conn, error), log *slog.Logger, metrics *state.Metrics) {
	defer func() { _ = conn.Close() }()

	if dialer == nil {
		dialer = net.Dial
	}

	target, err := socks5Handshake(conn)
	if err != nil {
		return
	}

	remote, err := dialer("tcp", target)
	if err != nil {
		log.Debug("SOCKS connect failed", "target", target, "error", err)
		_ = socksReply(conn, 0x05)
		return
	}
	defer func() { _ = remote.Close() }()

	if err := socksReply(conn, 0x00); err != nil {
		return
	}
	// Clear handshake deadline before entering data relay
	_ = conn.SetDeadline(time.Time{})
	if metrics != nil {
		metrics.Streams.Add(1)
		defer metrics.Streams.Add(-1)
		netutil.CountedBiCopy(conn, remote, &metrics.BytesTx, &metrics.BytesRx)
	} else {
		netutil.BiCopy(conn, remote)
	}
}

// socks5Handshake performs the SOCKS5 greeting and request exchange, returning
// the requested target address. It enforces a 30-second timeout using both
// SetDeadline (works on real TCP) and a context-based close (works on SSH channels
// where SetDeadline is a no-op).
func socks5Handshake(conn net.Conn) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer cancel()
	defer stop()

	buf := make([]byte, 258)

	// Greeting
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", err
	}
	if buf[0] != 0x05 {
		return "", fmt.Errorf("unsupported SOCKS version: %d", buf[0])
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nMethods]); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // No auth
		return "", err
	}

	// Request
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return "", err
	}
	if buf[0] != 0x05 || buf[1] != 0x01 || buf[2] != 0x00 { // Only CONNECT, RSV must be 0x00
		_ = socksReply(conn, 0x07)
		return "", fmt.Errorf("unsupported SOCKS command")
	}

	var destAddr string
	switch buf[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return "", err
		}
		destAddr = net.IP(buf[:4]).String()
	case 0x03: // Domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return "", err
		}
		n := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:n]); err != nil {
			return "", err
		}
		destAddr = string(buf[:n])
	case 0x04: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			return "", err
		}
		destAddr = net.IP(buf[:16]).String()
	default:
		_ = socksReply(conn, 0x08)
		return "", fmt.Errorf("unsupported address type: %d", buf[3])
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(buf[:2])
	return fmt.Sprintf("%s:%d", destAddr, port), nil
}

func socksReply(conn net.Conn, status byte) error {
	_, err := conn.Write([]byte{0x05, status, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}
