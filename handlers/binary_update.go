package handlers

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"zfsnas/internal/config"
	"zfsnas/internal/updater"
	"zfsnas/internal/version"
	"zfsnas/system"

	"github.com/gorilla/websocket"
)

// semverGreater returns true only if a is strictly newer than b (major.minor.patch).
func semverGreater(a, b string) bool {
	pa := parseSemver(a)
	pb := parseSemver(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// HandleCheckBinaryUpdate checks GitHub for a newer release and verifies its signature.
// Version checking is always allowed regardless of LiveUpdateEnabled.
// GET /api/binary-update/check
func HandleCheckBinaryUpdate(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := updater.CheckLatest()
		if err != nil {
			if system.DebugMode {
				log.Printf("[debug] binary-update/check: CheckLatest error: %v", err)
			}
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		latest := strings.TrimPrefix(info.Tag, "v")
		current := version.Version
		updateAvailable := semverGreater(latest, current)

		// Verify the release signature (downloads two tiny files: .sha256 + .sig).
		sigValid := false
		sigError := ""
		if valid, err := updater.VerifyRelease(info); err != nil {
			sigError = err.Error()
			if system.DebugMode {
				log.Printf("[debug] binary-update/check: VerifyRelease error: %v", err)
			}
		} else {
			sigValid = valid
		}

		if system.DebugMode {
			log.Printf("[debug] binary-update/check: current=%s latest=%s update_available=%v sig_valid=%v",
				current, latest, updateAvailable, sigValid)
		}
		jsonOK(w, map[string]interface{}{
			"current":           current,
			"latest":            latest,
			"update_available":  updateAvailable,
			"download_url":      info.DownloadURL,
			"sig_valid":         sigValid,
			"sig_error":         sigError,
			"service_installed": system.IsServiceInstalled(),
		})
	}
}

// HandleBinaryUpdateApply streams the update progress over WebSocket, verifies
// the binary hash, then atomically replaces the binary and calls syscall.Exec to restart.
// WS /ws/binary-update-apply
func HandleBinaryUpdateApply(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !appCfg.LiveUpdateEnabled {
			jsonErr(w, http.StatusForbidden, "live binary update is disabled")
			return
		}

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

		send("Step 1/5: Fetching release info from GitHub…")
		info, err := updater.CheckLatest()
		if err != nil {
			done(false, "fetch release info failed: "+err.Error())
			return
		}
		latest := strings.TrimPrefix(info.Tag, "v")
		if !semverGreater(latest, version.Version) {
			done(true, "already up to date (v"+version.Version+")")
			return
		}
		send("Latest release: v" + latest + "  (current: v" + version.Version + ")")

		send("Step 2/5: Verifying release signature…")
		if valid, err := updater.VerifyRelease(info); err != nil {
			done(false, "signature verification failed: "+err.Error())
			return
		} else if !valid {
			done(false, "signature verification failed: signature does not match release key")
			return
		}
		send("Signature valid ✓")

		exePath, err := updater.ExePath()
		if err != nil {
			done(false, "cannot determine executable path: "+err.Error())
			return
		}
		destDir := filepath.Dir(exePath)

		send("Step 3/5: Downloading binary to " + destDir + "…")
		tmpPath, err := updater.Download(info.DownloadURL, destDir)
		if err != nil {
			done(false, "download failed: "+err.Error())
			return
		}

		send("Step 4/5: Verifying binary signature…")
		if err := updater.VerifyDownloadedBinary(tmpPath, info.SigURL); err != nil {
			os.Remove(tmpPath)
			done(false, "signature verification failed: "+err.Error())
			return
		}
		send("Signature verified ✓")

		send("Step 5/5: Replacing binary at " + exePath + "…")
		if err := updater.Replace(tmpPath, exePath); err != nil {
			done(false, "replace failed: "+err.Error())
			return
		}

		send("Restarting process…")
		conn.WriteMessage(websocket.TextMessage, mustJSON(map[string]interface{}{
			"done":    true,
			"success": true,
			"message": "binary replaced — restarting now",
		}))
		conn.Close()

		// Replace process image; under systemd this keeps the service alive.
		_ = updater.Restart(exePath)
	}
}
