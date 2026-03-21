# Version 6.3.20 Plan — S3 Object Server (MinIO)

## Overview

One major feature area:

1. **S3 Object Server** — Optional feature backed by MinIO. Adds a new "S3 Buckets" section to the Sharing sidebar. Includes: binary installation, service management, global configuration, S3 user management (create/delete/disable/password), and per-bucket creation with versioning, object lock, quota, and per-user access control. Status dot on nav item reflects live service and bucket state.

No new Go module dependencies — stdlib + gorilla/mux + gorilla/websocket only.

MinIO is managed via the `mc` (MinIO Client) CLI. Both `minio` and `mc` binaries are downloaded from `dl.min.io` during the "Enable S3 Object Server" step.

---

## Feature 1: Prerequisites — S3 Object Server Optional Feature Row

### 1.1 Binary Installation Strategy

Two binaries are installed during the "Enable S3 Object Server" step:

| Binary | Source URL | Install Path |
|--------|-----------|-------------|
| `minio` | `https://dl.min.io/server/minio/release/linux-amd64/minio` | `/usr/local/bin/minio` |
| `mc`    | `https://dl.min.io/client/mc/release/linux-amd64/mc`       | `/usr/local/bin/mc`    |

Installation sequence run by `InstallMinIO()`:

```
1. wget -O /usr/local/bin/minio  https://dl.min.io/server/minio/release/linux-amd64/minio
2. wget -O /usr/local/bin/mc     https://dl.min.io/client/mc/release/linux-amd64/mc
3. chmod +x /usr/local/bin/minio /usr/local/bin/mc
4. useradd --system --home-dir /var/lib/minio --shell /usr/sbin/nologin minio-user  (skip if exists)
5. mkdir -p /var/lib/minio  &&  chown minio-user:minio-user /var/lib/minio
6. Write /etc/systemd/system/minio.service  (see §3.2)
7. systemctl daemon-reload
8. systemctl enable minio
```

Steps 1–8 are streamed to the frontend via the existing WebSocket install-output flow (reuse `HandleInstallPackageStream` or a new equivalent endpoint).

### 1.2 `system/minio.go` — Detection

```go
// MinIOPrereqsInstalled returns true when both minio and mc binaries are present.
func MinIOPrereqsInstalled() bool {
    _, err1 := exec.LookPath("minio")
    _, err2 := exec.LookPath("mc")
    return err1 == nil && err2 == nil
}
```

### 1.3 `POST /api/prerequisites/install` — Allowlist Extension

Add `"minio"` to the package allowlist in `HandleInstallPackage`. When `package == "minio"`, call `system.InstallMinIO()` (streaming variant) instead of apt-get.

### 1.4 Frontend — Prerequisites Optional Features Row

In the Optional Features table, add a second row below the existing `targetcli-fb` row:

```html
<div style="display:flex;align-items:center;gap:12px;padding:12px 0;border-bottom:1px solid var(--border);">
  <div style="flex:1;">
    <span style="color:var(--text-2);font-size:14px;">minio + mc</span>
    <i class="info-tip" data-tip="Required for S3 Object Server. Installs the MinIO binary and the mc management client.&#10;Both are downloaded directly from dl.min.io."></i>
    <div style="font-size:12px;color:var(--text-3);margin-top:2px;">S3-compatible object storage server via MinIO</div>
  </div>
  <span id="prereq-minio-status" style="font-size:13px;color:var(--text-3);">Not installed</span>
  <button id="prereq-minio-btn" class="btn btn-ghost btn-sm" onclick="enableMinIOFeature()" style="font-size:12px;white-space:nowrap;">Enable S3 Object Server</button>
</div>
```

Status is refreshed by `_updateMinIOPrereqStatus()` which calls `GET /api/minio/status`:
- If `prereqs_installed === true`: show green `✓ Installed`, hide button.
- If `prereqs_installed === false`: show grey `Not installed`, show button.

`enableMinIOFeature()` POSTs to `/api/prerequisites/install` with `{ "package": "minio" }` and streams output the same way as the existing install flow, then calls `loadPrereqs()` and updates the S3 Buckets nav item.

---

## Feature 2: Config Additions (`internal/config/config.go`)

### 2.1 New Config Types

```go
// S3User represents a MinIO IAM user tracked by the portal.
// The actual user lives in MinIO's IAM store; this is a local mirror for display.
type S3User struct {
    AccessKey string `json:"access_key"`  // MinIO access key (username)
    Status    string `json:"status"`      // "enabled" or "disabled"
    CreatedAt int64  `json:"created_at"`
}

// S3Bucket is a bucket managed by ZFSNAS and tracked in config.
type S3Bucket struct {
    Name        string   `json:"name"`          // S3 bucket name (lowercase)
    Comment     string   `json:"comment"`        // optional label stored in portal config
    UserKeys    []string `json:"user_keys"`      // access keys with explicit access
    CreatedAt   int64    `json:"created_at"`
}

// MinIOConfig holds all persistent MinIO / S3 Object Server settings.
type MinIOConfig struct {
    Enabled      bool       `json:"enabled"`        // true once installed + user has configured it
    DatasetPath  string     `json:"dataset_path"`   // ZFS dataset path used as backend (e.g. "tank/s3data")
    DataDir      string     `json:"data_dir"`       // absolute mountpoint of that dataset
    Port         int        `json:"port"`           // API port, default 9000
    ConsolePort  int        `json:"console_port"`   // web console port, default 9001
    RootUser     string     `json:"root_user"`      // MINIO_ROOT_USER
    RootPassword string     `json:"root_password"`  // MINIO_ROOT_PASSWORD (stored like other passwords in config)
    Region       string     `json:"region"`         // MINIO_SITE_REGION, default "us-east-1"
    SiteName     string     `json:"site_name"`      // MINIO_SITE_NAME, optional
    ServerURL    string     `json:"server_url"`     // MINIO_SERVER_URL, optional (reverse proxy)
    Buckets      []S3Bucket `json:"buckets"`
}
```

### 2.2 AppConfig Update

```go
// In AppConfig struct:
MinIO MinIOConfig `json:"minio"`
```

Default values in `DefaultAppConfig()`:

```go
MinIO: MinIOConfig{
    Port:        9000,
    ConsolePort: 9001,
    Region:      "us-east-1",
    RootUser:    "minioadmin",
},
```

---

## Feature 3: MinIO System Layer (`system/minio.go`)

### 3.1 Service Control

