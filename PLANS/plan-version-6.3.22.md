# Plan — Version 6.3.22

## Overview

Four independent improvements:

1. **UPS Management** — Optional feature (NUT daemon) installable from the Prerequisites tab. Adds a compact battery indicator in the top bar; clicking it opens a UPS Settings right panel with a rich visual status display and configurable shutdown behaviour.
2. **Cache Config Dropdown** — The single "Cache Config" button on the Pool tab becomes a split dropdown. The existing L2ARC/ZIL panel moves into a "Disk Cache" sub-item; a new "ARC Level 1 (Memory)" sub-item opens a dedicated ARC stats + tuning popup.
3. **SMB Valid Users — Read/Write Toggle** — In the SMB Create/Edit Share modals, each entry in the Valid Users table gains a per-user Read-Only / Read-Write toggle that maps to Samba's `read list` / `write list` directives.
4. **Certificate Management** — New "Certificate Management" card in the Settings tab; lets users upload, validate, export, delete, and activate TLS certificate pairs (`.crt` + `.key`). The self-generated certificate is listed as a built-in entry. After activating a new certificate a one-click restart button is offered.

---

## Feature 1 — UPS Management

### Prerequisites tab

In the "Optional Features" section, add a new card:

```
Title:       UPS Management
Description: Monitor and manage an attached UPS battery via NUT (Network UPS Tools).
             Enables battery status in the top bar and configurable shutdown rules.
Packages:    nut  nut-client
Button:      "Enable UPS Feature"  (becomes green check + "Installed" once done)
```

Installation calls `POST /api/ups/install` (same async pattern as MinIO install: job_id polling via activity bar).

`UPSPrereqsInstalled()` checks for `/usr/sbin/upsd` and `upsc` on PATH.

### Config additions — `internal/config/config.go`

```go
type UPSShutdownPolicy struct {
    Enabled          bool   `json:"enabled"`
    // Trigger type: "time" | "percent" | "both" (whichever fires first)
    TriggerType      string `json:"trigger_type"`
    // Shut down when battery runtime drops below this many seconds (0 = disabled)
    RuntimeThreshold int    `json:"runtime_threshold"`
    // Shut down when battery charge drops below this percentage (0 = disabled)
    PercentThreshold int    `json:"percent_threshold"`
    // Shell command run before shutdown (optional, e.g. snapshot script)
    PreShutdownCmd   string `json:"pre_shutdown_cmd,omitempty"`
}

type UPSConfig struct {
    Enabled        bool              `json:"enabled"`
    UPSName        string            `json:"ups_name"`        // NUT ups.conf identifier, e.g. "myups"
    Driver         string            `json:"driver"`          // e.g. "usbhid-ups"
    Port           string            `json:"port"`            // e.g. "auto"
    ShutdownPolicy UPSShutdownPolicy `json:"shutdown_policy"`
}
```

Add to `AppConfig`:

```go
UPS UPSConfig `json:"ups,omitempty"`
```

### `system/ups.go` — new file

```go
// UPSPrereqsInstalled returns true when nut packages are present.
func UPSPrereqsInstalled() bool

// InstallUPS runs apt-get install -y nut nut-client and returns a streaming log channel.
func InstallUPS(logCh chan<- string) error

// UPSStatus is returned by QueryUPS.
type UPSStatus struct {
    Name           string   `json:"name"`
    // ups.status raw string, e.g. "OL", "OB", "OB LB"
    RawStatus      string   `json:"raw_status"`
    // Derived booleans
    OnLine         bool     `json:"on_line"`
    OnBattery      bool     `json:"on_battery"`
    LowBattery     bool     `json:"low_battery"`
    ChargePct      *float64 `json:"charge_pct"`       // battery.charge
    RuntimeSecs    *int     `json:"runtime_secs"`     // battery.runtime
    BattVoltage    *float64 `json:"batt_voltage"`     // battery.voltage
    InputVoltage   *float64 `json:"input_voltage"`    // input.voltage
    OutputVoltage  *float64 `json:"output_voltage"`   // output.voltage
    LoadPct        *float64 `json:"load_pct"`         // ups.load
    TempC          *float64 `json:"temp_c"`           // ups.temperature
    Model          string   `json:"model"`            // ups.model
    Manufacturer   string   `json:"manufacturer"`     // ups.mfr
    Serial         string   `json:"serial"`           // ups.serial
    // Full key=value map for display of all variables
    AllVars        map[string]string `json:"all_vars"`
}

// QueryUPS runs `upsc <name>` and parses the output into UPSStatus.
func QueryUPS(name string) (*UPSStatus, error)

// ApplyUPSConfig writes /etc/nut/ups.conf, upsd.conf, upsd.users, upsmon.conf
// based on cfg, then restarts nut-server + nut-client.
func ApplyUPSConfig(cfg UPSConfig) error

// UPSServiceAction runs systemctl start|stop|restart nut-server.
func UPSServiceAction(action string) error
```

