// Package proxy implements standalone proxies (SOCKS5, HTTP) and TCP relays.
package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// RunStandaloneProxies starts all standalone (always-on) SOCKS/HTTP proxies.
// Each proxy goroutine is tracked via the provided WaitGroup.
func RunStandaloneProxies(ctx context.Context, proxies []config.Proxy, log *slog.Logger, wg *sync.WaitGroup) {
	for _, p := range proxies {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			pLog := log.With("component", "proxy", "type", p.Type, "bind", p.Bind)
			ln, err := net.Listen("tcp", p.Bind)
			if err != nil {
				pLog.Error("Listen failed", "error", err)
				return
			}
			defer ln.Close()
			stop := context.AfterFunc(ctx, func() { ln.Close() })
			defer stop()

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
// Each relay goroutine is tracked via the provided WaitGroup.
func RunStandaloneRelays(ctx context.Context, relays []config.Relay, log *slog.Logger, wg *sync.WaitGroup) {
	for _, r := range relays {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			rLog := log.With("component", "relay", "bind", r.Bind, "target", r.Target)
			ln, err := net.Listen("tcp", r.Bind)
			if err != nil {
				rLog.Error("Listen failed", "error", err)
				return
			}
			defer ln.Close()
			stop := context.AfterFunc(ctx, func() { ln.Close() })
			defer stop()

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
