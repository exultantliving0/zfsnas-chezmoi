# Plan — Version 6.4.19

**Scope:** LXC Network management (experimental, requires `--experimental` flag and LXC accessible)

---

## Feature Overview

1. **Mode bar: icons-only, no button border, tooltip on hover**
2. **Networking mode button** — 3rd mode alongside Storage and Compute
3. **Networks sidebar section** in networking mode — lists LXC bridges
4. **Networks page** — bridge table with VM counts, descriptions, edit, delete, create
5. **Create Bridge popup** — configures `/etc/network/interfaces` + `lxc network create`
6. **Edit Bridge popup** — edit description and LXC config keys

---

## Architecture

### Gate
All networking UI is hidden unless `appConfig.Experimental` is true AND `lxcAvailable` is true (same gate as compute mode).

### Data flow
- Bridge list: `lxc network list --format json` — returns all networks
- Bridge detail: `lxc network show <name> --format json`
- VM usage: parse `used_by[]` URIs — count entries matching `/1.0/instances/`
- Physical interfaces: read `/etc/network/interfaces` + `/etc/network/interfaces.d/*`

---

## Backend

### New file: `system/lxdnet.go`

```go
type LXDNetwork struct {
    Name        string            `json:"name"`
    Type        string            `json:"type"`         // "bridge", "physical", "vlan", etc.
    Managed     bool              `json:"managed"`
    Description string            `json:"description"`
    State       string            `json:"state"`        // "Created" | ""
    IPv4        string            `json:"ipv4"`
    IPv6        string            `json:"ipv6"`
    Config      map[string]string `json:"config"`
    UsedBy      []string          `json:"used_by"`      // raw /1.0/instances/... URIs
    VMCount     int               `json:"vm_count"`     // computed from used_by
}

type LXDNetworkCreateRequest struct {
    Name                string `json:"name"`
    Description         string `json:"description"`
    // Bridge external interface config
    BridgeType          string `json:"bridge_type"`          // "nat" | "vlan" | "plain"
    // For "nat": LXC manages IPv4/NAT (like lxdbr0)
    IPv4Address         string `json:"ipv4_address"`         // e.g. "10.10.10.1/24" or "none"
    IPv4NAT             bool   `json:"ipv4_nat"`
    IPv6Address         string `json:"ipv6_address"`         // or "none"
    IPv6NAT             bool   `json:"ipv6_nat"`
    // For "vlan" or "plain": uses a physical or VLAN interface
    ParentInterface     string `json:"parent_interface"`     // e.g. "enxa0cec8cd42e7"
    VLANTag             int    `json:"vlan_tag"`             // 0 = no VLAN, >0 = create VLAN sub-interface
    // /etc/network/interfaces stanza written only when VLANTag > 0
}

type LXDNetworkEditRequest struct {
    Name        string            `json:"name"`
    Description string            `json:"description"`
    Config      map[string]string `json:"config"`
}
```

**Functions:**
- `ListLXDNetworks() ([]LXDNetwork, error)` — `lxc network list --format json`, computes `VMCount`
- `GetLXDNetwork(name) (LXDNetwork, error)` — `lxc network show <name> --format json`
- `CreateLXDNetwork(req LXDNetworkCreateRequest) error`:
  1. If `VLANTag > 0`: write VLAN stanza to `/etc/network/interfaces` via `sudo tee`, then `sudo ifup <vlan-iface>` to bring it up immediately
  2. Run `lxc network create <name> --type bridge [config keys...]`
- `EditLXDNetwork(req LXDNetworkEditRequest) error` — `lxc network set <name> <key> <val>` per changed config key + description patch via `lxc network edit`
- `DeleteLXDNetwork(name string) error`:
  1. `lxc network delete <name>`
  2. If the bridge had a VLAN interface entry in `/etc/network/interfaces`, remove that stanza and call `sudo ifdown <vlan-iface>` (best-effort)
- `ListPhysicalInterfaces() ([]string, error)` — reads `/proc/net/dev`, filters out lo/lxd*/veth*/tap* — used to populate the "Parent Interface" picker in the create modal

### New file: `handlers/lxdnet.go`

Routes (all under `RequireAuth`, mutations under `RequireAdmin` or `RequirePermission("manage_nfs")` — reuse `manage_interlink` or create `manage_networking` perm):

```
GET  /api/lxd/networks              → HandleListLXDNetworks
GET  /api/lxd/networks/{name}       → HandleGetLXDNetwork
POST /api/lxd/networks              → HandleCreateLXDNetwork   (admin/manage_networking)
PUT  /api/lxd/networks/{name}       → HandleEditLXDNetwork     (admin/manage_networking)
DELETE /api/lxd/networks/{name}     → HandleDeleteLXDNetwork   (admin/manage_networking)
GET  /api/lxd/physical-interfaces   → HandleListPhysicalInterfaces
```