NUT config files written by `ApplyUPSConfig`:

**/etc/nut/nut.conf**
```
MODE=standalone
```

**/etc/nut/ups.conf**
```
[<name>]
  driver = <driver>
  port   = <port>
```

**/etc/nut/upsd.users**
```
[upsmon]
  password  = zfsnas_monitor
  upsmon master
```

**/etc/nut/upsmon.conf**  (managed block between `# BEGIN ZFSNAS` / `# END ZFSNAS` markers):
```
MONITOR <name>@localhost 1 upsmon zfsnas_monitor master
SHUTDOWNCMD "/sbin/shutdown -h +0"
MINSUPPLIES 1
POLLFREQ 5
POLLFREQALERT 5
HOSTSYNC 15
DEADTIME 15
POWERDOWNFLAG /etc/killpower
NOTIFYFLAG ONLINE  SYSLOG+WALL
NOTIFYFLAG ONBATT  SYSLOG+WALL
NOTIFYFLAG LOWBATT SYSLOG+WALL
```

Shutdown trigger logic is implemented as a background goroutine in `main.go` (`StartUPSShutdownWatcher`): polls `QueryUPS` every 10 s; when the configured threshold is breached, runs optional `PreShutdownCmd` then `/sbin/shutdown -h +0`.

### `handlers/ups.go` — new file

```go
// GET  /api/ups/status            → HandleGetUPSStatus
// GET  /api/ups/config            → HandleGetUPSConfig
// PUT  /api/ups/config            → HandleUpdateUPSConfig
// POST /api/ups/install           → HandleInstallUPS  (streams log via job_id)
// POST /api/ups/service           → HandleUPSService  { action: start|stop|restart }
```

`HandleUpdateUPSConfig`:
- Validates `TriggerType` ∈ `{"time","percent","both"}`.
- Validates thresholds are in range (0–100 for %, 0–3600 for time).
- Calls `system.ApplyUPSConfig(cfg)` then saves to `AppConfig`.
- Logs to audit.

### `handlers/router.go`

```
r.Handle("/api/ups/status",  RequireAuth(...HandleGetUPSStatus)).Methods("GET")
r.Handle("/api/ups/config",  RequireAuth(...HandleGetUPSConfig)).Methods("GET")
r.Handle("/api/ups/config",  RequireAuth(RequireAdmin(...HandleUpdateUPSConfig))).Methods("PUT")
r.Handle("/api/ups/install", RequireAuth(RequireAdmin(...HandleInstallUPS))).Methods("POST")
r.Handle("/api/ups/service", RequireAuth(RequireAdmin(...HandleUPSService))).Methods("POST")
```

### `main.go`

```go
if cfg.UPS.Enabled && system.UPSPrereqsInstalled() {
    go StartUPSShutdownWatcher(cfg)
}
```

### Top bar indicator

Inserted in the top bar HTML, immediately left of the theme selector:

```html
<div id="ups-topbar" class="ups-topbar-widget" onclick="openUPSPanel()" title="UPS Status" style="display:none">
  <span id="ups-icon" class="ups-icon">🔋</span>
  <span id="ups-pct">--</span>
  <span id="ups-state" class="ups-state-badge"></span>
</div>
```

