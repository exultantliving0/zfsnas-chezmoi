package handlers

import (
	"net/http"
)

// ServeTerminalMultiPage serves the standalone tab-bar + xterm.js host that
// "Open in New Tab" and "Open in New Window" launchers point at. The page
// fetches the user's live sessions via /api/terminal-sessions, renders one
// tab per session, and listens on a BroadcastChannel('znas-terminal') for
// "add"/"focus"/"closed" messages from the main portal so multiple clicks
// of "New Tab" funnel into THIS one page rather than spawning fresh popups.
//
// The fixed window name 'znas-term-tab' (and 'znas-term-window' for the
// popup variant) is what makes browsers reuse this same page across
// repeated window.open() calls.
// GET /terminal-multi
func ServeTerminalMultiPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(terminalMultiPageHTML)) //nolint:errcheck
}

const terminalMultiPageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Z Terminals</title>
<!-- "Add to Home Screen" on iPadOS picks up these tags. Naming the
     bookmarked app "Z Terminals" with its own Tron-themed icon so the
     user can launch directly into the consolidated terminal page from
     their iPad home screen, no portal navigation needed. -->
<meta name="apple-mobile-web-app-title" content="Z Terminals">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<meta name="theme-color" content="#03071a">
<!-- iOS Safari ignores SVG in apple-touch-icon and silently falls back
     to /apple-touch-icon.png at the site root (the main ZNAS portal
     icon). Point at the PNG variant so "Add to Home Screen" picks up
     the Z Terminals art. Multiple sizes via apple-touch-icon-precomposed
     hints iOS to skip the default rounded mask + reflection. -->
