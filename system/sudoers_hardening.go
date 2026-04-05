package system

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SudoersDiff is the result of comparing the installed sudoers file against
// the required template.
type SudoersDiff struct {
	UpToDate     bool              `json:"up_to_date"`
	FileExists   bool              `json:"file_exists"`
	MatchedLines []SudoersLineDiff `json:"matched_lines"` // in both required template and current file
	MissingLines []SudoersLineDiff `json:"missing_lines"`
	ExtraLines   []SudoersLineDiff `json:"extra_lines"`
}

// SudoersLineDiff describes a single logical sudoers command entry that differs.
type SudoersLineDiff struct {
	Line        string `json:"line"`
	Explanation string `json:"explanation"`
	Silenced    bool   `json:"silenced"`
}

// CanManageSudoers returns true when the current process can overwrite
// /etc/sudoers.d/zfsnas. True in three situations:
//  1. Running as root (UID 0)
//  2. Blanket NOPASSWD: ALL sudoers
//  3. Hardened sudoers that already contains tee /etc/sudoers.d/zfsnas
func CanManageSudoers(status SudoStatus) bool {
	switch status.Type {
	case "root", "all":
		return true
	case "hardened":
		out, err := exec.Command("sudo", "-l", "-n").Output()
		if err != nil {
			return false
		}
		sudoList := string(out)
		teePath, err := exec.LookPath("tee")
		if err != nil {
			teePath = "/usr/bin/tee"
		}
		needle := teePath + " /etc/sudoers.d/zfsnas"
		return strings.Contains(sudoList, needle) ||
			strings.Contains(sudoList, "/tee /etc/sudoers.d/zfsnas")
	}
	return false
}

// RequiredSudoersContent returns the canonical sudoers file content for this
// portal. The ZFSNAS_FILES section is generated dynamically: it always includes
// /mnt/* entries, and also adds entries for any ZFS pool mounted directly under
// / (i.e. not under /mnt/).
func RequiredSudoersContent() string {
	return strings.Replace(requiredSudoersTemplate, "{{ZFSNAS_FILES}}", buildFilesAlias(), 1)
}

// buildFilesAlias generates the ZFSNAS_FILES Cmnd_Alias block.
// It scopes find/chown/chmod to /mnt/* and to any pool root mounted outside /mnt/.
func buildFilesAlias() string {
	// Collect pool root mountpoints that sit outside /mnt/.
	var extraPaths []string
	seen := map[string]bool{}
	if datasets, err := ListAllDatasets(); err == nil {
		for _, d := range datasets {
			mp := d.Mountpoint
			if mp == "" || mp == "none" || mp == "-" || mp == "legacy" || mp == "/" {
				continue
			}
			// Only look at pool root datasets (name has no "/" component).
			if strings.Contains(d.Name, "/") {
				continue
			}
			if mp == "/mnt" || strings.HasPrefix(mp, "/mnt/") {
				continue
			}
			if !seen[mp] {
				seen[mp] = true
				extraPaths = append(extraPaths, mp)
			}
		}
	}

	allPaths := append([]string{"/mnt"}, extraPaths...)

	// For each mount base, emit five entries:
	//   find <base>/*             (file browser listing)
	//   chown * <base>/*          (non-recursive ownership change)
	//   chown -R * <base>/*       (recursive)
	//   chmod * <base>/*          (non-recursive mode change)
	//   chmod -R * <base>/*       (recursive)
	var entries []string
	for _, p := range allPaths {
		entries = append(entries,
			fmt.Sprintf("    /usr/bin/find %s/*", p),
			fmt.Sprintf("    /usr/bin/chown * %s/*", p),
			fmt.Sprintf("    /usr/bin/chown -R * %s/*", p),
			fmt.Sprintf("    /usr/bin/chmod * %s/*", p),
			fmt.Sprintf("    /usr/bin/chmod -R * %s/*", p),
		)
	}

	var sb strings.Builder
	sb.WriteString("Cmnd_Alias ZFSNAS_FILES = \\\n")
	for i, e := range entries {
		if i < len(entries)-1 {
			sb.WriteString(e + ", \\\n")
		} else {
			sb.WriteString(e + "\n")
		}
	}
	return sb.String()
}


