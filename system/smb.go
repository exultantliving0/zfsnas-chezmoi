package system

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	smbConfPath          = "/etc/samba/smb.conf"
	smbBeginMarker       = "# ===== ZFS NAS MANAGED SHARES BEGIN ====="
	smbEndMarker         = "# ===== ZFS NAS MANAGED SHARES END ====="
	smbGlobalBeginMarker = "# ===== ZFS NAS MANAGED GLOBAL BEGIN ====="
	smbGlobalEndMarker   = "# ===== ZFS NAS MANAGED GLOBAL END ====="
)

// SMBShare represents a Samba file share.
type SMBShare struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Comment    string   `json:"comment"`
	Browseable bool     `json:"browseable"`
	ReadOnly   bool     `json:"read_only"`
	ValidUsers []string `json:"valid_users"`
	GuestOK    bool     `json:"guest_ok"`

	// Time Machine
	TimeMachine bool `json:"time_machine"`
	TMQuotaGB   int  `json:"tm_quota_gb"` // 0 = unlimited

	// Recycle Bin
	RecycleBin         bool `json:"recycle_bin"`
	RecycleRetainDays  int  `json:"recycle_retain_days"` // 0 = keep forever

	// SMB2/3 Durable Handles (posix locking = no)
	DurableHandles bool `json:"durable_handles"`

	// Apple-style character encoding (vfs catia)
	AppleEncoding bool `json:"apple_encoding"`

	// Host access control
	AllowedHosts string `json:"allowed_hosts"` // space-separated IPs/hostnames/subnets
	HostsDeny    string `json:"hosts_deny"`
}

func smbSharesPath(configDir string) string {
	return filepath.Join(configDir, "shares.json")
}

// ListSMBShares returns all configured SMB shares from the JSON store.
func ListSMBShares(configDir string) ([]SMBShare, error) {
	data, err := os.ReadFile(smbSharesPath(configDir))
	if os.IsNotExist(err) {
		return []SMBShare{}, nil
	}
	if err != nil {
		return nil, err
	}
	var shares []SMBShare
	if err := json.Unmarshal(data, &shares); err != nil {
		return nil, err
	}
	if shares == nil {
		return []SMBShare{}, nil
	}
	return shares, nil
}

// SaveSMBShares persists shares to JSON and applies them to smb.conf.
func SaveSMBShares(configDir string, shares []SMBShare) error {
	data, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(smbSharesPath(configDir), data, 0640); err != nil {
		return err
	}
	return applySMBConf(shares)
}

// applySMBConf writes the managed section into /etc/samba/smb.conf.
func applySMBConf(shares []SMBShare) error {
	// Build the managed block.
	var sb strings.Builder
	sb.WriteString(smbBeginMarker + "\n")
	for _, s := range shares {
		sb.WriteString(fmt.Sprintf("\n[%s]\n", s.Name))
		if s.Comment != "" {
			sb.WriteString("   comment = " + s.Comment + "\n")
		}
		sb.WriteString("   path = " + s.Path + "\n")
		sb.WriteString("   browseable = " + boolSMB(s.Browseable) + "\n")
		sb.WriteString("   read only = " + boolSMB(s.ReadOnly) + "\n")
		sb.WriteString("   guest ok = " + boolSMB(s.GuestOK) + "\n")
		if len(s.ValidUsers) > 0 {
			sb.WriteString("   valid users = " + strings.Join(s.ValidUsers, ", ") + "\n")
		}
		sb.WriteString("   create mask = 0664\n")
		sb.WriteString("   directory mask = 0775\n")
		sb.WriteString("   force group = sambashare\n")

		// SMB2/3 Durable Handles — requires posix locking = no
		if s.DurableHandles {
			sb.WriteString("   posix locking = no\n")
		}

		// Host access control
		if s.AllowedHosts != "" {
			sb.WriteString("   hosts allow = " + s.AllowedHosts + "\n")
		}
		if s.HostsDeny != "" {
			sb.WriteString("   hosts deny = " + s.HostsDeny + "\n")
		}

		// VFS objects (combine as needed)
		var vfsObjs []string
		if s.AppleEncoding {
			vfsObjs = append(vfsObjs, "catia")
		}
		if s.RecycleBin {
			vfsObjs = append(vfsObjs, "recycle")
		}
		if s.TimeMachine {
			vfsObjs = append(vfsObjs, "fruit", "streams_xattr")
		}
		if len(vfsObjs) > 0 {
			sb.WriteString("   vfs objects = " + strings.Join(vfsObjs, " ") + "\n")
		}

		// Apple-style character encoding (catia)
		if s.AppleEncoding {
			sb.WriteString("   catia:mappings = 0x22:0xf022,0x2a:0xf02a,0x2f:0xf02f,0x3a:0xf03a,0x3c:0xf03c,0x3e:0xf03e,0x3f:0xf03f,0x5c:0xf05c,0x7c:0xf07c\n")
		}

		// Recycle Bin
		if s.RecycleBin {
			sb.WriteString("   recycle:repository = .recycle\n")
			sb.WriteString("   recycle:keeptree = yes\n")
			sb.WriteString("   recycle:versions = yes\n")
			sb.WriteString("   recycle:touch = yes\n")
			sb.WriteString("   recycle:directory_mode = 2770\n")
			sb.WriteString("   recycle:subdir_mode = 2770\n")
			sb.WriteString("   recycle:maxsize = 0\n")
		}

		// Time Machine
		if s.TimeMachine {
			sb.WriteString("   fruit:time machine = yes\n")
			if s.TMQuotaGB > 0 {
				sb.WriteString(fmt.Sprintf("   fruit:time machine max size = %dG\n", s.TMQuotaGB))
			}
		}
	}
	sb.WriteString("\n" + smbEndMarker + "\n")
	managed := sb.String()

	// Read existing smb.conf (readable without sudo on most systems).
	existing, err := os.ReadFile(smbConfPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read smb.conf: %w", err)
	}
	conf := string(existing)

	// Replace or append the managed section.
	begin := strings.Index(conf, smbBeginMarker)
	end := strings.Index(conf, smbEndMarker)
	var newConf string
	if begin >= 0 && end > begin {
		newConf = conf[:begin] + managed + conf[end+len(smbEndMarker):]
		// Trim any double newlines left by removal.
		newConf = strings.ReplaceAll(newConf, "\n\n\n", "\n\n")
	} else {
		newConf = strings.TrimRight(conf, "\n") + "\n\n" + managed
	}

	// If the managed global section is not yet in the file, seed it now with
	// the default value (100) so the parameter is always present from the
	// moment the first share is configured.
	if !strings.Contains(newConf, smbGlobalBeginMarker) {
		global := fmt.Sprintf("%s\n[global]\n   max smbd processes = 100\n%s\n",
			smbGlobalBeginMarker, smbGlobalEndMarker)
		newConf = strings.TrimRight(newConf, "\n") + "\n\n" + global
	}


	return writeFileSudo(smbConfPath, newConf)
}

