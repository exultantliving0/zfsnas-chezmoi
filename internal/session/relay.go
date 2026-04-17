package session

import "sync"

// RelayState holds which remote server a session is currently relaying to.
type RelayState struct {
	ServerID string // config.LinkedServer.ID
	Hostname string // human-readable remote name for the UI banner
}

var (
	relayMu    sync.RWMutex
	relayStore = map[string]*RelayState{} // session token → relay state
)

// SetRelay marks a session as being in relay mode for the given remote server.
func SetRelay(token string, state *RelayState) {
	relayMu.Lock()
	defer relayMu.Unlock()
	relayStore[token] = state
}

// GetRelay returns the relay state for a session token, or nil if not in relay mode.
func GetRelay(token string) *RelayState {
	relayMu.RLock()
	defer relayMu.RUnlock()
	return relayStore[token]
}

// ClearRelay removes the relay state for a session token (exits relay mode).
func ClearRelay(token string) {
	relayMu.Lock()
	defer relayMu.Unlock()
	delete(relayStore, token)
}
