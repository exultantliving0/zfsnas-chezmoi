# Plan — Version 6.4.17

## Overview

**InterLink Relay Mode** — When switching to a linked server, the user's browser stays on the
original ZNAS server instead of being redirected to the remote. Server A acts as a transparent
authenticated proxy: all portal API calls are forwarded to Server B over the existing InterLink
channel (HMAC-signed, TLS-pinned). Server B does not need to be directly reachable from the
user's browser.

The existing **Direct** mode (browser redirect) is preserved as an alternative option.

---

## Architecture

```
Browser ──HTTPS──► Server A  (relay session active)
                       │
                       │  X-Interlink-Relay-User: alice
                       │  X-Interlink-Relay-TS / Nonce / HMAC
                       │  (TLS-pinned, shared-secret HMAC)
                       ▼
                   Server B  ← validates HMAC, maps local user, executes handler
```

- Browser URL never changes — it stays on Server A.
- Server B never receives the browser's session cookie; identity is carried by HMAC headers only.
- Role used on Server B is the **Server B role** of the user, not the Server A role.
- WebSocket connections (terminal, live metrics) are not proxied in this version — a guard shows a warning instead of silently hanging.

---

## Feature 1 — Relay session state (Server A side)

### New file: `internal/session/relay.go`

In-memory map keyed by session token. Does not persist across restarts — relay mode is
per-browser-session by design.

```go
package session

import "sync"

// RelayState holds which remote server this session is relaying to.
type RelayState struct {
    ServerID string // config.LinkedServer.ID
    Hostname string // human-readable label for the UI banner
}

var (
    relayMu    sync.RWMutex
    relayStore = map[string]*RelayState{}
)

func SetRelay(token string, state *RelayState) {
    relayMu.Lock(); defer relayMu.Unlock()
    relayStore[token] = state
}

func GetRelay(token string) *RelayState {
    relayMu.RLock(); defer relayMu.RUnlock()
    return relayStore[token]
}

func ClearRelay(token string) {
    relayMu.Lock(); defer relayMu.Unlock()
    delete(relayStore, token)
}
```

---

## Feature 2 — HMAC helper for relay forwarding

### Addition to `system/interlink.go`

```go
// RelayForwardHMAC signs an outbound relay request.
// Prefix "relay|" is distinct from all existing interlink HMAC prefixes.
func RelayForwardHMAC(sharedSecret, username string, timestamp int64, nonce string) string {
    key, _ := hex.DecodeString(sharedSecret)
    if len(key) == 0 {
        key = []byte(sharedSecret)
    }
    h := hmac.New(sha256.New, key)
    h.Write([]byte("relay|" + username + "|" + strconv.FormatInt(timestamp, 10) + "|" + nonce))
    return hex.EncodeToString(h.Sum(nil))
}
```

---

## Feature 3 — Relay proxy middleware (Server A)

### New file: `handlers/relay.go` — proxy side

`RelayMiddleware(appCfg *config.AppConfig, next http.Handler) http.Handler`

**Logic per request:**

1. Extract session token from cookie. If none → pass through to `next`.
2. Call `session.GetRelay(token)` — if nil → pass through.
3. Look up `LinkedServer` by `relay.ServerID` in `appCfg.InterLink` — if not found, call
   `session.ClearRelay(token)` and pass through.
4. **Bypass list** — never relay these paths (pass straight to `next`):
   - `/api/interlink/` (any sub-path)
   - `/api/session`
   - `/api/interlink/relay-exit`
   - `/logout`
   - `/ws/` (WebSocket — handled separately with a guard in the UI)
   - `/interlink-login`
   - Static files (paths without `/api/` prefix that are not page navigations)
5. Build relay identity headers:
   - `X-Interlink-Relay-User: <username>`
   - `X-Interlink-Relay-TS: <unix timestamp>`
   - `X-Interlink-Relay-Nonce: <8-byte hex random>`
   - `X-Interlink-Relay-HMAC: RelayForwardHMAC(...)`
6. Clone the incoming request, **strip the Cookie header** (local session must not reach Server B),
   set the four relay headers, set target URL to `ls.URL + r.URL.RequestURI`.
