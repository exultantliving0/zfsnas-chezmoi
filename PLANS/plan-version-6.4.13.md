# Plan — Version 6.4.13

## Overview

Three encryption UX improvements for datasets:

1. **Lock / Unlock dataset actions** — 3-dot menu on encrypted datasets gets contextual Lock or Unlock items.
2. **Edit Dataset — Advanced Security section** — collapsed section lets the user re-assign the key source to a different stored key or switch to "Ask User" (passphrase at unlock time, key never stored on server).
3. **Create Dataset — "Stored Keys" + "Generate Client Key"** — rename "Managed Keys" to "Stored Keys"; add a new key-source option that generates a one-time-displayed hex key the user must save themselves; dataset created with `keylocation=prompt` (no server-side key file).

---

## Background / Current State

- Datasets store `Encrypted bool` and `KeyLocked bool` (from `keystatus` ZFS property).
- Stored keys live in `config/keys/<uuid>.key` (raw 32-byte files); metadata in `config/encryption_keys.json`.
- `autoLoadEncryptionKeys()` at startup only loads keys whose `keylocation` is `file://…`; any dataset with `keylocation=prompt` is naturally skipped at boot.
- Existing `POST /api/datasets/{path}/load-key` accepts `{"key_id": "<uuid>"}` and loads a stored key file.
- Lock/Unlock exist for **pools** (`/api/pool/load-key`, `/api/pool/unload-key`) but not yet for individual **datasets**.
- The Edit Dataset modal currently only shows a yellow warning banner when the dataset's key is locked — it has no section to change the key source.
- The Create Dataset modal has an "Advanced Security" section with an Encryption dropdown and a "Encryption Key" select (stored keys only).
- `zfs load-key` and `zfs unload-key` are already covered by `sudo zfs *` in sudoers — **no sudoers changes needed**.

---

## Feature 1 — Lock / Unlock Dataset Actions (3-dot menu)

### Logic

For each non-root dataset row, the 3-dot menu (`toggleDsMenu`) gains context-sensitive lock actions:
- Dataset is **encrypted + unlocked** (`d.encrypted && !d.key_locked`) → show **"🔒 Lock Dataset"**
- Dataset is **encrypted + locked** (`d.encrypted && d.key_locked`) → show **"🔓 Unlock Dataset"**
- Dataset is not encrypted → no lock actions

### Lock Action

Calls new handler: `POST /api/datasets/{path}/unload-key` (no body needed).

**`system/zfs.go` — add:**
```go
// UnloadDatasetKey unloads the encryption key for a dataset (locks it).
// The dataset is unmounted first; if unmount fails, the lock is still attempted.
func UnloadDatasetKey(name string) error {
    // Best-effort unmount; ZFS requires the dataset to be unmounted before unload-key.
    _ = exec.Command("sudo", "zfs", "umount", name).Run()
    out, err := exec.Command("sudo", "zfs", "unload-key", name).CombinedOutput()
    if err != nil {
        return fmt.Errorf("zfs unload-key: %s", strings.TrimSpace(string(out)))
    }
    return nil
}
```

**`handlers/datasets.go` — add `HandleUnloadDatasetKey`:**
- Validate path, call `system.UnloadDatasetKey(path)`.
- Audit `ActionLockDataset`.
- Return updated dataset JSON.

### Unlock Action

**Two unlock paths depending on `keylocation`:**

1. `keylocation=file://…` (stored key) — current `HandleLoadDatasetKey` works, but UI must look up the right key automatically or let user pick one.
2. `keylocation=prompt` (client key) — new modal asks user to type/paste their key hex.

**Unlock popup logic:**

When "🔓 Unlock Dataset" is clicked, `openUnlockDatasetModal(dsName)` is called:
- Fetch `GET /api/datasets/{path}/key-info` to get `keylocation`.
- If `keylocation` starts with `file://` → show stored-key picker (dropdown of stored keys) → calls existing `POST /api/datasets/{path}/load-key`.
- If `keylocation == "prompt"` → show passphrase input modal → calls new `POST /api/datasets/{path}/unlock-passphrase`.

