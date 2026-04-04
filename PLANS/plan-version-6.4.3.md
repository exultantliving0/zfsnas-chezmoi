# Plan — Version 6.4.3

## Overview

**File Browser** — a contextual file browser modal accessible from the 3-dot action menu on
datasets, SMB shares, and NFS shares.  The browser shows the contents of the share/dataset
mountpoint, lets the user navigate into subfolders via a breadcrumb, and lets admins change
file/folder ownership and permissions in place, optionally recursively.

---

## Entry points — where Browse Files appears

### Dataset 3-dot menu (`toggleDsMenu`)
New item added after "Take Snapshot" and before the separator before "Delete":
```
📂 Browse Files   → openFileBrowser('dataset', dsName)
```

### SMB Share 3-dot menu (`toggleShareMenu`)
New item added after "Edit Related Dataset" and before the separator before "Delete":
```
📂 Browse Files   → openFileBrowser('smb', share.name)
```

### NFS Share 3-dot menu (`toggleNFSMenu`)
New item added after "Edit Related Dataset" and before the separator before "Delete":
```
📂 Browse Files   → openFileBrowser('nfs', shareId)
```

---

## How the root path is resolved per source

| Source | Root path used |
|---|---|
| Dataset | ZFS mountpoint: `zfs get -H -o value mountpoint <dataset>` |
| SMB Share | `share.path` field stored in config |
| NFS Share | `share.path` field stored in config |

The resolved root is sent to the API as an opaque `root` token (base64url of the absolute path).
The backend validates it against the known list of legal roots (all dataset mountpoints + SMB paths
+ NFS paths) before trusting it.  This prevents a crafted request from accessing arbitrary paths.

---

## File Browser Modal — UI design

The modal is wide (max 960 px) and tall (fixed 72 vh inner scrollable area).

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  📂 Browse Files — tank/media                                  [×]           │
│                                                                              │
│  / > movies > action                                                         │
│  ─────────────────────────────────────────────────────────────────────────   │
│                                                                              │
│  Name                  Size       Permissions   Owner     Group    Modified  │
│  📁 comedies           —          drwxr-xr-x    root      data     2026-03   │
│  📁 drama              —          drwxrwxr-x    alice     data     2026-02   │
│  📄 sample.mkv         4.2 GB     -rw-r--r--    alice     data     2026-01   │
│  📄 readme.txt         1.2 KB     -rw-r--r--    root      root     2025-12   │
│                                                                              │
│  (rows are clickable for folders; 3-dot menu per row for admin actions)      │
│  ─────────────────────────────────────────────────────────────────────────   │
│                                                                              │
│                                                           [Close]            │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Breadcrumb bar

- Starts at `/` (representing the share/dataset root, not the real filesystem root)
- Each segment is a clickable link to navigate back up
- Current folder segment is plain text (not a link)
- A `← Back` link appears left of the breadcrumb when depth > 0

### File listing table

Columns:
- **Name** — folder icon `📁` or file icon `📄`; folders are clickable links to drill down
- **Size** — human-readable (`4.2 GB`, `1.2 KB`, `—` for directories)
- **Permissions** — `ls -la` style string (`drwxr-xr-x`)
- **Owner** — username string
- **Group** — group name string
- **Modified** — `YYYY-MM` display

Sorted: directories first, then files, both alphabetically.

### Per-row 3-dot menu (admin only)

Each row has a `⋮` button (same pattern as share/dataset rows).  Menu items:
```
🔑 Change Ownership…
🔒 Change Permissions…
```

Read-only users see the table but no `⋮` column and no action buttons.

---

## Change Ownership sub-modal

Opens stacked over the browser modal (same backdrop, higher z-index modal).

```
┌─────────────────────────────────────────────────────┐
│  Change Ownership                          [×]       │
│  /movies/action/sample.mkv                           │
│                                                      │
│  Owner   [alice              ▾]  (user list)         │
│  Group   [data               ▾]  (group list)        │
│                                                      │
│  ☐  Apply recursively to all contents               │
│     (only shown when target is a directory)          │
│                                                      │
│  [Cancel]                         [Apply Ownership]  │
└─────────────────────────────────────────────────────┘
```

