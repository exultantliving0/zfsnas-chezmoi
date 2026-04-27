package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/internal/interlink"
	"zfsnas/internal/session"
	"zfsnas/internal/totp"
	"zfsnas/internal/version"
	"zfsnas/system"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
)

// ─── online-status cache ─────────────────────────────────────────────────────

type serverStatus struct {
	Online        bool
	RemoteVersion string
	LXDEnabled    bool
	FetchedAt     time.Time
}

var (
	statusCacheMu  sync.Mutex
	statusCache    = map[string]serverStatus{} // keyed by LinkedServer.ID
	statusCacheTTL = 30 * time.Second
)

func cachedStatus(id string) (serverStatus, bool) {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()
	s, ok := statusCache[id]
	if !ok || time.Since(s.FetchedAt) > statusCacheTTL {
		return serverStatus{}, false
	}
	return s, true
}

func setStatus(id string, s serverStatus) {
	statusCacheMu.Lock()
	defer statusCacheMu.Unlock()
	statusCache[id] = s
}

// ─── inbound endpoints ───────────────────────────────────────────────────────

// HandleInterlinkPing handles GET /api/interlink/ping — no auth required.
func HandleInterlinkPing(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	jsonOK(w, map[string]interface{}{
		"hostname":    hostname,
		"version":     version.Version,
		"lxd_enabled": isLXDAvailable(),
		"ok":          true,
	})
}

// HandleInterlinkAcceptLink handles POST /api/interlink/accept-link — no session auth.
// Validates remote admin credentials, stores link, returns local ID + hostname.
func HandleInterlinkAcceptLink(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.AcceptLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.CallerURL == "" || req.SharedSecret == "" || req.AdminUsername == "" {
			jsonErr(w, http.StatusBadRequest, "missing required fields")
			return
		}

		users, err := config.LoadUsers()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to load users")
			return
		}
		user := config.FindUserByUsername(users, req.AdminUsername)
		if user == nil || user.Role != config.RoleAdmin {
			jsonErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.AdminPassword)); err != nil {
			jsonErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		// If TOTP is enabled on the admin account, require the code.
		if user.TOTPEnabled {
			if req.AdminTOTP == "" {
				jsonOK(w, system.AcceptLinkResponse{TOTPNeeded: true})
				return
			}
			if !totp.Verify(user.TOTPSecret, req.AdminTOTP) {
				jsonErr(w, http.StatusUnauthorized, "invalid 2FA code")
				return
			}
		}

		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "localhost"
		}

		// Idempotent: if already linked by this caller URL, return the existing IDs.
		for _, ls := range appCfg.InterLink {
			if ls.URL == req.CallerURL {
				jsonOK(w, system.AcceptLinkResponse{RemoteID: ls.ID, Hostname: hostname})
				return
			}
		}

		localID := newID()
		appCfg.InterLink = append(appCfg.InterLink, config.LinkedServer{
			ID:           localID,
			URL:          req.CallerURL,
			Hostname:     req.CallerHostname,
			SharedSecret: req.SharedSecret,
			RemoteID:     req.CallerID,
			LinkedBy:     req.AdminUsername,
			LinkedAt:     time.Now(),
		})
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}

		audit.Log(audit.Entry{
			User:    req.AdminUsername,
			Role:    config.RoleAdmin,
			Action:  audit.ActionInterlinkAccepted,
			Target:  req.CallerHostname,
			Result:  audit.ResultOK,
			Details: "InterLink accepted from " + req.CallerURL,
		})

		// Ensure our local user has ZFS access, then push our SSH key back to the caller.
		// Capture the caller's TLS fingerprint (TOFU) and store it for future pinning.
		callerURL, callerSecret, callerLSID := req.CallerURL, req.SharedSecret, localID
		go func() {
			// TOFU: capture and pin the caller's certificate on first outbound contact.
			callerFP := system.CaptureTLSFingerprint(callerURL)
			if callerFP != "" {
				for i := range appCfg.InterLink {
					if appCfg.InterLink[i].ID == callerLSID {
						appCfg.InterLink[i].TLSFingerprint = callerFP
						config.SaveAppConfig(appCfg) //nolint:errcheck
						break
					}
				}
			}
			if err := system.GrantLocalZFSAccess(); err != nil {
				log.Printf("interlink accept-link: zfs allow failed: %v", err)
			}
			pubKey, err := system.EnsureSSHKey()
			if err != nil {
				log.Printf("interlink accept-link: SSH key setup failed: %v", err)
				return
			}
			if err := system.SendPushSSHKey(callerURL, callerSecret, pubKey, callerFP); err != nil {
				log.Printf("interlink accept-link: push SSH key to %s failed: %v", callerURL, err)
			}
		}()

		jsonOK(w, system.AcceptLinkResponse{RemoteID: localID, Hostname: hostname})
	}
}

