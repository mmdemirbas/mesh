package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/filesync"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// buildAdminMux returns the HTTP handler for the local admin server.
// All endpoints are read-only and served on localhost only.
func buildAdminMux(ring *logRing, logFilePath string) *http.ServeMux {
	mux := http.NewServeMux()

	// Metrics cache: regenerated at most once per 5 seconds.
	var (
		mCacheMu   sync.Mutex
		mCacheBody string
		mCacheTime time.Time
	)

	// GET /healthz — health check. Always returns 200 if the process is up.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})

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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ring.PlainLines()) // write error: headers already sent, nothing to do
	})

	// GET /api/logs/file — full log file with optional offset/limit.
	// Query params: offset (byte offset, default 0), limit (max bytes, default 1MB).
	mux.HandleFunc("/api/logs/file", func(w http.ResponseWriter, r *http.Request) {
		if logFilePath == "" {
			http.Error(w, "log file not available (non-dashboard mode)", http.StatusNotFound)
			return
		}

		offset := int64(0)
		limit := int64(1024 * 1024) // 1 MB default

		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				offset = n
			}
		}
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				limit = n
			}
		}

		f, err := os.Open(logFilePath) //nolint:gosec // G304: path from internal config, not user input
		if err != nil {
			http.Error(w, "open log file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = f.Close() }()

		info, err := f.Stat()
		if err != nil {
			http.Error(w, "stat log file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Log-Size", strconv.FormatInt(info.Size(), 10))

		if offset > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				http.Error(w, "seek: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		_, _ = io.Copy(w, io.LimitReader(f, limit))
	})

	// GET /api/metrics — Prometheus text format. All data is derived from existing
	// atomic counters and state snapshots; no additional instrumentation needed.
	// Response is cached for 5 seconds to avoid repeated snapshot + openFDCount work.
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		const cacheTTL = 5 * time.Second

		// Hold the lock for the entire handler to prevent duplicate computation
		// when concurrent requests arrive after cache expiry.
		mCacheMu.Lock()
		now := time.Now()
		if now.Sub(mCacheTime) < cacheTTL {
			body := mCacheBody
			mCacheMu.Unlock()
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = io.WriteString(w, body)
			return
		}

		// SnapshotFull takes components and metrics under the same lock to avoid
		// cardinality divergence. Auth failures are snapshot separately; a brief
		// divergence there is harmless for Prometheus.
		full := state.Global.SnapshotFull()
		snap, metrics := full.Components, full.Metrics
		authFails := tunnel.SnapshotAuthFailures()
		nowNano := now.UnixNano()

		var b strings.Builder

		// mesh_component_up — also collect per-component metrics in a single pass.
		b.WriteString("# HELP mesh_component_up Whether the component is up (1) or down (0).\n")
		b.WriteString("# TYPE mesh_component_up gauge\n")

		type compMetric struct {
			compType, id string
			tx, rx       int64
			streams      int32
			uptime       float64
		}
		var cms []compMetric

		for key, comp := range snap {
			up := 0
			switch comp.Status {
			case state.Listening, state.Connected:
				up = 1
			}
			fmt.Fprintf(&b, "mesh_component_up{type=%q,id=%q,status=%q} %d\n",
				comp.Type, comp.ID, string(comp.Status), up)

			if m, ok := metrics[key]; ok {
				cm := compMetric{compType: comp.Type, id: comp.ID,
					tx: m.BytesTx.Load(), rx: m.BytesRx.Load(), streams: m.Streams.Load()}
				if st := m.StartTime.Load(); st != 0 {
					cm.uptime = float64(nowNano-st) / 1e9
				}
				cms = append(cms, cm)
			}
		}

		// Write collected per-component metrics grouped by family.
		b.WriteString("# HELP mesh_bytes_tx_total Total bytes transmitted per component.\n")
		b.WriteString("# TYPE mesh_bytes_tx_total counter\n")
		for _, cm := range cms {
			fmt.Fprintf(&b, "mesh_bytes_tx_total{type=%q,id=%q} %d\n", cm.compType, cm.id, cm.tx)
		}

		b.WriteString("# HELP mesh_bytes_rx_total Total bytes received per component.\n")
		b.WriteString("# TYPE mesh_bytes_rx_total counter\n")
		for _, cm := range cms {
			fmt.Fprintf(&b, "mesh_bytes_rx_total{type=%q,id=%q} %d\n", cm.compType, cm.id, cm.rx)
		}

		b.WriteString("# HELP mesh_active_streams Current active streams per component.\n")
		b.WriteString("# TYPE mesh_active_streams gauge\n")
		for _, cm := range cms {
			fmt.Fprintf(&b, "mesh_active_streams{type=%q,id=%q} %d\n", cm.compType, cm.id, cm.streams)
		}

		b.WriteString("# HELP mesh_uptime_seconds Seconds since the component last (re)connected.\n")
		b.WriteString("# TYPE mesh_uptime_seconds gauge\n")
		for _, cm := range cms {
			if cm.uptime > 0 {
				fmt.Fprintf(&b, "mesh_uptime_seconds{type=%q,id=%q} %.3f\n", cm.compType, cm.id, cm.uptime)
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

		body := b.String()
		mCacheBody = body
		mCacheTime = now
		mCacheMu.Unlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, body)
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