CSS classes:
- `.ups-state-badge` — small pill badge; `--ups-online`: green "AC"; `--ups-battery`: amber "BATT"; `--ups-low`: red "LOW"
- The widget is hidden when UPS feature is not enabled.

Top bar poller (every 15 s, alongside existing pollers):
```js
async function pollUPSTopbar() {
    const s = await apiFetch('/api/ups/status');
    // populate #ups-pct, #ups-icon, #ups-state-badge
    // show/hide #ups-topbar
}
```

The battery icon changes character based on charge:
- ≥ 80 %: `🔋` (full)
- 40–79 %: `🔋` (mid, CSS filter)
- 10–39 %: amber colour
- < 10 %: red colour + blinking

### UPS Settings right panel

Opens as a right-side panel (same pattern as other detail panels in the project).

Layout (top to bottom):

#### Battery Visual

A large SVG or CSS-drawn battery outline (horizontal, full-width of the panel):
- Fill bar animated to current `charge_pct`; colour transitions: green → amber → red at 40 % / 15 %
- Overlaid text: large `charge_pct`% centred, smaller "XX min remaining" below
- Status badge below the battery: "AC POWER" (green) / "ON BATTERY" (amber) / "LOW BATTERY" (red)

#### Metrics Grid

Two-column card grid showing all available UPS variables. Priority cards always shown first:

| Card | Source key | Info tip |
|---|---|---|
| Input Voltage | `input.voltage` | "AC voltage currently supplied by the wall outlet." |
| Output Voltage | `output.voltage` | "Voltage the UPS is delivering to connected equipment." |
| Load | `ups.load` | "Percentage of UPS power capacity currently in use." |
| Battery Voltage | `battery.voltage` | "Current battery cell voltage." |
| Temperature | `ups.temperature` | "Internal UPS temperature (if reported by device)." |
| Model | `ups.model` | "UPS model as reported by the device." |
| Manufacturer | `ups.mfr` | "Manufacturer of the UPS." |
| Serial | `ups.serial` | "Unique device serial number." |

Below the priority cards: a collapsible "All Variables" section listing every key/value from `AllVars` in a small monospace table (for power users / debugging).

#### Shutdown Behaviour Configuration

Form below the metrics grid:

```
Title: Shutdown Behaviour
[toggle] Enable automatic shutdown

Trigger: ( ) Shutdown when runtime < [___] minutes remaining
         ( ) Shutdown when battery < [___] % remaining
         (•) Either condition — whichever fires first

Pre-shutdown command (optional):
[___________________________________________]
Info tip: "Shell command executed before shutdown begins. Use this to trigger
           snapshots, unmount shares, etc. Max execution time: 30 seconds."

[Save Shutdown Policy]
```

Validation: at least one threshold must be non-zero when enabled; time 1–60 min; percent 5–50 %.

#### Service Control strip (bottom of panel)

`[Start]  [Stop]  [Restart]` buttons → `POST /api/ups/service`.

---

## Feature 2 — Cache Config Dropdown

### UI change — Pool tab

The single `<button>` labelled "Cache Config" becomes a button-group with a dropdown arrow:

```html
<div class="btn-group">
  <button class="btn btn-sm btn-ghost dropdown-toggle"
          onclick="toggleCacheDropdown(event)">Cache Config ▾</button>
  <div id="cache-dropdown" class="dropdown-menu" style="display:none">
    <button onclick="openDiskCachePanel(); closeCacheDropdown()">Disk Cache</button>
    <button onclick="openARCPanel();      closeCacheDropdown()">ARC Level 1 (Memory)</button>
  </div>
</div>
```

`openDiskCachePanel()` is the existing function that opens the current L2ARC/ZIL right panel (renamed from the current click handler — no behaviour change).

### ARC Level 1 popup — backend

#### `GET /api/pool/arc` → `HandleGetARC`

Reads `/proc/spl/kstat/zfs/arcstats` and `/sys/module/zfs/parameters/` and returns:

