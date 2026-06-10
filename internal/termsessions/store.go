// Package termsessions implements persistent PTY-backed terminal sessions
// for ZNAS v6.5.30+. A Session keeps its PTY alive across WebSocket
// disconnects so the user can close the browser, come back, and resume the
// same shell. Lifetime is bounded by:
//   1. PTY EOF (target process exits — VM reboot, `exit` typed, etc.)
//   2. Explicit Terminate (user clicks the "x" on a tab)
//   3. TerminateUser (web session evicted — idle timeout, logout, hard cap)
//
// Server restarts are NOT survived — PTYs are kernel resources owned by
// the ZNAS process; a restart wipes them all. This is a documented limit
// (see PLANS/plan-version-6.5.30.md).
package termsessions

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// oscQueryRe matches OSC sequences that ask the terminal to REPORT something:
// they carry a "?" before the BEL or ST terminator — e.g. a background-colour
// query "\x1b]11;?\x07" (also foreground ]10, cursor ]12, palette ]4;N).
var oscQueryRe = regexp.MustCompile("\x1b\\][0-9;]*\\?(\x07|\x1b\\\\)")

// csiQueries are exact CSI report-requests (cursor position, device status,
// device attributes) that, like the OSC colour queries, make the terminal
// answer back.
var csiQueries = [][]byte{
	[]byte("\x1b[6n"), []byte("\x1b[5n"),
	[]byte("\x1b[c"), []byte("\x1b[0c"),
	[]byte("\x1b[>c"), []byte("\x1b[>0c"), []byte("\x1b[=c"),
}

// stripScrollbackQueries removes terminal-report REQUESTS from replayed
// scrollback. A program (vim, a prompt, dircolors…) may have queried the
// terminal's colours/cursor while running; those query bytes are invisible but
// get stored in the scrollback ring. On reconnect we replay the ring, the
// client's emulator re-answers the query, and the answer ("\x1b]11;rgb:fafa/
// fafa/fafa\x07") is injected into the shell at the prompt — where readline
// renders the printable part as garbage like "11;rgb:fafa/fafa/fafa". Dropping
// the queries from the replay (only) prevents the spurious re-answer without
// changing anything the user sees.
func stripScrollbackQueries(b []byte) []byte {
	if !bytes.Contains(b, []byte{0x1b}) {
		return b
	}
	b = oscQueryRe.ReplaceAll(b, nil)
	for _, q := range csiQueries {
		if bytes.Contains(b, q) {
			b = bytes.ReplaceAll(b, q, nil)
		}
	}
	return b
}

// Session reason constants — passed to Terminate / surfaced in the final
// scrollback line so the UI can render an honest reason.
const (
	ReasonProcessExit   = "process_exit"     // PTY EOF (VM down, exit typed, host shell quit)
	ReasonUserClose     = "user_close"       // explicit close from the UI
	ReasonSessionExpire = "session_expired"  // web session evicted
	ReasonReplaced      = "replaced_by_new"  // (unused today, reserved)
	ReasonShutdown      = "server_shutdown"
)

// Kind constants — descriptive label only; nothing in this package branches
// on them. Handlers use them so the UI can render an icon.
const (
	KindHost    = "host"    // /ws/terminal
	KindLXD     = "lxd"     // /ws/lxd-console
	KindCompose = "compose" // /ws/compose-console
	KindDocker  = "docker"  // /ws/docker-console
)

// SpawnFunc returns a started *exec.Cmd and its controlling PTY. Sessions
// call this exactly once at New() time.
type SpawnFunc func() (*exec.Cmd, *os.File, error)

// Snapshot is the lightweight, copy-safe view of a session — what the
// list-sessions API returns and what the UI binds to.
type Snapshot struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
	Terminated bool      `json:"terminated"`
	TermReason string    `json:"term_reason,omitempty"`
	Attached   bool      `json:"attached"`
	Cols       uint16    `json:"cols"`
	Rows       uint16    `json:"rows"`
}

// Session owns one PTY + its scrollback ring + the currently-attached WS.
type Session struct {
	id, userID, kind, target, title string
	createdAt                       time.Time

	mu          sync.Mutex
	lastActive  time.Time
	ptmx        *os.File
	cmd         *exec.Cmd
	scrollback  *ringBuf
	attachedWS  *websocket.Conn
	wsWriteMu   sync.Mutex      // serializes WriteMessage / WriteControl on attachedWS (gorilla/websocket requires this)
	attachWG    *sync.WaitGroup // signals the current attach loop's goroutines to exit
	cols, rows  uint16
	terminated  bool
	termReason  string
	evictedTimer *time.Timer
}