<link rel="apple-touch-icon" sizes="180x180" href="/icons/z-terminals.png">
<link rel="apple-touch-icon-precomposed" sizes="180x180" href="/icons/z-terminals.png">
<link rel="icon" type="image/svg+xml" href="/icons/z-terminals.svg">
<link rel="icon" type="image/png" sizes="180x180" href="/icons/z-terminals.png">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.min.css">
<script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js"></script>
<style>
  :root {
    --bg-0:#0e1014; --bg-1:#161922; --bg-2:#1d2230;
    --bd:#262c3a; --text:#e8eaf0; --muted:#8e94a4;
    --accent:#3b82f6; --danger:#ef4444; --ok:#22c55e;
  }
  * { box-sizing:border-box; }
  html,body { height:100%; margin:0; background:var(--bg-0); color:var(--text); font-family:-apple-system,Segoe UI,Roboto,sans-serif; }
  body { display:flex; flex-direction:column; }
  #tabs {
    display:flex; align-items:stretch; background:var(--bg-1); border-bottom:1px solid var(--bd);
    flex:0 0 auto; overflow-x:auto; overflow-y:hidden; user-select:none; min-height:36px;
  }
  .tab {
    display:flex; align-items:center; gap:8px; padding:6px 10px 6px 12px;
    border-right:1px solid var(--bd); cursor:pointer; white-space:nowrap;
    font-size:13px; color:var(--muted); position:relative;
  }
  .tab:hover { background:var(--bg-2); }
  .tab.active { background:var(--bg-0); color:var(--text); }
  .tab .ic { font-size:12px; opacity:.85; }
  .tab .closex {
    display:inline-flex; align-items:center; justify-content:center;
    width:18px; height:18px; border-radius:4px; color:var(--muted);
    margin-left:4px; font-size:14px; line-height:1;
  }
  .tab .closex:hover { background:#444; color:#fff; }
  .tab .term-dot {
    width:6px; height:6px; border-radius:50%; background:var(--ok); display:inline-block;
  }
  .tab.terminated .term-dot { background:var(--muted); }
  #tabs .addbtn {
    padding:6px 12px; cursor:pointer; color:var(--muted); font-size:18px; line-height:1;
    border-right:1px solid var(--bd);
  }
  #tabs .addbtn:hover { color:var(--text); background:var(--bg-2); }
  #tabs-spacer { flex:1 1 auto; }
  #tabs .gearbtn {
    padding:6px 12px; cursor:pointer; color:var(--muted); font-size:14px; line-height:1;
    border-left:1px solid var(--bd); position:relative;
  }
  #tabs .gearbtn:hover { color:var(--text); background:var(--bg-2); }
  .gear-menu {
    position:fixed; z-index:1000; background:var(--bg-1); border:1px solid var(--bd);
    border-radius:8px; padding:6px; min-width:260px; box-shadow:0 8px 24px rgba(0,0,0,.5);
  }
  .gear-menu .row {
    display:flex; align-items:center; justify-content:space-between; gap:14px;
    padding:6px 10px; font-size:12px; color:var(--muted);
  }
  .gear-menu .pill {
    display:inline-flex; gap:2px; align-items:center;
    border:1px solid var(--bd); border-radius:6px; overflow:hidden;
  }
  .gear-menu .pill button {
    background:var(--bg-2); color:var(--text); border:0; cursor:pointer;
    padding:4px 8px; line-height:1; font-weight:600;
  }
  .gear-menu .pill button:hover { background:var(--bg-0); }
  .gear-menu .pill .smallA { font-size:10px; }
  .gear-menu .pill .bigA   { font-size:16px; }
  .gear-menu .pill .cur    { color:var(--muted); padding:0 8px; font-size:11px; font-family:monospace; }
  .gear-menu .divider      { height:1px; background:var(--bd); margin:4px 6px; }
  .gear-menu .theme-hdr    { padding:4px 10px; font-size:11px; text-transform:uppercase; letter-spacing:.06em; color:var(--muted); }
  .gear-menu .theme-row    { display:flex; align-items:center; gap:10px; padding:6px 10px; font-size:13px; color:var(--text); cursor:pointer; border-radius:4px; }
  .gear-menu .theme-row:hover { background:var(--bg-2); }
  .gear-menu .theme-row.active { background:var(--bg-2); }
  .gear-menu .swatch { width:24px; height:14px; border-radius:3px; border:1px solid var(--bd); position:relative; flex-shrink:0; }
  .gear-menu .swatch .fg { position:absolute; left:3px; top:3px; width:10px; height:1.5px; box-shadow:0 4px 0 0 currentColor; }
  .gear-menu .check { margin-left:auto; color:var(--accent); }
  #panes { flex:1 1 auto; position:relative; overflow:hidden; }
  .pane { position:absolute; inset:0; display:none; }
  .pane.active { display:block; }
  .pane .xt { position:absolute; inset:0; padding:6px; }
  .empty {
    position:absolute; inset:0; display:flex; align-items:center; justify-content:center;
    color:var(--muted); font-size:14px; text-align:center; padding:20px;
  }
  .reconnect-bar {
    position:absolute; left:0; right:0; top:0; padding:8px 12px; background:#3b2e16;
    color:#fde68a; font-size:12px; display:flex; align-items:center; gap:10px; z-index:5;
  }
  .reconnect-bar button {
    margin-left:auto; background:var(--accent); color:#fff; border:0; padding:6px 12px;
    border-radius:4px; cursor:pointer; font-size:12px;
  }
  #add-menu {
    position:absolute; top:36px; left:0; background:var(--bg-1); border:1px solid var(--bd);
    border-radius:6px; padding:6px; min-width:240px; box-shadow:0 8px 24px rgba(0,0,0,.5);
    z-index:50; display:none; max-height:60vh; overflow-y:auto;
  }
  #add-menu.show { display:block; }
  #add-menu .group-label {
    font-size:10px; text-transform:uppercase; color:var(--muted); padding:6px 10px 2px;
    letter-spacing:.06em;
  }
  #add-menu .item {
    padding:8px 10px; cursor:pointer; border-radius:4px; font-size:13px; display:flex;
    align-items:center; gap:8px;
  }
  #add-menu .item:hover { background:var(--bg-2); }
  #add-menu .item .ic { width:20px; text-align:center; opacity:.8; }
</style>
</head>
<body>
<div id="tabs">
  <div class="addbtn" id="add-btn" title="Open a new terminal">+</div>
  <div id="add-menu"></div>
  <div id="tabs-spacer"></div>
  <div class="gearbtn" id="gear-btn" title="Terminal settings">⚙</div>
</div>
<div id="panes">
  <div class="empty" id="empty-state">No terminal sessions. Click <b>+</b> to start one.</div>
</div>

<script>
// ── Session model ──────────────────────────────────────────────────────────
// Each tab maps to one termsessions.Session on the server. The browser
// holds an xterm.js instance + a WebSocket per tab; close-and-reopen here
// just re-attaches because the PTY is owned by the server.

const TABS = []; // {id, kind, target, title, terminated, term, ws, fit, paneEl, tabEl}
let activeIdx = -1;

const tabsEl  = document.getElementById('tabs');
const panesEl = document.getElementById('panes');
const emptyEl = document.getElementById('empty-state');
const addBtn  = document.getElementById('add-btn');
const addMenu = document.getElementById('add-menu');