// ApplySmbGlobal writes a managed block into smb.conf that contains the
// [global] performance parameters and, when homeDataset is non-empty and
// homeUsers is non-empty, a [homes] section restricted to those users.
// Samba merges multiple [global] sections (later values win), so this block
// is safe alongside the distro-written [global].
func ApplySmbGlobal(maxSmbdProcesses int, homeDataset string, homeUsers []string) error {
	var sb strings.Builder
	sb.WriteString(smbGlobalBeginMarker + "\n")
	sb.WriteString(fmt.Sprintf("[global]\n   max smbd processes = %d\n", maxSmbdProcesses))

	if homeDataset != "" && len(homeUsers) > 0 {
		mountpoint := datasetMountpoint(homeDataset)
		if mountpoint != "" {
			sb.WriteString("\n[homes]\n")
			sb.WriteString("   comment = User Home Directories\n")
			sb.WriteString(fmt.Sprintf("   path = %s/%%U\n", mountpoint))
			sb.WriteString("   valid users = " + strings.Join(homeUsers, " ") + "\n")
			sb.WriteString("   read only = no\n")
			sb.WriteString("   browseable = no\n")
			sb.WriteString("   create mask = 0700\n")
			sb.WriteString("   directory mask = 0700\n")
		}
	}
	sb.WriteString(smbGlobalEndMarker + "\n")
	managed := sb.String()

	existing, err := os.ReadFile(smbConfPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read smb.conf: %w", err)
	}
	conf := string(existing)

	begin := strings.Index(conf, smbGlobalBeginMarker)
	end   := strings.Index(conf, smbGlobalEndMarker)
	var newConf string
	if begin >= 0 && end > begin {
		newConf = conf[:begin] + managed + conf[end+len(smbGlobalEndMarker):]
		newConf = strings.ReplaceAll(newConf, "\n\n\n", "\n\n")
	} else {
		newConf = strings.TrimRight(conf, "\n") + "\n\n" + managed
	}

	// When our managed [homes] is active, comment out any pre-existing [homes]
	// sections outside the managed block so Samba doesn't merge them.
	if homeDataset != "" {
		newConf = commentOutHomesOutsideManaged(newConf, smbGlobalBeginMarker, smbGlobalEndMarker)
	}

	return writeFileSudo(smbConfPath, newConf)
}

