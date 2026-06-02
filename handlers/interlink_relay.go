package handlers

import (
	"net/http"
	"strings"
	"zfsnas/internal/config"

	"github.com/gorilla/mux"
)

// HandleInterlinkRelay is a generic, per-request forwarder to a *specific*
// linked peer, selected by the {server_id} path segment — unlike
// RelayMiddleware, it does NOT depend on the session's global relay state.
// Everything after the /interlink-relay/<server_id> prefix is forwarded
// verbatim to the peer (HTTP proxied, WebSocket bridged) with HMAC-signed
// relay identity headers.
//
// This lets an embedded VGA console tab target a peer VM's API + SPICE
// endpoints (/api/lxd/instances/..., /ws/lxd-vga, ...) while the user stays on
// their home portal, so a peer VGA tab can sit alongside local ones at the
// same time.
//
// Mounted under: /interlink-relay/{server_id}/...  (RequireAuth)
func HandleInterlinkRelay(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)
		serverID := mux.Vars(r)["server_id"]
		if serverID == "" {
			http.Error(w, "server_id required", http.StatusBadRequest)
			return
		}

		// Resolve the peer.
		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == serverID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			http.Error(w, "linked server not found", http.StatusNotFound)
			return
		}

		// Strip the /interlink-relay/<id> prefix; the remainder is the real
		// target path on the peer. Mutating r.URL.Path means both the HTTP
		// proxy and relayWebSocket (which read r.URL.RequestURI()) forward
		// the correct path + query.
		rest := strings.TrimPrefix(r.URL.Path, "/interlink-relay/"+serverID)
		if rest == "" || rest[0] != '/' {
			rest = "/" + rest
		}
		r.URL.Path = rest
		// Clear the (now-stale) escaped form so RequestURI() re-derives it
		// from the rewritten Path instead of forwarding the old prefix.
		r.URL.RawPath = ""

		ts, nonceHex, sig := relayIdentityHeaders(ls, sess.Username)

		// WebSocket (SPICE / consoles) — bridge bidirectionally.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			relayWebSocket(w, r, ls, sess.Username, ts, nonceHex, sig)
			return
		}

		// HTTP — proxy the (now prefix-stripped) request URI to the peer.
		proxyHTTPToPeer(w, r, ls, r.URL.RequestURI(), sess.Username, ts, nonceHex, sig)
	}
}