- Owner and Group are `<select>` dropdowns populated from `GET /api/files/users-groups`
- Recursive checkbox visible only when the target is a directory
- On submit: `POST /api/files/chown`
- On success: refreshes the current browser listing

---

## Change Permissions sub-modal

```
┌─────────────────────────────────────────────────────┐
│  Change Permissions                        [×]       │
│  /movies/action/sample.mkv   (current: 644)          │
│                                                      │
│              Read   Write   Execute                  │
│  Owner        ☑      ☑       ☐                      │
│  Group        ☑      ☐       ☐                      │
│  Others       ☑      ☐       ☐                      │
│                                                      │
│  Octal: [644]   (live-updates as checkboxes change)  │
│                                                      │
│  ☐  Apply recursively to all contents               │
│     (only shown when target is a directory)          │
│                                                      │
│  [Cancel]                        [Apply Permissions] │
└─────────────────────────────────────────────────────┘
```

- Checkboxes and octal field stay in sync (changing one updates the other)
- Recursive checkbox visible only when the target is a directory
- On submit: `POST /api/files/chmod`
- On success: refreshes the current browser listing and updates the displayed permissions string

---

## API design

### `GET /api/files/list`

Query params:
- `root` — base64url-encoded absolute root path (validated server-side against known roots)
- `subpath` — relative path within root (empty = root itself); URL-encoded

Response:
```json
{
  "root_label": "tank/media",
  "subpath": "movies/action",
  "entries": [
    {
      "name": "sample.mkv",
      "is_dir": false,
      "size_bytes": 4509715660,
      "permissions": "-rw-r--r--",
      "mode_octal": "644",
      "owner": "alice",
      "group": "data",
      "modified_unix": 1738350000
    }
  ]
}
```

Backend implementation: `os.ReadDir` + `os.Stat` (via `syscall.Stat_t` for uid/gid) — no sudo
required for reading.  Owner and group names resolved from `/etc/passwd` and `/etc/group` with
a small in-process cache (invalidated every 60 s).

### `GET /api/files/users-groups`

Returns:
```json
{ "users": ["alice", "bob", "root", ...], "groups": ["data", "root", "users", ...] }
```

Reads `/etc/passwd` and `/etc/group` directly in Go — no sudo needed.

### `POST /api/files/chown`

Body:
```json
{
  "root":      "<base64url>",
  "subpath":   "movies/action/sample.mkv",
  "owner":     "bob",
  "group":     "data",
  "recursive": false
}
```

Backend: executes `sudo chown [-R] bob:data <absolute-path>`.
Admin-only.  Validates root token, validates subpath has no traversal, validates owner/group
exist in `/etc/passwd` and `/etc/group` before shelling out.

### `POST /api/files/chmod`

Body:
```json
{
  "root":      "<base64url>",
  "subpath":   "movies/action/sample.mkv",
  "mode":      "755",
  "recursive": false
}
```

Backend: executes `sudo chmod [-R] 755 <absolute-path>`.
Admin-only.  Validates mode is a 3-digit octal string (000–777).

---

## Security model

| Check | Detail |
|---|---|
| Root validation | Backend resolves all dataset mountpoints + SMB paths + NFS paths at request time; `root` token must decode to one of these — no exceptions |
| Subpath sanitisation | `filepath.Clean(subpath)` must not start with `..`; result joined with root via `filepath.Join` and re-verified with `strings.HasPrefix` |
| Owner/group whitelisting | Only names present in `/etc/passwd` / `/etc/group` are accepted for chown |
| Mode validation | Regex `^[0-7]{3}$` before calling chmod |
| Admin gate | List endpoint: `RequireAuth`; chown/chmod: `RequireAuth(RequireAdmin(...))` |
| Audit | Every chown and chmod action is logged to the audit trail |

