package handlers

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"zfsnas/internal/audit"
	"zfsnas/internal/capacityrrd"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleGetUPSStatus returns the current UPS status.
// GET /api/ups/status
func HandleGetUPSStatus(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !system.UPSPrereqsInstalled() {
			jsonOK(w, map[string]interface{}{"installed": false})
			return
		}
		if !appCfg.UPS.Enabled || appCfg.UPS.UPSName == "" {
			jsonOK(w, map[string]interface{}{"installed": true, "enabled": false})
			return
		}
		status, err := system.QueryUPS(appCfg.UPS.UPSName)
		if err != nil {
			jsonOK(w, map[string]interface{}{
				"installed": true,
				"enabled":   true,
				"error":     err.Error(),
			})
			return
		}
		jsonOK(w, map[string]interface{}{
			"installed": true,
			"enabled":   true,
			"status":    status,
		})
	}
}

// HandleGetUPSConfig returns the UPS configuration.
// GET /api/ups/config
func HandleGetUPSConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, appCfg.UPS)
	}
}

// HandleUpdateUPSConfig updates and applies the UPS configuration.
// PUT /api/ups/config
func HandleUpdateUPSConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg config.UPSConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Validate trigger type
		if cfg.ShutdownPolicy.Enabled {
			switch cfg.ShutdownPolicy.TriggerType {
			case "time", "percent", "both":
			default:
				jsonErr(w, http.StatusBadRequest, "trigger_type must be time, percent, or both")
				return
			}
			if cfg.ShutdownPolicy.PercentThreshold < 0 || cfg.ShutdownPolicy.PercentThreshold > 100 {
				jsonErr(w, http.StatusBadRequest, "percent_threshold must be 0-100")
				return
			}
			if cfg.ShutdownPolicy.RuntimeThreshold < 0 || cfg.ShutdownPolicy.RuntimeThreshold > 3600 {
				jsonErr(w, http.StatusBadRequest, "runtime_threshold must be 0-3600 seconds")
				return
			}
		}

		// Preserve fields the frontend never sends back.
		cfg.MonitorPassword = appCfg.UPS.MonitorPassword
		cfg.NominalPowerW = appCfg.UPS.NominalPowerW
		if cfg.UPSName == "" {
			cfg.UPSName = appCfg.UPS.UPSName
		}
		if cfg.Driver == "" {
			cfg.Driver = appCfg.UPS.Driver
		}
		if cfg.Port == "" {
			cfg.Port = appCfg.UPS.Port
		}

		// Rewrite upsmon.conf and restart NUT whenever the user saves.
		// Shutdown is managed by StartUPSShutdownWatcher — upsmon always uses /bin/true.
		if cfg.UPSName == "" {
			jsonErr(w, http.StatusBadRequest, "UPS name is required — run Re-scan to detect your UPS first")
			return
		}
		if cfg.MonitorPassword == "" {
			jsonErr(w, http.StatusBadRequest, "monitor password missing — run Re-scan to regenerate NUT config")
			return
		}
		if err := system.ApplyUPSMonConfig(cfg.UPSName, cfg.MonitorPassword); err != nil {
			jsonErr(w, http.StatusInternalServerError, "apply upsmon config: "+err.Error())
			return
		}

		system.RestartNUTServices()

		appCfg.UPS = cfg
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
			Target: "ups",
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleDetectUPS re-runs nut-scanner and reconfigures NUT config files.
// POST /api/ups/detect
func HandleDetectUPS(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		detected, err := system.DetectAndConfigureUPS(appCfg.UPS.MonitorPassword)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "detect failed: "+err.Error())
			return
		}
		// Always persist the password; update device info if found.
		appCfg.UPS.MonitorPassword = detected.MonitorPassword
		if detected.Name != "" {
			appCfg.UPS.UPSName = detected.Name
			appCfg.UPS.Driver = detected.Driver
			appCfg.UPS.Port = detected.Port
			appCfg.UPS.RawUPSConf = detected.ScannerOutput
			appCfg.UPS.Enabled = true
		}
		_ = config.SaveAppConfig(appCfg)
		jsonOK(w, map[string]interface{}{"detected": detected})
	}
}

// HandleInstallUPS installs nut and nut-client via apt-get.
// POST /api/ups/install
func HandleInstallUPS(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)
		out, err := exec.Command("sudo", "apt-get", "install", "-y", "-q", "nut", "nut-client").CombinedOutput()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, string(out))
			return
		}
		appCfg.UPS.Enabled = true
		_ = config.SaveAppConfig(appCfg)
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionInstallPrereqs,
			Result:  audit.ResultOK,
			Details: "installed: nut, nut-client",
		})
		jsonOK(w, map[string]string{"message": "nut installed"})
	}
}

// HandleSetNominalPower stores the user-supplied nominal power rating.
// PUT /api/ups/nominal-power  Body: {"watts": 1500}
func HandleSetNominalPower(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Watts *int `json:"watts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Watts != nil && *req.Watts < 0 {
			jsonErr(w, http.StatusBadRequest, "watts must be >= 0")
			return
		}
		appCfg.UPS.NominalPowerW = req.Watts
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
			Target: "ups_nominal_power",
		})
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleUPSPerfData returns time-series RRD data for the UPS battery tab.
// GET /api/ups/perf/data?tier=0&since=1234567890
func HandleUPSPerfData(w http.ResponseWriter, r *http.Request) {
	db := system.GetUPSRRD()
	if db == nil {
		jsonOK(w, map[string]interface{}{"tier": 0, "series": map[string]interface{}{}})
		return
	}
	tier, _ := strconv.Atoi(r.URL.Query().Get("tier"))
	if tier < 0 || tier > 2 {
		tier = 0
	}
	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseInt(s, 10, 64)
	}
	keys := []string{"ups:charge_pct", "ups:runtime_secs", "ups:load_pct"}
	result := make(map[string][]capacityrrd.CapSample, len(keys))
	for _, key := range keys {
		samples := db.Query(tier, key)
		if since > 0 {
			cut := len(samples)
			for i, s := range samples {
				if s.TS >= since {
					cut = i
					break
				}
			}
			samples = samples[cut:]
		}
		if samples == nil {
			samples = []capacityrrd.CapSample{}
		}
		result[key] = samples
	}
	jsonOK(w, map[string]interface{}{"tier": tier, "series": result})
}

// HandleUPSPerfOldest returns the oldest Tier0 timestamp in the UPS RRD.
// GET /api/ups/perf/oldest
func HandleUPSPerfOldest(w http.ResponseWriter, r *http.Request) {
	db := system.GetUPSRRD()
	var oldest int64
	if db != nil {
		oldest = db.OldestTS()
	}
	jsonOK(w, map[string]int64{"oldest_ts": oldest})
}

// HandleUPSService starts/stops/restarts nut-server.
// POST /api/ups/service
func HandleUPSService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "start", "stop", "restart":
	default:
		jsonErr(w, http.StatusBadRequest, "action must be start, stop, or restart")
		return
	}
	if err := system.UPSServiceAction(req.Action); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}


