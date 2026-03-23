package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
	"zfsnas/internal/secret"
)

// ReplicationTask defines a ZFS send/receive replication job to a remote host.
type ReplicationTask struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SourceDataset string    `json:"source_dataset"` // full path: pool/dataset
	RemoteHost    string    `json:"remote_host"`    // hostname or IP
	RemoteUser    string    `json:"remote_user"`    // SSH user (default: root)
	RemoteDataset string    `json:"remote_dataset"` // destination: pool/dataset
	Recursive     bool      `json:"recursive"`      // -R flag: include child datasets
	Compressed    bool      `json:"compressed"`     // -c flag: send compressed stream
	LastSnap      string    `json:"last_snap,omitempty"`    // last successfully sent snapshot (for incremental)
	LastRun       time.Time `json:"last_run,omitempty"`
	LastStatus    string    `json:"last_status,omitempty"` // "ok", "error", "never"
	LastMessage   string    `json:"last_message,omitempty"`
}

const (
	RoleAdmin    = "admin"
	RoleReadOnly = "read-only"
	RoleSMBOnly  = "smb-only"
)

// S3Bucket is a MinIO bucket managed by ZFSNAS and tracked in portal config.
type S3Bucket struct {
	Name      string   `json:"name"`
	Comment   string   `json:"comment"`
	Versioning string  `json:"versioning"`  // "off", "enabled", "suspended"
	ObjectLock bool    `json:"object_lock"` // immutable after creation
	Quota     string   `json:"quota"`       // human string e.g. "50G", "" = unlimited
	AnonAccess string  `json:"anon_access"` // "none", "download", "public"
	UserKeys  []string `json:"user_keys"`
	CreatedAt int64    `json:"created_at"`
}

// MinIOConfig holds all persistent MinIO / S3 Object Server settings.
type MinIOConfig struct {
	Enabled      bool       `json:"enabled"`
	HideNav      bool       `json:"hide_nav"`     // hide nav item when not installed
	DatasetPath  string     `json:"dataset_path"`  // ZFS dataset path used as backend
	DataDir      string     `json:"data_dir"`       // absolute mountpoint of that dataset
	Port         int        `json:"port"`           // API port, default 9000
	ConsolePort  int        `json:"console_port"`   // web console port, default 9001
	TLS          bool       `json:"tls"`            // enable TLS on both ports
	RootUser     string     `json:"root_user"`
	RootPassword string     `json:"root_password"`
	Region       string     `json:"region"`
	SiteName     string     `json:"site_name"`
	ServerURL    string     `json:"server_url"`
	Buckets      []S3Bucket `json:"buckets"`
}

// ISCSIHost is a known initiator that can be granted access to iSCSI shares.
type ISCSIHost struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IQN     string `json:"iqn"`
	Comment string `json:"comment"`
}

// ISCSICredential is a named CHAP authentication credential for iSCSI.
type ISCSICredential struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Method      string `json:"method"`                // "incoming" or "bidirectional"
	InUsername  string `json:"in_username"`           // initiator → target authentication
	InPassword  string `json:"in_password"`
	OutUsername string `json:"out_username,omitempty"` // target → initiator (bidirectional only)
	OutPassword string `json:"out_password,omitempty"`
}

// ISCSIShare is a single exported iSCSI target backed by a ZVol.
type ISCSIShare struct {
	ID        string            `json:"id"`
	ZVol      string            `json:"zvol"`
	IQN       string            `json:"iqn"`
	HostIDs   []string          `json:"host_ids"`
	HostCreds map[string]string `json:"host_creds,omitempty"` // hostID → credID
	Comment   string            `json:"comment"`
	CreatedAt int64             `json:"created_at"`
}

// ISCSIConfig holds all persistent iSCSI settings.
type ISCSIConfig struct {
	Enabled     bool              `json:"enabled"`
	HideNav     bool              `json:"hide_nav"`    // hide nav item when not installed
	BaseName    string            `json:"base_name"`
	Port        int               `json:"port"`
	Hosts       []ISCSIHost       `json:"hosts"`
	Shares      []ISCSIShare      `json:"shares"`
	Credentials []ISCSICredential `json:"credentials,omitempty"`
}

// UPSShutdownPolicy defines when to automatically shut down the system.
type UPSShutdownPolicy struct {
	Enabled           bool   `json:"enabled"`
	TriggerType       string `json:"trigger_type"`        // "time" | "percent" | "both"
	RuntimeThreshold  int    `json:"runtime_threshold"`   // shut down when runtime < N seconds (0 = disabled)
	PercentThreshold  int    `json:"percent_threshold"`   // shut down when charge < N% (0 = disabled)
	PreShutdownCmd    string `json:"pre_shutdown_cmd,omitempty"`
}

