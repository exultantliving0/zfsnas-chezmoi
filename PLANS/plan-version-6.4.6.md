# Plan — Version 6.4.6

## Overview

Five independent features:

1. **SMB Shadow Copy** — Add a `Shadow Copy` toggle to the SMB Share create/edit modal. When enabled, exposes ZFS snapshots of the share's backing dataset to SMB clients as VSS-compatible Previous Versions via Samba's `vfs_shadow_copy2` module.

2. **Physical Disk Power Management** — Add a "Power Management" button in the Physical Disks page header. Opens a modal to configure hdparm settings (APM level, spindown timeout, write cache, acoustic management) globally for all physical disks. Settings persist across reboots via `/etc/hdparm.conf`.

3. **System/Platform Power Management** — Add a "Power Management" button in the Platform (Updates) page header alongside the existing Power menu. Opens a dedicated modal for CPU governor, power profile, USB autosuspend, and PCIe ASPM policy. Settings persist across reboots. The UI warns the user this is a server system designed to run 24/7.

4. **Network UPS (Remote NUT Server)** — In the UPS panel's Advanced collapsed section, add a new collapsed sub-section "Network UPS" for connecting to a remote NUT server (upsc-compatible). Includes a "Test Connection" button. When configured, the poller and `ApplyUPSMonConfig` use the remote syntax.

---

## Feature 1 — SMB Shadow Copy

### Overview

Samba's `vfs_shadow_copy2` VFS module maps ZFS `.zfs/snapshot` entries to Windows VSS "Previous Versions". When the share's dataset has ZFS snapshots, Windows clients see them as restore points accessible via "Restore previous versions" in Explorer.

The `shadow:format` directive tells Samba which snapshot naming pattern to look for. Snapshots must be named in `XXXXX-YYYY.MM.DD-HH.MM.SS` format (UTC) for VSS to recognise them. The user is informed of this requirement in the UI.

### Config changes — `system/smb.go`

Add a new field to `SMBShare`:

```go
// ShadowCopy enables VSS Previous Versions via vfs_shadow_copy2.
// Requires ZFS snapshots named XXXXX-YYYY.MM.DD-HH.MM.SS on the backing dataset.
ShadowCopy bool `json:"shadow_copy"`
```

The full updated `SMBShare` struct gains one field after `WindowsACL`:

```go
WindowsACL bool `json:"windows_acl"`
ShadowCopy bool `json:"shadow_copy"`
```

### `system/smb.go` — `applySMBConf` update

In the VFS objects assembly section, `shadow_copy2` must appear first (before `catia`, `recycle`, `fruit`). Reorder the assembly:

```go
var vfsObjs []string
if s.ShadowCopy {
    vfsObjs = append(vfsObjs, "shadow_copy2")
}
if s.AppleEncoding {
    vfsObjs = append(vfsObjs, "catia")
}
if s.RecycleBin {
    vfsObjs = append(vfsObjs, "recycle")
}
if s.TimeMachine || s.WindowsACL {
    vfsObjs = append(vfsObjs, "fruit", "streams_xattr")
}
```

After the `vfs objects` line, write shadow copy parameters when enabled:

```go
if s.ShadowCopy {
    sb.WriteString("   shadow:snapdir = .zfs/snapshot\n")
    sb.WriteString("   shadow:sort = desc\n")
    sb.WriteString("   shadow:format = XXXXX-%Y.%m.%d-%H.%M.%S\n")
    sb.WriteString("   shadow:localtime = no\n")
}
```

`shadow:localtime = no` — snapshot timestamps are UTC (standard for GMT-format names).
`shadow:snapdir = .zfs/snapshot` — ZFS built-in, always available when `.zfs` directory is visible.

### No backend handler changes needed

`SMBShare` is decoded by `HandleCreateShare` and `HandleUpdateShare` via straight `json.NewDecoder(r.Body).Decode(&req)`. The new `shadow_copy` boolean field is populated automatically from the JSON body.

### Frontend — `static/index.html`

#### Create Share / Edit Share modal

Add a new toggle after the `WindowsACL` toggle:

```html
<div class="form-group" style="display:flex;align-items:center;justify-content:space-between;">
  <label style="font-size:13px;font-weight:500;">
    Shadow Copy (Previous Versions)
    <i class="info-tip" data-tip="Exposes ZFS snapshots as Windows Previous Versions via Samba's vfs_shadow_copy2 module. Snapshots must be named in XXXXX-YYYY.MM.DD-HH.MM.SS format (UTC). Windows clients can then right-click a file › Properties › Previous Versions to restore."></i>
  </label>
  <label class="toggle-switch">
    <input type="checkbox" id="share-shadow-copy">
    <span class="toggle-slider"></span>
  </label>
</div>
```

Below the toggle, show a small info note (visible only when the toggle is on):

```html
<div id="shadow-copy-note" style="display:none;font-size:11px;color:var(--text-3);
  background:var(--surface2);border-radius:var(--radius-sm);padding:8px;margin-top:4px;">
  ℹ Snapshots must be named <code>XXXXXX-YYYY.MM.DD-HH.MM.SS</code> (UTC) to appear as
  Previous Versions in Windows Explorer. Use ZNAS snapshot schedules with this naming
  convention, or rename existing snapshots accordingly.
</div>
```

JS: toggle the note visibility on checkbox change.

#### JS — `openCreateShare()` (reset form)

```js
document.getElementById('share-shadow-copy').checked = false;
document.getElementById('shadow-copy-note').style.display = 'none';
```

#### JS — `openEditShare(share)` (populate form)

```js
const sc = document.getElementById('share-shadow-copy');
sc.checked = !!share.shadow_copy;
document.getElementById('shadow-copy-note').style.display = sc.checked ? '' : 'none';
```

#### JS — `saveShare()` / `createShare()` (build payload)

```js
shadow_copy: document.getElementById('share-shadow-copy').checked,
```

#### Share card display

