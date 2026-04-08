package main

// adminUI is the unified single-page web dashboard served at /ui, /ui/filesync,
// /ui/logs, /ui/metrics, and /ui/api. Tab is selected from the URL path. Polls
// API endpoints every second. No external dependencies — vanilla JS + CSS only.
var adminUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>mesh ` + version + `</title>
<style>
:root {
  --bg: #0f1117;
  --bg-card: #161922;
  --bg-hover: #1c2030;
  --bg-input: #1c2030;
  --border: #2a2d3a;
  --border-light: #353849;
  --text: #e1e4ed;
  --text-dim: #8b8fa3;
  --text-muted: #5a5e72;
  --green: #34d399;
  --green-dim: #064e36;
  --yellow: #fbbf24;
  --yellow-dim: #573d08;
  --red: #f87171;
  --red-dim: #541a1a;
  --blue: #60a5fa;
  --cyan: #22d3ee;
  --purple: #a78bfa;
  --radius: 8px;
  --font: -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif;
  --mono: 'SF Mono', 'Cascadia Code', 'Fira Code', 'JetBrains Mono', Consolas, monospace;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: var(--font); background: var(--bg); color: var(--text); font-size: 14px; line-height: 1.5; }

/* Header */
.header {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 24px; border-bottom: 1px solid var(--border);
  background: var(--bg-card);
}
.header-left { display: flex; align-items: center; gap: 12px; }
.logo { font-weight: 700; font-size: 18px; color: var(--green); letter-spacing: -0.5px; }
.logo span { color: var(--text-muted); font-weight: 400; font-size: 12px; margin-left: 4px; }
.header-status { font-size: 12px; color: var(--text-muted); }

/* Tabs */
.tabs {
  display: flex; gap: 0; border-bottom: 1px solid var(--border);
  background: var(--bg-card); padding: 0 24px;
}
.tab {
  padding: 10px 20px; font-size: 13px; font-weight: 500;
  color: var(--text-dim); cursor: pointer; border-bottom: 2px solid transparent;
  transition: all 0.15s;
}
.tab:hover { color: var(--text); background: var(--bg-hover); }
.tab.active { color: var(--green); border-bottom-color: var(--green); }

/* Content */
.content { padding: 20px 24px; max-width: 1400px; }
.panel { display: none; }
.panel.active { display: block; }

/* Cards */
.card {
  background: var(--bg-card); border: 1px solid var(--border);
  border-radius: var(--radius); margin-bottom: 16px; overflow: hidden;
}
.card-header {
  padding: 12px 16px; border-bottom: 1px solid var(--border);
  display: flex; align-items: center; justify-content: space-between;
  font-size: 13px; font-weight: 600; color: var(--text-dim);
  text-transform: uppercase; letter-spacing: 0.5px;
}
.card-body { padding: 0; }
.card-body.padded { padding: 16px; }

/* Stat grid */
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin-bottom: 16px; }
.stat {
  background: var(--bg-card); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 16px;
}
.stat-label { font-size: 11px; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.5px; margin-bottom: 4px; }
.stat-value { font-size: 24px; font-weight: 700; font-family: var(--mono); }
.stat-sub { font-size: 11px; color: var(--text-dim); margin-top: 2px; }

/* Tables */
table { width: 100%; border-collapse: collapse; font-size: 13px; }
thead th {
  text-align: left; padding: 8px 12px; font-weight: 500;
  color: var(--text-muted); font-size: 11px; text-transform: uppercase;
  letter-spacing: 0.5px; border-bottom: 1px solid var(--border);
  cursor: pointer; user-select: none; white-space: nowrap;
}
thead th:hover { color: var(--text-dim); }
thead th .sort-arrow { margin-left: 4px; font-size: 10px; }
tbody td {
  padding: 8px 12px; border-bottom: 1px solid var(--border);
  white-space: nowrap; font-family: var(--mono); font-size: 12px;
}
tbody tr:hover { background: var(--bg-hover); }
tbody tr:last-child td { border-bottom: none; }

/* Status badges */
.badge {
  display: inline-block; padding: 2px 8px; border-radius: 10px;
  font-size: 11px; font-weight: 600; letter-spacing: 0.3px;
}
.badge-ok { background: var(--green-dim); color: var(--green); }
.badge-warn { background: var(--yellow-dim); color: var(--yellow); }
.badge-err { background: var(--red-dim); color: var(--red); }
.badge-off { background: var(--border); color: var(--text-muted); }