```go
type MinIOServiceStatus struct {
    Active  bool   `json:"active"`
    Status  string `json:"status"` // "active", "inactive", "failed", "unknown"
}

func GetMinIOServiceStatus() MinIOServiceStatus
func StartMinIOService() error
func StopMinIOService() error
func RestartMinIOService() error
```

All implemented via `exec.Command("systemctl", action, "minio")`.

```go
func GetMinIOServiceStatus() MinIOServiceStatus {
    out, err := exec.Command("systemctl", "is-active", "minio").Output()
    status := strings.TrimSpace(string(out))
    if err != nil { status = "inactive" }
    return MinIOServiceStatus{Active: status == "active", Status: status}
}
```

### 3.2 Environment File and Systemd Unit

#### `WriteMinIOEnvFile(cfg *config.MinIOConfig) error`

Writes `/etc/default/minio` via `sudo tee`:

```
MINIO_ROOT_USER=<cfg.RootUser>
MINIO_ROOT_PASSWORD=<cfg.RootPassword>
MINIO_VOLUMES=<cfg.DataDir>
MINIO_OPTS="--address :<cfg.Port> --console-address :<cfg.ConsolePort>"
MINIO_SITE_REGION=<cfg.Region>
```

If `cfg.SiteName` is not empty: append `MINIO_SITE_NAME=<cfg.SiteName>`.
If `cfg.ServerURL` is not empty: append `MINIO_SERVER_URL=<cfg.ServerURL>`.

The content is piped via `echo '...' | sudo tee /etc/default/minio`.

#### `WriteMinIOServiceUnit() error`

Writes `/etc/systemd/system/minio.service` via `sudo tee`:

```ini
[Unit]
Description=MinIO Object Storage
Documentation=https://min.io/docs/minio/linux/index.html
Wants=network-online.target
After=network-online.target
AssertFileIsExecutable=/usr/local/bin/minio

[Service]
User=minio-user
Group=minio-user
EnvironmentFile=/etc/default/minio
ExecStartPre=/bin/bash -c "if [ -z \"${MINIO_VOLUMES}\" ]; then echo \"MINIO_VOLUMES not set\"; exit 1; fi"
ExecStart=/usr/local/bin/minio server $MINIO_OPTS $MINIO_VOLUMES
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

After writing the unit, calls `systemctl daemon-reload`.

#### `ApplyMinIOConfig(cfg *config.MinIOConfig) error`

Sequence:
1. `WriteMinIOEnvFile(cfg)`
2. If the service unit does not already exist: `WriteMinIOServiceUnit()`, `systemctl enable minio`
3. `systemctl daemon-reload`
4. `systemctl restart minio`
5. Wait up to 5 s for `systemctl is-active minio` to return `active`
6. Call `SetupMCAlias(cfg)` to refresh the `mc` alias with the new credentials

#### `SetupMCAlias(cfg *config.MinIOConfig) error`

Runs:
```
mc alias set zfsnas http://127.0.0.1:<cfg.Port> <cfg.RootUser> <cfg.RootPassword>
```

Must be run as the user that will run portal operations (not minio-user). Sets up the `zfsnas` alias used by all subsequent `mc` commands.

### 3.3 `mc` User Management Wrappers

All commands use the `zfsnas` mc alias set up in §3.2.

```go
type S3UserInfo struct {
    AccessKey string `json:"accessKey"`
    Status    string `json:"status"`    // "enabled" or "disabled"
}

// ListS3Users returns all MinIO IAM users.
func ListS3Users() ([]S3UserInfo, error)
// Output: mc admin user list zfsnas --json
// Parse JSON array from stdout.

// CreateS3User creates a new MinIO user with the given access key and secret.
func CreateS3User(accessKey, secretKey string) error
// mc admin user add zfsnas <accessKey> <secretKey>

// DeleteS3User removes a MinIO user.
func DeleteS3User(accessKey string) error
// mc admin user remove zfsnas <accessKey>

// SetS3UserStatus enables or disables a MinIO user.
func SetS3UserStatus(accessKey string, enabled bool) error
// mc admin user enable zfsnas <accessKey>  OR  mc admin user disable zfsnas <accessKey>

// SetS3UserPassword updates the secret key of a MinIO user.
func SetS3UserPassword(accessKey, newSecret string) error
// mc admin user add zfsnas <accessKey> <newSecret>  (MinIO upserts on add)

// AttachBucketPolicyToUser attaches a named IAM policy to a user.
func AttachBucketPolicyToUser(accessKey, policyName string) error
// mc admin policy attach zfsnas <policyName> --user <accessKey>

// DetachBucketPolicyFromUser removes a named IAM policy from a user.
func DetachBucketPolicyFromUser(accessKey, policyName string) error
// mc admin policy detach zfsnas <policyName> --user <accessKey>
```

### 3.4 Bucket Management Wrappers

```go
type S3BucketInfo struct {
    Name         string `json:"name"`
    CreationDate string `json:"creationDate"`
}

// ListS3Buckets returns all buckets from MinIO.
func ListS3Buckets() ([]S3BucketInfo, error)
// mc ls zfsnas --json  →  parse "key" field from each JSON line

// CreateS3Bucket creates a bucket with the given options.
type S3BucketCreateOptions struct {
    Name          string
    Versioning    string // "off", "enabled", "suspended"
    ObjectLock    bool   // cannot be changed after creation; requires versioning
    QuotaBytes    uint64 // 0 = no quota
    AnonAccess    string // "none", "download", "public"
}
func CreateS3Bucket(opts S3BucketCreateOptions) error
// mc mb zfsnas/<name>  [--with-lock if ObjectLock]
// If Versioning != "off": mc version enable zfsnas/<name>  OR mc version suspend ...
// If QuotaBytes > 0: mc quota set zfsnas/<name> --size <n>MiB
// If AnonAccess != "none": mc anonymous set <download|public> zfsnas/<name>

// DeleteS3Bucket removes a bucket and all its objects.
func DeleteS3Bucket(name string) error
// mc rb zfsnas/<name> --force --dangerous

