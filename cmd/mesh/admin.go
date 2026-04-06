package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

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

	// GET / — JSON state snapshot; kept for backward compat with the status command.
	// GET /api/state — same, versioned alias.
	stateHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.Global.Snapshot()) // write error: headers already sent, nothing to do
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
		_ = json.NewEncoder(w).Encode(plain) // write error: headers already sent, nothing to do
	})

	// GET /metrics — Prometheus text format. All data is derived from existing
	// atomic counters and state snapshots; no additional instrumentation needed.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
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

	// GET /ui — browser dashboard; polls /api/state and /api/logs every second.
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, adminUI) // write error: headers already sent, nothing to do
	})

	// GET /ui/filesync — filesync web dashboard.
	mux.HandleFunc("/ui/filesync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, filesyncUI)
	})

	return mux
}

// adminUI is the single-page web dashboard served at GET /ui.
// Polls /api/state and /api/logs every second via vanilla JS. No external deps.
var adminUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>mesh ` + version + `</title>
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
<h1>mesh <span style="color:#666;font-size:.8em">` + version + `</span></h1>
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

// filesyncUI is the web dashboard for filesync status, served at GET /ui/filesync.
const filesyncUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>mesh filesync</title>
<style>
body{font-family:monospace;background:#1a1a2e;color:#e0e0e0;margin:2em}
h1{color:#00d4aa;font-size:1.4em}
h2{color:#7ec8e3;font-size:1.1em;margin-top:1.5em}
table{border-collapse:collapse;width:100%;margin:.5em 0}
th,td{text-align:left;padding:.3em .8em;border-bottom:1px solid #333}
th{color:#888;font-weight:normal}
.ok{color:#4ade80}
.warn{color:#facc15}
.err{color:#f87171}
.conflict{background:#2a1a1a;color:#f87171}
#status{margin-bottom:1em;color:#888}
</style>
</head>
<body>
<h1>mesh filesync</h1>
<div id="status">loading...</div>
<h2>Folders</h2>
<table id="folders"><tr><th>ID</th><th>Path</th><th>Direction</th><th>Files</th><th>Peers</th></tr></table>
<h2>Conflicts</h2>
<div id="conflicts">none</div>
<script>
async function tick(){
  try{
    const[fr,cr]=await Promise.all([fetch('/api/filesync/folders'),fetch('/api/filesync/conflicts')]);
    const folders=await fr.json(),conflicts=await cr.json();
    document.getElementById('status').textContent='Updated: '+new Date().toLocaleTimeString();
    let ft='<tr><th>ID</th><th>Path</th><th>Direction</th><th>Files</th><th>Peers</th></tr>';
    for(const f of folders){
      ft+='<tr><td>'+x(f.id)+'</td><td>'+x(f.path)+'</td><td>'+x(f.direction)+'</td><td>'+f.file_count+'</td><td>'+x((f.peers||[]).join(', '))+'</td></tr>';
    }
    document.getElementById('folders').innerHTML=ft;
    if(conflicts.length===0){
      document.getElementById('conflicts').innerHTML='<span class="ok">none</span>';
    }else{
      let ct='<table><tr><th>Folder</th><th>Path</th></tr>';
      for(const c of conflicts){ct+='<tr class="conflict"><td>'+x(c.folder_id)+'</td><td>'+x(c.path)+'</td></tr>';}
      ct+='</table>';
      document.getElementById('conflicts').innerHTML=ct;
    }
  }catch(e){document.getElementById('status').textContent='Error: '+e.message}
}
function x(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}
tick();setInterval(tick,2000);
</script>
</body>
</html>`
