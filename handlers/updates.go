package handlers

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"

	"github.com/gorilla/websocket"
)

// HandleOSInfo reads /etc/os-release and returns the NAME and VERSION fields.
func HandleOSInfo(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		jsonOK(w, map[string]string{"name": "Linux", "version": ""})
		return
	}
	defer f.Close()
	fields := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := line[:idx]
		val := strings.Trim(line[idx+1:], `"`)
		fields[key] = val
	}
	jsonOK(w, map[string]string{
		"name":    fields["NAME"],
		"version": fields["DEBIAN_VERSION_FULL"],
	})
}

type updateCacheFile struct {
	CheckedAt time.Time            `json:"checked_at"`
	Packages  []map[string]string  `json:"packages"`
}

// zfsnasPrefixes lists package name prefixes (or exact names) that belong to the
// ZFS / NAS stack this portal depends on.
var zfsnasPrefixes = []string{
	// ZFS
	"zfs", "libzfs", "libzpool", "libnvpair", "libuutil", "libzutil",
	// Samba / SMB
	"samba", "libsamba", "libsmb", "smbclient", "winbind", "libnss-winbind", "libpam-winbind", "libwbclient",
	// NFS
	"nfs-", "libnfs", "rpcbind", "libnfsidmap", "libtirpc",
	// iSCSI / targetcli
	"targetcli", "python3-rtslib", "python3-configshell", "open-iscsi",
	// SMART
	"smartmontools",
}

// isZFSNASPackage returns true when the package name belongs to the ZFS/NAS stack.
func isZFSNASPackage(name string) bool {
	for _, prefix := range zfsnasPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// parseUpgradePackages parses apt-get --simulate upgrade output into a package list.
func parseUpgradePackages(out []byte) []map[string]string {
	var pkgs []map[string]string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Format: Inst pkg-name [old-ver] (new-ver suite [arch])
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name := parts[1]
		currentVersion := ""
		newVersion := ""
		// Scan tokens left to right; stop at "(" so we never read "[arch])" after it.
		for _, p := range parts[2:] {
			if strings.HasPrefix(p, "(") {
				newVersion = strings.TrimPrefix(p, "(")
				break
			}
			if strings.HasPrefix(p, "[") {
				currentVersion = strings.Trim(p, "[]")
			}
		}
		secType := "regular"
		if strings.Contains(line, "-security") {
			secType = "security"
		} else if isZFSNASPackage(name) {
			secType = "zfsnas"
		}
		pkgs = append(pkgs, map[string]string{
			"name":            name,
			"current_version": currentVersion,
			"version":         newVersion,
			"type":            secType,
		})
	}
	if pkgs == nil {
		pkgs = []map[string]string{}
	}
	return pkgs
}

func saveUpdateCache(configDir string, pkgs []map[string]string) {
	path := filepath.Join(configDir, "updates_cache.json")
	b, err := json.MarshalIndent(updateCacheFile{CheckedAt: time.Now(), Packages: pkgs}, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, b, 0600)
}

// HandleCheckUpdates runs `apt-get update` then returns the list of packages
// that would be upgraded by `apt-get upgrade` (excludes kept-back packages).
// Results are persisted to config/updates_cache.json.
func HandleCheckUpdates(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Refresh package index.
		exec.Command("sudo", "apt-get", "update", "-qq").Run()

		out, err := exec.Command("apt-get", "--simulate", "upgrade").Output()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "apt-get simulate failed: "+err.Error())
			return
		}

		pkgs := parseUpgradePackages(out)
		saveUpdateCache(appCfg.ConfigDir, pkgs)

		jsonOK(w, map[string]interface{}{
			"count":    len(pkgs),
			"packages": pkgs,
		})
	}
}

// HandleGetUpdateCache returns the last persisted update check results.
func HandleGetUpdateCache(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(appCfg.ConfigDir, "updates_cache.json")
		b, err := os.ReadFile(path)
		if err != nil {
			// No cache yet — return empty result.
			jsonOK(w, map[string]interface{}{"count": 0, "packages": []map[string]string{}, "checked_at": nil})
			return
		}
		var cache updateCacheFile
		if err := json.Unmarshal(b, &cache); err != nil {
			jsonOK(w, map[string]interface{}{"count": 0, "packages": []map[string]string{}, "checked_at": nil})
			return
		}
		jsonOK(w, map[string]interface{}{
			"count":      len(cache.Packages),
			"packages":   cache.Packages,
			"checked_at": cache.CheckedAt,
		})
	}
}

// HandleApplyUpdates upgrades the HTTP connection to WebSocket and streams
// the output of `sudo apt-get upgrade -y`.
func HandleApplyUpdates(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	send := func(line string) {
		conn.WriteMessage(websocket.TextMessage, mustJSON(map[string]interface{}{
			"line": line,
		}))
	}
	done := func(success bool, msg string) {
		conn.WriteMessage(websocket.TextMessage, mustJSON(map[string]interface{}{
			"done":    true,
			"success": success,
			"message": msg,
		}))
	}

	send("Running: sudo apt-get upgrade -y")
	send("─────────────────────────────────────────")

	cmd := exec.Command("sudo", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "upgrade", "-y")

	pr, pw, err := os.Pipe()
	if err != nil {
		done(false, "failed to create pipe")
		return
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		done(false, err.Error())
		return
	}
	pw.Close()

	buf := make([]byte, 4096)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			lines := strings.Split(string(buf[:n]), "\n")
			for _, l := range lines {
				if strings.TrimSpace(l) != "" {
					send(l)
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	cmdErr := cmd.Wait()
	send("─────────────────────────────────────────")

	sess := MustSession(r)
	if cmdErr != nil {
		send("Upgrade failed: " + cmdErr.Error())
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionApplyUpdates,
			Result:  audit.ResultError,
			Details: cmdErr.Error(),
		})
		done(false, cmdErr.Error())
		return
	}

	send("System upgraded successfully.")
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionApplyUpdates,
		Result: audit.ResultOK,
	})
	done(true, "upgrade complete")
}