addBtn.addEventListener('click', e => {
  e.stopPropagation();
  if (addMenu.classList.contains('show')) { addMenu.classList.remove('show'); return; }
  buildAddMenu();
});
document.addEventListener('click', () => {
  addMenu.classList.remove('show');
  gearMenu.classList.remove('show');
});

// ── Gear menu / font size + theme ──────────────────────────────────────────
const gearBtn = document.getElementById('gear-btn');
const FONT_MIN = 9, FONT_MAX = 28, FONT_DEFAULT = 13, FONT_KEY = 'znas-term-font-size';
const THEME_KEY = 'znas-term-theme';

const THEMES = {
  'znas-dark': { label: 'ZNAS Dark', theme: {
    background:'#0d0d0f', foreground:'#e2e2e6', cursor:'#7c7cff',
    selectionBackground:'rgba(124,124,255,0.3)',
    black:'#1a1a1f', red:'#ff5f57', green:'#28ca42', yellow:'#ffbe2e',
    blue:'#7c7cff', magenta:'#bf5af2', cyan:'#5ac8fa', white:'#e2e2e6',
    brightBlack:'#6e6e80', brightRed:'#ff6b6b', brightGreen:'#5cffa3',
    brightYellow:'#ffd60a', brightBlue:'#9595ff', brightMagenta:'#da8fff',
    brightCyan:'#70d7ff', brightWhite:'#ffffff',
  }},
  'tron': { label: 'Tron', theme: {
    background:'#000810', foreground:'#7df9ff', cursor:'#00f0ff',
    selectionBackground:'rgba(0,240,255,0.25)',
    black:'#000810', red:'#ff4b8a', green:'#39ff14', yellow:'#ffd700',
    blue:'#0066ff', magenta:'#ff00ff', cyan:'#00f0ff', white:'#7df9ff',
    brightBlack:'#0a2a4a', brightRed:'#ff80b0', brightGreen:'#70ff60',
    brightYellow:'#ffe066', brightBlue:'#4d99ff', brightMagenta:'#ff80ff',
    brightCyan:'#80f8ff', brightWhite:'#ccffff',
  }},
  'solarized-dark': { label: 'Solarized Dark', theme: {
    background:'#002b36', foreground:'#839496', cursor:'#93a1a1',
    selectionBackground:'rgba(131,148,150,0.25)',
    black:'#073642', red:'#dc322f', green:'#859900', yellow:'#b58900',
    blue:'#268bd2', magenta:'#d33682', cyan:'#2aa198', white:'#eee8d5',
    brightBlack:'#586e75', brightRed:'#cb4b16', brightGreen:'#586e75',
    brightYellow:'#657b83', brightBlue:'#839496', brightMagenta:'#6c71c4',
    brightCyan:'#93a1a1', brightWhite:'#fdf6e3',
  }},
  'dracula': { label: 'Dracula', theme: {
    background:'#282a36', foreground:'#f8f8f2', cursor:'#bd93f9',
    selectionBackground:'rgba(189,147,249,0.3)',
    black:'#21222c', red:'#ff5555', green:'#50fa7b', yellow:'#f1fa8c',
    blue:'#bd93f9', magenta:'#ff79c6', cyan:'#8be9fd', white:'#f8f8f2',
    brightBlack:'#6272a4', brightRed:'#ff6e6e', brightGreen:'#69ff94',
    brightYellow:'#ffffa5', brightBlue:'#d6acff', brightMagenta:'#ff92df',
    brightCyan:'#a4ffff', brightWhite:'#ffffff',
  }},
  'matrix': { label: 'Matrix', theme: {
    background:'#0d0208', foreground:'#00ff41', cursor:'#00ff41',
    selectionBackground:'rgba(0,255,65,0.25)',
    black:'#0d0208', red:'#a30000', green:'#008f11', yellow:'#557a47',
    blue:'#073642', magenta:'#5e35b1', cyan:'#00bcd4', white:'#00ff41',
    brightBlack:'#1e6f1e', brightRed:'#ff6b6b', brightGreen:'#00ff41',
    brightYellow:'#aaff66', brightBlue:'#268bd2', brightMagenta:'#9a55c4',
    brightCyan:'#33ffff', brightWhite:'#ccffaa',
  }},
  'light': { label: 'Light', theme: {
    background:'#fafafa', foreground:'#1a1a1f', cursor:'#0066ff',
    selectionBackground:'rgba(0,102,255,0.18)',
    black:'#000000', red:'#c91b00', green:'#00c200', yellow:'#c7c400',
    blue:'#0037da', magenta:'#c930c7', cyan:'#00c5c7', white:'#c7c7c7',
    brightBlack:'#676767', brightRed:'#ff6d67', brightGreen:'#5ff967',
    brightYellow:'#fefb67', brightBlue:'#6871ff', brightMagenta:'#ff76ff',
    brightCyan:'#5ffdff', brightWhite:'#feffff',
  }},
};

