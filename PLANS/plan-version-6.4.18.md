# Plan — Version 6.4.18

## Overview

**LXD VM & Container Management** — Experimental feature (`--experimental` flag required).
When ZNAS starts with `--experimental` and the `lxc` CLI is accessible to the ZNAS process user,
a **VMs & Containers** item appears in the left navigation sidebar. The feature provides a
unified table of LXD instances (both VMs and LXC containers), power lifecycle controls,
in-browser consoles, and full create wizards for both types.

---

## Experimental Flag Behaviour

- `--experimental` CLI flag sets `AppConfig.ExperimentalMode = true` (or an in-memory flag).
- On startup, if `ExperimentalMode` is true, ZNAS probes LXD accessibility:
  - Runs `lxc list --format json` as the ZNAS process user.
  - If it succeeds (exit 0), sets `lxdAvailable = true`; the nav item is shown.
  - If it fails (lxc not found, permission denied, LXD not running), `lxdAvailable = false`; the
    nav item is hidden. A warning is logged to stdout.
- The probe result is cached at startup; a refresh endpoint re-runs it on demand.

---

## Navigation

- New sidebar section **Virtualization** (below Sharing, above Overview or at bottom).
- Single nav item: **VMs & Containers** (icon: `⬡` or a server/VM icon using an SVG inline).
- Hidden when `lxdAvailable = false` or `ExperimentalMode = false`.
- Nav dot: green when LXD daemon is reachable, grey when not responding.

---

## Instance Table

