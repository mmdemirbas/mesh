package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/filesync"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// ansiEscape matches ANSI CSI escape sequences (colors, cursor movement, etc.).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// buildAdminMux returns the HTTP handler for the local admin server.
// All endpoints are read-only and served on localhost only.
func buildAdminMux(ring *logRing) *http.ServeMux {
	mux := http.NewServeMux()

	// GET / — redirect to web dashboard.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})

	// GET /api/state — JSON state snapshot.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.Global.Snapshot()) // write error: headers already sent, nothing to do
	})

	// GET /api/logs — recent log lines as a JSON string array, ANSI codes stripped.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		lines := ring.Lines()
		plain := make([]string, len(lines))
		for i, l := range lines {
			plain[i] = ansiEscape.ReplaceAllString(l, "")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plain) // write error: headers already sent, nothing to do
	})

	// GET /api/metrics — Prometheus text format. All data is derived from existing
	// atomic counters and state snapshots; no additional instrumentation needed.
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		// SnapshotFull takes components and metrics under the same lock to avoid
		// cardinality divergence. Auth failures are snapshot separately; a brief
		// divergence there is harmless for Prometheus.
		full := state.Global.SnapshotFull()
		snap, metrics := full.Components, full.Metrics
		authFails := tunnel.SnapshotAuthFailures()
		now := time.Now().UnixNano()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder

		// mesh_component_up
		b.WriteString("# HELP mesh_component_up Whether the component is up (1) or down (0).\n")
		b.WriteString("# TYPE mesh_component_up gauge\n")
		for _, comp := range snap {
			up := 0
			switch comp.Status {
			case state.Listening, state.Connected:
				up = 1
			}
			fmt.Fprintf(&b, "mesh_component_up{type=%q,id=%q,status=%q} %d\n",
				comp.Type, comp.ID, string(comp.Status), up)
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

		// mesh_process_goroutines
		b.WriteString("# HELP mesh_process_goroutines Current number of goroutines.\n")
		b.WriteString("# TYPE mesh_process_goroutines gauge\n")
		fmt.Fprintf(&b, "mesh_process_goroutines %d\n", runtime.NumGoroutine())

		// mesh_process_open_fds (omitted on platforms where counting is unavailable)
		if fds := openFDCount(); fds >= 0 {
			b.WriteString("# HELP mesh_process_open_fds Current number of open file descriptors.\n")
			b.WriteString("# TYPE mesh_process_open_fds gauge\n")
			fmt.Fprintf(&b, "mesh_process_open_fds %d\n", fds)
		}

		// mesh_state_components
		b.WriteString("# HELP mesh_state_components Number of tracked components in the state map.\n")
		b.WriteString("# TYPE mesh_state_components gauge\n")
		fmt.Fprintf(&b, "mesh_state_components %d\n", len(snap))

		// mesh_state_metrics
		b.WriteString("# HELP mesh_state_metrics Number of metrics entries in the state map.\n")
		b.WriteString("# TYPE mesh_state_metrics gauge\n")
		fmt.Fprintf(&b, "mesh_state_metrics %d\n", len(metrics))

		_, _ = fmt.Fprint(w, b.String()) // write error: headers already sent, nothing to do
	})

	// GET /api/filesync/folders — filesync folder statuses.
	mux.HandleFunc("/api/filesync/folders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		folders := filesync.GetFolderStatuses()
		if folders == nil {
			folders = []filesync.FolderStatus{}
		}
		_ = json.NewEncoder(w).Encode(folders)
	})

	// GET /api/filesync/conflicts — list conflict files.
	mux.HandleFunc("/api/filesync/conflicts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		conflicts := filesync.GetConflicts()
		if conflicts == nil {
			conflicts = []filesync.ConflictInfo{}
		}
		_ = json.NewEncoder(w).Encode(conflicts)
	})

	// GET /api/clipsync/activity — recent clipboard sync activities.
	mux.HandleFunc("/api/clipsync/activity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		activities := clipsync.GetActivities()
		if activities == nil {
			activities = []clipsync.ClipActivity{}
		}
		_ = json.NewEncoder(w).Encode(activities)
	})

	// GET /ui, /ui/filesync, /ui/logs, /ui/metrics, /ui/api — unified SPA dashboard.
	// The tab parameter selects the initial view.
	uiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, adminUI)
	})
	mux.Handle("/ui", uiHandler)
	mux.Handle("/ui/clipsync", uiHandler)
	mux.Handle("/ui/filesync", uiHandler)
	mux.Handle("/ui/logs", uiHandler)
	mux.Handle("/ui/metrics", uiHandler)
	mux.Handle("/ui/api", uiHandler)
	mux.Handle("/ui/debug", uiHandler)

	// pprof endpoints for runtime profiling (CPU, memory, goroutines).
	// Only accessible on localhost via the admin server.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

// adminUI is defined in admin_ui.go.
