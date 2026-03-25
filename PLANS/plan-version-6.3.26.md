# Plan — Version 6.3.26

## Overview

One feature: **InterLink** — a multi-server federation system that lets users switch between connected ZNAS instances directly from the top bar, without re-authenticating when their account exists on the destination.

---

## Feature — InterLink

### Concept

Two or more ZNAS servers can be "linked" together. Once linked, a dropdown on the hostname button in the top bar lists all connected servers. Clicking one instantly switches the user's browser to that server — **no password or 2FA is ever asked at switch time**. If the user's account exists on the destination, the switch is completely seamless: the destination creates a session automatically via a short-lived HMAC-signed SSO token. If the account doesn't exist on the destination, the destination's normal login page is shown.

2FA is only relevant during the **linking** step: if the admin account used to authorise the link on the remote server has 2FA enabled, the user must provide the TOTP code once at that moment. After that, switching never requires it.

No user passwords are ever stored. Trust between servers is established via a shared 32-byte secret negotiated at link time.

---

## Config additions — `internal/config/config.go`

```go
type LinkedServer struct {
    ID           string    `json:"id"`            // UUID v4
    URL          string    `json:"url"`            // e.g. "https://192.168.2.5:8443"
    Hostname     string    `json:"hostname"`       // fetched from remote during linking
    SharedSecret string    `json:"shared_secret"`  // 32-byte hex; used to sign SSO tokens
    LinkedBy     string    `json:"linked_by"`      // admin username who created the link
    LinkedAt     time.Time `json:"linked_at"`
}
```

Add to `AppConfig`:

```go
InterLink []LinkedServer `json:"inter_link,omitempty"`
```

---

## New package — `internal/interlink/`

Single file `internal/interlink/token.go`:

```go
// GenerateToken creates a 30-second one-time SSO token.
// Payload: "<nonce>|<username>|<unix_timestamp>"
// HMAC-SHA256 is computed over the payload using sharedSecret.
// Returns the payload + "." + hex(HMAC).
func GenerateToken(sharedSecret, username string) string

// ValidateToken verifies the HMAC, checks expiry (30 s), and returns the
// embedded username. Returns an error if invalid, expired, or HMAC mismatch.
func ValidateToken(sharedSecret, token string) (username string, err error)
```

---

## `system/interlink.go` — new file

HTTP client helpers used by the handlers to call remote ZNAS servers.
All calls skip TLS certificate verification (self-signed certs are normal here).

```go
// PingServer calls GET <url>/api/interlink/ping and returns the remote hostname + version.
// Timeout: 5 s. Returns error on network failure or non-200 response.
func PingServer(url string) (hostname, version string, err error)

// AcceptLinkRequest is the payload sent to the remote server during linking.
type AcceptLinkRequest struct {
    CallerURL      string `json:"caller_url"`
    CallerHostname string `json:"caller_hostname"`
    CallerID       string `json:"caller_id"`       // UUID this side uses for the link
    SharedSecret   string `json:"shared_secret"`   // 32-byte hex proposed by initiator
    AdminUsername  string `json:"admin_username"`
    AdminPassword  string `json:"admin_password"`
    AdminTOTP      string `json:"admin_totp,omitempty"`
}

// AcceptLinkResponse is the response from the remote server.
type AcceptLinkResponse struct {
    RemoteID   string `json:"remote_id"`   // UUID the remote server uses for this link
    Hostname   string `json:"hostname"`
    TOTPNeeded bool   `json:"totp_needed"` // true when re-submission with TOTP is required
}

// SendAcceptLink calls POST <url>/api/interlink/accept-link with the given payload.
// Returns the remote server's AcceptLinkResponse.
func SendAcceptLink(url string, req AcceptLinkRequest) (*AcceptLinkResponse, error)

// CheckUserRequest is the HMAC-signed payload for /api/interlink/check-user.
type CheckUserRequest struct {
    Username  string `json:"username"`
    Timestamp int64  `json:"timestamp"` // Unix seconds; request must be < 30 s old
    Nonce     string `json:"nonce"`
    HMAC      string `json:"hmac"` // hex(HMAC-SHA256(sharedSecret, "<username>|<timestamp>|<nonce>"))
}

// CheckUserResponse is what the remote server returns.
type CheckUserResponse struct {
    Exists bool `json:"exists"`
}

// CheckUserOnRemote calls POST <url>/api/interlink/check-user using the given shared secret.
func CheckUserOnRemote(url, sharedSecret, username string) (*CheckUserResponse, error)
```

