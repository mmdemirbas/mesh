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

		_, _ = fmt.Fprint(w, b.String())
	})

	// GET /ui — browser dashboard; polls /api/state and /api/logs every second.
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, adminUI)
	})

	return mux
}

// adminUI is the single-page web dashboard served at GET /ui.
// Polls /api/state and /api/logs every second via vanilla JS. No external deps.
const adminUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>mesh</title>
<style>
*{box-sizing:border-box}
body{font-family:monospace;background:#1a1a1a;color:#e0e0e0;margin:16px;font-size:13px}
h1{font-size:1rem;margin:0 0 12px;color:#fff}
h2{font-size:.8rem;color:#666;margin:16px 0 4px;text-transform:uppercase;letter-spacing:.05em}
table{border-collapse:collapse;width:100%}
th{text-align:left;padding:3px 16px 3px 0;border-bottom:1px solid #333;color:#666;font-size:.8rem}
td{padding:3px 16px 3px 0;white-space:nowrap}
.listening,.connected{color:#4caf50}
.connecting,.retrying{color:#ffc107}
.failed{color:#f44336}
.starting{color:#9e9e9e}
#logs{background:#111;border:1px solid #2a2a2a;padding:8px 10px;max-height:200px;overflow-y:auto;line-height:1.5}
.ll{white-space:pre-wrap;word-break:break-all}
#ts{font-size:.75rem;color:#444;margin-top:8px}
</style>
</head>
<body>
<h1>mesh</h1>
<div id="state"><em style="color:#555">loading…</em></div>
<h2>logs</h2>
<div id="logs"></div>
<div id="ts"></div>
<script>
async function tick(){
  try{
    const[sr,lr]=await Promise.all([fetch('/api/state'),fetch('/api/logs')]);
    renderState(await sr.json());
    renderLogs(await lr.json());
    document.getElementById('ts').textContent='updated '+new Date().toLocaleTimeString();
  }catch(e){
    document.getElementById('ts').textContent='error: '+e.message;
  }
}
function renderState(s){
  const rows=Object.values(s).sort((a,b)=>(a.type+a.id).localeCompare(b.type+b.id));
  if(!rows.length){document.getElementById('state').innerHTML='<em style="color:#555">no components</em>';return}
  let h='<table><tr><th>type</th><th>id</th><th>status</th><th>detail</th></tr>';
  for(const c of rows){
    const cls=c.status.replace(/\W/g,'');
    const detail=c.message||c.peer_addr||c.bound_addr||'';
    h+='<tr><td>'+x(c.type)+'</td><td>'+x(c.id)+'</td><td class="'+cls+'">'+x(c.status)+'</td><td style="color:#aaa">'+x(detail)+'</td></tr>';
  }
  document.getElementById('state').innerHTML=h+'</table>';
}
function renderLogs(lines){
  const el=document.getElementById('logs');
  if(!lines.length){el.innerHTML='<span style="color:#555">no logs yet</span>';return}
  el.innerHTML=lines.map(l=>'<div class="ll">'+x(l)+'</div>').join('');
  el.scrollTop=el.scrollHeight;
}
function x(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}
tick();setInterval(tick,1000);
</script>
</body>
</html>`
