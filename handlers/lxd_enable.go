package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

var lxdEnableJobs sync.Map // job_id → *system.LXDEnableJob

func newEnableJobID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// HandleLXDEnablePrereqs returns the four prerequisite check results.
// GET /api/lxd/enable/prereqs
func HandleLXDEnablePrereqs(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, system.LXDEnableCheckPrereqs())
}

// HandleLXDEnableStatus returns whether LXD is currently enabled.
// GET /api/lxd/enable/status
func HandleLXDEnableStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]bool{"enabled": isLXDAvailable()})
}

// HandleLXDEnableStart kicks off the LXD enablement job.
// POST /api/lxd/enable/start   body: {"storage_pool":"tank"}
func HandleLXDEnableStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StoragePool string `json:"storage_pool"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StoragePool == "" {
		jsonErr(w, http.StatusBadRequest, "storage_pool required")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := system.NewLXDEnableJob(cancel)
	jobID := newEnableJobID()
	lxdEnableJobs.Store(jobID, job)

	go func() {
		system.LXDEnableFeature(ctx, req.StoragePool, job)
		snap := job.Snapshot()
		if snap.Status == "done" {
			v := system.LXDAvailable()
			SetLXDAvailable(v)
			audit.Log(audit.Entry{Action: audit.ActionLXDEnable, User: "system", Details: "VMs & Containers feature enabled on pool " + req.StoragePool})
		}
	}()

	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, map[string]string{"job_id": jobID})
}

// HandleLXDEnableProgress returns the current state of an enable job.
// GET /api/lxd/enable/progress?job_id=<id>
func HandleLXDEnableProgress(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	v, ok := lxdEnableJobs.Load(jobID)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	jsonOK(w, v.(*system.LXDEnableJob).Snapshot())
}

// HandleLXDEnableCancel cancels a running enable job.
// POST /api/lxd/enable/cancel?job_id=<id>
func HandleLXDEnableCancel(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	v, ok := lxdEnableJobs.Load(jobID)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	v.(*system.LXDEnableJob).Cancel()
	jsonOK(w, map[string]bool{"cancelled": true})
}
