# Plan — Version 6.4.15

## Overview

Three enhancements to the **ZFSNAS Instant Update** card in Settings → Updates:

1. **"(Stable)" channel label** — appended to the Current and Latest version numbers.
2. **Release Notes button** — opens a polished popup showing the last 5 GitHub releases with full release notes rendered from the GitHub release body.
3. **Instant Downgrade** — each release entry in the popup has a small downgrade button that drives the same WebSocket progress flow already used by the update path.

---

## Feature 1 — "(Stable)" channel label

### What it looks like

```
Current       Latest
v6.4.14 (Stable)    v6.4.15 (Stable)
```

The `(Stable)` text is a small muted label rendered immediately to the right of the version number, inside the same line. It is always shown (both rows, all states).

### Scope

**Pure frontend, no backend change.**

Every place that writes to `#bup-current` or `#bup-latest` must append the label:
- `_applyVerData(data)` inner function (auto-check on page open)
- `checkBinaryUpdate()` (manual check button)
- Initial placeholder — `bup-current` is pre-populated with `'v' + (appSettings.version || '…')` before any fetch; append `(Stable)` there too.

### Implementation

In `_applyVerData` and `checkBinaryUpdate`, replace bare `textContent` assignments with a helper:

```js
function _setVerLabel(el, ver) {
  el.innerHTML = `<span style="font-family:'SF Mono',monospace;font-weight:700;">${esc('v'+ver)}</span>`
    + `<span style="font-size:11px;color:var(--text-3);margin-left:6px;font-weight:500;">(Stable)</span>`;
}
```

Call `_setVerLabel(currentEl, data.current)` and `_setVerLabel(latestEl, data.latest)` instead of `textContent =`.

Note: `latestEl.style.color` on the wrapper `<div id="bup-latest">` already sets the colour of the monospace span when an update is available — that remains unchanged because we're targeting the inner `<span>`, not the container div. The container div colour change will tint both the version and the `(Stable)` label when `update_available` is true; that's acceptable and consistent.

---

## Feature 2 — Release Notes button & popup

### Button placement

The control bar below the version comparison table currently holds one button:

```html
<div style="padding:10px 18px;">
  <button … id="btn-check-binary-update">↻ Check for Update</button>
</div>
```

Add the Release Notes button on the **same row**, right-aligned with a flex spacer:

```html
<div style="padding:10px 18px;display:flex;align-items:center;justify-content:space-between;">
  <button … id="btn-check-binary-update">↻ Check for Update</button>
  <button class="btn btn-ghost btn-sm" onclick="openReleaseNotes()" id="btn-release-notes"
    style="font-size:12px;">📋 Release Notes</button>
</div>
```

### New backend endpoint

**`GET /api/binary-update/releases`** — admin-only, returns the last 5 releases from GitHub.

Add to `handlers/binary_update.go`:

```go
// HandleListReleases returns the last 5 GitHub releases with tag, name, body, and assets.
// GET /api/binary-update/releases
func HandleListReleases(appCfg *config.AppConfig) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        releases, err := updater.CheckReleases(5)
        if err != nil {
            jsonErr(w, http.StatusBadGateway, err.Error())
            return
        }
        jsonOK(w, releases)
    }
}
```

Register in `handlers/router.go` alongside the other binary-update routes:

```go
r.Handle("/api/binary-update/releases",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleListReleases(appCfg))))).Methods("GET")
```

### New updater function `CheckReleases`

Add to `internal/updater/updater.go`:

