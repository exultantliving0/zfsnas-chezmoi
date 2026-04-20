package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/system"

	"github.com/creack/pty"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// lxdAvailable is set once at startup by main and read-only afterwards.
var lxdAvailable bool
var lxdAvailableMu sync.RWMutex

// SetLXDAvailable stores the LXD probe result.
func SetLXDAvailable(v bool) {
	lxdAvailableMu.Lock()
	lxdAvailable = v
	lxdAvailableMu.Unlock()
}

func isLXDAvailable() bool {
	lxdAvailableMu.RLock()
	defer lxdAvailableMu.RUnlock()
	return lxdAvailable
}

// lxdJob tracks an async LXD create operation.
type lxdJob struct {
	mu     sync.Mutex
	Status string `json:"status"` // "running", "done", "error"
	Error  string `json:"error,omitempty"`
	Lines  []string
}

var lxdJobs sync.Map // job_id → *lxdJob

// HandleLXDStatus returns whether LXD is available and its version.
// GET /api/lxd/status
func HandleLXDStatus(w http.ResponseWriter, r *http.Request) {
	available := isLXDAvailable()
	ver := ""
	if available {
		ver = system.LXDVersion()
	}
	jsonOK(w, map[string]interface{}{
		"available": available,
		"version":   ver,
	})
}

// HandleLXDRefreshStatus re-probes LXD and updates cached availability.
// POST /api/lxd/refresh-status
func HandleLXDRefreshStatus(w http.ResponseWriter, r *http.Request) {
	v := system.LXDAvailable()
	SetLXDAvailable(v)
	ver := ""
	if v {
		ver = system.LXDVersion()
	}
	jsonOK(w, map[string]interface{}{
		"available": v,
		"version":   ver,
	})
}

// HandleListInstances returns all LXD instances.
// GET /api/lxd/instances
func HandleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := system.ListLXDInstances()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, instances)
}

// HandleLXDInstanceStats returns live CPU/memory/disk usage for one instance.
// GET /api/lxd/instances/{name}/stats
func HandleLXDInstanceStats(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	stats, err := system.LXDGetInstanceStats(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, stats)
}

// HandleLXDInstanceStatus returns the current status of one instance.
// GET /api/lxd/instances/{name}/status
func HandleLXDInstanceStatus(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	status, err := system.LXDGetStatus(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"status": status})
}

// HandleLXDStart starts an instance.
// POST /api/lxd/instances/{name}/start
func HandleLXDStart(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	if err := system.LXDStart(name); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDStart, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDStart, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "started"})
}

// HandleLXDStop stops an instance.
// POST /api/lxd/instances/{name}/stop
func HandleLXDStop(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	var req struct {
		Force bool `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if err := system.LXDStop(name, req.Force); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDStop, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDStop, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "stopped"})
}

// HandleLXDRestart restarts an instance.
// POST /api/lxd/instances/{name}/restart
func HandleLXDRestart(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	if err := system.LXDRestart(name); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDRestart, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDRestart, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "restarted"})
}

// HandleListBridges returns available network bridges (LXD managed + OS host bridges).
// GET /api/lxd/bridges
func HandleListBridges(w http.ResponseWriter, r *http.Request) {
	managed, _ := system.LXDListNetworks()
	host, _ := system.ListHostBridges()
	managedSet := map[string]bool{}
	for _, n := range managed {
		managedSet[n] = true
	}
	seen := map[string]bool{}
	all := []string{}
	for _, n := range managed {
		if !seen[n] {
			all = append(all, n)
			seen[n] = true
		}
	}
	for _, b := range host {
		if !seen[b] {
			all = append(all, b)
			seen[b] = true
		}
	}
	if all == nil {
		all = []string{}
	}
	jsonOK(w, map[string]interface{}{"managed": managed, "all": all})
}

// HandleLXDGetConfig returns the editable configuration of an instance.
// GET /api/lxd/instances/{name}/config
func HandleLXDGetConfig(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	cfg, err := system.LXDGetConfig(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, cfg)
}

// HandleLXDSetConfig applies editable configuration to an instance.
// PUT /api/lxd/instances/{name}/config
func HandleLXDSetConfig(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	var cfg system.LXDInstanceConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.LXDSetConfig(name, cfg); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "updated"})
}

// HandleLXDDelete deletes an instance.
// DELETE /api/lxd/instances/{name}
func HandleLXDDelete(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	var req struct {
		DeleteVolumes bool `json:"delete_volumes"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if err := system.LXDDelete(name, req.DeleteVolumes); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDDelete, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDDelete, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "deleted"})
}

// HandleListImages returns images filtered by kind and source.
// GET /api/lxd/images?kind=virtual-machine|container&source=local|remote
func HandleListImages(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	var imgs []system.LXDImage
	var err error
	if r.URL.Query().Get("source") == "local" {
		imgs, err = system.LXDListLocalImages(kind)
	} else {
		imgs, err = system.LXDListRemoteImages("images:", kind)
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, imgs)
}

// HandleListProfiles returns LXD profile names.
// GET /api/lxd/profiles
func HandleListProfiles(w http.ResponseWriter, r *http.Request) {
	names, err := system.LXDListProfiles()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, names)
}

// HandleListStoragePools returns LXD storage pool names.
// GET /api/lxd/storage-pools
func HandleListStoragePools(w http.ResponseWriter, r *http.Request) {
	names, err := system.LXDListStoragePools()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, names)
}