```json
{
  "arc_size":          4294967296,
  "arc_min":           536870912,
  "arc_max":           8589934592,
  "arc_target":        4294967296,
  "arc_meta_limit":    2147483648,
  "arc_meta_used":     1073741824,
  "arc_meta_max":      2147483648,
  "hits":              9823741,
  "misses":            412839,
  "hit_ratio":         95.97,
  "mfu_size":          2684354560,
  "mru_size":          1610612736,
  "demand_data_hits":  7234123,
  "demand_meta_hits":  2589618,
  "prefetch_data_hits":0,
  "evicted_mfu":       0,
  "evicted_mru":       1048576,
  "l2_hits":           0,
  "l2_misses":         0,
  "l2_size":           0,
  // Tunable parameters (from /sys/module/zfs/parameters/)
  "param_arc_max":     8589934592,
  "param_arc_min":     536870912,
  "param_arc_meta_limit_percent": 75
}
```

Live read; no caching.

#### `PUT /api/pool/arc` → `HandleSetARC`

Accepts:

```json
{
  "arc_max":                   8589934592,
  "arc_min":                   536870912,
  "arc_meta_limit_percent":    75
}
```

Writes new values to:
- `/sys/module/zfs/parameters/zfs_arc_max`
- `/sys/module/zfs/parameters/zfs_arc_min`
- `/sys/module/zfs/parameters/zfs_arc_meta_limit_percent` (if kernel supports it)

Also writes a `/etc/modprobe.d/zfs.conf` block (between `# BEGIN ZFSNAS` / `# END ZFSNAS` markers) so the values persist across reboots:

```
options zfs zfs_arc_max=<value>
options zfs zfs_arc_min=<value>
```

Validates: `arc_min < arc_max`; `arc_max ≤ total_ram * 0.75`; `arc_meta_limit_percent` 10–90.

Logs to audit with `ActionUpdatePool` ("ARC parameters updated").

New functions in `system/zfs.go` (or a new `system/arc.go`):
```go
func GetARCStats() (*ARCStats, error)
func SetARCParams(max, min int64, metaLimitPct int) error
```

Routes registered in `handlers/router.go`:
```
r.Handle("/api/pool/arc", RequireAuth(...HandleGetARC)).Methods("GET")
r.Handle("/api/pool/arc", RequireAuth(RequireAdmin(...HandleSetARC))).Methods("PUT")
```

### ARC Level 1 popup — frontend

A modal popup (not a right panel — this is a focused configuration dialog).

#### Layout

```
┌─────────────────────────────────────────────────────────────┐
│  ARC Level 1 Cache (Memory)                           [×]   │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │ ⚠  Advanced Setting — Modify with care                  │ │
│  │  ARC is ZFS's adaptive read cache in RAM. Incorrect     │ │
│  │  tuning can reduce performance or cause OOM conditions.  │ │
│  │  These changes take effect immediately and persist       │ │
│  │  across reboots via /etc/modprobe.d/zfs.conf.           │ │
│  └─────────────────────────────────────────────────────────┘ │
│                                                               │
│  CURRENT STATE                                               │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐       │
│  │ARC Size  │ │ Hit Ratio│ │ ARC Max  │ │ ARC Min  │       │
│  │  4.0 GB  │ │  95.97 % │ │  8.0 GB  │ │  512 MB  │       │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘       │
│                                                               │
│  BREAKDOWN                                   [i]             │
│  ┌───────────────────────────────────────────────────┐       │
│  │ MFU (Most Freq. Used)  ████████░░░░░   2.5 GB    │       │
│  │ MRU (Most Rec. Used)   █████░░░░░░░░   1.5 GB    │       │
│  │ Metadata               ███░░░░░░░░░░   1.0 GB    │       │
│  └───────────────────────────────────────────────────┘       │
│                                                               │
│  STATISTICS                                                   │
│  Hits: 9,823,741    Misses: 412,839    Evictions (MFU): 0   │
│  Evictions (MRU): 1 MB    Demand Data Hits: 7,234,123       │
│                                                               │
│  TUNABLE PARAMETERS                                           │
│  ARC Maximum [i]  [    8192  ] MB   (current kernel: 8.0 GB) │
│  ARC Minimum [i]  [     512  ] MB   (current kernel: 512 MB) │
│  Metadata Limit % [i]  [ 75 ] %                              │
│                                                               │
│  [  Apply Changes  ]                          [ Close ]       │
└─────────────────────────────────────────────────────────────┘
```