// HandleInterlinkCheckUser handles POST /api/interlink/check-user — HMAC-authenticated.
// Returns whether the requested username exists as a non-SMB-only user locally.
func HandleInterlinkCheckUser(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.CheckUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Reject stale requests (±30 s window).
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}

		// Find the LinkedServer whose shared secret validates the HMAC.
		var matched bool
		for _, ls := range appCfg.InterLink {
			expected := system.CheckUserHMAC(ls.SharedSecret, req.Username, req.Timestamp, req.Nonce)
			if hmac.Equal([]byte(expected), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}

		users, _ := config.LoadUsers()
		user := config.FindUserByUsername(users, req.Username)
		exists := user != nil && user.Role != config.RoleSMBOnly

		jsonOK(w, system.CheckUserResponse{Exists: exists})
	}
}

// HandleInterlinkLogin handles GET /interlink-login — no session auth.
// Validates SSO token and creates a session for the user.
func HandleInterlinkLogin(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		serverID := r.URL.Query().Get("server_id")

		// Find the link by our local ID.
		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == serverID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			http.Redirect(w, r, "/login?reason=link_unknown", http.StatusSeeOther)
			return
		}

		username, err := interlink.ValidateToken(ls.SharedSecret, token)
		if err != nil {
			http.Redirect(w, r, "/login?reason=link_expired", http.StatusSeeOther)
			return
		}

		users, _ := config.LoadUsers()
		user := config.FindUserByUsername(users, username)
		if user == nil || user.Role == config.RoleSMBOnly {
			// User doesn't exist locally — show normal login.
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		sess, err := session.Default.Create(user.ID, user.Username, user.Role)
		if err != nil {
			http.Redirect(w, r, "/login?reason=session_error", http.StatusSeeOther)
			return
		}

		audit.Log(audit.Entry{
			User:    user.Username,
			Role:    user.Role,
			Action:  audit.ActionLogin,
			Result:  audit.ResultOK,
			Details: "InterLink switch from " + ls.Hostname,
		})

		SetSessionCookie(w, sess.Token)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// ─── outbound endpoints (called by local portal UI) ──────────────────────────

// HandleInterlinkListFast handles GET /api/interlink/servers/fast
// Returns server config immediately with last-cached online/lxd_enabled status (no live ping).
func HandleInterlinkListFast(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type serverOut struct {
			ID         string `json:"id"`
			URL        string `json:"url"`
			Hostname   string `json:"hostname"`
			Online     bool   `json:"online"`
			LXDEnabled bool   `json:"lxd_enabled"`
		}
		out := make([]serverOut, 0, len(appCfg.InterLink))
		for _, ls := range appCfg.InterLink {
			s, _ := cachedStatus(ls.ID)
			out = append(out, serverOut{
				ID:         ls.ID,
				URL:        ls.URL,
				Hostname:   ls.Hostname,
				Online:     s.Online,
				LXDEnabled: s.LXDEnabled,
			})
		}
		jsonOK(w, out)
	}
}