### Layout

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│  VMs & Containers                                            [+ New VM]  [+ New Container]  │
├──────┬───────────────┬──────────┬──────────┬────────────┬───────┬──────┬────────┬──────────┤
│ Type │ Name          │ Status   │ IP       │ Image      │ vCPU  │ RAM  │  [⏻]  │   [⋮]    │
├──────┼───────────────┼──────────┼──────────┼────────────┼───────┼──────┼────────┼──────────┤
│  VM  │ ubuntu-dev    │ ● Running│ 10.0.0.2 │ ubuntu/24  │   4   │ 8 GB │  [⏻]  │   [⋮]    │
│ LXC  │ alpine-test   │ ○ Stopped│ —        │ alpine/3.19│   2   │ 512M │  [⏻]  │   [⋮]    │
└──────┴───────────────┴──────────┴──────────┴────────────┴───────┴──────┴────────┴──────────┘
```

### Columns

| Column   | Source (`lxc list --format json`) | Notes |
|----------|-----------------------------------|-------|
| Type     | `type` field: `virtual-machine` → `VM`, `container` → `LXC` | Badge styled like pool health chips |
| Name     | `name` | Monospace |
| Status   | `status` | Green dot = Running, grey = Stopped, yellow = Starting/Stopping |
| IPv4     | `state.network[].addresses[family=inet]` first address | `—` if stopped/no addr |
| Image    | `config["image.description"]` or `config["image.os"]/config["image.version"]` | Truncated |
| vCPU     | `config["limits.cpu"]` or `—` (uses host default) | |
| RAM      | `config["limits.memory"]` formatted (MB/GB) or `—` | |
| Terminal | `[>_]` icon — opens console in new browser tab | |
| Power    | `[⏻]` icon — opens power action dropdown | |
| Burger   | `[⋮]` — opens action menu | |

### Power Dropdown

Context-sensitive based on current status:

- **Running** → Start (disabled), Restart, Shutdown (graceful), Power Off (force)
- **Stopped** → Start, (rest disabled)
- **Starting / Stopping** → all disabled, spinner shown

Power actions call `lxc start|stop|restart|stop --force <name>`.

After action, row status is polled every 2 s until stable (max 30 s), then refreshed.

### Burger Menu (⋮)

- **Delete** — opens confirmation modal:
  - "Delete [name]?" with instance name in red
  - Checkbox: "Also delete all storage volumes" (maps to `lxc delete --force`)
  - [Cancel] [Delete] buttons

### Terminal Icon

- Opens `/lxd-console/<name>` in a new browser tab (`target="_blank"`).
- That route serves a full-page HTML shell with an xterm.js terminal.
- Backend: WebSocket at `GET /ws/lxd-console?name=<name>` — pipes `lxc exec <name> -- bash`
  (VMs) or `lxc console <name>` (LXC containers — interactive console, not shell exec).
  Prefer `lxc exec <name> -- bash` for both types; fall back to `lxc exec <name> -- sh`.
- The full-page console shares the existing xterm.js from the regular terminal feature.
- Auth: same session cookie required; 401 if not authenticated.

---

## New VM Wizard

Triggered by **[+ New VM]** button. Multi-section modal (tall, scrollable, `max-width: 860px`).

### Sections

#### 1. Basic
| Field | Type | Default | Notes |
|-------|------|---------|-------|
| Name | text input | — | LXD instance name rules (alphanumeric + hyphens) |
| Image | custom select | — | Populated from `lxc image list images: --format json`; filtered to `vm` type; searchable |
| Profile | custom select | `default` | From `lxc profile list --format json` |
| Auto-start | checkbox | off | Sets `boot.autostart=true` in config |

#### 2. Resources
| Field | Type | Default |
|-------|------|---------|
| vCPU count | number input (1–128) | 2 |
| Memory | number + unit select (MB/GB) | 2 GB |
| CPU pinning (optional) | text input | e.g. `0-3` |

#### 3. Virtual Disks

Table of disks. One root disk pre-populated (cannot be removed):

| # | Pool | Size | Device name | Actions |
|---|------|------|-------------|---------|
| root | `default` (custom select from `lxc storage list`) | size input | `root` (fixed) | — |
| + | (custom select) | size input | text input | [Remove] |

**[+ Add Disk]** button appends a new disk row.

Each additional disk maps to `lxc config device add <name> <device> disk pool=<pool> size=<size>`.

#### 4. Network
| Field | Type | Default |
|-------|------|---------|
| NIC device name | text | `eth0` |
| Network | custom select (from `lxc network list --format json`) | `lxdbr0` |
| MAC address | text (optional) | auto |

**[+ Add NIC]** to add more network interfaces.

#### 5. USB Passthrough

Table; initially empty.

**[+ Add USB Device]** opens a sub-picker:
- Lists USB devices from `lsusb` parsed output (Bus, Device, ID, Description).
- User selects one; it is added to the table.
- Maps to `lxc config device add <name> usb<n> usb vendorid=<vid> productid=<pid>`.

#### 6. PCI Passthrough

Table; initially empty. Warning banner: "PCI passthrough requires IOMMU enabled in BIOS/UEFI and the host kernel. The device will be unavailable to the host while passed through."

**[+ Add PCI Device]** opens a sub-picker:
- Lists PCI devices from `lspci -vmm` parsed output (Slot, Class, Vendor, Device).
- User selects one; it is added to the table.
- Maps to `lxc config device add <name> pci<n> pci address=<slot>`.

#### 7. Cloud-Init (optional, collapsible)
- User-data textarea (raw YAML).
- Sets via `lxc config set <name> user.user-data <value>`.

### Create Flow

1. Client POSTs to `POST /api/lxd/vms` with full config JSON.
2. Backend assembles and runs:
   ```
   lxc init <image> <name> --vm -p <profile> [config key=value...]
   lxc config device override <name> root pool=<pool> size=<size>
   [lxc config device add <name> ... for each extra disk/usb/pci/nic]
   lxc start <name>   (if auto-start)
   ```
3. Progress streamed via WebSocket `GET /ws/lxd-create-progress?job_id=<id>` (same job_id polling pattern as pool creation).
4. On completion, instance table refreshes.

---

## New LXC Container Wizard

Triggered by **[+ New Container]** button. Lighter modal than VM (`max-width: 720px`).

### Sections

#### 1. Basic
| Field | Type | Default | Notes |
|-------|------|---------|-------|
| Name | text input | — | |
| Image | custom select | — | From `lxc image list images: --format json`; filtered to `container` type; searchable; shows OS + version + arch + size |
| Profile | custom select | `default` | |
| Auto-start | checkbox | off | |

#### 2. Resources
| Field | Type | Default |
|-------|------|---------|
| CPU cores | number input (1–128) | 1 |
| Memory limit | number + unit (MB/GB) | 512 MB |
| Disk size | number + unit (GB) | 10 GB |

#### 3. Device Passthrough

Table; initially empty.

**[+ Add Device]** opens a form row inline:
| Field | Notes |
|-------|-------|
| Device name | Arbitrary label used by LXD (e.g. `gpu0`, `ttyUSB0`) |
| Type | custom select: `unix-char`, `unix-block`, `usb`, `gpu`, `disk` |
| Host path | For unix-char/unix-block: `/dev/...` path on the host |
| Additional params | Key=value pairs, one per row with [+]/[-] buttons. Shown dynamically based on type. |

Passthrough maps to `lxc config device add <name> <device-name> <type> [key=value ...]`.

#### 4. Network
Same as VM wizard (NIC device name + network custom select).

### Create Flow

1. Client POSTs to `POST /api/lxd/containers` with config JSON.
2. Backend runs:
   ```
   lxc init <image> <name> -p <profile> [config key=value...]
   [lxc config device add <name> ... for each device]
   lxc start <name>   (if auto-start)
   ```
3. Progress via same WebSocket job_id pattern.
4. Instance table refreshes on completion.

---

## Backend — Go

### New Files

| File | Purpose |
|------|---------|
| `system/lxd.go` | All `lxc` CLI wrappers |
| `handlers/lxd.go` | HTTP + WebSocket handlers |

### `system/lxd.go`

```go
// Detection
func LXDAvailable() bool          // runs `lxc list --format json`, returns true on exit 0
func LXDVersion() string          // `lxc version`