Info tips:
- ARC Maximum: "Hard ceiling for ARC RAM usage. Reducing this frees RAM for applications but may increase disk I/O. Default: ~50% of total RAM."
- ARC Minimum: "ARC will not shrink below this size under memory pressure. Setting too high can starve other processes."
- Metadata Limit %: "Maximum percentage of the ARC that can be used for metadata (directory entries, inode info). Reduce if you see metadata pressure evicting data."
- Hit Ratio: "Percentage of read requests served from the ARC cache. Values above 90% indicate a well-sized cache."
- MFU: "Most Frequently Used segment — data accessed many times, prioritised for retention."
- MRU: "Most Recently Used segment — data accessed once recently; may be evicted first under pressure."

On open: `GET /api/pool/arc` to populate.
On "Apply Changes": `PUT /api/pool/arc` → success toast; refresh stats.

---

## Feature 3 — SMB Valid Users Read/Write Toggle

### Config changes — `internal/config/config.go`

Replace the current `[]string` valid-users field with a struct:

```go
type SMBUserAccess struct {
    Username string `json:"username"`
    ReadOnly bool   `json:"read_only"` // false = read-write (default)
}
```

In `SMBShare`:
```go
// Before:
ValidUsers []string `json:"valid_users,omitempty"`

// After:
ValidUsers []SMBUserAccess `json:"valid_users,omitempty"`
```

**Migration**: when loading config, if a `valid_users` entry is a plain string (old format), coerce it to `SMBUserAccess{Username: s, ReadOnly: false}`. This is handled in the JSON unmarshal step with a custom `UnmarshalJSON` on `SMBShare` or in the config load function.

### `system/smb.go` — update share stanza writer

The managed share block gains two new directives derived from `ValidUsers`:

```ini
[sharename]
   ...
   valid users = alice bob carol
   read list   = alice
   write list  = bob carol
```

Rules:
- `valid users` = all usernames in `ValidUsers`.
- `read list` = usernames where `ReadOnly == true`.
- `write list` = usernames where `ReadOnly == false`.
- If `read list` is empty, omit the line.
- If `write list` is empty, omit the line.

### Frontend — Create Share / Edit Share modals

**Valid Users table** gains a new column header "Access" and a toggle per row:

```html
<table id="share-valid-users-table">
  <thead>
    <tr>
      <th>Username</th>
      <th>Access</th>
      <th></th>  <!-- remove button -->
    </tr>
  </thead>
  <tbody>
    <!-- per user row: -->
    <tr data-username="alice">
      <td>alice</td>
      <td>
        <label class="access-toggle">
          <input type="checkbox" class="user-readonly-toggle" checked>
          <span class="toggle-label">Read-Only</span>
          <!-- unchecked = "Read-Write" -->
        </label>
      </td>
      <td><button onclick="removeShareUser('alice')">×</button></td>
    </tr>
  </tbody>
</table>
```

The toggle shows "Read-Only" (checked) or "Read-Write" (unchecked).

When adding a user from the dropdown, the default is Read-Write (unchecked).

On save, `valid_users` is serialised as:
```js
[...tbody rows].map(tr => ({
    username: tr.dataset.username,
    read_only: tr.querySelector('.user-readonly-toggle').checked
}))
```

On edit open, populate from `share.valid_users` array (set `checked` per `read_only` value).

---

## Feature 4 — Certificate Management

### Certificate storage

All certificates live in `config/certs/`. The auto-generated self-signed certificate is `config/certs/self-signed.crt` + `config/certs/self-signed.key`.

`AppConfig` gains:

```go
ActiveCertName string `json:"active_cert_name,omitempty"` // filename stem, e.g. "self-signed" or "mycompany"
```