// HandleInterlinkList handles GET /api/interlink/servers
// Returns all linked servers with cached online status and ZFS access check.
func HandleInterlinkList(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type serverOut struct {
			ID            string    `json:"id"`
			URL           string    `json:"url"`
			Hostname      string    `json:"hostname"`
			LinkedBy      string    `json:"linked_by"`
			LinkedAt      time.Time `json:"linked_at"`
			Online        bool      `json:"online"`
			RemoteVersion string    `json:"remote_version,omitempty"`
			ZFSAccess     bool      `json:"zfs_access"`
			LXDEnabled    bool      `json:"lxd_enabled"`
		}

		type result struct {
			id         string
			online     bool
			ver        string
			zfsAccess  bool
			lxdEnabled bool
		}
		ch := make(chan result, len(appCfg.InterLink))
		for _, ls := range appCfg.InterLink {
			ls := ls
			go func() {
				if s, ok := cachedStatus(ls.ID); ok {
					zfsAccess, _ := system.GetRemoteZFSAccess(ls.URL, ls.SharedSecret, ls.TLSFingerprint)
					ch <- result{ls.ID, s.Online, s.RemoteVersion, zfsAccess, s.LXDEnabled}
					return
				}
				h, v, err := system.PingServer(ls.URL, ls.TLSFingerprint)
				online := err == nil && h != ""
				lxdEnabled := system.RemotePingHasLXD(ls.URL, ls.TLSFingerprint)
				setStatus(ls.ID, serverStatus{Online: online, RemoteVersion: v, LXDEnabled: lxdEnabled, FetchedAt: time.Now()})
				zfsAccess, _ := system.GetRemoteZFSAccess(ls.URL, ls.SharedSecret, ls.TLSFingerprint)
				ch <- result{ls.ID, online, v, zfsAccess, lxdEnabled}
			}()
		}

		statusMap := make(map[string]result, len(appCfg.InterLink))
		for range appCfg.InterLink {
			res := <-ch
			statusMap[res.id] = res
		}

		out := make([]serverOut, 0, len(appCfg.InterLink))
		for _, ls := range appCfg.InterLink {
			s := statusMap[ls.ID]
			out = append(out, serverOut{
				ID:            ls.ID,
				URL:           ls.URL,
				Hostname:      ls.Hostname,
				LinkedBy:      ls.LinkedBy,
				LinkedAt:      ls.LinkedAt,
				Online:        s.online,
				RemoteVersion: s.ver,
				ZFSAccess:     s.zfsAccess,
				LXDEnabled:    s.lxdEnabled,
			})
		}
		jsonOK(w, out)
	}
}