### `handlers/router.go` additions
Register the 6 routes above.

### `system/sudoers_hardening.go` + `SECURITY.md`
New alias `ZFSNAS_LXDNET`:
```
Cmnd_Alias ZFSNAS_LXDNET = /usr/sbin/ifup *, /usr/sbin/ifdown *, /usr/bin/tee /etc/network/interfaces
```
Add `ZFSNAS_LXDNET` to the grant line.

---

## Frontend

### `static/style.css`

**Mode bar — icons only, no border:**
Replace current `.sidebar-mode-btn` with:
```css
.sidebar-mode-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 32px; height: 32px;
  padding: 0;
  background: none;
  border: none;
  border-radius: 6px;
  color: var(--text-3);
  cursor: pointer;
  transition: background .15s, color .15s;
}
.sidebar-mode-btn:hover  { background: var(--surface2); color: var(--text); }
.sidebar-mode-btn.active { background: var(--surface2); color: var(--accent); }
```

### `static/index.html`

#### Mode bar HTML (line ~310)
Remove text nodes "Storage" and "Compute". Add networking button.
```html
<div class="sidebar-mode-selector" id="sidebar-mode-selector" style="display:none;">
  <button class="sidebar-mode-btn active" id="mode-btn-storage"
          onclick="switchSidebarMode('storage')" title="Storage">
    <!-- storage stack SVG (existing) -->
  </button>
  <button class="sidebar-mode-btn" id="mode-btn-compute"
          onclick="switchSidebarMode('compute')" title="Compute">
    <!-- chip SVG (existing) -->
  </button>
  <button class="sidebar-mode-btn" id="mode-btn-networking"
          onclick="switchSidebarMode('networking')" title="Networking">
    <!-- network/switch SVG — 3 nodes connected -->
    <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
      <path d="M6.5 1a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0zm1.5 1a.5.5 0 1 1 0-1 .5.5 0 0 1 0 1zM1 7.5a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0zm1.5 1a.5.5 0 1 1 0-1 .5.5 0 0 1 0 1zm9.5-1a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0zm1.5 1a.5.5 0 1 1 0-1 .5.5 0 0 1 0 1zM8 13.5a1.5 1.5 0 1 0 3 0 1.5 1.5 0 0 0-3 0zm1.5 1a.5.5 0 1 1 0-1 .5.5 0 0 1 0 1zM8 1.5v5m-5.5 1h11M8 6.5l-5.5 1M8 6.5l5.5 1M8 6.5v7"/>
    </svg>
  </button>
</div>
```
*(exact SVG path to be chosen at implementation — use a clean network topology icon)*

#### Networks sidebar section (after nav-compute-section)
```html
<div id="nav-networking-section" style="display:none;">
  <div class="sidebar-section" style="cursor:pointer;user-select:none;"
       onclick="showPage('lxd-networks')">Networks</div>
</div>
```

#### Networks page (new `<div class="page hidden" id="page-lxd-networks">`)
```html
<div class="page hidden" id="page-lxd-networks">
  <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:20px;">
    <h2 style="margin:0;">Networks</h2>
    <button class="btn btn-primary btn-sm" onclick="openCreateBridgeModal()">+ Create Bridge</button>
  </div>
  <div class="card" style="padding:0;overflow:hidden;">
    <table class="data-table" id="lxd-networks-table">
      <thead>
        <tr>
          <th>Name</th>
          <th>Type</th>
          <th>IPv4</th>
          <th>IPv6</th>
          <th>VMs</th>
          <th>Description</th>
          <th></th>  <!-- actions -->
        </tr>
      </thead>
      <tbody id="lxd-networks-tbody"></tbody>
    </table>
  </div>
</div>
```

Each tbody row:
- Columns: name (monospace), type badge, IPv4, IPv6, VM count pill, description (grey if empty)
- Actions column: `Edit` button + 3-dot burger with one item "Delete" (greyed + `title="In use by N VM(s)"` if `vm_count > 0`)
- Only show `Edit` / Delete for `managed === true` networks (unmanaged ones like `vmbr0`, `wlp0s20f3` shown read-only, no actions)

#### Create Bridge modal (`modal-create-bridge`)
Fields:
1. **Name** — text input, validated `^[a-z][a-z0-9-]{0,14}$`
2. **Description** — text input (optional)
3. **Bridge type** radio/select:
   - `nat` — LXC-managed with IPv4 NAT (like lxdbr0); reveals IPv4 CIDR + IPv6 toggle
   - `vlan` — VLAN-backed external bridge; reveals Parent Interface picker + VLAN ID
   - `plain` — plain external bridge (no VLAN); reveals Parent Interface picker
