# Plan: Add Spare Disk — v6.4.13 extension

## Overview
Users can designate available disks as ZFS hot spares for a pool.
- "Add Spare" button next to "Add Capacity" in the Pool tab
- Spare disks listed in the physical disks table with a "Spare" badge
- "Remove Spare" burger menu item on spare disks

---

## 1. Backend — system/zfs.go

### 1a. Pool struct — add spare fields
```go
SpareDevices  []string `json:"spare_devices"`  // resolved /dev/sdX paths
SpareStatuses []string `json:"spare_statuses"` // "AVAIL" | "INUSE" | "FAULTED"
SparePresent  []bool   `json:"spare_present"`  // device exists in /dev
```

### 1b. Parse spares from `zpool status`
The existing parser has a `skipSections` map that skips "spares". Remove "spares" from the skip list and add a spare-parsing branch (same approach as cache: collect device names between "spares:" header and next section). Resolve raw names to /dev/sdX via `resolveDevice()` (already used for caches).

### 1c. New functions
```go
// AddPoolSpare adds a disk as a hot spare: zpool add <pool> spare <disk>
func AddPoolSpare(pool, disk string) error

// RemovePoolSpare removes a spare: zpool remove <pool> <disk>
func RemovePoolSpare(pool, disk string) error
```

---

## 2. Backend — handlers/pools.go

### 2a. HandleAddPoolSpare
`POST /api/pool/spare`
Body: `{"pool": "...", "disk": "/dev/sdX"}`
- Validate disk is not already a pool member or cache
- Call `system.AddPoolSpare(pool, disk)`
- Audit log entry

### 2b. HandleRemovePoolSpare
`DELETE /api/pool/spare`
Body: `{"pool": "...", "disk": "/dev/sdX"}`
- Call `system.RemovePoolSpare(pool, disk)`
- Audit log entry

---

## 3. Backend — handlers/router.go
```
POST   /api/pool/spare   → HandleAddPoolSpare
DELETE /api/pool/spare   → HandleRemovePoolSpare
```
Both require admin.

---

## 4. Backend — system/prereqs.go (sudoers)
No new sudo entries needed — `zpool *` is already covered by the wildcard entry.

---

## 5. Frontend — static/index.html

### 5a. "Add Spare" button
Placed immediately below the existing "+ Add Capacity" button (same pool tab cache/capacity section):
```html
<button ... onclick="openAddSpareModal()">+ Add Spare Disk</button>
```
Style: similar grey/neutral tone (not green like capacity, not blue like cache — use `--accent-3` amber/yellow to signal "standby" device).

### 5b. Add Spare Modal (`modal-add-spare`)
Simple modal, ~380px:
- Title: "Add Spare Disk"
- Info line: "A hot spare will automatically replace a failed vdev member. It is not used for storage until needed."
- Dropdown: available disks (disks not in any pool, not cache) — same `_availableDisks` list used by the disk wipe modal
- Submit → `POST /api/pool/spare`

### 5c. Physical disks table — spare rows
The `_diskEntry()` function (line ~8906) drives per-disk rows. After rendering pool member rows, add a loop over `pool.spare_devices` to render spare rows:
- Role badge: amber "Spare" pill (instead of mirror/raidz role)
- Status badge: "AVAIL" (green), "INUSE" (yellow), or "FAULTED" (red)
- `spare_present` drives greyed-out style when disk absent
- Burger menu: single item "✕ Remove Spare" → calls `removeSpare(pool, disk)`

### 5d. JS functions
- `openAddSpareModal()` — populate disk dropdown from `_availableDisks`, show modal
- `closeAddSpareModal()`
- `submitAddSpare(event)` — POST, reload disks/pool on success
- `removeSpare(pool, disk)` — DELETE, show confirmation first (styled modal, not browser confirm), reload on success

---

## Disk availability logic
A disk is "available for spare" if it does not appear in any pool's `member_devices`, `cache_devices`, OR `spare_devices`. Reuse the existing `_availableDisks` cache (already filtered for pool members and caches); after this feature ships it will also filter out spare_devices.

---

## Out of scope
- Spare replacement policy configuration (ZFS handles this automatically)
- Showing spare-in-use replacement progress (covered by the existing pool health/fixer views)