// commentOutHomesOutsideManaged scans conf line-by-line and prefixes with "; "
// any [homes] section (header + body) that lies outside the managed markers.
// Lines already commented with # or ; are left untouched.
func commentOutHomesOutsideManaged(conf, managedBegin, managedEnd string) string {
	lines := strings.Split(conf, "\n")
	var out strings.Builder
	inManaged := false
	inHomes   := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Track managed block boundaries.
		if strings.Contains(line, managedBegin) {
			inManaged = true
			inHomes   = false
			out.WriteString(line + "\n")
			continue
		}
		if strings.Contains(line, managedEnd) {
			inManaged = false
			out.WriteString(line + "\n")
			continue
		}
		if inManaged {
			out.WriteString(line + "\n")
			continue
		}
		// Outside managed block: detect section headers.
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section := strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			inHomes = (section == "homes")
		}
		// Comment out any active line that belongs to a [homes] section.
		if inHomes && trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ";") {
			out.WriteString("; " + line + "\n")
		} else {
			out.WriteString(line + "\n")
		}
	}
	result := out.String()
	// Remove the trailing newline added by the last iteration.
	return strings.TrimSuffix(result, "\n")
}

// datasetMountpoint returns the mountpoint of a ZFS dataset, or "" on error.
func datasetMountpoint(dataset string) string {
	out, err := exec.Command("sudo", "zfs", "get", "-H", "-o", "value", "mountpoint", dataset).Output()
	if err != nil {
		return ""
	}
	mp := strings.TrimSpace(string(out))
	if mp == "none" || mp == "-" || mp == "" {
		return ""
	}
	return mp
}