function getFontSize() {
  const v = parseInt(localStorage.getItem(FONT_KEY) || '', 10);
  return (v >= FONT_MIN && v <= FONT_MAX) ? v : FONT_DEFAULT;
}
function setFontSize(px) {
  px = Math.max(FONT_MIN, Math.min(FONT_MAX, px|0));
  try { localStorage.setItem(FONT_KEY, String(px)); } catch (_) {}
  TABS.forEach(t => { if (t.term) { try { t.term.options.fontSize = px; } catch (_) {} } });
  // xterm's renderer recomputes cell metrics asynchronously after the
  // fontSize change. Double-rAF + offsetHeight read so fit() measures
  // against the new cell size; then push dims to the PTY.
  requestAnimationFrame(() => requestAnimationFrame(() => {
    TABS.forEach(t => {
      if (!t.term || !t.paneEl) return;
      void t.paneEl.offsetHeight;
      try { t.fit && t.fit.fit(); } catch (_) {}
      try { sendResize(t); } catch (_) {}
    });
  }));
}

function getThemeKey() {
  const k = localStorage.getItem(THEME_KEY) || '';
  return THEMES[k] ? k : 'znas-dark';
}
function getTheme() { return THEMES[getThemeKey()].theme; }
function setTheme(key) {
  if (!THEMES[key]) return;
  try { localStorage.setItem(THEME_KEY, key); } catch (_) {}
  const theme = THEMES[key].theme;
  TABS.forEach(t => { if (t.term) { try { t.term.options.theme = theme; } catch (_) {} } });
}

function openGearMenu() {
  const existing = document.getElementById('gear-menu');
  if (existing) { existing.remove(); return; }
  const rect = gearBtn.getBoundingClientRect();
  const menu = document.createElement('div');
  menu.id = 'gear-menu';
  menu.className = 'gear-menu';
  menu.style.visibility = 'hidden';

  // Font row
  const fr = document.createElement('div');
  fr.className = 'row';
  fr.innerHTML =
    '<span>Font size</span>'
    + '<span class="pill">'
    + '  <button class="smallA" data-act="-">A</button>'
    + '  <button class="bigA"   data-act="+">A</button>'
    + '  <span class="cur"></span>'
    + '</span>';
  fr.querySelector('[data-act="-"]').onclick = (e) => { e.stopPropagation(); setFontSize(getFontSize() - 1); fr.querySelector('.cur').textContent = getFontSize() + ' px'; };
  fr.querySelector('[data-act="+"]').onclick = (e) => { e.stopPropagation(); setFontSize(getFontSize() + 1); fr.querySelector('.cur').textContent = getFontSize() + ' px'; };
  fr.querySelector('.cur').textContent = getFontSize() + ' px';
  menu.appendChild(fr);

  // Divider
  const div = document.createElement('div'); div.className = 'divider'; menu.appendChild(div);

  // Theme picker
  const hdr = document.createElement('div'); hdr.className = 'theme-hdr'; hdr.textContent = 'Theme'; menu.appendChild(hdr);
  const currentKey = getThemeKey();
  Object.keys(THEMES).forEach(key => {
    const t = THEMES[key];
    const row = document.createElement('div');
    row.className = 'theme-row' + (key === currentKey ? ' active' : '');
    const sw = document.createElement('span');
    sw.className = 'swatch';
    sw.style.background = t.theme.background;
    sw.style.color = t.theme.foreground; // for the fg bars via currentColor
    sw.innerHTML = '<span class="fg" style="background:' + t.theme.foreground + ';"></span>';
    row.appendChild(sw);
    const lbl = document.createElement('span');
    lbl.textContent = t.label;
    row.appendChild(lbl);
    if (key === currentKey) {
      const c = document.createElement('span'); c.className = 'check'; c.textContent = '✓'; row.appendChild(c);
    }
    row.addEventListener('click', (e) => { e.stopPropagation(); setTheme(key); menu.remove(); });
    menu.appendChild(row);
  });

  document.body.appendChild(menu);

  // Position — align right edge with the gear button, drop below.
  const mw = menu.getBoundingClientRect().width;
  const mh = menu.getBoundingClientRect().height;
  let left = rect.right - mw;
  if (left < 8) left = 8;
  let top = rect.bottom + 6;
  if (top + mh > window.innerHeight - 8) top = Math.max(8, rect.top - mh - 6);
  menu.style.left = left + 'px';
  menu.style.top  = top + 'px';
  menu.style.visibility = '';

  // Outside-click + Esc to close.
  const close = () => {
    menu.remove();
    document.removeEventListener('mousedown', outside, true);
    document.removeEventListener('keydown', esc, true);
  };
  const outside = (e) => { if (!menu.contains(e.target) && e.target.id !== 'gear-btn') close(); };
  const esc = (e) => { if (e.key === 'Escape') close(); };
  setTimeout(() => {
    document.addEventListener('mousedown', outside, true);
    document.addEventListener('keydown', esc, true);
  }, 0);
}
gearBtn.addEventListener('click', (e) => { e.stopPropagation(); openGearMenu(); });

