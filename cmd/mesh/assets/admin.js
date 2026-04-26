// --- State ---
let state = {}, logs = [], folders = [], conflicts = [], clipActivities = [], fsActivities = [], metricsText = '', gatewayAudit = [], gwStats = null, gwSubview = 'overview', gwWindow = '24h', fsMetrics = {}, perfEvents = [];
function gwBucket(w) { return w === '1h' ? 'minute' : w === '24h' ? 'hour' : w === '7d' ? 'hour' : 'day'; }
let logLevel = 'all';
let logMode = 'recent'; // 'recent' (ring buffer) or 'file' (full log file)
let fileLogLines = [], fileLogSize = 0, fileLogLoaded = false;
const HIST_LEN = 60;
const chartHist = {tx:[], rx:[], streams:[], goroutines:[], fds:[], tokensIn:[], tokensOut:[], healthUp:[], healthDown:[], healthPending:[], fsDlRate:[], fsUlRate:[], fsSyncErrors:[], fsSyncCycles:[]};
let prevTotalTx = 0, prevTotalRx = 0, prevTotalTokIn = 0, prevTotalTokOut = 0, firstTick = true;
let prevFsDl = 0, prevFsUl = 0, prevFsErrors = 0, prevFsCycles = 0;
let compMetrics = {};
let lastSuccessTime = 0;

// --- Tabs ---
const tabMap = {'/ui':'dashboard','/ui/clipsync':'clipsync','/ui/filesync':'filesync','/ui/gateway':'gateway','/ui/logs':'logs','/ui/metrics':'metrics','/ui/api':'api','/ui/debug':'debug'};
let activeTab = tabMap[location.pathname] || 'dashboard';
// Gateway hash routing state; declared up here so applyGwHash() (called from
// showTab on initial load) does not hit the TDZ.
let gwHashLast = '';
let gwHashApplyingDeep = '';
let gwSelectedSet = new Set();  // empty = all gateways shown
let gwSessionSet = new Set();   // empty = all sessions shown
let gwProjectSet = new Set();   // empty = all projects shown
let gwDetailKey = '';

// extractProject pulls the project path from a request body's system prompt.
// Mirrors the server-side extractProjectPath: looks for "Primary working
// directory:" and returns the last two path segments.
function extractProject(req) {
  if (req._project !== undefined) return req._project;
  req._project = '';
  const body = req.body;
  if (!body || typeof body !== 'object') return '';
  // Check system field (Anthropic format).
  const candidates = [];
  if (body.system) {
    if (typeof body.system === 'string') candidates.push(body.system);
    else if (Array.isArray(body.system)) {
      for (const b of body.system) if (b.type === 'text' && b.text) candidates.push(b.text);
    }
  }
  // Check system-role messages (OpenAI format).
  if (Array.isArray(body.messages)) {
    for (const m of body.messages) {
      if (m.role === 'system' && typeof m.content === 'string') candidates.push(m.content);
    }
  }
  const marker = 'Primary working directory: ';
  for (const text of candidates) {
    const idx = text.indexOf(marker);
    if (idx < 0) continue;
    let line = text.slice(idx + marker.length).split('\n')[0].trim();
    if (!line) continue;
    line = line.replace(/\\/g, '/').replace(/\/+$/, '');
    const parts = line.split('/').filter(Boolean);
    if (parts.length >= 2) { req._project = parts[parts.length-2]+'/'+parts[parts.length-1]; return req._project; }
    if (parts.length === 1) { req._project = parts[0]; return req._project; }
  }
  return '';
}

function showTab(name, opts) {
  opts = opts || {};
  const changed = activeTab !== name;
  activeTab = name;
  document.querySelectorAll('.tab[role="tab"]').forEach(t => {
    const sel = t.dataset.tab === name;
    t.classList.toggle('active', sel);
    t.setAttribute('aria-selected', sel ? 'true' : 'false');
    t.tabIndex = sel ? 0 : -1;
  });
  document.querySelectorAll('.panel').forEach(p => {
    const active = p.id === 'p-'+name;
    p.classList.toggle('active', active);
    p.setAttribute('aria-hidden', active ? 'false' : 'true');
  });
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
  if (e.target.classList.contains('tab') && e.target.dataset.tab) showTab(e.target.dataset.tab, {clearHash: true});
});
// Arrow-key navigation within the tab bar (WAI-ARIA tabs pattern).
document.getElementById('tabs').addEventListener('keydown', e => {
  if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
  const tabs = [...document.querySelectorAll('.tab[role="tab"]')];
  const cur = tabs.findIndex(t => t.dataset.tab === activeTab);
  if (cur < 0) return;
  const next = e.key === 'ArrowRight' ? (cur + 1) % tabs.length : (cur - 1 + tabs.length) % tabs.length;
  showTab(tabs[next].dataset.tab, {clearHash: true});
  tabs[next].focus();
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

  const hdr = document.getElementById('hdr-status');
  const mark = () => { lastSuccessTime = Date.now(); hdr.textContent = 'updated ' + new Date().toLocaleTimeString(); hdr.style.color = ''; };
  const fail = (what) => (e) => {
    const stale = lastSuccessTime ? Math.round((Date.now() - lastSuccessTime) / 1000) : 0;
    const suffix = stale > 5 ? ' (stale '+stale+'s)' : '';
    hdr.textContent = 'error('+what+'): ' + (e.message||e) + suffix;
    hdr.style.color = stale > 10 ? 'var(--red)' : 'var(--yellow)';
  };
  const ok = (r) => { if (!r.ok) throw 'HTTP ' + r.status; return r; };

  fetch('/api/state').then(r=>ok(r)).then(r=>r.json()).then(s => {
    state = s; renderStats(); if (activeTab === 'dashboard') renderComponents(); mark();
  }).catch(fail('state'));

  fetch('/api/metrics').then(r=>ok(r)).then(r=>r.text()).then(t => {
    metricsText = t;
    compMetrics = extractCompMetrics(t);
    fsMetrics = extractFsMetrics(t);
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
    if (activeTab === 'metrics') { renderCharts(); renderMetrics(); }
    if (activeTab === 'dashboard') renderComponents();
    if (activeTab === 'debug') renderDebugStats();
    mark();
  }).catch(fail('metrics'));

  if (needLogs) {
    fetch('/api/logs').then(r=>ok(r)).then(r=>r.json()).then(v => { logs = v; renderDashLogs(); renderLogs(); }).catch(fail('logs'));
  }
  if (needFilesync) {
    fetch('/api/filesync/folders').then(r=>ok(r)).then(r=>r.json()).then(v => { folders = v; renderFilesync(); }).catch(fail('folders'));
    fetch('/api/filesync/conflicts').then(r=>ok(r)).then(r=>r.json()).then(v => {
      const cur = new Set(v.map(c => c.folder_id+'|'+c.path));
      for (const k of Object.keys(conflictDiffCache)) { if (!cur.has(k)) delete conflictDiffCache[k]; }
      for (const k of expandedConflicts) { if (!cur.has(k)) expandedConflicts.delete(k); }
      conflicts = v; renderConflicts();
    }).catch(fail('conflicts'));
    fetch('/api/filesync/activity').then(r=>ok(r)).then(r=>r.json()).then(v => { fsActivities = v; renderFsActivity(); }).catch(fail('fs-activity'));
  }
  if (needClipsync) {
    fetch('/api/clipsync/activity').then(r=>ok(r)).then(r=>r.json()).then(v => { clipActivities = v; renderClipsync(); }).catch(fail('clipsync'));
  }
  if (needGateway) {
    fetch('/api/gateway/audit?limit=200').then(r=>ok(r)).then(r=>r.json()).then(v => { gatewayAudit = v; renderGateway(); }).catch(fail('gateway'));
    // Stats: fetch for each active gateway and merge client-side.
    // When no chips are selected, fetch all gateways.
    const statsGws = gwSelectedSet.size > 0 ? [...gwSelectedSet]
      : (gatewayAudit ? gatewayAudit.map(g => g.gateway) : []);
    if (statsGws.length > 0) {
      Promise.all(statsGws.map(gw =>
        fetch('/api/gateway/audit/stats?gateway='+encodeURIComponent(gw)+
          '&window='+encodeURIComponent(gwWindow)+'&bucket='+gwBucket(gwWindow))
          .then(r => ok(r)).then(r => r.json())
      )).then(results => {
        gwStats = results.length === 1 ? results[0] : mergeGwStats(results);
        if (gwSubview === 'overview') renderGatewayOverview();
      }).catch(fail('gw-stats'));
    }
  }
  if (activeTab === 'perf') {
    fetch('/api/perf?limit=2000').then(r=>ok(r)).then(r=>r.json()).then(v => { perfEvents = v; renderPerf(); }).catch(fail('perf'));
  }
  // B4 active-request indicator: chrome on every tab, polled at the
  // existing 1s tick.
  fetch('/api/gateway/active').then(r=>ok(r)).then(r=>r.json()).then(v => {
    activeState = v;
    renderActiveIndicator();
    if (activePanelOpen) renderActivePanel();
  }).catch(fail('active'));
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
// setHTML: skip innerHTML assignment when content is unchanged. Avoids DOM
// thrashing, preserves selection/scroll state, and reduces GC pressure on
// the 1s polling interval.
const _prevHTML = new WeakMap();
function setHTML(el, html) {
  if (typeof el === 'string') el = document.getElementById(el);
  if (!el) return;
  const prev = _prevHTML.get(el);
  if (prev === html) return;
  _prevHTML.set(el, html);
  el.innerHTML = html;
}
// --- TF: Table Filter/Sort System ---
// Generic Excel-like column filtering + sorting. Each table registers with
// TF.register(id, {columns, renderRow, onUpdate, tbody, emptyText}).
// Columns: [{key, label, type:'text'|'number'|'date', extract:(row)=>displayVal}]
// TF handles: multi-select per-column filters, sort asc/desc, dropdown UI.
const TF = (() => {
  const tables = {};    // id → config
  const fstate = {};    // id → {sort:{col,asc}, filters:{col→Set}}
  let openDD = null;    // currently open dropdown {tableId, colKey, el}

  function reg(id, cfg) {
    tables[id] = cfg;
    if (!fstate[id]) fstate[id] = {sort:{col:'', asc:true}, filters:{}};
  }

  function getState(id) {
    if (!fstate[id]) fstate[id] = {sort:{col:'', asc:true}, filters:{}};
    return fstate[id];
  }

  // Apply filters + sort to rows. Returns filtered+sorted array.
  function apply(id, rows) {
    const cfg = tables[id];
    if (!cfg) return rows;
    const st = getState(id);
    let out = rows;
    // Column filters
    for (const col of cfg.columns) {
      const sel = st.filters[col.key];
      if (!sel || sel.size === 0) continue;
      out = out.filter(r => {
        const v = String(col.extract ? col.extract(r) : (r[col.key]||''));
        return sel.has(v);
      });
    }
    // Sort
    if (st.sort.col) {
      const col = cfg.columns.find(c => c.key === st.sort.col);
      if (col) {
        const asc = st.sort.asc;
        const tp = col.type || 'text';
        out = [...out].sort((a, b) => {
          let va = col.extract ? col.extract(a) : (a[col.key]||'');
          let vb = col.extract ? col.extract(b) : (b[col.key]||'');
          if (tp === 'number') { va = Number(va)||0; vb = Number(vb)||0; return asc ? va-vb : vb-va; }
          if (tp === 'date') { va = va ? new Date(va).getTime() : 0; vb = vb ? new Date(vb).getTime() : 0; return asc ? va-vb : vb-va; }
          return asc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
        });
      }
    }
    return out;
  }

  // Build the <thead> HTML with filter-enabled headers.
  function thead(id) {
    const cfg = tables[id];
    if (!cfg) return '';
    const st = getState(id);
    return '<tr>' + cfg.columns.map(col => {
      const sorted = st.sort.col === col.key;
      const filtered = st.filters[col.key] && st.filters[col.key].size > 0;
      const arrow = sorted ? (st.sort.asc ? '&#9650;' : '&#9660;') : '';
      const cls = 'tf-th' + (filtered ? ' tf-filtered' : '') + (sorted ? ' tf-sorted' : '');
      return '<th class="'+cls+'" data-tf="'+col.key+'" data-table="'+id+'">' +
        col.label +
        '<span class="sort-arrow">'+arrow+'</span>' +
        '<span class="tf-icon">'+(filtered ? '&#9673;' : '&#9662;')+'</span>' +
      '</th>';
    }).join('') + '</tr>';
  }

  // Render active filter tags bar.
  function filterBar(id) {
    const cfg = tables[id];
    if (!cfg) return '';
    const st = getState(id);
    const tags = [];
    for (const col of cfg.columns) {
      const sel = st.filters[col.key];
      if (!sel || sel.size === 0) continue;
      const vals = [...sel];
      const label = vals.length <= 2 ? vals.map(v => x(v)).join(', ') : vals.length + ' selected';
      tags.push('<span class="tf-filter-tag" onclick="TF.clearCol(\''+col.key+'\',\''+id+'\')" title="'+vals.map(v=>xa(v)).join(', ')+'">' +
        x(col.label) + ': ' + label + ' <span class="tf-tag-x">&times;</span></span>');
    }
    if (!tags.length) return '';
    return tags.join('') +
      '<button class="tf-clear-all" onclick="TF.clearAll(\''+id+'\')">Clear all</button>';
  }

  // Count unique values for a column across the UNFILTERED data (so the user
  // can see what options exist even when other columns have active filters).
  function uniqueVals(id, colKey, allRows) {
    const cfg = tables[id];
    const col = cfg.columns.find(c => c.key === colKey);
    if (!col) return [];
    const counts = {};
    for (const r of allRows) {
      const v = String(col.extract ? col.extract(r) : (r[col.key]||''));
      counts[v] = (counts[v]||0) + 1;
    }
    // Sort by count desc, then alpha.
    return Object.entries(counts)
      .sort((a,b) => b[1]-a[1] || a[0].localeCompare(b[0]))
      .map(([val, cnt]) => ({val, cnt}));
  }

  // Open dropdown for a column.
  function openDropdown(tableId, colKey, thEl) {
    closeDropdown();
    const cfg = tables[tableId];
    const col = cfg.columns.find(c => c.key === colKey);
    if (!col) return;
    const st = getState(tableId);
    const sel = st.filters[colKey] || new Set();
    const allRows = cfg.allRows || [];
    const vals = uniqueVals(tableId, colKey, allRows);

    const dd = document.createElement('div');
    dd.className = 'tf-dd open';
    dd.onclick = e => e.stopPropagation();

    // Sort buttons
    const sortDiv = document.createElement('div');
    sortDiv.className = 'tf-dd-sort';
    const btnAsc = document.createElement('button');
    btnAsc.textContent = '↑ Sort A→Z';
    btnAsc.className = st.sort.col === colKey && st.sort.asc ? 'active' : '';
    btnAsc.onclick = () => { st.sort = {col: colKey, asc: true}; closeDropdown(); cfg.onUpdate(); };
    const btnDesc = document.createElement('button');
    btnDesc.textContent = '↓ Sort Z→A';
    btnDesc.className = st.sort.col === colKey && !st.sort.asc ? 'active' : '';
    btnDesc.onclick = () => { st.sort = {col: colKey, asc: false}; closeDropdown(); cfg.onUpdate(); };
    const btnClearSort = document.createElement('button');
    btnClearSort.textContent = '× Clear';
    btnClearSort.onclick = () => { if (st.sort.col === colKey) st.sort = {col:'',asc:true}; closeDropdown(); cfg.onUpdate(); };
    sortDiv.append(btnAsc, btnDesc, btnClearSort);
    dd.append(sortDiv);

    // Search within values
    const searchDiv = document.createElement('div');
    searchDiv.className = 'tf-dd-search';
    const searchInput = document.createElement('input');
    searchInput.placeholder = 'Search...';
    searchInput.oninput = () => renderItems(searchInput.value.toLowerCase());
    searchDiv.append(searchInput);
    dd.append(searchDiv);

    // Select all / none
    const actDiv = document.createElement('div');
    actDiv.className = 'tf-dd-actions';
    const btnAll = document.createElement('button');
    btnAll.textContent = 'Select all';
    btnAll.onclick = () => { st.filters[colKey] = new Set(); closeDropdown(); cfg.onUpdate(); };
    const btnNone = document.createElement('button');
    btnNone.textContent = 'Select none';
    btnNone.onclick = () => {
      const visible = [...listDiv.querySelectorAll('.tf-dd-item:not([style*="display: none"])')];
      const visibleVals = visible.map(el => el.dataset.val);
      // Invert: select only those NOT visible (effectively hide all visible)
      const allVals = vals.map(v => v.val);
      const keep = allVals.filter(v => !visibleVals.includes(v));
      st.filters[colKey] = keep.length ? new Set(keep) : new Set(['__tf_none__']);
      closeDropdown(); cfg.onUpdate();
    };
    const btnOnly = document.createElement('button');
    btnOnly.textContent = 'Only visible';
    btnOnly.onclick = () => {
      const visible = [...listDiv.querySelectorAll('.tf-dd-item:not([style*="display: none"])')];
      st.filters[colKey] = new Set(visible.map(el => el.dataset.val));
      closeDropdown(); cfg.onUpdate();
    };
    actDiv.append(btnAll, btnNone, btnOnly);
    dd.append(actDiv);

    // Value list with checkboxes
    const listDiv = document.createElement('div');
    listDiv.className = 'tf-dd-list';

    function renderItems(search) {
      listDiv.querySelectorAll('.tf-dd-item').forEach(el => {
        el.style.display = !search || el.dataset.val.toLowerCase().includes(search) ? '' : 'none';
      });
    }

    for (const {val, cnt} of vals) {
      const item = document.createElement('label');
      item.className = 'tf-dd-item';
      item.dataset.val = val;
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = sel.size === 0 || sel.has(val);
      cb.onchange = () => {
        // Rebuild the selection set from all checkboxes.
        const cbs = listDiv.querySelectorAll('input[type="checkbox"]');
        const checked = [...cbs].filter(c => c.checked).map(c => c.closest('.tf-dd-item').dataset.val);
        if (checked.length === vals.length || checked.length === 0) {
          delete st.filters[colKey];
        } else {
          st.filters[colKey] = new Set(checked);
        }
        cfg.onUpdate();
        // Update header without closing dropdown
        const th = document.querySelector('th[data-tf="'+colKey+'"][data-table="'+tableId+'"]');
        if (th) {
          const filtered = st.filters[colKey] && st.filters[colKey].size > 0;
          th.classList.toggle('tf-filtered', filtered);
          const icon = th.querySelector('.tf-icon');
          if (icon) icon.innerHTML = filtered ? '&#9673;' : '&#9662;';
        }
      };
      const valSpan = document.createElement('span');
      valSpan.className = 'tf-val';
      valSpan.textContent = val || '(empty)';
      valSpan.title = val;
      const cntSpan = document.createElement('span');
      cntSpan.className = 'tf-cnt';
      cntSpan.textContent = cnt;
      item.append(cb, valSpan, cntSpan);
      listDiv.append(item);
    }
    dd.append(listDiv);

    // Footer with count
    const footer = document.createElement('div');
    footer.className = 'tf-dd-footer';
    footer.textContent = vals.length + ' unique values';
    dd.append(footer);

    thEl.append(dd);
    openDD = {tableId, colKey, el: dd};
    setTimeout(() => searchInput.focus(), 0);
  }

  function closeDropdown() {
    if (openDD) { openDD.el.remove(); openDD = null; }
  }

  // Click on <th> to open dropdown.
  document.addEventListener('click', e => {
    const th = e.target.closest('th.tf-th');
    if (th && th.dataset.tf && th.dataset.table) {
      e.stopPropagation();
      if (openDD && openDD.tableId === th.dataset.table && openDD.colKey === th.dataset.tf) {
        closeDropdown();
      } else {
        openDropdown(th.dataset.table, th.dataset.tf, th);
      }
      return;
    }
    closeDropdown();
  });

  // Escape closes dropdown.
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && openDD) { closeDropdown(); e.stopPropagation(); } }, true);

  function clearCol(colKey, tableId) {
    const st = getState(tableId);
    delete st.filters[colKey];
    tables[tableId].onUpdate();
  }

  function clearAll(tableId) {
    const st = getState(tableId);
    st.filters = {};
    st.sort = {col:'', asc:true};
    tables[tableId].onUpdate();
  }

  // Store unfiltered rows so dropdown can show all values.
  function setRows(id, rows) {
    if (tables[id]) tables[id].allRows = rows;
  }

  return {reg, apply, thead, filterBar, setRows, clearCol, clearAll, closeDropdown, getState};
})();

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
  setHTML('dash-stats',
    stat('Components', comps.length, up+' up') +
    stat('Healthy', up, comps.length ? Math.round(up/comps.length*100)+'%' : '-', up === comps.length ? 'var(--green)' : 'var(--yellow)') +
    stat('Failed', down, '', down > 0 ? 'var(--red)' : 'var(--green)') +
    stat('Pending', pending, ''));
  const totalQuarantine = folders.reduce((s,f) => s + (f.quarantine_count||0), 0);
  const hasErr = folders.some(f => (f.peers||[]).some(p => p.last_error));
  const anyScanning = folders.some(f => f.scanning);
  const healthColor = (hasErr || conflicts.length > 0) ? 'var(--red)' : anyScanning ? 'var(--cyan)' : totalQuarantine > 0 ? 'var(--yellow)' : 'var(--green)';
  const healthLabel = (hasErr || conflicts.length > 0) ? 'Error' : anyScanning ? 'Scanning' : totalQuarantine > 0 ? 'Degraded' : 'Healthy';
  const fsFP = Object.values(state).find(c => c.type === 'filesync')?.tls_fingerprint || '';
  const fsFPStat = fsFP ? stat('TLS Fingerprint', fsFP.replace('sha256:',''), 'sha256', 'var(--green)') : '';
  setHTML('fs-stats',
    stat('Sync Health', healthLabel, '', healthColor) +
    stat('Folders', folders.length, '') +
    stat('Total Files', totalFiles.toLocaleString(), '') +
    stat('Total Size', fmtBytes(totalBytes), '') +
    stat('Conflicts', conflicts.length, '', conflicts.length > 0 ? 'var(--red)' : 'var(--green)') +
    stat('Quarantined', totalQuarantine, '', totalQuarantine > 0 ? 'var(--yellow)' : 'var(--green)') +
    fsFPStat);
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

