package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// sudoersCurrentContent returns the current file content, falling back to the
// last-applied content (via hash verification) when the file is root-readable only.
// Returns ("", false) when the file cannot be verified.
func sudoersCurrentContent(appCfg *config.AppConfig) (content string, fromCache bool) {
	content, _ = system.GetCurrentSudoersContent()
	if content != "" {
		return content, false
	}
	// File unreadable — check if what we last wrote still matches the expected content.
	if appCfg.SudoersAppliedHash != "" {
		required := system.RequiredSudoersContent()
		expected := system.BuildSudoersContent(required, appCfg.SudoersSilencedMissing, appCfg.SudoersSilencedExtra)
		if system.SudoersContentHash(expected) == appCfg.SudoersAppliedHash {
			return expected, true
		}
	}
	return "", false
}

// HandleSudoersStatus returns the availability and current state of Sudoers Hardening.
// GET /api/sudoers/status
func HandleSudoersStatus(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sudo := system.CheckSudoAccess()
		available := system.CanManageSudoers(sudo)

		missingCount, extraCount, silencedCount := 0, 0, 0
		upToDate := true

		if appCfg.SudoersHardeningEnabled {
			current, _ := sudoersCurrentContent(appCfg)
			diff := system.ComputeSudoersDiff(current, system.RequiredSudoersContent(), appCfg.SudoersSilencedLines)
			upToDate = diff.UpToDate
			for _, d := range diff.MissingLines {
				if d.Silenced {
					silencedCount++
				} else {
					missingCount++
				}
			}
			for _, d := range diff.ExtraLines {
				if d.Silenced {
					silencedCount++
				} else {
					extraCount++
				}
			}
		}

		jsonOK(w, map[string]interface{}{
			"available":      available,
			"sudo_type":      sudo.Type,
			"enabled":        appCfg.SudoersHardeningEnabled,
			"up_to_date":     upToDate,
			"missing_count":  missingCount,
			"extra_count":    extraCount,
			"silenced_count": silencedCount,
		})
	}
}

// HandleSudoersDiff returns the full diff between the installed and required sudoers.
// GET /api/sudoers/diff
func HandleSudoersDiff(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current, _ := sudoersCurrentContent(appCfg)
		diff := system.ComputeSudoersDiff(current, system.RequiredSudoersContent(), appCfg.SudoersSilencedLines)
		jsonOK(w, diff)
	}
}

// HandleEnableSudoersHardening enables or disables sudoers hardening monitoring.
// POST /api/sudoers/enable
// Body: { "enabled": true | false }
func HandleEnableSudoersHardening(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		appCfg.SudoersHardeningEnabled = req.Enabled
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		action := "enabled"
		if !req.Enabled {
			action = "disabled"
		}
		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionUpdateSudoers,
			Result:  audit.ResultOK,
			Details: "sudoers hardening: " + action,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleApplySudoers assembles and writes the sudoers file from approved changes.
// POST /api/sudoers/apply
// Body: { "silenced_missing": [...], "silenced_extra": [...], "pending_missing": [...], "pending_extra": [...] }
//
// silenced_* lines are excluded from the file AND persisted so they stay silenced next run.
// pending_* lines are excluded from the file but NOT persisted — they show as "New" next run.
func HandleApplySudoers(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SilencedMissing []string `json:"silenced_missing"`
			SilencedExtra   []string `json:"silenced_extra"`
			PendingMissing  []string `json:"pending_missing"`
			PendingExtra    []string `json:"pending_extra"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.SilencedMissing == nil {
			req.SilencedMissing = []string{}
		}
		if req.SilencedExtra == nil {
			req.SilencedExtra = []string{}
		}
		if req.PendingMissing == nil {
			req.PendingMissing = []string{}
		}
		if req.PendingExtra == nil {
			req.PendingExtra = []string{}
		}

		required := system.RequiredSudoersContent()

		// For the file write, pending lines are also excluded/kept — they just aren't persisted.
		allExcludedMissing := append(req.SilencedMissing, req.PendingMissing...)
		allKeptExtra := append(req.SilencedExtra, req.PendingExtra...)

		// Count what will actually be added/removed for the audit log.
		current, _ := sudoersCurrentContent(appCfg)
		diff := system.ComputeSudoersDiff(current, required, nil)

		excludeSet := make(map[string]bool, len(allExcludedMissing))
		for _, s := range allExcludedMissing {
			excludeSet[s] = true
		}
		keptSet := make(map[string]bool, len(allKeptExtra))
		for _, s := range allKeptExtra {
			keptSet[s] = true
		}
		addedCount, removedCount := 0, 0
		for _, d := range diff.MissingLines {
			if !excludeSet[d.Line] {
				addedCount++
			}
		}
		for _, d := range diff.ExtraLines {
			if !keptSet[d.Line] {
				removedCount++
			}
		}
		silencedCount := len(req.SilencedMissing) + len(req.SilencedExtra)

		if err := system.ApplySudoers(required, allExcludedMissing, allKeptExtra); err != nil {
			sess := MustSession(r)
			audit.Log(audit.Entry{
				User:    sess.Username,
				Role:    sess.Role,
				Action:  audit.ActionUpdateSudoers,
				Result:  audit.ResultError,
				Details: "sudoers apply failed: " + err.Error(),
			})
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Persist only the explicitly silenced decisions (not pending/new).
		// The hash is computed from the full written content (including pending exclusions)
		// so hash verification still works on next startup.
		content := system.BuildSudoersContent(required, allExcludedMissing, allKeptExtra)
		appCfg.SudoersAppliedHash = system.SudoersContentHash(content)
		appCfg.SudoersSilencedMissing = req.SilencedMissing
		appCfg.SudoersSilencedExtra = req.SilencedExtra
		appCfg.SudoersSilencedLines = append(req.SilencedMissing, req.SilencedExtra...)
		config.SaveAppConfig(appCfg)

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionUpdateSudoers,
			Result: audit.ResultOK,
			Details: fmt.Sprintf("sudoers apply: %d lines added, %d lines removed, %d silenced",
				addedCount, removedCount, silencedCount),
		})

		jsonOK(w, map[string]interface{}{
			"added":   addedCount,
			"removed": removedCount,
			"message": "sudoers file updated successfully",
		})
	}
}
