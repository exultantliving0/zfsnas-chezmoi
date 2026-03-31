# Plan — Version 6.3.31

## Overview

**Sudoers Hardening Manager** — an optional Security feature in the Prerequisites tab that lets
the portal compare its installed `/etc/sudoers.d/zfsnas` file against the canonical required
template, and apply any missing or obsolete lines in a guided, line-annotated review popup.

The feature is available only when the portal has the authority to overwrite its own sudoers
file — either via unrestricted sudo (`root`, `all`) or via a hardened sudoers that already
contains `tee /etc/sudoers.d/zfsnas`.

---

## Feature behaviour — two-stage design

### Stage 1 — not yet enabled

When the portal detects unrestricted sudo access (`type == "root"` or `type == "all"`) **or** a
hardened sudoers that includes write permission to `/etc/sudoers.d/zfsnas`, the Security section
of the Prerequisites tab shows a new card:

```
┌──────────────────────────────────────────────────────────────────┐
│ 🔒 Sudoers Hardening                                             │
│                                                                  │
│ Unrestricted sudo detected.  Restricting sudo to the exact       │
│ commands this portal needs reduces the blast radius of any       │
│ compromised session or code path.                                │
│                                                                  │
│  [Enable Sudoers Hardening]                                      │
└──────────────────────────────────────────────────────────────────┘
```

Clicking "Enable Sudoers Hardening" sets `AppConfig.SudoersHardeningEnabled = true` and saves
config.  The file is not written yet — only monitoring is activated.

### Stage 2 — enabled, diff found

After enabling, the button is replaced by an "Approve Sudoers Changes" button whenever the
installed file diverges from the required template (including when the file does not exist yet):

```
┌──────────────────────────────────────────────────────────────────┐
│ 🔒 Sudoers Hardening    ✓ Enabled                               │
│                                                                  │
│  ⚠  5 line(s) differ from the required sudoers template.        │
│                                                                  │
│  [Approve Sudoers Changes]                                       │
└──────────────────────────────────────────────────────────────────┘
```

If enabled and up to date:

```
│ 🔒 Sudoers Hardening    ✓ Enabled    ✓ Up to date              │
```

---

## Diff review popup

Opens when the user clicks "Approve Sudoers Changes".  It shows a structured, visually rich
modal with one row per differing line.

```
┌──────────────────────────────────────────────────────────────────────┐
│  Sudoers Review — /etc/sudoers.d/zfsnas              [×]            │
│  5 change(s) pending                                                 │
│                                                                      │
│  ── Missing lines (need to be added) ──────────────────────────────  │
│                                                                      │
│  + /usr/bin/tee /etc/sudoers.d/zfsnas                               │
│    Lets the portal write its own sudoers file when using the        │
│    Sudoers Hardening feature. Required for in-app changes to work   │
│    after sudo access is restricted.                                 │
│    [✓ Approve]  [— Silence]                                         │
│                                                                      │
│  + /usr/sbin/zpool replace *                                        │
│    Pool Fixer Wizard: replaces a failed disk in a DEGRADED pool     │
│    with a spare (v6.3.21+).                                         │
│    [✓ Approve]  [— Silence]                                         │
│                                                                      │
│  + /usr/bin/zfs allow *                                             │
│    Delegates ZFS permissions to the service account for InterLink  │
│    replication without requiring sudo on the remote side (v6.3.26+)│
│    [✓ Approve]  [— Silence]                                         │
│                                                                      │
│  ── Extra lines (present but no longer needed) ─────────────────── │
│                                                                      │
│  - /usr/bin/nvme *                                                  │
│    Replaced by the more targeted `nvme smart-log -o json *` entry  │
│    in v3.0.0.  The broad wildcard is no longer required.           │
│    [✓ Approve removal]  [— Silence]                                 │
│                                                                      │
│  - /usr/sbin/sgdisk *                                               │
│    Outdated Debian path.  The current entry uses the resolved path  │
│    returned by `which sgdisk` on this system.                       │
│    [✓ Approve removal]  [— Silence]                                 │
│                                                                      │
│  ── Silenced (ignored, will not be applied) ────────────────────── │
│  (shown only when silenced entries exist — collapsed by default)    │
│                                                                      │
│  ─────────────────────────────────────────────────────────────────  │
│                                                                      │
│  [Apply 3 Approved Changes]              [Cancel]                   │
└──────────────────────────────────────────────────────────────────────┘
```