When empty, the server falls back to `self-signed` (current behaviour).

### `internal/certgen/` additions

```go
// ListCerts returns all certificate pairs found in the certs directory.
func ListCerts(certsDir string) ([]CertInfo, error)

type CertInfo struct {
    Name        string    `json:"name"`          // filename stem
    CommonName  string    `json:"common_name"`
    SANs        []string  `json:"sans"`
    Issuer      string    `json:"issuer"`
    NotBefore   time.Time `json:"not_before"`
    NotAfter    time.Time `json:"not_after"`
    IsExpired   bool      `json:"is_expired"`
    IsValid     bool      `json:"is_valid"`       // cert+key pair match
    IsSelfSigned bool     `json:"is_self_signed"`
    IsActive    bool      `json:"is_active"`
    // true when both .crt and .key files exist
    HasKeyFile  bool      `json:"has_key_file"`
}

// ValidateCertPair checks that the public key in the cert matches the private key.
func ValidateCertPair(certPath, keyPath string) (bool, error)

// ImportCert writes certBytes to <name>.crt and keyBytes to <name>.key in certsDir.
// Returns an error if the pair does not validate.
func ImportCert(certsDir, name string, certBytes, keyBytes []byte) error
```

### `handlers/certs.go` — new file

```go
// GET  /api/certs             → HandleListCerts     (list all cert info)
// POST /api/certs/upload      → HandleUploadCert    (multipart: name, cert_file, key_file)
// DELETE /api/certs/{name}    → HandleDeleteCert    (cannot delete active cert)
// GET  /api/certs/{name}/export → HandleExportCert  (returns zip of .crt + .key)
// POST /api/certs/{name}/activate → HandleActivateCert (sets AppConfig.ActiveCertName)
// POST /api/certs/restart     → HandleCertRestart   (graceful TLS restart)
```

#### `HandleUploadCert`

- Accepts multipart form: `name` (alphanumeric + dashes, max 40 chars), `cert_file`, `key_file`.
- Validates name does not conflict with `self-signed`.
- Calls `certgen.ImportCert(...)` which validates the pair before writing.
- Returns the new `CertInfo`.

#### `HandleDeleteCert`

- Rejects if `name == cfg.ActiveCertName`.
- Removes `<name>.crt` and `<name>.key` from `config/certs/`.

#### `HandleActivateCert`

- Validates cert is present and `IsValid == true`.
- Sets `AppConfig.ActiveCertName = name`, saves config.
- Sets a flag `pendingCertRestart = true` (frontend polls this via `GET /api/certs`).

#### `HandleCertRestart`

- Calls `gracefulTLSRestart()` in `main.go`: closes the current TLS listener, reloads cert files, reopens listener.
- This approach avoids a full process restart for cert changes.

If a graceful restart is not feasible (edge case), fall back to `os.Exit(0)` (systemd will restart the service).

#### `HandleExportCert`

- Returns a `application/zip` archive containing `<name>.crt` and `<name>.key`.

### Routes — `handlers/router.go`

```
r.Handle("/api/certs",                RequireAuth(RequireAdmin(...HandleListCerts))).Methods("GET")
r.Handle("/api/certs/upload",         RequireAuth(RequireAdmin(...HandleUploadCert))).Methods("POST")
r.Handle("/api/certs/restart",        RequireAuth(RequireAdmin(...HandleCertRestart))).Methods("POST")
r.Handle("/api/certs/{name}/export",  RequireAuth(RequireAdmin(...HandleExportCert))).Methods("GET")
r.Handle("/api/certs/{name}/activate",RequireAuth(RequireAdmin(...HandleActivateCert))).Methods("POST")
r.Handle("/api/certs/{name}",         RequireAuth(RequireAdmin(...HandleDeleteCert))).Methods("DELETE")
```

### Settings tab — Certificate Management card

New card in the Settings tab, after existing cards:

