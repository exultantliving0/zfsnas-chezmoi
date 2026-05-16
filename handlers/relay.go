package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"zfsnas/internal/config"
	"zfsnas/internal/session"
	"zfsnas/system"

	"github.com/gorilla/websocket"
)

// relaySessionKey is the context key used by RelayAuthMiddleware to inject
// a synthetic session for requests arriving via a relay proxy.
const relaySessionKey contextKey = "relay_session"

// relayBypassPrefixes lists path prefixes that are always served locally on
// Server A and never forwarded to the remote when relay mode is active.
var relayBypassPrefixes = []string{
	"/api/auth/",
	"/api/interlink/",
	"/api/push-interlink/",
	"/api/prefs", // user preferences (theme etc.) always belong to home server
	"/ws/",
	"/static/",
	"/setup",
	"/login",
	"/interlink-login",
	"/apple-touch-icon",
}

// relayWSForwardPaths are WebSocket paths that ARE relayed to the remote server
// despite /ws/ being in the bypass list above.
var relayWSForwardPaths = []string{
	"/ws/terminal",
	"/ws/binary-update-apply",
	"/ws/updates-apply",
	"/ws/replication/",
	"/ws/lxd-console",
	"/ws/lxd-vga",
}

// isRelayBypassed reports whether path should be served locally even when
// relay mode is active.
func isRelayBypassed(path string) bool {
	// WebSocket paths explicitly forwarded to the remote override the /ws/ bypass.
	for _, p := range relayWSForwardPaths {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	for _, prefix := range relayBypassPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	// Non-/api/ and non-/ws/ paths (SPA pages, root) are always served locally.
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/ws/") {
		return true
	}
	return false
}

// ── Server A: outbound relay proxy ──────────────────────────────────────────

// RelayMiddleware wraps the main router on Server A.  For sessions that are in
// relay mode it forwards API requests to the remote server with HMAC-signed
// identity headers and streams the remote response back to the browser.
// Paths in relayBypassPrefixes are always served locally.
func RelayMiddleware(appCfg *config.AppConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always serve local-only paths.
		if isRelayBypassed(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Read session token to look up relay state.
		cookie, err := r.Cookie("zfsnas_session")
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := session.Default.Get(cookie.Value)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		relay := session.GetRelay(cookie.Value)
		if relay == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Look up the linked server.
		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == relay.ServerID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			// Linked server no longer exists — exit relay mode, serve locally.
			session.ClearRelay(cookie.Value)
			next.ServeHTTP(w, r)
			return
		}

		// Build HMAC-signed relay identity headers.
		nonce := make([]byte, 8)
		rand.Read(nonce) //nolint:errcheck
		ts := time.Now().Unix()
		nonceHex := hex.EncodeToString(nonce)
		sig := system.RelayForwardHMAC(ls.SharedSecret, sess.Username, ts, nonceHex)

		// WebSocket upgrade — bridge bidirectionally instead of HTTP proxy.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			relayWebSocket(w, r, ls, sess.Username, ts, nonceHex, sig)
			return
		}

		// Build the proxied request.
		targetURL := ls.URL + r.URL.RequestURI()
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "relay: failed to build proxy request")
			return
		}
		// Forward headers except Cookie (local session must never reach the remote) and
		// Origin (browser sends Origin: https://server-a which would fail EnforceOrigin
		// on Server B whose Host is server-b — strip it so Server B treats the request
		// as a trusted server-to-server call, which it is).
		for k, vv := range r.Header {
			if strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Origin") {
				continue
			}
			for _, v := range vv {
				proxyReq.Header.Add(k, v)
			}
		}
		// Inject relay identity headers.
		proxyReq.Header.Set("X-Interlink-Relay-User", sess.Username)
		proxyReq.Header.Set("X-Interlink-Relay-TS", strconv.FormatInt(ts, 10))
		proxyReq.Header.Set("X-Interlink-Relay-Nonce", nonceHex)
		proxyReq.Header.Set("X-Interlink-Relay-HMAC", sig)

		// Execute via the TLS-pinned interlink transport.
		client := system.InterlinkClientForRelay(ls.TLSFingerprint)
		resp, err := client.Do(proxyReq)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "relay: remote unreachable: "+err.Error())
			return
		}
		defer resp.Body.Close()

		// Copy response (skip Set-Cookie — remote must not set cookies on the browser).
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Set-Cookie") {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
	})
}