// safeWriteWS performs a serialized write to whichever WS the session
// is currently attached to. Returns ok=false if there is no attached
// WS (caller decides whether that's an error). Holding wsWriteMu while
// briefly capturing attachedWS keeps drainPTY, server pings, kick
// notices, and Attach's scrollback replay from interleaving on the
// gorilla/websocket conn (which forbids concurrent writers).
func (sess *Session) safeWriteWS(messageType int, data []byte) (ok bool) {
	sess.wsWriteMu.Lock()
	defer sess.wsWriteMu.Unlock()
	sess.mu.Lock()
	ws := sess.attachedWS
	sess.mu.Unlock()
	if ws == nil {
		return false
	}
	ws.WriteMessage(messageType, data) //nolint:errcheck
	return true
}

// Store is the per-process session registry.
type Store struct {
	mu            sync.Mutex
	sessions      map[string]*Session
	scrollbackKB  int // 256 by default
	maxPerUser    int // 20 by default
}

// Default is the global store; main.go configures it at startup.
var Default = &Store{
	sessions:     make(map[string]*Session),
	scrollbackKB: 256,
	maxPerUser:   20,
}

// Configure sets the per-session ring buffer size (in KB) and the per-user
// session cap. Pass 0 to keep the existing default for either.
func (s *Store) Configure(scrollbackKB, maxPerUser int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scrollbackKB > 0 {
		s.scrollbackKB = scrollbackKB
	}
	if maxPerUser > 0 {
		s.maxPerUser = maxPerUser
	}
}

// New creates a session and starts the PTY via spawn(). Returns the new
// session (already in the store, but not yet attached to a WebSocket).
func (s *Store) New(userID, kind, target, title string, spawn SpawnFunc) (*Session, error) {
	s.mu.Lock()
	cap := s.scrollbackKB * 1024
	maxPerUser := s.maxPerUser
	s.mu.Unlock()

	// Enforce per-user cap by soft-evicting the oldest detached session.
	s.enforceUserCap(userID, maxPerUser)

	cmd, ptmx, err := spawn()
	if err != nil {
		return nil, err
	}

	idBytes := make([]byte, 12)
	rand.Read(idBytes) //nolint:errcheck
	now := time.Now()
	sess := &Session{
		id:         hex.EncodeToString(idBytes),
		userID:     userID,
		kind:       kind,
		target:     target,
		title:      title,
		createdAt:  now,
		lastActive: now,
		ptmx:       ptmx,
		cmd:        cmd,
		scrollback: newRingBuf(cap),
		cols:       80,
		rows:       24,
	}

	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()

	// Start the PTY → scrollback drain. This runs for the life of the PTY
	// regardless of whether anyone is attached; the same goroutine also
	// fans out to the currently-attached WebSocket if there is one.
	go s.drainPTY(sess)

	return sess, nil
}

// drainPTY is the long-lived goroutine: read PTY → append to ring → maybe
// fan out to current WS. Exits on PTY EOF/error, after which it marks the
// session terminated and schedules eviction.
func (s *Store) drainPTY(sess *Session) {
	buf := make([]byte, 4096)
	for {
		n, err := sess.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			sess.mu.Lock()
			sess.scrollback.Write(chunk) //nolint:errcheck
			sess.lastActive = time.Now()
			sess.mu.Unlock()
			sess.safeWriteWS(websocket.BinaryMessage, chunk)
		}
		if err != nil {
			s.markTerminated(sess, ReasonProcessExit)
			return
		}
	}
}

// markTerminated flips the session's terminated bit, drops the final notice
// into scrollback, kicks any attached WS, then schedules eviction after a
// grace period so a reconnecting browser still sees the death message.
func (s *Store) markTerminated(sess *Session, reason string) {
	sess.mu.Lock()
	if sess.terminated {
		sess.mu.Unlock()
		return
	}
	sess.terminated = true
	sess.termReason = reason
	notice := []byte(fmt.Sprintf("\r\n\x1b[33m[session ended: %s]\x1b[0m\r\n", reason))
	sess.scrollback.Write(notice) //nolint:errcheck
	// Don't clear attachedWS yet — safeWriteWS needs it. We'll clear
	// after the final notice is sent.
	ws := sess.attachedWS
	if sess.cmd != nil && sess.cmd.Process != nil {
		sess.cmd.Process.Kill() //nolint:errcheck
	}
	sess.ptmx.Close()
	if sess.evictedTimer != nil {
		sess.evictedTimer.Stop()
	}
	sess.evictedTimer = time.AfterFunc(5*time.Minute, func() {
		s.mu.Lock()
		delete(s.sessions, sess.id)
		s.mu.Unlock()
	})
	sess.mu.Unlock()
	if ws != nil {
		sess.safeWriteWS(websocket.BinaryMessage, notice)
		sess.mu.Lock()
		sess.attachedWS = nil
		sess.mu.Unlock()
		ws.Close()
	}
}