function esc(s){ return String(s||'').replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }

function setEmpty(visible) { emptyEl.style.display = visible ? 'flex' : 'none'; }

function renderTabBar() {
  // Remove existing tab elements (preserve + button + menu + spacer + gear).
  Array.from(tabsEl.querySelectorAll('.tab')).forEach(n => n.remove());
  // Tabs must sit BETWEEN the add-menu and the flex spacer so the gear
  // stays alone on the right. Without this they'd append after the gear.
  const spacer = document.getElementById('tabs-spacer');
  TABS.forEach((t, i) => {
    const el = document.createElement('div');
    el.className = 'tab' + (i === activeIdx ? ' active' : '') + (t.terminated ? ' terminated' : '');
    el.innerHTML = '<span class="term-dot"></span>'
                 + '<span class="ic">' + kindIcon(t.kind) + '</span>'
                 + '<span class="ttl">' + esc(t.title || t.kind) + '</span>'
                 + '<span class="closex" title="Close session">×</span>';
    el.addEventListener('click', e => {
      if (e.target.classList.contains('closex')) {
        closeTab(i);
      } else {
        try { localStorage.setItem('znas-term-last-spec', _tabSpec(TABS[i])); } catch (_) {}
        activateTab(i);
      }
    });
    t.tabEl = el;
    tabsEl.insertBefore(el, spacer);
  });
  setEmpty(TABS.length === 0);
}

function kindIcon(k) {
  switch (k) {
    case 'lxd':     return '💻';
    case 'compose': return '📦';
    case 'docker':  return '🐳';
    case 'host':    return '🖥';
  }
  return '⌨';
}

function activateTab(i) {
  if (i < 0 || i >= TABS.length) return;
  activeIdx = i;
  TABS.forEach((t, j) => {
    if (t.paneEl) t.paneEl.classList.toggle('active', i === j);
  });
  Array.from(tabsEl.querySelectorAll('.tab')).forEach((n, j) => {
    n.classList.toggle('active', i === j);
  });
  // Now that the pane is visible, re-measure and *push the real size to
  // the server*. Without this, panes mounted hidden (e.g. on initial
  // load) stay locked at xterm's 80×24 default because that's what
  // term.open(el) measured — fixing fit() alone isn't enough; the PTY
  // also needs to be told. Use double-rAF: setTimeout(0) fires BEFORE
  // the browser has committed layout for the display:block→active
  // transition, so fit.fit() reads stale dimensions and term.resize is
  // a no-op. Two animation frames guarantee the new layout is final.
  requestAnimationFrame(() => requestAnimationFrame(() => {
    const tab = TABS[i];
    if (!tab) return;
    // Force a synchronous layout read so xterm renderer sees real dims.
    if (tab.paneEl) void tab.paneEl.offsetHeight;
    try { tab.fit && tab.fit.fit(); } catch (_) {}
    try { sendResize(tab); } catch (_) {}
    try { tab.term && tab.term.focus(); } catch (_) {}
  }));
}

function closeTab(i) {
  const t = TABS[i];
  if (!t) return;
  fetch('/api/terminal-sessions/' + encodeURIComponent(t.id) + '/close', {method:'POST', credentials:'same-origin'})
    .catch(()=>{});
  if (t.ws) try { t.ws.close(); } catch{}
  if (t.term) try { t.term.dispose(); } catch{}
  if (t.paneEl) t.paneEl.remove();
  TABS.splice(i, 1);
  if (activeIdx >= TABS.length) activeIdx = TABS.length - 1;
  renderTabBar();
  if (activeIdx >= 0) activateTab(activeIdx);
  try { bc.postMessage({type:'closed', id:t.id}); } catch{}
}

