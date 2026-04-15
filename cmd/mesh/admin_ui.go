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
/* Help icon (?) tooltip */
.help {
  display: inline-block; width: 14px; height: 14px; line-height: 13px;
  text-align: center; border-radius: 50%; border: 1px solid var(--border);
  color: var(--text-muted); font-size: 9px; font-family: var(--mono);
  cursor: help; margin-left: 4px; vertical-align: middle;
}
.help:hover { color: var(--green); border-color: var(--green); }

.empty-note {
  color: var(--text-muted); font-style: italic; font-size: 12px;
  padding: 8px 4px;
}

/* Chat-style messages */
.chat { display: flex; flex-direction: column; gap: 8px; padding: 4px 0; }
.chat-row { display: flex; gap: 6px; align-items: flex-start; }
.chat-row.right { justify-content: flex-end; }
.chat-row.center { justify-content: center; }
.bubble {
  max-width: min(78%, 720px); min-width: 0;
  border: 1px solid var(--border); border-radius: 10px;
  padding: 8px 10px; background: var(--bg-card);
  font-size: 13px; line-height: 1.45;
  display: flex; flex-direction: column; gap: 4px;
}
@media (max-width: 700px) { .bubble { max-width: 100%; } }
.bubble.role-user      { border-color: var(--cyan);   border-top-right-radius: 2px; background: linear-gradient(180deg, rgba(34,211,238,0.05), transparent); }
.bubble.role-assistant { border-color: var(--green);  border-top-left-radius: 2px;  background: linear-gradient(180deg, rgba(52,211,153,0.05), transparent); }
.bubble.role-system    { border-color: var(--purple); width: 100%; max-width: 100%; background: linear-gradient(180deg, rgba(167,139,250,0.06), transparent); }
.bubble.role-tool      { border-color: var(--yellow); border-top-left-radius: 2px;  background: linear-gradient(180deg, rgba(251,191,36,0.05), transparent); }
.bubble.role-unknown   { border-color: var(--text-muted); }
.bubble .b-foot {
  display: flex; align-items: center; gap: 8px;
  font-size: 10px; color: var(--text-muted);
  border-top: 1px dashed var(--border); padding-top: 4px; margin-top: 2px;
}
.bubble .b-foot .role-pill {
  padding: 0 6px; border-radius: 3px; background: var(--bg-input);
  text-transform: uppercase; letter-spacing: 0.5px; font-weight: 600; font-size: 9px;
}
.bubble.role-user      .b-foot .role-pill { color: var(--cyan); }
.bubble.role-assistant .b-foot .role-pill { color: var(--green); }
.bubble.role-system    .b-foot .role-pill { color: var(--purple); }
.bubble.role-tool      .b-foot .role-pill { color: var(--yellow); }
.bubble .b-foot .b-len { margin-left: auto; font-family: var(--mono); }
.bubble .b-foot a, .bubble .b-foot button {
  background: none; border: none; color: var(--text-muted);
  cursor: pointer; font-size: 10px; padding: 0;
}
.bubble .b-foot a:hover, .bubble .b-foot button:hover { color: var(--green); }

/* Pre/post-context drawers on user bubbles */
.bubble .pre-ctx, .bubble .post-ctx {
  margin-top: 6px; border-top: 1px dashed var(--border); padding-top: 6px;
  font-size: 11px;
}
.bubble .pre-ctx > summary, .bubble .post-ctx > summary {
  list-style: none; cursor: pointer; color: var(--text-muted);
  display: flex; align-items: center; gap: 6px; padding: 2px 0;
}
.bubble .pre-ctx > summary::-webkit-details-marker,
.bubble .post-ctx > summary::-webkit-details-marker { display: none; }
.bubble .pre-ctx > summary::before, .bubble .post-ctx > summary::before {
  content: '▶'; font-size: 8px; color: var(--text-muted);
  transition: transform 0.1s;
}
.bubble .pre-ctx[open] > summary::before,
.bubble .post-ctx[open] > summary::before { transform: rotate(90deg); }
.bubble .pre-ctx > summary .ctx-label,
.bubble .post-ctx > summary .ctx-label { font-weight: 600; color: var(--text); }
.bubble .pre-ctx > summary .ctx-summary-meta,
.bubble .post-ctx > summary .ctx-summary-meta { margin-left: auto; color: var(--text-dim); font-family: var(--mono); }

/* Block list — one row per injected block, name + size + % of message.
   Explicit display rules on both the closed and open states are required
   because author CSS (display:flex / display:block) overrides the UA
   stylesheet's display:none for hidden <details> children. Without these
   rules the ctx-list and ctx-body are always visible even when closed. */
.pre-ctx > .ctx-list,
.post-ctx > .ctx-list { display: none; }
.pre-ctx[open] > .ctx-list,
.post-ctx[open] > .ctx-list { display: flex; flex-direction: column; gap: 2px; margin-top: 6px; }
.ctx-item > .ctx-body { display: none; }
.ctx-item[open] > .ctx-body { display: block; }
.ctx-list { display: flex; flex-direction: column; gap: 2px; }
.ctx-item { border: 1px solid var(--border); border-radius: 3px; background: var(--bg); }
.ctx-item > summary {
  list-style: none; cursor: pointer; user-select: none;
  display: grid; grid-template-columns: 14px 1fr auto auto; gap: 8px;
  align-items: center; padding: 4px 8px; font-size: 11px;
}
.ctx-item > summary::-webkit-details-marker { display: none; }
.ctx-item > summary::before {
  content: '▶'; font-size: 8px; color: var(--text-muted);
  transition: transform 0.1s;
}
.ctx-item[open] > summary::before { transform: rotate(90deg); }
.ctx-item:hover > summary { background: var(--bg-hover); }
.ctx-item .ctx-name { font-family: var(--mono); color: var(--text); white-space: nowrap; }
.ctx-item .ctx-name .ctx-preview { color: var(--text-muted); font-weight: normal; overflow: hidden; text-overflow: ellipsis; }
.ctx-item .ctx-size { font-family: var(--mono); color: var(--text-muted); min-width: 80px; text-align: right; }
.ctx-item .ctx-pct  { font-family: var(--mono); color: var(--text-dim); min-width: 52px; text-align: right; }
.ctx-item.k-system-reminder   > summary .ctx-name { color: var(--yellow); }
.ctx-item.k-command           > summary .ctx-name { color: var(--cyan); }
.ctx-item.k-stdout            > summary .ctx-name { color: var(--text); }
.ctx-item.k-stderr            > summary .ctx-name { color: var(--red); }
.ctx-item.k-task-notification > summary .ctx-name { color: var(--purple); }
.ctx-item.k-hook              > summary .ctx-name { color: var(--purple); }
.ctx-item.k-unknown           > summary .ctx-name { color: var(--blue); }
.ctx-item > .ctx-body {
  border-top: 1px solid var(--border);
  padding: 6px 10px; white-space: pre-wrap; word-break: break-word;
  font-size: 11px; color: var(--text); max-height: 360px; overflow-y: auto;
}

/* Bubble flash highlight (used for tool_use_id click-back) */
@keyframes bubble-flash {
  0%   { box-shadow: 0 0 0 0 var(--green); }
  50%  { box-shadow: 0 0 0 4px rgba(52,211,153,0.4); }
  100% { box-shadow: 0 0 0 0 transparent; }
}
.bubble.flash { animation: bubble-flash 1.2s ease-out 1; }

/* Custom embedded blocks (system-reminder, command-*, task-notification, ...) */
.cblock {
  border: 1px solid var(--border); border-left-width: 3px;
  border-radius: 4px; margin: 6px 0; background: var(--bg-card);
  font-size: 12px;
}
.cblock > .cblock-head {
  display: flex; align-items: center; gap: 6px;
  padding: 4px 8px; font-family: var(--mono); font-size: 11px;
  color: var(--text-dim); border-bottom: 1px solid var(--border);
  cursor: pointer; user-select: none;
}
.cblock > .cblock-head:hover { background: var(--bg-hover); }
.cblock > .cblock-head .icon { font-size: 13px; }
.cblock > .cblock-head .name { font-weight: 600; color: var(--text); }
.cblock > .cblock-head .sec-len { margin-left: auto; }
.cblock > .cblock-body { padding: 8px; white-space: pre-wrap; word-break: break-word; color: var(--text); }
.cblock > .cblock-body pre { font-family: var(--mono); font-size: 11px; }
.cblock.json-collapsed > .cblock-body { display: none; }

.cblock.k-system-reminder    { border-left-color: var(--yellow); }
.cblock.k-command            { border-left-color: var(--cyan); }
.cblock.k-stdout             { border-left-color: var(--text-dim); }
.cblock.k-stderr             { border-left-color: var(--red); }
.cblock.k-task-notification  { border-left-color: var(--purple); }
.cblock.k-hook               { border-left-color: var(--purple); }
.cblock.k-unknown            { border-left-color: var(--blue); }
.cblock.k-system-reminder .name { color: var(--yellow); }
.cblock.k-command         .name { color: var(--cyan); }
.cblock.k-stderr          .name { color: var(--red); }
.cblock.k-task-notification .name { color: var(--purple); }
.cblock.k-hook .name { color: var(--purple); }