// GetCurrentSudoersContent reads /etc/sudoers.d/zfsnas.
// Returns ("", nil) when the file does not exist.
func GetCurrentSudoersContent() (string, error) {
	const path = "/etc/sudoers.d/zfsnas"
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if !os.IsNotExist(err) {
		// File exists but isn't readable by us — try via sudo cat.
		out, err2 := exec.Command("sudo", "cat", path).Output()
		if err2 == nil {
			return string(out), nil
		}
	}
	return "", nil
}

// ComputeSudoersDiff diffs current against required, marking silenced entries.
func ComputeSudoersDiff(current, required string, silenced []string) SudoersDiff {
	silencedSet := make(map[string]bool, len(silenced))
	for _, s := range silenced {
		silencedSet[s] = true
	}

	reqCmds := extractSudoCommands(required)
	curCmds := extractSudoCommands(current)

	curSet := make(map[string]bool, len(curCmds))
	for _, c := range curCmds {
		curSet[c] = true
	}
	reqSet := make(map[string]bool, len(reqCmds))
	for _, c := range reqCmds {
		reqSet[c] = true
	}

	var matched, missing, extra []SudoersLineDiff

	for _, c := range reqCmds {
		if curSet[c] {
			matched = append(matched, SudoersLineDiff{
				Line:        c,
				Explanation: lookupSudoersExplanation(c),
			})
		} else {
			missing = append(missing, SudoersLineDiff{
				Line:        c,
				Explanation: lookupSudoersExplanation(c),
				Silenced:    silencedSet[c],
			})
		}
	}
	for _, c := range curCmds {
		if !reqSet[c] {
			extra = append(extra, SudoersLineDiff{
				Line:        c,
				Explanation: lookupSudoersExplanation(c),
				Silenced:    silencedSet[c],
			})
		}
	}

	// Up-to-date means no unsilenced diffs.
	upToDate := true
	for _, d := range missing {
		if !d.Silenced {
			upToDate = false
			break
		}
	}
	if upToDate {
		for _, d := range extra {
			if !d.Silenced {
				upToDate = false
				break
			}
		}
	}

	return SudoersDiff{
		UpToDate:     upToDate,
		FileExists:   current != "",
		MatchedLines: matched,
		MissingLines: missing,
		ExtraLines:   extra,
	}
}