// ── WebSocket attach for one tab ───────────────────────────────────────────

function buildPane(tab) {
  const pane = document.createElement('div');
  pane.className = 'pane';
  const xt = document.createElement('div');
  xt.className = 'xt';
  pane.appendChild(xt);
  panesEl.appendChild(pane);
  tab.paneEl = pane;

  const term = new Terminal({
    fontSize: getFontSize(),
    fontFamily: '"SF Mono",Menlo,Consolas,monospace',
    theme: getTheme(),
    scrollback: 5000,
    convertEol: false,
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(xt);
  try { fit.fit(); } catch{}
  tab.term = term;
  tab.fit  = fit;

  window.addEventListener('resize', () => { try { fit.fit(); sendResize(tab); } catch{} });
}

function sendResize(tab) {
  if (!tab.ws || tab.ws.readyState !== 1) return;
  // Re-fit before reading cols/rows so hidden→visible transitions push
  // the actually-rendered dimensions to the PTY, not the stale 80×24
  // default xterm picked when the pane was first mounted hidden.
  try { tab.fit && tab.fit.fit(); } catch (_) {}
  const t = tab.term;
  try { tab.ws.send(JSON.stringify({type:'resize', cols:t.cols, rows:t.rows})); } catch{}
}

function attachTab(tab, opts) {
  opts = opts || {};
  if (!tab.paneEl) buildPane(tab);
  const term = tab.term;
  if (opts.reset) { term.reset(); term.write('\r\n[connecting…]\r\n'); }

  let path = '';
  if (tab.serverId) {
    // Tabs targeting a linked peer go through the local
    // /ws/interlink-terminal bridge which forwards HMAC-signed frames
    // to the peer's per-kind WS endpoint.
    path = '/ws/interlink-terminal?server_id=' + encodeURIComponent(tab.serverId)
         + '&kind=' + encodeURIComponent(tab.kind)
         + '&target=' + encodeURIComponent(tab.target || '');
  } else {
    switch (tab.kind) {
      case 'host':    path = '/ws/terminal'; break;
      case 'lxd':     path = '/ws/lxd-console?name=' + encodeURIComponent(tab.target); break;
      case 'compose': {
        const [stack, container] = tab.target.split(':');
        path = '/ws/compose-console?stack=' + encodeURIComponent(stack) + '&container=' + encodeURIComponent(container);
        break;
      }
      case 'docker': {
        const [instance, container] = tab.target.split(':');
        path = '/ws/docker-console?instance=' + encodeURIComponent(instance) + '&container=' + encodeURIComponent(container);
        break;
      }
    }
  }
  const sep = path.includes('?') ? '&' : '?';
  let url = path + sep + 'cols=' + term.cols + '&rows=' + term.rows;
  if (tab.id) url += '&session_id=' + encodeURIComponent(tab.id);

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(proto + '//' + location.host + url);
  ws.binaryType = 'arraybuffer';
  tab.ws = ws;

  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      try {
        const msg = JSON.parse(ev.data);
        if (msg.type === 'session' && msg.id) {
          tab.id = msg.id;
          renderTabBar();
        } else if (msg.type === 'error') {
          term.write('\r\n[error: ' + (msg.error||'unknown') + ']\r\n');
        }
        return;
      } catch{}
      term.write(ev.data);
    } else {
      const u8 = new Uint8Array(ev.data);
      term.write(u8);
    }
  };
  ws.onclose = () => {
    tab.ws = null;
    // Don't auto-reconnect — surface the disconnect.
  };
  term.onData(d => { if (ws.readyState === 1) ws.send(d); });
  // First resize after open.
  ws.addEventListener('open', () => { sendResize(tab); });
}

// ── Bootstrap: list existing sessions, build tabs ──────────────────────────

