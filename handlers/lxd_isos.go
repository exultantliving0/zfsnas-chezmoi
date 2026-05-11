package handlers

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// HandleListISOs returns all ISO files in the pool's .isos directory.
// GET /api/lxd/isos?pool=<pool>
func HandleListISOs(w http.ResponseWriter, r *http.Request) {
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "pool is required")
		return
	}
	isos, err := system.LXDListISOs(pool)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, isos)
}

// HandleDeleteISO deletes one ISO file.
// DELETE /api/lxd/isos/{filename}?pool=<pool>
func HandleDeleteISO(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	pool := r.URL.Query().Get("pool")
	filename := mux.Vars(r)["filename"]
	if pool == "" || filename == "" {
		jsonErr(w, http.StatusBadRequest, "pool and filename are required")
		return
	}
	target := pool + "/" + filename
	if err := system.LXDDeleteISO(pool, filename); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISODelete, Target: target, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISODelete, Target: target, Result: audit.ResultOK})
	jsonOK(w, map[string]bool{"ok": true})
}

// HandleFetchISOStart kicks off a server-side download into the pool's
// .isos directory. Returns the job ID; progress is polled via
// /api/lxd/isos/fetch/progress?job_id=…
//
// POST /api/lxd/isos/fetch  body: {"pool":"…","url":"https://…","name":"opt"}
func HandleFetchISOStart(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	var req struct {
		Pool string `json:"pool"`
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Pool == "" || req.URL == "" {
		jsonErr(w, http.StatusBadRequest, "pool and url are required")
		return
	}
	// The fetch runs in a goroutine and may finish long after this HTTP
	// handler returns. Capture the session up front so the audit entry
	// stamped on completion is attributed to the user that started the
	// download.
	user, role, sourceURL := sess.Username, sess.Role, req.URL
	pool := req.Pool
	id, err := system.LXDISOFetchStart(req.Pool, req.URL, req.Name, func(success bool, errMsg, finalName string) {
		target := pool + "/" + finalName
		details := "from " + sourceURL
		if success {
			audit.Log(audit.Entry{User: user, Role: role, Action: audit.ActionISOFetch, Target: target, Result: audit.ResultOK, Details: details})
			return
		}
		audit.Log(audit.Entry{User: user, Role: role, Action: audit.ActionISOFetch, Target: target, Result: audit.ResultError, Details: details + " — " + errMsg})
	})
	if err != nil {
		// Refused before the job started (invalid URL, bad filename, no
		// pool, etc.). Log the rejection so the activity log still has a
		// trace of the attempt and the reason.
		target := req.Pool + "/" + req.Name
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOFetch, Target: target, Result: audit.ResultError, Details: "from " + req.URL + " — " + err.Error()})
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]string{"job_id": id})
}

// HandleFetchISOProgress returns the current state of one fetch job.
// GET /api/lxd/isos/fetch/progress?job_id=…
func HandleFetchISOProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	job := system.LXDISOFetchJobGet(id)
	if job == nil {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	jsonOK(w, job)
}

// HandleUploadISO streams a multipart upload into the pool's .isos directory.
// POST /api/lxd/isos/upload?pool=<pool>
// Accepts multipart form field "file" with a .iso file.
func HandleUploadISO(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "pool is required")
		return
	}

	// Use streaming multipart reader so large ISO files are never fully
	// buffered in memory or written to a temp file by the framework.
	mr, err := r.MultipartReader()
	if err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOUpload, Target: pool + "/", Result: audit.ResultError, Details: "invalid multipart request"})
		jsonErr(w, http.StatusBadRequest, "invalid multipart request")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "file" {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOUpload, Target: pool + "/", Result: audit.ResultError, Details: "missing file field"})
		jsonErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	filename := filepath.Base(part.FileName())
	target := pool + "/" + filename
	if !strings.HasSuffix(strings.ToLower(filename), ".iso") {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOUpload, Target: target, Result: audit.ResultError, Details: "only .iso files are accepted"})
		jsonErr(w, http.StatusBadRequest, "only .iso files are accepted")
		return
	}
	if _, err := system.LXDSaveISO(pool, filename, part); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOUpload, Target: target, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionISOUpload, Target: target, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"name": filename})
}