---

## `handlers/interlink.go` — new file

### Inbound endpoints (called by other ZNAS servers)

#### `GET /api/interlink/ping`
No authentication required. Returns:
```json
{"hostname": "mynas", "version": "6.3.26", "ok": true}
```
Used by the status poller and during the linking handshake.

#### `POST /api/interlink/accept-link`
No session auth — the remote admin's credentials are in the payload.

1. Parse `AcceptLinkRequest`.
2. Look up `AdminUsername` in local users; verify `AdminPassword` via `bcrypt.CompareHashAndPassword`.
3. If the user has TOTP enabled and `AdminTOTP` is empty: return `{totp_needed: true}` with HTTP 200.
4. If TOTP is non-empty: validate via `totp.Validate`.
5. If validation passes:
   - Check no existing link with the same `CallerURL` already exists (deduplicate).
   - Generate a local UUID (`RemoteID`) for this link.
   - Append `LinkedServer{ID: RemoteID, URL: CallerURL, Hostname: CallerHostname, SharedSecret: SharedSecret, LinkedBy: AdminUsername, LinkedAt: now()}` to `cfg.InterLink`.
   - Save config.
   - Audit log: "InterLink accepted from `<CallerHostname>` by `<AdminUsername>`".
   - Return `{remote_id: RemoteID, hostname: localHostname}`.

#### `POST /api/interlink/check-user`
No session auth — authenticated via HMAC in payload.

1. Parse `CheckUserRequest`.
2. Find the `LinkedServer` whose shared secret validates the HMAC. Reject if no match or timestamp > 30 s old.
3. Look up `Username` in local users.
4. Return `{exists: true/false}`.

The `requires_totp` field is intentionally absent — 2FA is never checked or required during a switch.

#### `GET /interlink-login`
No session auth. Query params: `token=<token>&server_id=<id>`.

This is the endpoint the browser is redirected to after a switch.

1. Look up the `LinkedServer` by `server_id`.
2. Call `interlink.ValidateToken(server.SharedSecret, token)` → get `username`.
3. If token invalid or expired: redirect to `/` (login page) with flash param `?reason=link_expired`.
4. Look up `username` in local users.
5. If user not found: redirect to `/` (login page, user authenticates normally on this server).
6. Create a session for the user (same as `HandleLogin` does), set `Set-Cookie` header.
7. Redirect to `/`.

No TOTP check is performed at switch time. The server-to-server HMAC trust established at link time is sufficient authority to create the session.

### Outbound endpoints (called by the local portal UI)

#### `GET /api/interlink/servers`
Requires auth. Returns list of linked servers with live online status:

```json
[
  {
    "id": "abc123",
    "url": "https://192.168.2.5:8443",
    "hostname": "remotenas",
    "linked_by": "admin",
    "linked_at": "2026-03-01T10:00:00Z",
    "online": true,
    "remote_version": "6.3.26"
  }
]
```

Online status is determined by calling `system.PingServer(url)` for each server in parallel (5 s timeout). Results cached in memory for 30 s to avoid hammering on every dropdown open.

#### `POST /api/interlink/link`
Requires admin. Body:
```json
{
  "url":      "https://192.168.2.5:8443",
  "username": "admin",
  "password": "secret",
  "totp":     ""
}
```

1. Generate a UUID and a 32-byte random hex secret.
2. First call `system.PingServer(url)` to get the remote hostname and verify connectivity.
3. Call `system.SendAcceptLink(url, AcceptLinkRequest{...})`.
4. If response has `totp_needed: true`: return `{totp_needed: true}` to frontend (HTTP 200).
5. If success: append `LinkedServer` to local config, save.
6. Audit log.
7. Return `{ok: true, linked_server: {...}}`.

Error cases: remote unreachable (503), wrong credentials (401 with message), already linked (409).

#### `DELETE /api/interlink/{id}`
Requires admin.

Removes the `LinkedServer` from `cfg.InterLink` by ID. Does **not** call the remote to unlink it — unlinking is one-sided. Audit log.

#### `POST /api/interlink/switch`
Requires auth. Body:
```json
{
  "server_id": "abc123"
}
```