Add a `VSS` badge alongside existing feature badges (TM, RB, etc.) when `share.shadow_copy` is true:

```js
${share.shadow_copy ? '<span class="badge badge-info" title="Shadow Copy / Previous Versions">VSS</span>' : ''}
```

---

## Feature 2 — Physical Disk Power Management

### New config struct — `internal/config/config.go`

Add to `AppConfig`:

```go
DiskPower DiskPowerConfig `json:"disk_power,omitempty"`
```

New struct definition (add before `AppConfig`):

```go
// DiskPowerConfig holds hdparm-based power management settings applied to all
// physical (non-ZFS-metadata) block devices via /etc/hdparm.conf on boot.
type DiskPowerConfig struct {
    Enabled         bool  `json:"enabled"`
    // APM level: 1-127 (spindown allowed), 128-254 (no spindown), 255 (disable APM). 0 = not configured.
    APMLevel        int   `json:"apm_level"`
    // SpindownTimeout: 0=disabled, 1-240=multiples of 5s, 241-251=multiples of 30min. Passed to hdparm -S.
    SpindownTimeout int   `json:"spindown_timeout"`
    // WriteCache: nil=don't set, true=enable (-W1), false=disable (-W0).
    WriteCache      *bool `json:"write_cache,omitempty"`
    // AcousticLevel: -1=not configured, 0=disabled/vendor default, 128=quiet, 254=fast.
    AcousticLevel   int   `json:"acoustic_level"`
}
```

### New file — `system/diskpower.go`

```go
package system

import (
    "fmt"
    "os/exec"
    "strings"
    "github.com/macgaver/zfsnas/internal/config"
)

// DiskPowerPrereqsInstalled returns true when hdparm is available on PATH.
func DiskPowerPrereqsInstalled() bool {
    _, err := exec.LookPath("hdparm")
    return err == nil
}

// ListPhysicalDisks returns all physical block device names (sda, sdb, …)
// excluding loop devices, CD-ROMs, and RAM disks.
func ListPhysicalDisks() ([]string, error) {
    out, err := exec.Command("lsblk", "-dn", "-o", "NAME,TYPE").CombinedOutput()
    if err != nil {
        return nil, fmt.Errorf("lsblk: %w", err)
    }
    var disks []string
    for _, line := range strings.Split(string(out), "\n") {
        fields := strings.Fields(line)
        if len(fields) == 2 && fields[1] == "disk" {
            name := fields[0]
            if !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "sr") &&
               !strings.HasPrefix(name, "zram") {
                disks = append(disks, "/dev/"+name)
            }
        }
    }
    return disks, nil
}

// ApplyDiskPowerConfig writes /etc/hdparm.conf and applies settings immediately
// to all physical disks. Per-disk errors are logged but do not abort.
func ApplyDiskPowerConfig(cfg config.DiskPowerConfig) error

// GetActiveDiskPowerConfig reads the current /etc/hdparm.conf managed block.
func GetActiveDiskPowerConfig() (config.DiskPowerConfig, error)
```

#### `ApplyDiskPowerConfig` implementation notes

1. Generate `/etc/hdparm.conf` content with managed block markers:

```
# ===== ZNAS HDPARM BEGIN =====
/dev/disk/by-path/* {
    apm = <APMLevel>
    spindown_time = <SpindownTimeout>
    write_cache = on|off
    dma = on
}
# ===== ZNAS HDPARM END =====
```

The `disk/by-path/*` wildcard glob ensures settings apply to all present and future SATA disks at boot via the `hdparm` init script or udev.

2. Write via `sudo tee /etc/hdparm.conf`.

3. Discover physical devices via `ListPhysicalDisks()`.

4. For each device, build and run hdparm commands (with sudo):
   - `APMLevel > 0`: `sudo hdparm -B <APMLevel> /dev/sdX`
   - `SpindownTimeout > 0`: `sudo hdparm -S <SpindownTimeout> /dev/sdX`
   - `WriteCache != nil`: `sudo hdparm -W <0|1> /dev/sdX`
   - `AcousticLevel >= 128`: `sudo hdparm -M <AcousticLevel> /dev/sdX`

5. Log errors per disk but continue — SSDs reject APM commands, which is expected.

### New file — `handlers/diskpower.go`

```go
package handlers

// HandleGetDiskPower returns current disk power config + hdparm availability.
// GET /api/disks/power
func HandleGetDiskPower(appCfg *config.AppConfig) http.HandlerFunc

// HandleUpdateDiskPower saves and immediately applies disk power settings.
// PUT /api/disks/power
func HandleUpdateDiskPower(appCfg *config.AppConfig) http.HandlerFunc

// HandleInstallHdparm runs apt-get install hdparm.
// POST /api/disks/power/install
func HandleInstallHdparm(w http.ResponseWriter, r *http.Request)
```

`HandleGetDiskPower` response shape:
```json
{
  "config": { ...DiskPowerConfig... },
  "hdparm_installed": true,
  "disks": ["/dev/sda", "/dev/sdb"]
}
```

`HandleUpdateDiskPower`:
- Decode `config.DiskPowerConfig` from body.
- Validate: `APMLevel` 0–255, `SpindownTimeout` 0–251, `AcousticLevel` -1 or 0 or 128–254.
- Call `system.ApplyDiskPowerConfig(cfg)`.
- Save `appCfg.DiskPower = cfg` and `config.SaveAppConfig(appCfg)`.
- Audit log `audit.ActionUpdateSettings`, target `"disk_power"`.

### Route registration — `handlers/router.go`

```go
r.Handle("/api/disks/power",
    RequireAuth(RequireAdmin(HandleGetDiskPower(appCfg)))).Methods("GET")
r.Handle("/api/disks/power",
    RequireAuth(RequireAdmin(HandleUpdateDiskPower(appCfg)))).Methods("PUT")
r.Handle("/api/disks/power/install",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleInstallHdparm)))).Methods("POST")
```

### Sudoers changes — `SECURITY.md`