```go
const repoReleasesAPI = "https://api.github.com/repos/macgaver/zfsnas-chezmoi/releases"

// ReleaseSummary is a single GitHub release entry returned by CheckReleases.
// Prerelease is included so the UI can handle it if ever needed, but CheckReleases
// only ever returns stable (non-prerelease) entries — pre-release builds are
// always filtered out at the source.
type ReleaseSummary struct {
    Tag         string `json:"tag"`
    Name        string `json:"name"`
    Body        string `json:"body"`        // raw markdown from GitHub
    PublishedAt string `json:"published_at"` // RFC3339
    DownloadURL string `json:"download_url"`
    SigURL      string `json:"sig_url"`
    Prerelease  bool   `json:"prerelease"`  // always false — pre-releases are filtered out
}

// CheckReleases returns the latest n stable releases from GitHub.
// Pre-releases (GitHub prerelease flag == true) are always excluded, even if they
// appear at the top of the releases list. We request a larger page to guarantee
// n stable results are available after filtering.
func CheckReleases(n int) ([]ReleaseSummary, error) {
    url := fmt.Sprintf("%s?per_page=%d", repoReleasesAPI, n*3)
    resp, err := http.Get(url)
    if err != nil {
        return nil, fmt.Errorf("github API: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("github API returned status %d", resp.StatusCode)
    }

    var raw []struct {
        TagName     string `json:"tag_name"`
        Name        string `json:"name"`
        Body        string `json:"body"`
        PublishedAt string `json:"published_at"`
        Prerelease  bool   `json:"prerelease"`
        Assets      []struct {
            Name               string `json:"name"`
            BrowserDownloadURL string `json:"browser_download_url"`
        } `json:"assets"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
        return nil, fmt.Errorf("decode: %w", err)
    }

    suffix := "linux-" + runtime.GOARCH
    out := make([]ReleaseSummary, 0, n)
    for _, r := range raw {
        if r.Prerelease {
            continue // pre-releases are invisible to stable-channel users
        }
        s := ReleaseSummary{
            Tag:         r.TagName,
            Name:        r.Name,
            Body:        r.Body,
            PublishedAt: r.PublishedAt,
            Prerelease:  false,
        }
        for _, a := range r.Assets {
            switch {
            case strings.HasSuffix(a.Name, ".sig"):
                s.SigURL = a.BrowserDownloadURL
            case strings.Contains(a.Name, suffix):
                s.DownloadURL = a.BrowserDownloadURL
            }
        }
        out = append(out, s)
        if len(out) >= n {
            break
        }
    }
    return out, nil
}
```

Also add `CheckRelease(tag string) (ReleaseInfo, error)` for the downgrade path (see Feature 3 below).

### Release Notes modal — design

The modal is large (800 px max-width, up to 85 vh tall, scrollable body). It uses the existing `modal-overlay` pattern.

```
┌──────────────────────────────────────────────────────────┐
│  📋 Release Notes                              [✕ Close] │
│──────────────────────────────────────────────────────────│
│  ┌────────────────────────────────────────────────────┐  │
│  │  v6.4.15   Jun 12 2025         [⬇ Downgrade] [→]  │  │  ← collapsed accordion header
│  └────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────┐  │
│  │  v6.4.14   Jun 2 2025  ← Current  [⬇ Downgrade] [→]│  │  ← open (current version auto-expands)
│  │  ┌──────────────────────────────────────────────┐  │  │
│  │  │  ### Features                                 │  │  │
│  │  │  - RAIDZ-3 pool support                       │  │  │
│  │  │  …                                            │  │  │
│  │  └──────────────────────────────────────────────┘  │  │
│  └────────────────────────────────────────────────────┘  │
│  …                                                        │
└──────────────────────────────────────────────────────────┘
```

Details:
- **Accordion** — each release is a card/row. Clicking the header row toggles the body open/closed. The first release (latest) is expanded by default; the current installed version gets a `← Current` pill badge.
- **Release body** — rendered via a lightweight JS markdown-to-HTML converter (inline in `static/index.html`). Convert: `## Heading` → `<h4>`, `- item` → `<li>`, `**text**` → `<strong>`, `` `code` `` → `<code>`. No external library needed.
- **Date** — `published_at` formatted as "MMM D, YYYY" via `new Date(…).toLocaleDateString('en-US', {month:'short',day:'numeric',year:'numeric'})`.
- **Downgrade button** — styled as small amber button labeled `⬇ v6.4.14`; hidden for the latest release (no point "downgrading" to latest). See Feature 3.
- **Loading state** — while fetching, show a spinner row inside the modal body.
- **Error state** — if the fetch fails, show a red message with a retry button.
- The modal title bar shows "📋 Release Notes" on the left and a `✕` button on the right.

#### Modal HTML skeleton (static, in index.html alongside the other modals):

