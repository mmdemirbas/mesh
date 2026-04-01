package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// ansiEscape matches ANSI CSI escape sequences (colors, cursor movement, etc.).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// buildAdminMux returns the HTTP handler for the local admin server.
// All endpoints are read-only and served on localhost only.
func buildAdminMux(ring *logRing) *http.ServeMux {
	mux := http.NewServeMux()

	// GET / — JSON state snapshot; kept for backward compat with the status command.
	// GET /api/state — same, versioned alias.
	stateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.Global.Snapshot())
	})
	mux.Handle("/", stateHandler)
	mux.Handle("/api/state", stateHandler)

	// GET /api/logs — recent log lines as a JSON string array, ANSI codes stripped.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		lines := ring.Lines()
		plain := make([]string, len(lines))
		for i, l := range lines {
			plain[i] = ansiEscape.ReplaceAllString(l, "")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plain)
	})

	// GET /metrics — Prometheus text format. All data is derived from existing
	// atomic counters and state snapshots; no additional instrumentation needed.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snap := state.Global.Snapshot()
		metrics := state.Global.SnapshotMetrics()
		authFails := tunnel.SnapshotAuthFailures()
		now := time.Now().UnixNano()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder

		// mesh_component_up
		b.WriteString("# HELP mesh_component_up Whether the component is up (1) or down (0).\n")
		b.WriteString("# TYPE mesh_component_up gauge\n")
		for key, comp := range snap {
			up := 0
			switch comp.Status {
			case state.Listening, state.Connected:
				up = 1
			}
			fmt.Fprintf(&b, "mesh_component_up{type=%q,id=%q,status=%q} %d\n",
				comp.Type, comp.ID, string(comp.Status), up)
			_ = key
		}

		// mesh_bytes_tx_total, mesh_bytes_rx_total, mesh_active_streams, mesh_uptime_seconds
		b.WriteString("# HELP mesh_bytes_tx_total Total bytes transmitted per component.\n")
		b.WriteString("# TYPE mesh_bytes_tx_total counter\n")
		for key, comp := range snap {
			if m, ok := metrics[key]; ok {
				fmt.Fprintf(&b, "mesh_bytes_tx_total{type=%q,id=%q} %d\n", comp.Type, comp.ID, m.BytesTx.Load())
			}
		}

		b.WriteString("# HELP mesh_bytes_rx_total Total bytes received per component.\n")
		b.WriteString("# TYPE mesh_bytes_rx_total counter\n")
		for key, comp := range snap {
			if m, ok := metrics[key]; ok {
				fmt.Fprintf(&b, "mesh_bytes_rx_total{type=%q,id=%q} %d\n", comp.Type, comp.ID, m.BytesRx.Load())
			}
		}

		b.WriteString("# HELP mesh_active_streams Current active streams per component.\n")
		b.WriteString("# TYPE mesh_active_streams gauge\n")
		for key, comp := range snap {
			if m, ok := metrics[key]; ok {
				fmt.Fprintf(&b, "mesh_active_streams{type=%q,id=%q} %d\n", comp.Type, comp.ID, m.Streams.Load())
			}
		}

		b.WriteString("# HELP mesh_uptime_seconds Seconds since the component last (re)connected.\n")
		b.WriteString("# TYPE mesh_uptime_seconds gauge\n")
		for key, comp := range snap {
			if m, ok := metrics[key]; ok {
				if st := m.StartTime.Load(); st != 0 {
					uptimeSec := float64(now-st) / 1e9
					fmt.Fprintf(&b, "mesh_uptime_seconds{type=%q,id=%q} %.3f\n", comp.Type, comp.ID, uptimeSec)
				}
			}
		}

		// mesh_auth_failures_total
		b.WriteString("# HELP mesh_auth_failures_total Cumulative SSH auth rejections by remote IP.\n")
		b.WriteString("# TYPE mesh_auth_failures_total counter\n")
		for ip, count := range authFails {
			fmt.Fprintf(&b, "mesh_auth_failures_total{remote_ip=%q} %d\n", ip, count)
		}

		_, _ = fmt.Fprint(w, b.String())
	})

	return mux
}