// ApplySudoers assembles the final sudoers content, writes it via sudo tee,
// and validates it with visudo. On validation failure it rolls back.
// silencedMissing: lines from the required template the user chose NOT to add.
// silencedExtra:   lines from the current file the user chose to KEEP.
func ApplySudoers(required string, silencedMissing, silencedExtra []string) error {
	content := buildSudoersContent(required, silencedMissing, silencedExtra)

	// Basic pre-write sanity check: content must have at least one alias and a grant line.
	if err := validateSudoersContent(content); err != nil {
		return fmt.Errorf("content validation failed: %w", err)
	}

	// Write via sudo tee.
	teeCmd := exec.Command("sudo", "tee", "/etc/sudoers.d/zfsnas")
	teeCmd.Stdin = strings.NewReader(content)
	if out, err := teeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tee failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// SudoAllContent returns the minimal single-line sudoers file that grants
// the zfsnas service account unrestricted passwordless sudo access.
func SudoAllContent() string {
	return "# ZFS NAS Portal — unrestricted sudo access\n" +
		"# Applied via the \"Convert to sudo all\" option in the Sudoers Hardening UI.\n" +
		"# This is the most frictionless configuration: the zfsnas service account can\n" +
		"# run any command as root without a password prompt.\n" +
		"zfsnas ALL=(ALL) NOPASSWD: ALL\n"
}

// ApplySudoAll writes the minimal NOPASSWD:ALL sudoers entry for the zfsnas
// user. It bypasses the normal template + visudo pipeline and writes directly
// via sudo tee, then calls visudo -c to validate.
func ApplySudoAll() error {
	content := SudoAllContent()
	teeCmd := exec.Command("sudo", "tee", "/etc/sudoers.d/zfsnas")
	teeCmd.Stdin = strings.NewReader(content)
	if out, err := teeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tee failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RemoveSudoersWriteAccess rewrites /etc/sudoers.d/zfsnas removing the
// "tee /etc/sudoers.d/zfsnas" entry so ZNAS can no longer self-modify sudoers,
// while keeping "cat /etc/sudoers.d/zfsnas" so the diff view still works.
func RemoveSudoersWriteAccess() error {
	const teeLine = "/usr/bin/tee /etc/sudoers.d/zfsnas"

	current, err := GetCurrentSudoersContent()
	if err != nil || current == "" {
		// Nothing to remove.
		return nil
	}

	lines := strings.Split(current, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		norm := normalizeSudoLine(strings.TrimSpace(line))
		if norm == teeLine {
			continue
		}
		kept = append(kept, line)
	}

	// Fix orphaned trailing ", \" on the line that preceded the removed entry.
	for i := 0; i < len(kept); i++ {
		stripped := strings.TrimRight(kept[i], " ")
		if !strings.HasSuffix(stripped, ", \\") {
			continue
		}
		nextIsCmd := false
		for j := i + 1; j < len(kept); j++ {
			t := strings.TrimSpace(kept[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			nextIsCmd = strings.HasPrefix(t, "/")
			break
		}
		if !nextIsCmd {
			kept[i] = strings.TrimRight(stripped[:len(stripped)-3], " ")
		}
	}

	content := strings.Join(kept, "\n")
	if err := validateSudoersContent(content); err != nil {
		return fmt.Errorf("content validation after removal failed: %w", err)
	}

	teeCmd := exec.Command("sudo", "tee", "/etc/sudoers.d/zfsnas")
	teeCmd.Stdin = strings.NewReader(content)
	if out, err := teeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tee failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// BuildSudoersContent is the exported version of buildSudoersContent.
func BuildSudoersContent(required string, silencedMissing, silencedExtra []string) string {
	return buildSudoersContent(required, silencedMissing, silencedExtra)
}

// SudoersContentHash returns the SHA-256 hex digest of the given content.
func SudoersContentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// validateSudoersContent does a lightweight pre-write check on the assembled content.
func validateSudoersContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("assembled content is empty")
	}
	hasAlias := false
	hasGrant := false
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Cmnd_Alias ") {
			hasAlias = true
		}
		if strings.Contains(t, "NOPASSWD:") && !strings.HasPrefix(t, "#") {
			hasGrant = true
		}
	}
	if !hasAlias {
		return fmt.Errorf("no Cmnd_Alias entries found — file would be invalid")
	}
	if !hasGrant {
		return fmt.Errorf("grant line (NOPASSWD:) not found — file would be invalid")
	}
	return nil
}

// buildSudoersContent constructs the final file content from the required template
// minus silenced-missing lines, plus silenced-extra lines appended as a preserved block.
func buildSudoersContent(required string, silencedMissing, silencedExtra []string) string {
	if len(silencedMissing) == 0 && len(silencedExtra) == 0 {
		return required
	}

	silMissingSet := make(map[string]bool, len(silencedMissing))
	for _, cmd := range silencedMissing {
		silMissingSet[cmd] = true
	}

	rawLines := strings.Split(required, "\n")
	kept := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "/") {
			cmd := normalizeSudoLine(trimmed)
			if silMissingSet[cmd] {
				continue // silenced — do not include
			}
		}
		kept = append(kept, line)
	}

	// Fix orphaned trailing ", \" — if a command line ends with ", \" but the
	// next non-blank non-comment line is not another command path, strip it.
	for i := 0; i < len(kept); i++ {
		stripped := strings.TrimRight(kept[i], " ")
		if !strings.HasSuffix(stripped, ", \\") {
			continue
		}
		nextIsCmd := false
		for j := i + 1; j < len(kept); j++ {
			t := strings.TrimSpace(kept[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			nextIsCmd = strings.HasPrefix(t, "/")
			break
		}
		if !nextIsCmd {
			kept[i] = strings.TrimRight(stripped[:len(stripped)-3], " ")
		}
	}

	content := strings.Join(kept, "\n")

	// Append silenced-extra lines as a preserved section.
	if len(silencedExtra) > 0 {
		var sb strings.Builder
		sb.WriteString("\n# ── Preserved entries (kept by Sudoers Hardening) ────────────────────────────\n")
		sb.WriteString("Cmnd_Alias ZFSNAS_EXTRA = \\\n")
		for i, cmd := range silencedExtra {
			if i < len(silencedExtra)-1 {
				sb.WriteString("    " + cmd + ", \\\n")
			} else {
				sb.WriteString("    " + cmd + "\n")
			}
		}
		// Add ZFSNAS_EXTRA to the grant line.
		content = strings.Replace(content, "ZFSNAS_SECURITY", "ZFSNAS_SECURITY, ZFSNAS_EXTRA", 1)
		content = strings.TrimRight(content, "\n") + "\n" + sb.String()
	}

	return content
}

// extractSudoCommands extracts and normalizes individual command paths from
// sudoers content. Only lines that start with "/" (command entries within
// Cmnd_Alias blocks) are extracted.
func extractSudoCommands(content string) []string {
	var cmds []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") {
			continue
		}
		cmd := normalizeSudoLine(trimmed)
		if cmd != "" && !seen[cmd] {
			seen[cmd] = true
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// normalizeSudoLine strips trailing syntax characters (, \ space) and
// normalizes internal whitespace.
func normalizeSudoLine(line string) string {
	s := strings.TrimRight(line, " ,\\")
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), " ")
}