```html
<div id="modal-release-notes" class="modal-overlay hidden" onclick="if(event.target===this)closeReleaseNotes()">
  <div class="modal" style="max-width:800px;width:95vw;max-height:85vh;display:flex;flex-direction:column;">
    <div class="modal-header">
      <div style="display:flex;align-items:center;gap:10px;">
        <span style="font-size:18px;">📋</span>
        <h2 style="margin:0;">Release Notes</h2>
      </div>
      <button class="modal-close" onclick="closeReleaseNotes()">✕</button>
    </div>
    <div id="rn-body" style="flex:1;overflow-y:auto;padding:0 24px 24px;">
      <!-- populated by JS -->
    </div>
  </div>
</div>
```

#### JS functions in `static/index.html`:

```js
// ── Release Notes ──────────────────────────────────────────────────────────
let _rnReleases = null; // cache

function openReleaseNotes() {
  document.getElementById('modal-release-notes').classList.remove('hidden');
  _renderReleaseNotes();
}
function closeReleaseNotes() {
  document.getElementById('modal-release-notes').classList.add('hidden');
}

async function _renderReleaseNotes() {
  const body = document.getElementById('rn-body');
  if (_rnReleases) { _buildReleaseNotesDom(_rnReleases); return; }
  body.innerHTML = '<div style="padding:40px;text-align:center;color:var(--text-3);">Loading…</div>';
  try {
    const resp = await fetch('/api/binary-update/releases');
    if (!resp.ok) throw new Error((await resp.json()).error || 'fetch failed');
    _rnReleases = await resp.json();
    _buildReleaseNotesDom(_rnReleases);
  } catch(e) {
    body.innerHTML = `<div style="padding:24px;color:var(--accent-err);">Failed to load release notes: ${esc(e.message)}
      <button class="btn btn-ghost btn-sm" style="margin-left:12px;" onclick="_rnReleases=null;_renderReleaseNotes()">Retry</button></div>`;
  }
}

function _mdToHtml(md) {
  // Minimal Markdown → HTML (safe: no user input, comes from our own GitHub releases).
  return md
    .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')
    // Headings
    .replace(/^### (.+)$/gm, '<h5 style="margin:12px 0 4px;font-size:13px;color:var(--text-1);">$1</h5>')
    .replace(/^## (.+)$/gm,  '<h4 style="margin:14px 0 6px;font-size:14px;color:var(--text-1);">$1</h4>')
    .replace(/^# (.+)$/gm,   '<h3 style="margin:16px 0 8px;font-size:15px;color:var(--text-1);">$1</h3>')
    // Bold + italic
    .replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>')
    .replace(/\*(.+?)\*/g,     '<em>$1</em>')
    // Inline code
    .replace(/`([^`]+)`/g, '<code style="font-family:monospace;background:var(--surface2);padding:1px 5px;border-radius:3px;font-size:12px;">$1</code>')
    // Bullet lists
    .replace(/^- (.+)$/gm, '<li style="margin:2px 0;">$1</li>')
    .replace(/(<li[^>]*>.*<\/li>\n?)+/g, s => '<ul style="margin:6px 0 6px 18px;padding:0;list-style:disc;">'+s+'</ul>')
    // Paragraphs (blank lines)
    .replace(/\n{2,}/g, '</p><p style="margin:6px 0;">')
    .replace(/^(.)/,'<p style="margin:6px 0;">$1').replace(/(.)$/,'$1</p>');
}