async function loadExistingSessions() {
  try {
    // /aggregate returns local sessions PLUS every linked InterLink
    // peer's sessions for the same user, each tagged with server_id +
    // server_hostname. Restoring from this endpoint is what makes the
    // popup carry all the user's terminals (including remote ones)
    // across a window close → reopen.
    const r = await fetch('/api/terminal-sessions/aggregate', {credentials:'same-origin'});
    if (!r.ok) return;
    const list = await r.json();

    // Track which existing snapshot was most-recently active so we can
    // land the user on the right tab, same idea as the bottom terminal.
    let mostRecentTs = '', mostRecentIdx = -1;
    list.forEach((s, idx) => {
      if (s.terminated) return;
      if (s.last_active && s.last_active > mostRecentTs) {
        mostRecentTs = s.last_active;
        mostRecentIdx = idx;
      }
      addTabFromSnapshot(s);
    });

    if (TABS.length > 0) {
      // Prefer the user's persisted last-used spec if we restored a tab
      // matching it; otherwise pick the most-recently active session.
      let pickIdx = -1;
      try {
        const lastSpec = localStorage.getItem('znas-term-last-spec');
        if (lastSpec) {
          pickIdx = TABS.findIndex(t => _tabSpec(t) === lastSpec);
        }
      } catch (_) {}
      if (pickIdx < 0 && mostRecentIdx >= 0) {
        // mostRecentIdx is into the snapshot list; find the matching live tab.
        const ms = list[mostRecentIdx];
        const key = _snapshotKey(ms);
        pickIdx = TABS.findIndex(t => _snapshotKey({
          id: t.id, server_id: t.serverId, is_local: !t.serverId,
        }) === key);
      }
      activateTab(pickIdx >= 0 ? pickIdx : 0);
    }
  } catch{}
}

function _tabSpec(t) {
  return (t.serverId ? ('peer:' + t.serverId + ':') : '') + t.kind + (t.target ? (':' + t.target) : '');
}
function _snapshotKey(s) {
  return (s.is_local ? '' : ('peer:' + s.server_id + ':')) + s.id;
}

function addTabFromSnapshot(s) {
  // Build the same human label the bottom terminal uses: peer tabs get
  // a "hostname: …" prefix so users can tell at a glance which server
  // each shell lives on.
  const title = s.is_local
    ? (s.title || s.target || s.kind)
    : ((s.server_hostname || 'remote') + ': ' + (s.title || s.target || s.kind));
  const tab = {
    id: s.id,
    kind: s.kind,
    target: s.target,
    serverId: s.is_local ? '' : (s.server_id || ''),
    title,
    terminated: s.terminated,
  };
  TABS.push(tab);
  renderTabBar();
  buildPane(tab);
  if (!s.terminated) attachTab(tab, {reset:true});
}

function addTabFromSpec(spec, displayTitle) {
  // spec forms:
  //   host                       — local host shell
  //   lxd:NAME                   — local LXD/Incus instance
  //   compose:STACK:CONTAINER    — local podman compose container
  //   docker:INST:CONTAINER      — local docker-in-VM container
  //   peer:SID:host              — host shell on linked peer SID
  //   peer:SID:lxd:NAME          — LXD instance on linked peer SID
  let serverId = '';
  let kind, target;
  if (spec.startsWith('peer:')) {
    const parts = spec.split(':');           // ["peer", SID, KIND, ...]
    serverId = parts[1] || '';
    kind     = parts[2] || 'host';
    target   = parts.slice(3).join(':');
  } else {
    const idx = spec.indexOf(':');
    kind   = idx === -1 ? spec : spec.slice(0, idx);
    target = idx === -1 ? '' : spec.slice(idx+1);
  }
  // Avoid duplicating a tab for the same (server, kind, target) if one
  // is already attached.
  const existing = TABS.findIndex(t => (t.serverId||'') === serverId
                                    && t.kind === kind
                                    && t.target === target
                                    && !t.terminated);
  if (existing >= 0) { activateTab(existing); return; }
  const tab = { id:'', kind, target, serverId,
                title: displayTitle || target || kind, terminated:false };
  TABS.push(tab);
  renderTabBar();
  buildPane(tab);
  activateTab(TABS.length - 1);
  attachTab(tab, {reset:true});
}

// ── Add-menu population ────────────────────────────────────────────────────

