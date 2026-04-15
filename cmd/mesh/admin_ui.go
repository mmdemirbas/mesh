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

/* Gateway structured view */
.chip {
  display: inline-block; padding: 2px 8px; margin: 0 4px 4px 0;
  background: var(--bg-input); border: 1px solid var(--border);
  border-radius: 4px; font-size: 11px; color: var(--text-dim);
  font-family: var(--mono);
}
.chip b { color: var(--text); font-weight: 600; }
.chip.warn { border-color: var(--yellow); color: var(--yellow); }
.chip.error { border-color: var(--red); color: var(--red); }
.chip.ok { border-color: var(--green); color: var(--green); }
.msg-block {
  border: 1px solid var(--border); border-radius: 4px;
  margin-bottom: 6px; background: var(--bg);
}
.msg-head {
  display: flex; align-items: center; gap: 8px;
  padding: 4px 8px; border-bottom: 1px solid var(--border);
  font-size: 11px; color: var(--text-dim);
}
.msg-role {
  text-transform: uppercase; letter-spacing: 0.5px; font-weight: 600;
  padding: 1px 6px; border-radius: 3px; background: var(--bg-input);
}
.msg-role.user      { color: var(--cyan); }
.msg-role.assistant { color: var(--green); }
.msg-role.system    { color: var(--purple); }
.msg-role.tool      { color: var(--yellow); }
.msg-body { padding: 6px 8px; font-size: 12px; }
.msg-body .text { white-space: pre-wrap; word-break: break-word; }
.msg-body .truncate { color: var(--text-dim); cursor: pointer; }
.msg-body .truncate:hover { color: var(--green); }
.tool-block {
  border-left: 2px solid var(--yellow); padding: 4px 8px;
  margin: 4px 0; background: var(--bg-card);
}
.tool-block .tool-name { color: var(--yellow); font-family: var(--mono); font-size: 11px; font-weight: 600; }
.tool-block pre { font-family: var(--mono); font-size: 11px; color: var(--text-dim); white-space: pre-wrap; margin-top: 4px; }
.collapse {
  background: transparent; border: none; color: var(--text-muted);
  font-size: 11px; cursor: pointer; padding: 2px 6px;
}
.collapse:hover { color: var(--green); }
.section-title {
  font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px;
  color: var(--text-muted); margin: 8px 0 4px;
}
/* Per-pair token bar */
.token-bar {
  display: flex; height: 18px; border-radius: 4px; overflow: hidden;
  border: 1px solid var(--border); margin: 6px 0;
}
.token-bar > div { display: flex; align-items: center; justify-content: center; font-size: 10px; color: var(--bg); font-weight: 600; min-width: 0; }
.token-bar .seg-cache-read { background: var(--green); }
.token-bar .seg-cache-create { background: var(--purple); }
.token-bar .seg-input { background: var(--cyan); }
.token-bar .seg-output { background: var(--yellow); }
.token-legend { display: flex; flex-wrap: wrap; gap: 8px; font-size: 11px; color: var(--text-dim); margin-bottom: 4px; }
.token-legend span { display: inline-flex; align-items: center; gap: 4px; }
.token-legend i { display: inline-block; width: 10px; height: 10px; border-radius: 2px; }