/* Sub-view segmented control */
.gw-sub-btn {
  padding: 6px 14px; cursor: pointer; font-size: 12px;
  background: var(--bg-input); color: var(--text-dim);
  border-right: 1px solid var(--border);
}
.gw-sub-btn:last-child { border-right: none; }
.gw-sub-btn.active { background: var(--bg-card); color: var(--green); }
.gw-sub-btn:hover { color: var(--text); }

/* Sessions sub-view layout */
.gw-session-grid { display: grid; grid-template-columns: 280px 1fr; gap: 12px; }
@media (max-width: 900px) { .gw-session-grid { grid-template-columns: 1fr; } }
.gw-sess-row {
  padding: 8px 12px; border-bottom: 1px solid var(--border);
  cursor: pointer; font-size: 12px;
}
.gw-sess-row:hover { background: var(--bg-hover); }
.gw-sess-row.active { background: var(--bg-hover); border-left: 3px solid var(--green); padding-left: 9px; }
.gw-sess-row .id { font-family: var(--mono); color: var(--cyan); font-size: 11px; }
.gw-sess-row .meta { color: var(--text-muted); font-size: 11px; margin-top: 2px; }
.gw-turn {
  display: grid; grid-template-columns: 60px 1fr auto; gap: 8px;
  padding: 8px 12px; border: 1px solid var(--border); border-radius: 4px;
  margin-bottom: 6px; font-size: 12px; cursor: pointer;
  align-items: center;
}
.gw-turn:hover { background: var(--bg-hover); border-color: var(--text-dim); }
.gw-turn .turn-no { font-family: var(--mono); color: var(--text-muted); font-size: 11px; text-align: center; }
.gw-turn .delta-up { color: var(--red); }
.gw-turn .delta-down { color: var(--green); }

/* Stacked SVG bar chart */
.gw-series-svg { width: 100%; height: 100%; display: block; }
.gw-series-svg rect.gx-cache-read   { fill: var(--green); }
.gw-series-svg rect.gx-cache-create { fill: var(--purple); }
.gw-series-svg rect.gx-input        { fill: var(--cyan); }
.gw-series-svg rect.gx-output       { fill: var(--yellow); }
.gw-series-svg text { fill: var(--text-muted); font-size: 9px; font-family: var(--mono); }

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

/* Gateway detail (request | response) — page scrolls the card; only leaf
   content scrollers where justified (raw JSON pane, pre-context drawer). */
#gw-detail-card .card-body { scroll-behavior: smooth; }
.gw-detail-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
@media (max-width: 1100px) { .gw-detail-grid { grid-template-columns: 1fr; } }
.gw-detail-pane {
  background: var(--bg-card); border: 1px solid var(--border);
  border-radius: 6px; display: flex; flex-direction: column;
  /* Allow shrinking below content intrinsic size so overflow is real. */
  min-width: 0; max-width: 100%;
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
.gw-detail-structured { padding: 8px 12px; font-size: 13px; color: var(--text); min-width: 0; max-width: 100%; }
.gw-detail-structured:empty { display: none; }
.gw-detail-raw {
  font-family: var(--mono); font-size: 12px; line-height: 1.5;
  padding: 8px 12px; white-space: pre;
  /* Inner leaf scroller: raw JSON can be arbitrarily wide on a long string. */
  overflow-x: auto; max-width: 100%;
  max-height: 60vh; overflow-y: auto;
}

/* Collapsible section primitive — used everywhere a block can be folded */
.sec { border: 1px solid var(--border); border-radius: 4px; margin: 6px 0; background: var(--bg); min-width: 0; max-width: 100%; }
.sec > summary {
  list-style: none; cursor: pointer; padding: 6px 10px;
  display: flex; align-items: center; gap: 8px; font-size: 12px;
  color: var(--text-dim); user-select: none; border-radius: 4px;
}
.sec > summary::-webkit-details-marker { display: none; }
.sec > summary::before {
  content: '▶'; font-size: 9px; color: var(--text-muted);
  transition: transform 0.1s; display: inline-block; width: 10px;
}
.sec[open] > summary::before { transform: rotate(90deg); }
.sec > summary:hover { background: var(--bg-hover); color: var(--text); }
.sec > summary .sec-title { font-weight: 600; color: var(--text); }
.sec > summary .sec-len {
  margin-left: auto; font-family: var(--mono); font-size: 11px;
  color: var(--text-muted); padding: 1px 6px; border-radius: 3px;
  background: var(--bg-input);
}
.sec > .sec-body { padding: 8px 10px; border-top: 1px solid var(--border); min-width: 0; max-width: 100%; overflow-x: hidden; }
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
    <div style="display:flex;align-items:center;gap:12px;margin-bottom:12px;flex-wrap:wrap">
      <select id="gw-select" style="background:var(--bg-input);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px"></select>
      <select id="gw-window" style="background:var(--bg-input);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px">
        <option value="24h" selected>last 24h</option>
        <option value="1h">last 1h</option>
        <option value="7d">last 7d</option>
        <option value="30d">last 30d</option>
        <option value="all">all time</option>
      </select>
      <div style="display:inline-flex;border:1px solid var(--border);border-radius:6px;overflow:hidden">
        <div class="gw-sub-btn active" data-sub="overview">Overview</div>
        <div class="gw-sub-btn" data-sub="sessions">Sessions</div>
        <div class="gw-sub-btn" data-sub="requests">Requests</div>
      </div>
    </div>

    <!-- Overview sub-view -->
    <div id="gw-sub-overview" class="gw-subview">
      <div class="stats" id="gw-kpi"></div>
      <div class="card">
        <div class="card-header"><span>Token usage over time</span></div>
        <div class="card-body padded">
          <div id="gw-series-legend" class="token-legend" style="margin-bottom:8px"></div>
          <div id="gw-series" style="height:160px"></div>
        </div>
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
        <div class="card">
          <div class="card-header"><span>Top sessions by tokens</span></div>
          <div class="card-body">
            <table>
              <thead><tr><th>Session</th><th>Model</th><th>Turns</th><th>Tokens (in/out)</th><th>Last seen</th></tr></thead>
              <tbody id="gw-top-sessions"></tbody>
            </table>
          </div>
        </div>
        <div class="card">
          <div class="card-header"><span>Top models by tokens</span></div>
          <div class="card-body">
            <table>
              <thead><tr><th>Model</th><th>Requests</th><th>Tokens (in/out)</th><th>Cache read</th></tr></thead>
              <tbody id="gw-top-models"></tbody>
            </table>
          </div>
        </div>
        <div class="card">
          <div class="card-header"><span>By project</span></div>
          <div class="card-body">
            <table>
              <thead><tr><th>Project</th><th>Requests</th><th>Tokens (in/out)</th></tr></thead>
              <tbody id="gw-by-path"></tbody>
            </table>
          </div>
        </div>
        <div class="card">
          <div class="card-header"><span>By hour of day (UTC)</span></div>
          <div class="card-body padded">
            <div id="gw-by-hour" style="height:140px"></div>
          </div>
        </div>
      </div>
      <div class="card">
        <div class="card-header">
          <span>Biggest single requests</span>
          <span class="help" title="Highest-total-token pairs in the current window. Click any row to open its detail card.">?</span>
        </div>
        <div class="card-body">
          <table>
            <thead><tr><th>When</th><th>Session</th><th>Model</th><th>Path</th><th>Total tokens</th><th>In / Out</th></tr></thead>
            <tbody id="gw-top-requests"></tbody>
          </table>
        </div>
      </div>
      <div class="card">
        <div class="card-header">
          <span>Biggest preamble blocks</span>
          <span class="help" title="Injected pseudo-XML blocks (system-reminder, command-*, task-notification, hooks) aggregated by tag + first 60 chars. Total chars tells you which recurring context is wasting the most tokens across the window.">?</span>
        </div>
        <div class="card-body">
          <table>
            <thead><tr><th>Tag + signature</th><th>Occurrences</th><th>Total chars</th></tr></thead>
            <tbody id="gw-preamble-blocks"></tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- Sessions sub-view -->
    <div id="gw-sub-sessions" class="gw-subview" style="display:none">
      <div class="gw-session-grid">
        <div class="card">
          <div class="card-header">
            <span>Sessions</span>
            <input class="search-input" id="gw-sess-search" placeholder="Filter..." style="width:140px">
          </div>
          <div class="card-body" id="gw-sess-list" style="max-height:70vh;overflow:auto"></div>
        </div>
        <div class="card">
          <div class="card-header"><span id="gw-sess-title">Select a session</span></div>
          <div class="card-body padded" id="gw-sess-timeline"></div>
        </div>
      </div>
    </div>

    <!-- Requests sub-view (existing table) -->
    <div id="gw-sub-requests" class="gw-subview" style="display:none">
    <div class="card">
      <div class="card-header">
        <span>Requests</span>
        <div style="display:flex;gap:8px">
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
    </div><!-- /#gw-sub-requests -->

    <!-- Detail card is shared across sub-views -->
    <div class="card" id="gw-detail-card" style="display:none">
      <div class="card-header">
        <span id="gw-detail-title">Detail</span>
        <div class="filter-btn" onclick="document.getElementById('gw-detail-card').style.display='none'">close</div>
      </div>
      <div class="card-body padded">
        <div class="gw-detail-grid">
          <div class="gw-detail-pane">
            <h4>
              <span>Request <span id="gw-req-len" class="sec-len" style="margin-left:6px"></span></span>
              <span style="display:flex;gap:6px">
                <button class="copy-btn" onclick="bulkSec('req', true)">expand all</button>
                <button class="copy-btn" onclick="bulkSec('req', false)">collapse all</button>
                <button class="copy-btn" onclick="copyDetail('req')">copy</button>
              </span>
            </h4>
            <details class="sec" id="gw-req-json-sec">
              <summary><span class="sec-title">Raw JSON</span><span class="sec-len" id="gw-req-json-len"></span></summary>
              <div class="sec-body"><div class="gw-detail-raw" id="gw-req-raw"></div></div>
            </details>
            <div class="gw-detail-structured" id="gw-req-structured"></div>
          </div>
          <div class="gw-detail-pane">
            <h4>
              <span>Response <span id="gw-resp-len" class="sec-len" style="margin-left:6px"></span></span>
              <span style="display:flex;gap:6px">
                <button class="copy-btn" onclick="bulkSec('resp', true)">expand all</button>
                <button class="copy-btn" onclick="bulkSec('resp', false)">collapse all</button>
                <button class="copy-btn" onclick="copyDetail('resp')">copy</button>
              </span>
            </h4>
            <details class="sec" id="gw-resp-json-sec">
              <summary><span class="sec-title">Raw JSON</span><span class="sec-len" id="gw-resp-json-len"></span></summary>
              <div class="sec-body"><div class="gw-detail-raw" id="gw-resp-raw"></div></div>
            </details>
            <div class="gw-detail-structured" id="gw-resp-structured"></div>
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
let state = {}, logs = [], folders = [], conflicts = [], clipActivities = [], fsActivities = [], metricsText = '', gatewayAudit = [], gwStats = null, gwSubview = 'overview', gwWindow = '24h';
function gwBucket(w) { return w === '1h' ? 'minute' : w === '24h' ? 'hour' : w === '7d' ? 'hour' : 'day'; }
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
    if (needGateway) {
      fetches.push(fetch('/api/gateway/audit?limit=200').then(r=>r.json()));
      if (gwSelected) {
        fetches.push(fetch('/api/gateway/audit/stats?gateway='+encodeURIComponent(gwSelected)+
          '&window='+encodeURIComponent(gwWindow)+'&bucket='+gwBucket(gwWindow)).then(r=>r.json()).catch(()=>null));
      } else {
        fetches.push(Promise.resolve(null));
      }
    }

    const results = await Promise.all(fetches);
    let i = 0;
    state = results[i++]; metricsText = results[i++];
    if (needLogs) logs = results[i++];
    if (needFilesync) { folders = results[i++]; conflicts = results[i++]; fsActivities = results[i++]; }
    if (needClipsync) clipActivities = results[i++];
    if (needGateway) { gatewayAudit = results[i++]; gwStats = results[i++]; }

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
    const meta = document.getElementById('gw-meta');
    if (meta) meta.textContent = '';
    document.getElementById('gw-body').innerHTML =
      '<tr><td colspan="9" style="color:var(--text-muted);padding:20px">No gateways with audit logging configured. Set log.level: full or metadata in the gateway YAML to populate this view.</td></tr>';
    document.getElementById('gw-kpi').innerHTML =
      '<div class="stat" style="grid-column:1/-1;color:var(--text-muted)">No gateway audit data yet. Configure log.level to populate this view.</div>';
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
  renderGatewayOverview();
  if (gwSubview === 'sessions') renderSessions();

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
  const reqJSON = JSON.stringify(p.req);
  const respJSON = JSON.stringify(p.resp);
  document.getElementById('gw-req-raw').innerHTML = highlightJSON(p.req);
  document.getElementById('gw-resp-raw').innerHTML = highlightJSON(p.resp);
  document.getElementById('gw-req-json-len').textContent = fmtLen(reqJSON.length) + ' chars';
  document.getElementById('gw-resp-json-len').textContent = fmtLen(respJSON.length) + ' chars';
  document.getElementById('gw-req-len').textContent = fmtLen(reqJSON.length) + ' chars';
  document.getElementById('gw-resp-len').textContent = fmtLen(respJSON.length) + ' chars';
  // Force-collapse JSON panes when re-opening the card so the structured view is the focus.
  document.getElementById('gw-req-json-sec').open = false;
  document.getElementById('gw-resp-json-sec').open = false;
  document.getElementById('gw-req-structured').innerHTML = renderRequestStructured(p.req);
  document.getElementById('gw-resp-structured').innerHTML = renderResponseStructured(p.resp);
  // Reset scroll on the single scroll surface.
  const body = document.querySelector('#gw-detail-card .card-body');
  if (body) body.scrollTop = 0;
}

