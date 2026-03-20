package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// StartTreeMapScheduler runs a goroutine that fires folder-usage scans for all
// pools sequentially, according to the configured schedule.
func StartTreeMapScheduler(appCfg *config.AppConfig) {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for now := range tick.C {
			if !shouldRunTreeMap(now, appCfg.TreeMapSchedule, appCfg.TreeMapHour, appCfg.TreeMapMinute) {
				continue
			}
			runTreeMapSchedule(appCfg)
		}
	}()
}

func shouldRunTreeMap(now time.Time, schedule string, hour, minute int) bool {
	if schedule == "" {
		return false
	}
	if now.Hour() != hour || now.Minute() != minute {
		return false
	}
	switch schedule {
	case "daily":
		return true
	case "weekly":
		return now.Weekday() == time.Sunday
	case "biweekly":
		day := now.Day()
		return now.Weekday() == time.Sunday && (day <= 7 || (day >= 15 && day <= 21))
	case "monthly":
		return now.Day() == 1
	}
	return false
}

func runTreeMapSchedule(appCfg *config.AppConfig) {
	pools, err := system.GetAllPools()
	if err != nil || len(pools) == 0 {
		log.Printf("[treemap-schedule] no pools found: %v", err)
		return
	}
	log.Printf("[treemap-schedule] starting scheduled scan of %d pool(s)", len(pools))

	datasets, err := system.ListAllDatasets()
	if err != nil {
		log.Printf("[treemap-schedule] failed to list datasets: %v", err)
		return
	}

	// Build a mountpoint lookup keyed by dataset name.
	mountpoints := make(map[string]string, len(datasets))
	for _, d := range datasets {
		mountpoints[d.Name] = d.Mountpoint
	}

	for _, pool := range pools {
		mp := mountpoints[pool.Name]
		if mp == "" || mp == "none" || mp == "legacy" {
			log.Printf("[treemap-schedule] pool %s: no accessible mountpoint, skipping", pool.Name)
			audit.Log(audit.Entry{
				User:    "scheduler",
				Action:  audit.ActionTreeMapScheduleScan,
				Target:  pool.Name,
				Result:  audit.ResultError,
				Details: "scheduled treemap scan skipped: no accessible mountpoint",
			})
			continue
		}

		audit.Log(audit.Entry{
			User:    "scheduler",
			Action:  audit.ActionTreeMapScheduleScan,
			Target:  pool.Name,
			Result:  audit.ResultOK,
			Details: "scheduled treemap scan started",
		})

		_, scanErr := system.ScanDatasetFolders(pool.Name, mp, appCfg.ConfigDir)
		if scanErr != nil {
			log.Printf("[treemap-schedule] scan failed for %s: %v", pool.Name, scanErr)
			audit.Log(audit.Entry{
				User:    "scheduler",
				Action:  audit.ActionTreeMapScheduleScan,
				Target:  pool.Name,
				Result:  audit.ResultError,
				Details: "scheduled treemap scan failed: " + scanErr.Error(),
			})
		} else {
			log.Printf("[treemap-schedule] scan complete for %s", pool.Name)
			audit.Log(audit.Entry{
				User:    "scheduler",
				Action:  audit.ActionTreeMapScheduleScan,
				Target:  pool.Name,
				Result:  audit.ResultOK,
				Details: "scheduled treemap scan complete",
			})
		}
	}
}

// HandleGetTreeMapSchedule returns the current treemap schedule config.
func HandleGetTreeMapSchedule(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]interface{}{
			"schedule": appCfg.TreeMapSchedule,
			"hour":     appCfg.TreeMapHour,
			"minute":   appCfg.TreeMapMinute,
		})
	}
}

// HandleSetTreeMapSchedule updates the treemap schedule (admin only).
func HandleSetTreeMapSchedule(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Schedule string `json:"schedule"`
			Hour     int    `json:"hour"`
			Minute   int    `json:"minute"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		switch req.Schedule {
		case "", "daily", "weekly", "biweekly", "monthly":
			// valid
		default:
			jsonErr(w, http.StatusBadRequest, "invalid schedule value")
			return
		}
		if req.Hour < 0 || req.Hour > 23 {
			jsonErr(w, http.StatusBadRequest, "hour must be 0-23")
			return
		}
		if req.Minute < 0 || req.Minute > 59 {
			jsonErr(w, http.StatusBadRequest, "minute must be 0-59")
			return
		}
		appCfg.TreeMapSchedule = req.Schedule
		appCfg.TreeMapHour = req.Hour
		appCfg.TreeMapMinute = req.Minute
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}
		jsonOK(w, map[string]interface{}{
			"schedule": appCfg.TreeMapSchedule,
			"hour":     appCfg.TreeMapHour,
			"minute":   appCfg.TreeMapMinute,
		})
	}
}