/* Gateway detail (request | response) */
.gw-detail-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
@media (max-width: 1100px) { .gw-detail-grid { grid-template-columns: 1fr; } }
.gw-detail-pane {
  background: var(--bg-card); border: 1px solid var(--border);
  border-radius: 6px; display: flex; flex-direction: column; min-width: 0;
}
.gw-detail-pane h4 {
  font-size: 12px; font-weight: 600; color: var(--text-dim);
  padding: 8px 12px; border-bottom: 1px solid var(--border);
  text-transform: uppercase; letter-spacing: 0.5px;
  display: flex; justify-content: space-between; align-items: center;
}
.gw-detail-pane .copy-btn {
  background: transparent; border: 1px solid var(--border);
  border-radius: 4px; color: var(--text-muted); font-size: 11px;
  padding: 2px 8px; cursor: pointer; font-family: var(--mono);
}
.gw-detail-pane .copy-btn:hover { color: var(--green); border-color: var(--green); }
.gw-detail-structured { padding: 8px 12px; font-size: 13px; color: var(--text); border-bottom: 1px solid var(--border); max-height: 40vh; overflow: auto; }
.gw-detail-structured:empty { display: none; }
.gw-detail-raw {
  font-family: var(--mono); font-size: 12px; line-height: 1.5;
  padding: 8px 12px; max-height: 50vh; overflow: auto; white-space: pre;
}
/* JSON syntax tokens */
.json-key   { color: var(--cyan); }
.json-str   { color: var(--green); }
.json-num   { color: var(--yellow); }
.json-bool  { color: var(--purple); }
.json-null  { color: var(--text-muted); }
.json-punct { color: var(--text-dim); }
.json-toggle {
  cursor: pointer; user-select: none; color: var(--text-muted);
  display: inline-block; width: 12px; text-align: center;
}
.json-toggle:hover { color: var(--green); }
.json-collapsed { display: none; }
.json-summary { color: var(--text-muted); font-style: italic; }

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
  <div class="tab" data-tab="clipsync">Clipsync</div>
  <div class="tab" data-tab="filesync">Filesync</div>
  <div class="tab" data-tab="gateway">Gateway</div>
  <div class="tab" data-tab="logs">Logs</div>
  <div class="tab" data-tab="metrics">Metrics</div>
  <div class="tab" data-tab="api">API</div>
  <div class="tab" data-tab="debug">Debug</div>
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

  <!-- Clipsync panel -->
  <div class="panel" id="p-clipsync">
    <div class="stats" id="cs-stats"></div>
    <div class="card">
      <div class="card-header"><span>Recent Activity</span></div>
      <div class="card-body">
        <table>
          <thead><tr>
            <th>Direction</th>
            <th>Size</th>
            <th>Content</th>
            <th>Peer</th>
            <th>Time</th>
          </tr></thead>
          <tbody id="cs-body"></tbody>
        </table>
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
    <div class="card">
      <div class="card-header"><span>Recent Activity</span></div>
      <div class="card-body">
        <table>
          <thead><tr><th>Direction</th><th>Folder</th><th>Peer</th><th>Files</th><th>Size</th><th>Time</th></tr></thead>
          <tbody id="fsa-body"></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- Gateway panel -->
  <div class="panel" id="p-gateway">
    <div class="card">
      <div class="card-header">
        <span>Gateways</span>
        <div style="display:flex;gap:8px">
          <select id="gw-select" style="background:var(--bg-input);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px"></select>
          <input class="search-input" id="gw-search" placeholder="Filter rows..." style="width:220px">
          <select id="gw-outcome" style="background:var(--bg-input);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px">
            <option value="">all outcomes</option>
            <option value="ok">ok</option>
            <option value="error">error</option>
            <option value="truncated">truncated</option>
            <option value="client_cancelled">client_cancelled</option>
          </select>
        </div>
      </div>
      <div class="card-body">
        <div id="gw-meta" style="font-size:12px;color:var(--text-muted);margin-bottom:8px"></div>
        <table>
          <thead><tr>
            <th>Time</th>
            <th>Dir</th>
            <th>Model</th>
            <th>Stream</th>
            <th>Status</th>
            <th>Outcome</th>
            <th>Tokens</th>
            <th>Elapsed</th>
            <th>Summary</th>
          </tr></thead>
          <tbody id="gw-body"></tbody>
        </table>
      </div>
    </div>
    <div class="card" id="gw-detail-card" style="display:none">
      <div class="card-header">
        <span id="gw-detail-title">Detail</span>
        <div class="filter-btn" onclick="document.getElementById('gw-detail-card').style.display='none'">close</div>
      </div>
      <div class="card-body padded">
        <div class="gw-detail-grid">
          <div class="gw-detail-pane">
            <h4>Request <button class="copy-btn" onclick="copyDetail('req')">copy</button></h4>
            <div class="gw-detail-structured" id="gw-req-structured"></div>
            <div class="gw-detail-raw" id="gw-req-raw"></div>
          </div>
          <div class="gw-detail-pane">
            <h4>Response <button class="copy-btn" onclick="copyDetail('resp')">copy</button></h4>
            <div class="gw-detail-structured" id="gw-resp-structured"></div>
            <div class="gw-detail-raw" id="gw-resp-raw"></div>
          </div>
        </div>
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
        <div style="flex:1"></div>
        <div class="filter-btn active" id="log-mode-recent" onclick="setLogMode('recent')">Recent</div>
        <div class="filter-btn" id="log-mode-file" onclick="setLogMode('file')">Full Log</div>
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

  <!-- Debug panel -->
  <div class="panel" id="p-debug">
    <div class="stats" id="dbg-stats"></div>
    <div class="card">
      <div class="card-header">
        <span>Profiling</span>
      </div>
      <div class="card-body padded" style="display:flex;gap:8px;flex-wrap:wrap">
        <button class="endpoint-try" onclick="dbgProfile('goroutine')">Goroutine Dump</button>
        <button class="endpoint-try" onclick="dbgProfile('heap')">Heap Profile</button>
        <button class="endpoint-try" onclick="dbgProfile('allocs')">Allocs Profile</button>
        <button class="endpoint-try" onclick="dbgProfile('threadcreate')">Thread Create</button>
        <button class="endpoint-try" onclick="dbgProfile('block')">Block Profile</button>
        <button class="endpoint-try" onclick="dbgProfile('mutex')">Mutex Profile</button>
        <button class="endpoint-try" onclick="dbgCpuProfile()">CPU Profile (10s)</button>
        <button class="endpoint-try" onclick="dbgTrace()">Trace (5s)</button>
      </div>
    </div>
    <div class="card" id="dbg-result-card" style="display:none">
      <div class="card-header">
        <span id="dbg-result-title">Result</span>
        <button class="endpoint-try" onclick="document.getElementById('dbg-result-card').style.display='none'">Close</button>
      </div>
      <div class="card-body">
        <pre class="log-container" id="dbg-result" style="max-height:70vh;white-space:pre-wrap;word-break:break-all;padding:12px 16px"></pre>
      </div>
    </div>
  </div>
</div>

<script>
// --- State ---
let state = {}, logs = [], folders = [], conflicts = [], clipActivities = [], fsActivities = [], metricsText = '', gatewayAudit = [];
let compSort = {col:'type', asc:true};
let fsSort = {col:'id', asc:true};
let logLevel = 'all';
let logMode = 'recent'; // 'recent' (ring buffer) or 'file' (full log file)
let fileLogLines = [], fileLogSize = 0, fileLogLoaded = false;
const HIST_LEN = 60;
const chartHist = {tx:[], rx:[], streams:[], goroutines:[], fds:[]};
let prevTotalTx = 0, prevTotalRx = 0, firstTick = true;

// --- Tabs ---
const tabMap = {'/ui':'dashboard','/ui/clipsync':'clipsync','/ui/filesync':'filesync','/ui/gateway':'gateway','/ui/logs':'logs','/ui/metrics':'metrics','/ui/api':'api','/ui/debug':'debug'};
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