7. Execute via `interlinkClientFor(ls.TLSFingerprint)`.
8. Copy response status, headers (excluding `Set-Cookie`), and body back to `w`.

---

## Feature 4 — Relay auth middleware (Server B)

### Addition to `handlers/relay.go` — auth side

`RelayAuthMiddleware(appCfg *config.AppConfig, next http.Handler) http.Handler`

**Logic per request:**

1. Check for `X-Interlink-Relay-User` header — if absent, pass through (normal cookie auth).
2. Parse `X-Interlink-Relay-TS`, `X-Interlink-Relay-Nonce`, `X-Interlink-Relay-HMAC`.
3. Reject stale requests (±30 s window, same as all other interlink endpoints).
4. Validate HMAC against **all** `appCfg.InterLink` shared secrets using constant-time compare.
   Reject if none match.
5. **Anti-chain guard** — if the request already arrived via relay (i.e. Server B is itself
   proxying), reject with 403. Prevents A → B → C relay chains.
6. Look up `username` in local users; reject if not found or role is `smb-only`.
7. Build a synthetic `session.Session{Username, Role, ...}` and inject into request context
   via a private context key (`relaySessionKey`).
8. Pass to `next`.

### Modify `handlers/helpers.go` — `RequireAuth` / `MustSession`

Before checking the cookie-based session store, check `r.Context().Value(relaySessionKey)`.
If non-nil, use it as the active session. No other handler changes needed.

```go
type contextKey int
const relaySessionKey contextKey = iota
```

---

## Feature 5 — API changes

### `POST /api/interlink/switch` — add `mode` field

```json
{ "server_id": "abc123", "mode": "relay" }
```

| `mode` value | Behaviour |
|---|---|
| `"direct"` (default, existing) | Unchanged — check-user → SSO token → `redirect_url` |
| `"relay"` | check-user → `session.SetRelay(token, state)` → `{"relay":true,"hostname":"nas2"}` |

If `mode` is absent it defaults to `"direct"` for backwards compatibility.

### New: `POST /api/interlink/relay-exit` (auth required)

Calls `session.ClearRelay(sess.Token)` and returns `{"ok": true}`.

Register in `handlers/router.go`.

### `GET /api/session` — extend response

Add relay fields to the existing session info payload so the banner survives a page refresh:

```json
{
  "username": "alice",
  "role": "admin",
  "relay_active": true,
  "relay_hostname": "nas2.local"
}
```

---

## Feature 6 — Frontend (`static/index.html`)

### Switch flow — mode selector

The existing switch confirmation (3-dot → "Switch to server") gains two radio options:

```
Switch mode
  ● Relay   — Stay on this portal; manage the remote through this server
  ○ Direct  — Open the remote portal in your browser (requires direct access)
```

Default: **Relay**.

On confirm, POST to `/api/interlink/switch` with `mode: "relay"` or `"direct"`.
- `"direct"` response with `redirect_url` → existing redirect behavior unchanged.
- `"relay"` response with `relay:true` → store `relayActive = true`, `relayHostname`, show banner, reload nav.

### Relay banner

Persistent strip shown below the top bar when relay is active. Hidden otherwise.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  🔗  Relay mode — Managing: nas2.local        [Return to local server]      │
└─────────────────────────────────────────────────────────────────────────────┘
```

- Background: `rgba(10,132,255,0.10)`, border-bottom: `1px solid rgba(10,132,255,0.25)`.
- "Return to local server" POSTs to `/api/interlink/relay-exit`, then does `location.reload()`.

HTML (placed after the top bar `<div>`):

```html
<div id="relay-banner" style="display:none;padding:8px 20px;background:rgba(10,132,255,0.10);
  border-bottom:1px solid rgba(10,132,255,0.25);display:flex;align-items:center;gap:10px;">
  <span style="font-size:13px;">🔗</span>
  <span style="font-size:13px;color:var(--text-2);flex:1;">
    Relay mode — Managing: <strong id="relay-banner-hostname"></strong>
  </span>
  <button class="btn btn-ghost btn-sm" onclick="exitRelayMode()" style="font-size:12px;">
    Return to local server
  </button>
</div>
```

JS:

```js
let relayActive = false;
let relayHostname = '';

