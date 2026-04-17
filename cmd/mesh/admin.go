package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmdemirbas/mesh/internal/clipsync"
	"github.com/mmdemirbas/mesh/internal/filesync"
	"github.com/mmdemirbas/mesh/internal/gateway"
	"github.com/mmdemirbas/mesh/internal/state"
	"github.com/mmdemirbas/mesh/internal/tunnel"
)

// gatewayAuditResponse is the JSON shape returned by GET /api/gateway/audit.
type gatewayAuditResponse struct {
	Gateway   string            `json:"gateway"`
	Dir       string            `json:"dir"`
	File      string            `json:"file,omitempty"`
	FileSize  int64             `json:"file_size,omitempty"`
	Rows      []json.RawMessage `json:"rows"`
	Error     string            `json:"error,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// auditResponseByteCap bounds the total /api/gateway/audit response body so
// one huge audit row cannot wedge the browser or this process. 2000 rows of
// typical telemetry are well under 16 MB; 32 MB gives headroom for occasional
// large tool-result blobs while still capping worst-case transfer and parse
// time in the UI.
const auditResponseByteCap = 32 * 1024 * 1024

// perRowByteCap bounds a single audit row before it is emitted. Rows past
// this size are replaced with a stub {t, id, run, truncated:true, size} so
// the UI can still list the entry and the user can drill in via the pair
// endpoint if they really need the full payload.
const perRowByteCap = 256 * 1024

// capAuditRows enforces per-row and cumulative byte caps. Oversized rows are
// replaced with a truncation stub that preserves the identity fields (t, id,
// run, ts) so the UI renders a placeholder entry. Returns the clipped slice,
// whether any clipping happened, and the remaining byte budget.
func capAuditRows(rows []json.RawMessage, budget int) ([]json.RawMessage, bool, int) {
	truncated := false
	out := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if budget <= 0 {
			truncated = true
			break
		}
		if len(row) > perRowByteCap {
			stub := auditTruncationStub(row)
			if len(stub) > budget {
				truncated = true
				break
			}
			out = append(out, stub)
			budget -= len(stub)
			truncated = true
			continue
		}
		if len(row) > budget {
			truncated = true
			break
		}
		out = append(out, row)
		budget -= len(row)
	}
	return out, truncated, budget
}

// auditTruncationStub extracts the identity fields from an oversized row
// without re-parsing the whole payload, so the UI can show the row as
// truncated without paying the decode cost of the original blob.
func auditTruncationStub(row json.RawMessage) json.RawMessage {
	var hdr struct {
		T    string `json:"t"`
		ID   uint64 `json:"id,omitempty"`
		Run  string `json:"run,omitempty"`
		TS   string `json:"ts,omitempty"`
		Size int    `json:"size"`
	}
	_ = json.Unmarshal(row, &hdr)
	hdr.Size = len(row)
	b, err := json.Marshal(struct {
		T         string `json:"t"`
		ID        uint64 `json:"id,omitempty"`
		Run       string `json:"run,omitempty"`
		TS        string `json:"ts,omitempty"`
		Size      int    `json:"size"`
		Truncated bool   `json:"truncated"`
	}{hdr.T, hdr.ID, hdr.Run, hdr.TS, hdr.Size, true})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return b
}

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
			compType, id                                                    string
			tx, rx                                                          int64
			streams                                                         int32
			tokensIn, tokensOut, tokensCacheRd, tokensCacheWr, tokensReason int64
			uptime                                                          float64
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
					tx: m.BytesTx.Load(), rx: m.BytesRx.Load(), streams: m.Streams.Load(),
					tokensIn: m.TokensIn.Load(), tokensOut: m.TokensOut.Load(),
					tokensCacheRd: m.TokensCacheRd.Load(), tokensCacheWr: m.TokensCacheWr.Load(),
					tokensReason: m.TokensReason.Load()}
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

		// Gateway-only token counters (zero on every other component type).
		b.WriteString("# HELP mesh_gateway_tokens_in_total Cumulative input tokens reported by upstream usage fields.\n")
		b.WriteString("# TYPE mesh_gateway_tokens_in_total counter\n")
		for _, cm := range cms {
			if cm.compType == "gateway" {
				fmt.Fprintf(&b, "mesh_gateway_tokens_in_total{id=%q} %d\n", cm.id, cm.tokensIn)
			}
		}
		b.WriteString("# HELP mesh_gateway_tokens_out_total Cumulative output tokens reported by upstream usage fields.\n")
		b.WriteString("# TYPE mesh_gateway_tokens_out_total counter\n")
		for _, cm := range cms {
			if cm.compType == "gateway" {
				fmt.Fprintf(&b, "mesh_gateway_tokens_out_total{id=%q} %d\n", cm.id, cm.tokensOut)
			}
		}
		b.WriteString("# HELP mesh_gateway_tokens_cache_read_total Cumulative input tokens served from prompt cache.\n")
		b.WriteString("# TYPE mesh_gateway_tokens_cache_read_total counter\n")
		for _, cm := range cms {
			if cm.compType == "gateway" {
				fmt.Fprintf(&b, "mesh_gateway_tokens_cache_read_total{id=%q} %d\n", cm.id, cm.tokensCacheRd)
			}
		}
		b.WriteString("# HELP mesh_gateway_tokens_cache_creation_total Cumulative input tokens written to prompt cache (Anthropic).\n")
		b.WriteString("# TYPE mesh_gateway_tokens_cache_creation_total counter\n")
		for _, cm := range cms {
			if cm.compType == "gateway" {
				fmt.Fprintf(&b, "mesh_gateway_tokens_cache_creation_total{id=%q} %d\n", cm.id, cm.tokensCacheWr)
			}
		}
		b.WriteString("# HELP mesh_gateway_tokens_reasoning_total Cumulative reasoning tokens (OpenAI o-series).\n")
		b.WriteString("# TYPE mesh_gateway_tokens_reasoning_total counter\n")
		for _, cm := range cms {
			if cm.compType == "gateway" {
				fmt.Fprintf(&b, "mesh_gateway_tokens_reasoning_total{id=%q} %d\n", cm.id, cm.tokensReason)
			}
		}

		// mesh_auth_failures_total
		b.WriteString("# HELP mesh_auth_failures_total Cumulative SSH auth rejections by remote IP.\n")
		b.WriteString("# TYPE mesh_auth_failures_total counter\n")
		for ip, count := range authFails {
			fmt.Fprintf(&b, "mesh_auth_failures_total{remote_ip=%q} %d\n", ip, count)
		}

		// mesh_filesync_* per-folder metrics
		fsMetrics := filesync.GetFolderMetrics()
		if len(fsMetrics) > 0 {
			b.WriteString("# HELP mesh_filesync_peer_syncs_total Completed peer sync round trips per folder.\n")
			b.WriteString("# TYPE mesh_filesync_peer_syncs_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_peer_syncs_total{folder=%q} %d\n", m.FolderID, m.PeerSyncs)
			}
			b.WriteString("# HELP mesh_filesync_files_downloaded_total Files downloaded per folder.\n")
			b.WriteString("# TYPE mesh_filesync_files_downloaded_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_files_downloaded_total{folder=%q} %d\n", m.FolderID, m.FilesDownloaded)
			}
			b.WriteString("# HELP mesh_filesync_files_deleted_total Files deleted by remote tombstones per folder.\n")
			b.WriteString("# TYPE mesh_filesync_files_deleted_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_files_deleted_total{folder=%q} %d\n", m.FolderID, m.FilesDeleted)
			}
			b.WriteString("# HELP mesh_filesync_files_conflicted_total Conflict resolutions per folder.\n")
			b.WriteString("# TYPE mesh_filesync_files_conflicted_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_files_conflicted_total{folder=%q} %d\n", m.FolderID, m.FilesConflicted)
			}
			b.WriteString("# HELP mesh_filesync_sync_errors_total Per-file sync failures per folder.\n")
			b.WriteString("# TYPE mesh_filesync_sync_errors_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_sync_errors_total{folder=%q} %d\n", m.FolderID, m.SyncErrors)
			}
			b.WriteString("# HELP mesh_filesync_bytes_downloaded_total Bytes downloaded per folder.\n")
			b.WriteString("# TYPE mesh_filesync_bytes_downloaded_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_bytes_downloaded_total{folder=%q} %d\n", m.FolderID, m.BytesDownloaded)
			}
			b.WriteString("# HELP mesh_filesync_bytes_uploaded_total Bytes served to peers per folder.\n")
			b.WriteString("# TYPE mesh_filesync_bytes_uploaded_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_bytes_uploaded_total{folder=%q} %d\n", m.FolderID, m.BytesUploaded)
			}
			b.WriteString("# HELP mesh_filesync_index_exchanges_total Index exchange round trips per folder.\n")
			b.WriteString("# TYPE mesh_filesync_index_exchanges_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_index_exchanges_total{folder=%q} %d\n", m.FolderID, m.IndexExchanges)
			}
			b.WriteString("# HELP mesh_filesync_scans_total Scan cycles completed per folder.\n")
			b.WriteString("# TYPE mesh_filesync_scans_total counter\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_scans_total{folder=%q} %d\n", m.FolderID, m.ScanCount)
			}
			b.WriteString("# HELP mesh_filesync_scan_duration_seconds Last scan duration per folder.\n")
			b.WriteString("# TYPE mesh_filesync_scan_duration_seconds gauge\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_scan_duration_seconds{folder=%q} %.6f\n", m.FolderID, float64(m.ScanDurationNS)/1e9)
			}
			b.WriteString("# HELP mesh_filesync_peer_sync_duration_seconds Last peer sync duration per folder (last writer wins).\n")
			b.WriteString("# TYPE mesh_filesync_peer_sync_duration_seconds gauge\n")
			for _, m := range fsMetrics {
				fmt.Fprintf(&b, "mesh_filesync_peer_sync_duration_seconds{folder=%q} %.6f\n", m.FolderID, float64(m.PeerSyncNS)/1e9)
			}
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

	// GET /api/filesync/conflicts/diff — compute diff for a single conflict file.
	mux.HandleFunc("GET /api/filesync/conflicts/diff", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		folderID := q.Get("folder")
		conflictPath := q.Get("path")
		if folderID == "" || conflictPath == "" {
			http.Error(w, `{"error":"missing folder or path"}`, http.StatusBadRequest)
			return
		}
		folderRoot, ok := filesync.GetFolderPath(folderID)
		if !ok {
			http.Error(w, `{"error":"unknown folder"}`, http.StatusNotFound)
			return
		}
		diff, err := filesync.ComputeConflictDiff(folderRoot, conflictPath)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(diff)
	})

	// GET /api/filesync/activity — recent filesync activities.
	mux.HandleFunc("/api/filesync/activity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		activities := filesync.GetActivities()
		if activities == nil {
			activities = []filesync.SyncActivity{}
		}
		_ = json.NewEncoder(w).Encode(activities)
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
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprint(w, adminUI)
	})
	mux.Handle("/ui", uiHandler)
	mux.Handle("/ui/clipsync", uiHandler)
	mux.Handle("/ui/filesync", uiHandler)
	mux.Handle("/ui/gateway", uiHandler)
	mux.Handle("/ui/logs", uiHandler)
	mux.Handle("/ui/metrics", uiHandler)
	mux.Handle("/ui/api", uiHandler)
	mux.Handle("/ui/debug", uiHandler)

	// GET /api/gateway/audit — recent audit rows for one or all gateways.
	// Query params:
	//   gateway=<name>     filter to a specific gateway (default: all).
	//   limit=<N>          max rows per gateway (default: 200, capped at 2000).
	//   session=<hex>      only pairs whose request row carries this session_id.
	//   model=<name>       only pairs whose request row uses this model.
	//   outcome=<token>    only pairs whose response row has this outcome.
	//   since=<rfc3339>    only pairs whose request ts >= since.
	//   until=<rfc3339>    only pairs whose request ts <= until.
	//   min_tokens=<N>     only pairs whose response total tokens >= N.
	mux.HandleFunc("GET /api/gateway/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		filter := q.Get("gateway")
		limit := 200
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				if n > 2000 {
					n = 2000
				}
				limit = n
			}
		}
		af := auditFilter{
			session: q.Get("session"),
			model:   q.Get("model"),
			outcome: q.Get("outcome"),
		}
		if v := q.Get("since"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				af.since = t
			}
		}
		if v := q.Get("until"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				af.until = t
			}
		}
		if v := q.Get("min_tokens"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				af.minTokens = n
			}
		}

		dirs := gateway.AuditDirs()
		out := make([]gatewayAuditResponse, 0, len(dirs))
		names := make([]string, 0, len(dirs))
		for name := range dirs {
			if filter != "" && filter != name {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		// Track cumulative response size so one gateway's rows cannot starve
		// the others and the total payload stays bounded. Past the cap, later
		// gateways flip Truncated=true and ship zero rows.
		budget := auditResponseByteCap
		for _, name := range names {
			rows, file, size, err := queryAuditRows(dirs[name], af, limit)
			entry := gatewayAuditResponse{Gateway: name, Dir: dirs[name], File: file, FileSize: size}
			if err != nil {
				entry.Error = err.Error()
			}
			entry.Rows, entry.Truncated, budget = capAuditRows(rows, budget)
			out = append(out, entry)
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	// GET /api/gateway/audit/stats — aggregated counters for one gateway.
	// Required: gateway=<name>. Optional: window (1h|24h|7d|30d|all|<dur>),
	// bucket (minute|hour|day), session, model, since, until.
	mux.HandleFunc("GET /api/gateway/audit/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		name := q.Get("gateway")
		if name == "" {
			http.Error(w, "missing gateway", http.StatusBadRequest)
			return
		}
		dirs := gateway.AuditDirs()
		dir, ok := dirs[name]
		if !ok {
			http.Error(w, "unknown gateway", http.StatusNotFound)
			return
		}
		now := time.Now()
		since, until := parseWindowParam(q.Get("window"), now)
		// Explicit since/until override the canned window.
		if v := q.Get("since"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				since = t
			}
		}
		if v := q.Get("until"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				until = t
			}
		}
		f := statsFilter{
			session: q.Get("session"),
			model:   q.Get("model"),
			since:   since,
			until:   until,
			bucket:  parseBucketParam(q.Get("bucket")),
		}
		stats, err := computeAuditStats(dir, f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(stats)
	})

	// GET /api/gateway/audit/pair — full request+response rows for one pair.
	// Required: gateway=<name>, id=<uint64>, run=<hex>.
	mux.HandleFunc("GET /api/gateway/audit/pair", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		name := q.Get("gateway")
		idStr := q.Get("id")
		run := q.Get("run")
		if name == "" || idStr == "" || run == "" {
			http.Error(w, "missing gateway, id, or run", http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			http.Error(w, "id must be uint64", http.StatusBadRequest)
			return
		}
		dirs := gateway.AuditDirs()
		dir, ok := dirs[name]
		if !ok {
			http.Error(w, "unknown gateway", http.StatusNotFound)
			return
		}
		req, resp, err := findAuditPair(dir, id, run)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{
			"request":  req,
			"response": resp,
		})
	})

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