// fmtLen renders an integer in a compact form: < 1k raw, < 1M as 1.2k,
// else 1.4M. Used by every length badge so badges read uniformly.
function fmtLen(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n/1000).toFixed(n < 10000 ? 1 : 0) + 'k';
  return (n/1000000).toFixed(1) + 'M';
}

// sec wraps content in a <details class="sec"> with a title + length badge.
// Pass open:true to render expanded by default. Used by every collapsible
// block in the structured panes so they all behave identically.
function sec(title, lenBadge, body, open) {
  const len = lenBadge ? '<span class="sec-len">'+x(lenBadge)+'</span>' : '';
  return '<details class="sec"'+(open ? ' open' : '')+'>' +
    '<summary><span class="sec-title">'+x(title)+'</span>'+len+'</summary>' +
    '<div class="sec-body">'+body+'</div>' +
  '</details>';
}

// bulkSec opens or closes every <details class="sec"> inside a pane. The
// Raw JSON section is excluded so it stays collapsed by default — users
// asking for "expand all" want the conversation, not a 200KB dump.
function bulkSec(side, open) {
  const root = document.getElementById('gw-' + side + '-structured');
  if (!root) return;
  root.querySelectorAll('details.sec').forEach(d => { d.open = open; });
}

// info renders a small (?) tooltip. Plain title attribute is used so it
// works across browsers without a popper library; one-line strings only.
function info(text) {
  return '<span class="help" title="'+x(text)+'">?</span>';
}

// emptyNote is the standard placeholder for sections with nothing to show.
// Keeps the visual rhythm of the pane consistent and gives the user a hint
// about why it might be empty.
function emptyNote(text) {
  return '<div class="empty-note">'+x(text)+'</div>';
}