// UPSConfig holds all persistent UPS / NUT settings.
type UPSConfig struct {
	Enabled         bool              `json:"enabled"`
	UPSName         string            `json:"ups_name"`
	Driver          string            `json:"driver"`
	Port            string            `json:"port"`
	MonitorPassword string            `json:"monitor_password,omitempty"`
	RawUPSConf      string            `json:"raw_ups_conf,omitempty"` // original nut-scanner output, base for ups.conf
	ShutdownPolicy  UPSShutdownPolicy `json:"shutdown_policy"`
	NominalPowerW   *int              `json:"nominal_power_w,omitempty"` // user-overridable nominal VA/W rating
}

// AppConfig holds top-level application settings.
type AppConfig struct {
	ConfigDir         string    `json:"-"` // runtime-only, not persisted
	Port              int       `json:"port"`
	StorageUnit       string    `json:"storage_unit,omitempty"`        // "gb" (1000-based) or "gib" (1024-based)
	LoginTheme        string    `json:"login_theme,omitempty"`         // "dark" | "light" | "auto"
	SMARTLastRefresh  time.Time `json:"smart_last_refresh,omitempty"`
	WeeklyScrub       bool      `json:"weekly_scrub"`                  // deprecated: migrated to ScrubSchedule
	ScrubSchedule     string    `json:"scrub_schedule,omitempty"`      // weekly | biweekly | monthly | 2months | 4months | "" (off)
	ScrubHour         int       `json:"scrub_hour"`                    // hour of day to run scrub (0-23), default 2
	LiveUpdateEnabled  bool   `json:"live_update_enabled,omitempty"`  // enable in-place binary self-update
	MaxSmbdProcesses   int    `json:"max_smbd_processes,omitempty"`   // Samba max smbd processes (0 = use default 100)
	SMBHomeDataset     string `json:"smb_home_dataset,omitempty"`     // ZFS dataset path for SMB user home folders; "" = disabled
	TreeMapSchedule    string      `json:"treemap_schedule,omitempty"`     // daily | weekly | biweekly | monthly | "" (off)
	TreeMapHour        int         `json:"treemap_hour"`                   // hour of day to run treemap scan (0-23)
	TreeMapMinute      int         `json:"treemap_minute"`                 // minute of hour to run treemap scan (0-59)
	ISCSI              ISCSIConfig      `json:"iscsi,omitempty"`
	MinIO              MinIOConfig      `json:"minio,omitempty"`
	UPS                UPSConfig        `json:"ups,omitempty"`
	ActiveCertName     string           `json:"active_cert_name,omitempty"`
	PendingCertRestart bool             `json:"pending_cert_restart,omitempty"`
	Replication        []ReplicationTask `json:"replication,omitempty"`
}

// UserPreferences holds per-user UI preferences persisted across sessions.
type UserPreferences struct {
	ActivityBarCollapsed bool   `json:"activity_bar_collapsed,omitempty"`
	SelectedPool         string `json:"selected_pool,omitempty"`          // last pool shown in Pool tab
	SelectedTopBarPool   string `json:"selected_top_bar_pool,omitempty"`  // last pool shown in top bar
}

// User represents a portal or SMB-only user.
type User struct {
	ID           string          `json:"id"`
	Username     string          `json:"username"`
	Email        string          `json:"email"`
	PasswordHash string          `json:"password_hash"`
	Role         string          `json:"role"` // admin, read-only, smb-only
	CreatedAt    time.Time       `json:"created_at"`
	Preferences  UserPreferences `json:"preferences,omitempty"`
	TOTPSecret    string          `json:"totp_secret,omitempty"`     // base32-encoded TOTP secret
	TOTPEnabled   bool            `json:"totp_enabled,omitempty"`    // 2FA active
	SMBHomeFolder bool            `json:"smb_home_folder,omitempty"` // home dir under SMBHomeDataset
}

// EncryptionKey is metadata for a stored ZFS encryption key file.
// The raw 32-byte key is stored separately in config/keys/<ID>.key.
type EncryptionKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// LoadEncryptionKeys loads all encryption key metadata from disk.
func LoadEncryptionKeys() ([]EncryptionKey, error) {
	var keys []EncryptionKey
	if err := loadJSON("encryption_keys.json", &keys); err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []EncryptionKey{}
	}
	return keys, nil
}

// SaveEncryptionKeys persists encryption key metadata to disk.
func SaveEncryptionKeys(keys []EncryptionKey) error {
	return saveJSON("encryption_keys.json", keys)
}

var (
	configDir string
	mu        sync.RWMutex
	totpKey   []byte // AES-256 key for TOTP secret encryption
)

// Init creates the config directory, stores its path, and loads the TOTP encryption key.
func Init(dir string) error {
	configDir = dir
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	key, err := secret.LoadOrCreateKey(filepath.Join(dir, "totp.key"))
	if err != nil {
		log.Printf("[config] warning: could not load/create TOTP key: %v — secrets stored unencrypted", err)
	} else {
		totpKey = key
	}
	return nil
}

// Dir returns the current config directory path.
func Dir() string {
	return configDir
}

func loadJSON(filename string, v interface{}) error {
	path := filepath.Join(configDir, filename)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func saveJSON(filename string, v interface{}) error {
	mu.Lock()
	defer mu.Unlock()
	path := filepath.Join(configDir, filename)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0640)
}

