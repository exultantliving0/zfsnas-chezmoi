# Plan — Version 6.3.21

## Overview

Three independent improvements:

1. **Pool Fixer Wizard — DEGRADED support** — the wizard now also shows for DEGRADED pools, with scenario-aware content (bring offline disk back online, replace with a spare, or explain and link to docs).
2. **SMB Global Configure button** — new "Configure" button in the SMB Shares tab replaces the scattered Samba settings from the Settings tab; also adds a home-folder dataset option.
3. **User Home Folder flag** — new checkbox on User create/edit; when set and SMB home dataset is configured, that user's home folder is created and pointed to the dataset.

---

## Feature 1 — Pool Fixer Wizard: DEGRADED scenarios

### Current behaviour

The wizard button only appears when `pool.health === 'SUSPENDED'`.  The wizard handles two sub-cases: all disks back online (just `zpool clear`) or some disks offline-but-present (clear + online).

### New behaviour

The button also appears when `pool.health === 'DEGRADED'`.  When the wizard opens for a DEGRADED pool it analyses member statuses and detected disks and picks one of three scenes.

#### Scene A — `degraded-online`: offline disk still physically present

Condition: at least one member has status `OFFLINE` and `MemberPresent[i] === true`.

Content:
- Amber warning block explaining the pool is DEGRADED because one or more disks are marked OFFLINE but are still physically connected.
- List of the affected disks.
- Offer to bring them back online (`zpool online`).
- Action button: "Bring Online".
- Backend: `POST /api/pool/disk/online` (existing `HandleDiskOnline`); iterate over each offline-but-present disk.  No `zpool clear` needed (the pool is not suspended).

#### Scene B — `degraded-replace`: offline/absent disk + free replacement disk available

Condition: at least one member has a non-ONLINE status AND `MemberPresent[i] === false` (disk is physically gone or dead), AND at least one free disk (`!in_use`, no system mounts) exists on the system.

Content:
- Red danger block explaining that a pool member is missing or unreadable.
- "Failed / missing disk" row showing the ZFS member path.
- "Select replacement disk" section: a radio list of all free disks with their size, model, serial.  A note: "The replacement disk must be at least as large as the failed disk.  ZFS will start a resilver automatically."
- If multiple replacement candidates, the user picks one.
- Action button: "Replace Disk".
- Backend: new `POST /api/pool/fixer/replace` → `HandlePoolFixerReplace`.

#### Scene C — `degraded-unknown`: neither A nor B

Condition: pool is DEGRADED but no actionable path was identified (e.g. RAIDZ with multiple faults, cache/log failures, etc.).

Content:
- Yellow info block with the raw `zpool status` output (pre-formatted).
- Explanation of why ZFS marks the pool degraded.
- Link to the ZFS pool health documentation: https://openzfs.github.io/openzfs-docs/man/master/8/zpool.8.html
- "Fix Now" button is hidden; only "Close" is shown.

#### Wizard button visibility change

```js
// Before
const showFixer = currentUser.role === 'admin' && pool.health === 'SUSPENDED' && ...;

// After
const showFixer = currentUser.role === 'admin' &&
  (pool.health === 'SUSPENDED' || pool.health === 'DEGRADED');
```

For DEGRADED pools the wizard always appears (the wizard itself figures out what to say).
For SUSPENDED pools the existing logic (scenes `clear` / `online`) is unchanged.

### Backend additions

#### `system/zfs.go` — new function

```go
// ReplacePoolDisk replaces a failed pool member with a new device.
// Runs: zpool replace <pool> <oldDev> <newDev>
// ZFS starts a resilver automatically.
func ReplacePoolDisk(poolName, oldDev, newDev string) error
```

#### `handlers/pools.go` — new handler

```go
// HandlePoolFixerReplace runs zpool replace for a degraded pool.
// POST /api/pool/fixer/replace  { pool, old_device, new_device }
func HandlePoolFixerReplace(w http.ResponseWriter, r *http.Request)
```