### Row states

| State | Appearance | Button label |
|---|---|---|
| Missing — not yet actioned | Green `+` prefix, white background | `✓ Approve` / `— Silence` |
| Missing — approved | Green `+` prefix, green tint | `✓ Approved` (toggle off) |
| Missing — silenced | Grey `+` prefix, grey tint | `— Silenced` (toggle off) |
| Extra — not yet actioned | Red `–` prefix, white background | `✓ Approve removal` / `— Silence` |
| Extra — approved | Red `–` prefix, red tint | `✓ Approved` (toggle off) |
| Extra — silenced | Grey `–` prefix, grey tint | `— Silenced` |

The "Apply" button label updates in real time to reflect the count of approved (not silenced)
changes.  It is disabled when no changes are approved.

### Apply logic

When the user clicks Apply, the backend:
1. Starts with the **required template** as the base content.
2. Removes any "missing" lines that the user silenced (they chose not to add them).
3. Re-inserts any "extra" lines that the user silenced (they chose to keep them).
4. Runs `sudo tee /etc/sudoers.d/zfsnas` with the assembled content.
5. Runs `visudo -c -f /etc/sudoers.d/zfsnas` (read-only syntax check, no sudo needed).
6. On success: saves silenced-line decisions to `AppConfig.SudoersSilencedLines`; logs to audit.
7. On visudo failure: rolls back by writing the original content and returns an error.

---

## Access detection logic

```go
// CanManageSudoers returns true when the current process can overwrite
// /etc/sudoers.d/zfsnas.  True in three situations:
//   1. Running as root (UID 0)
//   2. Blanket NOPASSWD: ALL sudoers
//   3. Hardened sudoers that already contains tee /etc/sudoers.d/zfsnas
func CanManageSudoers(status SudoStatus) bool
```

Check for case 3 is a substring scan of `sudo -l -n` output for
`tee /etc/sudoers.d/zfsnas` (using same path-resolution logic as `CheckSudoAccess`).

---

## Diff computation

Comparison is done at the **logical line** level:

