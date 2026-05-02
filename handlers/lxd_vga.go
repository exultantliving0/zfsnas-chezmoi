package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

var lxdVGAUpgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"binary"},
}

func lxdSocketPath() string {
	for _, p := range []string{
		"/var/snap/lxd/common/lxd/unix.socket",
		"/var/lib/lxd/unix.socket",
		"/run/lxd.socket",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func startLXDVGAOp(name string) (opID, dataSecret, ctrlSecret string, err error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("lxc", "query", "-X", "POST",
		"--data", `{"type":"vga","width":1024,"height":768}`,
		"/1.0/instances/"+name+"/console",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", "", "", fmt.Errorf("start VGA console: %s", msg)
	}
	// lxc query returns the operation object directly:
	// { "id": "<uuid>", "metadata": { "fds": { "0": "<secret>", "control": "<secret>" } } }
	var resp struct {
		ID       string `json:"id"`
		Metadata struct {
			FDs map[string]string `json:"fds"`
		} `json:"metadata"`
	}
	if err = json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return "", "", "", fmt.Errorf("parse LXD response: %w", err)
	}
	opID = resp.ID
	if opID == "" {
		return "", "", "", fmt.Errorf("unexpected LXD response (no operation ID)")
	}
	dataSecret = resp.Metadata.FDs["0"]
	if dataSecret == "" {
		return "", "", "", fmt.Errorf("no SPICE secret in LXD response; is this a VM?")
	}
	ctrlSecret = resp.Metadata.FDs["control"]
	return opID, dataSecret, ctrlSecret, nil
}

// HandleLXDVGAConsole proxies the LXD VGA SPICE channel to the browser over WebSocket.
// GET /ws/lxd-vga?name=<name>
func HandleLXDVGAConsole(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	socketPath := lxdSocketPath()
	if socketPath == "" {
		http.Error(w, "LXD Unix socket not found", http.StatusInternalServerError)
		return
	}
	log.Printf("[vga] socket=%s name=%s", socketPath, name)

	opID, dataSecret, ctrlSecret, err := startLXDVGAOp(name)
	if err != nil {
		log.Printf("[vga] startLXDVGAOp error: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("[vga] opID=%s secretLen=%d ctrlLen=%d", opID, len(dataSecret), len(ctrlSecret))

	lxdDialer := websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
		HandshakeTimeout: 10 * time.Second,
	}

	// LXD requires the control channel to be connected alongside the data channel;
	// without it LXD closes the data channel immediately.
	if ctrlSecret != "" {
		ctrlURL := "ws://localhost/1.0/operations/" + opID + "/websocket?secret=" + ctrlSecret
		ctrlWS, _, ctrlErr := lxdDialer.Dial(ctrlURL, nil)
		if ctrlErr != nil {
			log.Printf("[vga] dial LXD control WS error: %v", ctrlErr)
		} else {
			log.Printf("[vga] LXD control WS connected")
			defer ctrlWS.Close()
			// Drain control messages in background; no data proxied to browser.
			go func() {
				for {
					if _, _, e := ctrlWS.ReadMessage(); e != nil {
						return
					}
				}
			}()
		}
	}

	// Connect to LXD's SPICE data channel via the Unix socket.
	wsURL := "ws://localhost/1.0/operations/" + opID + "/websocket?secret=" + dataSecret
	lxdWS, lxdResp, err := lxdDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("[vga] dial LXD data WS error: %v (resp=%v)", err, lxdResp)
		http.Error(w, "connect to LXD: "+err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("[vga] LXD data WS connected")
	defer lxdWS.Close()

	browserWS, err := lxdVGAUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[vga] browser upgrade error: %v", err)
		return
	}
	log.Printf("[vga] browser WS upgraded, proxying")
	defer browserWS.Close()

	var once sync.Once
	done := make(chan struct{})

	// LXD → browser
	go func() {
		defer once.Do(func() { close(done) })
		for {
			mt, msg, err := lxdWS.ReadMessage()
			if err != nil {
				log.Printf("[vga] lxd→browser read err: %v", err)
				return
			}
			log.Printf("[vga] lxd→browser: type=%d len=%d", mt, len(msg))
			if err := browserWS.WriteMessage(mt, msg); err != nil {
				log.Printf("[vga] lxd→browser write err: %v", err)
				return
			}
		}
	}()
	// browser → LXD
	go func() {
		defer once.Do(func() { close(done) })
		for {
			mt, msg, err := browserWS.ReadMessage()
			if err != nil {
				log.Printf("[vga] browser→lxd read err: %v", err)
				return
			}
			log.Printf("[vga] browser→lxd: type=%d len=%d", mt, len(msg))
			if err := lxdWS.WriteMessage(mt, msg); err != nil {
				log.Printf("[vga] browser→lxd write err: %v", err)
				return
			}
		}
	}()

	<-done
	log.Printf("[vga] proxy done for %s", name)
}

// ServeLXDVGAPage serves the VGA console viewer using spice-html5.
// GET /lxd-vga-console/{name}
func ServeLXDVGAPage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, lxdVGAPageHTML, name, name, name)
}

const lxdVGAPageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>VGA Console — %s</title>
<style>
* { margin:0; padding:0; box-sizing:border-box; }
html, body { width:100%%; height:100%%; background:#000; overflow:hidden; display:flex; flex-direction:column; font-family:system-ui,sans-serif; }
#toolbar { background:#1a1a1a; border-bottom:1px solid #2a2a2a; padding:6px 14px; display:flex; align-items:center; gap:10px; flex-shrink:0; font-size:13px; color:#bbb; }
#toolbar strong { color:#fff; }
#toolbar button { background:#2a2a2a; color:#ccc; border:1px solid #444; border-radius:4px; padding:3px 10px; cursor:pointer; font-size:12px; }
#toolbar button:hover { background:#3a3a3a; color:#fff; }
#status { margin-left:auto; font-size:12px; color:#666; }
#kbd-wrap { position:relative; display:inline-block; }
#kbd-menu { display:none; position:absolute; top:calc(100%% + 4px); left:0; background:#2a2a2a; border:1px solid #444; border-radius:4px; min-width:170px; z-index:100; box-shadow:0 4px 12px rgba(0,0,0,.6); }
.kbd-item { padding:7px 14px; cursor:pointer; font-size:12px; color:#ccc; white-space:nowrap; }
.kbd-item:hover { background:#3a3a3a; color:#fff; }
#status.ok   { color:#4ade80; }
#status.err  { color:#f87171; }
#status.warn { color:#fbbf24; }
#spice-area { flex:1; overflow:hidden; position:relative; display:flex; align-items:center; justify-content:center; }
#spice-area canvas { display:block; }
#notice { position:absolute; color:#666; font-size:13px; text-align:center; line-height:1.7; pointer-events:none; }
/* Pointer-style overrides applied to the SPICE canvas. spice-html5 sets an
   inline cursor style on every frame (often "none" when the guest doesn't
   send cursor data — which is why the pointer can vanish entirely on VMs
   without a SPICE agent or qxl driver). !important is required to keep our
   choice visible. The guest OS may also draw its own cursor; these styles
   only affect the host-side pointer the browser renders over the canvas. */
#spice-area.cur-default canvas    { cursor: default !important; }
#spice-area.cur-crosshair canvas  { cursor: crosshair !important; }
#spice-area.cur-cell canvas       { cursor: cell !important; }
#spice-area.cur-hidden canvas     { cursor: none !important; }
/* "Large arrow" — 32x32 SVG embedded inline so it scales up the host pointer
   for high-DPI / TV displays without a separate asset. */
#spice-area.cur-large canvas {
  cursor: url("data:image/svg+xml;utf8,%%3Csvg xmlns='http://www.w3.org/2000/svg' width='32' height='32' viewBox='0 0 32 32'%%3E%%3Cpath d='M4 2 L4 26 L11 19 L15 28 L19 26 L15 18 L24 18 Z' fill='%%23fff' stroke='%%23000' stroke-width='1.5' stroke-linejoin='round'/%%3E%%3C/svg%%3E") 4 2, default !important;
}
/* "Dot" — a 14x14 ring with a black core, drawn at the hot-spot. Helpful for
   pixel-precise positioning when the guest cursor is a thin line and gets
   lost against the desktop. */
#spice-area.cur-dot canvas {
  cursor: url("data:image/svg+xml;utf8,%%3Csvg xmlns='http://www.w3.org/2000/svg' width='14' height='14' viewBox='0 0 14 14'%%3E%%3Ccircle cx='7' cy='7' r='5.5' fill='none' stroke='%%23fff' stroke-width='2'/%%3E%%3Ccircle cx='7' cy='7' r='1.5' fill='%%23000'/%%3E%%3C/svg%%3E") 7 7, crosshair !important;
}
#cdrom-menu .kbd-item.current { background:#1a3a1a; color:#9be39b; }
#cdrom-menu .kbd-item.eject   { color:#fbbf24; }
#cdrom-menu .kbd-item.muted   { color:#666; cursor:default; }
#cdrom-menu .kbd-item.muted:hover { background:#2a2a2a; color:#666; }
#cdrom-menu .kbd-sub { padding:6px 14px; font-size:11px; color:#8a8a8a; border-top:1px solid #3a3a3a; }
#cdrom-btn.has-disc { color:#9be39b; }
</style>
</head>
<body>
<div id="toolbar">
  <span>VGA Console — <strong>%s</strong></span>
  <button onclick="reconnect()">Reconnect</button>
  <div id="kbd-wrap">
    <button onclick="toggleKbdMenu(event)" title="Send keyboard shortcut">⌨</button>
    <div id="kbd-menu">
      <div class="kbd-item" onclick="kbdCtrlAltDel()">Ctrl+Alt+Delete</div>
    </div>
  </div>
  <div id="cdrom-wrap" style="position:relative;display:none;">
    <button id="cdrom-btn" onclick="toggleCDROMMenu(event)" title="CD/DVD drive">💿</button>
    <div id="cdrom-menu" style="display:none;position:absolute;top:calc(100%% + 4px);left:0;background:#2a2a2a;border:1px solid #444;border-radius:4px;min-width:240px;max-width:340px;max-height:60vh;overflow-y:auto;z-index:100;box-shadow:0 4px 12px rgba(0,0,0,.6);"></div>
  </div>
  <div id="cur-wrap" style="position:relative;display:inline-block;">
    <button onclick="toggleCursorMenu(event)" title="Pointer style">🖱</button>
    <div id="cur-menu" style="display:none;position:absolute;top:calc(100%% + 4px);left:0;background:#2a2a2a;border:1px solid #444;border-radius:4px;min-width:170px;z-index:100;box-shadow:0 4px 12px rgba(0,0,0,.6);">
      <div class="kbd-item" data-cur="default"   onclick="setCursor('default')">Default arrow</div>
      <div class="kbd-item" data-cur="large"     onclick="setCursor('large')">Large arrow</div>
      <div class="kbd-item" data-cur="crosshair" onclick="setCursor('crosshair')">Crosshair</div>
      <div class="kbd-item" data-cur="dot"       onclick="setCursor('dot')">Precision dot</div>
      <div class="kbd-item" data-cur="cell"      onclick="setCursor('cell')">Cell (cell-grid)</div>
      <div class="kbd-item" data-cur="hidden"    onclick="setCursor('hidden')">Hidden (guest only)</div>
    </div>
  </div>
  <button onclick="document.getElementById('spice-area').requestFullscreen()">Fullscreen</button>
  <span id="status">Connecting…</span>
</div>
<div id="spice-area">
  <span id="notice">Connecting to VGA console…</span>
</div>
<script src="/static/vendor/spice-html5.js"></script>
<script>
const name = %q;
const proto = location.protocol === 'https:' ? 'wss' : 'ws';
let sc;
// Connection state machine drives the top-right badge and the auto-reconnect
// loop. Values: 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'vm-offline'
let _state = 'idle';
let _reconnectTimer = null;
let _userInitiatedReconnect = false;

function setStatus(msg, cls) {
  const el = document.getElementById('status');
  el.textContent = msg;
  el.className = cls || '';
}
function setNotice(msg) {
  let el = document.getElementById('notice');
  if (!el) {
    el = document.createElement('span');
    el.id = 'notice';
    document.getElementById('spice-area').appendChild(el);
  }
  el.textContent = msg;
}
function clearNotice() {
  const n = document.getElementById('notice');
  if (n) n.remove();
}

function _setState(s) { _state = s; }

function _cancelReconnect() {
  if (_reconnectTimer) { clearTimeout(_reconnectTimer); _reconnectTimer = null; }
}

// Reset the SPICE area back to a single notice — wipes any stale canvas left
// over from the previous connection so the user doesn't see a frozen frame
// behind the "Reconnecting…" status.
function _resetSpiceArea(noticeText) {
  const area = document.getElementById('spice-area');
  area.innerHTML = '<span id="notice">' + (noticeText || 'Connecting…') + '</span>';
}

function reconnect() {
  _userInitiatedReconnect = true;
  _cancelReconnect();
  if (sc) { try { sc.stop(); } catch(e) {} sc = null; }
  _resetSpiceArea('Connecting to VGA console…');
  _setState('connecting');
  setStatus('Connecting…');
  setTimeout(connect, 200);
}

// Poll the VM's run state and decide what to do next:
//   - Running          → start a fresh SPICE connection.
//   - Stopped/Frozen/… → surface that in the badge and keep polling.
//   - Probe failed     → keep polling (treat as transient network blip).
async function _probeAndReconnect() {
  _reconnectTimer = null;
  let vmStatus = '';
  let probeFailed = false;
  try {
    const r = await fetch('/api/lxd/instances/' + encodeURIComponent(name) + '/status', { cache: 'no-store' });
    if (r.ok) {
      const d = await r.json();
      vmStatus = (d && d.status) || '';
    } else {
      probeFailed = true;
    }
  } catch(_) { probeFailed = true; }

  if (vmStatus === 'Running') {
    _setState('connecting');
    setStatus('Reconnecting…', 'warn');
    setNotice('VM is running — reattaching SPICE…');
    if (sc) { try { sc.stop(); } catch(e) {} sc = null; }
    connect();
    return;
  }

  if (vmStatus) {
    _setState('vm-offline');
    setStatus('VM ' + vmStatus, 'err');
    setNotice('Waiting for VM to start (' + vmStatus + ')…');
  } else if (probeFailed) {
    _setState('reconnecting');
    setStatus('Reconnecting…', 'warn');
    setNotice('Status probe failed — retrying…');
  }
  _reconnectTimer = setTimeout(_probeAndReconnect, 3000);
}

// Clear the stale canvas/audio nodes spice-html5 leaves behind, then schedule
// the first probe. Quick first retry so a clean reboot reattaches in ~1s
// once the VM's SPICE socket is back up.
function _scheduleReconnect(firstDelayMs) {
  _cancelReconnect();
  const area = document.getElementById('spice-area');
  if (area) {
    area.querySelectorAll('canvas, audio, video').forEach(n => n.remove());
  }
  _reconnectTimer = setTimeout(_probeAndReconnect, firstDelayMs == null ? 1200 : firstDelayMs);
}

function connect() {
  _userInitiatedReconnect = false;
  const wsUrl = proto + '://' + location.host + '/ws/lxd-vga?name=' + encodeURIComponent(name);
  try {
    sc = new SpiceMainConn({
      uri: wsUrl,
      screen_id: 'spice-area',
      onerror: function(e) {
        // spice-html5 calls onerror for every disconnect, including normal
        // ones (VM rebooted / shut down): the WS-close handler synthesises an
        // "Unexpected close" Error. We treat all of them the same: clear the
        // stale frame, surface a transient status, then probe + auto-reconnect.
        const wasConnected = _state === 'connected';
        _setState('reconnecting');
        setStatus(wasConnected ? 'Reconnecting…' : 'Disconnected', 'warn');
        setNotice((wasConnected ? 'Connection lost' : ('Connection error: ' + (e && e.message || 'disconnected'))) + ' — checking VM status…');
        _scheduleReconnect();
      },
      onagent: function() {
        _setState('connected');
        setStatus('Connected', 'ok');
        clearNotice();
      }
    });
    // Detect connect-success even for VMs without a SPICE agent (where
    // onagent never fires) by watching for spice-html5 injecting its canvas.
    const area = document.getElementById('spice-area');
    const obs = new MutationObserver(() => {
      const cv = area.querySelector('canvas');
      if (cv) {
        _setState('connected');
        setStatus('Connected', 'ok');
        clearNotice();
        bindCanvasFocus(cv);
        obs.disconnect();
      }
    });
    obs.observe(area, { childList: true, subtree: true });
  } catch(e) {
    setStatus('Failed: ' + e.message, 'err');
    setNotice('Could not start SPICE client: ' + e.message + ' — retrying…');
    _setState('reconnecting');
    _scheduleReconnect(2000);
  }
}

// iPad/iOS Safari drops keyboard input as soon as the canvas loses focus
// (which happens whenever a toolbar button is tapped, the on-screen keyboard
// is dismissed, or the page sits idle for a few seconds with no pointer
// activity). spice-html5 attaches keydown/keyup listeners directly to the
// canvas (see vendor/spice-html5.js lines 6077–6082), so an unfocused canvas
// silently swallows every keystroke — that's why moving the mouse/finger
// "fixes" it temporarily. We aggressively keep focus on the canvas.
function bindCanvasFocus(canvas) {
  if (!canvas) return;
  canvas.setAttribute('tabindex', '0');
  canvas.style.outline = 'none';
  try { canvas.focus({ preventScroll: true }); } catch(_) { canvas.focus(); }

  // Re-focus immediately after blur. setTimeout(0) lets the next focus target
  // settle so we don't fight a deliberate focus on a toolbar button while its
  // onclick handler is running.
  canvas.addEventListener('blur', function() {
    setTimeout(function() {
      if (document.visibilityState === 'visible' && document.body.contains(canvas)) {
        try { canvas.focus({ preventScroll: true }); } catch(_){}
      }
    }, 0);
  });

  // Tap inside the console area → focus the canvas. Covers iPad finger taps
  // and mouse clicks with one handler.
  var area = document.getElementById('spice-area');
  if (area) {
    area.addEventListener('pointerdown', function() {
      try { canvas.focus({ preventScroll: true }); } catch(_){}
    });
  }

  // Safety net: if focus drifts to <body> (default landing spot after the
  // iPad on-screen keyboard dismiss or app-switch return), pull it back.
  // Skipped while the page is hidden so we don't yank focus from another tab.
  setInterval(function() {
    if (document.visibilityState !== 'visible') return;
    var ae = document.activeElement;
    if (ae !== canvas && (ae === document.body || ae === null || ae === document.documentElement)) {
      try { canvas.focus({ preventScroll: true }); } catch(_){}
    }
  }, 750);
}

function _closeAllToolbarMenus() {
  ['kbd-menu','cur-menu','cdrom-menu'].forEach(function(id) {
    const el = document.getElementById(id);
    if (el) el.style.display = 'none';
  });
}
function toggleKbdMenu(e) {
  e.stopPropagation();
  const m = document.getElementById('kbd-menu');
  const open = m.style.display !== 'block';
  _closeAllToolbarMenus();
  if (open) m.style.display = 'block';
}
function toggleCursorMenu(e) {
  e.stopPropagation();
  const m = document.getElementById('cur-menu');
  const open = m.style.display !== 'block';
  _closeAllToolbarMenus();
  if (open) m.style.display = 'block';
}
document.addEventListener('click', _closeAllToolbarMenus);

// --- CD/DVD drive picker ----------------------------------------------------
// Visible only when the VM has at least one CDROM drive configured. Clicking
// it opens a list of ISOs available in the same storage pool plus an Eject
// option. The change writes raw.qemu, which QEMU only re-reads at start, so
// running VMs need a restart for the new disc to appear.
let _cdromState = { configured: [], available: [], pool: '', running: false };

async function refreshCDROMState() {
  try {
    const r = await fetch('/api/lxd/instances/' + encodeURIComponent(name) + '/cdroms', { cache:'no-store' });
    if (!r.ok) { document.getElementById('cdrom-wrap').style.display = 'none'; return; }
    _cdromState = await r.json();
  } catch(_) { document.getElementById('cdrom-wrap').style.display = 'none'; return; }
  const wrap = document.getElementById('cdrom-wrap');
  const btn  = document.getElementById('cdrom-btn');
  // Show the disc button for every VM. Containers don't have a CDROM concept
  // in this UI; everything else benefits from being able to mount/swap an ISO
  // mid-session (Windows driver install, ISO-based recovery, etc.) even when
  // no disc is currently inserted.
  if (!_cdromState.is_vm) {
    wrap.style.display = 'none';
    return;
  }
  wrap.style.display = 'inline-block';
  const cur = (_cdromState.configured && _cdromState.configured[0]) || null;
  if (cur && cur.filename) {
    btn.classList.add('has-disc');
    btn.title = 'CD/DVD: ' + cur.filename;
  } else {
    btn.classList.remove('has-disc');
    btn.title = 'CD/DVD drive (empty)';
  }
}

function _renderCDROMMenu() {
  const m = document.getElementById('cdrom-menu');
  const cur = (_cdromState.configured && _cdromState.configured[0]) || { filename: '' };
  // Build with DOM nodes rather than HTML string concatenation so ISO names
  // containing quotes / backslashes / HTML metacharacters are always handled
  // correctly. .textContent escapes for display; data-iso carries the raw
  // value to the click handler.
  m.innerHTML = '';
  const eject = document.createElement('div');
  eject.className = 'kbd-item eject' + (cur.filename ? '' : ' muted');
  eject.textContent = '⏏ Empty (eject)';
  eject.dataset.iso = '';
  eject.addEventListener('click', function() { swapCDROM(''); });
  m.appendChild(eject);
  if (!_cdromState.available || !_cdromState.available.length) {
    const empty = document.createElement('div');
    empty.className = 'kbd-item muted';
    empty.textContent = 'No ISOs in pool ' + (_cdromState.pool || '?');
    m.appendChild(empty);
  } else {
    _cdromState.available.forEach(function(iso) {
      const isCur = iso.name === cur.filename;
      const row = document.createElement('div');
      row.className = 'kbd-item' + (isCur ? ' current' : '');
      row.textContent = (isCur ? '● ' : '') + iso.name;
      row.dataset.iso = iso.name;
      row.addEventListener('click', function() { swapCDROM(iso.name); });
      m.appendChild(row);
    });
  }
  if (_cdromState.running) {
    const hint = document.createElement('div');
    hint.className = 'kbd-sub';
    hint.textContent = 'Restart the VM to apply a new disc.';
    m.appendChild(hint);
  }
}

function toggleCDROMMenu(e) {
  e.stopPropagation();
  const m = document.getElementById('cdrom-menu');
  const open = m.style.display !== 'block';
  _closeAllToolbarMenus();
  if (open) {
    refreshCDROMState().then(function() {
      _renderCDROMMenu();
      m.style.display = 'block';
    });
  }
}

async function swapCDROM(filename) {
  document.getElementById('cdrom-menu').style.display = 'none';
  try {
    const r = await fetch('/api/lxd/instances/' + encodeURIComponent(name) + '/cdroms/swap', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ filename: filename })
    });
    const d = await r.json().catch(function(){ return {}; });
    if (!r.ok || d.ok === false) {
      setStatus('Disc swap failed', 'err');
      setNotice('Disc swap failed: ' + (d.error || r.statusText));
      setTimeout(clearNotice, 4000);
      return;
    }
    await refreshCDROMState();
    if (d.running) {
      setStatus('Restart VM to apply', 'warn');
    } else {
      setStatus(filename ? ('Loaded ' + filename) : 'Drive emptied', 'ok');
    }
  } catch(e) {
    setStatus('Disc swap error', 'err');
    setNotice('Disc swap error: ' + (e && e.message || e));
  }
}
function kbdCtrlAltDel() {
  document.getElementById('kbd-menu').style.display = 'none';
  sendCtrlAltDel(sc);
}