1. Look up `LinkedServer` by `server_id`.
2. Call `system.CheckUserOnRemote(server.URL, server.SharedSecret, currentUser)` to determine if the user exists.
3. If user doesn't exist on remote: return `{user_exists: false, redirect_url: server.URL}` — frontend redirects to the remote's root (login page).
4. Generate SSO token via `interlink.GenerateToken(server.SharedSecret, currentUser)`.
5. Return:
```json
{
  "user_exists": true,
  "redirect_url": "https://192.168.2.5:8443/interlink-login?token=<token>&server_id=<remoteID>"
}
```

The frontend performs the redirect; no server-side redirect is done. No TOTP is involved — the switch is always silent.

---

## `handlers/router.go`

```go
// Public interlink endpoints (no auth — called by other ZNAS servers or redirect target)
r.Handle("/api/interlink/ping",        http.HandlerFunc(HandleInterlinkPing)).Methods("GET")
r.Handle("/api/interlink/accept-link", http.HandlerFunc(HandleInterlinkAcceptLink)).Methods("POST")
r.Handle("/api/interlink/check-user",  http.HandlerFunc(HandleInterlinkCheckUser)).Methods("POST")
r.Handle("/interlink-login",           http.HandlerFunc(HandleInterlinkLogin)).Methods("GET")

// Authenticated interlink endpoints
r.Handle("/api/interlink/servers", RequireAuth(http.HandlerFunc(HandleInterlinkList))).Methods("GET")
r.Handle("/api/interlink/link",    RequireAuth(RequireAdmin(http.HandlerFunc(HandleInterlinkLink)))).Methods("POST")
r.Handle("/api/interlink/switch",  RequireAuth(http.HandlerFunc(HandleInterlinkSwitch))).Methods("POST")
r.Handle("/api/interlink/{id}",    RequireAuth(RequireAdmin(http.HandlerFunc(HandleInterlinkUnlink)))).Methods("DELETE")
```

---

## Frontend changes — `static/index.html`

### Hostname button → dropdown

The existing top-bar hostname `<button>` is replaced by a dropdown wrapper:

```html
<div class="hostname-dropdown-wrapper" id="hostname-dropdown-wrapper">
  <button class="hostname-btn" id="hostname-btn" onclick="toggleHostnameDropdown(event)">
    <span id="top-hostname">loading...</span>
    <span class="dropdown-chevron">&#9660;</span>
  </button>
  <div id="hostname-dropdown" class="hostname-dropdown" style="display:none">
    <!-- populated by JS -->
  </div>
</div>
```

The chevron (▾) is always visible, even when there are no linked servers, so users discover the feature.

### `loadHostnameDropdown()`

Called once at startup and after any interlink change.

```js
async function loadHostnameDropdown() {
    const servers = await apiFetch('/api/interlink/servers').catch(() => []);
    const menu = document.getElementById('hostname-dropdown');
    const localHostname = document.getElementById('top-hostname').textContent;
    let html = `
        <div class="hdd-current">
          <span class="status-dot online"></span>
          <span class="hdd-hostname">${escapeHtml(localHostname)}</span>
          <span class="hdd-badge">current</span>
        </div>`;
    for (const s of servers) {
        const dot = s.online ? 'online' : 'offline';
        html += `
        <div class="hdd-server" onclick="switchToServer('${s.id}')">
          <span class="status-dot ${dot}"></span>
          <span class="hdd-hostname">${escapeHtml(s.hostname)}</span>
          <span class="hdd-url">${escapeHtml(new URL(s.url).host)}</span>
        </div>`;
    }
    html += `<div class="hdd-divider"></div>
             <div class="hdd-manage" onclick="openInterlinkModal()">Manage InterLink</div>`;
    menu.innerHTML = html;
}
```

Click outside closes the dropdown (document `mousedown` listener with `contains` check).

### `switchToServer(serverId)`

```js
async function switchToServer(serverId) {
    closeHostnameDropdown();
    const res = await apiFetch('/api/interlink/switch', {
        method: 'POST',
        body: JSON.stringify({ server_id: serverId })
    });
    // Always redirect — either to the SSO landing or to the remote login page
    window.location.href = res.redirect_url;
}
```

No TOTP prompt, no password prompt. The switch is always a single click → redirect.

---

### Manage InterLink modal

Large modal, same style as other management modals:

