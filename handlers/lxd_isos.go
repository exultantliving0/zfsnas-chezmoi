package handlers

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
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
	pool := r.URL.Query().Get("pool")
	filename := mux.Vars(r)["filename"]
	if pool == "" || filename == "" {
		jsonErr(w, http.StatusBadRequest, "pool and filename are required")
		return
	}
	if err := system.LXDDeleteISO(pool, filename); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// HandleUploadISO streams a multipart upload into the pool's .isos directory.
// POST /api/lxd/isos/upload?pool=<pool>
// Accepts multipart form field "file" with a .iso file.
func HandleUploadISO(w http.ResponseWriter, r *http.Request) {
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "pool is required")
		return
	}

	// Use streaming multipart reader so large ISO files are never fully
	// buffered in memory or written to a temp file by the framework.
	mr, err := r.MultipartReader()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid multipart request")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "file" {
		jsonErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	filename := filepath.Base(part.FileName())
	if !strings.HasSuffix(strings.ToLower(filename), ".iso") {
		jsonErr(w, http.StatusBadRequest, "only .iso files are accepted")
		return
	}
	if _, err := system.LXDSaveISO(pool, filename, part); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"name": filename})
}