---

## New files

### `system/filebrowser.go`

```go
package system

// FileEntry describes one file or directory returned by ListDir.
type FileEntry struct {
    Name        string `json:"name"`
    IsDir       bool   `json:"is_dir"`
    SizeBytes   int64  `json:"size_bytes"`
    Permissions string `json:"permissions"`  // e.g. "-rw-r--r--"
    ModeOctal   string `json:"mode_octal"`   // e.g. "644"
    Owner       string `json:"owner"`
    Group       string `json:"group"`
    ModifiedUnix int64 `json:"modified_unix"`
}

// FileBrowserResult is returned by the list API.
type FileBrowserResult struct {
    RootLabel string      `json:"root_label"`
    Subpath   string      `json:"subpath"`
    Entries   []FileEntry `json:"entries"`
}

// ListDir reads the contents of root/subpath using os.ReadDir + syscall.Stat_t.
// Entries are sorted: directories first, then files, both alphabetically.
func ListDir(root, subpath string) (*FileBrowserResult, error)

// GetSystemUsersGroups reads /etc/passwd and /etc/group and returns sorted name lists.
func GetSystemUsersGroups() (users, groups []string, err error)

// ChownPath runs: sudo chown [-R] owner:group <abs-path>
func ChownPath(absPath, owner, group string, recursive bool) error

// ChmodPath runs: sudo chmod [-R] mode <abs-path>
func ChmodPath(absPath, mode string, recursive bool) error

// ResolveKnownRoots builds the set of legal root paths from the current config:
// ZFS mountpoints, SMB share paths, NFS share paths.
func ResolveKnownRoots(configDir string) (map[string]string, error)
// map key = absolute path, value = human-readable label (e.g. "tank/media")

// ValidateRootToken decodes a base64url root token and checks it against the
// known roots map.  Returns the absolute path and label, or an error.
func ValidateRootToken(token string, knownRoots map[string]string) (absPath, label string, err error)

// SafeJoin joins root + subpath and verifies the result is still under root.
// Returns an error if the joined path escapes root (directory traversal attempt).
func SafeJoin(root, subpath string) (string, error)
```

### `handlers/filebrowser.go`

```go
package handlers

// GET  /api/files/list          → HandleFileBrowserList
// GET  /api/files/users-groups  → HandleFileBrowserUsersGroups
// POST /api/files/chown         → HandleFileBrowserChown   (admin)
// POST /api/files/chmod         → HandleFileBrowserChmod   (admin)
```

---

## Routes — `handlers/router.go`

```go
r.Handle("/api/files/list",
    RequireAuth(http.HandlerFunc(HandleFileBrowserList))).Methods("GET")
r.Handle("/api/files/users-groups",
    RequireAuth(http.HandlerFunc(HandleFileBrowserUsersGroups))).Methods("GET")
r.Handle("/api/files/chown",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleFileBrowserChown)))).Methods("POST")
r.Handle("/api/files/chmod",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleFileBrowserChmod)))).Methods("POST")
```

---

## Audit entries

New constants in `internal/audit/audit.go`:
- `ActionFileBrowserChown` — logged as `"chown <owner>:<group> <path> [recursive]"`
- `ActionFileBrowserChmod` — logged as `"chmod <mode> <path> [recursive]"`

---

## SECURITY.md + sudoers additions

Two new sudo entries are required for chown and chmod operations.

Add to the `ZFSNAS_FILES` Cmnd_Alias (new alias):

```sudoers
# ── File Browser (v6.4.3+) ────────────────────────────────────────────────────
/usr/bin/chown *
/usr/bin/chmod *
```

Add `ZFSNAS_FILES` to the grant line.

Add a notes entry:
> **`chown *` and `chmod *`** — used by the File Browser feature (v6.4.3+) to change ownership
> and permissions on files and folders within dataset mountpoints, SMB share paths, and NFS share
> paths.  The portal validates the target path against the set of known share/dataset roots before
> calling these commands; arbitrary paths cannot be targeted via the UI.