// Terminate explicitly closes a session (UI clicked "x").
func (s *Store) Terminate(id, reason string) {
	s.mu.Lock()
	sess := s.sessions[id]
	s.mu.Unlock()
	if sess == nil {
		return
	}
	s.markTerminated(sess, reason)
}

// TerminateUser kills every session owned by userID. Called from the
// session-eviction hook so terminals die with the web session.
func (s *Store) TerminateUser(userID, reason string) {
	s.mu.Lock()
	var victims []*Session
	for _, sess := range s.sessions {
		if sess.userID == userID {
			victims = append(victims, sess)
		}
	}
	s.mu.Unlock()
	for _, v := range victims {
		s.markTerminated(v, reason)
	}
}

// ListForUser returns snapshots of all sessions belonging to userID, sorted
// oldest-first (UI tab order).
func (s *Store) ListForUser(userID string) []Snapshot {
	s.mu.Lock()
	var owned []*Session
	for _, sess := range s.sessions {
		if sess.userID == userID {
			owned = append(owned, sess)
		}
	}
	s.mu.Unlock()
	sort.Slice(owned, func(i, j int) bool { return owned[i].createdAt.Before(owned[j].createdAt) })
	out := make([]Snapshot, 0, len(owned))
	for _, sess := range owned {
		out = append(out, sess.snapshot())
	}
	return out
}

// Get returns a session by ID, or nil.
func (s *Store) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// Keepalive cadence — server-initiated WebSocket pings keep dead TCP
// connections detectable. Without them, an iPad in the background for
// 10+ minutes silently has its WS killed by the OS/NAT and neither the
// browser nor the server notice until the next write fails — which is
// exactly the "frozen terminal" the user reported. With ping/pong:
//   • server pings every pingInterval
//   • browser auto-replies pong (built-in WebSocket behaviour)
//   • server's read deadline gets extended on each pong via PongHandler
//   • if pongs stop, read deadline expires → conn errors → onclose fires
//     on the browser → the client-side auto-reconnect kicks in.
const (
	pingInterval = 25 * time.Second
	readDeadline = 90 * time.Second
)

