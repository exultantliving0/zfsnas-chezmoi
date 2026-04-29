package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry represents a single audit log event.
type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	System    string    `json:"system,omitempty"` // hostname of the originating server (v6.4.28+: stamped at Read time for the local host; populated from peer name when merged from /api/audit/aggregate)
	User      string    `json:"user"`
	Role      string    `json:"role"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Result    string    `json:"result"` // "ok" or "error"
	Details   string    `json:"details,omitempty"`
}

const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Common action names.
const (
	ActionLogin        = "login"
	ActionLogout       = "logout"
	ActionLoginFailed  = "login_failed"
	ActionSetupAdmin   = "setup_admin"
	ActionCreateUser   = "create_user"
	ActionDeleteUser   = "delete_user"
	ActionUpdateUser   = "update_user"
	ActionKillSession  = "kill_session"
	ActionCreatePool   = "create_pool"
	ActionImportPool   = "import_pool"
	ActionCreateDataset = "create_dataset"
	ActionUpdateDataset = "update_dataset"
	ActionDeleteDataset = "delete_dataset"
	ActionCreateShare  = "create_share"
	ActionDeleteShare  = "delete_share"
	ActionEnableShare  = "enable_share"
	ActionDisableShare = "disable_share"
	ActionCreateSnapshot = "create_snapshot"
	ActionDeleteSnapshot = "delete_snapshot"
	ActionRestoreSnapshot = "restore_snapshot"
	ActionInstallPrereqs = "install_prereqs"
	ActionInstallService = "install_service"
	ActionApplyUpdates  = "apply_updates"
	ActionGrowPool      = "grow_pool"
	ActionExportPool    = "export_pool"
	ActionDestroyPool   = "destroy_pool"
	ActionUpgradePool   = "upgrade_pool"
	ActionUpdatePool    = "update_pool"
	ActionSystemReboot  = "system_reboot"
	ActionSystemShutdown = "system_shutdown"
	ActionUpdateSettings = "update_settings"
	ActionCreateNFSShare  = "create_nfs_share"
	ActionUpdateNFSShare  = "update_nfs_share"
	ActionDeleteNFSShare  = "delete_nfs_share"
	ActionCreateSchedule  = "create_schedule"
	ActionUpdateSchedule  = "update_schedule"
	ActionDeleteSchedule  = "delete_schedule"
	// Health events — logged automatically by the background health poller.
	ActionPoolProblem    = "pool_problem"
	ActionPoolRecovered  = "pool_recovered"
	ActionDiskProblem    = "disk_problem"
	ActionDiskRecovered  = "disk_recovered"
	ActionFolderScan         = "folder_scan"
	ActionTreeMapScheduleScan = "treemap_schedule_scan"
	// 2FA events.
	Action2FAEnabled  = "2fa_enabled"
	Action2FADisabled = "2fa_disabled"
	// Encryption key events.
	ActionGenerateKey = "generate_key"
	ActionImportKey   = "import_key"
	ActionDeleteKey   = "delete_key"
	ActionLoadKey     = "load_key"
	ActionUnloadKey   = "unload_key"
	// ZVol events.
	ActionCreateZVol = "create_zvol"
	ActionEditZVol   = "edit_zvol"
	ActionDeleteZVol = "delete_zvol"
	// iSCSI events.
	ActionEditISCSIConfig       = "edit_iscsi_config"
	ActionCreateISCSIShare      = "create_iscsi_share"
	ActionDeleteISCSIShare      = "delete_iscsi_share"
	ActionCreateISCSICredential = "create_iscsi_credential"
	ActionDeleteISCSICredential = "delete_iscsi_credential"
	// InterLink events.
	ActionInterlinkAccepted = "interlink_accepted"
	ActionInterlinkLinked   = "interlink_linked"
	ActionInterlinkUnlinked = "interlink_unlinked"
	// MinIO / S3 Object Server events.
	ActionEditMinIOConfig = "edit_minio_config"
	ActionCreateS3User    = "create_s3_user"
	ActionDeleteS3User    = "delete_s3_user"
	ActionCreateS3Bucket  = "create_s3_bucket"
	ActionDeleteS3Bucket  = "delete_s3_bucket"
	ActionEditS3Bucket    = "edit_s3_bucket"
	// Replication events.
	ActionCreateReplication = "create_replication"
	ActionEditReplication   = "edit_replication"
	ActionDeleteReplication = "delete_replication"
	ActionRunReplication    = "run_replication"
	// UPS events.
	ActionUPSOnBattery = "ups_on_battery"
	ActionUPSShutdown  = "ups_shutdown"
	// Push InterLink events.
	ActionPushInterlink = "push_interlink"
	// Software self-update.
	ActionSoftwareUpdate = "software_update"
	// Sudoers hardening.
	ActionUpdateSudoers = "update_sudoers"
	// File Browser.
	ActionFileBrowserChown = "filebrowser_chown"
	ActionFileBrowserChmod = "filebrowser_chmod"
	// Dataset encryption actions.
	ActionLockDataset    = "lock_dataset"
	ActionUnlockDataset  = "unlock_dataset"
	ActionChangeKeySource = "change_key_source"
	// Access control.
	ActionForbidden = "forbidden_access"
	// LXD / VM events.
	ActionLXDCreateVM        = "lxd_create_vm"
	ActionLXDCreateContainer = "lxd_create_container"
	ActionLXDStart           = "lxd_start"
	ActionLXDStop            = "lxd_stop"
	ActionLXDRestart         = "lxd_restart"
	ActionLXDDelete          = "lxd_delete"
	ActionLXDEditConfig      = "lxd_edit_config"
	ActionLXDNetCreate       = "lxd_net_create"
	ActionLXDNetEdit         = "lxd_net_edit"
	ActionLXDNetDelete       = "lxd_net_delete"
	ActionLXDSnapshot        = "lxd_snapshot"
	ActionLXDRestore         = "lxd_restore"
	ActionLXDDeleteSnapshot  = "lxd_delete_snapshot"
	ActionLXDClone           = "lxd_clone"
	ActionLXDMoveStorage     = "lxd_move_storage"
	ActionProxmoxImport      = "proxmox_import"
	ActionLXDEnable          = "lxd_enable"
	ActionLXDStorageCreate   = "lxd_storage_create"
	ActionLXDStorageEdit     = "lxd_storage_edit"
	ActionLXDStorageDelete   = "lxd_storage_delete"
	ActionLXDMetricsToggle    = "lxd_metrics_toggle"     // v6.4.28 — enable/disable LXD prometheus endpoint + portal scraper
	ActionLXDGlobalConfigEdit = "lxd_global_config_edit" // v6.4.28 — admin edited LXD global config keys
)

var (
	logPath string
	mu      sync.Mutex
)

// Init sets the audit log file path.
func Init(configDir string) {
	logPath = filepath.Join(configDir, "audit.log")
}

// Log appends an entry to the audit log.
func Log(e Entry) {
	e.Timestamp = time.Now()
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit: failed to open log: %v\n", err)
		return
	}
	defer f.Close()

	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s\n", data)
}

// Read loads all audit entries from the log file. Each entry's System
// field is stamped with the local hostname so callers (the multi-host
// aggregate handler in particular) can attribute it correctly.
func Read() ([]Entry, error) {
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}

	host, _ := osHostname()
	var entries []Entry
	lines := splitLines(data)
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err == nil {
			if e.System == "" {
				e.System = host
			}
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// osHostname is a thin wrapper around os.Hostname so callers always get a
// non-error path; an empty string means "couldn't resolve hostname" and
// the System field is left blank for that read.
var osHostname = func() (string, error) { return os.Hostname() }

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