function _buildReleaseNotesDom(releases) {
  const body    = document.getElementById('rn-body');
  const current = (appSettings && appSettings.version) ? appSettings.version : '';
  let html = '';
  releases.forEach((r, i) => {
    const tag        = r.tag.replace(/^v/,'');
    const isCurrent  = tag === current;
    const isLatest   = i === 0;
    const dateStr    = r.published_at
      ? new Date(r.published_at).toLocaleDateString('en-US',{month:'short',day:'numeric',year:'numeric'})
      : '';
    const canDowngrade = !isLatest; // can downgrade to any release that is not the current latest

    html += `
    <div style="border:1px solid var(--border);border-radius:var(--radius-sm);margin-top:12px;overflow:hidden;">
      <div onclick="toggleRnSection(${i})" style="display:flex;align-items:center;gap:10px;padding:12px 16px;cursor:pointer;background:var(--surface2);user-select:none;">
        <span style="font-family:'SF Mono',monospace;font-weight:700;font-size:14px;flex-shrink:0;">v${esc(tag)}</span>
        ${isCurrent ? '<span style="font-size:11px;font-weight:600;padding:2px 8px;border-radius:10px;background:rgba(48,209,88,0.15);color:var(--accent-3);flex-shrink:0;">← Current</span>' : ''}
        ${isLatest  ? '<span style="font-size:11px;font-weight:600;padding:2px 8px;border-radius:10px;background:rgba(10,132,255,0.15);color:var(--accent-2);flex-shrink:0;">Latest</span>' : ''}
        <span style="font-size:12px;color:var(--text-3);flex:1;">${esc(dateStr)}</span>
        ${canDowngrade ? `<button class="btn btn-sm" onclick="event.stopPropagation();confirmDowngrade('${esc(r.tag)}','${esc(tag)}')"
          style="font-size:11px;padding:3px 10px;background:rgba(255,159,10,0.15);color:var(--accent-warn);border:1px solid rgba(255,159,10,0.3);font-weight:600;flex-shrink:0;">
          ⬇ Downgrade</button>` : ''}
        <span id="rn-chevron-${i}" style="font-size:11px;color:var(--text-3);flex-shrink:0;transition:transform .2s;">${i===0?'▲':'▼'}</span>
      </div>
      <div id="rn-section-${i}" style="padding:${i===0?'16px':'0 16px'};max-height:${i===0?'9999px':'0'};overflow:hidden;transition:max-height .25s ease,padding .25s ease;">
        <div style="font-size:13px;color:var(--text-2);line-height:1.6;">${_mdToHtml(r.body || '_No description provided._')}</div>
      </div>
    </div>`;
  });
  body.innerHTML = html || '<div style="padding:24px;color:var(--text-3);">No releases found.</div>';
}

function toggleRnSection(i) {
  const sec = document.getElementById('rn-section-'+i);
  const chv = document.getElementById('rn-chevron-'+i);
  const isOpen = sec.style.maxHeight !== '0px' && sec.style.maxHeight !== '0';
  if (isOpen) {
    sec.style.maxHeight = '0';
    sec.style.padding   = '0 16px';
    if (chv) chv.textContent = '▼';
  } else {
    sec.style.maxHeight = '9999px';
    sec.style.padding   = '16px';
    if (chv) chv.textContent = '▲';
  }
}
```

---

## Feature 3 — Instant Downgrade

### Backend changes

#### 1. `updater.CheckRelease(tag string) (ReleaseInfo, error)`

Add to `internal/updater/updater.go`:

```go
const repoTagAPI = "https://api.github.com/repos/macgaver/zfsnas-chezmoi/releases/tags/"