Add a new `Cmnd_Alias ZFSNAS_DISKPOWER`:

```sudoers
# ── Disk Power Management (hdparm) ────────────────────────────────────────────
# since v6.4.6 — APM level, spindown timeout, write cache, acoustic management
#   applied immediately to all physical disks; persisted in /etc/hdparm.conf
Cmnd_Alias ZFSNAS_DISKPOWER = \
    /usr/sbin/hdparm *, \
    /usr/bin/tee /etc/hdparm.conf
```

Add `ZFSNAS_DISKPOWER` to the grant line.

### Prerequisites tab

Follow the UPS pattern: add a prerequisite check card for `hdparm`. Show an "Enable" (install) button when not installed. The install button calls `POST /api/disks/power/install`.

### Frontend — `static/index.html`

#### Physical Disks page header — new button

In `id="page-disks"` page header button row:

```html
<button class="btn btn-ghost btn-sm" onclick="openDiskPowerModal()">⚡ Power Management</button>
```

Place it to the left of the existing "Scan for Disks" / "Refresh SMART" buttons.

#### Modal design

```
┌──────────────────────────────────────────────────────────────────┐
│  Disk Power Management                                      [×]  │
│  ─────────────────────────────────────────────────────────────── │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │ ℹ  Settings apply to all physical SATA/SAS disks. SSDs and │ │
│  │    NVMe drives may ignore APM and spindown commands.        │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
│  [✓] Enable disk power management                               │
│                                                                  │
│  APM Level [i]         [127 ▾]  1–127 = allow spindown          │
│  Spindown Timeout [i]  [  0  ] minutes  (0 = disabled)          │
│  Write Cache [i]       (•) Enable  ( ) Disable  ( ) Don't set  │
│  Acoustic Mgmt [i]     (•) Off  ( ) Quiet (128)  ( ) Fast (254)│
│                                                                  │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │ ⚠  Changes apply immediately and persist across reboots    │ │
│  │    via /etc/hdparm.conf.                                    │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
│             [Save & Apply]          [Cancel]                    │
└──────────────────────────────────────────────────────────────────┘
```

If `hdparm` is not installed, replace the form content with:
```
hdparm is not installed.
[Install hdparm]  — calls POST /api/disks/power/install, then reloads modal
```

Info tips:
- **APM Level**: "Advanced Power Management level. 1–127 allows the drive to spin down when idle; 128–254 prevents spindown. 255 disables APM (maximum performance, no power saving). Typical NAS value: 127."
- **Spindown Timeout**: "Idle time before the disk spins down. 0 = disabled. Note: frequent spin-up/spin-down cycles can reduce disk lifespan on spinning rust drives."
- **Write Cache**: "Enables the drive's internal write buffer. Disabling it reduces performance but eliminates data loss risk on sudden power failure (ZFS journaling protects data separately)."
- **Acoustic Mgmt**: "Reduces drive seek noise by limiting seek speed. 'Quiet' (128) = minimum noise; 'Fast' (254) = maximum performance. Most modern drives ignore this setting."

#### JS functions

- `openDiskPowerModal()` — `GET /api/disks/power`, populate form or show install prompt, show modal.
- `saveDiskPower()` — build payload, `PUT /api/disks/power`, toast on success, close modal.
- `closeDiskPowerModal()` — hide modal.

---

## Feature 3 — System/Platform Power Management

### New config struct — `internal/config/config.go`

Add to `AppConfig`:

```go
SystemPower SystemPowerConfig `json:"system_power,omitempty"`
```

New struct definition:

```go
// SystemPowerConfig holds platform-level power management settings.
// These are intended for always-on NAS systems — the UI warns users about
// the trade-offs of aggressive power saving on a server.
type SystemPowerConfig struct {
    // CPUGovernor: "performance"|"powersave"|"ondemand"|"conservative"|"schedutil"|""
    // Empty string = use kernel default (no change).
    CPUGovernor string `json:"cpu_governor,omitempty"`
    // PowerProfile: "performance"|"balanced"|"power-saver"|""
    // Applied via powerprofilesctl if available.
    PowerProfile string `json:"power_profile,omitempty"`
    // USBAutosuspend: nil=don't change, true=enable (2s delay), false=disable.
    USBAutosuspend *bool `json:"usb_autosuspend,omitempty"`
    // PCIeASPM: "default"|"performance"|"powersave"|"powersupersave"|""
    PCIeASPM string `json:"pcie_aspm,omitempty"`
}
```

### New file — `system/syspower.go`

```go
package system

// SystemPowerAvailability contains current settings and feature availability flags.
type SystemPowerAvailability struct {
    Current            config.SystemPowerConfig `json:"current"`
    CPUFreqAvailable   bool                     `json:"cpufreq_available"`
    PowerProfilesAvail bool                     `json:"power_profiles_available"`
    AvailableGovernors []string                 `json:"available_governors"`
    PCIeASPMAvailable  bool                     `json:"pcie_aspm_available"`
}

// GetSystemPowerAvailability reads current active settings + detects feature support.
// - CPU governor from /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
// - Available governors from /sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors
// - Power profile via `powerprofilesctl get` (if binary exists)
// - USB autosuspend from /sys/module/usbcore/parameters/autosuspend
// - PCIe ASPM from /sys/module/pcie_aspm/parameters/policy
func GetSystemPowerAvailability() SystemPowerAvailability

// ApplySystemPowerConfig applies settings and writes persistence config.
//
// Persistence strategy:
//   - CPU governor: /etc/default/cpufrequtils with GOVERNOR="<value>"; apply
//     immediately to all CPUs via /sys/.../scaling_governor (sudo tee).
//   - Power profile: `powerprofilesctl set <profile>` (daemon persists state).
//   - USB autosuspend + PCIe ASPM: managed block in /etc/rc.local.
func ApplySystemPowerConfig(cfg config.SystemPowerConfig) error
```

#### Persistence implementation details