**`system/zfs.go` — add:**
```go
// LoadDatasetKeyPassphrase loads an encryption key by piping the given
// passphrase/hex-key string to `zfs load-key` via stdin.
// The passphrase is never written to disk.
func LoadDatasetKeyPassphrase(name, passphrase string) error {
    cmd := exec.Command("sudo", "zfs", "load-key", name)
    cmd.Stdin = strings.NewReader(passphrase + "\n")
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("zfs load-key: %s", strings.TrimSpace(string(out)))
    }
    return nil
}
```

**`handlers/datasets.go` — add `HandleUnlockDatasetPassphrase`:**
- Body: `{"passphrase": "..."}` — trimmed but never logged in full.
- Calls `system.LoadDatasetKeyPassphrase(path, req.Passphrase)`.
- Then `system.MountDataset(path)` and `system.MountUnlockedChildren(path)`.
- Audit `ActionUnlockDataset` (target = dataset name, no passphrase in details).
- Returns `{"message": "dataset unlocked and mounted"}`.

**`handlers/datasets.go` — add `HandleGetDatasetKeyInfo`:**
- Returns `{"keylocation": "prompt|file://…", "key_locked": true|false}`.
- Used by the unlock modal to decide which unlock path to show.

### New Routes (router.go)

```
POST /api/datasets/{path:.+}/unload-key     → HandleUnloadDatasetKey
POST /api/datasets/{path:.+}/unlock-passphrase → HandleUnlockDatasetPassphrase
GET  /api/datasets/{path:.+}/key-info       → HandleGetDatasetKeyInfo
```

### Audit Constants (internal/audit/audit.go)

```go
ActionLockDataset   = "lock_dataset"
ActionUnlockDataset = "unlock_dataset"
```

### Frontend — Unlock Passphrase Modal

New modal `modal-unlock-dataset-passphrase` (max-width 440px):
- Header: "🔓 Unlock Dataset" + dataset name
- Warning banner (yellow): "This key is not stored on ZNAS. Type or paste the key you saved when this dataset was created. It will only be used once and will not be saved."
- Input: `<input type="password" id="unlock-ds-passphrase" placeholder="Paste your key here…">`  
  With a "👁 show/hide" toggle button.
- Footer: Cancel + "Unlock" button.

### Frontend — Stored Key Unlock in Unlock Modal

When keylocation is `file://`, show the same interface as the current edit-dataset locked-key banner: a `<select>` of stored keys + "Manage Keys" button, then an "Unlock & Mount" button.

---

## Feature 2 — Edit Dataset: Advanced Security Section

### Goal

When editing an encrypted dataset, a collapsed "Advanced Security" section (styled like the create-dataset one) lets the user change which key is used to unlock this dataset:

- **Stored Keys** — pick any key from the keystore; saves `keylocation=file://…`.
- **Ask User** — switches to `keylocation=prompt`; ZNAS will not auto-mount at boot; the user must use the "Unlock Dataset" action and type their key.

> Note: This section is only shown when `d.encrypted === true`. It is NOT shown if the key is currently locked (the yellow lock-warning section takes precedence; user must unlock first, then edit).

### Backend — `HandleSetDatasetKeySource`

New handler: `PUT /api/datasets/{path}/key-source`

Request body:
```json
{"key_source": "stored", "key_id": "<uuid>"}
// or
{"key_source": "prompt"}
```

Logic:
- `key_source == "stored"`:
  - Validate key exists in keystore.
  - `zfs set keylocation=file://<keypath> <dataset>`
  - `zfs set canmount=on <dataset>`
- `key_source == "prompt"`:
  - `zfs set keylocation=prompt <dataset>`
  - `zfs set canmount=noauto <dataset>` — prevents mount at boot since key can't be auto-loaded.

Both paths also call `zfs set keyformat=raw` or `zfs set keyformat=hex` as appropriate. Actually, keyformat is set at dataset creation and cannot be changed without re-encrypting — **do not change keyformat**, only change keylocation.

Audit: `ActionChangeKeySource` with details `"prompt"` or `"stored:<key-name>"`.

**`internal/audit/audit.go`:**
```go
ActionChangeKeySource = "change_key_source"
```

**New route:**
```
PUT /api/datasets/{path:.+}/key-source → HandleSetDatasetKeySource
```

### Frontend — Edit Dataset Modal