// ApplyBucketUserPolicy creates or updates the IAM policy for a bucket+user binding.
// Policy name: "zfsnas-<bucketName>-rw"
// Grants s3:GetObject, s3:PutObject, s3:DeleteObject, s3:ListBucket on arn:aws:s3:::<name> and arn:aws:s3:::<name>/*
func ApplyBucketUserPolicy(bucketName string, userKeys []string) error
// Writes policy JSON to a temp file, then:
// mc admin policy create zfsnas zfsnas-<bucketName>-rw /tmp/policy-<bucketName>.json
// For each userKey in userKeys: mc admin policy attach zfsnas zfsnas-<bucketName>-rw --user <key>
// Users previously attached but no longer in userKeys: mc admin policy detach ...
```

---

## Feature 4: API Handlers (`handlers/minio.go`)

### `HandleMinIOStatus` — `GET /api/minio/status`

```json
{
  "prereqs_installed": true,
  "enabled": true,
  "service_active": true,
  "service_status": "active",
  "bucket_count": 3,
  "configured": true
}
```

- `prereqs_installed`: `system.MinIOPrereqsInstalled()`
- `enabled`: `appCfg.MinIO.Enabled`
- `configured`: `appCfg.MinIO.RootUser != "" && appCfg.MinIO.DataDir != ""`
- `service_active` / `service_status`: `system.GetMinIOServiceStatus()`
- `bucket_count`: `len(appCfg.MinIO.Buckets)`

### `HandleMinIOServiceAction` — `POST /api/minio/service`

Body: `{ "action": "start" | "stop" | "restart" }`.
Calls the appropriate service function. Returns `{ "ok": true }` or jsonErr.

### `HandleGetMinIOConfig` — `GET /api/minio/config`

Returns `appCfg.MinIO` (excluding `RootPassword` — replace with `"***"` in the response).

### `HandleSaveMinIOConfig` — `POST /api/minio/config`

```go
type MinIOConfigRequest struct {
    DatasetPath  string `json:"dataset_path"`
    Port         int    `json:"port"`
    ConsolePort  int    `json:"console_port"`
    RootUser     string `json:"root_user"`
    RootPassword string `json:"root_password"`  // empty = keep existing
    Region       string `json:"region"`
    SiteName     string `json:"site_name"`
    ServerURL    string `json:"server_url"`
}
```

Validation:
- `DatasetPath` must not be empty; must exist as a valid mounted dataset (call `system.ListAllDatasets()` to verify).
- Derive `DataDir` from the dataset's `Mountpoint` property.
- `Port` and `ConsolePort` must be in range 1–65535 and must differ.
- `RootUser` min 3 characters; `RootPassword` min 8 characters (unless empty, in which case keep existing).
- Ensure `DataDir` is owned/writable: `chown -R minio-user:minio-user <DataDir>`.

On success:
1. Update `appCfg.MinIO` with the new values.
2. Set `appCfg.MinIO.Enabled = true`.
3. Save config.
4. Call `system.ApplyMinIOConfig(&appCfg.MinIO)` (writes env file, restarts service, sets up mc alias).
5. Write audit entry `audit.ActionEditMinIOConfig`.
6. Return `{ "ok": true }`.

### `HandleListS3Users` — `GET /api/minio/users`

Calls `system.ListS3Users()` and returns the array. Returns 503 if MinIO is not running.

### `HandleCreateS3User` — `POST /api/minio/user/create`

```go
type S3UserCreateRequest struct {
    AccessKey string `json:"access_key"` // min 3 chars, alphanumeric+dash+underscore
    SecretKey string `json:"secret_key"` // min 8 chars
}
```

Validates inputs, calls `system.CreateS3User(...)`. On success, appends to `appCfg.MinIO` users cache (optional — list is always live from mc). Writes audit entry `audit.ActionCreateS3User`. Returns `{ "ok": true }`.

### `HandleDeleteS3User` — `POST /api/minio/user/delete`

Body: `{ "access_key": "..." }`. Calls `system.DeleteS3User(...)`. Removes any references from `appCfg.MinIO.Buckets[*].UserKeys`. Saves config. Writes audit entry `audit.ActionDeleteS3User`.

### `HandleSetS3UserStatus` — `POST /api/minio/user/status`

Body: `{ "access_key": "...", "enabled": true|false }`. Calls `system.SetS3UserStatus(...)`. Returns `{ "ok": true }`.

### `HandleSetS3UserPassword` — `POST /api/minio/user/password`

Body: `{ "access_key": "...", "secret_key": "..." }`. Validates new secret (min 8 chars). Calls `system.SetS3UserPassword(...)`. Returns `{ "ok": true }`.

### `HandleListS3Buckets` — `GET /api/minio/buckets`

Calls `system.ListS3Buckets()` to get live data from MinIO, merges with `appCfg.MinIO.Buckets` for comment and user-access metadata. Returns merged array:

```json
[
  {
    "name": "backups",
    "comment": "Nightly backup target",
    "user_keys": ["backup-user"],
    "creation_date": "2026-03-20T00:00:00Z"
  }
]
```

### `HandleCreateS3Bucket` — `POST /api/minio/bucket/create`

```go
type S3BucketCreateRequest struct {
    Name       string   `json:"name"`        // required
    Comment    string   `json:"comment"`
    Versioning string   `json:"versioning"`  // "off", "enabled", "suspended"
    ObjectLock bool     `json:"object_lock"`
    QuotaStr   string   `json:"quota"`       // "0" or "10G", "500M", etc.
    AnonAccess string   `json:"anon_access"` // "none", "download", "public"
    UserKeys   []string `json:"user_keys"`
}
```

Validation:
- `Name`: 3–63 chars, lowercase, alphanumeric or `-`, must not start or end with `-`, no consecutive `--`.
- `Versioning` must be one of `"off"`, `"enabled"`, `"suspended"`.
- `ObjectLock` requires `Versioning == "enabled"`.
- `QuotaStr`: parse with `parseHumanBytes()`, 0 = unlimited.
- `AnonAccess` must be one of `"none"`, `"download"`, `"public"`.

On success:
1. Call `system.CreateS3Bucket(opts)`.
2. Call `system.ApplyBucketUserPolicy(name, userKeys)` if `len(userKeys) > 0`.
3. Append `S3Bucket{Name, Comment, UserKeys, CreatedAt}` to `appCfg.MinIO.Buckets`.
4. Save config.
5. Write audit entry `audit.ActionCreateS3Bucket`.

### `HandleDeleteS3Bucket` — `POST /api/minio/bucket/delete`

Body: `{ "name": "..." }`. Calls `system.DeleteS3Bucket(name)`. Removes the bucket from `appCfg.MinIO.Buckets`. Saves config. Writes audit entry `audit.ActionDeleteS3Bucket`.

### `HandleEditS3Bucket` — `POST /api/minio/bucket/edit`

Body: same as create but without `object_lock` (immutable) and `name` is the target. Updates comment, versioning, quota, anon access, and user keys. Re-applies bucket policy via `system.ApplyBucketUserPolicy(...)`. Saves config. Writes audit entry `audit.ActionEditS3Bucket`.

---

## Feature 5: Router Changes (`handlers/router.go`)

```go
r.Handle("/api/minio/status",
    RequireAuth(http.HandlerFunc(HandleMinIOStatus(appCfg)))).Methods("GET")