// LoadAppConfig loads or initializes application config with defaults.
func LoadAppConfig() (*AppConfig, error) {
	// Detect whether the config file already exists before loading, so we can
	// distinguish a fresh install (apply all defaults) from an existing config
	// that has WeeklyScrub explicitly set to false.
	fresh := false
	if _, err := os.Stat(filepath.Join(configDir, "config.json")); os.IsNotExist(err) {
		fresh = true
	}

	cfg := &AppConfig{Port: 8443}
	if err := loadJSON("config.json", cfg); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = 8443
	}
	if cfg.StorageUnit == "" {
		cfg.StorageUnit = "gb"
	}
	if cfg.MaxSmbdProcesses == 0 {
		cfg.MaxSmbdProcesses = 100
	}
	if cfg.ISCSI.BaseName == "" {
		cfg.ISCSI.BaseName = "iqn.2003-06.ca.chezmoi.zfsnas"
	}
	if cfg.ISCSI.Port == 0 {
		cfg.ISCSI.Port = 3260
	}
	if cfg.ISCSI.Hosts == nil {
		cfg.ISCSI.Hosts = []ISCSIHost{}
	}
	if cfg.ISCSI.Shares == nil {
		cfg.ISCSI.Shares = []ISCSIShare{}
	}
	if cfg.ISCSI.Credentials == nil {
		cfg.ISCSI.Credentials = []ISCSICredential{}
	}
	if cfg.Replication == nil {
		cfg.Replication = []ReplicationTask{}
	}
	if cfg.MinIO.Port == 0 {
		cfg.MinIO.Port = 9000
	}
	if cfg.MinIO.ConsolePort == 0 {
		cfg.MinIO.ConsolePort = 9001
	}
	if cfg.MinIO.Region == "" {
		cfg.MinIO.Region = "us-east-1"
	}
	if cfg.MinIO.RootUser == "" {
		cfg.MinIO.RootUser = "minioadmin"
	}
	if cfg.MinIO.Buckets == nil {
		cfg.MinIO.Buckets = []S3Bucket{}
	}
	// Migrate legacy WeeklyScrub bool to ScrubSchedule string.
	if cfg.ScrubSchedule == "" {
		if fresh {
			// Fresh install: default to weekly at 02:00
			cfg.ScrubSchedule = "weekly"
			cfg.ScrubHour = 2
		} else if cfg.WeeklyScrub {
			// Existing config with weekly scrub enabled → migrate
			cfg.ScrubSchedule = "weekly"
			cfg.ScrubHour = 2
		}
		// If WeeklyScrub was false, ScrubSchedule stays "" (off)
	}
	return cfg, nil
}

// APIKeyEntry represents a named API key used by external integrations (e.g. homepage widget).
type APIKeyEntry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// LoadAPIKeys loads all API keys from disk.
func LoadAPIKeys() ([]APIKeyEntry, error) {
	var keys []APIKeyEntry
	if err := loadJSON("api_keys.json", &keys); err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []APIKeyEntry{}
	}
	return keys, nil
}

// SaveAPIKeys persists all API keys to disk.
func SaveAPIKeys(keys []APIKeyEntry) error {
	return saveJSON("api_keys.json", keys)
}

// SaveAppConfig persists application config.
func SaveAppConfig(cfg *AppConfig) error {
	return saveJSON("config.json", cfg)
}

// LoadUsers loads all users from disk, decrypting TOTP secrets if encrypted.
func LoadUsers() ([]User, error) {
	var users []User
	if err := loadJSON("users.json", &users); err != nil {
		return nil, err
	}
	if users == nil {
		users = []User{}
	}
	// Decrypt TOTP secrets. Legacy plaintext secrets are left as-is.
	if totpKey != nil {
		for i := range users {
			if secret.IsEncrypted(users[i].TOTPSecret) {
				if plain, err := secret.Decrypt(totpKey, users[i].TOTPSecret); err == nil {
					users[i].TOTPSecret = plain
				}
			}
		}
	}
	return users, nil
}

// SaveUsers persists all users to disk, encrypting TOTP secrets if a key is available.
func SaveUsers(users []User) error {
	if totpKey == nil {
		return saveJSON("users.json", users)
	}
	// Encrypt on a copy so we don't modify the caller's slice.
	toWrite := make([]User, len(users))
	copy(toWrite, users)
	for i := range toWrite {
		s := toWrite[i].TOTPSecret
		if s != "" && !secret.IsEncrypted(s) {
			if enc, err := secret.Encrypt(totpKey, s); err == nil {
				toWrite[i].TOTPSecret = enc
			}
		}
	}
	return saveJSON("users.json", toWrite)
}

// FindUserByUsername returns the user with the given username, or nil.
func FindUserByUsername(users []User, username string) *User {
	for i := range users {
		if users[i].Username == username {
			return &users[i]
		}
	}
	return nil
}

// FindUserByID returns the user with the given ID, or nil.
func FindUserByID(users []User, id string) *User {
	for i := range users {
		if users[i].ID == id {
			return &users[i]
		}
	}
	return nil
}
