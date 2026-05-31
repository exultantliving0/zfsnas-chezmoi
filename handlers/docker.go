package handlers

// Docker Detection (v6.5.26) — HTTP + WS surface for the per-instance
// Docker card. All work is delegated to system/docker.go so the
// `incus exec --cwd <yaml-dir> -- docker compose …` invariant lives in
// one place.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/mux"

	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/internal/termsessions"
	"zfsnas/system"
)

// dockerDetectGate makes sure a per-instance request is allowed by the
// Settings → Virtualization → Docker Detection toggles. We look up the
// instance type via `incus list` because the URL only carries the name.
// Cached per-instance for 30 s to keep burger-menu spam off Incus.
type dockerInstTypeEntry struct {
	t        string // "virtual-machine" | "container"
	cachedAt time.Time
}

var (
	dockerInstTypeCache   = map[string]dockerInstTypeEntry{}
	dockerInstTypeCacheMu sync.Mutex
)

func dockerInstanceType(name string) string {
	dockerInstTypeCacheMu.Lock()
	if e, ok := dockerInstTypeCache[name]; ok && time.Since(e.cachedAt) < 30*time.Second {
		dockerInstTypeCacheMu.Unlock()
		return e.t
	}
	dockerInstTypeCacheMu.Unlock()
	out, err := exec.Command("incus", "list", name, "-c", "tn", "--format", "csv").Output()
	if err != nil {
		return ""
	}
	// Each row is "type,name". We just need the type of the first row
	// whose name matches exactly — `incus list <name>` is a substring
	// match so a name like "foo" can also hit "foobar".
	t := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ",", 2)
		if len(parts) == 2 && parts[1] == name {
			t = parts[0]
			break
		}
	}
	dockerInstTypeCacheMu.Lock()
	dockerInstTypeCache[name] = dockerInstTypeEntry{t: t, cachedAt: time.Now()}
	dockerInstTypeCacheMu.Unlock()
	return t
}

// dockerGateAllowed returns true when the per-type toggle in AppConfig
// covers this instance type. An unknown type defers to "container" to
// avoid surprise lockouts.
func dockerGateAllowed(appCfg *config.AppConfig, instance string) bool {
	t := dockerInstanceType(instance)
	if t == "virtual-machine" {
		return appCfg.DockerDetectVMs
	}
	return appCfg.DockerDetectContainers
}

// HandleDockerProbe returns {available, agent_ok, reason}. Cheap; the
// frontend calls this once per page visit before deciding to render
// the Docker card.
// GET /api/incus/instances/{name}/docker/probe
func HandleDockerProbe(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonOK(w, system.DockerProbeResult{Available: false, Reason: "docker detection disabled in settings for this instance type"})
			return
		}
		jsonOK(w, system.DockerProbe(name))
	}
}

// HandleDockerListContainers returns the full container list. Grouping
// is done client-side from the labels each entry carries.
// GET /api/incus/instances/{name}/docker/containers
func HandleDockerListContainers(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled for this instance type")
			return
		}
		containers, err := system.DockerListContainers(name)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, map[string]interface{}{"containers": containers})
	}
}

// validateComposePath confirms `path` is one of the config_files of an
// existing project in the instance. Without this, a crafted query string
// would let any logged-in user yank any path off the guest's disk.
//
// We accept either:
//   - exact match against any container's config_files entry, OR
//   - any path inside the project's working_dir (lets the user save a
//     YAML to a sibling override file that hasn't been picked up yet,
//     without inventing a separate "saveAs" UI).
func validateComposePath(instance, path string) (project string, ok bool) {
	if path == "" || path[0] != '/' {
		return "", false
	}
	clean := filepath.Clean(path)
	if clean != path {
		return "", false
	}
	containers, err := system.DockerListContainers(instance)
	if err != nil {
		return "", false
	}
	for _, c := range containers {
		for _, f := range c.ConfigFiles {
			if f == path {
				return c.Project, true
			}
		}
		if c.WorkingDir != "" {
			dir := filepath.Clean(c.WorkingDir)
			if strings.HasPrefix(path, dir+"/") && !strings.Contains(path[len(dir)+1:], "/..") {
				return c.Project, true
			}
		}
	}
	return "", false
}