// lookupSudoersExplanation returns a human-readable explanation for a command.
func lookupSudoersExplanation(cmd string) string {
	if exp, ok := sudoersExplanations[cmd]; ok {
		return exp
	}
	// Wildcard prefix match for other patterns ending in " *".
	for pattern, exp := range sudoersExplanations {
		if strings.HasSuffix(pattern, " *") {
			base := strings.TrimSuffix(pattern, " *")
			if strings.HasPrefix(cmd, base+" ") || cmd == base {
				return exp
			}
		}
	}
	return "Command required by the portal — no detailed explanation available for this entry."
}

var sudoersExplanations = map[string]string{
	"/usr/sbin/zpool *":                                  "All ZFS pool operations: create, import, export, scrub, status, offline/online (v1.0.0+). Covers Pool Fixer Wizard clear/replace actions (v6.3.21+).",
	"/usr/sbin/zfs *":                                    "All ZFS dataset operations: create, destroy, set, get, snapshot, rollback, load-key/unload-key (encryption v5.0.0+), send/recv (replication v6.1.0+), allow (delegation v6.3.26+).",
	"/usr/sbin/smartctl *":                               "SMART disk health data for SAS/SATA drives. Required for the Disk Health and SMART detail pages.",
	"/usr/bin/nvme smart-log -o json *":                  "NVMe drive health and temperature data (v3.0.0+).",
	"/usr/bin/apt-get *":                                 "Package installation and OS updates. Used by the Prerequisites tab and the Settings > OS Updates page.",
	"/usr/bin/tee /etc/samba/smb.conf":                   "Writes the Samba configuration file when a share is created, edited, or deleted.",
	"/usr/bin/tee /etc/exports":                          "Writes the NFS export table; exportfs -ra is called immediately after to apply the change.",
	"/usr/bin/tee /etc/systemd/system/zfsnas.service":    "Writes the systemd unit file when the portal registers itself as a system service.",
	"/usr/bin/tee /etc/modprobe.d/zfs.conf":              "Persists ARC size limits across reboots (v6.3.22+).",
	"/usr/bin/tee /sys/module/zfs/parameters/zfs_arc_max": "Applies a new ARC maximum immediately via the ZFS sysfs interface without requiring a reboot.",
	"/usr/bin/tee /sys/module/zfs/parameters/zfs_arc_min": "Applies a new ARC minimum immediately via the ZFS sysfs interface without requiring a reboot.",
	"/usr/bin/tee /etc/sudoers.d/zfsnas":                 "Lets the portal write its own sudoers file when using the Sudoers Hardening feature. Required so in-app changes continue to work after sudo is restricted.",
	"/usr/bin/cat /etc/sudoers.d/zfsnas":                 "Lets the portal read its own sudoers file so the Sudoers Review diff is always accurate and detects manual edits (v6.3.32+).",
	"/usr/sbin/useradd *":                                "Creates the Linux account for a new portal user. Wildcards required for optional --uid/--gid/--no-user-group flags (v3.0.0+).",
	"/usr/sbin/usermod -aG sambashare *":                 "Adds a user to the sambashare group for SMB access.",
	"/usr/sbin/userdel -f *":                             "Deletes a portal user's Linux account (force flag covers locked accounts).",
	"/usr/sbin/groupadd *":                               "Creates a Linux group. Used when creating users with a custom primary group (v3.0.0+).",
	"/usr/sbin/groupdel *":                               "Removes a Linux group when deleting the last user that owned it.",
	"/usr/bin/gpasswd -d * sambashare":                   "Removes a user from the sambashare group during account deletion.",
	"/usr/bin/smbpasswd *":                               "Sets or removes the Samba password for a user.",
	"/usr/bin/smbstatus -S":                              "Lists active SMB sessions per share (v6.1.0+).",
	"/usr/bin/chgrp sambashare *":                        "Sets group ownership of a share directory to sambashare (v6.3.27+).",
	"/usr/sbin/exportfs -ra":                             "Reloads all NFS exports after the export table is updated.",
	"/usr/bin/timedatectl set-timezone *":                "Sets the system timezone from the Settings > System page.",
	"/usr/sbin/shutdown *":                               "Allows scheduled shutdown/reboot from the power menu and from the UPS shutdown watcher.",
	"/usr/sbin/modprobe zfs":                             "Loads the ZFS kernel module after installation if it is not already loaded.",
	"/usr/bin/systemctl restart zfsnas":                  "Restarts the portal service from the power menu (v3.0.0+).",
	"/usr/bin/du -b -d 6 *":                              "Powers the Folder TreeMap — scans dataset mount points for per-folder disk usage.",
	"/usr/sbin/wipefs -a *":                              "Clears all disk signatures before adding a disk to a pool.",
	"/usr/sbin/sgdisk *":                                 "Wipes the GPT partition table before pool creation. Path may differ by distribution.",
	"/usr/bin/dd if=/dev/zero *":                         "Zero-fills the first sectors of a disk during the wipe-disk workflow.",
	"/usr/sbin/partprobe *":                              "Refreshes the kernel partition table after disk changes.",
	"/usr/bin/udevadm settle *":                          "Waits for udev to settle after disk operations before proceeding.",
	"/usr/sbin/blkid -o export":                         "Reads disk UUIDs and filesystem types after partitioning.",
	"/usr/bin/targetcli":                                 "Configures iSCSI LIO targets, backstores, and ACLs (v6.1.0+).",
	"/usr/sbin/nut-scanner *":                            "Scans USB and SNMP buses for attached UPS devices. Requires raw USB access.",
	"/usr/bin/nut-scanner":                               "Scans for attached UPS devices (Debian/Ubuntu path for nut-scanner binary).",
	"/usr/bin/systemctl stop nut-monitor":                "Stops the NUT monitor service (nut-monitor) during UPS config changes or uninstall.",
	"/usr/bin/systemctl disable nut-monitor":             "Disables the NUT monitor service from starting at boot during uninstall.",
	"/usr/bin/tee /etc/nut/ups.conf":                     "Writes the NUT UPS device configuration file.",
	"/usr/bin/tee /etc/nut/nut.conf":                     "Writes the NUT mode configuration file (MODE=standalone or MODE=none).",
	"/usr/bin/tee /etc/nut/upsd.conf":                    "Writes the NUT daemon configuration (listen address, port).",
	"/usr/bin/tee /etc/nut/upsd.users":                   "Writes the NUT user authentication file for upsmon.",
	"/usr/bin/tee /etc/nut/upsmon.conf":                  "Writes the UPS monitor configuration.",
	"/usr/bin/find /mnt/*":                               "File Browser: lists directory contents under /mnt (v6.4.4+). Path-scoped to dataset mount area; the portal validates the target before calling this command.",
	"/usr/bin/chown * /mnt/*":                            "File Browser: non-recursive ownership change on files/folders under /mnt (v6.4.3+). Path-scoped; arbitrary paths cannot be targeted via the UI.",
	"/usr/bin/chown -R * /mnt/*":                         "File Browser: recursive ownership change on a directory tree under /mnt (v6.4.3+). Path-scoped; arbitrary paths cannot be targeted via the UI.",
	"/usr/bin/chmod * /mnt/*":                            "File Browser: non-recursive permission change on files/folders under /mnt (v6.4.3+). Path-scoped; arbitrary paths cannot be targeted via the UI.",
	"/usr/bin/chmod -R * /mnt/*":                         "File Browser: recursive permission change on a directory tree under /mnt (v6.4.3+). Path-scoped; arbitrary paths cannot be targeted via the UI.",
	"/usr/bin/wget -q -O /usr/local/bin/minio *":         "Downloads the MinIO binary during feature install.",
	"/usr/bin/wget -q -O /usr/local/bin/mc *":            "Downloads the MinIO client (mc) binary during feature install.",
	"/usr/bin/tee /etc/systemd/system/minio.service":     "Writes the MinIO systemd service unit file.",
	"/usr/bin/tee /etc/default/minio":                    "Writes the MinIO environment configuration file.",
}