/* Search/filter bar */
.toolbar {
  display: flex; align-items: center; gap: 8px; padding: 12px 16px;
  border-bottom: 1px solid var(--border);
}
.search-input {
  flex: 1; padding: 6px 12px; background: var(--bg-input); border: 1px solid var(--border);
  border-radius: 6px; color: var(--text); font-size: 13px; font-family: var(--mono);
  outline: none;
}
.search-input:focus { border-color: var(--green); }
.search-input::placeholder { color: var(--text-muted); }
.filter-btn {
  padding: 6px 12px; background: var(--bg-input); border: 1px solid var(--border);
  border-radius: 6px; color: var(--text-dim); font-size: 12px; cursor: pointer;
}
.filter-btn:hover { border-color: var(--text-dim); color: var(--text); }
.filter-btn.active { border-color: var(--green); color: var(--green); }

/* Logs */
.log-container {
  font-family: var(--mono); font-size: 12px; line-height: 1.8;
  max-height: 70vh; overflow-y: auto; padding: 12px 16px;
}
.log-line { white-space: pre-wrap; word-break: break-all; padding: 1px 0; }
.log-line:hover { background: var(--bg-hover); }
.log-line.filtered-out { display: none; }
.log-line .ts { color: var(--text-muted); }
.log-line .lvl-INF { color: var(--blue); }
.log-line .lvl-WRN { color: var(--yellow); }
.log-line .lvl-ERR { color: var(--red); }
.log-line .lvl-DBG { color: var(--text-muted); }
.log-count { font-size: 11px; color: var(--text-muted); padding: 8px 16px; border-top: 1px solid var(--border); }

/* Traffic bar chart */
.bar-chart { display: flex; align-items: flex-end; gap: 2px; height: 40px; padding: 0 4px; }
.bar { flex: 1; background: var(--green); border-radius: 2px 2px 0 0; min-width: 3px; transition: height 0.3s; }
.bar.rx { background: var(--purple); }

/* API docs */
.endpoint { padding: 12px 16px; border-bottom: 1px solid var(--border); }
.endpoint:last-child { border-bottom: none; }
.endpoint-method { font-weight: 700; color: var(--green); font-family: var(--mono); font-size: 12px; margin-right: 8px; }
.endpoint-path { font-family: var(--mono); font-size: 13px; color: var(--cyan); }
.endpoint-desc { font-size: 12px; color: var(--text-dim); margin-top: 4px; }
.endpoint-try {
  margin-top: 6px; padding: 4px 10px; background: var(--bg-input);
  border: 1px solid var(--border); border-radius: 4px; color: var(--text-dim);
  font-size: 11px; cursor: pointer; font-family: var(--mono);
}
.endpoint-try:hover { border-color: var(--green); color: var(--green); }
.endpoint-response {
  margin-top: 8px; padding: 10px; background: var(--bg); border-radius: 6px;
  font-family: var(--mono); font-size: 11px; max-height: 300px; overflow: auto;
  white-space: pre-wrap; word-break: break-all; display: none;
  color: var(--text-dim); border: 1px solid var(--border);
}

/* Metrics */
.met-family { border-bottom: 1px solid var(--border); }
.met-family:last-child { border-bottom: none; }
.met-family-header {
  padding: 10px 16px; cursor: pointer; display: flex; align-items: center;
  justify-content: space-between; gap: 12px; user-select: none;
}
.met-family-header:hover { background: var(--bg-hover); }
.met-family-name { font-family: var(--mono); font-size: 13px; font-weight: 600; color: var(--cyan); }
.met-family-type { font-size: 11px; color: var(--text-muted); text-transform: uppercase; }
.met-family-help { font-size: 11px; color: var(--text-dim); flex: 1; text-align: right; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.met-family-arrow { color: var(--text-muted); font-size: 10px; transition: transform 0.15s; }
.met-family.open .met-family-arrow { transform: rotate(90deg); }
.met-samples { display: none; padding: 0 16px 10px; }
.met-family.open .met-samples { display: block; }
.met-samples table { font-size: 12px; }
.met-samples td { padding: 4px 12px; }
.met-samples .met-labels { color: var(--text-dim); }
.met-samples .met-value { color: var(--green); font-weight: 600; text-align: right; }

/* Charts */
.chart-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 12px; margin-bottom: 16px; }
.chart-card {
  background: var(--bg-card); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 14px 16px;
}
.chart-title { font-size: 11px; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.5px; margin-bottom: 2px; }
.chart-value { font-size: 20px; font-weight: 700; font-family: var(--mono); }
.chart-sub { display: flex; gap: 16px; margin-bottom: 8px; }
.chart-canvas { width: 100%; height: 80px; display: block; border-radius: 4px; }

/* Scrollbar */
::-webkit-scrollbar { width: 6px; }
::-webkit-scrollbar-track { background: transparent; }
::-webkit-scrollbar-thumb { background: var(--border-light); border-radius: 3px; }
::-webkit-scrollbar-thumb:hover { background: var(--text-muted); }
</style>
</head>
<body>