// Persist the chosen pointer style across reconnects and across viewer
// sessions for this VM. Stored under a key that includes the instance name
// so each VM can keep its own preference. We default to the large arrow
// because spice-html5 frequently sets cursor:none when the guest sends no
// cursor data — a custom URL cursor stays visible regardless.
const _CUR_STORE_KEY = 'znas.lxd.vga.cursor.' + name;
const _VALID_CURSORS = ['default','large','crosshair','dot','cell','hidden'];
function _readSavedCursor() {
  try {
    const v = localStorage.getItem(_CUR_STORE_KEY);
    if (_VALID_CURSORS.indexOf(v) >= 0) return v;
  } catch(_){}
  return 'large';
}
function _applyCursor(name) {
  const area = document.getElementById('spice-area');
  if (!area) return;
  _VALID_CURSORS.forEach(n => area.classList.remove('cur-' + n));
  area.classList.add('cur-' + name);
  // Highlight active item in the menu.
  document.querySelectorAll('#cur-menu .kbd-item').forEach(el => {
    el.style.background = el.dataset.cur === name ? '#3a3a3a' : '';
    el.style.color      = el.dataset.cur === name ? '#fff'    : '';
  });
}
function setCursor(name) {
  if (_VALID_CURSORS.indexOf(name) < 0) name = 'default';
  try { localStorage.setItem(_CUR_STORE_KEY, name); } catch(_){}
  _applyCursor(name);
  document.getElementById('cur-menu').style.display = 'none';
}

window.addEventListener('load', function() {
  _applyCursor(_readSavedCursor());
  refreshCDROMState();
  connect();
});
</script>
</body>
</html>`
