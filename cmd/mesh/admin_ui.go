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
  --bg-alt: #1a1d28;
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
.tab-sep { width: 1px; background: var(--border); margin: 8px 4px; flex-shrink: 0; }

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
/* Scroll wrapper for tables: bounded height + horizontal scroll for
   long rows. Assigned per-table where we expect many rows or wide content. */
.table-scroll { max-height: 50vh; overflow: auto; }
.loading-row td { color: var(--text-muted); padding: 16px; font-style: italic; }

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

/* Tree table */
.tree-group td { background: var(--bg-alt); font-weight: 600; cursor: pointer; user-select: none; }
.tree-l1 td:first-child { padding-left: 24px; }
.tree-l2 td:first-child { padding-left: 44px; }
.tree-l3 td:first-child { padding-left: 64px; }
.met-tag {
  display: inline-block; padding: 1px 6px; margin: 0 2px;
  background: var(--bg); border-radius: 3px;
  font-size: 11px; color: var(--text-dim); font-family: var(--mono);
  white-space: nowrap;
}

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
/* Metadata key-value table (replaces chip row) */
.meta-tbl {
  border-collapse: collapse; font-size: 11px; font-family: var(--mono);
  margin-bottom: 8px; width: auto;
}
.meta-tbl td {
  padding: 2px 10px 2px 0; vertical-align: top; white-space: nowrap;
}
.meta-tbl .mk { color: var(--text-muted); }
.meta-tbl .mv { color: var(--text); font-weight: 600; }
.meta-tbl .mv.dim { color: var(--text-dim); font-weight: 400; }
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
/* .text is used outside .msg-body too (tool_result, custom-block bodies). */
.text { white-space: pre-wrap; word-break: break-word; }

/* Markdown syntax coloring for plain-text content. Not rendered — colored. */
.md-h        { color: var(--purple); font-weight: 700; }
.md-bold     { color: var(--text); font-weight: 700; }
.md-italic   { color: var(--text-dim); font-style: italic; }
.md-code     { color: var(--green); background: var(--bg-input); padding: 0 3px; border-radius: 3px; font-family: var(--mono); }
.md-fence    { color: var(--green); background: var(--bg-input); padding: 4px 6px; border-radius: 3px; display: block; font-family: var(--mono); white-space: pre-wrap; }
.md-link     { color: var(--cyan); }
.md-url      { color: var(--text-muted); }
.md-list     { color: var(--yellow); font-weight: 700; }
.md-quote    { color: var(--text-dim); border-left: 2px solid var(--border); padding-left: 6px; display: block; }
.md-hr       { color: var(--text-muted); }
.md-xml      { color: var(--text-dim); }
.md-xml-tag  { color: var(--cyan); font-weight: 600; }
/* Scrollable markdown viewer with optional TOC sidebar */
.md-viewer { display: flex; gap: 0; border: 1px solid var(--border); border-radius: 4px; background: var(--bg); }
.md-viewer .md-body {
  flex: 1; min-width: 0; max-height: 50vh; overflow: auto; padding: 8px 12px;
  font-family: var(--mono); font-size: 12px; line-height: 1.6; white-space: pre-wrap; word-break: break-word;
}
.md-viewer .md-toc {
  width: 200px; flex-shrink: 0; max-height: 50vh; overflow: auto;
  border-left: 1px solid var(--border); padding: 6px 8px;
  font-size: 10px; line-height: 1.6;
}
.md-viewer .md-toc a {
  display: block; color: var(--text-muted); text-decoration: none; cursor: pointer;
  padding: 1px 0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.md-viewer .md-toc a:hover { color: var(--green); }
.md-viewer .md-toc a.depth-2 { padding-left: 8px; }
.md-viewer .md-toc a.depth-3 { padding-left: 16px; }
.md-viewer .md-toc .toc-len { font-size: 9px; display: inline-block; width: 32px; text-align: right; margin-right: 4px; flex-shrink: 0; }
@media (max-width: 900px) { .md-viewer .md-toc { display: none; } }
/* Loading overlay for stale data */
.gw-loading { position: relative; pointer-events: none; }
.gw-loading::after {
  content: 'Loading…'; position: absolute; inset: 0;
  display: flex; align-items: center; justify-content: center;
  background: rgba(15,17,23,0.7); color: var(--text-muted);
  font-size: 12px; z-index: 10; border-radius: var(--radius);
}
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
/* Help icon (?) tooltip — custom CSS tooltip, not native title (native is
   slow/flaky across browsers). Text lives in data-help; ::after renders it. */
.help {
  position: relative;
  display: inline-block; width: 14px; height: 14px; line-height: 13px;
  text-align: center; border-radius: 50%; border: 1px solid var(--border);
  color: var(--text-muted); font-size: 9px; font-family: var(--mono);
  cursor: help; margin-left: 4px; vertical-align: middle;
}
.help:hover { color: var(--green); border-color: var(--green); }
.help[data-help]:hover::after {
  content: attr(data-help);
  position: absolute; z-index: 1000;
  bottom: calc(100% + 6px); left: 50%; transform: translateX(-50%);
  background: var(--bg-card); color: var(--text);
  border: 1px solid var(--border); border-radius: 4px;
  padding: 6px 8px; font-size: 11px; font-family: var(--sans, sans-serif);
  line-height: 1.4; text-align: left; white-space: normal;
  width: max-content; max-width: 320px;
  box-shadow: 0 4px 12px rgba(0,0,0,0.4);
  pointer-events: none;
}
.help[data-help]:hover::before {
  content: ''; position: absolute; z-index: 1001;
  bottom: calc(100% + 1px); left: 50%; transform: translateX(-50%);
  border: 5px solid transparent; border-top-color: var(--border);
  pointer-events: none;
}

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
  display: grid; grid-template-columns: 14px minmax(0, 1fr) auto auto; gap: 8px;
  align-items: center; padding: 4px 8px; font-size: 11px;
}
.ctx-item > summary::-webkit-details-marker { display: none; }
.ctx-item > summary::before {
  content: '▶'; font-size: 8px; color: var(--text-muted);
  transition: transform 0.1s;
}
.ctx-item[open] > summary::before { transform: rotate(90deg); }
.ctx-item:hover > summary { background: var(--bg-hover); }
/* Name column must be clippable so long previews cannot shove size/pct off
   the right edge. min-width:0 lets the grid track actually shrink; overflow
   clips and flex keeps <name> + <preview> on one line with the preview
   absorbing the ellipsis. */
.ctx-item .ctx-name {
  font-family: var(--mono); color: var(--text);
  display: flex; gap: 6px; align-items: baseline;
  min-width: 0; overflow: hidden; white-space: nowrap;
}
.ctx-item .ctx-name > :first-child { flex: none; }
.ctx-item .ctx-name .ctx-preview {
  color: var(--text-muted); font-weight: normal;
  overflow: hidden; text-overflow: ellipsis; min-width: 0; flex: 1;
}
.ctx-item .ctx-size { font-family: var(--mono); color: var(--text-muted); min-width: 80px; text-align: right; white-space: nowrap; }
.ctx-item .ctx-pct  { font-family: var(--mono); color: var(--text-dim); min-width: 52px; text-align: right; white-space: nowrap; }
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
.cblock.k-thinking           { border-left-color: var(--purple); }
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

/* Filter chip bar — multi-select toggle chips for gateway and session filters. */
.gw-chip {
  display: inline-block; padding: 3px 10px; border-radius: 12px;
  font-size: 11px; cursor: pointer; border: 1px solid var(--border);
  background: var(--bg-input); color: var(--text-dim);
  transition: background 0.1s, color 0.1s;
  user-select: none; white-space: nowrap;
}
.gw-chip:hover { color: var(--text); }
.gw-chip.on { background: var(--green); color: var(--bg); border-color: var(--green); }

/* Scroll container for long tables; keeps thead visible while body scrolls. */
.gw-scroll { max-height: 65vh; overflow: auto; }
.gw-scroll table thead th { position: sticky; top: 0; background: var(--bg-card); z-index: 1; }

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
.json-collapsed:not(.cblock) { display: none; }
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
  <div class="tab-sep"></div>
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
        <div class="table-scroll">
        <table>
          <thead><tr>
            <th>Status</th>
            <th>Name</th>
            <th>Detail</th>
            <th>Metrics</th>
          </tr></thead>
          <tbody id="comp-body"><tr class="loading-row"><td colspan="4">Loading components…</td></tr></tbody>
        </table>
        </div>
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
        <div class="table-scroll">
        <table>
          <thead><tr>
            <th>Direction</th>
            <th>Size</th>
            <th>Content</th>
            <th>Peer</th>
            <th>Time</th>
          </tr></thead>
          <tbody id="cs-body"><tr class="loading-row"><td colspan="5">Loading clipsync activity…</td></tr></tbody>
        </table>
        </div>
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
        <div class="table-scroll">
        <table>
          <thead><tr>
            <th data-sort="id">ID <span class="sort-arrow"></span></th>
            <th data-sort="path">Path <span class="sort-arrow"></span></th>
            <th data-sort="direction">Direction <span class="sort-arrow"></span></th>
            <th data-sort="file_count">Files <span class="sort-arrow"></span></th>
            <th data-sort="dir_count">Dirs <span class="sort-arrow"></span></th>
            <th data-sort="total_bytes">Size <span class="sort-arrow"></span></th>
            <th data-sort="last_sync">Last sync <span class="sort-arrow"></span></th>
            <th data-sort="peers">Peers <span class="sort-arrow"></span></th>
          </tr></thead>
          <tbody id="fs-body"><tr class="loading-row"><td colspan="8">Loading folders…</td></tr></tbody>
        </table>
        </div>
      </div>
    </div>
    <div class="card" id="conflict-card">
      <div class="card-header"><span>Conflicts</span><span class="badge badge-ok" id="conflict-count">0</span></div>
      <div class="card-body">
        <div class="table-scroll">
        <table>
          <thead><tr><th>Folder</th><th>Path</th></tr></thead>
          <tbody id="conflict-body"><tr class="loading-row"><td colspan="2">Loading conflicts…</td></tr></tbody>
        </table>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><span>Recent Activity</span></div>
      <div class="card-body">
        <div class="table-scroll">
        <table>
          <thead><tr><th>Direction</th><th>Folder</th><th>Peer</th><th>Files</th><th>Size</th><th>Time</th></tr></thead>
          <tbody id="fsa-body"><tr class="loading-row"><td colspan="6">Loading activity…</td></tr></tbody>
        </table>
        </div>
      </div>
    </div>
  </div>

  <!-- Gateway panel -->
  <div class="panel" id="p-gateway">
    <div style="display:flex;align-items:center;gap:12px;margin-bottom:8px;flex-wrap:wrap">
      <div id="gw-chips" style="display:inline-flex;gap:4px;flex-wrap:wrap"></div>
      <select id="gw-window" style="background:var(--bg-input);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px">
        <option value="24h" selected>last 24h</option>
        <option value="1h">last 1h</option>
        <option value="7d">last 7d</option>
        <option value="30d">last 30d</option>
        <option value="all">all time</option>
      </select>
      <div style="display:inline-flex;border:1px solid var(--border);border-radius:6px;overflow:hidden">
        <div class="gw-sub-btn active" data-sub="overview">Overview</div>
        <div class="gw-sub-btn" data-sub="requests">Requests</div>
      </div>
    </div>
    <div id="gw-sess-chips" style="display:flex;gap:4px;flex-wrap:wrap;margin-bottom:12px"></div>

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
              <thead><tr><th>Project / path</th><th>Requests</th><th>Tokens (in/out)</th></tr></thead>
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
          <span class="help" data-help="Highest-total-token pairs in the current window. Click any row to open its detail card.">?</span>
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
          <span class="help" data-help="Injected pseudo-XML blocks (system-reminder, command-*, task-notification, hooks) aggregated by tag + first 60 chars. Total chars tells you which recurring context is wasting the most tokens across the window.">?</span>
        </div>
        <div class="card-body">
          <table>
            <thead><tr><th>Tag + signature</th><th>Occurrences</th><th>Total chars</th></tr></thead>
            <tbody id="gw-preamble-blocks"></tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- Requests sub-view -->
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
        <div class="gw-scroll">
          <table>
            <thead><tr>
              <th>Time</th>
              <th>Gateway</th>
              <th>Session</th>
              <th>Dir</th>
              <th>Client model</th>
              <th>Upstream model</th>
              <th>Stream</th>
              <th>Status</th>
              <th>Outcome</th>
              <th>In</th>
              <th>Out</th>
              <th>Time</th>
              <th>Summary</th>
            </tr></thead>
            <tbody id="gw-body"></tbody>
          </table>
        </div>
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
              <span>Client request <span id="gw-req-len" class="sec-len" style="margin-left:6px"></span></span>
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
            <div id="gw-upstream-req-sec" style="display:none">
              <h4 style="margin-top:16px;border-top:2px solid var(--border);padding-top:8px">
                <span>Upstream request <span id="gw-upstream-req-len" class="sec-len" style="margin-left:6px"></span></span>
              </h4>
              <details class="sec">
                <summary><span class="sec-title">Raw JSON</span><span class="sec-len" id="gw-upstream-req-json-len"></span></summary>
                <div class="sec-body"><div class="gw-detail-raw" id="gw-upstream-req-raw"></div></div>
              </details>
              <div class="gw-detail-structured" id="gw-upstream-req-structured"></div>
            </div>
          </div>
          <div class="gw-detail-pane">
            <h4>
              <span>Client response <span id="gw-resp-len" class="sec-len" style="margin-left:6px"></span></span>
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
            <div id="gw-upstream-resp-sec" style="display:none">
              <h4 style="margin-top:16px;border-top:2px solid var(--border);padding-top:8px">
                <span>Upstream response <span id="gw-upstream-resp-len" class="sec-len" style="margin-left:6px"></span></span>
              </h4>
              <details class="sec">
                <summary><span class="sec-title">Raw JSON</span><span class="sec-len" id="gw-upstream-resp-json-len"></span></summary>
                <div class="sec-body"><div class="gw-detail-raw" id="gw-upstream-resp-raw"></div></div>
              </details>
              <div class="gw-detail-structured" id="gw-upstream-resp-structured"></div>
            </div>
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
      <div class="chart-card" id="chart-tokens-card" style="display:none">
        <div class="chart-title">Gateway Token Throughput</div>
        <div class="chart-sub">
          <div class="chart-value" style="color:var(--cyan)"><span id="chart-tokin-val">0</span> <span style="font-size:11px;color:var(--text-muted)">in/s</span></div>
          <div class="chart-value" style="color:var(--yellow)"><span id="chart-tokout-val">0</span> <span style="font-size:11px;color:var(--text-muted)">out/s</span></div>
        </div>
        <canvas class="chart-canvas" id="chart-tokens"></canvas>
      </div>
      <div class="chart-card">
        <div class="chart-title">Component Health</div>
        <div class="chart-sub">
          <div class="chart-value" style="color:var(--green)"><span id="chart-health-up">0</span> <span style="font-size:11px;color:var(--text-muted)">up</span></div>
          <div class="chart-value" style="color:var(--red)"><span id="chart-health-down">0</span> <span style="font-size:11px;color:var(--text-muted)">down</span></div>
          <div class="chart-value" style="color:var(--yellow)"><span id="chart-health-pend">0</span> <span style="font-size:11px;color:var(--text-muted)">pending</span></div>
        </div>
        <canvas class="chart-canvas" id="chart-health"></canvas>
      </div>
      <div class="chart-card" id="chart-fs-traffic-card" style="display:none">
        <div class="chart-title">Filesync Traffic</div>
        <div class="chart-sub">
          <div class="chart-value" style="color:var(--green)"><span id="chart-fs-dl-val">0 B/s</span> <span style="font-size:11px;color:var(--text-muted)">dl</span></div>
          <div class="chart-value" style="color:var(--purple)"><span id="chart-fs-ul-val">0 B/s</span> <span style="font-size:11px;color:var(--text-muted)">ul</span></div>
        </div>
        <canvas class="chart-canvas" id="chart-fs-traffic"></canvas>
      </div>
      <div class="chart-card" id="chart-fs-sync-card" style="display:none">
        <div class="chart-title">Filesync Activity</div>
        <div class="chart-sub">
          <div class="chart-value" style="color:var(--cyan)"><span id="chart-fs-cycles-val">0</span> <span style="font-size:11px;color:var(--text-muted)">syncs</span></div>
          <div class="chart-value" style="color:var(--red)"><span id="chart-fs-errors-val">0</span> <span style="font-size:11px;color:var(--text-muted)">errors</span></div>
        </div>
        <canvas class="chart-canvas" id="chart-fs-sync"></canvas>
      </div>
    </div>
    <div class="card" id="met-comp-card">
      <div class="card-header">
        <span>Per-Component Traffic</span>
      </div>
      <div class="card-body">
        <div class="table-scroll">
        <table>
          <thead><tr>
            <th>Type</th><th>ID</th><th>TX</th><th>RX</th><th>Streams</th><th>Uptime</th>
          </tr></thead>
          <tbody id="met-comp-body"></tbody>
        </table>
        </div>
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
let fsSort = {col:'id', asc:true};
let logLevel = 'all';
let logMode = 'recent'; // 'recent' (ring buffer) or 'file' (full log file)
let fileLogLines = [], fileLogSize = 0, fileLogLoaded = false;
const HIST_LEN = 60;
const chartHist = {tx:[], rx:[], streams:[], goroutines:[], fds:[], tokensIn:[], tokensOut:[], healthUp:[], healthDown:[], healthPending:[], fsDlRate:[], fsUlRate:[], fsSyncErrors:[], fsSyncCycles:[]};
let prevTotalTx = 0, prevTotalRx = 0, prevTotalTokIn = 0, prevTotalTokOut = 0, firstTick = true;
let prevFsDl = 0, prevFsUl = 0, prevFsErrors = 0, prevFsCycles = 0;
let compMetrics = {};