---

## JS functions (`static/index.html`)

```js
// Entry points — called from 3-dot menus
function openFileBrowser(sourceType, sourceId)
// sourceType: 'dataset' | 'smb' | 'nfs'
// Calls GET /api/files/list?root=...&subpath= to load root listing,
// then opens modal-file-browser.

// Navigation
async function fileBrowserNavigate(subpath)
// Updates breadcrumb, calls API, re-renders table.

function renderFileBrowserBreadcrumb(subpath)
function renderFileBrowserTable(entries)

// Per-row actions (admin only)
function toggleFileBrowserRowMenu(event, entryIdx)
function openChownModal(entryIdx)
function openChmodModal(entryIdx)

// Ownership modal
async function loadUsersGroups()           // GET /api/files/users-groups (cached)
async function applyChown()                // POST /api/files/chown

// Permissions modal
function syncChmodCheckboxes()             // checkboxes → octal input
function syncChmodOctal()                  // octal input → checkboxes
async function applyChmod()                // POST /api/files/chmod

// Helpers
function encodeRootToken(absPath)          // base64url encode for API calls
function formatFileSize(bytes)             // e.g. 4509715660 → "4.2 GB"
function closeFileBrowser()
function closeChownModal()
function closeChmodModal()
```

Module-level state:
```js
let fileBrowserState = {
  root: '',          // base64url token
  rootLabel: '',     // display name (e.g. "tank/media")
  subpath: '',       // current relative path within root
  entries: [],       // current FileEntry[]
  usersGroups: null, // cached { users, groups } from API
  activeEntryIdx: -1 // entry targeted by sub-modal
};
```

---

## New modals (`static/index.html`)

### `modal-file-browser`

```html
<div class="modal-backdrop hidden" id="modal-file-browser">
  <div class="modal" style="max-width:960px;">
    <div class="modal-header">
      <h3>📂 Browse Files — <span id="fb-root-label"></span></h3>
      <button class="modal-close" onclick="closeFileBrowser()">✕</button>
    </div>
    <div class="modal-body" style="padding:0;">
      <div id="fb-breadcrumb" class="fb-breadcrumb"></div>
      <div class="fb-table-wrap">
        <table class="tbl" id="fb-table">
          <thead>
            <tr>
              <th>Name</th><th>Size</th><th>Permissions</th>
              <th>Owner</th><th>Group</th><th>Modified</th>
              <th class="admin-only"></th>
            </tr>
          </thead>
          <tbody id="fb-table-body"></tbody>
        </table>
      </div>
    </div>
    <div class="modal-footer">
      <button class="btn btn-ghost" onclick="closeFileBrowser()">Close</button>
    </div>
  </div>
</div>
```

### `modal-fb-chown`

```html
<div class="modal-backdrop hidden" id="modal-fb-chown">
  <div class="modal" style="max-width:480px;">
    <div class="modal-header"><h3>Change Ownership</h3>
      <button class="modal-close" onclick="closeChownModal()">✕</button></div>
    <div class="modal-body">
      <p id="fb-chown-path" class="fb-target-path"></p>
      <div class="form-row">
        <label>Owner</label>
        <select id="fb-chown-owner"></select>
      </div>
      <div class="form-row">
        <label>Group</label>
        <select id="fb-chown-group"></select>
      </div>
      <label class="checkbox-label" id="fb-chown-recursive-wrap">
        <input type="checkbox" id="fb-chown-recursive">
        Apply recursively to all contents
      </label>
      <div class="alert alert-error hidden" id="fb-chown-err"></div>
    </div>
    <div class="modal-footer">
      <button class="btn btn-ghost" onclick="closeChownModal()">Cancel</button>
      <button class="btn btn-primary" onclick="applyChown()">Apply Ownership</button>
    </div>
  </div>
</div>
```

### `modal-fb-chmod`