**CPU governor** — `/etc/default/cpufrequtils`:
```
# Generated by ZNAS
GOVERNOR="performance"
```
Written via `sudo tee /etc/default/cpufrequtils`. Immediate application: iterate `/sys/devices/system/cpu/cpu*/cpufreq/scaling_governor` and write via `sudo tee` for each CPU.

**`/etc/rc.local` managed block** (USB autosuspend + PCIe ASPM):
```bash
# ===== ZNAS SYSPOWER BEGIN =====
# USB autosuspend: disabled
for f in /sys/bus/usb/devices/*/power/autosuspend_delay_ms; do echo -1 > "$f" 2>/dev/null; done
for f in /sys/bus/usb/devices/*/power/control; do echo on > "$f" 2>/dev/null; done
# PCIe ASPM policy
echo powersupersave > /sys/module/pcie_aspm/parameters/policy 2>/dev/null || true
# ===== ZNAS SYSPOWER END =====
```

Read existing `/etc/rc.local`, replace the managed block (or append before `exit 0`), write via `sudo tee`. Then `sudo chmod +x /etc/rc.local` to ensure it is executable. Apply settings immediately to sysfs as well.

### New file — `handlers/syspower.go`

```go
// HandleGetSystemPower returns current system power config + availability.
// GET /api/system/power
func HandleGetSystemPower(appCfg *config.AppConfig) http.HandlerFunc

// HandleUpdateSystemPower saves and applies system power settings.
// PUT /api/system/power
func HandleUpdateSystemPower(appCfg *config.AppConfig) http.HandlerFunc
```

`HandleGetSystemPower` response shape:
```json
{
  "config": { ...SystemPowerConfig... },
  "cpufreq_available": true,
  "power_profiles_available": false,
  "available_governors": ["performance", "powersave", "schedutil"],
  "pcie_aspm_available": true
}
```

`HandleUpdateSystemPower`:
- Validate governor is one of known values or empty.
- Validate power profile is one of known values or empty.
- Validate PCIe ASPM is one of known values or empty.
- Call `system.ApplySystemPowerConfig(cfg)`.
- Save `appCfg.SystemPower = cfg` and `config.SaveAppConfig(appCfg)`.
- Audit log `audit.ActionUpdateSettings`, target `"system_power"`.

### Route registration — `handlers/router.go`

```go
r.Handle("/api/system/power",
    RequireAuth(RequireAdmin(HandleGetSystemPower(appCfg)))).Methods("GET")
r.Handle("/api/system/power",
    RequireAuth(RequireAdmin(HandleUpdateSystemPower(appCfg)))).Methods("PUT")
```

Admin-only.

### Sudoers changes — `SECURITY.md`

Add a new `Cmnd_Alias ZFSNAS_SYSPOWER`:

```sudoers
# ── System/Platform Power Management ─────────────────────────────────────────
# since v6.4.6 — CPU governor via cpufrequtils + sysfs; PCIe ASPM and USB
#   autosuspend persistence via /etc/rc.local
Cmnd_Alias ZFSNAS_SYSPOWER = \
    /usr/bin/tee /etc/default/cpufrequtils, \
    /usr/bin/tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor, \
    /usr/bin/tee /sys/module/pcie_aspm/parameters/policy, \
    /usr/bin/tee /etc/rc.local, \
    /usr/bin/chmod +x /etc/rc.local
```

Add `ZFSNAS_SYSPOWER` to the grant line.

### Frontend — `static/index.html`

#### Platform (Updates) page header — new button

In `id="page-updates"` page header, add to the left of the existing `⏻ Power` button:

```html
<button class="btn btn-ghost btn-sm" onclick="openSysPowerModal()">⚡ Power Management</button>
```

#### Modal design

```
┌─────────────────────────────────────────────────────────────────────┐
│  System Power Management                                      [×]  │
│  ────────────────────────────────────────────────────────────────── │
│                                                                    │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │ ⚠  This NAS is designed to run 24/7. Aggressive power       │  │
│  │    saving settings can introduce I/O latency, USB device    │  │
│  │    wake-up delays, and system instability.                   │  │
│  │    The defaults below are safe for always-on NAS operation.  │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                    │
│  CPU Governor [i]                                                  │
│  [performance ▾]   Available: performance, powersave, schedutil   │
│                                                                    │
│  Power Profile [i]            (powerprofilesctl — if available)   │
│  [performance ▾]              ── greyed out if not available ──   │
│                                                                    │
│  USB Autosuspend [i]    (•) Disabled  ( ) Enabled (2s delay)     │
│                                                                    │
│  PCIe ASPM Policy [i]   [default ▾]                              │
│                                                                    │
│             [Save & Apply]            [Cancel]                    │
└─────────────────────────────────────────────────────────────────────┘
```

Info tips:
- **CPU Governor**: "Controls kernel CPU frequency scaling. 'performance' keeps CPUs at max frequency — recommended for NAS. 'schedutil' adapts dynamically to load. 'powersave' minimises power but adds latency."
- **Power Profile**: "Uses power-profiles-daemon (if installed) to set system-wide power intent. 'performance' prevents dynamic throttling. Not available if power-profiles-daemon is not installed."
- **USB Autosuspend**: "Allows the kernel to suspend idle USB devices to save power. On a NAS, this can cause USB-attached UPS adapters or USB storage to become temporarily unresponsive. Recommended: Disabled."
- **PCIe ASPM Policy**: "Active State Power Management for the PCIe bus. 'default' lets the BIOS decide (safest). 'performance' disables ASPM for lowest latency. 'powersupersave' enables all ASPM power states."

Unavailable controls are greyed out with a tooltip: "Not available on this system."

#### JS functions

- `openSysPowerModal()` — `GET /api/system/power`, populate form based on `current` + availability flags, show modal.
- `saveSysPower()` — build payload, `PUT /api/system/power`, toast on success, close modal.
- `closeSysPowerModal()` — hide modal.

---

## Feature 4 — NUT Mode Selection (Standalone / Network Server / Network Client)