r.Handle("/api/minio/service",
    RequireAdmin(http.HandlerFunc(HandleMinIOServiceAction(appCfg)))).Methods("POST")
r.Handle("/api/minio/config",
    RequireAuth(http.HandlerFunc(HandleGetMinIOConfig(appCfg)))).Methods("GET")
r.Handle("/api/minio/config",
    RequireAdmin(http.HandlerFunc(HandleSaveMinIOConfig(appCfg)))).Methods("POST")
r.Handle("/api/minio/users",
    RequireAuth(http.HandlerFunc(HandleListS3Users(appCfg)))).Methods("GET")
r.Handle("/api/minio/user/create",
    RequireAdmin(http.HandlerFunc(HandleCreateS3User(appCfg)))).Methods("POST")
r.Handle("/api/minio/user/delete",
    RequireAdmin(http.HandlerFunc(HandleDeleteS3User(appCfg)))).Methods("POST")
r.Handle("/api/minio/user/status",
    RequireAdmin(http.HandlerFunc(HandleSetS3UserStatus(appCfg)))).Methods("POST")
r.Handle("/api/minio/user/password",
    RequireAdmin(http.HandlerFunc(HandleSetS3UserPassword(appCfg)))).Methods("POST")
r.Handle("/api/minio/buckets",
    RequireAuth(http.HandlerFunc(HandleListS3Buckets(appCfg)))).Methods("GET")
r.Handle("/api/minio/bucket/create",
    RequireAdmin(http.HandlerFunc(HandleCreateS3Bucket(appCfg)))).Methods("POST")
r.Handle("/api/minio/bucket/delete",
    RequireAdmin(http.HandlerFunc(HandleDeleteS3Bucket(appCfg)))).Methods("POST")
r.Handle("/api/minio/bucket/edit",
    RequireAdmin(http.HandlerFunc(HandleEditS3Bucket(appCfg)))).Methods("POST")
```

---

## Feature 6: Audit Constants (`internal/audit/audit.go`)

```go
const (
    ActionEditMinIOConfig  = "edit_minio_config"
    ActionCreateS3User     = "create_s3_user"
    ActionDeleteS3User     = "delete_s3_user"
    ActionCreateS3Bucket   = "create_s3_bucket"
    ActionDeleteS3Bucket   = "delete_s3_bucket"
    ActionEditS3Bucket     = "edit_s3_bucket"
)
```

---

## Feature 7: Frontend (`static/index.html`)

### 7.1 Sidebar Nav — S3 Buckets

In the Sharing section of the left nav, after the iSCSI nav item:

```html
<button class="nav-item" id="nav-s3" onclick="navS3()" style="opacity:0.45;" data-prereqs-missing="1"
        title="S3 Object Server requires MinIO — see Prerequisites">
  <span class="nav-icon"><img src="/static/s3.svg" class="s3-icon" alt="" style="width:16px;height:16px;"></span>
  <span class="nav-label"> S3 Buckets</span>
  <span id="nav-s3-dot" class="nav-dot" style="display:none;width:8px;height:8px;border-radius:50%;margin-left:auto;flex-shrink:0;"></span>
</button>
```

Use the existing AWS S3 orange cube icon or a generic storage SVG. The dot color and visibility are driven by `_updateS3NavDot()`.

#### `navS3()`

```js
function navS3() {
    if (document.getElementById('nav-s3').dataset.prereqsMissing === '1') {
        showPage('prereqs');
        setTimeout(() => {
            const el = document.getElementById('prereq-optional-features');
            if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
        }, 100);
        return;
    }
    showPage('s3');
}
```

#### `_updateS3NavDot(status)`

Called after any `GET /api/minio/status` response:

```js
function _updateS3NavDot(status) {
    const btn = document.getElementById('nav-s3');
    const dot = document.getElementById('nav-s3-dot');
    if (!btn || !dot) return;

    if (!status.prereqs_installed || !status.enabled) {
        btn.dataset.prereqsMissing = '1';
        btn.style.opacity = '0.45';
        dot.style.display = 'none';
        return;
    }

    btn.dataset.prereqsMissing = '0';
    btn.style.opacity = '';

    const hasBuckets = status.bucket_count > 0;
    if (hasBuckets && status.service_active) {
        dot.style.display = 'inline-block';
        dot.style.background = '#30d158'; // green
        dot.style.boxShadow = '0 0 4px rgba(48,209,88,0.6)';
    } else if (hasBuckets && !status.service_active) {
        dot.style.display = 'inline-block';
        dot.style.background = '#ff453a'; // red
        dot.style.boxShadow = '0 0 4px rgba(255,69,58,0.6)';
    } else {
        dot.style.display = 'none';
    }
}
```

Call `_updateS3NavDot()` from:
- `initApp()` on startup (alongside iSCSI status fetch)
- `loadS3Page()` whenever the S3 page is loaded
- After any service start/stop/restart action

### 7.2 S3 Buckets Page HTML

```html
<!-- S3 BUCKETS -->
<div class="page hidden" id="page-s3">

  <!-- Prerequisites banner (shown when prereqs not installed) -->
  <div id="s3-prereqs-banner" class="hidden" style="padding:16px 20px;margin-bottom:16px;background:rgba(255,159,10,0.07);border:1px solid rgba(255,159,10,0.25);border-radius:var(--radius-sm);">
    <span style="font-size:14px;color:var(--text-2);">
      S3 Object Server requires <strong>MinIO</strong> to be installed.
      Go to <a onclick="showPage('prereqs')" style="color:var(--accent);cursor:pointer;text-decoration:underline;">Prerequisites</a> to enable this feature.
    </span>
  </div>

  <!-- Configure required banner (shown when installed but not yet configured) -->
  <div id="s3-configure-banner" class="hidden" style="padding:16px 20px;margin-bottom:16px;background:rgba(100,210,255,0.07);border:1px solid rgba(100,210,255,0.25);border-radius:var(--radius-sm);">
    <span style="font-size:14px;color:var(--text-2);">
      MinIO is installed but not yet configured. Click <strong>Configure</strong> to set up the S3 Object Server.
    </span>
  </div>

  <!-- Page header (shown when installed + configured) -->
  <div id="s3-content" class="hidden">
    <div class="page-header">
      <div style="display:flex;align-items:center;gap:8px;">
        <h2>S3 Buckets</h2>
        <span id="s3-svc-badge" style="font-size:11px;padding:2px 8px;border-radius:10px;font-weight:600;background:var(--surface2);color:var(--text-3);">unknown</span>
      </div>
      <div style="display:flex;gap:8px;align-items:center;">
        <button class="btn btn-ghost btn-sm" onclick="openS3UsersModal()">👤 S3 Users</button>
        <button class="btn btn-ghost btn-sm" onclick="openS3ConfigureModal()">⚙ Configure</button>
        <button class="btn btn-primary btn-sm" onclick="openNewS3BucketModal()">+ New S3 Bucket</button>
      </div>
    </div>

    <!-- Buckets table -->
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Bucket</th>
            <th>Versioning</th>
            <th>Object Lock</th>
            <th>Quota</th>
            <th>Access</th>
            <th>Users</th>
            <th>Comment</th>
            <th></th>
          </tr>
        </thead>
        <tbody id="s3-buckets-tbody">
          <tr><td colspan="8" style="text-align:center;color:var(--text-3);padding:32px;">Loading…</td></tr>
        </tbody>
      </table>
    </div>
  </div>

