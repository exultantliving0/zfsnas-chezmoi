package handlers

import (
	"encoding/json"
	"net/http"

	"zfsnas/internal/termsessions"

	"github.com/gorilla/mux"
)

// HandleListTerminalSessions lists every live terminal session owned by
// the calling user. The new multi-tab terminal page calls this on load to
// reattach existing sessions; the in-portal bottom terminal uses it to
// rehydrate its tab strip across page reloads.
// GET /api/terminal-sessions
func HandleListTerminalSessions(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	list := termsessions.Default.ListForUser(sess.UserID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list) //nolint:errcheck
}

// HandleCloseTerminalSession explicitly closes one session — the "x" on a
// tab. Ownership is enforced; another user's session ID returns 404.
// POST /api/terminal-sessions/{id}/close
func HandleCloseTerminalSession(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	id := mux.Vars(r)["id"]
	target := termsessions.Default.Get(id)
	if target == nil || target.UserID() != sess.UserID {
		jsonErr(w, http.StatusNotFound, "session not found")
		return
	}
	termsessions.Default.Terminate(id, termsessions.ReasonUserClose)
	jsonOK(w, map[string]bool{"ok": true})
}