<div class="header">
  <div class="header-left">
    <div class="logo">mesh<span>` + version + `</span></div>
  </div>
  <div class="header-status" id="hdr-status">connecting...</div>
</div>

<div class="tabs" id="tabs">
  <div class="tab active" data-tab="dashboard">Dashboard</div>
  <div class="tab" data-tab="filesync">Filesync</div>
  <div class="tab" data-tab="logs">Logs</div>
  <div class="tab" data-tab="metrics">Metrics</div>
  <div class="tab" data-tab="api">API</div>
</div>

<div class="content">

  <!-- Dashboard panel -->
  <div class="panel active" id="p-dashboard">
    <div class="stats" id="dash-stats"></div>
    <div class="card">
      <div class="card-header">
        <span>Components</span>
        <div style="display:flex;gap:8px">
          <input class="search-input" id="comp-search" placeholder="Filter components..." style="width:220px">
        </div>
      </div>
      <div class="card-body">
        <table>
          <thead><tr>
            <th data-sort="status">Status <span class="sort-arrow"></span></th>
            <th data-sort="type">Type <span class="sort-arrow"></span></th>
            <th data-sort="id">ID <span class="sort-arrow"></span></th>
            <th data-sort="detail">Detail <span class="sort-arrow"></span></th>
          </tr></thead>
          <tbody id="comp-body"></tbody>
        </table>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><span>Recent Logs</span></div>
      <div class="card-body">
        <div class="log-container" id="dash-logs" style="max-height:200px"></div>
      </div>
    </div>
  </div>

  <!-- Filesync panel -->
  <div class="panel" id="p-filesync">
    <div class="stats" id="fs-stats"></div>
    <div class="card">
      <div class="card-header">
        <span>Folders</span>
        <input class="search-input" id="fs-search" placeholder="Filter folders..." style="width:220px">
      </div>
      <div class="card-body">
        <table>
          <thead><tr>
            <th data-sort="id">ID <span class="sort-arrow"></span></th>
            <th data-sort="path">Path <span class="sort-arrow"></span></th>
            <th data-sort="direction">Direction <span class="sort-arrow"></span></th>
            <th data-sort="file_count">Files <span class="sort-arrow"></span></th>
            <th data-sort="peers">Peers <span class="sort-arrow"></span></th>
          </tr></thead>
          <tbody id="fs-body"></tbody>
        </table>
      </div>
    </div>
    <div class="card" id="conflict-card">
      <div class="card-header"><span>Conflicts</span><span class="badge badge-ok" id="conflict-count">0</span></div>
      <div class="card-body">
        <table>
          <thead><tr><th>Folder</th><th>Path</th></tr></thead>
          <tbody id="conflict-body"></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- Logs panel -->
  <div class="panel" id="p-logs">
    <div class="card">
      <div class="toolbar">
        <input class="search-input" id="log-search" placeholder="Search logs...">
        <div class="filter-btn active" data-level="all">All</div>
        <div class="filter-btn" data-level="INF">Info</div>
        <div class="filter-btn" data-level="WRN">Warn</div>
        <div class="filter-btn" data-level="ERR">Error</div>
        <div class="filter-btn" data-level="DBG">Debug</div>
      </div>
      <div class="card-body">
        <div class="log-container" id="log-lines" style="max-height:75vh"></div>
        <div class="log-count" id="log-count"></div>
      </div>
    </div>
  </div>

  <!-- Metrics panel -->
  <div class="panel" id="p-metrics">
    <div class="stats" id="met-stats"></div>
    <div class="chart-grid" id="met-charts">
      <div class="chart-card">
        <div class="chart-title">Network Traffic</div>
        <div class="chart-sub">
          <div class="chart-value" style="color:var(--green)"><span id="chart-tx-val">0 B/s</span> <span style="font-size:11px;color:var(--text-muted)">tx</span></div>
          <div class="chart-value" style="color:var(--purple)"><span id="chart-rx-val">0 B/s</span> <span style="font-size:11px;color:var(--text-muted)">rx</span></div>
        </div>
        <canvas class="chart-canvas" id="chart-traffic"></canvas>
      </div>
      <div class="chart-card">
        <div class="chart-title">Active Streams</div>
        <div class="chart-value" id="chart-streams-val" style="margin-bottom:8px">0</div>
        <canvas class="chart-canvas" id="chart-streams"></canvas>
      </div>
      <div class="chart-card">
        <div class="chart-title">Goroutines</div>
        <div class="chart-value" id="chart-goroutines-val" style="margin-bottom:8px">0</div>
        <canvas class="chart-canvas" id="chart-goroutines"></canvas>
      </div>
      <div class="chart-card" id="chart-fds-card">
        <div class="chart-title">Open File Descriptors</div>
        <div class="chart-value" id="chart-fds-val" style="margin-bottom:8px">0</div>
        <canvas class="chart-canvas" id="chart-fds"></canvas>
      </div>
    </div>
    <div class="card">
      <div class="card-header">
        <span>Prometheus Metrics</span>
        <input class="search-input" id="met-search" placeholder="Filter metrics..." style="width:220px">
      </div>
      <div class="card-body" id="met-body" style="padding:0"></div>
    </div>
  </div>

  <!-- API panel -->
  <div class="panel" id="p-api">
    <div class="card">
      <div class="card-header"><span>API Endpoints</span></div>
      <div class="card-body" id="api-list"></div>
    </div>
  </div>