// HandleInterlinkLink handles POST /api/interlink/link (admin only)
func HandleInterlinkLink(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)

		var req struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
			TOTP     string `json:"totp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.URL == "" || req.Username == "" || req.Password == "" {
			jsonErr(w, http.StatusBadRequest, "url, username, and password are required")
			return
		}

		// Deduplicate.
		for _, ls := range appCfg.InterLink {
			if ls.URL == req.URL {
				jsonErr(w, http.StatusConflict, "server already linked")
				return
			}
		}

		// Verify reachability, get hostname, and capture TLS fingerprint (TOFU pin).
		remoteHostname, _, err := system.PingServer(req.URL, "")
		if err != nil {
			jsonErr(w, http.StatusServiceUnavailable, "cannot reach remote server: "+err.Error())
			return
		}
		remoteFP := system.CaptureTLSFingerprint(req.URL)

		localHostname, _ := os.Hostname()
		if localHostname == "" {
			localHostname = "localhost"
		}

		localID := newID()
		secret := system.GenerateSharedSecret()

		resp, err := system.SendAcceptLink(req.URL, remoteFP, system.AcceptLinkRequest{
			CallerURL:      localURL(r, appCfg),
			CallerHostname: localHostname,
			CallerID:       localID,
			SharedSecret:   secret,
			AdminUsername:  req.Username,
			AdminPassword:  req.Password,
			AdminTOTP:      req.TOTP,
		})
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if resp.TOTPNeeded {
			jsonOK(w, map[string]bool{"totp_needed": true})
			return
		}

		ls := config.LinkedServer{
			ID:             localID,
			URL:            req.URL,
			Hostname:       remoteHostname,
			SharedSecret:   secret,
			RemoteID:       resp.RemoteID,
			LinkedBy:       sess.Username,
			LinkedAt:       time.Now(),
			TLSFingerprint: remoteFP,
		}
		appCfg.InterLink = append(appCfg.InterLink, ls)
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}
		setStatus(ls.ID, serverStatus{Online: true, RemoteVersion: "", FetchedAt: time.Now()})

		// Ensure our local user has ZFS access, then push our SSH key to the remote.
		// The remote's accept-link handler will reciprocate and push its key back to us.
		if err := system.GrantLocalZFSAccess(); err != nil {
			log.Printf("interlink link: zfs allow failed: %v", err)
		}
		if pubKey, err := system.EnsureSSHKey(); err == nil {
			if err := system.SendPushSSHKey(ls.URL, ls.SharedSecret, pubKey, ls.TLSFingerprint); err != nil {
				log.Printf("interlink link: push SSH key to %s failed: %v", ls.URL, err)
			}
		} else {
			log.Printf("interlink link: SSH key setup failed: %v", err)
		}

		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionInterlinkLinked,
			Target:  remoteHostname,
			Result:  audit.ResultOK,
			Details: "linked " + req.URL,
		})

		jsonOK(w, map[string]interface{}{
			"ok":            true,
			"linked_server": ls,
		})
	}
}

// HandleInterlinkUnlink handles DELETE /api/interlink/{id} (admin only).
// After removing locally it also notifies the remote server to remove us (best-effort).
func HandleInterlinkUnlink(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)
		id := mux.Vars(r)["id"]

		var removed *config.LinkedServer
		kept := make([]config.LinkedServer, 0, len(appCfg.InterLink))
		for _, ls := range appCfg.InterLink {
			if ls.ID == id {
				lsCopy := ls
				removed = &lsCopy
			} else {
				kept = append(kept, ls)
			}
		}
		if removed == nil {
			jsonErr(w, http.StatusNotFound, "linked server not found")
			return
		}
		appCfg.InterLink = kept
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}

		statusCacheMu.Lock()
		delete(statusCache, id)
		statusCacheMu.Unlock()

		// Best-effort: tell the remote to remove us from its list too.
		if removed.RemoteID != "" {
			go system.SendRemoteUnlink(removed.URL, removed.SharedSecret, removed.RemoteID, removed.TLSFingerprint) //nolint:errcheck
		}

		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionInterlinkUnlinked,
			Target:  removed.Hostname,
			Result:  audit.ResultOK,
			Details: "unlinked " + removed.URL,
		})

		jsonOK(w, map[string]string{"message": "unlinked"})
	}
}

// HandleInterlinkRemotePools handles POST /api/interlink/remote-pools — no session auth.
// HMAC-authenticated by a peer; returns this server's pool names and process user.
func HandleInterlinkRemotePools(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.RemotePoolsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}
		var matched bool
		for _, ls := range appCfg.InterLink {
			if hmac.Equal([]byte(system.RemotePoolsHMAC(ls.SharedSecret, req.Timestamp, req.Nonce)), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}
		pools, err := system.GetAllPools()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "cannot get pools: "+err.Error())
			return
		}
		rPools := make([]system.RemotePool, 0, len(pools))
		for _, p := range pools {
			rPools = append(rPools, system.RemotePool{
				Name:      p.Name,
				Available: int64(p.UsableAvail),
			})
		}
		jsonOK(w, system.RemotePoolsResponse{
			Pools:       rPools,
			ProcessUser: system.GetProcessUser(),
		})
	}
}

// HandleInterlinkPushSSHKey handles POST /api/interlink/push-ssh-key — no session auth.
// HMAC-authenticated by a peer; adds the caller's SSH public key to authorized_keys
// and grants the caller's process user ZFS interlink permissions on all local pools.
func HandleInterlinkPushSSHKey(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.PushSSHKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}
		var matched bool
		for _, ls := range appCfg.InterLink {
			if hmac.Equal([]byte(system.PushSSHKeyHMAC(ls.SharedSecret, req.PublicKey, req.ProcessUser, req.Timestamp, req.Nonce)), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}
		if err := system.AddSSHAuthorizedKey(req.PublicKey); err != nil {
			jsonErr(w, http.StatusInternalServerError, "cannot add SSH key: "+err.Error())
			return
		}
		// Grant ZFS permissions to our own process user so that when the remote
		// SSHes in as us and runs "sudo zfs recv", the delegated permissions are set.
		if err := system.GrantLocalZFSAccess(); err != nil {
			log.Printf("interlink push-ssh-key: zfs allow failed: %v", err)
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleInterlinkGrantZFSAccess handles POST /api/interlink/grant-zfs-access — no session auth.
// HMAC-authenticated by a peer; runs zfs allow on all local pools so newly-created pools
// are covered even if they didn't exist when the InterLink was first established.
func HandleInterlinkGrantZFSAccess(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.CheckZFSAccessRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}
		var matched bool
		for _, ls := range appCfg.InterLink {
			if hmac.Equal([]byte(system.GrantZFSAccessHMAC(ls.SharedSecret, req.Timestamp, req.Nonce)), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}
		if err := system.GrantLocalZFSAccess(); err != nil {
			jsonErr(w, http.StatusInternalServerError, "zfs allow failed: "+err.Error())
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleInterlinkCheckZFSAccess handles POST /api/interlink/check-zfs-access — no session auth.
// HMAC-authenticated by a peer; reports whether the given process user has the required
// ZFS permissions on at least one local pool.
func HandleInterlinkCheckZFSAccess(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.CheckZFSAccessRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}
		var matched bool
		for _, ls := range appCfg.InterLink {
			if hmac.Equal([]byte(system.CheckZFSAccessHMAC(ls.SharedSecret, req.Timestamp, req.Nonce)), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}
		jsonOK(w, map[string]bool{"has_access": system.CheckLocalZFSAccess()})
	}
}

// HandleInterlinkRemoteUnlink handles POST /api/interlink/remote-unlink — no session auth.
// Called by a peer when it unlinks us; removes the matching entry from our list.
func HandleInterlinkRemoteUnlink(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.RemoteUnlinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Reject stale requests (±30 s window).
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}

		// Find the LinkedServer whose ID matches and whose secret validates the HMAC.
		var matched *config.LinkedServer
		for i := range appCfg.InterLink {
			ls := &appCfg.InterLink[i]
			if ls.ID != req.RemoteID {
				continue
			}
			expected := system.RemoteUnlinkHMAC(ls.SharedSecret, req.RemoteID, req.Timestamp, req.Nonce)
			if hmac.Equal([]byte(expected), []byte(req.HMAC)) {
				matched = ls
				break
			}
		}
		if matched == nil {
			// Return 200 anyway — idempotent, don't leak info.
			jsonOK(w, map[string]string{"message": "ok"})
			return
		}

		kept := make([]config.LinkedServer, 0, len(appCfg.InterLink)-1)
		for _, ls := range appCfg.InterLink {
			if ls.ID != matched.ID {
				kept = append(kept, ls)
			}
		}
		appCfg.InterLink = kept
		config.SaveAppConfig(appCfg) //nolint:errcheck

		statusCacheMu.Lock()
		delete(statusCache, matched.ID)
		statusCacheMu.Unlock()

		audit.Log(audit.Entry{
			User:    "system",
			Role:    config.RoleAdmin,
			Action:  audit.ActionInterlinkUnlinked,
			Target:  matched.Hostname,
			Result:  audit.ResultOK,
			Details: "remote-initiated unlink from " + matched.URL,
		})

		jsonOK(w, map[string]string{"message": "ok"})
	}
}

// HandleInterlinkSwitch handles POST /api/interlink/switch
// Accepts an optional "mode" field: "direct" (default, existing redirect behaviour)
// or "relay" (stay on this portal, proxy API calls to the remote).
func HandleInterlinkSwitch(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)

		var req struct {
			ServerID string `json:"server_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == req.ServerID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			jsonErr(w, http.StatusNotFound, "linked server not found")
			return
		}

		checkResp, err := system.CheckUserOnRemote(ls.URL, ls.SharedSecret, sess.Username, ls.TLSFingerprint)
		if err != nil || !checkResp.Exists {
			jsonOK(w, map[string]interface{}{
				"user_exists":  false,
				"redirect_url": "",
			})
			return
		}

		// Use relay if global relay mode is enabled.
		useRelay := appCfg.InterlinkRelayMode

		if useRelay {
			// Relay mode: store relay state on the session, return relay:true.
			cookie, cookieErr := r.Cookie("zfsnas_session")
			if cookieErr != nil {
				jsonErr(w, http.StatusUnauthorized, "no session cookie")
				return
			}
			session.SetRelay(cookie.Value, &session.RelayState{
				ServerID: ls.ID,
				Hostname: ls.Hostname,
			})
			jsonOK(w, map[string]interface{}{
				"user_exists": true,
				"relay":       true,
				"hostname":    ls.Hostname,
				"relay_url":   ls.URL,
			})
			return
		}

		// Default: direct mode — generate SSO token and return redirect URL.
		token := interlink.GenerateToken(ls.SharedSecret, sess.Username)
		redirectURL := ls.URL + "/interlink-login?token=" + token + "&server_id=" + ls.RemoteID

		jsonOK(w, map[string]interface{}{
			"user_exists":  true,
			"redirect_url": redirectURL,
		})
	}
}

// HandleInterlinkGetRelayMode handles GET /api/interlink/relay-mode
// Returns the global relay mode setting.
func HandleInterlinkGetRelayMode(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]bool{"relay_mode": appCfg.InterlinkRelayMode})
	}
}

// HandleInterlinkSetRelayMode handles PUT /api/interlink/relay-mode
// Saves the global relay mode preference.
func HandleInterlinkSetRelayMode(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RelayMode bool `json:"relay_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		appCfg.InterlinkRelayMode = req.RelayMode
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleInterlinkRelayExit handles POST /api/interlink/relay-exit
// Clears the relay state for the current session (exits relay mode).
func HandleInterlinkRelayExit(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("zfsnas_session")
	if err == nil {
		session.ClearRelay(cookie.Value)
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// localURL returns this server's externally-visible base URL for use in link handshakes.
func localURL(r *http.Request, appCfg *config.AppConfig) string {
	if r.Host != "" {
		return "https://" + r.Host
	}
	h, _ := os.Hostname()
	if h == "" {
		h = "localhost"
	}
	return fmt.Sprintf("https://%s:%d", h, appCfg.Port)
}