Logs to audit with `ActionUpdatePool`.  Returns updated pool JSON.

#### `handlers/router.go`

```
r.Handle("/api/pool/fixer/replace", RequireAuth(RequireAdmin(http.HandlerFunc(HandlePoolFixerReplace)))).Methods("POST")
```

### Frontend additions

- `openPoolFixerWizard()`:
  - New branch for `pool.health === 'DEGRADED'`.
  - Determines the scene by inspecting `member_statuses` and `member_present`.
  - For scene B: fetches `GET /api/disks` to get the free-disk list, then filters `!d.in_use`.
  - Renders appropriate HTML per scene.
- `submitPoolFixer()`:
  - New branch for `_fixerScene === 'degraded-online'`: calls `POST /api/pool/disk/online` once per offline disk.
  - New branch for `_fixerScene === 'degraded-replace'`: calls `POST /api/pool/fixer/replace`.
  - Scene C has no submit (button is hidden).
- `_fixerScene` gains new values: `'degraded-online'` | `'degraded-replace'` | `'degraded-unknown'`.

---

## Feature 2 — SMB Shares: Global Configure button

### Config changes — `internal/config/config.go`

Add to `AppConfig`:

```go
SMBHomeDataset string `json:"smb_home_dataset,omitempty"` // ZFS dataset path for user home folders; "" = disabled
```

`MaxSmbdProcesses` stays where it is (just moved in the UI).

### New backend routes

```
GET  /api/smb/global-config   → HandleGetSMBGlobalConfig
PUT  /api/smb/global-config   → HandleUpdateSMBGlobalConfig
```

Both live in `handlers/smb.go`.

#### `HandleGetSMBGlobalConfig`

Returns:
```json
{
  "max_smbd_processes": 100,
  "home_dataset": "tank/homes"
}
```

#### `HandleUpdateSMBGlobalConfig`

Accepts:
```json
{
  "max_smbd_processes": 200,
  "home_dataset": "tank/homes"
}
```

- Validates `max_smbd_processes` (1–10000).
- Validates `home_dataset`: if non-empty, checks the dataset exists (`zfs list -H <dataset>`).
- Saves to `AppConfig`.
- Calls `system.ApplySmbGlobal(maxSmbdProcesses, homeDataset)` and `system.ReloadSamba()` if Samba is installed.

Route registration in `handlers/router.go`.

### `system/smb.go` — update `ApplySmbGlobal`

Current signature: `ApplySmbGlobal(maxSmbdProcesses int) error`

New signature: `ApplySmbGlobal(maxSmbdProcesses int, homeDataset string) error`

Behaviour change: the managed block in `smb.conf` (between `BEGIN ZFSNAS MANAGED` / `END ZFSNAS MANAGED` markers) gains a `[homes]` section when `homeDataset` is non-empty.

The `[homes]` section:
```ini
[homes]
   comment = User Home Directories
   path = <mountpoint>/%U
   valid users = %U
   read only = no
   browseable = no
   create mask = 0700
   directory mask = 0700
```

Where `<mountpoint>` is obtained via `zfs get -H -o value mountpoint <homeDataset>`.

When `homeDataset` is empty the `[homes]` block is omitted from the managed section (effectively removing it from smb.conf).

All existing callers of `ApplySmbGlobal` pass the new `homeDataset` arg (loaded from `AppConfig`).

### Settings tab — remove Samba sub-section

The "Max smbd Processes" field and its save button are removed from the Settings tab UI.  `GET /api/settings` and `PUT /api/settings` still accept/return `max_smbd_processes` for backwards compatibility (no removal of the backend field), but the UI input and save button for it are deleted from the Settings tab HTML.

### SMB Shares tab — new "Configure" button

Top-right area of the SMB Shares tab header row, next to the existing service control buttons:

```html
<button class="btn btn-sm btn-ghost" onclick="openSMBConfigureModal()">⚙ Configure</button>
```

#### Configure modal