After the existing compression/sync/dedup/recordsize fields and before the comment textarea, add:

```html
<!-- Advanced Security (key management) — only for encrypted datasets -->
<div id="eds-adv-sec" style="display:none;margin-top:12px;border:1px solid var(--border);border-radius:6px;overflow:hidden;">
  <div style="display:flex;align-items:center;gap:8px;padding:10px 12px;background:var(--surface2);cursor:pointer;" onclick="toggleEDSAdvSec()">
    <span>🔒</span>
    <span style="font-weight:600;flex:1;">Advanced Security</span>
    <span id="eds-advsec-chevron" style="font-size:0.8em;color:var(--text3);">▶</span>
  </div>
  <div id="eds-advsec-body" style="display:none;padding:12px;border-top:1px solid var(--border);">
    <div class="form-group" style="margin-bottom:8px;">
      <label>Encryption Key Source</label>
      <select id="eds-key-source" onchange="onEDSKeySourceChange()" style="width:100%;">
        <option value="prompt">Ask User — enter key at unlock (not stored on ZNAS)</option>
        <!-- stored keys populated dynamically -->
      </select>
    </div>
    <div id="eds-key-source-note" class="alert" style="display:none;margin-bottom:0;font-size:12px;"></div>
    <div style="margin-top:10px;">
      <button type="button" class="btn btn-primary btn-sm" onclick="saveEDSKeySource()">Apply Key Source</button>
    </div>
  </div>
</div>
```

**JS:**
- `openEditDataset()` — if `d.encrypted && !d.key_locked`: show `#eds-adv-sec`, populate dropdown with "Ask User" + stored keys; pre-select current keylocation (fetch from `/api/datasets/{path}/key-info`).
- If `d.encrypted && d.key_locked`: hide `#eds-adv-sec` (show yellow lock banner instead, as before).
- `onEDSKeySourceChange()` — when "Ask User" is selected, show an informational note: *"The dataset will not auto-mount at boot. You will need to unlock it manually via the 'Unlock Dataset' action."*
- `saveEDSKeySource()` — PUT to `/api/datasets/{path}/key-source`, show inline success/error, close section on success.

---

## Feature 3 — Create Dataset: "Stored Keys" + "Generate Client Key"

### UI Changes in Create Dataset Modal

**Rename "Managed Keys" → "Stored Keys"** everywhere it appears as a label (this is only a frontend label change; the underlying API and key store are unchanged).

**Add "Generate Client Key" option to the key source dropdown:**

The "Encryption Key" select currently lists stored keys. Change it to:

```html
<select id="cds-enc-key" onchange="onCDSKeyChange()" style="flex:1;">
  <option value="">— select a key —</option>
  <option value="__client__">✦ Generate Client Key (you keep it)</option>
  <!-- stored keys populated dynamically -->
</select>
```

**When `__client__` is selected:**
- Hide the "Manage Keys" button.
- Show a new panel `#cds-client-key-panel` below the dropdown:

```html
<div id="cds-client-key-panel" style="display:none;margin-top:10px;border:1px solid var(--accent-warn);border-radius:6px;padding:12px;background:rgba(240,180,41,0.07);">
  <div style="font-weight:600;color:var(--accent-warn);margin-bottom:8px;">⚠ Your Responsibility</div>
  <p style="font-size:13px;margin:0 0 10px 0;line-height:1.5;">
    ZNAS will <strong>not store this key</strong>. If you lose it, <strong>your data will be permanently inaccessible</strong>.
    Save this key in a password manager or secure vault before continuing.
  </p>
  <div style="display:flex;align-items:center;gap:8px;margin-bottom:10px;">
    <code id="cds-generated-key" style="flex:1;font-size:12px;word-break:break-all;background:var(--surface2);padding:6px 8px;border-radius:4px;border:1px solid var(--border);user-select:all;"></code>
    <button type="button" class="btn btn-ghost btn-sm" onclick="copyCDSGeneratedKey()">Copy</button>
    <button type="button" class="btn btn-ghost btn-sm" onclick="regenerateCDSClientKey()" title="Generate a new key">↻</button>
  </div>
  <label style="display:flex;align-items:center;gap:8px;font-size:13px;cursor:pointer;">
    <input type="checkbox" id="cds-key-saved-cb" onchange="updateCDSSubmitState()">
    <span>I have saved this key securely and understand ZNAS cannot recover it.</span>
  </label>
</div>
```