// Token glossary — shared by the per-pair token bar and the Overview KPI strip.
const tokenHelp = {
  cacheRead:    'Cache read: prompt tokens served from prompt cache. Cheapest. Anthropic returns this directly; OpenAI exposes it under prompt_tokens_details.cached_tokens.',
  cacheWrite:   'Cache write (creation): prompt tokens stored in cache for future reuse. One-time cost; subsequent matching prompts read them as cache_read.',
  freshInput:   'Fresh input: prompt tokens neither read from nor written to cache. The most expensive bucket — minimize by structuring prompts so the cacheable prefix is stable.',
  output:       'Output: tokens generated by the model in this response.',
  reasoning:    'Reasoning: OpenAI o-series internal thinking tokens. Billed but not visible in the output.',
  cacheRatio:   'Cache hit ratio = cache_read / (cache_read + cache_write + fresh_input). Higher is cheaper. Typical Claude Code sessions on a stable system prompt should sit above 0.7.',
  stopReason:   'Anthropic: end_turn (normal) | max_tokens | tool_use | stop_sequence | refusal. OpenAI maps to: stop | length | tool_calls | content_filter.',
  sessionId:    'Derived from sha256(messages[0])[:12]. Same first message → same session, so a Claude Code conversation keeps its id across turns. Override with X-Mesh-Session header.',
  turnIndex:    'Number of messages in the request. Turn 1 is the first user message; turn N is 2N-1 (alternating user/assistant) plus the new user message.',
};

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
  if (summary.stop_reason) chips.push(chip('stop'+info(tokenHelp.stopReason), summary.stop_reason));
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
    html += sec('Stream errors ('+summary.errors.length+')', summary.errors.length+' errors',
      summary.errors.map(e =>
        '<div style="border-left:2px solid var(--red);padding:4px 8px;color:var(--red)">'+x(e)+'</div>'
      ).join(''), true);
  }

  // Build a synthetic assistant message from whichever shape we have, then
  // render it as the assistant bubble that continues the request chat.
  // - SSE summary: reassembled .content + .tool_calls (mapped to tool_use blocks).
  // - Anthropic buffered: body.content already has the right shape.
  // - OpenAI buffered: body.choices[0].message has content + optional tool_calls.
  let assistantMsg = null;
  if (summary.content || (Array.isArray(summary.tool_calls) && summary.tool_calls.length)) {
    const blocks = [];
    if (summary.content) blocks.push({type:'text', text: summary.content});
    for (const tc of (summary.tool_calls||[])) {
      let input = tc.args;
      if (typeof input === 'string') {
        try { input = JSON.parse(input); } catch (_) { /* leave as string */ }
      }
      blocks.push({type:'tool_use', id: tc.id, name: tc.name, input: input});
    }
    assistantMsg = {role:'assistant', content: blocks};
  } else if (resp.body && typeof resp.body === 'object') {
    if (Array.isArray(resp.body.content)) {
      assistantMsg = {role:'assistant', content: resp.body.content};
    } else if (Array.isArray(resp.body.choices) && resp.body.choices[0]) {
      const m = resp.body.choices[0].message || {};
      assistantMsg = {role:'assistant', content: m.content, tool_calls: m.tool_calls};
    }
  }

  if (assistantMsg) {
    const totalChars = msgChars(assistantMsg);
    html += sec('Assistant reply', fmtLen(totalChars)+' chars',
      '<div class="chat">'+renderBubble(assistantMsg, 0)+'</div>', true);
  }

  // Mid-stream errors (Anthropic event:error) — show in red, prominent.
  if (Array.isArray(summary.errors) && summary.errors.length) {
    html += sec('Stream errors ('+summary.errors.length+')', summary.errors.length+' errors',
      summary.errors.map(e =>
        '<div style="border-left:2px solid var(--red);padding:4px 8px;color:var(--red)">'+x(e)+'</div>'
      ).join(''), true);
  }

  if (summary.thinking) {
    html += sec('Thinking', fmtLen(summary.thinking.length)+' chars',
      '<div style="border-left:2px solid var(--purple);padding:4px 8px;color:var(--text-dim);font-style:italic">'+
        renderText(summary.thinking)+'</div>', false);
  }

  // Multi-choice OpenAI responses surface remaining choices verbatim — rare,
  // but keep the data accessible without forcing the user into the JSON pane.
  if (resp.body && Array.isArray(resp.body.choices) && resp.body.choices.length > 1) {
    html += sec('Other choices ('+(resp.body.choices.length-1)+')', (resp.body.choices.length-1)+' alts',
      resp.body.choices.slice(1).map((c, i) => {
        const msg = c.message || {};
        return '<div class="chat">'+renderBubble({role: msg.role||'assistant', content: msg.content, tool_calls: msg.tool_calls}, i+1)+'</div>';
      }).join(''), false);
  }

  if (!assistantMsg && (!summary.errors || !summary.errors.length)) {
    return html + emptyNote('No assistant content captured. Common causes: log.level=metadata (set to full), upstream returned only headers, or response was streamed and disconnected before any deltas.');
  }
  return html;
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
  return '<div class="section-title">tokens (total '+total.toLocaleString()+')'+info('Per-pair token breakdown. Hover any segment for absolute count and percentage.')+'</div>' +
    '<div class="token-legend">' +
      '<span><i class="seg-cache-read" style="background:var(--green)"></i>cache read '+cacheRead.toLocaleString()+info(tokenHelp.cacheRead)+'</span>' +
      '<span><i class="seg-cache-create" style="background:var(--purple)"></i>cache write '+cacheCreate.toLocaleString()+info(tokenHelp.cacheWrite)+'</span>' +
      '<span><i class="seg-input" style="background:var(--cyan)"></i>fresh input '+fresh.toLocaleString()+info(tokenHelp.freshInput)+'</span>' +
      '<span><i class="seg-output" style="background:var(--yellow)"></i>output '+out.toLocaleString()+info(tokenHelp.output)+'</span>' +
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
    return emptyNote('Body not captured. Set log.level: full in the gateway YAML to record full request bodies.');
  }
  let html = '';
  // Header chips: model, stream, session, turn, temperature, max_tokens.
  const chips = [];
  if (body.model) chips.push(chip('model', body.model));
  if (req.stream || body.stream) chips.push(chip('stream', 'true'));
  if (req.session_id) chips.push(chip('session'+info(tokenHelp.sessionId), req.session_id));
  if (req.turn_index) chips.push(chip('turn'+info(tokenHelp.turnIndex), req.turn_index));
  if (typeof body.temperature === 'number') chips.push(chip('temp', body.temperature));
  if (body.max_tokens) chips.push(chip('max_tokens', body.max_tokens));
  if (body.top_p) chips.push(chip('top_p', body.top_p));
  if (chips.length) html += '<div style="margin-bottom:8px">' + chips.join('') + '</div>';

  // System prompt (Anthropic top-level string or array of content blocks;
  // OpenAI inlines the system message in the messages array, so it appears
  // there instead).
  if (body.system) {
    const txt = typeof body.system === 'string' ? body.system : JSON.stringify(body.system);
    html += sec('System prompt', fmtLen(txt.length)+' chars',
      renderSystemPrompt(body.system), true);
  }

  // Messages → chat-style bubbles.
  const msgs = Array.isArray(body.messages) ? body.messages : [];
  if (msgs.length) {
    const totalChars = msgs.reduce((n, m) => n + msgChars(m), 0);
    html += sec('Conversation ('+msgs.length+')', msgs.length+' msgs · '+fmtLen(totalChars)+' chars',
      '<div class="chat">' + msgs.map((m, i) => renderBubble(m, i)).join('') + '</div>', true);
  }

  // Tools available to the model. Each tool carries its own length so it
  // is obvious which one is bloating the request; the section summary
  // shows the aggregate.
  const tools = Array.isArray(body.tools) ? body.tools : [];
  if (tools.length) {
    const totalToolChars = tools.reduce((n, t) => n + JSON.stringify(t).length, 0);
    html += sec('Tools ('+tools.length+')', tools.length+' tools · '+fmtLen(totalToolChars)+' chars',
      tools.map(renderToolDefinition).join(''), false);
  }
  return html || emptyNote('Empty body — request had no system, messages, or tools.');
}

