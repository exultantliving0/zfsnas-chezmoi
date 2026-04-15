package handlers

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/internal/updater"
	"zfsnas/internal/version"
	"zfsnas/system"

	"github.com/gorilla/websocket"
)

// semverGreater returns true only if a is strictly newer than b.
// Supports major.minor.patch and major.minor.patch-build (e.g. 6.3.24-1).
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

// parseSemver parses a version string into [major, minor, patch, build].
// The build component comes from an optional "-N" suffix on the patch segment
// (e.g. "6.3.24-1" → [6, 3, 24, 1]). Missing components default to 0.
func parseSemver(v string) [4]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var out [4]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		if i == 2 {
			// patch may carry a "-build" suffix
			if idx := strings.IndexByte(p, '-'); idx >= 0 {
				out[3], _ = strconv.Atoi(p[idx+1:])
				p = p[:idx]
			}
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

// HandleListReleases returns the last 5 stable GitHub releases with tag, name, body, and assets.
// Pre-releases are always excluded.
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

// HandleBinaryUpdateApply streams the update progress over WebSocket, verifies
// the binary hash, then atomically replaces the binary and calls syscall.Exec to restart.
// An optional ?tag= query parameter targets a specific release (used for downgrade).
// WS /ws/binary-update-apply
func HandleBinaryUpdateApply(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// If a specific tag is requested, target that release instead of latest.
		targetTag := strings.TrimSpace(r.URL.Query().Get("tag"))
		var info updater.ReleaseInfo
		if targetTag != "" {
			send("Step 1/5: Fetching release info for " + targetTag + " from GitHub…")
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
		// (user is intentionally downgrading to that version).
		if targetTag == "" && !semverGreater(latest, version.Version) {
			done(true, "already up to date (v"+version.Version+")")
			return
		}
		send("Target release: v" + latest + "  (current: v" + version.Version + ")")

		// Older releases may not have a .sig asset. For targeted downgrades we
		// skip both signature steps gracefully; for normal updates a missing sig
		// is still treated as a hard failure.
		hasSig := info.SigURL != ""

		send("Step 2/5: Verifying release signature…")
		if hasSig {
			if valid, err := updater.VerifyRelease(info); err != nil {
				done(false, "signature verification failed: "+err.Error())
				return
			} else if !valid {
				done(false, "signature verification failed: signature does not match release key")
				return
			}
			send("Signature valid ✓")
		} else if targetTag != "" {
			send("No signature asset for " + targetTag + " — skipping (pre-signing release)")
		} else {
			done(false, "signature verification failed: release has no signature asset")
			return
		}

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
		if hasSig {
			if err := updater.VerifyDownloadedBinary(tmpPath, info.SigURL); err != nil {
				os.Remove(tmpPath)
				done(false, "signature verification failed: "+err.Error())
				return
			}
			send("Signature verified ✓")
		} else {
			send("No signature asset — skipping post-download verification")
		}

		send("Step 5/5: Replacing binary at " + exePath + "…")
		if err := updater.Replace(tmpPath, exePath); err != nil {
			done(false, "replace failed: "+err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionSoftwareUpdate,
			Result:  audit.ResultOK,
			Details: "updated from v" + version.Version + " to v" + latest,
		})

		send("Restarting process…")
		conn.WriteMessage(websocket.TextMessage, mustJSON(map[string]interface{}{
			"done":    true,
			"success": true,
			"message": "binary replaced — restarting now",
		}))
		conn.Close()

		// Preferred path: replace process image in-place (same PID, no systemd restart event).
		if err := updater.Restart(exePath); err != nil {
			// syscall.Exec can fail on some systems (thread state, security policies, etc.).
			// Fall back to a clean exit so systemd (Restart=on-failure or Restart=always)
			// relaunches the service and picks up the new binary already on disk.
			log.Printf("[updater] syscall.Exec failed (%v) — exiting so systemd restarts with new binary", err)
			os.Exit(1)
		}
	}
}