- Blank lines and comment lines (`#`) are ignored when computing the diff.
- Continuation lines (ending in `\`) are joined before comparison.
- Each resulting logical entry (a single `command [args]` string) is treated as one diff unit.
- Order within a Cmnd_Alias block is not significant — only presence/absence matters.
- The grant line (`zfsnas ALL=(ALL) NOPASSWD: …`) is compared as a whole unit.

The diff result is two sets: `missing` (in required, not in current) and `extra` (in current,
not in required).  Lines in both sets are not shown.

---

## Explanation map

`system/sudoers_hardening.go` contains a compile-time map from normalised command strings to
explanation text.  Each entry is one or two sentences covering what the command does and which
feature/version introduced it.  Representative entries:

| Command pattern | Explanation |
|---|---|
| `zpool *` | All ZFS pool operations: create, import, export, scrub, status, offline/online (v1.0.0+). Covers Pool Fixer Wizard clear/replace actions (v6.3.21+). |
| `zfs *` | All ZFS dataset operations: create, destroy, set, get, snapshot, rollback, load-key/unload-key (encryption v5.0.0+), send/recv (replication v6.1.0+), allow (delegation v6.3.26+). |
| `smartctl *` | SMART disk health data for SAS/SATA drives. Required for the Disk Health and SMART detail pages. |
| `nvme smart-log -o json *` | NVMe drive health and temperature data (v3.0.0+). The narrower form replaces an earlier broad wildcard. |
| `apt-get *` | Package installation and OS updates. Used by the Prerequisites tab install button and the Settings > OS Updates page. |
| `tee /etc/samba/smb.conf` | Writes the Samba configuration file when a share is created, edited, or deleted. |
| `tee /etc/exports` | Writes the NFS export table; `exportfs -ra` is called immediately after to apply the change. |
| `tee /etc/systemd/system/zfsnas.service` | Writes the systemd unit file when the portal registers itself as a system service. |
| `tee /etc/modprobe.d/zfs.conf` | Persists ARC size limits across reboots (v6.3.22+). The file is only ever written with `options zfs ...` lines. |
| `tee /sys/module/zfs/parameters/zfs_arc_max` | Applies a new ARC maximum immediately via the ZFS sysfs interface without requiring a reboot. |
| `tee /etc/sudoers.d/zfsnas` | Lets the portal write its own sudoers file when using the Sudoers Hardening feature. Required so in-app changes continue to work after sudo is restricted. |
| `useradd *` | Creates the Linux account for a new portal user. Wildcards are required to support optional --uid/--gid/--no-user-group flags (v3.0.0+). |
| `smbpasswd *` | Sets or removes the Samba password for a user. Required for SMB authentication to work correctly. |
| `wipefs -a *` | Clears all disk signatures before adding a disk to a pool. Prevents ZFS import conflicts from stale labels. |
| `sgdisk --zap-all *` (or `sgdisk *`) | Wipes the GPT partition table before pool creation. The exact path varies by distribution; the portal uses the path returned by `which sgdisk`. |
| `dd if=/dev/zero *` | Zero-fills the first sectors of a disk during the wipe-disk workflow to ensure a clean slate. |
| `timedatectl set-timezone *` | Sets the system timezone from the Settings > System page. |
| `shutdown *` | Allows scheduled shutdown/reboot from the power menu and from the UPS shutdown watcher. |
| `exportfs -ra` | Reloads all NFS exports after the export table is updated. |
| `du -b -d 6 *` | Powers the Folder TreeMap — scans dataset mount points for per-folder disk usage without requiring root file access. |
| `find *` | Deletes files and empty directories inside `.recycle/` folders on SMB shares (recycle bin cleanup, v6.0.0+). |
| `nut-scanner *` | Scans USB and SNMP buses for attached UPS devices. Requires raw USB access so it must run as root. |
| `tee /etc/nut/ups.conf` | Writes the NUT UPS device configuration file when you configure or reconfigure your UPS. |
| `wget -q -O /usr/local/bin/minio *` | Downloads the MinIO binary from dl.min.io during feature install. Destination path is fixed; only the source URL is a wildcard. |
| `targetcli` | Configures iSCSI LIO targets, backstores, and ACLs via a piped script. No user-supplied input is passed directly to the shell. |

Lines with no known explanation are displayed with a generic fallback:
`"Command required by the portal — no detailed explanation available for this entry."`

---

## New files

### `system/sudoers_hardening.go`

```go
package system

// SudoersDiff is the result of comparing the installed sudoers file against the
// required template.
type SudoersDiff struct {
    UpToDate     bool              `json:"up_to_date"`
    FileExists   bool              `json:"file_exists"`
    MissingLines []SudoersLineDiff `json:"missing_lines"`
    ExtraLines   []SudoersLineDiff `json:"extra_lines"`
}

// SudoersLineDiff describes a single logical sudoers command entry that differs.
type SudoersLineDiff struct {
    Line        string `json:"line"`        // normalised command string
    Explanation string `json:"explanation"` // why this line is needed/was removed
    Silenced    bool   `json:"silenced"`    // user has silenced this change
}

// CanManageSudoers returns true when the process can overwrite /etc/sudoers.d/zfsnas.
func CanManageSudoers(status SudoStatus) bool

// RequiredSudoersContent returns the canonical sudoers template as a string.
// It is the same content shown in SECURITY.md.
func RequiredSudoersContent() string

// GetCurrentSudoersContent reads /etc/sudoers.d/zfsnas (via cat, no sudo needed if
// the file is world-readable; falls back to sudo cat if not).
// Returns ("", nil) when the file does not exist yet.
func GetCurrentSudoersContent() (string, error)

// ComputeSudoersDiff diffs current against required, marking entries in silenced
// as Silenced:true.
func ComputeSudoersDiff(current, required string, silenced []string) SudoersDiff

// ApplySudoers assembles the final file content from the required template plus
// silenced-extra lines, writes it via `sudo tee /etc/sudoers.d/zfsnas`, then
// validates with `visudo -c -f`.  On failure it rolls back and returns an error.
func ApplySudoers(required string, silencedMissing, silencedExtra []string) error

// sudoersExplanations maps normalised command strings to explanation text.
var sudoersExplanations = map[string]string{ /* ... see explanation map above ... */ }
```

### `handlers/sudoers_hardening.go`

```go
package handlers

// GET  /api/sudoers/status   → HandleSudoersStatus
//   Returns: { available, enabled, up_to_date, missing_count, extra_count }
//
// GET  /api/sudoers/diff     → HandleSudoersDiff
//   Returns: SudoersDiff (full detail including line text and explanations)
//
// POST /api/sudoers/enable   → HandleEnableSudoersHardening
//   Body: { "enabled": true | false }
//   Sets AppConfig.SudoersHardeningEnabled.  Does not touch the file.
//
// POST /api/sudoers/apply    → HandleApplySudoers
//   Body: { "silenced_missing": ["line1",...], "silenced_extra": ["line2",...] }
//   Assembles content, writes file, updates AppConfig.SudoersSilencedLines.
```

---

## Config additions — `internal/config/config.go`

```go
// Added to AppConfig:
SudoersHardeningEnabled bool     `json:"sudoers_hardening_enabled,omitempty"`
SudoersSilencedLines    []string `json:"sudoers_silenced_lines,omitempty"`
```

---

## Routes — `handlers/router.go`

```go
r.Handle("/api/sudoers/status", RequireAuth(RequireAdmin(http.HandlerFunc(HandleSudoersStatus)))).Methods("GET")
r.Handle("/api/sudoers/diff",   RequireAuth(RequireAdmin(http.HandlerFunc(HandleSudoersDiff)))).Methods("GET")
r.Handle("/api/sudoers/enable", RequireAuth(RequireAdmin(HandleEnableSudoersHardening(appCfg)))).Methods("POST")
r.Handle("/api/sudoers/apply",  RequireAuth(RequireAdmin(HandleApplySudoers(appCfg)))).Methods("POST")
```

---

## SECURITY.md additions

Add to the `ZFSNAS_APT` Cmnd_Alias (or a new `ZFSNAS_SECURITY` alias):

```sudoers
# ── Sudoers self-management (Sudoers Hardening feature) ───────────────────────
# since v6.3.31 — lets the portal overwrite its own sudoers file when the
#   Sudoers Hardening feature is enabled in the Prerequisites tab.
/usr/bin/tee /etc/sudoers.d/zfsnas
```

Add `ZFSNAS_SECURITY` to the grant line.

Add a note entry under Notes:
> **`tee /etc/sudoers.d/zfsnas`** — used by the Sudoers Hardening feature (v6.3.31+) to apply
> in-portal sudoers changes.  Only the portal's own drop-in file is writable; the main
> `/etc/sudoers` file is never touched.  If you do not use the Sudoers Hardening feature you can
> omit this entry.

---

## Prerequisites tab — UI additions (`static/index.html`)

### Security section layout (new)

The Prerequisites tab gains a **Security** section header between the existing "Optional Features"
section and any future sections.  It contains the Sudoers Hardening card.

```html
<div class="prereq-section-header">Security</div>
<div class="prereq-card" id="sudoers-hardening-card">
  <!-- rendered by renderSudoersHardeningCard() -->
</div>
```

`renderSudoersHardeningCard()` is called:
- on initial prerequisites page load
- after enable/disable toggle
- after apply completes

The card is hidden entirely when `sudoers_status.available == false`.

### JS functions

```js
async function loadSudoersStatus()          // GET /api/sudoers/status → renderSudoersHardeningCard()
async function enableSudoersHardening(on)   // POST /api/sudoers/enable
async function openSudoersReviewModal()     // GET /api/sudoers/diff → openModal('sudoers-review-modal')
async function applySudoersChanges()        // POST /api/sudoers/apply, close modal, reload card
function renderSudoersDiffRows(diff)        // build the two sections (missing / extra)
function toggleSudoersLineApprove(line, type)
function toggleSudoersLineSilence(line, type)
function updateApplyButtonLabel()           // counts approved (not silenced) lines
```

### Sudoers review modal

A full-width (max 760 px) modal with:
- Header: "Sudoers Review — /etc/sudoers.d/zfsnas" + change count
- Two collapsible sections: "Missing lines" (green `+` rows) and "Extra lines" (red `–` rows)
- A third section "Silenced" collapsed by default, listing items the user has already silenced
- Footer: `[Apply N Approved Changes]` (disabled when N=0) + `[Cancel]`

Each row:
```html
<div class="sudoers-diff-row sudoers-row-missing" data-line="...">
  <div class="sudoers-row-prefix">+</div>
  <div class="sudoers-row-content">
    <code class="sudoers-row-line">/usr/bin/tee /etc/sudoers.d/zfsnas</code>
    <p class="sudoers-row-explanation">Lets the portal write its own sudoers file…</p>
  </div>
  <div class="sudoers-row-actions">
    <button class="btn btn-xs btn-success" onclick="toggleSudoersLineApprove(...)">✓ Approve</button>
    <button class="btn btn-xs btn-ghost"   onclick="toggleSudoersLineSilence(...)">— Silence</button>
  </div>
</div>
```

---

## Audit entries

`audit.ActionUpdateSudoers` (new constant):
- On enable/disable: `"sudoers hardening: enabled"` / `"sudoers hardening: disabled"`
- On apply success: `"sudoers apply: N lines added, M lines removed, K silenced"`
- On apply failure: `"sudoers apply failed: <error>"`

---

## CSS additions (`static/style.css`)

```css
/* Sudoers diff rows */
.sudoers-diff-row           { display:flex; gap:12px; padding:10px 0; border-bottom:1px solid var(--border); }
.sudoers-row-prefix         { font-size:1.2rem; font-weight:700; width:18px; flex-shrink:0; }
.sudoers-row-missing .sudoers-row-prefix { color:var(--success); }
.sudoers-row-extra   .sudoers-row-prefix { color:var(--danger);  }
.sudoers-row-content        { flex:1; min-width:0; }
.sudoers-row-line           { display:block; font-size:.8rem; margin-bottom:4px; word-break:break-all; }
.sudoers-row-explanation    { margin:0; font-size:.78rem; color:var(--muted); line-height:1.4; }
.sudoers-row-actions        { display:flex; flex-direction:column; gap:6px; align-items:flex-end; }
.sudoers-row-approved       { background:color-mix(in srgb, var(--success) 8%, transparent); }
.sudoers-row-silenced       { opacity:.45; }
.sudoers-section-header     { font-size:.72rem; font-weight:700; letter-spacing:.08em;
                               text-transform:uppercase; color:var(--muted);
                               margin:18px 0 6px; }
```

---

## Files changed summary

| File | Change |
|---|---|
| `system/sudoers_hardening.go` | New — diff logic, apply, explanation map, access detection |
| `handlers/sudoers_hardening.go` | New — status/diff/enable/apply handlers |
| `handlers/router.go` | Register 4 new routes |
| `internal/config/config.go` | Add `SudoersHardeningEnabled`, `SudoersSilencedLines` |
| `internal/audit/audit.go` | Add `ActionUpdateSudoers` constant |
| `static/index.html` | Security section + Sudoers Hardening card + review modal + JS |
| `static/style.css` | Diff row styles |
| `SECURITY.md` | Add `tee /etc/sudoers.d/zfsnas` entry + notes |

---

## Version bump

`internal/version/version.go`: `"6.3.31"`

## Status: PLANNED