// --- Tabs ---
const tabMap = {'/ui':'dashboard','/ui/clipsync':'clipsync','/ui/filesync':'filesync','/ui/gateway':'gateway','/ui/logs':'logs','/ui/metrics':'metrics','/ui/api':'api','/ui/debug':'debug'};
let activeTab = tabMap[location.pathname] || 'dashboard';
// Gateway hash routing state; declared up here so applyGwHash() (called from
// showTab on initial load) does not hit the TDZ.
let gwHashLast = '';
let gwHashApplyingDeep = '';
let gwSelectedSet = new Set();  // empty = all gateways shown
let gwSessionSet = new Set();   // empty = all sessions shown
let gwDetailKey = '';

function showTab(name, opts) {
  opts = opts || {};
  const changed = activeTab !== name;
  activeTab = name;
  document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  document.querySelectorAll('.panel').forEach(p => p.classList.toggle('active', p.id === 'p-'+name));
  const path = '/ui' + (name === 'dashboard' ? '' : '/'+name);
  // Preserve the hash when pushing so deep-links (e.g. /ui/gateway#sessions/SID)
  // survive programmatic tab changes. Strip the hash for non-gateway tabs.
  const hash = (name === 'gateway' && !opts.clearHash) ? location.hash : '';
  const full = path + hash;
  if (opts.push !== false && (location.pathname + location.hash) !== full) {
    history.pushState(null, '', full);
  }
  if (name === 'gateway') applyGwHash();
  else if (changed && location.hash) history.replaceState(null, '', path);
}

document.getElementById('tabs').addEventListener('click', e => {
  if (e.target.classList.contains('tab')) showTab(e.target.dataset.tab, {clearHash: true});
});
window.addEventListener('popstate', () => { showTab(tabMap[location.pathname] || 'dashboard', {push: false}); });
window.addEventListener('hashchange', () => { if (activeTab === 'gateway') applyGwHash(); });
showTab(activeTab, {push: false});

