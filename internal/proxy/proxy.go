// Package proxy implements standalone proxies (SOCKS5, HTTP) and TCP relays.
package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/config"
	"github.com/mmdemirbas/mesh/internal/netutil"
	"github.com/mmdemirbas/mesh/internal/state"
)

// RunStandaloneProxies starts all standalone (always-on) SOCKS/HTTP proxies.
// Each proxy goroutine is tracked via the provided WaitGroup.
func RunStandaloneProxies(ctx context.Context, proxies []config.Listener, log *slog.Logger, wg *sync.WaitGroup) {
	for _, p := range proxies {
		if p.Type != "socks" && p.Type != "http" {
			continue
		}
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			pLog := log.With("component", "proxy", "type", p.Type, "bind", p.Bind)

			state.Global.Update("proxy", p.Bind, state.Starting, "")
			ln, err := net.Listen("tcp", p.Bind)
			if err != nil {
				state.Global.Update("proxy", p.Bind, state.Failed, err.Error())
				pLog.Error("Listen failed", "error", err)
				return
			}
			defer func() { _ = ln.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
			defer stop()

			state.Global.Update("proxy", p.Bind, state.Listening, "")
			metrics := state.Global.GetMetrics("proxy", p.Bind)
			metrics.StartTime.Store(time.Now().UnixNano())
			pLog.Info("Standalone proxy listening")

			switch p.Type {
			case "socks":
				ServeSocks(ctx, ln, nil, pLog, metrics)
			case "http":
				ServeHTTPProxy(ctx, ln, p.Target, pLog, metrics)
			}
		}()
	}
}

// RunStandaloneRelays starts raw TCP relays (e.g. replacing socat).
// Each relay goroutine is tracked via the provided WaitGroup.
func RunStandaloneRelays(ctx context.Context, relays []config.Listener, log *slog.Logger, wg *sync.WaitGroup) {
	for _, r := range relays {
		if r.Type != "relay" {
			continue
		}
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			rLog := log.With("component", "relay", "bind", r.Bind, "target", r.Target)

			state.Global.Update("relay", r.Bind, state.Starting, "")
			ln, err := net.Listen("tcp", r.Bind)
			if err != nil {
				state.Global.Update("relay", r.Bind, state.Failed, err.Error())
				rLog.Error("Listen failed", "error", err)
				return
			}
			defer func() { _ = ln.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
			defer stop()

			state.Global.Update("relay", r.Bind, state.Listening, "")
			metrics := state.Global.GetMetrics("relay", r.Bind)
			metrics.StartTime.Store(time.Now().UnixNano())
			rLog.Info("TCP relay listening")

			for {
				conn, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					select {
					case <-time.After(50 * time.Millisecond):
					case <-ctx.Done():
						return
					}
					continue
				}
				netutil.ApplyTCPKeepAlive(conn, 0)

				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					targetConn, err := net.DialTimeout("tcp", r.Target, 10*time.Second)
					if err != nil {
						rLog.Debug("Relay dial failed", "error", err)
						return
					}
					netutil.ApplyTCPKeepAlive(targetConn, 0)
					defer func() { _ = targetConn.Close() }()
					metrics.Streams.Add(1)
					netutil.CountedBiCopy(c, targetConn, &metrics.BytesTx, &metrics.BytesRx)
					metrics.Streams.Add(-1)
				}(conn)
			}
		}()
	}
}