// EnsureSMBHomeDir creates <mountpoint>/<username>/ under the given ZFS dataset
// if it does not already exist, and sets ownership to the Linux user.
// The Linux user must already exist (created via EnsureSambaUser or useradd).
func EnsureSMBHomeDir(dataset, username string) error {
	mountpoint := datasetMountpoint(dataset)
	if mountpoint == "" {
		return fmt.Errorf("cannot determine mountpoint for dataset %q", dataset)
	}
	dir := mountpoint + "/" + username
	if out, err := exec.Command("sudo", "mkdir", "-p", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir %s: %s", dir, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("sudo", "chmod", "0700", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod %s: %s", dir, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("sudo", "chown", username+":"+username, dir).CombinedOutput(); err != nil {
		// Non-fatal: Linux user may not exist yet (SMB-only users created lazily).
		_ = out
	}
	return nil
}

// ReloadSamba reloads the Samba configuration without dropping connections.
func ReloadSamba() error {
	out, err := exec.Command("sudo", "systemctl", "reload", "smbd").CombinedOutput()
	if err != nil {
		// Fall back to restart if reload fails (smbd not running yet).
		out2, err2 := exec.Command("sudo", "systemctl", "restart", "smbd").CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s / %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

// IsSambaInstalled checks if the smbd binary is available.
func IsSambaInstalled() bool {
	_, err := exec.LookPath("smbd")
	return err == nil
}

// SambaStatus returns "active", "inactive", or "not-installed".
func SambaStatus() string {
	if !IsSambaInstalled() {
		return "not-installed"
	}
	out, err := exec.Command("systemctl", "is-active", "smbd").Output()
	if err != nil {
		return "inactive"
	}
	return strings.TrimSpace(string(out))
}

// ControlSamba runs systemctl start/stop/restart on smbd (and nmbd if present).
func ControlSamba(action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid action: %s", action)
	}
	out, err := exec.Command("sudo", "systemctl", action, "smbd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s smbd: %s", action, strings.TrimSpace(string(out)))
	}
	// nmbd (NetBIOS name service) is optional; ignore errors.
	_ = exec.Command("sudo", "systemctl", action, "nmbd").Run()
	return nil
}

// EnsureSambaUser creates a Linux system account (if absent) and sets its
// Samba password, making the user ready for SMB authentication.
func EnsureSambaUser(username, password string) error {
	// Create a no-login Linux system account if it doesn't exist yet.
	// id exits 0 if user exists, non-zero otherwise.
	if err := exec.Command("id", username).Run(); err != nil {
		out, err2 := exec.Command("sudo", "useradd",
			"-M",                    // no home directory
			"-s", "/usr/sbin/nologin", // no shell login
			username).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("useradd: %s", strings.TrimSpace(string(out)))
		}
	}

	// Add to sambashare group (created by samba package; ignore error if absent).
	_ = exec.Command("sudo", "usermod", "-aG", "sambashare", username).Run()

	// Set / update the Samba password (-s = silent, -a = add or update).
	cmd := exec.Command("sudo", "smbpasswd", "-s", "-a", username)
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("smbpasswd: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ChmodSharePath sets permissions on a share path so SMB clients can read and
// write via the sambashare group. It recursively chgrps the tree to sambashare
// and sets 0770 (owner+group full access, no world access).
func ChmodSharePath(path string) error {
	if out, err := exec.Command("sudo", "chgrp", "-R", "sambashare", path).CombinedOutput(); err != nil {
		return fmt.Errorf("chgrp %s: %s", path, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("sudo", "chmod", "-R", "0770", path).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}

// boolSMB converts a bool to Samba "yes"/"no".
func boolSMB(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// StartRecycleCleaner starts a goroutine that runs at 2 AM daily and removes
// files older than RecycleRetainDays from each share's .recycle directory.
// configDir is passed so it can reload shares dynamically each night.
func StartRecycleCleaner(configDir string) {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			runRecycleCleaner(configDir)
		}
	}()
}

func runRecycleCleaner(configDir string) {
	shares, err := ListSMBShares(configDir)
	if err != nil {
		log.Printf("recycle cleaner: load shares: %v", err)
		return
	}
	for _, s := range shares {
		if !s.RecycleBin || s.RecycleRetainDays <= 0 {
			continue
		}
		recycleDir := filepath.Join(s.Path, ".recycle")
		cutoff := time.Now().AddDate(0, 0, -s.RecycleRetainDays)
		if err := cleanOlderThan(recycleDir, cutoff); err != nil {
			log.Printf("recycle cleaner: %s: %v", recycleDir, err)
		} else {
			log.Printf("recycle cleaner: cleaned %s (older than %d days)", recycleDir, s.RecycleRetainDays)
		}
	}
}

// CleanShareRecycleBin immediately runs the recycle-bin cleanup for a single
// named share, honouring its configured RecycleRetainDays.
func CleanShareRecycleBin(configDir, name string) error {
	shares, err := ListSMBShares(configDir)
	if err != nil {
		return err
	}
	for _, s := range shares {
		if !strings.EqualFold(s.Name, name) {
			continue
		}
		if !s.RecycleBin {
			return fmt.Errorf("share %q does not have a recycle bin configured", name)
		}
		if s.RecycleRetainDays <= 0 {
			return nil // no retention limit — nothing to prune
		}
		recycleDir := filepath.Join(s.Path, ".recycle")
		cutoff := time.Now().AddDate(0, 0, -s.RecycleRetainDays)
		return cleanOlderThan(recycleDir, cutoff)
	}
	return fmt.Errorf("share %q not found", name)
}

// cleanOlderThan removes files (and then empty directories) under dir whose
// mtime is before cutoff.  It uses sudo find so that it can delete files
// owned by arbitrary SMB users without the service account needing ownership.
func cleanOlderThan(dir string, cutoff time.Time) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	days := int(time.Since(cutoff).Hours() / 24)
	mtimeArg := "+" + strconv.Itoa(days)

	// Delete files (and symlinks) older than the cutoff.
	out, err := exec.Command("sudo", "find", dir,
		"-not", "-type", "d",
		"-mtime", mtimeArg,
		"-delete").CombinedOutput()
	if err != nil {
		return fmt.Errorf("find -delete files: %s", strings.TrimSpace(string(out)))
	}

	// Remove any directories that are now empty (ignore errors — best effort).
	_ = exec.Command("sudo", "find", dir,
		"-mindepth", "1", "-type", "d", "-empty", "-delete").Run()

	return nil
}

// ShareClient holds the IP address and optional reverse-DNS hostname of a
// connected SMB or NFS client.
type ShareClient struct {
	IP   string `json:"ip"`
	FQDN string `json:"fqdn,omitempty"`
}

// GetSMBSessions returns active Samba connections grouped by share name.
// It parses "smbstatus -S" output and performs a reverse-DNS lookup for each
// unique client IP.
func GetSMBSessions() map[string][]ShareClient {
	result := make(map[string][]ShareClient)
	out, err := exec.Command("sudo", "smbstatus", "-S").Output()
	if err != nil {
		return result
	}
	seen := make(map[string]map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		svc := fields[0]
		// Skip the header line, separator lines, and the internal IPC$ service.
		if svc == "Service" || strings.HasPrefix(svc, "-") || svc == "IPC$" {
			continue
		}
		machine := fields[2]
		// Strip port suffix when present (e.g. "192.168.1.5:445").
		if h, _, e := net.SplitHostPort(machine); e == nil {
			machine = h
		}
		if seen[svc] == nil {
			seen[svc] = make(map[string]bool)
		}
		if seen[svc][machine] {
			continue
		}
		seen[svc][machine] = true
		result[svc] = append(result[svc], ShareClient{
			IP:   machine,
			FQDN: reverseLookup(machine),
		})
	}
	return result
}

// reverseLookup returns the first PTR record for ip, stripped of its trailing
// dot.  Returns "" when no record exists or DNS is unavailable.
// A 1-second timeout prevents slow/broken reverse-DNS from stalling the table.
func reverseLookup(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimRight(names[0], ".")
}

// writeFileSudo writes content to a path using sudo tee.
func writeFileSudo(path, content string) error {
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %s", path, strings.TrimSpace(stderr.String()))
	}
	return nil
}