4. **nat** sub-fields: IPv4 Address (e.g. `10.10.10.1/24`), IPv4 NAT toggle (default on), IPv6 Address (optional, default `none`)
5. **vlan/plain** sub-fields: Parent Interface `<select>` (populated from `/api/lxd/physical-interfaces`), VLAN ID (vlan only, 1–4094)

#### Edit Bridge modal (`modal-edit-bridge`)
Fields:
1. **Description** — text input
2. **Config table** — shows current `config` key-value pairs, each row editable; "+ Add key" button for power users
3. Read-only info section showing current `used_by` instances

#### Delete confirmation
Standard danger modal: "Delete bridge `<name>`? This will remove the LXC network. If a VLAN interface was created by ZNAS, it will also be removed from `/etc/network/interfaces`."

---

## `switchSidebarMode` update
```js
function switchSidebarMode(mode) {
  if (mode === 'storage') _stopCIStatsPolling();
  _sidebarMode = mode;
  ['storage','compute','networking'].forEach(m => {
    document.getElementById('mode-btn-' + m).classList.toggle('active', m === mode);
  });
  document.getElementById('nav-group-storage-sharing').style.display  = mode === 'storage'    ? '' : 'none';
  document.getElementById('nav-virt-section').style.display           = 'none';
  document.getElementById('nav-lxd').style.display                    = 'none';
  document.getElementById('nav-compute-section').style.display        = mode === 'compute'    ? '' : 'none';
  document.getElementById('nav-networking-section').style.display     = mode === 'networking' ? '' : 'none';
  if (mode === 'compute') {
    refreshComputeNav();
    const activePage = document.querySelector('.page:not(.hidden)');
    if (activePage && !['page-dashboard','page-capacity','page-lxd','page-compute-instance'].includes(activePage.id)) {
      switchToLXDTable();
    }
  }
  if (mode === 'networking') {
    showPage('lxd-networks');
  }
}
```

---

## Interfaces file manipulation — `/etc/network/interfaces`

Pattern observed on the server for a VLAN-backed bridge:

**`/etc/network/interfaces` stanza (written by ZNAS for VLAN sub-interface):**
```
auto vmbr0-<VID>
iface vmbr0-<VID> inet manual
    pre-up ip link add link <parent> name vmbr0-<VID> type vlan id <VID>
    post-down ip link del vmbr0-<VID>
```

**LXC network (managed bridge that uses the VLAN sub-interface as external):**
```
lxc network create br-vlan<VID> \
  bridge.external_interfaces=vmbr0-<VID> \
  ipv4.address=none ipv6.address=none
```

ZNAS writes the `interfaces` stanza then calls `sudo ifup vmbr0-<VID>` to bring it up immediately without reboot.

For cleanup (delete): ZNAS removes the stanza from `/etc/network/interfaces` using `sudo tee` (full file rewrite), calls `sudo ifdown vmbr0-<VID>`, then `lxc network delete br-vlan<VID>`.

**Important:** Only remove the interfaces stanza if ZNAS created it. Track this with a comment marker in the stanza:
```
# znas-managed vlan
auto vmbr0-500
iface vmbr0-500 inet manual
    ...
# end znas-managed vlan
```

---

## Permissions

Add `manage_networking` to `StandardPermissions` struct (alongside existing perm flags). Default: false. Shown in create/edit user modal permissions panel.

---

## Files Changed / Created

| File | Change |
|------|--------|
| `system/lxdnet.go` | **new** — ListLXDNetworks, GetLXDNetwork, CreateLXDNetwork, EditLXDNetwork, DeleteLXDNetwork, ListPhysicalInterfaces |
| `handlers/lxdnet.go` | **new** — 6 HTTP handlers |
| `handlers/router.go` | add 6 routes |
| `system/sudoers_hardening.go` | add ZFSNAS_LXDNET alias |
| `SECURITY.md` | add ZFSNAS_LXDNET alias |
| `internal/config/config.go` | add `ManageNetworking bool` to `StandardPermissions` |
| `static/style.css` | restyle `.sidebar-mode-btn` (icons only, no border) |
| `static/index.html` | mode bar HTML, networking sidebar, Networks page, Create/Edit/Delete modals, `switchSidebarMode` update |

---

## Out of scope for 6.4.19
- Bond / team interfaces
- VXLAN / overlay networks
- Firewall / iptables rules management
- DNS configuration per bridge
