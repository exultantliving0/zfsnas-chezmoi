# Plan — Version 6.5.0

## Overview

**Standard User Role** — a new role between `admin` and `read-only`.  When creating or
editing a Standard user an admin selects which of 11 capability flags to enable.  The portal
enforces those flags on every API route (server-side) **and** hides the matching UI controls
for users who lack a given flag.

---

## New role constant

```go
// internal/config/config.go
const RoleStandard = "standard"
```

Existing roles (`admin`, `read-only`, `smb-only`) are unchanged in behaviour.

---

## Capability flags

| Flag (json key) | What it unlocks |
|---|---|
| `terminal` | Web terminal (`/ws/terminal`) |
| `review_sudoers` | Read-only access to Sudoers status + diff (apply/enable remain admin-only) |
| `browse_files` | File Browser list + users-groups (chown/chmod remain admin-only) |
| `manage_pool_dataset` | Pool and dataset mutation routes |
| `manage_smb` | SMB share CRUD + SMB service control + global SMB config |
| `manage_nfs` | NFS share CRUD + NFS service control |
| `manage_iscsi` | iSCSI config, host, share CRUD + service control |
| `manage_protection` | Snapshot schedule CRUD, scrub schedule, replication task CRUD, run-now |
| `manage_snapshots` | Create, delete, and restore snapshots |
| `edit_settings` | `PUT /api/settings`, `PUT /api/settings/timezone` |
| `manage_interlink` | Link/unlink InterLink servers, push-interlink jobs |

---

## Data model — `internal/config/config.go`

### New struct

```go
// StandardPermissions holds granular capability flags for a "standard" role user.
// All fields are omitempty so the struct serialises to {} when all are false.
type StandardPermissions struct {
    Terminal          bool `json:"terminal,omitempty"`
    ReviewSudoers     bool `json:"review_sudoers,omitempty"`
    BrowseFiles       bool `json:"browse_files,omitempty"`
    ManagePoolDataset bool `json:"manage_pool_dataset,omitempty"`
    ManageSMB         bool `json:"manage_smb,omitempty"`
    ManageNFS         bool `json:"manage_nfs,omitempty"`
    ManageISCSI       bool `json:"manage_iscsi,omitempty"`
    ManageProtection  bool `json:"manage_protection,omitempty"`
    ManageSnapshots   bool `json:"manage_snapshots,omitempty"`
    EditSettings      bool `json:"edit_settings,omitempty"`
    ManageInterlink   bool `json:"manage_interlink,omitempty"`
}
```

### Additions to `User`

```go
StandardPerms *StandardPermissions `json:"standard_perms,omitempty"`
```

`StandardPerms` is `nil` for admin / read-only / smb-only users.  It is initialised to
`&StandardPermissions{}` (all false) when a standard user is created, and is set to `nil`
again if the role is changed away from standard.

---

## Middleware — `handlers/middleware.go`

### New `RequirePermission` wrapper

```go
// RequirePermission passes if:
//   - the session user is admin, OR
//   - the session user is "standard" and their StandardPerms[perm] == true.
// All other sessions receive 403.
// perm must be the JSON key of a StandardPermissions field (e.g. "terminal").
func RequirePermission(perm string) func(http.Handler) http.Handler
```

Implementation sketch:
```go
func RequirePermission(perm string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            sess := MustSession(r)
            if sess.Role == config.RoleAdmin {
                next.ServeHTTP(w, r)
                return
            }
            if sess.Role == config.RoleStandard {
                users, _ := config.LoadUsers()
                u := config.FindUserByID(users, sess.UserID)
                if u != nil && u.StandardPerms != nil && permEnabled(u.StandardPerms, perm) {
                    next.ServeHTTP(w, r)
                    return
                }
            }
            jsonErr(w, http.StatusForbidden, "permission denied")
        })
    }
}

// permEnabled reads the named flag from a StandardPermissions value via a lookup map.
func permEnabled(p *StandardPermissions, perm string) bool {
    switch perm {
    case "terminal":           return p.Terminal
    case "review_sudoers":     return p.ReviewSudoers
    case "browse_files":       return p.BrowseFiles
    case "manage_pool_dataset":return p.ManagePoolDataset
    case "manage_smb":         return p.ManageSMB
    case "manage_nfs":         return p.ManageNFS
    case "manage_iscsi":       return p.ManageISCSI
    case "manage_protection":  return p.ManageProtection
    case "manage_snapshots":   return p.ManageSnapshots
    case "edit_settings":      return p.EditSettings
    case "manage_interlink":   return p.ManageInterlink
    }
    return false
}
```