</div>

<script>
// --- State ---
let state = {}, logs = [], folders = [], conflicts = [], metricsText = '';
let compSort = {col:'type', asc:true};
let fsSort = {col:'id', asc:true};
let logLevel = 'all';
const HIST_LEN = 60;
const chartHist = {tx:[], rx:[], streams:[], goroutines:[], fds:[]};
let prevTotalTx = 0, prevTotalRx = 0, firstTick = true;

// --- Tabs ---
const tabMap = {'/ui':'dashboard','/ui/filesync':'filesync','/ui/logs':'logs','/ui/metrics':'metrics','/ui/api':'api'};
let activeTab = tabMap[location.pathname] || 'dashboard';

function showTab(name) {
  activeTab = name;
  document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  document.querySelectorAll('.panel').forEach(p => p.classList.toggle('active', p.id === 'p-'+name));
  const path = '/ui' + (name === 'dashboard' ? '' : '/'+name);
  if (location.pathname !== path) history.pushState(null, '', path);
}

document.getElementById('tabs').addEventListener('click', e => {
  if (e.target.classList.contains('tab')) showTab(e.target.dataset.tab);
});
window.addEventListener('popstate', () => { showTab(tabMap[location.pathname] || 'dashboard'); });
showTab(activeTab);

// --- Data fetch ---
async function tick() {
  try {
    const [sr, lr, fr, cr, mr] = await Promise.all([
      fetch('/api/state').then(r=>r.json()),
      fetch('/api/logs').then(r=>r.json()),
      fetch('/api/filesync/folders').then(r=>r.json()),
      fetch('/api/filesync/conflicts').then(r=>r.json()),
      fetch('/api/metrics').then(r=>r.text()),
    ]);
    state = sr; logs = lr; folders = fr; conflicts = cr; metricsText = mr;

    // Accumulate chart history from metrics
    const nowTx = sumMetric(metricsText, 'mesh_bytes_tx_total');
    const nowRx = sumMetric(metricsText, 'mesh_bytes_rx_total');
    if (!firstTick) {
      pushHist('tx', Math.max(0, nowTx - prevTotalTx));
      pushHist('rx', Math.max(0, nowRx - prevTotalRx));
    } else { firstTick = false; }
    prevTotalTx = nowTx; prevTotalRx = nowRx;
    pushHist('streams', sumMetric(metricsText, 'mesh_active_streams'));
    pushHist('goroutines', valMetric(metricsText, 'mesh_process_goroutines'));
    const fds = valMetric(metricsText, 'mesh_process_open_fds');
    if (fds !== null) pushHist('fds', fds);

    document.getElementById('hdr-status').textContent = 'updated ' + new Date().toLocaleTimeString();
    render();
  } catch(e) {
    document.getElementById('hdr-status').textContent = 'error: ' + e.message;
  }
}

// --- Render ---
function render() {
  renderStats();
  renderComponents();
  renderDashLogs();
  renderFilesync();
  renderConflicts();
  renderLogs();
  renderMetrics();
  renderCharts();
}