</div>
```

#### `loadS3Page()`

```js
async function loadS3Page() {
    const r = await fetch('/api/minio/status');
    if (!r.ok) return;
    const s = await r.json();
    _updateS3NavDot(s);

    const prereqBanner   = document.getElementById('s3-prereqs-banner');
    const configureBanner = document.getElementById('s3-configure-banner');
    const content        = document.getElementById('s3-content');

    if (!s.prereqs_installed || !s.enabled) {
        prereqBanner.classList.remove('hidden');
        configureBanner.classList.add('hidden');
        content.classList.add('hidden');
        return;
    }
    prereqBanner.classList.add('hidden');

    if (!s.configured) {
        configureBanner.classList.remove('hidden');
        content.classList.add('hidden');
        return;
    }
    configureBanner.classList.add('hidden');
    content.classList.remove('hidden');

    // Update service badge
    const badge = document.getElementById('s3-svc-badge');
    if (badge) {
        badge.textContent = s.service_status;
        badge.style.background  = s.service_active ? 'rgba(48,209,88,0.15)' : 'rgba(255,69,58,0.15)';
        badge.style.color       = s.service_active ? '#30d158' : '#ff453a';
    }

    await loadS3Buckets();
}
```

#### `loadS3Buckets()`

```js
async function loadS3Buckets() {
    const tbody = document.getElementById('s3-buckets-tbody');
    tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--text-3);padding:24px;">Loading…</td></tr>';
    const r = await fetch('/api/minio/buckets');
    if (!r.ok) {
        tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--accent-danger);padding:24px;">Failed to load buckets.</td></tr>';
        return;
    }
    const buckets = await r.json();
    if (!buckets.length) {
        tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--text-3);padding:32px;">No buckets yet. Click "+ New S3 Bucket" to create one.</td></tr>';
        return;
    }
    tbody.innerHTML = buckets.map(b => `
      <tr>
        <td style="font-family:monospace;font-weight:600;">${esc(b.name)}</td>
        <td>${b.versioning || '—'}</td>
        <td>${b.object_lock ? '<span style="color:#f0b429;">🔒 Enabled</span>' : '—'}</td>
        <td>${b.quota || '—'}</td>
        <td>${_s3AnonBadge(b.anon_access)}</td>
        <td style="font-size:12px;color:var(--text-3);">${(b.user_keys||[]).join(', ') || 'All'}</td>
        <td style="color:var(--text-3);font-size:13px;">${esc(b.comment||'')}</td>
        <td>
          <button class="btn btn-ghost btn-sm" onclick="openEditS3BucketModal(${JSON.stringify(b)})">Edit</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--accent-danger);" onclick="confirmDeleteS3Bucket('${esc(b.name)}')">Delete</button>
        </td>
      </tr>
    `).join('');
}

function _s3AnonBadge(access) {
    if (access === 'public')   return '<span style="color:#ff9f0a;font-size:12px;">Public R/W</span>';
    if (access === 'download') return '<span style="color:#64d2ff;font-size:12px;">Public Read</span>';
    return '<span style="color:var(--text-3);font-size:12px;">Private</span>';
}
```

Add to `showPage()` dispatch:
```js
if (name === 's3') loadS3Page();
```

---

### 7.3 Configure Modal (S3 Object Server)

A `modal-lg` opened by `openS3ConfigureModal()`. Pre-populated via `GET /api/minio/config`.

```html
<div class="modal-header">
  <span>Configure S3 Object Server</span>
  <button onclick="closeModal()">✕</button>
</div>
<div class="modal-body" style="display:grid;gap:16px;">

  <!-- Storage -->
  <div class="form-group">
    <label>
      Backend Dataset
      <i class="info-tip" data-tip="The ZFS dataset whose mountpoint MinIO will use to store all objects. ZFSNAS will ensure the minio-user owns this directory. Choose a dataset with enough free space for your expected object storage usage."></i>
    </label>
    <select id="s3cfg-dataset"><!-- populated from /api/datasets --></select>
  </div>

  <!-- Network -->
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
    <div class="form-group">
      <label>
        API Port
        <i class="info-tip" data-tip="TCP port MinIO listens on for S3 API requests (e.g. AWS CLI, SDKs, s3cmd). Default: 9000."></i>
      </label>
      <input type="number" id="s3cfg-port" value="9000" min="1" max="65535">
    </div>
    <div class="form-group">
      <label>
        Console Port
        <i class="info-tip" data-tip="TCP port for the MinIO web management console. Must differ from API port. Default: 9001."></i>
      </label>
      <input type="number" id="s3cfg-console-port" value="9001" min="1" max="65535">
    </div>
  </div>

  <!-- Credentials -->
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
    <div class="form-group">
      <label>
        Root User
        <i class="info-tip" data-tip="MinIO root / admin username (MINIO_ROOT_USER). This is the superuser for the MinIO instance, separate from ZFSNAS portal users. Minimum 3 characters."></i>
      </label>
      <input type="text" id="s3cfg-root-user" placeholder="minioadmin" autocomplete="off">
    </div>
    <div class="form-group">
      <label>
        Root Password
        <i class="info-tip" data-tip="MinIO root password (MINIO_ROOT_PASSWORD). Minimum 8 characters. Leave blank to keep the existing password."></i>
      </label>
      <input type="password" id="s3cfg-root-password" placeholder="Leave blank to keep existing" autocomplete="new-password">
    </div>
  </div>

  <!-- Region & Site -->
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
    <div class="form-group">
      <label>
        Region
        <i class="info-tip" data-tip="S3 region name reported to clients (MINIO_SITE_REGION). Most clients default to us-east-1. Change only if your tooling expects a specific region."></i>
      </label>
      <input type="text" id="s3cfg-region" placeholder="us-east-1">
    </div>
    <div class="form-group">
      <label>
        Site Name
        <i class="info-tip" data-tip="Optional human-readable name for this MinIO instance (MINIO_SITE_NAME). Shown in the MinIO console header."></i>
      </label>
      <input type="text" id="s3cfg-site-name" placeholder="Optional">
    </div>
  </div>

  <!-- Server URL -->
  <div class="form-group">
    <label>
      Server URL
      <i class="info-tip" data-tip="Optional public URL used when MinIO is behind a reverse proxy (MINIO_SERVER_URL). Example: https://s3.yourdomain.com. Leave blank if clients connect directly to this server's IP and port."></i>
    </label>
    <input type="text" id="s3cfg-server-url" placeholder="https://s3.yourdomain.com  (optional)">
  </div>