// HandleDockerGetComposeFile reads the YAML at ?path=… and returns it
// alongside the project name (handy for the modal title).
// GET /api/incus/instances/{name}/docker/compose-file?path=<abs>
func HandleDockerGetComposeFile(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled for this instance type")
			return
		}
		path := r.URL.Query().Get("path")
		project, ok := validateComposePath(name, path)
		if !ok {
			jsonErr(w, http.StatusBadRequest, "path is not a known compose file for this instance")
			return
		}
		// `incus file pull` returns "Error: file does not exist" on a
		// missing file (older versions said "not found"); os.IsNotExist
		// doesn't catch that because the error is wrapped string-only.
		// We check a few well-known fragments rather than re-typing the
		// error to keep this resilient across Incus versions.
		isNotExistErr := func(err error) bool {
			if err == nil {
				return false
			}
			if os.IsNotExist(err) {
				return true
			}
			m := err.Error()
			return strings.Contains(m, "no such file") ||
				strings.Contains(m, "does not exist") ||
				strings.Contains(m, "not found")
		}
		yaml, err := system.DockerReadComposeFile(name, path)
		yamlMissing := false
		if err != nil {
			if isNotExistErr(err) {
				yamlMissing = true
				yaml = ""
			} else {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// Sibling .env discovery (v6.5.26). `docker compose` defaults
		// to reading <project-dir>/.env automatically, so the convention
		// is rock-solid in practice. We surface it next to the YAML
		// editor so the user doesn't need a separate terminal to tweak
		// variables. The path is always returned so the frontend can
		// offer to create one even when none exists yet.
		envPath := filepath.Join(filepath.Dir(path), ".env")
		envBody, envErr := system.DockerReadComposeFile(name, envPath)
		envMissing := false
		if envErr != nil {
			if isNotExistErr(envErr) {
				envMissing = true
				envBody = ""
			} else {
				// Don't fail the whole call on a perms hiccup reading
				// the .env — just hide that section in the UI by
				// returning an empty path.
				envPath = ""
			}
		}
		resp := map[string]interface{}{
			"path":        path,
			"project":     project,
			"yaml":        yaml,
			"env_path":    envPath,
			"env":         envBody,
			"env_missing": envMissing,
		}
		if yamlMissing {
			resp["missing"] = true
		}
		jsonOK(w, resp)
	}
}

