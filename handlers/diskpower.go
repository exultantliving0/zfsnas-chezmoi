package handlers

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleGetDiskPower returns current disk power config + hdparm availability.
// GET /api/disks/power
func HandleGetDiskPower(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		disks, _ := system.ListPhysicalDisks()
		jsonOK(w, map[string]interface{}{
			"config":           appCfg.DiskPower,
			"hdparm_installed": system.DiskPowerPrereqsInstalled(),
			"disks":            disks,
		})
	}
}

// HandleUpdateDiskPower saves and immediately applies disk power settings.
// PUT /api/disks/power
func HandleUpdateDiskPower(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg config.DiskPowerConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		// Validate ranges
		if cfg.APMLevel < 0 || cfg.APMLevel > 255 {
			jsonErr(w, http.StatusBadRequest, "apm_level must be 0-255")
			return
		}
		if cfg.SpindownTimeout < 0 || cfg.SpindownTimeout > 251 {
			jsonErr(w, http.StatusBadRequest, "spindown_timeout must be 0-251")
			return
		}
		if cfg.AcousticLevel != -1 && cfg.AcousticLevel != 0 && (cfg.AcousticLevel < 128 || cfg.AcousticLevel > 254) {
			jsonErr(w, http.StatusBadRequest, "acoustic_level must be -1, 0, or 128-254")
			return
		}

		if err := system.ApplyDiskPowerConfig(cfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		appCfg.DiskPower = cfg
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionUpdateSettings,
			Result: audit.ResultOK,
			Target: "disk_power",
		})
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleInstallHdparm runs apt-get install hdparm.
// POST /api/disks/power/install
func HandleInstallHdparm(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("sudo", "apt-get", "install", "-y", "-q", "hdparm").CombinedOutput()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, string(out))
		return
	}
	jsonOK(w, map[string]string{"message": "hdparm installed"})
}