</div>
<div class="modal-footer">
  <button onclick="closeModal()">Cancel</button>
  <button class="btn btn-primary" onclick="submitS3Configure()">Save &amp; Apply</button>
</div>
```

#### `openS3ConfigureModal()`

```js
async function openS3ConfigureModal() {
    // Fetch /api/minio/config and /api/datasets?pool=all
    // Populate dataset <select> with all mounted datasets
    // Pre-fill fields from config (password always blank)
    openModal('modal-s3-configure');
}
```

#### `submitS3Configure()`

```js
async function submitS3Configure() {
    const body = {
        dataset_path:  document.getElementById('s3cfg-dataset').value,
        port:          parseInt(document.getElementById('s3cfg-port').value),
        console_port:  parseInt(document.getElementById('s3cfg-console-port').value),
        root_user:     document.getElementById('s3cfg-root-user').value.trim(),
        root_password: document.getElementById('s3cfg-root-password').value,
        region:        document.getElementById('s3cfg-region').value.trim() || 'us-east-1',
        site_name:     document.getElementById('s3cfg-site-name').value.trim(),
        server_url:    document.getElementById('s3cfg-server-url').value.trim(),
    };
    // POST /api/minio/config
    // On success: closeModal(), loadS3Page(), _updateS3NavDot()
}
```

---

### 7.4 S3 Users Modal

Opened by `openS3UsersModal()`. A large modal with an inline user table and an "Add User" form at the bottom.

```html
<div class="modal-header">
  <span>S3 Users</span>
  <button onclick="closeModal()">✕</button>
</div>
<div class="modal-body">

  <!-- User table -->
  <table style="width:100%;">
    <thead>
      <tr>
        <th>Access Key</th>
        <th>Status</th>
        <th>Actions</th>
      </tr>
    </thead>
    <tbody id="s3-users-tbody">
      <tr><td colspan="3" style="text-align:center;color:var(--text-3);padding:20px;">Loading…</td></tr>
    </tbody>
  </table>

  <!-- Divider -->
  <hr style="border:none;border-top:1px solid var(--border);margin:20px 0;">

  <!-- Add User form -->
  <div style="font-weight:600;font-size:14px;margin-bottom:12px;">Add S3 User</div>
  <div style="display:grid;grid-template-columns:1fr 1fr auto;gap:10px;align-items:end;">
    <div class="form-group" style="margin:0;">
      <label>
        Access Key
        <i class="info-tip" data-tip="The username / access key ID used by S3 clients. Minimum 3 characters. Alphanumeric, dashes and underscores only."></i>
      </label>
      <input type="text" id="s3u-access-key" placeholder="my-backup-user" autocomplete="off">
    </div>
    <div class="form-group" style="margin:0;">
      <label>
        Secret Key
        <i class="info-tip" data-tip="The password / secret access key used by S3 clients. Minimum 8 characters."></i>
      </label>
      <input type="password" id="s3u-secret-key" placeholder="min 8 characters" autocomplete="new-password">
    </div>
    <button class="btn btn-primary" style="height:36px;" onclick="submitCreateS3User()">Add</button>
  </div>
  <div id="s3u-create-error" style="color:var(--accent-danger);font-size:12px;margin-top:6px;display:none;"></div>

</div>
<div class="modal-footer">
  <button onclick="closeModal()">Close</button>
</div>
```

#### Rendered user row

Each row in `#s3-users-tbody` has:

```html
<tr>
  <td style="font-family:monospace;">${esc(u.accessKey)}</td>
  <td>
    <span style="color:${u.status==='enabled'?'#30d158':'var(--text-3)'};">
      ${u.status === 'enabled' ? '● Enabled' : '○ Disabled'}
    </span>
  </td>
  <td style="display:flex;gap:6px;">
    <button class="btn btn-ghost btn-sm"
            onclick="toggleS3UserStatus('${esc(u.accessKey)}', ${u.status !== 'enabled'})">
      ${u.status === 'enabled' ? 'Disable' : 'Enable'}
    </button>
    <button class="btn btn-ghost btn-sm" onclick="openS3ChangePasswordModal('${esc(u.accessKey)}')">
      Change Password
    </button>
    <button class="btn btn-ghost btn-sm" style="color:var(--accent-danger);"
            onclick="confirmDeleteS3User('${esc(u.accessKey)}')">
      Delete
    </button>
  </td>
</tr>
```

#### JS Functions

```js
async function openS3UsersModal() {
    openModal('modal-s3-users');
    await _loadS3UsersTable();
}

async function _loadS3UsersTable() {
    const tbody = document.getElementById('s3-users-tbody');
    tbody.innerHTML = '<tr><td colspan="3" style="text-align:center;color:var(--text-3);">Loading…</td></tr>';
    const r = await fetch('/api/minio/users');
    if (!r.ok) { tbody.innerHTML = '<tr><td colspan="3" style="color:var(--accent-danger);">Failed to load users.</td></tr>'; return; }
    const users = await r.json();
    if (!users.length) { tbody.innerHTML = '<tr><td colspan="3" style="text-align:center;color:var(--text-3);">No S3 users yet.</td></tr>'; return; }
    tbody.innerHTML = users.map(u => `...`).join(''); // row template above
}

async function submitCreateS3User() { /* POST /api/minio/user/create, reload table */ }
async function toggleS3UserStatus(key, enable) { /* POST /api/minio/user/status, reload table */ }
async function confirmDeleteS3User(key) { /* confirm dialog, then POST /api/minio/user/delete, reload table */ }
function openS3ChangePasswordModal(key) {
    // Show a small inline prompt or a mini modal asking for new secret (min 8 chars)
    // POST /api/minio/user/password
}
```