- **Key generation**: done client-side via `crypto.getRandomValues(new Uint8Array(32))`, hex-encoded. Called when `__client__` is first selected (and on "↻ regenerate" click). Never sent to the server until the "Create" button is pressed.
- **"Create" button** is disabled until:
  - `key_source == "__client__"` → the `#cds-key-saved-cb` checkbox must be checked.
  - `key_source == a stored key ID` → key must be selected (existing validation).

### Backend Changes — `HandleCreateDataset`

Add `ClientKeyHex string` to the request struct.

When `ClientKeyHex != ""`:
- Validate it is a 64-character hex string (32 bytes).
- Do NOT store it in keystore.
- Call `system.CreateDatasetWithClientKey(name, opts, clientKeyHex)`.

**`system/zfs.go` — extend `DatasetCreateOptions`:**
```go
type DatasetCreateOptions struct {
    // ... existing fields ...
    KeyFilePath   string // stored key: absolute path; mutually exclusive with ClientKeyHex
    ClientKeyHex  string // client key: 64-char hex, passed via stdin; not stored on disk
}
```

**`system/zfs.go` — modify `CreateDataset`:**
```go
if opts.KeyFilePath != "" {
    args = append(args, "-o", "encryption=aes-256-gcm", "-o", "keyformat=raw",
        "-o", "keylocation=file://"+opts.KeyFilePath)
} else if opts.ClientKeyHex != "" {
    args = append(args, "-o", "encryption=aes-256-gcm", "-o", "keyformat=hex",
        "-o", "keylocation=prompt")
    cmd.Stdin = strings.NewReader(opts.ClientKeyHex + "\n")
}
```

### Dataset Behaviour After Creation with Client Key

- `keylocation=prompt` → `autoLoadEncryptionKeys()` skips it at boot (already checks `file://` prefix).
- Dataset is mounted at creation time (ZFS does this automatically after `zfs create` with `keylocation=prompt` when the key is supplied via stdin).
- On next reboot the dataset will be locked. User unlocks via the new "🔓 Unlock Dataset" action and types their key.

---

## SECURITY.md Update

Add a new section documenting:
- **Client Keys** — generated in the browser, displayed once, never sent to or stored on ZNAS. Lost key = permanent data loss.
- **keylocation=prompt datasets** — not auto-mounted at boot. Must be unlocked manually from the ZNAS UI.
- **Passphrase unlock** — key typed in UI is sent over HTTPS to ZNAS, piped directly to `zfs load-key`, discarded immediately after; never written to disk, logs, or audit entries.

The SECURITY.md note for these features does **not** require new sudoers entries since `zfs *` already covers `load-key` and `unload-key`.

---

## File Change Summary

| File | Change |
|------|--------|
| `system/zfs.go` | Add `UnloadDatasetKey()`, `LoadDatasetKeyPassphrase()`; extend `DatasetCreateOptions.ClientKeyHex`; update `CreateDataset()` to handle client key via stdin |
| `handlers/datasets.go` | Add `HandleUnloadDatasetKey`, `HandleUnlockDatasetPassphrase`, `HandleGetDatasetKeyInfo`, `HandleSetDatasetKeySource` |
| `handlers/router.go` | Register 4 new routes |
| `internal/audit/audit.go` | Add `ActionLockDataset`, `ActionUnlockDataset`, `ActionChangeKeySource` |
| `static/index.html` | `toggleDsMenu()` — add lock/unlock items; new passphrase unlock modal; Edit Dataset Advanced Security section; Create Dataset rename + client-key panel |
| `SECURITY.md` | New section on client keys, prompt keylocation, passphrase unlock behaviour |

---

## Implementation Order

1. `system/zfs.go` — new functions + `DatasetCreateOptions` extension
2. `internal/audit/audit.go` — new action constants
3. `handlers/datasets.go` — 4 new handlers
4. `handlers/router.go` — register routes
5. `static/index.html` — all frontend changes
6. `SECURITY.md` — documentation
7. Build + deploy