// --- Data fetch (gated by active tab) ---
// Each endpoint fires independently so a slow one (e.g. filesync on a huge
// m2 repo) never blocks the dashboard/logs/clipsync panels from rendering.
// Per-section render() is called as soon as that section's data lands.
function tick() {
  const needLogs = activeTab === 'dashboard' || activeTab === 'logs';
  const needFilesync = activeTab === 'dashboard' || activeTab === 'filesync';
  const needClipsync = activeTab === 'dashboard' || activeTab === 'clipsync';
  const needGateway = activeTab === 'gateway';

  const mark = () => { document.getElementById('hdr-status').textContent = 'updated ' + new Date().toLocaleTimeString(); };
  const fail = (what) => (e) => { document.getElementById('hdr-status').textContent = 'error('+what+'): ' + (e.message||e); };
  const ok = (r, what) => { if (!r.ok) throw 'HTTP ' + r.status; return r; };

  fetch('/api/state').then(r=>ok(r)).then(r=>r.json()).then(s => {
    state = s; renderStats(); if (activeTab === 'dashboard') renderComponents(); mark();
  }).catch(fail('state'));

  fetch('/api/metrics').then(r=>ok(r)).then(r=>r.text()).then(t => {
    metricsText = t;
    compMetrics = extractCompMetrics(t);
    const nowTx = sumMetric(metricsText, 'mesh_bytes_tx_total');
    const nowRx = sumMetric(metricsText, 'mesh_bytes_rx_total');
    const nowTokIn = sumMetric(metricsText, 'mesh_gateway_tokens_in_total');
    const nowTokOut = sumMetric(metricsText, 'mesh_gateway_tokens_out_total');
    const nowFsDl = sumMetric(metricsText, 'mesh_filesync_bytes_downloaded_total');
    const nowFsUl = sumMetric(metricsText, 'mesh_filesync_bytes_uploaded_total');
    const nowFsErrors = sumMetric(metricsText, 'mesh_filesync_sync_errors_total');
    const nowFsCycles = sumMetric(metricsText, 'mesh_filesync_peer_syncs_total');
    if (!firstTick) {
      pushHist('tx', Math.max(0, nowTx - prevTotalTx));
      pushHist('rx', Math.max(0, nowRx - prevTotalRx));
      pushHist('tokensIn', Math.max(0, nowTokIn - prevTotalTokIn));
      pushHist('tokensOut', Math.max(0, nowTokOut - prevTotalTokOut));
      pushHist('fsDlRate', Math.max(0, nowFsDl - prevFsDl));
      pushHist('fsUlRate', Math.max(0, nowFsUl - prevFsUl));
      pushHist('fsSyncErrors', Math.max(0, nowFsErrors - prevFsErrors));
      pushHist('fsSyncCycles', Math.max(0, nowFsCycles - prevFsCycles));
    } else { firstTick = false; }
    prevTotalTx = nowTx; prevTotalRx = nowRx;
    prevTotalTokIn = nowTokIn; prevTotalTokOut = nowTokOut;
    prevFsDl = nowFsDl; prevFsUl = nowFsUl; prevFsErrors = nowFsErrors; prevFsCycles = nowFsCycles;
    pushHist('streams', sumMetric(metricsText, 'mesh_active_streams'));
    pushHist('goroutines', valMetric(metricsText, 'mesh_process_goroutines'));
    const fds = valMetric(metricsText, 'mesh_process_open_fds');
    if (fds !== null) pushHist('fds', fds);
    const comps = Object.values(state);
    pushHist('healthUp', comps.filter(c => c.status === 'listening' || c.status === 'connected').length);
    pushHist('healthDown', comps.filter(c => c.status === 'failed').length);
    pushHist('healthPending', comps.filter(c => c.status !== 'listening' && c.status !== 'connected' && c.status !== 'failed').length);
    if (activeTab === 'metrics') renderMetrics();
    if (activeTab === 'dashboard') { renderCharts(); renderComponents(); }
    if (activeTab === 'debug') renderDebugStats();
  }).catch(fail('metrics'));

  if (needLogs) {
    fetch('/api/logs').then(r=>ok(r)).then(r=>r.json()).then(v => { logs = v; renderDashLogs(); renderLogs(); }).catch(fail('logs'));
  }
  if (needFilesync) {
    fetch('/api/filesync/folders').then(r=>ok(r)).then(r=>r.json()).then(v => { folders = v; renderFilesync(); }).catch(fail('folders'));
    fetch('/api/filesync/conflicts').then(r=>ok(r)).then(r=>r.json()).then(v => { conflicts = v; renderConflicts(); }).catch(fail('conflicts'));
    fetch('/api/filesync/activity').then(r=>ok(r)).then(r=>r.json()).then(v => { fsActivities = v; renderFsActivity(); }).catch(fail('fs-activity'));
  }
  if (needClipsync) {
    fetch('/api/clipsync/activity').then(r=>ok(r)).then(r=>r.json()).then(v => { clipActivities = v; renderClipsync(); }).catch(fail('clipsync'));
  }
  if (needGateway) {
    fetch('/api/gateway/audit?limit=200').then(r=>ok(r)).then(r=>r.json()).then(v => { gatewayAudit = v; renderGateway(); }).catch(fail('gateway'));
    // Stats endpoint takes a single gateway name. Fetch when exactly one is
    // active (either one selected chip or only one gateway exists).
    const statsGw = gwSelectedSet.size === 1 ? [...gwSelectedSet][0]
      : (gwSelectedSet.size === 0 && gatewayAudit && gatewayAudit.length === 1) ? gatewayAudit[0].gateway : '';
    if (statsGw) {
      fetch('/api/gateway/audit/stats?gateway='+encodeURIComponent(statsGw)+
        '&window='+encodeURIComponent(gwWindow)+'&bucket='+gwBucket(gwWindow))
        .then(r=>ok(r)).then(r=>r.json()).then(v => { gwStats = v; if (gwSubview === 'overview') renderGatewayOverview(); }).catch(fail('gw-stats'));
    }
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
function x(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
// xa: HTML-attribute-safe escape. Use whenever an interpolation lands inside
// attr="..." (title, data-*, href, src, style). x() alone leaves " and '
// which let attacker-controlled strings break out of the attribute value.
function xa(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')
                  .replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}
// xj: JS-string-in-HTML-attribute escape. Use for every '+ id +' interpolation
// inside onclick="foo('…')". HTML entities decode before JS sees the value, so
// xa() alone does not contain a breakout via ' — we must JS-escape first, then
// HTML-escape. Pattern: '<a onclick="foo(\''+xj(id)+'\')">'.
function xj(s) {
  const j = String(s).replace(/\\/g,'\\\\').replace(/'/g,"\\'")
                     .replace(/\r/g,'\\r').replace(/\n/g,'\\n');
  return j.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
// safeUrl: return url if scheme is http/https, else '#'. Blocks javascript:,
// data:, vbscript:, file: from reaching a rendered href/src. Called before xa.
function safeUrl(u) {
  const s = String(u||'').trim();
  return /^https?:\/\//i.test(s) ? s : '#';
}
// fmtLocalTime: show an ISO timestamp in the viewer's local timezone with the
// raw UTC string kept in a tooltip so the user can still copy/verify it.
function fmtLocalTime(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return String(ts);
  return '<span title="'+xa(ts)+'">'+xa(d.toLocaleString())+'</span>';
}
// fmtTokens: localized integer with a warning prefix when JS loses precision.
function fmtTokens(n) {
  const v = Number(n||0);
  if (!Number.isSafeInteger(v)) return '≈'+v.toLocaleString();
  return v.toLocaleString();
}

// fmtElapsed formats milliseconds into a human-readable string with
// appropriate units: ms for <1s, s for <60s, m+s for >=60s.
function fmtElapsed(ms) {
  ms = Number(ms) || 0;
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  const m = Math.floor(ms / 60000);
  const s = Math.round((ms % 60000) / 1000);
  return m + 'm ' + s + 's';
}
// fmtElapsedHtml wraps fmtElapsed in a colored span based on duration.
function fmtElapsedHtml(ms) {
  ms = Number(ms) || 0;
  const clr = ms > 30000 ? 'var(--red)' : ms > 5000 ? 'var(--yellow)' : ms > 1000 ? 'var(--text)' : 'var(--green)';
  return '<span style="color:'+clr+'">'+fmtElapsed(ms)+'</span>';
}
// fmtTokensHtml wraps a token count in a colored span based on magnitude.
function fmtTokensHtml(n) {
  n = Number(n) || 0;
  const clr = n > 50000 ? 'var(--red)' : n > 10000 ? 'var(--yellow)' : n > 1000 ? 'var(--text)' : 'var(--text-muted)';
  return '<span style="color:'+clr+'">'+fmtTokens(n)+'</span>';
}

function badge(status) {
  const s = String(status);
  if (s === 'listening' || s === 'connected') return '<span class="badge badge-ok">'+x(s)+'</span>';
  if (s === 'connecting' || s === 'retrying' || s === 'scanning' || s === 'starting') return '<span class="badge badge-warn">'+x(s)+'</span>';
  if (s === 'failed') return '<span class="badge badge-err">'+x(s)+'</span>';
  return '<span class="badge badge-off">'+x(s)+'</span>';
}

function renderStats() {
  const comps = Object.values(state);
  const up = comps.filter(c => c.status === 'listening' || c.status === 'connected').length;
  const down = comps.filter(c => c.status === 'failed').length;
  const pending = comps.length - up - down;
  const totalFiles = folders.reduce((s,f) => s + (f.file_count||0), 0);
  const totalBytes = folders.reduce((s,f) => s + (f.total_bytes||0), 0);
  document.getElementById('dash-stats').innerHTML =
    stat('Components', comps.length, up+' up') +
    stat('Healthy', up, comps.length ? Math.round(up/comps.length*100)+'%' : '-', up === comps.length ? 'var(--green)' : 'var(--yellow)') +
    stat('Failed', down, '', down > 0 ? 'var(--red)' : 'var(--green)') +
    stat('Pending', pending, '');
  document.getElementById('fs-stats').innerHTML =
    stat('Folders', folders.length, '') +
    stat('Total Files', totalFiles.toLocaleString(), '') +
    stat('Total Size', fmtBytes(totalBytes), '') +
    stat('Conflicts', conflicts.length, '', conflicts.length > 0 ? 'var(--red)' : 'var(--green)');
}

function stat(label, value, sub, color) {
  const c = color ? ' style="color:'+color+'"' : '';
  return '<div class="stat"><div class="stat-label">'+x(label)+'</div><div class="stat-value"'+c+'>'+x(String(value))+'</div>'+(sub?'<div class="stat-sub">'+x(sub)+'</div>':'')+'</div>';
}

const collapsedNodes = new Set();

function extractCompMetrics(text) {
  const cm = {};
  const labeled = /^(mesh_(?:bytes_tx_total|bytes_rx_total|active_streams|uptime_seconds|gateway_tokens_in_total|gateway_tokens_out_total|gateway_tokens_cache_read_total|gateway_tokens_cache_creation_total))\{([^}]*)\}\s+(\S+)/gm;
  let m;
  while ((m = labeled.exec(text)) !== null) {
    const name = m[1], labelsStr = m[2], val = parseFloat(m[3]) || 0;
    const labels = {};
    for (const part of labelsStr.split(',')) {
      const eq = part.indexOf('=');
      if (eq > 0) labels[part.slice(0, eq)] = part.slice(eq+1).replace(/^"|"$/g, '');
    }
    const key = (labels.type ? labels.type + ':' : 'gateway:') + (labels.id || '');
    if (!cm[key]) cm[key] = {};
    if (name === 'mesh_bytes_tx_total') cm[key].tx = val;
    else if (name === 'mesh_bytes_rx_total') cm[key].rx = val;
    else if (name === 'mesh_active_streams') cm[key].streams = val;
    else if (name === 'mesh_uptime_seconds') cm[key].uptime = val;
    else if (name === 'mesh_gateway_tokens_in_total') cm[key].tokensIn = val;
    else if (name === 'mesh_gateway_tokens_out_total') cm[key].tokensOut = val;
    else if (name === 'mesh_gateway_tokens_cache_read_total') cm[key].cacheRd = val;
    else if (name === 'mesh_gateway_tokens_cache_creation_total') cm[key].cacheWr = val;
  }
  return cm;
}

function fmtUptime(secs) {
  if (!secs || secs <= 0) return '';
  if (secs < 60) return Math.floor(secs) + 's';
  if (secs < 3600) return Math.floor(secs/60) + 'm';
  if (secs < 86400) { const h = Math.floor(secs/3600); const m = Math.floor((secs%3600)/60); return h + 'h' + (m ? ' ' + m + 'm' : ''); }
  const d = Math.floor(secs/86400); const h = Math.floor((secs%86400)/3600);
  return d + 'd' + (h ? ' ' + h + 'h' : '');
}

function metTags(c) {
  const key = c.type + ':' + c.id;
  const m = compMetrics[key];
  let tags = '';
  if (m) {
    if (m.uptime) tags += '<span class="met-tag" title="uptime">&#9650; ' + x(fmtUptime(m.uptime)) + '</span>';
    if (m.tx || m.rx) tags += '<span class="met-tag" title="traffic &#8593;/&#8595;">&#8693; ' + x(fmtBytes((m.tx||0)+(m.rx||0))) + '</span>';
    if (m.streams) tags += '<span class="met-tag" title="active streams">' + m.streams + ' stream' + (m.streams !== 1 ? 's' : '') + '</span>';
    if (m.tokensIn || m.tokensOut) tags += '<span class="met-tag" title="tokens in/out">' + fmtTokens(m.tokensIn||0) + ' in / ' + fmtTokens(m.tokensOut||0) + ' out</span>';
    if (m.cacheRd) tags += '<span class="met-tag" title="cache read">' + fmtTokens(m.cacheRd) + ' cached</span>';
  }
  if (c.type === 'filesync-folder' && c.file_count) {
    tags += '<span class="met-tag">' + c.file_count.toLocaleString() + ' files (' + fmtBytes(c.total_size||0) + ')</span>';
  }
  if (c.type === 'filesync-folder' && c.last_sync && !c.last_sync.startsWith('0001')) {
    tags += '<span class="met-tag" title="last sync">sync ' + timeAgo(c.last_sync) + '</span>';
  }
  if (c.type === 'filesync-peer' && c.last_sync && !c.last_sync.startsWith('0001')) {
    tags += '<span class="met-tag" title="last sync">sync ' + timeAgo(c.last_sync) + '</span>';
  }
  return tags;
}

function buildComponentTree() {
  const comps = Object.values(state);
  const byType = {};
  for (const c of comps) (byType[c.type] = byType[c.type] || []).push(c);
  const sortById = arr => arr.sort((a,b) => a.id.localeCompare(b.id));
  const groups = [];

  // Connections → forwards
  const connections = sortById(byType['connection'] || []);
  const forwards = sortById(byType['forward'] || []);
  if (connections.length || forwards.length) {
    const claimed = new Set();
    const items = connections.map(conn => {
      const children = forwards.filter(f => {
        const bi = f.id.indexOf(' [');
        return bi > 0 && f.id.substring(0, bi) === conn.id;
      });
      children.forEach(c => claimed.add(c.id));
      return { comp: conn, children: children.map(f => ({ comp: f, children: [] })) };
    });
    forwards.filter(f => !claimed.has(f.id)).forEach(f => items.push({ comp: f, children: [] }));
    groups.push({ label: 'Connections', items });
  }

  // Servers → dynamic sessions
  const servers = sortById(byType['server'] || []);
  const dynamics = sortById(byType['dynamic'] || []);
  if (servers.length || dynamics.length) {
    const claimed = new Set();
    const items = servers.map(srv => {
      const children = dynamics.filter(d => {
        const pi = d.id.lastIndexOf('|');
        return pi > 0 && d.id.substring(pi + 1) === srv.id;
      });
      children.forEach(c => claimed.add(c.id));
      return { comp: srv, children: children.map(d => ({ comp: d, children: [] })) };
    });
    dynamics.filter(d => !claimed.has(d.id)).forEach(d => items.push({ comp: d, children: [] }));
    groups.push({ label: 'Servers', items });
  }

  // Proxies
  if (byType['proxy']?.length) groups.push({ label: 'Proxies', items: sortById(byType['proxy']).map(c => ({ comp: c, children: [] })) });

  // Relays
  if (byType['relay']?.length) groups.push({ label: 'Relays', items: sortById(byType['relay']).map(c => ({ comp: c, children: [] })) });

  // Filesync → folders → peers
  const fsyncs = sortById(byType['filesync'] || []);
  const ffolders = sortById(byType['filesync-folder'] || []);
  const fpeers = sortById(byType['filesync-peer'] || []);
  if (fsyncs.length || ffolders.length) {
    const claimedPeers = new Set();
    const buildFolderNodes = () => ffolders.map(ff => {
      const peers = fpeers.filter(fp => { const pi = fp.id.indexOf('|'); return pi > 0 && fp.id.substring(0, pi) === ff.id; });
      peers.forEach(p => claimedPeers.add(p.id));
      return { comp: ff, children: peers.map(p => ({ comp: p, children: [] })) };
    });
    if (fsyncs.length) {
      const items = fsyncs.map(fs => ({ comp: fs, children: buildFolderNodes() }));
      const orphanPeers = fpeers.filter(p => !claimedPeers.has(p.id));
      orphanPeers.forEach(p => items.push({ comp: p, children: [] }));
      groups.push({ label: 'Filesync', items });
    } else {
      const folderNodes = buildFolderNodes();
      const orphanPeers = fpeers.filter(p => !claimedPeers.has(p.id));
      orphanPeers.forEach(p => folderNodes.push({ comp: p, children: [] }));
      groups.push({ label: 'Filesync', items: folderNodes });
    }
  }

  // Clipsync → peers
  const csyncs = sortById(byType['clipsync'] || []);
  const cpeers = sortById(byType['clipsync-peer'] || []);
  if (csyncs.length || cpeers.length) {
    const claimed = new Set();
    const items = csyncs.map(cs => {
      const peers = cpeers.filter(cp => { const pi = cp.id.indexOf('|'); return pi > 0 && cp.id.substring(0, pi) === cs.id; });
      peers.forEach(p => claimed.add(p.id));
      return { comp: cs, children: peers.map(p => ({ comp: p, children: [] })) };
    });
    cpeers.filter(p => !claimed.has(p.id)).forEach(p => items.push({ comp: p, children: [] }));
    groups.push({ label: 'Clipsync', items });
  }

  // Gateways
  if (byType['gateway']?.length) groups.push({ label: 'Gateways', items: sortById(byType['gateway']).map(c => ({ comp: c, children: [] })) });

  return groups;
}

function childDisplayName(c, parentId) {
  if (c.type === 'forward') {
    const bi = c.id.indexOf(' [');
    return bi > 0 ? c.id.substring(bi) : c.id;
  }
  if (c.type === 'dynamic' || c.type === 'filesync-peer' || c.type === 'clipsync-peer') {
    const pi = c.id.indexOf('|');
    return pi > 0 ? c.id.substring(0, pi) : c.id;
  }
  return c.id;
}

function renderComponents() {
  const filter = document.getElementById('comp-search').value.toLowerCase();
  const groups = buildComponentTree();
  const el = document.getElementById('comp-body');
  if (!groups.length) { el.innerHTML = '<tr><td colspan="4" style="color:var(--text-muted);padding:20px">No components</td></tr>'; return; }

  function matchFilter(node) {
    const c = node.comp;
    const txt = (c.type+c.id+c.status+(c.message||'')+(c.peer_addr||'')+(c.bound_addr||'')).toLowerCase();
    if (txt.includes(filter)) return true;
    return node.children.some(ch => matchFilter(ch));
  }

  function countHealth(items) {
    let total = 0, ok = 0;
    function walk(nodes) {
      for (const n of nodes) { total++; if (n.comp.status === 'connected' || n.comp.status === 'listening') ok++; walk(n.children); }
    }
    walk(items);
    return { total, ok };
  }

  let html = '';
  for (const g of groups) {
    const filtered = filter ? g.items.filter(n => matchFilter(n)) : g.items;
    if (!filtered.length) continue;
    const h = countHealth(filtered);
    const gKey = 'g:' + g.label;
    const collapsed = collapsedNodes.has(gKey);
    const arrow = collapsed ? '&#9654;' : '&#9660;';
    html += '<tr class="tree-group" onclick="toggleNode(\''+xj(gKey)+'\')">';
    html += '<td colspan="3">'+arrow+' '+x(g.label)+' <span style="color:var(--text-muted);font-weight:400">('+h.ok+'/'+h.total+')</span></td>';
    html += '<td></td></tr>';
    if (collapsed) continue;
    for (const node of filtered) {
      html += renderTreeNode(node, 1, filter);
    }
  }
  el.innerHTML = html;
}

function renderTreeNode(node, depth, filter) {
  const c = node.comp;
  const cls = 'tree-l' + depth;
  const hasChildren = node.children.length > 0;
  const nKey = c.type + ':' + c.id;
  const collapsed = collapsedNodes.has(nKey);
  const detail = c.message || c.peer_addr || c.bound_addr || '';
  const tags = metTags(c);
  let displayName = depth > 1 ? childDisplayName(c) : c.id;
  let html = '';

  if (hasChildren) {
    const arrow = collapsed ? '&#9654; ' : '&#9660; ';
    html += '<tr class="'+cls+'" onclick="toggleNode(\''+xj(nKey)+'\')" style="cursor:pointer">';
    html += '<td>'+badge(c.status)+'</td>';
    html += '<td>'+arrow+x(displayName)+'</td>';
    html += '<td style="color:var(--text-dim)">'+x(detail)+'</td>';
    html += '<td>'+tags+'</td></tr>';
    if (!collapsed) {
      for (const ch of node.children) {
        html += renderTreeNode(ch, depth + 1, filter);
      }
    }
  } else {
    html += '<tr class="'+cls+'">';
    html += '<td>'+badge(c.status)+'</td>';
    html += '<td>'+x(displayName)+'</td>';
    html += '<td style="color:var(--text-dim)">'+x(detail)+'</td>';
    html += '<td>'+tags+'</td></tr>';
  }
  return html;
}

function toggleNode(key) {
  if (collapsedNodes.has(key)) collapsedNodes.delete(key);
  else collapsedNodes.add(key);
  renderComponents();
}

function renderDashLogs() {
  const el = document.getElementById('dash-logs');
  if (!logs.length) { el.innerHTML = '<div style="color:var(--text-muted);padding:8px">No logs yet</div>'; return; }
  const last = logs.slice(-10);
  el.innerHTML = last.map(l => '<div class="log-line">' + colorLog(x(l)) + '</div>').join('');
  el.scrollTop = el.scrollHeight;
}

function peerLabel(p) {
  // Backward compat: if peers is an array of strings (old API), fall back.
  if (typeof p === 'string') return p;
  if (!p) return '';
  if (p.name) return p.name + ' (' + p.addr + ')';
  return p.addr || '';
}

const expandedFolders = new Set();
function toggleFolder(id) {
  if (expandedFolders.has(id)) expandedFolders.delete(id);
  else expandedFolders.add(id);
  renderFilesync();
}

function fmtTime(t) {
  if (!t || (typeof t === 'string' && t.startsWith('0001-01-01'))) {
    return '<span style="color:var(--text-muted)">never</span>';
  }
  return timeAgo(t);
}

function renderFolderDetail(f) {
  const ignores = (f.ignore_patterns||[]);
  const ignoreHtml = ignores.length
    ? ignores.map(p => '<span class="badge badge-off" style="margin:2px 4px 2px 0;font-family:var(--mono)">'+x(p)+'</span>').join('')
    : '<span style="color:var(--text-muted)">none</span>';

  const peers = f.peers||[];
  const peerRowsHtml = peers.length ? peers.map(p => {
    const pend = p.pending || {};
    const total = (pend.downloads||0) + (pend.conflicts||0) + (pend.deletes||0);
    const planParts = [];
    if (pend.downloads) planParts.push('<span style="color:var(--green)">&#8595; '+pend.downloads+'</span>');
    if (pend.conflicts) planParts.push('<span style="color:var(--red)">&#9888; '+pend.conflicts+'</span>');
    if (pend.deletes)   planParts.push('<span style="color:var(--yellow)">&#10007; '+pend.deletes+'</span>');
    const plan = planParts.length
      ? planParts.join(' &middot; ') + ' <span style="color:var(--text-muted)">('+fmtBytes(pend.bytes||0)+')</span>'
      : '<span style="color:var(--green)">in sync</span>';
    const planAge = pend.updated_at && !pend.updated_at.startsWith('0001-01-01')
      ? ' <span style="color:var(--text-muted)">&middot; '+timeAgo(pend.updated_at)+'</span>'
      : '';
    const errHtml = p.last_error
      ? '<div style="color:var(--red);font-size:11px;margin-top:2px">'+x(p.last_error)+'</div>'
      : '';
    return '<tr>'
      + '<td>'+x(p.name||'-')+'</td>'
      + '<td style="font-family:var(--mono)">'+x(p.addr)+'</td>'
      + '<td>'+fmtTime(p.last_sync)+'</td>'
      + '<td style="font-family:var(--mono)">'+(p.last_seen_sequence||0)+' / '+(p.last_sent_sequence||0)+'</td>'
      + '<td>'+plan+planAge+errHtml+'</td>'
      + '<td>'+(total ? fmtTokens(total) : '0')+'</td>'
      + '</tr>';
  }).join('') : '<tr><td colspan="6" style="color:var(--text-muted)">No peers configured</td></tr>';

  // Pending file preview, flattened across peers (label each entry by peer).
  const previewRows = [];
  for (const p of peers) {
    const files = (p.pending && p.pending.files) || [];
    for (const file of files) {
      previewRows.push(
        '<tr>'
        + '<td><span class="badge badge-'+(file.action==='conflict'?'err':file.action==='delete'?'warn':'ok')+'">'+x(file.action)+'</span></td>'
        + '<td style="font-family:var(--mono);color:var(--text-dim)">'+x(file.path)+'</td>'
        + '<td>'+fmtBytes(file.size||0)+'</td>'
        + '<td style="color:var(--text-muted)">'+x(peerLabel(p))+'</td>'
        + '</tr>'
      );
    }
  }
  const previewHtml = previewRows.length
    ? '<table style="margin-top:8px;width:100%"><thead><tr><th>Action</th><th>Path</th><th>Size</th><th>Peer</th></tr></thead><tbody>'
      + previewRows.join('') + '</tbody></table>'
      + '<div style="color:var(--text-muted);font-size:11px;margin-top:4px">Preview capped at 50 entries per peer.</div>'
    : '<div style="color:var(--text-muted);margin-top:8px">No pending actions across peers.</div>';

  return '<tr><td colspan="8" style="background:var(--bg-alt);padding:12px 16px">'
    + '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:8px 24px;margin-bottom:12px">'
    +   '<div><div style="color:var(--text-muted);font-size:11px">PATH</div><div style="font-family:var(--mono)">'+x(f.path)+'</div></div>'
    +   '<div><div style="color:var(--text-muted);font-size:11px">SEQUENCE</div><div style="font-family:var(--mono)">'+(f.sequence||0)+'</div></div>'
    +   '<div><div style="color:var(--text-muted);font-size:11px">DIRECTION</div><div>'+x(f.direction)+'</div></div>'
    +   '<div><div style="color:var(--text-muted);font-size:11px">FILES &middot; DIRS &middot; SIZE</div><div>'+fmtTokens(f.file_count)+' &middot; '+fmtTokens(f.dir_count||0)+' &middot; '+fmtBytes(f.total_bytes||0)+'</div></div>'
    + '</div>'
    + '<div style="margin-bottom:12px"><div style="color:var(--text-muted);font-size:11px;margin-bottom:4px">IGNORE PATTERNS ('+ignores.length+')</div>'+ignoreHtml+'</div>'
    + '<div style="color:var(--text-muted);font-size:11px;margin-bottom:4px">PEERS &amp; SYNC PLAN</div>'
    + '<table style="width:100%"><thead><tr><th>Name</th><th>Addr</th><th>Last sync</th><th>Seen / Sent seq</th><th>Plan</th><th>Total</th></tr></thead><tbody>'
    + peerRowsHtml + '</tbody></table>'
    + '<div style="color:var(--text-muted);font-size:11px;margin-top:12px;margin-bottom:4px">PENDING FILE PREVIEW</div>'
    + previewHtml
    + '</td></tr>';
}

function renderFilesync() {
  const filter = document.getElementById('fs-search').value.toLowerCase();
  let rows = folders.filter(f => {
    if (!filter) return true;
    const peerText = (f.peers||[]).map(peerLabel).join(' ');
    return (f.id+f.path+f.direction+peerText).toLowerCase().includes(filter);
  });
  rows.sort((a,b) => {
    let va, vb;
    if (fsSort.col === 'file_count' || fsSort.col === 'dir_count' || fsSort.col === 'total_bytes') {
      va = a[fsSort.col]||0; vb = b[fsSort.col]||0; return fsSort.asc ? va-vb : vb-va;
    }
    if (fsSort.col === 'last_sync') {
      va = a.last_sync ? Date.parse(a.last_sync) : 0;
      vb = b.last_sync ? Date.parse(b.last_sync) : 0;
      return fsSort.asc ? va-vb : vb-va;
    }
    if (fsSort.col === 'peers') { va = (a.peers||[]).map(peerLabel).join(','); vb = (b.peers||[]).map(peerLabel).join(','); }
    else { va = String(a[fsSort.col]||''); vb = String(b[fsSort.col]||''); }
    return fsSort.asc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
  });
  const el = document.getElementById('fs-body');
  if (!rows.length) { el.innerHTML = '<tr><td colspan="8" style="color:var(--text-muted);padding:20px">No folders</td></tr>'; return; }
  let html = '';
  for (const f of rows) {
    const dirBadge = f.direction === 'send-receive' ? 'badge-ok' :
                     f.direction === 'disabled' ? 'badge-off' :
                     f.direction === 'dry-run' ? 'badge-warn' : 'badge-ok';
    const peers = (f.peers||[]).map(peerLabel).map(x).join(', ');
    const lastSync = fmtTime(f.last_sync);
    const expanded = expandedFolders.has(f.id);
    const arrow = expanded ? '&#9660;' : '&#9654;';
    // Plan summary badge in the main row (aggregated across peers).
    let pendDown=0, pendConf=0, pendDel=0;
    for (const p of (f.peers||[])) {
      const pp = p.pending||{};
      pendDown += pp.downloads||0;
      pendConf += pp.conflicts||0;
      pendDel  += pp.deletes||0;
    }
    const planBits = [];
    if (pendDown) planBits.push('<span style="color:var(--green)">&#8595;'+pendDown+'</span>');
    if (pendConf) planBits.push('<span style="color:var(--red)">&#9888;'+pendConf+'</span>');
    if (pendDel)  planBits.push('<span style="color:var(--yellow)">&#10007;'+pendDel+'</span>');
    const planBadge = planBits.length
      ? ' <span style="margin-left:8px;font-family:var(--mono);font-size:11px">'+planBits.join(' ')+'</span>'
      : '';
    html += '<tr style="cursor:pointer" onclick="toggleFolder(\''+xj(f.id)+'\')">'
         +  '<td style="font-weight:600">'+arrow+' '+x(f.id)+planBadge+'</td>'
         +  '<td style="color:var(--text-dim)">'+x(f.path)+'</td>'
         +  '<td><span class="badge '+dirBadge+'">'+x(f.direction)+'</span></td>'
         +  '<td>'+fmtTokens(f.file_count)+'</td>'
         +  '<td>'+fmtTokens(f.dir_count||0)+'</td>'
         +  '<td>'+fmtBytes(f.total_bytes||0)+'</td>'
         +  '<td style="color:var(--text-muted)">'+lastSync+'</td>'
         +  '<td style="color:var(--text-dim)">'+peers+'</td></tr>';
    if (expanded) html += renderFolderDetail(f);
  }
  el.innerHTML = html;
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
  // Gateway Tokens (hidden when no gateway traffic)
  const tokCard = document.getElementById('chart-tokens-card');
  if (chartHist.tokensIn.some(v => v > 0) || chartHist.tokensOut.some(v => v > 0)) {
    tokCard.style.display = '';
    document.getElementById('chart-tokin-val').textContent = last(chartHist.tokensIn).toLocaleString();
    document.getElementById('chart-tokout-val').textContent = last(chartHist.tokensOut).toLocaleString();
    drawChart('chart-tokens', [chartHist.tokensIn, chartHist.tokensOut], ['#22d3ee', '#fbbf24']);
  } else {
    tokCard.style.display = 'none';
  }
  // Component Health
  document.getElementById('chart-health-up').textContent = last(chartHist.healthUp);
  document.getElementById('chart-health-down').textContent = last(chartHist.healthDown);
  document.getElementById('chart-health-pend').textContent = last(chartHist.healthPending);
  drawChart('chart-health', [chartHist.healthUp, chartHist.healthDown, chartHist.healthPending], ['#34d399', '#f87171', '#fbbf24']);
  // Filesync Traffic (hidden when no filesync data)
  const fsTrafficCard = document.getElementById('chart-fs-traffic-card');
  if (chartHist.fsDlRate.some(v => v > 0) || chartHist.fsUlRate.some(v => v > 0)) {
    fsTrafficCard.style.display = '';
    document.getElementById('chart-fs-dl-val').textContent = fmtRate(last(chartHist.fsDlRate));
    document.getElementById('chart-fs-ul-val').textContent = fmtRate(last(chartHist.fsUlRate));
    drawChart('chart-fs-traffic', [chartHist.fsDlRate, chartHist.fsUlRate], ['#34d399', '#a78bfa']);
  } else {
    fsTrafficCard.style.display = 'none';
  }
  // Filesync Activity (hidden when no filesync data)
  const fsSyncCard = document.getElementById('chart-fs-sync-card');
  if (chartHist.fsSyncCycles.some(v => v > 0) || chartHist.fsSyncErrors.some(v => v > 0)) {
    fsSyncCard.style.display = '';
    document.getElementById('chart-fs-cycles-val').textContent = last(chartHist.fsSyncCycles);
    document.getElementById('chart-fs-errors-val').textContent = last(chartHist.fsSyncErrors);
    drawChart('chart-fs-sync', [chartHist.fsSyncCycles, chartHist.fsSyncErrors], ['#22d3ee', '#f87171']);
  } else {
    fsSyncCard.style.display = 'none';
  }
  // Per-component traffic table
  renderCompTraffic();
}

function renderCompTraffic() {
  const el = document.getElementById('met-comp-body');
  if (!el) return;
  const entries = Object.entries(compMetrics).filter(([,m]) => m.tx || m.rx || m.streams);
  if (!entries.length) { el.innerHTML = '<tr><td colspan="6" style="color:var(--text-muted);padding:16px">No traffic data</td></tr>'; return; }
  entries.sort((a,b) => ((b[1].tx||0)+(b[1].rx||0)) - ((a[1].tx||0)+(a[1].rx||0)));
  el.innerHTML = entries.map(([key, m]) => {
    const sep = key.indexOf(':');
    const typ = key.substring(0, sep), id = key.substring(sep+1);
    return '<tr><td>'+x(typ)+'</td><td>'+x(id)+'</td>'
      + '<td style="color:var(--green)">'+fmtBytes(m.tx||0)+'</td>'
      + '<td style="color:var(--purple)">'+fmtBytes(m.rx||0)+'</td>'
      + '<td>'+(m.streams||0)+'</td>'
      + '<td>'+(m.uptime ? fmtUptime(m.uptime) : '-')+'</td></tr>';
  }).join('');
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
let metParsedText = '';   // last metricsText that was parsed
let metParsedResult = []; // cached parseMetrics() result

function renderMetrics() {
  const filter = document.getElementById('met-search').value.toLowerCase();
  if (metricsText !== metParsedText) {
    metParsedText = metricsText;
    metParsedResult = parseMetrics(metricsText);
  }
  const families = metParsedResult;
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
      return '<div class="met-family' + cls + '" data-met="' + xa(f.name) + '">' +
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
    return '<div class="met-family' + cls + '" data-met="' + xa(f.name) + '">' +
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
let metSearchTimer = 0;
document.getElementById('met-search').addEventListener('input', () => {
  if (metSearchTimer) clearTimeout(metSearchTimer);
  metSearchTimer = setTimeout(() => { metSearchTimer = 0; renderMetrics(); }, 150);
});

// --- Sorting ---
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
  {method:'GET', path:'/api/filesync/folders', desc:'Filesync folder statuses as JSON array: id, path, direction, file_count, dir_count, total_bytes, last_sync, peers (array of {name, addr}).'},
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
    const fmts = (a.formats||[]).map(f => x(f)).join(', ');
    const peer = a.peer ? x(a.peer) : '<span style="color:var(--text-muted)">-</span>';
    const ago = timeAgo(a.time);
    let content;
    if (a.preview) {
      content = '<div style="color:var(--text)">'+x(a.preview)+'</div>'
              + (fmts ? '<div style="color:var(--text-muted);font-size:85%">'+fmts+'</div>' : '');
    } else {
      content = fmts ? '<span style="color:var(--text-dim)">'+fmts+'</span>'
                     : '<span style="color:var(--text-muted)">-</span>';
    }
    return '<tr><td>'+dir+'</td><td>'+fmtBytes(a.size)+'</td><td>'+content+'</td><td>'+peer+'</td><td style="color:var(--text-muted)">'+ago+'</td></tr>';
  }).join('');
}

function fmtBytes(b) {
  const n = Number(b) || 0;
  const TB = 1024**4, GB = 1024**3, MB = 1024**2, KB = 1024;
  if (n >= TB) return (n/TB).toFixed(2)+' TB';
  if (n >= GB) return (n/GB).toFixed(2)+' GB';
  if (n >= MB) return (n/MB).toFixed(1)+' MB';
  if (n >= KB) return (n/KB).toFixed(1)+' KB';
  return n+' B';
}

function timeAgo(ts) {
  if (!ts) return '-';
  const d = Date.now() - new Date(ts).getTime();
  if (d < 1000) return 'just now';
  if (d < 60000) return Math.floor(d/1000)+'s ago';
  if (d < 3600000) return Math.floor(d/60000)+'m ago';
  if (d < 86400000) return Math.floor(d/3600000)+'h ago';
  return Math.floor(d/86400000)+'d ago';
}

// --- Gateway audit ---
// gwSelectedSet and gwDetailKey are hoisted near the tab-routing block so the
// initial applyGwHash() (which may call jumpToPair) does not hit the TDZ.
let gwRowsCache = []; // resp rows joined with their req row, newest first
let gwRowsByKey = new Map(); // key "run|id" → pair, for click-to-detail across refreshes
let gwSearchTerm = '';
let gwOutcomeFilter = '';
let gwSearchTimer = 0;  // debounce handle for the search input

function renderGateway() {
  gwFresh();
  const chipBar = document.getElementById('gw-chips');
  if (!chipBar) return;
  if (!gatewayAudit || !gatewayAudit.length) {
    chipBar.innerHTML = '<span style="color:var(--text-muted);font-size:12px">No gateways with audit logging</span>';
    const meta = document.getElementById('gw-meta');
    if (meta) meta.textContent = '';
    document.getElementById('gw-body').innerHTML =
      '<tr><td colspan="13" style="color:var(--text-muted);padding:20px">No gateways with audit logging configured. Set log.level: full or metadata in the gateway YAML to populate this view.</td></tr>';
    document.getElementById('gw-kpi').innerHTML =
      '<div class="stat" style="grid-column:1/-1;color:var(--text-muted)">No gateway audit data yet. Configure log.level to populate this view.</div>';
    return;
  }
  // Render gateway chips. Empty selection = all gateways shown.
  const names = gatewayAudit.map(g => g.gateway);
  // Prune stale selections (gateway removed from config).
  for (const s of gwSelectedSet) { if (!names.includes(s)) gwSelectedSet.delete(s); }
  chipBar.innerHTML = names.map(n =>
    '<span class="gw-chip'+(gwSelectedSet.has(n) ? ' on' : '')+'" data-gw="'+xa(n)+'">'+x(n)+'</span>'
  ).join('');
  chipBar.querySelectorAll('.gw-chip').forEach(c => c.addEventListener('click', () => {
    const nm = c.dataset.gw;
    if (gwSelectedSet.has(nm)) gwSelectedSet.delete(nm); else gwSelectedSet.add(nm);
    gwStats = null; gwStale(); tick();
    writeGwHash();
  }));
  if (gwSubview === 'overview') renderGatewayOverview();
  // If a hash-driven deep restore was queued before data arrived, run it now.
  if (gwHashApplyingDeep) {
    const deep = gwHashApplyingDeep;
    gwHashApplyingDeep = '';
    if (gwSubview === 'requests') {
      const bar = deep.indexOf('|');
      if (bar > 0) jumpToPair(deep.slice(0, bar), deep.slice(bar+1));
    }
  }

  // Merge rows from selected gateways (empty set = all).
  let rowsRaw = [];
  const activeGws = gwSelectedSet.size === 0 ? gatewayAudit : gatewayAudit.filter(g => gwSelectedSet.has(g.gateway));
  for (const g of activeGws) rowsRaw.push(...(g.rows || []));
  // Pair req with resp by id+run. Each pair gets a stable composite key
  // (run|id) so the detail card survives auto-refresh even when the list
  // reorders. Haystack (hay) is computed lazily on first search so idle
  // ticks skip the JSON.stringify cost entirely.
  const reqs = new Map();
  const pairs = [];
  for (const r of rowsRaw) {
    const key = (r.run||'')+'|'+r.id;
    if (r.t === 'req') {
      reqs.set(key, r);
    } else if (r.t === 'resp') {
      const req = reqs.get(key) || {};
      pairs.push({req, resp: r, key});
    }
  }
  pairs.reverse(); // newest first
  gwRowsCache = pairs;
  gwRowsByKey = new Map(pairs.map(p => [p.key, p]));

  // Populate session chip bar from distinct sessions in the current data.
  const sessIds = [...new Set(pairs.map(p => p.req.session_id).filter(Boolean))];
  // Prune stale selections.
  for (const s of gwSessionSet) { if (!sessIds.includes(s)) gwSessionSet.delete(s); }
  const sessBar = document.getElementById('gw-sess-chips');
  if (sessBar) {
    if (sessIds.length > 1) {
      sessBar.style.display = '';
      sessBar.innerHTML = sessIds.map(sid => {
        const short = sid.slice(0, 8);
        const on = gwSessionSet.has(sid) ? ' on' : '';
        return '<span class="gw-chip'+on+'" data-sess="'+xa(sid)+'" title="'+xa(sid)+'" style="border-color:'+sessColor(sid)+';'+(gwSessionSet.has(sid)?'background:'+sessColor(sid)+';color:var(--bg)':'')+'">' +
          '<span class="sess-dot" style="display:inline-block;width:6px;height:6px;border-radius:50%;background:'+sessColor(sid)+';margin-right:4px;vertical-align:middle"></span>'+x(short)+'</span>';
      }).join('');
      sessBar.querySelectorAll('.gw-chip').forEach(c => c.addEventListener('click', () => {
        const sid = c.dataset.sess;
        if (gwSessionSet.has(sid)) gwSessionSet.delete(sid); else gwSessionSet.add(sid);
        renderGateway();
        writeGwHash();
      }));
    } else {
      sessBar.style.display = 'none';
      sessBar.innerHTML = '';
    }
  }

  const nGw = activeGws.length;
  if (nGw !== 1) {
    document.getElementById('gw-meta').innerHTML =
      (gwSelectedSet.size === 0 ? '<b>all gateways</b>' : '<b>'+nGw+' gateways</b>')+
      ' · '+pairs.length+' completed requests';
  } else {
    const entry = activeGws[0];
    document.getElementById('gw-meta').innerHTML =
      'gateway <b>'+x(entry.gateway)+'</b> · file <span style="color:var(--text-dim)">'+x(entry.file||'(none)')+'</span>'+
      (entry.file_size ? ' · '+fmtBytes(entry.file_size) : '')+
      ' · '+pairs.length+' completed requests'+
      (entry.error ? ' · <span style="color:var(--red)">error: '+x(entry.error)+'</span>' : '');
  }

  if (gwSubview === 'requests') {
    const term = (gwSearchTerm||'').toLowerCase();
    const outcomeFilter = gwOutcomeFilter;
    const filtered = pairs.filter(p => {
      if (gwSessionSet.size > 0 && !gwSessionSet.has(p.req.session_id||'')) return false;
      if (outcomeFilter && (p.resp.outcome||'') !== outcomeFilter) return false;
      if (!term) return true;
      // Lazy haystack: only JSON.stringify when the user is actually searching.
      // Cached on the pair so subsequent filter passes within the same data set
      // reuse the result.
      if (p.hay === undefined) {
        p.hay = (JSON.stringify(p.req).slice(0, 200000) + ' ' +
                 JSON.stringify(p.resp).slice(0, 200000)).toLowerCase();
      }
      return p.hay.includes(term);
    });

    const body = document.getElementById('gw-body');
    if (!filtered.length) {
      body.innerHTML = '<tr><td colspan="13" style="color:var(--text-muted);padding:20px">No rows match the current filter.</td></tr>';
    } else {
    body.innerHTML = filtered.map(p => {
      const ts = p.resp.ts||p.req.ts||'';
      const gw = p.req.gateway || p.resp.gateway || '-';
      const dir = p.req.direction || '-';
      const model = p.req.model || '-';
      const ss = (p.resp.stream_summary || {});
      const upModel = p.req.mapped_model || ss.model || '';
      const sid = p.req.session_id || '';
      const sidShort = sid ? sid.slice(0, 8) : '-';
      const sidClr = sid ? sessColor(sid) : 'var(--text-muted)';
      const stream = p.req.stream ? 'yes' : 'no';
      const status = p.resp.status || 0;
      const statusColor = status >= 400 ? 'var(--red)' : status >= 200 ? 'var(--green)' : 'var(--text-dim)';
      const outcome = p.resp.outcome || '-';
      const outcomeColor = outcome === 'ok' ? 'var(--green)' : outcome === 'error' ? 'var(--red)' : 'var(--yellow)';
      const u = p.resp.usage || {};
      const summary = renderGwSummaryCell(p.resp);
      return '<tr style="cursor:pointer" onclick="showGwDetail(\''+xj(p.key)+'\')">'+
        '<td style="color:var(--text-muted);white-space:nowrap">'+fmtLocalTime(ts)+'</td>'+
        '<td style="color:'+modelColor(gw)+'">'+x(gw)+'</td>'+
        '<td><code style="color:'+sidClr+';font-size:11px" title="'+xa(sid)+'">'+x(sidShort)+'</code></td>'+
        '<td>'+x(dir)+'</td>'+
        '<td style="color:'+modelColor(model)+'">'+x(model)+'</td>'+
        '<td style="color:'+(upModel && upModel !== model ? modelColor(upModel) : 'var(--text-muted)')+'">'+(upModel && upModel !== model ? x(upModel) : '-')+'</td>'+
        '<td style="color:var(--text-muted)">'+stream+'</td>'+
        '<td style="color:'+statusColor+'">'+status+'</td>'+
        '<td style="color:'+outcomeColor+'">'+x(outcome)+'</td>'+
        '<td>'+fmtTokensHtml(u.input_tokens)+'</td>'+
        '<td>'+fmtTokensHtml(u.output_tokens)+'</td>'+
        '<td>'+fmtElapsedHtml(p.resp.elapsed_ms)+'</td>'+
        '<td style="max-width:400px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text-dim)">'+summary+'</td>'+
        '</tr>';
    }).join('');
    }
  }
  // If a detail card is open, re-resolve it against the new data so the copy
  // buttons reflect the current req/resp and not the stale snapshot from
  // before the last refresh.
  if (gwDetailKey && gwRowsByKey.has(gwDetailKey)) {
    const p = gwRowsByKey.get(gwDetailKey);
    gwDetailCache = {req: p.req, resp: p.resp};
  }
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
  // idx is a (run|id) composite key when called from the rendered list, a
  // numeric index when called from older paths (top-requests-by-tokens in
  // the overview builds a numeric index from gwRowsCache directly). Both
  // have to resolve to the same pair even after auto-refresh reorders the
  // list — the Map lookup handles that; numeric fallback is a last resort.
  let p = null;
  if (typeof idx === 'string') p = gwRowsByKey.get(idx);
  else if (typeof idx === 'number') p = gwRowsCache[idx];
  if (!p) return;
  gwDetailKey = p.key || '';
  gwDetailCache = {req: p.req, resp: p.resp};
  if (gwSubview === 'requests') writeGwHash();
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
  // Upstream request/response panes — only present in translation mode with full logging.
  const upReq = p.resp.upstream_req;
  const upResp = p.resp.upstream_resp;
  const upReqSec = document.getElementById('gw-upstream-req-sec');
  const upRespSec = document.getElementById('gw-upstream-resp-sec');
  if (upReq && typeof upReq === 'object') {
    upReqSec.style.display = '';
    const uj = JSON.stringify(upReq);
    document.getElementById('gw-upstream-req-raw').innerHTML = highlightJSON(upReq);
    document.getElementById('gw-upstream-req-json-len').textContent = fmtLen(uj.length)+' chars';
    document.getElementById('gw-upstream-req-len').textContent = fmtLen(uj.length)+' chars';
    document.getElementById('gw-upstream-req-structured').innerHTML = renderUpstreamStructured(upReq, 'request');
  } else {
    upReqSec.style.display = 'none';
  }
  if (upResp && typeof upResp === 'object') {
    upRespSec.style.display = '';
    const uj = JSON.stringify(upResp);
    document.getElementById('gw-upstream-resp-raw').innerHTML = highlightJSON(upResp);
    document.getElementById('gw-upstream-resp-json-len').textContent = fmtLen(uj.length)+' chars';
    document.getElementById('gw-upstream-resp-len').textContent = fmtLen(uj.length)+' chars';
    document.getElementById('gw-upstream-resp-structured').innerHTML = renderUpstreamStructured(upResp, 'response');
  } else {
    upRespSec.style.display = 'none';
  }
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

// sessColor returns a stable HSL color string for a session ID. Uses the
// hash of the ID to pick a hue, keeping saturation and lightness constant
// so every session gets a unique-ish tint that contrasts against the dark bg.
const sessColorCache = {};
function sessColor(sid) {
  if (sessColorCache[sid]) return sessColorCache[sid];
  let h = 0;
  for (let i = 0; i < sid.length; i++) h = (h * 31 + sid.charCodeAt(i)) & 0xFFFF;
  const hue = h % 360;
  sessColorCache[sid] = 'hsl('+hue+',70%,65%)';
  return sessColorCache[sid];
}

// modelColor returns a stable HSL color for a model name. Same hash approach
// as sessColor but with different saturation/lightness for visual distinction.
const modelColorCache = {};
function modelColor(name) {
  if (!name) return 'var(--text-dim)';
  if (modelColorCache[name]) return modelColorCache[name];
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) & 0xFFFF;
  const hue = h % 360;
  modelColorCache[name] = 'hsl('+hue+',55%,70%)';
  return modelColorCache[name];
}

// metaRow renders a single <tr> for a metadata table. The value string is
// always escaped; label may contain trusted HTML (info icons).
function metaRow(label, value, cls, style) {
  if (value == null || value === '' || value === undefined) return '';
  return '<tr><td class="mk">'+label+'</td><td class="mv'+(cls?' '+cls:'')+'"'+(style?' style="'+style+'"':'')+ '>'+x(String(value))+'</td></tr>';
}

// metaTable wraps rows in a .meta-tbl. Empty rows are omitted automatically
// (metaRow returns '' for missing values).
function metaTable(rows) {
  const joined = rows.join('');
  if (!joined) return '';
  return '<table class="meta-tbl">'+joined+'</table>';
}

// info renders a small (?) tooltip. Uses data-help with a pure-CSS ::after
// tooltip so the hint appears instantly on hover — native title is slow and
// unreliable across browsers.
function info(text) {
  return '<span class="help" data-help="'+xa(text)+'">?</span>';
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
  const summary = resp.stream_summary || {};
  // Metadata table for response.
  const statusCls = resp.status >= 400 ? ' style="color:var(--red)"' : resp.status >= 200 ? ' style="color:var(--green)"' : '';
  const outcomeCls = resp.outcome === 'ok' ? ' style="color:var(--green)"' : resp.outcome === 'error' ? ' style="color:var(--red)"' : '';
  html += '<table class="meta-tbl">' +
    (resp.status ? '<tr><td class="mk">status</td><td class="mv"'+statusCls+'>'+x(String(resp.status))+'</td></tr>' : '') +
    (resp.outcome ? '<tr><td class="mk">outcome</td><td class="mv"'+outcomeCls+'>'+x(resp.outcome)+'</td></tr>' : '') +
    metaRow('stop_reason '+info(tokenHelp.stopReason), summary.stop_reason) +
    metaRow('elapsed', resp.elapsed_ms ? fmtElapsed(resp.elapsed_ms) : '') +
    metaRow('events', summary.events) +
    metaRow('message_id', summary.message_id) +
    metaRow('upstream_model', summary.model, '', summary.model ? 'color:'+modelColor(summary.model) : '') +
  '</table>';

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

  if (summary.thinking) {
    html += sec('Thinking', fmtLen(summary.thinking.length)+' chars',
      renderPlainText(summary.thinking), false);
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
  // Metadata table: model, upstream model, stream, session, turn, etc.
  const mapped = req.mapped_model && req.mapped_model !== (body.model||'') ? req.mapped_model : '';
  html += metaTable([
    metaRow('model', body.model, '', body.model ? 'color:'+modelColor(body.model) : ''),
    mapped ? metaRow('upstream model', mapped, '', 'color:'+modelColor(mapped)) : '',
    metaRow('stream', (req.stream || body.stream) ? 'true' : ''),
    metaRow('session '+info(tokenHelp.sessionId), req.session_id),
    metaRow('turn '+info(tokenHelp.turnIndex), req.turn_index),
    typeof body.temperature === 'number' ? metaRow('temperature', body.temperature) : '',
    metaRow('max_tokens', body.max_tokens),
    body.top_p ? metaRow('top_p', body.top_p) : '',
    metaRow('direction', req.direction),
    metaRow('path', req.path, 'dim'),
  ]);

  // System prompt (Anthropic top-level string or array of content blocks;
  // OpenAI inlines the system message in the messages array, so it appears
  // there instead).
  if (body.system) {
    const txt = typeof body.system === 'string' ? body.system : JSON.stringify(body.system);
    html += sec('System prompt', fmtLen(txt.length)+' chars',
      renderSystemPrompt(body.system), false);
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

// renderUpstreamStructured renders the translated upstream request or raw
// upstream response body. Both Anthropic and OpenAI shapes are detected and
// rendered with the same building blocks used in the client-side panes.
function renderUpstreamStructured(obj, kind) {
  if (!obj || typeof obj !== 'object') return emptyNote('No data.');
  let html = '';
  if (kind === 'request') {
    // Could be OpenAI (messages, model, tools) or Anthropic (messages, model, system, tools).
    html += metaTable([
      metaRow('model', obj.model, '', obj.model ? 'color:'+modelColor(obj.model) : ''),
      metaRow('stream', obj.stream ? 'true' : ''),
      typeof obj.temperature === 'number' ? metaRow('temperature', obj.temperature) : '',
      metaRow('max_tokens', obj.max_tokens),
      obj.top_p ? metaRow('top_p', obj.top_p) : '',
    ]);
    if (obj.system) {
      const txt = typeof obj.system === 'string' ? obj.system : JSON.stringify(obj.system);
      html += sec('System prompt', fmtLen(txt.length)+' chars',
        renderSystemPrompt(obj.system), false);
    }
    const msgs = Array.isArray(obj.messages) ? obj.messages : [];
    if (msgs.length) {
      const totalChars = msgs.reduce((n, m) => n + msgChars(m), 0);
      html += sec('Conversation ('+msgs.length+')', msgs.length+' msgs · '+fmtLen(totalChars)+' chars',
        '<div class="chat">' + msgs.map((m, i) => renderBubble(m, i)).join('') + '</div>', true);
    }
    const tools = Array.isArray(obj.tools) ? obj.tools : [];
    if (tools.length) {
      const totalToolChars = tools.reduce((n, t) => n + JSON.stringify(t).length, 0);
      html += sec('Tools ('+tools.length+')', tools.length+' tools · '+fmtLen(totalToolChars)+' chars',
        tools.map(renderToolDefinition).join(''), false);
    }
  } else {
    // Response — could be OpenAI ChatCompletionResponse or Anthropic MessagesResponse.
    html += metaTable([
      metaRow('id', obj.id),
      metaRow('model', obj.model, '', obj.model ? 'color:'+modelColor(obj.model) : ''),
      metaRow('stop_reason', obj.stop_reason || (obj.choices && obj.choices[0] && obj.choices[0].finish_reason) || ''),
    ]);
    // Anthropic response: content array.
    if (Array.isArray(obj.content)) {
      const assistantMsg = {role:'assistant', content: obj.content};
      const totalChars = msgChars(assistantMsg);
      html += sec('Content', fmtLen(totalChars)+' chars',
        '<div class="chat">'+renderBubble(assistantMsg, 0)+'</div>', true);
    }
    // OpenAI response: choices array.
    if (Array.isArray(obj.choices)) {
      obj.choices.forEach(function(c, i) {
        const m = c.message || {};
        const msg = {role: m.role || 'assistant', content: m.content, tool_calls: m.tool_calls};
        html += sec('Choice '+i, '',
          '<div class="chat">'+renderBubble(msg, 0)+'</div>', i === 0);
      });
    }
    // Usage from either shape.
    const u = obj.usage;
    if (u) {
      const uRows = [];
      // Anthropic shape.
      if (u.input_tokens) uRows.push(metaRow('input_tokens', u.input_tokens));
      if (u.output_tokens) uRows.push(metaRow('output_tokens', u.output_tokens));
      if (u.cache_read_input_tokens) uRows.push(metaRow('cache_read', u.cache_read_input_tokens));
      if (u.cache_creation_input_tokens) uRows.push(metaRow('cache_creation', u.cache_creation_input_tokens));
      // OpenAI shape.
      if (u.prompt_tokens) uRows.push(metaRow('prompt_tokens', u.prompt_tokens));
      if (u.completion_tokens) uRows.push(metaRow('completion_tokens', u.completion_tokens));
      if (u.total_tokens) uRows.push(metaRow('total_tokens', u.total_tokens));
      if (uRows.length) html += sec('Usage', '', metaTable(uRows), false);
    }
  }
  return html || emptyNote('Upstream body is empty.');
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
  // Render as a single scrollable markdown viewer with TOC sidebar.
  return renderMdViewer(v);
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
    // Anthropic-style content blocks: text blocks get the preamble split and
    // their pre/post context is aggregated into a single leading/trailing
    // drawer so a turn with 3 reminder-only text blocks does not produce 3
    // separate "Pre-context" rows. Non-text blocks (tool_result, image,
    // tool_use) pass through as-is in document order.
    typedChars = 0;
    const aggPre = [], aggPost = [];
    let sawTyped = false;
    const parts = m.content.map(b => {
      if (b && b.type === 'text' && typeof b.text === 'string') {
        const s = splitUserText(b.text);
        typedChars += s.typed.length;
        // Blocks before any typed prose fold into pre-context; blocks after
        // the last typed prose fold into post-context. If this block has no
        // typed prose at all, route everything through the side we are still
        // collecting for (pre before we have seen typed, post after).
        if (s.typed === '') {
          if (sawTyped) aggPost.push(...s.pre, ...s.post);
          else aggPre.push(...s.pre, ...s.post);
          return '';
        }
        aggPre.push(...s.pre);
        aggPost.push(...s.post);
        sawTyped = true;
        return renderPlainText(s.typed);
      }
      sawTyped = true;
      return renderContentBlock(b);
    });
    if (aggPre.length) preCtxHtml = renderContextDrawer('pre', aggPre, totalLen);
    if (aggPost.length) postCtxHtml = renderContextDrawer('post', aggPost, totalLen);
    contentHtml = parts.join('');
  } else {
    contentHtml = renderContent(m.content);
  }
  const calls = Array.isArray(m.tool_calls) ? m.tool_calls.map(renderOpenAIToolCall).join('') : '';

  const chips = [];
  if (m.tool_call_id) chips.push('<span class="role-pill" data-link-tool="'+xa(m.tool_call_id)+'" onclick="flashToolUse(\''+xj(m.tool_call_id)+'\')">tool_call_id '+x(m.tool_call_id)+'</span>');
  const dataIds = collectToolUseIds(m).map(id => 'data-tool-use="'+xa(id)+'"').join(' ');

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
    // typed must be empty here, otherwise the full original string (blocks
    // included) renders alongside the drawer and we get duplicated content.
    return {pre: parts.filter(p => p.kind === 'block'), typed: '', post: []};
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
        '<span class="ctx-name">' +
          '<span>&lt;'+x(b.name)+'&gt;</span>' +
          (preview ? '<span class="ctx-preview">'+x(preview)+'</span>' : '') +
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
        '<pre class="gw-detail-raw" style="margin-top:4px">'+highlightJSON(b.input || {})+'</pre>' +
      '</div>';
    case 'tool_result':
      // tool_result bodies are machine output (file contents, HTML source,
      // stack traces). They frequently contain angle brackets that look like
      // pseudo-XML tags (<div>, <tbody>) but are NOT Claude-Code injected
      // blocks — feeding them through splitCustomBlocks creates spurious
      // sub-drawers. Render them as plain text with Markdown coloring only.
      return '<div class="tool-block" style="border-left-color:var(--green)">' +
        '<span class="tool-name" style="color:var(--green)">tool_result</span> ' +
        '<span style="color:var(--text-muted)">tool_use_id='+x(b.tool_use_id||'')+'</span>' +
        (b.is_error ? ' <span class="chip error">error</span>' : '') +
        '<div style="margin-top:4px">'+renderToolResultContent(b.content)+'</div>' +
      '</div>';
    case 'thinking':
      return renderCustomBlock({name: 'think', body: b.thinking || ''});
    default:
      return '<div class="tool-block"><span class="tool-name" style="color:var(--text-dim)">'+x(b.type||'unknown')+'</span>' +
        '<pre>'+x(JSON.stringify(b, null, 2))+'</pre></div>';
  }
}

function renderImage(b) {
  const src = b.source || {};
  if (src.type === 'base64') {
    // Whitelist media_type so a crafted value like image/png" onerror="... cannot
    // break out of the src attribute, and so text/html;base64,... cannot turn a
    // rendered <img> into a scripted document.
    const mt = String(src.media_type||'image/png').toLowerCase();
    const ok = /^image\/(png|jpe?g|gif|webp|svg\+xml)$/.test(mt);
    const safeMt = ok ? mt : 'image/png';
    // Base64 payload: keep to the Base64 alphabet so nothing else can slip into
    // the data URI. An invalid blob just renders broken, which is the desired
    // failure mode when the upstream sent garbage.
    const b64 = String(src.data||'').replace(/[^A-Za-z0-9+/=]/g, '');
    const data = b64 ? 'data:'+safeMt+';base64,'+b64 : '';
    return data ? '<img src="'+xa(data)+'" style="max-width:200px;max-height:200px;border:1px solid var(--border);border-radius:4px"/>'
                : '<span class="json-summary">(image, base64)</span>';
  }
  if (src.type === 'url' && src.url) {
    // Only http/https survive safeUrl — javascript:, data:, vbscript:, file:
    // collapse to "#" and render as a dead link.
    const href = safeUrl(src.url);
    const display = href === '#' ? '(blocked: non-http url)' : src.url;
    return '<a href="'+xa(href)+'" target="_blank" rel="noopener noreferrer" style="color:var(--cyan)">[image: '+x(display)+']</a>';
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
  return renderMdViewer(s);
}

// renderMdViewer renders a string as a scrollable box with a TOC sidebar.
// For short strings (< 600 chars) it skips the TOC and just shows text.
// The TOC is built from Markdown headings (# / ## / ###). Each heading
// gets an anchor that scrolls the body pane on click.
// _mdScroll scrolls to a heading anchor inside the .md-body container
// without scrolling the page itself.
function _mdScroll(anchorId) {
  const el = document.getElementById(anchorId);
  if (!el) return;
  const body = el.closest('.md-body');
  if (body) body.scrollTop = el.offsetTop - body.offsetTop;
}

function renderMdViewer(s) {
  if (!s) return '';
  // Short text: no viewer chrome needed.
  if (s.length < 600) {
    return '<div class="text">'+highlightMarkdown(s)+'</div>';
  }
  const id = 'mdv-'+(_hjId++);
  // Extract headings for TOC.
  const headingRe = /^(#{1,6})\s+(.+)$/gm;
  const headings = [];
  let m;
  while ((m = headingRe.exec(s)) !== null) {
    headings.push({depth: m[1].length, title: m[2].trim(), offset: m.index});
  }
  // Build body with anchor spans at heading positions.
  let body = highlightMarkdown(s);
  // Inject anchors before <span class="md-h"> tags. highlightMarkdown wraps
  // headings as <span class="md-h"># Title</span>, so we find each such span
  // in order and prepend an anchor for the TOC to scroll to.
  let hIdx = 0;
  body = body.replace(/<span class="md-h">/g, function(match) {
    if (hIdx < headings.length) {
      return '<span id="'+id+'-h'+(hIdx++)+'"></span>' + match;
    }
    return match;
  });
  let toc = '';
  if (headings.length > 0) {
    toc = '<div class="md-toc">' +
      '<div style="font-weight:600;color:var(--text-dim);margin-bottom:4px">Contents <span class="toc-len">'+fmtLen(s.length)+' chars</span></div>' +
      headings.map(function(h, i) {
        // Compute approximate char count for this section (until next heading or end).
        const nextOff = i+1 < headings.length ? headings[i+1].offset : s.length;
        const sectionLen = nextOff - h.offset;
        const depthCls = h.depth >= 3 ? 'depth-3' : h.depth >= 2 ? 'depth-2' : '';
        const lenClr = sectionLen > 10000 ? 'var(--red)' : sectionLen > 2000 ? 'var(--yellow)' : sectionLen > 500 ? 'var(--text)' : 'var(--text-muted)';
        return '<a class="'+depthCls+'" onclick="_mdScroll(\''+id+'-h'+i+'\')" title="'+xa(h.title)+'">' +
          '<span class="toc-len" style="color:'+lenClr+'">'+fmtLen(sectionLen)+'</span> '+x(h.title)+'</a>';
      }).join('') +
    '</div>';
  }
  return '<div class="md-viewer" id="'+id+'">' +
    '<div class="md-body">'+body+'</div>' +
    toc +
  '</div>';
}

// renderToolResultContent renders a tool_result payload. Unlike renderText,
// it does NOT call splitCustomBlocks — tool output routinely contains literal
// angle brackets (HTML source, XML, compiler error messages) that must not be
// misread as Claude-Code injected pseudo-XML wrappers.
function renderToolResultContent(content) {
  if (content == null) return '';
  if (typeof content === 'string') return renderPlainText(content);
  if (Array.isArray(content)) {
    return content.map(b => {
      if (!b || typeof b !== 'object') return '';
      if (b.type === 'text') return renderPlainText(b.text || '');
      if (b.type === 'image') return renderImage(b);
      return renderContentBlock(b);
    }).join('');
  }
  return '<pre class="gw-detail-raw">'+highlightJSON(content)+'</pre>';
}

// highlightMarkdown escapes HTML then applies lightweight syntax coloring to
// common Markdown structures. It does NOT render Markdown (no semantic <h1>,
// <strong>, <em>) — the user wants plain source with color cues so token
// boundaries are visible without fighting a rendered view. Order matters: we
// extract fenced/inline code first so later rules cannot mutate code bodies.
function highlightMarkdown(s) {
  if (!s) return '';
  const holds = [];
  const hold = (html) => { const i = holds.length; holds.push(html); return '\u0001MD' + i + '\u0002'; };
  // Escape HTML once; subsequent regexes operate on escaped text.
  let out = x(s);
  // Fenced code blocks (triple backtick). \x60 escapes keep the Go raw string
  // intact — literal backticks cannot appear in the enclosing Go raw string.
  out = out.replace(/\x60\x60\x60([a-zA-Z0-9_+.-]*)\n([\s\S]*?)\x60\x60\x60/g, (m, lang, body) =>
    hold('<span class="md-fence">\x60\x60\x60' + (lang ? x(lang) : '') + '\n' + body + '\x60\x60\x60</span>'));
  // Inline code (single backtick, same line only).
  out = out.replace(/\x60([^\x60\n]+)\x60/g, (m, body) => hold('<span class="md-code">\x60' + body + '\x60</span>'));
  // Links [text](url) — url is escaped already; we only color.
  out = out.replace(/\[([^\]\n]+)\]\(([^)\n\s]+)\)/g,
    (m, text, url) => hold('<span class="md-link">[' + text + ']</span><span class="md-url">(' + url + ')</span>'));
  // Headings (line-start # .. ######, at most 6 #).
  out = out.replace(/(^|\n)(#{1,6} [^\n]*)/g, (m, lead, h) => lead + hold('<span class="md-h">' + h + '</span>'));
  // Blockquotes (line starting with "> "). Highlight the whole line.
  out = out.replace(/(^|\n)(&gt; [^\n]*)/g, (m, lead, q) => lead + hold('<span class="md-quote">' + q + '</span>'));
  // Horizontal rule (line consisting of 3+ -, *, or _).
  out = out.replace(/(^|\n)(---+|\*\*\*+|___+)(?=\n|$)/g, (m, lead, hr) => lead + hold('<span class="md-hr">' + hr + '</span>'));
  // List markers at line start: -, *, +, or digits followed by . or ).
  out = out.replace(/(^|\n)([ \t]*)([-*+]|\d+[.)]) /g,
    (m, lead, indent, marker) => lead + indent + hold('<span class="md-list">' + marker + '</span>') + ' ');
  // Bold **..** (non-greedy, same-line only so we do not swallow paragraphs).
  out = out.replace(/\*\*([^*\n]+)\*\*/g, (m, body) => hold('<span class="md-bold">**' + body + '**</span>'));
  // Italic *..* / _.._ (same-line, not adjacent to word chars to avoid false
  // positives on identifiers like foo_bar_baz).
  out = out.replace(/(^|[^*\w])\*([^*\n]+)\*(?=[^*\w]|$)/g,
    (m, pre, body) => pre + hold('<span class="md-italic">*' + body + '*</span>'));
  out = out.replace(/(^|[^_\w])_([^_\n]+)_(?=[^_\w]|$)/g,
    (m, pre, body) => pre + hold('<span class="md-italic">_' + body + '_</span>'));
  // XML/HTML tags (escaped as &lt;tagname&gt;). Color tag names and delimiters.
  // Closing tags: &lt;/tagname&gt;
  out = out.replace(/&lt;\/([a-zA-Z][a-zA-Z0-9_-]*)&gt;/g,
    (m, tag) => hold('<span class="md-xml">&lt;/<span class="md-xml-tag">' + tag + '</span>&gt;</span>'));
  // Self-closing: &lt;tagname/&gt; or &lt;tagname /&gt;
  out = out.replace(/&lt;([a-zA-Z][a-zA-Z0-9_-]*)\s*\/&gt;/g,
    (m, tag) => hold('<span class="md-xml">&lt;<span class="md-xml-tag">' + tag + '</span>/&gt;</span>'));
  // Opening tags: &lt;tagname&gt; or &lt;tagname attr...&gt;
  out = out.replace(/&lt;([a-zA-Z][a-zA-Z0-9_-]*)(\s[^&]*?)?&gt;/g,
    (m, tag, attrs) => hold('<span class="md-xml">&lt;<span class="md-xml-tag">' + tag + '</span>' + (attrs||'') + '&gt;</span>'));
  // Restore placeholders.
  return out.replace(/\u0001MD(\d+)\u0002/g, (m, i) => holds[+i]);
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
  // Lowercase the whole input ONCE. The previous implementation did
  // s.slice(openEnd).toLowerCase() inside the loop, which allocated and
  // scanned a fresh copy of the remaining string per match — O(n²) on inputs
  // with many open tags but no close tags (5 MB × 50k tags ≈ 100 GB of work).
  const lower = s.toLowerCase();
  let m;
  while ((m = customTagOpenRe.exec(s)) !== null) {
    const openStart = m.index;
    const openEnd   = openStart + m[0].length;
    const name      = m[1].toLowerCase();
    const closeTag  = '</' + name + '>';
    const closeAbs  = lower.indexOf(closeTag, openEnd);
    if (closeAbs < 0) continue;
    if (openStart > i) out.push({kind: 'text', text: s.slice(i, openStart)});
    out.push({kind: 'block', name, body: s.slice(openEnd, closeAbs)});
    i = closeAbs + closeTag.length;
    customTagOpenRe.lastIndex = i;
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
  else if (name === 'think' || name === 'thinking') { kind = 'thinking'; icon = '◆'; }
  const id = 'cb-'+(_hjId++);
  const isLong = body.length > truncateLen;
  const collapseInitial = (name === 'system-reminder' && isLong) || kind === 'thinking';
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

document.getElementById('gw-search').addEventListener('input', e => {
  gwSearchTerm = e.target.value;
  if (gwSearchTimer) clearTimeout(gwSearchTimer);
  gwSearchTimer = setTimeout(() => { gwSearchTimer = 0; renderGateway(); }, 150);
});
document.getElementById('gw-outcome').addEventListener('change', e => { gwOutcomeFilter = e.target.value; renderGateway(); });
document.getElementById('gw-window').addEventListener('change', e => { gwWindow = e.target.value; gwStats = null; gwStale(); tick(); writeGwHash(); });
document.querySelectorAll('.gw-sub-btn').forEach(b => b.addEventListener('click', () => {
  setGwSub(b.dataset.sub);
  // User changed sub-view; drop any previous deep state from the URL.
  gwDetailKey = '';
  writeGwHash();
}));

// gwStale adds a loading overlay to gateway sub-views so the user sees
// that a refresh is in progress after changing filters.
function gwStale() {
  ['gw-sub-overview','gw-sub-requests'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.classList.add('gw-loading');
  });
}
function gwFresh() {
  ['gw-sub-overview','gw-sub-requests'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.classList.remove('gw-loading');
  });
}

// setGwSub switches the gateway sub-view (overview | requests).
// Caller is responsible for writeGwHash() afterwards when the state change
// should be reflected in the URL.
function setGwSub(sub) {
  const valid = sub === 'overview' || sub === 'requests';
  if (!valid) sub = 'overview';
  gwSubview = sub;
  document.querySelectorAll('.gw-sub-btn').forEach(x => x.classList.toggle('active', x.dataset.sub === sub));
  document.getElementById('gw-sub-overview').style.display = sub === 'overview' ? '' : 'none';
  document.getElementById('gw-sub-requests').style.display = sub === 'requests' ? '' : 'none';
}

// writeGwHash syncs the URL hash to the current gateway filter + sub-view +
// detail state, so refresh/back/forward restore the same view.
// Format: #sub?gw=a,b&window=24h&detail=run|id
function writeGwHash() {
  if (activeTab !== 'gateway') return;
  const parts = [gwSubview || 'overview'];
  const params = [];
  if (gwSelectedSet.size > 0) params.push('gw='+[...gwSelectedSet].map(encodeURIComponent).join(','));
  if (gwSessionSet.size > 0) params.push('sess='+[...gwSessionSet].map(encodeURIComponent).join(','));
  if (gwWindow && gwWindow !== '24h') params.push('window='+encodeURIComponent(gwWindow));
  if (gwSubview === 'requests' && gwDetailKey) params.push('detail='+encodeURIComponent(gwDetailKey));
  let h = '#' + parts[0] + (params.length ? '?' + params.join('&') : '');
  if (location.hash === h) return;
  const full = location.pathname + h;
  const prevDetail = gwHashLast.includes('detail=');
  const nextDetail = h.includes('detail=');
  if (nextDetail && !prevDetail) history.pushState(null, '', full);
  else history.replaceState(null, '', full);
  gwHashLast = h;
  gwHashApplyingDeep = '';
}

function parseGwHash() {
  const h = (location.hash || '').replace(/^#/, '');
  if (!h) return {sub: 'overview', gw: [], sess: [], window: '', detail: ''};
  const qIdx = h.indexOf('?');
  const sub = qIdx < 0 ? h : h.slice(0, qIdx);
  const qs = qIdx < 0 ? '' : h.slice(qIdx + 1);
  const p = {};
  qs.split('&').forEach(kv => { const [k,v] = kv.split('='); if (k) p[k] = decodeURIComponent(v||''); });
  return {
    sub: sub || 'overview',
    gw: p.gw ? p.gw.split(',').map(decodeURIComponent) : [],
    sess: p.sess ? p.sess.split(',').map(decodeURIComponent) : [],
    window: p.window || '',
    detail: p.detail || '',
  };
}

function applyGwHash() {
  if (activeTab !== 'gateway') return;
  const parsed = parseGwHash();
  setGwSub(parsed.sub);
  gwHashLast = location.hash;
  // Restore gateway and session selections from URL.
  if (parsed.gw.length) gwSelectedSet = new Set(parsed.gw);
  if (parsed.sess.length) gwSessionSet = new Set(parsed.sess);
  // Restore time window.
  if (parsed.window) {
    gwWindow = parsed.window;
    const wSel = document.getElementById('gw-window');
    if (wSel) wSel.value = gwWindow;
  }
  if (!parsed.detail) {
    if (parsed.sub !== 'requests') gwDetailKey = '';
    return;
  }
  gwHashApplyingDeep = parsed.detail;
  if (parsed.sub === 'requests') {
    const bar = parsed.detail.indexOf('|');
    if (bar > 0) {
      jumpToPair(parsed.detail.slice(0, bar), parsed.detail.slice(bar+1));
    }
  }
}

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
    // Stats require a single gateway. Show a helpful notice when multiple
    // gateways are selected or when stats are still loading.
    const multi = gwSelectedSet.size !== 1 && !(gwSelectedSet.size === 0 && gatewayAudit && gatewayAudit.length === 1);
    const msg = multi ? 'Select a single gateway for overview stats.' : 'Loading stats\u2026';
    kpi.innerHTML = '<div class="stat" style="grid-column:1/-1;color:var(--text-muted)">'+msg+'</div>';
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
    statBox('Requests', fmtTokens(t.requests), gwStats.window) +
    statBox('Errors', fmtTokens(t.errors)+' ('+errPct+')', '', t.errors > 0 ? 'var(--red)' : '') +
    statBox('Input tokens'+info('Sum of fresh + cache_read + cache_write input tokens.'), totalIn.toLocaleString(), '(incl. cache)') +
    statBox('Output tokens'+info(tokenHelp.output), fmtTokens(t.output_tokens), '') +
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
    : sessions.map(s => '<tr>' +
        '<td><code style="color:'+sessColor(s.key)+'">'+x(s.key)+'</code></td>' +
        '<td style="color:'+modelColor(s.first_model)+'">'+x(s.first_model||'-')+'</td>' +
        '<td>'+(s.turns||s.requests)+'</td>' +
        '<td>'+fmtTokens(s.input_tokens)+' / '+fmtTokens(s.output_tokens)+'</td>' +
        '<td style="color:var(--text-muted)">'+x(fmtAgo(s.last_seen||''))+'</td>' +
      '</tr>').join('');

  const models = (gwStats.by_model || []).slice(0, 10);
  document.getElementById('gw-top-models').innerHTML = models.length === 0
    ? '<tr><td colspan="4" style="color:var(--text-muted);padding:12px">No models in window.</td></tr>'
    : models.map(m => '<tr>' +
        '<td style="color:'+modelColor(m.key)+'">'+x(m.key||'-')+'</td>' +
        '<td>'+m.requests+'</td>' +
        '<td>'+fmtTokens(m.input_tokens)+' / '+fmtTokens(m.output_tokens)+'</td>' +
        '<td>'+fmtTokens(m.cache_read_tokens)+'</td>' +
      '</tr>').join('');

  // By-project: local project path (last two segments) extracted from the
  // system prompt. Falls back to URL path for non-Claude-Code clients.
  const paths = (gwStats.by_path || []).slice(0, 10);
  document.getElementById('gw-by-path').innerHTML = paths.length === 0
    ? '<tr><td colspan="3" style="color:var(--text-muted);padding:12px">No projects in window.</td></tr>'
    : paths.map(p => {
        const k = p.key || '-';
        const isUrl = k.startsWith('/');
        const style = isUrl ? 'color:var(--text-muted);font-style:italic' : 'color:var(--cyan)';
        return '<tr>' +
          '<td><code style="'+style+'">'+x(k)+'</code></td>' +
          '<td>'+p.requests+'</td>' +
          '<td>'+fmtTokens(p.input_tokens)+' / '+fmtTokens(p.output_tokens)+'</td>' +
        '</tr>';
      }).join('');

  // By-hour-of-day: 24-bucket bar chart (only populated hours). Tells the
  // user whether usage clusters at specific times (e.g. morning pair-programming).
  document.getElementById('gw-by-hour').innerHTML = renderHourChart(gwStats.by_hour || []);

  // Biggest single requests: jump-to-pair from the observability view.
  const topReqs = gwStats.top_requests || [];
  document.getElementById('gw-top-requests').innerHTML = topReqs.length === 0
    ? '<tr><td colspan="6" style="color:var(--text-muted);padding:12px">No requests in window.</td></tr>'
    : topReqs.map(r => '<tr style="cursor:pointer" onclick="jumpToPair(\''+xj(r.run)+'\','+(Number(r.id)||0)+')">' +
        '<td style="color:var(--text-muted);white-space:nowrap">'+x(fmtAgo(r.ts))+'</td>' +
        '<td><code style="color:'+(r.session ? sessColor(r.session) : 'var(--text-muted)')+'">'+x(r.session||'-')+'</code></td>' +
        '<td style="color:var(--text-dim)">'+x(r.model||'-')+'</td>' +
        '<td style="color:var(--text-muted)">'+x(r.path||'-')+'</td>' +
        '<td style="font-weight:600">'+fmtTokens(r.total_tokens)+'</td>' +
        '<td>'+fmtTokens(r.input_tokens)+' / '+fmtTokens(r.output_tokens)+'</td>' +
      '</tr>').join('');

  // Biggest preamble blocks: the "what injected block is costing me the
  // most" table. key is already "<tag> first-60-chars".
  const pre = (gwStats.preamble_blocks || []).slice(0, 15);
  document.getElementById('gw-preamble-blocks').innerHTML = pre.length === 0
    ? '<tr><td colspan="3" style="color:var(--text-muted);padding:12px">No preamble blocks detected in user messages.</td></tr>'
    : pre.map(p => '<tr>' +
        '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim);max-width:600px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="'+xa(p.key)+'">'+x(p.key)+'</td>' +
        '<td>'+p.requests+'</td>' +
        '<td>'+fmtTokens(p.input_tokens)+'</td>' +
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
  // Determine which gateway to query. Prefer the single selected chip;
  // fall back to the only available gateway.
  const pairGw = gwSelectedSet.size === 1 ? [...gwSelectedSet][0]
    : (gatewayAudit && gatewayAudit.length === 1) ? gatewayAudit[0].gateway : '';
  if (!pairGw) return;
  setGwSub('requests');
  // Try to resolve from the existing data first (avoids a round-trip and
  // the 400/404 errors that occur when the pair endpoint cannot find it).
  const key = (run||'')+'|'+id;
  if (gwRowsByKey.has(key)) {
    showGwDetail(key);
    return;
  }
  fetch('/api/gateway/audit/pair?gateway='+encodeURIComponent(pairGw)+
        '&run='+encodeURIComponent(run)+'&id='+encodeURIComponent(id))
    .then(r => r.ok ? r.json() : null)
    .then(pair => {
      if (!pair) return;
      const p = {req: pair.request, resp: pair.response, key, hay: ''};
      gwRowsCache = [p];
      gwRowsByKey.set(key, p);
      showGwDetail(key);
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

// gwWindowSince returns an ISO timestamp for the start of the current
// time window, or '' for 'all'. Passed as the "since" query param so
// the server doesn't scan beyond the visible window.
function gwWindowSince() {
  const ms = {
    '1h': 3600000, '24h': 86400000, '7d': 604800000, '30d': 2592000000,
  }[gwWindow];
  if (!ms) return '';
  return new Date(Date.now() - ms).toISOString();
}

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

// --- Keyboard shortcuts ---
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    const gw = document.getElementById('gw-detail-card');
    if (gw && gw.style.display === 'block') { gw.style.display = 'none'; gwDetailKey = ''; writeGwHash(); return; }
    const dbg = document.getElementById('dbg-result-card');
    if (dbg && dbg.style.display === 'block') { dbg.style.display = 'none'; return; }
  }
});

// --- Start ---
tick();
setInterval(tick, 1000);
</script>
</body>
</html>`