// HandleListNetworks returns LXD network names.
// GET /api/lxd/networks
func HandleListNetworks(w http.ResponseWriter, r *http.Request) {
	names, err := system.LXDListNetworks()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, names)
}

// HandleListUSB returns USB devices on the host.
// GET /api/lxd/usb-devices
func HandleListUSB(w http.ResponseWriter, r *http.Request) {
	devices, err := system.ListUSBDevices()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, devices)
}

// HandleListPCI returns PCI devices on the host.
// GET /api/lxd/pci-devices
func HandleListPCI(w http.ResponseWriter, r *http.Request) {
	devices, err := system.ListPCIDevices()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, devices)
}

// HandleCreateVM starts async VM creation and returns a job_id.
// POST /api/lxd/vms
func HandleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req system.LXDCreateVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.Image == "" {
		jsonErr(w, http.StatusBadRequest, "name and image are required")
		return
	}

	sess := MustSession(r)
	jobID := fmt.Sprintf("%d", time.Now().UnixNano())
	job := &lxdJob{Status: "running"}
	lxdJobs.Store(jobID, job)

	go func() {
		logCh := make(chan string, 64)
		go func() {
			for line := range logCh {
				job.mu.Lock()
				job.Lines = append(job.Lines, line)
				job.mu.Unlock()
			}
		}()
		err := system.LXDCreateVM(req, logCh)
		close(logCh)
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDCreateVM, Target: req.Name, Result: audit.ResultError, Details: err.Error()})
		} else {
			job.Status = "done"
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDCreateVM, Target: req.Name, Result: audit.ResultOK})
		}
		job.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// HandleCreateContainer starts async container creation and returns a job_id.
// POST /api/lxd/containers
func HandleCreateContainer(w http.ResponseWriter, r *http.Request) {
	var req system.LXDCreateContainerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.Image == "" {
		jsonErr(w, http.StatusBadRequest, "name and image are required")
		return
	}

	sess := MustSession(r)
	jobID := fmt.Sprintf("%d", time.Now().UnixNano())
	job := &lxdJob{Status: "running"}
	lxdJobs.Store(jobID, job)

	go func() {
		logCh := make(chan string, 64)
		go func() {
			for line := range logCh {
				job.mu.Lock()
				job.Lines = append(job.Lines, line)
				job.mu.Unlock()
			}
		}()
		err := system.LXDCreateContainer(req, logCh)
		close(logCh)
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDCreateContainer, Target: req.Name, Result: audit.ResultError, Details: err.Error()})
		} else {
			job.Status = "done"
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDCreateContainer, Target: req.Name, Result: audit.ResultOK})
		}
		job.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// HandleLXDCreateProgress returns job status + accumulated log lines.
// GET /api/lxd/create-progress?job_id=<id>
func HandleLXDCreateProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	val, ok := lxdJobs.Load(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	job := val.(*lxdJob)
	job.mu.Lock()
	defer job.mu.Unlock()
	jsonOK(w, map[string]interface{}{
		"status": job.Status,
		"error":  job.Error,
		"lines":  job.Lines,
	})
}

var lxdConsoleUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.HasSuffix(origin, "://"+r.Host)
	},
}

// HandleLXDConsole opens a WebSocket PTY to an LXD instance via `lxc exec`.
// WS /ws/lxd-console?name=<name>
func HandleLXDConsole(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	conn, err := lxdConsoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Try bash first, fall back to sh.
	shell := "bash"
	testCmd := exec.Command("lxc", "exec", name, "--", "which", "bash")
	if testCmd.Run() != nil {
		shell = "sh"
	}

	cmd := exec.Command("lxc", "exec", name, "--", shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("Failed to start console: "+err.Error()+"\r\n"))
		return
	}
	defer func() {
		cmd.Process.Kill()
		ptmx.Close()
	}()

	var once sync.Once
	done := make(chan struct{})

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
		}
	}()

	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
			if mt == websocket.TextMessage {
				var msg termMsg
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
					continue
				}
			}
			io.Copy(ptmx, newBytesReader(data))
		}
	}()

	<-done
}

// ServeLXDConsolePage serves the full-page xterm.js console for an LXD instance.
// GET /lxd-console/{name}
func ServeLXDConsolePage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, lxdConsolePageHTML, name, name)
}

const lxdConsolePageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Console — %s</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.min.css">
<style>
* { margin:0; padding:0; box-sizing:border-box; }
html, body { width:100%%; height:100%%; background:#0d1117; overflow:hidden; }
#term { width:100%%; height:100%%; }
</style>
</head>
<body>
<div id="term"></div>
<script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js"></script>
<script>
const name = %q;
const term = new Terminal({ cursorBlink: true, fontSize: 14, theme: { background: '#0d1117' } });
const fitAddon = new FitAddon.FitAddon();
term.loadAddon(fitAddon);
term.open(document.getElementById('term'));
fitAddon.fit();

const proto = location.protocol === 'https:' ? 'wss' : 'ws';
const ws = new WebSocket(proto + '://' + location.host + '/ws/lxd-console?name=' + encodeURIComponent(name));
ws.binaryType = 'arraybuffer';

ws.onopen = () => {
  term.focus();
  ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
};
ws.onmessage = e => {
  if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
  else term.write(e.data);
};
ws.onclose = () => term.write('\r\n\x1b[31m[Connection closed]\x1b[0m\r\n');

term.onData(data => { if (ws.readyState === WebSocket.OPEN) ws.send(data); });

window.addEventListener('resize', () => {
  fitAddon.fit();
  if (ws.readyState === WebSocket.OPEN)
    ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
});
</script>
</body>
</html>`