// Attach replaces (or sets) the WebSocket attached to sess. The previous
// WS, if any, is kicked with a notice. The scrollback ring is replayed on
// the new WS as one binary frame, then this call blocks reading input
// from the WS into the PTY until the WS closes or the session terminates.
//
// cols/rows are the client's reported terminal size; we issue an immediate
// resize so the PTY matches the new viewport.
func (s *Store) Attach(sess *Session, ws *websocket.Conn, cols, rows uint16) {
	// Replace existing attachment first.
	sess.mu.Lock()
	if sess.terminated {
		// Still send the scrollback (which contains the death notice) so
		// the user sees what happened, then close.
		scroll := sess.scrollback.Snapshot()
		sess.mu.Unlock()
		if len(scroll) > 0 {
			ws.WriteMessage(websocket.BinaryMessage, stripScrollbackQueries(scroll)) //nolint:errcheck
		}
		ws.Close()
		return
	}
	if sess.attachedWS != nil {
		old := sess.attachedWS
		go func() {
			// Text frame first so the client can recognise the kick as
			// "another browser took over" and SKIP its auto-reconnect —
			// otherwise both browsers ping-pong attaches in a tight loop
			// (each onclose triggers a reconnect, the reconnect kicks
			// the other browser, etc.). These two writes go through the
			// session write-mutex so they can't collide with the ping
			// goroutine or drainPTY on the conn we're about to close.
			sess.wsWriteMu.Lock()
			old.WriteMessage(websocket.TextMessage,
				[]byte(`{"type":"kicked","reason":"another_browser"}`)) //nolint:errcheck
			old.WriteMessage(websocket.BinaryMessage,
				[]byte("\r\n\x1b[33m[disconnected: another browser took over — press Enter to resume here]\x1b[0m\r\n")) //nolint:errcheck
			sess.wsWriteMu.Unlock()
			old.Close()
		}()
	}
	sess.attachedWS = ws
	if cols > 0 && rows > 0 {
		sess.cols, sess.rows = cols, rows
	}
	scroll := sess.scrollback.Snapshot()
	cur := sess.cols
	cur2 := sess.rows
	sess.mu.Unlock()

	// v6.5.30 — keepalive ping/pong. Set a read deadline; every pong
	// extends it. The ping ticker fires every pingInterval; if the
	// network is dead, the next ping write fails AND the read deadline
	// expires, ws.ReadMessage in the loop below returns an error, we
	// detach and the client's onclose-driven auto-reconnect re-attaches
	// to this same session (server replays scrollback).
	ws.SetReadDeadline(time.Now().Add(readDeadline))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(readDeadline))
		return nil
	})
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sess.wsWriteMu.Lock()
				err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				sess.wsWriteMu.Unlock()
				if err != nil {
					return
				}
			case <-pingDone:
				return
			}
		}
	}()
	defer close(pingDone)

	// Replay scrollback before any new output races in. The drainPTY
	// goroutine will write subsequent chunks to this same WS. Go through
	// safeWriteWS so the ping goroutine and any in-flight drainPTY
	// chunks can't interleave on the same conn.
	if len(scroll) > 0 {
		// Strip terminal report-requests so the client's emulator doesn't
		// re-answer them onto the shell prompt (e.g. "11;rgb:fafa/fafa/fafa").
		sess.safeWriteWS(websocket.BinaryMessage, stripScrollbackQueries(scroll))
	}
	if cur > 0 && cur2 > 0 {
		pty.Setsize(sess.ptmx, &pty.Winsize{Cols: cur, Rows: cur2}) //nolint:errcheck
	}

	// Pump WS → PTY until WS dies. The PTY → WS direction runs in drainPTY.
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.TextMessage {
			// Try resize control message.
			if c, r2, ok := parseResize(data); ok {
				sess.mu.Lock()
				sess.cols, sess.rows = c, r2
				sess.mu.Unlock()
				pty.Setsize(sess.ptmx, &pty.Winsize{Cols: c, Rows: r2}) //nolint:errcheck
				continue
			}
		}
		io.Copy(sess.ptmx, sliceReader(data)) //nolint:errcheck
		sess.mu.Lock()
		sess.lastActive = time.Now()
		sess.mu.Unlock()
	}

	// WS closed — detach but leave the PTY running.
	sess.mu.Lock()
	if sess.attachedWS == ws {
		sess.attachedWS = nil
	}
	sess.mu.Unlock()
}

// enforceUserCap evicts the oldest detached session above the cap.
func (s *Store) enforceUserCap(userID string, max int) {
	if max <= 0 {
		return
	}
	s.mu.Lock()
	var owned []*Session
	for _, sess := range s.sessions {
		if sess.userID == userID && !sess.terminated {
			owned = append(owned, sess)
		}
	}
	s.mu.Unlock()
	if len(owned) < max {
		return
	}
	// Sort by createdAt asc — oldest first.
	sort.Slice(owned, func(i, j int) bool { return owned[i].createdAt.Before(owned[j].createdAt) })
	excess := len(owned) - max + 1 // +1 to leave room for the new one
	for i := 0; i < excess; i++ {
		// Prefer detached victims; only fall through to attached if all are attached.
		victim := owned[i]
		s.markTerminated(victim, ReasonUserClose)
	}
}

func (sess *Session) snapshot() Snapshot {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return Snapshot{
		ID:         sess.id,
		UserID:     sess.userID,
		Kind:       sess.kind,
		Target:     sess.target,
		Title:      sess.title,
		CreatedAt:  sess.createdAt,
		LastActive: sess.lastActive,
		Terminated: sess.terminated,
		TermReason: sess.termReason,
		Attached:   sess.attachedWS != nil,
		Cols:       sess.cols,
		Rows:       sess.rows,
	}
}

// ID returns the session ID.
func (sess *Session) ID() string { return sess.id }

// UserID returns the owning user.
func (sess *Session) UserID() string { return sess.userID }

// ── small helpers ─────────────────────────────────────────────────────────────

type sliceReaderT struct{ b []byte; pos int }

func sliceReader(b []byte) io.Reader { return &sliceReaderT{b: b} }

func (r *sliceReaderT) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