// renderSystemPrompt turns a system prompt string into a minimal outline:
// a leading metadata row for key:value header lines, then one <details> per
// Markdown heading (# / ## / ###). Each heading section shows its char
// count. No Markdown styling is applied — the user prefers plain text, so
// we only introduce *structure*, never typography.
//
// Array form (Anthropic content-block style) falls back to recursing on the
// concatenated text, since the structure we care about is the prose, not
// the block wrapping.
function renderSystemPrompt(v) {
  if (Array.isArray(v)) {
    const joined = v.map(b => typeof b === 'string' ? b : (b && b.text) || '').join('\n');
    return renderSystemPrompt(joined);
  }
  if (typeof v !== 'string') {
    return '<div class="msg-block"><div class="msg-body">' + renderContent(v) + '</div></div>';
  }
  const s = v;
  const lines = s.split('\n');

  // Step 1: peel off leading key:value metadata lines (e.g. billing headers).
  // A metadata line matches /^[a-z][a-z0-9-]*:\s+\S/ and continues until the
  // first non-matching line.
  const metaRe = /^[a-z][a-z0-9-]*:\s+\S/i;
  const meta = [];
  let i = 0;
  while (i < lines.length && (lines[i] === '' || metaRe.test(lines[i]))) {
    if (lines[i] !== '') meta.push(lines[i]);
    i++;
  }

  // Step 2: collect sections split by # / ## / ### headings. Text before
  // the first heading becomes a synthetic "Preamble" section.
  const body = lines.slice(i);
  const headingRe = /^(#{1,6})\s+(.+)$/;
  const sections = [];
  let current = {depth: 0, title: 'Preamble', lines: []};
  for (const ln of body) {
    const h = ln.match(headingRe);
    if (h) {
      if (current.lines.length > 0 || current.title !== 'Preamble') sections.push(current);
      current = {depth: h[1].length, title: h[2].trim(), lines: []};
    } else {
      current.lines.push(ln);
    }
  }
  if (current.lines.length > 0 || current.title !== 'Preamble') sections.push(current);

  let html = '';
  if (meta.length) {
    html += '<div style="margin-bottom:8px;display:flex;flex-wrap:wrap;gap:4px">' +
      meta.map(line => {
        const colon = line.indexOf(':');
        const k = line.slice(0, colon);
        const v = line.slice(colon+1).trim();
        return chip(x(k), v);
      }).join('') +
    '</div>';
  }

  // If there are no headings and only one synthetic section, just render
  // the text straight — no pointless outer details wrapper.
  if (sections.length === 0) return html + emptyNote('(no body)');
  if (sections.length === 1 && sections[0].title === 'Preamble') {
    return html + '<div class="msg-block"><div class="msg-body">' +
      renderPlainText(sections[0].lines.join('\n').trim()) +
      '</div></div>';
  }

  for (const sec0 of sections) {
    const body = sec0.lines.join('\n').trim();
    const title = (sec0.depth ? '#'.repeat(sec0.depth) + ' ' : '') + sec0.title;
    const open = sec0.title === 'Preamble';
    html += sec(title, fmtLen(body.length)+' chars',
      '<div class="msg-block"><div class="msg-body">'+renderPlainText(body)+'</div></div>',
      open);
  }
  return html;
}

// msgChars returns the visible character count of a message — used for the
// per-section length badge. Counts only string content; tool blocks contribute
// their JSON length so a fat tool result still registers.
function msgChars(m) {
  if (!m) return 0;
  const c = m.content;
  if (typeof c === 'string') return c.length;
  if (Array.isArray(c)) return c.reduce((n, b) => {
    if (!b) return n;
    if (typeof b === 'string') return n + b.length;
    if (b.type === 'text') return n + (b.text||'').length;
    if (b.type === 'thinking') return n + (b.thinking||'').length;
    return n + JSON.stringify(b).length;
  }, 0);
  return c == null ? 0 : JSON.stringify(c).length;
}

// chip renders a labeled value pill. label may contain trusted HTML so an
// info(?) icon can sit beside the label; value is always escaped.
function chip(label, value, cls) {
  const c = cls ? ' '+cls : '';
  return '<span class="chip'+c+'">'+label+' <b>'+x(String(value))+'</b></span>';
}

// renderBubble paints a single chat message as a role-colored bubble with a
// footer carrying the index, role pill, length, and any contextual chips.
// User bubbles right-align; assistant + tool left-align; system spans the
// full row.
//
// For user messages, the content is split into: [pre-context blocks] +
// [typed text] + [post-context blocks]. Only the typed text shows in the
// bubble proper; the context drawers collapse underneath with per-block
// size. This keeps a 51 KB user message that says "Hi" actually legible.
function renderBubble(m, idx) {
  const role = String(m.role || 'unknown').toLowerCase();
  const cls = (role === 'user' || role === 'assistant' || role === 'system' || role === 'tool') ? role : 'unknown';
  const align = role === 'user' ? 'right' : role === 'system' ? 'center' : 'left';
  const totalLen = msgChars(m);

  let contentHtml, preCtxHtml = '', postCtxHtml = '', typedChars = totalLen;
  if (role === 'user' && typeof m.content === 'string') {
    const split = splitUserText(m.content);
    contentHtml = renderPlainText(split.typed);
    typedChars = split.typed.length;
    if (split.pre.length) preCtxHtml = renderContextDrawer('pre', split.pre, totalLen);
    if (split.post.length) postCtxHtml = renderContextDrawer('post', split.post, totalLen);
  } else if (role === 'user' && Array.isArray(m.content)) {
    // Anthropic-style content blocks: any leaf that is a plain-text block
    // gets the same preamble split. Non-text blocks (tool_result, image,
    // tool_use) pass through as-is.
    const parts = m.content.map(b => {
      if (b && b.type === 'text' && typeof b.text === 'string') {
        const s = splitUserText(b.text);
        const typedHtml = renderPlainText(s.typed);
        const pre = s.pre.length ? renderContextDrawer('pre', s.pre, b.text.length) : '';
        const post = s.post.length ? renderContextDrawer('post', s.post, b.text.length) : '';
        typedChars = s.typed.length;
        return pre + typedHtml + post;
      }
      return renderContentBlock(b);
    });
    contentHtml = parts.join('');
  } else {
    contentHtml = renderContent(m.content);
  }
  const calls = Array.isArray(m.tool_calls) ? m.tool_calls.map(renderOpenAIToolCall).join('') : '';

  const chips = [];
  if (m.tool_call_id) chips.push('<span class="role-pill" data-link-tool="'+x(m.tool_call_id)+'" onclick="flashToolUse(\''+x(m.tool_call_id)+'\')">tool_call_id '+x(m.tool_call_id)+'</span>');
  const dataIds = collectToolUseIds(m).map(id => 'data-tool-use="'+x(id)+'"').join(' ');

  const lenLabel = (role === 'user' && typedChars !== totalLen)
    ? fmtLen(typedChars)+' typed · '+fmtLen(totalLen)+' total'
    : fmtLen(totalLen)+' chars';

  return '<div class="chat-row '+align+'">' +
    '<div class="bubble role-'+cls+'" '+dataIds+'>' +
      '<div class="b-content">'+contentHtml+calls+'</div>' +
      preCtxHtml + postCtxHtml +
      '<div class="b-foot">' +
        '<span class="role-pill">'+x(role)+'</span>' +
        '<span>#'+(idx+1)+'</span>' +
        chips.join('') +
        '<span class="b-len">'+x(lenLabel)+'</span>' +
      '</div>' +
    '</div>' +
  '</div>';
}

// splitUserText separates a user message into [pre-context | typed | post-context].
// Claude Code (and similar agent harnesses) wrap reminders, hook outputs, and
// IDE hints in pseudo-XML blocks around the user's actual text. The heuristic:
//
//   - Tokenize the string into an alternating sequence of tag-blocks and
//     text-runs using the same splitter as the custom-block renderer.
//   - pre = leading contiguous tag-blocks (before the first non-blank text).
//   - post = trailing contiguous tag-blocks (after the last non-blank text).
//   - typed = everything in between, concatenated.
//
// False positive: if a user deliberately types <system-reminder>...</system-reminder>
// the splitter will treat it as injected context. User can still read it via
// the drawer. Per user's 1a choice.
function splitUserText(s) {
  const parts = splitCustomBlocks(s);
  let firstText = -1, lastText = -1;
  for (let i = 0; i < parts.length; i++) {
    if (parts[i].kind === 'text' && parts[i].text.trim() !== '') {
      if (firstText === -1) firstText = i;
      lastText = i;
    }
  }
  if (firstText === -1) {
    // Nothing but blocks (or whitespace) — treat all blocks as pre-context.
    return {pre: parts.filter(p => p.kind === 'block'), typed: s.trim(), post: []};
  }
  const pre = parts.slice(0, firstText).filter(p => p.kind === 'block');
  const post = parts.slice(lastText + 1).filter(p => p.kind === 'block');
  const typedParts = parts.slice(firstText, lastText + 1);
  // Within the typed span, concatenate text runs and *inline* any blocks
  // that sit between words of the user's actual prose — those are usually
  // legitimate (e.g. a pasted code example) so we do not hide them.
  const typed = typedParts.map(p => p.kind === 'text' ? p.text : '').join('').trim();
  return {pre, typed, post};
}

// ctxBodyPreview extracts the first meaningful line from a block body,
// collapsed to a single space-run and capped at maxLen chars. Used as the
// inline hint in collapsed ctx-item rows so the user can tell "Skills
// available" from "SessionStart hook" without expanding.
function ctxBodyPreview(body, maxLen) {
  // Take first non-blank line, collapse whitespace.
  const firstLine = body.split('\n').find(l => l.trim() !== '') || '';
  const collapsed = firstLine.replace(/\s+/g, ' ').trim();
  return collapsed.length > maxLen ? collapsed.slice(0, maxLen) + '…' : collapsed;
}

// renderContextDrawer emits the pre/post drawer listing injected blocks with
// per-block chars and percentage of message. Each block is a nested <details>
// so the user can expand the ones that matter.
function renderContextDrawer(kind, blocks, messageLen) {
  const total = blocks.reduce((n, b) => n + b.body.length, 0);
  const label = kind === 'pre' ? 'Pre-context' : 'Post-context';
  const rows = blocks.map(b => {
    // Determine kind class from block name (same logic as renderCustomBlock)
    let blkKind = 'unknown';
    const n = b.name;
    if (n === 'system-reminder') blkKind = 'system-reminder';
    else if (/^command-(name|message|args|stdout|stderr)$|^local-command-(stdout|stderr)$/.test(n))
      blkKind = n.endsWith('stdout') ? 'stdout' : n.endsWith('stderr') ? 'stderr' : 'command';
    else if (n === 'task-notification') blkKind = 'task-notification';
    else if (n === 'user-prompt-submit-hook' || n === 'stop-hook-feedback') blkKind = 'hook';

    const pct = messageLen > 0 ? (100 * b.body.length / messageLen).toFixed(1) + '%' : '';
    const preview = ctxBodyPreview(b.body, 48);
    const inner = (blkKind === 'stdout' || blkKind === 'stderr')
      ? '<pre>'+x(b.body)+'</pre>'
      : '<div>'+renderPlainText(b.body)+'</div>';
    return '<details class="ctx-item k-'+blkKind+'">' +
      '<summary>' +
        '<span class="ctx-name">&lt;'+x(b.name)+'&gt;' +
          (preview ? ' <span class="ctx-preview">'+x(preview)+'</span>' : '') +
        '</span>' +
        '<span class="ctx-size">'+fmtLen(b.body.length)+' chars</span>' +
        '<span class="ctx-pct">'+pct+'</span>' +
      '</summary>' +
      '<div class="ctx-body">'+inner+'</div>' +
    '</details>';
  }).join('');
  return '<details class="'+(kind === 'pre' ? 'pre-ctx' : 'post-ctx')+'">' +
    '<summary>' +
      '<span class="ctx-label">'+x(label)+'</span>' +
      '<span class="ctx-summary-meta">'+fmtLen(total)+' chars in '+blocks.length+' blocks</span>' +
    '</summary>' +
    '<div class="ctx-list">'+rows+'</div>' +
  '</details>';
}

function collectToolUseIds(m) {
  const out = [];
  const c = m.content;
  if (Array.isArray(c)) {
    for (const b of c) {
      if (b && b.type === 'tool_use' && b.id) out.push(b.id);
    }
  }
  if (Array.isArray(m.tool_calls)) {
    for (const tc of m.tool_calls) if (tc && tc.id) out.push(tc.id);
  }
  return out;
}

function flashToolUse(id) {
  // Find the bubble that originated this tool_use_id (request-side assistant
  // bubble for prior turns, or the response-side current assistant bubble)
  // and flash it. Best-effort within the open detail card.
  const sel = '.bubble[data-tool-use="'+CSS.escape(id)+'"]';
  const target = document.querySelector('#gw-req-structured '+sel) ||
                 document.querySelector('#gw-resp-structured '+sel);
  if (!target) return;
  target.classList.remove('flash');
  void target.offsetWidth;
  target.classList.add('flash');
  target.scrollIntoView({behavior:'smooth', block:'center'});
}

function renderMessage(m, idx) {
  const role = String(m.role || 'unknown');
  const content = m.content;
  const inner = renderContent(content);
  const toolID = m.tool_call_id ? '<span class="chip">tool_call_id <b>'+x(m.tool_call_id)+'</b></span>' : '';
  const calls = Array.isArray(m.tool_calls) ? m.tool_calls.map(renderOpenAIToolCall).join('') : '';
  const len = msgChars(m);
  return '<div class="msg-block">' +
    '<div class="msg-head">' +
      '<span class="msg-role '+x(role)+'">'+x(role)+'</span>' +
      '<span style="color:var(--text-muted)">#'+(idx+1)+'</span>' +
      toolID +
      '<span class="sec-len" style="margin-left:auto">'+fmtLen(len)+' chars</span>' +
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

// renderText is the leaf renderer for any string content the model sees or
// produces. It first carves out custom embedded blocks (<system-reminder>,
// <command-name>, <local-command-stdout>, etc.) and renders each as its own
// labeled box. The remaining plain-text spans are escaped, truncated past
// truncateLen, and shown verbatim. Markdown rendering is intentionally NOT
// applied here per user preference (Markdown=plain) — strings stay literal
// so debugging prompt issues never has to fight a renderer.
function renderText(s) {
  s = String(s);
  const parts = splitCustomBlocks(s);
  if (parts.length === 1 && parts[0].kind === 'text') {
    return renderPlainText(parts[0].text);
  }
  return parts.map(p => p.kind === 'text' ? renderPlainText(p.text) : renderCustomBlock(p)).join('');
}

function renderPlainText(s) {
  if (!s) return '';
  if (s.length <= truncateLen) {
    return '<div class="text">'+x(s)+'</div>';
  }
  const id = 'tx-'+(_hjId++);
  return '<div class="text" id="'+id+'-short">'+x(s.slice(0, truncateLen))+'…' +
    ' <span class="truncate" onclick="_txExpand(\''+id+'\')">expand ('+fmtLen(s.length)+' chars)</span></div>' +
    '<div class="text json-collapsed" id="'+id+'-full">'+x(s)+
    ' <span class="truncate" onclick="_txCollapse(\''+id+'\')">collapse</span></div>';
}

// splitCustomBlocks scans s for pseudo-XML wrappers and returns an ordered
// list of {kind:'text'|'block', name?, body?, text?}. Matches any
// <name>...</name> pair where name is 3–41 lowercase alphanum+hyphen chars.
//
// A backreference-based combined (A|B) regex is NOT used here because
// wrapping two /\1/-using patterns in a single outer group shifts \1 to
// reference the outer group instead of the inner tag-name capture, silently
// breaking all tag detection. Instead: find open tags with a plain regex,
// then locate the matching close tag with a case-insensitive indexOf — the
// same approach as the Go-side scanPreambleTags.
const customTagOpenRe = /<([a-z][a-z0-9-]{2,40})\b[^>]*>/gi;
function splitCustomBlocks(s) {
  const out = [];
  let i = 0;
  customTagOpenRe.lastIndex = 0;
  let m;
  while ((m = customTagOpenRe.exec(s)) !== null) {
    const openStart = m.index;
    const openEnd   = openStart + m[0].length;
    const name      = m[1].toLowerCase();
    const closeTag  = '</' + name + '>';
    // Case-insensitive close-tag search from just after the open tag.
    const tail      = s.slice(openEnd);
    const closeIdx  = tail.toLowerCase().indexOf(closeTag);
    if (closeIdx < 0) continue; // no matching close tag — treat as plain text
    // Text before this block.
    if (openStart > i) out.push({kind: 'text', text: s.slice(i, openStart)});
    out.push({kind: 'block', name, body: tail.slice(0, closeIdx)});
    i = openEnd + closeIdx + closeTag.length;
    customTagOpenRe.lastIndex = i; // skip past the matched block
  }
  if (i < s.length) out.push({kind: 'text', text: s.slice(i)});
  return out;
}

// renderCustomBlock paints a single detected pseudo-XML block. Each block
// gets its own color, an icon hint, a length badge, and a default-open
// toggle. system-reminder is yellow because the user usually opens these to
// audit what Claude Code injected; command/* are cyan terminal-like; stdout
// preserves whitespace; stderr is red.
function renderCustomBlock(p) {
  const name = p.name;
  const body = p.body;
  let kind = 'unknown', icon = '◆';
  if (name === 'system-reminder') { kind = 'system-reminder'; icon = '⚠'; }
  else if (/^command-(name|message|args|stdout|stderr)$|^local-command-(stdout|stderr)$/.test(name)) {
    kind = name.endsWith('stdout') ? 'stdout' : name.endsWith('stderr') ? 'stderr' : 'command';
    icon = name.endsWith('err') ? '✖' : name.endsWith('out') ? '▮' : '$';
  }
  else if (name === 'task-notification') { kind = 'task-notification'; icon = '🔔'.length===1 ? '🔔' : '*'; }
  else if (name === 'user-prompt-submit-hook' || name === 'stop-hook-feedback') { kind = 'hook'; icon = '◈'; }
  const id = 'cb-'+(_hjId++);
  const isLong = body.length > truncateLen;
  const collapseInitial = name === 'system-reminder' && isLong;
  const head = '<div class="cblock-head" onclick="document.getElementById(\''+id+'\').classList.toggle(\'json-collapsed\')">' +
    '<span class="icon">'+x(icon)+'</span>' +
    '<span class="name">&lt;'+x(name)+'&gt;</span>' +
    '<span class="sec-len">'+fmtLen(body.length)+' chars</span>' +
  '</div>';
  // Body is rendered as plain text with the same truncate behavior as text
  // content. stdout/stderr keep raw whitespace via a <pre> for readability.
  const inner = (kind === 'stdout' || kind === 'stderr')
    ? '<pre>'+x(body)+'</pre>'
    : renderPlainText(body);
  return '<div id="'+id+'" class="cblock k-'+kind+(collapseInitial?' json-collapsed':'')+'">' +
    head + '<div class="cblock-body">'+inner+'</div>' +
  '</div>';
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

// renderToolDefinition shows the tool's name, short description, and its
// input schema with full JSON syntax highlighting so the properties and
// types are scannable instead of a wall of monochrome text.
function renderToolDefinition(t) {
  const name = t.name || (t.function && t.function.name) || 'unknown';
  const desc = t.description || (t.function && t.function.description) || '';
  const schema = t.input_schema || (t.function && t.function.parameters) || {};
  const totalChars = JSON.stringify(t).length;
  const schemaChars = JSON.stringify(schema).length;
  const propCount = schema && schema.properties ? Object.keys(schema.properties).length : 0;
  const required = Array.isArray(schema && schema.required) ? schema.required.length : 0;
  return '<details class="tool-block">' +
    '<summary>' +
      '<span class="tool-name">'+x(name)+'</span>' +
      (desc ? ' <span style="color:var(--text-dim);font-style:italic">'+x(desc.slice(0, 120))+'</span>' : '') +
      '<span class="sec-len" style="float:right">' +
        (propCount ? propCount+' params' + (required ? ' ('+required+' req)' : '') + ' · ' : '') +
        fmtLen(totalChars)+' chars' +
      '</span>' +
    '</summary>' +
    (desc && desc.length > 120
      ? '<div style="color:var(--text-dim);font-style:italic;margin:4px 0;white-space:pre-wrap">'+x(desc)+'</div>'
      : '') +
    '<div style="margin-top:6px">' +
      '<div class="section-title" style="margin:0 0 4px">input_schema · '+fmtLen(schemaChars)+' chars</div>' +
      '<div class="gw-detail-raw" style="max-height:300px">'+highlightJSON(schema)+'</div>' +
    '</div>' +
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
document.getElementById('gw-select').addEventListener('change', e => { gwSelected = e.target.value; gwStats = null; refresh(); });
document.getElementById('gw-window').addEventListener('change', e => { gwWindow = e.target.value; gwStats = null; refresh(); });
document.querySelectorAll('.gw-sub-btn').forEach(b => b.addEventListener('click', () => {
  document.querySelectorAll('.gw-sub-btn').forEach(x => x.classList.toggle('active', x === b));
  gwSubview = b.dataset.sub;
  document.getElementById('gw-sub-overview').style.display = gwSubview === 'overview' ? '' : 'none';
  document.getElementById('gw-sub-sessions').style.display = gwSubview === 'sessions' ? '' : 'none';
  document.getElementById('gw-sub-requests').style.display = gwSubview === 'requests' ? '' : 'none';
  if (gwSubview === 'sessions') renderSessions();
}));

// --- Gateway overview ---
//
// Reads gwStats (the response from /api/gateway/audit/stats) and paints the
// KPI strip, the stacked time-series, and the top-N tables. Renders a clear
// empty state when stats have not arrived yet so the user sees "loading"
// rather than a blank pane.
function renderGatewayOverview() {
  const kpi = document.getElementById('gw-kpi');
  if (!kpi) return;
  if (!gwStats) {
    kpi.innerHTML = '<div class="stat" style="grid-column:1/-1;color:var(--text-muted)">Loading stats…</div>';
    document.getElementById('gw-series').innerHTML = '';
    document.getElementById('gw-top-sessions').innerHTML = '';
    document.getElementById('gw-top-models').innerHTML = '';
    return;
  }
  const t = gwStats.totals || {};
  const totalIn = (t.input_tokens||0) + (t.cache_read_tokens||0) + (t.cache_creation_tokens||0);
  const errPct = t.requests > 0 ? (100*t.errors/t.requests).toFixed(1)+'%' : '0%';
  const avgMs = t.requests > 0 ? Math.round(t.elapsed_sum_ms/t.requests)+'ms' : '-';
  const cacheRatio = (100*(t.cache_hit_ratio||0)).toFixed(1)+'%';
  kpi.innerHTML =
    statBox('Requests', (t.requests||0).toLocaleString(), gwStats.window) +
    statBox('Errors', (t.errors||0).toLocaleString()+' ('+errPct+')', '', t.errors > 0 ? 'var(--red)' : '') +
    statBox('Input tokens'+info('Sum of fresh + cache_read + cache_write input tokens.'), totalIn.toLocaleString(), '(incl. cache)') +
    statBox('Output tokens'+info(tokenHelp.output), (t.output_tokens||0).toLocaleString(), '') +
    statBox('Cache hit ratio'+info(tokenHelp.cacheRatio), cacheRatio, 'reads / total input', t.cache_hit_ratio >= 0.5 ? 'var(--green)' : t.cache_hit_ratio >= 0.2 ? 'var(--yellow)' : 'var(--red)') +
    statBox('Avg latency', avgMs, 'per request');

  document.getElementById('gw-series').innerHTML = renderSeriesSVG(gwStats.series || []);
  document.getElementById('gw-series-legend').innerHTML =
    '<span><i style="background:var(--green)"></i>cache read</span>' +
    '<span><i style="background:var(--purple)"></i>cache write</span>' +
    '<span><i style="background:var(--cyan)"></i>fresh input</span>' +
    '<span><i style="background:var(--yellow)"></i>output</span>';

  // Top sessions and top models (by_session and by_model are sorted by token total server-side).
  const sessions = (gwStats.by_session || []).slice(0, 10);
  document.getElementById('gw-top-sessions').innerHTML = sessions.length === 0
    ? '<tr><td colspan="5" style="color:var(--text-muted);padding:12px">No sessions in window.</td></tr>'
    : sessions.map(s => '<tr style="cursor:pointer" onclick="jumpToSession(\''+x(s.key)+'\')">' +
        '<td><code style="color:var(--cyan)">'+x(s.key)+'</code></td>' +
        '<td style="color:var(--text-dim)">'+x(s.first_model||'-')+'</td>' +
        '<td>'+(s.turns||s.requests)+'</td>' +
        '<td>'+(s.input_tokens||0).toLocaleString()+' / '+(s.output_tokens||0).toLocaleString()+'</td>' +
        '<td style="color:var(--text-muted)">'+x(fmtAgo(s.last_seen||''))+'</td>' +
      '</tr>').join('');

  const models = (gwStats.by_model || []).slice(0, 10);
  document.getElementById('gw-top-models').innerHTML = models.length === 0
    ? '<tr><td colspan="4" style="color:var(--text-muted);padding:12px">No models in window.</td></tr>'
    : models.map(m => '<tr>' +
        '<td style="color:var(--text-dim)">'+x(m.key||'-')+'</td>' +
        '<td>'+m.requests+'</td>' +
        '<td>'+(m.input_tokens||0).toLocaleString()+' / '+(m.output_tokens||0).toLocaleString()+'</td>' +
        '<td>'+(m.cache_read_tokens||0).toLocaleString()+'</td>' +
      '</tr>').join('');

  // By-project: local project path (last two segments) extracted from the
  // system prompt. Falls back to URL path for non-Claude-Code clients.
  const paths = (gwStats.by_path || []).slice(0, 10);
  document.getElementById('gw-by-path').innerHTML = paths.length === 0
    ? '<tr><td colspan="3" style="color:var(--text-muted);padding:12px">No projects in window.</td></tr>'
    : paths.map(p => '<tr>' +
        '<td><code style="color:var(--cyan)">'+x(p.key||'-')+'</code></td>' +
        '<td>'+p.requests+'</td>' +
        '<td>'+(p.input_tokens||0).toLocaleString()+' / '+(p.output_tokens||0).toLocaleString()+'</td>' +
      '</tr>').join('');

  // By-hour-of-day: 24-bucket bar chart (only populated hours). Tells the
  // user whether usage clusters at specific times (e.g. morning pair-programming).
  document.getElementById('gw-by-hour').innerHTML = renderHourChart(gwStats.by_hour || []);

  // Biggest single requests: jump-to-pair from the observability view.
  const topReqs = gwStats.top_requests || [];
  document.getElementById('gw-top-requests').innerHTML = topReqs.length === 0
    ? '<tr><td colspan="6" style="color:var(--text-muted);padding:12px">No requests in window.</td></tr>'
    : topReqs.map(r => '<tr style="cursor:pointer" onclick="jumpToPair(\''+x(r.run)+'\','+r.id+')">' +
        '<td style="color:var(--text-muted);white-space:nowrap">'+x(fmtAgo(r.ts))+'</td>' +
        '<td><code style="color:var(--cyan)">'+x(r.session||'-')+'</code></td>' +
        '<td style="color:var(--text-dim)">'+x(r.model||'-')+'</td>' +
        '<td style="color:var(--text-muted)">'+x(r.path||'-')+'</td>' +
        '<td style="font-weight:600">'+(r.total_tokens||0).toLocaleString()+'</td>' +
        '<td>'+(r.input_tokens||0).toLocaleString()+' / '+(r.output_tokens||0).toLocaleString()+'</td>' +
      '</tr>').join('');

  // Biggest preamble blocks: the "what injected block is costing me the
  // most" table. key is already "<tag> first-60-chars".
  const pre = (gwStats.preamble_blocks || []).slice(0, 15);
  document.getElementById('gw-preamble-blocks').innerHTML = pre.length === 0
    ? '<tr><td colspan="3" style="color:var(--text-muted);padding:12px">No preamble blocks detected in user messages.</td></tr>'
    : pre.map(p => '<tr>' +
        '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim);max-width:600px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="'+x(p.key)+'">'+x(p.key)+'</td>' +
        '<td>'+p.requests+'</td>' +
        '<td>'+(p.input_tokens||0).toLocaleString()+'</td>' +
      '</tr>').join('');
}

// renderHourChart paints a 24-column bar chart. Each bar's height is
// proportional to that hour's request count. Hours with no data render as
// an empty bar so the 0..23 scale is always visible.
function renderHourChart(rows) {
  const byHour = {};
  for (const r of rows) byHour[r.hour] = r;
  const max = Math.max(1, ...rows.map(r => r.requests));
  const w = 800, h = 120, pad = 16;
  const barW = (w - 2*pad) / 24 - 2;
  let out = '';
  for (let i = 0; i < 24; i++) {
    const r = byHour[i];
    const req = r ? r.requests : 0;
    const ht = req * (h - 2*pad) / max;
    const x0 = pad + i * (barW + 2);
    const y = h - pad - ht;
    const title = i + ':00 — ' + req + ' req';
    out += '<g><title>'+x(title)+'</title>' +
      (ht > 0 ? '<rect x="'+x0+'" y="'+y+'" width="'+barW+'" height="'+ht+'" fill="var(--cyan)"></rect>' : '') +
      (i % 6 === 0 ? '<text x="'+x0+'" y="'+(h-4)+'">'+i+'h</text>' : '') +
    '</g>';
  }
  return '<svg class="gw-series-svg" viewBox="0 0 '+w+' '+h+'" preserveAspectRatio="none">'+out+'</svg>';
}

// jumpToPair switches to the Requests sub-view, fetches the pair, and
// opens the detail card. Handy from the top-requests table and anywhere
// else the user wants to drill into a specific id+run.
function jumpToPair(run, id) {
  const sub = document.querySelector('.gw-sub-btn[data-sub="requests"]');
  if (sub) sub.click();
  fetch('/api/gateway/audit/pair?gateway='+encodeURIComponent(gwSelected)+
        '&run='+encodeURIComponent(run)+'&id='+encodeURIComponent(id))
    .then(r => r.ok ? r.json() : null)
    .then(pair => {
      if (!pair) return;
      // Inject synthesized gwRowsCache so showGwDetail can run.
      gwRowsCache = [{req: pair.request, resp: pair.response}];
      showGwDetail(0);
    }).catch(()=>{});
}

// statBox renders one KPI cell. label may contain trusted HTML (info icons);
// value and sub are escaped because they often hold dynamic data.
function statBox(label, value, sub, color) {
  return '<div class="stat">' +
    '<div class="stat-label">'+label+'</div>' +
    '<div class="stat-value"'+(color ? ' style="color:'+color+'"' : '')+'>'+x(value)+'</div>' +
    (sub ? '<div class="stat-sub">'+x(sub)+'</div>' : '') +
  '</div>';
}

function fmtAgo(ts) {
  if (!ts) return '-';
  return timeAgo(ts);
}

function jumpToSession(sid) {
  gwSubview = 'sessions';
  document.querySelectorAll('.gw-sub-btn').forEach(x => x.classList.toggle('active', x.dataset.sub === 'sessions'));
  document.getElementById('gw-sub-overview').style.display = 'none';
  document.getElementById('gw-sub-requests').style.display = 'none';
  document.getElementById('gw-sub-sessions').style.display = '';
  selectSession(sid);
  renderSessions();
}

// --- Sessions sub-view ---
//
// Reads gwStats.by_session for the left list and fetches the per-session
// pair stream from /api/gateway/audit?session= for the right timeline. Each
// turn shows the message count (from turn_index when available, otherwise
// derived), tokens, the delta vs the prior turn (so prompt growth is
// visible), and the stop_reason. Click any turn → opens the detail card.
let gwSessSelected = '';
let gwSessRows = [];      // raw audit rows for the selected session
let gwSessSearch = '';

function renderSessions() {
  const list = document.getElementById('gw-sess-list');
  if (!list) return;
  const sessions = (gwStats && gwStats.by_session) || [];
  const term = (gwSessSearch||'').toLowerCase();
  const filtered = term
    ? sessions.filter(s => (s.key+' '+s.first_model).toLowerCase().includes(term))
    : sessions;
  if (!filtered.length) {
    list.innerHTML = '<div style="color:var(--text-muted);padding:12px">No sessions in window.</div>';
    document.getElementById('gw-sess-timeline').innerHTML = '';
    document.getElementById('gw-sess-title').textContent = 'Select a session';
    return;
  }
  list.innerHTML = filtered.map(s => {
    const active = s.key === gwSessSelected ? ' active' : '';
    return '<div class="gw-sess-row'+active+'" onclick="selectSession(\''+x(s.key)+'\')">' +
      '<div class="id">'+x(s.key)+'</div>' +
      '<div class="meta">'+x(s.first_model||'?')+' · '+(s.turns||s.requests)+' turns · ' +
        ((s.input_tokens||0)+(s.output_tokens||0)).toLocaleString()+' tokens</div>' +
      '<div class="meta">last '+x(timeAgo(s.last_seen||''))+'</div>' +
    '</div>';
  }).join('');
  if (!gwSessSelected && filtered.length) {
    selectSession(filtered[0].key);
  } else if (gwSessSelected) {
    renderSessionTimeline();
  }
}

function selectSession(sid) {
  gwSessSelected = sid;
  document.querySelectorAll('.gw-sess-row').forEach(el => el.classList.remove('active'));
  // Fetch the per-session rows; the response is paired so the timeline can
  // be reconstructed.
  fetch('/api/gateway/audit?gateway='+encodeURIComponent(gwSelected)+
        '&session='+encodeURIComponent(sid)+'&limit=200')
    .then(r => r.json())
    .then(arr => {
      gwSessRows = (arr && arr[0] && arr[0].rows) || [];
      renderSessionTimeline();
      // Re-mark active row in case the list re-rendered.
      document.querySelectorAll('.gw-sess-row').forEach(el => {
        if (el.querySelector('.id') && el.querySelector('.id').textContent === sid) {
          el.classList.add('active');
        }
      });
    })
    .catch(() => {
      gwSessRows = [];
      renderSessionTimeline();
    });
}

function renderSessionTimeline() {
  const wrap = document.getElementById('gw-sess-timeline');
  const title = document.getElementById('gw-sess-title');
  if (!wrap || !title) return;
  if (!gwSessSelected) {
    title.textContent = 'Select a session';
    wrap.innerHTML = '';
    return;
  }
  // Pair req+resp by id+run, oldest first so turn order matches chat order.
  const reqs = new Map();
  const pairs = [];
  for (const r of gwSessRows) {
    const key = (r.run||'')+'|'+r.id;
    if (r.t === 'req') reqs.set(key, r);
    else if (r.t === 'resp') pairs.push({req: reqs.get(key) || {}, resp: r});
  }
  pairs.sort((a, b) => (a.req.ts||'').localeCompare(b.req.ts||''));

  title.innerHTML = 'Session <code style="color:var(--cyan)">'+x(gwSessSelected)+'</code> · '+pairs.length+' turns';

  if (!pairs.length) {
    wrap.innerHTML = '<div style="color:var(--text-muted);padding:12px">No turns recorded.</div>';
    return;
  }
  // Stash so click → detail can locate the same pair without a re-fetch.
  gwRowsCache = pairs.slice().reverse(); // showGwDetail uses this index
  let prevIn = 0;
  wrap.innerHTML = pairs.map((p, i) => {
    const u = p.resp.usage || (p.resp.stream_summary||{}).usage || {};
    const totalIn = (u.input_tokens||0) + (u.cache_read_input_tokens||0) + (u.cache_creation_input_tokens||0);
    const out = u.output_tokens||0;
    const delta = i === 0 ? 0 : totalIn - prevIn;
    prevIn = totalIn;
    const deltaCls = delta > 0 ? 'delta-up' : delta < 0 ? 'delta-down' : '';
    const deltaLabel = i === 0 ? '' : (delta >= 0 ? '+' : '') + delta.toLocaleString();
    const stop = (p.resp.stream_summary||{}).stop_reason || '';
    const elapsed = p.resp.elapsed_ms ? p.resp.elapsed_ms+'ms' : '';
    const tools = ((p.resp.stream_summary||{}).tool_calls||[]).length;
    const idx = gwRowsCache.length - 1 - i; // mirror the reversal above
    return '<div class="gw-turn" onclick="showGwDetail('+idx+')">' +
      '<div class="turn-no">#'+(p.req.turn_index||(i+1))+'</div>' +
      '<div>' +
        '<div>'+x((p.req.ts||'').replace('T',' ').slice(5,19))+'  ·  ' +
          '<span style="color:var(--text-dim)">'+x(p.req.model||'?')+'</span>'+
          (stop ? '  ·  <span class="chip">stop <b>'+x(stop)+'</b></span>' : '')+
          (tools ? '  ·  <span class="chip">'+tools+' tool calls</span>' : '')+
        '</div>' +
        '<div style="color:var(--text-muted);font-size:11px;margin-top:2px">' +
          'in '+totalIn.toLocaleString()+' / out '+out.toLocaleString()+
          (deltaLabel ? ' · <span class="'+deltaCls+'">Δin '+deltaLabel+'</span>' : '')+
          (elapsed ? ' · '+elapsed : '')+
        '</div>' +
      '</div>' +
      '<div style="text-align:right;color:var(--text-muted);font-size:11px">→</div>' +
    '</div>';
  }).join('');
}

document.getElementById('gw-sess-search').addEventListener('input', e => {
  gwSessSearch = e.target.value; renderSessions();
});

// renderSeriesSVG draws a stacked bar chart of the four token buckets per
// time bucket. SVG only — no canvas, no chart libs. Each bar is a vertical
// stack: cache_read at the bottom, then cache_create, fresh input, output.
// Bars are equal-width with 1px gap; max value scales the chart height.
function renderSeriesSVG(series) {
  if (!series.length) {
    return '<div style="color:var(--text-muted);padding:20px;text-align:center">No data in window.</div>';
  }
  const w = 800, h = 140, pad = 18;
  const max = Math.max(1, ...series.map(s =>
    (s.cache_read_tokens||0) + (s.cache_creation_tokens||0) +
    (s.input_tokens||0) + (s.output_tokens||0)));
  const barW = Math.max(1, (w - 2*pad) / series.length - 1);
  const scale = (h - 2*pad) / max;
  const bars = series.map((s, i) => {
    const cr = (s.cache_read_tokens||0) * scale;
    const cw = (s.cache_creation_tokens||0) * scale;
    const fi = (s.input_tokens||0) * scale;
    const ou = (s.output_tokens||0) * scale;
    const x0 = pad + i * (barW + 1);
    let y = h - pad;
    const out = [];
    if (cr > 0) { y -= cr; out.push('<rect class="gx-cache-read" x="'+x0+'" y="'+y+'" width="'+barW+'" height="'+cr+'"></rect>'); }
    if (cw > 0) { y -= cw; out.push('<rect class="gx-cache-create" x="'+x0+'" y="'+y+'" width="'+barW+'" height="'+cw+'"></rect>'); }
    if (fi > 0) { y -= fi; out.push('<rect class="gx-input" x="'+x0+'" y="'+y+'" width="'+barW+'" height="'+fi+'"></rect>'); }
    if (ou > 0) { y -= ou; out.push('<rect class="gx-output" x="'+x0+'" y="'+y+'" width="'+barW+'" height="'+ou+'"></rect>'); }
    const title = s.t + ' — total ' + (cr+cw+fi+ou)/scale | 0;
    return '<g><title>'+x(title)+'</title>'+out.join('')+'</g>';
  }).join('');
  // X-axis tick labels at first, middle, last.
  const ticks = [0, Math.floor(series.length/2), series.length-1].filter((v,i,a) => a.indexOf(v) === i);
  const labels = ticks.map(i => {
    const t = series[i].t.replace('T',' ').slice(5, 16); // MM-DD HH:MM
    const tx = pad + i * (barW + 1);
    return '<text x="'+tx+'" y="'+(h-4)+'">'+x(t)+'</text>';
  }).join('');
  return '<svg class="gw-series-svg" viewBox="0 0 '+w+' '+h+'" preserveAspectRatio="none">' +
    bars + labels + '</svg>';
}


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