function extractFsMetrics(text) {
  const fm = {};
  const re = /^mesh_filesync_(\w+)\{folder="([^"]+)"\}\s+(\S+)/gm;
  let m;
  while ((m = re.exec(text)) !== null) {
    const metric = m[1], folder = m[2], val = parseFloat(m[3]) || 0;
    if (!fm[folder]) fm[folder] = {};
    fm[folder][metric] = val;
  }
  return fm;
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
  if (!groups.length) { setHTML(el, '<tr><td colspan="4" style="color:var(--text-muted);padding:20px">No components</td></tr>'); return; }

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
  setHTML(el, html);
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
  if (!logs.length) { setHTML(el, '<div style="color:var(--text-muted);padding:8px">No logs yet</div>'); return; }
  const last = logs.slice(-10);
  const html = last.map(l => '<div class="log-line">' + colorLog(x(l)) + '</div>').join('');
  const changed = _prevHTML.get(el) !== html;
  setHTML(el, html);
  if (changed) el.scrollTop = el.scrollHeight;
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

const expandedConflicts = new Set();
const conflictDiffCache = {};
function toggleConflict(fid, path) {
  const key = fid + '|' + path;
  if (expandedConflicts.has(key)) expandedConflicts.delete(key);
  else { expandedConflicts.add(key); if (!conflictDiffCache[key]) fetchConflictDiff(fid, path); }
  renderConflicts();
}
function fetchConflictDiff(fid, path) {
  const key = fid + '|' + path;
  fetch('/api/filesync/conflicts/diff?folder='+encodeURIComponent(fid)+'&path='+encodeURIComponent(path))
    .then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); })
    .then(d => { conflictDiffCache[key] = d; renderConflicts(); })
    .catch(e => { conflictDiffCache[key] = {error: e.message}; renderConflicts(); });
}
function renderConflictDiff(key) {
  const d = conflictDiffCache[key];
  if (!d) return '<tr><td colspan="3" class="diff-info" style="color:var(--text-muted)">Loading diff...</td></tr>';
  if (d.error) return '<tr><td colspan="3" class="diff-info" style="color:var(--red)">Error: '+x(d.error)+'</td></tr>';
  let h = '<tr><td colspan="3" style="padding:0"><div class="diff-container"><div class="diff-meta">';
  h += '<div><div class="diff-meta-label">Conflict</div><div class="diff-meta-val">'+fmtBytes(d.conflict.size)+'</div><div class="diff-meta-val" style="color:var(--text-muted)">'+timeAgo(d.conflict.mtime)+'</div></div>';
  if (d.original_exists) {
    h += '<div><div class="diff-meta-label">Original ('+x(d.original_path)+')</div><div class="diff-meta-val">'+fmtBytes(d.original.size)+'</div><div class="diff-meta-val" style="color:var(--text-muted)">'+timeAgo(d.original.mtime)+'</div></div>';
  } else {
    h += '<div><div class="diff-meta-label">Original</div><div class="diff-meta-val" style="color:var(--yellow)">Deleted</div></div>';
  }
  h += '</div>';
  if (!d.original_exists) {
    h += '<div class="diff-info" style="color:var(--yellow)">Original file no longer exists. Conflict file is the only copy.</div>';
  } else if (d.is_binary) {
    const same = d.conflict.sha256 === d.original.sha256;
    h += '<div class="diff-info" style="color:var(--yellow)">Binary file. '+(same ? 'Content identical (SHA-256 match).' : 'Content differs.')+'</div>';
  } else if (d.lines && d.lines.length > 0) {
    h += '<div class="diff-lines">';
    for (const l of d.lines) {
      const cls = l.op==='add'?'diff-add':l.op==='delete'?'diff-del':'diff-eq';
      const pfx = l.op==='add'?'+':l.op==='delete'?'-':' ';
      h += '<div class="diff-line '+cls+'">'+pfx+' '+x(l.text)+'</div>';
    }
    h += '</div>';
  } else {
    h += '<div class="diff-info" style="color:var(--green)">Files are identical.</div>';
  }
  if (d.truncated) h += '<div class="diff-truncated">Diff truncated (file too large for full comparison).</div>';
  h += '</div></td></tr>';
  return h;
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
    const backoffHtml = (p.backoff_remaining||0) > 0
      ? ' <span class="badge badge-warn" title="Peer in consecutive-failure backoff; sync attempts are paused.">backing off &middot; '+fmtElapsed(Math.round(p.backoff_remaining/1e6))+'</span>'
      : '';
    return '<tr>'
      + '<td>'+x(p.name||'-')+'</td>'
      + '<td style="font-family:var(--mono)">'+x(p.addr)+'</td>'
      + '<td>'+fmtTime(p.last_sync)+'</td>'
      + '<td style="font-family:var(--mono)">'+(p.last_seen_sequence||0)+' / '+(p.last_sent_sequence||0)+'</td>'
      + '<td>'+plan+planAge+backoffHtml+errHtml+'</td>'
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
    +   '<div><div style="color:var(--text-muted);font-size:11px">FILES &middot; DIRS &middot; SIZE</div><div'+(f.scanning?' style="color:var(--text-muted);font-style:italic"':'')+'>'+fmtTokens(f.file_count)+' &middot; '+fmtTokens(f.dir_count||0)+' &middot; '+fmtBytes(f.total_bytes||0)+(f.scanning?' <span class="badge badge-warn" style="font-size:10px;margin-left:6px">scanning</span>':'')+'</div></div>'
    +   '<div><div style="color:var(--text-muted);font-size:11px">QUARANTINED</div><div style="color:'+((f.quarantine_count||0)>0?'var(--yellow)':'var(--green)')+'">'+fmtTokens(f.quarantine_count||0)+'</div></div>'
    + '</div>'
    + ((f.quarantine_paths && f.quarantine_paths.length) ? '<div style="margin-bottom:12px"><div style="color:var(--text-muted);font-size:11px;margin-bottom:4px">QUARANTINED FILES</div>'
      + f.quarantine_paths.map(p => '<div style="font-family:var(--mono);font-size:12px;color:var(--yellow);padding:2px 0">'+x(p)+'</div>').join('')
      + '</div>' : '')
    + '<div style="margin-bottom:12px"><div style="color:var(--text-muted);font-size:11px;margin-bottom:4px">IGNORE PATTERNS ('+ignores.length+')</div><div style="display:flex;flex-wrap:wrap">'+ignoreHtml+'</div></div>'
    + '<div style="color:var(--text-muted);font-size:11px;margin-bottom:4px">PEERS &amp; SYNC PLAN</div>'
    + '<table style="width:100%"><thead><tr><th>Name</th><th>Addr</th><th>Last sync</th><th>Seen / Sent seq</th><th>Plan</th><th>Total</th></tr></thead><tbody>'
    + peerRowsHtml + '</tbody></table>'
    + '<div style="color:var(--text-muted);font-size:11px;margin-top:12px;margin-bottom:4px">PENDING FILE PREVIEW</div>'
    + previewHtml
    + renderFolderMetrics(f.id)
    + '</td></tr>';
}

