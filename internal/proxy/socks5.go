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
)

// ServeSocks accepts connections on listener and handles SOCKS5 for each.
func ServeSocks(ctx context.Context, listener net.Listener, dialer func(string, string) (net.Conn, error), log *slog.Logger) {
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
		go handleSocks5(conn, dialer, log)
	}
}

// handleSocks5 handles a single SOCKS5 connection.
func handleSocks5(conn net.Conn, dialer func(string, string) (net.Conn, error), log *slog.Logger) {
	defer conn.Close()

	// Set a deadline for the SOCKS5 handshake to prevent slowloris attacks
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if dialer == nil {
		dialer = func(network, addr string) (net.Conn, error) {
			return net.Dial(network, addr)
		}
	}

	buf := make([]byte, 258)

	// Greeting
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	if buf[0] != 0x05 {
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nMethods]); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // No auth
		return
	}

	// Request
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}
	if buf[0] != 0x05 || buf[1] != 0x01 || buf[2] != 0x00 { // Only CONNECT, RSV must be 0x00
		_ = socksReply(conn, 0x07)
		return
	}

	var destAddr string
	switch buf[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return
		}
		destAddr = net.IP(buf[:4]).String()
	case 0x03: // Domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:n]); err != nil {
			return
		}
		destAddr = string(buf[:n])
	case 0x04: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			return
		}
		destAddr = net.IP(buf[:16]).String()
	default:
		_ = socksReply(conn, 0x08)
		return
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])
	target := fmt.Sprintf("%s:%d", destAddr, port)

	remote, err := dialer("tcp", target)
	if err != nil {
		log.Debug("SOCKS connect failed", "target", target, "error", err)
		_ = socksReply(conn, 0x05)
		return
	}
	defer remote.Close()

	if err := socksReply(conn, 0x00); err != nil {
		return
	}
	// Clear handshake deadline before entering data relay
	_ = conn.SetDeadline(time.Time{})
	netutil.BiCopy(conn, remote)
}

func socksReply(conn net.Conn, status byte) error {
	_, err := conn.Write([]byte{0x05, status, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}