### Overview

The UPS Advanced section gains a **Mode** dropdown at the very top. The three modes map directly to NUT's own `nut.conf` MODE setting:

| Mode | NUT MODE | Description |
|---|---|---|
| **Standalone** | `standalone` | Current behaviour — local driver + local upsmon. No network access. |
| **Network Server** | `netserver` | This machine runs the local UPS driver and exposes it to remote NUT clients over the network. |
| **Network Client** | `netclient` | No local UPS. This machine monitors a UPS served by another NUT server on the network. |

The settings shown below the Mode dropdown change based on selection:
- **Standalone** — existing advanced settings (UPS Name, Driver, Port, Pre-shutdown cmd, monitor password). No changes to current layout.
- **Network Server** — same UPS hardware fields as Standalone PLUS network server fields (listen IP, listen port, authorised clients, remote user credentials).
- **Network Client** — replaces hardware fields entirely with remote NUT connectivity fields (host, port, UPS name, username, password) + Test Connection button.

### Config changes — `internal/config/config.go`

New structs:

```go
// NUTServerConfig holds settings for running this machine as a NUT network server
// (MODE=netserver). Remote NUT clients can query this host for UPS data.
type NUTServerConfig struct {
    // ListenIP is the IP address upsd binds to. Default "0.0.0.0" (all interfaces).
    ListenIP   string          `json:"listen_ip"`
    // ListenPort is the NUT protocol port. Default 3493.
    ListenPort int             `json:"listen_port"`
    // AllowedClients is a list of IP addresses or CIDR ranges allowed to connect.
    // Empty list means no IP restriction (rely on firewall).
    AllowedClients []string    `json:"allowed_clients,omitempty"`
    // RemoteUsers is the list of NUT user accounts written to upsd.users for
    // remote clients. Each user needs at minimum upsmon slave access.
    RemoteUsers []NUTRemoteUser `json:"remote_users,omitempty"`
}

// NUTRemoteUser represents a user entry in /etc/nut/upsd.users for remote access.
type NUTRemoteUser struct {
    Username string `json:"username"`
    Password string `json:"password"`
    // Actions: "upsmon" (monitoring only) | "admin" (full control)
    Role     string `json:"role"`
}

// NUTClientConfig holds connectivity settings for connecting to a remote NUT server
// (MODE=netclient). The local UPS driver is not started in this mode.
type NUTClientConfig struct {
    Host     string `json:"host"`               // hostname or IP of remote NUT server
    Port     int    `json:"port"`               // NUT port, default 3493
    UPSName  string `json:"ups_name"`           // UPS name on the remote server
    Username string `json:"username,omitempty"` // NUT username for upsmon slave
    Password string `json:"password,omitempty"` // NUT password
}
```

Update `UPSConfig`:

```go
type UPSConfig struct {
    Enabled         bool              `json:"enabled"`
    // Mode: "standalone" | "network_server" | "network_client"
    // Default (empty) = "standalone" for backward compatibility.
    Mode            string            `json:"mode,omitempty"`
    // --- Standalone / Network Server fields (local hardware) ---
    UPSName         string            `json:"ups_name"`
    Driver          string            `json:"driver"`
    Port            string            `json:"port"`
    MonitorPassword string            `json:"monitor_password,omitempty"`
    RawUPSConf      string            `json:"raw_ups_conf,omitempty"`
    ShutdownPolicy  UPSShutdownPolicy `json:"shutdown_policy"`
    NominalPowerW   *int              `json:"nominal_power_w,omitempty"`
    // --- Network Server extra fields ---
    NUTServer       *NUTServerConfig  `json:"nut_server,omitempty"`
    // --- Network Client fields ---
    NUTClient       *NUTClientConfig  `json:"nut_client,omitempty"`
}
```

Remove the old `RemoteNUTConfig` / `RemoteNUT` references — replaced by `NUTClientConfig`.

### `system/ups.go` — changes

#### Add `QueryUPSClient()`

```go
// QueryUPSClient queries a remote NUT server using upsc remote syntax.
func QueryUPSClient(cfg *config.NUTClientConfig) (*UPSStatus, error) {
    port := cfg.Port
    if port == 0 {
        port = 3493
    }
    target := fmt.Sprintf("%s@%s:%d", cfg.UPSName, cfg.Host, port)
    out, err := exec.Command("upsc", target).CombinedOutput()
    if err != nil {
        return nil, fmt.Errorf("upsc %s: %w — %s", target, err, string(out))
    }
    return parseUPSCOutput(string(out)), nil
}
```

#### Add `ApplyNUTConf()` — writes `/etc/nut/nut.conf`

```go
// ApplyNUTConf writes /etc/nut/nut.conf with the correct MODE directive.
// mode must be "standalone", "netserver", or "netclient".
func ApplyNUTConf(mode string) error {
    content := fmt.Sprintf("# Generated by ZNAS — do not edit manually\nMODE=%s\n", mode)
    return writeFileViaSudo("/etc/nut/nut.conf", []byte(content), 0640)
}
```

#### Add `ApplyNUTUpsdConf()` — writes `/etc/nut/upsd.conf` (server mode)

```go
// ApplyNUTUpsdConf writes /etc/nut/upsd.conf for network server mode.
func ApplyNUTUpsdConf(srv *config.NUTServerConfig) error {
    ip := srv.ListenIP
    if ip == "" {
        ip = "0.0.0.0"
    }
    port := srv.ListenPort
    if port == 0 {
        port = 3493
    }
    var sb strings.Builder
    sb.WriteString("# Generated by ZNAS — do not edit manually\n")
    sb.WriteString(fmt.Sprintf("LISTEN %s %d\n", ip, port))
    for _, cidr := range srv.AllowedClients {
        sb.WriteString(fmt.Sprintf("ACL myhost %s\n", cidr))
        sb.WriteString("ACCEPT myhost\n")
        sb.WriteString("REJECT ALL\n")
    }
    return writeFileViaSudo("/etc/nut/upsd.conf", []byte(sb.String()), 0640)
}
```