function x(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

function badge(status) {
  const s = String(status);
  if (s === 'listening' || s === 'connected') return '<span class="badge badge-ok">'+x(s)+'</span>';
  if (s === 'connecting' || s === 'retrying') return '<span class="badge badge-warn">'+x(s)+'</span>';
  if (s === 'failed') return '<span class="badge badge-err">'+x(s)+'</span>';
  return '<span class="badge badge-off">'+x(s)+'</span>';
}

function renderStats() {
  const comps = Object.values(state);
  const up = comps.filter(c => c.status === 'listening' || c.status === 'connected').length;
  const down = comps.filter(c => c.status === 'failed').length;
  const pending = comps.length - up - down;
  const totalFiles = folders.reduce((s,f) => s + (f.file_count||0), 0);
  document.getElementById('dash-stats').innerHTML =
    stat('Components', comps.length, up+' up') +
    stat('Healthy', up, comps.length ? Math.round(up/comps.length*100)+'%' : '-', up === comps.length ? 'var(--green)' : 'var(--yellow)') +
    stat('Failed', down, '', down > 0 ? 'var(--red)' : 'var(--green)') +
    stat('Pending', pending, '');
  document.getElementById('fs-stats').innerHTML =
    stat('Folders', folders.length, '') +
    stat('Total Files', totalFiles.toLocaleString(), '') +
    stat('Conflicts', conflicts.length, '', conflicts.length > 0 ? 'var(--red)' : 'var(--green)');
}

function stat(label, value, sub, color) {
  const c = color ? ' style="color:'+color+'"' : '';
  return '<div class="stat"><div class="stat-label">'+x(label)+'</div><div class="stat-value"'+c+'>'+x(String(value))+'</div>'+(sub?'<div class="stat-sub">'+x(sub)+'</div>':'')+'</div>';
}

function renderComponents() {
  const filter = document.getElementById('comp-search').value.toLowerCase();
  let rows = Object.values(state).filter(c => {
    if (!filter) return true;
    return (c.type+c.id+c.status+(c.message||'')+(c.peer_addr||'')).toLowerCase().includes(filter);
  });
  rows.sort((a,b) => {
    const va = String(a[compSort.col]||''), vb = String(b[compSort.col]||'');
    return compSort.asc ? va.localeCompare(vb) : vb.localeCompare(va);
  });
  const el = document.getElementById('comp-body');
  if (!rows.length) { el.innerHTML = '<tr><td colspan="4" style="color:var(--text-muted);padding:20px">No components</td></tr>'; return; }
  el.innerHTML = rows.map(c => {
    const detail = c.message || c.peer_addr || c.bound_addr || '';
    return '<tr><td>'+badge(c.status)+'</td><td>'+x(c.type)+'</td><td>'+x(c.id)+'</td><td style="color:var(--text-dim)">'+x(detail)+'</td></tr>';
  }).join('');
}

function renderDashLogs() {
  const el = document.getElementById('dash-logs');
  if (!logs.length) { el.innerHTML = '<div style="color:var(--text-muted);padding:8px">No logs yet</div>'; return; }
  const last = logs.slice(-10);
  el.innerHTML = last.map(l => '<div class="log-line">' + colorLog(x(l)) + '</div>').join('');
  el.scrollTop = el.scrollHeight;
}

function renderFilesync() {
  const filter = document.getElementById('fs-search').value.toLowerCase();
  let rows = folders.filter(f => {
    if (!filter) return true;
    return (f.id+f.path+f.direction+(f.peers||[]).join('')).toLowerCase().includes(filter);
  });
  rows.sort((a,b) => {
    let va, vb;
    if (fsSort.col === 'file_count') { va = a.file_count||0; vb = b.file_count||0; return fsSort.asc ? va-vb : vb-va; }
    if (fsSort.col === 'peers') { va = (a.peers||[]).join(','); vb = (b.peers||[]).join(','); }
    else { va = String(a[fsSort.col]||''); vb = String(b[fsSort.col]||''); }
    return fsSort.asc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
  });
  const el = document.getElementById('fs-body');
  if (!rows.length) { el.innerHTML = '<tr><td colspan="5" style="color:var(--text-muted);padding:20px">No folders</td></tr>'; return; }
  el.innerHTML = rows.map(f => {
    const dirBadge = f.direction === 'send-receive' ? 'badge-ok' :
                     f.direction === 'disabled' ? 'badge-off' :
                     f.direction === 'dry-run' ? 'badge-warn' : 'badge-ok';
    return '<tr><td style="font-weight:600">'+x(f.id)+'</td><td style="color:var(--text-dim)">'+x(f.path)+'</td>' +
           '<td><span class="badge '+dirBadge+'">'+x(f.direction)+'</span></td>' +
           '<td>'+(f.file_count||0).toLocaleString()+'</td>' +
           '<td style="color:var(--text-dim)">'+x((f.peers||[]).join(', '))+'</td></tr>';
  }).join('');
}

function renderConflicts() {
  const el = document.getElementById('conflict-body');
  const cnt = document.getElementById('conflict-count');
  cnt.textContent = conflicts.length;
  cnt.className = 'badge ' + (conflicts.length > 0 ? 'badge-err' : 'badge-ok');
  if (!conflicts.length) { el.innerHTML = '<tr><td colspan="2" style="color:var(--text-muted);padding:16px">No conflicts</td></tr>'; return; }
  el.innerHTML = conflicts.map(c =>
    '<tr><td>'+x(c.folder_id)+'</td><td style="color:var(--red)">'+x(c.path)+'</td></tr>'
  ).join('');
}

function colorLog(line) {
  // Highlight timestamp, level, and key parts
  return line
    .replace(/^(\d{2}:\d{2}:\d{2}\.\d+)/, '<span class="ts">$1</span>')
    .replace(/ (INF) /g, ' <span class="lvl-INF">$1</span> ')
    .replace(/ (WRN) /g, ' <span class="lvl-WRN">$1</span> ')
    .replace(/ (ERR) /g, ' <span class="lvl-ERR">$1</span> ')
    .replace(/ (DBG) /g, ' <span class="lvl-DBG">$1</span> ');
}

function renderLogs() {
  const filter = document.getElementById('log-search').value.toLowerCase();
  const el = document.getElementById('log-lines');
  if (!logs.length) { el.innerHTML = '<div style="color:var(--text-muted);padding:16px">No logs yet</div>'; return; }

  let shown = 0, total = logs.length;
  const html = logs.map(l => {
    const plain = l;
    // Level filter
    if (logLevel !== 'all') {
      if (!plain.includes(' '+logLevel+' ')) return '';
    }
    // Text filter
    if (filter && !plain.toLowerCase().includes(filter)) return '';
    shown++;
    return '<div class="log-line">' + colorLog(x(l)) + '</div>';
  }).join('');

  el.innerHTML = html || '<div style="color:var(--text-muted);padding:16px">No matching logs</div>';
  document.getElementById('log-count').textContent = shown + ' / ' + total + ' lines';

  // Auto-scroll only if user is near bottom
  if (el.scrollHeight - el.scrollTop - el.clientHeight < 100) {
    el.scrollTop = el.scrollHeight;
  }
}

// --- Charts ---
function valMetric(text, name) {
  const m = text.match(new RegExp('^' + name + '\\s+(\\S+)', 'm'));
  return m ? parseFloat(m[1]) : null;
}
function sumMetric(text, name) {
  let sum = 0, re = new RegExp('^' + name + '(?:\\{[^}]*\\})?\\s+(\\S+)', 'gm'), m;
  while ((m = re.exec(text)) !== null) sum += parseFloat(m[1]) || 0;
  return sum;
}
function pushHist(key, val) {
  const arr = chartHist[key];
  arr.push(val == null ? 0 : val);
  if (arr.length > HIST_LEN) arr.shift();
}
function fmtRate(n) {
  if (n < 1024) return n.toFixed(0) + ' B/s';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB/s';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB/s';
  return (n / 1073741824).toFixed(2) + ' GB/s';
}
function drawChart(id, series, colors) {
  const canvas = document.getElementById(id);
  if (!canvas) return;
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  if (!rect.width) return;
  canvas.width = rect.width * dpr;
  canvas.height = rect.height * dpr;
  const ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);
  const w = rect.width, h = rect.height;
  ctx.clearRect(0, 0, w, h);
  // Global max across all series
  let max = 1;
  for (const s of series) for (const v of s) if (v > max) max = v;
  // Grid lines
  ctx.strokeStyle = '#2a2d3a';
  ctx.lineWidth = 0.5;
  for (let i = 1; i < 4; i++) {
    const y = h * i / 4;
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(w, y); ctx.stroke();
  }
  // Draw each series
  const step = w / (HIST_LEN - 1);
  series.forEach((data, si) => {
    if (!data.length) return;
    const off = HIST_LEN - data.length;
    ctx.beginPath();
    data.forEach((v, i) => {
      const px = (off + i) * step;
      const py = h - (v / max) * (h - 6) - 3;
      if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
    });
    ctx.strokeStyle = colors[si];
    ctx.lineWidth = 1.5;
    ctx.stroke();
    // Fill under line
    ctx.lineTo((off + data.length - 1) * step, h);
    ctx.lineTo(off * step, h);
    ctx.closePath();
    ctx.globalAlpha = 0.08;
    ctx.fillStyle = colors[si];
    ctx.fill();
    ctx.globalAlpha = 1;
  });
}
function renderCharts() {
  const last = arr => arr.length ? arr[arr.length - 1] : 0;
  // Traffic (dual line)
  document.getElementById('chart-tx-val').textContent = fmtRate(last(chartHist.tx));
  document.getElementById('chart-rx-val').textContent = fmtRate(last(chartHist.rx));
  drawChart('chart-traffic', [chartHist.tx, chartHist.rx], ['#34d399', '#a78bfa']);
  // Active Streams
  document.getElementById('chart-streams-val').textContent = last(chartHist.streams).toLocaleString();
  drawChart('chart-streams', [chartHist.streams], ['#60a5fa']);
  // Goroutines
  document.getElementById('chart-goroutines-val').textContent = last(chartHist.goroutines).toLocaleString();
  drawChart('chart-goroutines', [chartHist.goroutines], ['#22d3ee']);
  // FDs (hidden on platforms without open_fds)
  const fdsCard = document.getElementById('chart-fds-card');
  if (chartHist.fds.length && chartHist.fds.some(v => v > 0)) {
    fdsCard.style.display = '';
    document.getElementById('chart-fds-val').textContent = last(chartHist.fds).toLocaleString();
    drawChart('chart-fds', [chartHist.fds], ['#fbbf24']);
  } else {
    fdsCard.style.display = 'none';
  }
}