// requiredSudoersTemplate is the canonical sudoers file content for ZFS NAS Portal.
const requiredSudoersTemplate = `# ── ZFS pool & dataset management ────────────────────────────────────────────
# since v1.0.0 — pool creation, import, status, dataset CRUD, snapshots
# since v2.0.0 — zpool scrub (Scrub Management page)
# since v4.0.0 — zpool offline/online/clear (Pool Fixer Wizard)
# since v5.0.0 — zfs load-key / unload-key (native encryption)
# since v6.1.0 — zfs send / zfs recv (remote & local dataset replication)
# since v6.3.21 — zpool replace (Pool Fixer Wizard: disk replacement for DEGRADED pools)
# since v6.3.26 — zfs allow (InterLink push & schedule replication: ZFS delegation to service user)
Cmnd_Alias ZFSNAS_ZFS = \
    /usr/sbin/zpool *, \
    /usr/sbin/zfs *

# ── Samba (SMB shares) ────────────────────────────────────────────────────────
# since v1.0.0 — Samba service control, user provisioning, share config write
# since v3.0.0 — useradd with optional --uid/--gid/--no-user-group (custom UID/GID feature);
#                groupadd -g <gid> for primary group pre-creation;
#                userdel -f / groupdel / gpasswd -d for user deletion
# since v6.0.0 — find (recycle bin cleanup: delete files older than retention in .recycle/)
# since v6.1.0 — smbstatus -S (active session listing on the SMB shares page)
# since v6.3.27 — chgrp/chmod 0770 replace chmod 777 for share path setup;
#                 groupadd --system sambashare ensures the group exists
Cmnd_Alias ZFSNAS_SMB = \
    /usr/bin/systemctl reload smbd, \
    /usr/bin/systemctl restart smbd, \
    /usr/bin/systemctl start smbd, \
    /usr/bin/systemctl stop smbd, \
    /usr/bin/systemctl start nmbd, \
    /usr/bin/systemctl stop nmbd, \
    /usr/sbin/useradd *, \
    /usr/sbin/usermod -aG sambashare *, \
    /usr/sbin/userdel -f *, \
    /usr/sbin/groupadd *, \
    /usr/sbin/groupdel *, \
    /usr/bin/gpasswd -d * sambashare, \
    /usr/bin/smbpasswd *, \
    /usr/bin/chgrp sambashare *, \
    /usr/bin/tee /etc/samba/smb.conf, \
    /usr/bin/smbstatus -S

# ── NFS shares ────────────────────────────────────────────────────────────────
# since v2.0.0 — NFS share management and export config write
# since v6.3.22 — chmod 0777 on dataset path no longer required
Cmnd_Alias ZFSNAS_NFS = \
    /usr/sbin/exportfs -ra, \
    /usr/bin/systemctl start nfs-server, \
    /usr/bin/systemctl stop nfs-server, \
    /usr/bin/tee /etc/exports

# ── SMART & hardware monitoring ───────────────────────────────────────────────
# since v1.0.0 — SMART disk health data (SAS/SATA)
# since v3.0.0 — NVMe health and temperature monitoring
Cmnd_Alias ZFSNAS_SMART = \
    /usr/sbin/smartctl *, \
    /usr/bin/nvme smart-log -o json *

# ── Disk preparation & wipe ───────────────────────────────────────────────────
# since v1.0.0 — wipe and partition a disk before adding it to a pool;
#   wipefs clears signatures, sgdisk creates GPT layout, dd zero-fills,
#   partprobe + udevadm settle kernel/udev state, blkid reads UUIDs
# NOTE: sgdisk lives at /usr/sbin/sgdisk on Debian 12 and at /usr/bin/sgdisk on
#   some Ubuntu releases. Verify the correct path with "which sgdisk" and adjust
#   the two sgdisk lines below if needed.
Cmnd_Alias ZFSNAS_DISK = \
    /usr/sbin/wipefs -a *, \
    /usr/sbin/sgdisk *, \
    /usr/bin/dd if=/dev/zero *, \
    /usr/sbin/partprobe *, \
    /usr/bin/udevadm settle *, \
    /usr/sbin/blkid -o export

# ── iSCSI sharing ─────────────────────────────────────────────────────────────
# since v6.1.0 — iSCSI target management via targetcli-fb; service control for
#   rtslib-fb-targetctl / targetclid / tgt (whichever is installed)
Cmnd_Alias ZFSNAS_ISCSI = \
    /usr/bin/targetcli, \
    /usr/bin/systemctl start rtslib-fb-targetctl, \
    /usr/bin/systemctl stop rtslib-fb-targetctl, \
    /usr/bin/systemctl restart rtslib-fb-targetctl, \
    /usr/bin/systemctl start targetclid, \
    /usr/bin/systemctl stop targetclid, \
    /usr/bin/systemctl restart targetclid, \
    /usr/bin/systemctl start tgt, \
    /usr/bin/systemctl stop tgt, \
    /usr/bin/systemctl restart tgt

# ── S3 Object Server (MinIO) ──────────────────────────────────────────────────
# since v6.3.20 — optional MinIO S3 server; binary download, system account
#   setup, service unit, env file, data directory, TLS certs, and service control
Cmnd_Alias ZFSNAS_MINIO = \
    /usr/bin/wget -q -O /usr/local/bin/minio *, \
    /usr/bin/wget -q -O /usr/local/bin/mc *, \
    /usr/bin/chmod +x /usr/local/bin/minio, \
    /usr/bin/chmod +x /usr/local/bin/mc, \
    /usr/sbin/useradd --system --home-dir /var/lib/minio --shell /usr/sbin/nologin minio-user, \
    /usr/bin/mkdir -p /var/lib/minio, \
    /usr/bin/mkdir -p /var/lib/minio/.minio/certs, \
    /usr/bin/mkdir -p *, \
    /usr/bin/chown minio-user\:minio-user /var/lib/minio, \
    /usr/bin/chown -R minio-user\:minio-user /var/lib/minio/.minio/certs, \
    /usr/bin/chown -R minio-user\:minio-user *, \
    /usr/bin/chown root\:minio-user /etc/default/minio, \
    /usr/bin/chmod 640 /etc/default/minio, \
    /usr/bin/chmod 640 /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/cp * /var/lib/minio/.minio/certs/public.crt, \
    /usr/bin/cp * /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/rm -f /var/lib/minio/.minio/certs/public.crt, \
    /usr/bin/rm -f /var/lib/minio/.minio/certs/private.key, \
    /usr/bin/tee /etc/systemd/system/minio.service, \
    /usr/bin/tee /etc/default/minio, \
    /usr/bin/systemctl enable minio, \
    /usr/bin/systemctl disable minio, \
    /usr/bin/systemctl start minio, \
    /usr/bin/systemctl stop minio, \
    /usr/bin/systemctl restart minio

# ── UPS Management (NUT) ──────────────────────────────────────────────────────
# since v6.3.22 — optional NUT daemon; install, auto-detect UPS, write /etc/nut/
#   config files, service control; uninstall purges packages and removes /etc/nut;
#   udevadm control/trigger used after writing USB udev rules for UPS detection
Cmnd_Alias ZFSNAS_UPS = \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get purge -y nut nut-client nut-server, \
    /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get remove -y nut nut-client, \
    /usr/bin/udevadm control --reload-rules, \
    /usr/bin/udevadm trigger --subsystem-match=usb, \
    /usr/bin/systemctl enable nut-server, \
    /usr/bin/systemctl disable nut-server, \
    /usr/bin/systemctl start nut-server, \
    /usr/bin/systemctl stop nut-server, \
    /usr/bin/systemctl restart nut-server, \
    /usr/bin/systemctl enable nut-client, \
    /usr/bin/systemctl disable nut-client, \
    /usr/bin/systemctl start nut-client, \
    /usr/bin/systemctl stop nut-client, \
    /usr/bin/systemctl enable nut-monitor, \
    /usr/bin/systemctl disable nut-monitor, \
    /usr/bin/systemctl start nut-monitor, \
    /usr/bin/systemctl stop nut-monitor, \
    /usr/bin/systemctl reset-failed, \
    /usr/sbin/nut-scanner *, \
    /usr/bin/nut-scanner, \
    /usr/bin/chown root\:nut /etc/nut, \
    /usr/bin/chmod 750 /etc/nut, \
    /usr/bin/chown root\:nut /etc/nut/nut.conf, \
    /usr/bin/chown root\:nut /etc/nut/ups.conf, \
    /usr/bin/chown root\:nut /etc/nut/upsd.conf, \
    /usr/bin/chown root\:nut /etc/nut/upsd.users, \
    /usr/bin/chown root\:nut /etc/nut/upsmon.conf, \
    /usr/bin/chmod 640 /etc/nut/nut.conf, \
    /usr/bin/chmod 640 /etc/nut/ups.conf, \
    /usr/bin/chmod 640 /etc/nut/upsd.conf, \
    /usr/bin/chmod 640 /etc/nut/upsd.users, \
    /usr/bin/chmod 640 /etc/nut/upsmon.conf, \
    /usr/bin/tee /etc/nut/nut.conf, \
    /usr/bin/tee /etc/nut/upsd.conf, \
    /usr/bin/tee /etc/nut/upsd.users, \
    /usr/bin/tee /etc/nut/upsmon.conf, \
    /usr/bin/tee /etc/nut/ups.conf, \
    /usr/bin/rm -rf /etc/nut, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver.target.wants, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver@.service.d, \
    /usr/bin/rm -rf /etc/systemd/system/nut-driver@*

# ── Folder usage scanning ─────────────────────────────────────────────────────
# since v6.0.0 — Folder TreeMap feature; scans dataset mount points for per-folder sizes
Cmnd_Alias ZFSNAS_SCAN = \
    /usr/bin/du -b -d 6 *

# ── File Browser (v6.4.3+) ────────────────────────────────────────────────────
# since v6.4.3 — chown/chmod on files and folders within dataset mountpoints,
#   SMB share paths, and NFS share paths via the File Browser feature.
# since v6.4.4 — find used to list directory contents so paths not owned by the
#   zfsnas service account can still be browsed.
# since v6.4.4 — paths scoped to /mnt/* (and any pool mounted directly under /)
#   instead of the broad wildcard, limiting access to dataset mount areas only.
#   The portal validates the target path against known share/dataset roots before
#   calling these commands; arbitrary paths cannot be targeted via the UI.
{{ZFSNAS_FILES}}
# ── System management ─────────────────────────────────────────────────────────
# since v1.0.0 — timezone setting, shutdown/reboot from power menu, ZFS kernel module load
# since v3.0.0 — systemctl restart zfsnas ("Restart Portal" in the power menu)
# since v6.3.22 — tee entries for ARC Level 1 tuning (modprobe.d + sysfs parameters)
Cmnd_Alias ZFSNAS_SYSTEM = \
    /usr/bin/timedatectl set-timezone *, \
    /usr/sbin/shutdown *, \
    /usr/sbin/modprobe zfs, \
    /usr/bin/systemctl restart zfsnas, \
    /usr/bin/tee /etc/modprobe.d/zfs.conf, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_max, \
    /usr/bin/tee /sys/module/zfs/parameters/zfs_arc_min

# ── OS updates & service installation ────────────────────────────────────────
# since v1.0.0 — prerequisite package install (apt-get install) and
#   systemd service setup (tee + daemon-reload + enable)
# since v3.0.0 — OS package updates (apt-get upgrade) from the Settings page
# since v6.1.0 — apt-get remove / autoremove for optional feature uninstall
Cmnd_Alias ZFSNAS_APT = \
    /usr/bin/apt-get *, \
    /usr/bin/tee /etc/systemd/system/zfsnas.service, \
    /usr/bin/systemctl daemon-reload, \
    /usr/bin/systemctl enable zfsnas

# ── Sudoers self-management (Sudoers Hardening feature) ───────────────────────
# since v6.3.31 — lets the portal overwrite its own sudoers file when the
#   Sudoers Hardening feature is enabled in the Prerequisites tab.
# since v6.3.32 — cat /etc/sudoers.d/zfsnas lets the portal read its own file
#   so the Sudoers Review diff is always accurate (detects manual edits).
Cmnd_Alias ZFSNAS_SECURITY = \
    /usr/bin/tee /etc/sudoers.d/zfsnas, \
    /usr/bin/cat /etc/sudoers.d/zfsnas

# ── Grant all of the above, passwordless, to the service account ──────────────
zfsnas ALL=(ALL) NOPASSWD: \
    ZFSNAS_ZFS, ZFSNAS_SMB, ZFSNAS_NFS, ZFSNAS_ISCSI, ZFSNAS_MINIO, ZFSNAS_UPS, ZFSNAS_SMART, ZFSNAS_DISK, ZFSNAS_SCAN, ZFSNAS_FILES, ZFSNAS_SYSTEM, ZFSNAS_APT, ZFSNAS_SECURITY
`
