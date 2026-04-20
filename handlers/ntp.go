package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// HandleNTPStatus returns the current chrony sync status.
func HandleNTPStatus(w http.ResponseWriter, r *http.Request) {
	st, err := system.GetChronyStatus()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, st)
}

// HandleGetNTPServers returns the server/pool lines from chrony.conf.
func HandleGetNTPServers(w http.ResponseWriter, r *http.Request) {
	servers, err := system.GetNTPServers()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]interface{}{"servers": servers})
}

// HandleSetNTPServers replaces the server/pool entries and restarts chronyd.
func HandleSetNTPServers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servers []string `json:"servers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	for _, s := range req.Servers {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "server ") && !strings.HasPrefix(s, "pool ") {
			jsonErr(w, http.StatusBadRequest, "each entry must start with 'server ' or 'pool '")
			return
		}
	}
	if err := system.SetNTPServers(req.Servers); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionUpdateSettings,
		Result:  audit.ResultOK,
		Details: "NTP servers updated",
	})
	jsonOK(w, map[string]string{"message": "NTP servers updated"})
}