```
┌──────────────────────────────────────────────────────────────┐
│  Certificate Management                                       │
│                                                               │
│  [+ Upload Certificate]                                       │
│                                                               │
│  ┌──────────────┬──────────────┬─────────────┬──────┬──────┐ │
│  │ Name         │ Common Name  │ Expires     │Valid?│      │ │
│  ├──────────────┼──────────────┼─────────────┼──────┼──────┤ │
│  │ self-signed  │ zfsnas       │ 2027-03-10  │ ✓    │  ≡   │ │
│  │ (active)     │              │             │      │      │ │
│  │ mycompany    │ nas.corp.com │ 2026-01-01  │ ✓    │  ≡   │ │
│  │ badcert      │ test         │ 2024-06-01  │ ✗    │  ≡   │ │
│  └──────────────┴──────────────┴─────────────┴──────┴──────┘ │
│                                                               │
│  ┌────────────────────────────────────────┐                  │
│  │ ⚠ Certificate change pending restart   │ [Restart Now]    │
│  └────────────────────────────────────────┘                  │
│  (shown only after activating a new cert)                     │
└──────────────────────────────────────────────────────────────┘
```

- Active cert row has a subtle green left border + "(active)" tag.
- Expired certs show expiry date in red.
- Invalid pair (cert/key mismatch) shows "✗" in red; "Use for ZFSNAS" action is disabled/greyed.

#### Burger menu per row (≡)

```
Use for ZFSNAS    ← disabled if IsValid == false or IsActive == true
─────────────
Export
─────────────
Delete            ← disabled if IsActive == true
```

#### Upload Certificate modal

```
Title: Upload Certificate
Fields:
  Name:        [___________]  (alphanumeric, dashes only)
  Certificate: [Choose .crt file]
  Private Key: [Choose .key file]
Button: [Upload & Validate]
```

On success: closes modal, refreshes cert list, shows toast "Certificate imported successfully — pair is valid."

On validation failure: shows inline error "Certificate and key do not match. Please check your files."

#### Restart banner

After `POST /api/certs/{name}/activate` succeeds:
- Show a yellow banner in the Certificate Management card: "Certificate changed — a restart is required to apply the new certificate."
- `[Restart Now]` button calls `POST /api/certs/restart`.
- After restart the banner is cleared (connection will briefly drop and reconnect).

#### JS functions

- `loadCertList()` — `GET /api/certs`, render table.
- `openUploadCertModal()` / `closeUploadCertModal()`
- `uploadCert()` — build FormData, POST, handle errors inline.
- `activateCert(name)` — POST activate, set pending banner, refresh list.
- `exportCert(name)` — triggers download via anchor with `href=/api/certs/<name>/export`.
- `deleteCert(name)` — confirm dialog, DELETE, refresh.
- `restartForCert()` — POST restart, show "Restarting…" then reconnect poll.

---

## Files changed

| File | Change |
|---|---|
| `system/ups.go` | New — UPS prereq check, install, query, apply config, service control |
| `system/arc.go` | New — `GetARCStats()`, `SetARCParams()` |
| `system/smb.go` | Update share stanza writer for `SMBUserAccess` read/write lists |
| `system/zfs.go` | (no change for this release) |
| `handlers/ups.go` | New — UPS API handlers |
| `handlers/certs.go` | New — Certificate management handlers |
| `handlers/pools.go` | Add `HandleGetARC`, `HandleSetARC` |
| `handlers/router.go` | Register all new routes |
| `internal/config/config.go` | Add `UPSConfig`, `UPSShutdownPolicy`, `ActiveCertName`; change `SMBShare.ValidUsers` to `[]SMBUserAccess` |
| `internal/certgen/certgen.go` | Add `ListCerts`, `ValidateCertPair`, `ImportCert` |
| `main.go` | Start `StartUPSShutdownWatcher`; add `gracefulTLSRestart()` |
| `static/index.html` | Top bar UPS widget; UPS Settings panel; ARC modal; Cache Config dropdown; SMB Valid Users toggle; Certificate Management card + modals |
| `static/style.css` | Battery widget styles; ARC breakdown bars; toggle pill styles; cert table styles |

---

## Version bump

`internal/version/version.go`: `"6.3.22"`

## Status: PLANNED