#### Add `ApplyNUTUpsdUsers()` — writes `/etc/nut/upsd.users` (server mode)

```go
// ApplyNUTUpsdUsers writes /etc/nut/upsd.users for network server mode.
// Always includes the local upsmon master user; adds remote users from config.
func ApplyNUTUpsdUsers(monitorPassword string, remoteUsers []config.NUTRemoteUser) error {
    var sb strings.Builder
    sb.WriteString("# Generated by ZNAS — do not edit manually\n\n")
    // Local upsmon master user (always present)
    sb.WriteString("[upsmon]\n")
    sb.WriteString(fmt.Sprintf("  password = %s\n", monitorPassword))
    sb.WriteString("  actions = SET\n")
    sb.WriteString("  instcmds = ALL\n")
    sb.WriteString("  upsmon master\n\n")
    // Remote users
    for _, u := range remoteUsers {
        sb.WriteString(fmt.Sprintf("[%s]\n", u.Username))
        sb.WriteString(fmt.Sprintf("  password = %s\n", u.Password))
        if u.Role == "admin" {
            sb.WriteString("  actions = SET\n")
            sb.WriteString("  instcmds = ALL\n")
        }
        sb.WriteString("  upsmon slave\n\n")
    }
    return writeFileViaSudo("/etc/nut/upsd.users", []byte(sb.String()), 0640)
}
```

#### Add `ApplyUPSMonConfigClient()` — upsmon for network client mode

```go
// ApplyUPSMonConfigClient writes /etc/nut/upsmon.conf for network client mode.
// Uses "slave" since the remote server owns the UPS.
func ApplyUPSMonConfigClient(client *config.NUTClientConfig) error {
    port := client.Port
    if port == 0 {
        port = 3493
    }
    username := client.Username
    if username == "" {
        username = "upsmon"
    }
    monConf := fmt.Sprintf(`# Generated by ZNAS — do not edit manually
MONITOR %s@%s:%d 1 %s %s slave
SHUTDOWNCMD "/bin/true"
MINSUPPLIES 1
POLLFREQ 5
POLLFREQALERT 5
HOSTSYNC 15
DEADTIME 15
POWERDOWNFLAG /etc/killpower
NOTIFYFLAG ONLINE  SYSLOG+WALL
NOTIFYFLAG ONBATT  SYSLOG+WALL
NOTIFYFLAG LOWBATT SYSLOG+WALL
`, client.UPSName, client.Host, port, username, client.Password)
    return writeFileViaSudo("/etc/nut/upsmon.conf", []byte(monConf), 0640)
}
```

#### Update `StartUPSShutdownWatcher`

```go
var status *UPSStatus
var err error
mode := ups.Mode
if mode == "" {
    mode = "standalone"
}
switch mode {
case "network_client":
    if ups.NUTClient != nil && ups.NUTClient.Host != "" {
        status, err = QueryUPSClient(ups.NUTClient)
    }
default: // standalone or network_server both query localhost
    status, err = QueryUPS(ups.UPSName)
}
```

### `handlers/ups.go` — changes

#### Add `HandleTestNUTClient`

```go
// HandleTestNUTClient tests connectivity to a remote NUT server.
// POST /api/ups/test-client
func HandleTestNUTClient(w http.ResponseWriter, r *http.Request) {
    var req config.NUTClientConfig
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        jsonErr(w, http.StatusBadRequest, "invalid request body")
        return
    }
    if req.Host == "" || req.UPSName == "" {
        jsonErr(w, http.StatusBadRequest, "host and ups_name are required")
        return
    }
    if req.Port == 0 {
        req.Port = 3493
    }
    status, err := system.QueryUPSClient(&req)
    if err != nil {
        jsonOK(w, map[string]interface{}{"ok": false, "error": err.Error()})
        return
    }
    jsonOK(w, map[string]interface{}{"ok": true, "status": status})
}
```

#### Update `HandleGetUPSStatus`

```go
mode := appCfg.UPS.Mode
if mode == "" {
    mode = "standalone"
}
var status *UPSStatus
var err error
switch mode {
case "network_client":
    if appCfg.UPS.NUTClient != nil {
        status, err = system.QueryUPSClient(appCfg.UPS.NUTClient)
    }
default:
    status, err = system.QueryUPS(appCfg.UPS.UPSName)
}
```

#### Update `HandleUpdateUPSConfig`

Apply config files based on mode:

```go
mode := cfg.Mode
if mode == "" {
    mode = "standalone"
}

// Write nut.conf MODE directive
nutMode := map[string]string{
    "standalone":     "standalone",
    "network_server": "netserver",
    "network_client": "netclient",
}[mode]
if err := system.ApplyNUTConf(nutMode); err != nil {
    jsonErr(w, http.StatusInternalServerError, "apply nut.conf: "+err.Error())
    return
}

switch mode {
case "standalone":
    // Existing: ApplyUPSConf + ApplyUPSMonConfig (unchanged)
    if err := system.ApplyUPSConf(...); err != nil { ... }
    if err := system.ApplyUPSMonConfig(cfg.UPSName, cfg.MonitorPassword); err != nil { ... }

case "network_server":
    // Local driver config (same as standalone)
    if err := system.ApplyUPSConf(...); err != nil { ... }
    // upsd network config
    if cfg.NUTServer != nil {
        if err := system.ApplyNUTUpsdConf(cfg.NUTServer); err != nil { ... }
        if err := system.ApplyNUTUpsdUsers(cfg.MonitorPassword, cfg.NUTServer.RemoteUsers); err != nil { ... }
    }
    // upsmon as master (same as standalone)
    if err := system.ApplyUPSMonConfig(cfg.UPSName, cfg.MonitorPassword); err != nil { ... }

case "network_client":
    // No local driver — only upsmon pointing at remote
    if cfg.NUTClient != nil && cfg.NUTClient.Host != "" {
        if err := system.ApplyUPSMonConfigClient(cfg.NUTClient); err != nil { ... }
    }
}

system.RestartNUTServices()
```