function renderFolderMetrics(folderId) {
  const m = fsMetrics[folderId];
  if (!m) return '';
  function fv(key) { return m[key] != null ? m[key] : 0; }
  function dur(ns) { const s = ns; return s < 0.001 ? '<1ms' : s < 1 ? (s*1000).toFixed(0)+'ms' : s.toFixed(2)+'s'; }
  return '<div style="color:var(--text-muted);font-size:11px;margin-top:12px;margin-bottom:4px">CUMULATIVE COUNTERS</div>'
    + '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:6px 16px;font-size:12px">'
    + '<div><span style="color:var(--text-muted)">Peer syncs</span> '+fmtTokens(fv('peer_syncs_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Files DL</span> '+fmtTokens(fv('files_downloaded_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Files DEL</span> '+fmtTokens(fv('files_deleted_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Conflicts</span> '+fmtTokens(fv('files_conflicted_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Errors</span> <span style="color:'+(fv('sync_errors_total')>0?'var(--red)':'inherit')+'">'+fmtTokens(fv('sync_errors_total'))+'</span></div>'
    + '<div><span style="color:var(--text-muted)">Bytes DL</span> '+fmtBytes(fv('bytes_downloaded_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Bytes UL</span> '+fmtBytes(fv('bytes_uploaded_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Idx exchanges</span> '+fmtTokens(fv('index_exchanges_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Scans</span> '+fmtTokens(fv('scans_total'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Last scan</span> '+dur(fv('scan_duration_seconds'))+'</div>'
    + '<div><span style="color:var(--text-muted)">Last sync</span> '+dur(fv('peer_sync_duration_seconds'))+'</div>'
    + '</div>';
}

function renderFilesync() {
  TF.setRows('fs', folders);
  setHTML('fs-thead', TF.thead('fs'));
  setHTML('fs-filter-bar', TF.filterBar('fs'));
  const rows = TF.apply('fs', folders);
  const el = document.getElementById('fs-body');
  if (!rows.length) {
    let msg = 'No folders';
    if (folders.length) msg = 'No rows match the current filter.';
    else if (Object.values(state).some(c => c.type === 'filesync-folder')) msg = 'Starting\u2026';
    el.innerHTML = '<tr><td colspan="8" style="color:var(--text-muted);padding:20px">'+msg+'</td></tr>'; return;
  }
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
    const fHasErr = (f.peers||[]).some(p => p.last_error);
    const fScanning = f.scanning;
    const fDotColor = fScanning ? 'var(--cyan)' : fHasErr ? 'var(--red)' : (f.quarantine_count > 0 || f.direction === 'disabled') ? 'var(--yellow)' : 'var(--green)';
    const fDot = '<span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:'+fDotColor+';margin-right:6px;vertical-align:middle'+(fScanning?';animation:pulse 1.5s ease-in-out infinite':'')+'"></span>';
    const scanBadge = fScanning ? ' <span class="badge badge-warn" style="font-size:10px">scanning</span>' : '';
    const dimStyle = fScanning ? 'color:var(--text-muted);font-style:italic' : '';
    html += '<tr style="cursor:pointer" onclick="toggleFolder(\''+xj(f.id)+'\')">'
         +  '<td style="font-weight:600">'+arrow+' '+fDot+x(f.id)+scanBadge+planBadge+'</td>'
         +  '<td style="color:var(--text-dim)">'+x(f.path)+'</td>'
         +  '<td><span class="badge '+dirBadge+'">'+x(f.direction)+'</span></td>'
         +  '<td'+(dimStyle?' style="'+dimStyle+'"':'')+'>'+fmtTokens(f.file_count)+'</td>'
         +  '<td'+(dimStyle?' style="'+dimStyle+'"':'')+'>'+fmtTokens(f.dir_count||0)+'</td>'
         +  '<td'+(dimStyle?' style="'+dimStyle+'"':'')+'>'+fmtBytes(f.total_bytes||0)+'</td>'
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
  TF.setRows('conflict', conflicts);
  setHTML('conflict-thead', TF.thead('conflict'));
  setHTML('conflict-filter-bar', TF.filterBar('conflict'));
  const rows = TF.apply('conflict', conflicts);
  if (!rows.length) { el.innerHTML = '<tr><td colspan="3" style="color:var(--text-muted);padding:16px">'+(conflicts.length ? 'No rows match the current filter.' : 'No conflicts')+'</td></tr>'; return; }
  let html = '';
  for (const c of rows) {
    const key = c.folder_id + '|' + c.path;
    const exp = expandedConflicts.has(key);
    const arrow = exp ? '&#9660;' : '&#9654;';
    const orig = conflictDiffCache[key] && conflictDiffCache[key].original_path ? conflictDiffCache[key].original_path : '';
    const diffHint = exp ? '' : ' <span style="color:var(--text-muted);font-size:11px">[diff]</span>';
    html += '<tr style="cursor:pointer" title="Click to '+(exp?'collapse':'view diff')+'" onclick="toggleConflict(\''+xj(c.folder_id)+'\',\''+xj(c.path)+'\')">'
      + '<td>'+arrow+' '+x(c.folder_id)+diffHint+'</td><td style="color:var(--red)">'+x(c.path)+'</td><td style="color:var(--text-muted)">'+x(orig)+'</td></tr>';
    if (exp) html += renderConflictDiff(key);
  }
  el.innerHTML = html;
}

function renderFsActivity() {
  TF.setRows('fsa', fsActivities);
  setHTML('fsa-thead', TF.thead('fsa'));
  setHTML('fsa-filter-bar', TF.filterBar('fsa'));
  const el = document.getElementById('fsa-body');
  const rows = TF.apply('fsa', fsActivities);
  if (!rows.length) { el.innerHTML = '<tr><td colspan="7" style="color:var(--text-muted);padding:16px">'+(fsActivities.length ? 'No rows match the current filter.' : 'No activity yet')+'</td></tr>'; return; }
  el.innerHTML = rows.map(a => {
    const badge = a.direction === 'download' ? 'badge-ok' : a.direction === 'upload' ? 'badge-warn' : a.error ? 'badge-err' : '';
    return '<tr><td><span class="badge '+badge+'">'+x(a.direction||'error')+'</span></td><td>'+x(a.folder)+'</td><td>'+x(a.peer)+'</td><td>'+a.files+'</td><td>'+fmtBytes(a.bytes)+'</td><td>'+timeAgo(a.time)+'</td><td style="color:var(--red)">'+x(a.error||'')+'</td></tr>';
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

  const finalHtml = html || '<div style="color:var(--text-muted);padding:16px">No matching logs</div>';
  const wasNearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 100;
  setHTML(el, finalHtml);
  const suffix = logMode === 'file' && fileLogSize > 0 ? ' (file: ' + (fileLogSize/1024).toFixed(0) + ' KB)' : '';
  document.getElementById('log-count').textContent = shown + ' / ' + total + ' lines' + suffix;

  if (wasNearBottom) {
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
function last(arr) { return arr.length ? arr[arr.length - 1] : 0; }
function renderCharts() {
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
  // Filesync tab charts (mirrors dashboard charts but on the filesync tab)
  if (activeTab === 'filesync') renderFsTabCharts();
}

function renderFsTabCharts() {
  const tc = document.getElementById('fstab-traffic-card');
  if (chartHist.fsDlRate.some(v => v > 0) || chartHist.fsUlRate.some(v => v > 0)) {
    tc.style.display = '';
    document.getElementById('fstab-dl-val').textContent = fmtRate(last(chartHist.fsDlRate));
    document.getElementById('fstab-ul-val').textContent = fmtRate(last(chartHist.fsUlRate));
    drawChart('fstab-traffic', [chartHist.fsDlRate, chartHist.fsUlRate], ['#34d399', '#a78bfa']);
  } else { tc.style.display = 'none'; }
  const sc = document.getElementById('fstab-sync-card');
  if (chartHist.fsSyncCycles.some(v => v > 0) || chartHist.fsSyncErrors.some(v => v > 0)) {
    sc.style.display = '';
    document.getElementById('fstab-cycles-val').textContent = last(chartHist.fsSyncCycles);
    document.getElementById('fstab-errors-val').textContent = last(chartHist.fsSyncErrors);
    drawChart('fstab-sync', [chartHist.fsSyncCycles, chartHist.fsSyncErrors], ['#22d3ee', '#f87171']);
  } else { sc.style.display = 'none'; }
}

// --- Performance tab ---
const perfChartHist = { walkMs:[], hashMs:[], syncMs:[], heapMb:[], sysMb:[], goroutines:[], fds:[] };
const PERF_HIST_LEN = 60;
function pushPerfHist(k, v) { perfChartHist[k].push(v); if (perfChartHist[k].length > PERF_HIST_LEN) perfChartHist[k].shift(); }

function renderPerf() {
  if (!perfEvents || !perfEvents.length) return;
  const scans = perfEvents.filter(e => e.event === 'scan');
  const syncs = perfEvents.filter(e => e.event === 'sync');
  const persists = perfEvents.filter(e => e.event === 'persist');
  const snaps = perfEvents.filter(e => e.event === 'snapshot');

  // KPI cards from latest events
  const lastScan = scans.length ? scans[scans.length - 1] : null;
  const lastSync = syncs.length ? syncs[syncs.length - 1] : null;
  const lastSnap = snaps.length ? snaps[snaps.length - 1] : null;
  const scanTotal = lastScan ? (lastScan.walk_ms + lastScan.hash_ms + lastScan.stat_ms + (lastScan.ignore_ms||0) + (lastScan.deletion_ms||0)).toFixed(0) : '-';
  const syncDur = lastSync ? lastSync.duration_ms.toFixed(0) : '-';
  const heapMb = lastSnap ? lastSnap.heap_mb : '-';
  const sysMb = lastSnap ? lastSnap.sys_mb : '-';
  const gor = lastSnap ? lastSnap.goroutines : '-';
  const fds = lastSnap ? (lastSnap.open_fds >= 0 ? lastSnap.open_fds : 'n/a') : '-';
  setHTML('perf-stats',
    stat('Last Scan', scanTotal + ' ms', lastScan ? lastScan.folder : '') +
    stat('Last Sync', syncDur + ' ms', lastSync ? lastSync.folder + ' → ' + lastSync.peer : '') +
    stat('Heap', heapMb + ' MB', '') +
    stat('Sys', sysMb + ' MB', '') +
    stat('Goroutines', gor, '') +
    stat('FDs', fds, '')
  );

  // Build chart histories from snapshot events
  perfChartHist.heapMb = snaps.slice(-PERF_HIST_LEN).map(s => s.heap_mb);
  perfChartHist.sysMb = snaps.slice(-PERF_HIST_LEN).map(s => s.sys_mb);
  perfChartHist.goroutines = snaps.slice(-PERF_HIST_LEN).map(s => s.goroutines);
  perfChartHist.fds = snaps.slice(-PERF_HIST_LEN).map(s => s.open_fds >= 0 ? s.open_fds : 0);
  perfChartHist.walkMs = scans.slice(-PERF_HIST_LEN).map(s => s.walk_ms);
  perfChartHist.hashMs = scans.slice(-PERF_HIST_LEN).map(s => s.hash_ms);
  perfChartHist.syncMs = syncs.slice(-PERF_HIST_LEN).map(s => s.duration_ms);

  // Chart values
  if (lastScan) {
    document.getElementById('perf-scan-walk').textContent = lastScan.walk_ms.toFixed(0);
    document.getElementById('perf-scan-hash').textContent = lastScan.hash_ms.toFixed(0);
  }
  if (lastSync) document.getElementById('perf-sync-dur').textContent = lastSync.duration_ms.toFixed(0) + ' ms';
  if (lastSnap) {
    document.getElementById('perf-heap-val').textContent = lastSnap.heap_mb;
    document.getElementById('perf-sys-val').textContent = lastSnap.sys_mb;
    document.getElementById('perf-gor-val').textContent = lastSnap.goroutines;
    document.getElementById('perf-fd-val').textContent = lastSnap.open_fds >= 0 ? lastSnap.open_fds : 'n/a';
  }
  drawChart('perf-chart-scan', [perfChartHist.walkMs, perfChartHist.hashMs], ['#22d3ee', '#34d399']);
  drawChart('perf-chart-sync', [perfChartHist.syncMs], ['#a78bfa']);
  drawChart('perf-chart-mem', [perfChartHist.heapMb, perfChartHist.sysMb], ['#34d399', '#a78bfa']);
  drawChart('perf-chart-gor', [perfChartHist.goroutines, perfChartHist.fds], ['#22d3ee', '#facc15']);

  // Folder filter for scans
  const sel = document.getElementById('perf-scan-folder');
  const curVal = sel.value;
  const folderIds = [...new Set(scans.map(s => s.folder))].sort();
  if (sel.options.length !== folderIds.length + 1) {
    sel.innerHTML = '<option value="">All folders</option>' + folderIds.map(f => '<option value="'+x(f)+'">'+x(f)+'</option>').join('');
    sel.value = curVal;
  }
  const filterFolder = sel.value;

  // Scans table (most recent first)
  const filteredScans = filterFolder ? scans.filter(s => s.folder === filterFolder) : scans;
  const scanRows = filteredScans.slice(-50).reverse().map(s => {
    const total = s.walk_ms + s.hash_ms + s.stat_ms;
    const cls = total > 10000 ? ' style="color:var(--red)"' : total > 5000 ? ' style="color:var(--yellow)"' : '';
    return '<tr><td>'+fmtPerfTs(s.ts)+'</td><td>'+x(s.folder)+'</td>'+
      '<td'+cls+'>'+s.walk_ms.toFixed(0)+'</td><td'+cls+'>'+s.hash_ms.toFixed(0)+'</td><td>'+s.stat_ms.toFixed(0)+'</td>'+
      '<td>'+(s.snapshot_ms||0).toFixed(0)+'</td><td>'+s.active_files+'</td><td>'+s.files_hashed+'</td>'+
      '<td>'+(s.deletions||0)+'</td><td>'+(s.changed?'<span style="color:var(--green)">yes</span>':'no')+'</td></tr>';
  }).join('');
  setHTML('perf-scan-body', scanRows || '<tr><td colspan="10" style="text-align:center;color:var(--text-muted)">No scan events</td></tr>');

  // Syncs table
  const syncRows = syncs.slice(-50).reverse().map(s => {
    const cls = s.failed > 0 ? ' style="color:var(--red)"' : '';
    return '<tr><td>'+fmtPerfTs(s.ts)+'</td><td>'+x(s.folder)+'</td><td>'+x(s.peer)+'</td>'+
      '<td>'+s.duration_ms.toFixed(0)+' ms</td><td>'+s.remote_entries+'</td>'+
      '<td>'+(s.downloads||0)+'</td><td>'+(s.conflicts||0)+'</td><td>'+(s.deletes||0)+'</td>'+
      '<td'+cls+'>'+(s.failed||0)+'</td></tr>';
  }).join('');
  setHTML('perf-sync-body', syncRows || '<tr><td colspan="9" style="text-align:center;color:var(--text-muted)">No sync events</td></tr>');

  // Persists table
  const persistRows = persists.slice(-30).reverse().map(s => {
    return '<tr><td>'+fmtPerfTs(s.ts)+'</td><td>'+x(s.folder)+'</td>'+
      '<td>'+s.index_ms.toFixed(1)+'</td><td>'+s.peers_ms.toFixed(1)+'</td>'+
      '<td>'+s.index_bytes+'</td><td>'+(s.skipped_index?'<span style="color:var(--green)">yes</span>':'no')+'</td></tr>';
  }).join('');
  setHTML('perf-persist-body', persistRows || '<tr><td colspan="6" style="text-align:center;color:var(--text-muted)">No persist events</td></tr>');

  // Snapshots table
  const snapRows = snaps.slice(-20).reverse().map(s => {
    const folderInfo = (s.folders||[]).map(f => x(f.id)+': '+f.active+' files, '+fmtBytes(f.total_bytes)).join('; ');
    return '<tr><td>'+fmtPerfTs(s.ts)+'</td><td>'+s.heap_mb+'</td><td>'+s.sys_mb+'</td>'+
      '<td>'+s.goroutines+'</td><td>'+(s.open_fds>=0?s.open_fds:'n/a')+'</td>'+
      '<td>'+(s.gc_pause_us||0)+' \u00B5s</td><td style="max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="'+x(folderInfo)+'">'+x(folderInfo||'-')+'</td></tr>';
  }).join('');
  setHTML('perf-snap-body', snapRows || '<tr><td colspan="7" style="text-align:center;color:var(--text-muted)">No snapshots</td></tr>');
}

function fmtPerfTs(ts) {
  if (!ts) return '-';
  const d = new Date(ts);
  return d.toLocaleTimeString()+'.'+String(d.getMilliseconds()).padStart(3,'0');
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
  setHTML('met-stats',
    stat('Metric Families', families.length, '') +
    stat('Total Samples', totalSamples, '') +
    stat('Gauges', gauges, '') +
    stat('Counters', counters, ''));

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

// --- Filters ---
document.getElementById('comp-search').addEventListener('input', renderComponents);
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
  {method:'GET', path:'/api/filesync/conflicts/diff?folder=ID&path=REL', desc:'Diff between a conflict file and its original. Returns: conflict_path, original_path, original_exists, is_binary, conflict/original metadata (size, mtime, sha256), lines (op: equal/add/delete + text), truncated.'},
  {method:'GET', path:'/api/filesync/activity', desc:'Recent sync activities as JSON array: time, folder, peer, direction, files, bytes, error. Last 50 entries.'},
  {method:'GET', path:'/api/perf?limit=500&event=scan|sync|persist|snapshot', desc:'Performance events from JSONL perf log. Returns last N events (max 5000). Filter by event type.'},
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
  const csFP = Object.values(state).find(c => c.type === 'clipsync')?.tls_fingerprint || '';
  const fpStat = csFP ? stat('TLS Fingerprint', csFP.replace('sha256:',''), 'sha256', 'var(--green)') : '';
  setHTML('cs-stats',
    stat('Total Events', clipActivities.length, sends+' sent, '+recvs+' received') +
    stat('Total Size', fmtBytes(totalSize), '') +
    fpStat);

  TF.setRows('cs', clipActivities);
  setHTML('cs-thead', TF.thead('cs'));
  setHTML('cs-filter-bar', TF.filterBar('cs'));
  const el = document.getElementById('cs-body');
  const rows = TF.apply('cs', clipActivities);
  if (!rows.length) {
    setHTML(el, '<tr><td colspan="5" style="color:var(--text-muted);padding:20px">'+(clipActivities.length ? 'No rows match the current filter.' : 'No clipboard activity yet')+'</td></tr>');
    return;
  }
  setHTML(el, rows.map(a => {
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
  }).join(''));
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
  // Gather per-session metadata for richer chips.
  const sessMap = {};
  for (const p of pairs) {
    const sid = p.req.session_id;
    if (!sid) continue;
    if (!sessMap[sid]) sessMap[sid] = {turns: 0, model: '', project: '', lastTs: ''};
    const sm = sessMap[sid];
    sm.turns++;
    if (!sm.model && p.req.model) sm.model = p.req.model;
    if (!sm.project) sm.project = extractProject(p.req);
    const ts = p.resp.ts || p.req.ts || '';
    if (ts && (!sm.lastTs || ts > sm.lastTs)) sm.lastTs = ts;
  }
  const sessIds = Object.keys(sessMap);
  // Prune stale selections.
  for (const s of gwSessionSet) { if (!sessIds.includes(s)) gwSessionSet.delete(s); }
  const sessBar = document.getElementById('gw-sess-chips');
  if (sessBar) {
    if (sessIds.length > 1) {
      sessBar.style.display = '';
      sessBar.innerHTML = '<span style="color:var(--text-muted);font-size:11px;margin-right:4px">session:</span>' + sessIds.map(sid => {
        const short = sid.slice(0, 8);
        const sm = sessMap[sid];
        const on = gwSessionSet.has(sid) ? ' on' : '';
        const sub = [sm.project, sm.model, sm.turns + 't', sm.lastTs ? fmtAgo(sm.lastTs) : ''].filter(Boolean).join(' · ');
        const tip = sid + '\n' + sub;
        return '<span class="gw-chip'+on+'" data-sess="'+xa(sid)+'" title="'+xa(tip)+'" style="border-color:'+sessColor(sid)+';'+(gwSessionSet.has(sid)?'background:'+sessColor(sid)+';color:var(--bg)':'')+'">' +
          '<span class="sess-dot" style="display:inline-block;width:6px;height:6px;border-radius:50%;background:'+sessColor(sid)+';margin-right:4px;vertical-align:middle"></span>'+x(short)+
          '<span style="margin-left:4px;font-size:10px;opacity:0.7">'+x(sm.project || sm.model || '')+'</span></span>';
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

  // Project filter chips — extracted from request bodies.
  const projectIds = [...new Set(pairs.map(p => extractProject(p.req) || p.req.path || '').filter(Boolean))].sort();
  for (const s of gwProjectSet) { if (!projectIds.includes(s)) gwProjectSet.delete(s); }
  const projBar = document.getElementById('gw-project-chips');
  if (projBar) {
    if (projectIds.length >= 1) {
      projBar.style.display = '';
      projBar.innerHTML = '<span style="color:var(--text-muted);font-size:11px;margin-right:4px">project:</span>' + projectIds.map(pid => {
        const on = gwProjectSet.has(pid) ? ' on' : '';
        return '<span class="gw-chip'+on+'" data-proj="'+xa(pid)+'" style="color:var(--cyan)">'+x(pid)+'</span>';
      }).join('');
      projBar.querySelectorAll('.gw-chip').forEach(c => c.addEventListener('click', () => {
        const pid = c.dataset.proj;
        if (gwProjectSet.has(pid)) gwProjectSet.delete(pid); else gwProjectSet.add(pid);
        renderGateway();
        writeGwHash();
      }));
    } else {
      projBar.style.display = 'none';
      projBar.innerHTML = '';
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
    // Pre-filter by chip selections (session, project).
    let preFiltered = pairs;
    if (gwSessionSet.size > 0) preFiltered = preFiltered.filter(p => gwSessionSet.has(p.req.session_id||''));
    if (gwProjectSet.size > 0) preFiltered = preFiltered.filter(p => gwProjectSet.has(extractProject(p.req) || p.req.path || ''));

    TF.setRows('gw', pairs);
    setHTML('gw-thead', TF.thead('gw'));
    setHTML('gw-filter-bar', TF.filterBar('gw'));
    const filtered = TF.apply('gw', preFiltered);

    const body = document.getElementById('gw-body');
    if (!filtered.length) {
      body.innerHTML = '<tr><td colspan="14" style="color:var(--text-muted);padding:20px">No rows match the current filter.</td></tr>';
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
      const status = p.resp.status || 0;
      const statusColor = status >= 400 ? 'var(--red)' : status >= 200 ? 'var(--green)' : 'var(--text-dim)';
      const outcome = p.resp.outcome || '-';
      const outcomeColor = outcome === 'ok' ? 'var(--green)' : outcome === 'error' ? 'var(--red)' : 'var(--yellow)';
      const u = p.resp.usage || {};
      const project = extractProject(p.req) || p.req.path || '-';
      const summary = renderGwSummaryCell(p.resp);
      return '<tr style="cursor:pointer" onclick="showGwDetail(\''+xj(p.key)+'\')">'+
        '<td style="color:var(--text-muted);white-space:nowrap">'+fmtLocalTime(ts)+'</td>'+
        '<td style="color:'+modelColor(gw)+'">'+x(gw)+'</td>'+
        '<td><code style="color:'+sidClr+';font-size:11px" title="'+xa(sid)+'">'+x(sidShort)+'</code></td>'+
        '<td style="color:'+dirColor(dir)+'">'+x(dir)+'</td>'+
        '<td style="color:'+modelColor(model)+'">'+x(model)+'</td>'+
        '<td style="color:'+(upModel && upModel !== model ? modelColor(upModel) : 'var(--text-muted)')+'">'+(upModel && upModel !== model ? x(upModel) : '-')+'</td>'+
        '<td style="color:'+statusColor+'">'+status+'</td>'+
        '<td style="color:'+outcomeColor+'">'+x(outcome)+'</td>'+
        '<td>'+fmtTokensHtml(u.input_tokens)+'</td>'+
        '<td>'+fmtTokensHtml(u.output_tokens)+'</td>'+
        '<td>'+renderCacheBadgeForPair(p)+'</td>'+
        '<td>'+fmtElapsedHtml(p.resp.elapsed_ms)+'</td>'+
        '<td style="color:var(--cyan);max-width:160px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="'+xa(project)+'">'+x(project)+'</td>'+
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
  } else if (upReq && (p.req.stream || p.resp.stream_summary)) {
    // Streaming: upstream response body is consumed event-by-event and not
    // captured. Show the section with an explanatory note.
    upRespSec.style.display = '';
    document.getElementById('gw-upstream-resp-raw').innerHTML = '';
    document.getElementById('gw-upstream-resp-json-len').textContent = '';
    document.getElementById('gw-upstream-resp-len').textContent = '';
    document.getElementById('gw-upstream-resp-structured').innerHTML =
      '<div style="color:var(--text-muted);padding:12px">Upstream response not available for streamed requests. ' +
      'The response body is consumed event-by-event during SSE translation. ' +
      'See the client response stream summary for the reassembled content.</div>';
  } else {
    upRespSec.style.display = 'none';
  }
  // Turn details section below the 4-pane grid.
  document.getElementById('gw-turn-details').innerHTML = renderTurnDetails(p.req, p.resp);
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

// dirColor returns a stable color for a gateway direction string.
function dirColor(d) {
  if (d === 'a2o') return 'var(--cyan)';
  if (d === 'o2a') return 'var(--purple)';
  if (d === 'a2a') return 'var(--yellow)';
  if (d === 'o2o') return 'var(--green)';
  return 'var(--text-dim)';
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

// renderTurnDetails shows audit metadata and summary info for the selected
// turn in a dedicated section below the 4-pane grid. This keeps the panes
// focused on raw JSON body content only.
function renderTurnDetails(req, resp) {
  let html = '';
  const summary = resp.stream_summary || {};

  // Audit metadata table.
  const statusCls = resp.status >= 400 ? ' style="color:var(--red)"' : resp.status >= 200 ? ' style="color:var(--green)"' : '';
  const outcomeCls = resp.outcome === 'ok' ? ' style="color:var(--green)"' : resp.outcome === 'error' ? ' style="color:var(--red)"' : '';
  html += '<div class="section-title">Turn details</div>';
  html += '<table class="meta-tbl">' +
    metaRow('session '+info(tokenHelp.sessionId), req.session_id) +
    metaRow('turn '+info(tokenHelp.turnIndex), req.turn_index) +
    metaRow('direction', req.direction, '', req.direction ? 'color:'+dirColor(req.direction) : '') +
    metaRow('path', req.path, 'dim') +
    (resp.status ? '<tr><td class="mk">status</td><td class="mv"'+statusCls+'>'+x(String(resp.status))+'</td></tr>' : '') +
    (resp.outcome ? '<tr><td class="mk">outcome</td><td class="mv"'+outcomeCls+'>'+x(resp.outcome)+'</td></tr>' : '') +
    metaRow('stop_reason '+info(tokenHelp.stopReason), summary.stop_reason) +
    (resp.elapsed_ms ? '<tr><td class="mk">elapsed</td><td class="mv">'+fmtElapsedHtml(resp.elapsed_ms)+'</td></tr>' : '') +
    metaRow('events', summary.events) +
    metaRow('message_id', summary.message_id) +
    metaRow('upstream_model', summary.model, '', summary.model ? 'color:'+modelColor(summary.model) : '') +
  '</table>';

  // Token breakdown bar.
  const u = resp.usage || summary.usage;
  if (u && (u.input_tokens || u.output_tokens || u.cache_read_input_tokens || u.cache_creation_input_tokens)) {
    html += renderTokenBar(u);
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
  // Metadata table: only fields present in the raw JSON body.
  html += metaTable([
    metaRow('model', body.model, '', body.model ? 'color:'+modelColor(body.model) : ''),
    metaRow('stream', body.stream ? 'true' : ''),
    typeof body.temperature === 'number' ? metaRow('temperature', body.temperature) : '',
    metaRow('max_tokens', body.max_tokens),
    body.top_p ? metaRow('top_p', body.top_p) : '',
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
    // System prompt: Anthropic has top-level obj.system; OpenAI has
    // messages[0].role === "system". Extract and render consistently.
    let sysText = '';
    if (obj.system) {
      sysText = typeof obj.system === 'string' ? obj.system : JSON.stringify(obj.system);
    }
    const msgs = Array.isArray(obj.messages) ? obj.messages : [];
    // Extract system messages from the array (OpenAI format).
    const sysMsgs = msgs.filter(m => m.role === 'system');
    const nonSysMsgs = msgs.filter(m => m.role !== 'system');
    if (!sysText && sysMsgs.length) {
      sysText = sysMsgs.map(m => typeof m.content === 'string' ? m.content : JSON.stringify(m.content)).join('\n\n');
    }
    if (sysText) {
      html += sec('System prompt', fmtLen(sysText.length)+' chars',
        renderSystemPrompt(sysText), false);
    }
    if (nonSysMsgs.length) {
      const totalChars = nonSysMsgs.reduce((n, m) => n + msgChars(m), 0);
      html += sec('Conversation ('+nonSysMsgs.length+')', nonSysMsgs.length+' msgs · '+fmtLen(totalChars)+' chars',
        '<div class="chat">' + nonSysMsgs.map((m, i) => renderBubble(m, i)).join('') + '</div>', true);
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
// without scrolling the page itself. Uses bounding-rect deltas (not
// offsetTop) so the math is correct regardless of whether .md-body
// is the anchor's offsetParent — `.md-body` is `position: static`
// by default, so offsetTop reads relative to a further ancestor and
// produces the wrong scroll target.
function _mdScroll(anchorId) {
  const el = document.getElementById(anchorId);
  if (!el) return;
  const body = el.closest('.md-body');
  if (!body) return;
  const elRect = el.getBoundingClientRect();
  const bodyRect = body.getBoundingClientRect();
  body.scrollTop += elRect.top - bodyRect.top;
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
  } else if (s.length > 2000) {
    // No markdown headings but content is long — try to build a TOC from
    // top-level XML tags (e.g. <types>, <type>, <examples>).
    const xmlRe = /^<([a-zA-Z][a-zA-Z0-9_-]*)(?:\s[^>]*)?>$/gm;
    const xmlTags = [];
    let xm;
    while ((xm = xmlRe.exec(s)) !== null) {
      xmlTags.push({tag: xm[1], offset: xm.index});
    }
    if (xmlTags.length > 1) {
      // Inject anchors into the rendered body at each escaped opening tag.
      let xIdx = 0;
      body = body.replace(/&lt;([a-zA-Z][a-zA-Z0-9_-]*)(?:\s[^&]*?)?&gt;/g, function(match, tag) {
        if (xIdx < xmlTags.length && tag === xmlTags[xIdx].tag) {
          return '<span id="'+id+'-x'+(xIdx++)+'"></span>' + match;
        }
        return match;
      });
      toc = '<div class="md-toc">' +
        '<div style="font-weight:600;color:var(--text-dim);margin-bottom:4px">Tags <span class="toc-len">'+fmtLen(s.length)+' chars</span></div>' +
        xmlTags.map(function(t, i) {
          const nextOff = i+1 < xmlTags.length ? xmlTags[i+1].offset : s.length;
          const sectionLen = nextOff - t.offset;
          const lenClr = sectionLen > 10000 ? 'var(--red)' : sectionLen > 2000 ? 'var(--yellow)' : sectionLen > 500 ? 'var(--text)' : 'var(--text-muted)';
          return '<a onclick="_mdScroll(\''+id+'-x'+i+'\')" title="&lt;'+xa(t.tag)+'&gt;">' +
            '<span class="toc-len" style="color:'+lenClr+'">'+fmtLen(sectionLen)+'</span> &lt;'+x(t.tag)+'&gt;</a>';
        }).join('') +
      '</div>';
    }
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
  // XML/HTML tags (escaped as &lt;tagname&gt;). Color tag names,
  // delimiters, and attribute name/value pairs separately so nested
  // pseudo-XML in prompts (system-reminder, command-name, etc.)
  // reads as scannable structure instead of a monochrome line.
  // Closing tags: &lt;/tagname&gt;
  out = out.replace(/&lt;\/([a-zA-Z][a-zA-Z0-9_-]*)&gt;/g,
    (m, tag) => hold('<span class="md-xml">&lt;/<span class="md-xml-tag">' + tag + '</span>&gt;</span>'));
  // Self-closing: &lt;tagname/&gt; or &lt;tagname attrs.../&gt;
  out = out.replace(/&lt;([a-zA-Z][a-zA-Z0-9_-]*)(\s[^&]*?)?\s*\/&gt;/g,
    (m, tag, attrs) => hold('<span class="md-xml">&lt;<span class="md-xml-tag">' + tag + '</span>' + highlightXMLAttrs(attrs) + '/&gt;</span>'));
  // Opening tags: &lt;tagname&gt; or &lt;tagname attr=&quot;val&quot;...&gt;
  out = out.replace(/&lt;([a-zA-Z][a-zA-Z0-9_-]*)(\s[^&]*?)?&gt;/g,
    (m, tag, attrs) => hold('<span class="md-xml">&lt;<span class="md-xml-tag">' + tag + '</span>' + highlightXMLAttrs(attrs) + '&gt;</span>'));
  // Restore placeholders.
  return out.replace(/\u0001MD(\d+)\u0002/g, (m, i) => holds[+i]);
}

// highlightXMLAttrs walks an escaped attribute substring (the
// "(\s[^&]*?)?" capture from the tag regexes — text between the
// tag name and the closing &gt;) and wraps name=&quot;value&quot;
// pairs in their own spans so an attribute-laden tag reads as
// scannable structure instead of a monochrome line. Input is
// already HTML-escaped; this only inserts span markup.
function highlightXMLAttrs(attrs) {
  if (!attrs) return '';
  return attrs.replace(
    /(\s+)([a-zA-Z_][a-zA-Z0-9_-]*)(?:=(?:&(?:quot|#34);([^&]*?)&(?:quot|#34);|&(?:apos|#39);([^&]*?)&(?:apos|#39);|([^\s&]+)))?/g,
    function(m, ws, name, dq, sq, raw) {
      const valSpan = (v, q) =>
        '<span class="md-xml-eq">=</span><span class="md-xml-val">' + (q ? q + v + q : v) + '</span>';
      let out = ws + '<span class="md-xml-attr">' + name + '</span>';
      if (dq !== undefined)      out += valSpan(dq, '&quot;');
      else if (sq !== undefined) out += valSpan(sq, '&#39;');
      else if (raw !== undefined) out += valSpan(raw, '');
      return out;
    });
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

// viewRawJSONL opens a modal showing the literal JSONL row — the
// audit log line as it would appear on disk. The frontend has the
// parsed object, not the original bytes; JSON.stringify reproduces
// a faithful single-line JSONL form (key order may differ from the
// original because go's encoding/json writes map keys alphabetically
// and the parsed-then-restringified path mirrors that).
function viewRawJSONL(which) {
  const v = gwDetailCache && gwDetailCache[which];
  if (!v) return;
  const jsonl = JSON.stringify(v);
  const pretty = JSON.stringify(v, null, 2);
  let modal = document.getElementById('jsonl-modal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'jsonl-modal';
    modal.className = 'sess-modal';
    document.body.appendChild(modal);
  }
  modal.innerHTML =
    '<div class="sess-modal-bg"></div>' +
    '<div class="sess-modal-card" style="max-width:1100px">' +
      '<div class="sess-modal-header">' +
        '<span><b>JSONL row</b> · ' + x(which) + ' · ' + fmtBytes(jsonl.length) + '</span>' +
        '<span style="display:flex;gap:6px">' +
          '<button class="copy-btn" id="jsonl-copy-line">copy line</button>' +
          '<button class="copy-btn" id="jsonl-copy-pretty">copy pretty</button>' +
          '<button class="filter-btn" id="jsonl-modal-close">&#10005;</button>' +
        '</span>' +
      '</div>' +
      '<div class="sess-modal-body" style="grid-template-columns:1fr">' +
        '<pre style="font-family:var(--mono);font-size:11px;white-space:pre;overflow:auto;max-height:64vh;padding:10px;background:var(--bg-input);border:1px solid var(--border);border-radius:6px">' +
        x(pretty) +
        '</pre>' +
      '</div>' +
    '</div>';
  modal.style.display = 'block';
  document.getElementById('jsonl-modal-close').onclick = closeRawJSONLModal;
  modal.querySelector('.sess-modal-bg').onclick = closeRawJSONLModal;
  document.getElementById('jsonl-copy-line').onclick = () => {
    navigator.clipboard.writeText(jsonl).catch(()=>{});
  };
  document.getElementById('jsonl-copy-pretty').onclick = () => {
    navigator.clipboard.writeText(pretty).catch(()=>{});
  };
  document.addEventListener('keydown', jsonlModalEscape);
}

function closeRawJSONLModal() {
  const modal = document.getElementById('jsonl-modal');
  if (modal) modal.style.display = 'none';
  document.removeEventListener('keydown', jsonlModalEscape);
}
function jsonlModalEscape(e) { if (e.key === 'Escape') closeRawJSONLModal(); }

// copyAsCurl generates a curl invocation that replays the audited
// request. The audit row's headers, method, path, and body are
// woven into a multi-line curl command. Sensitive headers
// (Authorization, x-api-key, Cookie) are written as placeholders
// the operator fills in from their secret store. Host/URL is also
// a placeholder because the audit row doesn't carry the gateway
// bind address — pick the right base URL for replay context.
//
// Click also copies the resulting command to the clipboard and
// surfaces it in a modal so the operator can review and edit
// before paste.
function copyAsCurl() {
  const req = gwDetailCache && gwDetailCache.req;
  if (!req) return;
  const cmd = buildCurlCommand(req);
  navigator.clipboard.writeText(cmd).catch(()=>{});
  // Show in a modal too so the operator can inspect what was
  // produced — clipboard alone can be jarring when secrets need
  // substituting.
  let modal = document.getElementById('curl-modal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'curl-modal';
    modal.className = 'sess-modal';
    document.body.appendChild(modal);
  }
  modal.innerHTML =
    '<div class="sess-modal-bg"></div>' +
    '<div class="sess-modal-card" style="max-width:1100px">' +
      '<div class="sess-modal-header">' +
        '<span><b>curl replay</b> — already on your clipboard. Substitute the URL host and any "YOUR_*" placeholders before running.</span>' +
        '<span style="display:flex;gap:6px">' +
          '<button class="copy-btn" id="curl-copy-again">copy</button>' +
          '<button class="filter-btn" id="curl-modal-close">&#10005;</button>' +
        '</span>' +
      '</div>' +
      '<div class="sess-modal-body" style="grid-template-columns:1fr">' +
        '<pre style="font-family:var(--mono);font-size:12px;white-space:pre-wrap;word-break:break-all;overflow:auto;max-height:64vh;padding:10px;background:var(--bg-input);border:1px solid var(--border);border-radius:6px">' +
        x(cmd) +
        '</pre>' +
      '</div>' +
    '</div>';
  modal.style.display = 'block';
  document.getElementById('curl-modal-close').onclick = closeCurlModal;
  modal.querySelector('.sess-modal-bg').onclick = closeCurlModal;
  document.getElementById('curl-copy-again').onclick = () => {
    navigator.clipboard.writeText(cmd).catch(()=>{});
  };
  document.addEventListener('keydown', curlModalEscape);
}

function closeCurlModal() {
  const modal = document.getElementById('curl-modal');
  if (modal) modal.style.display = 'none';
  document.removeEventListener('keydown', curlModalEscape);
}
function curlModalEscape(e) { if (e.key === 'Escape') closeCurlModal(); }

// diffVsAnother opens a request-picker modal then renders a
// messages-array diff between the current detail card's request
// and the picked one. Reuses B2's diffSideHTML / messagesEqual /
// closeEdgeDiffModal so the diff visualisation stays uniform
// across surfaces (B2 edge clicks → diff modal; B6 here → same
// diff modal).
function diffVsAnother() {
  const cur = gwDetailCache && gwDetailCache.req;
  if (!cur) return;
  const candidates = (gwRowsCache || []).filter(p => p && p.req && p.req !== cur);
  if (candidates.length === 0) {
    pushBriefToast('No other requests loaded to diff against.');
    return;
  }
  // Same session first, then newest first across other sessions.
  const curSid = cur.session_id || '';
  candidates.sort((a, b) => {
    const aSame = (a.req.session_id || '') === curSid && curSid;
    const bSame = (b.req.session_id || '') === curSid && curSid;
    if (aSame !== bSame) return aSame ? -1 : 1;
    return (b.req.ts || '').localeCompare(a.req.ts || '');
  });

  let modal = document.getElementById('diff-pick-modal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'diff-pick-modal';
    modal.className = 'sess-modal';
    document.body.appendChild(modal);
  }
  const rows = candidates.slice(0, 200).map(p => {
    const sid = p.req.session_id || '';
    const sidShort = sid ? sid.slice(0, 8) : '-';
    const sidClr = sid ? sessColor(sid) : 'var(--text-muted)';
    const sameSession = sid && sid === curSid ? ' <span class="badge" style="background:var(--green-dim);color:var(--green);padding:0 6px;border-radius:8px;font-size:10px">same session</span>' : '';
    const ts = p.req.ts || p.resp.ts || '';
    const turn = p.req.turn_index ? 'turn ' + p.req.turn_index : '';
    const model = p.req.model || '-';
    const msgCount = (p.req.body && Array.isArray(p.req.body.messages)) ? p.req.body.messages.length : 0;
    return '<button type="button" class="active-row" data-key="' + xa(p.key || '') + '">' +
      '<div class="ar-line1">' + x(fmtLocalTime(ts)) + ' · ' +
        '<code style="color:' + sidClr + '">' + x(sidShort) + '</code>' + sameSession +
      '</div>' +
      '<div class="ar-line2">' + x(turn) + (turn ? ' · ' : '') +
        '<span style="color:' + modelColor(model) + '">' + x(model) + '</span>' +
        ' · ' + msgCount + ' msg' + (msgCount === 1 ? '' : 's') +
      '</div>' +
    '</button>';
  }).join('');
  modal.innerHTML =
    '<div class="sess-modal-bg"></div>' +
    '<div class="sess-modal-card" style="max-width:720px">' +
      '<div class="sess-modal-header">' +
        '<span><b>Pick a request to diff against</b></span>' +
        '<button class="filter-btn" id="diff-pick-close">&#10005;</button>' +
      '</div>' +
      '<div class="sess-modal-body" style="grid-template-columns:1fr;padding:0">' +
        '<div style="max-height:62vh;overflow-y:auto">' + rows + '</div>' +
      '</div>' +
    '</div>';
  modal.style.display = 'block';
  document.getElementById('diff-pick-close').onclick = closeDiffPickModal;
  modal.querySelector('.sess-modal-bg').onclick = closeDiffPickModal;
  modal.querySelectorAll('button[data-key]').forEach(b => {
    b.onclick = () => {
      const key = b.dataset.key;
      const target = (gwRowsCache || []).find(p => p && p.key === key);
      closeDiffPickModal();
      if (target) renderRequestDiff(cur, target.req);
    };
  });
  document.addEventListener('keydown', diffPickEscape);
}

function closeDiffPickModal() {
  const modal = document.getElementById('diff-pick-modal');
  if (modal) modal.style.display = 'none';
  document.removeEventListener('keydown', diffPickEscape);
}
function diffPickEscape(e) { if (e.key === 'Escape') closeDiffPickModal(); }

// renderRequestDiff renders the same diff modal B2 uses for edge
// clicks (parent vs child), repurposed for "this request vs that
// one." Computes the longest common message-prefix and shows the
// divergent tails side by side. No node IDs to display, so the
// header just labels the two sides "current" and "target."
function renderRequestDiff(curReq, targetReq) {
  let modal = document.getElementById('sess-edge-modal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'sess-edge-modal';
    modal.className = 'sess-modal';
    document.body.appendChild(modal);
  }
  const aMsgs = (curReq && curReq.body && Array.isArray(curReq.body.messages)) ? curReq.body.messages : [];
  const bMsgs = (targetReq && targetReq.body && Array.isArray(targetReq.body.messages)) ? targetReq.body.messages : [];
  let lcp = 0;
  while (lcp < aMsgs.length && lcp < bMsgs.length && messagesEqual(aMsgs[lcp], bMsgs[lcp])) lcp++;
  const aTail = aMsgs.slice(lcp);
  const bTail = bMsgs.slice(lcp);
  const aLabel = 'current request (id ' + (curReq.id != null ? curReq.id : '?') + ')';
  const bLabel = 'target request (id ' + (targetReq.id != null ? targetReq.id : '?') + ')';
  const html = [
    '<div class="sess-modal-bg"></div>',
    '<div class="sess-modal-card">',
      '<div class="sess-modal-header">',
        '<span><b>request diff</b> · ' + lcp + ' shared message' + (lcp === 1 ? '' : 's') + '</span>',
        '<button class="filter-btn" id="sess-modal-close">&#10005;</button>',
      '</div>',
      '<div class="sess-modal-body">',
        diffSideHTML(aLabel, aTail, 'parent'),
        diffSideHTML(bLabel, bTail, 'child'),
      '</div>',
    '</div>',
  ].join('');
  modal.innerHTML = html;
  modal.style.display = 'block';
  document.getElementById('sess-modal-close').onclick = closeEdgeDiffModal;
  modal.querySelector('.sess-modal-bg').onclick = closeEdgeDiffModal;
  document.addEventListener('keydown', edgeDiffEscape);
}

// pushBriefToast surfaces a transient feedback line at the bottom-
// right corner. Reuses the B2.7 toast stack for consistency.
function pushBriefToast(message) {
  if (typeof pushSessionToast === 'function') {
    pushSessionToast(message, null);
    return;
  }
  console.log('[mesh] ' + message);
}

// buildCurlCommand assembles a multi-line curl invocation from a
// req audit row. Output is shell-safe single-quoted; headers are
// emitted with -H flags. Body is passed via --data-binary @- with
// a heredoc so embedded quotes survive paste.
function buildCurlCommand(req) {
  const method = req.method || 'POST';
  const path = req.path || '/';
  const url = 'YOUR_GATEWAY_BASE_URL' + path;
  const lines = ['curl -sS -X ' + shellQuote(method) + ' \\', '  ' + shellQuote(url) + ' \\'];
  // Sensitive headers we always replace with placeholders, even
  // when the audit row already redacted them — operator sees the
  // intent unambiguously.
  const sensitive = new Set(['authorization', 'x-api-key', 'cookie', 'x-claude-code-session-id']);
  const headers = req.headers || {};
  const headerNames = Object.keys(headers).sort();
  for (const name of headerNames) {
    const lc = name.toLowerCase();
    const vals = Array.isArray(headers[name]) ? headers[name] : [headers[name]];
    for (const v0 of vals) {
      let v = String(v0);
      if (sensitive.has(lc)) v = lc === 'authorization' ? 'Bearer YOUR_TOKEN'
                              : lc === 'x-api-key' ? 'YOUR_API_KEY'
                              : 'YOUR_' + lc.toUpperCase().replace(/-/g, '_');
      lines.push('  -H ' + shellQuote(name + ': ' + v) + ' \\');
    }
  }
  // Body: prefer the parsed JSON body re-stringified compactly;
  // fall back to a literal string body if that's what the row
  // carries.
  let body = '';
  if (req.body !== undefined && req.body !== null) {
    body = typeof req.body === 'string' ? req.body : JSON.stringify(req.body);
  }
  if (body) {
    // Use heredoc form so single quotes inside the body don't need
    // escape-juggling.
    lines.push("  --data-binary @- <<'__MESH_BODY_EOF__'");
    lines.push(body);
    lines.push("__MESH_BODY_EOF__");
  } else {
    // Trailing backslash on the previous line is a problem when
    // there's no body — drop it.
    const last = lines[lines.length - 1];
    if (last.endsWith(' \\')) lines[lines.length - 1] = last.slice(0, -2);
  }
  return lines.join('\n');
}

// shellQuote wraps s in single quotes and escapes embedded
// single quotes via the standard '\'' bash idiom. Safe for any
// string, including ones with backticks, $vars, or newlines.
function shellQuote(s) {
  return "'" + String(s).replace(/'/g, "'\\''") + "'";
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
  const valid = sub === 'overview' || sub === 'requests' || sub === 'sessions';
  if (!valid) sub = 'overview';
  const prev = gwSubview;
  gwSubview = sub;
  document.querySelectorAll('.gw-sub-btn').forEach(x => x.classList.toggle('active', x.dataset.sub === sub));
  document.getElementById('gw-sub-overview').style.display = sub === 'overview' ? '' : 'none';
  document.getElementById('gw-sub-requests').style.display = sub === 'requests' ? '' : 'none';
  document.getElementById('gw-sub-sessions').style.display = sub === 'sessions' ? '' : 'none';
  // SSE lifecycle: connect when entering, disconnect when leaving.
  if (sub === 'sessions' && prev !== 'sessions') enterSessionsView();
  else if (sub !== 'sessions' && prev === 'sessions') leaveSessionsView();
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
  if (gwProjectSet.size > 0) params.push('proj='+[...gwProjectSet].map(encodeURIComponent).join(','));
  if (gwWindow && gwWindow !== '24h') params.push('window='+encodeURIComponent(gwWindow));
  if (gwSubview === 'requests' && gwDetailKey) params.push('detail='+encodeURIComponent(gwDetailKey));
  if (gwSubview === 'sessions' && sessionView.currentBranchID) params.push('branch='+encodeURIComponent(sessionView.currentBranchID));
  if (gwSubview === 'sessions' && sessionView.currentNodeID) params.push('node='+encodeURIComponent(sessionView.currentNodeID));
  if (gwSubview === 'sessions' && sessionView.currentView === 'graph') params.push('view=graph');
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
  qs.split('&').forEach(kv => { const eq = kv.indexOf('='); if (eq < 0) return; p[kv.slice(0,eq)] = kv.slice(eq+1); });
  return {
    sub: sub || 'overview',
    gw: p.gw ? p.gw.split(',').map(decodeURIComponent) : [],
    sess: p.sess ? p.sess.split(',').map(decodeURIComponent) : [],
    proj: p.proj ? p.proj.split(',').map(decodeURIComponent) : [],
    window: p.window ? decodeURIComponent(p.window) : '',
    detail: p.detail ? decodeURIComponent(p.detail) : '',
    branch: p.branch ? decodeURIComponent(p.branch) : '',
    node: p.node ? decodeURIComponent(p.node) : '',
    view: p.view ? decodeURIComponent(p.view) : '',
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
  if (parsed.proj.length) gwProjectSet = new Set(parsed.proj);
  // Restore time window.
  if (parsed.window) {
    gwWindow = parsed.window;
    const wSel = document.getElementById('gw-window');
    if (wSel) wSel.value = gwWindow;
  }
  // Restore session-view state (branch / node / view).
  if (parsed.sub === 'sessions') {
    if (parsed.branch) sessionView.pendingBranchID = parsed.branch;
    if (parsed.node) sessionView.pendingNodeID = parsed.node;
    if (parsed.view === 'graph' || parsed.view === 'linear') {
      sessionView.pendingView = parsed.view;
    }
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

// mergeGwStats combines stats responses from multiple gateways into one.
// Totals are summed, by_model/by_session/by_path/preamble_blocks are merged
// by key, series buckets are summed by timestamp, top_requests are merged and
// re-sorted, by_hour is summed.
function mergeGwStats(results) {
  if (!results.length) return null;
  const out = {
    window: results[0].window,
    bucket: results[0].bucket,
    totals: {requests:0, errors:0, input_tokens:0, output_tokens:0,
             cache_read_tokens:0, cache_creation_tokens:0, reasoning_tokens:0,
             cache_hit_ratio:0, elapsed_sum_ms:0},
    by_model: [], by_session: [], by_path: [], by_hour: [],
    top_requests: [], preamble_blocks: [], series: [],
  };
  // Merge keyed rows into maps.
  const models = {}, sessions = {}, paths = {}, preambles = {};
  const hours = {}, seriesMap = {};
  function mergeRow(map, row) {
    const e = map[row.key];
    if (!e) { map[row.key] = Object.assign({}, row); return; }
    e.requests += row.requests || 0;
    e.input_tokens += row.input_tokens || 0;
    e.output_tokens += row.output_tokens || 0;
    e.cache_read_tokens += row.cache_read_tokens || 0;
    e.cache_creation_tokens += row.cache_creation_tokens || 0;
    if (row.turns && row.turns > (e.turns||0)) e.turns = row.turns;
    if (row.first_seen && (!e.first_seen || row.first_seen < e.first_seen)) e.first_seen = row.first_seen;
    if (row.last_seen && (!e.last_seen || row.last_seen > e.last_seen)) e.last_seen = row.last_seen;
    if (!e.first_model && row.first_model) e.first_model = row.first_model;
    if (row.paths) {
      const existing = (e.paths||'').split(', ').filter(Boolean);
      for (const p of row.paths.split(', ').filter(Boolean)) {
        if (!existing.includes(p)) existing.push(p);
      }
      e.paths = existing.join(', ');
    }
  }
  for (const r of results) {
    const t = r.totals || {};
    out.totals.requests += t.requests || 0;
    out.totals.errors += t.errors || 0;
    out.totals.input_tokens += t.input_tokens || 0;
    out.totals.output_tokens += t.output_tokens || 0;
    out.totals.cache_read_tokens += t.cache_read_tokens || 0;
    out.totals.cache_creation_tokens += t.cache_creation_tokens || 0;
    out.totals.reasoning_tokens += t.reasoning_tokens || 0;
    out.totals.elapsed_sum_ms += t.elapsed_sum_ms || 0;
    for (const row of (r.by_model||[])) mergeRow(models, row);
    for (const row of (r.by_session||[])) mergeRow(sessions, row);
    for (const row of (r.by_path||[])) mergeRow(paths, row);
    for (const row of (r.preamble_blocks||[])) mergeRow(preambles, row);
    for (const row of (r.top_requests||[])) out.top_requests.push(row);
    for (const row of (r.by_hour||[])) {
      if (!hours[row.hour]) hours[row.hour] = {hour:row.hour, requests:0, input_tokens:0, output_tokens:0};
      hours[row.hour].requests += row.requests||0;
      hours[row.hour].input_tokens += row.input_tokens||0;
      hours[row.hour].output_tokens += row.output_tokens||0;
    }
    for (const row of (r.series||[])) {
      if (!seriesMap[row.t]) seriesMap[row.t] = {t:row.t, requests:0, input_tokens:0, output_tokens:0, cache_read_tokens:0, cache_creation_tokens:0};
      const s = seriesMap[row.t];
      s.requests += row.requests||0;
      s.input_tokens += row.input_tokens||0;
      s.output_tokens += row.output_tokens||0;
      s.cache_read_tokens += row.cache_read_tokens||0;
      s.cache_creation_tokens += row.cache_creation_tokens||0;
    }
  }
  // Recompute cache hit ratio.
  const totalIn = out.totals.input_tokens + out.totals.cache_read_tokens + out.totals.cache_creation_tokens;
  if (totalIn > 0) out.totals.cache_hit_ratio = out.totals.cache_read_tokens / totalIn;
  // Convert maps to sorted arrays.
  const sortByTokens = arr => arr.sort((a,b) => (b.input_tokens+b.output_tokens) - (a.input_tokens+a.output_tokens));
  out.by_model = sortByTokens(Object.values(models));
  out.by_session = sortByTokens(Object.values(sessions));
  out.by_path = sortByTokens(Object.values(paths));
  out.preamble_blocks = sortByTokens(Object.values(preambles));
  out.by_hour = Object.values(hours).sort((a,b) => a.hour - b.hour);
  out.top_requests.sort((a,b) => (b.total_tokens||0) - (a.total_tokens||0));
  out.top_requests = out.top_requests.slice(0, 20);
  out.series = Object.values(seriesMap).sort((a,b) => a.t < b.t ? -1 : 1);
  return out;
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
    kpi.innerHTML = '<div class="stat" style="grid-column:1/-1;color:var(--text-muted)">Loading stats\u2026</div>';
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
        '<td>'+fmtTokensHtml(s.input_tokens)+' / '+fmtTokensHtml(s.output_tokens)+'</td>' +
        '<td style="color:var(--text-muted)">'+x(fmtAgo(s.last_seen||''))+'</td>' +
      '</tr>').join('');

  const models = (gwStats.by_model || []).slice(0, 10);
  document.getElementById('gw-top-models').innerHTML = models.length === 0
    ? '<tr><td colspan="4" style="color:var(--text-muted);padding:12px">No models in window.</td></tr>'
    : models.map(m => '<tr>' +
        '<td style="color:'+modelColor(m.key)+'">'+x(m.key||'-')+'</td>' +
        '<td>'+m.requests+'</td>' +
        '<td>'+fmtTokensHtml(m.input_tokens)+' / '+fmtTokensHtml(m.output_tokens)+'</td>' +
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
          '<td>'+fmtTokensHtml(p.input_tokens)+' / '+fmtTokensHtml(p.output_tokens)+'</td>' +
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
        '<td style="color:'+(r.model ? modelColor(r.model) : 'var(--text-muted)')+'">'+x(r.model||'-')+'</td>' +
        '<td style="color:var(--text-muted)">'+x(r.path||'-')+'</td>' +
        '<td style="font-weight:600">'+fmtTokensHtml(r.total_tokens)+'</td>' +
        '<td>'+fmtTokensHtml(r.input_tokens)+' / '+fmtTokensHtml(r.output_tokens)+'</td>' +
      '</tr>').join('');

  // B5 cache visibility — overview card section. gwStats.cache_stats
  // is the AggregateCacheStats payload from the backend
  // (cache_stats.go). Empty (HitRate = -1, RowsTotal = 0) renders as
  // "no cache data."
  renderCacheOverview(gwStats.cache_stats || null);

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
  setHTML('dbg-stats', html);
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

// --- TF table registrations ---
TF.reg('cs', {
  columns: [
    {key:'dir', label:'Direction', extract: r => r.direction||''},
    {key:'size', label:'Size', type:'number', extract: r => r.size||0},
    {key:'content', label:'Content', extract: r => r.preview || (r.formats||[]).join(', ') || ''},
    {key:'peer', label:'Peer', extract: r => r.peer||''},
    {key:'time', label:'Time', type:'date', extract: r => r.time||''}
  ],
  onUpdate: renderClipsync
});
TF.reg('fs', {
  columns: [
    {key:'id', label:'ID', extract: r => r.id||''},
    {key:'path', label:'Path', extract: r => r.path||''},
    {key:'direction', label:'Direction', extract: r => r.direction||''},
    {key:'file_count', label:'Files', type:'number', extract: r => r.file_count||0},
    {key:'dir_count', label:'Dirs', type:'number', extract: r => r.dir_count||0},
    {key:'total_bytes', label:'Size', type:'number', extract: r => r.total_bytes||0},
    {key:'last_sync', label:'Last sync', type:'date', extract: r => r.last_sync||''},
    {key:'peers', label:'Peers', extract: r => (r.peers||[]).map(peerLabel).join(', ')}
  ],
  onUpdate: renderFilesync
});
TF.reg('conflict', {
  columns: [
    {key:'folder_id', label:'Folder', extract: r => r.folder_id||''},
    {key:'path', label:'Conflict Path', extract: r => r.path||''},
    {key:'original', label:'Original', extract: r => { const d = conflictDiffCache[r.folder_id+'|'+r.path]; return d && d.original_path ? d.original_path : ''; }}
  ],
  onUpdate: renderConflicts
});
TF.reg('fsa', {
  columns: [
    {key:'direction', label:'Direction', extract: r => r.direction||''},
    {key:'folder', label:'Folder', extract: r => r.folder||''},
    {key:'peer', label:'Peer', extract: r => r.peer||''},
    {key:'files', label:'Files', type:'number', extract: r => r.files||0},
    {key:'bytes', label:'Size', type:'number', extract: r => r.bytes||0},
    {key:'time', label:'Time', type:'date', extract: r => r.time||''},
    {key:'error', label:'Error', extract: r => r.error||''}
  ],
  onUpdate: renderFsActivity
});
TF.reg('gw', {
  columns: [
    {key:'ts', label:'Time', type:'date', extract: p => p.resp.ts||p.req.ts||''},
    {key:'gw', label:'Gateway', extract: p => p.req.gateway||p.resp.gateway||''},
    {key:'session', label:'Session', extract: p => p.req.session_id ? p.req.session_id.slice(0,8) : ''},
    {key:'dir', label:'Dir', extract: p => p.req.direction||''},
    {key:'model', label:'Client model', extract: p => p.req.model||''},
    {key:'upmodel', label:'Upstream model', extract: p => p.req.mapped_model||(p.resp.stream_summary||{}).model||''},
    {key:'status', label:'Status', type:'number', extract: p => p.resp.status||0},
    {key:'outcome', label:'Outcome', extract: p => p.resp.outcome||''},
    {key:'in', label:'In', type:'number', extract: p => (p.resp.usage||{}).input_tokens||0},
    {key:'out', label:'Out', type:'number', extract: p => (p.resp.usage||{}).output_tokens||0},
    {key:'cache', label:'Cache', type:'number', extract: p => {
      const cs = extractClientCache(p);
      // Sort key: hit_rate scaled to 0..100, with -1 (no data) sorted last.
      // Marker-present-zero-hit gets 0 (still sorts above no-data).
      if (!cs) return -1;
      return cs.hit_rate >= 0 ? cs.hit_rate * 100 : -0.5;
    }},
    {key:'elapsed', label:'Elapsed', type:'number', extract: p => p.resp.elapsed_ms||0},
    {key:'project', label:'Project', extract: p => extractProject(p.req)||p.req.path||''},
    {key:'summary', label:'Summary', extract: p => { const s=p.resp.stream_summary; return s&&s.content ? s.content.slice(0,120) : ''; }}
  ],
  onUpdate: () => renderGateway()
});

// --- B5 cache visibility ---
//
// Three surfaces. Backend extracts CacheMarkers + CacheUsage per row
// (cache_stats.go) and folds them into:
//   - audit_stats response → gwStats.cache_stats (overview card).
//   - sessionNode.cache_summary (session view bubbles + graph nodes).
// The requests table is the only surface where the backend does NOT
// pre-compute cache_summary; we extract client-side from the row's
// already-fetched body. extractClientCache mirrors the Go extractor
// in cache_stats.go for the simpler Anthropic shape.

function renderCacheOverview(s) {
  const el = document.getElementById('gw-cache-content');
  if (!el) return;
  if (!s || (s.rows_with_cache_data === 0 && s.rows_with_markers === 0)) {
    el.innerHTML = '<div class="cache-empty">No cache data available in this window. Upstream may not support prompt caching, or no requests carried cache_control markers.</div>';
    return;
  }
  const parts = [];
  // Headline hit rate.
  if (s.hit_rate >= 0) {
    const pct = (100 * s.hit_rate).toFixed(1) + '%';
    const color = s.hit_rate >= 0.5 ? 'var(--green)' : s.hit_rate >= 0.2 ? 'var(--yellow)' : 'var(--red)';
    parts.push('<div class="cache-stat-row headline">' +
      '<span class="label">Hit rate</span>' +
      '<span class="value headline-val" style="color:'+color+'">'+pct+'</span></div>');
  }
  // Token breakdown.
  const totalIn = (s.total_standard_input_tokens || 0) + (s.total_cache_creation_tokens || 0) + (s.total_cache_read_tokens || 0);
  if (totalIn > 0) {
    const pct = n => totalIn > 0 ? ' ('+(100*n/totalIn).toFixed(0)+'%)' : '';
    parts.push('<div class="cache-stat-row"><span class="label">Cached read</span><span class="value">'+
      fmtTokens(s.total_cache_read_tokens)+' tokens'+pct(s.total_cache_read_tokens)+'</span></div>');
    parts.push('<div class="cache-stat-row"><span class="label">Cache creation</span><span class="value">'+
      fmtTokens(s.total_cache_creation_tokens)+' tokens'+pct(s.total_cache_creation_tokens)+'</span></div>');
    parts.push('<div class="cache-stat-row"><span class="label">Standard input</span><span class="value">'+
      fmtTokens(s.total_standard_input_tokens)+' tokens'+pct(s.total_standard_input_tokens)+'</span></div>');
    parts.push('<div class="cache-stat-row"><span class="label" style="color:var(--text-muted)">Total input</span><span class="value" style="color:var(--text-muted)">'+fmtTokens(totalIn)+' tokens</span></div>');
  }
  // Marker presence stats.
  const eligible = s.rows_total || 0;
  if (eligible > 0) {
    const mPct = eligible > 0 ? Math.round(100*s.rows_with_markers/eligible)+'%' : '0%';
    parts.push('<div class="cache-section-divider"></div>');
    parts.push('<div class="cache-stat-row"><span class="label">Marker presence</span><span class="value">'+
      s.rows_with_markers+' of '+eligible+' requests ('+mPct+')</span></div>');
    if (s.rows_with_markers > 0) {
      parts.push('<div class="cache-stat-row indent"><span>on system prompt</span><span>'+s.rows_with_system_marker+'</span></div>');
      parts.push('<div class="cache-stat-row indent"><span>on tools</span><span>'+s.rows_with_tool_marker+'</span></div>');
      parts.push('<div class="cache-stat-row indent"><span>on messages</span><span>'+s.rows_with_message_marker+'</span></div>');
    } else {
      parts.push('<div class="cache-stat-row indent" style="color:var(--text-muted);font-style:italic">No requests in this window carried Anthropic-style cache_control markers. (OpenAI-style automatic caching is reflected in the hit rate above.)</div>');
    }
  }
  el.innerHTML = parts.join('');
}

// extractClientCache mirrors the Go cachedCacheMarkers + cachedCacheUsage
// for the requests table. The pair p has p.req.body and p.resp.body
// already fetched. Returns a CacheSummary-shaped object or null when
// there's nothing to surface.
function extractClientCache(p) {
  if (!p) return null;
  const reqBody = p.req && p.req.body;
  const respBody = p.resp && p.resp.body;
  let markerPresent = false;
  let onSystem = false, onToolCount = 0, toolTotal = 0, onMessageCount = 0;
  if (reqBody && typeof reqBody === 'object') {
    if (Array.isArray(reqBody.system)) {
      for (const b of reqBody.system) {
        if (b && b.cache_control) { onSystem = true; break; }
      }
    }
    if (Array.isArray(reqBody.tools)) {
      toolTotal = reqBody.tools.length;
      for (const t of reqBody.tools) {
        if (t && t.cache_control) onToolCount++;
      }
    }
    if (Array.isArray(reqBody.messages)) {
      for (const m of reqBody.messages) {
        if (Array.isArray(m && m.content)) {
          for (const b of m.content) if (b && b.cache_control) onMessageCount++;
        }
      }
    }
    markerPresent = onSystem || onToolCount > 0 || onMessageCount > 0;
  }
  // Determine source from direction (a* anthropic, o* openai).
  const dir = (p.req && p.req.direction) || '';
  const source = dir.charAt(0) === 'a' ? 'anthropic' : dir.charAt(0) === 'o' ? 'openai' : 'unknown';
  let hitRate = -1, cacheRead = 0, cacheCreate = 0, stdInput = 0;
  if (respBody && typeof respBody === 'object' && respBody.usage) {
    const u = respBody.usage;
    if (source === 'anthropic') {
      const hasCache = u.cache_read_input_tokens !== undefined || u.cache_creation_input_tokens !== undefined;
      cacheRead = u.cache_read_input_tokens || 0;
      cacheCreate = u.cache_creation_input_tokens || 0;
      stdInput = u.input_tokens || 0;
      if (hasCache) {
        const denom = stdInput + cacheRead + cacheCreate;
        hitRate = denom > 0 ? cacheRead / denom : 0;
      }
    } else if (source === 'openai') {
      if (u.prompt_cache_hit_tokens !== undefined) {
        cacheRead = u.prompt_cache_hit_tokens || 0;
        const pt = u.prompt_tokens || 0;
        stdInput = Math.max(0, pt - cacheRead);
        hitRate = pt > 0 ? cacheRead / pt : 0;
      }
    }
  }
  if (!markerPresent && hitRate < 0) return null;
  return {
    marker_present: markerPresent,
    hit_rate: hitRate,
    source: source,
    cache_read: cacheRead,
    cache_creation: cacheCreate,
    standard_input: stdInput,
    on_system: onSystem,
    on_tool_count: onToolCount,
    tool_total: toolTotal,
    on_message_count: onMessageCount,
  };
}

// renderCacheBadge produces a small status pill for the requests
// table column. cs is either a CacheSummary from the backend or one
// produced client-side via extractClientCache. Returns the dim dash
// for null.
function renderCacheBadge(cs) {
  if (!cs) return '<span class="cache-badge none">—</span>';
  const tooltip = cacheBadgeTooltip(cs);
  let cls, text;
  if (cs.hit_rate >= 0) {
    const pct = Math.round(100 * cs.hit_rate);
    if (cs.marker_present) {
      cls = pct >= 50 ? 'high' : pct > 0 ? 'low' : 'zero';
    } else {
      cls = 'openai';
    }
    text = pct + '% hit';
  } else if (cs.marker_present) {
    cls = 'low';
    text = 'mark';
  } else {
    return '<span class="cache-badge none">—</span>';
  }
  return '<span class="cache-badge '+cls+'" title="'+xa(tooltip)+'">'+text+'</span>';
}

function cacheBadgeTooltip(cs) {
  const lines = [];
  if (cs.marker_present) {
    lines.push('cache_control markers:');
    lines.push('  system: ' + (cs.on_system ? 'yes' : 'no'));
    if (cs.tool_total !== undefined) {
      lines.push('  tools: ' + (cs.on_tool_count || 0) + '/' + (cs.tool_total || 0));
    } else {
      lines.push('  tools: ' + (cs.on_tool_count || 0));
    }
    lines.push('  messages: ' + (cs.on_message_count || 0));
  } else {
    lines.push('cache_control markers: (none)');
  }
  if (cs.hit_rate >= 0) {
    lines.push('usage:');
    lines.push('  cache_read: ' + (cs.cache_read || 0).toLocaleString());
    if (cs.cache_creation) lines.push('  cache_creation: ' + cs.cache_creation.toLocaleString());
    lines.push('  standard input: ' + (cs.standard_input || 0).toLocaleString());
    lines.push('  source: ' + (cs.source || 'unknown'));
  }
  return lines.join('\n');
}

// renderCacheBadgeForPair is the table-cell entry point.
function renderCacheBadgeForPair(p) {
  return renderCacheBadge(extractClientCache(p));
}

// --- B2 Live Session View ---
//
// Linear-chat presentation of one session, fed by the SSE endpoint at
// /api/gateway/sessions/<sid>/events. State lives in sessionView (one
// object); SSE events mutate it; renderSessionView() repaints from
// state.
//
// Per-bubble pair bodies are fetched on demand via the existing
// /api/gateway/audit/pair endpoint and cached. Rendering shows a
// placeholder while fetching.
let sessionView = {
  sid: '',                  // active session id, '' when none
  dag: null,                // last-seen dag from SSE
  nodesByID: new Map(),     // body_hash -> node
  branchesByID: new Map(),  // branch_id -> branch
  defaultBranchID: '',
  currentBranchID: '',      // user-selected, persists in URL hash
  currentNodeID: '',        // highlighted node, persists in URL hash
  currentView: 'linear',    // 'linear' | 'graph', persists in URL hash
  pendingBranchID: '',      // applied from URL on first dag_init
  pendingNodeID: '',
  pendingView: '',
  pairCache: new Map(),     // body_hash -> {request, response}
  pairFetching: new Set(),  // body_hashes with fetch in flight
  sse: null,                // EventSource handle
  reconnectTimer: 0,        // timeout id for backoff after error
  // Graph view interactivity (B2.6).
  graphViewBox: null,       // {x, y, w, h} or null = autofit on next paint
  graphLayout: null,        // last-computed layout (for edge-click diff)
  graphPan: null,           // {startX, startY, originX, originY} during drag
  // Live-update polish (B2.7).
  freshNodeIDs: new Set(),  // node ids to fade-in on next render; cleared after
  pendingNewBubble: false,  // set when a new node lands on the current branch and the user is not at the bottom
};

// enterSessionsView opens the SSE connection if a single session is
// selected and renders the empty state otherwise.
function enterSessionsView() {
  refreshSessionsView();
}

// leaveSessionsView closes any open SSE connection. Called on tab/sub
// change away from sessions.
function leaveSessionsView() {
  closeSessionSSE();
  sessionView.sid = '';
}

// refreshSessionsView is called on sub-view entry and whenever the
// session-chip selection changes while the sessions sub-view is
// visible. Connects (or reconnects) to the right session, or shows
// the empty state.
function refreshSessionsView() {
  const sessIDs = [...gwSessionSet];
  const empty = document.getElementById('sess-empty');
  const content = document.getElementById('sess-content');
  if (sessIDs.length !== 1) {
    closeSessionSSE();
    sessionView.sid = '';
    if (empty) empty.style.display = '';
    if (content) content.style.display = 'none';
    return;
  }
  const sid = sessIDs[0];
  if (empty) empty.style.display = 'none';
  if (content) content.style.display = '';
  if (sid !== sessionView.sid) {
    openSessionSSE(sid);
  }
}

// openSessionSSE replaces any existing connection with one targeting
// /api/gateway/sessions/<sid>/events. The browser handles automatic
// retry with Last-Event-ID; on connection error we add a small
// frontend backoff to avoid hammering when offline.
function openSessionSSE(sid) {
  closeSessionSSE();
  sessionView.sid = sid;
  sessionView.dag = null;
  sessionView.nodesByID = new Map();
  sessionView.branchesByID = new Map();
  sessionView.pairCache = new Map();
  sessionView.pairFetching = new Set();
  setSessStatus('connecting…', '');
  const url = '/api/gateway/sessions/'+encodeURIComponent(sid)+'/events';
  let es;
  try {
    es = new EventSource(url);
  } catch (e) {
    setSessStatus('connection failed: '+(e.message||e), 'error');
    return;
  }
  sessionView.sse = es;
  es.addEventListener('dag_init', (ev) => {
    try {
      const dag = JSON.parse(ev.data);
      onSessionDagInit(dag);
    } catch (e) { setSessStatus('bad dag_init: '+e.message, 'error'); }
  });
  es.addEventListener('node_added', (ev) => {
    try {
      const payload = JSON.parse(ev.data);
      onSessionNodeAdded(payload);
    } catch (e) { setSessStatus('bad node_added: '+e.message, 'error'); }
  });
  es.addEventListener('error', () => {
    // Browsers auto-retry; just surface state.
    setSessStatus('reconnecting…', '');
  });
}

function closeSessionSSE() {
  if (sessionView.sse) {
    try { sessionView.sse.close(); } catch (_) {}
    sessionView.sse = null;
  }
  if (sessionView.reconnectTimer) {
    clearTimeout(sessionView.reconnectTimer);
    sessionView.reconnectTimer = 0;
  }
}

function setSessStatus(text, cls) {
  const el = document.getElementById('sess-status');
  if (!el) return;
  el.textContent = text || '';
  el.className = 'session-status' + (cls ? ' '+cls : '');
}

// onSessionDagInit populates state from the initial DAG. Resolves
// pending URL-hash selections (branch, node) against the new state.
function onSessionDagInit(dag) {
  sessionView.dag = dag;
  sessionView.nodesByID = new Map();
  sessionView.branchesByID = new Map();
  for (const n of (dag.nodes || [])) sessionView.nodesByID.set(n.id, n);
  for (const b of (dag.branches || [])) sessionView.branchesByID.set(b.branch_id, b);
  sessionView.defaultBranchID = dag.default_branch_id || '';
  // Resolve current branch.
  if (sessionView.pendingBranchID && sessionView.branchesByID.has(sessionView.pendingBranchID)) {
    sessionView.currentBranchID = sessionView.pendingBranchID;
  } else {
    sessionView.currentBranchID = sessionView.defaultBranchID;
  }
  sessionView.pendingBranchID = '';
  if (sessionView.pendingNodeID && sessionView.nodesByID.has(sessionView.pendingNodeID)) {
    sessionView.currentNodeID = sessionView.pendingNodeID;
  } else {
    sessionView.currentNodeID = '';
  }
  sessionView.pendingNodeID = '';
  if (sessionView.pendingView === 'graph' || sessionView.pendingView === 'linear') {
    setSessView(sessionView.pendingView);
  } else {
    setSessView(sessionView.currentView || 'linear');
  }
  sessionView.pendingView = '';
  setSessStatus(dag.nodes && dag.nodes.length ? 'live' : 'live (no nodes yet)', 'live');
  renderSessionView();
}

// onSessionNodeAdded merges one new node into state and applies any
// branch_changes payload. Branch metadata is re-derived from nodes
// for renames (the rename event carries only ids).
function onSessionNodeAdded(payload) {
  if (!payload || !payload.node) return;
  const n = payload.node;
  const isNew = !sessionView.nodesByID.has(n.id);
  sessionView.nodesByID.set(n.id, n);
  if (isNew) sessionView.freshNodeIDs.add(n.id);
  for (const c of (payload.branch_changes || [])) {
    if (c.kind === 'new_branch' && c.branch) {
      sessionView.branchesByID.set(c.branch.branch_id, c.branch);
      // Fork detected — surface as a toast (§7 polish, B2.7).
      const label = c.branch.branch_id === sessionView.defaultBranchID ? 'main' : c.branch.branch_id.slice(0, 8);
      pushSessionToast('New branch '+label+' diverged', () => {
        sessionView.currentBranchID = c.branch.branch_id;
        sessionView.currentNodeID = '';
        writeGwHash();
        renderSessionView();
      });
    } else if (c.kind === 'branch_renamed' && c.old_branch_id && c.new_branch_id) {
      const old = sessionView.branchesByID.get(c.old_branch_id);
      if (old) {
        sessionView.branchesByID.delete(c.old_branch_id);
        old.branch_id = c.new_branch_id;
        sessionView.branchesByID.set(c.new_branch_id, old);
      }
      if (sessionView.currentBranchID === c.old_branch_id) {
        sessionView.currentBranchID = c.new_branch_id;
        writeGwHash();
      }
    } else if (c.kind === 'tip_updated' && c.branch_id) {
      const b = sessionView.branchesByID.get(c.branch_id);
      if (b) {
        b.tip_id = c.new_tip_id;
        b.last_activity_at = n.ts || b.last_activity_at;
        b.node_count = (b.node_count || 0) + 1;
      }
    }
  }
  // For the linear view: if the new node lands on the current branch
  // and the user is not at the bottom, set the pending-indicator flag
  // so renderSessionBubbles renders the "↓ new message" pill.
  if (isNew && sessionView.currentView === 'linear' && gwSubview === 'sessions') {
    const chain = chainForBranch(sessionView.currentBranchID);
    if (chain.some(x => x.id === n.id)) {
      const el = document.getElementById('sess-bubbles');
      if (el) {
        const wasAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        if (!wasAtBottom) sessionView.pendingNewBubble = true;
      }
    }
  }
  renderSessionView();
  // Drop the fresh-set after the next animation frame so subsequent
  // renders do not replay the fade-in.
  if (sessionView.freshNodeIDs.size > 0) {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => { sessionView.freshNodeIDs.clear(); });
    });
  }
}

// chainForBranch walks from the branch's tip up via parent_id until
// it hits the branch root or runs out of parents. Returns the chain
// in chronological order (root first, leaf last). For a tip not in
// the local node map (race: dag_init lagged a tip_updated event)
// returns whatever it could resolve.
function chainForBranch(branchID) {
  const b = sessionView.branchesByID.get(branchID);
  if (!b) return [];
  const chain = [];
  let cur = b.tip_id;
  let safety = 0;
  while (cur && safety++ < 4096) {
    const node = sessionView.nodesByID.get(cur);
    if (!node) break;
    chain.unshift(node);
    if (node.id === b.root_id) break;
    cur = node.parent_id;
  }
  return chain;
}

function renderSessionView() {
  if (gwSubview !== 'sessions') return;
  renderSessionHeader();
  renderBranchStrip();
  if (sessionView.currentView === 'graph') {
    renderSessionGraph();
  } else {
    renderSessionBubbles();
  }
}

function renderSessionHeader() {
  const el = document.getElementById('sess-header');
  if (!el) return;
  const sid = sessionView.sid;
  const total = sessionView.nodesByID.size;
  const branches = sessionView.branchesByID.size;
  // B5 per-session cache aggregate. Sum CacheRead /
  // (CacheRead + CacheCreation + StandardInput) across nodes that
  // carry a CacheSummary with hit_rate ≥ 0.
  const agg = aggregateSessionCache();
  let cacheBlurb = '';
  if (agg) {
    const pct = Math.round(100 * agg.hitRate);
    const cls = pct >= 50 ? 'high' : pct > 0 ? 'low' : 'zero';
    cacheBlurb =
      '<span class="session-cache-summary '+cls+'" title="cache_read '+agg.read.toLocaleString()+
      ' / cache_creation '+agg.creation.toLocaleString()+' / standard input '+agg.std.toLocaleString()+'">' +
      'cache: '+pct+'% across '+agg.rows+' turn'+(agg.rows===1?'':'s')+'</span>';
  }
  el.innerHTML =
    '<span class="sess-id" title="'+xa(sid)+'">'+x(sid.slice(0, 12))+(sid.length > 12 ? '…' : '')+'</span>' +
    '<span class="sess-meta">'+total+' node'+(total===1?'':'s')+' · '+branches+' branch'+(branches===1?'':'es')+'</span>' +
    cacheBlurb;
}

function aggregateSessionCache() {
  let read = 0, creation = 0, std = 0, rows = 0;
  for (const n of sessionView.nodesByID.values()) {
    const cs = n.cache_summary;
    if (!cs || cs.hit_rate < 0) continue;
    rows++;
    read += cs.cache_read || 0;
    creation += cs.cache_creation || 0;
    std += cs.standard_input || 0;
  }
  if (rows === 0) return null;
  const denom = read + creation + std;
  return {
    hitRate: denom > 0 ? read / denom : 0,
    read, creation, std, rows,
  };
}

function renderBranchStrip() {
  const el = document.getElementById('sess-branches');
  if (!el) return;
  const branches = [...sessionView.branchesByID.values()];
  // Sort by last_activity_at descending so the most-recently-active
  // branch stays leftmost (§6.2).
  branches.sort((a, b) => (b.last_activity_at||'').localeCompare(a.last_activity_at||''));
  if (branches.length === 0) { el.innerHTML = ''; return; }
  el.innerHTML = branches.map(b => {
    const active = b.branch_id === sessionView.currentBranchID;
    const cls = 'branch-chip' + (active ? ' active' : '');
    const label = b.branch_id === sessionView.defaultBranchID ? 'main' : b.branch_id.slice(0, 8);
    const meta = (b.node_count||0)+' turn'+(b.node_count===1?'':'s');
    const ago = b.last_activity_at ? ' · '+timeAgo(b.last_activity_at) : '';
    return '<button type="button" class="'+cls+'" role="tab" aria-selected="'+active+
           '" data-branch-id="'+xa(b.branch_id)+'" title="'+xa(b.branch_id)+'">' +
           x(label) + '<span class="chip-meta">'+x(meta+ago)+'</span></button>';
  }).join('');
  el.querySelectorAll('button[data-branch-id]').forEach(btn => {
    btn.onclick = () => {
      const bid = btn.dataset.branchId;
      if (bid === sessionView.currentBranchID) return;
      sessionView.currentBranchID = bid;
      sessionView.currentNodeID = '';
      writeGwHash();
      renderSessionView();
    };
  });
}

function renderSessionBubbles() {
  const el = document.getElementById('sess-bubbles');
  if (!el) return;
  const chain = chainForBranch(sessionView.currentBranchID);
  if (chain.length === 0) { setHTML(el, ''); return; }
  // Track scroll-at-bottom so we auto-scroll on new messages.
  const wasAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  let html = chain.map(n => bubbleHTMLForNode(n)).join('');
  if (sessionView.pendingNewBubble) {
    html += '<button type="button" id="sess-new-msg-pill" class="sess-new-msg-indicator" title="Scroll to the latest turn">↓ new message</button>';
  }
  setHTML(el, html);
  // Trigger pair fetches for any nodes still missing a body.
  for (const n of chain) {
    if (!sessionView.pairCache.has(n.id)) maybeFetchPair(n);
  }
  if (wasAtBottom) {
    el.scrollTop = el.scrollHeight;
    sessionView.pendingNewBubble = false;
  }
  const pill = document.getElementById('sess-new-msg-pill');
  if (pill) pill.onclick = () => {
    el.scrollTop = el.scrollHeight;
    sessionView.pendingNewBubble = false;
    pill.remove();
  };
}

// pushSessionToast adds a transient notification card to the bottom-
// right corner. Click invokes the action and dismisses; otherwise the
// toast fades out after 6 s.
function pushSessionToast(message, onClick) {
  let stack = document.getElementById('sess-toast-stack');
  if (!stack) {
    stack = document.createElement('div');
    stack.id = 'sess-toast-stack';
    stack.className = 'sess-toast-stack';
    document.body.appendChild(stack);
  }
  const t = document.createElement('div');
  t.className = 'sess-toast';
  t.textContent = message;
  t.onclick = () => {
    if (onClick) onClick();
    dismissSessionToast(t);
  };
  stack.appendChild(t);
  setTimeout(() => dismissSessionToast(t), 6000);
}

function dismissSessionToast(el) {
  if (!el || !el.parentNode) return;
  el.classList.add('fade-out');
  setTimeout(() => { if (el.parentNode) el.parentNode.removeChild(el); }, 220);
}

// bubbleHTMLForNode renders one node as a pair of bubbles (user +
// assistant). When the pair body is not yet cached, shows a
// placeholder. Errored responses render in a single error-styled
// bubble.
function bubbleHTMLForNode(n) {
  const cached = sessionView.pairCache.get(n.id);
  const highlight = n.id === sessionView.currentNodeID ? ' highlighted' : '';
  const meta = bubbleMetaLine(n);
  // User bubble — last user message in the request body.
  let userText = '';
  if (cached && cached.request) {
    userText = extractLastUserMessage(cached.request) || '(no user message in request)';
  } else {
    userText = '<span class="placeholder">Loading turn…</span>';
  }
  let userHTML =
    '<div class="chat-bubble user'+highlight+'" data-node-id="'+xa(n.id)+'">' +
      '<div class="bubble-role">user</div>' +
      '<div class="bubble-content">' + userText + '</div>' +
      '<div class="bubble-meta">'+meta+'</div>' +
      bubbleActions(n) +
    '</div>';
  // Assistant bubble.
  if (!n.response_summary || !n.response_summary.has_response) {
    return userHTML +
      '<div class="chat-bubble assistant" data-node-id="'+xa(n.id)+'">' +
        '<div class="bubble-role">assistant</div>' +
        '<div class="bubble-content"><span class="placeholder">in flight…</span></div>' +
      '</div>';
  }
  const status = n.response_summary.status || 0;
  const ok = status >= 200 && status < 300;
  if (!ok) {
    let errBody = cached && cached.response ? truncateForBubble(rawJSONExtract(cached.response, 'body'), 800) : '';
    return userHTML +
      '<div class="chat-bubble error" data-node-id="'+xa(n.id)+'">' +
        '<div class="bubble-role">error · status '+status+' · '+x(n.response_summary.outcome||'')+'</div>' +
        '<div class="bubble-content">'+x(errBody||'(see detail card)')+'</div>' +
        bubbleActions(n) +
      '</div>';
  }
  let assistantText = '';
  if (cached && cached.response) {
    assistantText = extractAssistantText(cached.response, n) || '(no assistant text)';
  } else {
    assistantText = '<span class="placeholder">Loading reply…</span>';
  }
  let toolsHTML = '';
  if (cached && cached.response) {
    toolsHTML = renderToolUses(cached.response);
  }
  return userHTML +
    '<div class="chat-bubble assistant'+highlight+'" data-node-id="'+xa(n.id)+'">' +
      '<div class="bubble-role">assistant</div>' +
      '<div class="bubble-content">' + assistantText + '</div>' +
      toolsHTML +
      '<div class="bubble-meta">' +
        (n.response_summary.input_tokens ? n.response_summary.input_tokens+' in · ' : '') +
        (n.response_summary.output_tokens ? n.response_summary.output_tokens+' out · ' : '') +
        (n.response_summary.elapsed_ms ? n.response_summary.elapsed_ms+' ms' : '') +
      '</div>' +
      bubbleCacheLine(n) +
      bubbleActions(n) +
    '</div>';
}

// bubbleCacheLine renders the B5 per-node cache badge inside an
// assistant bubble. Empty when the node has no CacheSummary.
function bubbleCacheLine(n) {
  const cs = n && n.cache_summary;
  if (!cs) return '';
  return '<div class="bubble-cache">' + renderCacheBadge(cs) + cacheBubbleNote(cs) + '</div>';
}

function cacheBubbleNote(cs) {
  if (!cs) return '';
  const parts = [];
  if (cs.marker_present) {
    const bits = [];
    if (cs.on_system) bits.push('system');
    if (cs.on_tool_count) bits.push(cs.on_tool_count + (cs.tool_total ? '/'+cs.tool_total : '') + ' tools');
    if (cs.on_message_count) bits.push(cs.on_message_count + ' msg' + (cs.on_message_count === 1 ? '' : 's'));
    if (bits.length) parts.push(bits.join(' + '));
  }
  if (cs.cache_read || cs.cache_creation) {
    const t = [];
    if (cs.cache_read) t.push((cs.cache_read).toLocaleString()+' read');
    if (cs.cache_creation) t.push((cs.cache_creation).toLocaleString()+' written');
    parts.push(t.join(', '));
  }
  return parts.length ? '<span style="color:var(--text-dim)">'+parts.join(' · ')+'</span>' : '';
}

function bubbleMetaLine(n) {
  const rs = n.request_summary || {};
  const parts = [];
  if (rs.model) parts.push('<span style="color:'+modelColor(rs.model)+'">'+x(rs.model)+'</span>');
  if (rs.stream) parts.push('streaming');
  if (n.ts) parts.push(x(timeAgo(n.ts)));
  return parts.join(' · ');
}

function bubbleActions(n) {
  const rs = n.request_summary || {};
  if (!rs.gateway || !n.run || n.row_id == null) return '';
  return '<div class="bubble-actions">' +
    '<a onclick="openNodeDetail(\''+xj(n.run)+'\','+(+n.row_id)+',\''+xj(rs.gateway)+'\')">open in detail card</a>' +
    '</div>';
}

// openNodeDetail switches to the requests sub-view and opens the
// existing detail card for this pair (reuses jumpToPair, with the
// pair's gateway resolved from the node summary so the user does not
// need to have selected the gateway chip).
function openNodeDetail(run, id, gw) {
  if (gw && !gwSelectedSet.has(gw)) {
    // Push the pair's gateway onto the active set so jumpToPair finds it.
    gwSelectedSet.add(gw);
  }
  jumpToPair(run, id);
}

// maybeFetchPair starts a /api/gateway/audit/pair fetch for the
// node, if not already cached or fetching. Result populates
// sessionView.pairCache; rerenders on completion.
function maybeFetchPair(n) {
  const rs = n.request_summary || {};
  if (!rs.gateway || !n.run || n.row_id == null) return;
  if (sessionView.pairCache.has(n.id) || sessionView.pairFetching.has(n.id)) return;
  sessionView.pairFetching.add(n.id);
  const url = '/api/gateway/audit/pair?gateway='+encodeURIComponent(rs.gateway)+
    '&run='+encodeURIComponent(n.run)+'&id='+encodeURIComponent(n.row_id);
  fetch(url).then(r => r.ok ? r.json() : null).then(pair => {
    sessionView.pairFetching.delete(n.id);
    if (!pair) return;
    sessionView.pairCache.set(n.id, pair);
    if (gwSubview === 'sessions') renderSessionBubbles();
  }).catch(() => { sessionView.pairFetching.delete(n.id); });
}

// extractLastUserMessage returns the text content of the last
// role:"user" message in an audit pair's request body.
function extractLastUserMessage(reqRow) {
  if (!reqRow || !reqRow.body) return '';
  const body = reqRow.body;
  const msgs = Array.isArray(body.messages) ? body.messages : [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i].role === 'user') return contentToText(msgs[i].content);
  }
  return '';
}

// extractAssistantText pulls the assistant's textual content from a
// resp row. Streaming responses have a stream_summary with
// reassembled content; non-streaming have body.content (Anthropic) or
// body.choices[0].message.content (OpenAI).
function extractAssistantText(respRow, n) {
  if (!respRow) return '';
  if (respRow.stream_summary && respRow.stream_summary.content) {
    return truncateForBubble(respRow.stream_summary.content, 8000);
  }
  if (respRow.body) {
    if (Array.isArray(respRow.body.content)) {
      // Anthropic message format.
      const texts = respRow.body.content.filter(b => b && b.type === 'text').map(b => b.text || '').join('');
      if (texts) return truncateForBubble(texts, 8000);
    }
    if (Array.isArray(respRow.body.choices) && respRow.body.choices[0]) {
      // OpenAI chat completion format.
      const msg = respRow.body.choices[0].message || {};
      if (typeof msg.content === 'string') return truncateForBubble(msg.content, 8000);
    }
  }
  return '';
}

// renderToolUses inspects a response for tool_use blocks (Anthropic)
// or tool_calls (OpenAI) and renders an expandable summary.
function renderToolUses(respRow) {
  if (!respRow) return '';
  const calls = [];
  if (respRow.stream_summary && Array.isArray(respRow.stream_summary.tool_calls)) {
    for (const t of respRow.stream_summary.tool_calls) calls.push({name: t.name||'', args: t.args});
  } else if (respRow.body && Array.isArray(respRow.body.content)) {
    for (const b of respRow.body.content) {
      if (b && b.type === 'tool_use') calls.push({name: b.name||'', args: b.input});
    }
  } else if (respRow.body && Array.isArray(respRow.body.choices)) {
    const msg = (respRow.body.choices[0]||{}).message || {};
    for (const t of (msg.tool_calls||[])) calls.push({name: (t.function||{}).name||'', args: (t.function||{}).arguments});
  }
  if (calls.length === 0) return '';
  return '<div class="bubble-tools">' +
    calls.map(c =>
      '<details><summary><span class="tool-name">'+x(c.name||'(unnamed tool)')+'</span></summary>' +
      '<pre>'+x(typeof c.args === 'string' ? c.args : JSON.stringify(c.args, null, 2))+'</pre></details>'
    ).join('') +
    '</div>';
}

// contentToText collapses Anthropic-style content arrays (which can
// be a string or an array of blocks) into a plain string for display.
function contentToText(c) {
  if (typeof c === 'string') return truncateForBubble(c, 8000);
  if (!Array.isArray(c)) return '';
  const out = [];
  for (const b of c) {
    if (typeof b === 'string') out.push(b);
    else if (b && b.type === 'text' && b.text) out.push(b.text);
    else if (b && b.type === 'tool_result') out.push('[tool_result]');
    else if (b && b.type === 'image') out.push('[image]');
  }
  return truncateForBubble(out.join('\n'), 8000);
}

function truncateForBubble(s, max) {
  if (typeof s !== 'string') return '';
  if (s.length <= max) return s;
  return s.slice(0, max) + ' … (' + (s.length - max) + ' more chars)';
}

// rawJSONExtract reaches into a response row to pull a top-level
// field as a string, used by the error-bubble fallback.
function rawJSONExtract(row, field) {
  if (!row) return '';
  const v = row[field];
  if (typeof v === 'string') return v;
  try { return JSON.stringify(v); } catch (_) { return ''; }
}

// React to session-chip changes when the sessions sub-view is open.
// The existing renderGateway path mutates gwSessionSet via chip
// clicks; refreshSessionsView rechecks selection.
const _origSetGwSub = setGwSub;
// (no-op alias kept for clarity that we wrap the session lifecycle.)

// --- Session view toggle (linear vs graph) ---

function setSessView(view) {
  if (view !== 'linear' && view !== 'graph') view = 'linear';
  sessionView.currentView = view;
  document.querySelectorAll('#sess-view-toggle .gw-sub-btn').forEach(el => {
    el.classList.toggle('active', el.dataset.view === view);
  });
  const bubbles = document.getElementById('sess-bubbles');
  const graph = document.getElementById('sess-graph');
  if (bubbles) bubbles.style.display = view === 'linear' ? '' : 'none';
  if (graph) graph.style.display = view === 'graph' ? '' : 'none';
  if (view === 'graph') renderSessionGraph();
}

document.addEventListener('click', (e) => {
  const btn = e.target.closest('#sess-view-toggle .gw-sub-btn');
  if (!btn) return;
  const v = btn.dataset.view;
  if (v === sessionView.currentView) return;
  setSessView(v);
  writeGwHash();
});

// --- Session graph layout (hand-rolled tree) ---
//
// Layered top-to-bottom layout: rank = depth from root, slot = leaf
// position within the subtree. No external graph library — for the
// sizes mesh sees in real sessions (≤ low hundreds of nodes per
// session) a simple recursive layout is more than fast enough.
function layoutSessionGraph() {
  const nodes = [...sessionView.nodesByID.values()];
  if (nodes.length === 0) return { positions: new Map(), edges: [], width: 0, height: 0 };
  // Build child map.
  const children = new Map();
  for (const n of nodes) children.set(n.id, []);
  for (const n of nodes) {
    if (n.parent_id && children.has(n.parent_id)) {
      children.get(n.parent_id).push(n);
    }
  }
  // Stable child order: oldest row_id first → leftmost branch.
  for (const arr of children.values()) {
    arr.sort((a, b) => (a.row_id || 0) - (b.row_id || 0));
  }
  // Roots: no parent_id, or parent missing from this DAG (audit-log
  // rollover edge case §10.1).
  const roots = nodes.filter(n => !n.parent_id || !sessionView.nodesByID.has(n.parent_id));
  roots.sort((a, b) => (a.row_id || 0) - (b.row_id || 0));
  // Subtree leaf-count = visual width in node-units. For a leaf the
  // width is 1; for an internal node the sum of its children's widths.
  const widthOf = new Map();
  function computeWidth(id) {
    if (widthOf.has(id)) return widthOf.get(id);
    const kids = children.get(id) || [];
    if (kids.length === 0) { widthOf.set(id, 1); return 1; }
    let sum = 0;
    for (const k of kids) sum += computeWidth(k.id);
    widthOf.set(id, sum);
    return sum;
  }
  let totalUnits = 0;
  for (const r of roots) totalUnits += computeWidth(r.id);

  // Position: each node centered over its subtree.
  const nodeW = 200, nodeH = 64, hgap = 24, vgap = 36;
  const colWidth = nodeW + hgap;
  const rowHeight = nodeH + vgap;
  const positions = new Map(); // id -> {x, y, w, h, node}
  let xOffset = 0;
  function place(id, depth, leftUnits) {
    const w = widthOf.get(id) || 1;
    const cx = (leftUnits + w / 2) * colWidth - hgap / 2;
    const x = cx - nodeW / 2;
    const y = depth * rowHeight;
    const node = sessionView.nodesByID.get(id);
    positions.set(id, { x, y, w: nodeW, h: nodeH, node });
    let kx = leftUnits;
    for (const k of children.get(id) || []) {
      place(k.id, depth + 1, kx);
      kx += widthOf.get(k.id) || 1;
    }
  }
  for (const r of roots) {
    place(r.id, 0, xOffset);
    xOffset += widthOf.get(r.id) || 1;
  }
  // Bounds.
  let maxX = 0, maxY = 0;
  for (const p of positions.values()) {
    maxX = Math.max(maxX, p.x + p.w);
    maxY = Math.max(maxY, p.y + p.h);
  }
  // Edges: parent -> child orthogonal path (down, across, down).
  const edges = [];
  for (const n of nodes) {
    if (!n.parent_id) continue;
    const child = positions.get(n.id);
    const parent = positions.get(n.parent_id);
    if (!child || !parent) continue;
    edges.push({ from: parent, to: child, fromNode: parent.node, toNode: child.node });
  }
  return { positions, edges, width: maxX + 16, height: maxY + 16 };
}

// renderSessionGraph paints the SVG. Called on view switch and
// every state mutation while the graph is visible.
function renderSessionGraph() {
  const el = document.getElementById('sess-graph');
  if (!el) return;
  if (sessionView.nodesByID.size === 0) {
    el.innerHTML = '<div class="graph-empty">No nodes in this session yet. Make a request to populate the graph.</div>';
    return;
  }
  const layout = layoutSessionGraph();
  sessionView.graphLayout = layout;
  const chainIDs = new Set(chainForBranch(sessionView.currentBranchID).map(n => n.id));
  const intrinsicW = Math.max(layout.width, 240);
  const intrinsicH = Math.max(layout.height, 120);
  // Initialize viewBox on first paint or after layout reset.
  if (!sessionView.graphViewBox) {
    sessionView.graphViewBox = { x: 0, y: 0, w: intrinsicW, h: intrinsicH };
  }
  const vb = sessionView.graphViewBox;
  const parts = [];
  parts.push('<div class="graph-controls"><button type="button" class="graph-fit-btn" id="sess-graph-fit" title="Reset zoom and pan">fit</button></div>');
  // Display SVG at intrinsic size; viewBox controls what we see.
  // The container scrolls independently; pan/zoom adjusts viewBox so
  // the same rendering surface shows different parts.
  parts.push('<svg viewBox="'+vb.x+' '+vb.y+' '+vb.w+' '+vb.h+'" width="'+intrinsicW+'" height="'+intrinsicH+
    '" xmlns="http://www.w3.org/2000/svg" id="sess-graph-svg" preserveAspectRatio="xMidYMid meet">');
  // Edges first (drawn under nodes).
  parts.push('<g class="graph-edges">');
  for (const e of layout.edges) {
    const x1 = e.from.x + e.from.w / 2;
    const y1 = e.from.y + e.from.h;
    const x2 = e.to.x + e.to.w / 2;
    const y2 = e.to.y;
    const my = (y1 + y2) / 2;
    const d = 'M'+x1+' '+y1+' V'+my+' H'+x2+' V'+y2;
    const onChain = chainIDs.has(e.toNode.id) && chainIDs.has(e.fromNode.id);
    parts.push('<path class="graph-edge'+(onChain ? ' current-branch' : '')+
      '" d="'+d+'" data-from="'+xa(e.fromNode.id)+'" data-to="'+xa(e.toNode.id)+'"></path>');
  }
  parts.push('</g>');
  // Nodes.
  parts.push('<g class="graph-nodes">');
  for (const p of layout.positions.values()) {
    const n = p.node;
    const onChain = chainIDs.has(n.id);
    const status = (n.response_summary && n.response_summary.status) || 0;
    const isErr = status > 0 && (status < 200 || status >= 300);
    const inFlight = !(n.response_summary && n.response_summary.has_response);
    const highlighted = n.id === sessionView.currentNodeID;
    const cls = ['graph-node-rect'];
    if (onChain) cls.push('current-branch');
    if (isErr) cls.push('error');
    if (inFlight) cls.push('in-flight');
    if (highlighted) cls.push('highlighted');
    // B5: color the border by cache state when a cache summary is
    // present. Zero-hit-with-markers (the actionable case) goes
    // yellow; high-hit goes green; no marker / no data leaves the
    // base stroke alone.
    const cs = n.cache_summary;
    if (cs && cs.hit_rate >= 0) {
      if (cs.marker_present && cs.hit_rate < 0.01) cls.push('cache-zero');
      else if (cs.hit_rate >= 0.5) cls.push('cache-high');
    }
    const fresh = sessionView.freshNodeIDs && sessionView.freshNodeIDs.has(n.id);
    parts.push('<g class="graph-node'+(fresh ? ' fresh' : '')+
      '" data-node-id="'+xa(n.id)+
      '" style="transform:translate('+p.x+'px,'+p.y+'px)">');
    parts.push('<rect class="'+cls.join(' ')+'" width="'+p.w+'" height="'+p.h+'" rx="6"></rect>');
    const rs = n.request_summary || {};
    const turn = (rs.message_count != null) ? Math.ceil((rs.message_count + 1) / 2) : 0;
    const headline = (rs.model || '(no model)') + (turn ? ' · t' + turn : '');
    const tokensIn = (n.response_summary && n.response_summary.input_tokens) || 0;
    const tokensOut = (n.response_summary && n.response_summary.output_tokens) || 0;
    const sub = inFlight ? 'in flight…' :
      (isErr ? 'status '+status+' '+(n.response_summary.outcome||'') :
       (tokensIn ? tokensIn+' → '+tokensOut+'t' : (n.response_summary.outcome||'ok')));
    const modelFill = rs.model ? modelColor(rs.model) : 'var(--text)';
    parts.push('<text class="graph-node-text" x="'+(p.w/2)+'" y="20" text-anchor="middle" fill="'+modelFill+'">'+x(truncForLabel(headline, 28))+'</text>');
    parts.push('<text class="graph-node-text muted" x="'+(p.w/2)+'" y="36" text-anchor="middle">'+x(truncForLabel(sub, 32))+'</text>');
    // B5 per-node cache hit rate (third line). "no cache" suppressed.
    if (cs && cs.hit_rate >= 0) {
      const cacheLine = 'cache: ' + Math.round(100 * cs.hit_rate) + '%' + (cs.marker_present ? '' : ' · auto');
      parts.push('<text class="graph-node-text muted" x="'+(p.w/2)+'" y="52" text-anchor="middle">'+x(truncForLabel(cacheLine, 32))+'</text>');
    }
    parts.push('</g>');
  }
  parts.push('</g>');
  parts.push('</svg>');
  el.innerHTML = parts.join('');
  // Click a node → open detail card. Touch a node (without a real
  // drag in between) → also open. We discriminate by tracking whether
  // a pan-drag occurred between mousedown and click.
  el.querySelectorAll('.graph-node').forEach(g => {
    g.addEventListener('click', (e) => {
      if (sessionView.graphPan && sessionView.graphPan.dragged) return;
      const id = g.dataset.nodeId;
      const n = sessionView.nodesByID.get(id);
      if (!n) return;
      sessionView.currentNodeID = id;
      writeGwHash();
      const rs = n.request_summary || {};
      if (!rs.gateway || !n.run || n.row_id == null) return;
      openNodeDetail(n.run, n.row_id, rs.gateway);
    });
    g.addEventListener('mouseenter', (e) => showGraphTooltip(g.dataset.nodeId, e));
    g.addEventListener('mousemove', (e) => moveGraphTooltip(e));
    g.addEventListener('mouseleave', () => hideGraphTooltip());
  });
  // Edge clicks open a diff modal showing the messages-array delta
  // between parent and child (§7.6). Hover style is in CSS.
  el.querySelectorAll('.graph-edge').forEach(p => {
    p.addEventListener('click', () => {
      if (sessionView.graphPan && sessionView.graphPan.dragged) return;
      const fromID = p.getAttribute('data-from');
      const toID = p.getAttribute('data-to');
      openEdgeDiffModal(fromID, toID);
    });
  });
  // Pan + zoom on the SVG.
  const svg = document.getElementById('sess-graph-svg');
  if (svg) {
    svg.addEventListener('mousedown', graphMouseDown);
    svg.addEventListener('wheel', graphWheel, { passive: false });
  }
  // Fit button: reset viewBox to the layout's natural extent so the
  // whole graph is visible.
  const fit = document.getElementById('sess-graph-fit');
  if (fit) fit.onclick = () => {
    sessionView.graphViewBox = null; // forces autofit on next paint
    renderSessionGraph();
  };
}

// --- Pan + zoom (hand-rolled, no third-party) ---

const GRAPH_ZOOM_MIN = 0.25;
const GRAPH_ZOOM_MAX = 4;

function graphSVGCoordsFromEvent(e) {
  const svg = document.getElementById('sess-graph-svg');
  if (!svg) return { x: 0, y: 0 };
  const rect = svg.getBoundingClientRect();
  const px = (e.clientX - rect.left) / rect.width;
  const py = (e.clientY - rect.top) / rect.height;
  const vb = sessionView.graphViewBox;
  return { x: vb.x + px * vb.w, y: vb.y + py * vb.h };
}

function graphMouseDown(e) {
  // Left-button pan only; ignore clicks on interactive children
  // (rects/paths handle their own click events; we still register
  // the drag-start so a real drag suppresses the click).
  if (e.button !== 0) return;
  const svg = document.getElementById('sess-graph-svg');
  if (!svg) return;
  const rect = svg.getBoundingClientRect();
  sessionView.graphPan = {
    startClientX: e.clientX,
    startClientY: e.clientY,
    originX: sessionView.graphViewBox.x,
    originY: sessionView.graphViewBox.y,
    rectW: rect.width,
    rectH: rect.height,
    dragged: false,
  };
  window.addEventListener('mousemove', graphMouseMove);
  window.addEventListener('mouseup', graphMouseUp);
}

function graphMouseMove(e) {
  const pan = sessionView.graphPan;
  if (!pan) return;
  const dx = e.clientX - pan.startClientX;
  const dy = e.clientY - pan.startClientY;
  if (Math.hypot(dx, dy) > 4) pan.dragged = true;
  const vb = sessionView.graphViewBox;
  vb.x = pan.originX - dx * vb.w / pan.rectW;
  vb.y = pan.originY - dy * vb.h / pan.rectH;
  applyGraphViewBox();
}

function graphMouseUp() {
  window.removeEventListener('mousemove', graphMouseMove);
  window.removeEventListener('mouseup', graphMouseUp);
  // Keep .dragged flag set briefly so the click handler that fires
  // on mouseup can suppress its action when a real drag happened.
  // The handler clears it on the next mousedown.
  setTimeout(() => { if (sessionView.graphPan) sessionView.graphPan.dragged = false; }, 0);
}

function graphWheel(e) {
  e.preventDefault();
  const vb = sessionView.graphViewBox;
  if (!vb) return;
  const cursor = graphSVGCoordsFromEvent(e);
  const factor = e.deltaY > 0 ? 1.1 : 0.9;
  const layout = sessionView.graphLayout;
  const intrinsicW = layout ? Math.max(layout.width, 240) : vb.w;
  const intrinsicH = layout ? Math.max(layout.height, 120) : vb.h;
  // Compute new dimensions, clamped to the zoom range.
  const newW = vb.w * factor;
  const newH = vb.h * factor;
  // Effective zoom = intrinsic / viewBox-size (smaller viewBox = zoomed in).
  const zoomBefore = intrinsicW / vb.w;
  const zoomAfter = intrinsicW / newW;
  if (zoomAfter < GRAPH_ZOOM_MIN || zoomAfter > GRAPH_ZOOM_MAX) return;
  // Adjust origin so the cursor stays over the same SVG point.
  vb.x = cursor.x - (cursor.x - vb.x) * (newW / vb.w);
  vb.y = cursor.y - (cursor.y - vb.y) * (newH / vb.h);
  vb.w = newW;
  vb.h = newH;
  applyGraphViewBox();
}

function applyGraphViewBox() {
  const svg = document.getElementById('sess-graph-svg');
  if (!svg) return;
  const vb = sessionView.graphViewBox;
  svg.setAttribute('viewBox', vb.x+' '+vb.y+' '+vb.w+' '+vb.h);
}

// --- Hover tooltip ---

function showGraphTooltip(nodeID, e) {
  const n = sessionView.nodesByID.get(nodeID);
  if (!n) return;
  const container = document.getElementById('sess-graph');
  if (!container) return;
  let tip = document.getElementById('sess-graph-tooltip');
  if (!tip) {
    tip = document.createElement('div');
    tip.id = 'sess-graph-tooltip';
    tip.className = 'graph-tooltip';
    container.appendChild(tip);
  }
  const rs = n.request_summary || {};
  const ros = n.response_summary || {};
  const cached = sessionView.pairCache.get(n.id);
  const userMsg = cached ? extractLastUserMessage(cached.request) : '';
  const lines = [];
  const turn = rs.message_count != null ? Math.ceil((rs.message_count + 1) / 2) : 0;
  const tipModelStyle = rs.model ? ' style="color:'+modelColor(rs.model)+'"' : '';
  lines.push('<div class="tip-line"><b'+tipModelStyle+'>'+x(rs.model || '(unknown model)')+'</b>'+(turn ? ' · turn '+turn : '')+'</div>');
  if (n.ts) lines.push('<div class="tip-line">'+x(n.ts)+'</div>');
  if (ros.has_response) {
    const tokIn = ros.input_tokens || 0;
    const tokOut = ros.output_tokens || 0;
    lines.push('<div class="tip-line">'+(tokIn||tokOut ? tokIn+' → '+tokOut+' tokens · ' : '') +
      'status '+ros.status+' '+(ros.outcome||'')+(ros.elapsed_ms ? ' · '+ros.elapsed_ms+'ms' : '')+'</div>');
  } else {
    lines.push('<div class="tip-line">in flight</div>');
  }
  if (userMsg) lines.push('<div class="tip-quote">"'+x(truncForLabel(userMsg, 200))+'"</div>');
  else if (!cached) {
    // Schedule a fetch so subsequent hovers carry the snippet.
    maybeFetchPair(n);
    lines.push('<div class="tip-quote" style="color:var(--text-muted)">(loading turn…)</div>');
  }
  tip.innerHTML = lines.join('');
  tip.style.display = 'block';
  moveGraphTooltip(e);
}

function moveGraphTooltip(e) {
  const tip = document.getElementById('sess-graph-tooltip');
  const container = document.getElementById('sess-graph');
  if (!tip || !container) return;
  const rect = container.getBoundingClientRect();
  let x = e.clientX - rect.left + container.scrollLeft + 12;
  let y = e.clientY - rect.top + container.scrollTop + 12;
  // Flip on right edge.
  const tipRect = tip.getBoundingClientRect();
  if (e.clientX + tipRect.width + 24 > rect.right) {
    x = e.clientX - rect.left + container.scrollLeft - tipRect.width - 12;
  }
  tip.style.left = x + 'px';
  tip.style.top = y + 'px';
}

function hideGraphTooltip() {
  const tip = document.getElementById('sess-graph-tooltip');
  if (tip) tip.style.display = 'none';
}

// --- Edge click → diff modal ---

function openEdgeDiffModal(fromID, toID) {
  const fromNode = sessionView.nodesByID.get(fromID);
  const toNode = sessionView.nodesByID.get(toID);
  if (!fromNode || !toNode) return;
  // Ensure both pair bodies are loaded before computing the diff;
  // schedule fetches and re-open when ready.
  let pending = 0;
  if (!sessionView.pairCache.has(fromID)) { maybeFetchPair(fromNode); pending++; }
  if (!sessionView.pairCache.has(toID))   { maybeFetchPair(toNode);   pending++; }
  if (pending > 0) {
    setSessStatus('loading diff…', '');
    // Poll the cache with a deadline.
    const deadline = Date.now() + 4000;
    const wait = () => {
      if (sessionView.pairCache.has(fromID) && sessionView.pairCache.has(toID)) {
        setSessStatus('live', 'live');
        renderEdgeDiffModal(fromNode, toNode);
        return;
      }
      if (Date.now() > deadline) {
        setSessStatus('diff load timed out', 'error');
        return;
      }
      setTimeout(wait, 100);
    };
    wait();
    return;
  }
  renderEdgeDiffModal(fromNode, toNode);
}

function renderEdgeDiffModal(fromNode, toNode) {
  let modal = document.getElementById('sess-edge-modal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'sess-edge-modal';
    modal.className = 'sess-modal';
    document.body.appendChild(modal);
  }
  const fromPair = sessionView.pairCache.get(fromNode.id);
  const toPair = sessionView.pairCache.get(toNode.id);
  const fromMsgs = (fromPair && fromPair.request && Array.isArray(fromPair.request.body && fromPair.request.body.messages)) ? fromPair.request.body.messages : [];
  const toMsgs   = (toPair   && toPair.request   && Array.isArray(toPair.request.body   && toPair.request.body.messages))   ? toPair.request.body.messages   : [];
  // Common-prefix length (LCP): how many leading messages match by
  // role+content text. Mismatch in role or content → diverged at k.
  let lcp = 0;
  while (lcp < fromMsgs.length && lcp < toMsgs.length && messagesEqual(fromMsgs[lcp], toMsgs[lcp])) lcp++;
  const fromTail = fromMsgs.slice(lcp);
  const toTail   = toMsgs.slice(lcp);
  const fromTurn = fromNode.request_summary && fromNode.request_summary.message_count != null ? Math.ceil((fromNode.request_summary.message_count + 1) / 2) : '?';
  const toTurn   = toNode.request_summary   && toNode.request_summary.message_count   != null ? Math.ceil((toNode.request_summary.message_count   + 1) / 2) : '?';
  const html = [
    '<div class="sess-modal-bg"></div>',
    '<div class="sess-modal-card">',
      '<div class="sess-modal-header">',
        '<span><b>messages diff</b> · turn '+fromTurn+' → turn '+toTurn+' · '+lcp+' shared message'+(lcp===1?'':'s')+'</span>',
        '<button type="button" class="filter-btn" id="sess-modal-close">&#10005;</button>',
      '</div>',
      '<div class="sess-modal-body">',
        diffSideHTML('parent (turn '+fromTurn+')', fromTail, 'parent'),
        diffSideHTML('child (turn '+toTurn+')', toTail, 'child'),
      '</div>',
    '</div>',
  ].join('');
  modal.innerHTML = html;
  modal.style.display = 'block';
  document.getElementById('sess-modal-close').onclick = closeEdgeDiffModal;
  modal.querySelector('.sess-modal-bg').onclick = closeEdgeDiffModal;
  document.addEventListener('keydown', edgeDiffEscape);
}

function diffSideHTML(label, msgs, side) {
  if (msgs.length === 0) {
    return '<div class="diff-side"><div class="diff-meta-label">'+x(label)+'</div><div class="diff-info">(no new messages)</div></div>';
  }
  return '<div class="diff-side"><div class="diff-meta-label">'+x(label)+' — +'+msgs.length+' message'+(msgs.length===1?'':'s')+'</div>' +
    msgs.map(m => {
      const role = (m && m.role) || '?';
      const txt = contentToText((m && m.content) || '');
      return '<div class="diff-msg diff-'+side+'"><div class="diff-msg-role">'+x(role)+'</div><div class="diff-msg-content">'+x(truncForLabel(txt, 800))+'</div></div>';
    }).join('') + '</div>';
}

function messagesEqual(a, b) {
  if (!a || !b) return false;
  if (a.role !== b.role) return false;
  return contentToText(a.content) === contentToText(b.content);
}

function closeEdgeDiffModal() {
  const modal = document.getElementById('sess-edge-modal');
  if (modal) modal.style.display = 'none';
  document.removeEventListener('keydown', edgeDiffEscape);
}

function edgeDiffEscape(e) {
  if (e.key === 'Escape') closeEdgeDiffModal();
}

function truncForLabel(s, max) {
  if (typeof s !== 'string') return '';
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + '…';
}


// --- B4 Live Request Tail ---
//
// Three surfaces from one /api/gateway/active poll plus per-request
// SSE on demand:
//   - Indicator in the header (`#active-indicator`).
//   - Slide-in panel listing in-flight requests (`#active-panel`).
//   - Modal showing one request's live state (`#active-modal`),
//     fed by /api/gateway/active/<id>/events.
let activeState = null;     // last /api/gateway/active response
let activePanelOpen = false;
let activeModal = {
  id: 0,
  sse: null,
  state: null,             // last received SSE state event
  completed: false,
};

// Map B1's seven raw segments to the §3.2 three-pill labels for the
// header indicator.
function activePhaseLabel(seg) {
  switch (seg) {
    case 'client_to_mesh': case 'mesh_translation_in': return 'dispatching';
    case 'mesh_to_upstream': case 'upstream_processing': return 'upstream';
    case 'mesh_translation_out': case 'mesh_to_client': return 'streaming';
    default: return '';
  }
}

function renderActiveIndicator() {
  const el = document.getElementById('active-indicator');
  if (!el) return;
  const total = (activeState && activeState.total) || 0;
  document.getElementById('active-count').textContent = total;
  if (total === 0) {
    el.classList.add('idle');
    el.setAttribute('aria-disabled', 'true');
    document.getElementById('active-phases').innerHTML = '';
    return;
  }
  el.classList.remove('idle');
  el.removeAttribute('aria-disabled');
  // Pivot by_phase into the three operator-meaningful labels.
  const counts = { dispatching: 0, upstream: 0, streaming: 0 };
  const byPhase = activeState.by_phase || {};
  for (const seg of Object.keys(byPhase)) {
    const lbl = activePhaseLabel(seg);
    if (lbl) counts[lbl] += byPhase[seg];
  }
  const parts = [];
  for (const lbl of ['dispatching', 'upstream', 'streaming']) {
    if (counts[lbl] > 0) parts.push('<span class="ai-phase '+lbl+'">'+counts[lbl]+' '+lbl+'</span>');
  }
  document.getElementById('active-phases').innerHTML = parts.join('');
}

function toggleActivePanel(open) {
  activePanelOpen = open;
  const panel = document.getElementById('active-panel');
  if (!panel) return;
  panel.classList.toggle('open', open);
  panel.setAttribute('aria-hidden', open ? 'false' : 'true');
  const btn = document.getElementById('active-indicator');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) renderActivePanel();
}

function renderActivePanel() {
  const body = document.getElementById('active-panel-body');
  const title = document.getElementById('active-panel-title');
  if (!body || !title) return;
  const reqs = (activeState && activeState.requests) || [];
  title.textContent = 'Active requests (' + reqs.length + ')';
  if (reqs.length === 0) {
    body.innerHTML = '<div class="active-empty">No requests in flight.</div>';
    return;
  }
  // Sort newest first so the most recent dispatch appears at the top.
  const sorted = [...reqs].sort((a, b) => (b.started_at||'').localeCompare(a.started_at||''));
  body.innerHTML = sorted.map(r => {
    const lbl = activePhaseLabel(r.current_segment) || 'other';
    const sid = (r.session_id || '').slice(0, 8);
    const elapsed = r.started_at ? fmtDuration(Date.now() - new Date(r.started_at).getTime()) : '';
    const segElapsed = r.segment_started_at ? fmtDuration(Date.now() - new Date(r.segment_started_at).getTime()) : '';
    return '<button type="button" class="active-row '+lbl+'" data-id="'+r.id+'">' +
      '<div class="ar-line1">[' + x(r.gateway || '?') + ']' + (sid ? ' · session ' + x(sid) : '') + ' · ' + x(elapsed) + ' total</div>' +
      '<div class="ar-line2"><span class="ar-phase">' + x(lbl) + '</span> · ' + x(r.current_segment || '?') + ' · ' + x(segElapsed) + ' in this phase</div>' +
      '<div class="ar-line3">' + (r.bytes_upstream ? fmtBytes(r.bytes_upstream)+' upstream' : '') +
      (r.bytes_downstream ? ' · '+fmtBytes(r.bytes_downstream)+' downstream' : '') +
      (r.bytes_to_client ? ' · '+fmtBytes(r.bytes_to_client)+' to client' : '') +
      '</div>' +
      '</button>';
  }).join('');
  body.querySelectorAll('.active-row').forEach(el => {
    el.onclick = () => openActiveLiveView(parseInt(el.dataset.id, 10));
  });
}

function fmtDuration(ms) {
  if (!ms || ms < 0) return '0ms';
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.floor((ms % 60000) / 1000) + 's';
}

function openActiveLiveView(id) {
  if (!id) return;
  closeActiveLiveSSE();
  activeModal.id = id;
  activeModal.state = null;
  activeModal.completed = false;
  const modal = document.getElementById('active-modal');
  modal.innerHTML = '<div class="active-modal-card">' +
    '<div class="active-modal-header"><span><b>Live request ' + id + '</b></span>' +
    '<span style="display:flex;gap:8px"><button type="button" class="filter-btn" id="active-modal-back">&larr; back</button>' +
    '<button type="button" class="filter-btn" id="active-modal-close">&#10005;</button></span></div>' +
    '<div class="active-modal-body" id="active-modal-body">connecting&hellip;</div></div>';
  modal.classList.add('open');
  modal.setAttribute('aria-hidden', 'false');
  document.getElementById('active-modal-close').onclick = closeActiveLiveView;
  document.getElementById('active-modal-back').onclick = () => { closeActiveLiveView(); toggleActivePanel(true); };
  document.addEventListener('keydown', activeModalEscape);
  // Open SSE.
  let es;
  try {
    es = new EventSource('/api/gateway/active/' + id + '/events');
  } catch (e) {
    document.getElementById('active-modal-body').textContent = 'connection failed: ' + (e.message || e);
    return;
  }
  activeModal.sse = es;
  es.addEventListener('state', (ev) => {
    try {
      activeModal.state = JSON.parse(ev.data);
      renderActiveLiveModal();
    } catch (_) {}
  });
  es.addEventListener('completed', () => {
    activeModal.completed = true;
    closeActiveLiveSSE();
    renderActiveLiveCompleted();
  });
  es.addEventListener('error', () => {
    // Browser auto-retries unless we close. Modal stays open showing
    // last known state; the next reconnect will refresh it.
  });
}

function closeActiveLiveView() {
  closeActiveLiveSSE();
  const modal = document.getElementById('active-modal');
  if (modal) {
    modal.classList.remove('open');
    modal.setAttribute('aria-hidden', 'true');
    modal.innerHTML = '';
  }
  document.removeEventListener('keydown', activeModalEscape);
  activeModal.id = 0;
  activeModal.state = null;
  activeModal.completed = false;
}

function closeActiveLiveSSE() {
  if (activeModal.sse) {
    try { activeModal.sse.close(); } catch (_) {}
    activeModal.sse = null;
  }
}

function activeModalEscape(e) {
  if (e.key === 'Escape') closeActiveLiveView();
}

const ACTIVE_SEGMENTS = [
  'client_to_mesh','mesh_translation_in','mesh_to_upstream',
  'upstream_processing','mesh_translation_out','mesh_to_client','other'
];

function renderActiveLiveModal() {
  const body = document.getElementById('active-modal-body');
  if (!body) return;
  const s = activeModal.state;
  if (!s) { body.innerHTML = 'connecting&hellip;'; return; }
  // Phase strip: cells before current = done, current = pulsing,
  // future = neutral.
  const curIdx = Math.max(0, ACTIVE_SEGMENTS.indexOf(s.current_segment));
  const cells = ACTIVE_SEGMENTS.map((seg, i) => {
    const cls = i < curIdx ? 'done' : (i === curIdx ? 'current' : 'zero');
    return '<div class="live-phase-cell '+cls+'" title="'+xa(seg)+'"></div>';
  }).join('');
  const phaseLbl = activePhaseLabel(s.current_segment) || 'other';
  const segElapsed = s.segment_started_at ? fmtDuration(Date.now() - new Date(s.segment_started_at).getTime()) : '';
  const totalElapsed = s.started_at ? fmtDuration(Date.now() - new Date(s.started_at).getTime()) : '';
  body.innerHTML =
    '<div class="live-meta">' +
      '<span class="lm-key">gateway</span><span class="lm-val">'+x(s.gateway||'?')+'</span>' +
      (s.session_id ? '<span class="lm-key">session</span><span class="lm-val">'+x(s.session_id)+'</span>' : '') +
      (s.client_model ? '<span class="lm-key">client model</span><span class="lm-val" style="color:'+modelColor(s.client_model)+'">'+x(s.client_model)+'</span>' : '') +
      (s.upstream_model && s.upstream_model !== s.client_model ? '<span class="lm-key">upstream model</span><span class="lm-val" style="color:'+modelColor(s.upstream_model)+'">'+x(s.upstream_model)+'</span>' : '') +
      '<span class="lm-key">started</span><span class="lm-val">'+x(s.started_at||'')+' ('+x(totalElapsed)+' ago)</span>' +
      '<span class="lm-key">streaming</span><span class="lm-val">'+(s.streaming?'yes':'no')+'</span>' +
    '</div>' +
    '<div class="live-phase-strip">'+cells+'</div>' +
    '<div class="live-phase-label">phase: <b class="ar-phase '+phaseLbl+'">'+x(phaseLbl)+'</b> · '+x(s.current_segment||'?')+' · '+x(segElapsed)+' in this phase</div>' +
    '<div class="live-bytes">' +
      '<div class="live-byte-card"><div class="lb-label">bytes upstream</div><div class="lb-val">'+fmtBytes(s.bytes_upstream||0)+'</div></div>' +
      '<div class="live-byte-card"><div class="lb-label">bytes downstream</div><div class="lb-val">'+fmtBytes(s.bytes_downstream||0)+'</div></div>' +
      '<div class="live-byte-card"><div class="lb-label">bytes to client</div><div class="lb-val">'+fmtBytes(s.bytes_to_client||0)+'</div></div>' +
    '</div>';
}

function renderActiveLiveCompleted() {
  const body = document.getElementById('active-modal-body');
  if (!body) return;
  const id = activeModal.id;
  body.innerHTML = '<div class="live-completed">request ' + id + ' has completed.<br/><br/>' +
    '<a id="active-jump-detail">open the audit row →</a></div>';
  const a = document.getElementById('active-jump-detail');
  if (a) a.onclick = () => {
    closeActiveLiveView();
    // Best-effort: switch to gateway tab + requests sub-view; jumpToPair
    // requires the run id which we don't have, so we just navigate the
    // user to the place they can search for it.
    showTab('gateway');
    setGwSub('requests');
  };
}

// Wire indicator click + panel close button after DOM load.
(function() {
  const tryWire = () => {
    const ind = document.getElementById('active-indicator');
    const close = document.getElementById('active-panel-close');
    if (!ind || !close) { setTimeout(tryWire, 50); return; }
    ind.onclick = () => {
      // Idle indicator is non-interactive.
      if (ind.classList.contains('idle')) return;
      toggleActivePanel(!activePanelOpen);
    };
    close.onclick = () => toggleActivePanel(false);
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', tryWire);
  } else {
    tryWire();
  }
})();

// --- Visibility-aware polling ---
let tickTimer = null;
function startPolling() { if (!tickTimer) { tick(); tickTimer = setInterval(tick, 1000); } }
function stopPolling() { if (tickTimer) { clearInterval(tickTimer); tickTimer = null; } }
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    stopPolling();
    closeSessionSSE();
    closeActiveLiveSSE();
  } else {
    startPolling();
    if (gwSubview === 'sessions' && activeTab === 'gateway') refreshSessionsView();
    if (activeModal.id) openActiveLiveView(activeModal.id); // reconnect modal SSE
  }
});
startPolling();
