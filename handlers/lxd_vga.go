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
#status.ok  { color:#4ade80; }
#status.err { color:#f87171; }
#spice-area { flex:1; overflow:hidden; position:relative; display:flex; align-items:center; justify-content:center; }
#spice-area canvas { cursor:default; display:block; }
#notice { position:absolute; color:#666; font-size:13px; text-align:center; line-height:1.7; pointer-events:none; }
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

function setStatus(msg, cls) {
  const el = document.getElementById('status');
  el.textContent = msg;
  el.className = cls || '';
}
function setNotice(msg) {
  const el = document.getElementById('notice');
  if (el) el.textContent = msg;
}

function reconnect() {
  if (sc) { try { sc.stop(); } catch(e) {} sc = null; }
  const area = document.getElementById('spice-area');
  area.innerHTML = '<span id="notice">Connecting to VGA console…</span>';
  setStatus('Connecting…');
  setTimeout(connect, 400);
}

function clearNotice() {
  const n = document.getElementById('notice');
  if (n) n.remove();
}

function connect() {
  const wsUrl = proto + '://' + location.host + '/ws/lxd-vga?name=' + encodeURIComponent(name);
  try {
    sc = new SpiceMainConn({
      uri: wsUrl,
      screen_id: 'spice-area',
      onerror: function(e) {
        setStatus('Error: ' + (e.message || String(e)), 'err');
        setNotice('Connection error — click Reconnect to retry.\n' + (e.message || ''));
      },
      onagent: function() {
        setStatus('Connected', 'ok');
        clearNotice();
      }
    });
    // Remove the overlay as soon as spice-html5 injects its canvas — this fires
    // for VMs without a SPICE agent where onagent never fires.
    const area = document.getElementById('spice-area');
    const obs = new MutationObserver(() => {
      if (area.querySelector('canvas')) {
        setStatus('Connected', 'ok');
        clearNotice();
        obs.disconnect();
      }
    });
    obs.observe(area, { childList: true, subtree: true });
  } catch(e) {
    setStatus('Failed: ' + e.message, 'err');
    setNotice('Could not start SPICE client: ' + e.message);
  }
}

function toggleKbdMenu(e) {
  e.stopPropagation();
  const m = document.getElementById('kbd-menu');
  m.style.display = m.style.display === 'block' ? 'none' : 'block';
}
document.addEventListener('click', function() {
  const m = document.getElementById('kbd-menu');
  if (m) m.style.display = 'none';
});
function kbdCtrlAltDel() {
  document.getElementById('kbd-menu').style.display = 'none';
  sendCtrlAltDel(sc);
}

window.addEventListener('load', connect);
</script>
</body>
</html>`