### Route registration — `handlers/router.go`

```go
r.Handle("/api/ups/test-client",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleTestNUTClient)))).Methods("POST")
```

### Frontend — `static/index.html`

#### UPS Advanced section — Mode at the top

The existing Advanced `<details>` block gains a **Mode** dropdown as its very first element. The settings panels below swap based on the selected mode using a `data-ups-mode` show/hide pattern.

```
┌─ Advanced ──────────────────────────────────────────────────────────┐
│                                                                     │
│  Mode [i]   [Standalone ▾]                                         │
│             ─────────────────────────────                           │
│             Standalone     ← current                               │
│             Network Server ← this NAS serves UPS data              │
│             Network Client ← connect to remote NUT server          │
│                                                                     │
│  ┌── Shown when Standalone or Network Server ───────────────────┐  │
│  │  Pre-shutdown command  [________________]                     │  │
│  │  UPS Name              [ups            ]                      │  │
│  │  Driver                [usbhid-ups     ]                      │  │
│  │  Port                  [auto           ]                      │  │
│  │  Monitor Password      [••••••         ]                      │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌── Shown when Network Server ─────────────────────────────────┐  │
│  │  Listen IP     [0.0.0.0   ]  [i]                             │  │
│  │  Listen Port   [3493      ]                                   │  │
│  │  Allowed Clients (optional)  [+Add]                          │  │
│  │    192.168.1.0/24  [×]                                        │  │
│  │                                                               │  │
│  │  Remote Users    [+Add User]                                  │  │
│  │  ┌─────────────────────────────────────────────────────────┐ │  │
│  │  │ Username  Password    Role        [×]                   │ │  │
│  │  │ monitor   ••••••••   Monitoring  [×]                   │ │  │
│  │  └─────────────────────────────────────────────────────────┘ │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌── Shown when Network Client ─────────────────────────────────┐  │
│  │  ℹ  No local UPS driver is used in this mode. This machine  │  │
│  │     monitors a UPS served by another NUT server.             │  │
│  │                                                               │  │
│  │  Remote Host     [192.168.1.50           ]                   │  │
│  │  Port            [3493  ]  UPS Name  [ups]                   │  │
│  │  Username (opt)  [upsmon]  Password  [••••]                  │  │
│  │                                                               │  │
│  │  [Test Connection]  ✓ Connection successful — OL             │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

Info tips:
- **Mode — Standalone**: "The default NUT mode. The UPS driver, upsd daemon, and upsmon all run locally on this machine. No network exposure."
- **Mode — Network Server**: "This machine runs the local UPS driver and exposes UPS data to remote NUT clients over the network. Remote systems can monitor the UPS via this server."
- **Mode — Network Client**: "This machine does not have a directly connected UPS. It monitors a UPS via another NUT server on the network. The local driver and upsd are not started."
- **Listen IP**: "IP address that upsd listens on for incoming NUT client connections. Use 0.0.0.0 to accept connections on all network interfaces, or specify an IP to restrict to one interface."

#### JS — mode switching

```js
function upsModeSwitched() {
  const mode = document.getElementById('ups-mode-select').value;
  // Show/hide section panels
  document.getElementById('ups-adv-hardware').style.display =
    (mode === 'standalone' || mode === 'network_server') ? '' : 'none';
  document.getElementById('ups-adv-server').style.display =
    (mode === 'network_server') ? '' : 'none';
  document.getElementById('ups-adv-client').style.display =
    (mode === 'network_client') ? '' : 'none';
}
```

Called on `<select id="ups-mode-select" onchange="upsModeSwitched()">`.

#### JS — populate on panel open

```js
const mode = cfg.mode || 'standalone';
document.getElementById('ups-mode-select').value = mode;
upsModeSwitched(); // show/hide correct panels

// Server fields
const srv = cfg.nut_server || {};
document.getElementById('ups-srv-listen-ip').value   = srv.listen_ip   || '0.0.0.0';
document.getElementById('ups-srv-listen-port').value = srv.listen_port || 3493;
renderNUTAllowedClients(srv.allowed_clients || []);
renderNUTRemoteUsers(srv.remote_users || []);

// Client fields
const cli = cfg.nut_client || {};
document.getElementById('ups-cli-host').value     = cli.host     || '';
document.getElementById('ups-cli-port').value     = cli.port     || 3493;
document.getElementById('ups-cli-upsname').value  = cli.ups_name || '';
document.getElementById('ups-cli-username').value = cli.username || '';
document.getElementById('ups-cli-password').value = cli.password || '';
```

#### JS — `saveUPSConfig()` — include mode + server/client fields

```js
const mode = document.getElementById('ups-mode-select')?.value || 'standalone';