```html
<div class="modal-backdrop hidden" id="modal-fb-chmod">
  <div class="modal" style="max-width:440px;">
    <div class="modal-header"><h3>Change Permissions</h3>
      <button class="modal-close" onclick="closeChmodModal()">✕</button></div>
    <div class="modal-body">
      <p id="fb-chmod-path" class="fb-target-path"></p>
      <table class="tbl fb-chmod-table">
        <thead><tr><th></th><th>Read</th><th>Write</th><th>Execute</th></tr></thead>
        <tbody>
          <tr><td>Owner</td>
            <td><input type="checkbox" id="chmod-ur"></td>
            <td><input type="checkbox" id="chmod-uw"></td>
            <td><input type="checkbox" id="chmod-ux"></td></tr>
          <tr><td>Group</td>
            <td><input type="checkbox" id="chmod-gr"></td>
            <td><input type="checkbox" id="chmod-gw"></td>
            <td><input type="checkbox" id="chmod-gx"></td></tr>
          <tr><td>Others</td>
            <td><input type="checkbox" id="chmod-or"></td>
            <td><input type="checkbox" id="chmod-ow"></td>
            <td><input type="checkbox" id="chmod-ox"></td></tr>
        </tbody>
      </table>
      <div class="form-row" style="margin-top:12px;">
        <label>Octal</label>
        <input type="text" id="fb-chmod-octal" maxlength="3" style="width:60px;">
      </div>
      <label class="checkbox-label" id="fb-chmod-recursive-wrap">
        <input type="checkbox" id="fb-chmod-recursive">
        Apply recursively to all contents
      </label>
      <div class="alert alert-error hidden" id="fb-chmod-err"></div>
    </div>
    <div class="modal-footer">
      <button class="btn btn-ghost" onclick="closeChmodModal()">Cancel</button>
      <button class="btn btn-primary" onclick="applyChmod()">Apply Permissions</button>
    </div>
  </div>
</div>
```

---

## CSS additions (`static/style.css`)

```css
/* File Browser */
.fb-breadcrumb         { display:flex; align-items:center; gap:6px; padding:10px 16px;
                         border-bottom:1px solid var(--border); flex-wrap:wrap;
                         font-size:.85rem; }
.fb-breadcrumb a       { color:var(--accent); text-decoration:none; cursor:pointer; }
.fb-breadcrumb a:hover { text-decoration:underline; }
.fb-breadcrumb .sep    { color:var(--muted); }
.fb-table-wrap         { overflow-y:auto; max-height:calc(72vh - 130px); }
.fb-name-link          { cursor:pointer; color:var(--accent); text-decoration:none; }
.fb-name-link:hover    { text-decoration:underline; }
.fb-target-path        { font-size:.8rem; color:var(--muted); word-break:break-all;
                         margin:0 0 14px; }
.fb-chmod-table        { width:100%; margin-bottom:4px; }
.fb-chmod-table td,
.fb-chmod-table th     { text-align:center; padding:4px 8px; }
.fb-chmod-table td:first-child { text-align:left; font-weight:500; width:70px; }
```

---

## Files changed summary

| File | Change |
|---|---|
| `system/filebrowser.go` | New — `ListDir`, `ChownPath`, `ChmodPath`, `ResolveKnownRoots`, `ValidateRootToken`, `SafeJoin`, `GetSystemUsersGroups` |
| `handlers/filebrowser.go` | New — list, users-groups, chown, chmod handlers |
| `handlers/router.go` | Register 4 new routes |
| `internal/audit/audit.go` | Add `ActionFileBrowserChown`, `ActionFileBrowserChmod` constants |
| `static/index.html` | 3 new modals + JS functions + 3-dot menu entries in dataset/SMB/NFS rows |
| `static/style.css` | File browser layout styles |
| `SECURITY.md` | Add `chown *` and `chmod *` entries under new `ZFSNAS_FILES` alias |

---

## Version bump

`internal/version/version.go`: `"6.4.3"`

## Status: PLANNED
