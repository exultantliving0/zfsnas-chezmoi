package handlers

import (
	"encoding/json"
	"net/http"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleFileBrowserList lists the contents of a directory within a validated root.
// GET /api/files/list?root=<base64url>&subpath=<relative>
func HandleFileBrowserList(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("root")
	if token == "" {
		jsonErr(w, http.StatusBadRequest, "missing root parameter")
		return
	}
	subpath := r.URL.Query().Get("subpath")

	knownRoots, err := system.ResolveKnownRoots(config.Dir())
	if err != nil && len(knownRoots) == 0 {
		jsonErr(w, http.StatusInternalServerError, "could not resolve known roots")
		return
	}
	absRoot, label, err := system.ValidateRootToken(token, knownRoots)
	if err != nil {
		jsonErr(w, http.StatusForbidden, err.Error())
		return
	}

	result, err := system.ListDir(absRoot, subpath, label)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, result)
}

// HandleFileBrowserUsersGroups returns system user and group name lists.
// GET /api/files/users-groups
func HandleFileBrowserUsersGroups(w http.ResponseWriter, r *http.Request) {
	users, groups, err := system.GetSystemUsersGroups()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]interface{}{
		"users":  users,
		"groups": groups,
	})
}

// HandleFileBrowserChown changes ownership of a file or directory.
// POST /api/files/chown (admin only)
func HandleFileBrowserChown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root      string `json:"root"`
		Subpath   string `json:"subpath"`
		Owner     string `json:"owner"`
		Group     string `json:"group"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Owner == "" || req.Group == "" {
		jsonErr(w, http.StatusBadRequest, "owner and group are required")
		return
	}

	knownRoots, err := system.ResolveKnownRoots(config.Dir())
	if err != nil && len(knownRoots) == 0 {
		jsonErr(w, http.StatusInternalServerError, "could not resolve known roots")
		return
	}
	absRoot, _, err := system.ValidateRootToken(req.Root, knownRoots)
	if err != nil {
		jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	absPath, err := system.SafeJoin(absRoot, req.Subpath)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := system.ChownPath(absPath, req.Owner, req.Group, req.Recursive); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess, _ := SessionFromRequest(r)
	user := ""
	if sess != nil {
		user = sess.Username
	}
	details := "chown " + req.Owner + ":" + req.Group + " " + absPath
	if req.Recursive {
		details += " [recursive]"
	}
	audit.Log(audit.Entry{
		User:    user,
		Action:  audit.ActionFileBrowserChown,
		Target:  absPath,
		Result:  audit.ResultOK,
		Details: details,
	})

	jsonOK(w, map[string]bool{"ok": true})
}

// HandleFileBrowserChmod changes permissions of a file or directory.
// POST /api/files/chmod (admin only)
func HandleFileBrowserChmod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root      string `json:"root"`
		Subpath   string `json:"subpath"`
		Mode      string `json:"mode"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Mode == "" {
		jsonErr(w, http.StatusBadRequest, "mode is required")
		return
	}

	knownRoots, err := system.ResolveKnownRoots(config.Dir())
	if err != nil && len(knownRoots) == 0 {
		jsonErr(w, http.StatusInternalServerError, "could not resolve known roots")
		return
	}
	absRoot, _, err := system.ValidateRootToken(req.Root, knownRoots)
	if err != nil {
		jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	absPath, err := system.SafeJoin(absRoot, req.Subpath)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := system.ChmodPath(absPath, req.Mode, req.Recursive); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess, _ := SessionFromRequest(r)
	user := ""
	if sess != nil {
		user = sess.Username
	}
	details := "chmod " + req.Mode + " " + absPath
	if req.Recursive {
		details += " [recursive]"
	}
	audit.Log(audit.Entry{
		User:    user,
		Action:  audit.ActionFileBrowserChmod,
		Target:  absPath,
		Result:  audit.ResultOK,
		Details: details,
	})

	jsonOK(w, map[string]bool{"ok": true})
}