async function buildAddMenu() {
  addMenu.innerHTML = '<div class="group-label">Loading…</div>';
  addMenu.classList.add('show');

  // v6.5.30 — list local server + every online linked InterLink peer,
  // each with its own "Host shell" entry plus one row per *running* VM
  // and container. The "+" menu is the single jumping-off point for
  // every terminal target reachable from this portal.
  let localHostname = '';
  try {
    const v = await fetch('/api/version', {credentials:'same-origin'}).then(r => r.ok ? r.json() : null);
    if (v && v.hostname) localHostname = v.hostname;
  } catch {}
  const peers = await fetch('/api/interlink/servers', {credentials:'same-origin'})
                  .then(r => r.ok ? r.json() : []).catch(() => []);

  // Fetch local + each online peer's instances in parallel.
  const onlinePeers = (peers || []).filter(p => p.online);
  // Local instances come from /api/lxd/instances which returns full
  // LXDInstance objects: state lives on .status, type on .type. The
  // peer endpoint returns LXDInstanceSummary with .state + .type
  // (extended in v6.5.30 specifically so this menu could filter to
  // running) — normalise the local payload to the same shape below.
  const promises = [
    fetch('/api/lxd/instances', {credentials:'same-origin'})
      .then(r => r.ok ? r.json() : [])
      .then(list => ({
        serverId: '',
        hostname: localHostname || 'this server',
        instances: (list || []).map(i => ({
          name: i.name,
          description: i.description,
          type: i.type,
          state: i.status || i.state || '',
        })),
      }))
      .catch(() => ({serverId: '', hostname: localHostname || 'this server', instances: []})),
  ];
  for (const p of onlinePeers) {
    promises.push(
      fetch('/api/interlink/remote-lxd-instances/' + encodeURIComponent(p.id), {credentials:'same-origin'})
        .then(r => r.ok ? r.json() : {instances:[]})
        .then(data => ({serverId: p.id, hostname: p.hostname, instances: (data && data.instances) || []}))
        .catch(() => ({serverId: p.id, hostname: p.hostname, instances: []}))
    );
  }
  const results = await Promise.all(promises);

  addMenu.innerHTML = '';
  results.forEach(srv => {
    // Header for this server.
    const lbl = document.createElement('div');
    lbl.className = 'group-label';
    lbl.textContent = (srv.serverId ? '🔗 ' : '🖥 ') + (srv.hostname || 'server');
    addMenu.appendChild(lbl);
    const hostnamePrefix = srv.serverId ? (srv.hostname + ': ') : '';
    // Host shell on this server.
    const specHost = srv.serverId ? ('peer:' + srv.serverId + ':host') : 'host';
    addMenu.appendChild(_mkAddMenuItem('🖥', 'Host shell', specHost, hostnamePrefix + 'Host shell'));
    // Running instances. Filter to Running so we don't try to open WS
    // against a stopped container — incus exec would just error.
    const running = (srv.instances || []).filter(i => (i.state || '').toLowerCase() === 'running');
    running.sort((a, b) => (a.name || '').localeCompare(b.name || ''));
    running.forEach(inst => {
      const icon = inst.type === 'virtual-machine' ? '💻' : '📦';
      const spec = srv.serverId
        ? ('peer:' + srv.serverId + ':lxd:' + inst.name)
        : ('lxd:' + inst.name);
      addMenu.appendChild(_mkAddMenuItem(icon, inst.name, spec, hostnamePrefix + inst.name));
    });
    if (running.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'item';
      empty.style.opacity = '0.5';
      empty.style.fontStyle = 'italic';
      empty.style.cursor = 'default';
      empty.textContent = 'no running VMs/containers';
      addMenu.appendChild(empty);
    }
  });
}

function _mkAddMenuItem(icon, label, spec, tabTitle) {
  const el = document.createElement('div');
  el.className = 'item';
  el.innerHTML = '<span class="ic">' + (icon || '⌨') + '</span><span>' + esc(label) + '</span>';
  el.addEventListener('click', () => {
    addMenu.classList.remove('show');
    addTabFromSpec(spec, tabTitle);
  });
  return el;
}

// ── Cross-tab messaging from the main portal ───────────────────────────────

const bc = ('BroadcastChannel' in window) ? new BroadcastChannel('znas-terminal') : null;
if (bc) {
  bc.onmessage = (ev) => {
    const m = ev.data || {};
    if (m.type === 'add' && m.spec) { addTabFromSpec(m.spec); window.focus(); }
    if (m.type === 'focus')          { window.focus(); }
    if (m.type === 'closed' && m.id) {
      const i = TABS.findIndex(t => t.id === m.id);
      if (i >= 0) { closeTab(i); }
    }
  };
}

// On load: query string ?add=spec adds one immediately; hash variant
// #add=spec also supported (used by the main portal so a reload isn't
// needed when this page is already open).
function processAddFromURL() {
  const url = new URL(location.href);
  const fromQuery = url.searchParams.get('add');
  const fromHash  = (location.hash.match(/[#&]add=([^&]+)/) || [])[1];
  const spec = fromQuery || (fromHash ? decodeURIComponent(fromHash) : '');
  if (spec) addTabFromSpec(spec);
  // Clean URL so a refresh doesn't re-add.
  if (fromQuery) {
    url.searchParams.delete('add');
    history.replaceState({}, '', url.pathname + url.hash);
  }
  if (fromHash) {
    history.replaceState({}, '', location.pathname + location.search);
  }
}
window.addEventListener('hashchange', processAddFromURL);

loadExistingSessions().then(processAddFromURL);
</script>
</body>
</html>
`
