package handlers

import (
	"net/http"
	"os"
	"os/exec"
	"strings"

	"zfsnas/internal/termsessions"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

var termUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients (curl, etc.)
		}
		return strings.HasSuffix(origin, "://"+r.Host)
	},
}

// HandleTerminal opens a WebSocket attached to the user's host-shell
// terminal session. When the query string carries ?session_id=<uuid> we
// reattach to that existing PTY (replaying its scrollback); otherwise we
// create a new session via termsessions.New. WS disconnect now DETACHES
// — the PTY keeps running until the user explicitly closes it or their
// web session expires.
func HandleTerminal(w http.ResponseWriter, r *http.Request) {
	conn, err := termUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sess := MustSession(r)
	wsAttachOrCreate(conn, r, sess.UserID, termsessions.KindHost, "", "Host shell", func() (*exec.Cmd, *os.File, error) {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
		cmd := exec.Command(shell)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		ptmx, err := pty.Start(cmd)
		return cmd, ptmx, err
	})
}

// wsAttachOrCreate is the shared "find existing session or spawn a new
// one, then run the attach loop" used by every PTY-backed WS handler.
// Ownership is enforced — a session_id from another user is rejected.
// First text frame sent back to the client is a one-line JSON envelope
// with the session ID so the UI can persist it and tag the tab.
func wsAttachOrCreate(ws *websocket.Conn, r *http.Request,
	userID, kind, target, title string,
	spawn termsessions.SpawnFunc) {

	store := termsessions.Default
	id := r.URL.Query().Get("session_id")
	var ts *termsessions.Session
	reattach := false
	if id != "" {
		ts = store.Get(id)
		if ts == nil || ts.UserID() != userID {
			ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":"session not found"}`)) //nolint:errcheck
			return
		}
		reattach = true
	} else {
		var err error
		ts, err = store.New(userID, kind, target, title, spawn)
		if err != nil {
			ws.WriteMessage(websocket.TextMessage,
				[]byte(`{"type":"error","error":"failed to start: `+jsonEscape(err.Error())+`"}`)) //nolint:errcheck
			return
		}
	}
	// Initial envelope so the client learns its session ID.
	envelope := `{"type":"session","id":"` + ts.ID() + `","kind":"` + kind + `","reattach":` + boolJSON(reattach) + `}`
	ws.WriteMessage(websocket.TextMessage, []byte(envelope)) //nolint:errcheck

	// Pick up cols/rows from the query string if provided (the JS opens
	// the socket with ?cols=N&rows=M so the very first resize matches the
	// container before any data flows).
	cols := parseUint16(r.URL.Query().Get("cols"))
	rows := parseUint16(r.URL.Query().Get("rows"))

	store.Attach(ts, ws, cols, rows)
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func jsonEscape(s string) string {
	// Minimal escape for the inline envelope strings — backslashes + quotes.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func parseUint16(s string) uint16 {
	if s == "" {
		return 0
	}
	var n uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint32(c-'0')
		if n > 65535 {
			return 0
		}
	}
	return uint16(n)
}