```
┌────────────────────────────────────────────────────────────────────┐
│  InterLink — Connected Servers                               [×]   │
│                                                                    │
│  [+ Add Server]                                                    │
│                                                                    │
│  ┌──────┬──────────────┬──────────────────────┬────────┬────────┐  │
│  │      │ Hostname     │ URL                  │ Added  │        │  │
│  ├──────┼──────────────┼──────────────────────┼────────┼────────┤  │
│  │  ●   │ remotenas    │ 192.168.2.5:8443      │ 3d ago │ Unlink │  │
│  │  ●   │ backupnas    │ 192.168.2.8:8443      │ 14d    │ Unlink │  │
│  │  ○   │ offsite      │ vpn.home.arpa:8443    │ 2mo    │ Unlink │  │
│  └──────┴──────────────┴──────────────────────┴────────┴────────┘  │
│                                                                    │
│  ●  online   ○  offline                                           │
└────────────────────────────────────────────────────────────────────┘
```

- Online dot: green filled circle `●`; offline: grey hollow circle `○`.
- "Unlink" button: small red ghost button; triggers a confirm dialog ("Unlink `<hostname>`? This only removes the connection locally."), then `DELETE /api/interlink/{id}`.
- The "Added" column shows a human-readable relative time (e.g. "3d ago").

#### Add Server sub-popup

Opens as a second modal layered on top of the Manage modal:

```
┌───────────────────────────────────────────────────┐
│  Add InterLink Server                       [×]   │
│                                                   │
│  Server URL                                       │
│  [ https://192.168.2.5:8443 _________________ ]   │
│                                                   │
│  Admin Username                                   │
│  [ admin _____________________________________ ]   │
│                                                   │
│  Admin Password                                   │
│  [ ●●●●●●●● _________________________________ ]   │
│                                                   │
│  [  Connect  ]                      [ Cancel ]    │
└───────────────────────────────────────────────────┘
```

If the remote admin has 2FA, a third modal appears immediately after clicking Connect:

```
┌───────────────────────────────────────────────────┐
│  Remote 2FA Required                        [×]   │
│                                                   │
│  The admin account on the remote server has       │
│  two-factor authentication enabled.               │
│  Enter the 2FA code to complete linking.          │
│                                                   │
│  [ 6-digit code __________________________ ]      │
│                                                   │
│  [  Verify & Link  ]                [ Cancel ]    │
└───────────────────────────────────────────────────┘
```

JS flow:
```js
async function connectInterLink() {
    const url = ..., user = ..., pass = ...;
    const res = await apiFetch('/api/interlink/link', {
        method: 'POST',
        body: JSON.stringify({ url, username: user, password: pass, totp: '' })
    });
    if (res.totp_needed) {
        openLinkTOTPPrompt({ url, username: user, password: pass });
        return;
    }
    if (res.ok) {
        showToast(`Linked to ${res.linked_server.hostname}`, 'success');
        closeAddServerModal();
        loadInterlinkList();
        loadHostnameDropdown();
    }
}

async function verifyAndLink(url, username, password, totp) {
    const res = await apiFetch('/api/interlink/link', {
        method: 'POST',
        body: JSON.stringify({ url, username, password, totp })
    });
    if (res.ok) {
        showToast(`Linked to ${res.linked_server.hostname}`, 'success');
        closeAllInterlinkModals();
        loadInterlinkList();
        loadHostnameDropdown();
    } else {
        showToast('Invalid 2FA code', 'error');
    }
}
```

#### JS functions

- `openInterlinkModal()` — `GET /api/interlink/servers`, render table, show modal
- `closeInterlinkModal()`
- `loadInterlinkList()` — refresh table inside open modal
- `openAddServerModal()`
- `closeAddServerModal()`
- `connectInterLink()` — see above
- `openLinkTOTPPrompt(pendingPayload)` — show 2FA modal during **linking** only, stores pending payload
- `verifyAndLink(url, username, password, totp)` — see above
- `unlinkServer(id, hostname)` — confirm + DELETE + refresh
- `toggleHostnameDropdown(event)` — show/hide dropdown
- `closeHostnameDropdown()`

---

## CSS additions — `static/style.css`