const payload = {
  ...existingStandaloneFields,
  mode,
  nut_server: mode === 'network_server' ? {
    listen_ip:       (document.getElementById('ups-srv-listen-ip')?.value || '0.0.0.0').trim(),
    listen_port:     parseInt(document.getElementById('ups-srv-listen-port')?.value || '3493') || 3493,
    allowed_clients: getNUTAllowedClients(),   // reads dynamic list
    remote_users:    getNUTRemoteUsers(),       // reads dynamic list
  } : null,
  nut_client: mode === 'network_client' ? {
    host:     (document.getElementById('ups-cli-host')?.value     || '').trim(),
    port:     parseInt(document.getElementById('ups-cli-port')?.value || '3493') || 3493,
    ups_name: (document.getElementById('ups-cli-upsname')?.value  || '').trim(),
    username: (document.getElementById('ups-cli-username')?.value || '').trim() || undefined,
    password: document.getElementById('ups-cli-password')?.value  || undefined,
  } : null,
};
```

#### JS — `upsTestNUTClient()`

```js
async function upsTestNUTClient() {
  const btn    = document.getElementById('btn-ups-test-client');
  const result = document.getElementById('ups-client-test-result');
  const host    = (document.getElementById('ups-cli-host')?.value    || '').trim();
  const port    = parseInt(document.getElementById('ups-cli-port')?.value   || '3493') || 3493;
  const upsName = (document.getElementById('ups-cli-upsname')?.value || '').trim();

  if (!host || !upsName) {
    result.textContent = 'Enter host and UPS name first.';
    result.style.color = 'var(--accent-danger)';
    return;
  }

  btn.disabled = true;
  btn.textContent = 'Testing…';
  result.textContent = '';

  try {
    const r = await apiFetch('/api/ups/test-client', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        host,
        port,
        ups_name: upsName,
        username: (document.getElementById('ups-cli-username')?.value || '').trim(),
        password: document.getElementById('ups-cli-password')?.value  || '',
      }),
    });
    const d = await r.json();
    if (d.ok) {
      result.textContent = '✓ Connection successful' + (d.status?.raw_status ? ' — ' + d.status.raw_status : '');
      result.style.color = 'var(--accent-3)';
    } else {
      result.textContent = '✗ ' + (d.error || 'Connection failed');
      result.style.color = 'var(--accent-danger)';
    }
  } catch (e) {
    result.textContent = '✗ ' + e.message;
    result.style.color = 'var(--accent-danger)';
  } finally {
    btn.disabled = false;
    btn.textContent = 'Test Connection';
  }
}
```

---

## Files Changed Summary

| File | Change |
|---|---|
| `internal/config/config.go` | Add `DiskPowerConfig`, `SystemPowerConfig`, `NUTServerConfig`, `NUTRemoteUser`, `NUTClientConfig` structs; add `DiskPower`, `SystemPower` to `AppConfig`; add `Mode`, `NUTServer`, `NUTClient` to `UPSConfig` |
| `system/smb.go` | Add `ShadowCopy bool` to `SMBShare`; reorder VFS objects assembly to put `shadow_copy2` first; write shadow copy smb.conf parameters when enabled |
| `system/diskpower.go` | **New file** — `DiskPowerPrereqsInstalled()`, `ListPhysicalDisks()`, `ApplyDiskPowerConfig()`, `GetActiveDiskPowerConfig()`, `hdparmConfContent()` |
| `system/syspower.go` | **New file** — `SystemPowerAvailability` struct, `GetSystemPowerAvailability()`, `ApplySystemPowerConfig()` |
| `system/ups.go` | Add `QueryUPSRemote()`, `ApplyUPSMonConfigRemote()`; update `StartUPSShutdownWatcher` to use remote query when configured |
| `handlers/diskpower.go` | **New file** — `HandleGetDiskPower`, `HandleUpdateDiskPower`, `HandleInstallHdparm` |
| `handlers/syspower.go` | **New file** — `HandleGetSystemPower`, `HandleUpdateSystemPower` |
| `handlers/ups.go` | Add `HandleTestNUTClient`; update `HandleGetUPSStatus` and `HandleUpdateUPSConfig` for mode-based routing; add `ApplyNUTConf`, `ApplyNUTUpsdConf`, `ApplyNUTUpsdUsers`, `ApplyUPSMonConfigClient` in `system/ups.go` |
| `handlers/router.go` | Register: `GET/PUT /api/disks/power`, `POST /api/disks/power/install`, `GET/PUT /api/system/power`, `POST /api/ups/test-client` |
| `SECURITY.md` | Add `Cmnd_Alias ZFSNAS_DISKPOWER` and `Cmnd_Alias ZFSNAS_SYSPOWER`; add both to grant line |
| `static/index.html` | Feature 1: Shadow Copy toggle + info note in create/edit share modals + VSS badge in share cards. Feature 2: "Power Management" button in disks page header + `modal-disk-power` + JS. Feature 3: "Power Management" button in platform/updates header + `modal-sys-power` + JS. Feature 4: Mode dropdown at top of UPS Advanced section with three panels (Standalone / Network Server / Network Client); `upsModeSwitched()`, `upsTestNUTClient()`, extended `saveUPSConfig()` |
| `internal/version/version.go` | `"6.4.6"` |

---

## Sudoers New Entries

### `ZFSNAS_DISKPOWER` (new)

```sudoers
# ── Disk Power Management (hdparm) ────────────────────────────────────────────
# since v6.4.6 — APM level, spindown timeout, write cache, acoustic management
#   applied immediately to all physical disks; persisted via /etc/hdparm.conf
Cmnd_Alias ZFSNAS_DISKPOWER = \
    /usr/sbin/hdparm *, \
    /usr/bin/tee /etc/hdparm.conf
```

### `ZFSNAS_SYSPOWER` (new)

```sudoers
# ── System/Platform Power Management ─────────────────────────────────────────
# since v6.4.6 — CPU governor via cpufrequtils + sysfs; PCIe ASPM and USB
#   autosuspend persistence via /etc/rc.local
Cmnd_Alias ZFSNAS_SYSPOWER = \
    /usr/bin/tee /etc/default/cpufrequtils, \
    /usr/bin/tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor, \
    /usr/bin/tee /sys/module/pcie_aspm/parameters/policy, \
    /usr/bin/tee /etc/rc.local, \
    /usr/bin/chmod +x /etc/rc.local
```

### Updated grant line

```sudoers
zfsnas ALL=(ALL) NOPASSWD: \
    ZFSNAS_ZFS, ZFSNAS_SMB, ZFSNAS_NFS, ZFSNAS_ISCSI, ZFSNAS_MINIO, ZFSNAS_UPS, \
    ZFSNAS_SMART, ZFSNAS_DISK, ZFSNAS_SCAN, ZFSNAS_FILES, ZFSNAS_SYSTEM, ZFSNAS_APT, \
    ZFSNAS_SECURITY, ZFSNAS_DISKPOWER, ZFSNAS_SYSPOWER
```

---

## Version Bump

`internal/version/version.go`: `"6.4.6"`

## Status: COMPLETE