```
Title: SMB Global Settings
Fields:
  - Max smbd Processes  [number input, 1-10000]   (moved from Settings)
  - Home Folder Dataset [select dropdown]
      Options: "Disabled" (value ""), then one option per ZFS dataset from allDatasetsFlat
      (reuse the existing dataset list already loaded in memory)
Save button: "Save"
Cancel button
```

On open: `GET /api/smb/global-config` to populate fields.
On save: `PUT /api/smb/global-config`.

#### JS functions added

- `openSMBConfigureModal()` — fetches config, populates modal, shows it.
- `saveSMBGlobalConfig()` — PUT, handles errors, closes modal.
- `closeSMBConfigureModal()` — hides modal.

#### New modal HTML (added near other SMB modals)

`id="modal-smb-configure"` — standard modal structure matching existing modals.

---

## Feature 3 — User Home Folder flag

### Config changes — `internal/config/config.go`

Add to `User` struct:

```go
SMBHomeFolder bool `json:"smb_home_folder,omitempty"` // true = create home dir under SMBHomeDataset
```

### Backend — `handlers/users.go`

When creating or updating a user where `SMBHomeFolder == true`:
1. Load `AppConfig`; if `SMBHomeDataset == ""` ignore silently (no dataset configured yet).
2. If dataset is set: call `system.EnsureSMBHomeDir(appCfg.SMBHomeDataset, user.Username)`.

When `SMBHomeFolder` is changed from `true` to `false` (edit): do nothing (don't delete the directory).

### `system/smb.go` — new function

```go
// EnsureSMBHomeDir creates <mountpoint>/<username>/ if it doesn't already exist
// and sets ownership to the Linux user (which must already exist via EnsureSambaUser).
func EnsureSMBHomeDir(dataset, username string) error {
    // 1. zfs get -H -o value mountpoint <dataset>  → mountpoint
    // 2. os.MkdirAll(filepath.Join(mountpoint, username), 0700)
    // 3. exec.Command("sudo", "chown", username+":"+username, dir)
}
```

This function is safe to call even if the directory already exists.

### Frontend — Users tab

In both the "Create User" and "Edit User" modals, add a checkbox row after the role selector:

```html
<label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-size:14px;margin-top:8px;">
  <input type="checkbox" id="user-smb-home" style="width:15px;height:15px;accent-color:var(--accent-2);">
  Home User Folder
  <i class="info-tip" data-tip="When enabled and a home-folder dataset is configured in SMB Settings, this user's personal folder will be created inside that dataset and served as their SMB home share."></i>
</label>
```

Visibility: show the checkbox for all roles (admin and smb-only users both benefit; read-only users can also have a home folder).

On "Save" (create or edit): include `smb_home_folder: checkbox.checked` in the JSON body.

On "Edit" open: populate the checkbox from `user.smb_home_folder`.

---

## Files changed

| File | Change |
|---|---|
| `system/zfs.go` | Add `ReplacePoolDisk()` |
| `system/smb.go` | Update `ApplySmbGlobal()` signature; add `EnsureSMBHomeDir()` |
| `handlers/pools.go` | Add `HandlePoolFixerReplace()` |
| `handlers/smb.go` | Add `HandleGetSMBGlobalConfig()`, `HandleUpdateSMBGlobalConfig()` |
| `handlers/settings.go` | Pass `homeDataset` to `ApplySmbGlobal` call |
| `handlers/users.go` | Call `EnsureSMBHomeDir` when `SMBHomeFolder` is true |
| `handlers/router.go` | Register `/api/pool/fixer/replace`, `/api/smb/global-config` |
| `internal/config/config.go` | Add `AppConfig.SMBHomeDataset`, `User.SMBHomeFolder` |
| `static/index.html` | Wizard DEGRADED scenes; SMB Configure modal + button; User modal checkbox; remove Samba sub-section from Settings tab |

---

## Version bump

`internal/version/version.go`: `"6.3.21"`

## Status: COMPLETE