// --- Data fetch (gated by active tab) ---
async function tick() {
  try {
    // Always fetch state (needed by dashboard) and metrics (needed by charts).
    const fetches = [
      fetch('/api/state').then(r=>r.json()),
      fetch('/api/metrics').then(r=>r.text()),
    ];
    // Only fetch tab-specific APIs when that tab is active.
    const needLogs = activeTab === 'dashboard' || activeTab === 'logs';
    const needFilesync = activeTab === 'dashboard' || activeTab === 'filesync';
    const needClipsync = activeTab === 'dashboard' || activeTab === 'clipsync';
    const needGateway = activeTab === 'gateway';
    if (needLogs) fetches.push(fetch('/api/logs').then(r=>r.json()));
    if (needFilesync) {
      fetches.push(fetch('/api/filesync/folders').then(r=>r.json()));
      fetches.push(fetch('/api/filesync/conflicts').then(r=>r.json()));
      fetches.push(fetch('/api/filesync/activity').then(r=>r.json()));
    }
    if (needClipsync) fetches.push(fetch('/api/clipsync/activity').then(r=>r.json()));
    if (needGateway) fetches.push(fetch('/api/gateway/audit?limit=200').then(r=>r.json()));

    const results = await Promise.all(fetches);
    let i = 0;
    state = results[i++]; metricsText = results[i++];
    if (needLogs) logs = results[i++];
    if (needFilesync) { folders = results[i++]; conflicts = results[i++]; fsActivities = results[i++]; }
    if (needClipsync) clipActivities = results[i++];
    if (needGateway) gatewayAudit = results[i++];

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

function setLogMode(mode) {
  logMode = mode;
  document.getElementById('log-mode-recent').classList.toggle('active', mode === 'recent');
  document.getElementById('log-mode-file').classList.toggle('active', mode === 'file');
  if (mode === 'file' && !fileLogLoaded) loadFileLogs();
  renderLogs();
}
async function loadFileLogs() {
  try {
    const resp = await fetch('/api/logs/file?limit=5242880'); // 5 MB
    if (!resp.ok) { fileLogLines = ['(log file not available)']; fileLogLoaded = true; renderLogs(); return; }
    fileLogSize = parseInt(resp.headers.get('X-Log-Size') || '0', 10);
    const text = await resp.text();
    fileLogLines = text.split('\n').filter(l => l.length > 0);
    fileLogLoaded = true;
    renderLogs();
  } catch(e) { fileLogLines = ['(error loading log file: ' + e.message + ')']; fileLogLoaded = true; renderLogs(); }
}

// --- Render ---
function render() {
  renderStats();
  renderComponents();
  renderDashLogs();
  renderClipsync();
  renderFilesync();
  renderConflicts();
  renderFsActivity();
  renderLogs();
  renderMetrics();
  renderCharts();
  renderDebugStats();
  renderGateway();
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

const collapsedGroups = new Set();
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
  // Group by type for tree-table display.
  const groups = {};
  for (const c of rows) { (groups[c.type] = groups[c.type] || []).push(c); }
  const types = Object.keys(groups).sort();
  let html = '';
  for (const typ of types) {
    const items = groups[typ];
    const collapsed = collapsedGroups.has(typ);
    const arrow = collapsed ? '&#9654;' : '&#9660;';
    const count = items.length;
    const okCount = items.filter(c => c.status === 'connected' || c.status === 'listening').length;
    html += '<tr class="tree-group" onclick="toggleGroup(\''+typ+'\')" style="cursor:pointer;background:var(--bg-alt)">';
    html += '<td colspan="2" style="font-weight:600">'+arrow+' '+x(typ)+' <span style="color:var(--text-muted);font-weight:400">('+okCount+'/'+count+')</span></td>';
    html += '<td colspan="2"></td></tr>';
    if (!collapsed) {
      for (const c of items) {
        const detail = c.message || c.peer_addr || c.bound_addr || '';
        html += '<tr><td style="padding-left:24px">'+badge(c.status)+'</td><td>'+x(c.id)+'</td><td colspan="2" style="color:var(--text-dim)">'+x(detail)+'</td></tr>';
      }
    }
  }
  el.innerHTML = html;
}
function toggleGroup(typ) {
  if (collapsedGroups.has(typ)) collapsedGroups.delete(typ);
  else collapsedGroups.add(typ);
  renderComponents();
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

function renderFsActivity() {
  const el = document.getElementById('fsa-body');
  if (!fsActivities.length) { el.innerHTML = '<tr><td colspan="6" style="color:var(--text-muted);padding:16px">No activity yet</td></tr>'; return; }
  el.innerHTML = fsActivities.map(a => {
    const badge = a.direction === 'download' ? 'badge-ok' : a.direction === 'upload' ? 'badge-warn' : '';
    return '<tr><td><span class="badge '+badge+'">'+x(a.direction)+'</span></td><td>'+x(a.folder)+'</td><td>'+x(a.peer)+'</td><td>'+a.files+'</td><td>'+fmtBytes(a.bytes)+'</td><td>'+timeAgo(a.time)+'</td></tr>';
  }).join('');
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
  const src = logMode === 'file' ? fileLogLines : logs;
  if (!src.length) {
    el.innerHTML = '<div style="color:var(--text-muted);padding:16px">' +
      (logMode === 'file' && !fileLogLoaded ? 'Loading...' : 'No logs yet') + '</div>';
    return;
  }

  let shown = 0, total = src.length;
  const html = src.map(l => {
    if (logLevel !== 'all') {
      if (!l.includes(' '+logLevel+' ')) return '';
    }
    if (filter && !l.toLowerCase().includes(filter)) return '';
    shown++;
    return '<div class="log-line">' + colorLog(x(l)) + '</div>';
  }).join('');

  el.innerHTML = html || '<div style="color:var(--text-muted);padding:16px">No matching logs</div>';
  const suffix = logMode === 'file' && fileLogSize > 0 ? ' (file: ' + (fileLogSize/1024).toFixed(0) + ' KB)' : '';
  document.getElementById('log-count').textContent = shown + ' / ' + total + ' lines' + suffix;

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
  {method:'GET', path:'/api/logs/file?offset=0&limit=1048576', desc:'Full log file (plain text). Query params: offset (byte), limit (bytes, default 1MB). Header X-Log-Size gives total file size.'},
  {method:'GET', path:'/api/metrics', desc:'Prometheus text format metrics: mesh_component_up, mesh_bytes_tx_total, mesh_bytes_rx_total, mesh_active_streams, mesh_uptime_seconds, mesh_auth_failures_total.'},
  {method:'GET', path:'/api/filesync/folders', desc:'Filesync folder statuses as JSON array: id, path, direction, file_count, peers.'},
  {method:'GET', path:'/api/filesync/conflicts', desc:'Conflict files as JSON array: folder_id, path.'},
  {method:'GET', path:'/api/gateway/audit?gateway=NAME&limit=N&session=&model=&outcome=&since=&until=&min_tokens=', desc:'Recent audit rows. Filters: session (12-char hex from messages[0] hash), model, outcome (ok|error|truncated|client_cancelled), since/until (RFC3339), min_tokens (req+resp pair total). Returns paired rows.'},
  {method:'GET', path:'/api/gateway/audit/pair?gateway=NAME&id=N&run=HEX', desc:'Full request and response rows for a single audit pair. Required: gateway, id, run. Used by the detail card to fetch bodies on demand.'},
  {method:'GET', path:'/api/gateway/audit/stats?gateway=NAME&window=24h&bucket=hour&session=&model=', desc:'Aggregated counters: totals (input/output/cache/reasoning tokens, cache_hit_ratio), by_model, by_session, time series. Window: 1h|24h|7d|30d|all|<duration>. Bucket: minute|hour|day.'},
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

// --- Clipsync ---
function renderClipsync() {
  const sends = clipActivities.filter(a => a.direction === 'send').length;
  const recvs = clipActivities.filter(a => a.direction === 'receive').length;
  const totalSize = clipActivities.reduce((s,a) => s + (a.size||0), 0);
  document.getElementById('cs-stats').innerHTML =
    stat('Total Events', clipActivities.length, sends+' sent, '+recvs+' received') +
    stat('Total Size', fmtBytes(totalSize), '');

  const el = document.getElementById('cs-body');
  if (!clipActivities.length) {
    el.innerHTML = '<tr><td colspan="5" style="color:var(--text-muted);padding:20px">No clipboard activity yet</td></tr>';
    return;
  }
  el.innerHTML = clipActivities.map(a => {
    const dir = a.direction === 'send'
      ? '<span style="color:var(--green)">&#x2191; send</span>'
      : '<span style="color:var(--purple)">&#x2193; receive</span>';
    const fmts = (a.formats||[]).map(f => x(f)).join(', ') || '<span style="color:var(--text-muted)">-</span>';
    const peer = a.peer ? x(a.peer) : '<span style="color:var(--text-muted)">-</span>';
    const ago = timeAgo(a.time);
    return '<tr><td>'+dir+'</td><td>'+fmtBytes(a.size)+'</td><td style="color:var(--text-dim)">'+fmts+'</td><td>'+peer+'</td><td style="color:var(--text-muted)">'+ago+'</td></tr>';
  }).join('');
}

function fmtBytes(b) {
  if (b >= 1<<20) return (b/(1<<20)).toFixed(1)+' MB';
  if (b >= 1<<10) return (b/(1<<10)).toFixed(1)+' KB';
  return b+' B';
}

function timeAgo(ts) {
  if (!ts) return '-';
  const d = Date.now() - new Date(ts).getTime();
  if (d < 1000) return 'just now';
  if (d < 60000) return Math.floor(d/1000)+'s ago';
  if (d < 3600000) return Math.floor(d/60000)+'m ago';
  return Math.floor(d/3600000)+'h ago';
}

// --- Gateway audit ---
let gwSelected = '';
let gwRowsCache = []; // resp rows joined with their req row, newest first
let gwSearchTerm = '';
let gwOutcomeFilter = '';

function renderGateway() {
  const sel = document.getElementById('gw-select');
  if (!sel) return;
  if (!gatewayAudit || !gatewayAudit.length) {
    sel.innerHTML = '<option value="">(no gateways with audit logging)</option>';
    document.getElementById('gw-meta').textContent = '';
    document.getElementById('gw-body').innerHTML =
      '<tr><td colspan="9" style="color:var(--text-muted);padding:20px">No gateways with audit logging configured. Set log.level: full or metadata in the gateway YAML to populate this view.</td></tr>';
    return;
  }
  // Populate selector once / on changes.
  const names = gatewayAudit.map(g => g.gateway);
  const desired = (sel.options[sel.selectedIndex]||{}).value || gwSelected || names[0];
  if (sel.options.length !== names.length || Array.from(sel.options).some((o,i)=>o.value!==names[i])) {
    sel.innerHTML = names.map(n => '<option value="'+x(n)+'">'+x(n)+'</option>').join('');
    sel.value = names.includes(desired) ? desired : names[0];
  }
  gwSelected = sel.value;

  const entry = gatewayAudit.find(g => g.gateway === gwSelected) || gatewayAudit[0];
  const rowsRaw = entry.rows || [];
  // Pair req with resp by id+run.
  const reqs = new Map();
  const pairs = [];
  for (const r of rowsRaw) {
    const key = (r.run||'')+'|'+r.id;
    if (r.t === 'req') {
      reqs.set(key, r);
    } else if (r.t === 'resp') {
      pairs.push({req: reqs.get(key) || {}, resp: r});
    }
  }
  pairs.reverse(); // newest first
  gwRowsCache = pairs;

  document.getElementById('gw-meta').innerHTML =
    'gateway <b>'+x(entry.gateway)+'</b> · file <span style="color:var(--text-dim)">'+x(entry.file||'(none)')+'</span>'+
    (entry.file_size ? ' · '+fmtBytes(entry.file_size) : '')+
    ' · '+pairs.length+' completed requests'+
    (entry.error ? ' · <span style="color:var(--red)">error: '+x(entry.error)+'</span>' : '');

  const term = (gwSearchTerm||'').toLowerCase();
  const outcomeFilter = gwOutcomeFilter;
  const filtered = pairs.filter(p => {
    if (outcomeFilter && (p.resp.outcome||'') !== outcomeFilter) return false;
    if (!term) return true;
    return JSON.stringify(p.req).toLowerCase().includes(term) ||
           JSON.stringify(p.resp).toLowerCase().includes(term);
  });

  const body = document.getElementById('gw-body');
  if (!filtered.length) {
    body.innerHTML = '<tr><td colspan="9" style="color:var(--text-muted);padding:20px">No rows match the current filter.</td></tr>';
    return;
  }
  body.innerHTML = filtered.map((p, idx) => {
    const time = (p.resp.ts||p.req.ts||'').replace('T',' ').replace(/\..*Z/,'Z');
    const dir = p.req.direction || '-';
    const model = p.req.model || '-';
    const stream = p.req.stream ? 'yes' : 'no';
    const status = p.resp.status || 0;
    const statusColor = status >= 400 ? 'var(--red)' : status >= 200 ? 'var(--green)' : 'var(--text-dim)';
    const outcome = p.resp.outcome || '-';
    const outcomeColor = outcome === 'ok' ? 'var(--green)' : outcome === 'error' ? 'var(--red)' : 'var(--yellow)';
    const u = p.resp.usage || {};
    const tokens = (u.input_tokens||0)+'/'+(u.output_tokens||0);
    const elapsed = (p.resp.elapsed_ms||0)+'ms';
    const summary = renderGwSummaryCell(p.resp);
    return '<tr style="cursor:pointer" onclick="showGwDetail('+idx+')">'+
      '<td style="color:var(--text-muted);white-space:nowrap">'+x(time)+'</td>'+
      '<td>'+x(dir)+'</td>'+
      '<td style="color:var(--text-dim)">'+x(model)+'</td>'+
      '<td style="color:var(--text-muted)">'+stream+'</td>'+
      '<td style="color:'+statusColor+'">'+status+'</td>'+
      '<td style="color:'+outcomeColor+'">'+x(outcome)+'</td>'+
      '<td style="color:var(--text-dim)">'+tokens+'</td>'+
      '<td style="color:var(--text-muted)">'+elapsed+'</td>'+
      '<td style="max-width:400px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text-dim)">'+summary+'</td>'+
      '</tr>';
  }).join('');
  // Stash filtered rows for click-to-detail.
  gwRowsCache = filtered;
}

function renderGwSummaryCell(resp) {
  const s = resp.stream_summary;
  if (s && s.content) return x(s.content.slice(0, 120));
  if (s && s.errors && s.errors.length) return '<span style="color:var(--red)">'+x(s.errors.join('; '))+'</span>';
  if (typeof resp.body === 'string') return x(resp.body.slice(0, 120));
  if (resp.body && typeof resp.body === 'object') return x(JSON.stringify(resp.body).slice(0, 120));
  return '<span style="color:var(--text-muted)">-</span>';
}

// gwDetailCache holds the currently shown pair so the copy buttons and any
// future tab switching can reach it without re-fetching.
let gwDetailCache = {req: null, resp: null};

function showGwDetail(idx) {
  const p = gwRowsCache[idx];
  if (!p) return;
  gwDetailCache = {req: p.req, resp: p.resp};
  document.getElementById('gw-detail-card').style.display = 'block';
  const sid = p.req.session_id ? ' · session '+p.req.session_id : '';
  const turn = p.req.turn_index ? ' · turn '+p.req.turn_index : '';
  document.getElementById('gw-detail-title').textContent =
    'Request '+(p.req.id||'?')+' (run '+(p.req.run||p.resp.run||'?')+')' + sid + turn;
  document.getElementById('gw-req-raw').innerHTML = highlightJSON(p.req);
  document.getElementById('gw-resp-raw').innerHTML = highlightJSON(p.resp);
  document.getElementById('gw-req-structured').innerHTML = renderRequestStructured(p.req);
  document.getElementById('gw-resp-structured').innerHTML = renderResponseStructured(p.resp);
}

// --- Structured response view ---
//
// Renders chips (status, outcome, stop_reason, elapsed, events) plus the
// per-pair token bar. The body view dispatches on stream_summary first
// (streamed responses) and falls back to the parsed body for buffered
// JSON. tool_calls and errors are surfaced explicitly because they are the
// signals a user opens this view to find.
function renderResponseStructured(resp) {
  let html = '';
  const chips = [];
  if (resp.status) chips.push(chip('status', resp.status, resp.status >= 400 ? 'error' : 'ok'));
  if (resp.outcome) chips.push(chip('outcome', resp.outcome,
    resp.outcome === 'ok' ? 'ok' : resp.outcome === 'error' ? 'error' : 'warn'));
  const summary = resp.stream_summary || {};
  if (summary.stop_reason) chips.push(chip('stop', summary.stop_reason));
  if (resp.elapsed_ms) chips.push(chip('elapsed', resp.elapsed_ms+'ms'));
  if (summary.events) chips.push(chip('events', summary.events));
  if (summary.message_id) chips.push(chip('msg_id', summary.message_id));
  if (summary.model) chips.push(chip('upstream_model', summary.model));
  if (chips.length) html += '<div style="margin-bottom:8px">' + chips.join('') + '</div>';

  // Token breakdown bar — visible whenever any of the four buckets is non-zero.
  const u = resp.usage || summary.usage;
  if (u && (u.input_tokens || u.output_tokens || u.cache_read_input_tokens || u.cache_creation_input_tokens)) {
    html += renderTokenBar(u);
  }

  // Mid-stream errors (Anthropic event:error) — show in red, prominent.
  if (Array.isArray(summary.errors) && summary.errors.length) {
    html += '<div class="section-title">stream errors</div>';
    html += summary.errors.map(e =>
      '<div style="border-left:2px solid var(--red);padding:4px 8px;color:var(--red)">'+x(e)+'</div>'
    ).join('');
  }

  // Reassembled assistant content (streamed). When buffered, the body has
  // an Anthropic-shaped content array we can render with the same dispatcher.
  if (summary.content) {
    html += '<div class="section-title">content</div>';
    html += '<div class="msg-block"><div class="msg-body">'+renderText(summary.content)+'</div></div>';
  }
  if (summary.thinking) {
    html += '<div class="section-title">thinking</div>';
    html += '<div style="border-left:2px solid var(--purple);padding:4px 8px;color:var(--text-dim);font-style:italic">'+
      renderText(summary.thinking)+'</div>';
  }
  if (Array.isArray(summary.tool_calls) && summary.tool_calls.length) {
    html += '<div class="section-title">tool calls</div>';
    html += summary.tool_calls.map(tc =>
      '<div class="tool-block">' +
        '<span class="tool-name">'+x(tc.name||'?')+'</span> ' +
        '<span style="color:var(--text-muted)">id='+x(tc.id||'')+'</span>' +
        '<pre>'+x(typeof tc.args === 'string' ? tc.args : JSON.stringify(tc.args, null, 2))+'</pre>' +
      '</div>'
    ).join('');
  }

  // Buffered (non-streamed) JSON body — render Anthropic content blocks the
  // same way the request side does.
  if (resp.body && typeof resp.body === 'object' && !summary.content) {
    if (Array.isArray(resp.body.content)) {
      html += '<div class="section-title">content</div>';
      html += '<div class="msg-block"><div class="msg-body">' +
        resp.body.content.map(renderContentBlock).join('') + '</div></div>';
    } else if (Array.isArray(resp.body.choices)) {
      // OpenAI shape.
      html += '<div class="section-title">choices ('+resp.body.choices.length+')</div>';
      html += resp.body.choices.map((c, i) => {
        const msg = c.message || {};
        const inner = renderContent(msg.content);
        const calls = Array.isArray(msg.tool_calls) ? msg.tool_calls.map(renderOpenAIToolCall).join('') : '';
        return '<div class="msg-block"><div class="msg-head">' +
          '<span class="msg-role assistant">'+x(msg.role||'assistant')+'</span>' +
          '<span style="color:var(--text-muted)">#'+(i+1)+'</span>' +
          (c.finish_reason ? '<span class="chip">finish_reason <b>'+x(c.finish_reason)+'</b></span>' : '') +
        '</div><div class="msg-body">'+inner+calls+'</div></div>';
      }).join('');
    }
  }
  return html || '<span class="json-summary">(no body captured — set log.level: full to see content)</span>';
}

// renderTokenBar draws the four-segment horizontal stack used by both the
// detail card and (later) the overview view. Widths are proportional to the
// total of all four buckets; an empty bucket renders a 0-width segment.
function renderTokenBar(u) {
  const cacheRead   = u.cache_read_input_tokens || 0;
  const cacheCreate = u.cache_creation_input_tokens || 0;
  const fresh       = u.input_tokens || 0;
  const out         = u.output_tokens || 0;
  const total = cacheRead + cacheCreate + fresh + out;
  if (total === 0) return '';
  const pct = n => total === 0 ? 0 : (n / total * 100).toFixed(1);
  const seg = (cls, label, n) => {
    if (n === 0) return '';
    return '<div class="'+cls+'" style="flex:'+n+'" title="'+label+': '+n.toLocaleString()+' ('+pct(n)+'%)">'+
      (n / total > 0.08 ? n.toLocaleString() : '') + '</div>';
  };
  return '<div class="section-title">tokens (total '+total.toLocaleString()+')</div>' +
    '<div class="token-legend">' +
      '<span><i class="seg-cache-read" style="background:var(--green)"></i>cache read '+cacheRead.toLocaleString()+'</span>' +
      '<span><i class="seg-cache-create" style="background:var(--purple)"></i>cache write '+cacheCreate.toLocaleString()+'</span>' +
      '<span><i class="seg-input" style="background:var(--cyan)"></i>fresh input '+fresh.toLocaleString()+'</span>' +
      '<span><i class="seg-output" style="background:var(--yellow)"></i>output '+out.toLocaleString()+'</span>' +
    '</div>' +
    '<div class="token-bar">' +
      seg('seg-cache-read', 'cache read', cacheRead) +
      seg('seg-cache-create', 'cache write', cacheCreate) +
      seg('seg-input', 'fresh input', fresh) +
      seg('seg-output', 'output', out) +
    '</div>';
}

// --- Structured request view ---
//
// Renders the parsed body of a request row as a stack of message cards plus
// chip metadata. Anthropic and OpenAI message shapes are unified: a text
// content string is shown verbatim, an array of content blocks is unfolded
// per type (text | image | tool_use | tool_result for Anthropic; text +
// tool_calls + tool_call_id for OpenAI). Long text auto-truncates with an
// expand toggle so a 200KB system prompt does not blow up the pane.
const truncateLen = 400;
function renderRequestStructured(req) {
  const body = req.body;
  if (!body || typeof body !== 'object') {
    return '<span class="json-summary">(body not captured — set log.level: full)</span>';
  }
  let html = '';
  // Header chips: model, stream, session, turn, temperature, max_tokens.
  const chips = [];
  if (body.model) chips.push(chip('model', body.model));
  if (req.stream || body.stream) chips.push(chip('stream', 'true'));
  if (req.session_id) chips.push(chip('session', req.session_id));
  if (req.turn_index) chips.push(chip('turn', req.turn_index));
  if (typeof body.temperature === 'number') chips.push(chip('temp', body.temperature));
  if (body.max_tokens) chips.push(chip('max_tokens', body.max_tokens));
  if (body.top_p) chips.push(chip('top_p', body.top_p));
  if (chips.length) html += '<div style="margin-bottom:8px">' + chips.join('') + '</div>';

  // System prompt (Anthropic top-level string or array of content blocks;
  // OpenAI inlines the system message in the messages array, so it appears
  // there instead).
  if (body.system) {
    html += '<div class="section-title">system</div>';
    html += '<div class="msg-block"><div class="msg-body">' +
      renderContent(body.system) + '</div></div>';
  }

  // Messages.
  const msgs = Array.isArray(body.messages) ? body.messages : [];
  if (msgs.length) {
    html += '<div class="section-title">messages (' + msgs.length + ')</div>';
    html += msgs.map((m, i) => renderMessage(m, i)).join('');
  }

  // Tools available to the model.
  const tools = Array.isArray(body.tools) ? body.tools : [];
  if (tools.length) {
    html += '<div class="section-title">tools (' + tools.length + ')</div>';
    html += tools.map(renderToolDefinition).join('');
  }
  return html || '<span class="json-summary">(empty body)</span>';
}

function chip(label, value, cls) {
  const c = cls ? ' '+cls : '';
  return '<span class="chip'+c+'">'+x(label)+' <b>'+x(String(value))+'</b></span>';
}

function renderMessage(m, idx) {
  const role = String(m.role || 'unknown');
  const content = m.content;
  const inner = renderContent(content);
  const toolID = m.tool_call_id ? '<span class="chip">tool_call_id <b>'+x(m.tool_call_id)+'</b></span>' : '';
  const calls = Array.isArray(m.tool_calls) ? m.tool_calls.map(renderOpenAIToolCall).join('') : '';
  return '<div class="msg-block">' +
    '<div class="msg-head">' +
      '<span class="msg-role '+x(role)+'">'+x(role)+'</span>' +
      '<span style="color:var(--text-muted)">#'+(idx+1)+'</span>' +
      toolID +
    '</div>' +
    '<div class="msg-body">' + inner + calls + '</div>' +
  '</div>';
}

// renderContent dispatches on the shape: a string is shown verbatim with
// truncation; an array is unfolded per Anthropic content-block type; an
// object falls back to highlighted JSON so nothing is hidden.
function renderContent(content) {
  if (content == null) return '';
  if (typeof content === 'string') return renderText(content);
  if (Array.isArray(content)) return content.map(renderContentBlock).join('');
  return '<pre style="font-family:var(--mono);font-size:11px;color:var(--text-dim);white-space:pre-wrap">' +
    x(JSON.stringify(content, null, 2)) + '</pre>';
}

function renderContentBlock(b) {
  if (!b || typeof b !== 'object') return '';
  switch (b.type) {
    case 'text':
      return renderText(b.text || '');
    case 'image':
      return renderImage(b);
    case 'tool_use':
      return '<div class="tool-block">' +
        '<span class="tool-name">tool_use: '+x(b.name||'?')+'</span> ' +
        '<span style="color:var(--text-muted)">id='+x(b.id||'')+'</span>' +
        '<pre>'+x(JSON.stringify(b.input || {}, null, 2))+'</pre>' +
      '</div>';
    case 'tool_result':
      return '<div class="tool-block" style="border-left-color:var(--green)">' +
        '<span class="tool-name" style="color:var(--green)">tool_result</span> ' +
        '<span style="color:var(--text-muted)">tool_use_id='+x(b.tool_use_id||'')+'</span>' +
        (b.is_error ? ' <span class="chip error">error</span>' : '') +
        '<div style="margin-top:4px">'+renderContent(b.content)+'</div>' +
      '</div>';
    case 'thinking':
      return '<div style="border-left:2px solid var(--purple);padding:4px 8px;color:var(--text-dim);font-style:italic">'+
        renderText(b.thinking || '') + '</div>';
    default:
      return '<div class="tool-block"><span class="tool-name" style="color:var(--text-dim)">'+x(b.type||'unknown')+'</span>' +
        '<pre>'+x(JSON.stringify(b, null, 2))+'</pre></div>';
  }
}

function renderImage(b) {
  const src = b.source || {};
  if (src.type === 'base64') {
    const data = src.data ? 'data:'+x(src.media_type||'image/png')+';base64,'+x(src.data) : '';
    return data ? '<img src="'+data+'" style="max-width:200px;max-height:200px;border:1px solid var(--border);border-radius:4px"/>'
                : '<span class="json-summary">(image, base64)</span>';
  }
  if (src.type === 'url' && src.url) {
    return '<a href="'+x(src.url)+'" target="_blank" rel="noopener" style="color:var(--cyan)">[image: '+x(src.url)+']</a>';
  }
  return '<span class="json-summary">(image, '+x(src.type||'unknown')+')</span>';
}

function renderText(s) {
  s = String(s);
  if (s.length <= truncateLen) {
    return '<div class="text">'+x(s)+'</div>';
  }
  const id = 'tx-'+(_hjId++);
  return '<div class="text" id="'+id+'-short">'+x(s.slice(0, truncateLen))+'…' +
    ' <span class="truncate" onclick="_txExpand(\''+id+'\')">expand ('+s.length+' chars)</span></div>' +
    '<div class="text json-collapsed" id="'+id+'-full">'+x(s)+
    ' <span class="truncate" onclick="_txCollapse(\''+id+'\')">collapse</span></div>';
}
function _txExpand(id) {
  document.getElementById(id+'-short').classList.add('json-collapsed');
  document.getElementById(id+'-full').classList.remove('json-collapsed');
}
function _txCollapse(id) {
  document.getElementById(id+'-short').classList.remove('json-collapsed');
  document.getElementById(id+'-full').classList.add('json-collapsed');
}

function renderOpenAIToolCall(tc) {
  if (!tc || !tc.function) return '';
  return '<div class="tool-block">' +
    '<span class="tool-name">tool_call: '+x(tc.function.name||'?')+'</span> ' +
    '<span style="color:var(--text-muted)">id='+x(tc.id||'')+'</span>' +
    '<pre>'+x(tc.function.arguments||'')+'</pre>' +
  '</div>';
}

function renderToolDefinition(t) {
  const name = t.name || (t.function && t.function.name) || 'unknown';
  const desc = t.description || (t.function && t.function.description) || '';
  const schema = t.input_schema || (t.function && t.function.parameters) || {};
  return '<details class="tool-block">' +
    '<summary><span class="tool-name">'+x(name)+'</span>' +
    (desc ? ' <span style="color:var(--text-dim);font-style:italic">'+x(desc.slice(0, 120))+'</span>' : '') +
    '</summary>' +
    '<pre>'+x(JSON.stringify(schema, null, 2))+'</pre>' +
  '</details>';
}

function copyDetail(which) {
  const v = gwDetailCache[which];
  if (!v) return;
  navigator.clipboard.writeText(JSON.stringify(v, null, 2)).catch(()=>{});
}

// highlightJSON returns syntax-highlighted HTML for any JSON-compatible value.
// Walks the structure once, emitting span-classed tokens. Strings are escaped
// against XSS at the leaf. Objects/arrays past collapseAt entries fold by
// default with a clickable toggle; click expands them in place.
//
// Implementation note: we do this from the parsed value (not from a JSON
// string + regex tokenize) because the audit rows arrive as JS objects via
// fetch().json(); reserializing then regex-tokenizing would lose object key
// order on most engines and double the work.
const collapseAt = 30;
function highlightJSON(value) {
  return _hjVal(value, 0);
}
function _hjEsc(s) {
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function _hjIndent(n) { return '  '.repeat(n); }
function _hjVal(v, depth) {
  if (v === null) return '<span class="json-null">null</span>';
  if (typeof v === 'boolean') return '<span class="json-bool">'+v+'</span>';
  if (typeof v === 'number') return '<span class="json-num">'+v+'</span>';
  if (typeof v === 'string') return '<span class="json-str">"'+_hjEsc(v)+'"</span>';
  if (Array.isArray(v)) return _hjArr(v, depth);
  if (typeof v === 'object') return _hjObj(v, depth);
  return _hjEsc(String(v));
}
function _hjFold(open, close, items, depth, summaryText) {
  if (items.length === 0) return '<span class="json-punct">'+open+close+'</span>';
  const id = 'hj-'+(_hjId++);
  const collapsed = items.length > collapseAt;
  // Always emit a summary span; CSS toggles its visibility together with the
  // inner block so a single class flip on the inner span reverses the view.
  return '<span class="json-punct">'+open+'</span>' +
    '<span class="json-toggle" data-target="'+id+'" onclick="_hjTog(this)">'+(collapsed?'+':'-')+'</span>' +
    '<span class="json-summary"'+(collapsed?'':' style="display:none"')+'>'+summaryText+'</span>' +
    '<span id="'+id+'" class="'+(collapsed?'json-collapsed':'')+'">' +
    '\n' + items.join('\n') + '\n' + _hjIndent(depth) +
    '</span><span class="json-punct">'+close+'</span>';
}
function _hjArr(arr, depth) {
  const items = arr.map((v, i) =>
    _hjIndent(depth+1) + _hjVal(v, depth+1) +
    (i < arr.length-1 ? '<span class="json-punct">,</span>' : '')
  );
  return _hjFold('[', ']', items, depth, '['+arr.length+' items]');
}
function _hjObj(obj, depth) {
  const keys = Object.keys(obj);
  const items = keys.map((k, i) =>
    _hjIndent(depth+1) +
    '<span class="json-key">"'+_hjEsc(k)+'"</span>' +
    '<span class="json-punct">: </span>' +
    _hjVal(obj[k], depth+1) +
    (i < keys.length-1 ? '<span class="json-punct">,</span>' : '')
  );
  return _hjFold('{', '}', items, depth, '{'+keys.length+' fields}');
}
let _hjId = 0;
function _hjTog(togEl) {
  const inner = document.getElementById(togEl.dataset.target);
  if (!inner) return;
  const summary = togEl.nextElementSibling;
  inner.classList.toggle('json-collapsed');
  const isCollapsed = inner.classList.contains('json-collapsed');
  togEl.textContent = isCollapsed ? '+' : '-';
  if (summary && summary.classList.contains('json-summary')) {
    summary.style.display = isCollapsed ? '' : 'none';
  }
}

document.getElementById('gw-search').addEventListener('input', e => { gwSearchTerm = e.target.value; renderGateway(); });
document.getElementById('gw-outcome').addEventListener('change', e => { gwOutcomeFilter = e.target.value; renderGateway(); });
document.getElementById('gw-select').addEventListener('change', e => { gwSelected = e.target.value; renderGateway(); });

// --- Debug ---
function renderDebugStats() {
  const goroutines = valMetric(metricsText, 'mesh_process_goroutines') || 0;
  const fds = valMetric(metricsText, 'mesh_process_open_fds');
  const stateComps = valMetric(metricsText, 'mesh_state_components') || 0;
  const stateMetrics = valMetric(metricsText, 'mesh_state_metrics') || 0;
  let html = stat('Goroutines', goroutines, '') +
    stat('State Components', stateComps, '') +
    stat('State Metrics', stateMetrics, '');
  if (fds !== null) html += stat('Open FDs', fds, '', fds > 10000 ? 'var(--red)' : fds > 1000 ? 'var(--yellow)' : '');
  document.getElementById('dbg-stats').innerHTML = html;
}

async function dbgProfile(name) {
  const card = document.getElementById('dbg-result-card');
  const el = document.getElementById('dbg-result');
  const title = document.getElementById('dbg-result-title');
  card.style.display = 'block';
  title.textContent = name + ' profile';
  el.textContent = 'Loading...';
  try {
    const r = await fetch('/debug/pprof/' + name + '?debug=1');
    el.textContent = await r.text();
  } catch(e) { el.textContent = 'Error: ' + e.message; }
}

async function dbgCpuProfile() {
  const card = document.getElementById('dbg-result-card');
  const el = document.getElementById('dbg-result');
  const title = document.getElementById('dbg-result-title');
  card.style.display = 'block';
  title.textContent = 'CPU Profile';
  el.textContent = 'Collecting CPU profile for 10 seconds...\nThe result will download as a binary file for use with:\n  go tool pprof <file>';
  try {
    const r = await fetch('/debug/pprof/profile?seconds=10');
    const blob = await r.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = 'cpu-profile.pb.gz'; a.click();
    URL.revokeObjectURL(url);
    el.textContent = 'CPU profile downloaded.\nAnalyze with:\n  go tool pprof cpu-profile.pb.gz';
  } catch(e) { el.textContent = 'Error: ' + e.message; }
}

async function dbgTrace() {
  const card = document.getElementById('dbg-result-card');
  const el = document.getElementById('dbg-result');
  const title = document.getElementById('dbg-result-title');
  card.style.display = 'block';
  title.textContent = 'Execution Trace';
  el.textContent = 'Collecting trace for 5 seconds...\nThe result will download as a binary file for use with:\n  go tool trace <file>';
  try {
    const r = await fetch('/debug/pprof/trace?seconds=5');
    const blob = await r.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = 'trace.out'; a.click();
    URL.revokeObjectURL(url);
    el.textContent = 'Trace downloaded.\nAnalyze with:\n  go tool trace trace.out';
  } catch(e) { el.textContent = 'Error: ' + e.message; }
}

// --- Start ---
tick();
setInterval(tick, 1000);
</script>
</body>
</html>`