// relayWebSocket bridges a browser WebSocket connection to the remote server.
// Server A upgrades the browser connection, dials Server B with HMAC-signed
// headers, then forwards frames in both directions until either side closes.
func relayWebSocket(w http.ResponseWriter, r *http.Request, ls *config.LinkedServer, username string, ts int64, nonceHex, sig string) {
	// Capture requested subprotocols before upgrading so we can mirror them to Server B.
	requestedProtos := websocket.Subprotocols(r)

	upgrader := websocket.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: requestedProtos,
	}
	browserConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer browserConn.Close()

	// Convert https:// → wss://, http:// → ws://.
	wsBase := ls.URL
	if strings.HasPrefix(wsBase, "https://") {
		wsBase = "wss://" + wsBase[len("https://"):]
	} else if strings.HasPrefix(wsBase, "http://") {
		wsBase = "ws://" + wsBase[len("http://"):]
	}
	wsURL := wsBase + r.URL.RequestURI()

	dialHeader := http.Header{}
	dialHeader.Set("X-Interlink-Relay-User", username)
	dialHeader.Set("X-Interlink-Relay-TS", strconv.FormatInt(ts, 10))
	dialHeader.Set("X-Interlink-Relay-Nonce", nonceHex)
	dialHeader.Set("X-Interlink-Relay-HMAC", sig)
	// Subprotocols are forwarded via the Dialer.Subprotocols field below —
	// gorilla/websocket inserts the Sec-WebSocket-Protocol header itself.
	// Setting it explicitly here too triggers a "duplicate header not
	// allowed: Sec-Websocket-Protocol" error from gorilla, which broke
	// the VGA console relay specifically (SPICE is the only relayed WS
	// that requests a subprotocol — "binary" — so other relayed paths
	// like /ws/terminal and /ws/lxd-console were unaffected).

	dialer := websocket.Dialer{
		TLSClientConfig: system.InterlinkTLSConfigForRelay(ls.TLSFingerprint),
		Subprotocols:    requestedProtos,
	}
	remoteConn, _, err := dialer.Dial(wsURL, dialHeader)
	if err != nil {
		browserConn.WriteMessage(websocket.TextMessage, []byte("relay: could not connect to remote console: "+err.Error())) //nolint:errcheck
		return
	}
	defer remoteConn.Close()

	errc := make(chan error, 2)
	go func() {
		for {
			mt, msg, err := remoteConn.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			if err := browserConn.WriteMessage(mt, msg); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		for {
			mt, msg, err := browserConn.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			if err := remoteConn.WriteMessage(mt, msg); err != nil {
				errc <- err
				return
			}
		}
	}()
	<-errc
}

// ── Server B: inbound relay auth ─────────────────────────────────────────────

// RelayAuthMiddleware wraps the router on Server B.  When an inbound request
// carries relay identity headers it validates the HMAC against all known linked
// server secrets and injects a synthetic *session.Session into the context so
// that RequireAuth accepts the request without a browser cookie.
// Requests without relay headers pass through untouched (normal cookie auth).
func RelayAuthMiddleware(appCfg *config.AppConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := r.Header.Get("X-Interlink-Relay-User")
		if username == "" {
			next.ServeHTTP(w, r)
			return
		}

		tsStr := r.Header.Get("X-Interlink-Relay-TS")
		nonce := r.Header.Get("X-Interlink-Relay-Nonce")
		sig := r.Header.Get("X-Interlink-Relay-HMAC")

		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			jsonErr(w, http.StatusUnauthorized, "relay: invalid timestamp")
			return
		}

		// Reject stale requests (±30 s — same window as all other interlink endpoints).
		age := time.Since(time.Unix(ts, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "relay: request timestamp out of range")
			return
		}

		// Validate HMAC against all known linked server shared secrets.
		var matched bool
		for _, ls := range appCfg.InterLink {
			expected := system.RelayForwardHMAC(ls.SharedSecret, username, ts, nonce)
			if hmac.Equal([]byte(expected), []byte(sig)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "relay: invalid HMAC")
			return
		}

		// Resolve the user from Server B's own user list (B's role applies).
		users, loadErr := config.LoadUsers()
		if loadErr != nil {
			jsonErr(w, http.StatusInternalServerError, "relay: failed to load users")
			return
		}
		user := config.FindUserByUsername(users, username)
		if user == nil || user.Role == config.RoleSMBOnly {
			jsonErr(w, http.StatusForbidden, "relay: user not authorised on this server")
			return
		}

		// Inject a synthetic session under relaySessionKey.  RequireAuth will
		// promote it to the regular sessionKey so all downstream handlers work
		// unchanged.
		syntheticSess := &session.Session{
			UserID:   user.ID,
			Username: user.Username,
			Role:     user.Role,
		}
		ctx := context.WithValue(r.Context(), relaySessionKey, syntheticSess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
