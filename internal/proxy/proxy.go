// Package proxy implements standalone proxies (SOCKS5, HTTP) and TCP relays.
package proxy

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// RunStandaloneProxies starts all standalone (always-on) SOCKS/HTTP proxies.
func RunStandaloneProxies(ctx context.Context, proxies []config.Proxy, log *slog.Logger) {
	for _, p := range proxies {
		p := p
		go func() {
			pLog := log.With("component", "proxy", "type", p.Type, "bind", p.Bind)
			ln, err := net.Listen("tcp", p.Bind)
			if err != nil {
				pLog.Error("Listen failed", "error", err)
				return
			}
			defer ln.Close()
			go func() { <-ctx.Done(); ln.Close() }()

			pLog.Info("Standalone proxy listening")

			switch p.Type {
			case "socks":
				tunnel.ServeSocks(ctx, ln, nil, pLog)
			case "http":
				tunnel.ServeHTTPProxy(ctx, ln, p.Upstream, pLog)
			}
		}()
	}
}

// RunStandaloneRelays starts raw TCP relays (e.g. replacing socat).
func RunStandaloneRelays(ctx context.Context, relays []config.Relay, log *slog.Logger) {
	for _, r := range relays {
		r := r
		go func() {
			rLog := log.With("component", "relay", "bind", r.Bind, "target", r.Target)
			ln, err := net.Listen("tcp", r.Bind)
			if err != nil {
				rLog.Error("Listen failed", "error", err)
				return
			}
			defer ln.Close()
			go func() { <-ctx.Done(); ln.Close() }()

			rLog.Info("TCP relay listening")

			for {
				conn, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}

				go func(c net.Conn) {
					defer c.Close()
					targetConn, err := net.DialTimeout("tcp", r.Target, 10*time.Second)
					if err != nil {
						rLog.Debug("Relay dial failed", "error", err)
						return
					}
					defer targetConn.Close()
					tunnel.BiCopy(c, targetConn)
				}(conn)
			}
		}()
	}
}