// --- Metrics ---
function parseMetrics(text) {
  const families = [];
  let cur = null;
  for (const line of text.split('\n')) {
    if (!line) continue;
    if (line.startsWith('# HELP ')) {
      const rest = line.slice(7);
      const sp = rest.indexOf(' ');
      const name = rest.slice(0, sp), help = rest.slice(sp+1);
      cur = {name, help, type:'', samples:[]};
      families.push(cur);
    } else if (line.startsWith('# TYPE ')) {
      if (cur) { const rest = line.slice(7); cur.type = rest.slice(rest.indexOf(' ')+1); }
    } else if (!line.startsWith('#')) {
      // sample line: metric_name{labels} value  OR  metric_name value
      const m = line.match(/^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(.+)$/);
      if (m) {
        const labels = m[2] ? m[2].slice(1,-1) : '';
        const val = m[3];
        if (cur && m[1] === cur.name) {
          cur.samples.push({labels, value: val});
        } else {
          // orphan sample or different metric name
          if (!cur || m[1] !== cur.name) {
            cur = {name: m[1], help:'', type:'', samples:[{labels, value: val}]};
            families.push(cur);
          }
        }
      }
    }
  }
  return families;
}

let metOpenFamilies = new Set();

function renderMetrics() {
  const filter = document.getElementById('met-search').value.toLowerCase();
  const families = parseMetrics(metricsText);
  const filtered = families.filter(f => {
    if (!filter) return true;
    if (f.name.toLowerCase().includes(filter)) return true;
    if (f.help.toLowerCase().includes(filter)) return true;
    return f.samples.some(s => s.labels.toLowerCase().includes(filter) || s.value.includes(filter));
  });

  // Stats
  const totalSamples = families.reduce((s,f) => s + f.samples.length, 0);
  const gauges = families.filter(f => f.type === 'gauge').length;
  const counters = families.filter(f => f.type === 'counter').length;
  document.getElementById('met-stats').innerHTML =
    stat('Metric Families', families.length, '') +
    stat('Total Samples', totalSamples, '') +
    stat('Gauges', gauges, '') +
    stat('Counters', counters, '');

  const el = document.getElementById('met-body');
  if (!filtered.length) {
    el.innerHTML = '<div style="color:var(--text-muted);padding:20px">No metrics</div>';
    return;
  }

  el.innerHTML = filtered.map(f => {
    const isOpen = metOpenFamilies.has(f.name);
    const cls = isOpen ? ' open' : '';
    let samplesHtml = '';
    if (f.samples.length === 1 && !f.samples[0].labels) {
      // Single value — show inline
      samplesHtml = '<span style="font-family:var(--mono);color:var(--green);font-weight:600;font-size:13px">' + x(fmtVal(f.samples[0].value)) + '</span>';
      return '<div class="met-family' + cls + '" data-met="' + x(f.name) + '">' +
        '<div class="met-family-header">' +
          '<span class="met-family-name">' + x(f.name) + '</span>' +
          '<span class="met-family-help">' + x(f.help) + '</span>' +
          '<span class="met-family-type">' + x(f.type) + '</span>' +
          samplesHtml +
        '</div></div>';
    }
    // Multi-sample — collapsible table
    const rows = f.samples.map(s => {
      const labelParts = s.labels ? s.labels.split(',').map(l => {
        const eq = l.indexOf('=');
        const k = l.slice(0, eq), v = l.slice(eq+1).replace(/^"|"$/g, '');
        return '<span style="color:var(--text-muted)">' + x(k) + '</span>=<span style="color:var(--purple)">' + x(v) + '</span>';
      }).join(' ') : '';
      return '<tr><td class="met-labels">' + labelParts + '</td><td class="met-value">' + x(fmtVal(s.value)) + '</td></tr>';
    }).join('');
    return '<div class="met-family' + cls + '" data-met="' + x(f.name) + '">' +
      '<div class="met-family-header">' +
        '<span class="met-family-arrow">&#9654;</span>' +
        '<span class="met-family-name">' + x(f.name) + '</span>' +
        '<span class="met-family-help">' + x(f.help) + '</span>' +
        '<span class="met-family-type">' + x(f.type) + '</span>' +
        '<span style="font-family:var(--mono);color:var(--text-dim);font-size:11px">' + f.samples.length + ' series</span>' +
      '</div>' +
      '<div class="met-samples"><table><tbody>' + rows + '</tbody></table></div>' +
    '</div>';
  }).join('');
}