```css
/* Hostname dropdown wrapper */
.hostname-dropdown-wrapper { position: relative; display: inline-block; }

.hostname-btn {
    display: flex; align-items: center; gap: 6px;
    background: transparent; border: 1px solid var(--border);
    border-radius: 6px; padding: 4px 10px; cursor: pointer;
    color: var(--text); font-size: 0.875rem;
}
.hostname-btn:hover { background: var(--hover-bg); }

.dropdown-chevron { font-size: 0.65rem; opacity: 0.6; }

.hostname-dropdown {
    position: absolute; top: calc(100% + 4px); left: 0;
    min-width: 240px; background: var(--card-bg);
    border: 1px solid var(--border); border-radius: 8px;
    box-shadow: 0 6px 20px rgba(0,0,0,0.35);
    z-index: 1000; overflow: hidden;
}

.hdd-current {
    display: flex; align-items: center; gap: 8px;
    padding: 10px 14px; background: var(--hover-bg);
    font-weight: 600; font-size: 0.875rem;
}
.hdd-badge {
    margin-left: auto; font-size: 0.7rem; font-weight: 400;
    color: var(--text-muted); background: var(--border);
    border-radius: 4px; padding: 1px 6px;
}

.hdd-server {
    display: flex; align-items: center; gap: 8px;
    padding: 9px 14px; cursor: pointer; font-size: 0.875rem;
}
.hdd-server:hover { background: var(--hover-bg); }

.hdd-hostname { flex: 1; }
.hdd-url { font-size: 0.75rem; color: var(--text-muted); }

.hdd-divider { border-top: 1px solid var(--border); margin: 4px 0; }

.hdd-manage {
    padding: 9px 14px; cursor: pointer;
    font-size: 0.85rem; color: var(--accent);
    font-weight: 500;
}
.hdd-manage:hover { background: var(--hover-bg); }

/* Status dots */
.status-dot {
    width: 8px; height: 8px; border-radius: 50%;
    flex-shrink: 0; display: inline-block;
}
.status-dot.online  { background: #22c55e; }
.status-dot.offline { background: #6b7280; }

/* InterLink modal table */
.interlink-table { width: 100%; border-collapse: collapse; margin-top: 12px; }
.interlink-table th { text-align: left; padding: 8px 10px; font-size: 0.8rem;
    color: var(--text-muted); border-bottom: 1px solid var(--border); }
.interlink-table td { padding: 10px 10px; font-size: 0.875rem;
    border-bottom: 1px solid var(--border-subtle); vertical-align: middle; }
.interlink-table tr:last-child td { border-bottom: none; }
.interlink-table .col-status { width: 32px; }
.interlink-table .col-actions { width: 80px; text-align: right; }
```

---

## Security notes

- **No password storage**: shared secrets are random, negotiated only once at link time. No credentials are retained after linking.
- **2FA is a link-time concern only**: if the remote admin has TOTP enabled, it is verified once when creating the link. Switching never asks for 2FA. The HMAC trust established at link time is the authorisation mechanism.
- **SSO tokens expire in 30 seconds**: a captured redirect URL is useless after expiry.
- **HMAC prevents forgery**: only servers sharing the secret can generate valid tokens. A third party cannot manufacture a valid token without the shared secret.
- **TLS verification skipped for inter-server calls**: expected on a LAN with self-signed certs. A future release could allow CA bundle configuration.
- **Unlink is one-sided**: removes trust locally. The remote retains its config entry until its admin also unlinks. This is by design — it avoids a need for synchronized state.
- **`accept-link` uses bcrypt verification** of the provided admin password — the password is never stored.
- **`check-user` and `accept-link`** are protected by credential check or HMAC respectively; no additional rate limiting needed beyond what the HTTP server already applies.

---

## Files changed

| File | Change |
|---|---|
| `internal/interlink/token.go` | New — HMAC SSO token generation and validation |
| `system/interlink.go` | New — HTTP client helpers: ping, accept-link, check-user |
| `handlers/interlink.go` | New — all InterLink handlers (8 endpoints) |
| `handlers/router.go` | Register 8 new routes |
| `internal/config/config.go` | Add `LinkedServer`, `AppConfig.InterLink []LinkedServer` |
| `static/index.html` | Hostname button → dropdown; `loadHostnameDropdown()`; `switchToServer()`; TOTP prompt; Manage InterLink modal; Add Server sub-popup; all JS functions |
| `static/style.css` | Dropdown styles; hostname button; status dots; interlink table |

---

## Version bump

`internal/version/version.go`: `"6.3.26"`

## Status: PLANNED