`config.FindUserByID` already exists; the overhead is one JSON read per protected request
(config uses an RW mutex, so this is safe and fast enough for interactive use).

---

## Route changes — `handlers/router.go`

Replace `RequireAdmin` with `RequirePermission("...")` on the routes below.
Routes **not** listed stay `RequireAdmin` (user management, service install, OS updates,
binary update, UPS/MinIO install, cert management, sudoers apply/enable, power commands,
file chown/chmod, kill sessions).

| Route(s) | Change |
|---|---|
| `/ws/terminal` | `RequireAdmin` → `RequirePermission("terminal")` |
| `GET /api/sudoers/status` | `RequireAdmin` → `RequirePermission("review_sudoers")` |
| `GET /api/sudoers/diff` | `RequireAdmin` → `RequirePermission("review_sudoers")` |
| `GET /api/files/list` | `RequireAuth` → `RequirePermission("browse_files")` |
| `GET /api/files/users-groups` | `RequireAuth` → `RequirePermission("browse_files")` |
| `POST /api/pool` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/detect` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/import` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `PUT /api/pool` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/grow` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/export` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/destroy` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/upgrade` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/fixer/*` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/pool/arc` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/datasets` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `PUT /api/datasets/{name}` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `DELETE /api/datasets/{name}` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/zvols` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `PUT /api/zvols/{name}` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `DELETE /api/zvols/{name}` | `RequireAdmin` → `RequirePermission("manage_pool_dataset")` |
| `POST /api/smb/password` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `POST /api/smb/service` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `PUT /api/smb/global-config` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `POST /api/shares` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `PUT /api/shares/{name}` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `DELETE /api/shares/{name}` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `POST /api/shares/{name}/clean-recycle` | `RequireAdmin` → `RequirePermission("manage_smb")` |
| `POST /api/nfs/service` | `RequireAdmin` → `RequirePermission("manage_nfs")` |
| `POST /api/nfs/shares` | `RequireAdmin` → `RequirePermission("manage_nfs")` |
| `PUT /api/nfs/shares/{id}` | `RequireAdmin` → `RequirePermission("manage_nfs")` |
| `DELETE /api/nfs/shares/{id}` | `RequireAdmin` → `RequirePermission("manage_nfs")` |
| iSCSI service/config/host/share CRUD | `RequireAdmin` → `RequirePermission("manage_iscsi")` |
| `POST /api/snapshot-schedules` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `PUT /api/snapshot-schedules/{id}` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `DELETE /api/snapshot-schedules/{id}` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `POST /api/snapshot-schedules/{id}/run-now` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `POST /api/pool/scrub/start` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `POST /api/pool/scrub/stop` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `PUT /api/pool/scrub/schedule` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `PUT /api/treemap/schedule` | `RequireAdmin` → `RequirePermission("manage_protection")` |
| replication CRUD + run | `RequireAdmin` → `RequirePermission("manage_protection")` |
| `POST /api/snapshots` | `RequireAdmin` → `RequirePermission("manage_snapshots")` |
| `DELETE /api/snapshots/{name}` | `RequireAdmin` → `RequirePermission("manage_snapshots")` |
| `POST /api/snapshots/restore` | `RequireAdmin` → `RequirePermission("manage_snapshots")` |
| `PUT /api/settings` | `RequireAdmin` → `RequirePermission("edit_settings")` |
| `PUT /api/settings/timezone` | `RequireAdmin` → `RequirePermission("edit_settings")` |
| `POST /api/interlink/link` | `RequireAdmin` → `RequirePermission("manage_interlink")` |
| `DELETE /api/interlink/{id}` | `RequireAdmin` → `RequirePermission("manage_interlink")` |
| `POST /api/interlink/switch` | `RequireAuth` → stays `RequireAuth` (all logged-in users may switch) |
| push-interlink start/start-dataset | `RequireAdmin` → `RequirePermission("manage_interlink")` |

---

## `/api/auth/me` — `handlers/auth.go`

Include `standard_perms` in the response (nil → omitted):

```go
jsonOK(w, map[string]interface{}{
    "user_id":        sess.UserID,
    "username":       sess.Username,
    "role":           sess.Role,
    "totp_enabled":   user != nil && user.TOTPEnabled,
    "preferences":    prefs,
    "standard_perms": standardPerms, // *StandardPermissions or nil
})
```

The frontend stores this in `currentUser.standard_perms`.

---

## User handlers — `handlers/users.go`

### Create user (`POST /api/users`)

- Accept optional `standard_perms` object in request body (ignored unless `role == "standard"`)
- Initialise `StandardPerms` to `&StandardPermissions{}` when role is standard and no perms
  object is sent, so the user is created with all flags false (safe default)

### Update user (`PUT /api/users/{id}`)

- Accept optional `standard_perms` in request body
- When role changes away from standard, set `StandardPerms = nil`
- When role changes to standard, set `StandardPerms` from body (or `&StandardPermissions{}` if absent)

---

## Frontend — `static/index.html`

### `hasPerm(key)` helper

```js
function hasPerm(key) {
  if (!currentUser) return false;
  if (currentUser.role === 'admin') return true;
  if (currentUser.role === 'standard') return !!(currentUser.standard_perms?.[key]);
  return false;
}
```

Defined once, used everywhere instead of repeating `currentUser.role === 'admin'` guards.

### Replacing admin guards

Every existing `currentUser.role === 'admin'` check that gates a UI element corresponding
to one of the 11 capabilities must be replaced with `hasPerm('...')`.  Examples:

| Existing guard | Replace with |
|---|---|
| `currentUser.role === 'admin'` on terminal nav / button | `hasPerm('terminal')` |
| `currentUser.role === 'admin'` on Edit/Delete dataset buttons | `hasPerm('manage_pool_dataset')` |
| `currentUser.role === 'admin'` on New Share / Edit Share | `hasPerm('manage_smb')` |
| `currentUser.role === 'admin'` on Create Snapshot / Delete Snapshot | `hasPerm('manage_snapshots')` |
| `currentUser.role === 'admin'` on Settings save button | `hasPerm('edit_settings')` |
| (etc.) | |

Guards on routes that stay admin-only (user management, service install, OS updates, power
menu, etc.) keep `currentUser.role === 'admin'` unchanged.

### Create/Edit User modal — permissions panel

When the Role `<select>` value is `"standard"`, a collapsible permissions panel appears
immediately below:

```
Role  [Standard ▾]

Capabilities
─────────────────────────────────────────────────
☐  Terminal
☐  Review Sudoers  (read-only — apply stays admin-only)
☐  Browse Files    (listing only — chown/chmod stay admin-only)
☐  Manage Pool & Datasets
☐  Manage SMB Shares
☐  Manage NFS Shares
☐  Manage iSCSI Shares
☐  Manage Protection Policies  (schedules, scrub, replication)
☐  Create / Delete Snapshots
☐  Edit Settings
☐  Manage InterLink
─────────────────────────────────────────────────
```

The panel is hidden (via `display:none`) when Role is admin / read-only / smb-only.

On submit the `standard_perms` object is assembled from checkbox state and sent in the
create/update body alongside `role: "standard"`.

On edit-user open, if the loaded user has `role === "standard"`, show the panel and
pre-check boxes from `user.standard_perms`.

### User list display

In the users table, standard-role users show their enabled capability count as a small
badge next to the role label, e.g. `standard (4)`.  Clicking it does nothing — it is
informational.

---

## Files changed summary

| File | Change |
|---|---|
| `internal/config/config.go` | Add `StandardPermissions` struct + `StandardPerms` field on `User` + `RoleStandard = "standard"` constant |
| `handlers/middleware.go` | Add `RequirePermission(perm string)` + `permEnabled()` helper |
| `handlers/router.go` | Swap `RequireAdmin` → `RequirePermission(...)` on ~40 routes; swap `RequireAuth` → `RequirePermission("browse_files")` on file-list routes |
| `handlers/auth.go` | Include `standard_perms` in `/api/auth/me` response |
| `handlers/users.go` | Accept + persist `standard_perms` on create/update; clear on role change away from standard |
| `static/index.html` | `hasPerm()` helper; replace admin guards with `hasPerm(...)` throughout; permissions panel in create/edit user modals; capability badge in users table |

---

## Version bump

`internal/version/version.go`: `"6.5.0"`

## Status: PLANNED
