package handlers

// v6.6.22 — Compose deploy-target chooser backend.
//
// Three new ways to deploy a compose stack, beyond the original "fresh LXC":
//   - GET    /api/lxd/compose-targets                       — list instances that
//     already run Docker or Podman (option #2 target picker).
//   - POST   /api/lxd/instances/{name}/compose-deploy       — drop a new stack
//     into an existing instance's /opt/<stack>/ (option #2).
//   - POST   /api/lxd/compose-vms                           — create a fresh VM
//     with the chosen runtime + deploy the stack (option #3).
//   - DELETE /api/lxd/instances/{name}/docker/compose-project — tear down +
//     remove a detected stack's folder (§6).
//
// All long-running calls reuse the shared lxdJobs + create-progress streaming.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zfsnas/internal/audit"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// startComposeJob wires a streaming background job onto the shared lxdJobs map
// and returns the job id, mirroring HandleCreateComposeStack / HandleCreateVM.
func startComposeJob(run func(logCh chan<- string) error, onDone func(err error)) string {
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
		err := run(logCh)
		close(logCh)
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
		} else {
			job.Status = "done"
		}
		job.mu.Unlock()
		if onDone != nil {
			onDone(err)
		}
	}()
	return jobID
}

func writeJobID(w http.ResponseWriter, jobID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// HandleComposeTargets lists running instances with a Docker/Podman runtime.
// GET /api/lxd/compose-targets
func HandleComposeTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := system.ListComposeTargets()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"targets": targets})
}

// HandleComposeDeploy deploys a new stack into an existing instance (option #2).
// POST /api/lxd/instances/{name}/compose-deploy
func HandleComposeDeploy(w http.ResponseWriter, r *http.Request) {
	instance := mux.Vars(r)["name"]
	var req struct {
		StackName   string `json:"stack_name"`
		ComposeYAML string `json:"compose_yaml"`
		ComposeEnv  string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.StackName) == "" {
		jsonErr(w, http.StatusBadRequest, "stack_name is required")
		return
	}
	if strings.TrimSpace(req.ComposeYAML) == "" {
		jsonErr(w, http.StatusBadRequest, "docker-compose content is required")
		return
	}
	sess := MustSession(r)
	jobID := startComposeJob(
		func(logCh chan<- string) error {
			return system.DeployComposeToInstance(instance, req.StackName, req.ComposeYAML, req.ComposeEnv, logCh)
		},
		func(err error) {
			result, details := audit.ResultOK, ""
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: "compose:" + instance + "/" + req.StackName, Result: result, Details: details})
		},
	)
	writeJobID(w, jobID)
}

// HandleCreateComposeVM creates a fresh VM and deploys a stack into it (#3).
// POST /api/lxd/compose-vms
func HandleCreateComposeVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		system.LXDCreateVMRequest
		Runtime     string `json:"runtime"`
		ComposeYAML string `json:"compose_yaml"`
		ComposeEnv  string `json:"compose_env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req := body.LXDCreateVMRequest
	if req.Name == "" {
		jsonErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(body.ComposeYAML) == "" {
		jsonErr(w, http.StatusBadRequest, "docker-compose content is required")
		return
	}
	sess := MustSession(r)
	jobID := startComposeJob(
		func(logCh chan<- string) error {
			return system.LXDCreateComposeStackVM(req, body.Runtime, body.ComposeYAML, body.ComposeEnv, logCh)
		},
		func(err error) {
			result, details := audit.ResultOK, ""
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDCreateVM, Target: "compose-vm:" + req.Name, Result: result, Details: details})
		},
	)
	writeJobID(w, jobID)
}

// HandleDeleteComposeProject tears down + removes a detected stack (§6).
// DELETE /api/lxd/instances/{name}/docker/compose-project
func HandleDeleteComposeProject(w http.ResponseWriter, r *http.Request) {
	instance := mux.Vars(r)["name"]
	var req struct {
		ConfigFile    string `json:"config_file"`
		WorkingDir    string `json:"working_dir"`
		RemoveVolumes bool   `json:"remove_volumes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.ConfigFile) == "" {
		jsonErr(w, http.StatusBadRequest, "config_file is required")
		return
	}
	sess := MustSession(r)
	jobID := startComposeJob(
		func(logCh chan<- string) error {
			return system.DeleteComposeProject(instance, req.ConfigFile, req.WorkingDir, req.RemoveVolumes, logCh)
		},
		func(err error) {
			result, details := audit.ResultOK, ""
			if err != nil {
				result, details = audit.ResultError, err.Error()
			}
			audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDDelete, Target: "compose:" + instance + ":" + req.WorkingDir, Result: result, Details: details})
		},
	)
	writeJobID(w, jobID)
}