// Instance listing
func ListLXDInstances() ([]LXDInstance, error)

// Lifecycle
func LXDStart(name string) error
func LXDStop(name string, force bool) error
func LXDRestart(name string) error
func LXDDelete(name string, deleteVolumes bool) error

// Instance status poll
func LXDGetStatus(name string) (string, error)  // returns "Running", "Stopped", etc.

// Creation
func LXDCreateVM(req LXDCreateVMRequest, logCh chan<- string) error
func LXDCreateContainer(req LXDCreateContainerRequest, logCh chan<- string) error

// Image listing (for wizards)
func LXDListRemoteImages(remote string, kind string) ([]LXDImage, error) // kind = "vm"|"container"

// Resource listing (for wizards)
func LXDListProfiles() ([]string, error)
func LXDListStoragePools() ([]string, error)
func LXDListNetworks() ([]string, error)

// Hardware (for passthrough pickers)
func ListUSBDevices() ([]USBDevice, error)    // parses lsusb
func ListPCIDevices() ([]PCIDevice, error)    // parses lspci -vmm
```

### Key Structs

```go
type LXDInstance struct {
    Name        string
    Type        string   // "virtual-machine" | "container"
    Status      string
    IPv4        string
    Image       string
    CPULimit    string
    MemoryLimit string
}

type LXDCreateVMRequest struct {
    Name        string
    Image       string
    Profile     string
    AutoStart   bool
    VCPU        int
    MemoryMB    int
    RootPool    string
    RootSizeGB  int
    ExtraDisks  []LXDDisk
    NICs        []LXDNIC
    USBDevices  []LXDUSBDevice
    PCIDevices  []LXDPCIDevice
    CloudInit   string
}

type LXDCreateContainerRequest struct {
    Name       string
    Image      string
    Profile    string
    AutoStart  bool
    CPUCores   int
    MemoryMB   int
    DiskSizeGB int
    Devices    []LXDPassthroughDevice
    NICs       []LXDNIC
}
```

### `handlers/lxd.go` — Routes

| Method | Path | Handler | Notes |
|--------|------|---------|-------|
| GET | `/api/lxd/status` | `HandleLXDStatus` | `{available, version}` |
| GET | `/api/lxd/instances` | `HandleListInstances` | returns `[]LXDInstance` |
| POST | `/api/lxd/instances/:name/start` | `HandleLXDStart` | |
| POST | `/api/lxd/instances/:name/stop` | `HandleLXDStop` | body: `{force: bool}` |
| POST | `/api/lxd/instances/:name/restart` | `HandleLXDRestart` | |
| DELETE | `/api/lxd/instances/:name` | `HandleLXDDelete` | body: `{delete_volumes: bool}` |
| GET | `/api/lxd/instances/:name/status` | `HandleLXDInstanceStatus` | for polling after power action |
| POST | `/api/lxd/vms` | `HandleCreateVM` | returns `{job_id}` |
| POST | `/api/lxd/containers` | `HandleCreateContainer` | returns `{job_id}` |
| GET | `/api/lxd/images` | `HandleListImages` | query: `?kind=vm\|container` |
| GET | `/api/lxd/profiles` | `HandleListProfiles` | |
| GET | `/api/lxd/storage-pools` | `HandleListStoragePools` | |
| GET | `/api/lxd/networks` | `HandleListNetworks` | |
| GET | `/api/lxd/usb-devices` | `HandleListUSB` | |
| GET | `/api/lxd/pci-devices` | `HandleListPCI` | |
| WS | `/ws/lxd-create-progress` | `HandleLXDCreateProgress` | query: `?job_id=<id>` |
| WS | `/ws/lxd-console` | `HandleLXDConsole` | query: `?name=<name>` |
| GET | `/lxd-console/:name` | `ServeLXDConsolePage` | full-page xterm.js HTML (no auth redirect, session required) |

All mutating handlers require `RequireAdmin`.
`HandleListInstances`, `HandleLXDInstanceStatus` require `RequireAuth`.
`HandleLXDConsole` + `ServeLXDConsolePage` require `RequireAuth` (standard users with terminal permission if applicable).

### Route registration (`handlers/router.go`)

All LXD routes registered only when `cfg.ExperimentalMode` is true.

### `main.go`

- Parse `--experimental` flag → set `experimentalMode` bool.
- If `experimentalMode`, call `system.LXDAvailable()` and log result.
- Store availability in a package-level `lxdAvailable bool` (or pass via `AppState`).

### Config changes (`internal/config/config.go`)

```go
// No persistent config needed — experimental features are flag-driven.
// AppConfig gains no new fields for this feature.
```

---

## Frontend (`static/index.html`)

### Nav

```html
<!-- Shown only when lxdAvailable (set by /api/lxd/status on page load) -->
<div class="nav-section" id="nav-virt-section" style="display:none">
  <div class="nav-section-title">Virtualization</div>
  <div class="nav-item" onclick="showPage('lxd')">
    <span class="nav-icon">⬡</span> VMs &amp; Containers
    <span class="nav-dot" id="nav-dot-lxd"></span>
  </div>