// HandleDockerPutComposeFile writes new YAML and runs `docker compose
// up -d` as a progress job. cwd is the YAML's directory so relative
// references in the file resolve as the user expects.
// PUT /api/incus/instances/{name}/docker/compose-file
func HandleDockerPutComposeFile(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled for this instance type")
			return
		}
		var req struct {
			Path string  `json:"path"`
			YAML string  `json:"yaml"`
			// Env is a pointer so we can distinguish "field omitted"
			// (frontend never showed the .env editor) from "field
			// present with empty string" (user blanked the .env on
			// purpose, which we treat as 'delete the file').
			Env  *string `json:"env,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if _, ok := validateComposePath(name, req.Path); !ok {
			jsonErr(w, http.StatusBadRequest, "path is not a known compose file for this instance")
			return
		}
		if strings.TrimSpace(req.YAML) == "" {
			jsonErr(w, http.StatusBadRequest, "docker-compose content is required")
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
			err := system.DockerWriteComposeFile(name, req.Path, req.YAML)
			// Sibling .env write (v6.5.26). Only touch the file when
			// the frontend included the field — that way unrelated
			// .env files on the guest aren't risked by clients that
			// don't know about the new field.
			if err == nil && req.Env != nil {
				envPath := filepath.Join(filepath.Dir(req.Path), ".env")
				if strings.TrimSpace(*req.Env) == "" {
					// User blanked the editor → delete the file. A
					// stray .env left on disk with whitespace-only
					// content would still be sourced by docker compose,
					// which can shadow legitimate process env vars.
					err = system.DockerDeleteFile(name, envPath)
				} else {
					err = system.DockerWriteComposeFile(name, envPath, *req.Env)
				}
			}
			if err == nil {
				err = system.DockerComposeAction(name, req.Path, []string{"up", "-d"}, logCh)
			}
			close(logCh)
			job.mu.Lock()
			if err != nil {
				job.Status, job.Error = "error", err.Error()
			} else {
				job.Status = "done"
			}
			job.mu.Unlock()
			result, details := audit.ResultOK, req.Path
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{
				User: sess.Username, Role: sess.Role,
				Action: audit.ActionDockerComposeEdit, Target: "docker:" + name + ":" + req.Path,
				Result: result, Details: details,
			})
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": jobID}) //nolint:errcheck
	}
}

// HandleDockerComposeAction runs start/stop/restart/pull/up against a
// project's compose file. Returns a progress job_id like the other long
// operations on this page.
// POST /api/incus/instances/{name}/docker/compose-action
func HandleDockerComposeAction(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled for this instance type")
			return
		}
		var req struct {
			Path   string `json:"path"`   // absolute path to docker-compose.yml
			Action string `json:"action"` // start|stop|restart|update
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if _, ok := validateComposePath(name, req.Path); !ok {
			jsonErr(w, http.StatusBadRequest, "path is not a known compose file for this instance")
			return
		}
		// Map the user-facing action to one or two compose subcommands.
		// For "update" we also detect depends_on in the YAML — Podman's
		// `up -d` recreate flow deadlocks when a parent container is
		// referenced by a dependent that hasn't been removed yet
		// ("container … has dependent containers which must be removed
		// before it"). A `down` before `up -d` clears both sides so the
		// recreate can proceed.
		var stages [][]string
		switch req.Action {
		case "start":
			stages = [][]string{{"start"}}
		case "stop":
			stages = [][]string{{"stop"}}
		case "restart":
			stages = [][]string{{"restart"}}
		case "update":
			stages = [][]string{{"pull"}, {"up", "-d"}}
			if hasDeps, err := system.DockerComposeHasDependsOn(name, req.Path); err == nil && hasDeps {
				stages = [][]string{{"pull"}, {"down"}, {"up", "-d"}}
			}
		default:
			jsonErr(w, http.StatusBadRequest, "action must be start|stop|restart|update")
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
			var err error
			for _, stage := range stages {
				if err = system.DockerComposeAction(name, req.Path, stage, logCh); err != nil {
					break
				}
			}
			close(logCh)
			job.mu.Lock()
			if err != nil {
				job.Status, job.Error = "error", err.Error()
			} else {
				job.Status = "done"
			}
			job.mu.Unlock()
			result, details := audit.ResultOK, req.Action+" "+req.Path
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{
				User: sess.Username, Role: sess.Role,
				Action: audit.ActionDockerComposeAction, Target: "docker:" + name + ":" + req.Path,
				Result: result, Details: details,
			})
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": jobID}) //nolint:errcheck
	}
}

// HandleDockerContainerAction runs start/stop/restart synchronously, or
// returns a job_id for `update` (pull + restart). Mirrors the existing
// HandleComposeContainerAction shape.
// POST /api/incus/instances/{name}/docker/container-action
func HandleDockerContainerAction(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled for this instance type")
			return
		}
		var req struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		if req.ID == "" {
			jsonErr(w, http.StatusBadRequest, "id is required")
			return
		}
		switch req.Action {
		case "start", "stop", "restart":
			err := system.DockerContainerAction(name, req.ID, req.Action, nil)
			sess := MustSession(r)
			result, details := audit.ResultOK, req.Action+" "+req.ID
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{
				User: sess.Username, Role: sess.Role,
				Action: audit.ActionDockerContainerAction, Target: "docker:" + name + ":" + req.ID,
				Result: result, Details: details,
			})
			if err != nil {
				jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			jsonOK(w, map[string]bool{"ok": true})
		case "update":
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
				err := system.DockerContainerAction(name, req.ID, "update", logCh)
				close(logCh)
				job.mu.Lock()
				if err != nil {
					job.Status, job.Error = "error", err.Error()
				} else {
					job.Status = "done"
				}
				job.mu.Unlock()
				result, details := audit.ResultOK, "update "+req.ID
				if err != nil {
					result, details = audit.ResultError, err.Error()
				}
				audit.Log(audit.Entry{
					User: sess.Username, Role: sess.Role,
					Action: audit.ActionDockerContainerAction, Target: "docker:" + name + ":" + req.ID,
					Result: result, Details: details,
				})
			}()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"job_id": jobID}) //nolint:errcheck
		default:
			jsonErr(w, http.StatusBadRequest, "action must be start|stop|restart|update")
		}
	}
}

// HandleDockerContainerLogs returns the last N lines of `docker logs`.
// GET /api/incus/instances/{name}/docker/containers/{id}/logs?tail=200
func HandleDockerContainerLogs(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		id := vars["id"]
		if !dockerGateAllowed(appCfg, name) {
			http.Error(w, "docker detection disabled", http.StatusForbidden)
			return
		}
		tail := 200
		if t := r.URL.Query().Get("tail"); t != "" {
			fmt.Sscanf(t, "%d", &tail)
		}
		out, err := system.DockerContainerLogs(name, id, tail)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(out)) //nolint:errcheck
	}
}

// HandleDockerContainerInspect returns the raw `docker inspect` JSON.
// GET /api/incus/instances/{name}/docker/containers/{id}/inspect
func HandleDockerContainerInspect(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := vars["name"]
		id := vars["id"]
		if !dockerGateAllowed(appCfg, name) {
			jsonErr(w, http.StatusForbidden, "docker detection disabled")
			return
		}
		out, err := system.DockerContainerInspect(name, id)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(out)) //nolint:errcheck
	}
}

// HandleDockerConsoleWS attaches to a persistent terminal session for one
// docker container running inside a user VM/CT. PTY survives WS
// disconnect — see termsessions.Default for lifetime.
// WS /ws/docker-console?instance=<i>&container=<c>[&session_id=<id>]
func HandleDockerConsoleWS(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		instance := r.URL.Query().Get("instance")
		container := r.URL.Query().Get("container")
		if instance == "" || container == "" {
			http.Error(w, "instance and container required", http.StatusBadRequest)
			return
		}
		if !dockerGateAllowed(appCfg, instance) {
			http.Error(w, "docker detection disabled for this instance type", http.StatusForbidden)
			return
		}
		conn, err := lxdConsoleUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		sess := MustSession(r)
		target := instance + ":" + container
		title := container + " — " + instance
		wsAttachOrCreate(conn, r, sess.UserID, termsessions.KindDocker, target, title, func() (*exec.Cmd, *os.File, error) {
			shell := "/bin/sh"
			if exec.Command("incus", "exec", instance, "--",
				"docker", "exec", container, "which", "bash").Run() == nil {
				shell = "/bin/bash"
			}
			cmd := exec.Command("incus", "exec", instance, "--",
				"docker", "exec", "-it", container, shell)
			cmd.Env = append(os.Environ(), "TERM=xterm-256color")
			ptmx, err := pty.Start(cmd)
			return cmd, ptmx, err
		})
	}
}

// ServeDockerConsolePage serves a full-page xterm.js console targeting
// one docker container inside an instance — used by the "Tab" / "Window"
// terminal menu items.
// GET /docker-console/{instance}/{container}
func ServeDockerConsolePage(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	instance := vars["instance"]
	container := vars["container"]
	if instance == "" || container == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, dockerConsolePageHTML, container+" — "+instance, instance, container)
}

const dockerConsolePageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>%s</title>
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
const instance = %q;
const container = %q;
const term = new Terminal({ cursorBlink: true, fontSize: 14, theme: { background: '#0d1117' } });
const fitAddon = new FitAddon.FitAddon();
term.loadAddon(fitAddon);
term.open(document.getElementById('term'));
fitAddon.fit();

const proto = location.protocol === 'https:' ? 'wss' : 'ws';
const ws = new WebSocket(proto + '://' + location.host + '/ws/docker-console?instance=' + encodeURIComponent(instance) + '&container=' + encodeURIComponent(container));
ws.binaryType = 'arraybuffer';

ws.onopen = () => {
  term.focus();
  ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
};
ws.onmessage = (ev) => {
  if (ev.data instanceof ArrayBuffer) {
    term.write(new Uint8Array(ev.data));
  } else {
    term.write(ev.data);
  }
};
ws.onclose = () => term.write('\r\n\x1b[33m[session ended]\x1b[0m\r\n');
term.onData(d => ws.send(d));
window.addEventListener('resize', () => {
  fitAddon.fit();
  if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
});
</script>
</body>
</html>`