// CheckRelease fetches a specific GitHub release by tag name (e.g. "v6.4.10").
func CheckRelease(tag string) (ReleaseInfo, error) {
    resp, err := http.Get(repoTagAPI + tag)
    if err != nil {
        return ReleaseInfo{}, fmt.Errorf("github API: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusNotFound {
        return ReleaseInfo{}, fmt.Errorf("release %s not found on GitHub", tag)
    }
    if resp.StatusCode != http.StatusOK {
        return ReleaseInfo{}, fmt.Errorf("github API returned status %d", resp.StatusCode)
    }

    var release struct {
        TagName string `json:"tag_name"`
        Assets  []struct {
            Name               string `json:"name"`
            BrowserDownloadURL string `json:"browser_download_url"`
        } `json:"assets"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
        return ReleaseInfo{}, fmt.Errorf("decode: %w", err)
    }

    info := ReleaseInfo{Tag: release.TagName}
    suffix := "linux-" + runtime.GOARCH
    for _, a := range release.Assets {
        switch {
        case strings.HasSuffix(a.Name, ".sig"):
            info.SigURL = a.BrowserDownloadURL
        case strings.Contains(a.Name, suffix):
            info.DownloadURL = a.BrowserDownloadURL
        }
    }
    return info, nil
}
```

#### 2. `HandleBinaryUpdateApply` — accept optional `?tag=` query param

In `handlers/binary_update.go`, modify the start of `HandleBinaryUpdateApply`:

```go
// If a specific tag is requested, target that release instead of the latest.
targetTag := strings.TrimSpace(r.URL.Query().Get("tag"))
var info updater.ReleaseInfo
if targetTag != "" {
    send("Fetching release info for " + targetTag + " from GitHub…")
    info, err = updater.CheckRelease(targetTag)
} else {
    send("Step 1/5: Fetching release info from GitHub…")
    info, err = updater.CheckLatest()
}
if err != nil {
    done(false, "fetch release info failed: "+err.Error())
    return
}
latest := strings.TrimPrefix(info.Tag, "v")
// Skip the "already up to date" guard when a specific tag is requested
// (user is intentionally downgrading).
if targetTag == "" && !semverGreater(latest, version.Version) {
    done(true, "already up to date (v"+version.Version+")")
    return
}
```

The remaining steps (sig verify, download, hash verify, replace, restart) are identical — no further changes needed there.

The step counter prefix in `send()` calls can stay as "Step N/5" — they are informational only.

### Frontend — confirm + downgrade flow

```js
function confirmDowngrade(tag, ver) {
  // Re-use the existing styled modal pattern — open a simple confirm modal.
  _rnDowngradeTag = tag;
  document.getElementById('rn-downgrade-ver').textContent = 'v' + ver;
  document.getElementById('modal-rn-downgrade-confirm').classList.remove('hidden');
}

function closeDowngradeConfirm() {
  document.getElementById('modal-rn-downgrade-confirm').classList.add('hidden');
}

function doDowngrade() {
  closeDowngradeConfirm();
  closeReleaseNotes();
  // Reuse the existing binary update WS flow, but targeting a specific tag.
  applyBinaryUpdate(_rnDowngradeTag);
}
```

**Modify `applyBinaryUpdate(tag = null)`** — add an optional `tag` param. When present, it is appended to the WS URL:

```js
async function applyBinaryUpdate(tag = null) {
  // ... existing sig-check guard (keep as-is for downgrades too — sig must be valid) ...

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsUrl = proto + '//' + location.host + '/ws/binary-update-apply'
    + (tag ? '?tag=' + encodeURIComponent(tag) : '');
  const ws = new WebSocket(wsUrl);
  // ... rest of existing WS handler unchanged ...
}
```

#### Downgrade confirm modal HTML (add alongside the release notes modal):

```html
<div id="modal-rn-downgrade-confirm" class="modal-overlay hidden" onclick="if(event.target===this)closeDowngradeConfirm()">
  <div class="modal" style="max-width:420px;">
    <div class="modal-header">
      <h2 style="margin:0;color:var(--accent-warn);">⚠ Confirm Downgrade</h2>
      <button class="modal-close" onclick="closeDowngradeConfirm()">✕</button>
    </div>
    <div style="padding:20px 24px 8px;">
      <p style="font-size:14px;color:var(--text-2);margin-bottom:16px;">
        You are about to downgrade ZFSNAS to <strong id="rn-downgrade-ver"></strong>.<br><br>
        This will replace the running binary and restart the portal. Make sure the older version is compatible with your current configuration.
      </p>
      <div style="display:flex;gap:10px;justify-content:flex-end;padding-bottom:16px;">
        <button class="btn btn-ghost" onclick="closeDowngradeConfirm()">Cancel</button>
        <button class="btn btn-sm" onclick="doDowngrade()"
          style="background:var(--accent-warn);color:#000;font-weight:700;border:none;">⬇ Downgrade Now</button>
      </div>
    </div>
  </div>
</div>
```

---

## File change summary

| File | Change |
|---|---|
| `internal/updater/updater.go` | Add `CheckReleases(n)`, `CheckRelease(tag)`, `ReleaseSummary` struct; add `repoReleasesAPI`, `repoTagAPI` constants |
| `handlers/binary_update.go` | Add `HandleListReleases`; modify `HandleBinaryUpdateApply` to accept `?tag=` query param |
| `handlers/router.go` | Register `GET /api/binary-update/releases` |
| `static/index.html` | `(Stable)` labels via `_setVerLabel()`; Release Notes button; `#modal-release-notes`; `#modal-rn-downgrade-confirm`; JS: `openReleaseNotes`, `closeReleaseNotes`, `_renderReleaseNotes`, `_buildReleaseNotesDom`, `_mdToHtml`, `toggleRnSection`, `confirmDowngrade`, `doDowngrade`; modify `applyBinaryUpdate(tag=null)` |

---

## Implementation order

1. `internal/updater/updater.go` — add the two new functions and structs
2. `handlers/binary_update.go` — add `HandleListReleases` + patch `HandleBinaryUpdateApply`
3. `handlers/router.go` — register new route
4. `static/index.html` — add `_setVerLabel` + patch all version label writes
5. `static/index.html` — add Release Notes button to the card
6. `static/index.html` — add `#modal-release-notes` modal HTML
7. `static/index.html` — add `#modal-rn-downgrade-confirm` modal HTML
8. `static/index.html` — add all new JS functions; patch `applyBinaryUpdate`
9. Build + deploy + smoke-test

---

## Pre-release Channel — Forward Compatibility

### Background

All releases published to date are stable. A future version will introduce a **Beta / Pre-release** update train. This section defines:

- The tag/release convention to adopt on GitHub when pre-releases are published
- The specific choices made in 6.4.15 that keep future work cheap
- What is explicitly **not** done in 6.4.15 (no premature abstraction)
- A brief sketch of what the future release will add

---

### Proposed pre-release convention

Use **GitHub's native `prerelease` flag** (the checkbox in the GitHub release editor) combined with a **tag suffix**:

| Train | Tag format | Example | GitHub `prerelease` field |
|---|---|---|---|
| Stable | `vMAJOR.MINOR.PATCH` | `v6.5.0` | `false` |
| Beta | `vMAJOR.MINOR.PATCH-beta.N` | `v6.5.0-beta.1` | `true` |
| Release candidate | `vMAJOR.MINOR.PATCH-rc.N` | `v6.5.0-rc.2` | `true` |

Rules:
- The `-beta.N` / `-rc.N` suffix is the source of truth for the channel in the tag itself; the GitHub `prerelease: true` flag is redundant but should always be set for belt-and-suspenders filtering.
- A stable release never carries a suffix; any tag without a suffix is treated as stable by all code paths.
- Pre-release assets follow the same naming convention as stable assets (same `linux-amd64` / `linux-arm64` suffix, same `.sig` companion). The build pipeline is identical.
- The N counter resets to 1 for each new `MAJOR.MINOR.PATCH` base and increments per beta/rc build (e.g. `-beta.1`, `-beta.2`, `-rc.1`).

---

### Why `/releases/latest` is safe forever

`CheckLatest()` calls `/releases/latest`. GitHub defines this endpoint as:

> *"The latest published full release … Draft and prerelease releases are not returned."*

This means `CheckLatest()` will **never** surface a pre-release tag regardless of how many beta builds are published. No change is needed to `CheckLatest()` now or when the beta train launches.

---

### What is already implemented in 6.4.15

The filtering and struct design described in the previous section (`CheckReleases` + `ReleaseSummary`) **are the base implementation**, not optional additions. They are the only correct design given that pre-release tags will appear on the same GitHub repo in the near future.

To summarise what 6.4.15 ships and why:

| Decision | Reason |
|---|---|
| `CheckReleases` fetches `n×3` entries and skips `prerelease == true` | Pre-release builds published to GitHub must be completely invisible — they never appear in the Release Notes popup or anywhere else |
| `per_page = n×3` instead of `n` | Guarantees exactly n stable results remain after filtering even if several beta tags are interleaved |
| `Prerelease bool` carried in `ReleaseSummary` | The field exists so future code can opt in to showing pre-releases without changing the struct; for now it is always `false` |
| `CheckLatest()` unchanged | GitHub's `/releases/latest` already excludes pre-releases by spec — no filter needed there |

### Forward-compatible additions that pay off later

#### 1. `_setVerLabel(el, ver, channel)` accepts an explicit channel string

Change the helper signature to accept the channel name, defaulting to `"Stable"` today:

```js
function _setVerLabel(el, ver, channel = 'Stable') {
  el.innerHTML =
    `<span style="font-family:'SF Mono',monospace;font-weight:700;">${esc('v'+ver)}</span>`
    + `<span style="font-size:11px;color:var(--text-3);margin-left:6px;font-weight:500;">(${esc(channel)})</span>`;
}
```

All callers in 6.4.15 omit the third argument (default `'Stable'`). The future release adds an `AppConfig.UpdateChannel` field and passes `channel === 'beta' ? 'Beta' : 'Stable'` from the backend into the `check` response.

#### 2. `_buildReleaseNotesDom` renders a pre-release badge (defensive only)

Since `CheckReleases` already filters all pre-releases at the source, `r.prerelease` will always be `false` in the Release Notes popup. The badge line below is included purely as a defensive guard — if for any reason a pre-release entry slipped through (e.g. the filter was bypassed during a future beta-channel feature), the UI would label it clearly rather than presenting it silently as stable:

```js
${r.prerelease ? '<span style="font-size:11px;font-weight:600;padding:2px 8px;border-radius:10px;background:rgba(255,159,10,0.15);color:var(--accent-warn);flex-shrink:0;">Pre-release</span>' : ''}
```

The real source of truth is the backend filter in `CheckReleases`, not this badge.

---

### `parseSemver` — known limitation with pre-release suffixes

The current `parseSemver` in `handlers/binary_update.go` strips only a numeric `-N` build suffix (e.g. `6.3.24-1` → `[6,3,24,1]`). It handles this via `strconv.Atoi` on the text after the `-`. For a tag like `v6.5.0-beta.1`, `strconv.Atoi("beta.1")` returns `0`, so the parsed result is `[6,5,0,0]` — identical to `v6.5.0`. This means a beta tag compares **equal to** its stable counterpart rather than less-than, which is wrong.

**This is not a problem in 6.4.15** because `parseSemver` is only used in `semverGreater` to compare the latest stable (from `CheckLatest`) against the running version — both are always stable tags.

**When the beta channel is introduced**, `parseSemver` must be extended to recognise pre-release identifiers and sort them correctly:

```
v6.5.0-beta.1  <  v6.5.0-rc.1  <  v6.5.0  <  v6.5.1
```

The fix is to parse the pre-release label (`beta`/`rc`/`alpha`) into an ordinal before the numeric counter, and treat the absence of a pre-release label as higher than any pre-release. This work is scoped to the future pre-release version, not 6.4.15.

---

### What the future pre-release release will add (sketch)

This is **not in scope for 6.4.15** — documented here for continuity.

1. **`AppConfig.UpdateChannel string`** — `"stable"` (default) or `"beta"`. Stored in `config.json`.

2. **Channel selector in the Updates card** — a small toggle or radio group below the version table: `● Stable  ○ Beta`. Changing it saves `AppConfig.UpdateChannel` via a new `PUT /api/settings/update-channel` endpoint and invalidates the version cache.

3. **`CheckLatestForChannel(channel string) (ReleaseInfo, error)`** in `internal/updater/updater.go`:
   - `"stable"` → existing `GET /releases/latest` (unchanged)
   - `"beta"` → `GET /releases?per_page=10`, pick the first entry where `prerelease == true`

4. **`HandleCheckBinaryUpdate` updated** to call `CheckLatestForChannel(appCfg.UpdateChannel)` and include `"channel": appCfg.UpdateChannel` in its JSON response.

5. **`_setVerLabel` callers updated** to read `data.channel` from the `/check` response and pass it in.

6. **`CheckReleases` updated** to accept a `channel` param: `"stable"` filters `prerelease == false`, `"beta"` returns all (stable + pre-release) so the Release Notes popup shows the full mixed history.

7. **`parseSemver` fixed** to handle `-beta.N` and `-rc.N` suffixes correctly.

8. **Downgrade from a pre-release** to a stable — supported automatically since `CheckRelease(tag)` + `HandleBinaryUpdateApply(?tag=)` already work for any tag.

---

## File change summary (updated)

| File | Change |
|---|---|
| `internal/updater/updater.go` | Add `CheckReleases(n)` (fetches `n×3`, filters `prerelease==true` out, returns exactly n stable entries), `CheckRelease(tag)`, `ReleaseSummary` struct (includes `Prerelease bool`, always `false` in output); add `repoReleasesAPI`, `repoTagAPI` constants |
| `handlers/binary_update.go` | Add `HandleListReleases`; modify `HandleBinaryUpdateApply` to accept `?tag=` query param |
| `handlers/router.go` | Register `GET /api/binary-update/releases` |
| `static/index.html` | `(Stable)` labels via `_setVerLabel(el,ver,channel='Stable')`; Release Notes button; `#modal-release-notes` with pre-release badge (inert); `#modal-rn-downgrade-confirm`; JS: all new functions; modify `applyBinaryUpdate(tag=null)` |
