package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultSessionDuration is the hard cap used for "Default 24-hour session"
// mode. InactivityHardCap is the much longer hard cap used for inactivity
// mode — sessions beyond this are dropped even if the user kept actively
// using the portal (defence in depth against indefinite credential reuse).
const (
	DefaultSessionDuration = 24 * time.Hour
	InactivityHardCap      = 30 * 24 * time.Hour
)

// Session holds session data for an authenticated user.
type Session struct {
	Token          string        `json:"token"`
	UserID         string        `json:"user_id"`
	Username       string        `json:"username"`
	Role           string        `json:"role"`
	CreatedAt      time.Time     `json:"created_at"`
	ExpiresAt      time.Time     `json:"expires_at"`        // hard cap (24h or 30d)
	LastActivityAt time.Time     `json:"last_activity_at"`  // bumped by Touch() on each authenticated request
	IdleTimeout    time.Duration `json:"idle_timeout"`      // 0 = no idle expiry; >0 = expire after this duration of inactivity
}

// Store is an in-memory session store. Optional encrypted persistence is
// wired via BindPersistence (see persist.go).
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	// Persistence hook — set by BindPersistence. Nil-safe; nothing happens
	// before persistence is wired or when a build doesn't enable it.
	persist *persistConfig
}

// Default is the shared global session store.
var Default = &Store{sessions: make(map[string]*Session)}

// Create generates a new session for the given user and stores it.
// idleTimeout > 0 enables inactivity-based expiry; 0 means the session is
// only bounded by ExpiresAt (the hard cap, set by the caller via the
// duration argument).
func (s *Store) Create(userID, username, role string, duration, idleTimeout time.Duration) (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		Token:          hex.EncodeToString(b),
		UserID:         userID,
		Username:       username,
		Role:           role,
		CreatedAt:      now,
		ExpiresAt:      now.Add(duration),
		LastActivityAt: now,
		IdleTimeout:    idleTimeout,
	}
	s.mu.Lock()
	s.sessions[sess.Token] = sess
	s.mu.Unlock()
	s.savePersistAsync()
	return sess, nil
}

// Get retrieves a session by token. Returns nil if not found, past hard
// cap, or idle past IdleTimeout (when configured).
func (s *Store) Get(token string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	now := time.Now()
	if now.After(sess.ExpiresAt) {
		s.Delete(token)
		return nil, false
	}
	if sess.IdleTimeout > 0 && now.Sub(sess.LastActivityAt) > sess.IdleTimeout {
		s.Delete(token)
		return nil, false
	}
	return sess, true
}

// Touch updates the session's last-activity timestamp. Safe to call on
// every authenticated request — purely in-memory; the persistence layer
// flushes on its own ticker / shutdown so this is essentially free.
func (s *Store) Touch(token string) {
	s.mu.Lock()
	if sess, ok := s.sessions[token]; ok {
		sess.LastActivityAt = time.Now()
	}
	s.mu.Unlock()
}

// Delete removes a session by token.
func (s *Store) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
	s.savePersistAsync()
}

// List returns all non-expired sessions.
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if now.Before(sess.ExpiresAt) {
			out = append(out, sess)
		}
	}
	return out
}

// CleanExpired removes all expired sessions (both hard-cap and idle).
func (s *Store) CleanExpired() {
	s.mu.Lock()
	now := time.Now()
	dropped := false
	for k, v := range s.sessions {
		if now.After(v.ExpiresAt) {
			delete(s.sessions, k)
			dropped = true
			continue
		}
		if v.IdleTimeout > 0 && now.Sub(v.LastActivityAt) > v.IdleTimeout {
			delete(s.sessions, k)
			dropped = true
		}
	}
	s.mu.Unlock()
	if dropped {
		s.savePersistAsync()
	}
}
