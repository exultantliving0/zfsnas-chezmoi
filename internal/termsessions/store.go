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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

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
	attachWG    *sync.WaitGroup // signals the current attach loop's goroutines to exit
	cols, rows  uint16
	terminated  bool
	termReason  string
	evictedTimer *time.Timer
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
			ws := sess.attachedWS
			sess.mu.Unlock()
			if ws != nil {
				_ = ws.WriteMessage(websocket.BinaryMessage, chunk)
			}
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
	ws := sess.attachedWS
	sess.attachedWS = nil
	if sess.cmd != nil && sess.cmd.Process != nil {
		sess.cmd.Process.Kill() //nolint:errcheck
	}
	sess.ptmx.Close()
	// Schedule evict after 5 min so a reattach window still sees the notice.
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
		ws.WriteMessage(websocket.BinaryMessage, notice) //nolint:errcheck
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
			ws.WriteMessage(websocket.BinaryMessage, scroll) //nolint:errcheck
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
			// the other browser, etc.).
			old.WriteMessage(websocket.TextMessage,
				[]byte(`{"type":"kicked","reason":"another_browser"}`)) //nolint:errcheck
			old.WriteMessage(websocket.BinaryMessage,
				[]byte("\r\n\x1b[33m[disconnected: another browser took over]\x1b[0m\r\n")) //nolint:errcheck
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

	// Replay scrollback before any new output races in. The drainPTY
	// goroutine will write subsequent chunks to this same WS.
	if len(scroll) > 0 {
		ws.WriteMessage(websocket.BinaryMessage, scroll) //nolint:errcheck
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