---

### 7.5 New S3 Bucket Modal

A `modal-lg` opened by `openNewS3BucketModal()`. Also used for editing (pre-populated) when opened via `openEditS3BucketModal(bucket)`.

```html
<div class="modal-header">
  <span id="s3bkt-modal-title">New S3 Bucket</span>
  <button onclick="closeModal()">✕</button>
</div>
<div class="modal-body" style="display:grid;gap:16px;">

  <!-- Identity -->
  <div class="form-group">
    <label>
      Bucket Name
      <i class="info-tip" data-tip="S3 bucket names must be 3–63 characters, lowercase, use only letters, numbers, and hyphens, and cannot start or end with a hyphen. Example: my-backups-2026"></i>
    </label>
    <input type="text" id="s3bkt-name" placeholder="my-bucket-name">
  </div>

  <div class="form-group">
    <label>
      Comment
      <i class="info-tip" data-tip="Optional label stored in the ZFSNAS configuration. Not visible to S3 clients."></i>
    </label>
    <input type="text" id="s3bkt-comment" placeholder="Optional description">
  </div>

  <!-- Versioning & Object Lock -->
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
    <div class="form-group">
      <label>
        Versioning
        <i class="info-tip" data-tip="When enabled, MinIO preserves previous versions of objects on every write. Suspended keeps existing versions but stops creating new ones. Required for Object Lock."></i>
      </label>
      <select id="s3bkt-versioning" onchange="_s3BktVersioningChange()">
        <option value="off">Off</option>
        <option value="enabled">Enabled</option>
        <option value="suspended">Suspended</option>
      </select>
    </div>
    <div class="form-group">
      <label>
        Object Lock
        <i class="info-tip" data-tip="Prevents objects from being deleted or overwritten for a set retention period (WORM). Requires Versioning to be Enabled. Cannot be changed after bucket creation (S3 limitation)."></i>
      </label>
      <select id="s3bkt-object-lock" disabled>
        <option value="false">Disabled</option>
        <option value="true">Enabled</option>
      </select>
    </div>
  </div>

  <!-- Quota -->
  <div class="form-group">
    <label>
      Storage Quota
      <i class="info-tip" data-tip="Maximum total size for all objects in this bucket. Use suffixes: K, M, G, T (e.g. 50G). Set to 0 or leave blank for unlimited."></i>
    </label>
    <input type="text" id="s3bkt-quota" placeholder="e.g. 50G — leave blank for unlimited">
  </div>

  <!-- Anonymous Access -->
  <div class="form-group">
    <label>
      Anonymous Access
      <i class="info-tip" data-tip="Controls what unauthenticated (anonymous) S3 clients can do.&#10;• Private — no anonymous access (default, recommended).&#10;• Public Read — anyone can download objects (e.g. static file hosting).&#10;• Public R/W — anyone can read and write (not recommended for production)."></i>
    </label>
    <select id="s3bkt-anon-access">
      <option value="none">Private — no anonymous access (recommended)</option>
      <option value="download">Public Read — anyone can download</option>
      <option value="public">Public R/W — anyone can read and write</option>
    </select>
  </div>

  <!-- User Access -->
  <div>
    <label style="font-size:13px;font-weight:600;margin-bottom:8px;display:block;">
      Allowed Users
      <i class="info-tip" data-tip="S3 users that will have read/write access to this bucket via an IAM policy. If no users are selected, only the root user can access the bucket."></i>
    </label>
    <div id="s3bkt-users-list" style="display:flex;flex-wrap:wrap;gap:8px;">
      <!-- Populated from /api/minio/users — one checkbox chip per user -->
    </div>
    <div style="font-size:12px;color:var(--text-3);margin-top:6px;">
      No users selected = only root admin can access this bucket.
    </div>
  </div>

</div>
<div class="modal-footer">
  <button onclick="closeModal()">Cancel</button>
  <button id="s3bkt-submit-btn" class="btn btn-primary" onclick="submitS3Bucket()">Create Bucket</button>
</div>
```

#### `_s3BktVersioningChange()`

```js
function _s3BktVersioningChange() {
    const v = document.getElementById('s3bkt-versioning').value;
    const lockSel = document.getElementById('s3bkt-object-lock');
    lockSel.disabled = (v !== 'enabled');
    if (v !== 'enabled') lockSel.value = 'false';
}
```

#### `openNewS3BucketModal()`

```js
async function openNewS3BucketModal() {
    document.getElementById('s3bkt-modal-title').textContent = 'New S3 Bucket';
    document.getElementById('s3bkt-submit-btn').textContent  = 'Create Bucket';
    document.getElementById('s3bkt-name').disabled = false;
    // Clear all fields to defaults
    document.getElementById('s3bkt-name').value        = '';
    document.getElementById('s3bkt-comment').value     = '';
    document.getElementById('s3bkt-versioning').value  = 'off';
    document.getElementById('s3bkt-object-lock').value = 'false';
    document.getElementById('s3bkt-quota').value       = '';
    document.getElementById('s3bkt-anon-access').value = 'none';
    _s3BktVersioningChange();
    await _populateS3UserChips([]);
    openModal('modal-s3-bucket');
}
```

#### `openEditS3BucketModal(bucket)`

```js
async function openEditS3BucketModal(bucket) {
    document.getElementById('s3bkt-modal-title').textContent = 'Edit S3 Bucket — ' + bucket.name;
    document.getElementById('s3bkt-submit-btn').textContent  = 'Save Changes';
    document.getElementById('s3bkt-name').value    = bucket.name;
    document.getElementById('s3bkt-name').disabled = true; // name immutable
    document.getElementById('s3bkt-comment').value     = bucket.comment || '';
    document.getElementById('s3bkt-versioning').value  = bucket.versioning || 'off';
    document.getElementById('s3bkt-object-lock').value = bucket.object_lock ? 'true' : 'false';
    document.getElementById('s3bkt-object-lock').disabled = true; // immutable after creation
    document.getElementById('s3bkt-quota').value       = bucket.quota || '';
    document.getElementById('s3bkt-anon-access').value = bucket.anon_access || 'none';
    _s3BktVersioningChange();
    await _populateS3UserChips(bucket.user_keys || []);
    openModal('modal-s3-bucket');
}
```

#### `_populateS3UserChips(selectedKeys)`