function fmtVal(v) {
  const n = parseFloat(v);
  if (isNaN(n)) return v;
  if (Number.isInteger(n)) return n.toLocaleString();
  return n.toLocaleString(undefined, {minimumFractionDigits:1, maximumFractionDigits:3});
}

document.getElementById('met-body').addEventListener('click', e => {
  const hdr = e.target.closest('.met-family-header');
  if (!hdr) return;
  const fam = hdr.parentElement;
  if (!fam.querySelector('.met-samples')) return; // single-value, not collapsible
  const name = fam.dataset.met;
  if (metOpenFamilies.has(name)) { metOpenFamilies.delete(name); fam.classList.remove('open'); }
  else { metOpenFamilies.add(name); fam.classList.add('open'); }
});
document.getElementById('met-search').addEventListener('input', renderMetrics);

// --- Sorting ---
document.querySelectorAll('#p-dashboard th[data-sort]').forEach(th => {
  th.addEventListener('click', () => {
    if (compSort.col === th.dataset.sort) compSort.asc = !compSort.asc;
    else { compSort.col = th.dataset.sort; compSort.asc = true; }
    renderComponents();
  });
});
document.querySelectorAll('#p-filesync th[data-sort]').forEach(th => {
  th.addEventListener('click', () => {
    if (fsSort.col === th.dataset.sort) fsSort.asc = !fsSort.asc;
    else { fsSort.col = th.dataset.sort; fsSort.asc = true; }
    renderFilesync();
  });
});