</div>
```

### Page: `page-lxd`

```
Top bar: "VMs & Containers"  [Refresh icon]       [+ New VM]  [+ New Container]
Table: columns described above
```

### Key JS Functions

| Function | Purpose |
|----------|---------|
| `initLXD()` | Called on page load; fetches `/api/lxd/status`, shows/hides nav, loads instances |
| `loadLXDInstances()` | GET `/api/lxd/instances`, renders table |
| `renderLXDTable(instances)` | Builds table HTML |
| `openLXDPowerMenu(name, status, el)` | Shows power dropdown anchored to button |
| `lxdPowerAction(name, action)` | POST to start/stop/restart; polls status |
| `pollLXDInstanceStatus(name, targetStatus)` | Polls every 2 s, updates row |
| `confirmLXDDelete(name)` | Opens delete confirmation modal |
| `openNewVMModal()` | Opens VM creation wizard |
| `openNewContainerModal()` | Opens container creation wizard |
| `openLXDConsole(name)` | `window.open('/lxd-console/' + name, '_blank')` |
| `submitCreateVM(event)` | Collects wizard form, POST `/api/lxd/vms`, opens progress WS |
| `submitCreateContainer(event)` | Collects form, POST `/api/lxd/containers`, opens progress WS |
| `loadLXDImages(kind, selectId)` | GET `/api/lxd/images?kind=kind`, populates select |
| `addVMDiskRow()` | Appends disk row in VM wizard |
| `addVMNICRow()` | Appends NIC row in VM wizard |
| `openUSBPicker()` | Sub-modal listing USB devices |
| `openPCIPicker()` | Sub-modal listing PCI devices |
| `addContainerDeviceRow()` | Appends passthrough device row |

### Console Page (`/lxd-console/:name`)

Served as a standalone HTML page (not the SPA):
- Full-viewport dark background.
- xterm.js loaded from the same CDN used by existing terminal.
- Title: "Console — [name]".
- Opens WebSocket to `wss://<host>/ws/lxd-console?name=<name>`.
- No nav, no header — pure terminal.

---

## Security Notes

- All `lxc` commands are run via `exec.Command("lxc", ...)` — no shell interpolation.
- Instance names passed to commands are validated against `^[a-zA-Z0-9-]+$` before use.
- PCI passthrough device addresses validated against `^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`.
- USB vendorid/productid validated as 4-hex-char strings.
- No sudo required — ZNAS process user must be in the `lxd` group (documented in Prerequisites).
- Prerequisites page shows a note: "For LXD support (experimental), add the ZNAS user to the `lxd` group: `sudo usermod -aG lxd <user>`."

---

## Prerequisites Page Update

Add a new **optional** prerequisites row for LXD:

| Package | Status | Action |
|---------|--------|--------|
| LXD (experimental) | Detected / Not detected | [Install LXD] |

Install action runs `snap install lxd` + `lxd init --auto` (non-interactive) via existing prereq install mechanism.

Add to sudoers if needed: LXD install via snap may require sudo for `snap install lxd` only; post-install operations use `lxc` as the process user (no sudo needed). Update `SECURITY.md` accordingly.

---

## Version Bump

`internal/version/version.go`: `Version = "6.4.18"`

---

## File Change Summary

| File | Change |
|------|--------|
| `main.go` | Parse `--experimental`; probe LXD; store `lxdAvailable` |
| `system/lxd.go` | New — all `lxc` CLI wrappers + USB/PCI device lister |
| `handlers/lxd.go` | New — all HTTP + WebSocket handlers |
| `handlers/router.go` | Register LXD routes (gated on `experimentalMode`) |
| `internal/version/version.go` | Bump to 6.4.18 |
| `static/index.html` | Nav item, LXD page, VM wizard modal, container wizard modal, delete modal, console page template, all JS functions |
| `SECURITY.md` | LXD group membership note |

---

## Out of Scope (future versions)

- LXD clustering / remote LXD servers
- Snapshot management for instances
- Live migration
- Resource graphs per instance
- LXD project support
- Image publishing / export