```js
async function _populateS3UserChips(selectedKeys) {
    const container = document.getElementById('s3bkt-users-list');
    container.innerHTML = '<span style="color:var(--text-3);font-size:13px;">Loading users…</span>';
    const r = await fetch('/api/minio/users');
    if (!r.ok) { container.innerHTML = '<span style="color:var(--accent-danger);font-size:12px;">Could not load users.</span>'; return; }
    const users = await r.json();
    if (!users.length) { container.innerHTML = '<span style="color:var(--text-3);font-size:12px;">No S3 users yet. Add users via "S3 Users".</span>'; return; }
    container.innerHTML = users.map(u => {
        const checked = selectedKeys.includes(u.accessKey);
        return `<label style="display:flex;align-items:center;gap:6px;padding:5px 10px;border:1px solid var(--border);border-radius:var(--radius-sm);cursor:pointer;font-size:13px;">
          <input type="checkbox" data-key="${esc(u.accessKey)}" ${checked?'checked':''}>
          <span style="font-family:monospace;">${esc(u.accessKey)}</span>
        </label>`;
    }).join('');
}
```

#### `submitS3Bucket()`

```js
async function submitS3Bucket() {
    const name    = document.getElementById('s3bkt-name').value.trim();
    const isEdit  = document.getElementById('s3bkt-name').disabled;
    const checkedKeys = Array.from(
        document.querySelectorAll('#s3bkt-users-list input[type=checkbox]:checked')
    ).map(cb => cb.dataset.key);

    const body = {
        name:        name,
        comment:     document.getElementById('s3bkt-comment').value.trim(),
        versioning:  document.getElementById('s3bkt-versioning').value,
        object_lock: document.getElementById('s3bkt-object-lock').value === 'true',
        quota:       document.getElementById('s3bkt-quota').value.trim(),
        anon_access: document.getElementById('s3bkt-anon-access').value,
        user_keys:   checkedKeys,
    };

    const endpoint = isEdit ? '/api/minio/bucket/edit' : '/api/minio/bucket/create';
    // POST endpoint with body
    // On success: closeModal(), loadS3Buckets(), _updateS3NavDot()
}
```

#### Delete Confirmation

```js
function confirmDeleteS3Bucket(name) {
    // Show a simple confirm dialog:
    // "Delete bucket '<name>' and ALL its objects? This cannot be undone."
    // Two buttons: Cancel | Delete Bucket (danger red)
    // On confirm: POST /api/minio/bucket/delete { name }, then loadS3Buckets(), _updateS3NavDot()
}
```

---

## New Files Summary

| File | Purpose |
|------|---------|
| `system/minio.go` | `MinIOPrereqsInstalled`, `InstallMinIO`, service control, `ApplyMinIOConfig`, `WriteMinIOEnvFile`, `WriteMinIOServiceUnit`, `SetupMCAlias`, all `mc` command wrappers for users and buckets |
| `handlers/minio.go` | All MinIO API handlers (status, service, config, users CRUD, buckets CRUD) |

## Modified Files Summary

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `S3User`, `S3Bucket`, `MinIOConfig`; add `MinIO MinIOConfig` to `AppConfig`; default values in `DefaultAppConfig()` |
| `internal/audit/audit.go` | Add 6 new action constants |
| `handlers/prereqs.go` | Add `minio` to install allowlist, call `system.InstallMinIO()` for that package |
| `handlers/router.go` | Register 13 new MinIO routes |
| `static/index.html` | S3 Buckets sidebar nav item; `navS3()`; `_updateS3NavDot()`; S3 Buckets page HTML; Configure modal; S3 Users modal; New/Edit S3 Bucket modal; all JS functions; call `_updateS3NavDot()` from `initApp()`; add `if (name === 's3') loadS3Page()` to `showPage()` |
| `static/style.css` | S3 icon sizing; bucket chip styles (if not already covered by existing chip/badge styles) |

## New API Routes

```
GET  /api/minio/status             → prereqs, enabled, service status, bucket count
POST /api/minio/service            → start/stop/restart MinIO service (admin)
GET  /api/minio/config             → get MinIO configuration (password redacted)
POST /api/minio/config             → save + apply MinIO configuration (admin)
GET  /api/minio/users              → list all S3 IAM users from MinIO
POST /api/minio/user/create        → create S3 user (admin)
POST /api/minio/user/delete        → delete S3 user (admin)
POST /api/minio/user/status        → enable/disable S3 user (admin)
POST /api/minio/user/password      → change S3 user secret key (admin)
GET  /api/minio/buckets            → list buckets (live from MinIO + portal metadata)
POST /api/minio/bucket/create      → create bucket + set versioning/lock/quota/policy (admin)
POST /api/minio/bucket/delete      → delete bucket and all objects (admin)
POST /api/minio/bucket/edit        → update bucket settings + user policy (admin)
```

---

## Implementation Order

1. `internal/config/config.go` — add `S3User`, `S3Bucket`, `MinIOConfig`, `AppConfig.MinIO`, defaults
2. `internal/audit/audit.go` — add 6 new action constants
3. `system/minio.go` — `MinIOPrereqsInstalled`, `InstallMinIO`, service control, `WriteMinIOEnvFile`, `WriteMinIOServiceUnit`, `ApplyMinIOConfig`, `SetupMCAlias`, all `mc` wrappers
4. `handlers/minio.go` — all 13 handlers
5. `handlers/prereqs.go` — extend allowlist + minio install branch
6. `handlers/router.go` — register all new routes
7. `static/index.html` — S3 Buckets sidebar nav item + `navS3()` + `_updateS3NavDot()`
8. `static/index.html` — S3 Buckets page HTML (prereq banner, configure banner, header with buttons, table)
9. `static/index.html` — Configure modal HTML + `openS3ConfigureModal()` + `submitS3Configure()`
10. `static/index.html` — S3 Users modal HTML + `openS3UsersModal()` + `_loadS3UsersTable()` + `submitCreateS3User()` + `toggleS3UserStatus()` + `confirmDeleteS3User()` + `openS3ChangePasswordModal()`
11. `static/index.html` — New/Edit S3 Bucket modal HTML + all bucket JS functions
12. `static/index.html` — Prerequisites Optional Features row for minio + `_updateMinIOPrereqStatus()` + `enableMinIOFeature()`
13. `static/index.html` — Wire `_updateS3NavDot()` into `initApp()`, `loadS3Page()`, service actions
14. `static/style.css` — any missing icon/chip styles
15. Build, test, deploy

## Status: COMPLETE