// --- Filters ---
document.getElementById('comp-search').addEventListener('input', renderComponents);
document.getElementById('fs-search').addEventListener('input', renderFilesync);
document.getElementById('log-search').addEventListener('input', renderLogs);
document.querySelectorAll('.filter-btn[data-level]').forEach(btn => {
  btn.addEventListener('click', () => {
    logLevel = btn.dataset.level;
    document.querySelectorAll('.filter-btn[data-level]').forEach(b => b.classList.toggle('active', b.dataset.level === logLevel));
    renderLogs();
  });
});

// --- API docs ---
const endpoints = [
  {method:'GET', path:'/api/state', desc:'JSON snapshot of all component states (type, id, status, message, peer_addr, bound_addr, file_count, last_sync).'},
  {method:'GET', path:'/api/logs', desc:'Recent log lines as a JSON string array. ANSI escape codes are stripped.'},
  {method:'GET', path:'/api/metrics', desc:'Prometheus text format metrics: mesh_component_up, mesh_bytes_tx_total, mesh_bytes_rx_total, mesh_active_streams, mesh_uptime_seconds, mesh_auth_failures_total.'},
  {method:'GET', path:'/api/filesync/folders', desc:'Filesync folder statuses as JSON array: id, path, direction, file_count, peers.'},
  {method:'GET', path:'/api/filesync/conflicts', desc:'Conflict files as JSON array: folder_id, path.'},
];

document.getElementById('api-list').innerHTML = endpoints.map((ep, i) =>
  '<div class="endpoint">' +
    '<span class="endpoint-method">'+ep.method+'</span>' +
    '<span class="endpoint-path">'+ep.path+'</span>' +
    '<div class="endpoint-desc">'+x(ep.desc)+'</div>' +
    '<button class="endpoint-try" onclick="tryEndpoint('+i+')">Try it</button>' +
    '<pre class="endpoint-response" id="ep-resp-'+i+'"></pre>' +
  '</div>'
).join('');

async function tryEndpoint(i) {
  const ep = endpoints[i];
  const el = document.getElementById('ep-resp-'+i);
  el.style.display = el.style.display === 'block' ? 'none' : 'block';
  if (el.style.display !== 'block') return;
  try {
    const r = await fetch(ep.path);
    const ct = r.headers.get('content-type') || '';
    if (ct.includes('json')) {
      el.textContent = JSON.stringify(await r.json(), null, 2);
    } else {
      el.textContent = await r.text();
    }
  } catch(e) { el.textContent = 'Error: ' + e.message; }
}

// --- Start ---
tick();
setInterval(tick, 1000);
</script>
</body>
</html>`