function _applyRelayState(active, hostname) {
  relayActive  = active;
  relayHostname = hostname || '';
  const banner = document.getElementById('relay-banner');
  if (active) {
    document.getElementById('relay-banner-hostname').textContent = hostname;
    banner.style.display = 'flex';
  } else {
    banner.style.display = 'none';
  }
}

async function exitRelayMode() {
  await fetch('/api/interlink/relay-exit', {method:'POST'});
  location.reload();
}
```

### Page-load relay state restore

In the `loadSession()` / boot sequence (where `GET /api/session` is already called), read
`data.relay_active` and `data.relay_hostname` and call `_applyRelayState(...)`.
This ensures the banner reappears after a page refresh while relay is active.

### Sidebar subtitle

When `relayActive` is true, append a small subtitle line under the ZNAS logo:

```html
<div id="relay-sidebar-label" style="display:none;font-size:10px;color:var(--accent-2);
  text-align:center;margin-top:-6px;margin-bottom:4px;letter-spacing:.5px;">
  ↔ <span id="relay-sidebar-hostname"></span>
</div>
```

Show/hide alongside the banner.

### WebSocket guard

Before opening any WebSocket connection (terminal, live disk I/O, update apply), check:

```js
if (relayActive) {
  showModal('ws-relay-warn');
  return;
}
```

Modal `#modal-ws-relay-warn`:

```
WebSocket connections are not available in Relay mode.
To use the terminal or live metrics, open the remote portal directly.
```

Disable the terminal launch button and the "Apply Update" button when `relayActive` is true,
with a tooltip explaining why.

---

## Security notes

| Concern | Mitigation |
|---|---|
| HMAC reuse across endpoints | `"relay|"` prefix is unique — distinct from `check-user`, `remote-pools`, `push-ssh-key`, `grant-zfs-access`, `unlink` |
| Replay attacks | ±30 s timestamp window + random nonce (same pattern as all other interlink endpoints) |
| Cookie leakage to remote | Server A strips `Cookie` header before forwarding |
| Role escalation | Server B resolves role from its own local users table — ignores any role claim from Server A |
| Relay chaining (A→B→C) | Server B's `RelayAuthMiddleware` returns 403 if the request already carries relay headers from a previous hop |
| Relay bypass list | `/api/interlink/*` never relayed — prevents loops and keeps local interlink management always reachable |

---

## File change summary

| File | Change |
|---|---|
| `internal/session/relay.go` | **New** — `RelayState`, `SetRelay`, `GetRelay`, `ClearRelay` |
| `system/interlink.go` | Add `RelayForwardHMAC` |
| `handlers/relay.go` | **New** — `RelayMiddleware` (Server A proxy), `RelayAuthMiddleware` (Server B auth), `relaySessionKey` context key |
| `handlers/helpers.go` | Modify `RequireAuth` / `MustSession` to check relay context before cookie session |
| `handlers/interlink.go` | Modify `HandleInterlinkSwitch` to accept `mode` field; add `HandleInterlinkRelayExit` |
| `handlers/router.go` | Register `POST /api/interlink/relay-exit`; wrap router with `RelayMiddleware` (outbound) and `RelayAuthMiddleware` (inbound) |
| `static/index.html` | Mode selector in switch flow; `#relay-banner` HTML + `_applyRelayState` / `exitRelayMode` JS; page-load restore from `GET /api/session`; sidebar subtitle; WebSocket guard modal + button disabling |

---

## Implementation order

1. `internal/session/relay.go` — relay state map
2. `system/interlink.go` — `RelayForwardHMAC`
3. `handlers/relay.go` — `RelayAuthMiddleware` (Server B auth side) + context key
4. `handlers/helpers.go` — patch `RequireAuth` / `MustSession` for relay context
5. `handlers/relay.go` — `RelayMiddleware` (Server A proxy side)
6. `handlers/interlink.go` — patch `HandleInterlinkSwitch` + add `HandleInterlinkRelayExit`
7. `handlers/router.go` — register route + apply both middlewares
8. Update `GET /api/session` handler to include relay fields
9. `static/index.html` — mode selector, relay banner, page-load restore, sidebar label, WebSocket guard
10. Build + deploy + smoke-test both direct and relay switch paths
