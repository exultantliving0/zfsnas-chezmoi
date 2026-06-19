package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"zfsnas/internal/audit"
	"zfsnas/system"
)

// HandleLXDTags returns the host's tag → color registry.
// GET /api/lxd/tags
func HandleLXDTags(w http.ResponseWriter, r *http.Request) {
	reg, err := system.LoadTagRegistry()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to load tag registry")
		return
	}
	jsonOK(w, map[string]interface{}{"registry": reg})
}

// HandleLXDSetInstanceTags replaces the tag list on an instance.
// PUT /api/lxd/instances/{name}/tags   body: {"tags": ["prod","db"]}
func HandleLXDSetInstanceTags(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	var req struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.SetInstanceTags(name, req.Tags); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultError, Details: "tags: " + err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultOK, Details: "updated tags"})
	// Return the canonical (normalized) tags so the client stays in sync.
	tags, _ := system.GetInstanceTags(name)
	jsonOK(w, map[string]interface{}{"tags": tags})
}

// HandleLXDSetTagColor recolors a tag globally (every instance carrying it).
// PUT /api/lxd/tags/{tag}/color   body: {"color": "#e5484d"}
func HandleLXDSetTagColor(w http.ResponseWriter, r *http.Request) {
	tag := mux.Vars(r)["tag"]
	sess := MustSession(r)
	var req struct {
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.SetTagColor(tag, req.Color); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: "tag:" + tag, Result: audit.ResultOK, Details: "recolor " + req.Color})
	reg, _ := system.LoadTagRegistry()
	jsonOK(w, map[string]interface{}{"registry": reg})
}
